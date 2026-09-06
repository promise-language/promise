package sema

import (
	goast "go/ast"
	"go/parser"
	"go/token"
	"testing"

	"github.com/promise-language/promise/compiler/internal/ast"
	"github.com/promise-language/promise/compiler/internal/types"
)

// T1667: sema records every ast.TypeRef it resolves in Info.TypeRefs, and
// codegen reads that back instead of walking the type syntax a second time.
// These tests pin the recording contract — a ref kind missing from the map is
// a codegen resolution that silently returns nil.

// typeRefsByKind indexes recorded refs by their concrete AST type, so a test
// can ask "what did sema record for the one SliceTypeRef in this program?".
func typeRefsByKind[T ast.TypeRef](info *Info) []types.Type {
	var out []types.Type
	for ref, typ := range info.TypeRefs {
		if _, ok := ref.(T); ok {
			out = append(out, typ)
		}
	}
	return out
}

// firstRecorded returns the recorded type for the sole ref of kind T, failing
// if there is not exactly one.
func firstRecorded[T ast.TypeRef](t *testing.T, info *Info) types.Type {
	t.Helper()
	got := typeRefsByKind[T](info)
	if len(got) != 1 {
		t.Fatalf("expected exactly 1 recorded ref of the requested kind, got %d", len(got))
	}
	return got[0]
}

func TestTypeRefsRecordsNamedRef(t *testing.T) {
	info := checkOK(t, `main() { int x = 1; }`)
	found := false
	for ref, typ := range info.TypeRefs {
		n, ok := ref.(*ast.NamedTypeRef)
		if !ok || n.Name != "int" {
			continue
		}
		found = true
		if typ != types.TypInt {
			t.Fatalf("int recorded as %s, want int", typ)
		}
	}
	if !found {
		t.Fatal("no NamedTypeRef for `int` recorded")
	}
}

func TestTypeRefsRecordsGenericNamedRef(t *testing.T) {
	info := checkOK(t, `
		type Box[T] { T v; }
		main() { Box[int] b = Box[int](v: 1); }
	`)
	var got types.Type
	for ref, typ := range info.TypeRefs {
		if n, ok := ref.(*ast.NamedTypeRef); ok && n.Name == "Box" {
			got = typ
		}
	}
	inst, ok := got.(*types.Instance)
	if !ok {
		t.Fatalf("Box[int] recorded as %T (%v), want *types.Instance", got, got)
	}
	if len(inst.TypeArgs()) != 1 || inst.TypeArgs()[0] != types.TypInt {
		t.Fatalf("Box[int] recorded with args %v, want [int]", inst.TypeArgs())
	}
}

func TestTypeRefsRecordsSelfRef(t *testing.T) {
	// The codegen copy this replaced had no `Self` case at all and returned nil
	// here, so a `Self`-typed local fell back to the initializer's type.
	info := checkOK(t, `
		type Point {
			int x;
			origin() Self `+"`factory"+` { return Self(x: 0); }
		}
		main() { p := Point.origin(); }
	`)
	var got types.Type
	for ref, typ := range info.TypeRefs {
		if n, ok := ref.(*ast.NamedTypeRef); ok && n.Name == "Self" {
			got = typ
		}
	}
	if got == nil {
		t.Fatal("no NamedTypeRef for `Self` recorded")
	}
	if named, ok := got.(*types.Named); !ok || named.Obj().Name() != "Point" {
		t.Fatalf("Self recorded as %v, want Point", got)
	}
}

func TestTypeRefsRecordsOptionalRefAndItsInner(t *testing.T) {
	// `int??` — both OptionalTypeRef nodes must be recorded, at their own depth.
	info := checkOK(t, `main() { int?? x = none; }`)
	opts := typeRefsByKind[*ast.OptionalTypeRef](info)
	if len(opts) != 2 {
		t.Fatalf("expected 2 recorded OptionalTypeRefs for int??, got %d", len(opts))
	}
	depths := map[int]bool{}
	for _, typ := range opts {
		d := 0
		for {
			o, ok := typ.(*types.Optional)
			if !ok {
				break
			}
			d++
			typ = o.Elem()
		}
		depths[d] = true
	}
	if !depths[1] || !depths[2] {
		t.Fatalf("expected optional depths 1 and 2 recorded, got %v", depths)
	}
}

