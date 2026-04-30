package codegen

import (
	"fmt"

	"github.com/llir/llvm/ir"
	"github.com/llir/llvm/ir/constant"
	"github.com/llir/llvm/ir/enum"
	irtypes "github.com/llir/llvm/ir/types"
	"github.com/llir/llvm/ir/value"

	"djabi.dev/go/promise_lang/internal/ast"
	"djabi.dev/go/promise_lang/internal/types"
)

// assignTypeID assigns a unique type ID to a Named type. Returns the existing
// ID if one was already assigned.
func (c *Compiler) assignTypeID(named *types.Named) int32 {
	if id, ok := c.typeIDs[named]; ok {
		return id
	}
	id := c.nextTypeID
	c.nextTypeID++
	c.typeIDs[named] = id
	return id
}

// collectAllParentIDs recursively collects all ancestor type IDs (transitive).
// Returns a deduplicated slice.
func (c *Compiler) collectAllParentIDs(named *types.Named) []int32 {
	var ids []int32
	for _, p := range named.Parents() {
		ids = append(ids, c.assignTypeID(p))
		ids = append(ids, c.collectAllParentIDs(p)...)
	}
	// Deduplicate
	seen := make(map[int32]bool)
	var unique []int32
	for _, id := range ids {
		if !seen[id] {
			seen[id] = true
			unique = append(unique, id)
		}
	}
	return unique
}

// emitTypeInfo creates a global type info constant for a Named type.
// Layout: { i8* vtable_ptr, i32 type_id, i32 num_parents, [N x i32] parent_ids }
func (c *Compiler) emitTypeInfo(named *types.Named) *ir.Global {
	typeID := c.assignTypeID(named)
	parentIDs := c.collectAllParentIDs(named)
	numParents := len(parentIDs)

	var structType *irtypes.StructType
	var fields []constant.Constant

	// Field 0: vtable pointer (or null if no vtable)
	if vtGlobal, ok := c.vtableGlobals[named]; ok && vtGlobal != nil {
		fields = append(fields, constant.NewBitCast(vtGlobal, irtypes.I8Ptr))
	} else {
		fields = append(fields, constant.NewNull(irtypes.I8Ptr))
	}

	fields = append(fields, constant.NewInt(irtypes.I32, int64(typeID)))
	fields = append(fields, constant.NewInt(irtypes.I32, int64(numParents)))

	if numParents > 0 {
		arrayType := irtypes.NewArray(uint64(numParents), irtypes.I32)
		structType = irtypes.NewStruct(irtypes.I8Ptr, irtypes.I32, irtypes.I32, arrayType)

		var parentConsts []constant.Constant
		for _, pid := range parentIDs {
			parentConsts = append(parentConsts, constant.NewInt(irtypes.I32, int64(pid)))
		}
		parentArray := constant.NewArray(arrayType, parentConsts...)
		fields = append(fields, parentArray)
	} else {
		structType = irtypes.NewStruct(irtypes.I8Ptr, irtypes.I32, irtypes.I32)
	}

	init := constant.NewStruct(structType, fields...)
	globalName := "promise_typeinfo_" + named.Obj().Name()
	global := c.module.NewGlobalDef(globalName, init)
	global.Immutable = true
	return global
}

// emitVtableGlobal creates a vtable global constant for a Named type.
// Layout: [M x i8*] where each entry is a function pointer for a virtual method slot.
func (c *Compiler) emitVtableGlobal(named *types.Named) *ir.Global {
	methods := named.AllVirtualMethods()
	if len(methods) == 0 {
		return nil
	}
	var entries []constant.Constant
	for _, m := range methods {
		ownerName := c.resolveMethodOwner(named, m.Name())
		mangledName := mangleMethodName(ownerName, m.Name(), m.IsSetter())
		if fn, ok := c.funcs[mangledName]; ok {
			entries = append(entries, constant.NewBitCast(fn, irtypes.I8Ptr))
		} else {
			// Abstract method with no body — store null
			entries = append(entries, constant.NewNull(irtypes.I8Ptr))
		}
	}
	arrayType := irtypes.NewArray(uint64(len(entries)), irtypes.I8Ptr)
	init := constant.NewArray(arrayType, entries...)
	global := c.module.NewGlobalDef("promise_vtable_"+named.Obj().Name(), init)
	global.Immutable = true
	return global
}

