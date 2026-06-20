package codegen

import (
	"regexp"
	"strings"
	"testing"
)

// T0660: a borrowed-ref return of a droppable ``clone`` enum from a Vector,
// used as the receiver of an enum method/getter call, crashed with
// `fatal: invalid free (bad header magic)`.
//
// Root cause: genEnumMethodCall / genGenericEnumMethodCall / genEnumGetterAccess
// synthesize an `enum.this`/`enum.getter` receiver alloca, store the receiver
// enum into it, and emit a post-call `@<Enum>.drop` on that temp whenever
// isFreshEnumExpr(receiver) is true. isFreshEnumExpr is a pure AST predicate —
// it returns true for ANY *ast.CallExpr, so it cannot tell an owned-return call
// (`make_tagged()`) apart from a borrow-return call (`ev.at(0)` typed
// `Tagged&`). For a borrow return the synthesized temp is a shallow copy whose
// payload pointer is shared with the vector element; dropping it frees memory
// the vector still owns → double-free / UAF at scope exit
// (EnumVecRef.drop → Vector.drop → Tagged.drop on the same pointer).
//
// T0649 fixed the *call-site temp-track* and the *method-body return-clone* but
// missed this third, independent enum-receiver-drop mechanism. The fix gates
// the three `tempEnumPtr = ptr` assignments on `!c.isBorrowedExpr(<recv>)`
// (the same well-tested helper T0649's binding-site borrow-flag clear uses).
//
// These tests lock the IR signature: the synthesized enum receiver temp (the
// register that is `bitcast %<enum>* %enum.this/getter to i8*`) must be passed
// to the method/getter call but NOT to `@<Enum>.drop` for a `T&`/`T~` receiver
// — while an IR-identical OWNED `T` receiver MUST still be dropped (the gate is
// borrow-targeted, not a blanket disable). Runtime no-double-free / 0-leak
// across the bind / temp / mutref / generic / getter surfaces is enforced by
// the batch tests in tests/e2e/ref_return_bind_test.pr.

// enumReceiverTempRegister returns the SSA register that holds the i8* bitcast
// of the synthesized enum receiver temp (`%enum.this` / `%enum.getter`) in the
// given function body, or "" if no such bitcast exists. There is exactly one
// such alloca per single enum-method/getter call site.
func enumReceiverTempRegister(body string) string {
	re := regexp.MustCompile(`(%[\w.]+) = bitcast %\w+\* %enum\.(?:this|getter)[\w.]* to i8\*`)
	m := re.FindStringSubmatch(body)
	if m == nil {
		return ""
	}
	return m[1]
}

// enumReceiverTempDropped reports whether `@<enumName>.drop(i8* <reg>)` appears
// in body for the receiver-temp register reg. This is the exact, precise
// signature of the buggy post-call receiver drop — it does not collide with the
// unrelated vector-element / scope-exit `@<enumName>.drop` calls (those use
// different SSA registers).
func enumReceiverTempDropped(body, reg, enumName string) bool {
	return strings.Contains(body, "@"+enumName+".drop(i8* "+reg+")")
}

// TestT0660_BorrowedEnumReceiverNoReceiverDrop — the core repro (site 3,
// genEnumMethodCall). `EnumVecRef.at(int) Tagged&` hands back a borrow of
// ev.items[0] (T0649 Part 1 correctly suppresses the return-clone). Calling
// `.size()` on `ev.at(0)` must NOT drop the synthesized receiver temp —
// otherwise `Tagged.drop` frees the shared `Named(string)` payload that
// Vector.drop frees again at scope exit (the `bad header magic` crash).
func TestT0660_BorrowedEnumReceiverNoReceiverDrop(t *testing.T) {
	ir := generateIR(t, `
		enum Tagged `+"`clone"+` {
			Empty,
			Named(string s),
			size(this) int { match this { Empty => { return 0; }, Named(n) => { return n.len; } } }
		}
		type EnumVecRef { Tagged[] items; at(int i) Tagged& { return this.items[i]; } }
		caller() {
			ev := EnumVecRef(items: [Tagged.Named("ab" + "c"), Tagged.Empty]);
			a := ev.at(0).size();
		}
		main() { caller(); }
	`)
	body := extractFunction(ir, "__user.caller")
	if body == "" {
		t.Fatalf("expected @__user.caller in IR")
	}
	reg := enumReceiverTempRegister(body)
	if reg == "" {
		t.Fatalf("expected a `bitcast %%<enum>* %%enum.this to i8*` receiver "+
			"temp in @__user.caller (test program shape changed?):\n%s", body)
	}
	// Sanity: the method call on the synthesized temp is actually present —
	// otherwise the no-drop assertion below would be vacuous.
	if !strings.Contains(body, "@Tagged.size(i8* "+reg+")") {
		t.Fatalf("expected @Tagged.size(i8* %s) on the receiver temp:\n%s", reg, body)
	}
	if enumReceiverTempDropped(body, reg, "Tagged") {
		t.Errorf("borrowed `Tagged&` receiver temp %s must NOT be dropped "+
			"(T0660: !isBorrowedExpr gate) — dropping it double-frees the "+
			"vector element's shared payload; found @Tagged.drop(i8* %s):\n%s",
			reg, reg, body)
	}
}

