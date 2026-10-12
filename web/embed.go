// Package web embeds the built Vue UI (web/dist, produced by `npm run build`).
package web

import (
	"crypto/sha256"
	"embed"
	"encoding/base64"
	"io/fs"
	"net/http"
	"path"
	"regexp"
	"strings"
)

//go:embed all:dist
var dist embed.FS

// Handler serves the UI with SPA fallback to index.html.
func Handler() http.Handler {
	sub, _ := fs.Sub(dist, "dist")
	files := http.FileServerFS(sub)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
		if p == "" {
			p = "index.html"
		}
		if _, err := fs.Stat(sub, p); err != nil {
			if path.Ext(p) != "" {
				http.NotFound(w, r)
				return
			}
			// client-side route
			b, err := fs.ReadFile(sub, "index.html")
			if err != nil {
				http.Error(w, "UI not built: run `make web`", http.StatusNotFound)
				return
			}
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.Header().Set("Cache-Control", "no-cache")
			w.Write(b)
			return
		}
		if strings.HasPrefix(p, "assets/") {
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		} else {
			w.Header().Set("Cache-Control", "no-cache")
		}
		files.ServeHTTP(w, r)
	})
}

// ScriptHashes returns CSP source hashes ("sha256-…") of the inline <script> blocks in
// index.html, so the Content-Security-Policy can allow exactly those.
func ScriptHashes() []string {
	b, err := fs.ReadFile(dist, "dist/index.html")
	if err != nil {
		return nil
	}
	var out []string
	for _, m := range inlineScript.FindAllSubmatch(b, -1) {
		sum := sha256.Sum256(m[1])
		out = append(out, "sha256-"+base64.StdEncoding.EncodeToString(sum[:]))
	}
	return out
}

var inlineScript = regexp.MustCompile(`(?s)<script>(.*?)</script>`)
