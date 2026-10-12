package api

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"

	"docker-manager/internal/auth"
	"docker-manager/internal/compose"
	"docker-manager/internal/config"
	"docker-manager/internal/docker"
	"docker-manager/internal/events"
	"docker-manager/internal/files"
	"docker-manager/internal/hoststats"
	"docker-manager/internal/jobs"
	"docker-manager/internal/stacks"
	"docker-manager/internal/store"
)

type fakeEngine struct {
	actions    []string
	containers []docker.Container
	events     chan docker.Event
}

func (f *fakeEngine) Containers(context.Context) ([]docker.Container, error) {
	return f.containers, nil
}
func (f *fakeEngine) Inspect(_ context.Context, id string) (docker.Details, error) {
	for _, c := range f.containers {
		if c.ID == id {
			return docker.Details{Container: c, RestartCount: 2}, nil
		}
	}
	return docker.Details{}, errors.New("no such container")
}
func (f *fakeEngine) System(context.Context) (docker.System, error) {
	return docker.System{Version: "29.9.0", CPUs: 2}, nil
}
func (f *fakeEngine) DiskUsage(context.Context) (docker.DiskUsage, error) {
	return docker.DiskUsage{}, nil
}
func (f *fakeEngine) ContainerAction(_ context.Context, id, action string) error {
	f.actions = append(f.actions, action+" "+id)
	return nil
}
func (f *fakeEngine) IsTTY(context.Context, string) (bool, error) { return true, nil }
func (f *fakeEngine) Logs(_ context.Context, id string, o docker.LogOptions) (io.ReadCloser, error) {
	return io.NopCloser(strings.NewReader("2026-10-11T09:00:00.000000000Z hello from " + id + "\n2026-10-11T09:00:01.000000000Z second line\n")), nil
}
func (f *fakeEngine) Events(ctx context.Context) (<-chan docker.Event, <-chan error) {
	errc := make(chan error, 1)
	out := make(chan docker.Event)
	go func() {
		defer close(out)
		for {
			select {
			case <-ctx.Done():
				errc <- ctx.Err()
				return
			case e := <-f.events:
				out <- e
			}
		}
	}()
	return out, errc
}

type fakeCompose struct{}

func (fakeCompose) Config(_ context.Context, dir string, _ []string) (*compose.Project, error) {
	return &compose.Project{Name: filepath.Base(dir), Services: map[string]compose.Service{"web": {Image: "nginx:1"}}}, nil
}

type testEnv struct {
	srv    *httptest.Server
	s      *Server
	engine *fakeEngine
	root   string
}

func newEnv(t *testing.T, cfg config.Config) *testEnv {
	t.Helper()
	st, err := store.OpenFile(context.Background(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, "group", "app"), 0o755)
	os.WriteFile(filepath.Join(root, "group", "app", "compose.yaml"), []byte("services: {}"), 0o644)
	cfg.AppsRoot = root
	eng := &fakeEngine{events: make(chan docker.Event), containers: []docker.Container{{ID: "c1", Name: "app-web-1", Project: "app", Service: "web",
		WorkingDir: filepath.Join(root, "group", "app"), State: "running", Image: "nginx:1", Labels: map[string]string{}}}}
	sm := &stacks.Manager{Root: root, Depth: 3, Compose: fakeCompose{}, Docker: eng}
	sm.Scan(context.Background())
	ui := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ui")) })
	// a fake docker binary that only prints the compose arguments it got
	bin := filepath.Join(t.TempDir(), "docker")
	os.WriteFile(bin, []byte("#!/bin/sh\necho \"compose args: $*\"\n[ \"$FAIL\" = 1 ] && exit 4\nexit 0\n"), 0o755)
	hub := events.NewHub()
	s := New(Deps{Config: cfg, Store: st, Version: "test", UI: ui, Docker: eng, Stacks: sm, Compose: &compose.CLI{Bin: bin}, Hub: hub,
		Jobs: jobs.New(st.DB, hub, nil, 2), Files: &files.Manager{DB: st.DB, Validator: &compose.CLI{Bin: bin}}, Stats: hoststats.New(hoststats.Options{DiskPath: root})})
	srv := httptest.NewServer(LogRequests(slog.New(slog.DiscardHandler), s))
	t.Cleanup(srv.Close)
	return &testEnv{srv: srv, s: s, engine: eng, root: root}
}

func newTest(t *testing.T, cfg config.Config) *httptest.Server { return newEnv(t, cfg).srv }

