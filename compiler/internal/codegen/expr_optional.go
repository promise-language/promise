package codegen

import (
	"github.com/llir/llvm/ir"
	"github.com/llir/llvm/ir/constant"
	irtypes "github.com/llir/llvm/ir/types"
	"github.com/llir/llvm/ir/value"

	"github.com/promise-language/promise/compiler/internal/ast"
	"github.com/promise-language/promise/compiler/internal/sema"
	"github.com/promise-language/promise/compiler/internal/types"
)

// --- Optional ---

func (c *Compiler) genNoneLit(e *ast.NoneLit) value.Value {
	if c.targetType != nil {
		lt := c.resolveType(c.targetType)
		return c.zeroValue(lt)
	}
	return constant.NewInt(irtypes.I1, 0) // void optional fallback
}

// wrapOptional wraps a value into an optional struct: { true, val }.
func (c *Compiler) wrapOptional(val value.Value, optType *irtypes.StructType) value.Value {
	var agg value.Value = constant.NewUndef(optType)
	agg = c.block.NewInsertValue(agg, constant.NewInt(irtypes.I1, 1), 0)
	agg = c.block.NewInsertValue(agg, val, 1)
	return agg
}

// wrapArmValueOptional coerces a match/if arm value to the shared Optional
// result shape (T1189). When the arms unify to Optional[T] but this arm produced
// the bare inner `T` value (e.g. a value arm sibling of a `none` arm), wrap it as
// `{ i1 true, T }` so every arm — and the merge phi — shares the `{ i1, T }`
// type. A `none` arm (already the optional zero via genNoneLit's targetType) and
// an already-optional arm produce the struct shape and pass through unchanged.
func (c *Compiler) wrapArmValueOptional(val value.Value, resultType types.Type) value.Value {
	if val == nil || resultType == nil {
		return val
	}
	rt := resultType
	if c.typeSubst != nil {
		rt = types.Substitute(rt, c.typeSubst)
	}
	opt, ok := rt.(*types.Optional)
	if !ok {
		return val
	}
	optLL, ok := c.resolveType(rt).(*irtypes.StructType) // void-optional resolves to i1 → skip
	if !ok {
		return val
	}
	if val.Type().Equal(c.resolveType(opt.Elem())) { // produced the bare inner T
		return c.wrapOptional(val, optLL)
	}
	return val // already {i1,T} (none arm or optional-typed arm)
}

// coerceNoneToOptional coerces a none-typed value (the i1 void-optional bound
// from a bare `none` / all-`none` match-if) to the concrete Optional[T] zero
// expected at a consumption site. A bare `none` LITERAL already produced the
// target struct via targetType, so a val already matching passes through. T1190.
func (c *Compiler) coerceNoneToOptional(val value.Value, exprType, targetType types.Type) value.Value {
	if val == nil || targetType == nil {
		return val
	}
	if c.typeSubst != nil && exprType != nil {
		exprType = types.Substitute(exprType, c.typeSubst)
	}
	if exprType != types.TypNone {
		return val
	}
	if st, ok := c.resolveType(targetType).(*irtypes.StructType); ok && !val.Type().Equal(st) {
		return c.zeroValue(st)
	}
	return val
}

// wrapReturnOptional wraps val in an Optional struct if retType is Optional
// but the expression type is a non-optional, non-none value.
func (c *Compiler) wrapReturnOptional(val value.Value, expr ast.Expr, retType types.Type) value.Value {
	if retType == nil {
		return val
	}
	if _, isOpt := retType.(*types.Optional); !isOpt {
		return val
	}
	exprType := c.info.Types[expr]
	if c.typeSubst != nil {
		exprType = types.Substitute(exprType, c.typeSubst)
	}
	// NoneLit already produces the correct zero value via targetType; a
	// none-typed variable-read is coerced to the concrete Optional zero. T1190.
	if exprType == types.TypNone {
		return c.coerceNoneToOptional(val, exprType, retType)
	}
	// Same shape — no wrapping needed. Use Identical (not "is exprOpt?") so
	// returning T? from a T??-returning function still wraps.
	if types.Identical(exprType, retType) {
		return val
	}
	lt := c.resolveType(retType)
	if st, ok := lt.(*irtypes.StructType); ok {
		return c.wrapOptional(val, st)
	}
	return val
}

// coerceReturnToOptionalElem view-coerces/boxes a return value into the ELEMENT
// type of an Optional return type before wrapReturnOptional wraps it, so a
// concrete → structural-interface or child → parent return (e.g. `return
// Counter(...)` from a `Sink?`-returning function) becomes a proper view under
// the optional (T1298). Computes the source type exactly as the trailing
// coerceToView site does (typeSubst + selfSubst applied) so it stays in sync.
// No-op when retType isn't Optional or no coercion is needed.
func (c *Compiler) coerceReturnToOptionalElem(val value.Value, expr ast.Expr, retType types.Type) value.Value {
	if retType == nil {
		return val
	}
	if _, isOpt := retType.(*types.Optional); !isOpt {
		return val
	}
	exprType := c.info.Types[expr]
	if c.typeSubst != nil {
		exprType = types.Substitute(exprType, c.typeSubst)
	}
	if c.selfSubst != nil {
		exprType = types.SubstituteSelf(exprType, c.selfSubst.iface, c.selfSubst.concrete)
	}
	return c.coerceToOptionalElem(val, exprType, retType)
}

func (c *Compiler) genElvis(e *ast.BinaryExpr) value.Value {
	// T0954: capture+reset the consumed-by-await signal so it does not leak into
	// operand subexpressions (a nested elvis in e.Left/e.Right is not itself the
	// await operand). Used below to neutralize the none-path default's owner.
	consumedByReceive := c.elvisResultConsumed
	c.elvisResultConsumed = false
	// T0952: capture+reset the bound-result signal (set by the var-decl/assignment
	// RHS-eval sites in stmt.go). Same reasoning — a nested elvis in e.Left/e.Right
	// is not itself the bound RHS, so it must not inherit this flag.
	boundResult := c.elvisResultBound
	c.elvisResultBound = false
	// T0982: capture+reset the returned-result signal (set by genReturnStmt's
	// RHS-eval site). A returned elvis escapes to the caller, so — like a bound
	// result — a handle/heap none-path owned-local default must be neutralized here
	// (else the function's scope-exit drop AND the caller both free it → double
	// free/SEGV). Unlike boundResult, it does NOT create a per-path elvisBoundDropFlag.
	returnedResult := c.elvisResultReturned
	c.elvisResultReturned = false
	// T1166: capture+reset the force-own-clone signal (set by the member/index
	// assignment-target RHS-eval site in stmt.go). Reset so a nested elvis in
	// e.Left/e.Right does not inherit it, exactly like elvisResultBound.
	ownsForced := c.elvisResultOwnsForced
	c.elvisResultOwnsForced = false
	// T0940/T0981: defensively clear any stale per-path bound flag before this elvis
	// computes its own. Consumed by the var-decl binding in stmt.go.
	c.elvisBoundDropFlag = nil

	// T1166: for a member/index owned target, precompute the cloneable-droppable gate
	// and resolved result type once. Force-clone only the representations
	// cloneResolvedValue handles safely — Vector/string (elvisResultDrop) and
	// Map/Set/heap-user (elvisResultHeapDrop); single-owner native handles
	// (elvisResultHandleDrop) are not cloneable and sema rejects those operand shapes.
	_, _, vecOrStr := c.elvisResultDrop(e)
	forceOwnClone := ownsForced && (vecOrStr || c.elvisResultHeapDrop(e) != nil)
	resolvedElvisType := c.info.Types[e]
	if c.typeSubst != nil && resolvedElvisType != nil {
		resolvedElvisType = types.Substitute(resolvedElvisType, c.typeSubst)
	}

	// T0940: in a BOUND droppable context the var-decl set dup-on-read flags
	// (T0095/B0219/T0366/…) so genFieldAccess/genVectorIndex CLONES a member/index
	// source's inner into a fresh buffer the binding owns. Snapshot that now — before
	// e.Left's eval consumes (and clears) the flag — so the some-path bound flag marks
	// ownership of the clone. elvisSomeInnerOrphaned reports a member/index source as
	// container-owned (correct for the INLINE case, which does NOT dup); this promotes
	// it to owned only when a clone actually happens (the dup flag is live AND the
	// source is a field/index read).
	boundSourceDupCloned := false
	if boundResult {
		switch unwrapDestructureParens(e.Left).(type) {
		case *ast.MemberExpr, *ast.IndexExpr:
			boundSourceDupCloned = c.dupStringFieldAccess || c.dupContainerFieldAccess ||
				c.dupHeapUserFieldAccess || c.dupTupleFieldAccess
		}
	}

	optVal := c.genExprAutoPropagate(e.Left)

	// Extract the present flag (field 0)
	flag := c.block.NewExtractValue(optVal, 0)

	someBlock := c.newBlock("elvis.some")
	noneBlock := c.newBlock("elvis.none")
	mergeBlock := c.newBlock("elvis.merge")

	c.block.NewCondBr(flag, someBlock, noneBlock)

	// Some path: extract inner value
	c.block = someBlock
	var someVal value.Value = c.block.NewExtractValue(optVal, 1) // T1166: widened for the force-own-clone reassignment below
	// B0194/T0111: Clear drop flag on elvis of an *owned* optional identifier.
	// The inner value is extracted and transferred to the result — the optional's
	// scope-exit drop should NOT also free it (double-free). Peel ParenExpr so
	// `(a) ?: b` clears `a`'s flag too (T0937), matching the orphan classifier
	// (which also peels parens).
	//
	// someOwnsInner records whether the result OWNS the moved-out inner on the
	// some-path. It owns it only when the inner is "orphaned" — an owned local
	// (whose scope drop flag is cleared here) or a temporary optional. A borrowed
	// value parameter (T0945; caller-owned) and a member/index source (T0937;
	// container-owned) leave the inner with an existing owner, so the result
	// borrows it (someOwnsInner=false) and the inline result temp is never freed.
	someOwnsInner := c.elvisSomeInnerOrphaned(e.Left)
	if boundSourceDupCloned {
		// The bound binding cloned the member/index field — the binding owns the fresh
		// copy on the some-path (the container keeps the original; no flag to clear).
		someOwnsInner = true
	}
	if someOwnsInner {
		if ident, ok := unwrapDestructureParens(e.Left).(*ast.IdentExpr); ok {
			c.clearDropFlag(ident.Name)
		}
	}
	// T1166: member/index owned target — a borrowed some-path inner (someOwnsInner
	// false: borrowed param or not-yet-cloned container source) must be deep-cloned so
	// the field/element owns an independent copy. The container's unconditional
	// field/element drop is then balanced; the caller/container keeps the original.
	// Marks the result owned so trackElvisResultHeap/Temp register an owned temp that
	// the member/index assign branch claims (identical to the working owned-local case).
	if forceOwnClone && !someOwnsInner {
		someVal = c.cloneResolvedValue(someVal, resolvedElvisType)
		someOwnsInner = true
	}
	c.block.NewBr(mergeBlock)
	someEnd := c.block

	// None path: evaluate default
	c.block = noneBlock
	// T0983: when a BOUND droppable elvis's default is itself an elvis
	// (`m := a ?: (b ?: c)`), propagate the bound obligation into the inner elvis so
	// IT neutralizes its own terminal default's owner (clear a local's drop flag /
	// claim a fresh temp) and produces its own per-path bound drop flag. The outer
	// then (a) claims the inner elvis's inline result temp and (b) inherits the inner's
	// per-path flag as its none-path ownership — instead of value-identity claiming the
	// inner phi, which neutralized nothing (the inner default kept its scope-exit owner
	// while the bound variable also took an owning drop → double free / SEGV, T0983).
	// Recurses naturally for deeper nesting (`a ?: (b ?: (c ?: d))`).
	nestedBoundDefault := false
	if boundResult || returnedResult || consumedByReceive {
		_, _, ownedRes := c.elvisResultDrop(e)
		droppableRes := ownedRes || c.elvisResultHandleDrop(e) != nil || c.elvisResultHeapDrop(e) != nil
		if droppableRes {
			if be, ok := unwrapDestructureParens(e.Right).(*ast.BinaryExpr); ok && be.Op == ast.BinElvis {
				if boundResult {
					c.elvisResultBound = true
					nestedBoundDefault = true
				} else if returnedResult {
					// T0982: nested-elvis default in a RETURN (`return a ?: (b ?: c)`).
					// Propagate the returned obligation into the inner elvis so IT
					// neutralizes its own terminal owned-local default's scope-exit drop
					// (the all-none path returns that default, which escapes to the caller;
					// without this both the inner default's binding and the caller free it →
					// SEGV/double-free). Unlike the bound case this threads NO per-path flag
					// up — the escaping result temp is claimed unconditionally in
					// genReturnStmt, so the inner's flag-clear is the whole fix. Recurses
					// naturally for deeper nesting (`a ?: (b ?: (c ?: d))`).
					c.elvisResultReturned = true
				} else {
					// T0955: nested-elvis default consumed by an enclosing `<-` await
					// (`<-(a ?: (c ?: b))`). Propagate the consume signal into the inner
					// elvis so IT neutralizes its own terminal owned-local/fresh-temp
					// default (the await joins+frees the selected G; without this the
					// inner default's binding frees it again → double-free/SEGV/hang).
					// Like the returned case, threads NO per-path flag up — the await is
					// the single owner. Recurses for deeper nesting
					// (`<-(a ?: (b ?: (c ?: d)))`).
					c.elvisResultConsumed = true
				}
			}
		}
	}
	defaultVal := c.genExprAutoPropagate(e.Right)
	// T0936: the none-path SELECTS the default. The result OWNS it (and must free it
	// exactly once) only when we can neutralize the default's own owner here:
	//   - local-ident default → clear its scope-exit drop flag (path-conditional:
	//     emitted in the none block only, so on the some-path the *unselected*
	//     default is still dropped normally by its own binding);
	//   - fresh temp default (literal/call) → claim its string/heap temp.
	// A parameter/borrowed/static default keeps its existing owner (caller binding,
	// or none for .rodata) so the result BORROWS it (noneOwned=false) — matching the
	// ownership pass's borrow model for those operands and avoiding a double-free.
	noneOwned := false
	// T0983: nested-elvis default — the inner elvis already neutralized its terminal
	// default and set c.elvisBoundDropFlag. Capture that per-path flag (the outer's
	// none-path ownership), reset it so the outer's own bound-flag phi below is not
	// confused, and claim the inner elvis's inline result temp so it is not also freed
	// at statement end. noneOwned=true records that the outer binding may own on the
	// none-path (the exact per-path condition is the inherited flag, threaded below).
	var nestedNoneFlag value.Value
	if nestedBoundDefault {
		nestedNoneFlag = c.elvisBoundDropFlag
		c.elvisBoundDropFlag = nil
		c.claimElvisDefaultTemp(defaultVal)
		if nestedNoneFlag != nil {
			noneOwned = true
		}
	}
	if nestedNoneFlag != nil {
		// Handled above — skip the flat neutralization paths below.
	} else if _, _, owned := c.elvisResultDrop(e); owned {
		// Vector[T]/T[] and string results (inline + bound), unchanged semantics.
		noneOwned = c.neutralizeElvisNoneDefault(e, defaultVal)
	} else if consumedByReceive {
		// T0954: result consumed by an enclosing `<-` await (Task[T] handle, not a
		// Vector/string result elvisResultDrop tracks). The await joins+frees the
		// selected G, so the none-path default must not be freed again by its own
		// owner — neutralize an owned-local / fresh-temp default. noneOwned stays
		// false (the await is the single owner); a borrowed param default has no
		// drop flag to clear, leaving T0953's borrowed-source crash to its own fix.
		c.neutralizeElvisNoneDefault(e, defaultVal)
	} else if (boundResult || returnedResult) && (c.elvisResultHandleDrop(e) != nil || c.elvisResultHeapDrop(e) != nil) {
		// T0952 (single-owner native handle) + T0940 (Map/Set/heap-user type) elvis
		// bound DIRECTLY to a variable (`m := a ?: b`). The binding takes a per-path
		// owning drop (elvisBoundDropFlag); on the none-path it aliases the default,
		// so an owned-local / fresh-temp default's own scope-exit drop must be
		// neutralized here (path-conditional — none-block only) or the buffer is
		// freed twice (Mutex/Map/heap-user: SEGV/invalid-free). A borrowed param /
		// member / static default keeps its real owner → noneOwned stays false and
		// the bound variable BORROWS it on the none-path. The inline (non-bound) case
		// keeps borrow-on-none (T0951/T0937), so this is gated to boundResult.
		noneOwned = c.neutralizeElvisNoneDefault(e, defaultVal)
	}
	// T1166: member/index owned target — a borrowed none-path default (noneOwned false:
	// borrowed param / member / static, whose owner was NOT neutralized above) must be
	// deep-cloned so the field/element owns its copy; the default's real owner keeps the
	// original. Symmetric with the some-path clone.
	if forceOwnClone && !noneOwned {
		defaultVal = c.cloneResolvedValue(defaultVal, resolvedElvisType)
		noneOwned = true
	}
	noneEnd := c.block
	c.block.NewBr(mergeBlock)

	// Merge
	c.block = mergeBlock
	result := mergeBlock.NewPhi(
		&ir.Incoming{X: someVal, Pred: someEnd},
		&ir.Incoming{X: defaultVal, Pred: noneEnd},
	)
	// T0933/T0940/T0981/T0952/T0936: a BOUND elvis (`m := a ?: b`) replaces the variable
	// binding's unconditional owning drop with this per-path flag — `m` owns the
	// buffer only on a path whose selected operand was orphaned (some-path inner) or
	// neutralized (none-path default). Computed for every droppable result
	// representation (Vector/string via elvisResultDrop, native handles via
	// elvisResultHandleDrop, Map/Set/heap-user via elvisResultHeapDrop). Created here
	// — after the result phi, before the trackElvis* phis — so all phis precede the
	// merge block's non-phi instructions. Consumed in the var-decl path in stmt.go.
	if boundResult {
		_, _, vecOrStr := c.elvisResultDrop(e)
		if vecOrStr || c.elvisResultHandleDrop(e) != nil || c.elvisResultHeapDrop(e) != nil {
			someF, noneF := int64(0), int64(0)
			if someOwnsInner {
				someF = 1
			}
			if noneOwned {
				noneF = 1
			}
			// T0983: a nested-elvis default contributes a per-path (runtime) flag, not a
			// constant — the outer binding owns the selected value on the none-path exactly
			// when the inner elvis's own bound flag says so.
			var noneIncoming value.Value = constant.NewInt(irtypes.I1, noneF)
			if nestedNoneFlag != nil {
				noneIncoming = nestedNoneFlag
			}
			c.elvisBoundDropFlag = mergeBlock.NewPhi(
				&ir.Incoming{X: constant.NewInt(irtypes.I1, someF), Pred: someEnd},
				&ir.Incoming{X: noneIncoming, Pred: noneEnd},
			)
		}
	}
	// T0935/T0945/T0936/T0937: register the inline result with a path-dependent drop
	// flag. The some-path owns the moved-out inner only when it was actually orphaned
	// (someOwnsInner — false for a borrowed value parameter (T0945) or a member/index
	// source (T0937)); the none-path owns only when the default's owner was
	// neutralized above (noneOwned, T0936). A bound use claims this temp (by value
	// identity) and drops via the variable's own binding instead — leaving these
	// path-flags inert. trackElvisResultTemp handles the i8* container representation
	// (string, Vector[T]/T[]); trackElvisResultHeap handles the 2-word value-struct
	// representation (Map/Set and droppable heap user types, T0937).
	c.trackElvisResultTemp(e, result, someEnd, noneEnd, mergeBlock, someOwnsInner, noneOwned)
	c.trackElvisResultHeap(e, result, someEnd, noneEnd, mergeBlock, someOwnsInner, noneOwned)
	return result
}

