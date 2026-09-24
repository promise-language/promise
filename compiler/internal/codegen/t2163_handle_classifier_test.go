package codegen

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"testing"

	"github.com/promise-language/promise/compiler/internal/types"
)

// newTestChannel builds Channel[elem]; types has no NewChannel helper (channels
// are constructed as a generic instance, the same shape sema produces).
func newTestChannel(elem types.Type) *types.Instance {
	return types.NewInstance(types.TypChannel, []types.Type{elem})
}

// T2163: refcountedHandleElem is the single classifier behind the language-design.md#ownership-across-goroutines
// spawn-site duplication — it decides whether a value crossing a `go` boundary
// is a refcounted `sharable handle, and resolves the element type that selects
// the dup/drop PAIR. Getting it wrong is silent: a refusal means no retain, and
// no retain means the spawner's drop frees the referent under a running
// goroutine (the T2163 bug itself, which read 0xDEDE… allocator poison).
//
// retainSpawnHandle documents it as total — "a caller can offer it any type and
// act on the answer" — so it is tested directly here rather than only through
// the shapes that happen to reach it from genGoCallExpr today. Several branches
// below are guards for callers that do not exist yet: sweeping the whole Promise
// suite (11543 tests) and every codegen Go package with a panic wired into them
// showed the nil, SharedRef, and unresolved-element arms are unreachable through
// the current spawn sites, because every live caller passes either an argument
// expression's type or a declared parameter type — always an Instance, never a
// bare generic origin. They are kept because the classifier is shared, the next
// caller (T2171, captures on the two block spawn sites) reads types off bindings
// where a bare origin is reachable, and a silent refusal there is a
// use-after-free rather than a visible failure. These tests are what make that
// contract real instead of aspirational.
func TestT2163_RefcountedHandleElem(t *testing.T) {
	// A distinct type param + substitution, standing in for a monomorphized body.
	tp := types.NewTypeParam(types.NewTypeName(types.Pos{}, "T", nil), nil, 0)
	subst := map[*types.TypeParam]types.Type{tp: types.TypInt}

	tests := []struct {
		name       string
		typ        types.Type
		typeSubst  map[*types.TypeParam]types.Type
		wantOrigin *types.Named
		wantElem   types.Type
		wantOK     bool
	}{
		// --- the three handles, concrete ---
		{
			name:       "channel",
			typ:        newTestChannel(types.TypInt),
			wantOrigin: types.TypChannel, wantElem: types.TypInt, wantOK: true,
		},
		{
			name:       "arc",
			typ:        types.NewArc(types.TypInt),
			wantOrigin: types.TypArc, wantElem: types.TypInt, wantOK: true,
		},
		{
			name:       "weak",
			typ:        types.NewWeak(types.TypInt),
			wantOrigin: types.TypWeak, wantElem: types.TypInt, wantOK: true,
		},
		{
			name:       "arc of a heap element",
			typ:        types.NewArc(types.TypString),
			wantOrigin: types.TypArc, wantElem: types.TypString, wantOK: true,
		},
		{
			name:       "nested handle element",
			typ:        types.NewArc(newTestChannel(types.TypInt)),
			wantOrigin: types.TypArc, wantElem: newTestChannel(types.TypInt), wantOK: true,
		},

		// --- borrow wrappers: a `~`/`&` borrow of a handle is still a handle ---
		// The MutRef arm is the T2163 review fix: extractNamed already looks
		// through refs, so before it the ORIGIN matched while AsArc left the
		// element nil, and the handle was silently refused — a `Ref[int]~`
		// parameter passed to `go f(r)` read poison.
		{
			name:       "mut-ref borrow of an arc",
			typ:        types.NewMutRef(types.NewArc(types.TypInt)),
			wantOrigin: types.TypArc, wantElem: types.TypInt, wantOK: true,
		},
		{
			name:       "shared-ref borrow of a channel",
			typ:        types.NewSharedRef(newTestChannel(types.TypInt)),
			wantOrigin: types.TypChannel, wantElem: types.TypInt, wantOK: true,
		},
		{
			name:       "shared-ref borrow of a weak",
			typ:        types.NewSharedRef(types.NewWeak(types.TypString)),
			wantOrigin: types.TypWeak, wantElem: types.TypString, wantOK: true,
		},

		// --- monomorphization: the element must come out concrete ---
		{
			name:       "arc of a type param, substituted",
			typ:        types.NewArc(tp),
			typeSubst:  subst,
			wantOrigin: types.TypArc, wantElem: types.TypInt, wantOK: true,
		},
		{
			name:       "channel of a type param, substituted",
			typ:        newTestChannel(tp),
			typeSubst:  subst,
			wantOrigin: types.TypChannel, wantElem: types.TypInt, wantOK: true,
		},
		{
			// The bare generic origin carries no type args, so the element can
			// only come from the active substitution.
			name:       "bare generic origin resolved from the substitution",
			typ:        types.TypArc,
			typeSubst:  map[*types.TypeParam]types.Type{types.TypArc.TypeParams()[0]: types.TypInt},
			wantOrigin: types.TypArc, wantElem: types.TypInt, wantOK: true,
		},

		// --- refusals: no retain may be emitted without a resolvable release ---
		{
			// A bare origin with no substitution in scope has no element, so no
			// Ref[T].drop can be selected. Refusing is what keeps a retain from
			// ever escaping unpaired.
			name: "bare generic origin with no substitution is refused",
			typ:  types.TypArc,
		},
		{
			name: "bare generic origin whose param is absent from the substitution",
			typ:  types.TypChannel,
			// A substitution that does not mention Channel's own type param.
			typeSubst: subst,
		},
		{name: "nil"},
		{name: "int", typ: types.TypInt},
		{name: "string", typ: types.TypString},
		{name: "vector", typ: types.NewVector(types.TypInt)},
		{
			// Optional[handle] is NOT a bare handle: its LLVM value is an
			// aggregate, not the handle pointer, so retaining it would bump
			// whatever the aggregate's first word happens to be. extractNamed
			// deliberately does not see through Optional (T1640/T1653).
			name: "optional of an arc is refused",
			typ:  types.NewOptional(types.NewArc(types.TypInt)),
		},
		{
			name: "optional of a channel is refused",
			typ:  types.NewOptional(newTestChannel(types.TypInt)),
		},
		{
			// Mutex is `sharable but NOT refcounted — it cannot be duplicated,
			// so it must be REJECTED at the boundary rather than retained
			// (T2165). Accepting it here would paper over that check.
			name: "mutex is refused",
			typ:  types.NewMutex(types.TypInt),
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := &Compiler{typeSubst: tc.typeSubst}
			origin, elem, ok := c.refcountedHandleElem(tc.typ)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v (type %v)", ok, tc.wantOK, tc.typ)
			}
			if !tc.wantOK {
				// A refusal must not hand back a half-answer a caller could act on.
				if origin != nil || elem != nil {
					t.Fatalf("refusal returned (%v, %v), want (nil, nil)", origin, elem)
				}
				return
			}
			if origin != tc.wantOrigin {
				t.Fatalf("origin = %v, want %v", origin, tc.wantOrigin)
			}
			if !types.Identical(elem, tc.wantElem) {
				t.Fatalf("elem = %v, want %v", elem, tc.wantElem)
			}
		})
	}
}

