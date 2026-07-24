package codegen

import (
	"strings"
	"testing"
)

// T1311: `f.make(Counter(total:5));` — a METHOD returns an owned structural param
// BY VALUE (`make(Sink s) Sink { return s; }`), the result is UNBOUND (discard),
// and the arg is a FRESH rvalue-temp (a CallExpr, not a named local). T1310's
// maybeClearStructuralTempAliasArg only scans IdentExpr args, so a fresh-temp arg
// gets no guard: maybeTrackIterTemp registers the result temp for
// __promise_structural_drop over the SAME box as the fresh arg temp → double free.
//
// The fix records each structural-return call's owned arg instance ptrs in
// emitReturnAliasCheckSubst (which bails early for structural returns before the
// normal arg loop) and consumes them in maybeTrackIterTemp, clearing ONLY the
// result temp's drop flag on a runtime ptr match — the arg temp stays sole owner.

// The discarded fresh-temp method call must track a structural temp AND emit the
// fresh-temp arg-alias clear guard comparing the result temp's instance ptr
// against the recorded arg ptr.
func TestT1311MethodDiscardFreshTempClearsResult(t *testing.T) {
	ir := generateIR(t, `
		type Sink `+"`"+`structural { emit(int n) `+"`"+`abstract; }
		type Counter { int total; emit(~this, int n) { this.total = this.total + n; } }
		type Factory { int seed; make(Sink s) Sink { return s; } }
		method_discard_freshtmp() {
			f := Factory(seed: 1);
			f.make(Counter(total: 5));
		}
	`)
	fn := extractFunction(ir, "__user.method_discard_freshtmp")
	if fn == "" {
		t.Fatalf("could not extract @__user.method_discard_freshtmp from IR:\n%s", ir)
	}
	// The structural result temp is tracked for statement-end cleanup.
	assertContains(t, fn, "@__promise_structural_drop")
	// ...and the fresh-temp arg-alias clear guard is emitted.
	if !strings.Contains(fn, "struct.tmp.freshalias.clear") || !strings.Contains(fn, "struct.tmp.freshalias.skip") {
		t.Fatalf("expected fresh-temp arg-alias clear guard (struct.tmp.freshalias.clear/skip), got:\n%s", fn)
	}
	// The guard compares two instance pointers (icmp eq).
	if !strings.Contains(fn, "icmp eq ptr") && !strings.Contains(fn, "icmp eq i8*") {
		t.Fatalf("expected icmp eq of instance ptrs in fresh-temp alias guard, got:\n%s", fn)
	}
}

// The inline-use form `f.make(Counter(total:5)).emit(1);` produces the same unbound
// fresh-temp result and must likewise emit the fresh-temp arg-alias guard.
func TestT1311MethodInlineFreshTempClearsResult(t *testing.T) {
	ir := generateIR(t, `
		type Sink `+"`"+`structural { emit(int n) `+"`"+`abstract; }
		type Counter { int total; emit(~this, int n) { this.total = this.total + n; } }
		type Factory { int seed; make(Sink s) Sink { return s; } }
		method_inline_freshtmp() {
			f := Factory(seed: 1);
			f.make(Counter(total: 5)).emit(1);
		}
	`)
	fn := extractFunction(ir, "__user.method_inline_freshtmp")
	if fn == "" {
		t.Fatalf("could not extract @__user.method_inline_freshtmp from IR:\n%s", ir)
	}
	if !strings.Contains(fn, "struct.tmp.freshalias.clear") || !strings.Contains(fn, "struct.tmp.freshalias.skip") {
		t.Fatalf("expected fresh-temp arg-alias guard for inline use, got:\n%s", fn)
	}
}

