package queryapi_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/mjacobs/yas/internal/queryapi"
	"github.com/mjacobs/yas/internal/record"
	sqlitestore "github.com/mjacobs/yas/internal/store/sqlite"
)

var base = time.UnixMilli(1_700_000_000_000)

func ptr[T any](v T) *T { return &v }

// newTestServer returns an httptest server backed by a real sqlite store seeded
// with the given records.
func newTestServer(t *testing.T, recs ...record.Record) *httptest.Server {
	t.Helper()
	db, err := sqlitestore.Open(filepath.Join(t.TempDir(), "history.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if len(recs) > 0 {
		if err := db.Put(context.Background(), recs...); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	srv := httptest.NewServer(queryapi.NewHandler(db))
	t.Cleanup(srv.Close)
	return srv
}

func getJSON(t *testing.T, url string) (*http.Response, queryapi.SearchResponse) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	var out queryapi.SearchResponse
	if resp.Header.Get("Content-Type") == "application/json" {
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			t.Fatalf("decode: %v", err)
		}
	}
	return resp, out
}

func TestSearch_ReturnsRecordsAsJSON(t *testing.T) {
	srv := newTestServer(t,
		record.Record{ID: "r1", Command: "git status", Hostname: "h", ExitCode: ptr(0), StartTime: base, CreatedAt: base},
		record.Record{ID: "r2", Command: "ls", Hostname: "h", StartTime: base.Add(time.Minute), CreatedAt: base},
	)

	resp, body := getJSON(t, srv.URL+"/v1/search")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: got %d want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
		t.Fatalf("content-type: got %q want application/json", ct)
	}
	if len(body.Records) != 2 {
		t.Fatalf("want 2 records, got %d", len(body.Records))
	}
	// newest-first default: r2 (later start) before r1
	if body.Records[0].ID != "r2" || body.Records[1].ID != "r1" {
		t.Fatalf("order/ids wrong: %+v", body.Records)
	}
	if body.Records[1].Command != "git status" {
		t.Errorf("command not serialized: %+v", body.Records[1])
	}
}

func corpus() []record.Record {
	m := time.Minute
	return []record.Record{
		{ID: "r1", Command: "git status", Hostname: "hostA", Session: "s1", ExitCode: ptr(0), StartTime: base, CreatedAt: base},
		{ID: "r2", Command: "git commit", Hostname: "hostA", Session: "s2", ExitCode: ptr(1), StartTime: base.Add(m), CreatedAt: base},
		{ID: "r3", Command: "docker ps", Hostname: "hostB", Session: "s1", ExitCode: ptr(0), StartTime: base.Add(2 * m), CreatedAt: base},
		{ID: "r4", Command: "ls -la", Hostname: "hostB", Session: "s2", ExitCode: ptr(0), StartTime: base.Add(3 * m), CreatedAt: base},
	}
}

func idsOf(rs []record.Record) []string {
	out := make([]string, len(rs))
	for i, r := range rs {
		out[i] = r.ID
	}
	return out
}

func sameSet(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	w := map[string]bool{}
	for _, x := range want {
		w[x] = true
	}
	for _, g := range got {
		if !w[g] {
			return false
		}
	}
	return true
}

func TestSearch_QueryParams_Filter(t *testing.T) {
	srv := newTestServer(t, corpus()...)
	cases := []struct {
		query string
		want  []string
	}{
		{"q=git", []string{"r1", "r2"}},
		{"q=docker", []string{"r3"}},
		{"host=hostA", []string{"r1", "r2"}},
		{"session=s1", []string{"r1", "r3"}},
		{"exit=1", []string{"r2"}},
		{"since=2023-11-14T22:14:20Z&until=2023-11-14T22:16:20Z", []string{"r2", "r3"}},
	}
	for _, c := range cases {
		_, body := getJSON(t, srv.URL+"/v1/search?"+c.query)
		if got := idsOf(body.Records); !sameSet(got, c.want) {
			t.Errorf("?%s: got %v want %v", c.query, got, c.want)
		}
	}
}