// elvisSomeInnerOrphaned reports whether the some-path inner extracted by `?:`
// from `left` is left without an owner (so the elvis result must own it). True for
// an owned droppable LOCAL ident (its scope drop flag is cleared on the some-path)
// and for a temporary optional (call/expr result — never scope-tracked). False for
// member/index sources (the container's drop frees the inner) and for borrowed-param
// idents (the caller owns it; no local drop flag — T0931/T0945).
func (c *Compiler) elvisSomeInnerOrphaned(left ast.Expr) bool {
	switch l := unwrapDestructureParens(left).(type) {
	case *ast.IdentExpr:
		_, hasFlag := c.dropFlags[l.Name]
		return hasFlag
	case *ast.MemberExpr, *ast.IndexExpr:
		return false
	default:
		return true // temporary optional
	}
}

// claimElvisDefaultTemp neutralizes a fresh string/heap temp selected as an elvis
// none-path default (literal/call result) and reports whether ownership was
// actually transferred to the elvis result. Returns false when there was no owned
// temp to claim (e.g. a .rodata literal or a borrowed member read), so the caller
// leaves the result borrowing — exactly one owner frees the buffer either way.
func (c *Compiler) claimElvisDefaultTemp(val value.Value) bool {
	if val == nil {
		return false
	}
	claimed := false
	if idx, ok := c.stmtTempMap[val]; ok && idx >= 0 {
		claimed = true
	}
	c.claimStringTemp(val)
	c.claimHeapTemp(val) // sets c.lastClaimedDropFunc when it claims a heap temp
	if c.lastClaimedDropFunc != nil {
		claimed = true
	}
	return claimed
}

// neutralizeElvisNoneDefault transfers a none-path default's ownership to the elvis
// result and reports whether it succeeded (T0936/T0940/T0981). An owned-local ident
// default → clear its scope-exit drop flag (path-conditional: emitted in the none
// block only, so the some-path's unselected default is still dropped by its own
// binding). A fresh string/heap temp default (literal/call) → claim it. A borrowed
// param / member / static default (no scope drop flag, no owned temp) → returns
// false: the result must BORROW on the none-path so the default's real owner stays
// the sole owner. Peels parens so `a ?: (b)` matches the owned-local `b` (symmetric
// with the some-path's e.Left peel). Single source of truth for none-path transfer.
func (c *Compiler) neutralizeElvisNoneDefault(e *ast.BinaryExpr, defaultVal value.Value) bool {
	if ident, ok := unwrapDestructureParens(e.Right).(*ast.IdentExpr); ok {
		if _, has := c.dropFlags[ident.Name]; has {
			c.clearDropFlag(ident.Name)
			return true
		}
		return false
	}
	return c.claimElvisDefaultTemp(defaultVal)
}

// elvisResultHeapDrop resolves the drop function for an elvis result represented as
// a 2-word {i8*, i8*} value struct (Map/Set) or a droppable heap user type — the
// representation trackElvisResultHeap handles (T0937). Returns nil for value/copy/
// primitive/structural results, for i8* containers / native handles / string (those
// go through elvisResultDrop / elvisResultHandleDrop / trackElvisResultTemp), and
// for ref types. Single source of truth for the heap-droppable classification shared
// by genElvis's bound-flag gating (T0940) and trackElvisResultHeap.
func (c *Compiler) elvisResultHeapDrop(e *ast.BinaryExpr) *ir.Func {
	rt := c.info.Types[e]
	if c.typeSubst != nil && rt != nil {
		rt = types.Substitute(rt, c.typeSubst)
	}
	if c.selfSubst != nil && rt != nil {
		rt = types.SubstituteSelf(rt, c.selfSubst.iface, c.selfSubst.concrete)
	}
	named := extractNamed(rt)
	if named == nil {
		return nil
	}
	if named.IsValueType() || named.IsCopy() || isPrimitiveScalar(named) || named.IsStructural() {
		return nil
	}
	if isContainerType(rt) || named == types.TypString {
		return nil // keeps Map/Set; excludes handles/string (i8* containers go through trackElvisResultTemp)
	}
	return c.resolveDropFuncForTemp(named, rt)
}

// trackElvisResultHeap registers an inline elvis result that is a value-struct
// container (Map/Set) or heap user type {i8*, i8*} as an owned heap drop temp with
// a per-branch flag (someOwned on the some path where the inner is orphaned,
// noneOwned on the none path). T0937 (subsumes T0924's heap-user case — same
// representation + mechanism). Type filtering is delegated to elvisResultHeapDrop, so
// single-owner handles (Arc/Mutex/Task/...), strings, vectors, value/copy/primitive/
// structural results are excluded; only Map/Set and droppable heap user types pass.
// noneOwned can now be true for a BOUND Map/Set/heap-user default whose owner was
// neutralized (T0940) — harmless here because a bound use claims this temp by runtime
// pointer identity (claimHeapTemp), neutralizing the flag, and the variable's own
// per-path binding flag governs the drop instead.
func (c *Compiler) trackElvisResultHeap(e *ast.BinaryExpr, result value.Value, someEnd, noneEnd, mergeBlock *ir.Block, someOwned, noneOwned bool) {
	if !someOwned && !noneOwned {
		return // borrows on both paths (owner-governed source + borrowed default)
	}
	if !c.tempTrackingEnabled || result == nil {
		return
	}
	if c.entryBlock == nil || c.block == nil || c.block.Term != nil {
		return
	}
	dropFunc := c.elvisResultHeapDrop(e)
	if dropFunc == nil {
		return
	}

	// Per-branch live flag: owned on the some path when the extracted inner is
	// orphaned, owned on the none path when the default's owner was neutralized
	// (T0940 — reached for a bound Map/Set/heap-user default; a bound use claims this
	// temp by pointer identity so the flag is then inert). Created in the merge block
	// immediately after the result phi (phis-first).
	someFlag := int64(0)
	if someOwned {
		someFlag = 1
	}
	noneFlag := int64(0)
	if noneOwned {
		noneFlag = 1
	}
	owned := mergeBlock.NewPhi(
		&ir.Incoming{X: constant.NewInt(irtypes.I1, someFlag), Pred: someEnd},
		&ir.Incoming{X: constant.NewInt(irtypes.I1, noneFlag), Pred: noneEnd},
	)
	instPtr := c.block.NewExtractValue(result, 1)
	c.trackHeapTempWithFlag(instPtr, dropFunc, owned)
}

// elvisResultDrop resolves the elvis result type and returns the matching temp
// drop function (Vector.drop / promise_string_drop), the vector element type (or
// nil), and whether the result is an owned Vector/string the elvis owns on both
// paths. Returns ok=false for ref types and all other representations (T0940).
// Single source of truth for the gating in genElvis and the dispatch in
// trackElvisResultTemp.
func (c *Compiler) elvisResultDrop(e *ast.BinaryExpr) (*ir.Func, types.Type, bool) {
	rt := c.info.Types[e]
	if c.typeSubst != nil && rt != nil {
		rt = types.Substitute(rt, c.typeSubst)
	}
	if c.selfSubst != nil && rt != nil {
		rt = types.SubstituteSelf(rt, c.selfSubst.iface, c.selfSubst.concrete)
	}
	if rt == nil || isRefType(rt) {
		return nil, nil, false
	}
	if elem, ok := types.AsVector(rt); ok {
		dropFn := c.funcs["Vector.drop"]
		return dropFn, elem, dropFn != nil
	}
	if extractNamed(rt) == types.TypString {
		dropFn := c.funcs["promise_string_drop"]
		return dropFn, nil, dropFn != nil
	}
	return nil, nil, false
}

// elvisResultHandleDrop resolves the per-instantiation drop function for an elvis
// result that is a single-owner native handle represented as a bare i8* — Ref[T],
// Channel[T], Weak[T], Mutex[T], MutexGuard[T], Task[T] (T0951). These bypass
// elvisResultDrop (which only resolves Vector/string) and trackElvisResultHeap
// (which requires a 2-word {i8*,i8*} value struct), so the orphaned some-path
// handle had no drop path → leak. Mirrors the handle dispatch in trackGetterResult.
// rt is substituted first, so the element types from types.As* are concrete.
// Returns nil for every non-handle / ref result type.
func (c *Compiler) elvisResultHandleDrop(e *ast.BinaryExpr) *ir.Func {
	rt := c.info.Types[e]
	if c.typeSubst != nil && rt != nil {
		rt = types.Substitute(rt, c.typeSubst)
	}
	if c.selfSubst != nil && rt != nil {
		rt = types.SubstituteSelf(rt, c.selfSubst.iface, c.selfSubst.concrete)
	}
	if rt == nil || isRefType(rt) {
		return nil
	}
	named := extractNamed(rt)
	if chElem, ok := types.AsChannel(rt); ok || named == types.TypChannel {
		return c.getOrCreateChannelDrop(chElem)
	}
	if arcElem, ok := types.AsArc(rt); ok {
		return c.getOrCreateArcDrop(arcElem)
	}
	if weakElem, ok := types.AsWeak(rt); ok {
		return c.getOrCreateWeakDrop(weakElem)
	}
	if mutexElem, ok := types.AsMutex(rt); ok {
		return c.getOrCreateMutexDrop(mutexElem)
	}
	if _, ok := types.AsMutexGuard(rt); ok || named == types.TypMutexGuard {
		return c.funcs["MutexGuard.drop"]
	}
	if taskElem, ok, taskFail := types.AsAnyTaskFailable(rt); ok {
		return c.getOrCreateTaskDrop(taskElem, taskFail)
	}
	return nil
}

