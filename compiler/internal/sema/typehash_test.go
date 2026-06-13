package sema

import (
	"strings"
	"testing"

	antlr "github.com/antlr4-go/antlr/v4"
	"github.com/promise-language/promise/compiler/internal/ast"
	"github.com/promise-language/promise/compiler/internal/parser"
	"github.com/promise-language/promise/compiler/internal/types"
)

// --- typehash helpers ---

func parseTestFile(t *testing.T, src string) *ast.File {
	t.Helper()
	input := antlr.NewInputStream(src)
	lexer := parser.NewPromiseLexer(input)
	lexer.RemoveErrorListeners()
	stream := antlr.NewCommonTokenStream(lexer, antlr.TokenDefaultChannel)
	p := parser.NewPromiseParser(stream)
	p.RemoveErrorListeners()
	tree := p.CompilationUnit()
	file, errs := ast.Build("typehash_test.pr", tree)
	if len(errs) > 0 {
		t.Fatalf("AST build errors: %v", errs)
	}
	return file
}

func firstTypeDeclFrom(t *testing.T, src string) *ast.TypeDecl {
	t.Helper()
	file := parseTestFile(t, src)
	for _, decl := range file.Decls {
		if td, ok := decl.(*ast.TypeDecl); ok {
			return td
		}
	}
	t.Fatal("no TypeDecl found in source")
	return nil
}

func firstEnumDeclFrom(t *testing.T, src string) *ast.EnumDecl {
	t.Helper()
	file := parseTestFile(t, src)
	for _, decl := range file.Decls {
		if ed, ok := decl.(*ast.EnumDecl); ok {
			return ed
		}
	}
	t.Fatal("no EnumDecl found in source")
	return nil
}

// findTypeName looks up a type in the file-scope of a sema Info.
func findTypeName(t *testing.T, info *Info, name string) *types.TypeName {
	t.Helper()
	if len(info.ScopeOrder) == 0 {
		t.Fatal("ScopeOrder is empty")
	}
	obj := info.ScopeOrder[0].Lookup(name)
	if obj == nil {
		t.Fatalf("%q not found in file scope", name)
	}
	tn, ok := obj.(*types.TypeName)
	if !ok {
		t.Fatalf("%q is %T, not *types.TypeName", name, obj)
	}
	return tn
}

// --- HashTypeDecl tests ---

func TestHashTypeDeclDeterminism(t *testing.T) {
	td := firstTypeDeclFrom(t, `type Foo { int x; string name; }`)
	h1 := HashTypeDecl(td)
	h2 := HashTypeDecl(td)
	if h1 != h2 {
		t.Errorf("non-deterministic: %q != %q", h1, h2)
	}
	if h1 == "" {
		t.Error("returned empty string")
	}
	// FNV-128a → 16 bytes → 32 hex chars
	if len(h1) != 32 {
		t.Errorf("expected 32 hex chars, got %d: %q", len(h1), h1)
	}
	for _, c := range h1 {
		if !strings.ContainsRune("0123456789abcdef", c) {
			t.Errorf("non-hex char %q in hash %q", c, h1)
		}
	}
}

func TestHashTypeDeclFieldAdded(t *testing.T) {
	h1 := HashTypeDecl(firstTypeDeclFrom(t, `type Foo { int x; }`))
	h2 := HashTypeDecl(firstTypeDeclFrom(t, `type Foo { int x; int y; }`))
	if h1 == h2 {
		t.Error("expected different hash when field added")
	}
}

func TestHashTypeDeclFieldTypeChanged(t *testing.T) {
	h1 := HashTypeDecl(firstTypeDeclFrom(t, `type Foo { int x; }`))
	h2 := HashTypeDecl(firstTypeDeclFrom(t, `type Foo { string x; }`))
	if h1 == h2 {
		t.Error("expected different hash when field type changes")
	}
}

func TestHashTypeDeclMethodBodyChanged(t *testing.T) {
	h1 := HashTypeDecl(firstTypeDeclFrom(t, `type Foo { int x; get(this) int { return this.x; } }`))
	h2 := HashTypeDecl(firstTypeDeclFrom(t, `type Foo { int x; get(this) int { return this.x + 1; } }`))
	if h1 == h2 {
		t.Error("expected different hash when method body changes")
	}
}

