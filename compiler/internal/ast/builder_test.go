package ast

import (
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
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
			src:  `read!(String path) String { return ""; }`,
			check: func(t *testing.T, file *File) {
				fn := file.Decls[0].(*FuncDecl)
				assertNotNil(t, fn.ReturnType)
				assertTrue(t, fn.ReturnType.CanError)
			},
		},
		{
			name: "bang_shorthand_void_failable",
			src:  `fail!() { raise error("boom"); }`,
			check: func(t *testing.T, file *File) {
				fn := file.Decls[0].(*FuncDecl)
				assertNotNil(t, fn.ReturnType)
				assertTrue(t, fn.ReturnType.CanError)
				assertNil(t, fn.ReturnType.Type)
			},
		},
		{
			name: "new_syntax_failable_with_return",
			src:  `read!(String path) String { return ""; }`,
			check: func(t *testing.T, file *File) {
				fn := file.Decls[0].(*FuncDecl)
				assertEqual(t, fn.Name, "read")
				assertNotNil(t, fn.ReturnType)
				assertTrue(t, fn.ReturnType.CanError)
				assertNotNil(t, fn.ReturnType.Type)
			},
		},
		{
			name: "new_syntax_void_failable",
			src:  `fail!() { raise error("boom"); }`,
			check: func(t *testing.T, file *File) {
				fn := file.Decls[0].(*FuncDecl)
				assertNotNil(t, fn.ReturnType)
				assertTrue(t, fn.ReturnType.CanError)
				assertNil(t, fn.ReturnType.Type)
			},
		},
		{
			name: "new_syntax_generic_failable",
			src:  `parse![T](String s) T { return s; }`,
			check: func(t *testing.T, file *File) {
				fn := file.Decls[0].(*FuncDecl)
				assertLen(t, fn.TypeParams, 1)
				assertEqual(t, fn.TypeParams[0].Name, "T")
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

// TestOldFailableSyntaxRejected verifies old failable syntax produces errors.
func TestOldFailableSyntaxRejected(t *testing.T) {
	tests := []struct {
		name string
		src  string
		msg  string
	}{
		{
			name: "func_bare_bang",
			src:  `fail() ! { }`,
			msg:  "use 'fail!(...)' instead of 'fail(...) !'",
		},
		{
			name: "func_return_type_bang",
			src:  `read(string path) string! { return ""; }`,
			msg:  "use 'read!(...)' instead of 'read(...) !'",
		},
		{
			name: "method_return_type_bang",
			src:  `type Foo { speak(&this) string! { return "hi"; } }`,
			msg:  "use 'speak!(...)' instead of 'speak(...) !'",
		},
		{
			name: "method_bare_bang",
			src:  `type Foo { fail(&this)! { } }`,
			msg:  "use 'fail!(...)' instead of 'fail(...) !'",
		},
		{
			name: "getter_type_bang",
			src:  `type Foo { get value int! => 42; }`,
			msg:  "use 'get value! Type' instead of 'get value Type!'",
		},
		{
			name: "top_level_getter_type_bang",
			src:  `get name string! => "hi";`,
			msg:  "use 'get name! Type' instead of 'get name Type!'",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			input := antlr.NewInputStream(tt.src)
			lexer := parser.NewPromiseLexer(input)
			lexer.RemoveErrorListeners()
			stream := antlr.NewCommonTokenStream(lexer, antlr.TokenDefaultChannel)
			p := parser.NewPromiseParser(stream)
			p.RemoveErrorListeners()
			tree := p.CompilationUnit()
			_, errs := Build("test.pr", tree)
			if len(errs) == 0 {
				t.Fatal("expected error for old failable syntax, got none")
			}
			found := false
			for _, err := range errs {
				if strings.Contains(err.Error(), tt.msg) {
					found = true
					break
				}
			}
			if !found {
				t.Errorf("expected error containing %q, got: %v", tt.msg, errs)
			}
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
	// Sourced import (existing form)
	file := parseAndBuild(t, `use io "std/io";`)
	assertLen(t, file.Uses, 1)
	assertEqual(t, file.Uses[0].Alias, "io")
	assertEqual(t, file.Uses[0].Path, "std/io")
	assertEqual(t, file.Uses[0].CatalogName, "")

	// Catalog import (bare name)
	file = parseAndBuild(t, `use json;`)
	assertLen(t, file.Uses, 1)
	assertEqual(t, file.Uses[0].Alias, "json")
	assertEqual(t, file.Uses[0].CatalogName, "json")
	assertEqual(t, file.Uses[0].Path, "")

	// Catalog import with alias
	file = parseAndBuild(t, `use json as j;`)
	assertLen(t, file.Uses, 1)
	assertEqual(t, file.Uses[0].Alias, "j")
	assertEqual(t, file.Uses[0].CatalogName, "json")
	assertEqual(t, file.Uses[0].Path, "")

	// Catalog import with glob (as _)
	file = parseAndBuild(t, `use json as _;`)
	assertLen(t, file.Uses, 1)
	assertEqual(t, file.Uses[0].Alias, "_")
	assertEqual(t, file.Uses[0].CatalogName, "json")
	assertEqual(t, file.Uses[0].Path, "")

	// Sourced import with glob (use _ "path")
	file = parseAndBuild(t, `use _ "./libs/models";`)
	assertLen(t, file.Uses, 1)
	assertEqual(t, file.Uses[0].Alias, "_")
	assertEqual(t, file.Uses[0].Path, "./libs/models")
	assertEqual(t, file.Uses[0].CatalogName, "")
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
			src:  `f() { x := getValue()?^; }`,
			check: func(t *testing.T, file *File) {
				fn := file.Decls[0].(*FuncDecl)
				vd := fn.Body.Stmts[0].(*InferredVarDecl)
				_ = vd.Value.(*ErrorPropagateExpr)
			},
		},
		{
			name: "error_panic",
			src:  `f() { x := getValue()?!; }`,
			check: func(t *testing.T, file *File) {
				fn := file.Decls[0].(*FuncDecl)
				vd := fn.Body.Stmts[0].(*InferredVarDecl)
				_ = vd.Value.(*ErrorPanicExpr)
			},
		},
		{
			name: "optional_unwrap",
			src:  `f() { x := getValue()!; }`,
			check: func(t *testing.T, file *File) {
				fn := file.Decls[0].(*FuncDecl)
				vd := fn.Body.Stmts[0].(*InferredVarDecl)
				_ = vd.Value.(*OptionalUnwrapExpr)
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
		{
			name: "nested_block",
			src:  `f() { x := 1; { x = 2; } }`,
			check: func(t *testing.T, file *File) {
				fn := file.Decls[0].(*FuncDecl)
				assertLen(t, fn.Body.Stmts, 2)
				_ = fn.Body.Stmts[0].(*InferredVarDecl)
				blk := fn.Body.Stmts[1].(*Block)
				assertLen(t, blk.Stmts, 1)
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

// TestBuildBinaryOperators verifies all binary operator variants.
func TestBuildBinaryOperators(t *testing.T) {
	tests := []struct {
		name string
		src  string
		op   BinaryOp
	}{
		{"add", `f() { x := a + b; }`, BinAdd},
		{"sub", `f() { x := a - b; }`, BinSub},
		{"mul", `f() { x := a * b; }`, BinMul},
		{"div", `f() { x := a / b; }`, BinDiv},
		{"mod", `f() { x := a % b; }`, BinMod},
		{"eq", `f() { x := a == b; }`, BinEq},
		{"neq", `f() { x := a != b; }`, BinNeq},
		{"lt", `f() { x := a < b; }`, BinLt},
		{"gt", `f() { x := a > b; }`, BinGt},
		{"lte", `f() { x := a <= b; }`, BinLte},
		{"gte", `f() { x := a >= b; }`, BinGte},
		{"and", `f() { x := a && b; }`, BinAnd},
		{"or", `f() { x := a || b; }`, BinOr},
		{"elvis", `f() { x := a ?: b; }`, BinElvis},
		{"exclusive_range", `f() { x := a..b; }`, BinExclusiveRange},
		{"inclusive_range", `f() { x := a..=b; }`, BinInclusiveRange},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			file := parseAndBuild(t, tt.src)
			fn := file.Decls[0].(*FuncDecl)
			vd := fn.Body.Stmts[0].(*InferredVarDecl)
			be := vd.Value.(*BinaryExpr)
			assertEqual(t, be.Op, tt.op)
		})
	}
}

// TestBuildUnaryOperators verifies all unary operator variants.
func TestBuildUnaryOperators(t *testing.T) {
	tests := []struct {
		name string
		src  string
		op   UnaryOp
	}{
		{"neg", `f() { x := -a; }`, UnaryNeg},
		{"not", `f() { x := !a; }`, UnaryNot},
		{"receive", `f() { x := <-a; }`, UnaryReceive},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			file := parseAndBuild(t, tt.src)
			fn := file.Decls[0].(*FuncDecl)
			vd := fn.Body.Stmts[0].(*InferredVarDecl)
			ue := vd.Value.(*UnaryExpr)
			assertEqual(t, ue.Op, tt.op)
		})
	}
}

// TestBuildAssignOperators verifies all assignment operator variants.
func TestBuildAssignOperators(t *testing.T) {
	tests := []struct {
		name string
		src  string
		op   AssignOp
	}{
		{"assign", `f() { x = 1; }`, OpAssign},
		{"add_assign", `f() { x += 1; }`, OpAddAssign},
		{"sub_assign", `f() { x -= 1; }`, OpSubAssign},
		{"mul_assign", `f() { x *= 1; }`, OpMulAssign},
		{"div_assign", `f() { x /= 1; }`, OpDivAssign},
		{"mod_assign", `f() { x %= 1; }`, OpModAssign},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			file := parseAndBuild(t, tt.src)
			fn := file.Decls[0].(*FuncDecl)
			as := fn.Body.Stmts[0].(*AssignStmt)
			assertEqual(t, as.Op, tt.op)
		})
	}
}

// TestBuildPrecedence verifies operator precedence in various combinations.
func TestBuildPrecedence(t *testing.T) {
	tests := []struct {
		name    string
		src     string
		rootOp  BinaryOp
		leftOp  BinaryOp // -1 means left is not a BinaryExpr
		rightOp BinaryOp // -1 means right is not a BinaryExpr
	}{
		{
			name:    "mul_before_add",
			src:     `f() { x := 1 + 2 * 3; }`,
			rootOp:  BinAdd,
			leftOp:  -1,
			rightOp: BinMul,
		},
		{
			name:    "div_before_sub",
			src:     `f() { x := a - b / c; }`,
			rootOp:  BinSub,
			leftOp:  -1,
			rightOp: BinDiv,
		},
		{
			name:    "add_before_comparison",
			src:     `f() { x := a + b < c + d; }`,
			rootOp:  BinLt,
			leftOp:  BinAdd,
			rightOp: BinAdd,
		},
		{
			name:    "comparison_before_equality",
			src:     `f() { x := a < b == c > d; }`,
			rootOp:  BinEq,
			leftOp:  BinLt,
			rightOp: BinGt,
		},
		{
			name:    "equality_before_and",
			src:     `f() { x := a == b && c != d; }`,
			rootOp:  BinAnd,
			leftOp:  BinEq,
			rightOp: BinNeq,
		},
		{
			name:    "and_before_or",
			src:     `f() { x := a && b || c && d; }`,
			rootOp:  BinOr,
			leftOp:  BinAnd,
			rightOp: BinAnd,
		},
		{
			name:    "or_before_elvis",
			src:     `f() { x := a || b ?: c || d; }`,
			rootOp:  BinElvis,
			leftOp:  BinOr,
			rightOp: BinOr,
		},
		{
			name:    "add_before_range",
			src:     `f() { x := a + b..c + d; }`,
			rootOp:  BinExclusiveRange,
			leftOp:  BinAdd,
			rightOp: BinAdd,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			file := parseAndBuild(t, tt.src)
			fn := file.Decls[0].(*FuncDecl)
			vd := fn.Body.Stmts[0].(*InferredVarDecl)
			be := vd.Value.(*BinaryExpr)
			assertEqual(t, be.Op, tt.rootOp)
			if tt.leftOp >= 0 {
				lbe := be.Left.(*BinaryExpr)
				assertEqual(t, lbe.Op, tt.leftOp)
			}
			if tt.rightOp >= 0 {
				rbe := be.Right.(*BinaryExpr)
				assertEqual(t, rbe.Op, tt.rightOp)
			}
		})
	}
}

// TestBuildStringLiterals verifies all string literal variants.
func TestBuildStringLiterals(t *testing.T) {
	tests := []struct {
		name  string
		src   string
		check func(t *testing.T, sl *StringLit)
	}{
		{
			name: "regular",
			src:  `f() { x := "hello"; }`,
			check: func(t *testing.T, sl *StringLit) {
				assertEqual(t, sl.Kind, StringRegular)
				assertTrue(t, len(sl.Parts) > 0)
			},
		},
		{
			name: "raw",
			src:  `f() { x := r"no\escapes"; }`,
			check: func(t *testing.T, sl *StringLit) {
				assertEqual(t, sl.Kind, StringRaw)
			},
		},
		{
			name: "triple",
			src:  "f() { x := \"\"\"multi\nline\"\"\"; }",
			check: func(t *testing.T, sl *StringLit) {
				assertEqual(t, sl.Kind, StringTriple)
			},
		},
		{
			name: "raw_always_has_raw_text",
			src:  `f() { x := "hello"; }`,
			check: func(t *testing.T, sl *StringLit) {
				assertTrue(t, sl.Raw != "")
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			file := parseAndBuild(t, tt.src)
			fn := file.Decls[0].(*FuncDecl)
			vd := fn.Body.Stmts[0].(*InferredVarDecl)
			sl := vd.Value.(*StringLit)
			tt.check(t, sl)
		})
	}
}

// TestBuildCharLiteral verifies character literal AST structure.
func TestBuildCharLiteral(t *testing.T) {
	file := parseAndBuild(t, `f() { x := 'a'; }`)
	fn := file.Decls[0].(*FuncDecl)
	vd := fn.Body.Stmts[0].(*InferredVarDecl)
	cl := vd.Value.(*CharLit)
	assertEqual(t, cl.Raw, "'a'")
}

// TestBuildIdentExpr verifies identifier expression AST structure.
func TestBuildIdentExpr(t *testing.T) {
	file := parseAndBuild(t, `f() { x := foo; }`)
	fn := file.Decls[0].(*FuncDecl)
	vd := fn.Body.Stmts[0].(*InferredVarDecl)
	id := vd.Value.(*IdentExpr)
	assertEqual(t, id.Name, "foo")
}

// TestBuildParenExpr verifies parenthesized expression AST structure.
func TestBuildParenExpr(t *testing.T) {
	file := parseAndBuild(t, `f() { x := (a + b); }`)
	fn := file.Decls[0].(*FuncDecl)
	vd := fn.Body.Stmts[0].(*InferredVarDecl)
	pe := vd.Value.(*ParenExpr)
	be := pe.Expr.(*BinaryExpr)
	assertEqual(t, be.Op, BinAdd)
}

// TestBuildTypeRefsExtended verifies remaining type reference variants.
func TestBuildTypeRefsExtended(t *testing.T) {
	tests := []struct {
		name  string
		src   string
		check func(t *testing.T, file *File)
	}{
		{
			name: "tuple_type",
			src:  `f() { (Int, String) x = (1, "a"); }`,
			check: func(t *testing.T, file *File) {
				fn := file.Decls[0].(*FuncDecl)
				vd := fn.Body.Stmts[0].(*TypedVarDecl)
				tt := vd.Type.(*TupleTypeRef)
				assertLen(t, tt.Elements, 2)
				e0 := tt.Elements[0].(*NamedTypeRef)
				assertEqual(t, e0.Name, "Int")
				e1 := tt.Elements[1].(*NamedTypeRef)
				assertEqual(t, e1.Name, "String")
			},
		},
		{
			name: "function_type",
			src:  `f() { (Int, Int) -> Bool x = |a, b| -> a > b; }`,
			check: func(t *testing.T, file *File) {
				fn := file.Decls[0].(*FuncDecl)
				vd := fn.Body.Stmts[0].(*TypedVarDecl)
				ft := vd.Type.(*FunctionTypeRef)
				assertLen(t, ft.Params, 2)
				ret := ft.Return.(*NamedTypeRef)
				assertEqual(t, ret.Name, "Bool")
			},
		},
		{
			name: "mut_ref",
			src:  `f(Int~ x) {}`,
			check: func(t *testing.T, file *File) {
				fn := file.Decls[0].(*FuncDecl)
				mrt := fn.Params[0].Type.(*MutRefTypeRef)
				nt := mrt.Inner.(*NamedTypeRef)
				assertEqual(t, nt.Name, "Int")
			},
		},
		{
			name: "pointer",
			src:  `f(Int* x) {}`,
			check: func(t *testing.T, file *File) {
				fn := file.Decls[0].(*FuncDecl)
				pt := fn.Params[0].Type.(*PointerTypeRef)
				nt := pt.Inner.(*NamedTypeRef)
				assertEqual(t, nt.Name, "Int")
			},
		},
		{
			name: "multi_type_args",
			src:  `f() { map[String, Int] x = {"key": 1}; }`,
			check: func(t *testing.T, file *File) {
				fn := file.Decls[0].(*FuncDecl)
				vd := fn.Body.Stmts[0].(*TypedVarDecl)
				nt := vd.Type.(*NamedTypeRef)
				assertEqual(t, nt.Name, "map")
				assertLen(t, nt.TypeArgs, 2)
			},
		},
		{
			name: "optional_slice",
			src:  `f() { Int[]? x = none; }`,
			check: func(t *testing.T, file *File) {
				fn := file.Decls[0].(*FuncDecl)
				vd := fn.Body.Stmts[0].(*TypedVarDecl)
				opt := vd.Type.(*OptionalTypeRef)
				_ = opt.Inner.(*SliceTypeRef)
			},
		},
		{
			name: "nested_generic",
			src:  `f() { List[List[Int]] x = []; }`,
			check: func(t *testing.T, file *File) {
				fn := file.Decls[0].(*FuncDecl)
				vd := fn.Body.Stmts[0].(*TypedVarDecl)
				outer := vd.Type.(*NamedTypeRef)
				assertEqual(t, outer.Name, "List")
				assertLen(t, outer.TypeArgs, 1)
				inner := outer.TypeArgs[0].(*NamedTypeRef)
				assertEqual(t, inner.Name, "List")
				assertLen(t, inner.TypeArgs, 1)
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

// TestBuildDeclDetails verifies declaration details not covered by basic tests.
func TestBuildDeclDetails(t *testing.T) {
	tests := []struct {
		name  string
		src   string
		check func(t *testing.T, file *File)
	}{
		{
			name: "field_default",
			src:  `type Config { Int timeout = 30; }`,
			check: func(t *testing.T, file *File) {
				td := file.Decls[0].(*TypeDecl)
				assertLen(t, td.Fields, 1)
				assertEqual(t, td.Fields[0].Name, "timeout")
				assertNotNil(t, td.Fields[0].Default)
				il := td.Fields[0].Default.(*IntLit)
				assertEqual(t, il.Raw, "30")
			},
		},
		{
			name: "field_annotations",
			src:  "type T { Int x `deprecated; }",
			check: func(t *testing.T, file *File) {
				td := file.Decls[0].(*TypeDecl)
				assertLen(t, td.Fields[0].Annotations, 1)
				assertEqual(t, td.Fields[0].Annotations[0].Name, "deprecated")
			},
		},
		{
			name: "abstract_method",
			src:  `type Shape { area(&this) Float; }`,
			check: func(t *testing.T, file *File) {
				td := file.Decls[0].(*TypeDecl)
				assertLen(t, td.Methods, 1)
				assertEqual(t, td.Methods[0].Name, "area")
				assertNil(t, td.Methods[0].Body)
			},
		},
		{
			name: "receiver_mut",
			src:  `type T { reset(~this) { } }`,
			check: func(t *testing.T, file *File) {
				td := file.Decls[0].(*TypeDecl)
				assertNotNil(t, td.Methods[0].Receiver)
				assertEqual(t, td.Methods[0].Receiver.RefMod, RefMut)
			},
		},
		{
			name: "receiver_none",
			src:  `type T { info(this) String { return ""; } }`,
			check: func(t *testing.T, file *File) {
				td := file.Decls[0].(*TypeDecl)
				assertNotNil(t, td.Methods[0].Receiver)
				assertEqual(t, td.Methods[0].Receiver.RefMod, RefNone)
			},
		},
		{
			name: "type_constraint_single",
			src:  `f[T: Comparable](T a, T b) Bool { return a == b; }`,
			check: func(t *testing.T, file *File) {
				fn := file.Decls[0].(*FuncDecl)
				assertLen(t, fn.TypeParams, 1)
				assertEqual(t, fn.TypeParams[0].Name, "T")
				assertLen(t, fn.TypeParams[0].Constraint, 1)
				c := fn.TypeParams[0].Constraint[0].(*NamedTypeRef)
				assertEqual(t, c.Name, "Comparable")
			},
		},
		{
			name: "type_constraint_multi",
			src:  `f[T: Hashable + Comparable](T x) {}`,
			check: func(t *testing.T, file *File) {
				fn := file.Decls[0].(*FuncDecl)
				assertLen(t, fn.TypeParams[0].Constraint, 2)
			},
		},
		{
			name: "param_default",
			src:  `f(Int x = 0) {}`,
			check: func(t *testing.T, file *File) {
				fn := file.Decls[0].(*FuncDecl)
				assertLen(t, fn.Params, 1)
				assertEqual(t, fn.Params[0].Name, "x")
				assertNotNil(t, fn.Params[0].Default)
			},
		},
		{
			name: "param_discard",
			src:  `f(Int _) {}`,
			check: func(t *testing.T, file *File) {
				fn := file.Decls[0].(*FuncDecl)
				assertEqual(t, fn.Params[0].Name, "_")
			},
		},
		{
			name: "meta_annotation_named_params",
			src:  "f() `deprecated(since: \"1.0\", reason: \"use g\") {}",
			check: func(t *testing.T, file *File) {
				fn := file.Decls[0].(*FuncDecl)
				assertLen(t, fn.Annotations, 1)
				a := fn.Annotations[0]
				assertEqual(t, a.Name, "deprecated")
				assertLen(t, a.Params, 2)
				assertEqual(t, a.Params[0].Name, "since")
				assertEqual(t, a.Params[1].Name, "reason")
			},
		},
		{
			name: "meta_annotation_positional",
			src:  "f() `doc(\"Does nothing\") {}",
			check: func(t *testing.T, file *File) {
				fn := file.Decls[0].(*FuncDecl)
				a := fn.Annotations[0]
				assertEqual(t, a.Name, "doc")
				assertLen(t, a.Params, 1)
				assertEqual(t, a.Params[0].Name, "")
			},
		},
		{
			name: "multiple_annotations",
			src:  "f() `deprecated `inline {}",
			check: func(t *testing.T, file *File) {
				fn := file.Decls[0].(*FuncDecl)
				assertLen(t, fn.Annotations, 2)
				assertEqual(t, fn.Annotations[0].Name, "deprecated")
				assertEqual(t, fn.Annotations[1].Name, "inline")
			},
		},
		{
			name: "enum_annotations",
			src:  "enum Color `serializable { Red, Green, Blue }",
			check: func(t *testing.T, file *File) {
				ed := file.Decls[0].(*EnumDecl)
				assertLen(t, ed.Annotations, 1)
				assertEqual(t, ed.Annotations[0].Name, "serializable")
			},
		},
		{
			name: "param_annotation_doc",
			src:  "f(int x `doc(\"must be positive\")) {}",
			check: func(t *testing.T, file *File) {
				fn := file.Decls[0].(*FuncDecl)
				assertLen(t, fn.Params, 1)
				assertLen(t, fn.Params[0].Annotations, 1)
				assertEqual(t, fn.Params[0].Annotations[0].Name, "doc")
				assertLen(t, fn.Params[0].Annotations[0].Params, 1)
				assertEqual(t, fn.Params[0].Annotations[0].Params[0].Name, "")
			},
		},
		{
			name: "param_annotation_no_args",
			src:  "f(int x `deprecated) {}",
			check: func(t *testing.T, file *File) {
				fn := file.Decls[0].(*FuncDecl)
				assertLen(t, fn.Params[0].Annotations, 1)
				assertEqual(t, fn.Params[0].Annotations[0].Name, "deprecated")
				assertLen(t, fn.Params[0].Annotations[0].Params, 0)
			},
		},
		{
			name: "param_annotation_multiple_params",
			src:  "f(int a `doc(\"first\"), int b `doc(\"second\")) {}",
			check: func(t *testing.T, file *File) {
				fn := file.Decls[0].(*FuncDecl)
				assertLen(t, fn.Params, 2)
				assertLen(t, fn.Params[0].Annotations, 1)
				assertEqual(t, fn.Params[0].Annotations[0].Name, "doc")
				assertLen(t, fn.Params[1].Annotations, 1)
				assertEqual(t, fn.Params[1].Annotations[0].Name, "doc")
			},
		},
		{
			name: "param_annotation_with_default",
			src:  "f(int x `doc(\"the value\") = 42) {}",
			check: func(t *testing.T, file *File) {
				fn := file.Decls[0].(*FuncDecl)
				p := fn.Params[0]
				assertLen(t, p.Annotations, 1)
				assertEqual(t, p.Annotations[0].Name, "doc")
				assertNotNil(t, p.Default)
			},
		},
		{
			name: "param_annotation_method",
			src:  "type T { foo(&this, int x `doc(\"param\")) {} }",
			check: func(t *testing.T, file *File) {
				td := file.Decls[0].(*TypeDecl)
				m := td.Methods[0]
				assertLen(t, m.Params, 1)
				assertLen(t, m.Params[0].Annotations, 1)
				assertEqual(t, m.Params[0].Annotations[0].Name, "doc")
			},
		},
		{
			name: "multi_inherit",
			src:  `type Hybrid is Flyable, Swimmable { }`,
			check: func(t *testing.T, file *File) {
				td := file.Decls[0].(*TypeDecl)
				assertLen(t, td.Inherits, 2)
			},
		},
		{
			name: "method_type_params",
			src:  `type T { convert[U](&this) U { return this as! U; } }`,
			check: func(t *testing.T, file *File) {
				td := file.Decls[0].(*TypeDecl)
				assertLen(t, td.Methods[0].TypeParams, 1)
				assertEqual(t, td.Methods[0].TypeParams[0].Name, "U")
			},
		},
		{
			name: "multiple_use_decls",
			src:  `use io "std/io"; use fmt "std/fmt"; f() {}`,
			check: func(t *testing.T, file *File) {
				assertLen(t, file.Uses, 2)
				assertEqual(t, file.Uses[0].Alias, "io")
				assertEqual(t, file.Uses[0].Path, "std/io")
				assertEqual(t, file.Uses[1].Alias, "fmt")
				assertEqual(t, file.Uses[1].Path, "std/fmt")
			},
		},
		{
			name: "all_operator_overloads",
			src: `type V {
				+(&this, V o) V { return this; }
				-(&this, V o) V { return this; }
				*(&this, V o) V { return this; }
				/(&this, V o) V { return this; }
				%(&this, V o) V { return this; }
				==(&this, V o) Bool { return true; }
				!=(&this, V o) Bool { return false; }
				<(&this, V o) Bool { return false; }
				>(&this, V o) Bool { return false; }
				<=(&this, V o) Bool { return false; }
				>=(&this, V o) Bool { return false; }
			}`,
			check: func(t *testing.T, file *File) {
				td := file.Decls[0].(*TypeDecl)
				assertLen(t, td.Methods, 11)
				names := []string{"+", "-", "*", "/", "%", "==", "!=", "<", ">", "<=", ">="}
				for i, want := range names {
					assertEqual(t, td.Methods[i].Name, want)
				}
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

// TestBuildPatternsExtended verifies pattern types not covered by basic tests.
func TestBuildPatternsExtended(t *testing.T) {
	tests := []struct {
		name  string
		src   string
		check func(t *testing.T, file *File)
	}{
		{
			name: "type_binding",
			src:  `f() { match shape { Circle c => consume(c), _ => none, } }`,
			check: func(t *testing.T, file *File) {
				fn := file.Decls[0].(*FuncDecl)
				es := fn.Body.Stmts[0].(*ExprStmt)
				me := es.Expr.(*MatchExpr)
				tb := me.Arms[0].Pattern.(*TypeBindingMatchPattern)
				assertEqual(t, tb.TypeName, "Circle")
				assertEqual(t, tb.Binding, "c")
			},
		},
		{
			name: "name_pattern",
			src:  `f() { match x { val => consume(val), } }`,
			check: func(t *testing.T, file *File) {
				fn := file.Decls[0].(*FuncDecl)
				es := fn.Body.Stmts[0].(*ExprStmt)
				me := es.Expr.(*MatchExpr)
				np := me.Arms[0].Pattern.(*NameMatchPattern)
				assertEqual(t, np.Name, "val")
			},
		},
		{
			name: "bool_literal_pattern",
			src:  `f() { match b { true => a(), false => b(), } }`,
			check: func(t *testing.T, file *File) {
				fn := file.Decls[0].(*FuncDecl)
				es := fn.Body.Stmts[0].(*ExprStmt)
				me := es.Expr.(*MatchExpr)
				lp0 := me.Arms[0].Pattern.(*LiteralMatchPattern)
				bl0 := lp0.Value.(*BoolLit)
				assertTrue(t, bl0.Value)
				lp1 := me.Arms[1].Pattern.(*LiteralMatchPattern)
				bl1 := lp1.Value.(*BoolLit)
				assertFalse(t, bl1.Value)
			},
		},
		{
			name: "none_literal_pattern",
			src:  `f() { match opt { none => a(), _ => b(), } }`,
			check: func(t *testing.T, file *File) {
				fn := file.Decls[0].(*FuncDecl)
				es := fn.Body.Stmts[0].(*ExprStmt)
				me := es.Expr.(*MatchExpr)
				lp := me.Arms[0].Pattern.(*LiteralMatchPattern)
				_ = lp.Value.(*NoneLit)
			},
		},
		{
			name: "string_literal_pattern",
			src:  `f() { match s { "hello" => a(), _ => b(), } }`,
			check: func(t *testing.T, file *File) {
				fn := file.Decls[0].(*FuncDecl)
				es := fn.Body.Stmts[0].(*ExprStmt)
				me := es.Expr.(*MatchExpr)
				lp := me.Arms[0].Pattern.(*LiteralMatchPattern)
				_ = lp.Value.(*StringLit)
			},
		},
		{
			name: "float_literal_pattern",
			src:  `f() { match x { 3.14 => a(), _ => b(), } }`,
			check: func(t *testing.T, file *File) {
				fn := file.Decls[0].(*FuncDecl)
				es := fn.Body.Stmts[0].(*ExprStmt)
				me := es.Expr.(*MatchExpr)
				lp := me.Arms[0].Pattern.(*LiteralMatchPattern)
				fl := lp.Value.(*FloatLit)
				assertEqual(t, fl.Raw, "3.14")
			},
		},
		{
			name: "is_destructure",
			src:  `f() { x := val is Some(inner); }`,
			check: func(t *testing.T, file *File) {
				fn := file.Decls[0].(*FuncDecl)
				vd := fn.Body.Stmts[0].(*InferredVarDecl)
				ie := vd.Value.(*IsExpr)
				dp := ie.Pattern.(*DestructureIsPattern)
				assertEqual(t, dp.TypeName, "Some")
				assertLen(t, dp.Bindings, 1)
				assertEqual(t, dp.Bindings[0], "inner")
			},
		},
		{
			name: "match_with_block_body",
			src:  `f() { match x { 0 => { a(); b(); }, _ => c(), } }`,
			check: func(t *testing.T, file *File) {
				fn := file.Decls[0].(*FuncDecl)
				es := fn.Body.Stmts[0].(*ExprStmt)
				me := es.Expr.(*MatchExpr)
				arm := me.Arms[0]
				assertNotNil(t, arm.Block)
				assertNil(t, arm.Body)
				assertLen(t, arm.Block.Stmts, 2)
			},
		},
		{
			name: "match_guard_with_block",
			src:  `f() { match x { n if n > 0 => { positive(); }, _ => negative(), } }`,
			check: func(t *testing.T, file *File) {
				fn := file.Decls[0].(*FuncDecl)
				es := fn.Body.Stmts[0].(*ExprStmt)
				me := es.Expr.(*MatchExpr)
				arm := me.Arms[0]
				assertNotNil(t, arm.Guard)
				assertNotNil(t, arm.Block)
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

// TestBuildLambdaExtended verifies additional lambda variants.
func TestBuildLambdaExtended(t *testing.T) {
	tests := []struct {
		name  string
		src   string
		check func(t *testing.T, file *File)
	}{
		{
			name: "typed_params",
			src:  `f() { x := |Int a, Int b| -> a + b; }`,
			check: func(t *testing.T, file *File) {
				fn := file.Decls[0].(*FuncDecl)
				vd := fn.Body.Stmts[0].(*InferredVarDecl)
				le := vd.Value.(*LambdaExpr)
				assertLen(t, le.Params, 2)
				assertNotNil(t, le.Params[0].Type)
				nt := le.Params[0].Type.(*NamedTypeRef)
				assertEqual(t, nt.Name, "Int")
				assertEqual(t, le.Params[0].Name, "a")
			},
		},
		{
			name: "with_return_type",
			src:  `f() { x := |Int a| -> Int { return a; }; }`,
			check: func(t *testing.T, file *File) {
				fn := file.Decls[0].(*FuncDecl)
				vd := fn.Body.Stmts[0].(*InferredVarDecl)
				le := vd.Value.(*LambdaExpr)
				assertNotNil(t, le.ReturnType)
				rt := le.ReturnType.(*NamedTypeRef)
				assertEqual(t, rt.Name, "Int")
				assertNotNil(t, le.Body)
			},
		},
		{
			name: "no_params_block",
			src:  `f() { x := || { return 42; }; }`,
			check: func(t *testing.T, file *File) {
				fn := file.Decls[0].(*FuncDecl)
				vd := fn.Body.Stmts[0].(*InferredVarDecl)
				le := vd.Value.(*LambdaExpr)
				assertLen(t, le.Params, 0)
				assertNotNil(t, le.Body)
				assertNil(t, le.ExprBody)
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

// TestBuildGoExprExtended verifies go expression variants.
func TestBuildGoExprExtended(t *testing.T) {
	tests := []struct {
		name  string
		src   string
		check func(t *testing.T, file *File)
	}{
		{
			name: "go_expression",
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
			name: "go_block",
			src:  `f() { x := go { compute(); }; }`,
			check: func(t *testing.T, file *File) {
				fn := file.Decls[0].(*FuncDecl)
				vd := fn.Body.Stmts[0].(*InferredVarDecl)
				ge := vd.Value.(*GoExpr)
				assertNil(t, ge.Expr)
				assertNotNil(t, ge.Block)
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

// TestBuildChainedExpressions verifies chained member access and calls.
func TestBuildChainedExpressions(t *testing.T) {
	tests := []struct {
		name  string
		src   string
		check func(t *testing.T, file *File)
	}{
		{
			name: "chained_member_access",
			src:  `f() { x := a.b.c; }`,
			check: func(t *testing.T, file *File) {
				fn := file.Decls[0].(*FuncDecl)
				vd := fn.Body.Stmts[0].(*InferredVarDecl)
				// a.b.c → MemberExpr(.c, MemberExpr(.b, a))
				outer := vd.Value.(*MemberExpr)
				assertEqual(t, outer.Field, "c")
				inner := outer.Target.(*MemberExpr)
				assertEqual(t, inner.Field, "b")
				id := inner.Target.(*IdentExpr)
				assertEqual(t, id.Name, "a")
			},
		},
		{
			name: "method_chain",
			src:  `f() { x := a.foo().bar().baz(); }`,
			check: func(t *testing.T, file *File) {
				fn := file.Decls[0].(*FuncDecl)
				vd := fn.Body.Stmts[0].(*InferredVarDecl)
				// a.foo().bar().baz() → Call(Member(.baz, Call(Member(.bar, Call(Member(.foo, a))))))
				c3 := vd.Value.(*CallExpr)
				m3 := c3.Callee.(*MemberExpr)
				assertEqual(t, m3.Field, "baz")
				c2 := m3.Target.(*CallExpr)
				m2 := c2.Callee.(*MemberExpr)
				assertEqual(t, m2.Field, "bar")
				c1 := m2.Target.(*CallExpr)
				m1 := c1.Callee.(*MemberExpr)
				assertEqual(t, m1.Field, "foo")
				id := m1.Target.(*IdentExpr)
				assertEqual(t, id.Name, "a")
			},
		},
		{
			name: "call_with_index",
			src:  `f() { x := items[0].name; }`,
			check: func(t *testing.T, file *File) {
				fn := file.Decls[0].(*FuncDecl)
				vd := fn.Body.Stmts[0].(*InferredVarDecl)
				me := vd.Value.(*MemberExpr)
				assertEqual(t, me.Field, "name")
				ie := me.Target.(*IndexExpr)
				id := ie.Target.(*IdentExpr)
				assertEqual(t, id.Name, "items")
			},
		},
		{
			name: "optional_chain_mixed",
			src:  `f() { x := a?.b.c; }`,
			check: func(t *testing.T, file *File) {
				fn := file.Decls[0].(*FuncDecl)
				vd := fn.Body.Stmts[0].(*InferredVarDecl)
				outer := vd.Value.(*MemberExpr)
				assertEqual(t, outer.Field, "c")
				inner := outer.Target.(*OptionalChainExpr)
				assertEqual(t, inner.Field, "b")
			},
		},
		{
			name: "optional_chain_call",
			src:  `f() { x := getValue()?.process(); }`,
			check: func(t *testing.T, file *File) {
				fn := file.Decls[0].(*FuncDecl)
				vd := fn.Body.Stmts[0].(*InferredVarDecl)
				call := vd.Value.(*CallExpr)
				oc := call.Callee.(*OptionalChainExpr)
				assertEqual(t, oc.Field, "process")
				inner := oc.Target.(*CallExpr)
				callee := inner.Callee.(*IdentExpr)
				assertEqual(t, callee.Name, "getValue")
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

// TestBuildErrorHandling verifies error handling expression variants.
func TestBuildErrorHandling(t *testing.T) {
	tests := []struct {
		name  string
		src   string
		check func(t *testing.T, file *File)
	}{
		{
			name: "handler_with_binding",
			src:  `f() { x := getValue() ? e { log(e); }; }`,
			check: func(t *testing.T, file *File) {
				fn := file.Decls[0].(*FuncDecl)
				vd := fn.Body.Stmts[0].(*InferredVarDecl)
				eh := vd.Value.(*ErrorHandlerExpr)
				assertEqual(t, eh.Binding, "e")
				assertNotNil(t, eh.Body)
			},
		},
		{
			name: "handler_with_discard",
			src:  `f() { x := getValue() ? _ { fallback(); }; }`,
			check: func(t *testing.T, file *File) {
				fn := file.Decls[0].(*FuncDecl)
				vd := fn.Body.Stmts[0].(*InferredVarDecl)
				eh := vd.Value.(*ErrorHandlerExpr)
				assertEqual(t, eh.Binding, "_")
			},
		},
		{
			name: "handler_no_binding",
			src:  `f() { x := getValue() ? { fallback(); }; }`,
			check: func(t *testing.T, file *File) {
				fn := file.Decls[0].(*FuncDecl)
				vd := fn.Body.Stmts[0].(*InferredVarDecl)
				eh := vd.Value.(*ErrorHandlerExpr)
				assertEqual(t, eh.Binding, "")
			},
		},
		{
			name: "propagate_then_panic",
			src:  `f() { x := a()?^; y := b()?!; }`,
			check: func(t *testing.T, file *File) {
				fn := file.Decls[0].(*FuncDecl)
				vd1 := fn.Body.Stmts[0].(*InferredVarDecl)
				_ = vd1.Value.(*ErrorPropagateExpr)
				vd2 := fn.Body.Stmts[1].(*InferredVarDecl)
				_ = vd2.Value.(*ErrorPanicExpr)
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

// TestBuildEmptyCollections verifies empty collection literals.
func TestBuildEmptyCollections(t *testing.T) {
	tests := []struct {
		name  string
		src   string
		check func(t *testing.T, file *File)
	}{
		{
			name: "empty_array",
			src:  `f() { x := []; }`,
			check: func(t *testing.T, file *File) {
				fn := file.Decls[0].(*FuncDecl)
				vd := fn.Body.Stmts[0].(*InferredVarDecl)
				al := vd.Value.(*ArrayLit)
				assertLen(t, al.Elements, 0)
			},
		},
		{
			name: "single_element_array",
			src:  `f() { x := [1]; }`,
			check: func(t *testing.T, file *File) {
				fn := file.Decls[0].(*FuncDecl)
				vd := fn.Body.Stmts[0].(*InferredVarDecl)
				al := vd.Value.(*ArrayLit)
				assertLen(t, al.Elements, 1)
			},
		},
		{
			name: "trailing_comma_array",
			src:  `f() { x := [1, 2, 3,]; }`,
			check: func(t *testing.T, file *File) {
				fn := file.Decls[0].(*FuncDecl)
				vd := fn.Body.Stmts[0].(*InferredVarDecl)
				al := vd.Value.(*ArrayLit)
				assertLen(t, al.Elements, 3)
			},
		},
		{
			name: "single_entry_map",
			src:  `f() { x := {"key": 1}; }`,
			check: func(t *testing.T, file *File) {
				fn := file.Decls[0].(*FuncDecl)
				vd := fn.Body.Stmts[0].(*InferredVarDecl)
				ml := vd.Value.(*MapLit)
				assertLen(t, ml.Entries, 1)
			},
		},
		{
			name: "trailing_comma_map",
			src:  `f() { x := {"a": 1, "b": 2,}; }`,
			check: func(t *testing.T, file *File) {
				fn := file.Decls[0].(*FuncDecl)
				vd := fn.Body.Stmts[0].(*InferredVarDecl)
				ml := vd.Value.(*MapLit)
				assertLen(t, ml.Entries, 2)
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

// TestBuildComplexPrograms verifies AST building on multi-declaration programs.
func TestBuildComplexPrograms(t *testing.T) {
	tests := []struct {
		name  string
		src   string
		check func(t *testing.T, file *File)
	}{
		{
			name: "multiple_decls",
			src: `
				use io "std/io";
				type Point { Float x; Float y; }
				enum Color { Red, Green, Blue }
				main() { }
			`,
			check: func(t *testing.T, file *File) {
				assertLen(t, file.Uses, 1)
				assertLen(t, file.Decls, 3)
				_ = file.Decls[0].(*TypeDecl)
				_ = file.Decls[1].(*EnumDecl)
				_ = file.Decls[2].(*FuncDecl)
			},
		},
		{
			name: "nested_control_flow",
			src: `f() {
				for i in items {
					if i > 0 {
						while x < 10 {
							x += 1;
						}
					} else {
						break;
					}
				}
			}`,
			check: func(t *testing.T, file *File) {
				fn := file.Decls[0].(*FuncDecl)
				fi := fn.Body.Stmts[0].(*ForInStmt)
				is := fi.Body.Stmts[0].(*IfStmt)
				ws := is.Body.Stmts[0].(*WhileStmt)
				as := ws.Body.Stmts[0].(*AssignStmt)
				assertEqual(t, as.Op, OpAddAssign)
				elseBlock := is.Else.(*Block)
				_ = elseBlock.Stmts[0].(*BreakStmt)
			},
		},
		{
			name: "complex_expression",
			src:  `f() { x := a.b(c + d, e: f).g[0]?^; }`,
			check: func(t *testing.T, file *File) {
				fn := file.Decls[0].(*FuncDecl)
				vd := fn.Body.Stmts[0].(*InferredVarDecl)
				// x := a.b(c + d, e: f).g[0]?^
				ep := vd.Value.(*ErrorPropagateExpr)
				idx := ep.Expr.(*IndexExpr)
				mg := idx.Target.(*MemberExpr)
				assertEqual(t, mg.Field, "g")
				call := mg.Target.(*CallExpr)
				assertLen(t, call.Args, 2)
				assertEqual(t, call.Args[0].Name, "")
				assertEqual(t, call.Args[1].Name, "e")
				mb := call.Callee.(*MemberExpr)
				assertEqual(t, mb.Field, "b")
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
	tests := []struct {
		name  string
		src   string
		check func(t *testing.T, file *File)
	}{
		{
			name: "func_position",
			src:  `f() { return 42; }`,
			check: func(t *testing.T, file *File) {
				fn := file.Decls[0].(*FuncDecl)
				pos := fn.Pos()
				assertEqual(t, pos.File, "test.pr")
				assertEqual(t, pos.Line, 1)
				assertEqual(t, pos.Column, 0)
			},
		},
		{
			name: "multiline_positions",
			src:  "f() {\n  return 42;\n}",
			check: func(t *testing.T, file *File) {
				fn := file.Decls[0].(*FuncDecl)
				rs := fn.Body.Stmts[0].(*ReturnStmt)
				assertEqual(t, rs.Pos().Line, 2)
			},
		},
		{
			name: "end_position",
			src:  `f() {}`,
			check: func(t *testing.T, file *File) {
				fn := file.Decls[0].(*FuncDecl)
				end := fn.End()
				assertTrue(t, end.Line > 0)
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

// TestBuildInvalidInterpolation verifies that invalid expressions inside string
// interpolation are rejected with errors (B0311).
func TestBuildInvalidInterpolation(t *testing.T) {
	tests := []struct {
		name string
		src  string
		msg  string
	}{
		{
			name: "escaped_quotes_in_interpolation",
			src:  `main() { string s = "{\"a\":\"b\"}"; }`,
			msg:  "invalid expression in string interpolation",
		},
		{
			name: "unconsumed_tokens",
			src:  `main() { string s = "{1 2}"; }`,
			msg:  "invalid expression in string interpolation",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			input := antlr.NewInputStream(tt.src)
			lexer := parser.NewPromiseLexer(input)
			lexer.RemoveErrorListeners()
			stream := antlr.NewCommonTokenStream(lexer, antlr.TokenDefaultChannel)
			p := parser.NewPromiseParser(stream)
			p.RemoveErrorListeners()
			tree := p.CompilationUnit()
			_, errs := Build("test.pr", tree)
			if len(errs) == 0 {
				t.Fatal("expected error for invalid interpolation, got none")
			}
			found := false
			for _, err := range errs {
				if strings.Contains(err.Error(), tt.msg) {
					found = true
					break
				}
			}
			if !found {
				t.Errorf("expected error containing %q, got: %v", tt.msg, errs)
			}
		})
	}
}

// TestBuildEmptyInterpolationStillWorks verifies that truly empty {} interpolation
// still passes AST building (the error is reported by sema, not the AST builder).
func TestBuildEmptyInterpolationStillWorks(t *testing.T) {
	src := `main() { string s = "hello {} world"; }`
	input := antlr.NewInputStream(src)
	lexer := parser.NewPromiseLexer(input)
	lexer.RemoveErrorListeners()
	stream := antlr.NewCommonTokenStream(lexer, antlr.TokenDefaultChannel)
	p := parser.NewPromiseParser(stream)
	p.RemoveErrorListeners()
	tree := p.CompilationUnit()
	_, errs := Build("test.pr", tree)
	// Empty {} should not produce an AST build error — sema handles it
	for _, err := range errs {
		if strings.Contains(err.Error(), "invalid expression in string interpolation") {
			t.Errorf("empty {} should not produce AST build error, got: %v", err)
		}
	}
}
