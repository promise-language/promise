package codegen

import (
	"testing"

	"github.com/promise-language/promise/compiler/internal/ast"
	"github.com/promise-language/promise/compiler/internal/sema"
	"github.com/promise-language/promise/compiler/internal/types"
)

// T1667: resolveTypeRefToType reads back the type sema recorded for an
// ast.TypeRef instead of walking the type syntax a second time. The IR-level
// half of this lives in codegen/tests/regress11; what can only be reached from
// inside the package is the *absence* contract — the two ways the lookup comes
// up empty, which every caller branches on — and the scope-walking fallback
// lookupLocalType takes when it does.

// typeRefCompiler returns a Compiler with just the state resolveTypeRefToType
// reads: the recorded-ref map, plus the sema scopes lookupVarType walks.
func typeRefCompiler(refs map[ast.TypeRef]types.Type, scopes ...*types.Scope) *Compiler {
	return &Compiler{info: &sema.Info{TypeRefs: refs, ScopeOrder: scopes}}
}

// TestResolveTypeRefToTypeNilRef — a nil ref must be nil, not a map lookup on a
// nil key. A nil key would collide with every other unrecorded nil ref, so a
// single stray record would start answering for all of them.
func TestResolveTypeRefToTypeNilRef(t *testing.T) {
	c := typeRefCompiler(map[ast.TypeRef]types.Type{nil: types.TypInt})
	if got := c.resolveTypeRefToType(nil); got != nil {
		t.Fatalf("resolveTypeRefToType(nil) = %v, want nil even with a nil key recorded", got)
	}
}

// TestResolveTypeRefToTypeUnrecordedRef — a ref sema never resolved has no
// entry, and the documented contract is a nil return. Callers key their
// fallbacks off exactly this (genTypedVarDecl bails, genCastExpr panics with a
// "sema recorded no type" message), so a zero-value type here instead of nil
// would turn a frontend bug into a bogus alloca.
func TestResolveTypeRefToTypeUnrecordedRef(t *testing.T) {
	c := typeRefCompiler(map[ast.TypeRef]types.Type{})
	if got := c.resolveTypeRefToType(&ast.NamedTypeRef{Name: "Ghost"}); got != nil {
		t.Fatalf("unrecorded ref resolved to %v, want nil", got)
	}
}

// TestResolveTypeRefToTypeAppliesSubstitutionsInOrder — the recorded type is in
// terms of the declaring context, so it can still carry that context's type
// params AND its interface-as-Self at once (a structural default method body
// synthesized for a generic concrete type). Both substitutions must be applied,
// mono first: SubstituteSelf replaces Self with the concrete *Named, and only
// then can the type params inside it be resolved by the caller's map. Applying
// just one leaves an unsubstituted type that resolveType turns into the wrong
// LLVM shape.
func TestResolveTypeRefToTypeAppliesSubstitutionsInOrder(t *testing.T) {
	tp := types.NewTypeParam(types.NewTypeName(types.Pos{}, "T", nil), nil, 0)
	iface := types.NewNamed(types.NewTypeName(types.Pos{}, "Shower", nil), nil)
	concrete := types.NewNamed(types.NewTypeName(types.Pos{}, "Widget", nil), nil)

	ref := &ast.NamedTypeRef{Name: "T"}
	c := typeRefCompiler(map[ast.TypeRef]types.Type{ref: types.NewOptional(tp)})

	// Mono substitution only.
	c.typeSubst = map[*types.TypeParam]types.Type{tp: types.TypString}
	got := c.resolveTypeRefToType(ref)
	opt, ok := got.(*types.Optional)
	if !ok || opt.Elem() != types.TypString {
		t.Fatalf("with typeSubst active, T? resolved to %v, want string?", got)
	}

	// Self substitution only, on a separately recorded ref.
	selfRef := &ast.NamedTypeRef{Name: "Self"}
	c = typeRefCompiler(map[ast.TypeRef]types.Type{selfRef: types.NewOptional(iface)})
	c.selfSubst = &selfSubstInfo{iface: iface, concrete: concrete}
	got = c.resolveTypeRefToType(selfRef)
	opt, ok = got.(*types.Optional)
	if !ok || opt.Elem() != concrete {
		t.Fatalf("with selfSubst active, Self? resolved to %v, want Widget?", got)
	}

	// Both at once: Self inside a mono'd generic body.
	bothRef := &ast.NamedTypeRef{Name: "Self"}
	c = typeRefCompiler(map[ast.TypeRef]types.Type{bothRef: types.NewOptional(iface)})
	c.typeSubst = map[*types.TypeParam]types.Type{tp: types.TypString}
	c.selfSubst = &selfSubstInfo{iface: iface, concrete: concrete}
	got = c.resolveTypeRefToType(bothRef)
	if opt, ok = got.(*types.Optional); !ok || opt.Elem() != concrete {
		t.Fatalf("with both substitutions active, Self? resolved to %v, want Widget?", got)
	}
}

