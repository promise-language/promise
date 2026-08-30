package codegen

import (
	"github.com/llir/llvm/ir"
	"github.com/llir/llvm/ir/constant"
	"github.com/llir/llvm/ir/enum"
	irtypes "github.com/llir/llvm/ir/types"
	"github.com/llir/llvm/ir/value"

	"github.com/promise-language/promise/compiler/internal/codegen/pal"
	"github.com/promise-language/promise/compiler/internal/sema"
)

// declareIntrinsics declares compiler-intrinsic runtime functions (not user-declared externs).
func (c *Compiler) declareIntrinsics() {
	panicFn := c.module.NewFunc("promise_panic",
		irtypes.Void, ir.NewParam("msg", irtypes.I8Ptr))
	panicFn.FuncAttrs = append(panicFn.FuncAttrs, enum.FuncAttrNoUnwind)
	c.funcs["promise_panic"] = panicFn

	// promise_panic_at: panic with error message + source location (T0142)
	panicAtFn := c.module.NewFunc("promise_panic_at", irtypes.Void,
		ir.NewParam("msg_data", irtypes.I8Ptr),
		ir.NewParam("msg_len", irtypes.I64),
		ir.NewParam("filename", irtypes.I8Ptr),
		ir.NewParam("filename_len", irtypes.I64),
		ir.NewParam("line", irtypes.I32))
	panicAtFn.FuncAttrs = append(panicAtFn.FuncAttrs, enum.FuncAttrNoUnwind)
	c.funcs["promise_panic_at"] = panicAtFn

	// PAL: emit platform-specific allocator primitives (needed by string/vector funcs below)
	p := pal.ForTarget(c.module.TargetTriple)
	if c.debugAllocator {
		switch pp := p.(type) {
		case *pal.PosixPAL:
			pp.DebugAllocator = true
			pp.MemoryLimitAccounting = c.memoryLimitAccounting
		case *pal.WindowsPAL:
			pp.DebugAllocator = true
			pp.MemoryLimitAccounting = c.memoryLimitAccounting
		case *pal.WasmPAL:
			pp.DebugAllocator = true
			pp.MemoryLimitAccounting = c.memoryLimitAccounting
		case *pal.WasmWebPAL:
			pp.DebugAllocator = true
			pp.MemoryLimitAccounting = c.memoryLimitAccounting
		}
	}
	c.palAlloc = p.EmitAlloc(c.module)
	c.palFree = p.EmitFree(c.module)
	c.palRealloc = p.EmitRealloc(c.module)
	// T0689: emit memory-limit helpers when accounting is enabled. These are
	// only referenced by GenerateTestMain (per-test set_test_state calls) when
	// memoryLimitAccounting is on; non-test builds never emit these symbols.
	// Atomic only on multi-threaded targets — WASM (both wasi and web) is
	// single-threaded and uses plain load/store, matching what the existing
	// __promise_alloc_count tracking already does on WASM.
	if c.memoryLimitAccounting {
		pal.EmitMemoryLimitHelpers(c.module, !c.isWasm)
	}

	// PAL: emit threading primitives (Phase 5 — needed by go/receive codegen)
	c.palThreadCreate = p.EmitThreadCreate(c.module)
	c.palThreadJoin = p.EmitThreadJoin(c.module)
	c.palMutexInit = p.EmitMutexInit(c.module)
	c.palMutexLock = p.EmitMutexLock(c.module)
	c.palMutexUnlock = p.EmitMutexUnlock(c.module)
	c.palMutexDestroy = p.EmitMutexDestroy(c.module)
	c.palCondInit = p.EmitCondInit(c.module)
	c.palCondWait = p.EmitCondWait(c.module)
	c.palCondSignal = p.EmitCondSignal(c.module)
	c.palCondBroadcast = p.EmitCondBroadcast(c.module)
	c.palCondDestroy = p.EmitCondDestroy(c.module)

	// usleep — brief polling delays in thread-blocking mode
	// On WASM: stub. On Windows: Win32 Sleep(ms). On POSIX: usleep(us).
	if c.isWasm {
		c.palUsleep = c.defineWasmUsleep()
	} else if c.isWindows {
		c.palUsleep = c.defineWindowsUsleep()
	} else {
		c.palUsleep = c.module.NewFunc("usleep", irtypes.I32, ir.NewParam("usec", irtypes.I32))
		c.palUsleep.FuncAttrs = append(c.palUsleep.FuncAttrs, enum.FuncAttrNoUnwind)
	}

	// PAL: scheduler primitives (Phase 5c)
	c.palNumCPUs = p.EmitNumCPUs(c.module)

	// LLVM memcpy/memmove intrinsics (used instead of libc memcpy/memmove)
	c.funcs["llvm.memcpy"] = c.module.NewFunc("llvm.memcpy.p0i8.p0i8.i64",
		irtypes.Void,
		ir.NewParam("dest", irtypes.I8Ptr),
		ir.NewParam("src", irtypes.I8Ptr),
		ir.NewParam("len", irtypes.I64),
		ir.NewParam("isvolatile", irtypes.I1))
	c.funcs["llvm.memmove"] = c.module.NewFunc("llvm.memmove.p0i8.p0i8.i64",
		irtypes.Void,
		ir.NewParam("dest", irtypes.I8Ptr),
		ir.NewParam("src", irtypes.I8Ptr),
		ir.NewParam("len", irtypes.I64),
		ir.NewParam("isvolatile", irtypes.I1))

	// String new/concat/drop (codegen-emitted LLVM IR, replaces C runtime)
	c.defineStringNewFunc()
	c.defineStringConcatFunc()
	c.defineStringDropFunc()

	// Memcmp — used by string equality, vector contains, and string split
	if c.isWasm {
		c.funcs["memcmp"] = c.defineWasmMemcmp()
	} else {
		memcmpS1 := ir.NewParam("s1", irtypes.I8Ptr)
		memcmpS1.Attrs = append(memcmpS1.Attrs, enum.ParamAttrNoCapture, enum.ParamAttrNoUndef)
		memcmpS2 := ir.NewParam("s2", irtypes.I8Ptr)
		memcmpS2.Attrs = append(memcmpS2.Attrs, enum.ParamAttrNoCapture, enum.ParamAttrNoUndef)
		memcmpN := ir.NewParam("n", irtypes.I64)
		memcmpN.Attrs = append(memcmpN.Attrs, enum.ParamAttrNoUndef)
		memcmpFn := c.module.NewFunc("memcmp", irtypes.I32, memcmpS1, memcmpS2, memcmpN)
		memcmpFn.FuncAttrs = append(memcmpFn.FuncAttrs,
			enum.FuncAttrMustProgress, enum.FuncAttrNoUnwind,
			enum.FuncAttrReadOnly, enum.FuncAttrWillReturn, enum.FuncAttrArgMemOnly)
		c.funcs["memcmp"] = memcmpFn
	}

	// String direct equality and comparison (codegen-emitted LLVM IR)
	c.defineStringDirectEqFunc()
	c.defineStringCompareFunc()

	// Vector methods (codegen-emitted LLVM IR, replaces C runtime)
	c.defineVectorWithCapacityFunc()
	c.defineVectorPushFunc()
	c.defineVectorPopFunc()
	c.defineVectorContainsFunc()
	c.defineVectorRemoveFunc()
	c.defineVectorDropFunc()
	// T0663: Channel[T].drop is created lazily per element type by
	// getOrCreateChannelDrop (mirrors Arc/Mutex/Task) — no eager definition.
	// T0285: MutexGuard close/drop moved after scheduler functions (needs waiter_dequeue + sched_enqueue)
	c.defineVectorCOWFunc()

	// String trim/split/case/repeat (codegen-emitted LLVM IR)
	c.defineStringTrimFunc()
	c.defineStringSplitFunc()
	c.defineStringToUpperFunc()
	c.defineStringToLowerFunc()
	c.defineStringRepeatFunc()

	// Value-to-string conversion (codegen-emitted LLVM IR, replaces C runtime)
	c.defineBoolToStringFunc()
	c.defineIntToStringFunc()
	c.defineUintToStringFunc()
	c.declareF64ToStringFunc() // stub — body bridged to Promise _f64_to_str after declareFuncs
	c.declareF32ToStringFunc() // stub — body bridged to Promise _f32_to_str after declareFuncs
	c.defineCharToStringFunc()

	// String next_char UTF-8 decoder (codegen-emitted LLVM IR, replaces C runtime)
	c.defineStringNextCharFunc()

	// RTTI type check (codegen-emitted LLVM IR, replaces C runtime)
	c.defineTypeIsFunc()

	// String hash function (codegen-emitted LLVM IR, replaces C runtime)
	c.defineStringHashFunc()

	// String equality comparison (codegen-emitted LLVM IR, replaces C runtime)
	c.defineStringEqFunc()

	// Channel constructor (codegen-emitted LLVM IR)
	c.defineChannelNewFunc()

	// LLVM coroutine intrinsics (Phase 5c — M:N scheduler)
	c.declareCoroIntrinsics()

	// PAL: emit write/exit early — needed by stack overflow handler below
	c.palWrite = p.EmitWrite(c.module)
	c.palExit = p.EmitExit(c.module)

	// Stack overflow detection (B0010) — must be emitted before scheduler
	// functions because defineSchedLoopFunc references palStackOverflowThreadInit.
	c.palStackOverflowInit = p.EmitStackOverflowInit(c.module)
	c.palStackOverflowThreadInit = p.EmitStackOverflowThreadInit(c.module)

	// Scheduler globals and functions (Phase 5c)
	c.defineSchedulerGlobals()
	c.defineGNewFunc()
	c.defineI64MaxFunc()
	c.defineLocalEnqueueFunc()
	c.defineLocalDequeueFunc()
	c.defineStealWorkFunc()
	c.defineSchedWakeMFunc()
	c.defineExitSyscallFunc()
	c.defineSchedFindRunnableFunc()
	c.defineSchedEnqueueFunc()
	c.defineGoroutineExitFunc()
	c.defineSchedParkMFunc()
	// T1685: park_spare must precede sched_loop (referenced from its loop top);
	// startm must precede enter_syscall (which hands off P via startm) and follow
	// sched_loop (whose function pointer startm passes to pal_thread_create).
	c.defineSchedParkSpareFunc()
	c.defineSchedLoopFunc()
	c.defineSchedStartMFunc()
	c.defineEnterSyscallFunc()
	c.defineSysmonFunc()
	c.defineSchedInitFunc()
	c.defineSchedRunUntilMainFunc()
	if c.isWasm {
		c.defineSchedCoopStepFunc() // T0668: must precede coop_run (and Task[T].drop)
		c.defineSchedCoopRunFunc()
	}
	c.defineSchedShutdownFunc()
	c.defineWaiterEnqueueFunc()
	c.defineWaiterDequeueFunc()
	c.defineSelectWaiterEnqueueFunc()
	c.defineSelectTryWakeFunc()
	c.defineWaiterWakeOneFunc()
	c.defineWaiterWakeAllFunc()
	c.defineWaiterRemoveFunc()

	// T0285: MutexGuard close/drop need waiter_dequeue + sched_enqueue from above
	c.defineMutexGuardCloseFunc() // T0156: MutexGuard close (unlock + free)
	c.defineMutexGuardDropFunc()  // T0156: MutexGuard drop (unlock + free)

	// PAL: emit file I/O primitives (Phase D)
	c.palFileOpen = p.EmitFileOpen(c.module)
	c.palFileRead = p.EmitFileRead(c.module)
	c.palFileWrite = p.EmitFileWrite(c.module)
	c.palFileClose = p.EmitFileClose(c.module)
	c.palPipeRead = p.EmitPipeRead(c.module)
	c.palPipeWrite = p.EmitPipeWrite(c.module)
	c.palPipeClose = p.EmitPipeClose(c.module)
	c.palFileSeek = p.EmitFileSeek(c.module)
	c.palFileStatSize = p.EmitFileStatSize(c.module)
	c.palFileRemove = p.EmitFileRemove(c.module)
	c.palFileExists = p.EmitFileExists(c.module)
	c.palFileMkdir = p.EmitFileMkdir(c.module)
	c.palDirRemove = p.EmitDirRemove(c.module)
	c.palDirExists = p.EmitDirExists(c.module)
	c.palErrno = p.EmitErrno(c.module)
	c.palFileStat = p.EmitFileStat(c.module)
	c.palDirOpen = p.EmitDirOpen(c.module)
	c.palDirNextName = p.EmitDirNextName(c.module)
	c.palDirClose = p.EmitDirClose(c.module)
	c.palGetEnv = p.EmitGetEnv(c.module)
	c.palGetCwd = p.EmitGetCwd(c.module)
	c.palSetEnv = p.EmitSetEnv(c.module)
	c.palUnsetEnv = p.EmitUnsetEnv(c.module)
	c.palChdir = p.EmitChdir(c.module)
	c.palSpawn = p.EmitSpawn(c.module)
	c.palReadPipe = p.EmitReadPipe(c.module)
	c.palWaitPid = p.EmitWaitPid(c.module)
	c.palSpawnStreaming = p.EmitSpawnStreaming(c.module)
	c.palSpawnEnv = p.EmitSpawnEnv(c.module)
	c.palSpawnStreamingEnv = p.EmitSpawnStreamingEnv(c.module)
	c.palKill = p.EmitKill(c.module)
	c.palProcessAlive = p.EmitProcessAlive(c.module)
	c.palProcessStartTime = p.EmitProcessStartTime(c.module)
	c.palKillGroup = p.EmitKillGroup(c.module)
	c.palSpawnJobHandle = p.EmitSpawnJobHandle(c.module)
	c.palExecReplace = p.EmitExecReplace(c.module)
	c.palGetEnviron = p.EmitGetEnviron(c.module)
	c.palGetUserInfo = p.EmitGetUserInfo(c.module)
	c.palGetHostname = p.EmitGetHostname(c.module)
	c.palSignalInit = p.EmitSignalInit(c.module)
	c.palSignalRegister = p.EmitSignalRegister(c.module)

	// PAL socket primitives (T0069) are NOT emitted here — they are emitted
	// lazily in defineNetPALBodies() only when the net module is imported.
	// This avoids libc name collisions (connect, shutdown, send, recv, bind,
	// listen, accept) with user-defined Promise functions.

	// WASM: emit __multi3 (128-bit multiply) — LLVM may lower 64-bit multiply
	// chains into __multi3 calls, which wasm32 doesn't natively provide.
	// Also emit the 128-bit division/remainder builtins (__udivti3, __umodti3,
	// __divti3, __modti3) that i128/u128 `/` and `%` lower to — wasm32 has no
	// compiler-rt linked, so these must be provided in-IR (T0587). LLVM's
	// ExpandLargeDivRem pass inline-expands >128-bit div/rem, so only the
	// 128-bit width needs a libcall.
	if c.isWasm {
		c.emitMulti3()
	}
	// __udivti3/__umodti3/__divti3/__modti3 (128-bit div/rem builtins) are
	// emitted on EVERY target, not just wasm (T1399). Emitted with external
	// linkage in the main IR (declared extern in split module/instance .bc
	// files).
	//
	// The original justification — "the linux target statically links musl with
	// no compiler-rt/libgcc, so these are otherwise unresolved" — no longer
	// holds: T1676 put a real musl-built compiler-rt builtins archive on every
	// Linux musl link line (it had to, for aarch64's soft-float binary128 and
	// LSE outline-atomics helpers, which cannot be honestly defined in-IR). But
	// wasm still links no compiler runtime at all, so these definitions are
	// still required there, and keeping them unconditional is harmless
	// everywhere else: a strong definition simply satisfies the reference
	// before the archive member is consulted, so the archive's copy is never
	// pulled — no duplicate-symbol error. Same reasoning as the glibc/-lgcc
	// dynamic path.
	c.emitDivTi3()

	// Signal pipe read fd global (NOT TLS — dispatch goroutine reads from it)
	c.signalPipeRdFd = c.module.NewGlobal("__promise_signal_pipe_rd", irtypes.I32)
	c.signalPipeRdFd.Init = constant.NewInt(irtypes.I32, -1)

	// Command-line argument globals — populated from main's argc/argv
	c.argcGlobal = c.module.NewGlobalDef("__promise_argc", constant.NewInt(irtypes.I32, 0))
	c.argvGlobal = c.module.NewGlobalDef("__promise_argv", constant.NewNull(irtypes.NewPointer(irtypes.I8Ptr)))

	// Spawn result TLS globals — cached between _os_spawn and _os_spawn_stdout_fd/stderr_fd
	c.spawnStdoutFd = c.module.NewGlobal("__promise_spawn_stdout_fd", irtypes.I32)
	c.spawnStdoutFd.Init = constant.NewInt(irtypes.I32, -1)
	c.spawnStderrFd = c.module.NewGlobal("__promise_spawn_stderr_fd", irtypes.I32)
	c.spawnStderrFd.Init = constant.NewInt(irtypes.I32, -1)
	c.spawnStdinFd = c.module.NewGlobal("__promise_spawn_stdin_fd", irtypes.I32)
	c.spawnStdinFd.Init = constant.NewInt(irtypes.I32, -1)
	if !c.isWasm {
		c.spawnStdoutFd.TLSModel = enum.TLSModelGeneric
		c.spawnStderrFd.TLSModel = enum.TLSModelGeneric
		c.spawnStdinFd.TLSModel = enum.TLSModelGeneric
	}

	// strlen — needed by definePanicBody to get C string length.
	// May already be declared by PAL (e.g., Windows EmitDirOpen), so check first.
	if c.isWasm {
		c.funcs["strlen"] = c.defineWasmStrlen()
	} else {
		// Check if already declared (e.g., by Windows PAL EmitDirOpen)
		var strlenFn *ir.Func
		for _, f := range c.module.Funcs {
			if f.Name() == "strlen" {
				strlenFn = f
				break
			}
		}
		if strlenFn == nil {
			strlenFn = c.module.NewFunc("strlen", irtypes.I64,
				ir.NewParam("s", irtypes.I8Ptr))
			strlenFn.FuncAttrs = append(strlenFn.FuncAttrs,
				enum.FuncAttrNoUnwind, enum.FuncAttrReadOnly, enum.FuncAttrWillReturn)
		}
		c.funcs["strlen"] = strlenFn
	}

}

