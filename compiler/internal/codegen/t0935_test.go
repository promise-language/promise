package codegen

import (
	"strings"
	"testing"
)

// T0935: an inline (used/discarded, not bound) elvis `?:` whose result is a
// container (Vector[T]/T[]) must be dropped via the type-correct drop function
// with a path-dependent drop flag. Previously the inline result was tracked as a
// *string* temp (promise_string_drop), which read the static flag at the wrong
// offset for the vector layout and called free() on a .rodata buffer:
//   fatal: invalid free (bad header magic)
//
// These tests lock the IR signature: the elvis result temp must (1) carry a
// path-dependent flag phi (true on the some-path — owns the moved inner; false
// on the none-path — borrows the default), and (2) NOT route a vector result
// through @promise_string_drop.

// elvisFunc returns the IR body of the user `demo` function for the given body.
func elvisFunc(t *testing.T, body string) string {
	t.Helper()
	ir := generateIR(t, "sink(int n) { }\ndemo() {\n"+body+"\n}\n")
	return extractFunction(ir, "__user.demo")
}

// TestT0935VectorInlineElvisPathFlag verifies the inline vector elvis result is
// tracked with a path-dependent drop flag phi (true on some, false on none).
func TestT0935VectorInlineElvisPathFlag(t *testing.T) {
	fn := elvisFunc(t, `
		int[]? a = [1, 2, 3];
		int[] b = [9];
		sink((a ?: b).len);
	`)
	if fn == "" {
		t.Fatal("could not extract __user.demo")
	}
	// Path-dependent drop flag: free on some-path (sole owner of moved inner),
	// no-op on none-path (default is borrowed).
	assertContainsMatch(t, fn,
		`phi i1 \[ true, %elvis\.some\.\d+ \], \[ false, %elvis\.none\.\d+ \]`)
	// A vector result must never be dropped via the string drop helper.
	if strings.Contains(fn, "@promise_string_drop") {
		t.Errorf("vector inline elvis must NOT call @promise_string_drop (T0935)\n%s", fn)
	}
	// It IS dropped via Vector.drop (which honors the bit-63 static flag).
	assertContains(t, fn, "call void @Vector.drop")
}

// TestT0935StringInlineElvisPathFlag verifies the string path still works: a
// path-dependent flag phi is emitted and the result is dropped via
// @promise_string_drop (never @Vector.drop, since no vector exists here).
func TestT0935StringInlineElvisPathFlag(t *testing.T) {
	fn := elvisFunc(t, `
		string? a = "hi" + "!";
		string b = "x" + "y";
		sink((a ?: b).len);
	`)
	if fn == "" {
		t.Fatal("could not extract __user.demo")
	}
	assertContainsMatch(t, fn,
		`phi i1 \[ true, %elvis\.some\.\d+ \], \[ false, %elvis\.none\.\d+ \]`)
	assertContains(t, fn, "@promise_string_drop")
	if strings.Contains(fn, "call void @Vector.drop") {
		t.Errorf("string inline elvis must NOT call @Vector.drop (T0935)\n%s", fn)
	}
}

// elvisPathFlag is the regex for the T0935 path-dependent drop flag phi: true on
// the some-path (result owns the moved inner), false on the none-path (the
// default is borrowed and keeps its own owner).
const elvisPathFlag = `phi i1 \[ true, %elvis\.some\.\d+ \], \[ false, %elvis\.none\.\d+ \]`

