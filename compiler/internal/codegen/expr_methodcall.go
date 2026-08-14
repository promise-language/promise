package codegen

import (
	"fmt"
	"sort"

	"github.com/llir/llvm/ir"
	"github.com/llir/llvm/ir/constant"
	"github.com/llir/llvm/ir/enum"
	irtypes "github.com/llir/llvm/ir/types"
	"github.com/llir/llvm/ir/value"

	"github.com/promise-language/promise/compiler/internal/ast"
	"github.com/promise-language/promise/compiler/internal/sema"
	"github.com/promise-language/promise/compiler/internal/types"
)

// --- Method calls ---

// genMethodCall generates a method call on a user type instance.
func (c *Compiler) genMethodCall(e *ast.CallExpr, member *ast.MemberExpr) value.Value {
	targetType := c.info.Types[member.Target]
	// Apply typeSubst for mono context
	if c.typeSubst != nil {
		targetType = types.Substitute(targetType, c.typeSubst)
	}
	// Apply selfSubst for default method synthesis
	if c.selfSubst != nil {
		targetType = types.SubstituteSelf(targetType, c.selfSubst.iface, c.selfSubst.concrete)
	}

	// Container native method dispatch (Vector, Map, string)
	if result, ok := c.genContainerMethodCall(e, member, targetType); ok {
		return result
	}

	// Enum method dispatch
	if result, ok := c.genEnumMethodCall(e, member, targetType); ok {
		return result
	}

	named := extractNamed(targetType)
	if named == nil {
		panic(fmt.Sprintf("codegen: cannot resolve type for method call on %T", targetType))
	}

	method := named.LookupMethod(member.Field)
	if method == nil && c.selfSubst != nil {
		// T0766: Inside a synthesized structural default-method body, `this`
		// (Self → concrete) may invoke a *sibling* default method that is declared
		// on the interface, not on the concrete type's own method table. Resolve it
		// through the interface and make sure the per-concrete synthesized function
		// exists; dispatch below then mangles to `<concrete>.<method>`.
		if im := c.selfSubst.iface.LookupMethod(member.Field); im != nil {
			c.ensureDefaultMethodsSynthesized(c.selfSubst.concrete, c.selfSubst.iface)
			method = im
		}
	}
	if method == nil {
		panic(fmt.Sprintf("codegen: no method %s on type %s", member.Field, named))
	}

	// Virtual dispatch: if the static type needs vtable and the method is not native,
	// emit an indirect call through the vtable so the correct override is called.
	if c.needsVtable(named) && !method.IsNative() {
		return c.genVirtualMethodCall(e, member, named, method, targetType)
	}

	// Direct dispatch: resolve method to a compile-time-known function.
	// For mono/generic types, use resolveTypeName (handles Instance → mono name).
	// For regular Named types with inheritance, use resolveMethodOwner to find
	// the parent that actually defines the method.
	var mangledName string
	ownerName := c.resolveMethodOwner(named, member.Field)
	if ownerName != named.Obj().Name() {
		// Method inherited from parent. Check if the parent is structural —
		// if so, use the concrete type's name (methods are synthesized per-concrete).
		if structParent := c.findStructuralOwner(named, member.Field); structParent != nil {
			concreteName := c.resolveTypeName(targetType)
			c.ensureDefaultMethodsSynthesized(named, structParent)
			mangledName = mangleMethodName(concreteName, member.Field, false)
		} else {
			// Non-structural parent: use the monomorphized parent name.
			monoOwner := c.resolveMonoParentName(named, targetType, ownerName)
			mangledName = mangleMethodName(monoOwner, member.Field, false)
		}
	} else {
		mangledName = mangleMethodName(c.resolveTypeName(targetType), member.Field, false)
	}

	fn, ok := c.funcs[mangledName]
	if !ok {
		panic(fmt.Sprintf("codegen: undeclared method %s", mangledName))
	}

	// T1395: When a generic function dispatches a structural-interface method
	// through a type param (e.g. scan[T: Parse] calling T.parse(r)), the generic
	// body was type-checked against the interface's abstract signature, so e.Args
	// is frozen at the interface arity. The concrete override may carry EXTRA
	// trailing defaulted/optional params (a supported conformance shape — see
	// identicalSignaturesWithSelf). Pad the missing trailing args with their
	// defaults so the concrete method isn't invoked with too few arguments.
	callArgs := c.padTrailingDefaultArgs(e.Args, method.Sig())

	var args []value.Value
	if method.Sig().Recv() != nil {
		// T1358: A value-type non-`this` receiver on a `~this` (mutable-borrow)
		// method must pass the caller's in-place storage address so field/setter
		// mutations reach the caller's variable — not a spilled copy. Handled by
		// genValueTypeReceiverArg, which evaluates the receiver itself (no shared
		// pre-eval, so a side-effecting subscript isn't evaluated twice). Value
		// types have no heap temp, so pendingReceiverClaim/trackChainIntermediate
		// bookkeeping does not apply.
		if named.IsValueType() && !isThisReceiver(member.Target) {
			args = append(args, c.genValueTypeReceiverArg(member.Target, targetType,
				method.Sig().Recv().Ref() == types.RefMut))
		} else {
			target := c.genExprAutoPropagate(member.Target) // B0323
			// T0130: Defer receiver claim — only claim if method produces a new iterator.
			c.pendingReceiverClaim = target
			// Container types (Vector, Map, string) are already i8* pointers — pass directly.
			// `this` in a method body is also i8*.
			// Primitive scalars (int, f64, bool, char, etc.) are raw values — pass directly.
			// Regular user types are value structs — extract the instance pointer.
			if isThisReceiver(member.Target) {
				args = append(args, target)
			} else if isContainerType(targetType) {
				args = append(args, target)
			} else if isPrimitiveScalar(named) {
				args = append(args, target)
			} else {
				instancePtr := c.extractInstancePtr(target)
				args = append(args, instancePtr)
				// B0258: Track method chain intermediate for cleanup at statement end.
				c.trackChainIntermediateReceiver(member.Target, target, instancePtr, named, targetType)
			}
		}
	}
	// T0418: Build owner-type subst (e.g., Box[int].T → int) so generic
	// methods on a generic instance see TypeParam-typed params resolved.
	// T1223: compute the subst BEFORE evaluating args and route arg-gen through
	// genGenericCallArgs so genCallArgsWithMutRef sees CONCRETE param types. Without
	// this, a `T move` param (e.g. Set[string].add(T move elem)) reaches
	// maybeEnableDupForMutRefArg as the unsubstituted TypeParam `T`, so the field-read
	// dup that every move sink arms (T0366) is skipped — `out.add(this.label)` then
	// stores an alias of the owner's inner buffer into the set → UAF when the owner drops.
	ownerSubst := c.buildOwnerTypeArgSubst(targetType)
	argVals, argTypes, variadicPTs := c.genGenericCallArgs(callArgs, method.Sig(), ownerSubst)
	origArgVals := argVals // B0345
	argVals = c.coerceCallArgs(argVals, argTypes, method.Sig().Params(), callArgs, ownerSubst)
	args = append(args, argVals...)

	var result value.Value = c.block.NewCall(fn, args...)
	c.clearVariadicStaticFlags(variadicPTs)
	result = c.emitReturnAliasCheckSubst(result, method.Sig(), callArgs, origArgVals, ownerSubst, e) // B0345/T0418
	return result
}

