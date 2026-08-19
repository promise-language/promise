package codegen

import (
	"fmt"

	"github.com/llir/llvm/ir"
	"github.com/llir/llvm/ir/constant"
	irtypes "github.com/llir/llvm/ir/types"
	"github.com/llir/llvm/ir/value"

	"github.com/promise-language/promise/compiler/internal/ast"
	"github.com/promise-language/promise/compiler/internal/types"
)

// --- Variable declarations ---

func (c *Compiler) genTypedVarDecl(s *ast.TypedVarDecl) {
	// Uninitialized optional: `T? x;` — zero-init (none)
	if s.Value == nil {
		declType := c.resolveTypeRefToType(s.Type)
		if declType == nil {
			return
		}
		lt := c.resolveType(declType)
		alloca := c.createEntryAlloca(lt)
		alloca.SetName(c.uniqueLocalName(s.Name))
		c.block.NewStore(constant.NewZeroInitializer(lt), alloca)
		c.locals[s.Name] = alloca
		return
	}

	// Resolve the declared type (from sema's type annotation)
	declType := c.lookupLocalType(s)
	exprType := c.info.Types[s.Value]

	// Use declared type for alloca when available (handles NoneLit → Optional)
	var lt irtypes.Type
	if declType != nil {
		lt = c.resolveType(declType)
	} else {
		// Check if the AST declares a structural interface type that differs from the
		// expression type (e.g., `Encodable e = 42;` — alloca must be {i8*,i8*} not i64).
		// Only apply for structural interfaces to avoid breaking generics/value types.
		astDeclType := c.resolveTypeRefToType(s.Type)
		if astDeclNamed := extractNamed(astDeclType); astDeclNamed != nil && astDeclNamed.IsStructural() {
			if exprNamed := extractNamed(exprType); exprNamed != nil && exprNamed != astDeclNamed {
				lt = c.resolveType(astDeclType)
				declType = astDeclType
			} else {
				lt = c.resolveType(exprType)
			}
		} else {
			lt = c.resolveType(exprType)
		}
	}
	alloca := c.createEntryAlloca(lt)
	alloca.SetName(c.uniqueLocalName(s.Name))

	// Set targetType for contextual type resolution (NoneLit needs Optional(T))
	if declType != nil {
		c.targetType = declType
	}
	// T0095: Signal genFieldAccess to dup string fields from droppable types.
	// The variable will own the copy; without dup, both the var's drop and the
	// type's synthesized drop would free the same allocation.
	// B0179: Skip dup for borrow types (SharedRef/MutRef) — borrows don't own
	// the value, so duping would create a temp that gets freed while the borrow
	// still points to it (double-free / use-after-free).
	resolvedExprType := exprType
	if c.typeSubst != nil && resolvedExprType != nil {
		resolvedExprType = types.Substitute(resolvedExprType, c.typeSubst)
	}
	if extractNamed(resolvedExprType) == types.TypString && !isRefType(resolvedExprType) {
		c.dupStringFieldAccess = true
	}
	// B0310: Also set dup flag for Optional[string] fields.
	if opt, ok := resolvedExprType.(*types.Optional); ok && extractNamed(opt.Elem()) == types.TypString {
		c.dupStringFieldAccess = true
	}
	// B0219: Signal genFieldAccess to dup vector/channel/arc/weak fields from droppable types.
	if (types.IsVector(resolvedExprType) || types.IsChannel(resolvedExprType) || types.IsArc(resolvedExprType) || types.IsWeak(resolvedExprType)) && !isRefType(resolvedExprType) {
		c.dupContainerFieldAccess = true
	}
	// T0366: Also set dup flag for Optional[Vector|Channel|Arc|Weak] fields. Without
	// duping, both the source's owner drop and the new variable's optional drop would
	// drop the same inner buffer → double-free.
	if opt, ok := resolvedExprType.(*types.Optional); ok {
		elem := opt.Elem()
		if types.IsVector(elem) || types.IsChannel(elem) || types.IsArc(elem) || types.IsWeak(elem) {
			c.dupContainerFieldAccess = true
		}
	}
	// T0370: Set dup flag for droppable tuple types so genVectorIndex deep-clones
	// tuple elements on read. Without this, `t := v[0]` aliases v's element data
	// and bindingDropTuple would double-free with v's element walk.
	if _, isTup := resolvedExprType.(*types.Tuple); isTup && c.tupleNeedsDrop(resolvedExprType) {
		c.dupTupleFieldAccess = true
	}
	// T0397: Same flag for typed `(...)? opt = m[k]` — Optional[Tuple] LHS where
	// the inner tuple has droppable fields aliased into the container's bucket.
	// (Not borrow-gated — checks Optional[Tuple] type shape. Remains active post-T0438.)
	if opt, ok := resolvedExprType.(*types.Optional); ok {
		elem := opt.Elem()
		if c.typeSubst != nil {
			elem = types.Substitute(elem, c.typeSubst)
		}
		if _, isTup := elem.(*types.Tuple); isTup && c.tupleNeedsDrop(elem) {
			c.dupTupleFieldAccess = true
		}
	}
	// T0398: Set dup flag for heap user types so genVectorIndex deep-clones the
	// element on read. Without this, `b := v[0]` aliases v's element instance
	// pointer — b's drop binding and v's element walk would double-free. Only
	// fires when the RHS is a direct vector-index expression: chains like
	// `b := v[0].method()` are excluded because the cloned receiver would not
	// be consumed (method takes a borrow), leaking the clone.
	// (Not borrow-gated — checks AST shape (IndexExpr) and element type. Remains active post-T0438.)
	//
	// T0440: Also set the flag for `b := m[k]!` — the RHS unwraps an
	// Optional[heap-user-type] from a Map index. The unwrap consumes the
	// Optional and returns V; without the dup, b would alias the bucket.
	//
	// T0590: Also fire for heap-user-no-drop types (`_Bare[2]`) when the RHS is
	// a direct IndexExpr. These need dup-on-read in arrays so let-then-X reads
	// don't alias pal_free'd allocations.
	if isDroppableHeapUserType(resolvedExprType) || isHeapUserNoDropPalFree(resolvedExprType) {
		if _, isIdx := s.Value.(*ast.IndexExpr); isIdx {
			c.dupHeapUserFieldAccess = true
		} else if unwrap, isUnwrap := s.Value.(*ast.OptionalUnwrapExpr); isUnwrap {
			if _, isInnerIdx := unwrap.Expr.(*ast.IndexExpr); isInnerIdx {
				c.dupHeapUserFieldAccess = true
			}
		}
	}
	// T1129: bare droppable-enum element read from a native Vector/Array index
	// (`got := v[i]`) must be deep-cloned so the binding owns independent variant
	// data — else got's drop and the container's element walk double-free (fatal
	// for recursive enums). Only the bare IndexExpr form needs this: the Map form
	// (`got := m[k]!`) is owned by the `[]` method body's match-dup, and a borrowed
	// view (`match v[i]`) takes no owning drop. genVectorIndex/genArrayIndex consume
	// the flag via cloneResolvedValue.
	if c.enumElemNeedsDupOnRead(resolvedExprType) {
		if _, isIdx := s.Value.(*ast.IndexExpr); isIdx {
			c.dupHeapUserFieldAccess = true
		}
	}
	// T1130: Map/Set element read-back from a native Vector/Array index in a typed
	// var decl (`Map[K,V] got = v[i]`) must be deep-cloned so the binding owns an
	// independent Map/Set — else got's drop and the container's element walk double-
	// free. Mirrors the inferred-var-decl and assignment sites. isMapOrSetType
	// excludes the bare `m[k]` (Map container) form: that yields an Optional, handled
	// by the Optional branch below / the Map.[] body's own dup.
	if isMapOrSetType(resolvedExprType) {
		if _, isIdx := s.Value.(*ast.IndexExpr); isIdx {
			c.dupHeapUserFieldAccess = true
		}
	}
	// T1287: bare structural-interface element read from a native Vector index
	// (`Showable x = v[i]`) must deep-clone the {vtable, instance} box so the binding
	// owns an independent box — else x aliases the vector's element and dropping the old
	// box on overwrite (genVectorIndexAssign, T1287) or the vector's element walk (T1284)
	// leaves x dangling (UAF) / double-frees. genVectorIndex consumes the flag via
	// cloneStructuralView. Mirrors the inferred-var-decl site.
	if isNonValueStructuralType(resolvedExprType) {
		if _, isIdx := s.Value.(*ast.IndexExpr); isIdx {
			c.dupHeapUserFieldAccess = true
		}
	}
	// T0440: Same flag for typed `T? b = m[k]` — Optional[heap-user-type] LHS
	// where the inner value aliases the container's bucket. Set the flag so
	// genMethodIndex deep-clones via cloneHeapElement.
	// T1291: also fire for an Optional[structural] inner (`Showable? z = v[i]`) —
	// its element drop is now active, so the aliased read must clone the box.
	if opt, ok := resolvedExprType.(*types.Optional); ok {
		elem := opt.Elem()
		if c.typeSubst != nil {
			elem = types.Substitute(elem, c.typeSubst)
		}
		if isDroppableHeapUserType(elem) || isHeapUserNoDropPalFree(elem) || isNonValueStructuralType(elem) {
			c.dupHeapUserFieldAccess = true
		}
	}
	// T1230: a closure-nesting aggregate read from an aliasing container (`Fn fn = m[k]!`
	// on a `Fn{()->int}` value) is a borrow — the env can't be deep-cloned. Suppress
	// the dup-on-read set above so the local aliases the container's instance with the
	// env intact; the owning drop is cleared after maybeRegisterDrop below.
	if c.isClosureAggregateBorrow(s.Value) {
		c.dupHeapUserFieldAccess = false
	}
	// T0952: `Mutex[int] m = a ?: b` — signal genElvis (as in genInferredVarDecl) so
	// the none-path default's owner is neutralized; the bound variable owns the temp.
	prevElvisBound := c.elvisResultBound
	if be, ok := unwrapDestructureParens(s.Value).(*ast.BinaryExpr); ok && be.Op == ast.BinElvis {
		c.elvisResultBound = true
	}
	val := c.genExpr(s.Value)
	c.elvisResultBound = prevElvisBound
	c.dupStringFieldAccess = false
	c.dupContainerFieldAccess = false
	c.dupTupleFieldAccess = false
	c.dupHeapUserFieldAccess = false
	c.targetType = nil

	// T0685: Defensive — if the RHS produced no value (e.g., a type expression
	// like bare `T[]` slipped past sema), bail out with a diagnostic panic
	// rather than nil-storing through llir. Sema should have already rejected
	// these inputs, but this guard prevents future sema gaps from showing up
	// as opaque SIGSEGVs deep in github.com/llir/llvm.
	if val == nil {
		panic(fmt.Sprintf("codegen: nil value for typed var decl %q at %v (likely a sema gap — type expression used in value position)", s.Name, s.Pos()))
	}

	// Auto-propagate failable call in assignment: check tag, propagate error, extract ok value.
	if c.info.AutoPropagateExprs[s.Value] {
		val = c.genAutoPropagateValue(val)
	}

	// T1179: deep-clone a whole match-borrowed Array/Optional heap-user payload
	// bound by this var-decl so the new local owns independent data (see
	// cloneBorrowedWholePayloadVarDecl). resolvedExprType is the substituted RHS
	// type — for a whole-payload binding it equals the declared Array/Optional type.
	val = c.cloneBorrowedWholePayloadVarDecl(val, s.Value, resolvedExprType)

	// T0111: Claim string temp BEFORE optional wrapping. After wrapOptional, the
	// value identity changes and claimStringTemp can't find the tracked temp.
	// T0555: Also claim native handle / container temps before the wrap.
	// Without this, the post-wrap claim site (which uses the wrapped struct)
	// cannot locate the tracked i8* temp, so the stmt-temp drop AND the
	// optional binding drop both fire → double-free.
	var mergeWrapFlag value.Value // T1210
	if declType != nil {
		if _, isOpt := declType.(*types.Optional); isOpt {
			if exprType != nil {
				if extractNamed(exprType) == types.TypString ||
					types.IsVector(exprType) || types.IsChannel(exprType) ||
					types.IsArc(exprType) || types.IsWeak(exprType) ||
					types.IsMutex(exprType) || types.IsAnyTask(exprType) ||
					types.IsMutexGuard(exprType) {
					// T1210: an Optional-wrapped mixed owned/borrowed match/if result
					// owns its inner value only on the paths that selected an owned
					// arm. Capture the merge temp's live per-path flag BEFORE this
					// claim (and wrapOptional below) neutralize/rebind it; re-applied
					// to the wrapped Optional's drop flag after
					// maybeRegisterOptionalDrop. Elvis RHS is handled separately; the
					// member optional handler (ErrorHandlerExpr) is excluded — see
					// isMixedMergeBindingRHS (shared with T1209's plain-binding fix).
					if c.elvisBoundDropFlag == nil && isMixedMergeBindingRHS(s.Value) {
						mergeWrapFlag = c.captureLiveTempFlag(val)
					}
					c.claimStringTemp(val)
				}
			}
		}
	}
	// B0310: Claim dup'd inner string for Optional[string] field access.
	if c.optionalStringDup != nil {
		c.claimStringTemp(c.optionalStringDup)
		c.optionalStringDup = nil
	}
	// T0366: Claim dup'd inner container (Vector/Channel/Arc/Weak) for
	// Optional[container] field access. The dup is the value the new variable
	// owns; without claiming it would be freed at stmt end while the new
	// variable still references it.
	if c.optionalContainerDup != nil {
		c.claimStringTemp(c.optionalContainerDup)
		c.optionalContainerDup = nil
	}

	// Wrap value in Optional if declared type is Optional and expr differs in shape.
	// Using Identical (not "is exprOpt?") correctly handles T?? = T? — both are
	// Optional but at different depths, so a wrap is still needed.
	willWrap := false // T0585: track whether an Optional-wrap is materialized.
	if declType != nil {
		if _, isOpt := declType.(*types.Optional); isOpt {
			// Substitute exprType under typeSubst so generic body bodies (where the
			// AST records `T?` and the substitution maps T → some Optional) compare
			// against the resolved declType correctly.
			cmpExprType := exprType
			if c.typeSubst != nil && cmpExprType != nil {
				cmpExprType = types.Substitute(cmpExprType, c.typeSubst)
			}
			// T0856/T1087: A borrowed optional (`T?&`/`T?~`, e.g. `Ref[T?]`/
			// `Mutex[T?].borrow` with a value/Copy payload) auto-copies to a
			// bare optional value at the borrow site — genArcBorrow/
			// genMutexGuardBorrow load and return the full {i1,T} struct. The
			// recorded exprType is still the ref-to-optional, so strip the ref
			// before the wrap comparison; otherwise the already-optional value
			// is spuriously re-wrapped (insertvalue elem-type-mismatch panic).
			cmpExprType = unwrapRefsType(cmpExprType)
			if _, isNone := cmpExprType.(*types.Named); isNone && cmpExprType == types.TypNone {
				// NoneLit already handled via targetType
			} else if !types.Identical(cmpExprType, declType) {
				// T1298: box/view-coerce the RHS into the Optional's ELEMENT type
				// BEFORE wrapping, so a concrete → structural-interface or child →
				// parent RHS (e.g. `Sink? s = Counter(...)`) becomes a proper view
				// before it is insertvalue'd into the {i8*, i8*} optional payload.
				// The trailing coerceToView below runs against the Optional target
				// and is a no-op.
				val = c.coerceToOptionalElem(val, cmpExprType, declType)
				val = c.wrapOptional(val, lt.(*irtypes.StructType))
				willWrap = true
			}
		}
	}

	// Coerce value struct vtable when crossing type boundaries (e.g. Dog → Animal)
	coerceTarget := declType
	if coerceTarget == nil {
		// Resolve declared type from the AST TypeRef (handles non-optional typed decls)
		coerceTarget = c.resolveTypeRefToType(s.Type)
	}
	if coerceTarget == nil {
		// Final fallback: look up the declared type from sema scopes
		coerceTarget = c.lookupVarType(s.Name)
	}
	if coerceTarget != nil {
		val = c.coerceToView(val, exprType, coerceTarget)
	}

	// Clear drop flag on RHS if it's a variable being moved into this declaration.
	// Skip when LHS is a structural interface — the view borrows the original
	// value, so the original must retain its drop flag for cleanup. T0082.
	isStructuralTarget := false
	if coerceTarget != nil {
		if cn := extractNamed(coerceTarget); isStructuralView(cn) {
			isStructuralTarget = true
		}
	}
	// T0106: For droppable containers/strings, save the RHS's old flag value before clearing.
	// T0585: For an Optional-wrap from an IdentExpr RHS, also save the flag so we can mirror
	// the RHS's ownership state into the LHS drop flag after maybeRegisterDrop.
	var rhsOldDropFlag value.Value
	var rhsFlagForWrap value.Value
	if !isStructuralTarget {
		if ident, ok := s.Value.(*ast.IdentExpr); ok {
			dropType := declType
			if dropType == nil {
				dropType = exprType
			}
			if isDroppableContainerOrString(dropType) {
				if flag, ok := c.dropFlags[ident.Name]; ok {
					rhsOldDropFlag = c.block.NewLoad(irtypes.I1, flag)
				}
			}
			if willWrap {
				if flag, ok := c.dropFlags[ident.Name]; ok {
					rhsFlagForWrap = c.block.NewLoad(irtypes.I1, flag)
				}
			}
			c.clearDropFlag(ident.Name)
		}
	}
	// B0250: If RHS is a method call returning the same heap instance as its receiver,
	// clear the receiver's drop flag to prevent double-free. This handles the pattern
	// `w2 := w.self()` where self() does `return this` from a borrowing method —
	// both w and w2 would otherwise try to free the same heap allocation.
	// T0347: walk through chained method calls so `r := c.iter().iter()` also clears
	// `c`'s drop flag; for `r := this.method()` (chain rooted at `this`), defer until
	// after maybeRegisterDrop so we can clear the new binding's drop flag instead.
	// T0882: operator dispatch (m := a + b, m := -d) has RHS BinaryExpr/UnaryExpr,
	// not CallExpr, so use operatorReceiverOrigin to reach the same alias-clear when
	// a user-defined operator body is `return this`.
	if !isStructuralTarget {
		var aliasOrigin ast.Expr
		if call, ok := s.Value.(*ast.CallExpr); ok {
			aliasOrigin = chainOriginExpr(call)
		} else {
			aliasOrigin = operatorReceiverOrigin(s.Value)
		}
		switch origin := aliasOrigin.(type) {
		case *ast.IdentExpr:
			c.maybeClearReceiverDropFlag(val, origin.Name, resolvedExprType)
		case *ast.ThisExpr:
			c.pendingThisAliasClear = &thisAliasClearReq{val: val, retType: resolvedExprType}
		}
	}

	// T1209: capture a mixed owned/borrowed match/if result's live per-path flag
	// before the claims below neutralize it; applied after maybeRegisterDrop to
	// replace the unconditional owning drop. Skipped for an elvis RHS (handled by
	// consumeElvisBoundDropFlag), for an Optional-wrap (maybeRegisterOptionalDrop, a
	// different mechanism — out of scope, T1210), and for any non-match/if perPathFlag
	// temp — e.g. the member optional handler `owner.field? _ {}` (T1162), whose
	// binding MOVES the value out of the field (applying the stale flag would leak).
	// See isMixedMergeBindingRHS.
	var mergeBoundFlag value.Value
	if c.elvisBoundDropFlag == nil && !willWrap && isMixedMergeBindingRHS(s.Value) {
		mergeBoundFlag = c.captureLiveTempFlag(val)
	}
	// T0073: Claim string temp — ownership transferred to this variable.
	// B0204: Use resolvedExprType (substituted) so that generic T=string is handled.
	if resolvedExprType != nil && extractNamed(resolvedExprType) == types.TypString {
		c.claimStringTemp(val)
	}
	// B0219: Claim vector/channel/arc/weak temp — ownership transferred to this variable.
	// T0555: Mutex/Task also need claiming now that their constructor temps are tracked.
	// T0561: MutexGuard temps from m.lock() also need claiming.
	if resolvedExprType != nil && (types.IsVector(resolvedExprType) || types.IsChannel(resolvedExprType) ||
		types.IsArc(resolvedExprType) || types.IsWeak(resolvedExprType) ||
		types.IsMutex(resolvedExprType) || types.IsAnyTask(resolvedExprType) ||
		types.IsMutexGuard(resolvedExprType)) {
		c.claimStringTemp(val)
	}
	// T1181: Claim fixed-array temp — ownership transferred to this variable's
	// bindingDropArray; clearing the stmt-temp flag avoids a double-free.
	if resolvedExprType != nil {
		if _, ok := resolvedExprType.(*types.Array); ok {
			c.claimStringTemp(val)
		}
	}
	// T0088: Claim heap temp — ownership transferred to this variable.
	c.claimHeapTemp(val)
	// T1276: capture whether a fresh owned box was claimed (value/primitive coerced
	// to a structural interface) so maybeRegisterStructuralFree registers the free
	// even when the RHS shape (e.g. an identifier copy) isn't a call.
	claimedOwnedBox := c.lastClaimedDropFunc != nil
	// B0267/T1323: Clear enum temps only when the RHS itself MOVES the enum out
	// into this binding (an enum constructor, or a match/if producing the enum) —
	// NOT when it is a call whose RESULT happens to be an enum (`q = f(E.V(...))`),
	// where the by-value enum-ctor ARG temp stays owned here and must fall through
	// to the statement-boundary drain (a type-based `extractEnum` check would misfire
	// on the call result and orphan the arg's payload). See enumCtorTempMovesOut.
	// T1340: floor-bounded clear (see clearMovedOutEnumCtorTemps).
	if len(c.enumCtorTemps) > c.blockTempFloorEnum && c.enumCtorTempMovesOut(s.Value) {
		c.clearMovedOutEnumCtorTemps()
	}
	// B0222: When storing a structural interface (e.g., Iterator) in a variable,
	// promote remaining heapTemps to scope bindings. Intermediate iterators in
	// generic combinator chains must survive until scope exit, not be freed at
	// statement end. Uses the resolved type (substituted for generics).
	resolvedDeclType := declType
	if resolvedDeclType == nil {
		resolvedDeclType = exprType
	}
	if c.typeSubst != nil && resolvedDeclType != nil {
		resolvedDeclType = types.Substitute(resolvedDeclType, c.typeSubst)
	}
	if len(c.heapTemps) > 0 {
		if n := extractNamed(resolvedDeclType); isStructuralView(n) {
			c.promoteHeapTempsToScope()
		}
	}
	// T0100: Claim env temp — the variable's scope binding handles env free.
	c.claimEnvTemp(val)

	// T1190: a none-typed RHS bound to a typed Optional local must be coerced to
	// the concrete Optional[T] zero before the store (avoids an i1→{i1,T} mismatch).
	val = c.coerceNoneToOptional(val, exprType, declType)

	c.block.NewStore(val, alloca)
	c.locals[s.Name] = alloca
	// Use declared type if available, otherwise fall back to expression type
	dropType := declType
	if dropType == nil {
		dropType = exprType
	}
	// B0204: In monomorphized generic code, dropType may be a TypeParam (e.g., T).
	// Substitute to the concrete type so maybeRegisterDrop can register the correct
	// drop binding (e.g., string drop when T=string).
	if c.typeSubst != nil && dropType != nil {
		dropType = types.Substitute(dropType, c.typeSubst)
	}
	c.maybeRegisterDrop(s.Name, alloca, dropType)
	// T0940/T0981: a bound elvis owns its buffer only on paths where the selected
	// operand was orphaned/neutralized — replace the unconditional owning drop with
	// the per-path flag computed in genElvis.
	c.consumeElvisBoundDropFlag(s.Name)
	c.applyBoundMergeFlag(s.Name, mergeBoundFlag) // T1209
	// T0347: Drain pending this-alias clear request set when RHS is a chain rooted
	// at `this`. maybeRegisterDrop has now stored i1 1 into the binding's drop flag;
	// emit a runtime alias check that clears it back to false when the result really
	// aliases `this`, leaving the caller's drop flag intact.
	if req := c.pendingThisAliasClear; req != nil {
		c.pendingThisAliasClear = nil
		if flag, ok := c.dropFlags[s.Name]; ok {
			c.maybeClearBindingDropFlagOnThisAlias(req.val, flag, req.retType)
		}
	}
	// T0111: Register optional drop for explicitly declared optional locals (string? s = ...).
	if opt, ok := dropType.(*types.Optional); ok {
		c.maybeRegisterOptionalDrop(s.Name, alloca, opt)
	}
	// T1210: override the wrapped Optional's unconditional inner drop with the merge
	// temp's per-path ownership flag (0 on the borrowed arm's path, 1 on the owned
	// arm's path). No-op when the RHS was not a mixed owned/borrowed merge
	// (mergeWrapFlag nil).
	c.applyBoundMergeFlag(s.Name, mergeWrapFlag)
	// T0111: When RHS is opt!, neutralize the source optional (set present=false)
	// so its drop doesn't double-free the inner value now owned by this variable.
	c.neutralizeForceUnwrapSource(s.Value)
	// T0127: Register bindingFree for structural interface variables owning a heap allocation.
	c.maybeRegisterStructuralFree(s.Name, alloca, dropType, s.Value, claimedOwnedBox)
	// T1304: `r := pass_through(s)` where the callee returns an owned structural
	// param by value aliases the caller's still-owned arg box; clear r's structural
	// free flag on a runtime alias match so s stays the sole owner.
	if isStructuralTarget {
		if flag, ok := c.dropFlags[s.Name]; ok {
			c.maybeClearStructuralBindingAliasArg(val, s.Value, flag)
		}
	}
	// Clear drop flag when RHS is a borrow (container element, field access).
	// T0095: Skip for string MemberExpr on droppable types — genFieldAccess
	// dups the string, so the variable owns the copy (not a borrow).
	// T0137: Skip for getter calls (IdentExpr not in locals, or module.getter MemberExpr) —
	// getters return owned values, not borrows.
	// T0501: Also skip for local.getter / this.getter MemberExprs — getters on
	// locals return owned values whose tracking has already been claimed into
	// the LHS by claimStringTemp; clearing the drop flag here would orphan the
	// allocation.
	if isDroppableContainerOrString(dropType) && isStringBorrowExpr(s.Value) {
		isGetterCall := false
		if ident, ok := s.Value.(*ast.IdentExpr); ok {
			if _, isLocal := c.locals[ident.Name]; !isLocal {
				isGetterCall = true
			}
		}
		if member, ok := s.Value.(*ast.MemberExpr); ok {
			if ident, ok := member.Target.(*ast.IdentExpr); ok {
				if c.resolveModuleName(ident) != "" {
					isGetterCall = true
				}
			}
		}
		if !isGetterCall && c.isGetterCallExpr(s.Value) {
			isGetterCall = true
		}
		// T0647: user-defined non-native `[]` returns an owned temp (claimed into
		// the LHS), not a borrow — keep the LHS drop flag like a method call.
		if !isGetterCall && c.isUserIndexExpr(s.Value) {
			isGetterCall = true
		}
		if !isGetterCall && !c.isStringFieldDup(s.Value, dropType) {
			if rhsOldDropFlag != nil {
				// T0106: Propagate RHS's ownership state at runtime.
				if lhsFlag, ok := c.dropFlags[s.Name]; ok {
					c.block.NewStore(rhsOldDropFlag, lhsFlag)
				}
			} else {
				c.clearDropFlag(s.Name)
			}
		}
	}
	// T0585: For an Optional-wrap from an IdentExpr RHS, the wrapped local aliases
	// the RHS's inner heap value. Mirror the RHS's ownership state into the LHS
	// drop flag — `1` when RHS owned (transferring ownership; the RHS flag was
	// cleared above), `0` when RHS was borrowed (no flag existed). Without this,
	// scope-exit drop of the wrapped local would double-free the heap value still
	// owned by the original (borrowed) RHS.
	if willWrap {
		if _, isIdent := s.Value.(*ast.IdentExpr); isIdent {
			if lhsFlag, ok := c.dropFlags[s.Name]; ok {
				var newVal value.Value
				if rhsFlagForWrap != nil {
					newVal = rhsFlagForWrap
				} else {
					newVal = constant.NewInt(irtypes.I1, 0)
				}
				c.block.NewStore(newVal, lhsFlag)
			}
		}
	}
	// T0367/T0381: when the RHS expression's static type is `T&`/`T~`, it
	// is a non-owning reference. Clear the drop flag so scope cleanup
	// doesn't double-free with the owner's drop.
	if c.isBorrowedExpr(s.Value) {
		c.clearDropFlag(s.Name)
		c.markBorrowOptionalLocal(s.Name, dropType)
	}
	// T0747: a user-type RTTI cast of a borrow (`d := x as!/as T`) is a
	// non-consuming view — the subject keeps ownership. Clear the LHS drop flag
	// so the cast local doesn't double-free the aliased instance at scope exit.
	if c.isRttiCastBorrow(s.Value) {
		c.clearDropFlag(s.Name)
		c.markBorrowOptionalLocal(s.Name, dropType)
	}
	// T1230: `Fn fn = m[k]!` reads a closure-nesting struct out of an aliasing
	// container — a borrow (env can't be deep-cloned; dup suppressed above). Clear
	// the local's owning drop flag so scope exit doesn't free the container's shared
	// instance/env. Mirrors the genInferredVarDecl clear.
	if c.isClosureAggregateBorrow(s.Value) {
		c.clearDropFlag(s.Name)
	}
	// T1259/T1264: `hs := gs` where `gs` is a match-borrowed alias of a closure
	// nested in an enum variant (direct closure field or value-copying container of
	// closures). The new local borrows — see markMatchBorrowedRebind.
	c.markMatchBorrowedRebind(s.Name, s.Value, dropType)
	c.maybeRegisterEnvFree(s.Name, alloca, dropType, s.Value)
}

