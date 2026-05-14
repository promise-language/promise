package astcache

import (
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"

	"djabi.dev/go/promise_lang/internal/ast"
	"djabi.dev/go/promise_lang/internal/parser"
	"djabi.dev/go/promise_lang/internal/testutil"
	antlr "github.com/antlr4-go/antlr/v4"
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
	key := Key("test-compiler-hash", "test-content-hash")

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

	// Wrong compiler hash should miss
	loaded, _ = Load(dir, Key("wrong-hash", "test-content-hash"))
	if loaded != nil {
		t.Fatal("expected cache miss for wrong compiler hash")
	}

	// Wrong content hash should miss
	loaded, _ = Load(dir, Key("test-compiler-hash", "wrong-content"))
	if loaded != nil {
		t.Fatal("expected cache miss for wrong content hash")
	}

	// Missing dir should miss
	loaded, _ = Load("/nonexistent/path", key)
	if loaded != nil {
		t.Fatal("expected cache miss for missing dir")
	}
}

// TestContentHash tests content hash determinism and sensitivity.
func TestContentHash(t *testing.T) {
	files := []string{"a.pr", "b.pr"}
	contents := [][]byte{[]byte("hello"), []byte("world")}

	h1 := ContentHash(files, contents)
	h2 := ContentHash(files, contents)
	if h1 != h2 {
		t.Fatalf("ContentHash not deterministic: %s != %s", h1, h2)
	}

	// Different content → different hash
	h3 := ContentHash(files, [][]byte{[]byte("hello"), []byte("changed")})
	if h1 == h3 {
		t.Fatal("ContentHash should differ for different content")
	}

	// Different filenames → different hash (rename invalidation)
	h4 := ContentHash([]string{"a.pr", "c.pr"}, contents)
	if h1 == h4 {
		t.Fatal("ContentHash should differ for different filenames")
	}

	// Single file
	h5 := ContentHash([]string{"x.pr"}, [][]byte{[]byte("data")})
	if h5 == "" {
		t.Fatal("ContentHash should not be empty")
	}
}

// TestCacheDir verifies the cache directory path.
func TestCacheDir(t *testing.T) {
	got := CacheDir(filepath.Join("/home/user/.promise/cache", "build"))
	want := filepath.Join("/home/user/.promise/cache", "astcache")
	if got != want {
		t.Fatalf("CacheDir = %q, want %q", got, want)
	}
}

// TestRemove verifies cache entry removal.
func TestRemove(t *testing.T) {
	dir := t.TempDir()
	key := Key("compiler", "content")
	f := &ast.File{}
	f.SetPosEnd(ast.Pos{}, ast.Pos{})

	Save(dir, key, f)
	// Verify it exists
	loaded, _ := Load(dir, key)
	if loaded == nil {
		t.Fatal("expected cache hit after Save")
	}

	Remove(dir, key)
	loaded, _ = Load(dir, key)
	if loaded != nil {
		t.Fatal("expected cache miss after Remove")
	}

	// Remove of non-existent key is a no-op
	Remove(dir, Key("nonexistent", "key"))
}

// TestLoadCorruptData tests Load with various corrupt inputs.
func TestLoadCorruptData(t *testing.T) {
	dir := t.TempDir()
	key := Key("compiler", "content")
	path := cachePath(dir, key)

	// Truncated data (less than header size)
	os.WriteFile(path, []byte("short"), 0o644)
	loaded, _ := Load(dir, key)
	if loaded != nil {
		t.Fatal("expected miss for truncated data")
	}

	// Bad magic
	bad := make([]byte, headerSize+10)
	copy(bad[:4], "XXXX")
	os.WriteFile(path, bad, 0o644)
	loaded, _ = Load(dir, key)
	if loaded != nil {
		t.Fatal("expected miss for bad magic")
	}

	// Bad version (correct magic, wrong version)
	copy(bad[:4], magic)
	binary.LittleEndian.PutUint32(bad[4:8], 99)
	os.WriteFile(path, bad, 0o644)
	loaded, _ = Load(dir, key)
	if loaded != nil {
		t.Fatal("expected miss for bad version")
	}

	// Correct header but corrupt payload (triggers Decode error + Remove)
	binary.LittleEndian.PutUint32(bad[4:8], formatVersion)
	kh := keyToHash(key)
	copy(bad[8:24], kh[:])
	// payload is garbage bytes → Decode should fail
	os.WriteFile(path, bad, 0o644)
	loaded, _ = Load(dir, key)
	if loaded != nil {
		t.Fatal("expected miss for corrupt payload")
	}
	// File should be removed after corrupt decode
	if _, err := os.Stat(path); err == nil {
		t.Fatal("expected corrupt cache file to be removed")
	}
}

