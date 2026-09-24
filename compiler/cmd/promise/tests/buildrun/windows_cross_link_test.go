package buildrun

import (
	"context"
	"debug/pe"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/promise-language/promise/compiler/cmd/promise/clitest"
)

// T0531: `promise build -target x86_64-pc-windows-msvc` must produce a runnable
// .exe from a NON-Windows host, against the link surface the compiler carries
// itself.
//
// There is no sysroot to fetch and no MinGW anywhere: T0772's import libs are
// generated from the committed .def symbol lists and embedded unconditionally
// (winlink_embed.go is not host-gated), and the rest of the surface — the crt0
// @__promise_start, the TLS directory, __chkstk, _fltused — is codegen-emitted.
// That is what makes the target listable on every host rather than on Windows
// alone, so the claim needs a test that runs on every host too.
//
// windowsCrossTriple is the only Windows triple that links. aarch64 stays
// emit-ir only until its import libs exist (findWindowsLinkSurface rejects it
// by name).
const windowsCrossTriple = "x86_64-pc-windows-msvc"

// windowsCrossTripleIsNative reports whether windowsCrossTriple is this host's
// OWN target rather than a cross one. On windows-amd64 it is: supportedTargets()
// lists it as the native row instead of appending the cross one, and
// crossExecCommand takes its isHostTarget arm and runs the .exe.
//
// Everything this file asserts about LINKING holds either way — that is what
// makes the triple listable everywhere. Only the two tests about EXECUTION have
// a host in their premise, and this is what lets them say so (T2206).
func windowsCrossTripleIsNative() bool {
	return runtime.GOOS == "windows" && runtime.GOARCH == "amd64"
}

// windowsShippedDLLs is the whole runtime dependency a Promise .exe is allowed
// to have: DLLs present in a stock Windows install, needing no redistributable.
// kernel32/advapi32/ws2_32/secur32/crypt32/ncrypt/bcrypt have shipped since
// forever; ucrtbase.dll is in Windows 10 and later.
//
// This set is the zero-dependency promise of docs/windows-support.md#linking-against-a-self-generated-zero-dependency-surface
// stated as an assertion. An import outside it means a Promise program now
// needs something the user has to install — the regression this test exists to
// catch, and one that no amount of "it linked" can reveal.
var windowsShippedDLLs = map[string]bool{
	"kernel32.dll": true,
	"advapi32.dll": true,
	"ws2_32.dll":   true,
	"ucrtbase.dll": true,
	"secur32.dll":  true,
	"crypt32.dll":  true,
	"ncrypt.dll":   true,
	"bcrypt.dll":   true,
}

// buildWindowsExe cross-builds src to a .exe for windowsCrossTriple and returns
// its path. Shared by every test below so none of them drift into asserting on
// a differently-built binary. extraArgs go before -target, for the flags (like
// -release) that select a different backend pipeline.
func buildWindowsExe(t *testing.T, src string, extraArgs ...string) string {
	t.Helper()
	bin := clitest.Bin(t)

	dir := clitest.TempDir(t)
	srcPath := filepath.Join(dir, "prog.pr")
	if err := os.WriteFile(srcPath, []byte(src), 0644); err != nil {
		t.Fatal(err)
	}
	exePath := filepath.Join(dir, "prog.exe")

	args := append([]string{"build"}, extraArgs...)
	args = append(args, "-target", windowsCrossTriple, "-o", exePath, srcPath)

	res := clitest.Run(t, bin, nil, args...)
	if res.ExitCode != 0 {
		t.Fatalf("build -target %s failed (want exit 0)%s", windowsCrossTriple, res.Detail())
	}
	if _, err := os.Stat(exePath); err != nil {
		t.Fatalf("build reported success but wrote no %s: %v", exePath, err)
	}
	return exePath
}

// helloSource is the fixture for the tests that only need the binary to exist
// and say something. The link-surface test uses a heavier one of its own.
const helloSource = `main() {
  print_line("cross-built and running");
}
`

