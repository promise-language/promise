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

	case *ast.SliceExpr:
		c.checkExpr(e.Target)
		if e.Low != nil {
			c.checkExpr(e.Low)
		}
		if e.High != nil {
			c.checkExpr(e.High)
		}

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

	case *ast.ErrorUnwrapExpr:
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
		for _, elem := range e.Elements {
			c.checkExpr(elem)
			c.tryMove(elem)
		}

	case *ast.ArrayLit:
		for _, elem := range e.Elements {
			c.checkExpr(elem)
			c.tryMove(elem)
		}

	case *ast.MapLit:
		for _, entry := range e.Entries {
			c.checkExpr(entry.Key)
			c.tryMove(entry.Key)
			c.checkExpr(entry.Value)
			c.tryMove(entry.Value)
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
// variable is not actively borrowed.
func (c *Checker) tryMove(expr ast.Expr) {
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
	// Cannot move while borrowed
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
			c.tryMove(arg.Value)
		}
	}
}

// createBorrowWithKind checks for borrow conflicts with existing borrows and registers a new borrow.
func (c *Checker) createBorrowWithKind(expr ast.Expr, kind BorrowKind, pos ast.Pos) {
	ident, ok := expr.(*ast.IdentExpr)
	if !ok {
		return
	}
	name := ident.Name
	if c.borrows == nil {
		return
	}
	// Copy types don't need borrow tracking
	if obj := c.info.Objects[ident]; obj != nil {
		if v, ok := obj.(*types.Var); ok && isCopyType(v.Type()) {
			return
		}
	}

	// Check against existing borrows
	if kind == BorrowMut && c.borrows.HasAnyBorrow(name) {
		c.errorf(pos, "cannot borrow '%s' as mutable — it is already borrowed", name)
		return
	}
	if kind == BorrowShared && c.borrows.HasMutBorrow(name) {
		c.errorf(pos, "cannot borrow '%s' as shared — it is mutably borrowed", name)
		return
	}

	c.borrows.Add(&Borrow{Origin: name, Kind: kind, Pos: pos})
}

// checkReceiverBorrow creates a borrow for method calls with &this or ~this receivers.
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
	ident, ok := member.Target.(*ast.IdentExpr)
	if !ok {
		return
	}
	c.createBorrowWithKind(ident, recvKind, pos)
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
	kind BorrowKind
}

// checkBorrowConflicts detects conflicting borrows at a single call site.
// Multiple shared borrows of the same variable are OK.
// A mutable borrow combined with any other borrow of the same variable is an error.
func (c *Checker) checkBorrowConflicts(e *ast.CallExpr, sig *types.Signature) {
	params := sig.Params()
	var borrows []borrowEntry

	for i, arg := range e.Args {
		if i >= len(params) {
			break
		}
		kind := paramBorrowKind(params[i])
		if kind == BorrowNone {
			continue
		}
		ident, ok := arg.Value.(*ast.IdentExpr)
		if !ok {
			continue
		}
		borrows = append(borrows, borrowEntry{name: ident.Name, kind: kind})
	}

	for i := 0; i < len(borrows); i++ {
		for j := i + 1; j < len(borrows); j++ {
			if borrows[i].name != borrows[j].name {
				continue
			}
			if borrows[i].kind == BorrowMut || borrows[j].kind == BorrowMut {
				other := borrows[i].kind
				if borrows[i].kind == BorrowMut {
					other = borrows[j].kind
				}
				c.errorf(e.Pos(), "cannot borrow '%s' as mutable because it is also borrowed as %s in the same call",
					borrows[i].name, borrowKindLabel(other))
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
	}
}

// --- Lambda expressions ---

func (c *Checker) checkLambdaExpr(e *ast.LambdaExpr) {
	// Mark move-captured variables as moved in the enclosing scope
	if captures := c.info.LambdaCaptures[e]; len(captures) > 0 {
		for _, cv := range captures {
			if cv.ByMove {
				c.state[cv.Obj.Name()] = Moved
			}
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
