package codegen

import (
	"strings"
	"testing"
)

// T0879: Enum getter returning a heap value (string, vector, …) used as an
// unbound temporary (inline ==, call arg) leaked because genEnumGetterAccess
// did not call trackGetterResult. The fix adds the call after emitting the
// getter result, mirroring genGetterCall / genVirtualGetterCall.
//
// Tests:
//  1. String-returning getter used inline → promise_string_drop emitted
//  2. Int-returning getter used inline → no string_drop (not droppable)
//  3. String-returning getter used as call arg → promise_string_drop emitted
//  4. Non-leaking control: binding getter result to a named var before use
//     → promise_string_drop still present (via scope binding, not temp tracking,
//     but the test just confirms cleanup exists regardless of path)

// TestT0879_StringGetterInlineEqDrop — the primary repro: `t.text == "x"` where
// `text` is a string-returning enum getter on a data-payload enum. Pre-fix:
// the getter result was never registered for cleanup — promise_string_drop absent
// from the caller IR. Post-fix: trackGetterResult adds it.
func TestT0879_StringGetterInlineEqDrop(t *testing.T) {
	ir := generateIR(t, `
		enum Tagged {
			named(string s),
			blank,
			get text string {
				match this {
					named(s) => { return s; },
					blank    => { return "(blank)"; },
				}
			}
		}
		caller() bool {
			t := Tagged.named("hello");
			return t.text == "hello";
		}
		main() { caller(); }
	`)
	body := extractFunction(ir, "__user.caller")
	if body == "" {
		t.Fatalf("expected @__user.caller in IR")
	}
	if !strings.Contains(body, "promise_string_drop") {
		t.Errorf("T0879: string getter result used in inline == must be freed "+
			"(promise_string_drop); got no drop in @__user.caller:\n%s", body)
	}
}

// TestT0879_StringGetterCallArgDrop — getter result passed as a call argument:
// `sink(t.text, "x")`. Same root cause as the inline-eq case.
func TestT0879_StringGetterCallArgDrop(t *testing.T) {
	ir := generateIR(t, `
		enum Tagged {
			named(string s),
			blank,
			get text string {
				match this {
					named(s) => { return s; },
					blank    => { return "(blank)"; },
				}
			}
		}
		sink(string a, string b) bool { return a == b; }
		caller() bool {
			t := Tagged.named("world");
			return sink(t.text, "world");
		}
		main() { caller(); }
	`)
	body := extractFunction(ir, "__user.caller")
	if body == "" {
		t.Fatalf("expected @__user.caller in IR")
	}
	if !strings.Contains(body, "promise_string_drop") {
		t.Errorf("T0879: string getter result used as call arg must be freed "+
			"(promise_string_drop); got no drop in @__user.caller:\n%s", body)
	}
}

// TestT0879_IntGetterNoStringDrop — control: a getter returning int is not
// droppable; trackGetterResult must not emit promise_string_drop for it.
// (The pre-fix code path also never tracked it, so this test locks correctness
// rather than catching a regression, but it confirms the fix is type-gated.)
func TestT0879_IntGetterNoStringDrop(t *testing.T) {
	ir := generateIR(t, `
		enum Expr {
			Literal(int v),
			Neg(int v),
			get result int {
				match this {
					Literal(n) => { return n; },
					Neg(n)     => { return 0 - n; },
				}
			}
		}
		caller() int {
			e := Expr.Literal(v: 42);
			return e.result + 1;
		}
		main() { caller(); }
	`)
	body := extractFunction(ir, "__user.caller")
	if body == "" {
		t.Fatalf("expected @__user.caller in IR")
	}
	if strings.Contains(body, "promise_string_drop") {
		t.Errorf("T0879 control: int getter result must NOT trigger "+
			"promise_string_drop; found it in @__user.caller:\n%s", body)
	}
}

// TestT0879_StringGetterOnFieldlessEnum — enum with no payload variants still
// has a getter that returns a string literal from each branch. The fix must not
// break fieldless enums (their enum value is just an i32 tag, no heap payload).
// The returned string literals are .rodata, so promise_string_drop is still
// correct (it checks the static flag and skips the free).
func TestT0879_StringGetterOnFieldlessEnum(t *testing.T) {
	ir := generateIR(t, `
		enum Color {
			Red, Green, Blue,
			get label string {
				match this {
					Color.Red   => { return "red"; },
					Color.Green => { return "green"; },
					Color.Blue  => { return "blue"; },
				}
			}
		}
		caller() bool {
			c := Color.Red;
			return c.label == "red";
		}
		main() { caller(); }
	`)
	body := extractFunction(ir, "__user.caller")
	if body == "" {
		t.Fatalf("expected @__user.caller in IR")
	}
	// Even for static string literals the drop call must be emitted (the runtime
	// checks the static flag and skips the actual free — emitting the call is
	// always safe and required for consistency).
	if !strings.Contains(body, "promise_string_drop") {
		t.Errorf("T0879: string getter on fieldless enum used in inline == must "+
			"emit promise_string_drop; absent in @__user.caller:\n%s", body)
	}
}
