// Package gitmeta derives git repository metadata (repo root and branch) from a
// working directory using only filesystem reads — no `git` subprocess, no cgo,
// no network. It is called on the sacred record path, so every operation is a
// bounded stat-walk plus one or two small file reads, and every failure path
// yields empty strings rather than blocking or erroring a capture.
package gitmeta

import (
	"io"
	"os"
	"path/filepath"
	"strings"
)

const headRefPrefix = "ref: refs/heads/"

// maxGitFileBytes bounds how much of a git metadata file (.git/HEAD or a ".git"
// gitdir-pointer file) we read. Both are a single short line in practice;
// capping the read keeps a corrupt or hostile giant file from (a) reading
// unbounded bytes into memory on the sacred record path and (b) yielding a
// branch string over record.MaxFieldBytes, which would make the record fail
// validation and drop the capture. 4 KiB leaves the parsed branch strictly
// under that field cap even in the worst case.
const maxGitFileBytes = 4 << 10

// Detect walks up from dir looking for the nearest git repository and returns
// its root (the directory containing .git) and current branch. Both are
// best-effort: a non-repo, a detached HEAD, or any I/O error yields an empty
// string for the affected value. Detect never fails and never shells out.
//
// It must be derived at capture time only: a repo root reconstructed from a
// historical cwd would be wrong once the checkout moves or changes, which is
// exactly why the field is captured live and left empty on import.
func Detect(dir string) (root, branch string) {
	if dir == "" {
		return "", ""
	}
	dir = filepath.Clean(dir)
	for {
		gitPath := filepath.Join(dir, ".git")
		if fi, err := os.Stat(gitPath); err == nil {
			gitDir := gitPath
			if !fi.IsDir() {
				// .git is a file for linked worktrees/submodules: it points at
				// the real gitdir via "gitdir: <path>".
				gitDir = resolveGitFile(dir, gitPath)
			}
			return dir, readBranch(gitDir)
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", "" // reached the filesystem root without a hit
		}
		dir = parent
	}
}

// resolveGitFile parses a ".git" file ("gitdir: <path>") and returns the gitdir
// it points at, resolving a relative path against base. Returns "" on any error
// so readBranch simply yields no branch. The read is bounded and regular-file
// only (like HEAD): a hostile/corrupt or non-regular ".git" file must not stall
// or exhaust the record path.
func resolveGitFile(base, gitFile string) string {
	b, ok := readBounded(gitFile)
	if !ok {
		return ""
	}
	line := strings.TrimSpace(string(b))
	const p = "gitdir:"
	if !strings.HasPrefix(line, p) {
		return ""
	}
	target := strings.TrimSpace(line[len(p):])
	if target == "" {
		return ""
	}
	if !filepath.IsAbs(target) {
		target = filepath.Join(base, target)
	}
	return filepath.Clean(target)
}

// readBranch reads gitDir/HEAD and returns the branch name from a
// "ref: refs/heads/<name>" line. A detached HEAD (raw sha), a missing file, or
// an empty gitDir yields "".
func readBranch(gitDir string) string {
	if gitDir == "" {
		return ""
	}
	b, ok := readBounded(filepath.Join(gitDir, "HEAD"))
	if !ok {
		return ""
	}
	line := strings.TrimSpace(string(b))
	if rest, ok := strings.CutPrefix(line, headRefPrefix); ok {
		return rest
	}
	return ""
}

// readBounded reads at most maxGitFileBytes from a regular file at path. It rejects
// non-regular files (devices, fifos, dirs) and any I/O error with ok=false, so
// every caller treats a hostile or corrupt git file as absent rather than
// blocking or exhausting memory on the sacred record path.
//
// The open is O_NONBLOCK: a plain O_RDONLY open of a FIFO blocks until a writer
// appears, which would hang `yas record start` before it prints the UUID. It's
// a no-op on regular files, and the post-open f.Stat() (fstat on the fd we
// actually got) authoritatively rejects anything that isn't a regular file —
// with no time-of-check/time-of-use gap, since it inspects the open fd.
func readBounded(path string) ([]byte, bool) {
	f, err := os.OpenFile(path, os.O_RDONLY|openNonblock, 0)
	if err != nil {
		return nil, false
	}
	defer func() { _ = f.Close() }() // read-only; close error is irrelevant
	if fi, err := f.Stat(); err != nil || !fi.Mode().IsRegular() {
		return nil, false
	}
	b, err := io.ReadAll(io.LimitReader(f, maxGitFileBytes))
	if err != nil {
		return nil, false
	}
	return b, true
}
