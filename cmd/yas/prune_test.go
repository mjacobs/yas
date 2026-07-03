package main

import (
	"context"
	"testing"
	"time"

	"github.com/mjacobs/yas/internal/record"
	"github.com/mjacobs/yas/internal/store"
)

// skeletonRecord builds an "import skeleton": the exact shape a pre-h4t6
// importer left behind — a session-less row with NO exit code. This is the only
// signature doPruneLiveDupes may ever tombstone.
func skeletonRecord(cmd, host string, ts time.Time) record.Record {
	return record.Record{
		ID:        record.NewID(),
		Command:   cmd,
		Hostname:  host,
		Executor:  record.ExecutorHuman,
		StartTime: ts,
		CreatedAt: ts,
		// Session == "" AND ExitCode == nil -> the import skeleton signature.
	}
}

// A skeleton captured live gets tombstoned — but only under --yes. The dry run
// (apply=false) reports the count and mutates NOTHING; the apply run tombstones
// ONLY the skeleton, leaving the live capture intact.
func TestDoPruneLiveDupes_TombstonesSkeletonCoveredByLive(t *testing.T) {
	db := openTestStore(t)
	ctx := context.Background()

	// Live capture of `git status` at .470s, and an import skeleton for the same
	// command at the whole second the zsh history file stamps.
	live := liveRecord("git status", "hostZ", time.Unix(1771486905, 470_000_000))
	skel := skeletonRecord("git status", "hostZ", time.Unix(1771486905, 0))
	if err := db.Put(ctx, live, skel); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Dry run: reports 1, tombstones nothing (both rows still visible).
	n, err := doPruneLiveDupes(ctx, db, false)
	if err != nil {
		t.Fatalf("dry run: %v", err)
	}
	if n != 1 {
		t.Fatalf("dry-run count = %d, want 1", n)
	}
	if recs, _ := db.Search(ctx, store.Query{}); len(recs) != 2 {
		t.Fatalf("after dry run %d visible rows, want 2 (nothing mutated)", len(recs))
	}

	// Apply: tombstones ONLY the skeleton.
	n, err = doPruneLiveDupes(ctx, db, true)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if n != 1 {
		t.Fatalf("apply count = %d, want 1", n)
	}
	// The live row survives; the skeleton is gone from the default listing.
	recs, err := db.Search(ctx, store.Query{})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(recs) != 1 || recs[0].ID != live.ID {
		t.Fatalf("after apply visible rows = %d, want just the live capture %s", len(recs), live.ID)
	}
	// The skeleton is still present under IncludeDeleted, as a tombstone.
	all, err := db.Search(ctx, store.Query{IncludeDeleted: true})
	if err != nil {
		t.Fatalf("search all: %v", err)
	}
	var found *record.Record
	for i := range all {
		if all[i].ID == skel.ID {
			found = &all[i]
		}
	}
	if found == nil {
		t.Fatalf("skeleton %s vanished entirely", skel.ID)
	}
	if !found.Deleted {
		t.Fatalf("skeleton %s is not a tombstone: %+v", skel.ID, *found)
	}
}

// A skeleton with NO live peer is never tombstoned: prune only touches import
// skeletons that duplicate a real live capture.
func TestDoPruneLiveDupes_SkeletonWithoutLivePeerSurvives(t *testing.T) {
	db := openTestStore(t)
	ctx := context.Background()

	skel := skeletonRecord("orphan --cmd", "hostZ", time.Unix(1771486905, 0))
	if err := db.Put(ctx, skel); err != nil {
		t.Fatalf("seed: %v", err)
	}

	if n, err := doPruneLiveDupes(ctx, db, false); err != nil || n != 0 {
		t.Fatalf("dry run n=%d err=%v, want n=0", n, err)
	}
	// Even an apply run leaves it alone and visible.
	if n, err := doPruneLiveDupes(ctx, db, true); err != nil || n != 0 {
		t.Fatalf("apply n=%d err=%v, want n=0", n, err)
	}
	if recs, _ := db.Search(ctx, store.Query{}); len(recs) != 1 {
		t.Fatalf("orphan skeleton has %d visible rows, want 1 (untouched)", len(recs))
	}
}

// A session-less row that DID capture an exit code is NOT an import skeleton
// (an import never sets exit) — it is never a candidate, even with a live peer.
func TestDoPruneLiveDupes_SessionlessWithExitIsNotASkeleton(t *testing.T) {
	db := openTestStore(t)
	ctx := context.Background()

	exit := 0
	ts := time.Unix(1771486905, 0)
	sessionless := record.Record{
		ID:        record.NewID(),
		Command:   "make build",
		Hostname:  "hostZ",
		Executor:  record.ExecutorHuman,
		ExitCode:  &exit, // <- exit set: NOT the skeleton signature
		StartTime: ts,
		CreatedAt: ts,
	}
	// A live peer exists, so only the exit-nil signature keeps it safe.
	live := liveRecord("make build", "hostZ", ts)
	if err := db.Put(ctx, sessionless, live); err != nil {
		t.Fatalf("seed: %v", err)
	}

	if n, err := doPruneLiveDupes(ctx, db, true); err != nil || n != 0 {
		t.Fatalf("apply n=%d err=%v, want n=0 (sessionless+exit is not a skeleton)", n, err)
	}
	// The sessionless-with-exit row survives.
	recs, _ := db.Search(ctx, store.Query{})
	var stillThere bool
	for _, r := range recs {
		if r.ID == sessionless.ID {
			stillThere = true
		}
	}
	if !stillThere {
		t.Fatal("sessionless-with-exit row was tombstoned; it is not an import skeleton")
	}
}