func TestInfo(t *testing.T) {
	cfg := config.Default()
	cfg.ReadOnly = true
	srv := newTest(t, cfg)

	res, err := http.Get(srv.URL + "/api/info")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var info Info
	if err := json.NewDecoder(res.Body).Decode(&info); err != nil {
		t.Fatal(err)
	}
	if res.StatusCode != 200 || info.Version != "test" || !info.ReadOnly || info.SchemaVersion < 1 {
		t.Errorf("%d %+v", res.StatusCode, info)
	}
}

func TestRouting(t *testing.T) {
	srv := newTest(t, config.Default())
	for path, want := range map[string]int{
		"/api/nope":         404, // unknown API paths are JSON errors, not the SPA
		"/stacks/gateway":   200, // client-side route → UI
		"/api/openapi.json": 200,
		"/api/docs":         404, // docs only in dev
	} {
		res, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode != want {
			t.Errorf("%s: %d, want %d", path, res.StatusCode, want)
		}
		if path == "/api/nope" && !strings.Contains(res.Header.Get("Content-Type"), "json") {
			t.Errorf("/api/nope: content type %q", res.Header.Get("Content-Type"))
		}
	}
}

func TestEventsHello(t *testing.T) {
	srv := newTest(t, config.Default())
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", srv.URL+"/api/events", nil)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if ct := res.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content type %q", ct)
	}
	line, _ := bufio.NewReader(res.Body).ReadString('\n')
	if line != "event: hello\n" {
		t.Errorf("first line %q", line)
	}
}

func TestSpec(t *testing.T) {
	spec := Spec("1.2.3")
	if spec.Info.Version != "1.2.3" || spec.Paths["/api/info"] == nil || spec.Components.Schemas.Map()["Info"] == nil {
		t.Errorf("spec misses /api/info or the Info schema")
	}
}

func getJSON(t *testing.T, u string, v any) int {
	t.Helper()
	res, err := http.Get(u)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if v != nil {
		b, _ := io.ReadAll(res.Body)
		if err := json.Unmarshal(b, v); err != nil {
			t.Fatalf("%s: %v\n%s", u, err, b)
		}
	}
	return res.StatusCode
}

func TestStacksAPI(t *testing.T) {
	env := newEnv(t, config.Default())
	var list StackList
	if code := getJSON(t, env.srv.URL+"/api/stacks", &list); code != 200 || len(list.Stacks) != 1 || list.Stacks[0].ID != "group/app" || list.Stacks[0].Status != "running" {
		t.Fatalf("%d %+v", code, list)
	}
	// a stack ID with a slash travels URL-encoded in one path segment
	var det StackDetail
	if code := getJSON(t, env.srv.URL+"/api/stacks/"+url.PathEscape("group/app"), &det); code != 200 || len(det.Details) != 1 || det.Details[0].RestartCount != 2 {
		t.Fatalf("detail %d %+v", code, det)
	}
	if code := getJSON(t, env.srv.URL+"/api/stacks/"+url.PathEscape("nope"), nil); code != 404 {
		t.Errorf("missing stack: %d", code)
	}
	var h HostInfo
	if code := getJSON(t, env.srv.URL+"/api/host", &h); code != 200 || h.System.Version != "29.9.0" || h.Host.MemTotal == 0 {
		t.Errorf("host %d %+v", code, h.System)
	}
}

