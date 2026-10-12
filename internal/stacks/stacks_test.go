package stacks

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"docker-manager/internal/compose"
	"docker-manager/internal/docker"
)

type fakeCompose struct {
	calls    atomic.Int32
	projects map[string]*compose.Project // by dir base name
}

func (f *fakeCompose) Config(_ context.Context, dir string, _ []string) (*compose.Project, error) {
	f.calls.Add(1)
	if p, ok := f.projects[filepath.Base(dir)]; ok {
		return p, nil
	}
	return nil, errors.New("yaml: line 2: did not find expected node content")
}

type fakeDocker struct{ cs []docker.Container }

func (f *fakeDocker) Containers(context.Context) ([]docker.Container, error) { return f.cs, nil }

func write(t *testing.T, p string) {
	t.Helper()
	os.MkdirAll(filepath.Dir(p), 0o755)
	os.WriteFile(p, []byte("services: {}\n"), 0o644)
}

func ctr(id, project, service, dir, state, health string) docker.Container {
	return docker.Container{ID: id, Name: project + "-" + service, Project: project, Service: service, WorkingDir: dir,
		State: state, Health: health, Image: service + ":latest", Labels: map[string]string{}}
}

func setup(t *testing.T) (string, *fakeCompose) {
	root := t.TempDir()
	write(t, filepath.Join(root, "gateway/docker-compose.yaml"))
	write(t, filepath.Join(root, "gateway/.env"))
	write(t, filepath.Join(root, "tools/scanner-service/compose.yaml"))
	write(t, filepath.Join(root, "tools/scanner-service/data/compose.yaml")) // data of a stack: not a stack
	write(t, filepath.Join(root, "broken/compose.yaml"))
	write(t, filepath.Join(root, "idle/compose.yml"))
	write(t, filepath.Join(root, ".hidden/compose.yaml"))
	write(t, filepath.Join(root, "skipme/compose.yaml"))
	write(t, filepath.Join(root, "a/b/c/d/compose.yaml")) // deeper than 3 levels
	os.WriteFile(filepath.Join(root, ".dmignore"), []byte("# comment\nskipme\n"), 0o644)
	fc := &fakeCompose{projects: map[string]*compose.Project{
		"gateway": {Name: "gateway", Services: map[string]compose.Service{
			"gateway": {Image: "registry.example.com/acme/gateway:main-stable"},
			"db":      {Image: "postgres:16", Healthcheck: &compose.Healthcheck{Test: []string{"CMD", "pg_isready"}}},
		}},
		"scanner-service": {Name: "scanner-service", Services: map[string]compose.Service{"scanner": {Build: &compose.Build{Context: "."}}}},
		"idle":           {Name: "idle", Services: map[string]compose.Service{"hello": {Image: "busybox"}}},
	}}
	return root, fc
}