// buildParamDefaultInfos indexes every parameter default expression by the sema
// Info that type-checked it — the main file's and each imported module's (T1395).
//
// Call arguments normally belong to the same compilation unit as the call site,
// so codegen can read their recorded types straight out of c.info. Parameter
// defaults break that: sema (and padTrailingDefaultArgs) splice the DECLARING
// unit's expression into the call site's argument list, and that call site may
// be compiled with another unit's Info active — a mono'd `scan[T]` body from
// std filling a trailing default declared on a user type, for instance. This
// index lets useDeclaringInfo/exprType fall back to the owning Info.
func (c *Compiler) buildParamDefaultInfos() {
	record := func(info *sema.Info) {
		for _, expr := range info.ParamDefaults {
			// Only the Info that actually type-checked the expression can serve
			// it. std declarations are merged into every module's AST, so the
			// same node can appear in several Infos — first one with a recorded
			// type wins; they agree on the type either way.
			if _, ok := c.paramDefaultInfos[expr]; ok {
				continue
			}
			if _, typed := info.Types[expr]; typed {
				c.paramDefaultInfos[expr] = info
			}
		}
	}
	c.paramDefaultInfos = make(map[ast.Expr]*sema.Info)
	record(c.info)
	// Visit modules in name order: which Info wins for a node several units
	// share must not depend on map iteration order, or the emitted IR (and with
	// it the build cache key) becomes run-dependent.
	modNames := make([]string, 0, len(c.info.ModuleInfos))
	for name := range c.info.ModuleInfos {
		modNames = append(modNames, name)
	}
	sort.Strings(modNames)
	for _, name := range modNames {
		if modInfo := c.info.ModuleInfos[name]; modInfo != nil && modInfo.SemaInfo != nil {
			record(modInfo.SemaInfo)
		}
	}
}

// useDeclaringInfo switches c.info to the unit that type-checked expr when expr
// is a parameter default spliced in from another compilation unit (T1395), so
// the whole expression subtree is emitted against the Info that holds its types.
// Returns nil when no switch is needed; otherwise the caller must invoke the
// returned restore function.
//
// The switch is a strict fallback: when the active Info has a type for expr it
// resolved this splice itself (sema records the whole subtree at the call site,
// including per-call facts such as auto-propagation) and its records must win.
func (c *Compiler) useDeclaringInfo(expr ast.Expr) func() {
	if _, ok := c.info.Types[expr]; ok {
		return nil
	}
	owner := c.paramDefaultInfos[expr]
	if owner == nil || owner == c.info {
		return nil
	}
	saved := c.info
	c.info = owner
	return func() { c.info = saved }
}

// exprType returns the sema-recorded type of expr, falling back to the Info that
// declared it when expr is a parameter default from another compilation unit
// (T1395). Use it wherever an argument expression's type is read outside the
// useDeclaringInfo window.
func (c *Compiler) exprType(expr ast.Expr) types.Type {
	if t, ok := c.info.Types[expr]; ok {
		return t
	}
	if owner := c.paramDefaultInfos[expr]; owner != nil {
		return owner.Types[expr]
	}
	return nil
}

// emitTrailingDefaultArgValues emits argument values for sig's parameters from
// index `from` onward — the trailing parameters a call site does not supply
// (T1395). Each is that parameter's default expression, emitted against the Info
// that declared it, or a zero value (`none`) for an optional-typed parameter
// with no default. subst resolves the owner type's params in the parameter
// types. Returns the values and their LLVM types, the latter for callers that
// must also build the callee's function type (indirect/vtable dispatch).
//
// Use this where the call is emitted without an AST argument list; call sites
// that have one use padTrailingDefaultArgs so the padded args still flow through
// the normal ownership-aware argument pipeline.
func (c *Compiler) emitTrailingDefaultArgValues(sig *types.Signature, from int,
	subst map[*types.TypeParam]types.Type) ([]value.Value, []irtypes.Type, []types.Type) {

	params := sig.Params()
	var vals []value.Value
	var llvmTypes []irtypes.Type
	var semaTypes []types.Type
	for i := from; i < len(params); i++ {
		p := params[i]
		pType := p.Type()
		if subst != nil {
			pType = types.Substitute(pType, subst)
		}
		llvmTypes = append(llvmTypes, c.resolveType(pType))
		if defExpr := c.info.DefaultArgExpr(p); defExpr != nil {
			// Record the default's sema type (from the Info that declared it) so
			// callers can run the value through coerceCallArgs — optional wrapping,
			// view coercion / boxing — exactly as an ordinary call site would (T1465).
			semaTypes = append(semaTypes, c.exprType(defExpr))
			restore := c.useDeclaringInfo(defExpr)
			vals = append(vals, c.genExpr(defExpr))
			if restore != nil {
				restore()
			}
			continue
		}
		// Optional-typed param with no default → none (zeroed optional). The sema
		// type is TypNone, mirroring the synthetic `none` literal padTrailingDefaultArgs
		// inserts on the ordinary call path, so coerceCallArgs takes the none→T? path.
		semaTypes = append(semaTypes, types.TypNone)
		vals = append(vals, c.zeroValue(llvmTypes[len(llvmTypes)-1]))
	}
	return vals, llvmTypes, semaTypes
}

