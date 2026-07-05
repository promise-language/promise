package ownership

import (
	"fmt"

	"github.com/promise-language/promise/compiler/internal/ast"
	"github.com/promise-language/promise/compiler/internal/sema"
	"github.com/promise-language/promise/compiler/internal/types"
)

// checkExpr recursively walks an expression, checking for use-after-move
// at every variable reference.
func (c *Checker) checkExpr(expr ast.Expr) {
	if expr == nil {
		return
	}
	switch e := expr.(type) {
	case *ast.IdentExpr:
		c.checkIdentUse(e)

	case *ast.ThisExpr:
		if c.state["this"] == Moved {
			c.errorf(e.Pos(), "use of moved variable 'this'")
		}

	case *ast.CallExpr:
		c.checkCallExpr(e)

	case *ast.BinaryExpr:
		c.checkExpr(e.Left)
		c.checkExpr(e.Right)
		// T0936: elvis `?:` consumes both operands (move-at-ownership). The some-path
		// moves the optional's inner out (codegen clears its drop flag); the none-path
		// moves the default into the result (codegen neutralizes the default's own
		// owner). Both are statically consumed, so reusing either afterward is a
		// use-after-move. Mark after checking BOTH operands so the two reads inside one
		// elvis aren't falsely flagged against each other. Only ident operands carry
		// move state (tryMove no-ops on non-idents and Copy types — matching genElvis,
		// which only clears drop flags on ident operands).
		if e.Op == ast.BinElvis {
			if _, ok := e.Left.(*ast.IdentExpr); ok {
				c.tryMove(e.Left)
			}
			if _, ok := e.Right.(*ast.IdentExpr); ok {
				c.tryMove(e.Right)
			}
		}

	case *ast.UnaryExpr:
		c.checkExpr(e.Operand)
		// T0837/T0953: `<-` (await) is a *consuming* op on a Task — genReceiveTask
		// joins the goroutine and frees the G struct + result buffer. Awaiting a Task
		// the current function does not own (a borrowed ident, or a single-owner
		// handle field read out of a borrowed/transient owner) double-joins/double-
		// frees with the real owner's drop → SEGV. The receive does not flow through
		// tryMove/tryMoveConsume, so reject the out-of-borrow consume here.
		// rejectBorrowedTaskAwait walks every transparent wrapper the receive surfaces
		// the Task through (paren, force-unwrap, elvis, if/else, match). No-op for
		// channel receives — the IsTask gate keeps them legal (Channel `<-` is
		// non-consuming).
		if e.Op == ast.UnaryReceive {
			c.rejectBorrowedTaskAwait(e.Operand)
		}

	case *ast.IndexExpr:
		c.checkExpr(e.Target)
		c.checkExpr(e.Index)
		for _, extra := range e.ExtraIndices {
			c.checkExpr(extra)
		}

	case *ast.SliceExpr:
		c.checkExpr(e.Target)
		if e.Low != nil {
			c.checkExpr(e.Low)
		}
		if e.High != nil {
			c.checkExpr(e.High)
		}

	case *ast.SliceTypeExpr:
		c.checkExpr(e.Inner)

	case *ast.MemberExpr:
		c.checkExpr(e.Target)

	case *ast.OptionalChainExpr:
		c.checkExpr(e.Target)

	case *ast.IsExpr:
		c.checkExpr(e.Expr)

	case *ast.CastExpr:
		c.checkExpr(e.Expr)

	case *ast.ErrorPropagateExpr:
		c.checkExpr(e.Expr)

	case *ast.ErrorPanicExpr:
		c.checkExpr(e.Expr)

	case *ast.OptionalUnwrapExpr:
		c.checkExpr(e.Expr)

	case *ast.AutoCloneExpr:
		// T0605: synth-only deep-clone of `this.field` — a read/borrow, not a
		// move. The original stays owned by the clone() receiver.
		c.checkExpr(e.Expr)

	case *ast.ErrorHandlerExpr:
		c.checkExpr(e.Expr)
		if e.Binding != "" && e.Binding != "_" {
			c.state[e.Binding] = Owned
		}
		c.checkBlock(e.Body)

	case *ast.IfExpr:
		c.checkIfExpr(e)

	case *ast.MatchExpr:
		c.checkMatchExpr(e)

	case *ast.GoExpr:
		if e.Expr != nil {
			c.checkExpr(e.Expr)
			if ce, ok := e.Expr.(*ast.CallExpr); ok {
				c.rejectGoCallLoopBindingBorrowEscape(ce)
			}
		}
		if e.Block != nil {
			// T1151: a var-decl inside a `go { … }` block is owned by the
			// goroutine frame, not iteration-bounded — reset loop depth so such
			// locals are not flagged even when the go-block is lexically in a loop.
			savedLoopDepth := c.loopDepth
			c.loopDepth = 0
			c.checkBlock(e.Block)
			c.loopDepth = savedLoopDepth
		}

	case *ast.UnsafeExpr:
		c.inUnsafe++
		c.checkBlock(e.Body)
		c.inUnsafe--

	case *ast.LambdaExpr:
		c.checkLambdaExpr(e)

	case *ast.ParenExpr:
		c.checkExpr(e.Expr)

	case *ast.TupleLit:
		// T0338: collection literals consume their elements into a new
		// container that drops them — must reject borrowed params.
		for _, elem := range e.Elements {
			c.checkExpr(elem)
			c.tryMoveConsume(elem)
			c.tryMoveConsumeCastSubject(elem) // T0784
			// T1073: `(.., o!, ..)` — force-unwrap of a borrowed droppable
			// Optional param. The cast form is handled above; reject the
			// force-unwrap shape here.
			if isForceUnwrapForm(elem) {
				c.rejectBorrowedOptionalUnwrapConsume(elem)
			}
		}

	case *ast.ArrayLit:
		for _, elem := range e.Elements {
			c.checkExpr(elem)
			c.tryMoveConsume(elem)
			c.tryMoveConsumeCastSubject(elem) // T0784
			// T1073: `[.., o!, ..]` — force-unwrap of a borrowed droppable
			// Optional param. The cast form is handled above; reject the
			// force-unwrap shape here.
			if isForceUnwrapForm(elem) {
				c.rejectBorrowedOptionalUnwrapConsume(elem)
			}
		}

	case *ast.MapLit:
		for _, entry := range e.Entries {
			c.checkExpr(entry.Key)
			c.tryMoveConsume(entry.Key)
			c.tryMoveConsumeCastSubject(entry.Key) // T0784
			// T1073: force-unwrap of a borrowed droppable Optional param in a
			// map key. The cast form is handled above; reject force-unwrap.
			if isForceUnwrapForm(entry.Key) {
				c.rejectBorrowedOptionalUnwrapConsume(entry.Key)
			}
			c.checkExpr(entry.Value)
			c.tryMoveConsume(entry.Value)
			c.tryMoveConsumeCastSubject(entry.Value) // T0784
			// T1073: same for a map value.
			if isForceUnwrapForm(entry.Value) {
				c.rejectBorrowedOptionalUnwrapConsume(entry.Value)
			}
		}

	case *ast.StringLit:
		// Interpolated strings ("{x}") hold real sub-expressions in their parts;
		// walk each so reads of moved-from variables are detected (T1135). A
		// non-interpolated literal has only StringText/StringEscape parts and is
		// inert. Interpolation borrows its operands to format them (codegen writes
		// into a builder without taking ownership), so this only read-checks via
		// checkExpr — it must NOT consume.
		for _, part := range e.Parts {
			if interp, ok := part.(ast.StringInterp); ok {
				c.checkExpr(interp.Expr)
			}
		}

	// Literals have no sub-expressions to walk.
	case *ast.IntLit, *ast.FloatLit, *ast.BoolLit,
		*ast.NoneLit, *ast.CharLit:
	}
}

// checkIdentUse checks whether a variable reference uses a moved value.
func (c *Checker) checkIdentUse(e *ast.IdentExpr) {
	obj := c.info.Objects[e]
	if obj == nil {
		return
	}
	if _, isVar := obj.(*types.Var); !isVar {
		return
	}
	if c.state[e.Name] == Moved {
		c.errorf(e.Pos(), "use of moved variable '%s'", e.Name)
	}
}

// unwrapGoExpr peels ParenExpr wrappers and returns the *ast.GoExpr, or nil if
// expr is not (a paren-wrapped) `go` expression. T1152.
func unwrapGoExpr(expr ast.Expr) *ast.GoExpr {
	g, _ := unwrapDestructureParens(expr).(*ast.GoExpr)
	return g
}

// goCallBorrowsOwnedLocal reports the bare-ident argument that a `go f(arg)`
// call borrows from an owned, non-Copy, droppable, function/block-scope LOCAL —
// the shape whose Task handle, if it escapes the local's scope, reads the local
// after it drops (T1152). Iteration-bounded for-in bindings (T1147) and loop-body
// locals (T1151) are deliberately EXCLUDED here: those can never be safely
// borrowed into a `go` call (the goroutine always outlives the iteration) and are
// rejected outright at the call site by rejectGoCallLoopBindingBorrowEscape, so
// re-flagging them here would double-report.
//
// Returns nil (sound / out of scope) for: the `go { block }` form; an explicit
// `move` arg or a `~` (RefMut) move param (value transferred into the goroutine
// frame — T1148/T1098 territory); non-ident args (`.clone()`, constructor,
// temporary — already sound); Copy or non-droppable args; parameters
// (caller-owned — separate sibling gap); iteration-bounded loop bindings/locals
// (handled by T1147/T1151); and Borrowed/Moved/untracked roots.
func (c *Checker) goCallBorrowsOwnedLocal(g *ast.GoExpr) *ast.IdentExpr {
	if g == nil || g.Expr == nil {
		return nil
	}
	ce, ok := g.Expr.(*ast.CallExpr)
	if !ok {
		return nil
	}
	sig := c.calleeSignature(ce.Callee)
	if sig == nil {
		return nil
	}
	params := sig.Params()
	for i, arg := range ce.Args {
		if i >= len(params) {
			break
		}
		if arg.Move || params[i].Ref() == types.RefMut {
			continue // move into the goroutine frame — not a borrow
		}
		ident, ok := unwrapDestructureParens(arg.Value).(*ast.IdentExpr)
		if !ok {
			continue
		}
		if c.params[ident.Name] {
			continue // parameter, not a local (caller-owned)
		}
		if c.forInOwnedDroppableBindings[ident.Name] {
			continue // iteration-bounded — rejected by T1147/T1151 at the call site
		}
		if c.state[ident.Name] != Owned {
			continue // Borrowed / Moved / untracked
		}
		typ := c.info.Types[arg.Value]
		if typ == nil || isCopyType(typ) || !isDroppableType(typ) {
			continue
		}
		return ident
	}
	return nil
}

// goHandleEscapeMsg is the borrow-escape diagnostic for a `go` task handle that
// would outlive the local it borrows. It points at the three sound rewrites:
// clone into the goroutine, pass an owned value with `move`, or await before the
// local drops. T1152.
func goHandleEscapeMsg(local string) string {
	return "cannot let a 'go' task handle escape the scope of borrowed local '" + local +
		"'; the goroutine may read '" + local + "' after it is dropped — clone it into the goroutine ('" +
		local + ".clone()'), pass an owned value with 'move', or await the handle ('<-') before '" +
		local + "' goes out of scope"
}

// rejectGoHandleEscapeExpr errors and returns true when expr lets a `go` task
// handle borrow escape: either an inline `go f(local)` temporary, or an ident
// bound to such a handle (tracked in goHandleBorrowedLocal). Called at the top
// of tryMove/tryMoveConsume so every consume, store, and return site is covered
// uniformly. T1152.
func (c *Checker) rejectGoHandleEscapeExpr(expr ast.Expr) bool {
	if g := unwrapGoExpr(expr); g != nil {
		if bl := c.goCallBorrowsOwnedLocal(g); bl != nil {
			c.errorf(bl.Pos(), "%s", goHandleEscapeMsg(bl.Name))
			return true
		}
		return false
	}
	if ident, ok := expr.(*ast.IdentExpr); ok {
		if local, tracked := c.goHandleBorrowedLocal[ident.Name]; tracked {
			c.errorf(ident.Pos(), "%s", goHandleEscapeMsg(local))
			return true
		}
	}
	return false
}

