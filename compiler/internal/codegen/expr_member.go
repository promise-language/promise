package codegen

import (
	"fmt"

	"github.com/llir/llvm/ir/constant"
	irtypes "github.com/llir/llvm/ir/types"
	"github.com/llir/llvm/ir/value"

	"github.com/promise-language/promise/compiler/internal/ast"
	"github.com/promise-language/promise/compiler/internal/types"
)

// --- Member access ---

// genMemberExpr generates a field access on a user type instance or an enum variant value.
func (c *Compiler) genMemberExpr(e *ast.MemberExpr) value.Value {
	// T0993: non-destructive enum variant field read — `if x is V { x.namedField }`.
	// Sema recorded the variant + field index; emit a variant-data GEP+load.
	if access := c.info.NarrowedVariantField[e]; access != nil {
		return c.genNarrowedVariantField(e, access)
	}

	// Module-level getter: mod.property → call getter function with no args.
	// Guard: only intercept when sema actually resolved this member as a getter
	// (recorded in info.ModuleGetters). Keying on sema's resolution rather than
	// the result type's shape is required because a getter whose return type is
	// itself a function type (`get adder() -> int`) has a Signature result type
	// — the old "result is a Signature ⇒ function reference" heuristic
	// misclassified it, fell through to the type-based path, and panicked on the
	// module target's nil type (T1240).
	if _, isGetter := c.info.ModuleGetters[e]; isGetter {
		if ident, ok := e.Target.(*ast.IdentExpr); ok {
			if modName := c.resolveModuleName(ident); modName != "" {
				return c.genModuleGetterCall(e, modName, e.Field)
			}
		}
	}

	targetType := c.info.Types[e.Target]
	// Apply typeSubst for mono context
	if c.typeSubst != nil {
		targetType = types.Substitute(targetType, c.typeSubst)
	}
	if c.selfSubst != nil {
		targetType = types.SubstituteSelf(targetType, c.selfSubst.iface, c.selfSubst.concrete)
	}
	// T0381: unwrap SharedRef/MutRef so member dispatch sees the underlying
	// type. The runtime representation is identical (same pointer / value
	// struct), so all the type-based getter/method lookups below operate on
	// the owned form.
	if sr, ok := targetType.(*types.SharedRef); ok {
		targetType = sr.Elem()
	}
	if mr, ok := targetType.(*types.MutRef); ok {
		targetType = mr.Elem()
	}

	// Container .len property (string, vector, fixed array)
	// Check both Instance wrappers (user code: Vector[int]) and bare Named (method body: this is TypVector)
	if e.Field == "len" {
		if arr, ok := targetType.(*types.Array); ok {
			return constant.NewInt(irtypes.I64, arr.Size())
		}
		named := extractNamed(targetType)
		if named == types.TypString {
			return c.genStringLen(e)
		}
		if _, ok := types.AsVector(targetType); ok || named == types.TypVector {
			return c.genVectorLen(e)
		}
	}

	// Arc .borrow getter — returns the inner T value by loading from the Arc allocation.
	// T0155: Ref[T] atomic reference counting.
	if e.Field == "borrow" {
		if elem, ok := types.AsArc(targetType); ok {
			resolvedElem := elem
			if c.typeSubst != nil {
				resolvedElem = types.Substitute(elem, c.typeSubst)
			}
			return c.genArcBorrow(e, resolvedElem)
		}
		named := extractNamed(targetType)
		if named == types.TypArc {
			if tp := c.resolveTypeParam(types.TypArc.TypeParams()[0]); tp != nil {
				return c.genArcBorrow(e, tp)
			}
		}
		// MutexGuard .borrow getter — loads T through the guard's mutex pointer.
		// T0156: MutexGuard[T] interior mutability.
		if elem, ok := types.AsMutexGuard(targetType); ok {
			resolvedElem := elem
			if c.typeSubst != nil {
				resolvedElem = types.Substitute(elem, c.typeSubst)
			}
			return c.genMutexGuardBorrow(e, resolvedElem)
		}
		if named == types.TypMutexGuard {
			if tp := c.resolveTypeParam(types.TypMutexGuard.TypeParams()[0]); tp != nil {
				return c.genMutexGuardBorrow(e, tp)
			}
		}
	}

	// String .is_literal property — checks sign bit of length field
	if e.Field == "is_literal" {
		named := extractNamed(targetType)
		if named == types.TypString {
			return c.genStringIsLiteral(e)
		}
	}

	// Native hash getter for Hashable interface on primitive types
	if e.Field == "hash" {
		named := extractNamed(targetType)
		if named != nil {
			if v, ok := c.genNativeHashGetter(e, named); ok {
				return v
			}
		}
	}

	// Native bits getter: f64.bits/f32.bits returns IEEE 754 bit pattern
	if e.Field == "bits" {
		named := extractNamed(targetType)
		if named == types.TypF64 {
			target := c.genExprAutoPropagate(e.Target) // B0323
			return c.block.NewBitCast(target, irtypes.I64)
		}
		if named == types.TypF32 {
			target := c.genExprAutoPropagate(e.Target) // B0323
			return c.block.NewBitCast(target, irtypes.I32)
		}
	}

	// Enum variant access: Color.Red or Option[int].None
	// Check variant first; if the field is not a variant, check for enum getters.
	if enumLayout := c.lookupEnumLayout(targetType); enumLayout != nil {
		if _, isVariant := enumLayout.VariantTag[e.Field]; isVariant {
			return c.genEnumVariantValueLayout(enumLayout, e.Field)
		}
		// Not a variant — check for enum getter
		if result, ok := c.genEnumGetterAccess(e, targetType, enumLayout); ok {
			return result
		}
	}

	// For generic enum variants (e.g. Slot.Empty inside a generic type body),
	// the target type is a bare *types.Enum but the result type is an Instance
	// after mono substitution. Use the result type to find the layout.
	if _, ok := targetType.(*types.Enum); ok {
		resultType := c.info.Types[e]
		if c.typeSubst != nil {
			resultType = types.Substitute(resultType, c.typeSubst)
		}
		if enumLayout := c.lookupEnumLayout(resultType); enumLayout != nil {
			if _, isVariant := enumLayout.VariantTag[e.Field]; isVariant {
				return c.genEnumVariantValueLayout(enumLayout, e.Field)
			}
			if result, ok := c.genEnumGetterAccess(e, targetType, enumLayout); ok {
				return result
			}
		}
	}

	named := extractNamed(targetType)
	if named == nil {
		panic(fmt.Sprintf("codegen: cannot resolve type for member access on %T", targetType))
	}

	field := named.LookupField(e.Field)
	if field != nil {
		return c.genFieldAccess(e, targetType, field)
	}

	// Getter property: emit a method call with no args beyond receiver
	g := named.LookupGetter(e.Field)
	if g == nil && c.selfSubst != nil {
		// T1600: Inside a synthesized structural default-method body, `this`
		// (Self → concrete) may read a *sibling* default getter that is declared
		// on the interface, not on the concrete type's own method table. Resolve
		// it through the interface and ensure the per-concrete synthesized
		// function exists. Mirrors the T0766 method-call fallback in genMethodCall.
		if ig := c.selfSubst.iface.LookupGetter(e.Field); ig != nil {
			c.ensureDefaultMethodsSynthesized(c.selfSubst.concrete, c.selfSubst.iface)
			g = ig
		}
	}
	if g != nil {
		return c.genGetterCall(e, targetType, named, g)
	}

	panic(fmt.Sprintf("codegen: member %s on type %s is not a field (method references not yet supported)", e.Field, named))
}

