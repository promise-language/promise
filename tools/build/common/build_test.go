package common

import (
	"strings"
	"testing"
)

// TestRunBuild_RejectsFetchFlag confirms the legacy -fetch flag is no longer
// accepted. Pre-Phase-3 it selected the upstream-tarball bundling path; after
// Phase 3 the cache-first flow is the only path, so -fetch is gone. Anyone
// scripting the old flag should get a clear "usage" error rather than silently
// running the wrong build.
func TestRunBuild_RejectsFetchFlag(t *testing.T) {
	for _, flag := range []string{"-fetch", "--fetch", "-fetch=llvm"} {
		t.Run(flag, func(t *testing.T) {
			err := RunBuild("/nonexistent", []string{flag})
			if err == nil {
				t.Fatalf("expected RunBuild(%q) to fail with usage error, got nil", flag)
			}
			if !strings.Contains(err.Error(), "usage:") {
				t.Errorf("expected usage error from RunBuild(%q), got: %v", flag, err)
			}
			if strings.Contains(err.Error(), "-fetch") {
				t.Errorf("usage error still mentions -fetch (should be removed): %v", err)
			}
		})
	}
}
