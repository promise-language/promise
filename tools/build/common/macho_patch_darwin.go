//go:build darwin

package common

import (
	"os/exec"
	"path/filepath"
	"strings"
)

// patchAndSignMachO patches a fetched LLVM Mach-O so it loads from its own
// directory, then re-signs it ad-hoc. Mirrors the runtime resolver's
// compiler/internal/blobstore/materialize_darwin.go::PatchAndSignMachO (§5.1):
// the CAS / hosted blob stores the raw upstream bytes (so the content hash is
// deterministic and verifiable without running install_name_tool/codesign),
// and the build-tool slim cache is the equivalent of the runtime view dir —
// the loadable copy that DYLD_LIBRARY_PATH points at. The blob hash stays
// computable in a separate Go module; the patch step runs only on the local
// loadable copy.
//
// Best-effort, matching the runtime helper: each command's failure is harmless
// when the binary is already patched/signed (install_name_tool and codesign
// are tolerant of re-runs).
func patchAndSignMachO(path string) {
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