// genNativeHashGetter emits native hash computation for primitive types.
// Returns (value, true) if the type has a native hash getter, (nil, false) otherwise.
// All primitive hashes use the Promise-implemented _fnv1a_hash function.
// String hash uses a codegen-emitted LLVM IR function (__promise_hash_string).
func (c *Compiler) genNativeHashGetter(e *ast.MemberExpr, named *types.Named) (value.Value, bool) {
	target := c.genExprAutoPropagate(e.Target) // B0323
	hashFn := c.funcs["_fnv1a_hash"]
	switch named {
	case types.TypInt, types.TypI64, types.TypUint, types.TypU64:
		// Already i64 — call _fnv1a_hash directly
		return c.block.NewCall(hashFn, target), true
	case types.TypI32:
		ext := c.block.NewSExt(target, irtypes.I64)
		return c.block.NewCall(hashFn, ext), true
	case types.TypU32:
		ext := c.block.NewZExt(target, irtypes.I64)
		return c.block.NewCall(hashFn, ext), true
	case types.TypI16:
		ext := c.block.NewSExt(target, irtypes.I64)
		return c.block.NewCall(hashFn, ext), true
	case types.TypU16:
		ext := c.block.NewZExt(target, irtypes.I64)
		return c.block.NewCall(hashFn, ext), true
	case types.TypI8:
		ext := c.block.NewSExt(target, irtypes.I64)
		return c.block.NewCall(hashFn, ext), true
	case types.TypU8:
		ext := c.block.NewZExt(target, irtypes.I64)
		return c.block.NewCall(hashFn, ext), true
	case types.TypBool:
		// Hardcoded hash constants for bool (avoids hashing through fnv1a)
		trueHash := constant.NewInt(irtypes.I64, 0x517cc1b727220a95)
		falseHash := constant.NewInt(irtypes.I64, 0x6c62272e07bb0142)
		return c.block.NewSelect(target, trueHash, falseHash), true
	case types.TypI128, types.TypU128:
		return c.block.NewCall(hashFn, c.foldWideToI64(target, 128)), true
	case types.TypI256, types.TypU256:
		return c.block.NewCall(hashFn, c.foldWideToI64(target, 256)), true
	case types.TypI512, types.TypU512:
		return c.block.NewCall(hashFn, c.foldWideToI64(target, 512)), true
	case types.TypChar:
		ext := c.block.NewZExt(target, irtypes.I64)
		return c.block.NewCall(hashFn, ext), true
	case types.TypF64:
		// Bitcast double to i64 bits, then hash via Promise _fnv1a_hash
		bits := c.block.NewBitCast(target, irtypes.I64)
		return c.block.NewCall(hashFn, bits), true
	case types.TypF32:
		// Bitcast float to i32 bits, zero-extend to i64, then hash
		bits := c.block.NewBitCast(target, irtypes.I32)
		ext := c.block.NewZExt(bits, irtypes.I64)
		return c.block.NewCall(hashFn, ext), true
	case types.TypString:
		// String hash uses codegen-emitted LLVM IR function
		return c.block.NewCall(c.funcs["__promise_hash_string"], target), true
	default:
		return nil, false
	}
}

