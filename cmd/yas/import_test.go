package main

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mjacobs/yas/internal/histimport"
	"github.com/mjacobs/yas/internal/record"
	"github.com/mjacobs/yas/internal/store"
	_ "modernc.org/sqlite"
)

// mustParseZsh parses a zsh-history string into records, failing the test on a
// parse error. doImport takes parsed records, so tests parse explicitly.
func mustParseZsh(t *testing.T, in, host string) []record.Record {
	t.Helper()
	recs, err := histimport.ParseZsh(strings.NewReader(in), host)
	if err != nil {
		t.Fatalf("ParseZsh: %v", err)
	}
	return recs
}

// doImport persists parsed zsh history, stamped with host + executor=human,
// and is idempotent on re-import (deterministic ids).
func TestDoImport_ZshHistory(t *testing.T) {
	db := openTestStore(t)
	ctx := context.Background()
	in := ": 1771486905:2;git status\n: 1771486906:0;ls -la\n"

	n, _, err := doImport(ctx, db, mustParseZsh(t, in, "hostZ"))
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if n != 2 {
		t.Fatalf("imported %d want 2", n)
	}
	recs, err := db.Search(ctx, store.Query{})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(recs) != 2 {
		t.Fatalf("store has %d want 2", len(recs))
	}
	for _, r := range recs {
		if r.Executor != record.ExecutorHuman || r.Hostname != "hostZ" {
			t.Errorf("stamp wrong: executor=%q host=%q", r.Executor, r.Hostname)
		}
	}

	// Re-import the same history: deterministic ids upsert, so no duplicates.
	if _, _, err := doImport(ctx, db, mustParseZsh(t, in, "hostZ")); err != nil {
		t.Fatalf("reimport: %v", err)
	}
	recs2, _ := db.Search(ctx, store.Query{})
	if len(recs2) != 2 {
		t.Fatalf("after reimport store has %d want 2 (must be idempotent)", len(recs2))
	}
}

// liveRecord builds a live-hook-shaped record (random id, session/exit/duration
// set) — the shape `yas record start` + `finish` leaves behind.
func liveRecord(cmd, host string, ts time.Time) record.Record {
	exit := 0
	dur := int64(47)
	return record.Record{
		ID:         record.NewID(),
		Command:    cmd,
		Hostname:   host,
		Session:    host + "-42-1771486000",
		Shell:      "zsh",
		Executor:   record.ExecutorHuman,
		ExitCode:   &exit,
		DurationMS: &dur,
		StartTime:  ts,
		CreatedAt:  ts,
	}
}

// A command the live hook already captured must not gain a second, skeleton row
// on import: the hook records with a random UUIDv7 while the importer derives a
// deterministic id, so id-upsert alone cannot dedup them (kata h4t6).
func TestDoImport_SkipsCommandsAlreadyCapturedLive(t *testing.T) {
	db := openTestStore(t)
	ctx := context.Background()

	// Live capture of `git status` at .470s inside the same second the zsh
	// history file stamps the entry with.
	live := liveRecord("git status", "hostZ", time.Unix(1771486905, 470_000_000))
	if err := db.Put(ctx, live); err != nil {
		t.Fatalf("seed live record: %v", err)
	}

	in := ": 1771486905:2;git status\n: 1771486906:0;ls -la\n"
	imported, skipped, err := doImport(ctx, db, mustParseZsh(t, in, "hostZ"))
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if imported != 1 || skipped != 1 {
		t.Fatalf("imported=%d skipped=%d, want imported=1 skipped=1", imported, skipped)
	}

	recs, err := db.Search(ctx, store.Query{Text: "git status"})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(recs) != 1 {
		t.Fatalf("`git status` has %d rows, want 1 (the live capture only)", len(recs))
	}
	if recs[0].ID != live.ID {
		t.Errorf("surviving row is %s, want the live capture %s", recs[0].ID, live.ID)
	}
	// The uncovered entry still imports.
	if recs, _ := db.Search(ctx, store.Query{Text: "ls"}); len(recs) != 1 {
		t.Fatalf("`ls -la` has %d rows, want 1 (imported)", len(recs))
	}
}

