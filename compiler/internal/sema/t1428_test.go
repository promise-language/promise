package sema

import "testing"

// T1428: `yield` / `yield*` inside a `go { … }` block that is lexically nested in
// a generator body used to pass sema and then crash the backend with
// `use of undefined value '%yield_slot.addr'` — the go block's body is compiled
// into its OWN coroutine, which has no yield slot. §12.4 already requires `yield`
// to appear directly in the generator body, so the fix is a sema rejection.
//
// These tests are load-bearing beyond the diagnostic they assert: because the
// shape is now unreachable, `checkGoBlockCaptures` (internal/sema/sendable.go)
// dropped its `*ast.YieldStmt` / `*ast.YieldDelegateStmt` arms, and codegen's
// surviving arms are excused by `goWalkArmParityAllowlist`
// (internal/codegen/gowalk_parity_test.go). If the restriction is ever lifted,
// these tests are what fails first — put the sema walk arms back and drop those
// two allowlist entries in the same change, or a capture reached only through a
// yield value crosses the goroutine boundary ungated and unrecorded (T1658).

func TestT1428YieldInGoBlockRejected(t *testing.T) {
	errs := checkErrs(t, `
		gen() stream[int] {
			t := go { yield 1; 5 };
			yield <-t;
		}
		main() {
			for v in gen() { print_line(v.to_string()); }
		}
	`)
	expectError(t, errs, "yield inside a `go` block is not allowed")
}

func TestT1428YieldDelegateInGoBlockRejected(t *testing.T) {
	errs := checkErrs(t, `
		gen() stream[int] {
			t := go { yield * 1..=3; 5 };
			yield <-t;
		}
		main() {
			for v in gen() { print_line(v.to_string()); }
		}
	`)
	expectError(t, errs, "yield* inside a `go` block is not allowed")
}

func TestT1428YieldInFailableGoBlockRejected(t *testing.T) {
	errs := checkErrs(t, `
		produce!(int n) int { return n * 2; }
		gen() stream[int] {
			t := go! { yield 1; produce(3)?^ };
			yield 7;
		}
		main() {
			for v in gen() { print_line(v.to_string()); }
		}
	`)
	expectError(t, errs, "yield inside a `go` block is not allowed")
}

func TestT1428YieldInGoBlockInGeneratorMethodRejected(t *testing.T) {
	errs := checkErrs(t, `
		type Source {
			int n;
			rows(this) stream[int] {
				t := go { yield * 1..=3; 5 };
				yield this.n;
			}
		}
		main() {
			s := Source(n: 2);
			for v in s.rows() { print_line(v.to_string()); }
		}
	`)
	expectError(t, errs, "yield* inside a `go` block is not allowed")
}

// The innermost enclosing construct is the one named: a `go` block inside a
// lambda reports the go block, because checkGoExpr installs c.goBlock last.
func TestT1428YieldInGoBlockInsideLambdaReportsGoBlock(t *testing.T) {
	errs := checkErrs(t, `
		gen() stream[int] {
			f := || { go { yield 1; 5 }; };
			f();
			yield 2;
		}
		main() {
			for v in gen() { print_line(v.to_string()); }
		}
	`)
	expectError(t, errs, "yield inside a `go` block is not allowed")
}

// The mirror image: a lambda inside a `go` block still reports the lambda,
// because checkLambdaExpr clears c.goBlock on entry.
func TestT1428YieldInLambdaInsideGoBlockReportsLambda(t *testing.T) {
	errs := checkErrs(t, `
		gen() stream[int] {
			t := go { f := || { yield 1; }; f(); 5 };
			yield <-t;
		}
		main() {
			for v in gen() { print_line(v.to_string()); }
		}
	`)
	expectError(t, errs, "yield inside lambda/closure is not allowed")
}

// Outside a generator the outer guard keeps priority — the diagnostic is
// unchanged by this fix.
func TestT1428YieldInGoBlockOutsideGeneratorUnchanged(t *testing.T) {
	errs := checkErrs(t, `
		main() {
			t := go { yield 1; 5 };
			v := <-t;
		}
	`)
	expectError(t, errs, "yield outside of generator function")
}

// The guard must not over-reject: a generator containing a `go` block with no
// `yield` in it, and its own `yield` in the body, is legitimate.
func TestT1428GeneratorWithGoBlockWithoutYieldOk(t *testing.T) {
	checkOK(t, `
		gen() stream[int] {
			t := go { return 42; };
			yield <-t;
			yield 2;
		}
		main() {
			for v in gen() { print_line(v.to_string()); }
		}
	`)
}

