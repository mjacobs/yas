package histimport

import (
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/mjacobs/yas/internal/record"
	_ "modernc.org/sqlite" // pure-Go driver: the cgo-free invariant holds
)

// atuinColumns are the history-table columns the importer needs. SELECTing them
// by name keeps unknown/extra columns in newer atuin schemas harmless; a missing
// one (an older schema) is reported by name up front instead of as a bare
// sqlite error mid-query.
var atuinColumns = []string{
	"command", "cwd", "hostname", "timestamp", "duration", "exit", "deleted_at",
}

// ParseAtuin reads an atuin client database (history.db, atuin >= v18 schema)
// and returns one record per non-deleted history row, oldest first.
//
// The db is opened READ-ONLY (mode=ro): the importer never writes, and shared
// read locks coexist with a running atuin daemon. (immutable=1 would skip
// locking entirely and could see torn writes from a live daemon, so plain
// read-only mode is the safer choice.)
//
// Mapping: command/cwd copy over; hostname is the host part of atuin's
// "host:user" column (the whole value when there is no host part, then
// fallbackHost when even that is empty); exit is kept only when >= 0 and
// duration (nanoseconds) only when > 0. StartTime keeps atuin's full nanosecond
// precision, but the deterministic id is derived from the SECOND-floored time —
// zsh extended history stamps whole seconds, so the same event imported from
// either source maps to the same id and dedups by upsert. Session stays empty:
// imported rows are sessionless by convention (atuin session ids are not yas
// shell-session ids).
func ParseAtuin(path, fallbackHost string) ([]record.Record, error) {
	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, fmt.Errorf("atuin db %s: %w", path, err)
	}
	defer func() { _ = db.Close() }()

	if err := checkAtuinSchema(db, path); err != nil {
		return nil, err
	}

	// rowid tiebreak keeps same-nanosecond rows in a stable order, so the
	// occurrence-indexed ids below are deterministic across re-imports.
	rows, err := db.Query(`SELECT command, cwd, hostname, timestamp, duration, exit
		FROM history WHERE deleted_at IS NULL ORDER BY timestamp, rowid`)
	if err != nil {
		return nil, fmt.Errorf("atuin db %s: %w", path, err)
	}
	defer func() { _ = rows.Close() }()

	// Non-nil so an empty db yields [] (the store/JSON contract), not nil.
	out := []record.Record{}
	// Distinct atuin rows can share a (second, host, command) key — rapid
	// repeats within one second, which atuin's nanosecond stamps keep apart.
	// The second-floored id would collapse them into one row (silent data
	// loss through the upsert), so only the FIRST occurrence gets the plain,
	// zsh-compatible id; each repeat folds its occurrence index into the
	// hash. zsh history has no discriminator at all for such repeats (they
	// collapse in the file's own format), so the first-occurrence id is the
	// one a zsh import of the same event derives.
	occurrence := map[string]int{}
	for rows.Next() {
		var (
			cmd, cwd, hostname sql.NullString
			ts, dur, exit      sql.NullInt64
		)
		if err := rows.Scan(&cmd, &cwd, &hostname, &ts, &dur, &exit); err != nil {
			return nil, fmt.Errorf("atuin db %s: %w", path, err)
		}
		if strings.TrimSpace(cmd.String) == "" {
			continue // an empty command can never become a valid record
		}
		host := atuinHost(hostname.String, fallbackHost)
		sec := ts.Int64 / int64(time.Second) // floor to the second: the id contract
		start := time.Unix(0, ts.Int64)
		key := fmt.Sprintf("%d\x00%s\x00%s", sec, host, cmd.String)
		n := occurrence[key]
		occurrence[key] = n + 1
		rec := record.Record{
			ID:        stableIDN(time.Unix(sec, 0), cmd.String, host, n),
			Command:   cmd.String,
			CWD:       cwd.String,
			Hostname:  host,
			Executor:  record.ExecutorHuman,
			StartTime: start,
			CreatedAt: start,
		}
		if exit.Valid && exit.Int64 >= 0 {
			e := int(exit.Int64)
			rec.ExitCode = &e
		}
		if dur.Valid && dur.Int64 > 0 {
			ms := dur.Int64 / int64(time.Millisecond)
			rec.DurationMS = &ms
		}
		out = append(out, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("atuin db %s: %w", path, err)
	}
	return out, nil
}

// checkAtuinSchema verifies the history table exists and carries every column
// the importer reads, so a too-old atuin schema fails with the missing column's
// name rather than an opaque query error.
func checkAtuinSchema(db *sql.DB, path string) error {
	rows, err := db.Query(`PRAGMA table_info(history)`)
	if err != nil {
		return fmt.Errorf("atuin db %s: %w", path, err)
	}
	defer func() { _ = rows.Close() }()

	have := map[string]bool{}
	for rows.Next() {
		var (
			cid, notnull, pk int
			name, typ        string
			dflt             sql.NullString
		)
		if err := rows.Scan(&cid, &name, &typ, &notnull, &dflt, &pk); err != nil {
			return fmt.Errorf("atuin db %s: %w", path, err)
		}
		have[strings.ToLower(name)] = true
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("atuin db %s: %w", path, err)
	}
	if len(have) == 0 {
		return fmt.Errorf("atuin db %s: no history table (is this an atuin client history.db?)", path)
	}
	for _, col := range atuinColumns {
		if !have[col] {
			return fmt.Errorf("atuin db %s: history table has no %q column (atuin schema too old? yas imports the atuin >= v18 schema)", path, col)
		}
	}
	return nil
}

// atuinHost extracts the machine name from atuin's hostname column, which is
// conventionally "host:user": the part before the first ':' when there is one,
// else the whole value. fallback fills in only when that leaves nothing.
func atuinHost(s, fallback string) string {
	host := s
	if i := strings.Index(s, ":"); i > 0 {
		host = s[:i]
	}
	if host == "" {
		return fallback
	}
	return host
}
