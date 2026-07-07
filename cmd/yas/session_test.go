package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/mjacobs/yas/internal/record"
	"github.com/mjacobs/yas/internal/store"
)

func TestShortSession(t *testing.T) {
	// empty -> empty (imported/sessionless rows)
	if got := shortSession(""); got != "" {
		t.Errorf("shortSession(\"\") = %q, want \"\"", got)
	}
	// deterministic + fixed width 7, base36 charset
	id := "hostZ-348721-1751049600"
	a, b := shortSession(id), shortSession(id)
	if a != b {
		t.Errorf("not deterministic: %q != %q", a, b)
	}
	if len(a) != 7 {
		t.Errorf("width = %d, want 7 (%q)", len(a), a)
	}
	// Golden value pins the exact algorithm (fnv1a64 -> mod 36^7 -> base36 ->
	// pad7): the property checks above still pass if the modulus or hash variant
	// silently drifts, this catches that.
	if a != "olwnzfl" {
		t.Errorf("shortSession(%q) = %q, want olwnzfl (algorithm drift?)", id, a)
	}
	for _, r := range a {
		isDigit := r >= '0' && r <= '9'
		isLower := r >= 'a' && r <= 'z'
		if !isDigit && !isLower {
			t.Errorf("non-base36 rune %q in %q", r, a)
		}
	}
	// distinct ids -> distinct tokens (no trivial collision on close inputs)
	if shortSession("hostZ-348721-1751049600") == shortSession("hostZ-348721-1751049601") {
		t.Errorf("adjacent ids collided")
	}
}

func TestDoSession_ResolveByTokenAndFullID(t *testing.T) {
	db := openTestStore(t)
	ctx := context.Background()
	full := "hostZ-777-1751049600"
	tok := shortSession(full)
	b := time.UnixMilli(1_700_000_000_000)
	if err := db.Put(ctx,
		record.Record{ID: "1", Command: "ssh server1", Session: full, StartTime: b, CreatedAt: b},
		record.Record{ID: "2", Command: "docker ps", Session: full, StartTime: b.Add(time.Minute), CreatedAt: b},
		record.Record{ID: "3", Command: "other", Session: "zzz-1-1", StartTime: b, CreatedAt: b},
	); err != nil {
		t.Fatalf("seed: %v", err)
	}
	for _, arg := range []string{tok, full} {
		var buf bytes.Buffer
		if err := doSession(ctx, db, arg, historyOpts{showTime: false}, &buf, time.UTC); err != nil {
			t.Fatalf("doSession(%q): %v", arg, err)
		}
		out := buf.String()
		// oldest-first: ssh server1 (line 1) before docker ps (line 2); header present; other session excluded
		if !strings.Contains(out, full) || !strings.Contains(out, "ssh server1") || !strings.Contains(out, "docker ps") {
			t.Errorf("doSession(%q) missing content:\n%s", arg, out)
		}
		if strings.Contains(out, "other") {
			t.Errorf("doSession(%q) leaked another session:\n%s", arg, out)
		}
		if i, j := strings.Index(out, "ssh server1"), strings.Index(out, "docker ps"); i > j {
			t.Errorf("not oldest-first:\n%s", out)
		}
	}
}

func TestDoSession_UnknownTokenErrors(t *testing.T) {
	db := openTestStore(t)
	if err := doSession(context.Background(), db, "zzzzzzz", historyOpts{}, &bytes.Buffer{}, time.UTC); err == nil {
		t.Errorf("expected error for unknown token")
	}
}

func TestParseSessionArgs_RequiresArg(t *testing.T) {
	if _, _, err := parseSessionArgs(nil); err == nil {
		t.Errorf("expected usage error with no arg")
	}
}

func TestParseSessionArgs_BlankArgIsError(t *testing.T) {
	for _, arg := range []string{"", "   ", "\t"} {
		if _, _, err := parseSessionArgs([]string{arg}); err == nil {
			t.Errorf("parseSessionArgs(%q): expected usage error for blank arg, got nil", arg)
		}
	}
}

func TestParseSessionArgs_TooManyArgsIsError(t *testing.T) {
	if _, _, err := parseSessionArgs([]string{"a", "b"}); err == nil {
		t.Errorf("parseSessionArgs(a, b): expected error for surplus args, got nil")
	}
}

func TestParseSessionArgs_DurationColumn(t *testing.T) {
	t.Run("--no-duration parses", func(t *testing.T) {
		_, opts, err := parseSessionArgs([]string{"--no-duration", "tok"})
		if err != nil {
			t.Fatal(err)
		}
		if opts.showDuration {
			t.Fatal("expected showDuration=false with --no-duration")
		}
	})
	t.Run("default shows duration", func(t *testing.T) {
		_, opts, err := parseSessionArgs([]string{"tok"})
		if err != nil {
			t.Fatal(err)
		}
		if !opts.showDuration {
			t.Fatal("expected showDuration=true by default")
		}
	})
}

// fakeSessionStore is a minimal sessionStore that lets tests control what
// Sessions() returns while treating every Search as "not a full id".
type fakeSessionStore struct {
	sessions []string
	records  []record.Record // returned only when Session matches exactly
}

func (f *fakeSessionStore) Search(_ context.Context, q store.Query) ([]record.Record, error) {
	// Return only records whose Session equals q.Session exactly, so the
	// resolver sees them as full-id hits. Non-nil empty slice upholds the
	// store's empty-results-as-[] contract.
	out := []record.Record{}
	for _, r := range f.records {
		if q.Session != "" && r.Session == q.Session {
			out = append(out, r)
			if q.Limit > 0 && len(out) >= q.Limit {
				break
			}
		}
	}
	return out, nil
}