// tryMove marks the variable referenced by expr as Moved, if it is a
// non-copy variable tracked in the current state. Also checks that the
// variable is not actively borrowed. Borrowed parameters are not moved —
// reads stay legal — but consuming contexts (call to `~` param, etc.) must
// use tryMoveConsume to enforce the T0338 check.
func (c *Checker) tryMove(expr ast.Expr) {
	// T1152: reject an escaping `go f(&local)` task handle (inline temporary or
	// a tracked handle binding) before any other move bookkeeping.
	if c.rejectGoHandleEscapeExpr(expr) {
		return
	}
	// T0837: reject moving a single-owner native handle field out of a shared
	// borrow before the MemberExpr branch (this helper peels paren/unwrap
	// wrappers, so `borrowed.field!` is caught here where checkFieldMoveOwnership
	// — which only sees a bare MemberExpr — would miss it).
	if c.rejectMemberHandleMoveOutOfBorrow(expr) {
		return
	}

	// B0341: check field reads from droppable owners before ident handling.
	if member, ok := expr.(*ast.MemberExpr); ok {
		c.checkFieldMoveOwnership(member)
		return
	}

	// `this` is a parameter, not a regular tracked variable; no move bookkeeping.
	// T0548: still reject moves of `this` while a borrow on it is active
	// (e.g., from `(b, n) := this.pair` registering Origin:"this").
	// NOTE: `return this` is intentionally allowed here — codegen's
	// wrapThisReturnValue wraps the i8* into the correct value struct type, and
	// maybeClearReceiverDropFlag prevents double-free (T0576/B0250).
	// `use x := this` is guarded separately in checkStmt (T0593).
	if this, ok := expr.(*ast.ThisExpr); ok {
		if c.borrows != nil && c.borrows.HasAnyBorrow("this") {
			c.errorf(this.Pos(), "cannot move 'this' while it is borrowed")
		}
		return
	}

	// T0596: reject `arr[i]` / `v[i]` when the slot type is a single-owner
	// native handle (Mutex/MutexGuard/Task). Dup-on-read is not defined for
	// these — extracting would alias the slot's owned pointer and the
	// container's drop walks both copies → double-free / SEGV.
	if c.rejectIndexExprSingleOwnerMove(expr) {
		return
	}

	// T0652: reject moves of a for-in loop binding whose iterable element is a
	// single-owner native handle (sibling of T0596 for the loop-binding shape).
	if c.rejectForInSingleOwnerBindingMove(expr) {
		return
	}

	ident, ok := expr.(*ast.IdentExpr)
	if !ok {
		return
	}
	obj := c.info.Objects[ident]
	if obj == nil {
		return
	}
	v, ok := obj.(*types.Var)
	if !ok {
		return
	}
	if isCopyType(v.Type()) {
		return
	}
	if _, tracked := c.state[ident.Name]; !tracked {
		return
	}
	// Cannot move use-bound variables (they need close() at scope exit)
	if c.pinned[ident.Name] {
		c.errorf(ident.Pos(), "cannot move use-bound variable '%s'", ident.Name)
		return
	}
	// Borrowed parameters stay borrowed — their state is unchanged by
	// tryMove. The T0338 check fires at consuming sites via tryMoveConsume.
	if c.state[ident.Name] == Borrowed {
		return
	}
	// Cannot move while borrowed
	if c.borrows != nil && c.borrows.HasAnyBorrow(ident.Name) {
		c.errorf(ident.Pos(), "cannot move '%s' while it is borrowed", ident.Name)
	}
	c.state[ident.Name] = Moved
}

// tryMoveConsume is like tryMove but enforces the T0338 check: the value
// being moved must not be a borrowed parameter, since the caller still owns
// it and will drop it at scope exit. Used at sites that genuinely consume
// (e.g., passing to a `~` callee parameter).
func (c *Checker) tryMoveConsume(expr ast.Expr) {
	// T1152: reject an escaping `go f(&local)` task handle (inline temporary or
	// a tracked handle binding) before any other move bookkeeping.
	if c.rejectGoHandleEscapeExpr(expr) {
		return
	}
	// T0407: any expression whose static type is `T&`/`T~` (non-Copy) is a
	// non-owning reference produced by Arc.borrow, MutexGuard.borrow, or any
	// composition through if/match/paren that preserves the borrow type.
	// Moving it into a consume site transfers ownership of the inner pointer
	// while the parent Arc/Mutex retains its drop responsibility → UAF on the
	// next read or owner drop. The type-driven check supplants the AST-shape
	// recursion T0377/T0380/T0381/T0401 needed: a single check covers all
	// current and future wrapping shapes uniformly, since sema propagates
	// `T&` through expression composition.
	if c.isBorrowedExpr(expr) {
		c.errorf(expr.Pos(),
			"cannot move out of '.borrow' getter; the parent Arc/Mutex retains ownership — call .clone() to create an independent copy, or assign to a variable to bind a borrow")
		return
	}
	// T0596: reject `arr[i]` / `v[i]` when the slot type is a single-owner
	// native handle (Mutex/MutexGuard/Task). See tryMove for rationale.
	if c.rejectIndexExprSingleOwnerMove(expr) {
		return
	}
	// T0652: reject moves of a for-in loop binding whose iterable element is a
	// single-owner native handle. See tryMove for rationale.
	if c.rejectForInSingleOwnerBindingMove(expr) {
		return
	}
	// T0837: reject consuming a single-owner native handle field out of a shared
	// borrow (e.g. passing `borrowed.mtx!` to a `~` parameter). See tryMove.
	if c.rejectMemberHandleMoveOutOfBorrow(expr) {
		return
	}
	// B0341 field-move check delegated to tryMove; inherit it here too.
	if member, ok := expr.(*ast.MemberExpr); ok {
		c.checkFieldMoveOwnership(member)
		return
	}
	// T0569: `this` cannot be consumed from a method body, even under `~this`.
	// `~this` grants the body mutable access (mutate fields, call `~this`
	// setters), but the value still belongs to the caller — moving `this` into
	// a `~T` consume slot would leave the caller's drop flag set on a freed
	// allocation. The synthesized drop infrastructure (`emitFieldDrops` +
	// `pal_free` at the end of `drop`) bypasses this check, so it does not
	// affect drop bodies.
	if this, ok := expr.(*ast.ThisExpr); ok {
		if c.state["this"] == Moved {
			c.errorf(this.Pos(), "use of moved variable 'this'")
		} else if c.borrows != nil && c.borrows.HasAnyBorrow("this") {
			// T0548: reject consume of `this` while a borrow on it is
			// active (e.g., from `(b, n) := this.pair`).
			c.errorf(this.Pos(), "cannot move 'this' while it is borrowed")
		} else {
			c.errorf(this.Pos(),
				"cannot consume 'this'; the receiver belongs to the caller — call `.clone()` to produce an independent copy, or refactor into a free function taking `Type move`")
		}
		return
	}
	ident, ok := expr.(*ast.IdentExpr)
	if !ok {
		return
	}
	obj := c.info.Objects[ident]
	if obj == nil {
		return
	}
	v, ok := obj.(*types.Var)
	if !ok {
		return
	}
	// T0338/T0381: a Borrowed local cannot be consumed regardless of whether
	// the variable's static type is Copy. Reference types (SharedRef/MutRef)
	// are pointer-sized and thus marked Copy, but the underlying value still
	// belongs to the owner — moving the ref into a `~T` consume site would
	// double-free at the owner's drop. Check state before the Copy fast-path.
	if state, tracked := c.state[ident.Name]; tracked && state == Borrowed {
		if c.params[ident.Name] {
			c.errorf(ident.Pos(),
				"cannot move borrowed parameter '%s'; declare the parameter with `move` to consume it",
				ident.Name)
		} else {
			c.errorf(ident.Pos(), "cannot move borrowed value '%s'", ident.Name)
		}
		return
	}
	if isCopyType(v.Type()) {
		return
	}
	if _, tracked := c.state[ident.Name]; !tracked {
		return
	}
	if c.pinned[ident.Name] {
		c.errorf(ident.Pos(), "cannot move use-bound variable '%s'", ident.Name)
		return
	}
	if c.borrows != nil && c.borrows.HasAnyBorrow(ident.Name) {
		c.errorf(ident.Pos(), "cannot move '%s' while it is borrowed", ident.Name)
	}
	c.state[ident.Name] = Moved
}

// tryMoveConsumeCastSubject peels ParenExpr / CastExpr from expr and runs
// tryMoveConsume on the inner subject. Called at owning-slot stores
// (field/element assignment, constructor field-init) so an `x as!/as T`
// wrapper does not bypass the move-consume that the plain `= x` form
// already performs. The wrapper itself is a no-op in tryMoveConsume (CastExpr
// is not handled there), so the outer call this complements has already
// silently returned without consuming the subject — T0754.
//
// Scope is intentionally narrow: only owning-slot stores (no per-binding drop
// flag) need the peel. Local var-decl and local-IdentExpr reassignment keep
// T0747's view semantics (codegen's isRttiCastBorrow clears the local's drop
// flag); broadening to tryMoveConsume universally would silently reject those
// existing accepted shapes.
func (c *Checker) tryMoveConsumeCastSubject(expr ast.Expr) {
	// Peel any number of ParenExpr.
	for {
		p, ok := expr.(*ast.ParenExpr)
		if !ok {
			break
		}
		expr = p.Expr
	}
	cast, ok := expr.(*ast.CastExpr)
	if !ok {
		return
	}
	inner := cast.Expr
	for {
		p, ok := inner.(*ast.ParenExpr)
		if !ok {
			break
		}
		inner = p.Expr
	}
	// T0800: a chained cast wraps another CastExpr — recurse so the innermost
	// subject (x) is consumed, not the inner cast (a no-op in tryMoveConsume).
	if _, isCast := inner.(*ast.CastExpr); isCast {
		c.tryMoveConsumeCastSubject(inner)
		return
	}
	c.tryMoveConsume(inner)
}

// tryMoveCastSubject peels ParenExpr / CastExpr from expr and runs tryMove (not
// tryMoveConsume) on the inner subject. Called at ReturnStmt so a
// `return x as! T` wrapper does not bypass the move that the plain `return x`
// form already performs (the wrapper itself is a no-op in tryMove — CastExpr is
// not handled there). T0783.
//
// tryMove (the weaker form) is deliberate: the return path uses tryMove, so a
// borrowed parameter returned as a cast under a borrow-typed result must not be
// rejected — tryMove short-circuits on Borrowed state, whereas
// tryMoveConsumeCastSubject would wrongly reject it. The unsound
// borrow-returned-as-owned case is handled separately by checkReturnRefSafety.
//
// Only the *unconditional* `as!` cast (Force == true) moves its subject here. The
// optional `as` form (Force == false) yields `T?` and is a *conditional* move:
// the subject is aliased into the result only when the downcast succeeds; on
// failure the result is None and the subject must still be dropped. The return
// path is terminal (no later source-level use of the subject is possible), so
// leaving the subject Owned here is sound — T0849 makes the *codegen* drop
// conditional on the runtime downcast outcome (drop iff `!isMatch`, via
// consumeCastSubjectDropFlag) rather than clearing the flag unconditionally. The
// owning-slot sibling stays handled by tryMoveConsumeCastSubject (an
// unconditional Moved over-claim, which conservatively rejects any later use);
// its codegen drop is likewise made conditional by T0849. The gate is on the
// *outermost* cast only: once an `as!` wrapper is
// established, peel through any nested casts to the innermost subject (mirrors
// codegen's castSubjectMovableIdent) so a chained `(x as! A) as! B` still moves x.
func (c *Checker) tryMoveCastSubject(expr ast.Expr) {
	// Peel any number of ParenExpr.
	for {
		p, ok := expr.(*ast.ParenExpr)
		if !ok {
			break
		}
		expr = p.Expr
	}
	cast, ok := expr.(*ast.CastExpr)
	if !ok {
		return
	}
	if !cast.Force {
		// T0849: `as` is a conditional move; the subject stays Owned here and
		// codegen makes the scope-exit drop conditional on the runtime outcome.
		return
	}
	// Peel through any number of nested casts / parens to the innermost subject.
	// T0800: a chained cast `(x as! A) as! B` wraps another CastExpr.
	inner := cast.Expr
	for {
		if p, ok := inner.(*ast.ParenExpr); ok {
			inner = p.Expr
			continue
		}
		if nested, ok := inner.(*ast.CastExpr); ok {
			inner = nested.Expr
			continue
		}
		break
	}
	c.tryMove(inner)
}

// --- Call expressions ---

// paramBorrowKind determines whether a parameter is a borrow parameter by
// checking both the explicit Ref() modifier and whether the parameter type
// is a reference type (SharedRef/MutRef). The grammar parses `string &s` as
// typeRef=string& with refMod=none, so we must check the type as well.
func paramBorrowKind(p *types.Param) BorrowKind {
	// Check explicit Ref() first (used by receiver params)
	switch p.Ref() {
	case types.RefShared:
		return BorrowShared
	case types.RefMut:
		return BorrowMut
	}
	// Check if the parameter type is a reference type
	switch p.Type().(type) {
	case *types.SharedRef:
		return BorrowShared
	case *types.MutRef:
		return BorrowMut
	}
	return BorrowNone
}

// isThisCallArgSafe reports whether passing `this` of the given type as a
// plain (non-`~`, non-`&`) call argument is runtime-safe. Two shapes pass:
//
//  1. Primitive scalars (int / float / bool / char / uint / void / none):
//     `this` is the value directly (i64/i32/etc.), parameter ABI is the same,
//     no drop. `e.encode_int(this)` etc. in modules/std/*.pr rely on this.
//
//  2. Auto-dup container handles (string / Vector / Channel / Arc / Weak /
//     Mutex / MutexGuard / Task plus Optional thereof): codegen uses `i8*` for
//     both the receiver value and the parameter shape, and either auto-dups
//     at the call site or propagates per-slot drop-flag so the callee's drop
//     and the caller's drop refer to distinct allocations.
//
// Heap user types and value types (Copy via `IsCopy()` on a Named with all
// `value` fields) are NOT safe: heap user types expect `{i8*, i8*}` and
// value types expect `{i8*, field…}` — codegen at the call site has no
// wrapping path for ThisExpr in either case. T0581.
func isThisCallArgSafe(typ types.Type) bool {
	if typ == nil {
		return false
	}
	// Primitive scalars: hardcoded singletons; value and ABI coincide.
	switch typ {
	case types.TypInt, types.TypI8, types.TypI16, types.TypI32, types.TypI64,
		types.TypUint, types.TypU8, types.TypU16, types.TypU32, types.TypU64,
		types.TypF32, types.TypF64,
		types.TypBool, types.TypChar, types.TypNone, types.TypVoid:
		return true
	}
	// Auto-dup container handles share the codegen-safe alias path.
	return isVarDeclAliasSafeType(typ)
}

