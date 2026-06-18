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
// T0936 refined the per-path ownership to move-at-ownership: the result owns the
// some-path inner (moved out) AND owns the none-path default WHEN that default is a
// transferable local/fresh temp (its scope-exit owner is neutralized in the none
// block). When the default is a parameter/borrowed/static operand the result still
// borrows it (none-incoming false) — see the generic/structural tests below.
//
// These tests lock the IR signature: the elvis result temp must (1) carry the
// correct per-path flag phi, (2) clear a local default's drop flag on the none-path
// when ownership transfers, and (3) NOT route a vector result through
// @promise_string_drop (nor a string result through @Vector.drop).

// elvisFunc returns the IR body of the user `demo` function for the given body.
func elvisFunc(t *testing.T, body string) string {
	t.Helper()
	ir := generateIR(t, "sink(int n) { }\ndemo() {\n"+body+"\n}\n")
	return extractFunction(ir, "__user.demo")
}

// elvisOwnsBoth is the regex for the T0936 per-path drop flag phi when BOTH
// operands are transferable locals: the result owns the moved inner on the
// some-path AND the neutralized local default on the none-path (true, true).
const elvisOwnsBoth = `phi i1 \[ true, %elvis\.some\.\d+ \], \[ true, %elvis\.none\.\d+ \]`

// TestT0935VectorInlineElvisPathFlag verifies the inline vector elvis result with
// LOCAL operands owns the buffer on both paths (T0936): true on the some-path
// (moved inner) and true on the none-path (local default `b`, whose own drop flag
// is cleared in the none block so exactly one owner frees it).
func TestT0935VectorInlineElvisPathFlag(t *testing.T) {
	fn := elvisFunc(t, `
		int[]? a = [1, 2, 3];
		int[] b = [9];
		sink((a ?: b).len);
	`)
	if fn == "" {
		t.Fatal("could not extract __user.demo")
	}
	// Per-path drop flag: result owns on both paths (local operands).
	assertContainsMatch(t, fn, elvisOwnsBoth)
	// The none-path transfers the local default into the result, so it clears the
	// default's own scope-exit drop flag (path-conditional, none block only).
	assertContainsMatch(t, fn, `elvis\.none\.\d+:[\s\S]*store i1 false, i1\* %b\.dropflag`)
	// A vector result must never be dropped via the string drop helper.
	if strings.Contains(fn, "@promise_string_drop") {
		t.Errorf("vector inline elvis must NOT call @promise_string_drop (T0935)\n%s", fn)
	}
	// It IS dropped via Vector.drop (which honors the bit-63 static flag).
	assertContains(t, fn, "call void @Vector.drop")
}

// TestT0935StringInlineElvisPathFlag verifies the string path: LOCAL operands own
// on both paths (T0936), the none-path clears the local default's drop flag, and
// the result is dropped via @promise_string_drop (never @Vector.drop).
func TestT0935StringInlineElvisPathFlag(t *testing.T) {
	fn := elvisFunc(t, `
		string? a = "hi" + "!";
		string b = "x" + "y";
		sink((a ?: b).len);
	`)
	if fn == "" {
		t.Fatal("could not extract __user.demo")
	}
	assertContainsMatch(t, fn, elvisOwnsBoth)
	assertContainsMatch(t, fn, `elvis\.none\.\d+:[\s\S]*store i1 false, i1\* %b\.dropflag`)
	assertContains(t, fn, "@promise_string_drop")
	if strings.Contains(fn, "call void @Vector.drop") {
		t.Errorf("string inline elvis must NOT call @Vector.drop (T0935)\n%s", fn)
	}
}

// elvisPathFlag is the regex for the per-path drop flag phi when the none-path
// default is a parameter/borrowed operand: true on the some-path (result owns the
// moved inner), false on the none-path (the borrowed default keeps its own owner —
// the caller frees it). Used by the generic/structural tests, whose elvis operands
// are method/function parameters (no local drop flag to transfer).
const elvisPathFlag = `phi i1 \[ true, %elvis\.some\.\d+ \], \[ false, %elvis\.none\.\d+ \]`

// TestT0935GenericInlineElvisTypeSubst exercises the typeSubst substitution
// branch of trackElvisResultTemp: when the inline elvis lives inside a generic
// function, the result type (`T[]`) must be resolved through c.typeSubst to the
// concrete vector type before selecting the drop function. The monomorphized
// `inline_len[int]` must carry the path flag and drop via @Vector.drop — proving
// the element type was substituted (otherwise the vector result would be
// untracked or mis-dropped).
//
// The optional operand is an *owned local* (`opt`), not a parameter: T0945/T0937
// established that a borrowed value param's inner is owned by the caller and must
// NOT be freed by the callee's inline result temp (someOwnsInner=false), so the
// typeSubst-drop branch is only reached for orphaned operands. The owned local
// keeps that coverage intact. (The borrowed-param not-tracked case is locked by
// TestT0945BorrowedParamInlineElvisNoDrop below.)
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
// The optional operand is an *owned local* (`maybe`), not a parameter: per
// T0945/T0937 a borrowed value param's inner is owned by the caller, so the
// result-temp drop branch (where selfSubst runs) is only reached for orphaned
// operands.
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

