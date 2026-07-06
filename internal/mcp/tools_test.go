package mcp

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/mjacobs/yas/internal/record"
	"github.com/mjacobs/yas/internal/store"
)

var base = time.UnixMilli(1_700_000_000_000)

func ptr[T any](v T) *T { return &v }

// fakeSearcher records the last query it received and returns canned results.
type fakeSearcher struct {
	last store.Query
	recs []record.Record
	err  error
}

func (f *fakeSearcher) Search(_ context.Context, q store.Query) ([]record.Record, error) {
	f.last = q
	return f.recs, f.err
}

func TestSearchCommands_MapsQueryAndShapesOutput(t *testing.T) {
	fs := &fakeSearcher{recs: []record.Record{
		{ID: "r1", Command: "git status", Hostname: "h", CWD: "/w", Session: "s1",
			Executor: "human", ExitCode: ptr(0), DurationMS: ptr(int64(5)), StartTime: base, CreatedAt: base},
	}}
	ts := &toolset{search: fs}
	_, out, err := ts.searchCommands(context.Background(), nil, searchCommandsIn{
		Query: "git", Host: "h", Executor: "$all-human", Limit: 5,
	})
	if err != nil {
		t.Fatalf("searchCommands: %v", err)
	}
	if fs.last.Text != "git" || fs.last.Host != "h" || !fs.last.HumansOnly || fs.last.Limit != 5 {
		t.Errorf("query mapping wrong: %+v", fs.last)
	}
	if len(out.Commands) != 1 {
		t.Fatalf("want 1 command, got %d", len(out.Commands))
	}
	c := out.Commands[0]
	if c.ID != "r1" || c.Command != "git status" || c.Host != "h" || c.CWD != "/w" ||
		c.Session != "s1" || c.Executor != "human" {
		t.Errorf("output shape wrong: %+v", c)
	}
	if c.ExitCode == nil || *c.ExitCode != 0 || c.DurationMS == nil || *c.DurationMS != 5 {
		t.Errorf("mutable fields wrong: %+v", c)
	}
	if c.StartTime == "" {
		t.Errorf("start_time not formatted")
	}
}

func TestSearchCommands_ExecutorTokens(t *testing.T) {
	for _, c := range []struct {
		token string
		check func(store.Query) bool
	}{
		{"$all-agent", func(q store.Query) bool { return q.AgentsOnly && !q.HumansOnly && q.Executor == "" }},
		{"$all-human", func(q store.Query) bool { return q.HumansOnly && !q.AgentsOnly }},
		// Bare "human" is the human CLASS, not an exact match — an exact match
		// would drop legacy rows recorded with a NULL/empty executor.
		{"human", func(q store.Query) bool { return q.HumansOnly && !q.AgentsOnly && q.Executor == "" }},
		{"codex", func(q store.Query) bool { return q.Executor == "codex" && !q.AgentsOnly && !q.HumansOnly }},
		{"", func(q store.Query) bool { return q.Executor == "" && !q.AgentsOnly && !q.HumansOnly }},
	} {
		fs := &fakeSearcher{}
		ts := &toolset{search: fs}
		if _, _, err := ts.searchCommands(context.Background(), nil, searchCommandsIn{Executor: c.token}); err != nil {
			t.Fatalf("%q: %v", c.token, err)
		}
		if !c.check(fs.last) {
			t.Errorf("executor %q -> wrong query %+v", c.token, fs.last)
		}
	}
}

func TestSearchCommands_DefaultAndMaxLimit(t *testing.T) {
	fs := &fakeSearcher{}
	ts := &toolset{search: fs}
	_, _, _ = ts.searchCommands(context.Background(), nil, searchCommandsIn{}) // unset
	if fs.last.Limit != defaultLimit {
		t.Errorf("unset limit: got %d want %d", fs.last.Limit, defaultLimit)
	}
	_, _, _ = ts.searchCommands(context.Background(), nil, searchCommandsIn{Limit: 9999}) // over max
	if fs.last.Limit != defaultLimit {
		t.Errorf("over-max limit should fall to default: got %d", fs.last.Limit)
	}
}

