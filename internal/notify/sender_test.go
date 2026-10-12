package notify

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

// recorded is one request captured by a test server.
type recorded struct {
	Method, Path, Query string
	Header              http.Header
	Body                string
}

type recorder struct {
	mu     sync.Mutex
	reqs   []recorded
	status int    // response status; 0 = 200
	reply  string // response body
}

func (r *recorder) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	b, _ := io.ReadAll(req.Body)
	r.mu.Lock()
	r.reqs = append(r.reqs, recorded{req.Method, req.URL.Path, req.URL.RawQuery, req.Header.Clone(), string(b)})
	status, reply := r.status, r.reply
	r.mu.Unlock()
	if status != 0 {
		w.WriteHeader(status)
	}
	io.WriteString(w, reply)
}

func (r *recorder) all() []recorded {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]recorded(nil), r.reqs...)
}

func newServer(t *testing.T) (*recorder, *httptest.Server) {
	rec := &recorder{}
	srv := httptest.NewServer(rec)
	t.Cleanup(srv.Close)
	return rec, srv
}

func cfg(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}

func send(t *testing.T, ch Channel, e Event) error {
	t.Helper()
	if ch.Name == "" {
		ch.Name = "test"
	}
	s, err := NewSender(ch, nil)
	if err != nil {
		t.Fatal(err)
	}
	return s.Send(context.Background(), e)
}

var sample = Event{
	Type: "update.failed", Severity: Error, Title: "Update failed: web", Message: "pull <denied> & stuff",
	Stack: "web", URL: "https://dm.example.com/stacks/web", Time: time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC),
}

func TestNtfy(t *testing.T) {
	rec, srv := newServer(t)
	err := send(t, Channel{Type: TypeNtfy, Config: cfg(NtfyConfig{Server: srv.URL + "/sub", Topic: "alerts", Token: "tk_abc", Tags: []string{"docker"}})}, sample)
	if err != nil {
		t.Fatal(err)
	}
	r := rec.all()[0]
	if r.Method != "POST" || r.Path != "/sub/alerts" || r.Body != sample.Message {
		t.Errorf("%+v", r)
	}
	want := map[string]string{"Title": "Update failed: web", "Priority": "5", "Tags": "rotating_light,docker",
		"Click": sample.URL, "Authorization": "Bearer tk_abc"}
	for k, v := range want {
		if got := r.Header.Get(k); got != v {
			t.Errorf("%s = %q, want %q", k, got, v)
		}
	}

	// Basic auth, info priority, non-ASCII title encoded, no Click without URL.
	err = send(t, Channel{Type: TypeNtfy, Config: cfg(NtfyConfig{Server: srv.URL, Topic: "a", Username: "u", Password: "p"})},
		Event{Type: "update.available", Severity: Info, Title: "Posodobitev čaka\nnow", Message: ""})
	if err != nil {
		t.Fatal(err)
	}
	r = rec.all()[1]
	if u, p, _ := (&http.Request{Header: r.Header}).BasicAuth(); u != "u" || p != "p" {
		t.Errorf("basic auth %q %q", u, p)
	}
	if r.Header.Get("Priority") != "3" || r.Header.Get("Tags") != "information_source" || r.Header.Get("Click") != "" {
		t.Errorf("headers %v", r.Header)
	}
	if title := r.Header.Get("Title"); !strings.HasPrefix(title, "=?utf-8?b?") {
		t.Errorf("title not RFC 2047 encoded: %q", title)
	}
	if r.Body != "Posodobitev čaka\nnow" {
		t.Errorf("empty message should fall back to title, got %q", r.Body)
	}
}