// padTrailingDefaultArgs returns args extended with synthetic entries for any
// trailing parameters of sig that args does not supply (T1395). This handles the
// generic structural-dispatch case where the call's args were frozen at an
// interface's abstract arity but the resolved concrete method carries extra
// defaulted or optional-typed params. Each missing param is filled from its
// default expression or, for an optional-typed param with no default, a none
// literal — mirroring sema's resolveCallArgs. When args already covers every
// param (the common case), the original slice is returned unchanged.
//
// A missing trailing *variadic* param cannot reach here: structural conformance
// (identicalSignaturesWithSelf) only accepts extra params that have a default or
// an optional type, and a variadic param has neither — so such a type never
// satisfies the interface in the first place.
func (c *Compiler) padTrailingDefaultArgs(args []*ast.Arg, sig *types.Signature) []*ast.Arg {
	params := sig.Params()
	if len(args) >= len(params) {
		return args
	}
	padded := make([]*ast.Arg, len(args), len(params))
	copy(padded, args)
	for i := len(args); i < len(params); i++ {
		p := params[i]
		if defExpr := c.info.DefaultArgExpr(p); defExpr != nil {
			padded = append(padded, &ast.Arg{Name: p.Name(), Value: defExpr})
			continue
		}
		// Optional-typed param with no default → none literal. Register its type
		// as TypNone (exactly what sema records for a `none` literal) so the arg
		// pipeline — genGenericCallArgs / coerceCallArgs — takes the none→T?
		// zero-value path instead of mishandling an untyped node. Sema does this
		// for the normal call path by type-checking the synthetic none it inserts
		// (resolveCallArgs step 6); the generic structural path never re-resolves
		// these trailing params, so we mirror it here.
		none := &ast.NoneLit{}
		c.info.Types[none] = types.TypNone
		padded = append(padded, &ast.Arg{Name: p.Name(), Value: none})
	}
	return padded
}

// genEnumGetterAccess emits a getter call on an enum value (e.g., s.name where name is a getter on enum Shape).
// Returns (result, true) if the enum has a matching getter, (nil, false) otherwise.
func (c *Compiler) genEnumGetterAccess(e *ast.MemberExpr, targetType types.Type, layout *TypeDeclLayout) (value.Value, bool) {
	var enum *types.Enum
	var enumName string
	switch t := targetType.(type) {
	case *types.Enum:
		enum = t
		enumName = t.Obj().Name()
		// Inside mono method body, this is the origin enum — use mono name
		if c.monoCtx != nil {
			if origin, ok := c.monoCtx.origin.(*types.Enum); ok && t == origin {
				enumName = c.monoCtx.name
			}
		}
	case *types.Instance:
		if en, ok := t.Origin().(*types.Enum); ok {
			enum = en
			enumName = monoName(t)
		}
	}
	if enum == nil {
		return nil, false
	}
	getter := enum.LookupGetter(e.Field)
	if getter == nil {
		return nil, false
	}

	mangledName := mangleMethodName(enumName, e.Field, false)
	fn, ok := c.funcs[mangledName]
	if !ok {
		return nil, false
	}

	// Pass the enum value as receiver
	prevEnumTemps := len(c.enumCtorTemps)
	target := c.genExprAutoPropagate(e.Target) // B0323
	enumCtorTracked := len(c.enumCtorTemps) > prevEnumTemps
	var ptr value.Value
	var tempEnumPtr value.Value
	// `this` inside an enum method is already i8* pointing to the enum alloca — pass directly.
	if isThisReceiver(e.Target) {
		ptr = target
	} else {
		alloca := c.entryBlock.NewAlloca(target.Type())
		alloca.SetName(c.uniqueLocalName("enum.getter"))
		c.block.NewStore(target, alloca)
		ptr = c.block.NewBitCast(alloca, irtypes.I8Ptr)
		// T0660: a borrow-return receiver (`Tagged&`/`Tagged~`) aliases the
		// owner's payload; dropping the synthesized getter receiver temp
		// would double-free what the owner still frees at scope exit.
		if c.freshEnumReceiverNeedsDrop(e.Target) && !enumCtorTracked {
			tempEnumPtr = ptr
		}
	}

	result := c.block.NewCall(fn, ptr)

	// Drop temp enum receiver if it was a fresh temporary not tracked by enumCtorTemps.
	if tempEnumPtr != nil && c.enumInstanceHasDrop(targetType, enum) {
		dropName := mangleMethodName(enumName, "drop", false)
		if dropFn, ok := c.funcs[dropName]; ok {
			c.block.NewCall(dropFn, tempEnumPtr)
		} else if c.moduleInfos != nil {
			if dropFn := c.forwardDeclareModuleEnumDrop(enum, enumName, dropName); dropFn != nil {
				c.block.NewCall(dropFn, tempEnumPtr)
			}
		}
	}

	// T0879: Register the getter result for cleanup at statement end, matching
	// genGetterCall / genVirtualGetterCall. Without this, string/vector/etc.
	// results used as unbound temporaries (inline ==, call arg) leak.
	c.trackGetterResult(e, getter, targetType, result)
	return result, true
}