func TestSearch_QueryParams_OrderAndPaging(t *testing.T) {
	srv := newTestServer(t, corpus()...)

	_, body := getJSON(t, srv.URL+"/v1/search?limit=2")
	if got := idsOf(body.Records); len(got) != 2 || got[0] != "r4" || got[1] != "r3" {
		t.Errorf("limit=2 newest-first: got %v want [r4 r3]", got)
	}

	_, body = getJSON(t, srv.URL+"/v1/search?reverse=true&limit=2")
	if got := idsOf(body.Records); len(got) != 2 || got[0] != "r1" || got[1] != "r2" {
		t.Errorf("reverse limit=2: got %v want [r1 r2]", got)
	}
}

func TestSearch_RejectsNonGET(t *testing.T) {
	srv := newTestServer(t)
	resp, err := http.Post(srv.URL+"/v1/search", "application/json", nil)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("status: got %d want 405", resp.StatusCode)
	}
}

func TestSearch_BadParamIs400(t *testing.T) {
	srv := newTestServer(t, corpus()...)
	resp, _ := getJSON(t, srv.URL+"/v1/search?exit=notanint")
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status: got %d want 400", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("error content-type: got %q want application/json", ct)
	}
}

// The stable contract envelope is {"records": [...]} — a zero-match search must
// serialize as an empty JSON array, not null (which idiomatic clients mishandle).
func TestSearch_EmptyResultIsJSONArray(t *testing.T) {
	srv := newTestServer(t) // no records seeded
	resp, err := http.Get(srv.URL + "/v1/search?q=nomatch")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	body := strings.TrimSpace(string(raw))
	if !strings.Contains(body, `"records":[]`) || strings.Contains(body, `"records":null`) {
		t.Fatalf("empty result must be a JSON array, got: %s", body)
	}
}

func TestHealthz(t *testing.T) {
	srv := newTestServer(t)
	resp, err := http.Get(srv.URL + "/v1/healthz")
	if err != nil {
		t.Fatalf("GET healthz: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("healthz status: got %d want 200", resp.StatusCode)
	}
}

func TestSearch_ExecutorParam(t *testing.T) {
	srv := newTestServer(t,
		record.Record{ID: "h", Command: "ls", StartTime: base, CreatedAt: base, Executor: "human"},
		record.Record{ID: "a", Command: "deploy", StartTime: base.Add(time.Minute), CreatedAt: base, Executor: "claude-code"},
		// Legacy row from before executor tagging (574516f): no executor at
		// all. The human class must fold it in; exact-matching "human" would
		// silently drop it.
		record.Record{ID: "l", Command: "make", StartTime: base.Add(2 * time.Minute), CreatedAt: base},
	)
	for _, c := range []struct {
		query string
		want  []string
	}{
		{"executor=$all-agent", []string{"a"}},
		{"executor=$all-human", []string{"h", "l"}},
		{"executor=human", []string{"h", "l"}},
		{"executor=claude-code", []string{"a"}},
	} {
		_, body := getJSON(t, srv.URL+"/v1/search?"+c.query)
		if got := idsOf(body.Records); !sameSet(got, c.want) {
			t.Errorf("?%s: got %v want %v", c.query, got, c.want)
		}
	}
}

func TestVersion(t *testing.T) {
	srv := newTestServer(t)
	resp, err := http.Get(srv.URL + "/v1/version")
	if err != nil {
		t.Fatalf("GET version: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: got %d want 200", resp.StatusCode)
	}
	var out struct {
		Version      string   `json:"version"`
		RecordFields []string `json:"record_fields"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Version != "v1" {
		t.Errorf("version: got %q want v1", out.Version)
	}
	if want := record.ContractFields(); !reflect.DeepEqual(out.RecordFields, want) {
		t.Errorf("record_fields: got %v want %v", out.RecordFields, want)
	}
}
