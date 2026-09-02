package codegen

import (
	"fmt"

	"github.com/llir/llvm/ir/constant"
	"github.com/llir/llvm/ir/enum"
	irtypes "github.com/llir/llvm/ir/types"
	"github.com/llir/llvm/ir/value"

	"github.com/promise-language/promise/compiler/internal/ast"
	"github.com/promise-language/promise/compiler/internal/sema"
	"github.com/promise-language/promise/compiler/internal/types"
)

// genBlock generates LLVM IR for a block of statements.
func (c *Compiler) genBlock(block *ast.Block) {
	if block == nil {
		return
	}
	savedScopeLen := len(c.scopeBindings)

	// T0088: Save heapTemps so statement-level cleanup inside this block
	// doesn't free temps from the enclosing scope (e.g., iterator instances
	// in a for-in loop that are still alive during the loop body).
	savedHeapTemps := c.heapTemps
	savedHeapTempMap := c.heapTempMap
	c.heapTemps = nil
	c.heapTempMap = make(map[value.Value]int)

	for _, stmt := range block.Stmts {
		if c.block == nil || c.block.Term != nil {
			break // block already terminated (return, break, etc.)
		}
		c.genStmt(stmt)
		// B0035: NLL early drops — drop variables whose last use was this statement.
		c.emitEarlyDrops(stmt)
	}
	// Emit cleanup calls for scope bindings added in this block (fall-through exit)
	if c.block != nil && c.block.Term == nil && len(c.scopeBindings) > savedScopeLen {
		cap := c.emitScopeCleanup(savedScopeLen, false)
		c.emitCloseErrCheck(cap, savedScopeLen)
	}
	c.scopeBindings = c.scopeBindings[:savedScopeLen]
	c.heapTemps = savedHeapTemps
	c.heapTempMap = savedHeapTempMap
}

// isDroppableContainerOrString returns true if the type is a string or vector
// (types that use the i8*-alloca drop mechanism in maybeRegisterDrop).
func isDroppableContainerOrString(typ types.Type) bool {
	named := extractNamed(typ)
	if named == types.TypString {
		return true
	}
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
	if _, ok := types.AsAnyTask(typ); ok || types.IsTaskLikeOrigin(named) {
		return true
	}
	return false
}

// argTypeIsDroppable returns true if a type would cause resource cleanup when
// dropped. Used to detect non-ident enum variant args that transfer ownership
// of droppable resources into the enum (B0286).
func argTypeIsDroppable(typ types.Type) bool {
	switch t := typ.(type) {
	case *types.Named:
		if t == types.TypString || t == types.TypVector || t == types.TypChannel || t == types.TypTask || t == types.TypFailableTask {
			return true
		}
		if t.HasDrop() || t.NeedsSynthDrop() {
			return true
		}
		// Heap user types need pal_free even without explicit drop.
		return !t.IsValueType() && !t.IsStructural() && !isPrimitiveScalar(t)
	case *types.Enum:
		return t.HasDrop() || t.NeedsSynthDrop()
	case *types.Instance:
		if n, ok := t.Origin().(*types.Named); ok {
			if n == types.TypVector || n == types.TypChannel || n == types.TypTask || n == types.TypFailableTask {
				return true
			}
			if n.HasDrop() || n.NeedsSynthDrop() {
				return true
			}
			return !n.IsValueType() && !n.IsStructural() && !isPrimitiveScalar(n)
		}
		if e, ok := t.Origin().(*types.Enum); ok {
			return e.HasDrop() || e.NeedsSynthDrop()
		}
	case *types.Optional:
		return argTypeIsDroppable(t.Elem())
	case *types.Signature:
		return true // closure env struct needs freeing
	}
	return false
}

// isOwnedOptionalExpr returns true if the expression produces a uniquely owned
// optional value — meaning the unwrapped inner value can safely be dropped by
// the if-let/while-let binding. Returns false for MemberExpr/IndexExpr on
// droppable types where the parent owns the field's inner value. B0215.
func (c *Compiler) isOwnedOptionalExpr(expr ast.Expr) bool {
	switch e := expr.(type) {
	case *ast.IdentExpr:
		// T0485: Match-bound Optional variant fields have no drop binding (the
		// variant data owns the inner). Without this check, the if-let unwrap
		// would take ownership and double-free with the synth enum drop's
		// Optional walk. matchBorrowedIdents tracks idents bound by match
		// destructure as borrows (no dup, no drop binding registered).
		if c.matchBorrowedIdents != nil && c.matchBorrowedIdents[e.Name] {
			return false
		}
		return true // local variable — ownership transferred via clearDropFlag
	case *ast.CallExpr:
		return true // function call returns owned value
	case *ast.ErrorPanicExpr:
		return true // failable panic (?!) of a call/expression returns owned value
	case *ast.OptionalUnwrapExpr:
		return true // optional unwrap (!) of an expression returns owned value
	case *ast.AutoCloneExpr:
		return true // T0605: synth deep-clone returns a fresh owned value
	case *ast.MemberExpr:
		// Field access on a droppable type — parent's drop handles the field.
		targetType := c.info.Types[e.Target]
		if c.typeSubst != nil {
			targetType = types.Substitute(targetType, c.typeSubst)
		}
		ownerNamed := extractNamed(targetType)
		if ownerNamed != nil && ownerNamed.HasDrop() {
			return false
		}
		return true // non-droppable parent — we own the field value
	default:
		return true // conservative: assume owned for other expression types
	}
}

// isStringFieldDup returns true if the expression is a MemberExpr accessing a
// string/vector/channel field from a type with HasDrop(). In that case,
// genFieldAccess dups the value (T0095/B0219), so the result is an owned copy.
func (c *Compiler) isStringFieldDup(expr ast.Expr, dropType types.Type) bool {
	isString := extractNamed(dropType) == types.TypString
	isVecOrChan := types.IsVector(dropType) || types.IsChannel(dropType) || types.IsArc(dropType) || types.IsWeak(dropType) || types.IsMutex(dropType) || types.IsMutexGuard(dropType)
	if !isString && !isVecOrChan {
		return false
	}
	// MemberExpr: field access on droppable type → dup'd by dupStringFieldAccess/dupContainerFieldAccess.
	if member, ok := expr.(*ast.MemberExpr); ok {
		// T1011: a narrowed enum variant field read (`if x is V { x.field }`) is
		// dup'd by genNarrowedVariantField when the subject enum is droppable —
		// just like a struct field. Without this, the var-decl borrow-clear below
		// would zero the binding's drop flag while the binding owns the dup → leak.
		if matched, droppable := c.narrowedVariantFieldDroppable(member); matched {
			return droppable
		}
		targetType := c.info.Types[member.Target]
		if c.typeSubst != nil {
			targetType = types.Substitute(targetType, c.typeSubst)
		}
		ownerNamed := extractNamed(targetType)
		return ownerNamed != nil && ownerNamed.HasDrop()
	}
	// B0204: IndexExpr on Vector[string] → string is dup'd by dup-on-read in genVectorIndex.
	// T0383: IndexExpr on Vector[Vector|Channel|Arc|Weak] → element is dup'd by
	// dup-on-read in genVectorIndex (mirrors B0219 for fields).
	// T0590: Same for fixed-size array (T[N]) — element is dup'd by dup-on-read
	// in genArrayIndex. Without this case, isStringBorrowExpr's clear-drop-flag
	// branch fires for `string x = arr[0]` and leaks the dup.
	if idx, ok := expr.(*ast.IndexExpr); ok {
		targetType := c.info.Types[idx.Target]
		if c.typeSubst != nil {
			targetType = types.Substitute(targetType, c.typeSubst)
		}
		// Unwrap refs for auto-deref.
		if ref, ok := targetType.(*types.SharedRef); ok {
			targetType = ref.Elem()
		}
		if ref, ok := targetType.(*types.MutRef); ok {
			targetType = ref.Elem()
		}
		var elemType types.Type
		if elem, isVec := types.AsVector(targetType); isVec {
			elemType = elem
		} else if arr, isArr := targetType.(*types.Array); isArr {
			elemType = arr.Elem()
		}
		if elemType != nil {
			resolvedElem := elemType
			if c.typeSubst != nil {
				resolvedElem = types.Substitute(resolvedElem, c.typeSubst)
			}
			if isString && extractNamed(resolvedElem) == types.TypString {
				return true
			}
			if isVecOrChan {
				if types.IsVector(resolvedElem) || types.IsChannel(resolvedElem) ||
					types.IsArc(resolvedElem) || types.IsWeak(resolvedElem) {
					return true
				}
			}
		}
	}
	return false
}

