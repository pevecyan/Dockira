// Package auth guards the API with the password from the config: POST
// /api/login trades it for a session cookie signed with secret_key over its expiry and the
// password hash, so changing either ends every session. Dangerous actions additionally need a
// recent password confirmation (a short-lived second cookie).
//
// Because the UI controls root on the host and may face the internet, logins are throttled per
// client address and globally, client addresses are taken from X-Forwarded-For only when the
// connection comes from a trusted proxy, and changes must come from the UI's own origin.
package auth

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"
)

const (
	SessionCookie = "dm_session"
	ConfirmCookie = "dm_confirm"
	SessionTTL    = 30 * 24 * time.Hour
	ConfirmTTL    = 10 * time.Minute

	maxAttempts  = 5                // per client address, then one try per lockout
	lockout      = time.Minute      // wait between tries once maxAttempts are used
	forget       = 15 * time.Minute // quiet time after which an address starts over
	globalMax    = 30               // failed logins per globalWindow from all addresses…
	globalWindow = time.Hour
	globalPause  = 15 * time.Minute // …pause every login for this long
)

// Recorder persists login events and answers whether an address has logged in before.
type Recorder interface {
	Record(ctx context.Context, kind, ip, ua string, ok bool)
	KnownIP(ctx context.Context, ip string) bool
}

// Notify receives security events: "auth.lockout" and "auth.login_new_ip".
type Notify func(event, ip, detail string)

type Options struct {
	PasswordHash   string
	Key            []byte // secret_key
	TrustedProxies []*net.IPNet
	Allowlist      []*net.IPNet // empty = everyone
	PublicURL      string       // allowed Origin besides the request's own host
	DevOrigins     []string     // e.g. http://localhost:5173 (Vite) in dev
	ReadOnly       bool
	Recorder       Recorder
	Notify         Notify
	Now            func() time.Time
}

type Auth struct {
	o       Options
	key     []byte
	origins map[string]bool
	mu      sync.Mutex
	perIP   map[string]attempt
	global  []time.Time // failure times inside globalWindow
	paused  time.Time   // logins paused until
}

type attempt struct {
	n    int
	last time.Time
}

func New(o Options) *Auth {
	if o.Now == nil {
		o.Now = time.Now
	}
	a := &Auth{o: o, perIP: map[string]attempt{}, origins: map[string]bool{}}
	m := hmac.New(sha256.New, o.Key)
	m.Write([]byte("docker-manager session v1"))
	a.key = m.Sum(nil) // a purpose-specific key: secret_key also encrypts channel secrets
	if u, err := url.Parse(o.PublicURL); err == nil && u.Host != "" {
		a.origins[u.Scheme+"://"+u.Host] = true
	}
	for _, d := range o.DevOrigins {
		a.origins[strings.TrimRight(d, "/")] = true
	}
	return a
}

// Middleware: allowlist → origin check → read-only → session. Login/logout/session/confirm
// are served here; UI files pass without a session (they carry no secrets and bring the login
// form), as does /healthz.
func (a *Auth) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ip := a.ClientIP(r)
		if !a.allowed(ip) {
			writeJSON(w, http.StatusForbidden, map[string]any{"title": "Forbidden", "status": 403, "detail": "your address is not allowed"})
			return
		}
		if !a.originOK(r) {
			writeJSON(w, http.StatusForbidden, map[string]any{"title": "Forbidden", "status": 403, "detail": "cross-origin request refused"})
			return
		}
		p := r.URL.Path
		switch {
		case p == "/api/login" && r.Method == http.MethodPost:
			a.login(w, r)
			return
		case p == "/api/logout" && r.Method == http.MethodPost:
			a.logout(w, r)
			return
		case p == "/api/session" && r.Method == http.MethodGet:
			writeJSON(w, http.StatusOK, map[string]any{"authenticated": a.Authenticated(r), "confirmed": a.Confirmed(r), "read_only": a.o.ReadOnly})
			return
		case p == "/healthz":
			next.ServeHTTP(w, r)
			return
		case p != "/api" && !strings.HasPrefix(p, "/api/"):
			next.ServeHTTP(w, r) // UI files
			return
		}
		if !a.Authenticated(r) {
			writeJSON(w, http.StatusUnauthorized, map[string]any{"title": "Unauthorized", "status": 401, "detail": "login required", "login": true})
			return
		}
		if p == "/api/confirm" && r.Method == http.MethodPost {
			a.confirm(w, r)
			return
		}
		if a.o.ReadOnly && !safeMethod(r.Method) {
			writeJSON(w, http.StatusForbidden, map[string]any{"title": "Forbidden", "status": 403, "detail": "read-only mode: changes are disabled", "read_only": true})
			return
		}
		next.ServeHTTP(w, r)
	})
}

func safeMethod(m string) bool {
	return m == http.MethodGet || m == http.MethodHead || m == http.MethodOptions
}