func (c *Compiler) genInferredVarDecl(s *ast.InferredVarDecl) {
	typ := c.info.Types[s.Value]
	if c.typeSubst != nil {
		typ = types.Substitute(typ, c.typeSubst)
	}
	lt := c.resolveType(typ)
	alloca := c.createEntryAlloca(lt)
	alloca.SetName(c.uniqueLocalName(s.Name))
	// T0095/B0179/B0219/B0310/T0487: Signal genFieldAccess to dup string,
	// Vector|Channel|Arc|Weak, and Optional[...] fields from droppable types
	// so this binding owns an independent copy. Skip for borrow types
	// (B0179) — borrows don't own the value.
	c.setDupFlagsForFieldAccess(typ)
	// T0370: Set dup flag for droppable tuple types so genVectorIndex deep-clones
	// tuple elements on read. Without this, `t := v[0]` aliases v's element data
	// and bindingDropTuple would double-free with v's element walk.
	if _, isTup := typ.(*types.Tuple); isTup && c.tupleNeedsDrop(typ) {
		c.dupTupleFieldAccess = true
	}
	// T0397: Same flag for inferred `opt := m[k]` — Optional[Tuple] LHS where
	// the inner tuple has droppable fields aliased into the container's bucket.
	// (Not borrow-gated — checks Optional[Tuple] type shape. Remains active post-T0438.)
	if opt, ok := typ.(*types.Optional); ok {
		elem := opt.Elem()
		if c.typeSubst != nil {
			elem = types.Substitute(elem, c.typeSubst)
		}
		if _, isTup := elem.(*types.Tuple); isTup && c.tupleNeedsDrop(elem) {
			c.dupTupleFieldAccess = true
		}
	}
	// T0398: Set dup flag for heap user types so genVectorIndex deep-clones the
	// element on read. Without this, `b := v[0]` aliases v's element instance
	// pointer — b's drop binding and v's element walk would double-free. Only
	// fires when the RHS is a direct vector-index expression: chains like
	// `b := v[0].method()` are excluded because the cloned receiver would not
	// be consumed (method takes a borrow), leaking the clone.
	// (Not borrow-gated — checks AST shape (IndexExpr) and element type. Remains active post-T0438.)
	//
	// T0440: Also set the flag for `b := m[k]!` — the RHS unwraps an
	// Optional[heap-user-type] from a Map index. The unwrap consumes the
	// Optional and returns V; without the dup, b would alias the bucket.
	//
	// T0903/T0898: Also fire for no-drop heap user types (`b := vec[i]` for a
	// plain `type` with no drop/synth-drop). isDroppableHeapUserType excludes these
	// for the T0440 Map-clone gate, but the vector element read still aliases the
	// source instance — b's bindingFree (pal_free) and the vector's element
	// scope-exit free would double-free. Matches the typed-var-decl (T0590) and
	// assignment (genAssignStmt) paths, which already admit both predicates, and
	// the genVectorIndex/genArrayIndex no-drop dup-on-read branches. Also required
	// so the drop-on-overwrite added to genVectorIndexAssign doesn't free a slot
	// still aliased by a local on the swap idiom
	// (`t := v[lo]; v[lo] = v[mid]; v[mid] = t`).
	if isDroppableHeapUserType(typ) || isHeapUserNoDropPalFree(typ) {
		if _, isIdx := s.Value.(*ast.IndexExpr); isIdx {
			c.dupHeapUserFieldAccess = true
		} else if unwrap, isUnwrap := s.Value.(*ast.OptionalUnwrapExpr); isUnwrap {
			if _, isInnerIdx := unwrap.Expr.(*ast.IndexExpr); isInnerIdx {
				c.dupHeapUserFieldAccess = true
			}
		}
	}
	// T1129: bare droppable-enum element read from a native Vector/Array index
	// (`got := v[i]`) must be deep-cloned so the binding owns independent variant
	// data — else got's drop and the container's element walk double-free (fatal
	// for recursive enums). Only the bare IndexExpr form needs this: the Map form
	// (`got := m[k]!`) is owned by the `[]` method body's match-dup. genVectorIndex/
	// genArrayIndex consume the flag via cloneResolvedValue.
	if c.enumElemNeedsDupOnRead(typ) {
		if _, isIdx := s.Value.(*ast.IndexExpr); isIdx {
			c.dupHeapUserFieldAccess = true
		}
	}
	// T1130: Map/Set element read-back from a native Vector/Array index
	// (`got := v[i]`) must be deep-cloned so the binding owns an independent
	// Map/Set — else got's drop and the container's element walk double-free.
	// isMapOrSetType excludes the bare `m[k]` (Map container) form: that yields an
	// Optional, handled by the Optional branch below / the Map.[] body's own dup.
	if isMapOrSetType(typ) {
		if _, isIdx := s.Value.(*ast.IndexExpr); isIdx {
			c.dupHeapUserFieldAccess = true
		}
	}
	// T1287: bare structural-interface element read from a native Vector index
	// (`x := v[i]`) must deep-clone the {vtable, instance} box so the binding owns an
	// independent box — else x aliases the vector's element and dropping the old box on
	// overwrite (genVectorIndexAssign, T1287) or the vector's element walk (T1284)
	// leaves x dangling (UAF) / double-frees. genVectorIndex consumes the flag via
	// cloneStructuralView. Only the bare IndexExpr form needs this (the Optional
	// element form is handled by the branch below, T1291).
	if isNonValueStructuralType(typ) {
		if _, isIdx := s.Value.(*ast.IndexExpr); isIdx {
			c.dupHeapUserFieldAccess = true
		}
	}
	// T0440: Same flag for inferred `b := m[k]` — Optional[heap-user-type] LHS
	// where the inner value aliases the container's bucket. Set the flag so
	// genMethodIndex deep-clones via cloneHeapElement.
	if opt, ok := typ.(*types.Optional); ok {
		elem := opt.Elem()
		if c.typeSubst != nil {
			elem = types.Substitute(elem, c.typeSubst)
		}
		// T0903: include no-drop heap user inner (analog of the bare-type branch).
		// T1291: include a non-value structural inner (`x := v[i]` where the element
		// is `Showable?`) — its element drop is now active, so the aliased read must
		// clone the box else x's optional drop and the vector's element walk double-free.
		if isDroppableHeapUserType(elem) || isHeapUserNoDropPalFree(elem) || isNonValueStructuralType(elem) {
			c.dupHeapUserFieldAccess = true
		}
	}
	// T1230: a closure-nesting aggregate read from an aliasing container (`fn := m[k]!`
	// on a `Fn{()->int}` value) is a borrow — the env can't be deep-cloned. Suppress
	// the dup-on-read set above so the local aliases the container's instance with the
	// env intact; the owning drop is cleared after maybeRegisterDrop below.
	if c.isClosureAggregateBorrow(s.Value) {
		c.dupHeapUserFieldAccess = false
	}
	// T0952: `m := a ?: b` — when the RHS (peeling parens) is an inline elvis, signal
	// genElvis so it neutralizes the none-path default's owner on the none block
	// (path-conditionally). The bound variable claims the elvis result temp and takes
	// an unconditional owning drop, so the aliased default must not drop a second time.
	prevElvisBound := c.elvisResultBound
	if be, ok := unwrapDestructureParens(s.Value).(*ast.BinaryExpr); ok && be.Op == ast.BinElvis {
		c.elvisResultBound = true
	}
	val := c.genExpr(s.Value)
	c.elvisResultBound = prevElvisBound
	c.dupStringFieldAccess = false
	c.dupContainerFieldAccess = false
	c.dupTupleFieldAccess = false
	c.dupHeapUserFieldAccess = false

	// T0685: Defensive — if the RHS produced no value (e.g., a type expression
	// like bare `T[]` slipped past sema), bail out with a diagnostic panic
	// rather than nil-storing through llir. Sema should have already rejected
	// these inputs, but this guard prevents future sema gaps from showing up
	// as opaque SIGSEGVs deep in github.com/llir/llvm.
	if val == nil {
		panic(fmt.Sprintf("codegen: nil value for inferred var decl %q at %v (likely a sema gap — type expression used in value position)", s.Name, s.Pos()))
	}

	// Auto-propagate failable call in assignment: check tag, propagate error, extract ok value.
	if c.info.AutoPropagateExprs[s.Value] {
		val = c.genAutoPropagateValue(val)
	}

	// T1179: deep-clone a whole match-borrowed Array/Optional heap-user payload
	// bound by this var-decl so the new local owns independent data (see
	// cloneBorrowedWholePayloadVarDecl). `typ` is the substituted RHS type.
	val = c.cloneBorrowedWholePayloadVarDecl(val, s.Value, typ)

	// Clear drop flag on RHS if it's a variable being moved into this declaration.
	// Without this, `b := a` would leave both a and b with active drop flags → double-free.
	// T0106: For droppable containers/strings, save the RHS's old flag value before clearing.
	// This enables runtime ownership propagation: if RHS owned it (flag=true), LHS takes
	// ownership; if RHS borrowed it (flag=false), LHS also borrows.
	var rhsOldDropFlag value.Value
	if ident, ok := s.Value.(*ast.IdentExpr); ok {
		if isDroppableContainerOrString(typ) {
			if flag, ok := c.dropFlags[ident.Name]; ok {
				rhsOldDropFlag = c.block.NewLoad(irtypes.I1, flag)
			}
		}
		c.clearDropFlag(ident.Name)
	}
	// B0250: If RHS is a method call returning the same heap instance as its receiver,
	// clear the receiver's drop flag to prevent double-free.
	// T0347: walk through chained method calls; for chains rooted at `this`, defer
	// the alias-clear so it targets the new binding's drop flag (set by maybeRegisterDrop).
	// T0882: operator dispatch (m := a + b, m := -d) has RHS BinaryExpr/UnaryExpr,
	// not CallExpr, so use operatorReceiverOrigin to reach the same alias-clear when
	// a user-defined operator body is `return this`.
	//
	// Skip when the inferred type is a structural interface (e.g., a structural
	// default operator `-() Negatable { return this; }` resolving to `Negatable`):
	// the result binding never takes an owning drop (maybeRegisterDrop skips
	// structural types; maybeRegisterStructuralFree is itself alias-aware), so
	// clearing the operand's drop flag would leave the shared instance unfreed.
	// This mirrors the typed path's isStructuralTarget guard. T0882.
	isStructuralTarget := false
	if n := extractNamed(typ); isStructuralView(n) {
		isStructuralTarget = true
	}
	if !isStructuralTarget {
		var aliasOrigin ast.Expr
		if call, ok := s.Value.(*ast.CallExpr); ok {
			aliasOrigin = chainOriginExpr(call)
		} else {
			aliasOrigin = operatorReceiverOrigin(s.Value)
		}
		switch origin := aliasOrigin.(type) {
		case *ast.IdentExpr:
			c.maybeClearReceiverDropFlag(val, origin.Name, typ)
		case *ast.ThisExpr:
			c.pendingThisAliasClear = &thisAliasClearReq{val: val, retType: typ}
		}
	}

	// T1209: a mixed owned/borrowed match/if result bound to this local owns its
	// value only on the paths that selected an owned arm. Capture the merge temp's
	// live per-path flag before the claims below neutralize it; applied after
	// maybeRegisterDrop to replace the unconditional owning drop. Skipped for an
	// elvis RHS (handled by consumeElvisBoundDropFlag) and for any non-match/if
	// perPathFlag temp — e.g. the member optional handler `owner.field? _ {}` (T1162),
	// whose binding MOVES the present value out of the field so the binding owns it
	// unconditionally (applying the stale present=borrowed flag would leak). See
	// isMixedMergeBindingRHS.
	var mergeBoundFlag value.Value
	if c.elvisBoundDropFlag == nil && isMixedMergeBindingRHS(s.Value) {
		mergeBoundFlag = c.captureLiveTempFlag(val)
	}
	// T0073: Claim string temp — ownership transferred to this variable.
	if extractNamed(typ) == types.TypString {
		c.claimStringTemp(val)
	}
	// B0310: Claim dup'd inner string for Optional[string] field access.
	if c.optionalStringDup != nil {
		c.claimStringTemp(c.optionalStringDup)
		c.optionalStringDup = nil
	}
	// T0366: Claim dup'd inner container for Optional[Vector|Channel|Arc|Weak] field access.
	if c.optionalContainerDup != nil {
		c.claimStringTemp(c.optionalContainerDup)
		c.optionalContainerDup = nil
	}
	// B0219: Claim vector/channel/arc/weak temp — ownership transferred to this variable.
	// T0555: Mutex/Task also need claiming now that their constructor temps are tracked.
	// T0561: MutexGuard temps from m.lock() also need claiming.
	if types.IsVector(typ) || types.IsChannel(typ) || types.IsArc(typ) || types.IsWeak(typ) ||
		types.IsMutex(typ) || types.IsAnyTask(typ) || types.IsMutexGuard(typ) {
		c.claimStringTemp(val)
	}
	// T1181: Claim fixed-array temp — ownership transferred to this variable's
	// bindingDropArray; clearing the stmt-temp flag avoids a double-free.
	if typ != nil {
		if _, ok := typ.(*types.Array); ok {
			c.claimStringTemp(val)
		}
	}
	// B0175: Claim heap temp — ownership transferred to this variable.
	// Without this, iterator chain results (e.g., c.take(3)) assigned via
	// auto-typed declarations are freed at statement end, causing use-after-free.
	c.claimHeapTemp(val)
	// T1276: capture whether a fresh owned box was claimed (see genVarDecl).
	claimedOwnedBox := c.lastClaimedDropFunc != nil
	// B0267/T1323: Clear enum temps only when the RHS itself MOVES the enum out
	// into this binding (an enum constructor, or a match/if producing the enum) —
	// NOT when it is a call whose RESULT happens to be an enum (`q := f(E.V(...))`),
	// where the by-value enum-ctor ARG temp stays owned here and must fall through
	// to the statement-boundary drain (a type-based `extractEnum` check would misfire
	// on the call result and orphan the arg's payload). See enumCtorTempMovesOut.
	// T1340: floor-bounded clear (see clearMovedOutEnumCtorTemps).
	if len(c.enumCtorTemps) > c.blockTempFloorEnum && c.enumCtorTempMovesOut(s.Value) {
		c.clearMovedOutEnumCtorTemps()
	}
	// B0222: When storing a structural interface (e.g., Iterator) in a variable,
	// promote remaining heapTemps to scope bindings so intermediate iterators in
	// generic combinator chains survive until scope exit.
	if len(c.heapTemps) > 0 {
		if n := extractNamed(typ); isStructuralView(n) {
			c.promoteHeapTempsToScope()
		}
	}
	// T0100: Claim env temp — the variable's scope binding handles env free.
	c.claimEnvTemp(val)

	c.block.NewStore(val, alloca)
	c.locals[s.Name] = alloca
	c.maybeRegisterDrop(s.Name, alloca, typ)
	// T0940/T0981: replace the unconditional owning drop with the bound elvis's
	// per-path flag (see consumeElvisBoundDropFlag / matching block in genVarDecl).
	c.consumeElvisBoundDropFlag(s.Name)
	c.applyBoundMergeFlag(s.Name, mergeBoundFlag) // T1209
	// T0347: Drain pending this-alias clear (see matching block in genVarDecl).
	if req := c.pendingThisAliasClear; req != nil {
		c.pendingThisAliasClear = nil
		if flag, ok := c.dropFlags[s.Name]; ok {
			c.maybeClearBindingDropFlagOnThisAlias(req.val, flag, req.retType)
		}
	}
	// B0226: Register optional drop for inferred optional locals (r := factory_returning_optional()).
	// Without this, optional values from inferred declarations leak their inner value at scope exit.
	if opt, ok := typ.(*types.Optional); ok {
		c.maybeRegisterOptionalDrop(s.Name, alloca, opt)
	}
	// T0111: When RHS is opt!, neutralize the source optional (set present=false)
	// so its drop doesn't double-free the inner value now owned by this variable.
	c.neutralizeForceUnwrapSource(s.Value)
	// T0127: Register bindingFree for structural interface variables owning a heap allocation.
	c.maybeRegisterStructuralFree(s.Name, alloca, typ, s.Value, claimedOwnedBox)
	// T1304: see genVarDecl — clear r's structural free flag when it aliases a
	// still-owned call argument (owned structural param returned by value).
	if isStructuralTarget {
		if flag, ok := c.dropFlags[s.Name]; ok {
			c.maybeClearStructuralBindingAliasArg(val, s.Value, flag)
		}
	}
	// Clear drop flag when RHS is a borrow (container element, field access).
	// The container/struct still owns the value — freeing it here would cause use-after-free.
	// T0095: Skip for string MemberExpr on droppable types — genFieldAccess
	// dups the string, so the variable owns the copy (not a borrow).
	// T0137: Skip for getter calls (IdentExpr not in locals, or module.getter MemberExpr) —
	// getters return owned values, not borrows.
	// T0501: Also skip for local.getter / this.getter MemberExprs — getters on
	// locals return owned values whose tracking has already been claimed into
	// the LHS by claimStringTemp; clearing the drop flag here would orphan the
	// allocation.
	if isDroppableContainerOrString(typ) && isStringBorrowExpr(s.Value) {
		isGetterCall := false
		if ident, ok := s.Value.(*ast.IdentExpr); ok {
			if _, isLocal := c.locals[ident.Name]; !isLocal {
				isGetterCall = true
			}
		}
		if member, ok := s.Value.(*ast.MemberExpr); ok {
			if ident, ok := member.Target.(*ast.IdentExpr); ok {
				if c.resolveModuleName(ident) != "" {
					isGetterCall = true
				}
			}
		}
		if !isGetterCall && c.isGetterCallExpr(s.Value) {
			isGetterCall = true
		}
		// T0647: user-defined non-native `[]` returns an owned temp (claimed into
		// the LHS), not a borrow — keep the LHS drop flag like a method call.
		if !isGetterCall && c.isUserIndexExpr(s.Value) {
			isGetterCall = true
		}
		if !isGetterCall && !c.isStringFieldDup(s.Value, typ) {
			if rhsOldDropFlag != nil {
				// T0106: Propagate RHS's ownership state at runtime.
				// If RHS owned the value (flag was true), LHS takes ownership.
				// If RHS borrowed it (flag was false), LHS also borrows.
				if lhsFlag, ok := c.dropFlags[s.Name]; ok {
					c.block.NewStore(rhsOldDropFlag, lhsFlag)
				}
			} else {
				c.clearDropFlag(s.Name)
			}
		}
	}
	// T0367/T0381: when the RHS expression's static type is `T&`/`T~`, it
	// is a non-owning reference. Clear the drop flag so scope cleanup
	// doesn't double-free with the owner's drop.
	if c.isBorrowedExpr(s.Value) {
		c.clearDropFlag(s.Name)
		c.markBorrowOptionalLocal(s.Name, typ)
	}
	// T0747: a user-type RTTI cast of a borrow (`d := x as!/as T`) is a
	// non-consuming view — the subject keeps ownership. Clear the LHS drop flag
	// so the cast local doesn't double-free the aliased instance at scope exit.
	if c.isRttiCastBorrow(s.Value) {
		c.clearDropFlag(s.Name)
		c.markBorrowOptionalLocal(s.Name, typ)
	}
	// T1230: `fn := m[k]!` on a `Fn{()->int}` value reads a closure-nesting struct
	// out of an aliasing container. The env can't be deep-cloned, so the read is a
	// borrow — the local aliases the container's stored instance (env intact, dup
	// suppressed above). Clear the local's owning drop flag so scope exit doesn't
	// free the shared instance/env out from under the container's own drop
	// (double-free / UAF). Ownership marks the local Borrowed so escapes are
	// rejected. Placed after maybeRegisterDrop so the drop flag exists to clear.
	if c.isClosureAggregateBorrow(s.Value) {
		c.clearDropFlag(s.Name)
	}
	// T1259/T1264: `hs := gs` where `gs` is a match-borrowed alias of a closure
	// nested in an enum variant (direct closure field or value-copying container of
	// closures). The new local borrows — see markMatchBorrowedRebind.
	c.markMatchBorrowedRebind(s.Name, s.Value, typ)
	c.maybeRegisterEnvFree(s.Name, alloca, typ, s.Value)
}

