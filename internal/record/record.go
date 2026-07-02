// Package record defines the canonical shell-history event shared by the client
// agent, the sync protocol, and the server. It is the one type every other
// package agrees on, so it intentionally has no storage or transport concerns.
package record

import (
	"crypto/rand"
	"encoding/binary"
	"errors"
	"strconv"
	"strings"
	"time"
)

// Record is a single shell-history event: one command, captured at start and
// finalized when it completes. Records are immutable except for the fields only
// known after the command finishes (ExitCode, DurationMS) and the Deleted
// tombstone. Identity is the client-generated ID, which makes sync upserts
// idempotent across machines (last-writer-wins on the mutable fields).
type Record struct {
	ID         string    `json:"id"`                    // UUIDv7, client-generated, global dedup key
	Command    string    `json:"command"`               // the command line as entered
	CWD        string    `json:"cwd,omitempty"`         // working directory at start
	Hostname   string    `json:"hostname,omitempty"`    // machine that ran it
	Session    string    `json:"session,omitempty"`     // shell session id (groups one shell's commands)
	Shell      string    `json:"shell,omitempty"`       // zsh | bash | fish
	Username   string    `json:"username,omitempty"`    // OS user
	ExitCode   *int      `json:"exit_code,omitempty"`   // nil until the command finishes
	StartTime  time.Time `json:"start_time"`            // when the command began
	DurationMS *int64    `json:"duration_ms,omitempty"` // nil until the command finishes
	CreatedAt  time.Time `json:"created_at"`            // when this record was first written
	Deleted    bool      `json:"deleted,omitempty"`     // tombstone for redaction/deletion
	Executor   string    `json:"executor,omitempty"`    // who/what ran it: "human" | "claude-code" | "codex" | "ci" | ...
	CorrID     string    `json:"corr_id,omitempty"`     // cross-tool correlation key (e.g. an agentsview session); reserved, nullable
}

// Finished reports whether the command has completed (exit code captured).
func (r Record) Finished() bool { return r.ExitCode != nil }

// ExecutorHuman is the canonical Executor value for a command a person typed. An
// empty Executor is treated as human (records predating the field).
const ExecutorHuman = "human"

// IsAgent reports whether the command was run by an agent/automation rather than
// a person (Executor set to something other than "human").
func (r Record) IsAgent() bool { return r.Executor != "" && r.Executor != ExecutorHuman }

// ContractFields is the frozen, ordered set of JSON keys a fully-populated Record
// serializes to — the v1 query-API contract. Adding, removing, or renaming a key
// is a breaking change: it must be a deliberate, versioned decision (bump
// queryapi.ContractVersion and update docs/api/query-api-v1.md).
var ContractFields = []string{
	"id", "command", "cwd", "hostname", "session", "shell", "username",
	"exit_code", "start_time", "duration_ms", "created_at", "deleted",
	"executor", "corr_id",
}

// MaxCommandBytes bounds Command so a hostile or buggy peer can't sync
// unbounded blobs into every replica. 256 KiB is deliberately extravagant:
// real interactive command lines — even huge pastes and heredocs — run to a
// few KiB, so no genuine command ever fails to record (the record path is
// sacred), while the worst single record stays comfortably bounded.
const MaxCommandBytes = 256 << 10 // 256 KiB

// MaxFieldBytes bounds every other string field (id, cwd, hostname, session,
// shell, username, executor, corr_id). 4 KiB is Linux PATH_MAX — the largest
// value any of these can genuinely hold is a cwd, and ids (UUIDv7, 36 bytes;
// kept a loose byte cap rather than a strict shape so pre-existing imported
// ids keep validating), hostnames (RFC max 253), session ids, usernames, and
// executor tokens are far smaller — so no real record ever fails to validate.
// Together with MaxCommandBytes this bounds a valid record's encoded JSON
// (worst-case 6x escaping is ~1.5 MiB + 8 x 24 KiB ~ 1.7 MiB), which the sync
// client relies on: a single valid record always fits under
// syncproto.MaxPushBodyBytes.
const MaxFieldBytes = 4 << 10 // 4 KiB

// Validate checks the invariants every stored record must satisfy.
func (r Record) Validate() error {
	switch {
	case r.ID == "":
		return errors.New("record: empty id")
	case strings.TrimSpace(r.Command) == "" && !r.Deleted:
		return errors.New("record: empty command")
	case len(r.Command) > MaxCommandBytes:
		return errors.New("record: command exceeds " + strconv.Itoa(MaxCommandBytes) + " bytes")
	case r.StartTime.IsZero():
		return errors.New("record: zero start_time")
	}
	for _, f := range []struct{ name, v string }{
		{"id", r.ID}, {"cwd", r.CWD}, {"hostname", r.Hostname},
		{"session", r.Session}, {"shell", r.Shell}, {"username", r.Username},
		{"executor", r.Executor}, {"corr_id", r.CorrID},
	} {
		if len(f.v) > MaxFieldBytes {
			return errors.New("record: " + f.name + " exceeds " + strconv.Itoa(MaxFieldBytes) + " bytes")
		}
	}
	return nil
}

// NewID returns a UUIDv7: a 48-bit millisecond timestamp followed by random
// bits. It is time-sortable (helps local ordering and is friendly to indexes)
// while remaining globally unique, so independent machines never collide.
// Implemented here to keep the skeleton dependency-free.
func NewID() string {
	var b [16]byte
	// 48-bit big-endian millisecond timestamp in bytes 0..5 (the high 2 bytes of
	// a millisecond Unix time are zero for any realistic date).
	var ts [8]byte
	binary.BigEndian.PutUint64(ts[:], uint64(time.Now().UnixMilli()))
	copy(b[0:6], ts[2:])
	if _, err := rand.Read(b[6:]); err != nil {
		panic("record: crypto/rand failed: " + err.Error())
	}
	b[6] = (b[6] & 0x0f) | 0x70 // version 7
	b[8] = (b[8] & 0x3f) | 0x80 // RFC 4122 variant
	return formatUUID(b)
}

const hexDigits = "0123456789abcdef"

// formatUUID renders 16 bytes as canonical 8-4-4-4-12 lowercase hex.
func formatUUID(b [16]byte) string {
	var dst [36]byte
	j := 0
	for i := 0; i < 16; i++ {
		if i == 4 || i == 6 || i == 8 || i == 10 {
			dst[j] = '-'
			j++
		}
		dst[j] = hexDigits[b[i]>>4]
		dst[j+1] = hexDigits[b[i]&0x0f]
		j += 2
	}
	return string(dst[:])
}