// The dedup window is ±1s: zsh stamps whole seconds and the hook's clock can
// land just across a second boundary — but no wider, or distinct re-runs of the
// same command would be swallowed.
func TestDoImport_LiveCoverageWindowIsOneSecond(t *testing.T) {
	db := openTestStore(t)
	ctx := context.Background()

	// history epoch 1771486905; live captures 1.0s and 2.0s later.
	if err := db.Put(ctx,
		liveRecord("make build", "hostZ", time.Unix(1771486906, 0)),
		liveRecord("make test", "hostZ", time.Unix(1771486907, 0)),
	); err != nil {
		t.Fatalf("seed: %v", err)
	}

	in := ": 1771486905:0;make build\n: 1771486905:0;make test\n"
	imported, skipped, err := doImport(ctx, db, mustParseZsh(t, in, "hostZ"))
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	// `make build` (+1s) is covered; `make test` (+2s) is a distinct run.
	if imported != 1 || skipped != 1 {
		t.Fatalf("imported=%d skipped=%d, want imported=1 skipped=1", imported, skipped)
	}
	if recs, _ := db.Search(ctx, store.Query{Text: "make test"}); len(recs) != 2 {
		t.Fatalf("`make test` has %d rows, want 2 (live run + earlier history run)", len(recs))
	}
}

// A live capture on another host never covers this host's history entries.
func TestDoImport_LiveCoverageIsPerHost(t *testing.T) {
	db := openTestStore(t)
	ctx := context.Background()

	if err := db.Put(ctx, liveRecord("git push", "otherhost", time.Unix(1771486905, 0))); err != nil {
		t.Fatalf("seed: %v", err)
	}
	imported, skipped, err := doImport(ctx, db, mustParseZsh(t, ": 1771486905:0;git push\n", "hostZ"))
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if imported != 1 || skipped != 0 {
		t.Fatalf("imported=%d skipped=%d, want imported=1 skipped=0", imported, skipped)
	}
}

// Rows a previous import created must not suppress DISTINCT later candidates:
// two runs of the same command 1s apart, arriving in separate import passes,
// are both real history. Import-vs-import dedup is the exact deterministic id
// (same second), never the ±1s live window.
func TestDoImport_PriorImportRowsAreNotCoverage(t *testing.T) {
	db := openTestStore(t)
	ctx := context.Background()

	// Pass 1: history holds one `ls` run.
	if _, _, err := doImport(ctx, db, mustParseZsh(t, ": 1771486905:0;ls\n", "hostZ")); err != nil {
		t.Fatalf("first import: %v", err)
	}
	// Pass 2: the file has grown a DISTINCT `ls` run one second later.
	in := ": 1771486905:0;ls\n: 1771486906:0;ls\n"
	imported, skipped, err := doImport(ctx, db, mustParseZsh(t, in, "hostZ"))
	if err != nil {
		t.Fatalf("second import: %v", err)
	}
	// ls@905 already exists (exact id -> skipped, never re-Put); ls@906 is a
	// distinct run one second away and MUST import despite the ±1s window.
	if imported != 1 || skipped != 1 {
		t.Fatalf("imported=%d skipped=%d, want imported=1 skipped=1", imported, skipped)
	}
	if recs, _ := db.Search(ctx, store.Query{Text: "ls"}); len(recs) != 2 {
		t.Fatalf("`ls` has %d rows, want 2 (two distinct runs)", len(recs))
	}
}

// Plain (non-extended) history lines carry synthetic ~1970 timestamps: they can
// never coincide with a live capture, so they always import and must not match
// against real records.
func TestDoImport_PlainLinesNeverDedupAgainstLive(t *testing.T) {
	db := openTestStore(t)
	ctx := context.Background()

	if err := db.Put(ctx, liveRecord("git status", "hostZ", time.Unix(1771486905, 0))); err != nil {
		t.Fatalf("seed: %v", err)
	}
	imported, skipped, err := doImport(ctx, db, mustParseZsh(t, "git status\nls -la\n", "hostZ"))
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if imported != 2 || skipped != 0 {
		t.Fatalf("imported=%d skipped=%d, want imported=2 skipped=0", imported, skipped)
	}
}

