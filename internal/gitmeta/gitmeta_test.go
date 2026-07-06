package gitmeta

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeFile is a tiny helper: create parent dirs and write a file.
func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestDetect_RepoRootAndBranchFromSubdir(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, ".git", "HEAD"), "ref: refs/heads/main\n")
	sub := filepath.Join(root, "internal", "deep", "pkg")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}

	gotRoot, gotBranch := Detect(sub)
	// t.TempDir may sit under a symlinked path (e.g. /tmp -> /private/tmp on
	// macOS); compare resolved paths so the walk's Clean vs the test's literal
	// don't diverge.
	if !sameDir(t, gotRoot, root) {
		t.Errorf("root = %q, want %q", gotRoot, root)
	}
	if gotBranch != "main" {
		t.Errorf("branch = %q, want %q", gotBranch, "main")
	}
}

func TestDetect_BranchWithSlashes(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, ".git", "HEAD"), "ref: refs/heads/feature/xvt6-repo-root\n")
	_, branch := Detect(root)
	if branch != "feature/xvt6-repo-root" {
		t.Errorf("branch = %q, want %q", branch, "feature/xvt6-repo-root")
	}
}

func TestDetect_DetachedHeadHasRootButEmptyBranch(t *testing.T) {
	root := t.TempDir()
	// A detached HEAD stores a raw sha, not "ref: refs/heads/...".
	writeFile(t, filepath.Join(root, ".git", "HEAD"), "9fceb02c1234567890abcdef1234567890abcdef\n")
	gotRoot, branch := Detect(root)
	if !sameDir(t, gotRoot, root) {
		t.Errorf("root = %q, want %q", gotRoot, root)
	}
	if branch != "" {
		t.Errorf("branch = %q, want empty on detached HEAD", branch)
	}
}

func TestDetect_NotARepoYieldsEmpty(t *testing.T) {
	dir := t.TempDir() // no .git of our own
	// t.TempDir lives under the shared temp root; if some ancestor happens to
	// carry a .git (git would treat dir as inside that repo, and so must we),
	// this negative case can't hold — skip rather than fail spuriously.
	if r, _ := Detect(dir); r != "" {
		t.Skipf("temp dir sits inside an existing repo (%s); environment not hermetic", r)
	}
	root, branch := Detect(dir)
	if root != "" || branch != "" {
		t.Errorf("Detect(non-repo) = (%q, %q), want empty", root, branch)
	}
}

func TestDetect_EmptyDirYieldsEmpty(t *testing.T) {
	if root, branch := Detect(""); root != "" || branch != "" {
		t.Errorf(`Detect("") = (%q, %q), want empty`, root, branch)
	}
}

func TestDetect_GitFileWorktree(t *testing.T) {
	// A linked worktree (or submodule) has a .git FILE pointing at the real
	// gitdir via "gitdir: <path>". The worktree's own HEAD lives in that gitdir.
	realRepo := t.TempDir()
	wtGitDir := filepath.Join(realRepo, ".git", "worktrees", "wt")
	writeFile(t, filepath.Join(wtGitDir, "HEAD"), "ref: refs/heads/wt-branch\n")

	wt := t.TempDir()
	writeFile(t, filepath.Join(wt, ".git"), "gitdir: "+wtGitDir+"\n")

	gotRoot, branch := Detect(wt)
	if !sameDir(t, gotRoot, wt) {
		t.Errorf("root = %q, want worktree dir %q", gotRoot, wt)
	}
	if branch != "wt-branch" {
		t.Errorf("branch = %q, want %q", branch, "wt-branch")
	}
}

// A corrupt/hostile giant HEAD must not yield a branch over the read cap: the
// record path is sacred, so a derived branch that would fail record validation
// (over MaxFieldBytes) can never be produced. We bound the read to maxGitFileBytes.
func TestDetect_GiantHeadIsBounded(t *testing.T) {
	root := t.TempDir()
	huge := "ref: refs/heads/" + strings.Repeat("z", 1<<20) + "\n"
	writeFile(t, filepath.Join(root, ".git", "HEAD"), huge)
	_, branch := Detect(root)
	if len(branch) >= maxGitFileBytes {
		t.Errorf("branch len = %d, want < maxGitFileBytes (%d)", len(branch), maxGitFileBytes)
	}
}

// A hostile/corrupt ".git" pointer file must not stall or exhaust the record
// path: the read is bounded and the walk still resolves the worktree root (the
// dir holding the .git file), just with no branch when the gitdir is unusable.
func TestDetect_GiantGitFileIsBounded(t *testing.T) {
	wt := t.TempDir()
	huge := "gitdir: " + strings.Repeat("z", 1<<20) + "\n"
	writeFile(t, filepath.Join(wt, ".git"), huge)
	root, branch := Detect(wt)
	if !sameDir(t, root, wt) {
		t.Errorf("root = %q, want worktree dir %q", root, wt)
	}
	if branch != "" {
		t.Errorf("branch = %q, want empty (bogus gitdir has no readable HEAD)", branch)
	}
}

func TestDetect_NearestRepoWins(t *testing.T) {
	outer := t.TempDir()
	writeFile(t, filepath.Join(outer, ".git", "HEAD"), "ref: refs/heads/outer\n")
	inner := filepath.Join(outer, "vendor", "nested")
	writeFile(t, filepath.Join(inner, ".git", "HEAD"), "ref: refs/heads/inner\n")

	gotRoot, branch := Detect(inner)
	if !sameDir(t, gotRoot, inner) {
		t.Errorf("root = %q, want nearest %q", gotRoot, inner)
	}
	if branch != "inner" {
		t.Errorf("branch = %q, want %q", branch, "inner")
	}
}

// sameDir compares two directory paths by their resolved (symlink-free) form.
func sameDir(t *testing.T, a, b string) bool {
	t.Helper()
	ra, err := filepath.EvalSymlinks(a)
	if err != nil {
		return a == b
	}
	rb, err := filepath.EvalSymlinks(b)
	if err != nil {
		return a == b
	}
	return ra == rb
}
