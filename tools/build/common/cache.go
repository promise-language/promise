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
//
// The temp directory is redirected under every name a platform reads it by:
// TMPDIR on Unix, TMP and TEMP on Windows (Go's os.TempDir there, and every
// child that asks the OS). Setting all three everywhere is harmless and means no
// platform's temp files land in the machine-global temp directory.
func SetupLocalCache(root string) error {
	promiseHome := filepath.Join(root, ".promise-home")
	tmpDir := filepath.Join(promiseHome, "tmp")
	if err := os.MkdirAll(tmpDir, 0o755); err != nil {
		return err
	}
	os.Setenv("PROMISE_HOME", promiseHome)
	os.Unsetenv("PROMISE_CACHE")
	for _, name := range tempDirVars {
		os.Setenv(name, tmpDir)
	}
	return nil
}

// tempDirVars are the variables a process reads its temp directory from, on
// any platform.
var tempDirVars = []string{"TMPDIR", "TMP", "TEMP"}