// ClientIP is the connection's address, or the last X-Forwarded-For hop added before reaching a
// trusted proxy chain when the connection comes from a trusted proxy.
func (a *Auth) ClientIP(r *http.Request) string {
	ip := hostOnly(r.RemoteAddr)
	if !a.trusted(ip) {
		return ip
	}
	hops := strings.Split(r.Header.Get("X-Forwarded-For"), ",")
	for i := len(hops) - 1; i >= 0; i-- {
		h := strings.TrimSpace(hops[i])
		if h == "" {
			continue
		}
		if !a.trusted(h) {
			return h
		}
		ip = h
	}
	return ip
}

// Secure reports whether the browser talks HTTPS to us (directly or via a trusted proxy).
func (a *Auth) Secure(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	return a.trusted(hostOnly(r.RemoteAddr)) && strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}

func (a *Auth) trusted(ip string) bool { return inNets(ip, a.o.TrustedProxies) }

func (a *Auth) allowed(ip string) bool { return len(a.o.Allowlist) == 0 || inNets(ip, a.o.Allowlist) }

func inNets(ip string, nets []*net.IPNet) bool {
	p := net.ParseIP(ip)
	if p == nil {
		return false
	}
	for _, n := range nets {
		if n.Contains(p) {
			return true
		}
	}
	return false
}

func hostOnly(addr string) string {
	if h, _, err := net.SplitHostPort(addr); err == nil {
		return h
	}
	return addr
}

// originOK: changes (and WebSocket upgrades) must come from the UI's own origin. Browsers
// always send Origin on those; requests without one are not from a browser page and can't
// carry the user's cookies by surprise.
func (a *Auth) originOK(r *http.Request) bool {
	if safeMethod(r.Method) && !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
		return true
	}
	o := r.Header.Get("Origin")
	if o == "" {
		return true
	}
	if a.origins[o] {
		return true
	}
	scheme := "http"
	if a.Secure(r) {
		scheme = "https"
	}
	host := r.Host
	if a.trusted(hostOnly(r.RemoteAddr)) {
		if fh := r.Header.Get("X-Forwarded-Host"); fh != "" {
			host = fh
		}
	}
	return o == scheme+"://"+host
}

func (a *Auth) Authenticated(r *http.Request) bool {
	c, err := r.Cookie(SessionCookie)
	return err == nil && a.valid(c.Value, "session")
}

// Confirmed reports a password confirmation within ConfirmTTL.
func (a *Auth) Confirmed(r *http.Request) bool {
	c, err := r.Cookie(ConfirmCookie)
	return err == nil && a.valid(c.Value, "confirm")
}

// ConfirmedValue checks a confirmation cookie's value (handlers that get cookies as input).
func (a *Auth) ConfirmedValue(v string) bool { return v != "" && a.valid(v, "confirm") }

// RequireConfirm answers 403 {confirm: true} unless the password was confirmed recently;
// the UI then asks for it and retries.
func (a *Auth) RequireConfirm(w http.ResponseWriter, r *http.Request) bool {
	if a.Confirmed(r) {
		return true
	}
	writeJSON(w, http.StatusForbidden, map[string]any{"title": "Forbidden", "status": 403, "detail": "confirm your password to continue", "confirm": true})
	return false
}

type loginRequest struct {
	Password string `json:"password"`
}

func (a *Auth) login(w http.ResponseWriter, r *http.Request) {
	ip, ua := a.ClientIP(r), r.UserAgent()
	if wait := a.start(ip); wait > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(int(wait.Seconds())+1))
		writeJSON(w, http.StatusTooManyRequests, map[string]any{"title": "Too Many Requests", "status": 429,
			"detail": fmt.Sprintf("too many failed logins: try again in %s", wait.Round(time.Second))})
		return
	}
	pw, ok := readPassword(w, r)
	if !ok {
		return
	}
	if bcrypt.CompareHashAndPassword([]byte(a.o.PasswordHash), []byte(pw)) != nil {
		a.fail(ip)
		a.record(r, "login_failed", ip, ua, false)
		writeJSON(w, http.StatusUnauthorized, map[string]any{"title": "Unauthorized", "status": 401, "detail": "wrong password"})
		return
	}
	a.reset(ip)
	if a.o.Recorder != nil && a.o.Notify != nil && !a.o.Recorder.KnownIP(r.Context(), ip) {
		a.o.Notify("auth.login_new_ip", ip, ua)
	}
	a.record(r, "login", ip, ua, true)
	http.SetCookie(w, a.cookie(r, SessionCookie, a.sign(a.o.Now().Add(SessionTTL), "session"), SessionTTL))
	w.WriteHeader(http.StatusNoContent)
}

