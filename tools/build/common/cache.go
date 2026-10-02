package common

import (
	"os"
	"path/filepath"
)

// SetupLocalCache configures the process to use a repo-local Promise cache
// (.promise-home/) instead of the shared ~/.promise cache.
//
// An inherited PROMISE_CACHE is cleared, so the whole cache — derived caches
// included — lives in .promise-home/cache and a test that builds a fixture home
// finds its caches where it put them.
func SetupLocalCache(root string) error {
	promiseHome := filepath.Join(root, ".promise-home")
	tmpDir := filepath.Join(promiseHome, "tmp")
	if err := os.MkdirAll(tmpDir, 0o755); err != nil {
		return err
	}
	os.Setenv("PROMISE_HOME", promiseHome)
	os.Unsetenv("PROMISE_CACHE")
	os.Setenv("TMPDIR", tmpDir)
	return nil
}
