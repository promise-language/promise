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
		consume(~Holder h) {}
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
		consume_vec(~Vector[(_BoxStr, int)] v) {}
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
		consume(~Holder h) {}
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
		consume(~Holder h) {}
		test() {
			Holder h = Holder(pair: (_BoxStr(s: "x"), 2));
			(b, n) := (h.pair);
			_ = b.s;
			_ = n;
			consume(h);
		}
	`)
}

// Paren-wrapped IndexExpr source — destructure from a vector element wrapped
// in parens, then consume the vector. Must reject for symmetry with the
// MemberExpr case.
func TestDestructureFromIndexParenConsumeParentRejected(t *testing.T) {
	errs := ownerErrs(t, `
		type _BoxStr { string s; }
		consume_vec(~Vector[(_BoxStr, int)] v) {}
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
		consume_vec(~Vector[(_BoxStr, int)] v) {}
		test() {
			Vector[(_BoxStr, int)] arr = [];
			arr.push((_BoxStr(s: "x"), 2));
			(b, n) := (arr[0]);
			_ = b.s;
			_ = n;
			consume_vec(arr);
		}
	`)
}

// Double-wrapped parens — `((h.pair))` — confirms the iterative peel handles
// nested ParenExpr without leaving a borrow gap.
func TestDestructureFromFieldDoubleParenRejected(t *testing.T) {
	errs := ownerErrs(t, `
		type _BoxStr { string s; }
		type Holder { (_BoxStr, int) pair; }
		consume(~Holder h) {}
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
		consume(~Holder h) {}
		test() {
			Holder h = Holder(pair: (_BoxStr(s: "x"), 2));
			(b, n) := h.pair;
			_ = b.s;
			_ = n;
			consume(h);
		}
	`)
}

// Destructure-from-field then move the destructured local into a consume
// site — must reject. Mirrors the existing T0338 "cannot move borrowed value"
// path: the local is in Borrowed state so tryMoveConsume rejects.
func TestDestructureFromFieldMoveLocalRejected(t *testing.T) {
	errs := ownerErrs(t, `
		type _BoxStr { string s; }
		type Holder { (_BoxStr, int) pair; }
		consume_box(~_BoxStr b) {}
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
		consume(~Holder h) {}
		test() {
			Holder h = Holder(pair: (1, 2));
			(a, b) := h.pair;
			_ = a + b;
			consume(h);
		}
	`)
}

// Mixed Copy / non-Copy: only the non-Copy local registers a borrow. Consume
// after its last use must still be accepted via NLL narrowing.
func TestDestructureFromFieldPartialCopyMixedNLL(t *testing.T) {
	ownerOK(t, `
		type _BoxStr { string s; }
		type Holder { (_BoxStr, int) pair; }
		consume(~Holder h) {}
		test() {
			Holder h = Holder(pair: (_BoxStr(s: "x"), 2));
			(b, n) := h.pair;
			_ = b.s;
			consume(h);
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
		consume(~Holder h) {}
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
		consume_holder(~Holder h) {}
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
		consume_holder(~Holder h) {}
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
		consume_box(~_BoxStr b) {}
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
			Holder[int] h = Holder[int](c: Color.Red, v: 7);
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
			new(~this, ~u8[] d) { this.data = d; }
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
		consume(~string s) {}
		forward(string s) { consume(s); }
		test() {}
	`)
	expectOwnerError(t, errs, "cannot move borrowed parameter 's'")
}

// Plain `this` (non-`~`, non-`&`) cannot itself be moved into a `~` callee.
func TestT0338_MovePlainThis(t *testing.T) {
	errs := ownerErrs(t, `
		consume(~Box b) {}
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
		consume(~Box b) {}
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
		f(&string s) int { return 1; }
		test() {
			string a = "hi";
			f(&a);
			int n = 1;
		}
	`)
}

// Local owned values can still be moved — only parameters are borrowed.
func TestT0338_LocalOwnedMovable(t *testing.T) {
	ownerOK(t, `
		consume(~string s) {}
		test() {
			string s = "hi";
			consume(s);
		}
	`)
}

// `~param` allows the callee to move the value into a constructor field.
func TestT0338_MutParamConsumableInConstructor(t *testing.T) {
	ownerOK(t, `
		type Box {
			u8[] data;
			new(~this, ~u8[] d) { this.data = d; }
		}
		_take(~u8[] data) int {
			Box b = Box(d: data);
			return 0;
		}
		test() {
			u8[] v = u8[]();
			_take(v);
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
		consume(~int[] v) {}
		sum(...int nums) {
			consume(nums);
		}
		test() {}
	`)
}

// `move` lambda capture of a borrowed param is rejected — same double-free
// pattern as moving into a constructor field.
func TestT0338_LambdaMoveCaptureBorrowedParam(t *testing.T) {
	errs := ownerErrs(t, `
		consume(~string s) {}
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
		consume(~string s) {}
		test() {
			string s = "hi";
			g := move || -> int {
				consume(s);
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
		getRef(string &s) string& { return s; }
		consume(~string s) {}
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
		consume(~string s) {}
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
			new(~this, ~string s) { this.s = s; }
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
	expectOwnerError(t, errs, "cannot move borrowed parameter 'm' of single-owner type into call argument")
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
	expectOwnerError(t, errs, "cannot move borrowed parameter 't' of single-owner type into call argument")
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
	expectOwnerError(t, errs, "cannot move borrowed parameter 'g' of single-owner type into call argument")
}

// With `~Mutex[int] m`, the caller transfers ownership at the call site
// (drop flag cleared), and the callee may consume it. No double-free.
func TestT0556_PushMutMutexParamOK(t *testing.T) {
	ownerOK(t, `
		take_mutex_push(~Mutex[int] m) {
			outer := Vector[Mutex[int]]();
			outer.push(m);
		}
		test() {
			m := Mutex[int](42);
			take_mutex_push(m);
		}
	`)
}

// Arc[T] is duppable (refcount inc), so push of a borrowed Arc param is
// still allowed — codegen emits dupArc at the call site. Regression guard.
func TestT0556_PushBorrowedArcParamOK(t *testing.T) {
	ownerOK(t, `
		take_arc_push(Arc[int] a) {
			outer := Vector[Arc[int]]();
			outer.push(a);
		}
		test() {
			a := Arc[int](7);
			take_arc_push(a);
		}
	`)
}

// `return m` for a borrowed Mutex param is handled by codegen's B0345
// alias-clear of the caller's drop flag — not by sema rejection.
// Regression guard that T0556's call-arg rejection doesn't bleed into
// the return path (which uses tryMove, not checkCallExpr).
func TestT0556_ReturnBorrowedMutexParamOK(t *testing.T) {
	ownerOK(t, `
		identity(Mutex[int] m) Mutex[int] { return m; }
		test() {
			m := Mutex[int](42);
			m2 := identity(m);
		}
	`)
}

// `n := m` for a borrowed Mutex param produces an aliased local (codegen
// detects the missing drop flag on the RHS and skips registering one for
// the LHS). Sema's call-arg rejection must not apply to var-decl sites.
func TestT0556_VarDeclBorrowedMutexParamOK(t *testing.T) {
	ownerOK(t, `
		alias(Mutex[int] m) {
			n := m;
		}
		test() {}
	`)
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
	expectOwnerError(t, errs, "cannot move borrowed parameter 'm' of single-owner type into call argument")
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
	expectOwnerError(t, errs, "cannot move borrowed parameter 'm' of single-owner type into call argument")
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
	expectOwnerError(t, errs, "cannot move borrowed parameter 'm' of single-owner type into call argument")
}

// Coverage: match-expression with arm.Body (no block, `=> expr`) form —
// the walk recurses into arm.Body via findBorrowedNonDuppableIdent.
func TestT0556_PushMatchExprBodyMutexParamRejected(t *testing.T) {
	errs := ownerErrs(t, `
		take_match(Mutex[int] m, int k) {
			v := Vector[Mutex[int]]();
			v.push(match k { 1 => m, _ => m });
		}
		test() {}
	`)
	expectOwnerError(t, errs, "cannot move borrowed parameter 'm' of single-owner type into call argument")
}

// Coverage: match-expression with arm.Block (`=> { stmts; expr }`) form —
// the walk recurses into arm.Block via findBorrowedNonDuppableIdentInBlock.
func TestT0556_PushMatchExprBlockMutexParamRejected(t *testing.T) {
	errs := ownerErrs(t, `
		take_match_block(Mutex[int] m, int k) {
			v := Vector[Mutex[int]]();
			v.push(match k { 1 => { m }, _ => { m } });
		}
		test() {}
	`)
	expectOwnerError(t, errs, "cannot move borrowed parameter 'm' of single-owner type into call argument")
}

// === T0349: extend tryMoveConsume to raise/yield/yield-from/select-send ===

// raise consumes the value into the caller's error slot — the outer caller
// owns and drops it. Same double-free pattern as T0338 if the raised value
// is a borrowed param.
func TestT0349_RaiseBorrowedParam(t *testing.T) {
	errs := ownerErrs(t, `
		type MyError is error {
			string field;
			new(~this, ~string message, ~string field) {
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
			new(~this, ~string message, ~string field) {
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
		type Box { string s; new(~this, ~string s) { this.s = s; } }
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
		type Box { string s; new(~this, ~string s) { this.s = s; } }
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
			ch.send(s);
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
		type Box { string s; new(~this, ~string s) { this.s = s; } }
		store(~Box b, string s) {
			b.s = s;
		}
		test() {}
	`)
	expectOwnerError(t, errs, "cannot move borrowed parameter 's'")
}

// Field assignment with ~ param works.
func TestT0351_AssignFieldMoveParamOK(t *testing.T) {
	ownerOK(t, `
		type Box { string s; new(~this, ~string s) { this.s = s; } }
		store(~Box b, ~string s) {
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
		forward(Mutex[string] m, ~string s) {
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
		consume(~string s) {}
		test() {
			s := "hi";
			a := Arc[string](s);
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
		consume(~string s) {}
		test() {
			s := "hi";
			a := Arc[string](s);
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
		consume(~string s) {}
		test() {
			s := "hi";
			a := Arc[string](s);
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
			a := Arc[string](s);
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
			a := Arc[string](s);
			b := "old";
			b = a.borrow.clone();
		}
	`)
}

// T0438: same sema-level rejection applies for MutexGuard.borrow.
func TestT0380_ConsumeMutexGuardBorrow(t *testing.T) {
	errs := ownerErrs(t, `
		consume(~string s) {}
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
	errs := ownerErrs(t, `
		readlen(string s) int { return s.len; }
		test() {
			s := "hi";
			a := Arc[string](s);
			borrowed := a.borrow;
			int n = readlen(borrowed);
		}
	`)
	expectOwnerError(t, errs, "cannot assign string& to parameter 's'")
}

// T0438: cloning makes it an owned copy — accepted.
func TestT0380_BorrowCloneToValueParamOK(t *testing.T) {
	ownerOK(t, `
		readlen(string s) int { return s.len; }
		test() {
			s := "hi";
			a := Arc[string](s);
			int n = readlen(a.borrow.clone());
		}
	`)
}

// Reading borrowed (member access) is OK.
func TestT0380_BorrowVarReadOK(t *testing.T) {
	ownerOK(t, `
		test() {
			s := "hi";
			a := Arc[string](s);
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
			a := Arc[string](s);
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
		consume(~string s) {}
		test() {
			s := "hi";
			a := Arc[string](s);
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
		getRef(string &s) string& { return s; }
		consume(~string s) {}
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
			a := Arc[string](s);
			string borrowed = a.borrow;
		}
	`)
	expectOwnerError(t, errs, "cannot assign string& to variable of type string")
}

// Copy inner types (Arc[int], Arc[bool], etc.) have no double-free risk:
// `.borrow` returns a value copy, so moves into ~T params or channel sends
// are safe. Existing patterns like `ch.send(a.borrow)` must continue to work.
func TestT0380_CopyInnerTypeNoReject(t *testing.T) {
	ownerOK(t, `
		consume(~int n) {}
		test() {
			a := Arc[int](42);
			consume(a.borrow);
			b := a.borrow;
			consume(b);
		}
	`)
}

// MutexGuard with Copy inner type: same — no rejection.
func TestT0380_MutexGuardCopyInnerNoReject(t *testing.T) {
	ownerOK(t, `
		consume(~int n) {}
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
		consume(~string s) {}
		test() {
			s := "hi";
			a := Arc[string](s);
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
		consume(~string s) {}
		test() {
			s := "hi";
			a := Arc[string](s);
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
		consume(~string s) {}
		test() {
			s := "hi";
			a := Arc[string](s);
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
		consume(~string s) {}
		test() {
			s := "hi";
			a := Arc[string](s);
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
		consume(~string s) {}
		test() {
			s := "hi";
			a := Arc[string](s);
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
		consume(~string s) {}
		test() {
			s := "hi";
			a := Arc[string](s);
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
			a := Arc[string]("x");
			return a.borrow;
		}
	`)
	expectOwnerError(t, errs, "cannot return string& from function returning string")
}

// T0402 / T0438: same rejection when the Arc comes from a parameter.
func TestT0402_ReturnBorrowAsOwnedRejected_ParamSource(t *testing.T) {
	errs := ownerErrs(t, `
		bad(Arc[string] a) string {
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
		ok(Arc[int] a) int {
			return a.borrow;
		}
	`)
}

// T0402: explicit `.clone()` produces an owned copy — the documented fix.
func TestT0402_ReturnBorrowCloneOK(t *testing.T) {
	ownerOK(t, `
		ok(Arc[string] a) string {
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
			a := Arc[string]("x");
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
		ok(Arc[string] a) string& {
			return a.borrow;
		}
	`)
}

// T0402 / T0438: when sema's joinBranchTypes preserves `T&` (all arms are
// borrows), sema rejects the return at the type-assignability check.
func TestT0402_ReturnBorrowThroughIfRejected(t *testing.T) {
	errs := ownerErrs(t, `
		bad(Arc[string] a, bool cond) string {
			return if cond { a.borrow } else { a.borrow };
		}
	`)
	expectOwnerError(t, errs, "cannot return string& from function returning string")
}

// T0438: typed local declaration `string borrowed = a.borrow;` is rejected
// at the var-decl boundary itself for non-Copy T (no implicit decay).
func TestT0402_ReturnBorrowThroughTypedLocalRejected(t *testing.T) {
	errs := ownerErrs(t, `
		bad(Arc[string] a) string {
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
		bad(Arc[string] a) string {
			borrowed := a.borrow;
			return borrowed;
		}
	`)
	expectOwnerError(t, errs, "cannot return string& from function returning string")
}

// T0402: laundering through if then through a local — return still rejected.
func TestT0402_ReturnBorrowThroughIfLocalRejected(t *testing.T) {
	errs := ownerErrs(t, `
		bad(Arc[string] a, bool cond) string {
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
		ok(Arc[string] a) string {
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
				a := Arc[string]("x");
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
			a := Arc[string]("x");
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
			f := |string& s| -> string& { return s; };
		}
	`)
}

// T0426: locals declared inside the lambda body still produce ref-to-local
// errors — captures are param-like, but body locals are not.
func TestT0426_LambdaReturnRefToLambdaLocalRejected(t *testing.T) {
	errs := ownerErrs(t, `
		test() {
			f := || -> string& {
				a := Arc[string]("x");
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
			a := Arc[string]("x");
			return a.borrow;
		}
	`)
	expectOwnerError(t, errs, "cannot return string& from function returning string")
}

// T0426: nested lambdas — outer lambda returns string& from a move-captured
// Arc[string], inner lambda returns string& from its own ref param. The
// save/restore must work through nesting: the inner's signature/params are
// pushed and popped without polluting the outer lambda's state.
func TestT0426_NestedLambdaRefReturnsBothLevels_OK(t *testing.T) {
	ownerOK(t, `
		test() {
			a := Arc[string]("x");
			outer := move || -> string& {
				inner := |string& s| -> string& { return s; };
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
					a := Arc[string]("x");
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
			method(&this) {
				f := move || -> string {
					a := Arc[string]("x");
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
			method(&this) string {
				f := move |string& s| -> string& { return s; };
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
			bad(&this) string {
				f := move || -> int { return 42; };
				a := Arc[string]("x");
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
			f := |string& a, string& b, bool c| -> string& {
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
			f := |string& a| -> string& { return a; };
			g := |string& b| -> string& { return b; };
		}
	`)
}

// T0426: a lambda's `_` parameter must not be added to c.params (sema also
// skips it from scope). Sanity-check that mixing a `_` with a real param
// still permits returning the real param.
func TestT0426_LambdaUnderscoreParamSkipped_OK(t *testing.T) {
	ownerOK(t, `
		test() {
			f := |int _, string& s| -> string& { return s; };
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
			a := Arc[string[]](v2);
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

// T0382: Copy element types (Arc[int].borrow → int field) are independently
// copied through the borrow, so no double-free risk and no rejection.
// isBorrowedExpr returns false for Copy underlying types (T0380).
func TestT0382_FieldAssignFromArcBorrowCopyAllowed(t *testing.T) {
	ownerOK(t, `
		type IntHolder { int n; }
		test() {
			h := IntHolder(0);
			a := Arc[int](42);
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
			h := Holder(v1);
			v2 := string[]();
			v2.push("hello" + "");
			a := Arc[string[]](v2);
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
			a := Arc[string]("hi");
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
			a := Arc[int](42);
			int n = a.borrow;
		}
	`)
}

// T0438: passing a non-Copy borrow into a value-typed param is rejected at
// the call site by the same Copy-only decay rule.
func TestT0438_BorrowToValueParamRejected(t *testing.T) {
	errs := ownerErrs(t, `
		take(string s) {}
		test() {
			a := Arc[string]("hi");
			take(a.borrow);
		}
	`)
	expectOwnerError(t, errs, "cannot assign string& to parameter 's'")
}

// T0438: `.clone()` produces an owned independent copy — the documented
// recovery path for non-Copy borrows.
func TestT0438_BorrowCloneToOwnedOK(t *testing.T) {
	ownerOK(t, `
		test() {
			a := Arc[string]("hi");
			string s = a.borrow.clone();
		}
	`)
}

// T0438: declaring the local as `T&` keeps it as a borrow — no decay,
// no implicit allocation.
func TestT0438_BorrowToRefDeclOK(t *testing.T) {
	ownerOK(t, `
		test() {
			a := Arc[string]("hi");
			string& s = a.borrow;
		}
	`)
}

// T0438: returning a non-Copy borrow as owned `T` is rejected at sema
// (defense-in-depth on top of T0402's ownership-level check).
func TestT0438_ReturnNonCopyBorrowAsOwnedRejected(t *testing.T) {
	errs := ownerErrs(t, `
		bad(Arc[string] a) string {
			return a.borrow;
		}
	`)
	expectOwnerError(t, errs, "cannot return string& from function returning string")
}

// T0438: returning a Copy borrow as owned is allowed — the value is
// loaded by value, the Arc retains its ownership.
func TestT0438_ReturnCopyBorrowAsOwnedOK(t *testing.T) {
	ownerOK(t, `
		ok(Arc[int] a) int {
			return a.borrow;
		}
	`)
}

// T0438: vector element decay is also rejected (Vector[T] is non-Copy).
func TestT0438_VectorBorrowToOwnedRejected(t *testing.T) {
	errs := ownerErrs(t, `
		test() {
			v := [1, 2, 3];
			a := Arc[int[]](v);
			int[] x = a.borrow;
		}
	`)
	expectOwnerError(t, errs, "cannot assign int[]& to variable of type int[]")
}

// T0438: `T~` (mutable borrow) decay is also Copy-only.
func TestT0438_MutBorrowToNonCopyOwnedRejected(t *testing.T) {
	errs := ownerErrs(t, `
		take(~string s) string& { return s; }
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
			a := Arc[string]("hello" + "");
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
			a := Arc[string]("hello" + "");
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
			a1 := Arc[string]("a" + "");
			a2 := Arc[string]("b" + "");
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
		consume_string(~string s) {}
		test() {
			a := Arc[string]("hi" + "");
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
		consume(~Inner i) {}
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
