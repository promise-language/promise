package module

import (
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// HashModuleTree computes a deterministic SHA-256 hash over a module directory.
// Files are walked in sorted order; each file contributes its relative path
// (forward slashes) and contents to the running hash. Directories named .git
// and files named .DS_Store are skipped.
func HashModuleTree(dir string) (string, error) {
	h := sha256.New()
	err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		name := info.Name()
		if info.IsDir() {
			if name == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		if name == ".DS_Store" {
			return nil
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		// Normalize to forward slashes for cross-platform determinism.
		rel = strings.ReplaceAll(rel, string(filepath.Separator), "/")

		// Write relative path as a length-prefixed record to prevent collisions.
		fmt.Fprintf(h, "%d:%s\n", len(rel), rel)

		f, err := os.Open(path)
		if err != nil {
			return err
		}
		defer f.Close()
		if _, err := io.Copy(h, f); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		return "", fmt.Errorf("hashing module tree %s: %w", dir, err)
	}
	return fmt.Sprintf("%x", h.Sum(nil)), nil
}
