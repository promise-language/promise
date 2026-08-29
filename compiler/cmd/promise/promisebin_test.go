package main

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// locatePromiseBin is the locator for the compiler binary used by the few tests
// in package main that drive it as a subprocess. Most such tests live in
// cmd/promise/tests/... and use clitest.Bin instead; the ones here stayed
// because they also assert on unexported runner internals, which nothing
// outside package main can reach.
//
// It returns an absolute path so a test that runs the binary from a temp
// working directory still finds it, and skips when no binary is available.
func locatePromiseBin(t *testing.T) string {
	t.Helper()
	if bin := os.Getenv("PROMISE_TEST_BIN"); bin != "" {
		return bin
	}
	// go test cwd = compiler/cmd/promise → repo root is three levels up.
	name := "promise"
	if runtime.GOOS == "windows" {
		name = "promise.exe"
	}
	candidate := filepath.Join("..", "..", "..", "bin", name)
	if _, err := os.Stat(candidate); err == nil {
		if abs, err := filepath.Abs(candidate); err == nil {
			return abs
		}
		return candidate
	}
	t.Skip("set PROMISE_TEST_BIN or build via bin/build to run this end-to-end test")
	return ""
}
