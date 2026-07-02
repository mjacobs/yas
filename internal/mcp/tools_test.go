package mcp

import (
	"context"
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
