package ast

import (
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"testing"

	"djabi.dev/go/promise_lang/internal/parser"
	antlr "github.com/antlr4-go/antlr/v4"
)

func parseAndBuild(t *testing.T, src string) *File {
	t.Helper()
	input := antlr.NewInputStream(src)
	lexer := parser.NewPromiseLexer(input)
	lexer.RemoveErrorListeners()
	stream := antlr.NewCommonTokenStream(lexer, antlr.TokenDefaultChannel)
	p := parser.NewPromiseParser(stream)
	p.RemoveErrorListeners()
	tree := p.CompilationUnit()
	file, errs := Build("test.pr", tree)
	if len(errs) > 0 {
		t.Fatalf("build errors: %v", errs)
	}
	if file == nil {
		t.Fatal("Build returned nil")
	}
	return file
}

func testdataDir(t *testing.T) string {
	t.Helper()
	_, filename, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(filename), "..", "..", "testdata")
}

// TestBuildValidFiles verifies all valid test fixtures build to AST without errors.
func TestBuildValidFiles(t *testing.T) {
	dir := filepath.Join(testdataDir(t), "valid")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading valid dir: %v", err)
	}
	for _, e := range entries {
		if filepath.Ext(e.Name()) != ".pr" {
			continue
		}
		t.Run(e.Name(), func(t *testing.T) {
			src, err := os.ReadFile(filepath.Join(dir, e.Name()))
			if err != nil {
				t.Fatal(err)
			}
			file := parseAndBuild(t, string(src))
			if file == nil {
				t.Fatal("nil file")
			}
		})
	}
}

// TestBuildRootFiles verifies root testdata files build to AST.
func TestBuildRootFiles(t *testing.T) {
	dir := testdataDir(t)
	for _, name := range []string{"hello.pr", "features.pr", "comprehensive.pr"} {
		t.Run(name, func(t *testing.T) {
			src, err := os.ReadFile(filepath.Join(dir, name))
			if err != nil {
				t.Fatal(err)
			}
			file := parseAndBuild(t, string(src))
			if file == nil {
				t.Fatal("nil file")
			}
		})
	}
}

// TestBuildFuncDecl verifies function declaration AST structure.
func TestBuildFuncDecl(t *testing.T) {
	tests := []struct {
		name  string
		src   string
		check func(t *testing.T, file *File)
	}{
		{
			name: "empty",
			src:  `f() {}`,
			check: func(t *testing.T, file *File) {
				assertLen(t, file.Decls, 1)
				fn := file.Decls[0].(*FuncDecl)
				assertEqual(t, fn.Name, "f")
				assertLen(t, fn.Params, 0)
				assertNil(t, fn.ReturnType)
			},
		},
		{
			name: "with_params_and_return",
			src:  `add(Int a, Int b) Int { return a + b; }`,
			check: func(t *testing.T, file *File) {
				fn := file.Decls[0].(*FuncDecl)
				assertEqual(t, fn.Name, "add")
				assertLen(t, fn.Params, 2)
				assertEqual(t, fn.Params[0].Name, "a")
				assertEqual(t, fn.Params[1].Name, "b")
				assertNotNil(t, fn.ReturnType)
				assertFalse(t, fn.ReturnType.CanError)
			},
		},
		{
			name: "error_return",
			src:  `read(String path) String! { return ""; }`,
			check: func(t *testing.T, file *File) {
				fn := file.Decls[0].(*FuncDecl)
				assertNotNil(t, fn.ReturnType)
				assertTrue(t, fn.ReturnType.CanError)
			},
		},
		{
			name: "generics",
			src:  `identity[T](T val) T { return val; }`,
			check: func(t *testing.T, file *File) {
				fn := file.Decls[0].(*FuncDecl)
				assertLen(t, fn.TypeParams, 1)
				assertEqual(t, fn.TypeParams[0].Name, "T")
			},
		},
		{
			name: "meta_annotation",
			src:  "f() `deprecated {}",
			check: func(t *testing.T, file *File) {
				fn := file.Decls[0].(*FuncDecl)
				assertLen(t, fn.Annotations, 1)
				assertEqual(t, fn.Annotations[0].Name, "deprecated")
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			file := parseAndBuild(t, tt.src)
			tt.check(t, file)
		})
	}
}

