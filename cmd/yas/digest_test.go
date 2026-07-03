package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/mjacobs/yas/internal/record"
)

// drec builds a window record for the digest tests. A fresh UUIDv7 id keeps the
// deterministic tiebreak meaningful; the tests use distinct timestamps so it
// never actually decides ordering.
func drec(host, cwd, cmd string, exit *int, t time.Time) record.Record {
	return record.Record{
		ID:        record.NewID(),
		Hostname:  host,
		CWD:       cwd,
		Command:   cmd,
		ExitCode:  exit,
		StartTime: t,
	}
}

func TestBuildDigest_GroupsCountAndSort(t *testing.T) {
	base := time.Date(2026, 7, 2, 10, 0, 0, 0, time.UTC)
	since := time.Date(2026, 7, 2, 0, 0, 0, 0, time.UTC)
	until := time.Date(2026, 7, 3, 0, 0, 0, 0, time.UTC)
	recs := []record.Record{
		drec("b", "/x", "cmd1", ptr(0), base),
		drec("a", "/y", "fail1", ptr(1), base.Add(1*time.Minute)),
		drec("a", "/y", "fail1", ptr(2), base.Add(2*time.Minute)), // dup command, later
		drec("a", "/y", "fail2", ptr(1), base.Add(3*time.Minute)),
		drec("a", "/x", "ok", ptr(0), base),
		drec("b", "/x", "boom", ptr(127), base.Add(5*time.Minute)),
	}

	d := buildDigest(recs, since, until)

	if len(d.Groups) != 3 {
		t.Fatalf("want 3 groups, got %d: %+v", len(d.Groups), d.Groups)
	}
	// Deterministic order: host asc, then cwd asc.
	wantKeys := [][2]string{{"a", "/x"}, {"a", "/y"}, {"b", "/x"}}
	for i, k := range wantKeys {
		if d.Groups[i].Host != k[0] || d.Groups[i].CWD != k[1] {
			t.Fatalf("group %d: want %v, got (%s,%s)", i, k, d.Groups[i].Host, d.Groups[i].CWD)
		}
	}
	// (a,/x): one passing command.
	if g := d.Groups[0]; g.Count != 1 || g.Failures != 0 || len(g.FailedCommands) != 0 {
		t.Fatalf("(a,/x) unexpected: %+v", g)
	}
	// (a,/y): 3 commands, all failing; distinct failing commands most-recent-first
	// are fail2 (t+3) then fail1 (t+2, deduped with t+1).
	if g := d.Groups[1]; g.Count != 3 || g.Failures != 3 {
		t.Fatalf("(a,/y) counts unexpected: %+v", g)
	}
	if got := d.Groups[1].FailedCommands; len(got) != 2 || got[0] != "fail2" || got[1] != "fail1" {
		t.Fatalf("(a,/y) failing commands want [fail2 fail1], got %v", got)
	}
	// (b,/x): 2 commands, one failing.
	if g := d.Groups[2]; g.Count != 2 || g.Failures != 1 || len(g.FailedCommands) != 1 || g.FailedCommands[0] != "boom" {
		t.Fatalf("(b,/x) unexpected: %+v", g)
	}
}

func TestBuildDigest_ExitCodeSemantics(t *testing.T) {
	base := time.Date(2026, 7, 2, 10, 0, 0, 0, time.UTC)
	since := base.Add(-time.Hour)
	until := base.Add(time.Hour)
	recs := []record.Record{
		drec("h", "/d", "unfinished", nil, base),                 // nil exit: not a failure
		drec("h", "/d", "ok", ptr(0), base.Add(time.Minute)),     // exit 0: not a failure
		drec("h", "/d", "boom", ptr(2), base.Add(2*time.Minute)), // non-zero: failure
	}

	d := buildDigest(recs, since, until)

	if len(d.Groups) != 1 {
		t.Fatalf("want 1 group, got %d", len(d.Groups))
	}
	g := d.Groups[0]
	if g.Count != 3 {
		t.Fatalf("want count 3, got %d", g.Count)
	}
	if g.Failures != 1 {
		t.Fatalf("nil exit and exit 0 must not be failures; want 1 failure, got %d", g.Failures)
	}
	if len(g.FailedCommands) != 1 || g.FailedCommands[0] != "boom" {
		t.Fatalf("want [boom], got %v", g.FailedCommands)
	}
}

func TestBuildDigest_EmptyWindowGroupsNonNil(t *testing.T) {
	since := time.Date(2026, 7, 2, 0, 0, 0, 0, time.UTC)
	until := since.Add(24 * time.Hour)

	d := buildDigest(nil, since, until)

	if d.Groups == nil {
		t.Fatal("Groups must be non-nil (serializes as [] not null)")
	}
	if len(d.Groups) != 0 {
		t.Fatalf("want 0 groups, got %d", len(d.Groups))
	}
}