// importedDLLs reads the PE import directory and returns the DLL names, sorted.
//
// ImportedSymbols, not ImportedLibraries: the latter is a documented no-op in
// debug/pe and returns an empty slice, which would make an allowlist check pass
// against a binary importing anything at all. Each symbol is "name:dll".
func importedDLLs(t *testing.T, exePath string) []string {
	t.Helper()
	f, err := pe.Open(exePath)
	if err != nil {
		t.Fatalf("cross-built %s is not a readable PE file: %v", filepath.Base(exePath), err)
	}
	defer f.Close()

	if got := f.FileHeader.Machine; got != pe.IMAGE_FILE_MACHINE_AMD64 {
		t.Errorf("machine = 0x%x, want IMAGE_FILE_MACHINE_AMD64 (0x%x)",
			got, pe.IMAGE_FILE_MACHINE_AMD64)
	}

	syms, err := f.ImportedSymbols()
	if err != nil {
		t.Fatalf("reading the import table of %s: %v", filepath.Base(exePath), err)
	}
	seen := map[string]bool{}
	for _, s := range syms {
		// "symbol:dll.dll" — take the DLL half.
		if i := strings.LastIndex(s, ":"); i >= 0 {
			seen[strings.ToLower(s[i+1:])] = true
		}
	}
	dlls := make([]string, 0, len(seen))
	for d := range seen {
		dlls = append(dlls, d)
	}
	slices.Sort(dlls)
	return dlls
}

// TestWindowsCrossLinkImportsOnlyShippedDLLs is the always-on half: it needs no
// Windows and no Wine, so it guards the cross-link on every platform that runs
// the suite, including as part of `integration`.
func TestWindowsCrossLinkImportsOnlyShippedDLLs(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("skipping cross-build integration test in short mode")
	}

	// Deliberately more than hello-world: a goroutine and a channel pull in the
	// thread and synchronization surface, and the vector the heap one — which
	// is where an accidental dependency on a non-shipped DLL would appear. A
	// print-only fixture exercises almost none of the PAL.
	exePath := buildWindowsExe(t, `main() {
  numbers := [3, 1, 2];
  total := 0;
  for n in numbers { total = total + n; }
  done := channel[int](capacity: 1);
  go {
    done.send(total);
  };
  if sum := <-done {
    print_line("sum={sum}");
  }
}
`)

	dlls := importedDLLs(t, exePath)

	// An allowlist loop over an empty set passes without asserting anything,
	// which is how a misread import table would report a clean binary forever.
	// Every Promise .exe imports kernel32 — threads, heap, file handles and the
	// crt0's ExitProcess all live there — so requiring it proves the table was
	// actually read.
	if !slices.Contains(dlls, "kernel32.dll") {
		t.Fatalf("no kernel32.dll import found (imports: %v)\n"+
			"  every Promise .exe links against it, so this means the import "+
			"table was not parsed — the allowlist check below would be vacuous", dlls)
	}

	for _, dll := range dlls {
		if !windowsShippedDLLs[dll] {
			t.Errorf("cross-built .exe imports %s, which does not ship with Windows\n"+
				"  a Promise program must run on a stock install with nothing added "+
				"(docs/windows-support.md#linking-against-a-self-generated-zero-dependency-surface)\n"+
				"  if this import is legitimate, add its .def symbol list under "+
				"tools/build/winlink/def/ and extend windowsShippedDLLs", dll)
		}
	}
}

