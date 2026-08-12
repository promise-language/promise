package main

// Runtime crash tests for the T0365 sentinel-header debug allocator.
//
// Each test programmatically constructs an LLVM module that exercises
// pal_alloc/pal_free directly, links it into a real executable, runs it, and
// asserts that the expected abort message is written to stderr with exit code
// 134 (SIGABRT-style).
//
// Linking goes through compileAndLinkLLVM — the project's own backend driver
// (opt → llc → ld.lld against the embedded musl CRT), exactly the pipeline
// `promise build` uses. No system C compiler is involved: the toolchain is
// self-contained, so these tests run wherever `promise build` runs rather than
// only on machines that happen to have clang. That is also why they live in
// cmd/promise instead of internal/codegen/pal — compileAndLinkLLVM is here,
// and a pal-package test cannot import it without an import cycle.
//
// Skipped on Windows (PosixPAL only) and in -short mode.

import (
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/llir/llvm/ir"
	"github.com/llir/llvm/ir/constant"
	"github.com/llir/llvm/ir/enum"
	irtypes "github.com/llir/llvm/ir/types"

	"github.com/promise-language/promise/compiler/internal/codegen"
	"github.com/promise-language/promise/compiler/internal/codegen/pal"
)

// newTestMain adds an `@main` that opt must leave alone.
//
// These tests deliberately do things the optimizer is entitled to assume never
// happen — freeing twice, reading a header beside a freed block, writing past
// an allocation. compileAndLinkLLVM runs `opt -O1` (there is no -O0 mode; see
// B0314), and at -O1 LLVM knows free() ends an object's lifetime, so it folds
// the subsequent header loads to undef and the abort branch vanishes. optnone
// keeps the emitted sequence intact, which is the whole point: what is under
// test is the allocator's runtime checks, not what opt makes of the caller.
func newTestMain(m *ir.Module) *ir.Func {
	fn := m.NewFunc("main", irtypes.I32)
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoInline, enum.FuncAttrOptNone)
	return fn
}

