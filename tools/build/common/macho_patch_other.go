//go:build !darwin

package common

// patchAndSignMachO is a no-op on non-darwin hosts: only fetched macOS LLVM
// blobs require the install_name_tool + codesign dance (§5.1).
func patchAndSignMachO(string) {}
