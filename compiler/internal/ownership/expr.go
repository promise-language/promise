package ownership

import (
	"djabi.dev/go/promise_lang/internal/ast"
	"djabi.dev/go/promise_lang/internal/types"
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

	case *ast.UnaryExpr:
		c.checkExpr(e.Operand)

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
		}
		if e.Block != nil {
			c.checkBlock(e.Block)
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
		}

	case *ast.ArrayLit:
		for _, elem := range e.Elements {
			c.checkExpr(elem)
			c.tryMoveConsume(elem)
		}

	case *ast.MapLit:
		for _, entry := range e.Entries {
			c.checkExpr(entry.Key)
			c.tryMoveConsume(entry.Key)
			c.checkExpr(entry.Value)
			c.tryMoveConsume(entry.Value)
		}

	// Literals have no sub-expressions to walk.
	case *ast.IntLit, *ast.FloatLit, *ast.BoolLit,
		*ast.NoneLit, *ast.CharLit, *ast.StringLit:
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

// tryMove marks the variable referenced by expr as Moved, if it is a
// non-copy variable tracked in the current state. Also checks that the
// variable is not actively borrowed. Borrowed parameters are not moved —
// reads stay legal — but consuming contexts (call to `~` param, etc.) must
// use tryMoveConsume to enforce the T0338 check.
func (c *Checker) tryMove(expr ast.Expr) {
	// B0341: check field reads from droppable owners before ident handling.
	if member, ok := expr.(*ast.MemberExpr); ok {
		c.checkFieldMoveOwnership(member)
		return
	}

	// `this` is a parameter, not a regular tracked variable; no move bookkeeping.
	// T0548: still reject moves of `this` while a borrow on it is active
	// (e.g., from `(b, n) := this.pair` registering Origin:"this").
	if this, ok := expr.(*ast.ThisExpr); ok {
		if c.borrows != nil && c.borrows.HasAnyBorrow("this") {
			c.errorf(this.Pos(), "cannot move 'this' while it is borrowed")
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
	// B0341 field-move check delegated to tryMove; inherit it here too.
	if member, ok := expr.(*ast.MemberExpr); ok {
		c.checkFieldMoveOwnership(member)
		return
	}
	// `this` consume requires `~this` receiver.
	if this, ok := expr.(*ast.ThisExpr); ok {
		if c.state["this"] == Borrowed {
			c.errorf(this.Pos(),
				"cannot move borrowed receiver 'this'; declare the method as 'method(~this)' to consume the receiver")
		} else if c.state["this"] == Moved {
			c.errorf(this.Pos(), "use of moved variable 'this'")
		} else if c.borrows != nil && c.borrows.HasAnyBorrow("this") {
			// T0548: reject consume of `this` while a borrow on it is
			// active (e.g., from `(b, n) := this.pair`).
			c.errorf(this.Pos(), "cannot move 'this' while it is borrowed")
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
				"cannot move borrowed parameter '%s'; add '~' to the parameter declaration to consume it",
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

// isNonDuppableNativeHandle reports whether t is a single-owner native handle
// (Mutex[T], MutexGuard[T], Task[T]) with no clone/dup semantics. Moving such
// a value out of a Borrowed param into a call argument has no codegen
// compensation (see maybeDupPushElement in codegen/expr.go), so both the
// callee-side consumer's drop and the caller's drop fire on the same
// allocation → runtime double-free (T0556).
func isNonDuppableNativeHandle(t types.Type) bool {
	if _, ok := types.AsMutex(t); ok {
		return true
	}
	if _, ok := types.AsMutexGuard(t); ok {
		return true
	}
	if _, ok := types.AsTask(t); ok {
		return true
	}
	return false
}

// findBorrowedNonDuppableIdent reports a borrowed identifier of a non-duppable
// single-owner handle type reachable through transparent wrappers (parens,
// if/else, match arms). Returns the offending ident or nil. The walk mirrors
// the type-driven shape that isBorrowedExpr uses for SharedRef/MutRef: sema
// propagates the value type through these compositions, so any branch that
// surfaces a Borrowed Mutex/Task/MutexGuard variable is unsafe.
func (c *Checker) findBorrowedNonDuppableIdent(expr ast.Expr) *ast.IdentExpr {
	switch e := expr.(type) {
	case *ast.IdentExpr:
		if state, tracked := c.state[e.Name]; tracked && state == Borrowed {
			if isNonDuppableNativeHandle(c.info.Types[e]) {
				return e
			}
		}
	case *ast.ParenExpr:
		return c.findBorrowedNonDuppableIdent(e.Expr)
	case *ast.IfExpr:
		if id := c.findBorrowedNonDuppableIdentInBlock(e.Then); id != nil {
			return id
		}
		return c.findBorrowedNonDuppableIdentInBlock(e.Else)
	case *ast.MatchExpr:
		for _, arm := range e.Arms {
			if arm.Body != nil {
				if id := c.findBorrowedNonDuppableIdent(arm.Body); id != nil {
					return id
				}
			}
			if arm.Block != nil {
				if id := c.findBorrowedNonDuppableIdentInBlock(arm.Block); id != nil {
					return id
				}
			}
		}
	}
	return nil
}

// findBorrowedNonDuppableIdentInBlock inspects a block's trailing expression
// (the block's result value). Mirrors codegen's clearBlockResultDropFlags.
func (c *Checker) findBorrowedNonDuppableIdentInBlock(block *ast.Block) *ast.IdentExpr {
	if block == nil || len(block.Stmts) == 0 {
		return nil
	}
	if es, ok := block.Stmts[len(block.Stmts)-1].(*ast.ExprStmt); ok {
		return c.findBorrowedNonDuppableIdent(es.Expr)
	}
	return nil
}

// checkCallExpr handles function calls and constructor calls.
// For function calls, arguments matched to value parameters trigger moves;
// arguments matched to borrow parameters create borrows.
// For constructor calls, all arguments are consumed.
func (c *Checker) checkCallExpr(e *ast.CallExpr) {
	c.checkExpr(e.Callee)

	sig := c.calleeSignature(e.Callee)
	if sig != nil {
		// Function/method call: process args left-to-right.
		params := sig.Params()
		for i, arg := range e.Args {
			c.checkExpr(arg.Value)
			if i < len(params) {
				kind := paramBorrowKind(params[i])
				if kind == BorrowNone {
					// T0556: Reject moves of borrowed non-duppable single-owner
					// handles (Mutex/MutexGuard/Task) into plain-param call args.
					// Unlike duppable types (Arc, Channel, Vector, string), there
					// is no codegen path to dup the value at the call site, so
					// the callee's consumer drop and the caller's drop both
					// fire on the same allocation → runtime double-free.
					if ident := c.findBorrowedNonDuppableIdent(arg.Value); ident != nil {
						if c.params[ident.Name] {
							c.errorf(ident.Pos(),
								"cannot move borrowed parameter '%s' of single-owner type into call argument; add '~' to the parameter declaration to consume it",
								ident.Name)
						} else {
							c.errorf(ident.Pos(),
								"cannot move borrowed value '%s' of single-owner type into call argument",
								ident.Name)
						}
						continue
					}
					c.tryMove(arg.Value)
				} else if params[i].Ref() == types.RefMut {
					// T0087: ~ on regular params means move (callee owns).
					// T0338: a `~` callee genuinely consumes the value, so the
					// arg must not be a borrowed parameter — the outer caller
					// still drops the original at scope exit.
					c.tryMoveConsume(arg.Value)
				} else {
					c.createBorrowWithKind(arg.Value, kind, e.Pos())
				}
			}
		}
		c.checkReceiverBorrow(e.Callee, sig, e.Pos())
		c.checkBorrowConflicts(e, sig)
	} else {
		// Constructor or unresolved call — all args are consumed.
		for _, arg := range e.Args {
			c.checkExpr(arg.Value)
			c.tryMoveConsume(arg.Value)
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

// checkReceiverBorrow creates a borrow for method calls with &this or ~this receivers.
// For nested member expressions like f.sub.method(), the borrow is on f with path ["sub"].
func (c *Checker) checkReceiverBorrow(callee ast.Expr, sig *types.Signature, pos ast.Pos) {
	if sig.Recv() == nil {
		return
	}
	// Receiver uses Ref() (grammar: receiverParam : refMod? THIS)
	recvKind := paramBorrowKind(sig.Recv())
	if recvKind == BorrowNone {
		return
	}
	member, ok := callee.(*ast.MemberExpr)
	if !ok {
		return
	}
	// member.Target is the receiver expression (the object the method is called on).
	// Pass it directly to createBorrowWithKind which handles both IdentExpr and MemberExpr.
	c.createBorrowWithKind(member.Target, recvKind, pos)
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
	c.state = merge(thenState, elseState)
	c.borrows = MergeBorrowSets(thenBorrows, elseBorrows)
}

func (c *Checker) checkMatchExpr(e *ast.MatchExpr) {
	c.checkExpr(e.Subject)
	if len(e.Arms) == 0 {
		return
	}

	savedState := c.state.clone()
	savedBorrows := c.borrows.Clone()
	var states []StateMap
	var borrowSets []*BorrowSet

	for _, arm := range e.Arms {
		c.state = savedState.clone()
		c.borrows = savedBorrows.Clone()
		c.registerPatternBindings(arm.Pattern)
		if arm.Guard != nil {
			c.checkExpr(arm.Guard)
		}
		if arm.Body != nil {
			c.checkExpr(arm.Body)
		}
		if arm.Block != nil {
			c.checkBlock(arm.Block)
		}
		states = append(states, c.state)
		borrowSets = append(borrowSets, c.borrows)
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
		if me, ok := target.(*ast.MemberExpr); ok {
			target = me.Target
		} else {
			break
		}
	}
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
						"cannot move-capture borrowed parameter '%s' into a lambda; add '~' to the parameter declaration to consume it",
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
}