func (f *fakeSessionStore) Sessions(_ context.Context) ([]string, error) {
	return f.sessions, nil
}

// findCollisionPair returns two distinct session-id strings whose shortSession
// tokens are identical. It uses birthday-paradox iteration and is fast (<1 s).
func findCollisionPair() (string, string) {
	seen := make(map[string]string, 1<<19)
	// Birthday-paradox expectation over the 36^7 space is ~330k iterations; cap
	// well above that so a regression in shortSession fails loudly instead of
	// hanging CI.
	for i := 0; i < 1<<24; i++ {
		id := fmt.Sprintf("collision-candidate-%d", i)
		tok := shortSession(id)
		if prev, ok := seen[tok]; ok {
			return prev, id
		}
		seen[tok] = id
	}
	panic("findCollisionPair: no collision within 1<<24 candidates — shortSession regressed?")
}

func TestDoSession_AmbiguousToken(t *testing.T) {
	a, b := findCollisionPair()
	tok := shortSession(a)
	if tok != shortSession(b) {
		t.Fatalf("findCollisionPair bug: tokens differ (%q vs %q)", tok, shortSession(b))
	}

	st := &fakeSessionStore{sessions: []string{a, b}}
	err := doSession(context.Background(), st, tok, historyOpts{}, &bytes.Buffer{}, time.UTC)
	if err == nil {
		t.Fatal("expected ambiguous error, got nil")
	}
	msg := err.Error()
	if !strings.Contains(msg, "ambiguous") {
		t.Errorf("error missing 'ambiguous': %s", msg)
	}
	if !strings.Contains(msg, a) {
		t.Errorf("error missing candidate %q: %s", a, msg)
	}
	if !strings.Contains(msg, b) {
		t.Errorf("error missing candidate %q: %s", b, msg)
	}
}

func TestDoSession_JSONOutput(t *testing.T) {
	db := openTestStore(t)
	ctx := context.Background()
	full := "hostZ-json-test-1751049600"
	now := time.UnixMilli(1_700_000_001_000)
	if err := db.Put(ctx,
		record.Record{ID: "j1", Command: "echo hello", Session: full, StartTime: now, CreatedAt: now},
		record.Record{ID: "j2", Command: "echo world", Session: full, StartTime: now.Add(time.Second), CreatedAt: now},
	); err != nil {
		t.Fatalf("seed: %v", err)
	}
	var buf bytes.Buffer
	opts := historyOpts{asJSON: true}
	if err := doSession(ctx, db, full, opts, &buf, time.UTC); err != nil {
		t.Fatalf("doSession --json: %v", err)
	}
	var resp struct {
		Records []struct {
			Command string `json:"command"`
			Session string `json:"session"`
		} `json:"records"`
	}
	if err := json.Unmarshal(buf.Bytes(), &resp); err != nil {
		t.Fatalf("invalid JSON output: %v\n%s", err, buf.String())
	}
	if len(resp.Records) != 2 {
		t.Errorf("got %d records, want 2", len(resp.Records))
	}
	for _, r := range resp.Records {
		if r.Session != full {
			t.Errorf("record session %q != %q", r.Session, full)
		}
	}
	// short token must NOT appear in the JSON surface
	tok := shortSession(full)
	if strings.Contains(buf.String(), tok) {
		t.Errorf("short token %q leaked into JSON output", tok)
	}
}

// yas session must show the WHOLE session, not just the store's default page.
// A long-lived shell can run more than the default 100; because the listing is
// oldest-first (Reverse orders ASC), an inherited cap would keep the OLDEST 100
// and silently drop the most recent commands — the opposite of the neighbours
// the command exists to surface.
func TestDoSession_FetchesWholeSession(t *testing.T) {
	db := openTestStore(t)
	ctx := context.Background()
	full := "hostZ-long-1751049600"
	b := time.UnixMilli(1_700_000_000_000)
	const n = 150
	recs := make([]record.Record, n)
	for i := 0; i < n; i++ {
		recs[i] = record.Record{
			ID:        fmt.Sprintf("r%04d", i),
			Command:   fmt.Sprintf("cmd-%03d", i),
			Session:   full,
			StartTime: b.Add(time.Duration(i) * time.Minute),
			CreatedAt: b,
		}
	}
	if err := db.Put(ctx, recs...); err != nil {
		t.Fatalf("seed: %v", err)
	}
	var buf bytes.Buffer
	if err := doSession(ctx, db, full, historyOpts{showTime: false}, &buf, time.UTC); err != nil {
		t.Fatalf("doSession: %v", err)
	}
	out := buf.String()
	header := strings.SplitN(out, "\n", 2)[0]
	// header must report the full count, not the capped default page
	if !strings.Contains(out, fmt.Sprintf("· %d commands", n)) {
		t.Errorf("header should report %d commands, got: %q", n, header)
	}
	// the newest command must be present (it's the one a cap would drop)
	if !strings.Contains(out, "cmd-149") {
		t.Errorf("newest command cmd-149 missing — session truncated to the oldest page:\n%s", header)
	}
	// the oldest must also be present
	if !strings.Contains(out, "cmd-000") {
		t.Errorf("oldest command cmd-000 missing")
	}
}