func TestScan(t *testing.T) {
	root, fc := setup(t)
	dk := &fakeDocker{cs: []docker.Container{
		ctr("aaa1", "gateway", "gateway", filepath.Join(root, "gateway"), "running", ""),
		ctr("aaa2", "gateway", "db", filepath.Join(root, "gateway"), "running", "healthy"),
		ctr("bbb1", "scanner-service", "scanner", filepath.Join(root, "tools/scanner-service"), "running", ""),
		ctr("ccc1", "gone", "web", filepath.Join(root, "gone"), "exited", ""),
		ctr("ddd1", "elsewhere", "x", "/opt/elsewhere", "running", ""),
		{ID: "eee1", Name: "portainer", State: "running", Labels: map[string]string{}},
		{ID: "selfid0123", Name: "dm", Project: "gateway", Service: "gateway", WorkingDir: filepath.Join(root, "gateway"), State: "running",
			Labels: map[string]string{docker.LabelOneoff: "True"}, Oneoff: true},
	}}
	m := &Manager{Root: root, Depth: 3, Compose: fc, Docker: dk}
	if err := m.Scan(context.Background()); err != nil {
		t.Fatal(err)
	}
	ids := []string{}
	for _, s := range m.Stacks() {
		ids = append(ids, s.ID+"="+s.Status)
	}
	want := []string{"broken=invalid", "gateway=running", "gone=stopped", "idle=down", "tools/scanner-service=running", "~elsewhere=running"}
	if len(ids) != len(want) {
		t.Fatalf("stacks %v, want %v", ids, want)
	}
	for i := range want {
		if ids[i] != want[i] {
			t.Errorf("stacks %v, want %v", ids, want)
			break
		}
	}

	l, _ := m.Get("gateway")
	if !l.EnvFile || l.Total != 2 || l.Running != 2 || len(l.Services) != 2 || l.Services[0].Name != "db" ||
		!l.Services[0].Stateful || !l.Services[0].Healthcheck || l.Services[1].Stateful {
		t.Errorf("gateway: %+v", l)
	}
	q, _ := m.Get("tools/scanner-service")
	if !q.Services[0].Build || q.Services[0].Image != "scanner-service-scanner" {
		t.Errorf("built service: %+v", q.Services[0])
	}
	b, _ := m.Get("broken")
	if b.Error == "" {
		t.Error("broken stack has no error")
	}
	g, _ := m.Get("gone")
	if !g.Orphaned || g.Error == "" {
		t.Errorf("gone: %+v", g)
	}
	if sa := m.Standalone(); len(sa) != 1 || sa[0].Name != "portainer" {
		t.Errorf("standalone %v", sa)
	}

	// unchanged files: no new compose config runs
	n := fc.calls.Load()
	m.Scan(context.Background())
	if fc.calls.Load() != n {
		t.Errorf("config re-run for unchanged files: %d → %d", n, fc.calls.Load())
	}
	m.Invalidate(filepath.Join(root, "gateway"))
	m.Scan(context.Background())
	if fc.calls.Load() != n+1 {
		t.Errorf("invalidate did not reload exactly one stack: %d → %d", n, fc.calls.Load())
	}
}

func TestSelfAndStatuses(t *testing.T) {
	root, fc := setup(t)
	dir := filepath.Join(root, "gateway")
	dk := &fakeDocker{cs: []docker.Container{
		ctr("selfabc", "gateway", "gateway", dir, "running", ""),
		ctr("x2", "gateway", "db", dir, "exited", ""),
	}}
	m := &Manager{Root: root, Compose: fc, Docker: dk, SelfID: "selfabc"}
	m.Scan(context.Background())
	l, _ := m.Get("gateway")
	if !l.Self || l.Status != "partial" {
		t.Errorf("%+v", l)
	}
	for _, tc := range []struct {
		states, healths []string
		want            string
	}{
		{[]string{"running", "running"}, []string{"", "unhealthy"}, "unhealthy"},
		{[]string{"running", "restarting"}, []string{"", ""}, "restarting"},
		{[]string{"running"}, []string{"starting"}, "starting"},
		{[]string{"exited", "created"}, []string{"", ""}, "stopped"},
		{[]string{"exited"}, []string{"unhealthy"}, "stopped"}, // last health of a stopped container
		{[]string{"running", "exited"}, []string{"healthy", "unhealthy"}, "partial"},
	} {
		var cs []docker.Container
		for i := range tc.states {
			cs = append(cs, docker.Container{State: tc.states[i], Health: tc.healths[i]})
		}
		if got := Aggregate(cs); got != tc.want {
			t.Errorf("%v %v: %s, want %s", tc.states, tc.healths, got, tc.want)
		}
	}
}

func TestHelpers(t *testing.T) {
	for img, want := range map[string]bool{"postgres:16": true, "docker.io/library/redis:7-alpine": true, "ghcr.io/x/redis-commander:1": false,
		"bitnami/postgresql": false, "mariadb@sha256:abc": true, "nginx": false} {
		if IsStateful(img) != want {
			t.Errorf("IsStateful(%s) != %v", img, want)
		}
	}
	if n := defaultProjectName("/x/My App.v2"); n != "myappv2" {
		t.Error(n)
	}
}
