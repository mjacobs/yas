package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"

	mcpserver "github.com/mjacobs/yas/internal/mcp"
)

// cmdMCP runs `yas mcp`: an MCP server exposing read-only command-history tools
// (search_commands, recent_commands, what_failed, command_status) over stdio
// (default) or StreamableHTTP (--http). It reads the local replica through the
// same Searcher seam the query API uses — no separate `yas serve` required.
func cmdMCP(args []string) {
	fs := flag.NewFlagSet("mcp", flag.ExitOnError)
	httpAddr := fs.String("http", "", "serve StreamableHTTP on this address instead of stdio (e.g. 127.0.0.1:8770; bare ports bind loopback)")
	allowInsecure := fs.Bool("http-allow-insecure", false, "allow --http to bind a non-loopback address (requires a configured token)")
	excludeCorrID := fs.String("exclude-corr-id", os.Getenv("YAS_CORR_ID"), "corr_id of the querying agent's own session to exclude from results (self-reference guard); defaults to $YAS_CORR_ID")
	_ = fs.Parse(args)

	st, cfg, closeStore := openStore()
	defer closeStore()

	// A long-lived MCP server must shut down cleanly on SIGINT/SIGTERM.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	opts := mcpserver.ServeOptions{Search: st, Version: version, ExcludeCorrID: *excludeCorrID}

	var err error
	if *httpAddr == "" {
		err = mcpserver.ServeStdio(ctx, opts)
	} else {
		addr, nerr := normalizeMCPAddr(*httpAddr, *allowInsecure)
		if nerr != nil {
			fmt.Fprintln(os.Stderr, "yas mcp:", nerr)
			os.Exit(2)
		}
		host, _, _ := net.SplitHostPort(addr)
		if !isLoopbackHost(host) {
			// A non-loopback listener is a network-reachable read surface over
			// your whole history; it must be authenticated.
			if cfg.Token == "" {
				fmt.Fprintln(os.Stderr, "yas mcp: non-loopback --http requires a token (set token or YAS_TOKEN)")
				os.Exit(2)
			}
			opts.Token = cfg.Token
		}
		err = mcpserver.ServeHTTP(ctx, opts, addr)
	}
	if err != nil && !errors.Is(err, context.Canceled) {
		fmt.Fprintln(os.Stderr, "yas mcp:", err)
		os.Exit(1)
	}
}

// normalizeMCPAddr canonicalises a --http argument and refuses a non-loopback
// bind unless allowInsecure is set. "8770" and ":8770" become loopback binds
// (Go's default for ":8770" is all-interfaces — the footgun this guards).
func normalizeMCPAddr(addr string, allowInsecure bool) (string, error) {
	addr = strings.TrimSpace(addr)
	if addr == "" {
		return "", errors.New("--http requires an address")
	}
	if !strings.Contains(addr, ":") {
		if _, err := strconv.Atoi(addr); err == nil {
			return "127.0.0.1:" + addr, nil
		}
		return "", fmt.Errorf("--http %q: not a port and not host:port", addr)
	}
	if strings.HasPrefix(addr, ":") {
		return "127.0.0.1" + addr, nil
	}
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return "", fmt.Errorf("--http %q: %w", addr, err)
	}
	if isLoopbackHost(host) {
		return addr, nil
	}
	if !allowInsecure {
		return "", fmt.Errorf("--http %q: refusing to bind a non-loopback address without --http-allow-insecure", addr)
	}
	return addr, nil
}

// isLoopbackHost reports whether host is a loopback address. An empty host is
// NOT loopback (Go binds all interfaces), guarding the "[]:8770" footgun.
func isLoopbackHost(host string) bool {
	switch host {
	case "":
		return false
	case "localhost":
		return true
	default:
		ip := net.ParseIP(host)
		return ip != nil && ip.IsLoopback()
	}
}
