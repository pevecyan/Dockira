// Package jobs runs the long operations (compose up/pull/down, updates, rebuilds) in the
// background: one job per stack at a time, a few stacks in parallel, live output for the UI,
// cancellation that kills the whole process group, and a row in SQLite when it ends.
package jobs

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	Queued    = "queued"
	Running   = "running"
	Succeeded = "succeeded"
	Failed    = "failed"
	Cancelled = "cancelled"

	maxOutput = 256 << 10 // bytes kept per job
	maxLine   = 8 << 10
)

type Job struct {
	ID        int64     `json:"id"`
	StackID   string    `json:"stack_id"`
	Kind      string    `json:"kind"`
	Title     string    `json:"title"`
	Services  []string  `json:"services"`
	Trigger   string    `json:"trigger" enum:"user,auto,schedule"`
	State     string    `json:"state" enum:"queued,running,succeeded,failed,cancelled"`
	ExitCode  int       `json:"exit_code"`
	Error     string    `json:"error,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	StartedAt time.Time `json:"started_at,omitzero"`
	EndedAt   time.Time `json:"ended_at,omitzero"`
}

func (j Job) Done() bool { return j.State == Succeeded || j.State == Failed || j.State == Cancelled }

type Spec struct {
	StackID  string
	Kind     string
	Title    string
	Services []string
	Trigger  string   // default "user"
	Secrets  []string // values scrubbed from the output (the stack's .env values)
	Timeout  time.Duration
	Run      func(ctx context.Context, out *Output) error
}

// Publisher is the event hub (job state changes go to every UI).
type Publisher interface {
	Publish(name string, data any)
}

type Runner struct {
	db       *sql.DB
	hub      Publisher
	log      *slog.Logger
	sem      chan struct{}
	OnFinish func(Job) // audit, notifications

	mu    sync.Mutex
	live  map[int64]*live
	locks map[string]chan struct{}
	wg    sync.WaitGroup
}

type live struct {
	mu     sync.Mutex
	job    Job
	lines  []string
	size   int
	cut    int // lines dropped from the front
	subs   map[chan string]struct{}
	done   chan struct{}
	cancel context.CancelFunc
}

// New marks jobs a previous process left unfinished as failed.
func New(db *sql.DB, hub Publisher, log *slog.Logger, parallel int) *Runner {
	if parallel < 1 {
		parallel = 2
	}
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	_, _ = db.Exec(`UPDATE jobs SET state = 'failed', error = 'interrupted: Dockira restarted', ended_at = ?
		WHERE state IN ('queued', 'running')`, ts(time.Now()))
	return &Runner{db: db, hub: hub, log: log, sem: make(chan struct{}, parallel), live: map[int64]*live{}, locks: map[string]chan struct{}{}}
}

// Submit queues a job and returns at once.
func (r *Runner) Submit(spec Spec) (Job, error) {
	if spec.Trigger == "" {
		spec.Trigger = "user"
	}
	if spec.Services == nil {
		spec.Services = []string{}
	}
	j := Job{StackID: spec.StackID, Kind: spec.Kind, Title: spec.Title, Services: spec.Services, Trigger: spec.Trigger,
		State: Queued, CreatedAt: time.Now()}
	svc, _ := json.Marshal(j.Services)
	res, err := r.db.Exec(`INSERT INTO jobs (stack_id, kind, title, services, trigger, state, created_at) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		j.StackID, j.Kind, j.Title, string(svc), j.Trigger, j.State, ts(j.CreatedAt))
	if err != nil {
		return j, err
	}
	j.ID, _ = res.LastInsertId()
	ctx, cancel := context.WithCancel(context.Background())
	if spec.Timeout > 0 {
		ctx, cancel = context.WithTimeout(context.Background(), spec.Timeout)
	}
	l := &live{job: j, subs: map[chan string]struct{}{}, done: make(chan struct{}), cancel: cancel}
	r.mu.Lock()
	r.live[j.ID] = l
	r.mu.Unlock()
	r.publish(j)
	r.wg.Add(1)
	go r.run(ctx, l, spec)
	return j, nil
}