// T0866: a bare `{}` in a method body builds to an *ast.EmptyBraceLit. The node
// must hash deterministically (hExpr handles it rather than panicking on an
// unknown Expr), and a body containing it must hash distinctly from one without.
func TestHashTypeDeclEmptyBraceLitInBody(t *testing.T) {
	src := `type Foo { bar() { x := {}; } }`
	h1 := HashTypeDecl(firstTypeDeclFrom(t, src))
	h2 := HashTypeDecl(firstTypeDeclFrom(t, src))
	if h1 != h2 {
		t.Errorf("non-deterministic hash for body with `{}`: %q != %q", h1, h2)
	}
	h3 := HashTypeDecl(firstTypeDeclFrom(t, `type Foo { bar() { x := [];  } }`))
	if h1 == h3 {
		t.Error("expected different hash for `{}` body vs `[]` body")
	}
}

func TestHashTypeDeclMethodSignatureChanged(t *testing.T) {
	h1 := HashTypeDecl(firstTypeDeclFrom(t, `type Foo { int x; set(int v) { } }`))
	h2 := HashTypeDecl(firstTypeDeclFrom(t, `type Foo { int x; set(string v) { } }`))
	if h1 == h2 {
		t.Error("expected different hash when method parameter type changes")
	}
}

func TestHashTypeDeclMethodAdded(t *testing.T) {
	h1 := HashTypeDecl(firstTypeDeclFrom(t, `type Foo { int x; }`))
	h2 := HashTypeDecl(firstTypeDeclFrom(t, `type Foo { int x; get(this) int { return this.x; } }`))
	if h1 == h2 {
		t.Error("expected different hash when method added")
	}
}

func TestHashTypeDeclTypeParamChanged(t *testing.T) {
	h1 := HashTypeDecl(firstTypeDeclFrom(t, `type Box[T] { T value; }`))
	h2 := HashTypeDecl(firstTypeDeclFrom(t, `type Box[T, U] { T value; }`))
	if h1 == h2 {
		t.Error("expected different hash when type param count changes")
	}
}

func TestHashTypeDeclAnnotationChanged(t *testing.T) {
	h1 := HashTypeDecl(firstTypeDeclFrom(t, "type Foo `value { int x; }"))
	h2 := HashTypeDecl(firstTypeDeclFrom(t, `type Foo { int x; }`))
	if h1 == h2 {
		t.Error("expected different hash when annotation changes")
	}
}

func TestHashTypeDeclInheritanceChanged(t *testing.T) {
	file1 := parseTestFile(t, `type Base { int x; } type Derived is Base { int y; }`)
	file2 := parseTestFile(t, `type Base { int x; } type Derived { int y; }`)
	var d1, d2 *ast.TypeDecl
	for _, decl := range file1.Decls {
		if td, ok := decl.(*ast.TypeDecl); ok && td.Name == "Derived" {
			d1 = td
		}
	}
	for _, decl := range file2.Decls {
		if td, ok := decl.(*ast.TypeDecl); ok && td.Name == "Derived" {
			d2 = td
		}
	}
	if d1 == nil || d2 == nil {
		t.Fatal("Derived not found in one of the sources")
	}
	if HashTypeDecl(d1) == HashTypeDecl(d2) {
		t.Error("expected different hash when inheritance changes")
	}
}

func TestHashTypeDeclUnrelatedDeclNoEffect(t *testing.T) {
	// Foo's hash should not change when unrelated type Bar is added to the same file.
	src1 := `type Foo { int x; }`
	src2 := `type Foo { int x; } type Bar { string y; }`
	h1 := HashTypeDecl(firstTypeDeclFrom(t, src1))
	h2 := HashTypeDecl(firstTypeDeclFrom(t, src2)) // first TypeDecl is still Foo
	if h1 != h2 {
		t.Error("Foo's hash changed when unrelated Bar was added to the same file")
	}
}

