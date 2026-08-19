package codegen

import (
	"github.com/llir/llvm/ir"
	"github.com/llir/llvm/ir/constant"
	"github.com/llir/llvm/ir/enum"
	irtypes "github.com/llir/llvm/ir/types"
	"github.com/llir/llvm/ir/value"

	"github.com/promise-language/promise/compiler/internal/ast"
	"github.com/promise-language/promise/compiler/internal/types"
)

// emitReturnAliasCheck generates runtime pointer comparisons between a function call's
// return value and its non-Copy ident arguments. If the return pointer aliases an
// argument, the argument's drop flag is cleared to prevent double-free (B0345).
//
// Without this check, identity(v) where v is a heap string causes SIGABRT:
// the caller has drop flags for both v and the return value s, but they point
// to the same memory — both get freed at scope exit.
func (c *Compiler) emitReturnAliasCheck(result value.Value, sig *types.Signature, args []*ast.Arg, argVals []value.Value, callExpr ast.Expr) value.Value {
	return c.emitReturnAliasCheckSubst(result, sig, args, argVals, nil, callExpr)
}

// emitReturnAliasCheckSubst is the generic-aware variant. T0418: callSubst maps
// the callee's TypeParams to the call's concrete type args so droppability
// checks see through TypeParams (e.g., T? → _Box? → droppable).
//
// Returns the possibly-cloned result. T1269: when an arg is a borrowed param
// (no drop flag) and the owned result aliases it at runtime, the result is
// deep-cloned so the caller's borrow stays the sole owner of its buffer and the
// escaping owned result frees an independent allocation. In every other path the
// returned value is the unchanged input result.
func (c *Compiler) emitReturnAliasCheckSubst(result value.Value, sig *types.Signature, args []*ast.Arg, argVals []value.Value, callSubst map[*types.TypeParam]types.Type, callExpr ast.Expr) value.Value {
	if result == nil || sig == nil {
		return result
	}
	retType := sig.Result()
	if retType == nil {
		return result
	}
	if callSubst != nil {
		retType = types.Substitute(retType, callSubst)
	}
	if c.typeSubst != nil {
		retType = types.Substitute(retType, c.typeSubst)
	}
	// T1311: reset the structural-arg handoff on every call (structural or not) so a
	// stale value from a prior structural call whose result maybeTrackIterTemp never
	// consumed cannot leak into this call.
	c.pendingStructuralArgAliasPtrs = nil
	c.pendingStructuralArgAliasCall = nil
	// T1311: structural-interface returns are excluded from isTypeDroppable (they are
	// borrow views, not owned), so the arg-alias loop below never runs for them and
	// the function returns early. But a method returning an owned structural param by
	// value (`make(Sink s) Sink { return s; }`) hands back a view aliasing the arg's
	// box. When that result is a discarded temp, maybeTrackIterTemp registers it for
	// __promise_structural_drop over the SAME box as the (fresh rvalue-temp or named-
	// local) arg → double free (T1311). Record each owned arg's instance ptr so
	// maybeTrackIterTemp can clear the RESULT temp's flag on a runtime alias match,
	// leaving the arg the sole owner. The runtime ptr compare makes recording all
	// heap-typed args safe: two distinct live boxes never share an address, so an
	// unrelated arg can never spuriously match. Only the result temp is cleared —
	// unlike clearDiscardedAliasTempFlag, which clears ALL matching heap temps and is
	// safe only when the keeper is a non-temp local (a fresh arg temp is itself a heap
	// temp, so clearing every match would leave nobody freeing the box → leak).
	if isNonValueStructuralType(retType) {
		for i := range args {
			if i >= len(argVals) {
				break
			}
			if p := extractAliasPtr(c, argVals[i]); p != nil && p.Type() == irtypes.I8Ptr {
				c.pendingStructuralArgAliasPtrs = append(c.pendingStructuralArgAliasPtrs, p)
			}
		}
		if len(c.pendingStructuralArgAliasPtrs) > 0 {
			c.pendingStructuralArgAliasCall = callExpr
		}
	}
	// Only check for non-Copy return types that could alias.
	if !isTypeDroppable(retType) {
		return result
	}
	// A borrow return (`T&`/`T~`) is never dropped by the caller, so it can never
	// cause an aliasing double-free — and clearing an arg's drop flag here would
	// instead leak the arg (T0998: bare borrow params are passed by value).
	switch retType.(type) {
	case *types.SharedRef, *types.MutRef:
		return result
	}
	// Skip failable returns — the raw result is {i1, value, err_ptr}, not the value itself.
	if sig.CanError() {
		return result
	}

	// T1269: arg pointers of borrowed (drop-flag-less) params the owned result may
	// alias. Collected in the loop; the single guarded clone is emitted afterward.
	var borrowAliasPtrs []value.Value

	params := sig.Params()
	for i, arg := range args {
		if i >= len(argVals) || i >= len(params) {
			break
		}
		// Skip move (`~`) and variadic params — `~` already clears the flag;
		// variadic has separate handling. A `T&`/`T~` reference param is a borrow
		// and never owns the arg.
		p := params[i]
		if p.Ref() == types.RefMut || p.IsVariadic() {
			continue
		}
		switch p.Type().(type) {
		case *types.SharedRef, *types.MutRef:
			continue
		}
		paramType := p.Type()
		if callSubst != nil {
			paramType = types.Substitute(paramType, callSubst)
		}
		if c.typeSubst != nil {
			paramType = types.Substitute(paramType, c.typeSubst)
		}
		if !isTypeDroppable(paramType) {
			continue
		}
		// T0998: only clear the arg's drop flag when the result could actually be
		// a view of the arg — i.e. the param and (unwrapped) return types are
		// related by assignment. A distinct owned value of an unrelated type that
		// merely shares a sub-pointer (e.g. a `Ref` arg and a `Weak` return over
		// the same control block) is NOT an alias; clearing the arg's flag would
		// leak it. (`identity(T x) T` and upcast returns stay covered.)
		relType := retType
		if opt, ok := relType.(*types.Optional); ok {
			relType = opt.Elem()
		}
		if !types.AssignableTo(paramType, relType) && !types.AssignableTo(relType, paramType) {
			continue
		}

		// Extract instance pointers for comparison.
		retPtr := extractAliasPtr(c, result)
		argPtr := extractAliasPtr(c, argVals[i])
		if retPtr == nil || argPtr == nil {
			continue
		}

		ident, isIdent := arg.Value.(*ast.IdentExpr)
		if isIdent {
			dropFlag, ok := c.dropFlags[ident.Name]
			if !ok {
				// T1269: the ident has no drop flag → it is a borrowed param (bare
				// `T[]`/heap param, passed by value; T0998). The callee does not own
				// the arg's buffer, but an owned result aliasing it that escapes into
				// owned storage (`return C(xs: move s)`) or is discarded
				// (`ident(xs).len`) would be freed by BOTH the caller's binding and
				// the escaped/temp owner → double-free. Owned-source paths (drop
				// flag present) are handled below; here we record the arg pointer and
				// clone the RESULT after the loop iff it actually aliases at runtime,
				// leaving the borrow owned solely by the caller.
				borrowAliasPtrs = append(borrowAliasPtrs, argPtr)
				continue
			}

			// T1029/T1017: inside a discarded statement, the source local outlives
			// the statement and must remain the single owner of an aliased allocation.
			// Suppress the source drop-flag clear and record the arg's instance
			// pointer; the result temp's flag is cleared instead — via
			// emitDiscardAliasClears at the tracking site (heap user types and the i8*
			// stmtTemp path) and clearDiscardedAliasTempFlag for the top-level result
			// temp — so the allocation is freed once at scope exit (after the local's
			// later uses) rather than at statement end (use-after-free). Each clear is
			// a runtime pointer compare, so a nested call whose aliasing result is
			// consumed within the statement (e.g. `assert(ident(xs)[0] == ...)`) keeps
			// the source alive while its temp, which aliases the same allocation, is
			// neutralized — no double-free, no leak.
			if c.discardedExpr != nil {
				c.discardAliasArgPtrs = append(c.discardAliasArgPtrs, argPtr)
				continue
			}

			// T1031: the result aliases a source local the caller still owns
			// (e.g. `Node x = ident(n);` then `use n`). The original fix cleared
			// the source's drop flag, transferring sole ownership to the new
			// binding — but if the source is used after the call, freeing the
			// shared instance under it (via the new owner's last-use/scope-exit
			// drop) is a use-after-free. Instead, when the alias actually fires at
			// runtime, deep-clone into the source local's own storage so both the
			// source and the new owner hold independent allocations, each dropped
			// exactly once. The source's drop binding reloads from its alloca at
			// scope exit (emitDropCallDirect), so it frees the clone; the new
			// binding owns the original result. This fires only when retPtr ==
			// argPtr, so functions that relocate their param before returning
			// (e.g. sort()'s copy-on-write) hand back a distinct pointer and are
			// left untouched (no spurious clone, no leak).
			if alloca, hasAlloca := c.locals[ident.Name]; hasAlloca {
				same := c.block.NewICmp(enum.IPredEQ, retPtr, argPtr)
				dupBlock := c.newBlock("alias.dup")
				contBlock := c.newBlock("alias.cont")
				c.block.NewCondBr(same, dupBlock, contBlock)
				c.block = dupBlock
				dupVal, didDup := c.dupOwnedReturnValue(argVals[i], paramType)
				if didDup {
					// Independent clone produced — store it into the source local's
					// storage so the source drops the clone and the new binding keeps
					// the original. Both are owned and dropped exactly once.
					c.claimStringTemp(dupVal)
					c.claimHeapTemp(dupVal)
					c.claimEnvTemp(dupVal)
					c.block.NewStore(dupVal, alloca)
				} else {
					// No deep clone available for this shape — fall back to the
					// original behavior: transfer sole ownership to the new binding by
					// clearing the source's drop flag, avoiding the double-free. The
					// common heap/string/vector/enum/tuple/channel/Arc/Weak/Optional
					// cases are all covered by the clone path above, so this fires only
					// for the rare droppable-but-non-clonable shape (where the
					// source-still-live UAF would otherwise reappear, as before).
					c.block.NewStore(constant.NewInt(irtypes.I1, 0), dropFlag)
				}
				c.block.NewBr(contBlock)
				c.block = contBlock
				continue
			}

			// Fallback (source has no addressable storage — rare): preserve the
			// original drop-flag clear to avoid a double-free.
			same := c.block.NewICmp(enum.IPredEQ, retPtr, argPtr)
			clearBlock := c.newBlock("alias.clear")
			skipBlock := c.newBlock("alias.skip")
			c.block.NewCondBr(same, clearBlock, skipBlock)
			c.block = clearBlock
			c.block.NewStore(constant.NewInt(irtypes.I1, 0), dropFlag)
			c.block.NewBr(skipBlock)
			c.block = skipBlock
			continue
		}

		// B0359: Non-ident args (e.g., vector literals) may be tracked as heap temps.
		// If the return value aliases such an arg, clear the heap temp's drop flag
		// to prevent use-after-free (the caller will own the value via the return).
		if htIdx, ok := c.heapTempMap[argVals[i]]; ok {
			ht := c.heapTemps[htIdx]
			same := c.block.NewICmp(enum.IPredEQ, retPtr, argPtr)
			clearBlock := c.newBlock("alias.ht.clear")
			skipBlock := c.newBlock("alias.ht.skip")
			c.block.NewCondBr(same, clearBlock, skipBlock)
			c.block = clearBlock
			c.block.NewStore(constant.NewInt(irtypes.I1, 0), ht.dropFlag)
			c.block.NewBr(skipBlock)
			c.block = skipBlock
		}

		// T0331: Non-ident args from CallExpr (e.g., f(g())) are tracked as
		// stmtTemps via expr.go's CallExpr case. If the return aliases such
		// a temp (e.g., identity-style functions like Random.shuffle which
		// return their input), clear the stmtTemp's drop flag — otherwise
		// the caller's stmtTemp cleanup and the variable's drop binding
		// would both free the same allocation.
		if stIdx, ok := c.stmtTempMap[argVals[i]]; ok && stIdx >= 0 {
			st := c.stmtTemps[stIdx]
			same := c.block.NewICmp(enum.IPredEQ, retPtr, argPtr)
			clearBlock := c.newBlock("alias.st.clear")
			skipBlock := c.newBlock("alias.st.skip")
			c.block.NewCondBr(same, clearBlock, skipBlock)
			c.block = clearBlock
			c.block.NewStore(constant.NewInt(irtypes.I1, 0), st.dropFlag)
			c.block.NewBr(skipBlock)
			c.block = skipBlock
		}
	}

	// T1269: a borrowed-param arg the owned result may alias. Deep-clone the result
	// so the caller's borrow keeps sole ownership of its buffer and the escaping /
	// discarded owned result frees an independent allocation. The clone fires ONLY
	// when retPtr == argPtr at runtime, so a function that returns a fresh value
	// (retPtr != argPtr) is untouched — no spurious clone, no leak, just pointer
	// compares. Unlike the T1031 owned-local path (which clones into the SOURCE's
	// storage and keeps the original as the binding), here the borrow must never be
	// touched, so we clone the RESULT and hand that back — uniform for both the
	// move-into-owned-storage and discard escape shapes.
	if len(borrowAliasPtrs) > 0 && c.block != nil && c.block.Term == nil {
		if retPtr := extractAliasPtr(c, result); retPtr != nil {
			var aliases value.Value
			for _, ap := range borrowAliasPtrs {
				cmp := c.block.NewICmp(enum.IPredEQ, retPtr, ap)
				if aliases == nil {
					aliases = cmp
				} else {
					aliases = c.block.NewOr(aliases, cmp)
				}
			}
			entryBlock := c.block
			cloneBlock := c.newBlock("alias.borrow.clone")
			contBlock := c.newBlock("alias.borrow.cont")
			entryBlock.NewCondBr(aliases, cloneBlock, contBlock)

			c.block = cloneBlock
			dup, didDup := c.dupOwnedReturnValue(result, retType)
			// dupOwnedReturnValue may split the block (null-check diamonds); the phi
			// incoming for the clone path must use the block it actually left us in.
			cloneEnd := c.block
			cloneEnd.NewBr(contBlock)

			c.block = contBlock
			if didDup {
				// The clone is NOT claimed as a temp: the dup helpers do not register
				// temps, and `result` is only tracked downstream at the binding /
				// discard site. Returning the phi lets that single tracking free the
				// clone exactly once; the original borrow-aliasing value is discarded
				// on the clone path and never tracked, so the caller's borrow remains
				// the sole owner of the original buffer.
				result = c.block.NewPhi(
					ir.NewIncoming(result, entryBlock),
					ir.NewIncoming(dup, cloneEnd),
				)
			}
			// !didDup: no deep clone available for this droppable-but-non-clonable
			// shape — fall through with the original result (current behavior, no
			// regression versus today which also does nothing there).
		}
	}

	return result
}