// TestKeyToHashFallback tests keyToHash with a non-hex key string.
func TestKeyToHashFallback(t *testing.T) {
	// Valid hex key (normal path)
	validKey := Key("a", "b")
	h1 := keyToHash(validKey)
	h2 := keyToHash(validKey)
	if h1 != h2 {
		t.Fatal("keyToHash should be deterministic for valid hex")
	}

	// Non-hex key triggers fallback
	h3 := keyToHash("not-a-hex-string!!!")
	h4 := keyToHash("not-a-hex-string!!!")
	if h3 != h4 {
		t.Fatal("keyToHash fallback should be deterministic")
	}

	// Different non-hex keys produce different hashes
	h5 := keyToHash("another-non-hex")
	if h3 == h5 {
		t.Fatal("keyToHash fallback should differ for different inputs")
	}
}

// TestRoundTripSelectEmptyDefault verifies that a select with an empty default
// body (Default = []Stmt{}) survives encode/decode as non-nil (B0352).
func TestRoundTripSelectEmptyDefault(t *testing.T) {
	pos := ast.Pos{File: "test.pr", Line: 1, Column: 0}
	end := ast.Pos{File: "test.pr", Line: 5, Column: 0}

	sel := &ast.SelectStmt{
		Cases:   []*ast.SelectCase{},
		Default: []ast.Stmt{},
	}
	sel.SetPosEnd(pos, end)

	f := wrapStmtInFile(sel)
	encoded := Encode(f)
	decoded, err := Decode(encoded)
	if err != nil {
		t.Fatalf("Decode failed: %v", err)
	}

	// Extract the SelectStmt from decoded
	fd := decoded.Decls[0].(*ast.FuncDecl)
	got := fd.Body.Stmts[0].(*ast.SelectStmt)
	if got.Default == nil {
		t.Fatal("expected non-nil Default for empty default body, got nil")
	}
	if len(got.Default) != 0 {
		t.Fatalf("expected empty Default slice, got %d stmts", len(got.Default))
	}

	// Verify byte-level round-trip
	reencoded := Encode(decoded)
	if !bytes.Equal(encoded, reencoded) {
		t.Fatal("round-trip mismatch for select with empty default")
	}
}

// TestRoundTripSelectNoDefault verifies that a select with no default clause
// (Default = nil) survives encode/decode as nil (B0352).
func TestRoundTripSelectNoDefault(t *testing.T) {
	pos := ast.Pos{File: "test.pr", Line: 1, Column: 0}
	end := ast.Pos{File: "test.pr", Line: 5, Column: 0}

	sel := &ast.SelectStmt{
		Cases:   []*ast.SelectCase{},
		Default: nil,
	}
	sel.SetPosEnd(pos, end)

	f := wrapStmtInFile(sel)
	encoded := Encode(f)
	decoded, err := Decode(encoded)
	if err != nil {
		t.Fatalf("Decode failed: %v", err)
	}

	fd := decoded.Decls[0].(*ast.FuncDecl)
	got := fd.Body.Stmts[0].(*ast.SelectStmt)
	if got.Default != nil {
		t.Fatalf("expected nil Default for no default clause, got %d stmts", len(got.Default))
	}

	reencoded := Encode(decoded)
	if !bytes.Equal(encoded, reencoded) {
		t.Fatal("round-trip mismatch for select with no default")
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

// parseString parses Promise source into an AST File using ANTLR4.
func parseString(t *testing.T, filename, src string) *ast.File {
	t.Helper()
	input := antlr.NewInputStream(src)
	lexer := parser.NewPromiseLexer(input)
	lexer.RemoveErrorListeners()
	stream := antlr.NewCommonTokenStream(lexer, antlr.TokenDefaultChannel)
	p := parser.NewPromiseParser(stream)
	p.RemoveErrorListeners()
	tree := p.CompilationUnit()
	file, errs := ast.Build(filename, tree)
	if len(errs) > 0 {
		t.Fatalf("parse errors in %s: %v", filename, errs)
	}
	return file
}

// TestRoundTripRealStd parses the actual embedded std module source,
// encodes it, decodes it, and verifies the round-trip produces identical bytes.
func TestRoundTripRealStd(t *testing.T) {
	src := testutil.LoadStdFiles()
	original := parseString(t, "std.pr", src)

	encoded := Encode(original)
	decoded, err := Decode(encoded)
	if err != nil {
		t.Fatalf("Decode failed: %v", err)
	}

	reencoded := Encode(decoded)
	if !bytes.Equal(encoded, reencoded) {
		// Find first difference for debugging
		minLen := len(encoded)
		if len(reencoded) < minLen {
			minLen = len(reencoded)
		}
		for i := 0; i < minLen; i++ {
			if encoded[i] != reencoded[i] {
				t.Fatalf("round-trip mismatch at byte %d: encoded=0x%02x, re-encoded=0x%02x (encoded %d bytes, re-encoded %d bytes)",
					i, encoded[i], reencoded[i], len(encoded), len(reencoded))
			}
		}
		t.Fatalf("round-trip length mismatch: encoded %d bytes, re-encoded %d bytes", len(encoded), len(reencoded))
	}
}