// ownerHasOrSynthDrop returns true if the field owner needs cleanup at drop
// time. Covers explicit drop, sema-detected synth drop, and mono-detected synth
// drop on generic instances (T0513): the Named origin has HasDrop=false for
// generic types like Box[T] { T? value } because sema's fieldTypeHasDrop
// returns false for TypeParam fields. The concrete instance (e.g. Box[string])
// gets a synthesized drop via monoInstNeedsSynthDrop at codegen time, so the
// dup-on-read paths must also check that signal.
func (c *Compiler) ownerHasOrSynthDrop(typ types.Type, named *types.Named) bool {
	if named != nil && (named.HasDrop() || named.NeedsSynthDrop()) {
		return true
	}
	if inst, ok := typ.(*types.Instance); ok {
		return monoInstNeedsSynthDrop(inst)
	}
	// T0778: Inside a monomorphized method body the receiver type can surface as
	// the bare generic Named (e.g. GH[T]) rather than a concrete Instance, so the
	// Instance branch above is skipped. NeedsSynthDrop on the generic Named is
	// false — its field types still contain TypeParams (sema's fieldTypeHasDrop
	// returns false for TypeParam). Resolve through the active mono context: if
	// `named` is the origin of the instance currently being specialized, ask
	// whether THAT instance needs a synth drop (its substituted fields are
	// droppable). Without this, a borrowed-field read in a generic method whose
	// field substitutes to a droppable type (`return this.s`, `this.o!`,
	// `(this.o)? _ {...}` for string/vector) skips the field-access dup, so the
	// owner's synth drop and the returned alias both free the inner → double-free
	// (`fatal: invalid free`). Mirrors the monoCtx fallback in lookupTypeLayout.
	if c.monoCtx != nil && c.monoCtx.inst != nil && named != nil {
		if origin, ok := c.monoCtx.origin.(*types.Named); ok && origin == named {
			return monoInstNeedsSynthDrop(c.monoCtx.inst)
		}
	}
	return false
}

// genInstanceFieldSlot evaluates a member expression's owner EXACTLY ONCE and
// returns a pointer to the named field's slot inside the owner's instance struct.
// Shared by genFieldAccess (the read) and vectorFieldSlot (the T0990 write-back
// capture) so a Vector-field mutation stores the relocated buffer into the very
// instance it read the old buffer from — re-deriving the slot from a second
// evaluation of an impure owner (`make().items.push(x)`) wrote the grown buffer
// into a different object, leaving the first one holding a freed pointer.
func (c *Compiler) genInstanceFieldSlot(e *ast.MemberExpr, typ types.Type, layout *TypeDeclLayout, fieldIdx int) value.Value {
	targetVal := c.genExprAutoPropagate(e.Target) // B0323
	// `this` in methods is already an i8* instance pointer, not a value struct
	var instance value.Value
	if isThisReceiver(e.Target) {
		instance = targetVal
	} else {
		instance = c.extractInstancePtr(targetVal)
		// B0325: Track heap instance when target is a temporary (call result,
		// error unwrap). Without this, field access on temporaries like
		// make_pair().x or make_pair()?!.x leaks the instance.
		c.trackChainIntermediateReceiver(e.Target, targetVal, instance, extractNamed(typ), typ)
	}
	typedPtr := c.block.NewBitCast(instance, layout.InstancePtrType)

	return c.block.NewGetElementPtr(layout.Instance.LLVMType, typedPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(fieldIdx)))
}

// genFieldAccess loads a field value from a user type instance.
// Uses lookupTypeLayout for layout-driven field types that work for both
// regular and monomorphic types.
func (c *Compiler) genFieldAccess(e *ast.MemberExpr, typ types.Type, field *types.Field) value.Value {
	layout := c.lookupTypeLayout(typ)
	if layout == nil {
		panic(fmt.Sprintf("codegen: no layout for type %s", typ))
	}

	// Value types: fields are in the value struct, not an instance struct
	if layout.IsValueType {
		fieldIdx, ok := layout.ValueFieldIndex[field.Name()]
		if !ok {
			panic(fmt.Sprintf("codegen: field %s not in value layout for %s", field.Name(), typ))
		}
		targetVal := c.genExprAutoPropagate(e.Target) // B0323
		// `this` in value type methods is an i8* pointing to value struct
		if isThisReceiver(e.Target) {
			valuePtrType := irtypes.NewPointer(layout.Value.LLVMType)
			typedPtr := c.block.NewBitCast(targetVal, valuePtrType)
			fieldPtr := c.block.NewGetElementPtr(layout.Value.LLVMType, typedPtr,
				constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(fieldIdx)))
			return c.block.NewLoad(layout.Value.Fields[fieldIdx].LLVMType, fieldPtr)
		}
		// For non-this targets, the value is the full value struct — extractvalue
		return c.block.NewExtractValue(targetVal, uint64(fieldIdx))
	}

	fieldIdx, ok := layout.InstanceFieldIndex[field.Name()]
	if !ok {
		panic(fmt.Sprintf("codegen: field %s not in instance layout for %s", field.Name(), typ))
	}

	fieldPtr := c.genInstanceFieldSlot(e, typ, layout, fieldIdx)

	// Use layout field type (not llvmType(field.Type()) which fails for TypeParams)
	val := c.block.NewLoad(layout.Instance.Fields[fieldIdx].LLVMType, fieldPtr)

	// T0095/T0110: Dup string fields from types with drop to prevent double-free.
	// Only dup when the caller needs ownership (VarDecl, block result, etc.),
	// signaled by c.dupStringFieldAccess. Temporary uses (comparisons, function
	// args) don't dup — the type is alive during the expression evaluation.
	if c.tempTrackingEnabled {
		fType := field.Type()
		if c.typeSubst != nil {
			fType = types.Substitute(fType, c.typeSubst)
		}
		// T0513: Also substitute using the Instance's TypeArgs so generic field
		// types like T? on Box[T] resolve to string? when accessed on Box[string].
		// Without this, the dup checks below see TypeParam and skip the dup,
		// leaving the field aliased between the owner and the new variable.
		if inst, ok := typ.(*types.Instance); ok {
			if origin, ok := inst.Origin().(*types.Named); ok && len(origin.TypeParams()) > 0 {
				localSubst := types.BuildSubstMap(origin.TypeParams(), inst.TypeArgs())
				// T1215: An inherited generic field (e.g. `T? value` inherited
				// from `Box[T]` into `Counter[T] is Box[T]`) carries the PARENT's
				// type param (Box.T), which the child-only subst above does not
				// cover — so the field type stayed an unresolved TypeParam, the
				// escape dup was skipped, and the unwrapped binding aliased the
				// field. The owner's drop and the binding's drop then double-freed
				// the heap value ("fatal: invalid free (bad header magic)" on
				// macOS). Merge the inherited type-param mappings so the parent's
				// param resolves transitively (Box.T → Counter.T → string).
				mergeParentSubst(origin, localSubst)
				fType = types.Substitute(fType, localSubst)
			}
		}
		ownerNamed := extractNamed(typ)
		ownerDroppable := c.ownerHasOrSynthDrop(typ, ownerNamed)
		// Dup string/container fields from a droppable owner when an escape flag is
		// set (shared with the narrowed-enum-variant path, T1011).
		if dup, ok := c.dupHeapFieldForEscape(val, fType, ownerDroppable); ok {
			return dup
		}
		// B0190: Signal that this field access loaded a string? field from a
		// droppable type. genOptionalForceUnwrap's result should NOT be tracked
		// as a temp (the owner's drop handles the string's lifetime).
		if opt, ok := fType.(*types.Optional); ok && extractNamed(opt.Elem()) == types.TypString && ownerDroppable {
			c.optionalFieldString = true
		}
		// T0354: Same for vector fields — the owner's drop frees the inner Vector
		// via optfield.drop. Suppress unwrap-path stmt-temp tracking to avoid
		// double-free at statement end.
		if opt, ok := fType.(*types.Optional); ok && ownerDroppable {
			if _, isVec := types.AsVector(opt.Elem()); isVec {
				c.optionalFieldVector = true
			}
		}
	}

	return val
}

