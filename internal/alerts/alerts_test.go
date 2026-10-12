package alerts

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"docker-manager/internal/docker"
	"docker-manager/internal/jobs"
	"docker-manager/internal/notify"
	"docker-manager/internal/secret"
	"docker-manager/internal/store"
)

type sink struct {
	mu     sync.Mutex
	bodies []string
}

func (s *sink) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	s.mu.Lock()
	s.bodies = append(s.bodies, string(b))
	s.mu.Unlock()
}

func (s *sink) all() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.bodies...)
}

func setup(t *testing.T) (*Manager, *sink, string) {
	t.Helper()
	st, err := store.OpenFile(context.Background(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	box, _ := secret.NewBox([]byte(strings.Repeat("k", 32)), "notifications")
	sk := &sink{}
	srv := httptest.NewServer(sk)
	t.Cleanup(srv.Close)
	return New(st.DB, box, nil, "https://docker.example.com"), sk, srv.URL
}

func TestChannelsSecretsAndRouting(t *testing.T) {
	m, sk, url := setup(t)
	ctx := context.Background()
	cfg, _ := json.Marshal(notify.WebhookConfig{URL: url + "/hook?token=s3cret", Headers: map[string]string{"X-Key": "abc123"}})
	ch, err := m.SaveChannel(ctx, notify.Channel{Type: "webhook", Name: "test hook", Enabled: true, Config: cfg})
	if err != nil {
		t.Fatal(err)
	}
	var enc string
	m.db.QueryRow(`SELECT config_enc FROM notification_channels`).Scan(&enc)
	if strings.Contains(enc, "s3cret") || strings.Contains(enc, "abc123") {
		t.Error("config stored in plain text")
	}
	views, _ := m.Views(ctx)
	if len(views) != 1 || strings.Contains(string(views[0].Config), "s3cret") || len(views[0].SecretsSet) == 0 {
		t.Fatalf("view leaks or misses secrets: %s %v", views[0].Config, views[0].SecretsSet)
	}
	// an update with blank secrets keeps them
	ch.Name = "renamed"
	ch.Config = views[0].Config
	if _, err := m.SaveChannel(ctx, ch); err != nil {
		t.Fatal(err)
	}
	full, _ := m.Channel(ctx, ch.ID)
	if !strings.Contains(string(full.Config), "s3cret") || full.Name != "renamed" {
		t.Errorf("secrets lost on update: %s", full.Config)
	}
	if _, err := m.SaveChannel(ctx, notify.Channel{ID: ch.ID, Type: "ntfy", Name: "x", Config: cfg}); err == nil {
		t.Error("type change accepted")
	}

	if err := m.Test(ctx, ch.ID); err != nil {
		t.Fatalf("test send: %v", err)
	}
	// no rule yet: events go nowhere
	m.Send(notify.Event{Type: "update.applied", Title: "x", Stack: "web"})
	m.Wait()
	if n := len(sk.all()); n != 1 {
		t.Fatalf("%d deliveries, want only the test", n)
	}
	if _, err := m.SaveRule(ctx, notify.Rule{ChannelID: ch.ID, Events: []string{"update.*"}, MinSeverity: notify.Info}); err != nil {
		t.Fatal(err)
	}
	m.Send(notify.Event{Type: "update.applied", Title: "Updated web", Stack: "web", URL: "/stacks/web"})
	m.Send(notify.Event{Type: "container.died", Title: "x died"})
	m.Wait()
	got := sk.all()
	if len(got) != 2 || !strings.Contains(got[1], "Updated web") || !strings.Contains(got[1], "https://docker.example.com/stacks/web") {
		t.Errorf("deliveries %q", got)
	}
	log, _ := m.Log(ctx, 10)
	if len(log) != 2 || !log[0].OK || log[0].Event != "update.applied" {
		t.Errorf("log %+v", log)
	}
	views, _ = m.Views(ctx)
	if views[0].LastOK == nil {
		t.Error("last delivery not shown")
	}
	if err := m.DeleteChannel(ctx, ch.ID); err != nil {
		t.Fatal(err)
	}
	if rules, _ := m.Rules(ctx); len(rules) != 0 {
		t.Errorf("rules of a deleted channel remain: %v", rules)
	}
}

func TestDockerEvents(t *testing.T) {
	m, sk, url := setup(t)
	ctx := context.Background()
	cfg, _ := json.Marshal(notify.WebhookConfig{URL: url})
	ch, _ := m.SaveChannel(ctx, notify.Channel{Type: "webhook", Name: "all", Enabled: true, Config: cfg})
	m.SaveRule(ctx, notify.Rule{ChannelID: ch.ID, Events: []string{"*"}, DedupeMinutes: -1})

	die := func(code string) docker.Event {
		return docker.Event{Type: "container", Action: "die", ID: "c1", Attrs: map[string]string{"name": "web-1", "exitCode": code}, Time: time.Now()}
	}
	m.DockerEvent(die("0"), "web")   // clean stop: nothing
	m.DockerEvent(die("137"), "web") // crash
	m.Expect("web", time.Minute)
	m.DockerEvent(die("137"), "web") // during a user action: nothing
	m.DockerEvent(docker.Event{Type: "container", Action: "health_status: unhealthy", ID: "c2", Attrs: map[string]string{"name": "db-1"}}, "db")
	for range 4 {
		m.DockerEvent(docker.Event{Type: "container", Action: "start", ID: "c3", Attrs: map[string]string{"name": "loop-1"}}, "")
	}
	m.JobFinished(jobs.Job{ID: 7, Kind: "pull", Title: "Pull web", State: jobs.Failed, Error: "network", StackID: "x"})
	m.JobFinished(jobs.Job{ID: 8, Kind: "update", Title: "Update web", State: jobs.Failed})
	m.Wait()
	types := []string{}
	for _, b := range sk.all() {
		var e notify.Event
		json.Unmarshal([]byte(b), &e)
		types = append(types, e.Type)
	}
	want := "container.died,container.unhealthy,container.restart_loop,job.failed"
	got := strings.Join(types, ",")
	for _, w := range strings.Split(want, ",") {
		if !strings.Contains(got, w) {
			t.Errorf("missing %s in %s", w, got)
		}
	}
	if strings.Count(got, "container.died") != 1 || len(types) != 4 {
		t.Errorf("got %s", got)
	}
}
