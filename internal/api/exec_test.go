package api

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"docker-manager/internal/config"
	"docker-manager/internal/docker"
)

// echoEngine is a fake TTY: everything typed comes back, prefixed.
type echoEngine struct {
	*fakeEngine
	mu      sync.Mutex
	resizes []uint
}

type pipeConn struct{ *io.PipeWriter }

var _ io.WriteCloser = pipeConn{}

func (e *echoEngine) ExecTTY(_ context.Context, id string, cmd []string, cols, rows uint) (*docker.ExecSession, error) {
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	go func() {
		buf := make([]byte, 1024)
		for {
			n, err := inR.Read(buf)
			if err != nil {
				outW.Close()
				return
			}
			outW.Write(append([]byte("tty:"), buf[:n]...))
		}
	}()
	return &docker.ExecSession{ID: "exec1", Conn: pipeConn{inW}, Output: outR}, nil
}

func (e *echoEngine) ExecResize(_ context.Context, _ string, cols, rows uint) error {
	e.mu.Lock()
	e.resizes = append(e.resizes, cols, rows)
	e.mu.Unlock()
	return nil
}

func TestExecShell(t *testing.T) {
	cfg := config.Default()
	env := newEnv(t, cfg)
	wsURL := "ws" + strings.TrimPrefix(env.srv.URL, "http") + "/api/exec/c1?cols=100&rows=30"

	// turned off by default
	res, _ := http.Get(env.srv.URL + "/api/exec/c1")
	res.Body.Close()
	if res.StatusCode != 403 {
		t.Fatalf("exec while disabled: %d", res.StatusCode)
	}

	env.s.Config.ExecEnabled = true
	ee := &echoEngine{fakeEngine: env.engine}
	env.s.Docker = ee
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.CloseNow()
	c.Write(ctx, websocket.MessageText, []byte(`{"type":"resize","cols":120,"rows":40}`))
	c.Write(ctx, websocket.MessageBinary, []byte("ls\n"))
	_, data, err := c.Read(ctx)
	if err != nil || string(data) != "tty:ls\n" {
		t.Fatalf("%q %v", data, err)
	}
	ee.mu.Lock()
	if len(ee.resizes) != 2 || ee.resizes[0] != 120 {
		t.Errorf("resizes %v", ee.resizes)
	}
	ee.mu.Unlock()
	c.Close(websocket.StatusNormalClosure, "")
	time.Sleep(100 * time.Millisecond)
	entries, _ := env.s.Store.AuditLog(context.Background(), 0, 10)
	if len(entries) < 2 || entries[0].Action != "exec.end" || entries[1].Action != "exec.start" {
		t.Errorf("audit %+v", entries)
	}
}
