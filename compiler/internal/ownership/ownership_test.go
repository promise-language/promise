package ownership

import (
	"strings"
	"sync"
	"testing"

	antlr "github.com/antlr4-go/antlr/v4"
	"github.com/promise-language/promise/compiler/internal/ast"
	"github.com/promise-language/promise/compiler/internal/parser"
	"github.com/promise-language/promise/compiler/internal/sema"
	"github.com/promise-language/promise/compiler/internal/testutil"
	"github.com/promise-language/promise/compiler/internal/types"
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
// Returns the combined list of sema and ownership errors. Sema errors are not fatal so
// tests can assert on the new T0438 sema-level rejections of non-Copy borrow decay
// (which used to surface as ownership errors).
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
	allErrs := append([]error(nil), semaErrs...)
	// Only run ownership when sema succeeded — incomplete type info can crash the analyzer.
	if len(semaErrs) == 0 {
		allErrs = append(allErrs, Check(file, info)...)
	}
	return allErrs
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

func expectNoOwnerError(t *testing.T, errs []error, substr string) {
	t.Helper()
	for _, e := range errs {
		if strings.Contains(e.Error(), substr) {
			t.Errorf("expected no ownership error containing %q, but got %v", substr, errs)
			return
		}
	}
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
		consume(string move s) {}
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
		consume(string move s) {}
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
		consume(string move s) {}
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
		consume(string move s) {}
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
		consume(string move s) {}
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
		consume(string move s) {}
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
		consume(string move s) {}
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
		consume(string move s) {}
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
		consume(string move s) {}
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
		consume(string move s) {}
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
		consume(int[] move a) {}
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
		read(string s) {}
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
		read(string s) {}
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
		getRef(string s) string& { return s; }
		consume(string move s) {}
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
		getRef(string s) string& { return s; }
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
		read(string s) {}
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
		getRef(string s) string& { return s; }
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
		getRef(string s) string& { return s; }
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
		getRef(string s) string& { return s; }
		consume(string move s) {}
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
		good(string s) string& { return s; }
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
			read(this) int { return this.x; }
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
			getRef(this) int& { return this.x; }
		}
		consume(T move t) {}
		test() {
			T t = T(x: 1);
			int &r = t.getRef();
			consume(move t);
			int &r2 = r;
		}
	`)
	expectOwnerError(t, errs, "cannot move 't' while it is borrowed")
}

// === Control flow and borrows ===

func TestBorrowInIfBranch(t *testing.T) {
	// Stored borrow created in then-branch persists while borrower is alive.
	errs := ownerErrs(t, `
		getRef(string s) string& { return s; }
		consume(string move s) {}
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
		getRef(string s) string& { return s; }
		consume(string move s) {}
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
		getRef(string s) string& { return s; }
		consume(string move s) {}
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
		read(int x) {}
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
		read(string s) {}
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
		read(string s) {}
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
		getRef(string s) string& { return s; }
		consume(string move s) {}
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
		consume(Resource move r) {}
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
		test(Box b) {
			string v = "hi";
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
		consume(Resource move r) { }
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
		consume(Resource move r) { }
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
			take(Resource move r) { }
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
			o := Outer(inner: move r);
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
		consume(Resource move r) { }
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
		consume_arr(int[] move a) { }
	`)
	expectOwnerError(t, errs, "use of moved variable 'arr'")
}

// checkAssignTarget: member expression target checks sub-expressions
func TestAssignTargetMemberSubExpressions(t *testing.T) {
	errs := ownerErrs(t, `
		type Box {
			int val;
		}
		consume(Box move b) { }
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
		consume_arr(int[] move a) { }
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
		consume(Resource move r) { }
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
			next_key(this) string? { return none; }
			decode_string(this) string { return ""; }
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
			peek(this) int? { return none; }
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
			has_more(this) bool { return false; }
			read(this) int { return 0; }
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
			try_parse(this) string? { return none; }
			consume(this) string { return ""; }
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
			get_items(this) int[] { return this.items; }
			log(this) {}
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
			has_next(this) bool { return this.pos < 10; }
			read(this) int { return this.pos; }
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
			is_ready(this) bool { return this.level > 0; }
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
			try_get(this) string? { return none; }
			fallback(this) string { return ""; }
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
			dequeue(this) int? { return none; }
			size(this) int { return this.count; }
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
			check_a(this) bool { return true; }
			check_b(this) bool { return true; }
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
			get_max(this) int { return this.max; }
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
		getRef(string s) string& { return s; }
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
		getRef(string s) string& { return s; }
		consume(string move s) {}
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
		getRef(string s) string& { return s; }
		consume(string move s) {}
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
		getRef(string s) string& { return s; }
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
		getRef(string s) string& { return s; }
		consume(string move s) {}
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
		getRef(string s) string& { return s; }
		consume(string move s) {}
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
		getRef(string s) string& { return s; }
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
		getRef(string s) string& { return s; }
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
		getRef(string s) string& { return s; }
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
		getRef(string s) string& { return s; }
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
		getRef(string s) string& { return s; }
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
			get_channel(this) channel[int] { return this.ch; }
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
			get_channel(this) channel[int] { return this.ch; }
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
		read(string s) {}
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
		read(string s) {}
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
		getRef(string s) string& { return s; }
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
		getRef(Foo f) Foo& { return f; }
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
		getRef(string s) string& { return s; }
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
		both(string x, string y) {}
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
		mixed(string ~a, string b) {}
		test() {
			f := Foo(x: "hi");
			mixed(f.x, f.x);
		}
	`)
	expectOwnerError(t, errs, "cannot borrow 'f.x' as shared — it is mutably borrowed")
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
		type Inner { int v; get_v(this) int { return this.v; } }
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
		consume(string move s) { }
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
		consume(string move s) { }
		test() {
			string a = "hello";
			consume(move a);
		}
	`)
}

func TestMoveParamBorrowStillValid(t *testing.T) {
	// & param is borrowed — variable still valid after call
	ownerOK(t, `
		borrow(string s) int { return 0; }
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

func TestMoveThenInterpolationRead(t *testing.T) {
	// T1135: a read of a moved-from variable inside string interpolation must be
	// detected as a use-after-move (plain move, no container involved).
	errs := ownerErrs(t, `
		make_heap(string a, string b) string { return a + b; }
		test() {
			string h = make_heap("dead", "beef");
			string z = h;
			string msg = "{h}";
		}`)
	expectOwnerError(t, errs, "use of moved variable 'h'")
}

func TestMapAssignThenInterpolationRead(t *testing.T) {
	// T1135: the reporter's case — moving into a map then reading the moved-from
	// variable inside interpolation must be a use-after-move error.
	errs := ownerErrs(t, `
		make_heap(string a, string b) string { return a + b; }
		test() {
			string h = make_heap("dead", "beef");
			map[string, string] m = {:};
			m["k"] = h;
			string msg = "{h}";
		}`)
	expectOwnerError(t, errs, "use of moved variable 'h'")
}

func TestInterpolationReadNoMove(t *testing.T) {
	// T1135 regression guard: reading a still-owned variable in interpolation
	// (and again afterward) must NOT be flagged — the new case is read-only and
	// must not introduce a spurious consume.
	ownerOK(t, `
		make_heap(string a, string b) string { return a + b; }
		take(string s) {}
		test() {
			string h = make_heap("dead", "beef");
			string msg = "{h}";
			take(h);
		}`)
}

func TestInterpolationReadMovedNonFirstPart(t *testing.T) {
	// T1135: the walk must check *every* interpolation part, not just the first —
	// a moved-from read in a later "{…}" segment of a multi-part string must
	// still be caught.
	errs := ownerErrs(t, `
		make_heap(string a, string b) string { return a + b; }
		test() {
			string ok = make_heap("x", "y");
			string h = make_heap("dead", "beef");
			string z = h;
			string msg = "{ok} and {h}";
		}`)
	expectOwnerError(t, errs, "use of moved variable 'h'")
}

func TestInterpolationReadMovedNestedExpr(t *testing.T) {
	// T1135: the moved-from read can be nested inside a sub-expression of the
	// interpolation (here a binary `+`), so checkExpr must recurse all the way
	// down, not just inspect a top-level identifier part.
	errs := ownerErrs(t, `
		make_heap(string a, string b) string { return a + b; }
		test() {
			string h = make_heap("dead", "beef");
			string z = h;
			string msg = "{h + "!"}";
		}`)
	expectOwnerError(t, errs, "use of moved variable 'h'")
}

func TestInterpolationConsumesThroughCall(t *testing.T) {
	// T1135: walking interpolation sub-expressions also closes the move-consume
	// gap — a `move` that happens *inside* "{…}" must mark the variable moved, so
	// a later use is rejected. (Before the fix, the consume was bypassed entirely.)
	errs := ownerErrs(t, `
		type Box { string s; drop(~this){} }
		eat(Box move b) string { return b.s; }
		sink(Box move b) {}
		test() {
			Box b = Box(s: "x");
			string msg = "{eat(move b)}";
			sink(move b);
		}`)
	expectOwnerError(t, errs, "use of moved variable 'b'")
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

func TestNLLNoEarlyDropMutexGuardInExprStmt(t *testing.T) {
	// T0557: pushing MutexGuard[T] into a container stores a back-pointer to
	// the parent Mutex[T]. NLL must NOT early-drop the mutex after such a
	// statement — the guard outlives it and dereferences it at drop time.
	_, info := checkOwnershipWithInfo(t, `
		test() {
			m := Mutex[int](42);
			outer := Vector[MutexGuard[int]]();
			outer.push(m.lock());
			int sentinel = 1;
		}
	`)
	if hasEarlyDrop(info, "m") {
		t.Error("should not early-drop 'm' when m.lock() produces a MutexGuard captured by an enclosing call")
	}
}

func TestNLLNoEarlyDropMutexGuardDiscarded(t *testing.T) {
	// Even when the guard is the discarded ExprStmt result (no enclosing
	// capture), suppressing the early drop is harmless: the guard's drop
	// runs as a temp before m's scope-exit drop, so ordering stays LIFO.
	// Conservative behavior is fine here.
	_, info := checkOwnershipWithInfo(t, `
		test() {
			m := Mutex[int](42);
			m.lock();
			int sentinel = 1;
		}
	`)
	if hasEarlyDrop(info, "m") {
		t.Error("should not early-drop 'm' when m.lock() returns a MutexGuard temp")
	}
}

func TestNLLEarlyDropNonGuardMethod(t *testing.T) {
	// Regression: ExprStmt method calls that don't return a back-ref carrier
	// must still be eligible for early drop. The T0557 fix only suppresses
	// MutexGuard-returning calls.
	_, info := checkOwnershipWithInfo(t, `
		test() {
			s := "hello" + "";
			s.contains("ll");
			int sentinel = 1;
		}
	`)
	if !hasEarlyDrop(info, "s") {
		t.Error("expected early drop for 's' after s.contains() (returns bool, not a back-ref carrier)")
	}
}

func TestNLLNoEarlyDropMutexGuardInInferredVarDecl(t *testing.T) {
	// T0564: VarDecl/AssignStmt RHS may carry a back-ref-carrier call inside
	// a helper argument list. The helper's copy-type return value masks the
	// captured guard from the existing top-level type check, so NLL would
	// early-drop the parent Mutex before the container that owns the guard
	// runs its drop. Suppress when the RHS contains a m.lock() on `m`.
	_, info := checkOwnershipWithInfo(t, `
		helper(int x, MutexGuard[int] g) int { return x; }
		test() {
			m := Mutex[int](42);
			n := helper(1, m.lock());
			int sentinel = 1;
		}
	`)
	if hasEarlyDrop(info, "m") {
		t.Error("should not early-drop 'm' when InferredVarDecl RHS captures m.lock() in a helper arg")
	}
}

func TestNLLNoEarlyDropMutexGuardInTypedVarDecl(t *testing.T) {
	// T0564: TypedVarDecl form of the same gap.
	_, info := checkOwnershipWithInfo(t, `
		helper(int x, MutexGuard[int] g) int { return x; }
		test() {
			m := Mutex[int](42);
			int n = helper(1, m.lock());
			int sentinel = 1;
		}
	`)
	if hasEarlyDrop(info, "m") {
		t.Error("should not early-drop 'm' when TypedVarDecl RHS captures m.lock() in a helper arg")
	}
}

func TestNLLNoEarlyDropMutexGuardInAssignStmt(t *testing.T) {
	// T0564: AssignStmt form of the same gap (OpAssign).
	_, info := checkOwnershipWithInfo(t, `
		helper(int x, MutexGuard[int] g) int { return x; }
		test() {
			m := Mutex[int](42);
			int n = 0;
			n = helper(1, m.lock());
			int sentinel = 1;
		}
	`)
	if hasEarlyDrop(info, "m") {
		t.Error("should not early-drop 'm' when AssignStmt RHS captures m.lock() in a helper arg")
	}
}

func TestNLLNoEarlyDropMutexGuardInCompoundAssign(t *testing.T) {
	// T0564: compound assign (n += ...) must also run the back-ref check
	// before returning true. Without the fix, the early `return true` for
	// non-OpAssign skipped the check entirely.
	_, info := checkOwnershipWithInfo(t, `
		helper(int x, MutexGuard[int] g) int { return x; }
		test() {
			m := Mutex[int](42);
			int n = 0;
			n += helper(1, m.lock());
			int sentinel = 1;
		}
	`)
	if hasEarlyDrop(info, "m") {
		t.Error("should not early-drop 'm' when compound-assign RHS captures m.lock() in a helper arg")
	}
}

func TestNLLEarlyDropNonGuardArgInVarDecl(t *testing.T) {
	// T0564 regression guard: the new check must only suppress when a back-ref
	// carrier is actually involved. A helper that takes a string and returns
	// an int must still allow early-drop of the string argument.
	_, info := checkOwnershipWithInfo(t, `
		str_len(string s) int { return 0; }
		test() {
			s := "hello" + "";
			int n = str_len(s);
			int sentinel = 1;
		}
	`)
	if !hasEarlyDrop(info, "s") {
		t.Error("expected early drop for 's' after VarDecl with copy result and no back-ref carrier")
	}
}

func TestNLLEarlyDropNonGuardArgInAssignStmt(t *testing.T) {
	// T0564 regression guard: AssignStmt with simple `=` Op, no back-ref
	// carrier, and a copy-type RHS must still allow early drop. Verifies the
	// new exprBackRefCapturesVar check at the top of the AssignStmt arm falls
	// through to the existing copy-type logic when no carrier is present.
	_, info := checkOwnershipWithInfo(t, `
		str_len(string s) int { return 0; }
		test() {
			s := "hello" + "";
			int n = 0;
			n = str_len(s);
			int sentinel = 1;
		}
	`)
	if !hasEarlyDrop(info, "s") {
		t.Error("expected early drop for 's' after AssignStmt with simple `=` Op and no back-ref carrier")
	}
}

func TestNLLEarlyDropNonGuardArgInCompoundAssign(t *testing.T) {
	// T0564 regression guard: compound-assign (n += ...) without a back-ref
	// carrier must still allow early drop. Exercises the `s.Op != ast.OpAssign`
	// early `return true` branch — reached only after the new back-ref check
	// passes. Symmetric counterpart to TestNLLNoEarlyDropMutexGuardInCompoundAssign:
	// that test confirms the back-ref check runs before the op-check; this one
	// confirms the op-check still fires when no back-ref is present.
	_, info := checkOwnershipWithInfo(t, `
		str_len(string s) int { return 0; }
		test() {
			s := "hello" + "";
			int n = 0;
			n += str_len(s);
			int sentinel = 1;
		}
	`)
	if !hasEarlyDrop(info, "s") {
		t.Error("expected early drop for 's' after compound-assign with no back-ref carrier")
	}
}

// TestNLLNoEarlyDropReturnThisAlias verifies that a binding initialized from a
// return-this method call on a live local (`m := d.dup()`) is NOT registered for
// NLL early drop. The result aliases the still-live source `d`; an early drop of
// `m` would free the shared instance and make `d`'s later read a use-after-free
// (T0891). The free must defer to scope exit instead.
func TestNLLNoEarlyDropReturnThisAlias(t *testing.T) {
	_, info := checkOwnershipWithInfo(t, `
		type BB { int v; dup() BB { return this; } }
		test() {
			d := BB(v: 11);
			m := d.dup();
			int a = m.v;
			int b = d.v;
		}
	`)
	if hasEarlyDrop(info, "m") {
		t.Error("should not early-drop 'm' — it aliases the still-live source 'd' (T0891)")
	}
}

// TestNLLEarlyDropFreshConstructorResult is the over-suppression guard: a binding
// initialized from a constructor (not a method call on a live local) must STILL
// be early-dropped when its last use precedes the final statement. Confirms the
// T0891 suppression is narrow and does not disable the NLL optimization.
func TestNLLEarlyDropFreshConstructorResult(t *testing.T) {
	_, info := checkOwnershipWithInfo(t, `
		type BB { int n; }
		test() {
			t := BB(n: 5);
			int x = t.n;
			int sentinel = 1;
		}
	`)
	if !hasEarlyDrop(info, "t") {
		t.Error("expected early drop for 't' — constructor result is not a return-this alias")
	}
}

// TestNLLEarlyDropFreeFunctionResult is a second over-suppression guard: a
// binding initialized from a free-function call (chain origin is a func, not a
// live local variable) must STILL be early-dropped.
func TestNLLEarlyDropFreeFunctionResult(t *testing.T) {
	_, info := checkOwnershipWithInfo(t, `
		type BB { int n; }
		make_bb() BB { return BB(n: 1); }
		test() {
			b := make_bb();
			int x = b.n;
			int sentinel = 1;
		}
	`)
	if !hasEarlyDrop(info, "b") {
		t.Error("expected early drop for 'b' — free-function result is not a return-this alias")
	}
}

// TestNLLNoEarlyDropReturnThisAliasFromThis exercises the `this`-rooted branch of
// initMayAliasReceiver (aliasReceiverOrigin → *ast.ThisExpr): a method that binds
// the result of `this.dup()` (a return-this on its own receiver) must NOT
// early-drop that binding — the result aliases the still-live `this`. T0891.
func TestNLLNoEarlyDropReturnThisAliasFromThis(t *testing.T) {
	_, info := checkOwnershipWithInfo(t, `
		type BB {
			int v;
			dup() BB { return this; }
			chain() {
				m := this.dup();
				int a = m.v;
				int sentinel = 1;
			}
		}
		test() {}
	`)
	if hasEarlyDrop(info, "m") {
		t.Error("should not early-drop 'm' — it aliases the still-live receiver 'this' (T0891)")
	}
}

// TestNLLNoEarlyDropReturnThisAliasThroughErrorWrappers exercises the
// unwrapEarlyDropWrappers peel set: a return-this bind reached through the error
// operators (`?!` ErrorPanicExpr, `?^` ErrorPropagateExpr, `? e {}` ErrorHandlerExpr)
// must still be recognized as an aliasing init and NOT early-dropped. Mirrors the
// wrapper set the codegen alias-clear is meant to peel. T0891.
func TestNLLNoEarlyDropReturnThisAliasThroughErrorWrappers(t *testing.T) {
	cases := []struct {
		name string
		bind string
		fail bool // test function must be failable for ?^
	}{
		{"panic", `m := d.dup()?!;`, false},
		{"propagate", `m := d.dup()?^;`, true},
		{"handler", `m := d.dup() ? e { return; };`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fail := ""
			if tc.fail {
				fail = "!"
			}
			_, info := checkOwnershipWithInfo(t, `
				type BB { int v; dup!() BB { return this; } }
				test`+fail+`() {
					d := BB(v: 11);
					`+tc.bind+`
					int a = m.v;
					int sentinel = 1;
				}
			`)
			if hasEarlyDrop(info, "m") {
				t.Errorf("should not early-drop 'm' through %s wrapper — return-this alias (T0891)", tc.name)
			}
		})
	}
}

// TestNLLEarlyDropFailableFreeFunctionThroughPanic is the over-suppression guard
// for the wrapper-peel path: a NON-return-this failable free function reached
// through `?!` must STILL be early-dropped. Confirms unwrapEarlyDropWrappers does
// not blanket-suppress every wrapped initializer — only true return-this aliases.
func TestNLLEarlyDropFailableFreeFunctionThroughPanic(t *testing.T) {
	_, info := checkOwnershipWithInfo(t, `
		type BB { int v; }
		make_bb!() BB { return BB(v: 1); }
		test() {
			m := make_bb()?!;
			int a = m.v;
			int sentinel = 1;
		}
	`)
	if !hasEarlyDrop(info, "m") {
		t.Error("expected early drop for 'm' — failable free-function result through ?! is not a return-this alias")
	}
}

// TestExprBackRefCapturesVar_AllWrappers exercises every AST wrapper branch in
// exprBackRefCapturesVar by synthesizing AST trees with a `m.lock()` call (return
// type MutexGuard[int]) nested inside each wrapper type. The function must
// return true for every wrapper that can transitively contain the call. T0557.
//
// Without these tests, a missing wrapper case would silently allow NLL to
// early-drop the parent Mutex despite a guard being captured deeper in the
// expression tree → use-after-free at drop time.
func TestExprBackRefCapturesVar_AllWrappers(t *testing.T) {
	a := &lastUseAnalyzer{info: &sema.Info{Types: map[ast.Expr]types.Type{}}}

	// makeLock builds an `m.lock()` CallExpr with return type MutexGuard[int].
	makeLock := func() *ast.CallExpr {
		mem := &ast.MemberExpr{Target: &ast.IdentExpr{Name: "m"}, Field: "lock"}
		call := &ast.CallExpr{Callee: mem}
		a.info.Types[call] = types.NewMutexGuard(types.TypInt)
		return call
	}

	// Benign expression standing in for any non-matching subtree.
	benign := func() ast.Expr { return &ast.IntLit{Raw: "1"} }

	cases := []struct {
		name string
		make func(inner ast.Expr) ast.Expr
	}{
		{"ParenExpr", func(inner ast.Expr) ast.Expr {
			return &ast.ParenExpr{Expr: inner}
		}},
		{"BinaryExpr_Left", func(inner ast.Expr) ast.Expr {
			return &ast.BinaryExpr{Left: inner, Right: benign()}
		}},
		{"BinaryExpr_Right", func(inner ast.Expr) ast.Expr {
			return &ast.BinaryExpr{Left: benign(), Right: inner}
		}},
		{"UnaryExpr", func(inner ast.Expr) ast.Expr {
			return &ast.UnaryExpr{Operand: inner}
		}},
		{"IndexExpr_Target", func(inner ast.Expr) ast.Expr {
			return &ast.IndexExpr{Target: inner, Index: benign()}
		}},
		{"IndexExpr_Index", func(inner ast.Expr) ast.Expr {
			return &ast.IndexExpr{Target: &ast.IdentExpr{Name: "x"}, Index: inner}
		}},
		{"IndexExpr_ExtraIndices", func(inner ast.Expr) ast.Expr {
			return &ast.IndexExpr{
				Target:       &ast.IdentExpr{Name: "x"},
				Index:        benign(),
				ExtraIndices: []ast.Expr{inner},
			}
		}},
		{"SliceExpr_Target", func(inner ast.Expr) ast.Expr {
			return &ast.SliceExpr{Target: inner, Low: benign(), High: benign()}
		}},
		{"SliceExpr_Low", func(inner ast.Expr) ast.Expr {
			return &ast.SliceExpr{Target: &ast.IdentExpr{Name: "x"}, Low: inner, High: benign()}
		}},
		{"SliceExpr_High", func(inner ast.Expr) ast.Expr {
			return &ast.SliceExpr{Target: &ast.IdentExpr{Name: "x"}, Low: benign(), High: inner}
		}},
		{"CastExpr", func(inner ast.Expr) ast.Expr {
			return &ast.CastExpr{Expr: inner}
		}},
		{"IsExpr", func(inner ast.Expr) ast.Expr {
			return &ast.IsExpr{Expr: inner}
		}},
		{"ErrorPropagateExpr", func(inner ast.Expr) ast.Expr {
			return &ast.ErrorPropagateExpr{Expr: inner}
		}},
		{"ErrorPanicExpr", func(inner ast.Expr) ast.Expr {
			return &ast.ErrorPanicExpr{Expr: inner}
		}},
		{"OptionalUnwrapExpr", func(inner ast.Expr) ast.Expr {
			return &ast.OptionalUnwrapExpr{Expr: inner}
		}},
		{"ErrorHandlerExpr", func(inner ast.Expr) ast.Expr {
			return &ast.ErrorHandlerExpr{Expr: inner}
		}},
		{"IfExpr_Cond", func(inner ast.Expr) ast.Expr {
			return &ast.IfExpr{Cond: inner}
		}},
		{"MatchExpr_Subject", func(inner ast.Expr) ast.Expr {
			return &ast.MatchExpr{Subject: inner}
		}},
		{"TupleLit", func(inner ast.Expr) ast.Expr {
			return &ast.TupleLit{Elements: []ast.Expr{benign(), inner}}
		}},
		{"ArrayLit", func(inner ast.Expr) ast.Expr {
			return &ast.ArrayLit{Elements: []ast.Expr{benign(), inner}}
		}},
		{"MapLit_Key", func(inner ast.Expr) ast.Expr {
			return &ast.MapLit{Entries: []*ast.MapEntry{{Key: inner, Value: benign()}}}
		}},
		{"MapLit_Value", func(inner ast.Expr) ast.Expr {
			return &ast.MapLit{Entries: []*ast.MapEntry{{Key: benign(), Value: inner}}}
		}},
		{"MemberExpr_Target", func(inner ast.Expr) ast.Expr {
			return &ast.MemberExpr{Target: inner, Field: "field"}
		}},
		{"OptionalChainExpr_Target", func(inner ast.Expr) ast.Expr {
			return &ast.OptionalChainExpr{Target: inner, Field: "field"}
		}},
		{"CallExpr_Arg", func(inner ast.Expr) ast.Expr {
			return &ast.CallExpr{
				Callee: &ast.IdentExpr{Name: "f"},
				Args:   []*ast.Arg{{Value: inner}},
			}
		}},
		{"CallExpr_NestedCallee", func(inner ast.Expr) ast.Expr {
			// outer call whose callee tree contains the back-ref call.
			// e.g. inner.something(...) — exercises the Callee recursion (line 261-263).
			callee := &ast.MemberExpr{Target: inner, Field: "borrow"}
			return &ast.CallExpr{Callee: callee}
		}},
		// Deeply nested: array → paren → tuple → call.
		{"DeepNested", func(inner ast.Expr) ast.Expr {
			return &ast.ArrayLit{Elements: []ast.Expr{
				&ast.ParenExpr{Expr: &ast.TupleLit{Elements: []ast.Expr{
					&ast.BinaryExpr{Left: benign(), Right: inner},
				}}},
			}}
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name+"_positive", func(t *testing.T) {
			expr := tc.make(makeLock())
			if !a.exprBackRefCapturesVar(expr, "m") {
				t.Errorf("expected true for %s wrapping m.lock(), got false", tc.name)
			}
		})
		t.Run(tc.name+"_negative", func(t *testing.T) {
			// Same wrapper, but inner expression doesn't reference m.
			expr := tc.make(benign())
			if a.exprBackRefCapturesVar(expr, "m") {
				t.Errorf("expected false for %s wrapping benign expression, got true", tc.name)
			}
		})
	}
}

// TestExprBackRefCapturesVar_NilExpr verifies the nil-guard. Defense-in-depth
// for callers that may pass nil sub-expressions (e.g. SliceExpr.Low/High can be
// nil for [:high] / [low:] forms).
func TestExprBackRefCapturesVar_NilExpr(t *testing.T) {
	a := &lastUseAnalyzer{info: &sema.Info{Types: map[ast.Expr]types.Type{}}}
	if a.exprBackRefCapturesVar(nil, "m") {
		t.Error("nil expression must return false")
	}
	// SliceExpr with nil Low and High (legal AST for x[:]) must not panic.
	slice := &ast.SliceExpr{Target: &ast.IdentExpr{Name: "x"}, Low: nil, High: nil}
	if a.exprBackRefCapturesVar(slice, "m") {
		t.Error("SliceExpr with nil bounds and benign target must return false")
	}
}

// TestExprBackRefCapturesVar_WrongReceiver verifies that a back-ref-carrier
// method call on a *different* variable does not trigger suppression for the
// variable being analyzed.
func TestExprBackRefCapturesVar_WrongReceiver(t *testing.T) {
	a := &lastUseAnalyzer{info: &sema.Info{Types: map[ast.Expr]types.Type{}}}
	// n.lock() — receiver is "n", we ask about "m".
	mem := &ast.MemberExpr{Target: &ast.IdentExpr{Name: "n"}, Field: "lock"}
	call := &ast.CallExpr{Callee: mem}
	a.info.Types[call] = types.NewMutexGuard(types.TypInt)
	if a.exprBackRefCapturesVar(call, "m") {
		t.Error("expected false: receiver is 'n', not 'm'")
	}
	if !a.exprBackRefCapturesVar(call, "n") {
		t.Error("expected true: receiver matches 'n'")
	}
}

// TestExprBackRefCapturesVar_NonIdentReceiver verifies that a method call
// whose receiver is not a simple IdentExpr (e.g. `something.field.lock()`)
// does not trigger the direct-match path, but recursion still works.
func TestExprBackRefCapturesVar_NonIdentReceiver(t *testing.T) {
	a := &lastUseAnalyzer{info: &sema.Info{Types: map[ast.Expr]types.Type{}}}
	// x.field.lock() — receiver is MemberExpr, not IdentExpr.
	inner := &ast.MemberExpr{Target: &ast.IdentExpr{Name: "x"}, Field: "field"}
	mem := &ast.MemberExpr{Target: inner, Field: "lock"}
	call := &ast.CallExpr{Callee: mem}
	a.info.Types[call] = types.NewMutexGuard(types.TypInt)
	// Direct-match path requires IdentExpr receiver, so "x" is not matched here.
	// (Future work: recursive descent into MemberExpr targets if needed — see T0564 scope note.)
	if a.exprBackRefCapturesVar(call, "x") {
		t.Error("non-IdentExpr receiver should not trigger direct match for 'x'")
	}
}

// TestIsBackRefCarrier exercises the helper directly for all branches.
func TestIsBackRefCarrier(t *testing.T) {
	// nil → false (defensive)
	if isBackRefCarrier(nil) {
		t.Error("nil type must return false")
	}
	// MutexGuard[T] → true
	if !isBackRefCarrier(types.NewMutexGuard(types.TypInt)) {
		t.Error("MutexGuard[int] must return true")
	}
	// Plain types → false
	if isBackRefCarrier(types.TypInt) {
		t.Error("int must return false")
	}
	if isBackRefCarrier(types.TypString) {
		t.Error("string must return false")
	}
	if isBackRefCarrier(types.TypBool) {
		t.Error("bool must return false")
	}
}

// === NLL Phase 3: Borrow Narrowing (T0164) ===

func TestNLLBorrowExpiredAfterLastUse(t *testing.T) {
	// When a borrower is not used after the borrow, the borrow expires,
	// allowing subsequent moves of the origin.
	ownerOK(t, `
		getRef(string s) string& { return s; }
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
		read(string s) {}
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
		getRef(string s) string& { return s; }
		readRef(string s) {}
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
		getRef(string s) string& { return s; }
		readRef(string s) {}
		consume(string move s) {}
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
		getRef(string s) string& { return s; }
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
			getRef(this) int& { return this.x; }
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
		getRef(string s) string& { return s; }
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
		getRef(string s) string& { return s; }
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
		getRef(string s) string& { return s; }
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
		first(string a) string& { return a; }
	`)
}

func TestLifetimeElisionThisReceiver(t *testing.T) {
	// Elision rule 3: this receiver — always OK.
	ownerOK(t, `
		type Holder {
			string name;
			get_name(this) string& { return this.name; }
		}
	`)
}

func TestLifetimeAmbiguousMultiRefReturn(t *testing.T) {
	// Rule 4: two ref params, conditional return from both — ambiguous without annotation.
	errs := ownerErrs(t, `
		pick(string a, string b) string& {
			if true { return a; }
			return b;
		}
	`)
	expectOwnerError(t, errs, "ambiguous return reference")
}

func TestLifetimeUnambiguousMultiRefReturn(t *testing.T) {
	// Rule 4: two ref params but always returns the same one — unambiguous.
	ownerOK(t, `
		first_of(string a, string b) string& {
			return a;
		}
	`)
}

func TestLifetimeExplicitSameLifetime(t *testing.T) {
	// Explicit: both params share the same lifetime, return either — OK.
	ownerOK(t, `
		longest(string a `+"`"+`lifetime(x), string b `+"`"+`lifetime(x)) string& `+"`"+`lifetime(x) {
			if true { return a; }
			return b;
		}
	`)
}

func TestLifetimeExplicitMismatch(t *testing.T) {
	// Explicit: return borrows from param with different lifetime than declared.
	errs := ownerErrs(t, `
		pick(string a `+"`"+`lifetime(x), string b `+"`"+`lifetime(y)) string& `+"`"+`lifetime(x) {
			return b;
		}
	`)
	expectOwnerError(t, errs, "returned reference borrows from parameter 'b' (lifetime 'y') but return type declares lifetime 'x'")
}

func TestLifetimeExplicitCorrect(t *testing.T) {
	// Explicit: return borrows from param with matching lifetime — OK.
	ownerOK(t, `
		pick(string a `+"`"+`lifetime(x), string b `+"`"+`lifetime(y)) string& `+"`"+`lifetime(x) {
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
			Wrapper w = Wrapper(c: move Color.Red);
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
			Tagged t = Tagged(name: "x", tag: move Color.Red);
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

// === T0473: generic Optional/TypeParam field move on droppable instantiation ===

// `Holder[T]{T? value}` instantiated with a droppable T must reject `if y := h.value`
// — sema's fieldTypeHasDrop returns false for TypeParam, so the generic origin has
// HasDrop=false and NeedsSynthDrop=false, but codegen's monoInstNeedsSynthDrop
// generates a drop for `Holder[_BoxDrop]`, leading to a runtime double-free if the
// move is not rejected here.
func TestFieldMoveGenericOptionalDroppableError(t *testing.T) {
	errs := ownerErrs(t, `
		type _BoxDrop {
			int n;
			drop(~this) {}
		}
		type Holder[T] { T? value; }
		test() {
			_BoxDrop? a = _BoxDrop(n: 7);
			Holder[_BoxDrop] h = Holder[_BoxDrop](value: a);
			if y := h.value {}
		}
	`)
	expectOwnerError(t, errs, "cannot move field 'value'")
}

// Same shape via the var-decl path (no `if` unwrap) — also rejected.
func TestFieldMoveGenericOptionalDroppableVarDeclError(t *testing.T) {
	errs := ownerErrs(t, `
		type _BoxDrop {
			int n;
			drop(~this) {}
		}
		type Holder[T] { T? value; }
		test() {
			_BoxDrop? a = _BoxDrop(n: 7);
			Holder[_BoxDrop] h = Holder[_BoxDrop](value: a);
			_BoxDrop? y = h.value;
		}
	`)
	expectOwnerError(t, errs, "cannot move field 'value'")
}

// `Holder[int]{T? value}` — the substituted field type is `int?`, which is
// non-droppable. Bare field read must remain allowed (no false positive).
func TestFieldMoveGenericOptionalNonDroppableOK(t *testing.T) {
	ownerOK(t, `
		type Holder[T] { T? value; }
		test() {
			Holder[int] h = Holder[int](value: 7);
			if y := h.value {}
		}
	`)
}

// `Holder[T]{T value}` — non-Optional TypeParam field instantiated with a
// droppable type must also reject the field move (parallels B0202/B0209).
func TestFieldMoveGenericNonOptionalDroppableError(t *testing.T) {
	errs := ownerErrs(t, `
		type _BoxDrop {
			int n;
			drop(~this) {}
		}
		type Holder[T] { T value; }
		test() {
			Holder[_BoxDrop] h = Holder[_BoxDrop](value: _BoxDrop(n: 7));
			_BoxDrop b = h.value;
		}
	`)
	expectOwnerError(t, errs, "cannot move field 'value'")
}

// T0505: `Holder[T]{(T, int) pair}` instantiated with a droppable T must reject
// `(_BoxDrop, int) p = h.pair;` — sema's fieldTypeHasDrop doesn't see through
// the TypeParam-containing tuple field, but codegen's monoTypeHasDroppable
// recurses into tuple elements, so without the ownership-side Tuple case the
// move would slip through and double-free at runtime.
func TestFieldMoveGenericTupleDroppableError(t *testing.T) {
	errs := ownerErrs(t, `
		type _BoxDrop {
			int n;
			drop(~this) {}
		}
		type Holder[T] { (T, int) pair; }
		test() {
			Holder[_BoxDrop] h = Holder[_BoxDrop](pair: (_BoxDrop(n: 7), 2));
			(_BoxDrop, int) p = h.pair;
		}
	`)
	expectOwnerError(t, errs, "cannot move field 'pair'")
}

// Destructure-decl from a MemberExpr source is handled at codegen as a borrow
// (genDestructureVarDecl srcOwned=false: no drop bindings on destructured
// locals, parent owner retains ownership). So `(b, n) := h.pair` is safe at
// runtime even when the tuple has droppable elements. checkDestructureVarDecl
// skips the field-move check for MemberExpr/IndexExpr sources to align with
// this. Existing T0389/T0420 e2e tests rely on this borrow-from-field pattern.
func TestFieldMoveGenericTupleDroppableDestructureOK(t *testing.T) {
	ownerOK(t, `
		type _BoxDrop {
			int n;
			drop(~this) {}
		}
		type Holder[T] { (T, int) pair; }
		test() {
			Holder[_BoxDrop] h = Holder[_BoxDrop](pair: (_BoxDrop(n: 7), 2));
			(b, n) := h.pair;
		}
	`)
}

// `Holder[int]{(T, int) pair}` — substituted field type is `(int, int)`, all
// non-droppable. Bare field read must remain allowed (negative test: guards
// against the Tuple recursion producing false positives).
func TestFieldMoveGenericTupleNonDroppableOK(t *testing.T) {
	ownerOK(t, `
		type Holder[T] { (T, int) pair; }
		test() {
			Holder[int] h = Holder[int](pair: (7, 2));
			(int, int) p = h.pair;
		}
	`)
}

// Parallel to TestFieldMoveGenericTupleDroppableDestructureOK but exercising
// the *ast.IndexExpr branch of checkDestructureVarDecl's switch. A destructure
// from `v[i]` on a droppable-tuple vector must be allowed (borrow path) —
// codegen treats the destructured locals as non-owning, parent vector retains
// ownership. The corresponding e2e test
// test_destructure_indexexpr_droppable_tuple validates the runtime
// no-double-free behavior.
func TestFieldMoveGenericTupleDroppableIndexExprDestructureOK(t *testing.T) {
	ownerOK(t, `
		type _BoxDrop {
			int n;
			drop(~this) {}
		}
		test() {
			(_BoxDrop, int)[] v = [(_BoxDrop(n: 7), 2)];
			(b, n) := v[0];
		}
	`)
}

// === T0548: destructure-from-field produces tracked borrows ===
//
// Destructuring from a MemberExpr / IndexExpr source emits no drop bindings
// in codegen — the parent owner retains ownership of the inner data, and the
// destructured locals are borrows at runtime. T0505 left the ownership-side
// permissive (no field-move check, all locals marked Owned), which let
// `consume(h)` AFTER `(b, n) := h.pair` slip through to runtime UAF/double-free.
// T0548 marks the non-Copy locals as Borrowed and registers a shared borrow
// on the source's root variable so subsequent moves of the parent are
// rejected at compile time while any borrower is alive. T0164 NLL borrow
// narrowing expires the borrow at each borrower's last use.

// Destructure-from-field then consume parent — must reject. This was the
// original T0548 UAF / segfault repro.
func TestDestructureFromFieldConsumeParentRejected(t *testing.T) {
	errs := ownerErrs(t, `
		type _BoxStr { string s; }
		type Holder { (_BoxStr, int) pair; }
		consume(Holder move h) {}
		test() {
			Holder h = Holder(pair: (_BoxStr(s: "x"), 2));
			(b, n) := h.pair;
			consume(h);
			_ = b.s;
		}
	`)
	expectOwnerError(t, errs, "cannot move 'h' while it is borrowed")
}

// IndexExpr source variant — destructure-from-vector-element then consume the
// vector must reject. Parallel to the MemberExpr case.
func TestDestructureFromIndexConsumeParentRejected(t *testing.T) {
	errs := ownerErrs(t, `
		type _BoxStr { string s; }
		consume_vec(Vector[(_BoxStr, int)] move v) {}
		test() {
			Vector[(_BoxStr, int)] arr = [];
			arr.push((_BoxStr(s: "x"), 2));
			(b, n) := arr[0];
			consume_vec(arr);
			_ = b.s;
		}
	`)
	expectOwnerError(t, errs, "cannot move 'arr' while it is borrowed")
}

// === T0570: ParenExpr-wrapped destructure sources route through the same
// borrow path as bare MemberExpr / IndexExpr. Without paren peeling at the
// dispatch switch, the ownership-side tryMove silently no-ops on
// ParenExpr → destructured locals stayed Owned → consume of the parent slipped
// through to a runtime UAF / double-free. ===

// Paren-wrapped MemberExpr source — consume parent before locals' last use
// must reject. Mirrors TestDestructureFromFieldConsumeParentRejected, but
// with `(h.pair)` instead of `h.pair`.
func TestDestructureFromFieldParenConsumeParentRejected(t *testing.T) {
	errs := ownerErrs(t, `
		type _BoxStr { string s; }
		type Holder { (_BoxStr, int) pair; }
		consume(Holder move h) {}
		test() {
			Holder h = Holder(pair: (_BoxStr(s: "x"), 2));
			(b, n) := (h.pair);
			consume(h);
			_ = b.s;
		}
	`)
	expectOwnerError(t, errs, "cannot move 'h' while it is borrowed")
}

// Paren-wrapped MemberExpr + NLL borrow narrowing — read both locals, then
// consume the parent. Must accept (borrow expires at the borrowers' last use).
func TestDestructureFromFieldParenConsumeAfterLastUseOK(t *testing.T) {
	ownerOK(t, `
		type _BoxStr { string s; }
		type Holder { (_BoxStr, int) pair; }
		consume(Holder move h) {}
		test() {
			Holder h = Holder(pair: (_BoxStr(s: "x"), 2));
			(b, n) := (h.pair);
			_ = b.s;
			_ = n;
			consume(move h);
		}
	`)
}

// Paren-wrapped IndexExpr source — destructure from a vector element wrapped
// in parens, then consume the vector. Must reject for symmetry with the
// MemberExpr case.
func TestDestructureFromIndexParenConsumeParentRejected(t *testing.T) {
	errs := ownerErrs(t, `
		type _BoxStr { string s; }
		consume_vec(Vector[(_BoxStr, int)] move v) {}
		test() {
			Vector[(_BoxStr, int)] arr = [];
			arr.push((_BoxStr(s: "x"), 2));
			(b, n) := (arr[0]);
			consume_vec(arr);
			_ = b.s;
		}
	`)
	expectOwnerError(t, errs, "cannot move 'arr' while it is borrowed")
}

// Paren-wrapped IndexExpr + NLL borrow narrowing — read both locals, then
// consume the vector. Must accept; mirrors the MemberExpr OK test for the
// IndexExpr arm of the lastuse.go T0570 paren peel.
func TestDestructureFromIndexParenConsumeAfterLastUseOK(t *testing.T) {
	ownerOK(t, `
		type _BoxStr { string s; }
		consume_vec(Vector[(_BoxStr, int)] move v) {}
		test() {
			Vector[(_BoxStr, int)] arr = [];
			arr.push((_BoxStr(s: "x"), 2));
			(b, n) := (arr[0]);
			_ = b.s;
			_ = n;
			consume_vec(move arr);
		}
	`)
}

// Double-wrapped parens — `((h.pair))` — confirms the iterative peel handles
// nested ParenExpr without leaving a borrow gap.
func TestDestructureFromFieldDoubleParenRejected(t *testing.T) {
	errs := ownerErrs(t, `
		type _BoxStr { string s; }
		type Holder { (_BoxStr, int) pair; }
		consume(Holder move h) {}
		test() {
			Holder h = Holder(pair: (_BoxStr(s: "x"), 2));
			(b, n) := ((h.pair));
			consume(h);
			_ = b.s;
		}
	`)
	expectOwnerError(t, errs, "cannot move 'h' while it is borrowed")
}

// T0164 NLL borrow narrowing: destructure, use both locals, THEN consume the
// parent — must accept (borrow expires at the borrower's last use).
func TestDestructureFromFieldNLLNarrowing(t *testing.T) {
	ownerOK(t, `
		type _BoxStr { string s; }
		type Holder { (_BoxStr, int) pair; }
		consume(Holder move h) {}
		test() {
			Holder h = Holder(pair: (_BoxStr(s: "x"), 2));
			(b, n) := h.pair;
			_ = b.s;
			_ = n;
			consume(move h);
		}
	`)
}

// T0850: an if-unwrap of a borrowed optional (`Ref[T?].borrow`) binds a
// non-owning view (Borrowed state), so reading it is fine but moving it out
// into an owned var-decl must be rejected — otherwise the moved-out copy and
// the Arc payload would both free the same instance (double-free).
func TestIfUnwrapBorrowedOptionalReadOK(t *testing.T) {
	ownerOK(t, `
		type Shape { string name; area(this) f64 `+"`"+`abstract; }
		type Circle is Shape { f64 radius; area(this) f64 { return this.radius; } }
		test() {
			Circle? init = Circle(name: "c", radius: 1.0);
			a := Ref[Circle?](move init);
			if x := a.borrow {
				_ := x.radius;
			}
		}
	`)
}

func TestIfUnwrapBorrowedOptionalMoveRejected(t *testing.T) {
	errs := ownerErrs(t, `
		type Shape { string name; area(this) f64 `+"`"+`abstract; }
		type Circle is Shape { f64 radius; area(this) f64 { return this.radius; } }
		test() {
			Circle? init = Circle(name: "c", radius: 1.0);
			a := Ref[Circle?](init);
			if x := a.borrow {
				Circle owned = x;
				_ := owned.radius;
			}
		}
	`)
	expectOwnerError(t, errs, "cannot move borrowed value 'x'")
}

// Destructure-from-field then move the destructured local into a consume
// site — must reject. Mirrors the existing T0338 "cannot move borrowed value"
// path: the local is in Borrowed state so tryMoveConsume rejects.
func TestDestructureFromFieldMoveLocalRejected(t *testing.T) {
	errs := ownerErrs(t, `
		type _BoxStr { string s; }
		type Holder { (_BoxStr, int) pair; }
		consume_box(_BoxStr move b) {}
		test() {
			Holder h = Holder(pair: (_BoxStr(s: "x"), 2));
			(b, n) := h.pair;
			consume_box(b);
			_ = n;
		}
	`)
	expectOwnerError(t, errs, "cannot move borrowed value 'b'")
}

// All-Copy tuple elements: no borrow is registered for `b`/`n` (both int) →
// consuming the parent is allowed.
func TestDestructureFromFieldAllCopyElemsOK(t *testing.T) {
	ownerOK(t, `
		type Holder { (int, int) pair; }
		consume(Holder move h) {}
		test() {
			Holder h = Holder(pair: (1, 2));
			(a, b) := h.pair;
			_ = a + b;
			consume(move h);
		}
	`)
}

// Mixed Copy / non-Copy: only the non-Copy local registers a borrow. Consume
// after its last use must still be accepted via NLL narrowing.
func TestDestructureFromFieldPartialCopyMixedNLL(t *testing.T) {
	ownerOK(t, `
		type _BoxStr { string s; }
		type Holder { (_BoxStr, int) pair; }
		consume(Holder move h) {}
		test() {
			Holder h = Holder(pair: (_BoxStr(s: "x"), 2));
			(b, n) := h.pair;
			_ = b.s;
			consume(move h);
			_ = n;
		}
	`)
}

// `_` discard slot does not register a borrow (the unused element is dropped
// at scope exit normally); the non-`_` slot still registers, so consuming
// the parent before its last use is rejected.
func TestDestructureFromFieldDiscardSlotRejected(t *testing.T) {
	errs := ownerErrs(t, `
		type _BoxStr { string s; }
		type Holder { (_BoxStr, int) pair; }
		consume(Holder move h) {}
		test() {
			Holder h = Holder(pair: (_BoxStr(s: "x"), 2));
			(b, _) := h.pair;
			consume(h);
			_ = b.s;
		}
	`)
	expectOwnerError(t, errs, "cannot move 'h' while it is borrowed")
}

// ThisExpr root: destructure from `this.pair` in a `~this` method then call
// a consumer that takes `~Holder` — must reject. Without the `this` borrow
// check this slips through to a runtime UAF.
func TestDestructureFromThisConsumeRejected(t *testing.T) {
	errs := ownerErrs(t, `
		type _BoxStr { string s; }
		type Holder {
			(_BoxStr, int) pair;
			eat(~this) {
				(b, n) := this.pair;
				consume_holder(this);
				_ = b.s;
				_ = n;
			}
		}
		consume_holder(Holder move h) {}
		test() {}
	`)
	expectOwnerError(t, errs, "cannot move 'this' while it is borrowed")
}

// ThisExpr root + NLL narrowing: destructure, read both locals, THEN attempt
// to consume the receiver — T0569 rejects the consume regardless of NLL
// narrowing, since `~this` does not grant ownership.
func TestDestructureFromThisNLLNarrowing(t *testing.T) {
	errs := ownerErrs(t, `
		type _BoxStr { string s; }
		type Holder {
			(_BoxStr, int) pair;
			eat(~this) {
				(b, n) := this.pair;
				_ = b.s;
				_ = n;
				consume_holder(this);
			}
		}
		consume_holder(Holder move h) {}
		test() {}
	`)
	expectOwnerError(t, errs, "cannot consume 'this'")
}

// Destructure from a call-result member (`make_holder().pair`) — the source
// has no stable IdentExpr root, so destructureBorrowRoot returns "" and the
// T0571 rejection fires up front. The per-element loop still runs and marks
// non-Copy locals as Borrowed (no Origin to attach), so the subsequent
// `consume_box(b)` also triggers the "cannot move borrowed value" diagnostic.
// This test guards the Borrowed-state propagation path; the T0571 block below
// guards the primary rejection diagnostic.
func TestDestructureFromCallMemberMoveLocalRejected(t *testing.T) {
	errs := ownerErrs(t, `
		type _BoxStr { string s; }
		type Holder { (_BoxStr, int) pair; }
		make_holder() Holder { return Holder(pair: (_BoxStr(s: "x"), 2)); }
		consume_box(_BoxStr move b) {}
		test() {
			(b, n) := make_holder().pair;
			consume_box(b);
			_ = n;
		}
	`)
	expectOwnerError(t, errs, "cannot move borrowed value 'b'")
}

// ThisExpr root + non-consuming move (inferred var-decl RHS): `x := this`
// after a destructure-from-this borrow must reject via tryMove(ThisExpr)'s
// borrow check (distinct from the tryMoveConsume(ThisExpr) path covered by
// TestDestructureFromThisConsumeRejected — different call site, different
// error path).
func TestDestructureFromThisMoveLocalRejected(t *testing.T) {
	errs := ownerErrs(t, `
		type _BoxStr { string s; }
		type Holder {
			(_BoxStr, int) pair;
			eat(~this) {
				(b, n) := this.pair;
				x := this;
				_ = b.s;
				_ = n;
				_ = x;
			}
		}
		test() {}
	`)
	expectOwnerError(t, errs, "cannot move 'this' while it is borrowed")
}

// === T0571: destructure-from-temporary-expression is rejected at compile
// time. T0548/T0570 covered destructure sources rooted at a stable variable
// (IdentExpr / ThisExpr), but a MemberExpr/IndexExpr whose root is a transient
// temporary (CallExpr, conditional, error-handler, cast, …) has no anchoring
// owner to extend the borrow's lifetime to. Codegen drops the temp at end of
// the destructure statement (via stmtTemps cleanup), leaving any non-Copy
// destructured local dangling. The fix rejects the pattern up-front in
// checkDestructureVarDecl when destructureBorrowRoot returns "" and any
// non-Copy slot exists. Workaround: bind the source to a local first. ===

// Exact bug repro: `(b, n) := make_holder().pair;` — call-result.field source.
// Without the fix this segfaults at runtime; with it, a clear compile-time
// error fires.
func TestDestructureFromCallExprRejected(t *testing.T) {
	errs := ownerErrs(t, `
		type _BoxStr { string s; }
		type Holder { (_BoxStr, int) pair; }
		make_holder() Holder { return Holder(pair: (_BoxStr(s: "x"), 2)); }
		test() {
			(b, n) := make_holder().pair;
			_ = b.s;
			_ = n;
		}
	`)
	expectOwnerError(t, errs, "cannot destructure from temporary expression")
}

// IndexExpr arm: `(b, n) := make_vec()[0];` — call-result[0] source. The
// CallExpr return is a temp Vector; IndexExpr produces an inner-buffer
// reference that has no anchoring local to constrain its lifetime to.
func TestDestructureFromCallExprViaIndexExprRejected(t *testing.T) {
	errs := ownerErrs(t, `
		type _BoxStr { string s; }
		make_vec() Vector[(_BoxStr, int)] {
			Vector[(_BoxStr, int)] v = [];
			v.push((_BoxStr(s: "x"), 2));
			return v;
		}
		test() {
			(b, n) := make_vec()[0];
			_ = b.s;
			_ = n;
		}
	`)
	expectOwnerError(t, errs, "cannot destructure from temporary expression")
}

// ParenExpr-wrapped repro: `(b, n) := (make_holder().pair);` — the T0570 paren
// peel routes through the MemberExpr arm, then T0571's root check sees the
// inner CallExpr and rejects.
func TestDestructureFromCallExprParenRejected(t *testing.T) {
	errs := ownerErrs(t, `
		type _BoxStr { string s; }
		type Holder { (_BoxStr, int) pair; }
		make_holder() Holder { return Holder(pair: (_BoxStr(s: "x"), 2)); }
		test() {
			(b, n) := (make_holder().pair);
			_ = b.s;
			_ = n;
		}
	`)
	expectOwnerError(t, errs, "cannot destructure from temporary expression")
}

// All-Copy tuple elements: every destructured local is implicitly copied out
// of the temp before it's dropped, so there's no dangling borrow. The
// rejection must not fire.
func TestDestructureFromCallExprAllCopyOK(t *testing.T) {
	ownerOK(t, `
		type Holder { (int, int) pair; }
		make_holder() Holder { return Holder(pair: (1, 2)); }
		test() {
			(a, b) := make_holder().pair;
			_ = a + b;
		}
	`)
}

// All-discard slots: every non-Copy element is `_`, so nothing borrows the
// temp's heap data. The temp drops cleanly at end of statement with no
// dangling reference.
func TestDestructureFromCallExprAllDiscardOK(t *testing.T) {
	ownerOK(t, `
		type _BoxStr { string s; }
		type Holder { (_BoxStr, int) pair; }
		make_holder() Holder { return Holder(pair: (_BoxStr(s: "x"), 2)); }
		test() {
			(_, _) := make_holder().pair;
		}
	`)
}

// Documented workaround: bind the temp to a local first, then destructure
// from the local. T0548's borrow registration anchors the destructured
// locals to the local, which has scope-tied drop ordering — runtime-safe.
func TestDestructureFromCallExprWorkaroundOK(t *testing.T) {
	ownerOK(t, `
		type _BoxStr { string s; }
		type Holder { (_BoxStr, int) pair; }
		make_holder() Holder { return Holder(pair: (_BoxStr(s: "x"), 2)); }
		test() {
			Holder h = make_holder();
			(b, n) := h.pair;
			_ = b.s;
			_ = n;
		}
	`)
}

// Partial Copy: only one slot is non-Copy. A single non-Copy borrow with no
// anchor is enough to UAF, so the rejection still fires.
func TestDestructureFromCallExprPartialCopyRejected(t *testing.T) {
	errs := ownerErrs(t, `
		type _BoxStr { string s; }
		type Holder { (int, _BoxStr) pair; }
		make_holder() Holder { return Holder(pair: (1, _BoxStr(s: "x"))); }
		test() {
			(n, b) := make_holder().pair;
			_ = n;
			_ = b.s;
		}
	`)
	expectOwnerError(t, errs, "cannot destructure from temporary expression")
}

// Chained method call: `obj.method().field` — the inner CallExpr produces a
// temp, walked-up root is the CallExpr, so destructureBorrowRoot returns ""
// and the rejection fires. Mirrors the bare make_holder() case.
func TestDestructureFromChainedCallRejected(t *testing.T) {
	errs := ownerErrs(t, `
		type _BoxStr { string s; }
		type Holder { (_BoxStr, int) pair; }
		type Factory {
			make(this) Holder { return Holder(pair: (_BoxStr(s: "x"), 2)); }
		}
		test() {
			Factory f = Factory();
			(b, n) := f.make().pair;
			_ = b.s;
			_ = n;
		}
	`)
	expectOwnerError(t, errs, "cannot destructure from temporary expression")
}

// Discard-first with non-Copy second slot: `(_, b) := f().pair` exercises the
// "skip _ then encounter non-Copy" path in the rejection loop. Without this
// test, a future change that returns early on the first slot (e.g. checking
// only s.Names[0]) would silently miss this UAF pattern.
func TestDestructureFromCallExprDiscardFirstRejected(t *testing.T) {
	errs := ownerErrs(t, `
		type _BoxStr { string s; }
		type Holder { (_BoxStr, _BoxStr) pair; }
		make_holder() Holder { return Holder(pair: (_BoxStr(s: "x"), _BoxStr(s: "y"))); }
		test() {
			(_, b) := make_holder().pair;
			_ = b.s;
		}
	`)
	expectOwnerError(t, errs, "cannot destructure from temporary expression")
}

// OptionalUnwrapExpr source: `(b, n) := opt!.pair` — the `!` operator produces
// the inner value of the optional but the unwrapped expression is still a
// transient temp with no anchoring local. Falls through destructureBorrowRoot's
// default arm. Regression guard against a future change that adds
// OptionalUnwrapExpr to the walk-down switch without also extending the temp's
// lifetime.
func TestDestructureFromOptionalUnwrapRejected(t *testing.T) {
	errs := ownerErrs(t, `
		type _BoxStr { string s; }
		type Holder { (_BoxStr, int) pair; }
		test() {
			Holder? oh = Holder(pair: (_BoxStr(s: "x"), 2));
			(b, n) := oh!.pair;
			_ = b.s;
			_ = n;
		}
	`)
	expectOwnerError(t, errs, "cannot destructure from temporary expression")
}

// IfExpr source: `(b, n) := (if c { a } else { b }).pair`. Both arms produce
// owned Holder values, the IfExpr's result is a temp dropped at end of
// statement. Falls through destructureBorrowRoot's default arm. Regression
// guard against a future change that adds IfExpr to the walk-down.
func TestDestructureFromIfExprRejected(t *testing.T) {
	errs := ownerErrs(t, `
		type _BoxStr { string s; }
		type Holder { (_BoxStr, int) pair; }
		make_a() Holder { return Holder(pair: (_BoxStr(s: "a"), 1)); }
		make_b() Holder { return Holder(pair: (_BoxStr(s: "b"), 2)); }
		test() {
			bool flag = true;
			(b, n) := (if flag { make_a() } else { make_b() }).pair;
			_ = b.s;
			_ = n;
		}
	`)
	expectOwnerError(t, errs, "cannot destructure from temporary expression")
}

// ErrorPanicExpr source: `(b, n) := f()?!.pair` — the `?!` operator panics on
// error and otherwise produces the inner value. Like OptionalUnwrap, the
// unwrapped expression is a transient temp. Falls through to default arm.
// Regression guard for the failable-expression family of patterns.
func TestDestructureFromErrorPanicRejected(t *testing.T) {
	errs := ownerErrs(t, `
		type _BoxStr { string s; }
		type Holder { (_BoxStr, int) pair; }
		make_holder!() Holder { return Holder(pair: (_BoxStr(s: "x"), 2)); }
		test() {
			(b, n) := make_holder()?!.pair;
			_ = b.s;
			_ = n;
		}
	`)
	expectOwnerError(t, errs, "cannot destructure from temporary expression")
}

// Inside a generic method body, the owner's TypeArgs are still TypeParams.
// The check must skip — preserves the existing "skip on unresolved TypeParam"
// semantics. (Regression guard for the ContainsTypeParam(TypeArg) gate.)
func TestFieldMoveGenericMethodBodyOK(t *testing.T) {
	ownerOK(t, `
		type Holder[T] {
			T? value;
			peek(this) {
				_a := this.value;
			}
		}
		test() {}
	`)
}

// Concrete non-droppable field on a generic owner instantiated with a
// non-droppable TypeArg. Exercises the `continue` (non-TypeParam field) and
// final `return false` paths inside instanceHasDroppableField — origin Holder
// has no drop flags, TypeArgs are concrete, and no substituted field is
// droppable, so the move is allowed.
func TestFieldMoveGenericNoDropConcreteFieldOK(t *testing.T) {
	ownerOK(t, `
		enum Color { Red; Green; Blue; }
		type Holder[T] {
			Color c;
			T v;
		}
		test() {
			Holder[int] h = Holder[int](c: move Color.Red, v: 7);
			Color c = h.c;
		}
	`)
}

// Inside a generic function body, the parameter's type is an Instance whose
// TypeArgs are still TypeParams. Reading a concrete-typed (non-TypeParam)
// field from such an Instance exercises the TypeArg-contains-TypeParam early
// return inside instanceHasDroppableField. Without the guard, substitution
// would run with TypeParam args and produce nonsense. (Generic methods bind
// the receiver as the bare Named, so this path is reached only via generic
// free functions.)
func TestFieldMoveGenericFnBodyConcreteFieldOK(t *testing.T) {
	ownerOK(t, `
		enum Color { Red; Green; Blue; }
		type Holder[T] {
			Color c;
			T v;
		}
		peek_holder[T](Holder[T] h) {
			Color c = h.c;
		}
		test() {}
	`)
}

// Field type is a generic Instance (GenWrap[Color]) whose origin Named has no
// drop flags. Even though Color (the type arg) is non-droppable, the origin
// `GenWrap` itself is a heap user type (non-value, non-structural, non-Copy),
// so codegen's monoTypeHasDroppable returns true via the B0192 catch-all and
// synthesizes a drop that `pal_free`s the heap instance. Without the parallel
// catch-all on the ownership side, `GenWrap[Color] g = o.gw` slips through and
// double-frees at runtime (verified prior to T0549 fix: `fatal: double free`).
func TestFieldMoveGenericInstanceFieldHeapOriginError(t *testing.T) {
	errs := ownerErrs(t, `
		enum Color { Red; Green; Blue; }
		type GenWrap[T] { T inner; }
		type Outer {
			GenWrap[Color] gw;
			drop(~this) {}
		}
		test() {
			Outer o = Outer(gw: GenWrap[Color](inner: Color.Red));
			GenWrap[Color] g = o.gw;
		}
	`)
	expectOwnerError(t, errs, "cannot move field 'gw'")
}

// T0506: `Container[T]{Maybe[T] m}` instantiated with a droppable T must reject
// `Maybe[_BoxDrop] m = c.m;` — sema's fieldTypeHasDrop doesn't see through the
// TypeParam-containing generic enum field, but codegen's monoEnumInstNeedsSynthDrop
// synthesizes a drop for `Maybe[_BoxDrop]`, so without the ownership-side
// Enum-origin branch the move would slip through and double-free at runtime.
func TestFieldMoveGenericEnumDroppableError(t *testing.T) {
	errs := ownerErrs(t, `
		type _BoxDrop {
			int n;
			drop(~this) {}
		}
		enum Maybe[T] {
			Just(T val);
			Nothing;
		}
		type Container[T] { Maybe[T] m; }
		test() {
			Container[_BoxDrop] c = Container[_BoxDrop](m: Maybe[_BoxDrop].Just(_BoxDrop(n: 7)));
			Maybe[_BoxDrop] m = c.m;
		}
	`)
	expectOwnerError(t, errs, "cannot move field 'm'")
}

// T0506: variant payload is a tuple containing a TypeParam (`Both((T, int) data)`)
// — exercises the recursion into Tuple inside isDroppableType, confirming that
// T0505's Tuple case composes correctly with the new Enum case (the enum
// branch resolves to isDroppableType, which then recurses into the tuple
// element types).
func TestFieldMoveGenericEnumDroppableTupleVariantError(t *testing.T) {
	errs := ownerErrs(t, `
		type _BoxDrop {
			int n;
			drop(~this) {}
		}
		enum Pair[T] {
			Both((T, int) data);
			None;
		}
		type Container[T] { Pair[T] p; }
		test() {
			Container[_BoxDrop] c = Container[_BoxDrop](p: Pair[_BoxDrop].Both((_BoxDrop(n: 1), 2)));
			Pair[_BoxDrop] p = c.p;
		}
	`)
	expectOwnerError(t, errs, "cannot move field 'p'")
}

// `Container[int]{Maybe[T] m}` — substituted variant field types are non-droppable.
// Bare field read must remain allowed (negative test: guards against the new
// enumInstanceHasDroppableField producing false positives).
func TestFieldMoveGenericEnumNonDroppableOK(t *testing.T) {
	ownerOK(t, `
		enum Maybe[T] {
			Just(T val);
			Nothing;
		}
		type Container[T] { Maybe[T] m; }
		test() {
			Container[int] c = Container[int](m: Maybe[int].Just(7));
			Maybe[int] m = c.m;
		}
	`)
}

// `enum E[T] { A; B; }` — no variant fields at all. Substituted to a droppable
// T should still be non-droppable. Negative test for the loop-yields-no-droppable
// path inside enumInstanceHasDroppableField.
func TestFieldMoveGenericEnumNoVariantFieldsOK(t *testing.T) {
	ownerOK(t, `
		type _BoxDrop {
			int n;
			drop(~this) {}
		}
		enum E[T] {
			A;
			B;
		}
		type Container[T] { E[T] e; }
		test() {
			Container[_BoxDrop] c = Container[_BoxDrop](e: E[_BoxDrop].A);
			E[_BoxDrop] e = c.e;
		}
	`)
}

// Inside a generic method body, the enum instance's TypeArgs are still TypeParams.
// The TypeArg-contains-TypeParam early return inside enumInstanceHasDroppableField
// must skip — preserves the existing "skip on unresolved TypeParam" semantics
// parallel to TestFieldMoveGenericMethodBodyOK.
func TestFieldMoveGenericEnumInGenericMethodBodyOK(t *testing.T) {
	ownerOK(t, `
		enum Maybe[T] {
			Just(T val);
			Nothing;
		}
		type Container[T] {
			Maybe[T] m;
			peek(this) {
				_a := this.m;
			}
		}
		test() {}
	`)
}

// `Container[T]{Maybe[T]? m}` — Optional wrapping a generic enum instance with
// a TypeParam variant payload, instantiated with a droppable T. Exercises
// composition: isDroppableType's Optional case recurses into Elem, which is an
// Instance with Enum origin, dispatching to enumInstanceHasDroppableField. Without
// this composition working, the move would slip through. (Likely real-world
// pattern: optional generic enum field.)
func TestFieldMoveGenericOptionalEnumDroppableError(t *testing.T) {
	errs := ownerErrs(t, `
		type _BoxDrop {
			int n;
			drop(~this) {}
		}
		enum Maybe[T] {
			Just(T val);
			Nothing;
		}
		type Container[T] { Maybe[T]? m; }
		test() {
			Container[_BoxDrop] c = Container[_BoxDrop](m: Maybe[_BoxDrop].Just(_BoxDrop(n: 7)));
			Maybe[_BoxDrop]? x = c.m;
		}
	`)
	expectOwnerError(t, errs, "cannot move field 'm'")
}

// `Container[T]{(Maybe[T], int) p}` — Tuple element is a generic enum instance.
// Exercises composition: isDroppableType's Tuple case iterates elements, hitting
// the Instance/Enum branch on the first element. Confirms the new Enum-origin
// branch composes correctly with the Tuple recursion path (the inverse of
// TestFieldMoveGenericEnumDroppableTupleVariantError, which exercises Enum→Tuple).
func TestFieldMoveGenericTupleContainingEnumDroppableError(t *testing.T) {
	errs := ownerErrs(t, `
		type _BoxDrop {
			int n;
			drop(~this) {}
		}
		enum Maybe[T] {
			Just(T val);
			Nothing;
		}
		type Container[T] { (Maybe[T], int) p; }
		test() {
			Container[_BoxDrop] c = Container[_BoxDrop](p: (Maybe[_BoxDrop].Just(_BoxDrop(n: 1)), 2));
			(Maybe[_BoxDrop], int) p = c.p;
		}
	`)
	expectOwnerError(t, errs, "cannot move field 'p'")
}

// `Container[T]{Maybe[Maybe[T]] m}` — nested generic enum instance. The outer
// Maybe[Maybe[_BoxDrop]] variant carries a Maybe[_BoxDrop] payload, which must
// itself be detected as droppable via the recursive enumInstanceHasDroppableField
// call from within isDroppableType. Without recursion working through the enum
// branch, the move would slip through.
func TestFieldMoveGenericNestedEnumDroppableError(t *testing.T) {
	errs := ownerErrs(t, `
		type _BoxDrop {
			int n;
			drop(~this) {}
		}
		enum Maybe[T] {
			Just(T val);
			Nothing;
		}
		type Container[T] { Maybe[Maybe[T]] m; }
		test() {
			Container[_BoxDrop] c = Container[_BoxDrop](m: Maybe[Maybe[_BoxDrop]].Just(Maybe[_BoxDrop].Just(_BoxDrop(n: 7))));
			Maybe[Maybe[_BoxDrop]] m = c.m;
		}
	`)
	expectOwnerError(t, errs, "cannot move field 'm'")
}

// Variant `Just(int tag, T val)` mixes a concrete (non-TypeParam) field with a
// TypeParam-containing field. The concrete int tag triggers the `continue`
// path inside enumInstanceHasDroppableField (sema already accounted for it via
// the origin's flags), then T val is checked, substituted to _BoxDrop, and
// found droppable. Exercises both the continue and the return-true branches in
// a single test.
func TestFieldMoveGenericEnumMixedFieldVariantError(t *testing.T) {
	errs := ownerErrs(t, `
		type _BoxDrop {
			int n;
			drop(~this) {}
		}
		enum Tagged[T] {
			Just(int tag, T val);
			Nothing;
		}
		type Container[T] { Tagged[T] m; }
		test() {
			Container[_BoxDrop] c = Container[_BoxDrop](m: Tagged[_BoxDrop].Just(7, _BoxDrop(n: 1)));
			Tagged[_BoxDrop] m = c.m;
		}
	`)
	expectOwnerError(t, errs, "cannot move field 'm'")
}

// === T0549: plain Named / Instance field-move with B0192 catch-all ===

// `_Plain { int n; }` has no drop method and only primitive fields, so
// sema's fieldTypeHasDrop and the Named's HasDrop/NeedsSynthDrop flags are
// all false. But codegen treats it as a heap user type (B0192 catch-all in
// monoTypeHasDroppable) and emits `pal_free` for it both inside `_Outer`'s
// synth drop and at the moved local's scope exit — a runtime double-free.
// The new B0192 catch-all in isDroppableType rejects the move at compile time.
func TestFieldMovePlainNamedFromDroppableError(t *testing.T) {
	errs := ownerErrs(t, `
		type _Plain { int n; }
		type _Outer {
			_Plain inner;
			drop(~this) {}
		}
		test() {
			_Outer o = _Outer(inner: _Plain(n: 1));
			_Plain p = o.inner;
		}
	`)
	expectOwnerError(t, errs, "cannot move field 'inner'")
}

// Same shape but the field type is a generic Instance whose origin is a plain
// heap user type (`_Plain[U] { U n; }` instantiated as `_Plain[int]`). The
// instance has only a primitive field after substitution, so
// `instanceHasDroppableField` returns false — only the new Instance-branch
// B0192 catch-all on the origin catches it.
func TestFieldMoveGenericInstancePlainOriginError(t *testing.T) {
	errs := ownerErrs(t, `
		type _Plain[U] { U n; }
		type _Outer {
			_Plain[int] inner;
			drop(~this) {}
		}
		test() {
			_Outer o = _Outer(inner: _Plain[int](n: 1));
			_Plain[int] p = o.inner;
		}
	`)
	expectOwnerError(t, errs, "cannot move field 'inner'")
}

// `_Pt` is a value type — all fields `value, so it's inlined and has no
// heap drop. The new B0192 catch-all must exclude IsValueType via the
// `!t.IsValueType()` guard. Negative guard.
func TestFieldMovePlainValueTypeFieldOK(t *testing.T) {
	ownerOK(t, `
		type _Pt { int x `+"`value"+`; int y `+"`value"+`; }
		type _Outer {
			_Pt inner;
			drop(~this) {}
		}
		test() {
			_Outer o = _Outer(inner: _Pt(x: 1, y: 2));
			_Pt p = o.inner;
		}
	`)
}

// `_PtCopy `copy { ... }` is a Copy Named — auto-copied on assignment,
// no heap drop. The field-move check filters Copy types upstream
// (`isCopyType(fieldType)` returns true at line ~930), but the new
// catch-all also excludes IsCopy via `!isCopyType(t)`. Negative guard.
func TestFieldMovePlainCopyTypeFieldOK(t *testing.T) {
	ownerOK(t, `
		type _PtCopy `+"`copy"+` { int x; int y; }
		type _Outer {
			_PtCopy inner;
			drop(~this) {}
		}
		test() {
			_Outer o = _Outer(inner: _PtCopy(x: 1, y: 2));
			_PtCopy p = o.inner;
		}
	`)
}

// Field type is a generic Instance whose origin Named is a heap user type AND
// whose substituted TypeParam-bearing field is itself droppable
// (`GenWrap[map[string,string]]`). Both `instanceHasDroppableField` (via the
// droppable substituted field) AND the B0192 catch-all (via the heap origin)
// independently return true here — the catch-all subsumes the middle clause.
// Regression guard: ensure the move stays rejected when both paths agree, so
// any future simplification (removing the now-redundant `instanceHasDroppableField`
// call inside isDroppableType's Instance branch) still preserves correctness.
func TestFieldMoveGenericInstanceDroppableSubstFieldError(t *testing.T) {
	errs := ownerErrs(t, `
		type GenWrap[T] { T inner; }
		type Outer {
			GenWrap[map[string, string]] gw;
			drop(~this) {}
		}
		test() {
			map[string, string] m = map[string, string]();
			Outer o = Outer(gw: GenWrap[map[string, string]](inner: m));
			GenWrap[map[string, string]] g = o.gw;
		}
	`)
	expectOwnerError(t, errs, "cannot move field 'gw'")
}

// Field type is a generic enum Instance whose origin has HasDrop/NeedsSynthDrop
// set (via T0102: a variant has a concrete droppable string field at the generic
// level). Exercises the
// `case *types.Instance: ... if e, ok := t.Origin().(*types.Enum); ok { if e.HasDrop() ... return true }`
// branch inside isDroppableType, which had no test coverage prior — this
// branch must short-circuit before falling through to
// `enumInstanceHasDroppableField` (which only inspects substituted TypeParam
// fields). Without this short-circuit, an enum with a concrete-typed droppable
// variant field would slip through whenever none of its TypeParam fields
// substitute to droppable types.
func TestFieldMoveGenericEnumInstanceOriginHasDropError(t *testing.T) {
	errs := ownerErrs(t, `
		enum E[T] {
			Just(T x, string s);
			Nothing;
		}
		type Outer {
			E[int] e;
			drop(~this) {}
		}
		test() {
			Outer o = Outer(e: E[int].Just(7, "tag"));
			E[int] e = o.e;
		}
	`)
	expectOwnerError(t, errs, "cannot move field 'e'")
}

// === T0338: borrowed parameter cannot be moved ===

// Bug repro: moving a non-~ param into a constructor field is rejected.
func TestT0338_MoveBorrowedParamIntoConstructor(t *testing.T) {
	errs := ownerErrs(t, `
		type Box {
			u8[] data;
			new(~this, u8[] move d) { this.data = d; }
		}
		_take(u8[] data) int {
			Box b = Box(d: data);
			return 0;
		}
		test() {}
	`)
	expectOwnerError(t, errs, "cannot move borrowed parameter 'data'")
}

// Reading a non-~ param via a method call is fine — borrowed reads are OK.
func TestT0338_BorrowedParamReadOK(t *testing.T) {
	ownerOK(t, `
		_read(string s) int { return 1; }
		test() {
			string a = "hi";
			_read(a);
		}
	`)
}

// Returning a non-~ param by value is allowed — codegen emits a B0345
// post-call alias check that clears the caller's drop flag if the return
// value aliases the arg.
func TestT0338_ReturnBorrowedParamOK(t *testing.T) {
	ownerOK(t, `
		identity(string s) string { return s; }
		test() {
			string a = "hi";
			string b = identity(a);
		}
	`)
}

// Passing a non-~ param to a ~ callee is rejected.
func TestT0338_PassBorrowedToConsume(t *testing.T) {
	errs := ownerErrs(t, `
		consume(string move s) {}
		forward(string s) { consume(s); }
		test() {}
	`)
	expectOwnerError(t, errs, "cannot move borrowed parameter 's'")
}

// Plain `this` (non-`~`, non-`&`) cannot itself be moved into a `~` callee.
func TestT0338_MovePlainThis(t *testing.T) {
	errs := ownerErrs(t, `
		consume(Box move b) {}
		type Box {
			int x;
			leak(this) { consume(this); }
		}
		test() {}
	`)
	expectOwnerError(t, errs, "cannot consume 'this'")
}

// T0569: `~this` does NOT allow moving the receiver into a ~ callee — the
// value still belongs to the caller, so a consume from inside the body
// would double-free at the caller's scope exit.
func TestT0569_MutThisCannotBeConsumed(t *testing.T) {
	errs := ownerErrs(t, `
		consume(Box move b) {}
		type Box {
			int x;
			into(~this) { consume(this); }
		}
		test() {}
	`)
	expectOwnerError(t, errs, "cannot consume 'this'")
}

// T0569 regression guard: calling a `~this` method on the receiver (as the
// receiver position, not as an argument) is the de-facto borrow pattern
// used widely in stdlib. This must keep compiling.
func TestT0569_MutThisReceiverChaining(t *testing.T) {
	ownerOK(t, `
		type Counter {
			int n;
			bump(~this) { this.n = this.n + 1; }
			run(~this) {
				this.bump();
				this.bump();
			}
		}
		test() {}
	`)
}

// T0569 regression guard: a `~this` method body can still mutate fields.
func TestT0569_MutThisFieldWrite(t *testing.T) {
	ownerOK(t, `
		type Counter {
			int n;
			reset(~this) { this.n = 0; }
		}
		test() {}
	`)
}

// T0569 consume-site coverage: tuple literal containing `this` routes
// through tryMoveConsume(elem) at expr.go's TupleLit branch and must reject.
func TestT0569_MutThisInTupleLit(t *testing.T) {
	errs := ownerErrs(t, `
		type Box {
			int x;
			into(~this) {
				(Box, int) p = (this, 1);
			}
		}
		test() {}
	`)
	expectOwnerError(t, errs, "cannot consume 'this'")
}

// T0569 consume-site coverage: array literal element via tryMoveConsume.
func TestT0569_MutThisInArrayLit(t *testing.T) {
	errs := ownerErrs(t, `
		type Box {
			int x;
			into(~this) {
				Box[] arr = [this];
			}
		}
		test() {}
	`)
	expectOwnerError(t, errs, "cannot consume 'this'")
}

// T0569 consume-site coverage: map literal value via tryMoveConsume.
func TestT0569_MutThisInMapLit(t *testing.T) {
	errs := ownerErrs(t, `
		type Box {
			int x;
			into(~this) {
				map[int, Box] m = {1: this};
			}
		}
		test() {}
	`)
	expectOwnerError(t, errs, "cannot consume 'this'")
}

// T0569 consume-site coverage: assignment to an existing local routes
// through tryMoveConsume in checkAssignStmt — must reject `y = this`.
func TestT0569_MutThisAssignToExisting(t *testing.T) {
	errs := ownerErrs(t, `
		type Box {
			int x;
			into(~this) {
				Box y = Box(x: 0);
				y = this;
			}
		}
		test() {}
	`)
	expectOwnerError(t, errs, "cannot consume 'this'")
}

// T0569 consume-site coverage: tryMoveConsume's Moved-state branch on
// ThisExpr. Production code never sets state["this"] = Moved (the fix
// errors immediately instead of transitioning state), so this branch is
// only reachable via direct unit-level construction — but it remains as
// defense-in-depth, so exercise it explicitly.
func TestT0569_TryMoveConsumeThisMoved(t *testing.T) {
	c := newUnitChecker()
	c.state["this"] = Moved
	c.tryMoveConsume(&ast.ThisExpr{})
	expectOwnerError(t, c.errors, "use of moved variable 'this'")
}

// --- T0576 ---
// T0576: binding `this` to a fresh local via a typed var decl in a `~this`
// method body crashes codegen (the receiver `i8*` is stored into a value-
// struct slot). Sema must reject before codegen ever sees it.
func TestT0576_TypedVarDeclMutThisRejected(t *testing.T) {
	errs := ownerErrs(t, `
		type Box { int x; }
		type Holder {
			Box b;
			eat(~this) {
				Holder x = this;
			}
		}
		test() {}
	`)
	expectOwnerError(t, errs, "cannot consume 'this'")
}

// T0576: same crash on the inferred-var-decl path (`x := this;`).
func TestT0576_InferredVarDeclMutThisRejected(t *testing.T) {
	errs := ownerErrs(t, `
		type Box { int x; }
		type Holder {
			Box b;
			eat(~this) {
				x := this;
			}
		}
		test() {}
	`)
	expectOwnerError(t, errs, "cannot consume 'this'")
}

// T0576: same crash on plain `this` (no `~`) — receiver still belongs to
// caller, and codegen still mismatches the value-struct shape.
func TestT0576_TypedVarDeclPlainThisRejected(t *testing.T) {
	errs := ownerErrs(t, `
		type Box { int x; }
		type Counter {
			Box b;
			read(this) {
				Counter c = this;
			}
		}
		test() {}
	`)
	expectOwnerError(t, errs, "cannot consume 'this'")
}

// T0576: same crash on inferred-var-decl with plain `this`.
func TestT0576_InferredVarDeclPlainThisRejected(t *testing.T) {
	errs := ownerErrs(t, `
		type Box { int x; }
		type Counter {
			Box b;
			read(this) {
				c := this;
			}
		}
		test() {}
	`)
	expectOwnerError(t, errs, "cannot consume 'this'")
}

// T0576: pure value-type receivers also crash today (the destination
// alloca expects `{i8*, field…}` but codegen stores the raw `i8*`).
// Reject for consistency — user should call `.clone()` or construct
// manually.
func TestT0576_TypedVarDeclCopyThisRejected(t *testing.T) {
	errs := ownerErrs(t, `
		type Pair {
			int x `+"`value"+`;
			int y `+"`value"+`;
			peek(this) {
				Pair p = this;
			}
		}
		test() {}
	`)
	expectOwnerError(t, errs, "cannot consume 'this'")
}

// T0576: borrow-state branch — destructure-from-this first registers a
// shared borrow on "this"; the subsequent typed-var-decl move must hit
// the "cannot move 'this' while it is borrowed" branch of the new check.
// (`TestDestructureFromThisMoveLocalRejected` covers the inferred form.)
func TestT0576_VarDeclThisInBorrowedState(t *testing.T) {
	errs := ownerErrs(t, `
		type _BoxStr { string s; }
		type Holder {
			(_BoxStr, int) pair;
			eat(~this) {
				(b, n) := this.pair;
				Holder x = this;
				_ = b.s;
				_ = n;
				_ = x;
			}
		}
		test() {}
	`)
	expectOwnerError(t, errs, "cannot move 'this' while it is borrowed")
}

// T0576 regression guard: `return this;` from a `~this` method must keep
// compiling. The return path is wrapped by `wrapThisReturnValue` and the
// caller's drop flag is cleared via B0250 — moving `this` is the one
// place where it is semantically defensible.
func TestT0576_ReturnThisStillCompiles(t *testing.T) {
	ownerOK(t, `
		type Box { int x; }
		type Holder {
			Box b;
			eat(~this) Holder { return this; }
		}
		test() {}
	`)
}

// T0576 regression guard: `.clone()` is the recommended workaround — the
// suggested fix path must compile cleanly.
func TestT0576_CloneWorkaroundCompiles(t *testing.T) {
	ownerOK(t, `
		type Box {
			int x;
			clone(this) Box { return Box(x: this.x); }
		}
		type Holder {
			Box b;
			clone(this) Holder { return Holder(b: this.b.clone()); }
			eat(~this) {
				Holder x = this.clone();
			}
		}
		test() {}
	`)
}

// T0576 regression guard: calling a `~this` method while passing a
// borrow of `this` as the receiver chain (the de-facto pattern used by
// stdlib `string.format!`/`encode!`) must keep compiling — the var-decl
// rejection must not leak into the receiver chain.
func TestT0576_ReceiverChainThisStillCompiles(t *testing.T) {
	ownerOK(t, `
		type Box {
			int x;
			bump(~this) { this.x = this.x + 1; }
			run(~this) {
				this.bump();
				this.bump();
			}
		}
		test() {}
	`)
}

// T0576 defense-in-depth: the Moved-state branch in the new ThisExpr
// short-circuit (checkTypedVarDecl) is unreachable through normal sema
// — production never sets state["this"] = Moved (the new check errors
// immediately instead of transitioning state, mirroring T0569). Exercise
// it directly via newUnitChecker, parallel to TestT0569_TryMoveConsume-
// ThisMoved.
func TestT0576_TypedVarDeclThisMovedBranch(t *testing.T) {
	c := newUnitChecker()
	c.state["this"] = Moved
	c.checkTypedVarDecl(&ast.TypedVarDecl{Name: "x", Value: &ast.ThisExpr{}})
	expectOwnerError(t, c.errors, "use of moved variable 'this'")
}

// T0576 defense-in-depth: same as above for the inferred var decl path
// (`x := this`).
func TestT0576_InferredVarDeclThisMovedBranch(t *testing.T) {
	c := newUnitChecker()
	c.state["this"] = Moved
	c.checkInferredVarDecl(&ast.InferredVarDecl{Name: "x", Value: &ast.ThisExpr{}})
	expectOwnerError(t, c.errors, "use of moved variable 'this'")
}

// --- T0593 ---
// T0593: `use x = this` in a `~this` method body — the use-binding alloca
// expects a value struct {i8*, i8*} but `this` is a raw i8* instance pointer;
// storing it crashes codegen with "store operands are not compatible".
// Must be rejected at the ownership stage, the same way typed/inferred var
// decls were guarded by T0576.
func TestT0593_UseVarDeclMutThisRejected(t *testing.T) {
	errs := ownerErrs(t, `
		type Res {
			int id;
			close(~this) {}
			eat(~this) {
				use x := this;
			}
		}
		test() {}
	`)
	expectOwnerError(t, errs, "cannot consume 'this'")
}

// T0593: same crash on plain `this` (non-mutable) receiver.
func TestT0593_UseVarDeclPlainThisRejected(t *testing.T) {
	errs := ownerErrs(t, `
		type Res {
			int id;
			close(~this) {}
			read(this) {
				use x := this;
			}
		}
		test() {}
	`)
	expectOwnerError(t, errs, "cannot consume 'this'")
}

// T0593 regression guard: `return this;` from a `~this` method must keep
// compiling. Codegen's wrapThisReturnValue wraps the i8* into the correct
// value struct, and maybeClearReceiverDropFlag prevents double-free (B0250).
func TestT0593_ReturnThisMutStillCompiles(t *testing.T) {
	ownerOK(t, `
		type Box { int x; }
		type Holder {
			Box b;
			eat(~this) Holder { return this; }
		}
		test() {}
	`)
}

// T0593: defense-in-depth — Moved-state branch for use-var-decl path.
// Mirroring T0576's defense-in-depth tests for typed/inferred var decls.
func TestT0593_UseVarDeclThisMovedBranch(t *testing.T) {
	c := newUnitChecker()
	c.state["this"] = Moved
	c.checkStmt(&ast.UseVarDecl{Name: "x", Value: &ast.ThisExpr{}})
	expectOwnerError(t, c.errors, "use of moved variable 'this'")
}

// T0593: borrow-state branch for use-var-decl path — exercise via unit
// checker to avoid NLL expiry complexity, mirroring TestT0576_VarDeclThisInBorrowedState.
func TestT0593_UseVarDeclThisBorrowedBranch(t *testing.T) {
	c := newUnitChecker()
	c.borrows = NewBorrowSet()
	c.borrows.Add(&Borrow{Borrower: "tmp", Origin: "this", Kind: BorrowShared})
	c.checkStmt(&ast.UseVarDecl{Name: "x", Value: &ast.ThisExpr{}})
	expectOwnerError(t, c.errors, "cannot move 'this' while it is borrowed")
}

// T0593: borrow-state branch for tryMove(ThisExpr) — the path reached via
// ReturnStmt when `this` has an active borrow (e.g., from destructuring
// this.field). The UseVarDecl guard fires before tryMove, so this unit test
// exercises the tryMove path directly via a ReturnStmt.
func TestT0593_ReturnThisBorrowedBranch(t *testing.T) {
	c := newUnitChecker()
	c.borrows = NewBorrowSet()
	c.borrows.Add(&Borrow{Borrower: "tmp", Origin: "this", Kind: BorrowShared})
	c.checkStmt(&ast.ReturnStmt{Value: &ast.ThisExpr{}})
	expectOwnerError(t, c.errors, "cannot move 'this' while it is borrowed")
}

// Copy-type params are unaffected by the borrowed-param check.
func TestT0338_CopyParamMovable(t *testing.T) {
	ownerOK(t, `
		f(int x) int { int y = x; return y; }
		test() { f(1); }
	`)
}

// `&` typed param remains borrowed (existing behavior, re-confirm).
func TestT0338_RefParamBorrowed(t *testing.T) {
	ownerOK(t, `
		f(string s) int { return 1; }
		test() {
			string a = "hi";
			f(a);
			int n = 1;
		}
	`)
}

// Local owned values can still be moved — only parameters are borrowed.
func TestT0338_LocalOwnedMovable(t *testing.T) {
	ownerOK(t, `
		consume(string move s) {}
		test() {
			string s = "hi";
			consume(move s);
		}
	`)
}

// `~param` allows the callee to move the value into a constructor field.
func TestT0338_MutParamConsumableInConstructor(t *testing.T) {
	ownerOK(t, `
		type Box {
			u8[] data;
			new(~this, u8[] move d) { this.data = d; }
		}
		_take(u8[] move data) int {
			Box b = Box(d: move data);
			return 0;
		}
		test() {
			u8[] v = u8[]();
			_take(move v);
		}
	`)
}

// Methods that mutate `this.field = v` via plain receiver are still legal —
// no move of `this` itself occurs.
func TestT0338_PlainThisFieldAssignOK(t *testing.T) {
	ownerOK(t, `
		type T {
			int x;
			set_x(this, int v) { this.x = v; }
		}
		test() {}
	`)
}

// Setter parameters are implicitly consumed (codegen clears caller's drop
// flag at the property assignment), so moving the value into the field is OK.
func TestT0338_SetterParamConsumable(t *testing.T) {
	ownerOK(t, `
		type Box {
			string _inner;
			get inner string { return this._inner; }
			set inner(string v) { this._inner = v; }
		}
		test() {
			Box b = Box(_inner: "");
			string s = "hi";
			b.inner = s;
		}
	`)
}

// Variadic parameters are owned by the callee (synthesized vector).
func TestT0338_VariadicParamOwned(t *testing.T) {
	ownerOK(t, `
		consume(int[] move v) {}
		sum(...int nums) {
			consume(move nums);
		}
		test() {}
	`)
}

// `move` lambda capture of a borrowed param is rejected — same double-free
// pattern as moving into a constructor field.
func TestT0338_LambdaMoveCaptureBorrowedParam(t *testing.T) {
	errs := ownerErrs(t, `
		consume(string move s) {}
		f(string s) {
			g := move || -> int {
				consume(s);
				return 1;
			};
			int n = g();
		}
		test() {}
	`)
	expectOwnerError(t, errs, "cannot move-capture borrowed parameter 's' into a lambda")
}

// `move` lambda capture of an owned local is fine.
func TestT0338_LambdaMoveCaptureOwnedLocal(t *testing.T) {
	ownerOK(t, `
		consume(string move s) {}
		test() {
			string s = "hi";
			g := move || -> int {
				consume(move s);
				return 1;
			};
			int n = g();
		}
	`)
}

// Consuming an owned local that has an active stored borrow must error
// inside tryMoveConsume — exercises the HasAnyBorrow check on the consuming
// path (the equivalent on tryMove was already covered by
// TestStoredBorrowBlocksMove). Both paths must enforce the same invariant.
func TestT0338_ConsumeOwnedLocalWhileBorrowed(t *testing.T) {
	errs := ownerErrs(t, `
		getRef(string s) string& { return s; }
		consume(string move s) {}
		test() {
			string s = "hello";
			string &r = getRef(s);
			consume(s);
			string &r2 = r;
		}
	`)
	expectOwnerError(t, errs, "cannot move 's' while it is borrowed")
}

// Borrowed parameters that are read in both branches of an if/else must
// remain Borrowed after the merge, so a consuming use after the if/else
// still errors. Exercises the Borrowed fixed-point branch in state.merge.
func TestT0338_MergeBorrowedThenConsume(t *testing.T) {
	errs := ownerErrs(t, `
		consume(string move s) {}
		_use(string s, bool flag) {
			if (flag) {
				int n = s.len;
			} else {
				int m = s.len;
			}
			consume(s);
		}
		test() {}
	`)
	expectOwnerError(t, errs, "cannot move borrowed parameter 's'")
}

// Constructor calls go through the unresolved-callee branch of
// checkCallExpr (sig is nil). Exercises tryMoveConsume on a borrowed
// parameter passed by name to a constructor — same double-free pattern
// as the bug repro but via named-argument syntax.
func TestT0338_ConstructorNamedArgBorrowedParam(t *testing.T) {
	errs := ownerErrs(t, `
		type Box {
			string s;
			new(~this, string move s) { this.s = s; }
		}
		_take(string s) {
			Box b = Box(s: s);
		}
		test() {}
	`)
	expectOwnerError(t, errs, "cannot move borrowed parameter 's'")
}

// Tuple/array/map literals use tryMoveConsume on each element — verify
// rejecting a borrowed param being captured into a vector literal.
func TestT0338_VectorLitBorrowedParam(t *testing.T) {
	errs := ownerErrs(t, `
		_take(string s) {
			string[] v = [s];
		}
		test() {}
	`)
	expectOwnerError(t, errs, "cannot move borrowed parameter 's'")
}

// === T0634: generic enum-variant constructor rejects borrowed-param move ===

// `mk_holder[T](T x) Holder[T] { return Holder[T].Wrap(x); }` — sema types the
// `Holder[T].Wrap` callee as a synthetic *types.Signature, so ownership used to
// take the permissive function-call path; with T a bare *types.TypeParam,
// isDroppableType returned false and the borrowed move was silently allowed →
// codegen aliased the caller's value into the variant payload → double-free
// (surfaced as `fatal: stack overflow` for T=map[string,int]). The enum-variant
// constructor must consume its arg like a struct constructor and reject a
// borrowed parameter.
func TestT0634_GenericEnumVariantCtorBorrowedParamRejected(t *testing.T) {
	errs := ownerErrs(t, `
		enum Holder[T] { Wrap(T v), Nada, }
		mk_holder[T](T x) Holder[T] { return Holder[T].Wrap(x); }
		test() {}
	`)
	expectOwnerError(t, errs, "cannot move borrowed parameter 'x'")
}

// The required/correct form: `~T x` transfers ownership to the callee, so the
// move into the variant is sound. Must compile cleanly.
func TestT0634_GenericEnumVariantCtorMutParamOK(t *testing.T) {
	ownerOK(t, `
		enum Holder[T] { Wrap(T v), Nada, }
		mk_holder[T](T move x) Holder[T] { return Holder[T].Wrap(move x); }
		test() {}
	`)
}

// Regression guard: the non-generic enum-variant constructor was already
// rejected (concrete arg type → isDroppableType true). The new
// enum-variant-ctor detector must keep producing the same diagnostic.
func TestT0634_NonGenericEnumVariantCtorBorrowedParamRejected(t *testing.T) {
	errs := ownerErrs(t, `
		enum Payload { Data(map[string, int] m), Empty, }
		wrap_it(map[string, int] x) Payload { return Payload.Data(x); }
		test() {}
	`)
	expectOwnerError(t, errs, "cannot move borrowed parameter 'x'")
}

// An owned local consumed into a generic enum-variant constructor is valid:
// the local is moved into the variant payload (no aliasing). Must compile.
func TestT0634_GenericEnumVariantCtorOwnedLocalOK(t *testing.T) {
	ownerOK(t, `
		enum Holder[T] { Wrap(T v), Nada, }
		mk_holder[T](T move x) Holder[T] {
			T local = x;
			return Holder[T].Wrap(move local);
		}
		test() {}
	`)
}

// The targeted fix must not over-reject generic pass-through (a `return`, not
// an enum-variant constructor). The rejected blanket
// isDroppableType(TypeParam)=true alternative broke exactly this shape.
func TestT0634_GenericPassThroughStillOK(t *testing.T) {
	ownerOK(t, `
		identity[T](T val) T { return val; }
		test() {}
	`)
}

// Regression guard for the explicit no-regression claim in
// isEnumVariantConstructorCallee: an enum *method* call
// (`enumValue.method(arg…)`) is NOT an enum-variant constructor — the member's
// Target resolves (via extractEnumForMatch) to the enum, but
// LookupVariant(methodName) returns nil → the detector returns false → the
// call takes the normal function-call (Signature) path, not tryMoveConsume.
// A borrowed argument bound to a *borrowing* enum-method parameter must
// therefore NOT be rejected. If the detector wrongly matched enum-method
// callees, this borrowed `s` would be routed through tryMoveConsume and
// rejected with "cannot move borrowed parameter" — silently breaking all enum
// method dispatch. Exercises the false outcome of expr.go:994 (the
// TestT0634_*BorrowedParamRejected cases exercise only the true outcome). (T0634)
func TestT0634_EnumMethodCallNotTreatedAsVariantCtor(t *testing.T) {
	ownerOK(t, `
		enum Tag {
			A, B,
			label(this, string note) string { return note; }
		}
		use_method(string s) string {
			Tag t = Tag.A;
			return t.label(s);
		}
		test() {}
	`)
}

// Companion to the above on a *generic* enum instance: the bug was specific to
// generic enum-variant ctors (arg type is a bare *types.TypeParam). A method
// call on a generic enum instance must likewise route through normal dispatch
// (extractEnumForMatch resolves the *types.Instance's Enum origin;
// LookupVariant(methodName)==nil → false), so a borrowed arg into a borrowing
// method parameter is accepted. Guards against the detector over-matching
// generic enum *method* callees. (T0634)
func TestT0634_GenericEnumMethodCallNotTreatedAsVariantCtor(t *testing.T) {
	ownerOK(t, `
		enum Holder[T] {
			Wrap(T v), Nada,
			tag_of(this, string s) string { return s; }
		}
		use_it(string label) string {
			Holder[int] h = Holder[int].Nada;
			return h.tag_of(label);
		}
		test() {}
	`)
}

// === T0556: borrowed non-duppable single-owner handles into call args ===

// Mutex[T] has no clone/dup semantics. Without rejection, the callee's
// push consumes the value, the callee scope-exit drops the vector (and its
// Mutex element), and the caller's drop fires on the same allocation →
// runtime double-free. Sema must reject the move.
func TestT0556_PushBorrowedMutexParamRejected(t *testing.T) {
	errs := ownerErrs(t, `
		take_mutex_push(Mutex[int] m) {
			outer := Vector[Mutex[int]]();
			outer.push(m);
		}
		test() {}
	`)
	expectOwnerError(t, errs, "cannot move borrowed parameter 'm'")
}

// Task[T] is also a single-owner native handle with no dup path.
func TestT0556_PushBorrowedTaskParamRejected(t *testing.T) {
	errs := ownerErrs(t, `
		worker() int { return 42; }
		take_task_push(Task[int] t) {
			outer := Vector[Task[int]]();
			outer.push(t);
		}
		test() {}
	`)
	expectOwnerError(t, errs, "cannot move borrowed parameter 't'")
}

// MutexGuard[T] cannot be duped either (locking is exclusive).
func TestT0556_PushBorrowedMutexGuardParamRejected(t *testing.T) {
	errs := ownerErrs(t, `
		take_guard_push(MutexGuard[int] g) {
			outer := Vector[MutexGuard[int]]();
			outer.push(g);
		}
		test() {}
	`)
	expectOwnerError(t, errs, "cannot move borrowed parameter 'g'")
}

// With `~Mutex[int] m`, the caller transfers ownership at the call site
// (drop flag cleared), and the callee may consume it. No double-free.
func TestT0556_PushMutMutexParamOK(t *testing.T) {
	ownerOK(t, `
		take_mutex_push(Mutex[int] move m) {
			outer := Vector[Mutex[int]]();
			outer.push(move m);
		}
		test() {
			m := Mutex[int](42);
			take_mutex_push(move m);
		}
	`)
}

// Ref[T] is duppable (refcount inc), so push of a borrowed Arc param is
// still allowed — codegen emits dupArc at the call site. Regression guard.
func TestT0556_PushBorrowedArcParamOK(t *testing.T) {
	ownerOK(t, `
		take_arc_push(Ref[int] a) {
			outer := Vector[Ref[int]]();
			outer.push(a);
		}
		test() {
			a := Ref[int](7);
			take_arc_push(a);
		}
	`)
}

// T1102: `return m` for a borrowed (non-`move`) Mutex param is unsound — the
// handle has no clone, so the caller's result aliases its still-live source
// local and both ends drop the one handle (double-free / UAF). Formerly this
// relied on codegen's B0345 alias-clear, which only transfers sole ownership
// for clonable types; for a single-owner handle the surviving owner double-
// frees. Now rejected at the return site.
func TestT0556_ReturnBorrowedMutexParamRejected(t *testing.T) {
	errs := ownerErrs(t, `
		identity(Mutex[int] m) Mutex[int] { return m; }
		test() {
			m := Mutex[int](42);
			m2 := identity(m);
		}
	`)
	expectOwnerError(t, errs, "cannot return borrowed parameter")
}

// T1102: `n := m` aliasing a borrowed Mutex param into an owned local is
// rejected — a single-owner handle has no clone/dup, so the alias is unsound
// the moment it escapes (and pointless even when it does not). Same shape as
// the typed-decl carve-outs above, via the inferred var-decl path.
func TestT0556_VarDeclBorrowedMutexParamRejected(t *testing.T) {
	errs := ownerErrs(t, `
		alias(Mutex[int] m) {
			n := m;
		}
		test() {}
	`)
	expectOwnerError(t, errs, "cannot move borrowed parameter")
}

// Transparent wrappers must not let a borrowed Mutex slip past the check.
// Without unwrapping ParenExpr, `v.push((m))` segfaults at runtime — the
// outer ParenExpr is neither an IdentExpr nor handled by tryMove.
func TestT0556_PushParenWrappedMutexParamRejected(t *testing.T) {
	errs := ownerErrs(t, `
		take_paren(Mutex[int] m) {
			v := Vector[Mutex[int]]();
			v.push((m));
		}
		test() {}
	`)
	expectOwnerError(t, errs, "cannot move borrowed parameter 'm'")
}

// If-expression branches that return a borrowed Mutex must also be
// rejected — both branches produce the same aliased pointer the caller
// will drop.
func TestT0556_PushIfWrappedMutexParamRejected(t *testing.T) {
	errs := ownerErrs(t, `
		take_if(Mutex[int] m, bool flag) {
			v := Vector[Mutex[int]]();
			v.push(if flag { m } else { m });
		}
		test() {}
	`)
	expectOwnerError(t, errs, "cannot move borrowed parameter 'm'")
}

// Coverage: when only the Else branch surfaces the borrowed Mutex param,
// the walk must fall through to checking Else (the Then branch returns
// nil because make_mutex() is a fresh owned CallExpr, not an IdentExpr).
func TestT0556_PushIfElseOnlyMutexParamRejected(t *testing.T) {
	errs := ownerErrs(t, `
		make_mutex_t0556() Mutex[int] { return Mutex[int](0); }
		take_if_else(Mutex[int] m, bool flag) {
			v := Vector[Mutex[int]]();
			v.push(if flag { make_mutex_t0556() } else { m });
		}
		test() {}
	`)
	expectOwnerError(t, errs, "cannot move borrowed parameter 'm'")
}

// Coverage: match-expression with arm.Body (no block, `=> expr`) form —
// the walk recurses into arm.Body via findBorrowedNonAliasSafeIdent.
func TestT0556_PushMatchExprBodyMutexParamRejected(t *testing.T) {
	errs := ownerErrs(t, `
		take_match(Mutex[int] m, int k) {
			v := Vector[Mutex[int]]();
			v.push(match k { 1 => m, _ => m });
		}
		test() {}
	`)
	expectOwnerError(t, errs, "cannot move borrowed parameter 'm'")
}

// Coverage: match-expression with arm.Block (`=> { stmts; expr }`) form —
// the walk recurses into arm.Block via findBorrowedNonAliasSafeIdentInBlock.
func TestT0556_PushMatchExprBlockMutexParamRejected(t *testing.T) {
	errs := ownerErrs(t, `
		take_match_block(Mutex[int] m, int k) {
			v := Vector[Mutex[int]]();
			v.push(match k { 1 => { m }, _ => { m } });
		}
		test() {}
	`)
	expectOwnerError(t, errs, "cannot move borrowed parameter 'm'")
}

// === T0349: extend tryMoveConsume to raise/yield/yield-from/select-send ===

// raise consumes the value into the caller's error slot — the outer caller
// owns and drops it. Same double-free pattern as T0338 if the raised value
// is a borrowed param.
func TestT0349_RaiseBorrowedParam(t *testing.T) {
	errs := ownerErrs(t, `
		type MyError is error {
			string field;
			new(~this, string move message, string move field) {
				this.message = message;
				this.field = field;
			}
		}
		forward!(MyError e) {
			raise e;
		}
		test() {}
	`)
	expectOwnerError(t, errs, "cannot move borrowed parameter 'e'")
}

// Owned local raised — fine, the local is consumed in place of being dropped.
func TestT0349_RaiseOwnedLocalOK(t *testing.T) {
	ownerOK(t, `
		type MyError is error {
			string field;
			new(~this, string move message, string move field) {
				this.message = message;
				this.field = field;
			}
		}
		forward!() {
			MyError e = MyError(message: "boom", field: "x");
			raise e;
		}
		test() {}
	`)
}

// yield value goes to the generator's yield slot; consumer owns and drops it.
// Yielding a borrowed param is a double-free.
func TestT0349_YieldBorrowedParam(t *testing.T) {
	errs := ownerErrs(t, `
		type Box { string s; new(~this, string move s) { this.s = s; } }
		gen(Box b) stream[Box] {
			yield b;
		}
		test() {}
	`)
	expectOwnerError(t, errs, "cannot move borrowed parameter 'b'")
}

// Yielding an owned local works.
func TestT0349_YieldOwnedLocalOK(t *testing.T) {
	ownerOK(t, `
		type Box { string s; new(~this, string move s) { this.s = s; } }
		gen() stream[Box] {
			Box b = Box(s: "hi");
			yield b;
		}
		test() {}
	`)
}

// `yield* g` consumes the inner generator (iterates to exhaustion, then drops).
// Yielding a borrowed-param generator is a double-free.
func TestT0349_YieldDelegateBorrowedParam(t *testing.T) {
	errs := ownerErrs(t, `
		outer(stream[int] s) stream[int] {
			yield* s;
		}
		test() {}
	`)
	expectOwnerError(t, errs, "cannot move borrowed parameter 's'")
}

// select-case channel send transfers ownership to the receiver — borrowed
// param sent in a select case is a double-free.
func TestT0349_SelectSendBorrowedParam(t *testing.T) {
	errs := ownerErrs(t, `
		send_via_select(channel[string] ch, string s) {
			select {
				ch.send(s):
				default:
			}
		}
		test() {}
	`)
	expectOwnerError(t, errs, "cannot move borrowed parameter 's'")
}

// Direct ch.send(s) call routes through Channel.send(~T) → tryMoveConsume on
// the arg branch. Borrowed param fails the consume check.
func TestT0349_DirectChannelSendBorrowedParam(t *testing.T) {
	errs := ownerErrs(t, `
		send_direct(channel[string] ch, string s) {
			ch.send(s);
		}
		test() {}
	`)
	expectOwnerError(t, errs, "cannot move borrowed parameter 's'")
}

// Direct ch.send of an owned local works.
func TestT0349_DirectChannelSendOwnedLocalOK(t *testing.T) {
	ownerOK(t, `
		test() {
			channel[string] ch = channel[string]();
			string s = "hi";
			ch.send(move s);
		}
	`)
}

// === T0351: AssignStmt RHS borrow-param consume rejected ===
//
// `x = borrow_param`, `obj.field = borrow_param`, `vec[i] = borrow_param`,
// `m[k] = borrow_param`, `vec[i:j] = borrow_param`, and `g.borrow = borrow_param`
// all consume the RHS — caller still drops the original, so a double-free
// occurs at runtime. tryMoveConsume in checkAssignStmt rejects them at
// compile time with "cannot move borrowed parameter".

// Simple variable reassignment to a borrowed param double-frees.
func TestT0351_AssignVarBorrowedParam(t *testing.T) {
	errs := ownerErrs(t, `
		swap(string s) {
			string x = "init";
			x = s;
		}
		test() {}
	`)
	expectOwnerError(t, errs, "cannot move borrowed parameter 's'")
}

// Simple variable reassignment to an owned local works.
func TestT0351_AssignVarOwnedLocalOK(t *testing.T) {
	ownerOK(t, `
		test() {
			string x = "init";
			string y = "other";
			x = y;
		}
	`)
}

// Field assignment to a borrowed param double-frees.
func TestT0351_AssignFieldBorrowedParam(t *testing.T) {
	errs := ownerErrs(t, `
		type Box { string s; new(~this, string move s) { this.s = s; } }
		store(Box move b, string s) {
			b.s = s;
		}
		test() {}
	`)
	expectOwnerError(t, errs, "cannot move borrowed parameter 's'")
}

// Field assignment with ~ param works.
func TestT0351_AssignFieldMoveParamOK(t *testing.T) {
	ownerOK(t, `
		type Box { string s; new(~this, string move s) { this.s = s; } }
		store(Box move b, string move s) {
			b.s = s;
		}
		test() {}
	`)
}

// Vector index assign to a borrowed param double-frees.
func TestT0351_AssignIndexVectorBorrowedParam(t *testing.T) {
	errs := ownerErrs(t, `
		put(string[] vec, string s) {
			vec[0] = s;
		}
		test() {}
	`)
	expectOwnerError(t, errs, "cannot move borrowed parameter 's'")
}

// Map index assign to a borrowed param double-frees.
func TestT0351_AssignIndexMapBorrowedParam(t *testing.T) {
	errs := ownerErrs(t, `
		put(map[string,string] m, string k, string v) {
			m[k] = v;
		}
		test() {}
	`)
	expectOwnerError(t, errs, "cannot move borrowed parameter 'v'")
}

// Vector slice assign to a borrowed-param Vector double-frees.
func TestT0351_AssignSliceBorrowedParam(t *testing.T) {
	errs := ownerErrs(t, `
		put(string[] vec, string[] s) {
			vec[1:3] = s;
		}
		test() {}
	`)
	expectOwnerError(t, errs, "cannot move borrowed parameter 's'")
}

// MutexGuard.borrow setter assigning a borrowed param double-frees.
func TestT0351_AssignMutexGuardBorrowBorrowedParam(t *testing.T) {
	errs := ownerErrs(t, `
		forward(Mutex[string] m, string s) {
			use g := m.lock();
			g.borrow = s;
		}
		test() {}
	`)
	expectOwnerError(t, errs, "cannot move borrowed parameter 's'")
}

// MutexGuard.borrow setter assigning a ~ param works.
func TestT0351_AssignMutexGuardBorrowMoveParamOK(t *testing.T) {
	ownerOK(t, `
		forward(Mutex[string] m, string move s) {
			use g := m.lock();
			g.borrow = s;
		}
		test() {}
	`)
}

// MutexGuard.borrow setter with an owned local works.
func TestT0351_AssignMutexGuardBorrowOwnedLocalOK(t *testing.T) {
	ownerOK(t, `
		test() {
			m := Mutex[string]("init");
			use g := m.lock();
			string s = "new";
			g.borrow = s;
		}
	`)
}

// Copy types are unaffected by tryMoveConsume — int reassignment from a
// non-~ param is fine.
func TestT0351_AssignCopyParamUnaffected(t *testing.T) {
	ownerOK(t, `
		swap(int n) {
			int x = 0;
			x = n;
		}
		test() {}
	`)
}

// Compound assignment (`+=`, `-=`, etc.) takes the same path as plain
// assignment in checkAssignStmt — tryMoveConsume runs unconditionally.
// Borrowed-param RHS is rejected for all assign ops, including compound.
// This is a deliberately conservative consequence of T0351; the codegen
// panic on `string +=` (T0357) makes the practical user-visible impact
// minimal.
func TestT0351_CompoundAssignBorrowedParam(t *testing.T) {
	errs := ownerErrs(t, `
		append_to(string s) {
			string x = "init";
			x += s;
		}
		test() {}
	`)
	expectOwnerError(t, errs, "cannot move borrowed parameter 's'")
}

// Owned-local compound assign reaches sema move (drop flag clear) — the
// codegen panic on string += (T0357) is independent of the sema layer.
// This test confirms the sema accepts the move; codegen will then panic
// at run time, but that's a separate bug.
func TestT0351_CompoundAssignOwnedLocalMoves(t *testing.T) {
	ownerOK(t, `
		test() {
			string x = "hello";
			string y = "world";
			x += y;
		}
	`)
}

// === T0380: cannot move out of `.borrow` getter on Arc/MutexGuard ===

// Var bound to .borrow cannot be moved into a ~T callee. T0438: sema now
// rejects the implicit `string& → string` decay at the parameter boundary,
// so the safety check fires earlier (sema-level) than the previous
// ownership-level "cannot move borrowed value" diagnostic.
func TestT0380_ConsumeBorrowVar(t *testing.T) {
	errs := ownerErrs(t, `
		consume(string move s) {}
		test() {
			s := "hi";
			a := Ref[string](s);
			borrowed := a.borrow;
			consume(borrowed);
		}
	`)
	expectOwnerError(t, errs, "cannot assign string& to parameter 's'")
}

// Inline .borrow cannot be passed to a ~T callee. T0438: sema's
// non-Copy decay rejection now also catches this earlier than the
// previous "cannot move out of '.borrow' getter" ownership-level check.
func TestT0380_ConsumeInlineBorrow(t *testing.T) {
	errs := ownerErrs(t, `
		consume(string move s) {}
		test() {
			s := "hi";
			a := Ref[string](s);
			consume(a.borrow);
		}
	`)
	expectOwnerError(t, errs, "cannot assign string& to parameter 's'")
}

// T0438: reassigning a non-Copy borrow to an owned local is now rejected
// at the sema level (Rule 8b/8c gated on Copy). Previous behavior allowed
// the assignment and relied on ownership state tracking to reject any
// downstream consume — that downstream check is now defense-in-depth.
func TestT0380_AssignBorrowToOwnedThenConsumeRejected(t *testing.T) {
	errs := ownerErrs(t, `
		consume(string move s) {}
		test() {
			s := "hi";
			a := Ref[string](s);
			b := "old";
			b = a.borrow;
			consume(b);
		}
	`)
	expectOwnerError(t, errs, "cannot assign string& to string")
}

// T0438: same plain-reassignment now rejected at sema. Use `.clone()` for
// an owned independent copy or declare `b` as `string&` to keep it as a
// borrow.
func TestT0380_AssignBorrowToOwnedRejected(t *testing.T) {
	errs := ownerErrs(t, `
		test() {
			s := "hi";
			a := Ref[string](s);
			b := "old";
			b = a.borrow;
		}
	`)
	expectOwnerError(t, errs, "cannot assign string& to string")
}

// Cloning the borrow produces an owned independent copy — safe.
func TestT0380_AssignBorrowCloneToOwnedOK(t *testing.T) {
	ownerOK(t, `
		test() {
			s := "hi";
			a := Ref[string](move s);
			b := "old";
			b = a.borrow.clone();
		}
	`)
}

// T0438: same sema-level rejection applies for MutexGuard.borrow.
func TestT0380_ConsumeMutexGuardBorrow(t *testing.T) {
	errs := ownerErrs(t, `
		consume(string move s) {}
		test() {
			m := Mutex[string]("hi");
			use guard := m.lock();
			borrowed := guard.borrow;
			consume(borrowed);
		}
	`)
	expectOwnerError(t, errs, "cannot assign string& to parameter 's'")
}

// T0438: passing a non-Copy borrow to a value-typed `string` param is now
// rejected at sema. Use `.clone()` to pass an owned copy, or change the
// callee parameter to `string&`.
func TestT0380_BorrowVarToValueParamRejected(t *testing.T) {
	// T0998: a bare parameter is a shared borrow, so passing an existing borrow
	// reborrows into it — no longer rejected. (Consuming a borrow is still
	// rejected; see the `move`-parameter cases.)
	ownerOK(t, `
		readlen(string s) int { return s.len; }
		test() {
			s := "hi";
			a := Ref[string](move s);
			borrowed := a.borrow;
			int n = readlen(borrowed);
		}
	`)
}

// T0438: cloning makes it an owned copy — accepted.
func TestT0380_BorrowCloneToValueParamOK(t *testing.T) {
	ownerOK(t, `
		readlen(string s) int { return s.len; }
		test() {
			s := "hi";
			a := Ref[string](move s);
			int n = readlen(a.borrow.clone());
		}
	`)
}

// Reading borrowed (member access) is OK.
func TestT0380_BorrowVarReadOK(t *testing.T) {
	ownerOK(t, `
		test() {
			s := "hi";
			a := Ref[string](move s);
			borrowed := a.borrow;
			int n = borrowed.len;
		}
	`)
}

// Borrow used in vector literal is rejected (collection consumes).
// T0407: the type-driven check at the top of `tryMoveConsume` fires first
// because `borrowed` is typed `string&` (a non-Copy borrow) — the unified
// diagnostic supersedes the per-ident "borrowed value" message.
func TestT0380_BorrowInVectorLit(t *testing.T) {
	errs := ownerErrs(t, `
		test() {
			s := "hi";
			a := Ref[string](s);
			borrowed := a.borrow;
			string[] v = [borrowed];
		}
	`)
	expectOwnerError(t, errs, "cannot move out of '.borrow' getter")
}

// T0381 / T0438: explicit `string& borrowed = a.borrow;` keeps the var as a
// borrow; the call `consume(borrowed)` (which takes `~string`) is rejected
// by sema since `string&` is not assignable to `string` for non-Copy T.
func TestT0381_ExplicitRefDeclRejectsConsume(t *testing.T) {
	errs := ownerErrs(t, `
		consume(string move s) {}
		test() {
			s := "hi";
			a := Ref[string](s);
			string& borrowed = a.borrow;
			consume(borrowed);
		}
	`)
	expectOwnerError(t, errs, "cannot assign string& to parameter 's'")
}

// T0381 / T0438: a generic-style `T&` return passed into a `~T` consumer is
// likewise rejected at sema for non-Copy T.
func TestT0381_GenericRefReturnRejectsConsume(t *testing.T) {
	errs := ownerErrs(t, `
		getRef(string s) string& { return s; }
		consume(string move s) {}
		test() {
			string s = "hello";
			r := getRef(s);
			consume(r);
		}
	`)
	expectOwnerError(t, errs, "cannot assign string& to parameter 's'")
}

// T0438: typed `string borrowed = a.borrow;` (non-Copy) is rejected at the
// var-decl boundary itself.
func TestT0380_TypedDeclBorrowVarRejected(t *testing.T) {
	errs := ownerErrs(t, `
		test() {
			s := "hi";
			a := Ref[string](s);
			string borrowed = a.borrow;
		}
	`)
	expectOwnerError(t, errs, "cannot assign string& to variable of type string")
}

// Copy inner types (Ref[int], Ref[bool], etc.) have no double-free risk:
// `.borrow` returns a value copy, so moves into ~T params or channel sends
// are safe. Existing patterns like `ch.send(a.borrow)` must continue to work.
func TestT0380_CopyInnerTypeNoReject(t *testing.T) {
	ownerOK(t, `
		consume(int move n) {}
		test() {
			a := Ref[int](42);
			consume(a.borrow);
			b := a.borrow;
			consume(b);
		}
	`)
}

// MutexGuard with Copy inner type: same — no rejection.
func TestT0380_MutexGuardCopyInnerNoReject(t *testing.T) {
	ownerOK(t, `
		consume(int move n) {}
		test() {
			m := Mutex[int](42);
			use guard := m.lock();
			consume(guard.borrow);
		}
	`)
}

// T0377 / T0438: borrow laundered through an if-expression. The arms both
// produce `string&`, the joined type stays `string&`, and `consume(~string)`
// is rejected at sema.
func TestT0377_ConsumeIfBorrowVarRejected(t *testing.T) {
	errs := ownerErrs(t, `
		consume(string move s) {}
		test() {
			s := "hi";
			a := Ref[string](s);
			cond := true;
			borrowed := if cond { a.borrow } else { a.borrow };
			consume(borrowed);
		}
	`)
	expectOwnerError(t, errs, "cannot assign string& to parameter 's'")
}

// T0377 / T0438: same for match-laundered borrows.
func TestT0377_ConsumeMatchBorrowVarRejected(t *testing.T) {
	errs := ownerErrs(t, `
		consume(string move s) {}
		test() {
			s := "hi";
			a := Ref[string](s);
			k := 1;
			borrowed := match k { 1 => a.borrow, _ => a.borrow };
			consume(borrowed);
		}
	`)
	expectOwnerError(t, errs, "cannot assign string& to parameter 's'")
}

// T0488: Mixed-ownership if-expression (one borrow arm, one owned arm) of
// non-Copy type is rejected at sema time — the prior T0377 "gap" left the
// borrow inner pointer treated as owned, causing UAF on scope exit.
func TestT0488_MixedIfNonCopyRejected(t *testing.T) {
	errs := ownerErrs(t, `
		consume(string move s) {}
		test() {
			s := "hi";
			a := Ref[string](s);
			cond := true;
			other := "owned";
			borrowed := if cond { a.borrow } else { other };
			consume(borrowed);
		}
	`)
	expectOwnerError(t, errs, "mix borrowed and owned non-Copy 'string'")
}

// T0377 / T0438: parenthesized borrow likewise stays `string&` and is
// rejected by sema at the consume call.
func TestT0377_ConsumeParenBorrowVarRejected(t *testing.T) {
	errs := ownerErrs(t, `
		consume(string move s) {}
		test() {
			s := "hi";
			a := Ref[string](s);
			borrowed := (a.borrow);
			consume(borrowed);
		}
	`)
	expectOwnerError(t, errs, "cannot assign string& to parameter 's'")
}

// T0377 / T0438: block-bodied match arms produce `string&` joined type and
// are likewise rejected at the consume call.
func TestT0377_ConsumeMatchBlockBorrowVarRejected(t *testing.T) {
	errs := ownerErrs(t, `
		consume(string move s) {}
		test() {
			s := "hi";
			a := Ref[string](s);
			k := 1;
			borrowed := match k {
				1 => { a.borrow },
				_ => { a.borrow },
			};
			consume(borrowed);
		}
	`)
	expectOwnerError(t, errs, "cannot assign string& to parameter 's'")
}

// T0488: Mixed-ownership match (one borrow arm, one owned arm) of non-Copy
// type is rejected at sema time. Parallels TestT0488_MixedIfNonCopyRejected
// for the match-expression code path in checkMatchExpr.
func TestT0488_MixedMatchNonCopyRejected(t *testing.T) {
	errs := ownerErrs(t, `
		consume(string move s) {}
		test() {
			s := "hi";
			a := Ref[string](s);
			other := "owned";
			k := 1;
			borrowed := match k {
				1 => a.borrow,
				_ => other,
			};
			consume(borrowed);
		}
	`)
	expectOwnerError(t, errs, "mix borrowed and owned non-Copy 'string'")
}

// T0402 / T0438: returning `T&` (non-Copy elem) as owned `T` is unsafe.
// Sema now rejects the implicit decay at the return boundary itself —
// previously the ownership analyzer's `returnsBorrowAsOwned` was the
// only line of defense.
func TestT0402_ReturnBorrowAsOwnedRejected_LocalSource(t *testing.T) {
	errs := ownerErrs(t, `
		bad() string {
			a := Ref[string]("x");
			return a.borrow;
		}
	`)
	expectOwnerError(t, errs, "cannot return string& from function returning string")
}

// T0402 / T0438: same rejection when the Arc comes from a parameter.
func TestT0402_ReturnBorrowAsOwnedRejected_ParamSource(t *testing.T) {
	errs := ownerErrs(t, `
		bad(Ref[string] a) string {
			return a.borrow;
		}
	`)
	expectOwnerError(t, errs, "cannot return string& from function returning string")
}

// T0402 / T0438: same rejection for MutexGuard.borrow.
func TestT0402_ReturnBorrowAsOwnedRejected_MutexGuard(t *testing.T) {
	errs := ownerErrs(t, `
		bad() string {
			m := Mutex[string]("x");
			use g := m.lock();
			return g.borrow;
		}
	`)
	expectOwnerError(t, errs, "cannot return string& from function returning string")
}

// T0402: Copy element types (int, bool, etc.) are safe — the value is loaded
// at the borrow boundary and the original owner is unaffected.
func TestT0402_ReturnBorrowAsOwnedOK_CopyElem(t *testing.T) {
	ownerOK(t, `
		ok(Ref[int] a) int {
			return a.borrow;
		}
	`)
}

// T0402: explicit `.clone()` produces an owned copy — the documented fix.
func TestT0402_ReturnBorrowCloneOK(t *testing.T) {
	ownerOK(t, `
		ok(Ref[string] a) string {
			return a.borrow.clone();
		}
	`)
}

// T0402: regression check — the existing local-vs-param check on ref-typed
// returns must still fire when the result type is `string&` and the source
// is a local Arc (the reference outlives its Arc).
func TestT0402_ReturnBorrowAsRefRejected_Local(t *testing.T) {
	errs := ownerErrs(t, `
		bad() string& {
			a := Ref[string]("x");
			return a.borrow;
		}
	`)
	expectOwnerError(t, errs, "cannot return reference to local variable 'a'")
}

// T0402: returning `T&` from a borrow-typed expression where source is a
// parameter is allowed by the existing ref-result branch (the borrow stays
// a borrow, no decay to owned).
func TestT0402_ReturnBorrowAsRefOK_Param(t *testing.T) {
	ownerOK(t, `
		ok(Ref[string] a) string& {
			return a.borrow;
		}
	`)
}

// T0402 / T0438: when sema's joinBranchTypes preserves `T&` (all arms are
// borrows), sema rejects the return at the type-assignability check.
func TestT0402_ReturnBorrowThroughIfRejected(t *testing.T) {
	errs := ownerErrs(t, `
		bad(Ref[string] a, bool cond) string {
			return if cond { a.borrow } else { a.borrow };
		}
	`)
	expectOwnerError(t, errs, "cannot return string& from function returning string")
}

// T0438: typed local declaration `string borrowed = a.borrow;` is rejected
// at the var-decl boundary itself for non-Copy T (no implicit decay).
func TestT0402_ReturnBorrowThroughTypedLocalRejected(t *testing.T) {
	errs := ownerErrs(t, `
		bad(Ref[string] a) string {
			string borrowed = a.borrow;
			return borrowed;
		}
	`)
	expectOwnerError(t, errs, "cannot assign string& to variable of type string")
}

// T0402: inferred local keeps the type as `string&`; the return rejection
// then fires at the return boundary.
func TestT0402_ReturnBorrowThroughInferredLocalRejected(t *testing.T) {
	errs := ownerErrs(t, `
		bad(Ref[string] a) string {
			borrowed := a.borrow;
			return borrowed;
		}
	`)
	expectOwnerError(t, errs, "cannot return string& from function returning string")
}

// T0402: laundering through if then through a local — return still rejected.
func TestT0402_ReturnBorrowThroughIfLocalRejected(t *testing.T) {
	errs := ownerErrs(t, `
		bad(Ref[string] a, bool cond) string {
			borrowed := if cond { a.borrow } else { a.borrow };
			return borrowed;
		}
	`)
	expectOwnerError(t, errs, "cannot return string& from function returning string")
}

// T0438: the `string borrowed = a.borrow;` form is rejected at sema, so
// this test is updated to use `.clone()` for an owned independent copy
// (the documented recovery path for non-Copy borrows).
func TestT0402_ReturnAfterCloneToOwnedOK(t *testing.T) {
	ownerOK(t, `
		ok(Ref[string] a) string {
			string borrowed = a.borrow.clone();
			borrowed = "hello";
			return borrowed;
		}
	`)
}

// === T0426: checkLambdaExpr uses lambda signature for return checks ===
//
// Before T0426, `checkLambdaExpr` did not save/restore `c.curSig`, `c.params`,
// or `c.returnOrigins`, so a `return` inside a lambda body ran through
// `checkReturnRefSafety` using the OUTER function's signature. Two failure
// modes existed:
//
//  1. False negative: outer fn `void` → `c.curSig.Result() == nil` → T0402's
//     borrow-as-owned check skipped, even though the lambda's actual return
//     type is owned `T`. (Sema's T0438 still fires for this case via its own
//     curFunc save/restore — sema's correct here — but the ownership pass
//     becoming a defensive duplicate is the goal.)
//
//  2. False positive: outer fn returns owned `T`, lambda returns `T&`. The
//     ownership pass saw outer's owned `T` result type and `a.borrow` of type
//     `T&`, fired the "cannot return borrowed reference as owned" error,
//     even though the lambda's own signature is `T&` and the return is legit.

// T0426: lambda body returning `T&` typed expr from inside a void outer
// function — sema catches this via T0438 (lambda's c.curFunc has owned
// `string` result, return value typed `string&` is not assignable). Before
// T0426, even if sema were silent, the ownership pass would have skipped
// the check because outer was void. After T0426 the ownership pass also
// uses the lambda's signature, so the check would fire defensively.
func TestT0426_LambdaReturnBorrowAsOwnedRejected_VoidOuter(t *testing.T) {
	errs := ownerErrs(t, `
		test() {
			bar := move || -> string {
				a := Ref[string]("x");
				return a.borrow;
			};
		}
	`)
	expectOwnerError(t, errs, "cannot return string& from function returning string")
}

// T0426: false-positive case. Outer returns owned `string`, lambda returns
// `string&` and borrows from a move-captured Arc. Before T0426, the ownership
// pass used the outer's owned `string` signature inside the lambda body and
// rejected the legit `return a.borrow`. After the fix the lambda's own
// `string&` signature is used and captures are treated as parameter-like.
func TestT0426_LambdaReturnRefOK_OwnedOuter(t *testing.T) {
	ownerOK(t, `
		test() string {
			a := Ref[string]("x");
			f := move || -> string& {
				return a.borrow;
			};
			return "ok";
		}
	`)
}

// T0426: sanity — a lambda taking a ref param can return that param.
func TestT0426_LambdaReturnRefToLambdaParam_OK(t *testing.T) {
	ownerOK(t, `
		test() {
			f := |string s| -> string& { return s; };
		}
	`)
}

// T0426: locals declared inside the lambda body still produce ref-to-local
// errors — captures are param-like, but body locals are not.
func TestT0426_LambdaReturnRefToLambdaLocalRejected(t *testing.T) {
	errs := ownerErrs(t, `
		test() {
			f := || -> string& {
				a := Ref[string]("x");
				return a.borrow;
			};
		}
	`)
	expectOwnerError(t, errs, "cannot return reference to local variable 'a'")
}

// T0426: regression — outer fn's own `return` checks must still use the
// outer's signature after a lambda body has been processed. Place a lambda
// before the outer's return and confirm the outer's T0402 rejection fires.
func TestT0426_LambdaInsideOwnedReturnDoesNotPolluteOuter(t *testing.T) {
	errs := ownerErrs(t, `
		bad() string {
			f := move || -> int { return 42; };
			a := Ref[string]("x");
			return a.borrow;
		}
	`)
	expectOwnerError(t, errs, "cannot return string& from function returning string")
}

// T0426: nested lambdas — outer lambda returns string& from a move-captured
// Ref[string], inner lambda returns string& from its own ref param. The
// save/restore must work through nesting: the inner's signature/params are
// pushed and popped without polluting the outer lambda's state.
func TestT0426_NestedLambdaRefReturnsBothLevels_OK(t *testing.T) {
	ownerOK(t, `
		test() {
			a := Ref[string]("x");
			outer := move || -> string& {
				inner := |string s| -> string& { return s; };
				return inner(a.borrow);
			};
		}
	`)
}

// T0426: nested lambdas — inner lambda has its own owned-result signature;
// returning a borrow from a lambda-local Arc must still fail with the
// "borrow as owned" rejection, proving the inner lambda's signature is used
// (not the outer lambda's). The outer lambda's signature is also owned, so
// to ensure the rejection is coming from the *inner* check we use a
// distinct local name and assert the position points inside the inner body.
func TestT0426_NestedLambdaInnerBorrowAsOwnedRejected(t *testing.T) {
	errs := ownerErrs(t, `
		test() {
			outer := move || -> int {
				inner := move || -> string {
					a := Ref[string]("x");
					return a.borrow;
				};
				return 0;
			};
		}
	`)
	expectOwnerError(t, errs, "cannot return string& from function returning string")
}

// T0426: lambda inside method body, lambda has owned `string` result, body
// returns a borrow → must fire the borrow-as-owned check using the lambda's
// signature, not the method's. Method has no result (void), so this
// confirms the path where method's c.curSig.Result() is nil but the
// lambda's signature is correctly substituted in.
func TestT0426_LambdaInsideMethodBorrowAsOwnedRejected(t *testing.T) {
	errs := ownerErrs(t, `
		type W {
			int x;
			method(this) {
				f := move || -> string {
					a := Ref[string]("x");
					return a.borrow;
				};
			}
		}
		test() { w := W(x: 1); }
	`)
	expectOwnerError(t, errs, "cannot return string& from function returning string")
}

// T0426: lambda inside method body — method has owned `string` result, but
// the lambda inside it returns `string&` from its own ref param. Before
// T0426 the ownership pass would (wrongly) use the method's owned-string
// curSig inside the lambda body and reject the legit ref return. After the
// fix the lambda's own signature is used.
func TestT0426_LambdaInsideOwnedMethod_RefReturnOK(t *testing.T) {
	ownerOK(t, `
		type W {
			int x;
			method(this) string {
				f := move |string s| -> string& { return s; };
				return "ok";
			}
		}
		test() {}
	`)
}

// T0426: regression for the method case — after the lambda body has been
// checked, the method's own `return` must still use the method's signature.
// Place a lambda first, then a borrow-as-owned return; the method's owned
// result type must reject the return.
func TestT0426_LambdaInsideMethodDoesNotPolluteMethodSig(t *testing.T) {
	errs := ownerErrs(t, `
		type W {
			int x;
			bad(this) string {
				f := move || -> int { return 42; };
				a := Ref[string]("x");
				return a.borrow;
			}
		}
		test() {}
	`)
	expectOwnerError(t, errs, "cannot return string& from function returning string")
}

// T0426: returnAmbiguity now fires inside a lambda body. Lambda has two ref
// params and returns from both (via if/else), with the lambda's own
// signature being a ref result. Before T0426, c.returnOrigins was shared
// with the outer fn (and its outer signature was used), so this case
// either silently passed (void outer) or fired confusingly against the
// outer fn. After T0426, checkReturnAmbiguity is called inside
// checkLambdaExpr on the lambda's own returnOrigins.
func TestT0426_LambdaMultipleRefParamsAmbiguous(t *testing.T) {
	errs := ownerErrs(t, `
		test() {
			f := |string a, string b, bool c| -> string& {
				if c { return a; }
				return b;
			};
		}
	`)
	expectOwnerError(t, errs, "ambiguous return reference")
}

// T0426: lambda's returnOrigins must be reset on entry, so a previous
// lambda's return-from-param doesn't leak into a sibling lambda. Two
// independent lambdas in the same outer fn, each returning from its own
// (single) ref param, must both type-check cleanly with no ambiguity.
func TestT0426_SiblingLambdasReturnOriginsReset(t *testing.T) {
	ownerOK(t, `
		test() {
			f := |string a| -> string& { return a; };
			g := |string b| -> string& { return b; };
		}
	`)
}

// T0426: a lambda's `_` parameter must not be added to c.params (sema also
// skips it from scope). Sanity-check that mixing a `_` with a real param
// still permits returning the real param.
func TestT0426_LambdaUnderscoreParamSkipped_OK(t *testing.T) {
	ownerOK(t, `
		test() {
			f := |int _, string s| -> string& { return s; };
		}
	`)
}

// === T0382: borrow → owned field rejected ===
//
// T0385 (the IndexExpr sibling) is fixed by codegen-dup in T0383, so only
// the MemberExpr case needs a sema rejection here.

// T0382 / T0438: `obj.field = a.borrow` for a non-Copy element T is rejected
// at sema (no implicit `string[]& → string[]` decay). Use `.clone()` to
// deep-copy or restructure the field type for sharing.
func TestT0382_FieldAssignFromArcBorrowRejected(t *testing.T) {
	errs := ownerErrs(t, `
		type Holder { string[] field; }
		test() {
			v1 := string[]();
			v1.push("init" + "");
			h := Holder(v1);
			v2 := string[]();
			v2.push("hello" + "");
			a := Ref[string[]](v2);
			h.field = a.borrow;
		}
	`)
	expectOwnerError(t, errs, "cannot assign string[]& to string[]")
}

// T0382 / T0438: same rule applies to MutexGuard.borrow.
func TestT0382_FieldAssignFromMutexGuardBorrowRejected(t *testing.T) {
	errs := ownerErrs(t, `
		type Holder { string[] field; }
		test() {
			v1 := string[]();
			v1.push("init" + "");
			h := Holder(v1);
			v2 := string[]();
			v2.push("hello" + "");
			m := Mutex[string[]](v2);
			use guard := m.lock();
			h.field = guard.borrow;
		}
	`)
	expectOwnerError(t, errs, "cannot assign string[]& to string[]")
}

// T0382: Copy element types (Ref[int].borrow → int field) are independently
// copied through the borrow, so no double-free risk and no rejection.
// isBorrowedExpr returns false for Copy underlying types (T0380).
func TestT0382_FieldAssignFromArcBorrowCopyAllowed(t *testing.T) {
	ownerOK(t, `
		type IntHolder { int n; }
		test() {
			h := IntHolder(0);
			a := Ref[int](42);
			h.n = a.borrow;
		}
	`)
}

// T0382: explicit `.clone()` on the borrow yields an owned independent copy
// — assignment to the field is then a normal owned move and is permitted.
func TestT0382_FieldAssignFromBorrowClonedAllowed(t *testing.T) {
	ownerOK(t, `
		type Holder { string[] field; }
		test() {
			v1 := string[]();
			v1.push("init" + "");
			h := Holder(move v1);
			v2 := string[]();
			v2.push("hello" + "");
			a := Ref[string[]](move v2);
			h.field = a.borrow.clone();
		}
	`)
}

// === T0438: Implicit T&/T~ → T decay restricted to Copy types ===
//
// These tests pin the new sema-level rejection of borrow → owned decay for
// non-Copy element types, and confirm the recovery paths (`.clone()` for an
// owned copy, or `T&` for keeping it as a borrow). The previous unrestricted
// decay produced a steady stream of codegen dup-on-read patches (T0383,
// T0388, T0392, T0397, T0398, T0413, T0428, T0431, T0439) that this rule
// removes the root cause of.

// T0438: `T borrowed = expr_with_borrow_type;` rejected when T is non-Copy.
func TestT0438_AssignBorrowToNonCopyOwnedRejected(t *testing.T) {
	errs := ownerErrs(t, `
		test() {
			a := Ref[string]("hi");
			string s = a.borrow;
		}
	`)
	expectOwnerError(t, errs, "cannot assign string& to variable of type string")
}

// T0438: same form is allowed when T is Copy (int) — the decay is sound
// because the value is loaded at the borrow boundary and the original
// owner is unaffected.
func TestT0438_AssignBorrowToCopyOwnedOK(t *testing.T) {
	ownerOK(t, `
		test() {
			a := Ref[int](42);
			int n = a.borrow;
		}
	`)
}

// T0438: passing a non-Copy borrow into a value-typed param is rejected at
// the call site by the same Copy-only decay rule.
func TestT0438_BorrowToValueParamRejected(t *testing.T) {
	// T0998: a bare parameter borrows its argument, so a `.borrow` result
	// reborrows into it cleanly — the Ref retains ownership.
	ownerOK(t, `
		take(string s) {}
		test() {
			a := Ref[string]("hi");
			take(a.borrow);
		}
	`)
}

// T0438: `.clone()` produces an owned independent copy — the documented
// recovery path for non-Copy borrows.
func TestT0438_BorrowCloneToOwnedOK(t *testing.T) {
	ownerOK(t, `
		test() {
			a := Ref[string]("hi");
			string s = a.borrow.clone();
		}
	`)
}

// T0438: declaring the local as `T&` keeps it as a borrow — no decay,
// no implicit allocation.
func TestT0438_BorrowToRefDeclOK(t *testing.T) {
	ownerOK(t, `
		test() {
			a := Ref[string]("hi");
			string& s = a.borrow;
		}
	`)
}

// T0438: returning a non-Copy borrow as owned `T` is rejected at sema
// (defense-in-depth on top of T0402's ownership-level check).
func TestT0438_ReturnNonCopyBorrowAsOwnedRejected(t *testing.T) {
	errs := ownerErrs(t, `
		bad(Ref[string] a) string {
			return a.borrow;
		}
	`)
	expectOwnerError(t, errs, "cannot return string& from function returning string")
}

// T0438: returning a Copy borrow as owned is allowed — the value is
// loaded by value, the Arc retains its ownership.
func TestT0438_ReturnCopyBorrowAsOwnedOK(t *testing.T) {
	ownerOK(t, `
		ok(Ref[int] a) int {
			return a.borrow;
		}
	`)
}

// T0438: vector element decay is also rejected (Vector[T] is non-Copy).
func TestT0438_VectorBorrowToOwnedRejected(t *testing.T) {
	errs := ownerErrs(t, `
		test() {
			v := [1, 2, 3];
			a := Ref[int[]](v);
			int[] x = a.borrow;
		}
	`)
	expectOwnerError(t, errs, "cannot assign int[]& to variable of type int[]")
}

// T0438: `T~` (mutable borrow) decay is also Copy-only.
func TestT0438_MutBorrowToNonCopyOwnedRejected(t *testing.T) {
	errs := ownerErrs(t, `
		take(string move s) string& { return s; }
		test() {
			s := "hi";
			r := take(s);
			string owned = r;
		}
	`)
	// Two errors expected here; either is fine — assert the decay rejection.
	expectOwnerError(t, errs, "cannot assign string& to variable of type string")
}

// === T0401: assignment to `MutexGuard.borrow` setter from a borrow getter ===
//
// `guard.borrow = guard.borrow` (or any `g.borrow = src.borrow` where the
// underlying T is non-Copy) is a UAF: the setter does drop-then-store on the
// same slot, and the source's inner pointer aliases the dest's, so the drop
// frees what the store re-installs. T0379's codegen-level dropflag-clear
// only protects local IdentExpr LHS; member/index targets have no per-slot
// dropflag. T0401 narrows the T0380/T0381 skip to require IdentExpr LHS, so
// member/index targets fall through to `tryMoveConsume` and are rejected
// with the "cannot move out of '.borrow' getter" diagnostic.

// T0401: the original repro from the bug — self-assignment via the setter.
func TestT0401_AssignSetterFromBorrowSelf(t *testing.T) {
	errs := ownerErrs(t, `
		test() {
			m := Mutex[string]("hi" + "");
			use guard := m.lock();
			guard.borrow = guard.borrow;
		}
	`)
	expectOwnerError(t, errs, "cannot move out of '.borrow' getter")
}

// T0401: cross-mutex case — also UAF since the source mutex still owns its
// inner string and would double-free at end of scope.
func TestT0401_AssignSetterFromOtherBorrow(t *testing.T) {
	errs := ownerErrs(t, `
		test() {
			m1 := Mutex[string]("a" + "");
			m2 := Mutex[string]("b" + "");
			use g1 := m1.lock();
			use g2 := m2.lock();
			g1.borrow = g2.borrow;
		}
	`)
	expectOwnerError(t, errs, "cannot move out of '.borrow' getter")
}

// T0401: field-typed LHS — sema's T0438 non-Copy decay rejection fires
// first since the field's static type is `T`, not `T&`. Pinned here so
// future sema changes can't silently regress to runtime UAF.
func TestT0401_AssignFieldFromBorrow(t *testing.T) {
	errs := ownerErrs(t, `
		type Holder { string s; }
		test() {
			h := Holder("init" + "");
			a := Ref[string]("hello" + "");
			h.s = a.borrow;
		}
	`)
	expectOwnerError(t, errs, "cannot assign string& to string")
}

// T0401: vector index LHS — sema rejects the implicit decay at the setter
// param boundary (`[]$set` takes `~T`).
func TestT0401_AssignVectorIndexFromBorrow(t *testing.T) {
	errs := ownerErrs(t, `
		test() {
			v := string[]();
			v.push("init" + "");
			a := Ref[string]("hello" + "");
			v[0] = a.borrow;
		}
	`)
	expectOwnerError(t, errs, "cannot assign string& to")
}

// T0401: `.clone()` on the borrow yields an owned independent copy — the
// supported recovery path. No rejection.
func TestT0401_AssignSetterFromBorrowCloneOK(t *testing.T) {
	ownerOK(t, `
		test() {
			m1 := Mutex[string]("a" + "");
			m2 := Mutex[string]("b" + "");
			use g1 := m1.lock();
			use g2 := m2.lock();
			g1.borrow = g2.borrow.clone();
		}
	`)
}

// T0401: Copy inner type (`int`) — `isBorrowedExpr` returns false so
// `rhsIsBorrowGetter` stays false for both the old and new code paths.
// No spurious rejection on Copy types.
func TestT0401_AssignSetterCopyInnerOK(t *testing.T) {
	ownerOK(t, `
		test() {
			m1 := Mutex[int](1);
			m2 := Mutex[int](2);
			use g1 := m1.lock();
			use g2 := m2.lock();
			g1.borrow = g2.borrow;
		}
	`)
}

// T0401: re-assignment to a typed `T&` local (`string& b = a1.borrow; b = a2.borrow;`)
// is the preserved `lhsIsIdent && rhsIsBorrowGetter` path — the skip is sound
// here because T0379's codegen-level dropflag-clear protects local IdentExpr
// LHS. Pins the preserved branch so a future regression that broadens the
// narrow (or always runs tryMoveConsume) gets caught — existing T0381 var-decl
// tests don't exercise the OpAssign + IdentExpr LHS shape.
func TestT0401_TypedRefLocalReassignFromBorrowOK(t *testing.T) {
	ownerOK(t, `
		test() {
			a1 := Ref[string]("a" + "");
			a2 := Ref[string]("b" + "");
			string& b = a1.borrow;
			b = a2.borrow;
		}
	`)
}

// === T0407: setter LHS / consume site with if/match/paren-wrapped borrow RHS ===
//
// `tryMoveConsume` previously only inspected the direct `MemberExpr` shape,
// so `guard.borrow = if cond { guard.borrow } else { guard.borrow }` (and the
// match/paren variants) slipped through to runtime as a UAF — the setter
// drop-then-stores while the parent Mutex retains its drop responsibility.
// T0407 replaces the AST-shape check with a type-driven one at the top of
// `tryMoveConsume`: any expr typed `T&`/`T~` (non-Copy) is rejected
// uniformly, since sema's `joinBranchTypes` preserves the borrow type
// through if/match arms and `ParenExpr` propagates the inner type.

func TestT0407_AssignSetterFromIfBorrow(t *testing.T) {
	errs := ownerErrs(t, `
		test() {
			m := Mutex[string]("hi" + "");
			use guard := m.lock();
			cond := true;
			guard.borrow = if cond { guard.borrow } else { guard.borrow };
		}
	`)
	expectOwnerError(t, errs, "cannot move out of '.borrow' getter")
}

func TestT0407_AssignSetterFromMatchBorrow(t *testing.T) {
	errs := ownerErrs(t, `
		test() {
			m := Mutex[string]("hi" + "");
			use guard := m.lock();
			x := 1;
			guard.borrow = match x { 1 => guard.borrow, _ => guard.borrow };
		}
	`)
	expectOwnerError(t, errs, "cannot move out of '.borrow' getter")
}

func TestT0407_AssignSetterFromParenBorrow(t *testing.T) {
	errs := ownerErrs(t, `
		test() {
			m := Mutex[string]("hi" + "");
			use guard := m.lock();
			guard.borrow = (guard.borrow);
		}
	`)
	expectOwnerError(t, errs, "cannot move out of '.borrow' getter")
}

// T0407: clone() inside each arm yields independent owned copies — no UAF.
func TestT0407_AssignSetterFromIfBorrowCloneOK(t *testing.T) {
	ownerOK(t, `
		test() {
			m := Mutex[string]("hi" + "");
			use guard := m.lock();
			cond := true;
			guard.borrow = if cond { guard.borrow.clone() } else { guard.borrow.clone() };
		}
	`)
}

// T0407: Copy inner type — joined if-arm type decays via Rule 8b, so
// `isBorrowedExpr` returns false and there is no spurious rejection. Mirrors
// `TestT0401_AssignSetterCopyInnerOK` but for the wrapped RHS shape.
func TestT0407_AssignSetterFromIfBorrowCopyInnerOK(t *testing.T) {
	ownerOK(t, `
		test() {
			m := Mutex[int](1);
			use guard := m.lock();
			cond := true;
			guard.borrow = if cond { guard.borrow } else { guard.borrow };
		}
	`)
}

// T0407 — bug repro case (4): `~T` consume-site with if-wrapped borrow.
// Sema's T0438 (Rule 8b/8c gated on Copy) rejects this first because the
// joined arm type `string&` cannot decay implicitly to `string` for non-
// Copy T. Pinned here to satisfy the bug's "all four shapes" test plan and
// as defense-in-depth: if T0438 ever regresses, ownership's type-driven
// check at the top of `tryMoveConsume` is the next line of defense.
func TestT0407_ConsumeArgFromIfBorrow(t *testing.T) {
	errs := ownerErrs(t, `
		consume_string(string move s) {}
		test() {
			a := Ref[string]("hi" + "");
			cond := true;
			consume_string(if cond { a.borrow } else { a.borrow });
		}
	`)
	expectOwnerError(t, errs, "cannot assign string& to parameter 's'")
}

// === T0411: `this.field` move from droppable owner ===
//
// Before T0411, `this.field` slipped past the B0341 field-move check because
// `isValueTarget` only recognized IdentExpr/CallExpr roots — never ThisExpr.
// Heap user-type fields shallow-copied silently, leading to double-free at
// runtime. Auto-dup field types (string, Vector, Channel, Arc) are still
// allowed because codegen handles them via dupStringFieldAccess /
// dupContainerFieldAccess.

func TestT0411_VarDeclFromThisFieldUserTypeRejected(t *testing.T) {
	errs := ownerErrs(t, `
		type Inner { string label; drop(~this) {} }
		type Outer {
			Inner inner;
			drop(~this) {}
			extract() Inner {
				i := this.inner;
				return i;
			}
		}
		test() {}
	`)
	expectOwnerError(t, errs, "cannot move field 'inner'")
}

func TestT0411_ReturnThisFieldUserTypeRejected(t *testing.T) {
	errs := ownerErrs(t, `
		type Inner { string label; drop(~this) {} }
		type Outer {
			Inner inner;
			drop(~this) {}
			extract() Inner {
				return this.inner;
			}
		}
		test() {}
	`)
	expectOwnerError(t, errs, "cannot move field 'inner'")
}

func TestT0411_ConstructorFieldInitFromThisFieldUserTypeRejected(t *testing.T) {
	errs := ownerErrs(t, `
		type Inner { string label; drop(~this) {} }
		type Outer {
			Inner inner;
			drop(~this) {}
			clone() Outer {
				return Outer(inner: this.inner);
			}
		}
		test() {}
	`)
	expectOwnerError(t, errs, "cannot move field 'inner'")
}

func TestT0411_FunctionConsumeArgFromThisFieldUserTypeRejected(t *testing.T) {
	errs := ownerErrs(t, `
		type Inner { string label; drop(~this) {} }
		consume(Inner move i) {}
		type Outer {
			Inner inner;
			drop(~this) {}
			send() {
				consume(this.inner);
			}
		}
		test() {}
	`)
	expectOwnerError(t, errs, "cannot move field 'inner'")
}

func TestT0411_ConsumeReceiverFieldUserTypeRejected(t *testing.T) {
	// `~this` consume-receiver: even though `this` is consumed, B0341's
	// design demands `.clone()` for non-auto-dup heap user-type fields —
	// consistent with owned-local behavior.
	errs := ownerErrs(t, `
		type Inner { string label; drop(~this) {} }
		type Outer {
			Inner inner;
			drop(~this) {}
			destroy(~this) Inner {
				return this.inner;
			}
		}
		test() {}
	`)
	expectOwnerError(t, errs, "cannot move field 'inner'")
}

func TestT0411_StringFieldFromThisOK(t *testing.T) {
	// Strings are auto-dup — codegen handles via dupStringFieldAccess. No error.
	ownerOK(t, `
		type CB {
			string label;
			drop(~this) {}
			clone() CB {
				return CB(label: this.label);
			}
		}
		test() {}
	`)
}

func TestT0411_VectorFieldFromThisOK(t *testing.T) {
	// Vector[T] is auto-dup — codegen handles via dupContainerFieldAccess.
	ownerOK(t, `
		type V {
			int[] items;
			drop(~this) {}
			clone() V {
				return V(items: this.items);
			}
		}
		test() {}
	`)
}

func TestT0411_PrimitiveFieldFromThisOK(t *testing.T) {
	// Primitive (Copy) fields — no double-drop risk, no error.
	ownerOK(t, `
		type C {
			int n;
			string label;
			drop(~this) {}
			clone() C {
				return C(n: this.n, label: this.label);
			}
		}
		test() {}
	`)
}

func TestT0411_ExplicitCloneFromThisFieldOK(t *testing.T) {
	// The documented workaround: explicit .clone() returns an owned temp,
	// so the MemberExpr root is a CallExpr and the check passes.
	ownerOK(t, `
		type Inner {
			string label;
			drop(~this) {}
			clone() Inner { return Inner(label: this.label); }
		}
		type Outer {
			Inner inner;
			drop(~this) {}
			clone() Outer {
				return Outer(inner: this.inner.clone());
			}
		}
		test() {}
	`)
}

func TestT0411_FieldlessEnumFromThisOK(t *testing.T) {
	// Fieldless enum is non-Copy but non-droppable — safe to shallow-copy,
	// no error.
	ownerOK(t, `
		enum Tag { A; B; C; }
		type Tagged {
			string label;
			Tag tag;
			drop(~this) {}
			clone() Tagged {
				return Tagged(label: this.label, tag: this.tag);
			}
		}
		test() {}
	`)
}

func TestT0411_TupleLitElementFromThisFieldUserTypeRejected(t *testing.T) {
	// Tuple literal element from `this.field` for a non-auto-dup heap user-type
	// field hits the same B0341 path as constructor field-init / return.
	errs := ownerErrs(t, `
		type Inner { string label; drop(~this) {} }
		type Outer {
			Inner inner;
			drop(~this) {}
			pair() (Inner, int) {
				return (this.inner, 42);
			}
		}
		test() {}
	`)
	expectOwnerError(t, errs, "cannot move field 'inner'")
}

func TestT0411_MapFieldFromThisRejected(t *testing.T) {
	// Map[K, V] is not in isAutoDupType — sema rejects with B0341. This is the
	// shape that surfaced via modules/http/http.pr's response_headers getter.
	errs := ownerErrs(t, `
		type H {
			map[string, string] headers;
			drop(~this) {}
			clone() H {
				return H(headers: this.headers);
			}
		}
		test() {}
	`)
	expectOwnerError(t, errs, "cannot move field 'headers'")
}

func TestT0411_OptionalStringFieldFromThisOK(t *testing.T) {
	// Optional[string] is auto-dup — sema allows; codegen handles via
	// dupStringFieldAccess set in maybeEnableDupForConstructorArg.
	ownerOK(t, `
		type O {
			string? subtitle;
			drop(~this) {}
			clone() O {
				return O(subtitle: this.subtitle);
			}
		}
		test() {}
	`)
}

func TestT0411_ChannelFieldFromThisOK(t *testing.T) {
	// Channel is auto-dup — sema allows; codegen handles via
	// dupContainerFieldAccess set in maybeEnableDupForConstructorArg.
	ownerOK(t, `
		type ChH {
			channel[int] ch;
			drop(~this) {}
			clone() ChH {
				return ChH(ch: this.ch);
			}
		}
		test() {}
	`)
}

// T0568: a Borrowed param of a non-auto-dup heap user type cannot be moved
// into a typed var-decl — both the caller and the new local would drop the
// same heap allocation at scope exit (runtime double-free / segfault).
func TestT0568_TypedDeclBorrowedParamUserTypeRejected(t *testing.T) {
	errs := ownerErrs(t, `
		type _BoxStr { string s; }
		dup_param(_BoxStr b) {
			_BoxStr c = b;
		}
		test() {}
	`)
	expectOwnerError(t, errs, "cannot move borrowed parameter 'b'")
}

// T0568: same shape as TypedDecl but via the inferred var-decl path (`c := b`).
func TestT0568_InferredDeclBorrowedParamUserTypeRejected(t *testing.T) {
	errs := ownerErrs(t, `
		type _BoxStr { string s; }
		dup_param(_BoxStr b) {
			c := b;
		}
		test() {}
	`)
	expectOwnerError(t, errs, "cannot move borrowed parameter 'b'")
}

// T0568 carve-out: `string c = s` from a borrowed string param remains
// allowed — string is auto-dup and codegen clears the LHS drop flag.
func TestT0568_TypedDeclBorrowedStringParamAllowed(t *testing.T) {
	ownerOK(t, `
		dup_str(string s) {
			string c = s;
		}
		test() {}
	`)
}

// T0568 carve-out: `int[] c = v` from a borrowed vector param remains allowed.
func TestT0568_TypedDeclBorrowedVectorParamAllowed(t *testing.T) {
	ownerOK(t, `
		dup_vec(int[] v) {
			int[] c = v;
		}
		test() {}
	`)
}

// T0568 carve-out: handle types (Arc, Mutex, etc.) remain allowed at the
// var-decl site because codegen's drop-flag-propagation handles them
// safely (mirrors T0556's existing Mutex carve-out). Regression guard for
// the isVarDeclAliasSafeType match-up with codegen's
// isDroppableContainerOrString set.
func TestT0568_TypedDeclBorrowedArcParamAllowed(t *testing.T) {
	ownerOK(t, `
		dup_arc(Ref[int] a) {
			Ref[int] c = a;
		}
		test() {}
	`)
}

// T0568 carve-out coverage: Channel[T] is in isVarDeclAliasSafeType's
// codegen-safe set. Without this test the Channel branch of
// isVarDeclAliasSafeType goes uncovered.
func TestT0568_TypedDeclBorrowedChannelParamAllowed(t *testing.T) {
	ownerOK(t, `
		dup_ch(Channel[int] ch) {
			Channel[int] c = ch;
		}
		test() {}
	`)
}

// T0568 carve-out coverage: Weak[T] is in isVarDeclAliasSafeType's
// codegen-safe set.
func TestT0568_TypedDeclBorrowedWeakParamAllowed(t *testing.T) {
	ownerOK(t, `
		dup_weak(Weak[int] w) {
			Weak[int] c = w;
		}
		test() {}
	`)
}

// T1102: MutexGuard[T] is a single-owner handle with no clone/dup — aliasing a
// borrowed param into an owned local is unsound the moment it escapes, so the
// var-decl is rejected outright (formerly a T0568 carve-out, now corrected).
func TestT0568_TypedDeclBorrowedMutexGuardParamRejected(t *testing.T) {
	errs := ownerErrs(t, `
		dup_guard(MutexGuard[int] g) {
			MutexGuard[int] c = g;
		}
		test() {}
	`)
	expectOwnerError(t, errs, "cannot move borrowed parameter")
}

// T1102: Task[T] is a single-owner handle with no clone/dup — the borrowed-param
// alias is rejected at the var-decl (formerly a T0568 carve-out).
func TestT0568_TypedDeclBorrowedTaskParamRejected(t *testing.T) {
	errs := ownerErrs(t, `
		dup_task(Task[int] t) {
			Task[int] c = t;
		}
		test() {}
	`)
	expectOwnerError(t, errs, "cannot move borrowed parameter")
}

// T1102: Mutex[T] is a single-owner handle with no clone/dup — the borrowed-param
// alias is rejected at the var-decl (formerly a T0568 carve-out).
func TestT0568_TypedDeclBorrowedMutexParamRejected(t *testing.T) {
	errs := ownerErrs(t, `
		dup_mutex(Mutex[int] m) {
			Mutex[int] c = m;
		}
		test() {}
	`)
	expectOwnerError(t, errs, "cannot move borrowed parameter")
}

// T1102: returning a borrowed (non-`move`) Task[T] param as owned is rejected —
// the handle has no clone, so the caller's result would alias its still-live
// source local and both ends would drop the one handle (double-free / UAF).
func TestT1102_ReturnBorrowedTaskParamRejected(t *testing.T) {
	errs := ownerErrs(t, `
		pass_borrow(Task[int] t) Task[int] { return t; }
		test() {}
	`)
	expectOwnerError(t, errs, "cannot return borrowed parameter")
}

// T1102: Mutex[T] parity for the direct-return rejection.
func TestT1102_ReturnBorrowedMutexParamRejected(t *testing.T) {
	errs := ownerErrs(t, `
		pass_borrow(Mutex[int] m) Mutex[int] { return m; }
		test() {}
	`)
	expectOwnerError(t, errs, "cannot return borrowed parameter")
}

// T1102: MutexGuard[T] parity for the direct-return rejection.
func TestT1102_ReturnBorrowedMutexGuardParamRejected(t *testing.T) {
	errs := ownerErrs(t, `
		pass_borrow(MutexGuard[int] g) MutexGuard[int] { return g; }
		test() {}
	`)
	expectOwnerError(t, errs, "cannot return borrowed parameter")
}

// T1102: the laundered escape — `Task[int] x = t; return x;` — is rejected at
// the var-decl site (the alias into an owned binding), before it can escape.
func TestT1102_ReturnLaunderedBorrowedTaskParamRejected(t *testing.T) {
	errs := ownerErrs(t, `
		pass_borrow(Task[int] t) Task[int] {
			Task[int] x = t;
			return x;
		}
		test() {}
	`)
	expectOwnerError(t, errs, "cannot move borrowed parameter")
}

// T1102: a `move` Task[T] param returned as owned stays valid — the move
// transfers the single handle's ownership to the caller's result.
func TestT1102_ReturnMovedTaskParamAllowed(t *testing.T) {
	ownerOK(t, `
		pass_move(Task[int] move t) Task[int] { return t; }
		test() {}
	`)
}

// T1102: returning a freshly-created task (owned local / direct `go`) stays
// valid — there is no aliasing source local to double-free.
func TestT1102_ReturnFreshTaskAllowed(t *testing.T) {
	ownerOK(t, `
		worker() int { return 42; }
		get_task() Task[int] { return go worker(); }
		make_task() Task[int] {
			Task[int] t = go worker();
			return t;
		}
		test() {}
	`)
}

// T1177: awaiting a single-owner Task handle bound via an `if is`-destructure of
// an ENUM variant (`if b is Has(job) { <-job }`) is rejected — the escape-dup
// logic cannot clone the handle, so it aliases the subject's field which drops it
// once at scope exit; consuming it via `<-` would join+free the same handle twice.
func TestT1177_AwaitEnumDestructureTaskRejected(t *testing.T) {
	errs := ownerErrs(t, `
		worker() int { return 42; }
		enum HBox { Has(Task[int] job), Nothing }
		test() {
			HBox b = HBox.Has(job: go worker());
			if b is Has(job) {
				int r = <-job;
			}
		}
	`)
	expectOwnerError(t, errs, "cannot await borrowed task value 'job'")
}

// T1177: the named/subtype `if is`-destructure path (`if s is HNamed(job)`) has
// the same double-free shape as the enum path and is rejected identically.
func TestT1177_AwaitNamedDestructureTaskRejected(t *testing.T) {
	errs := ownerErrs(t, `
		worker() int { return 42; }
		type HShape {}
		type HNamed is HShape { Task[int] job; }
		test() {
			HShape s = HNamed(job: go worker());
			if s is HNamed(job) {
				int r = <-job;
			}
		}
	`)
	expectOwnerError(t, errs, "cannot await borrowed task value 'job'")
}

// T1177: a non-await consume of the borrowed handle binding — moving it out into
// an owned var-decl — is rejected too (the binding is Borrowed for the then-block,
// so tryMoveConsume catches the move-out of a borrow).
func TestT1177_MoveOutEnumDestructureTaskRejected(t *testing.T) {
	errs := ownerErrs(t, `
		worker() int { return 42; }
		enum HBox { Has(Task[int] job), Nothing }
		test() {
			HBox b = HBox.Has(job: go worker());
			if b is Has(job) {
				Task[int] c = job;
			}
		}
	`)
	expectOwnerError(t, errs, "borrowed")
}

// T1177: binding the handle and leaving it untouched (subject drops it exactly
// once) is drop-safe and must stay legal — only *consuming* the borrow is rejected.
func TestT1177_NonConsumingEnumDestructureTaskAllowed(t *testing.T) {
	ownerOK(t, `
		worker() int { return 42; }
		enum HBox { Has(Task[int] job), Nothing }
		test() {
			HBox b = HBox.Has(job: go worker());
			if b is Has(job) {
				int n = 1;
			}
		}
	`)
}

// T1177 regression guard: a `match` arm consuming a single-owner handle payload
// out of an OWNED subject stays legal — match consumes the subject (T0623
// ownership transfer), unlike the non-consuming `if is` borrow.
func TestT1177_MatchConsumeTaskStillAllowed(t *testing.T) {
	ownerOK(t, `
		worker() int { return 42; }
		enum HBox { Has(Task[int] job), Nothing }
		test() {
			HBox b = HBox.Has(job: go worker());
			int r = match b {
				HBox.Has(job) => <-job,
				HBox.Nothing => 0,
			};
		}
	`)
}

// T1177: a destructure binding whose payload is NOT a single-owner handle (here
// an `int`) is left untouched by markDestructureHandleBindingsBorrowed — the
// FirstNestedSingleOwnerHandle guard skips it (the `continue` path), so no
// binding is marked and the helper returns a no-op restore. Consuming/using such
// a value stays legal.
func TestT1177_NonHandleDestructureBindingUnaffected(t *testing.T) {
	ownerOK(t, `
		enum IBox { Has(int n), Nothing }
		test() {
			IBox b = IBox.Has(n: 7);
			if b is Has(n) {
				int r = n + 1;
			}
		}
	`)
}

// T1177: the borrowed-for-the-then-block mark is scoped to the destructure
// binding and must NOT leak to an outer variable of the same name it shadows.
// Here an owned `job` in scope is shadowed by the `if is` destructure binding;
// after the then-block the outer `job` is restored to Owned, so awaiting it
// (it is genuinely owned and dropped once) stays legal. Exercises the
// present-restore branch of markDestructureHandleBindingsBorrowed's closure.
func TestT1177_HandleBindingShadowRestoresOuterOwned(t *testing.T) {
	ownerOK(t, `
		worker() int { return 42; }
		enum HBox { Has(Task[int] job), Nothing }
		test() {
			HBox b = HBox.Has(job: go worker());
			Task[int] job = go worker();
			if b is Has(job) {
				int n = 1;
			}
			int r = <-job;
		}
	`)
}

// T1102: a single-owner handle reaching the var-decl alias reject as a NON-param
// Borrowed local (here via tuple destructuring of an owning aggregate field, the
// same shape as TestT0568_TypedDeclDestructuredBorrowRejected) takes the
// non-param else branch in rejectBorrowedIdentVarDecl — the
// `singleOwnerHandleKind` check fires before isVarDeclAliasSafeType and rejects
// with the "cannot move borrowed value" diagnostic (not the parameter form).
func TestT1102_VarDeclDestructuredBorrowMutexRejected(t *testing.T) {
	errs := ownerErrs(t, `
		type Holder { (Mutex[int], int) pair; }
		test() {
			Holder h = Holder(pair: (Mutex[int](1), 2));
			(b, n) := h.pair;
			Mutex[int] c = b;
		}
	`)
	expectOwnerError(t, errs, "cannot move borrowed value 'b'")
}

// T0568 carve-out coverage: Optional wrapping of a codegen-safe type
// (e.g., `Channel[int]?`) still routes through the recursive Optional
// branch of isVarDeclAliasSafeType. Without an Optional-wrapped non-string
// codegen-safe type, that recursion only fires from the string-Optional
// path covered indirectly elsewhere.
func TestT0568_TypedDeclBorrowedOptionalChannelParamAllowed(t *testing.T) {
	ownerOK(t, `
		dup_opt_ch(Channel[int]? ch) {
			Channel[int]? c = ch;
		}
		test() {}
	`)
}

// T0568 carve-out: when LHS adds an Optional wrap over the RHS type
// (`_Box?? b = a` with `a: _Box?`), sema inserts an implicit `Some` and
// codegen produces a wrapped value rather than a runtime alias. T0568 only
// targets pure-alias shapes (LHS depth ≤ RHS depth), so this case must keep
// compiling — regression guard for tests/e2e/optional_heap_iflet_test.pr's
// `_returns_triple` shape.
func TestT0568_TypedDeclOptionalWrapBorrowedParamAllowed(t *testing.T) {
	ownerOK(t, `
		type _Box { int n; }
		wrap_into_outer(_Box? a) _Box?? {
			_Box?? b = a;
			return b;
		}
		test() {}
	`)
}

// T0568: a `~` consume parameter has state Owned, not Borrowed, so the
// typed var-decl form is accepted (the move transfers ownership).
func TestT0568_TypedDeclMutParamUserTypeOK(t *testing.T) {
	ownerOK(t, `
		type _BoxStr { string s; }
		consume(_BoxStr move b) {
			_BoxStr c = b;
		}
		test() {}
	`)
}

// T0568 + T0548 interaction: a destructured local from a MemberExpr source
// is marked Borrowed; binding it into a fresh owned typed var-decl must be
// rejected with the non-param ("cannot move borrowed value") diagnostic.
func TestT0568_TypedDeclDestructuredBorrowRejected(t *testing.T) {
	errs := ownerErrs(t, `
		type _BoxStr { string s; }
		type Holder { (_BoxStr, int) pair; }
		test() {
			Holder h = Holder(pair: (_BoxStr(s: "x"), 2));
			(b, n) := h.pair;
			_BoxStr c = b;
		}
	`)
	expectOwnerError(t, errs, "cannot move borrowed value 'b'")
}

// T0568: same-depth Optional alias (`_BoxStr? c = b` with `b: _BoxStr?`)
// is still a double-free shape — both LHS and RHS wrap the same heap _BoxStr
// allocation, and codegen does not auto-dup Optional[heap-user-type].
func TestT0568_TypedDeclOptionalSameDepthRejected(t *testing.T) {
	errs := ownerErrs(t, `
		type _BoxStr { string s; }
		opt_alias(_BoxStr? b) {
			_BoxStr? c = b;
		}
		test() {}
	`)
	expectOwnerError(t, errs, "cannot move borrowed parameter 'b'")
}

// T0568: an inheritance upcast (`Parent p = childParam`) aliases at the
// instance pointer; both p and childParam would drop the same heap
// allocation. Confirmed runtime-segfault pre-fix.
func TestT0568_TypedDeclInheritanceUpcastRejected(t *testing.T) {
	errs := ownerErrs(t, `
		type Parent { int x; }
		type Child is Parent {
			int y;
			new(~this, int x, int y) { this.x = x; this.y = y; }
		}
		upcast(Child c) {
			Parent p = c;
		}
		test() {}
	`)
	expectOwnerError(t, errs, "cannot move borrowed parameter 'c'")
}

// --- T0586 / T0964 ---
// T0586 originally rejected passing a Borrowed non-Copy, non-auto-dup,
// droppable value to a plain (non-`~`, non-`&`) callee parameter, on the
// theory that no codegen-side dup existed so the caller's drop and the
// callee's drop would fire on the same allocation. T0964 corrects the model:
// a plain `T` parameter of a *general* call is a SHARED BORROW (the caller
// retains ownership and is the sole dropper; the callee never drops a plain-T
// arg), so these cases are now ACCEPTED as borrows — no double-free. The
// consume/dup rejection survives only for the container-store native path
// (Vector.push), which still takes ownership of its element; those are covered
// by the TestT0556_Push* tests. The taxonomy tests below now all assert that
// diverse droppable types (heap user type, generic, Map, Set, enum, tuple,
// Optional) borrow cleanly through a plain-T general call.
func TestT0586_CallPlainBorrowedParamUserTypeBorrows(t *testing.T) {
	ownerOK(t, `
		type _BoxStr { string s; }
		take(_BoxStr b) {}
		forward(_BoxStr s) {
			take(s);
		}
		test() {}
	`)
}

// T0586/T0964: generic instance — `_Holder[T]` with a string field is a
// heap-user type after substitution. Once a droppable generic instance, it
// now borrows cleanly through a plain-T general call (no consume).
func TestT0586_CallPlainBorrowedParamGenericTypeBorrows(t *testing.T) {
	ownerOK(t, `
		type _Holder[T] { T value; }
		take(_Holder[string] h) {}
		forward(_Holder[string] h) {
			take(h);
		}
		test() {}
	`)
}

// T0586/T0964: Map[K,V] is a heap container with synth drop. It borrows
// cleanly through a plain-T general call.
func TestT0586_CallPlainBorrowedParamMapBorrows(t *testing.T) {
	ownerOK(t, `
		take(map[string, int] m) {}
		forward(map[string, int] m) {
			take(m);
		}
		test() {}
	`)
}

// T0586/T0964: Set[T] follows the same shape as Map — a heap container that
// borrows cleanly through a plain-T general call.
func TestT0586_CallPlainBorrowedParamSetBorrows(t *testing.T) {
	ownerOK(t, `
		take(Set[int] s) {}
		forward(Set[int] s) {
			take(s);
		}
		test() {}
	`)
}

// T0586/T0964: a plain-T value argument borrows whether the callee is a free
// function or a method (the receiver borrow is orthogonal to the call-arg
// borrow).
func TestT0586_CallPlainBorrowedMethodArgBorrows(t *testing.T) {
	ownerOK(t, `
		type _BoxStr { string s; }
		type Holder {
			take(this, _BoxStr b) {}
		}
		forward(_BoxStr s) {
			h := Holder();
			h.take(s);
		}
		test() {}
	`)
}

// T0586/T0964 wrapper coverage: paren-wrapped `take((s))` — the borrowed
// ident surfaced through a ParenExpr borrows cleanly.
func TestT0586_CallPlainBorrowedThroughParenBorrows(t *testing.T) {
	ownerOK(t, `
		type _BoxStr { string s; }
		take(_BoxStr b) {}
		forward(_BoxStr s) {
			take((s));
		}
		test() {}
	`)
}

// T0586/T0964 wrapper coverage: if-expression branches that surface a
// borrowed ident — the value borrows cleanly through the plain-T call.
func TestT0586_CallPlainBorrowedThroughIfElseBorrows(t *testing.T) {
	ownerOK(t, `
		type _BoxStr { string s; }
		take(_BoxStr b) {}
		forward(_BoxStr s, bool flag) {
			take(if flag { s } else { s });
		}
		test() {}
	`)
}

// T0586/T0964 wrapper coverage: match arm Body (`=> expr`) form — a borrowed
// ident surfaced from an arm Body borrows cleanly.
func TestT0586_CallPlainBorrowedThroughMatchBodyBorrows(t *testing.T) {
	ownerOK(t, `
		type _BoxStr { string s; }
		take(_BoxStr b) {}
		forward(_BoxStr s, int k) {
			take(match k { 1 => s, _ => s });
		}
		test() {}
	`)
}

// T0586/T0964 wrapper coverage: match arm Block (`=> { stmts; expr }`) form
// — a borrowed ident surfaced from an arm Block borrows cleanly.
func TestT0586_CallPlainBorrowedThroughMatchBlockBorrows(t *testing.T) {
	ownerOK(t, `
		type _BoxStr { string s; }
		take(_BoxStr b) {}
		forward(_BoxStr s, int k) {
			take(match k { 1 => { s }, _ => { s } });
		}
		test() {}
	`)
}

// T0586/T0964: string is an auto-dup container; passing it through a plain-T
// callee borrows cleanly. Regression guard (was always allowed; pre-T0964 via
// the call-site auto-dup carve-out, now uniformly via plain-T borrow).
func TestT0586_CallPlainBorrowedParamStringAllowed(t *testing.T) {
	ownerOK(t, `
		take(string s) {}
		forward(string s) {
			take(s);
		}
		test() {}
	`)
}

// T0586/T0964: Vector[T] borrows cleanly through a plain-T call.
func TestT0586_CallPlainBorrowedParamVectorAllowed(t *testing.T) {
	ownerOK(t, `
		take(int[] v) {}
		forward(int[] v) {
			take(v);
		}
		test() {}
	`)
}

// T0586/T0964: Ref[T] borrows cleanly through a plain-T call.
func TestT0586_CallPlainBorrowedParamArcAllowed(t *testing.T) {
	ownerOK(t, `
		take(Ref[int] a) {}
		forward(Ref[int] a) {
			take(a);
		}
		test() {}
	`)
}

// T0586/T0964: Optional[Channel[int]] borrows cleanly through a plain-T
// call.
func TestT0586_CallPlainBorrowedParamOptionalChannelAllowed(t *testing.T) {
	ownerOK(t, `
		take(Channel[int]? ch) {}
		forward(Channel[int]? ch) {
			take(ch);
		}
		test() {}
	`)
}

// T0586/T0964: a `~_BoxStr` callee param consumes the local via the move
// path (the local's state is Owned, so the consume succeeds). Regression
// guard that the `~`-param consume path is unaffected by the plain-T borrow
// reclassification.
func TestT0586_CallMutParamUserTypeOK(t *testing.T) {
	ownerOK(t, `
		type _BoxStr { string s; }
		take(_BoxStr b) {}
		forward(_BoxStr move s) {
			take(s);
		}
		test() {}
	`)
}

// T0586/T0964 non-parameter coverage: a destructured local from a MemberExpr
// source is marked Borrowed (T0548); passing it as a plain-T general call arg
// now borrows cleanly (the caller retains ownership).
func TestT0586_CallPlainBorrowedDestructuredLocalBorrows(t *testing.T) {
	ownerOK(t, `
		type _BoxStr { string s; }
		type Holder { (_BoxStr, int) pair; }
		take(_BoxStr b) {}
		forward() {
			h := Holder(pair: (_BoxStr(s: "x"), 2));
			(b, _) := h.pair;
			take(b);
		}
		test() {}
	`)
}

// T0586/T0964 enum coverage: a plain enum with a droppable variant payload
// (e.g. a string field) has NeedsSynthDrop=true via the synthesized
// variant-data drop function. A borrowed enum-typed param borrows cleanly
// through a plain-T general call — the caller remains the sole dropper.
func TestT0586_CallPlainBorrowedParamEnumBorrows(t *testing.T) {
	ownerOK(t, `
		enum Msg { Text(string s); Ping; }
		take(Msg m) {}
		forward(Msg m) {
			take(m);
		}
		test() {}
	`)
}

// T0586/T0964 generic enum instance coverage: `Maybe[_BoxStr]` substitutes a
// droppable heap user type into the `Just(T)` variant (synth drop via
// monoEnumInstNeedsSynthDrop; T0506 added the recursion). A borrowed value of
// this type borrows cleanly through a plain-T general call.
func TestT0586_CallPlainBorrowedParamGenericEnumBorrows(t *testing.T) {
	ownerOK(t, `
		type _BoxStr { string s; }
		enum Maybe[T] { Just(T val); Nothing; }
		take(Maybe[_BoxStr] m) {}
		forward(Maybe[_BoxStr] m) {
			take(m);
		}
		test() {}
	`)
}

// T0586/T0964 Optional-of-heap-user-type coverage: `_BoxStr?` is droppable
// (Optional of a droppable heap user type). A borrowed value of this type
// borrows cleanly through a plain-T general call.
func TestT0586_CallPlainBorrowedParamOptionalHeapUserTypeBorrows(t *testing.T) {
	ownerOK(t, `
		type _BoxStr { string s; }
		take(_BoxStr? b) {}
		forward(_BoxStr? b) {
			take(b);
		}
		test() {}
	`)
}

// T0586/T0964 Tuple coverage: a tuple-typed param like `(_BoxStr, int)` is
// droppable (any droppable element makes the tuple droppable). A borrowed
// value of this type borrows cleanly through a plain-T general call.
func TestT0586_CallPlainBorrowedParamTupleBorrows(t *testing.T) {
	ownerOK(t, `
		type _BoxStr { string s; }
		take((_BoxStr, int) p) {}
		forward((_BoxStr, int) p) {
			take(p);
		}
		test() {}
	`)
}

// T0586/T0964: a plain tag-only enum (NeedsSynthDrop=false, HasDrop=false) is
// a Copy-like value; passing it through a plain-T call is a no-op copy.
// Regression guard that pure tag-only enums still pass freely.
func TestT0586_CallPlainBorrowedParamPlainEnumAllowed(t *testing.T) {
	ownerOK(t, `
		enum Color { Red; Green; Blue; }
		take(Color c) {}
		forward(Color c) {
			take(c);
		}
		test() {}
	`)
}

// T0586 carve-out: `int?` is not droppable (Optional[primitive] recurses to
// a non-droppable element), so the predicate must not fire. Regression
// guard alongside the Optional[Channel] carve-out which exercises the
// `isVarDeclAliasSafeType` Optional path; this case exercises the
// !isDroppableType short-circuit.
func TestT0586_CallPlainBorrowedParamOptionalPrimitiveAllowed(t *testing.T) {
	ownerOK(t, `
		take(int? n) {}
		forward(int? n) {
			take(n);
		}
		test() {}
	`)
}

// --- T0964 ---
// T0964: a plain (unmarked) move-type parameter is a SHARED BORROW. The caller
// retains ownership and may reuse the value (including passing it again) after
// the call; the callee may NOT move it out (it must declare `~T` to consume).
// These tests pin the borrow/consume boundary for string, a heap user type,
// and a vector — the three representative non-Copy categories.

// Plain `T` borrow + reuse: the argument stays usable after the call, and may
// be passed again to the same plain-T callee.
func TestT0964_PlainStringBorrowReuse(t *testing.T) {
	ownerOK(t, `
		read(string s) int { return s.len; }
		test() {
			string a = "hello";
			int n = read(a);
			int m = read(a);
			int k = a.len;
		}
	`)
}

func TestT0964_PlainHeapBorrowReuse(t *testing.T) {
	ownerOK(t, `
		type Heavy { int x; drop(~this) {} }
		read(Heavy h) int { return h.x; }
		test() {
			Heavy a = Heavy(x: 7);
			int n = read(a);
			int m = read(a);
			int k = a.x;
		}
	`)
}

func TestT0964_PlainVectorBorrowReuse(t *testing.T) {
	ownerOK(t, `
		read(int[] v) int { return v.len; }
		test() {
			int[] v = [1, 2, 3];
			int n = read(v);
			int m = read(v);
			int k = v.len;
		}
	`)
}

// Forwarding a borrowed plain-T param onward to another plain-T callee is a
// clean borrow chain (the old T0586 reject no longer applies to general calls).
func TestT0964_ForwardBorrowedParamOnward(t *testing.T) {
	ownerOK(t, `
		type Heavy { int x; drop(~this) {} }
		read(Heavy h) int { return h.x; }
		forward(Heavy h) int { return read(h); }
		test() {
			Heavy a = Heavy(x: 7);
			int n = forward(a);
			int k = a.x;
		}
	`)
}

// A plain `T` param may NOT be moved out of the callee — the value belongs to
// the caller. Author must write `~T` to consume. Repro #2 shape (string).
func TestT0964_PlainStringMoveOutRejected(t *testing.T) {
	errs := ownerErrs(t, `
		type Box { string val; }
		sink(string s) Box { return Box(val: s); }
		test() {}
	`)
	expectOwnerError(t, errs, "cannot move borrowed parameter 's'; declare the parameter with `move`")
}

func TestT0964_PlainHeapMoveOutRejected(t *testing.T) {
	errs := ownerErrs(t, `
		type Heavy { int x; drop(~this) {} }
		type Box { Heavy h; }
		sink(Heavy h) Box { return Box(h: h); }
		test() {}
	`)
	expectOwnerError(t, errs, "cannot move borrowed parameter 'h'; declare the parameter with `move`")
}

func TestT0964_PlainVectorMoveOutRejected(t *testing.T) {
	errs := ownerErrs(t, `
		type Box { int[] v; }
		sink(int[] v) Box { return Box(v: v); }
		test() {}
	`)
	expectOwnerError(t, errs, "cannot move borrowed parameter 'v'; declare the parameter with `move`")
}

// `~T` continues to consume: the callee may move the value out (into a field).
// Caller loses ownership.
func TestT0964_MutParamConsumeMoveOut(t *testing.T) {
	ownerOK(t, `
		type Heavy { int x; drop(~this) {} }
		type Box { Heavy h; }
		sink(Heavy move h) Box { return Box(h: move h); }
		test() {
			Heavy a = Heavy(x: 7);
			Box b = sink(move a);
		}
	`)
}

// `~T` consume + move-out for a vector — the callee may move the value into a
// field; the caller loses ownership (parallels the heap-user-type case above).
func TestT0964_MutVectorParamConsumeMoveOut(t *testing.T) {
	ownerOK(t, `
		type VecBox { int[] v; }
		sink(int[] move v) VecBox { return VecBox(v: move v); }
		test() {
			int[] a = [1, 2, 3];
			VecBox b = sink(move a);
		}
	`)
}

// `~T` consume marks the caller's variable moved — reuse after is rejected.
func TestT0964_MutParamConsumeUseAfterRejected(t *testing.T) {
	errs := ownerErrs(t, `
		type Heavy { int x; drop(~this) {} }
		consume(Heavy move h) {}
		test() {
			Heavy a = Heavy(x: 7);
			consume(a);
			int k = a.x;
		}
	`)
	expectOwnerError(t, errs, "use of moved variable 'a'")
}

// Copy-type params auto-copy — caller retains the value (unchanged by T0964).
func TestT0964_CopyTypePassthrough(t *testing.T) {
	ownerOK(t, `
		inc(int n) int { return n + 1; }
		test() {
			int x = 5;
			int y = inc(x);
			int z = inc(x);
			int w = x + 1;
		}
	`)
}

// `x = f(x)` self-assign: the transient call-scoped borrow on `v` produced by
// the reassignment's own RHS (plain `T[]` param) must not block reassigning v.
func TestT0964_SelfAssignPlainParam(t *testing.T) {
	ownerOK(t, `
		roundtrip(int[] v) int[] { return [v.len]; }
		test() {
			int[] v = [1, 2, 3];
			v = roundtrip(v);
			int k = v.len;
		}
	`)
}

// Same for an explicit `T&` param on the RHS — the pre-existing self-assign
// limitation is fixed as a bonus (T0964).
func TestT0964_SelfAssignRefParam(t *testing.T) {
	ownerOK(t, `
		combine(int[] v) int[] { return [v.len]; }
		test() {
			int[] v = [1, 2, 3];
			v = combine(v);
			int k = v.len;
		}
	`)
}

// A call-scoped plain-T borrow expires at statement end, so a later `~` consume
// of the same variable in a subsequent statement is allowed (the borrow does
// not persist past the borrowing call). Pins the borrow-then-move sequencing.
func TestT0964_BorrowThenConsumeOK(t *testing.T) {
	ownerOK(t, `
		type Heavy { int x; drop(~this) {} }
		read(Heavy h) int { return h.x; }
		consume(Heavy move h) {}
		test() {
			Heavy a = Heavy(x: 1);
			int n = read(a);
			consume(move a);
		}
	`)
}

// The same variable may be passed to two plain-T params in one call: each slot
// is an independent shared borrow, and shared borrows never conflict.
func TestT0964_DoubleBorrowSameVarInOneCall(t *testing.T) {
	ownerOK(t, `
		type Heavy { int x; drop(~this) {} }
		read2(Heavy a, Heavy b) int { return a.x + b.x; }
		test() {
			Heavy a = Heavy(x: 5);
			int n = read2(a, a);
			int k = a.x;
		}
	`)
}

// A single call may mix a plain-T borrow with a `~T` move of two different
// variables: the borrowed arg stays usable afterward, the moved arg does not.
func TestT0964_MixedBorrowAndMoveInOneCall(t *testing.T) {
	ownerOK(t, `
		type Heavy { int x; drop(~this) {} }
		mix(Heavy a, Heavy move b) int { return a.x + b.x; }
		test() {
			Heavy a = Heavy(x: 3);
			Heavy b = Heavy(x: 4);
			int n = mix(a, move b);
			int k = a.x;
		}
	`)
}

// The moved (`~T`) arg of a mixed call is consumed — using it after is rejected
// even though the plain-T arg of the same call is only borrowed.
func TestT0964_MixedMovedArgUseAfterRejected(t *testing.T) {
	errs := ownerErrs(t, `
		type Heavy { int x; drop(~this) {} }
		mix(Heavy a, Heavy move b) int { return a.x + b.x; }
		test() {
			Heavy a = Heavy(x: 3);
			Heavy b = Heavy(x: 4);
			int n = mix(a, b);
			int k = b.x;
		}
	`)
	expectOwnerError(t, errs, "use of moved variable 'b'")
}

// A plain `this` receiver is a borrow too (the receiver audit): the caller
// retains ownership and may call again / use the value after the method call.
func TestT0964_PlainThisReceiverBorrows(t *testing.T) {
	ownerOK(t, `
		type Heavy { int x; drop(~this) {} get_x(this) int { return this.x; } }
		test() {
			Heavy a = Heavy(x: 5);
			int n = a.get_x();
			int m = a.get_x();
			int k = a.x;
		}
	`)
}

// --- T0964 storeNative (Vector.push) path ---
// Vector.push declares a plain `T elem` but the native store TAKES OWNERSHIP of
// (or dups) the element — so unlike a general plain-T call, push does NOT borrow
// its argument. The T0556 tests pin the single-owner-handle subset (Mutex/Task/
// MutexGuard); these pin the broadened T0586 category (plain heap user type,
// Map) that T0964 now distinguishes via storeNative — caught at the push site
// only, NOT at general calls (which now borrow it cleanly). Plus the auto-dup
// carve-out (string/Vector element) that still pushes a borrowed arg safely.

// Pushing a borrowed plain heap-user-type param is rejected at the store site
// (no codegen dup path → callee element drop + caller drop double-free).
func TestT0964_PushBorrowedPlainHeapParamRejected(t *testing.T) {
	errs := ownerErrs(t, `
		type Heavy { int x; drop(~this) {} }
		take(Heavy h) {
			Heavy[] v = [];
			v.push(h);
		}
		test() {}
	`)
	expectOwnerError(t, errs, "cannot move borrowed parameter 'h'; declare the parameter with `move`")
}

// Pushing a borrowed plain Map element param is rejected for the same reason —
// Map is a non-auto-dup heap container.
func TestT0964_PushBorrowedPlainMapParamRejected(t *testing.T) {
	errs := ownerErrs(t, `
		take(map[string, int] m) {
			map[string, int][] v = [];
			v.push(m);
		}
		test() {}
	`)
	expectOwnerError(t, errs, "cannot move borrowed parameter 'm'; declare the parameter with `move`")
}

// Pushing a borrowed plain string param is allowed — string is auto-dup, so
// codegen dups it at the push site and the caller's owner is untouched.
func TestT0964_PushBorrowedStringParamAllowed(t *testing.T) {
	ownerOK(t, `
		take(string s) {
			string[] v = [];
			v.push(s);
		}
		test() {}
	`)
}

// Pushing a borrowed plain Vector element param is likewise allowed — Vector is
// auto-dup at the push site.
func TestT0964_PushBorrowedVectorParamAllowed(t *testing.T) {
	ownerOK(t, `
		take(int[] inner) {
			int[][] v = [];
			v.push(inner);
		}
		test() {}
	`)
}

// An OWNED local pushed into a Vector is consumed by the store (the storeNative
// tryMove path), so using it after the push is rejected.
func TestT0964_PushOwnedLocalConsumed(t *testing.T) {
	errs := ownerErrs(t, `
		type Heavy { int x; drop(~this) {} }
		test() {
			Heavy[] v = [];
			Heavy h = Heavy(x: 1);
			v.push(h);
			int k = h.x;
		}
	`)
	expectOwnerError(t, errs, "use of moved variable 'h'")
}

// --- T0581 ---
// T0581: passing `this` as a plain (non-`~`, non-`&`) call-arg whose
// parameter slot expects the type's value-struct shape segfaults at runtime
// (heap user type: `{i8*,i8*}` expected, raw `i8*` passed). Sema must
// reject before codegen ever sees it.
func TestT0581_CallArgPlainThisHeapUserTypeRejected(t *testing.T) {
	errs := ownerErrs(t, `
		type Box { int x; }
		type Holder {
			Box b;
			forward(this) int { return consume_holder(this); }
		}
		consume_holder(Holder h) int { return h.b.x; }
		test() {}
	`)
	expectOwnerError(t, errs, "cannot consume 'this'")
}

// T0581: same crash class on pure value-type receivers — destination
// parameter expects `{i8* vtable, fields…}` but the raw `i8*` receiver is
// passed, yielding garbage extractvalue reads.
func TestT0581_CallArgPlainThisValueTypeRejected(t *testing.T) {
	errs := ownerErrs(t, `
		type V {
			int x `+"`value"+`;
			int y `+"`value"+`;
			do_it(this) int { return take_v(this); }
		}
		take_v(V v) int { return v.x + v.y; }
		test() {}
	`)
	expectOwnerError(t, errs, "cannot consume 'this'")
}

// T0581: `~this` receiver variant — same crash class. `~this` grants
// mutate access but the receiver still belongs to the caller, so passing
// it as a plain-T call-arg has the same ABI mismatch + alias double-free.
func TestT0581_CallArgMutThisHeapUserTypeRejected(t *testing.T) {
	errs := ownerErrs(t, `
		type Box { int x; }
		type Holder {
			Box b;
			forward(~this) int { return consume_holder(this); }
		}
		consume_holder(Holder h) int { return h.b.x; }
		test() {}
	`)
	expectOwnerError(t, errs, "cannot consume 'this'")
}

// T0581 carve-out: `this` of a primitive Copy type passed to a free
// function taking the same primitive is safe — the value is the data,
// ABI matches, no drop. modules/std/int.pr's `int.encode` relies on this.
func TestT0581_CallArgThisCopyPrimitiveOK(t *testing.T) {
	ownerOK(t, `
		take_int(int n) int { return n; }
		type IntWrap {
			int v;
			use_it(this) int { return take_int(this.v); }
		}
		test() {}
	`)
}

// T0581 carve-out: passing `this` (an int) directly from inside a wrapper
// method body exercises the primitive branch of isThisCallArgSafe at the
// ThisExpr position. (Cannot extend int via `type` syntax, so this is
// covered transitively by the `IntWrap.use_it` shape above plus the
// existing `int.encode(this, Encoder e)` usage in modules/std/int.pr.)
func TestT0581_CallArgPlainPrimitiveFieldOK(t *testing.T) {
	ownerOK(t, `
		take_int(int n) int { return n; }
		type Counter {
			int n;
			get_doubled(this) int { return take_int(this.n) + take_int(this.n); }
		}
		test() {}
	`)
}

// T0581 carve-out: `this` of `string` (an auto-dup container handle) is
// runtime-safe — codegen uses `i8*` for both value-rep and parameter
// shape, and clones at the call site. modules/std/string.pr's
// `string.write` relies on this.
func TestT0581_CallArgThisStringOK(t *testing.T) {
	ownerOK(t, `
		take_str(string s) {}
		type StrHolder {
			string s;
			use_it(this) { take_str(this.s); }
		}
		test() {}
	`)
}

// T0581 carve-out: passing a `Vector[int]` field as a non-receiver arg
// is safe — Vector is auto-dup at the call-arg site (`_vec_clone`).
func TestT0581_CallArgVectorFieldOK(t *testing.T) {
	ownerOK(t, `
		take_vec(int[] v) int { return v.len; }
		type Holder {
			int[] xs;
			get_size(this) int { return take_vec(this.xs); }
		}
		test() {}
	`)
}

// T0581 borrow-arm coverage: when `this` has an active borrow registered
// (e.g., from `(b, n) := this.pair`), the subsequent call-arg move must
// diagnose with the "while it is borrowed" message rather than the
// generic consume message.
func TestT0581_CallArgThisInBorrowedState(t *testing.T) {
	errs := ownerErrs(t, `
		type _BoxStr { string s; }
		type Holder {
			(_BoxStr, int) pair;
			eat(~this) {
				(b, n) := this.pair;
				_ = consume_holder(this);
				_ = b.s;
				_ = n;
			}
		}
		consume_holder(Holder h) int { return 0; }
		test() {}
	`)
	expectOwnerError(t, errs, "cannot move 'this' while it is borrowed")
}

// T0581 regression guard: `return this;` from a plain or `~this` method
// must keep compiling. Return path uses `wrapThisReturnValue` + B0250
// alias-clear, so it's the one place moving `this` is semantically
// defensible. T0576's regression guard covers the same shape; this one
// makes sure the T0581 call-arg check doesn't leak into ReturnStmt.
func TestT0581_ReturnThisStillCompiles(t *testing.T) {
	ownerOK(t, `
		type Box { int x; }
		type Holder {
			Box b;
			eat(~this) Holder { return this; }
		}
		test() {}
	`)
}

// T0581 wrapper coverage: paren-wrapped `f((this))` reaches the call site
// through a transparent wrapper. Codegen forwards the inner value directly,
// so the same crash class applies. Mirrors the Paren branch of
// findBorrowedNonAliasSafeIdent (T0556).
func TestT0581_CallArgParenThisRejected(t *testing.T) {
	errs := ownerErrs(t, `
		type Box { int x; }
		type Holder {
			Box b;
			forward(this) int { return consume_holder((this)); }
		}
		consume_holder(Holder h) int { return h.b.x; }
		test() {}
	`)
	expectOwnerError(t, errs, "cannot consume 'this'")
}

// T0581 wrapper coverage: if-expression with `this` in a branch — codegen's
// PHI surfaces the raw `i8*` receiver as the if's value.
func TestT0581_CallArgIfBranchThisRejected(t *testing.T) {
	errs := ownerErrs(t, `
		type Box { int x; }
		type Holder {
			Box b;
			int f;
			forward(this) int {
				return consume_holder(if this.f > 0 { this } else { this });
			}
		}
		consume_holder(Holder h) int { return h.b.x; }
		test() {}
	`)
	expectOwnerError(t, errs, "cannot consume 'this'")
}

// T0581 regression guard: `.clone()` is the suggested workaround.
func TestT0581_CloneWorkaroundCompiles(t *testing.T) {
	ownerOK(t, `
		type Box {
			int x;
			clone(this) Box { return Box(x: this.x); }
		}
		type Holder {
			Box b;
			clone(this) Holder { return Holder(b: this.b.clone()); }
			forward(this) int { return consume_holder(this.clone()); }
		}
		consume_holder(Holder h) int { return h.b.x; }
		test() {}
	`)
}

// T0581 wrapper coverage: match-expression with `this` in an arm-body
// (`pattern => this`) — the arm's Body field holds the ThisExpr directly.
// Codegen's match lowering forwards the arm value (no wrap), so the same
// `{i8*,i8*}` vs raw `i8*` ABI mismatch applies. Covers the MatchExpr +
// arm.Body branch of findThisExprInArg.
func TestT0581_CallArgMatchArmBodyThisRejected(t *testing.T) {
	errs := ownerErrs(t, `
		type Box { int x; }
		type Holder {
			Box b;
			int kind;
			forward(this) int {
				return consume_holder(match this.kind {
					0 => this,
					_ => this,
				});
			}
		}
		consume_holder(Holder h) int { return h.b.x; }
		test() {}
	`)
	expectOwnerError(t, errs, "cannot consume 'this'")
}

// T0581 wrapper coverage: match-expression with `this` in an arm block
// (`pattern => { ...; this }`). The arm's Block field holds the block,
// whose trailing ExprStmt is the ThisExpr. Codegen's match lowering
// surfaces the block's trailing value as the arm result, so a `this`
// reaches the call site as the raw receiver pointer with no wrap.
// Covers the MatchExpr + arm.Block branch of findThisExprInArg via
// findThisExprInArgBlock.
func TestT0581_CallArgMatchArmBlockThisRejected(t *testing.T) {
	errs := ownerErrs(t, `
		type Box { int x; }
		type Holder {
			Box b;
			int kind;
			forward(this) int {
				return consume_holder(match this.kind {
					0 => { this },
					_ => this,
				});
			}
		}
		consume_holder(Holder h) int { return h.b.x; }
		test() {}
	`)
	expectOwnerError(t, errs, "cannot consume 'this'")
}

// T0581 carve-out unit test: `isThisCallArgSafe` must return true for
// every primitive scalar singleton (int / float / bool / char / uint /
// void / none) — that's what permits stdlib patterns like
// `int.encode!(Encoder ~e) { e.encode_int(this); }` to keep compiling.
// Stdlib re-checks via the precomputed scope in unit tests, so this
// branch of the helper is not reached by the parsed-source tests above.
// Exercise it directly to guard against accidental shrinkage of the
// carve-out (e.g., a refactor that drops one of the integer aliases).
func TestT0581_IsThisCallArgSafePrimitiveScalars(t *testing.T) {
	cases := []struct {
		name string
		typ  types.Type
	}{
		{"int", types.TypInt},
		{"i8", types.TypI8},
		{"i16", types.TypI16},
		{"i32", types.TypI32},
		{"i64", types.TypI64},
		{"uint", types.TypUint},
		{"u8", types.TypU8},
		{"u16", types.TypU16},
		{"u32", types.TypU32},
		{"u64", types.TypU64},
		{"f32", types.TypF32},
		{"f64", types.TypF64},
		{"bool", types.TypBool},
		{"char", types.TypChar},
		{"none", types.TypNone},
		{"void", types.TypVoid},
	}
	for _, tc := range cases {
		if !isThisCallArgSafe(tc.typ) {
			t.Errorf("isThisCallArgSafe(%s) = false, want true (primitive carve-out)", tc.name)
		}
	}
	if isThisCallArgSafe(nil) {
		t.Errorf("isThisCallArgSafe(nil) = true, want false (defensive nil branch)")
	}
}

// --- T0589 ---
// T0589: an `if x := a { … }` (or `while x := a { … }`) on a non-`~` Optional
// parameter whose inner type is droppable double-frees with the caller. The
// if-let / while-let binding takes ownership of the inner heap value (drops at
// scope exit), but the caller still owns and drops the same allocation. Sema
// must reject before codegen sees the unsafe shape. Mirrors T0586's call-arg
// reject pattern.

// Plain heap user type inner: `_Box?` borrowed param consumed via if-let.
func TestT0589_IfLetBorrowedHeapUserOptionalRejected(t *testing.T) {
	errs := ownerErrs(t, `
		type _Box { int n; }
		take(_Box? a) {
			if x := a {
				_ = x;
			}
		}
		test() {}
	`)
	expectOwnerError(t, errs, "cannot consume borrowed parameter 'a' via if-let")
}

// Heap string inner: `string?` borrowed param.
func TestT0589_IfLetBorrowedStringOptionalRejected(t *testing.T) {
	errs := ownerErrs(t, `
		take(string? s) {
			if x := s {
				_ = x;
			}
		}
		test() {}
	`)
	expectOwnerError(t, errs, "cannot consume borrowed parameter 's' via if-let")
}

// Heap vector inner: `int[]?` borrowed param.
func TestT0589_IfLetBorrowedVectorOptionalRejected(t *testing.T) {
	errs := ownerErrs(t, `
		take(int[]? v) {
			if x := v {
				_ = x;
			}
		}
		test() {}
	`)
	expectOwnerError(t, errs, "cannot consume borrowed parameter 'v' via if-let")
}

// Map inner: `map[string,int]?` borrowed param. Map has synth drop.
func TestT0589_IfLetBorrowedMapOptionalRejected(t *testing.T) {
	errs := ownerErrs(t, `
		take(map[string, int]? m) {
			if x := m {
				_ = x;
			}
		}
		test() {}
	`)
	expectOwnerError(t, errs, "cannot consume borrowed parameter 'm' via if-let")
}

// Channel inner: `Channel[int]?` borrowed param. Channel is droppable.
func TestT0589_IfLetBorrowedChannelOptionalRejected(t *testing.T) {
	errs := ownerErrs(t, `
		take(Channel[int]? c) {
			if x := c {
				_ = x;
			}
		}
		test() {}
	`)
	expectOwnerError(t, errs, "cannot consume borrowed parameter 'c' via if-let")
}

// Arc inner: `Ref[int]?` borrowed param. Arc is droppable (refcount dec).
func TestT0589_IfLetBorrowedArcOptionalRejected(t *testing.T) {
	errs := ownerErrs(t, `
		take(Ref[int]? a) {
			if x := a {
				_ = x;
			}
		}
		test() {}
	`)
	expectOwnerError(t, errs, "cannot consume borrowed parameter 'a' via if-let")
}

// Nested Optional inner: `_Box??` borrowed param. Inner `_Box?` is droppable
// (Optional recurses).
func TestT0589_IfLetBorrowedNestedOptionalRejected(t *testing.T) {
	errs := ownerErrs(t, `
		type _Box { int n; }
		take(_Box?? a) {
			if x := a {
				_ = x;
			}
		}
		test() {}
	`)
	expectOwnerError(t, errs, "cannot consume borrowed parameter 'a' via if-let")
}

// Tuple inner with droppable element: `(_Box, int)?` borrowed param.
func TestT0589_IfLetBorrowedTupleOptionalRejected(t *testing.T) {
	errs := ownerErrs(t, `
		type _Box { int n; }
		take((_Box, int)? p) {
			if x := p {
				_ = x;
			}
		}
		test() {}
	`)
	expectOwnerError(t, errs, "cannot consume borrowed parameter 'p' via if-let")
}

// Droppable enum inner: enum with droppable variant payload (string field).
// Exercises the `*types.Enum` branch of `isDroppableType`.
func TestT0589_IfLetBorrowedEnumOptionalRejected(t *testing.T) {
	errs := ownerErrs(t, `
		enum Msg { Text(string s); Ping; }
		take(Msg? m) {
			if x := m {
				_ = x;
			}
		}
		test() {}
	`)
	expectOwnerError(t, errs, "cannot consume borrowed parameter 'm' via if-let")
}

// While-let parallel of the if-let reject. Same predicate, different statement
// kind (checkWhileUnwrapStmt).
func TestT0589_WhileLetBorrowedHeapUserOptionalRejected(t *testing.T) {
	errs := ownerErrs(t, `
		type _Box { int n; }
		take(_Box? a) {
			while x := a {
				_ = x;
				break;
			}
		}
		test() {}
	`)
	expectOwnerError(t, errs, "cannot consume borrowed parameter 'a' via while-let")
}

// Paren-wrapped source: `if x := (a) { … }`. The walk peels ParenExpr just
// like T0586's helper.
func TestT0589_IfLetBorrowedThroughParenRejected(t *testing.T) {
	errs := ownerErrs(t, `
		type _Box { int n; }
		take(_Box? a) {
			if x := (a) {
				_ = x;
			}
		}
		test() {}
	`)
	expectOwnerError(t, errs, "cannot consume borrowed parameter 'a' via if-let")
}

// If-expr-wrapped source: `if x := (if flag { a } else { a }) { … }`. Codegen
// forwards the IfExpr's PHI value directly to the unwrap site, so a Borrowed
// param surfaced in either branch reaches the if-let as the same alias the
// caller still owns. Confirmed double-free on master without IfExpr peeling.
func TestT0589_IfLetBorrowedThroughIfExprRejected(t *testing.T) {
	errs := ownerErrs(t, `
		type _Box { int n; }
		take(_Box? a, bool flag) {
			if x := (if flag { a } else { a }) {
				_ = x;
			}
		}
		test() {}
	`)
	expectOwnerError(t, errs, "cannot consume borrowed parameter 'a' via if-let")
}

// Match-arm Body source: `if x := (match k { 1 => a, _ => a }) { … }`. The
// walk recurses into arm.Body via findBorrowedDroppableOptionalIfletSource.
func TestT0589_IfLetBorrowedThroughMatchBodyRejected(t *testing.T) {
	errs := ownerErrs(t, `
		type _Box { int n; }
		take(_Box? a, int k) {
			if x := (match k { 1 => a, _ => a }) {
				_ = x;
			}
		}
		test() {}
	`)
	expectOwnerError(t, errs, "cannot consume borrowed parameter 'a' via if-let")
}

// Match-arm Block source: `if x := (match k { 1 => { a }, _ => { a } }) { … }`.
// The walk recurses into arm.Block via findBorrowedDroppableOptionalIfletInBlock.
func TestT0589_IfLetBorrowedThroughMatchBlockRejected(t *testing.T) {
	errs := ownerErrs(t, `
		type _Box { int n; }
		take(_Box? a, int k) {
			if x := (match k { 1 => { a }, _ => { a } }) {
				_ = x;
			}
		}
		test() {}
	`)
	expectOwnerError(t, errs, "cannot consume borrowed parameter 'a' via if-let")
}

// Carve-out: `_Box? local = …; if x := local { … }`. The local's state is
// Owned, not Borrowed — the predicate doesn't fire and codegen's existing
// drop-flag propagation (T0585) handles this correctly.
func TestT0589_IfLetOwnedLocalAllowed(t *testing.T) {
	ownerOK(t, `
		type _Box { int n; }
		take() {
			_Box? a = _Box(n: 1);
			if x := a {
				_ = x;
			}
		}
		test() {}
	`)
}

// Carve-out: `~_Box? a` consume param. paramInitialState returns Owned for
// `~T?` params, so the predicate doesn't fire — codegen flows ownership
// through to the if-let binding via T0585's propagation.
func TestT0589_IfLetConsumeParamAllowed(t *testing.T) {
	ownerOK(t, `
		type _Box { int n; }
		take(_Box? move a) {
			if x := a {
				_ = x;
			}
		}
		test() {}
	`)
}

// Carve-out: `int?` borrowed param. int isn't droppable — the predicate
// short-circuits on `!isDroppableType(elem)`. Regression guard that pure
// primitive Optionals continue to flow through.
func TestT0589_IfLetBorrowedIntOptionalAllowed(t *testing.T) {
	ownerOK(t, `
		take(int? n) {
			if x := n {
				_ = x;
			}
		}
		test() {}
	`)
}

// Carve-out: pure value-type Optional. Value types have no drop (data
// embedded in the value struct), so `isDroppableType(P) == false` and the
// predicate skips them.
func TestT0589_IfLetBorrowedValueTypeOptionalAllowed(t *testing.T) {
	ownerOK(t, `
		type P {
			int x `+"`value"+`;
			int y `+"`value"+`;
		}
		take(P? p) {
			if x := p {
				_ = x;
			}
		}
		test() {}
	`)
}

// Carve-out: T0585's wrap-then-iflet path. `_Box? a; _Box?? b = a; if x := b`
// is the documented escape hatch for borrowed sources — codegen's T0585
// propagation clears b.dropflag at the wrap site. Regression guard that
// T0589's reject doesn't bleed into this path: the if-let source is the
// local `b` (Owned), not the param `a`.
func TestT0589_IfLetWrappedBorrowedOptionalAllowed(t *testing.T) {
	ownerOK(t, `
		type _Box { int n; }
		take(_Box? a) {
			_Box?? b = a;
			if x := b {
				_ = x;
			}
		}
		test() {}
	`)
}

// Coverage: a Borrowed local (e.g., a destructured borrow) is NOT a param,
// so the predicate's `c.params[name]` gate skips it. The shape is unsafe in
// the same way, but other sema rules (T0568, T0571) reject upstream — this
// regression guard just confirms T0589 itself doesn't generate a false
// positive on destructure-borrows.
func TestT0589_IfLetBorrowedDestructureLocalNotRejected(t *testing.T) {
	// T0571 rejects destructure from a temporary, so we use a tuple field
	// pattern that does compile. The destructured local is Borrowed.
	// Because it's not a param, T0589 doesn't fire (other sema rules do).
	// We assert here that the error, if any, is NOT T0589's diagnostic.
	errs := ownerErrs(t, `
		type _Box { int n; }
		type Holder { (_Box?, int) pair; }
		take() {
			h := Holder(pair: (_Box(n: 1), 2));
			(a, _) := h.pair;
			if x := a {
				_ = x;
			}
		}
		test() {}
	`)
	for _, e := range errs {
		if strings.Contains(e.Error(), "cannot consume borrowed parameter") {
			t.Errorf("T0589 diagnostic incorrectly fired on destructure-borrow local: %v", e)
		}
	}
}

// If-expr with the borrowed param surfaced ONLY in the Else branch (Then
// returns a non-borrowed Optional). Exercises the `e.Else` recursive call in
// findBorrowedDroppableOptionalIfletSource — the Then branch's
// findBorrowedDroppableOptionalIfletInBlock returns nil (the call expression
// doesn't surface a borrowed param), so the predicate falls through to the
// Else branch and detects `a` there. Without this case, the existing
// IfExpr/MatchExpr tests have the borrowed param in BOTH branches, so the
// Then path always returns first and the Else recursion is never exercised.
func TestT0589_IfLetBorrowedThroughIfExprElseOnlyRejected(t *testing.T) {
	errs := ownerErrs(t, `
		type _Box { int n; }
		gen() _Box? { return _Box(n: 1); }
		take(_Box? a, bool flag) {
			if x := (if flag { gen() } else { a }) {
				_ = x;
			}
		}
		test() {}
	`)
	expectOwnerError(t, errs, "cannot consume borrowed parameter 'a' via if-let")
}

// Method body context (instead of free function). The predicate must fire
// for if-let on a borrowed Optional method parameter as well — methods
// register their params via the same c.params/c.state machinery in
// checkMethodDecl that free functions use in checkFuncDecl. Regression guard
// against the predicate accidentally being function-scope-only.
func TestT0589_IfLetBorrowedMethodParamRejected(t *testing.T) {
	errs := ownerErrs(t, `
		type _Box { int n; }
		type Holder {
			int x;
			take(_Box? a) {
				if y := a {
					_ = y;
				}
			}
		}
		test() {}
	`)
	expectOwnerError(t, errs, "cannot consume borrowed parameter 'a' via if-let")
}

// --- T0811 ---
// T0811: binding the force-unwrapped (`o!`) or optional-cast (`o as! T` /
// `o as T`) inner of a borrowed droppable Optional *parameter* double-frees
// with the caller (callee binding-drop + caller drop). The bare/if-let/call-arg
// consume forms are already rejected (T0568/T0589/T0586); this closes the
// wrapper-consume gap. Carve-out matches T0568: scalar / string / vector inners
// stay allowed; `~T?` consume params stay allowed.

// Inferred var-decl: `p := o!` on a heap-user Optional param.
func TestT0811_InferredForceUnwrapBorrowedParamRejected(t *testing.T) {
	errs := ownerErrs(t, `
		type _Box { string name; drop(~this){} }
		take(_Box? o) {
			p := o!;
			_ = p;
		}
		test() {}
	`)
	expectOwnerError(t, errs, "cannot consume borrowed parameter 'o' via force-unwrap/cast")
}

// Typed var-decl: `_Box p = o!`.
func TestT0811_TypedForceUnwrapBorrowedParamRejected(t *testing.T) {
	errs := ownerErrs(t, `
		type _Box { string name; drop(~this){} }
		take(_Box? o) {
			_Box p = o!;
			_ = p;
		}
		test() {}
	`)
	expectOwnerError(t, errs, "cannot consume borrowed parameter 'o' via force-unwrap/cast")
}

// Assignment: `p = o!` into a local slot.
func TestT0811_AssignForceUnwrapBorrowedParamRejected(t *testing.T) {
	errs := ownerErrs(t, `
		type _Box { string name; drop(~this){} }
		take(_Box? o, _Box p) {
			p = o!;
			_ = p;
		}
		test() {}
	`)
	expectOwnerError(t, errs, "cannot consume borrowed parameter 'o' via force-unwrap/cast")
}

// Inferred var-decl via force cast: `d := o as! Der`.
func TestT0811_InferredForceCastBorrowedParamRejected(t *testing.T) {
	errs := ownerErrs(t, `
		type Base { string name; tag(this) string `+"`"+`abstract; drop(~this){} }
		type Der is Base { tag(this) string { return "d"; } }
		take(Base? o) {
			d := o as! Der;
			_ = d;
		}
		test() {}
	`)
	expectOwnerError(t, errs, "cannot consume borrowed parameter 'o' via force-unwrap/cast")
}

// Typed var-decl via force cast: `Der d = o as! Der`.
func TestT0811_TypedForceCastBorrowedParamRejected(t *testing.T) {
	errs := ownerErrs(t, `
		type Base { string name; tag(this) string `+"`"+`abstract; drop(~this){} }
		type Der is Base { tag(this) string { return "d"; } }
		take(Base? o) {
			Der d = o as! Der;
			_ = d;
		}
		test() {}
	`)
	expectOwnerError(t, errs, "cannot consume borrowed parameter 'o' via force-unwrap/cast")
}

// Typed-optional var-decl via non-force cast: `Der? d = o as Der`.
func TestT0811_OptionalCastBorrowedParamRejected(t *testing.T) {
	errs := ownerErrs(t, `
		type Base { string name; tag(this) string `+"`"+`abstract; drop(~this){} }
		type Der is Base { tag(this) string { return "d"; } }
		take(Base? o) {
			Der? d = o as Der;
			_ = d;
		}
		test() {}
	`)
	expectOwnerError(t, errs, "cannot consume borrowed parameter 'o' via force-unwrap/cast")
}

// Call-arg into `~` param: `g(o!)`.
func TestT0811_ForceUnwrapCallArgConsumeRejected(t *testing.T) {
	errs := ownerErrs(t, `
		type _Box { string name; drop(~this){} }
		g(_Box move p) { _ = p; }
		take(_Box? o) {
			g(o!);
		}
		test() {}
	`)
	expectOwnerError(t, errs, "cannot consume borrowed parameter 'o' via force-unwrap/cast")
}

// Call-arg with a paren-wrapped force-unwrap: `g((o!))` — parens must not
// defeat the reject. Exercises isForceUnwrapForm's ParenExpr peel.
func TestT0811_ParenWrappedForceUnwrapCallArgRejected(t *testing.T) {
	errs := ownerErrs(t, `
		type _Box { string name; drop(~this){} }
		g(_Box move p) { _ = p; }
		take(_Box? o) {
			g((o!));
		}
		test() {}
	`)
	expectOwnerError(t, errs, "cannot consume borrowed parameter 'o' via force-unwrap/cast")
}

// Constructor field-init: `Wrap(p: o!)`.
func TestT0811_ForceUnwrapConstructorArgRejected(t *testing.T) {
	errs := ownerErrs(t, `
		type _Box { string name; drop(~this){} }
		type Wrap { _Box p; drop(~this){} }
		take(_Box? o) Wrap {
			return Wrap(p: o!);
		}
		test() {}
	`)
	expectOwnerError(t, errs, "cannot consume borrowed parameter 'o' via force-unwrap/cast")
}

// Carve-out: inline use `o!.name` is not a binding — stays allowed.
func TestT0811_InlineForceUnwrapAllowed(t *testing.T) {
	ownerOK(t, `
		type _Box { string name; drop(~this){} }
		take(_Box? o) string {
			return o!.name;
		}
		test() {}
	`)
}

// Carve-out: `return o!` moves the inner out of the function — allowed.
func TestT0811_ReturnForceUnwrapAllowed(t *testing.T) {
	ownerOK(t, `
		type _Box { string name; drop(~this){} }
		take(_Box? o) _Box {
			return o!;
		}
		test() {}
	`)
}

// Carve-out: scalar inner `int?` — int isn't droppable, predicate skips it.
func TestT0811_ScalarForceUnwrapAllowed(t *testing.T) {
	ownerOK(t, `
		take(int? o) int {
			v := o!;
			return v;
		}
		test() {}
	`)
}

// Carve-out: string inner — auto-dup-safe at the binding site.
func TestT0811_StringForceUnwrapAllowed(t *testing.T) {
	ownerOK(t, `
		take(string? o) string {
			s := o!;
			return s;
		}
		test() {}
	`)
}

// Carve-out: vector inner — auto-dup-safe.
func TestT0811_VectorForceUnwrapAllowed(t *testing.T) {
	ownerOK(t, `
		take(int[]? o) int {
			v := o!;
			return v.len;
		}
		test() {}
	`)
}

// Carve-out: string inner into a `~string` call-arg — auto-dup-safe.
func TestT0811_StringForceUnwrapCallArgAllowed(t *testing.T) {
	ownerOK(t, `
		g(string move s) { _ = s; }
		take(string? o) {
			g(o!);
		}
		test() {}
	`)
}

// Carve-out: `~_Box?` consume param + `p := o!` — owner is the callee, allowed.
func TestT0811_ConsumeParamForceUnwrapAllowed(t *testing.T) {
	ownerOK(t, `
		type _Box { string name; drop(~this){} }
		take(_Box? move o) string {
			p := o!;
			return p.name;
		}
		test() {}
	`)
}

// Carve-out: `~Base?` consume param + force cast — allowed.
func TestT0811_ConsumeParamForceCastAllowed(t *testing.T) {
	ownerOK(t, `
		type Base { string name; tag(this) string `+"`"+`abstract; drop(~this){} }
		type Der is Base { tag(this) string { return "d"; } }
		take(Base? move o) string {
			d := o as! Der;
			return d.name;
		}
		test() {}
	`)
}

// Carve-out: non-optional-subject downcast keeps T0747 view semantics — must
// NOT be rejected. `b` is a borrowed Base param (non-optional), `b as! Der`.
func TestT0811_NonOptionalCastViewAllowed(t *testing.T) {
	ownerOK(t, `
		type Base { string name; tag(this) string `+"`"+`abstract; drop(~this){} }
		type Der is Base { tag(this) string { return "d"; } }
		take(Base b) string {
			d := b as! Der;
			return d.name;
		}
		test() {}
	`)
}

// Method-param form: the reject also fires inside a method body.
func TestT0811_ForceUnwrapMethodParamRejected(t *testing.T) {
	errs := ownerErrs(t, `
		type _Box { string name; drop(~this){} }
		type Holder {
			int x;
			take(_Box? o) {
				p := o!;
				_ = p;
			}
		}
		test() {}
	`)
	expectOwnerError(t, errs, "cannot consume borrowed parameter 'o' via force-unwrap/cast")
}

// Carve-out: the source Optional is a LOCAL, not a parameter — the binding
// owns it outright, so consuming its inner is safe (the documented "what
// works" contrast from the bug). Exercises the `!c.params` guard.
func TestT0811_LocalForceUnwrapAllowed(t *testing.T) {
	ownerOK(t, `
		type _Box { string name; drop(~this){} }
		take() string {
			_Box x = _Box(name: "a");
			_Box? oo = x;
			p := oo!;
			return p.name;
		}
		test() {}
	`)
}

// Carve-out: subject is not a bare ident (a member-expr Optional field). The
// surfaced subject isn't a tracked parameter ident, so the reject must not
// fire. Exercises the "subject not IdentExpr" / member-source path.
func TestT0811_MemberSourceForceUnwrapAllowed(t *testing.T) {
	ownerOK(t, `
		type _Box { string name; drop(~this){} }
		type Holder { _Box? slot; drop(~this){} }
		take(Holder move h) string {
			p := h.slot!;
			return p.name;
		}
		test() {}
	`)
}

// Paren-wrapped force-unwrap `p := (o!)` on a borrowed param is still rejected
// — parens must not defeat the consume check. Exercises the wrapper ParenExpr
// peel.
func TestT0811_ParenWrappedForceUnwrapRejected(t *testing.T) {
	errs := ownerErrs(t, `
		type _Box { string name; drop(~this){} }
		take(_Box? o) {
			p := (o!);
			_ = p;
		}
		test() {}
	`)
	expectOwnerError(t, errs, "cannot consume borrowed parameter 'o' via force-unwrap/cast")
}

// === T1073: force-unwrap of borrowed Optional param at remaining consume sites ===
// T0811 wired the var-decl/assign/call-arg/constructor/enum-variant sites; these
// cover the remaining tryMoveConsume sites: raise, yield, yield-delegate,
// select-case send, and the collection-literal element/entry loops.

// Array literal: `return [o!]` — the trivially-reachable repro from T1073.
func TestT1073_ArrayLitForceUnwrapBorrowedParamRejected(t *testing.T) {
	errs := ownerErrs(t, `
		type _Box { string name; drop(~this){} }
		g(_Box? o) _Box[] { return [o!]; }
		test() {}
	`)
	expectOwnerError(t, errs, "cannot consume borrowed parameter 'o' via force-unwrap/cast")
}

// Tuple literal element.
func TestT1073_TupleLitForceUnwrapBorrowedParamRejected(t *testing.T) {
	errs := ownerErrs(t, `
		type _Box { string name; drop(~this){} }
		g(_Box? o) (_Box, int) { return (o!, 1); }
		test() {}
	`)
	expectOwnerError(t, errs, "cannot consume borrowed parameter 'o' via force-unwrap/cast")
}

// Map literal value.
func TestT1073_MapLitValueForceUnwrapBorrowedParamRejected(t *testing.T) {
	errs := ownerErrs(t, `
		type _Box { string name; drop(~this){} }
		g(_Box? o) map[int, _Box] { return {1: o!}; }
		test() {}
	`)
	expectOwnerError(t, errs, "cannot consume borrowed parameter 'o' via force-unwrap/cast")
}

// Map literal key — a droppable heap user type used as a map key (Hashable +
// `==`) consumed via force-unwrap. Covers the entry.Key reject branch, distinct
// from the entry.Value branch above.
func TestT1073_MapLitKeyForceUnwrapBorrowedParamRejected(t *testing.T) {
	errs := ownerErrs(t, `
		type _Box {
			string name;
			drop(~this){}
			get hash int { return 7; }
			== (_Box other) bool { return this.name == other.name; }
		}
		g(_Box? o) map[_Box, int] { return {o!: 1}; }
		test() {}
	`)
	expectOwnerError(t, errs, "cannot consume borrowed parameter 'o' via force-unwrap/cast")
}

// Paren-wrapped force-unwrap at a collection-literal element `[(o!)]` — the
// reject must see through ParenExpr (isForceUnwrapForm peels it).
func TestT1073_ArrayLitParenWrappedForceUnwrapBorrowedParamRejected(t *testing.T) {
	errs := ownerErrs(t, `
		type _Box { string name; drop(~this){} }
		g(_Box? o) _Box[] { return [(o!)]; }
		test() {}
	`)
	expectOwnerError(t, errs, "cannot consume borrowed parameter 'o' via force-unwrap/cast")
}

// Raise statement: `raise o!`.
func TestT1073_RaiseForceUnwrapBorrowedParamRejected(t *testing.T) {
	errs := ownerErrs(t, `
		type _Err is error { string msg; }
		g!(_Err? o) int { raise o!; }
		test() {}
	`)
	expectOwnerError(t, errs, "cannot consume borrowed parameter 'o' via force-unwrap/cast")
}

// Carve-out: `yield o!` from a generator is ALLOWED for a borrowed param —
// a generator yields a *borrow* of the unwrapped inner (the for-in loop var does
// not own/drop it and the source optional stays usable after the loop), so there
// is no double-free. Unlike the collection-literal/raise/select-send sites, this
// must NOT be rejected. (Confirmed at runtime: the source optional is still
// present after the loop; see tests/e2e/optional_param_consume_test.pr.)
func TestT1073_YieldForceUnwrapBorrowedParamAllowed(t *testing.T) {
	ownerOK(t, `
		type _Box { string name; drop(~this){} }
		g(_Box? o) stream[_Box] { yield o!; }
		test() {}
	`)
}

// Select-case send: `case ch.send(o!)`.
func TestT1073_SelectSendForceUnwrapBorrowedParamRejected(t *testing.T) {
	errs := ownerErrs(t, `
		type _Box { string name; drop(~this){} }
		g(_Box? o, channel[_Box] ch) {
			select {
				ch.send(o!):
				default:
			}
		}
		test() {}
	`)
	expectOwnerError(t, errs, "cannot consume borrowed parameter 'o' via force-unwrap/cast")
}

// Carve-out: `move` param into an array literal — consume is genuinely safe.
func TestT1073_ArrayLitMoveParamForceUnwrapAllowed(t *testing.T) {
	ownerOK(t, `
		type _Box { string name; drop(~this){} }
		g(_Box? move o) _Box[] { return [o!]; }
		test() {}
	`)
}

// Carve-out: string inner into an array literal — auto-dup-safe.
func TestT1073_ArrayLitStringForceUnwrapAllowed(t *testing.T) {
	ownerOK(t, `
		g(string? o) string[] { return [o!]; }
		test() {}
	`)
}

// Carve-out: scalar inner into a tuple literal — int isn't droppable.
func TestT1073_TupleLitScalarForceUnwrapAllowed(t *testing.T) {
	ownerOK(t, `
		g(int? o) (int, int) { return (o!, 1); }
		test() {}
	`)
}

// === T0591: Getter var-decl from droppable owner ===

func TestT0591_GetterVarDeclFromDroppableOwnerOK(t *testing.T) {
	// Getter calls return owned values — not a field move.
	ownerOK(t, `
		type Resource {
			int id;
			drop(~this) {}
		}
		type Factory {
			drop(~this) {}
			get fresh Resource => Resource(id: 42);
		}
		test() {
			Factory f = Factory();
			Resource r = f.fresh;
		}
	`)
}

func TestT0591_GetterVarDeclInferredFromDroppableOwnerOK(t *testing.T) {
	// Inferred var-decl from getter on droppable owner.
	ownerOK(t, `
		type Resource {
			int id;
			drop(~this) {}
		}
		type Factory {
			drop(~this) {}
			get fresh Resource => Resource(id: 42);
		}
		test() {
			Factory f = Factory();
			r := f.fresh;
		}
	`)
}

func TestT0591_GetterReturningDroppableFromDroppableOwnerOK(t *testing.T) {
	// Getter returning droppable type from droppable owner.
	ownerOK(t, `
		type Inner {
			int v;
			drop(~this) {}
		}
		type Outer {
			int x;
			drop(~this) {}
			get make_inner Inner => Inner(v: this.x);
		}
		test() {
			Outer o = Outer(x: 5);
			Inner i = o.make_inner;
		}
	`)
}

func TestT0591_EnumGetterFromDroppableEnumOwnerOK(t *testing.T) {
	// Non-generic enum with drop + getter — exercises *types.Enum path.
	ownerOK(t, `
		type Resource {
			int id;
			drop(~this) {}
		}
		enum Source {
			A;
			B;
			drop(~this) {}
			get fresh Resource => Resource(id: 1);
		}
		test() {
			Source s = Source.A;
			Resource r = s.fresh;
		}
	`)
}

func TestT0591_GenericEnumGetterFromDroppableEnumOwnerOK(t *testing.T) {
	// Generic enum with drop + getter — exercises *types.Instance/*types.Enum path.
	ownerOK(t, `
		type Resource {
			int id;
			drop(~this) {}
		}
		enum Container[T] {
			Some(T value);
			None;
			drop(~this) {}
			get fresh Resource => Resource(id: 1);
		}
		test() {
			Container[int] c = Container[int].Some(value: 42);
			Resource r = c.fresh;
		}
	`)
}

func TestT0591_FieldMoveFromDroppableOwnerStillRejected(t *testing.T) {
	// Actual field reads (not getters) must still be rejected.
	errs := ownerErrs(t, `
		type Resource {
			int id;
			drop(~this) {}
		}
		type Owner {
			Resource r;
		}
		test() {
			Owner o = Owner(r: Resource(id: 1));
			Resource r2 = o.r;
		}
	`)
	expectOwnerError(t, errs, "cannot move field 'r'")
}

// === T0596: indexed-slot move of single-owner native handles rejected ===
//
// Mutex[T] / MutexGuard[T] / Task[T] have no dup-on-read path (T0508). Moving
// them out of an array or vector slot aliases the slot's owned pointer; the
// container's drop walks both copies → double-free / SEGV. Reject at the
// ownership pass with a type-driven check on IndexExpr result type.

// Slot-to-slot copy on a fixed-size Mutex array is the original repro (SEGV
// without the fix). The RHS `arr[0]` is the rejected expression.
func TestT0596_AssignSlotToSlotMutexRejected(t *testing.T) {
	errs := ownerErrs(t, `
		test() {
			Mutex[int] m0 = Mutex[int](1);
			Mutex[int] m1 = Mutex[int](2);
			Mutex[int][2] arr = [m0, m1];
			arr[1] = arr[0];
		}
	`)
	expectOwnerError(t, errs, "cannot move Mutex[int] out of indexed slot")
}

// Task[T] has the same single-owner semantics (T0508). Slot-to-slot on a
// fixed-size Task array must be rejected.
func TestT0596_AssignSlotToSlotTaskRejected(t *testing.T) {
	errs := ownerErrs(t, `
		_make_task() Task[int] { return go { 5 }; }
		test() {
			Task[int] t0 = _make_task();
			Task[int] t1 = _make_task();
			Task[int][2] arr = [t0, t1];
			arr[1] = arr[0];
		}
	`)
	expectOwnerError(t, errs, "cannot move Task[int] out of indexed slot")
}

// Vector slot-to-slot: same shape via a heap-backed container. Vector has its
// own dup-on-read path in codegen for Vector/Channel/Arc/Weak but skips
// Mutex/MutexGuard/Task — confirmed gap.
func TestT0596_VectorSlotToSlotMutexRejected(t *testing.T) {
	errs := ownerErrs(t, `
		test() {
			Mutex[int] m0 = Mutex[int](1);
			Mutex[int] m1 = Mutex[int](2);
			Vector[Mutex[int]] v = [m0, m1];
			v[1] = v[0];
		}
	`)
	expectOwnerError(t, errs, "cannot move Mutex[int] out of indexed slot")
}

// Vector[Task[T]] sibling — Task handles share the gap.
func TestT0596_VectorSlotToSlotTaskRejected(t *testing.T) {
	errs := ownerErrs(t, `
		_make_task() Task[int] { return go { 5 }; }
		test() {
			Task[int] t0 = _make_task();
			Task[int] t1 = _make_task();
			Vector[Task[int]] v = [t0, t1];
			v[1] = v[0];
		}
	`)
	expectOwnerError(t, errs, "cannot move Task[int] out of indexed slot")
}

// Var-decl from a Mutex slot (`Mutex[int] x = arr[0];`) aliases the slot's
// owned pointer just as much as slot-to-slot assignment does — the new local
// and the array slot both drop the same allocation at scope exit. Catch via
// tryMove's IndexExpr path.
func TestT0596_VarDeclFromMutexSlotRejected(t *testing.T) {
	errs := ownerErrs(t, `
		test() {
			Mutex[int] m0 = Mutex[int](1);
			Mutex[int] m1 = Mutex[int](2);
			Mutex[int][2] arr = [m0, m1];
			Mutex[int] x = arr[0];
		}
	`)
	expectOwnerError(t, errs, "cannot move Mutex[int] out of indexed slot")
}

// Inferred var-decl (`x := arr[0];`) walks the same tryMove path. Regression
// guard that the check fires uniformly across both var-decl forms.
func TestT0596_InferredVarDeclFromMutexSlotRejected(t *testing.T) {
	errs := ownerErrs(t, `
		test() {
			Mutex[int] m0 = Mutex[int](1);
			Mutex[int] m1 = Mutex[int](2);
			Mutex[int][2] arr = [m0, m1];
			x := arr[0];
		}
	`)
	expectOwnerError(t, errs, "cannot move Mutex[int] out of indexed slot")
}

// Optional wrapping coverage: `Mutex[int]?` slot read still resolves to a
// single-owner native handle (isSingleOwnerNativeType recurses through
// Optional). The slot-to-slot UAF shape is identical.
func TestT0596_VarDeclFromOptionalMutexSlotRejected(t *testing.T) {
	errs := ownerErrs(t, `
		test() {
			Mutex[int]? m0 = Mutex[int](1);
			Mutex[int]? m1 = Mutex[int](2);
			Mutex[int]?[2] arr = [m0, m1];
			Mutex[int]? x = arr[0];
		}
	`)
	expectOwnerError(t, errs, "cannot move Mutex[int]? out of indexed slot")
}

// Return statement consume path — `return arr[0];` flows through
// tryMoveConsume, so the check must fire there too (not just at var-decls).
func TestT0596_ReturnMutexSlotRejected(t *testing.T) {
	errs := ownerErrs(t, `
		take() Mutex[int] {
			Mutex[int] m0 = Mutex[int](1);
			Mutex[int] m1 = Mutex[int](2);
			Mutex[int][2] arr = [m0, m1];
			return arr[0];
		}
		test() {}
	`)
	expectOwnerError(t, errs, "cannot move Mutex[int] out of indexed slot")
}

// Positive: assigning a fresh-from-constructor Mutex to a slot is the
// intended overwrite path. The RHS is a CallExpr, not an IndexExpr, so the
// new check doesn't fire.
func TestT0596_FreshMutexAssignAllowed(t *testing.T) {
	ownerOK(t, `
		test() {
			Mutex[int] m0 = Mutex[int](1);
			Mutex[int] m1 = Mutex[int](2);
			Mutex[int][2] arr = [m0, m1];
			arr[1] = Mutex[int](3);
		}
	`)
}

// Positive: calling a method on a slot (`arr[0].lock()`) is a borrow, not a
// move. The IndexExpr is the receiver of a method call, never the consumed
// value, so tryMove/tryMoveConsume are not called on it.
func TestT0596_MutexSlotMethodCallAllowed(t *testing.T) {
	ownerOK(t, `
		test() {
			Mutex[int] m0 = Mutex[int](1);
			Mutex[int] m1 = Mutex[int](2);
			Mutex[int][2] arr = [m0, m1];
			use g := arr[0].lock();
		}
	`)
}

// Positive: parenthesised RHS (`(arr[0])`) — ParenExpr peeling makes the
// check fire uniformly regardless of surface syntax.
func TestT0596_ParenthesisedSlotMoveRejected(t *testing.T) {
	errs := ownerErrs(t, `
		test() {
			Mutex[int] m0 = Mutex[int](1);
			Mutex[int] m1 = Mutex[int](2);
			Mutex[int][2] arr = [m0, m1];
			Mutex[int] x = (arr[0]);
		}
	`)
	expectOwnerError(t, errs, "cannot move Mutex[int] out of indexed slot")
}

// Positive: Ref[T] / Vector[T] / Channel[T] / string slots are still
// duppable (codegen has the matching helpers), so they must not be
// over-rejected. Regression guard for the type filter.
func TestT0596_NonSingleOwnerSlotsAllowed(t *testing.T) {
	ownerOK(t, `
		test() {
			Ref[int] a0 = Ref[int](1);
			Ref[int] a1 = Ref[int](2);
			Ref[int][2] arr_a = [a0, a1];
			arr_a[1] = arr_a[0];

			Vector[int] v0 = Vector[int]();
			Vector[int] v1 = Vector[int]();
			Vector[int][2] arr_v = [v0, v1];
			arr_v[1] = arr_v[0];
		}
	`)
}

// MutexGuard[T] is the third single-owner native handle (T0508). It is
// produced only by `.lock()` and is use-bound, but an indexed slot read
// whose result type is MutexGuard must still be rejected — exercises the
// IsMutexGuard arm of isSingleOwnerNativeType end-to-end. (The `[g0]`
// literal also emits a use-bound-move error; expectOwnerError matches the
// T0596 message specifically.)
func TestT0596_VarDeclFromMutexGuardSlotRejected(t *testing.T) {
	errs := ownerErrs(t, `
		test() {
			Mutex[int] m0 = Mutex[int](1);
			use g0 := m0.lock();
			MutexGuard[int][1] arr = [g0];
			MutexGuard[int] x = arr[0];
		}
	`)
	expectOwnerError(t, errs, "cannot move MutexGuard[int] out of indexed slot")
}

// T0612 — Gap B: `arr[i]!` wraps the IndexExpr in an OptionalUnwrapExpr,
// so the cast in rejectIndexExprSingleOwnerMove's helper used to fail and
// the move slipped through silently → latent double-free. The peel loop
// now strips OptionalUnwrapExpr so the inner type (Optional[Mutex]) reaches
// the rejection. (T0612's gap A — nested-array row moves — is covered by
// T0545's sema-level rejection of any container that transitively contains
// a single-owner handle; those types can no longer be constructed.)
func TestT0612_OptionalUnwrapMutexArrayElementRejected(t *testing.T) {
	errs := ownerErrs(t, `
		test() {
			Mutex[int]? m0 = Mutex[int](1);
			Mutex[int]? m1 = Mutex[int](2);
			Mutex[int]?[2] arr = [m0, m1];
			Mutex[int] x = arr[0]!;
		}
	`)
	expectOwnerError(t, errs, "cannot move Mutex[int]? out of indexed slot")
}

// Task[T] parity for the OptionalUnwrap path.
func TestT0612_OptionalUnwrapTaskArrayElementRejected(t *testing.T) {
	errs := ownerErrs(t, `
		_make_task() Task[int] { return go { 5 }; }
		test() {
			Task[int]? t0 = _make_task();
			Task[int]? t1 = _make_task();
			Task[int]?[2] arr = [t0, t1];
			Task[int] x = arr[0]!;
		}
	`)
	expectOwnerError(t, errs, "cannot move Task[int]? out of indexed slot")
}

// Vector slot under OptionalUnwrap — same shape, heap-backed container.
func TestT0612_OptionalUnwrapVectorMutexElementRejected(t *testing.T) {
	errs := ownerErrs(t, `
		test() {
			Mutex[int]? m0 = Mutex[int](1);
			Mutex[int]? m1 = Mutex[int](2);
			Vector[Mutex[int]?] v = [m0, m1];
			Mutex[int] x = v[0]!;
		}
	`)
	expectOwnerError(t, errs, "cannot move Mutex[int]? out of indexed slot")
}

// ParenExpr + OptionalUnwrap composition — `(arr[0]!)` — the peel loop
// must strip both layers in any order.
func TestT0612_ParenthesisedOptionalUnwrapRejected(t *testing.T) {
	errs := ownerErrs(t, `
		test() {
			Mutex[int]? m0 = Mutex[int](1);
			Mutex[int]? m1 = Mutex[int](2);
			Mutex[int]?[2] arr = [m0, m1];
			Mutex[int] x = (arr[0]!);
		}
	`)
	expectOwnerError(t, errs, "cannot move Mutex[int]? out of indexed slot")
}

// Positive: unwrapping a non-handle Optional slot (e.g. `int?[2]`) must
// still be allowed — the OptionalUnwrap peel only escalates when the
// inner IndexExpr's element type is a single-owner native handle.
func TestT0612_OptionalUnwrapIntArrayElementAllowed(t *testing.T) {
	ownerOK(t, `
		test() {
			int? a = 1;
			int? b = 2;
			int?[2] arr = [a, b];
			int x = arr[0]!;
		}
	`)
}

// === T0623: match-destructure of single-owner-handle variant moves subject ===

// A destructure arm binding a single-owner-handle variant field consumes the
// subject — using the subject after the match is a use-after-move.
func TestT0623_MatchHandleDestructureMovesSubject(t *testing.T) {
	errs := ownerErrs(t, `
		worker() int { return 42; }
		enum E { Empty, Held(Task[int] t) }
		test() {
			e := E.Held(go worker());
			match e {
				E.Empty => assert(true, "empty"),
				E.Held(tk) => assert(true, "held"),
			}
			match e {
				E.Empty => assert(true, "again"),
				E.Held(_) => assert(true, "again held"),
			}
		}
	`)
	expectOwnerError(t, errs, "use of moved variable 'e'")
}

// Partial match — one arm destructures the handle, another doesn't. The
// arm-state merge propagates Moved through the non-moving arm too, so the
// subject is moved after the match overall.
func TestT0623_MatchPartialMovesSubject(t *testing.T) {
	errs := ownerErrs(t, `
		worker() int { return 42; }
		enum E { Stored(string s), Held(Task[int] t) }
		test() {
			e := E.Held(go worker());
			match e {
				E.Stored(s) => assert(s == "x", "stored"),
				E.Held(tk) => assert(true, "held"),
			}
			match e {
				E.Stored(s) => assert(true, "again stored"),
				E.Held(_) => assert(true, "again held"),
			}
		}
	`)
	expectOwnerError(t, errs, "use of moved variable 'e'")
}

// Wildcard `_` binding on the handle variant does NOT consume the subject —
// no move-out, no use-after-move on the second match.
func TestT0623_MatchHandleWildcardDoesNotMoveSubject(t *testing.T) {
	ownerOK(t, `
		worker() int { return 42; }
		enum E { Empty, Held(Task[int] t) }
		test() {
			e := E.Held(go worker());
			match e {
				E.Empty => assert(true, "empty"),
				E.Held(_) => assert(true, "held"),
			}
			match e {
				E.Empty => assert(true, "again"),
				E.Held(_) => assert(true, "again held"),
			}
		}
	`)
}

// A plain non-Copy `E e` parameter is Borrowed (the caller owns the enum, the
// callee only reads it). Sema's IdentExpr-with-non-ref-type rule accepts the
// destructure (the static type is `E`, not `E&`/`E~`), but moving out of a
// borrowed payload would alias the caller's variant data → double-free at the
// caller's synth enum drop. Ownership must reject this with a clear error.
func TestT0623_MatchBorrowedParamSubjectRejected(t *testing.T) {
	errs := ownerErrs(t, `
		worker() int { return 42; }
		enum E { Empty, Held(Task[int] t) }
		consume(E e) {
			match e {
				E.Empty => assert(true, "empty"),
				E.Held(t) => assert(true, "held"),
			}
		}
		test() {
			e := E.Held(go worker());
			consume(e);
		}
	`)
	expectOwnerError(t, errs, "owned local")
}

// Generic enum instantiated at a handle type (BoxG[Task[int]]) likewise moves
// the subject. Exercises the BuildSubstMap branch in armMovesSubject — the
// non-generic tests above use *types.Enum directly and never hit the *Instance
// + substitution path.
func TestT0623_MatchGenericEnumHandleMovesSubject(t *testing.T) {
	errs := ownerErrs(t, `
		worker() int { return 42; }
		enum BoxG[T] { Empty, Has(T v) }
		test() {
			b := BoxG[Task[int]].Has(go worker());
			match b {
				BoxG.Empty => assert(true, "empty"),
				BoxG.Has(t) => assert(true, "has"),
			}
			match b {
				BoxG.Empty => assert(true, "again"),
				BoxG.Has(_) => assert(true, "again has"),
			}
		}
	`)
	expectOwnerError(t, errs, "use of moved variable 'b'")
}

// === T0650: user-defined non-native `[]` returning a single-owner native
// handle is NOT a slot-alias move ===
//
// rejectIndexExprSingleOwnerMove keyed solely on the IndexExpr result type,
// so a user-defined non-native `[]` returning a freshly-constructed /
// `.lock()`-derived owned Mutex/Task/MutexGuard was wrongly rejected — even
// though the IDENTICAL plain `at()` method compiles, runs, and is 0-leak
// (T0647-class asymmetry, ownership-pass analogue). The fix exempts user
// non-native `[]` via isUserIndexExpr (mirrors codegen/stmt.go); native
// container/array indexing still aliases the slot's owned pointer and stays
// rejected.

// Fresh-constructor Mutex via a user `[]`: temp arg + binding + `.lock()`.
// Mirrors the proven /tmp/t0647_mtx_parity.pr operator arm.
func TestT0650_UserIndexMutexReturnAllowed(t *testing.T) {
	ownerOK(t, `
		type MtxBox {
			int n;
			[](int i) Mutex[int] { return Mutex[int](this.n + i); }
		}
		take_mtx_i(Mutex[int] m) {}
		test() {
			mb := MtxBox(n: 100);
			take_mtx_i(mb[0]);
			m := mb[1];
			use g := m.lock();
		}
	`)
}

// Task handle returned from a user `[]` (go-spawned). Temp arg + binding +
// receive. Bodies from the proven /tmp/t0647_method_only.pr task case.
func TestT0650_UserIndexTaskReturnAllowed(t *testing.T) {
	ownerOK(t, `
		worker_t0650() int { return 42; }
		type TaskBox {
			[](int i) Task[int] { return go worker_t0650(); }
		}
		take_task_i(Task[int] t) {}
		test() {
			tb := TaskBox();
			take_task_i(tb[0]);
			t := tb[1];
			r := <-t;
		}
	`)
}

// MutexGuard return from a user `[]` (`this.m.lock()`). The guard is
// use-bound; the index read must not hit the single-owner rejection.
func TestT0650_UserIndexMutexGuardReturnAllowed(t *testing.T) {
	ownerOK(t, `
		type MgBox {
			Mutex[int] m;
			[](int i) MutexGuard[int] { return this.m.lock(); }
		}
		test() {
			mgb := MgBox(m: Mutex[int](42));
			use g := mgb[0];
		}
	`)
}

// Optional-Mutex return: isSingleOwnerNativeType recurses through Optional,
// so the Optional arm must also be exempted via the user `[]`.
func TestT0650_UserIndexOptionalMutexReturnAllowed(t *testing.T) {
	ownerOK(t, `
		type OptMtxBox {
			[](int i) Mutex[int]? { return Mutex[int](i); }
		}
		test() {
			b := OptMtxBox();
			m := b[0];
		}
	`)
}

// Generic owner Box[T] whose `[]` returns a fresh Mutex[int]. extractNamedType
// resolves the Instance origin to the Named Box, whose non-native `[]` exempts
// the read (T0647 generic parity).
func TestT0650_GenericUserIndexMutexReturnAllowed(t *testing.T) {
	ownerOK(t, `
		type Box[T] {
			T v;
			[](int i) Mutex[int] { return Mutex[int](i); }
		}
		test() {
			b := Box[int](v: 9);
			m := b[0];
		}
	`)
}

// Regression guard: native Vector[Mutex[int]] indexing resolves to Vector's
// *native* `[]` (IsNative → not exempt). Moving a Mutex out of the slot
// aliases the container's owned pointer → must stay rejected.
func TestT0650_NativeVectorMutexIndexStillRejected(t *testing.T) {
	errs := ownerErrs(t, `
		test() {
			Mutex[int] m0 = Mutex[int](1);
			Mutex[int] m1 = Mutex[int](2);
			Vector[Mutex[int]] v = [m0, m1];
			Mutex[int] x = v[0];
		}
	`)
	expectOwnerError(t, errs, "cannot move Mutex[int] out of indexed slot")
}

// Regression guard: fixed-size Mutex[int][2] indexing. extractNamedType(Array)
// is nil → not exempt → slot-alias move stays rejected.
func TestT0650_FixedArrayMutexIndexStillRejected(t *testing.T) {
	errs := ownerErrs(t, `
		test() {
			Mutex[int] m0 = Mutex[int](1);
			Mutex[int] m1 = Mutex[int](2);
			Mutex[int][2] arr = [m0, m1];
			Mutex[int] x = arr[0];
		}
	`)
	expectOwnerError(t, errs, "cannot move Mutex[int] out of indexed slot")
}

// --- T0650 × T0596/T0612 peel-loop composition ---
//
// rejectIndexExprSingleOwnerMove peels ParenExpr / OptionalUnwrapExpr (T0596 /
// T0612) BEFORE the IndexExpr cast, then applies the isUserIndexExpr exemption
// to the peeled IndexExpr. The original T0650 tests only exercise bare `mb[0]`
// / `m := mb[1]`; they never exercise the exemption *through* a peel. Those
// composed shapes are reachable (an AI agent writes `take((box[i]))` or, for a
// fallible accessor, `take(box[i]!)`) and were uncovered. The ownership pass
// correctly ALLOWS all of these (the value is a freshly-constructed owned
// handle, not a container-slot alias). NOTE: the consume/temp `!`-unwrap
// *runtime* path currently leaks at codegen — that is a pre-existing,
// T0650-independent codegen gap (the IDENTICAL plain method `at()!` leaks the
// same), filed as **T0654**; the binding `m := b[i]!` form is 0-leak. These
// ownership tests assert only the (correct) ownership-pass acceptance.

// ParenExpr peel + user-`[]` exemption, BOTH call sites: `take((mb[0]))`
// (tryMoveConsume) and `m := (mb[1])` (tryMove var-decl). Mirrors
// TestT0596_ParenthesisedSlotMoveRejected on the exemption side.
func TestT0650_UserIndexMutexThroughParenAllowed(t *testing.T) {
	ownerOK(t, `
		type MtxBox {
			int n;
			[](int i) Mutex[int] { return Mutex[int](this.n + i); }
		}
		take_mtx_i(Mutex[int] m) {}
		test() {
			mb := MtxBox(n: 100);
			take_mtx_i((mb[0]));
			m := (mb[1]);
			use g := m.lock();
		}
	`)
}

// OptionalUnwrapExpr peel + user-`[]` exemption, BOTH call sites: the `[]`
// returns Mutex[int]? and the result is `!`-unwrapped at a consume site
// (`take_mtx_i(b[0]!)`) and a var-decl site (`m := b[1]!`). Mirrors
// TestT0612_OptionalUnwrapMutexArrayElementRejected on the exemption side.
func TestT0650_UserIndexOptionalMutexUnwrapAllowed(t *testing.T) {
	ownerOK(t, `
		type OptMtxBox {
			int n;
			[](int i) Mutex[int]? { return Mutex[int](this.n + i); }
		}
		take_mtx_i(Mutex[int] m) {}
		test() {
			b := OptMtxBox(n: 200);
			take_mtx_i(b[0]!);
			m := b[1]!;
			use g := m.lock();
		}
	`)
}

// ParenExpr + OptionalUnwrapExpr composition over a user `[]` — `(b[0]!)` —
// the peel loop must strip both layers in any order and the exemption must
// still apply. Mirrors TestT0612_ParenthesisedOptionalUnwrapRejected.
func TestT0650_UserIndexOptionalMutexParenUnwrapAllowed(t *testing.T) {
	ownerOK(t, `
		type OptMtxBox {
			int n;
			[](int i) Mutex[int]? { return Mutex[int](this.n + i); }
		}
		take_mtx_i(Mutex[int] m) {}
		test() {
			b := OptMtxBox(n: 300);
			take_mtx_i((b[0]!));
		}
	`)
}

// Task parity for the OptionalUnwrap peel + user-`[]` exemption (both sites).
func TestT0650_UserIndexOptionalTaskUnwrapAllowed(t *testing.T) {
	ownerOK(t, `
		worker_t0650u() int { return 42; }
		type OptTaskBox {
			[](int i) Task[int]? { return go worker_t0650u(); }
		}
		take_task_i(Task[int] t) {}
		test() {
			tb := OptTaskBox();
			take_task_i(tb[0]!);
			t := tb[1]!;
			r := <-t;
		}
	`)
}

// === T0635: enum-variant constructor consume-checks its args even under
// generics ===
//
// An enum-variant constructor (`Box[T].Full`) is signature-typed by sema, so
// it would otherwise take the function-call path whose borrowed-param
// rejection is droppability-gated (`findBorrowedNonAliasSafeIdent` →
// `isDroppableType`) and thus blind to `*types.TypeParam` fields. A variant
// field owns its value, so its args are now routed through `tryMoveConsume`
// (identical to a true struct constructor), which rejects borrowed params
// unconditionally — generic-safe and symmetric with the non-generic case.

// Generic fn, explicit `T?` (borrowed) param moved into an owned variant
// field — previously slipped through (TypeParam-blind), now rejected.
func TestT0635_GenericFnOptBorrowedParamRejected(t *testing.T) {
	errs := ownerErrs(t, `
		enum Box[T] { Full(T? v), Vacant, }
		make_box_opt[T](T? x) Box[T] {
			return Box[T].Full(x);
		}
		test() {}
	`)
	expectOwnerError(t, errs, "cannot move borrowed parameter 'x'")
}

// Generic fn, bare `T` (borrowed) param implicitly widened into the `T?`
// variant field — same defect shape, same rejection.
func TestT0635_GenericFnBareBorrowedParamRejected(t *testing.T) {
	errs := ownerErrs(t, `
		enum Box[T] { Full(T? v), Vacant, }
		make_box[T](T x) Box[T] {
			return Box[T].Full(x);
		}
		test() {}
	`)
	expectOwnerError(t, errs, "cannot move borrowed parameter 'x'")
}

// Generic METHOD body (owner type generic): borrowed `T?` param moved into
// the owned variant field — the bug title covers fn AND method bodies; the
// fix is enclosing-context-agnostic, this pins the method case.
func TestT0635_GenericMethodOptBorrowedParamRejected(t *testing.T) {
	errs := ownerErrs(t, `
		enum Box[T] { Full(T? v), Vacant, }
		type Holder[T] {
			T seed;
			wrap(this, T? o) Box[T] {
				return Box[T].Full(o);
			}
		}
		test() {}
	`)
	expectOwnerError(t, errs, "cannot move borrowed parameter 'o'")
}

// Generic METHOD body with `~T?` (owned) — correct, no error.
func TestT0635_GenericMethodOwnedOptParamOK(t *testing.T) {
	ownerOK(t, `
		enum Box[T] { Full(T? v), Vacant, }
		type Holder[T] {
			T seed;
			wrap(this, T? move o) Box[T] {
				return Box[T].Full(move o);
			}
		}
		test() {}
	`)
}

// `~T?` (owned) — the param consumes, so moving it into the variant field is
// correct. Must NOT error (the idiomatic remediation the diagnostic asks for).
func TestT0635_GenericFnOwnedOptParamOK(t *testing.T) {
	ownerOK(t, `
		enum Box[T] { Full(T? v), Vacant, }
		make_box_opt[T](T? move x) Box[T] {
			return Box[T].Full(move x);
		}
		test() {}
	`)
}

// `~T` (owned) bare param widened into `T?` — also correct, no error.
func TestT0635_GenericFnOwnedBareParamOK(t *testing.T) {
	ownerOK(t, `
		enum Box[T] { Full(T? v), Vacant, }
		make_box[T](T move x) Box[T] {
			return Box[T].Full(move x);
		}
		test() {}
	`)
}

// Non-regression: an owned LOCAL (not a borrowed param) moved into a variant
// field is fine — tryMoveConsume only rejects the Borrowed state.
func TestT0635_OwnedLocalIntoVariantOK(t *testing.T) {
	ownerOK(t, `
		enum Box[T] { Full(T? v), Vacant, }
		make_from_local() Box[string] {
			string s = "hi".to_upper();
			return Box[string].Full(move s);
		}
		test() {}
	`)
}

// Non-regression: a no-arg variant (`Box[T].Vacant`) is not a consuming call
// and must not be misclassified — no spurious error.
func TestT0635_NoArgVariantOK(t *testing.T) {
	ownerOK(t, `
		enum Box[T] { Full(T? v), Vacant, }
		make_vacant[T]() Box[T] {
			return Box[T].Vacant;
		}
		test() {}
	`)
}

// Non-regression: an enum-VALUE method call (`e.peek(s)`) is not a variant
// constructor — `LookupVariant("peek")` is nil — so it stays on the normal
// function-call path. A borrowed arg into a borrow param of that method is
// still accepted (method-arg borrow semantics unchanged).
func TestT0635_EnumMethodCallNotMisclassified(t *testing.T) {
	ownerOK(t, `
		enum E {
			A, B,
			peek(this, string s) bool { return s.len > 0; }
		}
		check_it(E e, string s) bool {
			return e.peek(s);
		}
		test() {}
	`)
}

// Non-regression guard: an enum-value method that genuinely consumes
// (`~string`) must STILL reject a borrowed-param arg — the new variant-ctor
// routing must not make method calls lenient.
func TestT0635_EnumMethodConsumeBorrowedParamStillRejected(t *testing.T) {
	errs := ownerErrs(t, `
		enum E {
			A, B,
			into(this, string move s) bool { return s.len > 0; }
		}
		feed(E e, string s) bool {
			return e.into(s);
		}
		test() {}
	`)
	expectOwnerError(t, errs, "cannot move borrowed parameter 's'")
}

// Guard against accidental relaxation: the non-generic concrete case sema
// already rejected must keep erroring.
func TestT0635_NonGenericVariantBorrowedParamStillRejected(t *testing.T) {
	errs := ownerErrs(t, `
		type Pt { int x; int y; }
		enum MaybePt { Slot(Pt? p), None_, }
		mk_maybe(Pt? p) MaybePt {
			return MaybePt.Slot(p);
		}
		test() {}
	`)
	expectOwnerError(t, errs, "cannot move borrowed parameter 'p'")
}

// Loop-coverage regression: a MULTI-field variant constructor must
// consume-check EVERY argument, not just e.Args[0]. First arg is owned
// (`~T?`, consumes fine) so any non-looping implementation would stop there;
// the second arg is a borrowed `T?` param and MUST still be rejected. A
// refactor to `c.tryMoveConsume(e.Args[0].Value)` would pass every other
// T0635 test (all single-arg) yet silently reintroduce the exact double-free
// bug class for the non-first field — this test pins the loop.
func TestT0635_MultiArgVariantSecondBorrowedParamRejected(t *testing.T) {
	errs := ownerErrs(t, `
		enum Pair[T] { Both(T? a, T? b), Empty, }
		mk_pair[T](T? move a_owned, T? b_borrow) Pair[T] {
			return Pair[T].Both(a_owned, b_borrow);
		}
		test() {}
	`)
	expectOwnerError(t, errs, "cannot move borrowed parameter 'b_borrow'")
}

// Non-regression: a multi-field variant constructor with ALL owned (`~T?`)
// params is correct — the loop iterates over every arg without spuriously
// erroring (multi-iteration no-error path at the unit level).
func TestT0635_MultiArgVariantAllOwnedOK(t *testing.T) {
	ownerOK(t, `
		enum Pair[T] { Both(T? a, T? b), Empty, }
		mk_pair[T](T? move a_owned, T? move b_owned) Pair[T] {
			return Pair[T].Both(move a_owned, move b_owned);
		}
		test() {}
	`)
}

// `this` (borrowed receiver) moved into an owned variant field from a method
// body: the new guard routes the arg through tryMoveConsume, whose own `this`
// branch must still reject it. Confirms the variant-ctor reroute does NOT
// open a silent escape for `this` consumption — that would be the same
// caller-drop-flag-still-set double-free class as T0635 itself.
func TestT0635_ThisIntoVariantConstructorRejected(t *testing.T) {
	errs := ownerErrs(t, `
		type Node {
			int v;
			pack(this) Holder { return Holder.Has(this); }
		}
		enum Holder { Has(Node? n), Empty, }
		test() {}
	`)
	expectOwnerError(t, errs, "cannot consume 'this'")
}

// === T0652: move of a for-in loop binding over a native container of
// single-owner native handles must be rejected (sibling of T0596 for the
// loop-binding shape; T0617 fixed direct `<-h` codegen but `x := h; <-x`
// still SIGSEGV'd/hung because the value-copy aliases the slot).

// Inferred var-decl from the loop binding (`x := h`) is the canonical T0652
// repro shape. Without the fix this codegens, runs, then double-frees the
// Vector's slot at scope-exit (Task.drop hangs spinning on freed G).
func TestT0652_VectorForInBindingVarDeclRejected(t *testing.T) {
	errs := ownerErrs(t, `
		worker() int { return 42; }
		test() {
			Vector[Task[int]] v = [go worker(), go worker()];
			for h in v {
				x := h;
				_ = <-x;
			}
		}
	`)
	expectOwnerError(t, errs, "cannot move for-in loop binding 'h'")
}

// Typed var-decl form (`Task[int] x = h;`) — same tryMove path, distinct
// surface syntax.
func TestT0652_VectorForInBindingTypedVarDeclRejected(t *testing.T) {
	errs := ownerErrs(t, `
		worker() int { return 42; }
		test() {
			Vector[Task[int]] v = [go worker(), go worker()];
			for h in v {
				Task[int] x = h;
				_ = <-x;
			}
		}
	`)
	expectOwnerError(t, errs, "cannot move for-in loop binding 'h'")
}

// Assignment RHS — `other = h;` flows through tryMove on the RHS.
func TestT0652_VectorForInBindingAssignRejected(t *testing.T) {
	errs := ownerErrs(t, `
		worker() int { return 42; }
		test() {
			Vector[Task[int]] v = [go worker(), go worker()];
			Task[int] other = go worker();
			for h in v {
				other = h;
			}
			_ = <-other;
		}
	`)
	expectOwnerError(t, errs, "cannot move for-in loop binding 'h'")
}

// Passing the binding to a `~T` callee — tryMoveConsume path.
func TestT0652_VectorForInBindingConsumeArgRejected(t *testing.T) {
	errs := ownerErrs(t, `
		worker() int { return 42; }
		take(Task[int] move t) int { return <-t; }
		test() {
			Vector[Task[int]] v = [go worker(), go worker()];
			for h in v {
				_ = take(h);
			}
		}
	`)
	expectOwnerError(t, errs, "cannot move for-in loop binding 'h'")
}

// `return h;` from inside a for-in over a for-in.
func TestT0652_VectorForInBindingReturnRejected(t *testing.T) {
	errs := ownerErrs(t, `
		worker() int { return 42; }
		first(Vector[Task[int]] move v) Task[int] {
			for h in v {
				return h;
			}
			return go worker();
		}
		test() {}
	`)
	expectOwnerError(t, errs, "cannot move for-in loop binding 'h'")
}

// Fixed-size Array iteration — same aliasing shape, different lowering.
func TestT0652_ArrayForInBindingVarDeclRejected(t *testing.T) {
	errs := ownerErrs(t, `
		worker() int { return 42; }
		test() {
			Task[int][2] arr = [go worker(), go worker()];
			for h in arr {
				x := h;
				_ = <-x;
			}
		}
	`)
	expectOwnerError(t, errs, "cannot move for-in loop binding 'h'")
}

// Map value-position iteration (`for k, h in m { x := h }`) — Map value
// slots have the same aliasing shape; only the value binding is flagged.
func TestT0652_MapForInBindingVarDeclRejected(t *testing.T) {
	errs := ownerErrs(t, `
		worker() int { return 42; }
		test() {
			Map[int, Task[int]] m = {0: go worker(), 1: go worker()};
			for k, h in m {
				x := h;
				_ = <-x;
				_ = k;
			}
		}
	`)
	expectOwnerError(t, errs, "cannot move for-in loop binding 'h'")
}

// Mutex element type (different single-owner native handle).
func TestT0652_MutexVecForInBindingRejected(t *testing.T) {
	errs := ownerErrs(t, `
		test() {
			Mutex[int] m0 = Mutex[int](1);
			Mutex[int] m1 = Mutex[int](2);
			Vector[Mutex[int]] v = [m0, m1];
			for h in v {
				x := h;
				_ = x;
			}
		}
	`)
	expectOwnerError(t, errs, "cannot move for-in loop binding 'h'")
}

// MutexGuard is single-owner too (use-bound, but the move check still
// applies before the use-bound check fires). Exercises the IsMutexGuard
// arm of isSingleOwnerNativeType for the for-in shape.
func TestT0652_MutexGuardVecForInBindingRejected(t *testing.T) {
	errs := ownerErrs(t, `
		test() {
			Mutex[int] m0 = Mutex[int](1);
			use g0 := m0.lock();
			MutexGuard[int][1] arr = [g0];
			for h in arr {
				x := h;
				_ = x;
			}
		}
	`)
	expectOwnerError(t, errs, "cannot move for-in loop binding 'h'")
}

// Optional wrapping coverage: `Vector[Task[int]?]` slot still resolves to
// single-owner (isSingleOwnerNativeType recurses through Optional).
func TestT0652_OptionalTaskVecForInBindingRejected(t *testing.T) {
	errs := ownerErrs(t, `
		worker() int { return 42; }
		test() {
			Task[int]? t0 = go worker();
			Task[int]? t1 = go worker();
			Vector[Task[int]?] v = [t0, t1];
			for h in v {
				x := h;
				_ = x;
			}
		}
	`)
	expectOwnerError(t, errs, "cannot move for-in loop binding 'h'")
}

// Parenthesised binding move — `x := (h)` — ParenExpr peel must fire.
func TestT0652_ParenthesisedBindingMoveRejected(t *testing.T) {
	errs := ownerErrs(t, `
		worker() int { return 42; }
		test() {
			Vector[Task[int]] v = [go worker(), go worker()];
			for h in v {
				x := (h);
				_ = <-x;
			}
		}
	`)
	expectOwnerError(t, errs, "cannot move for-in loop binding 'h'")
}

// OptionalUnwrap peel — `x := h!` for `Vector[Task[int]?]` reaches the
// inner IdentExpr through the peel loop.
func TestT0652_OptionalUnwrapBindingMoveRejected(t *testing.T) {
	errs := ownerErrs(t, `
		worker() int { return 42; }
		test() {
			Task[int]? t0 = go worker();
			Task[int]? t1 = go worker();
			Vector[Task[int]?] v = [t0, t1];
			for h in v {
				x := h!;
				_ = <-x;
			}
		}
	`)
	expectOwnerError(t, errs, "cannot move for-in loop binding 'h'")
}

// Sibling for-in loops reusing the same binding name: the first loop sets
// the flag on `h`, the second loop iterates a non-single-owner vector and
// must NOT inherit the flag from the prior loop. Verifies the delete-on-exit
// cleanup path in checkForInStmt.
func TestT0652_SiblingForInClearsFlag(t *testing.T) {
	ownerOK(t, `
		worker() int { return 42; }
		test() {
			Vector[Task[int]] v1 = [go worker(), go worker()];
			Vector[int] v2 = [1, 2, 3];
			total := 0;
			for h in v1 {
				total = total + (<-h);
			}
			for h in v2 {
				x := h;
				total = total + x;
			}
			_ = total;
		}
	`)
}

// --- Positive (allow) regression guards ---

// `<-h` direct receive on the loop binding is the T0617 fixed path:
// UnaryExpr does NOT go through tryMove, so the new check must NOT
// over-reject it.
func TestT0652_DirectReceiveAllowed(t *testing.T) {
	ownerOK(t, `
		worker() int { return 42; }
		test() {
			Vector[Task[int]] v = [go worker(), go worker()];
			total := 0;
			for h in v {
				total = total + (<-h);
			}
			_ = total;
		}
	`)
}

// Vector[int] iteration — int is Copy, no aliasing concern; the new
// check must not fire.
func TestT0652_VectorIntForInAllowed(t *testing.T) {
	ownerOK(t, `
		test() {
			Vector[int] v = [1, 2, 3];
			total := 0;
			for h in v {
				x := h;
				total = total + x;
			}
		}
	`)
}

// Vector[string] iteration — string has dup-on-yield (codegen handles
// the per-iteration ownership transfer); single-owner check must not fire.
func TestT0652_VectorStringForInMoveAllowed(t *testing.T) {
	ownerOK(t, `
		test() {
			Vector[string] v = ["a", "b"];
			for h in v {
				x := h;
				_ = x;
			}
		}
	`)
}

// T0978: User heap type (non-Copy, non-single-owner-native) move-out of an
// OWNED vector — the loop binding aliases the container's slot, so `x := h`
// would double-free at scope exit (both `x` and the container free the same
// instance). Previously allowed (silent double-free); now rejected by the
// broadened for-in alias guard.
func TestT0978_VectorBoxForInMoveRejected(t *testing.T) {
	errs := ownerErrs(t, `
		type Box { int v; }
		test() {
			Box b0 = Box(v: 1);
			Box b1 = Box(v: 2);
			Vector[Box] v = [b0, b1];
			for h in v {
				x := h;
				_ = x;
			}
		}
	`)
	expectOwnerError(t, errs, "cannot move for-in loop binding")
}

// Range iteration — Range elements are Copy, no aliasing concern.
func TestT0652_RangeForInAllowed(t *testing.T) {
	ownerOK(t, `
		test() {
			total := 0;
			for i in 0..3 {
				x := i;
				total = total + x;
			}
		}
	`)
}

// No-body-consume — for-in over Vector[Task] where the body doesn't move
// the binding. Must not error; T0503 unchanged.
func TestT0652_NoBodyConsumeAllowed(t *testing.T) {
	ownerOK(t, `
		worker() int { return 42; }
		test() {
			Vector[Task[int]] v = [go worker(), go worker()];
			count := 0;
			for h in v {
				count = count + 1;
			}
			_ = count;
		}
	`)
}

// Iterator-chain regression guard — `for h in v.iter()` over Vector[Task]
// goes through a different lowering (custom-iter / generator path) and the
// iterable's static type is _FnIter[T] (or similar), NOT Vector/Array/Map.
// `forInAliasingElementType` returns nil for those shapes, so the binding
// must NOT be flagged. Move of `h` inside the body is allowed (subject to
// the iterator's own per-yield ownership semantics, which are unchanged).
func TestT0652_IteratorChainAllowed(t *testing.T) {
	ownerOK(t, `
		worker() int { return 42; }
		test() {
			Vector[Task[int]] v = [go worker(), go worker()];
			total := 0;
			for h in v.iter() {
				total = total + (<-h);
			}
			_ = total;
		}
	`)
}

// === T0971: for-in over a *borrowed* container (T[]&/T[]~/.borrow). Reading is
// fine; moving an aliasing element binding out double-frees at the owner's drop,
// so it must be rejected. Copy and string elements stay movable. ===

// Read-only iteration of a borrowed-vector parameter must be accepted, and the
// borrow is not consumed (the iterable can be iterated again).
func TestT0971_ForInBorrowedVectorNoConsume(t *testing.T) {
	ownerOK(t, `
		type Box { string name; }
		count_twice(Box[] src) int {
			n := 0;
			for x in src { n = n + x.name.len; }
			for x in src { n = n + 1; }
			return n;
		}
		test() {}
	`)
}

// Moving a non-Copy element binding out of a borrowed-vector parameter
// (push into another container) must be rejected.
func TestT0971_ForInBorrowedMoveOutRejected(t *testing.T) {
	errs := ownerErrs(t, `
		type Box { string name; }
		drain(Box[] src) int {
			sink := Box[]();
			for x in src { sink.push(x); }
			return sink.len;
		}
		test() {}
	`)
	expectOwnerError(t, errs, "cannot move for-in loop binding")
}

// Var-decl form (`Box y = x;`) of the move-out — same tryMove path.
func TestT0971_ForInBorrowedMoveOutVarDeclRejected(t *testing.T) {
	errs := ownerErrs(t, `
		type Box { string name; }
		drain(Box[] src) {
			for x in src {
				Box y = x;
				_ = y;
			}
		}
		test() {}
	`)
	expectOwnerError(t, errs, "cannot move for-in loop binding")
}

// A `T[] &b = v` local is also ref-typed → its move-out is rejected too.
func TestT0971_ForInBorrowedLocalMoveOutRejected(t *testing.T) {
	errs := ownerErrs(t, `
		type Box { string name; }
		test() {
			boxes := [Box(name: "a"), Box(name: "b")];
			Box[] &b = boxes;
			sink := Box[]();
			for x in b { sink.push(x); }
		}
	`)
	expectOwnerError(t, errs, "cannot move for-in loop binding")
}

// Mutable-borrow (`~`) parameter — also ref-typed, move-out rejected.
func TestT0971_ForInMutBorrowedMoveOutRejected(t *testing.T) {
	errs := ownerErrs(t, `
		type Box { string name; }
		drain(Box[] ~src) int {
			sink := Box[]();
			for x in src { sink.push(x); }
			return sink.len;
		}
		test() {}
	`)
	expectOwnerError(t, errs, "cannot move for-in loop binding")
}

// Consume-arg form (`take(x)` into a `~` param) of the move-out.
func TestT0971_ForInBorrowedConsumeArgRejected(t *testing.T) {
	errs := ownerErrs(t, `
		type Box { string name; }
		take(Box move b) int { return b.name.len; }
		drain(Box[] src) {
			for x in src { _ = take(x); }
		}
		test() {}
	`)
	expectOwnerError(t, errs, "cannot move for-in loop binding")
}

// Copy elements (int) copy by value — move-out of a borrowed int[] is fine.
func TestT0971_ForInBorrowedCopyElementMovable(t *testing.T) {
	ownerOK(t, `
		drain(int[] src) int[] {
			sink := int[]();
			for x in src { sink.push(x); }
			return sink;
		}
		test() {}
	`)
}

// String elements are cloned per iteration (genForInVector dupStrings) — move-out
// of a borrowed string[] is sound and must be accepted.
func TestT0971_ForInBorrowedStringElementMovable(t *testing.T) {
	ownerOK(t, `
		drain(string[] src) string[] {
			sink := string[]();
			for x in src { sink.push(move x); }
			return sink;
		}
		test() {}
	`)
}

// T0978: move-out of an OWNED container's element is now rejected — the guard
// no longer keys on the ref type, so owned, plain-borrow-param, and borrowed-ref
// containers are all covered. (T0971 deliberately left this case passing; this
// is the broader gap T0978 closes.)
func TestT0978_ForInOwnedMoveOutRejected(t *testing.T) {
	errs := ownerErrs(t, `
		type Box { string name; }
		test() {
			boxes := [Box(name: "a"), Box(name: "b")];
			sink := Box[]();
			for x in boxes { sink.push(x); }
		}
	`)
	expectOwnerError(t, errs, "cannot move for-in loop binding")
}

// Borrowed *map* with a non-Copy value type: moving a value binding out (the
// second binding of `for k, v in`) aliases the map's owned storage → rejected.
// Exercises the Map branch of forInAliasingElementType.
func TestT0971_ForInBorrowedMapMoveOutRejected(t *testing.T) {
	errs := ownerErrs(t, `
		type Box { string name; }
		drain(map[string, Box] src) {
			sink := Box[]();
			for k, v in src { sink.push(v); }
		}
		test() {}
	`)
	expectOwnerError(t, errs, "cannot move for-in loop binding")
}

// Borrowed *fixed-size array* with a non-Copy element: move-out rejected.
// Exercises the Array branch of forInAliasingElementType.
func TestT0971_ForInBorrowedArrayMoveOutRejected(t *testing.T) {
	errs := ownerErrs(t, `
		type Box { string name; }
		drain(Box[3] src) {
			sink := Box[]();
			for x in src { sink.push(x); }
		}
		test() {}
	`)
	expectOwnerError(t, errs, "cannot move for-in loop binding")
}

// Borrowed map/array whose element IS Copy (int) stays freely movable — the
// isCopyType skip must not flag these.
func TestT0971_ForInBorrowedCopyMapArrayMovable(t *testing.T) {
	ownerOK(t, `
		drain_map(map[string, int] m) int[] {
			sink := int[]();
			for k, v in m { sink.push(v); }
			return sink;
		}
		drain_arr(int[3] a) int[] {
			sink := int[]();
			for x in a { sink.push(x); }
			return sink;
		}
		test() {}
	`)
}

// === T0978: move-out of a for-in element binding over an OWNED or
// plain-borrow-param container (not just a borrowed-ref one) double-frees,
// because the binding aliases the container's element storage and the
// container still drops every element at scope exit. The broadened guard
// rejects move-out for all aliasing-container shapes. ===

// Owned vector, push into another container — the canonical repro.
func TestT0978_ForInOwnedVectorPushRejected(t *testing.T) {
	errs := ownerErrs(t, `
		type Box { string name; }
		test() {
			boxes := [Box(name: "a"), Box(name: "b")];
			sink := Box[]();
			for x in boxes { sink.push(x); }
		}
	`)
	expectOwnerError(t, errs, "cannot move for-in loop binding")
}

// Plain-borrow parameter (`Box[] src`, not `Box[]&`) — the element binding
// still aliases the caller's storage, which the caller drops.
func TestT0978_ForInPlainBorrowParamPushRejected(t *testing.T) {
	errs := ownerErrs(t, `
		type Box { string name; }
		drain(Box[] src) int {
			sink := Box[]();
			for x in src { sink.push(x); }
			return sink.len;
		}
		test() {}
	`)
	expectOwnerError(t, errs, "cannot move for-in loop binding")
}

// Var-decl form of the move-out over an owned vector.
func TestT0978_ForInOwnedVarDeclRejected(t *testing.T) {
	errs := ownerErrs(t, `
		type Box { string name; }
		test() {
			boxes := [Box(name: "a"), Box(name: "b")];
			for x in boxes {
				Box y = x;
				_ = y;
			}
		}
	`)
	expectOwnerError(t, errs, "cannot move for-in loop binding")
}

// Nested vector (`Box[][]`): the element binding is itself a non-Copy
// `Box[]`, so moving it out double-frees the inner vector.
func TestT0978_ForInNestedVectorPushRejected(t *testing.T) {
	errs := ownerErrs(t, `
		type Box { string name; }
		test() {
			a := [Box(name: "a")];
			b := [Box(name: "b")];
			nested := [a, b];
			sink := Box[][]();
			for inner in nested { sink.push(inner); }
		}
	`)
	expectOwnerError(t, errs, "cannot move for-in loop binding")
}

// Owned map, moving the *value* binding (second binding of `for k, v in`) out.
func TestT0978_ForInOwnedMapValuePushRejected(t *testing.T) {
	errs := ownerErrs(t, `
		type Box { string name; }
		test() {
			m := map[string, Box]();
			m["a"] = Box(name: "a");
			sink := Box[]();
			for k, v in m { sink.push(v); }
		}
	`)
	expectOwnerError(t, errs, "cannot move for-in loop binding")
}

// Owned map iterated as a single pair binding — the pair carries the aliased
// value, so moving the pair out is rejected too.
func TestT0978_ForInOwnedMapPairMoveRejected(t *testing.T) {
	errs := ownerErrs(t, `
		type Box { string name; }
		test() {
			m := map[string, Box]();
			m["a"] = Box(name: "a");
			for pair in m {
				y := pair;
				_ = y;
			}
		}
	`)
	expectOwnerError(t, errs, "cannot move for-in loop binding")
}

// --- Negative cases that must still type-check ---

// Read-only iteration of an owned vector — no move, accepted.
func TestT0978_ForInOwnedReadOnlyAllowed(t *testing.T) {
	ownerOK(t, `
		type Box { string name; }
		test() {
			boxes := [Box(name: "a"), Box(name: "b")];
			n := 0;
			for x in boxes { n = n + x.name.len; }
			_ = n;
		}
	`)
}

// Passing the binding to a plain (borrow) parameter is a read, not a move —
// accepted (the callee borrows, the container keeps ownership).
func TestT0978_ForInOwnedBorrowCallAllowed(t *testing.T) {
	ownerOK(t, `
		type Box { string name; }
		read_box(Box b) int { return b.name.len; }
		test() {
			boxes := [Box(name: "a"), Box(name: "b")];
			n := 0;
			for x in boxes { n = n + read_box(x); }
			_ = n;
		}
	`)
}

// Pushing the *key* of an owned map (a string, cloned per iteration) is sound;
// only the aliased value binding is flagged.
func TestT0978_ForInOwnedMapKeyPushAllowed(t *testing.T) {
	ownerOK(t, `
		type Box { string name; }
		test() {
			m := map[string, Box]();
			m["a"] = Box(name: "a");
			keys := string[]();
			for k, v in m { keys.push(move k); }
			_ = keys;
		}
	`)
}

// String elements are cloned per iteration (genForInVector dupStrings), so
// move-out of a string element from an owned vector is sound.
func TestT0978_ForInOwnedStringElementMovable(t *testing.T) {
	ownerOK(t, `
		test() {
			names := ["a", "b"];
			sink := string[]();
			for s in names { sink.push(move s); }
			_ = sink;
		}
	`)
}

// Copy elements (int) copy by value — move-out of an owned int[] is fine.
func TestT0978_ForInOwnedCopyElementMovable(t *testing.T) {
	ownerOK(t, `
		test() {
			nums := [1, 2, 3];
			sink := int[]();
			for x in nums { sink.push(x); }
			_ = sink;
		}
	`)
}

// Generic body with a bare type-parameter element is NOT flagged — the
// ownership pass checks the generic body once with `T` unbound and never
// re-checks monomorphized instances; flagging `T` would over-reject legitimate
// Copy-`T` instantiations. (Concrete element types are still caught.)
func TestT0978_ForInGenericTypeParamElementNotFlagged(t *testing.T) {
	ownerOK(t, `
		drain[T](T[] v) T[] {
			sink := T[]();
			for x in v { sink.push(move x); }
			return sink;
		}
		test() {}
	`)
}

// A *borrowed* container of single-owner native handles (`Task[int][]&`, only
// iterable since T0971) must still reject move-out — and keep the dedicated
// single-owner message (the T0652 block now strips the ref type so it covers
// borrowed containers too, instead of the broadened alias message). Guards the
// regression where excluding single-owner natives from the alias set left this
// shape unflagged.
func TestT0978_ForInBorrowedSingleOwnerMoveOutRejected(t *testing.T) {
	errs := ownerErrs(t, `
		worker() int { return 42; }
		drain(Task[int][] src) {
			sink := Task[int][]();
			for h in src { sink.push(h); }
		}
		test() {}
	`)
	expectOwnerError(t, errs, "to receive the value directly")
}

// Borrow-destructure of a for-in pair binding (`(a, b) := tup`) must still
// type-check — codegen (T0371) gives the pieces no drop bindings, so the
// destructure is a borrow, not a move. The carve-out skips the move check.
func TestT0978_ForInOwnedTupleDestructureAllowed(t *testing.T) {
	ownerOK(t, `
		type Box { string name; }
		test() {
			m := map[string, Box]();
			m["a"] = Box(name: "a");
			n := 0;
			for pair in m {
				(k, b) := pair;
				n = n + b.name.len;
			}
			_ = n;
		}
	`)
}

// The destructure is a *borrow*, so the carve-out marks each non-Copy piece
// Borrowed — moving one out (`sink.push(b)`) must still be rejected. (Marking
// the pieces Owned, as an earlier draft did, let the aliased value escape and
// double-free at the map's drop.)
func TestT0978_ForInDestructurePieceConsumeRejected(t *testing.T) {
	errs := ownerErrs(t, `
		type Box { string name; }
		test() {
			m := map[string, Box]();
			m["a"] = Box(name: "a");
			sink := Box[]();
			for pair in m {
				(k, b) := pair;
				sink.push(b);
			}
		}
	`)
	expectOwnerError(t, errs, "cannot move borrowed value")
}

// Re-binding a non-Copy destructure piece (`y := b`) and then moving it out is
// also rejected: the var-decl of a Borrowed ident is caught by the T0568
// rejectBorrowedIdentVarDecl path, so the alias cannot be laundered into an
// Owned local.
func TestT0978_ForInDestructurePieceRebindRejected(t *testing.T) {
	errs := ownerErrs(t, `
		type Box { string name; }
		test() {
			m := map[string, Box]();
			m["a"] = Box(name: "a");
			for pair in m {
				(k, b) := pair;
				y := b;
				_ = y;
			}
		}
	`)
	expectOwnerError(t, errs, "cannot move borrowed value")
}

// String and Copy destructure pieces stay movable (forInElementAliasesContainer
// excludes both): a `(string, int)[]` for-in whose pieces are pushed onto other
// containers must still type-check. String elements are dup'd on store and Copy
// elements are value copies, so neither double-frees — moving them is sound
// (verified leak-free at runtime in tests/e2e/forin_move_out_test). Regression
// guard for the over-rejection an all-non-Copy-Borrowed marking would cause.
func TestT0978_ForInDestructureStringCopyPieceMovable(t *testing.T) {
	ownerOK(t, `
		test() {
			v := (string, int)[]();
			v.push(("ab", 1));
			strs := string[]();
			nums := int[]();
			for tup in v {
				(s, n) := tup;
				strs.push(move s);
				nums.push(n);
			}
			_ = strs; _ = nums;
		}
	`)
}

// A string destructure piece can also be *read* into a fresh string (the
// `concat = concat + s` shape that tuples_test.pr's T0370 regression guard
// exercises) — strings are not flagged, so the read is not mistaken for a move.
func TestT0978_ForInDestructureStringPieceReadAllowed(t *testing.T) {
	ownerOK(t, `
		test() {
			v := (string, int)[]();
			v.push(("ab", 1));
			concat := "";
			for tup in v {
				(s, n) := tup;
				concat = concat + s;
				_ = n;
			}
			_ = concat;
		}
	`)
}

// `return x` is a distinct move form (ReturnStmt → tryMove) from the
// push/var-decl/consume-arg forms above. Returning a for-in alias binding out
// of a plain-borrow-param container escapes an alias of the caller's storage,
// so it must be rejected too.
func TestT0978_ForInPlainBorrowParamReturnRejected(t *testing.T) {
	errs := ownerErrs(t, `
		type Box { string name; }
		extract(Box[] src) Box {
			for x in src { return x; }
			return Box(name: "z");
		}
		test() {}
	`)
	expectOwnerError(t, errs, "cannot move for-in loop binding")
}

// Owned fixed-size array (`Box[2]`) of a non-Copy, non-single-owner heap user
// type — the for-in binding aliases the array slot, so move-out double-frees
// at the array's drop. T0652 covers fixed arrays of single-owner natives and
// T0971 covers borrowed arrays; this is the owned/heap-user-type T0978 case.
func TestT0978_ForInOwnedArrayMoveOutRejected(t *testing.T) {
	errs := ownerErrs(t, `
		type Box { string name; }
		test() {
			Box[2] arr = [Box(name: "a"), Box(name: "b")];
			sink := Box[]();
			for x in arr { sink.push(x); }
		}
	`)
	expectOwnerError(t, errs, "cannot move for-in loop binding")
}

// Destructure piece named `_` is skipped (the carve-out's `name == "_"`
// branch): `(_, b) := pair` discards the key and binds only the value. The `_`
// must not disturb the marking of the remaining pieces — `b` (a non-Copy Box)
// still becomes Borrowed, so moving it out is rejected.
func TestT0978_ForInDestructureWildcardPieceConsumeRejected(t *testing.T) {
	errs := ownerErrs(t, `
		type Box { string name; }
		test() {
			m := map[string, Box]();
			m["a"] = Box(name: "a");
			sink := Box[]();
			for pair in m {
				(_, b) := pair;
				sink.push(b);
			}
		}
	`)
	expectOwnerError(t, errs, "cannot move borrowed value")
}

// Companion allow case: `(_, b) := pair` then only *reading* `b` is fine — the
// wildcard piece is skipped and the read of the Borrowed `b` is not a move.
func TestT0978_ForInDestructureWildcardPieceReadAllowed(t *testing.T) {
	ownerOK(t, `
		type Box { string name; }
		test() {
			m := map[string, Box]();
			m["a"] = Box(name: "a");
			n := 0;
			for pair in m {
				(_, b) := pair;
				n = n + b.name.len;
			}
			_ = n;
		}
	`)
}

// Nested for-in over DIFFERENT-named aliasing bindings: the outer `x` must stay
// flagged across the inner loop's flag-clear (the inner loop deletes its own
// `y` entry on exit, not `x`). Move-out of the outer `x` after the inner loop is
// still rejected. (The same-binding-name nesting that would exercise the
// hadPrev* save/restore branches is unreachable — Promise rejects shadowing at
// sema, so a nested for-in can never reuse the outer binding name.)
func TestT0978_ForInNestedDistinctBindingAliasGuardHolds(t *testing.T) {
	errs := ownerErrs(t, `
		type Box { string name; }
		test() {
			v1 := [Box(name: "a")];
			v2 := [Box(name: "b")];
			sink := Box[]();
			for x in v1 {
				for y in v2 {
					n := y.name.len;
					_ = n;
				}
				sink.push(x);
			}
		}
	`)
	expectOwnerError(t, errs, "cannot move for-in loop binding")
}

// === T0837: moving/consuming a single-owner handle field out of a shared
// borrow must be rejected ===
//
// Force-unwrapping a `Mutex[T]?` / `Task[T]?` field on a *borrowed* owner and
// then MOVING (binding) or CONSUMING (`<-`) the handle aliases the underlying
// i8* while the real owner (in the caller) still drops it → double-free. The
// owned counterparts (`~this`, owned local, `~` param) keep the field live and
// stay accepted (T0806). The borrowing `.lock()` temp also stays accepted.

// Shape 1: Mutex binding move-out of `this`.
func TestT0837_MutexBindingMoveOutOfBorrowedThisRejected(t *testing.T) {
	errs := ownerErrs(t, `
		type MtxHolder {
			Mutex[int]? mtx;
			drop(~this) {}
			steal(this) int {
				Mutex[int] m = this.mtx!;
				return m.lock().borrow;
			}
		}
		test() {}
	`)
	expectOwnerError(t, errs, "cannot move single-owner handle field")
}

// Shape 2: Task consuming await `<-(this.tsk!)` out of `this`.
func TestT0837_TaskAwaitOutOfBorrowedThisRejected(t *testing.T) {
	errs := ownerErrs(t, `
		worker() int { return 42; }
		type TskHolder {
			Task[int]? tsk;
			drop(~this) {}
			await_borrow(this) int {
				return <-(this.tsk!);
			}
		}
		test() {}
	`)
	expectOwnerError(t, errs, "cannot move single-owner handle field")
}

// Shape 3a: Mutex binding move-out of a free-function `&owner` parameter
// (SharedRef root carries Owned state — exercises the type-based discriminator).
func TestT0837_MutexBindingMoveOutOfSharedRefParamRejected(t *testing.T) {
	errs := ownerErrs(t, `
		type MtxH { Mutex[int]? mtx; drop(~this) {} }
		steal(MtxH h) int {
			Mutex[int] m = h.mtx!;
			return m.lock().borrow;
		}
		test() {}
	`)
	expectOwnerError(t, errs, "cannot move single-owner handle field")
}

// Shape 3b: Task await out of a free-function `&owner` parameter.
func TestT0837_TaskAwaitOutOfSharedRefParamRejected(t *testing.T) {
	errs := ownerErrs(t, `
		worker() int { return 42; }
		type TskH { Task[int]? tsk; drop(~this) {} }
		await_it(TskH h) int {
			return <-(h.tsk!);
		}
		test() {}
	`)
	expectOwnerError(t, errs, "cannot move single-owner handle field")
}

// Chained `outer.inner.mtx!` out of a borrowed root.
func TestT0837_ChainedMoveOutOfBorrowedRootRejected(t *testing.T) {
	errs := ownerErrs(t, `
		type Inner { Mutex[int]? mtx; drop(~this) {} }
		type Outer { Inner inner; drop(~this) {} }
		grab(Outer o) int {
			Mutex[int] m = o.inner.mtx!;
			return m.lock().borrow;
		}
		test() {}
	`)
	expectOwnerError(t, errs, "cannot move single-owner handle field")
}

// Acceptance: `~this` binding move-out keeps the field live (callee owns it).
func TestT0837_MutexBindingMoveOutOfOwnedThisAllowed(t *testing.T) {
	ownerOK(t, `
		type MtxHolder {
			Mutex[int]? mtx;
			drop(~this) {}
			steal(~this) int {
				Mutex[int] m = this.mtx!;
				return m.lock().borrow;
			}
		}
		test() {}
	`)
}

// Acceptance: owned-local binding move-out.
func TestT0837_MutexBindingMoveOutOfOwnedLocalAllowed(t *testing.T) {
	ownerOK(t, `
		type MtxHolder { Mutex[int]? mtx; drop(~this) {} }
		test() {
			h := MtxHolder(mtx: Mutex[int](5));
			Mutex[int] m = h.mtx!;
			_ = m.lock().borrow;
		}
	`)
}

// Acceptance: owned-local consuming await `<-(h.tsk!)`.
func TestT0837_TaskAwaitOutOfOwnedLocalAllowed(t *testing.T) {
	ownerOK(t, `
		worker() int { return 42; }
		type TskHolder { Task[int]? tsk; drop(~this) {} }
		test() {
			h := TskHolder(tsk: go worker());
			_ = <-(h.tsk!);
		}
	`)
}

// Acceptance: the borrowing `.lock()` temp on `this` — `.lock()` borrows, never
// moves, so it must NOT be rejected.
func TestT0837_BorrowingLockTempOnBorrowedThisAllowed(t *testing.T) {
	ownerOK(t, `
		type MtxHolder {
			Mutex[int]? mtx;
			drop(~this) {}
			peek(this) int {
				return (this.mtx!).lock().borrow;
			}
		}
		test() {}
	`)
}

// Acceptance: a `~` parameter move-out — the callee owns the handle.
func TestT0837_MutexMoveOutOfMutParamAllowed(t *testing.T) {
	ownerOK(t, `
		type MtxH { Mutex[int]? mtx; drop(~this) {} }
		steal(MtxH ~h) int {
			Mutex[int] m = h.mtx!;
			return m.lock().borrow;
		}
		test() {}
	`)
}

// Paren-wrapped member target out of a borrowed root must still be rejected —
// `(o.inner).mtx!` peels through the ParenExpr in memberChainRoot's chain walk,
// so wrapping the owner in parens is not an evasion of the check.
func TestT0837_ParenWrappedMemberTargetRejected(t *testing.T) {
	errs := ownerErrs(t, `
		type Inner { Mutex[int]? mtx; drop(~this) {} }
		type Outer { Inner inner; drop(~this) {} }
		grab(Outer o) int {
			Mutex[int] m = (o.inner).mtx!;
			return m.lock().borrow;
		}
		test() {}
	`)
	expectOwnerError(t, errs, "cannot move single-owner handle field")
}

// Acceptance: a getter named like a field that *returns* a freshly-constructed
// single-owner handle is NOT a field move — the getter produces an owned value,
// so moving it out of a borrowed owner is safe and must not be rejected
// (exercises the getter guard, mirroring checkFieldMoveOwnership).
func TestT0837_GetterReturningHandleOutOfBorrowAllowed(t *testing.T) {
	ownerOK(t, `
		type MtxH {
			drop(~this) {}
			get fresh_mtx Mutex[int]? { return Mutex[int](9); }
		}
		grab(MtxH h) int {
			Mutex[int] m = h.fresh_mtx!;
			return m.lock().borrow;
		}
		test() {}
	`)
}

// === T0953: awaiting a BORROWED-source Task double-consumes the handle ===
//
// `<-` (await) on a Task is a *consuming* op (joins the goroutine, frees the G
// struct + result buffer). Awaiting a task the current function does NOT own — a
// bare borrowed ident (`<-a`) or an inline elvis whose selected operand is a borrow
// (`<-(a ?: b)`) — double-joins/double-frees with the real owner's drop → SEGV.
// rejectMemberHandleMoveOutOfBorrow (T0837) only sees member/index/transient owners;
// rejectBorrowedTaskAwait closes the bare-ident and elvis holes. The IsTask gate
// keeps non-consuming borrowed-Channel receive (also `<-`) legal.

// Reject: the repro — awaiting an inline elvis whose `a` operand is a borrowed param.
func TestT0953_AwaitBorrowedElvisParamRejected(t *testing.T) {
	errs := ownerErrs(t, `
		worker() int { return 42; }
		await_it(Task[int]? a, Task[int] b) int {
			return <-(a ?: b);
		}
		test() {}
	`)
	expectOwnerError(t, errs, "cannot await borrowed task")
}

// Reject: the adjacent hole — awaiting a bare borrowed Task param (`<-a`).
func TestT0953_AwaitBareBorrowedTaskParamRejected(t *testing.T) {
	errs := ownerErrs(t, `
		worker() int { return 42; }
		await_it(Task[int] a) int {
			return <-a;
		}
		test() {}
	`)
	expectOwnerError(t, errs, "cannot await borrowed task")
}

// Reject: only the elvis RIGHT operand is borrowed (owned-optional left selects to
// the borrowed default on the none-path) — recursion must check both operands.
func TestT0953_AwaitBorrowedElvisRightOperandRejected(t *testing.T) {
	errs := ownerErrs(t, `
		worker() int { return 42; }
		await_it(Task[int] b) int {
			Task[int]? local = go worker();
			return <-(local ?: b);
		}
		test() {}
	`)
	expectOwnerError(t, errs, "cannot await borrowed task")
}

// Accept: owned-local elvis source — state is Owned, not Borrowed, so not rejected.
func TestT0953_AwaitOwnedLocalElvisAllowed(t *testing.T) {
	ownerOK(t, `
		worker() int { return 42; }
		test() {
			a := go worker();
			b := go worker();
			Task[int]? oa = a;
			_ = <-(oa ?: b);
		}
	`)
}

// Accept: owned-local direct await.
func TestT0953_AwaitOwnedLocalDirectAllowed(t *testing.T) {
	ownerOK(t, `
		worker() int { return 42; }
		test() {
			b := go worker();
			_ = <-b;
		}
	`)
}

// Accept: borrowed-Channel receive (also spelled `<-`) is NON-consuming — the IsTask
// gate must not fire on it.
func TestT0953_BorrowedChannelReceiveAllowed(t *testing.T) {
	ownerOK(t, `
		recv(Channel[int] ch) int { return (<-ch) ?: -1; }
		test() {}
	`)
}

// Accept: an owned `~Task` move-in param IS awaitable — the remedy works.
func TestT0953_AwaitOwnedMoveParamAllowed(t *testing.T) {
	ownerOK(t, `
		worker() int { return 42; }
		f(Task[int] move a) int { return <-a; }
		test() {}
	`)
}

// Reject: an if-expr surfacing a borrowed Task param (`<-(if c { a } else { b })`).
// Codegen forwards the selected branch value directly, so the borrowed task reaches
// the consuming `<-` unchanged — same crash class as the elvis form. The walker must
// recurse into both branch blocks (mirrors findBorrowedNonAliasSafeIdent).
func TestT0953_AwaitBorrowedIfExprRejected(t *testing.T) {
	errs := ownerErrs(t, `
		worker() int { return 42; }
		await_it(Task[int] a, Task[int] b, bool c) int {
			return <-(if c { a } else { b });
		}
		test() {}
	`)
	expectOwnerError(t, errs, "cannot await borrowed task")
}

// Reject: a match-expr surfacing a borrowed Task param. Both arm bodies (and arm
// blocks) must be walked.
func TestT0953_AwaitBorrowedMatchExprRejected(t *testing.T) {
	errs := ownerErrs(t, `
		worker() int { return 42; }
		await_it(Task[int] a, Task[int] b, int sel) int {
			return <-(match sel { 0 => a, _ => b });
		}
		test() {}
	`)
	expectOwnerError(t, errs, "cannot await borrowed task")
}

// Reject: force-unwrap of a borrowed optional Task param (`<-(a!)`). peelAwaitWrappers
// peels the `!` so the borrowed ident leaf is reached.
func TestT0953_AwaitForceUnwrapBorrowedOptParamRejected(t *testing.T) {
	errs := ownerErrs(t, `
		worker() int { return 42; }
		await_it(Task[int]? a) int {
			return <-(a!);
		}
		test() {}
	`)
	expectOwnerError(t, errs, "cannot await borrowed task")
}

// Reject: a single-owner handle field read out of a borrowed owner as an elvis
// operand (`<-(h.tsk ?: local)`). The member leaf delegates to the T0837 reject —
// rejectMemberHandleMoveOutOfBorrow — so the diagnostic is the field-move message,
// not the borrowed-ident message. Distinct from TestT0837_TaskAwaitOutOf* (direct
// `<-(h.tsk!)`): here the member is buried inside an elvis whose other operand is
// owned, the hole that the unified await walker closes.
func TestT0953_AwaitMemberOutOfBorrowElvisOperandRejected(t *testing.T) {
	errs := ownerErrs(t, `
		worker() int { return 42; }
		type Holder { Task[int]? tsk; drop(~this) {} }
		await_it(Holder h) int {
			Task[int] local = go worker();
			return <-(h.tsk ?: local);
		}
		test() {}
	`)
	expectOwnerError(t, errs, "cannot move single-owner handle field")
}

// Accept: an if-expr over owned locals is NOT falsely rejected — the walker must only
// fire on Borrowed leaves. (Ownership-only acceptance; the owned-await runtime path is
// exercised by the e2e suite.)
func TestT0953_AwaitOwnedIfExprAllowed(t *testing.T) {
	ownerOK(t, `
		worker() int { return 42; }
		test() {
			a := go worker();
			b := go worker();
			c := true;
			_ = <-(if c { a } else { b });
		}
	`)
}

// Reject: a block-bodied match arm surfacing a borrowed Task param
// (`<-(match sel { 0 => { a }, _ => { b } })`). The expression-arm form is covered by
// TestT0953_AwaitBorrowedMatchExprRejected; this exercises the arm.Block path of the
// walker (rejectAwaitNonOwnedSourceInBlock), which recurses into the block's trailing
// result expression. Codegen forwards the selected arm's block value directly, so a
// borrowed task reaches the consuming `<-` unchanged — same crash class.
func TestT0953_AwaitBorrowedMatchBlockArmRejected(t *testing.T) {
	errs := ownerErrs(t, `
		worker() int { return 42; }
		await_it(Task[int] a, Task[int] b, int sel) int {
			return <-(match sel { 0 => { a }, _ => { b } });
		}
		test() {}
	`)
	expectOwnerError(t, errs, "cannot await borrowed task")
}

// Accept: a match-expr over owned locals is NOT falsely rejected — the walker visits
// both arms, finds only Owned leaves, and falls through (return false after the arm
// loop). Mirrors TestT0953_AwaitOwnedIfExprAllowed for the match shape. Ownership-only
// acceptance: the owned-match-await *runtime* path currently double-frees in codegen
// (genValueMatch omits the arm-result drop-flag clear that genEnumMatch performs),
// filed separately as T0975; ownership correctly accepts it (owned operands).
func TestT0953_AwaitOwnedMatchExprAllowed(t *testing.T) {
	ownerOK(t, `
		worker() int { return 42; }
		test() {
			a := go worker();
			b := go worker();
			sel := 0;
			_ = <-(match sel { 0 => a, _ => b });
		}
	`)
}

// === T0841: moving/consuming a single-owner handle out of a TRANSIENT owner ===
//
// Sibling hole to T0837: the same move/consume out of a transient owner (a
// function/method/constructor call result — no variable at the root of the member
// chain) must also be rejected. The temporary owns the parent struct and drops it
// at end of the full expression, while the move/`<-` also takes ownership of the
// handle's i8* → double-free → segfault. There is no owned-local escape hatch for a
// temporary, so the non-variable root is rejected unconditionally.

// Mutex binding move-out of a call result.
func TestT0841_MutexBindingMoveOutOfCallResultRejected(t *testing.T) {
	errs := ownerErrs(t, `
		type MtxH { Mutex[int]? mtx; drop(~this) {} }
		make_h() MtxH { return MtxH(mtx: Mutex[int](7)); }
		grab() int {
			Mutex[int] m = make_h().mtx!;
			return m.lock().borrow;
		}
		test() {}
	`)
	expectOwnerError(t, errs, "cannot move single-owner handle field")
}

// Task consuming await `<-(make_h().tsk!)` out of a call result.
func TestT0841_TaskAwaitOutOfCallResultRejected(t *testing.T) {
	errs := ownerErrs(t, `
		worker() int { return 5; }
		type TskH { Task[int]? tsk; drop(~this) {} }
		make_h() TskH { return TskH(tsk: go worker()); }
		grab() int { return <-(make_h().tsk!); }
		test() {}
	`)
	expectOwnerError(t, errs, "cannot move single-owner handle field")
}

// Mutex binding move-out of a constructor literal (non-call transient root).
func TestT0841_MutexBindingMoveOutOfConstructorLiteralRejected(t *testing.T) {
	errs := ownerErrs(t, `
		type MtxH { Mutex[int]? mtx; drop(~this) {} }
		grab() int {
			Mutex[int] m = MtxH(mtx: Mutex[int](7)).mtx!;
			return m.lock().borrow;
		}
		test() {}
	`)
	expectOwnerError(t, errs, "cannot move single-owner handle field")
}

// Chained `make_outer().inner.mtx!` — chained member on a call result.
func TestT0841_ChainedMoveOutOfCallResultRejected(t *testing.T) {
	errs := ownerErrs(t, `
		type Inner { Mutex[int]? mtx; drop(~this) {} }
		type Outer { Inner inner; drop(~this) {} }
		make_outer() Outer { return Outer(inner: Inner(mtx: Mutex[int](7))); }
		grab() int {
			Mutex[int] m = make_outer().inner.mtx!;
			return m.lock().borrow;
		}
		test() {}
	`)
	expectOwnerError(t, errs, "cannot move single-owner handle field")
}

// Acceptance: a getter that *returns* a freshly-constructed handle on a call
// result is not a field move — the getter produces an owned value, so it is safe.
func TestT0841_GetterReturningHandleOutOfCallResultAllowed(t *testing.T) {
	ownerOK(t, `
		type MtxH {
			drop(~this) {}
			get fresh_mtx Mutex[int]? { return Mutex[int](9); }
		}
		make_h() MtxH { return MtxH(); }
		grab() int {
			Mutex[int] m = make_h().fresh_mtx!;
			return m.lock().borrow;
		}
		test() {}
	`)
}

// Acceptance: the recommended remedy — bind the transient to an owned local first,
// then move the field out of the owned local.
func TestT0841_TransientBoundToLocalThenMovedAllowed(t *testing.T) {
	ownerOK(t, `
		type MtxH { Mutex[int]? mtx; drop(~this) {} }
		make_h() MtxH { return MtxH(mtx: Mutex[int](7)); }
		grab() int {
			h := make_h();
			Mutex[int] m = h.mtx!;
			return m.lock().borrow;
		}
		test() {}
	`)
}

// Index hop through a *variable*-rooted owned container (`cs[0].tsk!`) is owned
// storage governed by `cs`, not a transient — the IndexExpr hop in
// memberChainRoot walks through to the variable root, so the unconditional
// transient reject does NOT fire (ownership accepts it).
//
// Accepting this is also runtime-safe (T0843): the T0638 genReceiveTask slot-null
// does not reach through the OptionalUnwrap+IndexExpr to the owned element's
// optional slot, but neutralizeMemberOptionalField now clears that optional present
// flag for the `<-(cs[0].tsk!)` shape, so the container's element drop no longer
// double-frees the consumed G (the plain non-optional `<-cs[0].t` was already safe
// via the slot-null). This test pins the ownership-pass decision (accepted, routed
// through the IndexExpr→variable-root branch); the runtime no-double-free
// counterparts live in task_drop_test.pr (task_recv_array_struct_optional_field /
// task_recv_vector_struct_optional_field).
func TestT0841_IndexHopThroughOwnedContainerTaskAwaitAcceptedT0843(t *testing.T) {
	ownerOK(t, `
		worker() int { return 5; }
		type TskH { Task[int]? tsk; drop(~this) {} }
		grab() int {
			cs := [TskH(tsk: go worker())];
			return <-(cs[0].tsk!);
		}
		test() {}
	`)
}

// Index hop through a *call-result* container (`make_vec()[0].tsk!`) — the
// IndexExpr hop bottoms out on a CallExpr, so memberChainRoot returns a
// non-variable (transient) root and the unconditional transient reject fires.
// The temporary vector is dropped at end of expression while the await also
// consumes the G → double-free, so it must be rejected. (Exercises the IndexExpr
// branch walking through to a transient call-result root.)
func TestT0841_IndexHopThroughCallResultTaskAwaitRejected(t *testing.T) {
	errs := ownerErrs(t, `
		worker() int { return 5; }
		type TskH { Task[int]? tsk; drop(~this) {} }
		make_vec() TskH[] { return [TskH(tsk: go worker())]; }
		grab() int { return <-(make_vec()[0].tsk!); }
		test() {}
	`)
	expectOwnerError(t, errs, "cannot move single-owner handle field")
}

// === T0842: field move out of an OWNED container element double-frees ===
//
// Moving a droppable, non-auto-dup field out of an element of an *owned*
// variable-rooted container (`cs[0].m`) flows through tryMove →
// checkFieldMoveOwnership — the same path that already rejects the owned-local
// analog `c.m`. The only gap was isValueTarget peeling MemberExpr but not
// IndexExpr, so a container-element target bottomed out on the IndexExpr and
// returned false (not a value target) → no reject → the moved handle aliased
// the element slot and the container's element drop double-freed it. Peeling
// through IndexExpr closes the gap.

// Non-optional Mutex field moved out of an owned fixed array element.
func TestT0842_NonOptionalMutexFieldMoveOutOfOwnedArrayRejected(t *testing.T) {
	errs := ownerErrs(t, `
		type MtxCell { Mutex[int] m; drop(~this) {} }
		test() {
			MtxCell[2] cs = [MtxCell(m: Mutex[int](7)), MtxCell(m: Mutex[int](8))];
			Mutex[int] a = cs[0].m;
		}
	`)
	expectOwnerError(t, errs, "cannot move field 'm'")
}

// Non-optional Mutex field moved out of an owned vector element.
func TestT0842_NonOptionalMutexFieldMoveOutOfOwnedVectorRejected(t *testing.T) {
	errs := ownerErrs(t, `
		type MtxCell { Mutex[int] m; drop(~this) {} }
		test() {
			MtxCell[] cs = [MtxCell(m: Mutex[int](7))];
			Mutex[int] a = cs[0].m;
		}
	`)
	expectOwnerError(t, errs, "cannot move field 'm'")
}

// The same gap closed for a plain heap-user-type field (not a native handle):
// `cs[0].b` out of an owned array element also double-freed before the fix.
func TestT0842_HeapUserFieldMoveOutOfOwnedContainerRejected(t *testing.T) {
	errs := ownerErrs(t, `
		type Box { int n; drop(~this) {} }
		type Cell { Box b; drop(~this) {} }
		test() {
			Cell[2] cs = [Cell(b: Box(n: 1)), Cell(b: Box(n: 2))];
			Box x = cs[0].b;
		}
	`)
	expectOwnerError(t, errs, "cannot move field 'b'")
}

// Nested field move through a mixed member/index chain (`cs[0].inner.m`):
// isValueTarget peels MemberExpr -> MemberExpr -> IndexExpr -> ident, so a
// handle two members deep inside an owned array element is rejected too. Pins
// that the peel loop iterates through more than a single level (the array/vector
// repros above each have just one MemberExpr directly over the IndexExpr).
func TestT0842_NestedMutexFieldMoveThroughMemberIndexChainRejected(t *testing.T) {
	errs := ownerErrs(t, `
		type Inner { Mutex[int] m; drop(~this) {} }
		type Outer { Inner inner; drop(~this) {} }
		test() {
			Outer[2] cs = [Outer(inner: Inner(m: Mutex[int](7))), Outer(inner: Inner(m: Mutex[int](8)))];
			Mutex[int] a = cs[0].inner.m;
		}
	`)
	expectOwnerError(t, errs, "cannot move field 'm'")
}

// Guard: an auto-dup field (Vector) moved out of an owned container element is
// still ACCEPTED — the isAutoDupType escape in checkFieldMoveOwnership runs
// before the reject, so the read is a copy, not a move. Pins that the IndexExpr
// peeling does not over-reject auto-dup fields.
func TestT0842_AutoDupFieldMoveOutOfOwnedContainerAccepted(t *testing.T) {
	ownerOK(t, `
		type Cell { int[] v; }
		test() {
			Cell[2] cs = [Cell(v: [1, 2]), Cell(v: [3, 4])];
			int[] x = cs[0].v;
		}
	`)
}

// Guard: the same field move out of a PAREN-WRAPPED owned local (`(c).m`) is
// rejected too — isValueTarget peels ParenExpr to reach the variable root, so a
// paren wrapper can't smuggle past the B0341 field-move reject.
func TestT0842_MutexFieldMoveOutOfParenWrappedOwnedLocalRejected(t *testing.T) {
	errs := ownerErrs(t, `
		type MtxCell { Mutex[int] m; drop(~this) {} }
		test() {
			c := MtxCell(m: Mutex[int](7));
			Mutex[int] a = (c).m;
		}
	`)
	expectOwnerError(t, errs, "cannot move field 'm'")
}

// === T0754: RTTI cast into an owning slot consumes the subject ===
//
// An `x as!/as T` flowing into an owning slot (field / element / constructor
// arg) must move-consume its subject — owning slots have no per-binding drop
// flag, so without consumption the cast wrapper aliases the subject's
// instance and both scopes double-free. Ownership now propagates the same
// rejects through the cast wrapper that the plain `= x` assignment already
// triggers.

// Borrowed param subject — must reject with the standard `~` affordance.
func TestT0754_CastIntoFieldFromBorrowedParamRejected(t *testing.T) {
	errs := ownerErrs(t, `
		type Shape { string name; area(this) f64 `+"`abstract"+`; }
		type Circle is Shape {
			f64 radius;
			area(this) f64 { return this.radius; }
		}
		type Holder { Shape held; }
		helper(Shape s) {
			h := Holder(held: Circle(name: "init", radius: 0.0));
			h.held = s as! Circle;
		}
		test() {
			Shape s = Circle(name: "src", radius: 2.0);
			helper(s);
		}
	`)
	expectOwnerError(t, errs, "cannot move borrowed parameter 's'")
}

// MemberExpr cast subject from a droppable owner — must reject via the
// existing B0341 field-move check the plain `= outer.s` would hit.
func TestT0754_CastIntoFieldFromMemberRejected(t *testing.T) {
	errs := ownerErrs(t, `
		type Shape { string name; area(this) f64 `+"`abstract"+`; }
		type Circle is Shape {
			f64 radius;
			area(this) f64 { return this.radius; }
		}
		type Holder { Shape held; }
		type Outer { Shape s; }
		test() {
			o := Outer(s: Circle(name: "src", radius: 2.0));
			h := Holder(held: Circle(name: "init", radius: 0.0));
			h.held = o.s as! Circle;
		}
	`)
	expectOwnerError(t, errs, "cannot move field 's' out of")
}

// Owned-local cast subject — accepted, but the subject becomes Moved so a
// subsequent use errors. Confirms the ownership pass marks the subject Moved
// at the owning-slot store.
func TestT0754_CastIntoFieldFromOwnedLocalMarksMoved(t *testing.T) {
	errs := ownerErrs(t, `
		type Shape { string name; area(this) f64 `+"`abstract"+`; }
		type Circle is Shape {
			f64 radius;
			area(this) f64 { return this.radius; }
		}
		type Holder { Shape held; }
		test() {
			Shape s = Circle(name: "src", radius: 2.0);
			h := Holder(held: Circle(name: "init", radius: 0.0));
			h.held = s as! Circle;
			Shape t = s;
		}
	`)
	expectOwnerError(t, errs, "use of moved variable 's'")
}

// Element-target (IndexExpr) shape — same propagation through the cast at
// the v[i] LHS owning slot.
func TestT0754_CastIntoElementFromBorrowedParamRejected(t *testing.T) {
	errs := ownerErrs(t, `
		type Shape { string name; area(this) f64 `+"`abstract"+`; }
		type Circle is Shape {
			f64 radius;
			area(this) f64 { return this.radius; }
		}
		helper(Shape s) {
			Shape[] v = [];
			v.push(Circle(name: "init", radius: 0.0));
			v[0] = s as! Circle;
		}
		test() {
			Shape s = Circle(name: "src", radius: 2.0);
			helper(s);
		}
	`)
	expectOwnerError(t, errs, "cannot move borrowed parameter 's'")
}

// Constructor-arg shape — `Holder(held: s as! Circle)` likewise consumes.
func TestT0754_CastIntoCtorArgFromBorrowedParamRejected(t *testing.T) {
	errs := ownerErrs(t, `
		type Shape { string name; area(this) f64 `+"`abstract"+`; }
		type Circle is Shape {
			f64 radius;
			area(this) f64 { return this.radius; }
		}
		type Holder { Shape held; }
		helper(Shape s) Holder {
			return Holder(held: s as! Circle);
		}
		test() {
			Shape s = Circle(name: "src", radius: 2.0);
			h := helper(s);
			_ = h;
		}
	`)
	expectOwnerError(t, errs, "cannot move borrowed parameter 's'")
}

// `~`-param call-arg shape — the explicit `~Circle` consume param triggers the
// sig != nil + RefMut branch (ownership/expr.go:679). The plain `consume(s)`
// already errors with the same diagnostic; the cast wrapper must not bypass it.
func TestT0754_CastIntoTildeParamFromBorrowedParamRejected(t *testing.T) {
	errs := ownerErrs(t, `
		type Shape { string name; area(this) f64 `+"`abstract"+`; }
		type Circle is Shape {
			f64 radius;
			area(this) f64 { return this.radius; }
		}
		consume_it(Circle move c) {
			_ = c;
		}
		helper(Shape s) {
			consume_it(s as! Circle);
		}
		test() {
			Shape s = Circle(name: "src", radius: 2.0);
			helper(s);
		}
	`)
	expectOwnerError(t, errs, "cannot move borrowed parameter 's'")
}

// Enum-variant constructor shape — the variant-constructor branch
// (ownership/expr.go:597) consumes args. The cast wrapper must not bypass it
// or the borrowed instance is silently stored in the variant payload.
func TestT0754_CastIntoEnumVariantFromBorrowedParamRejected(t *testing.T) {
	errs := ownerErrs(t, `
		type Shape { string name; area(this) f64 `+"`abstract"+`; }
		type Circle is Shape {
			f64 radius;
			area(this) f64 { return this.radius; }
		}
		enum Wrap { Hold(Circle c) }
		helper(Shape s) Wrap {
			return Wrap.Hold(s as! Circle);
		}
		test() {
			Shape s = Circle(name: "src", radius: 2.0);
			w := helper(s);
			_ = w;
		}
	`)
	expectOwnerError(t, errs, "cannot move borrowed parameter 's'")
}

// Inner-paren-peel coverage — a paren-wrapped cast subject `(s) as! Circle`
// exercises the second peel loop inside tryMoveConsumeCastSubject. The
// behavior must match the unwrapped form.
func TestT0754_CastIntoFieldFromBorrowedParamRejectedParenSubject(t *testing.T) {
	errs := ownerErrs(t, `
		type Shape { string name; area(this) f64 `+"`abstract"+`; }
		type Circle is Shape {
			f64 radius;
			area(this) f64 { return this.radius; }
		}
		type Holder { Shape held; }
		helper(Shape s) {
			h := Holder(held: Circle(name: "init", radius: 0.0));
			h.held = (s) as! Circle;
		}
		test() {
			Shape s = Circle(name: "src", radius: 2.0);
			helper(s);
		}
	`)
	expectOwnerError(t, errs, "cannot move borrowed parameter 's'")
}

// Outer-paren-peel coverage — `h.held = (s as! Circle)` wraps the whole cast
// in parens, exercising the first peel loop (outer ParenExpr removal).
func TestT0754_CastIntoFieldFromBorrowedParamRejectedParenCast(t *testing.T) {
	errs := ownerErrs(t, `
		type Shape { string name; area(this) f64 `+"`abstract"+`; }
		type Circle is Shape {
			f64 radius;
			area(this) f64 { return this.radius; }
		}
		type Holder { Shape held; }
		helper(Shape s) {
			h := Holder(held: Circle(name: "init", radius: 0.0));
			h.held = (s as! Circle);
		}
		test() {
			Shape s = Circle(name: "src", radius: 2.0);
			helper(s);
		}
	`)
	expectOwnerError(t, errs, "cannot move borrowed parameter 's'")
}

// === T0784: tryMoveConsumeCastSubject for raise/yield/yield-delegate/select-send/
// tuple-lit/array-lit/map-lit owning-slot stores ===
// Each site already rejects the plain `<stmt> s` form for a borrowed param via
// T0349's tryMoveConsume call. T0784 adds tryMoveConsumeCastSubject so the
// cast-wrapper `<stmt> s as! T` does not silently bypass the move-consume.

// TupleLit element with cast wrapper — borrowed param must be rejected.
func TestT0784_TupleLitElementCastFromBorrowedParamRejected(t *testing.T) {
	errs := ownerErrs(t, `
		type Shape { string name; area(this) f64 `+"`abstract"+`; }
		type Circle is Shape {
			f64 radius;
			area(this) f64 { return this.radius; }
		}
		helper(Shape s) (int, Circle) {
			return (1, s as! Circle);
		}
		test() {
			Shape s = Circle(name: "src", radius: 2.0);
			_ = helper(s);
		}
	`)
	expectOwnerError(t, errs, "cannot move borrowed parameter 's'")
}

// ArrayLit element with cast wrapper — borrowed param must be rejected.
func TestT0784_ArrayLitElementCastFromBorrowedParamRejected(t *testing.T) {
	errs := ownerErrs(t, `
		type Shape { string name; area(this) f64 `+"`abstract"+`; }
		type Circle is Shape {
			f64 radius;
			area(this) f64 { return this.radius; }
		}
		helper(Shape s) Circle[] {
			return [s as! Circle];
		}
		test() {
			Shape s = Circle(name: "src", radius: 2.0);
			_ = helper(s);
		}
	`)
	expectOwnerError(t, errs, "cannot move borrowed parameter 's'")
}

// MapLit value with cast wrapper — borrowed param must be rejected.
func TestT0784_MapLitValueCastFromBorrowedParamRejected(t *testing.T) {
	errs := ownerErrs(t, `
		type Shape { string name; area(this) f64 `+"`abstract"+`; }
		type Circle is Shape {
			f64 radius;
			area(this) f64 { return this.radius; }
		}
		helper(Shape s) map[string, Circle] {
			return {"k": s as! Circle};
		}
		test() {
			Shape s = Circle(name: "src", radius: 2.0);
			_ = helper(s);
		}
	`)
	expectOwnerError(t, errs, "cannot move borrowed parameter 's'")
}

// raise with cast wrapper — borrowed param must be rejected.
func TestT0784_RaiseCastFromBorrowedParamRejected(t *testing.T) {
	errs := ownerErrs(t, `
		type MyError is error {}
		type SpecialError is MyError { int code; }
		forward!(MyError e) int {
			raise e as! SpecialError;
		}
		test() {}
	`)
	expectOwnerError(t, errs, "cannot move borrowed parameter 'e'")
}

// yield with cast wrapper — borrowed param must be rejected.
func TestT0784_YieldCastFromBorrowedParamRejected(t *testing.T) {
	errs := ownerErrs(t, `
		type Shape { string name; area(this) f64 `+"`abstract"+`; }
		type Circle is Shape {
			f64 radius;
			area(this) f64 { return this.radius; }
		}
		gen(Shape s) stream[Circle] {
			yield s as! Circle;
		}
		test() {}
	`)
	expectOwnerError(t, errs, "cannot move borrowed parameter 's'")
}

// select-send with cast wrapper — borrowed param must be rejected.
func TestT0784_SelectSendCastFromBorrowedParamRejected(t *testing.T) {
	errs := ownerErrs(t, `
		type Shape { string name; area(this) f64 `+"`abstract"+`; }
		type Circle is Shape {
			f64 radius;
			area(this) f64 { return this.radius; }
		}
		helper(channel[Circle] ch, Shape s) {
			select {
				ch.send(s as! Circle):
				default:
			}
		}
		test() {}
	`)
	expectOwnerError(t, errs, "cannot move borrowed parameter 's'")
}

// Owned-local cast subject for yield — accepted, but Moved → subsequent use
// errors.
// (No analogous test for raise: control flow terminates at `raise`, so a use
// after the move is unreachable-code, not an observable move error. The
// borrowed-param-rejection test above already proves the cast subject
// reaches tryMoveConsume at the raise site.)
func TestT0784_YieldCastFromOwnedLocalMarksMoved(t *testing.T) {
	errs := ownerErrs(t, `
		type Shape { string name; area(this) f64 `+"`abstract"+`; }
		type Circle is Shape {
			f64 radius;
			area(this) f64 { return this.radius; }
		}
		gen() stream[Circle] {
			Shape s = Circle(name: "src", radius: 2.0);
			yield s as! Circle;
			_ = s;
		}
		test() {}
	`)
	expectOwnerError(t, errs, "use of moved variable 's'")
}

// === T0783: returning an RTTI cast of an owned local moves the subject ===
// `return s as! Circle` aliases s's instance into the returned value; ownership
// now calls tryMoveCastSubject at ReturnStmt so the subject is moved (mirroring
// T0754/T0784). The move itself is not separately observable via a later use
// (a return terminates flow), so these are ownerOK guards: the change must NOT
// over-reject the valid owned-param / owned-local / chained / borrow-typed
// return-cast shapes. The double-free regression is guarded by the codegen
// drop-flag test (TestT0783_ReturnCastClearsSubjectDropFlag) and the e2e
// casting_test.pr suite.

// Owned `~`-param return-cast (the repro) must type-check cleanly — the new
// tryMoveCastSubject must not introduce a spurious error.
func TestT0783_ReturnCastFromOwnedParamOK(t *testing.T) {
	ownerOK(t, `
		type Shape { string name; area(this) f64 `+"`abstract"+`; }
		type Circle is Shape {
			f64 radius;
			area(this) f64 { return this.radius; }
		}
		helper(Shape move s) Circle { return s as! Circle; }
		test() {}
	`)
}

// Owned-local return-cast must type-check cleanly.
func TestT0783_ReturnCastFromOwnedLocalOK(t *testing.T) {
	ownerOK(t, `
		type Shape { string name; area(this) f64 `+"`abstract"+`; }
		type Circle is Shape {
			f64 radius;
			area(this) f64 { return this.radius; }
		}
		helper() Circle {
			Shape s = Circle(name: "src", radius: 2.0);
			return s as! Circle;
		}
		test() {}
	`)
}

// Chained return-cast (T0800 sibling on the return path) must type-check cleanly:
// tryMoveCastSubject recurses through the nested CastExpr to the innermost
// subject without rejecting it.
func TestT0783_ReturnChainedCastOK(t *testing.T) {
	ownerOK(t, `
		type Shape { string name; area(this) f64 `+"`abstract"+`; }
		type Circle is Shape {
			f64 radius;
			area(this) f64 { return this.radius; }
		}
		helper(Shape move s) Circle { return (s as! Circle) as! Circle; }
		test() {}
	`)
}

// Borrow-typed return-cast must NOT be rejected — this pins the deliberate
// choice of tryMove over tryMoveConsume at the return. A borrowed parameter
// returned as a cast under a borrow-typed result is valid; tryMove short-circuits
// on Borrowed state, whereas tryMoveConsumeCastSubject would wrongly reject it
// ("cannot move out of '.borrow' getter" / "cannot move borrowed parameter").
func TestT0783_ReturnCastBorrowedParamNotRejected(t *testing.T) {
	ownerOK(t, `
		type Shape { string name; area(this) f64 `+"`abstract"+`; }
		type Circle is Shape {
			f64 radius;
			area(this) f64 { return this.radius; }
		}
		helper(Shape s) Circle & { return s as! Circle &; }
		test() {}
	`)
}

// Paren-wrapped return-cast (`return (s as! Circle);`) must type-check cleanly.
// This pins tryMoveCastSubject's *outer* ParenExpr peel loop: the whole return
// value is a ParenExpr wrapping the CastExpr, so the cast is only reached after
// stripping the leading paren(s). (The chained test exercises the *inner* paren
// peel — between two casts — but never the outer one.)
func TestT0783_ReturnParenWrappedCastOK(t *testing.T) {
	ownerOK(t, `
		type Shape { string name; area(this) f64 `+"`abstract"+`; }
		type Circle is Shape {
			f64 radius;
			area(this) f64 { return this.radius; }
		}
		helper(Shape move s) Circle { return (s as! Circle); }
		test() {}
	`)
}

// Optional `as` return-cast (`return s as Circle;`, result `Circle?`) must
// type-check cleanly AND must NOT move the subject at ownership level — it is a
// *conditional* move (None on a failed downcast). This pins tryMoveCastSubject's
// `!cast.Force` early return: the subject stays Owned so its scope-exit drop is
// preserved (clearing it would leak on the failure path). Tracked separately as
// T0849 (runtime-outcome-conditioned drop).
func TestT0783_ReturnOptionalCastOK(t *testing.T) {
	ownerOK(t, `
		type Shape { string name; area(this) f64 `+"`abstract"+`; }
		type Circle is Shape {
			f64 radius;
			area(this) f64 { return this.radius; }
		}
		helper(Shape move s) Circle? { return s as Circle; }
		test() {}
	`)
}

// MapLit *key* with cast wrapper — borrowed param must be rejected. The
// MapLit-value test above exercises the second of the two adjacent
// tryMoveConsumeCastSubject calls; this one pins the first (key) branch so
// a future refactor that drops the Key call doesn't regress silently.
// Uses a Hashable + Equal hierarchy so the map's K constraint is satisfied.
func TestT0784_MapLitKeyCastFromBorrowedParamRejected(t *testing.T) {
	errs := ownerErrs(t, `
		type Shape is Equal {
			int id;
			get hash int { return this.id; }
			== (Self other) bool { return this.id == other.id; }
		}
		type Circle is Shape { f64 radius; }
		helper(Shape s) map[Circle, int] {
			return {s as! Circle: 1};
		}
		test() {
			Shape s = Circle(id: 1, radius: 2.0);
			_ = helper(s);
		}
	`)
	expectOwnerError(t, errs, "cannot move borrowed parameter 's'")
}

// Paren-wrapped cast subject at a T0784 site — regression guard for the
// peel in tryMoveConsumeCastSubject (mirrors T0754's paren tests for the
// field-init site). Uses raise as the representative site; the helper
// is shared across all T0784 sites.
func TestT0784_RaiseParenWrappedCastFromBorrowedParamRejected(t *testing.T) {
	errs := ownerErrs(t, `
		type MyError is error {}
		type SpecialError is MyError { int code; }
		forward!(MyError e) int {
			raise (e as! SpecialError);
		}
		test() {}
	`)
	expectOwnerError(t, errs, "cannot move borrowed parameter 'e'")
}

// === T0800: chained RTTI cast (`(x as! A) as! B`) is a view-of-a-view ===
//
// tryMoveConsumeCastSubject recurses through nested CastExpr to the innermost
// subject. Without the recursion the outer cast's subject is itself a CastExpr,
// tryMoveConsume on which is a no-op — so a chained cast over a borrowed param
// into an owning slot would silently pass (and double-free at runtime). These
// mirror the single-layer T0754 tests with one extra cast layer.

// Chained cast over a borrowed param into a field slot — still rejected: the
// recursion must reach the innermost subject `s`.
func TestT0800_ChainedCastIntoFieldFromBorrowedParamRejected(t *testing.T) {
	errs := ownerErrs(t, `
		type Shape { string name; area(this) f64 `+"`abstract"+`; }
		type Circle is Shape {
			f64 radius;
			area(this) f64 { return this.radius; }
		}
		type Holder { Shape held; }
		helper(Shape s) {
			h := Holder(held: Circle(name: "init", radius: 0.0));
			h.held = (s as! Circle) as! Circle;
		}
		test() {
			Shape s = Circle(name: "src", radius: 2.0);
			helper(s);
		}
	`)
	expectOwnerError(t, errs, "cannot move borrowed parameter 's'")
}

// Chained cast over an owned local into a field slot — accepted, but the
// innermost subject `s` becomes Moved, so a later use errors. Confirms the
// recursion consumes the innermost subject (not the inner CastExpr).
func TestT0800_ChainedCastIntoFieldFromOwnedLocalMarksMoved(t *testing.T) {
	errs := ownerErrs(t, `
		type Shape { string name; area(this) f64 `+"`abstract"+`; }
		type Circle is Shape {
			f64 radius;
			area(this) f64 { return this.radius; }
		}
		type Holder { Shape held; }
		test() {
			Shape s = Circle(name: "src", radius: 2.0);
			h := Holder(held: Circle(name: "init", radius: 0.0));
			h.held = (s as! Circle) as! Circle;
			Shape t = s;
		}
	`)
	expectOwnerError(t, errs, "use of moved variable 's'")
}

// Chained cast over a borrowed param into a constructor arg — same propagation
// through both cast layers at the ctor-arg owning slot.
func TestT0800_ChainedCastIntoCtorArgFromBorrowedParamRejected(t *testing.T) {
	errs := ownerErrs(t, `
		type Shape { string name; area(this) f64 `+"`abstract"+`; }
		type Circle is Shape {
			f64 radius;
			area(this) f64 { return this.radius; }
		}
		type Holder { Shape held; }
		helper(Shape s) Holder {
			return Holder(held: (s as! Circle) as! Circle);
		}
		test() {
			Shape s = Circle(name: "src", radius: 2.0);
			h := helper(s);
			_ = h;
		}
	`)
	expectOwnerError(t, errs, "cannot move borrowed parameter 's'")
}

// === T0816: closure read out of an owning aggregate is a borrow ===
//
// Reading a closure out of a struct/optional field or container element aliases
// the aggregate's heap env (codegen treats it as a borrow, T0812). The local is
// bound Borrowed, so escaping it (returning) or re-storing it into a
// longer-lived aggregate is rejected, while same-scope read-and-invoke is valid.

func TestT0816ReturnClosureReadFromStructField(t *testing.T) {
	// Repro 1: returning a closure read out of a struct field escapes past the
	// aggregate's lifetime -> reject as borrowed-returned-as-owned.
	errs := ownerErrs(t, `
		type CbHolder { () -> int cb; }
		leak_out(CbHolder h) () -> int {
			f := h.cb;
			return f;
		}
	`)
	expectOwnerError(t, errs, "cannot return a borrowed reference as owned")
}

func TestT0816RestoreClosureIntoAggregate(t *testing.T) {
	// Repro 2: re-storing a borrowed closure into another owning aggregate would
	// double-free the env -> reject the move into the constructor field.
	errs := ownerErrs(t, `
		type CbHolder { () -> int cb; }
		make_cb(int n) CbHolder { return CbHolder(cb: move || -> n); }
		restore_into_aggregate() {
			h := make_cb(5);
			f := h.cb;
			h2 := CbHolder(cb: f);
			_ = h2;
		}
	`)
	expectOwnerError(t, errs, "cannot move borrowed value 'f'")
}

func TestT0816ReturnClosureFromOptionalField(t *testing.T) {
	// Optional closure field force-unwrap variant: `f := h.cb!`.
	errs := ownerErrs(t, `
		type OptHolder { (() -> int)? cb; }
		escape_opt(OptHolder h) () -> int {
			f := h.cb!;
			return f;
		}
	`)
	expectOwnerError(t, errs, "cannot return a borrowed reference as owned")
}

func TestT0816ReturnClosureFromVectorElement(t *testing.T) {
	// Container element variant: `f := v[0]`.
	errs := ownerErrs(t, `
		escape_vec(Vector[() -> int] v) () -> int {
			f := v[0];
			return f;
		}
	`)
	expectOwnerError(t, errs, "cannot return a borrowed reference as owned")
}

func TestT0816ReadAndInvokeInScopeOK(t *testing.T) {
	// Positive: same-scope read-and-invoke is valid (calling a Borrowed closure
	// is not a consume). Guards against over-rejection / T0812 regression.
	ownerOK(t, `
		type CbHolder { () -> int cb; }
		make_cb(int n) CbHolder { return CbHolder(cb: move || -> n); }
		read_invoke() {
			h := make_cb(5);
			f := h.cb;
			r := f();
			_ = r;
		}
	`)
}

func TestT0816GetterReturningClosureStaysOwned(t *testing.T) {
	// Owned-return exclusion: a getter returning a *fresh* closure binds Owned, so
	// returning it must remain accepted (not misclassified as a borrow).
	ownerOK(t, `
		type Factory {
			int n;
			get make () -> int { k := this.n; return move || -> k; }
		}
		use_getter(Factory fa) () -> int {
			f := fa.make;
			return f;
		}
	`)
}

func TestT0816ConsumeSourceWhileBorrowedRejected(t *testing.T) {
	// Source-lifetime escape: consuming the aggregate (`sink(h)` into a `~`
	// param) while the borrowing local is still live frees the heap env out
	// from under it -> UAF. The shared borrow registered on `h` makes this a
	// "cannot move 'h' while it is borrowed" rejection.
	errs := ownerErrs(t, `
		type CbHolder { () -> int cb; }
		make_cb(int n) CbHolder { return CbHolder(cb: move || -> n); }
		sink(CbHolder move h) {}
		probe() {
			h := make_cb(5);
			f := h.cb;
			sink(h);
			r := f();
			_ = r;
		}
	`)
	expectOwnerError(t, errs, "cannot move 'h' while it is borrowed")
}

func TestT0816ReassignSourceWhileBorrowedRejected(t *testing.T) {
	// Reassigning the source aggregate drops the old env while the borrowing
	// local is still live -> UAF. Rejected as "cannot assign to 'h' while it is
	// borrowed".
	errs := ownerErrs(t, `
		type CbHolder { () -> int cb; }
		make_cb(int n) CbHolder { return CbHolder(cb: move || -> n); }
		probe() {
			h := make_cb(5);
			f := h.cb;
			h = make_cb(6);
			r := f();
			_ = r;
		}
	`)
	expectOwnerError(t, errs, "cannot assign to 'h' while it is borrowed")
}

func TestT0816ConsumeSourceVectorWhileBorrowedRejected(t *testing.T) {
	// Container-element variant: consuming the source vector while an element
	// closure-borrow is live is rejected (here `src` is a borrowed param, so the
	// `~`-affordance diagnostic fires).
	errs := ownerErrs(t, `
		sink(Vector[() -> int] move v) {}
		probe(Vector[() -> int] src) {
			f := src[0];
			sink(src);
			r := f();
			_ = r;
		}
	`)
	expectOwnerError(t, errs, "cannot move borrowed parameter 'src'")
}

func TestT0816ConsumeSourceAfterLastUseOK(t *testing.T) {
	// NLL narrowing: consuming the source AFTER the borrowing local's last use
	// is valid — the shared borrow expires at `f`'s last use, not scope exit.
	ownerOK(t, `
		type CbHolder { () -> int cb; }
		make_cb(int n) CbHolder { return CbHolder(cb: move || -> n); }
		sink(CbHolder move h) {}
		probe() {
			h := make_cb(5);
			f := h.cb;
			r := f();
			_ = r;
			sink(move h);
		}
	`)
}

func TestT0816ReturnClosureFromOptionalCastForce(t *testing.T) {
	// Optional closure field unconditional cast `h.cb as! (() -> int)` — the
	// CastExpr.Force peel arm of closureAggregateBorrowSource (distinct from the
	// `!` OptionalUnwrapExpr arm). Returning the borrow escapes -> rejected.
	errs := ownerErrs(t, `
		type OptHolder { (() -> int)? cb; }
		escape_cast(OptHolder h) () -> int {
			f := h.cb as! (() -> int);
			return f;
		}
	`)
	expectOwnerError(t, errs, "cannot return a borrowed reference as owned")
}

func TestT0816ReturnClosureFromTemporaryAggregate(t *testing.T) {
	// Rootless source: reading the closure field off a *temporary* call result
	// (`make_cb(5).cb`) — destructureBorrowRoot yields "" so no source borrow is
	// registered, but the local is still bound Borrowed, so returning it is still
	// rejected as a borrowed-returned-as-owned escape (exercises the root == ""
	// arm of registerClosureAggregateBorrow).
	errs := ownerErrs(t, `
		type CbHolder { () -> int cb; }
		make_cb(int n) CbHolder { return CbHolder(cb: move || -> n); }
		escape_tmp() () -> int {
			f := make_cb(5).cb;
			return f;
		}
	`)
	expectOwnerError(t, errs, "cannot return a borrowed reference as owned")
}

func TestT0816UserIndexReturningClosureStaysOwned(t *testing.T) {
	// Owned-return exclusion via a user-defined non-native `[]` that constructs a
	// *fresh* closure: the local binds Owned (not Borrowed), so returning it is
	// accepted (exercises the isUserIndexExpr arm of
	// closureAggregateBorrowSource). Mirrors the getter exclusion above for the
	// container-element shape.
	ownerOK(t, `
		type CbBox {
			int n;
			[](int i) () -> int { k := this.n + i; return move || -> k; }
		}
		use_idx(CbBox b) () -> int {
			f := b[0];
			return f;
		}
	`)
}

func TestT0895RestoreViaAssignRejected(t *testing.T) {
	// T0895: the assignment-into-a-pre-declared-local path (`f = h.cb`, vs
	// T0816's var-decl `f := h.cb`). Re-storing the borrowed closure into another
	// owning aggregate would double-free the env -> reject the move.
	errs := ownerErrs(t, `
		type CbHolder { () -> int cb; }
		make_cb(int n) CbHolder { return CbHolder(cb: move || -> n); }
		restore_via_assign() {
			h := make_cb(5);
			() -> int f = || -> 0;
			f = h.cb;
			h2 := CbHolder(cb: f);
			_ = h2;
		}
	`)
	expectOwnerError(t, errs, "cannot move borrowed value 'f'")
}

func TestT0895ReturnViaAssignRejected(t *testing.T) {
	// T0895: returning a closure read into a pre-declared local via assignment
	// escapes past the aggregate's lifetime -> borrowed-returned-as-owned.
	errs := ownerErrs(t, `
		type CbHolder { () -> int cb; }
		make_cb(int n) CbHolder { return CbHolder(cb: move || -> n); }
		ret() () -> int {
			h := make_cb(5);
			() -> int f = || -> 0;
			f = h.cb;
			return f;
		}
	`)
	expectOwnerError(t, errs, "cannot return a borrowed reference as owned")
}

func TestT0895ConsumeSourceWhileBorrowedViaAssignRejected(t *testing.T) {
	// T0895: consuming the source aggregate while the local borrowed via
	// assignment is still live frees the env out from under it -> UAF. The shared
	// borrow registered on `h` makes this "cannot move 'h' while it is borrowed".
	errs := ownerErrs(t, `
		type CbHolder { () -> int cb; }
		make_cb(int n) CbHolder { return CbHolder(cb: move || -> n); }
		probe() {
			h := make_cb(5);
			() -> int f = || -> 0;
			f = h.cb;
			h2 := h;
			r := f();
			_ = r;
			_ = h2;
		}
	`)
	expectOwnerError(t, errs, "cannot move 'h' while it is borrowed")
}

func TestT0895ConsumeSourceAfterLastUseViaAssignOK(t *testing.T) {
	// T0895: NLL narrowing for the assignment borrower — consuming the source
	// AFTER the local's last use is valid (the borrow expires at `f`'s last use,
	// not scope exit). Verifies step 2 (lastuse.go AssignStmt case).
	ownerOK(t, `
		type CbHolder { () -> int cb; }
		make_cb(int n) CbHolder { return CbHolder(cb: move || -> n); }
		sink(CbHolder move h) {}
		probe() {
			h := make_cb(5);
			() -> int f = || -> 0;
			f = h.cb;
			r := f();
			_ = r;
			sink(move h);
		}
	`)
}

func TestT0895ContainerElementViaAssign(t *testing.T) {
	// T0895: container-element variant (`f = v[0]`) of the assignment borrow —
	// covers the IndexExpr arm. Returning the borrow escapes -> rejected.
	errs := ownerErrs(t, `
		escape_vec(Vector[() -> int] v) () -> int {
			() -> int f = || -> 0;
			f = v[0];
			return f;
		}
	`)
	expectOwnerError(t, errs, "cannot return a borrowed reference as owned")
}

func TestT0895ReturnClosureFromOptionalFieldViaAssign(t *testing.T) {
	// T0895: optional closure field force-unwrap (`f = h.cb!`) via assignment —
	// exercises the OptionalUnwrapExpr peel arm of closureAggregateBorrowSource on
	// the assignment path (distinct from the struct-field MemberExpr arm). The
	// borrow escapes via return -> rejected.
	errs := ownerErrs(t, `
		type OptHolder { (() -> int)? cb; }
		escape_opt(OptHolder h) () -> int {
			() -> int f = || -> 0;
			f = h.cb!;
			return f;
		}
	`)
	expectOwnerError(t, errs, "cannot return a borrowed reference as owned")
}

func TestT0895ReturnClosureFromOptionalCastForceViaAssign(t *testing.T) {
	// T0895: optional closure field unconditional cast (`f = h.cb as! (() -> int)`)
	// via assignment — exercises the CastExpr.Force peel arm of
	// closureAggregateBorrowSource on the assignment path (distinct from the `!`
	// OptionalUnwrapExpr arm). The borrow escapes via return -> rejected.
	errs := ownerErrs(t, `
		type OptHolder { (() -> int)? cb; }
		escape_cast(OptHolder h) () -> int {
			() -> int f = || -> 0;
			f = h.cb as! (() -> int);
			return f;
		}
	`)
	expectOwnerError(t, errs, "cannot return a borrowed reference as owned")
}

func TestT0895GetterReturningClosureStaysOwnedViaAssign(t *testing.T) {
	// T0895: owned-return exclusion via assignment — a getter returning a *fresh*
	// closure binds Owned even through `f = fa.make`, so escaping it must stay
	// accepted (the getter-nil arm of closureAggregateBorrowSource keeps
	// rhsClosureBorrowSrc nil, so the assign path does NOT over-borrow). Guards the
	// boundary opposite to the borrow-tracked field reads above.
	ownerOK(t, `
		type Factory {
			int n;
			get make () -> int { k := this.n; return move || -> k; }
		}
		use_getter(Factory fa) () -> int {
			() -> int f = || -> 0;
			f = fa.make;
			return f;
		}
	`)
}

func TestT0895UserIndexReturningClosureStaysOwnedViaAssign(t *testing.T) {
	// T0895: owned-return exclusion via assignment for a user-defined non-native
	// `[]` that constructs a *fresh* closure. The isUserIndexExpr arm keeps
	// rhsClosureBorrowSrc nil, so `f = b[2]` binds Owned and escaping it is valid.
	ownerOK(t, `
		type Boxes {
			int unused;
			[](this, int k) () -> int { j := k; return move || -> j; }
		}
		use_index(Boxes b) () -> int {
			() -> int f = || -> 0;
			f = b[2];
			return f;
		}
	`)
}

func TestT0895ReassignSourceWhileBorrowedViaAssign(t *testing.T) {
	// T0895: reassigning the source aggregate while the local borrowed via
	// assignment is still live drops the old env out from under it -> UAF.
	// Rejected as "cannot assign to 'h' while it is borrowed".
	errs := ownerErrs(t, `
		type CbHolder { () -> int cb; }
		make_cb(int n) CbHolder { return CbHolder(cb: move || -> n); }
		probe() {
			h := make_cb(5);
			() -> int f = || -> 0;
			f = h.cb;
			h = make_cb(6);
			r := f();
			_ = r;
		}
	`)
	expectOwnerError(t, errs, "cannot assign to 'h' while it is borrowed")
}

// === T1113: by-value read of a container element transitively nesting a
// single-owner native handle (Task/Mutex/MutexGuard) through a user-type field
// or enum variant field ===
//
// Map's `[]` is a Promise method (non-native) that returns the slot's element by
// value; a single-owner handle in the element is NOT duped on read (no copy
// semantics), so `h := m[k]!` aliases the slot's owned handle → double-free / UAF
// when the read-back copy drops. The original T1109 fix left these as silent
// shallow copies (segfault at runtime); T1113 rejects the by-value read.
// isUserIndexExpr would wrongly exempt Map — indexTargetIsAliasingContainer
// suppresses that exemption for std native containers (Vector/Map).

func TestT1113_MapEnumMutexReadRejected(t *testing.T) {
	errs := ownerErrs(t, `
		enum MHolder { M(Mutex[int] m, int n) }
		test() {
			m := Map[int, MHolder]();
			m[1] = MHolder.M(Mutex[int](5), 2);
			h := m[1]!;
		}
	`)
	expectOwnerError(t, errs, "transitively contains Mutex[int], a single-owner native handle")
}

func TestT1113_MapStructMutexReadRejected(t *testing.T) {
	errs := ownerErrs(t, `
		type SHolder { Mutex[int] m; int n; }
		test() {
			m := Map[int, SHolder]();
			m[1] = SHolder(m: Mutex[int](5), n: 2);
			h := m[1]!;
		}
	`)
	expectOwnerError(t, errs, "transitively contains Mutex[int], a single-owner native handle")
}

func TestT1113_MapEnumTaskReadRejected(t *testing.T) {
	errs := ownerErrs(t, `
		worker_t1113() int { return 42; }
		enum THolder { T(Task[int] t, int n) }
		test() {
			m := Map[int, THolder]();
			m[1] = THolder.T(go worker_t1113(), 2);
			h := m[1]!;
		}
	`)
	expectOwnerError(t, errs, "transitively contains Task[int], a single-owner native handle")
}

func TestT1113_MapStructMutexGuardReadRejected(t *testing.T) {
	errs := ownerErrs(t, `
		type GHolder { MutexGuard[int] g; }
		test(GHolder src) {
			m := Map[int, GHolder]();
			h := m[1]!;
		}
	`)
	expectOwnerError(t, errs, "transitively contains MutexGuard[int], a single-owner native handle")
}

// Typed var-decl form `T x = m[k]!` goes through tryMove as well.
func TestT1113_MapEnumMutexTypedVarRejected(t *testing.T) {
	errs := ownerErrs(t, `
		enum MHolder { M(Mutex[int] m, int n) }
		test() {
			m := Map[int, MHolder]();
			m[1] = MHolder.M(Mutex[int](5), 2);
			MHolder x = m[1]!;
		}
	`)
	expectOwnerError(t, errs, "single-owner native handle")
}

// Consuming function-arg form `f(m[k]!)` (a `move` parameter) goes through
// tryMoveConsume. (A plain borrow parameter is sound — the by-borrow temp is
// not dropped, so it never aliases-then-frees the slot; only consumption is
// unsound, and the handle also cannot be moved out inside the borrow callee.)
func TestT1113_MapEnumMutexConsumingCallArgRejected(t *testing.T) {
	errs := ownerErrs(t, `
		enum MHolder { M(Mutex[int] m, int n) }
		sink(MHolder move h) {}
		test() {
			m := Map[int, MHolder]();
			m[1] = MHolder.M(Mutex[int](5), 2);
			sink(m[1]!);
		}
	`)
	expectOwnerError(t, errs, "single-owner native handle")
}

// Vector element nesting a handle (Vector `[]` is native, no `!` unwrap).
func TestT1113_VectorEnumMutexReadRejected(t *testing.T) {
	errs := ownerErrs(t, `
		enum MHolder { M(Mutex[int] m, int n) }
		test() {
			v := Vector[MHolder]();
			h := v[0];
		}
	`)
	expectOwnerError(t, errs, "transitively contains Mutex[int], a single-owner native handle")
}

// Fixed-size array element nesting a handle.
func TestT1113_ArrayEnumMutexReadRejected(t *testing.T) {
	errs := ownerErrs(t, `
		enum MHolder { M(Mutex[int] m, int n) }
		test(MHolder a, MHolder b) {
			MHolder[2] arr = [a, b];
			h := arr[0];
		}
	`)
	expectOwnerError(t, errs, "transitively contains Mutex[int], a single-owner native handle")
}

// --- T1113 positive regression guards: refcounted/duplicable nesting and
// handle-free elements must still compile. ---
//
// These assert only that the OWNERSHIP PASS allows the read — they are not a
// runtime-soundness guarantee. Allowing refcounted nesting is correct in
// principle (Ref/Arc dup is a strong-count increment, not a shallow alias), but
// the by-value BIND form `h := m[k]!` of a Ref/Arc-bearing value is currently
// miscompiled (the bind read does not increment the strong count → UAF on the
// next read; the struct-field form segfaults on first access). That is the
// separate pre-existing codegen bug T1117 — these ownerOK tests stay valid
// across its fix; the runtime regressions live in the e2e suite (match/inline
// forms today, bind forms once T1117 lands).

// Ref[Mutex] in an enum: Ref dup is a refcount increment (sound at the ownership
// level; runtime bind-read codegen tracked as T1117). The read is allowed —
// FirstFieldNestedSingleOwnerHandle treats Ref as opaque → nil.
func TestT1113_MapEnumRefMutexReadAllowed(t *testing.T) {
	ownerOK(t, `
		enum RHolder { R(Ref[Mutex[int]] r, int n) }
		test() {
			m := Map[int, RHolder]();
			m[1] = RHolder.R(Ref[Mutex[int]](Mutex[int](5)), 2);
			h := m[1]!;
		}
	`)
}

// Ref[Mutex] directly as a Map value — sound (refcount dup).
func TestT1113_MapRefMutexValueReadAllowed(t *testing.T) {
	ownerOK(t, `
		test() {
			m := Map[int, Ref[Mutex[int]]]();
			m[1] = Ref[Mutex[int]](Mutex[int](5));
			h := m[1]!;
		}
	`)
}

// Channel in a struct: Channel dup is a refcount increment (sound).
func TestT1113_MapStructChannelReadAllowed(t *testing.T) {
	ownerOK(t, `
		type CHolder { Channel[int] c; int n; }
		test() {
			m := Map[int, CHolder]();
			m[1] = CHolder(c: Channel[int](), n: 2);
			h := m[1]!;
		}
	`)
}

// Handle-free enum read — no false positive.
func TestT1113_MapPlainEnumReadAllowed(t *testing.T) {
	ownerOK(t, `
		enum PHolder { P(int a, string b) }
		test() {
			m := Map[int, PHolder]();
			m[1] = PHolder.P(1, "x");
			h := m[1]!;
		}
	`)
}

// Handle-free struct read — no false positive.
func TestT1113_MapPlainStructReadAllowed(t *testing.T) {
	ownerOK(t, `
		type QHolder { int a; string b; }
		test() {
			m := Map[int, QHolder]();
			m[1] = QHolder(a: 1, b: "x");
			h := m[1]!;
		}
	`)
}

// A user-defined non-native `[]` returning a FRESH value that transitively
// nests a single-owner handle stays allowed. This is the T0650 exemption
// crossing the new T1113 nested-handle gate: the index target is a plain user
// type (not a Vector/Map), so indexTargetIsAliasingContainer is false and the
// isUserIndexExpr exemption survives — there is no container slot to alias (the
// `[]` constructs a fresh MHolder owning its own Mutex). Contrast the rejected
// Vector/Map cases above, where the read aliases internal storage. Guards
// against a future FirstFieldNestedSingleOwnerHandle change wrongly rejecting
// fresh-returning user operators.
func TestT1113_UserIndexFreshEnumHandleReturnAllowed(t *testing.T) {
	ownerOK(t, `
		enum MHolder { M(Mutex[int] m, int n) }
		type Factory { int seed; [](int i) MHolder { return MHolder.M(Mutex[int](this.seed + i), i); } }
		test() {
			f := Factory(seed: 10);
			h := f[5];
		}
	`)
}

// --- T1113 detection-path coverage: the gate must see a handle reached through
// generic-instance substitution, tuples, nested arrays, and recursive types, not
// only the simple non-generic struct/enum field. Each exercises a distinct branch
// of firstFieldNestedSingleOwnerHandle. ---

// Generic USER enum where the handle is reached ONLY via type-arg substitution
// (variant field type is the type param `T`, instantiated to Mutex[int]). Hits
// the Instance->Enum branch with BuildSubstMap/Substitute — distinct from the
// non-generic enum (*types.Enum) branch the earlier tests cover.
func TestT1113_MapGenericEnumHandleViaTypeParamRejected(t *testing.T) {
	errs := ownerErrs(t, `
		enum Holder[T] { M(T v, int n) }
		test() {
			m := Map[int, Holder[Mutex[int]]]();
			h := m[1]!;
		}
	`)
	expectOwnerError(t, errs, "transitively contains Mutex[int], a single-owner native handle")
}

// Generic USER struct, handle reached via type-arg substitution (field type is
// the type param). Hits the Instance->Named branch with Substitute — distinct
// from the non-generic struct (*types.Named) branch covered earlier.
func TestT1113_MapGenericStructHandleViaTypeParamRejected(t *testing.T) {
	errs := ownerErrs(t, `
		type Box[T] { T v; int n; }
		test() {
			m := Map[int, Box[Mutex[int]]]();
			h := m[1]!;
		}
	`)
	expectOwnerError(t, errs, "transitively contains Mutex[int], a single-owner native handle")
}

// Tuple element nesting a handle: the map-read result `(Mutex[int], int)?`
// unwraps Optional -> Tuple -> handle. Hits the *types.Tuple branch.
func TestT1113_MapTupleHandleReadRejected(t *testing.T) {
	errs := ownerErrs(t, `
		test() {
			m := Map[int, (Mutex[int], int)]();
			h := m[1]!;
		}
	`)
	expectOwnerError(t, errs, "transitively contains Mutex[int], a single-owner native handle")
}

// Handle reached through a fixed-size ARRAY field inside a struct element. Hits
// the *types.Array branch (recurse element type), reached from the Named field
// walk — distinct from indexing an array directly (covered earlier), where the
// result is already the element type.
func TestT1113_MapArrayFieldHandleReadRejected(t *testing.T) {
	errs := ownerErrs(t, `
		enum MHolder { M(Mutex[int] m, int n) }
		type AHolder { MHolder[2] arr; }
		test() {
			m := Map[int, AHolder]();
			h := m[1]!;
		}
	`)
	expectOwnerError(t, errs, "transitively contains Mutex[int], a single-owner native handle")
}

// Self-referential (recursive) struct: the field walk reaches `Node` again
// through `Node? next` before finding the Mutex. The `seen` cycle guard must stop
// the recursion (else infinite loop) and the walk must still find the sibling
// Mutex field. Exercises the seen[t]==true early-return.
func TestT1113_MapRecursiveStructHandleReadRejected(t *testing.T) {
	errs := ownerErrs(t, `
		type Node { Node? next; Mutex[int] m; }
		test() {
			m := Map[int, Node]();
			h := m[1]!;
		}
	`)
	expectOwnerError(t, errs, "transitively contains Mutex[int], a single-owner native handle")
}

// Diamond/shared type: the same handle-free enum `E` is referenced by two
// fields of the struct element. The first walk records E in `seen`; the second
// must short-circuit on seen[E] (else redundant re-walk) and the walk must still
// reach the sibling Mutex. Exercises the *types.Enum seen-guard early return.
func TestT1113_MapSharedEnumCycleGuardRejected(t *testing.T) {
	errs := ownerErrs(t, `
		enum E { M(int a), N }
		type Two { E first; E second; Mutex[int] m; }
		test() {
			m := Map[int, Two]();
			h := m[1]!;
		}
	`)
	expectOwnerError(t, errs, "transitively contains Mutex[int], a single-owner native handle")
}

// Generic-instance analogue: two fields share the same generic enum origin
// (Holder[int]). The seen guard keys on the origin *types.Enum, so the second
// field short-circuits. Exercises the Instance->Enum seen-guard early return.
func TestT1113_MapSharedGenericEnumCycleGuardRejected(t *testing.T) {
	errs := ownerErrs(t, `
		enum Holder[T] { M(T v), Nil }
		type Two { Holder[int] first; Holder[int] second; Mutex[int] m; }
		test() {
			m := Map[int, Two]();
			h := m[1]!;
		}
	`)
	expectOwnerError(t, errs, "transitively contains Mutex[int], a single-owner native handle")
}

// Positive guard: a std native container reached THROUGH generic instantiation
// stays opaque. `Holder[Mutex[int]]` instantiates the variant field `Ref[T]` to
// `Ref[Mutex[int]]`; the Instance->Named recursion must treat Ref as opaque
// (isStdNativeContainerNamed) and NOT recurse its Mutex type-arg — Ref's dup is
// a refcount increment, so the read is sound and must compile. Guards the
// std-container-opaque short-circuit on the substitution path.
func TestT1113_MapGenericEnumRefHandleViaTypeParamAllowed(t *testing.T) {
	ownerOK(t, `
		enum Holder[T] { M(Ref[T] r, int n) }
		test() {
			m := Map[int, Holder[Mutex[int]]]();
			h := m[1]!;
		}
	`)
}

// === T1134: divergence-aware move analysis in branch merges ===
//
// A move inside a branch that diverges (return/raise/break/continue) must not
// poison the variable on the fall-through path, which is only reached when the
// diverging branch did NOT run. These shapes were false-positive "use of moved
// variable" rejections before the merge was made divergence-aware.

// The reported repro: no-else `if` whose then-branch returns, then a fall-through
// `return s`. Reachable only when cond was false → s still owned.
func TestT1134_IfReturnNoElseFallThrough(t *testing.T) {
	ownerOK(t, `
		f(bool cond) string {
			string s = "x";
			if cond { return s; }
			return s;
		}
		test() {}
	`)
}

// `raise` is also a divergent terminator.
func TestT1134_IfRaiseNoElseFallThrough(t *testing.T) {
	ownerOK(t, `
		type MyError is error {
			new(~this) {}
		}
		consume(string move s) {}
		f!(bool cond) string {
			string s = "x";
			if cond { consume(move s); raise MyError(); }
			return s;
		}
		test() {}
	`)
}

// Diverging loop body: a for-in whose body always returns. The move inside
// never reaches the post-loop path (only the zero-iteration path does).
func TestT1134_DivergingLoopBody(t *testing.T) {
	ownerOK(t, `
		f(int[] xs) string {
			string s = "x";
			for x in xs { return s; }
			return s;
		}
		test() {}
	`)
}

// if-expression with a diverging then-branch used as a statement.
func TestT1134_IfExprDivergingThen(t *testing.T) {
	ownerOK(t, `
		f(bool cond) string {
			string s = "x";
			int n = if cond { return s; } else { 0 };
			_ = n;
			return s;
		}
		test() {}
	`)
}

// match arm that diverges must not poison the subject on the fall-through.
func TestT1134_MatchArmDiverges(t *testing.T) {
	ownerOK(t, `
		enum E { A, B }
		consume(string move s) {}
		f(E e) string {
			string s = "x";
			match e {
				E.A => { consume(move s); return "from-a"; },
				E.B => {},
			}
			return s;
		}
		test() {}
	`)
}

// Explicit-else equivalent must still compile (regression guard — already did).
func TestT1134_IfReturnWithElse(t *testing.T) {
	ownerOK(t, `
		f(bool cond) string {
			string s = "x";
			if cond { return s; } else { return s; }
		}
		test() {}
	`)
}

// --- Negative guards: the fix must NOT over-loosen ---

// Move in a branch that FALLS THROUGH (no divergence) → still Moved after the
// merge, so the post-if use must still be rejected.
func TestT1134_NegMoveInNonDivergingBranch(t *testing.T) {
	errs := ownerErrs(t, `
		f(bool cond) string {
			string s = "x";
			if cond { string x = s; }
			return s;
		}
		test() {}
	`)
	expectOwnerError(t, errs, "use of moved variable 's'")
}

// Move in a non-diverging else-branch → still Moved on that path; post-if use
// rejected.
func TestT1134_NegMoveInNonDivergingElse(t *testing.T) {
	errs := ownerErrs(t, `
		f(bool cond) string {
			string s = "x";
			if cond { return s; } else { string x = s; }
			return s;
		}
		test() {}
	`)
	expectOwnerError(t, errs, "use of moved variable 's'")
}

// Soundness guard: a loop body that moves a non-loop-local var then `break`s
// transfers the moved state to post-loop code, so the loop-divergence shortcut
// must NOT fire — the move must still be observed after the loop. (T1134 must
// not introduce a use-after-move false negative.)
func TestT1134_NegLoopBreakCarriesMove(t *testing.T) {
	errs := ownerErrs(t, `
		consume(string move s) {}
		f() string {
			string s = "x";
			for {
				consume(move s);
				break;
			}
			return s;
		}
		test() {}
	`)
	expectOwnerError(t, errs, "use of moved variable 's'")
}

// Soundness guard: a loop body that moves then `continue`s before its divergent
// terminator can take the continue on every iteration, so the loop completes
// naturally and reaches post-loop code with the variable moved — the divergent
// `return` never runs. The loop-divergence shortcut must NOT fire when a
// continue is present, or this becomes a use-after-move false negative. (T1134)
func TestT1134_NegLoopContinueCarriesMove(t *testing.T) {
	errs := ownerErrs(t, `
		consume(string move s) {}
		f(int[] xs, bool cond) string {
			string s = "x";
			for x in xs {
				consume(move s);
				if cond { continue; }
				return "done";
			}
			return s;
		}
		test() {}
	`)
	expectOwnerError(t, errs, "use of moved variable 's'")
}

// A non-diverging match arm that moves the subject still poisons the
// fall-through.
func TestT1134_NegMatchArmNonDivergingMove(t *testing.T) {
	errs := ownerErrs(t, `
		enum E { A, B }
		consume(string move s) {}
		f(E e) string {
			string s = "x";
			match e {
				E.A => { consume(s); },
				E.B => {},
			}
			return s;
		}
		test() {}
	`)
	expectOwnerError(t, errs, "use of moved variable 's'")
}

// === T1134: additional coverage for divergence-detection helpers ===
//
// These exercise the structural-divergence and break/continue-detection paths
// in diverge.go (else-if chains, infinite loops, and break/continue carried
// through nested if/block/match) that the primary T1134 tests don't reach.

// else-if chain where every arm diverges: the whole if never falls through, so
// the trailing use stays owned. Exercises stmtDiverges' *ast.IfStmt arm (the
// `else` of an if is itself an IfStmt) with all sub-arms diverging.
func TestT1134_ElseIfChainAllDiverge(t *testing.T) {
	ownerOK(t, `
		f(bool a, bool b) string {
			string s = "x";
			if a { return s; } else if b { return s; } else { return s; }
		}
		test() {}
	`)
}

// else-if chain where the inner if has no else → the chain can fall through, so
// the outer then-branch's divergence still leaves s owned on the fall-through.
// Exercises stmtDiverges' IfStmt arm returning false (inner Else == nil).
func TestT1134_ElseIfChainCanFallThrough(t *testing.T) {
	ownerOK(t, `
		f(bool a, bool b) string {
			string s = "x";
			if a { return s; } else if b { return s; }
			return s;
		}
		test() {}
	`)
}

// A branch whose trailing statement is an infinite loop with no break never
// falls through, so a move inside it does not poison the fall-through. Exercises
// stmtDiverges' *ast.InfiniteLoop arm (no-break → diverges).
func TestT1134_InfiniteLoopBranchDiverges(t *testing.T) {
	ownerOK(t, `
		consume(string move s) {}
		f(bool cond) string {
			string s = "x";
			if cond {
				consume(move s);
				for { }
			}
			return s;
		}
		test() {}
	`)
}

// Negative: an infinite loop WITH a break does fall through, so the branch does
// NOT diverge and the move must still poison the fall-through. Exercises the
// direct *ast.BreakStmt detection and InfiniteLoop's break check returning true.
func TestT1134_NegInfiniteLoopWithBreakNoDiverge(t *testing.T) {
	errs := ownerErrs(t, `
		consume(string move s) {}
		f(bool cond) string {
			string s = "x";
			if cond {
				consume(move s);
				for { break; }
			}
			return s;
		}
		test() {}
	`)
	expectOwnerError(t, errs, "use of moved variable 's'")
}

// Negative: a break nested inside an `if` in a loop body whose trailing stmt
// returns. The break carries the moved state to post-loop code, so the
// loop-divergence shortcut must not fire. Exercises break detection through
// *ast.IfStmt (then-arm).
func TestT1134_NegLoopBreakInIf(t *testing.T) {
	errs := ownerErrs(t, `
		consume(string move s) {}
		f(int[] xs, bool cond) string {
			string s = "x";
			for x in xs {
				if cond { consume(move s); break; }
				return "done";
			}
			return s;
		}
		test() {}
	`)
	expectOwnerError(t, errs, "use of moved variable 's'")
}

// Negative: break nested inside an `else` arm (then-arm has no break) of a loop
// body that otherwise returns. Exercises break detection recursing into the
// IfStmt's Else.
func TestT1134_NegLoopBreakInElse(t *testing.T) {
	errs := ownerErrs(t, `
		consume(string move s) {}
		f(int[] xs, bool cond) string {
			string s = "x";
			for x in xs {
				if cond { } else { consume(move s); break; }
				return "done";
			}
			return s;
		}
		test() {}
	`)
	expectOwnerError(t, errs, "use of moved variable 's'")
}

// Negative: break nested inside a bare block in a loop body that otherwise
// returns. Exercises break detection through *ast.Block.
func TestT1134_NegLoopBreakInBareBlock(t *testing.T) {
	errs := ownerErrs(t, `
		consume(string move s) {}
		f(int[] xs, bool cond) string {
			string s = "x";
			for x in xs {
				{ if cond { consume(move s); break; } }
				return "done";
			}
			return s;
		}
		test() {}
	`)
	expectOwnerError(t, errs, "use of moved variable 's'")
}

// Negative: break nested inside a match arm in a loop body that otherwise
// returns. Exercises break detection through a match expression's arms.
func TestT1134_NegLoopBreakInMatch(t *testing.T) {
	errs := ownerErrs(t, `
		enum E { A, B }
		consume(string move s) {}
		f(int[] xs, E e) string {
			string s = "x";
			for x in xs {
				match e {
					E.A => { consume(move s); break; },
					E.B => {},
				}
				return "done";
			}
			return s;
		}
		test() {}
	`)
	expectOwnerError(t, errs, "use of moved variable 's'")
}

// Negative: continue nested inside a bare block in a loop body that otherwise
// returns. The continue lets the loop complete naturally with s moved.
// Exercises continue detection through *ast.Block.
func TestT1134_NegLoopContinueInBareBlock(t *testing.T) {
	errs := ownerErrs(t, `
		consume(string move s) {}
		f(int[] xs, bool cond) string {
			string s = "x";
			for x in xs {
				{ if cond { consume(move s); continue; } }
				return "done";
			}
			return s;
		}
		test() {}
	`)
	expectOwnerError(t, errs, "use of moved variable 's'")
}

// Negative: continue nested inside a match arm in a loop body that otherwise
// returns. Exercises continue detection through a match expression's arms.
func TestT1134_NegLoopContinueInMatch(t *testing.T) {
	errs := ownerErrs(t, `
		enum E { A, B }
		consume(string move s) {}
		f(int[] xs, E e) string {
			string s = "x";
			for x in xs {
				match e {
					E.A => { consume(move s); continue; },
					E.B => {},
				}
				return "done";
			}
			return s;
		}
		test() {}
	`)
	expectOwnerError(t, errs, "use of moved variable 's'")
}

// Negative: continue nested inside an `else` arm of a loop body that otherwise
// returns. Exercises continue detection recursing into the IfStmt's Else.
func TestT1134_NegLoopContinueInElse(t *testing.T) {
	errs := ownerErrs(t, `
		consume(string move s) {}
		f(int[] xs, bool cond) string {
			string s = "x";
			for x in xs {
				if cond { } else { consume(move s); continue; }
				return "done";
			}
			return s;
		}
		test() {}
	`)
	expectOwnerError(t, errs, "use of moved variable 's'")
}

// A divergent loop body containing a no-else `if` (with no break/continue) still
// takes the loop-divergence shortcut, leaving s owned after the loop. Exercises
// the break/continue detection recursing past an if whose else is nil.
func TestT1134_DivergingLoopWithPlainIf(t *testing.T) {
	ownerOK(t, `
		f(int[] xs, bool cond) string {
			string s = "x";
			for x in xs {
				if cond { }
				return s;
			}
			return s;
		}
		test() {}
	`)
}

// if-statement with an else whose body diverges (then falls through): the move
// in the diverging else is excluded, so s stays owned via the then-state.
// Exercises checkIfStmt's `case elseDiverges` merge arm.
func TestT1134_IfStmtElseDivergesOnly(t *testing.T) {
	ownerOK(t, `
		consume(string move s) {}
		f(bool cond) string {
			string s = "x";
			if cond { } else { consume(move s); return "early"; }
			return s;
		}
		test() {}
	`)
}

// if-expression whose else diverges (then yields a value): the result state is
// the then-state, leaving s owned. Exercises checkIfExpr's `case elseDiverges`.
func TestT1134_IfExprElseDiverges(t *testing.T) {
	ownerOK(t, `
		consume(string move s) {}
		f(bool cond) string {
			string s = "x";
			int n = if cond { 0 } else { consume(move s); return "early" };
			_ = n;
			return s;
		}
		test() {}
	`)
}

// if-expression where BOTH arms diverge: the expression yields no value and the
// post-expression state is the pre-if baseline. Exercises checkIfExpr's
// `case thenDiverges && elseDiverges` arm.
func TestT1134_IfExprBothDiverge(t *testing.T) {
	ownerOK(t, `
		observe(int n) {}
		f(bool cond) string {
			string s = "x";
			int n = if cond { return "a" } else { return "b" };
			observe(n);
			return s;
		}
		test() {}
	`)
}

// match expression in statement position where every arm diverges: post-match
// code is unreachable, so the analyzer restores the pre-match baseline.
// Exercises checkMatchExpr's `len(states) == 0` path.
func TestT1134_MatchAllArmsDiverge(t *testing.T) {
	ownerOK(t, `
		enum E { A, B }
		f(E e) string {
			string s = "x";
			match e {
				E.A => { return s; },
				E.B => { return s; },
			}
		}
		test() {}
	`)
}

// select with a diverging case and a non-diverging default: the case's move is
// excluded; the default keeps s owned. Exercises the select-case divergence
// skip plus the default-clause merge.
func TestT1134_SelectCaseDivergesWithDefault(t *testing.T) {
	ownerOK(t, `
		consume(string move s) {}
		f(channel[int] ch) string {
			string s = "x";
			select {
				v := <-ch:
					consume(move s);
					return "from-case";
				default:
					string t = "y";
					_ = t;
			}
			return s;
		}
		test() {}
	`)
}

// select with a diverging case and a non-diverging case: the diverging case's
// move is excluded from the merge while the other case keeps s owned. Exercises
// the select-case divergence skip without a default clause (the merge-with-
// pre-select branch).
//
// NOTE: the non-diverging case bodies avoid a discard assignment (`_ = t;`)
// because that crashes the AST builder inside a select case (T1136). They use a
// no-op consumer call instead.
func TestT1134_SelectCaseDivergesNoDefault(t *testing.T) {
	ownerOK(t, `
		consume(string move s) {}
		observe(int n) {}
		f(channel[int] ch, channel[int] ch2) string {
			string s = "x";
			select {
				v := <-ch:
					consume(move s);
					return "from-case";
				w := <-ch2:
					observe(w!);
			}
			return s;
		}
		test() {}
	`)
}

// === T1147: borrow of an owned for-in loop binding escaping into a `go` call ===

// Reject: a `string` for-in binding is an owned per-iteration value (dup'd on
// store). Borrowing it into a `go` call lets the borrow escape into a goroutine
// that may outlive the iteration → use-after-free.
func TestT1147GoCallBorrowOfLoopBindingRejected(t *testing.T) {
	errs := ownerErrs(t, `
		keep(string p) string { return p.clone(); }
		test() {
			string[] xs = ["a".clone(), "b".clone()];
			for x in xs { _ = go keep(x); }
		}
	`)
	expectOwnerError(t, errs, "borrowed loop variable")
}

// Reject: the method-call arg form — the same helper walks CallExpr.Args
// regardless of whether the callee is a free function or a method.
func TestT1147GoCallMethodArgLoopBindingRejected(t *testing.T) {
	errs := ownerErrs(t, `
		type Joiner { string sep; combine(this, string p) string { return this.sep + p; } }
		test() {
			string[] xs = ["a".clone(), "b".clone()];
			Joiner j = Joiner(sep: "-");
			for x in xs { _ = go j.combine(x); }
		}
	`)
	expectOwnerError(t, errs, "borrowed loop variable")
}

// Accept: `move` of the loop binding into a consuming `move` param transfers
// ownership into the goroutine frame (T1148 path) — not a borrow, not rejected.
func TestT1147GoCallMoveLoopBindingOK(t *testing.T) {
	ownerOK(t, `
		store(string move s) string { return s; }
		test() {
			string[] xs = ["a".clone(), "b".clone()];
			for x in xs { _ = go store(move x); }
		}
	`)
}

// Accept: cloning the binding into a fresh temp (T1098) — the arg root is not an
// ident, so the loop-binding-borrow-escape check does not fire.
func TestT1147GoCallCloneTempOK(t *testing.T) {
	ownerOK(t, `
		keep(string p) string { return p.clone(); }
		test() {
			string[] xs = ["a".clone(), "b".clone()];
			for x in xs { _ = go keep(x.clone()); }
		}
	`)
}

// Accept: an `int` binding is Copy — passed by value into the coro frame, no
// dangling borrow possible.
func TestT1147GoCallCopyBindingOK(t *testing.T) {
	ownerOK(t, `
		add(int n) int { return n + 1; }
		test() {
			int[] xs = [1, 2, 3];
			for x in xs { _ = go add(x); }
		}
	`)
}

// Accept: a heap-user-type binding aliases the container's element storage (the
// data outlives the loop in the container) — flagged in forInAliasBindings, not
// the owned-droppable set, so the go-call check does not fire.
func TestT1147GoCallAliasingBindingOK(t *testing.T) {
	ownerOK(t, `
		type Box { string s; }
		describe(Box b) string { return b.s.clone(); }
		test() {
			Box[] xs = [Box(s: "a".clone()), Box(s: "b".clone())];
			for x in xs { _ = go describe(x); }
		}
	`)
}

// Accept: a function-level local borrowed into a `go` call is sound (its scope
// outlives the goroutine when awaited in scope) — only for-in loop bindings are
// flagged, so a plain local must not be rejected.
func TestT1147GoCallFunctionLocalBorrowOK(t *testing.T) {
	ownerOK(t, `
		keep(string p) string { return p.clone(); }
		test() {
			string s = "hello".clone();
			t := go keep(s);
			_ = <-t;
		}
	`)
}

// Accept: the loop binding as a method *receiver* (not an arg) is captured and
// auto-dup'd by the closure mechanism — sound, and the arg-walking check never
// inspects the receiver.
func TestT1147GoCallReceiverLoopBindingOK(t *testing.T) {
	ownerOK(t, `
		test() {
			string[] xs = ["a".clone(), "b".clone()];
			for x in xs { _ = go x.to_upper(); }
		}
	`)
}

// Reject: a parenthesized loop-binding arg — identRoot peels the ParenExpr
// layer(s) down to the underlying ident, so the borrow-escape check still fires.
func TestT1147GoCallParenLoopBindingRejected(t *testing.T) {
	errs := ownerErrs(t, `
		keep(string p) string { return p.clone(); }
		test() {
			string[] xs = ["a".clone(), "b".clone()];
			for x in xs { _ = go keep((x)); }
		}
	`)
	expectOwnerError(t, errs, "borrowed loop variable")
}

// Accept: a constructor callee has no Signature (calleeSignature returns nil),
// so the loop-binding-borrow-escape check returns early — constructors consume
// their args (out of scope for T1147; the nested-ctor shape is tracked by T1106).
func TestT1147GoCallConstructorLoopBindingOK(t *testing.T) {
	ownerOK(t, `
		type Box { string s; }
		test() {
			string[] xs = ["a".clone(), "b".clone()];
			for x in xs { _ = go Box(s: move x); }
		}
	`)
}

// Accept: a container-store native (`Vector.push`) consumes its arg into storage
// that outlives the goroutine frame — the `kind == BorrowNone && storeNative`
// skip fires (continue) before identRoot is consulted, so the moved binding is
// not flagged as a borrow escape.
func TestT1147GoCallStoreNativeLoopBindingOK(t *testing.T) {
	ownerOK(t, `
		test() {
			string[] xs = ["a".clone(), "b".clone()];
			string[] sink = [];
			for x in xs { _ = go sink.push(move x); }
		}
	`)
}

// Accept (regression guard): an owned for-in binding borrowed into a *plain*
// (non-`go`) call is sound — the borrow lives for the call's duration. Only the
// `go` form escapes, so the loop-binding flag must not leak into ordinary calls.
func TestT1147PlainCallBorrowOfLoopBindingOK(t *testing.T) {
	ownerOK(t, `
		keep(string p) string { return p.clone(); }
		test() {
			string[] xs = ["a".clone(), "b".clone()];
			for x in xs { _ = keep(x); }
		}
	`)
}

// Reject across nested loops: the owned-droppable flag must be set for the inner
// binding too, and the save/restore must not lose the outer flag. Both bindings
// are owned strings; borrowing either into a `go` call is rejected.
func TestT1147GoCallNestedLoopBindingRejected(t *testing.T) {
	errs := ownerErrs(t, `
		keep(string p) string { return p.clone(); }
		test() {
			string[] xs = ["a".clone(), "b".clone()];
			for x in xs {
				for y in xs { _ = go keep(y); }
				_ = go keep(x);
			}
		}
	`)
	expectOwnerError(t, errs, "borrowed loop variable")
}

// === T1151: borrow of an owned droppable local declared inside a loop body
// escaping into a `go` call (sibling of T1147's loop-binding shape) ===

// Reject: a typed owned `string` local declared in a for-in body, borrowed into
// a `go` call. The local's scope ends at the iteration boundary, so the borrow
// dangles into a goroutine that may outlive it → use-after-free (the exact repro).
func TestT1151GoCallLoopBodyLocalRejected(t *testing.T) {
	errs := ownerErrs(t, `
		keep(string p) string { return p.clone(); }
		test() {
			string[] xs = ["a".clone(), "b".clone()];
			for x in xs {
				string y = x.clone();
				tk := go keep(y);
				r := <-tk;
				print_line(r);
			}
		}
	`)
	expectOwnerError(t, errs, "borrowed loop variable")
}

// Reject: the inferred-decl form (`y := x.clone()`) — flagLoopBodyOwnedLocal
// fires in checkInferredVarDecl too.
func TestT1151GoCallLoopBodyLocalInferredRejected(t *testing.T) {
	errs := ownerErrs(t, `
		keep(string p) string { return p.clone(); }
		test() {
			string[] xs = ["a".clone(), "b".clone()];
			for x in xs {
				y := x.clone();
				tk := go keep(y);
				r := <-tk;
				print_line(r);
			}
		}
	`)
	expectOwnerError(t, errs, "borrowed loop variable")
}

// Reject: the same shape inside a `while` body — exercises checkWhileStmt raising
// loop depth.
func TestT1151GoCallLoopBodyLocalWhileRejected(t *testing.T) {
	errs := ownerErrs(t, `
		keep(string p) string { return p.clone(); }
		test() {
			bool go_on = true;
			while go_on {
				string y = "v".clone();
				tk := go keep(y);
				r := <-tk;
				print_line(r);
				go_on = false;
			}
		}
	`)
	expectOwnerError(t, errs, "borrowed loop variable")
}

// Reject: the same shape inside a classic `for` body — exercises
// checkClassicForStmt raising loop depth.
func TestT1151GoCallLoopBodyLocalClassicForRejected(t *testing.T) {
	errs := ownerErrs(t, `
		keep(string p) string { return p.clone(); }
		test() {
			for i := 0; i < 3; i = i + 1 {
				string y = "v".clone();
				tk := go keep(y);
				r := <-tk;
				print_line(r);
			}
		}
	`)
	expectOwnerError(t, errs, "borrowed loop variable")
}

// Reject: the same shape inside an infinite `for { }` loop body — exercises
// checkInfiniteLoop raising loop depth.
func TestT1151GoCallLoopBodyLocalInfiniteLoopRejected(t *testing.T) {
	errs := ownerErrs(t, `
		keep(string p) string { return p.clone(); }
		test() {
			for {
				string y = "v".clone();
				tk := go keep(y);
				r := <-tk;
				print_line(r);
				break;
			}
		}
	`)
	expectOwnerError(t, errs, "borrowed loop variable")
}

// Reject: an owned heap-user-type local in a loop body — coverage is broader than
// `string`; isDroppableType covers any droppable owned local.
func TestT1151GoCallLoopBodyHeapUserLocalRejected(t *testing.T) {
	errs := ownerErrs(t, `
		type Box { string s; }
		describe(Box b) string { return b.s.clone(); }
		test() {
			for i in 0..3 {
				Box b = Box(s: "a".clone());
				tk := go describe(b);
				r := <-tk;
				print_line(r);
			}
		}
	`)
	expectOwnerError(t, errs, "borrowed loop variable")
}

// Accept: a Copy local (`int y = ...`) in a loop body — excluded by isCopyType,
// passed by value into the coro frame.
func TestT1151GoCallLoopBodyCopyLocalOK(t *testing.T) {
	ownerOK(t, `
		add(int n) int { return n + 1; }
		test() {
			for i in 0..3 {
				int y = i;
				tk := go add(y);
				r := <-tk;
			}
		}
	`)
}

// Accept: `move` of the loop-body local into a consuming `move` param transfers
// ownership into the goroutine frame.
func TestT1151GoCallLoopBodyLocalMoveOK(t *testing.T) {
	ownerOK(t, `
		store(string move s) string { return s; }
		test() {
			string[] xs = ["a".clone(), "b".clone()];
			for x in xs {
				string y = x.clone();
				tk := go store(move y);
				r := <-tk;
				print_line(r);
			}
		}
	`)
}

// Accept: cloning the loop-body local into a fresh temp at the call site — the arg
// root is not an ident, so the borrow-escape check does not fire.
func TestT1151GoCallLoopBodyLocalCloneTempOK(t *testing.T) {
	ownerOK(t, `
		keep(string p) string { return p.clone(); }
		test() {
			string[] xs = ["a".clone(), "b".clone()];
			for x in xs {
				string y = x.clone();
				tk := go keep(y.clone());
				r := <-tk;
				print_line(r);
			}
		}
	`)
}

// Accept: an owned droppable local declared BEFORE the loop, borrowed into a `go`
// call inside the loop — loopDepth == 0 at its decl, so it is not flagged; its
// function-scope lifetime is sound when awaited in scope.
func TestT1151GoCallLocalBeforeLoopOK(t *testing.T) {
	ownerOK(t, `
		keep(string p) string { return p.clone(); }
		test() {
			string y = "shared".clone();
			for i in 0..3 {
				tk := go keep(y);
				r := <-tk;
				print_line(r);
			}
		}
	`)
}

// Accept: an owned local declared inside a `go { }` block that is itself inside a
// loop, borrowed into a nested `go` — the depth-reset guard in the GoExpr case
// prevents a false positive (the local is owned by the goroutine frame).
func TestT1151GoBlockLocalInLoopOK(t *testing.T) {
	ownerOK(t, `
		keep(string p) string { return p.clone(); }
		test() {
			for i in 0..3 {
				go {
					string y = "v".clone();
					tk := go keep(y);
					r := <-tk;
					print_line(r);
				}
			}
		}
	`)
}

// Accept: an owned local declared inside a lambda body that is itself inside a
// loop — the depth-reset guard in checkLambdaExpr prevents a false positive.
func TestT1151LambdaLocalInLoopOK(t *testing.T) {
	ownerOK(t, `
		keep(string p) string { return p.clone(); }
		test() {
			for i in 0..3 {
				f := || {
					string y = "v".clone();
					tk := go keep(y);
					r := <-tk;
					print_line(r);
				};
				f();
			}
		}
	`)
}

// Accept (regression guard): an owned loop-body local borrowed into a *plain*
// (non-`go`) call is sound — the flag must not leak into ordinary calls.
func TestT1151PlainCallLoopBodyLocalOK(t *testing.T) {
	ownerOK(t, `
		keep(string p) string { return p.clone(); }
		test() {
			string[] xs = ["a".clone(), "b".clone()];
			for x in xs {
				string y = x.clone();
				r := keep(y);
				print_line(r);
			}
		}
	`)
}

// Accept: a loop-body local must not stay flagged after the loop closes — the
// enterLoopBody/exitLoopBody snapshot removes body locals at loop exit, so a
// same-named local at function scope after the loop is sound to borrow into `go`.
func TestT1151LocalAfterLoopNotFlaggedOK(t *testing.T) {
	ownerOK(t, `
		keep(string p) string { return p.clone(); }
		test() {
			string[] xs = ["a".clone(), "b".clone()];
			for x in xs { r := keep(x); print_line(r); }
			string y = "after".clone();
			tk := go keep(y);
			r := <-tk;
			print_line(r);
		}
	`)
}

// Reject: an outer-loop-body local borrowed into a `go` call nested inside an
// INNER loop. The inner loop's enterLoopBody snapshot copies the outer local
// (already in the set) forward, so the borrow-escape still fires at the nested
// call site — the outer local is just as iteration-bounded from the inner loop's
// perspective. Exercises the snapshot-carries-forward path unique to T1151.
func TestT1151GoCallOuterLoopBodyLocalInNestedLoopRejected(t *testing.T) {
	errs := ownerErrs(t, `
		keep(string p) string { return p.clone(); }
		test() {
			for i in 0..2 {
				string y = "v".clone();
				for j in 0..2 {
					tk := go keep(y);
					r := <-tk;
					print_line(r);
				}
			}
		}
	`)
	expectOwnerError(t, errs, "borrowed loop variable")
}

// Accept: an inner-loop-body local must not stay flagged after the INNER loop
// closes while still inside the OUTER loop — the inner exitLoopBody snapshot
// restore drops it. A plain (non-`go`) reuse of the same name after the inner
// loop is sound, confirming the snapshot scope is the inner loop, not the outer.
func TestT1151InnerLoopBodyLocalNotFlaggedAfterInnerLoopOK(t *testing.T) {
	ownerOK(t, `
		keep(string p) string { return p.clone(); }
		test() {
			for i in 0..2 {
				for j in 0..2 {
					string y = "v".clone();
					r := keep(y);
					print_line(r);
				}
			}
		}
	`)
}

// Reject: a loop-body local of an owned droppable ENUM (a droppable variant
// payload) — confirms flagLoopBodyOwnedLocal's isDroppableType guard covers
// enums, not just strings / heap user types.
func TestT1151GoCallLoopBodyEnumLocalRejected(t *testing.T) {
	errs := ownerErrs(t, `
		enum Holder { Empty, Full(string value) }
		describe(Holder h) string { return "x".clone(); }
		test() {
			for i in 0..3 {
				Holder h = Holder.Full("a".clone());
				tk := go describe(h);
				r := <-tk;
				print_line(r);
			}
		}
	`)
	expectOwnerError(t, errs, "borrowed loop variable")
}

// === T1153: borrow of a `while x := opt? { … }` unwrap binding escaping into a
// `go` call (sibling of T1147's for-in binding and T1151's loop-body local) ===

// Reject: a `string` while-unwrap binding is a fresh owned per-iteration value
// (the unwrapped `opt.Elem()`), freed at the iteration boundary. Borrowing it
// into a `go` call lets the borrow escape into a goroutine that may outlive the
// iteration → use-after-free (the exact repro).
func TestT1153GoCallBorrowOfWhileUnwrapBindingRejected(t *testing.T) {
	errs := ownerErrs(t, `
		keep(string p) string { return p.clone(); }
		next() string? { return "v".clone(); }
		test() { while y := next() { _ = go keep(y); } }
	`)
	expectOwnerError(t, errs, "borrowed loop variable")
}

// Reject: the method-call arg form — the same helper walks CallExpr.Args
// regardless of whether the callee is a free function or a method.
func TestT1153GoCallMethodArgWhileUnwrapBindingRejected(t *testing.T) {
	errs := ownerErrs(t, `
		type Joiner { string sep; combine(this, string p) string { return this.sep + p; } }
		next() string? { return "v".clone(); }
		test() {
			Joiner j = Joiner(sep: "-");
			while y := next() { _ = go j.combine(y); }
		}
	`)
	expectOwnerError(t, errs, "borrowed loop variable")
}

// Reject: a parenthesized arg — identRoot peels the ParenExpr layer(s) down to
// the underlying ident, so the borrow-escape check still fires.
func TestT1153GoCallParenWhileUnwrapBindingRejected(t *testing.T) {
	errs := ownerErrs(t, `
		keep(string p) string { return p.clone(); }
		next() string? { return "v".clone(); }
		test() { while y := next() { _ = go keep((y)); } }
	`)
	expectOwnerError(t, errs, "borrowed loop variable")
}

// Reject: a heap-user-type unwrap binding — coverage is broader than `string`;
// isDroppableType covers any droppable owned binding.
func TestT1153GoCallWhileUnwrapHeapUserBindingRejected(t *testing.T) {
	errs := ownerErrs(t, `
		type Box { string s; }
		describe(Box b) string { return b.s.clone(); }
		next() Box? { return Box(s: "a".clone()); }
		test() { while y := next() { _ = go describe(y); } }
	`)
	expectOwnerError(t, errs, "borrowed loop variable")
}

// Accept: `move` of the unwrap binding into a consuming `move` param transfers
// ownership into the goroutine frame — not a borrow, not rejected.
func TestT1153GoCallMoveWhileUnwrapBindingOK(t *testing.T) {
	ownerOK(t, `
		store(string move s) string { return s; }
		next() string? { return "v".clone(); }
		test() { while y := next() { _ = go store(move y); } }
	`)
}

// Accept: cloning the binding into a fresh temp at the call site — the arg root
// is not an ident, so the borrow-escape check does not fire.
func TestT1153GoCallWhileUnwrapCloneTempOK(t *testing.T) {
	ownerOK(t, `
		keep(string p) string { return p.clone(); }
		next() string? { return "v".clone(); }
		test() { while y := next() { _ = go keep(y.clone()); } }
	`)
}

// Accept: an `int?` binding is Copy — passed by value into the coro frame, no
// dangling borrow possible.
func TestT1153GoCallWhileUnwrapCopyBindingOK(t *testing.T) {
	ownerOK(t, `
		add(int n) int { return n + 1; }
		next() int? { return 1; }
		test() { while y := next() { _ = go add(y); } }
	`)
}

// Accept: the unwrap binding as a method *receiver* (not an arg) is captured and
// auto-dup'd by the closure mechanism — sound, and the arg-walking check never
// inspects the receiver.
func TestT1153GoCallWhileUnwrapReceiverBindingOK(t *testing.T) {
	ownerOK(t, `
		next() string? { return "v".clone(); }
		test() { while y := next() { _ = go y.to_upper(); } }
	`)
}

// Accept (regression guard): an owned while-unwrap binding borrowed into a
// *plain* (non-`go`) call is sound — the borrow lives for the call's duration.
// Only the `go` form escapes, so the flag must not leak into ordinary calls.
func TestT1153PlainCallBorrowOfWhileUnwrapBindingOK(t *testing.T) {
	ownerOK(t, `
		keep(string p) string { return p.clone(); }
		next() string? { return "v".clone(); }
		test() { while y := next() { _ = keep(y); } }
	`)
}

// Accept (no regression): the T0589 borrowed-Optional-parameter while-let path
// still errors with its own dedicated message — the new flagging must not
// interfere with (or mask) that distinct diagnostic.
func TestT1153WhileLetBorrowedParamStillRejected(t *testing.T) {
	errs := ownerErrs(t, `
		type _Box { int n; }
		take(_Box? a) {
			while x := a {
				_ = x;
				break;
			}
		}
		test() {}
	`)
	expectOwnerError(t, errs, "cannot consume borrowed parameter 'a' via while-let")
}

// Accept (restore guard): the enterLoopBody snapshot must remove the while-unwrap
// binding from the owned-droppable set at loop exit. A same-named owned local
// declared AFTER the loop and borrowed into a `go` call is sound (it is not a
// loop binding) — it must NOT inherit the loop binding's flag. Regression guard
// for the snapshot restore claimed in checkWhileUnwrapStmt.
func TestT1153FlagRestoredAfterWhileUnwrapLoop(t *testing.T) {
	ownerOK(t, `
		keep(string p) string { return p.clone(); }
		next() string? { return "v".clone(); }
		test() {
			while y := next() { _ = keep(y); }
			string y = "outer".clone();
			_ = go keep(y);
			_ = move y;
		}
	`)
}

// Reject (nested): an inner while-unwrap loop's binding is flagged AND the
// outer binding survives the inner loop's enter/exit snapshot — a `go` borrow
// of the outer binding `y` AFTER the inner loop is still rejected. The inner
// snapshot must restore only the inner addition, not clobber the outer entry.
func TestT1153NestedWhileUnwrapBothBindingsRejected(t *testing.T) {
	errs := ownerErrs(t, `
		keep(string p) string { return p.clone(); }
		next() string? { return "v".clone(); }
		test() {
			while y := next() {
				while z := next() { _ = go keep(z); }
				_ = go keep(y);
			}
		}
	`)
	// Both the inner (z) and the outer-after-inner (y) borrows must be rejected.
	if len(errs) < 2 {
		t.Fatalf("expected at least 2 borrowed-loop-variable errors, got %d: %v", len(errs), errs)
	}
	expectOwnerError(t, errs, "borrowed loop variable 'z'")
	expectOwnerError(t, errs, "borrowed loop variable 'y'")
}

// === T1152: escaping `go f(&local)` task-handle borrows ===
//
// A `go f(arg)` of a bare-ident borrow of an owned, droppable, non-Copy
// function/block-scope local spawns a goroutine that reads `arg` from the
// caller's frame. If the resulting Task handle escapes the local's scope
// (returned, stored in a longer-lived container, reassigned out, or sent on a
// channel) the goroutine can read the local after it drops → use-after-free.
// The sound in-scope await/drop shapes stay accepted (the handle joins the
// goroutine while the local is still alive). Iteration-bounded for-in bindings
// (T1147) and loop-body locals (T1151) are a sibling case handled by
// rejectGoCallLoopBindingBorrowEscape at the call site — the last two tests here
// pin that the unified check delegates to it (no gap, no double-report).

// Inline `return go keep(s)` — the handle escapes via the return value.
func TestT1152_ReturnInlineGoBorrowRejected(t *testing.T) {
	errs := ownerErrs(t, `
		type Box { string s; }
		keep(string p) Box { return Box(s: p.clone()); }
		spawn() Task[Box] {
			string s = "hello".clone();
			return go keep(s);
		}
		test() {}
	`)
	expectOwnerError(t, errs, "'go' task handle escape")
}

// `t := go keep(s); return t;` — the handle is bound, then escapes via return.
func TestT1152_ReturnBoundGoHandleRejected(t *testing.T) {
	errs := ownerErrs(t, `
		type Box { string s; }
		keep(string p) Box { return Box(s: p.clone()); }
		spawn() Task[Box] {
			string s = "hello".clone();
			Task[Box] t = go keep(s);
			return t;
		}
		test() {}
	`)
	expectOwnerError(t, errs, "'go' task handle escape")
}

// `ts.push(go keep(s))` — the inline handle escapes into a longer-lived vector.
func TestT1152_PushInlineGoBorrowRejected(t *testing.T) {
	errs := ownerErrs(t, `
		type Box { string s; }
		keep(string p) Box { return Box(s: p.clone()); }
		spawn() {
			Vector[Task[Box]] ts = Vector[Task[Box]]();
			string s = "hello".clone();
			ts.push(go keep(s));
		}
		test() {}
	`)
	expectOwnerError(t, errs, "'go' task handle escape")
}

// `t := go keep(s); ts.push(move t);` — bound handle escapes into a vector.
func TestT1152_PushBoundGoHandleRejected(t *testing.T) {
	errs := ownerErrs(t, `
		type Box { string s; }
		keep(string p) Box { return Box(s: p.clone()); }
		spawn() {
			Vector[Task[Box]] ts = Vector[Task[Box]]();
			string s = "hello".clone();
			Task[Box] t = go keep(s);
			ts.push(move t);
		}
		test() {}
	`)
	expectOwnerError(t, errs, "'go' task handle escape")
}

// `ch.send(go keep(s))` — the inline handle escapes by being sent on a channel.
func TestT1152_ChannelSendInlineGoBorrowRejected(t *testing.T) {
	errs := ownerErrs(t, `
		type Box { string s; }
		keep(string p) Box { return Box(s: p.clone()); }
		spawn(channel[Task[Box]] ch) {
			string s = "hello".clone();
			ch.send(go keep(s));
		}
		test() {}
	`)
	expectOwnerError(t, errs, "'go' task handle escape")
}

// Reassigning a longer-lived (outer) binding from a bound handle escapes it.
// Plain assignment routes the RHS through tryMoveConsume, where the escape check
// runs before the "consuming requires move" requirement, so the diagnostic is
// the go-handle escape message.
func TestT1152_ReassignOuterFromGoHandleRejected(t *testing.T) {
	errs := ownerErrs(t, `
		worker(string p) int { return p.len; }
		spawn(Task[int] move outer) {
			string s = "hello".clone();
			Task[int] t = go worker(s);
			outer = t;
		}
		test() {}
	`)
	expectOwnerError(t, errs, "'go' task handle escape")
}

// Accept: `t := go keep(s); _ = <-t;` — the handle is awaited in scope, joining
// the goroutine while `s` is still alive. Sound.
func TestT1152_AwaitInScopeAllowed(t *testing.T) {
	ownerOK(t, `
		type Box { string s; }
		keep(string p) Box { return Box(s: p.clone()); }
		run() {
			string s = "hello".clone();
			Task[Box] t = go keep(s);
			Box b = <-t;
		}
		test() {}
	`)
}

// Accept: `t := go keep(s);` with no escape — the handle drops at scope exit,
// joining the goroutine (LIFO) before `s` drops. Sound.
func TestT1152_DropInScopeAllowed(t *testing.T) {
	ownerOK(t, `
		worker(string p) int { return p.len; }
		run() {
			string s = "hello".clone();
			Task[int] t = go worker(s);
		}
		test() {}
	`)
}

// Accept: cloning into the goroutine — the goroutine owns its own copy, so the
// handle may escape freely.
func TestT1152_CloneIntoGoroutineAllowed(t *testing.T) {
	ownerOK(t, `
		type Box { string s; }
		keep(string p) Box { return Box(s: p.clone()); }
		spawn() Task[Box] {
			string s = "hello".clone();
			return go keep(s.clone());
		}
		test() {}
	`)
}

// Accept: moving an owned value into the goroutine (`~`/`move`) transfers it into
// the goroutine frame, so the handle may escape freely.
func TestT1152_MoveIntoGoroutineAllowed(t *testing.T) {
	ownerOK(t, `
		type Box { string s; }
		store(string move p) Box { return Box(s: move p); }
		spawn() Task[Box] {
			string s = "hello".clone();
			return go store(move s);
		}
		test() {}
	`)
}

// Accept: a no-arg `go` call borrows nothing, so the handle escapes freely.
func TestT1152_NoArgGoHandleAllowed(t *testing.T) {
	ownerOK(t, `
		worker() int { return 42; }
		spawn() Task[int] {
			return go worker();
		}
		test() {}
	`)
}

// Accept: a Copy (int) arg is passed by value, not borrowed — handle escapes OK.
func TestT1152_CopyArgGoHandleAllowed(t *testing.T) {
	ownerOK(t, `
		use_int(int n) int { return n + 1; }
		spawn() Task[int] {
			int n = 5;
			return go use_int(n);
		}
		test() {}
	`)
}

// Accept: a plain (non-`go`) call of a droppable local is a shared borrow that
// ends at the call — unaffected by the go-handle escape check.
func TestT1152_PlainCallOfLocalAllowed(t *testing.T) {
	ownerOK(t, `
		type Box { string s; }
		keep(string p) Box { return Box(s: p.clone()); }
		run() {
			string s = "hello".clone();
			Box b = keep(s);
		}
		test() {}
	`)
}

// Integration guard: a for-in loop binding borrowed into `go f(x)` whose handle
// is pushed to a vector outliving the iteration is rejected by the sibling
// call-site check (T1147), NOT by the T1152 handle-escape check — the latter
// excludes iteration-bounded bindings to avoid double-reporting.
func TestT1152_ForInBindingDeferredToLoopCheck(t *testing.T) {
	errs := ownerErrs(t, `
		type Box { string s; }
		keep(string p) Box { return Box(s: p.clone()); }
		spawn(string[] xs) {
			Vector[Task[Box]] ts = Vector[Task[Box]]();
			for x in xs {
				ts.push(go keep(x));
			}
		}
		test() {}
	`)
	expectOwnerError(t, errs, "into a goroutine")
	expectNoOwnerError(t, errs, "'go' task handle escape")
}

// Integration guard: a loop-body local borrowed into `go f(y)` whose handle is
// pushed to a vector outliving the iteration is rejected by the sibling call-site
// check (T1151), NOT by the T1152 handle-escape check.
func TestT1152_LoopBodyLocalDeferredToLoopCheck(t *testing.T) {
	errs := ownerErrs(t, `
		type Box { string s; }
		keep(string p) Box { return Box(s: p.clone()); }
		spawn(string[] xs) {
			Vector[Task[Box]] ts = Vector[Task[Box]]();
			for x in xs {
				string y = x.clone();
				ts.push(go keep(y));
			}
		}
		test() {}
	`)
	expectOwnerError(t, errs, "into a goroutine")
	expectNoOwnerError(t, errs, "'go' task handle escape")
}

// Reject (inferred decl form): `t := go keep(s); return t;`. The inferred-var
// path (checkInferredVarDecl) tracks the handle just like the typed-var path, so
// the escape via return is still caught. Pins that both decl forms route through
// trackGoHandleBinding.
func TestT1152_InferredHandleBindingEscapeRejected(t *testing.T) {
	errs := ownerErrs(t, `
		type Box { string s; }
		keep(string p) Box { return Box(s: p.clone()); }
		spawn() Task[Box] {
			string s = "hello".clone();
			t := go keep(s);
			return t;
		}
		test() {}
	`)
	expectOwnerError(t, errs, "'go' task handle escape")
}

// Accept: a PARAMETER (caller-owned, not a function-level local) borrowed into
// `go f(p)` whose handle escapes via return is NOT flagged by T1152. A parameter
// is owned by the caller's frame, so the goroutine-vs-local lifetime reasoning of
// this check does not apply — goCallBorrowsOwnedLocal skips params (the separate
// sibling gap noted in T1152). Pins the `c.params` continue branch.
func TestT1152_ParamBorrowEscapeNotFlagged(t *testing.T) {
	errs := ownerErrs(t, `
		type Box { string s; }
		keep(string p) Box { return Box(s: p.clone()); }
		spawn(string p) Task[Box] {
			return go keep(p);
		}
		test() {}
	`)
	expectNoOwnerError(t, errs, "'go' task handle escape")
}

// Accept: the `go { block }` form (not a `go f(arg)` call) is outside the T1152
// borrow-arg check entirely — its argument-borrow analysis only applies to the
// CallExpr shape. A returned go-block handle is not flagged here (block captures
// are a separate concern). Pins the not-CallExpr early-return branch of
// goCallBorrowsOwnedLocal.
func TestT1152_GoBlockHandleNotFlagged(t *testing.T) {
	errs := ownerErrs(t, `
		spawn() Task[int] {
			return go { 42 };
		}
		test() {}
	`)
	expectNoOwnerError(t, errs, "'go' task handle escape")
}
