package codegen

import (
	"djabi.dev/go/promise_lang/internal/types"
	irtypes "github.com/llir/llvm/ir/types"
)

// TypeCategory classifies Named types for native method dispatch.
// The codegen only cares about categories, not individual types.
type TypeCategory int

const (
	CatUnknown     TypeCategory = iota
	CatSignedInt                // int, i8, i16, i32, i64
	CatUnsignedInt              // uint, u8, u16, u32, u64
	CatFloat                    // f32, f64
	CatBool                     // bool
	CatChar                     // char (i32 codepoint, signed comparisons)
)

// classify returns the backend category for a Named type.
// This is the single point in codegen that compares against universe type singletons.
func classify(n *types.Named) TypeCategory {
	switch n {
	case types.TypInt, types.TypI8, types.TypI16, types.TypI32, types.TypI64:
		return CatSignedInt
	case types.TypUint, types.TypU8, types.TypU16, types.TypU32, types.TypU64:
		return CatUnsignedInt
	case types.TypF32, types.TypF64:
		return CatFloat
	case types.TypBool:
		return CatBool
	case types.TypChar:
		return CatChar
	default:
		return CatUnknown
	}
}

// llvmType maps a Promise type to its LLVM IR type.
func llvmType(typ types.Type) irtypes.Type {
	if typ == nil {
		return irtypes.Void
	}
	switch t := typ.(type) {
	case *types.Named:
		return llvmNamedType(t)
	case *types.Signature:
		return irtypes.I8Ptr // function pointer
	case *types.Tuple:
		fields := make([]irtypes.Type, len(t.Elems()))
		for i, elem := range t.Elems() {
			fields[i] = llvmType(elem)
		}
		return irtypes.NewStruct(fields...)
	case *types.Optional:
		inner := llvmType(t.Elem())
		if _, ok := inner.(*irtypes.VoidType); ok {
			return irtypes.I1
		}
		return irtypes.NewStruct(irtypes.I1, inner)
	case *types.Slice:
		return irtypes.I8Ptr // opaque pointer to heap-allocated header + data
	case *types.Array:
		return irtypes.I8Ptr // treated as heap-allocated slice for now
	case *types.Map:
		return irtypes.I8Ptr // opaque pointer to runtime hash table
	default:
		return irtypes.I8Ptr // opaque pointer placeholder for future types
	}
}

// llvmNamedType maps a *Named type to its LLVM IR type.
// For primitives, this returns the raw LLVM type matching the design doc's `raw` field.
func llvmNamedType(n *types.Named) irtypes.Type {
	switch n {
	case types.TypInt, types.TypI64:
		return irtypes.I64
	case types.TypI32:
		return irtypes.I32
	case types.TypI16:
		return irtypes.I16
	case types.TypI8:
		return irtypes.I8
	case types.TypUint, types.TypU64:
		return irtypes.I64
	case types.TypU32:
		return irtypes.I32
	case types.TypU16:
		return irtypes.I16
	case types.TypU8:
		return irtypes.I8
	case types.TypF64:
		return irtypes.Double
	case types.TypF32:
		return irtypes.Float
	case types.TypBool:
		return irtypes.I1
	case types.TypChar:
		return irtypes.I32 // Unicode codepoint
	case types.TypString:
		return irtypes.I8Ptr // opaque pointer to promise_string_i
	case types.TypVoid, types.TypNone:
		return irtypes.Void
	default:
		return irtypes.I8Ptr // user-defined type: pointer for now
	}
}

// isRefType returns true if the type is a shared or mutable reference.
func isRefType(typ types.Type) bool {
	switch typ.(type) {
	case *types.SharedRef, *types.MutRef:
		return true
	}
	return false
}

// extractNamed returns the *Named type from a Promise type,
// unwrapping Instance, SharedRef, and MutRef if necessary.
func extractNamed(typ types.Type) *types.Named {
	switch t := typ.(type) {
	case *types.Named:
		return t
	case *types.Instance:
		if n, ok := t.Origin().(*types.Named); ok {
			return n
		}
	case *types.SharedRef:
		return extractNamed(t.Elem())
	case *types.MutRef:
		return extractNamed(t.Elem())
	}
	return nil
}