// --- ThisExpr ---

func (c *Compiler) genThisExpr() value.Value {
	alloca, ok := c.locals["this"]
	if !ok {
		panic("codegen: 'this' used but not in method context")
	}
	return c.block.NewLoad(alloca.ElemType, alloca)
}

// currentNamedType returns the concrete receiver type of the method currently
// being compiled, resolving generic type parameters. Prefers the mono instance
// (generic-owner method bodies) so `Box[int]` resolves rather than the unbound
// `Box[T]`; otherwise substitutes any active typeSubst into the owner Named.
// Returns nil outside a method context. Used by genGoBlock (T1219) to snapshot
// `this` for a `go { }` block capture.
func (c *Compiler) currentNamedType() types.Type {
	if c.monoCtx != nil && c.monoCtx.inst != nil {
		return c.monoCtx.inst
	}
	if c.currentNamed == nil {
		return nil
	}
	if c.typeSubst != nil {
		return types.Substitute(c.currentNamed, c.typeSubst)
	}
	return c.currentNamed
}

// genGetterCall emits a call to a getter method (zero args beyond receiver).
// Uses virtual dispatch through the vtable when the static type needs it.
// memberReceiver returns the receiver value for a getter/setter member access,
// consuming a value staged by genMemberCompoundAssign (T1353) when present so a
// compound `a.b += x` evaluates the receiver exactly once and target-first. When
// nothing is staged it evaluates the receiver via the site's own fallback.
func (c *Compiler) memberReceiver(fallback func() value.Value) value.Value {
	if c.stagedMemberReceiver != nil {
		v := c.stagedMemberReceiver
		c.stagedMemberReceiver = nil
		return v
	}
	return fallback()
}

func (c *Compiler) genGetterCall(e *ast.MemberExpr, targetType types.Type, named *types.Named, getter *types.Method) value.Value {
	// Virtual dispatch for getter when static type needs vtable. A `global getter
	// has no receiver — it is a static call on the type name, with no instance to
	// load a vtable pointer from — so it always dispatches directly (T1749).
	if c.needsVtable(named) && !getter.IsNative() && getter.Sig().Recv() != nil {
		return c.genVirtualGetterCall(e, named, getter, targetType)
	}

	// Own declaration → the receiver's (mono) name; a structural interface's
	// default getter → still the concrete name, because defaults are synthesized
	// per-concrete (T1559); a non-structural parent → the parent's mono name
	// (T0637). resolveDirectDispatchOwnerBy is the one implementation of that
	// three-case choice, shared with genMethodCall / genSetterCall — passing
	// LookupGetter because LookupMethod skips getters by design.
	mangledName := mangleMethodName(
		c.resolveDirectDispatchOwnerBy(named, targetType, e.Field, (*types.Named).LookupGetter), e.Field, false)

	fn, ok := c.funcs[mangledName]
	if !ok {
		panic(fmt.Sprintf("codegen: undeclared getter %s", mangledName))
	}

	if getter.Sig().Recv() == nil {
		// `global getter: no receiver argument (T1749).
		result := c.block.NewCall(fn)
		c.trackGetterResult(e, getter, targetType, result)
		return result
	}

	var args []value.Value
	target := c.memberReceiver(func() value.Value { return c.genExprAutoPropagate(e.Target) }) // B0323 / T1353
	if isThisReceiver(e.Target) {
		args = append(args, target)
	} else if isContainerType(targetType) {
		args = append(args, target)
	} else if isPrimitiveScalar(named) {
		args = append(args, target)
	} else if named.IsValueType() {
		args = append(args, c.valueTypeReceiverPtr(target, targetType))
	} else {
		instancePtr := c.extractInstancePtr(target)
		args = append(args, instancePtr)
		// B0258: Track getter chain intermediate for cleanup at statement end.
		c.trackChainIntermediateReceiver(e.Target, target, instancePtr, named, targetType)
	}

	result := c.block.NewCall(fn, args...)
	c.trackGetterResult(e, getter, targetType, result)
	return result
}