// isBorrowedExpr returns true if the expression's static type is `T&` or `T~`.
// Such expressions produce non-owning references (e.g., Arc.borrow,
// MutexGuard.borrow); assigning the result to a variable must NOT register an
// active drop binding, otherwise both the borrow and the parent's drop free
// the same inner value.
//
// Replaces the AST-shape heuristic from T0367/T0377/T0379. Sema propagates
// SharedRef/MutRef through if/match/paren composition, so the type check
// uniformly subsumes those cases (and extends to any future borrow-returning
// getter without enumerating expression shapes).
func (c *Compiler) isBorrowedExpr(expr ast.Expr) bool {
	typ := c.info.Types[expr]
	if c.typeSubst != nil {
		typ = types.Substitute(typ, c.typeSubst)
	}
	switch typ.(type) {
	case *types.SharedRef, *types.MutRef:
		return true
	}
	return false
}

// isRttiCastBorrow reports whether expr is a user-type RTTI downcast (`x as T` /
// `x as! T`) whose subject is a non-owning reference (this / variable / field /
// element access). Such a cast is a non-consuming *view*: the ownership pass
// never moves the subject (ownership/expr.go's CastExpr case only recurses into
// the subject, it never calls tryMoveConsume), so the cast result aliases the
// subject's instance. Binding it to a local must therefore NOT give that local
// its own drop binding — otherwise both the subject's owner and the cast local
// free the same instance (T0747 double-free). Excludes:
//   - optional-unwrap casts (`opt as! T`): those extract and own the inner value.
//   - primitive scalar casts (`x as i32`): those produce a fresh value (and carry
//     no drop binding anyway).
//   - casts of owned temps (factory()/constructor results): the local legitimately
//     claims ownership of the freshly produced instance via claimHeapTemp, so its
//     flag must stay set.
//   - casts of an owned-returning getter (`obj.getter as! T`) or user-defined `[]`
//     operator (`obj[k] as! T`): those subjects produce a *fresh owned* value (not
//     an alias), so the cast local owns it and must keep its drop flag — mirrors
//     the owned-return exemptions (isGetterCallExpr/isUserIndexExpr) elsewhere.
//
// T0800: a chained cast (`(x as! A) as! B`) wraps another CastExpr; this recurses
// to the innermost subject so the borrow check (and every exemption above) is
// re-evaluated against that subject's own type.
func (c *Compiler) isRttiCastBorrow(expr ast.Expr) bool {
	cast, ok := unwrapDestructureParens(expr).(*ast.CastExpr)
	if !ok {
		return false
	}
	subj := unwrapDestructureParens(cast.Expr)
	// T0800: a chained cast (`(x as! A) as! B`) is a view-of-a-view — the outer
	// cast aliases the inner cast, which aliases x. Recurse to the innermost
	// subject and re-run the borrow check there (each layer's optional/scalar
	// exemptions apply against its own subject type).
	if _, isCast := subj.(*ast.CastExpr); isCast {
		return c.isRttiCastBorrow(subj)
	}
	switch subj.(type) {
	case *ast.ThisExpr, *ast.IdentExpr, *ast.MemberExpr, *ast.IndexExpr:
		// borrow-producing subject — the cast aliases it
	default:
		return false
	}
	// A getter call or user-defined `[]` operator returns a fresh owned value
	// (unless it returns a borrow), so the cast owns it — not an alias.
	if (c.isGetterCallExpr(subj) || c.isUserIndexExpr(subj)) && !c.isBorrowedExpr(subj) {
		return false
	}
	srcType := c.info.Types[subj]
	if c.typeSubst != nil && srcType != nil {
		srcType = types.Substitute(srcType, c.typeSubst)
	}
	// T0850: peel a SharedRef/MutRef layer so a borrowed optional (`T?&`, e.g.
	// `Ref[T?].borrow`) is recognized as the optional-unwrap case below — its cast
	// dups the inner into an owned copy (genOptionalCastExpr borrowSource path), so
	// the cast local owns it and must keep its drop flag, not be treated as a view.
	switch ref := srcType.(type) {
	case *types.SharedRef:
		srcType = ref.Elem()
	case *types.MutRef:
		srcType = ref.Elem()
	}
	if _, isOpt := srcType.(*types.Optional); isOpt {
		return false // optional-unwrap — owns the extracted inner value
	}
	if srcNamed := extractNamed(srcType); srcNamed != nil && isPrimitiveScalar(srcNamed) {
		return false // scalar conversion — fresh value, not an alias
	}
	return true
}

// castSubjectMovableIdent peels ParenExpr/CastExpr from expr. If the
// underlying subject is an IdentExpr that has a tracked drop flag (a movable
// owned local), returns it. Otherwise returns nil. Used at owning-slot stores
// (struct field, container element, constructor argument): ownership now
// moves the cast subject at those sites (T0754), so codegen must
// symmetrically clear the subject's drop flag — otherwise the subject's
// scope-exit drop fires on the same allocation the slot now owns and produces
// a double-free.
//
// Borrowed params (no drop flag), ThisExpr, MemberExpr / IndexExpr (handled
// by the existing dup-on-read paths), and non-cast expressions all return
// nil: the existing per-shape codegen paths already handle them safely.
//
// T0800: a chained cast (`(x as! A) as! B`) wraps another CastExpr; this recurses
// to the innermost subject's IdentExpr.
func (c *Compiler) castSubjectMovableIdent(expr ast.Expr) *ast.IdentExpr {
	expr = unwrapDestructureParens(expr)
	cast, ok := expr.(*ast.CastExpr)
	if !ok {
		return nil
	}
	subj := unwrapDestructureParens(cast.Expr)
	// T0800: a chained cast moves the innermost subject at owning-slot stores,
	// so recurse to yield that subject's IdentExpr.
	if _, isCast := subj.(*ast.CastExpr); isCast {
		return c.castSubjectMovableIdent(subj)
	}
	ident, ok := subj.(*ast.IdentExpr)
	if !ok {
		return nil
	}
	if _, hasFlag := c.dropFlags[ident.Name]; !hasFlag {
		return nil
	}
	return ident
}

