package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/llir/llvm/ir"
	"github.com/llir/llvm/ir/constant"
	irtypes "github.com/llir/llvm/ir/types"

	"github.com/promise-language/promise/compiler/internal/ast"
	"github.com/promise-language/promise/compiler/internal/codegen"
	"github.com/promise-language/promise/compiler/internal/sema"
)

// TestBundledLibSystemLinksRepresentativePrograms is the regression test T1609
// asks for. findMacOSSDK now always resolves to the bundled libSystem TBD stub
// (T1609) — it is the only thing standing between "host SDK missing, unlicensed,
// or unparseable" and every macOS link, so a gap in it is no longer a fallback
// bug but a full outage. A hand-maintained symbol list drifts silently: one
// missing symbol (`_clock_gettime`, this item's original finding) grew to
// thirteen by the time the item's own audit reran, because the only existing
// link coverage (TestDarwinTLSLinksWithoutXcode, darwin_tls_link_test.go) only
// ever exercised the TLS surface.
//
// This links representative, already-existing programs — every catalog module
// compiled as its own test binary (exercising its own *_test.pr suite, exactly
// as `promise test modules/<name>/` does), a plain `test binary with no
// imports (this item's own repro), and a `tests/e2e` file (this item's own
// second finding — see below) — against nothing but the bundled stub. A
// symbol newly missing from libSystem fails here with the linker's own
// "undefined symbol" message, on the PR that introduces the gap, instead of
// waiting for a user on a fresh macOS install to find it.
//
// One gap survived the first version of this test: `_bzero`. `bin/verify` found
// it in nine files under tests/e2e and examples/, all SNAPSHOT tests
// (`main() `test(expected: ...)`), which compile through a materially different
// path (runE2ETest: the user's own main(), CompileOptions{TestBinary: true},
// never GenerateTestMain) than every case this test originally covered (batch
// batch tagged-test binaries via GenerateTestMain). opt/llc lowers a codegen-emitted,
// always-present zero-fill memset (SIGPIPE/SIGSEGV handler setup in every
// program's `_main`) to a `bzero` call once the surrounding code is shaped a
// certain way; that shape depends on which compile path produced it, so a
// representative set that only exercises one path can silently not be
// representative of the other. tests/e2e/named_args.pr is now covered below via
// the snapshot path specifically for this reason, not because it is special.
func TestBundledLibSystemLinksRepresentativePrograms(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("Mach-O link against the bundled macOS SDK stubs needs a darwin host")
	}

	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Skip("cannot determine source file path")
	}
	repoRoot := filepath.Join(filepath.Dir(thisFile), "..", "..", "..")
	modulesDir := filepath.Join(repoRoot, "modules")
	if _, err := os.Stat(filepath.Join(modulesDir, "std")); err != nil {
		t.Skipf("repo root not available at %s (running from installed binary?)", repoRoot)
	}

	// Catalog modules spanning the surface the audit found gaps in: file I/O,
	// networking, OS/process info, TLS, HTTP, timing, JSON and crypto.
	for _, name := range []string{"io", "net", "os", "tls", "http", "time", "json", "crypto"} {
		t.Run(name, func(t *testing.T) {
			t.Setenv("PROMISE_HOME", t.TempDir())
			modDir := filepath.Join(modulesDir, name)
			file, info := compileModuleTestFrontend(modDir, "")
			binPath, err := linkTestBinaryLikeCLI(file, info, modDir)
			if err != nil {
				t.Fatalf("modules/%s failed to link against the bundled SDK stub: %v", name, err)
			}
			os.Remove(binPath)
		})
	}

	// The bug's own repro: a plain `test binary with no module imports at all —
	// this is where `_clock_gettime` (referenced by the codegen-emitted
	// promise_test_run/GenerateTestMain harness, not by any PAL declaration)
	// first surfaced.
	t.Run("plain_no_imports", func(t *testing.T) {
		t.Setenv("PROMISE_HOME", t.TempDir())
		dir := t.TempDir()
		src := filepath.Join(dir, "plain_test.pr")
		body := "plain_math() `test {\n  assert(1 + 1 == 2, \"arithmetic works\");\n}\n"
		if err := os.WriteFile(src, []byte(body), 0644); err != nil {
			t.Fatal(err)
		}
		file, info := compileFrontendForTarget(src, "")
		binPath, err := linkTestBinaryLikeCLI(file, info, src)
		if err != nil {
			t.Fatalf("a plain `test binary with no imports failed to link against the bundled SDK stub: %v", err)
		}
		os.Remove(binPath)
	})

	// tests/e2e/named_args.pr: this item's second finding, and the reason this
	// test now covers two distinct compile paths, not one. `named_args.pr` is a
	// SNAPSHOT test (`main() `test(expected: ...)`), which compiles through
	// runE2ETest — CompileOptions{DebugAllocator: true, TestBinary: true} plus
	// the user's own main(), never GenerateTestMain — not through
	// linkTestBinaryLikeCLI's batch-test path. A codegen-emitted, always-present
	// zero-fill memset (SIGPIPE/SIGSEGV handler setup in every program's `_main`)
	// only survives to a `bzero` libcall once TestBinary's extra liveness-signal
	// code changes the surrounding function enough that opt doesn't scalarize it
	// away first; the batch-test path above never triggered it. Kept as a real
	// file (not reduced to a minimal repro) so it tracks whatever future compiler
	// changes shift that threshold, on either path.
	t.Run("e2e_named_args", func(t *testing.T) {
		t.Setenv("PROMISE_HOME", t.TempDir())
		src := filepath.Join(repoRoot, "tests", "e2e", "named_args.pr")
		if _, err := os.Stat(src); err != nil {
			t.Skipf("tests/e2e/named_args.pr not available at %s", src)
		}
		binPath, err := linkSnapshotTestBinaryLikeCLI(src)
		if err != nil {
			t.Fatalf("tests/e2e/named_args.pr failed to link against the bundled SDK stub: %v", err)
		}
		os.Remove(binPath)
	})
}