func TestTypeRefsRecordsSliceRef(t *testing.T) {
	info := checkOK(t, `main() { int[] v = [1, 2]; }`)
	got := firstRecorded[*ast.SliceTypeRef](t, info)
	inst, ok := got.(*types.Instance)
	if !ok || inst.Origin() != types.TypVector {
		t.Fatalf("int[] recorded as %T (%v), want Vector instance", got, got)
	}
}

func TestTypeRefsRecordsArrayRef(t *testing.T) {
	info := checkOK(t, `main() { int[3] a = [1, 2, 3]; }`)
	got := firstRecorded[*ast.ArrayTypeRef](t, info)
	arr, ok := got.(*types.Array)
	if !ok {
		t.Fatalf("int[3] recorded as %T (%v), want *types.Array", got, got)
	}
	if arr.Size() != 3 {
		t.Fatalf("int[3] recorded with len %d, want 3", arr.Size())
	}
}

func TestTypeRefsRecordsSharedRefAndInner(t *testing.T) {
	info := checkOK(t, `
		type Foo { int x; }
		type Holder {
			Foo item;
			get view Foo & `+"`native"+`;
		}
		main() { }
	`)
	got := firstRecorded[*ast.SharedRefTypeRef](t, info)
	sr, ok := got.(*types.SharedRef)
	if !ok {
		t.Fatalf("Foo& recorded as %T (%v), want *types.SharedRef", got, got)
	}
	// The peeled inner ref must have its own entry — expr_cast.go looks the
	// inner up directly after stripping the borrow.
	inner := false
	for ref, typ := range info.TypeRefs {
		if n, ok := ref.(*ast.NamedTypeRef); ok && n.Name == "Foo" && typ == sr.Elem() {
			inner = true
		}
	}
	if !inner {
		t.Fatal("inner ref of Foo& not recorded")
	}
}

func TestTypeRefsRecordsMutRef(t *testing.T) {
	info := checkOK(t, `
		take(int[]~ xs) int { return xs.len; }
		main() { }
	`)
	got := firstRecorded[*ast.MutRefTypeRef](t, info)
	if _, ok := got.(*types.MutRef); !ok {
		t.Fatalf("int[]~ recorded as %T (%v), want *types.MutRef", got, got)
	}
}

func TestTypeRefsRecordsTupleRef(t *testing.T) {
	info := checkOK(t, `
		pair() (int, string) { return (1, "a"); }
		main() { p := pair(); }
	`)
	got := firstRecorded[*ast.TupleTypeRef](t, info)
	tup, ok := got.(*types.Tuple)
	if !ok {
		t.Fatalf("(int, string) recorded as %T (%v), want *types.Tuple", got, got)
	}
	if len(tup.Elems()) != 2 {
		t.Fatalf("tuple recorded with %d elems, want 2", len(tup.Elems()))
	}
}

// T1634 is the change that had to make the same edit in both resolvers — a
// missed edit gives sema and codegen types that disagree on failability, which
// is a wrong LLVM result shape at an indirect call. The two assertions below
// are that regression, now pinned in the one place the resolution happens.

func TestTypeRefsRecordsFailableFunctionRef(t *testing.T) {
	info := checkOK(t, `
		double_or_raise!(int x) int { return x * 2; }
		main() { !(int) -> int f = double_or_raise; }
	`)
	got := firstRecorded[*ast.FunctionTypeRef](t, info)
	sig, ok := got.(*types.Signature)
	if !ok {
		t.Fatalf("!(int) -> int recorded as %T (%v), want *types.Signature", got, got)
	}
	if !sig.CanError() {
		t.Fatal("!(int) -> int recorded with CanError() == false")
	}
	if sig.Result() != types.TypInt {
		t.Fatalf("!(int) -> int recorded with result %v, want int", sig.Result())
	}
}

