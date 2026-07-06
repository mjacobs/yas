package sqlite_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/mjacobs/yas/internal/record"
	"github.com/mjacobs/yas/internal/store"
	sqlitestore "github.com/mjacobs/yas/internal/store/sqlite"
	_ "modernc.org/sqlite"
)

// openTestDB opens a fresh file-backed store in a temp dir (WAL needs a real
// file) and closes it on cleanup.
func openTestDB(t *testing.T) *sqlitestore.DB {
	t.Helper()
	db, err := sqlitestore.Open(filepath.Join(t.TempDir(), "history.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func ptr[T any](v T) *T { return &v }

// IncludeDeleted surfaces tombstoned rows to internal callers (the importer
// must see deletions to honor them); the default query path never does.
func TestSearch_IncludeDeleted(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	rec := record.Record{ID: "019ef273-4ad8-76d8-aaaa-00000000000d", Command: "rm secret",
		StartTime: base, CreatedAt: base}
	if err := db.Put(ctx, rec); err != nil {
		t.Fatalf("Put: %v", err)
	}
	rec.Deleted = true
	if err := db.Put(ctx, rec); err != nil {
		t.Fatalf("tombstone Put: %v", err)
	}

	got, err := db.Search(ctx, store.Query{})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("default Search returned %d rows, want 0 (tombstones hidden)", len(got))
	}

	got, err = db.Search(ctx, store.Query{IncludeDeleted: true})
	if err != nil {
		t.Fatalf("Search IncludeDeleted: %v", err)
	}
	if len(got) != 1 || !got[0].Deleted || got[0].ID != rec.ID {
		t.Fatalf("IncludeDeleted got %+v, want the single tombstoned row", got)
	}
}

// fixed base time so storage round-trips are deterministic (storage is unix-ms).
var base = time.UnixMilli(1_700_000_000_000)

func TestPutSearch_RoundTrip(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	want := record.Record{
		ID:         "019ef273-4ad8-76d8-aaaa-000000000001",
		Command:    "git status",
		CWD:        "/work/user/dev",
		Hostname:   "host1",
		Session:    "sess-1",
		Shell:      "zsh",
		Username:   "user",
		ExitCode:   ptr(0),
		StartTime:  base,
		DurationMS: ptr(int64(42)),
		CreatedAt:  base,
		RepoRoot:   "/work/user",
		Branch:     "feature/xvt6",
	}
	if err := db.Put(ctx, want); err != nil {
		t.Fatalf("Put: %v", err)
	}

	got, err := db.Search(ctx, store.Query{})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("want 1 record, got %d", len(got))
	}
	g := got[0]
	if g.ID != want.ID || g.Command != want.Command || g.CWD != want.CWD ||
		g.Hostname != want.Hostname || g.Session != want.Session ||
		g.Shell != want.Shell || g.Username != want.Username ||
		g.RepoRoot != want.RepoRoot || g.Branch != want.Branch {
		t.Errorf("string fields mismatch:\n got %+v\nwant %+v", g, want)
	}
	if g.ExitCode == nil || *g.ExitCode != 0 {
		t.Errorf("exit_code: got %v want 0", g.ExitCode)
	}
	if g.DurationMS == nil || *g.DurationMS != 42 {
		t.Errorf("duration_ms: got %v want 42", g.DurationMS)
	}
	if !g.StartTime.Equal(base) {
		t.Errorf("start_time: got %v want %v", g.StartTime, base)
	}
	if !g.CreatedAt.Equal(base) {
		t.Errorf("created_at: got %v want %v", g.CreatedAt, base)
	}
}

// A second Put of the same id (the record-finish path) overwrites the mutable
// fields but must NOT change the immutable command — and must not create a dup.
func TestPut_UpsertLastWriterWinsOnlyMutable(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	start := record.Record{
		ID: "id-upsert", Command: "sleep 1", StartTime: base, CreatedAt: base,
		Executor: "original-agent", CorrID: "orig-corr",
	}
	if err := db.Put(ctx, start); err != nil {
		t.Fatalf("Put start: %v", err)
	}
	// finish: same id, exit+duration now known. Stray different command/executor/corr_id
	// values here must be ignored (those fields are immutable).
	finish := record.Record{
		ID: "id-upsert", Command: "TAMPERED", StartTime: base, CreatedAt: base,
		ExitCode: ptr(7), DurationMS: ptr(int64(1234)),
		Executor: "TAMPERED-agent", CorrID: "TAMPERED-corr",
	}
	if err := db.Put(ctx, finish); err != nil {
		t.Fatalf("Put finish: %v", err)
	}

	got, err := db.Search(ctx, store.Query{})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("upsert must not duplicate: got %d rows", len(got))
	}
	g := got[0]
	if g.Command != "sleep 1" {
		t.Errorf("command must be immutable: got %q want %q", g.Command, "sleep 1")
	}
	if g.ExitCode == nil || *g.ExitCode != 7 {
		t.Errorf("exit_code: got %v want 7", g.ExitCode)
	}
	if g.DurationMS == nil || *g.DurationMS != 1234 {
		t.Errorf("duration_ms: got %v want 1234", g.DurationMS)
	}
	if g.Executor != "original-agent" {
		t.Errorf("executor must be immutable: got %q want %q", g.Executor, "original-agent")
	}
	if g.CorrID != "orig-corr" {
		t.Errorf("corr_id must be immutable: got %q want %q", g.CorrID, "orig-corr")
	}
}