// TestBuildTypeDecl verifies type declaration AST structure.
func TestBuildTypeDecl(t *testing.T) {
	tests := []struct {
		name  string
		src   string
		check func(t *testing.T, file *File)
	}{
		{
			name: "simple",
			src:  `type Point { Float x; Float y; }`,
			check: func(t *testing.T, file *File) {
				td := file.Decls[0].(*TypeDecl)
				assertEqual(t, td.Name, "Point")
				assertLen(t, td.Fields, 2)
				assertEqual(t, td.Fields[0].Name, "x")
				assertEqual(t, td.Fields[1].Name, "y")
			},
		},
		{
			name: "with_methods",
			src:  `type Greeter { String name; greet(&this) String { return "hi"; } }`,
			check: func(t *testing.T, file *File) {
				td := file.Decls[0].(*TypeDecl)
				assertLen(t, td.Fields, 1)
				assertLen(t, td.Methods, 1)
				assertEqual(t, td.Methods[0].Name, "greet")
				assertNotNil(t, td.Methods[0].Receiver)
				assertEqual(t, td.Methods[0].Receiver.RefMod, RefShared)
			},
		},
		{
			name: "inheritance",
			src:  `type Dog is Animal { String breed; }`,
			check: func(t *testing.T, file *File) {
				td := file.Decls[0].(*TypeDecl)
				assertLen(t, td.Inherits, 1)
			},
		},
		{
			name: "generic",
			src:  `type Box[T] { T value; }`,
			check: func(t *testing.T, file *File) {
				td := file.Decls[0].(*TypeDecl)
				assertLen(t, td.TypeParams, 1)
				assertEqual(t, td.TypeParams[0].Name, "T")
			},
		},
		{
			name: "operator_overload",
			src:  `type Vec { +(&this, Vec other) Vec { return this; } }`,
			check: func(t *testing.T, file *File) {
				td := file.Decls[0].(*TypeDecl)
				assertLen(t, td.Methods, 1)
				assertEqual(t, td.Methods[0].Name, "+")
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			file := parseAndBuild(t, tt.src)
			tt.check(t, file)
		})
	}
}

// TestBuildEnumDecl verifies enum declaration AST structure.
func TestBuildEnumDecl(t *testing.T) {
	tests := []struct {
		name  string
		src   string
		check func(t *testing.T, file *File)
	}{
		{
			name: "simple",
			src:  `enum Dir { North, South, East, West }`,
			check: func(t *testing.T, file *File) {
				ed := file.Decls[0].(*EnumDecl)
				assertEqual(t, ed.Name, "Dir")
				assertLen(t, ed.Variants, 4)
				assertEqual(t, ed.Variants[0].Name, "North")
			},
		},
		{
			name: "with_fields",
			src:  `enum Option[T] { Some(T value), None }`,
			check: func(t *testing.T, file *File) {
				ed := file.Decls[0].(*EnumDecl)
				assertLen(t, ed.TypeParams, 1)
				assertLen(t, ed.Variants, 2)
				assertLen(t, ed.Variants[0].Fields, 1)
				assertEqual(t, ed.Variants[0].Fields[0].Name, "value")
				assertLen(t, ed.Variants[1].Fields, 0)
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			file := parseAndBuild(t, tt.src)
			tt.check(t, file)
		})
	}
}

// TestBuildUseDecl verifies use declaration AST structure.
func TestBuildUseDecl(t *testing.T) {
	file := parseAndBuild(t, `use io "std/io";`)
	assertLen(t, file.Uses, 1)
	assertEqual(t, file.Uses[0].Alias, "io")
	assertEqual(t, file.Uses[0].Path, "std/io")
}

