package syncapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/mjacobs/yas/internal/record"
	"github.com/mjacobs/yas/internal/syncapi"
	"github.com/mjacobs/yas/internal/syncproto"
)

const testToken = "s3cr3t-token"

var base = time.UnixMilli(1_700_000_000_000)

// fakeBackend records pushes and serves canned pulls.
type fakeBackend struct {
	put      []record.Record
	highSeq  int64
	sinceFn  func(seq int64, limit int) ([]record.Record, int64, error)
	putErr   error // if set, Put returns it
	sinceErr error // if set, Since returns it
}

func (f *fakeBackend) Put(_ context.Context, recs ...record.Record) error {
	if f.putErr != nil {
		return f.putErr
	}
	f.put = append(f.put, recs...)
	return nil
}
func (f *fakeBackend) HighSeq(context.Context) (int64, error) { return f.highSeq, nil }
func (f *fakeBackend) Since(_ context.Context, seq int64, limit int) ([]record.Record, int64, error) {
	if f.sinceErr != nil {
		return nil, seq, f.sinceErr
	}
	if f.sinceFn != nil {
		return f.sinceFn(seq, limit)
	}
	return []record.Record{}, seq, nil
}

func newServer(t *testing.T, b syncapi.Backend) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(syncapi.NewHandler(b, testToken))
	t.Cleanup(srv.Close)
	return srv
}

// do issues a request with a valid bearer token unless token is overridden.
func do(t *testing.T, method, url, token string, body io.Reader) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, url, body)
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func TestPush(t *testing.T) {
	fb := &fakeBackend{highSeq: 7}
	srv := newServer(t, fb)

	body, _ := json.Marshal(syncproto.PushRequest{Records: []record.Record{
		{ID: "019ef273-4ad8-76d8-aaaa-000000000001", Command: "git status", StartTime: base, CreatedAt: base},
		{ID: "019ef273-4ad8-76d8-aaaa-000000000002", Command: "ls", StartTime: base, CreatedAt: base},
	}})
	resp := do(t, http.MethodPost, srv.URL+"/v1/sync/push", testToken, bytes.NewReader(body))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: got %d want 200", resp.StatusCode)
	}
	var out syncproto.PushResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Accepted != 2 {
		t.Errorf("accepted: got %d want 2", out.Accepted)
	}
	if out.HighSeq != 7 {
		t.Errorf("high_seq: got %d want 7", out.HighSeq)
	}
	if len(fb.put) != 2 {
		t.Errorf("backend received %d records, want 2", len(fb.put))
	}
}

func TestPull(t *testing.T) {
	fb := &fakeBackend{sinceFn: func(seq int64, limit int) ([]record.Record, int64, error) {
		if seq != 5 {
			t.Errorf("since passed through as %d, want 5", seq)
		}
		return []record.Record{
			{ID: "a", Command: "one", StartTime: base, CreatedAt: base},
			{ID: "b", Command: "two", StartTime: base, CreatedAt: base},
		}, 9, nil
	}}
	srv := newServer(t, fb)

	resp := do(t, http.MethodGet, srv.URL+"/v1/sync/pull?since=5&limit=100", testToken, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: got %d want 200", resp.StatusCode)
	}
	var out syncproto.PullResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out.Records) != 2 || out.Records[0].ID != "a" || out.Records[1].ID != "b" {
		t.Fatalf("records: %+v", out.Records)
	}
	if out.NextSeq != 9 {
		t.Errorf("next_seq: got %d want 9", out.NextSeq)
	}
	if !out.Done {
		t.Errorf("done: got false want true (partial page)")
	}
}

// A full page (len == limit) means there may be more — Done must be false.
func TestPull_FullPageNotDone(t *testing.T) {
	fb := &fakeBackend{sinceFn: func(seq int64, limit int) ([]record.Record, int64, error) {
		return make([]record.Record, limit), seq + int64(limit), nil
	}}
	srv := newServer(t, fb)

	resp := do(t, http.MethodGet, srv.URL+"/v1/sync/pull?since=0&limit=2", testToken, nil)
	var out syncproto.PullResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out.Records) != 2 {
		t.Fatalf("want 2 records, got %d", len(out.Records))
	}
	if out.Done {
		t.Errorf("done: got true want false (full page)")
	}
}

func TestAuth_Rejects(t *testing.T) {
	srv := newServer(t, &fakeBackend{})
	cases := []struct{ name, method, path, token string }{
		{"push: no token", http.MethodPost, "/v1/sync/push", ""},
		{"push: wrong token", http.MethodPost, "/v1/sync/push", "nope"},
		{"pull: no token", http.MethodGet, "/v1/sync/pull", ""},
		{"pull: wrong token", http.MethodGet, "/v1/sync/pull", "nope"},
	}
	for _, c := range cases {
		var body io.Reader
		if c.method == http.MethodPost {
			body = strings.NewReader("{}")
		}
		resp := do(t, c.method, srv.URL+c.path, c.token, body)
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s: got %d want 401", c.name, resp.StatusCode)
		}
	}
}

