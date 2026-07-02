package mcp

import (
	"context"
	"fmt"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/mjacobs/yas/internal/record"
	"github.com/mjacobs/yas/internal/store"
)

// Searcher is the read seam the tools need — the local SQLite replica satisfies
// it, the same interface the query API targets.
type Searcher interface {
	Search(ctx context.Context, q store.Query) ([]record.Record, error)
}

// toolset holds the dependencies shared by every tool handler. now is injectable
// so a future self-reference window (excluding the querying agent's own recent
// commands, once those are captured) is testable; it is unused today.
type toolset struct {
	search Searcher
	now    func() time.Time
}

// runQuery executes q and shapes the matching records into commandsOut.
func (t *toolset) runQuery(ctx context.Context, q store.Query) (*mcp.CallToolResult, commandsOut, error) {
	recs, err := t.search.Search(ctx, q)
	if err != nil {
		return nil, commandsOut{}, err
	}
	out := commandsOut{Commands: make([]commandOut, 0, len(recs))}
	for _, r := range recs {
		out.Commands = append(out.Commands, toCommandOut(r))
	}
	return nil, out, nil
}

// commandsOut is the shared output of the list-returning tools.
type commandsOut struct {
	Commands []commandOut `json:"commands"`
}

// --- search_commands ---

type searchCommandsIn struct {
	Query    string `json:"query,omitempty" jsonschema:"Full-text terms matched against the command (AND: every term must appear). Empty returns the most recent commands."`
	Host     string `json:"host,omitempty" jsonschema:"Exact hostname filter (which machine ran it)."`
	Cwd      string `json:"cwd,omitempty" jsonschema:"Exact working-directory filter."`
	Session  string `json:"session,omitempty" jsonschema:"Exact shell-session filter."`
	Executor string `json:"executor,omitempty" jsonschema:"Who/what ran it: a name (human, claude-code, codex, ci), or $all-agent / $all-human."`
	Exit     *int   `json:"exit,omitempty" jsonschema:"Exact exit-code filter."`
	Failed   bool   `json:"failed,omitempty" jsonschema:"Only commands that finished with a non-zero exit code."`
	Since    string `json:"since,omitempty" jsonschema:"Only commands at/after this RFC3339 time."`
	Until    string `json:"until,omitempty" jsonschema:"Only commands before this RFC3339 time (exclusive)."`
	Limit    int    `json:"limit,omitempty" jsonschema:"Max results, default 20, max 100."`
}

func (t *toolset) searchCommands(ctx context.Context, _ *mcp.CallToolRequest, in searchCommandsIn) (*mcp.CallToolResult, commandsOut, error) {
	q := store.Query{
		Text:       in.Query,
		Host:       in.Host,
		CWD:        in.Cwd,
		Session:    in.Session,
		ExitCode:   in.Exit,
		FailedOnly: in.Failed,
		Limit:      clampLimit(in.Limit, defaultLimit, maxLimit),
	}
	q.ApplyExecutorToken(in.Executor)
	var err error
	if q.Since, err = parseOptTime(in.Since); err != nil {
		return nil, commandsOut{}, fmt.Errorf("invalid since %q: want RFC3339 (e.g. 2006-01-02T15:04:05Z)", in.Since)
	}
	if q.Until, err = parseOptTime(in.Until); err != nil {
		return nil, commandsOut{}, fmt.Errorf("invalid until %q: want RFC3339 (e.g. 2006-01-02T15:04:05Z)", in.Until)
	}
	return t.runQuery(ctx, q)
}

// --- recent_commands ---

type recentCommandsIn struct {
	Host     string `json:"host,omitempty" jsonschema:"Exact hostname filter."`
	Cwd      string `json:"cwd,omitempty" jsonschema:"Exact working-directory filter."`
	Executor string `json:"executor,omitempty" jsonschema:"Who/what ran it: a name, or $all-agent / $all-human."`
	Limit    int    `json:"limit,omitempty" jsonschema:"Max results, default 20, max 100."`
}

func (t *toolset) recentCommands(ctx context.Context, _ *mcp.CallToolRequest, in recentCommandsIn) (*mcp.CallToolResult, commandsOut, error) {
	q := store.Query{
		Host:  in.Host,
		CWD:   in.Cwd,
		Limit: clampLimit(in.Limit, defaultLimit, maxLimit),
	}
	q.ApplyExecutorToken(in.Executor)
	return t.runQuery(ctx, q)
}

// --- what_failed ---

type whatFailedIn struct {
	Host  string `json:"host,omitempty" jsonschema:"Exact hostname filter."`
	Cwd   string `json:"cwd,omitempty" jsonschema:"Exact working-directory filter."`
	Since string `json:"since,omitempty" jsonschema:"Only failures at/after this RFC3339 time."`
	Limit int    `json:"limit,omitempty" jsonschema:"Max results, default 20, max 100."`
}

func (t *toolset) whatFailed(ctx context.Context, _ *mcp.CallToolRequest, in whatFailedIn) (*mcp.CallToolResult, commandsOut, error) {
	q := store.Query{
		Host:       in.Host,
		CWD:        in.Cwd,
		FailedOnly: true,
		Limit:      clampLimit(in.Limit, defaultLimit, maxLimit),
	}
	var err error
	if q.Since, err = parseOptTime(in.Since); err != nil {
		return nil, commandsOut{}, fmt.Errorf("invalid since %q: want RFC3339 (e.g. 2006-01-02T15:04:05Z)", in.Since)
	}
	return t.runQuery(ctx, q)
}

// --- command_status ---

type commandStatusIn struct {
	ID string `json:"id" jsonschema:"The command id (UUIDv7) to look up, e.g. from a previous result."`
}

type commandStatusOut struct {
	Found   bool        `json:"found"`
	Command *commandOut `json:"command,omitempty"`
}

func (t *toolset) commandStatus(ctx context.Context, _ *mcp.CallToolRequest, in commandStatusIn) (*mcp.CallToolResult, commandStatusOut, error) {
	recs, err := t.search.Search(ctx, store.Query{ID: in.ID, Limit: 1})
	if err != nil {
		return nil, commandStatusOut{}, err
	}
	if len(recs) == 0 {
		return nil, commandStatusOut{Found: false}, nil
	}
	c := toCommandOut(recs[0])
	return nil, commandStatusOut{Found: true, Command: &c}, nil
}
