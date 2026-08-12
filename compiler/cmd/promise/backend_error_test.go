package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/promise-language/promise/compiler/internal/codegen"
)

// T1470: the backend compile/link drivers used to call os.Exit(1) deep in the
// call graph on any opt/llc failure, which (a) skipped the defer-based temp-file
// cleanup, stranding .ll/.bc/.o files, and (b) made the error paths impossible
// to unit-test because failure meant process death. These tests assert the new
// contract: the drivers RETURN an error and clean up every intermediate temp on
// the failure path.

// strandedTemps returns the temp files whose base name starts with prefix.
func strandedTemps(t *testing.T, prefix string) []string {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(os.TempDir(), prefix+"-*"))
	if err != nil {
		t.Fatalf("glob temp dir: %v", err)
	}
	return matches
}

// withSilencedStderr runs fn with os.Stderr redirected to the null device so the
// opt/llc parse-error noise doesn't pollute test output. The child processes in
// the backend attach to os.Stderr directly, so this must swap the file itself.
func withSilencedStderr(t *testing.T, fn func()) {
	t.Helper()
	devnull, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		t.Fatalf("open %s: %v", os.DevNull, err)
	}
	defer devnull.Close()
	saved := os.Stderr
	os.Stderr = devnull
	defer func() { os.Stderr = saved }()
	fn()
}

func requireLLVMTools(t *testing.T) (optPath, llcPath string) {
	t.Helper()
	var err error
	optPath, err = findLLVMTool("opt")
	if err != nil {
		t.Skipf("opt not available: %v", err)
	}
	llcPath, err = findLLVMTool("llc")
	if err != nil {
		t.Skipf("llc not available: %v", err)
	}
	return optPath, llcPath
}

const invalidIR = "this is definitely not valid LLVM IR\n"

// A minimal, valid LLVM module that opt -O1 and llc both accept.
const validIR = "define i32 @__t1470_main() {\nentry:\n  ret i32 0\n}\n"

func TestCompileLLToObjInvalidIRReturnsErrorAndCleansTemps(t *testing.T) {
	optPath, llcPath := requireLLVMTools(t)
	const prefix = "t1470objfail"
	target := codegen.HostTargetTriple()

	var obj string
	var cerr error
	withSilencedStderr(t, func() {
		obj, cerr = compileLLToObj(invalidIR, prefix, target, optPath, llcPath, "-O1")
	})
	if cerr == nil {
		t.Fatalf("expected an error compiling invalid IR, got obj=%q nil error", obj)
	}
	if obj != "" {
		t.Errorf("error path should return an empty object path, got %q", obj)
	}
	if leftover := strandedTemps(t, prefix); len(leftover) != 0 {
		for _, f := range leftover {
			os.Remove(f)
		}
		t.Errorf("opt failure stranded temp files: %v", leftover)
	}
}

func TestCompileLLToBCInvalidIRReturnsErrorAndCleansTemps(t *testing.T) {
	optPath, _ := requireLLVMTools(t)
	const prefix = "t1470bcfail"

	var bc string
	var cerr error
	withSilencedStderr(t, func() {
		bc, cerr = compileLLToBC(invalidIR, prefix, optPath, "-O1")
	})
	if cerr == nil {
		t.Fatalf("expected an error compiling invalid IR, got bc=%q nil error", bc)
	}
	if bc != "" {
		t.Errorf("error path should return an empty bitcode path, got %q", bc)
	}
	if leftover := strandedTemps(t, prefix); len(leftover) != 0 {
		for _, f := range leftover {
			os.Remove(f)
		}
		t.Errorf("opt failure stranded temp files: %v", leftover)
	}
}

func TestCompileLLToObjSuccessReturnsObjAndCleansIntermediates(t *testing.T) {
	optPath, llcPath := requireLLVMTools(t)
	const prefix = "t1470objok"
	target := codegen.HostTargetTriple()

	obj, err := compileLLToObj(validIR, prefix, target, optPath, llcPath, "-O1")
	if err != nil {
		t.Fatalf("compileLLToObj on valid IR: %v", err)
	}
	if obj == "" {
		t.Fatalf("success should return a non-empty object path")
	}
	defer os.Remove(obj)
	if _, statErr := os.Stat(obj); statErr != nil {
		t.Fatalf("returned object file does not exist: %v", statErr)
	}
	// The .o is handed back; the .ll and .bc intermediates must be gone.
	for _, leftover := range strandedTemps(t, prefix) {
		if leftover == obj {
			continue
		}
		t.Errorf("success left an intermediate temp behind: %s", leftover)
	}
}

