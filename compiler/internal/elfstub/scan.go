package elfstub

import (
	"debug/elf"
	"fmt"
	"os"
	"path/filepath"
	"sort"
)

// stubbableLibs is the allowlist: host shared libraries a shipped LLVM tool
// declares but never actually calls, so a stub is a truthful substitute.
//
// It is deliberately tiny and explicit. Everything else an LLVM binary needs —
// libstdc++, libgcc_s, libz, libm, libc — is real code with real behavior; a
// stub for any of those would turn a clear load-time failure into a silent
// miscompile, and they are present on any glibc distribution anyway.
//
//   - libxml2.so.2: lld links it for LLVM's WindowsManifestMerger, which runs
//     only for COFF `/manifestinput:` arguments. Promise never passes one, on
//     any host or target (T1774).
var stubbableLibs = map[string]string{
	"libxml2.so.2": "used only by LLVM's COFF manifest merger, which Promise never invokes",
}

// Stub is one shared object to generate: its soname, the machine it must match,
// and the symbol surface the importing binary resolves against it.
type Stub struct {
	SOName  string
	Machine elf.Machine
	Syms    []Sym
	Reason  string
}

// Plan inspects an ELF binary and returns a stub for each allowlisted library
// it depends on, carrying exactly the symbols that binary imports from it. The
// surface is read out of the binary rather than hardcoded, so bumping the
// pinned LLVM cannot silently desync the stub from what the linker needs — a
// new symbol simply appears in the generated library.
//
// A non-ELF or unreadable file yields no stubs and no error: the caller runs
// over whatever the toolchain view happens to hold, and a file it cannot parse
// is a file that needs nothing from us.
func Plan(binaryPath string) ([]Stub, error) {
	f, err := elf.Open(binaryPath)
	if err != nil {
		return nil, nil
	}
	defer f.Close()

	libs, err := f.ImportedLibraries()
	if err != nil {
		return nil, nil
	}
	wanted := map[string]bool{}
	for _, lib := range libs {
		if _, ok := stubbableLibs[lib]; ok {
			wanted[lib] = true
		}
	}
	if len(wanted) == 0 {
		return nil, nil
	}

	// Symbol types come from the undefined entries themselves, so a data symbol
	// (libxml2's `xmlFree` is a function *pointer*, not a function) is defined
	// as data in the stub too and the dynamic loader sees a consistent picture.
	types := map[string]elf.SymType{}
	if dyn, derr := f.DynamicSymbols(); derr == nil {
		for _, s := range dyn {
			if s.Section == elf.SHN_UNDEF {
				types[s.Name] = elf.ST_TYPE(s.Info)
			}
		}
	}

	imported, err := f.ImportedSymbols()
	if err != nil {
		return nil, fmt.Errorf("elfstub: reading imports of %s: %w", filepath.Base(binaryPath), err)
	}
	byLib := map[string][]Sym{}
	for _, s := range imported {
		if !wanted[s.Library] {
			continue
		}
		byLib[s.Library] = append(byLib[s.Library], Sym{Name: s.Name, Version: s.Version, Type: types[s.Name]})
	}

	var stubs []Stub
	for lib, syms := range byLib {
		sort.Slice(syms, func(i, j int) bool { return syms[i].Name < syms[j].Name })
		stubs = append(stubs, Stub{
			SOName:  lib,
			Machine: f.Machine,
			Syms:    syms,
			Reason:  stubbableLibs[lib],
		})
	}
	sort.Slice(stubs, func(i, j int) bool { return stubs[i].SOName < stubs[j].SOName })
	return stubs, nil
}

// WriteFor generates every stub the given binaries need into dir, and returns
// the file names written (empty when they need none, which is the case on any
// host whose LLVM build is genuinely self-contained).
//
// Writing them unconditionally — rather than only when the host lacks the real
// library — is the point: the toolchain then behaves identically on every
// machine instead of silently depending on what happens to be installed.
func WriteFor(binaryPaths []string, dir string) ([]string, error) {
	stubs, err := planAll(binaryPaths)
	if err != nil || len(stubs) == 0 {
		return nil, err
	}
	var written []string
	for _, s := range stubs {
		data, berr := Build(s.Machine, s.SOName, s.Syms)
		if berr != nil {
			return written, berr
		}
		dst := filepath.Join(dir, s.SOName)
		if werr := os.WriteFile(dst, data, 0o644); werr != nil {
			return written, werr
		}
		written = append(written, s.SOName)
	}
	return written, nil
}

// planAll merges the plans of several binaries, so two tools that import
// overlapping surfaces of the same library yield one stub defining the union.
func planAll(binaryPaths []string) ([]Stub, error) {
	merged := map[string]*Stub{}
	for _, path := range binaryPaths {
		stubs, err := Plan(path)
		if err != nil {
			return nil, err
		}
		for _, s := range stubs {
			cur, ok := merged[s.SOName]
			if !ok {
				dup := s
				merged[s.SOName] = &dup
				continue
			}
			have := map[string]bool{}
			for _, sym := range cur.Syms {
				have[sym.Name] = true
			}
			for _, sym := range s.Syms {
				if !have[sym.Name] {
					cur.Syms = append(cur.Syms, sym)
				}
			}
		}
	}
	var out []Stub
	for _, s := range merged {
		sort.Slice(s.Syms, func(i, j int) bool { return s.Syms[i].Name < s.Syms[j].Name })
		out = append(out, *s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].SOName < out[j].SOName })
	return out, nil
}
