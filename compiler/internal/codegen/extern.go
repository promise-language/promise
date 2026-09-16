package codegen

import (
	"fmt"

	"github.com/llir/llvm/ir"
	"github.com/llir/llvm/ir/constant"
	irtypes "github.com/llir/llvm/ir/types"
	"github.com/llir/llvm/ir/value"

	"github.com/promise-language/promise/compiler/internal/types"
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

// rawABIExternSymbols are the canonical-ABI runtime helpers defined in
// compiler/cmd/promise/crt/wasm32/wasm_alloc.c and statically linked from
// wasm_alloc.o. Unlike the platform-layer symbols codegen synthesizes — which
// cross the `extern boundary in Promise's value-struct bridge form (a leading
// sret pointer, and every value parameter as a pointer to its value struct) —
// these are plain C functions and cross in the raw platform C ABI: scalars by
// value, string and T[] as their bare instance/header pointer, scalar and
// pointer results returned directly.
//
// Which ABI a symbol carries is a property of the *symbol*, not of the
// declaration, so it is registered here rather than marked up in generated
// binding source — annotations.md §13: the set of bindable symbols is closed and
// "registered in one place". Recording it here also means bindings already
// published against an older compiler (the wasi_preview_2 catalog module) become
// correct without regeneration (T1660).
//
// Adding a helper to wasm_alloc.c means adding its symbol here.
var rawABIExternSymbols = map[string]bool{
	"cabi_realloc":     true,
	"cabi_retarea_ptr": true,
	"cabi_load_i32":    true,
	"cabi_load_i64":    true,
	"cabi_load_f32":    true,
	"cabi_load_f64":    true,
	"cabi_store_i32":   true,
	"cabi_store_i64":   true,
	"cabi_store_f32":   true,
	"cabi_store_f64":   true,
	"cabi_string_data": true,
	"cabi_string_len":  true,
	"cabi_string_from": true,
	"cabi_vector_data": true,
	"cabi_vector_len":  true,
	"cabi_vector_from": true,
}

// isRawABIExtern reports whether an extern symbol is one of the raw-C-ABI
// canonical-ABI runtime helpers (see rawABIExternSymbols).
func isRawABIExtern(cName string) bool { return rawABIExternSymbols[cName] }

// fitsRawABI reports whether every parameter and the result of ext can cross in
// the raw C ABI — a scalar by value, or a bare pointer for a string or an opaque
// container. A user type, enum, optional or `&T` ref has no raw C form, and a
// failable result is Promise's own {i1, T, i8*} struct rather than anything C
// declares.
//
// A declaration that does not fit simply keeps the value-struct bridge ABI. That
// matters because `extern is writable in ordinary source: someone can name a
// canonical-ABI symbol with any signature at all, and crashing codegen over it
// would be a far worse diagnosis than the wasm-ld signature check, which names
// the symbol and explains the consequence (T1660).
func (c *Compiler) fitsRawABI(ext *ExternFunc) bool {
	fits := func(t types.Type) bool {
		if isOpaqueContainerType(t) {
			return true
		}
		if isRefType(t) {
			return false
		}
		layout := c.lookupLayout(t)
		return layout != nil && (layout.Kind == LayoutPrimitive || layout.Kind == LayoutString)
	}
	for _, pt := range ext.ParamTypes {
		if !fits(pt) {
			return false
		}
	}
	return ext.ResultType == nil || fits(ext.ResultType)
}

// declareExterns creates LLVM IR function declarations for all extern functions.
// Value params are passed by pointer (i8*); ref params as typed pointers.
// Struct returns use sret pattern (void return, first param is result pointer).
func (c *Compiler) declareExterns(externs []*ExternFunc, layouts map[*types.Named]*TypeDeclLayout) {
	// Track declared C functions to deduplicate
	cFuncs := make(map[string]*ir.Func)

	for _, ext := range externs {
		// Struct return: use sret pattern (return void, first param is pointer to result).
		// This matches C ABI on ARM64 where large structs are returned via pointer.
		// Container types (Vector, Channel, string) return i8* directly — no sret.
		// Failable externs always use sret for the {i1, value, i8*} result struct.
		hasSret := ext.ResultType != nil && !isOpaqueContainerType(ext.ResultType)
		if ext.IsFailable {
			hasSret = true // failable result {i1, ..., i8*} always via sret
		}
		// WASM imports: primitive scalars return directly, not via sret.
		// Only applies to actual WASM imports (wasm_import annotation), not internal
		// PAL functions whose synthesized bodies expect sret ABI.
		isWasmImport := c.isWasm && ext.WasmImportMod != ""
		if isWasmImport && hasSret && !ext.IsFailable {
			if named := extractNamed(ext.ResultType); named != nil {
				if layout := c.lookupLayout(ext.ResultType); layout != nil && layout.Kind == LayoutPrimitive {
					hasSret = false
				}
			}
		}

		// Raw-C-ABI runtime helpers: no sret, scalars by value, string/vector as
		// their bare pointer. The result is then whatever llvmNamedType gives
		// (a scalar, or i8* for string), which is exactly the C declaration.
		//
		// A `wasm_import on the same declaration names a *host* import rather
		// than the statically-linked helper, so it wins: the symbol is then just
		// an import name and the canonical (ptr, len) convention is the right one.
		// Decided once here and carried on ext.RawABI, because genExternCall has
		// to lower the arguments the same way this declared the parameters.
		isRawABI := isRawABIExtern(ext.CName) && !isWasmImport && !ext.IsFailable && c.fitsRawABI(ext)
		if isRawABI {
			hasSret = false
		}

		// Deduplicate by C name — multiple Promise externs may map to the same C function
		if fn, ok := cFuncs[ext.CName]; ok {
			ext.IRFunc = fn
			ext.HasSret = hasSret
			ext.RawABI = isRawABI
			c.funcs[ext.PromiseName] = fn
			continue
		}

		var params []*ir.Param
		if hasSret {
			params = append(params, ir.NewParam("sret", irtypes.I8Ptr))
		}

		for i, pt := range ext.ParamTypes {
			paramName := ext.Sig.Params()[i].Name()

			if isOpaqueContainerType(pt) {
				// Container types (Vector, Channel, string) are i8* pointers
				params = append(params, ir.NewParam(paramName, irtypes.I8Ptr))
				continue
			}

			layout := c.lookupLayout(pt)
			if layout == nil {
				panic(fmt.Sprintf("codegen: cannot resolve layout for extern param %d of %s", i, ext.PromiseName))
			}

			if isRefType(pt) {
				params = append(params, ir.NewParam(paramName, irtypes.NewPointer(layout.Value.LLVMType)))
				continue
			}

			if isRawABI {
				// Raw C ABI: scalars by value; a string is the bare instance
				// pointer, NOT the wasm_import (ptr, len) flattening below —
				// wasm_alloc.c reads the Promise string layout itself, whereas a
				// JS host cannot know that layout. fitsRawABI already established
				// that every parameter kind here has a raw form.
				params = append(params, ir.NewParam(paramName, llvmNamedType(extractNamed(pt))))
				continue
			}

			if isWasmImport && layout.Kind == LayoutPrimitive {
				// WASM imports: primitive scalars pass by value directly
				params = append(params, ir.NewParam(paramName, llvmNamedType(extractNamed(pt))))
				continue
			}

			if isWasmImport && layout.Kind == LayoutString {
				// WASM imports: strings flatten to a canonical (ptr, len) pair
				// instead of a pointer to Promise's private boxed-string value
				// struct — the host (JS) side has no knowledge of that internal
				// layout, and previously received a single pointer to it verbatim (T1506).
				params = append(params, ir.NewParam(paramName+"_ptr", irtypes.I8Ptr))
				params = append(params, ir.NewParam(paramName+"_len", irtypes.I32))
				continue
			}

			// All extern value struct params: pass by pointer to match C ABI
			params = append(params, ir.NewParam(paramName, irtypes.I8Ptr))
		}

		retType := irtypes.Type(irtypes.Void)
		if !ext.IsFailable && ext.ResultType != nil {
			if isOpaqueContainerType(ext.ResultType) {
				retType = irtypes.I8Ptr
			} else if !hasSret {
				// WASM import: primitive scalar direct return.
				// Raw C ABI: scalar, or i8* for a string instance pointer.
				retType = llvmNamedType(extractNamed(ext.ResultType))
			}
		}
		fn := c.module.NewFunc(ext.CName, retType, params...)

		// Emit WASM import attributes when targeting WASM
		if c.isWasm && ext.WasmImportMod != "" {
			fn.FuncAttrs = append(fn.FuncAttrs,
				ir.AttrPair{Key: "wasm-import-module", Value: ext.WasmImportMod},
				ir.AttrPair{Key: "wasm-import-name", Value: ext.WasmImportName})
		}

		ext.IRFunc = fn
		ext.HasSret = hasSret
		ext.RawABI = isRawABI
		c.funcs[ext.PromiseName] = fn
		cFuncs[ext.CName] = fn
	}
}

// genExternCall generates an extern function call with ABI coercion.
// Struct params are passed by pointer and struct returns use sret pattern,
// matching C ABI on ARM64 where large structs are passed/returned indirectly.
// Failable externs use sret for the {i1, value, i8*} result struct and return
// the failable result directly — the caller handles error checking via genErrorHandlerExpr.
func (c *Compiler) genExternCall(ext *ExternFunc, argVals []value.Value, argTypes []types.Type) value.Value {
	var callArgs []value.Value

	// sret: allocate space for the return struct, pass pointer as first arg.
	// Failable externs use sret for the failable result type {i1, T, i8*}.
	// Optional externs use sret for the optional type {i1, T}.
	var sretAlloca *ir.InstAlloca
	var failResultType *irtypes.StructType
	var optResultType irtypes.Type
	if ext.HasSret {
		if ext.IsFailable {
			// Failable: sret holds {i1, internal_type, i8*} — same as regular failable functions
			var innerType irtypes.Type = irtypes.Void
			if ext.ResultType != nil {
				innerType = c.resolveType(ext.ResultType)
			}
			failResultType = computeResultType(innerType)
			sretAlloca = c.createEntryAlloca(failResultType)
		} else if _, isOpt := ext.ResultType.(*types.Optional); isOpt {
			// Optional: sret holds {i1, T} — bridge writes internal-form optional directly
			optResultType = c.resolveType(ext.ResultType)
			sretAlloca = c.createEntryAlloca(optResultType)
		} else {
			layout := c.lookupLayout(ext.ResultType)
			sretAlloca = c.createEntryAlloca(layout.Value.LLVMType)
		}
		sretPtr := c.block.NewBitCast(sretAlloca, irtypes.I8Ptr)
		callArgs = append(callArgs, sretPtr)
	}

	for i, arg := range argVals {
		// Ref params: pass the pointer directly (already a pointer to value struct)
		if isRefType(ext.ParamTypes[i]) {
			callArgs = append(callArgs, arg)
			continue
		}

		// Container types (Vector, Channel, string) are already i8* — pass directly
		if isOpaqueContainerType(ext.ParamTypes[i]) {
			callArgs = append(callArgs, arg)
			continue
		}

		// Raw C ABI: the internal representation already *is* the C value — a
		// scalar for primitives, the i8* instance pointer for strings — so it
		// passes through with no value-struct packing. declareExterns decided
		// this and declared the parameters to match (T1660).
		if ext.RawABI {
			callArgs = append(callArgs, arg)
			continue
		}

		// WASM imports: primitive scalars pass by value directly
		if c.isWasm && ext.WasmImportMod != "" {
			paramLayout := c.lookupLayout(ext.ParamTypes[i])
			if paramLayout != nil && paramLayout.Kind == LayoutPrimitive {
				callArgs = append(callArgs, arg)
				continue
			}
			// WASM imports: strings flatten to a (ptr, len) pair rather than a
			// pointer to Promise's private boxed-string value struct (T1506). `arg`
			// is already the raw i8* string instance pointer (see packString).
			if paramLayout != nil && paramLayout.Kind == LayoutString {
				dataPtr, dataLen := c.extractStringDataLenFromInstance(c.block, arg)
				callArgs = append(callArgs, dataPtr, c.block.NewTrunc(dataLen, irtypes.I32))
				continue
			}
		}

		named := extractNamed(argTypes[i])
		layout := c.lookupLayout(argTypes[i])
		if layout == nil {
			panic(fmt.Sprintf("codegen: cannot resolve layout for arg %d in call to %s", i, ext.PromiseName))
		}
		packed := c.packToValueStruct(arg, named, layout)

		// All extern value args: alloca, store, pass pointer (matching C ABI)
		alloca := c.createEntryAlloca(layout.Value.LLVMType)
		c.block.NewStore(packed, alloca)
		callArgs = append(callArgs, c.block.NewBitCast(alloca, irtypes.I8Ptr))
	}

	// Container return types return i8* directly — no sret (non-failable only)
	if !ext.IsFailable && ext.ResultType != nil && isOpaqueContainerType(ext.ResultType) {
		return c.block.NewCall(ext.IRFunc, callArgs...)
	}

	// WASM: primitive scalar direct return — call returns value directly
	if !ext.HasSret && ext.ResultType != nil {
		return c.block.NewCall(ext.IRFunc, callArgs...)
	}

	c.block.NewCall(ext.IRFunc, callArgs...)

	// Failable: load the {i1, T, i8*} result and return directly.
	// The bridge writes internal-form values, so no value struct unpacking needed.
	if ext.IsFailable {
		return c.block.NewLoad(failResultType, sretAlloca)
	}

	// Optional: load the {i1, T} result and return directly.
	// The bridge writes internal-form optional values, so no value struct unpacking needed.
	if optResultType != nil {
		return c.block.NewLoad(optResultType, sretAlloca)
	}

	// sret: load result from the alloca and unpack to internal representation
	if ext.HasSret {
		named := extractNamed(ext.ResultType)
		layout := c.lookupLayout(ext.ResultType)
		if layout == nil {
			panic(fmt.Sprintf("codegen: cannot resolve layout for return type of call to %s", ext.PromiseName))
		}
		result := c.block.NewLoad(layout.Value.LLVMType, sretAlloca)
		return c.unpackFromValueStruct(result, named, layout)
	}
	return nil
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
