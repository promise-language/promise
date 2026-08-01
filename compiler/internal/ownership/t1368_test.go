package ownership

import (
	"testing"

	"github.com/promise-language/promise/compiler/internal/ast"
	"github.com/promise-language/promise/compiler/internal/sema"
	"github.com/promise-language/promise/compiler/internal/types"
)

// T1368: `Der? d = b as Der` (non-optional subject → optional result) binds `d`
// as a borrow view aliasing `b`'s instance pointer (codegen gives it no drop
// flag). If `b`'s last *direct* textual use is before the last use of `d`, NLL
// early-drop (B0035) would free `b`'s instance while `d` is still read → UAF
// segfault. AnalyzeLastUses must extend `b`'s live range to cover its aliases and
// therefore NOT register `b` for early drop.

// Optional-result borrow-view cast: `b`'s direct last use (stmt 2, a copy-typed
// int result that would otherwise be a safe early-drop point) precedes the alias
// read (stmt 3). Without the T1368 guard `b` would be early-dropped after stmt 2
// and `d`'s read would touch freed memory.
func TestT1368OptionalCastSubjectNotEarlyDropped(t *testing.T) {
	names := analyzeLastUsesForSrc(t, `
		type Base { string name; drop(~this){} }
		type Der is Base { drop(~this){} }
		f() {
			Base b = Der(name: "x");
			Der? d = b as Der;
			a := b.name.len;
			c := d!.name.len;
		}
	`)
	if names["b"] {
		t.Errorf("cast subject 'b' was registered for early drop; want suppressed (alias 'd' still live)")
	}
}

// Force cast to a non-optional result (`as!`) is the same view relationship —
// confirms the fix is not optional-specific.
func TestT1368ForceCastSubjectNotEarlyDropped(t *testing.T) {
	names := analyzeLastUsesForSrc(t, `
		type Base { string name; drop(~this){} }
		type Der is Base { drop(~this){} }
		f() {
			Base b = Der(name: "x");
			Der d = b as! Der;
			a := b.name.len;
			c := d.name.len;
		}
	`)
	if names["b"] {
		t.Errorf("cast subject 'b' was registered for early drop; want suppressed (alias 'd' still live)")
	}
}

// Transitive alias: `Der e = d as! Der` where `d` itself views `b`. A use of `e`
// after `b`'s last direct use must still pin `b` alive (resolveAliasRoot follows
// e -> d -> b).
func TestT1368TransitiveAliasSubjectNotEarlyDropped(t *testing.T) {
	names := analyzeLastUsesForSrc(t, `
		type Base { string name; drop(~this){} }
		type Mid is Base { drop(~this){} }
		type Der is Mid { drop(~this){} }
		f() {
			Base b = Der(name: "x");
			Mid d = b as! Mid;
			Der e = d as! Der;
			a := b.name.len;
			c := e.name.len;
		}
	`)
	if names["b"] {
		t.Errorf("root subject 'b' was registered for early drop; want suppressed (transitive alias 'e' still live)")
	}
	if names["d"] {
		t.Errorf("intermediate subject 'd' was registered for early drop; want suppressed (alias 'e' still live)")
	}
}

// Boundary: the guard is narrow. Once the borrow-view alias's own last use has
// passed, a later subject-independent statement still allows the subject to be
// early-dropped — the union last use (stmt 2) is non-final and safe, so `b` IS
// early dropped after stmt 2. Pins that the fix is not a blanket suppression.
func TestT1368SubjectStillEarlyDroppedAfterAliasDead(t *testing.T) {
	names := analyzeLastUsesForSrc(t, `
		type Base { string name; drop(~this){} }
		type Der is Base { drop(~this){} }
		f() {
			Base b = Der(name: "x");
			Der d = b as! Der;
			a := d.name.len;
			k := 1;
			m := k + 1;
		}
	`)
	if !names["b"] {
		t.Errorf("subject 'b' should still be early-dropped after alias 'd' is dead; got %v", names)
	}
}

// Assignment-created alias: the borrow view is bound by a plain `=` to a
// pre-declared local (not a var-decl), so the aliasing is recorded via the
// AssignStmt/OpAssign branch of analyzeBlock. The subject `b`'s last *direct* use
// (stmt 3) precedes the alias read (stmt 4), so without the guard `b` would be
// early-dropped mid-block while `d` still views it.
func TestT1368AssignAliasSubjectNotEarlyDropped(t *testing.T) {
	names := analyzeLastUsesForSrc(t, `
		type Base { string name; drop(~this){} }
		type Der is Base { drop(~this){} }
		f() {
			Base b = Der(name: "x");
			Der? d = none;
			d = b as Der;
			a := b.name.len;
			c := d!.name.len;
		}
	`)
	if names["b"] {
		t.Errorf("cast subject 'b' (assign-aliased by 'd') was registered for early drop; want suppressed")
	}
}