// consumeCastSubjectDropFlag handles the cast subject's drop flag at a consuming
// site (return / owning-slot store). For `as!` (Force) the move is unconditional
// → clear the flag. For `as` (non-Force, T0849) the move is *conditional* on the
// runtime downcast outcome → set the flag to `!isMatch` (drop the subject iff the
// cast failed and produced None), reusing the success flag captured by
// genCastExpr. This fixes the optional-`as` conditional move that previously
// double-freed on success (return path: flag left set) or leaked on failure
// (owning-slot path: flag cleared unconditionally).
//
// Force is read from expr (not from map staleness) so `as!` always takes the
// unconditional branch even if an earlier non-Force view-bind left a stale entry;
// the freshest isMatch for this consume is always set by the immediately
// preceding genCastExpr of the same subject.
func (c *Compiler) consumeCastSubjectDropFlag(expr ast.Expr, name string) {
	if cast, ok := unwrapDestructureParens(expr).(*ast.CastExpr); ok && !cast.Force {
		if matchFlag := c.castSubjectMatch[name]; matchFlag != nil {
			delete(c.castSubjectMatch, name)
			if flag, ok := c.dropFlags[name]; ok {
				notMatch := c.block.NewXor(matchFlag, constant.NewInt(irtypes.I1, 1))
				c.block.NewStore(notMatch, flag)
			}
			return
		}
	}
	c.clearDropFlag(name)
}

// isGetterCallExpr reports whether expr is a MemberExpr whose Field resolves
// to a getter method on its target's type. Getters return owned values
// (tracked via trackGetterResult/claimStringTemp), so the LHS of
// `s := obj.getter` must keep its drop flag instead of being cleared by
// the borrow-RHS branch. Complements the existing detection for
// non-local IdentExpr and module.getter MemberExpr. T0501.
func (c *Compiler) isGetterCallExpr(expr ast.Expr) bool {
	member, ok := expr.(*ast.MemberExpr)
	if !ok {
		return false
	}
	// Module-level getter (mod.property) — returns a fresh owned value, not a
	// borrow. Recognized via sema's resolution so a getter returning a function
	// type is also handled (its local owns the closure env) (T1240).
	if c.info.ModuleGetters[member] {
		return true
	}
	targetType := c.info.Types[member.Target]
	if c.typeSubst != nil && targetType != nil {
		targetType = types.Substitute(targetType, c.typeSubst)
	}
	if named := extractNamed(targetType); named != nil {
		return named.LookupGetter(member.Field) != nil
	}
	if enum := extractEnum(targetType); enum != nil {
		return enum.LookupGetter(member.Field) != nil
	}
	return false
}

// isUserIndexExpr reports whether expr is an IndexExpr that dispatches to a
// user-defined *non-native* `[]` operator. genIndexExpr compiles such reads via
// genMethodIndex, which (T0647) returns an *owned* heap temp tracked by
// trackUserIndexResult and claimed into the LHS by claimStringTemp/claimHeapTemp
// — exactly like an ordinary method call. Native container/array indexing
// (genNativeIndex/genVectorIndex/genStringIndex/genArrayIndex) instead returns a
// borrowed alias into container storage. isStringBorrowExpr treats *all*
// IndexExprs as borrows, so without this exemption the borrow-RHS drop-flag
// clearing in genVarDecl/genInferredVarDecl would clear the LHS flag and leak
// the owned operator return. Mirrors genIndexExpr's dispatch and the analogous
// isGetterCallExpr / module-getter owned-return exemptions.
func (c *Compiler) isUserIndexExpr(expr ast.Expr) bool {
	idx, ok := expr.(*ast.IndexExpr)
	if !ok {
		return false
	}
	targetType := c.info.Types[idx.Target]
	if c.typeSubst != nil {
		targetType = types.Substitute(targetType, c.typeSubst)
	}
	if ref, ok := targetType.(*types.MutRef); ok {
		targetType = ref.Elem()
	}
	if ref, ok := targetType.(*types.SharedRef); ok {
		targetType = ref.Elem()
	}
	if _, isArr := targetType.(*types.Array); isArr {
		return false // fixed-size array indexing — borrowed slot
	}
	named := extractNamed(targetType)
	if named == nil {
		return false
	}
	m := named.LookupMethod("[]")
	return m != nil && !m.IsNative()
}

// isClosureAggregateBorrow reports whether expr reads a closure (function value)
// out of an *owning aggregate* — a struct/optional closure field (`h.cb`,
// `h.cb!`) or a container element (`v[0]`). Such a read copies the closure's fat
// pointer `{fn, env}` by value while the aggregate retains ownership of the heap
// env (closures aren't Cloneable, so there is no env dup on read, and ownership
// treats the read as a copy/alias rather than a move). Registering an owning
// env-free binding for the local would therefore double-free the env at scope
// exit against the aggregate's own drop (T0812). Returning true here suppresses
// that binding — the local borrows, the aggregate keeps sole ownership, mirroring
// the borrow handling in isBorrowedExpr/isRttiCastBorrow.
//
// Excludes owned-return shapes whose local legitimately owns a *fresh* closure:
//   - getter returning a closure by value (isGetterCallExpr);
//   - user-defined non-native `[]` returning a closure (isUserIndexExpr).
//
// An *ast.IdentExpr source (`f := g`, `f := o!` on a local) is not matched: a
// plain move/unwrap of a local transfers ownership (the RHS drop flag / optional
// present flag is cleared), so the local must keep its owning binding.
func (c *Compiler) isClosureAggregateBorrow(expr ast.Expr) bool {
	if expr == nil {
		return false
	}
	// Type gate (T1230): the read's result must transitively nest a closure —
	// either a direct closure field (*types.Signature) or an aggregate whose
	// field/variant is a closure (`Fn { () -> int f; }`). FirstFieldNestedClosure
	// treats refcounted std containers (Ref/Weak/...) as opaque, so sound
	// refcounted nesting is not misclassified. Without this gate the shape checks
	// below also match non-closure field/element reads (strings, vectors), which
	// must be deep-cloned, not borrowed. The direct-Signature callers
	// (maybeRegisterEnvFree) are unaffected: FirstFieldNestedClosure(Signature)
	// returns the signature itself.
	rt := c.info.Types[expr]
	if c.typeSubst != nil && rt != nil {
		rt = types.Substitute(rt, c.typeSubst)
	}
	// T1262: use the Deep variant (mirrors ownership closureAggregateBorrowSource
	// exactly) so a BARE value-copying container of closures read from an aliasing
	// container (`b := m[0]!`, result `Vector[() -> int]`) is treated as a borrow —
	// its `[]` now leaves the value aliased (typeNeedsMatchDup returns false), so no
	// owning env-free/drop binding must be registered. Refcounted nesting stays
	// opaque (FirstFieldNestedClosureDeep keeps Ref/Weak/... opaque).
	if sema.FirstFieldNestedClosureDeep(rt) == nil {
		return false
	}
	e := unwrapDestructureParens(expr)
	// Peel a force-unwrap of an optional closure field: `h.cb!` or `h.cb as! (...)`.
	if unwrap, ok := e.(*ast.OptionalUnwrapExpr); ok {
		e = unwrapDestructureParens(unwrap.Expr)
	} else if cast, ok := e.(*ast.CastExpr); ok && cast.Force {
		subj := unwrapDestructureParens(cast.Expr)
		subjType := c.info.Types[subj]
		if c.typeSubst != nil && subjType != nil {
			subjType = types.Substitute(subjType, c.typeSubst)
		}
		if _, isOpt := subjType.(*types.Optional); isOpt {
			e = subj
		}
	}
	switch e.(type) {
	case *ast.MemberExpr, *ast.IndexExpr:
		// struct/optional closure field, or container element — aliasing read
	default:
		return false
	}
	// T1262/T1263: the Deep type gate also admits a BARE value-copying container of
	// closures as the read's RESULT (`m[0]!`, `vv[0]`, `h.fns` → Vector[() -> int]) — a
	// shape the shallow FirstFieldNestedClosure rejects (nil). For a bare container the
	// read is a borrow only when it ALIASES storage the owner frees at scope exit:
	//   - an aliasing container index — native Vector (vv[0], T1263) or user Map (m[0]!, T1262)
	//   - a struct/enum field read (h.fns, T1263); getters are classified below via the shared alias filter
	// A user non-aliasing `[]` returns a fresh OWNED value (indexTargetIsAliasingContainer false).
	if sema.FirstFieldNestedClosure(rt) == nil {
		switch e.(type) {
		case *ast.IndexExpr:
			if !c.indexTargetIsAliasingContainer(e) {
				return false
			}
		case *ast.MemberExpr:
			// struct/enum field — getters classified below via the shared alias filter
		default:
			return false
		}
	}
	// A getter's result is fresh only when T1227's "no direct field return" guarantee
	// applies. A getter CAN legally hand back a borrowed CONTAINER ELEMENT (e.g.
	// `get cb() -> int { return this.slots[0]; }`), which is not fresh — the receiver
	// still owns the element and frees it at scope exit. closureResultMayAliasCallInput
	// already carries this exact classification (it drives whether the getter's temp is
	// tracked at all); reuse it here instead of assuming every getter is owned, or a
	// var-decl binding of an aliasing getter result double-frees against the receiver's
	// drop (T1290-class bug — the discard case was already correct, only the bound case
	// defaulted to "owned").
	if c.isGetterCallExpr(e) {
		return c.closureResultMayAliasCallInput(e)
	}
	// A user-defined non-native `[]` normally returns a FRESH owned value (a
	// freshly-built closure / a duped string or heap result), so it is exempt —
	// the local legitimately owns it.
	//
	// EXCEPTION (T1247): a std aliasing container (Map — Vector's `[]` is native,
	// so isUserIndexExpr is already false for it) returns the slot's element BY
	// VALUE, aliasing internal storage. A closure element is not duped on read
	// (closures aren't Cloneable), so the returned fat pointer `{fn, env}` aliases
	// the container's stored env. Treating that as an owned return would register
	// an owning env-free binding that double-frees against the container's own
	// drop. Keep the borrow treatment for aliasing containers. Mirrors the
	// ownership-side isUserIndexExpr && !indexTargetIsAliasingContainer gate (T1113).
	if c.isUserIndexExpr(e) && !c.indexTargetIsAliasingContainer(e) {
		return false
	}
	return true
}