// clearDiscardedAliasTempFlag completes the T1017 discard-path alias handling.
// emitReturnAliasCheckSubst recorded (in argPtrs) the live-local arg pointers that
// a discarded call's return value may alias, without clearing their drop flags.
// Here, after the result has been tracked as a temp, emit
// `if retPtr == argPtr { clear resultTempFlag }` for each recorded arg so the
// aliasing result temp is not dropped (the live local remains the sole owner,
// dropped once at scope exit). No-op when nothing was recorded.
func (c *Compiler) clearDiscardedAliasTempFlag(result value.Value, argPtrs []value.Value) {
	if len(argPtrs) == 0 {
		return
	}
	if result == nil || c.block == nil || c.block.Term != nil {
		return
	}
	retPtr := extractAliasPtr(c, result)
	if retPtr == nil {
		return
	}
	// String/vector/channel results are tracked as stmtTemps keyed by the result
	// value itself (an i8*), so a direct map lookup matches. Clear that flag when
	// the result aliases a recorded live-local arg.
	if idx, ok := c.stmtTempMap[result]; ok && idx >= 0 {
		resultFlag := c.stmtTemps[idx].dropFlag
		for _, argPtr := range argPtrs {
			same := c.block.NewICmp(enum.IPredEQ, retPtr, argPtr)
			clearBlock := c.newBlock("alias.discard.clear")
			skipBlock := c.newBlock("alias.discard.skip")
			c.block.NewCondBr(same, clearBlock, skipBlock)
			c.block = clearBlock
			c.block.NewStore(constant.NewInt(irtypes.I1, 0), resultFlag)
			c.block.NewBr(skipBlock)
			c.block = skipBlock
		}
		return
	}
	// Heap user-type results are tracked as heapTemps keyed by an extractvalue SSA
	// value produced at track time — distinct from our freshly-extracted retPtr, so
	// heapTempMap[retPtr] misses. Instead scan the tracked heap temps and clear any
	// whose stored instance pointer equals a recorded live-local arg pointer at
	// runtime: that temp is exactly the discarded result aliasing the still-owned
	// local (which is dropped once at scope exit). A non-aliasing discarded result
	// has a distinct pointer, matches nothing, and is freed normally at stmt end.
	for _, argPtr := range argPtrs {
		for _, temp := range c.heapTemps {
			tracked := c.block.NewLoad(irtypes.I8Ptr, temp.alloca)
			same := c.block.NewICmp(enum.IPredEQ, tracked, argPtr)
			clearBlock := c.newBlock("alias.discard.clear")
			skipBlock := c.newBlock("alias.discard.skip")
			c.block.NewCondBr(same, clearBlock, skipBlock)
			c.block = clearBlock
			c.block.NewStore(constant.NewInt(irtypes.I1, 0), temp.dropFlag)
			c.block.NewBr(skipBlock)
			c.block = skipBlock
		}
	}
}