func TestSecurityHeaders(t *testing.T) {
	ScriptHashes = []string{"sha256-abc"}
	defer func() { ScriptHashes = nil }()
	cfg := config.Default()
	cfg.PublicURL = "https://docker.example.com"
	srv := newTest(t, cfg)
	res, err := http.Get(srv.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	csp := res.Header.Get("Content-Security-Policy")
	if !strings.Contains(csp, "script-src 'self' 'sha256-abc'") || !strings.Contains(csp, "frame-ancestors 'none'") ||
		res.Header.Get("X-Frame-Options") != "DENY" || res.Header.Get("Strict-Transport-Security") == "" {
		t.Errorf("headers %v", res.Header)
	}
}

func TestMonitorPublishesDockerEvents(t *testing.T) {
	env := newEnv(t, config.Default())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sub := env.s.Hub.Subscribe(32)
	go env.s.Run(ctx)
	time.Sleep(50 * time.Millisecond)

	env.engine.containers[0].State = "exited"
	env.engine.events <- docker.Event{Type: "container", Action: "die", ID: "c1", Time: time.Now(),
		Attrs: map[string]string{"name": "app-web-1", "exitCode": "137", docker.LabelProject: "app", docker.LabelWorkingDir: filepath.Join(env.root, "group", "app")}}
	env.engine.events <- docker.Event{Type: "container", Action: "exec_start: sh", ID: "c1", Time: time.Now()}

	deadline := time.After(3 * time.Second)
	var gotFeed, gotStacks bool
	for !gotFeed || !gotStacks {
		select {
		case ev := <-sub.C:
			switch ev.Name {
			case "feed":
				it := ev.Data.(FeedItem)
				if it.Action != "die" || it.Level != "warn" || it.Stack != "group/app" {
					t.Errorf("feed item %+v", it)
				}
				gotFeed = true
			case "stacks":
				l := ev.Data.(StackList)
				if l.Stacks[0].Status == "stopped" {
					gotStacks = true
				}
			}
		case <-deadline:
			t.Fatalf("feed %v stacks %v", gotFeed, gotStacks)
		}
	}
	if n := len(env.s.feed.list()); n != 1 {
		t.Errorf("exec events should be skipped: %d feed items", n)
	}
}

func postJSON(t *testing.T, u, body string, v any) int {
	t.Helper()
	res, err := http.Post(u, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	if v != nil && len(b) > 0 {
		if err := json.Unmarshal(b, v); err != nil {
			t.Fatalf("%s: %v\n%s", u, err, b)
		}
	}
	return res.StatusCode
}

func TestStackActionsRunCompose(t *testing.T) {
	env := newEnv(t, config.Default())
	id := url.PathEscape("group/app")
	var j jobs.Job
	if code := postJSON(t, env.srv.URL+"/api/stacks/"+id+"/actions", `{"action":"update","services":["web"]}`, &j); code != 200 || j.ID == 0 {
		t.Fatalf("%d %+v", code, j)
	}
	done, _ := env.s.Jobs.Wait(context.Background(), j.ID)
	_, out, _ := env.s.Jobs.Get(context.Background(), j.ID)
	dir := filepath.Join(env.root, "group", "app")
	want := "▸ docker compose pull web\ncompose args: compose --project-directory " + dir + " -f " + dir + "/compose.yaml pull web\n" +
		"▸ docker compose up -d web\ncompose args: compose --project-directory " + dir + " -f " + dir + "/compose.yaml up -d web\n"
	if done.State != "succeeded" || out != want {
		t.Errorf("%s\n%q\nwant\n%q", done.State, out, want)
	}
	var detail JobDetail
	if code := getJSON(t, env.srv.URL+"/api/jobs/"+strconv.FormatInt(j.ID, 10), &detail); code != 200 || detail.Output != want {
		t.Errorf("job detail %d %+v", code, detail)
	}
	var list JobList
	if getJSON(t, env.srv.URL+"/api/jobs?stack=group/app", &list); len(list.Jobs) != 1 {
		t.Errorf("jobs %+v", list)
	}

	for body, wantCode := range map[string]int{
		`{"action":"explode"}`:                 422,
		`{"action":"up","services":["nope"]}`:  400,
		`{"action":"down","services":["web"]}`: 400,
	} {
		var p map[string]any
		code := postJSON(t, env.srv.URL+"/api/stacks/"+id+"/actions", body, &p)
		if code != wantCode {
			t.Errorf("%s: %d %v, want %d", body, code, p, wantCode)
		}
	}
}

func TestContainerActionAndLogs(t *testing.T) {
	env := newEnv(t, config.Default())
	if code := postJSON(t, env.srv.URL+"/api/containers/c1/actions", `{"action":"restart"}`, nil); code != 204 {
		t.Errorf("restart: %d", code)
	}
	if code := postJSON(t, env.srv.URL+"/api/containers/zzz/actions", `{"action":"restart"}`, nil); code != 404 {
		t.Errorf("unknown container: %d", code)
	}
	if len(env.engine.actions) != 1 || env.engine.actions[0] != "restart c1" {
		t.Errorf("%v", env.engine.actions)
	}

	res, err := http.Get(env.srv.URL + "/api/logs?stack=" + url.QueryEscape("group/app") + "&follow=0")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(res.Body)
	res.Body.Close()
	body := string(b)
	if !strings.Contains(body, `"m":"hello from c1"`) || !strings.Contains(body, `"s":"web"`) || !strings.Contains(body, "event: end") {
		t.Errorf("logs stream:\n%s", body)
	}

	res, _ = http.Get(env.srv.URL + "/api/logs/download?container=c1")
	b, _ = io.ReadAll(res.Body)
	res.Body.Close()
	if !strings.Contains(string(b), "web | 2026-10-11T09:00:01.000000000Z | second line") || !strings.Contains(res.Header.Get("Content-Disposition"), "app-web-1") {
		t.Errorf("download: %s %v", b, res.Header)
	}
}

func TestAuthAndConfirmFlow(t *testing.T) {
	env := newEnv(t, config.Default())
	h, _ := bcrypt.GenerateFromPassword([]byte("a long enough password"), bcrypt.MinCost)
	env.s.Auth = auth.New(auth.Options{PasswordHash: string(h), Key: []byte(strings.Repeat("k", 32))})
	jar, _ := cookiejar.New(nil)
	c := &http.Client{Jar: jar}
	post := func(path, body string) (int, string) {
		req, _ := http.NewRequest("POST", env.srv.URL+path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Origin", env.srv.URL)
		res, err := c.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(res.Body)
		res.Body.Close()
		return res.StatusCode, string(b)
	}
	id := url.PathEscape("group/app")
	if code, body := post("/api/stacks/"+id+"/actions", `{"action":"restart"}`); code != 401 || !strings.Contains(body, `"login":true`) {
		t.Fatalf("no login: %d %s", code, body)
	}
	if code, _ := post("/api/login", `{"password":"a long enough password"}`); code != 204 {
		t.Fatalf("login %d", code)
	}
	if code, body := post("/api/stacks/"+id+"/actions", `{"action":"down_volumes"}`); code != 403 || !strings.Contains(body, `"confirm":true`) {
		t.Fatalf("down -v without confirmation: %d %s", code, body)
	}
	if code, _ := post("/api/confirm", `{"password":"a long enough password"}`); code != 204 {
		t.Fatalf("confirm %d", code)
	}
	if code, body := post("/api/stacks/"+id+"/actions", `{"action":"down_volumes"}`); code != 200 {
		t.Fatalf("down -v after confirmation: %d %s", code, body)
	}
	var entries []store.AuditEntry
	entries, _ = env.s.Store.AuditLog(context.Background(), 0, 10)
	if len(entries) != 1 || entries[0].Action != "stack.down_volumes" || entries[0].IP != "127.0.0.1" {
		t.Errorf("audit %+v", entries)
	}
}

func do(t *testing.T, method, u, body string, v any) int {
	t.Helper()
	req, _ := http.NewRequest(method, u, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	if v != nil && len(b) > 0 {
		if err := json.Unmarshal(b, v); err != nil {
			t.Fatalf("%s %s: %v\n%s", method, u, err, b)
		}
	}
	return res.StatusCode
}

func TestFilesAPI(t *testing.T) {
	env := newEnv(t, config.Default())
	base := env.srv.URL + "/api/stacks/" + url.PathEscape("group/app")
	var list FileListOutput
	if code := getJSON(t, base+"/files", &list.Body); code != 200 || len(list.Body.Files) != 2 || list.Body.Files[1].Name != ".env" || list.Body.Files[1].Exists {
		t.Fatalf("%d %+v", code, list.Body)
	}
	var f FileContent
	if code := getJSON(t, base+"/files/compose.yaml", &f); code != 200 || f.Content != "services: {}" || f.SHA256 == "" {
		t.Fatalf("%d %+v", code, f)
	}
	os.WriteFile(filepath.Join(env.root, "group", "outside.txt"), []byte("not yours"), 0o644)
	var esc FileContent
	if getJSON(t, base+"/files/..%2Foutside.txt", &esc); strings.Contains(esc.Content, "not yours") {
		t.Errorf("read a file outside the stack folder")
	}
	var saved SaveResult
	body := `{"content":"services:\n  web:\n    image: nginx:1.27\n","base_sha256":"` + f.SHA256 + `"}`
	if code := do(t, "PUT", base+"/files/compose.yaml", body, &saved); code != 200 || saved.Revision.ID == 0 {
		t.Fatalf("save %d %+v", code, saved)
	}
	var conflict FileError
	if code := do(t, "PUT", base+"/files/compose.yaml", body, &conflict); code != 409 || conflict.Current != saved.SHA256 {
		t.Errorf("conflict %d %+v", code, conflict)
	}
	var revs RevisionsOutput
	getJSON(t, base+"/revisions?file=compose.yaml", &revs.Body)
	if len(revs.Body.Revisions) != 2 || revs.Body.Revisions[0].Reason != "edit" || revs.Body.Revisions[1].Reason != "baseline" {
		t.Fatalf("revisions %+v", revs.Body)
	}
	var restored SaveResult
	if code := do(t, "POST", base+"/revisions/"+strconv.FormatInt(revs.Body.Revisions[1].ID, 10)+"/restore", `{}`, &restored); code != 200 || restored.Revision.Reason != "restore" {
		t.Errorf("restore %d %+v", code, restored)
	}
	b, _ := os.ReadFile(filepath.Join(env.root, "group", "app", "compose.yaml"))
	if string(b) != "services: {}" {
		t.Errorf("after restore: %q", b)
	}
	var e EnvOutput
	if code := getJSON(t, base+"/env", &e.Body); code != 200 || e.Body.Exists {
		t.Errorf("env %d %+v", code, e.Body)
	}
	// a new .env (no auth configured in this test, so no confirmation needed)
	if code := do(t, "PUT", base+"/files/.env", `{"content":"A=1\n","base_sha256":""}`, nil); code != 200 {
		t.Errorf("new .env: %d", code)
	}
}

func TestBrowseAPI(t *testing.T) {
	env := newEnv(t, config.Default())
	base := env.srv.URL + "/api/stacks/" + url.PathEscape("group/app")
	dir := filepath.Join(env.root, "group", "app")
	os.MkdirAll(filepath.Join(dir, "conf"), 0o755)
	os.WriteFile(filepath.Join(dir, "conf", "app.json"), []byte(`{"a": 1}`), 0o644)

	var l TreeOutput
	if code := getJSON(t, base+"/tree", &l.Body); code != 200 || len(l.Body.Entries) != 2 || l.Body.Entries[0].Name != "conf" {
		t.Fatalf("tree %d %+v", code, l.Body)
	}
	var f AnyFile
	if code := getJSON(t, base+"/file?path=conf/app.json", &f); code != 200 || f.Content != `{"a": 1}` {
		t.Fatalf("read %d %+v", code, f)
	}
	var bad FileError
	if code := do(t, "PUT", base+"/file", `{"path":"conf/app.json","content":"{oops","base_sha256":"`+f.SHA256+`"}`, &bad); code != 422 || bad.Line != 1 {
		t.Errorf("broken json: %d %+v", code, bad)
	}
	if code := do(t, "PUT", base+"/file", `{"path":"conf/app.json","content":"{\"a\": 2}","base_sha256":"`+f.SHA256+`"}`, nil); code != 200 {
		t.Errorf("save %d", code)
	}
	if code := do(t, "POST", base+"/folder", `{"path":"conf/sub"}`, nil); code != 204 {
		t.Errorf("mkdir %d", code)
	}
	req, _ := http.NewRequest("PUT", base+"/upload?path=conf/sub/cert.pem", strings.NewReader("PEM"))
	res, _ := http.DefaultClient.Do(req)
	res.Body.Close()
	if res.StatusCode != 204 {
		t.Errorf("upload %d", res.StatusCode)
	}
	res, _ = http.Get(base + "/download?path=conf/sub/cert.pem")
	b, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if string(b) != "PEM" || !strings.Contains(res.Header.Get("Content-Disposition"), "cert.pem") {
		t.Errorf("download %q %v", b, res.Header)
	}
	if code := do(t, "DELETE", base+"/file?path=conf/sub/cert.pem", "", nil); code != 204 {
		t.Errorf("delete %d", code)
	}
	if code := do(t, "DELETE", base+"/file?path=compose.yaml", "", nil); code != 409 {
		t.Errorf("deleting the compose file: %d", code)
	}
	if code := getJSON(t, base+"/file?path=conf/missing.txt", nil); code != 404 {
		t.Errorf("missing file: %d", code)
	}
}

func TestCreateApp(t *testing.T) {
	env := newEnv(t, config.Default())
	env.s.Config.AppsRoot = env.root
	var v struct {
		OK    bool
		Error *files.ValidationError
	}
	if code := do(t, "POST", env.srv.URL+"/api/apps/validate", `{"path":"group/app","compose":"services: {}"}`, &v); code != 200 || v.OK || !strings.Contains(v.Error.Message, "compose file already") {
		t.Errorf("validate existing: %d %+v", code, v)
	}
	var folders FoldersOutput
	if getJSON(t, env.srv.URL+"/api/apps/folders", &folders.Body); len(folders.Body.Folders) != 1 || folders.Body.Folders[0] != "group" {
		t.Errorf("folders %+v", folders.Body)
	}
	var res NewAppResult
	if code := do(t, "POST", env.srv.URL+"/api/apps", `{"path":"group/new-one","compose":"services:\n  web:\n    image: nginx\n","start":true}`, &res); code != 200 || res.Stack != "group/new-one" || res.Job == nil {
		t.Fatalf("create %d %+v", code, res)
	}
	done, _ := env.s.Jobs.Wait(context.Background(), res.Job.ID)
	if done.State != "succeeded" || done.Kind != "up" {
		t.Errorf("start job %+v", done)
	}
	if _, ok := env.s.Stacks.Get("group/new-one"); !ok {
		t.Error("new app not listed")
	}
}
