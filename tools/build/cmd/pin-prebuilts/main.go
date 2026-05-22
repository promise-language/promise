// pin-prebuilts populates the `sha256` fields in tools/build/prebuilts.toml.
//
// For each target with an empty sha256, it downloads the URL via FetchPrebuilt,
// which streams the bytes through SHA-256, caches the verified archive at the
// host-stable prebuilts cache, extracts the manifest's `out` files into the
// same cache dir, and writes the resulting hex digest back into the toml.
//
// Usage:
//
//	bin/pin-prebuilts                 # fill empty sha256 entries (with caching)
//	bin/pin-prebuilts -refresh        # recompute all sha256 entries
//	bin/pin-prebuilts -only llvm      # restrict to a single binary entry
//
// Targets marked `unsupported = "..."` in the manifest are skipped silently.
//
// This tool does not modify URLs or file lists — those remain hand-edited in
// the manifest. Pinning the SHA is the contract that says "this exact byte
// stream is what we expect" and gets verified on every fetch by FetchPrebuilt.
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/p5e-ia/promise-lang/tools/build/common"
)

var sourceHash = "dev"

func main() {
	common.CheckStale(sourceHash)

	refresh := flag.Bool("refresh", false, "recompute every sha256, not just empty ones")
	only := flag.String("only", "", "restrict to one binary entry (e.g., -only llvm)")
	flag.Parse()

	root, err := common.FindRoot()
	if err != nil {
		die(err)
	}

	manifest, err := common.LoadPrebuiltsManifest(root)
	if err != nil {
		die(err)
	}

	cacheRoot, err := common.PrebuiltsCacheRoot()
	if err != nil {
		die(err)
	}

	type job struct {
		binary  string
		version string
		target  string
		url     string
	}
	var jobs []job
	for name, entry := range manifest.Binaries {
		if *only != "" && name != *only {
			continue
		}
		for tname, t := range entry.Targets {
			if t.Unsupported != "" {
				continue // placeholder entry, no upstream artifact to hash
			}
			if t.SHA256 != "" && !*refresh {
				continue
			}
			jobs = append(jobs, job{name, entry.Version, tname, t.URL})
		}
	}
	if len(jobs) == 0 {
		fmt.Println("Nothing to pin — all sha256 entries already populated. Use -refresh to recompute.")
		return
	}

	sort.Slice(jobs, func(i, j int) bool {
		if jobs[i].binary != jobs[j].binary {
			return jobs[i].binary < jobs[j].binary
		}
		return jobs[i].target < jobs[j].target
	})

	type result struct {
		binary string
		target string
		hash   string
	}
	var results []result
	var failures []string
	for _, j := range jobs {
		fmt.Printf("Pinning %s/%s\n  %s\n", j.binary, j.target, j.url)

		// On -refresh: drop the existing archive.ok / tools.ok so FetchPrebuilt
		// re-downloads even if the cached archive matches the (now-stale)
		// manifest sha256.
		cacheDir := filepath.Join(cacheRoot, j.binary, j.version, j.target)
		if *refresh {
			_ = os.Remove(filepath.Join(cacheDir, "archive.ok"))
			_ = os.Remove(filepath.Join(cacheDir, "tools.ok"))
		}

		// FetchPrebuilt with empty manifest sha256 downloads + computes hash +
		// extracts files. Hash lands in <cacheDir>/archive.ok which we read back.
		if _, err := common.FetchPrebuilt(manifest, j.binary, j.target); err != nil {
			fmt.Fprintf(os.Stderr, "  WARNING: %s/%s: %v (skipped)\n", j.binary, j.target, err)
			failures = append(failures, fmt.Sprintf("%s/%s: %v", j.binary, j.target, err))
			continue
		}
		hash, err := readArchiveHash(cacheDir)
		if err != nil {
			fmt.Fprintf(os.Stderr, "  WARNING: %s/%s: read hash: %v (skipped)\n", j.binary, j.target, err)
			failures = append(failures, fmt.Sprintf("%s/%s: read hash: %v", j.binary, j.target, err))
			continue
		}
		fmt.Printf("  sha256 = %s\n", hash)
		fmt.Printf("  cached at %s\n", cacheDir)
		results = append(results, result{j.binary, j.target, hash})
	}
	if len(results) == 0 {
		fmt.Fprintln(os.Stderr, "\nNo sha256 entries updated — every URL failed to fetch.")
		for _, f := range failures {
			fmt.Fprintln(os.Stderr, "  "+f)
		}
		os.Exit(1)
	}

	manifestPath := filepath.Join(root, "tools", "build", "prebuilts.toml")
	data, err := os.ReadFile(manifestPath)
	if err != nil {
		die(err)
	}
	text := string(data)
	for _, r := range results {
		text, err = setSHA256(text, r.binary, r.target, r.hash)
		if err != nil {
			die(fmt.Errorf("update %s/%s: %w", r.binary, r.target, err))
		}
	}
	if err := os.WriteFile(manifestPath, []byte(text), 0o644); err != nil {
		die(err)
	}
	fmt.Printf("\nUpdated %d sha256 entries in %s\n", len(results), manifestPath)
	if len(failures) > 0 {
		fmt.Fprintf(os.Stderr, "\n%d target(s) skipped:\n", len(failures))
		for _, f := range failures {
			fmt.Fprintln(os.Stderr, "  "+f)
		}
		os.Exit(2)
	}
}

