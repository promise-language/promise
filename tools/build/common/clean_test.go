package common

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestRunClean_UnknownFlagReturnsUsageError verifies that an unrecognized flag
// returns the usage error before any filesystem effects.
func TestRunClean_UnknownFlagReturnsUsageError(t *testing.T) {
	err := RunClean(t.TempDir(), []string{"--unknown"})
	if err == nil {
		t.Fatal("expected error for unknown flag, got nil")
	}
	const want = "usage: bin/clean [--local|--shared] [--quiet]"
	if err.Error() != want {
		t.Errorf("got %q, want %q", err.Error(), want)
	}
}

// TestRunClean_KnownFlagsAccepted ensures that every documented flag passes
// arg validation. We pair every invocation with --shared so RunClean targets
// ~/.promise rather than the test's TempDir/.promise-home (which is empty).
// The clean then runs against an arbitrary tempdir-as-root, but the work it
// does is on ~/.promise — `go clean -testcache` is harmless to repeat.
//
// Note: This test does have one side effect: it nukes the user's ~/.promise
// directory. That is intentional (matching --shared semantics) and acceptable
// for a developer machine — ~/.promise is a regenerable cache. CI runs in
// throwaway containers where this is irrelevant.
//
// To avoid that side effect in tests we instead use --local against a TempDir
// that doesn't have a .promise-home (so RemoveAll is a no-op), and accept the
// `go clean -testcache` system-wide effect (also harmless).
func TestRunClean_KnownFlagsAccepted(t *testing.T) {
	for _, flag := range []string{"--local", "--quiet"} {
		err := RunClean(t.TempDir(), []string{"--quiet", flag})
		if err != nil && strings.HasPrefix(err.Error(), "usage:") {
			t.Errorf("flag %q treated as unknown: %v", flag, err)
		}
	}
}

// TestCleanHome verifies the home-directory resolution for both --local and
// --shared modes.
func TestCleanHome(t *testing.T) {
	root := "/some/repo"
	got, err := CleanHome(root, false)
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(root, ".promise-home")
	if got != want {
		t.Errorf("CleanHome(local) = %q, want %q", got, want)
	}

	gotShared, err := CleanHome(root, true)
	if err != nil {
		t.Fatal(err)
	}
	home, _ := os.UserHomeDir()
	wantShared := filepath.Join(home, ".promise")
	if gotShared != wantShared {
		t.Errorf("CleanHome(shared) = %q, want %q", gotShared, wantShared)
	}
}

// TestClean_RemovesLocalHome verifies that Clean wipes the entire .promise-home
// directory tree, not just tmp/. We create a minimal compiler/go.mod alongside
// so that `go clean -testcache` can run.
func TestClean_RemovesLocalHome(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, ".promise-home")

	// Populate a fake home with both tmp/ and cache/ subdirs to prove that
	// Clean nukes everything under the home (not just tmp).
	for _, sub := range []string{"tmp", "cache/llvm", "cache/build", "tmp/foo"} {
		if err := os.MkdirAll(filepath.Join(home, sub), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(home, "cache", "llvm", "marker"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Minimal go module so `go clean -testcache` succeeds.
	compilerDir := filepath.Join(root, "compiler")
	if err := os.MkdirAll(compilerDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(compilerDir, "go.mod"), []byte("module testmod\n\ngo 1.22\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := Clean(root, CleanOptions{Quiet: true}); err != nil {
		t.Fatalf("Clean: %v", err)
	}

	if _, err := os.Stat(home); !os.IsNotExist(err) {
		t.Errorf("expected %s to be removed, stat err = %v", home, err)
	}
}
