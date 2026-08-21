package ownership

import (
	"github.com/promise-language/promise/compiler/internal/ast"
	"github.com/promise-language/promise/compiler/internal/types"
)

// T1640 — closure env ownership across a `go` boundary.
//
// A closure value is a fat pointer `{fn, env}` whose `env` is a heap allocation
// freed by a `bindingFreeEnv` at exit of the scope that OWNS it. Now that
// isSendableType accepts *types.Signature (R1), a closure can be captured into a
// goroutine — and a goroutine may outlive the defining scope. Two rules make that
// sound, and they are enforced here:
//
//	R4 — the capture MOVES the closure. The captured binding becomes Moved in the
//	     enclosing scope, so a post-spawn use is a compile error rather than a
//	     silent read of memory the coroutine now owns. This mirrors codegen's
//	     B0354 transfer (the outer drop flag is cleared after the spawn), and the
//	     move-marking of `move`-captured lambda variables in checkLambdaExpr.
//
//	R5 — only a binding that provably OWNS its env may cross. A borrow of someone
//	     else's env (a non-`move` parameter, `h.cb`, a match-borrowed payload
//	     alias) must not be transferred: the real owner still frees it, so the
//	     coroutine would read freed memory. R5 is a fail-CLOSED allowlist — an
//	     unrecognised binding shape is rejected, because the failure mode of a
//	     missed shape is a use-after-free while the failure mode of an over-strict
//	     rule is a compile error that `move` resolves.
//
// R5 is the closure-shaped slice of T1397 (§17.4, "a borrow may never cross a
// `go` spawn boundary"); it does not pre-empt it. When T1397 lands and requires
// the inner-binding spelling `go { F h = handler; … }`, R4's move-marking and
// codegen's transfer remain exactly what that spelling needs.
//
// KNOWN RESIDUAL — T1650: a closure whose env borrows its defining receiver
// (`|| -> this.n`) is caught here only when the taint is visible in the same
// function (recordIterBorrowTaint / T1349). Arriving through a `move` parameter
// the taint is not tracked across the call boundary, and that is precisely
// `web.on`'s shape. Tracked with its repro and candidate fixes in T1650.

// checkGoClosureCaptures applies R4 and R5 to the closure-typed captures of a
// `go { … }` block. Called after the block body has been checked, so a capture is
// marked Moved only once its in-block uses have been validated.
func (c *Checker) checkGoClosureCaptures(e *ast.GoExpr) {
	for _, cv := range c.info.GoCaptures[e] {
		v, ok := cv.Obj.(*types.Var)
		if !ok {
			continue
		}
		name := v.Name()
		if name == "" || name == "_" || name == "this" {
			continue
		}
		if !isClosureType(v.Type()) {
			continue
		}
		if reason := c.closureEnvBorrowReason(name); reason != "" {
			c.errorf(e.Pos(),
				"cannot capture closure '%s' into a goroutine: %s; the goroutine may outlive '%s', so it must OWN the closure's environment — bind an owned closure (a lambda, a named function, or a call result) inside the goroutine, or take it as a `move` parameter",
				name, reason, name)
			continue
		}
		// R4: the goroutine takes ownership — the enclosing scope must not use it
		// again. noteLoopMoveSite first, so a capture inside a loop body is caught
		// by the existing loop-move machinery (a second iteration would move an
		// already-moved closure).
		c.noteLoopMoveSite(name, e.Pos(), v.Type())
		c.state[name] = Moved
	}
}