// extractEnum returns the *Enum type from a Promise type,
// unwrapping Instance, SharedRef, and MutRef if necessary.
func extractEnum(typ types.Type) *types.Enum {
	switch t := typ.(type) {
	case *types.Enum:
		return t
	case *types.Instance:
		if e, ok := t.Origin().(*types.Enum); ok {
			return e
		}
	case *types.SharedRef:
		return extractEnum(t.Elem())
	case *types.MutRef:
		return extractEnum(t.Elem())
	}
	return nil
}

// resolveType maps a Promise type to its LLVM IR type, with enum and mono awareness.
// Unlike llvmType (which is standalone), this method applies typeSubst for
// monomorphic codegen and looks up enum/mono layouts for correct internal types.
func (c *Compiler) resolveType(typ types.Type) irtypes.Type {
	// Apply current type substitution (inside monomorphic method/function bodies)
	if c.typeSubst != nil {
		typ = types.Substitute(typ, c.typeSubst)
	}

	// Handle Tuple types (elements may contain TypeParams needing substitution)
	if tup, ok := typ.(*types.Tuple); ok {
		fields := make([]irtypes.Type, len(tup.Elems()))
		for i, elem := range tup.Elems() {
			fields[i] = c.resolveType(elem)
		}
		return irtypes.NewStruct(fields...)
	}

	// Handle Optional types
	if opt, ok := typ.(*types.Optional); ok {
		inner := c.resolveType(opt.Elem())
		if _, ok := inner.(*irtypes.VoidType); ok {
			return irtypes.I1
		}
		return irtypes.NewStruct(irtypes.I1, inner)
	}

	// Handle Instance types
	if inst, ok := typ.(*types.Instance); ok {
		// Instance wrapping Enum → look up mono enum layout
		if layout := c.monoEnumLayouts[monoName(inst)]; layout != nil {
			return layout.EnumInternalType
		}
		// Instance wrapping Named → i8Ptr (heap pointer)
		return irtypes.I8Ptr
	}

	// Existing enum handling
	if enum := extractEnum(typ); enum != nil {
		if c.monoCtx != nil {
			if origin, ok := c.monoCtx.origin.(*types.Enum); ok && enum == origin {
				if layout := c.monoEnumLayouts[c.monoCtx.name]; layout != nil {
					return layout.EnumInternalType
				}
			}
		}
		if layout, ok := c.enumLayouts[enum]; ok {
			return layout.EnumInternalType
		}
		return irtypes.I32
	}
	return llvmType(typ)
}

// computeResultType builds the result struct type for a failable function.
// Non-void T!: { i1, T, i8* }. Void !: { i1, i8* }.
func computeResultType(innerType irtypes.Type) *irtypes.StructType {
	if _, ok := innerType.(*irtypes.VoidType); ok {
		return irtypes.NewStruct(irtypes.I1, irtypes.I8Ptr)
	}
	return irtypes.NewStruct(irtypes.I1, innerType, irtypes.I8Ptr)
}

// isVoidResult returns true if the result struct has no ok value field (void failable).
func isVoidResult(resultType *irtypes.StructType) bool {
	return len(resultType.Fields) == 2
}

// resultErrIdx returns the index of the error pointer field in a result struct.
// Void: { i1, i8* } → index 1. Non-void: { i1, T, i8* } → index 2.
func resultErrIdx(resultType *irtypes.StructType) uint64 {
	if isVoidResult(resultType) {
		return 1
	}
	return 2
}

// llvmTypeSize returns the byte size of an LLVM type on a 64-bit target.
// Used for computing enum variant data sizes.
func llvmTypeSize(typ irtypes.Type) int {
	switch t := typ.(type) {
	case *irtypes.IntType:
		return int((t.BitSize + 7) / 8)
	case *irtypes.FloatType:
		if t.Kind == irtypes.FloatKindFloat {
			return 4
		}
		return 8 // double
	case *irtypes.PointerType:
		return 8
	case *irtypes.StructType:
		size := 0
		for _, f := range t.Fields {
			size += llvmTypeSize(f)
		}
		return size
	case *irtypes.ArrayType:
		return int(t.Len) * llvmTypeSize(t.ElemType)
	default:
		return 8
	}
}
