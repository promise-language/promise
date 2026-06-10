package codegen

import (
	"fmt"

	"github.com/llir/llvm/ir"
	"github.com/llir/llvm/ir/constant"
	"github.com/llir/llvm/ir/enum"
	irtypes "github.com/llir/llvm/ir/types"
	"github.com/llir/llvm/ir/value"
)

// emitDebugPrint emits a call to pal_write(fd=2, msg, len) for debugging.
// Creates a private global for the message string. Each call gets a unique name.
func (c *Compiler) emitDebugPrint(blk *ir.Block, msg string) {
	data := constant.NewCharArrayFromString(msg)
	name := fmt.Sprintf(".dbg.%d", c.nextDebugID)
	c.nextDebugID++
	g := c.module.NewGlobalDef(name, data)
	g.Immutable = true
	g.Linkage = enum.LinkagePrivate
	ptr := blk.NewGetElementPtr(
		irtypes.NewArray(uint64(len(msg)), irtypes.I8), g,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
	blk.NewCall(c.palWrite, constant.NewInt(irtypes.I32, 2), ptr, constant.NewInt(irtypes.I64, int64(len(msg))))
}

// defineWindowsUsleep emits a usleep(i32 usec) → i32 wrapper using Win32 Sleep(ms).
// usleep takes microseconds; Sleep takes milliseconds. Minimum 1ms to avoid busy-spin.
func (c *Compiler) defineWindowsUsleep() *ir.Func {
	// declare void @Sleep(i32 %dwMilliseconds) nounwind
	sleepFn := c.getOrDeclareFunc("Sleep", irtypes.Void,
		ir.NewParam("dwMilliseconds", irtypes.I32))

	fn := c.module.NewFunc("usleep", irtypes.I32, ir.NewParam("usec", irtypes.I32))
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)
	entry := fn.NewBlock(".entry")

	// ms = usec / 1000; if ms < 1 then ms = 1
	ms := entry.NewUDiv(fn.Params[0], constant.NewInt(irtypes.I32, 1000))
	isZero := entry.NewICmp(enum.IPredEQ, ms, constant.NewInt(irtypes.I32, 0))
	clamped := entry.NewSelect(isZero, constant.NewInt(irtypes.I32, 1), ms)

	entry.NewCall(sleepFn, clamped)
	entry.NewRet(constant.NewInt(irtypes.I32, 0))
	return fn
}

// emitWindowsQPCNanos emits QPC/QPF calls and returns the monotonic nanosecond value.
// Does NOT emit a terminator — caller must use the returned value and terminate the block.
// Used by both defineNanotimeFunc (ret i64) and buildNanotimeExternBody (pack to sret).
func (c *Compiler) emitWindowsQPCNanos(blk *ir.Block) value.Value {
	qpc := c.getOrDeclareFunc("QueryPerformanceCounter", irtypes.I32,
		ir.NewParam("lpPerformanceCount", irtypes.NewPointer(irtypes.I64)))
	qpf := c.getOrDeclareFunc("QueryPerformanceFrequency", irtypes.I32,
		ir.NewParam("lpFrequency", irtypes.NewPointer(irtypes.I64)))

	counterPtr := blk.NewAlloca(irtypes.I64)
	freqPtr := blk.NewAlloca(irtypes.I64)

	blk.NewCall(qpc, counterPtr)
	blk.NewCall(qpf, freqPtr)

	counter := blk.NewLoad(irtypes.I64, counterPtr)
	freq := blk.NewLoad(irtypes.I64, freqPtr)

	// nanos = (counter / freq) * 1e9 + ((counter % freq) * 1e9) / freq
	// Two-step to avoid i64 overflow (counter * 1e9 overflows after ~106 days at 10MHz).
	billion := constant.NewInt(irtypes.I64, 1_000_000_000)
	wholeSec := blk.NewSDiv(counter, freq)
	wholeNanos := blk.NewMul(wholeSec, billion)
	remainder := blk.NewSRem(counter, freq)
	remScaled := blk.NewMul(remainder, billion)
	remNanos := blk.NewSDiv(remScaled, freq)
	return blk.NewAdd(wholeNanos, remNanos)
}

// buildWindowsNanotimeBody emits the body of promise_nanotime for Windows.
// Returns monotonic nanoseconds as i64 via QPC/QPF.
func (c *Compiler) buildWindowsNanotimeBody(entry *ir.Block) {
	nanos := c.emitWindowsQPCNanos(entry)
	entry.NewRet(nanos)
}

