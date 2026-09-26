//go:build !darwin

package common

// patchAndSignMachO is a no-op on non-darwin hosts: only fetched macOS LLVM
// blobs require the install_name_tool + codesign dance (language-design.md#primitive-types-are-regular-types).
func patchAndSignMachO(string) {}

// ensureRuntimeSignature is a no-op on non-darwin hosts: only macOS (arm64 in
// particular) requires an executable to carry a valid signature before the
// kernel will run it.
func ensureRuntimeSignature(string) {}