// Deleted records are tombstones and must not surface in Search results.
func TestSearch_ExcludesDeleted(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	live := record.Record{ID: "live", Command: "echo hi", StartTime: base, CreatedAt: base}
	dead := record.Record{ID: "dead", Command: "secret", StartTime: base, CreatedAt: base, Deleted: true}
	if err := db.Put(ctx, live, dead); err != nil {
		t.Fatalf("Put: %v", err)
	}

	got, err := db.Search(ctx, store.Query{})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(got) != 1 || got[0].ID != "live" {
		t.Fatalf("want only the live record, got %+v", got)
	}
}

// seedCorpus inserts four distinct records and returns the store.
func seedCorpus(t *testing.T) (*sqlitestore.DB, context.Context) {
	t.Helper()
	db := openTestDB(t)
	ctx := context.Background()
	min := time.Minute
	recs := []record.Record{
		{ID: "r1", Command: "git status", Hostname: "hostA", Session: "s1", ExitCode: ptr(0), StartTime: base, CreatedAt: base},
		{ID: "r2", Command: "git commit -m wip", Hostname: "hostA", Session: "s2", ExitCode: ptr(1), StartTime: base.Add(min), CreatedAt: base},
		{ID: "r3", Command: "docker ps", Hostname: "hostB", Session: "s1", ExitCode: ptr(0), StartTime: base.Add(2 * min), CreatedAt: base},
		{ID: "r4", Command: "ls -la", Hostname: "hostB", Session: "s2", ExitCode: ptr(0), StartTime: base.Add(3 * min), CreatedAt: base},
	}
	if err := db.Put(ctx, recs...); err != nil {
		t.Fatalf("seed Put: %v", err)
	}
	return db, ctx
}

func ids(recs []record.Record) map[string]bool {
	s := make(map[string]bool, len(recs))
	for _, r := range recs {
		s[r.ID] = true
	}
	return s
}

func searchIDs(t *testing.T, db *sqlitestore.DB, ctx context.Context, q store.Query) map[string]bool {
	t.Helper()
	got, err := db.Search(ctx, q)
	if err != nil {
		t.Fatalf("Search(%+v): %v", q, err)
	}
	return ids(got)
}

func eqIDs(t *testing.T, got map[string]bool, want ...string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("got ids %v, want %v", keys(got), want)
	}
	for _, w := range want {
		if !got[w] {
			t.Fatalf("got ids %v, want %v", keys(got), want)
		}
	}
}