func TestHashTypeDeclDistinctTypes(t *testing.T) {
	h1 := HashTypeDecl(firstTypeDeclFrom(t, `type Foo { int x; }`))
	h2 := HashTypeDecl(firstTypeDeclFrom(t, `type Bar { int x; }`))
	// Same body, different name → different hash
	if h1 == h2 {
		t.Error("expected different hash for types with different names")
	}
}

// --- HashEnumDecl tests ---

func TestHashEnumDeclDeterminism(t *testing.T) {
	ed := firstEnumDeclFrom(t, `enum Color { Red, Green, Blue }`)
	h1 := HashEnumDecl(ed)
	h2 := HashEnumDecl(ed)
	if h1 != h2 {
		t.Errorf("non-deterministic: %q != %q", h1, h2)
	}
	if len(h1) != 32 {
		t.Errorf("expected 32 hex chars, got %d: %q", len(h1), h1)
	}
}

func TestHashEnumDeclVariantAdded(t *testing.T) {
	h1 := HashEnumDecl(firstEnumDeclFrom(t, `enum Dir { North, South }`))
	h2 := HashEnumDecl(firstEnumDeclFrom(t, `enum Dir { North, South, East }`))
	if h1 == h2 {
		t.Error("expected different hash when variant added")
	}
}

func TestHashEnumDeclVariantFieldAdded(t *testing.T) {
	h1 := HashEnumDecl(firstEnumDeclFrom(t, `enum Event { Click(int x, int y) }`))
	h2 := HashEnumDecl(firstEnumDeclFrom(t, `enum Event { Click(int x, int y, int z) }`))
	if h1 == h2 {
		t.Error("expected different hash when variant field added")
	}
}

func TestHashEnumDeclVariantFieldTypeChanged(t *testing.T) {
	h1 := HashEnumDecl(firstEnumDeclFrom(t, `enum E { A(int x) }`))
	h2 := HashEnumDecl(firstEnumDeclFrom(t, `enum E { A(string x) }`))
	if h1 == h2 {
		t.Error("expected different hash when variant field type changes")
	}
}

func TestHashEnumDeclTypeParamChanged(t *testing.T) {
	h1 := HashEnumDecl(firstEnumDeclFrom(t, `enum Option[T] { Some(T), None }`))
	h2 := HashEnumDecl(firstEnumDeclFrom(t, `enum Option[T, U] { Some(T), None }`))
	if h1 == h2 {
		t.Error("expected different hash when type param count changes")
	}
}

func TestHashEnumDeclAnnotationChanged(t *testing.T) {
	h1 := HashEnumDecl(firstEnumDeclFrom(t, "enum Color `repr(int) { Red, Green }"))
	h2 := HashEnumDecl(firstEnumDeclFrom(t, `enum Color { Red, Green }`))
	if h1 == h2 {
		t.Error("expected different hash when annotation changes")
	}
}

func TestHashEnumDeclDistinctFromTypeDecl(t *testing.T) {
	// Type decl and enum decl hashes should be independent (different discriminants).
	tdHash := HashTypeDecl(firstTypeDeclFrom(t, `type Foo { int x; }`))
	edHash := HashEnumDecl(firstEnumDeclFrom(t, `enum Foo { X }`))
	if tdHash == edHash {
		t.Error("TypeDecl and EnumDecl with same name should produce different hashes")
	}
}

// --- DeclHashes population in sema ---

func TestDeclHashesPopulatedForType(t *testing.T) {
	info := checkOK(t, `type Foo { int x; } main() {}`)
	if info.DeclHashes == nil {
		t.Fatal("DeclHashes is nil")
	}
	tn := findTypeName(t, info, "Foo")
	hash, ok := info.DeclHashes[tn]
	if !ok {
		t.Error("DeclHashes missing entry for Foo")
	}
	if len(hash) != 32 {
		t.Errorf("expected 32-char hex hash, got %d: %q", len(hash), hash)
	}
}

