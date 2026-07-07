// Package webui serves the embedded web UI: a browser client of the /v1 query
// API. Assets are embedded (go:embed, pure Go — the cgo-free static binary is
// preserved) and served under /ui/. The UI's only data path is same-origin
// fetch('/v1/...'): it is a client of the JSON contract, never of the store.
package webui

import (
	"embed"
	"io/fs"
	"net/http"
	"strings"
)

//go:embed static
var staticFS embed.FS

// Handler serves the embedded UI. Mount it under /ui/ — the handler strips
// that prefix itself.
func Handler() http.Handler {
	sub, err := fs.Sub(staticFS, "static")
	if err != nil {
		panic("webui: embedded static tree missing: " + err.Error()) // unreachable: compiled in
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/ui/") {
			http.NotFound(w, r)
			return
		}
		path := strings.TrimPrefix(r.URL.Path, "/ui")
		if path == "" || path == "/" {
			path = "/index.html"
		}
		path = strings.TrimPrefix(path, "/")
		data, err := fs.ReadFile(sub, path)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		contentType := "application/octet-stream"
		if strings.HasSuffix(path, ".html") {
			contentType = "text/html; charset=utf-8"
		} else if strings.HasSuffix(path, ".css") {
			contentType = "text/css; charset=utf-8"
		}
		w.Header().Set("Content-Type", contentType)
		w.Write(data)
	})
}
