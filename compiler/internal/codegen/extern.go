package codegen

import (
	"fmt"

	"github.com/llir/llvm/ir"
	"github.com/llir/llvm/ir/constant"
	irtypes "github.com/llir/llvm/ir/types"
	"github.com/llir/llvm/ir/value"

	"djabi.dev/go/promise_lang/internal/types"
)

// declareExterns creates LLVM IR function declarations for all extern functions.
// Parameters use promise_T_v struct types; return types use promise_T_v or void.
func (c *Compiler) declareExterns(externs []*ExternFunc, layouts map[*types.Named]*TypeDeclLayout) {
	for _, ext := range externs {
		var params []*ir.Param
		for i, pt := range ext.ParamTypes {
			named := extractNamed(pt)
			if named == nil {
				panic(fmt.Sprintf("codegen: cannot resolve type for extern param %d of %s", i, ext.PromiseName))
			}
			layout := layouts[named]
			if layout == nil {
				panic(fmt.Sprintf("codegen: no layout for type %s in extern %s", named, ext.PromiseName))
			}

			paramName := ext.Sig.Params()[i].Name()

			var paramType irtypes.Type
			if isRefType(pt) {
				paramType = irtypes.NewPointer(layout.Value.LLVMType)
			} else {
				paramType = layout.Value.LLVMType
			}
			params = append(params, ir.NewParam(paramName, paramType))
		}

		retType := irtypes.Type(irtypes.Void)
		if ext.ResultType != nil {
			named := extractNamed(ext.ResultType)
			if named == nil {
				panic(fmt.Sprintf("codegen: cannot resolve return type for extern %s", ext.PromiseName))
			}
			layout := layouts[named]
			if layout == nil {
				panic(fmt.Sprintf("codegen: no layout for return type %s in extern %s", named, ext.PromiseName))
			}
			retType = layout.Value.LLVMType
		}

		fn := c.module.NewFunc(ext.CName, retType, params...)
		ext.IRFunc = fn
		c.funcs[ext.PromiseName] = fn
	}
}

// genExternCall generates an extern function call with ABI coercion.
// It packs each argument into a promise_T_v struct, calls the function,
// and unpacks the return value back to internal representation.
func (c *Compiler) genExternCall(ext *ExternFunc, argVals []value.Value, argTypes []types.Type) value.Value {
	coercedArgs := make([]value.Value, len(argVals))
	for i, arg := range argVals {
		named := extractNamed(argTypes[i])
		if named == nil {
			panic(fmt.Sprintf("codegen: cannot resolve type for arg %d in call to %s", i, ext.PromiseName))
		}
		layout := c.layouts[named]
		if layout == nil {
			panic(fmt.Sprintf("codegen: no layout for type %s in call to %s", named, ext.PromiseName))
		}
		coercedArgs[i] = c.packToValueStruct(arg, named, layout)
	}

	result := c.block.NewCall(ext.IRFunc, coercedArgs...)

	if ext.ResultType != nil {
		named := extractNamed(ext.ResultType)
		if named == nil {
			panic(fmt.Sprintf("codegen: cannot resolve return type in call to %s", ext.PromiseName))
		}
		layout := c.layouts[named]
		if layout == nil {
			panic(fmt.Sprintf("codegen: no layout for return type %s in call to %s", named, ext.PromiseName))
		}
		return c.unpackFromValueStruct(result, named, layout)
	}
	return result
}

// packToValueStruct packs a Promise internal value into a promise_T_v struct.
func (c *Compiler) packToValueStruct(val value.Value, named *types.Named, layout *TypeDeclLayout) value.Value {
	if layout.Kind == LayoutPrimitive {
		return c.packPrimitive(val, named, layout)
	}
	panic(fmt.Sprintf("codegen: packing non-primitive type %s not yet implemented", named))
}

