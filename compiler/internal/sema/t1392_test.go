package sema

import (
	"strings"
	"testing"
)

// T1392: a `return` inside a `go`/`go!` BLOCK terminates the goroutine, not the
// enclosing function (codegen branches it to the coroutine return block, B0353).
// A bare `return;` therefore carries no value and must be validated against the
// BLOCK's result type, not c.curFunc.Result(). Two defects followed from the old
// c.curFunc binding:
//
//  1. missed error — a bare `return;` in a value-producing block inside a VOID
//     enclosing function compiled clean, and `<-t` silently yielded a meaningless
//     zero where the analogous value function (`f() int { return; }`) errors.
//  2. misattributed error — the same statement in a VALUE-returning enclosing
//     function reported the ENCLOSING function's result type, and a legitimate
//     bare `return;` in a VOID go block was wrongly rejected.
//
// The rule keys on the BLOCK alone (§17.2), never on whether the task handle is
// received: a discarded fire-and-forget `go { … };` statement gets the identical
// diagnostic. The trailing `;` is insignificant in Promise, so `go { f(x); }` with
// a value-returning `f` really is value-producing; the repair is to make the block
// void — end it with a bare `return;`, or bind the trailing call.

// 1. Non-failable value block, bare return → error against the BLOCK's type.
func TestT1392NonFailableValueGoBlockBareReturnRejected(t *testing.T) {
	errs := checkErrs(t, `
		main() {
			n := 5;
			t := go {
				if n > 0 { return; }
				n + 100
			};
			v := <-t;
		}
	`)
	expectError(t, errs, "missing return value (expected int)")
}

// 2. Failable `go! {}` value block, bare return → same diagnostic.
func TestT1392FailableValueGoBlockBareReturnRejected(t *testing.T) {
	errs := checkErrs(t, `
		produce!(int x) int { if x < 0 { raise error(message: "neg"); } return x; }
		main() {
			n := 5;
			t := go! {
				base := produce(1)?^;
				if n > 0 { return; }
				base + n
			};
		}
	`)
	expectError(t, errs, "missing return value (expected int)")
}

// 3. Bare returns nested in a match arm and an error handler are still seen — the
// recording happens during checkBlock, so no per-statement-kind walk is needed. The
// verdict is per BLOCK (the block's `T` is only known once its body is checked), so
// the first bare return carries the single diagnostic.
func TestT1392NestedBareReturnsInValueGoBlockRejected(t *testing.T) {
	errs := checkErrs(t, `
		produce!(int x) int { if x < 0 { raise error(message: "neg"); } return x; }
		main() {
			n := 5;
			t := go {
				match n {
					0 => {
						return;
					},
					_ => { },
				}
				produce(n) ? e {
					return;
				};
				n + 1
			};
			v := <-t;
		}
	`)
	expectError(t, errs, "missing return value (expected int)")
	count := 0
	for _, e := range errs {
		if strings.Contains(e.Error(), "missing return value (expected int)") {
			count++
		}
	}
	if count != 1 {
		t.Errorf("expected one per-block diagnostic on the first nested bare return, got %d:\n%v", count, errs)
	}
}

// 4. A discarded fire-and-forget `go { … };` statement is rejected exactly like a
// bound one: §17.2 keys the rule on the block, not on whether the handle is
// received, so the same body cannot be legal as `go {…};` and illegal as
// `t := go {…};`.
func TestT1392FireAndForgetValueGoBlockBareReturnRejected(t *testing.T) {
	errs := checkErrs(t, `
		compute(int n) int { return n * 2; }
		main() {
			n := 5;
			go { if n > 0 { return; } compute(n) };
		}
	`)
	expectError(t, errs, "missing return value (expected int)")
}

// 4b. …and the same body with the trailing `;` — which is insignificant in Promise
// (`f(x)` and `f(x);` are the same statement), so the block is equally
// value-producing and equally rejected. This is the shape the removed
// fire-and-forget carve-out used to accept.
func TestT1392FireAndForgetValueGoBlockTrailingSemicolonAlsoRejected(t *testing.T) {
	errs := checkErrs(t, `
		compute(int n) int { return n * 2; }
		main() {
			n := 5;
			go { if n > 0 { return; } compute(n); };
		}
	`)
	expectError(t, errs, "missing return value (expected int)")
}

// 4c. The two documented repairs, both of which make the block void: end it with a
// bare `return;`, or bind the trailing call so the last statement is not an
// expression. The diagnostic names the first one.
func TestT1392FireAndForgetValueGoBlockRepairsAccepted(t *testing.T) {
	errs := checkErrs(t, `
		compute(int n) int { return n * 2; }
		main() {
			n := 5;
			go { if n > 0 { return; } compute(n); return; };
			go { if n > 0 { return; } ignored := compute(n); };
		}
	`)
	expectNoErrors(t, errs)
}

