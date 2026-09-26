//go:build darwin

package common

import (
	"os/exec"
	"path/filepath"
	"strings"
)

// patchAndSignMachO patches a fetched LLVM Mach-O so it loads from its own
// directory, then re-signs it ad-hoc. Mirrors the runtime resolver's
// compiler/internal/blobstore/materialize_darwin.go::PatchAndSignMachO (language-design.md#primitive-types-are-regular-types):
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

// ensureRuntimeSignature makes a fetched WASM test runtime loadable on macOS
// (T2169). arm64 refuses to execute an unsigned Mach-O, so a runtime whose
// signature did not survive the trip has to be ad-hoc signed before it can run.
//
// Deliberately NOT patchAndSignMachO. wasmtime and node are self-contained
// binaries with no Promise-relative dylib to find, so adding an @loader_path
// rpath would buy nothing and would invalidate a perfectly good upstream
// signature on the way — an unconditional re-sign is a change that can only
// make a working binary worse. Extraction and the brotli round trip are
// byte-preserving, so the normal case is that `codesign --verify` passes and
// this does nothing at all.
//
// Best-effort, like its sibling: a re-sign that fails leaves the binary exactly
// as it was, and the failure surfaces where it matters — at exec time, naming
// the runtime — rather than as a fetch error.
func ensureRuntimeSignature(path string) {
	if err := exec.Command("codesign", "--verify", path).Run(); err == nil {
		return // upstream signature survived — leave it alone
	}
	exec.Command("codesign", "--force", "--sign", "-", path).CombinedOutput()
}
