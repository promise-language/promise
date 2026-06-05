package codegen

import "testing"

// T0780: if/while-unwrap of a failable call returning an optional must
// auto-propagate the error before unwrapping. Before the fix, generateIR
// panicked (the raw failable result struct was misread as the optional).

func TestIfUnwrapFailableOptionalAutoPropagates(t *testing.T) {
	ir := generateIR(t, `
loadf!(string k) string? { return none; }
caller!() string {
  if v := loadf("x") {
    return v;
  }
  return "d";
}
main() {
  s := caller()? e { "" };
}
`)
	assertContains(t, ir, "auto.propagate")
	assertContains(t, ir, "ifunwrap.then")
}

func TestWhileUnwrapFailableOptionalAutoPropagates(t *testing.T) {
	ir := generateIR(t, `
dec!(int n) int? {
  if n <= 0 { return none; }
  return n - 1;
}
drain!(int start) int {
  int c = start;
  int steps = 0;
  while next := dec(c) {
    c = next;
    steps = steps + 1;
  }
  return steps;
}
main() {
  n := drain(3)? e { 0 };
}
`)
	assertContains(t, ir, "auto.propagate")
	assertContains(t, ir, "whileunwrap.body")
}

// T0781: `expr? e { none }` recovery on an optional-typed failable must lower
// `none` to the full optional struct, not a bare i1. Verifying the IR is
// well-formed (no panic, recovery present) guards the regression; the runtime
// behavior is covered by tests/e2e/failable_optional_unwrap_test.pr.
func TestNoneRecoveryOnOptionalFailable(t *testing.T) {
	ir := generateIR(t, `
loadf!(string k) string? { return none; }
recover_none!(string k) string? {
  return loadf(k)? e { none };
}
main() {
  r := recover_none("x")? e { none };
}
`)
	assertContains(t, ir, "error.handler")
	assertContains(t, ir, "error.merge")
}
