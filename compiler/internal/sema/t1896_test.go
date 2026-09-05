package sema

import "testing"

// T1896: a bare failable call used as the iterable of a for-in loop is an
// expression position like any other — it must auto-propagate in a failable
// function and be diagnosed in a non-failable one. checkForInStmt never called
// checkFailableEscape, so neither happened and codegen was handed a raw
// failable result struct.

// autoPropagated returns the number of expressions sema recorded for implicit
// unwrapping in the user file. checkOK alone cannot tell a fixed compiler from
// the broken one: the bug was a *missing* check, so the broken build also
// produced zero errors and passed. Counting the recorded expressions is what
// distinguishes "sema settled the iterable" from "sema walked past it".
func autoPropagated(t *testing.T, info *Info, want int) {
	t.Helper()
	if got := len(info.AutoPropagateExprs); got != want {
		t.Errorf("AutoPropagateExprs: got %d entries, want %d", got, want)
	}
}

func TestT1896BareFailableForInVectorPropagates(t *testing.T) {
	info := checkOK(t, `
		mkv!() u8[] {
			u8[] v = u8[]();
			v.push(1u8);
			return v;
		}
		run!() int {
			int n = 0;
			for b in mkv() { n += 1; }
			return n;
		}
		main() {}
	`)
	autoPropagated(t, info, 1)
}

func TestT1896BareFailableForInVectorNonFailableErrors(t *testing.T) {
	errs := checkErrs(t, `
		mkv!() u8[] {
			u8[] v = u8[]();
			v.push(1u8);
			return v;
		}
		run() int {
			int n = 0;
			for b in mkv() { n += 1; }
			return n;
		}
		main() {}
	`)
	expectError(t, errs, "failable call must be handled")
}

func TestT1896BareFailableForInStringPropagates(t *testing.T) {
	info := checkOK(t, `
		mks!() string { return "abc"; }
		run!() int {
			int n = 0;
			for ch in mks() { n += 1; }
			return n;
		}
		main() {}
	`)
	autoPropagated(t, info, 1)
}

// T1417 is the duck-typed half of the same defect: a failable factory returning
// a plain type with next(~this) T? iterated zero times with no diagnostic.
func TestT1896BareFailableForInDuckTypedPropagates(t *testing.T) {
	info := checkOK(t, `
		type Counter {
			int n;
			next(~this) int? {
				if this.n >= 3 { return none; }
				int v = this.n;
				this.n = this.n + 1;
				return v;
			}
		}
		mkc!() Counter { return Counter(n: 0); }
		run!() int {
			int sum = 0;
			for x in mkc() { sum += x; }
			return sum;
		}
		main() {}
	`)
	autoPropagated(t, info, 1)
}

func TestT1896BareFailableForInDuckTypedNonFailableErrors(t *testing.T) {
	errs := checkErrs(t, `
		type Counter {
			int n;
			next(~this) int? {
				if this.n >= 3 { return none; }
				int v = this.n;
				this.n = this.n + 1;
				return v;
			}
		}
		mkc!() Counter { return Counter(n: 0); }
		run() int {
			int sum = 0;
			for x in mkc() { sum += x; }
			return sum;
		}
		main() {}
	`)
	expectError(t, errs, "failable call must be handled")
}

// The raw generator path is the one iterable that must NOT take the new
// checkFailableEscape route. It owns its own error routing and deliberately
// *panics* rather than erroring when a bare failable factory appears in a
// non-propagating context (T0284) — the guard is isRawGeneratorForIn.
func TestT1896BareFailableGeneratorForInNonFailableStillAllowed(t *testing.T) {
	info := checkOK(t, `
		gen!(int n) stream[int] {
			int i = 0;
			while i < n { yield i; i = i + 1; }
		}
		run() int {
			int sum = 0;
			for x in gen(3) { sum += x; }
			return sum;
		}
		main() {}
	`)
	// The carve-out: the raw generator path routes its own error, so nothing
	// is recorded for implicit unwrapping here.
	autoPropagated(t, info, 0)
}

func TestT1896BareFailableGeneratorForInFailablePropagates(t *testing.T) {
	info := checkOK(t, `
		gen!(int n) stream[int] {
			int i = 0;
			while i < n { yield i; i = i + 1; }
		}
		run!() int {
			int sum = 0;
			for x in gen(3) { sum += x; }
			return sum;
		}
		main() {}
	`)
	// The carve-out: the raw generator path routes its own error, so nothing
	// is recorded for implicit unwrapping here.
	autoPropagated(t, info, 0)
}