// castBorrowViewRoot pins the borrow-view predicate directly, including the
// negative shapes that must NOT pin the subject: optional-unwrap casts (subject
// is `T?` — the cast owns the extracted inner), scalar/copy casts (fresh value),
// and call-result subjects (no bare root). Chained casts recurse to the innermost
// subject's root.
func TestT1368CastBorrowViewRoot(t *testing.T) {
	// A non-copy user Named type (default isCopy=false).
	named := types.NewNamed(types.NewTypeName(types.Pos{}, "Der", nil), nil)

	b := &ast.IdentExpr{Name: "b"}
	forceCast := &ast.CastExpr{Expr: b, Force: true}                                 // b as! Der
	optCast := &ast.CastExpr{Expr: b, Force: false}                                  // b as Der
	scalarCast := &ast.CastExpr{Expr: b, Force: false}                               // b as i32
	callSubj := &ast.CallExpr{Callee: &ast.MemberExpr{Target: b, Field: "get"}}      // b.get()
	callCast := &ast.CastExpr{Expr: callSubj, Force: true}                           // b.get() as! Der
	chained := &ast.CastExpr{Expr: &ast.CastExpr{Expr: b, Force: true}, Force: true} // (b as! Mid) as! Der

	a := &lastUseAnalyzer{info: &sema.Info{Types: map[ast.Expr]types.Type{
		b:          named,
		forceCast:  named,
		optCast:    types.NewOptional(named),
		scalarCast: named,
		callSubj:   named,
		callCast:   named,
		chained:    named,
	}}}
	// The inner subject of a chained cast is `b` (typed `named`); its type drives
	// the innermost check.

	tests := []struct {
		name string
		in   ast.Expr
		want string
	}{
		{"force cast of user type", forceCast, "b"},
		{"non-force cast of user type", optCast, "b"},
		{"chained cast recurses to root", chained, "b"},
		{"call-result subject has no root", callCast, ""},
		{"non-cast init", b, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := a.castBorrowViewRoot(tc.in); got != tc.want {
				t.Errorf("castBorrowViewRoot = %q, want %q", got, tc.want)
			}
		})
	}

	// Optional-unwrap subject: the cast owns the extracted inner, not a view.
	a.info.Types[b] = types.NewOptional(named)
	if got := a.castBorrowViewRoot(forceCast); got != "" {
		t.Errorf("optional-unwrap subject: castBorrowViewRoot = %q, want \"\"", got)
	}
	// Scalar/copy subject: fresh converted value, not a view.
	a.info.Types[b] = types.TypInt
	if got := a.castBorrowViewRoot(scalarCast); got != "" {
		t.Errorf("scalar subject: castBorrowViewRoot = %q, want \"\"", got)
	}
	// SharedRef-wrapped user subject (`&b as Der`): peel the ref layer (T0850)
	// and still recognize the non-copy inner as a borrow view → root "b".
	a.info.Types[b] = types.NewSharedRef(named)
	if got := a.castBorrowViewRoot(forceCast); got != "b" {
		t.Errorf("shared-ref subject: castBorrowViewRoot = %q, want \"b\"", got)
	}
	// MutRef-wrapped user subject: same peel path.
	a.info.Types[b] = types.NewMutRef(named)
	if got := a.castBorrowViewRoot(forceCast); got != "b" {
		t.Errorf("mut-ref subject: castBorrowViewRoot = %q, want \"b\"", got)
	}
	// SharedRef around a copy inner: after peeling, the inner is copy → fresh
	// value, not a view → "".
	a.info.Types[b] = types.NewSharedRef(types.TypInt)
	if got := a.castBorrowViewRoot(forceCast); got != "" {
		t.Errorf("shared-ref-of-copy subject: castBorrowViewRoot = %q, want \"\"", got)
	}
	// Subject absent from the type map: no type info → conservatively "".
	delete(a.info.Types, b)
	if got := a.castBorrowViewRoot(forceCast); got != "" {
		t.Errorf("untyped subject: castBorrowViewRoot = %q, want \"\"", got)
	}
}

// collectTransitiveRoots follows the alias chain to every reachable root and is
// cycle-safe.
func TestT1368CollectTransitiveRoots(t *testing.T) {
	// Chain e -> d -> b: e reaches both d and b.
	m := map[string][]string{"e": {"d"}, "d": {"b"}}
	if got := rootSet(collectTransitiveRoots(m, "e")); !got["b"] || !got["d"] {
		t.Errorf("collectTransitiveRoots(e) = %v, want {b, d}", got)
	}
	// A local that aliases nothing yields no roots.
	if got := collectTransitiveRoots(m, "z"); len(got) != 0 {
		t.Errorf("collectTransitiveRoots(z) = %v, want empty (not an alias)", got)
	}
	// Multiple roots for one alias (re-binding): d aliased both b and c.
	multi := map[string][]string{"d": {"b", "c"}}
	if got := rootSet(collectTransitiveRoots(multi, "d")); !got["b"] || !got["c"] {
		t.Errorf("collectTransitiveRoots(d) = %v, want {b, c}", got)
	}
	// Cycle protection: x -> y -> x must terminate.
	cyc := map[string][]string{"x": {"y"}, "y": {"x"}}
	_ = collectTransitiveRoots(cyc, "x")
}

func rootSet(roots []string) map[string]bool {
	m := make(map[string]bool, len(roots))
	for _, r := range roots {
		m[r] = true
	}
	return m
}