// emitWindowsEntry emits the self-contained Windows program entry: the
// `@__promise_start` crt0 (used as the lld-link `/entry:`) plus the runtime
// support symbols (`__chkstk`, `_fltused`, and the `_tls_used` TLS directory)
// that the MSVC static CRT would normally supply. This is what lets a Promise
// `.exe` link against ONLY the self-generated import libs + ucrtbase.dll, with
// no Visual Studio Build Tools / Windows SDK present (T0772).
//
// Idempotent: safe to call from both the program (wrapMainWithScheduler) and
// test-binary (GenerateTestMain) entry paths — only one runs per compile, but
// the guard prevents accidental duplicate-symbol emission.
func (c *Compiler) emitWindowsEntry(mainFn *ir.Func) {
	if c.windowsRuntimeEmitted {
		return
	}
	c.windowsRuntimeEmitted = true
	c.emitWindowsTLSSupport()
	c.emitWindowsChkstk()
	c.emitWindowsStart(mainFn)
}

// emitWindowsStart emits `@__promise_start`, the program entry point named in
// the lld-link `/entry:` flag (replacing the MSVC CRT's `mainCRTStartup`). It
// runs the minimal UCRT app-init sequence — `_configure_narrow_argv` +
// `_initialize_narrow_environment` — which parses the command line and populates
// `__argc`/`__argv`/`_environ` (the loader already initialized ucrtbase's heap +
// per-thread errno in its own DllMain, so nothing else is required). It then
// reads argc/argv via `__p___argc`/`__p___argv`, calls the scheduler-wrapped
// `@main(argc, argv)`, and exits via `pal_exit` (ExitProcess) with main's code.
// Initializing the narrow environment here is also what makes os.env work
// (pal_get_environ reads `__p__environ`).
func (c *Compiler) emitWindowsStart(mainFn *ir.Func) {
	i32 := irtypes.I32
	i8PtrPtr := irtypes.NewPointer(irtypes.I8Ptr) // char**
	i8PtrPtrPtr := irtypes.NewPointer(i8PtrPtr)   // char***
	i32Ptr := irtypes.NewPointer(i32)

	// int  _configure_narrow_argv(int mode)   — mode 1 = unexpanded (no globbing)
	// int  _initialize_narrow_environment()
	// int* __p___argc()                       — &__argc
	// char*** __p___argv()                    — &__argv
	configArgv := c.getOrDeclareFunc("_configure_narrow_argv", i32,
		ir.NewParam("mode", i32))
	initEnv := c.getOrDeclareFunc("_initialize_narrow_environment", i32)
	pArgc := c.getOrDeclareFunc("__p___argc", i32Ptr)
	pArgv := c.getOrDeclareFunc("__p___argv", i8PtrPtrPtr)

	start := c.module.NewFunc("__promise_start", irtypes.Void)
	start.FuncAttrs = append(start.FuncAttrs, enum.FuncAttrNoUnwind)
	entry := start.NewBlock(".entry")

	entry.NewCall(configArgv, constant.NewInt(i32, 1))
	entry.NewCall(initEnv)

	argcPtr := entry.NewCall(pArgc)
	argc := entry.NewLoad(i32, argcPtr)
	argvPtr := entry.NewCall(pArgv)
	argv := entry.NewLoad(i8PtrPtr, argvPtr)

	ret := entry.NewCall(mainFn, argc, argv)
	entry.NewCall(c.palExit, ret)
	entry.NewUnreachable()
}

