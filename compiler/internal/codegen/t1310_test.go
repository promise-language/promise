package codegen

import (
	"strings"
	"testing"
)

// T1310: `f.make(s);` where a METHOD returns an owned structural-interface param
// BY VALUE (`make(Sink s) Sink { return s; }`) and the result is UNBOUND (discard
// or inline-use). maybeTrackIterTemp registers the result's instance ptr as an
// owned heap temp freed at statement end via __promise_structural_drop, but the
// concrete→structural coercion is a borrow: the caller's `s` stays sole owner of
// the box → the temp free + s's scope-exit drop hit the SAME box → double free.
//
// The fix (maybeClearStructuralTempAliasArg, wired into maybeTrackIterTemp) emits
// a runtime alias guard on the tracked temp: if the temp's instance ptr equals a
// live-owned call arg's instance ptr, clear the temp's drop flag so `s` remains
// sole owner. This is the discard/inline sibling of T1304's binding-path fix, and
// unlike that sibling it admits a MemberExpr (method) callee.

// The discarded method call must track a structural temp AND emit the temp
// arg-alias clear guard comparing the temp's instance ptr against the arg's.
func TestT1310MethodDiscardClearsArgAlias(t *testing.T) {
	ir := generateIR(t, `
		type Sink `+"`"+`structural { emit(int n) `+"`"+`abstract; }
		type Counter { int total; emit(int n) { this.total = this.total + n; } }
		type Factory { int seed; make(Sink s) Sink { return s; } }
		method_discard() {
			f := Factory(seed: 1);
			s := Counter(total: 5);
			f.make(s);
			s.emit(1);
		}
	`)
	fn := extractFunction(ir, "__user.method_discard")
	if fn == "" {
		t.Fatalf("could not extract @__user.method_discard from IR:\n%s", ir)
	}
	// The structural result is tracked for statement-end cleanup.
	assertContains(t, fn, "@__promise_structural_drop")
	// ...and the temp arg-alias clear guard is emitted.
	if !strings.Contains(fn, "struct.tmp.alias.clear") || !strings.Contains(fn, "struct.tmp.alias.skip") {
		t.Fatalf("expected temp arg-alias clear guard (struct.tmp.alias.clear/skip), got:\n%s", fn)
	}
	// The guard compares two instance pointers (icmp eq).
	if !strings.Contains(fn, "icmp eq ptr") && !strings.Contains(fn, "icmp eq i8*") {
		t.Fatalf("expected icmp eq of instance ptrs in temp alias guard, got:\n%s", fn)
	}
}

// The inline-use form `f.make(s).emit(1);` produces the same unbound result and
// must likewise emit the temp arg-alias guard so `s` stays sole owner.
func TestT1310MethodInlineUseClearsArgAlias(t *testing.T) {
	ir := generateIR(t, `
		type Sink `+"`"+`structural { emit(int n) `+"`"+`abstract; }
		type Counter { int total; emit(int n) { this.total = this.total + n; } }
		type Factory { int seed; make(Sink s) Sink { return s; } }
		method_inline() {
			f := Factory(seed: 1);
			s := Counter(total: 5);
			f.make(s).emit(1);
			s.emit(1);
		}
	`)
	fn := extractFunction(ir, "__user.method_inline")
	if fn == "" {
		t.Fatalf("could not extract @__user.method_inline from IR:\n%s", ir)
	}
	if !strings.Contains(fn, "struct.tmp.alias.clear") || !strings.Contains(fn, "struct.tmp.alias.skip") {
		t.Fatalf("expected temp arg-alias clear guard for inline use, got:\n%s", fn)
	}
}