// genVirtualGetterCall emits an indirect getter call through the vtable.
func (c *Compiler) genVirtualGetterCall(e *ast.MemberExpr, named *types.Named, getter *types.Method, targetType types.Type) value.Value {
	receiverVal := c.memberReceiver(func() value.Value { return c.genExprAutoPropagate(e.Target) }) // B0323 / T1353

	var vtableRaw, instance value.Value
	if isThisReceiver(e.Target) {
		instance = receiverVal
		vtableRaw = c.loadVtablePtrFromInstance(receiverVal)
	} else {
		vtableRaw = c.extractVtablePtr(receiverVal)
		instance = c.extractInstancePtr(receiverVal)
		// B0258: Track getter chain intermediate for cleanup at statement end.
		c.trackChainIntermediateReceiver(e.Target, receiverVal, instance, named, targetType)
	}

	slotIndex := named.VirtualMethodIndex(e.Field, false) // getter, not setter
	if slotIndex < 0 {
		panic(fmt.Sprintf("codegen: getter %s not in vtable for %s", e.Field, named))
	}
	vtablePtr := c.block.NewBitCast(vtableRaw, irtypes.NewPointer(irtypes.I8Ptr))
	fnSlotPtr := c.block.NewGetElementPtr(irtypes.I8Ptr, vtablePtr,
		constant.NewInt(irtypes.I32, int64(slotIndex)))
	fnRaw := c.block.NewLoad(irtypes.I8Ptr, fnSlotPtr)

	// Substitute type params for generic instances (e.g. Transformer[int]).
	// T0418: include parent-type params so inherited getters resolve correctly.
	vtableSubst := c.buildOwnerTypeArgSubst(targetType)
	resolveVtableType := func(t types.Type) irtypes.Type {
		if vtableSubst != nil {
			t = types.Substitute(t, vtableSubst)
		}
		return c.resolveType(t)
	}

	retType := irtypes.Type(irtypes.Void)
	if getter.Sig().Result() != nil {
		retType = resolveVtableType(getter.Sig().Result())
	}
	if getter.Sig().CanError() {
		retType = computeResultType(retType)
	}
	paramTypes := []irtypes.Type{irtypes.I8Ptr}
	funcType := irtypes.NewFunc(retType, paramTypes...)
	fnTyped := c.block.NewBitCast(fnRaw, irtypes.NewPointer(funcType))

	result := c.block.NewCall(fnTyped, instance)
	c.trackGetterResult(e, getter, targetType, result)
	return result
}

// trackGetterResult registers a getter return value for cleanup at statement end.
// T0494: extends the original B0290 sliver (which only handled string results)
// to cover every droppable return type — string, vector, map, and user heap
// types — mirroring the tracking pattern in genExpr's *ast.CallExpr case.
// Without this, getter results in for-in iterable position (e.g.
// `for k,v in resp.headers`), method-chain receiver position (e.g.
// `resp.headers.contains(k)`), or any other dropped/expression position leak
// because no scope binding owns the cloned heap allocation.
//
// String/Vector i8* results dispatch to trackStringTemp / trackVectorTemp.
// {i8*, i8*} value-struct results (Map, Set, user heap types) dispatch to
// trackHeapUserTypeResult, which already filters out value/copy/structural
// types and primitives so calling it unconditionally is safe.
//
// targetType is the receiver type at the call site. It supplies owner-type
// substitution (e.g., ArcCell[int].fresh's `Ref[T]` → `Ref[int]`) so the
// per-element-type drop function looks up the concrete instantiation rather
// than the unsubstituted TypeParam.
func (c *Compiler) trackGetterResult(e *ast.MemberExpr, getter *types.Method, targetType types.Type, result value.Value) {
	if !c.tempTrackingEnabled || result == nil {
		return
	}
	// T1253/T1160: An instance getter whose return type is a function type
	// (`get adder() -> int`) yields an owned closure whose heap env must be
	// freed. Track its env (field 1 of the {fn,env} fat pointer) as an env temp
	// so cleanupEnvTemps frees it when the result is discarded (e.g. `(l.adder)()`
	// or `l.adder();`); if it's bound to a variable, claimEnvTemp releases the
	// temp and maybeRegisterEnvFree takes over ownership (single free either way).
	// Mirrors the module-getter arm in genModuleGetterCall (T1240).
	//
	// T1160: a getter can also hand back a closure it does NOT own — `get callback()
	// -> int { return this.cb; }` returns an alias of the receiver's field, whose env
	// the receiver's drop frees. Tracking that env here would free it twice. The
	// shared alias filter suppresses tracking for those receivers; the cost is the
	// pre-existing leak for a genuinely-fresh closure returned by such a type (T1229),
	// never a double free.
	if typ := c.info.Types[e]; typ != nil {
		if _, isSig := typ.(*types.Signature); isSig {
			if !c.closureResultMayAliasCallInput(e) {
				envPtr := c.block.NewExtractValue(result, 1)
				c.trackEnvTemp(envPtr)
			}
			return
		}
	}
	retType := getter.Sig().Result()
	// Owner-type subst: when the getter's owner is a generic instance
	// (e.g. ArcCell[int]), resolve the owner's TypeParams against the
	// instance's TypeArgs before applying any further substitution.
	// Without this, Ref[T] from ArcCell[T].fresh's signature stays as
	// Ref[T] and getOrCreateArcDrop(T) would produce an Ref[T].drop fn
	// that doesn't know T's concrete layout/inner-drop.
	if ownerSubst := c.buildOwnerTypeArgSubst(targetType); ownerSubst != nil && retType != nil {
		retType = types.Substitute(retType, ownerSubst)
	}
	if c.typeSubst != nil && retType != nil {
		retType = types.Substitute(retType, c.typeSubst)
	}
	if c.selfSubst != nil && retType != nil {
		retType = types.SubstituteSelf(retType, c.selfSubst.iface, c.selfSubst.concrete)
	}
	// T1559: a default getter inherited from a *generic* structural interface has
	// return type `T`. When the concrete implementor is a plain Named (not a
	// generic Instance), the interface's TypeParam→concrete mapping lives in the
	// implementor's parent ref, which none of the ambient substitutions above
	// carry — so retType can stay an unresolved TypeParam and an owned heap result
	// (e.g. a returned string) would never be registered for drop, leaking it.
	// Sema already resolved this member expression to its concrete type; use it.
	if _, isTP := retType.(*types.TypeParam); isTP {
		if semaType := c.info.Types[e]; semaType != nil {
			retType = semaType
		}
	}
	c.trackGetterResultByType(e, retType, result)
}