// genEnumMethodCall generates a method call on an enum value.
// Returns (result, true) if the target is an enum with a matching method, (nil, false) otherwise.
func (c *Compiler) genEnumMethodCall(e *ast.CallExpr, member *ast.MemberExpr, targetType types.Type) (value.Value, bool) {
	// T0639: unwrap a ~/& generic-enum-instance receiver so the enum +
	// monoName resolve (mirrors genGenericEnumMethodCall). Without this a
	// non-generic enum method on a `~`/`&` generic-enum param falls through
	// to the default branch and the call silently fails to dispatch.
	if ref, ok := targetType.(*types.MutRef); ok {
		return c.genEnumMethodCall(e, member, ref.Elem())
	}
	if ref, ok := targetType.(*types.SharedRef); ok {
		return c.genEnumMethodCall(e, member, ref.Elem())
	}
	var enum *types.Enum
	var enumName string

	switch t := targetType.(type) {
	case *types.Enum:
		enum = t
		enumName = t.Obj().Name()
		// Inside mono method body, this is the origin enum — use mono name
		if c.monoCtx != nil {
			if origin, ok := c.monoCtx.origin.(*types.Enum); ok && t == origin {
				enumName = c.monoCtx.name
			}
		}
	case *types.Instance:
		if en, ok := t.Origin().(*types.Enum); ok {
			enum = en
			enumName = monoName(t)
		}
	default:
		return nil, false
	}

	if enum == nil {
		return nil, false
	}

	method := enum.LookupMethod(member.Field)
	if method == nil {
		return nil, false
	}

	mangledName := mangleMethodName(enumName, member.Field, false)
	fn, ok := c.funcs[mangledName]
	if !ok {
		return nil, false
	}

	var args []value.Value
	var tempEnumPtr value.Value // non-nil when receiver needs post-call drop
	if method.Sig().Recv() != nil {
		// Track whether the enumCtorTemps mechanism captures this constructor.
		prevEnumTemps := len(c.enumCtorTemps)
		target := c.genExprAutoPropagate(member.Target) // B0323
		enumCtorTracked := len(c.enumCtorTemps) > prevEnumTemps
		// `this` inside an enum method is already i8* pointing to the enum alloca — pass directly.
		if isThisReceiver(member.Target) {
			args = append(args, target)
		} else {
			// Store the enum value to a temp alloca and pass pointer as i8*.
			// Use the actual LLVM type of the value (i32 for fieldless, struct for data enums).
			alloca := c.entryBlock.NewAlloca(target.Type())
			alloca.SetName(c.uniqueLocalName("enum.this"))
			c.block.NewStore(target, alloca)
			ptr := c.block.NewBitCast(alloca, irtypes.I8Ptr)
			args = append(args, ptr)
			// Track for post-call drop if target produces a fresh value not already
			// tracked by enumCtorTemps. IdentExpr targets share heap data with
			// their binding's alloca — dropping the shallow copy would double-free.
			// T0660: a borrow-return receiver (`Tagged&`/`Tagged~`, e.g.
			// `ev.at(0)`) likewise aliases the owner's payload — dropping it
			// double-frees what the owner (the vector) still frees at exit.
			if c.freshEnumReceiverNeedsDrop(member.Target) && !enumCtorTracked {
				tempEnumPtr = ptr
			}
		}
	}
	// T0418: Build owner-enum subst so generic methods on a generic enum
	// instance see TypeParam-typed params resolved.
	// T1223: compute the subst BEFORE evaluating args and route arg-gen through
	// genGenericCallArgs so genCallArgsWithMutRef sees CONCRETE param types (a raw
	// `T move` param would otherwise skip the field-read dup every move sink arms).
	var enumSubst map[*types.TypeParam]types.Type
	if inst, ok := targetType.(*types.Instance); ok {
		if origin, ok := inst.Origin().(*types.Enum); ok && len(origin.TypeParams()) > 0 {
			enumSubst = types.BuildSubstMap(origin.TypeParams(), inst.TypeArgs())
		}
	}
	callArgs := c.padTrailingDefaultArgs(e.Args, method.Sig()) // T1395
	argVals, argTypes, variadicPTs := c.genGenericCallArgs(callArgs, method.Sig(), enumSubst)
	origArgVals := argVals // B0345
	argVals = c.coerceCallArgs(argVals, argTypes, method.Sig().Params(), callArgs, enumSubst)
	args = append(args, argVals...)

	var result value.Value = c.block.NewCall(fn, args...)
	c.clearVariadicStaticFlags(variadicPTs)
	result = c.emitReturnAliasCheckSubst(result, method.Sig(), callArgs, origArgVals, enumSubst, e) // B0345/T0418

	// Drop temp enum receiver if it was a fresh temporary not tracked by the
	// enumCtorTemps mechanism (e.g. an enum returned from a call, not an inline
	// `Enum.Variant(...)` constructor — inline constructors are tracked
	// unconditionally per T1108 and dropped at statement end instead).
	if tempEnumPtr != nil && c.enumInstanceHasDrop(targetType, enum) {
		dropName := mangleMethodName(enumName, "drop", false)
		if dropFn, ok := c.funcs[dropName]; ok {
			c.block.NewCall(dropFn, tempEnumPtr)
		} else if c.moduleInfos != nil {
			if dropFn := c.forwardDeclareModuleEnumDrop(enum, enumName, dropName); dropFn != nil {
				c.block.NewCall(dropFn, tempEnumPtr)
			}
		}
	}

	return result, true
}

// isFreshEnumExpr returns true if the expression produces a fresh enum value
// (not a reference to an existing variable). Fresh values need post-call drop
// when used as a temporary method/getter receiver.
func isFreshEnumExpr(expr ast.Expr) bool {
	switch e := expr.(type) {
	case *ast.CallExpr:
		return true
	case *ast.ErrorPanicExpr:
		// Panic unwrap of a call result (e.g., at(0)?!) produces a fresh value.
		return isFreshEnumExpr(e.Expr)
	case *ast.OptionalUnwrapExpr:
		// Unwrap of a call result (e.g., at(0)!) produces a fresh value.
		// Unwrap of a variable (e.g., opt_var!) references existing data.
		return isFreshEnumExpr(e.Expr)
	case *ast.AutoCloneExpr:
		return true // T0605: a deep clone is always a fresh owned value
	default:
		return false
	}
}

// freshEnumReceiverNeedsDrop reports whether a non-`this` enum method/getter
// receiver expression yields a *fresh, owned* enum value whose synthesized stack
// temp must be dropped after the call (zero-leak policy).
//   - isFreshEnumExpr shapes (call results, deep clones, unwraps thereof),
//     excluding borrow-return receivers (Tagged&/Tagged~) that alias the owner (T0660).
//   - T1165: a force-/panic-unwrap (or direct read) of a user-defined *non-native*
//     `[]` (e.g. `m[k]!` on a Map) — Map.[] returns `V?` by deep-cloning the slot,
//     so the unwrapped enum is uniquely owned and leaks unless dropped. Native
//     container/array indexing is excluded by isUserIndexExpr (those alias storage,
//     so dropping the temp would double-free).
func (c *Compiler) freshEnumReceiverNeedsDrop(expr ast.Expr) bool {
	if isFreshEnumExpr(expr) {
		return !c.isBorrowedExpr(expr)
	}
	e := unwrapDestructureParens(expr)
	switch u := e.(type) {
	case *ast.OptionalUnwrapExpr:
		e = unwrapDestructureParens(u.Expr)
	case *ast.ErrorPanicExpr:
		e = unwrapDestructureParens(u.Expr)
	}
	return c.isUserIndexExpr(e) && !c.isBorrowedExpr(e)
}

