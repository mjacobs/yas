//go:build unix

package gitmeta

import "syscall"

// openNonblock makes readBounded's open non-blocking on unix, so opening a fifo
// or device .git/HEAD returns immediately instead of waiting for a writer and
// hanging the record path. It is a no-op on regular files.
const openNonblock = syscall.O_NONBLOCK
