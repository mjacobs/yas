package histimport_test

import (
	"strings"
	"testing"
	"time"

	"github.com/mjacobs/yas/internal/histimport"
	"github.com/mjacobs/yas/internal/record"
)

// A single zsh extended-history line becomes one record with the epoch as
// start_time, elapsed seconds as duration_ms, executor=human, and the host.
func TestParseZsh_ExtendedSingle(t *testing.T) {
	recs, err := histimport.ParseZsh(strings.NewReader(": 1771486905:3;git status\n"), "hostA")
	if err != nil {
		t.Fatalf("ParseZsh: %v", err)
	}
	if len(recs) != 1 {
		t.Fatalf("want 1 record, got %d", len(recs))
	}
	r := recs[0]
	if r.Command != "git status" {
		t.Errorf("command: got %q want %q", r.Command, "git status")
	}
	if !r.StartTime.Equal(time.Unix(1771486905, 0)) {
		t.Errorf("start_time: got %v want %v", r.StartTime, time.Unix(1771486905, 0))
	}
	if r.DurationMS == nil || *r.DurationMS != 3000 {
		t.Errorf("duration_ms: got %v want 3000 (elapsed 3s)", r.DurationMS)
	}
	if r.Executor != record.ExecutorHuman {
		t.Errorf("executor: got %q want %q", r.Executor, record.ExecutorHuman)
	}
	if r.Hostname != "hostA" {
		t.Errorf("hostname: got %q want hostA", r.Hostname)
	}
	if err := r.Validate(); err != nil {
		t.Errorf("imported record must be valid: %v", err)
	}
}

// A command with embedded newlines is stored by zsh as lines joined by a
// trailing backslash; the parser rejoins them into one record with real
// newlines.
func TestParseZsh_MultilineContinuation(t *testing.T) {
	in := ": 1771486905:0;for x in 1 2; do\\\n  echo $x\\\ndone\n"
	recs, err := histimport.ParseZsh(strings.NewReader(in), "h")
	if err != nil {
		t.Fatalf("ParseZsh: %v", err)
	}
	if len(recs) != 1 {
		t.Fatalf("multiline must be ONE record, got %d: %+v", len(recs), recs)
	}
	want := "for x in 1 2; do\n  echo $x\ndone"
	if recs[0].Command != want {
		t.Errorf("command:\n got %q\nwant %q", recs[0].Command, want)
	}
}

// Re-importing the same history yields identical ids (deterministic), so a
// second `yas import` upserts rather than duplicates.
func TestParseZsh_DeterministicIDs(t *testing.T) {
	in := ": 100:0;alpha\n: 200:0;beta\n"
	a, err := histimport.ParseZsh(strings.NewReader(in), "h")
	if err != nil {
		t.Fatalf("ParseZsh a: %v", err)
	}
	b, err := histimport.ParseZsh(strings.NewReader(in), "h")
	if err != nil {
		t.Fatalf("ParseZsh b: %v", err)
	}
	if len(a) != 2 || len(b) != 2 {
		t.Fatalf("want 2+2 records, got %d+%d", len(a), len(b))
	}
	for i := range a {
		if a[i].ID != b[i].ID {
			t.Errorf("entry %d id not deterministic: %q vs %q", i, a[i].ID, b[i].ID)
		}
		if a[i].ID == "" {
			t.Errorf("entry %d empty id", i)
		}
	}
	// Distinct entries get distinct ids.
	if a[0].ID == a[1].ID {
		t.Errorf("distinct commands must get distinct ids, both %q", a[0].ID)
	}
}

// Multiple distinct entries parse in file order; blank lines are skipped.
func TestParseZsh_MultipleAndBlanks(t *testing.T) {
	in := ": 100:0;one\n\n: 200:1;two\n: 300:0;three\n"
	recs, err := histimport.ParseZsh(strings.NewReader(in), "h")
	if err != nil {
		t.Fatalf("ParseZsh: %v", err)
	}
	got := []string{}
	for _, r := range recs {
		got = append(got, r.Command)
	}
	want := []string{"one", "two", "three"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("commands: got %v want %v", got, want)
	}
}

// A non-extended (plain) line without the `: ts:el;` marker is still imported
// as a command (best-effort) with a valid, nonzero start_time so Validate passes.
func TestParseZsh_PlainLineFallback(t *testing.T) {
	recs, err := histimport.ParseZsh(strings.NewReader("ls -la\n"), "h")
	if err != nil {
		t.Fatalf("ParseZsh: %v", err)
	}
	if len(recs) != 1 || recs[0].Command != "ls -la" {
		t.Fatalf("plain line: got %+v", recs)
	}
	if recs[0].StartTime.IsZero() {
		t.Errorf("plain line must get a nonzero synthetic start_time (Validate requires it)")
	}
	if err := recs[0].Validate(); err != nil {
		t.Errorf("plain imported record must be valid: %v", err)
	}
}