func TestSearchCommands_BadTimeIsError(t *testing.T) {
	ts := &toolset{search: &fakeSearcher{}}
	if _, _, err := ts.searchCommands(context.Background(), nil, searchCommandsIn{Since: "not-a-time"}); err == nil {
		t.Error("want error for bad since")
	}
	if _, _, err := ts.searchCommands(context.Background(), nil, searchCommandsIn{Until: "nope"}); err == nil {
		t.Error("want error for bad until")
	}
}

func TestWhatFailed_SetsFailedOnly(t *testing.T) {
	fs := &fakeSearcher{}
	ts := &toolset{search: fs}
	if _, _, err := ts.whatFailed(context.Background(), nil, whatFailedIn{Limit: 3}); err != nil {
		t.Fatalf("whatFailed: %v", err)
	}
	if !fs.last.FailedOnly || fs.last.Limit != 3 {
		t.Errorf("whatFailed query wrong: %+v", fs.last)
	}
}

func TestCommandStatus_FoundAndNotFound(t *testing.T) {
	fs := &fakeSearcher{recs: []record.Record{
		{ID: "x", Command: "ls", ExitCode: ptr(2), StartTime: base, CreatedAt: base},
	}}
	ts := &toolset{search: fs}
	_, out, err := ts.commandStatus(context.Background(), nil, commandStatusIn{ID: "x"})
	if err != nil {
		t.Fatalf("commandStatus: %v", err)
	}
	if fs.last.ID != "x" {
		t.Errorf("should query by id, got %+v", fs.last)
	}
	if !out.Found || out.Command == nil || out.Command.ID != "x" || out.Command.ExitCode == nil || *out.Command.ExitCode != 2 {
		t.Errorf("found case wrong: %+v", out)
	}

	ts2 := &toolset{search: &fakeSearcher{recs: nil}}
	_, out2, _ := ts2.commandStatus(context.Background(), nil, commandStatusIn{ID: "none"})
	if out2.Found || out2.Command != nil {
		t.Errorf("not-found case wrong: %+v", out2)
	}
}

