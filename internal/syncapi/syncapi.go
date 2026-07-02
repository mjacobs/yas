// Package syncapi serves the client<->server sync API (push/pull) over the
// server's durable store. Every /v1 route is guarded by a static bearer token.
// The wire types live in internal/syncproto so client and server share them.
package syncapi

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"strconv"

	"github.com/mjacobs/yas/internal/record"
	"github.com/mjacobs/yas/internal/syncproto"
)

// Backend is the durable-store capability the sync API needs. The Postgres
// source-of-record implements it.
type Backend interface {
	// Put upserts pushed records (last-writer-wins on the mutable fields).
	Put(ctx context.Context, recs ...record.Record) error
	// HighSeq returns the highest seq currently assigned.
	HighSeq(ctx context.Context) (int64, error)
	// Since returns up to limit records with seq greater than seq, ordered by
	// seq ascending, plus the highest seq returned (or seq if none).
	Since(ctx context.Context, seq int64, limit int) ([]record.Record, int64, error)
}

// NewHandler builds the sync API handler backed by b and guarded by token.
func NewHandler(b Backend, token string) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/sync/push", pushHandler(b))
	mux.HandleFunc("GET /v1/sync/pull", pullHandler(b))
	return requireBearer(token, mux)
}

// requireBearer rejects any request lacking a matching `Authorization: Bearer`
// token (constant-time compare). An empty configured token denies everything.
func requireBearer(token string, next http.Handler) http.Handler {
	const prefix = "Bearer "
	want := []byte(token)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := r.Header.Get("Authorization")
		ok := token != "" && len(h) > len(prefix) && h[:len(prefix)] == prefix &&
			subtle.ConstantTimeCompare([]byte(h[len(prefix):]), want) == 1
		if !ok {
			// One line per failure so probing (or a misconfigured client) is
			// visible in the journal. RemoteAddr comes from the connection, not
			// a header, so the line carries no attacker-controlled bytes (no log
			// injection); never log the presented credential.
			log.Printf("syncapi: auth failed from %s", r.RemoteAddr) //nolint:gosec // G706: RemoteAddr is host:port from the TCP conn, not attacker-controlled bytes
			w.Header().Set("WWW-Authenticate", "Bearer")
			writeJSON(w, http.StatusUnauthorized, errorResponse{Error: "unauthorized"})
			return
		}
		next.ServeHTTP(w, r)
	})
}

func pushHandler(b Backend) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Reject a declared-oversized body before reading a byte of it.
		if r.ContentLength > syncproto.MaxPushBodyBytes {
			writeBodyTooLarge(w)
			return
		}
		// The body cap (sizing arithmetic on the constant) is enforced by
		// MaxBytesReader; overflow surfaces as a clean 413, never a 500.
		r.Body = http.MaxBytesReader(w, r.Body, syncproto.MaxPushBodyBytes)
		dec := json.NewDecoder(r.Body)
		recs, err := decodePushRecords(dec)
		if err != nil {
			switch {
			case isMaxBytes(err):
				writeBodyTooLarge(w)
			case errors.Is(err, errTooManyRecords):
				writeJSON(w, http.StatusBadRequest, errorResponse{
					Error: "too many records: max " + strconv.Itoa(syncproto.MaxPushRecords) + " per push",
				})
			default:
				writeJSON(w, http.StatusBadRequest, errorResponse{Error: "invalid JSON body: " + err.Error()})
			}
			return
		}
		// The body must be exactly one JSON document: smuggled trailing data
		// is a 400, and over-cap padding the decoder alone would never read
		// still counts against the byte cap (413).
		if _, err := dec.Token(); !errors.Is(err, io.EOF) {
			if isMaxBytes(err) {
				writeBodyTooLarge(w)
				return
			}
			writeJSON(w, http.StatusBadRequest, errorResponse{Error: "trailing data after JSON body"})
			return
		}
		for i := range recs {
			if err := recs[i].Validate(); err != nil {
				writeJSON(w, http.StatusBadRequest, errorResponse{Error: err.Error()})
				return
			}
		}
		if err := b.Put(r.Context(), recs...); err != nil {
			serverError(w, "push: put", err)
			return
		}
		high, err := b.HighSeq(r.Context())
		if err != nil {
			serverError(w, "push: high_seq", err)
			return
		}
		writeJSON(w, http.StatusOK, syncproto.PushResponse{Accepted: len(recs), HighSeq: high})
	}
}

