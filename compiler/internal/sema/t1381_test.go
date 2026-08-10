package sema

import "testing"

// T1381: `failable_task[T]` is must-use, and `<-tasks` (drain over
// failable_task[T][]) is a failable operation yielding T[].

// Draining a failable_task[T][] yields Vector[T] (usable as such) and is a
// failable operation (auto-propagated in a failable function).
func TestT1381_DrainYieldsVectorInFailableFn(t *testing.T) {
	checkOK(t, `
		produce!(int x) int { return x; }
		run!() int {
			v := [go! produce(1), go! produce(2)];
			xs := (<-v)?^;
			return xs[0] + xs.len;
		}
	`)
}

// A drain is failable — an unhandled drain in a non-failable function is a
// compile error, exactly like any unhandled failable call.
func TestT1381_UnhandledDrainRejected(t *testing.T) {
	errs := checkErrs(t, `
		produce!(int x) int { return x; }
		run() {
			v := [go! produce(1), go! produce(2)];
			xs := <-v;
		}
	`)
	expectError(t, errs, "failable call must be handled")
}

// Discarding a bare `go!` failable task as an expression statement is rejected
// (fire-and-forget must be non-failable).
func TestT1381_DiscardGoBangRejected(t *testing.T) {
	errs := checkErrs(t, `
		produce!(int x) int { return x; }
		run() {
			go! produce(5);
		}
	`)
	expectError(t, errs, "fire-and-forget")
}

// Discarding a call that returns a failable_task (a must-use value) as an
// expression statement silently swallows its error — rejected.
func TestT1381_DiscardMustUseCallRejected(t *testing.T) {
	errs := checkErrs(t, `
		produce!(int x) int { return x; }
		make() failable_task[int] {
			t := go! produce(5);
			return t;
		}
		run() {
			make();
		}
	`)
	expectError(t, errs, "silently swallows its error")
}

// Binding a must-use value to `_` also discards it — rejected.
func TestT1381_DiscardToUnderscoreRejected(t *testing.T) {
	errs := checkErrs(t, `
		produce!(int x) int { return x; }
		run() {
			_ := go! produce(5);
		}
	`)
	expectError(t, errs, "silently swallows its error")
}