// trackElvisResultTemp registers an inline (used/discarded) elvis `?:` result as
// a statement temp with a path-dependent drop flag (T0935/T0945/T0936/T0937). The
// flag is true on a path only when the result actually owns the buffer selected on
// that path — someOwned on the some-path (the optional's inner was orphaned, false
// for a borrowed value parameter (T0945) or a member/index source (T0937)) and
// noneOwned on the none-path (the default's owner was neutralized, T0936). When a
// path's operand keeps an existing owner (caller param, container field, or
// .rodata) the flag is false there and the result borrows — avoiding a double-free.
// The drop function matches the result type — Vector.drop for vectors (honors the
// bit-63 static flag and walks droppable elements), promise_string_drop for
// strings. Other i8* result types fall through untracked (T0940). A bound use of
// the result claims the temp by value identity, neutralizing the flag so only the
// variable's binding drops it. The value-struct representation (Map/Set, heap user
// types) is handled by trackElvisResultHeap instead.
func (c *Compiler) trackElvisResultTemp(e *ast.BinaryExpr, result value.Value, someEnd, noneEnd, mergeBlock *ir.Block, someOwned, noneOwned bool) {
	// When neither path transfers ownership to the result (e.g. a borrowed value
	// parameter or a member/index source on the some-path with a borrowed/static
	// default on the none-path), the result borrows on both paths and must not be
	// dropped (T0945/T0936/T0937).
	if !someOwned && !noneOwned {
		return
	}
	if !c.tempTrackingEnabled || result == nil || result.Type() != irtypes.I8Ptr {
		return
	}
	if c.entryBlock == nil || c.block == nil || c.block.Term != nil {
		return
	}
	if _, ok := c.stmtTempMap[result]; ok {
		return
	}

	dropFn, elemType, owned := c.elvisResultDrop(e)
	if !owned {
		// T0951: a single-owner native handle (Arc/Channel/Weak/Mutex/MutexGuard/
		// Task) is a bare i8*, not a Vector/string, so elvisResultDrop returns
		// owned=false. Resolve the handle's native drop here so the orphaned
		// some-path handle is freed exactly once. cleanupStmtTemps routes a Task
		// drop through the cooperative join automatically.
		if handleDrop := c.elvisResultHandleDrop(e); handleDrop != nil {
			dropFn, elemType = handleDrop, nil
		} else {
			return // other result types untracked (T0940)
		}
	}

	// Path-dependent drop flag: per-path ownership computed in genElvis. Created in
	// the merge block immediately after the result phi so all phis precede the stores.
	someFlag := int64(0)
	if someOwned {
		someFlag = 1
	}
	noneFlag := int64(0)
	if noneOwned {
		noneFlag = 1
	}
	flagPhi := mergeBlock.NewPhi(
		&ir.Incoming{X: constant.NewInt(irtypes.I1, someFlag), Pred: someEnd},
		&ir.Incoming{X: constant.NewInt(irtypes.I1, noneFlag), Pred: noneEnd},
	)
	c.appendStmtTemp(result, dropFn, elemType, flagPhi)
}

// --- Optional Chaining ---

// genOptionalChainExpr generates x?.field — checks if the optional is present,
// accesses the field on the inner value in the some-block, returns none in the none-block.
func (c *Compiler) genOptionalChainExpr(e *ast.OptionalChainExpr) value.Value {
	optVal := c.genExpr(e.Target)

	// Extract flag (field 0)
	flag := c.block.NewExtractValue(optVal, 0)

	someBlock := c.newBlock("optchain.some")
	noneBlock := c.newBlock("optchain.none")
	mergeBlock := c.newBlock("optchain.merge")

	c.block.NewCondBr(flag, someBlock, noneBlock)

	// Some: extract inner value, access field, wrap in Optional
	c.block = someBlock
	innerVal := c.block.NewExtractValue(optVal, 1)

	// Resolve the inner type from sema
	targetType := c.info.Types[e.Target]
	if c.typeSubst != nil {
		targetType = types.Substitute(targetType, c.typeSubst)
	}
	optType := targetType.(*types.Optional)
	innerType := optType.Elem()

	// Determine the result Optional type from sema
	resultType := c.info.Types[e]
	if c.typeSubst != nil {
		resultType = types.Substitute(resultType, c.typeSubst)
	}
	resultLLVM := c.resolveType(resultType).(*irtypes.StructType)

	// Access field/getter on inner value. Delegate to the full member-access
	// machinery (genMemberExpr) via a synthetic target node staged with innerVal,
	// so native getters (string.len/Vector.len), virtual dispatch, value-type
	// receivers, and enum getters are all handled correctly (T1421). Temp tracking
	// is disabled during the delegated call: the some-path member result is only
	// valid in this branch, so it must not be registered for unconditional
	// statement-end cleanup; ownership of the wrapped optional follows the same
	// borrow semantics as the pre-T1421 path.
	//
	// T1463: disabling tracking here leaves `?.` without a full ownership model for
	// the member result — a fresh droppable getter result that is *discarded* (not
	// bound) leaks, and a heap field read through `?.` and bound to a variable is
	// double-freed (pre-existing). A correct fix needs branch-local, presence-flag-
	// guarded cleanup for fresh results plus clone-on-escape for borrowed fields.
	synTarget := &ast.IdentExpr{}
	synMember := &ast.MemberExpr{Target: synTarget, Field: e.Field}
	if c.stagedExprValues == nil {
		c.stagedExprValues = make(map[ast.Expr]value.Value)
	}
	c.stagedExprValues[synTarget] = innerVal
	c.info.Types[synTarget] = innerType
	c.info.Types[synMember] = resultType.(*types.Optional).Elem()
	savedTracking := c.tempTrackingEnabled
	c.tempTrackingEnabled = false
	fieldVal := c.genMemberExpr(synMember)
	c.tempTrackingEnabled = savedTracking
	delete(c.stagedExprValues, synTarget)

	someResult := c.wrapOptional(fieldVal, resultLLVM)
	c.block.NewBr(mergeBlock)
	someEnd := c.block

	// None: zeroinit Optional
	c.block = noneBlock
	noneResult := constant.NewZeroInitializer(resultLLVM)
	c.block.NewBr(mergeBlock)
	noneEnd := c.block

	// Merge
	c.block = mergeBlock
	return mergeBlock.NewPhi(
		&ir.Incoming{X: someResult, Pred: someEnd},
		&ir.Incoming{X: noneResult, Pred: noneEnd},
	)
}

// dupHeapFieldForEscape clones a loaded heap-typed field value (string,
// Optional[string], Vector/Channel/Arc/Weak, or Optional[those]) when the active
// dup-on-escape flag (dupStringFieldAccess / dupContainerFieldAccess) is set and
// the owner is droppable, tracking the clone as a statement temp so scope cleanup
// claims it once the consumer takes ownership. Returns (clone, true) when a clone
// was produced; (val, false) for an in-scope borrow (no flag set, or owner not
// droppable), in which case the caller returns val unchanged. The single source of
// truth for field-escape duplication, shared by genFieldAccess (struct fields) and
// genNarrowedVariantField (narrowed enum variant fields, T1011) so a heap field
// behaves identically in every consumer context. fType must already be fully
// substituted by the caller.
func (c *Compiler) dupHeapFieldForEscape(val value.Value, fType types.Type, ownerDroppable bool) (value.Value, bool) {
	// String / Optional[string] — gated on dupStringFieldAccess.
	if c.dupStringFieldAccess && ownerDroppable {
		if extractNamed(fType) == types.TypString {
			c.dupStringFieldAccess = false // consume the flag
			dup := c.dupString(val)
			c.trackStringTemp(dup)
			return dup, true
		}
		// B0181: Handle string? optional fields — extractNamed returns nil for
		// *types.Optional, so unwrap first to check the inner type.
		// B0190: Track the dup as a temp AND store it in optionalStringDup so
		// genOptionalForceUnwrap can return it directly (bypassing extractvalue
		// which creates a different value.Value that claimStringTemp can't match).
		if opt, ok := fType.(*types.Optional); ok && extractNamed(opt.Elem()) == types.TypString {
			c.dupStringFieldAccess = false // consume the flag
			innerStr := c.block.NewExtractValue(val, 1)
			dup := c.dupString(innerStr)
			c.trackStringTemp(dup)
			c.optionalStringDup = dup
			return c.block.NewInsertValue(val, dup, 1), true
		}
	}

	// B0219: Dup vector/channel fields from types with drop.
	// Vector: shallow copy (allocate + memcpy). Channel: incref.
	if c.dupContainerFieldAccess && ownerDroppable {
		// T1263: a value-copying container FIELD transitively nesting a closure cannot be
		// deep-copied (dupVector's element-clone loop zeroes the closure env → SEGV). Leave
		// it ALIASED — the read is a borrow of the owner; the borrow gates suppress the
		// owning drop binding and reject escapes. Mirrors the genVectorIndex guard and
		// T1262's typeNeedsMatchDup(false). Non-closure fields (int[]) keep deep-copying.
		if sema.FirstFieldNestedClosureDeep(fType) != nil {
			c.dupContainerFieldAccess = false // consume the flag
			return val, false
		}
		// T1176/T1173: whole fixed-Array field/binding escaping the owner — the
		// [N x T_v] aggregate merely ALIASES N inner heap allocations (heap-user
		// instances, or string/Vector/Channel/Arc/Weak buffers), so element-wise
		// deep-clone via dupArrayValueForEscape. Without this the owner's synth
		// drop frees the elements at scope exit while the escaped copy still points
		// into them (UAF for string/heap-user, double-free for containers). No temp
		// tracking: every sink that sets the flag stores the value into an owned
		// slot (return → caller bindingDropArray; assign → target bindingDropArray
		// after drop-old; constructor → instance synth drop; move-param → callee
		// param drop), and the subject's synth drop frees the originals exactly once.
		if elemT, n, ok := c.arrayElemNeedsEscapeDup(fType); ok {
			c.dupContainerFieldAccess = false // consume the flag
			return c.dupArrayValueForEscape(val, elemT, n)
		}
		if elemType, ok := types.AsVector(fType); ok {
			c.dupContainerFieldAccess = false // consume the flag
			elemLLVM := c.resolveType(elemType)
			elemSize := int64(c.typeSize(elemLLVM))
			dup := c.dupVector(val, elemSize)
			// T0540: Deep-clone droppable elements so the dup owns independent
			// copies. Without this, the shallow memcpy aliases element pointers
			// between the original field and the dup, causing a double-free at
			// scope end. No-op for primitive/copy element types.
			c.emitVectorElementCloneLoop(dup, elemType)
			// The dup now owns its elements independently; statement-end cleanup
			// must drop the elements before freeing the buffer.
			c.trackVectorTempWithElemType(dup, elemType)
			return dup, true
		}
		if chElem, ok := types.AsChannel(fType); ok {
			c.dupContainerFieldAccess = false // consume the flag
			dup := c.dupChannel(val)
			c.trackChannelTempWithElemType(dup, chElem) // T0663
			return dup, true
		}
		if arcElem, ok := types.AsArc(fType); ok {
			c.dupContainerFieldAccess = false // consume the flag
			resolvedArcElem := arcElem
			if c.typeSubst != nil {
				resolvedArcElem = types.Substitute(arcElem, c.typeSubst)
			}
			dup := c.dupArc(val, resolvedArcElem)
			c.trackTempWithDrop(dup, c.getOrCreateArcDrop(resolvedArcElem))
			return dup, true
		}
		if weakElem, ok := types.AsWeak(fType); ok {
			c.dupContainerFieldAccess = false // consume the flag
			resolvedWeakElem := weakElem
			if c.typeSubst != nil {
				resolvedWeakElem = types.Substitute(weakElem, c.typeSubst)
			}
			dup := c.dupWeak(val, resolvedWeakElem)
			c.trackTempWithDrop(dup, c.getOrCreateWeakDrop(resolvedWeakElem))
			return dup, true
		}
		// T0366: Optional[Vector|Channel|Arc|Weak] fields — dup the inner buffer
		// so the new optional owns an independent copy. Without this, both the
		// source's owner drop and the new variable's optional drop free the same
		// buffer (mirrors the Optional[String] handling above). optionalContainerDup
		// is consumed by genVarDecl (and similar sites) to claim the dup temp once
		// the containing optional is bound to a variable.
		if opt, ok := fType.(*types.Optional); ok {
			elem := opt.Elem()
			if elemType, isVec := types.AsVector(elem); isVec {
				c.dupContainerFieldAccess = false
				elemLLVM := c.resolveType(elemType)
				elemSize := int64(c.typeSize(elemLLVM))
				innerVec := c.block.NewExtractValue(val, 1)
				dup := c.dupVector(innerVec, elemSize)
				// T0540: Deep-clone droppable elements (mirror the bare Vector branch above).
				// T0939: dup is null on the optional's `none` path — guard the clone loop.
				c.emitVectorElementCloneLoopNullable(dup, elemType)
				c.trackVectorTempWithElemType(dup, elemType)
				c.optionalContainerDup = dup
				return c.block.NewInsertValue(val, dup, 1), true
			}
			if chElem, isCh := types.AsChannel(elem); isCh {
				c.dupContainerFieldAccess = false
				innerCh := c.block.NewExtractValue(val, 1)
				dup := c.dupChannel(innerCh)
				c.trackChannelTempWithElemType(dup, chElem) // T0663
				c.optionalContainerDup = dup
				return c.block.NewInsertValue(val, dup, 1), true
			}
			if arcElem, isArc := types.AsArc(elem); isArc {
				c.dupContainerFieldAccess = false
				innerArc := c.block.NewExtractValue(val, 1)
				resolvedArcElem := arcElem
				if c.typeSubst != nil {
					resolvedArcElem = types.Substitute(arcElem, c.typeSubst)
				}
				dup := c.dupArc(innerArc, resolvedArcElem)
				c.trackTempWithDrop(dup, c.getOrCreateArcDrop(resolvedArcElem))
				c.optionalContainerDup = dup
				return c.block.NewInsertValue(val, dup, 1), true
			}
			if weakElem, isWeak := types.AsWeak(elem); isWeak {
				c.dupContainerFieldAccess = false
				innerWeak := c.block.NewExtractValue(val, 1)
				resolvedWeakElem := weakElem
				if c.typeSubst != nil {
					resolvedWeakElem = types.Substitute(weakElem, c.typeSubst)
				}
				dup := c.dupWeak(innerWeak, resolvedWeakElem)
				c.trackTempWithDrop(dup, c.getOrCreateWeakDrop(resolvedWeakElem))
				c.optionalContainerDup = dup
				return c.block.NewInsertValue(val, dup, 1), true
			}
		}
	}

	// T1299: a non-value structural-interface field (bare or Optional) escaping a
	// droppable owner (`get val V? { return this._v; }`, `[](int i) V?`). The
	// {vtable, instance} view aliases the owner's box; the owner's synth drop (T1284)
	// and the escape sink's drop would free the same box (double-free / segfault).
	// Deep-clone via __promise_structural_clone so the sink owns an independent box,
	// tracked as a heap temp dropped through structuralDrop (RTTI: value-type boxes
	// have a null drop_fn → pal_free; drop-bearing subtypes run their real drop).
	// claimHeapTemp (B0233) peels the {i1, {vtable, instance}} optional and matches
	// the inner instance pointer at runtime, so the escape site claims the clone;
	// unclaimed inline temps drop exactly once. Gated on dupHeapUserFieldAccess (a
	// structural view is a boxed heap instance, not a container) — the same flag
	// genVectorIndex uses for structural element reads, so the two escape sources
	// (field/getter vs vector-index) stay in lockstep without colliding.
	if c.dupHeapUserFieldAccess && ownerDroppable {
		if isNonValueStructuralType(fType) {
			c.dupHeapUserFieldAccess = false // consume the flag
			dup := c.cloneStructuralView(val)
			c.trackHeapTemp(c.extractInstancePtr(dup), c.structuralDrop)
			return dup, true
		}
		if opt, ok := fType.(*types.Optional); ok && isNonValueStructuralType(opt.Elem()) {
			c.dupHeapUserFieldAccess = false // consume the flag
			inner := c.block.NewExtractValue(val, 1)
			dup := c.cloneStructuralView(inner)
			c.trackHeapTemp(c.extractInstancePtr(dup), c.structuralDrop)
			return c.block.NewInsertValue(val, dup, 1), true
		}
	}

	return val, false
}

