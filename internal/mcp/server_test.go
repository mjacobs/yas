package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/mjacobs/yas/internal/record"
)

// newInMemoryClient connects a real MCP client to srv over an in-memory
// transport and returns the client session (cleaned up on test end).
func newInMemoryClient(t *testing.T, srv *mcp.Server) *mcp.ClientSession {
	t.Helper()
	ctx := context.Background()
	st, ct := mcp.NewInMemoryTransports()
	ss, err := srv.Connect(ctx, st, nil)
	if err != nil {
		t.Fatalf("server connect: %v", err)
	}
	t.Cleanup(func() { _ = ss.Wait() })
	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "v0"}, nil)
	cs, err := client.Connect(ctx, ct, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

func TestNewServer_RegistersReadOnlyTools(t *testing.T) {
	srv := newServer(ServeOptions{Search: &fakeSearcher{}})
	cs := newInMemoryClient(t, srv)
	tools, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	names := map[string]bool{}
	for _, tl := range tools.Tools {
		names[tl.Name] = true
		if tl.Annotations == nil || !tl.Annotations.ReadOnlyHint {
			t.Errorf("tool %s not annotated read-only", tl.Name)
		}
	}
	for _, want := range []string{
		ToolSearchCommands, ToolRecentCommands, ToolWhatFailed, ToolCommandStatus,
		ToolFailureSummary, ToolHowDidIRun,
	} {
		if !names[want] {
			t.Errorf("missing tool %s", want)
		}
	}
	if len(tools.Tools) != 6 {
		t.Errorf("want 6 tools, got %d", len(tools.Tools))
	}
}

func TestCallSearchCommands_EndToEnd(t *testing.T) {
	fs := &fakeSearcher{recs: []record.Record{
		{ID: "r1", Command: "git status", ExitCode: ptr(0), StartTime: base, CreatedAt: base},
	}}
	srv := newServer(ServeOptions{Search: fs})
	cs := newInMemoryClient(t, srv)
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      ToolSearchCommands,
		Arguments: map[string]any{"query": "git", "limit": 5},
	})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if res.IsError {
		t.Fatalf("tool returned error: %+v", res.Content)
	}
	if fs.last.Text != "git" || fs.last.Limit != 5 {
		t.Errorf("args not passed through: %+v", fs.last)
	}
	b, _ := json.Marshal(res.StructuredContent)
	if !strings.Contains(string(b), "git status") {
		t.Errorf("result missing command: %s", b)
	}
}

func TestIsCleanStdioShutdown(t *testing.T) {
	cases := []struct {
		err  error
		want bool
	}{
		{nil, true},
		{context.Canceled, true},
		{io.EOF, true},
		{fmt.Errorf("wrap: %w", io.EOF), true},
		{errors.New("server is closing: EOF"), true},
		{errors.New("connection closed"), true},
		{errors.New("boom"), false},
		{errors.New("open store: permission denied"), false},
	}
	for _, c := range cases {
		if got := isCleanStdioShutdown(c.err); got != c.want {
			t.Errorf("isCleanStdioShutdown(%v) = %v, want %v", c.err, got, c.want)
		}
	}
}