// findThisExprInArg reports the ThisExpr surfaced through transparent
// wrappers (parens, if/else branches, match arms). Mirrors the walk in
// findBorrowedNonAliasSafeIdent: codegen emits the wrapped expression's
// branch value directly (the if/match's PHI value, the paren-stripped
// inner), so a `this` deep inside a wrapper still reaches the call site
// as the raw `i8*` receiver pointer with no wrapping → same crash class
// as a bare `f(this)`. T0581.
func findThisExprInArg(expr ast.Expr) *ast.ThisExpr {
	switch e := expr.(type) {
	case *ast.ThisExpr:
		return e
	case *ast.ParenExpr:
		return findThisExprInArg(e.Expr)
	case *ast.IfExpr:
		if t := findThisExprInArgBlock(e.Then); t != nil {
			return t
		}
		return findThisExprInArgBlock(e.Else)
	case *ast.MatchExpr:
		for _, arm := range e.Arms {
			if arm.Body != nil {
				if t := findThisExprInArg(arm.Body); t != nil {
					return t
				}
			}
			if arm.Block != nil {
				if t := findThisExprInArgBlock(arm.Block); t != nil {
					return t
				}
			}
		}
	}
	return nil
}

// findThisExprInArgBlock inspects a block's trailing expression for a
// surfaced ThisExpr. Mirrors findBorrowedNonAliasSafeIdentInBlock.
func findThisExprInArgBlock(block *ast.Block) *ast.ThisExpr {
	if block == nil || len(block.Stmts) == 0 {
		return nil
	}
	if es, ok := block.Stmts[len(block.Stmts)-1].(*ast.ExprStmt); ok {
		return findThisExprInArg(es.Expr)
	}
	return nil
}

// isCallArgUnsafeBorrowedType reports whether moving a Borrowed value of type
// t into a plain (non-`~`, non-`&`) call argument is runtime-unsafe.
//
// Two disjoint shapes produce the same crash class:
//
//  1. **T0556 — non-duppable single-owner native handles** (Mutex[T],
//     MutexGuard[T], Task[T]). Codegen's Vector.push (and similar
//     ownership-transferring builtins) has no dup path for these, so the
//     callee's element drop and the caller's drop fire on the same
//     allocation → double-free.
//
//  2. **T0586 — plain heap user types, Map, Set, and other non-auto-dup
//     droppable values**. Same root cause: the value-struct is passed by
//     alias, both caller and callee's downstream drop sites land on the
//     same heap allocation.
//
// Codegen-safe types (string, Vector, Channel, Arc, Weak, Optional thereof
// for the auto-dup set) are carved out — codegen's `maybeDupPushElement` /
// `setDupFlagsForFieldAccess` clones at the call site so caller and callee
// have independent values.
//
// The predicate is structured so that types satisfying T0586's broader rule
// (`!isCopy && !isVarDeclAliasSafe && isDroppable`) are caught alongside
// T0556's Mutex/MutexGuard/Task subset (which IS in
// `isVarDeclAliasSafeType` because of the codegen drop-flag propagation
// applicable at the var-decl site, not the call-arg site).
func isCallArgUnsafeBorrowedType(t types.Type) bool {
	if t == nil {
		return false
	}
	// T0556 subset — Mutex/MutexGuard/Task are flagged var-decl-alias-safe
	// (codegen drop-flag propagation works at the var-decl site) but NOT
	// call-arg safe (codegen has no dup path at push/store sites).
	if _, ok := types.AsMutex(t); ok {
		return true
	}
	if _, ok := types.AsMutexGuard(t); ok {
		return true
	}
	if _, ok := types.AsTask(t); ok {
		return true
	}
	// T0586 broader rule — plain heap user types, Map, Set, generic user
	// types with droppable fields. Same predicate shape as T0568's var-decl
	// reject; carves out auto-dup containers (string, Vector, Channel, Arc,
	// Weak) and their Optional wrappers via isVarDeclAliasSafeType.
	if !isCopyType(t) && !isVarDeclAliasSafeType(t) && isDroppableType(t) {
		return true
	}
	return false
}

// findBorrowedNonAliasSafeIdent reports a borrowed identifier of a type that
// is unsafe to move into a plain (non-`~`, non-`&`) call argument. Walks
// through transparent wrappers (parens, if/else, match arms). Returns the
// offending ident or nil. The walk mirrors the type-driven shape that
// isBorrowedExpr uses for SharedRef/MutRef: sema propagates the value type
// through these compositions, so any branch that surfaces a Borrowed
// call-arg-unsafe variable reaches the call site as the same alias the
// caller still owns.
//
// See `isCallArgUnsafeBorrowedType` for the union of T0556 (single-owner
// native handles) and T0586 (plain heap user types and friends).
func (c *Checker) findBorrowedNonAliasSafeIdent(expr ast.Expr) *ast.IdentExpr {
	switch e := expr.(type) {
	case *ast.IdentExpr:
		if state, tracked := c.state[e.Name]; tracked && state == Borrowed {
			if isCallArgUnsafeBorrowedType(c.info.Types[e]) {
				return e
			}
		}
	case *ast.ParenExpr:
		return c.findBorrowedNonAliasSafeIdent(e.Expr)
	case *ast.IfExpr:
		if id := c.findBorrowedNonAliasSafeIdentInBlock(e.Then); id != nil {
			return id
		}
		return c.findBorrowedNonAliasSafeIdentInBlock(e.Else)
	case *ast.MatchExpr:
		for _, arm := range e.Arms {
			if arm.Body != nil {
				if id := c.findBorrowedNonAliasSafeIdent(arm.Body); id != nil {
					return id
				}
			}
			if arm.Block != nil {
				if id := c.findBorrowedNonAliasSafeIdentInBlock(arm.Block); id != nil {
					return id
				}
			}
		}
	}
	return nil
}

// findBorrowedNonAliasSafeIdentInBlock inspects a block's trailing expression
// (the block's result value). Mirrors codegen's clearBlockResultDropFlags.
func (c *Checker) findBorrowedNonAliasSafeIdentInBlock(block *ast.Block) *ast.IdentExpr {
	if block == nil || len(block.Stmts) == 0 {
		return nil
	}
	if es, ok := block.Stmts[len(block.Stmts)-1].(*ast.ExprStmt); ok {
		return c.findBorrowedNonAliasSafeIdent(es.Expr)
	}
	return nil
}

// --- Call-site `move` keyword enforcement (T0998 §6.2) ---

// moveSubject peels parens, casts, and force-unwraps to reach the underlying
// argument subject, so `move (x as T)` / `move o!` resolve to `x` / `o`.
func moveSubject(expr ast.Expr) ast.Expr {
	for {
		switch e := expr.(type) {
		case *ast.ParenExpr:
			expr = e.Expr
		case *ast.CastExpr:
			expr = e.Expr
		case *ast.OptionalUnwrapExpr:
			expr = e.Expr
		default:
			return expr
		}
	}
}

// isNamedBinding reports whether expr (after peeling) is a reusable named
// binding — a variable or a field path rooted at one — as opposed to a temporary
// (call result, literal, index, etc.).
func isNamedBinding(expr ast.Expr) bool {
	_, _, ok := extractBorrowTarget(moveSubject(expr))
	return ok
}

// isConsumableNamedBinding reports whether a `move`-less argument would actually
// consume a named binding of a move (non-Copy) type, and therefore requires the
// call-site `move` keyword. Borrowed / moved / use-bound / Copy / temporary
// subjects do not (a borrowed subject is rejected separately by tryMoveConsume).
func (c *Checker) isConsumableNamedBinding(expr ast.Expr) bool {
	subject := moveSubject(expr)
	switch e := subject.(type) {
	case *ast.IdentExpr:
		v, ok := c.info.Objects[e].(*types.Var)
		if !ok || isCopyType(v.Type()) {
			return false
		}
		state, tracked := c.state[e.Name]
		if !tracked || state == Borrowed || state == Moved || c.pinned[e.Name] {
			return false
		}
		return true
	case *ast.MemberExpr:
		// Partial move of a field path rooted at a named binding.
		if _, _, ok := extractBorrowTarget(e); !ok {
			return false
		}
		// T1011: a narrowed enum variant field (`m.body` where `m` is an enum)
		// cannot be partial-moved — the synth enum drop frees the whole variant
		// by tag, so moving one field out would double-free. Non-destructive
		// narrowing reads the field through a borrow of the subject and clones
		// it on escape (genNarrowedVariantField). It is therefore never a
		// consumable named binding and never requires the `move` marker, matching
		// the return/assign escape shapes (which clone via isAutoDupType too).
		if extractEnumForMatch(c.info.Types[e.Target]) != nil {
			return false
		}
		t := c.info.Types[subject]
		return t != nil && !isCopyType(t)
	}
	return false
}

// enforceMoveMarker validates the call-site `move` keyword on a call/constructor
// argument that lands in a consuming slot (§6.2): a consumed named binding must
// be written `move x`, and `move` on a temporary is rejected.
func (c *Checker) enforceMoveMarker(arg *ast.Arg) {
	// The `move` keyword is a source-syntax requirement; skip compiler-synthesized
	// args (clone/encode/serialize), which carry no source position (Line == 0).
	if arg.Value == nil || arg.Value.Pos().Line == 0 {
		return
	}
	if arg.Move {
		if !isNamedBinding(arg.Value) {
			c.errorf(arg.Value.Pos(), "`move` applies to a named binding; this argument is a temporary — remove `move`")
		}
		return
	}
	if c.isConsumableNamedBinding(arg.Value) {
		name, path, _ := extractBorrowTarget(moveSubject(arg.Value))
		label := borrowTargetLabel(name, path)
		c.errorf(arg.Value.Pos(), "consuming '%s' requires `move %s`", label, label)
	}
}

// rejectMoveMarker reports a `move` keyword on an argument bound to a borrow
// slot, where the value is not consumed (§6.2).
func (c *Checker) rejectMoveMarker(arg *ast.Arg) {
	if arg.Move && arg.Value != nil && arg.Value.Pos().Line != 0 {
		c.errorf(arg.Value.Pos(), "this parameter borrows the argument — it is not consumed; remove `move`")
	}
}