// TestT0660_OwnedEnumReceiverStillDrops — precision control. An IDENTICAL
// accessor returning an OWNED `Tagged` (no `&`) used as a temp receiver MUST
// still drop the synthesized receiver temp: the caller received an independent
// enum (cloned by the T0649 owned-return path) with no other owner. This proves
// the T0660 guard is borrow-targeted, not a blanket disable. The two callers
// are IR-identical except for this single receiver-temp drop — so this test
// also pins exactly what the pre-fix borrowed IR looked like (the regression
// the borrowed test above catches).
func TestT0660_OwnedEnumReceiverStillDrops(t *testing.T) {
	ir := generateIR(t, `
		enum Tagged `+"`clone"+` {
			Empty,
			Named(string s),
			size(this) int { match this { Empty => { return 0; }, Named(n) => { return n.len; } } }
		}
		type EnumVecOwn { Tagged[] items; at(int i) Tagged { return this.items[i]; } }
		caller() {
			ev := EnumVecOwn(items: [Tagged.Named("ab" + "c"), Tagged.Empty]);
			a := ev.at(0).size();
		}
		main() { caller(); }
	`)
	body := extractFunction(ir, "__user.caller")
	if body == "" {
		t.Fatalf("expected @__user.caller in IR")
	}
	reg := enumReceiverTempRegister(body)
	if reg == "" {
		t.Fatalf("expected a receiver temp bitcast in @__user.caller:\n%s", body)
	}
	if !enumReceiverTempDropped(body, reg, "Tagged") {
		t.Errorf("owned `Tagged` receiver temp %s (control) MUST still be "+
			"dropped — the T0660 !isBorrowedExpr gate must be precise, not a "+
			"blanket disable; @Tagged.drop(i8* %s) absent:\n%s", reg, reg, body)
	}
}

// TestT0660_BorrowedMutRefEnumReceiverNoReceiverDrop — the `~` (MutRef)
// variant. isBorrowedExpr returns true for *types.MutRef as well as
// *types.SharedRef, so a `Tagged~` borrow-return receiver is gated identically
// to `Tagged&`.
func TestT0660_BorrowedMutRefEnumReceiverNoReceiverDrop(t *testing.T) {
	ir := generateIR(t, `
		enum Tagged `+"`clone"+` {
			Empty,
			Named(string s),
			size(this) int { match this { Empty => { return 0; }, Named(n) => { return n.len; } } }
		}
		type EnumVecRef { Tagged[] items; at_mut(int i) Tagged~ { return this.items[i]; } }
		caller() {
			ev := EnumVecRef(items: [Tagged.Named("ab" + "c"), Tagged.Empty]);
			a := ev.at_mut(0).size();
		}
		main() { caller(); }
	`)
	body := extractFunction(ir, "__user.caller")
	if body == "" {
		t.Fatalf("expected @__user.caller in IR")
	}
	reg := enumReceiverTempRegister(body)
	if reg == "" {
		t.Fatalf("expected a receiver temp bitcast in @__user.caller:\n%s", body)
	}
	if enumReceiverTempDropped(body, reg, "Tagged") {
		t.Errorf("borrowed `Tagged~` (MutRef) receiver temp %s must NOT be "+
			"dropped (T0660 gate covers *types.MutRef); found "+
			"@Tagged.drop(i8* %s):\n%s", reg, reg, body)
	}
}