// The self-reference guard: a toolset carrying excludeCorrID stamps it on the
// store.Query for every record-listing/scanning tool, so the querying agent's
// own in-flight session is filtered out. A by-id command_status lookup is
// exempt — an explicit id, even the agent's own, must still resolve.
func TestSelfReferenceGuard_ExcludeCorrID(t *testing.T) {
	listing := map[string]func(*toolset) error{
		"search_commands": func(ts *toolset) error {
			_, _, err := ts.searchCommands(context.Background(), nil, searchCommandsIn{})
			return err
		},
		"recent_commands": func(ts *toolset) error {
			_, _, err := ts.recentCommands(context.Background(), nil, recentCommandsIn{})
			return err
		},
		"what_failed": func(ts *toolset) error {
			_, _, err := ts.whatFailed(context.Background(), nil, whatFailedIn{})
			return err
		},
		"failure_summary": func(ts *toolset) error {
			_, _, err := ts.failureSummary(context.Background(), nil, failureSummaryIn{})
			return err
		},
		"how_did_i_run": func(ts *toolset) error {
			_, _, err := ts.howDidIRun(context.Background(), nil, howDidIRunIn{Command: "git"})
			return err
		},
	}
	for name, run := range listing {
		// With a corr_id set, every listing/scanning tool must stamp it.
		fs := &fakeSearcher{}
		ts := &toolset{search: fs, excludeCorrID: "S"}
		if err := run(ts); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if fs.last.ExcludeCorrID != "S" {
			t.Errorf("%s must stamp ExcludeCorrID=S, got %q", name, fs.last.ExcludeCorrID)
		}
		// With no corr_id set, the query must not filter by corr_id.
		fs0 := &fakeSearcher{}
		ts0 := &toolset{search: fs0} // excludeCorrID == ""
		if err := run(ts0); err != nil {
			t.Fatalf("%s (unset): %v", name, err)
		}
		if fs0.last.ExcludeCorrID != "" {
			t.Errorf("%s with unset excludeCorrID must not filter, got %q", name, fs0.last.ExcludeCorrID)
		}
	}

	// command_status is a point lookup by explicit id: even when the record's own
	// corr_id equals the guard value, the by-id lookup must resolve and must NOT
	// stamp ExcludeCorrID.
	fs := &fakeSearcher{recs: []record.Record{
		{ID: "own", Command: "ls", CorrID: "S", ExitCode: ptr(0), StartTime: base, CreatedAt: base},
	}}
	ts := &toolset{search: fs, excludeCorrID: "S"}
	_, out, err := ts.commandStatus(context.Background(), nil, commandStatusIn{ID: "own"})
	if err != nil {
		t.Fatalf("commandStatus: %v", err)
	}
	if fs.last.ExcludeCorrID != "" {
		t.Errorf("command_status must NOT stamp ExcludeCorrID, got %q", fs.last.ExcludeCorrID)
	}
	if fs.last.ID != "own" {
		t.Errorf("command_status must look up by id, got %+v", fs.last)
	}
	if !out.Found || out.Command == nil || out.Command.ID != "own" {
		t.Errorf("by-id lookup of own record must still resolve: %+v", out)
	}
}

func TestRecentCommands_NoTextFilter(t *testing.T) {
	fs := &fakeSearcher{}
	ts := &toolset{search: fs}
	if _, _, err := ts.recentCommands(context.Background(), nil, recentCommandsIn{Host: "h", Executor: "$all-agent"}); err != nil {
		t.Fatalf("recentCommands: %v", err)
	}
	if fs.last.Text != "" || fs.last.Host != "h" || !fs.last.AgentsOnly {
		t.Errorf("recentCommands query wrong: %+v", fs.last)
	}
}

// --- failure_summary (rollup) ---

func TestFailureSummary_GroupsSortsExcludesPassers(t *testing.T) {
	fs := &fakeSearcher{recs: []record.Record{
		{ID: "1", Command: "false", ExitCode: ptr(1), StartTime: base.Add(1 * time.Minute)},
		{ID: "2", Command: "false", ExitCode: ptr(1), StartTime: base.Add(5 * time.Minute)},
		{ID: "3", Command: "false", ExitCode: ptr(2), StartTime: base.Add(3 * time.Minute)},
		{ID: "4", Command: "go test ./...", ExitCode: ptr(1), StartTime: base.Add(2 * time.Minute)},
		{ID: "5", Command: "go test ./...", ExitCode: ptr(1), StartTime: base.Add(4 * time.Minute)},
		{ID: "6", Command: "curl x", ExitCode: ptr(7), StartTime: base.Add(6 * time.Minute)},
		// A passer sneaks into the returned slice; it must never form a group.
		{ID: "7", Command: "ls", ExitCode: ptr(0), StartTime: base.Add(9 * time.Minute)},
	}}
	ts := &toolset{search: fs}
	_, out, err := ts.failureSummary(context.Background(), nil, failureSummaryIn{})
	if err != nil {
		t.Fatalf("failureSummary: %v", err)
	}
	if !fs.last.FailedOnly {
		t.Errorf("failure_summary must request FailedOnly: %+v", fs.last)
	}
	if fs.last.Limit != scanCap {
		t.Errorf("scan window Limit: got %d want %d", fs.last.Limit, scanCap)
	}
	if out.ScanTruncated {
		t.Errorf("small dataset must not be Truncated")
	}
	if len(out.Failures) != 3 {
		t.Fatalf("want 3 groups, got %d: %+v", len(out.Failures), out.Failures)
	}
	// Sorted by count desc, tie-break last_seen desc.
	g0 := out.Failures[0]
	if g0.Command != "false" || g0.Count != 3 ||
		g0.LastSeen != base.Add(5*time.Minute).UTC().Format(time.RFC3339) ||
		g0.LastExitCode == nil || *g0.LastExitCode != 1 {
		t.Errorf("group 0 wrong: %+v", g0)
	}
	if out.Failures[1].Command != "go test ./..." || out.Failures[1].Count != 2 {
		t.Errorf("group 1 wrong: %+v", out.Failures[1])
	}
	if out.Failures[2].Command != "curl x" || out.Failures[2].Count != 1 {
		t.Errorf("group 2 wrong: %+v", out.Failures[2])
	}
	for _, g := range out.Failures {
		if g.Command == "ls" {
			t.Errorf("passing command leaked into rollup: %+v", g)
		}
	}
}

