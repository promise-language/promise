package ownership

import (
	"strings"
	"testing"

	"djabi.dev/go/promise_lang/internal/ast"
	"djabi.dev/go/promise_lang/internal/parser"
	"djabi.dev/go/promise_lang/internal/sema"
	"djabi.dev/go/promise_lang/internal/types"
	antlr "github.com/antlr4-go/antlr/v4"
)

// --- Test helpers ---

// checkOwnership parses source, runs sema, then runs ownership analysis.
func checkOwnership(t *testing.T, src string) []error {
	t.Helper()
	input := antlr.NewInputStream(src)
	lexer := parser.NewPromiseLexer(input)
	lexer.RemoveErrorListeners()
	stream := antlr.NewCommonTokenStream(lexer, antlr.TokenDefaultChannel)
	p := parser.NewPromiseParser(stream)
	p.RemoveErrorListeners()
	tree := p.CompilationUnit()
	file, buildErrs := ast.Build("test.pr", tree)
	if len(buildErrs) > 0 {
		t.Fatalf("AST build errors: %v", buildErrs)
	}
	info, semaErrs := sema.Check(file)
	if len(semaErrs) > 0 {
		t.Fatalf("sema errors: %v", semaErrs)
	}
	return Check(file, info)
}

func ownerOK(t *testing.T, src string) {
	t.Helper()
	errs := checkOwnership(t, src)
	if len(errs) > 0 {
		t.Errorf("unexpected ownership errors: %v", errs)
	}
}

func ownerErrs(t *testing.T, src string) []error {
	t.Helper()
	return checkOwnership(t, src)
}

func expectOwnerError(t *testing.T, errs []error, substr string) {
	t.Helper()
	for _, e := range errs {
		if strings.Contains(e.Error(), substr) {
			return
		}
	}
	t.Errorf("expected ownership error containing %q, got %v", substr, errs)
}

// === Move tracking ===

func TestUseAfterMove(t *testing.T) {
	errs := ownerErrs(t, `
		test() {
			string s = "hi";
			string t = s;
			string u = s;
		}
	`)
	expectOwnerError(t, errs, "use of moved variable 's'")
}

func TestUseAfterMoveInCall(t *testing.T) {
	errs := ownerErrs(t, `
		consume(string s) {}
		test() {
			string s = "hi";
			consume(s);
			string t = s;
		}
	`)
	expectOwnerError(t, errs, "use of moved variable 's'")
}

func TestDoubleMove(t *testing.T) {
	errs := ownerErrs(t, `
		consume(string s) {}
		test() {
			string s = "hi";
			consume(s);
			consume(s);
		}
	`)
	expectOwnerError(t, errs, "use of moved variable 's'")
}

func TestMoveInConstructor(t *testing.T) {
	errs := ownerErrs(t, `
		type Dog {
			string name;
		}
		test() {
			string s = "Rex";
			Dog d = Dog(name: s);
			string t = s;
		}
	`)
	expectOwnerError(t, errs, "use of moved variable 's'")
}

func TestReturnMovesOK(t *testing.T) {
	ownerOK(t, `
		foo() string {
			string s = "hi";
			return s;
		}
	`)
}

// === Copy exemption ===

func TestIntIsCopy(t *testing.T) {
	ownerOK(t, `
		test() {
			int x = 42;
			int y = x;
			int z = x;
		}
	`)
}

func TestBoolIsCopy(t *testing.T) {
	ownerOK(t, `
		test() {
			bool b = true;
			bool c = b;
			bool d = b;
		}
	`)
}

func TestStringMoves(t *testing.T) {
	errs := ownerErrs(t, `
		test() {
			string s = "hi";
			string t = s;
			string u = s;
		}
	`)
	expectOwnerError(t, errs, "use of moved variable 's'")
}

// === Resurrection ===

func TestAssignResurrects(t *testing.T) {
	ownerOK(t, `
		test() {
			string s = "hi";
			string t = s;
			s = "world";
			string u = s;
		}
	`)
}

func TestAssignResurrectsAfterCall(t *testing.T) {
	ownerOK(t, `
		consume(string s) {}
		test() {
			string s = "hi";
			consume(s);
			s = "new";
			consume(s);
		}
	`)
}

// === Control flow ===

func TestMoveInIfBranch(t *testing.T) {
	// Conservative: moved in then-branch without else means possibly moved after.
	errs := ownerErrs(t, `
		consume(string s) {}
		test() {
			string s = "hi";
			bool b = true;
			if b {
				consume(s);
			}
			string t = s;
		}
	`)
	expectOwnerError(t, errs, "use of moved variable 's'")
}

