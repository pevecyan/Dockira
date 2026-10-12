package snapshots

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"docker-manager/internal/docker"
	"docker-manager/internal/jobs"
	"docker-manager/internal/stacks"
	"docker-manager/internal/store"
)

type fakeInspector struct{ mounts []docker.Mount }

func (f fakeInspector) Inspect(context.Context, string) (docker.Details, error) {
	return docker.Details{Mounts: f.mounts}, nil
}

type fakeCompose struct{ calls *[]string }

func (f fakeCompose) Cmd(ctx context.Context, dir string, files []string, args ...string) *exec.Cmd {
	*f.calls = append(*f.calls, strings.Join(args, " "))
	return exec.CommandContext(ctx, "true")
}

func dockerRun(t *testing.T, args ...string) string {
	t.Helper()
	out, err := exec.Command("docker", args...).CombinedOutput()
	if err != nil {
		t.Fatalf("docker %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// run takes or restores inside a job, as the API does.
func run(t *testing.T, fn func(ctx context.Context, out *jobs.Output) error) string {
	t.Helper()
	st, _ := store.OpenFile(context.Background(), ":memory:")
	defer st.Close()
	r := jobs.New(st.DB, nil, nil, 1)
	j, _ := r.Submit(jobs.Spec{Kind: "x", Run: fn})
	done, _ := r.Wait(context.Background(), j.ID)
	_, out, _ := r.Get(context.Background(), j.ID)
	if done.State != jobs.Succeeded {
		t.Fatalf("%s: %s\n%s", done.State, done.Error, out)
	}
	return out
}

func TestSnapshotAndRestoreWithDocker(t *testing.T) {
	if exec.Command("docker", "image", "inspect", "busybox:1.36").Run() != nil {
		t.Skip("needs docker and the busybox:1.36 image")
	}
	vol := "dm-snapshot-test-" + time.Now().Format("150405")
	dockerRun(t, "volume", "create", vol)
	t.Cleanup(func() { exec.Command("docker", "volume", "rm", "-f", vol).Run() })
	dockerRun(t, "run", "--rm", "-v", vol+":/v", "busybox:1.36", "sh", "-c", "echo original > /v/data.txt && mkdir /v/sub && echo deep > /v/sub/x")

	stackDir := t.TempDir()
	os.WriteFile(filepath.Join(stackDir, "compose.yaml"), []byte("services: {}"), 0o644)
	os.MkdirAll(filepath.Join(stackDir, "data", "nested"), 0o755)
	os.WriteFile(filepath.Join(stackDir, "data", "nested", "file.txt"), []byte("bind original"), 0o640)
	os.Symlink("nested/file.txt", filepath.Join(stackDir, "data", "link"))

	var calls []string
	m := &Manager{DataDir: t.TempDir(), Docker: "docker", Image: "busybox:1.36", Compose: fakeCompose{&calls},
		Engine: fakeInspector{mounts: []docker.Mount{
			{Type: "volume", Name: vol, Destination: "/var/lib/data"},
			{Type: "bind", Source: filepath.Join(stackDir, "data"), Destination: "/data"},
			{Type: "bind", Source: stackDir, Destination: "/app"},             // the stack folder itself: never
			{Type: "bind", Source: "/etc/hostname", Destination: "/etc/host"}, // outside: never
		}}}
	st := stacks.Stack{ID: "app", Dir: stackDir, Files: []string{filepath.Join(stackDir, "compose.yaml")},
		Services: []stacks.Service{{Name: "web", InFile: true, Containers: []docker.Container{{ID: "c1", State: "running"}}}}}
	ctx := context.Background()

	var snap Snapshot
	out := run(t, func(ctx context.Context, out *jobs.Output) (err error) {
		snap, err = m.Take(ctx, st, Options{Stop: true, Binds: true}, out)
		return err
	})
	if len(snap.Items) != 2 || snap.Items[0].Kind != "bind" || snap.Items[1].Name != vol || !snap.Stopped {
		t.Fatalf("snapshot %+v\n%s", snap, out)
	}
	if strings.Join(calls, "|") != "stop web|start web" {
		t.Errorf("compose calls %v", calls)
	}

	// change everything, then restore
	dockerRun(t, "run", "--rm", "-v", vol+":/v", "busybox:1.36", "sh", "-c", "echo changed > /v/data.txt && rm -r /v/sub && echo new > /v/extra")
	os.WriteFile(filepath.Join(stackDir, "data", "nested", "file.txt"), []byte("bind changed"), 0o640)
	os.WriteFile(filepath.Join(stackDir, "data", "added.txt"), []byte("x"), 0o644)
	calls = nil
	run(t, func(ctx context.Context, out *jobs.Output) error { return m.Restore(ctx, st, snap.ID, out) })

	got := dockerRun(t, "run", "--rm", "-v", vol+":/v", "busybox:1.36", "sh", "-c", "cat /v/data.txt /v/sub/x; ls /v")
	if got != "original\ndeep\ndata.txt\nsub" {
		t.Errorf("volume after restore: %q", got)
	}
	if b, _ := os.ReadFile(filepath.Join(stackDir, "data", "nested", "file.txt")); string(b) != "bind original" {
		t.Errorf("bind file: %q", b)
	}
	if _, err := os.Stat(filepath.Join(stackDir, "data", "added.txt")); !os.IsNotExist(err) {
		t.Error("file added after the snapshot survived the restore")
	}
	if l, _ := os.Readlink(filepath.Join(stackDir, "data", "link")); l != "nested/file.txt" {
		t.Errorf("symlink %q", l)
	}
	if b, _ := os.ReadFile(filepath.Join(stackDir, "compose.yaml")); string(b) != "services: {}" {
		t.Error("the stack folder itself was touched")
	}

	list, _ := m.List("app")
	if len(list) != 2 || list[0].Trigger != "before restore" {
		t.Errorf("snapshots %+v", list)
	}
	if f, err := m.File("app", snap.ID, snap.Items[0].File); err != nil {
		t.Errorf("download: %v", err)
	} else {
		f.Close()
	}
	if _, err := m.File("app", snap.ID, "../../etc/passwd"); err == nil {
		t.Error("file outside the snapshot opened")
	}
	if _, err := m.Get("app", "../x"); err == nil {
		t.Error("bad id accepted")
	}
	_ = ctx
}

func TestPrune(t *testing.T) {
	m := &Manager{DataDir: t.TempDir()}
	for _, id := range []string{"20260101-000000", "20260102-000000", "20260103-000000", "20260104-000000"} {
		d := filepath.Join(m.stackDir("s"), id)
		os.MkdirAll(d, 0o700)
		os.WriteFile(filepath.Join(d, "manifest.json"), []byte(`{"id":"`+id+`","stack_id":"s","trigger":"manual","items":[]}`), 0o600)
	}
	m.prune("s", 3, "20260104-000000")
	list, _ := m.List("s")
	if len(list) != 3 || list[2].ID != "20260102-000000" {
		t.Errorf("%+v", list)
	}
}
