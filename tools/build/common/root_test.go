package common

import (
	"os"
	"path/filepath"
	"testing"
)

func TestFindCatalogRoot_Found(t *testing.T) {
	// Create a temp directory tree with catalog.toml at the root.
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "catalog.toml"), []byte(""), 0o644); err != nil {
		t.Fatal(err)
	}
	sub := filepath.Join(root, "a", "b", "c")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}

	// Walking up from a deeply nested subdirectory should find the root.
	got, ok := findCatalogRoot(sub)
	if !ok {
		t.Fatal("findCatalogRoot returned false, expected true")
	}
	if got != root {
		t.Errorf("findCatalogRoot = %q, want %q", got, root)
	}
}

func TestFindCatalogRoot_ExactDir(t *testing.T) {
	// catalog.toml is in the starting directory itself.
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "catalog.toml"), []byte(""), 0o644); err != nil {
		t.Fatal(err)
	}

	got, ok := findCatalogRoot(root)
	if !ok {
		t.Fatal("findCatalogRoot returned false, expected true")
	}
	if got != root {
		t.Errorf("findCatalogRoot = %q, want %q", got, root)
	}
}

func TestFindCatalogRoot_NotFound(t *testing.T) {
	// Clear TMPDIR so t.TempDir() uses /tmp rather than .promise-home/tmp.
	// When bin/verify sets TMPDIR to the repo-internal .promise-home/tmp,
	// t.TempDir() lands inside the repo and findCatalogRoot would walk up
	// and find the real catalog.toml.
	t.Setenv("TMPDIR", "")
	dir := t.TempDir()

	_, ok := findCatalogRoot(dir)
	if ok {
		t.Fatal("findCatalogRoot returned true, expected false")
	}
}
