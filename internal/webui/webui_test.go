package webui

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func get(t *testing.T, h http.Handler, path string) *http.Response {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr.Result()
}

func TestHandlerServesIndex(t *testing.T) {
	h := Handler()
	for _, path := range []string{"/ui/", "/ui/index.html"} {
		res := get(t, h, path)
		if res.StatusCode != http.StatusOK {
			t.Fatalf("GET %s = %d, want 200", path, res.StatusCode)
		}
		if ct := res.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
			t.Fatalf("GET %s Content-Type = %q, want text/html", path, ct)
		}
		body, _ := io.ReadAll(res.Body)
		if !strings.Contains(string(body), "yas") {
			t.Fatalf("GET %s body missing wordmark:\n%s", path, body)
		}
	}
}

func TestHandlerServesCSS(t *testing.T) {
	res := get(t, Handler(), "/ui/app.css")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("GET /ui/app.css = %d, want 200", res.StatusCode)
	}
	if ct := res.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/css") {
		t.Fatalf("Content-Type = %q, want text/css", ct)
	}
}

// Embedded files have a zero ModTime, so http.FileServer emits no
// Last-Modified/ETag; without an explicit Cache-Control browsers would
// heuristically cache assets across binary upgrades. no-cache forces
// revalidation (a cheap localhost round-trip) so a new binary's UI wins.
func TestHandlerSetsCacheControl(t *testing.T) {
	h := Handler()
	for _, path := range []string{"/ui/", "/ui/app.css", "/ui/app.js"} {
		res := get(t, h, path)
		if cc := res.Header.Get("Cache-Control"); cc != "no-cache" {
			t.Errorf("GET %s Cache-Control = %q, want no-cache", path, cc)
		}
	}
}

func TestHandlerUnknownPathIs404(t *testing.T) {
	if res := get(t, Handler(), "/ui/nope.js"); res.StatusCode != http.StatusNotFound {
		t.Fatalf("GET /ui/nope.js = %d, want 404", res.StatusCode)
	}
}

// Pins the MIME behavior for extensions beyond .html/.css and that every JS
// module the page imports is embedded and served.
func TestHandlerServesJS(t *testing.T) {
	h := Handler()
	for _, path := range []string{"/ui/app.js", "/ui/tokens.js", "/ui/view.js"} {
		res := get(t, h, path)
		if res.StatusCode != http.StatusOK {
			t.Fatalf("GET %s = %d, want 200", path, res.StatusCode)
		}
		if ct := res.Header.Get("Content-Type"); !strings.Contains(ct, "javascript") {
			t.Fatalf("GET %s Content-Type = %q, want it to contain javascript", path, ct)
		}
	}
}

// The digest dashboard is a second embedded page: /ui/digest.html plus its
// module /ui/digest.js, a client of GET /v1/digest only.
func TestHandlerServesDigestPage(t *testing.T) {
	h := Handler()
	res := get(t, h, "/ui/digest.html")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("GET /ui/digest.html = %d, want 200", res.StatusCode)
	}
	if ct := res.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Fatalf("Content-Type = %q, want text/html", ct)
	}
	body, _ := io.ReadAll(res.Body)
	if !strings.Contains(string(body), "digest") {
		t.Fatalf("GET /ui/digest.html body missing digest marker:\n%s", body)
	}

	res = get(t, h, "/ui/digest.js")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("GET /ui/digest.js = %d, want 200", res.StatusCode)
	}
	if ct := res.Header.Get("Content-Type"); !strings.Contains(ct, "javascript") {
		t.Fatalf("Content-Type = %q, want it to contain javascript", ct)
	}
}

// The search page links to the digest dashboard and vice versa, so both views
// are reachable without knowing the URLs.
func TestPagesCrossLink(t *testing.T) {
	h := Handler()
	for path, want := range map[string]string{
		"/ui/":            `href="digest.html"`,
		"/ui/digest.html": `href="./"`,
	} {
		res := get(t, h, path)
		body, _ := io.ReadAll(res.Body)
		if !strings.Contains(string(body), want) {
			t.Errorf("GET %s body missing %s", path, want)
		}
	}
}
