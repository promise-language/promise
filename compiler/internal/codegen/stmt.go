package codegen

import (
	"fmt"
	"math"
	"strconv"

	"github.com/llir/llvm/ir"
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
		c.emitCloseErrCheck(cap)
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
	if _, ok := types.AsTask(typ); ok || named == types.TypTask {
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
		if t == types.TypString || t == types.TypVector || t == types.TypChannel || t == types.TypTask {
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
			if n == types.TypVector || n == types.TypChannel || n == types.TypTask {
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
	// `Arc[T?].borrow`) is recognized as the optional-unwrap case below — its cast
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
	// Owned-return shapes: the local owns a fresh closure, keep its binding.
	if c.isGetterCallExpr(e) || c.isUserIndexExpr(e) {
		return false
	}
	return true
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
		return nil
	}
	savedScopeLen := len(c.scopeBindings)
	var result value.Value
	n := len(block.Stmts)
	for i, stmt := range block.Stmts {
		if c.block == nil || c.block.Term != nil {
			break
		}
		if i == n-1 {
			if es, ok := stmt.(*ast.ExprStmt); ok {
				if c.info.AutoPropagateExprs[es.Expr] {
					// Failable call: auto-propagate error, discard success value.
					// Block arms don't contribute typed results to match phis;
					// only expression arms (arm.Body) produce match result values.
					c.genAutoPropagate(es.Expr)
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
						c.clearDropFlag(ident.Name)
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
				break
			}
			// B0126: Handle if/else as the last statement in a block that
			// produces a value. The parser emits IfStmt (not IfExpr) in
			// statement position, but we need to capture the value from
			// both branches when the block is used as an expression.
			if ifS, ok := stmt.(*ast.IfStmt); ok {
				result = c.genIfStmtValue(ifS)
				break
			}
		}
		c.genStmt(stmt)
	}
	if c.block != nil && c.block.Term == nil && len(c.scopeBindings) > savedScopeLen {
		cap := c.emitScopeCleanup(savedScopeLen, false)
		c.emitCloseErrCheck(cap)
	}
	c.scopeBindings = c.scopeBindings[:savedScopeLen]
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
			discardedResult = c.genExpr(s.Expr)
		}
		c.goExprFireAndForget = false
		// B0196/B0208: When a discarded expression returns an Optional with a
		// droppable inner type, the inner value leaks because trackStringTemp
		// only tracks bare i8* values, not {i1, T} optional structs.
		c.dropDiscardedOptional(s.Expr, discardedResult)
		// B0211: When a discarded expression returns a heap-allocated user type
		// (e.g., bare constructor call like `Foo(x: 1);`), free the instance.
		c.dropDiscardedHeapType(s.Expr, discardedResult)
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
	if c.block != nil && c.block.Term == nil {
		c.cleanupStmtTemps()
		c.cleanupHeapTemps()
		c.cleanupEnvTemps() // T0100
		// B0267/B0269: Drop all inline enum constructor temps not consumed by a variable.
		for _, et := range c.enumCtorTemps {
			flag := c.block.NewLoad(irtypes.I1, et.dropFlag)
			dropBlk := c.newBlock("enum.ctor.drop")
			skipBlk := c.newBlock("enum.ctor.skip")
			c.block.NewCondBr(flag, dropBlk, skipBlk)
			c.block = dropBlk
			ptr := c.block.NewLoad(irtypes.I8Ptr, et.alloca)
			c.block.NewCall(et.dropFunc, ptr)
			c.block.NewBr(skipBlk)
			c.block = skipBlk
		}
		c.enumCtorTemps = c.enumCtorTemps[:0]
	}
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
	c.emitStmtTempCleanupForErrorPath() // T0103: free string temps before returning
	c.emitHeapTempCleanupForErrorPath() // T0103: free heap temps before returning
	if len(c.scopeBindings) > 0 {
		c.emitScopeCleanup(0, true) // error in flight — suppress close errors
	}
	errVal := c.block.NewExtractValue(result, resultErrIdx(calleeResultType))
	if c.inGenerator && c.generatorCanError {
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
	c.emitStmtTempCleanupForErrorPath() // T0103: free string temps before returning
	c.emitHeapTempCleanupForErrorPath() // T0103: free heap temps before returning
	if len(c.scopeBindings) > 0 {
		c.emitScopeCleanup(0, true) // error in flight — suppress close errors
	}
	errVal := c.block.NewExtractValue(result, resultErrIdx(calleeResultType))
	if c.inGenerator && c.generatorCanError {
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

// genReceiverExpr generates an expression used as a method receiver or member access target.
// If the expression is a failable call registered for auto-propagation (B0322),
// it extracts the success value (propagating the error on failure).
func (c *Compiler) genReceiverExpr(expr ast.Expr) value.Value {
	val := c.genExpr(expr)
	if c.info.AutoPropagateExprs[expr] {
		val = c.genAutoPropagateValue(val)
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
	val := c.genExpr(expr)
	if c.info.AutoPropagateExprs[expr] {
		val = c.genAutoPropagateValue(val)
	}
	return val
}

// genExprAutoPropagate evaluates an expression and, if it is a failable
// call registered for auto-propagation, unwraps the result (propagating
// the error on failure). Used for sub-expression targets (field access,
// method receivers, index targets) where the failable tuple must be
// unwrapped before use. B0323.
func (c *Compiler) genExprAutoPropagate(expr ast.Expr) value.Value {
	val := c.genExpr(expr)
	if c.info.AutoPropagateExprs[expr] {
		val = c.genAutoPropagateValue(val)
	}
	return val
}

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
	// T0440: Same flag for typed `T? b = m[k]` — Optional[heap-user-type] LHS
	// where the inner value aliases the container's bucket. Set the flag so
	// genMethodIndex deep-clones via cloneHeapElement.
	if opt, ok := resolvedExprType.(*types.Optional); ok {
		elem := opt.Elem()
		if c.typeSubst != nil {
			elem = types.Substitute(elem, c.typeSubst)
		}
		if isDroppableHeapUserType(elem) || isHeapUserNoDropPalFree(elem) {
			c.dupHeapUserFieldAccess = true
		}
	}
	val := c.genExpr(s.Value)
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

	// T0111: Claim string temp BEFORE optional wrapping. After wrapOptional, the
	// value identity changes and claimStringTemp can't find the tracked temp.
	// T0555: Also claim native handle / container temps before the wrap.
	// Without this, the post-wrap claim site (which uses the wrapped struct)
	// cannot locate the tracked i8* temp, so the stmt-temp drop AND the
	// optional binding drop both fire → double-free.
	if declType != nil {
		if _, isOpt := declType.(*types.Optional); isOpt {
			if exprType != nil {
				if extractNamed(exprType) == types.TypString ||
					types.IsVector(exprType) || types.IsChannel(exprType) ||
					types.IsArc(exprType) || types.IsWeak(exprType) ||
					types.IsMutex(exprType) || types.IsTask(exprType) ||
					types.IsMutexGuard(exprType) {
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
			// T0856: A borrowed optional (`T?&`/`T?~`, e.g. `Arc[T?]`/
			// `Mutex[T?].borrow` with a value/Copy payload) auto-copies to a
			// bare optional value at the borrow site — genArcBorrow/
			// genMutexGuardBorrow load and return the full {i1,T} struct. The
			// recorded exprType is still the ref-to-optional, so strip the ref
			// before the wrap comparison; otherwise the already-optional value
			// is spuriously re-wrapped (insertvalue elem-type-mismatch panic).
			switch ref := cmpExprType.(type) {
			case *types.SharedRef:
				cmpExprType = ref.Elem()
			case *types.MutRef:
				cmpExprType = ref.Elem()
			}
			if _, isNone := cmpExprType.(*types.Named); isNone && cmpExprType == types.TypNone {
				// NoneLit already handled via targetType
			} else if !types.Identical(cmpExprType, declType) {
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
		if cn := extractNamed(coerceTarget); cn != nil && cn.IsStructural() {
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
		types.IsMutex(resolvedExprType) || types.IsTask(resolvedExprType) ||
		types.IsMutexGuard(resolvedExprType)) {
		c.claimStringTemp(val)
	}
	// T0088: Claim heap temp — ownership transferred to this variable.
	c.claimHeapTemp(val)
	// B0267: Clear enum temps when the variable IS the enum (not a function result).
	if len(c.enumCtorTemps) > 0 && extractEnum(resolvedExprType) != nil {
		for i := range c.enumCtorTemps {
			c.block.NewStore(constant.NewInt(irtypes.I1, 0), c.enumCtorTemps[i].dropFlag)
		}
		c.enumCtorTemps = c.enumCtorTemps[:0]
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
		if n := extractNamed(resolvedDeclType); n != nil && n.IsStructural() {
			c.promoteHeapTempsToScope()
		}
	}
	// T0100: Claim env temp — the variable's scope binding handles env free.
	c.claimEnvTemp(val)

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
	// T0111: When RHS is opt!, neutralize the source optional (set present=false)
	// so its drop doesn't double-free the inner value now owned by this variable.
	c.neutralizeForceUnwrapSource(s.Value)
	// T0127: Register bindingFree for structural interface variables owning a heap allocation.
	c.maybeRegisterStructuralFree(s.Name, alloca, dropType, s.Value)
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
	}
	// T0747: a user-type RTTI cast of a borrow (`d := x as!/as T`) is a
	// non-consuming view — the subject keeps ownership. Clear the LHS drop flag
	// so the cast local doesn't double-free the aliased instance at scope exit.
	if c.isRttiCastBorrow(s.Value) {
		c.clearDropFlag(s.Name)
	}
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
	// T0440: Same flag for inferred `b := m[k]` — Optional[heap-user-type] LHS
	// where the inner value aliases the container's bucket. Set the flag so
	// genMethodIndex deep-clones via cloneHeapElement.
	if opt, ok := typ.(*types.Optional); ok {
		elem := opt.Elem()
		if c.typeSubst != nil {
			elem = types.Substitute(elem, c.typeSubst)
		}
		// T0903: include no-drop heap user inner (analog of the bare-type branch).
		if isDroppableHeapUserType(elem) || isHeapUserNoDropPalFree(elem) {
			c.dupHeapUserFieldAccess = true
		}
	}
	val := c.genExpr(s.Value)
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
	if n := extractNamed(typ); n != nil && n.IsStructural() {
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
		types.IsMutex(typ) || types.IsTask(typ) || types.IsMutexGuard(typ) {
		c.claimStringTemp(val)
	}
	// B0175: Claim heap temp — ownership transferred to this variable.
	// Without this, iterator chain results (e.g., c.take(3)) assigned via
	// auto-typed declarations are freed at statement end, causing use-after-free.
	c.claimHeapTemp(val)
	// B0267: Clear enum temps when the variable IS the enum (not a function result).
	if len(c.enumCtorTemps) > 0 && extractEnum(typ) != nil {
		for i := range c.enumCtorTemps {
			c.block.NewStore(constant.NewInt(irtypes.I1, 0), c.enumCtorTemps[i].dropFlag)
		}
		c.enumCtorTemps = c.enumCtorTemps[:0]
	}
	// B0222: When storing a structural interface (e.g., Iterator) in a variable,
	// promote remaining heapTemps to scope bindings so intermediate iterators in
	// generic combinator chains survive until scope exit.
	if len(c.heapTemps) > 0 {
		if n := extractNamed(typ); n != nil && n.IsStructural() {
			c.promoteHeapTempsToScope()
		}
	}
	// T0100: Claim env temp — the variable's scope binding handles env free.
	c.claimEnvTemp(val)

	c.block.NewStore(val, alloca)
	c.locals[s.Name] = alloca
	c.maybeRegisterDrop(s.Name, alloca, typ)
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
	c.maybeRegisterStructuralFree(s.Name, alloca, typ, s.Value)
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
	}
	// T0747: a user-type RTTI cast of a borrow (`d := x as!/as T`) is a
	// non-consuming view — the subject keeps ownership. Clear the LHS drop flag
	// so the cast local doesn't double-free the aliased instance at scope exit.
	if c.isRttiCastBorrow(s.Value) {
		c.clearDropFlag(s.Name)
	}
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
		srcOwned = false
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
			c.maybeRegisterDrop(bindKey, alloca, elemPromiseType)
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

// --- drop binding ---

// isTypeDroppable returns true if maybeRegisterDrop would register a drop binding
// for a variable of this type. Used by the return-alias check (B0345) to decide
// whether a return value could alias a droppable argument.
func isTypeDroppable(typ types.Type) bool {
	if enum := extractEnum(typ); enum != nil {
		if enum.HasDrop() {
			return true
		}
		if inst, ok := typ.(*types.Instance); ok && monoEnumInstNeedsSynthDrop(inst) {
			return true
		}
		return false
	}
	// T0371: Tuples with droppable fields require a tuple-walk drop binding.
	// Recurse into fields — pure-value tuples (int, bool, etc.) are not droppable.
	if tup, ok := typ.(*types.Tuple); ok {
		for _, e := range tup.Elems() {
			if isTypeDroppable(e) {
				return true
			}
		}
		return false
	}
	// T0391: Optional[T] is droppable iff T is droppable. Recurse so the return-alias
	// check sees through any number of Optional wrappings to reach the inner type.
	if opt, ok := typ.(*types.Optional); ok {
		return isTypeDroppable(opt.Elem())
	}
	named := extractNamed(typ)
	if named == nil {
		return false
	}
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
	if isContainerType(typ) {
		return false
	}
	if named.HasDrop() || named.NeedsSynthDrop() {
		return true
	}
	if inst, ok := typ.(*types.Instance); ok && monoInstNeedsSynthDrop(inst) {
		return true
	}
	if !named.IsValueType() && !named.IsCopy() && !isPrimitiveScalar(named) && !named.IsStructural() {
		return true
	}
	return false
}

// emitReturnAliasCheck generates runtime pointer comparisons between a function call's
// return value and its non-Copy ident arguments. If the return pointer aliases an
// argument, the argument's drop flag is cleared to prevent double-free (B0345).
//
// Without this check, identity(v) where v is a heap string causes SIGABRT:
// the caller has drop flags for both v and the return value s, but they point
// to the same memory — both get freed at scope exit.
func (c *Compiler) emitReturnAliasCheck(result value.Value, sig *types.Signature, args []*ast.Arg, argVals []value.Value) {
	c.emitReturnAliasCheckSubst(result, sig, args, argVals, nil)
}

// emitReturnAliasCheckSubst is the generic-aware variant. T0418: callSubst maps
// the callee's TypeParams to the call's concrete type args so droppability
// checks see through TypeParams (e.g., T? → _Box? → droppable).
func (c *Compiler) emitReturnAliasCheckSubst(result value.Value, sig *types.Signature, args []*ast.Arg, argVals []value.Value, callSubst map[*types.TypeParam]types.Type) {
	if result == nil || sig == nil {
		return
	}
	retType := sig.Result()
	if retType == nil {
		return
	}
	if callSubst != nil {
		retType = types.Substitute(retType, callSubst)
	}
	if c.typeSubst != nil {
		retType = types.Substitute(retType, c.typeSubst)
	}
	// Only check for non-Copy return types that could alias.
	if !isTypeDroppable(retType) {
		return
	}
	// Skip failable returns — the raw result is {i1, value, err_ptr}, not the value itself.
	if sig.CanError() {
		return
	}

	params := sig.Params()
	for i, arg := range args {
		if i >= len(argVals) || i >= len(params) {
			break
		}
		// Skip ~, &, and variadic params — ~ already clears flag, & is a borrow,
		// variadic has separate handling.
		p := params[i]
		if p.Ref() == types.RefMut || p.IsVariadic() {
			continue
		}
		if _, isSR := p.Type().(*types.SharedRef); isSR {
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
				continue
			}

			// Generate: if retPtr == argPtr { clear dropFlag }
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

// maybeRegisterDrop checks if a variable's type has a drop() method and, if so,
// registers a drop binding: allocates a drop flag (i1, initially true), resolves
// the drop function, and appends a scopeBinding.
// Strings are special: they use promise_string_drop (checks literal flag before freeing).
func (c *Compiler) maybeRegisterDrop(varName string, alloca *ir.InstAlloca, typ types.Type) {
	// T0381: A ref-typed local (`T&`/`T~`) starts life borrowing from the
	// owner — drop is cleared at the assignment site. But the same local
	// can later be reassigned to an owned `T` (decay rule), at which point
	// it owns the new value and must drop on scope exit. Register the
	// binding using the underlying owned type so the drop machinery emits
	// a proper drop (e.g., per-element string drops for `string[]`) when
	// the runtime dropflag is true.
	if sr, ok := typ.(*types.SharedRef); ok {
		typ = sr.Elem()
	}
	if mr, ok := typ.(*types.MutRef); ok {
		typ = mr.Elem()
	}
	// T0102: Enum drop — check before extractNamed since enums are *types.Enum, not *types.Named.
	if enum := extractEnum(typ); enum != nil {
		if enum.HasDrop() {
			c.maybeRegisterEnumDrop(varName, alloca, typ, enum)
			return
		}
		// B0238: Check for mono-time synthesized drops on generic enum instances
		// whose TypeParam variant fields resolve to droppable concrete types.
		if inst, ok := typ.(*types.Instance); ok && monoEnumInstNeedsSynthDrop(inst) {
			c.maybeRegisterEnumDrop(varName, alloca, typ, enum)
			return
		}
	}

	// T0585: Optional drop — delegate to maybeRegisterOptionalDrop so callers
	// passing Optional types (notably `~T?` consume params via defineFunc) get
	// a drop flag and binding consistent with other owned values. Without this,
	// `~T?` params had no flag, which both leaked when not consumed and broke
	// borrowed-vs-owned discrimination in the T0585 wrap propagation.
	if opt, ok := typ.(*types.Optional); ok {
		c.maybeRegisterOptionalDrop(varName, alloca, opt)
		return
	}

	// T0371: Tuple value with droppable fields — register a bindingDropTuple
	// that walks fields and drops each droppable one at scope exit.
	// Tuples are stored in struct allocas (not i8*), so we use a dedicated kind.
	if _, ok := typ.(*types.Tuple); ok {
		if !c.tupleNeedsDrop(typ) {
			return
		}
		dropFlag := c.createEntryAlloca(irtypes.I1)
		dropFlag.SetName(c.uniqueLocalName(varName + ".dropflag"))
		c.block.NewStore(constant.NewInt(irtypes.I1, 1), dropFlag)
		c.dropFlags[varName] = dropFlag
		binding := scopeBinding{
			kind:     bindingDropTuple,
			alloca:   alloca,
			valType:  typ,
			dropFlag: dropFlag,
			varName:  varName,
		}
		c.scopeBindings = append(c.scopeBindings, binding)
		c.dropBindings[varName] = binding
		return
	}

	// T0389: Fixed-size array with droppable element type — register a
	// bindingDropArray that walks elements and drops each droppable one at
	// scope exit. Arrays are stored in [N x T] allocas.
	if arr, ok := typ.(*types.Array); ok {
		elemType := arr.Elem()
		if c.typeSubst != nil {
			elemType = types.Substitute(elemType, c.typeSubst)
		}
		if !c.variantFieldNeedsDrop(elemType) {
			return
		}
		dropFlag := c.createEntryAlloca(irtypes.I1)
		dropFlag.SetName(c.uniqueLocalName(varName + ".dropflag"))
		c.block.NewStore(constant.NewInt(irtypes.I1, 1), dropFlag)
		c.dropFlags[varName] = dropFlag
		binding := scopeBinding{
			kind:     bindingDropArray,
			alloca:   alloca,
			valType:  typ,
			dropFlag: dropFlag,
			varName:  varName,
		}
		c.scopeBindings = append(c.scopeBindings, binding)
		c.dropBindings[varName] = binding
		return
	}

	named := extractNamed(typ)
	if named == nil {
		return
	}

	// String drop: register bindingDropString with promise_string_drop.
	// The drop flag is cleared at all move sites (return, assignment, constructor,
	// function call args) via clearDropFlag. Strings passed to functions have their
	// flag cleared (callee conceptually borrows/takes ownership), so they won't be
	// freed at scope exit. Strings that are NOT passed to functions are freed.
	if named == types.TypString {
		dropFlag := c.createEntryAlloca(irtypes.I1)
		dropFlag.SetName(c.uniqueLocalName(varName + ".dropflag"))
		c.block.NewStore(constant.NewInt(irtypes.I1, 1), dropFlag)
		c.dropFlags[varName] = dropFlag

		dropFunc := c.funcs["promise_string_drop"]
		binding := scopeBinding{
			kind:     bindingDropString,
			alloca:   alloca,
			named:    named,
			valType:  typ,
			dropFlag: dropFlag,
			dropFunc: dropFunc,
			varName:  varName,
		}
		c.scopeBindings = append(c.scopeBindings, binding)
		c.dropBindings[varName] = binding
		return
	}

	// Vector drop: register bindingDropString (same mechanism — i8* alloca + void(i8*) drop).
	// Vector.drop null-checks and frees the heap buffer. Drop flag semantics match strings:
	// cleared at all move sites, borrow detection skips drops for container element access.
	if elemType, ok := types.AsVector(typ); ok || named == types.TypVector {
		_ = elemType // B0245: elemType is available when typ is an Instance
		dropFlag := c.createEntryAlloca(irtypes.I1)
		dropFlag.SetName(c.uniqueLocalName(varName + ".dropflag"))
		c.block.NewStore(constant.NewInt(irtypes.I1, 1), dropFlag)
		c.dropFlags[varName] = dropFlag

		dropFunc := c.funcs["Vector.drop"]
		binding := scopeBinding{
			kind:     bindingDropString, // reuse: same i8* alloca + void(i8*) drop pattern
			alloca:   alloca,
			named:    named,
			valType:  typ,
			dropFlag: dropFlag,
			dropFunc: dropFunc,
			varName:  varName,
		}
		c.scopeBindings = append(c.scopeBindings, binding)
		c.dropBindings[varName] = binding
		return
	}

	// Channel drop (B0163): same i8* alloca + void(i8*) drop pattern as string/vector.
	// T0663: Channel[T].drop is per-element-type — it drops any un-received
	// buffered items before freeing the ring buffer, mutex, cond vars, and the
	// struct itself. Drop flag semantics handle moves: cleared when the channel
	// is passed to go blocks or functions, so only the last owner frees.
	if elemType, ok := types.AsChannel(typ); ok || named == types.TypChannel {
		resolvedElem := elemType
		if resolvedElem == nil && named == types.TypChannel && c.typeSubst != nil {
			if tp := types.TypChannel.TypeParams(); len(tp) > 0 {
				resolvedElem = c.typeSubst[tp[0]]
			}
		}
		if c.typeSubst != nil && resolvedElem != nil {
			resolvedElem = types.Substitute(resolvedElem, c.typeSubst)
		}
		if resolvedElem != nil {
			dropFlag := c.createEntryAlloca(irtypes.I1)
			dropFlag.SetName(c.uniqueLocalName(varName + ".dropflag"))
			c.block.NewStore(constant.NewInt(irtypes.I1, 1), dropFlag)
			c.dropFlags[varName] = dropFlag

			dropFunc := c.getOrCreateChannelDrop(resolvedElem)
			binding := scopeBinding{
				kind:     bindingDropString, // reuse: same i8* alloca + void(i8*) drop pattern
				alloca:   alloca,
				named:    named,
				valType:  typ,
				dropFlag: dropFlag,
				dropFunc: dropFunc,
				varName:  varName,
			}
			c.scopeBindings = append(c.scopeBindings, binding)
			c.dropBindings[varName] = binding
			return
		}
	}

	// Arc drop (T0155): same i8* alloca + void(i8*) drop pattern as string/vector/channel.
	// Arc.drop atomically decrements the refcount and frees when it reaches zero.
	if elemType, ok := types.AsArc(typ); ok || named == types.TypArc {
		resolvedElem := elemType
		if resolvedElem == nil && named == types.TypArc && c.typeSubst != nil {
			if tp := types.TypArc.TypeParams(); len(tp) > 0 {
				resolvedElem = c.typeSubst[tp[0]]
			}
		}
		if c.typeSubst != nil && resolvedElem != nil {
			resolvedElem = types.Substitute(resolvedElem, c.typeSubst)
		}
		if resolvedElem != nil {
			dropFlag := c.createEntryAlloca(irtypes.I1)
			dropFlag.SetName(c.uniqueLocalName(varName + ".dropflag"))
			c.block.NewStore(constant.NewInt(irtypes.I1, 1), dropFlag)
			c.dropFlags[varName] = dropFlag

			dropFunc := c.getOrCreateArcDrop(resolvedElem)
			binding := scopeBinding{
				kind:     bindingDropString, // reuse: same i8* alloca + void(i8*) drop pattern
				alloca:   alloca,
				named:    named,
				valType:  typ,
				dropFlag: dropFlag,
				dropFunc: dropFunc,
				varName:  varName,
			}
			c.scopeBindings = append(c.scopeBindings, binding)
			c.dropBindings[varName] = binding
			return
		}
	}

	// Weak drop (T0157): per-instantiation drop (decrements weak_count, frees when zero).
	if elemType, ok := types.AsWeak(typ); ok || named == types.TypWeak {
		resolvedElem := elemType
		if resolvedElem == nil && named == types.TypWeak && c.typeSubst != nil {
			if tp := types.TypWeak.TypeParams(); len(tp) > 0 {
				resolvedElem = c.typeSubst[tp[0]]
			}
		}
		if c.typeSubst != nil && resolvedElem != nil {
			resolvedElem = types.Substitute(resolvedElem, c.typeSubst)
		}
		if resolvedElem != nil {
			dropFlag := c.createEntryAlloca(irtypes.I1)
			dropFlag.SetName(c.uniqueLocalName(varName + ".dropflag"))
			c.block.NewStore(constant.NewInt(irtypes.I1, 1), dropFlag)
			c.dropFlags[varName] = dropFlag

			dropFunc := c.getOrCreateWeakDrop(resolvedElem)
			binding := scopeBinding{
				kind:     bindingDropString, // reuse: same i8* alloca + void(i8*) drop pattern
				alloca:   alloca,
				named:    named,
				valType:  typ,
				dropFlag: dropFlag,
				dropFunc: dropFunc,
				varName:  varName,
			}
			c.scopeBindings = append(c.scopeBindings, binding)
			c.dropBindings[varName] = binding
			return
		}
	}

	// Mutex drop (T0156): per-instantiation drop (drops inner T, destroys PAL mutex, frees).
	if elemType, ok := types.AsMutex(typ); ok || named == types.TypMutex {
		resolvedElem := elemType
		if resolvedElem == nil && named == types.TypMutex && c.typeSubst != nil {
			if tp := types.TypMutex.TypeParams(); len(tp) > 0 {
				resolvedElem = c.typeSubst[tp[0]]
			}
		}
		if c.typeSubst != nil && resolvedElem != nil {
			resolvedElem = types.Substitute(resolvedElem, c.typeSubst)
		}
		if resolvedElem != nil {
			dropFlag := c.createEntryAlloca(irtypes.I1)
			dropFlag.SetName(c.uniqueLocalName(varName + ".dropflag"))
			c.block.NewStore(constant.NewInt(irtypes.I1, 1), dropFlag)
			c.dropFlags[varName] = dropFlag

			dropFunc := c.getOrCreateMutexDrop(resolvedElem)
			binding := scopeBinding{
				kind:     bindingDropString,
				alloca:   alloca,
				named:    named,
				valType:  typ,
				dropFlag: dropFlag,
				dropFunc: dropFunc,
				varName:  varName,
			}
			c.scopeBindings = append(c.scopeBindings, binding)
			c.dropBindings[varName] = binding
			return
		}
	}

	// MutexGuard drop (T0156): T-independent drop (unlock + free guard).
	if _, ok := types.AsMutexGuard(typ); ok || named == types.TypMutexGuard {
		dropFlag := c.createEntryAlloca(irtypes.I1)
		dropFlag.SetName(c.uniqueLocalName(varName + ".dropflag"))
		c.block.NewStore(constant.NewInt(irtypes.I1, 1), dropFlag)
		c.dropFlags[varName] = dropFlag

		dropFunc := c.funcs["MutexGuard.drop"]
		binding := scopeBinding{
			kind:     bindingDropString,
			alloca:   alloca,
			named:    named,
			valType:  typ,
			dropFlag: dropFlag,
			dropFunc: dropFunc,
			varName:  varName,
		}
		c.scopeBindings = append(c.scopeBindings, binding)
		c.dropBindings[varName] = binding
		return
	}

	// Task drop (T0503): per-instantiation drop blocks until the goroutine
	// finishes, drops the result T (if any), then frees result_ptr/panic_msg/G.
	// Without this, `task[T] t = go fn();` leaks the G struct, the result_ptr
	// buffer, and any droppable result value when t is never awaited via <-t.
	if elemType, ok := types.AsTask(typ); ok || named == types.TypTask {
		resolvedElem := elemType
		if resolvedElem == nil && named == types.TypTask && c.typeSubst != nil {
			if tp := types.TypTask.TypeParams(); len(tp) > 0 {
				resolvedElem = c.typeSubst[tp[0]]
			}
		}
		if c.typeSubst != nil && resolvedElem != nil {
			resolvedElem = types.Substitute(resolvedElem, c.typeSubst)
		}
		// resolvedElem may be nil (task[void]) — getOrCreateTaskDrop handles that.
		dropFlag := c.createEntryAlloca(irtypes.I1)
		dropFlag.SetName(c.uniqueLocalName(varName + ".dropflag"))
		c.block.NewStore(constant.NewInt(irtypes.I1, 1), dropFlag)
		c.dropFlags[varName] = dropFlag

		dropFunc := c.getOrCreateTaskDrop(resolvedElem)
		binding := scopeBinding{
			kind:     bindingDropString, // reuse: same i8* alloca + void(i8*) drop pattern
			alloca:   alloca,
			named:    named,
			valType:  typ,
			dropFlag: dropFlag,
			dropFunc: dropFunc,
			varName:  varName,
		}
		c.scopeBindings = append(c.scopeBindings, binding)
		c.dropBindings[varName] = binding
		return
	}

	// Remaining container types without drop support skip.
	if isContainerType(typ) {
		return
	}

	if !named.HasDrop() {
		// B0202: Check if this is a mono instance with a synthesized drop
		// detected at codegen time (TypeParam fields → droppable concrete types).
		// Use monoInstNeedsSynthDrop to precisely match only B0202 instances,
		// not instances that already have drops via other paths.
		if inst, ok := typ.(*types.Instance); ok && monoInstNeedsSynthDrop(inst) {
			monoDropName := mangleMethodName(monoName(inst), "drop", false)
			if dropFn, exists := c.funcs[monoDropName]; exists {
				dropFlag := c.createEntryAlloca(irtypes.I1)
				dropFlag.SetName(c.uniqueLocalName(varName + ".dropflag"))
				c.block.NewStore(constant.NewInt(irtypes.I1, 1), dropFlag)
				c.dropFlags[varName] = dropFlag
				binding := scopeBinding{
					kind:          bindingDrop,
					alloca:        alloca,
					named:         named,
					valType:       typ,
					dropFlag:      dropFlag,
					dropFunc:      dropFn,
					varName:       varName,
					monoSynthDrop: true,
				}
				c.scopeBindings = append(c.scopeBindings, binding)
				c.dropBindings[varName] = binding
				return
			}
		}

		// B0164: Heap user types without drop methods still need pal_free at scope exit.
		// Types that are value types, copy types, or primitive scalars don't heap-allocate.
		// Only register for allocas that store value structs ({i8*, i8*}), not raw i8*
		// pointers (method receivers, captures, etc.) which would crash extractInstancePtr.
		// Only for types with value struct allocas (not raw i8* method receivers/captures),
		// excluding structural interfaces (their instance ptr may be a stack alloca, not heap).
		_, isStructAlloca := alloca.ElemType.(*irtypes.StructType)
		if isStructAlloca && !named.IsValueType() && !named.IsCopy() && !isPrimitiveScalar(named) && !named.IsStructural() {
			dropFlag := c.createEntryAlloca(irtypes.I1)
			dropFlag.SetName(c.uniqueLocalName(varName + ".dropflag"))
			c.block.NewStore(constant.NewInt(irtypes.I1, 1), dropFlag)
			c.dropFlags[varName] = dropFlag

			binding := scopeBinding{
				kind:     bindingFree,
				alloca:   alloca,
				named:    named,
				valType:  typ,
				dropFlag: dropFlag,
				varName:  varName,
				// T0917: Polymorphic heap types (abstract base / has children) may hold
				// a concrete subtype clone (from clone-on-`return this`, T0387/T0893) with
				// its own droppable fields. Dispatch the free through RTTI typeinfo
				// drop_fn_ptr so the concrete drop frees those fields, not a bare pal_free
				// that would leak them. Non-polymorphic leaves keep pal_free.
				rttiDrop: c.needsVtable(named),
			}
			c.scopeBindings = append(c.scopeBindings, binding)
			c.dropBindings[varName] = binding
		}
		return
	}

	// Allocate drop flag: i1, initialized to true (should drop).
	// Use entry-block alloca to avoid stack growth in loops.
	dropFlag := c.createEntryAlloca(irtypes.I1)
	dropFlag.SetName(c.uniqueLocalName(varName + ".dropflag"))
	c.block.NewStore(constant.NewInt(irtypes.I1, 1), dropFlag)
	c.dropFlags[varName] = dropFlag

	binding := scopeBinding{
		kind:     bindingDrop,
		alloca:   alloca,
		named:    named,
		valType:  typ,
		dropFlag: dropFlag,
		varName:  varName,
	}

	// Resolve drop function for direct dispatch.
	// Synthesized drops (B0158) always use direct dispatch — they're not in the vtable.
	dropMethod := named.LookupMethod("drop")
	if named.NeedsSynthDrop() || !c.needsVtable(named) || (dropMethod != nil && dropMethod.IsNative()) {
		// For mono instances (e.g., Wrapper[int]), use the mono-qualified name
		// (Wrapper[int].drop), not the origin name (Wrapper.drop).
		// In mono method bodies, type args may contain TypeParams — substitute
		// with c.typeSubst to get the concrete instance name.
		resolvedTyp := typ
		if c.typeSubst != nil {
			resolvedTyp = types.Substitute(typ, c.typeSubst)
		}
		ownerName := named.Obj().Name()
		if inst, ok := resolvedTyp.(*types.Instance); ok {
			ownerName = monoName(inst)
		} else if !named.NeedsSynthDrop() {
			ownerName = c.resolveDropOwner(named)
		}
		mangledName := mangleMethodName(ownerName, "drop", false)
		if fn, ok := c.funcs[mangledName]; ok {
			binding.dropFunc = fn
		}
	}

	c.scopeBindings = append(c.scopeBindings, binding)
	c.dropBindings[varName] = binding
}

// maybeRegisterStructuralFree registers a bindingFree for structural interface variables
// whose backing instance is heap-allocated from a call/constructor (T0127).
// Structural types are excluded from maybeRegisterDrop (their instance ptr could be a
// borrow from a concrete variable). This method is called only when the RHS is NOT a
// simple identifier, meaning the value comes from a fresh allocation (e.g., vec.iter(),
// iter.map(f)) and the variable owns the backing instance.
func (c *Compiler) maybeRegisterStructuralFree(varName string, alloca *ir.InstAlloca, typ types.Type, rhs ast.Expr) {
	// Only for structural interface types without an existing drop binding.
	if _, hasBinding := c.dropBindings[varName]; hasBinding {
		return
	}
	named := extractNamed(typ)
	if named == nil || !named.IsStructural() || named.IsValueType() {
		return
	}
	// Only register when the RHS produces a fresh heap allocation the variable
	// owns. Call expressions (e.g., vec.iter(), iter.map(f)) qualify; so do
	// overloaded operator expressions (e.g., `-it`, `a + b`) — a structural
	// result type can only come from a user operator method, which returns an
	// owned value (T0893: clone-on-`return this` makes operator results owned
	// allocations that must be freed here). Other RHS expressions — identifiers
	// (borrow from existing variable), literals (value types, no heap alloc),
	// member access (borrow) — should NOT get a free binding.
	// B0272: Unwrap error-handling wrappers (!, ^, ? {}) to find the inner call expression.
	// Without this, failable structural interface returns leak their backing instance.
	innerRHS := rhs
	for {
		switch e := innerRHS.(type) {
		case *ast.ErrorPanicExpr:
			innerRHS = e.Expr
			continue
		case *ast.OptionalUnwrapExpr:
			innerRHS = e.Expr
			continue
		case *ast.ErrorPropagateExpr:
			innerRHS = e.Expr
			continue
		case *ast.ErrorHandlerExpr:
			innerRHS = e.Expr
			continue
		}
		break
	}
	switch innerRHS.(type) {
	case *ast.CallExpr, *ast.UnaryExpr, *ast.BinaryExpr:
		// owned heap allocation — register the free binding below
	default:
		return
	}
	// Must be a struct alloca ({i8* vtable, i8* instance}) to extract instance ptr.
	if _, ok := alloca.ElemType.(*irtypes.StructType); !ok {
		return
	}

	dropFlag := c.createEntryAlloca(irtypes.I1)
	dropFlag.SetName(c.uniqueLocalName(varName + ".dropflag"))
	c.block.NewStore(constant.NewInt(irtypes.I1, 1), dropFlag)
	c.dropFlags[varName] = dropFlag

	// B0272: Use RTTI-based drop dispatch when there's no specific cleanup function,
	// or when the claimed dropFunc is just pal_free (generic heap free that doesn't
	// drop instance fields like strings). Iterator cleanup functions (e.g.,
	// __promise_iter_cleanup) ARE proper cleanup — they handle _FnIter instances
	// that don't have RTTI layout. pal_free-claimed instances DO have RTTI layout
	// (they're standard user type instances from constructors).
	claimedDrop := c.lastClaimedDropFunc
	useRTTI := claimedDrop == nil || claimedDrop == c.palFree
	if useRTTI {
		claimedDrop = nil // don't use pal_free directly — RTTI dispatch handles it
	}
	binding := scopeBinding{
		kind:     bindingFree,
		alloca:   alloca,
		named:    named,
		valType:  typ,
		dropFlag: dropFlag,
		dropFunc: claimedDrop, // T0127: use iter cleanup when available
		varName:  varName,
		rttiDrop: useRTTI, // B0272: RTTI-based drop for instances with standard layout
	}
	c.scopeBindings = append(c.scopeBindings, binding)
	c.dropBindings[varName] = binding
}

// maybeRegisterStructuralParamFree registers an RTTI-dispatched free binding for an
// owned (~) structural-interface view parameter (T0861). Unlike local structural
// variables (maybeRegisterStructuralFree), an owned param is always a genuine ownership
// transfer — the caller cleared its source drop flag at the move site — so the callee
// must drop the backing concrete instance at scope exit. Uses RTTI drop dispatch
// (typeinfo.drop_fn_ptr via __promise_structural_drop) so concrete field cleanup
// (e.g. string fields) runs before the instance is freed.
func (c *Compiler) maybeRegisterStructuralParamFree(varName string, alloca *ir.InstAlloca, typ types.Type) {
	if _, hasBinding := c.dropBindings[varName]; hasBinding {
		return
	}
	named := extractNamed(typ)
	if named == nil || !named.IsStructural() || named.IsValueType() {
		return
	}
	// Value-struct alloca ({i8* vtable, i8* instance}) required to extract the instance ptr.
	if _, ok := alloca.ElemType.(*irtypes.StructType); !ok {
		return
	}

	dropFlag := c.createEntryAlloca(irtypes.I1)
	dropFlag.SetName(c.uniqueLocalName(varName + ".dropflag"))
	c.block.NewStore(constant.NewInt(irtypes.I1, 1), dropFlag)
	c.dropFlags[varName] = dropFlag

	binding := scopeBinding{
		kind:     bindingFree,
		alloca:   alloca,
		named:    named,
		valType:  typ,
		dropFlag: dropFlag,
		varName:  varName,
		rttiDrop: true, // RTTI-dispatched drop for standard-layout instances
	}
	c.scopeBindings = append(c.scopeBindings, binding)
	c.dropBindings[varName] = binding
}

// registerErrorDrop registers a caught error instance for drop at scope exit (T0091).
// Uses the concrete error type's drop when available (T0110), falling back to the
// base error.drop for untyped catches. The concrete drop properly frees all string
// fields (message + child-specific fields like key). The drop flag enables proper
// handling of re-raise (genRaiseStmt clears it, T0086).
// concreteType is the resolved type — may be *types.Named or *types.Instance for generics.
//
// B0226: For untyped catches (concreteType == types.TypError), uses RTTI-based dispatch:
// loads the drop function pointer from the typeinfo (field 1) of the actual error
// instance at runtime, enabling correct drop for generic error subtypes like
// GenericError[Point] even when caught as bare `error`.
func (c *Compiler) registerErrorDrop(varName string, alloca *ir.InstAlloca, concreteType types.Type) {
	dropFlag := c.createEntryAlloca(irtypes.I1)
	dropFlag.SetName(c.uniqueLocalName(varName + ".dropflag"))
	c.block.NewStore(constant.NewInt(irtypes.I1, 1), dropFlag)
	c.dropFlags[varName] = dropFlag

	concreteNamed := extractNamed(concreteType)
	if concreteNamed == nil {
		concreteNamed = types.TypError
	}

	// B0226: For untyped catches where the concrete type is the base error type,
	// use RTTI-based dispatch to call the actual error subtype's drop at runtime.
	// This handles cases like GenericError[Point] caught via untyped `? e { ... }`.
	if concreteNamed == types.TypError {
		binding := scopeBinding{
			kind:     bindingDrop,
			alloca:   alloca,
			named:    concreteNamed,
			valType:  concreteNamed,
			dropFlag: dropFlag,
			rttiDrop: true,
			varName:  varName,
		}
		c.scopeBindings = append(c.scopeBindings, binding)
		c.dropBindings[varName] = binding
		return
	}

	// Resolve the drop function for the concrete error type (T0110).
	// For typed catches, this is the child type's drop (e.g., NotFoundError.drop).
	// For generic instances (e.g., AppError[int]), use the monomorphized name.
	var ownerName string
	if inst, ok := concreteType.(*types.Instance); ok {
		ownerName = monoName(inst)
	} else {
		ownerName = concreteNamed.Obj().Name()
	}
	dropName := mangleMethodName(ownerName, "drop", false)
	dropFunc := c.funcs[dropName]
	if dropFunc == nil {
		// Fallback: resolve via method owner chain (with child-first preference)
		fallbackOwner := c.resolveDropOwner(concreteNamed)
		dropFunc = c.funcs[mangleMethodName(fallbackOwner, "drop", false)]
	}
	if dropFunc == nil {
		// Last resort: use base error.drop (e.g., bare generic types like AppError
		// without type args where the monomorphized drop isn't available).
		dropFunc = c.funcs[mangleMethodName("__mod_std_error", "drop", false)]
	}

	binding := scopeBinding{
		kind:     bindingDrop,
		alloca:   alloca,
		named:    concreteNamed,
		valType:  concreteNamed,
		dropFlag: dropFlag,
		dropFunc: dropFunc,
		varName:  varName,
	}
	c.scopeBindings = append(c.scopeBindings, binding)
	c.dropBindings[varName] = binding
}

// maybeRegisterEnumDrop registers a drop binding for an enum variable whose variants
// contain heap-allocated data (T0102). The drop function takes i8* (pointer to the
// alloca storing the enum internal type) and switches on the tag to drop variant fields.
func (c *Compiler) maybeRegisterEnumDrop(varName string, alloca *ir.InstAlloca, typ types.Type, enum *types.Enum) {
	dropFlag := c.createEntryAlloca(irtypes.I1)
	dropFlag.SetName(c.uniqueLocalName(varName + ".dropflag"))
	c.block.NewStore(constant.NewInt(irtypes.I1, 1), dropFlag)
	c.dropFlags[varName] = dropFlag

	// Resolve the enum drop function name.
	enumName := enum.Obj().Name()
	if inst, ok := typ.(*types.Instance); ok {
		// B0238: typ is already a concrete Instance (e.g., Container[Wrapper]) — use mono name directly.
		enumName = monoName(inst)
	} else if c.typeSubst != nil {
		// Inside a generic body — substitute TypeParams to get the concrete Instance.
		resolvedTyp := types.Substitute(typ, c.typeSubst)
		if inst, ok := resolvedTyp.(*types.Instance); ok {
			enumName = monoName(inst)
		}
	}
	mangledName := mangleMethodName(enumName, "drop", false)
	var dropFunc *ir.Func
	if fn, ok := c.funcs[mangledName]; ok {
		dropFunc = fn
	}

	binding := scopeBinding{
		kind:     bindingDropEnum,
		alloca:   alloca,
		valType:  typ,
		dropFlag: dropFlag,
		dropFunc: dropFunc,
		varName:  varName,
	}
	c.scopeBindings = append(c.scopeBindings, binding)
	c.dropBindings[varName] = binding
}

// emitEnumDropCall emits a conditional drop call for an enum variable (T0102).
// Checks drop flag, then passes the alloca pointer (bitcast to i8*) to the drop function.
func (c *Compiler) emitEnumDropCall(b scopeBinding) {
	if b.dropFlag == nil || b.dropFunc == nil {
		return
	}

	flag := c.block.NewLoad(irtypes.I1, b.dropFlag)
	dropBlock := c.newBlock("enum.drop.call")
	skipBlock := c.newBlock("enum.drop.skip")
	c.block.NewCondBr(flag, dropBlock, skipBlock)

	c.block = dropBlock
	ptr := c.block.NewBitCast(b.alloca, irtypes.I8Ptr)
	c.block.NewCall(b.dropFunc, ptr)
	c.block.NewBr(skipBlock)

	c.block = skipBlock
}

// emitOptionalLocalValueDrop drops the inner value of an Optional struct held
// by a local variable binding. optVal is the loaded {i1 flag, X} struct;
// elemType is the immediate inner Promise type X. If X is itself Optional
// (nested), walks through layers recursively. At the bottom (non-Optional
// inner), dispatches via b.dropFunc / b.rttiDrop using elemType to choose the
// call shape (T0391). Distinct from compiler.go's emitOptionalValueDrop, which
// derives dispatch from the type alone — this helper reuses the precomputed
// dispatch info on the binding so per-instantiation drops (Arc, Mutex, Weak,
// MutexGuard) and structural-interface RTTI dispatch are handled correctly.
func (c *Compiler) emitOptionalLocalValueDrop(optVal value.Value, elemType types.Type, b scopeBinding) {
	hasVal := c.block.NewExtractValue(optVal, 0)
	dropInnerBlock := c.newBlock("optdrop.inner")
	doneBlock := c.newBlock("optdrop.done")
	c.block.NewCondBr(hasVal, dropInnerBlock, doneBlock)

	c.block = dropInnerBlock
	innerVal := c.block.NewExtractValue(optVal, 1)

	if innerOpt, ok := elemType.(*types.Optional); ok {
		// T0391: Nested Optional — walk into the next layer.
		innerElem := innerOpt.Elem()
		if c.typeSubst != nil {
			innerElem = types.Substitute(innerElem, c.typeSubst)
		}
		c.emitOptionalLocalValueDrop(innerVal, innerElem, b)
	} else if _, isTup := elemType.(*types.Tuple); isTup {
		// T0397: Tuple inner type — walk fields and drop droppable ones via
		// the same helper used by bindingDropTuple.
		typ := elemType
		if c.typeSubst != nil {
			typ = types.Substitute(typ, c.typeSubst)
		}
		c.emitVariantFieldDrop(innerVal, typ)
	} else if _, isSig := elemType.(*types.Signature); isSig {
		// T0814: closure inner — free the fat pointer's env (deep-drop captures).
		typ := elemType
		if c.typeSubst != nil {
			typ = types.Substitute(typ, c.typeSubst)
		}
		c.emitVariantFieldDrop(innerVal, typ)
	} else if b.rttiDrop {
		// B0243: RTTI-based drop dispatch for Optional[StructuralInterface].
		// The concrete type is unknown at compile time — dispatch through typeinfo.
		instance := c.extractInstancePtr(innerVal)
		nullCheck := c.block.NewICmp(enum.IPredEQ, instance, constant.NewNull(irtypes.I8Ptr))
		execBlock := c.newBlock("optdrop.rtti")
		nullSkip := c.newBlock("optdrop.null")
		c.block.NewCondBr(nullCheck, nullSkip, execBlock)

		c.block = execBlock
		c.emitStructuralInstanceDrop(instance)
		c.block.NewBr(nullSkip)

		c.block = nullSkip
	} else if b.dropFunc != nil {
		// Enum inner type: store to temp alloca, bitcast to i8*, call drop.
		if extractEnum(elemType) != nil {
			enumLLVM := c.resolveType(elemType)
			tmpAlloca := c.createEntryAlloca(enumLLVM)
			c.block.NewStore(innerVal, tmpAlloca)
			ptr := c.block.NewBitCast(tmpAlloca, irtypes.I8Ptr)
			c.block.NewCall(b.dropFunc, ptr)
		} else if _, isTask := types.AsTask(elemType); (isTask || b.named == types.TypTask) &&
			c.emitTaskJoinAndFreeByDropFn(innerVal, b.dropFunc) {
			// T0668: `task[T]? o` local — cooperative park-suspend join in a
			// coroutine body (test body / WASM main / go {}) so the
			// single-threaded WASM scheduler can run the pending goroutine;
			// emitTaskJoinAndFreeByDropFn falls back to the legacy spin
			// (returns true) when not in a coroutine.
		} else if isContainerType(elemType) || b.named == types.TypString {
			// String, vector, channel: inner is i8*, call drop directly.
			// T0938: For a vector inner with droppable elements (e.g. string[]?),
			// b.dropFunc is the generic Vector.drop which frees only the buffer.
			// Mirror emitStringDropCall: walk and drop elements first, under the
			// same bit-63 static-vector guard. Static .rodata vectors skip both
			// the element drops and the buffer free.
			resolved := elemType
			if c.typeSubst != nil {
				resolved = types.Substitute(resolved, c.typeSubst)
			}
			if vecElem, isVec := types.AsVector(resolved); isVec {
				headerType := vectorHeaderType()
				headerPtr := c.block.NewBitCast(innerVal, irtypes.NewPointer(headerType))
				rawLen := loadVectorLenRaw(c.block, headerPtr)
				bit63 := c.block.NewAnd(rawLen, constant.NewInt(irtypes.I64, math.MinInt64))
				isStatic := c.block.NewICmp(enum.IPredNE, bit63, constant.NewInt(irtypes.I64, 0))
				vecDoneBlock := c.newBlock("optvecdrop.done")
				nonStaticBlock := c.newBlock("optvecdrop.nonstatic")
				c.block.NewCondBr(isStatic, vecDoneBlock, nonStaticBlock)

				c.block = nonStaticBlock
				c.emitVectorElementDropLoop(innerVal, vecElem)
				c.block.NewCall(b.dropFunc, innerVal)
				c.block.NewBr(vecDoneBlock)

				c.block = vecDoneBlock
			} else {
				c.block.NewCall(b.dropFunc, innerVal)
			}
		} else {
			// User type: inner is value struct {vtable, instance}, extract instance ptr
			instance := c.extractInstancePtr(innerVal)
			nullCheck := c.block.NewICmp(enum.IPredEQ, instance, constant.NewNull(irtypes.I8Ptr))
			execBlock := c.newBlock("optdrop.exec")
			nullSkip := c.newBlock("optdrop.null")
			c.block.NewCondBr(nullCheck, nullSkip, execBlock)

			c.block = execBlock
			c.block.NewCall(b.dropFunc, instance)
			c.block.NewBr(nullSkip)

			c.block = nullSkip
		}
	}
	c.block.NewBr(doneBlock)

	c.block = doneBlock
}

// emitOptionalDropCall emits a conditional drop for an optional value (T0101).
// Checks: drop flag → has-value flag → drop inner value.
// Layout: optional is {i1 flag, T value} — field 0 is has-value, field 1 is inner.
// T0391: For nested Optionals (T??, T???...), b.valType is the immediate inner
// Optional and emitOptionalLocalValueDrop walks through layers recursively.
func (c *Compiler) emitOptionalDropCall(b scopeBinding) {
	if b.dropFlag == nil {
		return
	}

	flag := c.block.NewLoad(irtypes.I1, b.dropFlag)
	dropBlock := c.newBlock("optdrop.check")
	skipBlock := c.newBlock("optdrop.skip")
	c.block.NewCondBr(flag, dropBlock, skipBlock)

	c.block = dropBlock
	optVal := c.block.NewLoad(b.alloca.ElemType, b.alloca)

	c.emitOptionalLocalValueDrop(optVal, b.valType, b)

	c.block.NewBr(skipBlock)

	c.block = skipBlock
}

// emitTupleDropCall emits a conditional drop for a tuple value variable (T0371).
// Loads the tuple struct from its alloca, then walks fields and drops each
// droppable element via emitVariantFieldDrop (string, vector, channel, user
// types with drop, enums with drop, recursive tuples).
func (c *Compiler) emitTupleDropCall(b scopeBinding) {
	if b.dropFlag == nil {
		return
	}

	flag := c.block.NewLoad(irtypes.I1, b.dropFlag)
	dropBlock := c.newBlock("tupdrop.exec")
	skipBlock := c.newBlock("tupdrop.skip")
	c.block.NewCondBr(flag, dropBlock, skipBlock)

	c.block = dropBlock
	tupVal := c.block.NewLoad(b.alloca.ElemType, b.alloca)
	typ := b.valType
	if c.typeSubst != nil {
		typ = types.Substitute(typ, c.typeSubst)
	}
	c.emitVariantFieldDrop(tupVal, typ)
	c.block.NewBr(skipBlock)

	c.block = skipBlock
}

// emitArrayDropCall emits a conditional per-element drop for a fixed-size array
// variable (T0389). Walks each [N x T] slot via GEP, loads the element, and
// calls emitVariantFieldDrop on it (string, vector, channel, user types with
// drop, enums with drop, tuples with droppable fields).
func (c *Compiler) emitArrayDropCall(b scopeBinding) {
	if b.dropFlag == nil {
		return
	}
	arrType, ok := b.valType.(*types.Array)
	if !ok {
		return
	}
	elemType := arrType.Elem()
	if c.typeSubst != nil {
		elemType = types.Substitute(elemType, c.typeSubst)
	}

	flag := c.block.NewLoad(irtypes.I1, b.dropFlag)
	dropBlock := c.newBlock("arrdrop.exec")
	skipBlock := c.newBlock("arrdrop.skip")
	c.block.NewCondBr(flag, dropBlock, skipBlock)

	c.block = dropBlock
	llvmArrType := b.alloca.ElemType
	for i := int64(0); i < arrType.Size(); i++ {
		elemPtr := c.block.NewGetElementPtr(llvmArrType, b.alloca,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, i))
		elemVal := c.block.NewLoad(c.resolveType(elemType), elemPtr)
		c.emitVariantFieldDrop(elemVal, elemType)
	}
	c.block.NewBr(skipBlock)

	c.block = skipBlock
}

// registerValTypeOptionalDrop registers a bindingDropOptional that carries no
// dropFunc — emitOptionalLocalValueDrop dispatches on valType (immediateElem)
// instead. Shared by the Tuple (T0397) and closure/Signature (T0814) inner-type
// cases, whose drop is driven entirely by emitVariantFieldDrop on the inner value.
func (c *Compiler) registerValTypeOptionalDrop(varName string, alloca *ir.InstAlloca, immediateElem types.Type) {
	dropFlag := c.createEntryAlloca(irtypes.I1)
	dropFlag.SetName(c.uniqueLocalName(varName + ".dropflag"))
	c.block.NewStore(constant.NewInt(irtypes.I1, 1), dropFlag)
	c.dropFlags[varName] = dropFlag
	binding := scopeBinding{
		kind:     bindingDropOptional,
		alloca:   alloca,
		valType:  immediateElem,
		dropFlag: dropFlag,
		varName:  varName,
	}
	c.scopeBindings = append(c.scopeBindings, binding)
	c.dropBindings[varName] = binding
}

// maybeRegisterOptionalDrop registers a bindingDropOptional for an explicitly declared
// optional local variable (T0111). Only called for typed declarations (string? s = ...)
// where the inner type is droppable (string, vector, channel, user type with drop).
// Inferred optional variables (s := func_returning_optional()) are NOT registered —
// they are consumed via if-let/while-let/force-unwrap patterns.
//
// T0391: For nested Optionals (T??, T???, ...) walks through Optional layers to
// reach the bottom inner type for dispatch info, but stores the immediate inner
// type in valType so emitOptionalLocalValueDrop can recurse through layers correctly.
func (c *Compiler) maybeRegisterOptionalDrop(varName string, alloca *ir.InstAlloca, opt *types.Optional) {
	// Don't double-register if maybeRegisterDrop already handled this variable.
	if _, exists := c.dropBindings[varName]; exists {
		return
	}

	immediateElem := opt.Elem()
	if c.typeSubst != nil {
		immediateElem = types.Substitute(immediateElem, c.typeSubst)
	}
	// T0391: Walk past nested Optionals to find the bottom inner type for dispatch.
	// For T??, immediateElem is T? and elem becomes T. The helper recurses through
	// layers at IR generation time using valType = immediateElem.
	elem := immediateElem
	for {
		innerOpt, ok := elem.(*types.Optional)
		if !ok {
			break
		}
		next := innerOpt.Elem()
		if c.typeSubst != nil {
			next = types.Substitute(next, c.typeSubst)
		}
		elem = next
	}
	innerNamed := extractNamed(elem)

	// Determine the drop function for the inner type.
	var dropFunc *ir.Func

	switch {
	case innerNamed == types.TypString:
		dropFunc = c.funcs["promise_string_drop"]
	case innerNamed != nil && (func() bool { _, ok := types.AsVector(elem); return ok }() || innerNamed == types.TypVector):
		dropFunc = c.funcs["Vector.drop"]
	case innerNamed != nil && (func() bool { _, ok := types.AsChannel(elem); return ok }() || innerNamed == types.TypChannel):
		// T0663: Channel inner drop — resolve element type and get per-element-type drop
		if chanElem, ok := types.AsChannel(elem); ok {
			resolvedChanElem := chanElem
			if c.typeSubst != nil {
				resolvedChanElem = types.Substitute(chanElem, c.typeSubst)
			}
			dropFunc = c.getOrCreateChannelDrop(resolvedChanElem)
		}
	case innerNamed != nil && (func() bool { _, ok := types.AsArc(elem); return ok }() || innerNamed == types.TypArc):
		// T0155: Arc inner drop — resolve element type and get per-instantiation drop
		if arcElem, ok := types.AsArc(elem); ok {
			resolvedArcElem := arcElem
			if c.typeSubst != nil {
				resolvedArcElem = types.Substitute(arcElem, c.typeSubst)
			}
			dropFunc = c.getOrCreateArcDrop(resolvedArcElem)
		}
	case innerNamed != nil && (func() bool { _, ok := types.AsWeak(elem); return ok }() || innerNamed == types.TypWeak):
		// T0157: Weak inner drop — resolve element type and get per-instantiation drop
		if weakElem, ok := types.AsWeak(elem); ok {
			resolvedWeakElem := weakElem
			if c.typeSubst != nil {
				resolvedWeakElem = types.Substitute(weakElem, c.typeSubst)
			}
			dropFunc = c.getOrCreateWeakDrop(resolvedWeakElem)
		}
	case innerNamed != nil && (func() bool { _, ok := types.AsMutex(elem); return ok }() || innerNamed == types.TypMutex):
		// T0156: Mutex inner drop — per-instantiation
		if mutexElem, ok := types.AsMutex(elem); ok {
			resolvedElem := mutexElem
			if c.typeSubst != nil {
				resolvedElem = types.Substitute(mutexElem, c.typeSubst)
			}
			dropFunc = c.getOrCreateMutexDrop(resolvedElem)
		}
	case innerNamed != nil && (func() bool { _, ok := types.AsMutexGuard(elem); return ok }() || innerNamed == types.TypMutexGuard):
		// T0156: MutexGuard inner drop — T-independent
		dropFunc = c.funcs["MutexGuard.drop"]
	case innerNamed != nil && (func() bool { _, ok := types.AsTask(elem); return ok }() || innerNamed == types.TypTask):
		// T0558: Task inner drop — per-instantiation drop blocks on goroutine
		// completion, drops the result, frees result_ptr/panic_msg/G. Without
		// this case, dispatch fell through to the heap-user-type catch-all and
		// called pal_free on the raw G handle, causing segfaults at scope exit.
		var resolvedTaskElem types.Type
		if taskElem, ok := types.AsTask(elem); ok {
			resolvedTaskElem = taskElem
			if c.typeSubst != nil {
				resolvedTaskElem = types.Substitute(taskElem, c.typeSubst)
			}
		} else if innerNamed == types.TypTask && c.typeSubst != nil {
			if tp := types.TypTask.TypeParams(); len(tp) > 0 {
				resolvedTaskElem = c.typeSubst[tp[0]]
				if resolvedTaskElem != nil {
					resolvedTaskElem = types.Substitute(resolvedTaskElem, c.typeSubst)
				}
			}
		}
		dropFunc = c.getOrCreateTaskDrop(resolvedTaskElem)
	case innerNamed != nil && (innerNamed.HasDrop() || innerNamed.NeedsSynthDrop()):
		// User type with explicit or synthesized drop
		explicitDrop := innerNamed.HasDrop() && !innerNamed.NeedsSynthDrop()
		ownerName := innerNamed.Obj().Name()
		resolvedElem := elem
		if c.typeSubst != nil {
			resolvedElem = types.Substitute(elem, c.typeSubst)
		}
		if inst, ok := resolvedElem.(*types.Instance); ok {
			ownerName = monoName(inst)
		} else if explicitDrop {
			ownerName = c.resolveDropOwner(innerNamed)
		}
		mangledName := mangleMethodName(ownerName, "drop", false)
		if fn, ok := c.funcs[mangledName]; ok {
			if explicitDrop {
				// T0419: Explicit user drops don't include pal_free — wrap with $wrap
				// so the Optional drop path frees the instance after calling drop.
				// Synthesized drops already include pal_free.
				fn = c.getOrCreateDropWrap(mangledName, fn)
			}
			dropFunc = fn
		}
	case innerNamed != nil && !innerNamed.IsValueType() && !innerNamed.IsCopy() && !isPrimitiveScalar(innerNamed) && !innerNamed.IsStructural():
		// B0211: Heap user type without drop — use pal_free to free the instance.
		dropFunc = c.palFree
	case innerNamed == nil && func() bool {
		// Droppable enum inner type: look up the enum drop function.
		enum := extractEnum(elem)
		if enum == nil {
			return false
		}
		enumName := enum.Obj().Name()
		if inst, ok := elem.(*types.Instance); ok {
			enumName = monoName(inst)
		}
		mangledName := mangleMethodName(enumName, "drop", false)
		if fn, ok := c.funcs[mangledName]; ok {
			dropFunc = fn
			return true
		}
		return false
	}():
		// dropFunc already set by the closure above
	case innerNamed != nil && innerNamed.IsStructural() && !innerNamed.IsValueType():
		// B0229/B0243: Structural interface (e.g., Iterator[T]) — use RTTI-based drop
		// dispatch. The concrete type is unknown at compile time (could be _FnIter,
		// Counter, or any user type implementing the interface), so we dispatch through
		// the typeinfo drop_fn_ptr at runtime.
		dropFlag := c.createEntryAlloca(irtypes.I1)
		dropFlag.SetName(c.uniqueLocalName(varName + ".dropflag"))
		c.block.NewStore(constant.NewInt(irtypes.I1, 1), dropFlag)
		c.dropFlags[varName] = dropFlag
		binding := scopeBinding{
			kind:     bindingDropOptional,
			alloca:   alloca,
			named:    innerNamed,
			valType:  immediateElem,
			dropFlag: dropFlag,
			rttiDrop: true,
			varName:  varName,
		}
		c.scopeBindings = append(c.scopeBindings, binding)
		c.dropBindings[varName] = binding
		return
	case func() bool {
		_, isTup := elem.(*types.Tuple)
		return isTup && c.tupleNeedsDrop(elem)
	}():
		// T0397: Tuple inner type — register binding without dropFunc. The
		// emitOptionalLocalValueDrop Tuple branch dispatches via emitVariantFieldDrop
		// on the inner tuple value (walks fields, drops droppable elements).
		c.registerValTypeOptionalDrop(varName, alloca, immediateElem)
		return
	case func() bool { _, isSig := elem.(*types.Signature); return isSig }():
		// T0814: Optional[closure] local — the inner fat pointer {fn,env} owns a heap
		// env. Register an optional drop; emitOptionalLocalValueDrop frees the env
		// (deep-drops captures) when present. Same machinery as string?/vector? so
		// move-tracking (g := o) and reassignment (o = ...) clear/re-arm the flag.
		c.registerValTypeOptionalDrop(varName, alloca, immediateElem)
		return
	default:
		return // inner type not droppable
	}

	if dropFunc == nil {
		return
	}

	dropFlag := c.createEntryAlloca(irtypes.I1)
	dropFlag.SetName(c.uniqueLocalName(varName + ".dropflag"))
	c.block.NewStore(constant.NewInt(irtypes.I1, 1), dropFlag)
	c.dropFlags[varName] = dropFlag

	binding := scopeBinding{
		kind:     bindingDropOptional,
		alloca:   alloca,
		named:    innerNamed,
		valType:  immediateElem,
		dropFlag: dropFlag,
		dropFunc: dropFunc,
		varName:  varName,
	}
	c.scopeBindings = append(c.scopeBindings, binding)
	c.dropBindings[varName] = binding
}

// maybeRegisterCapturedOptionalStructuralDrop registers a reassignment-only drop binding
// for captured optional structural interface variables (B0229). Unlike maybeRegisterOptionalDrop,
// this does NOT add to scopeBindings — the env drop function handles final cleanup at env
// deallocation, and scope-exit drop would free a value that's been written back to the env.
func (c *Compiler) maybeRegisterCapturedOptionalStructuralDrop(varName string, alloca *ir.InstAlloca, typ types.Type) {
	if _, exists := c.dropBindings[varName]; exists {
		return
	}
	if c.typeSubst != nil {
		typ = types.Substitute(typ, c.typeSubst)
	}
	opt, ok := typ.(*types.Optional)
	if !ok {
		return
	}
	elem := opt.Elem()
	if c.typeSubst != nil {
		elem = types.Substitute(elem, c.typeSubst)
	}
	innerNamed := extractNamed(elem)
	if innerNamed == nil || !innerNamed.IsStructural() || innerNamed.IsValueType() {
		return
	}

	// B0243: Use RTTI-based drop dispatch — the concrete type behind the structural
	// interface is unknown at compile time.
	dropFlag := c.createEntryAlloca(irtypes.I1)
	dropFlag.SetName(c.uniqueLocalName(varName + ".dropflag"))
	c.block.NewStore(constant.NewInt(irtypes.I1, 1), dropFlag)
	c.dropFlags[varName] = dropFlag

	binding := scopeBinding{
		kind:     bindingDropOptional,
		alloca:   alloca,
		named:    innerNamed,
		valType:  elem,
		dropFlag: dropFlag,
		rttiDrop: true,
		varName:  varName,
	}
	// Only add to dropBindings (for reassignment drop), NOT scopeBindings (no scope-exit drop).
	c.dropBindings[varName] = binding
}

// clearDropFlag sets a variable's drop flag to false (indicating the value has been moved).
func (c *Compiler) clearDropFlag(name string) {
	if flag, ok := c.dropFlags[name]; ok {
		c.block.NewStore(constant.NewInt(irtypes.I1, 0), flag)
	}
}

// emitEarlyDrops checks if any variables should be dropped after the given statement
// (NLL last-use analysis, B0035). For each variable whose last use is this statement,
// emits the drop call and clears the drop flag so scope cleanup skips it.
func (c *Compiler) emitEarlyDrops(stmt ast.Stmt) {
	if c.block == nil || c.block.Term != nil {
		return // block already terminated
	}
	if c.info.EarlyDrops == nil {
		return
	}
	drops, ok := c.info.EarlyDrops[stmt]
	if !ok {
		return
	}
	// Process in reverse order to respect LIFO drop ordering:
	// variables declared later should be dropped first.
	for i := len(drops) - 1; i >= 0; i-- {
		varName := drops[i]
		binding, ok := c.dropBindings[varName]
		if !ok {
			continue // no drop binding (copy type, no-drop type, etc.)
		}
		// Skip use-bound (close) bindings — close() error handling is tied to scope exit.
		if binding.kind == bindingClose {
			continue
		}
		// Emit the appropriate drop call (checks drop flag internally).
		switch binding.kind {
		case bindingDrop:
			c.emitDropCall(binding)
		case bindingDropString:
			c.emitStringDropCall(binding)
		case bindingDropEnum:
			c.emitEnumDropCall(binding)
		case bindingDropOptional:
			c.emitOptionalDropCall(binding)
		case bindingDropTuple:
			c.emitTupleDropCall(binding)
		case bindingDropArray:
			c.emitArrayDropCall(binding)
		case bindingFree:
			c.emitFreeCall(binding)
		case bindingFreeEnv:
			c.emitEnvFree(binding)
		case bindingGenerator:
			c.emitGeneratorCleanup(binding)
		}
		// Clear the drop flag so scope cleanup skips this variable.
		// The drop call above already checked the flag — clearing it here
		// ensures the variable won't be double-dropped at scope exit.
		c.clearDropFlag(varName)
	}
}

// emitScopeCleanup emits cleanup calls for all scope bindings from fromIdx onwards,
// in reverse order (LIFO). Close bindings call close(), drop bindings check the
// drop flag and conditionally call drop().
//
// errorInFlight indicates the scope is exiting due to a raise or error propagation.
// When true, failable close() errors are suppressed. When false and the enclosing
// function is failable, the first close() error is captured and returned.
func (c *Compiler) emitScopeCleanup(fromIdx int, errorInFlight bool) *closeErrCapture {
	// Check if we need error capture: failable function, normal path, and at least
	// one failable close binding in the range.
	var cap *closeErrCapture
	if c.canError && !errorInFlight {
		for i := len(c.scopeBindings) - 1; i >= fromIdx; i-- {
			b := c.scopeBindings[i]
			if b.kind == bindingClose && b.closeIsFailable {
				cap = &closeErrCapture{
					flag: c.createEntryAlloca(irtypes.I1),
					val:  c.createEntryAlloca(irtypes.I8Ptr),
				}
				cap.flag.SetName(c.uniqueLocalName("close.err.flag"))
				cap.val.SetName(c.uniqueLocalName("close.err.val"))
				c.block.NewStore(constant.NewInt(irtypes.I1, 0), cap.flag)
				c.block.NewStore(constant.NewNull(irtypes.I8Ptr), cap.val)
				break
			}
		}
	}

	for i := len(c.scopeBindings) - 1; i >= fromIdx; i-- {
		b := c.scopeBindings[i]
		switch b.kind {
		case bindingClose:
			c.emitCloseCall(b, cap)
		case bindingDrop:
			c.emitDropCall(b)
		case bindingDropString:
			c.emitStringDropCall(b)
		case bindingDropEnum:
			c.emitEnumDropCall(b)
		case bindingDropOptional:
			c.emitOptionalDropCall(b)
		case bindingDropTuple:
			c.emitTupleDropCall(b)
		case bindingDropArray:
			c.emitArrayDropCall(b)
		case bindingFree:
			c.emitFreeCall(b)
		case bindingFreeEnv:
			c.emitEnvFree(b)
		case bindingGenerator:
			c.emitGeneratorCleanup(b)
		}
	}
	return cap
}

// emitCloseCall emits a close() call for a use-bound variable (direct or virtual dispatch).
// If cap is non-nil and close() is failable, the first error is captured into cap's allocas.
func (c *Compiler) emitCloseCall(b scopeBinding, cap *closeErrCapture) {
	val := c.block.NewLoad(b.alloca.ElemType, b.alloca)

	var result value.Value
	if b.closeFunc != nil {
		// Direct dispatch — extract instance pointer and call.
		// T0156: Container types (e.g. MutexGuard) are opaque i8* — pass directly.
		var receiver value.Value
		if isContainerType(b.valType) {
			receiver = val
		} else {
			receiver = c.extractInstancePtr(val)
		}
		result = c.block.NewCall(b.closeFunc, receiver)
	} else if b.named != nil {
		// Virtual dispatch through vtable
		vtableRaw := c.extractVtablePtr(val)
		instance := c.extractInstancePtr(val)

		slotIndex := b.named.VirtualMethodIndex("close", false)
		if slotIndex < 0 {
			panic(fmt.Sprintf("codegen: close method not in vtable for %s", b.named))
		}
		vtablePtr := c.block.NewBitCast(vtableRaw, irtypes.NewPointer(irtypes.I8Ptr))
		fnSlotPtr := c.block.NewGetElementPtr(irtypes.I8Ptr, vtablePtr,
			constant.NewInt(irtypes.I32, int64(slotIndex)))
		fnRaw := c.block.NewLoad(irtypes.I8Ptr, fnSlotPtr)

		closeMethod := b.named.LookupMethod("close")
		retType := irtypes.Type(irtypes.Void)
		if closeMethod.Sig().CanError() {
			retType = computeResultType(retType)
		}
		funcType := irtypes.NewFunc(retType, irtypes.I8Ptr)
		fnTyped := c.block.NewBitCast(fnRaw, irtypes.NewPointer(funcType))
		result = c.block.NewCall(fnTyped, instance)
	}

	// T0106: After close(), free the heap instance (and droppable fields).
	// use-bound types have close() but may not have drop(). Without this, the
	// heap instance leaks. If the type has a synthesized drop, call that (it handles
	// field drops + pal_free). If the type has an explicit drop, call that + pal_free.
	// Otherwise, just pal_free the instance directly.
	if b.named != nil && !isContainerType(b.valType) && !b.named.IsValueType() {
		instance := c.extractInstancePtr(val)
		// Null-check before freeing
		nullCheck := c.block.NewICmp(enum.IPredEQ, instance, constant.NewNull(irtypes.I8Ptr))
		freeBlock := c.newBlock("close.free")
		freeDone := c.newBlock("close.free.done")
		c.block.NewCondBr(nullCheck, freeDone, freeBlock)

		c.block = freeBlock
		if b.named.HasDrop() {
			// Type has drop (explicit or synthesized) — call it to clean up fields + free
			ownerName := c.resolveDropOwner(b.named)
			mangledName := mangleMethodName(ownerName, "drop", false)
			if dropFn, ok := c.funcs[mangledName]; ok {
				c.block.NewCall(dropFn, instance)
			}
			// Explicit drop doesn't include pal_free — add it
			if !b.named.NeedsSynthDrop() {
				c.block.NewCall(c.palFree, instance)
			}
		} else {
			// No drop at all — just free the instance
			c.block.NewCall(c.palFree, instance)
		}
		c.block.NewBr(freeDone)
		c.block = freeDone
	}

	// Handle failable close() errors: capture, suppress+drop, or ignore.
	if b.closeIsFailable && result != nil {
		resultType := result.Type().(*irtypes.StructType)
		tag := c.block.NewExtractValue(result, 0)

		if cap != nil {
			// Capture path: save first error, drop subsequent errors (T0135).
			errBlock := c.newBlock("close.err")
			contBlock := c.newBlock("close.cont")
			c.block.NewCondBr(tag, errBlock, contBlock)

			c.block = errBlock
			hasErr := c.block.NewLoad(irtypes.I1, cap.flag)
			saveBlock := c.newBlock("close.save")
			dropDupBlock := c.newBlock("close.err.drop.dup")
			c.block.NewCondBr(hasErr, dropDupBlock, saveBlock)

			// Save first error
			c.block = saveBlock
			c.block.NewStore(constant.NewInt(irtypes.I1, 1), cap.flag)
			errVal := c.block.NewExtractValue(result, resultErrIdx(resultType))
			c.block.NewStore(errVal, cap.val)
			c.block.NewBr(contBlock)

			// T0135: Drop duplicate close error to prevent leak
			c.block = dropDupBlock
			dupErrVal := c.block.NewExtractValue(result, resultErrIdx(resultType))
			c.emitDropSuppressedError(dupErrVal)
			c.block.NewBr(contBlock)

			c.block = contBlock
		} else {
			// T0135: Suppress path (error in flight or non-failable function).
			// Drop the close error to prevent leak.
			errDropBlock := c.newBlock("close.err.drop")
			contBlock := c.newBlock("close.cont")
			c.block.NewCondBr(tag, errDropBlock, contBlock)

			c.block = errDropBlock
			errVal := c.block.NewExtractValue(result, resultErrIdx(resultType))
			c.emitDropSuppressedError(errVal)
			c.block.NewBr(contBlock)

			c.block = contBlock
		}
	}
}

// emitCloseErrCheck checks a captured close error and, if set, returns it from
// the current failable function. Otherwise, continues in a new block.
func (c *Compiler) emitCloseErrCheck(cap *closeErrCapture) {
	if cap == nil {
		return
	}
	flag := c.block.NewLoad(irtypes.I1, cap.flag)
	errRetBlock := c.newBlock("close.err.ret")
	contBlock := c.newBlock("close.ok.cont")
	c.block.NewCondBr(flag, errRetBlock, contBlock)

	c.block = errRetBlock
	errVal := c.block.NewLoad(irtypes.I8Ptr, cap.val)
	resultType := c.currentResultType()
	c.block.NewRet(c.wrapError(errVal, resultType))

	c.block = contBlock
}

// emitDropSuppressedError drops an error instance (i8*) that is being suppressed.
// T0135: Used when a failable close() error is suppressed (error in flight or
// duplicate close error). Calls error.drop to free the message string and instance.
func (c *Compiler) emitDropSuppressedError(errPtr value.Value) {
	dropName := mangleMethodName("__mod_std_error", "drop", false)
	if dropFn, ok := c.funcs[dropName]; ok {
		c.block.NewCall(dropFn, errPtr)
	}
}

// emitDropCall emits a conditional drop() call for a droppable variable.
// Checks the drop flag; if true (not moved), calls drop().
// Dispatches to emitStringDropCall for bindingDropString bindings.
func (c *Compiler) emitDropCall(b scopeBinding) {
	if b.kind == bindingDropString {
		c.emitStringDropCall(b)
		return
	}
	if b.kind == bindingDropOptional {
		c.emitOptionalDropCall(b)
		return
	}
	if b.kind == bindingDropTuple {
		c.emitTupleDropCall(b)
		return
	}
	if b.kind == bindingDropArray {
		c.emitArrayDropCall(b)
		return
	}
	if b.dropFlag == nil {
		// No drop flag — unconditional drop
		c.emitDropCallDirect(b)
		return
	}

	flag := c.block.NewLoad(irtypes.I1, b.dropFlag)
	dropBlock := c.newBlock("drop.call")
	skipBlock := c.newBlock("drop.skip")
	c.block.NewCondBr(flag, dropBlock, skipBlock)

	c.block = dropBlock
	c.emitDropCallDirect(b)
	c.block.NewBr(skipBlock)

	c.block = skipBlock
}

// emitDropCallDirect emits the actual drop() call (direct or virtual dispatch).
// Guards against null instance pointers (e.g., zero-initialized values from
// error handler paths that don't produce a recovery value).
// Container types (Vector, Channel) store raw i8* — not value structs — so we
// load the i8* directly instead of extracting field 1 from a struct.
func (c *Compiler) emitDropCallDirect(b scopeBinding) {
	val := c.block.NewLoad(b.alloca.ElemType, b.alloca)

	// Container types (Vector, Channel) store raw i8* pointers, not value structs.
	// Use the loaded i8* directly — extractInstancePtr would crash on a non-struct.
	var instance value.Value
	if isContainerType(b.valType) {
		instance = val
	} else {
		instance = c.extractInstancePtr(val)
	}

	// Null-check instance pointer: zero-initialized values (from error handler
	// fallthrough) have null instance — skip drop to avoid dereferencing null.
	nullCheck := c.block.NewICmp(enum.IPredEQ, instance, constant.NewNull(irtypes.I8Ptr))
	dropExecBlock := c.newBlock("drop.exec")
	dropDoneBlock := c.newBlock("drop.done")
	c.block.NewCondBr(nullCheck, dropDoneBlock, dropExecBlock)

	c.block = dropExecBlock
	if b.rttiDrop {
		// B0226: RTTI-based drop dispatch for untyped error catches.
		// Load the drop function pointer from the error instance's typeinfo (field 1)
		// and call it. Synthesized drops handle pal_free internally; explicit user drops
		// use a $wrap function that calls drop + pal_free (B0247).
		c.emitRttiDropDispatch(instance)
	} else if b.dropFunc != nil {
		c.block.NewCall(b.dropFunc, instance)
	} else if b.named != nil {
		vtableRaw := c.extractVtablePtr(val)

		slotIndex := b.named.VirtualMethodIndex("drop", false)
		if slotIndex < 0 {
			panic(fmt.Sprintf("codegen: drop method not in vtable for %s", b.named))
		}
		vtablePtr := c.block.NewBitCast(vtableRaw, irtypes.NewPointer(irtypes.I8Ptr))
		fnSlotPtr := c.block.NewGetElementPtr(irtypes.I8Ptr, vtablePtr,
			constant.NewInt(irtypes.I32, int64(slotIndex)))
		fnRaw := c.block.NewLoad(irtypes.I8Ptr, fnSlotPtr)

		funcType := irtypes.NewFunc(irtypes.Void, irtypes.I8Ptr)
		fnTyped := c.block.NewBitCast(fnRaw, irtypes.NewPointer(funcType))
		c.block.NewCall(fnTyped, instance)
	}
	// B0159: Free the instance struct after drop() completes.
	// Only for types with explicit drop — synthesized drops (B0158/B0160/B0202) are
	// deferred until ownership tracking prevents aliasing issues.
	// Container types are excluded — their drop already frees the buffer.
	// B0226: rttiDrop dispatch calls the concrete drop which handles pal_free internally.
	if !b.rttiDrop && !isContainerType(b.valType) && b.named != nil && !b.named.NeedsSynthDrop() && !b.monoSynthDrop {
		c.block.NewCall(c.palFree, instance)
	}
	c.block.NewBr(dropDoneBlock)

	c.block = dropDoneBlock
}

// emitRttiDropDispatch loads the drop function pointer from the error instance's
// typeinfo and calls it. Falls back to base error.drop if the typeinfo drop_fn_ptr
// is null.
// B0226: Enables correct drop for generic error subtypes (e.g., GenericError[Point])
// caught via untyped error handlers (? e { ... }).
func (c *Compiler) emitRttiDropDispatch(instance value.Value) {
	// Load variant pointer from instance (field 0 of instance struct)
	variantPtr := c.loadVariantPtr(instance)

	// Typeinfo struct type (only need first 2 fields for drop_fn_ptr access)
	typeinfoType := irtypes.NewStruct(
		irtypes.I8Ptr, // field 0: vtable_ptr
		irtypes.I8Ptr, // field 1: drop_fn_ptr
	)

	// Load drop_fn_ptr (field 1 of typeinfo)
	typedPtr := c.block.NewBitCast(variantPtr, irtypes.NewPointer(typeinfoType))
	dropFnPtr := c.block.NewGetElementPtr(typeinfoType, typedPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 1))
	dropFn := c.block.NewLoad(irtypes.I8Ptr, dropFnPtr)

	// If non-null, call the concrete drop; otherwise fall back to base error.drop
	isNull := c.block.NewICmp(enum.IPredEQ, dropFn, constant.NewNull(irtypes.I8Ptr))
	callBlock := c.newBlock("rtti.drop.call")
	fallbackBlock := c.newBlock("rtti.drop.fallback")
	doneBlock := c.newBlock("rtti.drop.done")
	c.block.NewCondBr(isNull, fallbackBlock, callBlock)

	// Concrete drop via typeinfo
	c.block = callBlock
	dropFnType := irtypes.NewFunc(irtypes.Void, irtypes.I8Ptr)
	typedFn := c.block.NewBitCast(dropFn, irtypes.NewPointer(dropFnType))
	c.block.NewCall(typedFn, instance)
	c.block.NewBr(doneBlock)

	// Fallback: base error.drop
	c.block = fallbackBlock
	baseDropName := mangleMethodName("__mod_std_error", "drop", false)
	if baseDropFn, ok := c.funcs[baseDropName]; ok {
		c.block.NewCall(baseDropFn, instance)
	}
	c.block.NewBr(doneBlock)

	c.block = doneBlock
}

// emitStructuralInstanceDrop drops a heap-allocated instance behind a structural interface
// using RTTI-based dispatch (B0243). Loads the typeinfo drop_fn_ptr from the instance's
// variant field. If drop_fn is non-null, calls it — synthesized drops include pal_free;
// explicit user drops use a $wrap function that calls drop + pal_free (B0247).
// If drop_fn is null (type has no drop), calls pal_free directly.
func (c *Compiler) emitStructuralInstanceDrop(instance value.Value) {
	// Load variant pointer from instance (field 0 = typeinfo ptr)
	variantPtr := c.loadVariantPtr(instance)

	// Typeinfo layout: { i8* vtable_ptr, i8* drop_fn_ptr, ... }
	typeinfoType := irtypes.NewStruct(
		irtypes.I8Ptr, // field 0: vtable_ptr
		irtypes.I8Ptr, // field 1: drop_fn_ptr
	)
	typedPtr := c.block.NewBitCast(variantPtr, irtypes.NewPointer(typeinfoType))
	dropFnField := c.block.NewGetElementPtr(typeinfoType, typedPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 1))
	dropFn := c.block.NewLoad(irtypes.I8Ptr, dropFnField)

	isNull := c.block.NewICmp(enum.IPredEQ, dropFn, constant.NewNull(irtypes.I8Ptr))
	callBlock := c.newBlock("struct.drop.call")
	freeBlock := c.newBlock("struct.drop.free")
	doneBlock := c.newBlock("struct.drop.done")
	c.block.NewCondBr(isNull, freeBlock, callBlock)

	// Has drop function: call it (synth drops include pal_free; explicit user
	// drops use $wrap which calls drop + pal_free per B0247)
	c.block = callBlock
	dropFnType := irtypes.NewFunc(irtypes.Void, irtypes.I8Ptr)
	typedFn := c.block.NewBitCast(dropFn, irtypes.NewPointer(dropFnType))
	c.block.NewCall(typedFn, instance)
	c.block.NewBr(doneBlock)

	// No drop function: just free the instance
	c.block = freeBlock
	c.block.NewCall(c.palFree, instance)
	c.block.NewBr(doneBlock)

	c.block = doneBlock
}

// dropDiscardedOptional handles B0196/B0208: when an ExprStmt discards an
// Optional result with a droppable inner type (string, vector, channel, user
// type with drop), the inner value must be dropped. trackStringTemp only tracks
// bare i8* values, so {i1, T} optionals slip through.
func (c *Compiler) dropDiscardedOptional(expr ast.Expr, result value.Value) {
	if result == nil || c.block == nil || c.block.Term != nil {
		return
	}
	exprType := c.info.Types[expr]
	if exprType == nil {
		return
	}
	if c.typeSubst != nil {
		exprType = types.Substitute(exprType, c.typeSubst)
	}
	opt, ok := exprType.(*types.Optional)
	if !ok {
		return
	}
	elem := opt.Elem()
	if c.typeSubst != nil {
		elem = types.Substitute(elem, c.typeSubst)
	}
	innerNamed := extractNamed(elem)

	// Resolve the drop function for the inner type.
	var dropFunc *ir.Func
	var isContainer bool

	switch {
	case innerNamed == types.TypString:
		dropFunc = c.funcs["promise_string_drop"]
	case innerNamed != nil && (func() bool { _, ok := types.AsVector(elem); return ok }() || innerNamed == types.TypVector):
		dropFunc = c.funcs["Vector.drop"]
		isContainer = true
	case innerNamed != nil && (func() bool { _, ok := types.AsChannel(elem); return ok }() || innerNamed == types.TypChannel):
		// T0663: Channel inner drop — per-element-type drop walks buffered items.
		if chanElem, ok := types.AsChannel(elem); ok {
			resolvedChanElem := chanElem
			if c.typeSubst != nil {
				resolvedChanElem = types.Substitute(chanElem, c.typeSubst)
			}
			dropFunc = c.getOrCreateChannelDrop(resolvedChanElem)
			isContainer = true
		}
	case innerNamed != nil && (func() bool { _, ok := types.AsArc(elem); return ok }() || innerNamed == types.TypArc):
		// T0155: Arc inner drop
		if arcElem, ok := types.AsArc(elem); ok {
			resolvedArcElem := arcElem
			if c.typeSubst != nil {
				resolvedArcElem = types.Substitute(arcElem, c.typeSubst)
			}
			dropFunc = c.getOrCreateArcDrop(resolvedArcElem)
		}
		isContainer = true
	case innerNamed != nil && (func() bool { _, ok := types.AsWeak(elem); return ok }() || innerNamed == types.TypWeak):
		// T0157: Weak inner drop
		if weakElem, ok := types.AsWeak(elem); ok {
			resolvedWeakElem := weakElem
			if c.typeSubst != nil {
				resolvedWeakElem = types.Substitute(weakElem, c.typeSubst)
			}
			dropFunc = c.getOrCreateWeakDrop(resolvedWeakElem)
		}
		isContainer = true
	case innerNamed != nil && (func() bool { _, ok := types.AsMutex(elem); return ok }() || innerNamed == types.TypMutex):
		// T0156: Mutex inner drop
		if mutexElem, ok := types.AsMutex(elem); ok {
			resolvedElem := mutexElem
			if c.typeSubst != nil {
				resolvedElem = types.Substitute(mutexElem, c.typeSubst)
			}
			dropFunc = c.getOrCreateMutexDrop(resolvedElem)
		}
		isContainer = true
	case innerNamed != nil && (func() bool { _, ok := types.AsMutexGuard(elem); return ok }() || innerNamed == types.TypMutexGuard):
		// T0156: MutexGuard inner drop
		dropFunc = c.funcs["MutexGuard.drop"]
		isContainer = true
	case innerNamed != nil && (innerNamed.HasDrop() || innerNamed.NeedsSynthDrop()):
		ownerName := innerNamed.Obj().Name()
		resolvedElem := elem
		if c.typeSubst != nil {
			resolvedElem = types.Substitute(elem, c.typeSubst)
		}
		if inst, ok := resolvedElem.(*types.Instance); ok {
			ownerName = monoName(inst)
		} else if innerNamed.HasDrop() && !innerNamed.NeedsSynthDrop() {
			ownerName = c.resolveDropOwner(innerNamed)
		}
		mangledName := mangleMethodName(ownerName, "drop", false)
		dropFunc = c.funcs[mangledName]
	default:
		return // inner type not droppable
	}

	if dropFunc == nil {
		return
	}

	// result is {i1, T} — extract tag and conditionally drop inner value.
	tag := c.block.NewExtractValue(result, 0)
	dropBlock := c.newBlock("discard.drop")
	skipBlock := c.newBlock("discard.skip")
	c.block.NewCondBr(tag, dropBlock, skipBlock)

	c.block = dropBlock
	innerVal := c.block.NewExtractValue(result, 1)

	if innerNamed == types.TypString || isContainer {
		// String and containers store raw i8* — call drop directly.
		c.block.NewCall(dropFunc, innerVal)
	} else {
		// User type: inner is value struct {vtable, instance} — extract instance ptr.
		instance := c.extractInstancePtr(innerVal)
		nullCheck := c.block.NewICmp(enum.IPredEQ, instance, constant.NewNull(irtypes.I8Ptr))
		execBlock := c.newBlock("discard.exec")
		nullSkip := c.newBlock("discard.null")
		c.block.NewCondBr(nullCheck, nullSkip, execBlock)

		c.block = execBlock
		c.block.NewCall(dropFunc, instance)
		// B0159: Free the instance struct after drop() completes.
		if innerNamed != nil && !innerNamed.NeedsSynthDrop() {
			c.block.NewCall(c.palFree, instance)
		}
		c.block.NewBr(nullSkip)

		c.block = nullSkip
	}
	c.block.NewBr(skipBlock)

	c.block = skipBlock
}

// dropDiscardedHeapType handles B0211: when an ExprStmt discards a heap-allocated
// user type constructor result (e.g., `Foo(x: 1);`), the instance leaks.
// Only handles constructor calls — method/getter returns may share instance
// pointers with existing objects, so freeing them would cause use-after-free.
func (c *Compiler) dropDiscardedHeapType(expr ast.Expr, result value.Value) {
	if result == nil || c.block == nil || c.block.Term != nil {
		return
	}
	// Only handle constructor calls (CallExpr whose callee resolves to a type).
	callExpr, isCall := expr.(*ast.CallExpr)
	if !isCall {
		return
	}
	calleeType := c.info.Types[callExpr.Callee]
	if c.typeSubst != nil && calleeType != nil {
		calleeType = types.Substitute(calleeType, c.typeSubst)
	}
	switch calleeType.(type) {
	case *types.Named, *types.Instance:
		// Constructor call — proceed
	default:
		return // Not a constructor
	}

	exprType := c.info.Types[expr]
	if exprType == nil {
		return
	}
	if c.typeSubst != nil {
		exprType = types.Substitute(exprType, c.typeSubst)
	}
	// Only handle user types with value struct layout {i8*, i8*}
	named := extractNamed(exprType)
	if named == nil || named.IsValueType() || named.IsCopy() || isPrimitiveScalar(named) || named.IsStructural() {
		return
	}
	// Skip containers and strings — handled by trackStringTemp
	if isContainerType(exprType) || named == types.TypString {
		return
	}
	// Must be a struct value to extract instance pointer
	if _, ok := result.Type().(*irtypes.StructType); !ok {
		return
	}

	// T0346: Claim any existing heap temp tracking this allocation (e.g.,
	// genConstructorCallMono's palFree track at expr.go:1903) so cleanupHeapTemps
	// doesn't double-free what we're about to free explicitly.
	c.claimHeapTemp(result)

	instance := c.extractInstancePtr(result)
	nullCheck := c.block.NewICmp(enum.IPredEQ, instance, constant.NewNull(irtypes.I8Ptr))
	freeBlock := c.newBlock("discard.heap.free")
	doneBlock := c.newBlock("discard.heap.done")
	c.block.NewCondBr(nullCheck, doneBlock, freeBlock)

	c.block = freeBlock
	if dropFunc := c.resolveDropFuncForTemp(named, exprType); dropFunc != nil && dropFunc != c.palFree {
		c.block.NewCall(dropFunc, instance)
		// Explicit drop (not synth) doesn't include pal_free
		if named.HasDrop() && !named.NeedsSynthDrop() {
			c.block.NewCall(c.palFree, instance)
		}
	} else {
		c.block.NewCall(c.palFree, instance)
	}
	c.block.NewBr(doneBlock)

	c.block = doneBlock
}

// emitStringDropCall emits a conditional promise_string_drop call for a string variable.
// String allocas store raw i8* (instance pointer), not a value struct — so we load
// the i8* directly and pass it to promise_string_drop (which checks the literal flag).
// B0189: For vectors with droppable elements, emits an element-drop loop before freeing
// the buffer. This handles Vector[string], Vector[Vector[T]], Vector[Channel[T]], and
// vectors of user types with drop().
func (c *Compiler) emitStringDropCall(b scopeBinding) {
	if b.dropFlag == nil {
		panic("codegen: string drop binding must have a drop flag")
	}

	flag := c.block.NewLoad(irtypes.I1, b.dropFlag)
	dropBlock := c.newBlock("strdrop.call")
	skipBlock := c.newBlock("strdrop.skip")
	c.block.NewCondBr(flag, dropBlock, skipBlock)

	c.block = dropBlock
	ptr := c.block.NewLoad(b.alloca.ElemType, b.alloca)

	// Null-check: zero-initialized values from error handler fallthrough
	nullCheck := c.block.NewICmp(enum.IPredEQ, ptr, constant.NewNull(irtypes.I8Ptr))
	execBlock := c.newBlock("strdrop.exec")
	doneBlock := c.newBlock("strdrop.done")
	c.block.NewCondBr(nullCheck, doneBlock, execBlock)

	c.block = execBlock

	// B0203: For vectors, check the static flag (bit 63 of len). Passthrough
	// variadic vectors are marked static at the call site to prevent the callee
	// from dropping the caller's vector and its elements. Static .rodata vectors
	// also benefit (Vector.drop already checked bit 63, but element drops did not).
	valType := b.valType
	if c.typeSubst != nil {
		valType = types.Substitute(valType, c.typeSubst)
	}
	if _, isVec := types.AsVector(valType); isVec || (b.named != nil && b.named == types.TypVector) {
		headerType := vectorHeaderType()
		headerPtr := c.block.NewBitCast(ptr, irtypes.NewPointer(headerType))
		rawLen := loadVectorLenRaw(c.block, headerPtr)
		bit63 := c.block.NewAnd(rawLen, constant.NewInt(irtypes.I64, math.MinInt64))
		isStatic := c.block.NewICmp(enum.IPredNE, bit63, constant.NewInt(irtypes.I64, 0))
		nonStaticBlock := c.newBlock("vecdrop.nonstatic")
		c.block.NewCondBr(isStatic, doneBlock, nonStaticBlock)
		c.block = nonStaticBlock
	}

	// B0189: Drop vector elements before freeing the buffer.
	c.emitVectorElementDrops(b, ptr)

	// T0668: a direct `task[T] t = go {…}` binding reuses this bindingDropString
	// path (same i8* alloca + void(i8*) drop shape). In a coroutine body (test
	// body / WASM main / go {}) route the un-awaited-Task scope-exit drop
	// through the cooperative park-suspend join so the single-threaded WASM
	// scheduler can run the pending goroutine instead of livelocking.
	if _, isTask := types.AsTask(valType); (isTask || (b.named != nil && b.named == types.TypTask)) &&
		c.emitTaskJoinAndFreeByDropFn(ptr, b.dropFunc) {
		c.block.NewBr(doneBlock)
		c.block = doneBlock
		c.block.NewBr(skipBlock)
		c.block = skipBlock
		return
	}

	c.block.NewCall(b.dropFunc, ptr)
	c.block.NewBr(doneBlock)

	c.block = doneBlock
	c.block.NewBr(skipBlock)

	c.block = skipBlock
}

// emitStringDropOldValue conditionally drops the previous string at a non-local
// compound-assignment site. Mirrors the local-var drop pattern from T0357: the
// alias check is a no-op at runtime because promise_string_concat always
// allocates a fresh result (current and result never alias), but it keeps the
// emitted IR shape consistent across compound sites.
func (c *Compiler) emitStringDropOldValue(current, result value.Value) {
	dropFn, ok := c.funcs["promise_string_drop"]
	if !ok {
		return
	}
	diffBlk := c.newBlock("compound.strdrop.diff")
	mergeBlk := c.newBlock("compound.strdrop.merge")
	isSame := c.block.NewICmp(enum.IPredEQ, current, result)
	c.block.NewCondBr(isSame, mergeBlk, diffBlk)
	c.block = diffBlk
	c.block.NewCall(dropFn, current)
	c.block.NewBr(mergeBlk)
	c.block = mergeBlk
}

// hasVectorStringBinding returns true if there's at least one Vector[string]
// binding in the current scope that would trigger element drops.
// B0189: Used to determine if a string return value needs duping.
func (c *Compiler) hasVectorStringBinding() bool {
	for _, b := range c.scopeBindings {
		if b.kind != bindingDropString {
			continue
		}
		valType := b.valType
		if c.typeSubst != nil {
			valType = types.Substitute(valType, c.typeSubst)
		}
		if elemType, isVec := types.AsVector(valType); isVec {
			resolvedElem := elemType
			if c.typeSubst != nil {
				resolvedElem = types.Substitute(resolvedElem, c.typeSubst)
			}
			if extractNamed(resolvedElem) == types.TypString {
				return true
			}
		}
	}
	return false
}

// emitVectorElementDrops emits a loop that drops each element in a vector if the
// element type is droppable. Called before Vector.drop frees the buffer.
// B0189: Fixes memory leak where Vector[string] drop didn't free string elements.
func (c *Compiler) emitVectorElementDrops(b scopeBinding, vecPtr value.Value) {
	valType := b.valType
	if c.typeSubst != nil {
		valType = types.Substitute(valType, c.typeSubst)
	}
	elemType, isVec := types.AsVector(valType)
	if !isVec {
		return
	}
	c.emitVectorElementDropLoop(vecPtr, elemType)
}

// emitVectorElementDropLoop emits a loop that iterates vector elements and drops
// each one. Shared by scope-exit drops (emitVectorElementDrops) and field drops
// (emitFieldDrops). The elemType must already have type substitution applied.
// B0189: Fixes memory leak where Vector[string] drop didn't free string elements.
// B0212: Extended to also drop enum elements with synthesized drops.
func (c *Compiler) emitVectorElementDropLoop(vecPtr value.Value, elemType types.Type) {
	if c.typeSubst != nil {
		elemType = types.Substitute(elemType, c.typeSubst)
	}
	// B0189: String elements are safe to drop (push dups them).
	// B0212: Enum elements stored by value in vectors — each element is an
	// independent copy of the enum internal type, so dropping each is safe.
	// B0245: Heap user type elements (non-value, non-copy, non-primitive, non-structural)
	// are also dropped via pal_free or their drop method. Vector elements are the sole
	// owner of user-type instances — constructors transfer ownership to push, and
	// sort temps clear their drop flags when moved back into the vector.
	// T0741: closure (function value) elements own a heap env struct that
	// emitVariantFieldDrop's Signature case frees.
	_, isSig := elemType.(*types.Signature)
	if extractNamed(elemType) != types.TypString && !isSig {
		if !c.vecElemNeedsEnumDrop(elemType) && !c.vecElemNeedsUserTypeDrop(elemType) && !c.tupleNeedsDrop(elemType) && !c.vecElemNeedsOptionalDrop(elemType) {
			return
		}
	}
	c.emitVectorElementDropLoopBody(vecPtr, elemType)
}

// emitVectorElementDropLoopBody is the shared implementation for vector element drop loops.
func (c *Compiler) emitVectorElementDropLoopBody(vecPtr value.Value, elemType types.Type) {
	elemLLVM := c.resolveType(elemType)

	// Load vector length (masked — clears static flag bit 63)
	headerType := vectorHeaderType()
	headerPtr := c.block.NewBitCast(vecPtr, irtypes.NewPointer(headerType))
	length := loadVectorLen(c.block, headerPtr)

	// Data starts at offset vectorHeaderSize (16 bytes after buffer start)
	dataBase := c.block.NewGetElementPtr(irtypes.I8, vecPtr,
		constant.NewInt(irtypes.I64, int64(vectorHeaderSize)))
	dataTypedPtr := c.block.NewBitCast(dataBase, irtypes.NewPointer(elemLLVM))

	// Loop: for i = 0; i < len; i++ { drop(elements[i]); }
	loopHead := c.newBlock("vecdrop.head")
	loopBody := c.newBlock("vecdrop.body")
	loopDone := c.newBlock("vecdrop.done")

	// Initialize counter
	idxAlloca := c.createEntryAlloca(irtypes.I64)
	idxAlloca.SetName(c.uniqueLocalName("vecdrop.idx"))
	c.block.NewStore(constant.NewInt(irtypes.I64, 0), idxAlloca)
	c.block.NewBr(loopHead)

	// Loop head: check i < len
	c.block = loopHead
	idx := c.block.NewLoad(irtypes.I64, idxAlloca)
	cond := c.block.NewICmp(enum.IPredULT, idx, length)
	c.block.NewCondBr(cond, loopBody, loopDone)

	// Loop body: drop element[i], increment i
	c.block = loopBody
	idx2 := c.block.NewLoad(irtypes.I64, idxAlloca)
	elemPtr := c.block.NewGetElementPtr(elemLLVM, dataTypedPtr, idx2)
	elemVal := c.block.NewLoad(elemLLVM, elemPtr)

	c.emitVariantFieldDrop(elemVal, elemType)

	// Increment counter
	nextIdx := c.block.NewAdd(idx2, constant.NewInt(irtypes.I64, 1))
	c.block.NewStore(nextIdx, idxAlloca)
	c.block.NewBr(loopHead)

	c.block = loopDone
}

// emitVectorStringDupLoop iterates a vector's string elements and replaces each with
// a deep copy (dupString). Used by Vector.clone() to ensure the cloned vector owns
// independent copies of all string elements. T0154.
func (c *Compiler) emitVectorStringDupLoop(vecPtr value.Value, elemType types.Type) {
	elemLLVM := c.resolveType(elemType)

	// Load vector length (masked — clears static flag bit 63)
	headerType := vectorHeaderType()
	headerPtr := c.block.NewBitCast(vecPtr, irtypes.NewPointer(headerType))
	length := loadVectorLen(c.block, headerPtr)

	// Data starts at offset vectorHeaderSize (16 bytes after buffer start)
	dataBase := c.block.NewGetElementPtr(irtypes.I8, vecPtr,
		constant.NewInt(irtypes.I64, int64(vectorHeaderSize)))
	dataTypedPtr := c.block.NewBitCast(dataBase, irtypes.NewPointer(elemLLVM))

	// Loop: for i = 0; i < len; i++ { elements[i] = dupString(elements[i]); }
	loopHead := c.newBlock("vecdup_str.head")
	loopBody := c.newBlock("vecdup_str.body")
	loopDone := c.newBlock("vecdup_str.done")

	idxAlloca := c.createEntryAlloca(irtypes.I64)
	idxAlloca.SetName(c.uniqueLocalName("vecdup_str.idx"))
	c.block.NewStore(constant.NewInt(irtypes.I64, 0), idxAlloca)
	c.block.NewBr(loopHead)

	c.block = loopHead
	idx := c.block.NewLoad(irtypes.I64, idxAlloca)
	cond := c.block.NewICmp(enum.IPredULT, idx, length)
	c.block.NewCondBr(cond, loopBody, loopDone)

	c.block = loopBody
	idx2 := c.block.NewLoad(irtypes.I64, idxAlloca)
	elemPtr := c.block.NewGetElementPtr(elemLLVM, dataTypedPtr, idx2)
	elemVal := c.block.NewLoad(elemLLVM, elemPtr)
	duped := c.dupString(elemVal)
	c.block.NewStore(duped, elemPtr)

	nextIdx := c.block.NewAdd(idx2, constant.NewInt(irtypes.I64, 1))
	c.block.NewStore(nextIdx, idxAlloca)
	c.block.NewBr(loopHead)

	c.block = loopDone
}

// emitVectorElementCloneLoop iterates a cloned vector's elements and deep-clones
// each non-copy element so the cloned vector owns independent copies. B0275.
// Handles: strings (dupString), channels (dupChannel), nested vectors (dupVector +
// recursive clone), heap user types (clone method or dupHeapValue fallback),
// enum types with clone methods (B0244), and droppable enums without clone (B0290).
func (c *Compiler) emitVectorElementCloneLoop(vecPtr value.Value, elemType types.Type) {
	named := extractNamed(elemType)
	// B0244: Check for enum types with clone — not caught by extractNamed.
	// B0290: Also detect droppable enums without clone (e.g., Slot[K,V] in Map).
	isCloneableEnum := false
	isDupableEnum := false
	if named == nil {
		if enum := extractEnum(elemType); enum != nil {
			_, isCloneableEnum = c.funcs[c.enumCloneFuncName(enum, elemType)]
			if !isCloneableEnum {
				isDupableEnum = c.vecElemNeedsEnumDrop(elemType)
			}
		}
		if !isCloneableEnum && !isDupableEnum {
			return // primitive/copy type — shallow memcpy is correct
		}
	}

	// String: delegate to existing string dup loop
	if named == types.TypString {
		c.emitVectorStringDupLoop(vecPtr, elemType)
		return
	}

	// T0559 + T0545: single-owner native handles (Task/Mutex/MutexGuard) are
	// move-only i8* handles with no clone semantics. T0545's sema gate rejects
	// clone()/filled()/nesting on containers transitively containing them, so
	// well-formed user code never reaches here. This is the codegen backstop
	// for the residual generic-indirection path (T0616) — sema checks generic
	// bodies with unbound T, so dup[T](Vector[T]) instantiated with T=Task can
	// still reach this. Emit a length-guarded runtime panic (T0559) rather
	// than a silent shallow-copy: empty vectors clone trivially (no
	// double-ownership), non-empty would double-free at drop, so panic with a
	// type-specific message instead of falling through to dupHeapValue (which
	// Go-panics on the i8* → StructType cast).
	unclonableTypeName := ""
	if _, isTask := types.AsTask(elemType); isTask || named == types.TypTask {
		unclonableTypeName = "Task"
	} else if _, isMutex := types.AsMutex(elemType); isMutex || named == types.TypMutex {
		unclonableTypeName = "Mutex"
	} else if _, isMG := types.AsMutexGuard(elemType); isMG || named == types.TypMutexGuard {
		unclonableTypeName = "MutexGuard"
	}
	if unclonableTypeName != "" {
		headerType := vectorHeaderType()
		headerPtr := c.block.NewBitCast(vecPtr, irtypes.NewPointer(headerType))
		length := loadVectorLen(c.block, headerPtr)
		isEmpty := c.block.NewICmp(enum.IPredEQ, length, constant.NewInt(irtypes.I64, 0))
		okBlock := c.newBlock("vecclone.unsup.ok")
		panicBlock := c.newBlock("vecclone.unsup.panic")
		c.block.NewCondBr(isEmpty, okBlock, panicBlock)

		c.block = panicBlock
		panicMsg := c.makeGlobalString(fmt.Sprintf(
			"Vector[%s[T]].clone() is not supported; %s is move-only",
			unclonableTypeName, unclonableTypeName))
		c.block.NewCall(c.funcs["promise_panic"], panicMsg)
		c.emitPanicReturn()

		c.block = okBlock
		return
	}

	// Determine if element type needs cloning
	_, isCh := types.AsChannel(elemType)
	innerElem, isVec := types.AsVector(elemType)
	_, isArc := types.AsArc(elemType)
	weakElem, isWk := types.AsWeak(elemType)
	isChannel := !isCloneableEnum && !isDupableEnum && (isCh || named == types.TypChannel)
	isVector := !isCloneableEnum && !isDupableEnum && (isVec || named == types.TypVector)
	isArcType := !isCloneableEnum && !isDupableEnum && (isArc || named == types.TypArc)
	isWeakType := !isCloneableEnum && !isDupableEnum && (isWk || named == types.TypWeak)
	isHeapUser := !isCloneableEnum && !isDupableEnum && named != nil && !named.IsValueType() && !named.IsCopy() && !isPrimitiveScalar(named) && !named.IsStructural()

	if !isChannel && !isVector && !isArcType && !isWeakType && !isHeapUser && !isCloneableEnum && !isDupableEnum {
		return // value/copy type — shallow memcpy is correct
	}

	// Emit loop: for i = 0; i < len; i++ { elements[i] = clone(elements[i]); }
	elemLLVM := c.resolveType(elemType)

	headerType := vectorHeaderType()
	headerPtr := c.block.NewBitCast(vecPtr, irtypes.NewPointer(headerType))
	length := loadVectorLen(c.block, headerPtr)

	dataBase := c.block.NewGetElementPtr(irtypes.I8, vecPtr,
		constant.NewInt(irtypes.I64, int64(vectorHeaderSize)))
	dataTypedPtr := c.block.NewBitCast(dataBase, irtypes.NewPointer(elemLLVM))

	loopHead := c.newBlock("vecclone.head")
	loopBody := c.newBlock("vecclone.body")
	loopDone := c.newBlock("vecclone.done")

	idxAlloca := c.createEntryAlloca(irtypes.I64)
	idxAlloca.SetName(c.uniqueLocalName("vecclone.idx"))
	c.block.NewStore(constant.NewInt(irtypes.I64, 0), idxAlloca)
	c.block.NewBr(loopHead)

	c.block = loopHead
	idx := c.block.NewLoad(irtypes.I64, idxAlloca)
	cond := c.block.NewICmp(enum.IPredULT, idx, length)
	c.block.NewCondBr(cond, loopBody, loopDone)

	c.block = loopBody
	idx2 := c.block.NewLoad(irtypes.I64, idxAlloca)
	elemPtr := c.block.NewGetElementPtr(elemLLVM, dataTypedPtr, idx2)

	if isDupableEnum {
		// B0290: Droppable enum without clone — dup variant fields in place.
		c.dupEnumElementInPlace(elemPtr, elemType)
	} else {
		elemVal := c.block.NewLoad(elemLLVM, elemPtr)

		var cloned value.Value
		if isCloneableEnum {
			// B0244: Enum with clone — deep-copy via clone method.
			cloned, _ = c.cloneEnumValue(elemVal, elemType)
		} else if isChannel {
			cloned = c.dupChannel(elemVal)
		} else if isArcType {
			cloned = c.dupArc(elemVal)
		} else if isWeakType {
			resolvedWeakElem := weakElem
			if c.typeSubst != nil {
				resolvedWeakElem = types.Substitute(weakElem, c.typeSubst)
			}
			cloned = c.dupWeak(elemVal, resolvedWeakElem)
		} else if isVector {
			if isVec {
				innerLLVM := c.resolveType(innerElem)
				innerSize := int64(c.typeSize(innerLLVM))
				cloned = c.dupVector(elemVal, innerSize)
				// Recursively clone inner vector's elements
				c.emitVectorElementCloneLoop(cloned, innerElem)
			} else {
				cloned = c.dupVector(elemVal, 0)
			}
		} else {
			// Heap user type: try clone() method, fall back to dupHeapValue
			cloned = c.cloneHeapElement(elemVal, elemType, named)
		}
		c.block.NewStore(cloned, elemPtr)
	}

	nextIdx := c.block.NewAdd(idx2, constant.NewInt(irtypes.I64, 1))
	c.block.NewStore(nextIdx, idxAlloca)
	c.block.NewBr(loopHead)

	c.block = loopDone
}

// cloneHeapElement clones a single heap user type element by calling its clone()
// method if available, otherwise falling back to dupHeapValue. B0275.
func (c *Compiler) cloneHeapElement(elemVal value.Value, elemType types.Type, named *types.Named) value.Value {
	// T0545 backstop: single-owner native handles have no clone semantics and
	// are i8* (not the {vtable,instance} struct dupHeapValue expects). Return
	// the handle unchanged rather than asserting/panicking.
	if isSingleOwnerHandleType(elemType) {
		return elemVal
	}
	// Resolve clone method name
	ownerName := c.resolveMethodOwner(named, "clone")
	if inst, ok := elemType.(*types.Instance); ok {
		ownerName = monoName(inst)
	}
	mangledName := mangleMethodName(ownerName, "clone", false)
	if cloneFn, ok := c.funcs[mangledName]; ok {
		// B0289: For generic instances, verify all type arguments can be safely
		// handled by the clone's internal match-dup. Container clone methods (Map, Set)
		// iterate elements via match destructure — if any type argument can't be
		// safely match-dup'd, the clone would be shallow → fall back to dupHeapValue.
		if inst, ok := elemType.(*types.Instance); ok {
			for _, arg := range inst.TypeArgs() {
				if c.typeSubst != nil {
					arg = types.Substitute(arg, c.typeSubst)
				}
				if !c.typeArgSafeForCloneDup(arg) {
					return c.dupHeapValue(elemVal, elemType)
				}
			}
		}
		instance := c.extractInstancePtr(elemVal)
		return c.block.NewCall(cloneFn, instance)
	}

	// No clone method — fall back to dupHeapValue (alloc + memcpy + sub-field dup,
	// which is already null-safe internally).
	return c.dupHeapValue(elemVal, elemType)
}

// vecElemNeedsEnumDrop returns true if a vector element type is an enum that has
// a drop function available. Checks both sema-time HasDrop (non-generic enums) and
// codegen-time mono synth drops (generic enum instances like Slot[string, JsonValue]).
// B0212: Enables vector element drop loop to clean up enum elements.
func (c *Compiler) vecElemNeedsEnumDrop(elemType types.Type) bool {
	enum := extractEnum(elemType)
	if enum == nil {
		return false
	}
	// Non-generic enum with sema-detected drop
	if enum.HasDrop() {
		return true
	}
	// Generic enum instance — check if the mono drop function was generated
	if inst, ok := elemType.(*types.Instance); ok {
		mangledName := mangleMethodName(monoName(inst), "drop", false)
		if _, ok := c.funcs[mangledName]; ok {
			return true
		}
	}
	return false
}

// vecElemNeedsUserTypeDrop returns true if a vector element type is a heap user type
// that needs drop or pal_free. Covers: types with explicit/synthesized drops, mono
// instances with codegen-time drops, and plain heap user types (non-value, non-copy,
// non-primitive, non-structural) that need pal_free.
// B0245: Enables vector element drop loop to clean up user-type elements.
func (c *Compiler) vecElemNeedsUserTypeDrop(elemType types.Type) bool {
	named := extractNamed(elemType)
	if named == nil {
		return false
	}
	// Types with explicit or synthesized drop
	if named.HasDrop() || named.NeedsSynthDrop() {
		return true
	}
	// Mono instance with codegen-time synthesized drop
	if inst, ok := elemType.(*types.Instance); ok {
		if n, ok2 := inst.Origin().(*types.Named); ok2 && n.NeedsSynthDrop() {
			return true
		}
		mangledName := mangleMethodName(monoName(inst), "drop", false)
		if _, ok := c.funcs[mangledName]; ok {
			return true
		}
	}
	// Heap user type without any drop — needs pal_free
	if !named.IsValueType() && !named.IsCopy() && !isPrimitiveScalar(named) && !named.IsStructural() {
		return true
	}
	return false
}

// vecElemNeedsOptionalDrop returns true if a vector element type is Optional[T]
// where T is a droppable type. Enables emitVectorElementDropLoop to walk Optional
// elements and drop their inner payloads via emitOptionalValueDrop.
// T0620: Closes Gap B — without this, Vector[T?] drop skips inner payload drops.
func (c *Compiler) vecElemNeedsOptionalDrop(elemType types.Type) bool {
	opt, ok := elemType.(*types.Optional)
	if !ok {
		return false
	}
	inner := opt.Elem()
	if c.typeSubst != nil {
		inner = types.Substitute(inner, c.typeSubst)
	}
	return c.typeNeedsFieldDrop(inner)
}

// arrayFieldNeedsDrop returns true if a fixed-size array type has a droppable
// element type. Used by emitFieldDropsFor to skip non-droppable arrays (e.g.
// int[3]) instead of emitting an empty per-element loop. (T0579)
func (c *Compiler) arrayFieldNeedsDrop(arr *types.Array) bool {
	elem := arr.Elem()
	if c.typeSubst != nil {
		elem = types.Substitute(elem, c.typeSubst)
	}
	return c.typeNeedsFieldDrop(elem)
}

// typeNeedsFieldDrop returns true if a single value of typ has any drop work
// (string, vector, channel, heap user type, droppable tuple/array, Optional
// wrapping a droppable inner, enum with drop, etc.). Used by tuple/array field
// drop predicates. (T0579)
func (c *Compiler) typeNeedsFieldDrop(typ types.Type) bool {
	if c.typeSubst != nil {
		typ = types.Substitute(typ, c.typeSubst)
	}
	if tup, ok := typ.(*types.Tuple); ok {
		return c.tupleNeedsDrop(tup)
	}
	if arr, ok := typ.(*types.Array); ok {
		return c.arrayFieldNeedsDrop(arr)
	}
	if opt, ok := typ.(*types.Optional); ok {
		return c.typeNeedsFieldDrop(opt.Elem())
	}
	// T0741: Closure fields own a heap env struct that must be deep-dropped.
	// Covers tuple/array struct fields carrying a closure via tupleNeedsDrop /
	// arrayFieldNeedsDrop.
	if _, ok := typ.(*types.Signature); ok {
		return true
	}
	if named := extractNamed(typ); named != nil {
		if named == types.TypString || named.HasDrop() || named.NeedsSynthDrop() {
			return true
		}
		if _, isVec := types.AsVector(typ); isVec {
			return true
		}
		if _, isCh := types.AsChannel(typ); isCh {
			return true
		}
		if _, isArc := types.AsArc(typ); isArc {
			return true
		}
		if _, isWeak := types.AsWeak(typ); isWeak {
			return true
		}
		if _, isMutex := types.AsMutex(typ); isMutex {
			return true
		}
		if types.IsMutexGuard(typ) || named == types.TypMutexGuard {
			return true
		}
		if !named.IsValueType() && !named.IsCopy() && !isPrimitiveScalar(named) && !named.IsStructural() {
			return true
		}
	}
	if c.vecElemNeedsEnumDrop(typ) {
		return true
	}
	return false
}

// tupleNeedsDrop returns true if a tuple type contains any droppable element
// (string, vector, channel, user type with drop, enum with drop, droppable
// Optional/Array, or another droppable tuple).
// B0264: Enables vector element drop loop to clean up tuple elements.
// T0371: Recurses into nested tuples so e.g. ((int, string), int) is droppable.
// T0578: Delegates per-element check to typeNeedsFieldDrop so Optional, Array,
// Mutex, and MutexGuard tuple elements are recognized as droppable.
func (c *Compiler) tupleNeedsDrop(elemType types.Type) bool {
	tup, ok := elemType.(*types.Tuple)
	if !ok {
		return false
	}
	for _, e := range tup.Elems() {
		resolved := e
		if c.typeSubst != nil {
			resolved = types.Substitute(resolved, c.typeSubst)
		}
		if c.typeNeedsFieldDrop(resolved) {
			return true
		}
	}
	return false
}

// isDroppableHeapUserType returns true if typ is a heap user type whose instance
// is heap-allocated and registered for drop or pal_free at scope exit — i.e., the
// kind of type whose alias would cause double-free after a container index read.
// Excludes strings (handled by dupStringFieldAccess), containers/Arc/Weak
// (dupContainerFieldAccess), tuples (dupTupleFieldAccess), and
// borrow/value/Copy/primitive/structural types. T0398.
//
// T0440: Also excludes Map and Set — these are user-defined generic containers
// with their own clone() methods that don't reliably deep-clone V values (Map's
// clone uses `result[k] = v` which shallow-copies value-structs for nested heap
// types). Treating them as plain heap user types here would route through the
// problematic clone path; instead, we leave them to the existing aliasing
// behavior at the if-let/force-unwrap site.
func isDroppableHeapUserType(typ types.Type) bool {
	if isRefType(typ) {
		return false
	}
	if isContainerType(typ) {
		return false
	}
	named := extractNamed(typ)
	if named == nil {
		return false
	}
	if named == types.TypString {
		return false
	}
	if named == types.TypMap {
		return false
	}
	if named.Obj() != nil && named.Obj().Name() == "Set" {
		return false
	}
	if named.IsValueType() || named.IsCopy() || isPrimitiveScalar(named) || named.IsStructural() {
		return false
	}
	// T0440: Also requires the type to have an explicit drop or synthesized drop.
	// Heap user types without any drop method use the bindingFree (pal_free) path,
	// which has a separate latent leak issue with cloned values (T0484). Restricting
	// the dup branch to types with drop methods ensures the bindingDrop path
	// (which correctly emits drop+pal_free) handles the cloned instances.
	if !named.HasDrop() && !named.NeedsSynthDrop() {
		return false
	}
	return true
}

// isMapOrSetType reports whether typ is the standard-library Map[K,V] or Set[T]
// heap container. These are heap user types deliberately excluded from
// isDroppableHeapUserType / isHeapUserNoDropPalFree (T0440), so callers that
// want to treat them as ordinary heap user types — e.g. the T0732 spawn-side
// deep dup via dupHeapValue — must recognize them explicitly.
func isMapOrSetType(typ types.Type) bool {
	named := extractNamed(typ)
	if named == nil {
		return false
	}
	if named == types.TypMap {
		return true
	}
	if named.Obj() != nil && named.Obj().Name() == "Set" {
		return true
	}
	return false
}

// isHeapUserNoDropPalFree returns true for heap user types that are heap-
// allocated (and thus need pal_free at scope exit) but have no explicit `drop()`
// or synthesized drop — i.e., types excluded by `isDroppableHeapUserType` for
// the T0440 Map-clone-gating reason. Used by genArrayIndex's T0590 dup-on-read:
// arrays have no internal match-dup, so slot-to-slot / let-then-X reads must
// dup these pointers to avoid aliasing + double-free at pal_free time.
func isHeapUserNoDropPalFree(typ types.Type) bool {
	if isRefType(typ) {
		return false
	}
	if isContainerType(typ) {
		return false
	}
	named := extractNamed(typ)
	if named == nil {
		return false
	}
	if named == types.TypString {
		return false
	}
	if named == types.TypMap {
		return false
	}
	if named.Obj() != nil && named.Obj().Name() == "Set" {
		return false
	}
	if named.IsValueType() || named.IsCopy() || isPrimitiveScalar(named) || named.IsStructural() {
		return false
	}
	// Drop / synth-drop case is handled by `isDroppableHeapUserType` — this
	// helper covers only the pal_free-only complement.
	if named.HasDrop() || named.NeedsSynthDrop() {
		return false
	}
	return true
}

// dupTupleValue creates a deep copy of a tuple value by dup'ing each droppable
// field (strings, vectors, channels, nested tuples, heap user types, enums).
// Non-droppable fields (primitives, value types) are copied by struct value.
// Used when reading a tuple from a container (`t := v[0]`) so the result is
// independently owned and can be safely dropped without affecting the
// container's element. Symmetric with the Vector[string] dup-on-read pattern
// (B0204) and the Vector[user heap type] cloneHeapElement pattern (B0275).
// T0370.
func (c *Compiler) dupTupleValue(tupVal value.Value, tup *types.Tuple) value.Value {
	result := tupVal
	for i, fieldType := range tup.Elems() {
		resolved := fieldType
		if c.typeSubst != nil {
			resolved = types.Substitute(resolved, c.typeSubst)
		}
		elemVal := c.block.NewExtractValue(result, uint64(i))
		var dupped value.Value
		if innerTup, isTup := resolved.(*types.Tuple); isTup {
			if c.tupleNeedsDrop(resolved) {
				dupped = c.dupTupleValue(elemVal, innerTup)
			}
		} else if extractNamed(resolved) == types.TypString && !isRefType(resolved) {
			dupped = c.dupString(elemVal)
		} else {
			// Vectors, channels, heap user types, droppable enums: delegate.
			dupped = c.maybeDupPushElement(elemVal, resolved)
		}
		if dupped != nil {
			result = c.block.NewInsertValue(result, dupped, uint64(i))
		}
	}
	return result
}

// emitFreeCall emits a conditional pal_free call for a heap-allocated user type
// that has no drop method. Checks the drop flag and null-checks the instance pointer.
func (c *Compiler) emitFreeCall(b scopeBinding) {
	if b.dropFlag == nil {
		return
	}

	flag := c.block.NewLoad(irtypes.I1, b.dropFlag)
	freeBlock := c.newBlock("free.call")
	skipBlock := c.newBlock("free.skip")
	c.block.NewCondBr(flag, freeBlock, skipBlock)

	c.block = freeBlock
	val := c.block.NewLoad(b.alloca.ElemType, b.alloca)
	// B0222: Raw i8* instance pointer (from promoted heapTemp). Value struct
	// allocas need extractInstancePtr to get field 1; i8* allocas are the pointer.
	var instance value.Value
	if b.alloca.ElemType == irtypes.I8Ptr {
		instance = val
	} else {
		instance = c.extractInstancePtr(val)
	}

	// Null-check: zero-initialized values from error handler fallthrough
	nullCheck := c.block.NewICmp(enum.IPredEQ, instance, constant.NewNull(irtypes.I8Ptr))
	execBlock := c.newBlock("free.exec")
	doneBlock := c.newBlock("free.done")
	c.block.NewCondBr(nullCheck, doneBlock, execBlock)

	c.block = execBlock
	if b.dropFunc != nil {
		// T0127: Custom cleanup function (e.g., __promise_iter_cleanup for structural
		// interface variables from iterator chains). The cleanup function frees nested
		// allocations (closure env) and the instance itself.
		c.block.NewCall(b.dropFunc, instance)
	} else if b.rttiDrop {
		// B0272: Structural interface variables whose backing instance has RTTI layout.
		// Use RTTI-based drop dispatch to properly clean up all fields (e.g., string
		// fields) before freeing — raw pal_free would leak nested allocations.
		// Only set for bindings where the instance has standard RTTI (not _FnIter etc.).
		c.emitStructuralInstanceDrop(instance)
	} else {
		c.block.NewCall(c.palFree, instance)
	}
	c.block.NewBr(doneBlock)

	c.block = doneBlock
	c.block.NewBr(skipBlock)

	c.block = skipBlock
}

// emitEnvFree frees a closure's env struct at scope exit.
// Checks the drop flag (has the closure been moved?) and null-checks the env pointer.
func (c *Compiler) emitEnvFree(b scopeBinding) {
	if b.dropFlag == nil {
		return
	}
	flag := c.block.NewLoad(irtypes.I1, b.dropFlag)
	freeBlock := c.newBlock("env.free")
	skipBlock := c.newBlock("env.skip")
	c.block.NewCondBr(flag, freeBlock, skipBlock)

	c.block = freeBlock
	// Load closure, extract env ptr (field 1 of fat pointer)
	closure := c.block.NewLoad(b.alloca.ElemType, b.alloca)
	envPtr := c.block.NewExtractValue(closure, 1)
	// If non-null, free the env struct
	isNull := c.block.NewICmp(enum.IPredEQ, envPtr, constant.NewNull(irtypes.I8Ptr))
	callBlock := c.newBlock("env.free.call")
	c.block.NewCondBr(isNull, skipBlock, callBlock)

	c.block = callBlock
	c.emitEnvDropOrFree(envPtr)
	c.block.NewBr(skipBlock)

	c.block = skipBlock
}

// emitEnvDropOrFree loads the env drop function from the env struct header (field 0)
// and calls it if non-null, otherwise calls pal_free. B0221: env structs now store
// a drop function pointer as their first field so captured moved values can be
// properly dropped (not just the env struct freed).
// The env pointer must be non-null (caller is responsible for null-checking).
func (c *Compiler) emitEnvDropOrFree(envPtr value.Value) {
	// Load env drop fn pointer from field 0 (env struct header)
	envHeaderType := irtypes.NewStruct(irtypes.I8Ptr)
	typedHdr := c.block.NewBitCast(envPtr, irtypes.NewPointer(envHeaderType))
	dropFnField := c.block.NewGetElementPtr(envHeaderType, typedHdr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
	dropFnRaw := c.block.NewLoad(irtypes.I8Ptr, dropFnField)

	hasDrop := c.block.NewICmp(enum.IPredNE, dropFnRaw, constant.NewNull(irtypes.I8Ptr))
	callDropBlk := c.newBlock("env.deep_drop")
	justFreeBlk := c.newBlock("env.shallow_free")
	mergeBlk := c.newBlock("env.drop_done")
	c.block.NewCondBr(hasDrop, callDropBlk, justFreeBlk)

	// Call env drop function (drops captured values + frees env struct)
	c.block = callDropBlk
	envDropFnType := irtypes.NewFunc(irtypes.Void, irtypes.I8Ptr)
	typedDropFn := c.block.NewBitCast(dropFnRaw, irtypes.NewPointer(envDropFnType))
	c.block.NewCall(typedDropFn, envPtr)
	c.block.NewBr(mergeBlk)

	// No droppable captures — just free the env struct
	c.block = justFreeBlk
	c.block.NewCall(c.palFree, envPtr)
	c.block.NewBr(mergeBlk)

	c.block = mergeBlk
}

// trackStringTemp registers a heap-allocated string temporary for cleanup at
// statement end (T0073). Entry-block allocas are initialized to null/false so
// temps created inside branches have defined values on all paths.
func (c *Compiler) trackStringTemp(val value.Value) {
	c.trackTempWithDrop(val, c.funcs["promise_string_drop"])
}

// trackVectorTemp registers a vector temporary for cleanup at statement end.
// B0219: Used for vector field-read dups from droppable types.
// T0109: Also used for vector-producing calls (e.g., split()) to drop string elements.
func (c *Compiler) trackVectorTemp(val value.Value) {
	c.trackTempWithDrop(val, c.funcs["Vector.drop"])
}

// trackVectorTempWithElemType registers a vector temporary with element type info.
// When elemType is non-nil and is string, the cleanup will also drop string elements
// before freeing the vector buffer. Delegates to trackTempWithDrop, then patches elemType.
func (c *Compiler) trackVectorTempWithElemType(val value.Value, elemType types.Type) {
	prevLen := len(c.stmtTemps)
	c.trackTempWithDrop(val, c.funcs["Vector.drop"])
	// If a new temp was actually added, set its element type.
	if len(c.stmtTemps) > prevLen {
		c.stmtTemps[len(c.stmtTemps)-1].elemType = elemType
	}
}

// trackChannelTempWithElemType registers a channel temporary for cleanup at
// statement end. B0219: used for channel field-read dups from droppable types.
// T0663: unlike trackVectorTempWithElemType (which patches stmtTemp.elemType so
// cleanupStmtTemps walks elements), the per-element-type Channel[T].drop already
// walks any un-received buffered items itself — so the element type only needs
// to select the right drop function. elemType is substituted here so callers
// can pass the raw channel element type.
func (c *Compiler) trackChannelTempWithElemType(val value.Value, elemType types.Type) {
	if elemType == nil {
		return
	}
	if c.typeSubst != nil {
		elemType = types.Substitute(elemType, c.typeSubst)
	}
	c.trackTempWithDrop(val, c.getOrCreateChannelDrop(elemType))
}

// trackTempWithDrop registers a heap-allocated temporary (string/vector/channel)
// for cleanup at statement end using the specified drop function.
func (c *Compiler) trackTempWithDrop(val value.Value, dropFn *ir.Func) {
	if val == nil || c.block == nil || c.block.Term != nil {
		return
	}
	if c.entryBlock == nil {
		return
	}
	// Only track values that are actually i8* (string/vector/channel pointers).
	// Failable calls return structs like {i1, i8*} — those are NOT temps.
	if val.Type() != irtypes.I8Ptr {
		return
	}
	// Only track when explicitly enabled for the current function (T0073).
	// Set to true in defineFunc for user-defined free functions only.
	if !c.tempTrackingEnabled {
		return
	}
	// Don't double-track the same SSA value
	if _, ok := c.stmtTempMap[val]; ok {
		return
	}

	// An ordinary temp is unconditionally live where it is created (flag = 1).
	c.appendStmtTemp(val, dropFn, nil, constant.NewInt(irtypes.I1, 1))
}

// appendStmtTemp records a statement temp for cleanup at statement end: an
// entry-block i8* alloca (init null) + i1 drop flag (init false) for defined
// values on untaken paths (B0168), then stores val + liveFlag in the CURRENT
// block. liveFlag is the i1 "this temp owns its value here" flag — a constant 1
// for ordinary temps (trackTempWithDrop), or a per-branch phi for elvis results
// whose per-path value is whether the result owns the selected buffer on that path
// (trackElvisResultTemp, T0936: own a transferred local/fresh operand, borrow a
// parameter/static one).
// elemType (vector element type, nil otherwise) drives the per-element drop loop
// in cleanupStmtTemps. Callers own the guards (tempTrackingEnabled, i8* type,
// terminated-block, double-track) before calling this.
func (c *Compiler) appendStmtTemp(val value.Value, dropFn *ir.Func, elemType types.Type, liveFlag value.Value) {
	// Create entry-block allocas via createEntryAlloca (handles coroutine layout).
	// The entry block's Insts list is separate from its Term, so appending stores
	// after allocas is safe.
	alloca := c.createEntryAlloca(irtypes.I8Ptr)
	dropFlag := c.createEntryAlloca(irtypes.I1)
	c.entryBlock.NewStore(constant.NewNull(irtypes.I8Ptr), alloca)
	c.entryBlock.NewStore(constant.NewInt(irtypes.I1, 0), dropFlag)

	// Store value and set flag in current block.
	c.block.NewStore(val, alloca)
	c.block.NewStore(liveFlag, dropFlag)

	idx := len(c.stmtTemps)
	c.stmtTemps = append(c.stmtTemps, stmtTemp{alloca: alloca, dropFlag: dropFlag, dropFunc: dropFn, elemType: elemType})
	c.stmtTempMap[val] = idx
}

// claimStringTemp marks a tracked string temp as consumed (ownership transferred
// to a variable, constructor field, or container). Clears the drop flag so the
// temp won't be freed at statement end.
func (c *Compiler) claimStringTemp(val value.Value) {
	if val == nil {
		return
	}
	idx, ok := c.stmtTempMap[val]
	if !ok || idx < 0 {
		return
	}
	// Clear drop flag — ownership transferred
	c.block.NewStore(constant.NewInt(irtypes.I1, 0), c.stmtTemps[idx].dropFlag)
	c.stmtTempMap[val] = -1
}

// cleanupStmtTemps drops all unclaimed string/vector/channel temps at statement end (T0073).
// For each temp: check flag → null-check ptr → call temp-specific drop function.
func (c *Compiler) cleanupStmtTemps() {
	// B0190: Clear per-statement flags that must not leak across statements.
	// Done before early returns so flags are always reset.
	c.optionalFieldString = false
	c.optionalFieldVector = false // T0354
	c.optionalStringDup = nil
	c.optionalContainerDup = nil // T0366
	c.optionalTupleDup = nil     // T0397
	c.optionalHeapDup = nil      // T0440
	if len(c.stmtTemps) == 0 {
		return
	}
	if c.block == nil || c.block.Term != nil {
		c.stmtTemps = c.stmtTemps[:0]
		c.stmtTempMap = make(map[value.Value]int)
		return
	}

	for _, temp := range c.stmtTemps {
		// B0219: Each temp has its own drop function (string/vector/channel).
		if temp.dropFunc == nil {
			continue
		}
		flag := c.block.NewLoad(irtypes.I1, temp.dropFlag)
		dropBlock := c.newBlock("tmp.drop")
		skipBlock := c.newBlock("tmp.skip")
		c.block.NewCondBr(flag, dropBlock, skipBlock)

		c.block = dropBlock
		ptr := c.block.NewLoad(irtypes.I8Ptr, temp.alloca)
		isNull := c.block.NewICmp(enum.IPredEQ, ptr, constant.NewNull(irtypes.I8Ptr))
		execBlock := c.newBlock("tmp.exec")
		doneBlock := c.newBlock("tmp.done")
		c.block.NewCondBr(isNull, doneBlock, execBlock)

		c.block = execBlock
		// T0356: For vector temps, drop droppable elements (strings, enums with
		// droppable variants, heap user types, droppable tuples) before freeing
		// the vector buffer. Only set on sole-owner vector temps via
		// trackVectorTempWithElemType — shallow-dup field reads use trackVectorTemp
		// (no elemType) to avoid double-freeing shared elements.
		if temp.elemType != nil {
			c.emitVectorElementDropLoop(ptr, temp.elemType)
		}
		// T0668: a discarded Task statement-expr temp (e.g. `obj.task_getter;`
		// or `compute_task();`) reaches here un-awaited. In a coroutine body
		// route through the cooperative join so the single-threaded WASM
		// scheduler can run the pending goroutine; emitTaskJoinAndFreeByDropFn
		// returns false for non-Task temps (string/vector/channel) — those
		// keep the direct drop call.
		if !c.emitTaskJoinAndFreeByDropFn(ptr, temp.dropFunc) {
			c.block.NewCall(temp.dropFunc, ptr)
		}
		c.block.NewBr(doneBlock)

		c.block = doneBlock
		// B0172: Reset drop flag after dropping. Without this, in a loop where
		// a different match arm is taken on the next iteration, the stale flag=1
		// causes a double-free on the already-freed pointer.
		c.block.NewStore(constant.NewInt(irtypes.I1, 0), temp.dropFlag)
		c.block.NewBr(skipBlock)

		c.block = skipBlock
	}

	c.stmtTemps = c.stmtTemps[:0]
	c.stmtTempMap = make(map[value.Value]int)
}

// emitStmtTempCleanupForErrorPath emits cleanup IR for statement-level
// temps without resetting the tracking state (T0103). Used on error propagation
// paths where the error branch terminates (ret/unreachable) but the ok branch
// continues and still needs cleanup at statement end. The drop flags are stored
// in allocas, so each path independently checks and clears them at runtime.
func (c *Compiler) emitStmtTempCleanupForErrorPath() {
	if len(c.stmtTemps) == 0 {
		return
	}
	if c.block == nil || c.block.Term != nil {
		return
	}

	for _, temp := range c.stmtTemps {
		// B0219: Each temp has its own drop function (string/vector/channel).
		if temp.dropFunc == nil {
			continue
		}
		flag := c.block.NewLoad(irtypes.I1, temp.dropFlag)
		dropBlock := c.newBlock("err.tmp.drop")
		skipBlock := c.newBlock("err.tmp.skip")
		c.block.NewCondBr(flag, dropBlock, skipBlock)

		c.block = dropBlock
		ptr := c.block.NewLoad(irtypes.I8Ptr, temp.alloca)
		isNull := c.block.NewICmp(enum.IPredEQ, ptr, constant.NewNull(irtypes.I8Ptr))
		execBlock := c.newBlock("err.tmp.exec")
		doneBlock := c.newBlock("err.tmp.done")
		c.block.NewCondBr(isNull, doneBlock, execBlock)

		c.block = execBlock
		// T0356: Mirror cleanupStmtTemps — drop droppable vector elements
		// before freeing the vector buffer on the error-propagation path.
		if temp.elemType != nil {
			c.emitVectorElementDropLoop(ptr, temp.elemType)
		}
		// T0668: cooperative Task join (coroutine) — see cleanupStmtTemps.
		if !c.emitTaskJoinAndFreeByDropFn(ptr, temp.dropFunc) {
			c.block.NewCall(temp.dropFunc, ptr)
		}
		c.block.NewBr(doneBlock)

		c.block = doneBlock
		c.block.NewStore(constant.NewInt(irtypes.I1, 0), temp.dropFlag)
		c.block.NewBr(skipBlock)

		c.block = skipBlock
	}
}

// emitHeapTempCleanupForErrorPath emits cleanup IR for statement-level heap
// instance temps without resetting the tracking state (T0103). Same rationale
// as emitStmtTempCleanupForErrorPath.
func (c *Compiler) emitHeapTempCleanupForErrorPath() {
	if len(c.heapTemps) == 0 {
		return
	}
	if c.block == nil || c.block.Term != nil {
		return
	}

	for _, temp := range c.heapTemps {
		flag := c.block.NewLoad(irtypes.I1, temp.dropFlag)
		dropBlock := c.newBlock("err.heap.drop")
		skipBlock := c.newBlock("err.heap.skip")
		c.block.NewCondBr(flag, dropBlock, skipBlock)

		c.block = dropBlock
		ptr := c.block.NewLoad(irtypes.I8Ptr, temp.alloca)
		isNull := c.block.NewICmp(enum.IPredEQ, ptr, constant.NewNull(irtypes.I8Ptr))
		execBlock := c.newBlock("err.heap.exec")
		doneBlock := c.newBlock("err.heap.done")
		c.block.NewCondBr(isNull, doneBlock, execBlock)

		c.block = execBlock
		// T0369: Mirror cleanupHeapTemps — walk droppable vector elements before
		// freeing the buffer on the error-propagation path.
		if temp.elemType != nil {
			c.emitVectorElementDropLoop(ptr, temp.elemType)
		}
		c.block.NewCall(temp.dropFunc, ptr)
		c.block.NewBr(doneBlock)

		c.block = doneBlock
		c.block.NewStore(constant.NewInt(irtypes.I1, 0), temp.dropFlag)
		c.block.NewBr(skipBlock)

		c.block = skipBlock
	}
}

// isHeapTempProducer returns true if expr produces a new unowned heap instance
// that must be tracked for cleanup (call results, error unwrap, auto-propagation).
// B0325: Expanded from CallExpr-only to cover ErrorPanicExpr, ErrorPropagateExpr,
// and auto-propagated expressions.
func (c *Compiler) isHeapTempProducer(expr ast.Expr) bool {
	switch expr.(type) {
	case *ast.CallExpr, *ast.ErrorPanicExpr, *ast.ErrorPropagateExpr:
		return true
	}
	return c.info.AutoPropagateExprs[expr]
}

// trackChainIntermediateReceiver tracks a method chain or field access intermediate
// receiver for cleanup at statement end (B0258, B0325). When the receiver of a
// method call or field access is itself a temporary (call result, error unwrap,
// auto-propagation), the intermediate heap-allocated value would leak without
// explicit tracking.
// receiverVal is the full value struct (for claiming existing constructor heapTemps).
// instancePtr is the extracted instance pointer (field 1 of receiverVal).
func (c *Compiler) trackChainIntermediateReceiver(memberTarget ast.Expr, receiverVal value.Value, instancePtr value.Value, named *types.Named, targetType types.Type) {
	if !c.tempTrackingEnabled || c.block == nil || c.block.Term != nil {
		return
	}
	// Only track when receiver is a temporary producer (B0325)
	if !c.isHeapTempProducer(memberTarget) {
		return
	}
	if named == nil {
		return
	}
	// Skip types already handled by other tracking systems
	if named.IsValueType() || named.IsCopy() || isPrimitiveScalar(named) || named.IsStructural() {
		return
	}
	if isContainerType(targetType) || named == types.TypString {
		return
	}
	// Bitcast typed instance pointer to i8* for heap temp tracking
	trackedPtr := instancePtr
	if trackedPtr.Type() != irtypes.I8Ptr {
		if _, isPtr := trackedPtr.Type().(*irtypes.PointerType); isPtr {
			trackedPtr = c.block.NewBitCast(trackedPtr, irtypes.I8Ptr)
		} else {
			return
		}
	}
	dropFunc := c.resolveDropFuncForTemp(named, targetType)
	if dropFunc != nil {
		// Claim any existing heapTemp for this receiver (e.g., from constructor
		// allocation tracking at T0135) to prevent double-free.
		c.claimHeapTemp(receiverVal)
		c.trackHeapTemp(trackedPtr, dropFunc)
	}
}

// trackVectorHeapTempWithElemType registers a vector heap buffer for cleanup at
// statement end with element-type info. When the vector literal is consumed as a
// transient (fn arg, for-in source, expression stmt), cleanupHeapTemps walks
// droppable elements via emitVectorElementDropLoop before freeing the buffer via
// Vector.drop. Mirrors trackVectorTempWithElemType for the heapTemp path. T0369.
//
// T0371: With genTupleLit now claiming heap-tracked field temps as the tuple
// takes ownership (string concats, heap user types, enum ctor temps), the
// element walk is safe even when the element type contains a droppable tuple —
// the buffer-walk is the unique drop site for those tuple fields. Returns true
// when the heap temp is configured to walk droppable elements at cleanup
// (elemType set). Callers gate ownership-transfer claims on this.
func (c *Compiler) trackVectorHeapTempWithElemType(rawPtr value.Value, elemType types.Type) bool {
	dropFn := c.funcs["Vector.drop"]
	if dropFn == nil {
		return false
	}
	prevLen := len(c.heapTemps)
	c.trackHeapTemp(rawPtr, dropFn)
	if len(c.heapTemps) > prevLen {
		c.heapTemps[len(c.heapTemps)-1].elemType = elemType
		return true
	}
	return false
}

// trackHeapTemp registers a heap-allocated droppable instance for cleanup at
// statement end (T0088). The instance pointer and drop function are stored so
// unclaimed temps can be dropped at statement end.
func (c *Compiler) trackHeapTemp(instancePtr value.Value, dropFunc *ir.Func) {
	c.trackHeapTempWithFlag(instancePtr, dropFunc, constant.NewInt(irtypes.I1, 1))
}

// trackHeapTempWithFlag is trackHeapTemp with a caller-supplied initial live-flag
// (an i1). Used by genElvis to register the elvis result with a per-branch flag
// (owned on the some path where the extracted inner is orphaned, not-owned on the
// none path where the default keeps its own owner). T0937.
func (c *Compiler) trackHeapTempWithFlag(instancePtr value.Value, dropFunc *ir.Func, flagVal value.Value) {
	if instancePtr == nil || dropFunc == nil || c.block == nil || c.block.Term != nil {
		return
	}
	if c.entryBlock == nil || !c.tempTrackingEnabled {
		return
	}
	if instancePtr.Type() != irtypes.I8Ptr {
		return
	}
	if _, ok := c.heapTempMap[instancePtr]; ok {
		return // already tracked
	}

	alloca := c.createEntryAlloca(irtypes.I8Ptr)
	dropFlag := c.createEntryAlloca(irtypes.I1)

	c.entryBlock.NewStore(constant.NewNull(irtypes.I8Ptr), alloca)
	c.entryBlock.NewStore(constant.NewInt(irtypes.I1, 0), dropFlag)

	c.block.NewStore(instancePtr, alloca)
	c.block.NewStore(flagVal, dropFlag)

	idx := len(c.heapTemps)
	c.heapTemps = append(c.heapTemps, heapTemp{alloca: alloca, dropFlag: dropFlag, dropFunc: dropFunc})
	c.heapTempMap[instancePtr] = idx
}

// claimHeapTemp marks a tracked heap instance as consumed (ownership transferred
// to a variable). Clears the drop flag so the temp won't be dropped at statement end.
// Accepts either an i8* instance pointer or a value struct — extracts field 1
// (the instance pointer) at the LLVM level if needed.
func (c *Compiler) claimHeapTemp(val value.Value) {
	c.lastClaimedDropFunc = nil // T0127: reset before each claim attempt
	if val == nil || len(c.heapTemps) == 0 {
		return
	}
	if c.block == nil || c.block.Term != nil {
		return
	}
	// Try direct match (i8* instance pointer)
	if idx, ok := c.heapTempMap[val]; ok && idx >= 0 {
		c.lastClaimedDropFunc = c.heapTemps[idx].dropFunc // T0127
		c.block.NewStore(constant.NewInt(irtypes.I1, 0), c.heapTemps[idx].dropFlag)
		c.heapTempMap[val] = -1
		return
	}
	// For value structs ({vtable, instance}): extract field 1 and do a runtime
	// comparison against each tracked temp. This handles method call results
	// where maybeTrackIterTemp tracked the extractvalue but the caller has the
	// full value struct (different SSA value, same runtime pointer).
	if _, ok := val.Type().(*irtypes.StructType); ok {
		var instPtr value.Value = c.block.NewExtractValue(val, 1)
		// B0218: Bitcast typed instance pointers (e.g., promise_Point_i*) to i8*
		// so we can compare against tracked temps (which are always i8*).
		if instPtr.Type() != irtypes.I8Ptr {
			if _, isPtr := instPtr.Type().(*irtypes.PointerType); isPtr {
				instPtr = c.block.NewBitCast(instPtr, irtypes.I8Ptr)
			} else if innerSt, isStruct := instPtr.Type().(*irtypes.StructType); isStruct && len(innerSt.Fields) >= 2 {
				// B0233: Handle optional wrapping: {i1, {vtable, instance}} —
				// field 1 of the optional is a value struct, extract field 1 from it.
				instPtr = c.block.NewExtractValue(instPtr, 1)
				if instPtr.Type() != irtypes.I8Ptr {
					if _, isPtr2 := instPtr.Type().(*irtypes.PointerType); isPtr2 {
						instPtr = c.block.NewBitCast(instPtr, irtypes.I8Ptr)
					} else {
						return
					}
				}
			} else {
				return
			}
		}
		for _, temp := range c.heapTemps {
			if c.lastClaimedDropFunc == nil {
				c.lastClaimedDropFunc = temp.dropFunc // T0127: capture for scope binding
			}
			tracked := c.block.NewLoad(irtypes.I8Ptr, temp.alloca)
			isSame := c.block.NewICmp(enum.IPredEQ, instPtr, tracked)
			claimBlk := c.newBlock("heap.claim")
			skipBlk := c.newBlock("heap.claim.skip")
			c.block.NewCondBr(isSame, claimBlk, skipBlk)
			claimBlk.NewStore(constant.NewInt(irtypes.I1, 0), temp.dropFlag)
			claimBlk.NewBr(skipBlk)
			c.block = skipBlk
		}
	}
}

// cleanupHeapTemps drops all unclaimed heap instance temps at statement end (T0088).
// For each temp: check flag → null-check ptr → call drop(ptr).
func (c *Compiler) cleanupHeapTemps() {
	if len(c.heapTemps) == 0 {
		return
	}
	if c.block == nil || c.block.Term != nil {
		c.heapTemps = c.heapTemps[:0]
		c.heapTempMap = make(map[value.Value]int)
		return
	}

	for _, temp := range c.heapTemps {
		flag := c.block.NewLoad(irtypes.I1, temp.dropFlag)
		dropBlock := c.newBlock("heap.drop")
		skipBlock := c.newBlock("heap.skip")
		c.block.NewCondBr(flag, dropBlock, skipBlock)

		c.block = dropBlock
		ptr := c.block.NewLoad(irtypes.I8Ptr, temp.alloca)
		isNull := c.block.NewICmp(enum.IPredEQ, ptr, constant.NewNull(irtypes.I8Ptr))
		execBlock := c.newBlock("heap.exec")
		doneBlock := c.newBlock("heap.done")
		c.block.NewCondBr(isNull, doneBlock, execBlock)

		c.block = execBlock
		// T0369: For vector heap temps, walk droppable elements (strings, enums
		// with droppable variants, heap user types, droppable tuples) before
		// freeing the buffer via Vector.drop. Mirrors the stmtTemp path's T0356
		// fix. Only set on vector literals via trackVectorHeapTempWithElemType;
		// other heap temps (slice results, ctor allocations) leave elemType nil.
		if temp.elemType != nil {
			c.emitVectorElementDropLoop(ptr, temp.elemType)
		}
		c.block.NewCall(temp.dropFunc, ptr)
		c.block.NewBr(doneBlock)

		c.block = doneBlock
		c.block.NewBr(skipBlock)

		c.block = skipBlock
	}

	c.heapTemps = c.heapTemps[:0]
	c.heapTempMap = make(map[value.Value]int)
}

// promoteHeapTempsToScope converts remaining heapTemps into scope bindings (B0222).
// When a generic combinator chain result is stored in a variable, intermediate
// iterators must survive until scope exit — not be freed at statement end. Each
// heapTemp's existing drop flag is reused: the one claimed by the variable already
// has flag=0 (its scope binding won't fire), while unclaimed intermediates have
// flag=1 and will be freed at scope exit via emitFreeCall.
//
// T0369: temp.elemType is intentionally not propagated into the scopeBinding.
// This path only fires for structural-typed variables (e.g., Iterator). Vector
// literals have concrete Vector[T] declared types and never reach this code,
// so element drops via emitVectorElementDropLoop happen exclusively on the
// cleanupHeapTemps statement-end path. If a future callsite registers a vector
// heap temp that survives to a scope binding, emitFreeCall would lose element
// drops — extend scopeBinding/emitFreeCall at that point.
func (c *Compiler) promoteHeapTempsToScope() {
	if len(c.heapTemps) == 0 {
		return
	}
	for _, temp := range c.heapTemps {
		binding := scopeBinding{
			kind:     bindingFree,
			alloca:   temp.alloca,
			dropFlag: temp.dropFlag,
			dropFunc: temp.dropFunc,
		}
		c.scopeBindings = append(c.scopeBindings, binding)
	}
	// Clear heapTemps to prevent cleanupHeapTemps from double-processing.
	c.heapTemps = c.heapTemps[:0]
	c.heapTempMap = make(map[value.Value]int)
}

// promoteHandleTempToScopeBinding promotes a tracked single-owner handle
// stmtTemp (the receiver of a borrowing method such as Mutex.lock()) into a
// scope binding so it outlives the derived guard. A single-owner Mutex *temp*
// receiver (`Mutex[int](7).lock()`, `mk_mtx().lock()`) would otherwise be
// dropped at statement end before the MutexGuard that borrows it, and
// MutexGuard.drop then unlocks/derefs freed Mutex memory → UAF/SEGV (T0655).
// Registering it as a scope binding before the guard's var-decl scope binding
// makes LIFO scope cleanup drop the guard (unlock) before the Mutex (free) —
// exactly mirroring the already-correct bound-receiver path.
//
// Returns false (no-op) when val is not a currently-tracked stmtTemp — e.g. a
// bound-variable receiver (`m := ...; m.lock()`), where mutexRaw is a fresh
// load and never a stmtTempMap key — so the must-stay-correct bound and
// consume-only cases are provably untouched.
func (c *Compiler) promoteHandleTempToScopeBinding(val value.Value, dropFunc *ir.Func, valType types.Type) bool {
	if val == nil || c.block == nil || c.block.Term != nil || c.entryBlock == nil {
		return false
	}
	idx, ok := c.stmtTempMap[val]
	if !ok || idx < 0 { // not a tracked temp → leave bound path untouched
		return false
	}
	// Coroutine-safe entry-block allocas (same primitive as trackTempWithDrop):
	// initialized to null/false in the entry block so a temp created inside a
	// branch has defined values on untaken paths.
	alloca := c.createEntryAlloca(irtypes.I8Ptr)
	dropFlag := c.createEntryAlloca(irtypes.I1)
	c.entryBlock.NewStore(constant.NewNull(irtypes.I8Ptr), alloca)
	c.entryBlock.NewStore(constant.NewInt(irtypes.I1, 0), dropFlag)
	c.block.NewStore(val, alloca)
	// T0951: preserve the temp's live per-branch ownership flag instead of
	// hardcoding 1. An ordinary handle temp (`Mutex[int](7).lock()`,
	// `mk_mtx().lock()`) carries flag=1, so this is identical to the prior
	// `store 1` for the T0655 case. But an inline elvis handle result
	// (`(a ?: b).lock()`) carries a per-path flag — owned (1) on the orphaned
	// some-path, borrowed (0) on the none-path where the default keeps its own
	// owner. Hardcoding 1 would force-drop the borrowed none-path default, which
	// its own scope binding also drops → double-free. Loaded before claimStringTemp
	// below clears the source temp's flag.
	curFlag := c.block.NewLoad(irtypes.I1, c.stmtTemps[idx].dropFlag)
	c.block.NewStore(curFlag, dropFlag)
	// bindingDropString: i8* alloca + void(i8*) drop — identical IR shape to the
	// known-good bound-Mutex scope binding (stmt.go ~2097). The Vector
	// static-flag branch in emitStringDropCall is inert for a Mutex valType.
	c.scopeBindings = append(c.scopeBindings, scopeBinding{
		kind:     bindingDropString,
		alloca:   alloca,
		dropFlag: dropFlag,
		dropFunc: dropFunc,
		valType:  valType,
	})
	// Neutralize the stmt-temp (clears its flag + maps it to -1) so it is not
	// also dropped at statement end. Keeps the T0555/T0561 binding-site claim
	// machinery intact.
	c.claimStringTemp(val)
	return true
}

// trackEnvTemp registers a heap-allocated closure env pointer for cleanup at
// statement end (T0100). Called from genLambdaExpr when the lambda has captures.
// If the lambda is later stored in a variable, claimEnvTemp prevents double-free.
func (c *Compiler) trackEnvTemp(envPtr value.Value) {
	if envPtr == nil || c.block == nil || c.block.Term != nil {
		return
	}
	if c.entryBlock == nil || !c.tempTrackingEnabled {
		return
	}
	if envPtr.Type() != irtypes.I8Ptr {
		return
	}
	if _, ok := c.envTempMap[envPtr]; ok {
		return
	}

	alloca := c.createEntryAlloca(irtypes.I8Ptr)
	dropFlag := c.createEntryAlloca(irtypes.I1)
	c.entryBlock.NewStore(constant.NewNull(irtypes.I8Ptr), alloca)
	c.entryBlock.NewStore(constant.NewInt(irtypes.I1, 0), dropFlag)

	c.block.NewStore(envPtr, alloca)
	c.block.NewStore(constant.NewInt(irtypes.I1, 1), dropFlag)

	idx := len(c.envTemps)
	c.envTemps = append(c.envTemps, envTemp{alloca: alloca, dropFlag: dropFlag})
	c.envTempMap[envPtr] = idx
}

// claimEnvTemp marks a tracked env temp as consumed (ownership transferred
// to a variable's scope binding via maybeRegisterEnvFree). Accepts either a
// raw i8* env pointer (direct SSA match) or a closure fat pointer {i8*, i8*}
// (extracts field 1 and compares at runtime).
func (c *Compiler) claimEnvTemp(val value.Value) {
	if val == nil || len(c.envTemps) == 0 {
		return
	}
	if c.block == nil || c.block.Term != nil {
		return
	}
	// Try direct SSA match (rare — usually the env ptr is embedded in a fat pointer)
	if idx, ok := c.envTempMap[val]; ok && idx >= 0 {
		c.block.NewStore(constant.NewInt(irtypes.I1, 0), c.envTemps[idx].dropFlag)
		c.envTempMap[val] = -1
		return
	}
	// For closure fat pointers {i8*, i8*}: extract env (field 1) and compare at runtime
	if st, ok := val.Type().(*irtypes.StructType); ok && len(st.Fields) == 2 {
		envPtr := c.block.NewExtractValue(val, 1)
		// T0814: Optional-wrapped closure {present, {fn,env}} — field 1 is the closure
		// fat pointer, not the bare env i8*. Recurse so the env temp is claimed
		// (otherwise cleanupEnvTemps frees it early → dangling env in the optional).
		if _, isStruct := envPtr.Type().(*irtypes.StructType); isStruct {
			c.claimEnvTemp(envPtr)
			return
		}
		if envPtr.Type() != irtypes.I8Ptr {
			return
		}
		for _, temp := range c.envTemps {
			tracked := c.block.NewLoad(irtypes.I8Ptr, temp.alloca)
			isSame := c.block.NewICmp(enum.IPredEQ, envPtr, tracked)
			claimBlk := c.newBlock("env.claim")
			skipBlk := c.newBlock("env.claim.skip")
			c.block.NewCondBr(isSame, claimBlk, skipBlk)
			claimBlk.NewStore(constant.NewInt(irtypes.I1, 0), temp.dropFlag)
			claimBlk.NewBr(skipBlk)
			c.block = skipBlk
		}
	}
}

// claimAllEnvTemps claims all active (unclaimed) env temps. Called when
// maybeTrackIterTemp registers a heap temp — the callee stored our closure env
// in the returned instance (e.g., _FnIter), so its cleanup handles the env.
func (c *Compiler) claimAllEnvTemps() {
	if c.block == nil || c.block.Term != nil {
		return
	}
	for key, idx := range c.envTempMap {
		if idx >= 0 {
			c.block.NewStore(constant.NewInt(irtypes.I1, 0), c.envTemps[idx].dropFlag)
			c.envTempMap[key] = -1
		}
	}
}

// cleanupEnvTemps frees all unclaimed closure env temps at statement end (T0100).
// For each temp: check flag → null-check ptr → call env drop fn or pal_free (B0221).
func (c *Compiler) cleanupEnvTemps() {
	if len(c.envTemps) == 0 {
		return
	}
	if c.block == nil || c.block.Term != nil {
		c.envTemps = c.envTemps[:0]
		c.envTempMap = make(map[value.Value]int)
		return
	}

	for _, temp := range c.envTemps {
		flag := c.block.NewLoad(irtypes.I1, temp.dropFlag)
		dropBlock := c.newBlock("env.tmp.drop")
		skipBlock := c.newBlock("env.tmp.skip")
		c.block.NewCondBr(flag, dropBlock, skipBlock)

		c.block = dropBlock
		ptr := c.block.NewLoad(irtypes.I8Ptr, temp.alloca)
		isNull := c.block.NewICmp(enum.IPredEQ, ptr, constant.NewNull(irtypes.I8Ptr))
		execBlock := c.newBlock("env.tmp.exec")
		doneBlock := c.newBlock("env.tmp.done")
		c.block.NewCondBr(isNull, doneBlock, execBlock)

		c.block = execBlock
		// B0221: Use emitEnvDropOrFree to properly drop captured values
		c.emitEnvDropOrFree(ptr)
		c.block.NewBr(doneBlock)

		c.block = doneBlock
		c.block.NewBr(skipBlock)

		c.block = skipBlock
	}

	c.envTemps = c.envTemps[:0]
	c.envTempMap = make(map[value.Value]int)
}

// maybeTrackIterTemp tracks the instance pointer from a method call result
// when the result type is a structural interface (T0088). At statement end,
// unclaimed temps are cleaned up. Iterator/Stream types use __promise_iter_cleanup
// (handles _FnIter parent chain + closure env). Other structural types use
// __promise_structural_drop (B0270: RTTI-based drop for arbitrary concrete types).
func (c *Compiler) maybeTrackIterTemp(e *ast.CallExpr, result value.Value) {
	if result == nil || c.block == nil || c.block.Term != nil {
		return
	}
	if !c.tempTrackingEnabled {
		return
	}
	// Check if the result type is a structural interface (e.g., Iterator[T])
	resultType := c.info.Types[e]
	if c.typeSubst != nil {
		resultType = types.Substitute(resultType, c.typeSubst)
	}
	if c.selfSubst != nil {
		resultType = types.SubstituteSelf(resultType, c.selfSubst.iface, c.selfSubst.concrete)
	}
	resultNamed := extractNamed(resultType)
	if resultNamed == nil || !resultNamed.IsStructural() {
		return
	}
	// The result is a value struct {i8* vtable, i8* instance}. Extract instance ptr.
	if _, ok := result.Type().(*irtypes.StructType); !ok {
		return
	}
	instancePtr := c.block.NewExtractValue(result, 1)
	// Iterator[T] and Stream[T] use iterCleanup (handles _FnIter parent chain).
	// Other structural types use structuralDrop (B0270: RTTI-based, works for any type).
	_, isIter := types.AsIterator(resultType)
	_, isStream := types.AsStream(resultType)
	if (isIter || isStream) && c.iterCleanup != nil {
		c.trackHeapTemp(instancePtr, c.iterCleanup)
	} else if c.structuralDrop != nil {
		c.trackHeapTemp(instancePtr, c.structuralDrop)
	}
}

// isTrackedStringCall returns true if the call expression produces a NEW
// heap-allocated string (T0073, T0099, T0123). Tracks ALL calls returning
// string type. After B0255, string.to_string() allocates via clone(),
// so there are no known borrows left to exclude.
func (c *Compiler) isTrackedStringCall(_ *ast.CallExpr) bool {
	return true
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

// trackHeapUserTypeResult tracks an expression result that is a heap-allocated
// user type (Map, Set, regular user-defined heap types) returned as a value
// struct {i8*, i8*} (T0341, generalized in T0343). Used for direct CallExpr
// results and for ErrorPanicExpr / ErrorPropagateExpr / OptionalUnwrapExpr /
// ErrorHandlerExpr results, which all peel a failable/optional struct down to
// the bare value struct.
//
// Constructor calls already track via genConstructorCallMono (T0135) — skip
// them here to avoid double-tracking. Strings, vectors, structural interfaces,
// value types, copy types, and primitives have their own tracking paths and
// are skipped.
//
// Aliasing is handled via runtime pointer comparison:
//   - claimHeapTemp(result) clears any existing heapTemp whose runtime pointer
//     matches the result (e.g., method on a temp returning `this`).
//   - For method calls, an additional runtime check against the receiver's
//     instance pointer clears the new temp's drop flag if the call result
//     aliases a non-temp receiver (e.g., `c.iter()` where `c` is a local
//     variable whose own scope binding will free the allocation). The receiver
//     is found by peeling unwrap layers via findInnerCallExpr.
func (c *Compiler) trackHeapUserTypeResult(expr ast.Expr, result value.Value) {
	if result == nil || c.block == nil || c.block.Term != nil {
		return
	}
	if !c.tempTrackingEnabled {
		return
	}
	st, ok := result.Type().(*irtypes.StructType)
	if !ok || len(st.Fields) != 2 || st.Fields[0] != irtypes.I8Ptr || st.Fields[1] != irtypes.I8Ptr {
		return
	}
	// B0287 / T0343: For optional unwrap on ident source, the optional's drop
	// binding owns the inner allocation. Tracking would double-free at scope exit.
	// isIdentOptionalUnwrapSource peels ParenExpr so `(o)!` skips like `o!`.
	if opt, isOpt := expr.(*ast.OptionalUnwrapExpr); isOpt {
		if isIdentOptionalUnwrapSource(opt.Expr) {
			return
		}
		// T0775: member source on an owner-with-drop (`owner.field!`) used as a
		// temporary. The extracted inner aliases the owned field; the owner's drop
		// frees it. Skip temp-tracking (mirrors the ident skip) — tracking would
		// double-free at scope exit. EXCLUDE borrowed-`this`: genOptionalForceUnwrap
		// (T0428 Case 3B) makes an INDEPENDENT dup there, which DOES need tracking.
		if c.isOwnerGovernedMemberOptionalUnwrapSource(opt.Expr) &&
			!c.isBorrowedThisMemberSource(opt.Expr) {
			return
		}
	}
	// T0753: Same for the optional-handler unwrap (`o? _ { ... }`) on an ident
	// source. The handler extracts the inner value as an aliasing extractvalue;
	// the source optional's own drop binding governs the inner allocation's
	// lifetime (frees it for an owned optional; deliberately does NOT free it for
	// a borrow-holding optional once isRttiCastBorrow clears o's drop flag).
	// Tracking the extracted heap value as an owned temp double-frees.
	if eh, isEH := expr.(*ast.ErrorHandlerExpr); isEH {
		if isIdentOptionalUnwrapSource(eh.Expr) {
			return
		}
	}
	// Skip constructor calls — those are tracked inside genConstructorCallMono.
	// Only applies when the outermost expression is itself a CallExpr; the
	// unwrap/propagate operators can't legally have a constructor as their
	// inner expression (constructors don't return failable/optional types).
	if call, isCall := expr.(*ast.CallExpr); isCall {
		calleeType := c.info.Types[call.Callee]
		if c.typeSubst != nil && calleeType != nil {
			calleeType = types.Substitute(calleeType, c.typeSubst)
		}
		switch calleeType.(type) {
		case *types.Named, *types.Instance:
			return
		}
	}
	rt := c.info.Types[expr]
	if c.typeSubst != nil && rt != nil {
		rt = types.Substitute(rt, c.typeSubst)
	}
	if c.selfSubst != nil && rt != nil {
		rt = types.SubstituteSelf(rt, c.selfSubst.iface, c.selfSubst.concrete)
	}
	named := extractNamed(rt)
	if named == nil {
		return
	}
	if named.IsValueType() || named.IsCopy() || isPrimitiveScalar(named) || named.IsStructural() {
		return
	}
	if isContainerType(rt) || named == types.TypString {
		return
	}
	dropFunc := c.resolveDropFuncForTemp(named, rt)
	if dropFunc == nil {
		return
	}
	c.claimHeapTemp(result)
	instancePtr := c.block.NewExtractValue(result, 1)
	beforeLen := len(c.heapTemps)
	c.trackHeapTemp(instancePtr, dropFunc)
	if len(c.heapTemps) > beforeLen {
		if innerCall := findInnerCallExpr(expr); innerCall != nil {
			c.emitReceiverAliasCheck(innerCall, instancePtr, c.heapTemps[beforeLen].dropFlag)
		}
	}
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

	// T0582: peel ParenExpr so `(w).self()` and `((w)).self()` resolve to
	// the underlying IdentExpr/ThisExpr receiver, otherwise the switch
	// would fall through to `default: return` → no alias check → double-free
	// on discard statements like `(w).self();`.
	target := unwrapDestructureParens(mem.Target)

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

// hasStructuralParam returns true if any parameter of sig is a structural interface (T0092).
func hasStructuralParam(sig *types.Signature, typeSubst map[*types.TypeParam]types.Type) bool {
	for _, param := range sig.Params() {
		pt := param.Type()
		if typeSubst != nil {
			pt = types.Substitute(pt, typeSubst)
		}
		if named := extractNamed(pt); named != nil && named.IsStructural() {
			return true
		}
	}
	return false
}

// maybeRegisterEnvFree registers a scope binding to free the closure's env struct
// at scope exit. Only applies to variables whose type is *types.Signature (function values).
func (c *Compiler) maybeRegisterEnvFree(varName string, alloca *ir.InstAlloca, typ types.Type, valueExpr ast.Expr) {
	if _, ok := typ.(*types.Signature); !ok {
		return
	}
	// T0812: reading a closure out of an owning aggregate (struct/optional field,
	// container element) aliases the aggregate's heap env — it does not transfer
	// ownership. Registering an owning env-free binding here would double-free the
	// env at scope exit against the aggregate's own drop. The aggregate retains
	// ownership; the local borrows.
	if c.isClosureAggregateBorrow(valueExpr) {
		return
	}
	dropFlag := c.createEntryAlloca(irtypes.I1)
	dropFlag.SetName(c.uniqueLocalName(varName + ".dropflag"))
	c.block.NewStore(constant.NewInt(irtypes.I1, 1), dropFlag)
	c.dropFlags[varName] = dropFlag

	c.scopeBindings = append(c.scopeBindings, scopeBinding{
		kind:     bindingFreeEnv,
		alloca:   alloca,
		dropFlag: dropFlag,
		varName:  varName,
	})
}

// --- Assignment ---

func (c *Compiler) genAssignStmt(s *ast.AssignStmt) {
	// For compound index assignments, defer RHS evaluation to ensure correct
	// evaluation order: target → key → RHS (not RHS → target → key).
	if s.Op != ast.OpAssign {
		if idx, ok := s.Target.(*ast.IndexExpr); ok {
			c.genCompoundIndexAssign(idx, s.Op, s.Value)
			return
		}
	}

	// Set targetType for Optional member/variable assignments so NoneLit
	// produces the correct zero value (B0030)
	if s.Op == ast.OpAssign {
		targetType := c.info.Types[s.Target]
		if c.typeSubst != nil {
			targetType = types.Substitute(targetType, c.typeSubst)
		}
		if _, isOpt := targetType.(*types.Optional); isOpt {
			c.targetType = targetType
		}
	}
	// T0398/T0410/T0412: When the RHS is a direct vector-index expression and the
	// LHS is droppable, set the dup-on-read flag so genVectorIndex deep-clones
	// the element. Combined with drop-on-overwrite (genVectorIndexAssign for
	// IndexExpr LHS, genMemberAssign for MemberExpr LHS, the IdentExpr branch
	// below for IdentExpr LHS), this preserves the no-alias invariant — vec-to-
	// local writes produce independent clones instead of aliasing the bucket.
	// Direct IndexExpr RHS only — chains like `b = v[0].method()` are excluded
	// to avoid orphan clones (method takes a borrow, clone would leak).
	// T0491: also fire for `OptionalUnwrapExpr` wrapping `IndexExpr` (e.g.
	// `b = m[k]!`) — same dup-on-read need as the var-decl path (T0440).
	if s.Op == ast.OpAssign {
		isIdxRhs := false
		// T0754: peel ParenExpr / CastExpr so `field = v[i] as! T` reaches the
		// same dup-on-read path as `field = v[i]`. Ownership now moves the
		// cast subject at owning-slot stores, but the IndexExpr subject still
		// returns an alias of the container slot — without the dup, the cast
		// result would alias the source vector's element and double-free.
		probe := s.Value
		for {
			p, ok := probe.(*ast.ParenExpr)
			if !ok {
				break
			}
			probe = p.Expr
		}
		// T0800: peel *chained* casts (`(v[i] as! A) as! B`). Each layer is a
		// non-consuming view, so loop until the innermost subject is reached —
		// otherwise the dup-on-read below would not fire for an IndexExpr/
		// MemberExpr subject behind two casts and the stored value would alias
		// the source slot → double-free. Mirrors the recursion in
		// isRttiCastBorrow / castSubjectMovableIdent / tryMoveConsumeCastSubject.
		for {
			cast, ok := probe.(*ast.CastExpr)
			if !ok {
				break
			}
			probe = cast.Expr
			for {
				p, ok := probe.(*ast.ParenExpr)
				if !ok {
					break
				}
				probe = p.Expr
			}
		}
		// T0802: also recognize a field-access RHS (`x = obj.field` and the
		// unwrapped `x = obj.field!`) so the heap-string / Optional[string] field
		// is cloned on read — see the memberRhs branch below.
		var memberRhs *ast.MemberExpr
		if _, ok := probe.(*ast.IndexExpr); ok {
			isIdxRhs = true
		} else if m, ok := probe.(*ast.MemberExpr); ok {
			memberRhs = m
		} else if unwrap, ok := probe.(*ast.OptionalUnwrapExpr); ok {
			if _, ok := unwrap.Expr.(*ast.IndexExpr); ok {
				isIdxRhs = true
			} else if m, ok := unwrap.Expr.(*ast.MemberExpr); ok {
				memberRhs = m
			}
		}
		if isIdxRhs {
			var lhsType types.Type
			// T0590: For the string/container dup branches below, we further gate
			// on whether the LHS *target container* is a fixed-size array. Plain
			// Vector slot-to-slot reads alias on read by design (T0490) — the
			// destructive Vector.drop at the slice-assign call site (stmt.go:5648)
			// relies on that aliasing to balance ownership. Setting the dup flag
			// for Vector LHS would force the [:]= body to dup, leaving src's
			// inner buffers leaked. Fixed-size arrays have no such call-site
			// destructive drop; the dup is the only way to avoid the slot-to-slot
			// double-free. Heap-user / tuple flags already exist with this
			// asymmetry: the existing skipB0313 list at line 5648 covers them.
			lhsIsFixedArrayElem := false
			switch t := s.Target.(type) {
			case *ast.IndexExpr:
				lhsType = c.info.Types[t]
				targetType := c.info.Types[t.Target]
				if c.typeSubst != nil && targetType != nil {
					targetType = types.Substitute(targetType, c.typeSubst)
				}
				if ref, ok := targetType.(*types.SharedRef); ok {
					targetType = ref.Elem()
				}
				if ref, ok := targetType.(*types.MutRef); ok {
					targetType = ref.Elem()
				}
				if _, isArr := targetType.(*types.Array); isArr {
					lhsIsFixedArrayElem = true
				}
			case *ast.IdentExpr:
				lhsType = c.info.Types[t]
			case *ast.MemberExpr:
				lhsType = c.info.Types[t]
			}
			if lhsType != nil {
				if c.typeSubst != nil {
					lhsType = types.Substitute(lhsType, c.typeSubst)
				}
				if isDroppableHeapUserType(lhsType) || isHeapUserNoDropPalFree(lhsType) {
					c.dupHeapUserFieldAccess = true
				}
				// T0412/T0489: same dup-on-read for droppable tuple LHS. Combined
				// with the drop-old branches in genMemberAssign / genVectorIndexAssign /
				// IdentExpr's bindingDropTuple, preserves the no-alias invariant for
				// every LHS shape — `obj.tup = vec[i]`, `t = vec[i]`, and
				// `outer[0] = vec[i]` all produce independent clones in the new slot
				// instead of aliasing the source. Direct IndexExpr/OptionalUnwrap RHS
				// only (gate inherited from the surrounding isIdxRhs check) — same
				// orphan-clone safety reasoning as the heap-user-type branch above.
				if _, isTup := lhsType.(*types.Tuple); isTup && c.tupleNeedsDrop(lhsType) {
					c.dupTupleFieldAccess = true
				}
				// T0590: string and container LHS — required for fixed-size array
				// slot-to-slot copies (`arr[1] = arr[0]`). Without these, the bare
				// `genArrayIndex` returns an alias of the source slot's pointer; the
				// store + drop-on-overwrite then leaves both slots aliasing one
				// allocation → scope-exit double-free. Gated to fixed-size array
				// LHS only because Vector slot-to-slot (inside [:]=) is intentionally
				// aliased; the destructive drop at the slice-assign call site
				// balances ownership for plain container element types.
				if lhsIsFixedArrayElem {
					if extractNamed(lhsType) == types.TypString && !isRefType(lhsType) {
						c.dupStringFieldAccess = true
					}
					if (types.IsVector(lhsType) || types.IsChannel(lhsType) || types.IsArc(lhsType) || types.IsWeak(lhsType)) && !isRefType(lhsType) {
						c.dupContainerFieldAccess = true
					}
					if opt, ok := lhsType.(*types.Optional); ok {
						inner := opt.Elem()
						if c.typeSubst != nil {
							inner = types.Substitute(inner, c.typeSubst)
						}
						if extractNamed(inner) == types.TypString {
							c.dupStringFieldAccess = true
						}
						if types.IsVector(inner) || types.IsChannel(inner) || types.IsArc(inner) || types.IsWeak(inner) {
							c.dupContainerFieldAccess = true
						}
					}
				}
				if opt, ok := lhsType.(*types.Optional); ok {
					inner := opt.Elem()
					if c.typeSubst != nil {
						inner = types.Substitute(inner, c.typeSubst)
					}
					if _, isTup := inner.(*types.Tuple); isTup && c.tupleNeedsDrop(inner) {
						c.dupTupleFieldAccess = true
					}
					if isDroppableHeapUserType(inner) || isHeapUserNoDropPalFree(inner) {
						c.dupHeapUserFieldAccess = true
					}
				}
			}
		} else if memberRhs != nil {
			// T0802: `x = obj.field` (and the unwrapped `x = obj.field!`)
			// reassignment of a heap string / Optional[string] (or container)
			// field must clone-on-read, exactly like the var-decl path
			// (genDeclStmt) already does. Without this the field's heap pointer is
			// stored into x as a raw alias with x's drop flag set, so both x and the
			// field's owner drop the same allocation → double-free (latent on linux,
			// SIGABRT on macOS). Scoped to MemberExpr RHS only so the intentional
			// Vector slot-to-slot aliasing in the isIdxRhs branch (T0490/T0590) is
			// untouched. Heap-user / tuple field moves are deliberately excluded —
			// the ownership checker already rejects those, and an ungated clone here
			// risks orphan-clone leaks (same gating reason as the decl path).
			//
			// s.Value's resolved type is the field type for a bare `obj.field` and
			// the unwrapped inner type for `obj.field!`; setDupFlagsForFieldAccess
			// covers both (string and Optional[string] map to dupStringFieldAccess,
			// which genFieldAccess honors for the inner Optional[string] field too).
			rhsType := c.info.Types[s.Value]
			if c.typeSubst != nil && rhsType != nil {
				rhsType = types.Substitute(rhsType, c.typeSubst)
			}
			c.setDupFlagsForFieldAccess(rhsType)
		}
	}
	val := c.genExpr(s.Value)
	c.targetType = nil
	c.dupHeapUserFieldAccess = false
	c.dupTupleFieldAccess = false
	c.dupStringFieldAccess = false
	c.dupContainerFieldAccess = false

	// T0802: When the RHS is an Optional[string]/Optional[container] field read
	// (`opt = obj.field`), genFieldAccess clones the inner value and tracks it via
	// optionalStringDup/optionalContainerDup. The clone is stored into the LHS, so
	// claim it here to suppress the leftover stmt-temp drop — otherwise the clone is
	// freed while the LHS still references it → double-free. Mirrors genDeclStmt
	// (the var-decl path) which performs the same claim before optional wrapping.
	if c.optionalStringDup != nil {
		c.claimStringTemp(c.optionalStringDup)
		c.optionalStringDup = nil
	}
	if c.optionalContainerDup != nil {
		c.claimStringTemp(c.optionalContainerDup)
		c.optionalContainerDup = nil
	}

	// Auto-propagate failable call in assignment RHS.
	if c.info.AutoPropagateExprs[s.Value] {
		val = c.genAutoPropagateValue(val)
	}

	switch target := s.Target.(type) {
	case *ast.IdentExpr:
		// Same-file setter: property = value (or property += value)
		if setterFn, ok := c.funcs[target.Name+"$set"]; ok {
			if obj := c.lookupFunc(target.Name + "$set"); obj != nil && obj.IsSetter() {
				if s.Op != ast.OpAssign {
					getterFn, ok := c.funcs[target.Name]
					if !ok {
						panic(fmt.Sprintf("codegen: compound assignment to setter %s but no getter found", target.Name))
					}
					var current value.Value = c.block.NewCall(getterFn)
					current = c.unwrapFailableCompoundRead(current, c.info.Types[target]) // T0709
					val = c.genCompoundOp(s.Op, c.info.Types[target], current, val)
				}
				setterCall := c.block.NewCall(setterFn, val)
				c.propagateIfFailable(setterCall) // T0708
				if s.Op == ast.OpAssign {
					if rhsIdent, ok := s.Value.(*ast.IdentExpr); ok {
						c.clearDropFlag(rhsIdent.Name)
					}
					// B0312: When RHS is opt!, neutralize the source optional so its
					// drop doesn't double-free the inner value now owned by the setter.
					c.neutralizeForceUnwrapSource(s.Value)
				}
				return
			}
		}
		// MutRef param: store through the caller's pointer (B0149)
		if ptr, ok := c.mutRefPtrs[target.Name]; ok {
			if s.Op == ast.OpAssign {
				c.block.NewStore(val, ptr)
				if ident, ok := s.Value.(*ast.IdentExpr); ok {
					c.clearDropFlag(ident.Name)
				}
				// B0312: When RHS is opt!, neutralize the source optional so its
				// drop doesn't double-free the inner value now owned by the MutRef target.
				c.neutralizeForceUnwrapSource(s.Value)
				return
			}
			// Compound assignment on MutRef param
			current := c.block.NewLoad(c.mutRefTypes[target.Name], ptr)
			result := c.genCompoundOp(s.Op, c.info.Types[target], current, val)
			c.block.NewStore(result, ptr)
			return
		}
		alloca, ok := c.locals[target.Name]
		if !ok {
			panic(fmt.Sprintf("codegen: undefined variable %q in assignment", target.Name))
		}
		if s.Op == ast.OpAssign {
			// T0892: an operator/method RHS whose body is `return this` returns the
			// borrowed receiver as an owned result, aliasing the operand. Resolve the
			// receiver-origin the same way the two var-decl paths do (genTypedVarDecl /
			// genInferredVarDecl), so the assignment path gets the same
			// B0250/T0341/T0882 alias-clear it currently lacks. selfAliasOrigin marks
			// the case where the operand IS the target (`m = m + b`), which needs the
			// guarded drop-old below instead of an operand-clear.
			var aliasOrigin ast.Expr
			if call, ok := s.Value.(*ast.CallExpr); ok {
				aliasOrigin = chainOriginExpr(call)
			} else {
				aliasOrigin = operatorReceiverOrigin(s.Value)
			}
			selfAliasOrigin := false
			if id, ok := aliasOrigin.(*ast.IdentExpr); ok && id.Name == target.Name {
				selfAliasOrigin = true
			}
			// T0911: closure self-assignment (`f = f`) is a no-op — the local keeps
			// owning its env. Return early (mirroring the dropBindings self-assign
			// guard below) so the post-store clearDropFlag doesn't zero the env drop
			// flag, which would leak the env at scope exit. Non-self-assign env
			// drop is handled by the T0911/T0913 dropFlags block below.
			if ident, ok := s.Value.(*ast.IdentExpr); ok && ident.Name == target.Name {
				tt := c.info.Types[target]
				if c.typeSubst != nil {
					tt = types.Substitute(tt, c.typeSubst)
				}
				if _, isSig := tt.(*types.Signature); isSig {
					return
				}
			}
			// Drop old value before reassignment (if target is droppable)
			if binding, ok := c.dropBindings[target.Name]; ok {
				// Skip self-assignment (would drop then store dangling pointer)
				if ident, ok := s.Value.(*ast.IdentExpr); ok && ident.Name == target.Name {
					return
				}
				// For string/vector types (i8* pointers), the new value might alias the
				// old (e.g., v = sort(v) returns the same pointer). Compare old/new at
				// runtime and only drop if they differ (T0068).
				if binding.kind == bindingDropString {
					oldVal := c.block.NewLoad(binding.alloca.ElemType, binding.alloca)
					diffBlk := c.newBlock("reassign.diff")
					mergeBlk := c.newBlock("reassign.merge")
					isSame := c.block.NewICmp(enum.IPredEQ, oldVal, val)
					c.block.NewCondBr(isSame, mergeBlk, diffBlk)
					c.block = diffBlk
					c.emitStringDropCall(binding)
					if c.block.Term == nil {
						c.block.NewBr(mergeBlk)
					}
					c.block = mergeBlk
				} else if binding.kind == bindingDropEnum {
					c.emitEnumDropCall(binding)
				} else if binding.kind == bindingDropTuple {
					c.emitTupleDropCall(binding)
				} else if binding.kind == bindingDropArray {
					c.emitArrayDropCall(binding)
				} else if selfAliasOrigin &&
					(binding.kind == bindingFree || binding.kind == bindingDrop) &&
					isUserValueStructType(binding.alloca.ElemType) &&
					isUserValueStructType(val.Type()) {
					// T0892: `m = m + b` / `m = m.dup()` where the operator/method
					// returns `this`. The RHS is evaluated before this drop, so val
					// already holds the (possibly aliasing) instance pointer. Skip the
					// drop when it aliases the old value — otherwise we free the very
					// instance val points to (UAF/double-free). Mirrors the runtime
					// old-vs-new alias check the bindingDropString branch above performs.
					// Covers both heap-user kinds: bindingFree (no drop method →
					// pal_free) and bindingDrop (has a drop method).
					oldVal := c.block.NewLoad(binding.alloca.ElemType, binding.alloca)
					oldInst := c.block.NewExtractValue(oldVal, 1)
					newInst := c.block.NewExtractValue(val, 1)
					diffBlk := c.newBlock("reassign.self.diff")
					mergeBlk := c.newBlock("reassign.self.merge")
					isSame := c.block.NewICmp(enum.IPredEQ, oldInst, newInst)
					c.block.NewCondBr(isSame, mergeBlk, diffBlk)
					c.block = diffBlk
					if binding.kind == bindingFree {
						c.emitFreeCall(binding)
					} else {
						c.emitDropCall(binding)
					}
					if c.block.Term == nil {
						c.block.NewBr(mergeBlk)
					}
					c.block = mergeBlk
				} else if binding.kind == bindingFree {
					c.emitFreeCall(binding)
				} else {
					c.emitDropCall(binding)
				}
				// Reset drop flag: new value is now owned
				c.block.NewStore(constant.NewInt(irtypes.I1, 1), binding.dropFlag)
			}

			// T0911: Closures aren't in dropBindings — their env cleanup is a
			// bindingFreeEnv in scopeBindings with a flag in c.dropFlags (see
			// maybeRegisterEnvFree). The drop-old logic above never frees the old
			// env, so reassigning a closure local that owns a heap env leaks it.
			// Free the old env here (guarded by the flag + an old-vs-new
			// env-pointer alias check, since `f = <expr aliasing f's env>` could
			// occur) and re-arm the flag so the new value is owned. The later
			// T0895 borrow-clear (below) and the move-RHS clearDropFlag then
			// adjust the flag for borrow/move RHS as appropriate.
			if envFlag, hasEnvFlag := c.dropFlags[target.Name]; hasEnvFlag {
				tt := c.info.Types[target]
				if c.typeSubst != nil {
					tt = types.Substitute(tt, c.typeSubst)
				}
				if _, isSig := tt.(*types.Signature); isSig {
					oldClosure := c.block.NewLoad(alloca.ElemType, alloca)
					oldEnv := c.block.NewExtractValue(oldClosure, 1) // field 1 = env ptr
					newEnv := c.block.NewExtractValue(val, 1)
					flag := c.block.NewLoad(irtypes.I1, envFlag)
					isSame := c.block.NewICmp(enum.IPredEQ, oldEnv, newEnv)
					notSame := c.block.NewXor(isSame, constant.NewInt(irtypes.I1, 1))
					doFree := c.block.NewAnd(flag, notSame)
					freeBlk := c.newBlock("reassign.env.free")
					mergeBlk := c.newBlock("reassign.env.merge")
					c.block.NewCondBr(doFree, freeBlk, mergeBlk)

					c.block = freeBlk
					isNull := c.block.NewICmp(enum.IPredEQ, oldEnv, constant.NewNull(irtypes.I8Ptr))
					callBlk := c.newBlock("reassign.env.call")
					c.block.NewCondBr(isNull, mergeBlk, callBlk)

					c.block = callBlk
					c.emitEnvDropOrFree(oldEnv) // drops captured values + frees env struct
					c.block.NewBr(mergeBlk)

					c.block = mergeBlk
					// New value is now owned by the local.
					c.block.NewStore(constant.NewInt(irtypes.I1, 1), envFlag)
				}
			}

			// Coerce value struct vtable when crossing type boundaries
			exprType := c.info.Types[s.Value]
			targetType := c.info.Types[target]
			if c.typeSubst != nil {
				exprType = types.Substitute(exprType, c.typeSubst)
				targetType = types.Substitute(targetType, c.typeSubst)
			}
			val = c.coerceToView(val, exprType, targetType)

			// T0111: Claim string temp BEFORE optional wrapping — after wrap,
			// value identity changes and claimStringTemp can't match the temp.
			// T0555: Same for native handle / container temps so the stmt-temp
			// drop doesn't double-free with the optional's binding drop.
			if _, isOpt := targetType.(*types.Optional); isOpt {
				if exprType != nil {
					if extractNamed(exprType) == types.TypString ||
						types.IsVector(exprType) || types.IsChannel(exprType) ||
						types.IsArc(exprType) || types.IsWeak(exprType) ||
						types.IsMutex(exprType) || types.IsTask(exprType) ||
						types.IsMutexGuard(exprType) {
						c.claimStringTemp(val)
					}
				}
			}

			// Wrap value in Optional if target is Optional and expr differs in shape.
			// Using Identical (not "is exprOpt?") correctly handles T?? = T? — both
			// are Optional but at different depths, so a wrap is still needed.
			if _, isOpt := targetType.(*types.Optional); isOpt {
				if exprType == types.TypNone {
					// none: already handled by genExpr (zeroinit)
				} else if !types.Identical(exprType, targetType) {
					val = c.wrapOptional(val, alloca.ElemType.(*irtypes.StructType))
				}
			}

			c.block.NewStore(val, alloca)
			// Clear drop flag on RHS if it's being moved
			if ident, ok := s.Value.(*ast.IdentExpr); ok {
				c.clearDropFlag(ident.Name)
			}
			// T0892: clear the operand/receiver drop flag when the operator/method
			// result aliases it (user operator/method whose body is `return this`).
			// Mirrors the two var-decl paths. Skip structural targets — the view
			// borrows the original, which must keep its drop flag (T0082); and skip
			// the self-alias case (handled by the guarded drop-old above — clearing
			// here would compare post-store and wrongly clear the target's own flag,
			// leaking the instance).
			if binding, ok := c.dropBindings[target.Name]; ok && !selfAliasOrigin {
				structuralTarget := false
				if n := extractNamed(targetType); n != nil && n.IsStructural() {
					structuralTarget = true
				}
				if !structuralTarget {
					switch origin := aliasOrigin.(type) {
					case *ast.IdentExpr:
						c.maybeClearReceiverDropFlag(val, origin.Name, exprType)
					case *ast.ThisExpr:
						c.maybeClearBindingDropFlagOnThisAlias(val, binding.dropFlag, exprType)
					}
				}
			}
			// T0379/T0381: when RHS static type is `T&`/`T~`, override the
			// unconditional re-arm above. The borrow returns a non-owning
			// reference; the owner retains the value. Without this, both
			// the reassigned local's drop and the owner's drop free the same
			// inner value.
			if c.isBorrowedExpr(s.Value) {
				c.clearDropFlag(target.Name)
			}
			// T0747: a user-type RTTI cast of a borrow (`target = x as!/as T`) is a
			// non-consuming view — the subject keeps ownership. Clear the target's
			// drop flag (re-armed to 1 above when the old value was dropped) so the
			// reassigned local doesn't double-free the aliased instance at scope
			// exit. Mirrors the same clear in genTypedVarDecl/genInferredVarDecl.
			if c.isRttiCastBorrow(s.Value) {
				c.clearDropFlag(target.Name)
			}
			// T0895: `f = h.cb` reads a closure out of an owning aggregate — the
			// local borrows the heap env (the aggregate retains sole ownership;
			// closures aren't Cloneable, so the fat pointer {fn,env} is copied by
			// value with no env dup). Clear f's env-free drop flag (still 1 from
			// the var-decl's maybeRegisterEnvFree — the drop-old re-arm above only
			// runs for dropBindings entries, whereas closures register a
			// bindingFreeEnv in scopeBindings) so scope-exit env-free doesn't
			// double-free against the aggregate's drop. Mirrors
			// maybeRegisterEnvFree's var-decl suppression; gated on a Signature
			// target since isClosureAggregateBorrow alone also matches
			// string/vector field reads.
			if _, isSig := targetType.(*types.Signature); isSig && c.isClosureAggregateBorrow(s.Value) {
				c.clearDropFlag(target.Name)
			}
			// T0073: Claim string temp — ownership transferred to this variable.
			// Skip if already claimed above (optional target).
			if exprType != nil && extractNamed(exprType) == types.TypString {
				c.claimStringTemp(val)
			}
			// T0109: Claim vector/channel/arc/weak temp — ownership transferred to this variable.
			// T0555: Mutex/Task also need claiming now that their constructor temps are tracked.
			// T0561: MutexGuard temps from m.lock() also need claiming.
			if exprType != nil && (types.IsVector(exprType) || types.IsChannel(exprType) ||
				types.IsArc(exprType) || types.IsWeak(exprType) ||
				types.IsMutex(exprType) || types.IsTask(exprType) ||
				types.IsMutexGuard(exprType)) {
				c.claimStringTemp(val)
			}
			// B0187: Claim heap temp — ownership transferred to reassigned variable.
			// Without this, structural interface reassignment (e.g., iter = c.map(...))
			// leaves the heap temp unclaimed, causing double-free at statement end + scope exit.
			c.claimHeapTemp(val)
			// Claim env temp — ownership transferred to reassigned variable.
			c.claimEnvTemp(val)
			// B0293: Clear enum ctor temps — ownership transferred to reassigned variable.
			// Without this, the ctor temp drop fires at statement end and double-frees
			// variant data that the variable now owns (segfault on scope exit).
			if len(c.enumCtorTemps) > 0 && extractEnum(exprType) != nil {
				for i := range c.enumCtorTemps {
					c.block.NewStore(constant.NewInt(irtypes.I1, 0), c.enumCtorTemps[i].dropFlag)
				}
				c.enumCtorTemps = c.enumCtorTemps[:0]
			}
			// B0312: When RHS is opt!, neutralize the source optional so its
			// drop doesn't double-free the inner value now owned by this variable.
			c.neutralizeForceUnwrapSource(s.Value)
			return
		}
		// Compound assignment: load current value, apply operator, store result
		targetType := c.info.Types[target]
		current := c.block.NewLoad(alloca.ElemType, alloca)
		result := c.genCompoundOp(s.Op, targetType, current, val)
		// T0357: For string locals with a drop binding, drop the old value
		// before storing the new one. promise_string_concat always allocates,
		// so the result never aliases the old pointer; runtime alias check
		// mirrors the OpAssign branch above for consistency.
		if binding, ok := c.dropBindings[target.Name]; ok && binding.kind == bindingDropString {
			diffBlk := c.newBlock("compound.diff")
			mergeBlk := c.newBlock("compound.merge")
			isSame := c.block.NewICmp(enum.IPredEQ, current, result)
			c.block.NewCondBr(isSame, mergeBlk, diffBlk)
			c.block = diffBlk
			c.emitStringDropCall(binding)
			if c.block.Term == nil {
				c.block.NewBr(mergeBlk)
			}
			c.block = mergeBlk
			// New value is now owned by the local — drop flag stays at 1.
			c.block.NewStore(constant.NewInt(irtypes.I1, 1), binding.dropFlag)
		}
		c.block.NewStore(result, alloca)

	case *ast.MemberExpr:
		// Module-level setter: mod.property = value
		if ident, ok := target.Target.(*ast.IdentExpr); ok {
			if modName := c.resolveModuleName(ident); modName != "" {
				c.genModuleSetterAssign(target, modName, s.Op, val)
				if s.Op == ast.OpAssign {
					if rhsIdent, ok := s.Value.(*ast.IdentExpr); ok {
						c.clearDropFlag(rhsIdent.Name)
					}
					// B0312: When RHS is opt!, neutralize the source optional so its
					// drop doesn't double-free the inner value now owned by this setter.
					c.neutralizeForceUnwrapSource(s.Value)
				}
				break
			}
		}
		// Wrap value in Optional if field type is Optional but expr is not
		if s.Op == ast.OpAssign {
			memberType := c.info.Types[target]
			exprType := c.info.Types[s.Value]
			if c.typeSubst != nil {
				memberType = types.Substitute(memberType, c.typeSubst)
				exprType = types.Substitute(exprType, c.typeSubst)
			}
			if _, isOpt := memberType.(*types.Optional); isOpt {
				if exprType != types.TypNone {
					// T0394: Claim heap temps from stmtTemps BEFORE wrapping in
					// Optional. claimStringTemp uses direct val-identity lookup,
					// so the post-wrap claim below fails (val identity changes
					// after wrapOptional). Mirrors T0111 fix in IdentExpr branch
					// (~line 4811) and genVarDecl (~line 727). claimHeapTemp
					// post-wrap still handles heapTemp-tracked vector literals
					// via runtime extractvalue.
					if extractNamed(exprType) == types.TypString ||
						types.IsVector(exprType) || types.IsChannel(exprType) ||
						types.IsArc(exprType) || types.IsWeak(exprType) ||
						types.IsTask(exprType) || types.IsMutex(exprType) ||
						types.IsMutexGuard(exprType) {
						// T0560: Task RHS in `field = go ...` where the field is
						// Optional[Task[T]]. Without claiming the temp BEFORE
						// wrapping into the Optional struct, the stmtTemp cleanup
						// runs at statement end and drops G — but G is now owned
						// by the optional field, causing a double-free at scope
						// exit via the Optional field-drop path.
						// T0573: Mutex/MutexGuard added — their constructors track
						// stmtTemps too, so without claiming before wrapping the
						// optional field path's drop double-frees with the temp
						// cleanup.
						c.claimStringTemp(val)
					}
					// Use Identical (not "is exprOpt?") so T?? = T? still wraps.
					if !types.Identical(exprType, memberType) {
						optType := c.resolveType(memberType)
						if st, ok := optType.(*irtypes.StructType); ok {
							val = c.wrapOptional(val, st)
						}
					}
				}
			}
		}
		// T0095: Dup string values stored in fields of droppable types when the
		// source is a borrowed variable (no drop flag). This handles custom new()
		// methods like `this.src = s` where s is a non-~ parameter.
		if s.Op == ast.OpAssign {
			memberType := c.info.Types[target]
			if c.typeSubst != nil {
				memberType = types.Substitute(memberType, c.typeSubst)
			}
			ownerType := c.info.Types[target.Target]
			if c.typeSubst != nil {
				ownerType = types.Substitute(ownerType, c.typeSubst)
			}
			ownerNamed := extractNamed(ownerType)
			if extractNamed(memberType) == types.TypString && ownerNamed != nil && ownerNamed.HasDrop() {
				if ident, ok := s.Value.(*ast.IdentExpr); ok {
					if _, hasFlag := c.dropFlags[ident.Name]; hasFlag {
						// Has drop flag: move ownership
						c.genMemberAssign(target, s.Op, val, s.Value)
						c.clearDropFlag(ident.Name)
					} else {
						// No drop flag: dup for exclusive ownership.
						// Pass nil srcExpr so genMutexGuardBorrowSet's defensive
						// dup doesn't fire — already duped here.
						c.genMemberAssign(target, s.Op, c.dupString(val), nil)
					}
				} else {
					// Expression result: store directly, claim temp
					c.genMemberAssign(target, s.Op, val, s.Value)
					c.claimStringTemp(val)
					// B0312: When RHS is opt!, neutralize the source optional so its
					// drop doesn't double-free the inner value now owned by this field.
					c.neutralizeForceUnwrapSource(s.Value)
				}
				break
			}
		}
		c.genMemberAssign(target, s.Op, val, s.Value)
		// Clear drop flag on RHS if it's being moved via simple assign
		if s.Op == ast.OpAssign {
			if ident, ok := s.Value.(*ast.IdentExpr); ok {
				c.clearDropFlag(ident.Name)
			}
			// T0754: clear cast subject's drop flag — ownership moves it at
			// the owning-slot store, so the subject's scope-exit drop must
			// not fire on the same allocation the field now owns. T0849: for
			// the conditional `as` form, drop iff the downcast failed.
			if ident := c.castSubjectMovableIdent(s.Value); ident != nil {
				c.consumeCastSubjectDropFlag(s.Value, ident.Name)
			}
			// B0168: Claim string temp — ownership transferred to field.
			c.claimStringTemp(val)
			// B0233: Claim heap temp — ownership transferred to field.
			c.claimHeapTemp(val)
			// B0312: When RHS is opt!, neutralize the source optional so its
			// drop doesn't double-free the inner value now owned by this field.
			c.neutralizeForceUnwrapSource(s.Value)
			// T0899: an operator/method RHS whose body is `return this` returns the
			// borrowed receiver as an owned result, aliasing the operand local. The
			// field now owns that instance, so clear the operand's drop flag —
			// otherwise both the operand local and the field owner free it
			// (double-free). Mirrors the IdentExpr branch (T0892) and the two
			// var-decl paths. The self-alias case (`h.f = h.f + b`) is handled by
			// genMemberAssign's same-pointer drop-old guard (its non-Ident origin
			// skips the clear); see clearOperandAliasForOwnedStore.
			c.clearOperandAliasForOwnedStore(s.Value, val)
		}

	case *ast.IndexExpr:
		// B0195: Vector[string] index assign — dup new value so vector owns
		// an independent copy (like push, B0189). Source retains its string.
		// Old element is NOT dropped here (see B0204 for why).
		if s.Op == ast.OpAssign {
			idxTargetType := c.info.Types[target.Target]
			// T0386: Inside generic method bodies, c.info.Types[ThisExpr] returns
			// the bare Named owner without TypeArgs bound. Use c.monoCtx.inst
			// (the concrete Instance) so types.AsVector succeeds and the
			// per-element string-dup fires inside Vector[T].[:]=.
			if isThisReceiver(target.Target) && c.monoCtx != nil {
				idxTargetType = c.monoCtx.inst
			}
			if c.typeSubst != nil {
				idxTargetType = types.Substitute(idxTargetType, c.typeSubst)
			}
			// Unwrap borrows (auto-deref through &/&mut)
			if ref, ok := idxTargetType.(*types.MutRef); ok {
				idxTargetType = ref.Elem()
			}
			if ref, ok := idxTargetType.(*types.SharedRef); ok {
				idxTargetType = ref.Elem()
			}
			if elemType, isVec := types.AsVector(idxTargetType); isVec {
				resolvedElem := elemType
				if c.typeSubst != nil {
					resolvedElem = types.Substitute(resolvedElem, c.typeSubst)
				}
				if extractNamed(resolvedElem) == types.TypString {
					dupVal := c.dupString(val)
					c.genIndexAssign(target, s.Op, dupVal, s.Value)
					// Note: do NOT neutralize opt! source here — dupString creates an
					// independent copy for the vector, so the original stays owned by
					// the source optional (whose drop frees it at scope exit).
					break
				}
			}
			// B0350: Map[K,string] index assign from borrow param — dup value
			// so the map owns an independent copy. Borrow params have no drop
			// flag, so clearDropFlag below is a no-op and the caller still frees
			// the original → double-free without this dup.
			if _, valType, isMap := types.AsMap(idxTargetType); isMap {
				resolvedVal := valType
				if c.typeSubst != nil {
					resolvedVal = types.Substitute(resolvedVal, c.typeSubst)
				}
				if extractNamed(resolvedVal) == types.TypString {
					if ident, ok := s.Value.(*ast.IdentExpr); ok {
						if _, hasFlag := c.dropFlags[ident.Name]; !hasFlag {
							val = c.dupString(val)
						}
					} else if isStringBorrowExpr(s.Value) {
						// B0355: non-ident borrow expr (field access, container element) as map value —
						// the source still owns the pointer; dup so map holds an independent copy.
						val = c.dupString(val)
					}
				}
			}
			// T0599/T0615: Wrap bare RHS in Optional when an array/vector slot
			// type is Optional but the expr is not (mirrors the MemberExpr-LHS
			// path, stmt.go:5485-5528, and the IdentExpr-LHS path,
			// stmt.go:5371-5396). Without this, genArrayIndexAssign /
			// genVectorIndexAssign store a bare T into a {i1, T} slot →
			// "store operands are not compatible".
			//
			// Gated to *types.Array and Vector: both route to a path that does
			// raw NewStore (genArrayIndexAssign / genVectorIndexAssign) with no
			// argument coercion. Vector's []= is `native` so it bypasses the
			// argument-passing coercion that the original T0599 gating assumed
			// would handle the wrap. Map's []= is a normal Promise method so
			// argument-passing already wraps bare values into Optional —
			// wrapping here would double-wrap and corrupt every Map[K,V?]
			// index assign, so Map is intentionally excluded. idxTargetType is
			// already MutRef/SharedRef-unwrapped above, matching genIndexAssign's
			// own dispatch (stmt.go:8410-8421). For arrays/vectors
			// c.info.Types[target] is the element type (sema checkIndexExpr
			// returns the [] return type), i.e. directly the slot type.
			_, isArr := idxTargetType.(*types.Array)
			_, isVec := types.AsVector(idxTargetType)
			if isArr || isVec {
				slotType := c.info.Types[target]
				exprType := c.info.Types[s.Value]
				if c.typeSubst != nil {
					slotType = types.Substitute(slotType, c.typeSubst)
					exprType = types.Substitute(exprType, c.typeSubst)
				}
				if _, isOpt := slotType.(*types.Optional); isOpt && exprType != types.TypNone &&
					!types.Identical(exprType, slotType) {
					// T0394/T0111/T0555 pattern: string & native-handle/container
					// temps are tracked by direct val-identity in stmtTempMap,
					// which fails to match once val becomes the wrapped struct —
					// claim BEFORE wrapping. Heap user-type temps are still
					// claimed correctly post-wrap by claimHeapTemp's
					// struct-extraction fallback (B0233), so they are excluded.
					if extractNamed(exprType) == types.TypString ||
						types.IsVector(exprType) || types.IsChannel(exprType) ||
						types.IsArc(exprType) || types.IsWeak(exprType) ||
						types.IsTask(exprType) || types.IsMutex(exprType) ||
						types.IsMutexGuard(exprType) {
						c.claimStringTemp(val)
					}
					optType := c.resolveType(slotType)
					if st, ok := optType.(*irtypes.StructType); ok {
						val = c.wrapOptional(val, st)
					}
				}
			}
		}
		c.genIndexAssign(target, s.Op, val, s.Value)
		// Clear drop flag on RHS if it's being moved via simple assign
		if s.Op == ast.OpAssign {
			if ident, ok := s.Value.(*ast.IdentExpr); ok {
				c.clearDropFlag(ident.Name)
			}
			// T0754: clear cast subject's drop flag — ownership moves it at
			// the owning-slot store, so the subject's scope-exit drop must
			// not fire on the same allocation the container element now owns.
			// T0849: for the conditional `as` form, drop iff the downcast failed.
			if ident := c.castSubjectMovableIdent(s.Value); ident != nil {
				c.consumeCastSubjectDropFlag(s.Value, ident.Name)
			}
			// B0168: Claim string temp — ownership transferred to container.
			c.claimStringTemp(val)
			// B0233: Claim heap temp — ownership transferred to container.
			c.claimHeapTemp(val)
			// B0309: When RHS is opt!, neutralize the source optional so its
			// drop doesn't double-free the inner value now owned by the container.
			c.neutralizeForceUnwrapSource(s.Value)
			// T0597: When RHS is an Optional[X] field-read or array-index from a
			// droppable owner, genFieldAccess/genArrayIndex set these sentinels
			// to the bare inner-dup pointer. The wrapped {i1, ptr} struct passed
			// to claimStringTemp/claimHeapTemp above won't match the stmtTempMap
			// entry (which keys on the inner pointer). Without claiming per
			// sentinel here, cleanupStmtTemps drops the inner pointer at
			// statement end, then the container slot's drop at scope exit drops
			// the same pointer again → double-free. Mirrors T0498's per-arg
			// claim in constructor field-init and existing claims at
			// var-decl/return sites.
			if c.optionalStringDup != nil {
				c.claimStringTemp(c.optionalStringDup)
				c.optionalStringDup = nil
			}
			if c.optionalContainerDup != nil {
				c.claimStringTemp(c.optionalContainerDup)
				c.optionalContainerDup = nil
			}
			if c.optionalTupleDup != nil {
				c.claimHeapTemp(c.optionalTupleDup)
				c.optionalTupleDup = nil
			}
			if c.optionalHeapDup != nil {
				c.claimHeapTemp(c.optionalHeapDup)
				c.optionalHeapDup = nil
			}
			// T0899: an operator/method RHS whose body is `return this` returns the
			// borrowed receiver as an owned result, aliasing the operand local. The
			// container element now owns that instance, so clear the operand's drop
			// flag — otherwise both the operand local and the container free it
			// (double-free). Mirrors the IdentExpr branch (T0892) and the two
			// var-decl paths. The self-alias case (`v[0] = v[0] + b`) is handled by
			// genVectorIndexAssign's same-pointer drop-old guard (its non-Ident
			// origin skips the clear); see clearOperandAliasForOwnedStore. The
			// Vector[string]/Map[K,string] sub-paths dup rather than alias and break
			// before here.
			c.clearOperandAliasForOwnedStore(s.Value, val)
		}
		// Clear drop flag on index key if it's being stored (e.g., map[key] = val).
		// The map takes ownership of the key pointer.
		if s.Op == ast.OpAssign {
			if ident, ok := target.Index.(*ast.IdentExpr); ok {
				c.clearDropFlag(ident.Name)
			}
			// B0309: When opt! is used as a map key, neutralize the source
			// optional so its drop doesn't double-free the unwrapped key.
			c.neutralizeForceUnwrapSource(target.Index)
		}

	case *ast.SliceExpr:
		c.genSliceAssign(target, val)
		if s.Op == ast.OpAssign {
			if ident, ok := s.Value.(*ast.IdentExpr); ok {
				// B0313: For non-string element types, the [:]= method aliases
				// element pointers; we free the source backing array here, skip
				// normal vecdrop on the source (clearDropFlag) to avoid double-free.
				// T0386: For string element type, Patch 1 makes [:]= dup
				// strings via B0195, so the source retains independent
				// ownership of its elements — running B0313's destructive
				// path would orphan and leak them, and disarming the source's
				// drop flag would leak the source's backing array + element
				// strings. Let normal scope cleanup handle the source vector.
				rhsType := c.info.Types[s.Value]
				if c.typeSubst != nil {
					rhsType = types.Substitute(rhsType, c.typeSubst)
				}
				// T0490: Same skip applies to any element type whose [:]= body
				// dups elements on read — string (T0386), tuple-needs-drop
				// (T0412 dupTupleFieldAccess), heap user-type (T0398
				// dupHeapUserFieldAccess). Symmetric with the dup-flag set
				// in the IndexExpr-RHS branch above. Plain Vector[T]/Channel/
				// Arc/Weak/enum elements still alias on read inside [:]=, so
				// B0313's destructive shallow Vector.drop is correct for them.
				skipB0313 := false
				if elemType, isVec := types.AsVector(rhsType); isVec {
					resolvedElem := elemType
					if c.typeSubst != nil {
						resolvedElem = types.Substitute(resolvedElem, c.typeSubst)
					}
					isTupNeedsDrop := false
					if _, isTup := resolvedElem.(*types.Tuple); isTup && c.tupleNeedsDrop(resolvedElem) {
						isTupNeedsDrop = true
					}
					switch {
					case extractNamed(resolvedElem) == types.TypString:
						skipB0313 = true
					case isTupNeedsDrop:
						skipB0313 = true
					case isDroppableHeapUserType(resolvedElem):
						skipB0313 = true
					default:
						alloca := c.locals[ident.Name]
						srcPtr := c.block.NewLoad(irtypes.I8Ptr, alloca)
						c.block.NewCall(c.funcs["Vector.drop"], srcPtr)
					}
				}
				if !skipB0313 {
					c.clearDropFlag(ident.Name)
				}
			}
			// B0312: When RHS is opt!, neutralize the source optional so its
			// drop doesn't double-free the inner value now owned by the slice target.
			c.neutralizeForceUnwrapSource(s.Value)
		}

	default:
		panic(fmt.Sprintf("codegen: unsupported assignment target %T", s.Target))
	}
}

// genMemberAssign handles assignment to a field on a user type instance.
// If the member is a setter property, emits a setter call instead.
// Uses lookupTypeLayout for layout-driven field types that work for both
// regular and monomorphic types.
// srcExpr (may be nil) is the RHS source AST; used by the T0351 defensive
// dup path in genMutexGuardBorrowSet to detect a borrow-param string.
func (c *Compiler) genMemberAssign(target *ast.MemberExpr, op ast.AssignOp, val value.Value, srcExpr ast.Expr) {
	// Check for setter property
	targetType := c.info.Types[target.Target]
	if c.typeSubst != nil {
		targetType = types.Substitute(targetType, c.typeSubst)
	}

	// T0156: MutexGuard.borrow setter — intercept before generic setter lookup.
	// Container types are opaque i8* and need custom setter codegen.
	if target.Field == "borrow" {
		if elem, ok := types.AsMutexGuard(targetType); ok {
			resolvedElem := elem
			if c.typeSubst != nil {
				resolvedElem = types.Substitute(elem, c.typeSubst)
			}
			c.genMutexGuardBorrowSet(target, op, val, resolvedElem, srcExpr)
			return
		}
		guardNamed := extractNamed(targetType)
		if guardNamed == types.TypMutexGuard {
			if tp := c.resolveTypeParam(types.TypMutexGuard.TypeParams()[0]); tp != nil {
				c.genMutexGuardBorrowSet(target, op, val, tp, srcExpr)
				return
			}
		}
	}

	named := extractNamed(targetType)
	if named != nil {
		if setter := named.LookupSetter(target.Field); setter != nil {
			if op != ast.OpAssign {
				// Compound assignment (+=, -=, etc.): read via getter, apply op, write via setter
				getter := named.LookupGetter(target.Field)
				if getter == nil {
					panic(fmt.Sprintf("codegen: compound assignment to setter %s.%s but no getter found", named, target.Field))
				}
				current := c.genGetterCall(target, targetType, named, getter)
				current = c.unwrapFailableCompoundRead(current, c.info.Types[target]) // T0709
				val = c.genCompoundOp(op, c.info.Types[target], current, val)
			}
			c.genSetterCall(target, targetType, named, setter, val)
			return
		}
	}

	fieldPtr := c.genFieldPtr(target)

	if op == ast.OpAssign {
		// B0216/B0219: Drop old field value before reassignment for types that own heap memory.
		// Without this, overwriting a heap-allocated field leaks the old value.
		// Safe because field reads that save to locals create dups (T0095/B0219).
		if named != nil {
			field := named.LookupField(target.Field)
			if field != nil {
				// T0390: Use sema's resolved type for `target` rather than
				// `field.Type()`. Sema substitutes the field's TypeParam through
				// the receiver Instance's TypeArgs, so this is concrete even
				// outside generic context (where `c.typeSubst` is nil) — without
				// this, drop blocks below silently miss generic-typed fields and
				// leak the old value. See also T0368 (same root cause, compound).
				fieldType := c.info.Types[target]
				if c.typeSubst != nil {
					fieldType = types.Substitute(fieldType, c.typeSubst)
				}
				// String: call promise_string_drop (handles null + literal checks).
				if extractNamed(fieldType) == types.TypString {
					if dropFunc, ok := c.funcs["promise_string_drop"]; ok {
						oldVal := c.block.NewLoad(irtypes.I8Ptr, fieldPtr)
						isSame := c.block.NewICmp(enum.IPredEQ, oldVal, val)
						dropBlock := c.newBlock("field.strdrop")
						mergeBlock := c.newBlock("field.strdrop.done")
						c.block.NewCondBr(isSame, mergeBlock, dropBlock)
						c.block = dropBlock
						c.block.NewCall(dropFunc, oldVal)
						c.block.NewBr(mergeBlock)
						c.block = mergeBlock
					}
				}
				// B0219/T0405: Vector: drop elements first, then call Vector.drop (handles null + static flag).
				// Guard: skip if old == new (same pointer) OR old is null (zero-initialized from error
				// fallthrough). emitVectorElementDropLoop reads the header unconditionally, so we must
				// null-check here; Vector.drop has its own internal null check but the loop does not.
				if elemType, isVec := types.AsVector(fieldType); isVec {
					if dropFunc, ok := c.funcs["Vector.drop"]; ok {
						oldVal := c.block.NewLoad(irtypes.I8Ptr, fieldPtr)
						isNull := c.block.NewICmp(enum.IPredEQ, oldVal, constant.NewNull(irtypes.I8Ptr))
						isSame := c.block.NewICmp(enum.IPredEQ, oldVal, val)
						skipDrop := c.block.NewOr(isNull, isSame)
						dropBlock := c.newBlock("field.vecdrop")
						mergeBlock := c.newBlock("field.vecdrop.done")
						c.block.NewCondBr(skipDrop, mergeBlock, dropBlock)
						c.block = dropBlock
						c.emitVectorElementDropLoop(oldVal, elemType)
						c.block.NewCall(dropFunc, oldVal)
						c.block.NewBr(mergeBlock)
						c.block = mergeBlock
					}
				}
				// B0219/T0663: Channel: per-element-type drop (handles null +
				// refcount, and walks any un-received buffered items).
				if chanElem, isCh := types.AsChannel(fieldType); isCh {
					dropFunc := c.getOrCreateChannelDrop(chanElem)
					oldVal := c.block.NewLoad(irtypes.I8Ptr, fieldPtr)
					isSame := c.block.NewICmp(enum.IPredEQ, oldVal, val)
					dropBlock := c.newBlock("field.chdrop")
					mergeBlock := c.newBlock("field.chdrop.done")
					c.block.NewCondBr(isSame, mergeBlock, dropBlock)
					c.block = dropBlock
					c.block.NewCall(dropFunc, oldVal)
					c.block.NewBr(mergeBlock)
					c.block = mergeBlock
				}
				// T0155: Arc: call per-instantiation Arc drop (handles null + refcount).
				if arcElem, isArc := types.AsArc(fieldType); isArc {
					resolvedArcElem := arcElem
					if c.typeSubst != nil {
						resolvedArcElem = types.Substitute(arcElem, c.typeSubst)
					}
					dropFunc := c.getOrCreateArcDrop(resolvedArcElem)
					oldVal := c.block.NewLoad(irtypes.I8Ptr, fieldPtr)
					isSame := c.block.NewICmp(enum.IPredEQ, oldVal, val)
					dropBlock := c.newBlock("field.arcdrop")
					mergeBlock := c.newBlock("field.arcdrop.done")
					c.block.NewCondBr(isSame, mergeBlock, dropBlock)
					c.block = dropBlock
					c.block.NewCall(dropFunc, oldVal)
					c.block.NewBr(mergeBlock)
					c.block = mergeBlock
				}
				// T0157: Weak field reassignment drop.
				if weakElem, isWeak := types.AsWeak(fieldType); isWeak {
					resolvedElem := weakElem
					if c.typeSubst != nil {
						resolvedElem = types.Substitute(weakElem, c.typeSubst)
					}
					dropFunc := c.getOrCreateWeakDrop(resolvedElem)
					oldVal := c.block.NewLoad(irtypes.I8Ptr, fieldPtr)
					isSame := c.block.NewICmp(enum.IPredEQ, oldVal, val)
					dropBlock := c.newBlock("field.weakdrop")
					mergeBlock := c.newBlock("field.weakdrop.done")
					c.block.NewCondBr(isSame, mergeBlock, dropBlock)
					c.block = dropBlock
					c.block.NewCall(dropFunc, oldVal)
					c.block.NewBr(mergeBlock)
					c.block = mergeBlock
				}
				// T0156: Mutex field reassignment drop.
				if mutexElem, isMutex := types.AsMutex(fieldType); isMutex {
					resolvedElem := mutexElem
					if c.typeSubst != nil {
						resolvedElem = types.Substitute(mutexElem, c.typeSubst)
					}
					dropFunc := c.getOrCreateMutexDrop(resolvedElem)
					oldVal := c.block.NewLoad(irtypes.I8Ptr, fieldPtr)
					isSame := c.block.NewICmp(enum.IPredEQ, oldVal, val)
					dropBlock := c.newBlock("field.mutexdrop")
					mergeBlock := c.newBlock("field.mutexdrop.done")
					c.block.NewCondBr(isSame, mergeBlock, dropBlock)
					c.block = dropBlock
					c.block.NewCall(dropFunc, oldVal)
					c.block.NewBr(mergeBlock)
					c.block = mergeBlock
				}
				// T0156: MutexGuard field reassignment drop.
				if types.IsMutexGuard(fieldType) {
					if dropFunc := c.funcs["MutexGuard.drop"]; dropFunc != nil {
						oldVal := c.block.NewLoad(irtypes.I8Ptr, fieldPtr)
						isSame := c.block.NewICmp(enum.IPredEQ, oldVal, val)
						dropBlock := c.newBlock("field.guardrop")
						mergeBlock := c.newBlock("field.guardrop.done")
						c.block.NewCondBr(isSame, mergeBlock, dropBlock)
						c.block = dropBlock
						c.block.NewCall(dropFunc, oldVal)
						c.block.NewBr(mergeBlock)
						c.block = mergeBlock
					}
				}
				// T0560: Task field reassignment drop. Without this, `h.t = go ...`
				// for a plain Task[T] field silently leaks the old G handle (the
				// generic dispatch falls into the heap-user-type catch-all which
				// is gated by !isOpaqueContainerType and so skips Task entirely).
				if taskElem, isTask := types.AsTask(fieldType); isTask {
					resolvedElem := taskElem
					if c.typeSubst != nil {
						resolvedElem = types.Substitute(taskElem, c.typeSubst)
					}
					oldVal := c.block.NewLoad(irtypes.I8Ptr, fieldPtr)
					isSame := c.block.NewICmp(enum.IPredEQ, oldVal, val)
					dropBlock := c.newBlock("field.taskdrop")
					mergeBlock := c.newBlock("field.taskdrop.done")
					c.block.NewCondBr(isSame, mergeBlock, dropBlock)
					c.block = dropBlock
					// T0668: cooperative join in a coroutine body (this runs in
					// user code, often a test body / go {}); legacy spin otherwise.
					c.emitTaskJoinAndFree(oldVal, resolvedElem)
					c.block.NewBr(mergeBlock)
					c.block = mergeBlock
				}
				// B0240: Optional fields: drop old inner value before reassignment.
				// When overwriting an optional field (e.g., p.location = none), the old
				// inner value must be freed/dropped to prevent memory leaks.
				if opt, ok := fieldType.(*types.Optional); ok {
					c.emitOptionalFieldReassignDrop(opt, field, targetType, fieldPtr)
				}
				// T0410: Heap user-type fields: drop old instance before reassignment.
				// Without this, `h.f = v[0]` (after dup-on-read clones the RHS via the
				// flag set in genAssignStmt) leaks h.f's previous instance. Symmetric
				// with genVectorIndexAssign's heap user-type branch. Null + same-
				// pointer guard mirrors the existing Vector/Channel branches above.
				// T0908: also cover heap user types with NO drop (isHeapUserNoDropPalFree);
				// emitVariantFieldDrop's B0218 branch pal_frees the old no-drop instance.
				if isDroppableHeapUserType(fieldType) || isHeapUserNoDropPalFree(fieldType) {
					fieldLLVM := c.resolveType(fieldType)
					oldVal := c.block.NewLoad(fieldLLVM, fieldPtr)
					oldInstance := c.extractInstancePtr(oldVal)
					newInstance := c.extractInstancePtr(val)
					isNull := c.block.NewICmp(enum.IPredEQ, oldInstance, constant.NewNull(irtypes.I8Ptr))
					isSame := c.block.NewICmp(enum.IPredEQ, oldInstance, newInstance)
					skipDrop := c.block.NewOr(isNull, isSame)
					dropBlock := c.newBlock("field.userdrop")
					mergeBlock := c.newBlock("field.userdrop.done")
					c.block.NewCondBr(skipDrop, mergeBlock, dropBlock)
					c.block = dropBlock
					c.emitVariantFieldDrop(oldVal, fieldType)
					c.block.NewBr(mergeBlock)
					c.block = mergeBlock
				}
				// T0489: Tuple field reassignment drop. Without this, `obj.tup_field = X`
				// for droppable tuple types leaks the old tuple's heap fields (string,
				// vector buffer, nested user types). emitVariantFieldDrop's tuple branch
				// walks each element via ExtractValue + recursive drop. Mirrors the
				// heap-user-type T0410 branch above and the genVectorIndexAssign tuple
				// branch (T0412). Safe because Part 2 (dup-on-read in genAssignStmt)
				// ensures aliased RHS reads from containers produce independent clones.
				if c.tupleNeedsDrop(fieldType) {
					fieldLLVM := c.resolveType(fieldType)
					oldVal := c.block.NewLoad(fieldLLVM, fieldPtr)
					c.emitVariantFieldDrop(oldVal, fieldType)
				}
			}
		}
		c.block.NewStore(val, fieldPtr)
		// T0909: When RHS is a method/operator whose body is `return this`,
		// the returned value aliases the receiver. Clear the receiver's drop
		// flag so scope-exit doesn't double-free the instance now owned by
		// this field.
		if srcExpr != nil {
			var aliasOrigin ast.Expr
			if call, ok := srcExpr.(*ast.CallExpr); ok {
				aliasOrigin = chainOriginExpr(call)
			} else {
				aliasOrigin = operatorReceiverOrigin(srcExpr)
			}
			fieldType := c.info.Types[target]
			if c.typeSubst != nil {
				fieldType = types.Substitute(fieldType, c.typeSubst)
			}
			if id, ok := aliasOrigin.(*ast.IdentExpr); ok {
				c.maybeClearReceiverDropFlag(val, id.Name, fieldType)
			}
			// ThisExpr origin: inside a method body `this` has no per-variable
			// drop flag (callers own the instance), so no clear needed.
		}
		return
	}

	// Compound assignment: resolve field LLVM type for load
	layout := c.lookupTypeLayout(targetType)
	field := named.LookupField(target.Field)
	var fieldLLVMType irtypes.Type
	if layout.IsValueType {
		fieldIdx := layout.ValueFieldIndex[field.Name()]
		fieldLLVMType = layout.Value.Fields[fieldIdx].LLVMType
	} else {
		fieldIdx := layout.InstanceFieldIndex[field.Name()]
		fieldLLVMType = layout.Instance.Fields[fieldIdx].LLVMType
	}
	current := c.block.NewLoad(fieldLLVMType, fieldPtr)
	// T0368: Use sema's resolved type for `target` rather than `field.Type()`.
	// Sema's resolveInstanceMember substitutes the field's TypeParam through
	// the receiver Instance's TypeArgs, so this is the concrete operand type
	// even outside generic context (where `c.typeSubst` is nil).
	fieldType := c.info.Types[target]
	result := c.genCompoundOp(op, fieldType, current, val)
	// T0363: Drop the old field value before storing the new one. Without
	// this, heap-allocated old values leak. Only string is wired up — no
	// other heap-owning type has a `+` operator (the only compound op).
	if c.typeSubst != nil {
		fieldType = types.Substitute(fieldType, c.typeSubst)
	}
	if extractNamed(fieldType) == types.TypString {
		c.emitStringDropOldValue(current, result)
	}
	c.block.NewStore(result, fieldPtr)
}

// genSetterCall emits a call to a setter method.
// Uses virtual dispatch through the vtable when the static type needs it.
func (c *Compiler) genSetterCall(target *ast.MemberExpr, targetType types.Type, named *types.Named, setter *types.Method, val value.Value) {
	// Global setter (T0703): no receiver, just call the function directly with the value.
	// Mirrors the `global getter path in genGetterCall.
	if setter.Sig().Recv() == nil {
		mangledName := mangleMethodName(c.resolveTypeName(targetType), target.Field, true)
		fn, ok := c.funcs[mangledName]
		if !ok {
			panic(fmt.Sprintf("codegen: undeclared global setter %s", mangledName))
		}
		call := c.block.NewCall(fn, val)
		c.propagateIfFailable(call) // T0708
		return
	}

	// Virtual dispatch for setter when static type needs vtable
	if c.needsVtable(named) && !setter.IsNative() {
		c.genVirtualSetterCall(target, named, setter, val)
		return
	}

	var mangledName string
	ownerName := c.resolveMethodOwner(named, target.Field)
	if ownerName != named.Obj().Name() {
		// T0637: Setter inherited from parent. Resolve to mono name if parent
		// is generic (mirrors genGetterCall / genMethodCall).
		monoOwner := c.resolveMonoParentName(named, targetType, ownerName)
		mangledName = mangleMethodName(monoOwner, target.Field, true)
	} else {
		mangledName = mangleMethodName(c.resolveTypeName(targetType), target.Field, true)
	}

	fn, ok := c.funcs[mangledName]
	if !ok {
		panic(fmt.Sprintf("codegen: undeclared setter %s", mangledName))
	}

	var args []value.Value
	recv := c.genExpr(target.Target)
	if isThisReceiver(target.Target) {
		args = append(args, recv)
	} else if isContainerType(targetType) {
		args = append(args, recv)
	} else if named != nil && named.IsValueType() {
		args = append(args, c.valueTypeReceiverPtr(recv, targetType))
	} else {
		args = append(args, c.extractInstancePtr(recv))
	}
	args = append(args, val)
	call := c.block.NewCall(fn, args...)
	c.propagateIfFailable(call) // T0708
}

// genVirtualSetterCall emits an indirect setter call through the vtable.
func (c *Compiler) genVirtualSetterCall(target *ast.MemberExpr, named *types.Named, setter *types.Method, val value.Value) {
	receiverVal := c.genExpr(target.Target)

	var vtableRaw, instance value.Value
	if isThisReceiver(target.Target) {
		instance = receiverVal
		variantPtr := c.loadVariantPtr(receiverVal)
		typeinfoStruct := irtypes.NewStruct(irtypes.I8Ptr)
		typeinfoPtr := c.block.NewBitCast(variantPtr, irtypes.NewPointer(typeinfoStruct))
		vtableFieldPtr := c.block.NewGetElementPtr(typeinfoStruct, typeinfoPtr,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
		vtableRaw = c.block.NewLoad(irtypes.I8Ptr, vtableFieldPtr)
	} else {
		vtableRaw = c.extractVtablePtr(receiverVal)
		instance = c.extractInstancePtr(receiverVal)
	}

	slotIndex := named.VirtualMethodIndex(target.Field, true) // setter slot
	if slotIndex < 0 {
		panic(fmt.Sprintf("codegen: setter %s not in vtable for %s", target.Field, named))
	}
	vtablePtr := c.block.NewBitCast(vtableRaw, irtypes.NewPointer(irtypes.I8Ptr))
	fnSlotPtr := c.block.NewGetElementPtr(irtypes.I8Ptr, vtablePtr,
		constant.NewInt(irtypes.I32, int64(slotIndex)))
	fnRaw := c.block.NewLoad(irtypes.I8Ptr, fnSlotPtr)

	// Setter signature: (i8* receiver, ValueType val) → void
	valType := c.resolveType(setter.Sig().Params()[0].Type())
	paramTypes := []irtypes.Type{irtypes.I8Ptr, valType}
	retType := irtypes.Type(irtypes.Void)
	if setter.Sig().CanError() {
		retType = computeResultType(retType)
	}
	funcType := irtypes.NewFunc(retType, paramTypes...)
	fnTyped := c.block.NewBitCast(fnRaw, irtypes.NewPointer(funcType))

	call := c.block.NewCall(fnTyped, instance, val)
	c.propagateIfFailable(call) // T0708
}

// genModuleSetterAssign handles assignment to a module-level setter property.
// For compound assignment (+=, -=, etc.), calls getter first, applies op, then calls setter.
func (c *Compiler) genModuleSetterAssign(target *ast.MemberExpr, moduleName string, op ast.AssignOp, val value.Value) {
	setterKey := moduleName + "." + target.Field + "$set"
	setterFn, ok := c.moduleFuncs[setterKey]
	if !ok {
		panic(fmt.Sprintf("codegen: undefined module setter %s.%s", moduleName, target.Field))
	}

	if op != ast.OpAssign {
		// Compound assignment: call getter, apply op, then call setter
		getterKey := moduleName + "." + target.Field
		getterFn, ok := c.moduleFuncs[getterKey]
		if !ok {
			panic(fmt.Sprintf("codegen: compound assignment to module setter %s.%s but no getter found", moduleName, target.Field))
		}
		var current value.Value = c.block.NewCall(getterFn)
		current = c.unwrapFailableCompoundRead(current, c.info.Types[target]) // T0709
		val = c.genCompoundOp(op, c.info.Types[target], current, val)
	}

	call := c.block.NewCall(setterFn, val)
	c.propagateIfFailable(call) // T0708
}

// genCompoundOp applies a compound assignment operator through the type system.
// operandType is the AST type of the operand (current value being modified).
// Required because compound assignment on i8*-shaped types (string, vector,
// channel, etc.) cannot reverse-resolve from the LLVM type alone (T0357).
func (c *Compiler) genCompoundOp(op ast.AssignOp, operandType types.Type, current, val value.Value) value.Value {
	// Map compound op to binary operator name
	var binOp string
	switch op {
	case ast.OpAddAssign:
		binOp = "+"
	case ast.OpSubAssign:
		binOp = "-"
	case ast.OpMulAssign:
		binOp = "*"
	case ast.OpDivAssign:
		binOp = "/"
	case ast.OpModAssign:
		binOp = "%"
	default:
		panic(fmt.Sprintf("codegen: unsupported compound assignment %s", op))
	}

	if operandType == nil {
		panic(fmt.Sprintf("codegen: missing operand type for compound assignment %s", op))
	}
	if c.typeSubst != nil {
		operandType = types.Substitute(operandType, c.typeSubst)
	}
	if c.selfSubst != nil {
		operandType = types.SubstituteSelf(operandType, c.selfSubst.iface, c.selfSubst.concrete)
	}
	named := extractNamed(operandType)
	if named == nil {
		panic(fmt.Sprintf("codegen: cannot resolve Named type from %s for compound assignment %s", operandType, op))
	}

	method := named.LookupMethod(binOp)
	if method == nil {
		panic(fmt.Sprintf("codegen: no method %q on type %s for compound assignment", binOp, named))
	}

	if method.IsNative() {
		// String operators dispatch to runtime intrinsics (mirrors genBinaryOp).
		// The concat result is intentionally NOT tracked as a stmt temp here:
		// every caller stores the result into a location that owns it (local
		// alloca, field, vector slot, map slot, MutexGuard) so cleanup at
		// scope exit handles drop. Tracking here would conflict with stores
		// to non-local sites that don't claim, causing use-after-free.
		if named == types.TypString {
			return c.genStringOp(binOp, current, val)
		}
		return c.emitNativeOp(named, binOp, current, val)
	}

	panic(fmt.Sprintf("codegen: non-native compound op %s.%s not yet implemented", named, binOp))
}

// --- Increment / Decrement ---

// genIncDecStmt generates code for x++ or x-- statements.
func (c *Compiler) genIncDecStmt(s *ast.IncDecStmt) {
	c.genIncDecTarget(s.Target, s.IsInc)
}

// genIncDecTarget applies ++ or -- to the given expression target.
func (c *Compiler) genIncDecTarget(target ast.Expr, isInc bool) {
	op := "++"
	if !isInc {
		op = "--"
	}
	targetType := c.info.Types[target]
	if c.typeSubst != nil {
		targetType = types.Substitute(targetType, c.typeSubst)
	}
	if c.selfSubst != nil {
		targetType = types.SubstituteSelf(targetType, c.selfSubst.iface, c.selfSubst.concrete)
	}
	// targetType may be a primitive (native ++/--), a Named/value/enum user type
	// (non-native dispatch, T0880), or an enum (extractNamed returns nil). Dispatch
	// is handled per-target via emitUnaryOpResult, so no top-level Named is needed.

	switch t := target.(type) {
	case *ast.IdentExpr:
		alloca, ok := c.locals[t.Name]
		if !ok {
			panic(fmt.Sprintf("codegen: undefined variable %q in inc/dec", t.Name))
		}
		current := c.block.NewLoad(alloca.ElemType, alloca)
		result := c.emitUnaryOpResult(op, targetType, current, false)
		// T0880: x++ is `x = x.++()`. A non-native operator returns a NEW value,
		// so the old heap-owned value leaks unless dropped (zero-leak policy).
		c.dropOldUserValueAtPtr(alloca, targetType, result)
		c.block.NewStore(result, alloca)
	case *ast.MemberExpr:
		// T0712: property getter/setter dispatch. genFieldPtr panics ("no field")
		// for a property with no backing field; read via the getter, apply the op,
		// and write via the setter — mirroring genMemberAssign's compound path.
		recvType := c.info.Types[t.Target]
		if c.typeSubst != nil {
			recvType = types.Substitute(recvType, c.typeSubst)
		}
		if recvNamed := extractNamed(recvType); recvNamed != nil {
			if setter := recvNamed.LookupSetter(t.Field); setter != nil {
				getter := recvNamed.LookupGetter(t.Field)
				if getter == nil {
					panic(fmt.Sprintf("codegen: inc/dec on property %s.%s but no getter found", recvNamed, t.Field))
				}
				current := c.genGetterCall(t, recvType, recvNamed, getter)
				// A failable getter returns {i1, T, i8*}; unwrap + propagate the
				// error. T0923: detect failability from the getter signature, NOT
				// the result shape — since T0880 a non-native ++/-- operand can be
				// a user type whose Value struct is itself a *StructType, so the old
				// "struct ⇒ failable" heuristic misfired on non-failable user-type
				// getters. Mirrors the index path's indexMethod.Sig().CanError().
				// genSetterCall already auto-propagates the setter result via
				// propagateIfFailable (T0708).
				if getter.Sig().CanError() {
					current = c.genAutoPropagateValue(current)
				}
				result := c.emitUnaryOpResult(op, targetType, current, false)
				// Drop-old is handled inside the setter (it assigns the backing
				// field via genMemberAssign, which drops the old value).
				c.genSetterCall(t, recvType, recvNamed, setter, result)
				return
			}
		}
		// Load field, apply op, store back
		fieldPtr := c.genFieldPtr(t)
		llvmType := c.resolveType(targetType)
		current := c.block.NewLoad(llvmType, fieldPtr)
		result := c.emitUnaryOpResult(op, targetType, current, false)
		// T0880: drop the old heap-owned field value before overwriting it.
		c.dropOldUserValueAtPtr(fieldPtr, targetType, result)
		c.block.NewStore(result, fieldPtr)
	case *ast.IndexExpr:
		indexTargetType := c.info.Types[t.Target]
		if c.typeSubst != nil {
			indexTargetType = types.Substitute(indexTargetType, c.typeSubst)
		}
		indexNamed := extractNamed(indexTargetType)
		if indexNamed == nil {
			panic(fmt.Sprintf("codegen: inc/dec on index of unresolved type %s", indexTargetType))
		}
		indexMethod := indexNamed.LookupMethod("[]")
		assignMethod := indexNamed.LookupMethod("[]=")

		if indexMethod != nil && indexMethod.IsNative() && assignMethod != nil && assignMethod.IsNative() {
			// Native path: direct memory access (vectors)
			elem, ok := types.AsVector(indexTargetType)
			if !ok && indexNamed == types.TypVector && c.typeSubst != nil {
				tp := indexNamed.TypeParams()[0]
				elem, ok = c.typeSubst[tp], c.typeSubst[tp] != nil
			}
			if !ok {
				panic(fmt.Sprintf("codegen: inc/dec on index of non-vector native type %s", indexTargetType))
			}
			slicePtr := c.genExpr(t.Target)
			idx := c.genExpr(t.Index)
			elemLLVM := c.resolveType(elem)
			elemSize := int64(c.typeSize(elemLLVM))

			// COW: if static (.rodata), copy to heap first (T0062)
			cowSlice := c.block.NewCall(c.funcs["promise_vector_cow"],
				slicePtr, constant.NewInt(irtypes.I64, elemSize))
			c.storeBackSlicePtr(t.Target, cowSlice)

			headerType := vectorHeaderType()
			headerPtr := c.block.NewBitCast(cowSlice, irtypes.NewPointer(headerType))
			length := loadVectorLen(c.block, headerPtr)
			inBounds := c.block.NewICmp(enum.IPredULT, idx, length)
			okBlock := c.newBlock("incdec.index.ok")
			panicBlock := c.newBlock("incdec.index.oob")
			c.block.NewCondBr(inBounds, okBlock, panicBlock)

			c.block = panicBlock
			oobMsg := c.makeGlobalString("index out of bounds")
			c.block.NewCall(c.funcs["promise_panic"], oobMsg)
			c.emitPanicReturn()

			c.block = okBlock
			dataBase := c.block.NewGetElementPtr(irtypes.I8, cowSlice,
				constant.NewInt(irtypes.I64, int64(vectorHeaderSize)))
			dataTypedPtr := c.block.NewBitCast(dataBase, irtypes.NewPointer(elemLLVM))
			elemPtr := c.block.NewGetElementPtr(elemLLVM, dataTypedPtr, idx)
			current := c.block.NewLoad(elemLLVM, elemPtr)
			result := c.emitUnaryOpResult(op, targetType, current, false)
			// T0880: drop the old heap-owned element value before overwriting it.
			c.dropOldUserValueAtPtr(elemPtr, targetType, result)
			c.block.NewStore(result, elemPtr)
		} else if indexMethod != nil && assignMethod != nil {
			// Non-native: read via [], apply op, write via []=
			typeName := c.resolveTypeName(indexTargetType)
			getFnName := mangleMethodName(typeName, "[]", false)
			getFn, ok := c.funcs[getFnName]
			if !ok {
				panic(fmt.Sprintf("codegen: undeclared [] method %s", getFnName))
			}
			setFnName := mangleMethodName(typeName, "[]=", false)
			setFn, ok := c.funcs[setFnName]
			if !ok {
				panic(fmt.Sprintf("codegen: undeclared []= method %s", setFnName))
			}
			targetVal := c.genExpr(t.Target)
			keyVal := c.genExpr(t.Index)
			var instancePtr value.Value
			if isContainerType(indexTargetType) {
				instancePtr = targetVal
			} else {
				instancePtr = c.extractInstancePtr(targetVal)
			}
			// Read, inc/dec, write
			var optVal value.Value = c.block.NewCall(getFn, instancePtr, keyVal)
			if indexMethod.Sig().CanError() { // T0709: failable [] read propagates
				optVal = c.genAutoPropagateValue(optVal)
			}
			var current value.Value
			if _, isOpt := indexMethod.Sig().Result().(*types.Optional); isOpt {
				hasVal := c.block.NewExtractValue(optVal, 0)
				okBlock := c.newBlock("incdec.method.ok")
				panicBlock := c.newBlock("incdec.method.panic")
				c.block.NewCondBr(hasVal, okBlock, panicBlock)

				c.block = panicBlock
				panicMsg := c.makeGlobalString("inc/dec on missing key")
				c.block.NewCall(c.funcs["promise_panic"], panicMsg)
				c.emitPanicReturn()

				c.block = okBlock
				current = c.block.NewExtractValue(optVal, 1)
			} else {
				current = optVal
			}
			result := c.emitUnaryOpResult(op, targetType, current, false)
			// Drop-old is handled inside the []= setter (Map.[]= drops the old
			// slot value before storing), so no explicit drop here.
			setCall := c.block.NewCall(setFn, instancePtr, keyVal, result)
			c.propagateIfFailable(setCall) // T0708
		} else {
			panic(fmt.Sprintf("codegen: inc/dec on index of type %s without []/[]= methods", indexTargetType))
		}
	default:
		panic(fmt.Sprintf("codegen: unsupported inc/dec target %T", target))
	}
}

// dropOldUserValueAtPtr drops the heap-owned value currently stored at ptr
// before an inc/dec store-back overwrites it (T0880). `x++` is `x = x.++()`: a
// non-native operator borrows the receiver and returns a NEW value, so the old
// one leaks unless dropped (zero-leak policy). For a heap user type the drop is
// guarded by a null + instance-pointer alias check, so a `return this` result
// (which aliases the old value) is never freed. Value types and primitives own
// no heap memory, making this a no-op for them. emitVariantFieldDrop is the
// shared per-type drop walk; for an enum with no droppable data (a `copy` /
// fieldless enum) it emits nothing.
//
// Enum caveat (T0922): the enum branch has no alias guard. A sane cycling
// operator returns a FRESH variant, so dropping the old payload is correct and
// leak-free (the realistic case). But an operator that returns a value aliasing
// the receiver payload (e.g. `++() E => this`) double-frees that payload — the
// same pre-existing enum receiver-alias hole that already affects plain methods
// (`f := e.dup()` where `dup` returns `this`), since emitReceiverAliasCheck does
// not cover enum-value receivers. The proper fix (reject the aliasing return in
// ownership, or extend receiver-alias-clear to enums) is tracked in T0922; a
// bytewise guard here would be a partial proxy that breaks on multi-field
// variants, so it is deliberately not added.
func (c *Compiler) dropOldUserValueAtPtr(ptr value.Value, valueType types.Type, newVal value.Value) {
	if c.typeSubst != nil {
		valueType = types.Substitute(valueType, c.typeSubst)
	}
	if c.selfSubst != nil {
		valueType = types.SubstituteSelf(valueType, c.selfSubst.iface, c.selfSubst.concrete)
	}
	llvmType := c.resolveType(valueType)

	if isDroppableHeapUserType(valueType) || isHeapUserNoDropPalFree(valueType) {
		oldVal := c.block.NewLoad(llvmType, ptr)
		oldInstance := c.extractInstancePtr(oldVal)
		newInstance := c.extractInstancePtr(newVal)
		isNull := c.block.NewICmp(enum.IPredEQ, oldInstance, constant.NewNull(irtypes.I8Ptr))
		isSame := c.block.NewICmp(enum.IPredEQ, oldInstance, newInstance)
		skipDrop := c.block.NewOr(isNull, isSame)
		dropBlock := c.newBlock("incdec.userdrop")
		mergeBlock := c.newBlock("incdec.userdrop.done")
		c.block.NewCondBr(skipDrop, mergeBlock, dropBlock)
		c.block = dropBlock
		c.emitVariantFieldDrop(oldVal, valueType)
		c.block.NewBr(mergeBlock)
		c.block = mergeBlock
		return
	}

	// Non-`copy` enum: `++`/`--` returns a fresh value, so drop the old one.
	// (For a copy/fieldless enum emitVariantFieldDrop emits nothing.)
	if extractEnum(valueType) != nil {
		oldVal := c.block.NewLoad(llvmType, ptr)
		c.emitVariantFieldDrop(oldVal, valueType)
	}
}

// --- Return ---

func (c *Compiler) genReturnStmt(s *ast.ReturnStmt) {
	// Generator return: bare return means "stop producing values"
	if c.inGenerator {
		if len(c.scopeBindings) > 0 {
			c.emitScopeCleanup(0, false) // generators have canError=false, so no capture
		}
		// Branch to the single final suspend block
		c.block.NewBr(c.generatorFinalSuspend)
		// c.block already has a terminator, so subsequent codegen is skipped
		return
	}

	// B0353: Goroutine return: bare return means "exit this goroutine".
	// Branch to the coroutine's final suspend block instead of emitting
	// ret void (the coroutine function returns ptr, not void).
	if c.coroutineReturnBlock != nil {
		c.emitLambdaWritebacks()
		if len(c.scopeBindings) > 0 {
			c.emitScopeCleanup(0, false)
		}
		if c.block != nil && c.block.Term == nil {
			c.block.NewBr(c.coroutineReturnBlock)
		}
		return
	}

	// Write back move-captured variables to env struct before returning
	c.emitLambdaWritebacks()

	// Set targetType so NoneLit can resolve to the correct Optional struct
	retType := c.currentRetType
	if retType != nil && c.typeSubst != nil {
		retType = types.Substitute(retType, c.typeSubst)
	}
	if retType != nil && c.selfSubst != nil {
		retType = types.SubstituteSelf(retType, c.selfSubst.iface, c.selfSubst.concrete)
	}

	// Evaluate the return expression BEFORE scope cleanup. The expression may
	// reference local variables with drop bindings (e.g., string variables passed
	// as function arguments: `return func(str_var)`). Scope cleanup frees those
	// variables, so we must compute the return value while they're still alive.
	var val value.Value
	if s.Value != nil {
		c.targetType = retType
		// T0095/B0179/B0219/B0310/T0487: Signal genFieldAccess to dup string,
		// Vector|Channel|Arc|Weak, and Optional[...] fields for return values.
		// Scope cleanup after the return may drop the containing type, freeing
		// the field — the caller needs an independent copy. Skip for borrow
		// return types (B0179) — borrows don't own the value.
		c.setDupFlagsForFieldAccess(retType)
		// T0440: Signal genMethodIndex to dup heap user types out of container
		// indices for Optional[heap-user-type] return values. Without this,
		// `return m[k]` from a function owning m propagates an alias that
		// becomes dangling when m drops at function exit.
		if opt, ok := retType.(*types.Optional); ok {
			elem := opt.Elem()
			if c.typeSubst != nil {
				elem = types.Substitute(elem, c.typeSubst)
			}
			if isDroppableHeapUserType(elem) {
				c.dupHeapUserFieldAccess = true
			}
		}
		val = c.genExpr(s.Value)
		c.dupStringFieldAccess = false
		c.dupContainerFieldAccess = false
		c.dupHeapUserFieldAccess = false
		c.targetType = nil
		val = c.wrapThisReturnValue(val, s.Value, retType)
		val = c.wrapOperatorParamReturnValue(val, s.Value, retType) // T0897
	}

	// B0189: Dup return value if it's a string that might be borrowed from a
	// Vector[string] in the current scope. The element drop loop in scope
	// cleanup will free the vector's elements — if the return value borrows
	// one of those elements, it would become a dangling pointer.
	// Covers: `return strVar` (IdentExpr) and `return vec[i]` (IndexExpr).
	// T0649: skip for borrow return types (`string&`/`string~`) — a borrow
	// return must hand back the actual reference into existing storage (which
	// the ownership pass guarantees outlives the call), not a fresh copy.
	// Dup'ing here would leak: the call site treats a borrow result as a
	// non-owned alias and never frees it. extractNamed unwraps SharedRef/MutRef
	// so the TypString check alone fires for `string&`, hence the explicit
	// isRefType guard.
	needsDup := false
	if s.Value != nil && val != nil && extractNamed(retType) == types.TypString && !isRefType(retType) {
		if _, ok := s.Value.(*ast.IdentExpr); ok {
			needsDup = c.hasVectorStringBinding()
		} else if idx, ok := s.Value.(*ast.IndexExpr); ok {
			targetType := c.info.Types[idx.Target]
			if c.typeSubst != nil {
				targetType = types.Substitute(targetType, c.typeSubst)
			}
			if _, isVec := types.AsVector(targetType); isVec {
				needsDup = true
			}
		}
		if needsDup {
			val = c.dupString(val)
		}
	}

	// Clone return value if it's a droppable enum loaded from a vector index.
	// Scope cleanup drops the dup'd vector (freeing its buffer and all elements) —
	// the shallow enum copy returned by vec[i] would reference freed data.
	// Analogous to the B0189 string dup above.
	// T0649: skip for borrow return types (`MyEnum&`/`MyEnum~`) for the same
	// reason as the string dup — a borrow return must hand back the actual
	// reference, and cloning here would leak at a binding call site.
	if s.Value != nil && val != nil && !needsDup && !isRefType(retType) {
		if idx, ok := s.Value.(*ast.IndexExpr); ok {
			idxTargetType := c.info.Types[idx.Target]
			if c.typeSubst != nil {
				idxTargetType = types.Substitute(idxTargetType, c.typeSubst)
			}
			if elemType, isVec := types.AsVector(idxTargetType); isVec {
				resolvedElem := elemType
				if c.typeSubst != nil {
					resolvedElem = types.Substitute(resolvedElem, c.typeSubst)
				}
				if enum := extractEnum(resolvedElem); enum != nil {
					if c.enumInstanceHasDrop(resolvedElem, enum) {
						if cloned, ok := c.cloneEnumValue(val, resolvedElem); ok {
							val = cloned
							needsDup = true // preserve drop flag for source vector
						}
					}
				}
			}
		}
	}

	// Clear drop flag for returned variable (it's being moved out, not dropped).
	// B0205: When the return value was dup'd (B0189), the original variable must
	// still be dropped at scope exit — the caller receives the dup, not the original.
	// Only clear the flag when we're returning the original (no dup).
	if s.Value != nil && !needsDup {
		if ident, ok := s.Value.(*ast.IdentExpr); ok {
			c.clearDropFlag(ident.Name)
		} else if _, ok := unwrapDestructureParens(s.Value).(*ast.CastExpr); ok {
			// T0783: `return x as! T` aliases x's instance into the returned value;
			// ownership now moves x at the return, so clear x's drop flag to keep
			// codegen symmetric — otherwise x's scope-exit drop fires on the same
			// allocation the caller now owns (double-free).
			//
			// T0849: the optional `as` form (Force == false) yields `T?` and is a
			// *conditional* move (None on a failed downcast). consumeCastSubjectDropFlag
			// reads the outermost cast's Force: `as!` clears unconditionally; `as`
			// stores `!isMatch` so x is dropped iff the downcast failed (else the
			// returned optional owns the aliased instance).
			if ident := c.castSubjectMovableIdent(s.Value); ident != nil {
				c.consumeCastSubjectDropFlag(s.Value, ident.Name)
			}
		}
	}
	// T0108: Clean up statement temps before returning. The return expression may
	// create intermediate string temps (e.g., dupStringFieldAccess dup copies,
	// string concat intermediaries) that are normally freed at statement end.
	// Since return terminates the block, the post-statement cleanup never runs.
	// Claim the return value first so it's not freed — only intermediaries are freed.
	if s.Value != nil && val != nil {
		c.claimStringTemp(val)
		c.claimHeapTemp(val)
		c.claimEnvTemp(val)
	}
	// B0310: Claim dup'd inner string for Optional[string] return values.
	// Without this, cleanupStmtTemps would free the dup while it's still
	// embedded in the return value's optional struct.
	if c.optionalStringDup != nil {
		c.claimStringTemp(c.optionalStringDup)
		c.optionalStringDup = nil
	}
	// T0366: Claim dup'd inner container for Optional[Vector|Channel|Arc|Weak] return values.
	if c.optionalContainerDup != nil {
		c.claimStringTemp(c.optionalContainerDup)
		c.optionalContainerDup = nil
	}
	if c.block != nil && c.block.Term == nil {
		c.cleanupStmtTemps()
		c.cleanupHeapTemps()
		c.cleanupEnvTemps()
	}
	// Emit cleanup for all active scope bindings before returning
	var closeCap *closeErrCapture
	if len(c.scopeBindings) > 0 {
		closeCap = c.emitScopeCleanup(0, false)
	}
	c.emitCloseErrCheck(closeCap)

	if c.canError {
		resultType := c.currentResultType()
		if s.Value == nil {
			c.block.NewRet(c.wrapOk(nil, resultType))
		} else {
			// If the expression is itself a failable call, val is already a
			// failable result struct matching our result type — return directly.
			if c.info.FailableExprs[s.Value] && val != nil && val.Type().Equal(resultType) {
				c.block.NewRet(val)
			} else {
				// Wrap value in Optional if return type is Optional but expr is not
				val = c.wrapReturnOptional(val, s.Value, retType)
				// Coerce value struct vtable when returning through a parent type
				if retType != nil {
					exprType := c.info.Types[s.Value]
					if c.typeSubst != nil {
						exprType = types.Substitute(exprType, c.typeSubst)
					}
					if c.selfSubst != nil {
						exprType = types.SubstituteSelf(exprType, c.selfSubst.iface, c.selfSubst.concrete)
					}
					val = c.coerceToView(val, exprType, retType)
				}
				c.block.NewRet(c.wrapOk(val, resultType))
			}
		}
		return
	}
	if s.Value == nil {
		c.block.NewRet(nil)
	} else {
		// Wrap value in Optional if return type is Optional but expr is not
		val = c.wrapReturnOptional(val, s.Value, retType)
		// Coerce value struct vtable when returning through a parent type
		if retType != nil {
			exprType := c.info.Types[s.Value]
			if c.typeSubst != nil {
				exprType = types.Substitute(exprType, c.typeSubst)
			}
			if c.selfSubst != nil {
				exprType = types.SubstituteSelf(exprType, c.selfSubst.iface, c.selfSubst.concrete)
			}
			val = c.coerceToView(val, exprType, retType)
		}
		c.block.NewRet(val)
	}
}

// wrapThisReturnValue wraps a `this` expression (i8* instance pointer) into the
// appropriate value struct when returning from a method. For heap types, builds
// { vtable_ptr, instance_ptr }. For value types, loads the full value struct from
// the pointer. No-op if the return expression is not ThisExpr.
// T0582: peel ParenExpr so `return (this);` takes the same path as `return this;`.
func (c *Compiler) wrapThisReturnValue(val value.Value, expr ast.Expr, retType types.Type) value.Value {
	expr = unwrapDestructureParens(expr)
	if _, isThis := expr.(*ast.ThisExpr); !isThis {
		return val
	}
	if retType == nil {
		return val
	}
	// T0906: a method returning an optional of the receiver type (`OB?`) reaches
	// here with retType = Optional[OB]. Build the value-struct payload from the
	// `this` instance pointer using the optional's element type; genReturnStmt's
	// subsequent wrapReturnOptional call wraps that payload into the optional.
	// extractNamed does not peel Optional, so without this the function bails and
	// the bare i8* instance pointer flows into wrapOptional's {i8*,i8*} insertvalue
	// (panic: insertvalue elem type mismatch).
	effType := retType
	if opt, ok := retType.(*types.Optional); ok {
		effType = opt.Elem()
	}

	// Enum receiver: `this` is an i8* pointer (genThisExpr), but an enum method
	// returns the enum value by value ({i32 tag, [N x i8] data} or a bare i32 for
	// fieldless enums). Load the value struct via enumThisSubject so the bare i8*
	// pointer never flows into the function result / optional wrap (panic:
	// insertvalue elem type mismatch). Covers both `dup() E { return this; }` and
	// the optional form `dup() E? { return this; }` (T0906). extractNamed returns
	// nil for enums, so this must run before the Named handling below.
	if enumT := extractEnum(effType); enumT != nil {
		layout := c.lookupEnumLayout(effType)
		if layout == nil {
			return val
		}
		enumVal := c.enumThisSubject(val, layout)
		// T0893 analog: a borrowing `return this` whose variant payload is droppable
		// (e.g. `B(string)`) would hand back a shallow copy aliasing the receiver's
		// heap data — both the result and the receiver would free it (double-free).
		// Deep-clone the payload so the returned value owns it independently. Skip
		// for borrow return types (caller expects a reference) and `~this` receivers
		// (genuine ownership transfer — cloning would copy needlessly and leak).
		if !isRefType(effType) && !c.thisRecvIsOwned {
			enumVal = c.cloneOwnedReturnAlias(enumVal, effType)
		}
		return enumVal
	}

	named := extractNamed(effType)
	if named == nil {
		return val
	}
	if classify(named) != CatUnknown || named == types.TypString || named == types.TypVoid || named == types.TypNone {
		return val
	}

	if named.IsValueType() {
		// Value type: `this` is i8* pointing to the value struct — load it
		layout := c.lookupTypeLayout(effType)
		if layout == nil {
			return val
		}
		typedPtr := c.block.NewBitCast(val, irtypes.NewPointer(layout.Value.LLVMType))
		return c.block.NewLoad(layout.Value.LLVMType, typedPtr)
	}

	// Heap type: `this` is i8* instance pointer — build { vtable_ptr, instance_ptr }
	var vtablePtr value.Value
	if c.needsVtable(named) {
		// T0917: For an abstract base / parent return type, lookupVtableGlobal(effType)
		// yields the base's vtable global, whose slots are null for abstract methods —
		// a later virtual call on the result loads a null fn ptr and segfaults. Load the
		// concrete subtype's vtable from the receiver instance's RTTI instead
		// (instance → variant ptr (field 0) → typeinfo vtable_ptr (field 0)) so virtual
		// dispatch on the returned value resolves the override.
		vtablePtr = c.loadVtablePtrFromInstance(val)
	} else if vtGlobal := c.lookupVtableGlobal(effType); vtGlobal != nil {
		vtablePtr = constant.NewBitCast(vtGlobal, irtypes.I8Ptr)
	} else {
		vtablePtr = constant.NewNull(irtypes.I8Ptr)
	}
	var result value.Value = constant.NewUndef(userValueType())
	result = c.block.NewInsertValue(result, vtablePtr, 0)
	result = c.block.NewInsertValue(result, val, 1)

	// T0893: a borrowing method/operator whose body is `return this` (bare receiver)
	// would otherwise hand back a value struct aliasing the receiver's heap instance —
	// the result binding and the receiver then share the same mutable allocation, so
	// one's scope-drop frees memory the other still reads. Clone the instance so the
	// returned owned value is independent. The caller-side alias-clears (B0250/T0341/
	// T0347) remain as harmless no-ops once the pointers differ.
	//
	// Skip when:
	//   - the return type is a borrow (`T&`/`T~`): the caller expects a reference into
	//     existing storage, not a copy (isRefType).
	//   - the receiver is `~this` (RefMut): `this` is owned/moved-in, so `return this`
	//     is a genuine ownership transfer — cloning would copy needlessly and leak the
	//     moved-in instance (c.thisRecvIsOwned).
	if !isRefType(effType) && !c.thisRecvIsOwned {
		result = c.cloneOwnedReturnAlias(result, effType)
	}
	return result
}

// cloneOwnedReturnAlias deep-clones an already-materialized owned return value
// of effType so the returned value owns its heap data independently of whatever
// borrowed source it currently aliases — the `this` receiver (T0893) or a
// borrowed operator operand (T0897). For enums it clones droppable variant
// payloads; for heap user types it dups the instance; value/Copy/string/void/
// none types have no heap alias and are returned unchanged. Callers gate on
// isRefType / ownership before invoking. `val` must already be the
// function-ABI value (enum value, {vtable,instance} struct, or value struct).
func (c *Compiler) cloneOwnedReturnAlias(val value.Value, effType types.Type) value.Value {
	if enumT := extractEnum(effType); enumT != nil {
		if !c.enumInstanceHasDrop(effType, enumT) {
			return val
		}
		if cloned, ok := c.cloneEnumValue(val, effType); ok {
			return cloned
		}
		// Droppable enum without a clone method — dup variant fields in place
		// via an alloca round-trip (same path as maybeDupPushElement).
		alloca := c.createEntryAlloca(val.Type())
		c.block.NewStore(val, alloca)
		c.dupEnumElementInPlace(alloca, effType)
		return c.block.NewLoad(val.Type(), alloca)
	}
	named := extractNamed(effType)
	if named == nil {
		return val
	}
	if classify(named) != CatUnknown || named == types.TypString || named == types.TypVoid || named == types.TypNone {
		return val
	}
	if named.IsValueType() {
		return val
	}
	return c.dupHeapValue(val, effType)
}

// wrapOperatorParamReturnValue deep-clones the return value when an operator
// method body returns one of its borrowed value operands unchanged
// (e.g. `+(S other) S { return other; }`). Operator dispatch borrows operands
// rather than moving them, so the returned value would otherwise alias the
// caller's still-live operand and both bindings would free the same heap
// instance (double-free). Mirrors wrapThisReturnValue's clone-on-`return this`
// (T0893) for the right-hand operand (T0897). The value is already the
// function-ABI value, so only the clone is needed (no i8*→struct wrapping).
func (c *Compiler) wrapOperatorParamReturnValue(val value.Value, expr ast.Expr, retType types.Type) value.Value {
	if val == nil || retType == nil || len(c.currentOpValueParams) == 0 {
		return val
	}
	ident, ok := unwrapDestructureParens(expr).(*ast.IdentExpr)
	if !ok || !c.currentOpValueParams[ident.Name] {
		return val
	}
	// Borrow return type: hand back the reference into existing storage, not a copy.
	if isRefType(retType) {
		return val
	}
	effType := retType
	if opt, ok := retType.(*types.Optional); ok {
		effType = opt.Elem()
	}
	return c.cloneOwnedReturnAlias(val, effType)
}

// chainOriginExpr walks a possibly-chained method call back to its base
// expression. For `w.f().g().h()` returns the IdentExpr `w`; for
// `this.f().g()` returns the ThisExpr; for chains rooted in a non-method
// expression (constructor, free call, field access) returns that expression
// (which the caller's switch will ignore). T0347.
// T0582: peel ParenExpr at each step so `(w).f()`, `(w.f()).g()`, and
// `((w).f()).g()` all resolve to the underlying receiver.
//
// NOTE: keep in sync with ownership.aliasReceiverOrigin, which mirrors this to
// suppress NLL early-drop of the aliasing `return this` result (T0889). If which
// origins trigger the alias-clear here change, the NLL mirror must change too —
// otherwise codegen clears a drop flag the NLL pass does not suppress,
// reintroducing the use-after-free.
func chainOriginExpr(call *ast.CallExpr) ast.Expr {
	var expr ast.Expr = call
	for {
		expr = unwrapDestructureParens(expr)
		c, ok := expr.(*ast.CallExpr)
		if !ok {
			return expr
		}
		m, ok := c.Callee.(*ast.MemberExpr)
		if !ok {
			return expr
		}
		expr = m.Target
	}
}

// operatorReceiverOrigin returns the receiver-origin expression of a binary or
// prefix-unary operator that dispatches to a user-defined operator method — the
// LEFT operand of a binary operator (it becomes `this`), or the operand of a
// prefix unary operator. Used to extend the B0250/T0341 receiver-alias-clear to
// operator dispatch: a user operator whose body is `return this` yields a result
// that aliases this operand, and without the clear both would free the same
// instance (T0882). Returns nil for non-operators, the AST-level special binary
// forms (&&, ||, ?:, .., ..= — never alias a heap receiver) and <- receive.
// The downstream maybeClearReceiverDropFlag / pendingThisAliasClear guards
// (heap-user retType, exact {i8*,i8*} val shape) make a nil-safe over-call cheap
// and correct, so no native/value-type pre-filtering is needed here.
func operatorReceiverOrigin(e ast.Expr) ast.Expr {
	switch ex := e.(type) {
	case *ast.BinaryExpr:
		switch ex.Op {
		case ast.BinAnd, ast.BinOr, ast.BinElvis,
			ast.BinExclusiveRange, ast.BinInclusiveRange:
			return nil
		}
		return unwrapDestructureParens(ex.Left)
	case *ast.UnaryExpr:
		if ex.Op == ast.UnaryReceive {
			return nil
		}
		return unwrapDestructureParens(ex.Operand)
	}
	return nil
}

// maybeClearReceiverDropFlag emits a runtime check: if the method call result's
// instance pointer matches the receiver variable's instance pointer, clear the
// receiver's drop flag. This prevents double-free when a borrowing method does
// `return this` — both the receiver and the result would otherwise own the same
// heap allocation. B0250.
func (c *Compiler) maybeClearReceiverDropFlag(val value.Value, recvName string, retType types.Type) {
	if retType == nil {
		return
	}
	named := extractNamed(retType)
	if named == nil || classify(named) != CatUnknown || named == types.TypString || named == types.TypVoid || named == types.TypNone || named.IsValueType() {
		return
	}
	recvAlloca, exists := c.locals[recvName]
	if !exists {
		return
	}
	flag, hasDrop := c.dropFlags[recvName]
	if !hasDrop {
		return
	}

	// T0562: val must be exactly {i8*, i8*} (a bare user value struct). After
	// Optional/Tuple wrapping val becomes e.g. {i1, {i8*,i8*}} — field 1 is no
	// longer the instance pointer, and emitting the icmp would either crash
	// the IR builder (struct vs ptr) or compare wrong bytes.
	if !isUserValueStructType(val.Type()) {
		return
	}
	// T0562: recvAlloca's pointee must also be {i8*, i8*}. Native handles
	// (Arc/Weak/Mutex/MutexGuard/Channel/Vector/Task) use `alloca i8*` — loading
	// userValueType() would read 16 bytes from an 8-byte slot (UB), and if the
	// trailing garbage happens to match the result's inner pointer, the receiver's
	// drop flag would be wrongly cleared and the handle would leak.
	if !isUserValueStructType(recvAlloca.ElemType) {
		return
	}
	retInst := c.block.NewExtractValue(val, 1)
	recvVal := c.block.NewLoad(userValueType(), recvAlloca)
	recvInst := c.block.NewExtractValue(recvVal, 1)
	same := c.block.NewICmp(enum.IPredEQ, retInst, recvInst)

	clearBlk := c.newBlock("return.this.clear")
	skipBlk := c.newBlock("return.this.skip")
	c.block.NewCondBr(same, clearBlk, skipBlk)

	clearBlk.NewStore(constant.False, flag)
	clearBlk.NewBr(skipBlk)

	c.block = skipBlk
}

// clearOperandAliasForOwnedStore clears the drop flag of an operator/method
// operand when the RHS result aliases it via `return this`, for owned-slot
// assignment targets (field/element) that have no target-local drop flag. The
// self-alias case (`h.f = h.f + b` / `v[0] = v[0] + b`) has a non-Ident origin
// (MemberExpr/IndexExpr), so it is skipped here and handled by the target's own
// same-pointer drop-old guard in genMemberAssign/genVectorIndexAssign. A
// ThisExpr origin is likewise skipped (this is borrowed, and sema forbids moving
// it into an owned slot). The downstream maybeClearReceiverDropFlag runtime icmp
// makes a non-aliasing (fresh-value) call a no-op. T0899; shared by the
// MemberExpr and IndexExpr branches of genAssignStmt.
func (c *Compiler) clearOperandAliasForOwnedStore(rhs ast.Expr, val value.Value) {
	var aliasOrigin ast.Expr
	if call, ok := rhs.(*ast.CallExpr); ok {
		aliasOrigin = chainOriginExpr(call)
	} else {
		aliasOrigin = operatorReceiverOrigin(rhs)
	}
	origin, ok := aliasOrigin.(*ast.IdentExpr)
	if !ok {
		return
	}
	exprType := c.info.Types[rhs]
	if c.typeSubst != nil {
		exprType = types.Substitute(exprType, c.typeSubst)
	}
	c.maybeClearReceiverDropFlag(val, origin.Name, exprType)
}

// isUserValueStructType reports whether t is exactly the user value struct
// shape {i8*, i8*}. Used by the B0250/T0347 alias-clear emitters to guard
// against Optional/Tuple-wrapped values and native-handle allocas. T0562.
func isUserValueStructType(t irtypes.Type) bool {
	st, ok := t.(*irtypes.StructType)
	if !ok || len(st.Fields) != 2 {
		return false
	}
	return st.Fields[0].Equal(irtypes.I8Ptr) && st.Fields[1].Equal(irtypes.I8Ptr)
}

// maybeClearBindingDropFlagOnThisAlias emits a runtime check: if the method
// call result's instance pointer matches the current method's `this`, clear
// the new binding's drop flag. Mirrors maybeClearReceiverDropFlag but applies
// to `r := this.method()` (or any chain rooted at `this`) inside a method:
// `this` itself has no drop flag (it's borrowed), so we must clear the binding's
// flag instead of a receiver's. T0347.
func (c *Compiler) maybeClearBindingDropFlagOnThisAlias(val value.Value, bindingFlag value.Value, retType types.Type) {
	if retType == nil || bindingFlag == nil {
		return
	}
	named := extractNamed(retType)
	if named == nil || classify(named) != CatUnknown ||
		named == types.TypString || named == types.TypVoid ||
		named == types.TypNone || named.IsValueType() {
		return
	}
	thisAlloca, ok := c.locals["this"]
	if !ok {
		return
	}
	// T0562: val must be exactly {i8*, i8*}. After Optional/Tuple wrapping
	// (e.g., `Box? r = this.clone()`) val is `{i1, {i8*,i8*}}` and field 1 is
	// a struct, which would crash the icmp emit. Bail in those shapes — the
	// inner pointer is no longer at field 1.
	if !isUserValueStructType(val.Type()) {
		return
	}
	retInst := c.block.NewExtractValue(val, 1)
	thisVal := c.block.NewLoad(thisAlloca.ElemType, thisAlloca)
	var thisInst value.Value
	if structTy, isStruct := thisVal.Type().(*irtypes.StructType); isStruct && len(structTy.Fields) == 2 {
		thisInst = c.block.NewExtractValue(thisVal, 1)
	} else if thisVal.Type() == irtypes.I8Ptr {
		thisInst = thisVal
	} else {
		return
	}
	same := c.block.NewICmp(enum.IPredEQ, retInst, thisInst)

	clearBlk := c.newBlock("this.alias.clear")
	skipBlk := c.newBlock("this.alias.skip")
	c.block.NewCondBr(same, clearBlk, skipBlk)

	clearBlk.NewStore(constant.False, bindingFlag)
	clearBlk.NewBr(skipBlk)

	c.block = skipBlk
}

// --- Raise ---

func (c *Compiler) genRaiseStmt(s *ast.RaiseStmt) {
	// T0110: Generate the raise value expression BEFORE scope cleanup.
	// Constructor expressions (e.g., raise MyError(message: msg)) move string
	// fields from local variables (clearing their drop flags). If scope cleanup
	// ran first, it would free those variables before the constructor could use them.
	errVal := c.genExpr(s.Value)

	// T0086: If raising a local error variable, clear its drop flag so
	// emitScopeCleanup won't free the instance we're about to return.
	if ident, ok := s.Value.(*ast.IdentExpr); ok {
		c.clearDropFlag(ident.Name)
	}
	// T0784: same for `raise x as!/as T` — the cast is a view, so without
	// this clear the subject's scope-exit drop fires on the same allocation
	// the error slot now owns → double-free.
	if ident := c.castSubjectMovableIdent(s.Value); ident != nil {
		// T0849: for the conditional `as` form, drop iff the downcast failed.
		c.consumeCastSubjectDropFlag(s.Value, ident.Name)
	}

	// T0962: Clean up statement temps before raising. The raise expression may
	// create intermediate temps that are normally freed at statement end (e.g.
	// `raise error(message: "x: " + ch.to_string())` produces a throwaway
	// `ch.to_string()` string). Since raise terminates the block, the
	// post-statement cleanup never runs and those intermediaries leak. Use the
	// error-path variants (T0103) — like emitFailableResultPropagation — which
	// emit flag-guarded frees WITHOUT resetting the tracking state: a raise can
	// be nested inside a larger statement (e.g. a match arm) whose sibling/ok
	// path still needs the temp list intact for its own end-of-statement
	// cleanup. Claim the raised value first so its instance and embedded message
	// survive; only the throwaways are freed.
	c.claimStringTemp(errVal)
	c.claimHeapTemp(errVal)
	if c.block != nil && c.block.Term == nil {
		c.emitStmtTempCleanupForErrorPath()
		c.emitHeapTempCleanupForErrorPath()
	}

	// Emit close() for all active use bindings before raising
	if len(c.scopeBindings) > 0 {
		c.emitScopeCleanup(0, true) // error in flight — suppress close errors
	}
	// Error types are user types with value struct {vtable_ptr, instance_ptr}.
	// Extract the instance pointer (i8*) for storage in the result struct's error slot.
	if st, ok := errVal.Type().(*irtypes.StructType); ok && len(st.Fields) == 2 {
		errVal = c.block.NewExtractValue(errVal, 1)
	}
	if c.inGenerator && c.generatorCanError {
		// B0023: store error to generator error_slot and branch to final suspend
		c.emitGeneratorError(errVal)
	} else {
		resultType := c.currentResultType()
		c.block.NewRet(c.wrapError(errVal, resultType))
	}
}

// --- If statement ---

func (c *Compiler) genIfStmt(s *ast.IfStmt) {
	if s.Binding != "" {
		c.genIfUnwrapStmt(s)
		return
	}

	// Check for optional narrowing
	if narrow := c.info.OptionalNarrowings[s]; narrow != nil {
		c.genIfNarrowStmt(s, narrow)
		return
	}

	// Check for destructure is-pattern narrowing
	if destructNarrow := c.info.IsDestructureNarrowings[s]; destructNarrow != nil {
		c.genIfDestructureIsStmt(s, destructNarrow)
		return
	}

	cond := c.genExpr(s.Cond)

	thenBlock := c.newBlock("if.then")
	mergeBlock := c.newBlock("if.end")

	var elseBlock *ir.Block
	if s.Else != nil {
		elseBlock = c.newBlock("if.else")
		c.block.NewCondBr(cond, thenBlock, elseBlock)
	} else {
		c.block.NewCondBr(cond, thenBlock, mergeBlock)
	}

	// B0173: Save heap/env temps from the condition expression so branches don't
	// prematurely clean them. Cleanup runs once in the merge block.
	savedHeapTemps := c.heapTemps
	savedHeapTempMap := c.heapTempMap
	c.heapTemps = nil
	c.heapTempMap = make(map[value.Value]int)
	savedEnvTempsIf := c.envTemps     // T0100
	savedEnvTempMapIf := c.envTempMap // T0100
	c.envTemps = nil
	c.envTempMap = make(map[value.Value]int)

	// B0198: Save condition's string temps so branches don't permanently clear them.
	// Branches see the condition temps (for cleanup on return paths), but after each
	// branch we restore from the snapshot so the next branch and merge block also
	// emit flag-guarded cleanup. The flag system prevents double-free: if a branch
	// already dropped the temp, its flag is cleared and merge-block cleanup is a no-op.
	savedCondStmtTemps := append([]stmtTemp(nil), c.stmtTemps...)
	savedCondStmtTempMap := make(map[value.Value]int, len(c.stmtTempMap))
	for k, v := range c.stmtTempMap {
		savedCondStmtTempMap[k] = v
	}

	// Then branch
	c.block = thenBlock
	if c.shouldInstrument() {
		pos := s.Body.Pos()
		end := s.Body.End()
		idx := c.addCoverageRegion(pos.File, pos.Line, end.Line, c.currentCoverageFuncName(), "if.then")
		c.emitCoverageIncrement(idx)
	}
	c.genBlock(s.Body)
	if c.block.Term == nil {
		c.block.NewBr(mergeBlock)
	}

	// Else branch
	if s.Else != nil {
		// B0198: Restore condition temps so else-branch can also emit cleanup.
		c.stmtTemps = append([]stmtTemp(nil), savedCondStmtTemps...)
		c.stmtTempMap = make(map[value.Value]int, len(savedCondStmtTempMap))
		for k, v := range savedCondStmtTempMap {
			c.stmtTempMap[k] = v
		}
		c.block = elseBlock
		if c.shouldInstrument() {
			pos := s.Else.Pos()
			end := s.Else.End()
			idx := c.addCoverageRegion(pos.File, pos.Line, end.Line, c.currentCoverageFuncName(), "if.else")
			c.emitCoverageIncrement(idx)
		}
		c.genStmt(s.Else)
		if c.block.Term == nil {
			c.block.NewBr(mergeBlock)
		}
	}

	c.block = mergeBlock

	// B0198: Restore condition's string temps for merge-block cleanup.
	// The normal statement-end cleanupStmtTemps() will emit flag-guarded
	// cleanup IR here, covering the false-path where no branch ran.
	c.stmtTemps = savedCondStmtTemps
	c.stmtTempMap = savedCondStmtTempMap

	// B0173: Restore heap/env temps and clean up in the merge block.
	c.heapTemps = savedHeapTemps
	c.heapTempMap = savedHeapTempMap
	c.cleanupHeapTemps()
	c.envTemps = savedEnvTempsIf     // T0100
	c.envTempMap = savedEnvTempMapIf // T0100
	c.cleanupEnvTemps()
}

// genIfStmtValue generates an if/else statement in value-producing position
// (e.g., as the last statement in a block body of a match arm). Returns the
// phi of both branch values, or nil if the if/else cannot produce a value
// (no else, if-unwrap, optional narrowing, etc.). B0126.
func (c *Compiler) genIfStmtValue(s *ast.IfStmt) value.Value {
	// Only handle simple if/else — not if-unwrap, narrowing, or if without else.
	if s.Binding != "" || s.Else == nil {
		c.genIfStmt(s)
		return nil
	}
	if c.info.OptionalNarrowings[s] != nil || c.info.IsDestructureNarrowings[s] != nil {
		c.genIfStmt(s)
		return nil
	}

	cond := c.genExpr(s.Cond)

	thenBlock := c.newBlock("if.then")
	elseBlock := c.newBlock("if.else")
	mergeBlock := c.newBlock("if.end")

	c.block.NewCondBr(cond, thenBlock, elseBlock)

	// Then branch — capture value
	c.block = thenBlock
	thenVal := c.genBlockValue(s.Body)
	c.claimStringTemp(thenVal) // T0073
	thenEnd := c.block
	if c.block.Term == nil {
		c.block.NewBr(mergeBlock)
	}

	// Else branch — capture value
	c.block = elseBlock
	var elseVal value.Value
	switch e := s.Else.(type) {
	case *ast.Block:
		elseVal = c.genBlockValue(e)
	case *ast.IfStmt:
		elseVal = c.genIfStmtValue(e)
	default:
		c.genStmt(s.Else)
	}
	c.claimStringTemp(elseVal) // T0073
	elseEnd := c.block
	if c.block.Term == nil {
		c.block.NewBr(mergeBlock)
	}

	c.block = mergeBlock

	// Filter void-typed values — they cannot participate in phi nodes.
	if thenVal != nil {
		if _, isVoid := thenVal.Type().(*irtypes.VoidType); isVoid {
			thenVal = nil
		}
	}
	if elseVal != nil {
		if _, isVoid := elseVal.Type().(*irtypes.VoidType); isVoid {
			elseVal = nil
		}
	}

	// Build phi from branches that reach mergeBlock with values.
	// One branch may return/diverge, leaving only the other to contribute.
	var incomings []*ir.Incoming
	if thenVal != nil {
		if br, ok := thenEnd.Term.(*ir.TermBr); ok && br.Target == mergeBlock {
			incomings = append(incomings, &ir.Incoming{X: thenVal, Pred: thenEnd})
		}
	}
	if elseVal != nil {
		if br, ok := elseEnd.Term.(*ir.TermBr); ok && br.Target == mergeBlock {
			incomings = append(incomings, &ir.Incoming{X: elseVal, Pred: elseEnd})
		}
	}
	if len(incomings) > 0 {
		return mergeBlock.NewPhi(incomings...)
	}

	return nil
}

// genIfNarrowStmt handles if-statements that narrow optional variables.
// Supports single narrowing, compound narrowing (&&), and negated narrowing (!cc).
func (c *Compiler) genIfNarrowStmt(s *ast.IfStmt, narrow *sema.OptionalNarrowing) {
	if narrow.Negated {
		c.genNegatedNarrowStmt(s, narrow)
		return
	}
	if len(narrow.Vars) > 1 {
		c.genCompoundNarrowStmt(s, narrow)
		return
	}

	// Single variable narrowing
	v := narrow.Vars[0]
	alloca := c.locals[v.VarName]
	optVal := c.block.NewLoad(alloca.ElemType, alloca)
	flag := c.block.NewExtractValue(optVal, 0)

	thenBlock := c.newBlock("narrow.then")
	mergeBlock := c.newBlock("narrow.end")

	var elseBlock *ir.Block
	if s.Else != nil {
		elseBlock = c.newBlock("narrow.else")
		c.block.NewCondBr(flag, thenBlock, elseBlock)
	} else {
		c.block.NewCondBr(flag, thenBlock, mergeBlock)
	}

	// Then: shadow the variable with the unwrapped inner value
	c.block = thenBlock
	innerVal := c.block.NewExtractValue(optVal, 1)
	innerAlloca := c.createEntryAlloca(innerVal.Type()) // B0153: must be in entry block
	c.block.NewStore(innerVal, innerAlloca)
	prev := c.locals[v.VarName]
	c.locals[v.VarName] = innerAlloca

	c.genBlock(s.Body)
	if c.block.Term == nil {
		c.block.NewBr(mergeBlock)
	}
	c.locals[v.VarName] = prev

	if s.Else != nil {
		c.block = elseBlock
		c.genStmt(s.Else)
		if c.block.Term == nil {
			c.block.NewBr(mergeBlock)
		}
	}

	c.block = mergeBlock
}

// genNegatedNarrowStmt handles `if !cc { A } else { B }` — narrowing in else branch.
func (c *Compiler) genNegatedNarrowStmt(s *ast.IfStmt, narrow *sema.OptionalNarrowing) {
	v := narrow.Vars[0]
	alloca := c.locals[v.VarName]
	optVal := c.block.NewLoad(alloca.ElemType, alloca)
	flag := c.block.NewExtractValue(optVal, 0)

	thenBlock := c.newBlock("narrow.then")
	mergeBlock := c.newBlock("narrow.end")

	var elseBlock *ir.Block
	if s.Else != nil {
		elseBlock = c.newBlock("narrow.else")
	}

	// flag=true (present) → else (narrowed), flag=false (absent) → then (not narrowed)
	if s.Else != nil {
		c.block.NewCondBr(flag, elseBlock, thenBlock)
	} else {
		c.block.NewCondBr(flag, mergeBlock, thenBlock)
	}

	// Then: cc is none — no narrowing
	c.block = thenBlock
	c.genBlock(s.Body)
	if c.block.Term == nil {
		c.block.NewBr(mergeBlock)
	}

	// Else: cc is present — shadow with unwrapped value
	if s.Else != nil {
		c.block = elseBlock
		innerVal := c.block.NewExtractValue(optVal, 1)
		innerAlloca := c.createEntryAlloca(innerVal.Type()) // B0153: must be in entry block
		c.block.NewStore(innerVal, innerAlloca)
		prev := c.locals[v.VarName]
		c.locals[v.VarName] = innerAlloca

		c.genStmt(s.Else)
		if c.block.Term == nil {
			c.block.NewBr(mergeBlock)
		}
		c.locals[v.VarName] = prev
	}

	c.block = mergeBlock

	// Post-divergence narrowing: if the then-body diverges and there's no else,
	// we know the variable is present at the merge point. Shadow it with the
	// unwrapped inner value for all subsequent code.
	if narrow.PostNarrow {
		innerVal := c.block.NewExtractValue(optVal, 1)
		innerAlloca := c.createEntryAlloca(innerVal.Type()) // B0153: must be in entry block
		c.block.NewStore(innerVal, innerAlloca)
		c.locals[v.VarName] = innerAlloca
	}
}

// genCompoundNarrowStmt handles `if a && b { ... }` — both narrowed in then-block.
// Generates nested flag checks with short-circuit evaluation.
func (c *Compiler) genCompoundNarrowStmt(s *ast.IfStmt, narrow *sema.OptionalNarrowing) {
	mergeBlock := c.newBlock("narrow.end")
	var elseBlock *ir.Block
	if s.Else != nil {
		elseBlock = c.newBlock("narrow.else")
	}

	// Load all optional values and chain flag checks
	type optInfo struct {
		optVal value.Value
		v      sema.NarrowedVar
	}
	opts := make([]optInfo, len(narrow.Vars))
	for i, v := range narrow.Vars {
		alloca := c.locals[v.VarName]
		optVal := c.block.NewLoad(alloca.ElemType, alloca)
		flag := c.block.NewExtractValue(optVal, 0)
		opts[i] = optInfo{optVal: optVal, v: v}

		if i < len(narrow.Vars)-1 {
			// Not the last: chain to next check
			nextCheck := c.newBlock(fmt.Sprintf("narrow.check.%d", i+1))
			failTarget := elseBlock
			if failTarget == nil {
				failTarget = mergeBlock
			}
			c.block.NewCondBr(flag, nextCheck, failTarget)
			c.block = nextCheck
		} else {
			// Last: branch to then or else/merge
			thenBlock := c.newBlock("narrow.then")
			failTarget := elseBlock
			if failTarget == nil {
				failTarget = mergeBlock
			}
			c.block.NewCondBr(flag, thenBlock, failTarget)
			c.block = thenBlock
		}
	}

	// Then: shadow all variables with unwrapped values
	prevLocals := make(map[string]*ir.InstAlloca, len(opts))
	for _, info := range opts {
		innerVal := c.block.NewExtractValue(info.optVal, 1)
		innerAlloca := c.createEntryAlloca(innerVal.Type()) // B0153: must be in entry block
		c.block.NewStore(innerVal, innerAlloca)
		prevLocals[info.v.VarName] = c.locals[info.v.VarName]
		c.locals[info.v.VarName] = innerAlloca
	}

	c.genBlock(s.Body)
	if c.block.Term == nil {
		c.block.NewBr(mergeBlock)
	}

	// Restore all
	for name, prev := range prevLocals {
		c.locals[name] = prev
	}

	if s.Else != nil {
		c.block = elseBlock
		c.genStmt(s.Else)
		if c.block.Term == nil {
			c.block.NewBr(mergeBlock)
		}
	}

	c.block = mergeBlock
}

// genIfDestructureIsStmt handles if-statements with destructure is-patterns.
// Generates a type/variant check, then extracts fields into bindings in the then-block.
func (c *Compiler) genIfDestructureIsStmt(s *ast.IfStmt, narrow *sema.IsDestructureNarrowing) {
	subject := c.genExpr(narrow.SubjectExpr)

	// B0112: apply type substitution to TargetType for generic method bodies
	targetType := narrow.TargetType
	if c.typeSubst != nil {
		targetType = types.Substitute(targetType, c.typeSubst)
	}

	var cond value.Value
	if narrow.IsEnum {
		// Enum variant check: compare tag
		enumLayout := c.lookupEnumLayout(targetType)
		if enumLayout == nil {
			panic(fmt.Sprintf("codegen: no enum layout for %s", targetType))
		}
		// A `this` enum receiver is an i8* pointer — load the value so both the tag
		// check below and the field binding in bindIsDestructureEnum operate on the
		// by-value enum. (Non-enum/RTTI branch keeps the raw i8* `this`.)
		subject = c.enumThisSubject(subject, enumLayout)
		var tag value.Value
		if enumLayout.MaxVariantDataSize == 0 {
			tag = subject // fieldless enum: value IS the tag
		} else {
			tag = c.block.NewExtractValue(subject, 0)
		}
		expectedTag := constant.NewInt(irtypes.I32, int64(enumLayout.VariantTag[narrow.VariantName]))
		cond = c.block.NewICmp(enum.IPredEQ, tag, expectedTag)
	} else {
		// Named/Instance type check via RTTI
		targetID, ok := c.resolveTypeID(targetType)
		if !ok {
			targetNamed := extractNamed(targetType)
			if targetNamed == nil {
				panic(fmt.Sprintf("codegen: cannot extract Named from %s", targetType))
			}
			targetID = c.assignTypeID(targetNamed)
		}
		// For value types, use the compile-time-known RTTI global (no field in value struct).
		subjectType := c.info.Types[narrow.SubjectExpr]
		if c.typeSubst != nil {
			subjectType = types.Substitute(subjectType, c.typeSubst)
		}
		instance := c.instancePtrForRTTI(subject, subjectType)
		variantPtr := c.loadVariantPtr(instance)
		result := c.block.NewCall(c.funcs["promise_type_is"],
			variantPtr, constant.NewInt(irtypes.I32, int64(targetID)))
		cond = c.block.NewICmp(enum.IPredNE, result, constant.NewInt(irtypes.I32, 0))
	}

	thenBlock := c.newBlock("isdestr.then")
	mergeBlock := c.newBlock("isdestr.end")

	var elseBlock *ir.Block
	if s.Else != nil {
		elseBlock = c.newBlock("isdestr.else")
		c.block.NewCondBr(cond, thenBlock, elseBlock)
	} else {
		c.block.NewCondBr(cond, thenBlock, mergeBlock)
	}

	// Then: extract fields and bind them
	c.block = thenBlock

	// Save previous locals that might be shadowed by bindings
	type savedLocal struct {
		name string
		val  *ir.InstAlloca
		had  bool
	}
	var saved []savedLocal
	for _, b := range narrow.Bindings {
		if b.VarName != "_" {
			prev, had := c.locals[b.VarName]
			saved = append(saved, savedLocal{b.VarName, prev, had})
		}
	}

	if narrow.IsEnum {
		c.bindIsDestructureEnum(subject, narrow)
	} else {
		c.bindIsDestructureNamed(subject, narrow)
	}

	c.genBlock(s.Body)

	// Restore previous locals
	for _, s := range saved {
		if s.had {
			c.locals[s.name] = s.val
		} else {
			delete(c.locals, s.name)
		}
	}

	if c.block.Term == nil {
		c.block.NewBr(mergeBlock)
	}

	// Else branch
	if s.Else != nil {
		c.block = elseBlock
		c.genStmt(s.Else)
		if c.block.Term == nil {
			c.block.NewBr(mergeBlock)
		}
	}

	c.block = mergeBlock
}

// bindIsDestructureEnum extracts enum variant data fields and binds them to local variables.
func (c *Compiler) bindIsDestructureEnum(subject value.Value, narrow *sema.IsDestructureNarrowing) {
	// B0112: apply type substitution for generic method bodies
	targetType := narrow.TargetType
	if c.typeSubst != nil {
		targetType = types.Substitute(targetType, c.typeSubst)
	}
	enumLayout := c.lookupEnumLayout(targetType)
	dataType := enumLayout.VariantDataTypes[narrow.VariantName]
	if dataType == nil {
		return
	}

	internalType := enumLayout.EnumInternalType.(*irtypes.StructType)
	alloca := c.createEntryAlloca(internalType)
	c.block.NewStore(subject, alloca)

	dataPtr := c.block.NewGetElementPtr(internalType, alloca,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 1))
	typedDataPtr := c.block.NewBitCast(dataPtr, irtypes.NewPointer(dataType))

	for i, b := range narrow.Bindings {
		if b.VarName == "_" {
			continue
		}
		if i >= len(dataType.Fields) {
			break
		}
		fieldType := dataType.Fields[i]
		fieldPtr := c.block.NewGetElementPtr(dataType, typedDataPtr,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(i)))
		val := c.block.NewLoad(fieldType, fieldPtr)

		bindAlloca := c.createEntryAlloca(fieldType)
		c.block.NewStore(val, bindAlloca)
		c.locals[b.VarName] = bindAlloca
	}
}

// bindIsDestructureNamed extracts named type fields and binds them to local variables.
func (c *Compiler) bindIsDestructureNamed(subject value.Value, narrow *sema.IsDestructureNarrowing) {
	// B0112: apply type substitution for generic method bodies
	targetType := narrow.TargetType
	if c.typeSubst != nil {
		targetType = types.Substitute(targetType, c.typeSubst)
	}
	targetNamed := extractNamed(targetType)
	layout := c.lookupTypeLayout(targetType)
	if layout == nil {
		panic(fmt.Sprintf("codegen: no layout for type %s", targetType))
	}

	// For heap types, extract instance pointer once before the loop.
	// Value types don't use instance pointers — fields are in the value struct.
	var instancePtr value.Value
	if !layout.IsValueType {
		instancePtr = c.extractInstancePtr(subject)
	}

	allFields := targetNamed.AllFields()
	for i, b := range narrow.Bindings {
		if b.VarName == "_" {
			continue
		}
		if i >= len(allFields) {
			break
		}
		field := allFields[i]

		if layout.IsValueType {
			// Value type: fields are in value struct
			fieldIdx, ok := layout.ValueFieldIndex[field.Name()]
			if !ok {
				continue
			}
			// Extract field directly from the subject value struct
			fieldVal := c.block.NewExtractValue(subject, uint64(fieldIdx))
			bindAlloca := c.createEntryAlloca(fieldVal.Type())
			c.block.NewStore(fieldVal, bindAlloca)
			c.locals[b.VarName] = bindAlloca
		} else {
			// Heap type: fields in instance struct
			fieldIdx, ok := layout.InstanceFieldIndex[field.Name()]
			if !ok {
				continue
			}
			typedPtr := c.block.NewBitCast(instancePtr, layout.InstancePtrType)
			fieldPtr := c.block.NewGetElementPtr(layout.Instance.LLVMType, typedPtr,
				constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(fieldIdx)))
			fieldVal := c.block.NewLoad(layout.Instance.Fields[fieldIdx].LLVMType, fieldPtr)

			bindAlloca := c.createEntryAlloca(fieldVal.Type())
			c.block.NewStore(fieldVal, bindAlloca)
			c.locals[b.VarName] = bindAlloca
		}
	}
}

// genIfUnwrapStmt handles if-unwrap: if val := optExpr { } else { }
// Evaluates the optional, checks the present flag, binds the unwrapped value in the then block.
func (c *Compiler) genIfUnwrapStmt(s *ast.IfStmt) {
	// T0397: When unwrapping a Map[K, (droppable, ...)] index, the inner tuple
	// aliases the container's bucket data. Setting dupTupleFieldAccess here
	// causes genMethodIndex to deep-clone the tuple so the binding takes
	// ownership of an independent copy.
	dupInitType := c.info.Types[s.Init]
	if c.typeSubst != nil {
		dupInitType = types.Substitute(dupInitType, c.typeSubst)
	}
	if opt, ok := dupInitType.(*types.Optional); ok {
		elem := opt.Elem()
		if c.typeSubst != nil {
			elem = types.Substitute(elem, c.typeSubst)
		}
		if _, isTup := elem.(*types.Tuple); isTup && c.tupleNeedsDrop(elem) {
			c.dupTupleFieldAccess = true
		}
		// T0440: Same dup-on-read for Optional[heap-user-type] — the inner
		// value aliases the container's bucket; without dupping, the if-let
		// binding's drop would free the same instance the container drops.
		if isDroppableHeapUserType(elem) {
			c.dupHeapUserFieldAccess = true
		}
	}
	optVal := c.genExpr(s.Init)
	c.dupTupleFieldAccess = false
	c.dupHeapUserFieldAccess = false

	// T0770: When the scrutinee is a failable call (e.g. `if e := load()` where
	// `load!() T?`), auto-propagate the error first so optVal is the unwrapped
	// success value (the `T?` optional), not the raw failable result struct.
	// Without this the if-let reads the failable result's error flag as the
	// optional's present flag and binds the whole optional as the inner value.
	if c.info.AutoPropagateExprs[s.Init] {
		optVal = c.genAutoPropagateValue(optVal)
	}

	// Guard: if the expression is not an optional struct (e.g., post-narrowing
	// made it a plain value), treat the if as always-true with no unwrapping.
	// Bind the value directly to the unwrap variable name.
	if _, ok := optVal.Type().(*irtypes.StructType); !ok {
		if s.Binding != "" && s.Binding != "_" {
			alloca := c.createEntryAlloca(optVal.Type()) // B0153: must be in entry block
			c.block.NewStore(optVal, alloca)
			prev, had := c.locals[s.Binding]
			c.locals[s.Binding] = alloca
			c.genBlock(s.Body)
			if had {
				c.locals[s.Binding] = prev
			} else {
				delete(c.locals, s.Binding)
			}
		} else {
			c.genBlock(s.Body)
		}
		return
	}

	// Extract flag (field 0 of { i1, T } struct)
	flag := c.block.NewExtractValue(optVal, 0)

	thenBlock := c.newBlock("ifunwrap.then")
	mergeBlock := c.newBlock("ifunwrap.end")

	var elseBlock *ir.Block
	if s.Else != nil {
		elseBlock = c.newBlock("ifunwrap.else")
		c.block.NewCondBr(flag, thenBlock, elseBlock)
	} else {
		c.block.NewCondBr(flag, thenBlock, mergeBlock)
	}

	// B0173: Save heap/env temps from the init expression so branches don't
	// prematurely clean them. Cleanup runs once in the merge block.
	savedHeapTemps := c.heapTemps
	savedHeapTempMap := c.heapTempMap
	c.heapTemps = nil
	c.heapTempMap = make(map[value.Value]int)
	savedEnvTempsUW := c.envTemps     // T0100
	savedEnvTempMapUW := c.envTempMap // T0100
	c.envTemps = nil
	c.envTempMap = make(map[value.Value]int)

	// Then: unwrap value, bind to local (scoped to then-block only)
	c.block = thenBlock
	innerVal := c.block.NewExtractValue(optVal, 1)
	innerType := innerVal.Type()
	alloca := c.createEntryAlloca(innerType) // B0153: must be in entry block
	alloca.SetName(c.uniqueLocalName(s.Binding))
	c.block.NewStore(innerVal, alloca)
	prev, hadPrev := c.locals[s.Binding]
	c.locals[s.Binding] = alloca

	// B0215: Register drop binding for the unwrapped inner value when uniquely
	// owned. Function calls and force-unwraps return owned values; local
	// IdentExpr variables transfer ownership (we clear their drop flag).
	// Field access (MemberExpr) on droppable types is skipped — the parent
	// type's drop handles cleanup of the field's inner value.
	unwrapScopeLen := len(c.scopeBindings)
	savedDropFlag, hadDropFlag := c.dropFlags[s.Binding]
	savedDropBinding, hadDropBinding := c.dropBindings[s.Binding]
	// T0512: Snapshot the match-borrow marker for this binding so a marker
	// propagated through this if-let (below) is reverted at body end —
	// same lifetime as the drop flag/binding save/restore. Safe on nil map.
	savedBorrowMark, hadBorrowMark := c.matchBorrowedIdents[s.Binding]
	initType := c.info.Types[s.Init]
	if c.typeSubst != nil {
		initType = types.Substitute(initType, c.typeSubst)
	}
	if opt, ok := initType.(*types.Optional); ok && c.isOwnedOptionalExpr(s.Init) {
		// T0391: When unwrapping a nested Optional (T?? → T?), the element type is
		// itself Optional and needs an Optional drop binding so its inner heap value
		// is freed at scope exit (or transferred ownership to a further unwrap).
		elemType := opt.Elem()
		if c.typeSubst != nil {
			elemType = types.Substitute(elemType, c.typeSubst)
		}
		// T0585: For an IdentExpr source, load its drop flag value before
		// maybeRegister* / clearDropFlag so we can mirror the source's ownership
		// state into the binding. A borrowed source (no flag) means the unwrapped
		// binding is also a borrow (flag=0); without this, the binding would
		// incorrectly claim ownership and double-free the heap value at scope exit.
		var srcFlagVal value.Value
		if ident, isIdent := s.Init.(*ast.IdentExpr); isIdent {
			if srcFlag, has := c.dropFlags[ident.Name]; has {
				srcFlagVal = c.block.NewLoad(irtypes.I1, srcFlag)
			}
		}
		if innerOpt, ok := elemType.(*types.Optional); ok {
			c.maybeRegisterOptionalDrop(s.Binding, alloca, innerOpt)
		} else {
			c.maybeRegisterDrop(s.Binding, alloca, elemType)
		}
		// Only transfer ownership (clear source dropflag) if the unwrapped binding
		// actually got a drop registered. B0246: Structural interfaces don't get drops
		// via maybeRegisterDrop — the source must retain ownership so its Optional drop
		// (RTTI-based) handles cleanup on reassignment or scope exit.
		if _, innerHasDrop := c.dropBindings[s.Binding]; innerHasDrop {
			if ident, ok := s.Init.(*ast.IdentExpr); ok {
				// T0585: Propagate source's pre-clear drop flag into the binding's
				// drop flag only when the source had a flag. Source with no flag
				// is ambiguous at the callee — it could be a borrowed param or an
				// owned param that was auto-moved at the call site (Optional wrap
				// of a narrower arg). We can't distinguish without runtime info,
				// so leave the binding's flag as initialized (1 from
				// maybeRegisterDrop) and let the owned-via-wrap path drop the
				// value at scope exit.
				if srcFlagVal != nil {
					if bindingFlag, has := c.dropFlags[s.Binding]; has {
						c.block.NewStore(srcFlagVal, bindingFlag)
					}
				}
				c.clearDropFlag(ident.Name)
			}
		}
	}

	// T0512: A match-borrowed source means the unwrapped binding still
	// aliases variant-owned memory (the synth enum drop walks the full
	// nested Optional chain). Mark it borrowed so a further if-let/while-let
	// on this binding does not transfer ownership and double-free.
	if ident, isIdent := s.Init.(*ast.IdentExpr); isIdent &&
		c.matchBorrowedIdents != nil && c.matchBorrowedIdents[ident.Name] {
		c.matchBorrowedIdents[s.Binding] = true
	}

	c.genBlock(s.Body)

	// B0215: Emit drop for the unwrapped value on the fall-through path.
	// Return/break/raise paths handle this via emitScopeCleanup from their
	// respective base depths (which include this binding).
	if c.block != nil && c.block.Term == nil && len(c.scopeBindings) > unwrapScopeLen {
		cap := c.emitScopeCleanup(unwrapScopeLen, false)
		c.emitCloseErrCheck(cap)
	}
	c.scopeBindings = c.scopeBindings[:unwrapScopeLen]

	if c.block.Term == nil {
		c.block.NewBr(mergeBlock)
	}

	// Remove binding from scope (it's only visible in the then-block)
	if hadPrev {
		c.locals[s.Binding] = prev
	} else {
		delete(c.locals, s.Binding)
	}
	// B0215: Restore drop flag/binding state from before the if-let.
	if hadDropFlag {
		c.dropFlags[s.Binding] = savedDropFlag
	} else {
		delete(c.dropFlags, s.Binding)
	}
	if hadDropBinding {
		c.dropBindings[s.Binding] = savedDropBinding
	} else {
		delete(c.dropBindings, s.Binding)
	}
	// T0512: Revert the borrow marker propagated for this binding (scoped to
	// the if-let body, same lifetime as the drop flag/binding state above).
	if hadBorrowMark {
		c.matchBorrowedIdents[s.Binding] = savedBorrowMark
	} else if c.matchBorrowedIdents != nil {
		delete(c.matchBorrowedIdents, s.Binding)
	}

	// Else (optional)
	if s.Else != nil {
		c.block = elseBlock
		c.genStmt(s.Else)
		if c.block.Term == nil {
			c.block.NewBr(mergeBlock)
		}
	}

	c.block = mergeBlock

	// B0173: Restore heap/env temps and clean up in the merge block so both
	// then and else paths reach the cleanup (via their branches to mergeBlock).
	c.heapTemps = savedHeapTemps
	c.heapTempMap = savedHeapTempMap
	c.cleanupHeapTemps()
	c.envTemps = savedEnvTempsUW     // T0100
	c.envTempMap = savedEnvTempMapUW // T0100
	c.cleanupEnvTemps()
}

// --- While loop ---

func (c *Compiler) genWhileStmt(s *ast.WhileStmt) {
	headerBlock := c.newBlock("while.header")
	bodyBlock := c.newBlock("while.body")
	exitBlock := c.newBlock("while.exit")

	c.block.NewBr(headerBlock)

	// Header: evaluate condition
	c.block = headerBlock
	cond := c.genExpr(s.Cond)
	c.block.NewCondBr(cond, bodyBlock, exitBlock)

	// Body
	savedBreak := c.breakTarget
	savedContinue := c.continueTarget
	savedLoopUseDepth := c.loopScopeDepth
	c.breakTarget = exitBlock
	c.continueTarget = headerBlock
	c.loopScopeDepth = len(c.scopeBindings)

	c.block = bodyBlock
	if c.shouldInstrument() {
		pos := s.Body.Pos()
		end := s.Body.End()
		idx := c.addCoverageRegion(pos.File, pos.Line, end.Line, c.currentCoverageFuncName(), "while.body")
		c.emitCoverageIncrement(idx)
	}
	c.genBlock(s.Body)
	if c.block.Term == nil {
		c.block.NewBr(headerBlock)
	}

	c.breakTarget = savedBreak
	c.continueTarget = savedContinue
	c.loopScopeDepth = savedLoopUseDepth
	c.block = exitBlock
}

// genWhileUnwrapStmt handles while-unwrap: while val := optExpr { }
// Each iteration evaluates the optional; loop continues while present.
func (c *Compiler) genWhileUnwrapStmt(s *ast.WhileUnwrapStmt) {
	headerBlock := c.newBlock("whileunwrap.header")
	bodyBlock := c.newBlock("whileunwrap.body")
	exitBlock := c.newBlock("whileunwrap.exit")

	c.block.NewBr(headerBlock)

	// Header: evaluate optional, check flag
	c.block = headerBlock
	// T0397: Same dup-on-read pattern as genIfUnwrapStmt — when iterating
	// over `Map[K, (droppable,...)]` indices, the inner tuple aliases bucket
	// data; without dupping, the binding's per-iteration drop would free the
	// map's storage.
	dupValType := c.info.Types[s.Value]
	if c.typeSubst != nil {
		dupValType = types.Substitute(dupValType, c.typeSubst)
	}
	if opt, ok := dupValType.(*types.Optional); ok {
		elem := opt.Elem()
		if c.typeSubst != nil {
			elem = types.Substitute(elem, c.typeSubst)
		}
		if _, isTup := elem.(*types.Tuple); isTup && c.tupleNeedsDrop(elem) {
			c.dupTupleFieldAccess = true
		}
		// T0440: Same dup-on-read for Optional[heap-user-type] in while-let.
		if isDroppableHeapUserType(elem) {
			c.dupHeapUserFieldAccess = true
		}
	}
	optVal := c.genExpr(s.Value)
	c.dupTupleFieldAccess = false
	c.dupHeapUserFieldAccess = false
	// T0770: auto-propagate a failable scrutinee so optVal is the unwrapped `T?`
	// optional, not the raw failable result struct (mirrors genIfUnwrapStmt).
	if c.info.AutoPropagateExprs[s.Value] {
		optVal = c.genAutoPropagateValue(optVal)
	}
	flag := c.block.NewExtractValue(optVal, 0)
	c.block.NewCondBr(flag, bodyBlock, exitBlock)

	// Body: unwrap value, bind to local
	savedBreak := c.breakTarget
	savedContinue := c.continueTarget
	savedLoopUseDepth := c.loopScopeDepth
	c.breakTarget = exitBlock
	c.continueTarget = headerBlock
	c.loopScopeDepth = len(c.scopeBindings)

	c.block = bodyBlock
	innerVal := c.block.NewExtractValue(optVal, 1)
	innerType := innerVal.Type()
	alloca := c.createEntryAlloca(innerType) // B0153: must be in entry block
	alloca.SetName(c.uniqueLocalName(s.Binding))
	c.block.NewStore(innerVal, alloca)
	prev, hadPrev := c.locals[s.Binding]
	c.locals[s.Binding] = alloca

	// B0215: Register drop binding for the unwrapped inner value. Each iteration
	// gets a new value; the drop flag is set to 1 in the body block (via
	// maybeRegisterDrop's store in c.block) so it resets correctly per iteration.
	// The binding is above loopScopeDepth, so break/continue paths include it.
	unwrapScopeLen := len(c.scopeBindings)
	savedDropFlag, hadDropFlag := c.dropFlags[s.Binding]
	savedDropBinding, hadDropBinding := c.dropBindings[s.Binding]
	// T0512: Snapshot the match-borrow marker (see genIfUnwrapStmt). Reverted
	// at body end so the marker does not leak past the loop. Safe on nil map.
	savedBorrowMark, hadBorrowMark := c.matchBorrowedIdents[s.Binding]
	valType := c.info.Types[s.Value]
	if c.typeSubst != nil {
		valType = types.Substitute(valType, c.typeSubst)
	}
	if opt, ok := valType.(*types.Optional); ok && c.isOwnedOptionalExpr(s.Value) {
		// T0391: When unwrapping a nested Optional (T?? → T?), the element type is
		// itself Optional and needs an Optional drop binding so its inner heap value
		// is freed at scope exit (or transferred ownership to a further unwrap).
		elemType := opt.Elem()
		if c.typeSubst != nil {
			elemType = types.Substitute(elemType, c.typeSubst)
		}
		// T0585: Load source's drop flag value before maybeRegister* / clearDropFlag
		// so we can mirror the source's ownership state into the binding.
		var srcFlagVal value.Value
		if ident, isIdent := s.Value.(*ast.IdentExpr); isIdent {
			if srcFlag, has := c.dropFlags[ident.Name]; has {
				srcFlagVal = c.block.NewLoad(irtypes.I1, srcFlag)
			}
		}
		if innerOpt, ok := elemType.(*types.Optional); ok {
			c.maybeRegisterOptionalDrop(s.Binding, alloca, innerOpt)
		} else {
			c.maybeRegisterDrop(s.Binding, alloca, elemType)
		}
		// Only transfer ownership if the unwrapped binding got a drop registered.
		// B0246: Structural interfaces don't get drops via maybeRegisterDrop — the
		// source must retain ownership for its Optional drop (RTTI-based) to handle cleanup.
		if _, innerHasDrop := c.dropBindings[s.Binding]; innerHasDrop {
			if ident, ok := s.Value.(*ast.IdentExpr); ok {
				// T0585: Propagate source's pre-clear drop flag into the binding's
				// drop flag only when the source had a flag. See genIfUnwrapStmt
				// for the rationale — no-flag source is ambiguous at the callee.
				if srcFlagVal != nil {
					if bindingFlag, has := c.dropFlags[s.Binding]; has {
						c.block.NewStore(srcFlagVal, bindingFlag)
					}
				}
				c.clearDropFlag(ident.Name)
			}
		}
	}

	// T0512: A match-borrowed source means the unwrapped binding still
	// aliases variant-owned memory; mark it borrowed so a further
	// if-let/while-let on this binding does not transfer ownership.
	if ident, isIdent := s.Value.(*ast.IdentExpr); isIdent &&
		c.matchBorrowedIdents != nil && c.matchBorrowedIdents[ident.Name] {
		c.matchBorrowedIdents[s.Binding] = true
	}

	c.genBlock(s.Body)

	// B0215: Emit drop for the unwrapped value at iteration end (fall-through).
	if c.block != nil && c.block.Term == nil && len(c.scopeBindings) > unwrapScopeLen {
		cap := c.emitScopeCleanup(unwrapScopeLen, false)
		c.emitCloseErrCheck(cap)
	}
	c.scopeBindings = c.scopeBindings[:unwrapScopeLen]

	if c.block.Term == nil {
		c.block.NewBr(headerBlock)
	}

	// Remove binding from scope (it's only visible in the loop body)
	if hadPrev {
		c.locals[s.Binding] = prev
	} else {
		delete(c.locals, s.Binding)
	}
	// B0215: Restore drop flag/binding state.
	if hadDropFlag {
		c.dropFlags[s.Binding] = savedDropFlag
	} else {
		delete(c.dropFlags, s.Binding)
	}
	if hadDropBinding {
		c.dropBindings[s.Binding] = savedDropBinding
	} else {
		delete(c.dropBindings, s.Binding)
	}
	// T0512: Revert the borrow marker propagated for this binding.
	if hadBorrowMark {
		c.matchBorrowedIdents[s.Binding] = savedBorrowMark
	} else if c.matchBorrowedIdents != nil {
		delete(c.matchBorrowedIdents, s.Binding)
	}

	c.breakTarget = savedBreak
	c.continueTarget = savedContinue
	c.loopScopeDepth = savedLoopUseDepth
	c.block = exitBlock
}

// --- For-in loop ---

func (c *Compiler) genForInStmt(s *ast.ForInStmt) {
	iterableType := c.info.Types[s.Iterable]
	if c.typeSubst != nil {
		iterableType = types.Substitute(iterableType, c.typeSubst)
	}

	if arr, ok := iterableType.(*types.Array); ok {
		c.genForInArray(s, arr)
	} else if elem, ok := types.AsVector(iterableType); ok {
		slicePtr := c.genExpr(s.Iterable)
		// T0109: Register a scope binding for temporary vectors returned by call
		// expressions (e.g., for elem in set.to_vector()). Variable-backed vectors
		// are dropped by their own scope bindings; only call results are orphaned.
		// Using a scope binding ensures cleanup on ALL exit paths (normal exit,
		// early return, break, panic) — not just after the loop.
		// T0494: extended from CallExpr-only to any tracked stmt temp so getter
		// MemberExpr results (e.g., `for k,v in resp.headers`) also survive the
		// for-in's lifetime. stmtTemps are NOT saved across block entry (unlike
		// heapTemps), so without this promotion the temp would be dropped by the
		// first body statement's cleanupStmtTemps and the loop would read freed
		// memory.
		if idx, isTracked := c.stmtTempMap[slicePtr]; isTracked && idx >= 0 {
			if dropFn, ok := c.funcs["Vector.drop"]; ok {
				tmpName := c.uniqueLocalName("__forin_vec_tmp")
				tmpAlloca := c.createEntryAlloca(irtypes.I8Ptr)
				tmpAlloca.SetName(tmpName)
				c.block.NewStore(slicePtr, tmpAlloca)
				dropFlag := c.createEntryAlloca(irtypes.I1)
				dropFlag.SetName(tmpName + ".dropflag")
				c.block.NewStore(constant.NewInt(irtypes.I1, 1), dropFlag)
				c.scopeBindings = append(c.scopeBindings, scopeBinding{
					kind:     bindingDropString, // reuse: same i8* alloca + void(i8*) drop pattern
					alloca:   tmpAlloca,
					named:    types.TypVector,
					valType:  iterableType,
					dropFlag: dropFlag,
					dropFunc: dropFn,
					varName:  tmpName,
				})
				// Claim the stmtTemp so it's not also dropped at statement end —
				// ownership transferred to the scope binding (prevents double-free).
				c.claimStringTemp(slicePtr)
			}
		}
		c.genForInVector(s, slicePtr, elem)
	} else if key, val, ok := types.AsMap(iterableType); ok {
		mapPtr := c.genExpr(s.Iterable)
		c.genForInMap(s, mapPtr, key, val)
	} else if elem, ok := types.AsChannel(iterableType); ok {
		chPtr := c.genExpr(s.Iterable)
		// T0502: Same lifetime-extension fix as the vector/string for-in
		// branches. When the iterable is a tracked stmt temp (getter result,
		// call result), promote it to a scope binding so the body's
		// cleanupStmtTemps doesn't free the channel mid-loop and the channel
		// is reliably dropped on all exit paths.
		if idx, isTracked := c.stmtTempMap[chPtr]; isTracked && idx >= 0 {
			resolvedChanElem := elem
			if c.typeSubst != nil && resolvedChanElem != nil {
				resolvedChanElem = types.Substitute(resolvedChanElem, c.typeSubst)
			}
			if resolvedChanElem != nil {
				// T0663: per-element-type drop walks any un-received buffered items.
				dropFn := c.getOrCreateChannelDrop(resolvedChanElem)
				tmpName := c.uniqueLocalName("__forin_ch_tmp")
				tmpAlloca := c.createEntryAlloca(irtypes.I8Ptr)
				tmpAlloca.SetName(tmpName)
				c.block.NewStore(chPtr, tmpAlloca)
				dropFlag := c.createEntryAlloca(irtypes.I1)
				dropFlag.SetName(tmpName + ".dropflag")
				c.block.NewStore(constant.NewInt(irtypes.I1, 1), dropFlag)
				c.scopeBindings = append(c.scopeBindings, scopeBinding{
					kind:     bindingDropString, // reuse: same i8* alloca + void(i8*) drop pattern
					alloca:   tmpAlloca,
					named:    types.TypChannel,
					valType:  iterableType,
					dropFlag: dropFlag,
					dropFunc: dropFn,
					varName:  tmpName,
				})
				c.claimStringTemp(chPtr)
			}
		}
		c.genForInChannel(s, chPtr, elem)
	} else if elem, ok := types.AsStream(iterableType); ok {
		genVal := c.genExpr(s.Iterable)
		// T0284: Failable generator factory called without explicit error handling.
		// Unwrap the result struct before passing to genForInGenerator.
		if c.info.FailableExprs[s.Iterable] {
			genVal = c.unwrapFailableGeneratorResult(genVal, s.Pos())
		}
		// T0088: Generators have their own cleanup (bindingGenerator). Clear all
		// pending heap temps to prevent __promise_iter_cleanup from running on
		// generator instances (which have a different layout than _FnIter).
		for i := range c.heapTemps {
			c.block.NewStore(constant.NewInt(irtypes.I1, 0), c.heapTemps[i].dropFlag)
		}
		c.genForInGenerator(s, genVal, elem)
	} else if elem, ok := types.AsRange(iterableType); ok {
		c.genForInRange(s, elem)
	} else {
		// String iteration
		named := extractNamed(iterableType)
		if named == types.TypString {
			strPtr := c.genExpr(s.Iterable)
			// T0494: Same lifetime-extension fix as the vector path. When the
			// iterable is a tracked stmt temp (call result, getter result,
			// string concat result, etc.), promote it to a scope binding so
			// the body's stmt-end cleanup doesn't free the string mid-loop.
			if idx, isTracked := c.stmtTempMap[strPtr]; isTracked && idx >= 0 {
				if dropFn, ok := c.funcs["promise_string_drop"]; ok {
					tmpName := c.uniqueLocalName("__forin_str_tmp")
					tmpAlloca := c.createEntryAlloca(irtypes.I8Ptr)
					tmpAlloca.SetName(tmpName)
					c.block.NewStore(strPtr, tmpAlloca)
					dropFlag := c.createEntryAlloca(irtypes.I1)
					dropFlag.SetName(tmpName + ".dropflag")
					c.block.NewStore(constant.NewInt(irtypes.I1, 1), dropFlag)
					c.scopeBindings = append(c.scopeBindings, scopeBinding{
						kind:     bindingDropString,
						alloca:   tmpAlloca,
						named:    types.TypString,
						valType:  iterableType,
						dropFlag: dropFlag,
						dropFunc: dropFn,
						varName:  tmpName,
					})
					c.claimStringTemp(strPtr)
				}
			}
			c.genForInString(s, strPtr)
			return
		}
		// Duck-typed for-in: check sema ForInKinds
		if kind, ok := c.info.ForInKinds[s]; ok {
			iterVal := c.genExpr(s.Iterable)
			switch kind {
			case sema.ForInNext:
				c.genForInCustomIter(s, iterVal, iterableType)
			case sema.ForInIter:
				c.genForInCustomStream(s, iterVal, iterableType)
			}
			return
		}
		panic(fmt.Sprintf("codegen: unsupported for-in iterable type %s", iterableType))
	}
}

// genForInCustomIter handles for-in over any type with a next() T? method.
// Calls .next() in a loop via virtual dispatch (structural interface) or direct call (concrete type).
func (c *Compiler) genForInCustomIter(s *ast.ForInStmt, iterVal value.Value, iterType types.Type) {
	// Resolve element type from the next() return type
	named := extractNamed(iterType)
	if named == nil {
		panic(fmt.Sprintf("codegen: genForInCustomIter on non-named type %s", iterType))
	}
	nextMethod := named.LookupMethod("next")
	if nextMethod == nil {
		panic(fmt.Sprintf("codegen: type %s has no next() method", named))
	}

	// Resolve the optional return type: next() returns T?
	retType := nextMethod.Sig().Result()
	if inst, ok := iterType.(*types.Instance); ok {
		if origin, ok := inst.Origin().(*types.Named); ok && len(origin.TypeParams()) > 0 {
			subst := types.BuildSubstMap(origin.TypeParams(), inst.TypeArgs())
			retType = types.Substitute(retType, subst)
		}
	}
	if c.typeSubst != nil {
		retType = types.Substitute(retType, c.typeSubst)
	}
	optType, ok := retType.(*types.Optional)
	if !ok {
		panic(fmt.Sprintf("codegen: next() on %s does not return optional", named))
	}
	elemType := optType.Elem()
	elemLLVM := c.resolveType(elemType)
	optLLVM := c.resolveType(retType)

	// Store iterable value in alloca for repeated .next() calls
	iterAlloca := c.createEntryAlloca(iterVal.Type())
	iterAlloca.SetName(c.uniqueLocalName("iter.val"))
	c.block.NewStore(iterVal, iterAlloca)

	// Element binding
	elemAlloca := c.createEntryAlloca(elemLLVM)
	elemAlloca.SetName(c.uniqueLocalName(s.Binding))
	if s.Binding != "_" {
		c.locals[s.Binding] = elemAlloca
	}

	// Optional index variable
	if s.Index != "" && s.Index != "_" {
		indexAlloca := c.createEntryAlloca(irtypes.I64)
		indexAlloca.SetName(c.uniqueLocalName(s.Index))
		c.block.NewStore(constant.NewInt(irtypes.I64, 0), indexAlloca)
		c.locals[s.Index] = indexAlloca
	}

	headerBlk := c.newBlock("iter.header")
	bodyBlk := c.newBlock("iter.body")
	updateBlk := c.newBlock("iter.update")
	exitBlk := c.newBlock("iter.exit")

	c.block.NewBr(headerBlk)

	// Header: call .next(), check optional
	c.block = headerBlk
	curIter := c.block.NewLoad(iterVal.Type(), iterAlloca)
	nextResult := c.emitIterNext(curIter, iterType, named, nextMethod, optLLVM)

	// Check optional discriminant: field 0 is i1 (true=some, false=none)
	tag := c.block.NewExtractValue(nextResult, 0)
	isNone := c.block.NewICmp(enum.IPredEQ, tag, constant.NewInt(irtypes.I1, 0))
	c.block.NewCondBr(isNone, exitBlk, bodyBlk)

	// Body: extract value, bind, execute
	savedBreak := c.breakTarget
	savedContinue := c.continueTarget
	savedLoopScopeDepth := c.loopScopeDepth
	c.breakTarget = exitBlk
	c.continueTarget = updateBlk
	c.loopScopeDepth = len(c.scopeBindings)

	c.block = bodyBlk
	val := c.block.NewExtractValue(nextResult, 1)
	c.block.NewStore(val, elemAlloca)

	c.genBlock(s.Body)

	if c.block.Term == nil {
		c.block.NewBr(updateBlk)
	}

	// Update: increment index, branch back to header
	c.block = updateBlk
	if s.Index != "" && s.Index != "_" {
		idxAlloca := c.locals[s.Index]
		curIdx := c.block.NewLoad(irtypes.I64, idxAlloca)
		nextIdx := c.block.NewAdd(curIdx, constant.NewInt(irtypes.I64, 1))
		c.block.NewStore(nextIdx, idxAlloca)
	}
	c.block.NewBr(headerBlk)

	c.breakTarget = savedBreak
	c.continueTarget = savedContinue
	c.loopScopeDepth = savedLoopScopeDepth

	c.block = exitBlk
}

// genForInCustomStream handles for-in over any type with an iter() method.
// Calls .iter() to get an iterator, then delegates to genForInCustomIter.
func (c *Compiler) genForInCustomStream(s *ast.ForInStmt, streamVal value.Value, streamType types.Type) {
	named := extractNamed(streamType)
	if named == nil {
		panic(fmt.Sprintf("codegen: genForInCustomStream on non-named type %s", streamType))
	}
	iterMethod := named.LookupMethod("iter")
	if iterMethod == nil {
		panic(fmt.Sprintf("codegen: type %s has no iter() method", named))
	}

	// Resolve iter() return type
	iterRetType := iterMethod.Sig().Result()
	if inst, ok := streamType.(*types.Instance); ok {
		if origin, ok := inst.Origin().(*types.Named); ok && len(origin.TypeParams()) > 0 {
			subst := types.BuildSubstMap(origin.TypeParams(), inst.TypeArgs())
			iterRetType = types.Substitute(iterRetType, subst)
		}
	}
	if c.typeSubst != nil {
		iterRetType = types.Substitute(iterRetType, c.typeSubst)
	}

	// Call .iter() on the stream value
	iterResult := c.emitIterNext(streamVal, streamType, named, iterMethod, c.resolveType(iterRetType))

	// Delegate to genForInCustomIter with the iterator value
	c.genForInCustomIter(s, iterResult, iterRetType)

	// B0173: Clean up the iterator instance after the loop. The .iter() call
	// allocates a heap instance that is not tracked by statement-level cleanup
	// (synthetic call, no AST node for maybeTrackIterTemp).
	if c.block != nil && c.block.Term == nil {
		iterNamed := extractNamed(iterRetType)
		if iterNamed != nil && !iterNamed.IsValueType() {
			if _, ok := iterResult.Type().(*irtypes.StructType); ok {
				instancePtr := c.block.NewExtractValue(iterResult, 1)
				if iterNamed.IsStructural() && c.iterCleanup != nil {
					// Structural interface (e.g., Iterator[T]): use __promise_iter_cleanup
					// which handles _FnIter layout (frees env + instance).
					c.block.NewCall(c.iterCleanup, instancePtr)
				} else {
					// Concrete type (e.g., NumberIter): free the instance allocation.
					c.block.NewCall(c.palFree, instancePtr)
				}
			}
		}
	}
}

// emitIterNext emits a call to a method on a value, using virtual dispatch
// for types that need vtables (structural interfaces) or direct dispatch otherwise.
// This is a synthetic method call (no AST nodes) used by duck-typed for-in iteration.
func (c *Compiler) emitIterNext(receiverVal value.Value, receiverType types.Type,
	named *types.Named, method *types.Method, retLLVM irtypes.Type) value.Value {

	if c.needsVtable(named) && !method.IsNative() {
		// Virtual dispatch: extract vtable + instance, call through vtable slot
		vtableRaw := c.extractVtablePtr(receiverVal)
		instance := c.extractInstancePtr(receiverVal)

		slotIndex := named.VirtualMethodIndex(method.Name(), false)
		if slotIndex < 0 {
			panic(fmt.Sprintf("codegen: method %s not in vtable for %s", method.Name(), named))
		}
		vtablePtr := c.block.NewBitCast(vtableRaw, irtypes.NewPointer(irtypes.I8Ptr))
		fnSlotPtr := c.block.NewGetElementPtr(irtypes.I8Ptr, vtablePtr,
			constant.NewInt(irtypes.I32, int64(slotIndex)))
		fnRaw := c.block.NewLoad(irtypes.I8Ptr, fnSlotPtr)

		// Build function type: (i8*) -> retLLVM  (receiver only, no other args)
		funcType := irtypes.NewFunc(retLLVM, irtypes.I8Ptr)
		fnTyped := c.block.NewBitCast(fnRaw, irtypes.NewPointer(funcType))

		return c.block.NewCall(fnTyped, instance)
	}

	// Direct dispatch: call the concrete method function
	ownerName := c.resolveTypeName(receiverType)
	mangledName := mangleMethodName(ownerName, method.Name(), false)
	fn, ok := c.funcs[mangledName]
	if !ok {
		panic(fmt.Sprintf("codegen: undeclared method %s", mangledName))
	}

	// Extract instance pointer as receiver
	instance := c.extractInstancePtr(receiverVal)
	return c.block.NewCall(fn, instance)
}

// genForInRange handles for-in over a Range[T] value type (e.g., 0..10, 'a'..'z').
// Extracts start/end/inclusive from the value type struct and uses a direct counter loop.
func (c *Compiler) genForInRange(s *ast.ForInStmt, elemType types.Type) {
	rangeVal := c.genExpr(s.Iterable)

	// Get the layout to find field indices
	iterableType := c.info.Types[s.Iterable]
	if c.typeSubst != nil {
		iterableType = types.Substitute(iterableType, c.typeSubst)
	}
	layout := c.lookupTypeLayout(iterableType)
	if layout == nil {
		panic(fmt.Sprintf("codegen: no layout for range type %s", iterableType))
	}

	// Extract fields from value struct via extractvalue
	startIdx := uint64(layout.ValueFieldIndex["start"])
	endIdx := uint64(layout.ValueFieldIndex["end"])
	inclIdx := uint64(layout.ValueFieldIndex["inclusive"])
	start := c.block.NewExtractValue(rangeVal, startIdx)
	end := c.block.NewExtractValue(rangeVal, endIdx)
	inclusive := c.block.NewExtractValue(rangeVal, inclIdx)

	// Determine element LLVM type and comparison predicate
	elemLLVM := c.resolveType(elemType)
	ltPred := enum.IPredSLT // signed less-than by default
	named := extractNamed(elemType)
	if named != nil && classify(named) == CatUnsignedInt {
		ltPred = enum.IPredULT
	}

	counterAlloca := c.createEntryAlloca(elemLLVM)
	counterAlloca.SetName(c.uniqueLocalName(s.Binding))
	c.block.NewStore(start, counterAlloca)
	c.locals[s.Binding] = counterAlloca

	if s.Index != "" {
		indexAlloca := c.createEntryAlloca(irtypes.I64)
		indexAlloca.SetName(c.uniqueLocalName(s.Index))
		c.block.NewStore(constant.NewInt(irtypes.I64, 0), indexAlloca)
		c.locals[s.Index] = indexAlloca
	}

	headerBlock := c.newBlock("forin.header")
	bodyBlock := c.newBlock("forin.body")
	updateBlock := c.newBlock("forin.update")
	exitBlock := c.newBlock("forin.exit")

	c.block.NewBr(headerBlock)

	// Header: counter < end || (counter == end && inclusive)
	c.block = headerBlock
	counter := c.block.NewLoad(elemLLVM, counterAlloca)
	ltCond := c.block.NewICmp(ltPred, counter, end)
	eqCond := c.block.NewICmp(enum.IPredEQ, counter, end)
	inclAndEq := c.block.NewAnd(inclusive, eqCond)
	cond := c.block.NewOr(ltCond, inclAndEq)
	c.block.NewCondBr(cond, bodyBlock, exitBlock)

	savedBreak := c.breakTarget
	savedContinue := c.continueTarget
	savedLoopUseDepth := c.loopScopeDepth
	c.breakTarget = exitBlock
	c.continueTarget = updateBlock
	c.loopScopeDepth = len(c.scopeBindings)

	c.block = bodyBlock
	c.genBlock(s.Body)
	if c.block.Term == nil {
		c.block.NewBr(updateBlock)
	}

	// Update: increment counter
	c.block = updateBlock
	cur := c.block.NewLoad(elemLLVM, counterAlloca)
	one := constant.NewInt(elemLLVM.(*irtypes.IntType), 1)
	next := c.block.NewAdd(cur, one)
	c.block.NewStore(next, counterAlloca)

	if s.Index != "" {
		idxAlloca := c.locals[s.Index]
		curIdx := c.block.NewLoad(irtypes.I64, idxAlloca)
		nextIdx := c.block.NewAdd(curIdx, constant.NewInt(irtypes.I64, 1))
		c.block.NewStore(nextIdx, idxAlloca)
	}

	c.emitYieldCheck()
	c.block.NewBr(headerBlock)

	c.breakTarget = savedBreak
	c.continueTarget = savedContinue
	c.loopScopeDepth = savedLoopUseDepth
	c.block = exitBlock
}

// --- Classic for loop ---

func (c *Compiler) genClassicForStmt(s *ast.ClassicForStmt) {
	// Init: declare the loop variable
	if s.InitValue != nil {
		typ := c.info.Types[s.InitValue]
		lt := c.resolveType(typ)
		alloca := c.createEntryAlloca(lt)
		alloca.SetName(c.uniqueLocalName(s.InitName))
		val := c.genExpr(s.InitValue)
		c.block.NewStore(val, alloca)
		c.locals[s.InitName] = alloca
	}

	headerBlock := c.newBlock("for.header")
	bodyBlock := c.newBlock("for.body")
	updateBlock := c.newBlock("for.update")
	exitBlock := c.newBlock("for.exit")

	c.block.NewBr(headerBlock)

	// Header: evaluate condition
	c.block = headerBlock
	cond := c.genExpr(s.Cond)
	c.block.NewCondBr(cond, bodyBlock, exitBlock)

	// Body
	savedBreak := c.breakTarget
	savedContinue := c.continueTarget
	savedLoopUseDepth := c.loopScopeDepth
	c.breakTarget = exitBlock
	c.continueTarget = updateBlock
	c.loopScopeDepth = len(c.scopeBindings)

	c.block = bodyBlock
	if c.shouldInstrument() {
		pos := s.Body.Pos()
		end := s.Body.End()
		idx := c.addCoverageRegion(pos.File, pos.Line, end.Line, c.currentCoverageFuncName(), "for.body")
		c.emitCoverageIncrement(idx)
	}
	c.genBlock(s.Body)
	if c.block.Term == nil {
		c.block.NewBr(updateBlock)
	}

	// Update
	c.block = updateBlock
	if s.UpdateIncDec {
		// Inc/dec update: target++ or target--
		c.genIncDecTarget(s.UpdateTarget, s.UpdateIsInc)
	} else if s.UpdateTarget != nil {
		// Compound update: target op= value
		updateVal := c.genExpr(s.UpdateValue)
		ident, ok := s.UpdateTarget.(*ast.IdentExpr)
		if ok {
			alloca, ok := c.locals[ident.Name]
			if ok {
				if s.UpdateOp == ast.OpAssign {
					c.block.NewStore(updateVal, alloca)
				} else {
					current := c.block.NewLoad(alloca.ElemType, alloca)
					result := c.genCompoundOp(s.UpdateOp, c.info.Types[s.UpdateTarget], current, updateVal)
					c.block.NewStore(result, alloca)
				}
			}
		}
	} else if s.UpdateValue != nil {
		// Expression-only update
		c.genExpr(s.UpdateValue)
	}
	c.emitYieldCheck()
	c.block.NewBr(headerBlock)

	c.breakTarget = savedBreak
	c.continueTarget = savedContinue
	c.loopScopeDepth = savedLoopUseDepth
	c.block = exitBlock
}

// --- Infinite loop ---

func (c *Compiler) genInfiniteLoop(s *ast.InfiniteLoop) {
	bodyBlock := c.newBlock("loop.body")
	exitBlock := c.newBlock("loop.exit")

	c.block.NewBr(bodyBlock)

	savedBreak := c.breakTarget
	savedContinue := c.continueTarget
	savedLoopUseDepth := c.loopScopeDepth
	c.breakTarget = exitBlock
	c.continueTarget = bodyBlock
	c.loopScopeDepth = len(c.scopeBindings)

	c.block = bodyBlock
	if c.shouldInstrument() {
		pos := s.Body.Pos()
		end := s.Body.End()
		idx := c.addCoverageRegion(pos.File, pos.Line, end.Line, c.currentCoverageFuncName(), "loop.body")
		c.emitCoverageIncrement(idx)
	}
	c.genBlock(s.Body)
	if c.block != nil && c.block.Term == nil {
		c.emitYieldCheck()
		c.block.NewBr(bodyBlock)
	}

	c.breakTarget = savedBreak
	c.continueTarget = savedContinue
	c.loopScopeDepth = savedLoopUseDepth
	c.block = exitBlock
}

// --- Cooperative preemption yield check ---

// emitYieldCheck emits an inline cooperative yield check at loop back-edges.
// Only active when c.inCoroutine is true (inside goroutine/main coroutine).
// Checks G.preempt flag set by sysmon; if set, clears it, re-enqueues self,
// and calls coro.suspend to yield to the scheduler.
func (c *Compiler) emitYieldCheck() {
	if !c.inCoroutine {
		return
	}
	if c.block == nil || c.block.Term != nil {
		return
	}

	gTy := goroutineStructType()

	// Load current G
	curG := c.block.NewLoad(irtypes.I8Ptr, c.currentGGlobal)
	gPtr := c.block.NewBitCast(curG, irtypes.NewPointer(gTy))

	// Load G.preempt
	preemptField := c.block.NewGetElementPtr(gTy, gPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(gFieldPreempt)))
	preemptVal := c.block.NewLoad(irtypes.I8, preemptField)
	shouldYield := c.block.NewICmp(enum.IPredNE, preemptVal, constant.NewInt(irtypes.I8, 0))

	yieldBlk := c.newBlock("yield")
	continueBlk := c.newBlock("yield.cont")
	c.block.NewCondBr(shouldYield, yieldBlk, continueBlk)

	// yield: clear preempt, coro.suspend (scheduler re-enqueues after suspend)
	//
	// IMPORTANT: We must NOT enqueue self before coro.suspend. If we did,
	// another M could pick up G from the run queue and call coro.resume
	// before our coro.suspend completes — that's UB in LLVM's coroutine model.
	// Instead, we just suspend. The scheduler detects a yield (park_mutex==null)
	// and re-enqueues the goroutine after coro.suspend has fully completed.
	c.block = yieldBlk
	c.block.NewStore(constant.NewInt(irtypes.I8, 0), preemptField)

	// Suspend — scheduler detects yield (null park_mutex) and re-enqueues us
	suspResult := c.block.NewCall(c.coroSuspend, constant.None, constant.False)
	c.block.NewSwitch(suspResult, c.coroSuspendBlk,
		ir.NewCase(constant.NewInt(irtypes.I8, 0), continueBlk),
		ir.NewCase(constant.NewInt(irtypes.I8, 1), c.coroCleanupBlk))

	// yield.cont: continue with the loop
	c.block = continueBlk
}

// --- Break / Continue ---

func (c *Compiler) genBreakStmt() {
	if c.breakTarget != nil {
		// Close use bindings added within the loop body
		if len(c.scopeBindings) > c.loopScopeDepth {
			cap := c.emitScopeCleanup(c.loopScopeDepth, false)
			c.emitCloseErrCheck(cap)
		}
		c.block.NewBr(c.breakTarget)
	}
}

func (c *Compiler) genContinueStmt() {
	if c.continueTarget != nil {
		// Close use bindings added within the loop body
		if len(c.scopeBindings) > c.loopScopeDepth {
			cap := c.emitScopeCleanup(c.loopScopeDepth, false)
			c.emitCloseErrCheck(cap)
		}
		c.block.NewBr(c.continueTarget)
	}
}

// --- Index assignment ---

// genIndexAssign handles assignment to a container element: arr[i] = val, m[k] = val.
func (c *Compiler) genIndexAssign(target *ast.IndexExpr, op ast.AssignOp, val value.Value, srcExpr ast.Expr) {
	targetType := c.info.Types[target.Target]
	if c.typeSubst != nil {
		targetType = types.Substitute(targetType, c.typeSubst)
	}
	// Unwrap MutRef/SharedRef for index assignment (auto-deref through borrows)
	if ref, ok := targetType.(*types.MutRef); ok {
		targetType = ref.Elem()
	}
	if ref, ok := targetType.(*types.SharedRef); ok {
		targetType = ref.Elem()
	}

	// Fixed-size array index assignment
	if arr, ok := targetType.(*types.Array); ok {
		c.genArrayIndexAssign(target, arr, op, val)
		return
	}

	named := extractNamed(targetType)
	if named != nil {
		if m := named.LookupMethod("[]="); m != nil {
			if m.IsNative() {
				c.genNativeIndexAssign(target, targetType, op, val, srcExpr)
				return
			}
			c.genMethodIndexAssign(target, targetType, val)
			return
		}
	}
	panic(fmt.Sprintf("codegen: cannot assign to index of type %s", targetType))
}

// genArrayIndexAssign handles arr[i] = val for fixed-size arrays with bounds checking.
func (c *Compiler) genArrayIndexAssign(target *ast.IndexExpr, arr *types.Array, op ast.AssignOp, val value.Value) {
	basePtr := c.genArrayBasePtr(target.Target, arr)
	idx := c.genExpr(target.Index)
	elemLLVM := c.resolveType(arr.Elem())
	arrType := irtypes.NewArray(uint64(arr.Size()), elemLLVM)

	// Bounds check
	size := constant.NewInt(irtypes.I64, arr.Size())
	inBounds := c.block.NewICmp(enum.IPredULT, idx, size)
	okBlock := c.newBlock("arrassign.ok")
	panicBlock := c.newBlock("arrassign.oob")
	c.block.NewCondBr(inBounds, okBlock, panicBlock)

	c.block = panicBlock
	oobMsg := c.makeGlobalString("array index out of bounds")
	c.block.NewCall(c.funcs["promise_panic"], oobMsg)
	c.emitPanicReturn()

	c.block = okBlock
	elemPtr := c.block.NewGetElementPtr(arrType, basePtr,
		constant.NewInt(irtypes.I32, 0), idx)

	elemType := arr.Elem()
	if c.typeSubst != nil {
		elemType = types.Substitute(elemType, c.typeSubst)
	}

	if op == ast.OpAssign {
		// T0583: Drop the previous element before storing the new value. Without
		// this, overwriting a droppable element (string, Vector, Channel, Arc,
		// heap user, Optional<droppable>, droppable tuple/nested array) leaks
		// the previous allocation. Mirrors genVectorIndexAssign and
		// genMemberAssign drop-on-overwrite patterns.
		if c.typeNeedsFieldDrop(elemType) {
			oldVal := c.block.NewLoad(elemLLVM, elemPtr)
			c.emitVariantFieldDrop(oldVal, elemType)
		}
		c.block.NewStore(val, elemPtr)
		return
	}

	// Compound assignment
	current := c.block.NewLoad(elemLLVM, elemPtr)
	result := c.genCompoundOp(op, arr.Elem(), current, val)
	// T0583: Drop old string before storing the concat result. Numeric/bool/char
	// compound ops produce values, not new allocations — only string applies.
	if extractNamed(elemType) == types.TypString {
		c.emitStringDropOldValue(current, result)
	}
	c.block.NewStore(result, elemPtr)
}

// genNativeIndexAssign dispatches native []= implementations for built-in types.
func (c *Compiler) genNativeIndexAssign(target *ast.IndexExpr, targetType types.Type, op ast.AssignOp, val value.Value, srcExpr ast.Expr) {
	if elem, ok := types.AsVector(targetType); ok {
		c.genVectorIndexAssign(target, elem, op, val, srcExpr)
		return
	}
	// Inside monomorphized method body: targetType is Named(Vector) not Instance(Vector[T]).
	named := extractNamed(targetType)
	if named == types.TypVector && c.typeSubst != nil {
		tp := named.TypeParams()[0]
		if elem, ok := c.typeSubst[tp]; ok {
			c.genVectorIndexAssign(target, elem, op, val, srcExpr)
			return
		}
	}
	panic(fmt.Sprintf("codegen: no native []= implementation for type %s", targetType))
}

// genMethodIndexAssign calls the monomorphized []= method on a user type.
func (c *Compiler) genMethodIndexAssign(target *ast.IndexExpr, targetType types.Type, val value.Value) {
	mangledName := mangleMethodName(c.resolveTypeName(targetType), "[]=", false)
	fn, ok := c.funcs[mangledName]
	if !ok {
		panic(fmt.Sprintf("codegen: undeclared []= method %s", mangledName))
	}

	targetVal := c.genExpr(target.Target)
	keyVal := c.genExpr(target.Index)

	// B0343: Map []= takes ~K key (move). Handle string key ownership so the
	// map holds an independent copy. If the key source has a drop flag, clear
	// it (ownership transferred). If no drop flag (borrow param), dup so the
	// map doesn't hold a pointer that the caller will free.
	if keyType, _, isMap := types.AsMap(targetType); isMap {
		resolvedKey := keyType
		if c.typeSubst != nil {
			resolvedKey = types.Substitute(resolvedKey, c.typeSubst)
		}
		if extractNamed(resolvedKey) == types.TypString {
			if ident, ok := target.Index.(*ast.IdentExpr); ok {
				if _, hasFlag := c.dropFlags[ident.Name]; hasFlag {
					c.clearDropFlag(ident.Name)
				} else {
					keyVal = c.dupString(keyVal)
				}
			} else if isStringBorrowExpr(target.Index) {
				// B0355: non-ident borrow expr (field access, container element) as map key —
				// the source still owns the pointer; dup so map holds an independent copy.
				keyVal = c.dupString(keyVal)
			}
		}
	}

	var instancePtr value.Value
	switch {
	case isThisReceiver(target.Target):
		// T0745: `this` (incl. paren-wrapped) is already the i8* receiver ptr.
		instancePtr = targetVal
	case isContainerType(targetType):
		instancePtr = targetVal
	default:
		instancePtr = c.extractInstancePtr(targetVal)
	}

	call := c.block.NewCall(fn, instancePtr, keyVal, val)
	c.propagateIfFailable(call) // T0708
	// B0232: Claim string/heap temps for the key — ownership transfers to the []= method.
	// Without this, temporary keys (e.g., "a".repeat(2)) are freed at statement end
	// while still stored in the container, causing dangling pointers.
	c.claimStringTemp(keyVal)
	c.claimHeapTemp(keyVal)
}

// genVectorIndexAssign handles vec[i] = val with bounds check.
func (c *Compiler) genVectorIndexAssign(target *ast.IndexExpr, elemType types.Type, op ast.AssignOp, val value.Value, srcExpr ast.Expr) {
	slicePtr := c.genExpr(target.Target)
	idx := c.genExpr(target.Index)
	elemLLVM := c.resolveType(elemType)
	elemSize := int64(c.typeSize(elemLLVM))

	// COW: if static (.rodata), copy to heap first (T0062)
	cowSlice := c.block.NewCall(c.funcs["promise_vector_cow"],
		slicePtr, constant.NewInt(irtypes.I64, elemSize))
	c.storeBackSlicePtr(target.Target, cowSlice)

	// Bounds check (masked len)
	headerType := vectorHeaderType()
	headerPtr := c.block.NewBitCast(cowSlice, irtypes.NewPointer(headerType))
	length := loadVectorLen(c.block, headerPtr)

	inBounds := c.block.NewICmp(enum.IPredULT, idx, length)
	okBlock := c.newBlock("indexassign.ok")
	panicBlock := c.newBlock("indexassign.oob")
	c.block.NewCondBr(inBounds, okBlock, panicBlock)

	c.block = panicBlock
	oobMsg := c.makeGlobalString("index out of bounds")
	c.block.NewCall(c.funcs["promise_panic"], oobMsg)
	c.emitPanicReturn()

	c.block = okBlock
	dataBase := c.block.NewGetElementPtr(irtypes.I8, cowSlice,
		constant.NewInt(irtypes.I64, int64(vectorHeaderSize)))
	dataTypedPtr := c.block.NewBitCast(dataBase, irtypes.NewPointer(elemLLVM))
	elemPtr := c.block.NewGetElementPtr(elemLLVM, dataTypedPtr, idx)

	if op == ast.OpAssign {
		// B0195: New value is dup'd at the call site (like push, B0189) so the
		// vector owns an independent copy.
		// B0204: Drop old string element before storing new value. This is safe
		// because dup-on-read (B0204 in genVectorIndex) ensures any local variable
		// that captured the old value via vec[i] owns an independent copy.
		if extractNamed(elemType) == types.TypString {
			if dropFn, ok := c.funcs["promise_string_drop"]; ok {
				oldVal := c.block.NewLoad(elemLLVM, elemPtr)
				c.block.NewCall(dropFn, oldVal)
			}
		} else if c.vecElemNeedsEnumDrop(elemType) {
			// B0235: Drop old enum element before overwriting. Enum elements are
			// stored by value in vector buffers, so each element is an independent
			// copy. emitVariantFieldDrop allocas the old value, bitcasts to i8*,
			// and calls the synthesized enum drop function.
			oldVal := c.block.NewLoad(elemLLVM, elemPtr)
			c.emitVariantFieldDrop(oldVal, elemType)
		} else if types.IsVector(elemType) || types.IsChannel(elemType) ||
			types.IsArc(elemType) || types.IsWeak(elemType) {
			// T0383: Drop old element before overwriting for nested heap container
			// types (Vector, Channel, Arc, Weak). Without this, overwriting via
			// vec[i] = newVal leaks the old element. Safe because genVectorIndex
			// dups these on read (T0383 dup-on-read), so any aliased local owns
			// an independent copy. Mirrors the Vector[string] B0204 pattern.
			oldVal := c.block.NewLoad(elemLLVM, elemPtr)
			c.emitVariantFieldDrop(oldVal, elemType)
		} else if isDroppableHeapUserType(elemType) || isHeapUserNoDropPalFree(elemType) {
			// T0398: Drop old heap user-type element before overwriting. Without this,
			// `vec[i] = X` leaks vec[i]'s previous instance. Safe because dup-on-read
			// (T0398 in genVectorIndex, set above in genAssignStmt) ensures any RHS
			// vec reads return independent clones — no live alias to the freed instance.
			// T0908: also cover heap user types with NO drop (isHeapUserNoDropPalFree);
			// emitVariantFieldDrop's B0218 branch pal_frees the old no-drop instance.
			oldVal := c.block.NewLoad(elemLLVM, elemPtr)
			c.emitVariantFieldDrop(oldVal, elemType)
		} else if c.tupleNeedsDrop(elemType) {
			// T0412: Drop old tuple element before overwriting. Without this,
			// `vec[i] = X` for Vector[(droppable, ...)] leaks vec[i]'s previous
			// tuple's heap fields (vector buffers, strings, channels, nested
			// user types). Safe because the dup-on-read flag set in genAssignStmt
			// ensures vec-to-vec writes produce independent clones — no live
			// alias to the freed tuple's fields. emitVariantFieldDrop's tuple
			// branch walks each element via ExtractValue + recursive drop.
			oldVal := c.block.NewLoad(elemLLVM, elemPtr)
			c.emitVariantFieldDrop(oldVal, elemType)
		} else if c.vecElemNeedsOptionalDrop(elemType) {
			// T0620: Drop old Optional[droppable] element before overwriting.
			// Safe because: Gap A (genArrayLit clearDropFlag) ensures the vector
			// is the sole owner, and dup-on-read (genVectorIndex) ensures any
			// local variable that read via v[i] holds an independent copy.
			oldVal := c.block.NewLoad(elemLLVM, elemPtr)
			c.emitVariantFieldDrop(oldVal, elemType)
		}
		c.block.NewStore(val, elemPtr)
		// T0909: When RHS is a method/operator whose body is `return this`,
		// the returned value aliases the receiver. Clear the receiver's drop
		// flag so scope-exit doesn't double-free the instance now owned by
		// this element slot.
		if srcExpr != nil {
			var aliasOrigin ast.Expr
			if call, ok := srcExpr.(*ast.CallExpr); ok {
				aliasOrigin = chainOriginExpr(call)
			} else {
				aliasOrigin = operatorReceiverOrigin(srcExpr)
			}
			if id, ok := aliasOrigin.(*ast.IdentExpr); ok {
				c.maybeClearReceiverDropFlag(val, id.Name, elemType)
			}
			// ThisExpr origin: `this` has no per-variable drop flag, no clear needed.
		}
		return
	}

	// Compound assignment
	current := c.block.NewLoad(elemLLVM, elemPtr)
	result := c.genCompoundOp(op, elemType, current, val)
	// T0363: Drop the old element before storing the new one. Without this,
	// heap-allocated old values leak.
	if extractNamed(elemType) == types.TypString {
		c.emitStringDropOldValue(current, result)
	}
	c.block.NewStore(result, elemPtr)
}

// genCompoundIndexAssign handles compound index assignments (arr[i] += val, m[k] += val)
// with correct evaluation order: target → key → RHS.
func (c *Compiler) genCompoundIndexAssign(target *ast.IndexExpr, op ast.AssignOp, valueExpr ast.Expr) {
	targetType := c.info.Types[target.Target]
	if c.typeSubst != nil {
		targetType = types.Substitute(targetType, c.typeSubst)
	}

	// Fixed-size array compound assignment.
	// Evaluation order (RHS before target) is safe: arrays are stack-local copy types, no aliasing.
	if arr, ok := targetType.(*types.Array); ok {
		val := c.genExpr(valueExpr)
		if c.info.AutoPropagateExprs[valueExpr] {
			val = c.genAutoPropagateValue(val)
		}
		c.genArrayIndexAssign(target, arr, op, val)
		return
	}

	named := extractNamed(targetType)
	if named != nil {
		if m := named.LookupMethod("[]="); m != nil {
			if m.IsNative() {
				// Native compound assign (vectors)
				elem, ok := types.AsVector(targetType)
				if !ok && named == types.TypVector && c.typeSubst != nil {
					tp := named.TypeParams()[0]
					elem, ok = c.typeSubst[tp], c.typeSubst[tp] != nil
				}
				if ok {
					slicePtr := c.genExpr(target.Target)
					idx := c.genExpr(target.Index)
					val := c.genExpr(valueExpr)
					if c.info.AutoPropagateExprs[valueExpr] {
						val = c.genAutoPropagateValue(val)
					}
					// COW: if static (.rodata), copy to heap first (T0062)
					elemLLVM := c.resolveType(elem)
					elemSize := int64(c.typeSize(elemLLVM))
					cowSlice := c.block.NewCall(c.funcs["promise_vector_cow"],
						slicePtr, constant.NewInt(irtypes.I64, elemSize))
					c.storeBackSlicePtr(target.Target, cowSlice)
					c.genVectorCompoundAssign(cowSlice, idx, elem, op, val)
					return
				}
			} else {
				// Non-native: read via [], apply op, write via []=
				c.genMethodCompoundAssign(target, targetType, op, valueExpr)
				return
			}
		}
	}
	panic(fmt.Sprintf("codegen: cannot compound-assign to index of type %s", targetType))
}

// genMethodCompoundAssign handles compound assignment (e.g. m[k] += v) on non-native types
// by calling [] to read, applying the operator, then calling []= to write.
func (c *Compiler) genMethodCompoundAssign(target *ast.IndexExpr, targetType types.Type, op ast.AssignOp, valueExpr ast.Expr) {
	typeName := c.resolveTypeName(targetType)

	getFnName := mangleMethodName(typeName, "[]", false)
	getFn, ok := c.funcs[getFnName]
	if !ok {
		panic(fmt.Sprintf("codegen: undeclared [] method %s", getFnName))
	}
	setFnName := mangleMethodName(typeName, "[]=", false)
	setFn, ok := c.funcs[setFnName]
	if !ok {
		panic(fmt.Sprintf("codegen: undeclared []= method %s", setFnName))
	}

	targetVal := c.genExpr(target.Target)
	keyVal := c.genExpr(target.Index)
	val := c.genExpr(valueExpr)
	if c.info.AutoPropagateExprs[valueExpr] {
		val = c.genAutoPropagateValue(val)
	}

	var instancePtr value.Value
	if isContainerType(targetType) {
		instancePtr = targetVal
	} else {
		instancePtr = c.extractInstancePtr(targetVal)
	}

	// Call [] to get current value (returns V? for maps)
	var optVal value.Value = c.block.NewCall(getFn, instancePtr, keyVal)

	// T0709: a failable [] read propagates its error before the value is used.
	var indexMethod *types.Method
	if n := extractNamed(targetType); n != nil {
		indexMethod = n.LookupMethod("[]")
	}
	isOpt := true // existing behavior: [] returns V? (optional presence)
	if indexMethod != nil {
		if indexMethod.Sig().CanError() {
			optVal = c.genAutoPropagateValue(optVal)
		}
		_, isOpt = indexMethod.Sig().Result().(*types.Optional)
	}

	var current value.Value
	if isOpt {
		// Check has_value flag (field 0 of optional struct)
		hasVal := c.block.NewExtractValue(optVal, 0)
		okBlock := c.newBlock("mapcomp.ok")
		panicBlock := c.newBlock("mapcomp.panic")
		c.block.NewCondBr(hasVal, okBlock, panicBlock)

		c.block = panicBlock
		panicMsg := c.makeGlobalString("compound assignment on missing key")
		c.block.NewCall(c.funcs["promise_panic"], panicMsg)
		c.emitPanicReturn()

		c.block = okBlock
		current = c.block.NewExtractValue(optVal, 1)
	} else {
		current = optVal
	}
	// Compound op operates on V (the unwrapped element type from V?). For maps,
	// V is the second type argument; for other containers, derive from the []=
	// method's value parameter.
	operandType := c.compoundElemType(targetType)
	result := c.genCompoundOp(op, operandType, current, val)

	// T0363: Map.[] returns V? and dups the inner string when constructing the
	// optional, so `current` is a heap-allocated dup that would otherwise leak.
	// Drop it after computing `result`. The value stored in the map is freed
	// separately by Map.[]='s drop-old-on-overwrite logic.
	if operandType != nil && extractNamed(operandType) == types.TypString {
		c.emitStringDropOldValue(current, result)
	}

	call := c.block.NewCall(setFn, instancePtr, keyVal, result)
	c.propagateIfFailable(call) // T0708
}

// compoundElemType returns the element type that compound assignment on a
// container operates on (V for Map[K, V], element for Vector, etc.). Falls
// back to the []= method's value parameter when the container isn't a known
// builtin.
func (c *Compiler) compoundElemType(containerType types.Type) types.Type {
	if _, v, ok := types.AsMap(containerType); ok {
		return v
	}
	if elem, ok := types.AsVector(containerType); ok {
		return elem
	}
	if named := extractNamed(containerType); named != nil {
		if m := named.LookupMethod("[]="); m != nil {
			params := m.Sig().Params()
			if len(params) >= 2 {
				return params[1].Type()
			}
		}
	}
	return nil
}

// genVectorCompoundAssign handles vec[i] += val with bounds check and pre-evaluated operands.
func (c *Compiler) genVectorCompoundAssign(slicePtr, idx value.Value, elemType types.Type, op ast.AssignOp, val value.Value) {
	elemLLVM := c.resolveType(elemType)

	headerType := vectorHeaderType()
	headerPtr := c.block.NewBitCast(slicePtr, irtypes.NewPointer(headerType))
	length := loadVectorLen(c.block, headerPtr)

	inBounds := c.block.NewICmp(enum.IPredULT, idx, length)
	okBlock := c.newBlock("slicecomp.ok")
	panicBlock := c.newBlock("slicecomp.oob")
	c.block.NewCondBr(inBounds, okBlock, panicBlock)

	c.block = panicBlock
	oobMsg := c.makeGlobalString("index out of bounds")
	c.block.NewCall(c.funcs["promise_panic"], oobMsg)
	c.emitPanicReturn()

	c.block = okBlock
	dataBase := c.block.NewGetElementPtr(irtypes.I8, slicePtr,
		constant.NewInt(irtypes.I64, int64(vectorHeaderSize)))
	dataTypedPtr := c.block.NewBitCast(dataBase, irtypes.NewPointer(elemLLVM))
	elemPtr := c.block.NewGetElementPtr(elemLLVM, dataTypedPtr, idx)

	current := c.block.NewLoad(elemLLVM, elemPtr)
	result := c.genCompoundOp(op, elemType, current, val)
	// T0363: Drop the old element before storing the new one. Without this,
	// heap-allocated old values leak.
	if extractNamed(elemType) == types.TypString {
		c.emitStringDropOldValue(current, result)
	}
	c.block.NewStore(result, elemPtr)
}

// --- Slice assignment ---

// genSliceAssign handles assignment to a slice target: v[a:b] = val.
func (c *Compiler) genSliceAssign(target *ast.SliceExpr, val value.Value) {
	targetType := c.info.Types[target.Target]
	if c.typeSubst != nil {
		targetType = types.Substitute(targetType, c.typeSubst)
	}

	named := extractNamed(targetType)
	if named == nil {
		panic(fmt.Sprintf("codegen: cannot slice-assign to type %s", targetType))
	}
	m := named.LookupMethod("[:]=")
	if m == nil {
		panic(fmt.Sprintf("codegen: no [:]=  method on type %s", named))
	}

	targetVal := c.genExpr(target.Target)

	// Generate optional int arguments for low and high bounds
	optIntType := irtypes.NewStruct(irtypes.I1, irtypes.I64)
	low := c.genSliceBound(target.Low, optIntType)
	high := c.genSliceBound(target.High, optIntType)

	mangledName := mangleMethodName(c.resolveTypeName(targetType), "[:]=", false)
	fn, ok := c.funcs[mangledName]
	if !ok {
		panic(fmt.Sprintf("codegen: undeclared [:]=  method %s", mangledName))
	}

	var instancePtr value.Value
	switch {
	case isThisReceiver(target.Target):
		// T0745: `this` (incl. paren-wrapped) is already the i8* receiver ptr.
		instancePtr = targetVal
	case isContainerType(targetType):
		instancePtr = targetVal
	default:
		instancePtr = c.extractInstancePtr(targetVal)
	}

	// COW: if vector is static (.rodata), copy to heap first (T0062).
	// Must be done at the call site because [:]=  modifies this in-place
	// and the method's COW on individual element writes won't propagate back.
	if vecElem, isVec := types.AsVector(targetType); isVec {
		elemLLVM := c.resolveType(vecElem)
		elemSize := int64(c.typeSize(elemLLVM))
		instancePtr = c.block.NewCall(c.funcs["promise_vector_cow"],
			instancePtr, constant.NewInt(irtypes.I64, elemSize))
		c.storeBackSlicePtr(target.Target, instancePtr)
	}

	call := c.block.NewCall(fn, instancePtr, low, high, val)
	c.propagateIfFailable(call) // T0708
}

// --- lookupLocalType resolves the declared type for a TypedVarDecl ---
// It checks the TypeRef AST node to detect Optional declarations,
// then resolves the type by looking up the variable in sema scopes.

func (c *Compiler) lookupLocalType(s *ast.TypedVarDecl) types.Type {
	// Only need special handling for Optional declarations
	optRef, ok := s.Type.(*ast.OptionalTypeRef)
	if !ok {
		return nil // use expression type
	}
	// Always resolve the declared type from the AST so nested OptionalTypeRef
	// (T??, T???) preserves its full depth even when the value expr is itself
	// Optional (e.g. `T?? b = a` where a:T?). Using exprType here would collapse
	// the alloca to T?, mismatching sema and breaking unwraps.
	if t := c.resolveTypeRefToType(optRef); t != nil {
		return t
	}
	return c.lookupVarType(s.Name)
}

// resolveTypeRefToType resolves an AST TypeRef to a types.Type.
// Mirrors sema.Checker.resolveType so codegen can re-derive types for AST refs
// that don't have a direct Types[expr] entry (e.g., the target of `as!`).
func (c *Compiler) resolveTypeRefToType(ref ast.TypeRef) types.Type {
	switch r := ref.(type) {
	case *ast.NamedTypeRef:
		// If typeSubst is active, check if this name matches a TypeParam in the
		// substitution map. This avoids finding the wrong TypeParam from a different
		// generic type's scope during synthesized method body generation.
		if c.typeSubst != nil {
			for tp, concrete := range c.typeSubst {
				if tp.Obj().Name() == r.Name {
					return concrete
				}
			}
		}
		var base types.Type
		// Check Universe scope first (primitives)
		if obj, _ := types.Universe.LookupParent(r.Name); obj != nil {
			if tn, ok := obj.(*types.TypeName); ok {
				base = tn.Type()
			}
		}
		// Check file scope (user-defined types)
		if base == nil {
			for _, scope := range c.info.ScopeOrder {
				if obj := scope.Lookup(r.Name); obj != nil {
					if tn, ok := obj.(*types.TypeName); ok {
						base = tn.Type()
						break
					}
				}
			}
		}
		if base == nil {
			return nil
		}
		if len(r.TypeArgs) == 0 {
			return base
		}
		args := make([]types.Type, len(r.TypeArgs))
		for i, ta := range r.TypeArgs {
			args[i] = c.resolveTypeRefToType(ta)
			if args[i] == nil {
				return nil
			}
		}
		return types.NewInstance(base, args)
	case *ast.QualifiedTypeRef:
		// Module-qualified types: look up in sema scopes by unqualified name
		var base types.Type
		for _, scope := range c.info.ScopeOrder {
			if obj := scope.Lookup(r.Name); obj != nil {
				if tn, ok := obj.(*types.TypeName); ok {
					base = tn.Type()
					break
				}
			}
		}
		if base == nil {
			return nil
		}
		if len(r.TypeArgs) == 0 {
			return base
		}
		args := make([]types.Type, len(r.TypeArgs))
		for i, ta := range r.TypeArgs {
			args[i] = c.resolveTypeRefToType(ta)
			if args[i] == nil {
				return nil
			}
		}
		return types.NewInstance(base, args)
	case *ast.OptionalTypeRef:
		inner := c.resolveTypeRefToType(r.Inner)
		if inner != nil {
			return types.NewOptional(inner)
		}
	case *ast.SliceTypeRef:
		elem := c.resolveTypeRefToType(r.Element)
		if elem != nil {
			return types.NewVector(elem)
		}
	case *ast.ArrayTypeRef:
		elem := c.resolveTypeRefToType(r.Element)
		if elem == nil {
			return nil
		}
		size, err := strconv.ParseInt(r.Size, 10, 64)
		if err != nil {
			return nil
		}
		return types.NewArray(elem, size)
	case *ast.SharedRefTypeRef:
		inner := c.resolveTypeRefToType(r.Inner)
		if inner != nil {
			return types.NewSharedRef(inner)
		}
	case *ast.MutRefTypeRef:
		inner := c.resolveTypeRefToType(r.Inner)
		if inner != nil {
			return types.NewMutRef(inner)
		}
	case *ast.PointerTypeRef:
		inner := c.resolveTypeRefToType(r.Inner)
		if inner != nil {
			return types.NewPointer(inner)
		}
	case *ast.TupleTypeRef:
		elems := make([]types.Type, len(r.Elements))
		for i, e := range r.Elements {
			elems[i] = c.resolveTypeRefToType(e)
			if elems[i] == nil {
				return nil
			}
		}
		return types.NewTuple(elems)
	case *ast.FunctionTypeRef:
		params := make([]*types.Param, len(r.Params))
		for i, p := range r.Params {
			pt := c.resolveTypeRefToType(p)
			if pt == nil {
				return nil
			}
			params[i] = types.NewParam("", pt, types.RefNone)
		}
		var result types.Type
		if r.Return != nil {
			if named, ok := r.Return.(*ast.NamedTypeRef); ok && named.Name == "void" && len(named.TypeArgs) == 0 {
				// result stays nil
			} else {
				result = c.resolveTypeRefToType(r.Return)
				if result == nil {
					return nil
				}
			}
		}
		return types.NewSignature(nil, params, result, false)
	}
	return nil
}

// lookupVarType finds a variable's declared type by walking sema scopes.
func (c *Compiler) lookupVarType(name string) types.Type {
	for _, scope := range c.info.ScopeOrder {
		if obj := scope.Lookup(name); obj != nil {
			if v, ok := obj.(*types.Var); ok {
				typ := v.Type()
				if c.typeSubst != nil {
					typ = types.Substitute(typ, c.typeSubst)
				}
				return typ
			}
		}
	}
	return nil
}

// uniqueLocalName returns a unique LLVM name for a local variable alloca.
// On first use of a name within a function, returns it unchanged.
// On subsequent uses (shadowing in inner scopes), appends a numeric suffix.
func (c *Compiler) uniqueLocalName(name string) string {
	n := c.localNameCount[name]
	c.localNameCount[name] = n + 1
	if n == 0 {
		return name
	}
	return fmt.Sprintf("%s.%d", name, n)
}

// --- For-in over vectors ---

func (c *Compiler) genForInVector(s *ast.ForInStmt, slicePtr value.Value, elemType types.Type) {
	elemLLVM := c.resolveType(elemType)

	// Load length from header (masked)
	headerType := vectorHeaderType()
	headerPtr := c.block.NewBitCast(slicePtr, irtypes.NewPointer(headerType))
	length := loadVectorLen(c.block, headerPtr)

	// Counter alloca
	counterAlloca := c.createEntryAlloca(irtypes.I64)
	c.block.NewStore(constant.NewInt(irtypes.I64, 0), counterAlloca)

	// Element binding alloca
	elemAlloca := c.createEntryAlloca(elemLLVM)
	elemAlloca.SetName(c.uniqueLocalName(s.Binding))
	c.locals[s.Binding] = elemAlloca

	// B0277: For string elements, register a drop binding so dup'd strings are
	// freed when the loop variable is not moved. The flag starts at 0 (no value
	// to drop before the first iteration).
	dupStrings := s.Binding != "_" && extractNamed(elemType) == types.TypString
	if dupStrings {
		c.maybeRegisterDrop(s.Binding, elemAlloca, elemType)
		c.block.NewStore(constant.NewInt(irtypes.I1, 0), c.dropFlags[s.Binding])
	}

	// T0617: for a Task-element loop, record the current iteration's slot
	// address so `<-handle` (genReceiveTask) can null the consumed slot —
	// otherwise the Vector's scope-exit element drop reloads the freed G
	// and Task[T].drop double-frees → segfault. Per-iteration (not whole
	// vector) so un-awaited slots are still dropped once (T0503).
	isTaskElem := s.Binding != "_" && types.IsTask(elemType)
	var slotPtrAlloca *ir.InstAlloca
	if isTaskElem {
		slotPtrAlloca = c.createEntryAlloca(irtypes.NewPointer(elemLLVM))
	}

	// Index variable if present
	if s.Index != "" {
		indexAlloca := c.createEntryAlloca(irtypes.I64)
		indexAlloca.SetName(c.uniqueLocalName(s.Index))
		c.block.NewStore(constant.NewInt(irtypes.I64, 0), indexAlloca)
		c.locals[s.Index] = indexAlloca
	}

	headerBlock := c.newBlock("forin.header")
	bodyBlock := c.newBlock("forin.body")
	updateBlock := c.newBlock("forin.update")
	exitBlock := c.newBlock("forin.exit")

	c.block.NewBr(headerBlock)

	// Header: counter < length
	c.block = headerBlock
	counter := c.block.NewLoad(irtypes.I64, counterAlloca)
	cond := c.block.NewICmp(enum.IPredULT, counter, length)
	c.block.NewCondBr(cond, bodyBlock, exitBlock)

	// Body: load element, store to binding
	savedBreak := c.breakTarget
	savedContinue := c.continueTarget
	savedLoopUseDepth := c.loopScopeDepth
	c.breakTarget = exitBlock
	c.continueTarget = updateBlock
	c.loopScopeDepth = len(c.scopeBindings)

	// T0617: scope the slot-ptr map entry to this loop (save/restore, like
	// breakTarget) so nesting (`for x in v1 { for x in v2 {} }`) is safe and
	// it self-cleans across functions.
	var prevSlot *ir.InstAlloca
	var hadPrevSlot bool
	if isTaskElem {
		prevSlot, hadPrevSlot = c.forInHandleSlotPtr[s.Binding]
		c.forInHandleSlotPtr[s.Binding] = slotPtrAlloca
	}

	c.block = bodyBlock

	// B0277: Drop previous iteration's dup'd string if not moved, then dup new.
	if dupStrings {
		dropFlag := c.dropFlags[s.Binding]
		flag := c.block.NewLoad(irtypes.I1, dropFlag)
		dropBlk := c.newBlock("forin.str.drop")
		loadBlk := c.newBlock("forin.str.load")
		c.block.NewCondBr(flag, dropBlk, loadBlk)

		c.block = dropBlk
		oldVal := c.block.NewLoad(irtypes.I8Ptr, elemAlloca)
		c.block.NewCall(c.funcs["promise_string_drop"], oldVal)
		c.block.NewBr(loadBlk)

		c.block = loadBlk
	}

	dataBase := c.block.NewGetElementPtr(irtypes.I8, slicePtr,
		constant.NewInt(irtypes.I64, int64(vectorHeaderSize)))
	dataTypedPtr := c.block.NewBitCast(dataBase, irtypes.NewPointer(elemLLVM))
	curCounter := c.block.NewLoad(irtypes.I64, counterAlloca)
	elemPtr := c.block.NewGetElementPtr(elemLLVM, dataTypedPtr, curCounter)
	if isTaskElem {
		c.block.NewStore(elemPtr, slotPtrAlloca) // T0617
	}
	var elemVal value.Value = c.block.NewLoad(elemLLVM, elemPtr)

	if dupStrings {
		elemVal = c.dupString(elemVal)
		c.block.NewStore(elemVal, elemAlloca)
		c.block.NewStore(constant.NewInt(irtypes.I1, 1), c.dropFlags[s.Binding])
	} else {
		c.block.NewStore(elemVal, elemAlloca)
	}

	c.genBlock(s.Body)
	if c.block.Term == nil {
		c.block.NewBr(updateBlock)
	}

	// Update: increment counter (and index if present)
	c.block = updateBlock
	cur := c.block.NewLoad(irtypes.I64, counterAlloca)
	next := c.block.NewAdd(cur, constant.NewInt(irtypes.I64, 1))
	c.block.NewStore(next, counterAlloca)

	if s.Index != "" {
		idxAlloca := c.locals[s.Index]
		curIdx := c.block.NewLoad(irtypes.I64, idxAlloca)
		nextIdx := c.block.NewAdd(curIdx, constant.NewInt(irtypes.I64, 1))
		c.block.NewStore(nextIdx, idxAlloca)
	}

	c.emitYieldCheck()
	c.block.NewBr(headerBlock)

	c.breakTarget = savedBreak
	c.continueTarget = savedContinue
	c.loopScopeDepth = savedLoopUseDepth
	c.block = exitBlock

	if isTaskElem { // T0617: restore the scoped slot-ptr map entry
		if hadPrevSlot {
			c.forInHandleSlotPtr[s.Binding] = prevSlot
		} else {
			delete(c.forInHandleSlotPtr, s.Binding)
		}
	}
}

// --- For-in over fixed-size arrays ---

// genForInArray iterates a fixed-size array with a compile-time-known length.
func (c *Compiler) genForInArray(s *ast.ForInStmt, arr *types.Array) {
	basePtr := c.genArrayBasePtr(s.Iterable, arr)
	elemLLVM := c.resolveType(arr.Elem())
	arrType := irtypes.NewArray(uint64(arr.Size()), elemLLVM)
	length := constant.NewInt(irtypes.I64, arr.Size())

	// Counter alloca
	counterAlloca := c.createEntryAlloca(irtypes.I64)
	c.block.NewStore(constant.NewInt(irtypes.I64, 0), counterAlloca)

	// Element binding alloca
	elemAlloca := c.createEntryAlloca(elemLLVM)
	elemAlloca.SetName(c.uniqueLocalName(s.Binding))
	c.locals[s.Binding] = elemAlloca

	// B0279: For string elements, register a drop binding so dup'd strings are
	// freed when the loop variable is not moved. The flag starts at 0 (no value
	// to drop before the first iteration).
	dupStrings := s.Binding != "_" && extractNamed(arr.Elem()) == types.TypString
	if dupStrings {
		c.maybeRegisterDrop(s.Binding, elemAlloca, arr.Elem())
		c.block.NewStore(constant.NewInt(irtypes.I1, 0), c.dropFlags[s.Binding])
	}

	// T0617: for a Task-element loop, record the current iteration's slot
	// address so `<-handle` (genReceiveTask) can null the consumed slot —
	// otherwise the array's scope-exit element drop reloads the freed G and
	// Task[T].drop double-frees → segfault. Per-iteration (not whole array)
	// so un-awaited slots are still dropped once (T0503).
	isTaskElem := s.Binding != "_" && types.IsTask(arr.Elem())
	var slotPtrAlloca *ir.InstAlloca
	if isTaskElem {
		slotPtrAlloca = c.createEntryAlloca(irtypes.NewPointer(elemLLVM))
	}

	// Index variable if present
	if s.Index != "" {
		indexAlloca := c.createEntryAlloca(irtypes.I64)
		indexAlloca.SetName(c.uniqueLocalName(s.Index))
		c.block.NewStore(constant.NewInt(irtypes.I64, 0), indexAlloca)
		c.locals[s.Index] = indexAlloca
	}

	headerBlock := c.newBlock("forin.header")
	bodyBlock := c.newBlock("forin.body")
	updateBlock := c.newBlock("forin.update")
	exitBlock := c.newBlock("forin.exit")

	c.block.NewBr(headerBlock)

	// Header: counter < length
	c.block = headerBlock
	counter := c.block.NewLoad(irtypes.I64, counterAlloca)
	cond := c.block.NewICmp(enum.IPredULT, counter, length)
	c.block.NewCondBr(cond, bodyBlock, exitBlock)

	// Body: load element, store to binding
	savedBreak := c.breakTarget
	savedContinue := c.continueTarget
	savedLoopUseDepth := c.loopScopeDepth
	c.breakTarget = exitBlock
	c.continueTarget = updateBlock
	c.loopScopeDepth = len(c.scopeBindings)

	// T0617: scope the slot-ptr map entry to this loop (save/restore, like
	// breakTarget) so nesting is safe and it self-cleans across functions.
	var prevSlot *ir.InstAlloca
	var hadPrevSlot bool
	if isTaskElem {
		prevSlot, hadPrevSlot = c.forInHandleSlotPtr[s.Binding]
		c.forInHandleSlotPtr[s.Binding] = slotPtrAlloca
	}

	c.block = bodyBlock

	// B0279: Drop previous iteration's dup'd string if not moved, then dup new.
	if dupStrings {
		dropFlag := c.dropFlags[s.Binding]
		flag := c.block.NewLoad(irtypes.I1, dropFlag)
		dropBlk := c.newBlock("forin.str.drop")
		loadBlk := c.newBlock("forin.str.load")
		c.block.NewCondBr(flag, dropBlk, loadBlk)

		c.block = dropBlk
		oldVal := c.block.NewLoad(irtypes.I8Ptr, elemAlloca)
		c.block.NewCall(c.funcs["promise_string_drop"], oldVal)
		c.block.NewBr(loadBlk)

		c.block = loadBlk
	}

	curCounter := c.block.NewLoad(irtypes.I64, counterAlloca)
	elemPtr := c.block.NewGetElementPtr(arrType, basePtr,
		constant.NewInt(irtypes.I32, 0), curCounter)
	if isTaskElem {
		c.block.NewStore(elemPtr, slotPtrAlloca) // T0617
	}
	var elem value.Value = c.block.NewLoad(elemLLVM, elemPtr)

	if dupStrings {
		elem = c.dupString(elem)
		c.block.NewStore(elem, elemAlloca)
		c.block.NewStore(constant.NewInt(irtypes.I1, 1), c.dropFlags[s.Binding])
	} else {
		c.block.NewStore(elem, elemAlloca)
	}

	c.genBlock(s.Body)
	if c.block.Term == nil {
		c.block.NewBr(updateBlock)
	}

	// Update: increment counter (and index if present)
	c.block = updateBlock
	cur := c.block.NewLoad(irtypes.I64, counterAlloca)
	next := c.block.NewAdd(cur, constant.NewInt(irtypes.I64, 1))
	c.block.NewStore(next, counterAlloca)

	if s.Index != "" {
		idxAlloca := c.locals[s.Index]
		curIdx := c.block.NewLoad(irtypes.I64, idxAlloca)
		nextIdx := c.block.NewAdd(curIdx, constant.NewInt(irtypes.I64, 1))
		c.block.NewStore(nextIdx, idxAlloca)
	}

	c.emitYieldCheck()
	c.block.NewBr(headerBlock)

	c.breakTarget = savedBreak
	c.continueTarget = savedContinue
	c.loopScopeDepth = savedLoopUseDepth
	c.block = exitBlock

	if isTaskElem { // T0617: restore the scoped slot-ptr map entry
		if hadPrevSlot {
			c.forInHandleSlotPtr[s.Binding] = prevSlot
		} else {
			delete(c.forInHandleSlotPtr, s.Binding)
		}
	}
}

// --- For-in over channels ---

// genForInChannel loops receiving from a channel until it returns none (closed+empty).
// for v in ch { ... }  ≡  loop { val := <-ch; if val is none: break; v := unwrap(val); ... }
func (c *Compiler) genForInChannel(s *ast.ForInStmt, chRaw value.Value, elemType types.Type) {
	elemLLVM := c.resolveType(elemType)
	elemSize := int64(c.typeSize(elemLLVM))

	chanType := channelStructType()
	chPtr := c.block.NewBitCast(chRaw, irtypes.NewPointer(chanType))

	// Element binding alloca
	elemAlloca := c.createEntryAlloca(elemLLVM)
	elemAlloca.SetName(c.uniqueLocalName(s.Binding))
	c.locals[s.Binding] = elemAlloca

	headerBlock := c.newBlock("forin_ch.header")
	recvWaitBlock := c.newBlock("forin_ch.recv.wait")
	recvCheckBlock := c.newBlock("forin_ch.recv.check")
	recvNoneBlock := c.newBlock("forin_ch.recv.none")
	recvReadBlock := c.newBlock("forin_ch.recv.read")
	bodyBlock := c.newBlock("forin_ch.body")
	exitBlock := c.newBlock("forin_ch.exit")

	c.block.NewBr(headerBlock)

	// header: lock mutex, then enter receive wait loop
	c.block = headerBlock
	mtxFieldPtr := c.block.NewGetElementPtr(chanType, chPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(chanFieldMutex)))
	mtx := c.block.NewLoad(irtypes.I8Ptr, mtxFieldPtr)
	neFieldPtr := c.block.NewGetElementPtr(chanType, chPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(chanFieldNotEmpty)))
	notEmpty := c.block.NewLoad(irtypes.I8Ptr, neFieldPtr)
	nfFieldPtr := c.block.NewGetElementPtr(chanType, chPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(chanFieldNotFull)))
	notFull := c.block.NewLoad(irtypes.I8Ptr, nfFieldPtr)

	c.block.NewCall(c.palMutexLock, mtx)
	c.block.NewBr(recvWaitBlock)

	// recv.wait: while count==0 && !closed → wait
	c.block = recvWaitBlock
	countPtr := c.block.NewGetElementPtr(chanType, chPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(chanFieldCount)))
	count := c.block.NewLoad(irtypes.I64, countPtr)
	isEmpty := c.block.NewICmp(enum.IPredEQ, count, constant.NewInt(irtypes.I64, 0))
	closedPtr := c.block.NewGetElementPtr(chanType, chPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(chanFieldClosed)))
	closedVal := c.block.NewLoad(irtypes.I8, closedPtr)
	isOpen := c.block.NewICmp(enum.IPredEQ, closedVal, constant.NewInt(irtypes.I8, 0))
	shouldWait := c.block.NewAnd(isEmpty, isOpen)

	recvWaitBodyBlock := c.newBlock("forin_ch.recv.wait.body")
	c.block.NewCondBr(shouldWait, recvWaitBodyBlock, recvCheckBlock)

	if c.inCoroutine {
		// Goroutine mode: park on recv_waiters + coro.suspend
		c.block = recvWaitBodyBlock
		currentG := c.block.NewLoad(irtypes.I8Ptr, c.currentGGlobal)
		recvHeadPtr := c.block.NewGetElementPtr(chanType, chPtr,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(chanFieldRecvWaitersHead)))
		recvTailPtr := c.block.NewGetElementPtr(chanType, chPtr,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(chanFieldRecvWaitersTail)))
		c.block.NewCall(c.funcs["promise_waiter_enqueue"], recvHeadPtr, recvTailPtr, currentG)
		// Store mutex in G.park_mutex — scheduler releases after coro.suspend completes
		gTyForIn := goroutineStructType()
		forInGPtr := c.block.NewBitCast(currentG, irtypes.NewPointer(gTyForIn))
		forInPmField := c.block.NewGetElementPtr(gTyForIn, forInGPtr,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(gFieldParkMutex)))
		c.block.NewStore(mtx, forInPmField)

		suspResult := c.block.NewCall(c.coroSuspend, constant.None, constant.False)
		resumeBlk := c.newBlock("forin_ch.recv.resume")
		c.block.NewSwitch(suspResult, c.coroSuspendBlk,
			ir.NewCase(constant.NewInt(irtypes.I8, 0), resumeBlk),
			ir.NewCase(constant.NewInt(irtypes.I8, 1), c.coroCleanupBlk))

		c.block = resumeBlk
		c.block.NewCall(c.palMutexLock, mtx)
		c.block.NewBr(recvWaitBlock)
	} else {
		// Thread-blocking mode: cond_wait, loop
		c.block = recvWaitBodyBlock
		c.block.NewCall(c.palCondWait, notEmpty, mtx)
		c.block.NewBr(recvWaitBlock)
	}

	// recv.check: if empty → exit (channel closed), else → read
	c.block = recvCheckBlock
	countAgain := c.block.NewLoad(irtypes.I64, countPtr)
	stillEmpty := c.block.NewICmp(enum.IPredEQ, countAgain, constant.NewInt(irtypes.I64, 0))
	c.block.NewCondBr(stillEmpty, recvNoneBlock, recvReadBlock)

	// recv.none: unlock and exit loop
	c.block = recvNoneBlock
	c.block.NewCall(c.palMutexUnlock, mtx)
	c.block.NewBr(exitBlock)

	// recv.read: read value from buffer, advance head, count--, wake sender, unlock, enter body
	c.block = recvReadBlock
	bufPtr := c.block.NewGetElementPtr(chanType, chPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(chanFieldBuffer)))
	buf := c.block.NewLoad(irtypes.I8Ptr, bufPtr)
	headPtr := c.block.NewGetElementPtr(chanType, chPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(chanFieldHead)))
	head := c.block.NewLoad(irtypes.I64, headPtr)
	offset := c.block.NewMul(head, constant.NewInt(irtypes.I64, elemSize))
	src := c.block.NewGetElementPtr(irtypes.I8, buf, offset)

	resultAsI8 := c.block.NewBitCast(elemAlloca, irtypes.I8Ptr)
	c.block.NewCall(c.funcs["llvm.memcpy"], resultAsI8, src,
		constant.NewInt(irtypes.I64, elemSize), constant.False)

	// head = (head + 1) % capacity
	capPtr := c.block.NewGetElementPtr(chanType, chPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(chanFieldCapacity)))
	cap_ := c.block.NewLoad(irtypes.I64, capPtr)
	headPlusOne := c.block.NewAdd(head, constant.NewInt(irtypes.I64, 1))
	newHead := c.block.NewURem(headPlusOne, cap_)
	c.block.NewStore(newHead, headPtr)

	// count--
	countRead := c.block.NewLoad(irtypes.I64, countPtr)
	newCount := c.block.NewSub(countRead, constant.NewInt(irtypes.I64, 1))
	c.block.NewStore(newCount, countPtr)

	// Wake a waiting sender (handles both regular G and select SWN nodes)
	sendHeadPtr := c.block.NewGetElementPtr(chanType, chPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(chanFieldSendWaitersHead)))
	sendTailPtr := c.block.NewGetElementPtr(chanType, chPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(chanFieldSendWaitersTail)))
	c.block.NewCall(c.funcs["promise_waiter_wake_one"], sendHeadPtr, sendTailPtr, notFull)

	// Unlock
	c.block.NewCall(c.palMutexUnlock, mtx)

	// Fall into body
	c.block.NewBr(bodyBlock)

	// body: execute loop body
	savedBreak := c.breakTarget
	savedContinue := c.continueTarget
	savedLoopUseDepth := c.loopScopeDepth
	c.breakTarget = exitBlock
	c.continueTarget = headerBlock
	c.loopScopeDepth = len(c.scopeBindings)

	c.block = bodyBlock
	// T0671: the received element is moved out of the ring buffer (recv advanced
	// head / decremented count), so the loop variable owns it. Register a drop
	// binding at loopScopeDepth so a heap element (string/Vector/user/Arc/Optional…)
	// is dropped on every exit: normal iteration end (below), break/continue
	// (genBreak/ContinueStmt → emitScopeCleanup(loopScopeDepth)), and early
	// return/raise (function unwind). maybeRegisterDrop no-ops for value elems
	// (Channel[int] unchanged). Disjoint from T0663's Channel.drop, which only
	// walks still-buffered [head,head+count) items (received items already left it).
	bodyScopeStart := len(c.scopeBindings) // == c.loopScopeDepth
	c.maybeRegisterDrop(s.Binding, elemAlloca, elemType)
	c.genBlock(s.Body)
	if c.block != nil && c.block.Term == nil {
		if len(c.scopeBindings) > bodyScopeStart {
			cap := c.emitScopeCleanup(bodyScopeStart, false)
			c.emitCloseErrCheck(cap)
		}
		c.emitYieldCheck()
		c.block.NewBr(headerBlock)
	}
	c.scopeBindings = c.scopeBindings[:bodyScopeStart] // unconditional codegen-time pop

	c.breakTarget = savedBreak
	c.continueTarget = savedContinue
	c.loopScopeDepth = savedLoopUseDepth
	c.block = exitBlock
}

// --- For-in over maps ---

// genForInMap iterates a Promise-implemented map by calling keys() and values()
// to produce vectors, then looping over them in parallel.
func (c *Compiler) genForInMap(s *ast.ForInStmt, mapVal value.Value, keyType, valType types.Type) {
	keyLLVM := c.resolveType(keyType)
	valLLVM := c.resolveType(valType)

	// Resolve monomorphized type name for method lookup
	iterType := c.info.Types[s.Iterable]
	if c.typeSubst != nil {
		iterType = types.Substitute(iterType, c.typeSubst)
	}
	inst, ok := iterType.(*types.Instance)
	if !ok {
		panic(fmt.Sprintf("codegen: for-in map target is %T, want Instance", iterType))
	}
	name := monoName(inst)

	// Call keys() and values() methods
	keysFnName := mangleMethodName(name, "keys", false)
	keysFn := c.funcs[keysFnName]
	valuesFnName := mangleMethodName(name, "values", false)
	valuesFn := c.funcs[valuesFnName]
	if keysFn == nil || valuesFn == nil {
		panic(fmt.Sprintf("codegen: undeclared map keys/values method for %s", name))
	}

	instancePtr := c.extractInstancePtr(mapVal)
	keysVec := c.block.NewCall(keysFn, instancePtr)
	valsVec := c.block.NewCall(valuesFn, instancePtr)

	// Get length from keys vector
	headerType := vectorHeaderType()
	headerPtr := c.block.NewBitCast(keysVec, irtypes.NewPointer(headerType))
	length := loadVectorLen(c.block, headerPtr)

	// Counter alloca
	counterAlloca := c.createEntryAlloca(irtypes.I64)
	c.block.NewStore(constant.NewInt(irtypes.I64, 0), counterAlloca)

	twoBindings := s.Index != "" // for k, v in map

	// B0343: Determine which bindings need string dup to prevent double-free.
	// keys()/values() return vectors with cloned strings. Without dup, the
	// iteration variable shares the heap pointer with the vector element.
	// emitVectorElementDropLoop would double-free strings that were moved.
	isKeyStr := extractNamed(keyType) == types.TypString
	isValStr := extractNamed(valType) == types.TypString
	var dupKeyStr, dupValStr bool
	var keyDropName, valDropName string
	var keyStrAlloca, valStrAlloca *ir.InstAlloca

	if twoBindings {
		// Separate key and value allocas
		keyAlloca := c.createEntryAlloca(keyLLVM)
		keyAlloca.SetName(c.uniqueLocalName(s.Index))
		c.locals[s.Index] = keyAlloca

		valAlloca := c.createEntryAlloca(valLLVM)
		valAlloca.SetName(c.uniqueLocalName(s.Binding))
		c.locals[s.Binding] = valAlloca

		// B0343: Register drop bindings for string keys/values.
		dupKeyStr = s.Index != "_" && isKeyStr
		dupValStr = s.Binding != "_" && isValStr
		if dupKeyStr {
			keyDropName = s.Index
			keyStrAlloca = keyAlloca
			c.maybeRegisterDrop(keyDropName, keyStrAlloca, keyType)
			c.block.NewStore(constant.NewInt(irtypes.I1, 0), c.dropFlags[keyDropName])
		}
		if dupValStr {
			valDropName = s.Binding
			valStrAlloca = valAlloca
			c.maybeRegisterDrop(valDropName, valStrAlloca, valType)
			c.block.NewStore(constant.NewInt(irtypes.I1, 0), c.dropFlags[valDropName])
		}
	} else {
		// Single binding: (K, V) tuple
		tupleType := irtypes.NewStruct(keyLLVM, valLLVM)
		bindingAlloca := c.createEntryAlloca(tupleType)
		bindingAlloca.SetName(c.uniqueLocalName(s.Binding))
		c.locals[s.Binding] = bindingAlloca

		// B0343: Hidden allocas for string lifecycle tracking in single-binding case.
		dupKeyStr = isKeyStr
		dupValStr = isValStr
		if dupKeyStr {
			keyDropName = "__forin_key"
			keyStrAlloca = c.createEntryAlloca(keyLLVM)
			c.maybeRegisterDrop(keyDropName, keyStrAlloca, keyType)
			c.block.NewStore(constant.NewInt(irtypes.I1, 0), c.dropFlags[keyDropName])
		}
		if dupValStr {
			valDropName = "__forin_val"
			valStrAlloca = c.createEntryAlloca(valLLVM)
			c.maybeRegisterDrop(valDropName, valStrAlloca, valType)
			c.block.NewStore(constant.NewInt(irtypes.I1, 0), c.dropFlags[valDropName])
		}
	}

	headerBlock := c.newBlock("forin.header")
	bodyBlock := c.newBlock("forin.body")
	updateBlock := c.newBlock("forin.update")
	exitBlock := c.newBlock("forin.exit")

	c.block.NewBr(headerBlock)

	// Header: compare counter < length
	c.block = headerBlock
	counter := c.block.NewLoad(irtypes.I64, counterAlloca)
	cond := c.block.NewICmp(enum.IPredULT, counter, length)
	c.block.NewCondBr(cond, bodyBlock, exitBlock)

	// Body: load key[i] and value[i], build tuple
	savedBreak := c.breakTarget
	savedContinue := c.continueTarget
	savedLoopUseDepth := c.loopScopeDepth
	c.breakTarget = exitBlock
	c.continueTarget = updateBlock
	c.loopScopeDepth = len(c.scopeBindings)

	c.block = bodyBlock

	// B0343: Drop previous iteration's dup'd strings if not moved.
	if dupKeyStr {
		dropFlag := c.dropFlags[keyDropName]
		flag := c.block.NewLoad(irtypes.I1, dropFlag)
		dropBlk := c.newBlock("forin.key.drop")
		afterBlk := c.newBlock("forin.key.after")
		c.block.NewCondBr(flag, dropBlk, afterBlk)
		c.block = dropBlk
		oldKey := c.block.NewLoad(irtypes.I8Ptr, keyStrAlloca)
		c.block.NewCall(c.funcs["promise_string_drop"], oldKey)
		c.block.NewBr(afterBlk)
		c.block = afterBlk
	}
	if dupValStr {
		dropFlag := c.dropFlags[valDropName]
		flag := c.block.NewLoad(irtypes.I1, dropFlag)
		dropBlk := c.newBlock("forin.val.drop")
		afterBlk := c.newBlock("forin.val.after")
		c.block.NewCondBr(flag, dropBlk, afterBlk)
		c.block = dropBlk
		oldVal := c.block.NewLoad(irtypes.I8Ptr, valStrAlloca)
		c.block.NewCall(c.funcs["promise_string_drop"], oldVal)
		c.block.NewBr(afterBlk)
		c.block = afterBlk
	}

	idx := c.block.NewLoad(irtypes.I64, counterAlloca)

	// Load key from keys vector
	keyDataBase := c.block.NewGetElementPtr(irtypes.I8, keysVec,
		constant.NewInt(irtypes.I64, int64(vectorHeaderSize)))
	keyDataPtr := c.block.NewBitCast(keyDataBase, irtypes.NewPointer(keyLLVM))
	keyElemPtr := c.block.NewGetElementPtr(keyLLVM, keyDataPtr, idx)
	var key value.Value = c.block.NewLoad(keyLLVM, keyElemPtr)

	// Load value from values vector
	valDataBase := c.block.NewGetElementPtr(irtypes.I8, valsVec,
		constant.NewInt(irtypes.I64, int64(vectorHeaderSize)))
	valDataPtr := c.block.NewBitCast(valDataBase, irtypes.NewPointer(valLLVM))
	valElemPtr := c.block.NewGetElementPtr(valLLVM, valDataPtr, idx)
	var val value.Value = c.block.NewLoad(valLLVM, valElemPtr)

	// B0343: Dup strings for independent ownership.
	if dupKeyStr {
		key = c.dupString(key)
	}
	if dupValStr {
		val = c.dupString(val)
	}

	if twoBindings {
		// Store key and value to separate allocas
		c.block.NewStore(key, c.locals[s.Index])
		c.block.NewStore(val, c.locals[s.Binding])
	} else {
		// Build and store (K, V) tuple
		tupleType := irtypes.NewStruct(keyLLVM, valLLVM)
		var tuple value.Value = constant.NewZeroInitializer(tupleType)
		tuple = c.block.NewInsertValue(tuple, key, 0)
		tuple = c.block.NewInsertValue(tuple, val, 1)
		c.block.NewStore(tuple, c.locals[s.Binding])
		// B0343: Store to hidden tracking allocas.
		if dupKeyStr {
			c.block.NewStore(key, keyStrAlloca)
		}
		if dupValStr {
			c.block.NewStore(val, valStrAlloca)
		}
	}

	// B0343: Set drop flags for dup'd strings.
	if dupKeyStr {
		c.block.NewStore(constant.NewInt(irtypes.I1, 1), c.dropFlags[keyDropName])
	}
	if dupValStr {
		c.block.NewStore(constant.NewInt(irtypes.I1, 1), c.dropFlags[valDropName])
	}

	c.genBlock(s.Body)
	if c.block.Term == nil {
		c.block.NewBr(updateBlock)
	}

	// Update: increment counter
	c.block = updateBlock
	curCount := c.block.NewLoad(irtypes.I64, counterAlloca)
	nextCount := c.block.NewAdd(curCount, constant.NewInt(irtypes.I64, 1))
	c.block.NewStore(nextCount, counterAlloca)
	c.emitYieldCheck()
	c.block.NewBr(headerBlock)

	c.breakTarget = savedBreak
	c.continueTarget = savedContinue
	c.loopScopeDepth = savedLoopUseDepth
	c.block = exitBlock

	// B0214: Drop the temporary keys and values vectors after the loop.
	// keys() and values() return freshly heap-allocated vectors that must be freed.
	// B0244: values() match-destructures Slot.Used(_, v), which deep-clones all
	// droppable values (strings dup'd, enums cloned, heap types cloned). The values
	// vector contains independent copies, so all element types must be dropped.
	vectorDropFn := c.funcs["Vector.drop"]
	c.emitVectorElementDropLoop(keysVec, keyType)
	c.block.NewCall(vectorDropFn, keysVec)
	c.emitVectorElementDropLoop(valsVec, valType)
	c.block.NewCall(vectorDropFn, valsVec)
}

// --- For-in over strings ---

func (c *Compiler) genForInString(s *ast.ForInStmt, strPtr value.Value) {
	// Alloca for byte position
	posAlloca := c.createEntryAlloca(irtypes.I64)
	c.block.NewStore(constant.NewInt(irtypes.I64, 0), posAlloca)

	// Index variable if present
	if s.Index != "" {
		indexAlloca := c.createEntryAlloca(irtypes.I64)
		indexAlloca.SetName(c.uniqueLocalName(s.Index))
		c.block.NewStore(constant.NewInt(irtypes.I64, 0), indexAlloca)
		c.locals[s.Index] = indexAlloca
	}

	headerBlock := c.newBlock("forin.str.header")
	bodyBlock := c.newBlock("forin.str.body")
	exitBlock := c.newBlock("forin.str.exit")

	c.block.NewBr(headerBlock)

	// Header: call promise_string_next_char, check for -1
	c.block = headerBlock
	cp := c.block.NewCall(c.funcs["promise_string_next_char"], strPtr, posAlloca)
	done := c.block.NewICmp(enum.IPredEQ, cp, constant.NewInt(irtypes.I32, -1))
	c.block.NewCondBr(done, exitBlock, bodyBlock)

	// Body: bind char to loop variable
	savedBreak := c.breakTarget
	savedContinue := c.continueTarget
	savedLoopUseDepth := c.loopScopeDepth
	c.breakTarget = exitBlock
	c.continueTarget = headerBlock
	c.loopScopeDepth = len(c.scopeBindings)

	c.block = bodyBlock
	alloca := c.createEntryAlloca(irtypes.I32)
	alloca.SetName(c.uniqueLocalName(s.Binding))
	c.block.NewStore(cp, alloca)
	c.locals[s.Binding] = alloca

	c.genBlock(s.Body)

	// Increment index after body, before looping back
	if s.Index != "" && c.block.Term == nil {
		idxAlloca := c.locals[s.Index]
		curIdx := c.block.NewLoad(irtypes.I64, idxAlloca)
		nextIdx := c.block.NewAdd(curIdx, constant.NewInt(irtypes.I64, 1))
		c.block.NewStore(nextIdx, idxAlloca)
	}

	if c.block.Term == nil {
		c.emitYieldCheck()
		c.block.NewBr(headerBlock)
	}

	c.breakTarget = savedBreak
	c.continueTarget = savedContinue
	c.loopScopeDepth = savedLoopUseDepth
	c.block = exitBlock
}

// genSelectStmt generates LLVM IR for a select statement.
// Implements Go-style lock-all-channels protocol:
// 1. Evaluate all channel expressions
// 2. Lock all channels sorted by address (deadlock prevention)
// 3. Check which cases can proceed (non-blocking)
// 4. If one can: execute it, unlock all
// 5. If none + default: unlock all, execute default
// 6. If none + no default: park on waiter lists, suspend, dispatch on wake
func (c *Compiler) genSelectStmt(s *ast.SelectStmt) {
	nCases := len(s.Cases)
	chanType := channelStructType()

	// Step 1: Evaluate channel expressions and gather info
	type selectCaseInfo struct {
		chRaw         value.Value
		chPtr         value.Value
		isSend        bool
		sendValueExpr ast.Expr
		binding       string
		elemLLVM      irtypes.Type
		elemSize      int64
	}

	caseInfos := make([]selectCaseInfo, nCases)
	for i, sc := range s.Cases {
		chRaw := c.genExpr(sc.Channel)
		chPtr := c.block.NewBitCast(chRaw, irtypes.NewPointer(chanType))

		semaType := c.info.Types[sc.Channel]
		inst := semaType.(*types.Instance)
		elemType := inst.TypeArgs()[0]
		elemLLVM := c.resolveType(elemType)
		elemSize := int64(c.typeSize(elemLLVM))

		caseInfos[i] = selectCaseInfo{
			chRaw:         chRaw,
			chPtr:         chPtr,
			isSend:        sc.IsSend,
			sendValueExpr: sc.SendValue,
			binding:       sc.Binding,
			elemLLVM:      elemLLVM,
			elemSize:      elemSize,
		}
	}

	// Step 2: Sort channel pointers by address and lock all.
	i8PtrTy := irtypes.I8Ptr
	arrType := irtypes.NewArray(uint64(nCases), i8PtrTy)
	chArr := c.createEntryAlloca(arrType)

	for i, ci := range caseInfos {
		ptr := c.block.NewGetElementPtr(arrType, chArr,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(i)))
		c.block.NewStore(ci.chRaw, ptr)
	}

	// Inline bubble sort by pointer address (for deadlock prevention)
	if nCases > 1 {
		for pass := 0; pass < nCases-1; pass++ {
			for j := 0; j < nCases-1-pass; j++ {
				ptrA := c.block.NewGetElementPtr(arrType, chArr,
					constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(j)))
				ptrB := c.block.NewGetElementPtr(arrType, chArr,
					constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(j+1)))
				valA := c.block.NewLoad(i8PtrTy, ptrA)
				valB := c.block.NewLoad(i8PtrTy, ptrB)
				intA := c.block.NewPtrToInt(valA, c.ptrIntType())
				intB := c.block.NewPtrToInt(valB, c.ptrIntType())
				needSwap := c.block.NewICmp(enum.IPredUGT, intA, intB)

				swapBlk := c.newBlock(fmt.Sprintf("select.sort.swap.%d.%d", pass, j))
				contBlk := c.newBlock(fmt.Sprintf("select.sort.cont.%d.%d", pass, j))
				c.block.NewCondBr(needSwap, swapBlk, contBlk)

				c.block = swapBlk
				c.block.NewStore(valB, ptrA)
				c.block.NewStore(valA, ptrB)
				c.block.NewBr(contBlk)

				c.block = contBlk
			}
		}
	}

	// Lock all channels in sorted order (skip duplicates).
	// lockStartBlk is the entry point for the retry loop when blocking select
	// yields and needs to re-lock + re-check all cases.
	lockStartBlk := c.newBlock("select.lock.start")
	c.block.NewBr(lockStartBlk)
	c.block = lockStartBlk

	for i := 0; i < nCases; i++ {
		ptr := c.block.NewGetElementPtr(arrType, chArr,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(i)))
		chRawSorted := c.block.NewLoad(i8PtrTy, ptr)
		chPtrSorted := c.block.NewBitCast(chRawSorted, irtypes.NewPointer(chanType))
		mtxPtr := c.block.NewGetElementPtr(chanType, chPtrSorted,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(chanFieldMutex)))
		mtx := c.block.NewLoad(i8PtrTy, mtxPtr)

		if i > 0 {
			// Skip if same channel as previous (avoid double-lock)
			prevPtr := c.block.NewGetElementPtr(arrType, chArr,
				constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(i-1)))
			prevRaw := c.block.NewLoad(i8PtrTy, prevPtr)
			isSame := c.block.NewICmp(enum.IPredEQ, chRawSorted, prevRaw)
			lockBlk := c.newBlock(fmt.Sprintf("select.lock.%d", i))
			skipBlk := c.newBlock(fmt.Sprintf("select.lock.skip.%d", i))
			c.block.NewCondBr(isSame, skipBlk, lockBlk)
			c.block = lockBlk
			c.block.NewCall(c.palMutexLock, mtx)
			c.block.NewBr(skipBlk)
			c.block = skipBlk
		} else {
			c.block.NewCall(c.palMutexLock, mtx)
		}
	}

	// Step 3: Try each case to see if it can proceed
	mergeBlk := c.newBlock("select.merge")
	caseExecBlks := make([]*ir.Block, nCases)
	for i := range nCases {
		caseExecBlks[i] = c.newBlock(fmt.Sprintf("select.case%d.exec", i))
	}

	// After trying all: default or park or merge
	var afterTryBlk *ir.Block
	var defaultBlk *ir.Block
	if s.Default != nil {
		defaultBlk = c.newBlock("select.default")
		afterTryBlk = defaultBlk
	} else if c.inCoroutine {
		afterTryBlk = c.newBlock("select.park")
	} else if !c.isWasm {
		// Non-coroutine context (e.g., batch tests): poll-retry fallback (B0045)
		afterTryBlk = c.newBlock("select.poll")
	} else {
		afterTryBlk = mergeBlk
	}

	// Generate try-check chain
	firstTryBlk := c.newBlock("select.try0")
	c.block.NewBr(firstTryBlk)
	c.block = firstTryBlk

	for i, ci := range caseInfos {
		var nextCheck *ir.Block
		if i+1 < nCases {
			nextCheck = c.newBlock(fmt.Sprintf("select.try%d", i+1))
		} else {
			nextCheck = afterTryBlk
		}

		if ci.isSend {
			countPtr := c.block.NewGetElementPtr(chanType, ci.chPtr,
				constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(chanFieldCount)))
			count := c.block.NewLoad(irtypes.I64, countPtr)
			capPtr := c.block.NewGetElementPtr(chanType, ci.chPtr,
				constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(chanFieldCapacity)))
			cap_ := c.block.NewLoad(irtypes.I64, capPtr)
			notFull := c.block.NewICmp(enum.IPredULT, count, cap_)
			closedPtr := c.block.NewGetElementPtr(chanType, ci.chPtr,
				constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(chanFieldClosed)))
			closedVal := c.block.NewLoad(irtypes.I8, closedPtr)
			isOpen := c.block.NewICmp(enum.IPredEQ, closedVal, constant.NewInt(irtypes.I8, 0))
			canSend := c.block.NewAnd(notFull, isOpen)
			c.block.NewCondBr(canSend, caseExecBlks[i], nextCheck)
		} else {
			countPtr := c.block.NewGetElementPtr(chanType, ci.chPtr,
				constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(chanFieldCount)))
			count := c.block.NewLoad(irtypes.I64, countPtr)
			hasItems := c.block.NewICmp(enum.IPredUGT, count, constant.NewInt(irtypes.I64, 0))
			closedPtr := c.block.NewGetElementPtr(chanType, ci.chPtr,
				constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(chanFieldClosed)))
			closedVal := c.block.NewLoad(irtypes.I8, closedPtr)
			isClosed := c.block.NewICmp(enum.IPredEQ, closedVal, constant.NewInt(irtypes.I8, 1))
			canRecv := c.block.NewOr(hasItems, isClosed)
			c.block.NewCondBr(canRecv, caseExecBlks[i], nextCheck)
		}

		if i+1 < nCases {
			c.block = nextCheck
		}
	}

	// Helper: generate unlock-all code
	unlockAll := func() {
		for j := nCases - 1; j >= 0; j-- {
			ptr := c.block.NewGetElementPtr(arrType, chArr,
				constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(j)))
			chRawSorted := c.block.NewLoad(i8PtrTy, ptr)

			if j < nCases-1 {
				// Skip if same as next (since we're going in reverse)
				nextPtr := c.block.NewGetElementPtr(arrType, chArr,
					constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(j+1)))
				nextRaw := c.block.NewLoad(i8PtrTy, nextPtr)
				isSame := c.block.NewICmp(enum.IPredEQ, chRawSorted, nextRaw)
				unlockBlk := c.newBlock(fmt.Sprintf("select.unlock.%d", j))
				skipBlk := c.newBlock(fmt.Sprintf("select.unlock.skip.%d", j))
				c.block.NewCondBr(isSame, skipBlk, unlockBlk)
				c.block = unlockBlk
				chPtrSorted := c.block.NewBitCast(chRawSorted, irtypes.NewPointer(chanType))
				mtxPtr := c.block.NewGetElementPtr(chanType, chPtrSorted,
					constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(chanFieldMutex)))
				mtx := c.block.NewLoad(i8PtrTy, mtxPtr)
				c.block.NewCall(c.palMutexUnlock, mtx)
				c.block.NewBr(skipBlk)
				c.block = skipBlk
			} else {
				chPtrSorted := c.block.NewBitCast(chRawSorted, irtypes.NewPointer(chanType))
				mtxPtr := c.block.NewGetElementPtr(chanType, chPtrSorted,
					constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(chanFieldMutex)))
				mtx := c.block.NewLoad(i8PtrTy, mtxPtr)
				c.block.NewCall(c.palMutexUnlock, mtx)
			}
		}
	}

	// Helper: generate send execution code for a case
	execSend := func(ci selectCaseInfo, prefix string) {
		argVal := c.genExpr(ci.sendValueExpr)
		// The send value's bits are memcpy'd into the channel buffer below,
		// transferring ownership. Ownership marks the send value Moved at the
		// select-send site (B0341 / T0784 ownership changes), so static
		// use-after-move is rejected — but ownership does not touch the runtime
		// drop flag. Without clearing it here, the source local's scope-exit
		// drop and the channel buffer both free the same allocation →
		// double-free / SEGV. Mirror genChannelSend, which clears both the bare
		// IdentExpr and the cast-subject cases.
		//
		// T0799: bare IdentExpr send (`select { ch.send(s): ... }` with no cast).
		if ident, ok := ci.sendValueExpr.(*ast.IdentExpr); ok {
			c.clearDropFlag(ident.Name)
		}
		// T0784: cast-of-borrow send (`select { ch.send(x as!/as T): ... }`) —
		// the cast is a view over an owned local with the same double-free shape.
		// T0849: for the conditional `as` form, drop iff the downcast failed.
		if ident := c.castSubjectMovableIdent(ci.sendValueExpr); ident != nil {
			c.consumeCastSubjectDropFlag(ci.sendValueExpr, ident.Name)
		}
		// T0799: a freshly produced send value (`select { ch.send("a" + "b"): ... }`)
		// is a tracked statement temp. Its bits are memcpy'd into the buffer below,
		// so ownership transfers to the channel — claim it (mirror genChannelSend's
		// B0170/B0233 claims) or cleanupStmtTemps would free the buffer copy at
		// select-statement end → use-after-free / double-free.
		c.claimStringTemp(argVal)
		c.claimHeapTemp(argVal)
		argAlloca := c.createEntryAlloca(ci.elemLLVM)
		c.block.NewStore(argVal, argAlloca)
		argAsI8 := c.block.NewBitCast(argAlloca, i8PtrTy)

		bufPtr := c.block.NewGetElementPtr(chanType, ci.chPtr,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(chanFieldBuffer)))
		buf := c.block.NewLoad(i8PtrTy, bufPtr)
		tailPtr := c.block.NewGetElementPtr(chanType, ci.chPtr,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(chanFieldTail)))
		tail := c.block.NewLoad(irtypes.I64, tailPtr)
		offset := c.block.NewMul(tail, constant.NewInt(irtypes.I64, ci.elemSize))
		dest := c.block.NewGetElementPtr(irtypes.I8, buf, offset)
		c.block.NewCall(c.funcs["llvm.memcpy"], dest, argAsI8,
			constant.NewInt(irtypes.I64, ci.elemSize), constant.False)

		capPtr := c.block.NewGetElementPtr(chanType, ci.chPtr,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(chanFieldCapacity)))
		cap_ := c.block.NewLoad(irtypes.I64, capPtr)
		tailPlusOne := c.block.NewAdd(tail, constant.NewInt(irtypes.I64, 1))
		newTail := c.block.NewURem(tailPlusOne, cap_)
		c.block.NewStore(newTail, tailPtr)

		countPtr := c.block.NewGetElementPtr(chanType, ci.chPtr,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(chanFieldCount)))
		countVal := c.block.NewLoad(irtypes.I64, countPtr)
		newCount := c.block.NewAdd(countVal, constant.NewInt(irtypes.I64, 1))
		c.block.NewStore(newCount, countPtr)

		// Wake a waiting receiver (handles both regular G and select SWN nodes)
		recvHeadPtr := c.block.NewGetElementPtr(chanType, ci.chPtr,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(chanFieldRecvWaitersHead)))
		recvTailPtr := c.block.NewGetElementPtr(chanType, ci.chPtr,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(chanFieldRecvWaitersTail)))
		nePtr := c.block.NewGetElementPtr(chanType, ci.chPtr,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(chanFieldNotEmpty)))
		ne := c.block.NewLoad(i8PtrTy, nePtr)
		c.block.NewCall(c.funcs["promise_waiter_wake_one"], recvHeadPtr, recvTailPtr, ne)
	}

	// Helper: generate recv execution code for a case
	execRecv := func(ci selectCaseInfo, prefix string) {
		optType := irtypes.NewStruct(irtypes.I1, ci.elemLLVM)
		countPtr := c.block.NewGetElementPtr(chanType, ci.chPtr,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(chanFieldCount)))
		count := c.block.NewLoad(irtypes.I64, countPtr)
		isEmpty := c.block.NewICmp(enum.IPredEQ, count, constant.NewInt(irtypes.I64, 0))

		noneBlk := c.newBlock(prefix + ".none")
		readBlk := c.newBlock(prefix + ".read")
		doneBlk := c.newBlock(prefix + ".done")
		c.block.NewCondBr(isEmpty, noneBlk, readBlk)

		c.block = noneBlk
		noneVal := constant.NewZeroInitializer(optType)
		c.block.NewBr(doneBlk)

		c.block = readBlk
		bufPtr := c.block.NewGetElementPtr(chanType, ci.chPtr,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(chanFieldBuffer)))
		buf := c.block.NewLoad(i8PtrTy, bufPtr)
		headPtr := c.block.NewGetElementPtr(chanType, ci.chPtr,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(chanFieldHead)))
		head := c.block.NewLoad(irtypes.I64, headPtr)
		offset := c.block.NewMul(head, constant.NewInt(irtypes.I64, ci.elemSize))
		src := c.block.NewGetElementPtr(irtypes.I8, buf, offset)
		rAlloca := c.createEntryAlloca(ci.elemLLVM)
		rAsI8 := c.block.NewBitCast(rAlloca, i8PtrTy)
		c.block.NewCall(c.funcs["llvm.memcpy"], rAsI8, src,
			constant.NewInt(irtypes.I64, ci.elemSize), constant.False)
		rVal := c.block.NewLoad(ci.elemLLVM, rAlloca)

		capPtr := c.block.NewGetElementPtr(chanType, ci.chPtr,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(chanFieldCapacity)))
		cap_ := c.block.NewLoad(irtypes.I64, capPtr)
		headPlusOne := c.block.NewAdd(head, constant.NewInt(irtypes.I64, 1))
		newHead := c.block.NewURem(headPlusOne, cap_)
		c.block.NewStore(newHead, headPtr)

		countRead := c.block.NewLoad(irtypes.I64, countPtr)
		newCount := c.block.NewSub(countRead, constant.NewInt(irtypes.I64, 1))
		c.block.NewStore(newCount, countPtr)

		// Wake a waiting sender (handles both regular G and select SWN nodes)
		sendHeadPtr := c.block.NewGetElementPtr(chanType, ci.chPtr,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(chanFieldSendWaitersHead)))
		sendTailPtr := c.block.NewGetElementPtr(chanType, ci.chPtr,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(chanFieldSendWaitersTail)))
		nfPtr := c.block.NewGetElementPtr(chanType, ci.chPtr,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(chanFieldNotFull)))
		nf := c.block.NewLoad(i8PtrTy, nfPtr)
		c.block.NewCall(c.funcs["promise_waiter_wake_one"], sendHeadPtr, sendTailPtr, nf)

		// Wake a rendezvous-parked sender (T0312)
		rvSendHeadPtr := c.block.NewGetElementPtr(chanType, ci.chPtr,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(chanFieldRvWaitersHead)))
		rvSendTailPtr := c.block.NewGetElementPtr(chanType, ci.chPtr,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(chanFieldRvWaitersTail)))
		c.block.NewCall(c.funcs["promise_waiter_wake_one"], rvSendHeadPtr, rvSendTailPtr, nf)

		someVal := c.block.NewInsertValue(constant.NewZeroInitializer(optType), constant.True, 0)
		someVal2 := c.block.NewInsertValue(someVal, rVal, 1)
		someBlk := c.block // capture for phi predecessor
		c.block.NewBr(doneBlk)

		c.block = doneBlk
		recvPhi := c.block.NewPhi(
			&ir.Incoming{X: noneVal, Pred: noneBlk},
			&ir.Incoming{X: someVal2, Pred: someBlk},
		)

		if ci.binding != "_" {
			alloca := c.createEntryAlloca(optType)
			alloca.SetName(c.uniqueLocalName(ci.binding))
			c.block.NewStore(recvPhi, alloca)
			c.locals[ci.binding] = alloca
		}
	}

	// Step 4: Generate case execution blocks (non-blocking path)
	for i, ci := range caseInfos {
		c.block = caseExecBlks[i]
		savedScopeLen := len(c.scopeBindings)

		prefix := fmt.Sprintf("select.c%d", i)
		if ci.isSend {
			execSend(ci, prefix)
		} else {
			execRecv(ci, prefix)
		}

		unlockAll()

		for _, stmt := range s.Cases[i].Body {
			if c.block.Term != nil {
				break
			}
			c.genStmt(stmt)
		}
		if c.block != nil && c.block.Term == nil && len(c.scopeBindings) > savedScopeLen {
			cap := c.emitScopeCleanup(savedScopeLen, false)
			c.emitCloseErrCheck(cap)
		}
		c.scopeBindings = c.scopeBindings[:savedScopeLen]
		if c.block != nil && c.block.Term == nil {
			c.block.NewBr(mergeBlk)
		}
	}

	// Step 5: Default block
	if defaultBlk != nil {
		c.block = defaultBlk
		savedScopeLen := len(c.scopeBindings)
		unlockAll()
		for _, stmt := range s.Default {
			if c.block.Term != nil {
				break
			}
			c.genStmt(stmt)
		}
		if c.block != nil && c.block.Term == nil && len(c.scopeBindings) > savedScopeLen {
			cap := c.emitScopeCleanup(savedScopeLen, false)
			c.emitCloseErrCheck(cap)
		}
		c.scopeBindings = c.scopeBindings[:savedScopeLen]
		if c.block != nil && c.block.Term == nil {
			c.block.NewBr(mergeBlk)
		}
	}

	// Step 6: Blocking select (no default, coroutine mode) — waiter-list parking.
	// Uses SelectWaiterNode (SWN) entries that are layout-compatible with G at
	// fields 0-4, allowing them to coexist on channel waiter lists. A per-select
	// mutex (select_mutex) prevents enqueue-before-suspend races and provides
	// wake-once semantics via G.select_case CAS under the mutex.
	//
	// Protocol:
	//   1. Create select_mutex, lock it
	//   2. Set G.select_case = -1
	//   3. Store select_mutex in G.park_mutex (BEFORE enqueue — prevents race
	//      where a waker dequeues SWN and reads G.park_mutex before we set it)
	//   4. For each case: alloca SWN, init, enqueue on channel's waiter list
	//   5. Unlock all channel mutexes
	//   6. coro.suspend → scheduler unlocks select_mutex (via park_mutex)
	//   7. Channel wake code dequeues SWN, calls select_try_wake (wake-once)
	//   8. On resume: lock all channels, remove remaining SWNs, dispatch on G.select_case
	if s.Default == nil && c.inCoroutine {
		c.block = afterTryBlk

		gTy := goroutineStructType()
		swnTy := selectWaiterNodeType()
		currentG := c.block.NewLoad(i8PtrTy, c.currentGGlobal)
		gTyped := c.block.NewBitCast(currentG, irtypes.NewPointer(gTy))

		// 1. Create select_mutex and lock it
		selectMtx := c.block.NewCall(c.palMutexInit)
		c.block.NewCall(c.palMutexLock, selectMtx)

		// 2. Set G.select_case = -1 (unclaimed)
		scField := c.block.NewGetElementPtr(gTy, gTyped,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(gFieldSelectCase)))
		neg1 := constant.NewInt(irtypes.I32, 0xFFFFFFFF) // -1 as unsigned i32
		c.block.NewStore(neg1, scField)

		// 3. Store select_mutex in G.park_mutex BEFORE enqueueing SWNs.
		// This ensures that any waker that dequeues an SWN will see a valid
		// select_mutex in G.park_mutex (not null). The select_mutex is locked,
		// so the waker blocks in select_try_wake until the scheduler unlocks it
		// after coro.suspend — preventing the enqueue-before-suspend race.
		pmField := c.block.NewGetElementPtr(gTy, gTyped,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(gFieldParkMutex)))
		c.block.NewStore(selectMtx, pmField)

		// 4. For each case: alloca SWN, init, enqueue on channel's waiter list
		swnAllocas := make([]value.Value, nCases)
		for i, ci := range caseInfos {
			swn := c.createEntryAlloca(swnTy)
			swnAllocas[i] = swn

			// Initialize SWN fields. Fields 0,2,3 are padding (set to null).
			// Field 4 (next) is set to null by select_waiter_enqueue.
			for _, padIdx := range []int64{0, 2, 3} {
				padF := c.block.NewGetElementPtr(swnTy, swn,
					constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, padIdx))
				c.block.NewStore(constant.NewNull(i8PtrTy), padF)
			}
			// field 1 (kind) = 0xFF sentinel
			kindF := c.block.NewGetElementPtr(swnTy, swn,
				constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 1))
			c.block.NewStore(constant.NewInt(irtypes.I8, swnKindSentinel), kindF)
			// field 5 (g) = currentG
			gF := c.block.NewGetElementPtr(swnTy, swn,
				constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(swnFieldG)))
			c.block.NewStore(currentG, gF)
			// field 6 (case_index) = i
			ciF := c.block.NewGetElementPtr(swnTy, swn,
				constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(swnFieldCaseIndex)))
			c.block.NewStore(constant.NewInt(irtypes.I32, int64(i)), ciF)
			// field 7 (select_mutex) = selectMtx
			smF := c.block.NewGetElementPtr(swnTy, swn,
				constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(swnFieldSelectMutex)))
			c.block.NewStore(selectMtx, smF)

			// Enqueue SWN on the appropriate channel waiter list
			swnRaw := c.block.NewBitCast(swn, i8PtrTy)
			if ci.isSend {
				headPtr := c.block.NewGetElementPtr(chanType, ci.chPtr,
					constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(chanFieldSendWaitersHead)))
				tailPtr := c.block.NewGetElementPtr(chanType, ci.chPtr,
					constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(chanFieldSendWaitersTail)))
				c.block.NewCall(c.funcs["promise_select_waiter_enqueue"], headPtr, tailPtr, swnRaw)
			} else {
				headPtr := c.block.NewGetElementPtr(chanType, ci.chPtr,
					constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(chanFieldRecvWaitersHead)))
				tailPtr := c.block.NewGetElementPtr(chanType, ci.chPtr,
					constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(chanFieldRecvWaitersTail)))
				c.block.NewCall(c.funcs["promise_select_waiter_enqueue"], headPtr, tailPtr, swnRaw)
			}
		}

		// 5. Unlock all channel mutexes
		unlockAll()

		// 6. coro.suspend — G.park_mutex already set (step 3), scheduler unlocks after suspend
		suspResult := c.block.NewCall(c.coroSuspend, constant.None, constant.False)
		resumeBlk := c.newBlock("select.resume")
		c.block.NewSwitch(suspResult, c.coroSuspendBlk,
			ir.NewCase(constant.NewInt(irtypes.I8, 0), resumeBlk),
			ir.NewCase(constant.NewInt(irtypes.I8, 1), c.coroCleanupBlk))

		// 8. On resume: lock all channels, remove SWNs, dispatch on G.select_case
		c.block = resumeBlk

		// Re-lock all channels in sorted order (same code as lockStartBlk but inline)
		for i := 0; i < nCases; i++ {
			ptr := c.block.NewGetElementPtr(arrType, chArr,
				constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(i)))
			chRawSorted := c.block.NewLoad(i8PtrTy, ptr)
			chPtrSorted := c.block.NewBitCast(chRawSorted, irtypes.NewPointer(chanType))
			mtxPtr := c.block.NewGetElementPtr(chanType, chPtrSorted,
				constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(chanFieldMutex)))
			mtx := c.block.NewLoad(i8PtrTy, mtxPtr)

			if i > 0 {
				prevPtr := c.block.NewGetElementPtr(arrType, chArr,
					constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(i-1)))
				prevRaw := c.block.NewLoad(i8PtrTy, prevPtr)
				isSame := c.block.NewICmp(enum.IPredEQ, chRawSorted, prevRaw)
				lockBlk := c.newBlock(fmt.Sprintf("select.wake.lock.%d", i))
				skipBlk := c.newBlock(fmt.Sprintf("select.wake.lock.skip.%d", i))
				c.block.NewCondBr(isSame, skipBlk, lockBlk)
				c.block = lockBlk
				c.block.NewCall(c.palMutexLock, mtx)
				c.block.NewBr(skipBlk)
				c.block = skipBlk
			} else {
				c.block.NewCall(c.palMutexLock, mtx)
			}
		}

		// Remove all SWNs from channel waiter lists (cleanup)
		for i, ci := range caseInfos {
			swnRaw := c.block.NewBitCast(swnAllocas[i], i8PtrTy)
			if ci.isSend {
				headPtr := c.block.NewGetElementPtr(chanType, ci.chPtr,
					constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(chanFieldSendWaitersHead)))
				tailPtr := c.block.NewGetElementPtr(chanType, ci.chPtr,
					constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(chanFieldSendWaitersTail)))
				c.block.NewCall(c.funcs["promise_waiter_remove"], headPtr, tailPtr, swnRaw)
			} else {
				headPtr := c.block.NewGetElementPtr(chanType, ci.chPtr,
					constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(chanFieldRecvWaitersHead)))
				tailPtr := c.block.NewGetElementPtr(chanType, ci.chPtr,
					constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(chanFieldRecvWaitersTail)))
				c.block.NewCall(c.funcs["promise_waiter_remove"], headPtr, tailPtr, swnRaw)
			}
		}

		// Destroy select_mutex — no longer needed after SWN cleanup.
		// All channel mutexes are held, so no concurrent select_try_wake can
		// be in progress. The scheduler already unlocked it after suspend.
		c.block.NewCall(c.palMutexDestroy, selectMtx)

		// Read G.select_case to determine which case won
		wonCase := c.block.NewLoad(irtypes.I32, scField)

		// Generate wake-path case execution blocks
		// Each block: execute the send/recv, unlock all, run body, branch to merge
		wakeCaseBlks := make([]*ir.Block, nCases)
		var switchCases []*ir.Case
		for i := range nCases {
			wakeCaseBlks[i] = c.newBlock(fmt.Sprintf("select.wake.case%d", i))
			switchCases = append(switchCases, ir.NewCase(
				constant.NewInt(irtypes.I32, int64(i)), wakeCaseBlks[i]))
		}

		// Default for switch: unreachable (select_case must be a valid index)
		unreachableBlk := c.newBlock("select.wake.unreachable")
		c.block.NewSwitch(wonCase, unreachableBlk, switchCases...)
		unreachableBlk.NewUnreachable()

		// B0110: Create a retry block for wake-path send cases whose
		// send condition is no longer valid. Between the wake (receiver
		// drains a slot) and re-locking channels, another sender may
		// have filled the freed slot. When this happens, unlock all
		// channels and retry from the lock+try-check chain.
		wakeRetryBlk := c.newBlock("select.wake.retry")
		c.block = wakeRetryBlk
		unlockAll()
		c.block.NewBr(lockStartBlk)

		for i, ci := range caseInfos {
			c.block = wakeCaseBlks[i]
			savedScopeLen := len(c.scopeBindings)

			prefix := fmt.Sprintf("select.wk%d", i)
			if ci.isSend {
				// B0110: Re-check send condition after wake — between wake
				// and re-lock, another sender may have filled the freed slot.
				countPtr := c.block.NewGetElementPtr(chanType, ci.chPtr,
					constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(chanFieldCount)))
				count := c.block.NewLoad(irtypes.I64, countPtr)
				capPtr := c.block.NewGetElementPtr(chanType, ci.chPtr,
					constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(chanFieldCapacity)))
				cap_ := c.block.NewLoad(irtypes.I64, capPtr)
				notFull := c.block.NewICmp(enum.IPredULT, count, cap_)
				closedPtr := c.block.NewGetElementPtr(chanType, ci.chPtr,
					constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(chanFieldClosed)))
				closedVal := c.block.NewLoad(irtypes.I8, closedPtr)
				isOpen := c.block.NewICmp(enum.IPredEQ, closedVal, constant.NewInt(irtypes.I8, 0))
				canSend := c.block.NewAnd(notFull, isOpen)
				sendOkBlk := c.newBlock(prefix + ".send.ok")
				c.block.NewCondBr(canSend, sendOkBlk, wakeRetryBlk)
				c.block = sendOkBlk
				execSend(ci, prefix)
			} else {
				execRecv(ci, prefix)
			}

			unlockAll()

			for _, stmt := range s.Cases[i].Body {
				if c.block.Term != nil {
					break
				}
				c.genStmt(stmt)
			}
			if c.block != nil && c.block.Term == nil && len(c.scopeBindings) > savedScopeLen {
				cap := c.emitScopeCleanup(savedScopeLen, false)
				c.emitCloseErrCheck(cap)
			}
			c.scopeBindings = c.scopeBindings[:savedScopeLen]
			if c.block != nil && c.block.Term == nil {
				c.block.NewBr(mergeBlk)
			}
		}

	}

	// Thread-blocking poll fallback for non-coroutine context (B0045).
	// When no case is immediately ready and we can't park (not a coroutine),
	// unlock all channels, yield to let goroutines make progress, then
	// re-lock and retry the try-check chain.
	if s.Default == nil && !c.inCoroutine && !c.isWasm {
		c.block = afterTryBlk
		unlockAll()
		c.block.NewCall(c.palUsleep, constant.NewInt(irtypes.I32, 100))
		c.block.NewBr(lockStartBlk)
	}

	c.block = mergeBlk
}