// emitWindowsTLSSupport emits the `_tls_used` IMAGE_TLS_DIRECTORY (plus
// `_tls_index`, the `.tls` start/end markers, and the empty TLS-callback array)
// that the Windows loader needs to allocate per-thread storage for
// `__declspec(thread)` globals — the scheduler's current-G/P/M pointers and the
// panic-recovery flags. The MSVC CRT (tlssup) normally supplies these; without
// them the PE has no TLS directory and thread-locals read shared/zero storage.
// Also emits `_fltused`, the MSVC marker the linker requires once any floating
// point is used. Mirrors what `clang --target=x86_64-pc-windows-msvc` emits for
// a hand-written tlssup.c (verified byte-for-byte).
func (c *Compiler) emitWindowsTLSSupport() {
	i8 := irtypes.I8
	i32 := irtypes.I32
	i64 := irtypes.I64

	mkMarker := func(name, section string) *ir.Global {
		g := c.module.NewGlobalDef(name, constant.NewInt(i8, 0))
		g.Section = section
		g.Align = ir.Align(1)
		return g
	}
	tlsStart := mkMarker("_tls_start", ".tls")
	tlsEnd := mkMarker("_tls_end", ".tls$ZZZ")

	// TLS callback array bounds — both null (we register no TLS callbacks); the
	// loader stops at the first null entry, so this is an empty array.
	mkCallback := func(name, section string) *ir.Global {
		g := c.module.NewGlobalDef(name, constant.NewNull(irtypes.I8Ptr))
		g.Immutable = true
		g.Section = section
		g.Align = ir.Align(8)
		return g
	}
	xlA := mkCallback("__xl_a", ".CRT$XLA")
	mkCallback("__xl_z", ".CRT$XLZ")

	tlsIndex := c.module.NewGlobalDef("_tls_index", constant.NewInt(i32, 0))
	tlsIndex.Align = ir.Align(4)

	// IMAGE_TLS_DIRECTORY64: { StartAddressOfRawData, EndAddressOfRawData,
	// AddressOfIndex, AddressOfCallBacks (all u64), SizeOfZeroFill,
	// Characteristics (u32) }. lld-link roots `_tls_used` and emits the PE TLS
	// directory from it.
	tlsDirTy := irtypes.NewStruct(i64, i64, i64, i64, i32, i32)
	tlsUsed := c.module.NewGlobalDef("_tls_used", constant.NewStruct(tlsDirTy,
		constant.NewPtrToInt(tlsStart, i64),
		constant.NewPtrToInt(tlsEnd, i64),
		constant.NewPtrToInt(tlsIndex, i64),
		constant.NewPtrToInt(xlA, i64),
		constant.NewInt(i32, 0),
		constant.NewInt(i32, 0)))
	tlsUsed.Immutable = true
	tlsUsed.Section = ".rdata$T"
	tlsUsed.Align = ir.Align(4)

	fltused := c.module.NewGlobalDef("_fltused", constant.NewInt(i32, 0))
	fltused.Align = ir.Align(4)
}

// emitWindowsChkstk emits `__chkstk`, the stack-probe helper the LLVM MSVC x64
// backend calls in the prologue of any function whose frame exceeds one page
// (4 KiB). It walks the frame in 4 KiB steps touching each page so the guard
// page faults in order. compiler-rt's Windows builtins lib does NOT provide it
// and no Windows DLL exports it, so we supply the standard probe-only
// implementation (preserves RAX/RCX; the caller does `sub rsp, rax`). Emitted as
// a `naked` function with a single inline-asm body so LLVM adds no prologue that
// would violate the register/stack contract.
func (c *Compiler) emitWindowsChkstk() {
	fn := c.module.NewFunc("__chkstk", irtypes.Void)
	fn.FuncAttrs = append(fn.FuncAttrs, rawFuncAttr("naked"), enum.FuncAttrNoUnwind)
	blk := fn.NewBlock(".entry")

	// AT&T syntax. `$$` is a literal '$' (immediate prefix); real \n/\t are
	// escaped to \0A/\09 by the IR string quoter. Labels 1/2 are local.
	asmStr := "push %rcx\n\t" +
		"push %rax\n\t" +
		"cmp $$0x1000, %rax\n\t" +
		"lea 0x18(%rsp), %rcx\n\t" +
		"jb 2f\n" +
		"1:\n\t" +
		"sub $$0x1000, %rcx\n\t" +
		"orq $$0, (%rcx)\n\t" +
		"sub $$0x1000, %rax\n\t" +
		"cmp $$0x1000, %rax\n\t" +
		"ja 1b\n" +
		"2:\n\t" +
		"sub %rax, %rcx\n\t" +
		"orq $$0, (%rcx)\n\t" +
		"pop %rax\n\t" +
		"pop %rcx\n\t" +
		"ret"
	asmFn := ir.NewInlineAsm(irtypes.NewPointer(irtypes.NewFunc(irtypes.Void)), asmStr, "")
	asmFn.SideEffect = true
	blk.NewCall(asmFn)
	blk.NewUnreachable()
}

// buildWindowsSleepNanosBody emits the body of promise_sleep_nanos for Windows.
// Converts nanoseconds to milliseconds and calls Win32 Sleep.
func (c *Compiler) buildWindowsSleepNanosBody(entry *ir.Block, ns value.Value) {
	// declare void @Sleep(i32 %dwMilliseconds) nounwind
	sleepFn := c.getOrDeclareFunc("Sleep", irtypes.Void,
		ir.NewParam("dwMilliseconds", irtypes.I32))

	// ms = ns / 1_000_000; clamp to at least 1
	million := constant.NewInt(irtypes.I64, 1_000_000)
	ms64 := entry.NewSDiv(ns, million)
	ms32 := entry.NewTrunc(ms64, irtypes.I32)
	isZero := entry.NewICmp(enum.IPredSLE, ms32, constant.NewInt(irtypes.I32, 0))
	clamped := entry.NewSelect(isZero, constant.NewInt(irtypes.I32, 1), ms32)

	entry.NewCall(sleepFn, clamped)
	entry.NewRet(nil)
}
