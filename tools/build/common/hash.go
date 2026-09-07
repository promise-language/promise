package common

import (
	"crypto/sha256"
	"fmt"
	"hash/fnv"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
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

// gateHashExclusions are repo-relative path prefixes left out of the worktree
// hash. Both are excluded for a reason that would otherwise break the gate,
// not for tidiness:
//
//	.promise-home/ holds the gate-values sidecar the hash is recorded in, so
//	including it would make the hash cover the file that stores it.
//
//	tools/gates/baselines.json is the gate's own output: a *passing* commit
//	gate rewrites it, so including it would make a successful run invalidate
//	itself. It is also not an input to any gate value — verify never reads it.
var gateHashExclusions = []string{
	".promise-home/",
	"tools/gates/baselines.json",
}

// WorktreeHash returns a content identity for the source tree at root: a
// SHA-256 over every file git considers part of the project — tracked, plus
// untracked and not ignored — each contributed as path, length and bytes.
//
// It answers "is this the same tree?" exactly, where a timestamp only guesses.
//
// Two scoping decisions, both deliberate:
//
// Untracked-but-not-ignored files are *in*. The tools this identity vouches for
// read the filesystem, not the index: bin/verify compiles whatever .pr and .go
// files are on disk, so a brand-new file nobody has staged yet is part of what
// was tested. Hashing only tracked files would let "write a new file after
// verify, then commit it" through — the same false pass a clock allowed.
// Genuine scratch belongs in .gitignore or outside the repo, which is where the
// exclusion is expressed once for every tool rather than here.
//
// The index is *out*. The hash is a function of worktree content alone, so
// git add and git commit leave it unchanged and no verify is wasted on staging.
// The consequence is explicit: the gate vouches for the worktree, so a partial
// git add commits a subset of the tree that was verified — the same scope every
// other worktree-reading pre-commit check (formatting, docs) already assumes.
func WorktreeHash(root string) (string, error) {
	// RunBytesIn, not RunOutputIn: -z emits raw NUL-separated paths, and
	// TrimSpace would eat a leading space in the first path.
	out, err := RunBytesIn(root, "git", "ls-files", "-z", "--cached", "--others", "--exclude-standard")
	if err != nil {
		return "", fmt.Errorf("list worktree files: %w", err)
	}

	// An unmerged path is listed once per merge stage, so dedupe by path.
	seen := make(map[string]bool)
	var files []string
	for _, raw := range strings.Split(string(out), "\x00") {
		if raw == "" {
			continue
		}
		rel := filepath.ToSlash(raw)
		if gateHashExcluded(rel) || seen[rel] {
			continue
		}
		seen[rel] = true
		files = append(files, rel)
	}
	sort.Strings(files)

	h := sha256.New()
	for _, rel := range files {
		data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
		if err != nil {
			// Listed but not readable as a file: a tracked path deleted from
			// the worktree or left out by a sparse checkout, a submodule
			// gitlink, or an untracked nested repository (git lists the
			// directory rather than recursing). None of these should crash the
			// gate — record the path with a length no real file can have, so
			// the state still contributes to the identity and flipping to or
			// from it changes the hash.
			fmt.Fprintf(h, "%s\n-1\n", rel)
			continue
		}
		fmt.Fprintf(h, "%s\n%d\n", rel, len(data))
		h.Write(data)
	}
	return fmt.Sprintf("%x", h.Sum(nil)), nil
}

// gateHashExcluded reports whether a slash-separated repo-relative path is
// covered by gateHashExclusions (as a directory prefix or an exact path).
func gateHashExcluded(rel string) bool {
	for _, ex := range gateHashExclusions {
		if strings.HasSuffix(ex, "/") {
			if strings.HasPrefix(rel, ex) {
				return true
			}
		} else if rel == ex {
			return true
		}
	}
	return false
}
