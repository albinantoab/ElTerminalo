package ptymanager

import (
	"os"
	"path/filepath"
	"testing"
)

const testHome = "/Users/test/home"

// A permission error must never be mistaken for a missing directory. This is
// the regression that made panes silently reopen in $HOME: ~/Documents is
// TCC-protected, a denial surfaces as EPERM, and the 30s layout autosave then
// wrote the home path back to disk — losing the user's folder permanently.
func TestResolveStartDir_PermissionDeniedKeepsDirectory(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory permissions")
	}

	parent := t.TempDir()
	target := filepath.Join(parent, "project")
	if err := os.Mkdir(target, 0o755); err != nil {
		t.Fatal(err)
	}
	// Drop traverse permission on the parent so Stat(target) fails with EACCES
	// rather than ENOENT — the same shape a TCC denial has.
	if err := os.Chmod(parent, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(parent, 0o755) })

	if _, err := os.Stat(target); err == nil {
		t.Fatal("precondition failed: Stat should have been denied")
	} else if os.IsNotExist(err) {
		t.Fatalf("precondition failed: want a permission error, got %v", err)
	}

	if got := resolveStartDir(target, testHome); got != target {
		t.Errorf("permission error must preserve the directory:\n got %q\nwant %q", got, target)
	}
}

func TestResolveStartDir(t *testing.T) {
	existing := t.TempDir()

	file := filepath.Join(existing, "a-file")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name string
		cwd  string
		want string
	}{
		{"existing directory is used", existing, existing},
		{"empty request falls back to home", "", testHome},
		{"missing directory falls back to home", filepath.Join(existing, "gone"), testHome},
		{"a file is not a working directory", file, testHome},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := resolveStartDir(tt.cwd, testHome); got != tt.want {
				t.Errorf("resolveStartDir(%q) = %q, want %q", tt.cwd, got, tt.want)
			}
		})
	}
}
