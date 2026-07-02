package mcp

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Tool names. The same constant registers a tool and refers to it in tests.
const (
	ToolSearchCommands = "search_commands"
	ToolRecentCommands = "recent_commands"
	ToolWhatFailed     = "what_failed"
	ToolCommandStatus  = "command_status"
)

// ServeOptions configures the MCP server. Search is required; Version is
// reported in the server's implementation info; Now is injectable (reserved for
// a future self-reference window). Token, when non-empty, requires every
// StreamableHTTP request to carry "Authorization: Bearer <Token>" (no effect on
// stdio); the command layer sets it for non-loopback binds.
type ServeOptions struct {
	Search  Searcher
	Version string
	Now     func() time.Time
	Token   string
}

// newServer builds an MCP server with all read-only command-history tools
// registered. Shared by the stdio and StreamableHTTP transports.
func newServer(opts ServeOptions) *mcp.Server {
	version := opts.Version
	if version == "" {
		version = "dev"
	}
	s := mcp.NewServer(&mcp.Implementation{
		Name:    "yas",
		Title:   "yas command history",
		Version: version,
	}, nil)

	t := &toolset{search: opts.Search, now: opts.Now}
	readOnly := &mcp.ToolAnnotations{ReadOnlyHint: true}

	mcp.AddTool(s, &mcp.Tool{
		Name: ToolSearchCommands,
		Description: "Full-text search across your shell-command history from every synced machine. " +
			"Filter by host, cwd, session, exit code, executor (who/what ran it: a name, or " +
			"$all-agent / $all-human), failed-only, and an RFC3339 time window; newest-first. " +
			"Use it to answer 'have I run this before?', 'how did I do X here?', or 'what was that command'.",
		Annotations: readOnly,
	}, t.searchCommands)

	mcp.AddTool(s, &mcp.Tool{
		Name: ToolRecentCommands,
		Description: "The most recent commands (newest-first), optionally scoped by host, cwd, or " +
			"executor. Use search_commands when looking for specific content.",
		Annotations: readOnly,
	}, t.recentCommands)

	mcp.AddTool(s, &mcp.Tool{
		Name: ToolWhatFailed,
		Description: "Recent commands that exited non-zero (failures), optionally scoped by host, cwd, " +
			"or since an RFC3339 time. Use it to see what has been going wrong.",
		Annotations: readOnly,
	}, t.whatFailed)

	mcp.AddTool(s, &mcp.Tool{
		Name: ToolCommandStatus,
		Description: "Look up one command by id (e.g. from a previous result): its exit code, duration, " +
			"cwd, host, and executor. found is false when the id is unknown.",
		Annotations: readOnly,
	}, t.commandStatus)

	return s
}

// newHTTPHandler builds the StreamableHTTP handler. Passing nil options keeps
// the SDK's default DNS-rebinding protection (a loopback request with a
// non-loopback Host header is rejected), then withBearerAuth adds token auth for
// non-loopback binds (see the command layer).
func newHTTPHandler(opts ServeOptions) http.Handler {
	srv := newServer(opts)
	var handler http.Handler = mcp.NewStreamableHTTPHandler(
		func(*http.Request) *mcp.Server { return srv }, nil)
	return withBearerAuth(handler, opts.Token)
}

// withBearerAuth wraps next so every request must present
// "Authorization: Bearer <token>". When token is empty the handler is returned
// unwrapped (loopback binds run without listener auth). The comparison is
// constant-time to avoid leaking the token via timing.
func withBearerAuth(next http.Handler, token string) http.Handler {
	if token == "" {
		return next
	}
	want := []byte("Bearer " + token)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got := []byte(r.Header.Get("Authorization"))
		if subtle.ConstantTimeCompare(got, want) != 1 {
			w.Header().Set("WWW-Authenticate", "Bearer")
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// ServeStdio runs the MCP server over stdio. It blocks until stdin closes (the
// client disconnected) or ctx is cancelled — both the normal end of a stdio
// server's life, returning nil. Only an unexpected transport failure is an error.
func ServeStdio(ctx context.Context, opts ServeOptions) error {
	err := newServer(opts).Run(ctx, &mcp.StdioTransport{})
	if isCleanStdioShutdown(err) {
		return nil
	}
	return fmt.Errorf("serve MCP over stdio: %w", err)
}

// isCleanStdioShutdown reports whether a Run error is the normal end of a stdio
// session (client disconnect or signal) rather than a failure. When stdin closes
// with requests in flight the SDK surfaces an internal jsonrpc2 "server is
// closing" wire error that does not wrap io.EOF traversably, so that case is
// matched on its stable message as a last resort.
func isCleanStdioShutdown(err error) bool {
	if err == nil {
		return true
	}
	if errors.Is(err, context.Canceled) ||
		errors.Is(err, io.EOF) ||
		errors.Is(err, mcp.ErrConnectionClosed) {
		return true
	}
	msg := err.Error()
	return strings.Contains(msg, "server is closing") ||
		strings.Contains(msg, "connection closed")
}

// ServeHTTP runs the MCP server over StreamableHTTP on addr. When ctx is
// cancelled the HTTP server is shut down gracefully so in-flight tool calls can
// finish. addr must already be validated as a safe bind address (cmd layer).
func ServeHTTP(ctx context.Context, opts ServeOptions, addr string) error {
	// ReadHeaderTimeout bounds the header read (Slowloris guard) without
	// capping the response body, so StreamableHTTP's long-lived streaming
	// responses are not truncated.
	httpServer := &http.Server{
		Addr:              addr,
		Handler:           newHTTPHandler(opts),
		ReadHeaderTimeout: 10 * time.Second,
	}
	fmt.Fprintf(os.Stderr, "yas mcp: serving on %s\n", addr)

	errCh := make(chan error, 1)
	go func() {
		err := httpServer.ListenAndServe()
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
			return
		}
		errCh <- nil
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = httpServer.Shutdown(shutdownCtx)
		return ctx.Err()
	}
}