// unwrapDestructureParens peels any number of *ast.ParenExpr wrappers from a
// destructure source. T0570: the AST-shape dispatch in genDestructureVarDecl
// matches against *ast.IdentExpr / *ast.IndexExpr / *ast.MemberExpr; without
// peeling, paren-wrapped sources fall through to the default arm and
// destructured locals incorrectly get drop bindings → double-free at scope
// exit. (genExpr already sees through ParenExpr; this only fixes dispatch.)
func unwrapDestructureParens(e ast.Expr) ast.Expr {
	for {
		p, ok := e.(*ast.ParenExpr)
		if !ok {
			return e
		}
		e = p.Expr
	}
}

// isThisReceiver reports whether expr is a `this` reference, seeing through any
// number of parenthesization wrappers ((this), ((this)), ...). genExpr already
// evaluates ParenExpr transparently, so the receiver *value* is correct; this
// only fixes the AST-shape dispatch gates that decide how the receiver is passed
// (raw i8* for `this` vs. extractvalue from a value struct). Without the peel,
// `(this).method()` etc. fall through to the value-struct path and emit
// `extractvalue i8* ...`, which opt rejects. (T0613)
func isThisReceiver(expr ast.Expr) bool {
	_, ok := unwrapDestructureParens(expr).(*ast.ThisExpr)
	return ok
}