// checkCallExpr handles function calls and constructor calls.
// For function calls, arguments matched to value parameters trigger moves;
// arguments matched to borrow parameters create borrows.
// For constructor calls, all arguments are consumed.
func (c *Checker) checkCallExpr(e *ast.CallExpr) {
	c.checkExpr(e.Callee)

	// T0634: An enum-variant constructor call (`EnumType[Args].Variant(arg…)`)
	// is a constructor, but sema types its callee as a synthetic
	// *types.Signature (sema/expr.go: the *types.Enum variant branch and
	// resolveEnumMemberInst both `return types.NewSignature(...)`). It would
	// therefore otherwise take the permissive function-call path below. When
	// the variant field type is concrete (non-generic enum), that path happens
	// to reject a borrowed-parameter arg via isDroppableType; but when the
	// field type is a bare type parameter (e.g.
	// `mk_holder[T](T x) Holder[T] { return Holder[T].Wrap(x); }`)
	// isDroppableType has no *types.TypeParam case → the move is silently
	// allowed → codegen stores the borrowed value into the variant payload
	// with no clone → the returned enum and the caller's value alias the same
	// instance → double-free at scope exit. Route enum-variant-constructor
	// args through the same tryMoveConsume path used for struct constructors
	// (and the sig==nil constructor branch below), so a borrowed parameter
	// yields the standard `move`-parameter diagnostic — matching generic/non-generic
	// struct constructors and the non-generic enum-variant constructor.
	if c.isEnumVariantConstructorCallee(e.Callee) {
		for _, arg := range e.Args {
			c.checkExpr(arg.Value)
			c.enforceMoveMarker(arg)
			c.tryMoveConsume(arg.Value)
			// T0754: peel `as!/as T` so the cast subject is consumed too —
			// enum-variant payload owns the value, no per-arg drop flag.
			c.tryMoveConsumeCastSubject(arg.Value)
			// T0811: `Enum.Variant(o!)` — force-unwrap of a borrowed droppable
			// Optional param. The cast form is handled above; reject the
			// force-unwrap shape here.
			if isForceUnwrapForm(arg.Value) {
				c.rejectBorrowedOptionalUnwrapConsume(arg.Value)
			}
		}
		return
	}

	sig := c.calleeSignature(e.Callee)
	if sig != nil {
		// Function/method call: process args left-to-right.
		params := sig.Params()
		// T0964: a container-store native method (Vector.push) takes ownership
		// of (or dups) its value argument at the store site, so a plain `T`
		// argument is consumed/duped — NOT borrowed. General calls below treat
		// a plain `T` parameter as a shared borrow.
		storeNative := c.isElementStoringNativeCall(e.Callee)
		for i, arg := range e.Args {
			c.checkExpr(arg.Value)
			if i < len(params) {
				kind := paramBorrowKind(params[i])
				// T0998: the call-site `move` marker is required exactly where the
				// arg is consumed below (storeNative element store, or a `move`
				// parameter), and rejected on borrow slots — mirroring the dispatch.
				if (kind == BorrowNone && storeNative) || params[i].Ref() == types.RefMut {
					c.enforceMoveMarker(arg)
				} else {
					c.rejectMoveMarker(arg)
				}
				if kind == BorrowNone {
					// T0581: passing `this` as a plain (non-`~`, non-`&`) call-arg
					// into a slot expecting the type's value-struct shape (`{i8*,i8*}`
					// for heap user types, `{i8*, field…}` for value types) crashes at
					// runtime — codegen emits the raw `i8*` receiver pointer with no
					// wrapping, so the callee `extractvalue` reads garbage (heap user
					// type case) or returns a wrong value (value-type case). Even if
					// wrapping were added, the new local in the callee would alias the
					// caller's heap allocation and the caller's drop binding would
					// still fire → double-free. Carve out primitive scalars
					// (int/float/bool/char/uint — `this` is the value directly, ABI
					// matches) and the auto-dup container set captured by
					// isVarDeclAliasSafeType (string / Vector / Channel / Arc / Weak /
					// Mutex / MutexGuard / Task plus Optional thereof), whose
					// value-rep and parameter-shape coincide and whose lifetime is
					// handled safely by codegen's auto-dup or per-slot drop-flag
					// logic. User-defined Copy value types are NOT in the carve-out
					// because their parameter-shape is the embedded-field value
					// struct, not the raw receiver pointer. Surfacing through
					// transparent wrappers (parens, if/else, match) shares the same
					// runtime path — codegen forwards the branch value directly —
					// so peel them via findThisExprInArg, mirroring T0556's pattern.
					if this := findThisExprInArg(arg.Value); this != nil {
						thisType := c.info.Types[arg.Value]
						if thisType != nil && !isThisCallArgSafe(thisType) {
							if c.state["this"] == Moved {
								c.errorf(this.Pos(), "use of moved variable 'this'")
							} else if c.borrows != nil && c.borrows.HasAnyBorrow("this") {
								c.errorf(this.Pos(), "cannot move 'this' while it is borrowed")
							} else {
								c.errorf(this.Pos(),
									"cannot consume 'this'; the receiver belongs to the caller — call `.clone()` to produce an independent copy, or refactor into a free function taking `Type move`")
							}
							continue
						}
					}
					if storeNative {
						// T0665: storing a MutexGuard into a container declared
						// before the local that owns its source Mutex means the
						// container outlives that Mutex — the guard's scope-exit drop
						// would unlock an already-destroyed Mutex (UAF; the repro
						// segfaults at 0x0). Reject. The Mutex owner is the root of
						// the `.lock()` receiver chain (the Mutex local itself, or a
						// struct/vector local owning it). Guards with no tracked-local
						// provenance (Mutex reached via a param or `this`) are
						// conservatively allowed — the Mutex outlives the function, so
						// the container-store order stays valid (T0557's pattern).
						if types.IsMutexGuard(peelOptional(c.info.Types[arg.Value])) {
							if mem, ok := e.Callee.(*ast.MemberExpr); ok {
								if container, ok := mem.Target.(*ast.IdentExpr); ok {
									if cOrder, tracked := c.declOrder[container.Name]; tracked {
										if mroot := c.guardMutexExprRoot(arg.Value); mroot != "" {
											if mOrder, ok := c.declOrder[mroot]; ok && cOrder < mOrder {
												c.errorf(arg.Value.Pos(),
													"cannot store a MutexGuard into '%s': the container is declared before '%s' (which owns the guard's Mutex) and outlives it — the guard's drop would unlock an already-destroyed Mutex. Declare the container after '%s', or use a scoped `use guard := m.lock()` binding.",
													container.Name, mroot, mroot)
												continue
											}
										}
									}
								}
							}
						}
						// T0556/T0586: a container-store native method (Vector.push)
						// consumes or dups its element. Reject moving a borrowed
						// non-Copy, non-auto-dup, droppable value into the store site
						// (single-owner handles, plain heap user types, Map, Set):
						// codegen has no dup path for these, so the callee's element
						// drop and the caller's drop fire on the same allocation →
						// runtime double-free. The predicate matches T0568's var-decl
						// reject; auto-dup containers (string/Vector/Channel/Arc/Weak)
						// fall through and are duped by codegen at the push site.
						if ident := c.findBorrowedNonAliasSafeIdent(arg.Value); ident != nil {
							if c.params[ident.Name] {
								c.errorf(ident.Pos(),
									"cannot move borrowed parameter '%s'; declare the parameter with `move` to consume it",
									ident.Name)
							} else {
								c.errorf(ident.Pos(), "cannot move borrowed value '%s'", ident.Name)
							}
							continue
						}
						// An owned arg is consumed (marked Moved); a borrowed auto-dup
						// arg is a no-op here (tryMove short-circuits on Borrowed) and
						// duped by codegen.
						c.tryMove(arg.Value)
					} else {
						// T0964: a plain (unmarked) move-type parameter of a general
						// call is a SHARED BORROW, matching docs/language-guide.md — the
						// caller retains ownership and the value stays usable after the
						// call. Register a call-scoped shared borrow (expires at
						// statement end) exactly like a `T&` param, rather than marking
						// the arg Moved.
						c.createBorrowWithKind(arg.Value, BorrowShared, e.Pos())
					}
				} else if params[i].Ref() == types.RefMut {
					// T0087: ~ on regular params means move (callee owns).
					// T0338: a `~` callee genuinely consumes the value, so the
					// arg must not be a borrowed parameter — the outer caller
					// still drops the original at scope exit.
					c.tryMoveConsume(arg.Value)
					// T0754: peel `as!/as T` so the cast subject is consumed too —
					// the `~` callee takes ownership, no per-arg drop flag at this
					// site for the cast wrapper.
					c.tryMoveConsumeCastSubject(arg.Value)
					// T0811: `g(o!)` into a `~` slot — force-unwrap of a borrowed
					// droppable Optional param. The cast form is rejected by
					// tryMoveConsumeCastSubject above; reject the force-unwrap shape.
					if isForceUnwrapForm(arg.Value) {
						c.rejectBorrowedOptionalUnwrapConsume(arg.Value)
					}
				} else {
					c.createBorrowWithKind(arg.Value, kind, e.Pos())
				}
			} else {
				// Extra variadic args (no distinct param slot) are not consumed.
				c.rejectMoveMarker(arg)
			}
		}
		if member, ok := e.Callee.(*ast.MemberExpr); ok &&
			isConsumingNativeMethod(c.info.Types[member.Target], member.Field) {
			// T0846: close(~this) on a MutexGuard unlocks AND pal_free's the guard
			// (its body is @MutexGuard.drop, T0839). Treat the call as a consume so
			// any later use of the receiver is rejected at compile time
			// ("use of moved variable") instead of becoming a use-after-free — the
			// same machinery as the T0837 single-owner-handle guards. tryMoveConsume
			// routes member/index/borrowed-param/transient shapes through their
			// existing rejections; a bare ident receiver is marked Moved.
			c.tryMoveConsume(member.Target)
		} else {
			c.checkReceiverBorrow(e.Callee, sig, e.Pos())
		}
		c.checkBorrowConflicts(e, sig)
	} else {
		// Constructor or unresolved call — all args are consumed.
		for _, arg := range e.Args {
			c.checkExpr(arg.Value)
			c.enforceMoveMarker(arg)
			c.tryMoveConsume(arg.Value)
			// T0754: peel `as!/as T` so the cast subject is consumed too —
			// constructor field-init owns the value, no per-arg drop flag.
			c.tryMoveConsumeCastSubject(arg.Value)
			// T0811: `T(field: o!)` — force-unwrap of a borrowed droppable
			// Optional param. The cast form is handled above; reject the
			// force-unwrap shape here.
			if isForceUnwrapForm(arg.Value) {
				c.rejectBorrowedOptionalUnwrapConsume(arg.Value)
			}
		}
	}
}

// extractBorrowTarget walks an expression to extract the root variable name
// and field path for borrow tracking. Returns ("", nil, false) if the
// expression is not a trackable borrow target (e.g., a function call result).
func extractBorrowTarget(expr ast.Expr) (name string, path []string, ok bool) {
	switch e := expr.(type) {
	case *ast.IdentExpr:
		return e.Name, nil, true
	case *ast.MemberExpr:
		name, path, ok = extractBorrowTarget(e.Target)
		if ok {
			path = append(path, e.Field)
		}
		return
	}
	return "", nil, false
}

// borrowTargetLabel formats a borrow target as "name" or "name.field1.field2" for error messages.
func borrowTargetLabel(name string, path []string) string {
	if len(path) == 0 {
		return name
	}
	// Pre-compute total length to avoid repeated allocations.
	n := len(name)
	for _, f := range path {
		n += 1 + len(f)
	}
	buf := make([]byte, 0, n)
	buf = append(buf, name...)
	for _, f := range path {
		buf = append(buf, '.')
		buf = append(buf, f...)
	}
	return string(buf)
}

// createBorrowWithKind checks for borrow conflicts with existing borrows and registers a new borrow.
// Supports both simple variable references (IdentExpr) and field access chains (MemberExpr).
func (c *Checker) createBorrowWithKind(expr ast.Expr, kind BorrowKind, pos ast.Pos) {
	name, path, ok := extractBorrowTarget(expr)
	if !ok {
		return
	}
	if c.borrows == nil {
		return
	}
	// Copy types don't need borrow tracking — check the root variable.
	// Walk to the root IdentExpr (for both simple idents and member chains).
	rootExpr := expr
	for {
		if me, isME := rootExpr.(*ast.MemberExpr); isME {
			rootExpr = me.Target
		} else {
			break
		}
	}
	if ident, isIdent := rootExpr.(*ast.IdentExpr); isIdent {
		if obj := c.info.Objects[ident]; obj != nil {
			if v, isVar := obj.(*types.Var); isVar && isCopyType(v.Type()) {
				return
			}
		}
	}

	label := borrowTargetLabel(name, path)

	// Check against existing borrows using path-aware overlap
	if kind == BorrowMut && c.borrows.HasOverlappingBorrow(name, path) {
		c.errorf(pos, "cannot borrow '%s' as mutable — it is already borrowed", label)
		return
	}
	if kind == BorrowShared && c.borrows.HasOverlappingMutBorrow(name, path) {
		c.errorf(pos, "cannot borrow '%s' as shared — it is mutably borrowed", label)
		return
	}

	c.borrows.Add(&Borrow{Origin: name, FieldPath: path, Kind: kind, Pos: pos})
}

// isConsumingNativeMethod reports whether a native method frees its `~this`
// receiver, so ownership must treat the call as a consume (mark the receiver
// Moved) rather than a mutable borrow. Currently only MutexGuard.close: its
// body is @MutexGuard.drop, which unlocks AND pal_free's the guard (T0839), so
// any later use of the receiver is a use-after-free (T0846). Extend this list
// if another free-on-`~this` native method is added.
func isConsumingNativeMethod(recvType types.Type, methodName string) bool {
	return methodName == "close" && types.IsMutexGuard(recvType)
}

// isElementStoringNativeCall reports whether the call is to a native container
// method that takes ownership of (or dups) its value argument at the store
// site — currently only Vector.push(T elem). For these, a plain `T` argument is
// consumed (owned source) or duped by codegen (auto-dup source), and a borrowed
// single-owner handle / non-auto-dup heap value is rejected (T0556/T0586). A
// plain `T` parameter of any OTHER call is a shared borrow (T0964). Mirrors the
// hardcoded-native-method pattern of isConsumingNativeMethod; extend the method
// set here if another ownership-taking native container method is added (note
// Channel.send/Map._set/Set.add already declare `~T` and need no entry).
func (c *Checker) isElementStoringNativeCall(callee ast.Expr) bool {
	member, ok := callee.(*ast.MemberExpr)
	if !ok {
		return false
	}
	return member.Field == "push" && types.IsVector(c.info.Types[member.Target])
}

// guardMutexExprRoot returns the name of the local that owns the Mutex a pushed
// MutexGuard argument borrows, or "" when it cannot be resolved to a tracked
// local. Handles a temporary `v.push(m.lock())` / `v.push(h.m.lock())` (the
// root of the lock-call receiver chain) and a guard variable `v.push(move g)`
// (its recorded guardMutexRoot, which also transitively covers guard-to-guard
// aliases). A receiver rooted at `this` (or any untracked root) yields "" —
// the Mutex outlives the method, so the store is safe. T0665.
func (c *Checker) guardMutexExprRoot(expr ast.Expr) string {
	switch e := unwrapDestructureParens(expr).(type) {
	case *ast.IdentExpr:
		return c.guardMutexRoot[e.Name]
	case *ast.CallExpr: // m.lock() / h.m.lock() — receiver must be a Mutex
		if mem, ok := e.Callee.(*ast.MemberExpr); ok && types.IsMutex(c.info.Types[mem.Target]) {
			return destructureBorrowRoot(mem.Target)
		}
	}
	return ""
}