func TestGotify(t *testing.T) {
	rec, srv := newServer(t)
	err := send(t, Channel{Type: TypeGotify, Config: cfg(GotifyConfig{Server: srv.URL + "/", Token: "AppTok"})}, Event{
		Type: "job.failed", Severity: Warning, Title: "T", Message: "M", URL: "https://dm/x"})
	if err != nil {
		t.Fatal(err)
	}
	r := rec.all()[0]
	if r.Method != "POST" || r.Path != "/message" || r.Header.Get("X-Gotify-Key") != "AppTok" || r.Query != "" {
		t.Errorf("%+v", r)
	}
	var body map[string]any
	json.Unmarshal([]byte(r.Body), &body)
	want := map[string]any{"title": "T", "message": "M", "priority": 6.0,
		"extras": map[string]any{"client::notification": map[string]any{"click": map[string]any{"url": "https://dm/x"}}}}
	if !reflect.DeepEqual(body, want) {
		t.Errorf("body %v", body)
	}
}

func TestTelegram(t *testing.T) {
	rec, srv := newServer(t)
	rec.reply = `{"ok":true}`
	c := TelegramConfig{BotToken: "123:SECRET", ChatID: "-100", ThreadID: 7, SilentInfo: true, APIBase: srv.URL}
	if err := send(t, Channel{Type: TypeTelegram, Config: cfg(c)}, sample); err != nil {
		t.Fatal(err)
	}
	r := rec.all()[0]
	if r.Method != "POST" || r.Path != "/bot123:SECRET/sendMessage" || !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		t.Errorf("%+v", r)
	}
	var body map[string]any
	json.Unmarshal([]byte(r.Body), &body)
	wantText := "<b>Update failed: web</b>\npull &lt;denied&gt; &amp; stuff\n<a href=\"https://dm.example.com/stacks/web\">Open in Dockira</a>"
	if body["text"] != wantText || body["parse_mode"] != "HTML" || body["chat_id"] != "-100" || body["message_thread_id"] != 7.0 {
		t.Errorf("body %v", body)
	}
	if _, ok := body["disable_notification"]; ok {
		t.Error("error event sent silently")
	}

	info := sample
	info.Severity = Info
	send(t, Channel{Type: TypeTelegram, Config: cfg(c)}, info)
	json.Unmarshal([]byte(rec.all()[1].Body), &body)
	if body["disable_notification"] != true {
		t.Errorf("info not silent: %v", body)
	}
}

func TestTelegramErrorsHideToken(t *testing.T) {
	// The server echoes the path, as some proxies' error pages do.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		io.WriteString(w, `{"ok":false,"description":"Unauthorized `+r.URL.Path+`"}`)
	}))
	defer srv.Close()
	ch := Channel{Type: TypeTelegram, Config: cfg(TelegramConfig{BotToken: "999:TOPSECRET", ChatID: "1", APIBase: srv.URL})}
	err := send(t, ch, sample)
	if err == nil || strings.Contains(err.Error(), "TOPSECRET") || !strings.Contains(err.Error(), "HTTP 401") || !strings.Contains(err.Error(), "Unauthorized") {
		t.Errorf("http error: %v", err)
	}

	// Transport error: url.Error would print the full URL including the token.
	dead := httptest.NewServer(http.NotFoundHandler())
	dead.Close()
	ch.Config = cfg(TelegramConfig{BotToken: "999:TOPSECRET", ChatID: "1", APIBase: dead.URL})
	err = send(t, ch, sample)
	if err == nil || strings.Contains(err.Error(), "TOPSECRET") || !strings.Contains(err.Error(), "telegram:") {
		t.Errorf("transport error: %v", err)
	}
}

func TestNon2xxExcerpt(t *testing.T) {
	rec, srv := newServer(t)
	rec.status = http.StatusBadGateway
	rec.reply = strings.Repeat("x", 1000)
	err := send(t, Channel{Type: TypeWebhook, Config: cfg(WebhookConfig{URL: srv.URL + "/h"})}, sample)
	if err == nil || !strings.HasPrefix(err.Error(), "webhook: HTTP 502: xxx") || len(err.Error()) > 230 {
		t.Errorf("err = %v (len %d)", err, len(err.Error()))
	}
}