// trackGetterResultByType registers a getter result value for statement-end
// cleanup based on its (already substitution-resolved) result type, covering
// every heap-owning result kind that is passed by value: string, vector,
// channel, Arc/Weak/Mutex/Task, and heap user type. Callers must handle
// Signature (closure-env) results themselves before calling. Shared by
// trackGetterResult (instance getters) and genModuleGetterCall (module getters)
// so both free discarded temporaries of every kind identically — without this,
// a module getter returning a heap vector/channel/Arc used as a bare temporary
// leaked (only its heap-user-type case was covered by the original T1250 fix).
// The binding/assignment path claims the temp (claimStringTemp for stmtTemps,
// claimHeapTemp for heap-user-type instances), so a single owner frees it.
func (c *Compiler) trackGetterResultByType(e ast.Expr, retType types.Type, result value.Value) {
	if result.Type() == irtypes.I8Ptr {
		if retType == nil {
			return
		}
		named := extractNamed(retType)
		if named == types.TypString {
			c.trackStringTemp(result)
		} else if named == types.TypVector {
			if elemType, ok := types.AsVector(retType); ok {
				c.trackVectorTempWithElemType(result, elemType)
			} else {
				c.trackVectorTemp(result)
			}
		} else if chElem, isCh := types.AsChannel(retType); isCh || named == types.TypChannel {
			// T0486: Channel[T] getter result owns a heap allocation; without
			// tracking the cloned channel pointer leaks at statement end.
			// T0663: per-element-type drop walks any un-received buffered items.
			c.trackChannelTempWithElemType(result, chElem)
		} else if arcElem, isArc := types.AsArc(retType); isArc {
			// T0486: Ref[T] getter result owns a heap allocation; without
			// tracking the cloned Arc leaks at statement end. arcElem is
			// already substituted (Substitute on Instance produces a new
			// Instance with substituted typeArgs).
			c.trackTempWithDrop(result, c.getOrCreateArcDrop(arcElem))
		} else if weakElem, isWeak := types.AsWeak(retType); isWeak {
			// T0486: Weak[T] getter result owns a heap allocation.
			c.trackTempWithDrop(result, c.getOrCreateWeakDrop(weakElem))
		} else if mutexElem, isMutex := types.AsMutex(retType); isMutex {
			// T0486: Mutex[T] getter result owns a heap allocation.
			c.trackTempWithDrop(result, c.getOrCreateMutexDrop(mutexElem))
		} else if taskElem, isTask, taskFail := types.AsAnyTaskFailable(retType); isTask {
			// T0503: Task[T] getter result owns a G struct + result buffer.
			c.trackTempWithDrop(result, c.getOrCreateTaskDrop(taskElem, taskFail))
		}
	} else {
		c.trackHeapUserTypeResult(e, result)
	}
}