// extractAliasPtr returns the instance pointer from a value for aliasing comparison.
// For i8* values (string, vector, channel): returns the value directly.
// For value structs {i8*, i8*}: extracts field 1 (the instance pointer).
// For Optional structs {i1, X}: extracts field 1 and recurses (T0391).
func extractAliasPtr(c *Compiler, v value.Value) value.Value {
	if v == nil {
		return nil
	}
	// i8* values: string instance ptr, vector header ptr, channel ptr
	if v.Type() == irtypes.I8Ptr {
		return v
	}
	// Struct values: value struct {i8*, i8*}, Optional {i1, X}, or other.
	if st, ok := v.Type().(*irtypes.StructType); ok && len(st.Fields) == 2 {
		// Value structs {i8*, i8*}: extract instance pointer (field 1)
		if st.Fields[0] == irtypes.I8Ptr && st.Fields[1] == irtypes.I8Ptr {
			return c.block.NewExtractValue(v, 1)
		}
		// T0391: Optional struct {i1, X} — extract field 1 and recurse so we
		// reach the heap pointer through any number of Optional layers.
		// {i1, {i8*, i8*}} → {i8*, i8*} → i8*  (Optional[heap user type])
		// {i1, i8*}        → i8*               (Optional[string|vector|channel])
		// {i1, {i1, ...}}  → recurse           (Optional[Optional[...]])
		// {i1, i64}        → i64 → returns nil (Optional[primitive], non-droppable)
		if st.Fields[0] == irtypes.I1 {
			inner := c.block.NewExtractValue(v, 1)
			return extractAliasPtr(c, inner)
		}
	}
	return nil
}

