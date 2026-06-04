//go:build !darwin

package blobstore

// PatchAndSignMachO is a no-op on non-macOS platforms — LLVM tools there are
// either statically linked or use plain ELF rpath, needing no patching.
func PatchAndSignMachO(path string) {}
