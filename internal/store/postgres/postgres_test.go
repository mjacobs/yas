package postgres_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/mjacobs/yas/internal/record"
	"github.com/mjacobs/yas/internal/store/postgres"
)

var base = time.UnixMilli(1_700_000_000_000)

func ptr[T any](v T) *T { return &v }

const (
	idA = "019ef273-4ad8-76d8-aaaa-00000000000a"
	idB = "019ef273-4ad8-76d8-aaaa-00000000000b"
	idC = "019ef273-4ad8-76d8-aaaa-00000000000c"
)

// freshDB opens the test database and truncates it. Skips unless
// YAS_TEST_DATABASE_URL is set (e.g. a throwaway Postgres container).
func freshDB(t *testing.T) (*postgres.DB, context.Context) {
	t.Helper()
	dsn := os.Getenv("YAS_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set YAS_TEST_DATABASE_URL to run Postgres integration tests")
	}
	ctx := context.Background()
	db, err := postgres.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer conn.Close(ctx)
	if _, err := conn.Exec(ctx, "TRUNCATE records; ALTER SEQUENCE records_seq RESTART WITH 1;"); err != nil {
		t.Fatalf("reset: %v", err)
	}
	return db, ctx
}

func rec(id, cmd string) record.Record {
	return record.Record{ID: id, Command: cmd, StartTime: base, CreatedAt: base}
}

func TestPostgres_PutSinceSeq(t *testing.T) {
	db, ctx := freshDB(t)
	if err := db.Put(ctx, rec(idA, "git status"), rec(idB, "ls")); err != nil {
		t.Fatalf("Put: %v", err)
	}

	recs, next, err := db.Since(ctx, 0, 10)
	if err != nil {
		t.Fatalf("Since: %v", err)
	}
	if len(recs) != 2 {
		t.Fatalf("want 2 records, got %d", len(recs))
	}
	if recs[0].ID != idA || recs[1].ID != idB {
		t.Errorf("order/ids: %+v", recs) // seq asc == insert order
	}
	if recs[0].Command != "git status" {
		t.Errorf("command round-trip: %q", recs[0].Command)
	}
	if next != 2 {
		t.Errorf("next: got %d want 2", next)
	}
	if high, _ := db.HighSeq(ctx); high != 2 {
		t.Errorf("HighSeq: got %d want 2", high)
	}
}

// The bump-seq trigger is the heart of sync: a finish/redaction update must get
// a new seq so a client that already pulled the original re-pulls the change.
func TestPostgres_UpsertBumpsSeqAndRepulls(t *testing.T) {
	db, ctx := freshDB(t)
	if err := db.Put(ctx, rec(idA, "sleep 1"), rec(idB, "ls")); err != nil { // seq 1, 2
		t.Fatalf("seed: %v", err)
	}

	fin := rec(idA, "TAMPERED") // command must stay immutable
	fin.ExitCode = ptr(7)
	fin.DurationMS = ptr(int64(1234))
	if err := db.Put(ctx, fin); err != nil { // UPDATE -> trigger bumps seq to 3
		t.Fatalf("finish Put: %v", err)
	}

	recs, next, err := db.Since(ctx, 2, 10) // pull from the client's old cursor
	if err != nil {
		t.Fatalf("Since: %v", err)
	}
	if len(recs) != 1 || recs[0].ID != idA {
		t.Fatalf("updated record must re-pull, got %+v", recs)
	}
	if recs[0].Command != "sleep 1" {
		t.Errorf("command must be immutable: got %q", recs[0].Command)
	}
	if recs[0].ExitCode == nil || *recs[0].ExitCode != 7 {
		t.Errorf("exit_code not applied: %v", recs[0].ExitCode)
	}
	if recs[0].DurationMS == nil || *recs[0].DurationMS != 1234 {
		t.Errorf("duration_ms not applied: %v", recs[0].DurationMS)
	}
	// The exact seq may skip values (INSERT ... ON CONFLICT consumes one nextval
	// on the insert attempt and the trigger another on the update); what matters
	// is that it advanced past the old cursor and equals the high-water mark.
	high, err := db.HighSeq(ctx)
	if err != nil {
		t.Fatalf("HighSeq: %v", err)
	}
	if next <= 2 {
		t.Errorf("updated seq must advance past the old cursor 2, got %d", next)
	}
	if next != high {
		t.Errorf("next (%d) should equal HighSeq (%d)", next, high)
	}
}

// Sync must carry tombstones (the local query path hides them; sync does not).
func TestPostgres_TombstonePropagates(t *testing.T) {
	db, ctx := freshDB(t)
	dead := rec(idC, "secret")
	dead.Deleted = true
	if err := db.Put(ctx, dead); err != nil {
		t.Fatalf("Put: %v", err)
	}
	recs, _, err := db.Since(ctx, 0, 10)
	if err != nil {
		t.Fatalf("Since: %v", err)
	}
	if len(recs) != 1 || !recs[0].Deleted {
		t.Fatalf("tombstone must propagate via sync, got %+v", recs)
	}
}

// deleted is monotonic on the hub too: a later live write racing a delete must
// not resurrect the tombstone. Guards a38z.
func TestPostgres_DeletedIsMonotonic(t *testing.T) {
	db, ctx := freshDB(t)
	r := rec(idC, "secret")
	if err := db.Put(ctx, r); err != nil { // live
		t.Fatalf("put live: %v", err)
	}
	dead := r
	dead.Deleted = true
	if err := db.Put(ctx, dead); err != nil { // tombstone
		t.Fatalf("put tombstone: %v", err)
	}
	exit := 0
	finished := r // Deleted=false again, now finished
	finished.ExitCode = &exit
	if err := db.Put(ctx, finished); err != nil { // later live write racing the delete
		t.Fatalf("put finish: %v", err)
	}
	recs, _, err := db.Since(ctx, 0, 10)
	if err != nil {
		t.Fatalf("Since: %v", err)
	}
	if len(recs) != 1 || !recs[0].Deleted {
		t.Fatalf("tombstone must survive a later live write (sticky), got %+v", recs)
	}
}

func TestPostgres_ExecutorCorrIDRoundTrip(t *testing.T) {
	db, ctx := freshDB(t)
	r := record.Record{ID: idA, Command: "deploy", StartTime: base, CreatedAt: base, Executor: "claude-code", CorrID: "sess-9"}
	if err := db.Put(ctx, r); err != nil {
		t.Fatalf("Put: %v", err)
	}
	recs, _, err := db.Since(ctx, 0, 10)
	if err != nil {
		t.Fatalf("Since: %v", err)
	}
	if len(recs) != 1 {
		t.Fatalf("want 1, got %d", len(recs))
	}
	if recs[0].Executor != "claude-code" || recs[0].CorrID != "sess-9" {
		t.Errorf("round-trip: got executor=%q corr_id=%q", recs[0].Executor, recs[0].CorrID)
	}
}
