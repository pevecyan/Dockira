package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/coder/websocket"

	"docker-manager/internal/docker"
)

// ExecEngine opens interactive shells (the Docker client; absent in tests without it).
type ExecEngine interface {
	ExecTTY(ctx context.Context, id string, cmd []string, cols, rows uint) (*docker.ExecSession, error)
	ExecResize(ctx context.Context, execID string, cols, rows uint) error
}

// shell prefers bash and falls back to sh.
var defaultShell = []string{"sh", "-c", "if command -v bash >/dev/null 2>&1; then exec bash; else exec sh; fi"}

// execWS is a terminal in a container over a WebSocket: binary messages are keystrokes and
// output, text messages are control ({"type":"resize","cols":…,"rows":…}). Only with
// exec_enabled in the config and a fresh password confirmation; every session is audited.
func (s *Server) execWS(w http.ResponseWriter, r *http.Request) {
	if !s.Config.ExecEnabled {
		writeErr(w, http.StatusForbidden, "the container shell is turned off (exec_enabled in config.yaml)")
		return
	}
	if s.Auth != nil && !s.Auth.Confirmed(r) {
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(http.StatusForbidden)
		json.NewEncoder(w).Encode(map[string]any{"status": 403, "title": "Forbidden", "detail": "confirm your password to continue", "confirm": true})
		return
	}
	ee, ok := s.Docker.(ExecEngine)
	if !ok {
		writeErr(w, http.StatusNotImplemented, "not available")
		return
	}
	id := r.PathValue("id")
	var target docker.Container
	stackID := ""
	for _, st := range s.Stacks.Stacks() {
		for _, sv := range st.Services {
			for _, c := range sv.Containers {
				if c.ID == id || c.Name == id {
					target, stackID = c, st.ID
				}
			}
		}
	}
	for _, c := range s.Stacks.Standalone() {
		if c.ID == id || c.Name == id {
			target = c
		}
	}
	if target.ID == "" {
		writeErr(w, http.StatusNotFound, "no container %s", id)
		return
	}
	if target.State != "running" {
		writeErr(w, http.StatusConflict, "%s is not running", target.Name)
		return
	}
	q := r.URL.Query()
	cols, _ := strconv.ParseUint(q.Get("cols"), 10, 16)
	rows, _ := strconv.ParseUint(q.Get("rows"), 10, 16)
	cols, rows = max(cols, 20), max(rows, 5)
	cmd := defaultShell
	if c := q.Get("cmd"); c != "" {
		cmd = []string{"sh", "-c", c}
	}

	// the Origin was checked by the auth middleware (it knows about proxies and public_url)
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
	if err != nil {
		return
	}
	defer conn.CloseNow()
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	sess, err := ee.ExecTTY(ctx, target.ID, cmd, uint(cols), uint(rows))
	if err != nil {
		conn.Close(websocket.StatusInternalError, "exec: "+err.Error())
		return
	}
	defer sess.Close()
	started := time.Now()
	s.audit(ctx, "exec.start", target.Name, map[string]any{"stack": stackID, "cmd": q.Get("cmd")}, nil)
	defer func() {
		s.audit(context.WithoutCancel(ctx), "exec.end", target.Name, map[string]any{"stack": stackID, "seconds": int(time.Since(started).Seconds())}, nil)
	}()

	go func() { // container → browser
		defer cancel()
		buf := make([]byte, 32<<10)
		for {
			n, err := sess.Output.Read(buf)
			if n > 0 {
				if conn.Write(ctx, websocket.MessageBinary, buf[:n]) != nil {
					return
				}
			}
			if err != nil {
				if err == io.EOF {
					conn.Close(websocket.StatusNormalClosure, "the shell exited")
				}
				return
			}
		}
	}()
	for { // browser → container
		typ, data, err := conn.Read(ctx)
		if err != nil {
			return
		}
		if typ == websocket.MessageText {
			var m struct {
				Type       string `json:"type"`
				Cols, Rows uint
			}
			if json.Unmarshal(data, &m) == nil && m.Type == "resize" && m.Cols > 0 && m.Rows > 0 {
				_ = ee.ExecResize(ctx, sess.ID, m.Cols, m.Rows)
			}
			continue
		}
		if _, err := sess.Conn.Write(data); err != nil {
			return
		}
	}
}