func pullHandler(b Backend) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		since, err := parseInt64(q.Get("since"), 0)
		if err != nil || since < 0 {
			writeJSON(w, http.StatusBadRequest, errorResponse{Error: "invalid since: must be a non-negative integer"})
			return
		}
		limit, err := parseInt(q.Get("limit"), syncproto.DefaultPullLimit)
		if err != nil || limit < 0 {
			writeJSON(w, http.StatusBadRequest, errorResponse{Error: "invalid limit: must be a non-negative integer"})
			return
		}
		if limit == 0 || limit > syncproto.DefaultPullLimit {
			limit = syncproto.DefaultPullLimit
		}
		recs, next, err := b.Since(r.Context(), since, limit)
		if err != nil {
			serverError(w, "pull: since", err)
			return
		}
		writeJSON(w, http.StatusOK, syncproto.PullResponse{
			Records: recs,
			NextSeq: next,
			Done:    len(recs) < limit,
		})
	}
}

// parseInt64 parses s as int64, returning def for the empty string.
func parseInt64(s string, def int64) (int64, error) {
	if s == "" {
		return def, nil
	}
	return strconv.ParseInt(s, 10, 64)
}

// parseInt parses s as int, returning def for the empty string.
func parseInt(s string, def int) (int, error) {
	if s == "" {
		return def, nil
	}
	return strconv.Atoi(s)
}

// errTooManyRecords marks a push whose records array crossed the batch cap.
var errTooManyRecords = errors.New("too many records")

// decodePushRecords stream-decodes a syncproto.PushRequest body — a JSON
// object whose "records" key holds the array — enforcing the batch cap per
// element. Decoding the whole array first and counting afterwards would let
// an under-byte-cap body packed with tiny records (3 bytes each encoded)
// amplify into ~70x its wire size in decoded structs before rejection; here
// decoding stops the moment record syncproto.MaxPushRecords+1 begins. Unknown
// object keys are skipped and a null/absent records key yields nil, matching
// encoding/json's behavior for the same wire type.
func decodePushRecords(dec *json.Decoder) ([]record.Record, error) {
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return nil, errors.New("expected a JSON object")
	}
	var recs []record.Record
	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			return nil, err
		}
		if key, _ := keyTok.(string); key != "records" {
			var skip json.RawMessage
			if err := dec.Decode(&skip); err != nil {
				return nil, err
			}
			continue
		}
		recs = nil // duplicate "records" keys: last one wins, like encoding/json
		tok, err := dec.Token()
		if err != nil {
			return nil, err
		}
		if tok == nil { // "records": null
			continue
		}
		if d, ok := tok.(json.Delim); !ok || d != '[' {
			return nil, errors.New("records must be an array")
		}
		for dec.More() {
			if len(recs) == syncproto.MaxPushRecords {
				return nil, errTooManyRecords
			}
			var rec record.Record
			if err := dec.Decode(&rec); err != nil {
				return nil, err
			}
			recs = append(recs, rec)
		}
		if _, err := dec.Token(); err != nil { // consume ']'
			return nil, err
		}
	}
	if _, err := dec.Token(); err != nil { // consume '}'
		return nil, err
	}
	return recs, nil
}

// isMaxBytes reports whether err is http.MaxBytesReader's over-limit error.
func isMaxBytes(err error) bool {
	var tooBig *http.MaxBytesError
	return errors.As(err, &tooBig)
}

// writeBodyTooLarge answers 413 in the API's JSON error shape.
func writeBodyTooLarge(w http.ResponseWriter) {
	writeJSON(w, http.StatusRequestEntityTooLarge, errorResponse{
		Error: "request body too large: max " + strconv.Itoa(syncproto.MaxPushBodyBytes) + " bytes",
	})
}

// serverError logs the real error server-side and returns a generic 500 so a
// client never sees database/internal details.
func serverError(w http.ResponseWriter, op string, err error) {
	log.Printf("syncapi: %s: %v", op, err)
	writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "internal server error"})
}

type errorResponse struct {
	Error string `json:"error"`
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
