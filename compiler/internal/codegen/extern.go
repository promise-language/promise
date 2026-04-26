package codegen

import (
	"fmt"

	"github.com/llir/llvm/ir"
	"github.com/llir/llvm/ir/constant"
	irtypes "github.com/llir/llvm/ir/types"
	"github.com/llir/llvm/ir/value"

	"djabi.dev/go/promise_lang/internal/types"
)

// lookupLayout resolves a TypeDeclLayout for any Promise type (named or enum).
func (c *Compiler) lookupLayout(typ types.Type) *TypeDeclLayout {
	if named := extractNamed(typ); named != nil {
		return c.layouts[named]
	}
	if enum := extractEnum(typ); enum != nil {
		return c.enumLayouts[enum]
	}
	return nil
}

// declareExterns creates LLVM IR function declarations for all extern functions.
// Parameters use promise_T_v struct types; return types use promise_T_v or void.
func (c *Compiler) declareExterns(externs []*ExternFunc, layouts map[*types.Named]*TypeDeclLayout) {
	for _, ext := range externs {
		var params []*ir.Param
		for i, pt := range ext.ParamTypes {
			layout := c.lookupLayout(pt)
			if layout == nil {
				panic(fmt.Sprintf("codegen: cannot resolve layout for extern param %d of %s", i, ext.PromiseName))
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
			layout := c.lookupLayout(ext.ResultType)
			if layout == nil {
				panic(fmt.Sprintf("codegen: cannot resolve layout for return type of extern %s", ext.PromiseName))
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
		layout := c.lookupLayout(argTypes[i])
		if layout == nil {
			panic(fmt.Sprintf("codegen: cannot resolve layout for arg %d in call to %s", i, ext.PromiseName))
		}
		coercedArgs[i] = c.packToValueStruct(arg, named, layout)
	}

	result := c.block.NewCall(ext.IRFunc, coercedArgs...)

	if ext.ResultType != nil {
		named := extractNamed(ext.ResultType)
		layout := c.lookupLayout(ext.ResultType)
		if layout == nil {
			panic(fmt.Sprintf("codegen: cannot resolve layout for return type of call to %s", ext.PromiseName))
		}
		return c.unpackFromValueStruct(result, named, layout)
	}
	return result
}

// packToValueStruct packs a Promise internal value into a promise_T_v struct.
func (c *Compiler) packToValueStruct(val value.Value, named *types.Named, layout *TypeDeclLayout) value.Value {
	switch layout.Kind {
	case LayoutPrimitive:
		return c.packPrimitive(val, named, layout)
	case LayoutString:
		return c.packString(val, layout)
	case LayoutUserType:
		return c.packUserType(val, layout)
	case LayoutEnum:
		return c.packEnum(val, layout)
	default:
		panic(fmt.Sprintf("codegen: packing kind %d not yet implemented", layout.Kind))
	}
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
	switch layout.Kind {
	case LayoutPrimitive:
		return c.unpackPrimitive(val, named, layout)
	case LayoutString:
		return c.unpackString(val, layout)
	case LayoutUserType:
		return c.unpackUserType(val, layout)
	case LayoutEnum:
		return c.unpackEnum(val, layout)
	default:
		panic(fmt.Sprintf("codegen: unpacking kind %d not yet implemented", layout.Kind))
	}
}

// unpackPrimitive extracts the raw scalar from a primitive value struct.
func (c *Compiler) unpackPrimitive(val value.Value, named *types.Named, layout *TypeDeclLayout) value.Value {
	rawVal := c.block.NewExtractValue(val, 2)
	return c.coerceFromRaw(rawVal, named, layout)
}

// packString packs an i8* instance pointer into a promise_string_v struct.
// Result: { null, bitcast(val) }
func (c *Compiler) packString(val value.Value, layout *TypeDeclLayout) value.Value {
	valueStructType := layout.Value.LLVMType
	var agg value.Value = constant.NewUndef(valueStructType)

	// Field 0: _vtable = null (i8*)
	agg = c.block.NewInsertValue(agg, constant.NewNull(irtypes.I8Ptr), 0)

	// Field 1: _instance = bitcast i8* to promise_string_i*
	instancePtrType := layout.Value.Fields[1].LLVMType.(*irtypes.PointerType)
	inst := c.block.NewBitCast(val, instancePtrType)
	agg = c.block.NewInsertValue(agg, inst, 1)

	return agg
}

// unpackString extracts the i8* instance pointer from a promise_string_v return.
func (c *Compiler) unpackString(val value.Value, layout *TypeDeclLayout) value.Value {
	// extractvalue field 1 → promise_string_i*
	inst := c.block.NewExtractValue(val, 1)
	// bitcast back to i8*
	return c.block.NewBitCast(inst, irtypes.I8Ptr)
}

// packUserType packs an internal value struct { vtable, instance } into a promise_T_v struct.
// Extracts instance from value struct, bitcasts to typed instance pointer.
func (c *Compiler) packUserType(val value.Value, layout *TypeDeclLayout) value.Value {
	valueStructType := layout.Value.LLVMType
	var agg value.Value = constant.NewUndef(valueStructType)

	// Field 0: _vtable = null (i8*)
	agg = c.block.NewInsertValue(agg, constant.NewNull(irtypes.I8Ptr), 0)

	// Field 1: _instance = bitcast(extracted instance i8* → promise_T_i*)
	instance := c.extractInstancePtr(val)
	instancePtrType := layout.Value.Fields[1].LLVMType.(*irtypes.PointerType)
	inst := c.block.NewBitCast(instance, instancePtrType)
	agg = c.block.NewInsertValue(agg, inst, 1)

	return agg
}

// unpackUserType extracts the internal value struct from a promise_T_v return.
// Builds { vtable, instance } from the C ABI struct { i8* vtable, promise_T_i* instance }.
func (c *Compiler) unpackUserType(val value.Value, layout *TypeDeclLayout) value.Value {
	// Extract vtable (field 0) — i8*
	vtable := c.block.NewExtractValue(val, 0)
	// Extract instance (field 1) — promise_T_i*, bitcast to i8*
	inst := c.block.NewExtractValue(val, 1)
	instancePtr := c.block.NewBitCast(inst, irtypes.I8Ptr)

	// Build internal value struct { i8*, i8* }
	var agg value.Value = constant.NewUndef(userValueType())
	agg = c.block.NewInsertValue(agg, vtable, 0)
	agg = c.block.NewInsertValue(agg, instancePtr, 1)
	return agg
}

// packEnum packs an enum internal value into a promise_T_v struct.
// Fieldless: { null, null, tag }. Data: { null, null, tag, data_bytes }.
func (c *Compiler) packEnum(val value.Value, layout *TypeDeclLayout) value.Value {
	valueStructType := layout.Value.LLVMType
	var agg value.Value = constant.NewUndef(valueStructType)

	// Field 0: _vtable = null (i8*)
	agg = c.block.NewInsertValue(agg, constant.NewNull(irtypes.I8Ptr), 0)

	// Field 1: _instance = null (promise_T_i*)
	instancePtrType := layout.Value.Fields[1].LLVMType.(*irtypes.PointerType)
	agg = c.block.NewInsertValue(agg, constant.NewNull(instancePtrType), 1)

	if layout.MaxVariantDataSize == 0 {
		// Fieldless enum: internal value is i32 tag
		agg = c.block.NewInsertValue(agg, val, 2)
	} else {
		// Data enum: internal value is { i32, [N x i8] }
		tag := c.block.NewExtractValue(val, 0)
		agg = c.block.NewInsertValue(agg, tag, 2)
		data := c.block.NewExtractValue(val, 1)
		agg = c.block.NewInsertValue(agg, data, 3)
	}

	return agg
}

// unpackEnum extracts the internal enum value from a promise_T_v return.
func (c *Compiler) unpackEnum(val value.Value, layout *TypeDeclLayout) value.Value {
	if layout.MaxVariantDataSize == 0 {
		// Fieldless: extract tag at index 2
		return c.block.NewExtractValue(val, 2)
	}
	// Data enum: build { i32, [N x i8] } from tag (index 2) and data (index 3)
	tag := c.block.NewExtractValue(val, 2)
	data := c.block.NewExtractValue(val, 3)
	var agg value.Value = constant.NewUndef(layout.EnumInternalType)
	agg = c.block.NewInsertValue(agg, tag, 0)
	agg = c.block.NewInsertValue(agg, data, 1)
	return agg
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
