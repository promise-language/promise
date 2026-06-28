package pal

// Runtime crash tests for the T0365 sentinel-header debug allocator.
//
// Each test programmatically constructs an LLVM module that exercises
// pal_alloc/pal_free directly, compiles it with clang, runs the resulting
// binary, and asserts that the expected abort message is written to stderr
// with exit code 134 (SIGABRT-style).
//
// The tests are skipped when clang is not on PATH, so they don't break CI
// in environments without a system LLVM. Skipped by default in -short mode.

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/llir/llvm/ir"
	"github.com/llir/llvm/ir/constant"
	irtypes "github.com/llir/llvm/ir/types"
)

// findClang locates a usable C compiler on PATH. Returns "" if none found.
func findClang() string {
	for _, name := range []string{"clang", "cc", "gcc"} {
		if p, err := exec.LookPath(name); err == nil {
			return p
		}
	}
	return ""
}

// buildAndRunDebugAlloc emits modBuilder's IR into a temp .ll, compiles with
// clang into a binary, runs it, and returns (stdout, stderr, exitCode).
func buildAndRunDebugAlloc(t *testing.T, modBuilder func() *ir.Module) (string, string, int) {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping debug allocator runtime test in -short mode")
	}
	if runtime.GOOS == "windows" {
		t.Skip("debug allocator runtime tests are POSIX-only (clang+libc)")
	}
	clang := findClang()
	if clang == "" {
		t.Skip("clang/cc/gcc not found on PATH")
	}

	mod := modBuilder()
	tmp := t.TempDir()
	llPath := filepath.Join(tmp, "test.ll")
	binPath := filepath.Join(tmp, "test")
	if err := os.WriteFile(llPath, []byte(mod.String()), 0644); err != nil {
		t.Fatalf("write IR: %v", err)
	}

	// Use clang to compile + link (drops a binary). -O0 keeps the IR
	// transparent so any abort branch isn't optimized away. Wno-override-module
	// quiets a benign warning about target triple mismatches.
	cmd := exec.Command(clang, "-O0", "-Wno-override-module", "-o", binPath, llPath)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("clang failed: %v\n%s", err, out)
	}

	// Run the produced binary; capture stderr separately from stdout.
	runCmd := exec.Command(binPath)
	stderr := &strings.Builder{}
	stdout := &strings.Builder{}
	runCmd.Stderr = stderr
	runCmd.Stdout = stdout
	err := runCmd.Run()

	exit := 0
	if ee, ok := err.(*exec.ExitError); ok {
		exit = ee.ExitCode()
	} else if err != nil {
		t.Fatalf("unexpected run error: %v", err)
	}
	return stdout.String(), stderr.String(), exit
}

// makeDebugAllocModule builds a self-contained module with PAL alloc/free/realloc
// emitted in DebugAllocator mode, plus a `@main` function defined by mainBuilder.
// Returns the module ready for emission.
func makeDebugAllocModule(t *testing.T, mainBuilder func(m *ir.Module, palAlloc, palFree, palRealloc *ir.Func)) *ir.Module {
	t.Helper()
	m := ir.NewModule()
	m.TargetTriple = ""
	p := &PosixPAL{target: runtime.GOOS, DebugAllocator: true}
	palAlloc := p.EmitAlloc(m)
	palFree := p.EmitFree(m)
	palRealloc := p.EmitRealloc(m)
	mainBuilder(m, palAlloc, palFree, palRealloc)
	return m
}

// TestRuntimeDoubleFreeAborts: pal_free(p); pal_free(p) must abort.
//
// The exact message can be either "double free" (when libc has not yet
// overwritten the MAGIC_FREED markers we wrote at offsets 0 and 8 of the
// header) or "bad header magic" (when libc reclaimed both slots for free-list
// bookkeeping — common on macOS libsystem_malloc, where both slots get used).
// Either message is a valid double-free detection — the bug is caught and the
// program aborts with code 134 in both cases.
func TestRuntimeDoubleFreeAborts(t *testing.T) {
	stdout, stderr, exit := buildAndRunDebugAlloc(t, func() *ir.Module {
		return makeDebugAllocModule(t, func(m *ir.Module, palAlloc, palFree, _ *ir.Func) {
			fn := m.NewFunc("main", irtypes.I32)
			b := fn.NewBlock("entry")
			p := b.NewCall(palAlloc, constant.NewInt(irtypes.I64, 64))
			b.NewCall(palFree, p)
			b.NewCall(palFree, p)
			b.NewRet(constant.NewInt(irtypes.I32, 0))
		})
	})
	if exit != 134 {
		t.Errorf("expected exit code 134 (SIGABRT-style), got %d", exit)
	}
	if !strings.Contains(stderr, "double free") && !strings.Contains(stderr, "bad header magic") {
		t.Errorf("expected double-free or bad-magic abort in stderr, got: stderr=%q stdout=%q", stderr, stdout)
	}
}

