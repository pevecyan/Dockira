package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"docker-manager/internal/docker"
	"docker-manager/internal/events"
	"docker-manager/internal/notify"
)

// FeedItem is one line of the Events card: a Docker event or a job / system message.
type FeedItem struct {
	Time      time.Time `json:"time"`
	Kind      string    `json:"kind" enum:"docker,job,update,system"`
	Level     string    `json:"level" enum:"info,warn,error"`
	Stack     string    `json:"stack,omitempty"`
	Container string    `json:"container,omitempty"`
	Action    string    `json:"action"`
	Message   string    `json:"message"`
}

// feed keeps the last events in memory (they are also in Docker's own event log, and the
// important ones go to the audit log / notifications).
type feed struct {
	mu    sync.Mutex
	items []FeedItem
	max   int
}

func (f *feed) add(it FeedItem) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.items = append(f.items, it)
	if len(f.items) > f.max {
		f.items = append([]FeedItem(nil), f.items[len(f.items)-f.max:]...)
	}
}

func (f *feed) list() []FeedItem {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]FeedItem{}, f.items...)
}

// Run starts the background work: initial scan, Docker event watcher, periodic rescans and the
// stats sampler. It returns when ctx ends.
func (s *Server) Run(ctx context.Context) {
	if err := s.Stacks.Scan(ctx); err != nil {
		s.Log.Warn("initial scan", "err", err)
	}
	s.publishStacks()
	go s.trackFiles(ctx)
	var wg sync.WaitGroup
	for _, f := range []func(context.Context){s.watchDocker, s.pollScan, s.pollStats, s.watchDisk, s.sampleHost, s.dailyCleanup} {
		wg.Add(1)
		go func() { defer wg.Done(); f(ctx) }()
	}
	if s.Updates != nil {
		wg.Add(1)
		go func() { defer wg.Done(); s.Updates.Run(ctx) }()
	}
	for _, f := range s.runners {
		wg.Add(1)
		go func() { defer wg.Done(); f(ctx) }()
	}
	wg.Wait()
}

// publishStacks sends the stack list when it changed since the last publish.
func (s *Server) publishStacks() {
	l := s.stackList()
	b, _ := json.Marshal(struct {
		S, A any
		E    string
	}{l.Stacks, l.Standalone, l.DockerError})
	s.lastMu.Lock()
	same := bytes.Equal(b, s.lastStacks)
	s.lastStacks = b
	s.lastMu.Unlock()
	if !same {
		s.Hub.Publish("stacks", l)
	}
	for _, f := range s.onStacks {
		f(l.Stacks)
	}
}

func (s *Server) watchDocker(ctx context.Context) {
	refresh := events.NewDebouncer(400*time.Millisecond, func() {
		if err := s.Stacks.RefreshContainers(ctx); err != nil && ctx.Err() == nil {
			s.Log.Warn("refresh containers", "err", err)
		}
		s.publishStacks()
	})
	backoff := time.Second
	for ctx.Err() == nil {
		ch, errc := s.Docker.Events(ctx)
		connected := time.Now()
		for e := range ch {
			backoff = time.Second
			if e.Type == "container" {
				refresh.Trigger()
			}
			if it, ok := s.describe(e); ok {
				s.feed.add(it)
				s.Hub.Publish("feed", it)
			}
			for _, f := range s.onDocker {
				f(e)
			}
			if s.Alerts != nil && e.Type == "container" {
				s.Alerts.DockerEvent(e, s.stackIDForProject(e.Attrs[docker.LabelProject], e.Attrs[docker.LabelWorkingDir]))
			}
		}
		err := <-errc
		if ctx.Err() != nil {
			return
		}
		s.Log.Warn("docker events disconnected", "err", err, "after", time.Since(connected).Round(time.Second))
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, 30*time.Second)
		refresh.Trigger() // resync whatever happened while disconnected
	}
}