// TestWindowsCrossBuiltExeRunsUnderWine is the execution half. It is separate
// from the link assertion above, and skipped rather than required, because Wine
// is a property of the machine: CI installs it on the linux-amd64 job so the
// case runs somewhere every PR, and a developer without it still gets the link
// coverage.
//
// Wine lives here and not in the compiler. crossExecCommand deliberately
// refuses a non-host native target rather than reaching for an emulator — a
// built artifact's runtime is the test layer's business, the same seam through
// which wasmtime and node already run (docs/code-style.md §"Host tools in Go
// sources"). `promise run -target x86_64-pc-windows-msvc` still errors.
func TestWindowsCrossBuiltExeRunsUnderWine(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("skipping cross-build integration test in short mode")
	}
	if windowsCrossTripleIsNative() {
		// Wine's premise is a foreign binary. Here the .exe is the host's own
		// and runs directly — TestRunWithNativeTripleExecutes covers that. A
		// `wine` on PATH must not turn this into a third, undefined path.
		t.Skip("the .exe is native on this host; nothing for wine to emulate")
	}
	wine, err := exec.LookPath("wine") // path-ok: a runtime that executes a built artifact without contributing to it
	if err != nil {
		t.Skip("wine is not installed, so the cross-built .exe was not run")
	}

	exePath := buildWindowsExe(t, helloSource)

	// A prefix of its own: a test must not write to the developer's ~/.wine,
	// and a fresh one makes the run reproducible. WINEDEBUG silences the
	// fixme/err chatter that would otherwise dominate the failure output;
	// mscoree=d stops a first run from trying to fetch Wine Mono, which we
	// neither need nor want reaching the network from a test.
	//
	// Bounded like every other child here: initializing a fresh prefix is slow
	// and a wedged wineserver would otherwise hang until Go's global timeout,
	// which reports the panic against whatever test was running rather than
	// against wine.
	budget := clitest.Budget()
	ctx, cancel := context.WithTimeout(context.Background(), budget)
	defer cancel()

	prefix := clitest.TempDir(t)
	cmd := exec.CommandContext(ctx, wine, exePath)
	cmd.Env = append(os.Environ(),
		"WINEPREFIX="+prefix, "WINEDEBUG=-all", "WINEDLLOVERRIDES=mscoree=d")
	out, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		t.Fatalf("wine did not finish within %s (killed):\n%s", budget, out)
	}
	if err != nil {
		t.Fatalf("running the cross-built .exe under wine failed: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "cross-built and running") {
		t.Errorf("cross-built .exe printed no expected output under wine:\n%s", out)
	}
}

// TestWindowsCrossLinkReleaseMode covers the other backend pipeline. Windows
// forces useLTO=false whatever the build mode, so -release and debug take the
// same opt → llc → lld-link route by a DIFFERENT branch of the useLTO
// expression — a branch that would be the one to break if LTO is ever wired up
// for MSVC (T0049), and which the debug-mode test above never touches.
func TestWindowsCrossLinkReleaseMode(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("skipping cross-build integration test in short mode")
	}
	exePath := buildWindowsExe(t, helloSource, "-release")

	dlls := importedDLLs(t, exePath)
	if !slices.Contains(dlls, "kernel32.dll") {
		t.Fatalf("release cross-build imports no kernel32.dll (imports: %v)", dlls)
	}
	for _, dll := range dlls {
		if !windowsShippedDLLs[dll] {
			t.Errorf("release cross-built .exe imports %s, which does not ship with Windows", dll)
		}
	}
}

// TestWindowsCrossBuildDefaultsToExeName: with no -o, the output is named from
// the TARGET, not from the host. A Linux host defaulting to an extensionless
// name would hand the user a file Windows will not execute.
//
// The child runs with its cwd in a temp dir because `promise build` writes the
// default output there rather than beside the source — asserting on the name
// means controlling where it lands, not just what it is called.
func TestWindowsCrossBuildDefaultsToExeName(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("skipping cross-build integration test in short mode")
	}
	bin := clitest.Bin(t)

	dir := clitest.TempDir(t)
	if err := os.WriteFile(filepath.Join(dir, "prog.pr"), []byte(helloSource), 0644); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(bin, "build", "-target", windowsCrossTriple, "prog.pr")
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build -target %s with no -o failed: %v\n%s", windowsCrossTriple, err, out)
	}

	if _, err := os.Stat(filepath.Join(dir, "prog.exe")); err != nil {
		entries, _ := os.ReadDir(dir)
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("no prog.exe after a default-named windows cross build; dir holds %v", names)
	}
}

