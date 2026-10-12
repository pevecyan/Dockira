package api

import (
	"bufio"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"docker-manager/internal/config"
)

type statusWriter struct {
	http.ResponseWriter
	code int
}

func (w *statusWriter) WriteHeader(code int) {
	w.code = code
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter } // Flush for SSE

// Hijack lets WebSocket upgrades (exec shell) through the logger.
func (w *statusWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	w.code = http.StatusSwitchingProtocols
	return http.NewResponseController(w.ResponseWriter).Hijack()
}

// LogRequests logs API calls: changes at info, reads at debug.
func LogRequests(log *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, code: http.StatusOK}
		next.ServeHTTP(sw, r)
		if len(r.URL.Path) < 5 || r.URL.Path[:5] != "/api/" {
			return
		}
		lvl := slog.LevelDebug
		if r.Method != http.MethodGet && r.Method != http.MethodHead || sw.code >= 500 {
			lvl = slog.LevelInfo
		}
		log.Log(r.Context(), lvl, "request", "method", r.Method, "path", r.URL.Path, "status", sw.code,
			"duration", time.Since(start).Round(time.Millisecond), "remote", r.RemoteAddr)
	})
}

// ScriptHashes are the CSP hashes ('sha256-…') of inline scripts in index.html (the theme
// bootstrap); set by the caller from the embedded UI.
var ScriptHashes []string

// securityHeaders: nothing external is loaded, nothing may frame the UI, no referrers leak.
func securityHeaders(w http.ResponseWriter, r *http.Request, cfg config.Config) {
	h := w.Header()
	script := "'self'"
	for _, sh := range ScriptHashes {
		script += " '" + sh + "'"
	}
	h.Set("Content-Security-Policy", "default-src 'self'; script-src "+script+"; style-src 'self' 'unsafe-inline'; img-src 'self' data:; "+
		"connect-src 'self'; font-src 'self'; object-src 'none'; base-uri 'self'; form-action 'self'; frame-ancestors 'none'")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("X-Frame-Options", "DENY")
	h.Set("Referrer-Policy", "no-referrer")
	if r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https") {
		h.Set("Cross-Origin-Opener-Policy", "same-origin") // browsers ignore (and warn about) it on plain HTTP
	}
	h.Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
	if u, err := url.Parse(cfg.PublicURL); err == nil && strings.EqualFold(u.Scheme, "https") {
		h.Set("Strict-Transport-Security", "max-age=31536000")
	}
}
