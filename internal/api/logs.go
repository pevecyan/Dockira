package api

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"docker-manager/internal/docker"
)

// LogLine is one log line as the UI gets it (short keys: thousands of these per minute).
type LogLine struct {
	C string `json:"c"`           // container name
	S string `json:"s"`           // service
	T string `json:"t"`           // RFC 3339 timestamp from Docker
	E bool   `json:"e,omitempty"` // stderr
	M string `json:"m"`
}

func (s *Server) registerLogs() {
	s.mux.HandleFunc("GET /api/logs", s.streamLogs)
	s.mux.HandleFunc("GET /api/logs/download", s.downloadLogs)
}

type logTarget struct {
	c       docker.Container
	service string
}

// logTargets resolves ?container= (ID or name) or ?stack= with optional ?services=a,b.
func (s *Server) logTargets(r *http.Request) ([]logTarget, string, error) {
	q := r.URL.Query()
	if id := q.Get("container"); id != "" {
		for _, c := range s.Stacks.Containers() {
			if c.ID == id || c.Name == id || strings.HasPrefix(c.ID, id) && len(id) >= 12 {
				return []logTarget{{c, c.Service}}, c.Name, nil
			}
		}
		return nil, "", fmt.Errorf("no container %s", id)
	}
	st, ok := s.Stacks.Get(q.Get("stack"))
	if !ok {
		return nil, "", fmt.Errorf("no stack %q", q.Get("stack"))
	}
	var only []string
	if v := q.Get("services"); v != "" {
		only = strings.Split(v, ",")
	}
	var out []logTarget
	for _, sv := range st.Services {
		if len(only) > 0 && !slices.Contains(only, sv.Name) {
			continue
		}
		for _, c := range sv.Containers {
			out = append(out, logTarget{c, sv.Name})
		}
	}
	return out, st.ID, nil
}

// streamLogs follows the logs of a container or a whole stack. In stack mode it attaches to
// containers that appear later (recreated by an update) and re-attaches after restarts,
// continuing from the last line it saw.
func (s *Server) streamLogs(w http.ResponseWriter, r *http.Request) {
	targets, _, err := s.logTargets(r)
	if err != nil {
		writeErr(w, http.StatusNotFound, "%v", err)
		return
	}
	q := r.URL.Query()
	tail := q.Get("tail")
	if n, err := strconv.Atoi(tail); tail != "all" && (err != nil || n < 0) {
		tail = "200"
	}
	follow := q.Get("follow") != "0"
	since := q.Get("since")
	stackMode := q.Get("container") == ""
	send, ok := sse(w)
	if !ok {
		return
	}
	ctx := r.Context()
	out := make(chan any, 4096)
	var dropped int
	var dmu sync.Mutex
	push := func(v any) {
		select {
		case out <- v:
		default:
			dmu.Lock()
			dropped++
			dmu.Unlock()
		}
	}

	var mu sync.Mutex
	attached := map[string]bool{}
	lastTS := map[string]string{}
	var wg sync.WaitGroup
	attach := func(t logTarget, tail, since string) {
		mu.Lock()
		if attached[t.c.ID] {
			mu.Unlock()
			return
		}
		attached[t.c.ID] = true
		skip := lastTS[t.c.ID]
		mu.Unlock()
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { mu.Lock(); attached[t.c.ID] = false; mu.Unlock() }()
			tty, _ := s.Docker.IsTTY(ctx, t.c.ID)
			rc, err := s.Docker.Logs(ctx, t.c.ID, docker.LogOptions{Tail: tail, Since: since, Follow: follow, Timestamps: true})
			if err != nil {
				if ctx.Err() == nil {
					push(map[string]string{"info": t.c.Name + ": " + err.Error()})
				}
				return
			}
			defer rc.Close()
			_ = docker.ReadLines(rc, tty, func(stream, line string) {
				ts, msg, ok := strings.Cut(line, " ")
				if !ok {
					ts, msg = "", line
				}
				if skip != "" && ts <= skip { // since= is inclusive and second-granular
					return
				}
				mu.Lock()
				lastTS[t.c.ID] = ts
				mu.Unlock()
				push(LogLine{C: t.c.Name, S: t.service, T: ts, E: stream == "stderr", M: msg})
			})
			if follow && ctx.Err() == nil {
				push(map[string]string{"info": t.c.Name + " stopped"})
			}
		}()
	}

	started := time.Now()
	for _, t := range targets {
		attach(t, tail, since)
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()

	var rescan <-chan time.Time
	if follow {
		tk := time.NewTicker(2 * time.Second)
		defer tk.Stop()
		rescan = tk.C
	}
	flush := time.NewTicker(100 * time.Millisecond)
	defer flush.Stop()
	batch := make([]any, 0, 256)
	for {
		select {
		case <-ctx.Done():
			return
		case v := <-out:
			batch = append(batch, v)
			if len(batch) < 500 {
				continue
			}
		case <-flush.C:
		case <-rescan:
			again, _, err := s.logTargets(r)
			if err == nil {
				for _, t := range again {
					mu.Lock()
					isAttached, seen := attached[t.c.ID], lastTS[t.c.ID]
					mu.Unlock()
					if isAttached || t.c.State != "running" {
						continue
					}
					switch {
					case seen != "": // restarted: continue where we were
						attach(t, "all", seen[:min(len(seen), 19)]+"Z")
					case stackMode || !t.c.Created.Before(started): // new container (recreated)
						attach(t, "all", strconv.FormatInt(started.Unix(), 10))
					}
				}
			}
			continue
		case <-done:
			if !follow {
				for len(out) > 0 {
					batch = append(batch, <-out)
				}
				s.sendLogBatch(send, batch)
				send("end", map[string]bool{"end": true})
				return
			}
			done = nil // followers ended (all stopped): keep waiting for restarts
			continue
		}
		dmu.Lock()
		n := dropped
		dropped = 0
		dmu.Unlock()
		if n > 0 {
			batch = append(batch, map[string]string{"info": fmt.Sprintf("… %d lines skipped (too fast to show)", n)})
		}
		if len(batch) > 0 {
			if s.sendLogBatch(send, batch) != nil {
				return
			}
			batch = batch[:0]
		}
	}
}

