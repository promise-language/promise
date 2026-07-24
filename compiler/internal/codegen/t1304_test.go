package codegen

import (
	"regexp"
	"strings"
	"testing"
)

// T1304: `r := pass_through(s)` where a free function returns an owned
// structural-interface param BY VALUE (`pass_through(Sink s) Sink { return s; }`)
// hands back a {vtable, instance} view that aliases the caller's still-owned arg
// box. The concrete→structural coercion at the call site is a borrow (not a
// move), so `s` stays the sole owner; but the binding path treated the CallExpr
// RHS as fresh-owned and registered a structural free for `r` over the SAME box →
// two owners, one box → double free at scope exit.
//
// The fix (maybeClearStructuralBindingAliasArg) emits a runtime alias guard at
// the binding site: if the result's instance ptr equals a live-owned call arg's
// instance ptr, clear r's structural free flag so s remains sole owner.

// The binding must contain an icmp of the result instance ptr against the
// argument's instance ptr feeding a conditional clear of r's structural free flag.
func TestT1304BindingClearsArgAlias(t *testing.T) {
	ir := generateIR(t, `
		type Sink `+"`"+`structural { emit(int n) `+"`"+`abstract; }
		type Counter { int total; emit(~this, int n) { this.total = this.total + n; } }
		pass_through(Sink s) Sink { return s; }
		binding_passthrough() {
			s := Counter(total: 5);
			r := pass_through(s);
			r.emit(1);
		}
	`)
	fn := extractFunction(ir, "__user.binding_passthrough")
	if fn == "" {
		t.Fatalf("could not extract @__user.binding_passthrough from IR:\n%s", ir)
	}
	// r is registered with its own structural free flag...
	assertContains(t, fn, "%r.dropflag")
	// ...and the arg-alias clear runtime guard is emitted for it.
	if !strings.Contains(fn, "struct.arg.alias.clear") || !strings.Contains(fn, "struct.arg.alias.skip") {
		t.Fatalf("expected arg-alias clear guard (struct.arg.alias.clear/skip), got:\n%s", fn)
	}
	// The guard compares two instance pointers (icmp eq) — the result vs the arg.
	if !strings.Contains(fn, "icmp eq ptr") && !strings.Contains(fn, "icmp eq i8*") {
		t.Fatalf("expected icmp eq of instance ptrs in alias guard, got:\n%s", fn)
	}
}

// A fresh-constructing structural return (`return Widget(...)`) has no call-arg
// alias, so the binding keeps its own structural free (frees independently). The
// arg-alias guard machinery still emits, but the runtime pointer compare fails and
// r frees its own box — no leak, no double free. Presence of r's own drop flag
// confirms the binding retains ownership.
func TestT1304FreshReturnKeepsOwnDrop(t *testing.T) {
	ir := generateIR(t, `
		type Sink `+"`"+`structural { emit(int n) `+"`"+`abstract; }
		type Widget { int id; emit(~this, int n) { this.id = this.id + n; } }
		fresh_return() Sink { return Widget(id: 10); }
		binding_fresh() {
			r := fresh_return();
			r.emit(3);
		}
	`)
	fn := extractFunction(ir, "__user.binding_fresh")
	if fn == "" {
		t.Fatalf("could not extract @__user.binding_fresh from IR:\n%s", ir)
	}
	// r owns and frees its own fresh box.
	assertContains(t, fn, "%r.dropflag")
	// No call argument to alias against → no arg-alias guard emitted (the callee
	// takes no owned structural params).
	assertNotContains(t, fn, "struct.arg.alias.clear")
}

