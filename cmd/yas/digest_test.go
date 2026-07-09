package main

// The digest computation and JSON envelope are tested in internal/digest —
// these tests cover the CLI shell: the human renderer, the indented CLI JSON
// framing, and flag parsing.

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/mjacobs/yas/internal/digest"
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

func TestRenderDigest_JSONEmptyIsBracketsNotNull(t *testing.T) {
	since := time.Date(2026, 7, 2, 0, 0, 0, 0, time.UTC)
	until := time.Date(2026, 7, 2, 14, 33, 0, 0, time.UTC)
	d := digest.Build(nil, since, until)

	var buf bytes.Buffer
	if err := renderDigest(&buf, d, true, newCLIStyles(false)); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.Contains(out, `"groups": []`) {
		t.Fatalf("empty groups must serialize as [] not null:\n%s", out)
	}
	if strings.Contains(out, "null") {
		t.Fatalf("digest JSON must never contain null:\n%s", out)
	}

	var env digest.Envelope
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
	d := digest.Build(recs, since, until)

	var buf bytes.Buffer
	if err := renderDigest(&buf, d, true, newCLIStyles(false)); err != nil {
		t.Fatal(err)
	}
	// A group with no failures must still emit failed_commands: [] (never null).
	if !strings.Contains(buf.String(), `"failed_commands": []`) {
		t.Fatalf("a failure-free group must emit failed_commands []:\n%s", buf.String())
	}

	var env digest.Envelope
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
	d := digest.Build(nil, since, until)

	var buf bytes.Buffer
	if err := renderDigest(&buf, d, false, newCLIStyles(false)); err != nil {
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
	d := digest.Build(recs, since, until)

	var buf bytes.Buffer
	if err := renderDigest(&buf, d, false, newCLIStyles(false)); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, want := range []string{"host-a", "/proj", "2 commands", "1 failed", "go test ./..."} {
		if !strings.Contains(out, want) {
			t.Fatalf("human output missing %q:\n%s", want, out)
		}
	}
}

// With color on the human digest carries ANSI escapes (the accented title, the
// red failure count and ✗); with color off it stays plain. The failing command
// text is present either way. Mirrors the search color tests.
func TestRenderDigest_HumanColor(t *testing.T) {
	base := time.Date(2026, 7, 2, 10, 0, 0, 0, time.UTC)
	since := time.Date(2026, 7, 2, 0, 0, 0, 0, time.UTC)
	until := time.Date(2026, 7, 3, 0, 0, 0, 0, time.UTC)
	d := digest.Build([]record.Record{
		drec("host-a", "/proj", "make lint", ptr(1), base),
	}, since, until)

	var plain, color bytes.Buffer
	if err := renderDigest(&plain, d, false, newCLIStyles(false)); err != nil {
		t.Fatal(err)
	}
	if err := renderDigest(&color, d, false, newCLIStyles(true)); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(plain.String(), "\x1b[") {
		t.Fatalf("plain digest must not contain ANSI escapes:\n%q", plain.String())
	}
	if !strings.Contains(color.String(), "\x1b[") {
		t.Fatalf("colored digest should contain ANSI escapes:\n%q", color.String())
	}
	if !strings.Contains(plain.String(), "make lint") || !strings.Contains(color.String(), "make lint") {
		t.Fatalf("digest must keep the failing command text:\nplain=%q\ncolor=%q", plain.String(), color.String())
	}
}

func TestParseDigestArgs_DefaultWindowIsToday(t *testing.T) {
	now := time.Date(2026, 7, 2, 14, 30, 45, 0, time.Local)

	since, until, asJSON, _, err := parseDigestArgs(nil, now)
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

	since, until, asJSON, _, err := parseDigestArgs(
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
		if _, _, _, _, err := parseDigestArgs(args, now); err == nil {
			t.Fatalf("%s: expected an error for args %v", name, args)
		}
	}
}
