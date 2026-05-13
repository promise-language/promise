package module

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
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

// InstalledEpochs returns a sorted list of epoch names found under
// <PromiseHome>/epochs/. Each entry is a directory name (e.g., "2026.0", "dev").
func InstalledEpochs() ([]string, error) {
	home, err := PromiseHome()
	if err != nil {
		return nil, err
	}
	epochsDir := filepath.Join(home, "epochs")
	entries, err := os.ReadDir(epochsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
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
