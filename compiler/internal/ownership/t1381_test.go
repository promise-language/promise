package ownership

import "testing"

// T1381: `failable_task[T]` is linear (must-use). It carries an error that must
// reach exactly one receiver, so it must be discharged — received (`<-t` /
// `<-tasks`) or moved onward (field/collection/argument/return) — before its
// owner's scope ends. Must-use is transitive: a type/collection that owns a
// failable_task is itself must-use.

// An undischarged local reaching end of scope is a compile error.
func TestT1381_UndischargedLocalRejected(t *testing.T) {
	errs := ownerErrs(t, `
		produce!(int x) int { return x; }
		test() {
			t := go! produce(5);
		}
	`)
	expectOwnerError(t, errs, "never received")
}

// Receiving the task discharges the obligation.
func TestT1381_ReceivedOK(t *testing.T) {
	ownerOK(t, `
		produce!(int x) int { return x; }
		test() {
			t := go! produce(5);
			a := (<-t)?!;
		}
	`)
}

// Returning the task moves the obligation to the caller — discharged here.
func TestT1381_ReturnedOK(t *testing.T) {
	ownerOK(t, `
		produce!(int x) int { return x; }
		make() failable_task[int] {
			t := go! produce(5);
			return t;
		}
	`)
}

// Moving the task into another owned binding transfers the obligation; the new
// binding must then be discharged.
func TestT1381_MoveTransfersObligation(t *testing.T) {
	ownerOK(t, `
		produce!(int x) int { return x; }
		test() {
			t := go! produce(5);
			c := t;
			a := (<-c)?!;
		}
	`)
	// ...but if the moved-to binding is not discharged, it is still an error.
	errs := ownerErrs(t, `
		produce!(int x) int { return x; }
		test() {
			t := go! produce(5);
			c := t;
		}
	`)
	expectOwnerError(t, errs, "never received")
}

// Reusing a task after it was received is a use-after-move (receive is
// consuming).
func TestT1381_ReuseAfterReceiveRejected(t *testing.T) {
	errs := ownerErrs(t, `
		produce!(int x) int { return x; }
		test() {
			t := go! produce(5);
			a := (<-t)?!;
			b := (<-t)?!;
		}
	`)
	expectOwnerError(t, errs, "moved")
}

// A collection of failable tasks is transitively must-use: dropping it is an
// error; draining it discharges the obligation.
func TestT1381_ContainerMustUse(t *testing.T) {
	errs := ownerErrs(t, `
		produce!(int x) int { return x; }
		test() {
			v := [go! produce(1), go! produce(2)];
		}
	`)
	expectOwnerError(t, errs, "never received")

	ownerOK(t, `
		produce!(int x) int { return x; }
		test() {
			v := [go! produce(1), go! produce(2)];
			rs := (<-v)?!;
		}
	`)
}

// A user type holding a failable_task field is transitively must-use: dropping
// it is an error; moving it onward or receiving the field discharges it.
func TestT1381_HolderMustUse(t *testing.T) {
	errs := ownerErrs(t, `
		produce!(int x) int { return x; }
		type H { failable_task[int] t; }
		test() {
			h := H(t: go! produce(1));
		}
	`)
	expectOwnerError(t, errs, "never received")

	// Field receive discharges the holder.
	ownerOK(t, `
		produce!(int x) int { return x; }
		type H { failable_task[int] t; }
		test() {
			h := H(t: go! produce(1));
			a := (<-h.t)?!;
		}
	`)

	// Moving the whole holder onward transfers the obligation.
	ownerOK(t, `
		produce!(int x) int { return x; }
		type H { failable_task[int] t; }
		make() H {
			h := H(t: go! produce(1));
			return h;
		}
	`)
}

// An in-place field receive `<-h.t` discharges the holder's obligation WITHOUT
// consuming the holder — its other fields stay usable afterward (no false
// use-after-move).
func TestT1381_FieldReceiveKeepsHolderUsable(t *testing.T) {
	ownerOK(t, `
		produce!(int x) int { return x; }
		type H { failable_task[int] t; int other; }
		test() {
			h := H(t: go! produce(1), other: 5);
			a := (<-h.t)?!;
			b := h.other;
		}
	`)
}

// Branch-partial discharge is unsound: a task received on only one branch is
// still must-use (the other path swallows the error). Validates the AND-merge.
func TestT1381_BranchPartialRejected(t *testing.T) {
	errs := ownerErrs(t, `
		produce!(int x) int { return x; }
		test(bool c) {
			t := go! produce(1);
			if c {
				a := (<-t)?!;
			}
		}
	`)
	expectOwnerError(t, errs, "never received")
}

// Discharged on EVERY branch → OK.
func TestT1381_BranchAllDischargedOK(t *testing.T) {
	ownerOK(t, `
		produce!(int x) int { return x; }
		test(bool c) {
			t := go! produce(1);
			if c {
				a := (<-t)?!;
			} else {
				b := (<-t)?!;
			}
		}
	`)
}

