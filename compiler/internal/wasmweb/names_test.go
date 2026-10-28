package wasmweb

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// declaredNames parses this package's non-test sources and returns the value of
// every string constant they declare. The scan's name set is derived from the
// declarations themselves, so a name added here is guarded the moment it is
// declared, with no second list to keep in sync.
func declaredNames(t *testing.T) map[string]bool {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}
	fset := token.NewFileSet()
	names := map[string]bool{}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		for _, decl := range f.Decls {
			gd, ok := decl.(*ast.GenDecl)
			if !ok || gd.Tok != token.CONST {
				continue
			}
			for _, spec := range gd.Specs {
				for _, v := range spec.(*ast.ValueSpec).Values {
					if bl, ok := v.(*ast.BasicLit); ok && bl.Kind == token.STRING {
						if s, err := strconv.Unquote(bl.Value); err == nil {
							names[s] = true
						}
					}
				}
			}
		}
	}
	return names
}

// scanNameLiterals reports every string literal in f whose value is exactly
// one of names. Exact, not prefix or substring: unrelated symbols legitimately
// share the promise_web_ prefix (docs/wasm-bindings.md's
// promise_web_console_log), and a name inside a larger literal is a different
// string.
func scanNameLiterals(fset *token.FileSet, f *ast.File, names map[string]bool) []string {
	var found []string
	ast.Inspect(f, func(n ast.Node) bool {
		bl, ok := n.(*ast.BasicLit)
		if !ok || bl.Kind != token.STRING {
			return true
		}
		if s, err := strconv.Unquote(bl.Value); err == nil && names[s] {
			found = append(found, fset.Position(bl.Pos()).String()+": string literal "+bl.Value+
				" spells a wasmweb name — use the wasmweb constant (docs/web-apps.md \"One name one definition\")")
		}
		return true
	})
	return found
}

// TestNoNameLiteralOutsideWasmweb enforces docs/web-apps.md "One name one
// definition": no Go string literal anywhere in the compiler spells a name this
// package defines. #25's Bug 1 was two generators independently inventing a
// name for the same thing and drifting apart.
func TestNoNameLiteralOutsideWasmweb(t *testing.T) {
	names := declaredNames(t)
	// Non-vacuity: the guard means something only if it found the names.
	for _, want := range []string{ImportModule, ExportInitialize, ExportEnqueue, ExportPump, GlobalLiveRegistrations} {
		if !names[want] {
			t.Fatalf("declaredNames did not collect %q — the const scan is not reading names.go", want)
		}
	}

	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	self, err := filepath.Abs(".")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	files := 0
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if path == self {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		f, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Errorf("parse %s: %v", path, err)
			return nil
		}
		files++
		for _, msg := range scanNameLiterals(fset, f, names) {
			t.Error(msg)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	// Non-vacuity: a mis-rooted walk would report a clean tree.
	if files < 500 {
		t.Errorf("scanned only %d .go files under %s — the walk is not reading the compiler", files, root)
	}
}

// TestScanNameLiteralsDetects exercises the detector itself: it must flag an
// exact literal and leave a prefix-sharing name, a name inside a larger
// literal, and a comment alone.
func TestScanNameLiteralsDetects(t *testing.T) {
	names := map[string]bool{ExportEnqueue: true}
	cases := []struct {
		name string
		body string
		want int
	}{
		{"exact literal", `x := ` + strconv.Quote(ExportEnqueue) + `; _ = x`, 1},
		{"exact raw literal", "x := `" + ExportEnqueue + "`; _ = x", 1},
		{"prefix-sharing name", `x := "promise_web_console_log"; _ = x`, 0},
		{"inside a larger literal", `x := ` + strconv.Quote("--export="+ExportEnqueue) + `; _ = x`, 0},
		{"comment", "// " + ExportEnqueue + "\n", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fset := token.NewFileSet()
			f, err := parser.ParseFile(fset, "x.go", "package x\nfunc f() {\n"+tc.body+"\n}\n", 0)
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			if got := scanNameLiterals(fset, f, names); len(got) != tc.want {
				t.Errorf("got %d findings %v, want %d", len(got), got, tc.want)
			}
		})
	}
}

