package sema

import "testing"

// T0780: an if/while-unwrap whose scrutinee is a failable call must be handled —
// auto-propagated in a failable function, rejected in a non-failable one.

func TestIfUnwrapFailableInNonFailableErrors(t *testing.T) {
	errs := checkErrs(t, `
loadf!(string k) string? { return none; }
caller() {
  if v := loadf("x") {
  }
}
main() {}
`)
	expectError(t, errs, "failable call must be handled")
}

func TestWhileUnwrapFailableInNonFailableErrors(t *testing.T) {
	errs := checkErrs(t, `
loadf!(string k) string? { return none; }
caller() {
  while v := loadf("x") {
  }
}
main() {}
`)
	expectError(t, errs, "failable call must be handled")
}

func TestIfUnwrapFailableInFailableOK(t *testing.T) {
	errs := checkErrs(t, `
loadf!(string k) string? { return none; }
caller!() string {
  if v := loadf("x") {
    return v;
  }
  return "default";
}
main() {}
`)
	expectNoErrors(t, errs)
}

func TestWhileUnwrapFailableInFailableOK(t *testing.T) {
	errs := checkErrs(t, `
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
main() {}
`)
	expectNoErrors(t, errs)
}
