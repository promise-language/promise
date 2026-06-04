package main

import (
	"testing"

	"github.com/promise-language/promise/compiler/internal/blobstore"
)

// TestEmbeddedManifestParses verifies the always-embedded runtime manifest
// (resources/manifest.json) is valid and parseable (T0769). Debug builds carry
// the empty-entries placeholder; release builds carry real LLVM entries.
func TestEmbeddedManifestParses(t *testing.T) {
	m, err := loadEmbeddedManifest()
	if err != nil {
		t.Fatalf("embedded manifest must parse: %v", err)
	}
	if m == nil {
		t.Fatal("embedded manifest is nil")
	}
	if m.Schema != blobstore.ManifestSchema {
		t.Fatalf("manifest schema = %d, want %d", m.Schema, blobstore.ManifestSchema)
	}
	// Every entry (if any) must be a well-formed LLVM/CRT blob.
	for _, e := range m.Entries {
		if e.SHA256 == "" || len(e.Sources) == 0 {
			t.Fatalf("entry %q is malformed", e.Name)
		}
	}
}

// TestResolveLLVMViewEmptyManifestFallsThrough verifies the LLVM view resolver
// reports "no view" (so findLLVMTool falls through to local toolchain probes)
// when the manifest carries no LLVM entries — the debug-build default.
func TestResolveLLVMViewEmptyManifestFallsThrough(t *testing.T) {
	m, err := loadEmbeddedManifest()
	if err != nil {
		t.Fatal(err)
	}
	hasLLVM := false
	for _, e := range m.Entries {
		if len(e.Name) >= len(llvmEntryPrefix) && e.Name[:len(llvmEntryPrefix)] == llvmEntryPrefix {
			hasLLVM = true
		}
	}
	if hasLLVM {
		t.Skip("manifest carries LLVM entries (release build) — view resolution is exercised elsewhere")
	}
	view, err := resolveLLVMView(false)
	if err != nil {
		t.Fatalf("empty-manifest view resolution should not error: %v", err)
	}
	if view != "" {
		t.Fatalf("expected no view for empty manifest, got %q", view)
	}
}