func TestTypeRefsRecordsVoidReturningFunctionRef(t *testing.T) {
	info := checkOK(t, `
		run((int) -> void f) { f(1); }
		main() { }
	`)
	got := firstRecorded[*ast.FunctionTypeRef](t, info)
	sig, ok := got.(*types.Signature)
	if !ok {
		t.Fatalf("(int) -> void recorded as %T (%v), want *types.Signature", got, got)
	}
	if sig.Result() != nil {
		t.Fatalf("(int) -> void recorded with result %v, want nil", sig.Result())
	}
	if sig.CanError() {
		t.Fatal("(int) -> void recorded with CanError() == true")
	}
}

func TestTypeRefsRecordsFunctionReturnRefSeparately(t *testing.T) {
	// The `Return` ref gets its own entry, so a caller that peels a function
	// type apart can look the pieces up directly.
	info := checkOK(t, `
		run((int) -> string f) { s := f(1); }
		main() { }
	`)
	found := false
	for ref, typ := range info.TypeRefs {
		if n, ok := ref.(*ast.NamedTypeRef); ok && n.Name == "string" && typ == types.TypString {
			found = true
		}
	}
	if !found {
		t.Fatal("return-position `string` ref not recorded")
	}
}

func TestTypeRefsUnresolvedRefIsAbsent(t *testing.T) {
	// A ref that fails to resolve records nothing — codegen's nil return is the
	// signal, and compilation has already errored.
	info, errs := checkSource(t, `main() { Nope? x; }`)
	if len(errs) == 0 {
		t.Fatal("expected an error for the undefined type")
	}
	// Neither the undefined `Nope` nor the `Nope?` wrapping it may be recorded:
	// a partial entry would let codegen build an alloca from a type sema rejected.
	for ref := range info.TypeRefs {
		switch r := ref.(type) {
		case *ast.NamedTypeRef:
			if r.Name == "Nope" {
				t.Fatal("the unresolved `Nope` ref must not be recorded")
			}
		case *ast.OptionalTypeRef:
			t.Fatal("the `Nope?` ref must not be recorded once its inner failed")
		}
	}
}

func TestTypeRefsRecordsQualifiedRef(t *testing.T) {
	// The codegen copy this replaced reimplemented module-member lookup by hand
	// (T1462) — here it is sema's resolveQualifiedType result, read back.
	modScope := types.NewScope(nil, types.Pos{}, types.Pos{}, "module")
	obj := types.NewTypeName(types.Pos{}, "Widget", nil)
	named := types.NewNamed(obj, nil)
	named.SetExported(true)
	modScope.Insert(obj)

	info, errs := checkWithModules(t, `
		use mymod;
		main() { mymod.Widget? w; }
	`, map[string]*types.Scope{"mymod": modScope})
	if len(errs) > 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	got := firstRecorded[*ast.QualifiedTypeRef](t, info)
	if got != named {
		t.Fatalf("mymod.Widget recorded as %v, want the module's Widget", got)
	}
}

// --- The recording contract when resolution fails ---

