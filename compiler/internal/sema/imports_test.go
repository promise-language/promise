package sema

import (
	"strings"
	"testing"

	"github.com/antlr4-go/antlr/v4"

	"github.com/promise-language/promise/compiler/internal/ast"
	"github.com/promise-language/promise/compiler/internal/parser"
	"github.com/promise-language/promise/compiler/internal/types"
)

// T1686: imports are file-scoped. A `use` binds its alias only in the file that
// declares it; declarations stay module-wide. These tests build a multi-file
// compilation unit (mirroring mergeModuleFiles + injectStdImport) with distinct
// per-file Pos().File values, then run the full checker.

// unitFile is one source file of a synthetic compilation unit.
type unitFile struct {
	name string
	src  string
}

// parseNamed parses src into an ast.File whose node positions carry the given
// filename, so file-scoped import routing can key on Pos().File.
func parseNamed(t *testing.T, name, src string) *ast.File {
	t.Helper()
	input := antlr.NewInputStream(src)
	lexer := parser.NewPromiseLexer(input)
	lexer.RemoveErrorListeners()
	stream := antlr.NewCommonTokenStream(lexer, antlr.TokenDefaultChannel)
	p := parser.NewPromiseParser(stream)
	p.RemoveErrorListeners()
	tree := p.CompilationUnit()
	file, buildErrs := ast.Build(name, tree)
	if len(buildErrs) > 0 {
		t.Fatalf("AST build errors for %s: %v", name, buildErrs)
	}
	return file
}

// makeModuleScope builds an exported module scope from a small source, so import
// tests can `use` it by catalog name.
func buildModuleFromSource(t *testing.T, name, src string) *types.Scope {
	t.Helper()
	file := parseNamed(t, name+".pr", src)
	file.Uses = append([]*ast.UseDecl{{Alias: "_", CatalogName: "std"}}, file.Uses...)
	info, errs := CheckWithModules(file, map[string]*types.Scope{"std": getSemaStdScope()})
	if semaHasFatalErr(errs) {
		t.Fatalf("module %s failed to compile: %v", name, errs)
	}
	return ExportedScope(info, file)
}

// semaHasFatalErr reports whether errs holds any non-warning diagnostic.
func semaHasFatalErr(errs []error) bool {
	for _, e := range errs {
		if se, ok := e.(*Error); ok && se.Warning {
			continue
		}
		return true
	}
	return false
}

// testModuleScopes builds the small catalog modules the import tests reference.
func testModuleScopes(t *testing.T) map[string]*types.Scope {
	t.Helper()
	return map[string]*types.Scope{
		"std":  getSemaStdScope(),
		"path": buildModuleFromSource(t, "path", "join(string a, string b) string `public { return a; }\nfile_name(string p) string `public { return p; }\n"),
		"math": buildModuleFromSource(t, "math", "gcd(int a, int b) int `public { return a; }\nlerp(f64 a, f64 b, f64 t) f64 `public { return a; }\n"),
		"strings": buildModuleFromSource(t, "strings",
			"reverse(string s) string `public { return s; }\njoin(string a, string b) string `public { return a; }\n"),
	}
}

// checkUnit merges the files (as mergeModuleFiles does), injects `use std as _;`
// once on the merged file (as injectStdImport does), and runs the checker.
func checkUnit(t *testing.T, files []unitFile, mods map[string]*types.Scope) []error {
	t.Helper()
	var merged *ast.File
	for _, f := range files {
		file := parseNamed(t, f.name, f.src)
		if merged == nil {
			merged = file
			continue
		}
		merged.Uses = append(merged.Uses, file.Uses...)
		merged.Decls = append(merged.Decls, file.Decls...)
	}
	// injectStdImport: prepend a Pos-less `use std as _;` (module-wide).
	merged.Uses = append([]*ast.UseDecl{{Alias: "_", CatalogName: "std"}}, merged.Uses...)
	_, errs := CheckWithModules(merged, mods)
	return errs
}

// Case 1: a named alias declared in one file is not visible in a sibling file.
func TestImportScope_NamedAliasNotVisibleAcrossFiles(t *testing.T) {
	errs := checkUnit(t, []unitFile{
		{"a.pr", "use path;\nhelper_a() string `public { return path.join(\"/usr\", \"bin\"); }\n"},
		{"b.pr", "helper_b() string `public { return path.join(\"/opt\", \"lib\"); }\n"},
	}, testModuleScopes(t))
	expectError(t, errs, "undefined module 'path'")
	expectError(t, errs, "imports are per-file, not per-module")
	// b.pr, not a.pr, is where the missing import is reported.
	for _, e := range errs {
		if strings.Contains(e.Error(), "undefined module 'path'") && !strings.Contains(e.Error(), "b.pr") {
			t.Errorf("expected the missing-import error in b.pr, got %v", e)
		}
	}
}

