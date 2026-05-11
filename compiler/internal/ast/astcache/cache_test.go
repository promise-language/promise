package astcache

import (
	"bytes"
	"testing"

	"djabi.dev/go/promise_lang/internal/ast"
)

// TestRoundTripSimple tests encode/decode of a simple AST.
func TestRoundTripSimple(t *testing.T) {
	f := &ast.File{}
	f.SetPosEnd(ast.Pos{File: "test.pr", Line: 1, Column: 0}, ast.Pos{File: "test.pr", Line: 10, Column: 0})
	f.Uses = []*ast.UseDecl{
		makeUseDecl("_", "", "std"),
	}
	f.Decls = []ast.Decl{
		makeFuncDecl("main"),
	}

	encoded := Encode(f)
	decoded, err := Decode(encoded)
	if err != nil {
		t.Fatalf("Decode failed: %v", err)
	}

	// Re-encode the decoded AST and compare bytes
	reencoded := Encode(decoded)
	if !bytes.Equal(encoded, reencoded) {
		t.Fatalf("round-trip mismatch: encoded %d bytes, re-encoded %d bytes", len(encoded), len(reencoded))
	}
}

// TestRoundTripExpressions tests all expression types.
func TestRoundTripExpressions(t *testing.T) {
	pos := ast.Pos{File: "test.pr", Line: 1, Column: 0}
	end := ast.Pos{File: "test.pr", Line: 1, Column: 10}

	exprs := []ast.Expr{
		makeExpr(&ast.BinaryExpr{Left: makeIdent("a"), Op: ast.BinAdd, Right: makeIdent("b")}, pos, end),
		makeExpr(&ast.UnaryExpr{Op: ast.UnaryNeg, Operand: makeIdent("x")}, pos, end),
		makeExpr(&ast.IntLit{Raw: "42", Suffix: ""}, pos, end),
		makeExpr(&ast.FloatLit{Raw: "3.14", Suffix: "f32"}, pos, end),
		makeExpr(&ast.BoolLit{Value: true}, pos, end),
		makeExpr(&ast.NoneLit{}, pos, end),
		makeExpr(&ast.CharLit{Raw: "'a'"}, pos, end),
		makeExpr(&ast.StringLit{Raw: `"hello"`, Kind: ast.StringRegular}, pos, end),
		makeExpr(&ast.IdentExpr{Name: "foo"}, pos, end),
		makeExpr(&ast.ThisExpr{}, pos, end),
		makeExpr(&ast.ParenExpr{Expr: makeIdent("x")}, pos, end),
		makeExpr(&ast.MemberExpr{Target: makeIdent("a"), Field: "b"}, pos, end),
		makeExpr(&ast.OptionalChainExpr{Target: makeIdent("a"), Field: "b"}, pos, end),
		makeExpr(&ast.SliceTypeExpr{Inner: makeIdent("int")}, pos, end),
		makeExpr(&ast.ErrorPropagateExpr{Expr: makeIdent("x")}, pos, end),
		makeExpr(&ast.ErrorPanicExpr{Expr: makeIdent("x")}, pos, end),
		makeExpr(&ast.OptionalUnwrapExpr{Expr: makeIdent("x")}, pos, end),
		makeExpr(&ast.TupleLit{Elements: []ast.Expr{makeIdent("a"), makeIdent("b")}}, pos, end),
		makeExpr(&ast.ArrayLit{Elements: []ast.Expr{makeIdent("a")}}, pos, end),
	}

	for _, expr := range exprs {
		f := wrapExprInFile(expr)
		encoded := Encode(f)
		decoded, err := Decode(encoded)
		if err != nil {
			t.Fatalf("Decode failed for expr: %v", err)
		}
		reencoded := Encode(decoded)
		if !bytes.Equal(encoded, reencoded) {
			t.Fatalf("round-trip mismatch for expr type %T", expr)
		}
	}
}