// genVirtualMethodCall emits an indirect call through the vtable.
// Reads vtable pointer from the value struct (field 0), indexes into it
// to get the function pointer, casts it, and calls.
func (c *Compiler) genVirtualMethodCall(e *ast.CallExpr, member *ast.MemberExpr,
	named *types.Named, method *types.Method, targetType types.Type) value.Value {

	// 1. Evaluate receiver
	receiverVal := c.genExprAutoPropagate(member.Target) // B0323
	// T0130: Defer receiver claim — only claim if method produces a new iterator.
	c.pendingReceiverClaim = receiverVal

	// 2. Extract vtable and instance
	var vtableRaw, instance value.Value
	if isThisReceiver(member.Target) {
		// `this` is already i8* — load vtable from typeinfo chain
		instance = receiverVal
		vtableRaw = c.loadVtablePtrFromInstance(receiverVal)
	} else {
		vtableRaw = c.extractVtablePtr(receiverVal)
		instance = c.extractInstancePtr(receiverVal)
		// B0258: Track method chain intermediate for cleanup at statement end.
		c.trackChainIntermediateReceiver(member.Target, receiverVal, instance, named, targetType)
	}

	// 3. Index into vtable — use the STATIC type's slot layout
	slotIndex := named.VirtualMethodIndex(member.Field, false) // regular method, not setter
	if slotIndex < 0 {
		panic(fmt.Sprintf("codegen: method %s not in vtable for %s", member.Field, named))
	}
	vtablePtr := c.block.NewBitCast(vtableRaw, irtypes.NewPointer(irtypes.I8Ptr))
	fnSlotPtr := c.block.NewGetElementPtr(irtypes.I8Ptr, vtablePtr,
		constant.NewInt(irtypes.I32, int64(slotIndex)))
	fnRaw := c.block.NewLoad(irtypes.I8Ptr, fnSlotPtr)

	// 4. Build the correct function type and bitcast.
	// If the static type is a generic instance (e.g. Transformer[int]),
	// substitute type params so T→int in method signatures.
	// T0418: include parent-type params (via mergeParentSubst inside
	// buildOwnerTypeArgSubst) so inherited methods using parent's TypeParams
	// resolve correctly.
	vtableSubst := c.buildOwnerTypeArgSubst(targetType)
	resolveVtableType := func(t types.Type) irtypes.Type {
		if vtableSubst != nil {
			t = types.Substitute(t, vtableSubst)
		}
		return c.resolveType(t)
	}

	retType := irtypes.Type(irtypes.Void)
	if method.Sig().Result() != nil {
		retType = resolveVtableType(method.Sig().Result())
	}
	if method.Sig().CanError() {
		retType = computeResultType(retType)
	}
	var paramTypes []irtypes.Type
	if method.Sig().Recv() != nil {
		paramTypes = append(paramTypes, irtypes.I8Ptr)
	}
	for _, p := range method.Sig().Params() {
		pt := resolveVtableType(p.Type())
		// MutRef params are passed as pointers (B0149)
		if _, isMutRef := p.Type().(*types.MutRef); isMutRef {
			pt = irtypes.NewPointer(pt)
		}
		paramTypes = append(paramTypes, pt)
	}
	funcType := irtypes.NewFunc(retType, paramTypes...)
	fnTyped := c.block.NewBitCast(fnRaw, irtypes.NewPointer(funcType))

	// 5. Call — receiver is instance (i8*), not the value struct
	var args []value.Value
	if method.Sig().Recv() != nil {
		args = append(args, instance)
	}
	// T0418: vtableSubst (with parent subst merged) covers both the static
	// type's TypeParams and inherited parent-type TypeParams.
	// T1223: route arg-gen through genGenericCallArgs so genCallArgsWithMutRef sees
	// CONCRETE param types (a raw `T move` param would otherwise skip the field-read
	// dup every move sink arms).
	// T1395: the vtable slot's LLVM signature is built above from the FULL param
	// list of method.Sig(), so trailing defaulted/optional params omitted by a
	// generic structural-interface call site must be filled here too — otherwise
	// the indirect call passes too few arguments and the callee reads garbage.
	callArgs := c.padTrailingDefaultArgs(e.Args, method.Sig())
	argVals, argTypes, variadicPTs := c.genGenericCallArgs(callArgs, method.Sig(), vtableSubst)
	origArgVals := argVals // B0345
	argVals = c.coerceCallArgs(argVals, argTypes, method.Sig().Params(), callArgs, vtableSubst)
	args = append(args, argVals...)
	var result value.Value = c.block.NewCall(fnTyped, args...)
	c.clearVariadicStaticFlags(variadicPTs)
	result = c.emitReturnAliasCheckSubst(result, method.Sig(), callArgs, origArgVals, vtableSubst, e) // B0345/T0418
	return result
}

// genContainerMethodCall dispatches native method calls on Vector, Map, and string.
// Returns (result, true) if handled, (nil, false) otherwise.
// Non-native methods (with Promise bodies) fall through to the regular call path.
// Handles both Instance wrappers (user code: Vector[int]) and bare Named types
// (method body: this is TypVector) by resolving type args from typeSubst.
func (c *Compiler) genContainerMethodCall(e *ast.CallExpr, member *ast.MemberExpr, targetType types.Type) (value.Value, bool) {
	methodName := member.Field

	// Unwrap MutRef/SharedRef so types.AsVector etc. can see the Instance.
	// Parameters declared as `T[] ~buf` have type MutRef{Instance{TypVector, [T]}}.
	unwrapped := targetType
	if mr, ok := unwrapped.(*types.MutRef); ok {
		unwrapped = mr.Elem()
	} else if sr, ok := unwrapped.(*types.SharedRef); ok {
		unwrapped = sr.Elem()
	}

	// Check if the method is native — only native methods are handled here.
	// Non-native methods fall through to the regular user method path.
	named := extractNamed(targetType)
	if named == types.TypVector || named == types.TypString || named == types.TypChannel || named == types.TypArc ||
		named == types.TypMutex || named == types.TypMutexGuard {
		m := named.LookupMethod(methodName)
		if m == nil || !m.IsNative() {
			return nil, false // fall through to regular method dispatch
		}
	}

	// Vector methods: push, pop, contains, remove
	if elem, ok := types.AsVector(unwrapped); ok {
		return c.genVectorMethodCall(e, member, elem, methodName), true
	}
	// Bare TypVector (inside a method body on Vector): resolve T from typeSubst
	if named == types.TypVector {
		if elem := c.resolveTypeParam(types.TypVector.TypeParams()[0]); elem != nil {
			return c.genVectorMethodCall(e, member, elem, methodName), true
		}
	}

	// Channel methods: send, close
	if elem, ok := types.AsChannel(unwrapped); ok {
		return c.genChannelMethodCall(e, member, elem, methodName), true
	}
	if named == types.TypChannel {
		if elem := c.resolveTypeParam(types.TypChannel.TypeParams()[0]); elem != nil {
			return c.genChannelMethodCall(e, member, elem, methodName), true
		}
	}

	// Arc methods: clone, downgrade
	if elem, ok := types.AsArc(unwrapped); ok {
		return c.genArcMethodCall(e, member, elem, methodName), true
	}
	if named == types.TypArc {
		if elem := c.resolveTypeParam(types.TypArc.TypeParams()[0]); elem != nil {
			return c.genArcMethodCall(e, member, elem, methodName), true
		}
	}

	// Weak methods: upgrade, clone (T0157)
	if elem, ok := types.AsWeak(unwrapped); ok {
		return c.genWeakMethodCall(e, member, elem, methodName), true
	}
	if named == types.TypWeak {
		if elem := c.resolveTypeParam(types.TypWeak.TypeParams()[0]); elem != nil {
			return c.genWeakMethodCall(e, member, elem, methodName), true
		}
	}

	// Mutex methods: lock
	if elem, ok := types.AsMutex(unwrapped); ok {
		return c.genMutexMethodCall(e, member, elem, methodName), true
	}
	if named == types.TypMutex {
		if elem := c.resolveTypeParam(types.TypMutex.TypeParams()[0]); elem != nil {
			return c.genMutexMethodCall(e, member, elem, methodName), true
		}
	}

	// MutexGuard methods: close (T0839). The `borrow` get/set are getter/setter
	// property accesses handled in genMemberExpr, not method calls; drop is
	// automatic. close is the only user-callable method reachable here.
	if _, ok := types.AsMutexGuard(unwrapped); ok {
		return c.genMutexGuardMethodCall(e, member, methodName), true
	}
	if named == types.TypMutexGuard {
		return c.genMutexGuardMethodCall(e, member, methodName), true
	}

	// String native methods: trim, split (contains/starts_with/ends_with/index_of are now pure Promise)
	if named == types.TypString {
		if result, ok := c.genStringMethodCall(e, member, methodName); ok {
			return result, true
		}
	}

	return nil, false
}