// findInnerCallExpr peels through unwrap/propagate/parenthesis layers to find
// the underlying CallExpr (used to derive the receiver for alias checks in
// trackHeapUserTypeResult). Returns nil if the chain doesn't bottom out in a
// call.
func findInnerCallExpr(expr ast.Expr) *ast.CallExpr {
	for {
		switch e := expr.(type) {
		case *ast.CallExpr:
			return e
		case *ast.ParenExpr:
			expr = e.Expr
		case *ast.ErrorPanicExpr:
			expr = e.Expr
		case *ast.ErrorPropagateExpr:
			expr = e.Expr
		case *ast.OptionalUnwrapExpr:
			expr = e.Expr
		case *ast.AutoCloneExpr: // T0605
			expr = e.Expr
		case *ast.ErrorHandlerExpr:
			expr = e.Expr
		default:
			return nil
		}
	}
}

// resolvedExprType returns sema's type for e with the active generic and Self
// substitutions applied.
func (c *Compiler) resolvedExprType(e ast.Expr) types.Type {
	t := c.info.Types[e]
	if t == nil {
		// Defensive: sema Types map may be absent for synthesized AST nodes.
		return nil
	}
	if c.typeSubst != nil {
		t = types.Substitute(t, c.typeSubst)
	}
	if c.selfSubst != nil {
		t = types.SubstituteSelf(t, c.selfSubst.iface, c.selfSubst.concrete)
	}
	return t
}

