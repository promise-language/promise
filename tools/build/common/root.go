package common

import (
	"fmt"
	"os"
	"path/filepath"
)

// FindRoot walks up from the current directory looking for catalog.toml,
// which marks the Promise repository root.
func FindRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("getwd: %w", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "catalog.toml")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("could not find catalog.toml in any parent directory")
		}
		dir = parent
	}
}
