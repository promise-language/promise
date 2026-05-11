package common

import (
	"os"
	"path/filepath"
)

// SetupLocalCache configures the process to use a repo-local Promise cache
// (.promise-home/) instead of the shared ~/.promise cache.
func SetupLocalCache(root string) error {
	promiseHome := filepath.Join(root, ".promise-home")
	tmpDir := filepath.Join(promiseHome, "tmp")
	if err := os.MkdirAll(tmpDir, 0o755); err != nil {
		return err
	}
	os.Setenv("PROMISE_HOME", promiseHome)
	os.Setenv("TMPDIR", tmpDir)
	return nil
}
