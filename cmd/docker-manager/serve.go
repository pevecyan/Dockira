package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"docker-manager/internal/alerts"
	"docker-manager/internal/api"
	"docker-manager/internal/auth"
	"docker-manager/internal/compose"
	"docker-manager/internal/config"
	"docker-manager/internal/docker"
	"docker-manager/internal/events"
	"docker-manager/internal/files"
	"docker-manager/internal/hoststats"
	"docker-manager/internal/jobs"
	"docker-manager/internal/notify"
	"docker-manager/internal/registry"
	"docker-manager/internal/secret"
	"docker-manager/internal/snapshots"
	"docker-manager/internal/stacks"
	"docker-manager/internal/store"
	"docker-manager/internal/updates"
	"docker-manager/web"
)

func cmdServe(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	cfgPath := fs.String("config", config.DefaultPath, "config file")
	fs.Parse(args)

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}
	if err := cfg.Validate(); err != nil {
		return fmt.Errorf("%s:\n%w", *cfgPath, err)
	}
	level := slog.LevelInfo
	if cfg.Dev {
		level = slog.LevelDebug
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	st, err := store.Open(ctx, cfg.DataDir)
	if err != nil {
		return fmt.Errorf("database: %w", err)
	}
	defer st.Close()

	dk, err := docker.New()
	if err != nil {
		return fmt.Errorf("docker client: %w", err)
	}
	defer dk.Close()
	if err := dk.Ping(ctx); err != nil {
		log.Warn("docker is not reachable yet", "err", err)
	}
	cli := compose.New()
	sm := &stacks.Manager{Root: cfg.AppsRoot, Depth: cfg.ScanDepth, Compose: cli, Docker: dk, Log: log, SelfID: selfID()}
	key, _ := cfg.Key()
	proxies, _ := config.ParseCIDRs(cfg.TrustedProxies)
	allow, _ := config.ParseCIDRs(cfg.IPAllowlist)
	hub := events.NewHub()
	nbox, err := secret.NewBox(key, "notifications")
	if err != nil {
		return err
	}
	am := alerts.New(st.DB, nbox, log, cfg.PublicURL)
	a := auth.New(auth.Options{
		PasswordHash: cfg.PasswordHash, Key: key, TrustedProxies: proxies, Allowlist: allow,
		PublicURL: cfg.PublicURL, ReadOnly: cfg.ReadOnly, Recorder: st,
		Notify: func(event, ip, detail string) {
			title := map[string]string{"auth.login_new_ip": "Login from a new address: " + ip, "auth.lockout": "Logins paused after many wrong passwords"}[event]
			am.Send(notify.Event{Type: event, Severity: notify.Warning, Title: title, Message: detail, Fields: map[string]string{"ip": ip}})
		},
	})
	api.ScriptHashes = web.ScriptHashes()
	runner := jobs.New(st.DB, hub, log, 2)
	runner.OnFinish = am.JobFinished
	box, err := secret.NewBox(key, "file revisions")
	if err != nil {
		return err
	}
	fm := &files.Manager{DB: st.DB, Validator: cli, Box: box}
	defer runner.Shutdown()
	sn := &snapshots.Manager{DataDir: cfg.DataDir, Docker: cli.Bin, Image: helperImage(ctx, dk), Engine: dk, Compose: cli}
	um := &updates.Manager{Snapshots: sn, Store: st, Engine: dk, Registry: registry.New(registry.Options{UserAgent: "docker-manager/" + version}),
		Stacks: sm, Compose: cli, Jobs: runner, Hub: hub, Log: log, DataDir: cfg.DataDir, DiskPath: cfg.AppsRoot, Notify: am.Send}
	srv := api.New(api.Deps{Config: cfg, Store: st, Version: version, Log: log, UI: web.Handler(),
		Docker: dk, Compose: cli, Stacks: sm, Hub: hub, Auth: a, Jobs: runner, Files: fm, Updates: um, Alerts: am, Snapshots: sn,
		Stats: hoststats.New(hoststats.Options{DiskPath: cfg.AppsRoot})})
	go srv.Run(ctx)

	hs := &http.Server{
		Addr:              cfg.Listen,
		Handler:           api.LogRequests(log, srv),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
		// no WriteTimeout: event and log streams stay open
		BaseContext: func(net.Listener) context.Context { return ctx }, // shutdown ends the streams
	}
	errc := make(chan error, 1)
	go func() { errc <- hs.ListenAndServe() }()
	log.Info("docker-manager started", "version", version, "listen", cfg.Listen, "apps_root", cfg.AppsRoot,
		"data_dir", cfg.DataDir, "read_only", cfg.ReadOnly)

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	log.Info("shutting down")
	sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := hs.Shutdown(sctx); err != nil && !errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	return nil
}

// selfID is this process's container ID prefix when running in a container (Docker sets the
// hostname to it), so the UI can protect the manager's own stack.
func selfID() string {
	if _, err := os.Stat("/.dockerenv"); err != nil {
		return ""
	}
	h, _ := os.Hostname()
	return h
}

// helperImage is the image the volume snapshots run in: our own (it has sh and tar, and needs no
// download) when we run in a container, else busybox.
func helperImage(ctx context.Context, dk *docker.Client) string {
	if id := selfID(); id != "" {
		if d, err := dk.Inspect(ctx, id); err == nil && d.ImageID != "" {
			return d.ImageID
		}
	}
	return "busybox:1.36"
}
