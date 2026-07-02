package syncclient_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mjacobs/yas/internal/record"
	"github.com/mjacobs/yas/internal/syncclient"
	"github.com/mjacobs/yas/internal/syncproto"
)

func TestPush(t *testing.T) {
	var gotMethod, gotPath, gotAuth string
	var gotReq syncproto.PushRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath, gotAuth = r.Method, r.URL.Path, r.Header.Get("Authorization")
		_ = json.NewDecoder(r.Body).Decode(&gotReq)
		_ = json.NewEncoder(w).Encode(syncproto.PushResponse{Accepted: len(gotReq.Records), HighSeq: 5})
	}))
	defer srv.Close()

	c := syncclient.New(srv.URL, "tok")
	resp, err := c.Push(context.Background(), []record.Record{{ID: "a"}, {ID: "b"}})
	if err != nil {
		t.Fatalf("Push: %v", err)
	}
	if gotMethod != http.MethodPost || gotPath != "/v1/sync/push" {
		t.Errorf("request: %s %s", gotMethod, gotPath)
	}
	if gotAuth != "Bearer tok" {
		t.Errorf("auth header: %q", gotAuth)
	}
	if len(gotReq.Records) != 2 {
		t.Errorf("server got %d records", len(gotReq.Records))
	}
	if resp.Accepted != 2 || resp.HighSeq != 5 {
		t.Errorf("resp: %+v", resp)
	}
}

// Push must never send more than syncproto.MaxPushRecords in one request —
// the server rejects bigger batches — so a large backlog is split into
// consecutive chunked pushes and the responses aggregated.
func TestPush_ChunksAtMaxPushRecords(t *testing.T) {
	const total = 2*syncproto.MaxPushRecords + 50
	var gotCounts []int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req syncproto.PushRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decode push %d: %v", len(gotCounts), err)
		}
		gotCounts = append(gotCounts, len(req.Records))
		_ = json.NewEncoder(w).Encode(syncproto.PushResponse{
			Accepted: len(req.Records),
			HighSeq:  int64(100 * len(gotCounts)),
		})
	}))
	defer srv.Close()

	recs := make([]record.Record, total)
	for i := range recs {
		recs[i] = record.Record{ID: "id"}
	}
	resp, err := syncclient.New(srv.URL, "tok").Push(context.Background(), recs)
	if err != nil {
		t.Fatalf("Push: %v", err)
	}
	want := []int{syncproto.MaxPushRecords, syncproto.MaxPushRecords, 50}
	if len(gotCounts) != len(want) {
		t.Fatalf("requests: got %v want %v", gotCounts, want)
	}
	for i := range want {
		if gotCounts[i] != want[i] {
			t.Errorf("request %d carried %d records, want %d", i, gotCounts[i], want[i])
		}
	}
	if resp.Accepted != total {
		t.Errorf("accepted: got %d want %d (sum over chunks)", resp.Accepted, total)
	}
	if resp.HighSeq != 300 {
		t.Errorf("high_seq: got %d want 300 (from the final chunk)", resp.HighSeq)
	}
}

// Chunks must respect the server's body-size cap too, not just the record
// count: records with commands near record.MaxCommandBytes make even a
// count-legal chunk encode far past syncproto.MaxPushBodyBytes, which the
// server rejects with 413 — retrying the same unsendable chunk would wedge
// sync forever.
func TestPush_ChunksByBodySize(t *testing.T) {
	var bodySizes, counts []int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read push %d: %v", len(bodySizes), err)
		}
		var req syncproto.PushRequest
		if err := json.Unmarshal(raw, &req); err != nil {
			t.Errorf("decode push %d: %v", len(bodySizes), err)
		}
		bodySizes = append(bodySizes, len(raw))
		counts = append(counts, len(req.Records))
		_ = json.NewEncoder(w).Encode(syncproto.PushResponse{Accepted: len(req.Records)})
	}))
	defer srv.Close()

	const total = 100 // 100 records x 256 KiB commands ~ 25.6 MiB encoded
	recs := make([]record.Record, total)
	big := strings.Repeat("a", record.MaxCommandBytes)
	for i := range recs {
		recs[i] = record.Record{ID: "id", Command: big}
	}
	resp, err := syncclient.New(srv.URL, "tok").Push(context.Background(), recs)
	if err != nil {
		t.Fatalf("Push: %v", err)
	}
	if len(bodySizes) < 2 {
		t.Fatalf("want the backlog split across multiple pushes, got %d", len(bodySizes))
	}
	var delivered int
	for i, size := range bodySizes {
		if size > syncproto.MaxPushBodyBytes {
			t.Errorf("push %d body is %d bytes, over the %d cap", i, size, syncproto.MaxPushBodyBytes)
		}
		delivered += counts[i]
	}
	if delivered != total {
		t.Errorf("delivered %d records across chunks, want %d", delivered, total)
	}
	if resp.Accepted != total {
		t.Errorf("accepted: got %d want %d", resp.Accepted, total)
	}
}

// A failure mid-way must surface as an error (already-pushed chunks are
// harmless: pushes are idempotent upserts and the caller retries next round).
func TestPush_ChunkFailureIsError(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls > 1 {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"error":"internal server error"}`))
			return
		}
		_ = json.NewEncoder(w).Encode(syncproto.PushResponse{Accepted: syncproto.MaxPushRecords})
	}))
	defer srv.Close()

	recs := make([]record.Record, syncproto.MaxPushRecords+1)
	for i := range recs {
		recs[i] = record.Record{ID: "id"}
	}
	if _, err := syncclient.New(srv.URL, "tok").Push(context.Background(), recs); err == nil {
		t.Fatal("expected an error when a chunk fails")
	}
	if calls != 2 {
		t.Errorf("server saw %d requests, want 2 (stop at first failure)", calls)
	}
}

func TestPull(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/sync/pull" {
			t.Errorf("path: %s", r.URL.Path)
		}
		if r.URL.Query().Get("since") != "7" {
			t.Errorf("since: %q", r.URL.Query().Get("since"))
		}
		_ = json.NewEncoder(w).Encode(syncproto.PullResponse{
			Records: []record.Record{{ID: "x"}}, NextSeq: 9, Done: true,
		})
	}))
	defer srv.Close()

	c := syncclient.New(srv.URL, "tok")
	resp, err := c.Pull(context.Background(), 7, 100)
	if err != nil {
		t.Fatalf("Pull: %v", err)
	}
	if len(resp.Records) != 1 || resp.Records[0].ID != "x" || resp.NextSeq != 9 || !resp.Done {
		t.Errorf("resp: %+v", resp)
	}
}

func TestNon200IsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"unauthorized"}`))
	}))
	defer srv.Close()

	c := syncclient.New(srv.URL, "bad")
	if _, err := c.Pull(context.Background(), 0, 10); err == nil {
		t.Fatal("expected an error on 401")
	}
}