// identBorrowsMatchBorrowedClosure reports whether a var-decl `hs := gs` re-binds
// an ident (`gs`) recorded in matchBorrowedIdents as a match-borrowed alias whose
// type transitively nests a closure — either a DIRECT closure (`E.Cb(() -> int f)`,
// T1259) or a value-copying container of closures (`E.Fns(Vector[() -> int] fs)`,
// T1264). Such a binding is a borrow: the shared env is owned by the enum's variant
// data (freed by the enum's own drop), so the new local must NOT register an owning
// container/env-free drop — that would double-free. isClosureAggregateBorrow does
// not peel ident sources, so a re-bound match-borrowed ident needs this dedicated
// gate (used by the var-decl arms for the container path and by maybeRegisterEnvFree
// for the Signature path). The Deep predicate keeps refcounted handles opaque, so
// `hs := gs` off a `Ref[...]` binding is unaffected.
func (c *Compiler) identBorrowsMatchBorrowedClosure(valueExpr ast.Expr, typ types.Type) bool {
	id, ok := unwrapDestructureParens(valueExpr).(*ast.IdentExpr)
	if !ok {
		return false
	}
	if c.matchBorrowedIdents == nil || !c.matchBorrowedIdents[id.Name] {
		return false
	}
	rt := typ
	if c.typeSubst != nil && rt != nil {
		rt = types.Substitute(rt, c.typeSubst)
	}
	return sema.FirstFieldNestedClosureDeep(rt) != nil
}

// markMatchBorrowedRebind handles a var-decl `hs := gs` whose source `gs` is a
// match-borrowed alias of a closure nested in an enum variant (T1259 direct
// closure / T1264 value-copying container of closures). The new local borrows —
// clear its owning container/env-free drop (a no-op via the i1 flag) so the
// shared buffer/envs are freed exactly once (by the enum's own drop), and
// propagate the borrow mark so a further `js := hs` also borrows. Shared by
// genTypedVarDecl and genInferredVarDecl so the two arms stay in lockstep.
func (c *Compiler) markMatchBorrowedRebind(name string, valueExpr ast.Expr, typ types.Type) {
	if !c.identBorrowsMatchBorrowedClosure(valueExpr, typ) {
		return
	}
	c.clearDropFlag(name)
	if c.matchBorrowedIdents == nil {
		c.matchBorrowedIdents = make(map[string]bool)
	}
	c.matchBorrowedIdents[name] = true
}

// indexTargetIsAliasingContainer reports whether e is an IndexExpr whose target is
// a std aliasing container (Map or Vector) — one whose `[]` returns the stored
// element by value, aliasing internal storage, rather than a freshly-constructed
// owned value. Used by isClosureAggregateBorrow to keep borrow treatment for a
// closure read out of such a container (T1247). Mirrors the ownership-side
// indexTargetIsAliasingContainer.
func (c *Compiler) indexTargetIsAliasingContainer(e ast.Expr) bool {
	idx, ok := e.(*ast.IndexExpr)
	if !ok {
		return false
	}
	t := c.info.Types[idx.Target]
	if c.typeSubst != nil && t != nil {
		t = types.Substitute(t, c.typeSubst)
	}
	if _, ok := types.AsVector(t); ok {
		return true
	}
	if _, _, ok := types.AsMap(t); ok {
		return true
	}
	if _, _, ok := types.AsArray(t); ok {
		return true // T1266: a fixed-array index aliases the array's owned storage
	}
	return false
}

// isStringBorrowExpr returns true if the expression borrows an existing value
// (e.g., container element access, field access) rather than creating a new one.
// Borrowed values should not be freed by the borrower — the owner retains responsibility.
// Used for both string and vector drop flag management.
func isStringBorrowExpr(expr ast.Expr) bool {
	switch expr.(type) {
	case *ast.IndexExpr:
		return true // vector[i], map[key] — borrows from container
	case *ast.MemberExpr:
		// T0095: String fields from droppable types are duped in genFieldAccess,
		// so the result is an owned copy, not a borrow. The caller handles the
		// distinction based on type info — MemberExpr alone cannot determine this.
		return true
	case *ast.IdentExpr:
		return true // variable reference — handled by clearDropFlag on RHS
	default:
		return false
	}
}

// genBlockValue generates a block like genBlock, but returns the value of the
// last expression statement (if any). Avoids the double-generation that would
// occur if genBlock + separate genExpr on the last statement were used.