// Case 2: an anonymous import in one file does not leak unqualified names into a
// sibling file.
func TestImportScope_AnonymousImportDoesNotLeak(t *testing.T) {
	errs := checkUnit(t, []unitFile{
		{"a.pr", "use path as _;\nhelper_a() string `public { return join(\"/usr\", \"bin\"); }\n"},
		{"b.pr", "helper_b() string `public { return join(\"/opt\", \"lib\"); }\n"},
	}, testModuleScopes(t))
	// a.pr resolves `join`; b.pr must not.
	expectError(t, errs, "undefined: join")
	for _, e := range errs {
		if strings.Contains(e.Error(), "undefined: join") && !strings.Contains(e.Error(), "b.pr") {
			t.Errorf("expected the leak to be absent only in b.pr, got %v", e)
		}
	}
}

// Case 3: declaring the same import in two files is accepted, not a redeclaration.
func TestImportScope_SameImportInTwoFilesOK(t *testing.T) {
	errs := checkUnit(t, []unitFile{
		{"a.pr", "use path;\nhelper_a() string `public { return path.join(\"/usr\", \"bin\"); }\n"},
		{"b.pr", "use path;\nhelper_b() string `public { return path.join(\"/opt\", \"lib\"); }\n"},
	}, testModuleScopes(t))
	if semaHasFatalErr(errs) {
		t.Fatalf("same import in two files should be accepted, got %v", errs)
	}
	expectNoErrorContaining(t, errs, "redeclared")
	expectNoErrorContaining(t, errs, "duplicate import alias")
}

// Cases 4/5: two files may bind different aliases to one module (and the same
// alias to different modules), each visible only in its own file.
func TestImportScope_DivergentAliasesAcrossFiles(t *testing.T) {
	errs := checkUnit(t, []unitFile{
		{"a.pr", "use path;\npa() string `public { return path.join(\"/a\", \"b\"); }\n"},
		{"b.pr", "use path as p;\npb() string `public { return p.join(\"/a\", \"b\"); }\n"},
	}, testModuleScopes(t))
	if semaHasFatalErr(errs) {
		t.Fatalf("divergent aliases should be accepted, got %v", errs)
	}
	// a.pr's `path` must NOT be reachable in b.pr, and b.pr's `p` not in a.pr.
	errs2 := checkUnit(t, []unitFile{
		{"a.pr", "use path;\npa() string `public { return path.join(\"/a\", \"b\"); }\n"},
		{"b.pr", "use path as p;\npb() string `public { return path.join(\"/a\", \"b\"); }\n"},
	}, testModuleScopes(t))
	expectError(t, errs2, "undefined module 'path'")
}

// Case 6: same alias bound to different modules in two files — each resolves
// correctly within its own file, with no cascading false "no exported member".
func TestImportScope_SameAliasDifferentModules(t *testing.T) {
	errs := checkUnit(t, []unitFile{
		{"a.pr", "use path as m;\npa() string `public { return m.file_name(\"/a/b\"); }\n"},
		{"b.pr", "use strings as m;\npb() string `public { return m.reverse(\"abc\"); }\n"},
	}, testModuleScopes(t))
	if semaHasFatalErr(errs) {
		t.Fatalf("same alias to different modules should be accepted, got %v", errs)
	}
	expectNoErrorContaining(t, errs, "has no exported member")
	expectNoErrorContaining(t, errs, "redeclared")
}

// §6.5: a duplicate alias within one file is an error with the specified text.
func TestImportScope_DuplicateAliasWithinFile(t *testing.T) {
	errs := checkUnit(t, []unitFile{
		{"a.pr", "use path;\nuse path;\npa() string `public { return path.join(\"/a\", \"b\"); }\n"},
	}, testModuleScopes(t))
	expectError(t, errs, "duplicate import alias 'path'")
	expectError(t, errs, "use `as` to rename one")
}

// §5.2 rule 3: an import alias may not collide with a module-wide declaration.
func TestImportScope_ImportVersusDeclarationCollision(t *testing.T) {
	errs := checkUnit(t, []unitFile{
		{"a.pr", "use path as wire;\npa() string `public { return wire.join(\"/a\", \"b\"); }\n"},
		{"b.pr", "type wire { int x; }\n"},
	}, testModuleScopes(t))
	expectError(t, errs, "import alias 'wire' conflicts with type 'wire'")
	expectError(t, errs, "alias the import")
}

// §5.2 rule 4: a named import its own file never references warns (non-fatally).
func TestImportScope_UnusedNamedImportWarns(t *testing.T) {
	errs := checkUnit(t, []unitFile{
		{"a.pr", "use path;\nmain() { print_line(\"hi\"); }\n"},
	}, testModuleScopes(t))
	expectError(t, errs, "unused import 'path'")
	if semaHasFatalErr(errs) {
		t.Fatalf("an unused import must warn, not fail: %v", errs)
	}
}

