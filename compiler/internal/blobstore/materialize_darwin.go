//go:build darwin

package blobstore

import (
	"os/exec"
	"path/filepath"
	"strings"
)

// PatchAndSignMachO patches an extracted LLVM Mach-O file so it can find
// libLLVM in its own directory, then re-signs it ad-hoc (§5.1). Lifted verbatim
// from the old extractCompressedLLVM macOS block (main.go). Homebrew tools set
// @rpath to @loader_path/../lib and may hardcode absolute Homebrew dylib paths;
// we:
//  1. add @loader_path as an rpath so tools find dylibs in their own directory,
//  2. rewrite absolute Homebrew dylib references to @rpath/<name>,
//  3. set dylib install names to @rpath/<name> (so other binaries find them),
//  4. re-sign ad-hoc (install_name_tool invalidates code signatures).
//
// Best-effort, matching the original: each command's failure is harmless when
// the binary is already patched/signed.
//
// NOTE (T0769, §10 open contract): this runs on the per-target VIEW-DIR working
// copy, not on the CAS blob. The CAS stores the raw upstream-extracted bytes, so
// its content hash is deterministic and computable at release time without
// running install_name_tool/codesign — which a separate Go module (the build
// tooling) could not share. The patched, signed, loadable copy lives in the
// view dir, which DYLD_LIBRARY_PATH points at.
func PatchAndSignMachO(path string) {
	name := filepath.Base(path)
	if strings.HasSuffix(name, ".dylib") {
		exec.Command("install_name_tool", "-id", "@rpath/"+name, path).CombinedOutput()
		exec.Command("install_name_tool", "-add_rpath", "@loader_path", path).CombinedOutput()
	} else {
		exec.Command("install_name_tool", "-add_rpath", "@loader_path", path).CombinedOutput()
	}

	if out, err := exec.Command("otool", "-L", path).Output(); err == nil {
		for _, line := range strings.Split(string(out), "\n") {
			line = strings.TrimSpace(line)
			if (strings.HasPrefix(line, "/opt/homebrew/") || strings.HasPrefix(line, "/usr/local/opt/")) && strings.Contains(line, ".dylib") {
				if idx := strings.Index(line, " (compatibility"); idx > 0 {
					oldPath := line[:idx]
					newName := "@rpath/" + filepath.Base(oldPath)
					exec.Command("install_name_tool", "-change", oldPath, newName, path).CombinedOutput()
				}
			}
		}
	}

	exec.Command("codesign", "--force", "--sign", "-", path).CombinedOutput()
}
