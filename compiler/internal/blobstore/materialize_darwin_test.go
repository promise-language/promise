//go:build darwin

package blobstore

import (
	"os"
	"path/filepath"
	"testing"
)

// TestPatchAndSignMachOBestEffort verifies the macOS patch+sign step is
// best-effort: on a non-Mach-O file the install_name_tool/otool/codesign
// invocations fail harmlessly without panicking and the file is left in place
// (§5.1). Real Mach-O patching is exercised end-to-end by the build; this guards
// the no-crash contract the resolver relies on.
func TestPatchAndSignMachOBestEffort(t *testing.T) {
	dir := t.TempDir()
	// A .dylib-suffixed file takes the dylib branch (install_name_tool -id).
	dylib := filepath.Join(dir, "libLLVM.dylib")
	if err := os.WriteFile(dylib, []byte("not a real mach-o"), 0o755); err != nil {
		t.Fatal(err)
	}
	PatchAndSignMachO(dylib) // must not panic
	if _, err := os.Stat(dylib); err != nil {
		t.Fatalf("file should survive best-effort patch: %v", err)
	}

	// A plain tool name takes the executable branch (-add_rpath only).
	tool := filepath.Join(dir, "opt")
	if err := os.WriteFile(tool, []byte("not a real mach-o"), 0o755); err != nil {
		t.Fatal(err)
	}
	PatchAndSignMachO(tool) // must not panic
	if _, err := os.Stat(tool); err != nil {
		t.Fatalf("file should survive best-effort patch: %v", err)
	}
}