// A row already in the store is never re-Put by an import: cross-source
// imports share deterministic ids on purpose (same event from atuin and zsh
// history maps to one row), and the store's upsert is LWW on mutable fields —
// so a later, SPARSER source (zsh has no exit codes) would wipe the richer
// row's metadata, and even a same-source re-import would dirty synced rows
// into needless re-pushes (roborev 909).
func TestDoImport_SparserSourceDoesNotWipeRicherRow(t *testing.T) {
	db := openTestStore(t)
	ctx := context.Background()

	// Atuin knows the exit code; import it first.
	path := makeAtuinFixture(t, [][3]any{
		{"git status", "hostZ:mj", int64(1771486905_470_000_000)},
	})
	if _, _, err := doImport(ctx, db, mustParseAtuin(t, path, "hostZ")); err != nil {
		t.Fatalf("atuin import: %v", err)
	}

	// The same event from zsh history (whole-second epoch -> same stable id),
	// which never carries an exit code.
	imported, skipped, err := doImport(ctx, db, mustParseZsh(t, ": 1771486905:0;git status\n", "hostZ"))
	if err != nil {
		t.Fatalf("zsh import: %v", err)
	}
	if imported != 0 || skipped != 1 {
		t.Fatalf("imported=%d skipped=%d, want imported=0 skipped=1 (row already present)", imported, skipped)
	}
	recs, err := db.Search(ctx, store.Query{Text: "git status"})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(recs) != 1 {
		t.Fatalf("`git status` has %d rows, want 1 (same stable id)", len(recs))
	}
	if recs[0].ExitCode == nil {
		t.Fatal("exit code wiped: the sparser zsh row overwrote atuin's metadata")
	}
}

// Deleted history stays deleted: a tombstoned row still covers its id, so a
// re-import neither resurrects it nor re-Puts the tombstone (which would dirty
// a synced deletion into a re-push on every import — roborev 924). The same
// applies to a deleted live capture: deleting a recorded command (say, one
// with a leaked secret) must not be undone by the next import.
func TestDoImport_DeletedRowsStayDeleted(t *testing.T) {
	db := openTestStore(t)
	ctx := context.Background()

	if _, _, err := doImport(ctx, db, mustParseZsh(t, ": 1771486905:0;export TOKEN=oops\n", "hostZ")); err != nil {
		t.Fatalf("import: %v", err)
	}
	recs, err := db.Search(ctx, store.Query{})
	if err != nil || len(recs) != 1 {
		t.Fatalf("seed search: %v (%d rows)", err, len(recs))
	}
	tomb := recs[0]
	tomb.Deleted = true
	if err := db.Put(ctx, tomb); err != nil {
		t.Fatalf("tombstone: %v", err)
	}

	imported, skipped, err := doImport(ctx, db, mustParseZsh(t, ": 1771486905:0;export TOKEN=oops\n", "hostZ"))
	if err != nil {
		t.Fatalf("reimport: %v", err)
	}
	if imported != 0 || skipped != 1 {
		t.Fatalf("imported=%d skipped=%d, want imported=0 skipped=1 (deleted row covers its id)", imported, skipped)
	}
	if recs, _ := db.Search(ctx, store.Query{}); len(recs) != 0 {
		t.Fatalf("store has %d visible rows, want 0 (deletion honored)", len(recs))
	}
}

// makeAtuinFixture builds a minimal atuin history.db (>= v18 schema) with one
// row per (command, hostname, tsNS) triple and returns its path.
func makeAtuinFixture(t *testing.T, rows [][3]any) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "atuin-history.db")
	fdb, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatalf("open fixture db: %v", err)
	}
	defer func() { _ = fdb.Close() }()
	if _, err := fdb.Exec(`CREATE TABLE history (
		id text primary key, timestamp integer not null, duration integer not null,
		exit integer not null, command text not null, cwd text not null,
		session text not null, hostname text not null, deleted_at integer)`); err != nil {
		t.Fatalf("create fixture schema: %v", err)
	}
	for i, r := range rows {
		if _, err := fdb.Exec(
			`INSERT INTO history (id, timestamp, duration, exit, command, cwd, session, hostname, deleted_at)
			 VALUES (?, ?, 1000000, 0, ?, '/w', 'atuin-sess', ?, NULL)`,
			i, r[2], r[0], r[1]); err != nil {
			t.Fatalf("insert fixture row %d: %v", i, err)
		}
	}
	return path
}

// mustParseAtuin parses an atuin fixture db, failing the test on error.
func mustParseAtuin(t *testing.T, path, fallbackHost string) []record.Record {
	t.Helper()
	recs, err := histimport.ParseAtuin(path, fallbackHost)
	if err != nil {
		t.Fatalf("ParseAtuin: %v", err)
	}
	return recs
}