// checkReceiverBorrow creates a borrow for method calls with &this or ~this receivers.
// For nested member expressions like f.sub.method(), the borrow is on f with path ["sub"].
func (c *Checker) checkReceiverBorrow(callee ast.Expr, sig *types.Signature, pos ast.Pos) {
	if sig.Recv() == nil {
		return
	}
	// Receiver uses Ref() (grammar: receiverParam : refMod? THIS). A bare `this`
	// is a shared (read-only) borrow of the receiver (T0998), so an unmarked
	// receiver (RefNone → BorrowNone) borrows shared, mirroring the old `&this`.
	recvKind := paramBorrowKind(sig.Recv())
	if recvKind == BorrowNone {
		recvKind = BorrowShared
	}
	member, ok := callee.(*ast.MemberExpr)
	if !ok {
		return
	}
	// member.Target is the receiver expression (the object the method is called on).
	// Pass it directly to createBorrowWithKind which handles both IdentExpr and MemberExpr.
	c.createBorrowWithKind(member.Target, recvKind, pos)
}

// rejectGoCallLoopBindingBorrowEscape rejects passing an owned, non-copy,
// droppable for-in loop binding as a BORROW argument into a `go` call (T1147).
// The spawned goroutine can outlive the loop iteration, so the borrow dangles —
// the goroutine reads the per-iteration value after its slot is freed/reused →
// non-deterministic use-after-free. Move/consume args (`~` params,
// container-store natives) are NOT rejected — ownership transfers into the
// goroutine frame, which is sound (handled by T1148/T0964). The method receiver
// itself is untouched (captured & auto-dup'd by the closure mechanism).
// Constructor / unresolved callees (sig == nil) consume their args and are out
// of scope here. Mirrors option 1 of T1147 and the existing borrow-escape
// rejections (return / channel-send / raise of borrowed values).
func (c *Checker) rejectGoCallLoopBindingBorrowEscape(e *ast.CallExpr) {
	sig := c.calleeSignature(e.Callee)
	if sig == nil {
		return
	}
	params := sig.Params()
	storeNative := c.isElementStoringNativeCall(e.Callee)
	for i, arg := range e.Args {
		if i >= len(params) {
			continue
		}
		kind := paramBorrowKind(params[i])
		// Skip consume slots (`move` param, or container-store native) — those
		// transfer ownership into the goroutine frame and are sound.
		if params[i].Ref() == types.RefMut || (kind == BorrowNone && storeNative) {
			continue
		}
		// Borrow slot: reject an owned droppable for-in binding root.
		if id := identRoot(arg.Value); id != nil && c.forInOwnedDroppableBindings[id.Name] {
			c.errorf(id.Pos(),
				"cannot pass borrowed loop variable '%s' into a goroutine; the goroutine may outlive the loop iteration, leaving a dangling borrow — clone it with `%s.clone()`, or move an owned value into the goroutine",
				id.Name, id.Name)
		}
	}
}

// identRoot peels ParenExpr layers and returns the underlying *ast.IdentExpr,
// or nil for any other expression shape. Kept deliberately conservative (paren /
// ident only) to match the sibling reject* helpers; cast / member roots are out
// of scope for the T1147 loop-binding-borrow-escape check.
func identRoot(expr ast.Expr) *ast.IdentExpr {
	for {
		if p, ok := expr.(*ast.ParenExpr); ok {
			expr = p.Expr
			continue
		}
		break
	}
	if id, ok := expr.(*ast.IdentExpr); ok {
		return id
	}
	return nil
}

// calleeSignature extracts the Signature from a callee expression's type.
func (c *Checker) calleeSignature(callee ast.Expr) *types.Signature {
	typ := c.info.Types[callee]
	if typ == nil {
		return nil
	}
	sig, _ := typ.(*types.Signature)
	return sig
}

// --- Borrow conflict detection ---

type borrowEntry struct {
	name string
	path []string
	kind BorrowKind
}

// checkBorrowConflicts detects conflicting borrows at a single call site.
// Multiple shared borrows of the same variable are OK.
// A mutable borrow combined with any overlapping borrow of the same variable is an error.
// Disjoint field borrows (e.g., f.x and f.y) do not conflict.
func (c *Checker) checkBorrowConflicts(e *ast.CallExpr, sig *types.Signature) {
	params := sig.Params()
	var borrows []borrowEntry

	for i, arg := range e.Args {
		if i >= len(params) {
			break
		}
		kind := paramBorrowKind(params[i])
		if kind == BorrowNone || params[i].Ref() == types.RefMut {
			continue // T0087: ~ params are moves, not borrows
		}
		name, path, ok := extractBorrowTarget(arg.Value)
		if !ok {
			continue
		}
		borrows = append(borrows, borrowEntry{name: name, path: path, kind: kind})
	}

	for i := 0; i < len(borrows); i++ {
		for j := i + 1; j < len(borrows); j++ {
			if borrows[i].name != borrows[j].name {
				continue
			}
			if !pathsOverlap(borrows[i].path, borrows[j].path) {
				continue // disjoint field borrows — no conflict
			}
			if borrows[i].kind == BorrowMut || borrows[j].kind == BorrowMut {
				other := borrows[i].kind
				if borrows[i].kind == BorrowMut {
					other = borrows[j].kind
				}
				label := borrowTargetLabel(borrows[i].name, borrows[i].path)
				c.errorf(e.Pos(), "cannot borrow '%s' as mutable because it is also borrowed as %s in the same call",
					label, borrowKindLabel(other))
				return
			}
		}
	}
}

func borrowKindLabel(k BorrowKind) string {
	switch k {
	case BorrowShared:
		return "shared"
	case BorrowMut:
		return "mutable"
	default:
		return "value"
	}
}

// --- Control flow expressions ---

func (c *Checker) checkIfExpr(e *ast.IfExpr) {
	c.checkExpr(e.Cond)
	savedState := c.state.clone()
	savedBorrows := c.borrows.Clone()
	c.checkBlock(e.Then)
	thenState := c.state
	thenBorrows := c.borrows
	c.state = savedState.clone()
	c.borrows = savedBorrows.Clone()
	c.checkBlock(e.Else)
	elseState := c.state
	elseBorrows := c.borrows
	// T1134: exclude a diverging branch's end-state from the merge so a move on
	// a non-fall-through path does not poison post-expression state.
	thenDiverges := blockDiverges(e.Then)
	elseDiverges := blockDiverges(e.Else)
	switch {
	case thenDiverges && elseDiverges:
		c.state = savedState
		c.borrows = savedBorrows
	case thenDiverges:
		c.state = elseState
		c.borrows = elseBorrows
	case elseDiverges:
		c.state = thenState
		c.borrows = thenBorrows
	default:
		c.state = merge(thenState, elseState)
		c.borrows = MergeBorrowSets(thenBorrows, elseBorrows)
	}
}

func (c *Checker) checkMatchExpr(e *ast.MatchExpr) {
	c.checkExpr(e.Subject)
	if len(e.Arms) == 0 {
		return
	}

	subjectType := c.info.Types[e.Subject]

	savedState := c.state.clone()
	savedBorrows := c.borrows.Clone()
	var states []StateMap
	var borrowSets []*BorrowSet

	for _, arm := range e.Arms {
		c.state = savedState.clone()
		c.borrows = savedBorrows.Clone()
		c.registerPatternBindings(arm.Pattern)
		// T0623: a destructure arm binding (non-`_`) a variant field whose
		// resolved type transitively owns a single-owner handle takes ownership
		// of the subject's variant payload. Mark the subject as Moved inside
		// this arm's state — the merge across arms then propagates "subject is
		// moved after the match" through non-moving arms too (Owned ∧ Moved →
		// Moved), satisfying the partial-move acceptance criterion.
		//
		// A borrowed-parameter subject (plain non-Copy `E e` param — the caller
		// owns the enum, the callee borrows it) cannot move out: the binding
		// would alias the caller-owned variant payload, and the caller's drop
		// at scope exit would double-free. Sema's IdentExpr check accepts the
		// callee-side ident (the static type is `E`, not `E&`/`E~`); ownership
		// is the layer with the Borrowed-state info to reject it cleanly.
		if armMovesSubject(arm.Pattern, subjectType) {
			c.rejectBorrowedMoveSubject(e.Subject, arm.Pattern)
			c.tryMove(e.Subject)
		}
		if arm.Guard != nil {
			c.checkExpr(arm.Guard)
		}
		if arm.Body != nil {
			c.checkExpr(arm.Body)
		}
		if arm.Block != nil {
			c.checkBlock(arm.Block)
		}
		// T1134: a block arm that diverges (ends in return/raise/break/continue)
		// never falls through to the post-match path, so exclude its end-state —
		// including any moves it performed — from the merge across arms.
		if arm.Block != nil && blockDiverges(arm.Block) {
			continue
		}
		states = append(states, c.state)
		borrowSets = append(borrowSets, c.borrows)
	}

	if len(states) == 0 {
		// Every arm diverges: post-match code is unreachable. Restore the
		// pre-match baseline as a safe state.
		c.state = savedState
		c.borrows = savedBorrows
		return
	}

	resultState := states[0]
	resultBorrows := borrowSets[0]
	for i := 1; i < len(states); i++ {
		resultState = merge(resultState, states[i])
		resultBorrows = MergeBorrowSets(resultBorrows, borrowSets[i])
	}
	c.state = resultState
	c.borrows = resultBorrows
}

// armMovesSubject reports whether a match arm's destructure pattern binds out
// a variant field whose resolved type transitively owns a single-owner handle
// (Task/Mutex/MutexGuard). Such an arm consumes the subject: the binding takes
// ownership of the handle and the subject must be marked Moved so subsequent
// uses (and the synth enum drop) do not double-free. (T0623)
func armMovesSubject(pat ast.MatchPattern, subjectType types.Type) bool {
	if pat == nil || subjectType == nil {
		return false
	}
	var bindings []string
	var variantName string
	switch p := pat.(type) {
	case *ast.EnumDestructureMatchPattern:
		bindings = p.Bindings
		variantName = p.Variant
	case *ast.ShortDestructureMatchPattern:
		bindings = p.Bindings
		variantName = p.Name
	default:
		return false
	}
	enum := extractEnumForMatch(subjectType)
	if enum == nil {
		return false
	}
	v := enum.LookupVariant(variantName)
	if v == nil {
		return false
	}
	var subst map[*types.TypeParam]types.Type
	if inst, ok := subjectType.(*types.Instance); ok && len(enum.TypeParams()) > 0 {
		subst = types.BuildSubstMap(enum.TypeParams(), inst.TypeArgs())
	}
	n := len(bindings)
	if n > v.NumFields() {
		n = v.NumFields()
	}
	for i := 0; i < n; i++ {
		if bindings[i] == "_" {
			continue
		}
		ft := v.Fields()[i].Type()
		if subst != nil {
			ft = types.Substitute(ft, subst)
		}
		if sema.FirstNestedSingleOwnerHandle(ft) != nil {
			return true
		}
	}
	return false
}

// rejectBorrowedMoveSubject errors when a destructure arm would move out of a
// Borrowed-state ident (typically a plain non-Copy `E e` parameter — the caller
// owns the enum, the callee only reads it). Moving out would alias the caller-
// owned variant payload, and the caller's synth enum drop would double-free
// the handle the binding now owns. (T0623)
func (c *Checker) rejectBorrowedMoveSubject(subject ast.Expr, pat ast.MatchPattern) {
	id, ok := subject.(*ast.IdentExpr)
	if !ok {
		return
	}
	if c.state[id.Name] != Borrowed {
		return
	}
	c.errorf(pat.Pos(),
		"cannot destructure a variant owning a single-owner handle from borrowed '%s': move-out would alias the owner's payload and double-free; bind to a local owned variable before matching, or use '_' to skip the handle field",
		id.Name)
}

// extractEnumForMatch unwraps an Instance to its *types.Enum origin, or returns
// a bare *types.Enum directly. Used by armMovesSubject. (T0623)
func extractEnumForMatch(typ types.Type) *types.Enum {
	switch t := typ.(type) {
	case *types.Enum:
		return t
	case *types.Instance:
		if e, ok := t.Origin().(*types.Enum); ok {
			return e
		}
	}
	return nil
}

// isEnumVariantConstructorCallee reports whether callee is the callee of an
// enum-variant constructor call of the form `EnumType[Args].Variant(arg…)`.
// Both generic and non-generic enums are covered: the member Target's recorded
// type resolves (via extractEnumForMatch) to a *types.Enum or an enum-origin
// *types.Instance, and the member Field names a variant of that enum. Enum
// *methods* (and getters) return false here because LookupVariant does not
// match a method name, so callee dispatch for `value.method(arg…)` is
// unaffected. (T0634)
func (c *Checker) isEnumVariantConstructorCallee(callee ast.Expr) bool {
	member, ok := callee.(*ast.MemberExpr)
	if !ok {
		return false
	}
	enum := extractEnumForMatch(c.info.Types[member.Target])
	if enum == nil {
		return false
	}
	return enum.LookupVariant(member.Field) != nil
}

func (c *Checker) registerPatternBindings(pat ast.MatchPattern) {
	if pat == nil {
		return
	}
	switch p := pat.(type) {
	case *ast.NameMatchPattern:
		if p.Name != "_" {
			c.state[p.Name] = Owned
		}
	case *ast.EnumDestructureMatchPattern:
		for _, b := range p.Bindings {
			if b != "_" {
				c.state[b] = Owned
			}
		}
	case *ast.ShortDestructureMatchPattern:
		for _, b := range p.Bindings {
			if b != "_" {
				c.state[b] = Owned
			}
		}
	case *ast.TypeBindingMatchPattern:
		if p.Binding != "_" {
			c.state[p.Binding] = Owned
		}
	case *ast.ExpressionMatchPattern:
		c.checkExpr(p.Expr)
	}
}