// genDestructureVarDecl handles tuple destructuring: (a, b) := expr
func (c *Compiler) genDestructureVarDecl(s *ast.DestructureVarDecl) {
	if c.info.FailableDestructures[s] {
		c.genFailableDestructure(s)
		return
	}
	// T0397: Direct destructure of `m[k]!` aliases the map's bucket data:
	// each destructured local would get its own drop binding, but the inner
	// string/vector pointers still belong to the map → double-free at scope
	// exit. Set dupTupleFieldAccess so genMethodIndex (reached through the
	// force-unwrap inside `s.Value`) deep-clones the tuple. Skip when the
	// source obviously aliases (IndexExpr/MemberExpr), where srcOwned will be
	// false and no drop bindings are registered anyway.
	// T0570: peel ParenExpr so `(b, n) := (arr[i]);` takes the same alias
	// path as `(b, n) := arr[i];` — otherwise we'd dup the tuple (default
	// arm) while the second switch correctly leaves srcOwned=false, leaking
	// the dup'd pieces.
	switch unwrapDestructureParens(s.Value).(type) {
	case *ast.IndexExpr, *ast.MemberExpr:
		// borrow path — no drop bindings registered → no double-free.
	default:
		valType := c.info.Types[s.Value]
		if c.typeSubst != nil {
			valType = types.Substitute(valType, c.typeSubst)
		}
		if _, isTup := valType.(*types.Tuple); isTup && c.tupleNeedsDrop(valType) {
			c.dupTupleFieldAccess = true
		}
	}
	tupleVal := c.genExpr(s.Value)
	c.dupTupleFieldAccess = false
	tupleType := c.info.Types[s.Value]
	if c.typeSubst != nil {
		tupleType = types.Substitute(tupleType, c.typeSubst)
	}
	tup, ok := tupleType.(*types.Tuple)
	if !ok {
		panic(fmt.Sprintf("codegen: destructure value type is %T, want *types.Tuple", tupleType))
	}
	// T0371: Determine whether the source tuple is owned (has its own drop
	// binding, or is a transient temp like a literal/call result). If the source
	// is a borrow (for-in loop variable, container index expression, member
	// access), the destructured fields are also borrows and must not get drop
	// bindings — otherwise they would double-free with the container's element
	// walk or the parent's drop.
	//
	// T0570: peel ParenExpr so `(b, n) := (h.pair);` takes the same borrow
	// path as `(b, n) := h.pair;`. Without peeling, srcOwned stayed true →
	// destructured locals got drop bindings → double-free at scope exit.
	unwrappedSrc := unwrapDestructureParens(s.Value)
	srcOwned := true
	switch src := unwrappedSrc.(type) {
	case *ast.IdentExpr:
		_, hasBinding := c.dropBindings[src.Name]
		srcOwned = hasBinding
	case *ast.IndexExpr, *ast.MemberExpr:
		// T1241: Owned-return shapes behind Index/Member syntax produce a FRESH
		// owned tuple whose heap fields (strings, vectors, closure envs) the
		// destructured locals must free — a getter returning a tuple by value
		// (isGetterCallExpr) and a user-defined non-native `[]` returning a tuple
		// (isUserIndexExpr). A native container/array index or plain struct-field
		// read instead aliases storage the container/parent owns (borrow →
		// srcOwned=false). Mirrors tupleArgIsCallerOwnedTemp on the arg path.
		srcOwned = c.tupleArgIsCallerOwnedTemp(src)
	}
	for i, name := range s.Names {
		elemPromiseType := tup.Elems()[i]
		if c.typeSubst != nil {
			elemPromiseType = types.Substitute(elemPromiseType, c.typeSubst)
		}
		elemType := c.resolveType(elemPromiseType)
		alloca := c.createEntryAlloca(elemType)
		// T0481: `_` slots still need an alloca + drop binding. The source's
		// drop flag is cleared below (transfer to LHS locals), so without a
		// drop binding under a synthetic key the discarded heap field would
		// be orphaned. Use a unique synthetic name so multiple `_` slots and
		// repeated destructures within a scope don't collide.
		bindKey := name
		if name == "_" {
			bindKey = c.uniqueLocalName("_destructure.discard")
			alloca.SetName(bindKey)
		} else {
			alloca.SetName(c.uniqueLocalName(name))
		}
		c.block.NewStore(c.block.NewExtractValue(tupleVal, uint64(i)), alloca)
		if name != "_" {
			c.locals[name] = alloca
			// T0672: Record the borrow status of each destructured local so a
			// downstream Optional unwrap (`if mm := m` / `while mm := m` / `m!`)
			// does not transfer ownership. When the source is a borrow (struct
			// field / container index — srcOwned=false), the local aliases heap
			// owned by the parent/container; without this marker
			// isOwnedOptionalExpr would treat `m` as owned and give the
			// unwrapped binding an owning drop binding → double-free with the
			// parent's drop (segfault for multi-word aggregates like
			// map/Vector). Mirrors the match-destructure marking at
			// expr.go:6618; the if/while-let propagation (stmt.go ~7751) carries
			// the mark to chained unwraps. Delete (not skip) for owned sources
			// so a re-destructure / shadow into the same name with an owned
			// source clears any stale borrow mark.
			if !srcOwned {
				if c.matchBorrowedIdents == nil {
					c.matchBorrowedIdents = make(map[string]bool)
				}
				c.matchBorrowedIdents[name] = true
			} else {
				delete(c.matchBorrowedIdents, name)
			}
		}
		// T0371: Register drop tracking so destructured locals own and free
		// their pieces. Skipped when the source is a borrow (ident without a
		// drop binding) — otherwise destructured locals would double-free with
		// the container's element walk (e.g., for tup in vec { (a, b) := tup }).
		if srcOwned {
			if _, isSig := elemPromiseType.(*types.Signature); isSig {
				// T1233: a closure destructured out of an owned tuple takes
				// ownership of its heap env, but maybeRegisterDrop no-ops on
				// *types.Signature (extractNamed returns nil). Register the
				// bindingFreeEnv directly. nil valueExpr => isClosureAggregateBorrow
				// won't suppress it; the source tuple's drop flag is cleared just
				// below so the env is owned exactly once.
				//
				// T1248: EXCEPTION for a tuple LITERAL source whose element reads
				// a closure out of an aliasing aggregate (`(m["k"]!, 7)` — a map
				// `[]` returns the stored fat pointer by value; closures aren't
				// Cloneable, so the element aliases the map's env). Pass that
				// element expression so isClosureAggregateBorrow suppresses the
				// owning env-free binding — otherwise this local frees the env at
				// scope exit while the map's drop frees it again (double free).
				var envValueExpr ast.Expr
				if tupLit, ok := unwrappedSrc.(*ast.TupleLit); ok && i < len(tupLit.Elements) {
					envValueExpr = tupLit.Elements[i]
				}
				c.maybeRegisterEnvFree(bindKey, alloca, elemPromiseType, envValueExpr)
			} else {
				c.maybeRegisterDrop(bindKey, alloca, elemPromiseType)
			}
		}
	}
	// T0371: Source tuple transferred field ownership to the destructured
	// locals. Clear its drop flag (ident case) so its scope-exit tuple-walk
	// doesn't double-free those pieces. For non-ident sources (literal,
	// function-call result), genTupleLit's per-element claims plus the
	// per-name drops registered above cover ownership.
	// T0570: use the paren-peeled expression so `(ident)` still clears the
	// drop flag on the underlying variable.
	if srcOwned {
		if ident, ok := unwrappedSrc.(*ast.IdentExpr); ok {
			c.clearDropFlag(ident.Name)
		}
	}
	// T0522: When RHS is `opt!` (force-unwrap of an Optional containing a tuple
	// with droppable fields), neutralize the source Optional's present flag so
	// its scope-exit optdrop doesn't free the inner values now owned by the
	// destructured locals. Mirrors genTypedVarDecl/genInferredVarDecl.
	c.neutralizeForceUnwrapSource(s.Value)
}

