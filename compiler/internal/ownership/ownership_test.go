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

// === Inferred var decl (:=) ===

func TestInferredVarDeclMove(t *testing.T) {
	errs := ownerErrs(t, `
		consume(string s) {}
		test() {
			s := "hi";
			consume(s);
			consume(s);
		}
	`)
	expectOwnerError(t, errs, "use of moved variable 's'")
}

func TestInferredVarDeclCopy(t *testing.T) {
	ownerOK(t, `
		test() {
			x := 42;
			int y = x;
			int z = x;
		}
	`)
}

// === Destructure var decl ===

func TestDestructureVarDeclMove(t *testing.T) {
	errs := ownerErrs(t, `
		pair() (string, string) { return ("a", "b"); }
		test() {
			(a, b) := pair();
			string c = a;
			string d = a;
		}
	`)
	expectOwnerError(t, errs, "use of moved variable 'a'")
}

func TestDestructureVarDeclCopy(t *testing.T) {
	ownerOK(t, `
		pair() (int, int) { return (1, 2); }
		test() {
			(a, b) := pair();
			int c = a;
			int d = a;
		}
	`)
}

// === For-in loop ===

func TestForInMoveInsideBody(t *testing.T) {
	errs := ownerErrs(t, `
		consume(string s) {}
		test() {
			string s = "hi";
			for i in 0..3 {
				consume(s);
			}
			string t = s;
		}
	`)
	expectOwnerError(t, errs, "use of moved variable 's'")
}

func TestForInBindingOK(t *testing.T) {
	ownerOK(t, `
		test() {
			for i in 0..10 {
				int x = i;
			}
		}
	`)
}

// === Classic for loop ===

func TestClassicForMoveInBody(t *testing.T) {
	errs := ownerErrs(t, `
		consume(string s) {}
		test() {
			string s = "hi";
			for int i = 0; i < 3; i += 1 {
				consume(s);
			}
			string t = s;
		}
	`)
	expectOwnerError(t, errs, "use of moved variable 's'")
}

// === Infinite loop ===

func TestInfiniteLoopMove(t *testing.T) {
	errs := ownerErrs(t, `
		consume(string s) {}
		test() {
			string s = "hi";
			for {
				consume(s);
				break;
			}
			string t = s;
		}
	`)
	expectOwnerError(t, errs, "use of moved variable 's'")
}

// === Match expression ===

func TestMatchPatternBindingOK(t *testing.T) {
	ownerOK(t, `
		enum Option { Some(int value), None }
		test() {
			Option o = Option.Some(42);
			x := match o {
				Some(v) => v + 1,
				None => 0,
			};
		}
	`)
}

func TestMatchMoveInOneArm(t *testing.T) {
	errs := ownerErrs(t, `
		enum Color { Red, Green, Blue }
		consume(string s) {}
		test() {
			Color c = Color.Red;
			string s = "hi";
			int x = match c {
				Color.Red => { consume(s); 1; },
				Color.Green => 2,
				Color.Blue => 3,
			};
			string t = s;
		}
	`)
	expectOwnerError(t, errs, "use of moved variable 's'")
}

// === If-expression ===

func TestIfExprMoveInBranch(t *testing.T) {
	errs := ownerErrs(t, `
		consume(string s) {}
		test() {
			string s = "hi";
			bool b = true;
			int x = if b { consume(s); 1; } else { 2; };
			string t = s;
		}
	`)
	expectOwnerError(t, errs, "use of moved variable 's'")
}

// === Method body ===

func TestMethodBodyOwnership(t *testing.T) {
	ownerOK(t, `
		type Dog {
			string name;
			bark() string {
				return this.name;
			}
		}
	`)
}

func TestMethodBodyUseAfterMove(t *testing.T) {
	errs := ownerErrs(t, `
		type Dog {
			string name;
			test() {
				string s = "hi";
				string t = s;
				string u = s;
			}
		}
	`)
	expectOwnerError(t, errs, "use of moved variable 's'")
}

// === Lambda expression ===

func TestLambdaParamOwnership(t *testing.T) {
	ownerOK(t, `
		test() {
			f := |int x| -> x + 1;
		}
	`)
}

func TestLambdaDoesNotLeakMoveState(t *testing.T) {
	ownerOK(t, `
		test() {
			string s = "hi";
			f := |int x| -> x + 1;
			string t = s;
		}
	`)
}

// === Expression branches ===

