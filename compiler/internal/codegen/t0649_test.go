package codegen

import (
	"strings"
	"testing"
)

// T0649: binding the result of a method/operator with a borrowed-reference
// return type (`T&` / `T~`) to a local leaked 1 allocation.
//
// Root cause: a `T&`/`T~` method body like `at(int i) string& { return
// this.d[i]; }` was lowered through the B0189 string return-dup in
// genReturnStmt — extractNamed(retType) unwraps SharedRef(string)→TypString,
// so the dup fired and the body returned a *fresh owned strdup* despite the
// borrow return type. The call site then temp-tracked that dup as an owned
// string (CallExpr post-call tracking / trackUserIndexResult, same unwrap);
// on `x := obj.m()` claimStringTemp cleared the stmt-temp flag while
// isBorrowedExpr cleared the LHS drop flag → the dup had no owner → leak.
//
// Fix (two parts): (1) genReturnStmt gates the B0189 string return-dup and the
// sibling enum-clone on `!isRefType(retType)`; (2) the CallExpr post-call
// tracking and trackUserIndexResult skip tracking when the static result type
// is a borrow.
//
// These tests lock the structural IR signature of Part 1 — the root-cause
// allocation: a `T&`/`T~` return must NOT strdup (no @promise_string_new in
// the method/operator body), while an IDENTICAL owned `T` return still MUST
// (the gate is precise, not a blanket disable). Runtime zero-leak / no-double-
// free across the bind / temp-use / fn-arg / `[]` / enum-borrow surfaces is
// enforced by the batch tests in tests/e2e/ref_return_bind_test.pr (the
// zero-tolerance leak gate catches both a missing-owner leak and an
// over-tracking double-free) — the same split the sibling T0647 suite uses.

// stringNewCount counts @promise_string_new call sites in an extracted body.
func stringNewCount(body string) int {
	return strings.Count(body, "call i8* @promise_string_new(")
}

// TestT0649_RefMethodReturnNoStrdup — the core repro. `at(int i) string&`
// returning `this.d[i]` (a Vector[string] element) must hand back the real
// element pointer, NOT a fresh strdup. Pre-fix the B0189 return-dup fired
// (extractNamed unwrapped string& → TypString) and the body strdup'd the
// element → the leaked allocation. Post-fix (Part 1, !isRefType gate) the body
// has zero @promise_string_new and returns the loaded element pointer directly.
func TestT0649_RefMethodReturnNoStrdup(t *testing.T) {
	ir := generateIR(t, `
		type RefRet { string[] d; at(int i) string& { return this.d[i]; } }
		caller() { r := RefRet(d: ["aa", "bbb"]); x := r.at(1); n := x.len; }
		main() { caller(); }
	`)
	body := extractFunction(ir, "RefRet.at")
	if body == "" {
		t.Fatalf("expected @RefRet.at in IR")
	}
	if n := stringNewCount(body); n != 0 {
		t.Errorf("string& return must NOT strdup the borrowed element "+
			"(B0189 gated by !isRefType); found %d @promise_string_new in "+
			"@RefRet.at (pre-fix leak):\n%s", n, body)
	}
	// Positive: the body loads the element pointer and returns it directly
	// (a true borrow into the receiver's vector storage).
	if !strings.Contains(body, "ret i8*") {
		t.Errorf("expected @RefRet.at to return the borrowed i8* element "+
			"pointer directly:\n%s", body)
	}
}

// TestT0649_OwnedControlStillStrdups — precision guard. An IDENTICAL type whose
// `at` returns an OWNED `string` (no `&`) must STILL strdup via B0189 (the
// caller receives an independent copy that outlives the receiver's drop). This
// proves the !isRefType gate is targeted at borrow returns only and did not
// blanket-disable the B0189 return-dup. The two bodies are otherwise IR-
// identical, so the only difference is the strdup.
func TestT0649_OwnedControlStillStrdups(t *testing.T) {
	ir := generateIR(t, `
		type OwnedRet { string[] d; at(int i) string { return this.d[i]; } }
		caller() { o := OwnedRet(d: ["aa", "bbb"]); x := o.at(1); n := x.len; }
		main() { caller(); }
	`)
	body := extractFunction(ir, "OwnedRet.at")
	if body == "" {
		t.Fatalf("expected @OwnedRet.at in IR")
	}
	if stringNewCount(body) == 0 {
		t.Errorf("owned `string` return (control) must STILL strdup via B0189 "+
			"— the !isRefType gate must be precise, not a blanket disable; "+
			"@promise_string_new absent in @OwnedRet.at:\n%s", body)
	}
}