// dupBorrowedHeapUserPayload deep-clones the inner heap payload of a
// match-borrowed `Optional[heap-user-type]` (T1174) or fixed
// `Array[heap-user-type]` (T1171) ident so a value ESCAPING the `if
// is`/`match` narrowing scope owns it independently. Such a binding
// (T0485/T1012, see matchBindingIsBorrow) merely ALIASES the subject's variant
// payload — the subject's synth enum drop frees the original at scope exit, so
// an escaped alias is a use-after-free (segfault). This is the plain-ident
// analogue of the field-access dup in dupHeapFieldForEscape and the
// container-index dup in genMethodIndex/genVectorIndex (the `optionalHeapDup`
// path): extract the inner value struct, deep-clone it, and re-insert it into
// the Optional.
//
// Returns (dupedVal, true) when a dup was performed — the escape destination
// (return value / assignment target / consuming `~` param / constructor field)
// then owns the fresh inner via its normal drop machinery, and the subject's
// synth enum drop still frees the original exactly once. Returns (val, false)
// for any other expr/type so callers can use it as a transparent pass-through.
//
// Deliberately NOT routed through a read-side flag (dupHeapUserFieldAccess): that
// flag is also set by genIfUnwrapStmt for the IN-SCOPE `if r := maybe` unwrap,
// which must stay zero-copy (the T0512 nested-Optional invariant). Gating on an
// explicit escape-site call keeps in-scope borrows alias-only.
//
// dupHeapValue is null-safe (handles the optional's `none` path via a phi) and
// dispatches through the type's typeinfo clone_fn for polymorphic subtypes
// (T0387); it also deep-clones droppable sub-fields (e.g. Row.name), so no
// shallow alias leaks.
func (c *Compiler) dupBorrowedHeapUserPayload(expr ast.Expr, val value.Value) (value.Value, bool) {
	if val == nil || c.block == nil || c.block.Term != nil {
		return val, false
	}
	ident, ok := unwrapDestructureParens(expr).(*ast.IdentExpr)
	if !ok {
		return val, false
	}
	t := c.info.Types[ident]
	if c.typeSubst != nil && t != nil {
		t = types.Substitute(t, c.typeSubst)
	}
	isMatchBorrowed := c.matchBorrowedIdents != nil && c.matchBorrowedIdents[ident.Name]
	// T1184: a borrowed (default/`&`, non-`~`) fixed-array VALUE param returned or
	// otherwise escaped by value hands back an array whose [N x T_v] elements ALIAS
	// the caller's heap allocations (the caller keeps ownership of the borrow), so
	// both the escaped copy and the caller would free the same elements → double-free
	// / UAF. This is the array analog of the scalar return-implicitly-dups contract
	// (a borrowed `string`/`Vector` param returned as owned is deep-cloned today);
	// element-wise dup below makes the escaped array own its elements independently.
	// Gated on the array shape so only arrays that actually alias heap dup — plain
	// value/copy-element arrays are untouched. The Optional branch stays
	// match-borrowed-only (a distinct, separately-tracked shape; cf. T1183).
	_, isBorrowedArrayParam := c.borrowedArrayParamEscapeDup(ident.Name, t)
	if !isMatchBorrowed && !isBorrowedArrayParam {
		return val, false
	}
	if isMatchBorrowed {
		if inner, ok := c.optionalHeapDupElem(t); ok {
			// val must be the Optional struct { i1 present, T_v value }.
			if _, isStruct := val.Type().(*irtypes.StructType); !isStruct {
				return val, false
			}
			innerVal := c.block.NewExtractValue(val, 1)
			dup := c.dupHeapValue(innerVal, inner)
			return c.block.NewInsertValue(val, dup, 1), true
		}
	}
	// Fixed-Array whose elements alias heap (T1171 heap-user; T1173 string /
	// Vector / Channel / Arc / Weak / Optional[heap-user]): element-wise deep-clone
	// the [N x T_v] aggregate so the escaped array owns independent elements; the
	// subject's synth enum drop (match-borrowed) or the caller's bindingDropArray
	// (borrowed array param, T1184) still frees the originals exactly once.
	if elemT, n, ok := c.arrayElemNeedsEscapeDup(t); ok {
		return c.dupArrayValueForEscape(val, elemT, n)
	}
	return val, false
}

// dupBorrowedEnumParam deep-clones the variant payload of a borrowed (non-`~`)
// enum VALUE parameter returned by value (`consume(E p) E { return p; }`). This
// extends the "a borrowed heap param returned by value is deep-cloned" contract
// — already honored for strings (dupString) and containers/arrays
// (dupBorrowedHeapUserPayload) — to enums (T1323). A by-value enum param is a
// BORROW: the caller retains ownership of the arg temp, so returning the param
// unchanged hands the caller's payload back as the result, and both the caller's
// arg temp and the result then free the same heap payload (double-free / SEGV).
//
// The caller-side alias machinery (emitReturnAliasCheckSubst → extractAliasPtr)
// can't cover this: an enum value is `{i32 tag, [N x i8] data}` with the payload
// buried in tag-specific inline data — no top-level pointer to compare — so
// extractAliasPtr returns nil for enums and the clone never fires. Cloning here
// on the return makes the call result unconditionally independent of the arg
// temp, which is what lets the statement-boundary enum-ctor-temp drain (T1323
// Part B) drop the orphaned by-value arg temp safely at the binding site.
//
// Deliberately narrow: enum-only (strings/vectors/arrays are handled by the other
// return dups; cloning them here too would double-clone → leak), only a direct
// `return <param>` of a borrowed param, and only when the return type is that
// (droppable) enum and not a borrow (`E&`/`E~` hand back a reference). `~`/move
// params are a genuine transfer and are already excluded from borrowedValueParams.
// Returns (dup, true) when a clone was produced, else (val, false) as a
// transparent pass-through.
func (c *Compiler) dupBorrowedEnumParam(expr ast.Expr, val value.Value, retType types.Type) (value.Value, bool) {
	if val == nil || c.block == nil || c.block.Term != nil || retType == nil {
		return val, false
	}
	if isRefType(retType) {
		return val, false
	}
	ident, ok := unwrapDestructureParens(expr).(*ast.IdentExpr)
	if !ok {
		return val, false
	}
	if c.borrowedValueParams == nil || !c.borrowedValueParams[ident.Name] {
		return val, false
	}
	// An operator method's value operand is ALSO a borrowed value param, but its
	// return-alias clone is owned by wrapOperatorParamReturnValue (T0897), which
	// runs right after this and routes the enum through the same
	// cloneOwnedReturnAlias path. Cloning here too would double-clone → the first
	// clone leaks. Defer to the operator path for operands.
	if c.currentOpValueParams != nil && c.currentOpValueParams[ident.Name] {
		return val, false
	}
	effType := retType
	if opt, isOpt := retType.(*types.Optional); isOpt {
		effType = opt.Elem()
	}
	if c.typeSubst != nil {
		effType = types.Substitute(effType, c.typeSubst)
	}
	enumT := extractEnum(effType)
	if enumT == nil || !c.enumInstanceHasDrop(effType, enumT) {
		return val, false
	}
	// Confirm the returned param's own resolved type is that enum (not an unrelated
	// type coerced into an enum return) so the clone applies to a genuine enum value.
	pt := c.info.Types[ident]
	if c.typeSubst != nil && pt != nil {
		pt = types.Substitute(pt, c.typeSubst)
	}
	if extractEnum(pt) == nil {
		return val, false
	}
	return c.cloneOwnedReturnAlias(val, effType), true
}

// optionalHeapDupElem reports whether typ is an Optional whose element is a heap
// user type (droppable, or no-drop-but-pal-free) — the shape whose value struct
// merely ALIASES an inner heap instance, so it must be deep-cloned whenever a
// copy escapes the original owner (a match-borrowed variant payload, or a
// container element slot). Returns the resolved inner element type and true when
// so, else (nil, false). Single recognition point shared by
// dupBorrowedHeapUserPayload (escape-site dup) and maybeDupPushElement /
// pushElemNeedsDup (vector-push + slice dup) so the two stay in sync (T1174).
func (c *Compiler) optionalHeapDupElem(typ types.Type) (types.Type, bool) {
	opt, ok := typ.(*types.Optional)
	if !ok {
		return nil, false
	}
	inner := opt.Elem()
	if c.typeSubst != nil {
		inner = types.Substitute(inner, c.typeSubst)
	}
	if isDroppableHeapUserType(inner) || isHeapUserNoDropPalFree(inner) {
		return inner, true
	}
	return nil, false
}

// optionalPushElemNeedsDup reports whether typ is an Optional whose inner value
// aliases heap and must be deep-cloned when the Optional is pushed into a
// container OR when a whole-array copy of an Optional[...][N] escapes its owner.
// PUSH/ESCAPE-path recognizer: broader than optionalHeapDupElem (kept narrow for
// the index-read/field-escape sinks that hardcode dupHeapUserFieldAccess). Also
// matches string / Vector / Channel / Arc / Weak / droppable-tuple /
// droppable-enum / nested-droppable-Optional inners — every shape
// dupOptionalVectorElem can clone.
// Recursion terminates: each level unwraps exactly one Optional. T1183.
func (c *Compiler) optionalPushElemNeedsDup(typ types.Type) (*types.Optional, types.Type, bool) {
	opt, ok := typ.(*types.Optional)
	if !ok {
		return nil, nil, false
	}
	inner := opt.Elem()
	if c.typeSubst != nil {
		inner = types.Substitute(inner, c.typeSubst)
	}
	// T1291: a non-value structural interface inner boxes a heap instance that
	// must be deep-cloned via __promise_structural_clone (dupOptionalVectorElem's
	// structural case) — pushElemNeedsDup excludes structural, so recognize it here.
	if (extractNamed(inner) == types.TypString && !isRefType(inner)) || c.pushElemNeedsDup(inner) || isNonValueStructuralType(inner) {
		return opt, inner, true
	}
	return nil, nil, false
}

// borrowedArrayParamEscapeDup reports whether name is a borrowed (default/`&`,
// non-`~`) value parameter of the current function whose type is a fixed array
// whose elements alias heap — the T1184 shape. Returning/escaping such a param by
// value hands back an aggregate whose element pointers alias the caller's heap
// allocations; the caller keeps ownership of the borrow, so the escaped copy must
// element-wise deep-clone them (see dupBorrowedHeapUserPayload). Reuses
// borrowedValueParams (the single-source borrowed-param set) and
// arrayElemNeedsEscapeDup (the single-source per-array escape predicate). Returns
// the resolved element type and true when so, else (nil, false).
func (c *Compiler) borrowedArrayParamEscapeDup(name string, typ types.Type) (types.Type, bool) {
	if c.borrowedValueParams == nil || !c.borrowedValueParams[name] {
		return nil, false
	}
	if elemT, _, ok := c.arrayElemNeedsEscapeDup(typ); ok {
		return elemT, true
	}
	return nil, false
}

// arrayElemNeedsEscapeDup reports whether typ is a fixed Array whose element's
// value struct merely ALIASES heap — a heap-user type (droppable, or
// no-drop-but-pal-free), string, Vector, Channel, Arc, Weak, a droppable
// enum/tuple, or an Optional whose inner aliases heap — including
// Optional[heap-user] AND Optional[string]/Optional[container] (via
// pushElemNeedsDup's optionalPushElemNeedsDup recognizer, T1183). For such an array the [N x T_v]
// aggregate aliases N inner heap allocations, so a whole-array VALUE copy
// escaping the owner (a struct field read like `return w.rows`, or a
// match-borrowed variant payload) must be element-wise deep-cloned; the owner's
// synth drop otherwise frees the elements at scope exit while the escaped copy
// still points into them (UAF for string/heap-user, double-free for containers).
// Returns the resolved element type, the array size, and true when so, else
// (nil, 0, false). Element recognition reuses pushElemNeedsDup (the single-source
// per-element deep-clone predicate, shared with vector-push) plus the bare-string
// case that push callers handle separately (cf. dupTupleValue). Sibling of
// optionalHeapDupElem; single recognition point shared by dupHeapFieldForEscape
// (field-access + variant-payload escape sinks), dupBorrowedHeapUserPayload
// (match-borrowed-ident sink), setDupFlagsForFieldAccess, and the genIdentExpr
// read-side gate, so the shapes stay in sync (T1176/T1173).
func (c *Compiler) arrayElemNeedsEscapeDup(typ types.Type) (types.Type, int64, bool) {
	arr, ok := typ.(*types.Array)
	if !ok {
		return nil, 0, false
	}
	elem := arr.Elem()
	if c.typeSubst != nil {
		elem = types.Substitute(elem, c.typeSubst)
	}
	if (extractNamed(elem) == types.TypString && !isRefType(elem)) || c.pushElemNeedsDup(elem) {
		return elem, arr.Size(), true
	}
	return nil, 0, false
}

