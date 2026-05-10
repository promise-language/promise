package common

import (
	"fmt"
	"os"
	"path/filepath"
)

// RunStress builds the compiler and runs stress tests.
// Args are passed through to "promise test -stress" (e.g., iteration count).
func RunStress(root string, args []string) error {
	promiseBin := filepath.Join(root, "bin", BinaryName())

	// Use local cache (same as verify --local)
	promiseHome := filepath.Join(root, ".promise-home")
	tmpDir := filepath.Join(promiseHome, "tmp")
	if err := os.MkdirAll(tmpDir, 0o755); err != nil {
		return err
	}
	os.Setenv("PROMISE_HOME", promiseHome)
	os.Setenv("TMPDIR", tmpDir)

	// Build first
	fmt.Println("Building compiler...")
	if err := RunBuild(root, nil); err != nil {
		return fmt.Errorf("build: %w", err)
	}

	// Build promise test args: -timeout 15s -stress [args...] tests/... modules/...
	testArgs := []string{"test", "-timeout", "15s", "-stress"}
	testArgs = append(testArgs, args...)
	testArgs = append(testArgs, "tests/...", "modules/...")

	fmt.Println("Running stress tests (Ctrl+C to stop)...")
	return Run(promiseBin, testArgs...)
}
