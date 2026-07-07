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

func TestHandlerUnknownPathIs404(t *testing.T) {
	if res := get(t, Handler(), "/ui/nope.js"); res.StatusCode != http.StatusNotFound {
		t.Fatalf("GET /ui/nope.js = %d, want 404", res.StatusCode)
	}
}