// trackReceivedTaskResult registers the heap result of a task-handle receive
// (`<-t`, `t : task[T]`) as a droppable statement temp so it is freed at
// statement end when the value is consumed inline (e.g. `out.push(<-t)`,
// `(<-t).len`, `(<-t) + "!"`) rather than bound to a named variable. When the
// receive flows into a binding/move site, the existing claim-on-consume sites
// (binding RHS, call-arg move, match-arm phi) clear the flag, so this integrates
// with the working named-binding path without double-free risk. T1150.
//
// innerType is the already-substituted task element type (inst.TypeArgs()[0] in
// genReceiveTask, where inst comes from the substituted operand type), so no
// further typeSubst/selfSubst is applied. Mirrors trackGetterResult's dispatch;
// the underlying track* helpers all guard on tempTrackingEnabled, a terminated
// block, and i8*-typed results, so this is safe to call unconditionally.
func (c *Compiler) trackReceivedTaskResult(result value.Value, innerType types.Type) {
	if !c.tempTrackingEnabled || result == nil || innerType == nil {
		return
	}
	if result.Type() == irtypes.I8Ptr {
		named := extractNamed(innerType)
		if named == types.TypString {
			c.trackStringTemp(result)
		} else if named == types.TypVector {
			if elemType, ok := types.AsVector(innerType); ok {
				c.trackVectorTempWithElemType(result, elemType)
			} else {
				c.trackVectorTemp(result)
			}
		} else if chElem, isCh := types.AsChannel(innerType); isCh || named == types.TypChannel {
			c.trackChannelTempWithElemType(result, chElem)
		} else if arcElem, isArc := types.AsArc(innerType); isArc {
			c.trackTempWithDrop(result, c.getOrCreateArcDrop(arcElem))
		} else if weakElem, isWeak := types.AsWeak(innerType); isWeak {
			c.trackTempWithDrop(result, c.getOrCreateWeakDrop(weakElem))
		} else if mutexElem, isMutex := types.AsMutex(innerType); isMutex {
			c.trackTempWithDrop(result, c.getOrCreateMutexDrop(mutexElem))
		} else if taskElem, isTask, taskFail := types.AsAnyTaskFailable(innerType); isTask {
			c.trackTempWithDrop(result, c.getOrCreateTaskDrop(taskElem, taskFail))
		}
		return
	}
	// {i8*, i8*} value struct → heap user type. Mirror trackHeapUserTypeResult's
	// tail filters so pure-value/copy/structural/primitive/container results are
	// skipped (those don't own a separate heap allocation to drop here).
	st, ok := result.Type().(*irtypes.StructType)
	if !ok || len(st.Fields) != 2 || st.Fields[0] != irtypes.I8Ptr || st.Fields[1] != irtypes.I8Ptr {
		return
	}
	named := extractNamed(innerType)
	if named == nil {
		return
	}
	if named.IsValueType() || named.IsCopy() || isPrimitiveScalar(named) || named.IsStructural() {
		return
	}
	if isContainerType(innerType) || named == types.TypString {
		return
	}
	dropFunc := c.resolveDropFuncForTemp(named, innerType)
	if dropFunc == nil {
		return
	}
	c.trackHeapTemp(c.block.NewExtractValue(result, 1), dropFunc)
}

// isMemberGetter reports whether a member access resolves to a getter (a
// module-level `mod.prop` getter, or an instance/enum getter `owner.getter`)
// rather than a plain field. Used by genMutRefArg to route a getter temporary
// through the materialize-and-track path instead of genFieldPtr (which panics on
// a getter). T1272.
func (c *Compiler) isMemberGetter(e *ast.MemberExpr) bool {
	if c.info.ModuleGetters[e] {
		return true
	}
	ownerType := c.info.Types[e.Target]
	if c.typeSubst != nil && ownerType != nil {
		ownerType = types.Substitute(ownerType, c.typeSubst)
	}
	if c.selfSubst != nil && ownerType != nil {
		ownerType = types.SubstituteSelf(ownerType, c.selfSubst.iface, c.selfSubst.concrete)
	}
	if named := extractNamed(ownerType); named != nil {
		if named.LookupGetter(e.Field) != nil {
			return true
		}
		// T1600: Inside a synthesized structural default body, `this` has the
		// concrete type which may lack the sibling default getter. Fall back to
		// the interface's getter table so genMutRefArg routes through the
		// materialize path instead of genFieldPtr (which would panic).
		if c.selfSubst != nil && c.selfSubst.iface.LookupGetter(e.Field) != nil {
			return true
		}
	} else if en := extractEnum(ownerType); en != nil {
		if en.LookupGetter(e.Field) != nil {
			return true
		}
	}
	return false
}

