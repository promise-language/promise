package codegen

import (
	"github.com/llir/llvm/ir"
	irtypes "github.com/llir/llvm/ir/types"
	"github.com/llir/llvm/ir/value"

	"djabi.dev/go/promise_lang/internal/types"
)

// declareRuntime declares external C runtime functions used by compiled programs.
func (c *Compiler) declareRuntime() {
	c.runtimeFuncs = make(map[string]*ir.Func)

	c.runtimeFuncs["promise_print_int"] = c.module.NewFunc("promise_print_int",
		irtypes.Void, ir.NewParam("x", irtypes.I64))

	c.runtimeFuncs["promise_print_f64"] = c.module.NewFunc("promise_print_f64",
		irtypes.Void, ir.NewParam("x", irtypes.Double))

	c.runtimeFuncs["promise_print_bool"] = c.module.NewFunc("promise_print_bool",
		irtypes.Void, ir.NewParam("x", irtypes.I8))

	c.runtimeFuncs["promise_panic"] = c.module.NewFunc("promise_panic",
		irtypes.Void, ir.NewParam("msg", irtypes.I8Ptr))
}

// isRuntimePrintFunc checks if a function is one of the runtime print functions.
func (c *Compiler) isRuntimePrintFunc(fn *ir.Func) bool {
	return fn == c.runtimeFuncs["promise_print_int"] ||
		fn == c.runtimeFuncs["promise_print_f64"] ||
		fn == c.runtimeFuncs["promise_print_bool"]
}

// genPrintCall generates a call to the appropriate print runtime function
// based on the argument type.
func (c *Compiler) genPrintCall(argType types.Type, argVal value.Value) value.Value {
	named := extractNamed(argType)
	if named == nil {
		return nil
	}

	cat := classify(named)
	switch cat {
	case CatSignedInt, CatUnsignedInt:
		// Extend or truncate to i64 for the print function
		val := c.intToI64(argVal, cat == CatSignedInt)
		return c.block.NewCall(c.runtimeFuncs["promise_print_int"], val)
	case CatFloat:
		// Extend float to double if needed
		val := c.floatToF64(argVal)
		return c.block.NewCall(c.runtimeFuncs["promise_print_f64"], val)
	case CatBool:
		// Zero-extend i1 to i8 for the C function
		val := c.block.NewZExt(argVal, irtypes.I8)
		return c.block.NewCall(c.runtimeFuncs["promise_print_bool"], val)
	}
	return nil
}

// intToI64 converts an integer value to i64, using sign/zero extension or truncation.
func (c *Compiler) intToI64(val value.Value, signed bool) value.Value {
	valType, ok := val.Type().(*irtypes.IntType)
	if !ok || valType.BitSize == 64 {
		return val
	}
	if valType.BitSize < 64 {
		if signed {
			return c.block.NewSExt(val, irtypes.I64)
		}
		return c.block.NewZExt(val, irtypes.I64)
	}
	return c.block.NewTrunc(val, irtypes.I64)
}

// floatToF64 converts a float value to double if needed.
func (c *Compiler) floatToF64(val value.Value) value.Value {
	if val.Type() == irtypes.Double {
		return val
	}
	return c.block.NewFPExt(val, irtypes.Double)
}