// A used named import does not warn.
func TestImportScope_UsedNamedImportNoWarn(t *testing.T) {
	errs := checkUnit(t, []unitFile{
		{"a.pr", "use path;\nmain() { print_line(path.join(\"/a\", \"b\")); }\n"},
	}, testModuleScopes(t))
	expectNoErrorContaining(t, errs, "unused import")
}

// §5.3: an anonymous import its file never references warns; a used one does not.
func TestImportScope_UnusedAnonymousImportWarns(t *testing.T) {
	errs := checkUnit(t, []unitFile{
		{"a.pr", "use path as _;\nmain() { print_line(\"hi\"); }\n"},
	}, testModuleScopes(t))
	expectError(t, errs, "unused import")
	if semaHasFatalErr(errs) {
		t.Fatalf("an unused anonymous import must warn, not fail: %v", errs)
	}

	errs2 := checkUnit(t, []unitFile{
		{"a.pr", "use path as _;\nmain() { print_line(join(\"/a\", \"b\")); }\n"},
	}, testModuleScopes(t))
	expectNoErrorContaining(t, errs2, "unused import")
}

// §5.3: two anonymous imports in one file that export the same name conflict.
func TestImportScope_TwoAnonymousImportsSameNameConflict(t *testing.T) {
	errs := checkUnit(t, []unitFile{
		{"a.pr", "use path as _;\nuse strings as _;\nmain() { print_line(join(\"/a\", \"b\")); }\n"},
	}, testModuleScopes(t))
	expectError(t, errs, "conflicts with existing symbol 'join'")
}

// §5.3: a module-wide declaration wins over a name a file-local anonymous import
// would inject — the glob name is silently skipped (no "conflicts with existing
// symbol" error), so the declaration stays in force. Exercises the parent-chain
// skip branch of mergeGlobImport.
func TestImportScope_DeclarationWinsOverGlobImport(t *testing.T) {
	errs := checkUnit(t, []unitFile{
		{"a.pr", "use strings as _;\ncall_it(string s) string `public { return reverse(s); }\n"},
		{"b.pr", "reverse(string s) string `public { return s; }\n"},
	}, testModuleScopes(t))
	// The glob's `reverse` must not collide with the module-wide `reverse`.
	expectNoErrorContaining(t, errs, "conflicts with existing symbol")
	expectNoErrorContaining(t, errs, "redeclared")
}

// §5.2 rule 3: the import-vs-declaration collision names the declaration's kind.
// A collision with an enum reports "enum 'X'" (describeObjectKind enum branch).
func TestImportScope_ImportVersusEnumCollision(t *testing.T) {
	errs := checkUnit(t, []unitFile{
		{"a.pr", "use path as wire;\npa() string `public { return wire.join(\"/a\", \"b\"); }\n"},
		{"b.pr", "enum wire { on, off }\n"},
	}, testModuleScopes(t))
	expectError(t, errs, "import alias 'wire' conflicts with enum 'wire'")
	expectError(t, errs, "alias the import")
}

// §5.2 rule 3: a collision with a function reports "function 'X'"
// (describeObjectKind func branch).
func TestImportScope_ImportVersusFunctionCollision(t *testing.T) {
	errs := checkUnit(t, []unitFile{
		{"a.pr", "use path as wire;\npa() string `public { return wire.join(\"/a\", \"b\"); }\n"},
		{"b.pr", "wire() int `public { return 1; }\n"},
	}, testModuleScopes(t))
	expectError(t, errs, "import alias 'wire' conflicts with function 'wire'")
}

// A module-qualified TYPE reference (mod.Type in a field/signature position) to a
// module a sibling file imports but this file does not gets the per-file hint via
// resolveQualifiedType → suggestForUndefinedModule (distinct from the value-member
// path in Case 1). shapes exports a public type; only a.pr imports it.
func TestImportScope_QualifiedTypeMissingImportHint(t *testing.T) {
	mods := testModuleScopes(t)
	mods["shapes"] = buildModuleFromSource(t, "shapes",
		"type Point `public { int x; int y; }\n")
	errs := checkUnit(t, []unitFile{
		{"a.pr", "use shapes;\nfn_a(shapes.Point p) int `public { return p.x; }\n"},
		{"b.pr", "type Holder { shapes.Point p; }\n"},
	}, mods)
	expectError(t, errs, "undefined module: shapes")
	expectError(t, errs, "imports are per-file, not per-module")
	// The hint must land in b.pr (the file missing the import), not a.pr.
	for _, e := range errs {
		if strings.Contains(e.Error(), "imports are per-file") && !strings.Contains(e.Error(), "b.pr") {
			t.Errorf("expected the per-file hint in b.pr, got %v", e)
		}
	}
}