// TestBuildStatements verifies statement AST structure.
func TestBuildStatements(t *testing.T) {
	tests := []struct {
		name  string
		src   string
		check func(t *testing.T, file *File)
	}{
		{
			name: "typed_var",
			src:  `f() { Int x = 42; }`,
			check: func(t *testing.T, file *File) {
				fn := file.Decls[0].(*FuncDecl)
				vd := fn.Body.Stmts[0].(*TypedVarDecl)
				assertEqual(t, vd.Name, "x")
				assertNotNil(t, vd.Value)
			},
		},
		{
			name: "inferred_var",
			src:  `f() { x := 42; }`,
			check: func(t *testing.T, file *File) {
				fn := file.Decls[0].(*FuncDecl)
				vd := fn.Body.Stmts[0].(*InferredVarDecl)
				assertEqual(t, vd.Name, "x")
			},
		},
		{
			name: "destructure",
			src:  `f() { (a, b) := pair(); }`,
			check: func(t *testing.T, file *File) {
				fn := file.Decls[0].(*FuncDecl)
				vd := fn.Body.Stmts[0].(*DestructureVarDecl)
				assertLen(t, vd.Names, 2)
				assertEqual(t, vd.Names[0], "a")
				assertEqual(t, vd.Names[1], "b")
			},
		},
		{
			name: "assignment",
			src:  `f() { x += 1; }`,
			check: func(t *testing.T, file *File) {
				fn := file.Decls[0].(*FuncDecl)
				as := fn.Body.Stmts[0].(*AssignStmt)
				assertEqual(t, as.Op, OpAddAssign)
			},
		},
		{
			name: "return_value",
			src:  `f() { return 42; }`,
			check: func(t *testing.T, file *File) {
				fn := file.Decls[0].(*FuncDecl)
				rs := fn.Body.Stmts[0].(*ReturnStmt)
				assertNotNil(t, rs.Value)
			},
		},
		{
			name: "return_bare",
			src:  `f() { return; }`,
			check: func(t *testing.T, file *File) {
				fn := file.Decls[0].(*FuncDecl)
				rs := fn.Body.Stmts[0].(*ReturnStmt)
				assertNil(t, rs.Value)
			},
		},
		{
			name: "break_continue",
			src:  `f() { for { break; continue; } }`,
			check: func(t *testing.T, file *File) {
				fn := file.Decls[0].(*FuncDecl)
				loop := fn.Body.Stmts[0].(*InfiniteLoop)
				_ = loop.Body.Stmts[0].(*BreakStmt)
				_ = loop.Body.Stmts[1].(*ContinueStmt)
			},
		},
		{
			name: "yield",
			src:  `f() { yield 1; }`,
			check: func(t *testing.T, file *File) {
				fn := file.Decls[0].(*FuncDecl)
				_ = fn.Body.Stmts[0].(*YieldStmt)
			},
		},
		{
			name: "yield_delegate",
			src:  `f() { yield* items; }`,
			check: func(t *testing.T, file *File) {
				fn := file.Decls[0].(*FuncDecl)
				_ = fn.Body.Stmts[0].(*YieldDelegateStmt)
			},
		},
		{
			name: "raise",
			src:  `f() { raise err; }`,
			check: func(t *testing.T, file *File) {
				fn := file.Decls[0].(*FuncDecl)
				_ = fn.Body.Stmts[0].(*RaiseStmt)
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			file := parseAndBuild(t, tt.src)
			tt.check(t, file)
		})
	}
}

