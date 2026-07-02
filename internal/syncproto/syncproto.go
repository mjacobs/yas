// Package syncproto defines the wire types for the client<->server sync API.
// Both yas and yas-server import these so the contract stays in one place.
//
// The protocol is deliberately minimal:
//
//	POST /v1/sync/push  -> body PushRequest,  reply PushResponse
//	GET  /v1/sync/pull?since=<seq>&limit=<n>  -> reply PullResponse
//
// The server assigns each row a monotonic seq; clients track the highest seq
// they have pulled and ask for everything after it. seq never appears inside a
// Record — it is transport metadata, returned alongside.
package syncproto

import "github.com/mjacobs/yas/internal/record"

// DefaultPullLimit caps how many records a single pull returns.
const DefaultPullLimit = 1000

// MaxPushRecords caps how many records a single push may carry. The server
// rejects bigger batches with a 400, and the client chunks larger backlogs
// into pushes of at most this size, so the pair can never wedge on a batch
// that only one side accepts. 400 keeps a full batch small enough to validate
// and upsert in one comfortable transaction.
const MaxPushRecords = 400

// MaxPushBodyBytes caps the encoded size of one push request body. Both sides
// honor it — the server cuts off bigger bodies with a 413, and the client
// chunks pushes so the encoded body stays under it (as well as under
// MaxPushRecords) — so a legal backlog can never wedge on a request only one
// side accepts. Arithmetic: a full batch of MaxPushRecords (400) records at a
// generous ~4 KiB of JSON each is ~1.6 MiB; 8 MiB is ~5x headroom, letting a
// full batch average ~20 KiB per record while a hostile or runaway request
// stays bounded. A single record always fits: even a record.MaxCommandBytes
// command at worst-case 6x JSON escaping is ~1.5 MiB.
const MaxPushBodyBytes = 8 << 20 // 8 MiB

// PushRequest carries new or updated local records up to the server.
type PushRequest struct {
	Records []record.Record `json:"records"`
}

// PushResponse acknowledges a push. HighSeq is the server's highest seq after
// the upsert, useful for logging/diagnostics.
type PushResponse struct {
	Accepted int   `json:"accepted"`
	HighSeq  int64 `json:"high_seq"`
}

// PullResponse returns records with seq greater than the requested cursor,
// ordered by seq ascending. NextSeq is the cursor to pass on the next pull.
// Done is true when the server has no more records beyond this page.
type PullResponse struct {
	Records []record.Record `json:"records"`
	NextSeq int64           `json:"next_seq"`
	Done    bool            `json:"done"`
}
