package codegen

import (
	"github.com/llir/llvm/ir"
	"github.com/llir/llvm/ir/constant"
	irtypes "github.com/llir/llvm/ir/types"
	"github.com/llir/llvm/ir/value"

	"github.com/promise-language/promise/compiler/internal/types"
)

// declareMathIntrinsics declares LLVM math intrinsics used by std/math.pr extern functions.
func (c *Compiler) declareMathIntrinsics() {
	unary := []string{
		"llvm.sqrt.f64",
		"llvm.sin.f64",
		"llvm.cos.f64",
		"llvm.exp.f64",
		"llvm.log.f64",
		"llvm.fabs.f64",
		"llvm.floor.f64",
		"llvm.ceil.f64",
		"llvm.round.f64",
	}
	for _, name := range unary {
		c.funcs[name] = c.module.NewFunc(name, irtypes.Double,
			ir.NewParam("x", irtypes.Double))
	}
	// Binary: pow(f64, f64) -> f64
	c.funcs["llvm.pow.f64"] = c.module.NewFunc("llvm.pow.f64", irtypes.Double,
		ir.NewParam("x", irtypes.Double),
		ir.NewParam("y", irtypes.Double))
}

// defineMathBodies adds LLVM IR function bodies to the math extern bridge functions
// declared by std/math.pr. Each bridge extracts the raw f64 from the extern ABI
// value struct, calls the corresponding LLVM intrinsic, and packs the result.
func (c *Compiler) defineMathBodies() {
	irFuncByName := make(map[string]*ir.Func)
	for _, fn := range c.module.Funcs {
		if len(fn.Blocks) == 0 {
			irFuncByName[fn.Name()] = fn
		}
	}

	unary := []struct{ cName, intrinsic string }{
		{"promise_sqrt", "llvm.sqrt.f64"},
		{"promise_sin", "llvm.sin.f64"},
		{"promise_cos", "llvm.cos.f64"},
		{"promise_exp", "llvm.exp.f64"},
		{"promise_log", "llvm.log.f64"},
		{"promise_fabs", "llvm.fabs.f64"},
		{"promise_floor", "llvm.floor.f64"},
		{"promise_ceil", "llvm.ceil.f64"},
		{"promise_round", "llvm.round.f64"},
	}
	for _, mu := range unary {
		if fn, ok := irFuncByName[mu.cName]; ok {
			c.defineMathUnaryBody(fn, mu.intrinsic)
		}
	}
	if fn, ok := irFuncByName["promise_pow"]; ok {
		c.defineMathBinaryBody(fn, "llvm.pow.f64")
	}
}

// defineMathUnaryBody adds a body to void @promise_X(i8* %sret, i8* %x):
// extract raw f64 from x, call LLVM intrinsic, pack result via sret.
func (c *Compiler) defineMathUnaryBody(fn *ir.Func, intrinsicName string) {
	entry := fn.NewBlock(".entry")

	f64Layout := c.layouts[types.TypF64]
	valType := f64Layout.Value.LLVMType

	// Extract raw double from x (param 1)
	valPtr := entry.NewBitCast(fn.Params[1], irtypes.NewPointer(valType))
	rawPtr := entry.NewGetElementPtr(valType, valPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 2))
	raw := entry.NewLoad(irtypes.Double, rawPtr)

	// Call LLVM intrinsic
	result := entry.NewCall(c.funcs[intrinsicName], raw)

	// Pack result into f64 value struct via sret (param 0)
	c.storeMathResult(entry, fn.Params[0], result, f64Layout)
}

// defineMathBinaryBody adds a body to void @promise_pow(i8* %sret, i8* %x, i8* %y):
// extract raw f64 from both args, call LLVM intrinsic, pack result via sret.
func (c *Compiler) defineMathBinaryBody(fn *ir.Func, intrinsicName string) {
	entry := fn.NewBlock(".entry")

	f64Layout := c.layouts[types.TypF64]
	valType := f64Layout.Value.LLVMType

	// Extract raw doubles from x (param 1) and y (param 2)
	valPtrX := entry.NewBitCast(fn.Params[1], irtypes.NewPointer(valType))
	rawPtrX := entry.NewGetElementPtr(valType, valPtrX,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 2))
	rawX := entry.NewLoad(irtypes.Double, rawPtrX)

	valPtrY := entry.NewBitCast(fn.Params[2], irtypes.NewPointer(valType))
	rawPtrY := entry.NewGetElementPtr(valType, valPtrY,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 2))
	rawY := entry.NewLoad(irtypes.Double, rawPtrY)

	// Call LLVM intrinsic
	result := entry.NewCall(c.funcs[intrinsicName], rawX, rawY)

	// Pack result into f64 value struct via sret (param 0)
	c.storeMathResult(entry, fn.Params[0], result, f64Layout)
}

// storeMathResult packs a raw f64 result into a value struct and stores via sret pointer.
func (c *Compiler) storeMathResult(block *ir.Block, sretParam value.Value, result value.Value, f64Layout *TypeDeclLayout) {
	valType := f64Layout.Value.LLVMType
	instancePtrType := f64Layout.Value.Fields[1].LLVMType.(*irtypes.PointerType)

	var agg value.Value = constant.NewUndef(valType)
	agg = block.NewInsertValue(agg, constant.NewNull(irtypes.I8Ptr), 0)
	agg = block.NewInsertValue(agg, constant.NewNull(instancePtrType), 1)
	agg = block.NewInsertValue(agg, result, 2)

	sretPtr := block.NewBitCast(sretParam, irtypes.NewPointer(valType))
	block.NewStore(agg, sretPtr)
	block.NewRet(nil)
}
