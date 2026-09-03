package sema

import "testing"

// T1416: Failable [] and [:] getter reads in expression position were not
// recorded in FailableExprs, so sema never diagnosed the unhandled failable
// call and codegen panicked trying to store {i1,T,i8*} into T*.

// --- failable [] read ---

func TestT1416FailableIndexReadInNonFailableFunction(t *testing.T) {
	errs := checkErrs(t, `
		type Box {
			int v;
			[]!(int i) int { if i < 0 { raise error(message: "oob"); } return this.v; }
		}
		main() {
			b := Box(v: 3);
			int y = b[0];
		}
	`)
	expectError(t, errs, "failable call must be handled")
}

func TestT1416FailableIndexReadInFailableFunction(t *testing.T) {
	errs := checkErrs(t, `
		type Box {
			int v;
			[]!(int i) int { if i < 0 { raise error(message: "oob"); } return this.v; }
		}
		main!() {
			b := Box(v: 3);
			int y = b[0];
		}
	`)
	expectNoErrors(t, errs)
}

func TestT1416NonFailableIndexReadNoError(t *testing.T) {
	// Sanity check: a total [] getter must not be flagged.
	checkOK(t, `
		type Box {
			int v;
			[](int i) int { return this.v; }
		}
		main() {
			b := Box(v: 3);
			int y = b[0];
		}
	`)
}

func TestT1416FailableIndexReadAsSubExpr(t *testing.T) {
	errs := checkErrs(t, `
		type Box {
			int v;
			[]!(int i) int { if i < 0 { raise error(message: "oob"); } return this.v; }
		}
		main() {
			b := Box(v: 3);
			print_line("{b[0]}");
		}
	`)
	expectError(t, errs, "failable call must be handled")
}

func TestT1416FailableIndexReadWithErrorHandler(t *testing.T) {
	errs := checkErrs(t, `
		type Box {
			int v;
			[]!(int i) int { if i < 0 { raise error(message: "oob"); } return this.v; }
		}
		main() {
			b := Box(v: 3);
			int y = b[0] ? e { 0 };
		}
	`)
	expectNoErrors(t, errs)
}

// --- failable [:] read ---

func TestT1416FailableSliceReadInNonFailableFunction(t *testing.T) {
	errs := checkErrs(t, `
		type Box {
			int v;
			[:]!(int? low, int? high) int { if this.v < 0 { raise error(message: "neg"); } return this.v; }
		}
		main() {
			b := Box(v: 3);
			int y = b[0:1];
		}
	`)
	expectError(t, errs, "failable call must be handled")
}

func TestT1416FailableSliceReadInFailableFunction(t *testing.T) {
	errs := checkErrs(t, `
		type Box {
			int v;
			[:]!(int? low, int? high) int { if this.v < 0 { raise error(message: "neg"); } return this.v; }
		}
		main!() {
			b := Box(v: 3);
			int y = b[0:1];
		}
	`)
	expectNoErrors(t, errs)
}

func TestT1416NonFailableSliceReadNoError(t *testing.T) {
	// Sanity check: a total [:] getter must not be flagged.
	checkOK(t, `
		type Box {
			int v;
			[:](int? low, int? high) int { return this.v; }
		}
		main() {
			b := Box(v: 3);
			int y = b[0:1];
		}
	`)
}

func TestT1416FailableSliceReadAsSubExpr(t *testing.T) {
	errs := checkErrs(t, `
		type Box {
			int v;
			[:]!(int? low, int? high) int { if this.v < 0 { raise error(message: "neg"); } return this.v; }
		}
		main() {
			b := Box(v: 3);
			print_line("{b[0:1]}");
		}
	`)
	expectError(t, errs, "failable call must be handled")
}

func TestT1416FailableSliceReadWithErrorHandler(t *testing.T) {
	errs := checkErrs(t, `
		type Box {
			int v;
			[:]!(int? low, int? high) int { if this.v < 0 { raise error(message: "neg"); } return this.v; }
		}
		main() {
			b := Box(v: 3);
			int y = b[0:1] ? e { 0 };
		}
	`)
	expectNoErrors(t, errs)
}

// --- failable [] with ?^ propagation ---

func TestT1416FailableIndexReadWithPropagation(t *testing.T) {
	errs := checkErrs(t, `
		type Box {
			int v;
			[]!(int i) int { if i < 0 { raise error(message: "oob"); } return this.v; }
		}
		helper!() int {
			b := Box(v: 3);
			int y = b[0] ?^;
			return y;
		}
		main!() {
			int r = helper()?!;
		}
	`)
	expectNoErrors(t, errs)
}

func TestT1416FailableSliceReadWithPropagation(t *testing.T) {
	errs := checkErrs(t, `
		type Box {
			int v;
			[:]!(int? low, int? high) int { if this.v < 0 { raise error(message: "neg"); } return this.v; }
		}
		helper!() int {
			b := Box(v: 3);
			int y = b[0:1] ?^;
			return y;
		}
		main!() {
			int r = helper()?!;
		}
	`)
	expectNoErrors(t, errs)
}

// --- failable [] on generic type (exercises the subst path) ---

func TestT1416FailableIndexReadOnGenericType(t *testing.T) {
	errs := checkErrs(t, `
		type Container[T] {
			T val;
			[]!(int i) T { if i < 0 { raise error(message: "oob"); } return this.val; }
		}
		main() {
			c := Container[int](val: 42);
			int y = c[0];
		}
	`)
	expectError(t, errs, "failable call must be handled")
}

func TestT1416FailableIndexReadOnGenericTypeInFailableFunc(t *testing.T) {
	errs := checkErrs(t, `
		type Container[T] {
			T val;
			[]!(int i) T { if i < 0 { raise error(message: "oob"); } return this.val; }
		}
		main!() {
			c := Container[int](val: 42);
			int y = c[0];
		}
	`)
	expectNoErrors(t, errs)
}

// --- failable [] in return position ---

func TestT1416FailableIndexReadInReturnPosition(t *testing.T) {
	errs := checkErrs(t, `
		type Box {
			int v;
			[]!(int i) int { if i < 0 { raise error(message: "oob"); } return this.v; }
		}
		get_value() int {
			b := Box(v: 3);
			return b[0];
		}
		main() { int x = get_value(); }
	`)
	expectError(t, errs, "failable call must be handled")
}

func TestT1416FailableIndexReadInReturnPositionOfFailableFunc(t *testing.T) {
	errs := checkErrs(t, `
		type Box {
			int v;
			[]!(int i) int { if i < 0 { raise error(message: "oob"); } return this.v; }
		}
		get_value!() int {
			b := Box(v: 3);
			return b[0];
		}
		main!() { int x = get_value()?!; }
	`)
	expectNoErrors(t, errs)
}
