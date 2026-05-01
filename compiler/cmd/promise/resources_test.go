package main

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestResourcesFresh detects stale embedded resources. If this test fails,
// run `make resources` from the compiler/ directory to refresh copies.
func TestResourcesFresh(t *testing.T) {
	// Find the repo root: this file is at compiler/cmd/promise/resources_test.go,
	// so the repo root is three levels up.
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Skip("cannot determine source file path")
	}
	repoRoot := filepath.Join(filepath.Dir(thisFile), "..", "..", "..")

	// Verify the repo root looks right (has std/ directory).
	if _, err := os.Stat(filepath.Join(repoRoot, "std")); err != nil {
		t.Skipf("repo root not available at %s (running from installed binary?)", repoRoot)
	}

	// Parse the embedded checksum file: each line is "hash  path"
	scanner := bufio.NewScanner(bytes.NewReader(embeddedSourcesChecksum))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, "  ", 2)
		if len(parts) != 2 {
			t.Fatalf("malformed checksum line: %s", line)
		}
		expectedHash, relPath := parts[0], parts[1]

		absPath := filepath.Join(repoRoot, relPath)
		data, err := os.ReadFile(absPath)
		if err != nil {
			t.Errorf("cannot read source file %s: %v", relPath, err)
			continue
		}
		actualHash := fmt.Sprintf("%x", sha256.Sum256(data))
		if actualHash != expectedHash {
			t.Errorf("stale resource: %s has changed — run `make resources`", relPath)
		}
	}
}