func keys(m map[string]bool) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}

func TestSearch_TextFTS(t *testing.T) {
	db, ctx := seedCorpus(t)
	eqIDs(t, searchIDs(t, db, ctx, store.Query{Text: "git"}), "r1", "r2")
	eqIDs(t, searchIDs(t, db, ctx, store.Query{Text: "docker"}), "r3")
}

// CommandTextOnly scopes the FTS match to the command column: a term that
// appears only in a record's cwd (a directory path) must not match. Default
// Text still matches command OR cwd. Guards how_did_i_run against directory
// paths crowding genuine invocations out of its scan window.
func TestSearch_CommandTextOnly(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	recs := []record.Record{
		// "git" only in the cwd path, not the command.
		{ID: "cwdonly", Command: "ls -la", CWD: "/srv/git-repos/x", ExitCode: ptr(0), StartTime: base, CreatedAt: base},
		// "git" is the actual command.
		{ID: "cmd", Command: "git status", CWD: "/tmp", ExitCode: ptr(0), StartTime: base.Add(time.Minute), CreatedAt: base},
	}
	if err := db.Put(ctx, recs...); err != nil {
		t.Fatalf("seed: %v", err)
	}
	// Default: FTS matches command OR cwd, so both records match "git".
	eqIDs(t, searchIDs(t, db, ctx, store.Query{Text: "git"}), "cwdonly", "cmd")
	// Command-scoped: only the record whose command is git.
	eqIDs(t, searchIDs(t, db, ctx, store.Query{Text: "git", CommandTextOnly: true}), "cmd")
}

// Whitespace-only text has no searchable tokens; it must not produce a MATCH ”
// syntax error — it's treated as no text filter (matches everything).
func TestSearch_BlankTextIsNoFilter(t *testing.T) {
	db, ctx := seedCorpus(t)
	eqIDs(t, searchIDs(t, db, ctx, store.Query{Text: "   "}), "r1", "r2", "r3", "r4")
}

func TestSearch_EqualityFilters(t *testing.T) {
	db, ctx := seedCorpus(t)
	eqIDs(t, searchIDs(t, db, ctx, store.Query{Host: "hostA"}), "r1", "r2")
	eqIDs(t, searchIDs(t, db, ctx, store.Query{Session: "s1"}), "r1", "r3")
	eqIDs(t, searchIDs(t, db, ctx, store.Query{ExitCode: ptr(1)}), "r2")
}

func TestSearch_ByID(t *testing.T) {
	db, ctx := seedCorpus(t)
	eqIDs(t, searchIDs(t, db, ctx, store.Query{ID: "r3"}), "r3")
	eqIDs(t, searchIDs(t, db, ctx, store.Query{ID: "nope"})) // no match -> empty
}

func TestSearch_TimeWindow(t *testing.T) {
	db, ctx := seedCorpus(t)
	// [base+1min, base+3min) -> r2, r3 (r1 too early, r4 at the exclusive upper bound)
	eqIDs(t, searchIDs(t, db, ctx, store.Query{
		Since: base.Add(time.Minute),
		Until: base.Add(3 * time.Minute),
	}), "r2", "r3")
}

func orderedIDs(t *testing.T, db *sqlitestore.DB, ctx context.Context, q store.Query) []string {
	t.Helper()
	got, err := db.Search(ctx, q)
	if err != nil {
		t.Fatalf("Search(%+v): %v", q, err)
	}
	var out []string
	for _, r := range got {
		out = append(out, r.ID)
	}
	return out
}

func eqOrder(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("order: got %v want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("order: got %v want %v", got, want)
		}
	}
}

func TestSearch_NewestFirstByDefault(t *testing.T) {
	db, ctx := seedCorpus(t) // start times r1<r2<r3<r4
	eqOrder(t, orderedIDs(t, db, ctx, store.Query{}), []string{"r4", "r3", "r2", "r1"})
}

