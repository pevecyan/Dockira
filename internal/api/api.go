// Package api is the HTTP API under /api. Typed endpoints are registered with Huma, which also
// produces the OpenAPI document the web client's types are generated from (`make gen-api`).
// Streams (SSE) are plain handlers on the same mux.
package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path"
	"reflect"
	"strings"
	"sync"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humago"

	"docker-manager/internal/alerts"
	"docker-manager/internal/auth"
	"docker-manager/internal/compose"
	"docker-manager/internal/config"
	"docker-manager/internal/docker"
	"docker-manager/internal/events"
	"docker-manager/internal/files"
	"docker-manager/internal/hoststats"
	"docker-manager/internal/jobs"
	"docker-manager/internal/snapshots"
	"docker-manager/internal/stacks"
	"docker-manager/internal/store"
	"docker-manager/internal/updates"
)

type Deps struct {
	Config    config.Config
	Store     *store.Store
	Version   string
	Log       *slog.Logger
	UI        http.Handler // the embedded web app, served for everything outside /api
	Docker    Engine
	Compose   *compose.CLI
	Stacks    *stacks.Manager
	Hub       *events.Hub
	Auth      *auth.Auth
	Stats     *hoststats.Sampler
	Jobs      *jobs.Runner
	Files     *files.Manager
	Updates   *updates.Manager   // update checks and the update flow (nil: "update" is plain pull + up)
	Alerts    *alerts.Manager    // notifications
	Snapshots *snapshots.Manager // volume snapshots
}

// Engine is the part of the Docker client the API uses (a fake in tests).
type Engine interface {
	stacks.Lister
	Inspect(ctx context.Context, id string) (docker.Details, error)
	System(ctx context.Context) (docker.System, error)
	DiskUsage(ctx context.Context) (docker.DiskUsage, error)
	Events(ctx context.Context) (<-chan docker.Event, <-chan error)
	ContainerAction(ctx context.Context, id, action string) error
	Logs(ctx context.Context, id string, o docker.LogOptions) (io.ReadCloser, error)
	IsTTY(ctx context.Context, id string) (bool, error)
}

type Server struct {
	Deps
	mux      *http.ServeMux
	api      huma.API
	hostname string
	started  time.Time

	feed           *feed
	history        hostHistory
	lastMu         sync.Mutex
	lastStacks     []byte
	system         cached[docker.System]
	diskUsage      cached[docker.DiskUsage]
	composeVersion cached[string]
	runners        []func(context.Context)      // more background loops (jobs, updates, …)
	onStacks       []func([]stacks.Stack)       // called after each stack publish
	onDocker       []func(docker.Event)         // called for each Docker event
	onFileChange   []func(stackID, file string) // outside edits noticed
}

func init() {
	// Arrays are never null in responses (handlers return empty slices), so the generated
	// TypeScript types don't need null checks everywhere.
	huma.DefaultArrayNullable = false
}

func New(d Deps) *Server {
	if d.Log == nil {
		d.Log = slog.New(slog.DiscardHandler)
	}
	if d.Hub == nil {
		d.Hub = events.NewHub()
	}
	s := &Server{Deps: d, mux: http.NewServeMux(), hostname: hostname(), started: time.Now(), feed: &feed{max: 300}}

	cfg := huma.DefaultConfig("Dockira", d.Version)
	cfg.Components.Schemas = huma.NewMapRegistry("#/components/schemas/", schemaName)
	cfg.CreateHooks = nil // no $schema links in responses
	cfg.OpenAPIPath = "/api/openapi"
	cfg.SchemasPath = ""
	cfg.DocsPath = ""
	if d.Config.Dev {
		cfg.DocsPath = "/api/docs"
	}
	s.api = humago.New(s.mux, cfg)
	s.register()

	s.mux.HandleFunc("GET /api/events", s.events)
	s.mux.HandleFunc("GET /api/exec/{id}", s.execWS)
	s.mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.Write([]byte("ok\n"))
	})
	s.mux.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) {
		writeErr(w, http.StatusNotFound, "no such endpoint: %s %s", r.Method, r.URL.Path)
	})
	if d.UI != nil {
		s.mux.Handle("/", d.UI)
	}
	return s
}