// TestT0660_GenericOwnerBorrowedEnumReceiverNoReceiverDrop — the substituted
// path. `GBox[GTag].at(int) T&` returns `SharedRef(TypeParam T)` which only
// becomes `SharedRef(Instance GTag)` after types.Substitute under the
// GBox[GTag] mono subst. isBorrowedExpr applies c.typeSubst before the
// SharedRef/MutRef check, so the borrow is still detected and the
// monomorphized caller must not drop the receiver temp. (`.size()` is a
// non-generic method on the concrete GTag instance → site 3 with subst.)
func TestT0660_GenericOwnerBorrowedEnumReceiverNoReceiverDrop(t *testing.T) {
	ir := generateIR(t, `
		enum GTag `+"`clone"+` {
			Nil,
			Val(string s),
			size(this) int { match this { Nil => { return 0; }, Val(n) => { return n.len; } } }
		}
		type GBox[T] { T[] d; at(int i) T& { return this.d[i]; } }
		caller() {
			b := GBox[GTag](d: [GTag.Val("ab" + "c"), GTag.Nil]);
			a := b.at(0).size();
		}
		main() { caller(); }
	`)
	body := extractFunction(ir, "__user.caller")
	if body == "" {
		t.Fatalf("expected @__user.caller in IR")
	}
	reg := enumReceiverTempRegister(body)
	if reg == "" {
		t.Fatalf("expected a receiver temp bitcast in @__user.caller:\n%s", body)
	}
	if enumReceiverTempDropped(body, reg, "GTag") {
		t.Errorf("borrowed generic `T&` (T=GTag) receiver temp %s must NOT be "+
			"dropped — the T0660 gate must fire on the *substituted* receiver "+
			"type (SharedRef(GTag)); found @GTag.drop(i8* %s):\n%s",
			reg, reg, body)
	}
}

// TestT0660_GenericEnumMethodBorrowedReceiverNoReceiverDrop — site 1
// (genGenericEnumMethodCall). A GENERIC enum method (`describe[U]`) invoked on
// a borrowed enum receiver routes through the separate generic-enum-method-call
// path; that site has its own `tempEnumPtr` assignment and its own T0660 gate.
func TestT0660_GenericEnumMethodBorrowedReceiverNoReceiverDrop(t *testing.T) {
	ir := generateIR(t, `
		enum GTag `+"`clone"+` {
			Nil,
			Val(string s),
			describe[U](U tag) int { return 7; }
		}
		type GBox[T] { T[] d; at(int i) T& { return this.d[i]; } }
		caller() {
			b := GBox[GTag](d: [GTag.Val("ab" + "c"), GTag.Nil]);
			a := b.at(0).describe[int](5);
		}
		main() { caller(); }
	`)
	body := extractFunction(ir, "__user.caller")
	if body == "" {
		t.Fatalf("expected @__user.caller in IR")
	}
	reg := enumReceiverTempRegister(body)
	if reg == "" {
		t.Fatalf("expected a receiver temp bitcast in @__user.caller:\n%s", body)
	}
	if !strings.Contains(body, `@"GTag.describe[int]"(i8* `+reg+`,`) {
		t.Fatalf("expected the generic enum method call on the receiver "+
			"temp %s:\n%s", reg, body)
	}
	if enumReceiverTempDropped(body, reg, "GTag") {
		t.Errorf("borrowed receiver temp %s of a GENERIC enum method "+
			"(genGenericEnumMethodCall, site 1) must NOT be dropped (T0660); "+
			"found @GTag.drop(i8* %s):\n%s", reg, reg, body)
	}
}

// TestT0660_EnumGetterBorrowedReceiverNoReceiverDrop — site 2
// (genEnumGetterAccess). An enum getter accessed on a borrowed enum receiver
// synthesizes an `enum.getter` temp; that site has its own `tempEnumPtr`
// assignment and its own T0660 gate.
func TestT0660_EnumGetterBorrowedReceiverNoReceiverDrop(t *testing.T) {
	ir := generateIR(t, `
		enum GTag `+"`clone"+` {
			Nil,
			Val(string s),
			get label int { match this { Nil => { return 0; }, Val(n) => { return n.len; } } }
		}
		type GBox[T] { T[] d; at(int i) T& { return this.d[i]; } }
		caller() {
			b := GBox[GTag](d: [GTag.Val("ab" + "c"), GTag.Nil]);
			a := b.at(0).label;
		}
		main() { caller(); }
	`)
	body := extractFunction(ir, "__user.caller")
	if body == "" {
		t.Fatalf("expected @__user.caller in IR")
	}
	reg := enumReceiverTempRegister(body)
	if reg == "" {
		t.Fatalf("expected an `enum.getter` receiver temp bitcast in "+
			"@__user.caller:\n%s", body)
	}
	if !strings.Contains(body, "@GTag.label(i8* "+reg+")") {
		t.Fatalf("expected @GTag.label(i8* %s) on the getter temp:\n%s", reg, body)
	}
	if enumReceiverTempDropped(body, reg, "GTag") {
		t.Errorf("borrowed receiver temp %s of an enum GETTER "+
			"(genEnumGetterAccess, site 2) must NOT be dropped (T0660); "+
			"found @GTag.drop(i8* %s):\n%s", reg, reg, body)
	}
}