// --- Field move ownership (B0341) ---

// extractNamedType unwraps Instance/SharedRef/MutRef to get the underlying *types.Named.
func extractNamedType(typ types.Type) *types.Named {
	switch t := typ.(type) {
	case *types.Named:
		return t
	case *types.Instance:
		if n, ok := t.Origin().(*types.Named); ok {
			return n
		}
	case *types.SharedRef:
		return extractNamedType(t.Elem())
	case *types.MutRef:
		return extractNamedType(t.Elem())
	}
	return nil
}

// isBorrowedExpr returns true if the expression's static type is a SharedRef
// or MutRef (T&/T~) AND the underlying T is non-Copy. Such an expression
// produces a non-owning reference; consuming or moving the result while the
// owner remains alive would double-free. For Copy T (int, float, bool, etc.)
// the value is independently copied, so no rejection is needed.
//
// Replaces the AST-shape heuristic from T0367/T0377/T0380 — keying on the
// type means if/match/paren chains and any future borrow-returning getter
// (not just `.borrow`) are handled uniformly, since sema propagates the
// SharedRef through expression composition.
func (c *Checker) isBorrowedExpr(expr ast.Expr) bool {
	typ := c.info.Types[expr]
	return isBorrowedType(typ)
}

// isBorrowedType reports whether typ is a SharedRef/MutRef whose element is
// non-Copy. Copy elements (primitives) flow through borrows as plain values.
func isBorrowedType(typ types.Type) bool {
	switch t := typ.(type) {
	case *types.SharedRef:
		return !isCopyType(t.Elem())
	case *types.MutRef:
		return !isCopyType(t.Elem())
	}
	return false
}

// isSingleOwnerNativeType reports whether typ resolves to one of the
// single-owner native handle types (Mutex/MutexGuard/Task) for which dup
// semantics are explicitly undefined (T0508). These cannot be extracted from
// an array/vector slot because the read would alias the slot's owned pointer
// → double-free at the container's scope-exit drop. Recurses through Optional.
// Note: nested containers (e.g. `Mutex[T][N][M]`) do not need an Array arm here
// because T0545 rejects any container that transitively contains a single-owner
// handle at sema — those types cannot be constructed, so the ownership pass
// never sees a row-typed IndexExpr result.
func isSingleOwnerNativeType(typ types.Type) bool {
	if typ == nil {
		return false
	}
	if opt, ok := typ.(*types.Optional); ok {
		return isSingleOwnerNativeType(opt.Elem())
	}
	return types.IsMutex(typ) || types.IsMutexGuard(typ) || types.IsTask(typ)
}

// forInAliasingElementType returns the element type of an iterable used in a
// for-in whose loop binding *aliases* the container's backing storage (a slot
// pointer / map value pointer). For these shapes, moving the binding out
// without updating the container leaves a dangling slot that double-frees at
// scope-exit. Returns nil for shapes that yield owned values per iteration
// (Range, Channel, custom Iterator/Stream via .iter()/.next(), strings) —
// those don't introduce aliasing and must NOT be flagged.
//
// Restricted to the three native shapes the codegen lowers via genForInVector
// / genForInArray / genForInMap. Iterator-based for-ins go through
// genForInCustomIter/genForInGenerator (yield-based) and produce owned values.
func forInAliasingElementType(typ types.Type) types.Type {
	if typ == nil {
		return nil
	}
	if arr, ok := typ.(*types.Array); ok {
		return arr.Elem()
	}
	if elem, ok := types.AsVector(typ); ok {
		return elem
	}
	if _, val, ok := types.AsMap(typ); ok {
		return val
	}
	return nil
}

// rejectForInSingleOwnerBindingMove emits an error if expr is an IdentExpr
// whose name is a recorded for-in loop binding over a native container of
// single-owner native handles. Mirrors rejectIndexExprSingleOwnerMove for
// the loop-binding operand shape. Peels ParenExpr / OptionalUnwrapExpr so
// `(h)` and `h!` (on Optional<Mutex> bindings) both reach the IdentExpr.
// Returns true when rejected so the caller can skip move bookkeeping.
func (c *Checker) rejectForInSingleOwnerBindingMove(expr ast.Expr) bool {
	for {
		if p, ok := expr.(*ast.ParenExpr); ok {
			expr = p.Expr
			continue
		}
		if u, ok := expr.(*ast.OptionalUnwrapExpr); ok {
			expr = u.Expr
			continue
		}
		break
	}
	ident, ok := expr.(*ast.IdentExpr)
	if !ok {
		return false
	}
	if c.forInSingleOwnerBindings[ident.Name] {
		typ := c.info.Types[ident]
		c.errorf(ident.Pos(),
			"cannot move for-in loop binding '%s' (%s); the binding aliases the container's slot — moving it would leave the container with a dangling pointer that double-frees at scope exit. Use `<-%s` to receive the value directly, or call `.pop()` / `.remove()` on the container to take ownership of an element",
			ident.Name, typ.String(), ident.Name)
		return true
	}
	// T0971/T0978: moving a binding out of a native container (owned,
	// plain-borrow-param, or borrowed-ref) aliases the container's element
	// storage, which its owner still drops at scope exit.
	if c.forInAliasBindings[ident.Name] {
		typ := c.info.Types[ident]
		c.errorf(ident.Pos(),
			"cannot move for-in loop binding '%s' (%s); the binding aliases the container's element storage, so moving it would double-free when the container drops its elements. Call `.clone()` to take an independent copy, or `.pop()` / `.remove()` to take ownership of an element",
			ident.Name, typ.String())
		return true
	}
	// T1035: moving a binding out of a native container whose element is a bare
	// TypeParam. The verdict can't be decided in the generic body (`T` is
	// unbound) — record a deferred drain requirement that propagateDrainReqs
	// validates per concrete instantiation, and do NOT reject inline (Copy/string
	// instantiations stay legal). Return false so move bookkeeping proceeds
	// normally for the generic body.
	if tp := c.forInTypeParamAliasBindings[ident.Name]; tp != nil {
		c.recordDrainReq(tp, ident.Pos(), ident.Name)
		return false
	}
	return false
}

// isUserIndexExpr reports whether idx dispatches to a user-defined *non-native*
// `[]` operator. Such a read is an ordinary method call returning an *owned*
// value (freshly constructed / `.lock()`-derived / cloned) — there is no
// container slot to alias, so moving the result out is sound and is already
// made leak-safe at codegen by T0647's trackUserIndexResult. Mirrors codegen's
// isUserIndexExpr (codegen/stmt.go) with one difference: the ownership pass
// runs on the un-monomorphized generic AST, so no typeSubst is applied here.
// extractNamedType peels MutRef/SharedRef/Instance and returns nil for
// *types.Array, so native container indexing and fixed-size array indexing
// (which alias the slot's owned pointer) are NOT exempted and remain correctly
// rejected by rejectIndexExprSingleOwnerMove.
//
// A free function (not a *Checker method) so the last-use analyzer
// (lastUseAnalyzer, which holds *sema.Info but is not a Checker) can share the
// same classification when narrowing closure-aggregate borrows (T0816).
func isUserIndexExpr(info *sema.Info, idx *ast.IndexExpr) bool {
	named := extractNamedType(info.Types[idx.Target])
	if named == nil {
		return false
	}
	m := named.LookupMethod("[]")
	return m != nil && !m.IsNative()
}

// closureAggregateBorrowSource reports whether expr reads a closure (function
// value, `*types.Signature`) out of an *owning aggregate* — a struct/optional
// closure field (`h.cb`, `h.cb!`, `h.cb as! (...)`) or a container element
// (`v[0]`) — and, if so, returns the peeled *ast.MemberExpr / *ast.IndexExpr
// access. Returns nil otherwise.
//
// Codegen treats such a read as a borrow (T0812): the local copies the closure's
// fat pointer `{fn, env}` by value while the aggregate retains sole ownership of
// the heap env (closures aren't Cloneable, so there is no env dup on read), and
// registers no owning env-free binding for the local. That borrow is only sound
// when the aggregate strictly outlives every use of the local. Marking the local
// Borrowed lets the existing escape/consume checks (returnsBorrowAsOwned on
// return, tryMoveConsume on re-store into another aggregate / `~`-param / store /
// raise / yield / channel-send) reject the unsound escapes at compile time, while
// the same-scope read-and-invoke case stays valid (calling a Borrowed closure is
// not a consume). Kept intentionally identical in shape to codegen's
// isClosureAggregateBorrow (codegen/stmt.go) so the two stay in lockstep:
// ownership marks Borrowed iff codegen suppresses the owning env-free binding.
//
// The type gate (closure-typed result) is explicit here — codegen gets it for
// free via maybeRegisterEnvFree's *types.Signature check; without it, non-closure
// field reads (strings, vectors) would be misclassified. Owned-return shapes —
// a getter returning a closure, or a user-defined non-native `[]` — are excluded
// (the local owns a fresh closure and keeps its owning binding). A plain
// *ast.IdentExpr source falls through to nil: a local move transfers ownership.
//
// Exposing the peeled access (rather than just a bool) lets callers recover the
// aggregate's root variable via destructureBorrowRoot so a shared borrow can be
// registered against it (T0816) — preventing the source aggregate from being
// moved/consumed/reassigned while the borrowing local is still live, which would
// free the env out from under it (UAF / double-free). A free function (not a
// *Checker method) so the last-use analyzer (lastUseAnalyzer, which holds
// *sema.Info but is not a Checker) can share the same classification.
func closureAggregateBorrowSource(info *sema.Info, expr ast.Expr) ast.Expr {
	if expr == nil {
		return nil
	}
	// Type gate: only closure-typed reads alias a heap env.
	if _, isSig := info.Types[expr].(*types.Signature); !isSig {
		return nil
	}
	e := unwrapDestructureParens(expr)
	// Peel a force-unwrap of an optional closure field: `h.cb!` or `h.cb as! (...)`.
	if unwrap, ok := e.(*ast.OptionalUnwrapExpr); ok {
		e = unwrapDestructureParens(unwrap.Expr)
	} else if cast, ok := e.(*ast.CastExpr); ok && cast.Force {
		subj := unwrapDestructureParens(cast.Expr)
		if _, isOpt := info.Types[subj].(*types.Optional); isOpt {
			e = subj
		}
	}
	switch e.(type) {
	case *ast.MemberExpr, *ast.IndexExpr:
		// struct/optional closure field, or container element — aliasing read
	default:
		return nil
	}
	// Owned-return shapes: the local owns a fresh closure, keep its binding.
	if mem, ok := e.(*ast.MemberExpr); ok {
		if n := extractNamedType(info.Types[mem.Target]); n != nil {
			if n.LookupGetter(mem.Field) != nil {
				return nil
			}
		}
	}
	if idx, ok := e.(*ast.IndexExpr); ok && isUserIndexExpr(info, idx) {
		return nil
	}
	return e
}

// rejectIndexExprSingleOwnerMove emits an error if expr is an IndexExpr whose
// result type is a single-owner native handle (Mutex/MutexGuard/Task or
// Optional thereof). T0596: dup-on-read is not defined for these container
// types; moving them out of a slot aliases the slot's owned pointer and the
// container's drop walks both copies → double-free / SEGV. Reject at the
// ownership pass with a clear diagnostic. Returns true when rejected so the
// caller can skip the regular move/state bookkeeping.
func (c *Checker) rejectIndexExprSingleOwnerMove(expr ast.Expr) bool {
	// Peel ParenExpr so `(arr[0])` is treated like `arr[0]`. Also peel
	// OptionalUnwrapExpr so `arr[i]!` on an `Optional<Mutex>` slot reaches
	// the inner IndexExpr — without this the cast below fails and the move
	// slips through (T0612 gap B).
	for {
		if p, ok := expr.(*ast.ParenExpr); ok {
			expr = p.Expr
			continue
		}
		if u, ok := expr.(*ast.OptionalUnwrapExpr); ok {
			expr = u.Expr
			continue
		}
		break
	}
	idx, ok := expr.(*ast.IndexExpr)
	if !ok {
		return false
	}
	typ := c.info.Types[idx]
	// T0650: a user-defined non-native `[]` on a *plain user type* is an ordinary
	// method returning a *freshly-constructed / `.lock()`-derived owned* value —
	// there is no container slot to alias. T0647's trackUserIndexResult makes that
	// owned return leak-safe at codegen, so it is exempt.
	//
	// T1113 EXCEPTION: a std native container (Vector/Map) returns the slot's
	// element BY VALUE, aliasing internal storage. Vector's `[]` is native (so
	// isUserIndexExpr is already false), but Map's `[]` is a Promise method —
	// isUserIndexExpr would wrongly exempt it. A single-owner handle in the element
	// is not duped on read (no copy semantics), so the read aliases the slot's
	// owned pointer exactly like a native slot. Do NOT exempt aliasing containers
	// for the handle checks below.
	exemptUserIndex := isUserIndexExpr(c.info, idx) && !indexTargetIsAliasingContainer(c.info, idx)
	if exemptUserIndex {
		return false
	}
	if isSingleOwnerNativeType(typ) {
		c.errorf(idx.Pos(),
			"cannot move %s out of indexed slot; this is a single-owner native handle with no copy/clone semantics — use a fresh constructor for the slot, or call a method that returns a borrow (e.g. `.lock()`)",
			typ.String())
		return true
	}
	// T1113: the index RESULT is itself an enum/struct (so isSingleOwnerNativeType
	// is false), but it transitively OWNS a single-owner handle through a user-type
	// field or enum variant field. A native container read produces a by-value
	// copy whose nested handle pointer aliases the container's slot — moving the
	// copy out and dropping it frees memory the container still references
	// (double-free / UAF). There is no copy semantics for the handle, so reject
	// the read. (Refcounted nesting — Ref[Mutex], Channel[Task], enum{Ref} — is
	// excluded by FirstFieldNestedSingleOwnerHandle treating std containers as
	// opaque, so those sound reads still compile.)
	if off := sema.FirstFieldNestedSingleOwnerHandle(typ); off != nil {
		c.errorf(idx.Pos(),
			"cannot read %s out of indexed slot; it transitively contains %s, a single-owner native handle with no copy/clone semantics — indexing copies the element and would alias the container's handle (double-free at drop). Construct a fresh value for the slot, or call .remove()/.pop() to take ownership of an element.",
			typ.String(), off.String())
		return true
	}
	return false
}