func TestBuildDigest_WindowFilteringUntilExclusive(t *testing.T) {
	base := time.Date(2026, 7, 2, 10, 0, 0, 0, time.UTC)
	since := base
	until := base.Add(10 * time.Minute)
	recs := []record.Record{
		drec("h", "/d", "before", ptr(0), base.Add(-time.Minute)),  // excluded (< since)
		drec("h", "/d", "atstart", ptr(0), base),                   // included (Since inclusive)
		drec("h", "/d", "inside", ptr(0), base.Add(5*time.Minute)), // included
		drec("h", "/d", "atend", ptr(0), until),                    // excluded (Until exclusive)
	}

	d := buildDigest(recs, since, until)

	if len(d.Groups) != 1 {
		t.Fatalf("want 1 group, got %d", len(d.Groups))
	}
	if d.Groups[0].Count != 2 {
		t.Fatalf("want count 2 (atstart, inside), got %d", d.Groups[0].Count)
	}
}

func TestBuildDigest_FailedCommandsCapAndTruncate(t *testing.T) {
	base := time.Date(2026, 7, 2, 10, 0, 0, 0, time.UTC)
	since := base.Add(-time.Hour)
	until := base.Add(2 * time.Hour)

	var recs []record.Record
	// 15 distinct failing commands -> the sample is capped at maxFailedPerGroup.
	for i := 0; i < 15; i++ {
		recs = append(recs, drec("h", "/d", fmt.Sprintf("fail-%02d", i), ptr(1), base.Add(time.Duration(i)*time.Minute)))
	}
	// One very long, latest failing command -> sampled first and truncated.
	long := strings.Repeat("x", maxFailedCommandRunes+50)
	recs = append(recs, drec("h", "/d", long, ptr(1), base.Add(90*time.Minute)))

	d := buildDigest(recs, since, until)

	g := d.Groups[0]
	if g.Failures != 16 {
		t.Fatalf("Failures must count every failing record (16), got %d", g.Failures)
	}
	if len(g.FailedCommands) != maxFailedPerGroup {
		t.Fatalf("sample must cap at %d, got %d", maxFailedPerGroup, len(g.FailedCommands))
	}
	if !strings.HasSuffix(g.FailedCommands[0], "…") {
		t.Fatalf("latest (long) command should be truncated with an ellipsis, got %q", g.FailedCommands[0])
	}
	// The ellipsis counts toward the budget: the result is exactly the cap.
	if n := utf8.RuneCountInString(g.FailedCommands[0]); n != maxFailedCommandRunes {
		t.Fatalf("truncated command want %d runes, got %d", maxFailedCommandRunes, n)
	}
}

func TestRenderDigest_JSONEmptyIsBracketsNotNull(t *testing.T) {
	since := time.Date(2026, 7, 2, 0, 0, 0, 0, time.UTC)
	until := time.Date(2026, 7, 2, 14, 33, 0, 0, time.UTC)
	d := buildDigest(nil, since, until)

	var buf bytes.Buffer
	if err := renderDigest(&buf, d, true); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.Contains(out, `"groups": []`) {
		t.Fatalf("empty groups must serialize as [] not null:\n%s", out)
	}
	if strings.Contains(out, "null") {
		t.Fatalf("digest JSON must never contain null:\n%s", out)
	}

	var env digestEnvelope
	if err := json.Unmarshal(buf.Bytes(), &env); err != nil {
		t.Fatalf("round-trip failed: %v\n%s", err, out)
	}
	if env.Since != "2026-07-02T00:00:00Z" || env.Until != "2026-07-02T14:33:00Z" {
		t.Fatalf("since/until must be RFC3339: got %q %q", env.Since, env.Until)
	}
	if env.Groups == nil {
		t.Fatal("groups must unmarshal to a non-nil empty slice")
	}
}

