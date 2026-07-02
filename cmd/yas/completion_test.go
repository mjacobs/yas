package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestDoCompletion_ZshOffersEveryCommand(t *testing.T) {
	var buf bytes.Buffer
	if err := doCompletion(&buf, "zsh"); err != nil {
		t.Fatalf("doCompletion(zsh): %v", err)
	}
	out := buf.String()
	if !strings.HasPrefix(out, "#compdef yas\n") {
		t.Errorf("script must start with %q, got %.40q", "#compdef yas", out)
	}
	// Every word route() dispatches on must be offered as a completion, so
	// this list mirrors route's switch — including completion itself.
	for _, sub := range []string{
		"record", "search", "history", "serve", "sync", "import",
		"session", "mcp", "completion", "version", "help",
	} {
		if !strings.Contains(out, "'"+sub+":") {
			t.Errorf("script does not offer subcommand %q", sub)
		}
	}
	// Spot-check flag and value sources survive in the script.
	for _, want := range []string{"--from", "zsh-history", "atuin", "--json", "--failed"} {
		if !strings.Contains(out, want) {
			t.Errorf("script missing %q", want)
		}
	}
}

func TestDoCompletion_UnsupportedShellErrorsAndWritesNothing(t *testing.T) {
	var buf bytes.Buffer
	err := doCompletion(&buf, "bash")
	if err == nil {
		t.Fatal("want an error for an unsupported shell")
	}
	if !strings.Contains(err.Error(), "zsh") {
		t.Errorf("error should name the supported shell, got %q", err)
	}
	if buf.Len() != 0 {
		t.Errorf("nothing must reach the writer on error, got %d bytes", buf.Len())
	}
}

// The generated script must be syntactically valid zsh: `zsh -n` parses
// without executing. Skipped where zsh is not installed.
func TestDoCompletion_ZshScriptParses(t *testing.T) {
	zsh, err := exec.LookPath("zsh")
	if err != nil {
		t.Skip("zsh not installed")
	}
	var buf bytes.Buffer
	if err := doCompletion(&buf, "zsh"); err != nil {
		t.Fatalf("doCompletion(zsh): %v", err)
	}
	path := filepath.Join(t.TempDir(), "_yas")
	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(zsh, "-fn", path).CombinedOutput(); err != nil {
		t.Fatalf("zsh -n rejected the script: %v\n%s", err, out)
	}
}