// emitMulti3 emits __multi3 for WASM — 128-bit integer multiply using 64-bit ops.
// LLVM may lower 64-bit multiply optimizations into __multi3 calls on wasm32.
// Signature: __multi3(i128 %a, i128 %b) -> i128
// Implementation: split into 64-bit halves, compute lower product via 32-bit
// decomposition (to avoid recursive __multi3 calls), add cross products.
func (c *Compiler) emitMulti3() {
	i128 := irtypes.I128
	fn := c.module.NewFunc("__multi3", i128,
		ir.NewParam("a", i128),
		ir.NewParam("b", i128))

	entry := fn.NewBlock(".entry")
	mask32 := constant.NewInt(irtypes.I64, 0xFFFFFFFF)

	// Split a into lo/hi (i64)
	aLo := entry.NewTrunc(fn.Params[0], irtypes.I64)
	aShr := entry.NewLShr(fn.Params[0], constant.NewInt(i128, 64))
	aHi := entry.NewTrunc(aShr, irtypes.I64)

	// Split b into lo/hi (i64)
	bLo := entry.NewTrunc(fn.Params[1], irtypes.I64)
	bShr := entry.NewLShr(fn.Params[1], constant.NewInt(i128, 64))
	bHi := entry.NewTrunc(bShr, irtypes.I64)

	// Compute aLo * bLo as 128 bits using 32-bit decomposition (no i128 mul!)
	// Split aLo = (a1 << 32) | a0, bLo = (b1 << 32) | b0
	a0 := entry.NewAnd(aLo, mask32)
	a1 := entry.NewLShr(aLo, constant.NewInt(irtypes.I64, 32))
	b0 := entry.NewAnd(bLo, mask32)
	b1 := entry.NewLShr(bLo, constant.NewInt(irtypes.I64, 32))

	// Four 32x32 -> 64 bit products (all fit in i64)
	p00 := entry.NewMul(a0, b0)
	p01 := entry.NewMul(a0, b1)
	p10 := entry.NewMul(a1, b0)
	p11 := entry.NewMul(a1, b1)

	// Combine into 128-bit result: loLo (bits 0-63), carry (bits 64-127)
	// mid = (p00 >> 32) + (p01 & mask) + (p10 & mask)
	p00hi := entry.NewLShr(p00, constant.NewInt(irtypes.I64, 32))
	p01lo := entry.NewAnd(p01, mask32)
	p10lo := entry.NewAnd(p10, mask32)
	mid := entry.NewAdd(p00hi, p01lo)
	mid = entry.NewAdd(mid, p10lo)

	// resultLo = ((mid & mask) << 32) | (p00 & mask)
	midLo := entry.NewAnd(mid, mask32)
	midLoShifted := entry.NewShl(midLo, constant.NewInt(irtypes.I64, 32))
	p00lo := entry.NewAnd(p00, mask32)
	resultLo := entry.NewOr(midLoShifted, p00lo)

	// carry = p11 + (p01 >> 32) + (p10 >> 32) + (mid >> 32)
	p01hi := entry.NewLShr(p01, constant.NewInt(irtypes.I64, 32))
	p10hi := entry.NewLShr(p10, constant.NewInt(irtypes.I64, 32))
	midHi := entry.NewLShr(mid, constant.NewInt(irtypes.I64, 32))
	carry := entry.NewAdd(p11, p01hi)
	carry2 := entry.NewAdd(carry, p10hi)
	carry3 := entry.NewAdd(carry2, midHi)

	// resultHi = carry + aLo*bHi + aHi*bLo (i64 mul, lower 64 bits only)
	cross1 := entry.NewMul(aLo, bHi)
	cross2 := entry.NewMul(aHi, bLo)
	rh1 := entry.NewAdd(carry3, cross1)
	resultHi := entry.NewAdd(rh1, cross2)

	// Combine into i128: (resultHi << 64) | resultLo
	// Using i128 zext + shl + or (no multiply, so no __multi3 recursion)
	resultLoWide := entry.NewZExt(resultLo, i128)
	resultHiWide := entry.NewZExt(resultHi, i128)
	hiShifted := entry.NewShl(resultHiWide, constant.NewInt(i128, 64))
	result := entry.NewOr(hiShifted, resultLoWide)

	entry.NewRet(result)
}