func TestSearch_ReverseIsOldestFirst(t *testing.T) {
	db, ctx := seedCorpus(t)
	eqOrder(t, orderedIDs(t, db, ctx, store.Query{Reverse: true}), []string{"r1", "r2", "r3", "r4"})
}

func TestSearch_LimitAndOffset(t *testing.T) {
	db, ctx := seedCorpus(t)
	eqOrder(t, orderedIDs(t, db, ctx, store.Query{Limit: 2}), []string{"r4", "r3"})
	eqOrder(t, orderedIDs(t, db, ctx, store.Query{Limit: 2, Offset: 2}), []string{"r2", "r1"})
}

// Cursor (the client sync side): freshly Put records are unsynced until pushed.
func TestCursor_UnsyncedAndMarkSynced(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	if err := db.Put(ctx,
		record.Record{ID: "a", Command: "one", StartTime: base, CreatedAt: base},
		record.Record{ID: "b", Command: "two", StartTime: base.Add(time.Minute), CreatedAt: base},
	); err != nil {
		t.Fatalf("Put: %v", err)
	}

	un, err := db.Unsynced(ctx, 10)
	if err != nil {
		t.Fatalf("Unsynced: %v", err)
	}
	if !sameSet(idsList(un), []string{"a", "b"}) {
		t.Fatalf("want a,b unsynced, got %v", idsList(un))
	}

	if err := db.MarkSynced(ctx, "a"); err != nil {
		t.Fatalf("MarkSynced: %v", err)
	}
	un, _ = db.Unsynced(ctx, 10)
	if !sameSet(idsList(un), []string{"b"}) {
		t.Fatalf("after MarkSynced(a), want only b, got %v", idsList(un))
	}
}

