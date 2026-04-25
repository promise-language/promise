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
// non-copy variable tracked in the current state. Does not report errors;
// use-after-move is caught by checkIdentUse.
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
	c.state[ident.Name] = Moved
}

// --- Call expressions ---

// checkCallExpr handles function calls and constructor calls.
// For function calls, arguments matched to value parameters trigger moves.
// For constructor calls, all arguments are consumed.
func (c *Checker) checkCallExpr(e *ast.CallExpr) {
	c.checkExpr(e.Callee)

	sig := c.calleeSignature(e.Callee)
	if sig != nil {
		// Function/method call: process args left-to-right with move tracking.
		params := sig.Params()
		for i, arg := range e.Args {
			c.checkExpr(arg.Value)
			if i < len(params) && params[i].Ref() == types.RefNone {
				c.tryMove(arg.Value)
			}
		}
		c.checkBorrowConflicts(e, sig)
	} else {
		// Constructor or unresolved call — all args are consumed.
		for _, arg := range e.Args {
			c.checkExpr(arg.Value)
			c.tryMove(arg.Value)
		}
	}
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
	ref  types.RefMod
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
		ref := params[i].Ref()
		if ref == types.RefNone {
			continue
		}
		ident, ok := arg.Value.(*ast.IdentExpr)
		if !ok {
			continue
		}
		borrows = append(borrows, borrowEntry{name: ident.Name, ref: ref})
	}

	for i := 0; i < len(borrows); i++ {
		for j := i + 1; j < len(borrows); j++ {
			if borrows[i].name != borrows[j].name {
				continue
			}
			if borrows[i].ref == types.RefMut || borrows[j].ref == types.RefMut {
				other := borrows[i].ref
				if borrows[i].ref == types.RefMut {
					other = borrows[j].ref
				}
				c.errorf(e.Pos(), "cannot borrow '%s' as mutable because it is also borrowed as %s in the same call",
					borrows[i].name, refLabel(other))
				return
			}
		}
	}
}

func refLabel(r types.RefMod) string {
	switch r {
	case types.RefShared:
		return "shared"
	case types.RefMut:
		return "mutable"
	default:
		return "value"
	}
}

// --- Control flow expressions ---

func (c *Checker) checkIfExpr(e *ast.IfExpr) {
	c.checkExpr(e.Cond)
	saved := c.state.clone()
	c.checkBlock(e.Then)
	thenState := c.state
	c.state = saved.clone()
	c.checkBlock(e.Else)
	elseState := c.state
	c.state = merge(thenState, elseState)
}

func (c *Checker) checkMatchExpr(e *ast.MatchExpr) {
	c.checkExpr(e.Subject)
	if len(e.Arms) == 0 {
		return
	}

	saved := c.state.clone()
	var states []StateMap

	for _, arm := range e.Arms {
		c.state = saved.clone()
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
	}

	result := states[0]
	for _, s := range states[1:] {
		result = merge(result, s)
	}
	c.state = result
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
	saved := c.state.clone()
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
	c.state = saved
}