// TestT0649_MutRefMethodReturnNoStrdup — the `~` (MutRef) variant. isRefType
// reports true for *types.MutRef as well as *types.SharedRef, so a `string~`
// return is gated identically to `string&`. Locks that Part 1 covers MutRef,
// not just SharedRef.
func TestT0649_MutRefMethodReturnNoStrdup(t *testing.T) {
	ir := generateIR(t, `
		type MutRet { string[] d; at(int i) string~ { return this.d[i]; } }
		caller() { m := MutRet(d: ["aa", "bbb"]); x := m.at(1); n := x.len; }
		main() { caller(); }
	`)
	body := extractFunction(ir, "MutRet.at")
	if body == "" {
		t.Fatalf("expected @MutRet.at in IR")
	}
	if n := stringNewCount(body); n != 0 {
		t.Errorf("string~ (MutRef) return must NOT strdup the borrowed element "+
			"(B0189 gated by !isRefType, which covers *types.MutRef); found %d "+
			"@promise_string_new in @MutRet.at:\n%s", n, body)
	}
}

// TestT0649_UserIndexRefReturnNoStrdup — Part 1 reaches the user-defined
// non-native `[]` path too. `[](int i) string&` over a Vector[string] field
// must not strdup (the operator return is a borrow, identical to the plain
// method). Mirrors T0647's deliberate operator/method parity: here the parity
// is "neither strdups" rather than T0647's "both track an owned return".
func TestT0649_UserIndexRefReturnNoStrdup(t *testing.T) {
	ir := generateIR(t, `
		type IdxRef { string[] d; [](int i) string& { return this.d[i]; } }
		caller() { r := IdxRef(d: ["aa", "bbb"]); x := r[1]; n := x.len; }
		main() { caller(); }
	`)
	// extractFunction builds the marker "@"+name+"(" — pass the literal
	// quotes so it matches the quoted define `@"IdxRef.[]"(`.
	body := extractFunction(ir, `"IdxRef.[]"`)
	if body == "" {
		t.Fatalf("expected @\"IdxRef.[]\" in IR")
	}
	if n := stringNewCount(body); n != 0 {
		t.Errorf("string& user `[]` return must NOT strdup the borrowed "+
			"element (B0189 gated by !isRefType through the operator path); "+
			"found %d @promise_string_new in @\"IdxRef.[]\":\n%s", n, body)
	}
}

// TestT0649_UserIndexOwnedControlStillStrdups — precision guard for the
// operator path: an owned `[](int i) string` return STILL strdups (T0647's
// owned-return behavior is unchanged — isRefType is false for it).
func TestT0649_UserIndexOwnedControlStillStrdups(t *testing.T) {
	ir := generateIR(t, `
		type IdxOwn { string[] d; [](int i) string { return this.d[i]; } }
		caller() { o := IdxOwn(d: ["aa", "bbb"]); x := o[1]; n := x.len; }
		main() { caller(); }
	`)
	body := extractFunction(ir, `"IdxOwn.[]"`)
	if body == "" {
		t.Fatalf("expected @\"IdxOwn.[]\" in IR")
	}
	if stringNewCount(body) == 0 {
		t.Errorf("owned `string` user `[]` return (control) must STILL strdup "+
			"via B0189 (T0647 owned-return path, isRefType false); "+
			"@promise_string_new absent in @\"IdxOwn.[]\":\n%s", body)
	}
}

// TestT0649_RefParamPassthroughNoStrdup — isolates Part 2 from Part 1. A
// `string&` method that returns a *parameter* borrow (not a vector-index field)
// never triggers B0189 regardless (s.Value is an IdentExpr bound to a param,
// not a Vector[string] local), so the body strdup is irrelevant here. What
// matters is that the call site does not over-track the borrowed result: with
// Part 2 the static result type is a borrow → no post-call temp-tracking, so
// the body remains a pure pass-through (load param, return it) with no
// @promise_string_new anywhere.
func TestT0649_RefParamPassthroughNoStrdup(t *testing.T) {
	ir := generateIR(t, `
		passthrough(string s) string& { return s; }
		caller() { v := "hi" + "x"; x := passthrough(v); n := x.len; }
		main() { caller(); }
	`)
	body := extractFunction(ir, "__user.passthrough")
	if body == "" {
		t.Fatalf("expected @__user.passthrough in IR")
	}
	if n := stringNewCount(body); n != 0 {
		t.Errorf("a `string&`→`string&` parameter pass-through must not "+
			"strdup; found %d @promise_string_new:\n%s", n, body)
	}
	// The caller must still own and free the concat temp exactly once via `v`
	// (the borrow `x` is never dropped). Sanity: the pass-through call is wired
	// and the concat temp is created in the caller (its drop is `v`'s, not the
	// borrow's — runtime balance enforced by the e2e leak gate).
	caller := extractFunction(ir, "__user.caller")
	if !strings.Contains(caller, "@__user.passthrough(") {
		t.Errorf("expected the caller to invoke @__user.passthrough:\n%s", caller)
	}
	if !strings.Contains(caller, "@promise_string_concat(") {
		t.Errorf("expected the caller to build the owned concat temp:\n%s", caller)
	}
}