// indexTargetIsAliasingContainer reports whether idx reads an element out of a
// std native container (Vector / Map) whose `[]` returns the slot's element BY
// VALUE, aliasing internal storage — as opposed to a plain user type's `[]`,
// which constructs a fresh owned value (T0650). The distinction matters only for
// the single-owner-handle gate (T1113): a handle element is not duped on read,
// so an aliasing container's read shares the slot's owned pointer (double-free /
// UAF), while a fresh-returning user `[]` is sound. Vector's `[]` is native (so
// isUserIndexExpr already rejects exemption); Map's is a Promise method, so this
// catches the Map case that isUserIndexExpr would otherwise wrongly exempt.
func indexTargetIsAliasingContainer(info *sema.Info, idx *ast.IndexExpr) bool {
	t := info.Types[idx.Target]
	if _, ok := types.AsVector(t); ok {
		return true
	}
	if _, _, ok := types.AsMap(t); ok {
		return true
	}
	return false
}

// peelToMemberSource peels ParenExpr / OptionalUnwrapExpr wrappers off expr and
// returns the underlying MemberExpr (e.g. the `owner.field` inside `(owner.field!)`),
// or nil when the peeled expression is not a member access.
func peelToMemberSource(expr ast.Expr) *ast.MemberExpr {
	for {
		switch e := expr.(type) {
		case *ast.ParenExpr:
			expr = e.Expr
		case *ast.OptionalUnwrapExpr:
			expr = e.Expr
		default:
			if m, ok := expr.(*ast.MemberExpr); ok {
				return m
			}
			return nil
		}
	}
}

// memberChainRoot walks a (possibly chained, paren-wrapped, indexed) MemberExpr
// down to its root IdentExpr / ThisExpr and returns the root expr plus its
// variable name ("this" for a ThisExpr). The bool is false when the chain bottoms
// out on a non-variable target (call result, literal) — a transient owner. The
// root expr is still returned in that case (so the caller can classify the
// transient owner for diagnostics); only the bool and name reflect variable-ness.
// Index hops are walked through to the indexed container's root: a container
// element rooted in a variable (`cs[0].t`) is owned storage governed by that
// variable, not a transient, so it resolves to the same variable-root logic
// rather than the unconditional transient reject. This keeps the legitimate
// owned-container Task case working (the *non-optional* `<-cs[0].t`, made safe by
// genReceiveTask's T0638 slot-null). Awaiting an *optional* Task field, and moving
// an *optional* Mutex/Task field, out of an owned container element
// (`<-(cs[0].tsk!)`, `Mutex[int] m = cs[0].mtx!`) is likewise safe: the T0638
// slot-null does not reach through the OptionalUnwrap+index, but
// neutralizeMemberOptionalField now clears the element's optional present flag for
// that shape (T0843 — which thereby also covers the *optional* half of T0842),
// parallel to the non-optional slot-null. NOTE one remaining gap this walk-through
// exposes but does NOT fix (pre-existing, latent double-free, not papered over
// here): moving a NON-optional handle field out of an owned container element
// (`Mutex[int] m = cs[0].m`, no `!`) has neither a slot-null nor an optional present
// flag to clear — see T0842.
func (c *Checker) memberChainRoot(m *ast.MemberExpr) (ast.Expr, string, bool) {
	target := m.Target
	for {
		switch t := target.(type) {
		case *ast.ParenExpr:
			target = t.Expr
		case *ast.MemberExpr:
			target = t.Target
		case *ast.IndexExpr:
			target = t.Target
		case *ast.ThisExpr:
			return t, "this", true
		case *ast.IdentExpr:
			if obj := c.info.Objects[t]; obj != nil {
				if _, isVar := obj.(*types.Var); isVar {
					return t, t.Name, true
				}
			}
			return t, "", false
		default:
			return target, "", false
		}
	}
}

// rejectMemberHandleMoveOutOfBorrow rejects moving or consuming a single-owner
// native handle field (Mutex/MutexGuard/Task) out of a *shared-borrow* owner
// (T0837). Shapes like `Mutex[int] m = borrowed.mtx!` or `<-(borrowed.tsk!)`
// alias the handle's i8* while the real owner (in the caller) still drops it →
// double-free. Heap-user-type fields are masked by T0428 Case 3B's independent
// dup; single-owner handles have no copy/clone semantics, so the move/consume
// must be rejected here. Owned owners (`~this`, owned local, `~` param) keep
// the field live and are correctly handled by codegen's neutralize/temp-tracking
// (T0806), so they are NOT rejected. Returns true when rejected so the caller
// can skip the regular move/state bookkeeping.
func (c *Checker) rejectMemberHandleMoveOutOfBorrow(expr ast.Expr) bool {
	member := peelToMemberSource(expr)
	if member == nil {
		return false
	}
	if !isSingleOwnerNativeType(c.info.Types[member]) {
		return false
	}
	// Getter calls return freshly produced owned values — no field move involved
	// (mirrors checkFieldMoveOwnership's getter guard).
	ownerType := c.info.Types[member.Target]
	if n := extractNamedType(ownerType); n != nil {
		if n.LookupGetter(member.Field) != nil {
			return false
		}
	}
	root, rootName, isVar := c.memberChainRoot(member)
	// Peel the Optional wrapper (`Task[int]?`) before the Task check below; both
	// branches use handleType to tailor the remedy.
	handleType := c.info.Types[member]
	if opt, ok := handleType.(*types.Optional); ok {
		handleType = opt.Elem()
	}
	if !isVar {
		// T0841: transient / non-variable owner (call result, constructor literal,
		// ...). The owner is dropped at the end of the full expression, so
		// moving/consuming the single-owner handle out of it aliases the i8* the
		// owner will free → double-free. Unlike the variable case there is no
		// owned-local escape hatch for a temporary, so reject unconditionally.
		// (Indexed owners rooted in a variable, e.g. `cs[0].t`, are owned storage
		// and were already walked through to their variable root in
		// memberChainRoot — they never reach this branch.)
		ownerDesc := "a temporary"
		if _, isCall := root.(*ast.CallExpr); isCall {
			ownerDesc = "a call result"
		}
		remedy := "bind the owner to a local first, then move the field"
		if !types.IsTask(handleType) {
			remedy = fmt.Sprintf("read it in place through a borrow (e.g. `(...%s!).lock()`), or %s", member.Field, remedy)
		}
		c.errorf(member.Pos(),
			"cannot move single-owner handle field '%s' (%s) out of %s; it is dropped at the end of this expression, so moving/consuming the handle here would double-free — %s",
			member.Field, c.info.Types[member].String(), ownerDesc, remedy)
		return true
	}
	// Borrow discriminator: reject only when the owner is a shared borrow.
	// `&this` receivers, plain non-`~`/`&` borrowed params, and Borrowed-state
	// locals carry Borrowed state. Explicit `&owner` params/locals carry an Owned
	// state (the SharedRef itself is Copy), so check the static type too. A
	// `~`/RefMut owner is owned by the callee → not a borrow → not rejected.
	borrowed := c.state[rootName] == Borrowed
	if !borrowed {
		if _, isShared := c.info.Types[root].(*types.SharedRef); isShared {
			borrowed = true
		}
	}
	if !borrowed {
		return false
	}
	// Tailor the remedy to the handle: a Mutex can be read in place via the
	// borrowing `.lock()`, but a Task has no in-place read (the only operation,
	// `<-`, consumes it) — so for a Task only the take-ownership remedy applies.
	remedy := "take ownership with a `~this` receiver or a `move` parameter"
	if !types.IsTask(handleType) {
		remedy = fmt.Sprintf("read it in place through a borrow (e.g. `(%s.%s!).lock()`), or %s", rootName, member.Field, remedy)
	}
	c.errorf(member.Pos(),
		"cannot move single-owner handle field '%s' (%s) out of borrowed '%s'; the real owner still drops it, so moving/consuming the handle here would double-free — %s",
		member.Field, c.info.Types[member].String(), rootName, remedy)
	return true
}

// rejectBorrowedTaskAwait rejects `<-` (await) on a Task[T] the current function
// does not own. `<-` on a Task is a *consuming* op: genReceiveTask joins the
// goroutine and frees the G struct + result buffer. If the awaited Task is not
// owned here — a borrowed ident (`<-a`), an inline elvis whose selected operand is
// a borrow (`<-(a ?: b)`), or a single-owner handle field read out of a borrowed/
// transient owner (`<-(borrowed.tsk!)`) — the real owner still drops (joins+frees)
// it at scope exit, so awaiting it here double-joins/double-frees the same G → SEGV
// (T0837/T0953).
//
// The IsTask gate is essential: Channel receive is also spelled `<-` but is
// non-consuming, so borrowed-channel receives must stay legal. The gate is keyed on
// the operand's static type — for elvis/if/match operands that is the merged result
// type (Task vs Channel), so the whole receive is classified correctly.
func (c *Checker) rejectBorrowedTaskAwait(operand ast.Expr) {
	if !types.IsTask(c.info.Types[unwrapDestructureParens(operand)]) {
		return
	}
	c.rejectAwaitNonOwnedSource(operand)
}

// rejectAwaitNonOwnedSource walks an awaited Task operand through every transparent
// wrapper the receive surfaces the handle through and rejects the first non-owned
// leaf. Returns true once a leaf is rejected so recursion short-circuits (one error
// per await). Wrappers peeled/recursed: paren and force-unwrap (`!`, via
// peelAwaitWrappers), elvis (`?:`), if/else, match. Leaves:
//   - IdentExpr in Borrowed state → reject (the caller still owns + drops the task).
//   - member / index / transient owner → delegate to rejectMemberHandleMoveOutOfBorrow
//     (T0837), which rejects a single-owner handle field read out of a borrowed or
//     transient owner and leaves owned-owner reads (`~this`, owned local, owned
//     container element) legal.
//
// The if/match recursion mirrors findBorrowedNonAliasSafeIdent: codegen forwards the
// branch/arm value directly, so a non-owned Task surfaced through any branch reaches
// the consuming `<-` as the same alias the real owner still drops.
func (c *Checker) rejectAwaitNonOwnedSource(expr ast.Expr) bool {
	expr = peelAwaitWrappers(expr)
	switch e := expr.(type) {
	case *ast.IdentExpr:
		if c.state[e.Name] != Borrowed {
			return false
		}
		kind := "value"
		if c.params[e.Name] {
			kind = "parameter"
		}
		c.errorf(e.Pos(),
			"cannot await borrowed task %s '%s'; `<-` consumes (joins and frees) the task, but the owner still drops it → double-free — take ownership with a `move` parameter, or await it where it is owned",
			kind, e.Name)
		return true
	case *ast.BinaryExpr:
		if e.Op != ast.BinElvis {
			return false
		}
		if c.rejectAwaitNonOwnedSource(e.Left) {
			return true
		}
		return c.rejectAwaitNonOwnedSource(e.Right)
	case *ast.IfExpr:
		if c.rejectAwaitNonOwnedSourceInBlock(e.Then) {
			return true
		}
		return c.rejectAwaitNonOwnedSourceInBlock(e.Else)
	case *ast.MatchExpr:
		for _, arm := range e.Arms {
			if arm.Body != nil && c.rejectAwaitNonOwnedSource(arm.Body) {
				return true
			}
			if arm.Block != nil && c.rejectAwaitNonOwnedSourceInBlock(arm.Block) {
				return true
			}
		}
		return false
	}
	// member / index / transient owner (incl. `borrowed.tsk!`, `make_h().tsk!`).
	return c.rejectMemberHandleMoveOutOfBorrow(expr)
}

// rejectAwaitNonOwnedSourceInBlock recurses into a block's trailing expression (its
// result value). Mirrors findBorrowedNonAliasSafeIdentInBlock.
func (c *Checker) rejectAwaitNonOwnedSourceInBlock(block *ast.Block) bool {
	if block == nil || len(block.Stmts) == 0 {
		return false
	}
	if es, ok := block.Stmts[len(block.Stmts)-1].(*ast.ExprStmt); ok {
		return c.rejectAwaitNonOwnedSource(es.Expr)
	}
	return false
}

