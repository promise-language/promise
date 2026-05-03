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
	for _, pr := range named.Parents() {
		ids = append(ids, c.assignTypeID(pr.Named))
		ids = append(ids, c.collectAllParentIDs(pr.Named)...)
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
	globalName := "promise_typeinfo_" + c.typeGlobalName(named)
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
		if _, ok := c.funcs[mangledName]; !ok && ownerName != named.Obj().Name() {
			// Inherited method from a generic parent — resolve to mono name.
			monoOwner := c.resolveMonoParentName(named, named, ownerName)
			mangledName = mangleMethodName(monoOwner, m.Name(), m.IsSetter())
		}
		if fn, ok := c.funcs[mangledName]; ok {
			entries = append(entries, constant.NewBitCast(fn, irtypes.I8Ptr))
		} else {
			// Abstract method with no body — store null
			entries = append(entries, constant.NewNull(irtypes.I8Ptr))
		}
	}
	arrayType := irtypes.NewArray(uint64(len(entries)), irtypes.I8Ptr)
	init := constant.NewArray(arrayType, entries...)
	global := c.module.NewGlobalDef("promise_vtable_"+c.typeGlobalName(named), init)
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
		tiGlobal := c.emitTypeInfo(named)
		c.typeInfoGlobals[named] = tiGlobal

		// For value types: emit a global RTTI instance { variant_ptr } that the
		// value struct stores in field 1. loadVariantPtr loads from this to get RTTI.
		if named.IsValueType() {
			layout := c.layouts[named]
			if layout != nil {
				instanceStructType := layout.Instance.LLVMType
				variantFieldType := layout.Instance.Fields[0].LLVMType
				rttiInit := constant.NewStruct(instanceStructType,
					constant.NewBitCast(tiGlobal, variantFieldType))
				rttiGlobal := c.module.NewGlobalDef("promise_rtti_"+c.typeGlobalName(named), rttiInit)
				rttiGlobal.Immutable = true
				c.valueTypeRTTI[named] = rttiGlobal
			}
		}
	}
}

// computeMonoVtableInfo marks mono instance origin types that have children
// among the mono instances. For example, if ConstProducer[int] is Producer[int],
// Producer needs to be marked as having children so its vtable slots are recognized.
func (c *Compiler) computeMonoVtableInfo(instances []*types.Instance) {
	for _, inst := range instances {
		named, ok := inst.Origin().(*types.Named)
		if !ok || len(named.TypeParams()) == 0 {
			continue
		}
		// Walk parent chain and mark each parent origin as having children
		var markParents func(n *types.Named)
		markParents = func(n *types.Named) {
			for _, pr := range n.Parents() {
				c.hasChildren[pr.Named] = true
				markParents(pr.Named)
			}
		}
		markParents(named)
	}
}

// emitMonoVtableGlobals creates vtable globals for monomorphic type instances
// that have virtual methods. Each mono instance gets its own vtable with
// method pointers resolved to the mono-specialized functions.
//
// Called twice: once before module compilation (may produce null entries for
// module-owned types whose methods aren't declared yet) and once inside
// compileModule (methods now declared). The second call updates any null
// entries filled in by the module's method declarations.
func (c *Compiler) emitMonoVtableGlobals(instances []*types.Instance) {
	for _, inst := range instances {
		named, ok := inst.Origin().(*types.Named)
		if !ok || len(named.TypeParams()) == 0 {
			continue
		}
		name := monoName(inst)
		methods := named.AllVirtualMethods()
		if len(methods) == 0 {
			continue
		}
		var entries []constant.Constant
		for _, m := range methods {
			ownerName := c.resolveMethodOwner(named, m.Name())
			// Resolve to mono method name: Owner__typeargs.method
			var mangledName string
			if ownerName == named.Obj().Name() {
				// Method defined on this type — use mono instance name
				mangledName = mangleMethodName(name, m.Name(), m.IsSetter())
			} else {
				// Inherited method — resolve through mono parent chain
				monoOwner := c.resolveMonoParentName(named, inst, ownerName)
				mangledName = mangleMethodName(monoOwner, m.Name(), m.IsSetter())
				// Structural parents skip mono method generation — fall back to
				// concrete mono name where synthesized defaults are registered.
				if _, ok := c.funcs[mangledName]; !ok {
					mangledName = mangleMethodName(name, m.Name(), m.IsSetter())
				}
			}
			if fn, ok := c.funcs[mangledName]; ok {
				entries = append(entries, constant.NewBitCast(fn, irtypes.I8Ptr))
			} else {
				entries = append(entries, constant.NewNull(irtypes.I8Ptr))
			}
		}
		arrayType := irtypes.NewArray(uint64(len(entries)), irtypes.I8Ptr)
		init := constant.NewArray(arrayType, entries...)
		if existing, exists := c.monoVtableGlobals[name]; exists {
			// Update the existing vtable with newly available function pointers.
			// This handles module-owned generic types (e.g. Map[K,V]) whose
			// methods weren't declared when the vtable was first created.
			existing.Init = init
			continue
		}
		global := c.module.NewGlobalDef("promise_vtable_"+name, init)
		global.Immutable = true
		c.monoVtableGlobals[name] = global
	}
}