func (a *Auth) confirm(w http.ResponseWriter, r *http.Request) {
	ip, ua := a.ClientIP(r), r.UserAgent()
	if wait := a.start(ip); wait > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(int(wait.Seconds())+1))
		writeJSON(w, http.StatusTooManyRequests, map[string]any{"title": "Too Many Requests", "status": 429,
			"detail": fmt.Sprintf("too many wrong passwords: try again in %s", wait.Round(time.Second))})
		return
	}
	pw, ok := readPassword(w, r)
	if !ok {
		return
	}
	if bcrypt.CompareHashAndPassword([]byte(a.o.PasswordHash), []byte(pw)) != nil {
		a.fail(ip)
		a.record(r, "confirm", ip, ua, false)
		writeJSON(w, http.StatusUnauthorized, map[string]any{"title": "Unauthorized", "status": 401, "detail": "wrong password"})
		return
	}
	a.reset(ip)
	a.record(r, "confirm", ip, ua, true)
	http.SetCookie(w, a.cookie(r, ConfirmCookie, a.sign(a.o.Now().Add(ConfirmTTL), "confirm"), ConfirmTTL))
	w.WriteHeader(http.StatusNoContent)
}

func (a *Auth) logout(w http.ResponseWriter, r *http.Request) {
	for _, name := range []string{SessionCookie, ConfirmCookie} {
		c := a.cookie(r, name, "", 0)
		c.MaxAge = -1
		http.SetCookie(w, c)
	}
	if a.Authenticated(r) {
		a.record(r, "logout", a.ClientIP(r), r.UserAgent(), true)
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *Auth) record(r *http.Request, kind, ip, ua string, ok bool) {
	if a.o.Recorder != nil {
		a.o.Recorder.Record(context.WithoutCancel(r.Context()), kind, ip, ua, ok)
	}
}

func readPassword(w http.ResponseWriter, r *http.Request) (string, bool) {
	if !strings.HasPrefix(strings.ToLower(r.Header.Get("Content-Type")), "application/json") {
		writeJSON(w, http.StatusUnsupportedMediaType, map[string]any{"title": "Unsupported Media Type", "status": 415, "detail": "send application/json"})
		return "", false
	}
	var req loginRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 4<<10)).Decode(&req); err != nil || req.Password == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"title": "Bad Request", "status": 400, "detail": `expected {"password": "…"}`})
		return "", false
	}
	return req.Password, true
}

func (a *Auth) cookie(r *http.Request, name, value string, ttl time.Duration) *http.Cookie {
	return &http.Cookie{Name: name, Value: value, Path: "/", HttpOnly: true, Secure: a.Secure(r),
		SameSite: http.SameSiteStrictMode, MaxAge: int(ttl.Seconds())}
}

// sign returns "expiry.MAC" where the MAC covers purpose, expiry and the password hash.
func (a *Auth) sign(exp time.Time, purpose string) string {
	s := strconv.FormatInt(exp.Unix(), 10)
	return s + "." + base64.RawURLEncoding.EncodeToString(a.mac(s, purpose))
}

func (a *Auth) mac(exp, purpose string) []byte {
	m := hmac.New(sha256.New, a.key)
	m.Write([]byte(purpose + "\n" + exp + "\n" + a.o.PasswordHash))
	return m.Sum(nil)
}

func (a *Auth) valid(v, purpose string) bool {
	if a.o.PasswordHash == "" || len(a.o.Key) == 0 {
		return false
	}
	exp, sig, ok := strings.Cut(v, ".")
	if !ok {
		return false
	}
	t, err := strconv.ParseInt(exp, 10, 64)
	if err != nil || a.o.Now().Unix() > t {
		return false
	}
	got, err := base64.RawURLEncoding.DecodeString(sig)
	return err == nil && hmac.Equal(got, a.mac(exp, purpose))
}

// start records an attempt from ip, or says how long to wait. An attempt counts when it starts,
// so parallel requests can't slip past the limit; a success starts the address over.
func (a *Auth) start(ip string) time.Duration {
	a.mu.Lock()
	defer a.mu.Unlock()
	now := a.o.Now()
	if wait := a.paused.Sub(now); wait > 0 {
		return wait
	}
	for k, e := range a.perIP {
		if now.Sub(e.last) > forget {
			delete(a.perIP, k)
		}
	}
	e := a.perIP[ip]
	if e.n >= maxAttempts {
		if wait := e.last.Add(lockout).Sub(now); wait > 0 {
			return wait
		}
	}
	a.perIP[ip] = attempt{e.n + 1, now}
	return 0
}

// fail counts a wrong password towards the global limit: guesses spread over many addresses
// pause all logins for a while (and alert).
func (a *Auth) fail(ip string) {
	a.mu.Lock()
	now := a.o.Now()
	kept := a.global[:0]
	for _, t := range a.global {
		if now.Sub(t) < globalWindow {
			kept = append(kept, t)
		}
	}
	a.global = append(kept, now)
	tripped := len(a.global) >= globalMax
	if tripped {
		a.paused = now.Add(globalPause)
		a.global = nil
	}
	a.mu.Unlock()
	if tripped && a.o.Notify != nil {
		a.o.Notify("auth.lockout", ip, fmt.Sprintf("%d failed logins within %s: logins paused for %s", globalMax, globalWindow, globalPause))
	}
}

func (a *Auth) reset(ip string) {
	a.mu.Lock()
	delete(a.perIP, ip)
	a.mu.Unlock()
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	ct := "application/json"
	if code >= 400 {
		ct = "application/problem+json"
	}
	w.Header().Set("Content-Type", ct)
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