// TestLookupLocalTypeIgnoresNonOptionalRef — lookupLocalType only speaks for
// Optional declarations; every other declared type is left to the caller's
// expression-type path. Returning a type here would override that path for
// every typed local in the program.
func TestLookupLocalTypeIgnoresNonOptionalRef(t *testing.T) {
	ref := &ast.NamedTypeRef{Name: "int"}
	c := typeRefCompiler(map[ast.TypeRef]types.Type{ref: types.TypInt})
	if got := c.lookupLocalType(&ast.TypedVarDecl{Name: "x", Type: ref}); got != nil {
		t.Fatalf("lookupLocalType on a non-optional ref = %v, want nil", got)
	}
}

// TestLookupLocalTypeFallsBackToScopeLookup — the fallback for an
// OptionalTypeRef sema left unrecorded. Reading the recorded map is the primary
// path and this is the only thing behind it, so if the map read ever regresses
// to nil for a well-typed program, this is the behaviour the compiler silently
// falls into: the declared type comes from the sema scope entry instead.
func TestLookupLocalTypeFallsBackToScopeLookup(t *testing.T) {
	scope := types.NewScope(nil, types.Pos{}, types.Pos{}, "file")
	declared := types.NewOptional(types.TypString)
	scope.Insert(types.NewVar(types.Pos{}, "s", declared))

	// The ref is deliberately absent from TypeRefs.
	c := typeRefCompiler(map[ast.TypeRef]types.Type{}, scope)
	decl := &ast.TypedVarDecl{Name: "s", Type: &ast.OptionalTypeRef{Inner: &ast.NamedTypeRef{Name: "string"}}}
	if got := c.lookupLocalType(decl); got != declared {
		t.Fatalf("lookupLocalType fallback = %v, want the scope's string?", got)
	}

	// And the recorded ref wins when there is one. The scope entry here is a
	// one-deep optional while the record is two-deep: `string?? b = a` is exactly
	// the case where the two disagree, and taking the shallower one collapses the
	// alloca and breaks the unwrap.
	recorded := types.NewOptional(declared)
	c = typeRefCompiler(map[ast.TypeRef]types.Type{decl.Type: recorded}, scope)
	if got := c.lookupLocalType(decl); got != recorded {
		t.Fatalf("lookupLocalType = %v, want the recorded string??", got)
	}
}

// TestLookupVarTypeSubstitutesTypeParams — the fallback runs inside mono'd
// bodies too, where the scope entry is still written in terms of the generic's
// type params. It must apply the ambient substitution, exactly as the recorded
// path does; a raw TypeParam reaching resolveType has no LLVM layout.
func TestLookupVarTypeSubstitutesTypeParams(t *testing.T) {
	tp := types.NewTypeParam(types.NewTypeName(types.Pos{}, "T", nil), nil, 0)
	scope := types.NewScope(nil, types.Pos{}, types.Pos{}, "file")
	scope.Insert(types.NewVar(types.Pos{}, "v", types.NewOptional(tp)))

	c := typeRefCompiler(map[ast.TypeRef]types.Type{}, scope)
	c.typeSubst = map[*types.TypeParam]types.Type{tp: types.TypInt}
	opt, ok := c.lookupVarType("v").(*types.Optional)
	if !ok || opt.Elem() != types.TypInt {
		t.Fatalf("lookupVarType under mono = %v, want int?", c.lookupVarType("v"))
	}
}

// TestLookupVarTypeMissesAndNonVars — an unknown name, and a name bound to a
// non-Var object (a type name shadowing a local). Both must be nil rather than
// a type read off the wrong object kind.
func TestLookupVarTypeMissesAndNonVars(t *testing.T) {
	scope := types.NewScope(nil, types.Pos{}, types.Pos{}, "file")
	scope.Insert(types.NewTypeName(types.Pos{}, "Widget", nil))

	c := typeRefCompiler(map[ast.TypeRef]types.Type{}, scope)
	if got := c.lookupVarType("nothing"); got != nil {
		t.Fatalf("lookupVarType on an unknown name = %v, want nil", got)
	}
	if got := c.lookupVarType("Widget"); got != nil {
		t.Fatalf("lookupVarType on a TypeName = %v, want nil", got)
	}
}