func (c *Compiler) genBlockValue(block *ast.Block) value.Value {
	if block == nil {
		c.blockValueOwnedResult = false // T1107
		return nil
	}
	// T1029: a block value (if/match/handler arm body) is its own straight-line
	// region nested inside the discarded expression — it is not the discarded
	// top-level call. Clear discardedExpr so alias-arg pointers are not recorded in
	// non-dominating branch blocks (which would produce invalid IR at the clear
	// site). Inner ExprStmts set their own discardedExpr as usual.
	prevDiscard := c.discardedExpr
	prevArgPtrs := c.discardAliasArgPtrs
	c.discardedExpr = nil
	c.discardAliasArgPtrs = nil
	defer func() {
		c.discardedExpr = prevDiscard
		c.discardAliasArgPtrs = prevArgPtrs
	}()
	// T1329: a block-value body (if/match arm, `?` handler) is used as a non-last
	// argument to an enclosing call whose earlier arguments may have materialized
	// sibling temps that are still tracked with live drop flags. A LEADING (non-last)
	// statement in this body reaches a statement boundary (cleanupStmtLevelTemps)
	// mid-body — without a barrier that would drain the WHOLE tracking array,
	// freeing the enclosing expression's siblings, which it then re-reads after the
	// merge → use-after-free. Snapshot the current tracking depths as a floor so
	// intermediate drains only touch temps created within the body. Also snapshot
	// the sibling prefix so it can be rebuilt if the body diverges (return/raise),
	// where genReturnStmt/genRaiseStmt drain the FULL array (dropping the prefix on
	// that runtime path — correct) and truncate below the floor.
	savedFloorStmt, savedFloorHeap := c.blockTempFloorStmt, c.blockTempFloorHeap
	savedFloorEnv, savedFloorEnum := c.blockTempFloorEnv, c.blockTempFloorEnum
	floorStmt, floorHeap := len(c.stmtTemps), len(c.heapTemps)
	floorEnv, floorEnum := len(c.envTemps), len(c.enumCtorTemps)
	prefixStmt := append([]stmtTemp(nil), c.stmtTemps...)
	prefixHeap := append([]heapTemp(nil), c.heapTemps...)
	prefixEnv := append([]envTemp(nil), c.envTemps...)
	prefixEnum := append([]enumCtorTemp(nil), c.enumCtorTemps...)
	prefixStmtMap := copyTempMap(c.stmtTempMap)
	prefixHeapMap := copyTempMap(c.heapTempMap)
	prefixEnvMap := copyTempMap(c.envTempMap)
	c.blockTempFloorStmt, c.blockTempFloorHeap = floorStmt, floorHeap
	c.blockTempFloorEnv, c.blockTempFloorEnum = floorEnv, floorEnum
	defer func() {
		c.blockTempFloorStmt, c.blockTempFloorHeap = savedFloorStmt, savedFloorHeap
		c.blockTempFloorEnv, c.blockTempFloorEnum = savedFloorEnv, savedFloorEnum
		// If the body diverged, the divergent path already dropped the prefix (and
		// reset each drop flag to 0), truncating the array below the floor. Rebuild
		// the prefix tracking so a SIBLING arm and the enclosing statement still emit
		// their own flag-guarded drops on the non-diverging runtime path. Drop flags
		// are runtime allocas the divergent path zeroed, so the re-emitted drops are
		// no-ops there → no double free (B0198's flag-guarded idempotence). On the
		// non-divergent path the floor kept the prefix intact → these are no-ops.
		if len(c.stmtTemps) < floorStmt {
			c.stmtTemps = prefixStmt
			c.stmtTempMap = prefixStmtMap
		}
		if len(c.heapTemps) < floorHeap {
			c.heapTemps = prefixHeap
			c.heapTempMap = prefixHeapMap
		}
		if len(c.envTemps) < floorEnv {
			c.envTemps = prefixEnv
			c.envTempMap = prefixEnvMap
		}
		if len(c.enumCtorTemps) < floorEnum {
			c.enumCtorTemps = prefixEnum
		}
	}()
	// T1384: read-and-clear the one-shot go!-value flag so only THIS (outermost,
	// the go! body) block yields a trailing auto-propagated value; nested arm
	// blocks reached via genExpr below see it cleared and keep discard semantics.
	wantAutoPropValue := c.goBlockTrailingWantValue
	c.goBlockTrailingWantValue = false
	// T1427: read-and-clear the one-shot go!-value result drop type — only THIS
	// (outermost go! body) block drops its trailing result on a close-error divert;
	// nested arm blocks reached via genExpr see it cleared.
	resultDropType := c.goBlockResultDropType
	c.goBlockResultDropType = nil
	savedScopeLen := len(c.scopeBindings)
	// T1107: track whether this block yields an owned heap value moved out of the
	// block scope, so genIfExpr / genMatchArmValue can register the merge phi as an
	// owned stmt temp. Reset here; set authoritatively at the normal-result path
	// below (stays false for borrow / auto-propagate / value results).
	blockOwned := false
	// T1208: live per-path i1 ownership flag when the block result is a nested tracked
	// temp (an if/match phi with its own per-path flag phi). Captured before
	// claimStringTemp neutralizes it; threaded to the enclosing merge phi so a nested
	// mixed owned/borrowed conditional does not drop a borrowed value. nil for
	// owned-local idents (whole-arm transfer → constant is correct) and borrows.
	var blockOwnedFlag value.Value
	var result value.Value
	n := len(block.Stmts)
	for i, stmt := range block.Stmts {
		if c.block == nil || c.block.Term != nil {
			break
		}
		if i == n-1 {
			if es, ok := stmt.(*ast.ExprStmt); ok {
				// T1326: snapshot the enum-ctor temp count before evaluating the arm
				// tail so drainNestedArmEnumCtorTemps can drop any by-value call-arg
				// ctor temps this arm creates that are NOT the phi'd result.
				tailEnumSnap := len(c.enumCtorTemps)
				if c.info.AutoPropagateExprs[es.Expr] {
					if wantAutoPropValue {
						// T1384: a `go! {}` value body's trailing bare failable call
						// yields its auto-propagated success value as the block result
						// (the error path routes to the go-block sink inside
						// genAutoPropagateValue). Ownership of an owned heap success value
						// transfers to the block's caller (genGoBlock stores it into
						// G.result_ptr), so mark it owned like the normal-result path.
						result = c.genAutoPropagateValue(c.genExpr(es.Expr))
						if result != nil {
							if c.resultIsFreshOwnedHeapTemp(result) {
								blockOwned = true
							}
							c.claimStringTemp(result)
						}
					} else {
						// Failable call: auto-propagate error, discard success value.
						// Block arms don't contribute typed results to match phis;
						// only expression arms (arm.Body) produce match result values.
						c.genAutoPropagate(es.Expr)
					}
				} else if c.borrowBlockResult {
					// T0792: result consumed as a borrow (`T&`/`T~`) — the last expr
					// aliases storage owned elsewhere, so do not dup or track it. The
					// inner expr's natural type (`r.d[0]` → `string`) would otherwise
					// set dupStringFieldAccess and allocate a copy that the borrow bind
					// site never takes ownership of. Reset across the genExpr call so a
					// nested block/if/match keeps its normal owning semantics.
					savedBorrow := c.borrowBlockResult
					c.borrowBlockResult = false
					c.dupStringFieldAccess = false
					c.dupContainerFieldAccess = false
					result = c.genExpr(es.Expr)
					c.borrowBlockResult = savedBorrow
				} else {
					// T0095/B0219/B0310/T0487: Signal genFieldAccess to dup string,
					// Vector|Channel|Arc|Weak, and Optional[...] fields for block
					// results so the block's caller owns an independent copy.
					exprType := c.info.Types[es.Expr]
					if c.typeSubst != nil && exprType != nil {
						exprType = types.Substitute(exprType, c.typeSubst)
					}
					c.setDupFlagsForFieldAccess(exprType)
					result = c.genExpr(es.Expr)
					c.dupStringFieldAccess = false
					c.dupContainerFieldAccess = false
					// Clear drop flag for ident block result — the value is being
					// moved out of the block scope. Without this, scope cleanup would
					// free the string while the outer scope still holds the pointer.
					if ident, ok := es.Expr.(*ast.IdentExpr); ok {
						// T1107: an owned-local ident moved out (had a live drop flag)
						// transfers ownership to the block's caller.
						if _, has := c.dropFlags[ident.Name]; has {
							blockOwned = true
						}
						c.clearDropFlag(ident.Name)
					}
					// T1107: a live tracked stmt temp (clone()/call result, or a
					// nested if/match phi tracked by trackMergeResultTemp) as the block
					// result is an owned heap value the caller now owns.
					if result != nil {
						if idx, ok := c.stmtTempMap[result]; ok && idx >= 0 {
							blockOwned = true
							// T1208: capture the temp's live per-path flag before
							// claimStringTemp zeroes it, so a nested mixed owned/borrowed
							// conditional threads the real per-path bit to the enclosing phi.
							blockOwnedFlag = c.captureLiveTempFlag(result)
						} else if c.resultIsFreshOwnedHeapTemp(result) {
							// T1211: a fresh owned heap value struct (heap-user-type / Map
							// constructor or clone) moved out of the block transfers
							// ownership to the block's caller, but is tracked as a heapTemp
							// (not a stmtTemp) so the stmtTempMap check above misses it.
							blockOwned = true
						} else {
							// T1211: a nested value-struct merge result carries its per-path
							// ownership flag in mergeBoundStructFlag — thread it up.
							blockOwnedFlag = c.captureLiveTempFlag(result)
							if blockOwnedFlag != nil {
								blockOwned = true
							}
						}
					}
					// T0095: Claim string dup temps from block result expressions.
					// Without this, a dup from e.g. `e.message` would be freed at
					// statement end while the caller still holds the pointer.
					c.claimStringTemp(result)
					// T0487: Claim dup'd inner string for Optional[string] field
					// access — the dup is embedded in the result struct.
					if c.optionalStringDup != nil {
						c.claimStringTemp(c.optionalStringDup)
						c.optionalStringDup = nil
					}
					// T0487: Claim dup'd inner container for
					// Optional[Vector|Channel|Arc|Weak] field access — the dup is
					// embedded in the result struct and must survive past
					// statement-end cleanup.
					if c.optionalContainerDup != nil {
						c.claimStringTemp(c.optionalContainerDup)
						c.optionalContainerDup = nil
					}
				}
				// T1326: drain any by-value call-arg enum-ctor temps this arm tail
				// created (kept only when the tail itself is a move-out ctor).
				c.drainNestedArmEnumCtorTemps(es.Expr, tailEnumSnap)
				break
			}
			// B0126: Handle if/else as the last statement in a block that
			// produces a value. The parser emits IfStmt (not IfExpr) in
			// statement position, but we need to capture the value from
			// both branches when the block is used as an expression.
			if ifS, ok := stmt.(*ast.IfStmt); ok {
				result = c.genIfStmtValue(ifS)
				// T1206: a value-producing if in statement position registers its owned
				// i8* phi as a tracked temp (genIfStmtValue → trackMergeResultTemp). When
				// it does, the block yields an owned heap value moved out to the block's
				// caller — mirror the ExprStmt path's blockOwned handling so genIfExpr /
				// genMatchArmValue register the enclosing merge phi as owned too.
				if result != nil {
					if idx, ok := c.stmtTempMap[result]; ok && idx >= 0 {
						blockOwned = true
						// T1208: no claimStringTemp on this path, so the temp's per-path
						// flag is still live — capture it for the enclosing merge phi.
						blockOwnedFlag = c.captureLiveTempFlag(result)
					} else if c.resultIsFreshOwnedHeapTemp(result) {
						// T1211: fresh owned heap value struct (heap-user-type / Map)
						// moved out of a value-producing if in statement position.
						blockOwned = true
					} else {
						// T1211: nested value-struct merge result — thread its per-path flag.
						blockOwnedFlag = c.captureLiveTempFlag(result)
						if blockOwnedFlag != nil {
							blockOwned = true
						}
					}
				}
				break
			}
		}
		c.genStmt(stmt)
	}
	if c.block != nil && c.block.Term == nil && len(c.scopeBindings) > savedScopeLen {
		// T1427: arm the divert-drop for the trailing heap result across the close
		// check — if a failing use-close diverts this exit, the claimed result would
		// otherwise be orphaned (the store into G.result_ptr happens only on the OK
		// continuation). Cleared immediately after so no other exit sees it.
		if c.inFailableGoBlock && result != nil && resultDropType != nil {
			c.goResultDivertVal, c.goResultDivertType = result, resultDropType
		}
		cap := c.emitScopeCleanup(savedScopeLen, false)
		c.emitCloseErrCheck(cap, savedScopeLen)
		c.goResultDivertVal, c.goResultDivertType = nil, nil
	}
	c.scopeBindings = c.scopeBindings[:savedScopeLen]
	c.blockValueOwnedResult = blockOwned   // T1107
	c.blockValueOwnedFlag = blockOwnedFlag // T1208
	return result
}

