package ownership

import (
	"sort"
	"strings"

	"github.com/promise-language/promise/compiler/internal/ast"
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
					"cannot consume 'this'; the receiver belongs to the caller — call `.clone()` to produce an independent copy, or refactor into a free function taking `~Type`")
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

	case *ast.YieldDelegateStmt:
		c.checkExpr(s.Value)
		c.tryMoveConsume(s.Value)
		c.tryMoveConsumeCastSubject(s.Value) // T0784

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
					"cannot consume 'this'; the receiver belongs to the caller — call `.clone()` to produce an independent copy, or refactor into a free function taking `~Type`")
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
		// Weak / Mutex / MutexGuard / Task plus Optional thereof): for those
		// codegen propagates the RHS's drop flag (cleared, for a borrowed
		// param) or auto-dups via `setDupFlagsForFieldAccess`, so the LHS
		// does not double-drop.
		if c.rejectBorrowedIdentVarDecl(s.Value, s.Type) {
			return
		}
		// T0811: `Plain p = o!` / `Der? d = o as Der` etc. — see
		// checkInferredVarDecl. Reject the wrapper-consume of a borrowed
		// droppable Optional parameter before tryMove.
		if c.rejectBorrowedOptionalUnwrapConsume(s.Value) {
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
	if isCopyType(typ) || isVarDeclAliasSafeType(typ) || !isDroppableType(typ) {
		return false
	}
	// Skip when the LHS adds Optional wrap layers — that's a coercion, not
	// an alias, and codegen materializes a wrapped value.
	if optionalDepthTypeRef(lhsRef) > optionalDepthType(typ) {
		return false
	}
	if c.params[ident.Name] {
		c.errorf(ident.Pos(),
			"cannot move borrowed parameter '%s'; add '~' to the parameter declaration to consume it",
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

// isBorrowedOptionalType reports whether typ is a reference to an Optional
// (`T?&` / `T?~`) — a borrowed optional, e.g. the result of `Arc[T?].borrow` or
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
	// T0576: see checkTypedVarDecl — same crash class on `x := this`.
	if this, isThis := s.Value.(*ast.ThisExpr); isThis {
		if c.state["this"] == Moved {
			c.errorf(this.Pos(), "use of moved variable 'this'")
		} else if c.borrows != nil && c.borrows.HasAnyBorrow("this") {
			c.errorf(this.Pos(), "cannot move 'this' while it is borrowed")
		} else {
			c.errorf(this.Pos(),
				"cannot consume 'this'; the receiver belongs to the caller — call `.clone()` to produce an independent copy, or refactor into a free function taking `~Type`")
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
	c.tryMove(s.Value)
	if s.Name != "_" {
		c.state[s.Name] = Owned
		c.promoteCallBorrows(s.Name, s.Value)
		if typ := c.info.Types[s.Value]; typ != nil {
			c.trackDeclOrder(s.Name, typ)
		}
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
	if !rhsIsBorrowGetter {
		// T0351: assignment consumes the RHS — borrowed params cause a double-free
		// because the caller still drops the original. tryMoveConsume rejects them
		// at compile time (matches T0338/T0349 pattern for raise/yield/select-send).
		c.tryMoveConsume(s.Value)
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
			// Cannot reassign a variable that is actively borrowed by others
			if c.borrows != nil && c.borrows.HasAnyBorrow(ident.Name) {
				c.errorf(s.Pos(), "cannot assign to '%s' while it is borrowed", ident.Name)
			}
			// Expire borrows where this variable is the borrower
			if c.borrows != nil {
				c.borrows.ExpireBorrower(ident.Name)
			}
			if _, tracked := c.state[ident.Name]; tracked {
				if rhsIsBorrowGetter {
					c.state[ident.Name] = Borrowed
				} else {
					c.state[ident.Name] = Owned
				}
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
//     `Arc[T].borrow` and `MutexGuard[T].borrow` return `T&` post-T0381.
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
// affordance ("add '~' to the parameter declaration") only applies to params.
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
// findBorrowedDroppableOptionalIfletSource (T0589); same "add '~'" affordance.
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
			"cannot consume borrowed parameter '%s' via force-unwrap/cast; add '~' to the parameter declaration to consume the Optional",
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
				"cannot consume borrowed parameter '%s' via if-let; add '~' to the parameter declaration to consume the Optional, or wrap into a wider Optional via an intermediate local",
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
		// T0850: a borrowed optional scrutinee (`T?&` / `T?~`, e.g. `Arc[T?].borrow`)
		// binds a non-owning view of the external owner's payload — mark it Borrowed
		// so moving it out (into an owned var-decl / `~` arg) is rejected (T0568),
		// preventing a double-free with the owner's drop. Owned optionals stay Owned.
		if t := c.info.Types[s.Init]; isBorrowedOptionalType(t) {
			c.state[s.Binding] = Borrowed
		} else {
			c.state[s.Binding] = Owned
		}
	}
	c.checkBlock(s.Body)
	thenState := c.state
	thenBorrows := c.borrows

	if s.Else != nil {
		c.state = savedState.clone()
		c.borrows = savedBorrows.Clone()
		c.checkStmt(s.Else)
		elseState := c.state
		elseBorrows := c.borrows
		c.state = merge(thenState, elseState)
		c.borrows = MergeBorrowSets(thenBorrows, elseBorrows)
	} else {
		// No else: conservative merge with pre-if state.
		c.state = merge(savedState, thenState)
		c.borrows = MergeBorrowSets(savedBorrows, thenBorrows)
	}
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
	c.checkBlock(s.Body)
	c.state = merge(savedState, c.state)
	c.borrows = MergeBorrowSets(savedBorrows, c.borrows)
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
			"cannot consume borrowed parameter '%s' via while-let; add '~' to the parameter declaration to consume the Optional, or wrap into a wider Optional via an intermediate local",
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
	c.checkBlock(s.Body)
	c.state = merge(savedState, c.state)
	c.borrows = MergeBorrowSets(savedBorrows, c.borrows)
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

	// T0652: for native Vector/Array/Map iteration whose element type is a
	// single-owner native handle, mark the binding so moves (x := h, foo(h),
	// use x := h, return h) are rejected — the binding aliases the slot.
	// Save/restore for nested-loop safety (e.g., `for x in v1 { for x in v2 {} }`).
	var prevSingleOwner bool
	var hadPrevSingleOwner bool
	flaggedSingleOwner := false
	if s.Binding != "_" {
		iterType := c.info.Types[s.Iterable]
		if elem := forInAliasingElementType(iterType); elem != nil && isSingleOwnerNativeType(elem) {
			prevSingleOwner, hadPrevSingleOwner = c.forInSingleOwnerBindings[s.Binding]
			c.forInSingleOwnerBindings[s.Binding] = true
			flaggedSingleOwner = true
		}
	}

	savedState := c.state.clone()
	savedBorrows := c.borrows.Clone()
	c.checkBlock(s.Body)
	c.state = merge(savedState, c.state)
	c.borrows = MergeBorrowSets(savedBorrows, c.borrows)

	if flaggedSingleOwner {
		if hadPrevSingleOwner {
			c.forInSingleOwnerBindings[s.Binding] = prevSingleOwner
		} else {
			delete(c.forInSingleOwnerBindings, s.Binding)
		}
	}
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
	c.checkBlock(s.Body)
	if s.UpdateIncDec {
		if s.UpdateTarget != nil {
			c.checkExpr(s.UpdateTarget)
		}
	} else if s.UpdateValue != nil {
		c.checkExpr(s.UpdateValue)
	}
	c.state = merge(savedState, c.state)
	c.borrows = MergeBorrowSets(savedBorrows, c.borrows)
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
		states = append(states, c.state)
		borrowSets = append(borrowSets, c.borrows)
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
		states = append(states, c.state)
		borrowSets = append(borrowSets, c.borrows)
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
	c.checkBlock(s.Body)
	c.state = merge(savedState, c.state)
	c.borrows = MergeBorrowSets(savedBorrows, c.borrows)
}
