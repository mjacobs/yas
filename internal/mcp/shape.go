// Package mcp serves yas's command history to AI agents over the Model Context
// Protocol (read-only tools), as a thin adapter over the same Searcher seam the
// query API targets — so it speaks the record/query contract, never the schema.
package mcp

import (
	"time"
	"unicode/utf8"

	"github.com/mjacobs/yas/internal/record"
)

const (
	defaultLimit = 20
	maxLimit     = 100
	// commandMaxChars caps a command string in tool output; truncation is
	// flagged so a client can re-fetch the full record by id if it needs it.
	commandMaxChars = 4000
)

// commandOut is the compact per-record shape every tool returns. It mirrors the
// record JSON contract (the stable surface), not the DB schema.
type commandOut struct {
	ID         string `json:"id"`
	Command    string `json:"command"`
	Truncated  bool   `json:"truncated,omitempty"`
	CWD        string `json:"cwd,omitempty"`
	Host       string `json:"host,omitempty"`
	Session    string `json:"session,omitempty"`
	Executor   string `json:"executor,omitempty"`
	ExitCode   *int   `json:"exit_code,omitempty"`
	DurationMS *int64 `json:"duration_ms,omitempty"`
	StartTime  string `json:"start_time"`
}

func toCommandOut(r record.Record) commandOut {
	cmd, cut := truncate(r.Command, commandMaxChars)
	return commandOut{
		ID:         r.ID,
		Command:    cmd,
		Truncated:  cut,
		CWD:        r.CWD,
		Host:       r.Hostname,
		Session:    r.Session,
		Executor:   r.Executor,
		ExitCode:   r.ExitCode,
		DurationMS: r.DurationMS,
		StartTime:  r.StartTime.UTC().Format(time.RFC3339),
	}
}

// clampLimit normalizes a requested page size into [1, max], using def when the
// request is unset or out of range.
func clampLimit(requested, def, max int) int {
	if requested <= 0 || requested > max {
		return def
	}
	return requested
}

// truncate cuts s to at most max runes on a rune boundary, returning the
// (possibly shortened) string and whether truncation occurred.
func truncate(s string, max int) (string, bool) {
	if max <= 0 || utf8.RuneCountInString(s) <= max {
		return s, false
	}
	n := 0
	for i := range s {
		if n == max {
			return s[:i], true
		}
		n++
	}
	return s, false
}

// parseOptTime parses an optional RFC3339 timestamp; "" yields the zero time.
func parseOptTime(s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, nil
	}
	return time.Parse(time.RFC3339, s)
}