// The coverage window is ±1s: a live peer exactly +1s away covers the skeleton;
// +2s does not (or distinct same-command runs would be swallowed).
func TestDoPruneLiveDupes_WindowIsOneSecond(t *testing.T) {
	db := openTestStore(t)
	ctx := context.Background()

	base := time.Unix(1771486905, 0)
	// cmdA: live peer exactly +1s -> covered.
	skelA := skeletonRecord("cmdA", "hostZ", base)
	liveA := liveRecord("cmdA", "hostZ", base.Add(time.Second))
	// cmdB: live peer +2s -> NOT covered.
	skelB := skeletonRecord("cmdB", "hostZ", base)
	liveB := liveRecord("cmdB", "hostZ", base.Add(2*time.Second))
	if err := db.Put(ctx, skelA, liveA, skelB, liveB); err != nil {
		t.Fatalf("seed: %v", err)
	}

	if n, err := doPruneLiveDupes(ctx, db, false); err != nil || n != 1 {
		t.Fatalf("count n=%d err=%v, want n=1 (only the +1s skeleton is covered)", n, err)
	}
}

// A live capture on another host never covers this host's skeleton.
func TestDoPruneLiveDupes_CoverageIsPerHost(t *testing.T) {
	db := openTestStore(t)
	ctx := context.Background()

	ts := time.Unix(1771486905, 0)
	skel := skeletonRecord("git push", "hostZ", ts)
	live := liveRecord("git push", "otherhost", ts) // same command+second, other host
	if err := db.Put(ctx, skel, live); err != nil {
		t.Fatalf("seed: %v", err)
	}

	if n, err := doPruneLiveDupes(ctx, db, false); err != nil || n != 0 {
		t.Fatalf("count n=%d err=%v, want n=0 (different host is not coverage)", n, err)
	}
}

// A live capture that was later tombstoned still covers its command: deletions
// are honored, not resurrected — exactly as importCoverage's set treats them
// (the shared-helper invariant). So a skeleton whose ONLY live peer is Deleted
// is still pruned; leaving it would keep a visible skeleton duplicate of a
// redacted command.
func TestDoPruneLiveDupes_DeletedLivePeerStillCovers(t *testing.T) {
	db := openTestStore(t)
	ctx := context.Background()

	// The live capture, then redacted (tombstoned) — its command should still
	// cover the import skeleton.
	live := liveRecord("export TOKEN=oops", "hostZ", time.Unix(1771486905, 470_000_000))
	live.Deleted = true
	skel := skeletonRecord("export TOKEN=oops", "hostZ", time.Unix(1771486905, 0))
	if err := db.Put(ctx, live, skel); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Dry run counts the skeleton even though its only peer is deleted.
	if n, err := doPruneLiveDupes(ctx, db, false); err != nil || n != 1 {
		t.Fatalf("dry run n=%d err=%v, want n=1 (deleted live peer still covers)", n, err)
	}
	// Apply tombstones it, leaving no visible duplicate of the redacted command.
	if n, err := doPruneLiveDupes(ctx, db, true); err != nil || n != 1 {
		t.Fatalf("apply n=%d err=%v, want n=1", n, err)
	}
	if recs, _ := db.Search(ctx, store.Query{}); len(recs) != 0 {
		t.Fatalf("after apply %d visible rows, want 0 (both the redacted live row and its skeleton gone)", len(recs))
	}
}

// A second apply run tombstones 0: already-deleted skeletons are skipped, so the
// prune is idempotent.
func TestDoPruneLiveDupes_Idempotent(t *testing.T) {
	db := openTestStore(t)
	ctx := context.Background()

	live := liveRecord("git status", "hostZ", time.Unix(1771486905, 470_000_000))
	skel := skeletonRecord("git status", "hostZ", time.Unix(1771486905, 0))
	if err := db.Put(ctx, live, skel); err != nil {
		t.Fatalf("seed: %v", err)
	}

	if n, err := doPruneLiveDupes(ctx, db, true); err != nil || n != 1 {
		t.Fatalf("first apply n=%d err=%v, want n=1", n, err)
	}
	if n, err := doPruneLiveDupes(ctx, db, true); err != nil || n != 0 {
		t.Fatalf("second apply n=%d err=%v, want n=0 (idempotent)", n, err)
	}
}
