package sema

import "testing"

// T1873: a bare failable call used as the entire condition of if/while/for/
// if-expression must be auto-propagated (failable function) or rejected
// (non-failable function), same as T1267 (match arms) and T0984 (binary
// operands).

// --- if statement ---

func TestT1873BareFailableIfCondFailableOK(t *testing.T) {
	checkOK(t, `
		f!() bool { return true; }
		g!() int {
			if f() { return 1; }
			return 0;
		}
		main() {}
	`)
}

func TestT1873BareFailableIfCondNonFailableErrors(t *testing.T) {
	errs := checkErrs(t, `
		f!() bool { return true; }
		g() int {
			if f() { return 1; }
			return 0;
		}
		main() {}
	`)
	expectError(t, errs, "failable call must be handled")
}

// --- while statement ---

func TestT1873BareFailableWhileCondFailableOK(t *testing.T) {
	checkOK(t, `
		f!() bool { return true; }
		g!() {
			while f() { break; }
		}
		main() {}
	`)
}

func TestT1873BareFailableWhileCondNonFailableErrors(t *testing.T) {
	errs := checkErrs(t, `
		f!() bool { return true; }
		g() {
			while f() { break; }
		}
		main() {}
	`)
	expectError(t, errs, "failable call must be handled")
}

// --- C-style for statement ---

func TestT1873BareFailableForCondFailableOK(t *testing.T) {
	checkOK(t, `
		f!() bool { return true; }
		g!() {
			for int i = 0; f(); i++ { break; }
		}
		main() {}
	`)
}

func TestT1873BareFailableForCondNonFailableErrors(t *testing.T) {
	errs := checkErrs(t, `
		f!() bool { return true; }
		g() {
			for int i = 0; f(); i++ { break; }
		}
		main() {}
	`)
	expectError(t, errs, "failable call must be handled")
}

// --- if expression ---

func TestT1873BareFailableIfExprCondFailableOK(t *testing.T) {
	checkOK(t, `
		f!() bool { return true; }
		g!() int {
			v := if f() { 1 } else { 0 };
			return v;
		}
		main() {}
	`)
}

func TestT1873BareFailableIfExprCondNonFailableErrors(t *testing.T) {
	errs := checkErrs(t, `
		f!() bool { return true; }
		g() int {
			v := if f() { 1 } else { 0 };
			return v;
		}
		main() {}
	`)
	expectError(t, errs, "failable call must be handled")
}

// --- if-stmt-value (value-producing position via genBlockValue) ---

func TestT1873BareFailableIfStmtValueCondFailableOK(t *testing.T) {
	checkOK(t, `
		f!() bool { return true; }
		g!() int {
			int x = if f() { 1; } else { 0; };
			return x;
		}
		main() {}
	`)
}

func TestT1873BareFailableIfStmtValueCondNonFailableErrors(t *testing.T) {
	errs := checkErrs(t, `
		f!() bool { return true; }
		g() int {
			int x = if f() { 1; } else { 0; };
			return x;
		}
		main() {}
	`)
	expectError(t, errs, "failable call must be handled")
}

// --- edge: failable method call as condition ---

func TestT1873BareFailableMethodCallCondOK(t *testing.T) {
	checkOK(t, `
		type Checker {
			check!(this) bool { return true; }
		}
		g!() int {
			c := Checker();
			if c.check() { return 1; }
			return 0;
		}
		main() {}
	`)
}

func TestT1873BareFailableMethodCallCondNonFailableErrors(t *testing.T) {
	errs := checkErrs(t, `
		type Checker {
			check!(this) bool { return true; }
		}
		g() int {
			c := Checker();
			if c.check() { return 1; }
			return 0;
		}
		main() {}
	`)
	expectError(t, errs, "failable call must be handled")
}

// --- edge: nested failable conditions ---

func TestT1873NestedBareFailableCondOK(t *testing.T) {
	checkOK(t, `
		outer!() bool { return true; }
		inner!() bool { return false; }
		g!() int {
			if outer() {
				if inner() { return 2; }
				return 1;
			}
			return 0;
		}
		main() {}
	`)
}

// --- edge: non-failable call in condition is not affected ---

func TestT1873NonFailableCallCondUnchanged(t *testing.T) {
	checkOK(t, `
		f() bool { return true; }
		g() int {
			if f() { return 1; }
			return 0;
		}
		main() {}
	`)
}
