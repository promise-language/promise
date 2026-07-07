package ownership

import (
	"sort"
	"strings"

	"github.com/promise-language/promise/compiler/internal/ast"
	"github.com/promise-language/promise/compiler/internal/sema"
	"github.com/promise-language/promise/compiler/internal/types"
)

// checkBlock walks all statements in a block sequentially.
// After each statement, call-scoped borrows are expired, and NLL borrow
// narrowing (T0164) expires variable-scoped borrows whose borrower's last
// use was this statement.
func (c *Checker) checkBlock(block *ast.Block) {
	if block == nil {
		return
	}
	for _, stmt := range block.Stmts {
		c.checkStmt(stmt)
		if c.borrows != nil {
			c.borrows.ExpireCallScoped()
			// T0164: NLL borrow narrowing — expire borrows whose borrower
			// variable's last use was this statement.
			if names, ok := c.refLastUses[stmt]; ok {
				for _, name := range names {
					c.borrows.ExpireBorrower(name)
				}
			}
		}
	}
}

// checkStmt dispatches ownership analysis on a single statement.
func (c *Checker) checkStmt(stmt ast.Stmt) {
	if stmt == nil {
		return
	}
	switch s := stmt.(type) {
	case *ast.Block:
		c.checkBlock(s)

	case *ast.TypedVarDecl:
		c.checkTypedVarDecl(s)

	case *ast.InferredVarDecl:
		c.checkInferredVarDecl(s)

	case *ast.DestructureVarDecl:
		c.checkDestructureVarDecl(s)

	case *ast.UseVarDecl:
		c.checkExpr(s.Value)
		// T0593: `use x := this` would store the raw i8* receiver into a use-binding
		// alloca that expects a value struct — crashes codegen like the typed/inferred
		// var-decl cases rejected by T0576. Guard before tryMove (which allows ThisExpr
		// for the return path).
		if this, ok := s.Value.(*ast.ThisExpr); ok {
			if c.state["this"] == Moved {
				c.errorf(this.Pos(), "use of moved variable 'this'")
			} else if c.borrows != nil && c.borrows.HasAnyBorrow("this") {
				c.errorf(this.Pos(), "cannot move 'this' while it is borrowed")
			} else {
				c.errorf(this.Pos(),
					"cannot consume 'this'; the receiver belongs to the caller — call `.clone()` to produce an independent copy, or refactor into a free function taking `Type move`")
			}
			break
		}
		c.tryMove(s.Value)
		if s.Name != "_" {
			c.state[s.Name] = Owned
			c.pinned[s.Name] = true
			if typ := c.info.Types[s.Value]; typ != nil {
				c.trackDeclOrder(s.Name, typ)
			}
		}

	case *ast.AssignStmt:
		c.checkAssignStmt(s)

	case *ast.ReturnStmt:
		if s.Value != nil {
			c.checkExpr(s.Value)
			c.tryMove(s.Value)
			c.tryMoveCastSubject(s.Value) // T0783
			c.checkReturnRefSafety(s)
		}

	case *ast.RaiseStmt:
		c.checkExpr(s.Value)
		c.tryMoveConsume(s.Value)
		c.tryMoveConsumeCastSubject(s.Value) // T0784
		// T1073: `raise o!` — force-unwrap of a borrowed droppable Optional
		// param. The cast form is handled above; reject the force-unwrap shape.
		if isForceUnwrapForm(s.Value) {
			c.rejectBorrowedOptionalUnwrapConsume(s.Value)
		}

	case *ast.ExprStmt:
		c.checkExpr(s.Expr)

	case *ast.IfStmt:
		c.checkIfStmt(s)

	case *ast.WhileStmt:
		c.checkWhileStmt(s)

	case *ast.WhileUnwrapStmt:
		c.checkWhileUnwrapStmt(s)

	case *ast.ForInStmt:
		c.checkForInStmt(s)

	case *ast.ClassicForStmt:
		c.checkClassicForStmt(s)

	case *ast.InfiniteLoop:
		c.checkInfiniteLoop(s)

	case *ast.YieldStmt:
		c.checkExpr(s.Value)
		c.tryMoveConsume(s.Value)
		c.tryMoveConsumeCastSubject(s.Value) // T0784
		// T1073: `yield o!` is intentionally NOT rejected for borrowed params.
		// Unlike the collection-literal/raise/select-send consume sites, a
		// generator yields a *borrow* of the unwrapped inner — the for-in loop var
		// does not own/drop it and the source optional stays owned and usable after
		// the loop, so there is no double-free. Requiring `move` here would wrongly
		// force the caller to surrender ownership it can still use.

	case *ast.YieldDelegateStmt:
		c.checkExpr(s.Value)
		c.tryMoveConsume(s.Value)
		c.tryMoveConsumeCastSubject(s.Value) // T0784
		// T1073: see YieldStmt — yield* also borrows, not consumes; no reject.

	case *ast.IncDecStmt:
		c.checkExpr(s.Target)

	case *ast.SelectStmt:
		c.checkSelectStmt(s)

	case *ast.BreakStmt, *ast.ContinueStmt:
		// no ownership effect
	}
}

func (c *Checker) checkTypedVarDecl(s *ast.TypedVarDecl) {
	if s.Value != nil {
		c.checkExpr(s.Value)
		// T0380/T0381: when the RHS expression's static type is `T&`/`T~`,
		// the LHS binds a non-owning reference. Mark as Borrowed so
		// downstream consume sites are rejected via the existing T0338
		// path. Skip tryMove (a no-op for the auto-dup MemberExpr today,
		// but explicit short-circuit is clearer). Still promote any
		// pending call-scoped borrows so cross-statement borrow tracking
		// (e.g., `string &r = getRef(s)`) still anchors them to `s.Name`.
		if c.isBorrowedExpr(s.Value) {
			if s.Name != "_" {
				c.state[s.Name] = Borrowed
				c.promoteCallBorrows(s.Name, s.Value)
				if typ := c.info.Types[s.Value]; typ != nil {
					c.trackDeclOrder(s.Name, typ)
				}
			}
			if c.inUnsafe == 0 && isPointerTypeRef(s.Type) {
				c.errorf(s.Pos(), "raw pointer type used outside of unsafe block")
			}
			return
		}
		// T0816: reading a closure (function value) out of an owning aggregate
		// (struct/optional closure field, container element) aliases the
		// aggregate's heap env — codegen treats it as a borrow (T0812: no owning
		// env-free binding for the local). Mark the local Borrowed so escapes —
		// returning it, or re-storing it into a longer-lived aggregate — are
		// rejected by the existing Borrowed-state checks (returnsBorrowAsOwned /
		// tryMoveConsume), while the same-scope read-and-invoke case stays valid.
		// Returns before tryMove so the field-move check (checkFieldMoveOwnership)
		// does not fire on the valid read.
		if src := closureAggregateBorrowSource(c.info, s.Value); src != nil {
			if s.Name != "_" {
				c.state[s.Name] = Borrowed
				c.registerClosureAggregateBorrow(s.Name, src, s.Pos())
				if typ := c.info.Types[s.Value]; typ != nil {
					c.trackDeclOrder(s.Name, typ)
				}
			}
			return
		}
		// T0576: binding `this` to a fresh local crashes codegen — the
		// receiver value at runtime is the raw `i8*` instance pointer, but
		// the destination alloca expects the type's value-struct shape
		// (`{i8*,i8*}` for heap user types, `{i8*, field…}` for value
		// types). Even if codegen wrapped correctly, the new local would
		// carry its own drop binding aliasing the caller's heap allocation
		// → double-free at scope exit. Reject at sema with the same
		// diagnostic shape as `tryMoveConsume(ThisExpr)` (T0569), since
		// var decls bypass that path.
		if this, isThis := s.Value.(*ast.ThisExpr); isThis {
			if c.state["this"] == Moved {
				c.errorf(this.Pos(), "use of moved variable 'this'")
			} else if c.borrows != nil && c.borrows.HasAnyBorrow("this") {
				c.errorf(this.Pos(), "cannot move 'this' while it is borrowed")
			} else {
				c.errorf(this.Pos(),
					"cannot consume 'this'; the receiver belongs to the caller — call `.clone()` to produce an independent copy, or refactor into a free function taking `Type move`")
			}
			return
		}
		// T0568: reject moving a Borrowed ident into an owned var-decl when
		// the type is not codegen-safe for the alias pattern. Codegen does a
		// shallow copy for plain heap user types and both the caller (which
		// still owns the origin) and the new local will drop the same heap
		// allocation → runtime double-free. The codegen-safe set is captured
		// by `isVarDeclAliasSafeType` and mirrors codegen's
		// `isDroppableContainerOrString` (string / Vector / Channel / Arc /
		// Weak plus Optional thereof): for those codegen propagates the RHS's
		// drop flag (cleared, for a borrowed param) or auto-dups via
		// `setDupFlagsForFieldAccess`, so the LHS does not double-drop.
		// T1102: single-owner handles (Mutex / MutexGuard / Task) are NOT in
		// the safe set — they have no clone/dup, so an escaping alias would
		// double-free; `rejectBorrowedIdentVarDecl` rejects them outright.
		if c.rejectBorrowedIdentVarDecl(s.Value, s.Type) {
			return
		}
		// T0811: `Plain p = o!` / `Der? d = o as Der` etc. — see
		// checkInferredVarDecl. Reject the wrapper-consume of a borrowed
		// droppable Optional parameter before tryMove.
		if c.rejectBorrowedOptionalUnwrapConsume(s.Value) {
			return
		}
		// T1152: `Task[T] t = go f(s)` of an owned droppable local binds a
		// handle that borrows `s`; track it Borrowed-against-`s` so an escape is
		// rejected at the escape site while the in-scope await/drop stays valid.
		// See checkInferredVarDecl / trackGoHandleBinding.
		if c.trackGoHandleBinding(s.Name, s.Value, s.Pos()) {
			return
		}
		c.tryMove(s.Value)
	}
	if s.Name != "_" {
		c.state[s.Name] = Owned
		c.promoteCallBorrows(s.Name, s.Value)
		if typ := c.info.Types[s.Value]; typ != nil {
			c.trackDeclOrder(s.Name, typ)
		}
		c.recordGuardMutexRoot(s.Name, s.Value) // T0665
		c.flagLoopBodyOwnedLocal(s.Name, s.Value)
	}
	// Raw pointer types are only allowed inside unsafe blocks.
	if c.inUnsafe == 0 && isPointerTypeRef(s.Type) {
		c.errorf(s.Pos(), "raw pointer type used outside of unsafe block")
	}
}

