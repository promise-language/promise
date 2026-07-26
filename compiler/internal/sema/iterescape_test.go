package sema

import (
	"testing"

	"github.com/promise-language/promise/compiler/internal/ast"
	"github.com/promise-language/promise/compiler/internal/types"
)

// The tests below drive the T1349 return-holds-receiver walker (iterescape.go)
// directly on hand-built AST. Real std iterator methods only exercise the
// CallExpr+arg+lambda shape, so the walker's other value/statement/expression
// forms (paren/cast/tuple/array wrapping, every control-flow statement, if/match
// return contexts) are covered here to lock in the "detect a returned
// this-capturing lambda no matter how it is wrapped" contract.

// walkerChecker returns a bare Checker with just enough info to run the walker.
func walkerChecker() *Checker {
	return &Checker{info: &Info{
		LambdaCaptures: make(map[*ast.LambdaExpr][]*CapturedVar),
	}}
}

// thisLambda builds a lambda registered as capturing `this`.
func (c *Checker) thisLambda() *ast.LambdaExpr {
	l := &ast.LambdaExpr{}
	c.info.LambdaCaptures[l] = []*CapturedVar{{Obj: types.NewVar(types.Pos{}, "this", nil)}}
	return l
}

// plainLambda builds a lambda that captures a non-`this` variable.
func (c *Checker) plainLambda() *ast.LambdaExpr {
	l := &ast.LambdaExpr{}
	c.info.LambdaCaptures[l] = []*CapturedVar{{Obj: types.NewVar(types.Pos{}, "x", nil)}}
	return l
}

func blockOf(stmts ...ast.Stmt) *ast.Block { return &ast.Block{Stmts: stmts} }
func ret(e ast.Expr) *ast.ReturnStmt       { return &ast.ReturnStmt{Value: e} }

// --- valueHoldsThis: every wrapper form around a returned this-capturing lambda.

func TestReturnedHoldsThis_ValueForms(t *testing.T) {
	c := walkerChecker()
	lam := func() ast.Expr { return c.thisLambda() }

	cases := map[string]ast.Expr{
		"bareLambda": lam(),
		"paren":      &ast.ParenExpr{Expr: lam()},
		"cast":       &ast.CastExpr{Expr: lam()},
		"callCallee": &ast.CallExpr{Callee: &ast.ParenExpr{Expr: lam()}},
		"callArg":    &ast.CallExpr{Callee: &ast.IdentExpr{Name: "_FnIter"}, Args: []*ast.Arg{{Value: lam()}}},
		"tuple":      &ast.TupleLit{Elements: []ast.Expr{lam()}},
		"array":      &ast.ArrayLit{Elements: []ast.Expr{lam()}},
		"nestedCast": &ast.CastExpr{Expr: &ast.ParenExpr{Expr: lam()}},
	}
	for name, val := range cases {
		if !c.returnedExprHoldsThis(blockOf(ret(val))) {
			t.Errorf("%s: expected returnedExprHoldsThis=true", name)
		}
	}
}

func TestReturnedHoldsThis_Negatives(t *testing.T) {
	c := walkerChecker()
	cases := map[string]*ast.Block{
		"nilBlock":        nil,
		"emptyBlock":      blockOf(),
		"bareReturn":      blockOf(&ast.ReturnStmt{Value: nil}),
		"plainLambda":     blockOf(ret(c.plainLambda())),
		"identReturn":     blockOf(ret(&ast.IdentExpr{Name: "y"})),
		"emptyCallArgs":   blockOf(ret(&ast.CallExpr{Callee: &ast.IdentExpr{Name: "f"}})),
		"emptyTuple":      blockOf(ret(&ast.TupleLit{})),
		"emptyArray":      blockOf(ret(&ast.ArrayLit{})),
		"nilValueInParen": blockOf(ret(&ast.ParenExpr{Expr: nil})),
	}
	for name, blk := range cases {
		if c.returnedExprHoldsThis(blk) {
			t.Errorf("%s: expected returnedExprHoldsThis=false", name)
		}
	}
}

// --- walkStmt: every statement form that can transitively hold a return.

func TestReturnedHoldsThis_StatementForms(t *testing.T) {
	c := walkerChecker()
	inner := func() *ast.Block { return blockOf(ret(c.thisLambda())) }

	cases := map[string]ast.Stmt{
		"nestedBlock":   inner(),
		"if":            &ast.IfStmt{Body: inner()},
		"ifElseBlock":   &ast.IfStmt{Body: blockOf(), Else: inner()},
		"ifElseIf":      &ast.IfStmt{Body: blockOf(), Else: &ast.IfStmt{Body: inner()}},
		"while":         &ast.WhileStmt{Body: inner()},
		"whileUnwrap":   &ast.WhileUnwrapStmt{Body: inner()},
		"forIn":         &ast.ForInStmt{Body: inner()},
		"classicFor":    &ast.ClassicForStmt{Body: inner()},
		"infiniteLoop":  &ast.InfiniteLoop{Body: inner()},
		"selectCase":    &ast.SelectStmt{Cases: []*ast.SelectCase{{Body: []ast.Stmt{ret(c.thisLambda())}}}},
		"selectDefault": &ast.SelectStmt{Default: []ast.Stmt{ret(c.thisLambda())}},
	}
	for name, st := range cases {
		if !c.returnedExprHoldsThis(blockOf(st)) {
			t.Errorf("%s: expected returnedExprHoldsThis=true", name)
		}
	}
}

