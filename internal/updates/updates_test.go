package updates

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"docker-manager/internal/docker"
	"docker-manager/internal/jobs"
	"docker-manager/internal/notify"
	"docker-manager/internal/registry"
	"docker-manager/internal/stacks"
	"docker-manager/internal/store"
)

// world is a tiny simulated Docker host: images by ref, one container per service, and a
// registry. compose pull/up change it like the real thing would.
type world struct {
	mu        sync.Mutex
	remote    map[string]string // ref → digest in the registry
	remoteErr map[string]error
	images    map[string]docker.Image // ref or tag → image
	ctr       map[string]*docker.Details
	nextBad   bool // the next `up` starts a container that exits
	tags      []string
	removed   []string
	composed  []string
	stack     stacks.Stack
}

func newWorld(dir string) *world {
	w := &world{remote: map[string]string{}, remoteErr: map[string]error{}, images: map[string]docker.Image{}, ctr: map[string]*docker.Details{}}
	w.remote["nginx:1"] = dg("n1")
	w.images["nginx:1"] = docker.Image{ID: "sha256:img-n1", RepoDigests: []string{"nginx@" + dg("n1")}}
	w.images["postgres:16"] = docker.Image{ID: "sha256:img-p1", RepoDigests: []string{"postgres@" + dg("p1")}}
	w.remote["postgres:16"] = dg("p1")
	w.remote["alpine:3.20"] = dg("a2")
	w.images["alpine:3.20"] = docker.Image{ID: "sha256:img-a1", RepoDigests: []string{"alpine@" + dg("a1")}}
	old := time.Now().Add(-time.Hour)
	w.ctr["web"] = &docker.Details{Container: docker.Container{ID: "c-web", Name: "app-web-1", State: "running", ImageID: "sha256:img-n1"}, StartedAt: old}
	w.ctr["db"] = &docker.Details{Container: docker.Container{ID: "c-db", Name: "app-db-1", State: "running", Health: "healthy", ImageID: "sha256:img-p1"}, StartedAt: old}
	w.ctr["app"] = &docker.Details{Container: docker.Container{ID: "c-app", Name: "app-app-1", State: "running", ImageID: "sha256:img-built"}, StartedAt: old}
	os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte("ARG V=3.20\nFROM alpine:${V}\n"), 0o644)
	w.stack = stacks.Stack{ID: "app", Name: "app", Dir: dir, Status: "running", Services: []stacks.Service{
		{Name: "web", Image: "nginx:1", InFile: true},
		{Name: "db", Image: "postgres:16", InFile: true, Stateful: true, Healthcheck: true},
		{Name: "app", Image: "app-app", InFile: true, Build: true, BuildContext: dir, Dockerfile: "Dockerfile"},
	}}
	return w
}

func (w *world) Stacks() []stacks.Stack {
	st, _ := w.Get("app")
	return []stacks.Stack{st}
}

func (w *world) Get(id string) (stacks.Stack, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	st := w.stack
	st.Services = append([]stacks.Service(nil), st.Services...)
	for i := range st.Services {
		if c, ok := w.ctr[st.Services[i].Name]; ok {
			st.Services[i].Containers = []docker.Container{c.Container}
		}
	}
	return st, id == "app"
}

func (w *world) RefreshContainers(context.Context) error { return nil }

func (w *world) ImageInspect(_ context.Context, ref string) (docker.Image, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if img, ok := w.images[ref]; ok {
		return img, nil
	}
	for _, img := range w.images {
		if img.ID == ref {
			return img, nil
		}
	}
	return docker.Image{}, errors.New("not found")
}

func (w *world) ImageTag(_ context.Context, source, target string) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.tags = append(w.tags, source+"→"+target)
	img, ok := w.images[source]
	if !ok {
		img = docker.Image{ID: source}
		for _, i := range w.images { // by ID, like docker: the image keeps its digests
			if i.ID == source {
				img = i
			}
		}
	}
	w.images[target] = img
	return nil
}

func (w *world) ImageRemove(_ context.Context, ref string) error {
	w.mu.Lock()
	w.removed = append(w.removed, ref)
	delete(w.images, ref)
	w.mu.Unlock()
	return nil
}

func (w *world) Inspect(_ context.Context, id string) (docker.Details, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, c := range w.ctr {
		if c.ID == id {
			return *c, nil
		}
	}
	return docker.Details{}, errors.New("no container")
}