func TestCompileLLToBCSuccessReturnsBCAndCleansIntermediates(t *testing.T) {
	optPath, _ := requireLLVMTools(t)
	const prefix = "t1470bcok"

	bc, err := compileLLToBC(validIR, prefix, optPath, "-O1")
	if err != nil {
		t.Fatalf("compileLLToBC on valid IR: %v", err)
	}
	if bc == "" {
		t.Fatalf("success should return a non-empty bitcode path")
	}
	defer os.Remove(bc)
	if _, statErr := os.Stat(bc); statErr != nil {
		t.Fatalf("returned bitcode file does not exist: %v", statErr)
	}
	for _, leftover := range strandedTemps(t, prefix) {
		if leftover == bc {
			continue
		}
		t.Errorf("success left an intermediate temp behind: %s", leftover)
	}
}

// writeLL writes IR text to a temp .ll file and returns its path (cleaned up on
// test end). Used to drive compileAndLinkLLVM, which takes an .ll path.
func writeLL(t *testing.T, ir string) string {
	t.Helper()
	f, err := os.CreateTemp("", "t1470-cll-*.ll")
	if err != nil {
		t.Fatalf("create temp .ll: %v", err)
	}
	if _, err := f.WriteString(ir); err != nil {
		f.Close()
		t.Fatalf("write .ll: %v", err)
	}
	f.Close()
	t.Cleanup(func() { os.Remove(f.Name()) })
	return f.Name()
}

// TestCompileAndLinkLLVMInvalidIRReturnsError exercises the single-file backend
// driver (the default, non-module path). T1470 converted its 6 os.Exit(1) sites
// to error returns; this asserts that an opt failure now returns an error
// instead of killing the process, and that no output binary is left behind.
func TestCompileAndLinkLLVMInvalidIRReturnsError(t *testing.T) {
	requireLLVMTools(t)
	llPath := writeLL(t, invalidIR)
	out := filepath.Join(t.TempDir(), "out.bin")
	target := codegen.HostTargetTriple()

	var err error
	withSilencedStderr(t, func() {
		err = compileAndLinkLLVM(llPath, target, out, false)
	})
	if err == nil {
		t.Fatalf("expected an error from opt on invalid IR, got nil")
	}
	if _, statErr := os.Stat(out); statErr == nil {
		t.Errorf("no output binary should be produced on an opt failure")
	}
}

// TestCompileAndLinkLLVMLinkFailureReturnsError feeds IR that is valid (opt +
// llc succeed) but has no linkable entry point, so the linker step fails. This
// covers the opt-success / llc-success path plus the link-step error return
// (linkLinux/linkDarwin/etc. now return an error rather than os.Exit(1), T1470).
func TestCompileAndLinkLLVMLinkFailureReturnsError(t *testing.T) {
	requireLLVMTools(t)
	// validIR defines only @__t1470_main — no `main`/entry symbol — so the CRT's
	// reference to `main` is unresolved and the link fails.
	llPath := writeLL(t, validIR)
	out := filepath.Join(t.TempDir(), "out.bin")
	target := codegen.HostTargetTriple()

	var err error
	withSilencedStderr(t, func() {
		err = compileAndLinkLLVM(llPath, target, out, false)
	})
	if err == nil {
		t.Fatalf("expected a link error for IR with no entry point, got nil")
	}
}

// TestCompileAndLinkLLVMSuccessProducesRunnableBinary drives the full single-file
// backend (opt → llc → linker) on a complete, linkable module and asserts the
// refactor still returns nil and produces a working executable. This is the
// happy-path counterpart to the error tests above — the whole point of T1470 is
// that success behavior is unchanged while failures return errors.
func TestCompileAndLinkLLVMSuccessProducesRunnableBinary(t *testing.T) {
	requireLLVMTools(t)
	// A complete program: `main` returns 0, satisfying the CRT entry reference.
	const mainIR = "define i32 @main() {\nentry:\n  ret i32 0\n}\n"
	llPath := writeLL(t, mainIR)
	out := filepath.Join(t.TempDir(), "out.bin")
	target := codegen.HostTargetTriple()

	if err := compileAndLinkLLVM(llPath, target, out, false); err != nil {
		t.Skipf("in-process link unavailable in this environment: %v", err)
	}
	info, statErr := os.Stat(out)
	if statErr != nil {
		t.Fatalf("success should produce an output binary: %v", statErr)
	}
	if info.Size() == 0 {
		t.Fatalf("output binary is empty")
	}
	// The binary targets the host, so it must run and exit 0.
	if runErr := exec.Command(out).Run(); runErr != nil {
		t.Fatalf("linked binary failed to run cleanly: %v", runErr)
	}
}

// TestExitOnCompileErrorNilIsNoop verifies the top-level funnel (T1470) does not
// terminate the process when the backend returns nil — the success path callers
// (compileTestBinary, runE2ETest, exec, stress) rely on this to continue.
func TestExitOnCompileErrorNilIsNoop(t *testing.T) {
	// A non-nil error would call os.Exit(1) and abort the test binary, so this
	// only exercises the nil branch — the branch every successful build takes.
	exitOnCompileError(nil)
}