// 5. A bare return in a VOID go block inside a VALUE-returning enclosing function
// is legal — it terminates the goroutine, not the function. This errored before
// the fix (misattributing the enclosing function's `int` result).
func TestT1392VoidGoBlockBareReturnInValueFuncAccepted(t *testing.T) {
	errs := checkErrs(t, `
		log_it(int n) void { }
		f() int {
			n := 1;
			t := go { if n > 0 { return; } log_it(n); };
			<-t;
			return 7;
		}
		main() { v := f(); }
	`)
	expectNoErrors(t, errs)
}

// 6. A `return;` inside a LAMBDA declared in a value-producing go block binds to
// the lambda's own (void) signature, not to the enclosing block.
func TestT1392LambdaBareReturnInsideValueGoBlockAccepted(t *testing.T) {
	errs := checkErrs(t, `
		log_it(int n) void { }
		main() {
			t := go {
				f := |int x| -> void { if x > 0 { return; } log_it(x); };
				f(1);
				42
			};
			v := <-t;
		}
	`)
	expectNoErrors(t, errs)
}

// 7. Only the BARE form draws this diagnostic. `return <expr>;` is §17.2's
// explicit-return style — a first-class producer of the block's result (T1385) — so a
// body that produces on every path through `return` is accepted, not rejected.
func TestT1392ValueReturnInGoBlockNotRejectedHere(t *testing.T) {
	errs := checkErrs(t, `
		f() int {
			n := 1;
			t := go {
				if n > 0 { return 5; }
				return n + 100;
			};
			v := <-t;
			return 0;
		}
		main() { v := f(); }
	`)
	expectNoErrorContaining(t, errs, "cannot return a value from a `go` block")
	expectNoErrorContaining(t, errs, "missing return value")
}

// 8. `return <expr>;` in a LAMBDA inside a go block still binds to the lambda.
func TestT1392ValueReturnInLambdaInsideGoBlockAccepted(t *testing.T) {
	errs := checkErrs(t, `
		main() {
			t := go {
				f := |int x| -> int { return x * 2; };
				f(1)
			};
			v := <-t;
		}
	`)
	expectNoErrors(t, errs)
}

// 10. The error position points at the `return` statement, not at the go block.
func TestT1392BareReturnErrorPositionIsTheReturn(t *testing.T) {
	errs := checkErrs(t, `
		main() {
			n := 5;
			t := go {
				n + 1;
				if n > 0 {
					return;
				}
				n + 100
			};
			v := <-t;
		}
	`)
	expectError(t, errs, "missing return value (expected int)")
	found := false
	for _, e := range errs {
		msg := e.Error()
		if strings.Contains(msg, "missing return value") && strings.Contains(msg, ":7:") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected the error at line 7 (the `return`), got:\n%v", errs)
	}
}

// countErrors reports how many of errs contain sub.
func countErrors(errs []error, sub string) int {
	n := 0
	for _, e := range errs {
		if strings.Contains(e.Error(), sub) {
			n++
		}
	}
	return n
}

// 11. A `go {}` nested in a GENERATOR function. The go-block branch is placed ahead
// of checkReturnStmt's `c.inGenerator` branch precisely so the goroutine's `return`
// is not swallowed as a generator terminator (which accepts any bare return) — the
// block's own result type must still be enforced.
func TestT1392BareReturnInValueGoBlockInsideGeneratorRejected(t *testing.T) {
	errs := checkErrs(t, `
		gen() stream[int] {
			t := go {
				if 1 > 0 { return; }
				42
			};
			v := <-t;
			yield v;
		}
		main() { for x in gen() { } }
	`)
	expectError(t, errs, "missing return value (expected int)")
}

// 12. The generator's OWN bare return (the terminator) is untouched — the go-block
// branch must not capture returns that sit outside a `go {}` body.
func TestT1392GeneratorOwnBareReturnStillAccepted(t *testing.T) {
	errs := checkErrs(t, `
		log_it(int n) void { }
		gen() stream[int] {
			t := go { log_it(1); if 1 > 0 { return; } };
			<-t;
			yield 1;
			return;
		}
		main() { for x in gen() { } }
	`)
	expectNoErrors(t, errs)
}

// 13. Nested `go {}` blocks: the save/restore around the body check must keep each
// block's bare returns bound to its OWN result type — the inner value block errors,
// the outer void block's own bare return does not, and neither leaks into the other.
func TestT1392NestedGoBlocksBindReturnsToTheirOwnBlock(t *testing.T) {
	errs := checkErrs(t, `
		log_it(int n) void { }
		main() {
			n := 1;
			outer := go {
				inner := go {
					if n > 0 { return; }
					n + 1
				};
				<-inner;
				log_it(n);
				if n > 0 { return; }
			};
			<-outer;
		}
	`)
	expectError(t, errs, "missing return value (expected int)")
	if got := countErrors(errs, "missing return value"); got != 1 {
		t.Errorf("expected exactly 1 error (the inner value block's bare return; the outer void block's is legal), got %d:\n%v", got, errs)
	}
}