func (w *world) Exec(_ context.Context, id string, cmd []string, out, errOut io.Writer) (int, error) {
	if strings.Contains(strings.Join(cmd, " "), "pg_dumpall") {
		io.WriteString(out, "-- dump\n")
		return 0, nil
	}
	if strings.Contains(strings.Join(cmd, " "), "fail") {
		return 3, nil
	}
	return 0, nil
}

func (w *world) CheckAll(_ context.Context, refs []registry.Ref) map[string]registry.Result {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := map[string]registry.Result{}
	for _, r := range refs {
		fam := r.Familiar()
		out[r.String()] = registry.Result{Ref: r, Digest: w.remote[fam], Err: w.remoteErr[fam]}
	}
	return out
}

func (w *world) Tags(context.Context, registry.Ref) ([]string, error) {
	return []string{"16", "16.4", "17"}, nil
}

// Cmd simulates compose: pull fetches the registry's image, up recreates changed containers.
func (w *world) Cmd(ctx context.Context, dir string, files []string, args ...string) *exec.Cmd {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.composed = append(w.composed, strings.Join(args, " "))
	switch args[0] {
	case "pull":
		for _, svc := range args[1:] {
			for _, sv := range w.stack.Services {
				if sv.Name == svc {
					d := w.remote[sv.Image]
					w.images[sv.Image] = docker.Image{ID: "sha256:img-" + labels[d], RepoDigests: []string{strings.Split(sv.Image, ":")[0] + "@" + d}}
				}
			}
		}
	case "up":
		for _, svc := range args[1:] {
			for _, sv := range w.stack.Services {
				if sv.Name != svc {
					continue
				}
				c := w.ctr[svc]
				if img, ok := w.images[sv.Image]; ok && img.ID != c.ImageID {
					c.ImageID, c.StartedAt, c.RestartCount = img.ID, time.Now().Add(-time.Minute), 0
					c.State = "running"
					if w.nextBad {
						c.State, c.ExitCode = "exited", 1
						w.nextBad = false
					}
				}
			}
		}
	}
	return exec.CommandContext(ctx, "true")
}

type notes struct {
	mu sync.Mutex
	ev []notify.Event
}

func (n *notes) add(e notify.Event) { n.mu.Lock(); n.ev = append(n.ev, e); n.mu.Unlock() }
func (n *notes) types() string {
	n.mu.Lock()
	defer n.mu.Unlock()
	var t []string
	for _, e := range n.ev {
		t = append(t, e.Type)
	}
	return strings.Join(t, ",")
}