// TestT0935GenericInlineElvisTypeSubst exercises the typeSubst substitution
// branch of trackElvisResultTemp: when the inline elvis lives inside a generic
// function, the result type (`T[]`) must be resolved through c.typeSubst to the
// concrete vector type before selecting the drop function. The monomorphized
// `inline_len[int]` must carry the path flag and drop via @Vector.drop — proving
// the element type was substituted (otherwise the vector result would be
// untracked or mis-dropped).
//
// The optional operand is an *owned local* (`opt`), not a parameter: T0945
// established that a borrowed value param's inner is owned by the caller and must
// NOT be freed by the callee's inline result temp, so the typeSubst-drop branch
// is only reached for owned operands. The owned local keeps that coverage intact.
func TestT0935GenericInlineElvisTypeSubst(t *testing.T) {
	ir := generateIR(t, `
		sink(int n) { }
		inline_len[T](~T seed, T[] b) int {
			T[]? opt = [seed];
			return (opt ?: b).len;
		}
		demo() {
			int[] b = [9];
			sink(inline_len(1, b));
		}
	`)
	// The mono name is quoted in IR (@"inline_len[int]"); pass embedded quotes so
	// extractDefine's "@"+name+"(" needle matches and anchors on the define.
	fn := extractDefine(ir, `"inline_len[int]"`)
	if fn == "" {
		t.Fatal("could not extract monomorphized inline_len[int]")
	}
	assertContainsMatch(t, fn, elvisPathFlag)
	assertContains(t, fn, "call void @Vector.drop")
	if strings.Contains(fn, "@promise_string_drop") {
		t.Errorf("generic vector inline elvis must NOT call @promise_string_drop (T0935)\n%s", fn)
	}
}

// TestT0935StructuralDefaultInlineElvisSelfSubst exercises the selfSubst
// substitution branch of trackElvisResultTemp: an inline elvis inside a
// structural interface's default method is compiled once per concrete type with
// c.selfSubst active (so `Self` resolves to the concrete type). The synthesized
// @Box.pick_len body must carry the path flag and drop the vector via
// @Vector.drop — confirming the result type is resolved through SubstituteSelf
// rather than mis-dropped.
//
// The optional operand is an *owned local* (`maybe`), not a parameter: per T0945
// a borrowed value param's inner is owned by the caller, so the result-temp drop
// branch (where selfSubst runs) is only reached for owned operands.
func TestT0935StructuralDefaultInlineElvisSelfSubst(t *testing.T) {
	ir := generateIR(t, `
		type Fallbackable `+"`structural"+` {
			tag() int `+"`abstract"+`;
			pick_len(int[] fallback) int {
				int[]? maybe = [1, 2, 3];
				return (maybe ?: fallback).len;
			}
		}
		type Box is Fallbackable {
			tag() int { return 1; }
		}
		demo() {
			b := Box();
			int[] fb = [9];
			s := b.pick_len(fb);
		}
	`)
	// The concrete-type synthesized default (not the Fallbackable template) is the
	// body compiled with selfSubst active.
	fn := extractDefine(ir, "Box.pick_len")
	if fn == "" {
		t.Fatal("could not extract synthesized Box.pick_len")
	}
	assertContainsMatch(t, fn, elvisPathFlag)
	assertContains(t, fn, "call void @Vector.drop")
	if strings.Contains(fn, "@promise_string_drop") {
		t.Errorf("structural-default vector inline elvis must NOT call @promise_string_drop (T0935)\n%s", fn)
	}
}

// TestT0945BorrowedParamInlineElvisNoDrop locks the T0945 fix: when the inline
// elvis left operand is a *borrowed value parameter*, the some-path inner is
// owned by the caller (which drops the param after the call), so the callee must
// NOT free it via the inline result temp. The function body must therefore carry
// neither the path-flag phi nor a @Vector.drop for the elvis result. Without the
// fix this double-freed the heap buffer ("fatal: invalid free (bad header
// magic)"); the static-vector cases merely masked it because @Vector.drop no-ops
// on .rodata vectors.
func TestT0945BorrowedParamInlineElvisNoDrop(t *testing.T) {
	ir := generateIR(t, `
		sink(int n) { }
		borrowed_len[T](T[]? a, T[] b) int {
			return (a ?: b).len;
		}
		demo() {
			int[]? a = [1, 2, 3];
			int[] b = [9];
			sink(borrowed_len(a, b));
		}
	`)
	fn := extractDefine(ir, `"borrowed_len[int]"`)
	if fn == "" {
		t.Fatal("could not extract monomorphized borrowed_len[int]")
	}
	// The result borrows on both paths (some: aliases the caller-owned param;
	// none: aliases the borrowed default), so it is never tracked for drop.
	if strings.Contains(fn, "call void @Vector.drop") {
		t.Errorf("borrowed-param inline elvis must NOT drop its result (T0945)\n%s", fn)
	}
	assertNotContainsMatch(t, fn, elvisPathFlag)
}
