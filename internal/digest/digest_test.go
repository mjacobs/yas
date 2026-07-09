package digest

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/mjacobs/yas/internal/record"
)

func ptr[T any](v T) *T { return &v }

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

func TestBuild_GroupsCountAndSort(t *testing.T) {
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

	d := Build(recs, since, until)

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

// drecRepo is drec plus a git repo root: it lets the digest group by project
// (repo root) rather than by exact cwd.
func drecRepo(host, cwd, repoRoot, cmd string, exit *int, t time.Time) record.Record {
	r := drec(host, cwd, cmd, exit, t)
	r.RepoRoot = repoRoot
	return r
}

// When records carry a repo root, sibling subdirectories of one repo collapse
// into a single project group (keyed by repo root), while records without a
// repo root still group by their exact cwd. This is xvt6's payoff: digest
// groups by PROJECT, not by noisy deep subdirs.
func TestBuild_GroupsByRepoRootWhenPresent(t *testing.T) {
	base := time.Date(2026, 7, 2, 10, 0, 0, 0, time.UTC)
	since := time.Date(2026, 7, 2, 0, 0, 0, 0, time.UTC)
	until := time.Date(2026, 7, 3, 0, 0, 0, 0, time.UTC)
	recs := []record.Record{
		// Two different cwds under the same repo -> one project group.
		drecRepo("h", "/repo/a", "/repo/a", "go build", ptr(0), base),
		drecRepo("h", "/repo/a/internal/deep", "/repo/a", "go test", ptr(1), base.Add(time.Minute)),
		// A command with no repo root -> grouped by its bare cwd.
		drec("h", "/loose", "ls", ptr(0), base.Add(2*time.Minute)),
	}

	d := Build(recs, since, until)

	if len(d.Groups) != 2 {
		t.Fatalf("want 2 groups (one project, one loose cwd), got %d: %+v", len(d.Groups), d.Groups)
	}
	// Sorted by host then location: "/loose" < "/repo/a".
	loose, project := d.Groups[0], d.Groups[1]
	if loose.CWD != "/loose" || loose.RepoRoot != "" || loose.Count != 1 {
		t.Errorf("loose group unexpected: %+v", loose)
	}
	if project.CWD != "/repo/a" || project.RepoRoot != "/repo/a" {
		t.Errorf("project group location: want cwd=/repo/a repo_root=/repo/a, got cwd=%q repo_root=%q", project.CWD, project.RepoRoot)
	}
	if project.Count != 2 || project.Failures != 1 {
		t.Errorf("project group counts: want 2 commands / 1 failure, got %d / %d", project.Count, project.Failures)
	}
	if len(project.FailedCommands) != 1 || project.FailedCommands[0] != "go test" {
		t.Errorf("project group failed commands: want [go test], got %v", project.FailedCommands)
	}
}

// A project group (RepoRoot set) and a bare-cwd group whose exact cwd equals
// that repo root must NOT merge: they are different buckets that happen to share
// a path string (e.g. an imported record whose cwd was /repo, plus a live record
// now inside the /repo checkout). Merging them would conflate counts and make
// the emitted repo_root depend on record processing order.
func TestBuild_RepoRootAndCwdSamePathDoNotMerge(t *testing.T) {
	base := time.Date(2026, 7, 2, 10, 0, 0, 0, time.UTC)
	since := time.Date(2026, 7, 2, 0, 0, 0, 0, time.UTC)
	until := time.Date(2026, 7, 3, 0, 0, 0, 0, time.UTC)
	recs := []record.Record{
		drecRepo("h", "/repo/sub", "/repo", "go build", ptr(0), base), // project group
		drec("h", "/repo", "ls", ptr(0), base.Add(time.Minute)),       // bare-cwd group
		drec("h", "/repo", "pwd", ptr(0), base.Add(2*time.Minute)),    // same bare-cwd group
	}

	d := Build(recs, since, until)

	if len(d.Groups) != 2 {
		t.Fatalf("want 2 groups (project + bare cwd, not merged), got %d: %+v", len(d.Groups), d.Groups)
	}
	// Deterministic order: same host and CWD "/repo", tie broken by RepoRoot
	// ("" before "/repo"), so the bare-cwd group comes first.
	cwdGroup, projGroup := d.Groups[0], d.Groups[1]
	if cwdGroup.RepoRoot != "" || cwdGroup.Count != 2 {
		t.Errorf("bare-cwd group: want repo_root='' count=2, got repo_root=%q count=%d", cwdGroup.RepoRoot, cwdGroup.Count)
	}
	if projGroup.RepoRoot != "/repo" || projGroup.Count != 1 {
		t.Errorf("project group: want repo_root=/repo count=1, got repo_root=%q count=%d", projGroup.RepoRoot, projGroup.Count)
	}
}

func TestBuild_ExitCodeSemantics(t *testing.T) {
	base := time.Date(2026, 7, 2, 10, 0, 0, 0, time.UTC)
	since := base.Add(-time.Hour)
	until := base.Add(time.Hour)
	recs := []record.Record{
		drec("h", "/d", "unfinished", nil, base),                 // nil exit: not a failure
		drec("h", "/d", "ok", ptr(0), base.Add(time.Minute)),     // exit 0: not a failure
		drec("h", "/d", "boom", ptr(2), base.Add(2*time.Minute)), // non-zero: failure
	}

	d := Build(recs, since, until)

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

func TestBuild_EmptyWindowGroupsNonNil(t *testing.T) {
	since := time.Date(2026, 7, 2, 0, 0, 0, 0, time.UTC)
	until := since.Add(24 * time.Hour)

	d := Build(nil, since, until)

	if d.Groups == nil {
		t.Fatal("Groups must be non-nil (serializes as [] not null)")
	}
	if len(d.Groups) != 0 {
		t.Fatalf("want 0 groups, got %d", len(d.Groups))
	}
}

func TestBuild_WindowFilteringUntilExclusive(t *testing.T) {
	base := time.Date(2026, 7, 2, 10, 0, 0, 0, time.UTC)
	since := base
	until := base.Add(10 * time.Minute)
	recs := []record.Record{
		drec("h", "/d", "before", ptr(0), base.Add(-time.Minute)),  // excluded (< since)
		drec("h", "/d", "atstart", ptr(0), base),                   // included (Since inclusive)
		drec("h", "/d", "inside", ptr(0), base.Add(5*time.Minute)), // included
		drec("h", "/d", "atend", ptr(0), until),                    // excluded (Until exclusive)
	}

	d := Build(recs, since, until)

	if len(d.Groups) != 1 {
		t.Fatalf("want 1 group, got %d", len(d.Groups))
	}
	if d.Groups[0].Count != 2 {
		t.Fatalf("want count 2 (atstart, inside), got %d", d.Groups[0].Count)
	}
}

func TestBuild_FailedCommandsCapAndTruncate(t *testing.T) {
	base := time.Date(2026, 7, 2, 10, 0, 0, 0, time.UTC)
	since := base.Add(-time.Hour)
	until := base.Add(2 * time.Hour)

	var recs []record.Record
	// 15 distinct failing commands -> the sample is capped at maxFailedPerGroup.
	for i := 0; i < 15; i++ {
		recs = append(recs, drec("h", "/d", fmt.Sprintf("fail-%02d", i), ptr(1), base.Add(time.Duration(i)*time.Minute)))
	}
	// One very long, latest failing command -> sampled first and truncated.
	long := strings.Repeat("x", MaxFailedCommandRunes+50)
	recs = append(recs, drec("h", "/d", long, ptr(1), base.Add(90*time.Minute)))

	d := Build(recs, since, until)

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
	if n := utf8.RuneCountInString(g.FailedCommands[0]); n != MaxFailedCommandRunes {
		t.Fatalf("truncated command want %d runes, got %d", MaxFailedCommandRunes, n)
	}
}

func TestToEnvelope_EmptyIsBracketsNotNull(t *testing.T) {
	since := time.Date(2026, 7, 2, 0, 0, 0, 0, time.UTC)
	until := time.Date(2026, 7, 2, 14, 33, 0, 0, time.UTC)

	raw, err := json.Marshal(ToEnvelope(Build(nil, since, until)))
	if err != nil {
		t.Fatal(err)
	}
	out := string(raw)
	if !strings.Contains(out, `"groups":[]`) {
		t.Fatalf("empty groups must serialize as [] not null:\n%s", out)
	}
	if strings.Contains(out, "null") {
		t.Fatalf("digest JSON must never contain null:\n%s", out)
	}

	var env Envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("round-trip failed: %v\n%s", err, out)
	}
	if env.Since != "2026-07-02T00:00:00Z" || env.Until != "2026-07-02T14:33:00Z" {
		t.Fatalf("since/until must be RFC3339: got %q %q", env.Since, env.Until)
	}
	if env.Groups == nil {
		t.Fatal("groups must unmarshal to a non-nil empty slice")
	}
}

func TestToEnvelope_StructureAndFailedCommandsBrackets(t *testing.T) {
	base := time.Date(2026, 7, 2, 10, 0, 0, 0, time.UTC)
	since := time.Date(2026, 7, 2, 0, 0, 0, 0, time.UTC)
	until := time.Date(2026, 7, 3, 0, 0, 0, 0, time.UTC)
	recs := []record.Record{
		drec("h", "/ok", "clean", ptr(0), base),
		drec("h", "/bad", "boom", ptr(1), base.Add(time.Minute)),
	}

	raw, err := json.Marshal(ToEnvelope(Build(recs, since, until)))
	if err != nil {
		t.Fatal(err)
	}
	// A group with no failures must still emit failed_commands: [] (never null).
	if !strings.Contains(string(raw), `"failed_commands":[]`) {
		t.Fatalf("a failure-free group must emit failed_commands []:\n%s", raw)
	}

	var env Envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("round-trip failed: %v\n%s", err, raw)
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

func TestStartOfDay(t *testing.T) {
	now := time.Date(2026, 7, 2, 14, 30, 45, 123, time.Local)
	want := time.Date(2026, 7, 2, 0, 0, 0, 0, time.Local)
	if got := StartOfDay(now); !got.Equal(want) {
		t.Fatalf("StartOfDay: want %v, got %v", want, got)
	}
}
