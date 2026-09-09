package regress1

import (
	"regexp"
	"strings"
	"testing"

	"github.com/promise-language/promise/compiler/internal/codegen/codegentest"
)

// T1516: a mut-ref (`T ~p`) parameter on a generator read garbage after the first
// resume. The cause was an ABI mismatch, not a lifetime problem: the factory
// forwards a pointer to the caller's storage (B0149), but buildGeneratorCoroutine
// built the ramp's parameter list with resolveType — which strips MutRef — instead
// of resolveParamType. The ramp therefore declared the *pointee* type and the body
// read the caller's address as a value.
//
// The defect is invisible at the source level, so these tests assert on IR shape:
// the ramp's declared parameter type, its agreement with the factory's call, and
// the state hygiene half of the fix (an enclosing function's mut-ref bindings must
// not leak into a generator body compiled after it).

// t1516Ramp finds the first `.generator.N` ramp definition in ir and returns its
// symbol name and declared parameter list. The counter in the symbol is not stable
// across builds, so tests match the shape rather than a fixed name. Each source
// below declares exactly one generator, and the callers pin the full parameter
// list, so a ramp from elsewhere would fail loudly rather than pass silently.
var t1516RampHeader = regexp.MustCompile(`define i8\* @(\.generator\.[0-9]+)\(([^)]*)\) presplitcoroutine`)

func t1516Ramp(t *testing.T, ir string) (name, params string) {
	t.Helper()
	m := t1516RampHeader.FindStringSubmatch(ir)
	if m == nil {
		t.Fatalf("no user generator ramp found in IR")
	}
	return m[1], m[2]
}

// t1516MutRefSrc is the item's repro: a generator with an `int ~k` parameter,
// consumed by a for-in so the factory is actually called. Shared by the two tests
// that inspect the ramp's signature and the factory's forward of it.
const t1516MutRefSrc = `
	mut_int_items(int n, int ~k) stream[int] {
		int i = 0;
		while (i < n) { yield k + i; i += 1; }
	}
	main() {
		int k = 5;
		for x in mut_int_items(2, k) { consume(x); }
	}
	consume(int v) {}
`

// The ramp must declare `i64* %k`, not `i64 %k` — matching what the factory passes.
func TestMutRefGeneratorRampDeclaresPointerParam(t *testing.T) {
	ir := codegentest.GenerateIR(t, t1516MutRefSrc)
	name, params := t1516Ramp(t, ir)
	want := "i64 %n, i64* %k, i8* %yield_slot"
	if params != want {
		t.Errorf("ramp @%s params = %q, want %q", name, params, want)
	}
}

// The factory's forward and the ramp's signature must agree — the mismatch between
// them is exactly what regressed.
func TestMutRefGeneratorFactoryAndRampAgree(t *testing.T) {
	ir := codegentest.GenerateIR(t, t1516MutRefSrc)
	name, params := t1516Ramp(t, ir)

	factory := codegentest.ExtractDefine(ir, "__user.mut_int_items")
	if factory == "" {
		t.Fatalf("factory @__user.mut_int_items not found")
	}
	// The factory itself takes the pointer through from its caller.
	codegentest.AssertContains(t, factory, "@__user.mut_int_items(i64 %n, i64* %k)")

	// Derive the expected call from the ramp's OWN declared parameter list, so this
	// fails whenever the two drift apart in either direction. The trailing yield
	// slot is a factory-local SSA value, every earlier argument is forwarded verbatim.
	declared := strings.Split(params, ", ")
	if len(declared) < 2 {
		t.Fatalf("ramp @%s has too few params: %q", name, params)
	}
	forwarded := strings.Join(declared[:len(declared)-1], ", ")
	call := regexp.MustCompile(`call i8\* @` + regexp.QuoteMeta(name) + `\(` +
		regexp.QuoteMeta(forwarded) + `, i8\* %[0-9]+\)`)
	if !call.MatchString(factory) {
		t.Errorf("factory call does not match ramp @%s signature %q\n%s", name, params, factory)
	}
	// And that agreed-on shape is the pointer one, not the pointee one.
	if forwarded != "i64 %n, i64* %k" {
		t.Errorf("ramp/factory agree on %q, want %q", forwarded, "i64 %n, i64* %k")
	}
}