// TestT0937StructuralDefaultBorrowedParamInlineElvisNotTracked covers the orphan
// gate (T0937) for a structural-interface default method whose optional operand
// is a BORROWED param. The default body is synthesized per concrete type with
// c.selfSubst active. Before T0937 the synthesized @Box.pick_len tracked the
// inline result (path-flag phi + @Vector.drop); since `maybe` is a borrowed param
// the caller owns it, so dropping it in the callee would double-free. The orphan
// gate (someOwnsInner=false for a borrowed source) now leaves the result
// untracked: no path-flag phi. This is the borrowed-source companion to
// TestT0935StructuralDefaultInlineElvisSelfSubst (owned-local, tracked) — it
// locks the selfSubst path's orphan gate in addition to the generic-function
// borrowed case in TestT0945BorrowedParamInlineElvisNoDrop.
func TestT0937StructuralDefaultBorrowedParamInlineElvisNotTracked(t *testing.T) {
	ir := generateIR(t, `
		type Fallbackable `+"`structural"+` {
			tag() int `+"`abstract"+`;
			pick_len(int[]? maybe, int[] fallback) int {
				return (maybe ?: fallback).len;
			}
		}
		type Box is Fallbackable {
			tag() int { return 1; }
		}
		demo() {
			b := Box();
			int[]? a = [1, 2, 3];
			int[] fb = [9];
			s := b.pick_len(a, fb);
		}
	`)
	// The concrete-type synthesized default (not the Fallbackable template) is the
	// body compiled with selfSubst active.
	fn := extractDefine(ir, "Box.pick_len")
	if fn == "" {
		t.Fatal("could not extract synthesized Box.pick_len")
	}
	assertNotContainsMatch(t, fn, elvisPathFlag)
	if strings.Contains(fn, "call void @Vector.drop") {
		t.Errorf("borrowed-param structural-default inline elvis must NOT drop the result in the callee — the caller owns it (T0937)\n%s", fn)
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

// TestT0936NonIdentStringDefaultClaimed exercises claimElvisDefaultTemp on a
// NON-ident none-path default that is a fresh heap-string temp (a `+`-concat,
// which always allocates — bit-63 clear). genElvis takes the `else` branch
// (default is not an ident) and claimElvisDefaultTemp claims the temp, so the
// result owns the buffer on the none-path too: the flag phi is [true, true]. The
// claimed concat must not get its own separate drop — only the result frees it.
func TestT0936NonIdentStringDefaultClaimed(t *testing.T) {
	fn := elvisFunc(t, `
		string? a = none;
		x := (a ?: ("p" + "q")).len;
	`)
	if fn == "" {
		t.Fatal("could not extract __user.demo")
	}
	// Fresh heap-string default claimed → result owns on both paths.
	assertContainsMatch(t, fn, elvisOwnsBoth)
	assertContains(t, fn, "@promise_string_drop")
	if strings.Contains(fn, "call void @Vector.drop") {
		t.Errorf("string inline elvis must NOT call @Vector.drop (T0936)\n%s", fn)
	}
}

// TestT0936NonIdentLiteralDefaultBorrows exercises claimElvisDefaultTemp returning
// false: a NON-ident none-path default that is a `.rodata` vector literal has no
// owned temp to claim (static buffer), so the result BORROWS it on the none-path —
// the flag phi is [true, false]. The result still routes through @Vector.drop
// (which no-ops on the bit-63 static flag), never @promise_string_drop.
func TestT0936NonIdentLiteralDefaultBorrows(t *testing.T) {
	fn := elvisFunc(t, `
		int[]? a = none;
		x := (a ?: [1, 2, 3, 4]).len;
	`)
	if fn == "" {
		t.Fatal("could not extract __user.demo")
	}
	// Static literal default not claimed → result borrows on the none-path.
	assertContainsMatch(t, fn, elvisPathFlag)
	assertContains(t, fn, "call void @Vector.drop")
	if strings.Contains(fn, "@promise_string_drop") {
		t.Errorf("vector inline elvis must NOT call @promise_string_drop (T0936)\n%s", fn)
	}
}

// TestT0936NonIdentCallDefaultClaimed exercises the claimHeapTemp branch of
// claimElvisDefaultTemp (sets c.lastClaimedDropFunc) for a NON-ident none-path
// default that is a heap-vector call result. Distinct from the string case: the
// call introduces a panic-check block, so the result temp's flag-phi none-incoming
// comes from a `%panic.ok` predecessor (not `%elvis.none`) — exercising the
// non-elvis-labeled predecessor variant — and the flag is still true (claimed).
func TestT0936NonIdentCallDefaultClaimed(t *testing.T) {
	ir := generateIR(t, `
		heap_vec() int[] {
			int[] v = [];
			v.push(7);
			v.push(8);
			return v;
		}
		demo() {
			int[]? a = none;
			x := (a ?: heap_vec()).len;
		}
	`)
	fn := extractFunction(ir, "__user.demo")
	if fn == "" {
		t.Fatal("could not extract __user.demo")
	}
	// Claimed heap-call default → owns on none-path; none-incoming is the call's
	// panic.ok block, not an elvis.none block.
	assertContainsMatch(t, fn,
		`phi i1 \[ true, %elvis\.some\.\d+ \], \[ true, %panic\.ok\.\d+ \]`)
	assertContains(t, fn, "call void @Vector.drop")
	if strings.Contains(fn, "@promise_string_drop") {
		t.Errorf("vector inline elvis must NOT call @promise_string_drop (T0936)\n%s", fn)
	}
}