// Exports is the one list of host-called names the linker exports and sema
// refuses to bind, so it must hold every Export* constant — derived from the
// declarations, as declaredNames is, so a new export cannot be missed.
func TestExportsCoversExportConstants(t *testing.T) {
	f, err := parser.ParseFile(token.NewFileSet(), "names.go", nil, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parse names.go: %v", err)
	}
	var want []string
	for _, decl := range f.Decls {
		gd, ok := decl.(*ast.GenDecl)
		if !ok || gd.Tok != token.CONST {
			continue
		}
		for _, spec := range gd.Specs {
			vs := spec.(*ast.ValueSpec)
			for i, id := range vs.Names {
				if !strings.HasPrefix(id.Name, "Export") {
					continue
				}
				s, err := strconv.Unquote(vs.Values[i].(*ast.BasicLit).Value)
				if err != nil {
					t.Fatalf("%s: %v", id.Name, err)
				}
				want = append(want, s)
			}
		}
	}
	if len(want) == 0 {
		t.Fatal("found no Export* constants in names.go")
	}
	got := Exports()
	slices.Sort(want)
	slices.Sort(got)
	if !slices.Equal(got, want) {
		t.Errorf("Exports() = %v, want every Export* constant %v", got, want)
	}
	for _, s := range want {
		if !IsExport(s) {
			t.Errorf("IsExport(%q) = false", s)
		}
	}
	for _, s := range []string{SubscribeFunc, ImportModule, "promise_web_console_log", ""} {
		if IsExport(s) {
			t.Errorf("IsExport(%q) = true", s)
		}
	}
}

func TestLookupCoversSignatures(t *testing.T) {
	sigs := Signatures()
	if len(sigs) == 0 {
		t.Fatal("Signatures() is empty")
	}
	for _, s := range sigs {
		got, ok := Lookup(s.Sym)
		if !ok || got.String() != s.String() || got.WebOnly != s.WebOnly {
			t.Errorf("Lookup(%q) = %+v, %v; want %+v", s.Sym, got, ok, s)
		}
	}
}

// The exports, the host import and the globals are names, not symbols a
// Promise `extern binds.
func TestNonExternNamesHaveNoSignature(t *testing.T) {
	for _, n := range []string{ExportInitialize, ExportPump, ExportEnqueue, ImportModule,
		SchedulePumpImport, SchedulePumpSymbol, GlobalLiveRegistrations, GlobalInPump,
		GlobalTerminated, GlobalExitCode, ""} {
		if _, ok := Lookup(n); ok {
			t.Errorf("Lookup(%q) found a signature", n)
		}
	}
}

// Codegen emits the delivery primitives only for wasm32-web, so each is
// wasm32-web-only.
func TestDeliveryPrimitivesAreWebOnly(t *testing.T) {
	for _, s := range Signatures() {
		if !s.WebOnly {
			t.Errorf("%s: WebOnly = false, but codegen defines it only on wasm32-web", s.Sym)
		}
	}
}

func TestSignatureString(t *testing.T) {
	cases := map[string]string{
		SubscribeFunc:           "(i32 capacity, i32 policy) i32",
		ChannelFunc:             "(i32 sub_id) Channel[i32]",
		UnsubscribeFunc:         "(i32 sub_id)",
		SubscriptionDroppedFunc: "(i32 sub_id) i32",
	}
	for sym, want := range cases {
		s, ok := Lookup(sym)
		if !ok {
			t.Fatalf("Lookup(%q) failed", sym)
		}
		if got := s.String(); got != want {
			t.Errorf("%s: String() = %q, want %q", sym, got, want)
		}
	}
	if len(cases) != len(Signatures()) {
		t.Errorf("test covers %d signatures, registry has %d", len(cases), len(Signatures()))
	}
}

func TestSignaturesReturnsACopy(t *testing.T) {
	a := Signatures()
	a[0].Sym = "mutated"
	a[0].Params[0].Type = "mutated"
	b := Signatures()
	if b[0].Sym == "mutated" || b[0].Params[0].Type == "mutated" {
		t.Error("mutating Signatures()'s result changed the registry")
	}
	s, _ := Lookup(SubscribeFunc)
	s.Params[0].Type = "mutated"
	if again, _ := Lookup(SubscribeFunc); again.Params[0].Type == "mutated" {
		t.Error("mutating Lookup's result changed the registry")
	}
}