// rejectBorrowedIdentVarDecl errors and returns true when `value` is an
// IdentExpr whose state is Borrowed and whose type is a non-Copy droppable
// type that codegen does NOT safely alias at the var-decl site — the shape
// that surfaces a double-free if allowed through to `tryMove`'s silent
// Borrowed-return path. Callers proceed with the usual `tryMove` only when
// this returns false.
//
// `lhsRef` is the declared LHS TypeRef when known (typed var-decls); pass nil
// for inferred var-decls. When the LHS adds extra Optional wrap layers over
// the RHS type (e.g., `_Box?? b = a` with `a: _Box?`), sema inserts an
// implicit `Some` wrap and codegen produces a wrapped value rather than an
// alias — those cases are out of scope for T0568 (the runtime safety relies
// on B0345 caller-side alias clearing after the function returns, which fires
// when the wrapped value is itself returned). Only reject pure-alias shapes
// where the LHS wrap depth ≤ RHS wrap depth. T0568.
func (c *Checker) rejectBorrowedIdentVarDecl(value ast.Expr, lhsRef ast.TypeRef) bool {
	ident, ok := value.(*ast.IdentExpr)
	if !ok {
		return false
	}
	state, tracked := c.state[ident.Name]
	if !tracked || state != Borrowed {
		return false
	}
	typ := c.info.Types[value]
	// T1138/T0568: a Some-wrap coercion — the LHS adds Optional layers over the
	// RHS type — materializes a fresh wrapped value rather than aliasing the
	// source, so it is not a move. Must run BEFORE the single-owner check so the
	// legitimate `Mutex[int]?? x = optMutex` (LHS depth > RHS depth, RHS already
	// Optional) is not misclassified as an alias. (The direct-return path has no
	// such subtlety.)
	//
	// EXCEPTION — a BARE single-owner handle (`singleOwnerHandleKind(typ) != ""`,
	// i.e. RHS depth 0) wrap-coerced into an Optional local is NOT exempt: the
	// handle has no clone, and the caller-side return-alias check only clears the
	// source drop flag when the result is bound directly to a local — when the
	// wrapped result instead flows into a call argument (`v.push(launder(mm))`) it
	// aliases the one handle and double-frees (segfault). The direct `v.push(mm)`
	// form is already rejected as consuming a borrow; the wrap-coerce launder
	// `Mutex[int]? x = m; return x;` must be rejected too (old T1102 behavior —
	// the reorder must not open this hole). So exclude bare handles from the
	// carve-out and let them fall through to the singleOwnerHandleKindDeep reject.
	//
	// NOTE (T1212, T1138 follow-up): the DEEPER wrap-coerce-then-return shape
	// `Mutex[int]?? x = m;` (RHS already Optional, so carved out here) still
	// escapes an aliased single-owner handle when returned — the carve-out
	// legitimately makes `x` owned, so neither the var-decl nor the return check
	// fires. Catching it needs handle-provenance tracking on the wrapped owned
	// local (outer Optional owned, inner handle borrowed).
	if optionalDepthTypeRef(lhsRef) > optionalDepthType(typ) && singleOwnerHandleKind(typ) == "" {
		return false
	}
	// T1102/T1138: a single-owner native handle (task/Mutex/MutexGuard), possibly
	// wrapped in Optional layers (Mutex[int]?, task[int]??, …), has no clone/dup,
	// so aliasing it into an owned binding is unsound the moment that binding
	// escapes (returned, or laundered then returned). isVarDeclAliasSafeType
	// reports these as safe (true only for the non-escaping drop case), so check
	// for them BEFORE that early-return and reject the alias outright.
	if k := singleOwnerHandleKindDeep(typ); k != "" {
		if c.params[ident.Name] {
			c.errorf(ident.Pos(),
				"cannot move borrowed parameter '%s'; declare the parameter with `move` to consume it",
				ident.Name)
		} else {
			c.errorf(ident.Pos(), "cannot move borrowed value '%s'", ident.Name)
		}
		return true
	}
	if isCopyType(typ) || isVarDeclAliasSafeType(typ) || !isDroppableType(typ) {
		return false
	}
	if c.params[ident.Name] {
		c.errorf(ident.Pos(),
			"cannot move borrowed parameter '%s'; declare the parameter with `move` to consume it",
			ident.Name)
	} else {
		c.errorf(ident.Pos(), "cannot move borrowed value '%s'", ident.Name)
	}
	return true
}

// optionalDepthTypeRef returns the count of leading OptionalTypeRef wrappers
// on a TypeRef. Used to detect implicit `Some` wrapping at a typed var-decl
// site (T0568). Returns 0 for non-Optional refs and nil refs.
func optionalDepthTypeRef(ref ast.TypeRef) int {
	n := 0
	for {
		opt, ok := ref.(*ast.OptionalTypeRef)
		if !ok {
			return n
		}
		n++
		ref = opt.Inner
	}
}

// optionalDepthType returns the count of nested *types.Optional layers on a
// resolved type. Used to detect implicit `Some` wrapping at a typed var-decl
// site (T0568). Returns 0 for non-Optional types and nil.
func optionalDepthType(t types.Type) int {
	n := 0
	for {
		opt, ok := t.(*types.Optional)
		if !ok {
			return n
		}
		n++
		t = opt.Elem()
	}
}

// peelOptional strips leading *types.Optional layers, returning the innermost
// non-Optional type (or the input unchanged when it is not Optional, and nil
// for nil). T0665.
func peelOptional(t types.Type) types.Type {
	for {
		opt, ok := t.(*types.Optional)
		if !ok {
			return t
		}
		t = opt.Elem()
	}
}

// recordGuardMutexRoot links a newly-bound owned MutexGuard local to the local
// that owns the Mutex it borrows: the root of the `.lock()` receiver chain
// (`m` in `g := m.lock()`, `h` in `g := h.m.lock()`, `arr` in
// `g := arr[i].lock()`), or (for a guard-to-guard alias `g2 := g`) the root
// inherited from the source guard. Called from the owned-binding path of the
// var-decl checks. The recorded provenance lets a later container-store site
// reject storing the guard into a container declared before that owner (T0665)
// — see guardMutexExprRoot / the push-site check in checkCallExpr.
func (c *Checker) recordGuardMutexRoot(name string, rhs ast.Expr) {
	if name == "_" || c.guardMutexRoot == nil || rhs == nil {
		return
	}
	if !types.IsMutexGuard(peelOptional(c.info.Types[rhs])) {
		return
	}
	switch e := unwrapDestructureParens(rhs).(type) {
	case *ast.CallExpr: // g := m.lock() / g := h.m.lock() / g := arr[i].lock()
		if mem, ok := e.Callee.(*ast.MemberExpr); ok && types.IsMutex(c.info.Types[mem.Target]) {
			if root := destructureBorrowRoot(mem.Target); root != "" {
				if _, tracked := c.declOrder[root]; tracked {
					c.guardMutexRoot[name] = root
				}
			}
		}
	case *ast.IdentExpr: // g2 := g — inherit the source guard's Mutex root
		if root, ok := c.guardMutexRoot[e.Name]; ok {
			c.guardMutexRoot[name] = root
		}
	}
}

// isBorrowedOptionalType reports whether typ is a reference to an Optional
// (`T?&` / `T?~`) — a borrowed optional, e.g. the result of `Ref[T?].borrow` or
// a `Mutex[T?]` guard's `.borrow`. T0850: an if-unwrap of such a scrutinee binds
// a non-owning view, so its binding is marked Borrowed (not Owned).
func isBorrowedOptionalType(typ types.Type) bool {
	switch ref := typ.(type) {
	case *types.SharedRef:
		_, ok := ref.Elem().(*types.Optional)
		return ok
	case *types.MutRef:
		_, ok := ref.Elem().(*types.Optional)
		return ok
	}
	return false
}

// isVarDeclAliasSafeType reports whether a typed/inferred var-decl from a
// Borrowed RHS of this type is runtime-safe due to codegen's drop-flag
// propagation or auto-dup. Mirrors codegen's `isDroppableContainerOrString`
// (compiler/internal/codegen/stmt.go) plus Optional wrapping any of those.
// Used by T0568 to carve out the safe shapes that the borrowed-ident reject
// must not block. Keep in sync with codegen when new container/handle types
// are added.
//
// NOTE (T1102): Mutex/MutexGuard/Task are listed here because their drop flag
// is correctly propagated/cleared for the non-escaping drop case (and other
// callers, e.g. `this`-arg safety and if-let paths, rely on that). They are
// NOT safe to alias when the binding escapes (returned, or laundered then
// returned) — single-owner handles have no clone/dup. `rejectBorrowedIdentVarDecl`
// checks `singleOwnerHandleKind` BEFORE this function and rejects those.
func isVarDeclAliasSafeType(typ types.Type) bool {
	if typ == nil {
		return false
	}
	if n := extractNamedType(typ); n != nil && n == types.TypString {
		return true
	}
	if types.IsVector(typ) {
		return true
	}
	if types.IsChannel(typ) {
		return true
	}
	if types.IsArc(typ) {
		return true
	}
	if types.IsWeak(typ) {
		return true
	}
	if types.IsMutex(typ) {
		return true
	}
	if types.IsMutexGuard(typ) {
		return true
	}
	if types.IsTask(typ) {
		return true
	}
	if opt, ok := typ.(*types.Optional); ok {
		return isVarDeclAliasSafeType(opt.Elem())
	}
	return false
}