// dupArrayElemForEscape deep-clones one loaded array element whose value struct
// aliases heap (string / Vector / Channel / Arc / Weak / heap-user / droppable
// enum / tuple / Optional[heap-user]) so a whole-array VALUE escaping its owner owns
// the element independently. Reuses the single-source per-element dispatchers
// maybeDupPushElement (every heap element shape, shared with vector-push) and
// dupString (bare string, which push callers handle separately, cf.
// dupTupleValue). Returns elem unchanged for primitive/value/copy elements.
// elemType must already be fully substituted by the caller. NO temp tracking —
// see dupArrayValueForEscape. T1173.
func (c *Compiler) dupArrayElemForEscape(elem value.Value, elemType types.Type) value.Value {
	if extractNamed(elemType) == types.TypString && !isRefType(elemType) {
		return c.dupString(elem)
	}
	if dup := c.maybeDupPushElement(elem, elemType); dup != nil {
		return dup
	}
	return elem
}

// dupArrayValueForEscape element-wise deep-clones a loaded fixed-array VALUE (the
// [N x T_elem] aggregate) whose elements alias heap, rebuilding the aggregate with
// the clones so a whole-array escape (return / store-to-outer / consuming `~`
// param / constructor field) owns its elements independently. The subject's synth
// drop frees the originals exactly once, so there is NO double-free and NO leak —
// hence no temp tracking: the clones flow into an owned sink (caller/target
// bindingDropArray, instance synth drop, or callee param drop). Returns
// (rebuilt, true) on success; (val, false) when val is not an array aggregate.
// Shared by dupHeapFieldForEscape (field-access + enum-target sinks) and
// dupBorrowedHeapUserPayload (match-borrowed-ident sink). T1176/T1173.
func (c *Compiler) dupArrayValueForEscape(val value.Value, elemType types.Type, n int64) (value.Value, bool) {
	if _, isArr := val.Type().(*irtypes.ArrayType); !isArr {
		return val, false
	}
	out := val
	for i := int64(0); i < n; i++ {
		elem := c.block.NewExtractValue(out, uint64(i))
		dup := c.dupArrayElemForEscape(elem, elemType)
		out = c.block.NewInsertValue(out, dup, uint64(i))
	}
	return out, true
}

// genOptionalHandlerExpr generates code for `optExpr ? { recovery }`.
// Checks the optional flag, runs the handler on none, extracts inner value on some.
func (c *Compiler) genOptionalHandlerExpr(e *ast.ErrorHandlerExpr) value.Value {
	optVal := c.genExpr(e.Expr)
	// T0778: Capture any per-field dup created while evaluating the source (a
	// droppable-owner optional field like `(this.o)? _ {...}` — genFieldAccess
	// makes an INDEPENDENT dup of the inner string/vector and tracks it as a
	// statement temp). Capture it NOW, before the handler body's genBlockValue
	// runs cleanupStmtTemps and nils these fields. The dup is claimed in
	// someBlock below — see the comment there.
	srcStringDup := c.optionalStringDup
	srcContainerDup := c.optionalContainerDup
	// Clear so the handler body's genBlockValue (evaluated next, in noneBlock)
	// does not wrongly claim the SOURCE's dup in the none path — that dup is the
	// present-path value and must be claimed in someBlock instead. genBlockValue
	// is free to set/claim its own dup for a field-access recovery value.
	c.optionalStringDup = nil
	c.optionalContainerDup = nil
	flag := c.block.NewExtractValue(optVal, 0)

	noneBlock := c.newBlock("opt.none")
	someBlock := c.newBlock("opt.some")
	mergeBlock := c.newBlock("opt.merge")
	c.block.NewCondBr(flag, someBlock, noneBlock)

	// None path: run handler body
	c.block = noneBlock
	handlerVal := c.genBlockValue(e.Body)
	handlerDiverged := c.block.Term != nil
	handlerEnd := c.block
	if !handlerDiverged {
		c.block.NewBr(mergeBlock)
	}

	// Some path: extract inner value
	c.block = someBlock
	var okVal value.Value = c.block.NewExtractValue(optVal, 1)

	// T0778: Droppable-owner field source — `(owner.field)? _ { ... }` where the
	// optional lives in a field of a droppable owner (e.g. borrowed `this.o`).
	// genFieldAccess already made an INDEPENDENT dup of the inner string/vector
	// (srcStringDup/srcContainerDup captured above) — because the owner's own
	// drop still frees the original — and tracked that dup as a statement temp;
	// the present-path okVal IS that dup. Claim the dup's temp slot here in
	// someBlock so the PRESENT runtime keeps it (its drop flag is cleared in this
	// block, which only executes when present; the absent runtime's dup is null,
	// so its statement-end cleanup drop is a null no-op). The merged phi is then
	// tracked EXACTLY ONCE by genExpr's *ast.ErrorHandlerExpr branch (non-ident
	// source ⇒ it trackStringTemp's the phi) and is the sole owner. Without this,
	// the dup is freed at statement end AND aliased by the returned phi →
	// double-free (`fatal: invalid free`). Mirrors genOptionalForceUnwrap's
	// optionalStringDup consumption; ident sources never reach here with a dup
	// (genFieldAccess is not involved → the captured fields are nil).
	if srcStringDup != nil {
		c.claimStringTemp(srcStringDup)
	}
	if srcContainerDup != nil {
		c.claimStringTemp(srcContainerDup)
	}

	// T0775: Member-source optional handler (`owner.field? _ { ... }`) where the
	// owner has a drop that governs the field's inner allocation. The extracted
	// okVal ALIASES the owned field's inner; without a dup it would be freed BOTH
	// by the result's owner (the statement temp at statement end, or the bound LHS
	// at scope end) AND by the owner's drop → double-free (heap-user) or
	// use-after-free crash (vector). Dup the present inner so the result owns an
	// INDEPENDENT copy and the field's owner keeps & frees the original. This is
	// uniform across temporary and binding contexts: neutralizeForceUnwrapSource
	// deliberately does NOT neutralize the field for member-source handlers (see
	// the ErrorHandlerExpr case there), so the owner always frees the original and
	// the dup is the result's sole owner — which also keeps the field reusable
	// (`(h.f? _ {}).x` twice reads the live field both times). Gated to no per-field
	// dup already produced by genFieldAccess (srcStringDup/srcContainerDup — the
	// T0778 binding/borrowed-this path, which makes its own independent copy). The
	// dup is NOT tracked here; genExpr's ErrorHandlerExpr branch tracks the merged
	// phi (non-diverging) or the diverging okVal exactly once as the sole owner.
	//
	// LIMITED to the three types whose handler result is actually taken into
	// independent ownership downstream: string + vector (tracked by genExpr's
	// ErrorHandlerExpr i8* branch via trackStringTemp / trackVectorTemp) and
	// heap-user (tracked by trackHeapUserTypeResult). For refcounted/opaque
	// containers (Arc/Weak/Channel/Mutex/...) the handler result is NOT tracked as
	// an owned temp (the i8* branch only tracks string/vector; trackHeapUserTypeResult
	// early-returns on containers), so the aliasing okVal is already safe — the
	// owner's drop is the sole free and the temp merely reads it. Dup'ing those
	// here would incref/copy with no matching free → leak (the binding context for
	// those is instead covered by genFieldAccess's T0366/T0498 dup, gated above via
	// srcContainerDup).
	if srcStringDup == nil && srcContainerDup == nil &&
		c.isOwnerGovernedMemberOptionalUnwrapSource(e.Expr) {
		rt := c.info.Types[e]
		if c.typeSubst != nil && rt != nil {
			rt = types.Substitute(rt, c.typeSubst)
		}
		if c.selfSubst != nil && rt != nil {
			rt = types.SubstituteSelf(rt, c.selfSubst.iface, c.selfSubst.concrete)
		}
		named := extractNamed(rt)
		switch {
		case named == types.TypString:
			okVal = c.dupString(okVal)
		case types.IsVector(rt):
			if elemType, ok := types.AsVector(rt); ok {
				elemLLVM := c.resolveType(elemType)
				elemSize := int64(c.typeSize(elemLLVM))
				dup := c.dupVector(okVal, elemSize)
				// T0540: deep-clone droppable elements so the dup owns them.
				c.emitVectorElementCloneLoop(dup, elemType)
				okVal = dup
			}
		case named != nil && !named.IsValueType() && !named.IsCopy() &&
			!isPrimitiveScalar(named) && !named.IsStructural() && !isOpaqueContainerType(rt):
			// Heap user type — okVal is the `{vtable, instance}` value struct;
			// dupHeapValue returns an independent value struct. isOpaqueContainerType
			// already excludes Vector/Channel/Arc/Weak/Mutex/MutexGuard/Task.
			okVal = c.dupHeapValue(okVal, rt)
		}
	}

	// T0778: Non-diverging handler on an ident-source optional whose inner is
	// i8* (string/vector). The phi values are:
	//   - present runtime: okVal (aliases the source optional's owned inner)
	//   - absent  runtime: handler recovery (claimed by genBlockValue → its
	//     drop flag is cleared in noneBlock)
	// With the T0753 ident-skip leaving the phi untracked (see genExpr's
	// *ast.ErrorHandlerExpr branch), the absent-runtime recovery leaks. Fix:
	// neutralize the source ident's present flag here in someBlock (so the
	// optional's scope drop is a no-op in the present runtime), then track
	// the merged phi as an owned statement temp at mergeBlock. The i8*
	// restriction keeps this off the borrow-holding optional path: borrow-
	// holding requires a user-type inner via RTTI cast (isRttiCastBorrow only
	// matches *types.Named user types), so borrow-holding + string/vector is
	// impossible. Diverging handlers stay covered by the existing T0753 skip:
	// the phi degenerates to okVal aliasing the source's inner and stays
	// untracked, with the optional's drop binding governing the lifetime.
	var trackPhiI8Type types.Type
	var trackPhiHeapType types.Type
	if !handlerDiverged && isIdentOptionalUnwrapSource(e.Expr) {
		rt := c.info.Types[e]
		if c.typeSubst != nil && rt != nil {
			rt = types.Substitute(rt, c.typeSubst)
		}
		if c.selfSubst != nil && rt != nil {
			rt = types.SubstituteSelf(rt, c.selfSubst.iface, c.selfSubst.concrete)
		}
		named := extractNamed(rt)
		// T1085: Extend the T0778 recovery-leak fix beyond string/vector to the
		// opaque i8*-backed containers (Channel/Arc/Weak/Mutex/Task/MutexGuard).
		// isOpaqueContainerType already covers Vector and all the i8* handles.
		// Like string/vector, the absent-runtime recovery is otherwise an
		// untracked phi and leaks. neutralizeForceUnwrapSource is type-agnostic
		// (clears the source ident's present flag) so the source's scope drop is
		// a no-op on the present runtime; the merged phi becomes the sole owner,
		// tracked at mergeBlock below via the type-aware tracker.
		//
		if named == types.TypString || isOpaqueContainerType(rt) {
			trackPhiI8Type = rt
			c.neutralizeForceUnwrapSource(e)
		} else if named != nil && !named.IsValueType() && !named.IsCopy() &&
			!isPrimitiveScalar(named) && !named.IsStructural() &&
			!c.isBorrowHoldingOptionalIdentSource(e.Expr) {
			// T1085: Heap user-type inner (e.g. Map/Set) from an OWNED optional
			// ident source. The recovery may be a block returning a moved-out local
			// (e.g. `o? { mk := map(); mk }`), which genBlockValue claims (its drop
			// flag cleared in noneBlock) — so without phi-tracking the absent-runtime
			// recovery leaks. Neutralizing the OWNED source transfers ownership of the
			// present-arm inner to the merged phi; the phi is then the sole owner and
			// trackHeapValueTemp drops it once at statement end.
			//
			// EXCLUDES borrow-holding optional sources (`CSquare? o = this as CSquare`):
			// there the present arm aliases an external owner's instance and must NOT
			// be dropped. Those keep trackHeapUserTypeResult's T0753 ident-skip (the
			// present arm is governed by the external owner; a bare-constructor
			// recovery is tracked at its own construction site).
			trackPhiHeapType = rt
			c.neutralizeForceUnwrapSource(e)
		}
	}

	c.block.NewBr(mergeBlock)
	someEnd := c.block

	c.block = mergeBlock

	// If handler diverges, no phi needed - only the some path reaches merge
	if handlerDiverged {
		return okVal
	}

	// Both paths reach merge - phi merge the values
	if handlerVal != nil && okVal != nil {
		phi := c.block.NewPhi(
			&ir.Incoming{X: okVal, Pred: someEnd},
			&ir.Incoming{X: handlerVal, Pred: handlerEnd},
		)
		if trackPhiI8Type != nil {
			named := extractNamed(trackPhiI8Type)
			if named == types.TypString {
				c.trackStringTemp(phi)
			} else if named == types.TypVector {
				if elemType, ok := types.AsVector(trackPhiI8Type); ok {
					c.trackVectorTempWithElemType(phi, elemType)
				} else {
					c.trackVectorTemp(phi)
				}
			} else if arcElem, isArc := types.AsArc(trackPhiI8Type); isArc {
				// T1085: opaque container recovery now owned by the merged phi.
				c.trackTempWithDrop(phi, c.getOrCreateArcDrop(arcElem))
			} else if weakElem, isWeak := types.AsWeak(trackPhiI8Type); isWeak {
				c.trackTempWithDrop(phi, c.getOrCreateWeakDrop(weakElem))
			} else if mutexElem, isMutex := types.AsMutex(trackPhiI8Type); isMutex {
				c.trackTempWithDrop(phi, c.getOrCreateMutexDrop(mutexElem))
			} else if taskElem, isTask, taskFail := types.AsAnyTaskFailable(trackPhiI8Type); isTask {
				c.trackTempWithDrop(phi, c.getOrCreateTaskDrop(taskElem, taskFail))
			} else if _, isMG := types.AsMutexGuard(trackPhiI8Type); isMG {
				if dropFn, ok := c.funcs["MutexGuard.drop"]; ok {
					c.trackTempWithDrop(phi, dropFn)
				}
			} else if chElem, isCh := types.AsChannel(trackPhiI8Type); isCh {
				c.trackChannelTempWithElemType(phi, chElem)
			}
		} else if trackPhiHeapType != nil {
			// T1085: heap user-type inner (e.g. Map/Set) from an OWNED optional
			// ident source — the merged phi owns the present-arm inner (source
			// neutralized) and the absent-arm recovery. genExpr's
			// trackHeapUserTypeResult skips this ident source (T0753), so the phi
			// must be tracked here. trackHeapValueTemp re-validates (drop func
			// present, not a container/string) and is the single authority on
			// whether tracking actually happens.
			c.trackHeapValueTemp(phi, trackPhiHeapType)
		}
		// T1162: Owner-governed member source whose result is a single-owner opaque
		// handle (Channel/Mutex/MutexGuard/Task). These can't be deep-copied, so the
		// present-arm okVal aliases the owner's field (the owner's drop frees it)
		// while the absent-arm handlerVal is a fresh recovery handle nobody else
		// frees → leak. A single compile-time track/skip can't express both
		// ownerships, so register the merged phi as a statement temp with a
		// PER-BRANCH live flag: cleared (0) on the present (some) edge so the owner
		// stays sole owner, armed (1) on the absent (none/recovery) edge so the fresh
		// handle is dropped exactly once at statement end. Mirrors
		// trackElvisResultTemp's per-path flag (T0937/T0951). A `<-` await consumer
		// claims this temp via genReceiveTask's claimStringTemp(gRaw) (the await
		// joins+frees the G), and a bound use claims it via the var-decl
		// claimStringTemp — both neutralize the flag so the consumer/binding governs
		// the drop. Gated to no per-field dup (srcStringDup/srcContainerDup —
		// string/vector/Arc/Weak go through genFieldAccess's independent dup) and the
		// non-diverging path (a diverging handler produces no surviving recovery).
		if trackPhiI8Type == nil && srcStringDup == nil && srcContainerDup == nil &&
			c.tempTrackingEnabled && c.block.Term == nil && phi.Type() == irtypes.I8Ptr &&
			c.isOwnerGovernedMemberOptionalUnwrapSource(e.Expr) {
			if handleDrop := c.optionalHandlerHandleDrop(e); handleDrop != nil {
				if _, already := c.stmtTempMap[phi]; !already {
					flagPhi := c.block.NewPhi(
						&ir.Incoming{X: constant.NewInt(irtypes.I1, 0), Pred: someEnd},
						&ir.Incoming{X: constant.NewInt(irtypes.I1, 1), Pred: handlerEnd},
					)
					c.appendStmtTemp(phi, handleDrop, nil, flagPhi)
				}
			}
		}
		return phi
	}
	return okVal
}

