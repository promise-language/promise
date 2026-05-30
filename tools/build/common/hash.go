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

// FlowsSourceHash computes an FNV-1a hash of the flow source (flows/), the
// tracker-backend flow SDK (flow-sdk/), and the OSS flow substrate (flow/).
// ./make (and make.cmd, which runs the same meta-builder) uses it to skip
// rebuilding the flow binaries when none of those trees have changed. Only .go,
// go.mod, and go.sum files are hashed; a submodule .git directory is skipped (its
// contents churn and are not build inputs). A missing tree is simply omitted from
// the hash.
func FlowsSourceHash(root string) (string, error) {
	h := fnv.New128a()
	// Each tree is hashed under its directory label so a file can't collide
	// across trees on an identical relative path, and so the digest changes if a
	// file moves between flows/, flow-sdk/, and flow/.
	for _, dir := range []string{"flows", "flow-sdk", "flow"} {
		base := filepath.Join(root, dir)
		if !Exists(base) {
			continue
		}
		var files []string
		err := filepath.WalkDir(base, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				if d.Name() == ".git" {
					return filepath.SkipDir
				}
				return nil
			}
			name := d.Name()
			if filepath.Ext(path) == ".go" || name == "go.mod" || name == "go.sum" {
				rel, _ := filepath.Rel(base, path)
				files = append(files, rel)
			}
			return nil
		})
		if err != nil {
			return "", fmt.Errorf("walk %s: %w", dir, err)
		}
		sort.Strings(files)
		for _, rel := range files {
			data, err := os.ReadFile(filepath.Join(base, rel))
			if err != nil {
				return "", fmt.Errorf("read %s/%s: %w", dir, rel, err)
			}
			fmt.Fprintf(h, "%s/%s\n%d\n", dir, rel, len(data))
			h.Write(data)
		}
	}
	return fmt.Sprintf("%x", h.Sum(nil)), nil
}