// singleOwnerHandleKind returns the display name ("task"/"Mutex"/"MutexGuard")
// when t is a single-owner native handle that has no clone/dup semantics
// (codegen's maybeDupPushElement returns nil for these), else "". T1102: such a
// handle borrowed by value cannot be aliased into an owned binding or returned
// as owned — the caller still owns the one handle, so a second drop double-frees.
// Although these appear in isVarDeclAliasSafeType (their drop flag is correctly
// propagated/cleared for the non-escaping drop case), that safety does NOT hold
// when the alias escapes (returned or laundered then returned): the caller's
// result then aliases its still-live source local and both ends drop the one
// handle. The call-arg-unsafe rationale mirrors expr.go's
// isCallArgUnsafeBorrowedType, which already flags these for move-arg passing.
func singleOwnerHandleKind(t types.Type) string {
	if _, ok := types.AsTask(t); ok {
		return "task"
	}
	if _, ok := types.AsMutex(t); ok {
		return "Mutex"
	}
	if _, ok := types.AsMutexGuard(t); ok {
		return "MutexGuard"
	}
	return ""
}

// singleOwnerHandleKindDeep is singleOwnerHandleKind after peeling any leading
// Optional layers: an Optional-wrapped single-owner handle (Mutex[int]?,
// task[int]??, …) is exactly as unsafe to alias or return as the bare handle —
// dupOptionalVectorElem (codegen) has no clone for these and shares the one
// handle, so both the source local and the caller's result drop it. T1138.
func singleOwnerHandleKindDeep(t types.Type) string {
	return singleOwnerHandleKind(peelOptional(t))
}

// isPointerTypeRef checks whether a type reference is a raw pointer type.
func isPointerTypeRef(tr ast.TypeRef) bool {
	_, ok := tr.(*ast.PointerTypeRef)
	return ok
}

func (c *Checker) checkInferredVarDecl(s *ast.InferredVarDecl) {
	c.checkExpr(s.Value)
	// T0380/T0381: see checkTypedVarDecl — when RHS static type is `T&`/`T~`,
	// the variable binds a non-owning reference and must not transition to Owned.
	if c.isBorrowedExpr(s.Value) {
		if s.Name != "_" {
			c.state[s.Name] = Borrowed
			c.promoteCallBorrows(s.Name, s.Value)
			if typ := c.info.Types[s.Value]; typ != nil {
				c.trackDeclOrder(s.Name, typ)
			}
		}
		return
	}
	// T0816: see checkTypedVarDecl — reading a closure out of an owning
	// aggregate (`f := h.cb` / `f := h.cb!` / `f := v[0]`) aliases the heap env,
	// so bind Borrowed before tryMove to reject escapes/re-stores.
	if src := closureAggregateBorrowSource(c.info, s.Value); src != nil {
		if s.Name != "_" {
			c.state[s.Name] = Borrowed
			c.registerClosureAggregateBorrow(s.Name, src, s.Pos())
			if typ := c.info.Types[s.Value]; typ != nil {
				c.trackDeclOrder(s.Name, typ)
			}
		}
		return
	}
	// T0576: see checkTypedVarDecl — same crash class on `x := this`.
	if this, isThis := s.Value.(*ast.ThisExpr); isThis {
		if c.state["this"] == Moved {
			c.errorf(this.Pos(), "use of moved variable 'this'")
		} else if c.borrows != nil && c.borrows.HasAnyBorrow("this") {
			c.errorf(this.Pos(), "cannot move 'this' while it is borrowed")
		} else {
			c.errorf(this.Pos(),
				"cannot consume 'this'; the receiver belongs to the caller — call `.clone()` to produce an independent copy, or refactor into a free function taking `Type move`")
		}
		return
	}
	// T0568: see checkTypedVarDecl — same double-free shape on `c := b`.
	// Inferred decls cannot have implicit Optional wrap (LHS type is the RHS
	// type), so no LHS TypeRef is needed for the wrap-depth carve-out.
	if c.rejectBorrowedIdentVarDecl(s.Value, nil) {
		return
	}
	// T0811: `p := o!` / `d := o as! T` / `d := o as T` binding the unwrapped
	// inner of a borrowed droppable Optional parameter double-frees (callee
	// binding-drop + caller drop). Reject before tryMove, like the if-let form.
	if c.rejectBorrowedOptionalUnwrapConsume(s.Value) {
		return
	}
	// T1152: `t := go f(s)` of an owned droppable local binds a handle that
	// borrows `s`; track it (the in-scope await/drop stays valid; an escape is
	// rejected at the escape site). See trackGoHandleBinding.
	if c.trackGoHandleBinding(s.Name, s.Value, s.Pos()) {
		return
	}
	c.tryMove(s.Value)
	if s.Name != "_" {
		c.state[s.Name] = Owned
		c.promoteCallBorrows(s.Name, s.Value)
		if typ := c.info.Types[s.Value]; typ != nil {
			c.trackDeclOrder(s.Name, typ)
		}
		c.recordGuardMutexRoot(s.Name, s.Value) // T0665
		c.flagLoopBodyOwnedLocal(s.Name, s.Value)
	}
}

func (c *Checker) checkDestructureVarDecl(s *ast.DestructureVarDecl) {
	c.checkExpr(s.Value)
	// T0505: For MemberExpr/IndexExpr sources, codegen's genDestructureVarDecl
	// treats the destructured locals as borrows (srcOwned=false, no drop
	// bindings) — the parent owner retains ownership of the data. Routing
	// these through the B0341 field-move check (via tryMove → MemberExpr →
	// checkFieldMoveOwnership) would falsely reject safe borrow patterns like
	// `(a, b) := holder.tup` that the existing T0389/T0420 tests rely on.
	// Skip the move check for these source kinds; var-decl paths (typed and
	// inferred) still go through tryMove and catch the real unsafe case.
	//
	// T0548: The destructured locals are borrows at runtime, so mark non-Copy
	// names as Borrowed and register a shared borrow on the source's root
	// variable. Subsequent moves/consumes of the parent are then rejected by
	// the existing HasAnyBorrow check in tryMove/tryMoveConsume while the
	// borrowers are alive. T0164 NLL narrowing expires the borrow at each
	// borrower's last use (extended for this shape in lastuse.go).
	//
	// T0570: peel ParenExpr so `(b, n) := (h.pair);` takes the same borrow
	// path as the bare `(b, n) := h.pair;` form. Without peeling, the
	// paren-wrapped source falls through to `tryMove(ParenExpr)` (a no-op
	// for non-IdentExpr/ThisExpr/MemberExpr) leaving the destructured names
	// Owned and the parent un-borrowed → consume of the parent slips through
	// to a runtime UAF / double-free.
	//
	// T0978: `for tup in v { (a, b) := tup }` is a borrow destructure — codegen
	// (T0371) gives the pieces no drop bindings because the source ident (a for-in
	// alias binding) has none. Routing it through tryMove(tup) would hit the
	// broadened for-in alias guard and falsely reject the destructure itself, even
	// when the pieces are only read. Carve it out: skip tryMove, and mark each
	// piece per the same aliasing predicate the for-in guard uses on whole
	// bindings (forInElementAliasesContainer). A piece that aliases droppable
	// container storage (a non-Copy heap user type, nested container, …) is marked
	// Borrowed so a later move/consume of it (`sink.push(b)`, `y := b`) is rejected
	// by tryMoveConsume / rejectBorrowedIdentVarDecl. Copy pieces (value copies)
	// and string pieces (dup'd on store, verified leak-free at runtime) stay Owned
	// and freely movable — matching the whole-binding guard's Copy/string
	// exclusions, so `(s, n) := tup; concat = concat + s` over a `(string, int)[]`
	// keeps working. The single-owner set is checked defensively; in practice it
	// never matches here because single-owner handles are not tuples, so a
	// destructure of one fails to type-check earlier.
	destructureSrc := unwrapDestructureParens(s.Value)
	if id, ok := destructureSrc.(*ast.IdentExpr); ok &&
		(c.forInAliasBindings[id.Name] || c.forInSingleOwnerBindings[id.Name]) {
		var elems []types.Type
		if tup, ok := c.info.Types[s.Value].(*types.Tuple); ok {
			elems = tup.Elems()
		}
		for i, name := range s.Names {
			if name == "_" {
				continue
			}
			var elem types.Type
			if i < len(elems) {
				elem = elems[i]
			}
			if forInElementAliasesContainer(elem) {
				c.state[name] = Borrowed
			} else {
				c.state[name] = Owned
			}
		}
		return
	}
	switch unwrapDestructureParens(s.Value).(type) {
	case *ast.MemberExpr, *ast.IndexExpr:
		rootName := destructureBorrowRoot(s.Value)
		var elems []types.Type
		if tup, ok := c.info.Types[s.Value].(*types.Tuple); ok {
			elems = tup.Elems()
		}
		// T0571: When the destructure source's root is not a stable variable
		// (CallExpr / conditional / error-handler / cast / etc.), the source is
		// a transient temporary that codegen drops at end of the destructure
		// statement (via stmtTemps cleanup). Non-Copy destructured locals are
		// pure borrows into that temp's heap data, so they dangle the moment
		// the statement ends. Reject at compile time and direct the user to
		// bind the source to a local first.
		if rootName == "" {
			for i, name := range s.Names {
				if name == "_" {
					continue
				}
				if i < len(elems) && !isCopyType(elems[i]) {
					c.errorf(s.Pos(),
						"cannot destructure from temporary expression; non-Copy destructured locals would dangle after the source is dropped — bind the source to a local first")
					break
				}
			}
		}
		for i, name := range s.Names {
			if name == "_" {
				continue
			}
			var elem types.Type
			if i < len(elems) {
				elem = elems[i]
			}
			if elem != nil && isCopyType(elem) {
				c.state[name] = Owned
				continue
			}
			c.state[name] = Borrowed
			if rootName != "" && c.borrows != nil {
				c.borrows.Add(&Borrow{
					Origin:   rootName,
					Kind:     BorrowShared,
					Borrower: name,
					Pos:      s.Pos(),
				})
			}
		}
		return
	default:
		c.tryMove(s.Value)
	}
	for _, name := range s.Names {
		if name != "_" {
			c.state[name] = Owned
		}
	}
}

// unwrapDestructureParens peels any number of *ast.ParenExpr wrappers from a
// destructure source expression. T0570: the dispatch switch in
// checkDestructureVarDecl matches on AST shape; without peeling, a paren-
// wrapped MemberExpr/IndexExpr would fall to the `default` arm and skip the
// borrow registration, allowing a runtime UAF.
func unwrapDestructureParens(e ast.Expr) ast.Expr {
	for {
		p, ok := e.(*ast.ParenExpr)
		if !ok {
			return e
		}
		e = p.Expr
	}
}