// resolveTypeParam looks up a type parameter in the current typeSubst map.
// Returns nil if not in a monomorphic context or the param is not mapped.
func (c *Compiler) resolveTypeParam(tp *types.TypeParam) types.Type {
	if c.typeSubst == nil {
		return nil
	}
	return c.typeSubst[tp]
}

// getOrEmitOptContainsEqFn returns a comparison function (ABI: i32(i8*,i8*,i64))
// for Optional scalar element types in vector.contains. Instead of memcmp, the
// generated function compares the i1 presence flag and then (if both are Some)
// the inner scalar value using icmp/fcmp — completely ignoring the struct's
// padding bytes (bytes 1..7 of {i1, i64}, bytes 1..3 of {i1, i32}, etc.).
//
// Why this matters: LLVM O1 decomposes `store { i1, T } zeroinitializer` into
// per-field stores covering only the defined fields. The 1..7 padding bytes get
// no explicit store and remain uninitialized stack memory. O1 then replaces
// promise_vector_contains's memcmp with a single `icmp ne i128`, comparing all
// 16 bytes of the slot. When push and contains are in different functions (e.g.
// the cross-function generic test), their independent O1 contexts produce
// different stack garbage in those padding bytes → icmp finds inequality even
// when the logical Optional values are equal.
//
// Returns constant.NewNull(irtypes.I8Ptr) for non-scalar inner types (heap
// types, strings, nested Optional, value-type structs), which fall back to the
// existing memcmp path. The function is cached in c.funcs by name to avoid
// duplicate definitions.
func (c *Compiler) getOrEmitOptContainsEqFn(optLLVM irtypes.Type) value.Value {
	st, ok := optLLVM.(*irtypes.StructType)
	if !ok || len(st.Fields) < 2 {
		return constant.NewNull(irtypes.I8Ptr)
	}
	innerLLVM := st.Fields[1]

	// Only scalar inner types: fall back to memcmp for complex types (pointers,
	// structs, nested Optional, strings) where identity/equality semantics differ.
	var isFloat bool
	switch innerLLVM.(type) {
	case *irtypes.IntType:
		isFloat = false
	case *irtypes.FloatType:
		isFloat = true
	default:
		return constant.NewNull(irtypes.I8Ptr)
	}

	fnName := "__promise_opt_eq_" + innerLLVM.String()
	if fn, exists := c.funcs[fnName]; exists {
		return c.block.NewBitCast(fn, irtypes.I8Ptr)
	}

	// Emit: i32 fnName(i8* a, i8* b, i64 _ksz)
	// Returns 1 if equal (same presence + same inner value), 0 otherwise.
	aParam := ir.NewParam("a", irtypes.I8Ptr)
	bParam := ir.NewParam("b", irtypes.I8Ptr)
	kszParam := ir.NewParam("_ksz", irtypes.I64)
	fn := c.module.NewFunc(fnName, irtypes.I32, aParam, bParam, kszParam)
	c.funcs[fnName] = fn

	i32zero := constant.NewInt(irtypes.I32, 0)
	i32one := constant.NewInt(irtypes.I32, 1)
	gepOuter := constant.NewInt(irtypes.I32, 0)
	gep0 := constant.NewInt(irtypes.I32, 0)
	gep1 := constant.NewInt(irtypes.I32, 1)

	entry := fn.NewBlock(".entry")
	flagsMatch := fn.NewBlock("flags.match")
	compareInner := fn.NewBlock("compare.inner")
	retTrue := fn.NewBlock("ret.true")
	retFalse := fn.NewBlock("ret.false")

	// entry: cast i8* → {i1,T}*, load flags, branch on equality
	ap := entry.NewBitCast(aParam, irtypes.NewPointer(st))
	bp := entry.NewBitCast(bParam, irtypes.NewPointer(st))
	aFlagPtr := entry.NewGetElementPtr(st, ap, gepOuter, gep0)
	bFlagPtr := entry.NewGetElementPtr(st, bp, gepOuter, gep0)
	aFlag := entry.NewLoad(irtypes.I1, aFlagPtr)
	bFlag := entry.NewLoad(irtypes.I1, bFlagPtr)
	flagsEq := entry.NewICmp(enum.IPredEQ, aFlag, bFlag)
	entry.NewCondBr(flagsEq, flagsMatch, retFalse)

	// flags.match: flags are equal; if a_flag=false → both None → equal
	flagsMatch.NewCondBr(aFlag, compareInner, retTrue)

	// compare.inner: both Some — compare the inner scalar value
	aValPtr := compareInner.NewGetElementPtr(st, ap, gepOuter, gep1)
	bValPtr := compareInner.NewGetElementPtr(st, bp, gepOuter, gep1)
	aVal := compareInner.NewLoad(innerLLVM, aValPtr)
	bVal := compareInner.NewLoad(innerLLVM, bValPtr)
	var valsEq value.Value
	if isFloat {
		valsEq = compareInner.NewFCmp(enum.FPredOEQ, aVal, bVal)
	} else {
		valsEq = compareInner.NewICmp(enum.IPredEQ, aVal, bVal)
	}
	compareInner.NewCondBr(valsEq, retTrue, retFalse)

	retTrue.NewRet(i32one)
	retFalse.NewRet(i32zero)

	return c.block.NewBitCast(fn, irtypes.I8Ptr)
}

