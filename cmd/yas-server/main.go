// Command yas-server is the homelab sync hub: a small HTTP service backed by
// Postgres that accepts pushed history records and serves pulls by seq cursor.
// It serves only the sync API (push/pull), guarded by a static bearer token.
package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/mjacobs/yas/internal/config"
	"github.com/mjacobs/yas/internal/store/postgres"
	"github.com/mjacobs/yas/internal/syncapi"
)

var version = "0.0.0-dev"

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "version", "--version", "-v":
			fmt.Println("yas-server", version)
			return
		case "help", "-h", "--help":
			fmt.Fprint(os.Stderr, "usage: yas-server  (config via YAS_DATABASE_URL, YAS_ADDR, YAS_TOKEN)\n")
			return
		}
	}
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "yas-server:", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.LoadServer(os.Getenv("YAS_CONFIG"))
	if err != nil {
		return err
	}
	if cfg.Token == "" {
		return errors.New("a bearer token is required (set YAS_TOKEN) — refusing to serve the sync API unauthenticated")
	}

	// Bound the startup connect so a wedged or unreachable Postgres fails the
	// process fast instead of hanging: systemd's Restart=on-failure/RestartSec=5
	// then drives the retry loop. 10s is comfortably above a healthy LAN
	// connect and well under systemd's default 90s start timeout. It bounds
	// reaching the DB only — the schema statements run under postgres.Open's
	// own longer bound (index builds can legitimately take minutes) and pgxpool
	// does not retain this context for later queries.
	const dbConnectTimeout = 10 * time.Second
	ctx, cancel := context.WithTimeout(context.Background(), dbConnectTimeout)
	db, err := postgres.Open(ctx, cfg.DatabaseURL)
	cancel()
	if err != nil {
		return fmt.Errorf("open postgres: %w", err)
	}
	defer func() { _ = db.Close() }()

	srv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           syncapi.NewHandler(db, cfg.Token),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	fmt.Fprintf(os.Stderr, "yas-server %s: sync API on %s\n", version, cfg.Addr)
	// Graceful shutdown (signal handling) lands with the systemd units in M6;
	// ErrServerClosed is already tolerated for that future.
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
