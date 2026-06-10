package codegen

import (
	"strings"
	"testing"

	"github.com/llir/llvm/ir"
	irtypes "github.com/llir/llvm/ir/types"
)

// T0772: Windows zero-dependency link surface. These tests pin the IR shape of
// the self-contained crt0 entry, the CRT-replacement runtime support symbols
// (_tls_used / __chkstk / _fltused), and the _beginthreadex → CreateThread
// switch, so the compiler keeps emitting a link surface that needs no MSVC /
// Windows SDK files.

const winTarget = "x86_64-pc-windows-msvc"

// TestWindowsCrt0Entry verifies the @__promise_start entry point performs the
// UCRT app-init sequence, reads argc/argv, calls @main, and exits.
func TestWindowsCrt0Entry(t *testing.T) {
	ir := generateIRForTarget(t, `main() { print_line("hi"); }`, winTarget)

	assertContains(t, ir, "define void @__promise_start()")
	// UCRT narrow app-init (parses command line + populates _environ).
	assertContains(t, ir, "@_configure_narrow_argv")
	assertContains(t, ir, "@_initialize_narrow_environment")
	assertContains(t, ir, "@__p___argc")
	assertContains(t, ir, "@__p___argv")
	// Calls the program entry and exits with its return code.
	assertContains(t, ir, "call i32 @main(")
	// Old MSVC CRT entry must not appear anywhere.
	assertNotContains(t, ir, "mainCRTStartup")
	assertNotContains(t, ir, "__getmainargs")
}

// TestWindowsTLSSupport verifies the loader-visible TLS directory and the
// _fltused FP marker are emitted, so __declspec(thread) globals (scheduler +
// panic flags) get per-thread storage without the MSVC CRT's tlssup.
func TestWindowsTLSSupport(t *testing.T) {
	ir := generateIRForTarget(t, `main() { print_line("hi"); }`, winTarget)

	assertContains(t, ir, "@_tls_used =")
	assertContains(t, ir, "@_tls_index =")
	assertContains(t, ir, "@_tls_start =")
	assertContains(t, ir, "@_tls_end =")
	assertContains(t, ir, "@_fltused =")
	// Section placement the Windows loader requires.
	assertContains(t, ir, `section ".tls"`)
	assertContains(t, ir, `section ".tls$ZZZ"`)
	assertContains(t, ir, `section ".CRT$XLA"`)
	assertContains(t, ir, `section ".rdata$T"`)
}

// TestWindowsChkstk verifies __chkstk is emitted as a naked inline-asm function
// (compiler-rt's Windows builtins lib does not provide it, and no Windows DLL
// exports it).
func TestWindowsChkstk(t *testing.T) {
	ir := generateIRForTarget(t, `main() { print_line("hi"); }`, winTarget)

	assertContains(t, ir, "define void @__chkstk()")
	assertContains(t, ir, "naked")
	assertContains(t, ir, "call void asm sideeffect")
}

// TestWindowsThreadCreateUsesCreateThread verifies pal_thread_create launches
// worker threads via kernel32 CreateThread (always-present DLL) rather than the
// UCRT _beginthreadex — safe because panic recovery is TLS-flag-based, not
// setjmp/longjmp (T0146).
func TestWindowsThreadCreateUsesCreateThread(t *testing.T) {
	ir := generateIRForTarget(t, `main() { go { print_line("g"); } }`, winTarget)

	assertContains(t, ir, "@CreateThread")
	assertContains(t, ir, "call i8* @pal_thread_create")
	assertNotContains(t, ir, "_beginthreadex")
}

// TestWindowsRuntimeSupportEmittedOnce guards the idempotency of
// emitWindowsEntry — the support symbols must be defined exactly once to avoid
// duplicate-symbol link errors.
func TestWindowsRuntimeSupportEmittedOnce(t *testing.T) {
	ir := generateIRForTarget(t, `main() { print_line("hi"); }`, winTarget)

	for _, sym := range []string{"@__promise_start()", "@__chkstk()", "@_tls_used =", "@_fltused ="} {
		if got := strings.Count(ir, sym); got != 1 {
			t.Errorf("expected exactly one definition of %q, got %d", sym, got)
		}
	}
}

// TestEmitWindowsEntryGuard exercises the windowsRuntimeEmitted early-return
// directly: both entry paths (wrapMainWithScheduler and GenerateTestMain) call
// emitWindowsEntry, but only the guard prevents a duplicate-symbol link error if
// both ever run. Calling it twice on the same Compiler must emit the support
// symbols exactly once.
func TestEmitWindowsEntryGuard(t *testing.T) {
	c := &Compiler{module: ir.NewModule()}
	c.palExit = c.module.NewFunc("pal_exit", irtypes.Void, ir.NewParam("code", irtypes.I32))
	mainFn := c.module.NewFunc("main", irtypes.I32,
		ir.NewParam("argc", irtypes.I32),
		ir.NewParam("argv", irtypes.NewPointer(irtypes.I8Ptr)))

	c.emitWindowsEntry(mainFn)
	if !c.windowsRuntimeEmitted {
		t.Fatal("windowsRuntimeEmitted should be set after the first call")
	}
	// Second call must hit the guard and return without re-emitting anything.
	c.emitWindowsEntry(mainFn)

	out := c.module.String()
	for _, sym := range []string{"@__promise_start()", "@__chkstk()", "@_tls_used =", "@_fltused ="} {
		if got := strings.Count(out, sym); got != 1 {
			t.Errorf("after double emitWindowsEntry: %q defined %d times, want 1", sym, got)
		}
	}
}