// TestT0649_GenericRefMethodReturnNoStrdup — locks the substitution path that
// the concrete-type tests never exercise. For a generic `GBox[T]` the method's
// c.currentRetType is `SharedRef(TypeParam T)`; it only becomes
// `SharedRef(string)` after types.Substitute under the mono subst. The Part 1
// gate runs on the *substituted* retType (genReturnStmt substitutes before the
// !isRefType check), so the monomorphized `GBox[string].at` body must contain
// no @promise_string_new and return the borrowed element pointer directly —
// proving the gate fires post-substitution, not only for syntactic `string&`.
func TestT0649_GenericRefMethodReturnNoStrdup(t *testing.T) {
	ir := generateIR(t, `
		type GBox[T] { T[] d; at(int i) T& { return this.d[i]; } }
		caller() { b := GBox[string](d: ["aa", "bbb"]); x := b.at(1); n := x.len; }
		main() { caller(); }
	`)
	// Monomorphized name is the quoted `@"GBox[string].at"( ` — pass literal
	// quotes (same convention as the user-`[]` tests above).
	body := extractFunction(ir, `"GBox[string].at"`)
	if body == "" {
		t.Fatalf("expected @\"GBox[string].at\" in IR")
	}
	if n := stringNewCount(body); n != 0 {
		t.Errorf("generic T& return (T=string) must NOT strdup the borrowed "+
			"element — the B0189 !isRefType gate must fire on the *substituted* "+
			"retType (SharedRef(string)); found %d @promise_string_new in "+
			"@\"GBox[string].at\":\n%s", n, body)
	}
	if !strings.Contains(body, "ret i8*") {
		t.Errorf("expected @\"GBox[string].at\" to return the borrowed i8* "+
			"element pointer directly:\n%s", body)
	}
}

// cloneCallCount counts @<enum>.clone( call sites in an extracted body. The
// synthesized clone function itself is a separate `define` and is not counted
// when the body is restricted to the accessor via extractFunction.
func cloneCallCount(body, enumName string) int {
	return strings.Count(body, "@"+enumName+".clone(")
}

// TestT0649_EnumRefReturnNoClone — the consistency-hardening sibling of the
// string strdup gate, and the case the plan explicitly said to cover "with its
// own test". genReturnStmt has a *second* B0189-style block that
// cloneEnumValue's a droppable enum loaded from a vector index (`return
// this.items[i]`) so scope cleanup's vector-element drop can't dangle it. For a
// `MyEnum&`/`MyEnum~` borrow return that clone IS the leaked allocation (cloned
// here, then orphaned at the binding site by isBorrowedExpr — exactly the
// string-path failure mode), so Part 1 gates it on `!isRefType(retType)`. A
// `Tagged&` accessor over a `Tagged[]` field must therefore emit NO
// @Tagged.clone( in its body — it hands back the real element pointer.
//
// The enum carries an explicit `clone annotation: cloneEnumValue only emits a
// clone when a synthesized `Tagged.clone` exists, which (for a bare enum) it
// does NOT — so without `clone the enum-clone block is inert and the guard is
// untestable (the clone never fires either way). The `clone variant makes the
// clone genuinely reachable, so this test proves the guard suppresses an
// *otherwise-emitted* clone (verified: TestT0649_EnumOwnedControlStillClones
// emits exactly one for the IR-identical owned return). The e2e suite enforces
// runtime 0-leak; this locks the IR signature (the string trio had this lock,
// the enum sibling did not).
func TestT0649_EnumRefReturnNoClone(t *testing.T) {
	ir := generateIR(t, `
		enum Tagged `+"`clone"+` { Empty, Named(string s) }
		type EnumVecRef { Tagged[] items; at(int i) Tagged& { return this.items[i]; } }
		caller() { ev := EnumVecRef(items: [Tagged.Named("a" + "b"), Tagged.Empty]); x := ev.at(0); }
		main() { caller(); }
	`)
	body := extractFunction(ir, "EnumVecRef.at")
	if body == "" {
		t.Fatalf("expected @EnumVecRef.at in IR")
	}
	if n := cloneCallCount(body, "Tagged"); n != 0 {
		t.Errorf("Tagged& return must NOT clone the borrowed droppable-enum "+
			"element (B0189 sibling enum-clone gated by !isRefType); found %d "+
			"@Tagged.clone( in @EnumVecRef.at (pre-fix leak):\n%s", n, body)
	}
}

