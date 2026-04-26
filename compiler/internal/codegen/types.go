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
		return irtypes.I8Ptr // function pointer placeholder
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
