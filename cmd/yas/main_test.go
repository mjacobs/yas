package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mjacobs/yas/internal/queryapi"
	"github.com/mjacobs/yas/internal/record"
	"github.com/mjacobs/yas/internal/store"
	sqlitestore "github.com/mjacobs/yas/internal/store/sqlite"
	"github.com/mjacobs/yas/internal/syncapi"
	"github.com/mjacobs/yas/internal/syncclient"
)

// runServe must serve while the context lives and drain cleanly (nil, no
// process exit) when it is canceled — the signal-driven shutdown systemd's
// TERM relies on; a killed listener must not report as a failure.
func TestRunServe_GracefulShutdown(t *testing.T) {
	db := openTestStore(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := &http.Server{Handler: queryapi.NewHandler(db)}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	errCh := make(chan error, 1)
	go func() { errCh <- runServe(ctx, srv, ln) }()

	// The server answers while the context lives.
	url := "http://" + ln.Addr().String() + "/v1/healthz"
	var resp *http.Response
	for i := 0; i < 50; i++ {
		if resp, err = http.Get(url); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("healthz never came up: %v", err)
	}
	resp.Body.Close()

	// Cancel = SIGTERM/SIGINT arrived: runServe returns nil promptly.
	cancel()
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("graceful shutdown returned %v, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("runServe did not return after cancel")
	}
}

// A request still in flight when the grace period expires is force-closed and
// the stop still reports clean: a signal-initiated stop is never a failure
// (roborev 915 — systemd must not see TERM-under-load as exit!=0).
func TestRunServe_ForceCloseAfterGraceIsStillClean(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	entered := make(chan struct{})
	block := make(chan struct{})
	defer close(block)
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		<-block // hold the request past the grace period
	})}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	errCh := make(chan error, 1)
	go func() { errCh <- runServeGrace(ctx, srv, ln, 50*time.Millisecond) }()

	go func() {
		resp, err := http.Get("http://" + ln.Addr().String() + "/")
		if err == nil {
			resp.Body.Close()
		}
	}()
	<-entered // the slow request is in flight
	cancel()  // stop signal arrives while it hangs

	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("stop with a stuck request returned %v, want nil (forced close)", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("runServe did not return after grace expired")
	}
}

// memBackend is an in-memory syncapi.Backend that mimics the Postgres server:
// upsert by id with LWW on mutable fields, assigning a fresh seq on every write.
type memBackend struct {
	mu   sync.Mutex
	recs map[string]record.Record
	seq  map[string]int64
	next int64
}

func newMemBackend() *memBackend {
	return &memBackend{recs: map[string]record.Record{}, seq: map[string]int64{}}
}

func (m *memBackend) Put(_ context.Context, recs ...record.Record) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, r := range recs {
		m.next++
		if e, ok := m.recs[r.ID]; ok { // LWW on mutable fields; deleted is monotonic (sticky tombstone)
			e.ExitCode, e.DurationMS = r.ExitCode, r.DurationMS
			e.Deleted = e.Deleted || r.Deleted
			m.recs[r.ID] = e
		} else {
			m.recs[r.ID] = r
		}
		m.seq[r.ID] = m.next
	}
	return nil
}

func (m *memBackend) HighSeq(context.Context) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.next, nil
}

func (m *memBackend) Since(_ context.Context, since int64, limit int) ([]record.Record, int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	type sr struct {
		r record.Record
		s int64
	}
	var all []sr
	for id, s := range m.seq {
		if s > since {
			all = append(all, sr{m.recs[id], s})
		}
	}
	sort.Slice(all, func(i, j int) bool { return all[i].s < all[j].s })
	out := []record.Record{}
	next := since
	for _, x := range all {
		if len(out) >= limit {
			break
		}
		out = append(out, x.r)
		next = x.s
	}
	return out, next, nil
}