// emitVtableGlobals creates vtable globals for all user types that have virtual methods.
// Generic types are skipped (consistent with RTTI).
func (c *Compiler) emitVtableGlobals(file *ast.File) {
	for _, decl := range file.Decls {
		td, ok := decl.(*ast.TypeDecl)
		if !ok {
			continue
		}
		named := c.lookupNamedType(td.Name)
		if named == nil {
			continue
		}
		if len(named.TypeParams()) > 0 {
			continue
		}
		if vt := c.emitVtableGlobal(named); vt != nil {
			c.vtableGlobals[named] = vt
		}
	}
}

// emitTypeInfoGlobals creates global type info constants for all user types in the file.
// Generic types are skipped (they get type info through monomorphization).
func (c *Compiler) emitTypeInfoGlobals(file *ast.File) {
	for _, decl := range file.Decls {
		td, ok := decl.(*ast.TypeDecl)
		if !ok {
			continue
		}
		named := c.lookupNamedType(td.Name)
		if named == nil {
			continue
		}
		if len(named.TypeParams()) > 0 {
			continue // generic — monomorphic instances handled separately
		}
		c.typeInfoGlobals[named] = c.emitTypeInfo(named)
	}
}

// defineTypeIsFunc emits an LLVM IR function that checks if a type identified
// by its variant pointer is or inherits from the expected type ID.
// Replaces the C runtime promise_type_is.
// Typeinfo layout: { i8* vtable_ptr, i32 type_id, i32 num_parents, [0 x i32] parent_ids }
func (c *Compiler) defineTypeIsFunc() {
	variantParam := ir.NewParam("variant_ptr", irtypes.I8Ptr)
	expectedParam := ir.NewParam("expected_id", irtypes.I32)
	fn := c.module.NewFunc("promise_type_is", irtypes.I32, variantParam, expectedParam)

	// Typeinfo struct type with flexible array member for parent_ids
	typeinfoType := irtypes.NewStruct(
		irtypes.I8Ptr,                    // field 0: vtable_ptr
		irtypes.I32,                      // field 1: type_id
		irtypes.I32,                      // field 2: num_parents
		irtypes.NewArray(0, irtypes.I32), // field 3: parent_ids (flexible)
	)

	zero32 := constant.NewInt(irtypes.I32, 0)
	one32 := constant.NewInt(irtypes.I32, 1)

	// --- Basic blocks ---
	entry := fn.NewBlock("entry")
	checkID := fn.NewBlock("check_id")
	loopInit := fn.NewBlock("loop_init")
	loopHeader := fn.NewBlock("loop_header")
	loopBody := fn.NewBlock("loop_body")
	retTrueBlk := fn.NewBlock("ret_true")
	retFalseBlk := fn.NewBlock("ret_false")

	// entry: null check
	isNull := entry.NewICmp(enum.IPredEQ, variantParam, constant.NewNull(irtypes.I8Ptr))
	entry.NewCondBr(isNull, retFalseBlk, checkID)

	// check_id: load type_id, compare with expected_id
	typedPtr := checkID.NewBitCast(variantParam, irtypes.NewPointer(typeinfoType))
	tidPtr := checkID.NewGetElementPtr(typeinfoType, typedPtr, zero32, one32)
	typeID := checkID.NewLoad(irtypes.I32, tidPtr)
	match := checkID.NewICmp(enum.IPredEQ, typeID, expectedParam)
	checkID.NewCondBr(match, retTrueBlk, loopInit)

	// loop_init: load num_parents, check if > 0
	npPtr := loopInit.NewGetElementPtr(typeinfoType, typedPtr, zero32, constant.NewInt(irtypes.I32, 2))
	numParents := loopInit.NewLoad(irtypes.I32, npPtr)
	hasParents := loopInit.NewICmp(enum.IPredSGT, numParents, zero32)
	loopInit.NewCondBr(hasParents, loopHeader, retFalseBlk)

	// loop_header: phi node for index, bounds check
	iPhi := loopHeader.NewPhi(&ir.Incoming{X: zero32, Pred: loopInit})
	inBounds := loopHeader.NewICmp(enum.IPredSLT, iPhi, numParents)
	loopHeader.NewCondBr(inBounds, loopBody, retFalseBlk)

	// loop_body: load parent_ids[i], compare, increment
	pidPtr := loopBody.NewGetElementPtr(typeinfoType, typedPtr,
		zero32, constant.NewInt(irtypes.I32, 3), iPhi)
	parentID := loopBody.NewLoad(irtypes.I32, pidPtr)
	parentMatch := loopBody.NewICmp(enum.IPredEQ, parentID, expectedParam)
	iNext := loopBody.NewAdd(iPhi, one32)
	loopBody.NewCondBr(parentMatch, retTrueBlk, loopHeader)

	// Add second incoming to phi (from loop_body)
	iPhi.Incs = append(iPhi.Incs, &ir.Incoming{X: iNext, Pred: loopBody})

	// ret_true / ret_false
	retTrueBlk.NewRet(constant.NewInt(irtypes.I32, 1))
	retFalseBlk.NewRet(constant.NewInt(irtypes.I32, 0))

	c.funcs["promise_type_is"] = fn
}