// genOptionalForceUnwrap generates code for T? → T, panicking on none.
// Used by `as!` on optionals and `x!` on optionals.
// T0111: When source is an identifier with a drop binding, clears the drop flag
// (ownership transfers to the unwrapped value). Field access dup is handled by
// the dupStringFieldAccess mechanism in genTypedVarDecl/genInferredVarDecl.
func (c *Compiler) genOptionalForceUnwrap(expr ast.Expr) value.Value {
	// T1143: reset so a stale value from a prior dup-returning call can never
	// leak into this call's tracking decision. Only set below on the plain
	// (no-dup) path when the source is a container index.
	c.optionalUnwrapContainerBorrow = false
	optVal := c.genExpr(expr)
	flag := c.block.NewExtractValue(optVal, 0)

	okBlock := c.newBlock("unwrap.ok")
	panicBlock := c.newBlock("unwrap.panic")
	c.block.NewCondBr(flag, okBlock, panicBlock)

	c.block = panicBlock
	panicMsg := c.makeGlobalString("unwrap failed: optional is none")
	c.block.NewCall(c.funcs["promise_panic"], panicMsg)
	c.emitPanicReturn()

	c.block = okBlock
	// B0190: If genFieldAccess (B0181) created a dup for the inner string,
	// return the dup directly instead of extractvalue. This preserves the
	// value.Value identity so claimStringTemp can match it in VarDecl.
	if c.optionalStringDup != nil {
		dup := c.optionalStringDup
		c.optionalStringDup = nil
		return dup
	}
	// T0397: Same shape for tuple dup — when genMethodIndex created a dup for
	// the inner Optional[Tuple], return it directly so the binding takes
	// ownership of the deep-cloned tuple instead of an aliased extractvalue.
	if c.optionalTupleDup != nil {
		dup := c.optionalTupleDup
		c.optionalTupleDup = nil
		return dup
	}
	// T0440: Same shape for heap-user-type dup — when genMethodIndex created a
	// dup for the inner Optional[heap-user-type], return it directly so the
	// binding takes ownership of the cloned instance instead of an aliased
	// extractvalue.
	if c.optionalHeapDup != nil {
		dup := c.optionalHeapDup
		c.optionalHeapDup = nil
		return dup
	}
	var result value.Value
	result = c.block.NewExtractValue(optVal, 1)

	// T1143: we reached the plain extractvalue path, so no dup was made (the
	// binding/return/arg dup paths return early above). If the source is a
	// container index (`container[k]!`), the extracted inner aliases the
	// container's owned slot — record this so trackHeapUserTypeResult skips
	// owned-temp registration (the container's drop frees it; tracking the
	// alias as a temp double-frees at scope exit).
	c.optionalUnwrapContainerBorrow = c.isContainerIndexUnwrapSource(expr)

	// T0428 Case 3B: borrowed this.field! — dup the inner heap value so the new
	// variable gets an independent copy. The caller still owns the original (we
	// can't clear the present flag on a borrowed receiver), so both the caller's
	// synth drop and the new variable get independent copies to free.
	if member, ok := expr.(*ast.MemberExpr); ok {
		if isThisReceiver(member.Target) && !c.thisRecvIsOwned && !c.returningBorrowedUnwrap {
			innerType := c.info.Types[expr]
			if c.typeSubst != nil {
				innerType = types.Substitute(innerType, c.typeSubst)
			}
			if opt, optOk := innerType.(*types.Optional); optOk {
				innerElem := opt.Elem()
				if c.typeSubst != nil {
					innerElem = types.Substitute(innerElem, c.typeSubst)
				}
				innerNamed := extractNamed(innerElem)
				if innerNamed != nil && !innerNamed.IsValueType() && !innerNamed.IsCopy() &&
					!isPrimitiveScalar(innerNamed) && innerNamed != types.TypString &&
					!types.IsVector(innerElem) && !types.IsChannel(innerElem) &&
					!innerNamed.IsStructural() && !isOpaqueContainerType(innerElem) {
					result = c.dupHeapValue(result, innerElem)
				}
			}
		}
	}

	// T0111: Do NOT clear the drop flag here. The optional still owns the inner
	// value and will free it at scope exit via its drop binding. For temporary
	// access (opt!.len), this is correct — the inner stays alive until scope exit.
	// For assignment (val = opt!), the assignment site neutralizes the optional
	// by setting its present flag to false (see genTypedVarDecl/genInferredVarDecl).

	// Track the unwrapped i8* as a statement temp when the source is NOT an
	// identifier (e.g., method call returning string? or T[]?). For ident
	// sources, the optional's own drop handles the inner. For non-ident sources
	// (call results), the optional? temporary has no scope drop → the extracted
	// pointer must be tracked and freed at statement end.
	// B0299: Skip when optionalFieldString is set — the field comes from a
	// droppable type whose drop handles the string's lifetime. Tracking it
	// as a temp would cause double-free (statement-end + owner drop).
	// T0354: Same for optionalFieldVector — vector field on droppable type.
	// T0350: Type-aware tracking — strings via promise_string_drop, vectors via
	// Vector.drop with element type so heap elements (e.g., string[]) are dropped.
	// T0776: peel ParenExpr so `(o)!` is recognized like `o!` and the source
	// optional's drop owns the inner (mirrors expr.go:234, stmt.go
	// trackHeapUserTypeResult).
	// T0806: For a native-handle field (`Mutex[T]?` / `Task[T]?`) on a
	// droppable owner used as a temporary (`(h.mtx!).lock()`), the owner's
	// (possibly synthesized) drop already governs the handle. Registering the
	// extracted i8* as an owned statement temp double-frees it (statement-end
	// free + owner drop) → segfault. The string/vector siblings are guarded by
	// optionalFieldString/optionalFieldVector above; native handles have no
	// such flag, so skip via the same owner-governed member-source predicate
	// the heap-user case uses (isOwnerGovernedMemberOptionalUnwrapSource, T0775).
	// T1182 gap: a container/array-index borrow (`arr[i]!` / `vec[i]!`, or a
	// clone-less Map value) also aliases the container's owned slot — recorded in
	// optionalUnwrapContainerBorrow above. trackHeapUserTypeResult already skips
	// owned-temp registration for it, but the string/vector/Arc/Weak/Mutex/Task/
	// Channel tracking below is reached inline on the same no-dup path and was
	// unguarded, so an inline `string?[N]`/`Vector?[N]` element unwrap registered
	// the borrowed inner as a statement temp — a double free at scope exit (the
	// container's element drop frees it too). macOS's allocator turns this into a
	// fatal "invalid free (bad header magic)"; other allocators over-free silently.
	if !isIdentOptionalUnwrapSource(expr) && c.tempTrackingEnabled && !c.optionalFieldString && !c.optionalFieldVector &&
		!c.optionalUnwrapContainerBorrow && !c.isOwnerGovernedMemberOptionalUnwrapSource(expr) &&
		!c.isNestedOwnerGovernedUnwrapSource(expr) {
		if result.Type().Equal(irtypes.I8Ptr) {
			innerType := c.info.Types[expr]
			if opt, ok := innerType.(*types.Optional); ok {
				innerType = opt.Elem()
			}
			if c.typeSubst != nil && innerType != nil {
				innerType = types.Substitute(innerType, c.typeSubst)
			}
			named := extractNamed(innerType)
			if named == types.TypString {
				c.trackStringTemp(result)
			} else if named == types.TypVector {
				if elemType, ok := types.AsVector(innerType); ok {
					c.trackVectorTempWithElemType(result, elemType)
				} else {
					c.trackVectorTemp(result)
				}
			} else if arcElem, isArc := types.AsArc(innerType); isArc {
				c.trackTempWithDrop(result, c.getOrCreateArcDrop(arcElem))
			} else if weakElem, isWeak := types.AsWeak(innerType); isWeak {
				c.trackTempWithDrop(result, c.getOrCreateWeakDrop(weakElem))
			} else if mutexElem, isMutex := types.AsMutex(innerType); isMutex {
				// T0654: Optional<Mutex[T]> from a non-binding-site unwrap leaked
				// because the inner i8* fell through with no tracking. The
				// binding-site claim (stmt.go) is a no-op when no temp exists.
				c.trackTempWithDrop(result, c.getOrCreateMutexDrop(mutexElem))
			} else if taskElem, isTask, taskFail := types.AsAnyTaskFailable(innerType); isTask {
				c.trackTempWithDrop(result, c.getOrCreateTaskDrop(taskElem, taskFail))
			} else if _, isMG := types.AsMutexGuard(innerType); isMG {
				if dropFn, ok := c.funcs["MutexGuard.drop"]; ok {
					c.trackTempWithDrop(result, dropFn)
				}
			} else if chElem, isCh := types.AsChannel(innerType); isCh {
				c.trackChannelTempWithElemType(result, chElem)
			}
		}
	}

	return result
}

// isIdentOptionalUnwrapSource reports whether expr — the source of an optional
// unwrap (`opt!` or `opt? _ { ... }`) — is ultimately a bare identifier, peeling
// ParenExpr wrappers so `(o)!` / `((o))? _ { ... }` are recognized exactly like
// `o!` / `o? _ { ... }`. When true, the source optional has its own scope drop
// binding that governs the inner allocation's lifetime, so the unwrap-extracted
// inner must NOT be registered as an owned statement temp — doing so double-frees
// at scope exit (`fatal: invalid free`). genExpr already sees through ParenExpr
// (it recurses), so the peel only fixes the AST-shape check here; mirrors the
// ParenExpr peeling in neutralizeForceUnwrapSource (T0577).
func isIdentOptionalUnwrapSource(expr ast.Expr) bool {
	for {
		p, ok := expr.(*ast.ParenExpr)
		if !ok {
			break
		}
		expr = p.Expr
	}
	_, isIdent := expr.(*ast.IdentExpr)
	return isIdent
}

// isBorrowingPlaceExpr reports whether expr (peeling ParenExpr) is a place
// expression that, when discarded as a bare statement, only borrows the value it
// designates rather than producing an owned temp: a local/field read (`o`,
// `obj.f`) or an index read (`arr[i]`). The storage behind the place owns the
// value and frees it (variable binding / owner drop / container drop), so a
// discarded-result drop path (T1234) must skip these to avoid a double-free.
// A `move` out of a place is a MoveExpr — not matched here — so it still drops.
func isBorrowingPlaceExpr(expr ast.Expr) bool {
	for {
		p, ok := expr.(*ast.ParenExpr)
		if !ok {
			break
		}
		expr = p.Expr
	}
	switch expr.(type) {
	case *ast.IdentExpr, *ast.MemberExpr, *ast.IndexExpr:
		return true
	}
	return false
}

// isBorrowHoldingOptionalIdentSource reports whether expr (peeling ParenExpr) is
// an ident referring to a borrow-holding optional local — one bound from a
// non-owning borrow (RTTI downcast `x as T` / `T&`/`T~` RHS), recorded in
// borrowOptionalLocals at the var-decl borrow-clear sites (T1085). The present
// arm of a non-diverging handler unwrap on such a source aliases an external
// owner's instance, so it must NOT be neutralized + temp-tracked — the merged phi
// would otherwise drop a value the external owner still frees (double-free).
func (c *Compiler) isBorrowHoldingOptionalIdentSource(expr ast.Expr) bool {
	for {
		p, ok := expr.(*ast.ParenExpr)
		if !ok {
			break
		}
		expr = p.Expr
	}
	ident, ok := expr.(*ast.IdentExpr)
	if !ok {
		return false
	}
	return c.borrowOptionalLocals[ident.Name]
}

