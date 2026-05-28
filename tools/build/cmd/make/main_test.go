package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestHasGoFiles(t *testing.T) {
	dir := t.TempDir()
	if hasGoFiles(dir) {
		t.Error("empty dir should report no Go files")
	}
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if hasGoFiles(dir) {
		t.Error("dir with only non-Go files should report no Go files")
	}
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !hasGoFiles(dir) {
		t.Error("dir with a .go file should report Go files")
	}
}

func TestHasGoFiles_MissingDir(t *testing.T) {
	if hasGoFiles(filepath.Join(t.TempDir(), "does-not-exist")) {
		t.Error("a missing dir should report no Go files (not panic)")
	}
}

func TestWriteFlowRootMarker(t *testing.T) {
	root := t.TempDir()
	writeFlowRootMarker(root)
	marker := filepath.Join(root, ".flow", "root")
	data, err := os.ReadFile(marker)
	if err != nil {
		t.Fatalf("marker not written: %v", err)
	}
	// The SDK's DiscoverWorktreeRoot only checks existence, but the content names
	// the root for human inspection — assert it points at the worktree root.
	if got := string(data); got != root+"\n" {
		t.Errorf("marker content = %q, want %q", got, root+"\n")
	}
}

func TestWriteFlowRootMarker_Idempotent(t *testing.T) {
	root := t.TempDir()
	writeFlowRootMarker(root)
	writeFlowRootMarker(root) // second call must not error or duplicate
	if _, err := os.Stat(filepath.Join(root, ".flow", "root")); err != nil {
		t.Fatalf("marker missing after repeated writes: %v", err)
	}
}