func TestMoveInBothBranchesNoUse(t *testing.T) {
	// Moved in both branches, but no use after — should be OK.
	ownerOK(t, `
		consume(string s) {}
		test() {
			string s = "hi";
			bool b = true;
			if b {
				consume(s);
			} else {
				consume(s);
			}
		}
	`)
}

func TestMoveInLoopBody(t *testing.T) {
	// Conservative: moved in loop body means possibly moved after.
	errs := ownerErrs(t, `
		consume(string s) {}
		test() {
			string s = "hi";
			while true {
				consume(s);
				break;
			}
			string t = s;
		}
	`)
	expectOwnerError(t, errs, "use of moved variable 's'")
}

// === Borrow conflicts (unit tests — sema doesn't yet support implicit T → T&/T~ coercion) ===

func TestBorrowConflictDetection(t *testing.T) {
	// Directly test the borrow conflict checker with constructed types.
	c := &Checker{state: make(StateMap)}
	c.state["s"] = Owned

	// Build a signature: f(string ~a, string &b)
	sig := types.NewSignature(nil, []*types.Param{
		types.NewParam("a", types.TypString, types.RefMut),
		types.NewParam("b", types.TypString, types.RefShared),
	}, nil, false)

	// Build a CallExpr with two args both referencing "s"
	callExpr := &ast.CallExpr{}
	callExpr.Args = []*ast.Arg{
		{Value: &ast.IdentExpr{Name: "s"}},
		{Value: &ast.IdentExpr{Name: "s"}},
	}

	c.checkBorrowConflicts(callExpr, sig)

	if len(c.errors) != 1 {
		t.Fatalf("expected 1 borrow conflict error, got %d: %v", len(c.errors), c.errors)
	}
	if !strings.Contains(c.errors[0].Error(), "cannot borrow") {
		t.Errorf("expected 'cannot borrow' error, got: %v", c.errors[0])
	}
}

func TestBorrowConflictDoubleMut(t *testing.T) {
	c := &Checker{state: make(StateMap)}
	c.state["s"] = Owned

	sig := types.NewSignature(nil, []*types.Param{
		types.NewParam("a", types.TypString, types.RefMut),
		types.NewParam("b", types.TypString, types.RefMut),
	}, nil, false)

	callExpr := &ast.CallExpr{}
	callExpr.Args = []*ast.Arg{
		{Value: &ast.IdentExpr{Name: "s"}},
		{Value: &ast.IdentExpr{Name: "s"}},
	}

	c.checkBorrowConflicts(callExpr, sig)

	if len(c.errors) != 1 {
		t.Fatalf("expected 1 borrow conflict error, got %d: %v", len(c.errors), c.errors)
	}
	if !strings.Contains(c.errors[0].Error(), "cannot borrow") {
		t.Errorf("expected 'cannot borrow' error, got: %v", c.errors[0])
	}
}

func TestMultipleSharedNoBorrowConflict(t *testing.T) {
	c := &Checker{state: make(StateMap)}
	c.state["s"] = Owned

	sig := types.NewSignature(nil, []*types.Param{
		types.NewParam("a", types.TypString, types.RefShared),
		types.NewParam("b", types.TypString, types.RefShared),
	}, nil, false)

	callExpr := &ast.CallExpr{}
	callExpr.Args = []*ast.Arg{
		{Value: &ast.IdentExpr{Name: "s"}},
		{Value: &ast.IdentExpr{Name: "s"}},
	}

	c.checkBorrowConflicts(callExpr, sig)

	if len(c.errors) != 0 {
		t.Errorf("expected no borrow conflict errors, got: %v", c.errors)
	}
}

// === Unsafe pointer (unit tests — sema doesn't yet support pointer value construction) ===

func TestIsPointerTypeRef(t *testing.T) {
	ptr := &ast.PointerTypeRef{}
	if !isPointerTypeRef(ptr) {
		t.Error("expected PointerTypeRef to be detected as pointer")
	}
	named := &ast.NamedTypeRef{}
	if isPointerTypeRef(named) {
		t.Error("expected NamedTypeRef to NOT be detected as pointer")
	}
}

func TestPointerCheckOutsideUnsafe(t *testing.T) {
	// Directly test the pointer check in checkTypedVarDecl.
	c := &Checker{
		state:    make(StateMap),
		inUnsafe: 0,
		info:     &sema.Info{Types: make(map[ast.Expr]types.Type), Objects: make(map[*ast.IdentExpr]types.Object), Scopes: make(map[ast.Node]*types.Scope)},
	}

	decl := &ast.TypedVarDecl{
		Type:  &ast.PointerTypeRef{},
		Name:  "p",
		Value: &ast.IntLit{Raw: "0"},
	}
	c.checkTypedVarDecl(decl)

	if len(c.errors) != 1 {
		t.Fatalf("expected 1 pointer error, got %d: %v", len(c.errors), c.errors)
	}
	if !strings.Contains(c.errors[0].Error(), "raw pointer") {
		t.Errorf("expected 'raw pointer' error, got: %v", c.errors[0])
	}
}

