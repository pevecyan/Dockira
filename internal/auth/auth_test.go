package auth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"

	"docker-manager/internal/config"
)

const password = "correct horse battery staple"

type rec struct {
	mu     sync.Mutex
	events []string
	known  map[string]bool
}

func (r *rec) Record(_ context.Context, kind, ip, _ string, ok bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, kind+"@"+ip)
	if kind == "login" && ok {
		r.known[ip] = true
	}
}

func (r *rec) KnownIP(_ context.Context, ip string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.known[ip]
}

type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

func newAuth(t *testing.T, mod func(*Options)) (*Auth, http.Handler, *rec, *[]string, *clock) {
	t.Helper()
	h, _ := bcrypt.GenerateFromPassword([]byte(password), bcrypt.MinCost)
	proxies, _ := config.ParseCIDRs([]string{"10.0.0.2"})
	r := &rec{known: map[string]bool{}}
	var notes []string
	var mu sync.Mutex
	c := &clock{time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)}
	o := Options{PasswordHash: string(h), Key: []byte(strings.Repeat("k", 32)), TrustedProxies: proxies,
		PublicURL: "https://docker.example.com", Recorder: r, Now: c.now,
		Notify: func(ev, ip, _ string) { mu.Lock(); notes = append(notes, ev+"@"+ip); mu.Unlock() }}
	if mod != nil {
		mod(&o)
	}
	a := New(o)
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok " + r.URL.Path)) })
	return a, a.Middleware(next), r, &notes, c
}

type req struct {
	method, path, body, origin, remote, xff, xfp string
	cookies                                      []*http.Cookie
}

