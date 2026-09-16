//go:build !darwin

package blobstore

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// TestCloneFileIsUnsupportedOffDarwin: clonefile(2) is APFS-only, and the
// caller's contract is "any error means fall back to a streamed copy". So the
// stub has to REPORT unsupported rather than quietly return nil — a nil here
// would leave materializeViewFile believing it had produced a file and hand the
// driver a destination that does not exist.
func TestCloneFileIsUnsupportedOffDarwin(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	src := filepath.Join(dir, "blob")
	if err := os.WriteFile(src, []byte("bytes"), 0o755); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(dir, "opt")
	err := CloneFile(src, dst)
	if err == nil {
		t.Fatal("CloneFile returned nil off darwin; the caller would skip its fallback")
	}
	if !errors.Is(err, errors.ErrUnsupported) {
		t.Errorf("CloneFile error = %v, want errors.ErrUnsupported", err)
	}
	if _, err := os.Stat(dst); !os.IsNotExist(err) {
		t.Errorf("the unsupported clone created %q anyway: %v", dst, err)
	}
}

// TestPatchAndSignMachOIsANoOpOffDarwin: LLVM tools elsewhere are statically
// linked or use plain ELF rpath, so there is nothing to patch — and the no-op
// must leave the file exactly as materialization left it.
func TestPatchAndSignMachOIsANoOpOffDarwin(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	tool := filepath.Join(dir, "opt")
	if err := os.WriteFile(tool, []byte("elf bytes"), 0o755); err != nil {
		t.Fatal(err)
	}
	PatchAndSignMachO(tool)
	got, err := os.ReadFile(tool)
	if err != nil || string(got) != "elf bytes" {
		t.Errorf("the no-op changed the tool: %q, %v", got, err)
	}
}
