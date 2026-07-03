// Package sqlite implements the local-replica Store on top of a single SQLite
// file using the pure-Go modernc.org/sqlite driver (no cgo, so yas stays a
// trivially cross-compiled static binary). Times are stored as unix
// milliseconds; see schema.sql.
package sqlite

import (
	"context"
	"database/sql"
	_ "embed"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/mjacobs/yas/internal/record"
	"github.com/mjacobs/yas/internal/store"
	_ "modernc.org/sqlite"
)

//go:embed schema.sql
var schema string

// defaultLimit caps a Search when Query.Limit is unset (0).
const defaultLimit = 100

// DB is a local SQLite-backed Store.
type DB struct {
	db *sql.DB
}

// DB implements the shared store contract and the client-sync cursor.
var (
	_ store.Store  = (*DB)(nil)
	_ store.Cursor = (*DB)(nil)
)

// Open opens (creating if needed) the SQLite replica at path in WAL mode and
// applies the schema idempotently. WAL lets a separate `yas serve` reader run
// concurrently with the recording hook's writes on the same file.
func Open(path string) (*DB, error) {
	dsn := "file:" + path +
		"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)"
	sqldb, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	if _, err := sqldb.ExecContext(context.Background(), schema); err != nil {
		_ = sqldb.Close()
		return nil, fmt.Errorf("apply schema: %w", err)
	}
	if err := migrate(context.Background(), sqldb); err != nil {
		_ = sqldb.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return &DB{db: sqldb}, nil
}

// Close releases the underlying handle.
func (d *DB) Close() error { return d.db.Close() }

// Sessions returns the distinct non-empty session ids of live (non-tombstoned)
// records, ordered newest-session first (by the session's latest start_time).
// Used by `yas session` to resolve a short token back to a full session id.
func (d *DB) Sessions(ctx context.Context) ([]string, error) {
	rows, err := d.db.QueryContext(ctx, `
		SELECT session FROM records
		WHERE deleted = 0 AND session <> ''
		GROUP BY session
		ORDER BY max(start_time) DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

const putSQL = `
INSERT INTO records
    (id, command, cwd, hostname, session, shell, username, exit_code, start_time, duration_ms, created_at, deleted, executor, corr_id, synced)
VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,0)
ON CONFLICT(id) DO UPDATE SET
    exit_code   = excluded.exit_code,
    duration_ms = excluded.duration_ms,
    deleted     = max(deleted, excluded.deleted),
    synced      = 0`

// Put upserts records by ID. On conflict only the mutable fields (exit_code,
// duration_ms, deleted) are overwritten — everything else is immutable — and
// the row is re-marked unsynced so the change re-pushes. deleted is monotonic
// (max, never flips back to 0): a tombstone is sticky, so a later live write
// racing a delete can't resurrect the record (a38z). All records are applied in
// one transaction.
func (d *DB) Put(ctx context.Context, recs ...record.Record) error {
	if len(recs) == 0 {
		return nil
	}
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	stmt, err := tx.PrepareContext(ctx, putSQL)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for _, r := range recs {
		if err := r.Validate(); err != nil {
			return err
		}
		if _, err := stmt.ExecContext(ctx,
			r.ID, r.Command, r.CWD, r.Hostname, r.Session, r.Shell, r.Username,
			r.ExitCode, r.StartTime.UnixMilli(), r.DurationMS, r.CreatedAt.UnixMilli(),
			boolToInt(r.Deleted), r.Executor, r.CorrID,
		); err != nil {
			return err
		}
	}
	return tx.Commit()
}

const selectCols = `r.id, r.command, COALESCE(r.cwd,''), COALESCE(r.hostname,''), COALESCE(r.session,''), COALESCE(r.shell,''), COALESCE(r.username,''), r.exit_code, r.start_time, r.duration_ms, r.created_at, r.deleted, COALESCE(r.executor,''), COALESCE(r.corr_id,'')`

// filterClause builds the shared `WHERE ...` predicate used by both Search and
// Count from the non-ordering fields of q. Zero-valued fields are ignored, and
// tombstoned (deleted) rows are excluded unless q.IncludeDeleted asks for them
// (an internal-only escape hatch — see store.Query). The returned string begins
// with a leading space so it appends directly after `FROM records r`.
func filterClause(q store.Query) (string, []any) {
	var sb strings.Builder
	var args []any
	if q.IncludeDeleted {
		sb.WriteString(` WHERE 1 = 1`)
	} else {
		sb.WriteString(` WHERE r.deleted = 0`)
	}
	if q.ID != "" {
		sb.WriteString(` AND r.id = ?`)
		args = append(args, q.ID)
	}
	if q.ExcludeID != "" {
		sb.WriteString(` AND r.id != ?`)
		args = append(args, q.ExcludeID)
	}
	if fts := ftsQuery(q.Text); fts != "" {
		if q.CommandTextOnly {
			// Scope the FTS5 match to the command column only (records_fts also
			// indexes cwd), so a program name isn't matched via directory paths.
			fts = "command : (" + fts + ")"
		}
		sb.WriteString(` AND r.rowid IN (SELECT rowid FROM records_fts WHERE records_fts MATCH ?)`)
		args = append(args, fts)
	}
	if q.Host != "" {
		sb.WriteString(` AND r.hostname = ?`)
		args = append(args, q.Host)
	}
	if q.CWD != "" {
		sb.WriteString(` AND r.cwd = ?`)
		args = append(args, q.CWD)
	}
	if q.Session != "" {
		sb.WriteString(` AND r.session = ?`)
		args = append(args, q.Session)
	}
	if q.ExitCode != nil {
		sb.WriteString(` AND r.exit_code = ?`)
		args = append(args, *q.ExitCode)
	}
	if q.FailedOnly {
		sb.WriteString(` AND r.exit_code IS NOT NULL AND r.exit_code != 0`)
	}
	if q.Executor != "" {
		sb.WriteString(` AND r.executor = ?`)
		args = append(args, q.Executor)
	}
	if q.AgentsOnly {
		sb.WriteString(` AND r.executor IS NOT NULL AND r.executor != '' AND r.executor != 'human'`)
	}
	if q.HumansOnly {
		sb.WriteString(` AND (r.executor IS NULL OR r.executor = '' OR r.executor = 'human')`)
	}
	if !q.Since.IsZero() {
		sb.WriteString(` AND r.start_time >= ?`)
		args = append(args, q.Since.UnixMilli())
	}
	if !q.Until.IsZero() {
		sb.WriteString(` AND r.start_time < ?`)
		args = append(args, q.Until.UnixMilli())
	}
	return sb.String(), args
}

// Search returns records matching q. Zero-valued Query fields are ignored.
// Tombstoned (deleted) records are only returned under q.IncludeDeleted.
func (d *DB) Search(ctx context.Context, q store.Query) ([]record.Record, error) {
	where, args := filterClause(q)
	var sb strings.Builder
	sb.WriteString(`SELECT ` + selectCols + ` FROM records r`)
	sb.WriteString(where)

	// Newest-first by default; Reverse flips to oldest-first. id (a time-sortable
	// UUIDv7) is a deterministic tiebreak for records sharing a start_time.
	dir := ` DESC`
	if q.Reverse {
		dir = ` ASC`
	}
	sb.WriteString(` ORDER BY r.start_time` + dir + `, r.id` + dir)

	limit := q.Limit
	if limit <= 0 {
		limit = defaultLimit
	}
	sb.WriteString(` LIMIT ?`)
	args = append(args, limit)
	if q.Offset > 0 {
		sb.WriteString(` OFFSET ?`)
		args = append(args, q.Offset)
	}

	rows, err := d.db.QueryContext(ctx, sb.String(), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanRecords(rows)
}

// Count returns how many live (non-tombstoned) records match q, applying the
// same filters as Search but ignoring ordering/limit/offset. It is the basis for
// `yas history`'s absolute entry numbers.
func (d *DB) Count(ctx context.Context, q store.Query) (int, error) {
	where, args := filterClause(q)
	var n int
	err := d.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM records r`+where, args...).Scan(&n)
	return n, err
}

