package sema

import "testing"

// T0998: `&` is removed from parameter position — the bare `T name` form is the
// shared (read-only) borrow. `&`-typed params are a hard sema error guiding to
// the bare spelling. Mutable-borrow params keep the `T~ name` spelling.

func TestT0998_AmpParamRejected(t *testing.T) {
	errs := checkErrs(t, `
		peek(Box& b) int { return 1; }
		type Box { string s; }
	`)
	expectError(t, errs, "`&` is not a parameter marker")
}

func TestT0998_AmpParamSpacedRejected(t *testing.T) {
	errs := checkErrs(t, `
		peek(Box &b) int { return 1; }
		type Box { string s; }
	`)
	expectError(t, errs, "is already a shared (read-only) borrow")
}

func TestT0998_BareParamIsSharedBorrowOK(t *testing.T) {
	// The bare form replaces `&` and must type-check cleanly.
	checkOK(t, `
		peek(Box b) int { return 1; }
		type Box { string s; }
	`)
}

func TestT0998_MutBorrowParamStillValid(t *testing.T) {
	// `T~ name` (mutable borrow, via the MutRef type) remains valid.
	checkOK(t, `
		bump(Box~ b) { }
		type Box { string s; }
	`)
}

func TestT0998_MoveParamValid(t *testing.T) {
	checkOK(t, `
		sink(Box move b) { }
		type Box { string s; }
	`)
}

// The `&`-param rejection must fire on method signatures, not just free
// functions (resolveMethodSignature path).
func TestT0998_AmpParamOnMethodRejected(t *testing.T) {
	errs := checkErrs(t, `
		type Box { string s; peek(Box& other) int { return 1; } }
	`)
	expectError(t, errs, "`&` is not a parameter marker")
}

// ...and on enum method signatures (resolveEnumMethodSignature path).
func TestT0998_AmpParamOnEnumMethodRejected(t *testing.T) {
	errs := checkErrs(t, `
		enum E {
			A,
			peek(Box& b) int { return 1; }
		}
		type Box { string s; }
	`)
	expectError(t, errs, "`&` is not a parameter marker")
}

// `extern` declarations are an FFI boundary: `T&` describes the C pointer ABI,
// not an ownership borrow, so the `&`-param removal is exempted there.
func TestT0998_ExternAmpParamAllowed(t *testing.T) {
	checkOK(t, "c_peek(string& s) `extern(\"c_peek\");")
}

// A non-extern `&` param of the same shape is still rejected — confirms the
// extern carve-out, not a blanket pass, is what allows the FFI form.
func TestT0998_NonExternAmpParamStillRejected(t *testing.T) {
	errs := checkErrs(t, `peek(string& s) {}`)
	expectError(t, errs, "`&` is not a parameter marker")
}

// A `T&` reference value reborrows into a bare (shared-borrow) parameter
// (reborrowAssignable, SharedRef branch).
func TestT0998_ReborrowSharedRefToBareParam(t *testing.T) {
	checkOK(t, `
		get_ref(string s) string& { return s; }
		peek(string b) {}
		test() {
			string s = "hi";
			string& r = get_ref(s);
			peek(r);
		}
	`)
}

// A `T~` mutable reference value also reborrows into a bare borrow parameter
// (reborrowAssignable, MutRef branch).
func TestT0998_ReborrowMutRefToBareParam(t *testing.T) {
	checkOK(t, `
		get_mut(string ~s) string~ { return s; }
		peek(string b) {}
		test() {
			string s = "hi";
			string~ r = get_mut(s);
			peek(r);
		}
	`)
}

// A `T&` value does NOT satisfy a `T~` (mutable-borrow) parameter — reborrow
// only relaxes binding into a bare borrow slot, not a stricter mutable one.
func TestT0998_NoReborrowSharedIntoMutParam(t *testing.T) {
	errs := checkErrs(t, `
		get_ref(string s) string& { return s; }
		bump(string ~b) {}
		test() {
			string s = "hi";
			string& r = get_ref(s);
			bump(r);
		}
	`)
	expectError(t, errs, "cannot assign")
}
