package main

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/promise-language/promise/compiler/internal/elfstub"
)

// stubMarkerName records which compatibility stub libraries a view dir carries.
// Its presence is what tells viewComplete that a view was built by a compiler
// that knows about them — a view staged by an older build has the tools but not
// the stubs, and must be rebuilt rather than served from the fast path.
const stubMarkerName = ".stubs.ok"

// needsToolchainStubs reports whether this host's toolchain view is one that
// can need generated stub libraries at all. ELF hosts only: the macOS tools are
// Mach-O and resolve against always-present system dylibs, and the Windows LLVM
// build is statically linked.
func needsToolchainStubs() bool { return runtime.GOOS == "linux" }

// writeToolchainStubs generates the stub shared libraries the materialized LLVM
// tools depend on but never call, alongside those tools in the view dir —
// which runLLVMCmd already puts on LD_LIBRARY_PATH, so nothing else has to
// change for the loader to find them.
//
// This is what keeps the "no system dependencies" bar honest on Linux. The
// upstream LLVM release binaries are not fully static: lld carries a DT_NEEDED
// on libxml2.so.2 for LLVM's COFF manifest merger and is linked BIND_NOW, so on
// a machine without libxml2 — a base Ubuntu install has none — the linker
// cannot start and every build dies with "error while loading shared libraries"
// (T1774). Supplying the library ourselves is the same move Windows makes with
// its self-generated import libs (T0772): we own the link surface rather than
// asking the user to install one.
func writeToolchainStubs(viewDir string) error {
	if !needsToolchainStubs() {
		return nil
	}
	entries, err := os.ReadDir(viewDir)
	if err != nil {
		return err
	}
	var tools []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		tools = append(tools, filepath.Join(viewDir, e.Name()))
	}
	written, err := elfstub.WriteFor(tools, viewDir)
	if err != nil {
		return fmt.Errorf("generating LLVM toolchain compatibility stubs: %w", err)
	}
	// The marker is written even when nothing needed stubbing, so "this view was
	// built by a compiler that checked" and "this view needs no stubs" are the
	// same fast-path answer.
	return os.WriteFile(filepath.Join(viewDir, stubMarkerName), []byte(strings.Join(written, "\n")), 0o644)
}