// sendLogBatch sends lines as one "lines" event and notes as "info" events.
func (s *Server) sendLogBatch(send func(string, any) error, batch []any) error {
	lines := make([]LogLine, 0, len(batch))
	for _, v := range batch {
		switch x := v.(type) {
		case LogLine:
			lines = append(lines, x)
		case map[string]string:
			if len(lines) > 0 {
				if err := send("lines", lines); err != nil {
					return err
				}
				lines = lines[:0]
			}
			if err := send("info", x); err != nil {
				return err
			}
		}
	}
	if len(lines) > 0 {
		return send("lines", lines)
	}
	return nil
}

// downloadLogs writes the logs as plain text ("service | time | line"), sorted by time.
func (s *Server) downloadLogs(w http.ResponseWriter, r *http.Request) {
	targets, name, err := s.logTargets(r)
	if err != nil {
		writeErr(w, http.StatusNotFound, "%v", err)
		return
	}
	q := r.URL.Query()
	tail := q.Get("tail")
	if tail == "" {
		tail = "all"
	}
	var all []LogLine
	ctx, cancel := context.WithTimeout(r.Context(), time.Minute)
	defer cancel()
	for _, t := range targets {
		tty, _ := s.Docker.IsTTY(ctx, t.c.ID)
		rc, err := s.Docker.Logs(ctx, t.c.ID, docker.LogOptions{Tail: tail, Since: q.Get("since"), Until: q.Get("until"), Timestamps: true})
		if err != nil {
			continue
		}
		_ = docker.ReadLines(rc, tty, func(stream, line string) {
			ts, msg, _ := strings.Cut(line, " ")
			all = append(all, LogLine{C: t.c.Name, S: t.service, T: ts, E: stream == "stderr", M: msg})
		})
		rc.Close()
	}
	slices.SortStableFunc(all, func(a, b LogLine) int { return strings.Compare(a.T, b.T) })
	file := strings.NewReplacer("/", "_", "~", "").Replace(name) + "-" + time.Now().Format("20060102-150405") + ".log"
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="`+file+`"`)
	for _, l := range all {
		src := l.S
		if src == "" {
			src = l.C
		}
		fmt.Fprintf(w, "%s | %s | %s\n", src, l.T, l.M)
	}
}