// destructureBorrowRoot walks a destructure source down to the root variable
// name, threading through MemberExpr / IndexExpr / ParenExpr. Returns the
// root IdentExpr name, or "this" for a ThisExpr root (ownership tracks the
// receiver under the name "this"). Returns "" if no root is reachable (e.g.
// nested call expressions).
func destructureBorrowRoot(expr ast.Expr) string {
	for {
		switch e := expr.(type) {
		case *ast.IdentExpr:
			return e.Name
		case *ast.ThisExpr:
			return "this"
		case *ast.MemberExpr:
			expr = e.Target
		case *ast.IndexExpr:
			expr = e.Target
		case *ast.ParenExpr:
			expr = e.Expr
		default:
			return ""
		}
	}
}

// registerClosureAggregateBorrow records a shared borrow of the aggregate that a
// closure-aggregate read borrows from (T0816). `borrower` is the local bound by
// `f := h.cb` / `f := h.cb!` / `f := v[0]`; `src` is the peeled aggregate access
// returned by closureAggregateBorrowSource (`h.cb` / `v[0]`). The borrow's origin
// is the access's root variable, so the existing HasAnyBorrow checks reject
// moving/consuming/reassigning the source while the borrowing local is still live
// (which would free the heap env out from under the local — UAF / double-free).
// NLL narrowing in AnalyzeRefLastUses expires the borrow at the borrower's last
// use, so consume-after-last-use of the source stays valid. Mirrors the
// destructure-from-aggregate borrow registration in checkDestructureVarDecl.
func (c *Checker) registerClosureAggregateBorrow(borrower string, src ast.Expr, pos ast.Pos) {
	if c.borrows == nil {
		return
	}
	root := destructureBorrowRoot(src)
	if root == "" {
		return
	}
	c.borrows.Add(&Borrow{
		Origin:   root,
		Kind:     BorrowShared,
		Borrower: borrower,
		Pos:      pos,
	})
}

// promoteCallBorrows promotes pending call-scoped borrows to variable-scoped
// when a function returning a reference type stores its result in a variable.
func (c *Checker) promoteCallBorrows(varName string, value ast.Expr) {
	if value == nil || c.borrows == nil {
		return
	}
	typ := c.info.Types[value]
	if !isRefType(typ) {
		return
	}
	for _, b := range c.borrows.borrows {
		if b.Borrower == "" {
			b.Borrower = varName
		}
	}
}

// isRefType returns true if the type is a reference type (SharedRef or MutRef).
func isRefType(t types.Type) bool {
	if t == nil {
		return false
	}
	switch t.(type) {
	case *types.SharedRef, *types.MutRef:
		return true
	}
	return false
}

func (c *Checker) checkAssignStmt(s *ast.AssignStmt) {
	if s.Op != ast.OpAssign {
		// Compound assignment (+=, -=, etc.) reads the target.
		c.checkExpr(s.Target)
	} else {
		// Simple assignment: check sub-expressions of the target (member/index
		// receivers) but not the target variable itself since we're writing to it.
		c.checkAssignTarget(s.Target)
	}

	c.checkExpr(s.Value)
	// T0380/T0381: `b = a.borrow` (or any RHS whose static type is `T&`/`T~`)
	// is runtime-safe via the T0379 codegen fix (clearDropFlag at the store
	// site). Skip tryMoveConsume's inline-borrow rejection here — and below,
	// force the LHS state to Borrowed so downstream consume sites still
	// reject.
	//
	// T0401: this skip is only sound when LHS is a local IdentExpr — that's
	// the shape T0379's codegen-level dropflag-clear protects. For
	// MemberExpr/IndexExpr (e.g., `guard.borrow = guard.borrow` via the
	// MutexGuard.borrow setter), there is no per-slot dropflag: codegen does
	// drop-then-store on the same slot, and self-aliasing produces a UAF.
	// Falling through to tryMoveConsume rejects with the existing
	// "cannot move out of '.borrow' getter" diagnostic (T0380).
	_, lhsIsIdent := s.Target.(*ast.IdentExpr)
	rhsIsBorrowGetter := s.Op == ast.OpAssign && lhsIsIdent && c.isBorrowedExpr(s.Value)
	// T0895: `f = h.cb` reassigns a pre-declared local from a closure read out
	// of an owning aggregate (struct/optional closure field, container element).
	// Mirrors T0816's var-decl handling (checkTypedVarDecl/checkInferredVarDecl):
	// the read aliases the aggregate's heap env (codegen suppresses the local's
	// owning env-free binding), so treat it as a borrow rather than a move —
	// skip tryMoveConsume (else checkFieldMoveOwnership falsely fires on the
	// valid read) and resurrect the LHS Borrowed below so escapes/re-stores are
	// rejected. rhsClosureBorrowSrc != nil implies an IdentExpr target, so the
	// MemberExpr arm below is unaffected.
	rhsClosureBorrowSrc := ast.Expr(nil)
	if s.Op == ast.OpAssign && lhsIsIdent {
		rhsClosureBorrowSrc = closureAggregateBorrowSource(c.info, s.Value)
	}
	if !rhsIsBorrowGetter && rhsClosureBorrowSrc == nil {
		// T0351: assignment consumes the RHS — borrowed params cause a double-free
		// because the caller still drops the original. tryMoveConsume rejects them
		// at compile time (matches T0338/T0349 pattern for raise/yield/select-send).
		c.tryMoveConsume(s.Value)
		// T1205: `container[k] = m.lock()` stores a MutexGuard into an indexable
		// container. When the container is a `~`/`&` parameter that outlives a local
		// (or `~`(move)-param) Mutex, the guard's drop unlocks an already-destroyed
		// Mutex (UAF). The `mp._set(k, g)` method-call form is caught in
		// checkCallExpr; this covers the index-assignment surface syntax that lowers
		// to the same store. No `continue` — falling through to the resurrection
		// logic is harmless after the error is recorded.
		if idx, ok := s.Target.(*ast.IndexExpr); ok {
			if mroot, escapes := c.guardStoreEscapesLocalMutex(idx.Target, s.Value); escapes {
				if container, ok := idx.Target.(*ast.IdentExpr); ok {
					c.errorf(s.Value.Pos(), "%s", guardEscapeMsg(container.Name, mroot, c.paramIsMove(mroot)))
				}
			}
		}
		// T0811: `p = o!` / `p = o as! T` — reassigning a slot from the unwrapped
		// inner of a borrowed droppable Optional parameter double-frees. The
		// carve-out (isVarDeclAliasSafeType) keeps string/vector field stores
		// allowed, matching verified runtime safety.
		c.rejectBorrowedOptionalUnwrapConsume(s.Value)
	} else if _, ok := s.Target.(*ast.MemberExpr); ok {
		// T0382: `obj.field = a.borrow` for a non-ref-typed field stores an
		// alias to the source's inner buffer. Fields have no per-slot
		// dropflag, so the parent's drop walks them unconditionally and
		// double-frees the buffer the source still owns. T0367/T0379 only
		// cover IdentExpr targets via per-local dropflag clear. Mirrors
		// T0380's pattern. (IndexExpr siblings are handled by codegen-dup —
		// see T0383.)
		if !isRefType(c.info.Types[s.Target]) {
			c.errorf(s.Pos(), "cannot assign borrow to owned field; use '.clone()' to copy")
		}
	}

	// T0754: an RTTI cast (`x as T` / `x as! T`) into an owning slot — a struct
	// field or container element — must consume its subject. tryMoveConsume
	// above is a no-op on CastExpr (the CastExpr case in checkExpr only
	// recurses), so without this peel the cast wrapper silently aliases the
	// subject into the field/element and both scopes double-free. IdentExpr
	// targets keep T0747's view semantics (codegen clears the local's drop
	// flag); only owning slots without per-binding drop flags need the peel.
	if s.Op == ast.OpAssign {
		switch s.Target.(type) {
		case *ast.MemberExpr, *ast.IndexExpr:
			c.tryMoveConsumeCastSubject(s.Value)
		}
	}

	// Simple assignment resurrects the target variable.
	if s.Op == ast.OpAssign {
		if ident, ok := s.Target.(*ast.IdentExpr); ok {
			// Cannot reassign a variable that is actively borrowed by a named
			// borrower (ref local). A transient call-scoped borrow created by the
			// reassignment's own RHS — e.g. `v = sort(v)`, where the plain-`T[]`
			// param borrows v (T0964) — is not a conflict; it expires at statement
			// end or is promoted below for a ref-returning RHS.
			if c.borrows != nil && c.borrows.HasPersistentBorrow(ident.Name) {
				c.errorf(s.Pos(), "cannot assign to '%s' while it is borrowed", ident.Name)
			}
			// Expire borrows where this variable is the borrower
			if c.borrows != nil {
				c.borrows.ExpireBorrower(ident.Name)
			}
			if _, tracked := c.state[ident.Name]; tracked {
				if rhsIsBorrowGetter || rhsClosureBorrowSrc != nil {
					c.state[ident.Name] = Borrowed
				} else {
					c.state[ident.Name] = Owned
				}
			}
			// T0895: register the shared borrow of the source aggregate *after*
			// ExpireBorrower above so it is not immediately expired. Protects the
			// source from being moved/consumed/reassigned while the borrowing
			// local is live (UAF / double-free of the heap env otherwise).
			if rhsClosureBorrowSrc != nil {
				c.registerClosureAggregateBorrow(ident.Name, rhsClosureBorrowSrc, s.Pos())
			}
			// Promote call-scoped borrows if the RHS returns a ref type
			c.promoteCallBorrows(ident.Name, s.Value)
		}
	}
}

// checkAssignTarget checks sub-expressions of an assignment target for
// use-after-move, without checking the final target variable itself.
func (c *Checker) checkAssignTarget(target ast.Expr) {
	switch e := target.(type) {
	case *ast.IdentExpr:
		// Don't check — we're assigning TO this variable.
	case *ast.MemberExpr:
		c.checkExpr(e.Target)
	case *ast.IndexExpr:
		c.checkExpr(e.Target)
		c.checkExpr(e.Index)
	case *ast.SliceExpr:
		c.checkExpr(e.Target)
		if e.Low != nil {
			c.checkExpr(e.Low)
		}
		if e.High != nil {
			c.checkExpr(e.High)
		}
	}
}