func TestWebhookPresets(t *testing.T) {
	cases := []struct {
		name string
		c    WebhookConfig
		want string // JSON (compared structurally) or raw body for custom
		raw  bool
	}{
		{name: "json", c: WebhookConfig{}, want: `{"type":"update.failed","severity":"error","title":"Update failed: web",
			"message":"pull <denied> & stuff","stack":"web","url":"https://dm.example.com/stacks/web","time":"2026-10-11T12:00:00Z"}`},
		{name: "discord", c: WebhookConfig{Preset: PresetDiscord}, want: `{"content":null,"embeds":[{"title":"Update failed: web",
			"description":"pull <denied> & stuff","url":"https://dm.example.com/stacks/web","color":15158332,"timestamp":"2026-10-11T12:00:00Z"}]}`},
		{name: "slack", c: WebhookConfig{Preset: PresetSlack}, want: `{"text":"*Update failed: web*\npull &lt;denied&gt; &amp; stuff <https://dm.example.com/stacks/web|Open>"}`},
		{name: "custom", c: WebhookConfig{Preset: PresetCustom, Template: `{"msg":{{json .Message}},"sev":"{{.Severity}}","stack":"{{.Stack}}"}`},
			want: `{"msg":"pull <denied> & stuff","sev":"error","stack":"web"}`, raw: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec, srv := newServer(t)
			tc.c.URL = srv.URL + "/hook?k=1"
			tc.c.Headers = map[string]string{"X-Token": "hdr-secret"}
			if err := send(t, Channel{Type: TypeWebhook, Config: cfg(tc.c)}, sample); err != nil {
				t.Fatal(err)
			}
			r := rec.all()[0]
			if r.Method != "POST" || r.Path != "/hook" || r.Query != "k=1" || r.Header.Get("X-Token") != "hdr-secret" ||
				r.Header.Get("Content-Type") != "application/json" {
				t.Errorf("%+v", r)
			}
			if tc.raw {
				if r.Body != tc.want {
					t.Errorf("body\n got %s\nwant %s", r.Body, tc.want)
				}
				return
			}
			var got, want any
			if err := json.Unmarshal([]byte(r.Body), &got); err != nil {
				t.Fatalf("body is not JSON: %s", r.Body)
			}
			json.Unmarshal([]byte(tc.want), &want)
			if !reflect.DeepEqual(got, want) {
				t.Errorf("body\n got %s\nwant %s", r.Body, tc.want)
			}
		})
	}
}

func TestWebhookMethodAndContentTypeOverride(t *testing.T) {
	rec, srv := newServer(t)
	c := WebhookConfig{URL: srv.URL, Method: "put", Preset: PresetCustom, Template: "{{.Title}}",
		Headers: map[string]string{"Content-Type": "text/plain"}}
	if err := send(t, Channel{Type: TypeWebhook, Config: cfg(c)}, sample); err != nil {
		t.Fatal(err)
	}
	r := rec.all()[0]
	if r.Method != "PUT" || r.Header.Get("Content-Type") != "text/plain" || r.Body != sample.Title {
		t.Errorf("%+v", r)
	}
}

func TestWebhookErrorsHideURL(t *testing.T) {
	dead := httptest.NewServer(http.NotFoundHandler())
	dead.Close()
	c := WebhookConfig{URL: dead.URL + "/api/webhooks/1/DISCORDTOKEN"}
	err := send(t, Channel{Type: TypeWebhook, Config: cfg(c)}, sample)
	if err == nil || strings.Contains(err.Error(), "DISCORDTOKEN") {
		t.Errorf("err = %v", err)
	}
}

func TestNewSenderRejectsInvalid(t *testing.T) {
	if _, err := NewSender(Channel{Type: TypeGotify, Name: "g", Config: cfg(GotifyConfig{})}, nil); err == nil {
		t.Error("invalid config accepted")
	}
}
