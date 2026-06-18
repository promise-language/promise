package codegen

import (
	"github.com/llir/llvm/ir"
	"github.com/llir/llvm/ir/constant"
	irtypes "github.com/llir/llvm/ir/types"
)

// defineTimeBodies adds LLVM IR function bodies to wall-clock extern declarations
// from modules/time/time.pr. The single native primitive is promise_wallclock,
// which returns nanoseconds since the Unix epoch (1970-01-01T00:00:00Z, UTC).
//
// Must run after compileModules() so the time module extern is declared in
// c.module.Funcs. Mirrors defineOSBodies / defineNetPALBodies.
func (c *Compiler) defineTimeBodies() {
	irFuncByName := make(map[string]*ir.Func)
	for _, fn := range c.module.Funcs {
		if len(fn.Blocks) == 0 {
			irFuncByName[fn.Name()] = fn
		}
	}

	if fn, ok := irFuncByName["promise_wallclock"]; ok {
		c.buildWallclockExternBody(fn)
	}
}

// buildWallclockExternBody adds a body to extern promise_wallclock(i8* %sret).
// Computes realtime nanos since the Unix epoch and packs them into a Promise int
// value struct. Parallels buildNanotimeExternBody but reads CLOCK_REALTIME (id 0
// on both Linux and macOS) rather than the monotonic clock.
func (c *Compiler) buildWallclockExternBody(fn *ir.Func) {
	entry := fn.NewBlock(".entry")

	// WASM has no portable realtime source from emitted IR; return 0. The Promise
	// round-trip/serialization tests don't depend on the absolute value, and the
	// now() sanity assertion is gated off WASM with `test(exclude: wasm).
	if c.isWasm {
		c.packNanosToSret(entry, fn.Params[0], constant.NewInt(irtypes.I64, 0))
		return
	}

	if c.isWindows {
		nanos := c.emitWindowsWallclockNanos(entry)
		c.packNanosToSret(entry, fn.Params[0], nanos)
		return
	}

	// POSIX: clock_gettime(CLOCK_REALTIME=0, &ts); nanos = sec*1e9 + nsec.
	timespecType := irtypes.NewStruct(irtypes.I64, irtypes.I64)
	clockGettime := c.getOrDeclareFunc("clock_gettime", irtypes.I32,
		ir.NewParam("clk_id", irtypes.I32),
		ir.NewParam("tp", irtypes.NewPointer(timespecType)))

	clockRealtime := int64(0) // CLOCK_REALTIME is 0 on both Linux and macOS

	ts := entry.NewAlloca(timespecType)
	entry.NewCall(clockGettime, constant.NewInt(irtypes.I32, clockRealtime), ts)

	secPtr := entry.NewGetElementPtr(timespecType, ts,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
	nsecPtr := entry.NewGetElementPtr(timespecType, ts,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 1))
	sec := entry.NewLoad(irtypes.I64, secPtr)
	nsec := entry.NewLoad(irtypes.I64, nsecPtr)

	billion := constant.NewInt(irtypes.I64, 1_000_000_000)
	secNanos := entry.NewMul(sec, billion)
	totalNanos := entry.NewAdd(secNanos, nsec)
	c.packNanosToSret(entry, fn.Params[0], totalNanos)
}
