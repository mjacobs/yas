// Package postgres implements the server's durable source-of-record store on
// Postgres via pgx. It is the cross-machine merge point: pushed records are
// upserted by id (last-writer-wins on the mutable fields), and every insert or
// update assigns a fresh monotonic seq (via the schema's default + trigger) so
// clients can pull everything after their cursor — including finalized and
// tombstoned rows.
package postgres

import (
	"context"
	_ "embed"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mjacobs/yas/internal/record"
)

//go:embed schema.sql
var schema string

// DB is the Postgres-backed sync store.
type DB struct {
	pool *pgxpool.Pool
}

// Open connects to dsn and applies the schema idempotently.
func Open(ctx context.Context, dsn string) (*DB, error) {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, err
	}
	if err := applySchema(ctx, pool); err != nil {
		pool.Close()
		return nil, err
	}
	return &DB{pool: pool}, nil
}

// schemaApplyTimeout bounds the schema statements themselves, detached from
// the caller's (deliberately short) connect deadline: index builds on a
// populated database — the trigram GIN index in particular — can legitimately
// take minutes, and dying at a fast connect deadline mid-CREATE INDEX would
// leave a restart-supervised server crash-looping without ever finishing.
// 10 minutes is deep headroom over any homelab-scale build while still
// guaranteeing a wedged mid-migration connection eventually fails.
const schemaApplyTimeout = 10 * time.Minute

// applySchema runs the multi-statement schema via the simple protocol (the
// extended protocol pgx uses for Exec rejects multiple statements). Acquiring
// the connection runs under the caller's ctx — that is the fast "is the DB
// reachable" probe — while the statements run under schemaApplyTimeout.
func applySchema(ctx context.Context, pool *pgxpool.Pool) error {
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()
	sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), schemaApplyTimeout)
	defer cancel()
	if _, err := conn.Conn().PgConn().Exec(sctx, schema).ReadAll(); err != nil {
		return fmt.Errorf("apply schema: %w", err)
	}
	return nil
}

// Close releases the connection pool.
func (d *DB) Close() error { d.pool.Close(); return nil }

// Note: INSERT ... ON CONFLICT DO UPDATE evaluates the seq column's
// DEFAULT nextval on the insert attempt AND fires the bump-seq trigger on the
// update, so an upsert that becomes an update consumes two seq values. seq stays
// monotonic and the row gets the higher value, so cursor-based pull is correct;
// the skipped value is a harmless gap.
const upsertSQL = `
INSERT INTO records
    (id, command, cwd, hostname, session, shell, username, exit_code, start_time, duration_ms, created_at, deleted, executor, corr_id)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)
ON CONFLICT (id) DO UPDATE SET
    exit_code   = EXCLUDED.exit_code,
    duration_ms = EXCLUDED.duration_ms,
    deleted     = records.deleted OR EXCLUDED.deleted`

// Put upserts records by id in one transaction. On conflict only the mutable
// fields (exit_code, duration_ms, deleted) change; the bump-seq trigger assigns
// a new seq so the change re-pulls. deleted is monotonic (OR, never flips back
// to false): a tombstone is sticky, so a later live write racing a delete can't
// resurrect the record (a38z).
func (d *DB) Put(ctx context.Context, recs ...record.Record) error {
	if len(recs) == 0 {
		return nil
	}
	tx, err := d.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	for _, r := range recs {
		if err := r.Validate(); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, upsertSQL,
			r.ID, r.Command, r.CWD, r.Hostname, r.Session, r.Shell, r.Username,
			r.ExitCode, r.StartTime, r.DurationMS, r.CreatedAt, r.Deleted, r.Executor, r.CorrID,
		); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

// HighSeq returns the highest seq currently assigned (0 if empty).
func (d *DB) HighSeq(ctx context.Context) (int64, error) {
	var seq int64
	err := d.pool.QueryRow(ctx, `SELECT COALESCE(max(seq), 0) FROM records`).Scan(&seq)
	return seq, err
}

const sinceSQL = `
SELECT id::text, command, cwd, hostname, session, shell, username, exit_code, start_time, duration_ms, created_at, deleted, COALESCE(executor,''), COALESCE(corr_id,''), seq
FROM records WHERE seq > $1 ORDER BY seq ASC LIMIT $2`

// Since returns up to limit records with seq greater than seq, ordered by seq
// ascending, plus the highest seq returned (or seq if none). Unlike the local
// query path it does NOT exclude tombstones — deletions must propagate.
func (d *DB) Since(ctx context.Context, seq int64, limit int) ([]record.Record, int64, error) {
	rows, err := d.pool.Query(ctx, sinceSQL, seq, limit)
	if err != nil {
		return nil, seq, err
	}
	defer rows.Close()

	out := []record.Record{}
	next := seq
	for rows.Next() {
		var (
			r    record.Record
			exit *int
			dur  *int64
			rseq int64
		)
		if err := rows.Scan(
			&r.ID, &r.Command, &r.CWD, &r.Hostname, &r.Session, &r.Shell, &r.Username,
			&exit, &r.StartTime, &dur, &r.CreatedAt, &r.Deleted, &r.Executor, &r.CorrID, &rseq,
		); err != nil {
			return nil, seq, err
		}
		r.ExitCode = exit
		r.DurationMS = dur
		out = append(out, r)
		next = rseq // ordered ascending, so the last row carries the max seq
	}
	return out, next, rows.Err()
}