// The body must read and write through the pointer, never through a frame copy —
// a `~` param has to keep aliasing the caller across every suspend.
func TestMutRefGeneratorBodyUsesPointerDirectly(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		mut_int_items(int n, int ~k) stream[int] {
			int i = 0;
			while (i < n) { yield k + i; i += 1; }
			k += 20;
		}
		main() {
			int k = 5;
			for x in mut_int_items(2, k) { consume(x); }
		}
		consume(int v) {}
	`)
	name, _ := t1516Ramp(t, ir)
	ramp := codegentest.ExtractDefine(ir, name)
	if ramp == "" {
		t.Fatalf("ramp @%s body not found", name)
	}
	// No spill of the pointee into the coroutine frame.
	codegentest.AssertNotContains(t, ramp, "%k.addr")
	// Reads load through the caller's pointer...
	codegentest.AssertContainsMatch(t, ramp, `load i64, i64\* %k`)
	// ...and the write-back stores through it.
	codegentest.AssertContainsMatch(t, ramp, `store i64 %[0-9]+, i64\* %k`)
}

// State hygiene: an enclosing function's mut-ref binding must not leak into a
// generator compiled after it. Before the fix, the by-value `k` below was resolved
// through @__user.enclosing's `i64* %k` param — a cross-function reference, i.e.
// invalid IR. tests/e2e/t1516_mutref_generator_test.pr covers the same ordering at
// runtime; this pins the IR shape, which is where the corruption is visible.
func TestMutRefFromEnclosingFuncDoesNotLeakIntoGenerator(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		enclosing(int ~k) { k += 1; }
		plain_items(int n, int k) stream[int] {
			int i = 0;
			while (i < n) { yield k + i; i += 1; }
		}
		main() {
			int a = 1;
			enclosing(a);
			for x in plain_items(2, a) { consume(x); }
		}
		consume(int v) {}
	`)
	name, params := t1516Ramp(t, ir)
	if params != "i64 %n, i64 %k, i8* %yield_slot" {
		t.Fatalf("by-value generator ramp params = %q", params)
	}
	ramp := codegentest.ExtractDefine(ir, name)
	if ramp == "" {
		t.Fatalf("ramp @%s body not found", name)
	}
	// The by-value param is spilled to the frame and read from there...
	codegentest.AssertContains(t, ramp, "%k.addr = alloca i64")
	codegentest.AssertContainsMatch(t, ramp, `load i64, i64\* %k\.addr`)
	// ...never through the enclosing function's mut-ref pointer. `(?m)` is load-
	// bearing: Go's `$` is end-of-TEXT without it, so the anchor that separates
	// `%k` from `%k.addr` would never match and the assertion would be vacuous.
	codegentest.AssertNotContainsMatch(t, ramp, `(?m)load i64, i64\* %k$`)
}

// t1516AllRamps returns every user `.generator.N` ramp in ir as (name, params)
// pairs, in definition order. std's own generators are named
// `.generator.__mod_std_Random.ints.0`, so the digits-only suffix in the pattern
// already excludes them. Tests with two generators need all of them, not just the
// first one t1516Ramp returns.
func t1516AllRamps(t *testing.T, ir string) [][2]string {
	t.Helper()
	ms := t1516RampHeader.FindAllStringSubmatch(ir, -1)
	if len(ms) == 0 {
		t.Fatalf("no user generator ramp found in IR")
	}
	out := make([][2]string, 0, len(ms))
	for _, m := range ms {
		out = append(out, [2]string{m[1], m[2]})
	}
	return out
}

// A generator *method* prepends `this`, so the mut-ref pointer has to land at
// index 1, not 0. buildGeneratorCoroutine tracks that with a paramIdx it advances
// separately for the receiver, the by-value params and the mut-ref params; this
// pins the resulting order.
func TestMutRefGeneratorMethodRampParamOrder(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Counter {
			int base;
			scaled(this, int n, int ~k) stream[int] {
				int i = 0;
				while (i < n) { yield this.base + k + i; i += 1; }
				k += 100;
			}
		}
		main() {
			Counter c = Counter(base: 1);
			int k = 2;
			for x in c.scaled(1, k) { consume(x); }
		}
		consume(int v) {}
	`)
	name, params := t1516Ramp(t, ir)
	want := "i8* %this, i64 %n, i64* %k, i8* %yield_slot"
	if params != want {
		t.Errorf("method ramp @%s params = %q, want %q", name, params, want)
	}
	ramp := codegentest.ExtractDefine(ir, name)
	if ramp == "" {
		t.Fatalf("ramp @%s body not found", name)
	}
	// The receiver is still spilled to the frame; only the mut-ref is not.
	codegentest.AssertContains(t, ramp, "%this.addr")
	codegentest.AssertNotContains(t, ramp, "%k.addr")
	codegentest.AssertContainsMatch(t, ramp, `store i64 %[0-9]+, i64\* %k`)
}

// A failable generator appends an error slot after the yield slot. Interleaving
// by-value and mut-ref params must not disturb either trailing slot, and each
// mut-ref must become a pointer to its own pointee type — `string ~s` is `i8**`,
// one indirection past the `i8*` a by-value string uses.
func TestFailableMutRefGeneratorRampParamOrder(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		f!(int a, int ~k, string b, string ~s) stream[int] {
			int i = 0;
			while (i < 2) { yield k + a + b.len + s.len; i += 1; }
		}
		main() {
			int k = 2;
			string s = "x";
			for y in f(1, k, "b", s)?! { consume(y); }
		}
		consume(int v) {}
	`)
	name, params := t1516Ramp(t, ir)
	want := "i64 %a, i64* %k, i8* %b, i8** %s, i8* %yield_slot, i8* %error_slot"
	if params != want {
		t.Errorf("failable ramp @%s params = %q, want %q", name, params, want)
	}
	// The factory declares the same prefix, so the forward still lines up.
	factory := codegentest.ExtractDefine(ir, "__user.f")
	if factory == "" {
		t.Fatalf("factory @__user.f not found")
	}
	codegentest.AssertContains(t, factory, "@__user.f(i64 %a, i64* %k, i8* %b, i8** %s)")
}