// genMethodIndex calls the monomorphized [] method on a user type.
func (c *Compiler) genMethodIndex(e *ast.IndexExpr, targetType types.Type) value.Value {
	// Resolve mangled method name
	mangledName := mangleMethodName(c.resolveTypeName(targetType), "[]", false)
	fn, ok := c.funcs[mangledName]
	if !ok {
		panic(fmt.Sprintf("codegen: undeclared [] method %s", mangledName))
	}

	target := c.genExprAutoPropagate(e.Target) // B0323
	keyVal := c.genExpr(e.Index)

	// Extract instance pointer: container types (Vector, Map) are already i8*,
	// value types store to temp alloca, regular user types extract instance ptr.
	named := extractNamed(targetType)
	var instancePtr value.Value
	switch {
	case isThisReceiver(e.Target):
		// T0745: `this` (incl. paren-wrapped) is already the i8* receiver ptr the
		// operator method expects — must precede the value-type branch, which
		// would otherwise panic trying to take a value-struct ptr of a raw i8*.
		instancePtr = target
	case isContainerType(targetType):
		instancePtr = target
	case named != nil && named.IsValueType():
		instancePtr = c.valueTypeReceiverPtr(target, targetType)
	default:
		instancePtr = c.extractInstancePtr(target)
	}

	result := c.block.NewCall(fn, instancePtr, keyVal)

	// B0347: Dup borrowed string when `[]` on a Vector-shaped container returns
	// `Optional[string]` that points into the container's buffer. Without the
	// dup, the caller's unwrapped string and the element alias the same heap
	// allocation, causing double-free at scope exit. Guarded by
	// `c.dupStringFieldAccess` (set by the return site and typed var decls) so
	// temporary uses (comparisons, function args) don't dup. Map is not covered
	// here (see B0350) — enabling the dup for Map regressed existing tests that
	// rely on aliased map-value storage.
	if c.dupStringFieldAccess && c.tempTrackingEnabled && isContainerType(targetType) {
		resultType := c.info.Types[e]
		if c.typeSubst != nil {
			resultType = types.Substitute(resultType, c.typeSubst)
		}
		if opt, ok := resultType.(*types.Optional); ok && extractNamed(opt.Elem()) == types.TypString {
			c.dupStringFieldAccess = false // consume the flag
			innerStr := c.block.NewExtractValue(result, 1)
			dup := c.dupString(innerStr)
			c.trackStringTemp(dup)
			c.optionalStringDup = dup
			return c.block.NewInsertValue(result, dup, 1)
		}
	}

	// T0397: Dup borrowed tuple when `[]` returns `Optional[(droppable, ...)]`
	// whose inner fields alias the container's stored heap allocations. Without
	// the dup, force-unwrapping into a variable would let both the variable's
	// bindingDropTuple and the container's element walk drop the same heap data
	// → double-free. Mirrors B0347 (Optional[string]) and T0370 (Vector[Tuple]).
	// NOT gated on isContainerType — fires for Map and any other type with
	// `[]` returning `Optional[Tuple]`.
	if c.dupTupleFieldAccess && c.tempTrackingEnabled {
		resultType := c.info.Types[e]
		if c.typeSubst != nil {
			resultType = types.Substitute(resultType, c.typeSubst)
		}
		if opt, ok := resultType.(*types.Optional); ok {
			elem := opt.Elem()
			if c.typeSubst != nil {
				elem = types.Substitute(elem, c.typeSubst)
			}
			if tup, isTup := elem.(*types.Tuple); isTup && c.tupleNeedsDrop(elem) {
				c.dupTupleFieldAccess = false // consume the flag
				innerTup := c.block.NewExtractValue(result, 1)
				dup := c.dupTupleValue(innerTup, tup)
				c.optionalTupleDup = dup
				return c.block.NewInsertValue(result, dup, 1)
			}
		}
	}

	// T0440: Dup borrowed heap user type when `[]` returns `Optional[heap-user-type]`
	// whose inner value aliases the container's stored element. Without the dup,
	// force-unwrapping into a variable would let both the variable's drop binding
	// and the container's element walk drop the same instance → double-free.
	// Mirrors B0347 (Optional[string]) and T0397 (Optional[Tuple]). NOT gated on
	// isContainerType — fires for Map and any other type with `[]` returning
	// `Optional[heap-user-type]`.
	//
	// Gated to V types that `Map[K, V].[]` returns by ALIAS — i.e. NOT
	// match-dup'able (`!typeNeedsMatchDup`). `Map[K, V].[]` is a synthesized method
	// whose body uses a match destructure (`Slot.Used(k, v) => return v;`) that
	// already dups V internally when V is safely-dup'able (typeNeedsMatchDup →
	// heapTypeSafeToDup walks fields) or when V has a `clone()` method. The V shapes
	// where Map.[]'s body leaves V aliased are exactly `!typeNeedsMatchDup`: an
	// *explicit* `drop()` with no clone (T0484), OR a synth-drop type whose fields
	// aren't shallow-dup-safe (e.g. a droppable-element vector field — T1146).
	// typeNeedsMatchDup already returns true for clone-bearing V (so this is skipped
	// for them — their body dup makes the result owned) and for shallow-dup-safe V,
	// so `!typeNeedsMatchDup` fires precisely on the aliased shapes. Firing outside
	// that shape would produce a redundant second copy whose pointer is lost (one
	// alloc leaks per read), which is why the predicate must be exact. The drop
	// origin (explicit vs synthesized) is irrelevant — what matters is whether the
	// body aliased. Uses dupHeapValue (memcpy + sub-field dup) directly, which is
	// null-safe internally — important because `result`'s value field is zero/null
	// when the Optional is None.
	if c.dupHeapUserFieldAccess && c.tempTrackingEnabled {
		resultType := c.info.Types[e]
		if c.typeSubst != nil {
			resultType = types.Substitute(resultType, c.typeSubst)
		}
		if opt, ok := resultType.(*types.Optional); ok {
			elem := opt.Elem()
			if c.typeSubst != nil {
				elem = types.Substitute(elem, c.typeSubst)
			}
			if isDroppableHeapUserType(elem) {
				if named := extractNamed(elem); named != nil &&
					named.LookupMethod("clone") == nil &&
					!c.typeNeedsMatchDup(elem) {
					c.dupHeapUserFieldAccess = false // consume the flag
					innerVal := c.block.NewExtractValue(result, 1)
					dup := c.dupHeapValue(innerVal, elem)
					c.optionalHeapDup = dup
					return c.block.NewInsertValue(result, dup, 1)
				}
			}
		}
	}

	// T1117: Dup a borrowed droppable enum element when `[]` returns
	// `Optional[enum]` whose variant data aliases the container's stored slot.
	// `Map[K,V].[]`'s match-destructure body returns V by alias when V is an enum
	// that isn't safely match-dup'able or clone-bearing (typeNeedsMatchDup ==
	// false) — e.g. an enum whose variant carries an Arc/Ref. An owning bind
	// (`h := m[k]!`, or assignment) sets dupHeapUserFieldAccess at the bind site;
	// without a dup here, the binding's drop walks the variant's Arc/Ref and
	// decrements the shared refcount, corrupting the slot the Map still owns (UAF
	// on the next read). Deep-dup the variant fields via an alloca round-trip
	// through dupEnumElementInPlace (dupArc/dupWeak increment the refcount), then
	// re-insert into the Optional so the bound copy owns an independent count.
	// The inline/borrow form (`match m[k]!`) never sets the flag, so it stays
	// aliased — balanced because it takes no owning drop. Mirrors
	// maybeDupPushElement's B0290 alloca round-trip. Returns early like the dup
	// branches above (the binding claims the result; not a stmt-temp).
	if c.dupHeapUserFieldAccess && c.tempTrackingEnabled {
		resultType := c.info.Types[e]
		if c.typeSubst != nil {
			resultType = types.Substitute(resultType, c.typeSubst)
		}
		if opt, ok := resultType.(*types.Optional); ok {
			elem := opt.Elem()
			if c.typeSubst != nil {
				elem = types.Substitute(elem, c.typeSubst)
			}
			if extractEnum(elem) != nil && c.vecElemNeedsEnumDrop(elem) && !c.typeNeedsMatchDup(elem) {
				c.dupHeapUserFieldAccess = false // consume the flag
				innerVal := c.block.NewExtractValue(result, 1)
				alloca := c.createEntryAlloca(innerVal.Type())
				c.block.NewStore(innerVal, alloca)
				c.dupEnumElementInPlace(alloca, elem)
				dup := c.block.NewLoad(innerVal.Type(), alloca)
				return c.block.NewInsertValue(result, dup, 1)
			}
		}
	}

	// T0647: A user-defined non-native `[](int i) T` compiles to an ordinary
	// method whose body returns an *owned* heap value (IR-identical to the
	// equivalent plain method). The *ast.CallExpr genExpr case tracks such
	// return temps for cleanup at statement end; the *ast.IndexExpr case does
	// not, so `s[i]` (→ here) leaked the returned string/vector/Arc/.../heap-
	// user value while `s.at(i)` did not. Mirror the CallExpr post-call
	// tracking. This is reached ONLY for user-defined `[]` (genIndexExpr
	// dispatches native index reads to genNativeIndex/genStringIndex/
	// genVectorIndex/genArrayIndex, which return borrowed aliases and must NOT
	// be tracked), so the tracking is correctly scoped to owned operator
	// returns. The Optional-dup branches above return early and are unaffected.
	c.trackUserIndexResult(e, result)
	return result
}