// isOwnerGovernedMemberOptionalUnwrapSource reports whether src — the source of
// an optional unwrap (`owner.field!` / `owner.field? _ { ... }`) — is a member
// access `owner.field` whose owner type has a (possibly synthesized) drop that
// governs the field's inner allocation's lifetime (T0775). When true, an unwrap
// used as a temporary must NOT register the extracted inner as an owned
// statement temp (force-unwrap path) — the owner's drop already frees it, so a
// statement-temp drop double-frees. Peels ParenExpr so `(owner.field)!` is
// recognized exactly like `owner.field!`. Mirrors the ident skip
// (isIdentOptionalUnwrapSource) for the member-source case.
func (c *Compiler) isOwnerGovernedMemberOptionalUnwrapSource(src ast.Expr) bool {
	for {
		p, ok := src.(*ast.ParenExpr)
		if !ok {
			break
		}
		src = p.Expr
	}
	mem, ok := src.(*ast.MemberExpr)
	if !ok {
		return false
	}
	ownerType := c.info.Types[mem.Target]
	if c.typeSubst != nil && ownerType != nil {
		ownerType = types.Substitute(ownerType, c.typeSubst)
	}
	if c.selfSubst != nil && ownerType != nil {
		ownerType = types.SubstituteSelf(ownerType, c.selfSubst.iface, c.selfSubst.concrete)
	}
	ownerNamed := extractNamed(ownerType)
	if ownerNamed == nil {
		return false
	}
	return c.ownerHasOrSynthDrop(ownerType, ownerNamed)
}

// isStructuralGetterMemberSource reports whether src (peeling ParenExpr) is a
// member access `owner.getter` that resolves to a GETTER whose result (unwrapping
// one Optional layer) is a non-value structural interface. Unlike a direct field
// read, a getter returns its structural view by VALUE: the accessor body deep-
// clones the `{vtable, instance}` box on field-escape (T1299), so the caller owns
// an independent box that must be tracked and dropped once — the owner's drop
// frees only its own field, not this returned clone. This lets trackHeapUserTypeResult
// treat a structural getter unwrap like the index-operator case (which is already
// owned because an IndexExpr isn't a MemberExpr), rather than as an owner-governed
// alias. T1299.
func (c *Compiler) isStructuralGetterMemberSource(src ast.Expr) bool {
	for {
		p, ok := src.(*ast.ParenExpr)
		if !ok {
			break
		}
		src = p.Expr
	}
	mem, ok := src.(*ast.MemberExpr)
	if !ok {
		return false
	}
	ownerType := c.info.Types[mem.Target]
	if c.typeSubst != nil && ownerType != nil {
		ownerType = types.Substitute(ownerType, c.typeSubst)
	}
	if c.selfSubst != nil && ownerType != nil {
		ownerType = types.SubstituteSelf(ownerType, c.selfSubst.iface, c.selfSubst.concrete)
	}
	ownerNamed := extractNamed(ownerType)
	if ownerNamed == nil || ownerNamed.LookupGetter(mem.Field) == nil {
		return false
	}
	rt := c.resolvedExprType(mem)
	if opt, ok := rt.(*types.Optional); ok {
		rt = opt.Elem()
		if c.typeSubst != nil {
			rt = types.Substitute(rt, c.typeSubst)
		}
	}
	return isNonValueStructuralType(rt)
}

// isModuleGetterExpr reports whether expr is a module-level getter access —
// either a bare `*ast.IdentExpr` resolving to a module-level getter (same-file
// or glob import) or a qualified `mod.prop` member recorded in
// c.info.ModuleGetters. Module getters take no receiver and always construct
// fresh, owned values (no mutable globals), so their non-value structural-box
// result is a genuine owned temp — safe to track for statement-end RTTI drop
// without any alias/double-free hazard, exactly like the instance-getter branch
// gated by isStructuralGetterMemberSource. T1321.
func (c *Compiler) isModuleGetterExpr(expr ast.Expr) bool {
	for {
		p, ok := expr.(*ast.ParenExpr)
		if !ok {
			break
		}
		expr = p.Expr
	}
	switch e := expr.(type) {
	case *ast.IdentExpr:
		// Not a local / mut-ref borrow — a genuine module-level name.
		if _, isLocal := c.locals[e.Name]; isLocal {
			return false
		}
		if c.mutRefPtrs != nil {
			if _, isMutRef := c.mutRefPtrs[e.Name]; isMutRef {
				return false
			}
		}
		if obj := c.lookupFunc(e.Name); obj != nil && obj.IsGetter() {
			return true
		}
		return false
	case *ast.MemberExpr:
		return c.info.ModuleGetters[e]
	}
	return false
}

// isNestedOwnerGovernedUnwrapSource reports whether src — the source of an
// optional unwrap — is itself an optional unwrap (`inner!` / `inner as! T`)
// chain that ultimately bottoms out in an owned ident or owner-governed member.
// This is the nested-Optional double-force shape `r!!` where `r: T??`: the
// outermost owner's (recursive) drop binding governs the innermost inner
// allocation, so the extracted inner is an ALIAS, not a transferred owner.
// Registering it as an owned statement temp double-frees at scope exit — the
// owner's nested-optional drop frees it too (fatal segfault / invalid free for
// native handles like Mutex/Task/MutexGuard whose drop releases OS resources).
// Peels ParenExpr and one-or-more nested force-unwrap layers (OptionalUnwrapExpr
// and force `as!` CastExpr), then reuses the same owner-governed checks the
// single-level guard uses (isIdentOptionalUnwrapSource /
// isOwnerGovernedMemberOptionalUnwrapSource). Only fires for a genuinely nested
// unwrap (at least one force layer peeled) so it never overlaps the single-level
// ident/member guards. A base that is NOT owner-governed (call/borrow result)
// falls through to the normal owned-temp tracking. T1215.
func (c *Compiler) isNestedOwnerGovernedUnwrapSource(src ast.Expr) bool {
	peeled := false
	for {
		switch s := src.(type) {
		case *ast.ParenExpr:
			src = s.Expr
			continue
		case *ast.OptionalUnwrapExpr:
			src = s.Expr
			peeled = true
			continue
		case *ast.CastExpr:
			if s.Force {
				src = s.Expr
				peeled = true
				continue
			}
		}
		break
	}
	if !peeled {
		return false
	}
	return isIdentOptionalUnwrapSource(src) || c.isOwnerGovernedMemberOptionalUnwrapSource(src)
}

// isContainerIndexUnwrapSource reports whether src (peeling ParenExpr) is a
// Map index `m[key]` whose `[]` getter returns Optional[V] by ALIAS — i.e. its
// match-destructure does NOT dup V (T0440). The synthesized `Map.[]` body does
// `match this._buckets[h] { Slot.Used(k, v) => return v, ... }`; the V binding
// is dup'd only when `enumHasDrop && matchFieldNeedsDup(V)` (bindEnumDestructure,
// expr.go), which for a non-enum V reduces to typeNeedsMatchDup(V). So the result
// aliases the bucket's slot exactly when typeNeedsMatchDup(V) is false (V has an
// explicit/synth drop but no usable clone — e.g. `Resource{string; drop()}` or a
// synth-drop struct with a droppable-element vector field). In that case the
// inline `m[k]!` force-unwrap (reaching the plain no-dup extractvalue path)
// borrows the slot, so it must NOT be registered as an owned statement temp —
// the Map's drop frees it; tracking double-frees. When V is clone-bearing the
// `[]` body dups internally, the result is owned, and tracking must stay (else a
// leak). Set is excluded implicitly — it has no `[]`. T1143.
func (c *Compiler) isContainerIndexUnwrapSource(src ast.Expr) bool {
	for {
		p, ok := src.(*ast.ParenExpr)
		if !ok {
			break
		}
		src = p.Expr
	}
	idx, ok := src.(*ast.IndexExpr)
	if !ok {
		return false
	}
	t := c.info.Types[idx.Target]
	if c.typeSubst != nil && t != nil {
		t = types.Substitute(t, c.typeSubst)
	}
	if c.selfSubst != nil && t != nil {
		t = types.SubstituteSelf(t, c.selfSubst.iface, c.selfSubst.concrete)
	}
	// T1182: fixed-array (`arr[i]!`) and Vector (`vec[i]!`) index sources. Their
	// native `[]` read in the plain (no-dup) unwrap path NEVER dups the element —
	// genArrayIndex / genVectorIndex only clone when a sibling dup flag (e.g.
	// dupHeapUserFieldAccess) is set, which happens only in binding/return/arg
	// contexts, not the inline-temp context reaching this predicate. So the
	// unwrap-extracted inner ALWAYS aliases the array/vector's owned slot; the
	// container's element drop frees it, and registering it as an owned temp
	// double-frees at scope exit (segfault for heap-user elements whose drop
	// derefs a freed sub-field, silent over-free for strings). Unlike Map/Set
	// (which dup clone-bearing V inside `[]`), fixed arrays/Vectors have no
	// internal dup here, so the result is ALWAYS a borrow — return true
	// unconditionally. (When a dup DOES occur in a binding/return/arg context,
	// genOptionalForceUnwrap returns early via optionalHeapDup before reaching
	// this predicate, so the flag is only ever set on the genuine borrow path.)
	if _, isArr := t.(*types.Array); isArr {
		return true
	}
	if types.IsVector(t) {
		return true
	}
	if !isMapOrSetType(t) {
		return false
	}
	// Element type V = the index expression's result, peeling the Optional the
	// `[]` getter returns. The aliasing-vs-dup decision must mirror the `[]`
	// body's match-destructure (typeNeedsMatchDup) exactly.
	vt := c.info.Types[idx]
	if c.typeSubst != nil && vt != nil {
		vt = types.Substitute(vt, c.typeSubst)
	}
	if c.selfSubst != nil && vt != nil {
		vt = types.SubstituteSelf(vt, c.selfSubst.iface, c.selfSubst.concrete)
	}
	if opt, ok := vt.(*types.Optional); ok {
		vt = opt.Elem()
		if c.typeSubst != nil && vt != nil {
			vt = types.Substitute(vt, c.typeSubst)
		}
		if c.selfSubst != nil && vt != nil {
			vt = types.SubstituteSelf(vt, c.selfSubst.iface, c.selfSubst.concrete)
		}
	}
	if vt == nil {
		return false
	}
	return !c.typeNeedsMatchDup(vt)
}

// handlerResultIsNativeHandle reports whether an optional handler's unwrapped
// result type is a single-owner native handle (Mutex[T]/Task[T]) — opaque i8*
// handles that genOptionalHandlerExpr does NOT dup. T0838: such member-source
// handler bindings must neutralize the owner's optional field to avoid a
// double-free between the bound local and the owner's drop. The
// typeSubst/selfSubst resolution mirrors genOptionalHandlerExpr so it works
// inside monomorphized generic and structural-default method bodies.
func (c *Compiler) handlerResultIsNativeHandle(e *ast.ErrorHandlerExpr) bool {
	rt := c.info.Types[e]
	if c.typeSubst != nil && rt != nil {
		rt = types.Substitute(rt, c.typeSubst)
	}
	if c.selfSubst != nil && rt != nil {
		rt = types.SubstituteSelf(rt, c.selfSubst.iface, c.selfSubst.concrete)
	}
	if _, ok := types.AsMutex(rt); ok {
		return true
	}
	_, ok := types.AsAnyTask(rt)
	return ok
}

// optionalHandlerHandleDrop resolves the native drop function for an optional
// handler result that is a single-owner opaque handle represented as a bare i8*
// — Channel[T], Mutex[T], MutexGuard[T], Task[T] (T1162). These cannot be
// deep-copied (genOptionalHandlerExpr's !isOpaqueContainerType dup gate skips
// them), so for an owner-governed member source the present-arm aliases the
// owner's field while the absent-arm recovery handle is left unowned. Returns
// nil for every other result type. rt is substituted first so element types from
// types.As* are concrete. Mirrors elvisResultHandleDrop (T0951) but scoped to the
// handle classes whose recovery actually leaks here — Arc[T]/Weak[T] refcount-dup
// via genFieldAccess's srcContainerDup path and are excluded.
func (c *Compiler) optionalHandlerHandleDrop(e *ast.ErrorHandlerExpr) *ir.Func {
	rt := c.info.Types[e]
	if c.typeSubst != nil && rt != nil {
		rt = types.Substitute(rt, c.typeSubst)
	}
	if c.selfSubst != nil && rt != nil {
		rt = types.SubstituteSelf(rt, c.selfSubst.iface, c.selfSubst.concrete)
	}
	if rt == nil || isRefType(rt) {
		return nil
	}
	named := extractNamed(rt)
	if chElem, ok := types.AsChannel(rt); ok || named == types.TypChannel {
		return c.getOrCreateChannelDrop(chElem)
	}
	if mutexElem, ok := types.AsMutex(rt); ok {
		return c.getOrCreateMutexDrop(mutexElem)
	}
	if _, ok := types.AsMutexGuard(rt); ok || named == types.TypMutexGuard {
		return c.funcs["MutexGuard.drop"]
	}
	if taskElem, ok, taskFail := types.AsAnyTaskFailable(rt); ok {
		return c.getOrCreateTaskDrop(taskElem, taskFail)
	}
	return nil
}

// isBorrowedThisMemberSource reports whether src (peeling ParenExpr) is a member
// access on a borrowed `this` receiver (`this.field` inside a non-`~this`
// method). For force-unwrap (T0428 Case 3B) genOptionalForceUnwrap makes an
// INDEPENDENT dup of the inner there — the caller still owns the original — so
// that dup DOES need statement-temp tracking and must be excluded from the
// T0775 member skip. (T0775)
func (c *Compiler) isBorrowedThisMemberSource(src ast.Expr) bool {
	for {
		p, ok := src.(*ast.ParenExpr)
		if !ok {
			break
		}
		src = p.Expr
	}
	mem, ok := src.(*ast.MemberExpr)
	if !ok {
		return false
	}
	return isThisReceiver(mem.Target) && !c.thisRecvIsOwned
}

// isForceUnwrapElem reports whether expr (peeling ParenExpr) is a bare
// force-unwrap `o!`. Collection-literal / raise / select-send element sites use
// this to neutralize the source optional after a force-unwrap consume (T1073) —
// the cast form (`o as! T`) is handled separately via consumeCastSubjectDropFlag,
// so guarding to the force-unwrap shape avoids double-neutralizing it.
func isForceUnwrapElem(expr ast.Expr) bool {
	for {
		p, ok := expr.(*ast.ParenExpr)
		if !ok {
			break
		}
		expr = p.Expr
	}
	_, ok := expr.(*ast.OptionalUnwrapExpr)
	return ok
}