// Two owned structural args passed to a method that returns only one of them by
// value; the discarded result aliases exactly one arg's box. The guard loop
// scans every arg, so a guard pair is emitted for EACH owned local arg (the
// runtime ptr-equality picks which one actually clears). Assert two clear/skip
// guard pairs are present.
func TestT1310MethodDiscardMultiArgEmitsGuardPerArg(t *testing.T) {
	ir := generateIR(t, `
		type Sink `+"`"+`structural { emit(int n) `+"`"+`abstract; }
		type Counter { int total; emit(int n) { this.total = this.total + n; } }
		type Factory { int seed; pick_second(Sink a, Sink b) Sink { return b; } }
		method_two_args() {
			f := Factory(seed: 1);
			a := Counter(total: 1);
			b := Counter(total: 2);
			f.pick_second(a, b);
			a.emit(1);
			b.emit(1);
		}
	`)
	fn := extractFunction(ir, "__user.method_two_args")
	if fn == "" {
		t.Fatalf("could not extract @__user.method_two_args from IR:\n%s", ir)
	}
	// One clear-guard block per owned structural arg (two args → two guards).
	if n := strings.Count(fn, "struct.tmp.alias.clear"); n < 2 {
		t.Fatalf("expected a temp arg-alias guard per owned arg (>=2), got %d:\n%s", n, fn)
	}
}

// A paren-wrapped argument `f.make((s));` must still reach the bare identifier so
// the guard is emitted against the owned local `s`.
func TestT1310MethodDiscardParenArgClearsAlias(t *testing.T) {
	ir := generateIR(t, `
		type Sink `+"`"+`structural { emit(int n) `+"`"+`abstract; }
		type Counter { int total; emit(int n) { this.total = this.total + n; } }
		type Factory { int seed; make(Sink s) Sink { return s; } }
		method_paren() {
			f := Factory(seed: 1);
			s := Counter(total: 5);
			f.make((s));
			s.emit(1);
		}
	`)
	fn := extractFunction(ir, "__user.method_paren")
	if fn == "" {
		t.Fatalf("could not extract @__user.method_paren from IR:\n%s", ir)
	}
	if !strings.Contains(fn, "struct.tmp.alias.clear") || !strings.Contains(fn, "struct.tmp.alias.skip") {
		t.Fatalf("expected temp arg-alias guard for paren-wrapped arg, got:\n%s", fn)
	}
}

// A generic method (`wrap[T](Sink s) Sink { return s; }`) routes through a
// distinct codegen branch (genGenericMethodCall) but reaches the same temp
// tracking helper, so the discarded generic-method passthrough must also emit
// the arg-alias guard.
func TestT1310GenericMethodDiscardClearsArgAlias(t *testing.T) {
	ir := generateIR(t, `
		type Sink `+"`"+`structural { emit(int n) `+"`"+`abstract; }
		type Counter { int total; emit(int n) { this.total = this.total + n; } }
		type Factory { int seed; wrap[T](Sink s) Sink { return s; } }
		method_generic() {
			f := Factory(seed: 1);
			s := Counter(total: 5);
			f.wrap[int](s);
			s.emit(1);
		}
	`)
	fn := extractFunction(ir, "__user.method_generic")
	if fn == "" {
		t.Fatalf("could not extract @__user.method_generic from IR:\n%s", ir)
	}
	if !strings.Contains(fn, "struct.tmp.alias.clear") || !strings.Contains(fn, "struct.tmp.alias.skip") {
		t.Fatalf("expected temp arg-alias guard for generic method discard, got:\n%s", fn)
	}
}

// A fresh-constructing method return (`make_fresh(int n) Sink { return
// Counter(...); }`) has a distinct instance ptr and no owned structural arg, so
// no temp arg-alias guard is emitted — the temp frees its fresh box independently.
func TestT1310MethodDiscardFreshReturnNoGuard(t *testing.T) {
	ir := generateIR(t, `
		type Sink `+"`"+`structural { emit(int n) `+"`"+`abstract; }
		type Counter { int total; emit(int n) { this.total = this.total + n; } }
		type Factory { int seed; make_fresh(int n) Sink { return Counter(total: n); } }
		method_fresh() {
			f := Factory(seed: 1);
			f.make_fresh(9);
		}
	`)
	fn := extractFunction(ir, "__user.method_fresh")
	if fn == "" {
		t.Fatalf("could not extract @__user.method_fresh from IR:\n%s", ir)
	}
	// The fresh structural result is still tracked and freed.
	assertContains(t, fn, "@__promise_structural_drop")
	// No owned structural arg to alias against → no temp arg-alias guard.
	assertNotContains(t, fn, "struct.tmp.alias.clear")
}