// describe turns a Docker event into a feed line; noisy ones (healthcheck execs) are skipped.
func (s *Server) describe(e docker.Event) (FeedItem, bool) {
	a := e.Action
	if strings.HasPrefix(a, "exec_") || a == "attach" || a == "resize" || a == "top" || a == "copy" || a == "archive-path" || a == "extract-to-dir" {
		return FeedItem{}, false
	}
	it := FeedItem{Time: e.Time, Kind: "docker", Level: "info", Action: a}
	switch e.Type {
	case "container":
		// throwaway containers of image builds: unnamed project, image referenced by ID
		if e.Attrs[docker.LabelProject] == "" && strings.HasPrefix(e.Attrs["image"], "sha256:") {
			return FeedItem{}, false
		}
		it.Container = e.Attrs["name"]
		if p := e.Attrs[docker.LabelProject]; p != "" {
			it.Stack = s.stackIDForProject(p, e.Attrs[docker.LabelWorkingDir])
		}
		switch {
		case a == "die":
			code := e.Attrs["exitCode"]
			it.Message = "exited with code " + code
			if code != "0" {
				it.Level = "warn"
			}
		case a == "oom":
			it.Message, it.Level = "out of memory", "error"
		case strings.HasPrefix(a, "health_status"):
			st := strings.TrimSpace(strings.TrimPrefix(a, "health_status:"))
			it.Action, it.Message = "health", st
			if st == "unhealthy" {
				it.Level = "error"
			}
		default:
			it.Message = a
			if img := e.Attrs["image"]; img != "" && (a == "create" || a == "start") {
				it.Message = fmt.Sprintf("%s (%s)", a, img)
			}
		}
	case "image":
		name := e.Attrs["name"]
		if name == "" || strings.HasPrefix(name, "sha256:") || strings.HasPrefix(e.ID, "sha256:") && a == "create" {
			return FeedItem{}, false // build layers
		}
		it.Container = ""
		it.Message = a + " " + name
	default:
		return FeedItem{}, false
	}
	return it, true
}

func (s *Server) stackIDForProject(project, workingDir string) string {
	for _, st := range s.Stacks.Stacks() {
		if (workingDir != "" && st.Dir == workingDir) || st.Name == project {
			return st.ID
		}
	}
	return ""
}

func shortID(id string) string {
	id = strings.TrimPrefix(id, "sha256:")
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

func (s *Server) pollScan(ctx context.Context) {
	t := time.NewTicker(60 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := s.Stacks.Scan(ctx); err != nil && ctx.Err() == nil {
				s.Log.Debug("scan", "err", err)
			}
			s.publishStacks()
			s.trackFiles(ctx)
		}
	}
}

// pollStats samples host and container stats every 2 s while someone is watching.
func (s *Server) pollStats(ctx context.Context) {
	t := time.NewTicker(2 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if s.Hub.Subscribers() == 0 {
			continue
		}
		if h, err := s.Stats.Host(); err == nil || h.MemTotal > 0 {
			s.Hub.Publish("host", h)
		}
		var ids []string
		for _, c := range s.Stacks.Containers() {
			if c.State == "running" {
				ids = append(ids, c.ID)
			}
		}
		s.Hub.Publish("cstats", s.Stats.Containers(ids))
	}
}

// trackFiles records compose/.env changes made outside the UI and reports them.
func (s *Server) trackFiles(ctx context.Context) {
	if s.Files == nil {
		return
	}
	for _, c := range s.Files.TrackAll(ctx, s.Stacks.Stacks()) {
		s.addFeed(FeedItem{Time: time.Now(), Kind: "system", Level: "warn", Stack: c.StackID, Action: "file",
			Message: c.File + " changed outside Dockira (saved in History)"})
		if s.Alerts != nil {
			s.Alerts.Send(notify.Event{Type: "stack.file_changed_externally", Severity: notify.Info, Title: c.File + " of " + c.StackID + " changed",
				Message: "The file was changed outside Dockira. The previous version is in History.", Stack: c.StackID,
				URL: "/stacks/" + c.StackID + "?tab=history", Fields: map[string]string{"dedupe": c.File}})
		}
		for _, f := range s.onFileChange {
			f(c.StackID, c.File)
		}
	}
}

// watchDisk checks the free space under apps_root every 10 minutes (alerts below 10 %).
func (s *Server) watchDisk(ctx context.Context) {
	t := time.NewTicker(10 * time.Minute)
	defer t.Stop()
	for {
		if s.Alerts != nil && s.Stats != nil {
			if h, _ := s.Stats.Host(); h.DiskTotal > 0 {
				s.Alerts.Host(h)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// dailyCleanup trims old history once a day.
func (s *Server) dailyCleanup(ctx context.Context) {
	if s.Store == nil {
		return
	}
	t := time.NewTicker(24 * time.Hour)
	defer t.Stop()
	for {
		if err := s.Store.Cleanup(ctx); err != nil && ctx.Err() == nil {
			s.Log.Warn("cleanup", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
