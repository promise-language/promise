package common

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ParseEpoch reads the epoch field from catalog.toml.
func ParseEpoch(root string) (string, error) {
	f, err := os.Open(filepath.Join(root, "catalog.toml"))
	if err != nil {
		return "", fmt.Errorf("open catalog.toml: %w", err)
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if strings.HasPrefix(line, "epoch") {
			// epoch = "2026.0"
			parts := strings.SplitN(line, "=", 2)
			if len(parts) == 2 {
				val := strings.TrimSpace(parts[1])
				val = strings.Trim(val, `"`)
				return val, nil
			}
		}
	}
	return "", fmt.Errorf("epoch not found in catalog.toml")
}

// GitSHA returns the short (7-char) git commit hash.
func GitSHA(root string) string {
	out, err := RunOutputIn(root, "git", "rev-parse", "--short=7", "HEAD")
	if err != nil {
		return "unknown"
	}
	return out
}

// GitSHAFull returns the full 40-char git commit hash at HEAD, or "" if
// unavailable (so callers can treat a missing SHA as "no provenance"). Used to
// stamp published compiler binaries with their build commit so the install gate
// can pin test sources to the exact sources the binary was built from (T0854).
func GitSHAFull(root string) string {
	out, err := RunOutputIn(root, "git", "rev-parse", "HEAD")
	if err != nil {
		return ""
	}
	return out
}

// BuildVersion returns the version string for the compiler binary.
// Dev: "<epoch>-<gitsha7>", Release: "<epoch>".
func BuildVersion(root string, release bool) (string, error) {
	epoch, err := ParseEpoch(root)
	if err != nil {
		return "", err
	}
	if release {
		return epoch, nil
	}
	sha := GitSHA(root)
	return epoch + "-" + sha, nil
}

// ReleaseVersion returns the version to stamp into a stable release binary.
// For a numeric epoch tag (epoch-<Y.N>) it takes the version from the TAG NAME,
// so it matches the tag even when the tagged commit's catalog.toml still holds a
// prior epoch (promote-same-hash at a year rollover, T1195). For epoch-next, a
// non-epoch ref (manual workflow_dispatch), or an empty ref it falls back to the
// catalog.toml epoch via BuildVersion(root, true).
func ReleaseVersion(root, tagRef string) (string, error) {
	if v, ok := epochFromTag(tagRef); ok {
		return v, nil
	}
	return BuildVersion(root, true)
}

// epochFromTag extracts "<Y.N>" from a numeric epoch tag ("epoch-2027.0" →
// "2027.0"). Returns ok=false for "epoch-next", non-epoch refs, and empty input.
func epochFromTag(tagRef string) (string, bool) {
	name := strings.TrimSpace(tagRef)
	rest, ok := strings.CutPrefix(name, "epoch-")
	if !ok || rest == "next" {
		return "", false
	}
	e, err := parseEpochStr(rest) // same-package helper in release_cut.go
	if err != nil {
		return "", false
	}
	return e.String(), true // normalizes via epoch.String()
}