// ftsQuery turns free user text into a safe FTS5 MATCH expression: each
// whitespace-separated term is double-quoted (and inner quotes doubled) so the
// terms are matched literally and AND-ed, never interpreted as FTS5 operators.
func ftsQuery(text string) string {
	fields := strings.Fields(text)
	for i, f := range fields {
		fields[i] = `"` + strings.ReplaceAll(f, `"`, `""`) + `"`
	}
	return strings.Join(fields, " ")
}

func scanRecords(rows *sql.Rows) ([]record.Record, error) {
	// Non-nil so a zero-row result serializes as a JSON [] (the API/CLI contract),
	// never null.
	out := []record.Record{}
	for rows.Next() {
		var (
			r                  record.Record
			exit, dur          sql.NullInt64
			startMS, createdMS int64
			deleted            int
		)
		if err := rows.Scan(
			&r.ID, &r.Command, &r.CWD, &r.Hostname, &r.Session, &r.Shell, &r.Username,
			&exit, &startMS, &dur, &createdMS, &deleted, &r.Executor, &r.CorrID,
		); err != nil {
			return nil, err
		}
		if exit.Valid {
			v := int(exit.Int64)
			r.ExitCode = &v
		}
		if dur.Valid {
			v := dur.Int64
			r.DurationMS = &v
		}
		r.StartTime = time.UnixMilli(startMS)
		r.CreatedAt = time.UnixMilli(createdMS)
		r.Deleted = deleted != 0
		out = append(out, r)
	}
	return out, rows.Err()
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// --- Cursor: client-side sync state ---

const lastPulledKey = "last_pulled_seq"

// Unsynced returns local records not yet pushed to the server, oldest first.
// Tombstones are included — deletions must propagate to the server.
func (d *DB) Unsynced(ctx context.Context, limit int) ([]record.Record, error) {
	if limit <= 0 {
		limit = defaultLimit
	}
	rows, err := d.db.QueryContext(ctx,
		`SELECT id, command, COALESCE(cwd,''), COALESCE(hostname,''), COALESCE(session,''), COALESCE(shell,''), COALESCE(username,''), exit_code, start_time, duration_ms, created_at, deleted, COALESCE(executor,''), COALESCE(corr_id,'')
		 FROM records WHERE synced = 0 ORDER BY start_time ASC, id ASC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanRecords(rows)
}

// MarkSynced flags the given record ids as pushed (synced).
func (d *DB) MarkSynced(ctx context.Context, ids ...string) error {
	if len(ids) == 0 {
		return nil
	}
	ph := make([]string, len(ids))
	args := make([]any, len(ids))
	for i, id := range ids {
		ph[i] = "?"
		args[i] = id
	}
	//nolint:gosec // the concatenated parts are literal "?" placeholders; ids are bound parameters
	_, err := d.db.ExecContext(ctx,
		`UPDATE records SET synced = 1 WHERE id IN (`+strings.Join(ph, ",")+`)`, args...)
	return err
}

// LastPulled returns the highest server seq already applied locally (0 if unset).
func (d *DB) LastPulled(ctx context.Context) (int64, error) {
	var seq int64
	err := d.db.QueryRowContext(ctx, `SELECT value FROM sync_state WHERE key = ?`, lastPulledKey).Scan(&seq)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return seq, err
}

// SetLastPulled advances the persisted pull cursor.
func (d *DB) SetLastPulled(ctx context.Context, seq int64) error {
	_, err := d.db.ExecContext(ctx,
		`INSERT INTO sync_state (key, value) VALUES (?, ?)
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value`, lastPulledKey, seq)
	return err
}

// migrate applies additive column migrations for databases created before a
// column existed. CREATE TABLE IF NOT EXISTS leaves an existing table untouched,
// so missing columns are added here. Idempotent: a freshly-created DB already has
// them (added via `ALTER TABLE` for each missing column otherwise). Columns are nullable (legacy rows read "").
func migrate(ctx context.Context, db *sql.DB) error {
	have, err := tableColumns(ctx, db, "records")
	if err != nil {
		return err
	}
	for _, c := range []struct{ name, ddl string }{
		{"executor", `ALTER TABLE records ADD COLUMN executor TEXT`},
		{"corr_id", `ALTER TABLE records ADD COLUMN corr_id TEXT`},
	} {
		if !have[c.name] {
			if _, err := db.ExecContext(ctx, c.ddl); err != nil {
				return fmt.Errorf("add column %s: %w", c.name, err)
			}
		}
	}
	return nil
}

// tableColumns returns the set of column names on a table via PRAGMA table_info.
func tableColumns(ctx context.Context, db *sql.DB, table string) (map[string]bool, error) {
	// table is a compile-time constant here, so the concatenation is injection-safe.
	rows, err := db.QueryContext(ctx, `PRAGMA table_info(`+table+`)`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	cols := map[string]bool{}
	for rows.Next() {
		var (
			cid           int
			name, ctype   string
			notnull, pkey int
			dflt          sql.NullString
		)
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pkey); err != nil {
			return nil, err
		}
		cols[name] = true
	}
	return cols, rows.Err()
}