// checkReturnRefSafety validates that returned references don't point to locals
// and enforces lifetime constraints (B0033).
func (c *Checker) checkReturnRefSafety(s *ast.ReturnStmt) {
	if c.curSig == nil || c.curSig.Result() == nil {
		return
	}
	if !isRefType(c.curSig.Result()) {
		// T1102/T1138: returning a borrowed (non-`move`) single-owner native
		// handle parameter (task/Mutex/MutexGuard), possibly wrapped in Optional
		// layers (Mutex[int]?, task[int]??, …), as owned is unsound — these have
		// no clone/dup, so the caller's result aliases its still-live source local
		// and both ends drop the one handle (double-free / UAF). returnsBorrowAsOwned
		// deliberately exempts parameters (it assumes "return implicitly dups for
		// non-Copy types"), which is false for these handles — reject here first.
		// The direct-return path has no result-wrap coercion subtlety: returning a
		// borrowed handle param as owned is always unsound, so peel and reject.
		if ident, ok := s.Value.(*ast.IdentExpr); ok &&
			c.params[ident.Name] && c.state[ident.Name] == Borrowed {
			if k := singleOwnerHandleKindDeep(c.info.Types[ident]); k != "" {
				c.errorf(s.Pos(),
					"cannot return borrowed parameter '%s' as owned; a `%s` handle has no clone and the caller still owns it — declare the parameter with `move` to transfer ownership",
					ident.Name, k)
				return
			}
		}
		// T0402: returning a non-Copy borrow as owned is unsafe — the caller
		// would register a drop for the inner pointer that the original Arc/
		// Mutex still owns, leading to double-free. Reject regardless of
		// whether the borrow's source is a local or a parameter (parameter
		// case is also unsound: the caller's Arc/Mutex retains ownership of
		// the inner T after the call returns).
		if s.Value != nil && c.returnsBorrowAsOwned(s.Value) {
			c.errorf(s.Pos(),
				"cannot return a borrowed reference as owned '%s'; the original Arc/Mutex still owns this value — call .clone() to produce an independent owned copy",
				c.curSig.Result().String())
		}
		return
	}

	origin := resolveReturnOrigin(s.Value)
	if origin == "" {
		return // can't determine origin (complex expression)
	}

	// Existing check: can't return reference to local variable.
	if c.params != nil && !c.params[origin] {
		c.errorf(s.Pos(), "cannot return reference to local variable '%s'", origin)
		return
	}

	// If explicit lifetime declared on return type, validate the origin matches.
	if lt := c.curSig.ResultLifetime(); lt != "" {
		c.checkExplicitLifetimeReturn(s, origin, lt)
		return
	}

	// Elision rule 3: &this/~this receiver — always OK.
	if origin == "this" {
		return
	}

	// Elision rule 2: exactly one ref param — always OK.
	if c.countRefParams() == 1 {
		return
	}

	// Rule 4: multiple ref params, no explicit annotation — record for ambiguity check.
	if c.returnOrigins == nil {
		c.returnOrigins = make(map[string]ast.Pos)
	}
	if _, exists := c.returnOrigins[origin]; !exists {
		c.returnOrigins[origin] = s.Pos()
	}
}

// resolveReturnOrigin walks an expression to find the root variable name.
// Returns "" if the expression is too complex to track.
func resolveReturnOrigin(expr ast.Expr) string {
	switch e := expr.(type) {
	case *ast.IdentExpr:
		return e.Name
	case *ast.MemberExpr:
		return resolveReturnOrigin(e.Target)
	}
	return ""
}

// returnsBorrowAsOwned reports whether `expr` is a non-Copy borrow being
// returned where the function declares an owned non-ref result. T0402.
//
// Two shapes need to be caught:
//
//  1. Static-type borrow — `return arc.borrow` (expr type is `T&`/`T~` non-Copy).
//     `Ref[T].borrow` and `MutexGuard[T].borrow` return `T&` post-T0381.
//
//  2. Laundered borrow — `string borrowed = arc.borrow; return borrowed;`
//     The typed local declaration decays `T&` → `T` so the expr's static type
//     is owned `T`, but ownership tracks the local with state Borrowed. Without
//     this branch the laundered form slips through the type-based check and
//     still double-frees at runtime.
//
// The state check is restricted to non-parameter locals: by-value parameters
// (plain `T`) are also tracked as Borrowed (T0338) but their return path is
// safe — return implicitly dups for non-Copy types so the caller gets an
// independent value.
func (c *Checker) returnsBorrowAsOwned(expr ast.Expr) bool {
	t := c.info.Types[expr]
	if isBorrowedType(t) {
		return true
	}
	if ident, ok := expr.(*ast.IdentExpr); ok {
		if c.state[ident.Name] == Borrowed && !c.params[ident.Name] && !isCopyType(t) {
			return true
		}
	}
	return false
}

// countRefParams counts the number of reference parameters in the current signature
// (excluding the receiver). Move params (Ref()==RefMut, T0087) are excluded since
// they take ownership — the moved value is destroyed at function exit.
func (c *Checker) countRefParams() int {
	if c.curSig == nil {
		return 0
	}
	count := 0
	for _, p := range c.curSig.Params() {
		kind := paramBorrowKind(p)
		if kind == BorrowNone {
			continue
		}
		// T0087: ~ on regular params means move, not borrow.
		if p.Ref() == types.RefMut {
			continue
		}
		count++
	}
	return count
}

// checkExplicitLifetimeReturn validates that the returned value borrows from
// a parameter whose lifetime matches the declared return lifetime.
func (c *Checker) checkExplicitLifetimeReturn(s *ast.ReturnStmt, origin string, resultLifetime string) {
	// Find the parameter and check its lifetime.
	if c.curSig == nil {
		return
	}
	// Check receiver.
	if origin == "this" && c.curSig.Recv() != nil {
		// &this receiver implicitly satisfies any return lifetime.
		return
	}
	for _, p := range c.curSig.Params() {
		if p.Name() == origin {
			paramLt := p.Lifetime()
			if paramLt == "" {
				// Param has no explicit lifetime — doesn't match any declared lifetime.
				c.errorf(s.Pos(), "returned reference borrows from parameter '%s' which has no `lifetime annotation", origin)
				return
			}
			if paramLt != resultLifetime {
				c.errorf(s.Pos(), "returned reference borrows from parameter '%s' (lifetime '%s') but return type declares lifetime '%s'", origin, paramLt, resultLifetime)
			}
			return
		}
	}
}

// checkReturnAmbiguity is called after checking the entire function body.
// If the function returns a reference type, has multiple ref params, no explicit
// lifetime, and the body returns from more than one distinct parameter, it's ambiguous.
func (c *Checker) checkReturnAmbiguity() {
	if len(c.returnOrigins) <= 1 {
		return
	}
	// Collect the parameter names for the error message (sorted for determinism).
	var names []string
	var firstPos ast.Pos
	for name, pos := range c.returnOrigins {
		names = append(names, "'"+name+"'")
		if firstPos.Line == 0 || pos.Line < firstPos.Line {
			firstPos = pos
		}
	}
	sort.Strings(names)
	c.errorf(firstPos, "ambiguous return reference: function returns references from multiple parameters (%s); add `lifetime annotations to disambiguate", strings.Join(names, ", "))
}

// --- Control flow statements ---

// findBorrowedDroppableOptionalIfletSource reports the borrowed parameter ident
// surfaced as an if-let / while-let init expression whose static type is an
// `Optional[T]` with `isDroppableType(T)` true. The if-let / while-let unwrap
// transfers ownership of the inner heap value into the binding, which then
// drops at scope exit — but a non-`~` Optional parameter is borrowed (the
// caller retains ownership and will also drop), so the consume would
// double-free. Mirror T0586's call-arg reject (`findBorrowedNonAliasSafeIdent`)
// at the if-let / while-let init position. Walks through transparent wrappers
// (ParenExpr / IfExpr / MatchExpr) — codegen forwards the branch value directly
// (the if/match's PHI value, the paren-stripped inner), so a Borrowed param
// surfaced through any of these wrappers reaches the unwrap site as the same
// alias the caller still owns. T0589.
//
// Limited to identifiers tracked as parameters (`c.params[name]`) — non-param
// Borrowed locals (destructure-bound borrows) are caught elsewhere and the
// affordance ("declare the parameter with `move`") only applies to params.
// The carve-outs match the bug analysis: `int?`, `bool?`, primitive Optionals,
// value-type Optionals, and structural-interface Optionals are not droppable
// (their inner type isn't), so the predicate skips them.
func (c *Checker) findBorrowedDroppableOptionalIfletSource(expr ast.Expr) *ast.IdentExpr {
	switch e := expr.(type) {
	case *ast.IdentExpr:
		if !c.params[e.Name] {
			return nil
		}
		state, tracked := c.state[e.Name]
		if !tracked || state != Borrowed {
			return nil
		}
		opt, ok := c.info.Types[e].(*types.Optional)
		if !ok {
			return nil
		}
		if !isDroppableType(opt.Elem()) {
			return nil
		}
		return e
	case *ast.ParenExpr:
		return c.findBorrowedDroppableOptionalIfletSource(e.Expr)
	case *ast.IfExpr:
		if id := c.findBorrowedDroppableOptionalIfletInBlock(e.Then); id != nil {
			return id
		}
		return c.findBorrowedDroppableOptionalIfletInBlock(e.Else)
	case *ast.MatchExpr:
		for _, arm := range e.Arms {
			if arm.Body != nil {
				if id := c.findBorrowedDroppableOptionalIfletSource(arm.Body); id != nil {
					return id
				}
			}
			if arm.Block != nil {
				if id := c.findBorrowedDroppableOptionalIfletInBlock(arm.Block); id != nil {
					return id
				}
			}
		}
	}
	return nil
}