// neutralizeForceUnwrapElem neutralizes the source optional of a force-unwrap
// element `o!` whose unwrapped inner needs dropping. Used at the collection-literal
// (array/tuple/map), raise, and select-send consume sites where `o!` moves the
// inner into a container / error-slot / channel that owns and frees it. Without
// this, the source optional's own scope-exit drop double-frees the moved inner
// (T1073). Self-gating: a no-op unless the element is a bare force-unwrap (the
// cast form is handled via consumeCastSubjectDropFlag) of a droppable inner
// (copy/scalar inners aren't consumed, so their source must stay intact).
func (c *Compiler) neutralizeForceUnwrapElem(elemExpr ast.Expr) {
	if !isForceUnwrapElem(elemExpr) {
		return
	}
	t := c.info.Types[elemExpr]
	if c.typeSubst != nil {
		t = types.Substitute(t, c.typeSubst)
	}
	if !c.typeNeedsFieldDrop(t) {
		return
	}
	c.neutralizeForceUnwrapSource(elemExpr)
}

// neutralizeForceUnwrapSource sets the present flag to false in the source
// optional's alloca when a force-unwrap result is consumed by an assignment.
// T0111: Prevents double-free when both the new variable and the source optional
// would otherwise try to drop the same inner value. Called from many assignment
// and arg-passing sites in expr.go (call-arg paths) and stmt.go (var decls,
// destructure, assign, for/yield).
func (c *Compiler) neutralizeForceUnwrapSource(expr ast.Expr) {
	// T0577: peel ParenExpr wrappers so `(opt!)` neutralizes like `opt!`.
	// genExpr already sees through ParenExpr; this peel fixes the AST-shape
	// dispatch below. A second peel inside the T0436 inner loop handles the
	// mirror case `(opt)!` (parens around the source, not the unwrap).
	for {
		p, ok := expr.(*ast.ParenExpr)
		if !ok {
			break
		}
		expr = p.Expr
	}
	// Extract the source identifier from opt!, opt as! T, or opt? _ { fallback }.
	var inner ast.Expr
	switch e := expr.(type) {
	case *ast.OptionalUnwrapExpr:
		inner = e.Expr
	case *ast.CastExpr:
		// B0293: as! on optionals also force-unwraps — must neutralize source.
		// Only applies to optional→concrete casts, not inheritance downcasts.
		if e.Force {
			if _, isOpt := c.info.Types[e.Expr].(*types.Optional); isOpt {
				inner = e.Expr
			}
		}
	case *ast.ErrorHandlerExpr:
		// B0293: optional handler (p? _ { fallback }) also extracts inner value.
		if _, isOpt := c.info.Types[e.Expr].(*types.Optional); isOpt {
			// T0775: member-source handlers (`owner.field? _ { ... }`) are made
			// independent by the present-arm dup in genOptionalHandlerExpr — the
			// owner keeps & frees the original, so the field must NOT be neutralized
			// (neutralizing would orphan the original → leak; not neutralizing keeps
			// the field reusable). Ident sources have no dup and still neutralize.
			//
			// T0838 EXCEPTION: Mutex[T]/Task[T] are single-owner opaque i8*
			// handles that genOptionalHandlerExpr does NOT dup (opaque containers
			// can't be deep-copied — see its !isOpaqueContainerType gate). So a
			// handler BINDING `Mutex[int] m = h.mtx? _ {...}` would leave both the
			// bound local and the owner's optional field owning the same handle →
			// double-free. Neutralize the owner field for these, mirroring T0806
			// Fix C for the force-unwrap binding. neutralizeMemberOptionalField
			// applies the same Mutex/Task carve-out (T0806) when clearing the flag.
			if !c.isOwnerGovernedMemberOptionalUnwrapSource(e.Expr) || c.handlerResultIsNativeHandle(e) {
				inner = e.Expr
			}
		}
	}
	if inner == nil {
		return
	}
	// T0436: traverse nested force-unwraps (e.g., `b := h.data!!`).
	// Each nested OptionalUnwrapExpr exposes one Optional level; we only need to
	// clear the OUTERMOST present flag (on the original source) — synth drop will
	// then skip the field entirely and not descend into the inner Optional.
	// T0577: also peel ParenExpr inside the chain so `(opt)!`, `(opt) as! T`,
	// `(opt)? _ { ... }`, and combinations like `((opt))!` all reach IdentExpr.
	for {
		if uw, ok := inner.(*ast.OptionalUnwrapExpr); ok {
			inner = uw.Expr
			continue
		}
		if p, ok := inner.(*ast.ParenExpr); ok {
			inner = p.Expr
			continue
		}
		break
	}
	// Clear the source optional's present flag (ident) or the owner's optional
	// field flag (member) — shared with the optional-cast move path (T0761). The
	// MemberExpr arm (T0392) clears the present flag in the owner's instance
	// memory so the owner's drop skips the field rather than double-freeing.
	c.neutralizeOptionalCastSource(inner)
}

// neutralizeMemberOptionalField clears the present flag of an Optional[heap-user-type]
// field on an owned variable (T0392). Handles:
//   - Simple `ident.field!` (original case)
//   - T0428 Case 1: `ident.field!!` (T?? field — look through inner Optional for guard checks)
//   - T0428 Case 2: `outer.inner.field!` (chained MemberExpr — walk chain)
//   - T0428 Case 3A: `this.field!` inside ~this method (owned receiver)
//
// String/Vector/Channel/Arc/Weak optional fields are skipped because genFieldAccess
// already dups them — clearing the flag would leak the original.
// T0428 Case 3B (borrowed this.field!): handled in genOptionalForceUnwrap via dup.
func (c *Compiler) neutralizeMemberOptionalField(m *ast.MemberExpr) {
	// T0428 Case 2: Walk the MemberExpr chain to find the root variable.
	// chain[i] = step i from root toward leaf. chain[0].Target is the root.
	// chain[last] = m (the final Optional field access).
	// T0613: peel ParenExpr at each chain step so paren-wrapped roots/links
	// ((this).field!, (outer).inner.field!) resolve to the IdentExpr/ThisExpr
	// the switch below handles, rather than falling through to the default arm.
	chain := []*ast.MemberExpr{m}
	cur := ast.Expr(unwrapDestructureParens(m.Target))
	for {
		if me, ok := cur.(*ast.MemberExpr); ok {
			chain = append([]*ast.MemberExpr{me}, chain...)
			cur = unwrapDestructureParens(me.Target)
		} else {
			break
		}
	}

	// Resolve the root alloca and initial owner named type.
	var ownerAlloca *ir.InstAlloca
	var ownerType types.Type // used for layout lookup (ident-rooted chains)
	var ownerNamed *types.Named
	var rootIsThis bool
	// T0843: when the chain root is a container element (`cs[0].tsk!`), there is
	// no alloca for the element — we GEP/extract the heap instance pointer
	// directly and stash it here so the post-switch code uses it as-is.
	var precomputedInstance value.Value

	switch root := cur.(type) {
	case *ast.IdentExpr:
		a, ok := c.locals[root.Name]
		if !ok {
			return // not an owned local
		}
		ownerAlloca = a
		ownerType = c.info.Types[root]
		if c.typeSubst != nil {
			ownerType = types.Substitute(ownerType, c.typeSubst)
		}
		// Don't neutralize through a borrow.
		if _, isShared := ownerType.(*types.SharedRef); isShared {
			return
		}
		if _, isMut := ownerType.(*types.MutRef); isMut {
			return
		}
		ownerNamed = extractNamed(ownerType)
		if ownerNamed == nil {
			return
		}
		// Only neutralize when the owner's drop would actually visit this field.
		if ownerNamed.IsValueType() || ownerNamed.IsStructural() {
			return
		}
		if !ownerNamed.HasDrop() && !ownerNamed.NeedsSynthDrop() {
			if inst, ok := ownerType.(*types.Instance); ok {
				if !monoInstNeedsSynthDrop(inst) {
					return
				}
			} else {
				return
			}
		}
	case *ast.ThisExpr:
		// T0428 Case 3A: owned ~this — 'this' alloca holds a raw i8* instance ptr.
		// T0428 Case 3B: borrowed this — handled via dup in genOptionalForceUnwrap; skip here.
		if !c.thisRecvIsOwned {
			return
		}
		a, ok := c.locals["this"]
		if !ok {
			return
		}
		ownerAlloca = a
		ownerNamed = c.currentNamed
		if ownerNamed == nil {
			return
		}
		if ownerNamed.IsValueType() || ownerNamed.IsStructural() {
			return
		}
		if !ownerNamed.HasDrop() && !ownerNamed.NeedsSynthDrop() {
			return
		}
		ownerType = ownerNamed
		rootIsThis = true
	case *ast.IndexExpr:
		// T0843: optional single-owner handle field (Task[T]?/Mutex[T]?) on an
		// element of an OWNED container, consumed by an await (`<-(cs[0].tsk!)`) or
		// taken by a binding move (`Mutex[int] m = cs[0].mtx!`). The unwrap takes the
		// handle out; clear the element's optional present flag so the container's
		// scope-exit element drop does not double-free it. Mirrors the IdentExpr
		// owned-local path. Reached from both genReceiveTask (await) and
		// neutralizeForceUnwrapSource (binding move) — so this also covers the
		// optional-Mutex half of T0842. The non-optional `<-cs[0].t` is already covered
		// by the T0638 genReceiveTaskSlotPtr slot-null; the non-optional Mutex move
		// `cs[0].m` (no `!`) has neither slot-null nor optional flag and is the open
		// T0842 gap.
		containerType := c.info.Types[root.Target]
		if c.typeSubst != nil {
			containerType = types.Substitute(containerType, c.typeSubst)
		}
		// Don't neutralize through a borrowed container.
		if _, isShared := containerType.(*types.SharedRef); isShared {
			return
		}
		if _, isMut := containerType.(*types.MutRef); isMut {
			return
		}
		elemType := c.info.Types[root] // type of cs[0] = element type
		if c.typeSubst != nil {
			elemType = types.Substitute(elemType, c.typeSubst)
		}
		ownerNamed = extractNamed(elemType)
		if ownerNamed == nil {
			return
		}
		if ownerNamed.IsValueType() || ownerNamed.IsStructural() {
			return
		}
		if !ownerNamed.HasDrop() && !ownerNamed.NeedsSynthDrop() {
			if inst, ok := elemType.(*types.Instance); ok {
				if !monoInstNeedsSynthDrop(inst) {
					return
				}
			} else {
				return
			}
		}
		ownerType = elemType
		precomputedInstance = c.extractInstancePtr(c.genExpr(root))
	default:
		return
	}

	// Load the root instance pointer.
	var rootInstance value.Value
	if precomputedInstance != nil {
		// T0843: container element — heap instance ptr already computed above.
		rootInstance = precomputedInstance
	} else {
		ownerVal := c.block.NewLoad(ownerAlloca.ElemType, ownerAlloca)
		if rootIsThis {
			// ~this: the alloca holds an i8* instance pointer directly.
			rootInstance = ownerVal
		} else {
			rootInstance = c.extractInstancePtr(ownerVal)
		}
	}

	// Walk the chain. For each step, GEP through the instance to the field.
	// For intermediate steps: load the value struct, extract instance ptr, advance named type.
	// For the final step: validate Optional field type and clear the present flag.
	curInstance := rootInstance
	curNamed := ownerNamed
	curType := ownerType

	for i, step := range chain {
		stepLayout := c.lookupTypeLayout(curType)
		if stepLayout == nil || stepLayout.IsValueType {
			return
		}
		stepFieldIdx, ok := stepLayout.InstanceFieldIndex[step.Field]
		if !ok {
			return
		}
		typedPtr := c.block.NewBitCast(curInstance, stepLayout.InstancePtrType)
		fieldPtr := c.block.NewGetElementPtr(stepLayout.Instance.LLVMType, typedPtr,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(stepFieldIdx)))
		fieldLLVMType := stepLayout.Instance.Fields[stepFieldIdx].LLVMType

		if i < len(chain)-1 {
			// Intermediate step: load the value struct and extract instance ptr.
			fieldVal := c.block.NewLoad(fieldLLVMType, fieldPtr)
			fieldSt, ok2 := fieldLLVMType.(*irtypes.StructType)
			if !ok2 || len(fieldSt.Fields) < 2 {
				return
			}
			curInstance = c.block.NewExtractValue(fieldVal, 1)
			// Advance named/type for next step.
			stepField := curNamed.LookupField(step.Field)
			if stepField == nil {
				return
			}
			nextType := stepField.Type()
			if c.typeSubst != nil {
				nextType = types.Substitute(nextType, c.typeSubst)
			}
			curNamed = extractNamed(nextType)
			if curNamed == nil {
				return
			}
			curType = nextType
			continue
		}

		// Final step: validate the Optional field type and clear the present flag.
		// Look up the field from curNamed to get its declared type.
		stepField := curNamed.LookupField(step.Field)
		if stepField == nil {
			return
		}
		fType := stepField.Type()
		if c.typeSubst != nil {
			fType = types.Substitute(fType, c.typeSubst)
		}
		opt, isOpt := fType.(*types.Optional)
		if !isOpt {
			return
		}
		// T0428 Case 1: For T?? fields, opt.Elem() is itself Optional[T]. Look through
		// to find the deep inner named type for guard checks, but still neutralize the
		// outermost Optional's present flag (field 0 of the field struct).
		elem := opt.Elem()
		innerElem := elem
		if innerOpt, ok2 := elem.(*types.Optional); ok2 {
			innerElem = innerOpt.Elem()
		}
		innerNamed := extractNamed(innerElem)
		// Skip for inner types where genFieldAccess already dups.
		if innerNamed == types.TypString || types.IsVector(innerElem) || types.IsChannel(innerElem) ||
			types.IsArc(innerElem) || types.IsWeak(innerElem) {
			return
		}
		// T0806: Mutex[T]/Task[T] are single-owner opaque i8* handles that
		// genFieldAccess does NOT dup (unlike Arc/Weak refcount-dups). Moving one
		// out of an owner's optional field (binding force-unwrap, or `<-(h.tsk!)`
		// which consumes the task) must clear the owner's present flag so the
		// owner's drop does not double-free the handle we already took ownership
		// of. So let these two through the opaque-container skip below.
		_, isMutexField := types.AsMutex(innerElem)
		_, isTaskField := types.AsAnyTask(innerElem)
		if innerNamed == nil || innerNamed.IsValueType() || innerNamed.IsCopy() ||
			isPrimitiveScalar(innerNamed) || innerNamed.IsStructural() ||
			(isOpaqueContainerType(innerElem) && !isMutexField && !isTaskField) {
			return
		}

		// GEP to the Optional present flag (field 0) and clear it.
		optStruct, ok2 := fieldLLVMType.(*irtypes.StructType)
		if !ok2 || len(optStruct.Fields) < 2 {
			return
		}
		flagPtr := c.block.NewGetElementPtr(optStruct, fieldPtr,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
		c.block.NewStore(constant.NewInt(irtypes.I1, 0), flagPtr)
	}
}
