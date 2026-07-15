package codegen

import (
	"fmt"

	"github.com/llir/llvm/ir"
	"github.com/llir/llvm/ir/constant"
	"github.com/llir/llvm/ir/enum"
	irtypes "github.com/llir/llvm/ir/types"
	"github.com/llir/llvm/ir/value"

	"github.com/promise-language/promise/compiler/internal/ast"
	"github.com/promise-language/promise/compiler/internal/types"
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

// resolveTypeID returns the RTTI type ID for a types.Type that may be either
// a *types.Named (non-generic) or *types.Instance (generic instantiation).
// Returns the type ID and true if resolved, 0 and false otherwise.
func (c *Compiler) resolveTypeID(typ types.Type) (int32, bool) {
	switch t := typ.(type) {
	case *types.Named:
		return c.assignTypeID(t), true
	case *types.Instance:
		name := monoName(t)
		if id, ok := c.monoTypeIDs[name]; ok {
			return id, true
		}
		// Fall back: origin Named type (allows matching `x is Box` even for Box[int])
		if named, ok := t.Origin().(*types.Named); ok {
			return c.assignTypeID(named), true
		}
	}
	return 0, false
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

// collectMonoParentIDs recursively collects mono parent type IDs for a generic
// instance. For LabeledContainer[int] is Container[T], substitutes T→int to get
// Container[int] and includes its mono type ID. Recurses into grandparents.
func (c *Compiler) collectMonoParentIDs(named *types.Named, subst map[*types.TypeParam]types.Type, ids *[]int32) {
	for _, pr := range named.Parents() {
		if len(pr.TypeArgs) == 0 || len(pr.Named.TypeParams()) == 0 {
			continue
		}
		// Substitute type args: Container[T] with T→int → Container[int]
		concreteArgs := make([]types.Type, len(pr.TypeArgs))
		allConcrete := true
		for i, ta := range pr.TypeArgs {
			concreteArgs[i] = types.Substitute(ta, subst)
			if types.ContainsTypeParam(concreteArgs[i]) {
				allConcrete = false
			}
		}
		if !allConcrete {
			continue
		}
		parentInst := types.NewInstance(pr.Named, concreteArgs)
		parentMono := monoName(parentInst)
		if id, ok := c.monoTypeIDs[parentMono]; ok {
			*ids = append(*ids, id)
		}
		// Recurse into grandparents with a combined substitution
		parentSubst := types.BuildSubstMap(pr.Named.TypeParams(), concreteArgs)
		if parentSubst != nil {
			c.collectMonoParentIDs(pr.Named, parentSubst, ids)
		}
	}
}

// emitTypeInfo creates a global type info constant for a Named type.
// Layout: { i8* vtable_ptr, i8* drop_fn_ptr, i8* clone_fn_ptr, i32 type_id, i32 num_parents, [N x i32] parent_ids }
// B0226: drop_fn_ptr enables runtime dispatch to the correct drop function for
// untyped error catches (where the concrete error type isn't known at compile time).
// T0387: clone_fn_ptr enables runtime dispatch to the correct synthesized clone
// function in dupHeapValue, so polymorphic vector slicing produces independent
// copies of the concrete subtype (rather than truncating via the static layout).
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

	// Field 1: drop function pointer (B0226)
	fields = append(fields, c.resolveTypeInfoDropFn(named.Obj().Name(), named))

	// Field 2: clone function pointer (T0387)
	fields = append(fields, c.resolveTypeInfoCloneFn(named))

	fields = append(fields, constant.NewInt(irtypes.I32, int64(typeID)))
	fields = append(fields, constant.NewInt(irtypes.I32, int64(numParents)))

	if numParents > 0 {
		arrayType := irtypes.NewArray(uint64(numParents), irtypes.I32)
		structType = irtypes.NewStruct(irtypes.I8Ptr, irtypes.I8Ptr, irtypes.I8Ptr, irtypes.I32, irtypes.I32, arrayType)

		var parentConsts []constant.Constant
		for _, pid := range parentIDs {
			parentConsts = append(parentConsts, constant.NewInt(irtypes.I32, int64(pid)))
		}
		parentArray := constant.NewArray(arrayType, parentConsts...)
		fields = append(fields, parentArray)
	} else {
		structType = irtypes.NewStruct(irtypes.I8Ptr, irtypes.I8Ptr, irtypes.I8Ptr, irtypes.I32, irtypes.I32)
	}

	init := constant.NewStruct(structType, fields...)
	globalName := "promise_typeinfo_" + c.typeGlobalName(named)
	global := c.module.NewGlobalDef(globalName, init)
	global.Immutable = true
	return global
}

// resolveTypeInfoDropFn resolves the drop function pointer for a type's typeinfo.
// Returns a bitcast to i8* of the drop function, or null if no drop exists.
// B0226: Used to populate the typeinfo drop_fn_ptr field for runtime dispatch.
// B0247: For explicit user drops (not synthesized), returns a wrapper that calls
// drop + pal_free, since user drop functions don't free the instance themselves.
func (c *Compiler) resolveTypeInfoDropFn(ownerName string, named *types.Named) constant.Constant {
	if named.HasDrop() || named.NeedsSynthDrop() {
		resolvedOwner := ownerName
		explicitDrop := named.HasDrop() && !named.NeedsSynthDrop()
		if explicitDrop {
			resolvedOwner = c.resolveDropOwner(named)
		}
		mangledName := mangleMethodName(resolvedOwner, "drop", false)
		if fn, ok := c.funcs[mangledName]; ok {
			if explicitDrop {
				// B0247: Wrap explicit user drop with pal_free
				fn = c.getOrCreateDropWrap(mangledName, fn)
			}
			return constant.NewBitCast(fn, irtypes.I8Ptr)
		}
	}
	return constant.NewNull(irtypes.I8Ptr)
}

// resolveTypeInfoCloneFn resolves the clone function pointer for a type's typeinfo.
// Returns a bitcast to i8* of the synthesized clone function, or null if no clone
// fn was synthesized for this type.
// T0387: Used to populate the typeinfo clone_fn_ptr field for runtime dispatch in
// dupHeapValue when the static element type is polymorphic.
func (c *Compiler) resolveTypeInfoCloneFn(named *types.Named) constant.Constant {
	if fn, ok := c.typeCloneFns[named]; ok && fn != nil {
		return constant.NewBitCast(fn, irtypes.I8Ptr)
	}
	return constant.NewNull(irtypes.I8Ptr)
}

// maybeSynthesizeCloneFn synthesizes a per-type clone function for a concrete heap
// user type. The clone takes an i8* instance pointer and returns a freshly malloc'd
// i8* instance pointer with all droppable sub-fields independently dup'd.
// T0387: Stored in typeinfo at field 2 (clone_fn_ptr) so dupHeapValue can dispatch
// to the runtime concrete type rather than truncating via the static layout.
//
// owner is *types.Named (non-generic) or *types.Instance (mono). globalName is the
// IR symbol prefix (e.g. "Shape" or "Box__int"). layout/subst are nil for non-generic
// owners — looked up via lookupTypeLayout in that case.
func (c *Compiler) maybeSynthesizeCloneFn(named *types.Named, owner types.Type, globalName string, layout *TypeDeclLayout, subst map[*types.TypeParam]types.Type) {
	// Eligibility: only concrete heap user types. Abstract/value/copy/structural
	// types are skipped — none can reach dupHeapValue's polymorphic path.
	if named.IsAbstract() || named.IsValueType() || named.IsCopy() || named.IsStructural() {
		return
	}
	if isPrimitiveScalar(named) {
		return
	}
	// Opaque container types (Vector, Channel, Task, Arc, Weak, Mutex, MutexGuard)
	// are stored as raw i8* buffers — their dup goes through type-specific helpers
	// (dupVector, dupChannel, dupArc, dupWeak), not the synthesized clone path.
	if isOpaqueContainerType(owner) {
		return
	}
	// Skip generic origin types in the non-mono path; their mono instances handle clone.
	if owner == named && len(named.TypeParams()) > 0 {
		return
	}
	// Don't double-emit
	if owner == named {
		if _, exists := c.typeCloneFns[named]; exists {
			return
		}
	} else {
		if _, exists := c.monoTypeCloneFns[globalName]; exists {
			return
		}
	}

	// Resolve layout
	if layout == nil {
		layout = c.lookupTypeLayout(owner)
	}
	if layout == nil || layout.Instance == nil || layout.InstancePtrType == nil {
		return
	}
	// Only user types have heap layouts compatible with the synth clone path.
	// Strings, primitives, opaque containers, and enums use specialized clones.
	if layout.Kind != LayoutUserType {
		return
	}

	cloneFnName := globalName + ".__clone"
	if _, exists := c.funcs[cloneFnName]; exists {
		return
	}

	param := ir.NewParam("instance", irtypes.I8Ptr)
	fn := c.module.NewFunc(cloneFnName, irtypes.I8Ptr, param)
	c.funcs[cloneFnName] = fn
	if owner == named {
		c.typeCloneFns[named] = fn
	} else {
		c.monoTypeCloneFns[globalName] = fn
	}
	if c.compilingModule != "" {
		c.moduleOwnedFuncs[cloneFnName] = c.compilingModule
	}

	// Build body with full state save/restore so we can reuse dupHeapValueFields.
	saved := c.saveState()
	defer c.restoreState(saved)
	c.fn = fn
	c.locals = make(map[string]*ir.InstAlloca)
	c.localNameCount = make(map[string]int)
	c.typeSubst = subst
	if inst, ok := owner.(*types.Instance); ok {
		c.monoCtx = &monoContext{inst: inst, origin: named, name: globalName}
	} else {
		c.monoCtx = nil
	}

	entry := fn.NewBlock(".entry")
	c.block = entry
	c.entryBlock = entry

	// Null check on the input
	isNull := c.block.NewICmp(enum.IPredEQ, param, constant.NewNull(irtypes.I8Ptr))
	allocBlk := fn.NewBlock("clone.alloc")
	nullBlk := fn.NewBlock("clone.null")
	c.block.NewCondBr(isNull, nullBlk, allocBlk)

	nullBlk.NewRet(constant.NewNull(irtypes.I8Ptr))

	c.block = allocBlk
	instanceStructType := layout.Instance.LLVMType
	instancePtrType := layout.InstancePtrType

	// Compute size via GEP-from-null
	nullPtr := constant.NewNull(instancePtrType)
	sizePtr := c.block.NewGetElementPtr(instanceStructType, nullPtr,
		constant.NewInt(irtypes.I32, 1))
	sizeRaw := c.block.NewPtrToInt(sizePtr, c.ptrIntType())
	var size value.Value = sizeRaw
	if c.isWasm {
		size = c.block.NewZExt(sizeRaw, irtypes.I64)
	}

	newPtr := c.block.NewCall(c.palAlloc, size)
	c.block.NewCall(c.funcs["llvm.memcpy"], newPtr, param, size, constant.False)

	typedNewPtr := c.block.NewBitCast(newPtr, instancePtrType)
	c.dupHeapValueFields(named, owner, layout, typedNewPtr)
	c.block.NewRet(newPtr)
}

// getOrCreateDropWrap returns a wrapper function that calls the given drop function
// and then frees the instance via pal_free. Used for explicit user drops in typeinfo
// so that RTTI-based drop dispatch handles the full cleanup (B0247).
// Synthesized drops already include pal_free, so they don't need wrapping.
func (c *Compiler) getOrCreateDropWrap(dropName string, dropFn *ir.Func) *ir.Func {
	wrapName := dropName + "$wrap"
	if existing, ok := c.funcs[wrapName]; ok {
		return existing
	}
	wrapFn := c.module.NewFunc(wrapName, irtypes.Void,
		ir.NewParam("this", irtypes.I8Ptr))
	entry := wrapFn.NewBlock(".entry")
	entry.NewCall(dropFn, wrapFn.Params[0])
	entry.NewCall(c.palFree, wrapFn.Params[0])
	entry.NewRet(nil)
	c.funcs[wrapName] = wrapFn
	return wrapFn
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
		mangledName := mangleMethodNameForMethod(ownerName, m)
		// T0468/T0507: Prefer the child's own (possibly synthesized) implementation
		// when the method is inherited — otherwise virtual dispatch through the
		// vtable skips the child's cleanup. The T0507 inherited-drop synthesis adds
		// e.g. _NgBox.drop even though drop is owned by _NgLogger.
		if ownerName != named.Obj().Name() {
			ownMangled := mangleMethodNameForMethod(named.Obj().Name(), m)
			if _, ok := c.funcs[ownMangled]; ok {
				mangledName = ownMangled
			} else if _, ok := c.funcs[mangledName]; !ok {
				// Inherited method from a generic parent — resolve to mono name.
				monoOwner := c.resolveMonoParentName(named, named, ownerName)
				mangledName = mangleMethodNameForMethod(monoOwner, m)
			}
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
	// First pass: synthesize per-type clone functions so the typeinfo can
	// reference them (T0387). Done as a separate pass so all clone fns are
	// declared before any typeinfo emits, which lets typeinfo also reference
	// clone fns of other types (e.g. for nested polymorphic dispatch).
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
		c.maybeSynthesizeCloneFn(named, named, named.Obj().Name(), nil, nil)
	}

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

		// For value types: emit a global RTTI instance { variant_ptr } used for
		// RTTI queries. Accessed via lookupValueTypeRTTI (not stored in value struct).
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
				mangledName = mangleMethodNameForMethod(name, m)
			} else {
				// Inherited method — prefer the child's own mono name if it
				// exists (e.g. T0468 inherited-drop synthesis adds Box[int].drop
				// even though drop is owned by Logger). Falls back to the parent's
				// mono name otherwise. Without the child-first preference, virtual
				// dispatch through the vtable would skip the child's field cleanup.
				ownMangled := mangleMethodNameForMethod(name, m)
				if _, ok := c.funcs[ownMangled]; ok {
					mangledName = ownMangled
				} else {
					monoOwner := c.resolveMonoParentName(named, inst, ownerName)
					mangledName = mangleMethodNameForMethod(monoOwner, m)
					// Structural parents skip mono method generation — fall back to
					// concrete mono name where synthesized defaults are registered.
					if _, ok := c.funcs[mangledName]; !ok {
						mangledName = ownMangled
					}
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
// Uses two passes: first assigns all type IDs (so parent lookups succeed regardless
// of instance ordering), then emits the typeinfo globals.
func (c *Compiler) emitMonoTypeInfoGlobals(instances []*types.Instance) {
	// Pass 1: assign type IDs for all mono instances
	for _, inst := range instances {
		named, ok := inst.Origin().(*types.Named)
		if !ok || len(named.TypeParams()) == 0 {
			continue
		}
		name := monoName(inst)
		if _, exists := c.monoTypeInfoGlobals[name]; exists {
			continue
		}
		if _, exists := c.monoTypeIDs[name]; !exists {
			c.monoTypeIDs[name] = c.nextTypeID
			c.nextTypeID++
		}
	}

	// Pass 1b: synthesize per-mono clone functions so typeinfo can reference them (T0387).
	for _, inst := range instances {
		named, ok := inst.Origin().(*types.Named)
		if !ok || len(named.TypeParams()) == 0 {
			continue
		}
		name := monoName(inst)
		if _, exists := c.monoTypeInfoGlobals[name]; exists {
			continue
		}
		layout := c.monoLayouts[name]
		if layout == nil {
			continue
		}
		subst := types.BuildSubstMap(named.TypeParams(), inst.TypeArgs())
		c.maybeSynthesizeCloneFn(named, inst, name, layout, subst)
	}

	// Pass 2: emit typeinfo globals using the pre-assigned IDs
	for _, inst := range instances {
		named, ok := inst.Origin().(*types.Named)
		if !ok || len(named.TypeParams()) == 0 {
			continue
		}
		name := monoName(inst)
		if _, exists := c.monoTypeInfoGlobals[name]; exists {
			continue
		}

		typeID := c.monoTypeIDs[name]

		// Collect parent IDs. Include both origin Named IDs (for bare `x is Container`)
		// and mono instance IDs (for generic `x is Container[int]`).
		parentIDs := c.collectAllParentIDs(named)
		originID := c.assignTypeID(named)
		// Prepend origin ID (dedup handled below)
		parentIDs = append([]int32{originID}, parentIDs...)

		// Add mono parent type IDs: for LabeledContainer[int] is Container[T],
		// substitute T→int to get Container[int], then include its mono type ID.
		subst := types.BuildSubstMap(named.TypeParams(), inst.TypeArgs())
		if subst != nil {
			c.collectMonoParentIDs(named, subst, &parentIDs)
		}
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

		// Field 1: drop function pointer (B0226)
		// B0247: Explicit user drops get wrapped with pal_free.
		dropFnConst := constant.Constant(constant.NewNull(irtypes.I8Ptr))
		monoDropName := mangleMethodName(name, "drop", false)
		if fn, ok := c.funcs[monoDropName]; ok {
			if named.HasDrop() && !named.NeedsSynthDrop() {
				fn = c.getOrCreateDropWrap(monoDropName, fn)
			}
			dropFnConst = constant.NewBitCast(fn, irtypes.I8Ptr)
		} else {
			// Fall back to origin type's drop
			dropFnConst = c.resolveTypeInfoDropFn(named.Obj().Name(), named)
		}
		fields = append(fields, dropFnConst)

		// Field 2: clone function pointer (T0387)
		cloneFnConst := constant.Constant(constant.NewNull(irtypes.I8Ptr))
		if fn, ok := c.monoTypeCloneFns[name]; ok {
			cloneFnConst = constant.NewBitCast(fn, irtypes.I8Ptr)
		}
		fields = append(fields, cloneFnConst)

		fields = append(fields, constant.NewInt(irtypes.I32, int64(typeID)))
		fields = append(fields, constant.NewInt(irtypes.I32, int64(numParents)))

		if numParents > 0 {
			arrayType := irtypes.NewArray(uint64(numParents), irtypes.I32)
			structType = irtypes.NewStruct(irtypes.I8Ptr, irtypes.I8Ptr, irtypes.I8Ptr, irtypes.I32, irtypes.I32, arrayType)
			var parentConsts []constant.Constant
			for _, pid := range parentIDs {
				parentConsts = append(parentConsts, constant.NewInt(irtypes.I32, int64(pid)))
			}
			parentArray := constant.NewArray(arrayType, parentConsts...)
			fields = append(fields, parentArray)
		} else {
			structType = irtypes.NewStruct(irtypes.I8Ptr, irtypes.I8Ptr, irtypes.I8Ptr, irtypes.I32, irtypes.I32)
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
// Typeinfo layout: { i8* vtable_ptr, i8* drop_fn_ptr, i8* clone_fn_ptr, i32 type_id, i32 num_parents, [0 x i32] parent_ids }
func (c *Compiler) defineTypeIsFunc() {
	variantParam := ir.NewParam("variant_ptr", irtypes.I8Ptr)
	expectedParam := ir.NewParam("expected_id", irtypes.I32)
	fn := c.module.NewFunc("promise_type_is", irtypes.I32, variantParam, expectedParam)

	// Typeinfo struct type with flexible array member for parent_ids
	// B0226: field 1 is drop_fn_ptr.
	// T0387: field 2 is clone_fn_ptr, shifting type_id to field 3, etc.
	typeinfoType := irtypes.NewStruct(
		irtypes.I8Ptr,                    // field 0: vtable_ptr
		irtypes.I8Ptr,                    // field 1: drop_fn_ptr (B0226)
		irtypes.I8Ptr,                    // field 2: clone_fn_ptr (T0387)
		irtypes.I32,                      // field 3: type_id
		irtypes.I32,                      // field 4: num_parents
		irtypes.NewArray(0, irtypes.I32), // field 5: parent_ids (flexible)
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

	// check_id: load type_id (field 3), compare with expected_id
	typedPtr := checkID.NewBitCast(variantParam, irtypes.NewPointer(typeinfoType))
	tidPtr := checkID.NewGetElementPtr(typeinfoType, typedPtr, zero32, constant.NewInt(irtypes.I32, 3))
	typeID := checkID.NewLoad(irtypes.I32, tidPtr)
	match := checkID.NewICmp(enum.IPredEQ, typeID, expectedParam)
	checkID.NewCondBr(match, retTrueBlk, loopInit)

	// loop_init: load num_parents (field 4), check if > 0
	npPtr := loopInit.NewGetElementPtr(typeinfoType, typedPtr, zero32, constant.NewInt(irtypes.I32, 4))
	numParents := loopInit.NewLoad(irtypes.I32, npPtr)
	hasParents := loopInit.NewICmp(enum.IPredSGT, numParents, zero32)
	loopInit.NewCondBr(hasParents, loopHeader, retFalseBlk)

	// loop_header: phi node for index, bounds check
	iPhi := loopHeader.NewPhi(&ir.Incoming{X: zero32, Pred: loopInit})
	inBounds := loopHeader.NewICmp(enum.IPredSLT, iPhi, numParents)
	loopHeader.NewCondBr(inBounds, loopBody, retFalseBlk)

	// loop_body: load parent_ids[i] (field 5), compare, increment
	pidPtr := loopBody.NewGetElementPtr(typeinfoType, typedPtr,
		zero32, constant.NewInt(irtypes.I32, 5), iPhi)
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
		mangledName := mangleMethodNameForMethod(ownerName, m)
		// T0468: Prefer the concrete's mono name if it has its own (possibly
		// synthesized) implementation — virtual dispatch must reach the child's
		// override/synthesis, not the parent's directly.
		concreteMonoMangled := mangleMethodNameForMethod(concreteCacheKey, m)
		if _, ok := c.funcs[concreteMonoMangled]; ok && ownerName != concrete.Obj().Name() {
			mangledName = concreteMonoMangled
		} else if _, ok := c.funcs[mangledName]; !ok && ownerName != concrete.Obj().Name() {
			// Inherited method from a generic parent — resolve to mono name,
			// same fallback as emitVtableGlobal.
			monoOwner := c.resolveMonoParentName(concrete, fromType, ownerName)
			mangledName = mangleMethodNameForMethod(monoOwner, m)
		}
		// Also try the mono concrete name (e.g., Entity__int.method)
		if _, ok := c.funcs[mangledName]; !ok {
			if _, ok2 := c.funcs[concreteMonoMangled]; ok2 {
				mangledName = concreteMonoMangled
			}
		}
		if fn, ok := c.funcs[mangledName]; ok {
			// Check if the concrete method signature differs from the interface method
			// (extra optional/default params, non-failable→failable, T→T? return,
			// or primitive scalar receiver vs i8* receiver).
			// If so, generate an adapter thunk with the interface's signature.
			concreteMethod := c.lookupMethodForMethod(concrete, m)
			// T1280: a string concrete always needs an adapter — the receiver behind the
			// view is now the heap box { i8* typeinfo, i8* string_ptr }, so the raw
			// string.method (which expects the string ptr as `this`) cannot be used
			// directly; the adapter loads field 1 from the box first.
			needsAdapter := concreteMethod != nil && (needsViewAdapter(concreteMethod.Sig(), m.Sig()) || isPrimitiveScalar(concrete) || concrete == types.TypString)
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
		params = append(params, ir.NewParam(fmt.Sprintf("p%d", i), c.resolveParamType(p)))
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
			// T1276: The primitive box is { i8* typeinfo, scalarT } (heap-allocated by
			// boxForStructuralView), so load the scalar receiver from field 1.
			scalarType := llvmNamedType(concreteType)
			boxType := irtypes.NewStruct(irtypes.I8Ptr, scalarType)
			typedPtr := c.block.NewBitCast(params[paramIdx], irtypes.NewPointer(boxType))
			scalarField := c.block.NewGetElementPtr(boxType, typedPtr,
				constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 1))
			scalar := c.block.NewLoad(scalarType, scalarField)
			args = append(args, scalar)
		} else if concreteType == types.TypString {
			// T1280: The string box is { i8* typeinfo, i8* string_ptr } (heap-allocated by
			// boxForStructuralView), so load the string pointer receiver from field 1.
			boxType := irtypes.NewStruct(irtypes.I8Ptr, irtypes.I8Ptr)
			typedPtr := c.block.NewBitCast(params[paramIdx], irtypes.NewPointer(boxType))
			strField := c.block.NewGetElementPtr(boxType, typedPtr,
				constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 1))
			strPtr := c.block.NewLoad(irtypes.I8Ptr, strField)
			args = append(args, strPtr)
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
			// Compile the default expression — check local ParamDefaults first,
			// then fall back to the default stored on the param (cross-module).
			if defExpr, ok := c.info.ParamDefaults[p]; ok {
				args = append(args, c.genExpr(defExpr))
				continue
			}
			if raw := p.DefaultExpr(); raw != nil {
				if defExpr, ok := raw.(ast.Expr); ok {
					args = append(args, c.genExpr(defExpr))
					continue
				}
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
	ifaceResultUnwrapped := ifaceResult
	if concreteResult != nil && ifaceResult != nil {
		if ifaceOpt, isOpt := ifaceResult.(*types.Optional); isOpt {
			ifaceResultUnwrapped = ifaceOpt.Elem()
			if !types.Identical(concreteResult, ifaceResult) {
				needsOptWrap = true
			}
		}
	}

	// Check if we need covariant return coercion (ConcreteType → StructuralInterface)
	// This must happen before optional/failable wrapping.
	needsCovariantCoerce := false
	if concreteResult != nil && ifaceResultUnwrapped != nil {
		if ifaceRetNamed, ok := ifaceResultUnwrapped.(*types.Named); ok && ifaceRetNamed.IsAbstract() && ifaceRetNamed.IsStructural() {
			concreteRetUnwrapped := concreteResult
			if concreteOpt, ok := concreteRetUnwrapped.(*types.Optional); ok {
				concreteRetUnwrapped = concreteOpt.Elem()
			}
			if !types.Identical(concreteRetUnwrapped, ifaceResultUnwrapped) {
				needsCovariantCoerce = true
			}
		}
	}

	// Apply covariant return coercion (swap vtable to structural view).
	// When both concrete and interface are failable, we must extract the inner value
	// from the failable result, coerce it, then rebuild the failable tuple.
	var retResult value.Value = result
	if needsCovariantCoerce {
		if concreteCanError && ifaceCanError {
			// Both failable: extract inner value {i8*, i8*} from {i1, {i8*, i8*}, i8*}
			innerVal := c.block.NewExtractValue(result, 1)
			coerced := c.coerceToView(innerVal, concreteResult, ifaceResultUnwrapped)
			retResult = c.block.NewInsertValue(result, coerced, 1)
		} else {
			retResult = c.coerceToView(result, concreteResult, ifaceResultUnwrapped)
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
			innerVal = c.wrapSome(retResult, c.resolveType(concreteResult))
		} else {
			innerVal = retResult
		}
		retVal := c.wrapSuccessResult(innerVal, ifaceRetType.(*irtypes.StructType))
		c.block.NewRet(retVal)
	} else if needsOptWrap {
		// T → T?: wrap as some(T)
		optVal := c.wrapSome(retResult, c.resolveType(concreteResult))
		c.block.NewRet(optVal)
	} else if concreteResult == nil && !concreteCanError {
		c.block.NewRet(nil)
	} else {
		c.block.NewRet(retResult)
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

	// Opaque container types (Vector, Channel, Task) are user types with i8*
	// representation (no value struct). Box them like primitives/string when
	// targeting a structural interface they actually implement.
	if isOpaqueContainerType(fromType) && toNamed.IsStructural() &&
		fromNamed != toNamed && types.Implements(fromNamed, toNamed) {
		return c.boxForStructuralView(val, fromNamed, toNamed, fromType)
	}

	if !c.isUserValueType(fromType) || !c.isUserValueType(toType) {
		return val
	}

	// Pure value type → structural interface: the value struct is wider than {i8*, i8*}.
	// Stack-allocate the value, store it, and create a view with {vtable, &alloca}.
	if fromNamed.IsValueType() && toNamed.IsStructural() {
		return c.boxValueTypeForStructuralView(val, fromNamed, toNamed, fromType)
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

// isMaterializedViewPtr reports whether val is a pointer to a two-field view struct
// ({i8*, i8*}). This is the shape genCallArgsWithMutRef produces for a `~` (MutRef)
// param — it already coerced the arg into a view and stored it in a temp alloca, then
// passes the temp pointer. coerceCallArgs then runs a second coercion pass over the
// same argVals; without this guard the box helpers would re-box that pointer (T1276).
// Distinct from a string/opaque i8* (pointer to i8, not to a 2-field struct).
func isMaterializedViewPtr(val value.Value) bool {
	pt, ok := val.Type().(*irtypes.PointerType)
	if !ok {
		return false
	}
	st, ok := pt.ElemType.(*irtypes.StructType)
	return ok && len(st.Fields) == 2 &&
		st.Fields[0].Equal(irtypes.I8Ptr) && st.Fields[1].Equal(irtypes.I8Ptr)
}

// boxForStructuralView boxes a primitive or string value into a structural interface
// view ({i8*, i8*}) when the target is a structural interface.
// For primitives: heap-allocates a { typeinfo, scalar } box and creates {vtable, box}.
// For string (T1280): heap-allocates a { typeinfo, string_ptr } box holding an owned
// deep clone and creates {vtable, box}, so the box drops cleanly on escape.
// For opaque containers: creates {vtable, i8*} directly (the raw pointer).
func (c *Compiler) boxForStructuralView(val value.Value, fromNamed, toNamed *types.Named, fromType types.Type) value.Value {
	// Only box when target is a structural interface
	if !toNamed.IsStructural() {
		return val
	}
	// Skip void/none
	if fromNamed == types.TypVoid || fromNamed == types.TypNone {
		return val
	}
	// T1276: already-materialized `~`-param view — don't re-box.
	if isMaterializedViewPtr(val) {
		return val
	}

	// Get view vtable for concrete → structural interface
	viewVtable := c.getOrEmitViewVtable(fromNamed, toNamed, fromType)
	vtablePtr := constant.NewBitCast(viewVtable, irtypes.I8Ptr)

	// Create the instance pointer
	var instancePtr value.Value
	if isPrimitiveScalar(fromNamed) {
		// T1276: Heap-allocate a box { i8* typeinfo, scalarT } (not a stack alloca)
		// so the interface fat pointer can escape its defining frame safely. Field 0
		// carries a shared null-drop typeinfo header so __promise_structural_drop
		// pal_free's the box (primitives never drop); the scalar receiver lives in
		// field 1 (see emitViewMethodAdapter). The malloc is tracked as an owned heap
		// temp so the correct owner frees it exactly once.
		scalarType := llvmNamedType(fromNamed)
		boxType := irtypes.NewStruct(irtypes.I8Ptr, scalarType)
		size := constant.NewInt(irtypes.I64, int64(c.typeSize(boxType)))
		raw := c.block.NewCall(c.palAlloc, size)
		typed := c.block.NewBitCast(raw, irtypes.NewPointer(boxType))
		tiField := c.block.NewGetElementPtr(boxType, typed,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
		c.block.NewStore(constant.NewBitCast(c.getNoValueTypeInfo(), irtypes.I8Ptr), tiField)
		scalarField := c.block.NewGetElementPtr(boxType, typed,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 1))
		c.block.NewStore(val, scalarField)
		c.trackHeapTemp(raw, c.palFree)
		instancePtr = raw
	} else if fromNamed == types.TypString {
		// T1280: Heap-box the string as { i8* typeinfo, i8* string_ptr }. Field 1 is an
		// OWNED deep clone (dupString) so the box owns its payload independently of the
		// caller's string temp — no aliasing, no use-after-return dangle. Field 0 carries
		// a dedicated typeinfo (@promise_typeinfo_stringbox) whose drop_fn
		// (@__promise_string_box_drop) drops the cloned string via promise_string_drop
		// (honoring the rodata literal flag), then pal_free's the box. That real drop_fn
		// makes every RTTI drop site (local free, moved-param free, struct/enum-field
		// drop) work uniformly through __promise_structural_drop. The malloc is tracked as
		// an owned heap temp so exactly one owner frees it.
		cloned := c.dupString(val)
		boxType := irtypes.NewStruct(irtypes.I8Ptr, irtypes.I8Ptr)
		size := constant.NewInt(irtypes.I64, int64(c.typeSize(boxType)))
		raw := c.block.NewCall(c.palAlloc, size)
		typed := c.block.NewBitCast(raw, irtypes.NewPointer(boxType))
		tiField := c.block.NewGetElementPtr(boxType, typed,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
		c.block.NewStore(constant.NewBitCast(c.getStringBoxTypeInfo(), irtypes.I8Ptr), tiField)
		strField := c.block.NewGetElementPtr(boxType, typed,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 1))
		c.block.NewStore(cloned, strField)
		c.trackHeapTemp(raw, c.getStringBoxDrop())
		instancePtr = raw
	} else {
		// Other i8* types (opaque containers): already an i8* pointer
		instancePtr = val
	}

	// Construct the view struct: { vtable_ptr, instance_ptr }
	viewType := userValueType()
	result := constant.NewZeroInitializer(viewType)
	tmp := c.block.NewInsertValue(result, vtablePtr, 0)
	return c.block.NewInsertValue(tmp, instancePtr, 1)
}

// boxValueTypeForStructuralView boxes a pure value type into a structural interface
// view ({i8*, i8*}). Value types have wider value structs (e.g., {vtable, x, y})
// that don't fit in the standard {i8*, i8*} layout.
//
// T1276: The box must be HEAP-allocated (not a stack alloca) because the resulting
// interface fat pointer can escape its defining frame (return, store into a longer-
// lived location). A stack box would dangle (use-after-return) and its drop would
// pal_free a stack address ("invalid free"). We mirror the heap-user-type convention:
// pal_alloc the value struct, then overwrite field 0 (the otherwise-dead value-struct
// vtable slot — method dispatch uses the view vtable in the fat pointer, and value-type
// methods only read fields at index >= 1) with the concrete typeinfo pointer so
// __promise_structural_drop dispatches correctly: value types have a null drop_fn, so
// it falls through to pal_free(box). The malloc is registered as an owned heap temp so
// the correct owner frees it exactly once (transfers to the caller on return/binding,
// freed at statement end for borrow args).
func (c *Compiler) boxValueTypeForStructuralView(val value.Value, fromNamed, toNamed *types.Named, fromType types.Type) value.Value {
	// T1276: already-materialized `~`-param view (pointer to {i8*, i8*}) — the arg was
	// boxed and stored into a temp by genCallArgsWithMutRef; the second coerceCallArgs
	// pass must not re-box it.
	if isMaterializedViewPtr(val) {
		return val
	}
	// Get view vtable for value type → structural interface
	viewVtable := c.getOrEmitViewVtable(fromNamed, toNamed, fromType)
	vtablePtr := constant.NewBitCast(viewVtable, irtypes.I8Ptr)

	// Heap-allocate the value type struct and store the value.
	valType := val.Type()
	size := constant.NewInt(irtypes.I64, int64(c.typeSize(valType)))
	raw := c.block.NewCall(c.palAlloc, size)
	typed := c.block.NewBitCast(raw, irtypes.NewPointer(valType))
	c.block.NewStore(val, typed)

	// Repurpose field 0 (value-struct vtable slot) as the RTTI typeinfo header so
	// __promise_structural_drop reads a null drop_fn and pal_free's the wrapper.
	// Prefer the concrete typeinfo (also makes `is` checks on the box resolve), but
	// fall back to the shared null-drop header — value types never drop, so pal_free is
	// always correct and we must never leave the value's own vtable in field 0 (that
	// would make the drop path misread a bogus drop_fn).
	var tiPtr constant.Constant = constant.NewBitCast(c.getNoValueTypeInfo(), irtypes.I8Ptr)
	if ti := c.lookupTypeInfoGlobal(fromType); ti != nil {
		tiPtr = constant.NewBitCast(ti, irtypes.I8Ptr)
	}
	field0 := c.block.NewGetElementPtr(valType, typed,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
	c.block.NewStore(tiPtr, field0)

	// Register the heap box as an owned temp (freed via pal_free by whichever owner
	// keeps it — the tracked drop func is pal_free, dispatched via RTTI at drop sites).
	c.trackHeapTemp(raw, c.palFree)

	// Construct the view struct: { vtable_ptr, instance_ptr }
	viewType := userValueType()
	result := constant.NewZeroInitializer(viewType)
	tmp := c.block.NewInsertValue(result, vtablePtr, 0)
	return c.block.NewInsertValue(tmp, raw, 1)
}

// coerceCallArgs applies optional wrapping (T→T?) and view coercion to each
// argument whose type differs from the parameter type.
// args is the AST argument list (may be nil); used to clear drop flags when
// wrapping droppable values into optionals (B0358).
// callSubst (T0418) maps the callee's TypeParams to the call's concrete type
// args; applied BEFORE c.typeSubst so generic params like T? resolve correctly
// at the call site even when the outer mono context doesn't cover them.
// Returns a new slice (or the original if no coercion was needed).
func (c *Compiler) coerceCallArgs(argVals []value.Value, argTypes []types.Type, params []*types.Param, args []*ast.Arg, callSubst map[*types.TypeParam]types.Type) []value.Value {
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
		if callSubst != nil {
			paramType = types.Substitute(paramType, callSubst)
		}
		if c.typeSubst != nil {
			paramType = types.Substitute(paramType, c.typeSubst)
		}
		if _, isOpt := paramType.(*types.Optional); isOpt {
			argType := argTypes[i]
			if callSubst != nil && argType != nil {
				argType = types.Substitute(argType, callSubst)
			}
			if c.typeSubst != nil && argType != nil {
				argType = types.Substitute(argType, c.typeSubst)
			}
			// Use Identical (not "is argOpt?") so a T? arg targeting a T?? param
			// still gets wrapped to match the param's depth.
			if !types.Identical(argType, paramType) {
				lt := c.resolveType(paramType)
				if xn, ok := argType.(*types.Named); ok && xn == types.TypNone {
					// none → T?: produce zeroinitializer
					v = c.zeroValue(lt)
				} else if st, ok := lt.(*irtypes.StructType); ok {
					// B0358: The value is moving into the optional — transfer
					// ownership so the caller doesn't double-drop at scope exit.
					// T1188: Only transfer ownership for a `move` (RefMut) param.
					// The callee's prologue registers an optional drop and frees
					// the payload at its scope exit, so the caller must release
					// it. For a borrow param (RefNone/RefShared) the callee only
					// reads the widened optional and never drops it, so the
					// caller must retain the temp/local and drop it at its own
					// scope exit — exactly like the non-optional borrow arg
					// `g(D(x:1))`, which never claims the temp either.
					if args != nil && i < len(args) && params[i].Ref() == types.RefMut {
						if ident, ok := args[i].Value.(*ast.IdentExpr); ok {
							if argTypeIsDroppable(argType) {
								c.clearDropFlag(ident.Name)
							}
						}
						c.claimStringTemp(v)
						c.claimHeapTemp(v)
					}
					// T → T?: wrap as some (or T? → T??)
					v = c.wrapOptional(v, st)
				}
			}
		}

		// View coercion (structural interface vtable swap, or boxing for primitives/string)
		v = c.coerceToView(v, argTypes[i], params[i].Type())

		// T1276: A value/primitive coerced into a fresh structural-interface box for a
		// `move` (RefMut, plain type) param transfers ownership to the callee, which
		// frees the box via its maybeRegisterStructuralParamFree binding. Claim the box
		// so the caller's statement-end cleanup doesn't also free it (double free). `~`
		// (MutRef-typed) params arrive already materialized (coerceToView returns the
		// arg unchanged) so v == argVals[i] and nothing is claimed — the callee borrows.
		if v != argVals[i] && params[i].Ref() == types.RefMut {
			c.claimHeapTemp(v)
		}

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