// borrowedOptionalUnwrapConsumeSubject reports the borrowed-parameter ident
// surfaced as the subject of a force-unwrap (`o!`) or optional cast
// (`o as! T` / `o as T`) whose extracted inner is a droppable, non-alias-safe
// type (heap/generic user type, Map, Set). Binding such an extraction takes
// ownership of the caller-owned inner → callee binding-drop + caller drop
// double-free (T0811). Mirrors rejectBorrowedIdentVarDecl (T0568) /
// findBorrowedDroppableOptionalIfletSource (T0589); same "declare with `move`" affordance.
//
// Only optional-subject casts force-unwrap (`opt as! T` / `opt as T`); a cast
// whose subject is a non-optional value keeps T0747 view semantics (codegen
// clears the local's drop flag) and must NOT be rejected here. The inner-type
// carve-out matches T0568: string/vector/handle inners are auto-dup-safe at
// the binding site and stay allowed — only genuinely-unsafe heap-user inners
// surface a double-free.
func (c *Checker) borrowedOptionalUnwrapConsumeSubject(expr ast.Expr) *ast.IdentExpr {
	// Peel ParenExpr on the wrapper.
	for {
		p, ok := expr.(*ast.ParenExpr)
		if !ok {
			break
		}
		expr = p.Expr
	}
	var subject ast.Expr
	var innerType types.Type
	switch e := expr.(type) {
	case *ast.OptionalUnwrapExpr:
		subject = e.Expr
		innerType = c.info.Types[e]
	case *ast.CastExpr:
		// Only optional-subject casts perform the force-unwrap that takes
		// ownership of the inner. A non-optional downcast keeps view semantics.
		if _, ok := c.info.Types[e.Expr].(*types.Optional); !ok {
			return nil
		}
		subject = e.Expr
		if e.Force {
			innerType = c.info.Types[e]
		} else if opt, ok := c.info.Types[e].(*types.Optional); ok {
			innerType = opt.Elem()
		} else {
			innerType = c.info.Types[e]
		}
	default:
		return nil
	}
	// Peel ParenExpr on the subject.
	for {
		p, ok := subject.(*ast.ParenExpr)
		if !ok {
			break
		}
		subject = p.Expr
	}
	ident, ok := subject.(*ast.IdentExpr)
	if !ok {
		return nil
	}
	if !c.params[ident.Name] {
		return nil
	}
	if state, tracked := c.state[ident.Name]; !tracked || state != Borrowed {
		return nil
	}
	if _, ok := c.info.Types[ident].(*types.Optional); !ok {
		return nil
	}
	if innerType == nil || isCopyType(innerType) || isVarDeclAliasSafeType(innerType) || !isDroppableType(innerType) {
		return nil
	}
	return ident
}

