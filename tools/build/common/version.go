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
