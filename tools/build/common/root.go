package common

import (
	"fmt"
	"os"
	"path/filepath"
)

// FindRoot walks up from the current directory looking for catalog.toml,
// which marks the Promise repository root. If the current directory is
// outside the repo, it falls back to deriving the root from the
// executable's own location (all bin/ tools live at <root>/bin/<tool>).
func FindRoot() (string, error) {
	// Try walking up from cwd first.
	if dir, err := os.Getwd(); err == nil {
		if root, ok := findCatalogRoot(dir); ok {
			return root, nil
		}
	}

	// Fallback: derive root from executable path (<root>/bin/<tool>).
	if exe, err := os.Executable(); err == nil {
		if resolved, err := filepath.EvalSymlinks(exe); err == nil {
			candidate := filepath.Dir(filepath.Dir(resolved)) // up 2: bin/<tool> → <root>
			if root, ok := findCatalogRoot(candidate); ok {
				return root, nil
			}
		}
	}

	return "", fmt.Errorf("could not find catalog.toml from cwd or executable path")
}

// findCatalogRoot walks up from dir looking for catalog.toml.
// Returns the directory containing it and true, or ("", false).
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