// peelAwaitWrappers peels paren and force-unwrap (`!`) wrappers from an awaited
// operand so the underlying ident/member leaf is reached. Force-unwrap is peeled
// because `<-(a!)` / `<-(borrowed.tsk!)` surface the inner Task to the consuming
// receive unchanged — the awaited handle is the same alias the real owner drops.
func peelAwaitWrappers(expr ast.Expr) ast.Expr {
	for {
		switch e := expr.(type) {
		case *ast.ParenExpr:
			expr = e.Expr
		case *ast.OptionalUnwrapExpr:
			expr = e.Expr
		default:
			return expr
		}
	}
}

// isAutoDupType returns true for types that codegen auto-dups on field read:
// string, Vector[T]/T[], Channel[T]/channel[T], and Optional wrapping any of these.
func isAutoDupType(typ types.Type) bool {
	if n := extractNamedType(typ); n != nil && n == types.TypString {
		return true
	}
	if types.IsVector(typ) {
		return true
	}
	if types.IsChannel(typ) {
		return true
	}
	if opt, ok := typ.(*types.Optional); ok {
		return isAutoDupType(opt.Elem())
	}
	return false
}

// isDroppableOwner returns true if the type has drop semantics (explicit or synthesized).
// extractNamedType handles Named, Instance, SharedRef, MutRef; the Enum case
// covers enums directly (extractNamedType does not unwrap Enum).
func isDroppableOwner(typ types.Type) bool {
	if n := extractNamedType(typ); n != nil {
		if n.HasDrop() || n.NeedsSynthDrop() {
			return true
		}
	}
	if inst, ok := typ.(*types.Instance); ok {
		if instanceHasDroppableField(inst) {
			return true
		}
	}
	if e, ok := typ.(*types.Enum); ok {
		return e.HasDrop() || e.NeedsSynthDrop()
	}
	return false
}

// isDroppableType reports whether a type has drop semantics (explicit or synthesized).
// For Optional and Tuple types, recurses into the wrapped/element types.
// For generic Instance types, handles both *types.Named and *types.Enum origins
// via substitution-aware helpers. Mirrors codegen's monoTypeHasDroppable (T0506).
//
// For *types.Named and *types.Instance (with Named origin), also applies the
// B0192 catch-all: a non-value, non-structural, non-copy Named is droppable
// because codegen synthesizes `pal_free` for it at scope exit. This brings
// ownership into alignment with sema's `fieldTypeHasDrop` and codegen's
// `monoTypeHasDroppable`, which both treat plain heap user types (e.g.
// `_Plain { int n; }` with no drop method and only primitive fields) as
// droppable (T0549).
func isDroppableType(typ types.Type) bool {
	switch t := typ.(type) {
	case *types.Named:
		if t.HasDrop() || t.NeedsSynthDrop() {
			return true
		}
		return !t.IsValueType() && !t.IsStructural() && !isCopyType(t)
	case *types.Instance:
		if n, ok := t.Origin().(*types.Named); ok {
			if n.HasDrop() || n.NeedsSynthDrop() {
				return true
			}
			if instanceHasDroppableField(t) {
				return true
			}
			return !n.IsValueType() && !n.IsStructural() && !isCopyType(n)
		}
		if e, ok := t.Origin().(*types.Enum); ok {
			if e.HasDrop() || e.NeedsSynthDrop() {
				return true
			}
			return enumInstanceHasDroppableField(t)
		}
		return false
	case *types.Enum:
		return t.HasDrop() || t.NeedsSynthDrop()
	case *types.Optional:
		return isDroppableType(t.Elem())
	case *types.Tuple:
		for _, e := range t.Elems() {
			if isDroppableType(e) {
				return true
			}
		}
		return false
	}
	return false
}

// instanceHasDroppableField reports whether a generic Instance with concrete
// TypeArgs has any field whose substituted type is droppable. Mirrors codegen's
// monoInstNeedsSynthDrop (compiler/internal/codegen/mono.go): sema's
// fieldTypeHasDrop skips TypeParam fields by design, so a generic origin like
// Holder[T]{T? v} has HasDrop=false and NeedsSynthDrop=false even when
// Holder[_BoxDrop] is droppable. The ownership checker needs the
// substitution-aware view to reject moves that would otherwise double-free at
// runtime (T0473).
func instanceHasDroppableField(inst *types.Instance) bool {
	named, ok := inst.Origin().(*types.Named)
	if !ok {
		return false
	}
	if named.IsCopy() || named.IsValueType() || named.IsStructural() {
		return false
	}
	// Don't apply when TypeArgs aren't concrete (e.g. inside a generic method
	// body where the outer T is still a TypeParam) — preserves the existing
	// "skip on TypeParam" semantics that callers already enforce on the
	// fieldType side.
	for _, ta := range inst.TypeArgs() {
		if types.ContainsTypeParam(ta) {
			return false
		}
	}
	subst := types.BuildSubstMap(named.TypeParams(), inst.TypeArgs())
	for _, f := range named.AllFields() {
		if !types.ContainsTypeParam(f.Type()) {
			continue // sema already accounted for it via origin's flags
		}
		ft := types.Substitute(f.Type(), subst)
		if isDroppableType(ft) {
			return true
		}
	}
	return false
}

// enumInstanceHasDroppableField reports whether a generic enum Instance with
// concrete TypeArgs has any variant field whose substituted type is droppable.
// Mirrors codegen's monoEnumInstNeedsSynthDrop (compiler/internal/codegen/mono.go):
// sema's fieldTypeHasDrop skips TypeParam variant fields, so a generic enum
// origin like Maybe[T]{Just(T)} has HasDrop=false and NeedsSynthDrop=false even
// when Maybe[_BoxDrop] is droppable. Without this, the ownership pass treats
// Maybe[_BoxDrop] as non-droppable and lets the bare field move through,
// producing a runtime double-free (T0506).
func enumInstanceHasDroppableField(inst *types.Instance) bool {
	enum, ok := inst.Origin().(*types.Enum)
	if !ok {
		return false
	}
	if enum.IsCopy() {
		return false
	}
	// Skip when TypeArgs aren't concrete (e.g. inside a generic method body
	// where the outer T is still a TypeParam) — preserves the existing
	// "skip on unresolved TypeParam" semantics.
	for _, ta := range inst.TypeArgs() {
		if types.ContainsTypeParam(ta) {
			return false
		}
	}
	subst := types.BuildSubstMap(enum.TypeParams(), inst.TypeArgs())
	for _, v := range enum.Variants() {
		for _, f := range v.Fields() {
			if !types.ContainsTypeParam(f.Type()) {
				continue // sema already accounted for it via origin's flags
			}
			ft := types.Substitute(f.Type(), subst)
			if isDroppableType(ft) {
				return true
			}
		}
	}
	return false
}

// isValueTarget returns true when the root of a MemberExpr chain is a
// variable (owned value) or a function call (owned temporary). Returns
// false for type names (Enum.Variant), module references
// (json.JsonValue.Null), and other non-value targets.
func (c *Checker) isValueTarget(e *ast.MemberExpr) bool {
	target := e.Target
	for {
		switch t := target.(type) {
		case *ast.MemberExpr:
			target = t.Target
		case *ast.IndexExpr:
			// T0842: a container element (`cs[0].m`) is itself an owned value —
			// peel through to the container's root so an element-field move out
			// of an owned container is rejected like an owned-local field move.
			target = t.Target
		case *ast.ParenExpr:
			target = t.Expr
		default:
			goto rooted
		}
	}
rooted:
	// Function calls return owned temporaries — field reads from them
	// have the same double-drop risk as field reads from variables (B0351).
	if _, ok := target.(*ast.CallExpr); ok {
		return true
	}
	// T0411: `this.field` is also a value-target — `this` is a borrowed or
	// consumed owner whose drop will free its fields, so moving a non-auto-dup
	// droppable field out of `this` has the same double-drop risk as moving
	// from an owned local or a call temp.
	if _, ok := target.(*ast.ThisExpr); ok {
		return true
	}
	if ident, ok := target.(*ast.IdentExpr); ok {
		if obj := c.info.Objects[ident]; obj != nil {
			_, isVar := obj.(*types.Var)
			return isVar
		}
	}
	return false
}

// checkFieldMoveOwnership rejects reading a droppable, non-auto-dup field from
// a droppable owner in an ownership-transfer context (B0341). The user must
// call .clone() to create an independent copy.
func (c *Checker) checkFieldMoveOwnership(e *ast.MemberExpr) {
	// Skip enum variant constructors (Enum.Variant), module references
	// (json.JsonValue.Null), etc. — only field reads from owned values matter.
	if !c.isValueTarget(e) {
		return
	}
	fieldType := c.info.Types[e]
	if fieldType == nil {
		return
	}
	if isCopyType(fieldType) {
		return
	}
	if types.ContainsTypeParam(fieldType) {
		return
	}
	ownerType := c.info.Types[e.Target]
	if ownerType == nil {
		return
	}
	if !isDroppableOwner(ownerType) {
		return
	}
	// Getter calls return owned values — no field move involved (T0591).
	if n := extractNamedType(ownerType); n != nil {
		if n.LookupGetter(e.Field) != nil {
			return
		}
	}
	if inst, ok := ownerType.(*types.Instance); ok {
		if enum, ok := inst.Origin().(*types.Enum); ok {
			if enum.LookupGetter(e.Field) != nil {
				return
			}
		}
	}
	if enum, ok := ownerType.(*types.Enum); ok {
		if enum.LookupGetter(e.Field) != nil {
			return
		}
	}
	if isAutoDupType(fieldType) {
		return
	}
	// Only error if the field type itself is droppable — non-droppable
	// non-Copy types (fieldless enums, etc.) have value semantics and
	// are safe to shallow-copy without causing double-free.
	if !isDroppableType(fieldType) {
		return
	}
	c.errorf(e.Pos(), "cannot move field '%s' out of '%s' — use .clone() to create an independent copy",
		e.Field, ownerType)
}

// --- Lambda expressions ---

func (c *Checker) checkLambdaExpr(e *ast.LambdaExpr) {
	// Mark move-captured variables as moved in the enclosing scope.
	// T0338: a `move` capture of a borrowed parameter would let the lambda
	// env take ownership of a value the outer caller still drops — the same
	// double-free pattern as moving into a constructor field. Reject it
	// with the same hint to add `~`. (Skip `this`: capturing `this` from a
	// non-`~this` method is a common closure-returning-method pattern;
	// codegen treats the captured `this` as a borrowed pointer rather than
	// adding it to the env-drop chain.)
	if captures := c.info.LambdaCaptures[e]; len(captures) > 0 {
		for _, cv := range captures {
			if !cv.ByMove {
				continue
			}
			name := cv.Obj.Name()
			if name == "this" {
				continue
			}
			if c.state[name] == Borrowed {
				if c.params[name] {
					c.errorf(e.Pos(),
						"cannot move-capture borrowed parameter '%s' into a lambda; declare the parameter with `move` to consume it",
						name)
				} else {
					c.errorf(e.Pos(), "cannot move-capture borrowed value '%s' into a lambda", name)
				}
				continue
			}
			c.state[name] = Moved
		}
	}

	// Check lambda body in isolation
	savedState := c.state.clone()
	savedBorrows := c.borrows.Clone()
	savedSig := c.curSig
	savedParams := c.params
	savedReturnOrigins := c.returnOrigins
	// T1151: a var-decl inside a lambda body is owned by the closure frame, not
	// iteration-bounded — reset loop depth so such locals are not flagged even
	// when the lambda is lexically inside a loop.
	savedLoopDepth := c.loopDepth
	c.loopDepth = 0

	// T0426: use the lambda's own signature for return checks. Without this,
	// `checkReturnRefSafety` reads the outer fn's c.curSig, producing both
	// false negatives (outer void → skips T0402) and false positives (outer
	// owned T → rejects legit `return &capture` from a lambda returning T&).
	// Sema records the lambda's signature in info.Types[e].
	lambdaSig, _ := c.info.Types[e].(*types.Signature)
	c.curSig = lambdaSig
	c.params = make(map[string]bool)
	c.returnOrigins = nil

	// Captured variables are fresh owned values inside the lambda. Treat
	// them as parameter-like for return-ref checks so `return &capture`
	// from a `move ||` lambda is allowed (the capture lives in the env,
	// whose lifetime is tied to the closure value).
	if captures := c.info.LambdaCaptures[e]; len(captures) > 0 {
		for _, cv := range captures {
			name := cv.Obj.Name()
			c.state[name] = Owned
			if name != "_" && name != "this" {
				c.params[name] = true
			}
		}
	}

	for _, p := range e.Params {
		if p.Name != "_" {
			c.state[p.Name] = Owned
			c.params[p.Name] = true
		}
	}
	if e.Body != nil {
		c.checkBlock(e.Body)
	}
	if e.ExprBody != nil {
		c.checkExpr(e.ExprBody)
	}
	c.checkReturnAmbiguity()

	c.state = savedState
	c.borrows = savedBorrows
	c.curSig = savedSig
	c.params = savedParams
	c.returnOrigins = savedReturnOrigins
	c.loopDepth = savedLoopDepth
}