// 14. Restoring (not just clearing) the recorded bare returns across a lambda body:
// a bare return AFTER a lambda in the same value block must still be reported. A
// reset-without-restore would drop it.
func TestT1392BareReturnAfterLambdaInValueGoBlockRejected(t *testing.T) {
	errs := checkErrs(t, `
		log_it(int n) void { }
		main() {
			n := 1;
			t := go {
				f := |int x| -> void { if x > 0 { return; } log_it(x); };
				f(1);
				if n > 0 { return; }
				n + 100
			};
			v := <-t;
		}
	`)
	expectError(t, errs, "missing return value (expected int)")
	if got := countErrors(errs, "missing return value"); got != 1 {
		t.Errorf("expected exactly 1 error (the block's own bare return; the lambda's is legal), got %d:\n%v", got, errs)
	}
}

// 15. A `go {}` inside a LAMBDA inside a `go {}` — the lambda resets the context and
// the inner go block re-establishes it, so the innermost bare return binds to the
// innermost block, not to the lambda (which would accept it as a void return).
func TestT1392BareReturnInGoBlockInsideLambdaInsideGoBlockRejected(t *testing.T) {
	errs := checkErrs(t, `
		log_it(int n) void { }
		main() {
			n := 1;
			outer := go {
				f := |int x| -> int {
					t := go { if x > 0 { return; } x + 1 };
					return <-t;
				};
				log_it(f(1));
				if n > 0 { return; }
			};
			<-outer;
		}
	`)
	expectError(t, errs, "missing return value (expected int)")
	if got := countErrors(errs, "missing return value"); got != 1 {
		t.Errorf("expected exactly 1 error (the innermost block's bare return), got %d:\n%v", got, errs)
	}
}

// 16. A bare return inside a LOOP body in a value block is recorded too — recording
// during checkBlock means no per-statement-kind walk is needed.
func TestT1392BareReturnInLoopInValueGoBlockRejected(t *testing.T) {
	errs := checkErrs(t, `
		main() {
			n := 1;
			t := go {
				for i in 0..3 { if i == 1 { return; } }
				n + 100
			};
			v := <-t;
		}
	`)
	expectError(t, errs, "missing return value (expected int)")
}

// 16b. The tracker item's original repro, verbatim in shape: a `use` binding plus a
// bare return in a failable VALUE body. This printed `-6148914691236517206` (0xAAAA…
// poison) at `<-t` when the bug was filed, and a meaningless `0` after codegen's
// interim defined-default store. It must now not compile at all.
func TestT1392OriginalReproUseBindingValueGoBlockRejected(t *testing.T) {
	errs := checkErrs(t, `
		type OkResource { int id; close!(~this) void { } }
		produce!(int x) int { if x < 0 { raise error(message: "neg"); } return x * 2; }
		main() {
			t := go! {
				base := produce(1)?^;
				use r := OkResource(id: 7);
				if base > 0 { return; }
				base + r.id
			};
		}
	`)
	expectError(t, errs, "missing return value (expected int)")
}

// 17. The failable analog of case 7: `return <expr>;` in a `go! {}` body produces the
// block's result (§17.2 explicit-return style, T1385) — only the bare form is rejected.
func TestT1392ValueReturnInFailableGoBlockNotRejectedHere(t *testing.T) {
	errs := checkErrs(t, `
		produce!(int x) int { if x < 0 { raise error(message: "neg"); } return x; }
		main() {
			t := go! {
				base := produce(1)?^;
				if base > 0 { return base; }
				return base + 1;
			};
		}
	`)
	expectNoErrorContaining(t, errs, "cannot return a value from a `go` block")
	expectNoErrorContaining(t, errs, "missing return value")
}

// 18. A failable `go! {}` cannot be fire-and-forget at all (§17.2.1): a discarded
// `go! { … }` statement is rejected for that reason on top of its body's bare
// return, so the two diagnostics coexist on the same spawn.
func TestT1392FailableGoBlockCannotBeFireAndForget(t *testing.T) {
	errs := checkErrs(t, `
		produce!(int x) int { if x < 0 { raise error(message: "neg"); } return x; }
		main() {
			n := 5;
			go! {
				base := produce(1)?^;
				if n > 0 { return; }
				base + n
			};
		}
	`)
	expectError(t, errs, "fire-and-forget goroutine must be non-failable")
}