// TestT0660_OwnedGenericEnumMethodReceiverStillDrops — precision control for
// site 1 (genGenericEnumMethodCall). The borrowed site-1 test above only
// exercises the SUPPRESSED branch (isBorrowedExpr → true → tempEnumPtr stays
// nil); the `tempEnumPtr = ptr` assignment at expr.go:2097 was uncovered by
// the entire codegen suite. This pins the inverse direction: a generic enum
// method (`scaled[U]`) invoked on a fresh OWNED `Tagged` receiver (by-value
// accessor → T0649 owned-return clone, no other owner) MUST still drop the
// synthesized receiver temp — otherwise the over-suppression of T0660 would
// silently leak the cloned `Named(string)` payload. Mirrors
// TestT0660_OwnedEnumReceiverStillDrops but routes through the
// generic-enum-method path.
func TestT0660_OwnedGenericEnumMethodReceiverStillDrops(t *testing.T) {
	ir := generateIR(t, `
		enum Tagged `+"`clone"+` {
			Empty,
			Named(string s),
			scaled[U](this, U _marker, int extra) int {
				match this { Empty => { return extra; }, Named(n) => { return n.len + extra; } }
			}
		}
		type EnumVecOwn { Tagged[] items; at(int i) Tagged { return this.items[i]; } }
		caller() {
			ev := EnumVecOwn(items: [Tagged.Named("ab" + "c"), Tagged.Empty]);
			a := ev.at(0).scaled[int](0, 10);
		}
		main() { caller(); }
	`)
	body := extractFunction(ir, "__user.caller")
	if body == "" {
		t.Fatalf("expected @__user.caller in IR")
	}
	reg := enumReceiverTempRegister(body)
	if reg == "" {
		t.Fatalf("expected a receiver temp bitcast in @__user.caller:\n%s", body)
	}
	// Sanity: the generic enum method call on the synthesized temp is present
	// (otherwise the drop assertion is vacuous and we are not at site 1).
	if !strings.Contains(body, `@"Tagged.scaled[int]"(i8* `+reg+`,`) {
		t.Fatalf("expected the generic enum method call on the receiver "+
			"temp %s (site 1, genGenericEnumMethodCall):\n%s", reg, body)
	}
	if !enumReceiverTempDropped(body, reg, "Tagged") {
		t.Errorf("owned `Tagged` receiver temp %s of a GENERIC enum method "+
			"(genGenericEnumMethodCall, site 1) MUST still be dropped — the "+
			"T0660 !isBorrowedExpr gate at expr.go:2097 must be precise, not a "+
			"blanket disable; @Tagged.drop(i8* %s) absent (would leak the "+
			"cloned payload):\n%s", reg, reg, body)
	}
}

// TestT0660_OwnedEnumGetterReceiverStillDrops — precision control for site 2
// (genEnumGetterAccess), symmetric to the site-3 owned control. A fresh OWNED
// `Tagged` receiver of an enum GETTER (`tag_len`) must still drop the
// synthesized getter temp. Locks the T0660 gate's precision at all three
// modified sites (site 3: TestT0660_OwnedEnumReceiverStillDrops; site 1:
// TestT0660_OwnedGenericEnumMethodReceiverStillDrops; site 2: here).
func TestT0660_OwnedEnumGetterReceiverStillDrops(t *testing.T) {
	ir := generateIR(t, `
		enum Tagged `+"`clone"+` {
			Empty,
			Named(string s),
			get tag_len int { match this { Empty => { return 0; }, Named(n) => { return n.len; } } }
		}
		type EnumVecOwn { Tagged[] items; at(int i) Tagged { return this.items[i]; } }
		caller() {
			ev := EnumVecOwn(items: [Tagged.Named("ab" + "c"), Tagged.Empty]);
			a := ev.at(0).tag_len;
		}
		main() { caller(); }
	`)
	body := extractFunction(ir, "__user.caller")
	if body == "" {
		t.Fatalf("expected @__user.caller in IR")
	}
	reg := enumReceiverTempRegister(body)
	if reg == "" {
		t.Fatalf("expected an `enum.getter` receiver temp bitcast in "+
			"@__user.caller:\n%s", body)
	}
	if !strings.Contains(body, "@Tagged.tag_len(i8* "+reg+")") {
		t.Fatalf("expected @Tagged.tag_len(i8* %s) on the getter temp:\n%s", reg, body)
	}
	if !enumReceiverTempDropped(body, reg, "Tagged") {
		t.Errorf("owned `Tagged` receiver temp %s of an enum GETTER "+
			"(genEnumGetterAccess, site 2) MUST still be dropped — the T0660 "+
			"!isBorrowedExpr gate at expr.go:4416 must be precise, not a "+
			"blanket disable; @Tagged.drop(i8* %s) absent:\n%s", reg, reg, body)
	}
}
