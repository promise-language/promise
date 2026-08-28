package elfstub

import (
	"bytes"
	"debug/elf"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

func sampleSyms() []Sym {
	return []Sym{
		{Name: "xmlAddChild", Version: "LIBXML2_2.4.30", Type: elf.STT_FUNC},
		{Name: "xmlFree", Version: "LIBXML2_2.4.30", Type: elf.STT_OBJECT},
		{Name: "xmlReadMemory", Version: "LIBXML2_2.6.0", Type: elf.STT_FUNC},
		{Name: "xmlNoVersion", Type: elf.STT_FUNC},
	}
}

// TestBuildIsAWellFormedDSO checks the generated image against the stdlib's own
// ELF reader: if debug/elf can recover the soname, the symbols, and the version
// definitions, the parts the dynamic loader reads are in the right places.
func TestBuildIsAWellFormedDSO(t *testing.T) {
	data, err := Build(elf.EM_X86_64, "libxml2.so.2", sampleSyms())
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	f, err := elf.NewFile(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("generated image does not parse: %v", err)
	}
	defer f.Close()

	if f.Type != elf.ET_DYN {
		t.Errorf("type = %v, want ET_DYN", f.Type)
	}
	if f.Machine != elf.EM_X86_64 {
		t.Errorf("machine = %v, want EM_X86_64", f.Machine)
	}
	soname, err := f.DynString(elf.DT_SONAME)
	if err != nil || len(soname) != 1 || soname[0] != "libxml2.so.2" {
		t.Errorf("DT_SONAME = %v (err %v), want [libxml2.so.2]", soname, err)
	}

	syms, err := f.DynamicSymbols()
	if err != nil {
		t.Fatalf("DynamicSymbols: %v", err)
	}
	got := map[string]elf.Symbol{}
	for _, s := range syms {
		got[s.Name] = s
	}
	for _, want := range sampleSyms() {
		s, ok := got[want.Name]
		if !ok {
			t.Errorf("%s: not defined by the stub", want.Name)
			continue
		}
		if s.Section == elf.SHN_UNDEF {
			t.Errorf("%s: defined as undefined — the loader would not resolve it", want.Name)
		}
		if s.Version != want.Version {
			t.Errorf("%s: version = %q, want %q", want.Name, s.Version, want.Version)
		}
		if typ := elf.ST_TYPE(s.Info); typ != want.Type {
			t.Errorf("%s: type = %v, want %v", want.Name, typ, want.Type)
		}
	}

	// The version definitions are the part a stub is usually missing: glibc
	// rejects an unversioned library for a versioned reference outright.
	versions, err := f.DynamicVersions()
	if err != nil {
		t.Fatalf("DynamicVersions: %v", err)
	}
	defined := map[string]bool{}
	for _, v := range versions {
		defined[v.Name] = true
	}
	for _, want := range []string{"libxml2.so.2", "LIBXML2_2.4.30", "LIBXML2_2.6.0"} {
		if !defined[want] {
			t.Errorf("verdef missing %q (have %v)", want, defined)
		}
	}
}

// TestBuildIsDeterministic keeps a re-materialized view dir from churning.
func TestBuildIsDeterministic(t *testing.T) {
	a, err := Build(elf.EM_AARCH64, "libxml2.so.2", sampleSyms())
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	b, err := Build(elf.EM_AARCH64, "libxml2.so.2", sampleSyms())
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if !bytes.Equal(a, b) {
		t.Error("two builds of the same input differ")
	}
}

func TestBuildRejectsEmptyInput(t *testing.T) {
	if _, err := Build(elf.EM_X86_64, "", sampleSyms()); err == nil {
		t.Error("empty soname accepted")
	}
	if _, err := Build(elf.EM_X86_64, "libxml2.so.2", nil); err == nil {
		t.Error("empty symbol list accepted")
	}
}

func TestPlanIgnoresNonELF(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "notelf")
	if err := os.WriteFile(path, []byte("#!/bin/sh\necho hi\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	stubs, err := Plan(path)
	if err != nil {
		t.Fatalf("Plan on a non-ELF file: %v", err)
	}
	if len(stubs) != 0 {
		t.Errorf("planned %d stubs for a shell script", len(stubs))
	}
}

// TestPlanDerivesLLDSurface pins the actual reason this package exists: the
// pinned lld imports libxml2, and the plan built from the binary covers exactly
// what it imports.
func TestPlanDerivesLLDSurface(t *testing.T) {
	// The premise below — "the pinned lld imports libxml2" — is only true of the
	// Linux lld. On any other host the staged lld is that host's own binary (a
	// Mach-O on macOS), Plan's elf.Open declines it, and the assertion below
	// fails on a file it was never meant to read. Same guard as
	// TestStubLetsLLDRun, which has always had it.
	if runtime.GOOS != "linux" {
		t.Skip("deriving an ELF import surface is a Linux concern")
	}
	lld := findHostLLD(t)
	stubs, err := Plan(lld)
	if err != nil {
		t.Fatalf("Plan(%s): %v", lld, err)
	}
	if len(stubs) != 1 || stubs[0].SOName != "libxml2.so.2" {
		t.Fatalf("stubs = %+v, want one libxml2.so.2 stub", stubs)
	}
	if len(stubs[0].Syms) == 0 {
		t.Fatal("libxml2 stub has no symbols")
	}
	for _, s := range stubs[0].Syms {
		if s.Version == "" {
			t.Errorf("%s: no version tag — glibc needs one to match the reference", s.Name)
		}
	}
}

// TestStubLetsLLDRun is the end-to-end proof: the generated library alone
// satisfies lld's libxml2 dependency, so the linker starts on a host that has
// no libxml2 at all. LD_LIBRARY_PATH takes precedence over the system search
// path, so the stub is what gets loaded even on a machine that has the real one.
func TestStubLetsLLDRun(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("ELF stub loading is a Linux concern")
	}
	lld := findHostLLD(t)
	dir := t.TempDir()
	written, err := WriteFor([]string{lld}, dir)
	if err != nil {
		t.Fatalf("WriteFor: %v", err)
	}
	if len(written) == 0 {
		t.Skip("this lld build needs no stubs")
	}

	cmd := exec.Command(lld, "-flavor", "gnu", "--version")
	cmd.Env = append(os.Environ(), "LD_LIBRARY_PATH="+dir)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("lld failed to run against the stub: %v\n%s", err, out)
	}
	if !bytes.Contains(out, []byte("LLD")) {
		t.Errorf("unexpected lld output: %s", out)
	}
}

// findHostLLD locates the lld this checkout has staged, in the toolchain view
// or the prebuilts cache. Tests that need it skip when it is absent, so a
// checkout that has never built stays green.
func findHostLLD(t *testing.T) string {
	t.Helper()
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home dir to search for a staged lld")
	}
	patterns := []string{
		filepath.Join(home, ".promise", "cache", "llvm-view", "*", "lld"),
		filepath.Join(home, ".cache", "promise", "prebuilts", "llvm-slim", "*", "*", "lld"),
	}
	for _, pat := range patterns {
		matches, _ := filepath.Glob(pat)
		for _, m := range matches {
			if fi, err := os.Stat(m); err == nil && !fi.IsDir() {
				return m
			}
		}
	}
	t.Skip("no staged lld on this host (run bin/build first)")
	return ""
}