// TestRoundTripStatements tests statement types.
func TestRoundTripStatements(t *testing.T) {
	pos := ast.Pos{File: "test.pr", Line: 1, Column: 0}
	end := ast.Pos{File: "test.pr", Line: 1, Column: 10}

	stmts := []ast.Stmt{
		makeStmt(&ast.ReturnStmt{Value: makeIdent("x")}, pos, end),
		makeStmt(&ast.BreakStmt{}, pos, end),
		makeStmt(&ast.ContinueStmt{}, pos, end),
		makeStmt(&ast.RaiseStmt{Value: makeIdent("e")}, pos, end),
		makeStmt(&ast.YieldStmt{Value: makeIdent("v")}, pos, end),
		makeStmt(&ast.ExprStmt{Expr: makeIdent("x")}, pos, end),
		makeStmt(&ast.InferredVarDecl{Name: "x", Value: makeIdent("y")}, pos, end),
		makeStmt(&ast.DestructureVarDecl{Names: []string{"a", "b"}, Value: makeIdent("t")}, pos, end),
		makeStmt(&ast.IncDecStmt{Target: makeIdent("i"), IsInc: true}, pos, end),
	}

	for _, stmt := range stmts {
		f := wrapStmtInFile(stmt)
		encoded := Encode(f)
		decoded, err := Decode(encoded)
		if err != nil {
			t.Fatalf("Decode failed for stmt: %v", err)
		}
		reencoded := Encode(decoded)
		if !bytes.Equal(encoded, reencoded) {
			t.Fatalf("round-trip mismatch for stmt type %T", stmt)
		}
	}
}

// TestRoundTripTypeDecl tests type declaration encoding.
func TestRoundTripTypeDecl(t *testing.T) {
	pos := ast.Pos{File: "test.pr", Line: 1, Column: 0}
	end := ast.Pos{File: "test.pr", Line: 5, Column: 0}

	td := &ast.TypeDecl{
		Name:       "Foo",
		TypeParams: []*ast.TypeParam{{Name: "T"}},
		Inherits:   []ast.TypeRef{&ast.NamedTypeRef{Name: "Bar"}},
		Annotations: []*ast.MetaAnnotation{
			{Name: "public"},
		},
		Fields: []*ast.FieldDecl{
			{Name: "x", Type: &ast.NamedTypeRef{Name: "int"}},
		},
		Methods: []*ast.MethodDecl{
			{
				Name:     "get_x",
				IsGetter: true,
				ReturnType: &ast.ReturnTypeSpec{
					Type: &ast.NamedTypeRef{Name: "int"},
				},
				Body: &ast.Block{Stmts: []ast.Stmt{
					&ast.ReturnStmt{Value: &ast.MemberExpr{Target: &ast.ThisExpr{}, Field: "x"}},
				}},
			},
		},
	}
	td.SetPosEnd(pos, end)

	f := &ast.File{Decls: []ast.Decl{td}}
	f.SetPosEnd(pos, end)

	encoded := Encode(f)
	decoded, err := Decode(encoded)
	if err != nil {
		t.Fatalf("Decode failed: %v", err)
	}
	reencoded := Encode(decoded)
	if !bytes.Equal(encoded, reencoded) {
		t.Fatalf("round-trip mismatch: %d vs %d bytes", len(encoded), len(reencoded))
	}
}

// TestRoundTripEnumDecl tests enum declaration encoding.
func TestRoundTripEnumDecl(t *testing.T) {
	pos := ast.Pos{File: "test.pr", Line: 1, Column: 0}
	end := ast.Pos{File: "test.pr", Line: 5, Column: 0}

	ed := &ast.EnumDecl{
		Name: "Color",
		Variants: []*ast.EnumVariant{
			{Name: "Red"},
			{Name: "Custom", Fields: []*ast.EnumField{
				{Name: "r", Type: &ast.NamedTypeRef{Name: "int"}},
				{Name: "g", Type: &ast.NamedTypeRef{Name: "int"}},
			}},
		},
	}
	ed.SetPosEnd(pos, end)

	f := &ast.File{Decls: []ast.Decl{ed}}
	f.SetPosEnd(pos, end)

	encoded := Encode(f)
	decoded, err := Decode(encoded)
	if err != nil {
		t.Fatalf("Decode failed: %v", err)
	}
	reencoded := Encode(decoded)
	if !bytes.Equal(encoded, reencoded) {
		t.Fatalf("round-trip mismatch")
	}
}

// TestRoundTripStringInterp tests interpolated string encoding.
func TestRoundTripStringInterp(t *testing.T) {
	pos := ast.Pos{File: "test.pr", Line: 1, Column: 0}
	end := ast.Pos{File: "test.pr", Line: 1, Column: 20}

	sl := &ast.StringLit{
		Raw:  `"hello ${name}!"`,
		Kind: ast.StringRegular,
		Parts: []ast.StringPart{
			ast.StringText{Text: "hello "},
			ast.StringInterp{Raw: "name", Expr: makeIdent("name")},
			ast.StringText{Text: "!"},
		},
	}
	sl.SetPosEnd(pos, end)

	f := wrapExprInFile(sl)
	encoded := Encode(f)
	decoded, err := Decode(encoded)
	if err != nil {
		t.Fatalf("Decode failed: %v", err)
	}
	reencoded := Encode(decoded)
	if !bytes.Equal(encoded, reencoded) {
		t.Fatalf("round-trip mismatch")
	}
}

