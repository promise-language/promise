package common

// The `fit` gate: is this machine fit to be given work at all?
//
// It is the one gate whose subject is not the code, and the project provides it
// because only the project knows what its work requires. It reports free space
// and stops — how much is enough is held in thresholds.json and applied by the
// judge (judge.go).

import (
	"fmt"
	"path/filepath"
	"strings"
)

// measureFit reports free space on the two filesystems this project's work
// writes to: the worktree (the change, and .promise-home/ with its build and
// test caches) and the Go build cache. Both are reported every run even when
// they are one device — an envelope whose shape varied by host is one no
// threshold can be written against.
func measureFit(root string) ([]Metric, string, error) {
	free, err := freeBytesNear(root)
	if err != nil {
		return nil, "", fmt.Errorf("free space at %s: %w", root, err)
	}
	metrics := []Metric{Size("worktree_free_bytes", free, "bytes")}

	cache, why := goBuildCache(root)
	if why != "" {
		// Named rather than omitted: one filesystem of two is a run that
		// measured less than a full one, and an incomplete run is never a pass.
		return metrics, "the Go build cache location is unknown, so only the worktree filesystem was measured: " + why, nil
	}
	free, err = freeBytesNear(cache)
	if err != nil {
		return nil, "", fmt.Errorf("free space at %s: %w", cache, err)
	}
	return append(metrics, Size("build_cache_free_bytes", free, "bytes")), "", nil
}

// goBuildCache asks the toolchain where it writes what it reuses. `go env
// GOCACHE` is the only authoritative answer; recomputing it here would be wrong
// on every machine that configured it. The value is read from stdout alone —
// go writes diagnostics and toolchain-download notices to stderr, and a path
// read from the combined stream has the warning glued to its front.
func goBuildCache(root string) (path, why string) {
	stdout, stderr, err := captureSplit(root, "go", "env", "GOCACHE")
	cache := strings.TrimSpace(stdout)
	if err != nil || cache == "" {
		if line := firstLine(strings.TrimSpace(stderr)); line != "" {
			return "", line
		}
		if err != nil {
			return "", err.Error()
		}
		return "", "`go env GOCACHE` printed nothing"
	}
	// A relative answer is not a location: freeBytesNear would walk up to the
	// process's own directory and report a filesystem nobody asked about.
	if !filepath.IsAbs(cache) {
		return "", fmt.Sprintf("`go env GOCACHE` answered %q, which is not an absolute path", cache)
	}
	return cache, ""
}

// freeBytesNear reports free space for path, or for its nearest existing
// ancestor. A build cache that has never been written has no directory yet,
// and `fit` runs on exactly that machine — the fresh one, before work is given.
func freeBytesNear(path string) (int64, error) {
	for p := filepath.Clean(path); ; {
		if Exists(p) {
			return freeBytes(p)
		}
		parent := filepath.Dir(p)
		if parent == p {
			return 0, fmt.Errorf("nothing on the path %s exists", path)
		}
		p = parent
	}
}