func TestPointerCheckInsideUnsafe(t *testing.T) {
	// Same declaration but inside unsafe — no error.
	c := &Checker{
		state:    make(StateMap),
		inUnsafe: 1,
		info:     &sema.Info{Types: make(map[ast.Expr]types.Type), Objects: make(map[*ast.IdentExpr]types.Object), Scopes: make(map[ast.Node]*types.Scope)},
	}

	decl := &ast.TypedVarDecl{
		Type:  &ast.PointerTypeRef{},
		Name:  "p",
		Value: &ast.IntLit{Raw: "0"},
	}
	c.checkTypedVarDecl(decl)

	if len(c.errors) != 0 {
		t.Errorf("expected no errors inside unsafe, got: %v", c.errors)
	}
}

// === Member access after move ===

func TestUseAfterMoveViaMemberAccess(t *testing.T) {
	// Accessing a member on a moved variable should still trigger use-after-move.
	// Unit test because sema doesn't resolve .length on string.
	c := &Checker{
		state: make(StateMap),
		info:  &sema.Info{Types: make(map[ast.Expr]types.Type), Objects: make(map[*ast.IdentExpr]types.Object), Scopes: make(map[ast.Node]*types.Scope)},
	}

	// Register variable "s" as a string (non-copy) in both state and objects.
	ident1 := &ast.IdentExpr{Name: "s"}
	sVar := types.NewVar(types.Pos{}, "s", types.TypString)
	c.info.Objects[ident1] = sVar
	c.state["s"] = Owned

	// First, check and move "s" (simulating `string t = s;`).
	c.checkIdentUse(ident1)
	c.state["s"] = Moved

	// Now access s.length — the ident inside the MemberExpr should trigger error.
	ident2 := &ast.IdentExpr{Name: "s"}
	c.info.Objects[ident2] = sVar
	memberExpr := &ast.MemberExpr{Target: ident2, Field: "length"}
	c.checkExpr(memberExpr)

	if len(c.errors) != 1 {
		t.Fatalf("expected 1 error, got %d: %v", len(c.errors), c.errors)
	}
	if !strings.Contains(c.errors[0].Error(), "use of moved variable 's'") {
		t.Errorf("expected use-after-move error for 's', got: %v", c.errors[0])
	}
}

// === Borrow conflict ordering ===

func TestBorrowConflictSharedBeforeMut(t *testing.T) {
	// Verify that the conflict is detected even when shared comes before mutable.
	// This validates the fix for Issue 1 (the `other` variable bug).
	c := &Checker{state: make(StateMap)}
	c.state["s"] = Owned

	// Signature: f(string &a, string ~b) — shared first, then mutable.
	sig := types.NewSignature(nil, []*types.Param{
		types.NewParam("a", types.TypString, types.RefShared),
		types.NewParam("b", types.TypString, types.RefMut),
	}, nil, false)

	callExpr := &ast.CallExpr{}
	callExpr.Args = []*ast.Arg{
		{Value: &ast.IdentExpr{Name: "s"}},
		{Value: &ast.IdentExpr{Name: "s"}},
	}

	c.checkBorrowConflicts(callExpr, sig)

	if len(c.errors) != 1 {
		t.Fatalf("expected 1 borrow conflict error, got %d: %v", len(c.errors), c.errors)
	}
	if !strings.Contains(c.errors[0].Error(), "cannot borrow") {
		t.Errorf("expected 'cannot borrow' error, got: %v", c.errors[0])
	}
	// Verify the error message mentions the correct borrow kind for the other borrow.
	if !strings.Contains(c.errors[0].Error(), "shared") {
		t.Errorf("expected error to mention 'shared', got: %v", c.errors[0])
	}
}

// === isCopyType ===

func TestIsCopyType(t *testing.T) {
	copyTypes := []types.Type{
		types.TypInt, types.TypI8, types.TypI16, types.TypI32, types.TypI64,
		types.TypUint, types.TypU8, types.TypU16, types.TypU32, types.TypU64,
		types.TypF32, types.TypF64,
		types.TypBool, types.TypChar, types.TypNone, types.TypVoid,
		types.NewSharedRef(types.TypString),
		types.NewMutRef(types.TypString),
	}
	for _, typ := range copyTypes {
		if !isCopyType(typ) {
			t.Errorf("expected %s to be copy type", typ)
		}
	}

	moveTypes := []types.Type{
		types.TypString,
		nil,
	}
	for _, typ := range moveTypes {
		if isCopyType(typ) {
			t.Errorf("expected %v to NOT be copy type", typ)
		}
	}
}
