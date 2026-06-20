package common

import (
	"fmt"
	"hash/fnv"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
)

// ToolsSourceHash computes an FNV-1a hash of all .go and go.mod files under
// the tools/build/ directory. This is used for staleness detection — if the
// hash at compile time differs from the hash at runtime, the binary is stale.
func ToolsSourceHash(root string) (string, error) {
	toolsDir := filepath.Join(root, "tools", "build")
	var files []string
	err := filepath.WalkDir(toolsDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		ext := filepath.Ext(path)
		name := filepath.Base(path)
		if ext == ".go" || name == "go.mod" || name == "go.sum" {
			rel, _ := filepath.Rel(toolsDir, path)
			files = append(files, rel)
		}
		return nil
	})
	if err != nil {
		return "", fmt.Errorf("walk tools/build: %w", err)
	}
	sort.Strings(files)

	h := fnv.New128a()
	for _, rel := range files {
		abs := filepath.Join(toolsDir, rel)
		data, err := os.ReadFile(abs)
		if err != nil {
			return "", fmt.Errorf("read %s: %w", rel, err)
		}
		fmt.Fprintf(h, "%s\n%d\n", rel, len(data))
		h.Write(data)
	}
	return fmt.Sprintf("%x", h.Sum(nil)), nil
}