func (r *Runner) run(ctx context.Context, l *live, spec Spec) {
	defer r.wg.Done()
	defer l.cancel()
	out := &Output{l: l, secrets: secrets(spec.Secrets)}

	// one job per stack: wait for the stack's lock, then for a free slot
	if spec.StackID != "" {
		lock := r.stackLock(spec.StackID)
		select {
		case lock <- struct{}{}:
			defer func() { <-lock }()
		case <-ctx.Done():
			r.finish(l, out, ctx.Err())
			return
		}
	}
	select {
	case r.sem <- struct{}{}:
		defer func() { <-r.sem }()
	case <-ctx.Done():
		r.finish(l, out, ctx.Err())
		return
	}

	l.mu.Lock()
	l.job.State, l.job.StartedAt = Running, time.Now()
	j := l.job
	l.mu.Unlock()
	_, _ = r.db.Exec(`UPDATE jobs SET state = ?, started_at = ? WHERE id = ?`, j.State, ts(j.StartedAt), j.ID)
	r.publish(j)

	err := func() (err error) {
		defer func() {
			if p := recover(); p != nil {
				err = fmt.Errorf("internal error: %v", p)
			}
		}()
		return spec.Run(ctx, out)
	}()
	if ctx.Err() != nil { // killed by cancel/timeout: report that, not "signal: killed"
		err = ctx.Err()
	}
	r.finish(l, out, err)
}

func (r *Runner) stackLock(id string) chan struct{} {
	r.mu.Lock()
	defer r.mu.Unlock()
	l, ok := r.locks[id]
	if !ok {
		l = make(chan struct{}, 1)
		r.locks[id] = l
	}
	return l
}

func (r *Runner) finish(l *live, out *Output, err error) {
	out.flush()
	l.mu.Lock()
	j := &l.job
	j.EndedAt = time.Now()
	switch {
	case err == nil:
		j.State = Succeeded
	case errors.Is(err, context.Canceled):
		j.State, j.Error = Cancelled, "cancelled"
	case errors.Is(err, context.DeadlineExceeded):
		j.State, j.Error = Failed, "timed out"
	default:
		j.State, j.Error = Failed, out.scrub(err.Error())
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			j.ExitCode = ee.ExitCode()
		}
	}
	if j.ExitCode == 0 && j.State == Failed {
		j.ExitCode = 1
	}
	text := l.text()
	done := *j
	for ch := range l.subs {
		close(ch)
	}
	l.subs = map[chan string]struct{}{}
	l.mu.Unlock()
	defer close(l.done) // Wait returns once the row is saved and listeners ran

	if _, dbErr := r.db.Exec(`UPDATE jobs SET state = ?, exit_code = ?, error = ?, output = ?, started_at = ?, ended_at = ? WHERE id = ?`,
		done.State, done.ExitCode, done.Error, text, ts(done.StartedAt), ts(done.EndedAt), done.ID); dbErr != nil {
		r.log.Warn("save job", "id", done.ID, "err", dbErr)
	}
	r.publish(done)
	if r.OnFinish != nil {
		r.OnFinish(done)
	}
	// keep the finished job in memory briefly so late subscribers get the output without a DB read
	time.AfterFunc(2*time.Minute, func() {
		r.mu.Lock()
		delete(r.live, done.ID)
		r.mu.Unlock()
	})
}

func (r *Runner) publish(j Job) {
	if r.hub != nil {
		r.hub.Publish("job", j)
	}
}

// Cancel stops a queued or running job.
func (r *Runner) Cancel(id int64) bool {
	r.mu.Lock()
	l, ok := r.live[id]
	r.mu.Unlock()
	if !ok {
		return false
	}
	l.mu.Lock()
	done := l.job.Done()
	l.mu.Unlock()
	if done {
		return false
	}
	l.cancel()
	return true
}

