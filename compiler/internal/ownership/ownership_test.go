package ownership

import (
	"strings"
	"sync"
	"testing"

	"djabi.dev/go/promise_lang/internal/ast"
	"djabi.dev/go/promise_lang/internal/parser"
	"djabi.dev/go/promise_lang/internal/sema"
	"djabi.dev/go/promise_lang/internal/testutil"
	"djabi.dev/go/promise_lang/internal/types"
	antlr "github.com/antlr4-go/antlr/v4"
)

// --- Test helpers ---

var (
	ownerStdOnce  sync.Once
	ownerStdScope *types.Scope
)

func getOwnerStdScope() *types.Scope {
	ownerStdOnce.Do(func() {
		src := testutil.LoadStdFiles()
		input := antlr.NewInputStream(src)
		lexer := parser.NewPromiseLexer(input)
		lexer.RemoveErrorListeners()
		stream := antlr.NewCommonTokenStream(lexer, antlr.TokenDefaultChannel)
		p := parser.NewPromiseParser(stream)
		p.RemoveErrorListeners()
		tree := p.CompilationUnit()
		stdFile, buildErrs := ast.Build("std.pr", tree)
		if len(buildErrs) > 0 {
			panic("std AST build errors: " + buildErrs[0].Error())
		}
		stdInfo, _ := sema.CheckWithTarget(stdFile, nil, sema.HostTargetInfo())
		ownerStdScope = sema.ExportedScope(stdInfo, stdFile)
	})
	return ownerStdScope
}