// setDupFlagsForFieldAccess sets dupStringFieldAccess, dupContainerFieldAccess,
// or dupHeapUserFieldAccess based on the resolved type shape — the shapes that
// need dup at every owner-droppable field-read consume site: string,
// Optional[string], Vector|Channel|Arc|Weak, and Optional[Vector|Channel|Arc|Weak]
// (dupContainer/dupString), plus non-value structural interfaces bare or under
// Optional (dupHeapUserFieldAccess — the {vtable, instance} view boxes a heap
// instance deep-cloned via __promise_structural_clone, T1299). Borrow types are
// skipped (they don't own the value). Caller is responsible for the
// owner-droppable gate (where applicable) and for clearing the flags after the
// dependent codegen runs. T0487.
func (c *Compiler) setDupFlagsForFieldAccess(t types.Type) {
	if t == nil || isRefType(t) {
		return
	}
	// T1432: `Map[K, V?].[]` returns `V??` — `Optional[Optional[X]]`, which matched
	// none of the shape arms below (extractNamed/arrayElemNeedsEscapeDup/
	// isNonValueStructuralType all miss on an Optional, and the single-Optional arm
	// peels only to `Optional[X]`), so NO dup flag was armed and the getter's
	// match-borrowed `return v` handed back the bucket's stored payload by alias →
	// the read temp's cleanup and the map's own drop free the same allocation
	// (double-free). Peel down to the LAST single Optional so a nested optional is
	// classified by the same inner shape the one-layer form already uses. Strictly
	// additive: a nested Optional previously matched no case at all, so this only
	// turns "no flag" into the flag the innermost type already dictates; one-layer
	// `T?` is untouched. (`Optional[Map]`/`Optional[Set]` inners still get no flag —
	// Map/Set are excluded from every dup predicate by design; see T1438/T1439.)
	for {
		opt, ok := t.(*types.Optional)
		if !ok {
			break
		}
		if _, nested := opt.Elem().(*types.Optional); !nested {
			break
		}
		t = opt.Elem()
	}
	if extractNamed(t) == types.TypString {
		c.dupStringFieldAccess = true
		return
	}
	if types.IsVector(t) || types.IsChannel(t) || types.IsArc(t) || types.IsWeak(t) {
		c.dupContainerFieldAccess = true
		return
	}
	// T1299: a non-value structural-interface field read out by value
	// (`[](int i) V? { return this._v; }`, `get val V { return this._v; }`) returns
	// the {vtable, instance} view aliasing the owner's box. The owner's synth drop
	// (T1284) and the escape sink's drop would free the same box (double-free).
	// Route through dupHeapUserFieldAccess (a structural view is a boxed heap
	// instance, not a container) so dupHeapFieldForEscape deep-clones the box via
	// __promise_structural_clone — the SAME flag genVectorIndex uses for structural
	// element reads (T1287/T1291), so a vector-index escape source is not disturbed.
	if isNonValueStructuralType(t) {
		c.dupHeapUserFieldAccess = true
		return
	}
	// T1176/T1173: a whole fixed-Array field/binding read out by value
	// (`return w.rows`) aliases N inner heap allocations (heap-user instances, or
	// string/Vector/Channel/Arc/Weak buffers); the owner's synth drop frees them
	// at scope exit while the escaped copy still points in (UAF/double-free). Route
	// through dupContainerFieldAccess so dupHeapFieldForEscape element-wise
	// deep-clones. Gated on the aliasing-element shape (int[N]/value arrays are
	// untouched — arrayElemNeedsEscapeDup returns false for them).
	if _, _, ok := c.arrayElemNeedsEscapeDup(t); ok {
		c.dupContainerFieldAccess = true
		return
	}
	if opt, ok := t.(*types.Optional); ok {
		elem := opt.Elem()
		if extractNamed(elem) == types.TypString {
			c.dupStringFieldAccess = true
			return
		}
		if types.IsVector(elem) || types.IsChannel(elem) || types.IsArc(elem) || types.IsWeak(elem) {
			c.dupContainerFieldAccess = true
			return
		}
		// T1299: Optional[structural] field read (`[](int i) V? { return this._v; }`).
		// The inner {vtable, instance} view aliases the owner's box — clone it so the
		// escaped optional owns an independent box. Uses dupHeapUserFieldAccess (the
		// structural view is a boxed heap instance), matching genVectorIndex's
		// Optional[structural] read path (T1291) so a vector-index escape source is
		// not disturbed.
		if isNonValueStructuralType(elem) {
			c.dupHeapUserFieldAccess = true
		}
	}
}

// genFieldPtr computes a pointer to a field on a user type instance.
// Used by storeBackSlicePtr and genMemberAssign.
func (c *Compiler) genFieldPtr(target *ast.MemberExpr) value.Value {
	targetType := c.info.Types[target.Target]
	if c.typeSubst != nil {
		targetType = types.Substitute(targetType, c.typeSubst)
	}
	if c.selfSubst != nil {
		targetType = types.SubstituteSelf(targetType, c.selfSubst.iface, c.selfSubst.concrete)
	}
	named := extractNamed(targetType)
	if named == nil {
		panic("codegen: cannot resolve type for field pointer")
	}

	layout := c.lookupTypeLayout(targetType)
	if layout == nil {
		panic(fmt.Sprintf("codegen: no layout for type %s", targetType))
	}

	field := named.LookupField(target.Field)
	if field == nil {
		panic(fmt.Sprintf("codegen: no field %s on type %s", target.Field, named))
	}

	// Value types: GEP directly into the variable's alloca or this pointer
	if layout.IsValueType {
		fieldIdx, ok := layout.ValueFieldIndex[field.Name()]
		if !ok {
			panic(fmt.Sprintf("codegen: field %s not in value layout for %s", field.Name(), named))
		}
		valuePtrType := irtypes.NewPointer(layout.Value.LLVMType)
		// T1356: take the in-place address of the receiver l-value — a local,
		// `this`, a nested value-type member (o.inner), or a container element
		// (vs[0]) — so the field store mutates real storage, not a spilled copy.
		basePtr, addrOK := c.genValueTypeReceiverAddr(target.Target)
		if !addrOK {
			// T1359: a non-addressable receiver (e.g. a getter/call result) is a fresh
			// temporary — there is no caller storage to write back to. Spill the
			// receiver value into a throwaway temp and GEP into that, so the field store
			// mutates the temp and is discarded. Matches the value-type-setter and
			// heap-field-assign paths, which already spill/no-op for the same rvalue
			// receiver.
			recv := c.memberReceiver(func() value.Value { return c.genExpr(target.Target) })
			basePtr = c.valueTypeReceiverPtr(recv, targetType)
		}
		typedPtr := c.block.NewBitCast(basePtr, valuePtrType)
		return c.block.NewGetElementPtr(layout.Value.LLVMType, typedPtr,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(fieldIdx)))
	}

	fieldIdx, ok := layout.InstanceFieldIndex[field.Name()]
	if !ok {
		panic(fmt.Sprintf("codegen: field %s not in layout for %s", field.Name(), named))
	}

	obj := c.genExpr(target.Target)
	var instance value.Value
	if isThisReceiver(target.Target) {
		instance = obj
	} else {
		instance = c.extractInstancePtr(obj)
	}
	typedPtr := c.block.NewBitCast(instance, layout.InstancePtrType)

	return c.block.NewGetElementPtr(layout.Instance.LLVMType, typedPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(fieldIdx)))
}