// win64IndirectI128Libcall reports whether the target lowers 128-bit integer
// libcalls (__udivti3 and friends) with the Win64 convention: both operands
// passed INDIRECTLY by pointer in rcx/rdx, and the 128-bit result returned as
// <2 x i64> in xmm0 — not the by-value i128 register pairs every other target
// uses. Only x86-64 Windows does this; aarch64-pc-windows-msvc passes i128 by
// value in x0:x1 / x2:x3 exactly like SysV (T1414).
func (c *Compiler) win64IndirectI128Libcall() bool {
	ti := sema.ParseTargetInfo(c.target)
	return ti.OS == "windows" && ti.Arch == "x86_64"
}

// emitDivTi3 emits the 128-bit integer division/remainder builtins:
// __udivti3 (unsigned quotient), __umodti3 (unsigned remainder), __divti3
// (signed quotient), __modti3 (signed remainder). LLVM lowers i128/u128 `/`
// and `%` to these libcalls, which wasm32 does not provide (no compiler-rt)
// and which the static-musl linux target does not link either (T0587, T1399).
//
// Core routine is an internal shift-subtract long-division helper returning
// {quotient, remainder}; the four public builtins wrap it. Division by zero is
// undefined (as elsewhere in Promise) — the loop simply produces an all-ones
// quotient rather than trapping.
//
// The builtin *bodies* are target-independent; only the wrapper *signature*
// varies, because it must match the calling convention LLVM's backend uses when
// it emits the libcall. See win64IndirectI128Libcall — on x86-64 Windows the
// operands arrive by pointer and the result is returned as <2 x i64> in xmm0,
// so a by-value `i128` definition silently reads the pointers as operands and
// returns into registers the caller never reads (T1414).
func (c *Compiler) emitDivTi3() {
	i128 := irtypes.I128
	udivmod := c.emitUdivmod128()

	emit := func(name string, build func(e *ir.Block, a, b value.Value) value.Value) {
		if c.win64IndirectI128Libcall() {
			vecTy := irtypes.NewVector(2, irtypes.I64)
			ptrTy := irtypes.NewPointer(i128)
			fn := c.module.NewFunc(name, vecTy,
				ir.NewParam("pa", ptrTy), ir.NewParam("pb", ptrTy))
			e := fn.NewBlock(".entry")
			a := e.NewLoad(i128, fn.Params[0])
			b := e.NewLoad(i128, fn.Params[1])
			e.NewRet(e.NewBitCast(build(e, a, b), vecTy))
			return
		}
		fn := c.module.NewFunc(name, i128,
			ir.NewParam("a", i128), ir.NewParam("b", i128))
		e := fn.NewBlock(".entry")
		e.NewRet(build(e, fn.Params[0], fn.Params[1]))
	}

	// __udivti3(a, b) -> a / b   (unsigned)
	emit("__udivti3", func(e *ir.Block, a, b value.Value) value.Value {
		return e.NewExtractValue(e.NewCall(udivmod, a, b), 0)
	})

	// __umodti3(a, b) -> a % b   (unsigned)
	emit("__umodti3", func(e *ir.Block, a, b value.Value) value.Value {
		return e.NewExtractValue(e.NewCall(udivmod, a, b), 1)
	})

	// magnitude emits |v| plus v's sign mask (0 for non-negative, -1 for
	// negative) branchlessly: |v| == (v ^ mask) - mask.
	magnitude := func(e *ir.Block, v value.Value) (abs, sign value.Value) {
		sign = e.NewAShr(v, constant.NewInt(i128, 127)) // 0 or -1
		return e.NewSub(e.NewXor(v, sign), sign), sign
	}

	// __divti3(a, b) -> a / b   (signed): divide magnitudes, apply xor of signs.
	emit("__divti3", func(e *ir.Block, a, b value.Value) value.Value {
		absA, sa := magnitude(e, a)
		absB, sb := magnitude(e, b)
		q := e.NewExtractValue(e.NewCall(udivmod, absA, absB), 0)
		sign := e.NewXor(sa, sb)
		return e.NewSub(e.NewXor(q, sign), sign)
	})

	// __modti3(a, b) -> a % b   (signed): remainder takes the dividend's sign.
	emit("__modti3", func(e *ir.Block, a, b value.Value) value.Value {
		absA, sa := magnitude(e, a)
		absB, _ := magnitude(e, b)
		r := e.NewExtractValue(e.NewCall(udivmod, absA, absB), 1)
		return e.NewSub(e.NewXor(r, sa), sa) // apply dividend sign
	})
}