// TestTypeRefsCompositeWithUnresolvedInnerIsAbsent — every composite ref kind
// must drop out of the map when a nested ref fails, not record a half-built
// type. `TestTypeRefsUnresolvedRefIsAbsent` pins this for `T?`; the composites
// below are the rest of the walk, and each one is a distinct early return in
// resolveTypeWalk. A recorded entry here would let codegen build an alloca (or
// a cast target) from type syntax sema rejected — the class of bug the single
// resolver exists to make impossible.
func TestTypeRefsCompositeWithUnresolvedInnerIsAbsent(t *testing.T) {
	cases := []struct {
		name string
		src  string
		// kinds that must have no entry at all in this program
		absent []func(ast.TypeRef) bool
	}{
		{
			name:   "slice",
			src:    `main() { Nope[] v; }`,
			absent: []func(ast.TypeRef) bool{isKind[*ast.SliceTypeRef]},
		},
		{
			name:   "array",
			src:    `main() { Nope[3] a; }`,
			absent: []func(ast.TypeRef) bool{isKind[*ast.ArrayTypeRef]},
		},
		{
			name:   "shared ref",
			src:    `take(Nope& x) { } main() { }`,
			absent: []func(ast.TypeRef) bool{isKind[*ast.SharedRefTypeRef]},
		},
		{
			name:   "mut ref",
			src:    `take(Nope~ x) { } main() { }`,
			absent: []func(ast.TypeRef) bool{isKind[*ast.MutRefTypeRef]},
		},
		{
			name:   "tuple",
			src:    `take((Nope, int) p) { } main() { }`,
			absent: []func(ast.TypeRef) bool{isKind[*ast.TupleTypeRef]},
		},
		{
			name:   "function param",
			src:    `take((Nope) -> int f) { } main() { }`,
			absent: []func(ast.TypeRef) bool{isKind[*ast.FunctionTypeRef]},
		},
		{
			name:   "function return",
			src:    `take((int) -> Nope f) { } main() { }`,
			absent: []func(ast.TypeRef) bool{isKind[*ast.FunctionTypeRef]},
		},
		{
			// The composite wrapping the composite: `Nope[]?` must lose both the
			// slice and the optional, so the failure propagates all the way out.
			name: "optional of slice",
			src:  `main() { Nope[]? v; }`,
			absent: []func(ast.TypeRef) bool{
				isKind[*ast.SliceTypeRef], isKind[*ast.OptionalTypeRef],
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			info, errs := checkSource(t, tc.src)
			if len(errs) == 0 {
				t.Fatal("expected an error for the undefined inner type")
			}
			for ref := range info.TypeRefs {
				for _, absent := range tc.absent {
					if absent(ref) {
						t.Fatalf("%T was recorded even though its inner ref failed to resolve", ref)
					}
				}
				if n, ok := ref.(*ast.NamedTypeRef); ok && n.Name == "Nope" {
					t.Fatal("the unresolved `Nope` ref must not be recorded")
				}
			}
		})
	}
}

// isKind reports whether ref has the concrete AST type T.
func isKind[T ast.TypeRef](ref ast.TypeRef) bool {
	_, ok := ref.(T)
	return ok
}

// TestTypeRefsInvalidArraySizeIsAbsent — the array size is an INT_LITERAL, so
// the lexer accepts `0x10` and `1_0` while resolveTypeWalk parses base 10 only.
// The element resolves fine here; it is the *size* that fails, and the array
// ref must still be absent so codegen never sizes an alloca from a rejected
// literal.
func TestTypeRefsInvalidArraySizeIsAbsent(t *testing.T) {
	for _, src := range []string{`main() { int[0x10] a; }`, `main() { int[1_0] a; }`} {
		info, errs := checkSource(t, src)
		expectError(t, errs, "invalid array size")
		for ref := range info.TypeRefs {
			if _, ok := ref.(*ast.ArrayTypeRef); ok {
				t.Fatalf("%s: the array ref must not be recorded when its size is rejected", src)
			}
		}
		// The element type still resolved and is still recorded — the failure is
		// scoped to the node that failed, not to its whole subtree.
		if len(typeRefsByKind[*ast.NamedTypeRef](info)) == 0 {
			t.Fatalf("%s: the element ref should still be recorded", src)
		}
	}
}

// TestTypeRefsNilRefRecordsNothing — resolveType(nil) is the guard callers rely
// on for an omitted type (an inferred `:=`, a bare return type). It must not
// reach the map: a nil key would be a lookup hit for every unrecorded ref
// codegen asks about.
func TestTypeRefsNilRefRecordsNothing(t *testing.T) {
	c := &Checker{info: &Info{TypeRefs: make(map[ast.TypeRef]types.Type)}}
	if got := c.resolveType(nil); got != nil {
		t.Fatalf("resolveType(nil) = %v, want nil", got)
	}
	if len(c.info.TypeRefs) != 0 {
		t.Fatalf("resolveType(nil) recorded %d entries, want 0", len(c.info.TypeRefs))
	}
}