// closureResultMayAliasCallInput reports whether a call result that is a closure
// might hand back a closure it does not own — i.e. one reachable from an argument
// or from the receiver's fields. Conservative: any closure mentioned anywhere in
// an argument type or (transitively) in the receiver type suppresses tracking, so
// the worst case is a pre-existing leak (T1160), never a double free.
// T1227 made returning a borrowed closure FIELD a compile error, so the receiver
// field walk and needsVtable check are now dead in the real pipeline (ownership
// rejects those patterns before codegen); they remain as defense-in-depth for IR
// generated outside the ownership pass (e.g. Go unit tests via generateIR).
// Also called from trackGetterResult (to decide whether a getter's closure temp is
// tracked at all) and isClosureAggregateBorrow (to decide whether a var-decl binding
// of a getter result owns the closure) — T1227 only bars a getter from returning a
// closure FIELD directly; a borrowed container ELEMENT is still a legal getter
// result, so getter results are not unconditionally fresh.
func (c *Compiler) closureResultMayAliasCallInput(expr ast.Expr) bool {
	// Peel the failable-unwrap layers, which only extract the success value of the
	// inner call — ownership of the closure is the inner call's.
	// Not peeled: OptionalUnwrapExpr and ErrorHandlerExpr (owner-governed sources
	// fall through to the conservative default); ParenExpr (genExpr recurses straight
	// through parens before any tracking call, so no caller ever passes one here);
	// AutoCloneExpr (only synthesized in clone bodies by sema, and Signature types are
	// non-Cloneable per T0813, so a Signature-typed AutoCloneExpr cannot exist).
peel:
	for {
		switch e := expr.(type) {
		case *ast.ErrorPanicExpr:
			expr = e.Expr
		case *ast.ErrorPropagateExpr:
			expr = e.Expr
		default:
			break peel
		}
	}
	var receiver ast.Expr
	switch e := expr.(type) {
	case *ast.CallExpr:
		// An indirect call through a closure VALUE (`f()`, `o!()`, `(g)()`) may hand
		// back a closure its own env owns — the env's drop fn frees that nested env,
		// so freeing the result as a temp double-frees. Only a statically-known
		// function/method callee is analyzable by the arg/receiver checks below.
		if c.isClosureValueCallee(e.Callee) {
			return true
		}
		for _, arg := range e.Args {
			if c.typeMentionsSignature(c.resolvedExprType(arg.Value), map[*types.TypeName]bool{}) {
				return true
			}
		}
		// An explicit type-argument list (`mk[int](…)`, `box.transform[string](…)`)
		// wraps the real callee in an IndexExpr — peel it so a generic method's
		// receiver still reaches the alias check below.
		callee := e.Callee
		if idx, ok := callee.(*ast.IndexExpr); ok && c.isGenericInstantiation(idx) {
			callee = idx.Target
		}
		if mem, ok := callee.(*ast.MemberExpr); ok {
			receiver = mem.Target
		}
	case *ast.MemberExpr: // getter
		receiver = e.Target
	default:
		// Operator results and other shapes: keep today's behavior (no tracking).
		return true
	}
	if receiver == nil {
		return false
	}
	// `mod.make_adder(x)` — the "receiver" names a module, not a value, so the
	// call is a free function with no receiver to alias.
	if ident, ok := receiver.(*ast.IdentExpr); ok && c.resolveModuleName(ident) != "" {
		return false
	}
	rt := c.resolvedExprType(receiver)
	if rt == nil {
		return true
	}
	// Virtual dispatch: the receiver's runtime type is any type in its subtree, so an
	// override can hand back a field the static receiver type does not even have — a
	// `VBase`-typed receiver whose runtime type is a `VDerived` holding a closure
	// field dispatches to `VDerived.get_cb` returning `this.cb`, an alias the
	// receiver still owns. Walk the whole subtree, not just `rt`. A structural
	// receiver's implementers are open-ended (any type can satisfy it), so it stays
	// unconditionally conservative.
	if named := extractNamed(rt); named != nil {
		if isStructuralView(named) {
			return true
		}
		if c.needsVtable(named) {
			return c.namedSubtreeMentionsSignature(named)
		}
	}
	return c.typeMentionsSignature(rt, map[*types.TypeName]bool{})
}

// namedSubtreeMentionsSignature reports whether `root` — or any type that inherits
// from it, transitively — holds a closure anywhere in its fields, i.e. whether a
// vtable dispatch on a `root`-typed receiver could land on an override handing back
// a closure the receiver owns rather than a fresh one (T1160/T1227).
func (c *Compiler) namedSubtreeMentionsSignature(root *types.Named) bool {
	visited := map[*types.Named]bool{}
	var walk func(n *types.Named) bool
	walk = func(n *types.Named) bool {
		if visited[n] {
			return false
		}
		visited[n] = true
		if c.typeMentionsSignature(n, map[*types.TypeName]bool{}) {
			return true
		}
		for _, child := range c.directChildren[n] {
			if walk(child) {
				return true
			}
		}
		return false
	}
	return walk(root)
}