// A failable passthrough bound with `?!` wraps the CallExpr in an ErrorPanicExpr;
// the peel loop must reach the underlying call and still emit the arg-alias guard
// so `s` stays sole owner. Covers the wrapper-peel branch of the fix.
func TestT1304FailableBindingPeelsWrapper(t *testing.T) {
	ir := generateIR(t, `
		type Sink `+"`"+`structural { emit(int n) `+"`"+`abstract; }
		type Counter { int total; emit(~this, int n) { this.total = this.total + n; } }
		pass_through_fail!(Sink s) Sink { return s; }
		binding_fail() {
			s := Counter(total: 5);
			r := pass_through_fail(s)?!;
			r.emit(1);
		}
	`)
	fn := extractFunction(ir, "__user.binding_fail")
	if fn == "" {
		t.Fatalf("could not extract @__user.binding_fail from IR:\n%s", ir)
	}
	// The ErrorPanicExpr wrapper is peeled to the call, guard still emitted.
	if !strings.Contains(fn, "struct.arg.alias.clear") || !strings.Contains(fn, "struct.arg.alias.skip") {
		t.Fatalf("expected arg-alias clear guard after peeling ?! wrapper, got:\n%s", fn)
	}
}

// A multi-arg passthrough where the first arg is a non-ident literal (`9`) must
// skip the literal (continue) and still emit the guard for the owned structural
// arg `s`. Covers the non-ident-arg continue plus per-arg guard emission.
func TestT1304MultiArgSkipsNonIdent(t *testing.T) {
	ir := generateIR(t, `
		type Sink `+"`"+`structural { emit(int n) `+"`"+`abstract; }
		type Counter { int total; emit(~this, int n) { this.total = this.total + n; } }
		pass_through2(int x, Sink s) Sink { return s; }
		binding_multi() {
			s := Counter(total: 5);
			r := pass_through2(9, s);
			r.emit(1);
		}
	`)
	fn := extractFunction(ir, "__user.binding_multi")
	if fn == "" {
		t.Fatalf("could not extract @__user.binding_multi from IR:\n%s", ir)
	}
	// Exactly one guard: the literal arg is skipped, only the owned structural arg
	// `s` gets a clear guard. Clear-block labels are uniquified (…clear.N:), so
	// count label definitions.
	clearLabels := regexp.MustCompile(`struct\.arg\.alias\.clear[.\d]*:`).FindAllString(fn, -1)
	if len(clearLabels) != 1 {
		t.Fatalf("expected exactly one arg-alias guard (literal arg skipped), got %d:\n%s",
			len(clearLabels), fn)
	}
}

// Error-propagate binding (`r := pass_through_fail(s)?^`) inside a failable caller
// wraps the CallExpr in an ErrorPropagateExpr; the peel loop must reach the call
// and emit the guard. Covers the ErrorPropagateExpr peel branch.
func TestT1304ErrorPropagatePeelsWrapper(t *testing.T) {
	ir := generateIR(t, `
		type Sink `+"`"+`structural { emit(int n) `+"`"+`abstract; }
		type Counter { int total; emit(~this, int n) { this.total = this.total + n; } }
		pass_through_fail!(Sink s) Sink { return s; }
		binding_propagate!() {
			s := Counter(total: 5);
			r := pass_through_fail(s)?^;
			r.emit(1);
		}
	`)
	fn := extractFunction(ir, "__user.binding_propagate")
	if fn == "" {
		t.Fatalf("could not extract @__user.binding_propagate from IR:\n%s", ir)
	}
	if !strings.Contains(fn, "struct.arg.alias.clear") {
		t.Fatalf("expected arg-alias clear guard after peeling ? wrapper, got:\n%s", fn)
	}
}

// A structural binding whose RHS is a plain identifier (`r := s`, not a call) must
// reach the `inner.(*ast.CallExpr)` guard and bail out with no arg-alias guard —
// there is no call whose args could alias the return. Covers the non-call return.
func TestT1304NonCallRHSNoGuard(t *testing.T) {
	ir := generateIR(t, `
		type Sink `+"`"+`structural { emit(int n) `+"`"+`abstract; }
		type Counter { int total; emit(~this, int n) { this.total = this.total + n; } }
		make_sink() Sink { return Counter(total: 5); }
		binding_ident() {
			s := make_sink();
			r := s;
			r.emit(1);
		}
	`)
	fn := extractFunction(ir, "__user.binding_ident")
	if fn == "" {
		t.Fatalf("could not extract @__user.binding_ident from IR:\n%s", ir)
	}
	assertNotContains(t, fn, "struct.arg.alias.clear")
}