// Each failed bearer auth logs one line including the remote address, so
// probing (or a misconfigured client) is visible in the server journal.
func TestAuth_FailureLogged(t *testing.T) {
	var buf bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&buf)
	defer log.SetOutput(prev)

	srv := newServer(t, &fakeBackend{})
	resp := do(t, http.MethodGet, srv.URL+"/v1/sync/pull", "wrong-token", nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status: got %d want 401", resp.StatusCode)
	}
	got := buf.String()
	if !strings.Contains(got, "auth failed") {
		t.Errorf("log missing auth-failure line: %q", got)
	}
	if !strings.Contains(got, "127.0.0.1") {
		t.Errorf("log missing remote address: %q", got)
	}
	if n := strings.Count(got, "auth failed"); n != 1 {
		t.Errorf("want exactly 1 log line per failure, got %d: %q", n, got)
	}
}

func TestPush_BadJSON(t *testing.T) {
	srv := newServer(t, &fakeBackend{})
	resp := do(t, http.MethodPost, srv.URL+"/v1/sync/push", testToken, strings.NewReader("{not json"))
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("got %d want 400", resp.StatusCode)
	}
}

func TestPush_InvalidRecordRejected(t *testing.T) {
	fb := &fakeBackend{}
	srv := newServer(t, fb)
	// missing ID -> Record.Validate fails -> 400, nothing stored.
	body, _ := json.Marshal(syncproto.PushRequest{Records: []record.Record{
		{Command: "no id", StartTime: base, CreatedAt: base},
	}})
	resp := do(t, http.MethodPost, srv.URL+"/v1/sync/push", testToken, bytes.NewReader(body))
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("got %d want 400", resp.StatusCode)
	}
	if len(fb.put) != 0 {
		t.Errorf("invalid batch must not be stored, got %d", len(fb.put))
	}
}

// makeRecords builds n minimal valid records.
func makeRecords(n int) []record.Record {
	recs := make([]record.Record, n)
	for i := range recs {
		recs[i] = record.Record{
			ID:      "019ef273-4ad8-76d8-bbbb-" + strconv.Itoa(100000000000+i),
			Command: "cmd", StartTime: base, CreatedAt: base,
		}
	}
	return recs
}