func TestBinaryExprMovedOperand(t *testing.T) {
	errs := ownerErrs(t, `
		test() {
			string s = "hi";
			string t = s;
			string u = s + "world";
		}
	`)
	expectOwnerError(t, errs, "use of moved variable 's'")
}

func TestParenExprMovedInner(t *testing.T) {
	errs := ownerErrs(t, `
		test() {
			string s = "hi";
			string t = s;
			string u = (s);
		}
	`)
	expectOwnerError(t, errs, "use of moved variable 's'")
}

func TestTupleLitMoveElements(t *testing.T) {
	errs := ownerErrs(t, `
		test() {
			string s = "hi";
			x := (s, s);
		}
	`)
	expectOwnerError(t, errs, "use of moved variable 's'")
}

func TestArrayLitMoveElements(t *testing.T) {
	errs := ownerErrs(t, `
		test() {
			string a = "a";
			string b = "b";
			x := [a, b];
			string c = a;
		}
	`)
	expectOwnerError(t, errs, "use of moved variable 'a'")
}

func TestMapLitMoveValues(t *testing.T) {
	errs := ownerErrs(t, `
		test() {
			string v = "val";
			m := {"key": v};
			string x = v;
		}
	`)
	expectOwnerError(t, errs, "use of moved variable 'v'")
}

func TestIndexExprMovedTarget(t *testing.T) {
	errs := ownerErrs(t, `
		consume(int[] a) {}
		test() {
			int[] items = [1, 2, 3];
			consume(items);
			int x = items[0];
		}
	`)
	expectOwnerError(t, errs, "use of moved variable 'items'")
}

// === Assignment targets ===

func TestAssignTargetMemberExpr(t *testing.T) {
	errs := ownerErrs(t, `
		type Dog { string name; }
		test() {
			string s = "hi";
			string t = s;
			Dog d = Dog(name: "Rex");
			d.name = s;
		}
	`)
	expectOwnerError(t, errs, "use of moved variable 's'")
}

func TestCompoundAssignReadsTarget(t *testing.T) {
	// int is copy so no error — just exercises the compound assign code path.
	ownerOK(t, `
		test() {
			int x = 1;
			int y = 2;
			x += y;
		}
	`)
}

func TestWhileUnwrapStmt(t *testing.T) {
	// Unit test: construct WhileUnwrapStmt directly since sema syntax support is limited.
	c := &Checker{
		state: make(StateMap),
		info:  &sema.Info{Types: make(map[ast.Expr]types.Type), Objects: make(map[*ast.IdentExpr]types.Object), Scopes: make(map[ast.Node]*types.Scope)},
	}

	// Simulate: while val := expr { <use s> }
	// Register "s" as owned non-copy var.
	sIdent := &ast.IdentExpr{Name: "s"}
	sVar := types.NewVar(types.Pos{}, "s", types.TypString)
	c.info.Objects[sIdent] = sVar
	c.state["s"] = Owned

	// Build a WhileUnwrapStmt with binding "val" and body that uses "s".
	stmt := &ast.WhileUnwrapStmt{
		Binding: "val",
		Value:   &ast.IntLit{Raw: "1"},
		Body: &ast.Block{
			Stmts: []ast.Stmt{
				&ast.ExprStmt{Expr: sIdent},
			},
		},
	}
	c.checkWhileUnwrapStmt(stmt)

	// After the loop, "s" should still be usable (conservative merge with pre-loop),
	// but the binding "val" should be registered as Owned.
	if c.state["val"] != Owned {
		t.Errorf("expected binding 'val' to be Owned, got %v", c.state["val"])
	}
}

// === Copy meta integration ===

func TestUserCopyTypeNeverMoves(t *testing.T) {
	ownerOK(t, `
		type Point `+"`copy"+` {
			int x;
			int y;
		}
		test() {
			Point p = Point(x: 1, y: 2);
			Point q = p;
			Point r = p;
		}
	`)
}

func TestUserNonCopyTypeMoves(t *testing.T) {
	errs := ownerErrs(t, `
		type Dog {
			string name;
		}
		test() {
			Dog d = Dog(name: "Rex");
			Dog e = d;
			Dog f = d;
		}
	`)
	expectOwnerError(t, errs, "use of moved variable 'd'")
}

func TestUserCopyInCall(t *testing.T) {
	ownerOK(t, `
		type Pt `+"`copy"+` {
			int x;
		}
		take(Pt p) {}
		test() {
			Pt p = Pt(x: 1);
			take(p);
			take(p);
		}
	`)
}
