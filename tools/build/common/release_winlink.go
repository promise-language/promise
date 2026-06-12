package common

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

// release_winlink.go implements `bin/release winlink` — the generator for the
// Windows zero-dependency link surface (T0772). It produces MSVC-ABI x86_64
// import libraries from the license-clean symbol-list .def files under
// tools/build/winlink/def/ using llvm-dlltool, and writes them to the embedded
// resources dir (compiler/cmd/promise/resources/winlink/windows-amd64/), which
// the compiler binary go:embeds.
//
// Symbol→DLL mappings are not copyrightable, so the generated .lib files are
// freely re-hostable and carry no Microsoft toolchain bytes. Each .def declares
// its own `LIBRARY <name>.dll`, so the import name→DLL binding is single-sourced
// in the .def (no per-file mapping here). The .def is the source of truth; the
// generated .lib is a reproducible build artifact — EmbedResources generates it
// at build time (via ensureWinlinkLibs) straight into the build-cleared embed
// resource dir, regenerating when a .def is newer, and it can be regenerated
// explicitly with `bin/release winlink`. The client running promise needs
// neither llvm-dlltool nor the .def/.lib (the libs are go:embedded).

// winlinkDefDir is the .def source directory, relative to the repo root.
const winlinkDefDir = "tools/build/winlink/def"

// winlinkResDir is the import-lib output directory, relative to root — the
// go:embed tree itself (compiler/cmd/promise/resources/, gitignored and
// repopulated each build). ensureWinlinkLibs generates the .lib straight here
// and clears the dir when stale, so there is no intermediate dir and no copy
// step. The compiler binary go:embeds them.
const winlinkResDir = "compiler/cmd/promise/resources/winlink/windows-amd64"

// runReleaseWinlink regenerates the Windows import libraries from the .def files.
func runReleaseWinlink(root string, args []string) error {
	fs := flag.NewFlagSet("winlink", flag.ContinueOnError)
	dllTool := fs.String("llvm-dlltool", "", "path to llvm-dlltool (default: found on PATH)")
	defDir := fs.String("def-dir", filepath.Join(root, filepath.FromSlash(winlinkDefDir)), "directory of .def symbol lists")
	outDir := fs.String("out", filepath.Join(root, filepath.FromSlash(winlinkResDir)), "output directory for generated import libs")
	if err := fs.Parse(args); err != nil {
		return err
	}

	tool := *dllTool
	if tool == "" {
		tool = resolveWinlinkDllTool(root)
	}
	if tool == "" {
		return fmt.Errorf("llvm-dlltool not found; pass --llvm-dlltool <path> or put it on PATH")
	}

	defs, err := filepath.Glob(filepath.Join(*defDir, "*.def"))
	if err != nil {
		return fmt.Errorf("glob %s: %w", *defDir, err)
	}
	if len(defs) == 0 {
		return fmt.Errorf("no .def files in %s", *defDir)
	}
	sort.Strings(defs)

	if err := os.MkdirAll(*outDir, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", *outDir, err)
	}

	for _, def := range defs {
		base := strings.TrimSuffix(filepath.Base(def), ".def")
		out := filepath.Join(*outDir, base+".lib")
		// x86_64, no name decoration. llvm-dlltool reads the DLL name from the
		// .def's LIBRARY directive, so no -D is needed.
		cmd := exec.Command(tool, "-m", "i386:x86-64", "-d", def, "-l", out)
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("llvm-dlltool %s: %w", base, err)
		}
		info, err := os.Stat(out)
		if err != nil {
			return fmt.Errorf("stat %s: %w", out, err)
		}
		fmt.Printf("  winlink: %s.lib (%d bytes)\n", base, info.Size())
	}
	fmt.Printf("generated %d Windows import libs into %s\n", len(defs), *outDir)
	return nil
}

// resolveWinlinkDllTool locates llvm-dlltool for build-time import-lib
// generation. It prefers an already-populated slim LLVM prebuilt cache, then a
// PATH/system install, and finally fetches the slim blobs (T0833 ships
// llvm-dlltool in the slim set for every host) so a prebuilt-only host with no
// system LLVM still resolves it. The fetch is necessary because winlink
// generation runs during EmbedResources, *before* FindLLVM populates the slim
// cache — so a non-fetching probe would spuriously fail on such hosts.
// Best-effort: returns "" when nothing is found and the fetch fails, so the
// caller surfaces a clear error.
func resolveWinlinkDllTool(root string) string {
	target := CurrentBuildTarget()
	if dir, ok := SlimLLVMCacheDir(root, target); ok {
		if p := filepath.Join(dir, "llvm-dlltool"+ExeSuffix()); Exists(p) {
			return p
		}
	}
	if p := Which("llvm-dlltool"); p != "" {
		return p
	}
	// Nothing cached and not on PATH — fetch the slim blobs (includes the
	// build-only llvm-dlltool) and re-check the cache.
	if root != "" {
		if dir, err := EnsureLLVMBlobs(root, target); err == nil {
			if p := filepath.Join(dir, "llvm-dlltool"+ExeSuffix()); Exists(p) {
				return p
			}
		}
	}
	return ""
}

// ensureWinlinkLibs generates the Windows import libraries from the .def symbol
// lists into the build-cleared embed resource dir, so a fresh checkout (where
// the resources/ tree is gitignored and repopulated each build) still embeds a
// complete link surface. No-op when the .def sources are missing (a trimmed
// checkout) or the generated .lib are already fresh (up to date with every
// .def). When stale — any .def newer than its .lib, or a .def added/removed —
// the output dir is cleared and the libs are regenerated, so editing a .def and
// rebuilding never embeds a stale artifact (T0835). llvm-dlltool is a
// build-time-only dependency — the client running promise needs neither it nor
// the .def/.lib, which are go:embedded into the compiler binary.
func ensureWinlinkLibs(root string) error {
	defDir := filepath.Join(root, filepath.FromSlash(winlinkDefDir))
	if !Exists(defDir) {
		return nil
	}
	outDir := filepath.Join(root, filepath.FromSlash(winlinkResDir))
	if winlinkLibsFresh(defDir, outDir) {
		return nil
	}
	// Stale or absent → clear the embed dir and regenerate. resolveWinlinkDllTool
	// (invoked by runReleaseWinlink) seeds the slim LLVM cache with the build-only
	// llvm-dlltool (T0833/T0840) when it isn't already on PATH or in the cache.
	os.RemoveAll(outDir)
	return runReleaseWinlink(root, nil)
}

// winlinkLibsFresh reports whether the generated .lib in outDir are up to date
// with the .def sources in defDir: every .def must have a matching <base>.lib
// whose mtime is ≥ the .def's, and the counts must match (so a .def added or
// removed — leaving an orphan .lib — is treated as stale). A missing .lib dir or
// an empty one is not fresh, forcing initial generation.
func winlinkLibsFresh(defDir, outDir string) bool {
	defs, err := filepath.Glob(filepath.Join(defDir, "*.def"))
	if err != nil || len(defs) == 0 {
		return false
	}
	libs, err := filepath.Glob(filepath.Join(outDir, "*.lib"))
	if err != nil || len(libs) != len(defs) {
		return false
	}
	for _, def := range defs {
		di, err := os.Stat(def)
		if err != nil {
			return false
		}
		base := strings.TrimSuffix(filepath.Base(def), ".def")
		li, err := os.Stat(filepath.Join(outDir, base+".lib"))
		if err != nil || li.ModTime().Before(di.ModTime()) {
			return false
		}
	}
	return true
}