// Wait blocks until job id is done (or ctx ends) and returns it.
func (r *Runner) Wait(ctx context.Context, id int64) (Job, error) {
	r.mu.Lock()
	l, ok := r.live[id]
	r.mu.Unlock()
	if ok {
		select {
		case <-l.done:
		case <-ctx.Done():
			return Job{}, ctx.Err()
		}
	}
	j, _, err := r.Get(ctx, id)
	return j, err
}

// Shutdown cancels everything and waits.
func (r *Runner) Shutdown() {
	r.mu.Lock()
	for _, l := range r.live {
		l.cancel()
	}
	r.mu.Unlock()
	r.wg.Wait()
}

// Subscribe returns the output so far and a channel with the following lines; the channel is
// closed when the job ends. ok is false for jobs that are not live (read them with Get).
func (r *Runner) Subscribe(id int64) (lines []string, ch <-chan string, unsubscribe func(), ok bool) {
	r.mu.Lock()
	l, found := r.live[id]
	r.mu.Unlock()
	if !found {
		return nil, nil, func() {}, false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	lines = append([]string(nil), l.lines...)
	if l.cut > 0 {
		lines = append([]string{fmt.Sprintf("[… %d earlier lines dropped …]", l.cut)}, lines...)
	}
	c := make(chan string, 4096)
	if l.job.Done() {
		close(c)
		return lines, c, func() {}, true
	}
	l.subs[c] = struct{}{}
	return lines, c, func() {
		l.mu.Lock()
		if _, ok := l.subs[c]; ok {
			delete(l.subs, c)
			close(c)
		}
		l.mu.Unlock()
	}, true
}

// Get returns a job and its output, live or from the database.
func (r *Runner) Get(ctx context.Context, id int64) (Job, string, error) {
	r.mu.Lock()
	l, ok := r.live[id]
	r.mu.Unlock()
	if ok {
		l.mu.Lock()
		defer l.mu.Unlock()
		return l.job, l.text(), nil
	}
	rows, err := r.query(ctx, `WHERE id = ?`, []any{id}, true)
	if err != nil {
		return Job{}, "", err
	}
	if len(rows) == 0 {
		return Job{}, "", sql.ErrNoRows
	}
	return rows[0].Job, rows[0].output, nil
}

type row struct {
	Job
	output string
}

// List returns the latest jobs, optionally for one stack and/or before an ID, newest first.
func (r *Runner) List(ctx context.Context, stackID string, before int64, limit int) ([]Job, error) {
	where, args := []string{}, []any{}
	if stackID != "" {
		where, args = append(where, "stack_id = ?"), append(args, stackID)
	}
	if before > 0 {
		where, args = append(where, "id < ?"), append(args, before)
	}
	q := ""
	if len(where) > 0 {
		q = "WHERE " + strings.Join(where, " AND ")
	}
	rows, err := r.query(ctx, q+" ORDER BY id DESC LIMIT ?", append(args, limit), false)
	if err != nil {
		return nil, err
	}
	out := make([]Job, 0, len(rows))
	r.mu.Lock()
	for _, row := range rows {
		if l, ok := r.live[row.ID]; ok { // live state is fresher than the row
			l.mu.Lock()
			row.Job = l.job
			l.mu.Unlock()
		}
		out = append(out, row.Job)
	}
	r.mu.Unlock()
	return out, nil
}

// Active returns queued and running jobs.
func (r *Runner) Active() []Job {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []Job
	for _, l := range r.live {
		l.mu.Lock()
		if !l.job.Done() {
			out = append(out, l.job)
		}
		l.mu.Unlock()
	}
	return out
}

func (r *Runner) query(ctx context.Context, where string, args []any, withOutput bool) ([]row, error) {
	cols := `id, stack_id, kind, title, services, trigger, state, exit_code, error, created_at, started_at, ended_at`
	if withOutput {
		cols += `, output`
	}
	rows, err := r.db.QueryContext(ctx, `SELECT `+cols+` FROM jobs `+where, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []row
	for rows.Next() {
		var x row
		var svc, created, started, ended string
		dest := []any{&x.ID, &x.StackID, &x.Kind, &x.Title, &svc, &x.Trigger, &x.State, &x.ExitCode, &x.Error, &created, &started, &ended}
		if withOutput {
			dest = append(dest, &x.output)
		}
		if err := rows.Scan(dest...); err != nil {
			return nil, err
		}
		_ = json.Unmarshal([]byte(svc), &x.Services)
		if x.Services == nil {
			x.Services = []string{}
		}
		x.CreatedAt, x.StartedAt, x.EndedAt = parse(created), parse(started), parse(ended)
		out = append(out, x)
	}
	return out, rows.Err()
}

func ts(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339Nano)
}

func parse(s string) time.Time {
	t, _ := time.Parse(time.RFC3339Nano, s)
	return t
}

func (l *live) text() string {
	var b strings.Builder
	if l.cut > 0 {
		fmt.Fprintf(&b, "[… %d earlier lines dropped …]\n", l.cut)
	}
	for _, s := range l.lines {
		b.WriteString(s)
		b.WriteByte('\n')
	}
	return b.String()
}

// Output collects a job's output line by line (compose writes progress to stderr, so both
// streams go here), scrubbing secrets and capping the size.
type Output struct {
	l       *live
	mu      sync.Mutex
	partial bytes.Buffer
	secrets []string
}

func secrets(vals []string) []string {
	var out []string
	for _, v := range vals {
		if len(v) >= 4 { // short values (ports, "true") would scrub half the output
			out = append(out, v)
		}
	}
	return out
}

func (o *Output) scrub(s string) string {
	for _, v := range o.secrets {
		s = strings.ReplaceAll(s, v, "•••••")
	}
	return s
}

func (o *Output) Write(p []byte) (int, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.partial.Write(p)
	for {
		b := o.partial.Bytes()
		i := bytes.IndexAny(b, "\n\r")
		if i < 0 {
			if o.partial.Len() > maxLine {
				o.line(string(b[:maxLine]))
				o.partial.Next(o.partial.Len())
			}
			break
		}
		o.line(string(b[:i]))
		o.partial.Next(i + 1)
	}
	return len(p), nil
}

// Printf adds a line of its own (step headers: "▸ pulling images").
func (o *Output) Printf(format string, a ...any) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.line(fmt.Sprintf(format, a...))
}

