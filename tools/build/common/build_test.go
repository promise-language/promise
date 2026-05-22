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

// TestRunBuild_RejectsUnknownFlags ensures arbitrary unknown flags surface the
// usage error too — not just the historical -fetch we wrote a dedicated test
// for. Catches accidental flag introductions / typos.
func TestRunBuild_RejectsUnknownFlags(t *testing.T) {
	for _, flag := range []string{"-bogus", "--also-bogus", "-debug=on"} {
		t.Run(flag, func(t *testing.T) {
			err := RunBuild("/nonexistent", []string{flag})
			if err == nil {
				t.Fatalf("expected RunBuild(%q) to fail with usage error, got nil", flag)
			}
			if !strings.Contains(err.Error(), "usage:") {
				t.Errorf("expected usage error from RunBuild(%q), got: %v", flag, err)
			}
		})
	}
}

// TestBundleLLVM_NoEntry covers the manifest sanity-check branches in
// BundleLLVM that fire when the manifest is malformed for the running target.
// These are reachable from `bin/build --release` via FetchAll producing a
// cache dir for a target whose llvm entry got mangled in the manifest.
func TestBundleLLVM_NoEntry(t *testing.T) {
	t.Run("missing binaries.llvm", func(t *testing.T) {
		err := BundleLLVM(t.TempDir(), &PrebuiltsManifest{}, t.TempDir())
		if err == nil || !strings.Contains(err.Error(), "missing [binaries.llvm]") {
			t.Errorf("expected missing-llvm-entry error, got: %v", err)
		}
	})

	t.Run("missing target entry", func(t *testing.T) {
		// Empty Targets map → tEntry == nil for the running target.
		m := &PrebuiltsManifest{
			Binaries: map[string]*PrebuiltEntry{
				"llvm": {Version: "1", BundleDir: "out", Targets: map[string]*TargetEntry{}},
			},
		}
		err := BundleLLVM(t.TempDir(), m, t.TempDir())
		if err == nil || !strings.Contains(err.Error(), "no [binaries.llvm.targets") {
			t.Errorf("expected missing-target-entry error, got: %v", err)
		}
	})

	t.Run("unsupported target surfaces reason", func(t *testing.T) {
		target := CurrentBuildTarget()
		m := &PrebuiltsManifest{
			Binaries: map[string]*PrebuiltEntry{
				"llvm": {
					Version:   "1",
					BundleDir: "out",
					Targets: map[string]*TargetEntry{
						target: {Unsupported: "test reason: target not supported"},
					},
				},
			},
		}
		err := BundleLLVM(t.TempDir(), m, t.TempDir())
		if err == nil {
			t.Fatal("expected unsupported error, got nil")
		}
		// The unsupported reason must be surfaced verbatim so users know why.
		if !strings.Contains(err.Error(), "test reason: target not supported") {
			t.Errorf("unsupported reason not surfaced; got: %v", err)
		}
	})
}
