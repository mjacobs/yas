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
//
// Serving is delegated to http.FileServer, which gives us the MIME type
// table (correct Content-Type for any future asset extension, not just
// .html/.css) and Range requests for free. Embedded files carry a zero
// ModTime, so FileServer emits no Last-Modified/ETag (no conditional-GET);
// Cache-Control: no-cache below makes browsers revalidate instead of
// heuristically caching assets across binary upgrades. The one wrinkle:
// http.FileServer 301-redirects any request whose path ends in "/index.html"
// to its directory form ("./"), which would break direct requests for
// /ui/index.html. The shim below rewrites the path to its directory form
// itself before handing off to FileServer, pre-empting that redirect.
func Handler() http.Handler {
	sub, err := fs.Sub(staticFS, "static")
	if err != nil {
		panic("webui: embedded static tree missing: " + err.Error()) // unreachable: compiled in
	}
	fileServer := http.FileServer(http.FS(sub))
	return http.StripPrefix("/ui/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// http.StripPrefix leaves r.URL.Path without a leading slash (e.g.
		// "index.html", "app.css", "" for "/ui/"), but http.FileServer
		// expects one.
		path := "/" + r.URL.Path
		if path == "/index.html" {
			path = "/"
		} else if strings.HasSuffix(path, "/index.html") {
			path = strings.TrimSuffix(path, "index.html")
		}
		r.URL.Path = path
		w.Header().Set("Cache-Control", "no-cache")
		fileServer.ServeHTTP(w, r)
	}))
}