// TestBuildControlFlow verifies control flow statement AST structure.
func TestBuildControlFlow(t *testing.T) {
	tests := []struct {
		name  string
		src   string
		check func(t *testing.T, file *File)
	}{
		{
			name: "if_simple",
			src:  `f() { if x > 0 { a(); } }`,
			check: func(t *testing.T, file *File) {
				fn := file.Decls[0].(*FuncDecl)
				is := fn.Body.Stmts[0].(*IfStmt)
				assertNotNil(t, is.Cond)
				assertNil(t, is.Else)
			},
		},
		{
			name: "if_else",
			src:  `f() { if x > 0 { a(); } else { b(); } }`,
			check: func(t *testing.T, file *File) {
				fn := file.Decls[0].(*FuncDecl)
				is := fn.Body.Stmts[0].(*IfStmt)
				assertNotNil(t, is.Else)
				_ = is.Else.(*Block)
			},
		},
		{
			name: "if_else_if",
			src:  `f() { if x > 0 { a(); } else if x < 0 { b(); } else { c(); } }`,
			check: func(t *testing.T, file *File) {
				fn := file.Decls[0].(*FuncDecl)
				is := fn.Body.Stmts[0].(*IfStmt)
				elseIf := is.Else.(*IfStmt)
				assertNotNil(t, elseIf.Else)
			},
		},
		{
			name: "if_unwrap",
			src:  `f() { if val := maybe { consume(val); } }`,
			check: func(t *testing.T, file *File) {
				fn := file.Decls[0].(*FuncDecl)
				is := fn.Body.Stmts[0].(*IfStmt)
				assertEqual(t, is.Binding, "val")
				assertNotNil(t, is.Init)
				assertNil(t, is.Cond)
			},
		},
		{
			name: "for_in",
			src:  `f() { for x in items { consume(x); } }`,
			check: func(t *testing.T, file *File) {
				fn := file.Decls[0].(*FuncDecl)
				fi := fn.Body.Stmts[0].(*ForInStmt)
				assertEqual(t, fi.Binding, "x")
				assertEqual(t, fi.Index, "")
			},
		},
		{
			name: "for_in_index",
			src:  `f() { for i, x in items { consume(i, x); } }`,
			check: func(t *testing.T, file *File) {
				fn := file.Decls[0].(*FuncDecl)
				fi := fn.Body.Stmts[0].(*ForInStmt)
				assertEqual(t, fi.Index, "i")
				assertEqual(t, fi.Binding, "x")
			},
		},
		{
			name: "for_classic",
			src:  `f() { for Int i = 0; i < 10; i += 1 { consume(i); } }`,
			check: func(t *testing.T, file *File) {
				fn := file.Decls[0].(*FuncDecl)
				cf := fn.Body.Stmts[0].(*ClassicForStmt)
				assertEqual(t, cf.InitName, "i")
				assertNotNil(t, cf.InitType)
				assertEqual(t, cf.UpdateOp, OpAddAssign)
			},
		},
		{
			name: "for_inferred",
			src:  `f() { for i := 0; i < 10; i += 1 { consume(i); } }`,
			check: func(t *testing.T, file *File) {
				fn := file.Decls[0].(*FuncDecl)
				cf := fn.Body.Stmts[0].(*ClassicForStmt)
				assertNil(t, cf.InitType)
			},
		},
		{
			name: "infinite_loop",
			src:  `f() { for { break; } }`,
			check: func(t *testing.T, file *File) {
				fn := file.Decls[0].(*FuncDecl)
				_ = fn.Body.Stmts[0].(*InfiniteLoop)
			},
		},
		{
			name: "while",
			src:  `f() { while x < 10 { x += 1; } }`,
			check: func(t *testing.T, file *File) {
				fn := file.Decls[0].(*FuncDecl)
				ws := fn.Body.Stmts[0].(*WhileStmt)
				assertNotNil(t, ws.Cond)
			},
		},
		{
			name: "while_unwrap",
			src:  `f() { while val := next() { consume(val); } }`,
			check: func(t *testing.T, file *File) {
				fn := file.Decls[0].(*FuncDecl)
				wu := fn.Body.Stmts[0].(*WhileUnwrapStmt)
				assertEqual(t, wu.Binding, "val")
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			file := parseAndBuild(t, tt.src)
			tt.check(t, file)
		})
	}
}