// genStmt generates LLVM IR for a single statement.
func (c *Compiler) genStmt(stmt ast.Stmt) {
	switch s := stmt.(type) {
	case *ast.ExprStmt:
		// Mark fire-and-forget go expressions: when a go expression is used
		// as a statement (result discarded), the G struct should be freed by
		// goroutine_exit rather than waiting for a receiver that doesn't exist.
		if _, ok := s.Expr.(*ast.GoExpr); ok {
			c.goExprFireAndForget = true
		}
		var discardedResult value.Value
		if c.info.AutoPropagateExprs[s.Expr] {
			c.genAutoPropagate(s.Expr)
		} else {
			// T1029/T1017: mark the discarded expression so a call returning a value
			// that aliases an owned-local arg keeps the source local as the single
			// owner (freed once at scope exit), instead of freeing the result temp at
			// statement end while the local is still live (use-after-free).
			// emitReturnAliasCheckSubst suppresses the source drop-flag clear and
			// records the aliased arg pointers; the result temp's flag is cleared
			// instead — via emitDiscardAliasClears at the tracking site (heap user
			// types in trackHeapUserTypeResult, and the i8* stmtTemp path in genExpr's
			// CallExpr case), plus clearDiscardedAliasTempFlag below for the top-level
			// stmtTemp result (T1017's vector/string discard path). Each clear is a
			// runtime pointer compare, so only temps that genuinely alias a recorded
			// arg are neutralized. Save/restore across the (possibly nested) genExpr so
			// a sub-expression that itself contains discarded statements keeps its own
			// state without corrupting this statement's.
			prevDiscard := c.discardedExpr
			prevArgPtrs := c.discardAliasArgPtrs
			c.discardedExpr = unwrapDestructureParens(s.Expr)
			c.discardAliasArgPtrs = nil
			discardedResult = c.genExpr(s.Expr)
			recorded := c.discardAliasArgPtrs
			c.discardedExpr = prevDiscard
			c.discardAliasArgPtrs = prevArgPtrs
			c.clearDiscardedAliasTempFlag(discardedResult, recorded)
		}
		c.goExprFireAndForget = false
		// B0196/B0208: When a discarded expression returns an Optional with a
		// droppable inner type, the inner value leaks because trackStringTemp
		// only tracks bare i8* values, not {i1, T} optional structs.
		c.dropDiscardedOptional(s.Expr, discardedResult)
		// B0211: When a discarded expression returns a heap-allocated user type
		// (e.g., bare constructor call like `Foo(x: 1);`), free the instance.
		c.dropDiscardedHeapType(s.Expr, discardedResult)
		// T1233: When a discarded expression is a droppable tuple temp (e.g.
		// `(make_str(), 1);`), field-wise drop it at statement end.
		c.dropDiscardedTuple(s.Expr, discardedResult)
		// T1306: free a discarded generator factory result (raw {handle, slot}
		// coroutine value) — genDestroy(handle) + palFree(slot), matching
		// consumed-generator cleanup.
		c.dropDiscardedGenerator(s.Expr, discardedResult)
	case *ast.ReturnStmt:
		c.genReturnStmt(s)
	case *ast.TypedVarDecl:
		c.genTypedVarDecl(s)
	case *ast.InferredVarDecl:
		c.genInferredVarDecl(s)
	case *ast.AssignStmt:
		c.genAssignStmt(s)
	case *ast.IfStmt:
		c.genIfStmt(s)
	case *ast.WhileStmt:
		c.genWhileStmt(s)
	case *ast.WhileUnwrapStmt:
		c.genWhileUnwrapStmt(s)
	case *ast.ForInStmt:
		c.genForInStmt(s)
	case *ast.ClassicForStmt:
		c.genClassicForStmt(s)
	case *ast.InfiniteLoop:
		c.genInfiniteLoop(s)
	case *ast.BreakStmt:
		c.genBreakStmt()
	case *ast.ContinueStmt:
		c.genContinueStmt()
	case *ast.RaiseStmt:
		c.genRaiseStmt(s)
	case *ast.DestructureVarDecl:
		c.genDestructureVarDecl(s)
	case *ast.UseVarDecl:
		c.genUseVarDecl(s)
	case *ast.IncDecStmt:
		c.genIncDecStmt(s)
	case *ast.SelectStmt:
		c.genSelectStmt(s)
	case *ast.YieldStmt:
		c.genYieldStmt(s)
	case *ast.YieldDelegateStmt:
		c.genYieldDelegateStmt(s)
	case *ast.Block:
		c.genBlock(s)
	default:
		panic(fmt.Sprintf("codegen: unhandled statement type %T", stmt))
	}
	// T0073: Drop any unclaimed string temps from this statement.
	// T0088: Drop any unclaimed heap instance temps (e.g., _FnIter in iterator chains).
	c.cleanupStmtLevelTemps()
}