// rejectGoCallClosureBorrow applies R5 to the *call* form, `go f(handler)`. The
// only sound spelling is `move <ident>` into a consuming slot: that is the one
// shape where the caller surrenders the env (its drop flag is cleared at the call
// site) and the callee owns it for the goroutine's whole lifetime. Everything else
// is rejected:
//
//   - a plain ident in a borrow slot — the caller's binding still frees the env
//     while the goroutine is using it (use-after-free);
//   - a non-ident closure expression (`h.cb`, `v[0]`) — the aggregate frees it;
//   - a TEMPORARY (`go f(|x| -> …)`, `go f(make_cb())`) — the spawning scope frees
//     the temp env at statement end. In a borrow slot the goroutine then reads
//     freed memory; in a consuming slot the callee frees it a second time
//     ("fatal: invalid free"). `move` cannot rescue it either — `move` applies
//     only to a named binding — so the temp must be bound to a local first.
//     Transferring a go-call temp into the coroutine frame is T1654.
//
// Runs BEFORE checkExpr so the diagnostic fires on the original binding state
// (checkExpr's tryMove would otherwise have already marked a `move` argument).
func (c *Checker) rejectGoCallClosureBorrow(e *ast.CallExpr) {
	var params []*types.Param
	if sig := c.calleeSignature(e.Callee); sig != nil {
		params = sig.Params()
	}
	storeNative := c.isElementStoringNativeCall(e.Callee)
	for i, arg := range e.Args {
		if arg.Move {
			continue // explicit transfer — the caller surrenders the env
		}
		if !isClosureType(c.info.Types[arg.Value]) {
			continue
		}
		id := identRoot(arg.Value)
		if id == nil {
			if c.freshOwnedClosureExpr(arg.Value) {
				c.errorf(arg.Value.Pos(),
					"cannot pass a temporary closure across a 'go' boundary; the spawning scope frees the closure's environment at the end of the statement while the goroutine may still be using it — bind it to a local first and pass that with `move`")
			} else {
				c.errorf(arg.Value.Pos(),
					"cannot pass a borrowed closure across a 'go' boundary; this expression aliases an environment owned elsewhere, which the owner frees while the goroutine may still be using it — bind an owned closure to a local and pass that with `move`")
			}
			continue
		}
		// Skip CONSUMING slots for a NAMED argument, exactly as
		// rejectGoCallLoopBindingBorrowEscape does: ownership transfers into the
		// callee there, and the existing "consuming 'f' requires `move f`"
		// diagnostic already demands the `move`. Reporting here too would only
		// duplicate it. A temporary gets no such diagnostic, which is why the
		// check above runs first.
		if i < len(params) {
			kind := paramBorrowKind(params[i])
			if params[i].Ref() == types.RefMut || (kind == BorrowNone && storeNative) {
				continue
			}
		}
		c.errorf(id.Pos(),
			"cannot pass a borrow of closure '%s' across a 'go' boundary; the goroutine may outlive '%s' and would then use its freed environment — pass it with `move %s`",
			id.Name, id.Name, id.Name)
	}
}

// closureEnvBorrowReason reports why the closure binding `name` does NOT own its
// heap env, or "" when it provably does (R5's fail-closed allowlist).
//
// Two independent conditions must hold:
//
//   - The binding is not a borrow. Ownership already classifies every borrowing
//     shape — a non-`move` parameter (paramInitialState → Borrowed), an aggregate
//     read `h.cb` / `v[0]` (closureAggregateBorrowSource → Borrowed), a
//     destructured or for-in alias binding — so `c.state` is consulted rather
//     than re-derived here; that keeps this check and the borrow classification a
//     single source of truth.
//   - The binding was recorded as owning a FRESH env by closureOwnedLocals, which
//     is the positive half of the allowlist: a `move` parameter, or a local bound
//     from a fresh-owned RHS. Anything not recorded is rejected even if `c.state`
//     happens to say Owned.
//
// The T1349 iterator-borrow taint is checked too: a closure returned from a
// method that captures its receiver (`c.make_getter()`) owns its env but that env
// borrows the local receiver, which the enclosing scope still frees.
func (c *Checker) closureEnvBorrowReason(name string) string {
	switch c.state[name] {
	case Borrowed:
		if c.params[name] {
			return "it is a borrowed parameter, not an owned value"
		}
		return "it borrows an environment owned elsewhere"
	case Moved:
		return "" // already reported as a use-of-moved by the normal path
	}
	if origin := c.iterBorrowOrigin[name]; origin != "" {
		return "its environment borrows local '" + origin + "'"
	}
	if !c.closureOwnedLocals[name] {
		return "it is not known to own its environment"
	}
	return ""
}

// recordClosureOwnedLocal records that `name` binds a closure whose heap env it
// OWNS, which is the positive half of R5's allowlist. Called from every var-decl
// and assignment site; `value` is the RHS.
func (c *Checker) recordClosureOwnedLocal(name string, value ast.Expr, typ types.Type) {
	if name == "" || name == "_" || c.closureOwnedLocals == nil {
		return
	}
	if !isClosureType(typ) {
		delete(c.closureOwnedLocals, name)
		return
	}
	if c.freshOwnedClosureExpr(value) {
		c.closureOwnedLocals[name] = true
	} else {
		delete(c.closureOwnedLocals, name) // reassignment clears a stale record
	}
}

// recordClosureOwnedParam records a `move` closure parameter as owning its env.
// The caller cleared its drop flag at the call site, so the callee is the owner.
//
// This is where T1650's residual lives: a closure whose env BORROWS its defining
// receiver arrives here indistinguishable from one that owns everything it
// captured, because a function type erases the capture set and the T1349 taint is
// not propagated across the call boundary. Accepted deliberately; see T1650.
func (c *Checker) recordClosureOwnedParam(p *types.Param) {
	if c.closureOwnedLocals == nil || p.Ref() != types.RefMut || !isClosureType(p.Type()) {
		return
	}
	if n := p.Name(); n != "" && n != "_" {
		c.closureOwnedLocals[n] = true
	}
}