func do(h http.Handler, q req) *httptest.ResponseRecorder {
	if q.method == "" {
		q.method = "GET"
	}
	r := httptest.NewRequest(q.method, "http://docker.example.com"+q.path, strings.NewReader(q.body))
	if q.body != "" {
		r.Header.Set("Content-Type", "application/json")
	}
	if q.origin != "" {
		r.Header.Set("Origin", q.origin)
	}
	r.RemoteAddr = "192.0.2.1:5555"
	if q.remote != "" {
		r.RemoteAddr = q.remote
	}
	if q.xff != "" {
		r.Header.Set("X-Forwarded-For", q.xff)
	}
	if q.xfp != "" {
		r.Header.Set("X-Forwarded-Proto", q.xfp)
	}
	for _, c := range q.cookies {
		r.AddCookie(c)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func login(t *testing.T, h http.Handler, pw string) *httptest.ResponseRecorder {
	t.Helper()
	return do(h, req{method: "POST", path: "/api/login", body: `{"password":"` + pw + `"}`})
}

func TestLoginFlow(t *testing.T) {
	_, h, r, notes, c := newAuth(t, nil)

	if w := do(h, req{path: "/api/stacks"}); w.Code != 401 || !strings.Contains(w.Body.String(), `"login":true`) {
		t.Fatalf("no session: %d %s", w.Code, w.Body)
	}
	if w := do(h, req{path: "/stacks/gateway"}); w.Code != 200 {
		t.Errorf("UI files need no login: %d", w.Code)
	}
	if w := login(t, h, "wrong"); w.Code != 401 {
		t.Errorf("wrong password: %d", w.Code)
	}
	w := login(t, h, password)
	if w.Code != 204 {
		t.Fatalf("login: %d %s", w.Code, w.Body)
	}
	cookies := w.Result().Cookies()
	if len(cookies) != 1 || cookies[0].Name != SessionCookie || !cookies[0].HttpOnly || cookies[0].SameSite != http.SameSiteStrictMode || cookies[0].Secure {
		t.Errorf("cookie %+v", cookies)
	}
	if w := do(h, req{path: "/api/stacks", cookies: cookies}); w.Code != 200 {
		t.Errorf("with session: %d", w.Code)
	}
	if len(*notes) != 1 || (*notes)[0] != "auth.login_new_ip@192.0.2.1" {
		t.Errorf("new IP alert: %v", *notes)
	}
	login(t, h, password)
	if len(*notes) != 1 {
		t.Errorf("known IP alerted again: %v", *notes)
	}
	if strings.Join(r.events, ",") != "login_failed@192.0.2.1,login@192.0.2.1,login@192.0.2.1" {
		t.Errorf("recorded %v", r.events)
	}

	// expiry
	c.t = c.t.Add(SessionTTL + time.Minute)
	if w := do(h, req{path: "/api/stacks", cookies: cookies}); w.Code != 401 {
		t.Errorf("expired session accepted: %d", w.Code)
	}
}

func TestSessionTiedToPasswordAndKey(t *testing.T) {
	_, h, _, _, _ := newAuth(t, nil)
	cookies := login(t, h, password).Result().Cookies()
	other, _ := bcrypt.GenerateFromPassword([]byte("another password!!"), bcrypt.MinCost)
	_, h2, _, _, _ := newAuth(t, func(o *Options) { o.PasswordHash = string(other) })
	if w := do(h2, req{path: "/api/x", cookies: cookies}); w.Code != 401 {
		t.Error("session survived a password change")
	}
	_, h3, _, _, _ := newAuth(t, func(o *Options) { o.Key = []byte(strings.Repeat("z", 32)) })
	if w := do(h3, req{path: "/api/x", cookies: cookies}); w.Code != 401 {
		t.Error("session survived a key change")
	}
	// a session value is not a confirmation (the MAC covers the purpose)
	a, _, _, _, _ := newAuth(t, nil)
	r := httptest.NewRequest("GET", "/", nil)
	r.AddCookie(&http.Cookie{Name: ConfirmCookie, Value: cookies[0].Value})
	if a.Confirmed(r) {
		t.Error("session value accepted as confirmation")
	}
}

func TestPerIPThrottle(t *testing.T) {
	_, h, _, _, c := newAuth(t, nil)
	for i := range maxAttempts {
		if w := login(t, h, "nope"); w.Code != 401 {
			t.Fatalf("attempt %d: %d", i, w.Code)
		}
	}
	w := login(t, h, password)
	if w.Code != 429 || w.Header().Get("Retry-After") == "" {
		t.Fatalf("6th attempt: %d", w.Code)
	}
	if w := do(h, req{method: "POST", path: "/api/login", body: `{"password":"` + password + `"}`, remote: "198.51.100.9:1"}); w.Code != 204 {
		t.Errorf("other address throttled: %d", w.Code)
	}
	c.t = c.t.Add(lockout + time.Second)
	if w := login(t, h, password); w.Code != 204 {
		t.Errorf("after lockout: %d", w.Code)
	}
}

func TestGlobalLockout(t *testing.T) {
	_, h, _, notes, c := newAuth(t, nil)
	for i := range globalMax {
		do(h, req{method: "POST", path: "/api/login", body: `{"password":"x"}`, remote: "203.0.113." + strconv.Itoa(i) + ":1"})
	}
	if w := do(h, req{method: "POST", path: "/api/login", body: `{"password":"` + password + `"}`, remote: "198.51.100.1:1"}); w.Code != 429 {
		t.Errorf("global pause not applied: %d", w.Code)
	}
	if len(*notes) != 1 || !strings.HasPrefix((*notes)[0], "auth.lockout") {
		t.Errorf("notes %v", *notes)
	}
	c.t = c.t.Add(globalPause + time.Second)
	if w := do(h, req{method: "POST", path: "/api/login", body: `{"password":"` + password + `"}`, remote: "198.51.100.1:1"}); w.Code != 204 {
		t.Errorf("after pause: %d", w.Code)
	}
}

func TestClientIPAndSecure(t *testing.T) {
	a, _, _, _, _ := newAuth(t, nil)
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "192.0.2.7:1"
	r.Header.Set("X-Forwarded-For", "1.2.3.4")
	r.Header.Set("X-Forwarded-Proto", "https")
	if a.ClientIP(r) != "192.0.2.7" || a.Secure(r) {
		t.Error("untrusted peer's forwarded headers believed")
	}
	r.RemoteAddr = "10.0.0.2:1"
	r.Header.Set("X-Forwarded-For", "6.6.6.6, 1.2.3.4") // client spoofed 6.6.6.6, proxy appended the real 1.2.3.4
	if got := a.ClientIP(r); got != "1.2.3.4" || !a.Secure(r) {
		t.Errorf("via proxy: %s secure=%v", got, a.Secure(r))
	}
}

func TestOriginAllowlistReadOnly(t *testing.T) {
	_, h, _, _, _ := newAuth(t, func(o *Options) {
		o.Allowlist, _ = config.ParseCIDRs([]string{"192.0.2.0/24"})
		o.ReadOnly = true
	})
	if w := do(h, req{path: "/api/x", remote: "203.0.113.5:1"}); w.Code != 403 {
		t.Errorf("address outside allowlist: %d", w.Code)
	}
	cookies := login(t, h, password).Result().Cookies()
	if w := do(h, req{method: "POST", path: "/api/stacks/x/actions", body: "{}", origin: "https://evil.example", cookies: cookies}); w.Code != 403 || !strings.Contains(w.Body.String(), "cross-origin") {
		t.Errorf("cross origin: %d %s", w.Code, w.Body)
	}
	if w := do(h, req{method: "POST", path: "/api/stacks/x/actions", body: "{}", origin: "https://docker.example.com", cookies: cookies}); w.Code != 403 || !strings.Contains(w.Body.String(), "read-only") {
		t.Errorf("read-only: %d %s", w.Code, w.Body)
	}
	if w := do(h, req{path: "/api/x", origin: "https://evil.example", cookies: cookies}); w.Code != 200 {
		t.Errorf("GET with foreign origin should pass: %d", w.Code)
	}
}

func TestConfirm(t *testing.T) {
	a, h, _, _, c := newAuth(t, nil)
	cookies := login(t, h, password).Result().Cookies()
	r := httptest.NewRequest("POST", "/api/x", nil)
	for _, ck := range cookies {
		r.AddCookie(ck)
	}
	w := httptest.NewRecorder()
	if a.RequireConfirm(w, r) || w.Code != 403 || !strings.Contains(w.Body.String(), `"confirm":true`) {
		t.Fatalf("unconfirmed: %d %s", w.Code, w.Body)
	}
	if w := do(h, req{method: "POST", path: "/api/confirm", body: `{"password":"bad"}`, cookies: cookies}); w.Code != 401 {
		t.Errorf("bad confirm: %d", w.Code)
	}
	cw := do(h, req{method: "POST", path: "/api/confirm", body: `{"password":"` + password + `"}`, cookies: cookies})
	if cw.Code != 204 {
		t.Fatalf("confirm: %d", cw.Code)
	}
	r.AddCookie(cw.Result().Cookies()[0])
	if !a.Confirmed(r) {
		t.Error("confirmation not accepted")
	}
	c.t = c.t.Add(ConfirmTTL + time.Second)
	if a.Confirmed(r) {
		t.Error("confirmation outlived its TTL")
	}
}