// TestBuildExpressions verifies expression AST structure.
func TestBuildExpressions(t *testing.T) {
	tests := []struct {
		name  string
		src   string
		check func(t *testing.T, file *File)
	}{
		{
			name: "binary_add",
			src:  `f() { x := 1 + 2; }`,
			check: func(t *testing.T, file *File) {
				fn := file.Decls[0].(*FuncDecl)
				vd := fn.Body.Stmts[0].(*InferredVarDecl)
				be := vd.Value.(*BinaryExpr)
				assertEqual(t, be.Op, BinAdd)
			},
		},
		{
			name: "precedence",
			src:  `f() { x := 1 + 2 * 3; }`,
			check: func(t *testing.T, file *File) {
				fn := file.Decls[0].(*FuncDecl)
				vd := fn.Body.Stmts[0].(*InferredVarDecl)
				be := vd.Value.(*BinaryExpr)
				assertEqual(t, be.Op, BinAdd)
				rhs := be.Right.(*BinaryExpr)
				assertEqual(t, rhs.Op, BinMul)
			},
		},
		{
			name: "unary_neg",
			src:  `f() { x := -y; }`,
			check: func(t *testing.T, file *File) {
				fn := file.Decls[0].(*FuncDecl)
				vd := fn.Body.Stmts[0].(*InferredVarDecl)
				ue := vd.Value.(*UnaryExpr)
				assertEqual(t, ue.Op, UnaryNeg)
			},
		},
		{
			name: "call",
			src:  `f() { g(1, 2); }`,
			check: func(t *testing.T, file *File) {
				fn := file.Decls[0].(*FuncDecl)
				es := fn.Body.Stmts[0].(*ExprStmt)
				ce := es.Expr.(*CallExpr)
				assertLen(t, ce.Args, 2)
			},
		},
		{
			name: "named_args",
			src:  `f() { g(x: 1, y: 2); }`,
			check: func(t *testing.T, file *File) {
				fn := file.Decls[0].(*FuncDecl)
				es := fn.Body.Stmts[0].(*ExprStmt)
				ce := es.Expr.(*CallExpr)
				assertEqual(t, ce.Args[0].Name, "x")
				assertEqual(t, ce.Args[1].Name, "y")
			},
		},
		{
			name: "member_access",
			src:  `f() { x := a.b; }`,
			check: func(t *testing.T, file *File) {
				fn := file.Decls[0].(*FuncDecl)
				vd := fn.Body.Stmts[0].(*InferredVarDecl)
				me := vd.Value.(*MemberExpr)
				assertEqual(t, me.Field, "b")
			},
		},
		{
			name: "optional_chain",
			src:  `f() { x := a?.b; }`,
			check: func(t *testing.T, file *File) {
				fn := file.Decls[0].(*FuncDecl)
				vd := fn.Body.Stmts[0].(*InferredVarDecl)
				oc := vd.Value.(*OptionalChainExpr)
				assertEqual(t, oc.Field, "b")
			},
		},
		{
			name: "index",
			src:  `f() { x := a[0]; }`,
			check: func(t *testing.T, file *File) {
				fn := file.Decls[0].(*FuncDecl)
				vd := fn.Body.Stmts[0].(*InferredVarDecl)
				_ = vd.Value.(*IndexExpr)
			},
		},
		{
			name: "is_expr",
			src:  `f() { x := a is Circle; }`,
			check: func(t *testing.T, file *File) {
				fn := file.Decls[0].(*FuncDecl)
				vd := fn.Body.Stmts[0].(*InferredVarDecl)
				ie := vd.Value.(*IsExpr)
				ip := ie.Pattern.(*IdentIsPattern)
				assertEqual(t, ip.Name, "Circle")
			},
		},
		{
			name: "cast",
			src:  `f() { x := a as Int; }`,
			check: func(t *testing.T, file *File) {
				fn := file.Decls[0].(*FuncDecl)
				vd := fn.Body.Stmts[0].(*InferredVarDecl)
				ce := vd.Value.(*CastExpr)
				assertFalse(t, ce.Force)
			},
		},
		{
			name: "force_cast",
			src:  `f() { x := a as! Int; }`,
			check: func(t *testing.T, file *File) {
				fn := file.Decls[0].(*FuncDecl)
				vd := fn.Body.Stmts[0].(*InferredVarDecl)
				ce := vd.Value.(*CastExpr)
				assertTrue(t, ce.Force)
			},
		},
		{
			name: "error_propagate",
			src:  `f() { x := getValue()?; }`,
			check: func(t *testing.T, file *File) {
				fn := file.Decls[0].(*FuncDecl)
				vd := fn.Body.Stmts[0].(*InferredVarDecl)
				_ = vd.Value.(*ErrorPropagateExpr)
			},
		},
		{
			name: "error_unwrap",
			src:  `f() { x := getValue()!; }`,
			check: func(t *testing.T, file *File) {
				fn := file.Decls[0].(*FuncDecl)
				vd := fn.Body.Stmts[0].(*InferredVarDecl)
				_ = vd.Value.(*ErrorUnwrapExpr)
			},
		},
		{
			name: "error_handler",
			src:  `f() { x := getValue() ? e { handleErr(e); }; }`,
			check: func(t *testing.T, file *File) {
				fn := file.Decls[0].(*FuncDecl)
				vd := fn.Body.Stmts[0].(*InferredVarDecl)
				eh := vd.Value.(*ErrorHandlerExpr)
				assertEqual(t, eh.Binding, "e")
			},
		},
		{
			name: "range",
			src:  `f() { for x in 0..10 { consume(x); } }`,
			check: func(t *testing.T, file *File) {
				fn := file.Decls[0].(*FuncDecl)
				fi := fn.Body.Stmts[0].(*ForInStmt)
				be := fi.Iterable.(*BinaryExpr)
				assertEqual(t, be.Op, BinExclusiveRange)
			},
		},
		{
			name: "receive",
			src:  `f() { x := <-task; }`,
			check: func(t *testing.T, file *File) {
				fn := file.Decls[0].(*FuncDecl)
				vd := fn.Body.Stmts[0].(*InferredVarDecl)
				ue := vd.Value.(*UnaryExpr)
				assertEqual(t, ue.Op, UnaryReceive)
			},
		},
		{
			name: "if_expr",
			src:  `f() { x := if a > 0 { 1 } else { 0 }; }`,
			check: func(t *testing.T, file *File) {
				fn := file.Decls[0].(*FuncDecl)
				vd := fn.Body.Stmts[0].(*InferredVarDecl)
				_ = vd.Value.(*IfExpr)
			},
		},
		{
			name: "match_expr",
			src:  `f() { match x { 0 => a(), _ => b(), } }`,
			check: func(t *testing.T, file *File) {
				fn := file.Decls[0].(*FuncDecl)
				es := fn.Body.Stmts[0].(*ExprStmt)
				me := es.Expr.(*MatchExpr)
				assertLen(t, me.Arms, 2)
			},
		},
		{
			name: "go_expr",
			src:  `f() { x := go compute(); }`,
			check: func(t *testing.T, file *File) {
				fn := file.Decls[0].(*FuncDecl)
				vd := fn.Body.Stmts[0].(*InferredVarDecl)
				ge := vd.Value.(*GoExpr)
				assertNotNil(t, ge.Expr)
				assertNil(t, ge.Block)
			},
		},
		{
			name: "unsafe_block",
			src:  `f() { unsafe { x(); } }`,
			check: func(t *testing.T, file *File) {
				fn := file.Decls[0].(*FuncDecl)
				es := fn.Body.Stmts[0].(*ExprStmt)
				_ = es.Expr.(*UnsafeExpr)
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			file := parseAndBuild(t, tt.src)
			tt.check(t, file)
		})
	}
}

