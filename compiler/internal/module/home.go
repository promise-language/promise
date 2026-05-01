package module

import (
	"os"
	"path/filepath"
)

// PromiseHome returns the Promise home directory.
// Uses PROMISE_HOME env var if set, otherwise defaults to ~/.promise/.
func PromiseHome() (string, error) {
	if dir := os.Getenv("PROMISE_HOME"); dir != "" {
		return dir, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".promise"), nil
}
