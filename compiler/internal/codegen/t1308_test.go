package codegen

import (
	"strings"
	"testing"
)

// T1308: `r := f.make(s)` where a METHOD returns an owned structural-interface
// param BY VALUE (`make(Sink s) Sink { return s; }`) hands back a
// {vtable, instance} view aliasing the caller's still-owned arg box — the exact
// sibling of T1304 but through a method call. The T1304 fix originally returned
// early for MemberExpr callees (only the receiver-alias path was considered
// covered), leaving the method ARGUMENT-passthrough case double-freeing.
//
// The fix removes that early return: for a MemberExpr callee, `call.Args` holds
// only the value arguments, so the existing arg-alias guard now fires for the
// method's owned structural args too (the receiver is a separate concern handled
// by maybeClearReceiverDropFlag).

// The binding must contain an icmp of the result instance ptr against the
// method-argument's instance ptr feeding a conditional clear of r's structural
// free flag.
func TestT1308MethodBindingClearsArgAlias(t *testing.T) {
	ir := generateIR(t, `
		type Sink `+"`"+`structural { emit(int n) `+"`"+`abstract; }
		type Counter { int total; emit(~this, int n) { this.total = this.total + n; } }
		type Factory { int seed; make(Sink s) Sink { return s; } }
		method_binding_passthrough() {
			f := Factory(seed: 1);
			s := Counter(total: 5);
			r := f.make(s);
			r.emit(1);
		}
	`)
	fn := extractFunction(ir, "__user.method_binding_passthrough")
	if fn == "" {
		t.Fatalf("could not extract @__user.method_binding_passthrough from IR:\n%s", ir)
	}
	// r is registered with its own structural free flag...
	assertContains(t, fn, "%r.dropflag")
	// ...and the arg-alias clear runtime guard is emitted for it (method arg `s`).
	if !strings.Contains(fn, "struct.arg.alias.clear") || !strings.Contains(fn, "struct.arg.alias.skip") {
		t.Fatalf("expected arg-alias clear guard (struct.arg.alias.clear/skip), got:\n%s", fn)
	}
	// The guard compares two instance pointers (icmp eq) — the result vs the arg.
	if !strings.Contains(fn, "icmp eq ptr") && !strings.Contains(fn, "icmp eq i8*") {
		t.Fatalf("expected icmp eq of instance ptrs in alias guard, got:\n%s", fn)
	}
}

// A method with no owned structural arg that returns a fresh construction
// (`make(int n) Sink { return Widget(id: n); }`) has no call-arg alias, so the
// binding keeps its own structural free (frees independently). No owned
// structural argument means the arg-alias guard has nothing to emit against.
func TestT1308MethodFreshReturnKeepsOwnDrop(t *testing.T) {
	ir := generateIR(t, `
		type Sink `+"`"+`structural { emit(int n) `+"`"+`abstract; }
		type Widget { int id; emit(~this, int n) { this.id = this.id + n; } }
		type Factory { int seed; make(int n) Sink { return Widget(id: n); } }
		method_binding_fresh() {
			f := Factory(seed: 1);
			r := f.make(7);
			r.emit(3);
		}
	`)
	fn := extractFunction(ir, "__user.method_binding_fresh")
	if fn == "" {
		t.Fatalf("could not extract @__user.method_binding_fresh from IR:\n%s", ir)
	}
	// r owns and frees its own fresh box.
	assertContains(t, fn, "%r.dropflag")
	// No owned structural call argument to alias against → no arg-alias guard.
	assertNotContains(t, fn, "struct.arg.alias.clear")
}

// A method with TWO owned structural args exercises the arg loop in
// maybeClearStructuralBindingAliasArg — a guard must be emitted for EACH owned
// structural arg (the runtime pointer compare then fires for whichever one the
// method actually returns). Single-arg tests above never exercise the loop past
// one iteration; this locks in that both args get an independent guard so a
// return of the non-first arg is still neutralized.
func TestT1308MethodMultiArgEmitsGuardPerArg(t *testing.T) {
	ir := generateIR(t, `
		type Sink `+"`"+`structural { emit(int n) `+"`"+`abstract; }
		type Counter { int total; emit(~this, int n) { this.total = this.total + n; } }
		type Factory { int seed; pick_second(Sink a, Sink b) Sink { return b; } }
		method_multi_arg() {
			f := Factory(seed: 1);
			a := Counter(total: 1);
			b := Counter(total: 2);
			r := f.pick_second(a, b);
			r.emit(1);
		}
	`)
	fn := extractFunction(ir, "__user.method_multi_arg")
	if fn == "" {
		t.Fatalf("could not extract @__user.method_multi_arg from IR:\n%s", ir)
	}
	assertContains(t, fn, "%r.dropflag")
	// Two owned structural args → two clear guards (LLVM auto-suffixes the second
	// block name), so at least two "struct.arg.alias.clear" occurrences appear.
	if n := strings.Count(fn, "struct.arg.alias.clear"); n < 2 {
		t.Fatalf("expected an arg-alias clear guard per owned structural arg (>=2), got %d:\n%s", n, fn)
	}
}