// genAutoPropagate generates implicit error propagation for a failable call
// used as a statement in a failable function. Same semantics as explicit `?`:
// check the error tag, propagate on error, discard ok value on success.
func (c *Compiler) genAutoPropagate(expr ast.Expr) {
	result := c.genExpr(expr)
	calleeResultType := result.Type().(*irtypes.StructType)
	c.emitFailableResultPropagation(result)

	// Ok path: drop discarded success value, then continue (B0261).
	if !isVoidResult(calleeResultType) {
		okVal := c.block.NewExtractValue(result, 1)
		c.dropDiscardedAutoPropagate(expr, okVal)
	}
}

// propagateIfFailable wraps a setter-style call result in auto-propagation when
// the call returns a failable result struct ({i1, ...}). For non-failable void
// returns this is a no-op. T0708.
func (c *Compiler) propagateIfFailable(result value.Value) {
	if _, isStruct := result.Type().(*irtypes.StructType); isStruct {
		c.emitFailableResultPropagation(result)
	}
}

// unwrapFailableCompoundRead unwraps a getter call result used as the "current"
// value in a compound assignment, propagating the error when the getter is
// failable. operandType is the (sema, pre-subst) value type of the compound
// target — the result is unwrapped only when its LLVM type is exactly the
// failable-result shape {i1, operandLLVM, i8*}. A non-failable value-type/Map
// getter returns {i8*, ...} (field0 = i8*) and won't match; a non-failable
// scalar getter returns a non-struct. T0709.
func (c *Compiler) unwrapFailableCompoundRead(current value.Value, operandType types.Type) value.Value {
	st, ok := current.Type().(*irtypes.StructType)
	if !ok {
		return current
	}
	inner := operandType
	if c.typeSubst != nil {
		inner = types.Substitute(inner, c.typeSubst)
	}
	if st.Equal(computeResultType(c.resolveType(inner))) {
		return c.genAutoPropagateValue(current)
	}
	return current
}

// emitFailableResultPropagation emits the auto.propagate / auto.ok branch for
// a failable LLVM call result. After this returns, c.block is the auto.ok block
// and the caller can continue emitting code (the ok-value, if any, is unused
// by this helper). T0708.
func (c *Compiler) emitFailableResultPropagation(result value.Value) {
	calleeResultType := result.Type().(*irtypes.StructType)

	tag := c.block.NewExtractValue(result, 0)

	propagateBlock := c.newBlock("auto.propagate")
	okBlock := c.newBlock("auto.ok")
	c.block.NewCondBr(tag, propagateBlock, okBlock)

	// Error path: cleanup stmt temps + scope bindings, extract error, propagate
	c.block = propagateBlock
	c.emitAllStmtTempCleanupForErrorPath() // T0103/T1272: free all temp kinds before returning
	if len(c.scopeBindings) > 0 {
		c.emitScopeCleanup(0, true) // error in flight — suppress close errors
	}
	errVal := c.block.NewExtractValue(result, resultErrIdx(calleeResultType))
	if c.inFailableGoBlock {
		// T1384: store the error into the goroutine's result aggregate and branch
		// to the coroutine's final suspend (a `ret` is invalid in the coro ramp).
		c.emitFailableGoBlockError(errVal)
	} else if c.inGenerator && c.generatorCanError {
		// B0023: store error to generator error_slot and branch to final suspend
		c.emitGeneratorError(errVal)
	} else {
		callerResultType := c.currentResultType()
		c.block.NewRet(c.wrapError(errVal, callerResultType))
	}

	c.block = okBlock
}

// dropDiscardedAutoPropagate drops a discarded success value from an auto-propagated
// failable call. Without this, heap-allocated return values (strings, vectors, channels)
// leak when the caller discards the result. B0261.
func (c *Compiler) dropDiscardedAutoPropagate(expr ast.Expr, val value.Value) {
	exprType := c.info.Types[expr]
	if exprType == nil {
		return
	}
	if c.typeSubst != nil {
		exprType = types.Substitute(exprType, c.typeSubst)
	}
	// B0262: Closures — free env struct via drop-or-free.
	if _, isSig := exprType.(*types.Signature); isSig {
		envPtr := c.block.NewExtractValue(val, 1)
		isNull := c.block.NewICmp(enum.IPredEQ, envPtr, constant.NewNull(irtypes.I8Ptr))
		freeBlock := c.newBlock("autoprop.env.free")
		skipBlock := c.newBlock("autoprop.env.skip")
		c.block.NewCondBr(isNull, skipBlock, freeBlock)
		c.block = freeBlock
		c.emitEnvDropOrFree(envPtr)
		c.block.NewBr(skipBlock)
		c.block = skipBlock
		return
	}

	named := extractNamed(exprType)
	if named == nil {
		return
	}
	switch {
	case named == types.TypString:
		if dropFn := c.funcs["promise_string_drop"]; dropFn != nil {
			c.block.NewCall(dropFn, val)
		}
	case named == types.TypVector || types.IsVector(exprType):
		if dropFn := c.funcs["Vector.drop"]; dropFn != nil {
			c.block.NewCall(dropFn, val)
		}
	case named == types.TypChannel || types.IsChannel(exprType):
		if elemType, ok := types.AsChannel(exprType); ok {
			// T0663: per-element-type drop walks any un-received buffered items.
			dropFn := c.getOrCreateChannelDrop(elemType)
			c.block.NewCall(dropFn, val)
		}
	case types.IsArc(exprType) || named == types.TypArc:
		if elemType, ok := types.AsArc(exprType); ok {
			dropFn := c.getOrCreateArcDrop(elemType)
			c.block.NewCall(dropFn, val)
		}
	case types.IsWeak(exprType) || named == types.TypWeak:
		if elemType, ok := types.AsWeak(exprType); ok {
			dropFn := c.getOrCreateWeakDrop(elemType)
			c.block.NewCall(dropFn, val)
		}
	case types.IsMutex(exprType) || named == types.TypMutex:
		if elemType, ok := types.AsMutex(exprType); ok {
			dropFn := c.getOrCreateMutexDrop(elemType)
			c.block.NewCall(dropFn, val)
		}
	case types.IsMutexGuard(exprType) || named == types.TypMutexGuard:
		if dropFn := c.funcs["MutexGuard.drop"]; dropFn != nil {
			c.block.NewCall(dropFn, val)
		}
	case named.HasDrop() || named.NeedsSynthDrop():
		// B0262: Heap user types (including Map, Set) — call drop + free.
		ownerName := named.Obj().Name()
		resolvedType := exprType
		if c.typeSubst != nil {
			resolvedType = types.Substitute(exprType, c.typeSubst)
		}
		if inst, ok := resolvedType.(*types.Instance); ok {
			ownerName = monoName(inst)
		} else if named.HasDrop() && !named.NeedsSynthDrop() {
			ownerName = c.resolveDropOwner(named)
		}
		mangledName := mangleMethodName(ownerName, "drop", false)
		if dropFn := c.funcs[mangledName]; dropFn != nil {
			instance := c.extractInstancePtr(val)
			nullCheck := c.block.NewICmp(enum.IPredEQ, instance, constant.NewNull(irtypes.I8Ptr))
			execBlock := c.newBlock("autoprop.drop")
			skipBlock := c.newBlock("autoprop.drop.skip")
			c.block.NewCondBr(nullCheck, skipBlock, execBlock)
			c.block = execBlock
			c.block.NewCall(dropFn, instance)
			if !named.NeedsSynthDrop() {
				c.block.NewCall(c.palFree, instance)
			}
			c.block.NewBr(skipBlock)
			c.block = skipBlock
		}
	}
}