// TestTypeRefsPopulatedByDeclareAndDefine — the declare/define-only entry point
// builds its own Info, and it resolves every field type and method signature.
// If that constructor ever loses its TypeRefs initializer the first resolution
// panics on a nil-map write, so this pins the second of the two init sites.
func TestTypeRefsPopulatedByDeclareAndDefine(t *testing.T) {
	file := parseNamed(t, "crate.pr", `
		type Crate {
			int[] items;
			get first int? `+"`native"+`;
		}
	`)
	info, errs := DeclareAndDefine(file)
	if len(errs) > 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	if len(typeRefsByKind[*ast.SliceTypeRef](info)) != 1 {
		t.Fatalf("declare/define did not record the field's `int[]` ref: %d slice refs", len(typeRefsByKind[*ast.SliceTypeRef](info)))
	}
	if len(typeRefsByKind[*ast.OptionalTypeRef](info)) != 1 {
		t.Fatalf("declare/define did not record the getter's `int?` ref: %d optional refs", len(typeRefsByKind[*ast.OptionalTypeRef](info)))
	}
}

// TestResolveTypeWalkHandlesEveryTypeRefKind — the point of T1667 is that there
// is exactly ONE ast.TypeRef -> types.Type walk. A newly added ref kind that
// nobody adds a case for falls into resolveTypeWalk's `default`, which errors
// at check time on every program that writes the new syntax — and since
// ast.TypeRef's tag method is unexported, no test can construct such a kind to
// catch it. So compare the sets directly: every type in the ast package that
// implements TypeRef must appear as a case in the walk.
func TestResolveTypeWalkHandlesEveryTypeRefKind(t *testing.T) {
	kinds := typeRefKindsInAST(t)
	if len(kinds) < 8 {
		t.Fatalf("only found %d TypeRef kinds in ../ast — the scan is broken: %v", len(kinds), kinds)
	}
	handled := typeRefKindsHandledByWalk(t)
	for _, k := range kinds {
		if !handled[k] {
			t.Errorf("ast.%s implements TypeRef but resolveTypeWalk has no case for it — "+
				"it would fall through to `default` and error as an unknown type reference kind", k)
		}
	}
	for k := range handled {
		found := false
		for _, want := range kinds {
			if want == k {
				found = true
			}
		}
		if !found {
			t.Errorf("resolveTypeWalk has a case for ast.%s, which no longer implements TypeRef", k)
		}
	}
}

// typeRefKindsInAST returns the names of every ast type with a typeRefTag method.
func typeRefKindsInAST(t *testing.T) []string {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), "../ast/typeref.go", nil, 0)
	if err != nil {
		t.Fatalf("parsing ../ast/typeref.go: %v", err)
	}
	var out []string
	for _, decl := range f.Decls {
		fn, ok := decl.(*goast.FuncDecl)
		if !ok || fn.Name.Name != "typeRefTag" || fn.Recv == nil || len(fn.Recv.List) != 1 {
			continue
		}
		star, ok := fn.Recv.List[0].Type.(*goast.StarExpr)
		if !ok {
			continue
		}
		if ident, ok := star.X.(*goast.Ident); ok {
			out = append(out, ident.Name)
		}
	}
	return out
}

// typeRefKindsHandledByWalk returns the ast type names named by the type-switch
// cases in resolveTypeWalk.
func typeRefKindsHandledByWalk(t *testing.T) map[string]bool {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), "resolve.go", nil, 0)
	if err != nil {
		t.Fatalf("parsing resolve.go: %v", err)
	}
	handled := map[string]bool{}
	for _, decl := range f.Decls {
		fn, ok := decl.(*goast.FuncDecl)
		if !ok || fn.Name.Name != "resolveTypeWalk" {
			continue
		}
		goast.Inspect(fn, func(n goast.Node) bool {
			cc, ok := n.(*goast.CaseClause)
			if !ok {
				return true
			}
			for _, expr := range cc.List {
				star, ok := expr.(*goast.StarExpr)
				if !ok {
					continue
				}
				sel, ok := star.X.(*goast.SelectorExpr)
				if !ok {
					continue
				}
				if pkg, ok := sel.X.(*goast.Ident); ok && pkg.Name == "ast" {
					handled[sel.Sel.Name] = true
				}
			}
			return true
		})
	}
	if len(handled) == 0 {
		t.Fatal("found no ast.* type-switch cases in resolveTypeWalk — the scan is broken")
	}
	return handled
}