// TestRuntimeBadFreeAborts: pal_free on a stack pointer → bad header magic.
func TestRuntimeBadFreeAborts(t *testing.T) {
	stdout, stderr, exit := buildAndRunDebugAlloc(t, func() *ir.Module {
		return makeDebugAllocModule(t, func(m *ir.Module, _, palFree, _ *ir.Func) {
			fn := m.NewFunc("main", irtypes.I32)
			b := fn.NewBlock("entry")
			// 64-byte stack buffer of zeros — magic_alive at -16 will be all zeros,
			// not MAGIC_ALIVE / MAGIC_FREED → "bad header magic" path.
			arr := b.NewAlloca(irtypes.NewArray(64, irtypes.I8))
			p := b.NewBitCast(arr, irtypes.I8Ptr)
			// Offset by 32 so header check at p-16 hits zeroed alloca region.
			off := b.NewGetElementPtr(irtypes.I8, p, constant.NewInt(irtypes.I64, 32))
			b.NewCall(palFree, off)
			b.NewRet(constant.NewInt(irtypes.I32, 0))
		})
	})
	if exit != 134 {
		t.Errorf("expected exit code 134, got %d", exit)
	}
	if !strings.Contains(stderr, "bad header magic") {
		t.Errorf("expected 'bad header magic' in stderr, got: stderr=%q stdout=%q", stderr, stdout)
	}
}

// TestRuntimeTailCorruptionAborts: write past end of allocation, then free →
// tail sentinel mismatch.
func TestRuntimeTailCorruptionAborts(t *testing.T) {
	stdout, stderr, exit := buildAndRunDebugAlloc(t, func() *ir.Module {
		return makeDebugAllocModule(t, func(m *ir.Module, palAlloc, palFree, _ *ir.Func) {
			fn := m.NewFunc("main", irtypes.I32)
			b := fn.NewBlock("entry")
			// Allocate 8 bytes, then write a single byte at offset 8 (clobbers
			// the first byte of the tail sentinel).
			p := b.NewCall(palAlloc, constant.NewInt(irtypes.I64, 8))
			oob := b.NewGetElementPtr(irtypes.I8, p, constant.NewInt(irtypes.I64, 8))
			b.NewStore(constant.NewInt(irtypes.I8, 0x42), oob)
			b.NewCall(palFree, p)
			b.NewRet(constant.NewInt(irtypes.I32, 0))
		})
	})
	if exit != 134 {
		t.Errorf("expected exit code 134, got %d", exit)
	}
	if !strings.Contains(stderr, "tail sentinel mismatch") {
		t.Errorf("expected 'tail sentinel mismatch' in stderr, got: stderr=%q stdout=%q", stderr, stdout)
	}
}

// hostSigsegvTarget returns a PosixPAL target string whose codegen decisions
// (Darwin vs Linux handler, glibc vs musl sigaction layout) match the host the
// test binary will actually run on. The struct layouts only depend on the OS
// and libc, not the architecture, so a representative triple is sufficient.
//
// The libc is taken from `cc -dumpmachine` — the compiler's own default target
// triple, i.e. exactly the libc it will link. This is decisive where a mere
// loader-file check is not: a glibc host may also have the musl cross-loader
// installed (e.g. musl-tools) without clang linking musl by default.
func hostSigsegvTarget(cc string) string {
	if runtime.GOOS == "darwin" {
		return "arm64-apple-darwin23.0.0"
	}
	if cc != "" {
		if out, err := exec.Command(cc, "-dumpmachine").Output(); err == nil &&
			strings.Contains(string(out), "musl") {
			return "x86_64-unknown-linux-musl"
		}
	}
	return "x86_64-unknown-linux-gnu"
}