// TestBuildLiterals verifies literal expression AST structure.
func TestBuildLiterals(t *testing.T) {
	tests := []struct {
		name  string
		src   string
		check func(t *testing.T, file *File)
	}{
		{
			name: "int",
			src:  `f() { x := 42; }`,
			check: func(t *testing.T, file *File) {
				fn := file.Decls[0].(*FuncDecl)
				vd := fn.Body.Stmts[0].(*InferredVarDecl)
				il := vd.Value.(*IntLit)
				assertEqual(t, il.Raw, "42")
			},
		},
		{
			name: "float",
			src:  `f() { x := 3.14; }`,
			check: func(t *testing.T, file *File) {
				fn := file.Decls[0].(*FuncDecl)
				vd := fn.Body.Stmts[0].(*InferredVarDecl)
				fl := vd.Value.(*FloatLit)
				assertEqual(t, fl.Raw, "3.14")
			},
		},
		{
			name: "true",
			src:  `f() { x := true; }`,
			check: func(t *testing.T, file *File) {
				fn := file.Decls[0].(*FuncDecl)
				vd := fn.Body.Stmts[0].(*InferredVarDecl)
				bl := vd.Value.(*BoolLit)
				assertTrue(t, bl.Value)
			},
		},
		{
			name: "false",
			src:  `f() { x := false; }`,
			check: func(t *testing.T, file *File) {
				fn := file.Decls[0].(*FuncDecl)
				vd := fn.Body.Stmts[0].(*InferredVarDecl)
				bl := vd.Value.(*BoolLit)
				assertFalse(t, bl.Value)
			},
		},
		{
			name: "none",
			src:  `f() { x := none; }`,
			check: func(t *testing.T, file *File) {
				fn := file.Decls[0].(*FuncDecl)
				vd := fn.Body.Stmts[0].(*InferredVarDecl)
				_ = vd.Value.(*NoneLit)
			},
		},
		{
			name: "array",
			src:  `f() { x := [1, 2, 3]; }`,
			check: func(t *testing.T, file *File) {
				fn := file.Decls[0].(*FuncDecl)
				vd := fn.Body.Stmts[0].(*InferredVarDecl)
				al := vd.Value.(*ArrayLit)
				assertLen(t, al.Elements, 3)
			},
		},
		{
			name: "tuple",
			src:  `f() { x := (1, 2); }`,
			check: func(t *testing.T, file *File) {
				fn := file.Decls[0].(*FuncDecl)
				vd := fn.Body.Stmts[0].(*InferredVarDecl)
				tl := vd.Value.(*TupleLit)
				assertLen(t, tl.Elements, 2)
			},
		},
		{
			name: "map",
			src:  `f() { x := {"a": 1, "b": 2}; }`,
			check: func(t *testing.T, file *File) {
				fn := file.Decls[0].(*FuncDecl)
				vd := fn.Body.Stmts[0].(*InferredVarDecl)
				ml := vd.Value.(*MapLit)
				assertLen(t, ml.Entries, 2)
			},
		},
		{
			name: "this",
			src:  `type T { f(&this) { return this; } }`,
			check: func(t *testing.T, file *File) {
				td := file.Decls[0].(*TypeDecl)
				rs := td.Methods[0].Body.Stmts[0].(*ReturnStmt)
				_ = rs.Value.(*ThisExpr)
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			file := parseAndBuild(t, tt.src)
			tt.check(t, file)
		})
	}
}

