// Package histimport parses existing shell-history files into yas records so a
// new install can backfill its store. Imported records get a DETERMINISTIC id
// derived from their content + timestamp, so re-running an import upserts the
// same rows (idempotent) rather than duplicating them.
package histimport

import (
	"bufio"
	"crypto/sha256"
	"encoding/binary"
	"io"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/mjacobs/yas/internal/record"
)

// extLine matches a zsh extended-history entry: ": <epoch>:<elapsed>;<command>".
var extLine = regexp.MustCompile(`^: (\d+):(\d+);(.*)$`)

// ParseZsh reads a zsh history file (extended ": ts:elapsed;cmd" entries, or
// plain one-command-per-line) and returns one record per command, stamped with
// host and executor=human. Multiline commands — which zsh stores as physical
// lines joined by a trailing backslash — are rejoined with real newlines.
//
// Extended entries use the stored epoch as start_time and elapsed*1000 as
// duration_ms. Plain lines (no ": ts:el;" marker) get a small synthetic,
// monotonic start_time so the record stays valid and ordering is stable across
// re-imports.
func ParseZsh(r io.Reader, host string) ([]record.Record, error) {
	br := bufio.NewReader(r)
	// Non-nil so a zero-entry file yields [] (the store/JSON contract), not nil.
	out := []record.Record{}
	var plainSeq int64

	eof := false
	readLine := func() string {
		s, err := br.ReadString('\n')
		if err != nil {
			eof = true
		}
		return strings.TrimRight(s, "\r\n")
	}

	for !eof {
		line := readLine()
		if line == "" {
			continue // blank separator (or trailing EOF)
		}

		var (
			ts      time.Time
			cmd     string
			elapsed int64
			ext     bool
		)
		if m := extLine.FindStringSubmatch(line); m != nil {
			sec, _ := strconv.ParseInt(m[1], 10, 64)
			elapsed, _ = strconv.ParseInt(m[2], 10, 64)
			ts = time.Unix(sec, 0)
			cmd = m[3]
			ext = true
		} else {
			plainSeq++
			ts = time.UnixMilli(plainSeq) // nonzero + monotonic + deterministic
			cmd = line
		}

		// Trailing backslash = the command continues on the next physical line.
		for strings.HasSuffix(cmd, `\`) && !eof {
			cmd = strings.TrimSuffix(cmd, `\`) + "\n" + readLine()
		}
		if strings.TrimSpace(cmd) == "" {
			continue
		}

		rec := record.Record{
			ID:        stableID(ts, cmd, host),
			Command:   cmd,
			Hostname:  host,
			Executor:  record.ExecutorHuman,
			StartTime: ts,
			CreatedAt: ts,
		}
		if ext {
			ms := elapsed * 1000
			rec.DurationMS = &ms
		}
		out = append(out, rec)
	}
	return out, nil
}

// stableID builds a UUIDv7-shaped id whose 48-bit timestamp prefix is the
// entry's millisecond time (so imported rows stay time-sortable like live ones)
// and whose remaining bits are a content hash of (ms, host, command). Identical
// entries therefore map to the same id, making re-import an idempotent upsert.
func stableID(ts time.Time, command, host string) string {
	return stableIDN(ts, command, host, 0)
}

// stableIDN is stableID with an occurrence discriminator for sources that keep
// DISTINCT entries a plain (ts, host, command) key cannot tell apart — e.g.
// atuin's same-second rapid repeats. n=0 hashes exactly like stableID, so the
// first occurrence stays dedupable against other sources of the same event.
func stableIDN(ts time.Time, command, host string, n int) string {
	var b [16]byte
	var tb [8]byte
	ms := ts.UnixMilli()
	binary.BigEndian.PutUint64(tb[:], uint64(ms))
	copy(b[0:6], tb[2:]) // low 48 bits of the ms timestamp

	input := strconv.FormatInt(ms, 10) + "\x00" + host + "\x00" + command
	if n > 0 {
		input += "\x00" + strconv.Itoa(n)
	}
	sum := sha256.Sum256([]byte(input))
	copy(b[6:16], sum[0:10])

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