// TestRuntimeSigsegvHandlerPrintsFaultAddress is the end-to-end T1161 check.
//
// A real SIGSEGV (deliberate null-pointer write) must be caught by
// @__promise_sigsegv_handler, which reads si_addr from siginfo_t, formats it as
// "fatal: segmentation fault at 0x<16 hex>", writes it to stderr, and _exit(2)s.
// This exercises the shared emitSigsegvAddrHandler end-to-end on the host:
// Linux reads si_addr at offset 16, macOS at offset 24 — the exact-zero address
// assertion below would fail if the offset (or the SA_SIGINFO registration that
// delivers siginfo_t at all) were wrong. IR-shape tests can't catch an off-by-N
// offset; only a real signal delivery can.
func TestRuntimeSigsegvHandlerPrintsFaultAddress(t *testing.T) {
	target := hostSigsegvTarget(findClang())
	stdout, stderr, exit := buildAndRunDebugAlloc(t, func() *ir.Module {
		m := ir.NewModule()
		m.TargetTriple = ""
		p := &PosixPAL{target: target}
		initFn := p.EmitStackOverflowInit(m)

		fn := m.NewFunc("main", irtypes.I32)
		b := fn.NewBlock("entry")
		b.NewCall(initFn) // pal_stack_overflow_init() installs the handler
		// Deliberate volatile write to address 0 → SIGSEGV with si_addr == 0x0.
		// Volatile + -O0 ensures the store is emitted, not folded to a trap.
		st := b.NewStore(constant.NewInt(irtypes.I32, 0),
			constant.NewNull(irtypes.NewPointer(irtypes.I32)))
		st.Volatile = true
		b.NewRet(constant.NewInt(irtypes.I32, 0))
		return m
	})
	// Handler ends in _exit(2); the process must not die by raw signal.
	if exit != 2 {
		t.Errorf("expected exit code 2 (handler _exit(2)), got %d (stderr=%q stdout=%q)", exit, stderr, stdout)
	}
	if !strings.Contains(stderr, "fatal: segmentation fault at 0x") {
		t.Errorf("expected fault-address message in stderr, got: %q", stderr)
	}
	// A null write has si_addr == 0, so the formatted address is all zeros. This
	// is the load-bearing assertion: a wrong si_addr offset would print garbage
	// (the next siginfo field) instead of 0x0000000000000000.
	if !strings.Contains(stderr, "fatal: segmentation fault at 0x0000000000000000") {
		t.Errorf("expected null fault address 0x0..0 (validates si_addr offset), got: %q", stderr)
	}
}

// TestRuntimeAllocFreeRoundtrip: a normal alloc/free pair should not abort.
// This guards against the validation logic having false positives.
func TestRuntimeAllocFreeRoundtrip(t *testing.T) {
	_, stderr, exit := buildAndRunDebugAlloc(t, func() *ir.Module {
		return makeDebugAllocModule(t, func(m *ir.Module, palAlloc, palFree, palRealloc *ir.Func) {
			fn := m.NewFunc("main", irtypes.I32)
			b := fn.NewBlock("entry")
			// Several alloc/free cycles, including realloc grow + shrink.
			p1 := b.NewCall(palAlloc, constant.NewInt(irtypes.I64, 16))
			b.NewCall(palFree, p1)
			p2 := b.NewCall(palAlloc, constant.NewInt(irtypes.I64, 100))
			p3 := b.NewCall(palRealloc, p2, constant.NewInt(irtypes.I64, 1024))
			p4 := b.NewCall(palRealloc, p3, constant.NewInt(irtypes.I64, 50))
			b.NewCall(palFree, p4)
			b.NewRet(constant.NewInt(irtypes.I32, 0))
		})
	})
	if exit != 0 {
		t.Errorf("expected clean exit 0, got %d (stderr=%q)", exit, stderr)
	}
	if strings.Contains(stderr, "fatal:") {
		t.Errorf("unexpected abort message: %q", stderr)
	}
}