// genFailableDestructure handles (val, err) := failableCall()
// Extracts the value and converts the error into an error? optional.
func (c *Compiler) genFailableDestructure(s *ast.DestructureVarDecl) {
	result := c.genExpr(s.Value)
	resultType := result.Type().(*irtypes.StructType)
	tag := c.block.NewExtractValue(result, 0) // i1: false=ok, true=error

	errOptType := irtypes.NewStruct(irtypes.I1, userValueType()) // error? = {i1, {i8*, i8*}}

	errBlock := c.newBlock("destruct.err")
	okBlock := c.newBlock("destruct.ok")
	mergeBlock := c.newBlock("destruct.merge")
	c.block.NewCondBr(tag, errBlock, okBlock)

	// --- Error path ---
	c.block = errBlock
	errPtr := c.block.NewExtractValue(result, resultErrIdx(resultType))
	// Reconstruct error value struct {vtable_ptr, instance_ptr}
	variantPtr := c.loadVariantPtr(errPtr)
	typeinfoStruct := irtypes.NewStruct(irtypes.I8Ptr)
	typeinfoPtr := c.block.NewBitCast(variantPtr, irtypes.NewPointer(typeinfoStruct))
	vtableFieldPtr := c.block.NewGetElementPtr(typeinfoStruct, typeinfoPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
	vtablePtr := c.block.NewLoad(irtypes.I8Ptr, vtableFieldPtr)
	var errValStruct value.Value = constant.NewZeroInitializer(userValueType())
	errValStruct = c.block.NewInsertValue(errValStruct, vtablePtr, 0)
	errValStruct = c.block.NewInsertValue(errValStruct, errPtr, 1)
	// Wrap as present optional: {true, errValStruct}
	var errOpt value.Value = constant.NewZeroInitializer(errOptType)
	errOpt = c.block.NewInsertValue(errOpt, constant.True, 0)
	errOpt = c.block.NewInsertValue(errOpt, errValStruct, 1)
	// Value on error path: zero-initialized
	valType := c.info.Types[s.Value]
	if c.typeSubst != nil {
		valType = types.Substitute(valType, c.typeSubst)
	}
	llValType := c.resolveType(valType)
	var errPathVal value.Value
	if !isVoidResult(resultType) {
		errPathVal = constant.NewZeroInitializer(llValType)
	}
	errEnd := c.block
	c.block.NewBr(mergeBlock)

	// --- Ok path ---
	c.block = okBlock
	var okPathVal value.Value
	if !isVoidResult(resultType) {
		okPathVal = c.block.NewExtractValue(result, 1)
	}
	// Absent optional: {false, zeroinitializer}
	okOpt := constant.NewZeroInitializer(errOptType)
	okEnd := c.block
	c.block.NewBr(mergeBlock)

	// --- Merge ---
	c.block = mergeBlock

	// Emit all PHI nodes first (LLVM requires PHIs grouped at block top)
	var mergedVal value.Value
	if s.Names[0] != "_" && !isVoidResult(resultType) {
		mergedVal = mergeBlock.NewPhi(
			&ir.Incoming{X: errPathVal, Pred: errEnd},
			&ir.Incoming{X: okPathVal, Pred: okEnd},
		)
	}
	// B0193: Always create error PHI — even when discarded with _, the error
	// instance must be dropped to avoid leaking.
	mergedErr := mergeBlock.NewPhi(
		&ir.Incoming{X: errOpt, Pred: errEnd},
		&ir.Incoming{X: okOpt, Pred: okEnd},
	)

	// Now emit stores (after all PHIs)
	if mergedVal != nil {
		alloca := c.createEntryAlloca(llValType)
		alloca.SetName(c.uniqueLocalName(s.Names[0]))
		c.block.NewStore(mergedVal, alloca)
		c.locals[s.Names[0]] = alloca
		// B0263: Register drop/free for the value variable so heap-allocated
		// user types are freed at scope exit. Without this, the instance from
		// the ok path leaks (the error path contributes a null that the
		// null-check in emitFreeCall safely skips).
		c.maybeRegisterDrop(s.Names[0], alloca, valType)
	}

	// B0193: Always register the error optional for drop at scope exit.
	errVarName := s.Names[1]
	errAlloca := c.createEntryAlloca(errOptType)
	errAlloca.SetName(c.uniqueLocalName(errVarName))
	c.block.NewStore(mergedErr, errAlloca)

	dropFlag := c.createEntryAlloca(irtypes.I1)
	dropFlag.SetName(c.uniqueLocalName(errVarName + ".dropflag"))
	c.block.NewStore(constant.NewInt(irtypes.I1, 1), dropFlag)

	dropName := mangleMethodName("__mod_std_error", "drop", false)
	dropFunc := c.funcs[dropName]

	binding := scopeBinding{
		kind:     bindingDropOptional,
		alloca:   errAlloca,
		named:    types.TypError,
		valType:  types.TypError,
		dropFlag: dropFlag,
		dropFunc: dropFunc,
		varName:  errVarName,
	}
	c.scopeBindings = append(c.scopeBindings, binding)

	if errVarName != "_" {
		c.locals[errVarName] = errAlloca
		c.dropFlags[errVarName] = dropFlag
		c.dropBindings[errVarName] = binding
	}
}

// --- use binding ---

func (c *Compiler) genUseVarDecl(s *ast.UseVarDecl) {
	typ := c.info.Types[s.Value]
	if c.typeSubst != nil {
		typ = types.Substitute(typ, c.typeSubst)
	}
	lt := c.resolveType(typ)
	alloca := c.createEntryAlloca(lt)
	alloca.SetName(c.uniqueLocalName(s.Name))
	val := c.genExpr(s.Value)
	// GitHub #3: a failable initializer (bare call auto-propagating in a `!`
	// function) yields the failable-result aggregate; unwrap it to the ok value
	// before the store, exactly like the typed/inferred var-decl paths. Without
	// this the aggregate is stored into the unwrapped slot → llir type panic.
	if c.info.AutoPropagateExprs[s.Value] {
		val = c.genAutoPropagateValue(val)
	}
	c.block.NewStore(val, alloca)
	c.locals[s.Name] = alloca
	// B0233: Claim heap temp — ownership transferred to use binding.
	c.claimHeapTemp(val)
	// T0561: Claim stmt-temp (string/vector/Arc/Weak/Mutex/Task/MutexGuard)
	// so the per-statement drop doesn't double-cleanup against the close
	// binding. Without this, `use g := m.lock();` causes a double-free
	// because MutexGuard's stmt-temp drop AND the bindingClose's close()
	// both run on the same pointer.
	c.claimStringTemp(val)
	// Track for scope-exit close() insertion
	named := extractNamed(typ)
	var closeMethod *types.Method
	if named != nil {
		closeMethod = named.LookupMethod("close")
	}
	binding := scopeBinding{
		kind:            bindingClose,
		alloca:          alloca,
		named:           named,
		valType:         typ,
		closeIsFailable: closeMethod != nil && closeMethod.Sig().CanError(),
	}
	// Resolve close function for direct dispatch
	if named != nil && closeMethod != nil && (!c.needsVtable(named) || closeMethod.IsNative()) {
		ownerName := c.resolveMethodOwner(named, "close")
		mangledName := mangleMethodName(ownerName, "close", false)
		if fn, ok := c.funcs[mangledName]; ok {
			binding.closeFunc = fn
		}
	}
	c.scopeBindings = append(c.scopeBindings, binding)
}