func openTestStore(t *testing.T) *sqlitestore.DB {
	t.Helper()
	db, err := sqlitestore.Open(filepath.Join(t.TempDir(), "history.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// The two-phase contract the zsh hook depends on: start persists an unfinished
// record and returns its id; finish finalizes THAT record in place.
func TestRecordStartThenFinish(t *testing.T) {
	db := openTestStore(t)
	ctx := context.Background()
	now := time.UnixMilli(1_700_000_000_000)

	id, err := doRecordStart(ctx, db, now, record.Record{
		Command: "make test", CWD: "/w", Session: "s1", Shell: "zsh",
		Hostname: "host", Username: "mj",
	})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if id == "" {
		t.Fatal("start returned empty id")
	}

	recs, err := db.Search(ctx, store.Query{ID: id})
	if err != nil {
		t.Fatalf("search after start: %v", err)
	}
	if len(recs) != 1 {
		t.Fatalf("want 1 stored record, got %d", len(recs))
	}
	if recs[0].Finished() {
		t.Fatal("record must not be Finished() before finish")
	}
	if recs[0].Command != "make test" || !recs[0].StartTime.Equal(now) {
		t.Fatalf("stored fields wrong: %+v", recs[0])
	}

	dur := int64(250)
	if err := doRecordFinish(ctx, db, id, 3, &dur); err != nil {
		t.Fatalf("finish: %v", err)
	}
	recs, _ = db.Search(ctx, store.Query{ID: id})
	if len(recs) != 1 {
		t.Fatalf("finish must not duplicate: got %d", len(recs))
	}
	g := recs[0]
	if !g.Finished() || *g.ExitCode != 3 {
		t.Fatalf("exit_code not set by finish: %v", g.ExitCode)
	}
	if g.DurationMS == nil || *g.DurationMS != 250 {
		t.Fatalf("duration_ms not set by finish: %v", g.DurationMS)
	}
	if g.Command != "make test" {
		t.Fatalf("finish must preserve command, got %q", g.Command)
	}
}

func TestRecordFinish_UnknownIDErrors(t *testing.T) {
	db := openTestStore(t)
	if err := doRecordFinish(context.Background(), db, "ghost", 0, nil); err == nil {
		t.Fatal("expected an error finishing an unknown id")
	}
}

// When the shell can't measure duration it passes --duration-ms -1, which the
// CLI maps to a nil pointer: the record still finishes (exit set) but keeps a
// null duration rather than a bogus 0.
func TestRecordFinish_WithoutDuration(t *testing.T) {
	db := openTestStore(t)
	ctx := context.Background()

	id, err := doRecordStart(ctx, db, time.UnixMilli(1_700_000_000_000), record.Record{Command: "true"})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if err := doRecordFinish(ctx, db, id, 5, nil); err != nil {
		t.Fatalf("finish: %v", err)
	}

	recs, _ := db.Search(ctx, store.Query{ID: id})
	if len(recs) != 1 {
		t.Fatalf("want 1 record, got %d", len(recs))
	}
	g := recs[0]
	if !g.Finished() || *g.ExitCode != 5 {
		t.Fatalf("exit_code not set: %v", g.ExitCode)
	}
	if g.DurationMS != nil {
		t.Errorf("duration_ms should stay nil, got %v", *g.DurationMS)
	}
}

// Flags must work whether they come before OR after the free-text query
// (git's flag pkg stops at the first operand, so we partition args ourselves).
func TestParseSearchArgs(t *testing.T) {
	cases := []struct {
		name     string
		args     []string
		wantText string
		wantJSON bool
		check    func(q store.Query) string // "" = ok, else failure msg
	}{
		{"text only", []string{"git", "status"}, "git status", false, nil},
		{"flag after text", []string{"git", "--json"}, "git", true, nil},
		{"flag before text", []string{"--json", "git"}, "git", true, nil},
		{"valued flag spaced after text", []string{"docker", "--host", "hostA"}, "docker", false,
			func(q store.Query) string {
				if q.Host != "hostA" {
					return "host=" + q.Host
				}
				return ""
			}},
		{"valued flag equals before text", []string{"--limit=2", "ls"}, "ls", false,
			func(q store.Query) string {
				if q.Limit != 2 {
					return "limit not 2"
				}
				return ""
			}},
		{"bool then text", []string{"--reverse", "x"}, "x", false,
			func(q store.Query) string {
				if !q.Reverse {
					return "reverse not set"
				}
				return ""
			}},
		{"exit zero is a real filter", []string{"--exit", "0", "y"}, "y", false,
			func(q store.Query) string {
				if q.ExitCode == nil || *q.ExitCode != 0 {
					return "exit filter missing"
				}
				return ""
			}},
		{"failed flag", []string{"--failed", "z"}, "z", false,
			func(q store.Query) string {
				if !q.FailedOnly {
					return "failed filter not set"
				}
				return ""
			}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			q, opts, err := parseSearchArgs(c.args)
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			if q.Text != c.wantText {
				t.Errorf("text: got %q want %q", q.Text, c.wantText)
			}
			if opts.asJSON != c.wantJSON {
				t.Errorf("json: got %v want %v", opts.asJSON, c.wantJSON)
			}
			if c.check != nil {
				if msg := c.check(q); msg != "" {
					t.Errorf("check: %s", msg)
				}
			}
		})
	}
}

// doSync must push local unsynced records to the server and pull the server's
// records into the local store, leaving nothing unsynced and advancing the cursor.
func TestDoSync_PushThenPull(t *testing.T) {
	local := openTestStore(t)
	ctx := context.Background()
	b := time.UnixMilli(1_700_000_000_000)
	// local has two unsynced records
	if err := local.Put(ctx,
		record.Record{ID: "a", Command: "cmd-a", StartTime: b, CreatedAt: b},
		record.Record{ID: "b", Command: "cmd-b", StartTime: b.Add(time.Minute), CreatedAt: b},
	); err != nil {
		t.Fatalf("seed local: %v", err)
	}
	// the server already has a record from "another machine"
	mem := newMemBackend()
	_ = mem.Put(ctx, record.Record{ID: "c", Command: "cmd-c", StartTime: b, CreatedAt: b})

	srv := httptest.NewServer(syncapi.NewHandler(mem, "tok"))
	defer srv.Close()
	client := syncclient.New(srv.URL, "tok")

	pushed, pulled, err := doSync(ctx, local, client, 100)
	if err != nil {
		t.Fatalf("doSync: %v", err)
	}
	if pushed != 2 {
		t.Errorf("pushed: got %d want 2", pushed)
	}
	if pulled < 1 {
		t.Errorf("pulled: got %d want >=1 (at least c)", pulled)
	}

	// server now has a, b, c
	srvRecs, _, _ := mem.Since(ctx, 0, 100)
	if !sameSet(idsOfRecs(srvRecs), []string{"a", "b", "c"}) {
		t.Errorf("server records: %v want a,b,c", idsOfRecs(srvRecs))
	}
	// local now has c too
	localAll, _ := local.Search(ctx, store.Query{})
	if !sameSet(idsOfRecs(localAll), []string{"a", "b", "c"}) {
		t.Errorf("local records: %v want a,b,c", idsOfRecs(localAll))
	}
	// nothing left unsynced, cursor advanced
	un, _ := local.Unsynced(ctx, 100)
	if len(un) != 0 {
		t.Errorf("unsynced after sync: %v want none", idsOfRecs(un))
	}
	if seq, _ := local.LastPulled(ctx); seq == 0 {
		t.Errorf("LastPulled should have advanced past 0")
	}
}

// Pushing more records than one batch must paginate through several rounds and
// leave nothing unsynced (guards the push-loop continuation logic).
func TestDoSync_MultiBatchPush(t *testing.T) {
	local := openTestStore(t)
	ctx := context.Background()
	b := time.UnixMilli(1_700_000_000_000)
	for i := 0; i < 5; i++ {
		if err := local.Put(ctx, record.Record{
			ID: string(rune('a' + i)), Command: "cmd", StartTime: b.Add(time.Duration(i) * time.Minute), CreatedAt: b,
		}); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	mem := newMemBackend()
	srv := httptest.NewServer(syncapi.NewHandler(mem, "tok"))
	defer srv.Close()

	pushed, _, err := doSync(ctx, local, syncclient.New(srv.URL, "tok"), 2) // batch=2 -> 3 rounds
	if err != nil {
		t.Fatalf("doSync: %v", err)
	}
	if pushed != 5 {
		t.Errorf("pushed: got %d want 5", pushed)
	}
	if srvRecs, _, _ := mem.Since(ctx, 0, 100); len(srvRecs) != 5 {
		t.Errorf("server has %d records, want 5", len(srvRecs))
	}
	if un, _ := local.Unsynced(ctx, 100); len(un) != 0 {
		t.Errorf("unsynced after sync: %d want 0", len(un))
	}
}

func idsOfRecs(recs []record.Record) []string {
	out := make([]string, len(recs))
	for i, r := range recs {
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

func seedTwo(t *testing.T, db *sqlitestore.DB) {
	t.Helper()
	b := time.UnixMilli(1_700_000_000_000)
	err := db.Put(context.Background(),
		record.Record{ID: "a", Command: "git status", ExitCode: ptr(0), StartTime: b, CreatedAt: b},
		record.Record{ID: "b", Command: "ls", ExitCode: ptr(2), StartTime: b.Add(time.Minute), CreatedAt: b},
	)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
}

func ptr[T any](v T) *T { return &v }

// --json output reuses the same envelope the HTTP API serves.
func TestDoSearch_JSON(t *testing.T) {
	db := openTestStore(t)
	seedTwo(t, db)

	var buf bytes.Buffer
	if err := doSearch(context.Background(), db, store.Query{}, &buf, searchOpts{asJSON: true, showSession: true, showDuration: true}, newCLIStyles(false)); err != nil {
		t.Fatalf("doSearch: %v", err)
	}
	var resp queryapi.SearchResponse
	if err := json.Unmarshal(buf.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v\n%s", err, buf.String())
	}
	if len(resp.Records) != 2 || resp.Records[0].ID != "b" || resp.Records[1].ID != "a" {
		t.Fatalf("want [b a] newest-first, got %+v", resp.Records)
	}
}

// JSON output is the UI contract and must never carry color, even when color is
// enabled.
func TestDoSearch_JSONNeverColored(t *testing.T) {
	db := openTestStore(t)
	seedTwo(t, db)
	var buf bytes.Buffer
	if err := doSearch(context.Background(), db, store.Query{}, &buf, searchOpts{asJSON: true, showSession: true, showDuration: true}, newCLIStyles(true)); err != nil {
		t.Fatalf("doSearch: %v", err)
	}
	if strings.Contains(buf.String(), "\x1b[") {
		t.Fatalf("JSON output must not contain ANSI escapes:\n%q", buf.String())
	}
}

func TestDoSearch_Human(t *testing.T) {
	db := openTestStore(t)
	seedTwo(t, db)

	var buf bytes.Buffer
	if err := doSearch(context.Background(), db, store.Query{}, &buf, searchOpts{asJSON: false, showSession: true, showDuration: true}, newCLIStyles(false)); err != nil {
		t.Fatalf("doSearch: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "git status") || !strings.Contains(out, "ls") {
		t.Fatalf("human output missing commands:\n%s", out)
	}
	// exit codes surfaced
	if !strings.Contains(out, "0") || !strings.Contains(out, "2") {
		t.Fatalf("human output missing exit codes:\n%s", out)
	}
	// plain styles emit no ANSI
	if strings.Contains(out, "\x1b[") {
		t.Fatalf("no-color human output must be plain:\n%q", out)
	}
}

// With color enabled the human search output carries ANSI escapes, and the
// non-zero exit is colored differently from the zero exit.
func TestDoSearch_Colored(t *testing.T) {
	db := openTestStore(t)
	seedTwo(t, db) // exit 0 ("git status") and exit 2 ("ls")

	var buf bytes.Buffer
	if err := doSearch(context.Background(), db, store.Query{}, &buf, searchOpts{asJSON: false, showSession: true, showDuration: true}, newCLIStyles(true)); err != nil {
		t.Fatalf("doSearch: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "\x1b[") {
		t.Fatalf("colored output should contain ANSI escapes:\n%q", out)
	}
	// color routes to the rich layout (titled)
	if !strings.Contains(out, "yas · search") {
		t.Errorf("colored search should use the rich (titled) layout:\n%s", out)
	}
	// commands themselves are still present and readable
	if !strings.Contains(out, "git status") || !strings.Contains(out, "ls") {
		t.Fatalf("colored output missing commands:\n%s", out)
	}
}

// The result token maps outcome -> color: green for 0, bold red for non-zero,
// yellow for still-running. Asserting the actual SGR codes (not just "some
// ANSI") catches a red/green swap, a collapsed palette, or a passthrough exit().
func TestCLIStyles_ExitColors(t *testing.T) {
	s := newCLIStyles(true)
	ok := s.exit(record.Record{ExitCode: ptr(0)})
	fail := s.exit(record.Record{ExitCode: ptr(2)})
	wait := s.exit(record.Record{}) // nil exit -> [-]

	// visible text survives styling
	for _, c := range []struct{ tok, want string }{{ok, "[0]"}, {fail, "[2]"}, {wait, "[-]"}} {
		if got := ansiPattern.ReplaceAllString(c.tok, ""); got != c.want {
			t.Errorf("visible text: got %q want %q", got, c.want)
		}
	}
	// each outcome is styled distinctly
	if ok == fail || ok == wait || fail == wait {
		t.Fatalf("exit styles must differ: ok=%q fail=%q wait=%q", ok, fail, wait)
	}
	// green ok, bold red fail, yellow wait (SGR: 32 / 1;31 / 33)
	if !strings.Contains(ok, "32") {
		t.Errorf("exit 0 should be green (SGR 32): %q", ok)
	}
	if !strings.Contains(fail, "31") || !strings.Contains(fail, "1;") {
		t.Errorf("non-zero exit should be bold red (SGR 1;31): %q", fail)
	}
	if !strings.Contains(wait, "33") {
		t.Errorf("unfinished should be yellow (SGR 33): %q", wait)
	}
	// color off -> plain passthrough, no escapes
	if plain := newCLIStyles(false).exit(record.Record{ExitCode: ptr(2)}); plain != "[2]" {
		t.Errorf("no-color exit must be plain [2], got %q", plain)
	}
}

// colorTerminal is the auto-off gate: NO_COLOR or a non-TTY writer disables color.
func TestColorTerminal(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	if colorTerminal(os.Stdout) {
		t.Error("NO_COLOR set: want color off")
	}
	// A pipe is not a TTY, so color is off even with NO_COLOR cleared.
	t.Setenv("NO_COLOR", "")
	rd, wr, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer rd.Close()
	defer wr.Close()
	if colorTerminal(wr) {
		t.Error("pipe is not a TTY: want color off")
	}
}

// The rich (TTY) status glyph: green ✓ for 0, bold red ✗ + code for non-zero,
// yellow ○ while running. Failures keep the exit code.
func TestCLIStyles_Glyph(t *testing.T) {
	s := newCLIStyles(true)
	ok := s.glyph(record.Record{ExitCode: ptr(0)})
	fail := s.glyph(record.Record{ExitCode: ptr(130)})
	wait := s.glyph(record.Record{})

	for _, c := range []struct{ tok, want string }{{ok, "✓"}, {fail, "✗ 130"}, {wait, "○"}} {
		if got := ansiPattern.ReplaceAllString(c.tok, ""); got != c.want {
			t.Errorf("glyph text: got %q want %q", got, c.want)
		}
	}
	if !strings.Contains(ok, "32") {
		t.Errorf("✓ should be green: %q", ok)
	}
	if !strings.Contains(fail, "31") || !strings.Contains(fail, "1;") {
		t.Errorf("✗ should be bold red: %q", fail)
	}
	if !strings.Contains(wait, "33") {
		t.Errorf("○ should be yellow: %q", wait)
	}
}

// renderHistoryRich emits a titled, headered, glyph-based layout; stripping ANSI
// must leave aligned columns (padding is by visible width, not byte length).
func TestRenderHistoryRich(t *testing.T) {
	recs := []record.Record{
		{Command: "a", ExitCode: ptr(0), StartTime: histBase},
		{Command: "b", ExitCode: ptr(130), StartTime: histBase.Add(time.Minute)},
		{Command: "c", StartTime: histBase.Add(2 * time.Minute)}, // running -> ○
	}
	var buf bytes.Buffer
	if err := renderHistoryRich(&buf, recs, 1, time.UTC, historyOpts{layout: "2006-01-02 15:04:05", showTime: true, showExit: true}, newCLIStyles(true)); err != nil {
		t.Fatalf("renderHistoryRich: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "\x1b[") {
		t.Fatalf("rich output should contain ANSI escapes:\n%q", out)
	}
	plain := ansiPattern.ReplaceAllString(out, "")
	for _, want := range []string{
		"yas · history",
		"    1  2023-11-14 22:13:20  ✓      a",
		"    2  2023-11-14 22:14:20  ✗ 130  b", // failure keeps its code, column aligned
		"    3  2023-11-14 22:15:20  ○      c",
	} {
		if !strings.Contains(plain, want) {
			t.Errorf("missing %q in rich output:\n%s", want, plain)
		}
	}
	// header row labels present
	if !strings.Contains(plain, "WHEN") || !strings.Contains(plain, "COMMAND") {
		t.Errorf("rich header missing column labels:\n%s", plain)
	}

	// empty history prints nothing (no title/header chrome), like the plain path
	var empty bytes.Buffer
	if err := renderHistoryRich(&empty, nil, 1, time.UTC, historyOpts{showTime: true, showExit: true}, newCLIStyles(true)); err != nil {
		t.Fatalf("renderHistoryRich empty: %v", err)
	}
	if empty.Len() != 0 {
		t.Errorf("empty history should render nothing, got %q", empty.String())
	}
}

func TestRenderSearchRich(t *testing.T) {
	// Mismatched glyph widths (✓ vs "✗ 1") force the glyph column to widen, so the
	// command column only lines up if padding is by visible width, not byte length.
	recs := []record.Record{
		{Command: "git push", ExitCode: ptr(0), StartTime: histBase},
		{Command: "false", ExitCode: ptr(1), StartTime: histBase},
	}
	var buf bytes.Buffer
	if err := renderSearchRich(&buf, recs, newCLIStyles(true), searchOpts{showSession: true, showDuration: true}); err != nil {
		t.Fatalf("renderSearchRich: %v", err)
	}
	if !strings.Contains(buf.String(), "\x1b[") {
		t.Fatalf("rich search output should contain ANSI escapes:\n%q", buf.String())
	}
	plain := ansiPattern.ReplaceAllString(buf.String(), "")
	if !strings.Contains(plain, "yas · search") {
		t.Errorf("missing title:\n%s", plain)
	}
	// exact stripped rows: the ✓ row is padded to match "✗ 1" so commands align;
	// blank SESS column (7 spaces + 2 sep = 9 chars) appears between glyph and command
	for _, want := range []string{
		"2023-11-14 22:13:20Z  ✓             git push",
		"2023-11-14 22:13:20Z  ✗ 1           false",
	} {
		if !strings.Contains(plain, want) {
			t.Errorf("missing/misaligned %q in:\n%s", want, plain)
		}
	}

	// no matches -> nothing printed (no title/header chrome), like the plain path
	var empty bytes.Buffer
	if err := renderSearchRich(&empty, nil, newCLIStyles(true), searchOpts{showSession: true, showDuration: true}); err != nil {
		t.Fatalf("renderSearchRich empty: %v", err)
	}
	if empty.Len() != 0 {
		t.Errorf("empty search should render nothing, got %q", empty.String())
	}
}

// --- yas history ---

var histBase = time.UnixMilli(1_700_000_000_000) // 2023-11-14 22:13:20 UTC

// seedN inserts n records cmd0..cmd(n-1), oldest first (cmd0 earliest), and
// returns their ids in chronological order.
func seedN(t *testing.T, db *sqlitestore.DB, n int) []string {
	t.Helper()
	recs := make([]record.Record, n)
	ids := make([]string, n)
	for i := 0; i < n; i++ {
		ids[i] = fmt.Sprintf("h%03d", i) // zero-padded so id order == time order
		recs[i] = record.Record{
			ID:        ids[i],
			Command:   fmt.Sprintf("cmd%d", i),
			ExitCode:  ptr(i), // cmd0 -> [0], cmd1 -> [1], ...
			StartTime: histBase.Add(time.Duration(i) * time.Minute),
			CreatedAt: histBase,
		}
	}
	if err := db.Put(context.Background(), recs...); err != nil {
		t.Fatalf("seedN: %v", err)
	}
	return ids
}

func TestParseHistoryArgs(t *testing.T) {
	cases := []struct {
		name    string
		args    []string
		wantErr bool
		check   func(o historyOpts) string // "" = ok
	}{
		{"no args lists", nil, false, func(o historyOpts) string {
			if o.mode != histList || o.n != 0 || !o.showTime || !o.showExit {
				return fmt.Sprintf("%+v", o)
			}
			return ""
		}},
		{"no-exit drops result column", []string{"--no-exit"}, false, func(o historyOpts) string {
			if o.mode != histList || o.showExit {
				return fmt.Sprintf("%+v", o)
			}
			return ""
		}},
		{"count", []string{"20"}, false, func(o historyOpts) string {
			if o.mode != histList || o.n != 20 {
				return fmt.Sprintf("%+v", o)
			}
			return ""
		}},
		{"json before count", []string{"--json", "10"}, false, func(o historyOpts) string {
			if o.mode != histList || o.n != 10 || !o.asJSON {
				return fmt.Sprintf("%+v", o)
			}
			return ""
		}},
		{"no-time and time-format", []string{"--no-time", "--time-format", "15:04", "3"}, false, func(o historyOpts) string {
			if o.showTime || o.layout != "15:04" || o.n != 3 {
				return fmt.Sprintf("%+v", o)
			}
			return ""
		}},
		{"delete offset", []string{"-d", "3"}, false, func(o historyOpts) string {
			if o.mode != histDelete || o.delSpec != "3" {
				return fmt.Sprintf("%+v", o)
			}
			return ""
		}},
		{"delete range", []string{"-d", "2-4"}, false, func(o historyOpts) string {
			if o.mode != histDelete || o.delSpec != "2-4" {
				return fmt.Sprintf("%+v", o)
			}
			return ""
		}},
		{"clear needs yes", []string{"-c"}, true, nil},
		{"clear with yes", []string{"-c", "--yes"}, false, func(o historyOpts) string {
			if o.mode != histClear || !o.yes {
				return fmt.Sprintf("%+v", o)
			}
			return ""
		}},
		{"clear with count is invalid", []string{"-c", "--yes", "5"}, true, nil},
		{"clear and delete is invalid", []string{"-c", "--yes", "-d", "1"}, true, nil},
		{"non-numeric count is invalid", []string{"abc"}, true, nil},
		{"two counts invalid", []string{"3", "4"}, true, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			o, err := parseHistoryArgs(c.args)
			if c.wantErr {
				if err == nil {
					t.Fatalf("want error, got opts %+v", o)
				}
				return
			}
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			if c.check != nil {
				if msg := c.check(o); msg != "" {
					t.Errorf("check: %s", msg)
				}
			}
		})
	}
}

func TestPlanDelete(t *testing.T) {
	cases := []struct {
		spec       string
		wantAsc    bool
		wantOffset int
		wantLimit  int
		wantErr    bool
	}{
		{"3", true, 2, 1, false},      // 3rd oldest
		{"2-4", true, 1, 3, false},    // oldest ranks 2..4
		{"1-5", true, 0, 5, false},    // from the oldest
		{"-1", false, 0, 1, false},    // newest
		{"-2", false, 1, 1, false},    // second-newest
		{"-3--1", false, 0, 3, false}, // newest three
		{"0", false, 0, 0, true},      // 1-based; 0 invalid
		{"3-2", false, 0, 0, true},    // start after end
		{"-1--3", false, 0, 0, true},  // start after end (negative)
		{"2--1", false, 0, 0, true},   // mixed-sign range unsupported
		{"0-3", false, 0, 0, true},    // endpoint touches 0
		{"", false, 0, 0, true},
		{"x", false, 0, 0, true},
		{"1-y", false, 0, 0, true},
	}
	for _, c := range cases {
		t.Run(c.spec, func(t *testing.T) {
			p, err := planDelete(c.spec)
			if c.wantErr {
				if err == nil {
					t.Fatalf("want error for %q, got %+v", c.spec, p)
				}
				return
			}
			if err != nil {
				t.Fatalf("planDelete(%q): %v", c.spec, err)
			}
			if p.ascending != c.wantAsc || p.offset != c.wantOffset || p.limit != c.wantLimit {
				t.Errorf("got %+v want {asc:%v offset:%d limit:%d}", p, c.wantAsc, c.wantOffset, c.wantLimit)
			}
		})
	}
}

func TestRenderHistory(t *testing.T) {
	recs := []record.Record{
		{ID: "a", Command: "git status", ExitCode: ptr(0), StartTime: histBase},
		{ID: "b", Command: "ls", ExitCode: ptr(2), StartTime: histBase.Add(time.Minute)},
		{ID: "c", Command: "sleep", StartTime: histBase.Add(2 * time.Minute)}, // unfinished -> [-]
	}
	const layout = "2006-01-02 15:04:05"

	// Defaults: number + time + exit (result) + command.
	var buf bytes.Buffer
	if err := renderHistory(&buf, recs, 7, time.UTC, historyOpts{layout: layout, showTime: true, showExit: true}, newCLIStyles(false)); err != nil {
		t.Fatalf("renderHistory: %v", err)
	}
	out := buf.String()
	for _, w := range []string{
		"    7  2023-11-14 22:13:20  [0]  git status",
		"    8  2023-11-14 22:14:20  [2]  ls",
		"    9  2023-11-14 22:15:20  [-]  sleep", // unfinished command shows [-]
	} {
		if !strings.Contains(out, w) {
			t.Errorf("missing %q in:\n%s", w, out)
		}
	}

	// --no-exit drops the result column; --no-time drops the timestamp.
	buf.Reset()
	renderHistory(&buf, recs, 7, time.UTC, historyOpts{layout: layout, showTime: true, showExit: false}, newCLIStyles(false))
	if got := buf.String(); !strings.Contains(got, "    7  2023-11-14 22:13:20  git status") || strings.Contains(got, "[0]") {
		t.Errorf("--no-exit output wrong:\n%s", got)
	}

	// --no-time but keep the result column.
	buf.Reset()
	renderHistory(&buf, recs, 7, time.UTC, historyOpts{showTime: false, showExit: true}, newCLIStyles(false))
	if got := buf.String(); !strings.Contains(got, "    7  [0]  git status") || strings.Contains(got, "2023-11") {
		t.Errorf("no-time+exit output wrong:\n%s", got)
	}

	// Bare bash look: number + command only.
	buf.Reset()
	renderHistory(&buf, recs, 7, time.UTC, historyOpts{showTime: false, showExit: false}, newCLIStyles(false))
	if got := buf.String(); !strings.Contains(got, "    7  git status") || strings.Contains(got, "[0]") || strings.Contains(got, "2023-11") {
		t.Errorf("bare output wrong:\n%s", got)
	}

	// Mixed exit-code widths: the result token is padded so the command column
	// stays aligned ([0] gets extra spaces to match the width of [128]).
	buf.Reset()
	wide := []record.Record{
		{Command: "a", ExitCode: ptr(0), StartTime: histBase},
		{Command: "b", ExitCode: ptr(128), StartTime: histBase},
	}
	renderHistory(&buf, wide, 1, time.UTC, historyOpts{showTime: false, showExit: true}, newCLIStyles(false))
	const wantAligned = "    1  [0]    a\n    2  [128]  b\n"
	if got := buf.String(); got != wantAligned {
		t.Errorf("aligned columns wrong:\ngot:  %q\nwant: %q", got, wantAligned)
	}

	// With color on, lines carry ANSI escapes but alignment is preserved: the
	// padding is computed from the plain width, not the escaped string length.
	buf.Reset()
	renderHistory(&buf, wide, 1, time.UTC, historyOpts{showTime: false, showExit: true}, newCLIStyles(true))
	colored := buf.String()
	if !strings.Contains(colored, "\x1b[") {
		t.Errorf("colored render should contain ANSI escapes:\n%q", colored)
	}
	// stripping ANSI yields exactly the aligned plain output
	if stripped := ansiPattern.ReplaceAllString(colored, ""); stripped != wantAligned {
		t.Errorf("colored render misaligns once stripped:\ngot:  %q\nwant: %q", stripped, wantAligned)
	}
}

// ansiPattern matches SGR escape sequences for tests that assert visible layout.
var ansiPattern = regexp.MustCompile("\x1b\\[[0-9;]*m")

// Listing is oldest-first with absolute numbers; newest sits at the bottom.
func TestDoHistory_ListNumbering(t *testing.T) {
	db := openTestStore(t)
	seedN(t, db, 5)
	ctx := context.Background()

	var buf bytes.Buffer
	if _, err := doHistory(ctx, db, historyOpts{mode: histList, showTime: false}, &buf, time.UTC); err != nil {
		t.Fatalf("doHistory: %v", err)
	}
	out := buf.String()
	for _, w := range []string{"    1  cmd0", "    2  cmd1", "    3  cmd2", "    4  cmd3", "    5  cmd4"} {
		if !strings.Contains(out, w) {
			t.Errorf("missing %q in:\n%s", w, out)
		}
	}
	if strings.Index(out, "cmd0") > strings.Index(out, "cmd4") {
		t.Errorf("want oldest-first (cmd0 before cmd4):\n%s", out)
	}
}

// `history n` shows only the last n entries, keeping their absolute numbers.
func TestDoHistory_ListLastN(t *testing.T) {
	db := openTestStore(t)
	seedN(t, db, 5)
	ctx := context.Background()

	var buf bytes.Buffer
	if _, err := doHistory(ctx, db, historyOpts{mode: histList, n: 2, showTime: false}, &buf, time.UTC); err != nil {
		t.Fatalf("doHistory: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "    4  cmd3") || !strings.Contains(out, "    5  cmd4") {
		t.Errorf("want last two numbered 4,5:\n%s", out)
	}
	if strings.Contains(out, "cmd2") {
		t.Errorf("older entries must not appear:\n%s", out)
	}
}

// The default listing surfaces each command's result (exit code).
func TestDoHistory_ListShowsExit(t *testing.T) {
	db := openTestStore(t)
	seedN(t, db, 3) // cmd0 -> [0], cmd1 -> [1], cmd2 -> [2]
	ctx := context.Background()

	var buf bytes.Buffer
	if _, err := doHistory(ctx, db, historyOpts{mode: histList, showTime: false, showExit: true}, &buf, time.UTC); err != nil {
		t.Fatalf("doHistory: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "    1  [0]  cmd0") || !strings.Contains(out, "    3  [2]  cmd2") {
		t.Errorf("exit column missing from listing:\n%s", out)
	}
	// default opts.color is false -> no ANSI
	if strings.Contains(out, "\x1b[") {
		t.Errorf("listing must be plain when color is off:\n%q", out)
	}
}

// opts.color threads color through the list path to the renderer.
func TestDoHistory_ListColored(t *testing.T) {
	db := openTestStore(t)
	seedN(t, db, 3)
	var buf bytes.Buffer
	if _, err := doHistory(context.Background(), db, historyOpts{mode: histList, showTime: false, showExit: true, color: true}, &buf, time.UTC); err != nil {
		t.Fatalf("doHistory: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "\x1b[") {
		t.Errorf("colored listing should contain ANSI escapes:\n%q", out)
	}
	if !strings.Contains(out, "yas · history") {
		t.Errorf("colored history should use the rich (titled) layout:\n%s", out)
	}
}

func TestDoHistory_ListJSON(t *testing.T) {
	db := openTestStore(t)
	seedN(t, db, 3)
	ctx := context.Background()

	var buf bytes.Buffer
	if _, err := doHistory(ctx, db, historyOpts{mode: histList, asJSON: true}, &buf, time.UTC); err != nil {
		t.Fatalf("doHistory json: %v", err)
	}
	var resp queryapi.SearchResponse
	if err := json.Unmarshal(buf.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v\n%s", err, buf.String())
	}
	// oldest-first, matching the human listing order
	if len(resp.Records) != 3 || resp.Records[0].Command != "cmd0" || resp.Records[2].Command != "cmd2" {
		t.Fatalf("want [cmd0..cmd2] oldest-first, got %+v", resp.Records)
	}
}

// -d offset tombstones exactly the addressed entry; the tombstone is left
// unsynced so the deletion propagates on the next sync.
func TestDoHistory_DeleteOffset(t *testing.T) {
	db := openTestStore(t)
	ids := seedN(t, db, 5)
	ctx := context.Background()

	var buf bytes.Buffer
	n, err := doHistory(ctx, db, historyOpts{mode: histDelete, delSpec: "2"}, &buf, time.UTC)
	if err != nil {
		t.Fatalf("doHistory delete: %v", err)
	}
	if n != 1 {
		t.Fatalf("deleted %d want 1", n)
	}
	recs, _ := db.Search(ctx, store.Query{})
	if len(recs) != 4 {
		t.Fatalf("want 4 live records, got %d", len(recs))
	}
	for _, r := range recs {
		if r.Command == "cmd1" {
			t.Errorf("cmd1 (offset 2) should be deleted")
		}
	}
	// the tombstone is queued for sync
	un, _ := db.Unsynced(ctx, 100)
	var gotTombstone bool
	for _, r := range un {
		if r.ID == ids[1] && r.Deleted {
			gotTombstone = true
		}
	}
	if !gotTombstone {
		t.Errorf("deleted record %s should be an unsynced tombstone", ids[1])
	}
}

func TestDoHistory_DeleteRange(t *testing.T) {
	db := openTestStore(t)
	seedN(t, db, 5)
	ctx := context.Background()

	n, err := doHistory(ctx, db, historyOpts{mode: histDelete, delSpec: "2-3"}, &bytes.Buffer{}, time.UTC)
	if err != nil {
		t.Fatalf("doHistory delete range: %v", err)
	}
	if n != 2 {
		t.Fatalf("deleted %d want 2", n)
	}
	recs, _ := db.Search(ctx, store.Query{})
	for _, r := range recs {
		if r.Command == "cmd1" || r.Command == "cmd2" {
			t.Errorf("offsets 2-3 (cmd1,cmd2) should be deleted, found %q", r.Command)
		}
	}
}

func TestDoHistory_DeleteNegative(t *testing.T) {
	db := openTestStore(t)
	seedN(t, db, 5)
	ctx := context.Background()

	n, err := doHistory(ctx, db, historyOpts{mode: histDelete, delSpec: "-1"}, &bytes.Buffer{}, time.UTC)
	if err != nil {
		t.Fatalf("doHistory delete -1: %v", err)
	}
	if n != 1 {
		t.Fatalf("deleted %d want 1", n)
	}
	recs, _ := db.Search(ctx, store.Query{})
	for _, r := range recs {
		if r.Command == "cmd4" {
			t.Errorf("offset -1 (newest, cmd4) should be deleted")
		}
	}
}

func TestDoHistory_DeleteOutOfRange(t *testing.T) {
	db := openTestStore(t)
	seedN(t, db, 3)
	ctx := context.Background()
	for _, spec := range []string{"99", "-9"} {
		if _, err := doHistory(ctx, db, historyOpts{mode: histDelete, delSpec: spec}, &bytes.Buffer{}, time.UTC); err == nil {
			t.Errorf("want error deleting out-of-range offset %q", spec)
		}
	}
	// nothing was tombstoned by the failed deletes
	if c, _ := db.Count(ctx, store.Query{}); c != 3 {
		t.Fatalf("count after failed deletes: got %d want 3", c)
	}
}

func TestDoHistory_Clear(t *testing.T) {
	db := openTestStore(t)
	seedN(t, db, 5)
	ctx := context.Background()

	n, err := doHistory(ctx, db, historyOpts{mode: histClear, yes: true}, &bytes.Buffer{}, time.UTC)
	if err != nil {
		t.Fatalf("doHistory clear: %v", err)
	}
	if n != 5 {
		t.Fatalf("cleared %d want 5", n)
	}
	if c, _ := db.Count(ctx, store.Query{}); c != 0 {
		t.Fatalf("count after clear: got %d want 0", c)
	}
	// every cleared record is an unsynced tombstone so the wipe propagates
	un, _ := db.Unsynced(ctx, 100)
	if len(un) != 5 {
		t.Fatalf("want 5 unsynced tombstones, got %d", len(un))
	}
	for _, r := range un {
		if !r.Deleted {
			t.Errorf("record %s should be a tombstone after clear", r.ID)
		}
	}
}

// newSyncServer starts an in-memory sync hub over httptest and returns the
// backend (for direct inspection) plus a client pointed at it.
func newSyncServer(t *testing.T) (*memBackend, *syncclient.Client) {
	t.Helper()
	mem := newMemBackend()
	srv := httptest.NewServer(syncapi.NewHandler(mem, "tok"))
	t.Cleanup(srv.Close)
	return mem, syncclient.New(srv.URL, "tok")
}

// syncAll runs one full push+pull round and fails the test on error.
func syncAll(t *testing.T, local syncLocal, remote syncRemote) {
	t.Helper()
	if _, _, err := doSync(context.Background(), local, remote, syncBatch); err != nil {
		t.Fatalf("doSync: %v", err)
	}
}

// Two replicas have both synced the same history. Replica A deletes an entry
// with `yas history -d`; after A pushes the tombstone and B pulls, the entry is
// gone from B too. This is the high-blast-radius guarantee from bm9t: a local
// delete reaches every replica via tombstone + LWW sync, overwriting B's
// previously-live copy.
func TestDoHistory_DeletePropagatesAcrossReplicas(t *testing.T) {
	ctx := context.Background()
	_, client := newSyncServer(t)
	a, b := openTestStore(t), openTestStore(t)

	seedN(t, a, 3)        // h000=cmd0, h001=cmd1, h002=cmd2 on replica A
	syncAll(t, a, client) // push A's records to the hub
	syncAll(t, b, client) // B pulls them — both replicas now hold all three live
	if c, _ := b.Count(ctx, store.Query{}); c != 3 {
		t.Fatalf("replica B should hold 3 records after initial sync, got %d", c)
	}

	// A deletes offset 2 (cmd1 / h001), then both replicas sync.
	if n, err := doHistory(ctx, a, historyOpts{mode: histDelete, delSpec: "2"}, &bytes.Buffer{}, time.UTC); err != nil || n != 1 {
		t.Fatalf("doHistory delete: n=%d err=%v", n, err)
	}
	syncAll(t, a, client) // push the tombstone to the hub
	syncAll(t, b, client) // B pulls the tombstone

	live := idsOfRecs(mustSearch(t, b))
	if contains(live, "h001") {
		t.Errorf("tombstoned h001 must not survive on replica B: %v", live)
	}
	if !contains(live, "h000") || !contains(live, "h002") {
		t.Errorf("untouched records must remain on replica B: %v", live)
	}
	if c, _ := b.Count(ctx, store.Query{}); c != 2 {
		t.Errorf("replica B count after propagated delete: got %d want 2", c)
	}
}

// `yas history -c --yes` on one replica must wipe history on every other replica
// too: the clear tombstones the whole store and those tombstones propagate. This
// is the literal "nuke everywhere" path bm9t flags.
func TestDoHistory_ClearPropagatesAcrossReplicas(t *testing.T) {
	ctx := context.Background()
	_, client := newSyncServer(t)
	a, b := openTestStore(t), openTestStore(t)

	seedN(t, a, 4)
	syncAll(t, a, client)
	syncAll(t, b, client)
	if c, _ := b.Count(ctx, store.Query{}); c != 4 {
		t.Fatalf("replica B should hold 4 records after initial sync, got %d", c)
	}

	// A clears its entire history, then both replicas sync.
	n, err := doHistory(ctx, a, historyOpts{mode: histClear, yes: true}, &bytes.Buffer{}, time.UTC)
	if err != nil || n != 4 {
		t.Fatalf("doHistory clear: n=%d err=%v", n, err)
	}
	syncAll(t, a, client)
	syncAll(t, b, client)

	if c, _ := b.Count(ctx, store.Query{}); c != 0 {
		t.Errorf("replica B should be wiped after propagated clear, got %d live records", c)
	}
	if live := mustSearch(t, b); len(live) != 0 {
		t.Errorf("replica B search should be empty after clear, got %v", idsOfRecs(live))
	}
}

// A tombstone is sticky: once a record is deleted, a later live update (e.g. a
// delayed record-finish racing the delete) cannot resurrect it. `deleted` is
// monotonic in every store, so delete wins regardless of write order. This is
// the fix for a38z — without it, B's later deleted=0 write would resurrect X
// everywhere via unconditional LWW.
func TestDoHistory_TombstoneStickyAgainstLaterLiveUpdate(t *testing.T) {
	ctx := context.Background()
	mem, client := newSyncServer(t)
	a, b := openTestStore(t), openTestStore(t)

	// X is live on the hub and on both replicas.
	x := record.Record{ID: "x", Command: "rm -rf /tmp/scratch", StartTime: histBase, CreatedAt: histBase}
	if err := a.Put(ctx, x); err != nil {
		t.Fatalf("seed: %v", err)
	}
	syncAll(t, a, client)
	syncAll(t, b, client)

	// A deletes X and pushes the tombstone to the hub.
	if n, err := doHistory(ctx, a, historyOpts{mode: histDelete, delSpec: "1"}, &bytes.Buffer{}, time.UTC); err != nil || n != 1 {
		t.Fatalf("delete: n=%d err=%v", n, err)
	}
	syncAll(t, a, client)
	if !mem.recs["x"].Deleted {
		t.Fatalf("hub should hold X tombstoned after A's push")
	}

	// B has not pulled the tombstone yet. It re-touches X with a live update
	// (a delayed record-finish: exit code + duration) and syncs. doSync pushes
	// before it pulls, so B's deleted=0 write reaches the hub AFTER the tombstone
	// — the exact race that used to resurrect X.
	live := x
	live.ExitCode, live.DurationMS = ptr(0), ptr(int64(12))
	if err := b.Put(ctx, live); err != nil { // deleted stays false; row re-marked unsynced
		t.Fatalf("b live update: %v", err)
	}
	syncAll(t, b, client)

	// Sticky delete: the tombstone survives B's later live write everywhere.
	if !mem.recs["x"].Deleted {
		t.Errorf("hub X must stay tombstoned despite B's later live write (sticky delete)")
	}
	if contains(idsOfRecs(mustSearch(t, b)), "x") {
		t.Errorf("X must not be live on B after it pulls back the sticky tombstone")
	}
	syncAll(t, a, client)
	if contains(idsOfRecs(mustSearch(t, a)), "x") {
		t.Errorf("X must stay deleted on A — no resurrection")
	}
}

// The in-flight `yas history` command (its own record) is omitted from its own
// output, and the absolute numbering ignores it.
func TestDoHistory_ExcludesSelf(t *testing.T) {
	db := openTestStore(t)
	ids := seedN(t, db, 4) // cmd0..cmd3; pretend cmd3 is the running `yas history`
	ctx := context.Background()

	var buf bytes.Buffer
	if _, err := doHistory(ctx, db, historyOpts{mode: histList, showTime: false, excludeID: ids[3]}, &buf, time.UTC); err != nil {
		t.Fatalf("doHistory: %v", err)
	}
	out := buf.String()
	if strings.Contains(out, "cmd3") {
		t.Errorf("the in-flight self (cmd3) must be omitted:\n%s", out)
	}
	for _, w := range []string{"    1  cmd0", "    2  cmd1", "    3  cmd2"} {
		if !strings.Contains(out, w) {
			t.Errorf("missing %q (numbering should ignore self):\n%s", w, out)
		}
	}
}

// `yas history -d -1` must delete the newest *other* command, not the in-flight
// delete command itself.
func TestDoHistory_DeleteExcludesSelf(t *testing.T) {
	db := openTestStore(t)
	ids := seedN(t, db, 4) // cmd0..cmd3; cmd3 is the in-flight `yas history -d -1`
	ctx := context.Background()

	n, err := doHistory(ctx, db, historyOpts{mode: histDelete, delSpec: "-1", excludeID: ids[3]}, &bytes.Buffer{}, time.UTC)
	if err != nil {
		t.Fatalf("doHistory delete: %v", err)
	}
	if n != 1 {
		t.Fatalf("deleted %d want 1", n)
	}
	live := idsOfRecs(mustSearch(t, db))
	if contains(live, ids[2]) {
		t.Errorf("newest non-self (cmd2 / %s) should be deleted", ids[2])
	}
	if !contains(live, ids[3]) {
		t.Errorf("the in-flight self (cmd3 / %s) must not be deleted", ids[3])
	}
}

func mustSearch(t *testing.T, db *sqlitestore.DB) []record.Record {
	t.Helper()
	recs, err := db.Search(context.Background(), store.Query{})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	return recs
}

func contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

// `yas search` omits the in-flight search command from its own results.
func TestDoSearch_ExcludesSelf(t *testing.T) {
	db := openTestStore(t)
	seedTwo(t, db) // id "a" = "git status", id "b" = "ls"
	var buf bytes.Buffer
	if err := doSearch(context.Background(), db, store.Query{ExcludeID: "a"}, &buf, searchOpts{asJSON: false, showSession: true, showDuration: true}, newCLIStyles(false)); err != nil {
		t.Fatalf("doSearch: %v", err)
	}
	out := buf.String()
	if strings.Contains(out, "git status") {
		t.Errorf("excluded record (id a) should not appear:\n%s", out)
	}
	if !strings.Contains(out, "ls") {
		t.Errorf("non-excluded record (ls) should appear:\n%s", out)
	}
}

func TestRecordStart_Executor(t *testing.T) {
	db := openTestStore(t)
	ctx := context.Background()
	id, err := doRecordStart(ctx, db, time.UnixMilli(1_700_000_000_000), record.Record{Command: "deploy", Executor: "claude-code"})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	recs, _ := db.Search(ctx, store.Query{ID: id})
	if len(recs) != 1 || recs[0].Executor != "claude-code" {
		t.Fatalf("executor not persisted: %+v", recs)
	}
}

// TestRecordStart_CorrIDPrecedence drives the real `record start` CLI path
// (flag parsing + os.Getenv default) end to end: --corr-id and $YAS_CORR_ID
// both feed record.Record.CorrID, with an explicit flag winning over the env
// default — the M10 cross-tool correlation seam.
func TestRecordStart_CorrIDPrecedence(t *testing.T) {
	for _, tc := range []struct {
		name    string
		flagVal string // "" = --corr-id omitted
		envVal  string // "" = YAS_CORR_ID unset
		want    string
	}{
		{"flag only", "flag-x", "", "flag-x"},
		{"env only", "", "env-y", "env-y"},
		{"flag wins over env", "flag-x", "env-y", "flag-x"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dataDir := t.TempDir()
			t.Setenv("YAS_DATA_DIR", dataDir)
			t.Setenv("YAS_CORR_ID", tc.envVal)

			args := []string{"start", "--command", "echo hi", "--cwd", dataDir, "--session", "s1", "--shell", "zsh"}
			if tc.flagVal != "" {
				args = append(args, "--corr-id", tc.flagVal)
			}
			id := runRecordStartCLI(t, args)

			recs := searchRecordedID(t, dataDir, id)
			if recs[0].CorrID != tc.want {
				t.Errorf("CorrID = %q, want %q", recs[0].CorrID, tc.want)
			}
		})
	}
}

// TestRecordStart_CorrIDOmittedWhenUnset is the fourth precedence case: with
// neither --corr-id nor $YAS_CORR_ID set, CorrID stays empty and (per the
// record package's omitempty contract) corr_id never appears in the record's
// marshaled JSON — old/unset records simply lack the key.
func TestRecordStart_CorrIDOmittedWhenUnset(t *testing.T) {
	dataDir := t.TempDir()
	t.Setenv("YAS_DATA_DIR", dataDir)
	t.Setenv("YAS_CORR_ID", "")

	id := runRecordStartCLI(t, []string{"start", "--command", "echo hi", "--cwd", dataDir, "--session", "s1", "--shell", "zsh"})

	recs := searchRecordedID(t, dataDir, id)
	if recs[0].CorrID != "" {
		t.Fatalf("CorrID = %q, want empty", recs[0].CorrID)
	}
	b, err := json.Marshal(recs[0])
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(b), "corr_id") {
		t.Errorf("corr_id must be omitted when unset: %s", b)
	}
}

// searchRecordedID opens the SQLite replica under dataDir (matching
// config.Client.DBPath's "history.db" layout) and fetches the single record
// with the given id.
func searchRecordedID(t *testing.T, dataDir, id string) []record.Record {
	t.Helper()
	db, err := sqlitestore.Open(filepath.Join(dataDir, "history.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer db.Close()
	recs, err := db.Search(context.Background(), store.Query{ID: id})
	if err != nil || len(recs) != 1 {
		t.Fatalf("search %q: recs=%v err=%v", id, recs, err)
	}
	return recs
}

// runRecordStartCLI invokes the real `record start` flag parsing + store path
// via cmdRecord and returns the printed record id. It only exercises the
// success path (valid flags, a writable temp $YAS_DATA_DIR), so cmdRecord's
// os.Exit branches are never reached.
func runRecordStartCLI(t *testing.T, args []string) string {
	t.Helper()
	rd, wr, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	origStdout := os.Stdout
	os.Stdout = wr
	cmdRecord(args)
	os.Stdout = origStdout
	if err := wr.Close(); err != nil {
		t.Fatalf("close pipe writer: %v", err)
	}
	out, err := io.ReadAll(rd)
	if err != nil {
		t.Fatalf("read pipe: %v", err)
	}
	return strings.TrimSpace(string(out))
}

func TestParseSearchArgs_Executor(t *testing.T) {
	for _, c := range []struct {
		arg   string
		check func(store.Query) bool
	}{
		{"$all-agent", func(q store.Query) bool { return q.AgentsOnly && !q.HumansOnly && q.Executor == "" }},
		{"$all-human", func(q store.Query) bool { return q.HumansOnly && !q.AgentsOnly && q.Executor == "" }},
		{"human", func(q store.Query) bool { return q.HumansOnly && !q.AgentsOnly && q.Executor == "" }},
		{"codex", func(q store.Query) bool { return q.Executor == "codex" && !q.AgentsOnly && !q.HumansOnly }},
	} {
		q, _, err := parseSearchArgs([]string{"--executor", c.arg})
		if err != nil {
			t.Fatalf("parse %q: %v", c.arg, err)
		}
		if !c.check(q) {
			t.Errorf("--executor %q: wrong query %+v", c.arg, q)
		}
	}
}

// The SESS token column appears in history output and is blank for sessionless rows.
func TestRenderHistory_SessionColumn(t *testing.T) {
	recs := []record.Record{
		{ID: "a", Command: "docker ps", Session: "hostZ-1-2", StartTime: histBase, CreatedAt: histBase},
		{ID: "b", Command: "git log", Session: "", StartTime: histBase, CreatedAt: histBase},
	}
	var on bytes.Buffer
	if err := renderHistory(&on, recs, 1, time.UTC,
		historyOpts{showTime: false, showExit: false, showSession: true}, newCLIStyles(false)); err != nil {
		t.Fatalf("renderHistory: %v", err)
	}
	if !strings.Contains(on.String(), shortSession("hostZ-1-2")) {
		t.Errorf("session token missing:\n%s", on.String())
	}
	// The sessionless row ("git log", Session="") must render a width-stable
	// blank cell (7 spaces), not collapse the column, so the command stays
	// aligned with the tokened rows above it.
	if !strings.Contains(on.String(), "git log") || !strings.Contains(on.String(), "       "+"  git log") {
		t.Errorf("blank session must render as a 7-space cell before the command:\n%q", on.String())
	}

	// --no-session path: no token column.
	var off bytes.Buffer
	renderHistory(&off, recs, 1, time.UTC,
		historyOpts{showTime: false, showExit: false, showSession: false}, newCLIStyles(false))
	if strings.Contains(off.String(), shortSession("hostZ-1-2")) {
		t.Errorf("session token must be hidden with showSession=false:\n%s", off.String())
	}
}

// --no-session is parsed for both search and history.
func TestParseArgs_NoSession(t *testing.T) {
	if _, _, err := parseSearchArgs([]string{"--no-session", "foo"}); err != nil {
		t.Fatalf("search --no-session: %v", err)
	}
	o, err := parseHistoryArgs([]string{"--no-session"})
	if err != nil {
		t.Fatalf("history --no-session: %v", err)
	}
	if o.showSession {
		t.Errorf("--no-session should set showSession=false")
	}
}

func TestRoute(t *testing.T) {
	tests := []struct {
		name     string
		args     []string
		wantCmd  string
		wantRest []string
	}{
		{"no args defaults to history", nil, "history", nil},
		{"empty slice defaults to history", []string{}, "history", nil},
		{"bare count", []string{"20"}, "history", []string{"20"}},
		{"zero count", []string{"0"}, "history", []string{"0"}},
		{"count then flag", []string{"20", "--json"}, "history", []string{"20", "--json"}},
		{"leading flag", []string{"--json"}, "history", []string{"--json"}},
		{"delete flag and operand", []string{"-d", "3"}, "history", []string{"-d", "3"}},
		{"explicit history passes through", []string{"history", "20"}, "history", []string{"20"}},
		{"search", []string{"search", "foo"}, "search", []string{"foo"}},
		{"session", []string{"session", "tok"}, "session", []string{"tok"}},
		{"record start", []string{"record", "start"}, "record", []string{"start"}},
		{"serve", []string{"serve"}, "serve", nil},
		{"sync", []string{"sync"}, "sync", nil},
		{"import", []string{"import", "--from", "zsh-history"}, "import", []string{"--from", "zsh-history"}},
		{"mcp", []string{"mcp"}, "mcp", nil},
		{"completion", []string{"completion", "zsh"}, "completion", []string{"zsh"}},
		{"version word", []string{"version"}, "version", nil},
		{"version long flag", []string{"--version"}, "version", nil},
		{"version short flag", []string{"-v"}, "version", nil},
		{"help word", []string{"help"}, "help", nil},
		{"help short flag", []string{"-h"}, "help", nil},
		{"help long flag", []string{"--help"}, "help", nil},
		{"unknown word", []string{"serch"}, "unknown", []string{"serch"}},
		{"unknown word with args", []string{"serch", "git"}, "unknown", []string{"serch", "git"}},
		// Guard the isFlagLike boundary: a lone "-" is not flag-like, so it is
		// not a history shortcut — it stays an unknown command.
		{"lone dash is unknown", []string{"-"}, "unknown", []string{"-"}},
		// A negative integer is flag-like (rule 5), not a count (rule 4): it
		// routes to history, whose flag parser then rejects it — `yas -3` does
		// NOT mean "last 3".
		{"negative integer is flag-like", []string{"-3"}, "history", []string{"-3"}},
		// An out-of-int-range numeric token is neither a valid count nor a
		// flag, so it is an unknown command (accepted edge: the message is
		// "unknown command", not "invalid count").
		{"overflow int is unknown", []string{"99999999999999999999999"}, "unknown", []string{"99999999999999999999999"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cmd, rest := route(tt.args)
			if cmd != tt.wantCmd {
				t.Errorf("route(%q) cmd = %q, want %q", tt.args, cmd, tt.wantCmd)
			}
			if !slices.Equal(rest, tt.wantRest) {
				t.Errorf("route(%q) rest = %#v, want %#v", tt.args, rest, tt.wantRest)
			}
		})
	}
}

func TestDurationField(t *testing.T) {
	ms := func(n int64) *int64 { return &n }
	cases := []struct {
		name string
		in   *int64
		want string
	}{
		{"nil (unfinished or imported)", nil, ""},
		{"zero", ms(0), "0ms"},
		{"millis", ms(85), "85ms"},
		{"seconds one decimal", ms(1234), "1.2s"},
		{"just under a minute", ms(59949), "59.9s"},
		{"rounds up to a minute", ms(59950), "1m00s"},
		{"minutes", ms(220000), "3m40s"},
		{"minutes pads seconds", ms(180000), "3m00s"},
		{"hours", ms(3720000), "1h02m"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := durationField(record.Record{DurationMS: tc.in})
			if got != tc.want {
				t.Fatalf("durationField(%v) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestHistoryDurationColumn(t *testing.T) {
	ms := func(n int64) *int64 { return &n }
	exit0 := 0
	recs := []record.Record{
		{Command: "sleep 2", StartTime: time.Date(2026, 7, 7, 10, 0, 0, 0, time.UTC),
			ExitCode: &exit0, DurationMS: ms(2100), Session: "abc123def456"},
		{Command: "true", StartTime: time.Date(2026, 7, 7, 10, 0, 5, 0, time.UTC),
			ExitCode: &exit0, DurationMS: nil, Session: "abc123def456"},
	}
	styles := newCLIStyles(false)

	t.Run("shown by default, blank when nil", func(t *testing.T) {
		var b strings.Builder
		opts := historyOpts{layout: defaultHistTimeLayout, showTime: true,
			showExit: true, showSession: true, showDuration: true}
		if err := renderHistory(&b, recs, 1, time.UTC, opts, styles); err != nil {
			t.Fatal(err)
		}
		out := b.String()
		if !strings.Contains(out, "2.1s") {
			t.Fatalf("expected duration 2.1s in output:\n%s", out)
		}
		// The nil-duration row still aligns: the command column follows padding.
		if !strings.Contains(out, "true") {
			t.Fatalf("expected nil-duration row rendered:\n%s", out)
		}
	})

	t.Run("--no-duration parses", func(t *testing.T) {
		opts, err := parseHistoryArgs([]string{"--no-duration"})
		if err != nil {
			t.Fatal(err)
		}
		if opts.showDuration {
			t.Fatal("expected showDuration=false with --no-duration")
		}
	})

	t.Run("omitted when disabled", func(t *testing.T) {
		var b strings.Builder
		opts := historyOpts{layout: defaultHistTimeLayout, showTime: true,
			showExit: true, showSession: true, showDuration: false}
		if err := renderHistory(&b, recs, 1, time.UTC, opts, styles); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(b.String(), "2.1s") {
			t.Fatalf("expected no duration column:\n%s", b.String())
		}
	})
}

func TestSearchDurationColumn(t *testing.T) {
	t.Run("--no-duration parses", func(t *testing.T) {
		_, opts, err := parseSearchArgs([]string{"--no-duration", "git"})
		if err != nil {
			t.Fatal(err)
		}
		if opts.showDuration {
			t.Fatal("expected showDuration=false with --no-duration")
		}
	})
	t.Run("default shows duration", func(t *testing.T) {
		_, opts, err := parseSearchArgs([]string{"git"})
		if err != nil {
			t.Fatal(err)
		}
		if !opts.showDuration {
			t.Fatal("expected showDuration=true by default")
		}
	})
}
