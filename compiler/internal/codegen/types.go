package codegen

import (
	irtypes "github.com/llir/llvm/ir/types"
	"github.com/promise-language/promise/compiler/internal/types"
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

// isPrimitiveNumeric reports whether a Named type is a primitive numeric type.
func isPrimitiveNumeric(n *types.Named) bool {
	cat := classify(n)
	return cat == CatSignedInt || cat == CatUnsignedInt || cat == CatFloat
}

// isSignedType reports whether a Named type is a signed integer type.
func isSignedType(n *types.Named) bool {
	return classify(n) == CatSignedInt
}

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
	case *types.Enum:
		// Fieldless enums are i32 (tag only). Data enums need layout context
		// (ptrSize) to compute their struct type — callers should use
		// llvmTypeForEnumFieldFromPromise or instanceFieldLLVMType instead.
		for _, v := range t.Variants() {
			if v.NumFields() > 0 {
				return irtypes.I8Ptr // conservative fallback for data enums without layout context
			}
		}
		return irtypes.I32
	case *types.Signature:
		return closureType() // fat pointer: {fn_ptr, env_ptr}
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
	case *types.Array:
		return irtypes.NewArray(uint64(t.Size()), llvmType(t.Elem()))
	case *types.Instance:
		return irtypes.I8Ptr // generic instances (resolveType/instanceFieldLLVMType handle user types)
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

// closureType returns the fat pointer struct type used for all function values: {i8*, i8*}.
// Field 0 is the function pointer, field 1 is the environment pointer (null if no captures).
func closureType() *irtypes.StructType {
	return irtypes.NewStruct(irtypes.I8Ptr, irtypes.I8Ptr)
}

// isRefType returns true if the type is a shared or mutable reference.
func isRefType(typ types.Type) bool {
	switch typ.(type) {
	case *types.SharedRef, *types.MutRef:
		return true
	}
	return false
}

// isPrimitiveScalar reports whether a Named type is a scalar primitive
// (int, i8-i64, uint, u8-u64, f32, f64, bool, char).
// These use raw LLVM types (i64, i32, i1, double, etc.), NOT i8* pointers.
func isPrimitiveScalar(n *types.Named) bool {
	cat := classify(n)
	return cat != CatUnknown
}

// isOpaqueContainerType returns true for Vector, Channel, and Task types,
// which are opaque i8* pointers without value struct layouts.
// Unlike isContainerType, this excludes string (which has a value struct layout).
func isOpaqueContainerType(typ types.Type) bool {
	named := extractNamed(typ)
	if _, ok := types.AsVector(typ); ok || named == types.TypVector {
		return true
	}
	if _, ok := types.AsChannel(typ); ok || named == types.TypChannel {
		return true
	}
	if named == types.TypTask {
		return true
	}
	if _, ok := types.AsArc(typ); ok || named == types.TypArc {
		return true
	}
	if _, ok := types.AsWeak(typ); ok || named == types.TypWeak {
		return true
	}
	if _, ok := types.AsMutex(typ); ok || named == types.TypMutex {
		return true
	}
	if _, ok := types.AsMutexGuard(typ); ok || named == types.TypMutexGuard {
		return true
	}
	return false
}

// isSingleOwnerHandleType reports whether typ is a single-owner native handle
// (Task[T] / Mutex[T] / MutexGuard[T]). These are LLVM `i8*` handles with no
// clone/dup semantics. Sema rejects clone()/filled()/nesting on containers
// transitively containing them (T0545); this is the codegen backstop so any
// residual generic-instantiation path degrades to a shallow copy instead of a
// Go panic (the dupHeapValue `{vtable,instance}` struct assert fails on i8*).
func isSingleOwnerHandleType(typ types.Type) bool {
	named := extractNamed(typ)
	if _, ok := types.AsTask(typ); ok || named == types.TypTask {
		return true
	}
	if _, ok := types.AsMutex(typ); ok || named == types.TypMutex {
		return true
	}
	if _, ok := types.AsMutexGuard(typ); ok || named == types.TypMutexGuard {
		return true
	}
	return false
}

// isContainerType returns true for Vector and string types,
// which are represented as i8* pointers (not value structs) in codegen.
// Map is no longer a container type — it's a user-defined type with value struct layout.
// Checks both Instance wrappers (user code: Vector[int]) and bare Named
// types (method body: this is TypVector).
func isContainerType(typ types.Type) bool {
	named := extractNamed(typ)
	if _, ok := types.AsVector(typ); ok || named == types.TypVector {
		return true
	}
	if _, ok := types.AsChannel(typ); ok || named == types.TypChannel {
		return true
	}
	if _, ok := types.AsArc(typ); ok || named == types.TypArc {
		return true
	}
	if _, ok := types.AsWeak(typ); ok || named == types.TypWeak {
		return true
	}
	if _, ok := types.AsMutex(typ); ok || named == types.TypMutex {
		return true
	}
	if _, ok := types.AsMutexGuard(typ); ok || named == types.TypMutexGuard {
		return true
	}
	// T0508: Task[T] is an opaque i8* handle (G struct pointer), same as the
	// other native handles. Missing entry caused Optional[Task[T]] drop paths
	// to fall through to extractInstancePtr and emit malformed IR.
	if _, ok := types.AsTask(typ); ok || named == types.TypTask {
		return true
	}
	if named == types.TypString {
		return true
	}
	return false
}

// isErrorType returns true if the named type is the error base type or inherits from it.
func isErrorType(named *types.Named) bool {
	if types.TypError != nil && named == types.TypError {
		return true
	}
	for _, p := range named.Parents() {
		if isErrorType(p.Named) {
			return true
		}
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

// userValueType returns the value struct layout for user-defined types.
// All user types share this layout: { i8* _vtable, i8* _instance }.
// This is the unit of passing per the four-struct model (T#v).
func userValueType() *irtypes.StructType {
	return irtypes.NewStruct(irtypes.I8Ptr, irtypes.I8Ptr)
}

// llvmTypeForEnumFieldFromPromise returns the LLVM type for an enum variant field
// given the original Promise type. User-defined Named types use {i8*, i8*} (value
// struct) to match the representation produced by genExpr. Enum types use their
// internal representation (i32 for fieldless, {i32, [N x i8]} for data enums).
func llvmTypeForEnumFieldFromPromise(typ types.Type, ptrSize int, enumLayouts map[*types.Enum]*TypeDeclLayout, monoEnumLayouts map[string]*TypeDeclLayout) irtypes.Type {
	// Handle Tuple types — recurse so heap user / optional / enum elements get
	// their proper layout (value struct {i8*, i8*}, not bare i8*). (T0442)
	if tup, ok := typ.(*types.Tuple); ok {
		fields := make([]irtypes.Type, len(tup.Elems()))
		for i, elem := range tup.Elems() {
			fields[i] = llvmTypeForEnumFieldFromPromise(elem, ptrSize, enumLayouts, monoEnumLayouts)
		}
		return irtypes.NewStruct(fields...)
	}
	// Handle Optional types — recurse for inner type so Optional<UserType>
	// gets {i1, {i8*, i8*}} instead of {i1, i8*}. (T0442)
	if opt, ok := typ.(*types.Optional); ok {
		inner := llvmTypeForEnumFieldFromPromise(opt.Elem(), ptrSize, enumLayouts, monoEnumLayouts)
		if _, isVoid := inner.(*irtypes.VoidType); isVoid {
			return irtypes.I1
		}
		return irtypes.NewStruct(irtypes.I1, inner)
	}
	// Handle Array types — recurse for element type. (T0442)
	if arr, ok := typ.(*types.Array); ok {
		elem := llvmTypeForEnumFieldFromPromise(arr.Elem(), ptrSize, enumLayouts, monoEnumLayouts)
		return irtypes.NewArray(uint64(arr.Size()), elem)
	}
	// Handle enum types — enums used as fields in other enum variants
	if lt := enumInternalTypeForField(typ, ptrSize, enumLayouts, monoEnumLayouts); lt != nil {
		return lt
	}
	// User-defined Named types (not primitives, string, void) → value struct
	// Native container/handle types (Vector, Channel, Task) are opaque i8* pointers.
	if n := extractNamed(typ); n != nil && classify(n) == CatUnknown {
		if n != types.TypString && n != types.TypVoid && n != types.TypNone &&
			n != types.TypVector && n != types.TypChannel && n != types.TypTask && n != types.TypArc &&
			n != types.TypWeak && n != types.TypMutex && n != types.TypMutexGuard {
			return userValueType() // {i8*, i8*}
		}
	}
	return llvmType(typ)
}

// enumInternalTypeForField returns the LLVM internal representation type for an
// enum type used as a field. Returns nil if typ is not an enum.
// Handles both non-generic enums (*types.Enum) and generic enum instances
// (*types.Instance wrapping *types.Enum).
// When existing layouts are available, returns the named struct type from the layout
// to ensure LLVM type identity matches across the IR.
func enumInternalTypeForField(typ types.Type, ptrSize int, enumLayouts map[*types.Enum]*TypeDeclLayout, monoEnumLayouts map[string]*TypeDeclLayout) irtypes.Type {
	switch t := typ.(type) {
	case *types.Enum:
		// Look up existing layout first (named struct types must match)
		if enumLayouts != nil {
			if layout, ok := enumLayouts[t]; ok {
				return layout.EnumInternalType
			}
		}
		// Fall back to computing from definition
		return computeEnumInternalType(t, nil, ptrSize, enumLayouts, monoEnumLayouts)
	case *types.Instance:
		e, ok := t.Origin().(*types.Enum)
		if !ok {
			return nil
		}
		// Look up existing mono layout first
		if monoEnumLayouts != nil {
			name := monoName(t)
			if layout, ok := monoEnumLayouts[name]; ok {
				return layout.EnumInternalType
			}
		}
		subst := types.BuildSubstMap(e.TypeParams(), t.TypeArgs())
		return computeEnumInternalType(e, subst, ptrSize, enumLayouts, monoEnumLayouts)
	default:
		return nil
	}
}

// computeEnumInternalType computes the LLVM internal representation for an enum
// from its definition. Used as fallback when no pre-computed layout is available.
func computeEnumInternalType(enum *types.Enum, subst map[*types.TypeParam]types.Type, ptrSize int, enumLayouts map[*types.Enum]*TypeDeclLayout, monoEnumLayouts map[string]*TypeDeclLayout) irtypes.Type {
	maxDataSize := 0
	for _, v := range enum.Variants() {
		if v.NumFields() > 0 {
			var fieldTypes []irtypes.Type
			for _, f := range v.Fields() {
				ft := f.Type()
				if subst != nil {
					ft = types.Substitute(ft, subst)
				}
				fieldTypes = append(fieldTypes, llvmTypeForEnumFieldFromPromise(ft, ptrSize, enumLayouts, monoEnumLayouts))
			}
			// Compute data size from the struct type to account for alignment padding
			ds := llvmTypeSizeWithPtr(irtypes.NewStruct(fieldTypes...), ptrSize)
			if ds > maxDataSize {
				maxDataSize = ds
			}
		}
	}
	if maxDataSize == 0 {
		return irtypes.I32
	}
	return irtypes.NewStruct(irtypes.I32, irtypes.NewArray(uint64(maxDataSize), irtypes.I8))
}

// instanceFieldLLVMType returns the LLVM type for an instance struct field.
// User-defined types are stored as value structs { vtable, instance } to
// preserve vtable information for dispatch. Enum types use their internal
// representation (i32 for fieldless, {i32, [N x i8]} for data enums).
// Value types use their wider value struct (with embedded fields) instead of
// the standard {i8*, i8*} layout.
func instanceFieldLLVMType(typ types.Type, allLayouts map[*types.Named]*TypeDeclLayout, ptrSize int, enumLayouts map[*types.Enum]*TypeDeclLayout, monoEnumLayouts map[string]*TypeDeclLayout, monoLayouts map[string]*TypeDeclLayout) irtypes.Type {
	// Handle Optional types — recurse into inner type so Optional<UserType>
	// gets {i1, {i8*, i8*}} instead of the incorrect {i1, i8*} from llvmType. (B0030)
	if opt, ok := typ.(*types.Optional); ok {
		inner := instanceFieldLLVMType(opt.Elem(), allLayouts, ptrSize, enumLayouts, monoEnumLayouts, monoLayouts)
		if _, isVoid := inner.(*irtypes.VoidType); isVoid {
			return irtypes.I1
		}
		return irtypes.NewStruct(irtypes.I1, inner)
	}
	// Handle Tuple types — recurse so heap user / optional / enum elements get
	// their proper field layout (value struct {i8*, i8*}, not bare i8*). (T0420)
	if tup, ok := typ.(*types.Tuple); ok {
		fields := make([]irtypes.Type, len(tup.Elems()))
		for i, elem := range tup.Elems() {
			fields[i] = instanceFieldLLVMType(elem, allLayouts, ptrSize, enumLayouts, monoEnumLayouts, monoLayouts)
		}
		return irtypes.NewStruct(fields...)
	}
	// Handle Array types — recurse so heap user / optional / enum / value-type
	// elements get their proper field layout. Without this, [N x T] field slots
	// fall through to llvmType(typ) which yields [N x i8*] for any user-typed
	// element, mismatching the array-literal expression layout. (T0579)
	if arr, ok := typ.(*types.Array); ok {
		elem := instanceFieldLLVMType(arr.Elem(), allLayouts, ptrSize, enumLayouts, monoEnumLayouts, monoLayouts)
		return irtypes.NewArray(uint64(arr.Size()), elem)
	}
	// Handle enum types — enums used as fields in user types
	if lt := enumInternalTypeForField(typ, ptrSize, enumLayouts, monoEnumLayouts); lt != nil {
		return lt
	}
	// Handle generic value-type instances — use the mono layout's value struct so
	// the field slot matches the wider {i8* _vtable, field1, field2, ...} layout
	// produced by the value-type constructor. (T0565)
	if inst, ok := typ.(*types.Instance); ok {
		if origin, ok := inst.Origin().(*types.Named); ok && origin.IsValueType() {
			if monoLayouts != nil {
				if layout, ok := monoLayouts[monoName(inst)]; ok {
					return layout.Value.LLVMType
				}
			}
		}
	}
	if n := extractNamed(typ); n != nil && classify(n) == CatUnknown {
		if n != types.TypString && n != types.TypVoid && n != types.TypNone &&
			n != types.TypVector && n != types.TypChannel && n != types.TypTask && n != types.TypArc &&
			n != types.TypWeak && n != types.TypMutex && n != types.TypMutexGuard {
			// Value types have a wider value struct with embedded fields
			if n.IsValueType() {
				if layout, ok := allLayouts[n]; ok {
					return layout.Value.LLVMType
				}
			}
			return userValueType()
		}
	}
	return llvmType(typ)
}

// resolveType maps a Promise type to its LLVM IR type, with enum and mono awareness.
// Unlike llvmType (which is standalone), this method applies typeSubst for
// monomorphic codegen and looks up enum/mono layouts for correct internal types.
func (c *Compiler) resolveType(typ types.Type) irtypes.Type {
	// Apply current type substitution (inside monomorphic method/function bodies)
	if c.typeSubst != nil {
		typ = types.Substitute(typ, c.typeSubst)
	}
	// Apply self-type substitution (inside default method synthesis)
	if c.selfSubst != nil {
		typ = types.SubstituteSelf(typ, c.selfSubst.iface, c.selfSubst.concrete)
	}

	// Unwrap MutRef/SharedRef — borrows have the same LLVM representation as the inner type
	if ref, ok := typ.(*types.MutRef); ok {
		return c.resolveType(ref.Elem())
	}
	if ref, ok := typ.(*types.SharedRef); ok {
		return c.resolveType(ref.Elem())
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

	// Handle Array types (element type may need substitution)
	if arr, ok := typ.(*types.Array); ok {
		elemLLVM := c.resolveType(arr.Elem())
		return irtypes.NewArray(uint64(arr.Size()), elemLLVM)
	}

	// Handle Instance types
	if inst, ok := typ.(*types.Instance); ok {
		// Instance wrapping Enum → look up mono enum layout
		if layout := c.monoEnumLayouts[monoName(inst)]; layout != nil {
			return layout.EnumInternalType
		}
		// Vector/task instances → opaque pointer (native type)
		if origin, ok := inst.Origin().(*types.Named); ok {
			if origin == types.TypVector || origin == types.TypTask || origin == types.TypChannel || origin == types.TypArc || origin == types.TypWeak || origin == types.TypMutex || origin == types.TypMutexGuard {
				return irtypes.I8Ptr
			}
			// Instance wrapping Named user type → value struct
			if classify(origin) == CatUnknown && origin != types.TypString && origin != types.TypVoid && origin != types.TypNone {
				// Check for monomorphized value type layout
				if origin.IsValueType() {
					if layout := c.monoLayouts[monoName(inst)]; layout != nil {
						return layout.Value.LLVMType
					}
				}
				return userValueType()
			}
		}
		// Instance wrapping primitive Named → delegate to llvmType
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
		// Fallback: compute from definition. Handles cross-module enums whose
		// layout hasn't been computed yet (e.g. JsonValue from json module used
		// as Map value type during std module compilation).
		return computeEnumInternalType(enum, nil, c.ptrSize(), c.enumLayouts, c.monoEnumLayouts)
	}
	// User-defined Named types → value struct { vtable, instance }
	if n := extractNamed(typ); n != nil && classify(n) == CatUnknown {
		if n != types.TypString && n != types.TypVoid && n != types.TypNone {
			// Value types have a wider value struct with embedded fields
			if n.IsValueType() {
				if layout := c.lookupTypeLayout(typ); layout != nil {
					return layout.Value.LLVMType
				}
			}
			return userValueType()
		}
	}

	return llvmType(typ)
}

// isUserValueType returns true if typ resolves to a user value struct.
func (c *Compiler) isUserValueType(typ types.Type) bool {
	n := extractNamed(typ)
	if n == nil {
		return false
	}
	if classify(n) != CatUnknown {
		return false
	}
	return n != types.TypString && n != types.TypVoid && n != types.TypNone
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

// resolveParamType resolves a function parameter's LLVM type.
// MutRef params use pointer-to-inner-type so the callee can write back (B0149).
func (c *Compiler) resolveParamType(p *types.Param) irtypes.Type {
	paramType := c.resolveType(p.Type())
	if _, isMutRef := p.Type().(*types.MutRef); isMutRef {
		paramType = irtypes.NewPointer(paramType)
	}
	return paramType
}

// resultErrIdx returns the index of the error pointer field in a result struct.
// Void: { i1, i8* } → index 1. Non-void: { i1, T, i8* } → index 2.
func resultErrIdx(resultType *irtypes.StructType) uint64 {
	if isVoidResult(resultType) {
		return 1
	}
	return 2
}

// llvmTypeAlign returns the natural alignment of an LLVM type in bytes on a 64-bit target.
func llvmTypeAlign(typ irtypes.Type) int {
	return llvmTypeAlignWithPtr(typ, 8)
}

// llvmTypeAlignWithPtr returns the natural alignment of an LLVM type with the given pointer size.
func llvmTypeAlignWithPtr(typ irtypes.Type, ptrSize int) int {
	switch t := typ.(type) {
	case *irtypes.IntType:
		sz := int((t.BitSize + 7) / 8)
		if sz > 8 {
			return 8
		}
		return sz
	case *irtypes.FloatType:
		if t.Kind == irtypes.FloatKindFloat {
			return 4
		}
		return 8
	case *irtypes.PointerType:
		return ptrSize
	case *irtypes.StructType:
		maxAlign := 1
		for _, f := range t.Fields {
			if a := llvmTypeAlignWithPtr(f, ptrSize); a > maxAlign {
				maxAlign = a
			}
		}
		return maxAlign
	case *irtypes.ArrayType:
		return llvmTypeAlignWithPtr(t.ElemType, ptrSize)
	default:
		return 8
	}
}

// llvmTypeSize returns the byte size of an LLVM type on a 64-bit target,
// accounting for struct field alignment and padding.
func llvmTypeSize(typ irtypes.Type) int {
	return llvmTypeSizeWithPtr(typ, 8)
}

// llvmTypeSizeWithPtr returns the byte size of an LLVM type with the given pointer size.
func llvmTypeSizeWithPtr(typ irtypes.Type, ptrSize int) int {
	switch t := typ.(type) {
	case *irtypes.IntType:
		return int((t.BitSize + 7) / 8)
	case *irtypes.FloatType:
		if t.Kind == irtypes.FloatKindFloat {
			return 4
		}
		return 8 // double
	case *irtypes.PointerType:
		return ptrSize
	case *irtypes.StructType:
		offset := 0
		maxAlign := 1
		for _, f := range t.Fields {
			fieldAlign := llvmTypeAlignWithPtr(f, ptrSize)
			if fieldAlign > maxAlign {
				maxAlign = fieldAlign
			}
			if rem := offset % fieldAlign; rem != 0 {
				offset += fieldAlign - rem
			}
			offset += llvmTypeSizeWithPtr(f, ptrSize)
		}
		// Pad to struct alignment
		if rem := offset % maxAlign; rem != 0 {
			offset += maxAlign - rem
		}
		return offset
	case *irtypes.ArrayType:
		return int(t.Len) * llvmTypeSizeWithPtr(t.ElemType, ptrSize)
	default:
		return 8
	}
}
