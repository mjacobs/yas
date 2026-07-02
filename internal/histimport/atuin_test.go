package histimport_test

import (
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mjacobs/yas/internal/histimport"
	"github.com/mjacobs/yas/internal/record"
	_ "modernc.org/sqlite"
)

// atuinDDL is the atuin (>= v18) client history schema the importer targets:
// nanosecond integer timestamps/durations, exit codes that may be -1, an
// "host:user" hostname column, and a nullable deleted_at tombstone.
const atuinDDL = `CREATE TABLE history (
	id text primary key,
	timestamp integer not null,
	duration integer not null,
	exit integer not null,
	command text not null,
	cwd text not null,
	session text not null,
	hostname text not null,
	deleted_at integer
)`

// atuinRow is one fixture history row; deletedAt nil leaves the column NULL.
type atuinRow struct {
	id        string
	ts        int64 // nanoseconds since epoch
	dur       int64 // nanoseconds; -1/0 = unknown
	exit      int64 // -1 = unknown
	command   string
	cwd       string
	session   string
	hostname  string
	deletedAt *int64
}

// makeAtuinDB builds a throwaway sqlite db with the given DDL and rows and
// returns its path. Built with the same pure-Go driver the importer uses.
func makeAtuinDB(t *testing.T, ddl string, rows []atuinRow) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "history.db")
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatalf("open fixture db: %v", err)
	}
	defer func() { _ = db.Close() }()
	if _, err := db.Exec(ddl); err != nil {
		t.Fatalf("create fixture schema: %v", err)
	}
	for _, r := range rows {
		if _, err := db.Exec(
			`INSERT INTO history (id, timestamp, duration, exit, command, cwd, session, hostname, deleted_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			r.id, r.ts, r.dur, r.exit, r.command, r.cwd, r.session, r.hostname, r.deletedAt,
		); err != nil {
			t.Fatalf("insert fixture row %q: %v", r.command, err)
		}
	}
	return path
}

// Two DISTINCT atuin rows in the same whole second (rapid repeats — atuin
// stamps nanoseconds, so it keeps both) must not collapse onto one id: that
// silently loses a real history entry through the id-upsert (roborev 963).
// Only the FIRST occurrence keeps the second-floored, zsh-compatible id; a
// repeat folds its occurrence index into the hash. Ids are deterministic
// across re-parses, so re-import stays an idempotent upsert.
func TestParseAtuin_SameSecondRepeatsKeepDistinctIDs(t *testing.T) {
	const sec = int64(1771486905)
	path := makeAtuinDB(t, atuinDDL, []atuinRow{
		{id: "a1", ts: sec*1_000_000_000 + 100_000_000, dur: -1, exit: 0,
			command: "ls", cwd: "/w", hostname: "hostA:mj"},
		{id: "a2", ts: sec*1_000_000_000 + 800_000_000, dur: -1, exit: 0,
			command: "ls", cwd: "/w", hostname: "hostA:mj"},
	})

	recs, err := histimport.ParseAtuin(path, "fallback")
	if err != nil {
		t.Fatalf("ParseAtuin: %v", err)
	}
	if len(recs) != 2 {
		t.Fatalf("got %d records, want 2", len(recs))
	}
	if recs[0].ID == recs[1].ID {
		t.Fatalf("same-second repeats share id %q — the second entry would be lost on import", recs[0].ID)
	}

	// The first occurrence still carries the zsh-compatible id (cross-source
	// dedup contract).
	fromZsh, err := histimport.ParseZsh(strings.NewReader(": 1771486905:0;ls\n"), "hostA")
	if err != nil {
		t.Fatalf("ParseZsh: %v", err)
	}
	if recs[0].ID != fromZsh[0].ID {
		t.Errorf("first occurrence id %q != zsh id %q (cross-source dedup broken)", recs[0].ID, fromZsh[0].ID)
	}

	// Deterministic across re-parses: re-import must upsert, not duplicate.
	again, err := histimport.ParseAtuin(path, "fallback")
	if err != nil {
		t.Fatalf("ParseAtuin (again): %v", err)
	}
	if again[0].ID != recs[0].ID || again[1].ID != recs[1].ID {
		t.Errorf("ids not stable across re-parses: %v vs %v",
			[]string{recs[0].ID, recs[1].ID}, []string{again[0].ID, again[1].ID})
	}
}

// A well-formed atuin row maps onto a record: command/cwd copied, the host part
// of "host:user" as hostname, exit and duration (ns -> ms) set, executor=human,
// nanosecond-precision start time, and NO session (imports are sessionless).
func TestParseAtuin_Basic(t *testing.T) {
	const ns = int64(1771486905_470_000_000) // 1771486905.470s
	path := makeAtuinDB(t, atuinDDL, []atuinRow{
		{id: "a1", ts: ns, dur: 2_500_000_000, exit: 0, command: "git status",
			cwd: "/w", session: "atuin-sess", hostname: "hostA:mj"},
	})

	recs, err := histimport.ParseAtuin(path, "fallback")
	if err != nil {
		t.Fatalf("ParseAtuin: %v", err)
	}
	if len(recs) != 1 {
		t.Fatalf("want 1 record, got %d", len(recs))
	}
	r := recs[0]
	if r.Command != "git status" || r.CWD != "/w" {
		t.Errorf("command/cwd: got %q %q", r.Command, r.CWD)
	}
	if r.Hostname != "hostA" {
		t.Errorf("hostname: got %q want %q (host part of host:user)", r.Hostname, "hostA")
	}
	if r.Session != "" {
		t.Errorf("session: got %q want empty (imported rows are sessionless)", r.Session)
	}
	if r.Executor != record.ExecutorHuman {
		t.Errorf("executor: got %q want %q", r.Executor, record.ExecutorHuman)
	}
	if !r.StartTime.Equal(time.Unix(0, ns)) {
		t.Errorf("start_time: got %v want %v (full atuin precision)", r.StartTime, time.Unix(0, ns))
	}
	if !r.CreatedAt.Equal(r.StartTime) {
		t.Errorf("created_at: got %v want start_time %v", r.CreatedAt, r.StartTime)
	}
	if r.ExitCode == nil || *r.ExitCode != 0 {
		t.Errorf("exit_code: got %v want 0", r.ExitCode)
	}
	if r.DurationMS == nil || *r.DurationMS != 2500 {
		t.Errorf("duration_ms: got %v want 2500 (2.5s in ns)", r.DurationMS)
	}
	if err := r.Validate(); err != nil {
		t.Errorf("imported record must be valid: %v", err)
	}
}

// The dedup contract with the zsh importer: the id is derived from the
// SECOND-floored timestamp, so the same command at the same second imported
// from atuin (nanosecond stamps) and from ~/.zsh_history (whole-second stamps)
// maps to the SAME id and upserts into one row.
func TestParseAtuin_IDMatchesZshImportSameSecond(t *testing.T) {
	const sec = int64(1771486905)
	path := makeAtuinDB(t, atuinDDL, []atuinRow{
		{id: "a1", ts: sec*1_000_000_000 + 470_000_000, dur: -1, exit: 0,
			command: "git status", cwd: "/w", hostname: "hostA:mj"},
	})
	fromAtuin, err := histimport.ParseAtuin(path, "fallback")
	if err != nil {
		t.Fatalf("ParseAtuin: %v", err)
	}
	fromZsh, err := histimport.ParseZsh(strings.NewReader(": 1771486905:0;git status\n"), "hostA")
	if err != nil {
		t.Fatalf("ParseZsh: %v", err)
	}
	if len(fromAtuin) != 1 || len(fromZsh) != 1 {
		t.Fatalf("want 1+1 records, got %d+%d", len(fromAtuin), len(fromZsh))
	}
	if fromAtuin[0].ID != fromZsh[0].ID {
		t.Errorf("ids differ: atuin %q vs zsh %q (same second+host+command must dedup)",
			fromAtuin[0].ID, fromZsh[0].ID)
	}
}

// Rows atuin has tombstoned (deleted_at set) are not imported.
func TestParseAtuin_SkipsDeletedRows(t *testing.T) {
	del := int64(1771487000_000_000_000)
	path := makeAtuinDB(t, atuinDDL, []atuinRow{
		{id: "a1", ts: 1_000_000_000_000_000_000, dur: -1, exit: 0, command: "kept", hostname: "h"},
		{id: "a2", ts: 2_000_000_000_000_000_000, dur: -1, exit: 0, command: "dropped", hostname: "h", deletedAt: &del},
	})
	recs, err := histimport.ParseAtuin(path, "fallback")
	if err != nil {
		t.Fatalf("ParseAtuin: %v", err)
	}
	if len(recs) != 1 || recs[0].Command != "kept" {
		t.Fatalf("want just the non-deleted row, got %+v", recs)
	}
}

// atuin stamps -1 (or 0 duration) when it doesn't know the exit/duration; those
// must stay nil rather than importing as a bogus failure or zero-length run.
func TestParseAtuin_UnknownExitAndDuration(t *testing.T) {
	path := makeAtuinDB(t, atuinDDL, []atuinRow{
		{id: "a1", ts: 1_771_486_905_000_000_000, dur: -1, exit: -1, command: "no exit", hostname: "h"},
		{id: "a2", ts: 1_771_486_906_000_000_000, dur: 0, exit: 7, command: "zero dur", hostname: "h"},
	})
	recs, err := histimport.ParseAtuin(path, "fallback")
	if err != nil {
		t.Fatalf("ParseAtuin: %v", err)
	}
	if len(recs) != 2 {
		t.Fatalf("want 2 records, got %d", len(recs))
	}
	if recs[0].ExitCode != nil {
		t.Errorf("exit -1 must import as nil ExitCode, got %d", *recs[0].ExitCode)
	}
	if recs[0].DurationMS != nil {
		t.Errorf("duration -1 must import as nil DurationMS, got %d", *recs[0].DurationMS)
	}
	if recs[1].ExitCode == nil || *recs[1].ExitCode != 7 {
		t.Errorf("exit 7 must import, got %v", recs[1].ExitCode)
	}
	if recs[1].DurationMS != nil {
		t.Errorf("duration 0 must import as nil DurationMS, got %d", *recs[1].DurationMS)
	}
}

// Hostname handling: "host:user" keeps the host part, a plain value is used
// as-is (including a degenerate ":user"), and only an empty value falls back.
func TestParseAtuin_HostnameForms(t *testing.T) {
	path := makeAtuinDB(t, atuinDDL, []atuinRow{
		{id: "a1", ts: 1_000_000_000_000_000_000, dur: -1, exit: 0, command: "one", hostname: "boxA:mj"},
		{id: "a2", ts: 2_000_000_000_000_000_000, dur: -1, exit: 0, command: "two", hostname: "plainhost"},
		{id: "a3", ts: 3_000_000_000_000_000_000, dur: -1, exit: 0, command: "three", hostname: ":mj"},
		{id: "a4", ts: 4_000_000_000_000_000_000, dur: -1, exit: 0, command: "four", hostname: ""},
	})
	recs, err := histimport.ParseAtuin(path, "fallback")
	if err != nil {
		t.Fatalf("ParseAtuin: %v", err)
	}
	want := []string{"boxA", "plainhost", ":mj", "fallback"}
	if len(recs) != len(want) {
		t.Fatalf("want %d records, got %d", len(want), len(recs))
	}
	for i, w := range want {
		if recs[i].Hostname != w {
			t.Errorf("row %d (%s): hostname got %q want %q", i, recs[i].Command, recs[i].Hostname, w)
		}
	}
}

// An older atuin schema missing a needed column fails loudly, naming the column
// rather than surfacing a bare sqlite error.
func TestParseAtuin_MissingColumnFailsByName(t *testing.T) {
	old := `CREATE TABLE history (
		id text primary key,
		timestamp integer not null,
		duration integer not null,
		exit integer not null,
		command text not null,
		cwd text not null,
		session text not null,
		hostname text not null
	)` // no deleted_at
	path := makeAtuinDB(t, old, nil)
	_, err := histimport.ParseAtuin(path, "fallback")
	if err == nil || !strings.Contains(err.Error(), `"deleted_at"`) {
		t.Fatalf("want an error naming the missing deleted_at column, got %v", err)
	}
}

// A db without a history table at all (not an atuin client db) fails clearly.
func TestParseAtuin_NoHistoryTable(t *testing.T) {
	path := makeAtuinDB(t, `CREATE TABLE other (x integer)`, nil)
	_, err := histimport.ParseAtuin(path, "fallback")
	if err == nil || !strings.Contains(err.Error(), "no history table") {
		t.Fatalf("want a no-history-table error, got %v", err)
	}
}

// Columns the importer doesn't know about (a newer atuin schema) are ignored:
// it selects only the columns it needs, by name.
func TestParseAtuin_ExtraColumnsIgnored(t *testing.T) {
	newer := strings.Replace(atuinDDL, "deleted_at integer",
		"deleted_at integer,\n\tshiny_new_field text,\n\tanother integer", 1)
	path := makeAtuinDB(t, newer, nil)
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatalf("open fixture: %v", err)
	}
	if _, err := db.Exec(
		`INSERT INTO history (id, timestamp, duration, exit, command, cwd, session, hostname, deleted_at, shiny_new_field, another)
		 VALUES ('a1', 1771486905000000000, -1, 0, 'ls', '/', '', 'h', NULL, 'x', 9)`); err != nil {
		t.Fatalf("insert: %v", err)
	}
	_ = db.Close()

	recs, err := histimport.ParseAtuin(path, "fallback")
	if err != nil {
		t.Fatalf("ParseAtuin: %v", err)
	}
	if len(recs) != 1 || recs[0].Command != "ls" {
		t.Fatalf("want the row despite extra columns, got %+v", recs)
	}
}

// The importer opens the db read-only: parsing a write-protected file works and
// leaves it untouched.
func TestParseAtuin_ReadOnlyFile(t *testing.T) {
	path := makeAtuinDB(t, atuinDDL, []atuinRow{
		{id: "a1", ts: 1_771_486_905_000_000_000, dur: -1, exit: 0, command: "ls", hostname: "h"},
	})
	if err := os.Chmod(path, 0o444); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	recs, err := histimport.ParseAtuin(path, "fallback")
	if err != nil {
		t.Fatalf("ParseAtuin on a read-only file: %v", err)
	}
	if len(recs) != 1 {
		t.Fatalf("want 1 record, got %d", len(recs))
	}
}

// An empty history table yields a non-nil empty slice (the [] contract), and a
// missing file is an error, not a silent zero-row import.
func TestParseAtuin_EmptyAndMissing(t *testing.T) {
	path := makeAtuinDB(t, atuinDDL, nil)
	recs, err := histimport.ParseAtuin(path, "fallback")
	if err != nil {
		t.Fatalf("ParseAtuin: %v", err)
	}
	if recs == nil || len(recs) != 0 {
		t.Fatalf("empty db must yield a non-nil empty slice, got %#v", recs)
	}

	if _, err := histimport.ParseAtuin(filepath.Join(t.TempDir(), "nope.db"), "fallback"); err == nil {
		t.Fatal("a missing db file must be an error")
	}
}