func setup(t *testing.T) (*Manager, *world, *notes, *jobs.Runner) {
	t.Helper()
	st, err := store.OpenFile(context.Background(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	w := newWorld(t.TempDir())
	n := &notes{}
	r := jobs.New(st.DB, nil, nil, 2)
	m := &Manager{Store: st, Engine: w, Registry: w, Stacks: w, Compose: w, Jobs: r, Notify: n.add, DataDir: t.TempDir(),
		Log: nil, attempted: map[string]time.Time{}}
	m.Log = discard()
	set := DefaultSettings()
	set.VerifySeconds = 20
	set.CooldownHours = 0
	m.SaveSettings(context.Background(), set)
	return m, w, n, r
}

func states(t *testing.T, m *Manager) map[string]ServiceUpdate {
	t.Helper()
	svcs, err := m.Services(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]ServiceUpdate{}
	for _, s := range svcs {
		out[s.Service] = s
	}
	return out
}

func TestCheckStatesAndNotifications(t *testing.T) {
	m, w, n, _ := setup(t)
	ctx := context.Background()
	if s := states(t, m); s["web"].State != NotChecked {
		t.Fatalf("before a check: %+v", s["web"])
	}
	if err := m.Check(ctx, "", nil); err != nil {
		t.Fatal(err)
	}
	s := states(t, m)
	if s["web"].State != UpToDate || s["db"].State != UpToDate || s["app"].State != BaseUpdated {
		t.Errorf("states web=%s db=%s app=%s", s["web"].State, s["db"].State, s["app"].State)
	}
	if s["db"].HintSame != "16.4" || s["db"].HintMajor != "17" {
		t.Errorf("hints %q %q", s["db"].HintSame, s["db"].HintMajor)
	}
	if len(s["app"].Base) != 1 || s["app"].Base[0].Image != "alpine:3.20" {
		t.Errorf("base %+v", s["app"].Base)
	}
	if n.types() != "update.available" {
		t.Fatalf("notifications %q", n.types())
	}

	w.mu.Lock()
	w.remote["nginx:1"] = dg("n2")
	w.remoteErr["postgres:16"] = errors.New("UNAUTHORIZED: authentication required")
	w.mu.Unlock()
	m.Check(ctx, "", nil)
	s = states(t, m)
	if s["web"].State != UpdateAvailable || s["db"].State != Unknown || s["db"].ErrorKind == "" {
		t.Errorf("after registry change: web=%s db=%s (%s)", s["web"].State, s["db"].State, s["db"].ErrorKind)
	}
	if n.types() != "update.available,update.available" || !strings.Contains(n.ev[1].Message, "app/web") || strings.Contains(n.ev[1].Message, "alpine") {
		t.Errorf("second notification should only name the new update: %q / %s", n.types(), n.ev[len(n.ev)-1].Message)
	}
	m.Check(ctx, "", nil)
	if strings.Count(n.types(), "update.available") != 2 {
		t.Errorf("same update announced twice: %s", n.types())
	}

	// pulled but not applied: the local image is new, the container runs the old one
	w.mu.Lock()
	w.images["nginx:1"] = docker.Image{ID: "sha256:img-n2", RepoDigests: []string{"nginx@" + dg("n2")}}
	w.mu.Unlock()
	m.Check(ctx, "app", nil)
	if s := states(t, m); s["web"].State != PulledNotApplied {
		t.Errorf("pulled not applied: %s", s["web"].State)
	}
	sum := m.Summary(ctx)
	if sum.Available != 2 || sum.Errors != 1 {
		t.Errorf("summary %+v", sum)
	}
}

func runUpdate(t *testing.T, m *Manager, r *jobs.Runner, w *world, services []string, trigger string) jobs.Job {
	t.Helper()
	st, _ := w.Get("app")
	spec, err := m.UpdateSpec(st, services, trigger, jobs.Spec{StackID: "app", Kind: "update", Title: "Update", Trigger: trigger})
	if err != nil {
		t.Fatal(err)
	}
	j, _ := r.Submit(spec)
	done, _ := r.Wait(context.Background(), j.ID)
	return done
}

func TestUpdateSucceeds(t *testing.T) {
	m, w, n, r := setup(t)
	ctx := context.Background()
	w.remote["nginx:1"] = dg("n2")
	m.Check(ctx, "", nil)
	j := runUpdate(t, m, r, w, []string{"web"}, "user")
	_, out, _ := r.Get(ctx, j.ID)
	if j.State != jobs.Succeeded {
		t.Fatalf("%s %s\n%s", j.State, j.Error, out)
	}
	if !strings.Contains(strings.Join(w.composed, "|"), "pull web|up -d web") {
		t.Errorf("compose calls %v", w.composed)
	}
	if len(w.tags) != 1 || !strings.HasPrefix(w.tags[0], "sha256:img-n1→dm-rollback/app-web:") {
		t.Errorf("rollback tag %v", w.tags)
	}
	runs, _ := m.Runs(ctx, "app", 5)
	if len(runs) != 1 || runs[0].Result != "updated" || runs[0].JobID != j.ID || len(runs[0].Changes) != 1 || runs[0].Changes[0].To != "sha256:img-n2" {
		t.Errorf("runs %+v", runs)
	}
	if !strings.Contains(n.types(), "update.applied") {
		t.Errorf("notifications %s", n.types())
	}
	pts, _ := m.RollbackPoints(ctx, "app")
	if len(pts) != 1 || pts[0].Current {
		t.Errorf("points %+v", pts)
	}

	// nothing new: the run says unchanged and the rollback point is dropped again
	j = runUpdate(t, m, r, w, []string{"web"}, "user")
	runs, _ = m.Runs(ctx, "app", 5)
	if j.State != jobs.Succeeded || runs[0].Result != "unchanged" {
		t.Errorf("second run %s %+v", j.State, runs[0])
	}
	if pts, _ := m.RollbackPoints(ctx, "app"); len(pts) != 1 {
		t.Errorf("unchanged update left a rollback point: %d", len(pts))
	}
}

func TestUpdateRollsBack(t *testing.T) {
	m, w, n, r := setup(t)
	ctx := context.Background()
	w.remote["nginx:1"] = dg("n2")
	w.nextBad = true
	m.Check(ctx, "", nil)
	j := runUpdate(t, m, r, w, []string{"web"}, "auto")
	_, out, _ := r.Get(ctx, j.ID)
	if j.State != jobs.Failed || !strings.Contains(j.Error, "rolled back") {
		t.Fatalf("%s %s\n%s", j.State, j.Error, out)
	}
	if !strings.Contains(strings.Join(w.composed, "|"), "up -d --no-deps --no-build --pull never web") {
		t.Errorf("no rollback up: %v", w.composed)
	}
	w.mu.Lock()
	running := w.ctr["web"].ImageID
	w.mu.Unlock()
	if running != "sha256:img-n1" {
		t.Errorf("after rollback the container runs %s", running)
	}
	runs, _ := m.Runs(ctx, "app", 5)
	if runs[0].Result != "rolled_back" {
		t.Errorf("run %+v", runs[0])
	}
	if !strings.Contains(n.types(), "update.rolled_back") {
		t.Errorf("notifications %s", n.types())
	}
	// the failed digest is not tried automatically again
	m.SaveStackPolicy(ctx, "app", StackPolicy{Stack: Policy{Mode: "auto", AutoRollback: true}})
	if s := states(t, m)["web"]; s.Auto.Eligible || !strings.Contains(s.Auto.Blocked, "rolled back") {
		t.Errorf("bad digest still eligible: %+v", s.Auto)
	}
}

func TestHooksAndManualRollback(t *testing.T) {
	m, w, _, r := setup(t)
	ctx := context.Background()
	w.remote["nginx:1"] = dg("n2")
	m.SaveStackPolicy(ctx, "app", StackPolicy{Stack: Policy{Mode: "notify", AutoRollback: true, Hooks: []Hook{{Type: "pg_dump", Service: "db"}}}})
	j := runUpdate(t, m, r, w, []string{"web"}, "user")
	if j.State != jobs.Succeeded {
		t.Fatalf("%+v", j)
	}
	dumps, _ := filepath.Glob(filepath.Join(m.DataDir, "backups", "app", "db-*.sql.gz"))
	if len(dumps) != 1 {
		t.Errorf("dumps %v", dumps)
	}
	pts, _ := m.RollbackPoints(ctx, "app")
	st, _ := w.Get("app")
	spec, err := m.RollbackSpec(ctx, st, pts[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	rj, _ := r.Submit(spec)
	if d, _ := r.Wait(ctx, rj.ID); d.State != jobs.Succeeded {
		_, out, _ := r.Get(ctx, rj.ID)
		t.Fatalf("rollback %+v\n%s", d, out)
	}
	if w.ctr["web"].ImageID != "sha256:img-n1" {
		t.Errorf("rollback did not restore: %s", w.ctr["web"].ImageID)
	}

	// a failing command hook stops the update before anything changes
	m.SaveStackPolicy(ctx, "app", StackPolicy{Stack: Policy{Hooks: []Hook{{Type: "command", Service: "web", Command: "fail now"}}}})
	before := len(w.composed)
	if j := runUpdate(t, m, r, w, []string{"web"}, "user"); j.State != jobs.Failed || !strings.Contains(j.Error, "hook") {
		t.Errorf("%+v", j)
	}
	if len(w.composed) != before {
		t.Errorf("compose ran after a failed hook: %v", w.composed[before:])
	}
}

func TestAutoInfoAndPolicy(t *testing.T) {
	m, w, _, _ := setup(t)
	ctx := context.Background()
	w.remote["nginx:1"] = dg("n2")
	w.remote["postgres:16"] = dg("p2")
	m.Check(ctx, "", nil)
	if s := states(t, m); s["web"].Mode != "notify" || s["web"].Auto.Eligible {
		t.Errorf("default mode: %+v", s["web"])
	}
	m.SaveStackPolicy(ctx, "app", StackPolicy{Stack: Policy{Mode: "auto", AutoRollback: true}, Services: map[string]string{"app": "manual"}})
	s := states(t, m)
	if !s["web"].Auto.Eligible || s["web"].Auto.At.IsZero() {
		t.Errorf("web should be eligible: %+v", s["web"].Auto)
	}
	if s["db"].Auto.Eligible || !strings.Contains(s["db"].Auto.Blocked, "database") {
		t.Errorf("db: %+v", s["db"].Auto)
	}
	if s["app"].Mode != "manual" {
		t.Errorf("service override: %s", s["app"].Mode)
	}
	set := m.Settings(ctx)
	set.CooldownHours = 24
	m.SaveSettings(ctx, set)
	if a := states(t, m)["web"].Auto; a.Eligible || !strings.Contains(a.Blocked, "cooldown") || a.At.IsZero() {
		t.Errorf("cooldown: %+v", a)
	}
	set.Paused = true
	m.SaveSettings(ctx, set)
	if a := states(t, m)["web"].Auto; !strings.Contains(a.Blocked, "paused") {
		t.Errorf("paused: %+v", a)
	}
	sp, _ := m.StackPolicy(ctx, "app")
	if sp.Stack.Mode != "auto" || sp.Services["app"] != "manual" || sp.Default != "notify" {
		t.Errorf("policy %+v", sp)
	}
	if err := m.SaveStackPolicy(ctx, "app", StackPolicy{Stack: Policy{Mode: "sometimes"}}); err == nil {
		t.Error("bad mode accepted")
	}
	bad := DefaultSettings()
	bad.WindowCron = "not cron"
	if err := m.SaveSettings(ctx, bad); err == nil {
		t.Error("bad cron accepted")
	}
}

func TestWindow(t *testing.T) {
	sched, _ := cronParser.Parse("0 4 * * *")
	day := time.Date(2026, 10, 11, 0, 0, 0, 0, time.Local)
	for h, want := range map[float64]bool{3.9: false, 4: true, 4.5: true, 4.99: true, 5: false, 12: false} {
		at := day.Add(time.Duration(h * float64(time.Hour)))
		if got := inWindow(sched, 60, at); got != want {
			t.Errorf("%v: %v", at.Format("15:04"), got)
		}
	}
}

func TestAutoUpdateRunsOncePerWindow(t *testing.T) {
	m, w, _, _ := setup(t)
	ctx := context.Background()
	w.remote["nginx:1"] = dg("n2")
	m.Check(ctx, "", nil)
	m.SaveStackPolicy(ctx, "app", StackPolicy{Stack: Policy{Mode: "auto", AutoRollback: true}})
	set := m.Settings(ctx)
	start := time.Now().Add(-time.Minute)
	m.autoUpdate(ctx, set, start, time.Now().Add(time.Hour))
	if w.ctr["web"].ImageID != "sha256:img-n2" {
		t.Fatalf("auto update did not apply: %s (%v)", w.ctr["web"].ImageID, w.composed)
	}
	runs, _ := m.Runs(ctx, "app", 5)
	if len(runs) != 1 || runs[0].Trigger != "auto" {
		t.Errorf("runs %+v", runs)
	}
	n := len(w.composed)
	w.remote["nginx:1"] = dg("n3")
	m.Check(ctx, "", nil)
	m.autoUpdate(ctx, set, start, time.Now().Add(time.Hour))
	if len(w.composed) != n {
		t.Errorf("second attempt in the same window: %v", w.composed[n:])
	}
}

func discard() *slog.Logger { return slog.New(slog.DiscardHandler) }

var labels = map[string]string{}

// dg makes a valid digest for a short label ("n2" → sha256:<64 hex>) and remembers the label.
func dg(label string) string {
	sum := sha256.Sum256([]byte(label))
	d := "sha256:" + hex.EncodeToString(sum[:])
	labels[d] = label
	return d
}

func TestBaseImageBaseline(t *testing.T) {
	m, w, _, r := setup(t)
	ctx := context.Background()
	delete(w.images, "alpine:3.20") // BuildKit keeps no local copy of the base image
	m.Check(ctx, "", nil)
	if s := states(t, m)["app"]; s.State != LocalBuild || s.Base[0].State != UpToDate {
		t.Fatalf("first sight = baseline: %+v", s)
	}
	w.mu.Lock()
	w.remote["alpine:3.20"] = dg("a3")
	w.mu.Unlock()
	m.Check(ctx, "", nil)
	if s := states(t, m)["app"]; s.State != BaseUpdated {
		t.Fatalf("base moved: %+v", s)
	}
	runUpdate(t, m, r, w, []string{"app"}, "user") // builds with --pull
	if !strings.Contains(strings.Join(w.composed, "|"), "build --pull app") {
		t.Errorf("compose %v", w.composed)
	}
	m.Check(ctx, "", nil)
	if s := states(t, m)["app"]; s.State != LocalBuild {
		t.Errorf("after the rebuild the baseline moves along: %+v", s.Base)
	}
}