// isForceUnwrapForm reports whether expr (after peeling ParenExpr) is a bare
// force-unwrap `o!`. Used at call-arg sites where the cast form `o as! T` is
// already rejected by the adjacent tryMoveConsumeCastSubject — guarding to the
// force-unwrap shape avoids a duplicate T0811 diagnostic.
func isForceUnwrapForm(expr ast.Expr) bool {
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

// rejectBorrowedOptionalUnwrapConsume errors and returns true when `expr` is a
// force-unwrap / optional-cast consume of a borrowed droppable-inner Optional
// parameter (the T0811 double-free shape). Callers short-circuit on true,
// mirroring rejectBorrowedIdentVarDecl.
func (c *Checker) rejectBorrowedOptionalUnwrapConsume(expr ast.Expr) bool {
	if ident := c.borrowedOptionalUnwrapConsumeSubject(expr); ident != nil {
		c.errorf(ident.Pos(),
			"cannot consume borrowed parameter '%s' via force-unwrap/cast; declare the parameter with `move` to consume the Optional",
			ident.Name)
		return true
	}
	return false
}

// findBorrowedDroppableOptionalIfletInBlock inspects a block's trailing
// expression (the block's result value) for a surfaced borrowed-Optional
// ident. Mirrors T0586's findBorrowedNonAliasSafeIdentInBlock — codegen's
// match-arm-block lowering returns the trailing ExprStmt as the arm's value.
func (c *Checker) findBorrowedDroppableOptionalIfletInBlock(block *ast.Block) *ast.IdentExpr {
	if block == nil || len(block.Stmts) == 0 {
		return nil
	}
	if es, ok := block.Stmts[len(block.Stmts)-1].(*ast.ExprStmt); ok {
		return c.findBorrowedDroppableOptionalIfletSource(es.Expr)
	}
	return nil
}

func (c *Checker) checkIfStmt(s *ast.IfStmt) {
	if s.Binding != "" {
		// if-unwrap: if val := expr { }
		c.checkExpr(s.Init)
		c.tryMove(s.Init)
		// T0589: reject consume of a borrowed droppable Optional parameter
		// via if-let. The if-let binding takes ownership of the inner heap
		// pointer (drops at scope exit), but the caller still owns and drops
		// the same allocation → double-free. Force `~T?` to make the consume
		// explicit, or wrap into a wider Optional via an intermediate local
		// (T0585's working wrap-then-iflet path).
		if ident := c.findBorrowedDroppableOptionalIfletSource(s.Init); ident != nil {
			c.errorf(ident.Pos(),
				"cannot consume borrowed parameter '%s' via if-let; declare the parameter with `move` to consume the Optional, or wrap into a wider Optional via an intermediate local",
				ident.Name)
		}
	} else {
		c.checkExpr(s.Cond)
	}
	// Expire call-scoped borrows from the condition/init expression so the
	// then/else branches can re-borrow the same variables.
	if c.borrows != nil {
		c.borrows.ExpireCallScoped()
	}

	savedState := c.state.clone()
	savedBorrows := c.borrows.Clone()
	if s.Binding != "" && s.Binding != "_" {
		// T0850: a borrowed optional scrutinee (`T?&` / `T?~`, e.g. `Ref[T?].borrow`)
		// binds a non-owning view of the external owner's payload — mark it Borrowed
		// so moving it out (into an owned var-decl / `~` arg) is rejected (T0568),
		// preventing a double-free with the owner's drop. Owned optionals stay Owned.
		if t := c.info.Types[s.Init]; isBorrowedOptionalType(t) {
			c.state[s.Binding] = Borrowed
		} else {
			c.state[s.Binding] = Owned
		}
	}
	// T1177: a single-owner handle (Task/Mutex/MutexGuard/...) bound via an
	// `if x is V(job)` / `if s is T(job)` destructure is a *borrow* — the escape-dup
	// logic (T1012/T1169) cannot deep-clone a single-owner handle, so it is left
	// aliasing the subject's field, and the subject drops it exactly once at scope
	// exit. Mark such bindings Borrowed for the then-block so any *consume* is
	// rejected: awaiting `<-job` (joins+frees the task), moving it out, or sending
	// it on a channel would free the handle a second time when the subject also
	// drops it → double-free / SIGSEGV. Non-consuming use (bind-and-leave,
	// `guard.lock()`) stays legal. The marks are then-block-local and restored
	// afterward so an outer variable the binding shadows is unaffected.
	restoreHandleStates := c.markDestructureHandleBindingsBorrowed(s)
	c.checkBlock(s.Body)
	restoreHandleStates()
	thenState := c.state
	thenBorrows := c.borrows

	// T1134: a branch body that diverges (ends in return/raise/break/continue)
	// never falls through to the post-if path, so its end-state — including any
	// moves it performed — must be excluded from the merge. Otherwise a move on
	// the diverging path falsely poisons a variable that is still owned on the
	// fall-through path.
	thenDiverges := blockDiverges(s.Body)
	if s.Else != nil {
		c.state = savedState.clone()
		c.borrows = savedBorrows.Clone()
		c.checkStmt(s.Else)
		elseState := c.state
		elseBorrows := c.borrows
		elseDiverges := stmtDiverges(s.Else)
		switch {
		case thenDiverges && elseDiverges:
			// Post-if code is unreachable; fall back to the pre-if baseline.
			c.state = savedState.clone()
			c.borrows = savedBorrows.Clone()
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
	} else if thenDiverges {
		// No else and the then-branch diverges: the fall-through path is only
		// reached when the branch did not run, so keep the pre-if state.
		c.state = savedState
		c.borrows = savedBorrows
	} else {
		// No else: conservative merge with pre-if state.
		c.state = merge(savedState, thenState)
		c.borrows = MergeBorrowSets(savedBorrows, thenBorrows)
	}
}

// markDestructureHandleBindingsBorrowed marks every single-owner-handle binding
// of an `if is`-destructure narrowing as Borrowed for the then-block and returns
// a closure that restores the prior state entries. See the call site in
// checkIfStmt for the rationale (T1177). Returns a no-op restore when the if has
// no destructure narrowing or no handle bindings.
func (c *Checker) markDestructureHandleBindingsBorrowed(s *ast.IfStmt) func() {
	dn := c.info.IsDestructureNarrowings[s]
	if dn == nil {
		return func() {}
	}
	type savedEntry struct {
		name    string
		state   VarState
		present bool
	}
	var saved []savedEntry
	for _, b := range dn.Bindings {
		if sema.FirstNestedSingleOwnerHandle(b.Type) == nil {
			continue
		}
		prev, ok := c.state[b.VarName]
		saved = append(saved, savedEntry{name: b.VarName, state: prev, present: ok})
		c.state[b.VarName] = Borrowed
	}
	if saved == nil {
		return func() {}
	}
	return func() {
		for _, e := range saved {
			if e.present {
				c.state[e.name] = e.state
			} else {
				delete(c.state, e.name)
			}
		}
	}
}

// enterLoopBody raises the loop-nesting depth and snapshots the owned-droppable
// set so loop-body-local declarations added while checking the body (via
// flagLoopBodyOwnedLocal) are removed at loop exit — their iteration-bounded
// scope ends with the loop. Returns the snapshot to pass to exitLoopBody. T1151.
func (c *Checker) enterLoopBody() map[string]bool {
	c.loopDepth++
	snap := make(map[string]bool, len(c.forInOwnedDroppableBindings))
	for k, v := range c.forInOwnedDroppableBindings {
		snap[k] = v
	}
	return snap
}

// exitLoopBody restores the owned-droppable set to its pre-body snapshot and
// lowers the loop-nesting depth. T1151.
func (c *Checker) exitLoopBody(snap map[string]bool) {
	c.forInOwnedDroppableBindings = snap
	c.loopDepth--
}

// flagLoopBodyOwnedLocal records an owned, non-copy, droppable local declared
// inside a loop body (loopDepth ≥ 1) as iteration-bounded, so borrowing it into
// a `go f(arg)` call is rejected by rejectGoCallLoopBindingBorrowEscape — the
// goroutine can outlive the iteration, leaving a dangling borrow (T1151). Only
// the genuinely-Owned var-decl path reaches here: aliasing/borrow decls return
// early (marked Borrowed) before this point, and Copy locals are excluded.
func (c *Checker) flagLoopBodyOwnedLocal(name string, value ast.Expr) {
	if c.loopDepth == 0 || name == "_" {
		return
	}
	typ := c.info.Types[value]
	if typ != nil && !isCopyType(typ) && isDroppableType(typ) {
		c.forInOwnedDroppableBindings[name] = true
	}
}

// trackGoHandleBinding handles `name := go f(local)` / `Type name = go f(local)`
// where the `go` call borrows an owned, droppable, non-Copy function/block-scope
// local (T1152). The resulting Task handle borrows that local: the goroutine may
// read it after it drops, so the handle must not escape the local's scope. The
// handle itself is genuinely OWNED (so the in-scope `<-t`/drop join the goroutine
// normally, with codegen clearing its drop flag) — only its *escape* is unsound.
// Mark it Owned and record it in goHandleBorrowedLocal so the escape checks in
// tryMove/tryMoveConsume reject the handle leaving scope (returned, stored,
// reassigned, sent on a channel) while leaving the consuming `<-t` await — which
// reads, not consumes — accepted. Also register a shared borrow of the local so
// it cannot be consumed while the handle is live. Returns true when the RHS was
// such a go-handle borrow, signalling the caller to skip tryMove (and the normal
// Owned-assignment) — the handle is sound in-scope; an escape is caught at the
// escape site. The `_` binding skips the tracking but still returns true — the
// discarded temporary joins the goroutine at statement end while the local is
// alive, which is sound. A non-go-handle RHS returns false and clears any stale
// same-name entry so a reused binding name is not mistaken for a handle.
func (c *Checker) trackGoHandleBinding(name string, value ast.Expr, pos ast.Pos) bool {
	if g := unwrapGoExpr(value); g != nil {
		if bl := c.goCallBorrowsOwnedLocal(g); bl != nil {
			if name != "_" {
				c.state[name] = Owned
				c.goHandleBorrowedLocal[name] = bl.Name
				if c.borrows != nil {
					c.borrows.Add(&Borrow{
						Origin:   bl.Name,
						Kind:     BorrowShared,
						Borrower: name,
						Pos:      pos,
					})
				}
				if typ := c.info.Types[value]; typ != nil {
					c.trackDeclOrder(name, typ)
				}
			}
			return true
		}
	}
	delete(c.goHandleBorrowedLocal, name)
	return false
}

func (c *Checker) checkWhileStmt(s *ast.WhileStmt) {
	c.checkExpr(s.Cond)
	// Expire call-scoped borrows from the condition so the loop body can
	// re-borrow the same variables.
	if c.borrows != nil {
		c.borrows.ExpireCallScoped()
	}
	savedState := c.state.clone()
	savedBorrows := c.borrows.Clone()
	snap := c.enterLoopBody()
	c.checkBlock(s.Body)
	c.mergeLoopState(s.Body, savedState, savedBorrows)
	c.exitLoopBody(snap)
}

func (c *Checker) checkWhileUnwrapStmt(s *ast.WhileUnwrapStmt) {
	c.checkExpr(s.Value)
	// T0589: same shape as if-let — `while x := a { … }` consumes the inner
	// heap value of a borrowed Optional parameter on each loop iteration.
	// Caller still owns and drops the same allocation → double-free / UAF on
	// the second iteration even if the first wouldn't crash. Reject under the
	// same predicate as if-let.
	if ident := c.findBorrowedDroppableOptionalIfletSource(s.Value); ident != nil {
		c.errorf(ident.Pos(),
			"cannot consume borrowed parameter '%s' via while-let; declare the parameter with `move` to consume the Optional, or wrap into a wider Optional via an intermediate local",
			ident.Name)
	}
	// Expire call-scoped borrows from the condition expression so the loop
	// body can re-borrow the same variables (B0004).
	if c.borrows != nil {
		c.borrows.ExpireCallScoped()
	}
	if s.Binding != "" && s.Binding != "_" {
		c.state[s.Binding] = Owned
	}
	savedState := c.state.clone()
	savedBorrows := c.borrows.Clone()
	snap := c.enterLoopBody()

	// T1153: the while-unwrap binding is a fresh owned value produced per iteration
	// (the unwrapped `opt.Elem()`), scoped to the loop body — its slot is freed at
	// the iteration boundary. Borrowing it into a `go` call lets the borrow escape
	// into a goroutine that may outlive the iteration → use-after-free. Flag it
	// (when owned/non-copy/droppable) so rejectGoCallLoopBindingBorrowEscape rejects
	// the go-call borrow uniformly, mirroring the for-in binding flagging (T1147).
	// The enterLoopBody snapshot restores the set at loop exit, so no per-key
	// save/restore is needed. No aliasing carve-out applies: the binding is a genuine
	// fresh owned value, not an alias into a container.
	if s.Binding != "" && s.Binding != "_" {
		if bt := c.loopBindingType(s.Body, s.Binding); bt != nil && !isCopyType(bt) && isDroppableType(bt) {
			c.forInOwnedDroppableBindings[s.Binding] = true
		}
	}

	c.checkBlock(s.Body)
	c.mergeLoopState(s.Body, savedState, savedBorrows)
	c.exitLoopBody(snap)
}

func (c *Checker) checkForInStmt(s *ast.ForInStmt) {
	c.checkExpr(s.Iterable)
	// For-in borrows the iterable for iteration — field reads are not
	// consumed, so skip the field-move check for MemberExpr (B0341).
	if _, isMember := s.Iterable.(*ast.MemberExpr); !isMember {
		c.tryMove(s.Iterable)
	}
	// Expire call-scoped borrows from the iterable expression so the loop
	// body can re-borrow the same variables.
	if c.borrows != nil {
		c.borrows.ExpireCallScoped()
	}
	if s.Binding != "_" {
		c.state[s.Binding] = Owned
	}
	if s.Index != "" && s.Index != "_" {
		c.state[s.Index] = Owned
	}

	// Element type aliased by a for-in binding over a native Vector/Array/Map-value
	// container (nil for the `_` binding or non-aliasing iterable shapes). Computed
	// once and shared by the single-owner (T0652), concrete-alias (T0978), and
	// bare-TypeParam (T1035) flagging blocks below. stripRefType is an identity for
	// owned/plain-borrow Instances and peels SharedRef/MutRef for borrowed-ref
	// containers, so one element covers all three container forms.
	var aliasElem types.Type
	if s.Binding != "_" {
		aliasElem = forInAliasingElementType(stripRefType(c.info.Types[s.Iterable]))
	}

	// T0652: for native Vector/Array/Map iteration whose element type is a
	// single-owner native handle, mark the binding so moves (x := h, foo(h),
	// use x := h, return h) are rejected — the binding aliases the slot. A
	// *borrowed* such container (`Task[int][]&`, newly iterable since T0971) is
	// covered too (aliasElem peels the ref), keeping the dedicated single-owner
	// message (and disjoint from the T0978 alias set, which excludes these).
	// Save/restore for nested-loop safety (e.g., `for x in v1 { for x in v2 {} }`).
	var prevSingleOwner bool
	var hadPrevSingleOwner bool
	flaggedSingleOwner := false
	if aliasElem != nil && isSingleOwnerNativeType(aliasElem) {
		prevSingleOwner, hadPrevSingleOwner = c.forInSingleOwnerBindings[s.Binding]
		c.forInSingleOwnerBindings[s.Binding] = true
		flaggedSingleOwner = true
	}

	// T0971/T0978: a for-in loop binding over a native Vector/Array/Map-value
	// container *aliases* the container's element storage (genForInVector /
	// genForInArray / genForInMap load each slot's Value struct directly — only
	// `string` elements are cloned via dupStrings). Whether the container is
	// owned, a plain-borrow parameter (`T[] src`), or a borrowed ref (`T[]&` /
	// `T[]~` / `.borrow`), its owner still drops every element at scope exit, so
	// moving the binding out (`sink.push(x)`, `y := x`, `return x`, passing to a
	// `~` param) would double-free. Flag it so tryMove/tryMoveConsume reject the
	// move and direct the user to `.clone()` / `.pop()` / `.remove()`. stripRefType
	// is an identity for owned/plain-borrow Instances and peels SharedRef/MutRef
	// for the borrowed-ref case, so one rule covers all three. The
	// movable/aliasing decision (Copy, string, and bare-TypeParam exclusions) lives
	// in forInElementAliasesContainer, shared with the destructure carve-out;
	// single-owner native handles keep the dedicated T0652 message (excluded here
	// so the two flag sets stay disjoint).
	var prevAlias bool
	var hadPrevAlias bool
	flaggedAlias := false
	// Single-owner native handles are routed to forInSingleOwnerBindings above
	// (T0652) for their dedicated message, so exclude them here to keep the two
	// flag sets disjoint.
	if forInElementAliasesContainer(aliasElem) && !isSingleOwnerNativeType(aliasElem) {
		prevAlias, hadPrevAlias = c.forInAliasBindings[s.Binding]
		c.forInAliasBindings[s.Binding] = true
		flaggedAlias = true
	}

	// T1035: a for-in over a native container whose element is a *bare TypeParam*
	// (`T[] v` / `Map[K, T]` value) can't be classified copy/non-copy in the
	// generic body, so forInElementAliasesContainer excludes it (avoiding
	// over-rejection of Copy/string instantiations). Record the binding's element
	// TypeParam so a move-out is deferred to per-instantiation validation
	// (recordDrainReq → propagateDrainReqs) rather than rejected inline. Skip
	// single-owner handles (their nested forms are already gated at sema/decl).
	var prevTPAlias *types.TypeParam
	var hadPrevTPAlias bool
	flaggedTPAlias := false
	if tp, ok := aliasElem.(*types.TypeParam); ok && !isSingleOwnerNativeType(aliasElem) {
		prevTPAlias, hadPrevTPAlias = c.forInTypeParamAliasBindings[s.Binding]
		c.forInTypeParamAliasBindings[s.Binding] = tp
		flaggedTPAlias = true
	}

	// Snapshot the owned-droppable set and raise loop depth BEFORE adding the
	// for-in binding below, so exitLoopBody removes both the binding and any
	// loop-body locals (added by flagLoopBodyOwnedLocal) together at loop exit.
	snap := c.enterLoopBody()

	// T1147: an OWNED (non-aliasing) non-copy droppable for-in binding — a string
	// element (dup'd per iteration), or an owned value yielded by an iterator /
	// Channel / generator. Its lifetime ends at the iteration boundary, so
	// borrowing it into a `go` call lets the borrow escape into a goroutine that
	// may outlive the iteration → use-after-free. The `!flaggedAlias &&
	// !flaggedSingleOwner` guard keeps this set disjoint from the aliasing sets
	// (heap-user / single-owner bindings point into a container that outlives the
	// loop and are sound). The whole-map snapshot above subsumes per-key
	// save/restore for nested-loop binding-name reuse.
	if s.Binding != "_" && !flaggedAlias && !flaggedSingleOwner {
		if bt := c.loopBindingType(s.Body, s.Binding); bt != nil && !isCopyType(bt) && isDroppableType(bt) {
			c.forInOwnedDroppableBindings[s.Binding] = true
		}
	}

	savedState := c.state.clone()
	savedBorrows := c.borrows.Clone()
	c.checkBlock(s.Body)
	c.mergeLoopState(s.Body, savedState, savedBorrows)
	c.exitLoopBody(snap)

	if flaggedSingleOwner {
		if hadPrevSingleOwner {
			c.forInSingleOwnerBindings[s.Binding] = prevSingleOwner
		} else {
			delete(c.forInSingleOwnerBindings, s.Binding)
		}
	}
	if flaggedAlias {
		if hadPrevAlias {
			c.forInAliasBindings[s.Binding] = prevAlias
		} else {
			delete(c.forInAliasBindings, s.Binding)
		}
	}
	if flaggedTPAlias {
		if hadPrevTPAlias {
			c.forInTypeParamAliasBindings[s.Binding] = prevTPAlias
		} else {
			delete(c.forInTypeParamAliasBindings, s.Binding)
		}
	}
}

// loopBindingType returns the type sema recorded for a loop binding, looked up
// from the loop body's scope (sema inserts the binding Var there). Shared by
// for-in (T1147) and while-unwrap (T1153) binding flagging. Returns nil if the
// scope or binding is unavailable.
func (c *Checker) loopBindingType(body ast.Node, binding string) types.Type {
	scope, ok := c.info.Scopes[body]
	if !ok || scope == nil {
		return nil
	}
	obj := scope.Lookup(binding)
	if obj == nil {
		return nil
	}
	return obj.Type()
}

// forInElementAliasesContainer reports whether a for-in element type (or a
// destructured piece of one) aliases the container's droppable storage such that
// moving the binding out would double-free at the container's drop. True for
// non-Copy heap user types, nested containers, Maps/Sets-as-elements, etc.
// (T0978). False — i.e. freely movable — for:
//   - nil (no aliasing element shape).
//   - Copy elements: value copies, independent of the container.
//   - string elements: dup'd on store (genForInVector dupStrings / push-time
//     string dup), verified leak/double-free-free at runtime.
//   - bare TypeParam elements: the ownership pass checks each generic body once
//     with `T` unbound and never re-checks monomorphized instances, so flagging
//     `T` here would over-reject legitimate Copy-`T`/string-`T` instantiations.
//     Instead the whole-binding for-in guard records the binding's element
//     TypeParam (forInTypeParamAliasBindings) and defers a per-instantiation
//     verdict to propagateDrainReqs, which validates the concrete substitution
//     with concreteElementAliasesContainer below (T1035).
//
// Note: single-owner native handles (Mutex/MutexGuard/Task) ARE aliasing and
// return true here; the whole-binding for-in guard additionally routes them to
// the dedicated forInSingleOwnerBindings set (T0652) for a tailored message, but
// a single-owner *destructured piece* relies on this predicate to be flagged.
func forInElementAliasesContainer(elem types.Type) bool {
	if elem == nil {
		return false
	}
	if _, isTypeParam := elem.(*types.TypeParam); isTypeParam {
		return false
	}
	return concreteElementAliasesContainer(elem)
}

// concreteElementAliasesContainer is the concrete-element aliasing predicate
// shared by the generic-body for-in guard (forInElementAliasesContainer) and the
// deferred per-instantiation drain validator (propagateDrainReqs). A non-nil,
// non-Copy, non-string element type aliases the container's droppable storage,
// so moving the for-in binding out double-frees. Copy elements are value copies
// and string elements are dup'd per iteration, so both are freely movable. (T1035)
func concreteElementAliasesContainer(elem types.Type) bool {
	if elem == nil {
		return false
	}
	return !isCopyType(elem) && extractNamedType(elem) != types.TypString
}

// stripRefType peels one borrow layer (MutRef/SharedRef) and returns the
// underlying type. Unlike extractNamedType it preserves the container type
// (Array/Vector/Map Instance) needed by forInAliasingElementType.
func stripRefType(typ types.Type) types.Type {
	switch t := typ.(type) {
	case *types.SharedRef:
		return t.Elem()
	case *types.MutRef:
		return t.Elem()
	}
	return typ
}

func (c *Checker) checkClassicForStmt(s *ast.ClassicForStmt) {
	if s.InitValue != nil {
		c.checkExpr(s.InitValue)
		c.tryMove(s.InitValue)
	}
	if s.InitName != "" && s.InitName != "_" {
		c.state[s.InitName] = Owned
	}
	// Expire call-scoped borrows from the init expression.
	if c.borrows != nil {
		c.borrows.ExpireCallScoped()
	}

	savedState := c.state.clone()
	savedBorrows := c.borrows.Clone()
	if s.Cond != nil {
		c.checkExpr(s.Cond)
	}
	// Expire call-scoped borrows from the condition so the loop body can
	// re-borrow the same variables.
	if c.borrows != nil {
		c.borrows.ExpireCallScoped()
	}
	snap := c.enterLoopBody()
	c.checkBlock(s.Body)
	if s.UpdateIncDec {
		if s.UpdateTarget != nil {
			c.checkExpr(s.UpdateTarget)
		}
	} else if s.UpdateValue != nil {
		c.checkExpr(s.UpdateValue)
	}
	c.mergeLoopState(s.Body, savedState, savedBorrows)
	c.exitLoopBody(snap)
}

func (c *Checker) checkSelectStmt(s *ast.SelectStmt) {
	// Each case channel expression is checked; at most one case executes.
	savedState := c.state.clone()
	savedBorrows := c.borrows.Clone()

	var states []StateMap
	var borrowSets []*BorrowSet

	for _, sc := range s.Cases {
		c.state = savedState.clone()
		c.borrows = savedBorrows.Clone()
		c.checkExpr(sc.Channel)
		if sc.IsSend && sc.SendValue != nil {
			c.checkExpr(sc.SendValue)
			c.tryMoveConsume(sc.SendValue)
			c.tryMoveConsumeCastSubject(sc.SendValue) // T0784
			// T1073: `case ch.send(o!)` — force-unwrap of a borrowed droppable
			// Optional param. The cast form is handled above; reject force-unwrap.
			if isForceUnwrapForm(sc.SendValue) {
				c.rejectBorrowedOptionalUnwrapConsume(sc.SendValue)
			}
		}
		// Expire call-scoped borrows from channel/send expressions so the case
		// body can re-borrow the same variables (B0103).
		if c.borrows != nil {
			c.borrows.ExpireCallScoped()
		}
		if !sc.IsSend && sc.Binding != "_" {
			c.state[sc.Binding] = Owned
		}
		for _, stmt := range sc.Body {
			c.checkStmt(stmt)
			if c.borrows != nil {
				c.borrows.ExpireCallScoped()
			}
		}
		// T1134: a case body that diverges never falls through to the post-select
		// path, so exclude its end-state (and any moves it performed) from the merge.
		if !stmtsDiverge(sc.Body) {
			states = append(states, c.state)
			borrowSets = append(borrowSets, c.borrows)
		}
	}

	if s.Default != nil {
		c.state = savedState.clone()
		c.borrows = savedBorrows.Clone()
		for _, stmt := range s.Default {
			c.checkStmt(stmt)
			if c.borrows != nil {
				c.borrows.ExpireCallScoped()
			}
		}
		if !stmtsDiverge(s.Default) {
			states = append(states, c.state)
			borrowSets = append(borrowSets, c.borrows)
		}
	}

	// Merge all branches
	if len(states) > 0 {
		merged := states[0]
		mergedBorrows := borrowSets[0]
		for i := 1; i < len(states); i++ {
			merged = merge(merged, states[i])
			mergedBorrows = MergeBorrowSets(mergedBorrows, borrowSets[i])
		}
		// Also merge with pre-select state (select might not execute any case if no default)
		if s.Default == nil {
			merged = merge(savedState, merged)
			mergedBorrows = MergeBorrowSets(savedBorrows, mergedBorrows)
		}
		c.state = merged
		c.borrows = mergedBorrows
	} else {
		c.state = savedState
		c.borrows = savedBorrows
	}
}

func (c *Checker) checkInfiniteLoop(s *ast.InfiniteLoop) {
	savedState := c.state.clone()
	savedBorrows := c.borrows.Clone()
	snap := c.enterLoopBody()
	c.checkBlock(s.Body)
	c.mergeLoopState(s.Body, savedState, savedBorrows)
	c.exitLoopBody(snap)
}

// mergeLoopState merges the post-body state of a loop with the pre-loop state.
// T1134: if the loop body always exits the function (return/raise) and has no
// `break` — e.g. `for x in xs { return s; }` — then the only way control
// reaches code after the loop is the zero-iteration path, so the post-loop
// state is exactly the pre-loop state. A move inside such a body never reaches
// post-loop code and must not poison it. When a `break` is present (or the body
// can fall through), the conservative merge is kept: a `break` transfers the
// body's current state to post-loop code, which the merge must still cover.
func (c *Checker) mergeLoopState(body *ast.Block, savedState StateMap, savedBorrows *BorrowSet) {
	if loopBodyExitsFunction(body) {
		c.state = savedState
		c.borrows = savedBorrows
		return
	}
	c.state = merge(savedState, c.state)
	c.borrows = MergeBorrowSets(savedBorrows, c.borrows)
}