// isClosureValueCallee reports whether a call's callee denotes a closure *value*
// (local, param, capture, or any expression producing a fat pointer) rather than a
// statically-known function or method declaration. genCallExpr dispatches exactly
// these through genIndirectCall. The callee's env is opaque here, so a closure it
// returns may be one the env owns (`f := move || -> h; f();`) rather than a fresh one.
func (c *Compiler) isClosureValueCallee(callee ast.Expr) bool {
	for {
		p, ok := callee.(*ast.ParenExpr)
		if !ok {
			break
		}
		callee = p.Expr
	}
	if _, isSig := c.resolvedExprType(callee).(*types.Signature); !isSig {
		// Defensive: callee is non-Signature (e.g. nil or non-function type); not a closure callee.
		return false
	}
	switch ce := callee.(type) {
	case *ast.IdentExpr:
		// A declared free function is analyzable; a var/param/capture holding a
		// closure is not. A missing object resolves conservatively to "value".
		_, isFunc := c.info.Objects[ce].(*types.Func)
		return !isFunc
	case *ast.MemberExpr:
		// Method, module-qualified function, or closure-typed field. A field call
		// aliases the receiver's env and is caught by the receiver check.
		return false
	case *ast.IndexExpr:
		// `mk[int](…)` / `box.transform[string](…)`: an explicit type-argument list
		// on a statically-known generic function or method, not a subscript. Only a
		// value subscript (`fns[0](x)`) is a real closure value.
		return !c.isGenericInstantiation(ce)
	}
	// `o!()`, `make()()` — a fat pointer materialized from an expression.
	return true
}

// isGenericInstantiation reports whether an IndexExpr in callee position is a
// type-argument list on a generic function/method (`mk[int]`, `box.transform[T]`)
// rather than a value subscript yielding a callable (`fns[0]`, `h.fns[0]`).
// Mirrors the gate genCallExpr uses to route to genGenericFuncCall (T0674).
func (c *Compiler) isGenericInstantiation(idx *ast.IndexExpr) bool {
	sig, ok := c.resolvedExprType(idx.Target).(*types.Signature)
	return ok && len(sig.TypeParams()) > 0
}

// typeMentionsSignature reports whether t is, or transitively contains, a
// function type. `seen` guards against recursive type definitions.
func (c *Compiler) typeMentionsSignature(t types.Type, seen map[*types.TypeName]bool) bool {
	switch tt := t.(type) {
	case nil:
		// Defensive: nil type in a field/variant slot; treat as no closure.
		return false
	case *types.Signature:
		return true
	case *types.Optional:
		return c.typeMentionsSignature(tt.Elem(), seen)
	case *types.SharedRef:
		return c.typeMentionsSignature(tt.Elem(), seen)
	case *types.MutRef:
		return c.typeMentionsSignature(tt.Elem(), seen)
	case *types.Array:
		return c.typeMentionsSignature(tt.Elem(), seen)
	case *types.Tuple:
		for _, el := range tt.Elems() {
			if c.typeMentionsSignature(el, seen) {
				return true
			}
		}
		return false
	}
	if elem, ok := types.AsVector(t); ok {
		return c.typeMentionsSignature(elem, seen)
	}
	if k, v, ok := types.AsMap(t); ok {
		return c.typeMentionsSignature(k, seen) || c.typeMentionsSignature(v, seen)
	}
	if inst, ok := t.(*types.Instance); ok {
		for _, ta := range inst.TypeArgs() {
			if c.typeMentionsSignature(ta, seen) {
				return true
			}
		}
		return c.typeMentionsSignature(inst.Origin(), seen)
	}
	if enum, ok := t.(*types.Enum); ok {
		if seen[enum.Obj()] {
			return false
		}
		seen[enum.Obj()] = true
		for _, variant := range enum.Variants() {
			for _, f := range variant.Fields() {
				if c.typeMentionsSignature(f.Type(), seen) {
					return true
				}
			}
		}
		return false
	}
	if named, ok := t.(*types.Named); ok {
		if seen[named.Obj()] {
			return false
		}
		seen[named.Obj()] = true
		// AllFields, not Fields: a method/getter returning a closure field inherited
		// from a parent (`type Child is Base` handing back `Base.cb`) aliases the
		// receiver's env just as much, and Fields() covers only own fields.
		for _, f := range named.AllFields() {
			if c.typeMentionsSignature(f.Type(), seen) {
				return true
			}
		}
		return false
	}
	return false
}

// isIteratorAdapterName reports whether name is one of the built-in iterator /
// generator adapter type names whose boxes are managed by the iter-cleanup path
// (recursive `_FnIter` parent chain) rather than plain RTTI structural drop.
// T1294: a free function returning such an adapter must NOT be routed through
// structuralDrop — its non-standard-RTTI `_FnIter`-shaped box would be
// double-dropped / crash.
func isIteratorAdapterName(name string) bool {
	switch name {
	case "Iterator", "_FnIter", "Stream", "Generator":
		return true
	}
	return false
}

// argMayAliasStructuralReturn reports whether a call argument carries a heap box
// that a callee could return by alias (e.g. `pass_through(Sink s) Sink { return
// s; }`, or a container arg whose element/field is returned by view). It is true
// when the arg (peeling parens) has a heap reference type — a named type that is
// NOT a value type, NOT copy, and NOT a primitive scalar. Scalars, value types,
// and copy types carry no heap box for the return to alias, so they can never be
// the source of an alias-based double-free.
//
// This is the arg-side heap-box predicate. Whether such an arg's box is ACTUALLY
// a possible alias of the return is decided per-parameter in
// isFreshOwnedStructuralCall using the sema return-alias fact (T1305). T1294.
func (c *Compiler) argMayAliasStructuralReturn(a ast.Expr) bool {
	a = unwrapDestructureParens(a)
	named := extractNamed(c.resolvedExprType(a))
	if named == nil {
		return false
	}
	return !named.IsValueType() && !named.IsCopy() && !isPrimitiveScalar(named)
}

