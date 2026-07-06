package mcp

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
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
// for testability. excludeCorrID realizes the self-reference guard: when set, the
// record-listing/scanning tools omit records carrying that corr_id (the querying
// agent's own in-flight session), so the agent doesn't recall its own commands;
// the by-id command_status lookup is exempt. scanCap overrides the fold-window
// size for the rollup/recall verbs (0 -> the scanCap const); it exists so tests
// can exercise the Truncated boundary cheaply.
type toolset struct {
	search        Searcher
	now           func() time.Time
	excludeCorrID string
	scanCap       int
}

// scanLimit is the effective fold window for the rollup/recall verbs: the
// injected override (tests) or the scanCap default (production).
func (t *toolset) scanLimit() int {
	if t.scanCap > 0 {
		return t.scanCap
	}
	return scanCap
}

// runQuery executes q and shapes the matching records into commandsOut. It
// stamps the self-reference guard (excludeCorrID) so every listing tool routed
// through it omits the querying agent's own in-flight session; the by-id
// command_status lookup does not use this path and so is exempt.
func (t *toolset) runQuery(ctx context.Context, q store.Query) (*mcp.CallToolResult, commandsOut, error) {
	q.ExcludeCorrID = t.excludeCorrID
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

// --- failure_summary (rollup) ---

type failureSummaryIn struct {
	Host  string `json:"host,omitempty" jsonschema:"Exact hostname filter (which machine failed)."`
	Cwd   string `json:"cwd,omitempty" jsonschema:"Exact working-directory filter."`
	Since string `json:"since,omitempty" jsonschema:"Only failures at/after this RFC3339 time."`
	Limit int    `json:"limit,omitempty" jsonschema:"Max failing-command groups to return (top-N), default 10, max 50."`
}

// failureGroup is one recurring failing command: how many times it failed within
// the scan window, when it last failed, and that last failure's exit code.
type failureGroup struct {
	Command      string `json:"command"`
	Truncated    bool   `json:"truncated,omitempty"`
	Count        int    `json:"count"`
	LastSeen     string `json:"last_seen"`
	LastExitCode *int   `json:"last_exit_code,omitempty"`
}

type failureSummaryOut struct {
	Failures []failureGroup `json:"failures"`
	// ScanTruncated is the scan-window flag (distinct from a per-item command's
	// truncated flag): the fold hit scanCap, so failures older than the window
	// weren't counted.
	ScanTruncated bool `json:"scan_truncated,omitempty"`
}

// failureSummary rolls up recurring failures: it scans the most recent failing
// commands (up to the fold window), folds them by exact command string, and
// returns the top-N groups by count (tie-break: most-recent failure first). It
// complements what_failed (individual failures) with the aggregate "what keeps
// breaking" view. Counts/last-seen are exact within the window; Truncated is set
// when the scan hit the cap, so older failures may exist beyond it.
func (t *toolset) failureSummary(ctx context.Context, _ *mcp.CallToolRequest, in failureSummaryIn) (*mcp.CallToolResult, failureSummaryOut, error) {
	q := store.Query{
		Host:          in.Host,
		CWD:           in.Cwd,
		FailedOnly:    true,
		ExcludeCorrID: t.excludeCorrID, // self-reference guard
		Limit:         t.scanLimit(),
	}
	var err error
	if q.Since, err = parseOptTime(in.Since); err != nil {
		return nil, failureSummaryOut{}, fmt.Errorf("invalid since %q: want RFC3339 (e.g. 2006-01-02T15:04:05Z)", in.Since)
	}
	recs, err := t.search.Search(ctx, q)
	if err != nil {
		return nil, failureSummaryOut{}, err
	}
	// The store returns at most Limit rows; a full window means more may exist.
	truncated := len(recs) >= t.scanLimit()

	type agg struct {
		command  string
		count    int
		lastSeen time.Time
		lastExit *int
	}
	byCommand := make(map[string]*agg)
	order := make([]*agg, 0)
	for i := range recs {
		r := recs[i]
		// Defensive: FailedOnly should have excluded passers, but never fold a
		// non-failure into a "failing command" rollup.
		if r.ExitCode == nil || *r.ExitCode == 0 {
			continue
		}
		g := byCommand[r.Command]
		if g == nil {
			g = &agg{command: r.Command}
			byCommand[r.Command] = g
			order = append(order, g)
		}
		g.count++
		if g.lastSeen.IsZero() || r.StartTime.After(g.lastSeen) {
			g.lastSeen = r.StartTime
			g.lastExit = r.ExitCode
		}
	}

	sort.SliceStable(order, func(i, j int) bool {
		if order[i].count != order[j].count {
			return order[i].count > order[j].count
		}
		return order[i].lastSeen.After(order[j].lastSeen)
	})

	n := clampLimit(in.Limit, failureSummaryDefaultLimit, failureSummaryMaxLimit)
	out := failureSummaryOut{Failures: make([]failureGroup, 0, n), ScanTruncated: truncated}
	for _, g := range order {
		if len(out.Failures) >= n {
			break
		}
		cmd, cut := truncate(g.command, commandMaxChars)
		out.Failures = append(out.Failures, failureGroup{
			Command:      cmd,
			Truncated:    cut,
			Count:        g.count,
			LastSeen:     g.lastSeen.UTC().Format(time.RFC3339),
			LastExitCode: g.lastExit,
		})
	}
	return nil, out, nil
}

// --- how_did_i_run (recall) ---

type howDidIRunIn struct {
	Command string `json:"command" jsonschema:"REQUIRED. The program name to recall, e.g. git, ssh, systemctl. Matches a command whose first token's basename equals this (so both git ... and /usr/bin/git ... match git); a leading VAR=val env-assignment is skipped."`
	Host    string `json:"host,omitempty" jsonschema:"Exact hostname filter."`
	Cwd     string `json:"cwd,omitempty" jsonschema:"Exact working-directory filter."`
	Limit   int    `json:"limit,omitempty" jsonschema:"Max distinct argument patterns to return, default 20, max 100."`
}

// recallPattern is one distinct way the program was invoked: a representative
// full command line (the most-recent one in the group), how many times a
// matching line was run, and when it last ran.
type recallPattern struct {
	Command   string `json:"command"`
	Truncated bool   `json:"truncated,omitempty"`
	Count     int    `json:"count"`
	LastSeen  string `json:"last_seen"`
}

type howDidIRunOut struct {
	Patterns []recallPattern `json:"patterns"`
	// ScanTruncated is the scan-window flag (distinct from a per-item command's
	// truncated flag): the scan hit scanCap, so older invocations may exist
	// beyond the window.
	ScanTruncated bool `json:"scan_truncated,omitempty"`
}

// howDidIRun recalls how a specific program was invoked. It scans candidates via
// full-text search on the program name (a superset), then keeps only lines whose
// first-token basename equals Command, folds near-duplicates by a
// quoted-substring-masked pattern, and returns the distinct patterns newest-first
// with a representative (most-recent) full command line and a count.
func (t *toolset) howDidIRun(ctx context.Context, _ *mcp.CallToolRequest, in howDidIRunIn) (*mcp.CallToolResult, howDidIRunOut, error) {
	if in.Command == "" {
		return nil, howDidIRunOut{}, fmt.Errorf("command is required (the program name to recall, e.g. git)")
	}
	q := store.Query{
		Text:            in.Command,
		CommandTextOnly: true, // anchor on the program, not cwd paths containing it
		Host:            in.Host,
		CWD:             in.Cwd,
		ExcludeCorrID:   t.excludeCorrID, // self-reference guard
		Limit:           t.scanLimit(),
	}
	recs, err := t.search.Search(ctx, q)
	if err != nil {
		return nil, howDidIRunOut{}, err
	}
	truncated := len(recs) >= t.scanLimit()

	type agg struct {
		command  string // representative: most-recent full command line
		count    int
		lastSeen time.Time
	}
	byPattern := make(map[string]*agg)
	order := make([]*agg, 0)
	for i := range recs {
		r := recs[i]
		// Full-text search is a superset; anchor to the actual program token.
		if firstProgram(r.Command) != in.Command {
			continue
		}
		key := maskQuoted(r.Command)
		g := byPattern[key]
		if g == nil {
			g = &agg{}
			byPattern[key] = g
			order = append(order, g)
		}
		g.count++
		if g.lastSeen.IsZero() || r.StartTime.After(g.lastSeen) {
			g.lastSeen = r.StartTime
			g.command = r.Command
		}
	}

	// Newest-first; tie-break on the representative for determinism.
	sort.SliceStable(order, func(i, j int) bool {
		if !order[i].lastSeen.Equal(order[j].lastSeen) {
			return order[i].lastSeen.After(order[j].lastSeen)
		}
		return order[i].command < order[j].command
	})

	n := clampLimit(in.Limit, defaultLimit, maxLimit)
	out := howDidIRunOut{Patterns: make([]recallPattern, 0, n), ScanTruncated: truncated}
	for _, g := range order {
		if len(out.Patterns) >= n {
			break
		}
		cmd, cut := truncate(g.command, commandMaxChars)
		out.Patterns = append(out.Patterns, recallPattern{
			Command:   cmd,
			Truncated: cut,
			Count:     g.count,
			LastSeen:  g.lastSeen.UTC().Format(time.RFC3339),
		})
	}
	return nil, out, nil
}

// firstProgram extracts the invoked program's basename from a command line: it
// skips leading VAR=val environment-assignment tokens (shell semantics: env
// assignments precede the program), takes the first remaining
// whitespace-delimited token, and returns its path basename. So
// "FOO=1 /usr/bin/git status" -> "git". Returns "" for a blank command line.
func firstProgram(cmdline string) string {
	for _, tok := range strings.Fields(cmdline) {
		if isEnvAssignment(tok) {
			continue
		}
		return filepath.Base(tok)
	}
	return ""
}

// isEnvAssignment reports whether tok is a shell env-assignment prefix like
// FOO=bar: a valid shell identifier (letters/underscore, then alnum/underscore)
// followed by '='. It does not treat a bare "=x" or a value with '=' in a later
// token as an assignment.
func isEnvAssignment(tok string) bool {
	eq := strings.IndexByte(tok, '=')
	if eq <= 0 {
		return false
	}
	for i := 0; i < eq; i++ {
		c := tok[i]
		switch {
		case c == '_':
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z':
		case i > 0 && c >= '0' && c <= '9':
		default:
			return false
		}
	}
	return true
}

// quotePlaceholder replaces a quoted substring in a grouping key so lines that
// differ only inside quotes collapse together.
const quotePlaceholder = "\"…\""

// maskQuoted builds a grouping key for a command line by replacing every single-
// or double-quoted substring with a fixed placeholder, so invocations that
// differ only inside quotes (e.g. commit messages) collapse to one pattern.
// Heuristic (deterministic, not a full shell parser): scan left to right; at an
// opening ' or ", emit the placeholder and skip through the matching closing
// quote (or end of string if unterminated); copy everything outside quotes
// verbatim. Backslash escapes are not interpreted.
func maskQuoted(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); {
		c := s[i]
		if c == '\'' || c == '"' {
			b.WriteString(quotePlaceholder)
			i++
			for i < len(s) && s[i] != c {
				i++
			}
			if i < len(s) {
				i++ // consume the closing quote
			}
			continue
		}
		b.WriteByte(c)
		i++
	}
	return b.String()
}
