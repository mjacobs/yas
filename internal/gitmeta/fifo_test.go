//go:build linux || darwin || freebsd || netbsd || openbsd || dragonfly

package gitmeta

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// A .git/HEAD that is a FIFO (hostile/corrupt worktree) must not hang the
// record path: a blocking os.Open on a fifo waits for a writer forever. Detect
// must return promptly, treating the non-regular file as absent (empty branch).
// Gated to the platforms that define syscall.Mkfifo (not solaris/illumos), which
// covers the real build/test hosts; the fix it guards (non-blocking open) is a
// unix concern.
func TestDetect_FifoHeadDoesNotHang(t *testing.T) {
	root := t.TempDir()
	gitDir := filepath.Join(root, ".git")
	if err := os.MkdirAll(gitDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(filepath.Join(gitDir, "HEAD"), 0o600); err != nil {
		t.Skipf("mkfifo unsupported here: %v", err)
	}

	done := make(chan string, 1)
	go func() {
		_, branch := Detect(root)
		done <- branch
	}()
	select {
	case branch := <-done:
		if branch != "" {
			t.Errorf("branch = %q, want empty for a fifo HEAD", branch)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Detect hung on a fifo HEAD — the record path must never block")
	}
}