// structuralReturnAliasCallee resolves a call's callee to the free-function object
// it names (or nil for method / non-ident callees), so its sema return-alias fact
// can be looked up.
func (c *Compiler) structuralReturnAliasCallee(call *ast.CallExpr) *types.Func {
	ident, ok := call.Callee.(*ast.IdentExpr)
	if !ok {
		return nil
	}
	fn, _ := c.info.Objects[ident].(*types.Func)
	return fn
}

// argsAlignPositionally reports whether a call's arguments line up 1:1 with the
// signature's parameters — no named args, no variadic, exact arity. Only then can
// the per-parameter return-alias fact (indexed positionally) be applied safely.
func argsAlignPositionally(call *ast.CallExpr, sig *types.Signature) bool {
	if sig.IsVariadic() || len(call.Args) != len(sig.Params()) {
		return false
	}
	for _, a := range call.Args {
		if a.Name != "" {
			return false
		}
	}
	return true
}

// isFreshOwnedStructuralCall reports whether expr is a plain free-function
// CallExpr whose non-value structural-interface return is a freshly-constructed
// OWNED box (must be freed at statement end) rather than a borrowed alias of the
// receiver or of an argument (must NOT be freed). T1294: a discarded/inline
// structural result from such a call — `show(1);` or `show(3).to_string()` —
// otherwise leaks its heap box, because trackHeapUserTypeResult only tracks
// unwrap/handler/getter sources.
//
// The classification admits ONLY the provably-fresh, non-aliasing shape:
//   - !isBorrowedExpr: excludes borrow-returning (`T&`/`T~`) calls.
//   - callee is a plain free function (IdentExpr) OR a module-qualified free
//     function (`mod.fresh_return()`, a MemberExpr whose Target resolves via
//     resolveModuleName; T1327) — both own their fresh box. A MemberExpr that is
//     a genuine method call is excluded: it can hand back `this` (`c.get_self()`)
//     or an iterator adapter over the receiver (`v.iter()`), whose box the
//     receiver / iter-cleanup already frees.
//   - return type is not an iterator/generator adapter: excludes free-function
//     generator returns whose non-standard-RTTI box would be double-dropped.
//   - no argument's heap box can be returned by alias: excludes `pass_through(c)`
//     (returns its param, aliasing the caller's still-owned local) and
//     `pass_through(mk(1))` (returns its param, aliasing a fresh temp that is
//     ITSELF temp-tracked — tracking the outer too would double-free the same box;
//     T1294 segfault). T1305: per argument, a heap box disqualifies the call ONLY
//     when the callee's return can actually alias that parameter. The sema fact
//     StructuralReturnAliasParams[fn][i] answers this; when the args align 1:1 with
//     the params, an arg is admitted (does not disqualify) when it is consumed by a
//     move param / call-site `move` (caller relinquished the box), or when the fact
//     proves the return does not alias parameter i (e.g. an owned index clone
//     `return v[0]` or a fresh construction `return Widget(...)`). Absent fact or
//     non-positional args ⇒ conservative reject (a possible leak, never a crash).
func (c *Compiler) isFreshOwnedStructuralCall(expr ast.Expr, rt types.Type) bool {
	call, ok := expr.(*ast.CallExpr)
	if !ok {
		return false
	}
	if c.isBorrowedExpr(expr) {
		return false
	}
	if mem, isMember := call.Callee.(*ast.MemberExpr); isMember {
		// T1327: a module-qualified free-function call (`mod.fresh_return()`) has a
		// MemberExpr callee whose Target is a module name — not a method call. Its
		// fresh non-value structural return is an owned box, exactly like the
		// IdentExpr free-function form. Admit it and fall through to the arg-alias
		// check below (still declines `mod.pass_through(s)`). Value-receiver method
		// calls (`c.get_self()`, `v.iter()`) still return false.
		ident, isIdent := mem.Target.(*ast.IdentExpr)
		if !isIdent || c.resolveModuleName(ident) == "" {
			return false
		}
	}
	if rtNamed := extractNamed(rt); rtNamed != nil && isIteratorAdapterName(rtNamed.Obj().Name()) {
		return false
	}
	var sig *types.Signature
	var alias []bool
	var known bool
	if fn := c.structuralReturnAliasCallee(call); fn != nil {
		sig, _ = fn.Type().(*types.Signature)
		alias, known = c.info.StructuralReturnAliasParams[fn]
	}
	// A per-parameter fact can be applied only when the args map 1:1 onto the
	// params (so index i is meaningful) and the fact covers every parameter.
	positional := known && sig != nil && len(alias) == len(sig.Params()) &&
		argsAlignPositionally(call, sig)
	for i, a := range call.Args {
		if !c.argMayAliasStructuralReturn(a.Value) {
			continue // no heap box → nothing for the return to alias
		}
		if positional {
			p := sig.Params()[i]
			if a.Move || p.Ref() == types.RefMut {
				continue // consumed (move) → caller relinquished this box
			}
			if !alias[i] {
				continue // return provably does not alias this borrowed arg
			}
		}
		return false // conservative: may alias a still-owned box
	}
	return true
}