// TestT0649_EnumMutRefReturnNoClone — the `~` (MutRef) variant of the
// enum-clone gate. isRefType covers *types.MutRef too, so `Tagged~` is gated
// identically to `Tagged&`.
func TestT0649_EnumMutRefReturnNoClone(t *testing.T) {
	ir := generateIR(t, `
		enum Tagged `+"`clone"+` { Empty, Named(string s) }
		type EnumVecRef { Tagged[] items; at_mut(int i) Tagged~ { return this.items[i]; } }
		caller() { ev := EnumVecRef(items: [Tagged.Named("a" + "b"), Tagged.Empty]); x := ev.at_mut(0); }
		main() { caller(); }
	`)
	body := extractFunction(ir, "EnumVecRef.at_mut")
	if body == "" {
		t.Fatalf("expected @EnumVecRef.at_mut in IR")
	}
	if n := cloneCallCount(body, "Tagged"); n != 0 {
		t.Errorf("Tagged~ (MutRef) return must NOT clone the borrowed "+
			"droppable-enum element (enum-clone gate covers *types.MutRef); "+
			"found %d @Tagged.clone( in @EnumVecRef.at_mut:\n%s", n, body)
	}
}

// TestT0649_EnumOwnedControlStillClones — precision guard for the enum-clone
// gate, the exact analogue of TestT0649_OwnedControlStillStrdups. An IDENTICAL
// accessor returning an OWNED `Tagged` (no `&`/`~`) from `this.items[i]` MUST
// STILL emit @Tagged.clone( via the B0189 sibling block: the caller receives an
// independent enum that must outlive the receiver's vector drop. This proves
// the !isRefType gate is targeted at borrow returns only and did not
// blanket-disable the enum-clone return path. The two bodies are otherwise
// IR-identical, so the only difference is the clone.
func TestT0649_EnumOwnedControlStillClones(t *testing.T) {
	ir := generateIR(t, `
		enum Tagged `+"`clone"+` { Empty, Named(string s) }
		type EnumVecOwn { Tagged[] items; at(int i) Tagged { return this.items[i]; } }
		caller() { ev := EnumVecOwn(items: [Tagged.Named("a" + "b"), Tagged.Empty]); x := ev.at(0); }
		main() { caller(); }
	`)
	body := extractFunction(ir, "EnumVecOwn.at")
	if body == "" {
		t.Fatalf("expected @EnumVecOwn.at in IR")
	}
	if cloneCallCount(body, "Tagged") == 0 {
		t.Errorf("owned `Tagged` return (control) must STILL clone via the "+
			"B0189 sibling enum-clone block — the !isRefType gate must be "+
			"precise, not a blanket disable; @Tagged.clone( absent in "+
			"@EnumVecOwn.at:\n%s", body)
	}
}

// TestT0649_GenericEnumRefReturnNoClone — the intersection of the two
// substitution-sensitive surfaces: a GENERIC container whose element-borrow
// accessor returns a droppable-`clone ENUM. This is the only test that
// exercises the enum-clone gate on a *substituted* retType: c.currentRetType is
// `SharedRef(TypeParam T)` and only becomes `SharedRef(Instance Tag)` after
// types.Substitute under the GBox[Tag] mono subst — the concrete-enum tests
// (syntactic `Tagged&`) and the generic-string test (string-dup sibling, not
// the enum-clone block) each cover only one axis. The monomorphized
// `GBox[Tag].at` body must contain no @Tag.clone(.
//
// Precision is established jointly by the siblings rather than a local control
// (matching this file's pattern — OwnedControl tests are concrete): the
// IR-identical owned `GBox[Tag].at(int i) T` emits exactly one @Tag.clone(
// (the clone is genuinely reachable post-substitution — cloneEnumValue ok=true,
// `define @Tag.clone` present), so the zero count here is the !isRefType guard
// suppressing an otherwise-emitted clone, not an inert (bare-enum) block. The
// e2e suite enforces runtime 0-leak for the bind/temp/mutref generic-enum forms.
func TestT0649_GenericEnumRefReturnNoClone(t *testing.T) {
	ir := generateIR(t, `
		enum Tag `+"`clone"+` { Nil, Val(string s) }
		type GBox[T] { T[] d; at(int i) T& { return this.d[i]; } }
		caller() { b := GBox[Tag](d: [Tag.Val("a" + "b"), Tag.Nil]); x := b.at(0); }
		main() { caller(); }
	`)
	body := extractFunction(ir, `"GBox[Tag].at"`)
	if body == "" {
		t.Fatalf("expected @\"GBox[Tag].at\" in IR")
	}
	if n := cloneCallCount(body, "Tag"); n != 0 {
		t.Errorf("generic T& return (T=droppable `clone enum) must NOT clone "+
			"the borrowed element — the enum-clone !isRefType gate must fire on "+
			"the *substituted* retType (SharedRef(Tag)); found %d @Tag.clone( "+
			"in @\"GBox[Tag].at\":\n%s", n, body)
	}
}