// --- walkExpr: return-context expression forms reached via a return value that
// itself embeds nested returns (e.g. `return if c { return E } else {...}`).

func TestReturnedHoldsThis_ExprForms(t *testing.T) {
	c := walkerChecker()
	inner := func() *ast.Block { return blockOf(ret(c.thisLambda())) }

	// A return whose value is a control-flow expr embedding a nested return.
	cases := map[string]ast.Expr{
		"ifThen":       &ast.IfExpr{Then: inner(), Else: blockOf()},
		"ifElse":       &ast.IfExpr{Then: blockOf(), Else: inner()},
		"matchArmBlk":  &ast.MatchExpr{Arms: []*ast.MatchArm{{Block: inner()}}},
		"matchArmBody": &ast.MatchExpr{Arms: []*ast.MatchArm{{Body: &ast.IfExpr{Then: inner(), Else: blockOf()}}}},
		"errHandler":   &ast.ErrorHandlerExpr{Body: inner()},
		"errElse":      &ast.ErrorHandlerExpr{ElseBody: inner()},
		"unsafe":       &ast.UnsafeExpr{Body: inner()},
		"parenNested":  &ast.ParenExpr{Expr: &ast.IfExpr{Then: inner(), Else: blockOf()}},
	}
	for name, val := range cases {
		if !c.returnedExprHoldsThis(blockOf(ret(val))) {
			t.Errorf("%s: expected returnedExprHoldsThis=true", name)
		}
	}
}

// --- walkStmt reaches returns nested inside statement-position control flow
// whose values are themselves control-flow expressions (walkExpr entry via
// ExprStmt and var-decl values).

func TestReturnedHoldsThis_NestedExprStatements(t *testing.T) {
	c := walkerChecker()
	retLam := func() ast.Expr { return &ast.IfExpr{Then: blockOf(ret(c.thisLambda())), Else: blockOf()} }

	cases := map[string]ast.Stmt{
		"exprStmt":        &ast.ExprStmt{Expr: retLam()},
		"typedVarDecl":    &ast.TypedVarDecl{Name: "v", Value: retLam()},
		"inferredVarDecl": &ast.InferredVarDecl{Name: "v", Value: retLam()},
		"destructureDecl": &ast.DestructureVarDecl{Names: []string{"a", "b"}, Value: retLam()},
		"useVarDecl":      &ast.UseVarDecl{Name: "v", Value: retLam()},
		"assignStmt":      &ast.AssignStmt{Target: &ast.IdentExpr{Name: "v"}, Value: retLam()},
	}
	for name, st := range cases {
		if !c.returnedExprHoldsThis(blockOf(st)) {
			t.Errorf("%s: expected returnedExprHoldsThis=true", name)
		}
	}
}

// A block whose first statement already holds `this` must short-circuit the
// remaining statements (walkBlock / walkStmt / walkExpr found-guards).
func TestReturnedHoldsThis_ShortCircuits(t *testing.T) {
	c := walkerChecker()
	// First return sets found; the trailing statements exercise every found-guard
	// (walkBlock loop, walkStmt entry, walkExpr entry) without changing the result.
	blk := blockOf(
		ret(c.thisLambda()),
		&ast.ExprStmt{Expr: &ast.IfExpr{Then: blockOf(ret(c.thisLambda())), Else: blockOf()}},
		ret(c.thisLambda()),
	)
	if !c.returnedExprHoldsThis(blk) {
		t.Error("expected returnedExprHoldsThis=true for short-circuit block")
	}

	// A match whose first arm sets found forces walkExpr's own found-guard to fire
	// when the loop reaches the second arm's body expression.
	m := &ast.MatchExpr{Arms: []*ast.MatchArm{
		{Block: blockOf(ret(c.thisLambda()))},
		{Body: &ast.IfExpr{Then: blockOf(ret(c.thisLambda())), Else: blockOf()}},
	}}
	if !c.returnedExprHoldsThis(blockOf(ret(m))) {
		t.Error("expected returnedExprHoldsThis=true for short-circuit match")
	}
}

// --- recordReturnHoldsReceiver guards: nil sig/body, nil receiver, Copy receiver.

func TestRecordReturnHoldsReceiver_Guards(t *testing.T) {
	c := walkerChecker()
	body := blockOf(ret(c.thisLambda()))

	// nil sig / nil body: no panic, nothing to set.
	c.recordReturnHoldsReceiver(nil, body)
	c.recordReturnHoldsReceiver(types.NewSignature(nil, nil, nil, false), nil)

	// No receiver (free function): flag stays false even though body holds a
	// this-capturing lambda.
	freeSig := types.NewSignature(nil, nil, nil, false)
	c.recordReturnHoldsReceiver(freeSig, body)
	if freeSig.ReturnHoldsReceiver() {
		t.Error("free function (nil recv) must not be flagged")
	}

	// Copy receiver: captured by value, so an escaping iterator over it is safe.
	copyRecv := types.NewParam("this", types.TypInt, types.RefNone)
	copySig := types.NewSignature(copyRecv, nil, nil, false)
	c.recordReturnHoldsReceiver(copySig, body)
	if copySig.ReturnHoldsReceiver() {
		t.Error("Copy receiver must not be flagged")
	}
}
