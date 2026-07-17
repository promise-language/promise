package codegen

import (
	"strings"
	"testing"
)

// T1306: a discarded generator factory call (`gen(3);`, return type `stream[int]`)
// is never iterated, so no for-in iter-cleanup runs and — before the fix — its raw
// {handle, slot} coroutine value leaked its yield slot and coroutine frame. The fix
// (dropDiscardedGenerator in stmt.go) emits the generator-native cleanup at statement
// end: a null-guarded `@__promise_gen_destroy(handle)` + `@pal_free(slot)` — the same
// shape as a consumed generator's bindingGenerator cleanup, NOT __promise_structural_drop
// (a generator box has a distinct _FnIter-shaped layout; routing it through RTTI
// structural drop would crash — see T1294 exclusion).

// A discarded non-failable generator must emit the {handle, slot} native cleanup:
// a `discard.gen.cleanup` block that destroys the coroutine handle and frees the
// yield slot, null-guarded so an empty {null, null} value is a no-op.
func TestT1306DiscardedGeneratorEmitsNativeCleanup(t *testing.T) {
	ir := generateIR(t, `
		gen(int n) stream[int] { int i = 0; while i < n { yield i; i = i + 1; } }
		build() { gen(3); }
	`)
	fn := extractDefine(ir, "__user.build")
	if fn == "" {
		t.Fatalf("could not extract @__user.build from IR:\n%s", ir)
	}
	if !strings.Contains(fn, "discard.gen.cleanup") {
		t.Fatalf("expected discarded `gen(3);` to emit a discard.gen.cleanup block; got:\n%s", fn)
	}
	if !strings.Contains(fn, "@__promise_gen_destroy") {
		t.Fatalf("expected discarded generator to destroy its coroutine handle via @__promise_gen_destroy; got:\n%s", fn)
	}
	if !strings.Contains(fn, "@pal_free") {
		t.Fatalf("expected discarded generator to free its yield slot via @pal_free; got:\n%s", fn)
	}
	// A non-failable generator is the 2-field {handle, slot} shape: exactly ONE
	// pal_free (the yield slot). The failable 3-field shape adds a second.
	if n := strings.Count(fn, "@pal_free"); n != 1 {
		t.Fatalf("expected exactly 1 @pal_free (yield slot) for a non-failable discarded generator, got %d:\n%s", n, fn)
	}
}

// A discarded FAILABLE generator is the 3-field {handle, slot, errslot} shape — the
// cleanup must additionally free the error slot (a second @pal_free) so the error
// channel does not leak (B0023 shape).
func TestT1306DiscardedFailableGeneratorFreesErrorSlot(t *testing.T) {
	ir := generateIR(t, `
		gen!(int n) stream[int] {
			if n < 0 { raise error("neg"); }
			int i = 0; while i < n { yield i; i = i + 1; }
		}
		build() { gen(3)?!; }
	`)
	fn := extractDefine(ir, "__user.build")
	if fn == "" {
		t.Fatalf("could not extract @__user.build from IR:\n%s", ir)
	}
	if !strings.Contains(fn, "discard.gen.cleanup") {
		t.Fatalf("expected discarded failable `gen(3)?!;` to emit a discard.gen.cleanup block; got:\n%s", fn)
	}
	// 3-field shape: yield slot + error slot ⇒ two @pal_free calls in the cleanup.
	if n := strings.Count(fn, "@pal_free"); n != 2 {
		t.Fatalf("expected 2 @pal_free (yield slot + error slot) for a discarded failable generator, got %d:\n%s", n, fn)
	}
	// The error slot is field 2 of the {handle, slot, errslot} value.
	if !strings.Contains(fn, "extractvalue { i8*, i8*, i8* }") {
		t.Fatalf("expected 3-field {handle, slot, errslot} extractvalue for failable generator cleanup; got:\n%s", fn)
	}
}

// Negative: a discarded NON-generator call (plain scalar return) must NOT emit any
// generator cleanup — dropDiscardedGenerator gates on AsStream(exprType).
func TestT1306NonGeneratorDiscardNoGenCleanup(t *testing.T) {
	ir := generateIR(t, `
		plain(int n) int { return n + 1; }
		build() { plain(2); }
	`)
	fn := extractDefine(ir, "__user.build")
	if fn == "" {
		t.Fatalf("could not extract @__user.build from IR:\n%s", ir)
	}
	if strings.Contains(fn, "discard.gen.cleanup") {
		t.Fatalf("expected discarded non-generator `plain(2);` NOT to emit generator cleanup; got:\n%s", fn)
	}
}

// Negative: a generator CONSUMED by a for-in loop is freed by the iter-cleanup path,
// not the ExprStmt discard path — no discard.gen.cleanup must be emitted (guards
// against a double-free from both paths firing).
func TestT1306IteratedGeneratorNoDiscardCleanup(t *testing.T) {
	ir := generateIR(t, `
		gen(int n) stream[int] { int i = 0; while i < n { yield i; i = i + 1; } }
		build() { for x in gen(3) {} }
	`)
	fn := extractDefine(ir, "__user.build")
	if fn == "" {
		t.Fatalf("could not extract @__user.build from IR:\n%s", ir)
	}
	if strings.Contains(fn, "discard.gen.cleanup") {
		t.Fatalf("expected for-in consumed generator NOT to emit ExprStmt discard cleanup; got:\n%s", fn)
	}
}
