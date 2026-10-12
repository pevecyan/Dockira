package jobs

import (
	"context"
	"errors"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"docker-manager/internal/store"
)

type pub struct {
	mu   sync.Mutex
	jobs []Job
}

func (p *pub) Publish(name string, data any) {
	p.mu.Lock()
	p.jobs = append(p.jobs, data.(Job))
	p.mu.Unlock()
}

func newRunner(t *testing.T, parallel int) (*Runner, *pub) {
	t.Helper()
	st, err := store.OpenFile(context.Background(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	p := &pub{}
	return New(st.DB, p, nil, parallel), p
}

func TestJobLifecycleAndOutput(t *testing.T) {
	r, p := newRunner(t, 2)
	ctx := context.Background()
	var finished atomic.Int32
	r.OnFinish = func(Job) { finished.Add(1) }
	j, err := r.Submit(Spec{StackID: "web", Kind: "up", Title: "Start web", Secrets: []string{"hunter22", "on"},
		Run: func(ctx context.Context, out *Output) error {
			out.Printf("▸ starting")
			return out.Exec(exec.CommandContext(ctx, "sh", "-c", `echo "token is hunter22"; echo "state on" >&2; printf 'no newline'`))
		}})
	if err != nil || j.ID == 0 || j.State != Queued {
		t.Fatalf("%+v %v", j, err)
	}
	done, err := r.Wait(ctx, j.ID)
	if err != nil || done.State != Succeeded || done.StartedAt.IsZero() || done.EndedAt.IsZero() {
		t.Fatalf("%+v %v", done, err)
	}
	_, out, _ := r.Get(ctx, j.ID)
	if out != "▸ starting\ntoken is •••••\nstate on\nno newline\n" {
		t.Errorf("output %q", out)
	}
	if finished.Load() != 1 {
		t.Error("OnFinish not called")
	}
	p.mu.Lock()
	states := []string{}
	for _, j := range p.jobs {
		states = append(states, j.State)
	}
	p.mu.Unlock()
	if strings.Join(states, ",") != "queued,running,succeeded" {
		t.Errorf("published %v", states)
	}

	// from the database after the live entry is gone
	r.mu.Lock()
	delete(r.live, j.ID)
	r.mu.Unlock()
	dbJob, dbOut, err := r.Get(ctx, j.ID)
	if err != nil || dbJob.State != Succeeded || dbOut != out || dbJob.Title != "Start web" {
		t.Errorf("db: %+v %q %v", dbJob, dbOut, err)
	}
	list, _ := r.List(ctx, "web", 0, 10)
	if len(list) != 1 || list[0].ID != j.ID {
		t.Errorf("list %+v", list)
	}
}

func TestFailureExitCodeAndPanic(t *testing.T) {
	r, _ := newRunner(t, 2)
	ctx := context.Background()
	j, _ := r.Submit(Spec{Kind: "x", Run: func(ctx context.Context, out *Output) error {
		return out.Exec(exec.CommandContext(ctx, "sh", "-c", "echo broken >&2; exit 3"))
	}})
	if d, _ := r.Wait(ctx, j.ID); d.State != Failed || d.ExitCode != 3 {
		t.Errorf("%+v", d)
	}
	j, _ = r.Submit(Spec{Kind: "x", Run: func(context.Context, *Output) error { panic("boom") }})
	if d, _ := r.Wait(ctx, j.ID); d.State != Failed || !strings.Contains(d.Error, "boom") || d.ExitCode != 1 {
		t.Errorf("%+v", d)
	}
	j, _ = r.Submit(Spec{Kind: "x", Run: func(context.Context, *Output) error { return errors.New("plain") }})
	if d, _ := r.Wait(ctx, j.ID); d.State != Failed || d.Error != "plain" {
		t.Errorf("%+v", d)
	}
}

func TestCancelKillsProcessGroup(t *testing.T) {
	r, _ := newRunner(t, 2)
	ctx := context.Background()
	started := make(chan struct{})
	j, _ := r.Submit(Spec{StackID: "s", Kind: "pull", Run: func(ctx context.Context, out *Output) error {
		close(started)
		cmd := exec.CommandContext(ctx, "sh", "-c", "sleep 30 & wait")
		return out.Exec(cmd)
	}})
	<-started
	time.Sleep(50 * time.Millisecond)
	t0 := time.Now()
	if !r.Cancel(j.ID) {
		t.Fatal("cancel refused")
	}
	d, _ := r.Wait(ctx, j.ID)
	if d.State != Cancelled || time.Since(t0) > 8*time.Second {
		t.Errorf("%+v after %s", d, time.Since(t0))
	}
	if r.Cancel(j.ID) {
		t.Error("cancelled a finished job")
	}
}

func TestOneJobPerStack(t *testing.T) {
	r, _ := newRunner(t, 4)
	ctx := context.Background()
	var running, maxRunning atomic.Int32
	run := func(ctx context.Context, out *Output) error {
		n := running.Add(1)
		for {
			m := maxRunning.Load()
			if n <= m || maxRunning.CompareAndSwap(m, n) {
				break
			}
		}
		time.Sleep(60 * time.Millisecond)
		running.Add(-1)
		return nil
	}
	var ids []int64
	for range 3 {
		j, _ := r.Submit(Spec{StackID: "same", Kind: "x", Run: run})
		ids = append(ids, j.ID)
	}
	for _, id := range ids {
		r.Wait(ctx, id)
	}
	if maxRunning.Load() != 1 {
		t.Errorf("%d jobs ran at once on one stack", maxRunning.Load())
	}
	// different stacks run in parallel
	maxRunning.Store(0)
	ids = nil
	for _, s := range []string{"a", "b", "c"} {
		j, _ := r.Submit(Spec{StackID: s, Kind: "x", Run: run})
		ids = append(ids, j.ID)
	}
	for _, id := range ids {
		r.Wait(ctx, id)
	}
	if maxRunning.Load() < 2 {
		t.Errorf("stacks did not run in parallel: max %d", maxRunning.Load())
	}
}

func TestSubscribeAndCap(t *testing.T) {
	r, _ := newRunner(t, 1)
	ctx := context.Background()
	gate := make(chan struct{})
	j, _ := r.Submit(Spec{Kind: "x", Run: func(ctx context.Context, out *Output) error {
		out.Printf("first")
		<-gate
		for i := range 40000 { // ~10 bytes × 40000 > maxOutput
			out.Printf("line %05d", i)
		}
		return nil
	}})
	time.Sleep(30 * time.Millisecond)
	lines, ch, unsub, ok := r.Subscribe(j.ID)
	defer unsub()
	if !ok || len(lines) != 1 || lines[0] != "first" {
		t.Fatalf("snapshot %v %v", lines, ok)
	}
	close(gate)
	n := 0
	for range ch {
		n++
	}
	if n == 0 {
		t.Error("no live lines")
	}
	r.Wait(ctx, j.ID)
	_, out, _ := r.Get(ctx, j.ID)
	if len(out) > maxOutput+200 || !strings.HasPrefix(out, "[… ") || !strings.HasSuffix(out, "line 39999\n") {
		t.Errorf("capped output: %d bytes, starts %q", len(out), out[:30])
	}
}

func TestRestartMarksInterrupted(t *testing.T) {
	st, _ := store.OpenFile(context.Background(), ":memory:")
	defer st.Close()
	st.DB.Exec(`INSERT INTO jobs (kind, title, state, created_at) VALUES ('up', 'x', 'running', '2026-01-01T00:00:00Z')`)
	r := New(st.DB, nil, nil, 1)
	j, _, err := r.Get(context.Background(), 1)
	if err != nil || j.State != Failed || !strings.Contains(j.Error, "interrupted") {
		t.Errorf("%+v %v", j, err)
	}
}
