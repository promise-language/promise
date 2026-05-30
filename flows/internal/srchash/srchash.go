// Package srchash gives the flow binaries the same staleness self-check the other
// bin/ tools have (tools/build/common.CheckStale): each flow binary bakes in a
// hash of the flow source at build time (injected by ./make via ldflags) and
// recomputes it at startup, refusing to run with "run ./make" when the source has
// changed since the binary was built.
//
// The flow binaries are a SEPARATE Go module from the build tools (they depend on
// the flow SDK and OSS flow submodules), so they cannot import tools/build/common.
// This package is the flows-side counterpart: the single source of truth for the
// flow source hash, used both by each flow binary at runtime (CheckStale) and by
// ./make at build time (the internal/buildhash helper prints Hash) — so the
// build-time and runtime hashes are computed by identical code and can never drift.
package srchash

import (
	"fmt"
	"hash/fnv"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
)

// flowSourceDirs are the directories (relative to the repo root) whose contents
// determine a flow binary's behavior: the flows module itself, the tracker-backend
// flow SDK (flow-sdk/), and the OSS flow substrate (flow/) — both wired in via
// flows/go.mod's local replaces. A change in any of them must mark a built flow
// binary stale.
var flowSourceDirs = []string{"flows", "flow-sdk", "flow"}

// Hash computes an FNV-128a hash of every .go / go.mod / go.sum file under the
// flow source directories, relative to root. A missing source dir contributes
// nothing (so deleting flow-sdk/ changes the hash → stale), and .git directories
// are skipped (flow-sdk/ and flow/ are git submodules). The result is stable for a
// given tree and changes whenever any covered file is added, removed, or edited.
func Hash(root string) (string, error) {
	var rels []string
	for _, dir := range flowSourceDirs {
		base := filepath.Join(root, dir)
		if _, err := os.Stat(base); err != nil {
			continue // absent dir: contributes nothing (its files "vanish" from the hash)
		}
		err := filepath.WalkDir(base, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				if d.Name() == ".git" {
					return fs.SkipDir
				}
				return nil
			}
			if isSourceFile(d.Name()) {
				rel, relErr := filepath.Rel(root, path)
				if relErr != nil {
					return relErr
				}
				rels = append(rels, filepath.ToSlash(rel))
			}
			return nil
		})
		if err != nil {
			return "", fmt.Errorf("walk %s: %w", dir, err)
		}
	}
	sort.Strings(rels)

	h := fnv.New128a()
	for _, rel := range rels {
		data, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			return "", fmt.Errorf("read %s: %w", rel, err)
		}
		fmt.Fprintf(h, "%s\n%d\n", rel, len(data))
		h.Write(data)
	}
	return fmt.Sprintf("%x", h.Sum(nil)), nil
}

// isSourceFile reports whether a filename contributes to the flow source hash.
func isSourceFile(name string) bool {
	return filepath.Ext(name) == ".go" || name == "go.mod" || name == "go.sum"
}

// CheckStale compares the compiled-in source hash against the current flow source
// hash and exits with a "run ./make" message if they differ — the flows-side
// mirror of tools/build/common.CheckStale. Call it first thing in a flow binary's
// main(). A "dev" compiled hash (the default when built without ldflags, e.g. via
// `go run` or a dlv debug session) skips the check.
func CheckStale(compiledHash string) {
	if compiledHash == "dev" {
		return
	}
	root, err := FindRoot()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	current, err := Hash(root)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	if compiledHash != current {
		make := filepath.Join(root, "make")
		fmt.Fprintf(os.Stderr, "flow source has changed — run: ./make (or %s)\n", make)
		os.Exit(1)
	}
}

// FindRoot locates the repository root (the directory containing catalog.toml).
// It walks up from the current directory first (a flow always runs inside the
// worktree), then falls back to the executable's own location — bin/flow/<name>
// is three levels below the root.
func FindRoot() (string, error) {
	if dir, err := os.Getwd(); err == nil {
		if root, ok := findCatalogRoot(dir); ok {
			return root, nil
		}
	}
	if exe, err := os.Executable(); err == nil {
		if resolved, err := filepath.EvalSymlinks(exe); err == nil {
			candidate := filepath.Dir(filepath.Dir(filepath.Dir(resolved))) // bin/flow/<name> → root
			if root, ok := findCatalogRoot(candidate); ok {
				return root, nil
			}
		}
	}
	return "", fmt.Errorf("could not find catalog.toml from cwd or executable path")
}

// findCatalogRoot walks up from dir looking for catalog.toml, returning the
// directory that contains it.
func findCatalogRoot(dir string) (string, bool) {
	for {
		if _, err := os.Stat(filepath.Join(dir, "catalog.toml")); err == nil {
			return dir, true
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", false
		}
		dir = parent
	}
}