// TestBuildLambda verifies lambda expression AST structure.
func TestBuildLambda(t *testing.T) {
	tests := []struct {
		name  string
		src   string
		check func(t *testing.T, file *File)
	}{
		{
			name: "expr_body",
			src:  `f() { x := |a, b| -> a + b; }`,
			check: func(t *testing.T, file *File) {
				fn := file.Decls[0].(*FuncDecl)
				vd := fn.Body.Stmts[0].(*InferredVarDecl)
				le := vd.Value.(*LambdaExpr)
				assertLen(t, le.Params, 2)
				assertNotNil(t, le.ExprBody)
				assertNil(t, le.Body)
				assertFalse(t, le.Move)
			},
		},
		{
			name: "block_body",
			src:  `f() { x := |a| { return a; }; }`,
			check: func(t *testing.T, file *File) {
				fn := file.Decls[0].(*FuncDecl)
				vd := fn.Body.Stmts[0].(*InferredVarDecl)
				le := vd.Value.(*LambdaExpr)
				assertLen(t, le.Params, 1)
				assertNotNil(t, le.Body)
				assertNil(t, le.ExprBody)
			},
		},
		{
			name: "no_params",
			src:  `f() { x := || -> 42; }`,
			check: func(t *testing.T, file *File) {
				fn := file.Decls[0].(*FuncDecl)
				vd := fn.Body.Stmts[0].(*InferredVarDecl)
				le := vd.Value.(*LambdaExpr)
				assertLen(t, le.Params, 0)
			},
		},
		{
			name: "move",
			src:  `f() { x := move |a| -> a; }`,
			check: func(t *testing.T, file *File) {
				fn := file.Decls[0].(*FuncDecl)
				vd := fn.Body.Stmts[0].(*InferredVarDecl)
				le := vd.Value.(*LambdaExpr)
				assertTrue(t, le.Move)
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			file := parseAndBuild(t, tt.src)
			tt.check(t, file)
		})
	}
}

// TestBuildTypeRefs verifies type reference AST structure.
func TestBuildTypeRefs(t *testing.T) {
	tests := []struct {
		name  string
		src   string
		check func(t *testing.T, file *File)
	}{
		{
			name: "named",
			src:  `f() { Int x = 0; }`,
			check: func(t *testing.T, file *File) {
				fn := file.Decls[0].(*FuncDecl)
				vd := fn.Body.Stmts[0].(*TypedVarDecl)
				nt := vd.Type.(*NamedTypeRef)
				assertEqual(t, nt.Name, "Int")
			},
		},
		{
			name: "generic",
			src:  `f() { List[Int] x = []; }`,
			check: func(t *testing.T, file *File) {
				fn := file.Decls[0].(*FuncDecl)
				vd := fn.Body.Stmts[0].(*TypedVarDecl)
				nt := vd.Type.(*NamedTypeRef)
				assertEqual(t, nt.Name, "List")
				assertLen(t, nt.TypeArgs, 1)
			},
		},
		{
			name: "slice",
			src:  `f() { Int[] x = []; }`,
			check: func(t *testing.T, file *File) {
				fn := file.Decls[0].(*FuncDecl)
				vd := fn.Body.Stmts[0].(*TypedVarDecl)
				st := vd.Type.(*SliceTypeRef)
				nt := st.Element.(*NamedTypeRef)
				assertEqual(t, nt.Name, "Int")
			},
		},
		{
			name: "array",
			src:  `f() { Int[10] x = []; }`,
			check: func(t *testing.T, file *File) {
				fn := file.Decls[0].(*FuncDecl)
				vd := fn.Body.Stmts[0].(*TypedVarDecl)
				at := vd.Type.(*ArrayTypeRef)
				assertEqual(t, at.Size, "10")
			},
		},
		{
			name: "optional",
			src:  `f() { Int? x = none; }`,
			check: func(t *testing.T, file *File) {
				fn := file.Decls[0].(*FuncDecl)
				vd := fn.Body.Stmts[0].(*TypedVarDecl)
				_ = vd.Type.(*OptionalTypeRef)
			},
		},
		{
			name: "shared_ref",
			src:  `f(Int& x) {}`,
			check: func(t *testing.T, file *File) {
				fn := file.Decls[0].(*FuncDecl)
				srt := fn.Params[0].Type.(*SharedRefTypeRef)
				nt := srt.Inner.(*NamedTypeRef)
				assertEqual(t, nt.Name, "Int")
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			file := parseAndBuild(t, tt.src)
			tt.check(t, file)
		})
	}
}