func TestFailureSummary_TopNAndScanCapTruncation(t *testing.T) {
	recs := make([]record.Record, 3)
	for i := range recs {
		recs[i] = record.Record{
			ID:        fmt.Sprint(i),
			Command:   fmt.Sprintf("cmd%d", i),
			ExitCode:  ptr(1),
			StartTime: base.Add(time.Duration(i) * time.Minute),
		}
	}
	// Injected small scan window: exactly scanCap rows returned -> Truncated.
	fs := &fakeSearcher{recs: recs}
	ts := &toolset{search: fs, scanCap: 3}
	_, out, err := ts.failureSummary(context.Background(), nil, failureSummaryIn{Limit: 2})
	if err != nil {
		t.Fatalf("failureSummary: %v", err)
	}
	if fs.last.Limit != 3 {
		t.Errorf("scan window Limit: got %d want 3", fs.last.Limit)
	}
	if !out.ScanTruncated {
		t.Errorf("scan hitting the cap must set Truncated")
	}
	if len(out.Failures) != 2 {
		t.Fatalf("Limit=2 should cap top-N at 2, got %d", len(out.Failures))
	}

	// Below the cap -> not Truncated.
	fs2 := &fakeSearcher{recs: recs[:2]}
	ts2 := &toolset{search: fs2, scanCap: 3}
	_, out2, err := ts2.failureSummary(context.Background(), nil, failureSummaryIn{})
	if err != nil {
		t.Fatalf("failureSummary: %v", err)
	}
	if out2.ScanTruncated {
		t.Errorf("below-cap scan must not be Truncated")
	}
}

func TestFailureSummary_EmptyIsNonNilAndBadSinceErrors(t *testing.T) {
	ts := &toolset{search: &fakeSearcher{}}
	_, out, err := ts.failureSummary(context.Background(), nil, failureSummaryIn{})
	if err != nil {
		t.Fatalf("failureSummary: %v", err)
	}
	if out.Failures == nil {
		t.Errorf("empty Failures must be [] not nil")
	}
	if _, _, err := ts.failureSummary(context.Background(), nil, failureSummaryIn{Since: "nope"}); err == nil {
		t.Error("want error for bad since")
	}
}

// --- how_did_i_run (recall) ---