// checkOwnership parses source, runs sema with the std module, then runs ownership analysis.
func checkOwnership(t *testing.T, src string) []error {
	t.Helper()

	// Parse user
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

	// Inject use std as _
	stdUse := &ast.UseDecl{Alias: "_", CatalogName: "std"}
	file.Uses = append([]*ast.UseDecl{stdUse}, file.Uses...)

	info, semaErrs := sema.CheckWithModules(file, map[string]*types.Scope{"std": getOwnerStdScope()})
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

	// Build a signature: f(string var a, string &b) — MutRef type is a mutable borrow
	sig := types.NewSignature(nil, []*types.Param{
		types.NewParam("a", types.NewMutRef(types.TypString), types.RefNone),
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

	// MutRef type params are mutable borrows (distinct from ~ move params)
	sig := types.NewSignature(nil, []*types.Param{
		types.NewParam("a", types.NewMutRef(types.TypString), types.RefNone),
		types.NewParam("b", types.NewMutRef(types.TypString), types.RefNone),
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

	// Signature: f(string &a, string var b) — shared first, then mutable borrow.
	sig := types.NewSignature(nil, []*types.Param{
		types.NewParam("a", types.TypString, types.RefShared),
		types.NewParam("b", types.NewMutRef(types.TypString), types.RefNone),
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

// ===== Stage 6b: Borrow Tracking =====

// === Call-scoped borrow expiry ===

func TestCallScopedBorrowExpires(t *testing.T) {
	// Passing a variable by shared borrow should not prevent subsequent moves.
	// The borrow expires at the statement boundary.
	ownerOK(t, `
		read(string &s) {}
		consume(string s) {}
		test() {
			string s = "a";
			read(s);
			consume(s);
		}
	`)
}

func TestSequentialMutBorrowsOK(t *testing.T) {
	// Each mutable borrow expires at statement boundary, so sequential calls are OK.
	ownerOK(t, `
		modify(string ~s) {}
		test() {
			string s = "a";
			modify(s);
			modify(s);
		}
	`)
}

func TestSequentialSharedThenMutOK(t *testing.T) {
	// Shared borrow expires before mutable borrow starts.
	ownerOK(t, `
		read(string &s) {}
		modify(string ~s) {}
		test() {
			string s = "a";
			read(s);
			modify(s);
		}
	`)
}

// === Cross-statement borrow conflicts (variable-scoped borrows) ===

func TestStoredBorrowBlocksMove(t *testing.T) {
	// When a function returns a ref type and the result is stored,
	// borrow is promoted to variable-scoped. Moving the origin is blocked
	// while the borrower is still alive (T0164: NLL narrows to last-use).
	errs := ownerErrs(t, `
		getRef(string &s) string& { return s; }
		consume(string s) {}
		test() {
			string s = "hello";
			string &r = getRef(s);
			consume(s);
			string &r2 = r;
		}
	`)
	expectOwnerError(t, errs, "cannot move 's' while it is borrowed")
}

func TestStoredBorrowBlocksMutBorrow(t *testing.T) {
	// Stored shared borrow blocks a subsequent mutable borrow while borrower is alive.
	errs := ownerErrs(t, `
		getRef(string &s) string& { return s; }
		modify(string ~s) {}
		test() {
			string s = "hello";
			string &r = getRef(s);
			modify(s);
			string &r2 = r;
		}
	`)
	expectOwnerError(t, errs, "cannot borrow 's' as mutable")
}

func TestStoredMutBorrowBlocksShared(t *testing.T) {
	// Stored mutable borrow blocks a subsequent shared borrow while borrower is alive.
	errs := ownerErrs(t, `
		getMut(string ~s) string~ { return s; }
		read(string &s) {}
		test() {
			string s = "hello";
			string ~r = getMut(s);
			read(s);
			string ~r2 = r;
		}
	`)
	expectOwnerError(t, errs, "cannot borrow 's' as shared")
}

// === Move-while-borrowed ===

func TestMoveWhileBorrowedAssign(t *testing.T) {
	// Assigning a borrowed variable to another variable is a move while borrower is alive.
	errs := ownerErrs(t, `
		getRef(string &s) string& { return s; }
		test() {
			string s = "hello";
			string &r = getRef(s);
			string t = s;
			string &r2 = r;
		}
	`)
	expectOwnerError(t, errs, "cannot move 's' while it is borrowed")
}

// === Assignment-while-borrowed ===

func TestAssignWhileBorrowed(t *testing.T) {
	// Cannot reassign a variable while it is borrowed by another variable (borrower alive).
	errs := ownerErrs(t, `
		getRef(string &s) string& { return s; }
		test() {
			string s = "hello";
			string &r = getRef(s);
			s = "world";
			string &r2 = r;
		}
	`)
	expectOwnerError(t, errs, "cannot assign to 's' while it is borrowed")
}

func TestBorrowerReassignExpiresBorrow(t *testing.T) {
	// When the borrower variable is reassigned, the old borrow expires.
	// However, if r is reassigned to a new borrow of s and r is still alive,
	// s is still borrowed (T0164: NLL narrows to last-use of borrower).
	errs := ownerErrs(t, `
		getRef(string &s) string& { return s; }
		consume(string s) {}
		test() {
			string s = "hello";
			string &r = getRef(s);
			r = getRef(s);
			consume(s);
			string &r2 = r;
		}
	`)
	// s is still borrowed through the new r (r is alive past consume)
	expectOwnerError(t, errs, "cannot move 's' while it is borrowed")
}

// === Return reference safety ===

func TestReturnRefToLocal(t *testing.T) {
	// Cannot return a reference to a local variable — would create a dangling reference.
	errs := ownerErrs(t, `
		bad() string& {
			string s = "hello";
			return s;
		}
	`)
	expectOwnerError(t, errs, "cannot return reference to local variable 's'")
}

func TestReturnRefToParam(t *testing.T) {
	// Returning a reference to a parameter is OK — the caller still owns it.
	ownerOK(t, `
		good(string &s) string& { return s; }
	`)
}

func TestReturnNonRefOK(t *testing.T) {
	// Returning a non-ref type local is fine (it's a move, not a dangling ref).
	ownerOK(t, `
		ok() string {
			string s = "hello";
			return s;
		}
	`)
}

// === Method receiver borrows ===

func TestMethodSharedReceiverCallScoped(t *testing.T) {
	// Calling a shared-receiver method creates a call-scoped borrow that expires.
	ownerOK(t, `
		type T {
			int x;
			read(&this) int { return this.x; }
		}
		consume(T t) {}
		test() {
			T t = T(x: 1);
			t.read();
			consume(t);
		}
	`)
}

func TestMethodMutReceiverCallScoped(t *testing.T) {
	// Calling a mut-receiver method creates a call-scoped borrow that expires.
	ownerOK(t, `
		type T {
			int x;
			mutate(~this) { this.x = 2; }
		}
		consume(T t) {}
		test() {
			T t = T(x: 1);
			t.mutate();
			consume(t);
		}
	`)
}

func TestMethodReceiverStoredBorrow(t *testing.T) {
	// Method returning a ref type creates a stored borrow on the receiver
	// that persists while the borrower is alive (T0164: NLL).
	errs := ownerErrs(t, `
		type T {
			int x;
			getRef(&this) int& { return this.x; }
		}
		consume(T t) {}
		test() {
			T t = T(x: 1);
			int &r = t.getRef();
			consume(t);
			int &r2 = r;
		}
	`)
	expectOwnerError(t, errs, "cannot move 't' while it is borrowed")
}

// === Control flow and borrows ===

func TestBorrowInIfBranch(t *testing.T) {
	// Stored borrow created in then-branch persists while borrower is alive.
	errs := ownerErrs(t, `
		getRef(string &s) string& { return s; }
		consume(string s) {}
		test() {
			string s = "hello";
			bool b = true;
			string &r = "";
			if b {
				r = getRef(s);
			}
			consume(s);
			string &r2 = r;
		}
	`)
	expectOwnerError(t, errs, "cannot move 's' while it is borrowed")
}

func TestBorrowInLoop(t *testing.T) {
	// Stored borrow created in loop body persists while borrower is alive.
	errs := ownerErrs(t, `
		getRef(string &s) string& { return s; }
		consume(string s) {}
		test() {
			string s = "hello";
			string &r = "";
			while true {
				r = getRef(s);
				break;
			}
			consume(s);
			string &r2 = r;
		}
	`)
	expectOwnerError(t, errs, "cannot move 's' while it is borrowed")
}

func TestBorrowInBothBranches(t *testing.T) {
	// Stored borrow in both branches persists while borrower is alive.
	errs := ownerErrs(t, `
		getRef(string &s) string& { return s; }
		consume(string s) {}
		test() {
			string s = "hello";
			bool b = true;
			string &r = "";
			if b {
				r = getRef(s);
			} else {
				r = getRef(s);
			}
			consume(s);
			string &r2 = r;
		}
	`)
	expectOwnerError(t, errs, "cannot move 's' while it is borrowed")
}

// === Copy types and borrows ===

func TestCopyTypeNoBorrowTracking(t *testing.T) {
	// Copy types don't need borrow tracking — borrows of copy types are allowed freely.
	ownerOK(t, `
		read(int &x) {}
		test() {
			int x = 1;
			read(x);
			int y = x;
		}
	`)
}

func TestBorrowDoesNotMoveValue(t *testing.T) {
	// Passing by borrow does NOT consume the value — the variable can still be used.
	ownerOK(t, `
		read(string &s) {}
		consume(string s) {}
		test() {
			string s = "hello";
			read(s);
			read(s);
			consume(s);
		}
	`)
}

// === Borrow parameter does not move ===

func TestBorrowParamMultipleCalls(t *testing.T) {
	// Multiple shared borrow calls on same variable should work (borrows expire).
	ownerOK(t, `
		read(string &s) {}
		test() {
			string s = "hello";
			read(s);
			read(s);
			read(s);
		}
	`)
}

func TestMutBorrowParamDoesNotMove(t *testing.T) {
	// Passing by mutable borrow does NOT consume the value.
	ownerOK(t, `
		modify(string ~s) {}
		consume(string s) {}
		test() {
			string s = "hello";
			modify(s);
			consume(s);
		}
	`)
}

// === Cross-statement borrow: inferred var decl ===

func TestStoredBorrowInferredVarDecl(t *testing.T) {
	// Borrow promotion works with inferred var decls; persists while borrower alive.
	errs := ownerErrs(t, `
		getRef(string &s) string& { return s; }
		consume(string s) {}
		test() {
			string s = "hello";
			r := getRef(s);
			consume(s);
			string &r2 = r;
		}
	`)
	expectOwnerError(t, errs, "cannot move 's' while it is borrowed")
}

// ===== Coverage: unit tests for uncovered branches =====

// newUnitChecker creates a Checker suitable for unit tests.
func newUnitChecker() *Checker {
	return &Checker{
		state: make(StateMap),
		info: &sema.Info{
			Types:   make(map[ast.Expr]types.Type),
			Objects: make(map[*ast.IdentExpr]types.Object),
			Scopes:  make(map[ast.Node]*types.Scope),
		},
	}
}

// movedIdent registers a string variable as Moved and returns its IdentExpr.
func movedIdent(c *Checker, name string) *ast.IdentExpr {
	ident := &ast.IdentExpr{Name: name}
	c.info.Objects[ident] = types.NewVar(types.Pos{}, name, types.TypString)
	c.state[name] = Moved
	return ident
}

// ownedIdent registers a string variable as Owned and returns its IdentExpr.
func ownedIdent(c *Checker, name string) *ast.IdentExpr {
	ident := &ast.IdentExpr{Name: name}
	c.info.Objects[ident] = types.NewVar(types.Pos{}, name, types.TypString)
	c.state[name] = Owned
	return ident
}

// --- BorrowSet ---

func TestActiveBorrowsOf(t *testing.T) {
	bs := NewBorrowSet()
	bs.Add(&Borrow{Origin: "s", Kind: BorrowShared, Borrower: "r"})
	bs.Add(&Borrow{Origin: "t", Kind: BorrowMut})
	bs.Add(&Borrow{Origin: "s", Kind: BorrowMut, Borrower: "q"})

	if got := len(bs.ActiveBorrowsOf("s")); got != 2 {
		t.Errorf("expected 2 borrows of 's', got %d", got)
	}
	if got := len(bs.ActiveBorrowsOf("t")); got != 1 {
		t.Errorf("expected 1 borrow of 't', got %d", got)
	}
	if got := len(bs.ActiveBorrowsOf("x")); got != 0 {
		t.Errorf("expected 0 borrows of 'x', got %d", got)
	}
}

// --- checkExpr: expression branches ---

func TestThisExprMoved(t *testing.T) {
	c := newUnitChecker()
	c.state["this"] = Moved
	c.checkExpr(&ast.ThisExpr{})
	expectOwnerError(t, c.errors, "use of moved variable 'this'")
}

func TestOptionalChainOnMovedVar(t *testing.T) {
	c := newUnitChecker()
	ident := movedIdent(c, "s")
	c.checkExpr(&ast.OptionalChainExpr{Target: ident, Field: "length"})
	expectOwnerError(t, c.errors, "use of moved variable 's'")
}

func TestIsExprOnMovedVar(t *testing.T) {
	c := newUnitChecker()
	ident := movedIdent(c, "s")
	c.checkExpr(&ast.IsExpr{Expr: ident})
	expectOwnerError(t, c.errors, "use of moved variable 's'")
}

func TestCastExprOnMovedVar(t *testing.T) {
	c := newUnitChecker()
	ident := movedIdent(c, "s")
	c.checkExpr(&ast.CastExpr{Expr: ident})
	expectOwnerError(t, c.errors, "use of moved variable 's'")
}

func TestErrorPropagateOnMovedVar(t *testing.T) {
	c := newUnitChecker()
	ident := movedIdent(c, "s")
	c.checkExpr(&ast.ErrorPropagateExpr{Expr: ident})
	expectOwnerError(t, c.errors, "use of moved variable 's'")
}

func TestErrorPanicOnMovedVar(t *testing.T) {
	c := newUnitChecker()
	ident := movedIdent(c, "s")
	c.checkExpr(&ast.ErrorPanicExpr{Expr: ident})
	expectOwnerError(t, c.errors, "use of moved variable 's'")
}

func TestOptionalUnwrapOnMovedVar(t *testing.T) {
	c := newUnitChecker()
	ident := movedIdent(c, "s")
	c.checkExpr(&ast.OptionalUnwrapExpr{Expr: ident})
	expectOwnerError(t, c.errors, "use of moved variable 's'")
}

func TestErrorHandlerBindingOwnership(t *testing.T) {
	c := newUnitChecker()
	c.checkExpr(&ast.ErrorHandlerExpr{
		Expr:    &ast.IntLit{Raw: "1"},
		Binding: "err",
		Body:    &ast.Block{},
	})
	if c.state["err"] != Owned {
		t.Errorf("expected 'err' to be Owned, got %v", c.state["err"])
	}
}

func TestGoExprBlockMoveTracking(t *testing.T) {
	c := newUnitChecker()
	ident := movedIdent(c, "s")
	c.checkExpr(&ast.GoExpr{
		Block: &ast.Block{
			Stmts: []ast.Stmt{&ast.ExprStmt{Expr: ident}},
		},
	})
	expectOwnerError(t, c.errors, "use of moved variable 's'")
}

func TestGoExprExprMoveTracking(t *testing.T) {
	c := newUnitChecker()
	ident := movedIdent(c, "s")
	c.checkExpr(&ast.GoExpr{Expr: ident})
	expectOwnerError(t, c.errors, "use of moved variable 's'")
}

func TestUnsafeExprMoveTracking(t *testing.T) {
	c := newUnitChecker()
	ident := movedIdent(c, "s")
	c.checkExpr(&ast.UnsafeExpr{
		Body: &ast.Block{
			Stmts: []ast.Stmt{&ast.ExprStmt{Expr: ident}},
		},
	})
	expectOwnerError(t, c.errors, "use of moved variable 's'")
}

// --- checkStmt: statement branches ---

func TestRaiseStmtMoveTracking(t *testing.T) {
	c := newUnitChecker()
	ident := ownedIdent(c, "s")
	c.checkStmt(&ast.RaiseStmt{Value: ident})
	if c.state["s"] != Moved {
		t.Errorf("expected 's' to be Moved after raise, got %v", c.state["s"])
	}
}

func TestYieldStmtMoveTracking(t *testing.T) {
	c := newUnitChecker()
	ident := ownedIdent(c, "s")
	c.checkStmt(&ast.YieldStmt{Value: ident})
	if c.state["s"] != Moved {
		t.Errorf("expected 's' to be Moved after yield, got %v", c.state["s"])
	}
}

func TestYieldDelegateStmtMoveTracking(t *testing.T) {
	c := newUnitChecker()
	ident := ownedIdent(c, "s")
	c.checkStmt(&ast.YieldDelegateStmt{Value: ident})
	if c.state["s"] != Moved {
		t.Errorf("expected 's' to be Moved after yield*, got %v", c.state["s"])
	}
}

func TestNestedBlockStmt(t *testing.T) {
	c := newUnitChecker()
	ident := ownedIdent(c, "s")
	c.checkStmt(&ast.Block{
		Stmts: []ast.Stmt{
			&ast.InferredVarDecl{Name: "t", Value: ident},
		},
	})
	if c.state["s"] != Moved {
		t.Errorf("expected 's' to be Moved after nested block, got %v", c.state["s"])
	}
}

// --- registerPatternBindings ---

func TestEnumDestructurePatternBindings(t *testing.T) {
	c := newUnitChecker()
	c.registerPatternBindings(&ast.EnumDestructureMatchPattern{
		Enum:     "Color",
		Variant:  "Custom",
		Bindings: []string{"r", "g", "_"},
	})
	if c.state["r"] != Owned {
		t.Errorf("expected 'r' to be Owned")
	}
	if c.state["g"] != Owned {
		t.Errorf("expected 'g' to be Owned")
	}
	if _, exists := c.state["_"]; exists {
		t.Error("'_' should not be registered in state")
	}
}

func TestTypeBindingPatternBindings(t *testing.T) {
	c := newUnitChecker()
	c.registerPatternBindings(&ast.TypeBindingMatchPattern{
		TypeName: "Circle",
		Binding:  "c",
	})
	if c.state["c"] != Owned {
		t.Errorf("expected 'c' to be Owned")
	}
}

// --- checkAssignTarget: IndexExpr branch ---

func TestAssignTargetIndexExpr(t *testing.T) {
	c := newUnitChecker()
	target := movedIdent(c, "arr")
	index := movedIdent(c, "idx")
	c.checkAssignTarget(&ast.IndexExpr{Target: target, Index: index})
	if len(c.errors) != 2 {
		t.Fatalf("expected 2 use-after-move errors, got %d: %v", len(c.errors), c.errors)
	}
}

// --- checkForInStmt: index binding ---

func TestForInIndexBinding(t *testing.T) {
	c := newUnitChecker()
	c.checkStmt(&ast.ForInStmt{
		Binding:  "v",
		Index:    "i",
		Iterable: &ast.IntLit{Raw: "0"},
		Body:     &ast.Block{},
	})
	if c.state["v"] != Owned {
		t.Errorf("expected 'v' to be Owned")
	}
	if c.state["i"] != Owned {
		t.Errorf("expected 'i' to be Owned")
	}
}

// --- Stage 8m: use Bindings ---

func TestUseVarCannotBeMoved(t *testing.T) {
	errs := ownerErrs(t, `
		type Resource {
			close() {}
		}
		consume(Resource r) {}
		test() {
			use r := Resource();
			consume(r);
		}
	`)
	expectOwnerError(t, errs, "cannot move use-bound variable 'r'")
}

// --- Getter/Setter same name ---

func TestOwnershipGetterSetterSameName(t *testing.T) {
	// Ownership checker must resolve getter and setter bodies independently.
	ownerOK(t, `
		type Box {
			string _inner;
			get inner string { return this._inner; }
			set inner(string v) { this._inner = v; }
		}
		test(Box b, string v) {
			b.inner = v;
		}
	`)
}

// --- Droppable variable ownership ---

func TestDroppableVariableMove(t *testing.T) {
	ownerOK(t, `
		type Resource {
			int id;
			drop(~this) { }
		}
		consume(Resource r) { }
		test() {
			r := Resource(id: 1);
			consume(r);
		}
	`)
}

func TestDroppableVariableUseAfterMove(t *testing.T) {
	errs := ownerErrs(t, `
		type Resource {
			int id;
			drop(~this) { }
		}
		consume(Resource r) { }
		test() {
			r := Resource(id: 1);
			consume(r);
			int x = r.id;
		}
	`)
	expectOwnerError(t, errs, "use of moved variable 'r'")
}

func TestDroppableConditionalMoveUseAfter(t *testing.T) {
	// Moving in one branch makes it "maybe moved" — use after is an error
	errs := ownerErrs(t, `
		type Resource {
			int id;
			drop(~this) { }
		}
		consume(Resource r) { }
		test(bool cond) {
			r := Resource(id: 1);
			if cond {
				consume(r);
			}
			int x = r.id;
		}
	`)
	expectOwnerError(t, errs, "use of moved variable 'r'")
}

func TestDroppableConditionalMoveBothBranchesOK(t *testing.T) {
	// Moving in both branches is fine — no use after the if/else
	ownerOK(t, `
		type Resource {
			int id;
			drop(~this) { }
		}
		consume(Resource r) { }
		other(Resource r) { }
		test(bool cond) {
			r := Resource(id: 1);
			if cond {
				consume(r);
			} else {
				other(r);
			}
		}
	`)
}

func TestDroppableMoveToAssignment(t *testing.T) {
	// Moving via assignment is valid
	ownerOK(t, `
		type Resource {
			int id;
			drop(~this) { }
		}
		test() {
			Resource a = Resource(id: 1);
			Resource b = a;
			int x = b.id;
		}
	`)
}

func TestDroppableMoveToAssignmentUseAfter(t *testing.T) {
	// Use after move via assignment
	errs := ownerErrs(t, `
		type Resource {
			int id;
			drop(~this) { }
		}
		test() {
			Resource a = Resource(id: 1);
			Resource b = a;
			int x = a.id;
		}
	`)
	expectOwnerError(t, errs, "use of moved variable 'a'")
}

func TestDroppableMoveToMethodArg(t *testing.T) {
	// Moving to a method argument is valid
	ownerOK(t, `
		type Resource {
			int id;
			drop(~this) { }
		}
		type Container {
			int id;
			take(Resource r) { }
		}
		test() {
			c := Container(id: 0);
			r := Resource(id: 1);
			c.take(r);
		}
	`)
}

func TestDroppableMoveToMethodArgUseAfter(t *testing.T) {
	errs := ownerErrs(t, `
		type Resource {
			int id;
			drop(~this) { }
		}
		type Container {
			int id;
			take(Resource r) { }
		}
		test() {
			c := Container(id: 0);
			r := Resource(id: 1);
			c.take(r);
			int x = r.id;
		}
	`)
	expectOwnerError(t, errs, "use of moved variable 'r'")
}

func TestDroppableMoveToConstructorField(t *testing.T) {
	// Moving into constructor is valid
	ownerOK(t, `
		type Inner {
			int id;
			drop(~this) { }
		}
		type Outer {
			Inner inner;
		}
		test() {
			r := Inner(id: 1);
			o := Outer(inner: r);
			int x = o.inner.id;
		}
	`)
}

func TestDroppableMoveToConstructorFieldUseAfter(t *testing.T) {
	errs := ownerErrs(t, `
		type Inner {
			int id;
			drop(~this) { }
		}
		type Outer {
			Inner inner;
		}
		test() {
			r := Inner(id: 1);
			o := Outer(inner: r);
			int x = r.id;
		}
	`)
	expectOwnerError(t, errs, "use of moved variable 'r'")
}

func TestDroppableReturnMove(t *testing.T) {
	// Returning a droppable variable is a valid move
	ownerOK(t, `
		type Resource {
			int id;
			drop(~this) { }
		}
		make() Resource {
			r := Resource(id: 1);
			return r;
		}
	`)
}

func TestDroppableReturnMoveUseAfter(t *testing.T) {
	errs := ownerErrs(t, `
		type Resource {
			int id;
			drop(~this) { }
		}
		consume(Resource r) { }
		test() Resource {
			r := Resource(id: 1);
			consume(r);
			return r;
		}
	`)
	expectOwnerError(t, errs, "use of moved variable 'r'")
}

func TestDroppableMoveToMemberAssign(t *testing.T) {
	// Moving via member assignment is valid
	ownerOK(t, `
		type Inner {
			int id;
			drop(~this) { }
		}
		type Outer {
			Inner inner;
		}
		test() {
			o := Outer(inner: Inner(id: 0));
			r := Inner(id: 1);
			o.inner = r;
		}
	`)
}

func TestDroppableMoveToMemberAssignUseAfter(t *testing.T) {
	errs := ownerErrs(t, `
		type Inner {
			int id;
			drop(~this) { }
		}
		type Outer {
			Inner inner;
		}
		test() {
			o := Outer(inner: Inner(id: 0));
			r := Inner(id: 1);
			o.inner = r;
			int x = r.id;
		}
	`)
	expectOwnerError(t, errs, "use of moved variable 'r'")
}

func TestDroppableNoMoveNoError(t *testing.T) {
	// Variable never moved — just used normally, then dropped at scope exit
	ownerOK(t, `
		type Resource {
			int id;
			drop(~this) { }
		}
		test() {
			r := Resource(id: 1);
			int x = r.id;
			int y = r.id;
		}
	`)
}

func TestDroppableMultipleVarsIndependentMoves(t *testing.T) {
	// Multiple droppable vars moved independently
	ownerOK(t, `
		type Resource {
			int id;
			drop(~this) { }
		}
		consume(Resource r) { }
		test() {
			a := Resource(id: 1);
			b := Resource(id: 2);
			consume(a);
			consume(b);
		}
	`)
}

func TestDroppableReassignmentResurrects(t *testing.T) {
	// After moving, reassigning brings the variable back to Owned
	ownerOK(t, `
		type Resource {
			int id;
			drop(~this) { }
		}
		consume(Resource r) { }
		test() {
			r := Resource(id: 1);
			consume(r);
			r = Resource(id: 2);
			int x = r.id;
		}
	`)
}

// checkAssignTarget: index expression target checks sub-expressions
func TestAssignTargetIndexSubExpressions(t *testing.T) {
	// arr[i] = val — should check arr is not moved
	errs := ownerErrs(t, `
		type Resource {
			int id;
			drop(~this) { }
		}
		test() {
			arr := [1, 2, 3];
			consume_arr(arr);
			arr[0] = 5;
		}
		consume_arr(int[] a) { }
	`)
	expectOwnerError(t, errs, "use of moved variable 'arr'")
}

// checkAssignTarget: member expression target checks sub-expressions
func TestAssignTargetMemberSubExpressions(t *testing.T) {
	errs := ownerErrs(t, `
		type Box {
			int val;
		}
		consume(Box b) { }
		test() {
			b := Box(val: 1);
			consume(b);
			b.val = 5;
		}
	`)
	expectOwnerError(t, errs, "use of moved variable 'b'")
}

// checkAssignTarget: slice expression target
func TestAssignTargetSliceSubExpressions(t *testing.T) {
	errs := ownerErrs(t, `
		test() {
			arr := [1, 2, 3];
			consume_arr(arr);
			arr[0:2] = [5, 6];
		}
		consume_arr(int[] a) { }
	`)
	expectOwnerError(t, errs, "use of moved variable 'arr'")
}

// checkAssignTarget: index expression checks both target AND index
func TestAssignTargetIndexExprChecksIndex(t *testing.T) {
	// The index sub-expression itself uses a moved variable
	errs := ownerErrs(t, `
		type Resource {
			int id;
			drop(~this) { }
		}
		consume(Resource r) { }
		test() {
			r := Resource(id: 0);
			consume(r);
			arr := [1, 2, 3];
			arr[r.id] = 5;
		}
	`)
	expectOwnerError(t, errs, "use of moved variable 'r'")
}

// Move to index assignment
func TestDroppableMoveToIndexAssign(t *testing.T) {
	ownerOK(t, `
		type Resource {
			int id;
			drop(~this) { }
		}
		test() {
			arr := [Resource(id: 0)];
			r := Resource(id: 1);
			arr[0] = r;
		}
	`)
}

// Use after move to index assignment
func TestDroppableMoveToIndexAssignUseAfter(t *testing.T) {
	errs := ownerErrs(t, `
		type Resource {
			int id;
			drop(~this) { }
		}
		test() {
			arr := [Resource(id: 0)];
			r := Resource(id: 1);
			arr[0] = r;
			int x = r.id;
		}
	`)
	expectOwnerError(t, errs, "use of moved variable 'r'")
}

// === Lambda capture ownership ===

func TestMoveCaptureMarksVariableMoved(t *testing.T) {
	errs := ownerErrs(t, `
		type Foo { int x; drop(~this) {} }
		test() {
			f := Foo(x: 1);
			g := move |int y| -> f.x + y;
			int z = f.x;
		}
	`)
	expectOwnerError(t, errs, "use of moved variable 'f'")
}

func TestCopyCaptureDoesNotMoveVariable(t *testing.T) {
	ownerOK(t, `
		test() {
			int x = 42;
			f := |int y| -> x + y;
			int z = x + 1;
		}
	`)
}

// === Variadic Parameters ===

func TestVariadicBasicOwnership(t *testing.T) {
	// Basic variadic with copy types — no ownership issues.
	ownerOK(t, `
		sum(...int nums) int {
			int total = 0;
			for n in nums { total += n; }
			return total;
		}
		test() {
			sum(1, 2, 3);
		}
	`)
}

func TestVariadicPassVectorOwnership(t *testing.T) {
	// Passing a vector directly to variadic — vector is used after call.
	ownerOK(t, `
		sum(...int nums) int {
			int total = 0;
			for n in nums { total += n; }
			return total;
		}
		test() {
			int[] v = [1, 2, 3];
			sum(v);
		}
	`)
}

func TestVariadicEmptyCallOwnership(t *testing.T) {
	// Empty variadic call should not cause ownership issues.
	ownerOK(t, `
		process(...string items) {}
		test() {
			process();
		}
	`)
}

func TestVariadicWithFixedParamsOwnership(t *testing.T) {
	// Mixed fixed + variadic, all copy types.
	ownerOK(t, `
		mylog(string level, ...string msgs) {}
		test() {
			mylog("info", "a", "b", "c");
		}
	`)
}

func TestVariadicNestedCallOwnership(t *testing.T) {
	// Variadic function passing its param to another variadic.
	ownerOK(t, `
		sum(...int nums) int {
			int total = 0;
			for n in nums { total += n; }
			return total;
		}
		doubleSum(...int nums) int {
			return sum(nums) * 2;
		}
		test() {
			doubleSum(1, 2, 3);
		}
	`)
}

// === While-unwrap borrow conflict (B0004) ===

func TestWhileUnwrapBodyCanReBorrow(t *testing.T) {
	// B0004: while-unwrap condition borrows obj, body must be able to re-borrow it.
	ownerOK(t, `
		type Decoder {
			int pos;
			next_key(&this) string? { return none; }
			decode_string(&this) string { return ""; }
		}
		test() {
			Decoder dec = Decoder(pos: 0);
			while key := dec.next_key() {
				dec.decode_string();
			}
		}
	`)
}

func TestWhileUnwrapBodyCanMutBorrow(t *testing.T) {
	// B0004 variant: condition shared-borrows, body mut-borrows.
	ownerOK(t, `
		type Iter {
			int pos;
			peek(&this) int? { return none; }
			advance(~this) { this.pos += 1; }
		}
		test() {
			Iter it = Iter(pos: 0);
			while val := it.peek() {
				it.advance();
			}
		}
	`)
}

func TestWhileCondBodyCanReBorrow(t *testing.T) {
	// Same fix for regular while: condition borrows, body re-borrows.
	ownerOK(t, `
		type Stream {
			int pos;
			has_more(&this) bool { return false; }
			read(&this) int { return 0; }
		}
		test() {
			Stream s = Stream(pos: 0);
			while s.has_more() {
				s.read();
			}
		}
	`)
}

func TestIfUnwrapBodyCanReBorrow(t *testing.T) {
	// Same fix for if-unwrap: init expression borrows, body re-borrows.
	ownerOK(t, `
		type Parser {
			int pos;
			try_parse(&this) string? { return none; }
			consume(&this) string { return ""; }
		}
		test() {
			Parser p = Parser(pos: 0);
			if val := p.try_parse() {
				p.consume();
			}
		}
	`)
}

func TestForInBodyCanReBorrow(t *testing.T) {
	// for-in iterable expression borrows, body re-borrows.
	ownerOK(t, `
		type DataSource {
			int[] items;
			get_items(&this) int[] { return this.items; }
			log(&this) {}
		}
		test() {
			DataSource ds = DataSource(items: [1, 2, 3]);
			for item in ds.get_items() {
				ds.log();
			}
		}
	`)
}

func TestClassicForCondBodyCanReBorrow(t *testing.T) {
	// Classic for condition borrows, body re-borrows.
	ownerOK(t, `
		type Cursor {
			int pos;
			has_next(&this) bool { return this.pos < 10; }
			read(&this) int { return this.pos; }
		}
		test() {
			Cursor cur = Cursor(pos: 0);
			for i := 0; cur.has_next(); i += 1 {
				cur.read();
				break;
			}
		}
	`)
}

// --- Additional positive coverage ---

func TestIfCondBodyCanReBorrow(t *testing.T) {
	// Non-unwrap if: condition method call borrows, body re-borrows.
	ownerOK(t, `
		type Guard {
			int level;
			is_ready(&this) bool { return this.level > 0; }
			activate(~this) { this.level = 0; }
		}
		test() {
			Guard g = Guard(level: 1);
			if g.is_ready() {
				g.activate();
			}
		}
	`)
}

func TestIfUnwrapElseCanReBorrow(t *testing.T) {
	// If-unwrap: both then and else branches can re-borrow.
	ownerOK(t, `
		type Source {
			int pos;
			try_get(&this) string? { return none; }
			fallback(&this) string { return ""; }
			reset(~this) { this.pos = 0; }
		}
		test() {
			Source s = Source(pos: 0);
			if val := s.try_get() {
				s.fallback();
			} else {
				s.reset();
			}
		}
	`)
}

func TestWhileUnwrapBindingAndReBorrow(t *testing.T) {
	// While-unwrap: body uses both the binding and re-borrows the object.
	ownerOK(t, `
		type Queue {
			int count;
			dequeue(&this) int? { return none; }
			size(&this) int { return this.count; }
		}
		test() {
			Queue q = Queue(count: 0);
			int total = 0;
			while item := q.dequeue() {
				total += item;
				int remaining = q.size();
			}
		}
	`)
}

func TestCondMultipleCallsSameObject(t *testing.T) {
	// Condition with multiple method calls on same object.
	ownerOK(t, `
		type Validator {
			int x;
			check_a(&this) bool { return true; }
			check_b(&this) bool { return true; }
			run(~this) {}
		}
		test() {
			Validator v = Validator(x: 0);
			if v.check_a() {
				v.run();
			}
		}
	`)
}

func TestClassicForInitBorrowDoesNotLeakToBody(t *testing.T) {
	// Classic for: init expression borrows, body can still borrow.
	ownerOK(t, `
		type Config {
			int max;
			get_max(&this) int { return this.max; }
			process(~this) {}
		}
		test() {
			Config cfg = Config(max: 10);
			for i := cfg.get_max(); i > 0; i -= 1 {
				cfg.process();
				break;
			}
		}
	`)
}

// --- Negative tests: variable-scoped borrows must still be caught ---

func TestStoredBorrowStillBlocksInWhileBody(t *testing.T) {
	// A stored borrow blocks conflicting borrows inside a loop body
	// while the borrower is alive (T0164: NLL narrows to last-use).
	errs := ownerErrs(t, `
		getRef(string &s) string& { return s; }
		mutate(string ~s) {}
		test() {
			string s = "hello";
			string &r = getRef(s);
			while true {
				mutate(s);
				break;
			}
			string &r2 = r;
		}
	`)
	expectOwnerError(t, errs, "cannot borrow 's' as mutable")
}

func TestStoredBorrowStillBlocksInWhileUnwrapBody(t *testing.T) {
	// Variable-scoped borrow persists into while-unwrap body while borrower alive.
	errs := ownerErrs(t, `
		getRef(string &s) string& { return s; }
		consume(string s) {}
		test() {
			string s = "hello";
			string &r = getRef(s);
			int[] nums = [1];
			while v := nums.pop() {
				consume(s);
			}
			string &r2 = r;
		}
	`)
	expectOwnerError(t, errs, "cannot move 's' while it is borrowed")
}

func TestStoredBorrowCreatedInLoopPersists(t *testing.T) {
	// A stored borrow created in a while-unwrap body persists while borrower alive.
	errs := ownerErrs(t, `
		getRef(string &s) string& { return s; }
		consume(string s) {}
		test() {
			string s = "hello";
			string &r = "";
			int[] nums = [1];
			while v := nums.pop() {
				r = getRef(s);
			}
			consume(s);
			string &r2 = r;
		}
	`)
	expectOwnerError(t, errs, "cannot move 's' while it is borrowed")
}

func TestStoredBorrowStillBlocksInIfBody(t *testing.T) {
	// Variable-scoped borrow persists into if body while borrower alive.
	errs := ownerErrs(t, `
		getRef(string &s) string& { return s; }
		mutate(string ~s) {}
		test() {
			string s = "hello";
			string &r = getRef(s);
			if true {
				mutate(s);
			}
			string &r2 = r;
		}
	`)
	expectOwnerError(t, errs, "cannot borrow 's' as mutable")
}

func TestStoredBorrowStillBlocksInForInBody(t *testing.T) {
	// Variable-scoped borrow persists into for-in body while borrower alive.
	errs := ownerErrs(t, `
		getRef(string &s) string& { return s; }
		consume(string s) {}
		test() {
			string s = "hello";
			string &r = getRef(s);
			int[] items = [1, 2];
			for item in items {
				consume(s);
			}
			string &r2 = r;
		}
	`)
	expectOwnerError(t, errs, "cannot move 's' while it is borrowed")
}

func TestStoredBorrowStillBlocksInClassicForBody(t *testing.T) {
	// Variable-scoped borrow persists into classic for body while borrower alive.
	errs := ownerErrs(t, `
		getRef(string &s) string& { return s; }
		consume(string s) {}
		test() {
			string s = "hello";
			string &r = getRef(s);
			for i := 0; i < 1; i += 1 {
				consume(s);
			}
			string &r2 = r;
		}
	`)
	expectOwnerError(t, errs, "cannot move 's' while it is borrowed")
}

// === Drop ordering (B0036) ===

func TestDropOrderSafeBorrowDeclaredAfterOrigin(t *testing.T) {
	// Borrower declared after origin — safe LIFO order.
	// Origin is dropped last (declared first), borrower dropped first.
	ownerOK(t, `
		getRef(string &s) string& { return s; }
		test() {
			string s = "hello";
			string &r = getRef(s);
		}
	`)
}

func TestDropOrderSafeDroppableVariables(t *testing.T) {
	// Multiple droppable variables — declared in order, dropped in LIFO.
	ownerOK(t, `
		type Resource {
			int id;
			drop(~this) { }
		}
		test() {
			a := Resource(id: 1);
			b := Resource(id: 2);
			c := Resource(id: 3);
		}
	`)
}

func TestDropOrderSafeDroppableAndBorrowCoexist(t *testing.T) {
	// A droppable variable and a borrow in the same scope — both safe.
	// Borrow is on a copy-type reference, droppable has no borrows.
	ownerOK(t, `
		type Handle {
			int id;
			drop(~this) { }
		}
		getRef(string &s) string& { return s; }
		test() {
			string s = "hello";
			h := Handle(id: 1);
			string &r = getRef(s);
		}
	`)
}

func TestDropOrderSafeParameterBorrows(t *testing.T) {
	// Parameters are declared before locals — borrows from params are always safe.
	ownerOK(t, `
		getRef(string &s) string& { return s; }
		test(string s) {
			string &r = getRef(s);
		}
	`)
}

func TestDropOrderSafeMultipleLocalsWithDropAndBorrow(t *testing.T) {
	// Multiple locals with drop — borrow between them, safe order.
	ownerOK(t, `
		type Resource {
			int id;
			drop(~this) { }
		}
		getRef(string &s) string& { return s; }
		test() {
			string s = "hello";
			r := Resource(id: 1);
			string &ref = getRef(s);
		}
	`)
}

func TestDropOrderDeclOrderTracking(t *testing.T) {
	// Verify the checker tracks declaration order for parameters and locals.
	// This test ensures basic infrastructure works (params first, then locals).
	ownerOK(t, `
		type Resource {
			int id;
			drop(~this) { }
		}
		getRef(string &s) string& { return s; }
		test(string s) {
			a := Resource(id: 1);
			string &r = getRef(s);
			b := Resource(id: 2);
		}
	`)
}

// Note: Drop ordering violation (borrower with drop() declared before origin)
// is currently impossible to construct without stored references in structs
// (B0034). The checkDropOrderSafety infrastructure detects this pattern and
// will produce errors once B0034 is implemented. Reference types are Copy
// (no drop), so ref-typed borrower variables never trigger the check.

func TestHasDropMethod(t *testing.T) {
	// Verify hasDropMethod correctly identifies types with drop().
	if hasDropMethod(nil) {
		t.Error("nil type should not have drop")
	}
	if hasDropMethod(types.TypInt) {
		t.Error("int should not have drop")
	}
	n := types.NewNamed(types.NewTypeName(types.Pos{}, "Res", nil), nil)
	if hasDropMethod(n) {
		t.Error("Named without drop should return false")
	}
	n.SetHasDrop(true)
	if !hasDropMethod(n) {
		t.Error("Named with drop should return true")
	}
	inst := types.NewInstance(n, []types.Type{types.TypInt})
	if !hasDropMethod(inst) {
		t.Error("Instance of Named with drop should return true")
	}
}

// === Select case channel expression borrow leak (B0103) ===

func TestSelectCaseChannelBorrowDoesNotLeakIntoBody(t *testing.T) {
	// B0103: borrows from the channel expression in a select case must be expired
	// before the case body, so the body can re-borrow the same variables.
	// Channel expr shared-borrows obj; body needs mutable borrow.
	ownerOK(t, `
		type Router {
			channel[int] ch;
			int count;
			get_channel(&this) channel[int] { return this.ch; }
			advance(~this) { this.count += 1; }
		}
		test() {
			r := Router(ch: channel[int](), count: 0);
			select {
				v := <-r.get_channel():
					r.advance();
			}
		}
	`)
}

func TestSelectCaseSendBorrowDoesNotLeakIntoBody(t *testing.T) {
	// B0103 variant: send case with method call on channel expression.
	// Channel expr shared-borrows obj; body needs mutable borrow.
	ownerOK(t, `
		type Sender {
			channel[int] ch;
			int count;
			get_channel(&this) channel[int] { return this.ch; }
			advance(~this) { this.count += 1; }
		}
		test() {
			s := Sender(ch: channel[int](), count: 0);
			select {
				s.get_channel().send(42):
					s.advance();
			}
		}
	`)
}

// === Disjoint field borrows (B0037) ===

func TestDisjointFieldBorrowsSharedOK(t *testing.T) {
	// Borrowing disjoint fields as shared should not conflict.
	ownerOK(t, `
		type Pair { string a; string b; }
		read(string &s) {}
		test() {
			p := Pair(a: "x", b: "y");
			read(p.a);
			read(p.b);
		}
	`)
}

func TestDisjointFieldBorrowsMutOK(t *testing.T) {
	// Passing disjoint fields as mutable borrows should not conflict.
	ownerOK(t, `
		type Pair { string a; string b; }
		mutate(string ~s) {}
		test() {
			p := Pair(a: "x", b: "y");
			mutate(p.a);
			mutate(p.b);
		}
	`)
}

func TestDisjointFieldBorrowsMixedOK(t *testing.T) {
	// Shared borrow of one field and mutable borrow of a different field — OK.
	ownerOK(t, `
		type Pair { string a; string b; }
		read(string &s) {}
		mutate(string ~s) {}
		test() {
			p := Pair(a: "x", b: "y");
			read(p.a);
			mutate(p.b);
		}
	`)
}

func TestSameFieldStoredMutConflict(t *testing.T) {
	// Stored mutable borrow of a field blocks a second mutable borrow while alive.
	errs := ownerErrs(t, `
		type Foo { string x; }
		getMut(string ~s) string~ { return s; }
		mutate(string ~s) {}
		test() {
			f := Foo(x: "hi");
			string ~r = getMut(f.x);
			mutate(f.x);
			string ~r2 = r;
		}
	`)
	expectOwnerError(t, errs, "cannot borrow 'f.x' as mutable")
}

func TestSameFieldStoredSharedThenMutConflict(t *testing.T) {
	// Stored shared borrow of a field blocks a mutable borrow while borrower alive.
	errs := ownerErrs(t, `
		type Foo { string x; }
		getRef(string &s) string& { return s; }
		mutate(string ~s) {}
		test() {
			f := Foo(x: "hi");
			string &r = getRef(f.x);
			mutate(f.x);
			string &r2 = r;
		}
	`)
	expectOwnerError(t, errs, "cannot borrow 'f.x' as mutable")
}

func TestWholeVariableStoredVsFieldConflict(t *testing.T) {
	// Stored whole-variable borrow conflicts with field mutable borrow while alive.
	errs := ownerErrs(t, `
		type Foo { string x; string y; }
		getRef(Foo &f) Foo& { return f; }
		mutate(string ~s) {}
		test() {
			f := Foo(x: "a", y: "b");
			Foo &r = getRef(f);
			mutate(f.x);
			Foo &r2 = r;
		}
	`)
	expectOwnerError(t, errs, "cannot borrow 'f.x' as mutable")
}

func TestFieldStoredVsWholeVariableMutConflict(t *testing.T) {
	// Stored field borrow then whole-variable mutable borrow — conflict while alive.
	errs := ownerErrs(t, `
		type Foo { string x; string y; }
		getRef(string &s) string& { return s; }
		mutate_whole(Foo ~f) {}
		test() {
			f := Foo(x: "a", y: "b");
			string &r = getRef(f.x);
			mutate_whole(f);
			string &r2 = r;
		}
	`)
	expectOwnerError(t, errs, "cannot borrow 'f' as mutable")
}

func TestDisjointFieldsInSameCallOK(t *testing.T) {
	// Passing disjoint fields as borrow params in a single call — OK.
	ownerOK(t, `
		type Pair { string a; string b; }
		both(string &x, string &y) {}
		test() {
			p := Pair(a: "x", b: "y");
			both(p.a, p.b);
		}
	`)
}

func TestSameFieldInSameCallConflict(t *testing.T) {
	// Same field as mutable + shared in one call — conflict.
	errs := ownerErrs(t, `
		type Foo { string x; }
		mixed(string ~a, string &b) {}
		test() {
			f := Foo(x: "hi");
			mixed(f.x, f.x);
		}
	`)
	expectOwnerError(t, errs, "cannot borrow 'f.x' as mutable because it is also borrowed as shared in the same call")
}

func TestDisjointFieldsInSameCallMutOK(t *testing.T) {
	// Disjoint fields as mutable params in a single call — OK.
	ownerOK(t, `
		type Pair { string a; string b; }
		swap(string ~x, string ~y) {}
		test() {
			p := Pair(a: "x", b: "y");
			swap(p.a, p.b);
		}
	`)
}

func TestReceiverBorrowDisjointFieldOK(t *testing.T) {
	// Method call on receiver (borrows receiver) + separate field borrow — OK if disjoint.
	// NOTE: receiver borrows the whole object, so a field borrow of the same object conflicts.
	// But method call on a sub-object's field is disjoint from another field.
	ownerOK(t, `
		type Inner { int v; get_v(&this) int { return this.v; } }
		type Outer { Inner a; Inner b; }
		test() {
			o := Outer(a: Inner(v: 1), b: Inner(v: 2));
			int x = o.a.get_v();
			int y = o.b.get_v();
		}
	`)
}

// === pathsOverlap unit tests ===

func TestPathsOverlap(t *testing.T) {
	tests := []struct {
		a, b   []string
		expect bool
	}{
		{nil, nil, true},                                // whole vs whole
		{nil, []string{"x"}, true},                      // whole vs field
		{[]string{"x"}, nil, true},                      // field vs whole
		{[]string{"x"}, []string{"x"}, true},            // same field
		{[]string{"x"}, []string{"y"}, false},           // disjoint siblings
		{[]string{"x"}, []string{"x", "a"}, true},       // parent/child
		{[]string{"x", "a"}, []string{"x"}, true},       // child/parent
		{[]string{"x", "a"}, []string{"x", "b"}, false}, // disjoint nested
		{[]string{"x", "a"}, []string{"y", "a"}, false}, // different roots
	}
	for _, tt := range tests {
		got := pathsOverlap(tt.a, tt.b)
		if got != tt.expect {
			t.Errorf("pathsOverlap(%v, %v) = %v, want %v", tt.a, tt.b, got, tt.expect)
		}
	}
}

// T0087: ~ (move) parameter annotations

func TestMoveParamUseAfterMove(t *testing.T) {
	errs := ownerErrs(t, `
		consume(~string s) { }
		test() {
			string a = "hello";
			consume(a);
			string b = a;
		}
	`)
	expectOwnerError(t, errs, "use of moved variable 'a'")
}

func TestMoveParamNoError(t *testing.T) {
	ownerOK(t, `
		consume(~string s) { }
		test() {
			string a = "hello";
			consume(a);
		}
	`)
}

func TestMoveParamBorrowStillValid(t *testing.T) {
	// & param is borrowed — variable still valid after call
	ownerOK(t, `
		borrow(string &s) int { return 0; }
		test() {
			string a = "hello";
			borrow(a);
			string b = a;
		}
	`)
}

// === NLL Last-Use Analysis (B0035) ===

// checkOwnershipWithInfo parses source, runs sema + ownership, and returns
// both errors and sema.Info (for inspecting EarlyDrops).
func checkOwnershipWithInfo(t *testing.T, src string) ([]error, *sema.Info) {
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
	stdUse := &ast.UseDecl{Alias: "_", CatalogName: "std"}
	file.Uses = append([]*ast.UseDecl{stdUse}, file.Uses...)
	info, semaErrs := sema.CheckWithModules(file, map[string]*types.Scope{"std": getOwnerStdScope()})
	if len(semaErrs) > 0 {
		t.Fatalf("sema errors: %v", semaErrs)
	}
	errs := Check(file, info)
	return errs, info
}

// hasEarlyDrop checks if any early drop entry contains the given variable name.
func hasEarlyDrop(info *sema.Info, varName string) bool {
	for _, names := range info.EarlyDrops {
		for _, n := range names {
			if n == varName {
				return true
			}
		}
	}
	return false
}

func TestNLLBasicEarlyDrop(t *testing.T) {
	// Variable used in ExprStmt then not used — should be early-dropped.
	_, info := checkOwnershipWithInfo(t, `
		consume(string s) {}
		test() {
			string s = "hello";
			consume(s);
			int x = 42;
		}
	`)
	if !hasEarlyDrop(info, "s") {
		t.Error("expected early drop for 's' after consume(s)")
	}
}

func TestNLLNoEarlyDropLastStmt(t *testing.T) {
	// Variable last used in the final statement — no early drop.
	_, info := checkOwnershipWithInfo(t, `
		consume(string s) {}
		test() {
			string s = "hello";
			consume(s);
		}
	`)
	if hasEarlyDrop(info, "s") {
		t.Error("should not early-drop 's' when it's used in the last statement")
	}
}

func TestNLLNoEarlyDropNonCopyResult(t *testing.T) {
	// Variable used in VarDecl with non-copy result — skip (reference retention risk).
	_, info := checkOwnershipWithInfo(t, `
		type Wrapper { string value; }
		wrap(string s) Wrapper { return Wrapper(value: s); }
		test() {
			string s = "hello";
			Wrapper w = wrap(s);
			int x = 42;
		}
	`)
	if hasEarlyDrop(info, "s") {
		t.Error("should not early-drop 's' when last use is in VarDecl with non-copy result")
	}
}

func TestNLLEarlyDropCopyResult(t *testing.T) {
	// Variable used in VarDecl with copy result — safe to early-drop.
	_, info := checkOwnershipWithInfo(t, `
		get_len(string s) int { return 0; }
		test() {
			string s = "hello";
			int n = get_len(s);
			int x = 42;
		}
	`)
	if !hasEarlyDrop(info, "s") {
		t.Error("expected early drop for 's' after VarDecl with copy result")
	}
}

func TestNLLStringInterpolation(t *testing.T) {
	// Variable used in string interpolation — must be detected as a reference.
	_, info := checkOwnershipWithInfo(t, `
		test() {
			string s = "world";
			string msg = "hello {s}";
			int x = 42;
		}
	`)
	// s is used in the string interp at stmt 1, which produces a non-copy string.
	// isSafeForEarlyDrop should return false (VarDecl with non-copy result).
	if hasEarlyDrop(info, "s") {
		t.Error("should not early-drop 's' when used in string interpolation stored in non-copy var")
	}
}

func TestNLLCompoundAssignment(t *testing.T) {
	// Variable used in compound assignment — safe to early-drop.
	_, info := checkOwnershipWithInfo(t, `
		get_val(string s) int { return 0; }
		test() {
			string s = "hello";
			int x = get_val(s);
			x += 1;
		}
	`)
	if !hasEarlyDrop(info, "s") {
		t.Error("expected early drop for 's' after get_val(s)")
	}
}

// === NLL Phase 3: Borrow Narrowing (T0164) ===

func TestNLLBorrowExpiredAfterLastUse(t *testing.T) {
	// When a borrower is not used after the borrow, the borrow expires,
	// allowing subsequent moves of the origin.
	ownerOK(t, `
		getRef(string &s) string& { return s; }
		consume(string s) {}
		test() {
			string s = "hello";
			string &r = getRef(s);
			consume(s);
		}
	`)
}

func TestNLLBorrowExpiredMutAfterLastUse(t *testing.T) {
	// Mutable borrow expires when the borrower's last use has passed.
	ownerOK(t, `
		getMut(string ~s) string~ { return s; }
		read(string &s) {}
		test() {
			string s = "hello";
			string ~r = getMut(s);
			read(s);
		}
	`)
}

func TestNLLBorrowExpiredBeforeMove(t *testing.T) {
	// Borrower used only in ExprStmt — borrow expires, move allowed after.
	ownerOK(t, `
		getRef(string &s) string& { return s; }
		readRef(string &s) {}
		consume(string s) {}
		test() {
			string s = "hello";
			string &r = getRef(s);
			readRef(r);
			consume(s);
		}
	`)
}

func TestNLLBorrowActiveWhenUsedAfterConflict(t *testing.T) {
	// Borrower used after the conflict point — borrow must be active.
	errs := ownerErrs(t, `
		getRef(string &s) string& { return s; }
		readRef(string &s) {}
		consume(string s) {}
		test() {
			string s = "hello";
			string &r = getRef(s);
			consume(s);
			readRef(r);
		}
	`)
	expectOwnerError(t, errs, "cannot move 's' while it is borrowed")
}

func TestNLLBorrowExpiredInControlFlow(t *testing.T) {
	// Borrower not used after control flow — borrow expires.
	ownerOK(t, `
		getRef(string &s) string& { return s; }
		consume(string s) {}
		test() {
			string s = "hello";
			string &r = getRef(s);
			if true {
				string &r2 = r;
			}
			consume(s);
		}
	`)
}

func TestNLLBorrowExpiredMethodReceiver(t *testing.T) {
	// Method receiver borrow expires when borrower is no longer used.
	ownerOK(t, `
		type T {
			int x;
			getRef(&this) int& { return this.x; }
		}
		consume(T t) {}
		test() {
			T t = T(x: 1);
			int &r = t.getRef();
			consume(t);
		}
	`)
}

func TestNLLBorrowExpiredFieldBorrow(t *testing.T) {
	// Field borrow expires when borrower is no longer used.
	ownerOK(t, `
		type Foo { string x; string y; }
		getRef(string &s) string& { return s; }
		mutate(string ~s) {}
		test() {
			f := Foo(x: "a", y: "b");
			string &r = getRef(f.x);
			mutate(f.x);
		}
	`)
}

func TestNLLBorrowExpiredInferredVarDecl(t *testing.T) {
	// Inferred ref variable — borrow expires at last use.
	ownerOK(t, `
		getRef(string &s) string& { return s; }
		consume(string s) {}
		test() {
			string s = "hello";
			r := getRef(s);
			consume(s);
		}
	`)
}

func TestNLLBorrowExpiredReassigned(t *testing.T) {
	// After borrower reassignment and no further use, borrow expires.
	ownerOK(t, `
		getRef(string &s) string& { return s; }
		consume(string s) {}
		test() {
			string s = "hello";
			string &r = getRef(s);
			r = getRef(s);
			consume(s);
		}
	`)
}

// === Lifetime annotations (B0033) ===

func TestLifetimeElisionSingleRefParam(t *testing.T) {
	// Elision rule 2: exactly one ref param — its lifetime covers the return.
	ownerOK(t, `
		first(string &a) string& { return a; }
	`)
}

func TestLifetimeElisionThisReceiver(t *testing.T) {
	// Elision rule 3: &this receiver — always OK.
	ownerOK(t, `
		type Holder {
			string name;
			get_name(&this) string& { return this.name; }
		}
	`)
}

func TestLifetimeAmbiguousMultiRefReturn(t *testing.T) {
	// Rule 4: two ref params, conditional return from both — ambiguous without annotation.
	errs := ownerErrs(t, `
		pick(string &a, string &b) string& {
			if true { return a; }
			return b;
		}
	`)
	expectOwnerError(t, errs, "ambiguous return reference")
}

func TestLifetimeUnambiguousMultiRefReturn(t *testing.T) {
	// Rule 4: two ref params but always returns the same one — unambiguous.
	ownerOK(t, `
		first_of(string &a, string &b) string& {
			return a;
		}
	`)
}

func TestLifetimeExplicitSameLifetime(t *testing.T) {
	// Explicit: both params share the same lifetime, return either — OK.
	ownerOK(t, `
		longest(string &a `+"`"+`lifetime(x), string &b `+"`"+`lifetime(x)) string& `+"`"+`lifetime(x) {
			if true { return a; }
			return b;
		}
	`)
}

func TestLifetimeExplicitMismatch(t *testing.T) {
	// Explicit: return borrows from param with different lifetime than declared.
	errs := ownerErrs(t, `
		pick(string &a `+"`"+`lifetime(x), string &b `+"`"+`lifetime(y)) string& `+"`"+`lifetime(x) {
			return b;
		}
	`)
	expectOwnerError(t, errs, "returned reference borrows from parameter 'b' (lifetime 'y') but return type declares lifetime 'x'")
}

func TestLifetimeExplicitCorrect(t *testing.T) {
	// Explicit: return borrows from param with matching lifetime — OK.
	ownerOK(t, `
		pick(string &a `+"`"+`lifetime(x), string &b `+"`"+`lifetime(y)) string& `+"`"+`lifetime(x) {
			return a;
		}
	`)
}

func TestLifetimeReturnLocalStillErrors(t *testing.T) {
	// Returning a local variable as a reference is still an error (preserved behavior).
	errs := ownerErrs(t, `
		bad() string& {
			string s = "hello";
			return s;
		}
	`)
	expectOwnerError(t, errs, "cannot return reference to local variable 's'")
}

// === B0341: Field move from droppable owner ===

func TestFieldMoveMapFromDroppableError(t *testing.T) {
	errs := ownerErrs(t, `
		type Inner { map[string, string] headers; }
		type Outer { map[string, string] headers; }
		test() {
			Inner inner = Inner(headers: map[string, string]());
			Outer outer = Outer(headers: inner.headers);
		}
	`)
	expectOwnerError(t, errs, "cannot move field 'headers'")
}

func TestFieldMoveSetFromDroppableError(t *testing.T) {
	errs := ownerErrs(t, `
		type Wrapper { Set[int] items; }
		test() {
			Wrapper w = Wrapper(items: Set[int]());
			Set[int] s = w.items;
		}
	`)
	expectOwnerError(t, errs, "cannot move field 'items'")
}

func TestFieldMoveUserTypeWithDropError(t *testing.T) {
	errs := ownerErrs(t, `
		type Resource {
			int id;
			drop(~this) {}
		}
		type Owner { Resource r; }
		test() {
			Owner o = Owner(r: Resource(id: 1));
			Resource r2 = o.r;
		}
	`)
	expectOwnerError(t, errs, "cannot move field 'r'")
}

func TestFieldMoveStringFromDroppableOK(t *testing.T) {
	ownerOK(t, `
		type Inner { string name; }
		test() {
			Inner inner = Inner(name: "hello");
			string s = inner.name;
		}
	`)
}

func TestFieldMoveVectorFromDroppableOK(t *testing.T) {
	ownerOK(t, `
		type Inner { int[] items; }
		test() {
			Inner inner = Inner(items: [1, 2, 3]);
			int[] v = inner.items;
		}
	`)
}

func TestFieldMoveChannelFromDroppableOK(t *testing.T) {
	ownerOK(t, `
		type Inner { channel[int] ch; }
		test() {
			Inner inner = Inner(ch: channel[int]());
			channel[int] c = inner.ch;
		}
	`)
}

func TestFieldMoveCopyFieldOK(t *testing.T) {
	ownerOK(t, `
		type Inner { int x; string name; }
		test() {
			Inner inner = Inner(x: 42, name: "hi");
			int v = inner.x;
		}
	`)
}

func TestFieldMoveNonDroppableOwnerOK(t *testing.T) {
	// Owner has only Copy fields → no synth drop → field read is safe.
	ownerOK(t, `
		type Pair { int x; int y; }
		test() {
			Pair p = Pair(x: 1, y: 2);
			int v = p.x;
		}
	`)
}

func TestFieldMoveReturnError(t *testing.T) {
	errs := ownerErrs(t, `
		type Inner { map[string, string] headers; }
		extract(Inner inner) map[string, string] {
			return inner.headers;
		}
	`)
	expectOwnerError(t, errs, "cannot move field 'headers'")
}

func TestFieldMoveNestedCopyFieldOK(t *testing.T) {
	ownerOK(t, `
		type Inner { int id; string name; }
		type Outer { Inner inner; }
		test() {
			Outer o = Outer(inner: Inner(id: 1, name: "x"));
			int v = o.inner.id;
		}
	`)
}

func TestFieldMoveOptionalMapError(t *testing.T) {
	errs := ownerErrs(t, `
		type Wrapper { map[string, string]? headers; }
		test() {
			Wrapper w = Wrapper(headers: map[string, string]());
			map[string, string]? h = w.headers;
		}
	`)
	expectOwnerError(t, errs, "cannot move field 'headers'")
}

func TestFieldMoveOptionalStringOK(t *testing.T) {
	ownerOK(t, `
		type Wrapper { string? name; }
		test() {
			Wrapper w = Wrapper(name: "hello");
			string? s = w.name;
		}
	`)
}

func TestFieldMoveCloneCallOK(t *testing.T) {
	// .clone() returns an owned copy — tryMove sees the CallExpr result,
	// not the MemberExpr, so no error.
	ownerOK(t, `
		type Inner { map[string, string] headers; }
		type Outer { map[string, string] headers; }
		test() {
			Inner inner = Inner(headers: map[string, string]());
			Outer outer = Outer(headers: inner.headers.clone());
		}
	`)
}

func TestFieldMoveForInIterableOK(t *testing.T) {
	// For-in borrows the iterable — reading a droppable field for iteration
	// is safe and must not trigger the field-move check.
	ownerOK(t, `
		type Holder { map[string, string] data; }
		test() {
			Holder h = Holder(data: map[string, string]());
			for k, v in h.data {}
		}
	`)
}

func TestFieldMoveNonDroppableOwnerNonCopyFieldOK(t *testing.T) {
	// Owner has no drop (only contains a fieldless enum, which is non-droppable).
	// The enum field is non-Copy (no `copy annotation), but the owner isn't
	// droppable so the field read is safe — exercises the !isDroppableOwner return.
	ownerOK(t, `
		enum Color { Red; Green; Blue; }
		type Wrapper { Color c; }
		test() {
			Wrapper w = Wrapper(c: Color.Red);
			Color c = w.c;
		}
	`)
}

func TestFieldMoveNonDroppableFieldTypeOK(t *testing.T) {
	// Owner IS droppable (has string field → synth drop), but the accessed
	// field is a fieldless enum — non-Copy, non-auto-dup, but NOT droppable.
	// Exercises the !isDroppableType return in checkFieldMoveOwnership.
	ownerOK(t, `
		enum Color { Red; Green; Blue; }
		type Tagged { string name; Color tag; }
		test() {
			Tagged t = Tagged(name: "x", tag: Color.Red);
			Color c = t.tag;
		}
	`)
}

func TestFieldMoveEnumWithDropFieldError(t *testing.T) {
	// Field type is an enum that has synth-drop (variant contains a map).
	// Owner is droppable. Exercises the isDroppableType Enum branch.
	errs := ownerErrs(t, `
		enum Payload { Data(map[string, string] m); Empty; }
		type Container { Payload p; }
		test() {
			Container c = Container(p: Payload.Data(m: map[string, string]()));
			Payload p2 = c.p;
		}
	`)
	expectOwnerError(t, errs, "cannot move field 'p'")
}

// === B0351: field move from function-return temporaries ===

func TestFieldMoveFromCallResultError(t *testing.T) {
	errs := ownerErrs(t, `
		type Inner { map[string, string] headers; }
		make_inner() Inner { return Inner(headers: map[string, string]()); }
		test() {
			map[string, string] h = make_inner().headers;
		}
	`)
	expectOwnerError(t, errs, "cannot move field 'headers'")
}

func TestFieldMoveFromCallResultNestedError(t *testing.T) {
	errs := ownerErrs(t, `
		type Inner { map[string, string] headers; }
		type Outer { Inner inner; }
		make_outer() Outer { return Outer(inner: Inner(headers: map[string, string]())); }
		test() {
			map[string, string] h = make_outer().inner.headers;
		}
	`)
	expectOwnerError(t, errs, "cannot move field 'headers'")
}

func TestFieldMoveFromCallResultCloneOK(t *testing.T) {
	ownerOK(t, `
		type Inner { map[string, string] headers; }
		make_inner() Inner { return Inner(headers: map[string, string]()); }
		test() {
			map[string, string] h = make_inner().headers.clone();
		}
	`)
}

func TestFieldMoveFromCallResultCopyFieldOK(t *testing.T) {
	ownerOK(t, `
		type Inner { int id; map[string, string] headers; }
		make_inner() Inner { return Inner(id: 1, headers: map[string, string]()); }
		test() {
			int id = make_inner().id;
		}
	`)
}

func TestFieldMoveFromCallResultNonDroppableOwnerOK(t *testing.T) {
	ownerOK(t, `
		type Pair { int x; int y; }
		make_pair() Pair { return Pair(x: 1, y: 2); }
		test() {
			int x = make_pair().x;
		}
	`)
}

func TestFieldMoveFromCallResultConstructorArgError(t *testing.T) {
	// Exact reproduction case from B0351 — constructor arg context.
	errs := ownerErrs(t, `
		type Inner { map[string, string] headers; }
		type Outer { map[string, string] headers; }
		make_inner() Inner { return Inner(headers: map[string, string]()); }
		test() {
			Outer o = Outer(headers: make_inner().headers);
		}
	`)
	expectOwnerError(t, errs, "cannot move field 'headers'")
}

func TestFieldMoveFromCallResultAutoDupFieldOK(t *testing.T) {
	// String fields are auto-dup — accessing from call result should be OK.
	ownerOK(t, `
		type Inner { string name; map[string, string] headers; }
		make_inner() Inner { return Inner(name: "foo", headers: map[string, string]()); }
		test() {
			string n = make_inner().name;
		}
	`)
}

func TestFieldMoveFromCallResultReturnError(t *testing.T) {
	// Returning a droppable field from a call result — same double-drop risk.
	errs := ownerErrs(t, `
		type Inner { map[string, string] headers; }
		make_inner() Inner { return Inner(headers: map[string, string]()); }
		get_headers() map[string, string] {
			return make_inner().headers;
		}
		test() {}
	`)
	expectOwnerError(t, errs, "cannot move field 'headers'")
}