// getOrEmitViewVtable emits a vtable ordered by the view type's AllVirtualMethods(),
// with function pointers resolved from the concrete type. Used when a concrete type
// is viewed through a non-first-parent interface (where slot layout differs).
func (c *Compiler) getOrEmitViewVtable(concrete, view *types.Named) *ir.Global {
	key := viewVtableKey{concrete, view}
	if vt, ok := c.viewVtables[key]; ok {
		return vt
	}

	// Synthesize default methods for this (concrete, view) pair
	if view.IsStructural() {
		c.synthesizeDefaultMethods(concrete, view)
	}

	methods := view.AllVirtualMethods()
	var entries []constant.Constant
	for _, m := range methods {
		ownerName := c.resolveMethodOwner(concrete, m.Name())
		mangledName := mangleMethodName(ownerName, m.Name(), m.IsSetter())
		if fn, ok := c.funcs[mangledName]; ok {
			entries = append(entries, constant.NewBitCast(fn, irtypes.I8Ptr))
		} else {
			entries = append(entries, constant.NewNull(irtypes.I8Ptr))
		}
	}
	arrayType := irtypes.NewArray(uint64(len(entries)), irtypes.I8Ptr)
	init := constant.NewArray(arrayType, entries...)
	name := fmt.Sprintf("promise_vtable_%s_as_%s", concrete.Obj().Name(), view.Obj().Name())
	global := c.module.NewGlobalDef(name, init)
	global.Immutable = true
	c.viewVtables[key] = global
	return global
}

// isInFirstParentChain returns true if target is reachable from concrete
// through the first parent only (always prefix-compatible vtable layout).
func isInFirstParentChain(concrete, target *types.Named) bool {
	for c := concrete; c != nil; {
		if c == target {
			return true
		}
		if len(c.Parents()) == 0 {
			return false
		}
		c = c.Parents()[0]
	}
	return false
}

// coerceToView swaps the vtable pointer in a value struct when the value crosses
// a type boundary to a non-first-parent view. For first parent chain coercion
// (prefix-compatible), the vtable is left unchanged.
func (c *Compiler) coerceToView(val value.Value, fromType, toType types.Type) value.Value {
	fromNamed := extractNamed(fromType)
	toNamed := extractNamed(toType)
	if fromNamed == nil || toNamed == nil {
		return val
	}
	if !c.isUserValueType(fromType) || !c.isUserValueType(toType) {
		return val
	}
	if fromNamed == toNamed {
		return val
	}

	// Guard: verify the LLVM value is actually a {i8*, i8*} struct before modifying it.
	// During monomorphization, type substitution can produce non-user-value LLVM types
	// even when the sema type passes isUserValueType.
	if st, ok := val.Type().(*irtypes.StructType); !ok || len(st.Fields) != 2 ||
		!st.Fields[0].Equal(irtypes.I8Ptr) || !st.Fields[1].Equal(irtypes.I8Ptr) {
		return val
	}

	// First parent chain → vtable is prefix-compatible, no swap needed
	if isInFirstParentChain(fromNamed, toNamed) {
		return val
	}

	// Need view-specific vtable (second+ parent or structural satisfaction)
	viewVtable := c.getOrEmitViewVtable(fromNamed, toNamed)
	vtablePtr := constant.NewBitCast(viewVtable, irtypes.I8Ptr)
	return c.block.NewInsertValue(val, vtablePtr, 0)
}

// coerceCallArgs applies coerceToView to each argument whose type differs from the
// parameter type. Returns a new slice (or the original if no coercion was needed).
func (c *Compiler) coerceCallArgs(argVals []value.Value, argTypes []types.Type, params []*types.Param) []value.Value {
	n := len(params)
	if n > len(argVals) {
		n = len(argVals)
	}
	coerced := false
	result := argVals
	for i := 0; i < n; i++ {
		v := c.coerceToView(argVals[i], argTypes[i], params[i].Type())
		if v != argVals[i] && !coerced {
			// Lazily copy so we don't allocate when no coercion is needed
			coerced = true
			result = make([]value.Value, len(argVals))
			copy(result, argVals)
		}
		if coerced {
			result[i] = v
		}
	}
	return result
}