// emitMonoTypeInfoGlobals creates type info globals for monomorphic type instances.
// Each mono instance gets its own typeinfo with a unique type ID and parent IDs.
func (c *Compiler) emitMonoTypeInfoGlobals(instances []*types.Instance) {
	for _, inst := range instances {
		named, ok := inst.Origin().(*types.Named)
		if !ok || len(named.TypeParams()) == 0 {
			continue
		}
		name := monoName(inst)
		if _, exists := c.monoTypeInfoGlobals[name]; exists {
			continue
		}

		// Assign a unique type ID for this mono instance
		typeID := c.nextTypeID
		c.nextTypeID++

		// Collect parent IDs (using origin Named types, same as non-mono).
		// Include the origin Named type's own ID so that `x is OriginName`
		// matches when x holds a mono instance (e.g., `b is LabeledBox`
		// where b holds LabeledBox[int]).
		parentIDs := c.collectAllParentIDs(named)
		originID := c.assignTypeID(named)
		// Prepend origin ID (dedup handled below)
		parentIDs = append([]int32{originID}, parentIDs...)
		// Deduplicate in case origin ID was already in parent chain
		seen := make(map[int32]bool)
		var deduped []int32
		for _, id := range parentIDs {
			if !seen[id] {
				seen[id] = true
				deduped = append(deduped, id)
			}
		}
		parentIDs = deduped
		numParents := len(parentIDs)

		var structType *irtypes.StructType
		var fields []constant.Constant

		// Field 0: vtable pointer (from mono vtable globals)
		if vtGlobal, ok := c.monoVtableGlobals[name]; ok && vtGlobal != nil {
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
		tiGlobal := c.module.NewGlobalDef("promise_typeinfo_"+name, init)
		tiGlobal.Immutable = true
		c.monoTypeInfoGlobals[name] = tiGlobal

		// For value types: emit a global RTTI instance
		if named.IsValueType() {
			layout := c.monoLayouts[name]
			if layout != nil {
				instanceStructType := layout.Instance.LLVMType
				variantFieldType := layout.Instance.Fields[0].LLVMType
				rttiInit := constant.NewStruct(instanceStructType,
					constant.NewBitCast(tiGlobal, variantFieldType))
				rttiGlobal := c.module.NewGlobalDef("promise_rtti_"+name, rttiInit)
				rttiGlobal.Immutable = true
				c.monoValueTypeRTTI[name] = rttiGlobal
			}
		}
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
	entry := fn.NewBlock(".entry")
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
// fromType is the full concrete type (may be *types.Instance for generic types),
// needed to resolve monomorphized parent method names.
func (c *Compiler) getOrEmitViewVtable(concrete, view *types.Named, fromType types.Type) *ir.Global {
	// Build cache key that distinguishes mono instances (Entity[int] vs Entity[string]).
	concreteCacheKey := concrete.Obj().Name()
	if inst, ok := fromType.(*types.Instance); ok {
		concreteCacheKey = monoName(inst)
	}
	key := viewVtableKey{concreteCacheKey, view}
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
		if _, ok := c.funcs[mangledName]; !ok && ownerName != concrete.Obj().Name() {
			// Inherited method from a generic parent — resolve to mono name,
			// same fallback as emitVtableGlobal.
			monoOwner := c.resolveMonoParentName(concrete, fromType, ownerName)
			mangledName = mangleMethodName(monoOwner, m.Name(), m.IsSetter())
		}
		// Also try the mono concrete name (e.g., Entity__int.method)
		if _, ok := c.funcs[mangledName]; !ok {
			monoMangledName := mangleMethodName(concreteCacheKey, m.Name(), m.IsSetter())
			if _, ok2 := c.funcs[monoMangledName]; ok2 {
				mangledName = monoMangledName
			}
		}
		if fn, ok := c.funcs[mangledName]; ok {
			// Check if the concrete method signature differs from the interface method
			// (extra optional/default params, non-failable→failable, T→T? return,
			// or primitive scalar receiver vs i8* receiver).
			// If so, generate an adapter thunk with the interface's signature.
			concreteMethod := c.lookupAnyMethod(concrete, m.Name(), m.IsGetter(), m.IsSetter())
			needsAdapter := concreteMethod != nil && (needsViewAdapter(concreteMethod.Sig(), m.Sig()) || isPrimitiveScalar(concrete))
			if needsAdapter {
				adapter := c.emitViewMethodAdapter(concrete, concreteMethod, m, fn)
				entries = append(entries, constant.NewBitCast(adapter, irtypes.I8Ptr))
			} else {
				entries = append(entries, constant.NewBitCast(fn, irtypes.I8Ptr))
			}
		} else {
			entries = append(entries, constant.NewNull(irtypes.I8Ptr))
		}
	}
	arrayType := irtypes.NewArray(uint64(len(entries)), irtypes.I8Ptr)
	init := constant.NewArray(arrayType, entries...)
	name := fmt.Sprintf("promise_vtable_%s_as_%s", concreteCacheKey, view.Obj().Name())
	global := c.module.NewGlobalDef(name, init)
	global.Immutable = true
	c.viewVtables[key] = global
	return global
}

// needsViewAdapter reports whether the concrete method's signature differs from
// the interface method's in ways that require an adapter thunk in the vtable:
//   - concrete has more params than iface (extra optional/default params)
//   - concrete is non-failable but iface is failable
//   - concrete returns T but iface returns T?
func needsViewAdapter(concrete, iface *types.Signature) bool {
	if len(concrete.Params()) != len(iface.Params()) {
		return true
	}
	if !concrete.CanError() && iface.CanError() {
		return true
	}
	if concrete.Result() != nil && iface.Result() != nil {
		if !types.Identical(concrete.Result(), iface.Result()) {
			return true
		}
	}
	return false
}

// emitViewMethodAdapter generates a thunk function with the interface method's LLVM
// signature that forwards to the concrete method, supplying default values for extra
// params and wrapping the return if needed (non-failable→failable, T→T?).
func (c *Compiler) emitViewMethodAdapter(
	concreteType *types.Named,
	concreteMethod, ifaceMethod *types.Method,
	concreteFn *ir.Func,
) *ir.Func {
	ifaceSig := ifaceMethod.Sig()
	concreteSig := concreteMethod.Sig()

	// Build adapter function type matching the interface method's signature
	adapterName := fmt.Sprintf("%s.%s$view_adapt", concreteType.Obj().Name(), ifaceMethod.Name())

	var params []*ir.Param
	if ifaceSig.Recv() != nil {
		params = append(params, ir.NewParam("this", irtypes.I8Ptr))
	}
	for i, p := range ifaceSig.Params() {
		params = append(params, ir.NewParam(fmt.Sprintf("p%d", i), c.resolveType(p.Type())))
	}

	ifaceRetType := irtypes.Type(irtypes.Void)
	if ifaceSig.Result() != nil {
		ifaceRetType = c.resolveType(ifaceSig.Result())
	}
	if ifaceSig.CanError() {
		ifaceRetType = computeResultType(ifaceRetType)
	}

	fn := c.module.NewFunc(adapterName, ifaceRetType, params...)

	// Save compiler state
	saved := c.saveState()
	defer c.restoreState(saved)

	c.fn = fn
	c.locals = make(map[string]*ir.InstAlloca)
	c.localNameCount = make(map[string]int)

	entry := fn.NewBlock(".entry")
	c.block = entry
	c.entryBlock = entry

	// Build arguments for the concrete method call
	var args []value.Value
	paramIdx := 0

	// Forward receiver
	if concreteSig.Recv() != nil {
		if isPrimitiveScalar(concreteType) {
			// Primitive receiver: load scalar from i8* pointer
			scalarType := llvmNamedType(concreteType)
			typedPtr := c.block.NewBitCast(params[paramIdx], irtypes.NewPointer(scalarType))
			scalar := c.block.NewLoad(scalarType, typedPtr)
			args = append(args, scalar)
		} else {
			args = append(args, params[paramIdx])
		}
		paramIdx++
	}

	// Forward interface params
	for i := 0; i < len(ifaceSig.Params()); i++ {
		args = append(args, params[paramIdx])
		paramIdx++
	}

	// Supply defaults for extra concrete params
	for i := len(ifaceSig.Params()); i < len(concreteSig.Params()); i++ {
		p := concreteSig.Params()[i]
		pType := c.resolveType(p.Type())

		if p.HasDefault() {
			// Compile the default expression
			if defExpr, ok := c.info.ParamDefaults[p]; ok {
				args = append(args, c.genExpr(defExpr))
				continue
			}
		}
		// Optional type or fallback: pass zeroinitializer (none)
		args = append(args, constant.NewZeroInitializer(pType))
	}

	// Call the concrete method
	result := c.block.NewCall(concreteFn, args...)

	// Handle return type adaptation
	concreteCanError := concreteSig.CanError()
	ifaceCanError := ifaceSig.CanError()
	concreteResult := concreteSig.Result()
	ifaceResult := ifaceSig.Result()

	// Check if we need optional wrapping (T → T?)
	needsOptWrap := false
	if concreteResult != nil && ifaceResult != nil {
		if _, isOpt := ifaceResult.(*types.Optional); isOpt {
			if !types.Identical(concreteResult, ifaceResult) {
				needsOptWrap = true
			}
		}
	}

	if !concreteCanError && ifaceCanError {
		// Non-failable → failable: wrap result as success {i1 false, T, i8* null}
		var innerVal value.Value
		if concreteResult == nil {
			// void → void!: just {i1 false, i8* null}
			innerVal = nil
		} else if needsOptWrap {
			// T → T?!: wrap T as some(T) first, then as success
			innerVal = c.wrapSome(result, c.resolveType(concreteResult))
		} else {
			innerVal = result
		}
		retVal := c.wrapSuccessResult(innerVal, ifaceRetType.(*irtypes.StructType))
		c.block.NewRet(retVal)
	} else if needsOptWrap {
		// T → T?: wrap as some(T)
		optVal := c.wrapSome(result, c.resolveType(concreteResult))
		c.block.NewRet(optVal)
	} else if concreteResult == nil && !concreteCanError {
		c.block.NewRet(nil)
	} else {
		c.block.NewRet(result)
	}

	return fn
}

// wrapSome wraps a value T as some(T) = {i1 true, T}.
func (c *Compiler) wrapSome(val value.Value, innerType irtypes.Type) value.Value {
	optType := irtypes.NewStruct(irtypes.I1, innerType)
	optVal := constant.NewZeroInitializer(optType)
	tmp := c.block.NewInsertValue(optVal, constant.True, 0)
	return c.block.NewInsertValue(tmp, val, 1)
}

// wrapSuccessResult wraps a value as a failable success: {i1 false, T, i8* null}.
// For void results (innerVal == nil), produces {i1 false, i8* null}.
func (c *Compiler) wrapSuccessResult(innerVal value.Value, resultType *irtypes.StructType) value.Value {
	result := constant.NewZeroInitializer(resultType)
	// i1 false is already zero-initialized
	if innerVal != nil {
		tmp := c.block.NewInsertValue(result, innerVal, 1)
		return c.block.NewInsertValue(tmp, constant.NewNull(irtypes.I8Ptr), uint64(len(resultType.Fields)-1))
	}
	return c.block.NewInsertValue(result, constant.NewNull(irtypes.I8Ptr), uint64(len(resultType.Fields)-1))
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
		c = c.Parents()[0].Named
	}
	return false
}

// coerceToView swaps the vtable pointer in a value struct when the value crosses
// a type boundary to a non-first-parent view. For first parent chain coercion
// (prefix-compatible), the vtable is left unchanged.
// Also handles boxing of primitives and strings into structural interface views.
func (c *Compiler) coerceToView(val value.Value, fromType, toType types.Type) value.Value {
	fromNamed := extractNamed(fromType)
	toNamed := extractNamed(toType)
	if fromNamed == nil || toNamed == nil {
		return val
	}
	if fromNamed == toNamed {
		return val
	}

	// Non-user-value types (primitives, string) → structural interface: box into view
	if !c.isUserValueType(fromType) && c.isUserValueType(toType) {
		return c.boxForStructuralView(val, fromNamed, toNamed, fromType)
	}

	if !c.isUserValueType(fromType) || !c.isUserValueType(toType) {
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
	viewVtable := c.getOrEmitViewVtable(fromNamed, toNamed, fromType)
	vtablePtr := constant.NewBitCast(viewVtable, irtypes.I8Ptr)
	return c.block.NewInsertValue(val, vtablePtr, 0)
}

// boxForStructuralView boxes a primitive or string value into a structural interface
// view ({i8*, i8*}) when the target is a structural interface.
// For primitives: stack-allocates the scalar and creates {vtable, &scalar}.
// For string: creates {vtable, string_ptr} (string is already i8*).
func (c *Compiler) boxForStructuralView(val value.Value, fromNamed, toNamed *types.Named, fromType types.Type) value.Value {
	// Only box when target is a structural interface
	if !toNamed.IsStructural() {
		return val
	}
	// Skip void/none
	if fromNamed == types.TypVoid || fromNamed == types.TypNone {
		return val
	}

	// Get view vtable for concrete → structural interface
	viewVtable := c.getOrEmitViewVtable(fromNamed, toNamed, fromType)
	vtablePtr := constant.NewBitCast(viewVtable, irtypes.I8Ptr)

	// Create the instance pointer
	var instancePtr value.Value
	if isPrimitiveScalar(fromNamed) {
		// Alloca the scalar on stack, store, bitcast to i8*
		scalarType := llvmNamedType(fromNamed)
		alloca := c.entryBlock.NewAlloca(scalarType)
		c.block.NewStore(val, alloca)
		instancePtr = c.block.NewBitCast(alloca, irtypes.I8Ptr)
	} else {
		// String and other i8* types: already an i8* pointer
		instancePtr = val
	}

	// Construct the view struct: { vtable_ptr, instance_ptr }
	viewType := userValueType()
	result := constant.NewZeroInitializer(viewType)
	tmp := c.block.NewInsertValue(result, vtablePtr, 0)
	return c.block.NewInsertValue(tmp, instancePtr, 1)
}

// coerceCallArgs applies optional wrapping (T→T?) and view coercion to each
// argument whose type differs from the parameter type.
// Returns a new slice (or the original if no coercion was needed).
func (c *Compiler) coerceCallArgs(argVals []value.Value, argTypes []types.Type, params []*types.Param) []value.Value {
	n := len(params)
	if n > len(argVals) {
		n = len(argVals)
	}
	coerced := false
	result := argVals
	for i := 0; i < n; i++ {
		v := argVals[i]

		// Optional wrapping: param is T? but arg is not optional
		paramType := params[i].Type()
		if c.typeSubst != nil {
			paramType = types.Substitute(paramType, c.typeSubst)
		}
		if _, isOpt := paramType.(*types.Optional); isOpt {
			argType := argTypes[i]
			if c.typeSubst != nil && argType != nil {
				argType = types.Substitute(argType, c.typeSubst)
			}
			_, argIsOpt := argType.(*types.Optional)
			if !argIsOpt {
				lt := c.resolveType(paramType)
				if xn, ok := argType.(*types.Named); ok && xn == types.TypNone {
					// none → T?: produce zeroinitializer
					v = c.zeroValue(lt)
				} else if st, ok := lt.(*irtypes.StructType); ok {
					// T → T?: wrap as some
					v = c.wrapOptional(v, st)
				}
			}
		}

		// View coercion (structural interface vtable swap, or boxing for primitives/string)
		v = c.coerceToView(v, argTypes[i], params[i].Type())

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