// TestBuildMatchPatterns verifies match pattern AST structure.
func TestBuildMatchPatterns(t *testing.T) {
	tests := []struct {
		name  string
		src   string
		check func(t *testing.T, file *File)
	}{
		{
			name: "int_literal",
			src:  `f() { match x { 0 => a(), _ => b(), } }`,
			check: func(t *testing.T, file *File) {
				fn := file.Decls[0].(*FuncDecl)
				es := fn.Body.Stmts[0].(*ExprStmt)
				me := es.Expr.(*MatchExpr)
				_ = me.Arms[0].Pattern.(*LiteralMatchPattern)
				_ = me.Arms[1].Pattern.(*WildcardMatchPattern)
			},
		},
		{
			name: "enum_variant",
			src:  `f() { match d { Dir.North => a(), _ => b(), } }`,
			check: func(t *testing.T, file *File) {
				fn := file.Decls[0].(*FuncDecl)
				es := fn.Body.Stmts[0].(*ExprStmt)
				me := es.Expr.(*MatchExpr)
				ev := me.Arms[0].Pattern.(*EnumVariantMatchPattern)
				assertEqual(t, ev.Enum, "Dir")
				assertEqual(t, ev.Variant, "North")
			},
		},
		{
			name: "enum_destructure",
			src:  `f() { match c { Color.Custom(r, g, b) => a(), _ => b(), } }`,
			check: func(t *testing.T, file *File) {
				fn := file.Decls[0].(*FuncDecl)
				es := fn.Body.Stmts[0].(*ExprStmt)
				me := es.Expr.(*MatchExpr)
				ed := me.Arms[0].Pattern.(*EnumDestructureMatchPattern)
				assertEqual(t, ed.Enum, "Color")
				assertEqual(t, ed.Variant, "Custom")
				assertLen(t, ed.Bindings, 3)
			},
		},
		{
			name: "short_destructure",
			src:  `f() { match r { Ok(val) => consume(val), Err(e) => handle(e), } }`,
			check: func(t *testing.T, file *File) {
				fn := file.Decls[0].(*FuncDecl)
				es := fn.Body.Stmts[0].(*ExprStmt)
				me := es.Expr.(*MatchExpr)
				sd := me.Arms[0].Pattern.(*ShortDestructureMatchPattern)
				assertEqual(t, sd.Name, "Ok")
				assertLen(t, sd.Bindings, 1)
			},
		},
		{
			name: "with_guard",
			src:  `f() { match n { x if x > 0 => big(), _ => small(), } }`,
			check: func(t *testing.T, file *File) {
				fn := file.Decls[0].(*FuncDecl)
				es := fn.Body.Stmts[0].(*ExprStmt)
				me := es.Expr.(*MatchExpr)
				assertNotNil(t, me.Arms[0].Guard)
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			file := parseAndBuild(t, tt.src)
			tt.check(t, file)
		})
	}
}

// TestBuildPosition verifies source position tracking.
func TestBuildPosition(t *testing.T) {
	file := parseAndBuild(t, `f() { return 42; }`)
	fn := file.Decls[0].(*FuncDecl)
	pos := fn.Pos()
	assertEqual(t, pos.File, "test.pr")
	assertEqual(t, pos.Line, 1)
}

// Test helpers

func assertEqual[T comparable](t *testing.T, got, want T) {
	t.Helper()
	if got != want {
		t.Errorf("got %v, want %v", got, want)
	}
}

func assertTrue(t *testing.T, b bool) {
	t.Helper()
	if !b {
		t.Error("expected true, got false")
	}
}

func assertFalse(t *testing.T, b bool) {
	t.Helper()
	if b {
		t.Error("expected false, got true")
	}
}

func assertNil(t *testing.T, v interface{}) {
	t.Helper()
	if v != nil {
		// Handle typed nil pointers wrapped in interfaces
		rv := reflect.ValueOf(v)
		if rv.Kind() == reflect.Ptr && rv.IsNil() {
			return
		}
		t.Errorf("expected nil, got %v", v)
	}
}

func assertNotNil(t *testing.T, v interface{}) {
	t.Helper()
	if v == nil {
		t.Error("expected non-nil, got nil")
		return
	}
	// Handle typed nil pointers wrapped in interfaces
	rv := reflect.ValueOf(v)
	if rv.Kind() == reflect.Ptr && rv.IsNil() {
		t.Error("expected non-nil, got typed nil")
	}
}

func assertLen[T any](t *testing.T, slice []T, want int) {
	t.Helper()
	if len(slice) != want {
		t.Errorf("got len %d, want %d", len(slice), want)
	}
}
