// Package queryapi serves the localhost HTTP+JSON query API over the local
// history store. This JSON surface — not the database schema — is the stable
// contract that UIs (cli, tui, web, fzf, ...) target.
package queryapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/mjacobs/yas/internal/record"
	"github.com/mjacobs/yas/internal/store"
	"github.com/mjacobs/yas/internal/webui"
)

// ContractVersion identifies the stable query-API / record-JSON contract. It is
// bumped only on a breaking change to the record shape (see docs/api/query-api-v1.md).
const ContractVersion = "v1"

// versionResponse is the JSON body for GET /v1/version.
type versionResponse struct {
	Version      string   `json:"version"`
	RecordFields []string `json:"record_fields"`
}

// Searcher is the read capability the API needs from the store (the local
// SQLite replica satisfies it).
type Searcher interface {
	Search(ctx context.Context, q store.Query) ([]record.Record, error)
}

// SearchResponse is the JSON body returned by GET /v1/search.
type SearchResponse struct {
	Records []record.Record `json:"records"`
}

// NewHandler returns the HTTP handler for the query API, backed by s. Method
// patterns make the mux answer non-GET requests with 405 automatically.
func NewHandler(s Searcher) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/search", searchHandler(s))
	mux.HandleFunc("GET /v1/version", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, versionResponse{Version: ContractVersion, RecordFields: record.ContractFields()})
	})
	mux.HandleFunc("GET /v1/healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	// The embedded web UI is a client of this same /v1 contract, served on the
	// same listener for zero-config dogfooding. GET /{$} matches the bare root
	// only (Go 1.22 pattern), so /v1/* routing is unaffected.
	mux.Handle("GET /ui/", webui.Handler())
	mux.HandleFunc("GET /ui", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/ui/", http.StatusMovedPermanently)
	})
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/ui/", http.StatusMovedPermanently)
	})
	return mux
}

func searchHandler(s Searcher) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		q, err := queryFromValues(r.URL.Query())
		if err != nil {
			writeJSON(w, http.StatusBadRequest, errorResponse{Error: err.Error()})
			return
		}
		recs, err := s.Search(r.Context(), q)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, errorResponse{Error: err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, SearchResponse{Records: recs})
	}
}

// errorResponse is the JSON body for non-2xx responses.
type errorResponse struct {
	Error string `json:"error"`
}

// queryFromValues maps URL query parameters onto a store.Query. Unknown/blank
// params are ignored; malformed values are an error (-> HTTP 400).
func queryFromValues(v url.Values) (store.Query, error) {
	q := store.Query{
		ID:      v.Get("id"),
		Text:    v.Get("q"),
		Host:    v.Get("host"),
		CWD:     v.Get("cwd"),
		Session: v.Get("session"),
	}
	if s := v.Get("exit"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil {
			return store.Query{}, fmt.Errorf("invalid exit %q: must be an integer", s)
		}
		q.ExitCode = &n
	}
	for _, p := range []struct {
		key string
		dst *time.Time
	}{{"since", &q.Since}, {"until", &q.Until}} {
		if s := v.Get(p.key); s != "" {
			ts, err := time.Parse(time.RFC3339, s)
			if err != nil {
				return store.Query{}, fmt.Errorf("invalid %s %q: must be RFC3339 (e.g. 2006-01-02T15:04:05Z)", p.key, s)
			}
			*p.dst = ts
		}
	}
	for _, p := range []struct {
		key string
		dst *int
	}{{"limit", &q.Limit}, {"offset", &q.Offset}} {
		if s := v.Get(p.key); s != "" {
			n, err := strconv.Atoi(s)
			if err != nil || n < 0 {
				return store.Query{}, fmt.Errorf("invalid %s %q: must be a non-negative integer", p.key, s)
			}
			*p.dst = n
		}
	}
	if s := v.Get("failed"); s != "" {
		b, err := strconv.ParseBool(s)
		if err != nil {
			return store.Query{}, fmt.Errorf("invalid failed %q: must be a boolean", s)
		}
		q.FailedOnly = b
	}
	if s := v.Get("reverse"); s != "" {
		b, err := strconv.ParseBool(s)
		if err != nil {
			return store.Query{}, fmt.Errorf("invalid reverse %q: must be a boolean", s)
		}
		q.Reverse = b
	}
	q.ApplyExecutorToken(v.Get("executor"))
	return q, nil
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
