// Package store defines the storage contract shared by the local SQLite replica
// (client) and the Postgres source-of-record (server). Concrete implementations
// live in the sqlite/ and postgres/ subpackages and are wired in M2/M4.
package store

import (
	"context"
	"time"

	"github.com/mjacobs/yas/internal/record"
)

// Query expresses a history search. Zero-valued fields are ignored, so an empty
// Query matches everything (most-recent-first by default).
type Query struct {
	ID         string    // exact record id (fetch a single record)
	ExcludeID  string    // omit this record id (e.g. the in-flight query command itself)
	Text       string    // full-text match against the command (and cwd)
	Host       string    // exact hostname filter
	CWD        string    // exact working-directory filter
	Session    string    // exact shell-session filter
	Since      time.Time // start_time >= Since
	Until      time.Time // start_time <  Until
	ExitCode   *int      // exact exit-code filter
	FailedOnly bool      // only finished commands with a non-zero exit code
	Executor   string    // exact executor match (who/what ran it)
	AgentsOnly bool      // only agent-run commands (executor set and != "human")
	HumansOnly bool      // only human-run commands (executor unset/empty/"human")
	Limit      int       // 0 -> implementation default
	Offset     int       // for pagination
	Reverse    bool      // false: newest first; true: oldest first

	// IncludeDeleted also returns tombstoned rows. Internal-only: the importer
	// needs to SEE deletions to honor them (never resurrect, never re-dirty a
	// synced tombstone). No client-facing surface (CLI/query API/MCP) sets it.
	IncludeDeleted bool
}

// ApplyExecutorToken maps a client-facing executor token onto the query. This
// is the single definition of the token vocabulary shared by the CLI
// (--executor), the query API (?executor=), and the MCP tools:
//
//	""            no executor filter
//	"$all-agent"  agent-run commands (executor set and != "human")
//	"$all-human"  human-run commands (executor unset/empty/"human")
//	"human"       alias for "$all-human" — bare "human" folds legacy rows
//	              recorded before executor tagging (NULL/empty) into the
//	              human class rather than exact-matching the string
//	anything else exact executor match
func (q *Query) ApplyExecutorToken(token string) {
	switch token {
	case "":
		// no executor filter
	case "$all-agent":
		q.AgentsOnly = true
	case "$all-human", "human":
		q.HumansOnly = true
	default:
		q.Executor = token
	}
}

// Store is the read/write surface both backends implement.
type Store interface {
	// Put upserts records by ID. The mutable fields (exit/duration) and the
	// Deleted tombstone are last-writer-wins; everything else is immutable.
	Put(ctx context.Context, recs ...record.Record) error
	// Search returns records matching q.
	Search(ctx context.Context, q Query) ([]record.Record, error)
	// Close releases the underlying handles.
	Close() error
}

// SyncSource is the server-side capability of streaming records by seq cursor.
type SyncSource interface {
	// Since returns up to limit records whose seq is greater than the cursor,
	// ordered by seq ascending, plus the highest seq returned.
	Since(ctx context.Context, seq int64, limit int) (recs []record.Record, next int64, err error)
}

// Cursor is the client-side persisted sync position.
type Cursor interface {
	// LastPulled returns the highest server seq already applied locally.
	LastPulled(ctx context.Context) (int64, error)
	// SetLastPulled advances the persisted cursor.
	SetLastPulled(ctx context.Context, seq int64) error
	// Unsynced returns local records not yet acknowledged by the server.
	Unsynced(ctx context.Context, limit int) ([]record.Record, error)
	// MarkSynced flags the given record IDs as pushed.
	MarkSynced(ctx context.Context, ids ...string) error
}
