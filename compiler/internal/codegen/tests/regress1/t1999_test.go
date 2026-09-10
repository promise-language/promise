package regress1

import (
	"testing"

	"github.com/promise-language/promise/compiler/internal/codegen/codegentest"
)

// T1999: `stream[T]` and the `Stream[T]` protocol are the SAME type (sema installs
// the lowercase spelling as an alias), so dropDiscardedGenerator's type test alone
// could not tell a raw coroutine pair from a structural view. A discarded VIEW
// reached __promise_gen_destroy with the value struct's field 0 — a vtable pointer
// — and segfaulted. isGeneratorFactoryCallExpr now gates the destroy on the
// discarded expression actually being a factory CALL.
//
// Both drivers are the free function `drive`, so the body is extracted with the
// package's existing t1467Body (t1467_test.go).
func TestT1999DiscardedStructuralStreamViewNotDestroyed(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
drive() {
  int[] v = [1, 2, 3];
  Stream[int] s = v;
  s;
}
main() { drive(); }
`)
	body := t1467Body(t, ir)
	codegentest.AssertNotContains(t, body, "discard.gen.cleanup")
	codegentest.AssertNotContains(t, body, "@__promise_gen_destroy")
}

// The gate must still see through every wrapper a discarded FACTORY call wears, or
// the coroutine frame and yield slot leak (T1306). One case per node
// isGeneratorFactoryCallExpr peels.
func TestT1999DiscardedGeneratorCallThroughWrappersStillDestroyed(t *testing.T) {
	for _, tc := range []struct{ name, discard, sig string }{
		{"bare", "gen(3);", ""},
		{"paren", "(gen(3));", ""},
		{"panic", "genf(3)?!;", ""},
		{"handler", "genf(3)? e { };", ""},
		{"propagate", "genf(3)?^;", "!"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mainSuffix := ""
			if tc.sig == "!" {
				mainSuffix = "?!" // a failable drive() must be handled at the call site
			}
			ir := codegentest.GenerateIR(t, `
gen(int n) stream[int] {
  int i = 0;
  while (i < n) { yield i; i += 1; }
}
genf!(int n) stream[int] {
  int i = 0;
  while (i < n) { if (i > 5) { raise error("x"); } yield i; i += 1; }
}
drive`+tc.sig+`() {
  `+tc.discard+`
}
main() { drive()`+mainSuffix+`; }
`)
			body := t1467Body(t, ir)
			codegentest.AssertContains(t, body, "discard.gen.cleanup")
			codegentest.AssertContains(t, body, "@__promise_gen_destroy")
		})
	}
}

// The peel and the call test are separate conditions: peeling a wrapper off a
// NON-call must still land on "not a factory". `(s);` over a structural view is
// the shape that distinguishes them — a gate that returned true as soon as it
// peeled something would destroy the view's vtable pointer.
func TestT1999ParenWrappedStructuralViewNotDestroyed(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
drive() {
  int[] v = [1, 2, 3];
  Stream[int] s = v;
  (s);
}
main() { drive(); }
`)
	body := t1467Body(t, ir)
	codegentest.AssertNotContains(t, body, "discard.gen.cleanup")
	codegentest.AssertNotContains(t, body, "@__promise_gen_destroy")
}
