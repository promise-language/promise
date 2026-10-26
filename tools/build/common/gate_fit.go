package common

// The `fit` gate: is this machine fit to be given work at all?
//
// It is the one gate whose subject is not the code, and the project provides it
// because only the project knows what its work requires. It reports free space,
// and on Unix whether the worktree's filesystem lets a removed file's name go
// (gate_fit_unlink_unix.go), and stops — what is acceptable is held in
// thresholds.json and applied by the judge (judge.go).

import (
	"fmt"
	"path/filepath"
	"slices"
	"strings"
)

// unlinkMetricName is the one fit metric only some platforms measure
// (gate_fit_unlink_unix.go). Named here, on every platform, because the terms
// file is one file for every platform and must be able to name it everywhere.
const unlinkMetricName = "worktree_hides_unlinked_files"

// fitMetricSpecs is what fit declares on this platform: the worktree's free
// space, what the unlink probe reports where it runs, and the build cache's.
func fitMetricSpecs() []MetricSpec {
	return slices.Concat(bytesSpecs("worktree_free_bytes"), unlinkMetricSpecs, bytesSpecs("cache_free_bytes"))
}

// measureFit reports free space on the two filesystems this project's work
// writes to: the worktree (the change, and .promise-home/ with its build and
// test caches) and the Go build cache. Both are reported every run even when
// they are one device — an envelope whose shape varied by host is one no
// threshold can be written against. Beside the worktree's free space it reports
// what unlinkMetrics measures of that filesystem — before the build cache, so a
// run left incomplete by an unknown cache location still carries everything it
// knows about the worktree.
func measureFit(root string) (measurement, error) {
	return measureFitWith(root, captureSplit)
}

// measureFitWith is measureFit with the `go env` child as a parameter, so a
// test can stand in for a toolchain that cannot say where its cache is. Each
// path measured is a group carrying its own figure.
func measureFitWith(root string, capture captureFunc) (measurement, error) {
	free, err := freeBytesNear(root)
	if err != nil {
		return measurement{}, fmt.Errorf("free space at %s: %w", root, err)
	}
	worktree := Size("worktree_free_bytes", free, unitBytes)
	m := measurement{
		metrics: []Metric{worktree},
		groups:  []Group{{Name: root, Metrics: []Metric{worktree}}},
	}
	unlink, err := unlinkMetrics(root)
	if err != nil {
		return measurement{}, fmt.Errorf("unlink semantics at %s: %w", root, err)
	}
	m.metrics = append(m.metrics, unlink...)
	m.groups[0].Metrics = append(m.groups[0].Metrics, unlink...)

	cache, why := goBuildCache(root, capture)
	if why != "" {
		// Named rather than omitted: one filesystem of two is a run that
		// measured less than a full one, and an incomplete run is never a pass.
		m.groups = append(m.groups, Group{Name: "GOCACHE", Members: []Member{newMember("go env GOCACHE", OutcomeNotRun, why)}})
		m.incomplete = "the Go build cache location is unknown, so only the worktree filesystem was measured: " + why
		return m, nil
	}
	free, err = freeBytesNear(cache)
	if err != nil {
		return measurement{}, fmt.Errorf("free space at %s: %w", cache, err)
	}
	cacheFree := Size("cache_free_bytes", free, unitBytes)
	m.metrics = append(m.metrics, cacheFree)
	m.groups = append(m.groups, Group{Name: cache, Metrics: []Metric{cacheFree}})
	return m, nil
}

// goBuildCache asks the toolchain where it writes what it reuses. `go env
// GOCACHE` is the only authoritative answer; recomputing it here would be wrong
// on every machine that configured it. The value is read from stdout alone —
// go writes diagnostics and toolchain-download notices to stderr, and a path
// read from the combined stream has the warning glued to its front.
func goBuildCache(root string, capture captureFunc) (path, why string) {
	stdout, stderr, err := capture(root, "go", "env", "GOCACHE")
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
