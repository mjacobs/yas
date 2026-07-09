package webui_test

// The token grammar's source of truth is testdata/token-vectors.json: search-box
// input -> expected /v1/search params. Two consumers keep it honest:
//   - this Go test replays every vector's expected params against the real
//     query API handler (each must be accepted, HTTP 200), so the table can
//     never drift from the contract;
//   - the JS parser (static/tokens.js) is table-tested against the same file
//     via `node --test` (optional, not part of `make test`).

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"testing"

	"github.com/mjacobs/yas/internal/queryapi"
	"github.com/mjacobs/yas/internal/record"
	"github.com/mjacobs/yas/internal/store"
)

type vector struct {
	Name   string            `json:"name"`
	Input  string            `json:"input"`
	Params map[string]string `json:"params"`
}

type stubSearcher struct{}

func (stubSearcher) Search(context.Context, store.Query) ([]record.Record, error) {
	return []record.Record{}, nil
}

func TestTokenVectors_ParamsAcceptedByQueryAPI(t *testing.T) {
	raw, err := os.ReadFile("testdata/token-vectors.json")
	if err != nil {
		t.Fatalf("read vectors: %v", err)
	}
	var vectors []vector
	if err := json.Unmarshal(raw, &vectors); err != nil {
		t.Fatalf("parse vectors: %v", err)
	}
	if len(vectors) == 0 {
		t.Fatal("no vectors")
	}

	h := queryapi.NewHandler(stubSearcher{})
	for _, vec := range vectors {
		q := url.Values{}
		for k, val := range vec.Params {
			q.Set(k, val)
		}
		req := httptest.NewRequest(http.MethodGet, "/v1/search?"+q.Encode(), nil)
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Errorf("%s (%q): params %v rejected by /v1/search: %d %s",
				vec.Name, vec.Input, vec.Params, rr.Code, rr.Body.String())
		}
	}
}
