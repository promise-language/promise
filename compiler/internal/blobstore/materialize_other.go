//go:build !darwin

package blobstore

import "errors"

// PatchAndSignMachO is a no-op on non-macOS platforms — LLVM tools there are
// either statically linked or use plain ELF rpath, needing no patching.
func PatchAndSignMachO(path string) {}

// CloneFile has no portable equivalent outside APFS. Callers treat any error as
// "fall back to a streamed copy", so the unsupported answer is the error.
func CloneFile(src, dst string) error { return errors.ErrUnsupported }

// EnsureAdHocSignature is a no-op on non-macOS platforms — only macOS (arm64 in
// particular) requires an executable to carry a valid signature before the
// kernel will run it.
func EnsureAdHocSignature(path string) {}
