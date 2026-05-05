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

// callSetjmp emits a call to setjmp in the given block.
// On Windows MSVC, __intrinsic_setjmp takes (env, frame_pointer) — this helper
// passes @llvm.frameaddress(i32 0) as the second arg automatically.
// On other platforms, setjmp takes just (env).
func (c *Compiler) callSetjmp(blk *ir.Block, envPtr value.Value) *ir.InstCall {
	if c.isWindows {
		frameAddr := blk.NewCall(c.funcs["llvm.frameaddress"], constant.NewInt(irtypes.I32, 0))
		return blk.NewCall(c.funcs["setjmp"], envPtr, frameAddr)
	}
	return blk.NewCall(c.funcs["setjmp"], envPtr)
}

// defineWindowsUsleep emits a usleep(i32 usec) → i32 wrapper using Win32 Sleep(ms).
// usleep takes microseconds; Sleep takes milliseconds. Minimum 1ms to avoid busy-spin.
func (c *Compiler) defineWindowsUsleep() *ir.Func {
	// declare void @Sleep(i32 %dwMilliseconds) nounwind
	sleepFn := c.module.NewFunc("Sleep", irtypes.Void,
		ir.NewParam("dwMilliseconds", irtypes.I32))
	sleepFn.FuncAttrs = append(sleepFn.FuncAttrs, enum.FuncAttrNoUnwind)

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

// buildNanotimeBody emits the body of promise_nanotime for Windows using
// QueryPerformanceCounter and QueryPerformanceFrequency.
// Returns monotonic nanoseconds as i64.
func (c *Compiler) buildWindowsNanotimeBody(entry *ir.Block) *ir.Block {
	// declare i32 @QueryPerformanceCounter(i64*) nounwind
	qpc := c.getOrDeclareFunc("QueryPerformanceCounter", irtypes.I32,
		ir.NewParam("lpPerformanceCount", irtypes.NewPointer(irtypes.I64)))
	// declare i32 @QueryPerformanceFrequency(i64*) nounwind
	qpf := c.getOrDeclareFunc("QueryPerformanceFrequency", irtypes.I32,
		ir.NewParam("lpFrequency", irtypes.NewPointer(irtypes.I64)))

	counterPtr := entry.NewAlloca(irtypes.I64)
	freqPtr := entry.NewAlloca(irtypes.I64)

	entry.NewCall(qpc, counterPtr)
	entry.NewCall(qpf, freqPtr)

	counter := entry.NewLoad(irtypes.I64, counterPtr)
	freq := entry.NewLoad(irtypes.I64, freqPtr)

	// nanos = (counter / freq) * 1e9 + ((counter % freq) * 1e9) / freq
	// Two-step to avoid i64 overflow (counter * 1e9 overflows after ~106 days at 10MHz).
	billion := constant.NewInt(irtypes.I64, 1_000_000_000)
	wholeSec := entry.NewSDiv(counter, freq)
	wholeNanos := entry.NewMul(wholeSec, billion)
	remainder := entry.NewSRem(counter, freq)
	remScaled := entry.NewMul(remainder, billion)
	remNanos := entry.NewSDiv(remScaled, freq)
	nanos := entry.NewAdd(wholeNanos, remNanos)
	entry.NewRet(nanos)
	return entry
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