func TestHowDidIRun_CollapsesQuotedArgsAndAnchors(t *testing.T) {
	fs := &fakeSearcher{recs: []record.Record{
		{ID: "1", Command: `git commit -m "a"`, StartTime: base.Add(1 * time.Minute)},
		{ID: "2", Command: `git commit -m "b"`, StartTime: base.Add(2 * time.Minute)},
		{ID: "3", Command: `git status`, StartTime: base.Add(3 * time.Minute)},
		{ID: "4", Command: `git status`, StartTime: base.Add(4 * time.Minute)},
		{ID: "5", Command: `git status`, StartTime: base.Add(5 * time.Minute)},
		{ID: "6", Command: `git status`, StartTime: base.Add(6 * time.Minute)},
		{ID: "7", Command: `git status`, StartTime: base.Add(7 * time.Minute)},
		// Non-matches: first-token basename is not exactly "git".
		{ID: "8", Command: `grepgit foo`, StartTime: base.Add(8 * time.Minute)},
		{ID: "9", Command: `digit 3`, StartTime: base.Add(9 * time.Minute)},
		// Absolute path anchors to its basename.
		{ID: "10", Command: `/usr/bin/git push`, StartTime: base.Add(10 * time.Minute)},
	}}
	ts := &toolset{search: fs}
	_, out, err := ts.howDidIRun(context.Background(), nil, howDidIRunIn{Command: "git"})
	if err != nil {
		t.Fatalf("howDidIRun: %v", err)
	}
	if fs.last.Text != "git" || fs.last.Limit != scanCap || !fs.last.CommandTextOnly {
		t.Errorf("anchor query wrong (must scope Text to the command column): %+v", fs.last)
	}
	if len(out.Patterns) != 3 {
		t.Fatalf("want 3 patterns, got %d: %+v", len(out.Patterns), out.Patterns)
	}
	// Newest-first by last_seen.
	if out.Patterns[0].Command != "/usr/bin/git push" || out.Patterns[0].Count != 1 {
		t.Errorf("pattern 0 wrong: %+v", out.Patterns[0])
	}
	if out.Patterns[1].Command != "git status" || out.Patterns[1].Count != 5 {
		t.Errorf("pattern 1 wrong: %+v", out.Patterns[1])
	}
	// Quoted-arg collapse: representative is the most-recent full line.
	if out.Patterns[2].Command != `git commit -m "b"` || out.Patterns[2].Count != 2 {
		t.Errorf("pattern 2 wrong: %+v", out.Patterns[2])
	}
	if out.Patterns[2].LastSeen != base.Add(2*time.Minute).UTC().Format(time.RFC3339) {
		t.Errorf("collapsed group last_seen wrong: %+v", out.Patterns[2])
	}
}

func TestHowDidIRun_SkipsEnvAssignmentWhenAnchoring(t *testing.T) {
	fs := &fakeSearcher{recs: []record.Record{
		{ID: "1", Command: `FOO=1 git status`, StartTime: base.Add(1 * time.Minute)},
		{ID: "2", Command: `PATH=/x:/y BAR=2 /usr/bin/git log`, StartTime: base.Add(2 * time.Minute)},
	}}
	ts := &toolset{search: fs}
	_, out, err := ts.howDidIRun(context.Background(), nil, howDidIRunIn{Command: "git"})
	if err != nil {
		t.Fatalf("howDidIRun: %v", err)
	}
	if len(out.Patterns) != 2 {
		t.Fatalf("env-assignment prefixes must be skipped when anchoring, got %d: %+v",
			len(out.Patterns), out.Patterns)
	}
}

func TestHowDidIRun_RequiresCommandAndEmptyResult(t *testing.T) {
	ts := &toolset{search: &fakeSearcher{}}
	if _, _, err := ts.howDidIRun(context.Background(), nil, howDidIRunIn{}); err == nil {
		t.Error("want error when command is empty")
	}
	fs := &fakeSearcher{recs: []record.Record{
		{ID: "1", Command: "git status", StartTime: base},
	}}
	ts2 := &toolset{search: fs}
	_, out, err := ts2.howDidIRun(context.Background(), nil, howDidIRunIn{Command: "docker"})
	if err != nil {
		t.Fatalf("howDidIRun: %v", err)
	}
	if len(out.Patterns) != 0 {
		t.Errorf("no anchor match must yield 0 patterns, got %+v", out.Patterns)
	}
	if out.Patterns == nil {
		t.Errorf("empty Patterns must be [] not nil")
	}
}