// genAutoPropagateValue extracts the ok value from a failable result,
// propagating the error to the caller if the call failed.
// Used for auto-propagation in variable declarations.
func (c *Compiler) genAutoPropagateValue(result value.Value) value.Value {
	calleeResultType := result.Type().(*irtypes.StructType)

	tag := c.block.NewExtractValue(result, 0)

	propagateBlock := c.newBlock("auto.propagate")
	okBlock := c.newBlock("auto.ok")
	c.block.NewCondBr(tag, propagateBlock, okBlock)

	// Error path: cleanup stmt temps + scope bindings, extract error, propagate
	c.block = propagateBlock
	c.emitAllStmtTempCleanupForErrorPath() // T0103/T1272: free all temp kinds before returning
	if len(c.scopeBindings) > 0 {
		c.emitScopeCleanup(0, true) // error in flight — suppress close errors
	}
	errVal := c.block.NewExtractValue(result, resultErrIdx(calleeResultType))
	if c.inFailableGoBlock {
		// T1384: store the error into the goroutine's result aggregate and branch
		// to the coroutine's final suspend (a `ret` is invalid in the coro ramp).
		c.emitFailableGoBlockError(errVal)
	} else if c.inGenerator && c.generatorCanError {
		// B0023: store error to generator error_slot and branch to final suspend
		c.emitGeneratorError(errVal)
	} else {
		callerResultType := c.currentResultType()
		c.block.NewRet(c.wrapError(errVal, callerResultType))
	}

	// Ok path: extract the success value
	c.block = okBlock
	if !isVoidResult(calleeResultType) {
		return c.block.NewExtractValue(result, 1)
	}
	return nil
}

// trackUnwrappedFailableTemp registers the unwrapped success value of a
// failable call as a statement-end temp so it is freed if not claimed by a
// variable. `expr` is the node whose c.info.Types entry holds the success
// type; `result` is the already-unwrapped value. Used by the explicit `?^`
// (ErrorPropagateExpr) and `?!` (ErrorPanicExpr) paths in genExpr, and by every
// bare auto-propagate site through genAutoPropagateTracked (T0966, T1883). §7.2
// of docs/language-design.md gives the bare form in "all expression positions",
// so all three spellings must register the same temp. Borrow returns (`T&`/`T~`)
// are never owned temps and are skipped.
//
// It deliberately does NOT consult `optionalFieldString`. That flag is a signal
// from genFieldAccess to the *immediately following* genOptionalForceUnwrap
// (B0190: `owner.opt_name!` aliases a field the owner's drop already frees).
// B0190 landed when one genExpr branch served both the optional unwrap and the
// failable unwrap, so the guard came along when the failable half was extracted
// here — but a failable call's success value is a fresh return, never an alias
// of a field on the receiver (a borrow return is `T&`/`T~` and returns above).
// Consulting the flag here only swallowed it: `f(owner.opt_name, mk()?^)` set the
// flag on the field read, and the unrelated `mk()` temp then skipped tracking and
// leaked. The Vector branch below never consulted the flag — this is the string
// branch catching up (T1883).
func (c *Compiler) trackUnwrappedFailableTemp(expr ast.Expr, result value.Value) {
	exprType := c.resolvedExprType(expr)
	if exprType != nil && isRefType(exprType) {
		return
	}
	if result != nil && result.Type() == irtypes.I8Ptr {
		named := extractNamed(exprType)
		if named == types.TypString {
			c.trackStringTemp(result)
		} else if named == types.TypVector {
			if elemType, ok := types.AsVector(exprType); ok {
				c.trackVectorTempWithElemType(result, elemType)
			} else {
				c.trackVectorTemp(result)
			}
		}
	} else {
		c.trackHeapUserTypeResult(expr, result)
	}
}

// genAutoPropagateTracked unwraps a bare failable call registered for implicit
// propagation and registers the unwrapped heap success value as a statement-end
// temp, so the implicit path emits exactly what the explicit `?^` path emits
// (T1883). Without the tracking the temp is never claimed nor dropped and leaks.
func (c *Compiler) genAutoPropagateTracked(expr ast.Expr, result value.Value) value.Value {
	val := c.genAutoPropagateValue(result)
	if val != nil {
		c.trackUnwrappedFailableTemp(expr, val)
	}
	return val
}

// genCallArgExpr generates an expression used as a call argument.
// If the expression is a failable call registered for auto-propagation,
// it extracts the success value (propagating the error on failure).
//
// T0331: A previous version of this function unconditionally claimed
// stmtTemps for vector/channel/arc/weak CallExpr args ("the callee takes
// ownership"). That assumption is wrong for plain (non-`~`) heap params
// on free functions and non-`new` methods — the callee borrows but doesn't
// drop, so claiming the temp leaked it. Per-call-site emitters explicitly
// claim where ownership actually transfers (~ params, variadic, container
// stores, constructors via genConstructorCallMono). The return-aliases-arg
// case is handled at runtime by emitReturnAliasCheck.
func (c *Compiler) genCallArgExpr(expr ast.Expr) value.Value {
	// T1395: a spliced parameter default belongs to the unit that declared it,
	// which may not be the unit being compiled — emit it against its own Info.
	if restore := c.useDeclaringInfo(expr); restore != nil {
		defer restore()
	}
	val := c.genExpr(expr)
	if c.info.AutoPropagateExprs[expr] {
		val = c.genAutoPropagateTracked(expr, val)
	}
	return val
}

// genExprAutoPropagate evaluates an expression and, if it is a failable
// call registered for auto-propagation, unwraps the result (propagating
// the error on failure). Used for sub-expression targets (field access,
// method receivers, index targets) where the failable tuple must be
// unwrapped before use. B0323, and the sole receiver path since B0322's
// genReceiverExpr — a byte-identical twin — was retired.
func (c *Compiler) genExprAutoPropagate(expr ast.Expr) value.Value {
	val := c.genExpr(expr)
	if c.info.AutoPropagateExprs[expr] {
		val = c.genAutoPropagateTracked(expr, val)
	}
	return val
}
