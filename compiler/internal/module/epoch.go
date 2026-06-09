package module

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// CompilerEpoch parses the embedded catalog.toml bytes and returns the epoch
// string (e.g., "2026.0"). Returns an error if the catalog cannot be parsed
// or has no epoch field.
func CompilerEpoch(catalogData []byte) (string, error) {
	cat, err := ParseCatalog(catalogData)
	if err != nil {
		return "", fmt.Errorf("parsing catalog: %w", err)
	}
	if cat.Epoch == "" {
		return "", fmt.Errorf("catalog has no epoch field")
	}
	return cat.Epoch, nil
}

// EpochDir returns the directory for a given epoch: <PromiseHome>/epochs/<epoch>/
func EpochDir(epoch string) (string, error) {
	home, err := PromiseHome()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "epochs", epoch), nil
}

// ActiveEpoch reads the active epoch from <PromiseHome>/active.
// If the file does not exist, falls back to the latest installed epoch
// (lexicographically last directory under epochs/).
// Returns an error if no epoch can be determined.
func ActiveEpoch() (string, error) {
	home, err := PromiseHome()
	if err != nil {
		return "", err
	}
	data, err := os.ReadFile(filepath.Join(home, "active"))
	if err == nil {
		epoch := strings.TrimSpace(string(data))
		if epoch != "" {
			return epoch, nil
		}
	}
	// Fallback: latest installed epoch.
	epochs, err := InstalledEpochs()
	if err != nil {
		return "", err
	}
	if len(epochs) == 0 {
		return "", fmt.Errorf("no installed epochs found in %s", filepath.Join(home, "epochs"))
	}
	return epochs[len(epochs)-1], nil
}

// WriteActiveEpoch writes the given epoch string to <PromiseHome>/active.
func WriteActiveEpoch(epoch string) error {
	home, err := PromiseHome()
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(home, "active"), []byte(epoch+"\n"), 0644)
}

// WriteEpochBuildID records the build identity (the verified release asset's
// SHA-256) for an installed epoch at <EpochDir>/build-id. For the rolling
// "next" channel this is the only reliable identity of "the binary I'd
// download" — the commit hash has no trustworthy remote counterpart (T0825).
func WriteEpochBuildID(epoch, sha string) error {
	dir, err := EpochDir(epoch)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "build-id"), []byte(sha+"\n"), 0644)
}

// ReadEpochBuildID reads the recorded build identity for an epoch. Returns
// ("", nil) when no build-id has been recorded — callers treat an unknown local
// build as "update available".
func ReadEpochBuildID(epoch string) (string, error) {
	dir, err := EpochDir(epoch)
	if err != nil {
		return "", err
	}
	data, err := os.ReadFile(filepath.Join(dir, "build-id"))
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", err
	}
	return strings.TrimSpace(string(data)), nil
}

// CompareEpochs orders two "YYYY.N" epoch strings numerically, returning -1, 0,
// or +1 (like strings.Compare). Splitting on "." and comparing each half as an
// integer fixes the lexicographic bug where "2026.10" sorted below "2026.9".
// Non-numeric epochs (e.g. "next", "dev") fall back to a string compare so the
// comparison never panics.
func CompareEpochs(a, b string) int {
	ay, an, aok := splitEpoch(a)
	by, bn, bok := splitEpoch(b)
	if !aok || !bok {
		return strings.Compare(a, b)
	}
	switch {
	case ay != by:
		return cmpInt(ay, by)
	case an != bn:
		return cmpInt(an, bn)
	default:
		return 0
	}
}

// splitEpoch parses a "YYYY.N" epoch into its year and minor components. ok is
// false when the string is not two dot-separated integers.
func splitEpoch(s string) (year, minor int, ok bool) {
	parts := strings.SplitN(s, ".", 2)
	if len(parts) != 2 {
		return 0, 0, false
	}
	y, err1 := strconv.Atoi(parts[0])
	m, err2 := strconv.Atoi(parts[1])
	if err1 != nil || err2 != nil {
		return 0, 0, false
	}
	return y, m, true
}

func cmpInt(a, b int) int {
	if a < b {
		return -1
	}
	return 1
}

// InstalledEpochs returns a sorted list of epoch names found under
// <PromiseHome>/epochs/. Each entry is a directory name (e.g., "2026.0", "dev").
func InstalledEpochs() ([]string, error) {
	home, err := PromiseHome()
	if err != nil {
		return nil, err
	}
	epochsDir := filepath.Join(home, "epochs")
	// Stat first so a non-directory at this path produces a consistent error
	// across platforms. On Windows, os.ReadDir(regular_file) returns
	// ([]DirEntry{}, nil) due to a quirk in Go's dir_windows.go that swallows
	// ERROR_FILE_NOT_FOUND from GetFileInformationByHandleEx.
	info, err := os.Stat(epochsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("%s is not a directory", epochsDir)
	}
	entries, err := os.ReadDir(epochsDir)
	if err != nil {
		return nil, err
	}
	var epochs []string
	for _, e := range entries {
		if e.IsDir() {
			epochs = append(epochs, e.Name())
		}
	}
	sort.Strings(epochs)
	return epochs, nil
}