// A mut-ref is a borrow, so the generator must not drop it — a drop here would
// free storage the caller still owns. The generator below has exactly one heap
// value in scope, the borrowed `s`, so any string drop in the ramp is that bug.
func TestMutRefGeneratorDoesNotDropBorrowedParam(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		g(int n, string ~s) stream[int] {
			int i = 0;
			while (i < n) { yield s.len + i; i += 1; }
		}
		main() {
			string s = "abc";
			for y in g(2, s) { consume(y); }
		}
		consume(int v) {}
	`)
	name, _ := t1516Ramp(t, ir)
	ramp := codegentest.ExtractDefine(ir, name)
	if ramp == "" {
		t.Fatalf("ramp @%s body not found", name)
	}
	codegentest.AssertNotContains(t, ramp, "promise_string_drop")
	codegentest.AssertNotContains(t, ramp, "%s.addr")
	codegentest.AssertContainsMatch(t, ramp, `load i8\*, i8\*\* %s`)
}

// The `continue` that skips a mut-ref param must skip only that param. A by-value
// param sitting beside one still needs its frame spill, or it would be read as an
// uninitialized SSA value after the first resume — the mirror image of the
// original defect. Both params are strings so the contrast is purely ownership.
func TestByValueGeneratorParamStillSpilledAlongsideMutRef(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		g(string owned, string ~borrowed) stream[int] {
			int i = 0;
			while (i < 2) { yield owned.len + borrowed.len; i += 1; }
		}
		main() {
			string s = "x";
			for y in g("o", s) { consume(y); }
		}
		consume(int v) {}
	`)
	name, params := t1516Ramp(t, ir)
	if params != "i8* %owned, i8** %borrowed, i8* %yield_slot" {
		t.Fatalf("ramp @%s params = %q", name, params)
	}
	ramp := codegentest.ExtractDefine(ir, name)
	if ramp == "" {
		t.Fatalf("ramp @%s body not found", name)
	}
	// The owned param is copied into the frame and read from the copy...
	codegentest.AssertContains(t, ramp, "%owned.addr = alloca i8*")
	codegentest.AssertContainsMatch(t, ramp, `load i8\*, i8\*\* %owned\.addr`)
	// ...while the borrow is read straight through the caller's pointer.
	codegentest.AssertNotContains(t, ramp, "%borrowed.addr")
	codegentest.AssertContainsMatch(t, ramp, `load i8\*, i8\*\* %borrowed`)
}

// Two generators in one file, the first with a mut-ref param and the second
// without: c.mutRefPtrs is reset on entry to each buildGeneratorCoroutine, so the
// second must not resolve its identically-named `k` through the first's pointer.
// This is the generator-to-generator direction of the same state-hygiene bug that
// TestMutRefFromEnclosingFuncDoesNotLeakIntoGenerator covers function-to-generator.
func TestMutRefDoesNotLeakBetweenTwoGenerators(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		first(int n, int ~k) stream[int] {
			int i = 0;
			while (i < n) { yield k + i; i += 1; }
		}
		second(int n, int k) stream[int] {
			int i = 0;
			while (i < n) { yield k + i; i += 1; }
		}
		main() {
			int a = 1;
			for x in first(2, a) { consume(x); }
			for y in second(2, a) { consume(y); }
		}
		consume(int v) {}
	`)
	ramps := t1516AllRamps(t, ir)
	if len(ramps) != 2 {
		t.Fatalf("want 2 user ramps, got %d: %v", len(ramps), ramps)
	}
	if ramps[0][1] != "i64 %n, i64* %k, i8* %yield_slot" {
		t.Errorf("first ramp params = %q", ramps[0][1])
	}
	if ramps[1][1] != "i64 %n, i64 %k, i8* %yield_slot" {
		t.Errorf("second ramp params = %q", ramps[1][1])
	}
	second := codegentest.ExtractDefine(ir, ramps[1][0])
	if second == "" {
		t.Fatalf("ramp @%s body not found", ramps[1][0])
	}
	// The by-value generator reads its own frame copy...
	codegentest.AssertContains(t, second, "%k.addr = alloca i64")
	// ...never a bare `%k` pointer inherited from the first generator. `(?m)` is
	// load-bearing, as in the enclosing-function test above.
	codegentest.AssertNotContainsMatch(t, second, `(?m)load i64, i64\* %k$`)
}