// TestCacheHeaderValidation tests cache file header checks.
func TestCacheHeaderValidation(t *testing.T) {
	dir := t.TempDir()
	key := Key("test-compiler-hash")

	f := &ast.File{}
	f.SetPosEnd(ast.Pos{}, ast.Pos{})

	// Save and load
	Save(dir, key, f)
	loaded, err := Load(dir, key)
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}
	if loaded == nil {
		t.Fatal("expected cache hit, got miss")
	}

	// Wrong key should miss
	loaded, _ = Load(dir, Key("wrong-hash"))
	if loaded != nil {
		t.Fatal("expected cache miss for wrong key")
	}

	// Missing dir should miss
	loaded, _ = Load("/nonexistent/path", key)
	if loaded != nil {
		t.Fatal("expected cache miss for missing dir")
	}
}

// --- helpers ---

func makeIdent(name string) *ast.IdentExpr {
	n := &ast.IdentExpr{Name: name}
	n.SetPosEnd(ast.Pos{File: "test.pr", Line: 1, Column: 0}, ast.Pos{File: "test.pr", Line: 1, Column: len(name)})
	return n
}

func makeUseDecl(alias, path, catalog string) *ast.UseDecl {
	u := &ast.UseDecl{Alias: alias, Path: path, CatalogName: catalog}
	u.SetPosEnd(ast.Pos{File: "test.pr", Line: 1, Column: 0}, ast.Pos{File: "test.pr", Line: 1, Column: 10})
	return u
}

func makeFuncDecl(name string) *ast.FuncDecl {
	f := &ast.FuncDecl{
		Name: name,
		Body: &ast.Block{},
	}
	f.SetPosEnd(ast.Pos{File: "test.pr", Line: 1, Column: 0}, ast.Pos{File: "test.pr", Line: 3, Column: 0})
	f.Body.SetPosEnd(ast.Pos{File: "test.pr", Line: 1, Column: 8}, ast.Pos{File: "test.pr", Line: 3, Column: 0})
	return f
}

type posEndSetter interface {
	SetPosEnd(ast.Pos, ast.Pos)
}

func makeExpr(e ast.Expr, pos, end ast.Pos) ast.Expr {
	if s, ok := e.(posEndSetter); ok {
		s.SetPosEnd(pos, end)
	}
	return e
}

func makeStmt(s ast.Stmt, pos, end ast.Pos) ast.Stmt {
	if setter, ok := s.(posEndSetter); ok {
		setter.SetPosEnd(pos, end)
	}
	return s
}

func wrapExprInFile(e ast.Expr) *ast.File {
	body := &ast.Block{Stmts: []ast.Stmt{&ast.ExprStmt{Expr: e}}}
	body.SetPosEnd(ast.Pos{File: "test.pr", Line: 1, Column: 0}, ast.Pos{File: "test.pr", Line: 1, Column: 20})
	fd := &ast.FuncDecl{Name: "test", Body: body}
	fd.SetPosEnd(ast.Pos{File: "test.pr", Line: 1, Column: 0}, ast.Pos{File: "test.pr", Line: 1, Column: 20})
	f := &ast.File{Decls: []ast.Decl{fd}}
	f.SetPosEnd(ast.Pos{File: "test.pr", Line: 1, Column: 0}, ast.Pos{File: "test.pr", Line: 1, Column: 20})
	return f
}

func wrapStmtInFile(s ast.Stmt) *ast.File {
	body := &ast.Block{Stmts: []ast.Stmt{s}}
	body.SetPosEnd(ast.Pos{File: "test.pr", Line: 1, Column: 0}, ast.Pos{File: "test.pr", Line: 1, Column: 20})
	fd := &ast.FuncDecl{Name: "test", Body: body}
	fd.SetPosEnd(ast.Pos{File: "test.pr", Line: 1, Column: 0}, ast.Pos{File: "test.pr", Line: 1, Column: 20})
	f := &ast.File{Decls: []ast.Decl{fd}}
	f.SetPosEnd(ast.Pos{File: "test.pr", Line: 1, Column: 0}, ast.Pos{File: "test.pr", Line: 1, Column: 20})
	return f
}
