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
	if _, ok := expr.(*ast.ThisExpr); ok {
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
	// B0341 field-move check delegated to tryMove; inherit it here too.
	if member, ok := expr.(*ast.MemberExpr); ok {
		// T0380: `.borrow` on Arc/MutexGuard returns a non-owning reference;
		// moving it transfers ownership of the inner pointer to the consumer
		// while the parent retains its own drop responsibility → double-free.
		if c.isBorrowGetterExpr(member) {
			c.errorf(member.Pos(),
				"cannot move out of '.borrow' getter; the parent Arc/Mutex retains ownership — call .clone() to create an independent copy, or assign to a variable to bind a borrow")
			return
		}
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
	if c.pinned[ident.Name] {
		c.errorf(ident.Pos(), "cannot move use-bound variable '%s'", ident.Name)
		return
	}
	// T0338: cannot consume a borrowed parameter — the caller still drops it.
	if c.state[ident.Name] == Borrowed {
		if c.params[ident.Name] {
			c.errorf(ident.Pos(),
				"cannot move borrowed parameter '%s'; add '~' to the parameter declaration to consume it",
				ident.Name)
		} else {
			c.errorf(ident.Pos(), "cannot move borrowed value '%s'", ident.Name)
		}
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

// isBorrowGetterExpr returns true if expr is the `.borrow` getter on Arc[T] or
// MutexGuard[T] AND T is non-Copy. These getters return a non-owning reference
// to the inner T; for non-Copy T, a move out of the result causes a double-free
// since the parent Arc/Mutex retains ownership and drops on its own destruction
// (T0380). For Copy T (int, float, bool, etc.) the move is a value copy with no
// shared ownership, so no rejection is needed.
func (c *Checker) isBorrowGetterExpr(expr ast.Expr) bool {
	member, ok := expr.(*ast.MemberExpr)
	if !ok || member.Field != "borrow" {
		return false
	}
	targetType := c.info.Types[member.Target]
	if elem, ok := types.AsArc(targetType); ok {
		return !isCopyType(elem)
	}
	if elem, ok := types.AsMutexGuard(targetType); ok {
		return !isCopyType(elem)
	}
	if n := extractNamedType(targetType); n == types.TypArc || n == types.TypMutexGuard {
		// Bare Arc/MutexGuard without instantiation — conservative reject.
		return true
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
		return n.HasDrop() || n.NeedsSynthDrop()
	}
	if e, ok := typ.(*types.Enum); ok {
		return e.HasDrop() || e.NeedsSynthDrop()
	}
	return false
}

// isDroppableType reports whether a type has drop semantics (explicit or synthesized).
// For Optional types, checks the wrapped element recursively.
func isDroppableType(typ types.Type) bool {
	switch t := typ.(type) {
	case *types.Named:
		return t.HasDrop() || t.NeedsSynthDrop()
	case *types.Instance:
		if n, ok := t.Origin().(*types.Named); ok {
			return n.HasDrop() || n.NeedsSynthDrop()
		}
	case *types.Enum:
		return t.HasDrop() || t.NeedsSynthDrop()
	case *types.Optional:
		return isDroppableType(t.Elem())
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

	// Captured variables are fresh owned values inside the lambda
	if captures := c.info.LambdaCaptures[e]; len(captures) > 0 {
		for _, cv := range captures {
			c.state[cv.Obj.Name()] = Owned
		}
	}

	for _, p := range e.Params {
		if p.Name != "_" {
			c.state[p.Name] = Owned
		}
	}
	if e.Body != nil {
		c.checkBlock(e.Body)
	}
	if e.ExprBody != nil {
		c.checkExpr(e.ExprBody)
	}
	c.state = savedState
	c.borrows = savedBorrows
}
