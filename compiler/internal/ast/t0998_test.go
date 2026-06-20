package ast

import (
	"strings"
	"testing"

	antlr "github.com/antlr4-go/antlr/v4"
	"github.com/promise-language/promise/compiler/internal/parser"
)

// t0998BuildErrs parses + builds and returns the builder errors without fataling,
// so the removed-spelling guiding migration errors can be asserted.
func t0998BuildErrs(t *testing.T, src string) []error {
	t.Helper()
	input := antlr.NewInputStream(src)
	lexer := parser.NewPromiseLexer(input)
	lexer.RemoveErrorListeners()
	stream := antlr.NewCommonTokenStream(lexer, antlr.TokenDefaultChannel)
	p := parser.NewPromiseParser(stream)
	p.RemoveErrorListeners()
	tree := p.CompilationUnit()
	_, errs := Build("test.pr", tree)
	return errs
}

func t0998ExpectErr(t *testing.T, errs []error, substr string) {
	t.Helper()
	for _, e := range errs {
		if strings.Contains(e.Error(), substr) {
			return
		}
	}
	t.Errorf("expected build error containing %q, got %v", substr, errs)
}

// The removed `~Type name` move-param spelling produces a guiding migration
// error mapping it to `Type move name` (VisitLegacyMoveParam).
func TestT0998_LegacyMoveParamGuidingError(t *testing.T) {
	errs := t0998BuildErrs(t, `sink(~Box b) {} type Box { string s; }`)
	t0998ExpectErr(t, errs, "no longer valid")
	t0998ExpectErr(t, errs, "Type move b")
}

// The removed `&this` receiver produces a guiding error mapping it to bare
// `this` (VisitReceiverParam RefShared branch).
func TestT0998_LegacyAmpThisGuidingError(t *testing.T) {
	errs := t0998BuildErrs(t, `type Box { string s; get_s(&this) string { return this.s; } }`)
	t0998ExpectErr(t, errs, "`&this` is no longer a valid receiver")
}

// A move param carrying a meta-annotation exercises the annotation loop in
// VisitMoveParam.
func TestT0998_MoveParamWithAnnotation(t *testing.T) {
	file := parseAndBuild(t, "sink(Box move b `inline) {} type Box { string s; }")
	fn := file.Decls[0].(*FuncDecl)
	assertLen(t, fn.Params, 1)
	assertEqual(t, fn.Params[0].RefMod, RefMut)
	assertLen(t, fn.Params[0].Annotations, 1)
}

// T0998: AST-level wiring for the keyword-based ownership surface syntax —
// `Type move name` params, the call-site `move <expr>` arg marker, and the
// re-attach of `move` to a lambda literal argument (`f(move || ...)`).

func TestT0998_MoveParamSetsRefMut(t *testing.T) {
	file := parseAndBuild(t, `sink(Box move b) {} type Box { string s; }`)
	fn := file.Decls[0].(*FuncDecl)
	assertLen(t, fn.Params, 1)
	assertEqual(t, fn.Params[0].Name, "b")
	assertEqual(t, fn.Params[0].RefMod, RefMut)
}

func TestT0998_CallSiteMoveSetsArgMove(t *testing.T) {
	file := parseAndBuild(t, `f() { sink(move x); }`)
	fn := file.Decls[0].(*FuncDecl)
	call := fn.Body.Stmts[0].(*ExprStmt).Expr.(*CallExpr)
	assertLen(t, call.Args, 1)
	assertTrue(t, call.Args[0].Move)
}

func TestT0998_BareArgHasNoMove(t *testing.T) {
	file := parseAndBuild(t, `f() { sink(x); }`)
	fn := file.Decls[0].(*FuncDecl)
	call := fn.Body.Stmts[0].(*ExprStmt).Expr.(*CallExpr)
	assertLen(t, call.Args, 1)
	assertFalse(t, call.Args[0].Move)
}

// `move` before a lambda literal argument is the lambda's move-capture keyword,
// not the call-site arg marker — the arg rule's greedy MOVE must be re-attached
// to the lambda (Arg.Move=false, LambdaExpr.Move=true).
func TestT0998_MoveLambdaArgReattaches(t *testing.T) {
	file := parseAndBuild(t, `f() { run(move |a| -> a); }`)
	fn := file.Decls[0].(*FuncDecl)
	call := fn.Body.Stmts[0].(*ExprStmt).Expr.(*CallExpr)
	assertLen(t, call.Args, 1)
	assertFalse(t, call.Args[0].Move)
	lam := call.Args[0].Value.(*LambdaExpr)
	assertTrue(t, lam.Move)
}

// A named lambda-arg keeps the same re-attach behavior.
func TestT0998_MoveLambdaNamedArgReattaches(t *testing.T) {
	file := parseAndBuild(t, `f() { build(next: move || -> 1); }`)
	fn := file.Decls[0].(*FuncDecl)
	call := fn.Body.Stmts[0].(*ExprStmt).Expr.(*CallExpr)
	assertLen(t, call.Args, 1)
	assertEqual(t, call.Args[0].Name, "next")
	assertFalse(t, call.Args[0].Move)
	lam := call.Args[0].Value.(*LambdaExpr)
	assertTrue(t, lam.Move)
}