func TestRenderDigest_JSONStructureAndFailedCommandsBrackets(t *testing.T) {
	base := time.Date(2026, 7, 2, 10, 0, 0, 0, time.UTC)
	since := time.Date(2026, 7, 2, 0, 0, 0, 0, time.UTC)
	until := time.Date(2026, 7, 3, 0, 0, 0, 0, time.UTC)
	recs := []record.Record{
		drec("h", "/ok", "clean", ptr(0), base),
		drec("h", "/bad", "boom", ptr(1), base.Add(time.Minute)),
	}
	d := buildDigest(recs, since, until)

	var buf bytes.Buffer
	if err := renderDigest(&buf, d, true); err != nil {
		t.Fatal(err)
	}
	// A group with no failures must still emit failed_commands: [] (never null).
	if !strings.Contains(buf.String(), `"failed_commands": []`) {
		t.Fatalf("a failure-free group must emit failed_commands []:\n%s", buf.String())
	}

	var env digestEnvelope
	if err := json.Unmarshal(buf.Bytes(), &env); err != nil {
		t.Fatalf("round-trip failed: %v\n%s", err, buf.String())
	}
	if len(env.Groups) != 2 {
		t.Fatalf("want 2 groups, got %d", len(env.Groups))
	}
	// cwd asc: "/bad" sorts before "/ok".
	if g := env.Groups[0]; g.Host != "h" || g.CWD != "/bad" || g.Count != 1 || g.Failures != 1 ||
		len(g.FailedCommands) != 1 || g.FailedCommands[0] != "boom" {
		t.Fatalf("group 0 unexpected: %+v", g)
	}
	if g := env.Groups[1]; g.Host != "h" || g.CWD != "/ok" || g.Count != 1 || g.Failures != 0 ||
		len(g.FailedCommands) != 0 {
		t.Fatalf("group 1 unexpected: %+v", g)
	}
}

func TestRenderDigest_HumanEmpty(t *testing.T) {
	since := time.Date(2026, 7, 2, 0, 0, 0, 0, time.UTC)
	until := time.Date(2026, 7, 2, 14, 33, 0, 0, time.UTC)
	d := buildDigest(nil, since, until)

	var buf bytes.Buffer
	if err := renderDigest(&buf, d, false); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "no commands") {
		t.Fatalf("empty window should print a friendly no-commands line, got:\n%s", buf.String())
	}
}

func TestRenderDigest_Human(t *testing.T) {
	base := time.Date(2026, 7, 2, 10, 0, 0, 0, time.UTC)
	since := time.Date(2026, 7, 2, 0, 0, 0, 0, time.UTC)
	until := time.Date(2026, 7, 3, 0, 0, 0, 0, time.UTC)
	recs := []record.Record{
		drec("host-a", "/proj", "go build", ptr(0), base),
		drec("host-a", "/proj", "go test ./...", ptr(1), base.Add(time.Minute)),
	}
	d := buildDigest(recs, since, until)

	var buf bytes.Buffer
	if err := renderDigest(&buf, d, false); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, want := range []string{"host-a", "/proj", "2 commands", "1 failed", "go test ./..."} {
		if !strings.Contains(out, want) {
			t.Fatalf("human output missing %q:\n%s", want, out)
		}
	}
}

func TestParseDigestArgs_DefaultWindowIsToday(t *testing.T) {
	now := time.Date(2026, 7, 2, 14, 30, 45, 0, time.Local)

	since, until, asJSON, err := parseDigestArgs(nil, now)
	if err != nil {
		t.Fatal(err)
	}
	if asJSON {
		t.Fatal("default --json should be false")
	}
	wantSince := time.Date(2026, 7, 2, 0, 0, 0, 0, time.Local)
	if !since.Equal(wantSince) {
		t.Fatalf("default since should be local midnight: want %v, got %v", wantSince, since)
	}
	if !until.Equal(now) {
		t.Fatalf("default until should be now: want %v, got %v", now, until)
	}
}

func TestParseDigestArgs_ExplicitWindowAndJSON(t *testing.T) {
	now := time.Date(2026, 7, 2, 14, 0, 0, 0, time.UTC)

	since, until, asJSON, err := parseDigestArgs(
		[]string{"--since", "2026-07-01T00:00:00Z", "--until", "2026-07-02T00:00:00Z", "--json"}, now)
	if err != nil {
		t.Fatal(err)
	}
	if !asJSON {
		t.Fatal("want --json true")
	}
	if since.Format(time.RFC3339) != "2026-07-01T00:00:00Z" {
		t.Fatalf("since: got %s", since.Format(time.RFC3339))
	}
	if until.Format(time.RFC3339) != "2026-07-02T00:00:00Z" {
		t.Fatalf("until: got %s", until.Format(time.RFC3339))
	}
}

func TestParseDigestArgs_Errors(t *testing.T) {
	now := time.Date(2026, 7, 2, 14, 0, 0, 0, time.UTC)
	cases := map[string][]string{
		"bad since":     {"--since", "not-a-time"},
		"bad until":     {"--until", "nope"},
		"inverted":      {"--since", "2026-07-02T00:00:00Z", "--until", "2026-07-01T00:00:00Z"},
		"stray operand": {"extra-arg"},
	}
	for name, args := range cases {
		if _, _, _, err := parseDigestArgs(args, now); err == nil {
			t.Fatalf("%s: expected an error for args %v", name, args)
		}
	}
}