// The `yield*` mirror of TestT1428YieldInLambdaInsideGoBlockReportsLambda. The
// two guards are ordered the same way in both arms, but only the `yield` arm's
// ordering was pinned; a `yield*` arm that checked `c.lambdaDepth` first would
// misattribute this to the go block, and nothing would have noticed.
func TestT1428YieldDelegateInLambdaInsideGoBlockReportsLambda(t *testing.T) {
	errs := checkErrs(t, `
		inner() stream[int] {
			yield 1;
		}
		gen() stream[int] {
			t := go { f := || { yield * inner(); }; f(); 5 };
			yield <-t;
		}
		main() {
			for v in gen() { print_line(v.to_string()); }
		}
	`)
	expectError(t, errs, "yield* inside lambda/closure is not allowed")
}

// The plain "yield* inside a lambda" diagnostic — the arm the new `c.goBlock`
// check now sits directly above. It had no test of its own, so a mis-edit that
// swallowed it (falling through to the yield-typing branch and type-checking a
// `yield*` that can never run) would have been silent.
func TestT1428YieldDelegateInLambdaRejected(t *testing.T) {
	errs := checkErrs(t, `
		inner() stream[int] {
			yield 1;
		}
		gen() stream[int] {
			f := || { yield * inner(); };
			f();
			yield 2;
		}
		main() {
			for v in gen() { print_line(v.to_string()); }
		}
	`)
	expectError(t, errs, "yield* inside lambda/closure is not allowed")
}

// The `yield*` half of TestT1428YieldInGoBlockOutsideGeneratorUnchanged: outside
// a generator the `!c.inGenerator` guard still wins, so the go block does not
// change which of the two diagnostics the user sees.
func TestT1428YieldDelegateInGoBlockOutsideGeneratorUnchanged(t *testing.T) {
	errs := checkErrs(t, `
		main() {
			t := go { yield * 1..=3; 5 };
			v := <-t;
		}
	`)
	expectError(t, errs, "yield* outside of generator function")
}

// Both diagnostics carry the hint naming the replacement idiom (the channel
// handoff in §12.4, exercised at runtime by
// tests/concurrency/t1428_generator_go_block_test.pr). The hint is the whole
// value of the rejection to the user — "not allowed" alone leaves them guessing
// — so it is asserted rather than left to drift.
func TestT1428GoBlockYieldDiagnosticsCarryHint(t *testing.T) {
	for _, tc := range []struct{ name, src string }{
		{"yield", `
			gen() stream[int] {
				t := go { yield 1; 5 };
				yield <-t;
			}
			main() {
				for v in gen() { print_line(v.to_string()); }
			}
		`},
		{"yield_delegate", `
			gen() stream[int] {
				t := go { yield * 1..=3; 5 };
				yield <-t;
			}
			main() {
				for v in gen() { print_line(v.to_string()); }
			}
		`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			errs := checkErrs(t, tc.src)
			expectError(t, errs, "hint: a goroutine has no consumer driving `next()` — send the values over a channel and `yield` them from the generator body")
		})
	}
}

// A go block nested in another go block, both inside a generator. `c.goBlock` is
// saved and restored per block rather than counted, so the inner block must see
// a non-nil context of its own — a naive "restore to nil" would leave the inner
// yield accepted and back at the original crash.
func TestT1428YieldInNestedGoBlockRejected(t *testing.T) {
	errs := checkErrs(t, `
		gen() stream[int] {
			t := go { u := go { yield 1; 2 }; <-u };
			yield <-t;
		}
		main() {
			for v in gen() { print_line(v.to_string()); }
		}
	`)
	expectError(t, errs, "yield inside a `go` block is not allowed")
}

// A rejected `yield` does not set `c.yieldFound`, so a generator whose ONLY
// yield sits in a go block is additionally reported as having none. That second
// error is the correct reading — the go block's yield was never the generator's
// — and pinning it keeps the pair from being "fixed" into a single misleading
// message.
func TestT1428GoBlockYieldDoesNotCountAsGeneratorYield(t *testing.T) {
	errs := checkErrs(t, `
		gen() stream[int] {
			t := go { yield 1; 5 };
			v := <-t;
		}
		main() {
			for v in gen() { print_line(v.to_string()); }
		}
	`)
	expectError(t, errs, "yield inside a `go` block is not allowed")
	expectError(t, errs, "function gen returns stream[int] but contains no yield statements")
}

// The guard is scoped to the block, not sticky for the rest of the body: a real
// `yield` after the go block still takes the typing branch, as its element-type
// mismatch here shows. A leaked `c.goBlock` would replace this error with a
// second "inside a `go` block" one.
func TestT1428YieldAfterGoBlockStillTypeChecked(t *testing.T) {
	errs := checkErrs(t, `
		gen() stream[int] {
			t := go { yield 1; 5 };
			yield "oops";
		}
		main() {
			for v in gen() { print_line(v.to_string()); }
		}
	`)
	expectError(t, errs, "yield inside a `go` block is not allowed")
	expectError(t, errs, "cannot yield string in generator returning stream[int]")
}