// emitUdivmod128 emits an internal helper computing unsigned 128-bit
// division and remainder together via a shift-subtract loop, returning
// {quotient, remainder}. Used by the __*ti3 builtins above.
func (c *Compiler) emitUdivmod128() *ir.Func {
	i128 := irtypes.I128
	retTy := irtypes.NewStruct(i128, i128)
	fn := c.module.NewFunc("__promise_udivmod128", retTy,
		ir.NewParam("a", i128), ir.NewParam("b", i128))
	// External linkage (not internal): the main IR holds the definition; split
	// module/instance .bc files reference it as an extern declaration resolved
	// at link time — same pattern as __multi3. Internal linkage would strip to
	// an invalid `declare internal` in those split IRs (T0587).
	a, b := fn.Params[0], fn.Params[1]

	entry := fn.NewBlock(".entry")
	head := fn.NewBlock(".head")
	body := fn.NewBlock(".body")
	exit := fn.NewBlock(".exit")

	entry.NewBr(head)

	// Classic MSB-first long division. Shift the combined {rem:num} pair left by
	// one bit per iteration, pulling the top bit of `num` into `rem`, and shift
	// the quotient left, setting its low bit when rem >= b. All shifts are by a
	// COMPILE-TIME-CONSTANT amount (1 or 127) so llc lowers them inline on wasm —
	// avoiding the variable-shift libcalls (__lshrti3/__ashlti3) that wasm lacks.
	one := constant.NewInt(i128, 1)
	c127 := constant.NewInt(i128, 127)
	iPhi := head.NewPhi(ir.NewIncoming(constant.NewInt(irtypes.I32, 0), entry))
	qPhi := head.NewPhi(ir.NewIncoming(constant.NewInt(i128, 0), entry))
	rPhi := head.NewPhi(ir.NewIncoming(constant.NewInt(i128, 0), entry))
	nPhi := head.NewPhi(ir.NewIncoming(a, entry))
	cond := head.NewICmp(enum.IPredSLT, iPhi, constant.NewInt(irtypes.I32, 128))
	head.NewCondBr(cond, body, exit)

	// rem = (rem << 1) | (num >> 127); num <<= 1; quo <<= 1
	topBit := body.NewLShr(nPhi, c127)
	rShift := body.NewOr(body.NewShl(rPhi, one), topBit)
	nNew := body.NewShl(nPhi, one)
	qShift := body.NewShl(qPhi, one)
	// if rem >= b: rem -= b; quo |= 1
	ge := body.NewICmp(enum.IPredUGE, rShift, b)
	rNew := body.NewSelect(ge, body.NewSub(rShift, b), rShift)
	qNew := body.NewSelect(ge, body.NewOr(qShift, one), qShift)
	iNext := body.NewAdd(iPhi, constant.NewInt(irtypes.I32, 1))
	body.NewBr(head)

	iPhi.Incs = append(iPhi.Incs, ir.NewIncoming(iNext, body))
	qPhi.Incs = append(qPhi.Incs, ir.NewIncoming(qNew, body))
	rPhi.Incs = append(rPhi.Incs, ir.NewIncoming(rNew, body))
	nPhi.Incs = append(nPhi.Incs, ir.NewIncoming(nNew, body))

	agg := exit.NewInsertValue(constant.NewUndef(retTy), qPhi, 0)
	agg2 := exit.NewInsertValue(agg, rPhi, 1)
	exit.NewRet(agg2)

	return fn
}