// linkTestBinaryLikeCLI compiles and links a BATCH test binary (a tagged-test file +
// assert()) the same way `promise test` does by default — with the default 60s
// per-test timeout (computeTestTimeouts) and the default per-test memory-limit
// accounting (T0689's defaultMemoryLimitBytes) both computed, not
// compileTestBinary's bare nil/nil, so a test that skips these defaults isn't
// checking a program the CLI ever actually produces. Do not use this for a
// SNAPSHOT test (`main() `test(expected: ...)`) — see linkSnapshotTestBinaryLikeCLI.
func linkTestBinaryLikeCLI(file *ast.File, info *sema.Info, sourceFile string) (string, error) {
	cfg := testTimeoutConfig{defaultTimeout: 60 * time.Second, scale: 1.0, defaultMemoryBytes: defaultMemoryLimitBytes}
	testTimeouts := computeTestTimeouts(info.Tests, info, cfg)
	testMemoryLimits := computeTestMemoryLimits(info.Tests, info, cfg)
	return compileTestBinary(file, info, "", sourceFile, testTimeouts, testMemoryLimits)
}

// linkSnapshotTestBinaryLikeCLI compiles and links a SNAPSHOT test
// (`main() `test(expected: ...)`) the way runE2ETest does: the user's own
// main(), not GenerateTestMain's synthesized harness, with
// CompileOptions{DebugAllocator: true, TestBinary: true} (TestBinary emits the
// liveness signal, T1815, even though main() is unchanged). This is a
// materially different compile from a batch test binary — see
// TestBundledLibSystemLinksRepresentativePrograms's e2e_named_args case.
func linkSnapshotTestBinaryLikeCLI(sourceFile string) (string, error) {
	file, info := compileFrontendForTarget(sourceFile, "")
	target := codegen.HostTargetTriple()
	result := codegen.CompileWithOptions(file, info, target, &codegen.CompileOptions{
		DebugAllocator: true,
		TestBinary:     true,
	})
	tmpOutput, err := os.CreateTemp("", "promise-e2e-*"+binaryExtension(target))
	if err != nil {
		return "", err
	}
	tmpOutput.Close()
	if err := compileAndLink(result, tmpOutput.Name(), target, sourceFile, false); err != nil {
		os.Remove(tmpOutput.Name())
		return "", err
	}
	return tmpOutput.Name(), nil
}

