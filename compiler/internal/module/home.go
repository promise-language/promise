package module

import (
	"fmt"
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

// CacheRoot returns the root of the caches the compiler derives from its own
// inputs — the build and AST caches, the extracted embedded modules, CRT,
// OpenSSL, compiler-rt, winlink, WASM harness and macOS SDK stub, and the
// compiler stamp. PROMISE_CACHE relocates exactly these; unset or empty, they
// stay at <PromiseHome>/cache. This is the only reader of PROMISE_CACHE.
//
// What the compiler acquires rather than computes — the content-addressed
// store (blobs/, archives/), the views materialized from it, and fetched
// modules — stays under <PromiseHome>/cache regardless (docs/module-system.md
// §"Cache Layout").
//
// A relative value is refused: each process in a tree resolves it against its
// own working directory, so one run would scatter across several caches.
func CacheRoot() (string, error) {
	if dir := os.Getenv("PROMISE_CACHE"); dir != "" {
		if !filepath.IsAbs(dir) {
			return "", fmt.Errorf("PROMISE_CACHE must be an absolute path, got %q", dir)
		}
		return dir, nil
	}
	home, err := PromiseHome()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "cache"), nil
}