// ServeHTTP applies security headers and (when configured) authentication around the routes.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	securityHeaders(w, r, s.Config)
	if s.Auth != nil {
		r = r.WithContext(context.WithValue(r.Context(), clientIPKey, s.Auth.ClientIP(r)))
		s.Auth.Middleware(s.mux).ServeHTTP(w, r)
		return
	}
	s.mux.ServeHTTP(w, r)
}

// Spec builds the OpenAPI document without a running server (`docker-manager openapi`).
func Spec(version string) *huma.OpenAPI {
	return New(Deps{Version: version}).api.OpenAPI()
}

func (s *Server) register() {
	s.registerStacks()
	s.registerHost()
	s.registerActions()
	s.registerLogs()
	s.registerFiles()
	s.registerBrowse()
	s.registerApps()
	if s.Snapshots != nil || s.Store == nil {
		s.registerBackups()
	}
	s.registerSystem()
	if s.Updates != nil || s.Store == nil {
		s.registerUpdates()
	}
	if s.Alerts != nil || s.Store == nil {
		s.registerNotifications()
	}
	huma.Register(s.api, huma.Operation{
		OperationID: "get-info",
		Method:      http.MethodGet,
		Path:        "/api/info",
		Summary:     "Server and host information",
		Tags:        []string{"system"},
	}, s.info)
}

type Info struct {
	Version       string    `json:"version"`
	Hostname      string    `json:"hostname"`
	AppsRoot      string    `json:"apps_root"`
	ReadOnly      bool      `json:"read_only" doc:"Every change is refused (read_only in the config)"`
	ExecEnabled   bool      `json:"exec_enabled"`
	SchemaVersion int       `json:"schema_version" doc:"Applied database migration"`
	StartedAt     time.Time `json:"started_at"`
}

type InfoOutput struct {
	Body Info
}

func (s *Server) info(ctx context.Context, _ *struct{}) (*InfoOutput, error) {
	v, err := s.Store.Version(ctx)
	if err != nil {
		return nil, huma.Error500InternalServerError("database", err)
	}
	host := s.hostname
	if s.Docker != nil {
		if sys, err := s.system.get(30*time.Second, func() (docker.System, error) { return s.Docker.System(ctx) }); err == nil && sys.Name != "" {
			host = sys.Name // inside our container, the host's name comes from Docker
		}
	}
	return &InfoOutput{Body: Info{
		Version:       s.Version,
		Hostname:      host,
		AppsRoot:      s.Config.AppsRoot,
		ReadOnly:      s.Config.ReadOnly,
		ExecEnabled:   s.Config.ExecEnabled,
		SchemaVersion: v,
		StartedAt:     s.started,
	}}, nil
}

// hostname prefers the Docker host's name (/proc of the host mounted at /host/proc) over the
// container's.
func hostname() string {
	if b, err := os.ReadFile("/host/proc/sys/kernel/hostname"); err == nil {
		if h := strings.TrimSpace(string(b)); h != "" {
			return h
		}
	}
	h, _ := os.Hostname()
	return h
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// writeErr answers in the same shape as Huma's errors (RFC 9457 problem details).
func writeErr(w http.ResponseWriter, code int, format string, a ...any) {
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"title":  http.StatusText(code),
		"status": code,
		"detail": fmt.Sprintf(format, a...),
	})
}

// schemaName prefixes types from the domain packages ("DockerContainer",
// "HoststatsContainer"), whose short names collide.
func schemaName(t reflect.Type, hint string) string {
	name := huma.DefaultSchemaNamer(t, hint)
	for t.Kind() == reflect.Pointer || t.Kind() == reflect.Slice || t.Kind() == reflect.Array || t.Kind() == reflect.Map {
		t = t.Elem()
	}
	switch pkg := path.Base(t.PkgPath()); pkg {
	case "docker", "hoststats", "notify", "registry", "store", "jobs", "updates", "files":
		return strings.ToUpper(pkg[:1]) + pkg[1:] + name
	}
	return name
}