// A method that takes an owned structural arg but returns a FRESH box
// (`make_fresh(Sink s) Sink { s.emit(1); return Counter(...); }`) discarded with a
// fresh-temp arg present: the guard IS emitted (an owned structural arg was
// recorded) but the result's instance ptr differs from the arg's at runtime, so
// the ptr compare never fires — both boxes free independently. No leak, no double
// free.
func TestT1311MethodDiscardFreshReturnEmitsGuardDistinctPtr(t *testing.T) {
	ir := generateIR(t, `
		type Sink `+"`"+`structural { emit(int n) `+"`"+`abstract; }
		type Counter { int total; emit(~this, int n) { this.total = this.total + n; } }
		type Factory { int seed; make_fresh(Sink s) Sink { s.emit(1); return Counter(total: 99); } }
		method_fresh_return() {
			f := Factory(seed: 1);
			f.make_fresh(Counter(total: 5));
		}
	`)
	fn := extractFunction(ir, "__user.method_fresh_return")
	if fn == "" {
		t.Fatalf("could not extract @__user.method_fresh_return from IR:\n%s", ir)
	}
	// The fresh structural result is still tracked and freed.
	assertContains(t, fn, "@__promise_structural_drop")
	// An owned structural arg was passed → the fresh-temp guard is emitted; the
	// runtime distinct-ptr path frees both boxes independently.
	if !strings.Contains(fn, "struct.tmp.freshalias.clear") {
		t.Fatalf("expected fresh-temp arg-alias guard (owned structural arg present), got:\n%s", fn)
	}
}

// Two fresh rvalue-temp structural args, only the SECOND returned by value and
// discarded. Both args' instance ptrs are recorded, so the fresh-temp guard loop
// runs once PER arg — two clear blocks are emitted. At runtime only the matching
// ptr fires; the non-returned arg temp stays sole owner of its own box.
func TestT1311MethodDiscardFreshTempTwoArgsEmitsPerArgGuards(t *testing.T) {
	ir := generateIR(t, `
		type Sink `+"`"+`structural { emit(int n) `+"`"+`abstract; }
		type Counter { int total; emit(~this, int n) { this.total = this.total + n; } }
		type Factory { int seed; pick_second(Sink a, Sink b) Sink { return b; } }
		method_two_freshtmp() {
			f := Factory(seed: 1);
			f.pick_second(Counter(total: 1), Counter(total: 2));
		}
	`)
	fn := extractFunction(ir, "__user.method_two_freshtmp")
	if fn == "" {
		t.Fatalf("could not extract @__user.method_two_freshtmp from IR:\n%s", ir)
	}
	assertContains(t, fn, "@__promise_structural_drop")
	// The per-arg loop emits one guard per recorded arg ptr → at least two clear
	// blocks (LLVM suffixes the second: freshalias.clear, freshalias.clear1, ...).
	if n := strings.Count(fn, "struct.tmp.freshalias.clear"); n < 2 {
		t.Fatalf("expected >=2 per-arg fresh-temp clear guards for two structural args, got %d:\n%s", n, fn)
	}
}

// Generic-method fresh-temp passthrough discard (`f.wrap[int](Counter(...));`)
// reaches maybeTrackIterTemp via the generic-method codegen branch and must still
// emit the fresh-temp arg-alias guard so only the result temp is cleared.
func TestT1311GenericMethodDiscardFreshTempClearsResult(t *testing.T) {
	ir := generateIR(t, `
		type Sink `+"`"+`structural { emit(int n) `+"`"+`abstract; }
		type Counter { int total; emit(~this, int n) { this.total = this.total + n; } }
		type Factory { int seed; wrap[T](Sink s) Sink { return s; } }
		generic_freshtmp() {
			f := Factory(seed: 1);
			f.wrap[int](Counter(total: 5));
		}
	`)
	fn := extractFunction(ir, "__user.generic_freshtmp")
	if fn == "" {
		t.Fatalf("could not extract @__user.generic_freshtmp from IR:\n%s", ir)
	}
	assertContains(t, fn, "@__promise_structural_drop")
	if !strings.Contains(fn, "struct.tmp.freshalias.clear") {
		t.Fatalf("expected fresh-temp arg-alias guard for generic method, got:\n%s", fn)
	}
}

// A method with NO structural arg at all (`make_none() Sink { return
// Counter(...); }`) records nothing, so no fresh-temp guard is emitted — the temp
// frees its fresh box independently.
func TestT1311MethodDiscardNoArgNoGuard(t *testing.T) {
	ir := generateIR(t, `
		type Sink `+"`"+`structural { emit(int n) `+"`"+`abstract; }
		type Counter { int total; emit(~this, int n) { this.total = this.total + n; } }
		type Factory { int seed; make_none() Sink { return Counter(total: 7); } }
		method_none() {
			f := Factory(seed: 1);
			f.make_none();
		}
	`)
	fn := extractFunction(ir, "__user.method_none")
	if fn == "" {
		t.Fatalf("could not extract @__user.method_none from IR:\n%s", ir)
	}
	assertContains(t, fn, "@__promise_structural_drop")
	assertNotContains(t, fn, "struct.tmp.freshalias.clear")
}