// TestBundledLibSystemLinkFailsOnMissingSymbol is the negative control for
// TestBundledLibSystemLinksRepresentativePrograms — the same concern
// TestDarwinTLSLinksWithoutXcode raises for the framework stubs: "a link test
// that cannot fail proves nothing." It proves the bundled stub is actually
// load-bearing by stripping one of the symbols this item added (`_bzero`) and
// confirming ld64.lld refuses to link a trivial object that references it,
// with the same "undefined symbol" error this item's bug report describes —
// so a future change that silently swallows the link error (or otherwise makes
// the representative-programs test vacuously pass) gets caught here instead.
//
// This builds its own minimal IR rather than reusing a compiled Promise
// program, and links to a hand-built sysroot rather than editing
// ensureBundledSDK's cache in place, because ensureBundledSDK rewrites its
// cache entry whenever the on-disk file size differs from the compiled-in
// constant (T1609's own bug report describes exactly this obstacle to
// verifying the stub by hand) — a second call from anywhere in this process
// would undo the corruption before the negative link runs.
func TestBundledLibSystemLinkFailsOnMissingSymbol(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("Mach-O link against the bundled macOS SDK stubs needs a darwin host")
	}
	llc, err := findLLVMTool("llc")
	if err != nil {
		t.Skipf("llc unavailable: %v", err)
	}
	linker, err := findDarwinLinker()
	if err != nil {
		t.Skipf("ld64.lld unavailable: %v", err)
	}

	tmp := t.TempDir()
	t.Setenv("PROMISE_HOME", tmp)
	sdk, err := ensureBundledSDK()
	if err != nil {
		t.Fatalf("ensureBundledSDK failed: %v", err)
	}

	tri := parseDarwinTriple(runtime.GOARCH + "-apple-macosx14.0.0")
	triple := tri.arch + "-apple-macosx" + tri.minVersion

	// A trivial module that calls _bzero directly — no PAL, no frontend, so
	// this exercises exactly the stub, not the compiler above it.
	module := ir.NewModule()
	module.TargetTriple = triple
	bzeroFn := module.NewFunc("bzero", irtypes.Void,
		ir.NewParam("ptr", irtypes.I8Ptr), ir.NewParam("len", irtypes.I64))
	caller := module.NewFunc("call_bzero", irtypes.Void)
	block := caller.NewBlock("")
	block.NewCall(bzeroFn, constant.NewNull(irtypes.I8Ptr), constant.NewInt(irtypes.I64, 0))
	block.NewRet(nil)

	llPath := filepath.Join(tmp, "bzero.ll")
	if err := os.WriteFile(llPath, []byte(module.String()), 0644); err != nil {
		t.Fatal(err)
	}
	objPath := filepath.Join(tmp, "bzero.o")
	if out, err := exec.Command(llc, "-filetype=obj", "-mtriple="+triple, llPath, "-o", objPath).CombinedOutput(); err != nil {
		t.Fatalf("llc could not compile the negative-control IR: %v\n%s", err, out)
	}

	link := func(sysroot, out string) ([]byte, error) {
		return exec.Command(linker,
			"-dylib", "-arch", tri.arch,
			"-platform_version", "macos", tri.minVersion, tri.minVersion,
			"-syslibroot", sysroot, "-o", out, objPath,
			"-lSystem",
		).CombinedOutput()
	}

	// Positive control — the real bundled stub links fine.
	if out, err := link(sdk.sysroot, filepath.Join(tmp, "bzero-ok.dylib")); err != nil {
		t.Fatalf("a program referencing _bzero cannot link against the real bundled stub: %v\n%s", err, out)
	}

	// Negative control — a hand-built sysroot missing _bzero must fail to link.
	trimmed := strings.Replace(bundledLibSystemTBD, "_bzero, ", "", 1)
	if trimmed == bundledLibSystemTBD {
		t.Fatal("negative control could not remove _bzero from the stub")
	}
	badLibDir := filepath.Join(tmp, "bad-sdk", "usr", "lib")
	if err := os.MkdirAll(badLibDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(badLibDir, "libSystem.tbd"), []byte(trimmed), 0644); err != nil {
		t.Fatal(err)
	}
	if out, err := link(filepath.Join(tmp, "bad-sdk"), filepath.Join(tmp, "bzero-bad.dylib")); err == nil {
		t.Fatalf("link succeeded with _bzero missing from the stub — this test is not actually checking the stub\n%s", out)
	} else if !strings.Contains(string(out), "_bzero") {
		t.Errorf("link failed for some reason other than the removed symbol:\n%s", out)
	}
}