// packPrimitive packs a scalar into a primitive value struct using insertvalue.
// Result: { null, null, <coerced_raw> }
func (c *Compiler) packPrimitive(val value.Value, named *types.Named, layout *TypeDeclLayout) value.Value {
	valueStructType := layout.Value.LLVMType

	// Start with undef
	var agg value.Value = constant.NewUndef(valueStructType)

	// Field 0: _vtable = null (i8*)
	agg = c.block.NewInsertValue(agg, constant.NewNull(irtypes.I8Ptr), 0)

	// Field 1: _instance = null (promise_T_i*)
	instancePtrType := layout.Value.Fields[1].LLVMType.(*irtypes.PointerType)
	agg = c.block.NewInsertValue(agg, constant.NewNull(instancePtrType), 1)

	// Field 2: raw = the value (with type coercion if needed)
	rawVal := c.coerceToRaw(val, named, layout)
	agg = c.block.NewInsertValue(agg, rawVal, 2)

	return agg
}

// unpackFromValueStruct extracts the internal representation from a promise_T_v return.
func (c *Compiler) unpackFromValueStruct(val value.Value, named *types.Named, layout *TypeDeclLayout) value.Value {
	if layout.Kind == LayoutPrimitive {
		return c.unpackPrimitive(val, named, layout)
	}
	panic(fmt.Sprintf("codegen: unpacking non-primitive type %s not yet implemented", named))
}

// unpackPrimitive extracts the raw scalar from a primitive value struct.
func (c *Compiler) unpackPrimitive(val value.Value, named *types.Named, layout *TypeDeclLayout) value.Value {
	rawVal := c.block.NewExtractValue(val, 2)
	return c.coerceFromRaw(rawVal, named, layout)
}

// coerceToRaw converts an internal Promise value to the raw field type.
// Key cases: bool i1 → i8, integer widening, float widening.
func (c *Compiler) coerceToRaw(val value.Value, named *types.Named, layout *TypeDeclLayout) value.Value {
	rawType := layout.RawLLVM
	valType := val.Type()

	// Bool: internal i1 → raw i8
	if named == types.TypBool {
		return c.block.NewZExt(val, irtypes.I8)
	}

	// Integer widening/narrowing
	if valInt, ok := valType.(*irtypes.IntType); ok {
		if rawInt, ok := rawType.(*irtypes.IntType); ok {
			if valInt.BitSize < rawInt.BitSize {
				if layout.IsSigned {
					return c.block.NewSExt(val, rawType)
				}
				return c.block.NewZExt(val, rawType)
			}
			if valInt.BitSize > rawInt.BitSize {
				return c.block.NewTrunc(val, rawType)
			}
		}
	}

	// Float widening (float → double)
	if valType == irtypes.Float && rawType == irtypes.Double {
		return c.block.NewFPExt(val, irtypes.Double)
	}

	return val // already the right type
}

// coerceFromRaw converts a raw field value back to the internal Promise type.
// Mirrors coerceToRaw: bool i8→i1, integer narrowing/widening, float truncation.
func (c *Compiler) coerceFromRaw(val value.Value, named *types.Named, layout *TypeDeclLayout) value.Value {
	// Bool: raw i8 → internal i1
	if named == types.TypBool {
		return c.block.NewTrunc(val, irtypes.I1)
	}

	internalType := llvmNamedType(named)
	rawType := val.Type()

	// Integer narrowing/widening (raw → internal)
	if rawInt, ok := rawType.(*irtypes.IntType); ok {
		if intlInt, ok := internalType.(*irtypes.IntType); ok {
			if rawInt.BitSize > intlInt.BitSize {
				return c.block.NewTrunc(val, internalType)
			}
			if rawInt.BitSize < intlInt.BitSize {
				if layout.IsSigned {
					return c.block.NewSExt(val, internalType)
				}
				return c.block.NewZExt(val, internalType)
			}
		}
	}

	// Float truncation (double → float)
	if rawType == irtypes.Double && internalType == irtypes.Float {
		return c.block.NewFPTrunc(val, irtypes.Float)
	}

	return val
}