// A push may carry at most syncproto.MaxPushRecords records; an over-limit
// batch is a clean 400 with the API's JSON error shape, and nothing stored.
func TestPush_BatchTooLarge(t *testing.T) {
	fb := &fakeBackend{}
	srv := newServer(t, fb)

	body, _ := json.Marshal(syncproto.PushRequest{Records: makeRecords(syncproto.MaxPushRecords + 1)})
	resp := do(t, http.MethodPost, srv.URL+"/v1/sync/push", testToken, bytes.NewReader(body))
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status: got %d want 400", resp.StatusCode)
	}
	var e struct {
		Error string `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&e); err != nil || e.Error == "" {
		t.Fatalf("want JSON {\"error\":...} body, decode err=%v body=%+v", err, e)
	}
	if len(fb.put) != 0 {
		t.Errorf("over-limit batch must not be stored, got %d", len(fb.put))
	}
}

// A push body over syncproto.MaxPushBodyBytes is cut off by the server and
// answered with a clean 413 + the API's JSON error shape — never a 500. The
// body is a syntactically fine PushRequest whose one giant string forces the
// decoder past the byte limit, so this exercises the size guard, not the JSON
// parser.
func TestPush_BodyTooLarge(t *testing.T) {
	fb := &fakeBackend{}
	srv := newServer(t, fb)

	body := `{"records":[{"id":"a","command":"` +
		strings.Repeat("a", syncproto.MaxPushBodyBytes) + `"}]}`
	resp := do(t, http.MethodPost, srv.URL+"/v1/sync/push", testToken, strings.NewReader(body))
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("status: got %d want 413", resp.StatusCode)
	}
	var e struct {
		Error string `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&e); err != nil || e.Error == "" {
		t.Fatalf("want JSON {\"error\":...} body, decode err=%v body=%+v", err, e)
	}
	if len(fb.put) != 0 {
		t.Errorf("over-limit body must not be stored, got %d", len(fb.put))
	}
}

// The record-count cap must be enforced while streaming the array, not after
// materializing it — an under-byte-cap body packed with tiny records must be
// cut off at the cap instead of allocating them all first. The poisoned
// element after the cap position proves it: a handler that materialized the
// whole array before counting would report a JSON syntax error, not the
// count error.
func TestPush_BatchCapEnforcedWhileStreaming(t *testing.T) {
	fb := &fakeBackend{}
	srv := newServer(t, fb)

	body := `{"records":[` + strings.Repeat(`{},`, syncproto.MaxPushRecords+1) + `@@@]}`
	resp := do(t, http.MethodPost, srv.URL+"/v1/sync/push", testToken, strings.NewReader(body))
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status: got %d want 400", resp.StatusCode)
	}
	var e struct {
		Error string `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&e); err != nil {
		t.Fatalf("decode error body: %v", err)
	}
	if !strings.Contains(e.Error, "too many records") {
		t.Errorf("want the count-cap error (decoding stopped at the cap), got: %q", e.Error)
	}
	if len(fb.put) != 0 {
		t.Errorf("over-limit batch must not be stored, got %d", len(fb.put))
	}
}

// The push body must be exactly one JSON document. A valid PushRequest with
// bytes smuggled after it (a second document, garbage) is a 400 and nothing
// is stored; over-cap trailing padding that the decoder alone would never
// read is cut off as a 413.
func TestPush_TrailingDataRejected(t *testing.T) {
	valid := `{"records":[{"id":"a","command":"x","start_time":"2023-11-14T22:13:20Z","created_at":"2023-11-14T22:13:20Z"}]}`
	for name, tail := range map[string]struct {
		tail string
		want int
	}{
		"second document": {`{"records":[]}`, http.StatusBadRequest},
		"garbage":         {`garbage`, http.StatusBadRequest},
		"over-cap padding": {
			strings.Repeat(" ", syncproto.MaxPushBodyBytes),
			http.StatusRequestEntityTooLarge,
		},
	} {
		fb := &fakeBackend{}
		srv := newServer(t, fb)
		resp := do(t, http.MethodPost, srv.URL+"/v1/sync/push", testToken, strings.NewReader(valid+tail.tail))
		if resp.StatusCode != tail.want {
			t.Errorf("%s: got %d want %d", name, resp.StatusCode, tail.want)
		}
		if len(fb.put) != 0 {
			t.Errorf("%s: body with trailing data must not be stored, got %d", name, len(fb.put))
		}
	}
}

// A batch exactly at the cap is accepted.
func TestPush_BatchAtCapAccepted(t *testing.T) {
	fb := &fakeBackend{}
	srv := newServer(t, fb)

	body, _ := json.Marshal(syncproto.PushRequest{Records: makeRecords(syncproto.MaxPushRecords)})
	resp := do(t, http.MethodPost, srv.URL+"/v1/sync/push", testToken, bytes.NewReader(body))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: got %d want 200", resp.StatusCode)
	}
	if len(fb.put) != syncproto.MaxPushRecords {
		t.Errorf("backend received %d records, want %d", len(fb.put), syncproto.MaxPushRecords)
	}
}

func TestPull_BadParams(t *testing.T) {
	srv := newServer(t, &fakeBackend{})
	for _, q := range []string{"since=abc", "limit=abc", "since=-1"} {
		resp := do(t, http.MethodGet, srv.URL+"/v1/sync/pull?"+q, testToken, nil)
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("?%s: got %d want 400", q, resp.StatusCode)
		}
	}
}

// A backend (database) error must surface as a generic 500 — never leak the raw
// pgx/Postgres error text (schema, constraints) to the client.
func TestBackendErrorIsGeneric(t *testing.T) {
	const secret = "SECRET-pgerror-records_pkey-constraint-internals"

	t.Run("push", func(t *testing.T) {
		srv := newServer(t, &fakeBackend{putErr: errors.New(secret)})
		body, _ := json.Marshal(syncproto.PushRequest{Records: []record.Record{
			{ID: "019ef273-4ad8-76d8-aaaa-0000000000aa", Command: "x", StartTime: base, CreatedAt: base},
		}})
		resp := do(t, http.MethodPost, srv.URL+"/v1/sync/push", testToken, bytes.NewReader(body))
		assertGeneric500(t, resp, secret)
	})
	t.Run("pull", func(t *testing.T) {
		srv := newServer(t, &fakeBackend{sinceErr: errors.New(secret)})
		resp := do(t, http.MethodGet, srv.URL+"/v1/sync/pull?since=0", testToken, nil)
		assertGeneric500(t, resp, secret)
	})
}

func assertGeneric500(t *testing.T, resp *http.Response, secret string) {
	t.Helper()
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status: got %d want 500", resp.StatusCode)
	}
	raw, _ := io.ReadAll(resp.Body)
	if strings.Contains(string(raw), secret) {
		t.Fatalf("500 body leaked internal error: %s", raw)
	}
}

func TestMethodMismatch(t *testing.T) {
	srv := newServer(t, &fakeBackend{})
	resp := do(t, http.MethodGet, srv.URL+"/v1/sync/push", testToken, nil)
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("GET push: got %d want 405", resp.StatusCode)
	}
	resp = do(t, http.MethodPost, srv.URL+"/v1/sync/pull", testToken, strings.NewReader("{}"))
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("POST pull: got %d want 405", resp.StatusCode)
	}
}