// Tombstones must sync, so Unsynced returns deleted records (Search hides them).
func TestCursor_UnsyncedIncludesTombstones(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	if err := db.Put(ctx, record.Record{ID: "gone", Command: "secret", StartTime: base, CreatedAt: base, Deleted: true}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	un, err := db.Unsynced(ctx, 10)
	if err != nil {
		t.Fatalf("Unsynced: %v", err)
	}
	if len(un) != 1 || !un[0].Deleted {
		t.Fatalf("tombstone must be unsynced, got %+v", un)
	}
}

// deleted is monotonic: once a record is tombstoned, a later live update (e.g. a
// record-finish racing the delete) must not resurrect it. Guards a38z.
func TestPut_DeletedIsMonotonic(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	exit := 0
	rec := record.Record{ID: "x", Command: "secret", StartTime: base, CreatedAt: base}

	if err := db.Put(ctx, rec); err != nil { // live
		t.Fatalf("put live: %v", err)
	}
	dead := rec
	dead.Deleted = true
	if err := db.Put(ctx, dead); err != nil { // tombstone
		t.Fatalf("put tombstone: %v", err)
	}
	finished := rec // Deleted=false again, now with an exit code
	finished.ExitCode = &exit
	if err := db.Put(ctx, finished); err != nil { // later live write racing the delete
		t.Fatalf("put finish: %v", err)
	}

	// The tombstone stuck: still hidden from Search/Count...
	if recs, _ := db.Search(ctx, store.Query{}); len(recs) != 0 {
		t.Errorf("tombstoned record must stay hidden, got %v", recs)
	}
	if n, _ := db.Count(ctx, store.Query{}); n != 0 {
		t.Errorf("Count after sticky tombstone: got %d want 0", n)
	}
	// ...and the stored row is still Deleted=true: the later live write recorded
	// its exit code but did not un-delete.
	un, _ := db.Unsynced(ctx, 10)
	if len(un) != 1 || !un[0].Deleted {
		t.Fatalf("row must remain a tombstone after later live write, got %+v", un)
	}
}

func TestCursor_LastPulledRoundTrip(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	if seq, err := db.LastPulled(ctx); err != nil || seq != 0 {
		t.Fatalf("default LastPulled: got %d,%v want 0,nil", seq, err)
	}
	if err := db.SetLastPulled(ctx, 42); err != nil {
		t.Fatalf("SetLastPulled: %v", err)
	}
	if seq, _ := db.LastPulled(ctx); seq != 42 {
		t.Fatalf("LastPulled after set: got %d want 42", seq)
	}
	// advancing again overwrites
	if err := db.SetLastPulled(ctx, 99); err != nil {
		t.Fatalf("SetLastPulled: %v", err)
	}
	if seq, _ := db.LastPulled(ctx); seq != 99 {
		t.Fatalf("LastPulled after re-set: got %d want 99", seq)
	}
}

// FailedOnly keeps only finished commands with a non-zero exit code — neither
// successes (exit 0) nor still-unfinished records (NULL exit) match.
func TestSearch_FailedOnly(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	if err := db.Put(ctx,
		record.Record{ID: "ok", Command: "ok", ExitCode: ptr(0), StartTime: base, CreatedAt: base},
		record.Record{ID: "fail2", Command: "boom", ExitCode: ptr(2), StartTime: base.Add(time.Minute), CreatedAt: base},
		record.Record{ID: "fail127", Command: "nope", ExitCode: ptr(127), StartTime: base.Add(2 * time.Minute), CreatedAt: base},
		record.Record{ID: "running", Command: "sleep 99", StartTime: base.Add(3 * time.Minute), CreatedAt: base}, // unfinished, NULL exit
	); err != nil {
		t.Fatalf("seed: %v", err)
	}
	got, err := db.Search(ctx, store.Query{FailedOnly: true})
	if err != nil {
		t.Fatalf("Search FailedOnly: %v", err)
	}
	if !sameSet(idsList(got), []string{"fail2", "fail127"}) {
		t.Fatalf("FailedOnly returned %v, want [fail2 fail127]", idsList(got))
	}
}

// ExcludeID omits a single record (used to hide the in-flight query command
// from its own output), and is reflected in both Search and Count.
func TestSearch_ExcludeID(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	for i, id := range []string{"a", "b", "c"} {
		if err := db.Put(ctx, record.Record{
			ID: id, Command: "cmd", ExitCode: ptr(0),
			StartTime: base.Add(time.Duration(i) * time.Minute), CreatedAt: base,
		}); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	got, err := db.Search(ctx, store.Query{ExcludeID: "b"})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if !sameSet(idsList(got), []string{"a", "c"}) {
		t.Fatalf("ExcludeID=b returned %v, want [a c]", idsList(got))
	}
	if n, _ := db.Count(ctx, store.Query{ExcludeID: "b"}); n != 2 {
		t.Fatalf("Count ExcludeID=b: got %d want 2", n)
	}
}

// ExcludeCorrID omits records carrying a given corr_id (the querying agent's own
// in-flight session) while keeping NULL/empty-corr_id rows, in both Search and
// Count; an empty ExcludeCorrID filters nothing.
func TestSearch_ExcludeCorrID(t *testing.T) {
	// Opened directly (not via openTestDB) so the test can also reach the file
	// through a second raw *sql.DB connection, to seed a genuinely NULL corr_id
	// below — something Put/record.Record can't express (CorrID: "" binds as
	// the empty string, never SQL NULL).
	path := filepath.Join(t.TempDir(), "history.db")
	db, err := sqlitestore.Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	ctx := context.Background()
	seed := []struct{ id, corr string }{
		{"x1", "X"},
		{"y1", "Y"},
		{"n1", ""},    // empty-string corr_id (bound as TEXT '') — must be kept, not dropped
		{"null1", ""}, // placeholder; nulled out below via raw SQL
	}
	for i, s := range seed {
		if err := db.Put(ctx, record.Record{
			ID: s.id, Command: "cmd", CorrID: s.corr, ExitCode: ptr(0),
			StartTime: base.Add(time.Duration(i) * time.Minute), CreatedAt: base,
		}); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	// Give "null1" a genuinely NULL corr_id (as every pre-M10 record has) by
	// updating it through a second connection to the same file. This is what
	// actually exercises the COALESCE(r.corr_id,'') in the exclude predicate —
	// without it, a naive `r.corr_id != ?` would still pass the test above
	// while silently dropping every NULL-corr_id row.
	raw, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatalf("open raw: %v", err)
	}
	if _, err := raw.Exec(`UPDATE records SET corr_id = NULL WHERE id = 'null1'`); err != nil {
		t.Fatalf("null out corr_id: %v", err)
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("close raw: %v", err)
	}

	got, err := db.Search(ctx, store.Query{ExcludeCorrID: "X"})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if !sameSet(idsList(got), []string{"y1", "n1", "null1"}) {
		t.Fatalf("ExcludeCorrID=X returned %v, want [y1 n1 null1] (incl. empty- and NULL-corr_id rows)", idsList(got))
	}
	if n, _ := db.Count(ctx, store.Query{ExcludeCorrID: "X"}); n != 3 {
		t.Fatalf("Count ExcludeCorrID=X: got %d want 3", n)
	}
	// An empty ExcludeCorrID excludes nothing.
	if n, _ := db.Count(ctx, store.Query{ExcludeCorrID: ""}); n != 4 {
		t.Fatalf("Count ExcludeCorrID='' (no filter): got %d want 4", n)
	}
}

// Count returns the number of live (non-tombstoned) records matching a query,
// the basis for `yas history`'s absolute entry numbers.
func TestCount(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	if n, err := db.Count(ctx, store.Query{}); err != nil || n != 0 {
		t.Fatalf("empty Count: got %d, %v; want 0", n, err)
	}

	for i := 0; i < 3; i++ {
		if err := db.Put(ctx, record.Record{
			ID: string(rune('a' + i)), Command: "cmd", Hostname: "h1",
			StartTime: base.Add(time.Duration(i) * time.Minute), CreatedAt: base,
		}); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	if err := db.Put(ctx, record.Record{
		ID: "z", Command: "other", Hostname: "h2", StartTime: base.Add(time.Hour), CreatedAt: base,
	}); err != nil {
		t.Fatalf("seed h2: %v", err)
	}

	if n, _ := db.Count(ctx, store.Query{}); n != 4 {
		t.Fatalf("Count all: got %d want 4", n)
	}
	// Filters apply just like Search.
	if n, _ := db.Count(ctx, store.Query{Host: "h1"}); n != 3 {
		t.Fatalf("Count host=h1: got %d want 3", n)
	}

	// Tombstones drop out of the count (mirrors Search excluding deleted rows).
	if err := db.Put(ctx, record.Record{
		ID: "a", Command: "cmd", StartTime: base, CreatedAt: base, Deleted: true,
	}); err != nil {
		t.Fatalf("tombstone: %v", err)
	}
	if n, _ := db.Count(ctx, store.Query{}); n != 3 {
		t.Fatalf("Count after tombstone: got %d want 3", n)
	}
}

func idsList(recs []record.Record) []string {
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

// Executor + corr_id round-trip through Put -> Search.
func TestPutSearch_ExecutorCorrID(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	want := record.Record{ID: "e1", Command: "deploy", StartTime: base, CreatedAt: base, Executor: "claude-code", CorrID: "sess-9"}
	if err := db.Put(ctx, want); err != nil {
		t.Fatalf("Put: %v", err)
	}
	got, err := db.Search(ctx, store.Query{})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("want 1, got %d", len(got))
	}
	if got[0].Executor != "claude-code" || got[0].CorrID != "sess-9" {
		t.Errorf("round-trip: got executor=%q corr_id=%q", got[0].Executor, got[0].CorrID)
	}
}

// Opening a database created before these columns existed must add them and
// preserve existing rows (additive migration).
func TestMigrate_AddsColumnsToOldDB(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	raw, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatalf("open raw: %v", err)
	}
	if _, err := raw.Exec(`CREATE TABLE records (
		id TEXT PRIMARY KEY, command TEXT NOT NULL, cwd TEXT, hostname TEXT,
		session TEXT, shell TEXT, username TEXT, exit_code INTEGER,
		start_time INTEGER NOT NULL, duration_ms INTEGER, created_at INTEGER NOT NULL,
		deleted INTEGER NOT NULL DEFAULT 0, synced INTEGER NOT NULL DEFAULT 0)`); err != nil {
		t.Fatalf("create old: %v", err)
	}
	if _, err := raw.Exec(`INSERT INTO records (id, command, start_time, created_at) VALUES ('old1','legacy',1,1)`); err != nil {
		t.Fatalf("seed old: %v", err)
	}
	raw.Close()

	db, err := sqlitestore.Open(path) // applies schema + migration
	if err != nil {
		t.Fatalf("Open migrated: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	got, err := db.Search(context.Background(), store.Query{}) // no Text -> no FTS dependency on the legacy row
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(got) != 1 || got[0].ID != "old1" {
		t.Fatalf("legacy row lost after migration: %+v", got)
	}
	if got[0].Executor != "" {
		t.Errorf("legacy executor should be empty, got %q", got[0].Executor)
	}
	if err := db.Put(context.Background(), record.Record{ID: "new1", Command: "x", StartTime: base, CreatedAt: base, Executor: "codex"}); err != nil {
		t.Fatalf("Put after migrate: %v", err)
	}
}

func TestSearch_ExecutorFilters(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	recs := []record.Record{
		{ID: "h0", Command: "ls", StartTime: base, CreatedAt: base}, // empty -> human
		{ID: "h1", Command: "cd", StartTime: base.Add(time.Minute), CreatedAt: base, Executor: "human"},
		{ID: "a1", Command: "deploy", StartTime: base.Add(2 * time.Minute), CreatedAt: base, Executor: "claude-code"},
		{ID: "a2", Command: "test", StartTime: base.Add(3 * time.Minute), CreatedAt: base, Executor: "codex"},
	}
	if err := db.Put(ctx, recs...); err != nil {
		t.Fatalf("seed: %v", err)
	}
	eqIDs(t, searchIDs(t, db, ctx, store.Query{Executor: "claude-code"}), "a1")
	eqIDs(t, searchIDs(t, db, ctx, store.Query{AgentsOnly: true}), "a1", "a2")
	eqIDs(t, searchIDs(t, db, ctx, store.Query{HumansOnly: true}), "h0", "h1")
}

// Sessions returns distinct non-empty session ids of live records, newest first.
func TestSessions_DistinctLiveNewestFirst(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	mk := func(id, sess string, min int, deleted bool) record.Record {
		return record.Record{ID: id, Command: "c", Session: sess,
			StartTime: base.Add(time.Duration(min) * time.Minute), CreatedAt: base, Deleted: deleted}
	}
	if err := db.Put(ctx,
		mk("1", "sessA", 0, false),
		mk("2", "sessA", 5, false), // dup session, newer
		mk("3", "sessB", 10, false),
		mk("4", "", 11, false),     // empty session -> excluded
		mk("5", "sessC", 12, true), // only-deleted session -> excluded
	); err != nil {
		t.Fatalf("seed: %v", err)
	}
	got, err := db.Sessions(ctx)
	if err != nil {
		t.Fatalf("Sessions: %v", err)
	}
	// sessB (max t=10) newer than sessA (max t=5); sessC/"" excluded
	want := []string{"sessB", "sessA"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("Sessions = %v, want %v", got, want)
	}
}

// Sessions on an empty store returns a non-nil empty slice (the []-not-null
// contract), so the JSON paths that consume it never serialize null.
func TestSessions_EmptyStoreReturnsNonNil(t *testing.T) {
	got, err := openTestDB(t).Sessions(context.Background())
	if err != nil {
		t.Fatalf("Sessions: %v", err)
	}
	if got == nil {
		t.Fatal("Sessions on empty store returned nil, want []string{}")
	}
	if len(got) != 0 {
		t.Fatalf("Sessions on empty store = %v, want empty", got)
	}
}