// readArchiveHash reads <cacheDir>/archive.ok which FetchPrebuilt writes after
// successfully downloading and verifying (or, for empty-manifest-sha256 mode,
// computing) the archive's sha256.
func readArchiveHash(cacheDir string) (string, error) {
	data, err := os.ReadFile(filepath.Join(cacheDir, "archive.ok"))
	if err != nil {
		return "", err
	}
	hash := strings.TrimSpace(string(data))
	if hash == "" {
		return "", fmt.Errorf("archive.ok is empty")
	}
	return hash, nil
}

// setSHA256 finds the [binaries.<binary>.targets.<target>] section in `text`
// and replaces the first `sha256 = "..."` line within it. The manifest is
// parsed structurally (we already loaded it via LoadPrebuiltsManifest), but
// we rewrite via text replacement to preserve comments and whitespace that a
// toml library round-trip would lose.
func setSHA256(text, binary, target, hash string) (string, error) {
	header := fmt.Sprintf("[binaries.%s.targets.%s]", binary, target)
	headerIdx := strings.Index(text, header)
	if headerIdx < 0 {
		return "", fmt.Errorf("section %s not found", header)
	}
	// Bound the search to this section: from header to the next [section] or EOF.
	rest := text[headerIdx:]
	endIdx := len(rest)
	if next := nextSectionStart(rest); next > 0 {
		endIdx = next
	}
	section := rest[:endIdx]

	re := regexp.MustCompile(`(?m)^sha256\s*=\s*"[^"]*"`)
	loc := re.FindStringIndex(section)
	if loc == nil {
		return "", fmt.Errorf("no sha256 line in section %s", header)
	}
	updatedSection := section[:loc[0]] + fmt.Sprintf(`sha256 = "%s"`, hash) + section[loc[1]:]
	return text[:headerIdx] + updatedSection + rest[endIdx:], nil
}

// nextSectionStart returns the byte offset of the next top-level [section]
// header after the first line of `text`, or -1 if none.
func nextSectionStart(text string) int {
	// Skip the first line (the current section header).
	first := strings.IndexByte(text, '\n')
	if first < 0 {
		return -1
	}
	rest := text[first+1:]
	re := regexp.MustCompile(`(?m)^\[`)
	loc := re.FindStringIndex(rest)
	if loc == nil {
		return -1
	}
	return first + 1 + loc[0]
}

func die(err error) {
	fmt.Fprintf(os.Stderr, "error: %v\n", err)
	os.Exit(1)
}
