// Package syncclient is the client side of the sync protocol: it pushes local
// records to yas-server and pulls records by seq cursor, authenticating with a
// static bearer token. The orchestration loop lives in the yas agent.
package syncclient

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/mjacobs/yas/internal/record"
	"github.com/mjacobs/yas/internal/syncproto"
)

// Client talks to a yas-server sync API.
type Client struct {
	baseURL string
	token   string
	hc      *http.Client
}

// New returns a Client for the server at baseURL using the given bearer token.
func New(baseURL, token string) *Client {
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		token:   token,
		hc:      &http.Client{Timeout: 30 * time.Second},
	}
}

// Push uploads records and returns the server's acknowledgement. Backlogs the
// server would reject in one request — more than syncproto.MaxPushRecords
// records, or an encoded body over syncproto.MaxPushBodyBytes — are sent as
// consecutive chunked pushes: Accepted is summed and HighSeq taken from the
// final chunk. A chunk failure aborts with an error; chunks already pushed
// are harmless (idempotent upserts, retried next sync).
func (c *Client) Push(ctx context.Context, recs []record.Record) (syncproto.PushResponse, error) {
	var out syncproto.PushResponse
	for {
		n, err := nextChunkLen(recs)
		if err != nil {
			return syncproto.PushResponse{}, err
		}
		resp, err := c.pushChunk(ctx, recs[:n])
		if err != nil {
			return syncproto.PushResponse{}, err
		}
		out.Accepted += resp.Accepted
		out.HighSeq = resp.HighSeq
		recs = recs[n:]
		if len(recs) == 0 {
			return out, nil
		}
	}
}

// nextChunkLen returns how many leading records fit in one push under both
// server caps: the record count (syncproto.MaxPushRecords) and the encoded
// body size (syncproto.MaxPushBodyBytes). The size model is exact — the
// PushRequest body is `{"records":[` + records joined by "," + `]}`. A
// non-empty backlog always yields at least one record so a chunk can never
// stall; a lone over-cap record cannot occur for records that pass
// record.Validate, whose per-field caps (MaxCommandBytes + MaxFieldBytes)
// bound a valid record's worst-case encoded JSON at ~1.7 MiB against the
// 8 MiB body cap.
func nextChunkLen(recs []record.Record) (int, error) {
	size := len(`{"records":[]}`)
	n := 0
	for i := range recs {
		if n == syncproto.MaxPushRecords {
			break
		}
		b, err := json.Marshal(recs[i])
		if err != nil {
			return 0, err
		}
		add := len(b)
		if n > 0 {
			add++ // "," separator
		}
		if n > 0 && size+add > syncproto.MaxPushBodyBytes {
			break
		}
		size += add
		n++
	}
	return n, nil
}

// pushChunk performs one push request for at most syncproto.MaxPushRecords.
func (c *Client) pushChunk(ctx context.Context, recs []record.Record) (syncproto.PushResponse, error) {
	var out syncproto.PushResponse
	body, err := json.Marshal(syncproto.PushRequest{Records: recs})
	if err != nil {
		return out, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/v1/sync/push", bytes.NewReader(body))
	if err != nil {
		return out, err
	}
	req.Header.Set("Content-Type", "application/json")
	return out, c.do(req, &out)
}

// Pull fetches up to limit records with seq greater than since.
func (c *Client) Pull(ctx context.Context, since int64, limit int) (syncproto.PullResponse, error) {
	var out syncproto.PullResponse
	url := fmt.Sprintf("%s/v1/sync/pull?since=%d&limit=%d", c.baseURL, since, limit)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return out, err
	}
	return out, c.do(req, &out)
}

func (c *Client) do(req *http.Request, out any) error {
	req.Header.Set("Authorization", "Bearer "+c.token)
	resp, err := c.hc.Do(req) //nolint:gosec // URL is the user's own configured server, not untrusted input
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return fmt.Errorf("%s %s: %s: %s", req.Method, req.URL.Path, resp.Status, strings.TrimSpace(string(b)))
	}
	return json.NewDecoder(resp.Body).Decode(out)
}