// emitReceiverAliasCheck handles T0341's receiver-aliasing case for method
// calls: if a method returns its receiver (e.g., `c.iter()` returning `this`),
// the receiver's owning variable (or `this` parameter) will free the allocation
// at scope exit. The given drop flag is cleared at runtime when the result
// pointer equals the receiver's instance pointer to prevent double-free.
//
// Only side-effect-free receiver expressions are handled (IdentExpr loading a
// local, ThisExpr) — re-evaluating other expressions (chained calls, etc.)
// would risk duplicating side effects. claimHeapTemp already covers the case
// where the receiver is itself a tracked temp.
func (c *Compiler) emitReceiverAliasCheck(e *ast.CallExpr, newTempInstancePtr value.Value, newTempDropFlag value.Value) {
	mem, ok := e.Callee.(*ast.MemberExpr)
	if !ok {
		return
	}
	c.emitReceiverAliasCheckForTarget(mem.Target, newTempInstancePtr, newTempDropFlag)
}

// emitReceiverAliasCheckForTarget emits the runtime alias-clear for a given
// receiver-origin expression (IdentExpr loading a local, or ThisExpr). Split out
// of emitReceiverAliasCheck (T0958) so operator dispatch — which has no CallExpr
// callee — can reach the same clear via operatorReceiverOrigin. The new temp's
// drop flag is cleared at runtime when its instance pointer equals the receiver's
// instance pointer (the operand retains ownership; the temp must not double-free).
func (c *Compiler) emitReceiverAliasCheckForTarget(target ast.Expr, newTempInstancePtr value.Value, newTempDropFlag value.Value) {
	// T0582: peel ParenExpr so `(w).self()` and `((w)).self()` resolve to
	// the underlying IdentExpr/ThisExpr receiver, otherwise the switch
	// would fall through to `default: return` → no alias check → double-free
	// on discard statements like `(w).self();`.
	target = unwrapDestructureParens(target)

	var recvInstPtr value.Value
	switch t := target.(type) {
	case *ast.IdentExpr:
		alloca, isLocal := c.locals[t.Name]
		if !isLocal {
			return
		}
		structTy, isStruct := alloca.ElemType.(*irtypes.StructType)
		if !isStruct || len(structTy.Fields) != 2 {
			return
		}
		recvVal := c.block.NewLoad(alloca.ElemType, alloca)
		recvInstPtr = c.block.NewExtractValue(recvVal, 1)
	case *ast.ThisExpr:
		alloca, isLocal := c.locals["this"]
		if !isLocal {
			return
		}
		thisVal := c.block.NewLoad(alloca.ElemType, alloca)
		if structTy, isStruct := thisVal.Type().(*irtypes.StructType); isStruct && len(structTy.Fields) == 2 {
			recvInstPtr = c.block.NewExtractValue(thisVal, 1)
		} else if thisVal.Type() == irtypes.I8Ptr {
			recvInstPtr = thisVal
		} else {
			return
		}
	default:
		return
	}
	if recvInstPtr == nil {
		return
	}
	if recvInstPtr.Type() != irtypes.I8Ptr {
		if _, isPtr := recvInstPtr.Type().(*irtypes.PointerType); isPtr {
			recvInstPtr = c.block.NewBitCast(recvInstPtr, irtypes.I8Ptr)
		} else {
			return
		}
	}
	same := c.block.NewICmp(enum.IPredEQ, newTempInstancePtr, recvInstPtr)
	clearBlk := c.newBlock("recv.alias.clear")
	skipBlk := c.newBlock("recv.alias.skip")
	c.block.NewCondBr(same, clearBlk, skipBlk)
	c.block = clearBlk
	c.block.NewStore(constant.NewInt(irtypes.I1, 0), newTempDropFlag)
	c.block.NewBr(skipBlk)
	c.block = skipBlk
}

// emitDiscardAliasClears clears the result temp's drop flag at runtime when the
// result instance pointer equals any owned-local arg pointer whose source-flag
// clear was suppressed in the current discarded statement (T1029). This keeps the
// source local as the single owner of an aliased allocation, so it is freed once at
// scope exit (after the local's later uses) instead of at statement end via the
// temp cleanup (which would dangle the still-live local). instPtr and each recorded
// argPtr are i8* instance pointers.
func (c *Compiler) emitDiscardAliasClears(instPtr value.Value, dropFlag value.Value) {
	if instPtr == nil || dropFlag == nil || c.block == nil || c.block.Term != nil {
		return
	}
	if instPtr.Type() != irtypes.I8Ptr {
		return
	}
	for _, argPtr := range c.discardAliasArgPtrs {
		if argPtr == nil || argPtr.Type() != irtypes.I8Ptr {
			continue
		}
		same := c.block.NewICmp(enum.IPredEQ, instPtr, argPtr)
		clearBlk := c.newBlock("discard.alias.clear")
		skipBlk := c.newBlock("discard.alias.skip")
		c.block.NewCondBr(same, clearBlk, skipBlk)
		c.block = clearBlk
		c.block.NewStore(constant.NewInt(irtypes.I1, 0), dropFlag)
		c.block.NewBr(skipBlk)
		c.block = skipBlk
	}
}

// hasStructuralParam returns true if any parameter of sig is a structural interface (T0092).
func hasStructuralParam(sig *types.Signature, typeSubst map[*types.TypeParam]types.Type) bool {
	for _, param := range sig.Params() {
		pt := param.Type()
		if typeSubst != nil {
			pt = types.Substitute(pt, typeSubst)
		}
		if named := extractNamed(pt); isStructuralView(named) {
			return true
		}
	}
	return false
}