func (o *Output) flush() {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.partial.Len() > 0 {
		o.line(o.partial.String())
		o.partial.Reset()
	}
}

func (o *Output) line(s string) {
	s = strings.TrimRight(s, " \t")
	if s == "" {
		return
	}
	if len(s) > maxLine {
		s = s[:maxLine]
	}
	s = o.scrub(s)
	l := o.l
	l.mu.Lock()
	defer l.mu.Unlock()
	if n := len(l.lines); n > 0 && l.lines[n-1] == s {
		return // compose's plain progress prints many events twice in a row
	}
	l.lines = append(l.lines, s)
	l.size += len(s) + 1
	for l.size > maxOutput && len(l.lines) > 1 {
		l.size -= len(l.lines[0]) + 1
		l.lines = l.lines[1:]
		l.cut++
	}
	for ch := range l.subs {
		select {
		case ch <- s:
		default: // a stuck reader loses lines rather than stalling the job
		}
	}
}

// Exec runs a command with its output going to the job. The command gets its own process
// group, and cancelling kills the whole group (compose starts helpers that would otherwise
// keep the output pipe open).
func (o *Output) Exec(cmd *exec.Cmd) error {
	cmd.Stdout, cmd.Stderr = o, o
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	if cmd.WaitDelay == 0 {
		cmd.WaitDelay = 5 * time.Second
	}
	err := cmd.Run()
	o.flush()
	return err
}

var _ io.Writer = (*Output)(nil)

// JobID is the ID of the job this output belongs to.
func (o *Output) JobID() int64 {
	o.l.mu.Lock()
	defer o.l.mu.Unlock()
	return o.l.job.ID
}
