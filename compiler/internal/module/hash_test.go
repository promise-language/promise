package module

import (
	"os"
	"path/filepath"
	"testing"
)

func TestHashModuleTree(t *testing.T) {
	dir := t.TempDir()
	// Create a small module tree.
	if err := os.WriteFile(filepath.Join(dir, "promise.toml"), []byte("[module]\nname = \"m\"\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "src"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "src", "main.pr"), []byte("main() {}"), 0644); err != nil {
		t.Fatal(err)
	}

	hash, err := HashModuleTree(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(hash) != 64 {
		t.Errorf("hash length = %d, want 64", len(hash))
	}

	// Same content → same hash (deterministic).
	hash2, err := HashModuleTree(dir)
	if err != nil {
		t.Fatal(err)
	}
	if hash != hash2 {
		t.Errorf("non-deterministic: %q != %q", hash, hash2)
	}
}

func TestHashModuleTreeDifferentContent(t *testing.T) {
	dir1 := t.TempDir()
	dir2 := t.TempDir()

	if err := os.WriteFile(filepath.Join(dir1, "a.pr"), []byte("hello"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir2, "a.pr"), []byte("world"), 0644); err != nil {
		t.Fatal(err)
	}

	h1, err := HashModuleTree(dir1)
	if err != nil {
		t.Fatal(err)
	}
	h2, err := HashModuleTree(dir2)
	if err != nil {
		t.Fatal(err)
	}
	if h1 == h2 {
		t.Error("different content should produce different hashes")
	}
}

func TestHashModuleTreeSkipsGitAndDSStore(t *testing.T) {
	dir1 := t.TempDir()
	dir2 := t.TempDir()

	// Both have the same source file.
	for _, d := range []string{dir1, dir2} {
		if err := os.WriteFile(filepath.Join(d, "main.pr"), []byte("main() {}"), 0644); err != nil {
			t.Fatal(err)
		}
	}
	// dir2 also has .git/ and .DS_Store — should not affect hash.
	gitDir := filepath.Join(dir2, ".git")
	if err := os.MkdirAll(gitDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(gitDir, "HEAD"), []byte("ref: refs/heads/main"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir2, ".DS_Store"), []byte("junk"), 0644); err != nil {
		t.Fatal(err)
	}

	h1, err := HashModuleTree(dir1)
	if err != nil {
		t.Fatal(err)
	}
	h2, err := HashModuleTree(dir2)
	if err != nil {
		t.Fatal(err)
	}
	if h1 != h2 {
		t.Errorf("hashes differ despite same source content: %q vs %q", h1, h2)
	}
}

func TestHashModuleTreeDifferentPaths(t *testing.T) {
	// Same content but different file names should produce different hashes.
	dir1 := t.TempDir()
	dir2 := t.TempDir()

	if err := os.WriteFile(filepath.Join(dir1, "a.pr"), []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir2, "b.pr"), []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}

	h1, err := HashModuleTree(dir1)
	if err != nil {
		t.Fatal(err)
	}
	h2, err := HashModuleTree(dir2)
	if err != nil {
		t.Fatal(err)
	}
	if h1 == h2 {
		t.Error("different file paths with same content should produce different hashes")
	}
}

func TestHashModuleTreeEmpty(t *testing.T) {
	dir := t.TempDir()
	hash, err := HashModuleTree(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(hash) != 64 {
		t.Errorf("hash length = %d, want 64", len(hash))
	}
}