// trackUserIndexResult mirrors the *ast.CallExpr post-call heap-temp tracking
// (genExpr) for the user-defined non-native `[]` read path (T0647). All track*
// helpers self-gate on c.tempTrackingEnabled / c.block.Term / SSA-dedup, so the
// unconditional calls here faithfully match the CallExpr path. findInnerCallExpr
// returns nil for *ast.IndexExpr, so trackHeapUserTypeResult performs no
// receiver-alias check (claimHeapTemp still dedups aliasing).
func (c *Compiler) trackUserIndexResult(e *ast.IndexExpr, result value.Value) {
	if result == nil {
		return
	}
	// T0649: mirror the CallExpr path — a borrow return (`T&`/`T~`) from a
	// user-defined `[]` operator is a reference into storage owned elsewhere,
	// not an owned temp. Resolve the static result type before the I8Ptr split
	// (so the heap-user `T~` branch is covered too) and skip tracking for
	// borrow results; otherwise the binding-site borrow-flag clear would leave
	// the value with no owner (leak), exactly as in the plain-method path.
	rt := c.info.Types[e]
	if c.typeSubst != nil && rt != nil {
		rt = types.Substitute(rt, c.typeSubst)
	}
	if c.selfSubst != nil && rt != nil {
		rt = types.SubstituteSelf(rt, c.selfSubst.iface, c.selfSubst.concrete)
	}
	if rt != nil && isRefType(rt) {
		return
	}
	if result.Type() == irtypes.I8Ptr {
		if rt != nil {
			named := extractNamed(rt)
			if named == types.TypString {
				c.trackStringTemp(result)
			} else if named == types.TypVector {
				if elemType, ok := types.AsVector(rt); ok {
					c.trackVectorTempWithElemType(result, elemType)
				} else {
					c.trackVectorTemp(result)
				}
			} else if arcElem, isArc := types.AsArc(rt); isArc {
				c.trackTempWithDrop(result, c.getOrCreateArcDrop(arcElem))
			} else if weakElem, isWeak := types.AsWeak(rt); isWeak {
				c.trackTempWithDrop(result, c.getOrCreateWeakDrop(weakElem))
			} else if mutexElem, isMutex := types.AsMutex(rt); isMutex {
				c.trackTempWithDrop(result, c.getOrCreateMutexDrop(mutexElem))
			} else if taskElem, isTask, taskFail := types.AsAnyTaskFailable(rt); isTask {
				c.trackTempWithDrop(result, c.getOrCreateTaskDrop(taskElem, taskFail))
			} else if _, isMG := types.AsMutexGuard(rt); isMG {
				// T0561: MutexGuard.drop is a single non-per-element-type symbol.
				if dropFn, ok := c.funcs["MutexGuard.drop"]; ok {
					c.trackTempWithDrop(result, dropFn)
				}
			}
		}
	} else {
		c.trackHeapUserTypeResult(e, result)
	}
}