// TestWindowsCrossRunRefusesToExecute pins the seam this target's arrival
// moved. `-target x86_64-pc-windows-msvc` used to be rejected by the flag gate,
// so `run` failed before compiling anything; now the gate accepts it and the
// refusal comes later, from crossExecCommand, after a successful build.
//
// The refusal is about a NON-HOST target (docs/windows-support.md: "a
// non-Windows host links the .exe but cannot run it"), so this case belongs to
// the hosts where that triple is foreign. On windows-amd64 the CLI cannot reach
// the seam at all: supportedTargets() there is the host plus the two wasm
// targets, every one of them executable, and any other triple is turned away by
// the flag gate with the wording asserted against below. The skip is therefore
// a property of the target set, not a gap someone forgot to fill — it lifts as
// soon as a second cross-linkable target ships (T0530/T0532).
//
// Both halves are asserted. That it FAILS keeps a cross-built binary from ever
// looking like it ran here. That it does NOT fail with the flag gate's wording
// keeps the target from being quietly un-listed again — which would restore the
// old message and pass a test that only checked for a non-zero exit.
func TestWindowsCrossRunRefusesToExecute(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("skipping cross-build integration test in short mode")
	}
	if windowsCrossTripleIsNative() {
		t.Skip("x86_64-pc-windows-msvc is this host's native target, so run executes it: " +
			"TestRunWithNativeTripleExecutes asserts that side, and TestHostTargetMatrix " +
			"in package main covers the refusal predicate for every host")
	}
	bin := clitest.Bin(t)

	dir := clitest.TempDir(t)
	srcPath := filepath.Join(dir, "prog.pr")
	if err := os.WriteFile(srcPath, []byte(helloSource), 0644); err != nil {
		t.Fatal(err)
	}

	res := clitest.Run(t, bin, nil, "run", "-target", windowsCrossTriple, srcPath)
	if res.ExitCode == 0 {
		t.Fatalf("run -target %s succeeded; a windows binary cannot run on this host%s",
			windowsCrossTriple, res.Detail())
	}
	out := res.Combined()
	if !strings.Contains(out, "cross-target execution is not supported") {
		t.Errorf("run -target %s did not explain that execution is unsupported%s",
			windowsCrossTriple, res.Detail())
	}
	if strings.Contains(out, "cannot be built by this release") {
		t.Errorf("run -target %s was refused by the target gate rather than at execution"+
			" — the triple has dropped out of supportedTargets()%s", windowsCrossTriple, res.Detail())
	}
	// And the program genuinely did not run: its own output must be absent.
	// Without this, a `run` that somehow executed the binary AND printed a
	// complaint afterwards would still satisfy the checks above.
	if strings.Contains(out, "cross-built and running") {
		t.Errorf("the windows binary produced its output on this host — it was executed,"+
			" not refused%s", res.Detail())
	}
}

// TestWindowsArm64CrossBuildIsRejected is the other side of the boundary: one
// Windows triple links here and its sibling does not, so the sibling has to be
// turned away by name. Handing lld-link the amd64 import libs for an arm64
// object would otherwise fail deep in the linker with a machine-type mismatch
// (T0772), long after the point where the reason is still obvious.
func TestWindowsArm64CrossBuildIsRejected(t *testing.T) {
	t.Parallel()
	bin := clitest.Bin(t)

	dir := clitest.TempDir(t)
	srcPath := filepath.Join(dir, "prog.pr")
	if err := os.WriteFile(srcPath, []byte(helloSource), 0644); err != nil {
		t.Fatal(err)
	}
	outPath := filepath.Join(dir, "prog.exe")

	res := clitest.Run(t, bin, nil, "build", "-target", "aarch64-pc-windows-msvc", "-o", outPath, srcPath)
	if res.ExitCode == 0 {
		t.Fatalf("build -target aarch64-pc-windows-msvc succeeded; no arm64 import libs exist%s", res.Detail())
	}
	out := res.Combined()
	// Reported as known-but-unlinkable, not as a typo: it is a real triple that
	// `emit-ir` accepts today.
	if !strings.Contains(out, "cannot be built by this release") {
		t.Errorf("arm64 windows was not reported as a known-but-unlinkable target%s", res.Detail())
	}
	if strings.Contains(out, "invalid target") {
		t.Errorf("arm64 windows reported as an invalid triple, sending the reader after a typo%s", res.Detail())
	}
	if _, err := os.Stat(outPath); err == nil {
		t.Error("a rejected target still produced an output file")
	}
}