func TestDeclHashesPopulatedForEnum(t *testing.T) {
	info := checkOK(t, `enum Color { Red, Green, Blue } main() {}`)
	if info.DeclHashes == nil {
		t.Fatal("DeclHashes is nil")
	}
	tn := findTypeName(t, info, "Color")
	hash := info.DeclHashes[tn]
	if hash == "" {
		t.Error("DeclHashes missing or empty for Color enum")
	}
}

func TestDeclHashesStableAcrossReruns(t *testing.T) {
	src := `type Foo { int x; string name; add(int v) int { return v + this.x; } } main() {}`
	info1 := checkOK(t, src)
	info2 := checkOK(t, src)
	tn1 := findTypeName(t, info1, "Foo")
	tn2 := findTypeName(t, info2, "Foo")
	if info1.DeclHashes[tn1] != info2.DeclHashes[tn2] {
		t.Errorf("DeclHash not stable: %q != %q",
			info1.DeclHashes[tn1], info2.DeclHashes[tn2])
	}
}

func TestDeclHashesDistinctForDifferentTypes(t *testing.T) {
	info := checkOK(t, `type Foo { int x; } type Bar { string y; } main() {}`)
	fooTN := findTypeName(t, info, "Foo")
	barTN := findTypeName(t, info, "Bar")
	if info.DeclHashes[fooTN] == info.DeclHashes[barTN] {
		t.Error("expected different hashes for Foo and Bar")
	}
}

func TestDeclHashUnchangedByUnrelatedDecl(t *testing.T) {
	// Foo's hash should be the same with or without Bar in the same file.
	src1 := `type Foo { int x; } main() {}`
	src2 := `type Foo { int x; } type Bar { string y; } main() {}`
	info1 := checkOK(t, src1)
	info2 := checkOK(t, src2)
	tn1 := findTypeName(t, info1, "Foo")
	tn2 := findTypeName(t, info2, "Foo")
	if info1.DeclHashes[tn1] != info2.DeclHashes[tn2] {
		t.Errorf("Foo's hash changed when unrelated Bar added: %q != %q",
			info1.DeclHashes[tn1], info2.DeclHashes[tn2])
	}
}

func TestDeclHashChangesWithMethodBodyChange(t *testing.T) {
	src1 := `type Foo { int x; get(this) int { return this.x; } } main() {}`
	src2 := `type Foo { int x; get(this) int { return this.x + 1; } } main() {}`
	info1 := checkOK(t, src1)
	info2 := checkOK(t, src2)
	tn1 := findTypeName(t, info1, "Foo")
	tn2 := findTypeName(t, info2, "Foo")
	if info1.DeclHashes[tn1] == info2.DeclHashes[tn2] {
		t.Error("expected DeclHash to change when method body changes")
	}
}

func TestDeclHashChangesWithFieldChange(t *testing.T) {
	src1 := `type Foo { int x; } main() {}`
	src2 := `type Foo { int x; int y; } main() {}`
	info1 := checkOK(t, src1)
	info2 := checkOK(t, src2)
	tn1 := findTypeName(t, info1, "Foo")
	tn2 := findTypeName(t, info2, "Foo")
	if info1.DeclHashes[tn1] == info2.DeclHashes[tn2] {
		t.Error("expected DeclHash to change when field added")
	}
}

func TestDeclHashesForGenericType(t *testing.T) {
	// Generic types should also have stable hashes.
	src := `type Box[T] { T value; get(this) T { return this.value; } } main() {}`
	info1 := checkOK(t, src)
	info2 := checkOK(t, src)
	tn1 := findTypeName(t, info1, "Box")
	tn2 := findTypeName(t, info2, "Box")
	if info1.DeclHashes[tn1] != info2.DeclHashes[tn2] {
		t.Errorf("generic type DeclHash not stable: %q != %q",
			info1.DeclHashes[tn1], info2.DeclHashes[tn2])
	}
	// Adding a type param should change the hash
	src2 := `type Box[T, U] { T value; get(this) T { return this.value; } } main() {}`
	info3 := checkOK(t, src2)
	tn3 := findTypeName(t, info3, "Box")
	if info1.DeclHashes[tn1] == info3.DeclHashes[tn3] {
		t.Error("expected different hash when type param count changes")
	}
}