// --- Iterables that are not CallExprs -------------------------------------
//
// FailableExprs is set on more node kinds than calls: a failable *getter* is a
// MemberExpr, `<-t` on a failable_task[T] is a UnaryExpr (§17.2.1), and
// failability propagates through parentheses (T1379). checkFailableEscape keys
// off FailableExprs, not off the node kind, so each of these is a for-in
// subject the fix must settle — and each reaches a different corner of
// isRawGeneratorForIn, whose CallExpr test is the thing that decides whether an
// iterable is a raw generator.

func TestT1896BareFailableForInGetterPropagates(t *testing.T) {
	info := checkOK(t, `
		type Holder {
			get items! int[] {
				int[] v = [];
				v.push(1);
				return v;
			}
		}
		run!() int {
			Holder h = Holder();
			int n = 0;
			for x in h.items { n += x; }
			return n;
		}
		main() {}
	`)
	autoPropagated(t, info, 1)
}

func TestT1896BareFailableForInGetterNonFailableErrors(t *testing.T) {
	errs := checkErrs(t, `
		type Holder {
			get items! int[] {
				int[] v = [];
				v.push(1);
				return v;
			}
		}
		run() int {
			Holder h = Holder();
			int n = 0;
			for x in h.items { n += x; }
			return n;
		}
		main() {}
	`)
	expectError(t, errs, "failable call must be handled")
}

func TestT1896BareFailableForInParenthesizedPropagates(t *testing.T) {
	info := checkOK(t, `
		mkv!() int[] {
			int[] v = [];
			v.push(1);
			return v;
		}
		run!() int {
			int n = 0;
			for x in (mkv()) { n += x; }
			return n;
		}
		main() {}
	`)
	// One entry, on the ParenExpr: failability propagates outward through the
	// parentheses (T1379), and the iterable — the outermost node — is what
	// checkForInStmt settles and what codegen later looks up.
	autoPropagated(t, info, 1)
}

// §17.2.1: `<-t` on a failable_task[T] is a failable operation in its own right,
// so awaiting a task that produced a container and iterating the result in one
// step is a bare failable iterable with no call node anywhere.
func TestT1896BareFailableForInAwaitedTaskPropagates(t *testing.T) {
	info := checkOK(t, `
		mkv!() int[] {
			int[] v = [];
			v.push(1);
			return v;
		}
		run!() int {
			t := go! { mkv()?^ };
			int n = 0;
			for x in <-t { n += x; }
			return n;
		}
		main() {}
	`)
	autoPropagated(t, info, 1)
}

// --- Per-branch diagnostics ------------------------------------------------
//
// genForInStmt dispatches on the iterable's type, and each branch reads the
// value independently. The diagnostic is emitted once in checkForInStmt, before
// that dispatch, so one test per shape is what proves it is not accidentally
// tied to the vector branch that the repro used.

func TestT1896BareFailableForInMapNonFailableErrors(t *testing.T) {
	errs := checkErrs(t, `
		mkm!() map[string, int] {
			map[string, int] m = {:};
			return m;
		}
		run() int {
			int n = 0;
			for k, v in mkm() { n += v; }
			return n;
		}
		main() {}
	`)
	expectError(t, errs, "failable call must be handled")
}

func TestT1896BareFailableForInRangeNonFailableErrors(t *testing.T) {
	errs := checkErrs(t, `
		mkr!() Range[int] { return 0..3; }
		run() int {
			int n = 0;
			for i in mkr() { n += 1; }
			return n;
		}
		main() {}
	`)
	expectError(t, errs, "failable call must be handled")
}

func TestT1896BareFailableForInStringNonFailableErrors(t *testing.T) {
	errs := checkErrs(t, `
		mks!() string { return "abc"; }
		run() int {
			int n = 0;
			for ch in mks() { n += 1; }
			return n;
		}
		main() {}
	`)
	expectError(t, errs, "failable call must be handled")
}

func TestT1896BareFailableForInArrayNonFailableErrors(t *testing.T) {
	errs := checkErrs(t, `
		mka!() int[3] {
			int[3] a = [1, 2, 3];
			return a;
		}
		run() int {
			int n = 0;
			for x in mka() { n += x; }
			return n;
		}
		main() {}
	`)
	expectError(t, errs, "failable call must be handled")
}

func TestT1896BareFailableForInChannelNonFailableErrors(t *testing.T) {
	errs := checkErrs(t, `
		mkch!() channel[int] {
			ch := channel[int](capacity: 1);
			ch.close();
			return ch;
		}
		run() int {
			int n = 0;
			for x in mkch() { n += 1; }
			return n;
		}
		main() {}
	`)
	expectError(t, errs, "failable call must be handled")
}