// T2163: the classifier's accepted set must stay equal to
// ownership.isRefcountedHandle (internal/ownership/expr.go). That equality is
// the load-bearing invariant of language-design.md#ownership-across-goroutines's "`sharable types are the exception"
// rule: the ownership checker ACCEPTS a spawn that borrows one of these handles
// precisely because codegen promises to duplicate it. Let the two drift and the
// failure is silent in the worst direction — ownership admits a spawn codegen
// then refuses to retain, so the program compiles and reads freed memory, which
// is exactly the T2163 bug.
//
// isRefcountedHandle is unexported in another package, so this reads its source
// and asserts the two agree, rather than restating its membership as a literal
// that could rot independently (the same source-guard approach as
// TestT1583StructuralViewPredicateIsNotSpelledInline). Adding a fourth handle to
// ownership without teaching refcountedHandleElem about it fails here.
func TestT2163_AcceptedSetMatchesOwnership(t *testing.T) {
	src, err := os.ReadFile(filepath.Join("..", "ownership", "expr.go"))
	if err != nil {
		t.Fatalf("read ownership/expr.go: %v", err)
	}
	body := regexp.MustCompile(`(?s)func isRefcountedHandle\(typ types\.Type\) bool \{.*?\n\}`).
		Find(src)
	if body == nil {
		t.Fatal("isRefcountedHandle not found in ownership/expr.go — if it was renamed or " +
			"removed, this guard and refcountedHandleElem's doc comment both need updating")
	}

	// Every types.AsX probe ownership uses to admit a handle at the boundary.
	var ownershipAccepts []string
	for _, m := range regexp.MustCompile(`types\.As(\w+)\(`).FindAllSubmatch(body, -1) {
		ownershipAccepts = append(ownershipAccepts, string(m[1]))
	}
	sort.Strings(ownershipAccepts)
	if len(ownershipAccepts) == 0 {
		t.Fatal("no types.AsX probes found in isRefcountedHandle — the guard cannot see its set")
	}

	// One representative instance per probe name, so a new probe appearing in
	// ownership fails loudly here instead of silently widening the boundary.
	sample := map[string]types.Type{
		"Channel": newTestChannel(types.TypInt),
		"Arc":     types.NewArc(types.TypInt),
		"Weak":    types.NewWeak(types.TypInt),
		"Mutex":   types.NewMutex(types.TypInt),
		"Vector":  types.NewVector(types.TypInt),
	}

	c := &Compiler{}
	for _, name := range ownershipAccepts {
		typ, known := sample[name]
		if !known {
			t.Fatalf("ownership.isRefcountedHandle now accepts types.As%s, which this guard has "+
				"no sample for — add one and make sure refcountedHandleElem duplicates it too", name)
		}
		if _, _, ok := c.refcountedHandleElem(typ); !ok {
			t.Errorf("ownership accepts %v (types.As%s) but codegen refuses it: ownership would "+
				"admit the spawn while codegen skipped the retain — a use-after-free", typ, name)
		}
	}

	// ...and nothing beyond it. A handle codegen retains but ownership rejects
	// is at best dead weight, at worst a retain on a boundary never cleared.
	accepted := map[string]bool{}
	for _, n := range ownershipAccepts {
		accepted[n] = true
	}
	for name, typ := range sample {
		if accepted[name] {
			continue
		}
		if _, _, ok := c.refcountedHandleElem(typ); ok {
			t.Errorf("codegen accepts %v (types.As%s) but ownership.isRefcountedHandle rejects it",
				typ, name)
		}
	}
}

// T2163: retainSpawnHandle refuses without emitting, so a caller may offer it
// any type. The retain itself needs a live IR builder, but the refusal path
// must not touch one — that is what lets the spawn sites call it unconditionally
// and act on the answer. A regression here is a nil-block panic in codegen.
func TestT2163_RetainSpawnHandleRefusesWithoutEmitting(t *testing.T) {
	c := &Compiler{} // no module, no current block: emitting anything would panic
	for _, typ := range []types.Type{
		nil,
		types.TypInt,
		types.TypString,
		types.NewMutex(types.TypInt),
		types.NewOptional(types.NewArc(types.TypInt)),
		types.TypArc, // bare origin, no substitution → element unresolvable
	} {
		val, release, ok := c.retainSpawnHandle(nil, typ)
		if ok {
			t.Fatalf("%v: retained, want refused", typ)
		}
		if release != nil {
			t.Fatalf("%v: refusal returned a release function", typ)
		}
		if val != nil {
			t.Fatalf("%v: refusal altered the value", typ)
		}
	}
}