// An atuin import flows through the same doImport pipeline: rows land in the
// store stamped executor=human, and a re-import is idempotent (deterministic
// ids re-upsert the same rows; import rows are not live coverage, so nothing
// is "skipped" — the store just doesn't grow).
func TestDoImport_Atuin(t *testing.T) {
	db := openTestStore(t)
	ctx := context.Background()
	path := makeAtuinFixture(t, [][3]any{
		{"git status", "hostA:mj", int64(1771486905_470_000_000)},
		{"ls -la", "hostA:mj", int64(1771486906_120_000_000)},
	})

	imported, skipped, err := doImport(ctx, db, mustParseAtuin(t, path, "fallback"))
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if imported != 2 || skipped != 0 {
		t.Fatalf("imported=%d skipped=%d, want imported=2 skipped=0", imported, skipped)
	}
	recs, err := db.Search(ctx, store.Query{})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(recs) != 2 {
		t.Fatalf("store has %d want 2", len(recs))
	}
	for _, r := range recs {
		if r.Executor != record.ExecutorHuman || r.Hostname != "hostA" || r.Session != "" {
			t.Errorf("stamp wrong: executor=%q host=%q session=%q", r.Executor, r.Hostname, r.Session)
		}
	}

	// Re-import: every row already exists by exact id, so nothing is re-Put
	// (re-Putting would dirty synced rows and let sparser sources wipe richer
	// metadata — see TestDoImport_SparserSourceDoesNotWipeRicherRow).
	imported, skipped, err = doImport(ctx, db, mustParseAtuin(t, path, "fallback"))
	if err != nil {
		t.Fatalf("reimport: %v", err)
	}
	if imported != 0 || skipped != 2 {
		t.Fatalf("reimport imported=%d skipped=%d, want imported=0 skipped=2", imported, skipped)
	}
	if recs, _ := db.Search(ctx, store.Query{}); len(recs) != 2 {
		t.Fatalf("after reimport store has %d want 2 (must be idempotent)", len(recs))
	}
}

// A multi-host atuin db dedups per record host: a live capture on hostA covers
// only hostA's row; the same command at the same second on hostB still imports.
func TestDoImport_AtuinMultiHostCoverage(t *testing.T) {
	db := openTestStore(t)
	ctx := context.Background()

	live := liveRecord("git push", "hostA", time.Unix(1771486905, 470_000_000))
	if err := db.Put(ctx, live); err != nil {
		t.Fatalf("seed live record: %v", err)
	}

	path := makeAtuinFixture(t, [][3]any{
		{"git push", "hostA:mj", int64(1771486905_000_000_000)},
		{"git push", "hostB:mj", int64(1771486905_000_000_000)},
	})
	imported, skipped, err := doImport(ctx, db, mustParseAtuin(t, path, "fallback"))
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if imported != 1 || skipped != 1 {
		t.Fatalf("imported=%d skipped=%d, want imported=1 skipped=1", imported, skipped)
	}
	if recs, _ := db.Search(ctx, store.Query{Host: "hostB"}); len(recs) != 1 {
		t.Fatalf("hostB has %d rows, want 1 (imported)", len(recs))
	}
	// hostA keeps only the live capture.
	recs, _ := db.Search(ctx, store.Query{Host: "hostA"})
	if len(recs) != 1 || recs[0].ID != live.ID {
		t.Fatalf("hostA rows: got %d (want just the live capture %s)", len(recs), live.ID)
	}
}

// The same event imported first from ~/.zsh_history and then from atuin stays
// ONE row: zsh stamps whole seconds, atuin nanoseconds, and the atuin id is
// derived from the second-floored time, so the sources converge.
func TestDoImport_AtuinDedupsAgainstZshImport(t *testing.T) {
	db := openTestStore(t)
	ctx := context.Background()

	if _, _, err := doImport(ctx, db, mustParseZsh(t, ": 1771486905:0;git status\n", "hostA")); err != nil {
		t.Fatalf("zsh import: %v", err)
	}
	path := makeAtuinFixture(t, [][3]any{
		{"git status", "hostA:mj", int64(1771486905_470_000_000)},
	})
	if _, _, err := doImport(ctx, db, mustParseAtuin(t, path, "fallback")); err != nil {
		t.Fatalf("atuin import: %v", err)
	}
	recs, err := db.Search(ctx, store.Query{})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(recs) != 1 {
		t.Fatalf("store has %d rows, want 1 (zsh + atuin imports of one event must converge)", len(recs))
	}
}