// A type that is iterable through iter() rather than next() records ForInIter,
// and it does so on the *duck-typed* branch — not the Stream[T] one, which is
// where isRawGeneratorForIn's carve-out lives. Getting a failable factory of
// this shape past the carve-out is the distinction the predicate has to draw.
func TestT1896BareFailableForInIterMethodPropagates(t *testing.T) {
	info := checkOK(t, `
		type NumsIter {
			int cur;
			int limit;
			next(~this) int? {
				if this.cur >= this.limit { return none; }
				int v = this.cur;
				this.cur = this.cur + 1;
				return v;
			}
		}
		type Nums {
			int limit;
			iter(~this) NumsIter { return NumsIter(cur: 0, limit: this.limit); }
		}
		mkn!(int limit) Nums { return Nums(limit: limit); }
		run!() int {
			int sum = 0;
			for x in mkn(4) { sum += x; }
			return sum;
		}
		main() {}
	`)
	autoPropagated(t, info, 1)
}

func TestT1896BareFailableForInIterMethodNonFailableErrors(t *testing.T) {
	errs := checkErrs(t, `
		type NumsIter {
			int cur;
			int limit;
			next(~this) int? {
				if this.cur >= this.limit { return none; }
				int v = this.cur;
				this.cur = this.cur + 1;
				return v;
			}
		}
		type Nums {
			int limit;
			iter(~this) NumsIter { return NumsIter(cur: 0, limit: this.limit); }
		}
		mkn!(int limit) Nums { return Nums(limit: limit); }
		run() int {
			int sum = 0;
			for x in mkn(4) { sum += x; }
			return sum;
		}
		main() {}
	`)
	expectError(t, errs, "failable call must be handled")
}

// --- The explicit spellings still work ------------------------------------
//
// §11 makes `?^` equivalent to the bare form, so adding the bare route must not
// disturb the operator forms — including in a non-failable function, where `?!`
// is the only legal spelling and must stay legal.

func TestT1896ExplicitOperatorsOnForInIterableStillAccepted(t *testing.T) {
	for _, spelling := range []string{"mkv()?^", "mkv()?!"} {
		t.Run(spelling, func(t *testing.T) {
			info := checkOK(t, `
				mkv!() int[] {
					int[] v = [];
					v.push(1);
					return v;
				}
				run!() int {
					int n = 0;
					for x in `+spelling+` { n += x; }
					return n;
				}
				main() {}
			`)
			// The operator handles the error itself, so nothing is recorded for
			// implicit unwrapping — the two mechanisms must not both fire.
			autoPropagated(t, info, 0)
		})
	}
}

func TestT1896ExplicitPanicOperatorAllowedInNonFailableForIn(t *testing.T) {
	errs := checkErrs(t, `
		mkv!() int[] {
			int[] v = [];
			v.push(1);
			return v;
		}
		run() int {
			int n = 0;
			for x in mkv()?! { n += x; }
			return n;
		}
		main() {}
	`)
	expectNoErrors(t, errs)
}

// --- isRawGeneratorForIn's own edges --------------------------------------

// The predicate runs before the iterable's type has been validated, so it must
// tolerate a nil type: an unresolvable iterable has to produce the "undefined"
// diagnostic, not a nil dereference inside the new for-in code.
func TestT1896UnresolvableForInIterableDoesNotPanic(t *testing.T) {
	errs := checkErrs(t, `
		run() int {
			int n = 0;
			for x in no_such_function() { n += 1; }
			return n;
		}
		main() {}
	`)
	expectError(t, errs, "undefined: no_such_function")
	expectNoErrorContaining(t, errs, "failable call must be handled")
}

// T1735's shape, re-asserted here because the fix reordered the branch that
// chooses between the raw-generator path and the structural view: a Stream[T]
// *variable* is a view, must not be mistaken for a generator, and carries no
// failability of its own.
//
// The *getter* spelling of the same thing is deliberately absent: a getter that
// yields is a generator factory, but isStructuralStreamView only tests for a
// CallExpr, so it is misread as a view and segfaults at runtime — T1943, which
// predates this item and is not on the failable path at all. Add the case here
// when it lands.
func TestT1896StructuralStreamViewIterableUnaffected(t *testing.T) {
	info := checkOK(t, `
		type NumsIter {
			int cur;
			int limit;
			next(~this) int? {
				if this.cur >= this.limit { return none; }
				int v = this.cur;
				this.cur = this.cur + 1;
				return v;
			}
		}
		type Nums {
			int limit;
			iter(~this) NumsIter { return NumsIter(cur: 0, limit: this.limit); }
		}
		run() int {
			Stream[int] s = Nums(limit: 3);
			int sum = 0;
			for x in s { sum += x; }
			return sum;
		}
		main() {}
	`)
	autoPropagated(t, info, 0)
}