// buildAndRunDebugAlloc emits modBuilder's IR into a temp .ll, links it into a
// binary with the project toolchain, runs it, and returns (stdout, stderr, exitCode).
func buildAndRunDebugAlloc(t *testing.T, modBuilder func() *ir.Module) (string, string, int) {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping debug allocator runtime test in -short mode")
	}
	if runtime.GOOS == "windows" {
		t.Skip("debug allocator runtime tests are POSIX-only (PosixPAL)")
	}
	requireLLVMTools(t)

	llPath := writeLL(t, modBuilder().String())
	binPath := filepath.Join(t.TempDir(), "test")
	if err := compileAndLinkLLVM(llPath, codegen.HostTargetTriple(), binPath, false); err != nil {
		t.Fatalf("compileAndLinkLLVM failed: %v", err)
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

// hostPosixPAL returns a PosixPAL configured for the exact triple the test
// binary will be linked for, so every libc-layout decision it makes (Darwin vs
// Linux, glibc vs musl, x86_64 vs aarch64) matches the binary that actually runs.
func hostPosixPAL(t *testing.T) *pal.PosixPAL {
	t.Helper()
	p, ok := pal.ForTarget(codegen.HostTargetTriple()).(*pal.PosixPAL)
	if !ok {
		t.Skipf("host target %q is not a POSIX target", codegen.HostTargetTriple())
	}
	return p
}

// makeDebugAllocModule builds a self-contained module with PAL alloc/free/realloc
// emitted in DebugAllocator mode, plus a `@main` function defined by mainBuilder.
// Returns the module ready for emission.
func makeDebugAllocModule(t *testing.T, mainBuilder func(m *ir.Module, palAlloc, palFree, palRealloc *ir.Func)) *ir.Module {
	t.Helper()
	m := ir.NewModule()
	m.TargetTriple = ""
	p := hostPosixPAL(t)
	p.DebugAllocator = true
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
//
// The extra never-freed allocation below keeps the block's slot group mapped,
// which pins this test to the in-line header check. Without it musl's mallocng
// would have already unmapped the group and the fault-handler path would answer
// instead — that case is TestRuntimeDoubleFreeUnmappedAborts. Both must report
// a double free and exit 134; they just get there differently.
func TestRuntimeDoubleFreeAborts(t *testing.T) {
	stdout, stderr, exit := buildAndRunDebugAlloc(t, func() *ir.Module {
		return makeDebugAllocModule(t, func(m *ir.Module, palAlloc, palFree, _ *ir.Func) {
			fn := newTestMain(m)
			b := fn.NewBlock("entry")
			// Same size class as p, deliberately never freed — see above.
			b.NewCall(palAlloc, constant.NewInt(irtypes.I64, 64))
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

// wantDoubleFreeMessage returns the diagnostic a lone-allocation double free
// must produce on the host allocator.
//
// The debug allocator marks a block MAGIC_FREED at header offsets 0 and 8 and
// then hands it to libc. Whether that marker is still there when the second
// pal_free reads it is entirely up to libc:
//
//   - musl (the default Linux target): mallocng unmaps a slot group the moment
//     it empties, so the header is gone and reading it faults. The thread-local
//     probe guard turns that SIGSEGV into "double free or invalid free
//     (unmapped block)" — the path this test exists to pin down.
//   - macOS libsystem_malloc: zeroes the whole block on free (a hardening
//     measure, so a use-after-free reads zeros rather than stale data). Both
//     marker slots read back as 0 — neither MAGIC_ALIVE nor MAGIC_FREED — so
//     detection lands on the bad-header-magic path. No in-band marker can
//     survive there, which is why this is asserted rather than fixed.
//
// glibc leaves enough of the block alone that "double free" itself survives,
// and is covered by the substring below.
func wantDoubleFreeMessage() string {
	if runtime.GOOS == "darwin" {
		return "invalid free (bad header magic)"
	}
	return "double free"
}

// TestRuntimeDoubleFreeUnmappedAborts is the counterpart to the test above: the
// double-freed block is the program's ONLY live allocation, so by the time the
// second pal_free runs, musl's mallocng has handed the whole slot group back to
// the OS and the header pal_free wants to inspect is unmapped.
//
// This used to be a silent death — SIGSEGV, empty stderr, no exit code of our
// own — while glibc reported the double free normally. It is the exact shape of
// the bug that "only reproduces on musl". The thread-local probe guard makes the
// handler recognize the fault as a double free instead.
//
// pal_stack_overflow_init() is called first because that is what installs the
// handler; real Promise binaries call it during runtime startup.
//
// Which of the three detection paths answers depends on what the host allocator
// does with a block it has just taken back, so the assertion below is
// per-allocator (see wantDoubleFreeMessage). All of them abort with 134 and all
// of them name a free that should not have happened; what must never happen is
// the silent death, or the fault being reported as a plain segfault.
func TestRuntimeDoubleFreeUnmappedAborts(t *testing.T) {
	stdout, stderr, exit := buildAndRunDebugAlloc(t, func() *ir.Module {
		m := ir.NewModule()
		m.TargetTriple = ""
		p := hostPosixPAL(t)
		p.DebugAllocator = true
		palAlloc := p.EmitAlloc(m)
		palFree := p.EmitFree(m)
		p.EmitRealloc(m)
		initFn := p.EmitStackOverflowInit(m)

		fn := newTestMain(m)
		b := fn.NewBlock("entry")
		b.NewCall(initFn)
		// The only allocation in the program — freeing it empties the group.
		ptr := b.NewCall(palAlloc, constant.NewInt(irtypes.I64, 64))
		b.NewCall(palFree, ptr)
		b.NewCall(palFree, ptr)
		b.NewRet(constant.NewInt(irtypes.I32, 0))
		return m
	})
	if exit != 134 {
		t.Errorf("expected exit code 134, got %d (stderr=%q stdout=%q)", exit, stderr, stdout)
	}
	if want := wantDoubleFreeMessage(); !strings.Contains(stderr, want) {
		t.Errorf("expected %q in stderr, got: stderr=%q stdout=%q", want, stderr, stdout)
	}
	// The generic handler message would mean the guard did not fire and we fell
	// through to reporting this as an ordinary segfault.
	if strings.Contains(stderr, "segmentation fault") {
		t.Errorf("double free reported as a plain segfault: %q", stderr)
	}
}

// TestRuntimeBadFreeAborts: pal_free on a stack pointer → bad header magic.
func TestRuntimeBadFreeAborts(t *testing.T) {
	stdout, stderr, exit := buildAndRunDebugAlloc(t, func() *ir.Module {
		return makeDebugAllocModule(t, func(m *ir.Module, _, palFree, _ *ir.Func) {
			fn := newTestMain(m)
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
			fn := newTestMain(m)
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
//
// The PAL is built for the host triple, so the sigaction layout it picks (glibc
// vs musl) is the one the linker actually links against — no guessing.
func TestRuntimeSigsegvHandlerPrintsFaultAddress(t *testing.T) {
	stdout, stderr, exit := buildAndRunDebugAlloc(t, func() *ir.Module {
		m := ir.NewModule()
		m.TargetTriple = ""
		p := hostPosixPAL(t)
		initFn := p.EmitStackOverflowInit(m)

		fn := newTestMain(m)
		b := fn.NewBlock("entry")
		b.NewCall(initFn) // pal_stack_overflow_init() installs the handler
		// Deliberate volatile write to address 0 → SIGSEGV with si_addr == 0x0.
		// Volatile keeps the store from being folded away by opt.
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

// TestRuntimeSigsegvNotMisreportedAfterFree is the false-positive check for the
// probe guard: a debug build that has completed a normal alloc/free must still
// report an unrelated null dereference as a segmentation fault, not as a double
// free. If pal_free ever left the guard raised on its success path, every later
// crash in the process would be blamed on the allocator.
func TestRuntimeSigsegvNotMisreportedAfterFree(t *testing.T) {
	stdout, stderr, exit := buildAndRunDebugAlloc(t, func() *ir.Module {
		m := ir.NewModule()
		m.TargetTriple = ""
		p := hostPosixPAL(t)
		p.DebugAllocator = true
		palAlloc := p.EmitAlloc(m)
		palFree := p.EmitFree(m)
		p.EmitRealloc(m)
		initFn := p.EmitStackOverflowInit(m)

		fn := newTestMain(m)
		b := fn.NewBlock("entry")
		b.NewCall(initFn)
		// A clean alloc/free pair — raises the guard, then clears it.
		ptr := b.NewCall(palAlloc, constant.NewInt(irtypes.I64, 64))
		b.NewCall(palFree, ptr)
		// Now an unrelated fault.
		st := b.NewStore(constant.NewInt(irtypes.I32, 0),
			constant.NewNull(irtypes.NewPointer(irtypes.I32)))
		st.Volatile = true
		b.NewRet(constant.NewInt(irtypes.I32, 0))
		return m
	})
	if exit != 2 {
		t.Errorf("expected exit code 2 (segfault handler), got %d (stderr=%q stdout=%q)", exit, stderr, stdout)
	}
	if !strings.Contains(stderr, "fatal: segmentation fault at 0x0000000000000000") {
		t.Errorf("expected the null fault address, got: %q", stderr)
	}
	if strings.Contains(stderr, "double free") {
		t.Errorf("unrelated segfault misreported as a double free: %q", stderr)
	}
}

// TestRuntimeAllocFreeRoundtrip: a normal alloc/free pair should not abort.
// This guards against the validation logic having false positives.
func TestRuntimeAllocFreeRoundtrip(t *testing.T) {
	_, stderr, exit := buildAndRunDebugAlloc(t, func() *ir.Module {
		return makeDebugAllocModule(t, func(m *ir.Module, palAlloc, palFree, palRealloc *ir.Func) {
			fn := newTestMain(m)
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