// A failable_task PARAM is the caller's obligation, not the callee's — a callee
// that never receives a borrowed failable_task param is not flagged.
func TestT1381_ParamNotFlagged(t *testing.T) {
	ownerOK(t, `
		observe(failable_task[int] tk) {
			x := 1;
		}
	`)
}

// Match-arm partial discharge is unsound: a task received on only one arm is
// still must-use (the other arm swallows the error). Validates the AND-merge at
// checkMatchExpr — the match-statement sibling of TestT1381_BranchPartialRejected.
func TestT1381_MatchArmPartialRejected(t *testing.T) {
	errs := ownerErrs(t, `
		produce!(int x) int { return x; }
		enum E { A, B }
		test(E e) {
			t := go! produce(1);
			match e {
				E.A => { a := (<-t)?!; },
				E.B => { },
			}
		}
	`)
	expectOwnerError(t, errs, "never received")
}

// Discharged on EVERY match arm → OK (merge(Moved, Moved) → Moved).
func TestT1381_MatchAllArmsDischargedOK(t *testing.T) {
	ownerOK(t, `
		produce!(int x) int { return x; }
		enum E { A, B }
		test(E e) {
			t := go! produce(1);
			match e {
				E.A => { a := (<-t)?!; },
				E.B => { b := (<-t)?!; },
			}
		}
	`)
}

// Select-case partial discharge is unsound the same way: a task received on one
// case but not the `default` is still must-use. Validates the AND-merge at
// checkSelectStmt (which also merges with the pre-select state when no default
// case exists).
func TestT1381_SelectCasePartialRejected(t *testing.T) {
	errs := ownerErrs(t, `
		produce!(int x) int { return x; }
		test(channel[int] ch) {
			t := go! produce(1);
			select {
				v := <-ch:
					a := (<-t)?!;
				default:
					b := 0;
			}
		}
	`)
	expectOwnerError(t, errs, "never received")
}

// Discharged on EVERY select alternative → OK.
func TestT1381_SelectAllCasesDischargedOK(t *testing.T) {
	ownerOK(t, `
		produce!(int x) int { return x; }
		test(channel[int] ch) {
			t := go! produce(1);
			select {
				v := <-ch:
					a := (<-t)?!;
				default:
					b := (<-t)?!;
			}
		}
	`)
}

// A field receive through a nested field chain (`<-o.inner.t`) discharges the
// root holder `o` (memberRootName walks the whole chain to the ident root), and
// `o` stays usable afterward.
func TestT1381_NestedFieldReceiveDischarges(t *testing.T) {
	ownerOK(t, `
		produce!(int x) int { return x; }
		type Inner { failable_task[int] t; int n; }
		type Outer { Inner inner; }
		test() {
			o := Outer(inner: Inner(t: go! produce(1), n: 5));
			a := (<-o.inner.t)?!;
			b := o.inner.n;
		}
	`)
}

// A parenthesized field-receive target (`<-(h).t`) still discharges the holder:
// memberRootName peels the ParenExpr around the target down to the ident root.
func TestT1381_ParenTargetFieldReceiveDischarges(t *testing.T) {
	ownerOK(t, `
		produce!(int x) int { return x; }
		type H { failable_task[int] t; int n; }
		test() {
			h := H(t: go! produce(1), n: 5);
			a := (<-(h).t)?!;
			b := h.n;
		}
	`)
}

// §17.2.1 lists "argument" as a discharge path. Moving a failable_task into a
// `move` parameter transfers the obligation to the callee — discharged here.
func TestT1381_MoveIntoArgumentDischarges(t *testing.T) {
	ownerOK(t, `
		produce!(int x) int { return x; }
		consume(failable_task[int] move tk) { a := (<-tk)?!; }
		test() {
			t := go! produce(5);
			consume(move t);
		}
	`)
}

// A BORROW parameter does not consume, so passing a failable_task to one leaves
// the caller still owning it — the obligation persists and reaching scope end
// undischarged is an error (the argument-move dual of the above).
func TestT1381_BorrowArgumentDoesNotDischarge(t *testing.T) {
	errs := ownerErrs(t, `
		produce!(int x) int { return x; }
		observe(failable_task[int] tk) { }
		test() {
			t := go! produce(5);
			observe(t);
		}
	`)
	expectOwnerError(t, errs, "never received")
}

// An enum whose variant payload owns a failable_task is transitively must-use
// (ContainsFailableTask descends into variant fields): letting the enum value
// reach scope end undischarged is an error, and moving the whole enum onward
// transfers the obligation. (The types predicate is covered in the types package;
// this exercises the ownership-pass integration — the enum-payload sibling of
// TestT1381_HolderMustUse.)
func TestT1381_EnumPayloadMustUse(t *testing.T) {
	errs := ownerErrs(t, `
		produce!(int x) int { return x; }
		enum Box { Some(failable_task[int] t), Empty }
		test() {
			b := Box.Some(go! produce(1));
		}
	`)
	expectOwnerError(t, errs, "never received")

	// Moving the whole enum onward (return) transfers the obligation.
	ownerOK(t, `
		produce!(int x) int { return x; }
		enum Box { Some(failable_task[int] t), Empty }
		make() Box {
			b := Box.Some(go! produce(1));
			return b;
		}
	`)
}