// freshOwnedClosureExpr reports whether a closure-typed RHS produces a FRESH,
// owned heap env rather than aliasing one that lives elsewhere.
//
// Deliberately an allowlist of the shapes codegen gives an owning
// `bindingFreeEnv`: a lambda literal, a named-function reference (env is null —
// nothing to free, trivially safe), a call/method result, a getter (the third
// getter kind, module getters, included via ModuleGetters — mirrors codegen's
// isGetterCallExpr), a user-defined non-native `[]`, and a channel receive.
// Error/optional wrappers are peeled. A bare *ast.IdentExpr is NOT accepted: a
// local-to-local move is sound, but distinguishing it from an alias of a borrowed
// binding needs the state check the caller already applies, and R5 is fail-closed.
func (c *Checker) freshOwnedClosureExpr(e ast.Expr) bool {
	switch ex := e.(type) {
	case nil:
		return false
	case *ast.ParenExpr:
		return c.freshOwnedClosureExpr(ex.Expr)
	case *ast.ErrorPropagateExpr:
		return c.freshOwnedClosureExpr(ex.Expr)
	case *ast.ErrorPanicExpr:
		return c.freshOwnedClosureExpr(ex.Expr)
	case *ast.OptionalUnwrapExpr:
		return c.freshOwnedClosureExpr(ex.Expr)
	case *ast.AutoCloneExpr:
		return c.freshOwnedClosureExpr(ex.Expr)
	case *ast.LambdaExpr:
		return true
	case *ast.UnaryExpr:
		// `<-ch` — a received closure was moved into the channel by its sender.
		return ex.Op == ast.UnaryReceive
	case *ast.CallExpr:
		return true
	case *ast.MemberExpr:
		// A getter returns a fresh owned value; a plain field read aliases the
		// aggregate's env (and is already marked Borrowed by the caller's state
		// check — this arm just keeps the two classifications aligned).
		return c.isGetterMember(ex)
	case *ast.IndexExpr:
		// A user-defined non-native `[]` returns a fresh owned value; native
		// container indexing aliases container storage.
		return c.isUserIndexRead(ex)
	case *ast.IdentExpr:
		// A named-function reference has a NULL env — there is nothing to free, so
		// crossing the boundary is trivially safe. A closure-typed *variable*
		// reference falls through to false (fail-closed; see the doc comment).
		if _, isFunc := c.info.Objects[ex].(*types.Func); isFunc {
			return true
		}
		return false
	}
	return false
}

// isGetterMember reports whether a MemberExpr resolves to a getter (on a struct,
// an enum, or a module). Mirrors codegen's isGetterCallExpr and the getter
// exclusion in closureAggregateBorrowSource, so the three agree by construction.
func (c *Checker) isGetterMember(e *ast.MemberExpr) bool {
	if c.info.ModuleGetters[e] {
		return true
	}
	targetType := c.info.Types[e.Target]
	if named := extractNamedType(targetType); named != nil {
		return named.LookupGetter(e.Field) != nil
	}
	if enum := extractEnumForMatch(targetType); enum != nil {
		return enum.LookupGetter(e.Field) != nil
	}
	return false
}

// isUserIndexRead reports whether an IndexExpr dispatches to a user-defined
// *non-native* `[]` operator, which returns a fresh owned value. Mirrors
// codegen's isUserIndexExpr.
func (c *Checker) isUserIndexRead(e *ast.IndexExpr) bool {
	targetType := c.info.Types[e.Target]
	if ref, ok := targetType.(*types.MutRef); ok {
		targetType = ref.Elem()
	}
	if ref, ok := targetType.(*types.SharedRef); ok {
		targetType = ref.Elem()
	}
	if _, isArr := targetType.(*types.Array); isArr {
		return false
	}
	named := extractNamedType(targetType)
	if named == nil {
		return false
	}
	m := named.LookupMethod("[]")
	return m != nil && !m.IsNative()
}

// isClosureType reports whether t is a function value (*types.Signature), peeling
// leading Optional layers — an `Optional[() -> int]` binding owns exactly the same
// heap env as the bare closure.
func isClosureType(t types.Type) bool {
	for {
		opt, ok := t.(*types.Optional)
		if !ok {
			break
		}
		t = opt.Elem()
	}
	_, ok := t.(*types.Signature)
	return ok
}
