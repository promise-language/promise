package sema

import (
	"strings"
	"testing"
)

// T1386: `go! { }` body-can-fail detection missed several escaping constructs —
// a for-in over a failable generator, a use binding whose close() is failable, a
// typed error handler with no `else`/`!`, and every failable-operator form
// (binary, prefix-unary, inc/dec, compound assignment, failable setter/getter).
// Each was wrongly rejected with "this goroutine's body cannot fail". The guard
// must still fire for bodies that genuinely cannot fail, so every accept case
// has a reject twin.
//
// Runtime coverage for the accepted bodies (the error really does surface at
// `<-t`): tests/concurrency/gobang_failable_operator_test.pr,
// tests/concurrency/gobang_use_failable_close_test.pr,
// tests/concurrency/failable_task_test.pr.
//
// Two shapes are deliberately NOT covered because they are separately broken:
//   - `for x in gen() ? e { alt() }` (the ErrorHandlerExpr arm of the iterable
//     peel) — T1420: invalid IR, or a silent swallow when the arms' layouts
//     happen to match. Reproduces without any `go`/`go!`.
//   - a fan-out through a `failable_task[T][]` — T1434: the for-in `<-` exits the
//     process with no output on Windows and hangs on macOS. The concurrent
//     fan-out coverage uses plain task locals instead.

const t1386CannotFail = "cannot fail; spawn it with plain"

// expectCannotFailCount asserts exactly how many `go!` bodies were rejected —
// the accept/reject twins only prove a body was scored, not that a *sibling* or
// enclosing body kept its own score.
func expectCannotFailCount(t *testing.T, errs []error, want int) {
	t.Helper()
	got := 0
	for _, e := range errs {
		if strings.Contains(e.Error(), t1386CannotFail) {
			got++
		}
	}
	if got != want {
		t.Fatalf("expected %d %q errors, got %d: %v", want, t1386CannotFail, got, errs)
	}
}

// checkedVec is a value type whose `+`, prefix `-` and `++` are all failable.
const checkedVec = `
	type CheckedVec {
		int x ` + "`value" + `;
		+!(CheckedVec o) CheckedVec { if o.x < 0 { raise error("neg"); } return CheckedVec(x: this.x + o.x); }
		- !() CheckedVec { if this.x < 0 { raise error("neg"); } return CheckedVec(x: 0 - this.x); }
		++!() CheckedVec { if this.x < 0 { raise error("neg"); } return CheckedVec(x: this.x + 1); }
	}
`

// plainVec is checkedVec's non-failable twin — the reject side of each operator
// case, so the accept cases prove the *failability* is what counts, not the
// presence of a user-defined operator.
const plainVec = `
	type PlainVec {
		int x ` + "`value" + `;
		+(PlainVec o) PlainVec { return PlainVec(x: this.x + o.x); }
		- () PlainVec { return PlainVec(x: 0 - this.x); }
		++() PlainVec { return PlainVec(x: this.x + 1); }
	}
`

// --- for-in over a failable generator ---

func TestT1386GoBlockForInFailableGenerator(t *testing.T) {
	// The generator's mid-stream `raise` auto-propagates into the task, so the
	// for-in alone makes the body failable.
	errs := checkErrs(t, `
		gen!(bool f) stream[int] { if f { raise error(message: "x"); } yield 1; }
		main!() {
			t := go! { int s = 0; for x in gen(true) { s = s + x; } s };
			v := (<-t)?!;
		}
	`)
	expectNoErrors(t, errs)
}

func TestT1386GoBlockForInFailableGeneratorWithPanicOperator(t *testing.T) {
	// `?!` handles only the *factory* call's error; mid-stream errors still
	// route to the enclosing sink, so this is still a failable body.
	errs := checkErrs(t, `
		gen!(bool f) stream[int] { if f { raise error(message: "x"); } yield 1; }
		main!() {
			t := go! { int s = 0; for x in gen(true)?! { s = s + x; } s };
			v := (<-t)?!;
		}
	`)
	expectNoErrors(t, errs)
}

func TestT1386GoBlockForInNonFailableGeneratorRejected(t *testing.T) {
	// A non-failable generator cannot fail mid-stream — the body must still be
	// rejected, guarding against marking every stream for-in as an escape.
	errs := checkErrs(t, `
		gen() stream[int] { yield 1; }
		main!() {
			t := go! { int s = 0; for x in gen() { s = s + x; } s };
			v := (<-t)?!;
		}
	`)
	expectError(t, errs, t1386CannotFail)
}

func TestT1386GoBlockForInFailableNonGeneratorRejected(t *testing.T) {
	// A failable call returning a plain container cannot fail *during*
	// iteration, and the `?!` handles the call's own error locally — so the
	// body genuinely cannot fail. Guards the stream-only gating of the mark.
	errs := checkErrs(t, `
		get_list!() int[] { return [1, 2]; }
		main!() {
			t := go! { int s = 0; for x in get_list()?! { s = s + x; } s };
			v := (<-t)?!;
		}
	`)
	expectError(t, errs, t1386CannotFail)
}

// --- use binding with a failable close() ---

func TestT1386GoBlockUseFailableClose(t *testing.T) {
	// close!() raises on scope exit — an escape out of the goroutine body.
	errs := checkErrs(t, `
		type CloseError is error { int code; }
		type Res { int id; close!(~this) { raise CloseError(message: "cf", code: 1); } }
		main!() {
			t := go! { use r := Res(id: 1); r.id };
			v := (<-t)?!;
		}
	`)
	expectNoErrors(t, errs)
}

func TestT1386GoBlockUseNonFailableCloseRejected(t *testing.T) {
	// A non-failable close() cannot raise, so the body still cannot fail.
	errs := checkErrs(t, `
		type Res { int id; close(~this) { } }
		main!() {
			t := go! { use r := Res(id: 1); r.id };
			v := (<-t)?!;
		}
	`)
	expectError(t, errs, t1386CannotFail)
}

func TestT1386GoBlockUseGenericFailableClose(t *testing.T) {
	// The resource is a generic instance, so the close() lookup goes through the
	// *types.Instance arm of checkUseVarDecl rather than the plain *types.Named
	// one — the same escape must be recorded.
	errs := checkErrs(t, `
		type CloseError is error { int code; }
		type Res[T] { T id; close!(~this) { raise CloseError(message: "cf", code: 1); } }
		main!() {
			t := go! { use r := Res[int](id: 1); r.id };
			v := (<-t)?!;
		}
	`)
	expectNoErrors(t, errs)
}

func TestT1386GoBlockUseGenericNonFailableCloseRejected(t *testing.T) {
	errs := checkErrs(t, `
		type Res[T] { T id; close(~this) { } }
		main!() {
			t := go! { use r := Res[int](id: 1); r.id };
			v := (<-t)?!;
		}
	`)
	expectError(t, errs, t1386CannotFail)
}

func TestT1386GoBlockUseFailableCloseThatNeverRaises(t *testing.T) {
	// Being declared `close!` is the contract; whether the body can actually
	// raise is not knowable here (same rule as `go! f()` for a failable f that
	// always succeeds). So a total-in-practice failable close still counts.
	errs := checkErrs(t, `
		type Res { int id; close!(~this) { } }
		main!() {
			t := go! { use r := Res(id: 1); r.id };
			v := (<-t)?!;
		}
	`)
	expectNoErrors(t, errs)
}

func TestT1386UseBindingOnNonNamedTypeRejected(t *testing.T) {
	// The escape check reads the resolved close() method, so it sits after the
	// two "this cannot be a use binding" guards. A lambda resolves to a signature
	// type — no *types.Named at all — and must be rejected by the first guard,
	// before anything tries to look a close() up on it. (A primitive like `int`
	// does resolve to a Named, so it falls into the second guard instead.)
	errs := checkErrs(t, `
		main() {
			use x := || -> 1;
		}
	`)
	expectError(t, errs, "use binding requires a type with close() method")
}

func TestT1386UseBindingWithoutCloseMethodRejected(t *testing.T) {
	// The second guard: a real type, but no close() to be failable or otherwise.
	errs := checkErrs(t, `
		type Plain { int id; }
		main() {
			use r := Plain(id: 1);
		}
	`)
	expectError(t, errs, "has no close() method")
}

// --- typed error handler with no else/! ---

func TestT1386GoBlockTypedHandlerNoMatch(t *testing.T) {
	// A non-matching error propagates out of the handler into the task.
	errs := checkErrs(t, `
		type IoError is error { int code; }
		foo!() int { raise IoError(message: "fail", code: 1); }
		main!() {
			t := go! { int v = foo() ? e is IoError { 0 }; v };
			v := (<-t)?!;
		}
	`)
	expectNoErrors(t, errs)
}

func TestT1386GoBlockTypedHandlerElseRejected(t *testing.T) {
	// `else { }` makes the handler exhaustive — nothing escapes.
	errs := checkErrs(t, `
		type IoError is error { int code; }
		foo!() int { raise IoError(message: "fail", code: 1); }
		main!() {
			t := go! { int v = foo() ? e is IoError { 0 } else { 0 }; v };
			v := (<-t)?!;
		}
	`)
	expectError(t, errs, t1386CannotFail)
}

func TestT1386GoBlockTypedHandlerPanicRejected(t *testing.T) {
	// The `!` suffix panics on non-match — also exhaustive, nothing escapes.
	errs := checkErrs(t, `
		type IoError is error { int code; }
		foo!() int { raise IoError(message: "fail", code: 1); }
		main!() {
			t := go! { int v = foo() ? e is IoError { 0 } !; v };
			v := (<-t)?!;
		}
	`)
	expectError(t, errs, t1386CannotFail)
}

// --- failable operators (T0708/T0709/T0715/T0984 sites) ---
//
// Each of these auto-propagates a {ok, value, err} result via
// genAutoPropagateValue, whose inFailableGoBlock branch routes the error into
// the task — so each is an escape.

func TestT1386GoBlockFailableBinaryOperator(t *testing.T) {
	errs := checkErrs(t, checkedVec+`
		main!() {
			t := go! { a := CheckedVec(x: 10); c := a + CheckedVec(x: 5); c.x };
			v := (<-t)?!;
		}
	`)
	expectNoErrors(t, errs)
}

func TestT1386GoBlockNonFailableBinaryOperatorRejected(t *testing.T) {
	errs := checkErrs(t, plainVec+`
		main!() {
			t := go! { a := PlainVec(x: 10); c := a + PlainVec(x: 5); c.x };
			v := (<-t)?!;
		}
	`)
	expectError(t, errs, t1386CannotFail)
}

func TestT1386GoBlockFailableUnaryOperator(t *testing.T) {
	errs := checkErrs(t, checkedVec+`
		main!() {
			t := go! { a := CheckedVec(x: 10); c := -a; c.x };
			v := (<-t)?!;
		}
	`)
	expectNoErrors(t, errs)
}

func TestT1386GoBlockNonFailableUnaryOperatorRejected(t *testing.T) {
	errs := checkErrs(t, plainVec+`
		main!() {
			t := go! { a := PlainVec(x: 10); c := -a; c.x };
			v := (<-t)?!;
		}
	`)
	expectError(t, errs, t1386CannotFail)
}

func TestT1386GoBlockFailableIncDecOperator(t *testing.T) {
	errs := checkErrs(t, checkedVec+`
		main!() {
			t := go! { CheckedVec a = CheckedVec(x: 10); a++; a.x };
			v := (<-t)?!;
		}
	`)
	expectNoErrors(t, errs)
}

func TestT1386GoBlockNonFailableIncDecOperatorRejected(t *testing.T) {
	errs := checkErrs(t, plainVec+`
		main!() {
			t := go! { PlainVec a = PlainVec(x: 10); a++; a.x };
			v := (<-t)?!;
		}
	`)
	expectError(t, errs, t1386CannotFail)
}

func TestT1386GoBlockFailableCompoundAssign(t *testing.T) {
	errs := checkErrs(t, checkedVec+`
		main!() {
			t := go! { CheckedVec a = CheckedVec(x: 10); a += CheckedVec(x: 5); a.x };
			v := (<-t)?!;
		}
	`)
	expectNoErrors(t, errs)
}

func TestT1386GoBlockNonFailableCompoundAssignRejected(t *testing.T) {
	errs := checkErrs(t, plainVec+`
		main!() {
			t := go! { PlainVec a = PlainVec(x: 10); a += PlainVec(x: 5); a.x };
			v := (<-t)?!;
		}
	`)
	expectError(t, errs, t1386CannotFail)
}

func TestT1386GoBlockFailableSetterAssignment(t *testing.T) {
	// A failable setter propagates its error out of the assignment (T0708).
	errs := checkErrs(t, `
		type Counter {
			int _n;
			get value int { return this._n; }
			set value!(int v) { if v < 0 { raise error("neg"); } this._n = v; }
		}
		main!() {
			t := go! { c := Counter(_n: 0); c.value = 5; c._n };
			v := (<-t)?!;
		}
	`)
	expectNoErrors(t, errs)
}

func TestT1386GoBlockNonFailableSetterAssignmentRejected(t *testing.T) {
	errs := checkErrs(t, `
		type Counter {
			int _n;
			get value int { return this._n; }
			set value(int v) { this._n = v; }
		}
		main!() {
			t := go! { c := Counter(_n: 0); c.value = 5; c._n };
			v := (<-t)?!;
		}
	`)
	expectError(t, errs, t1386CannotFail)
}

func TestT1386GoBlockFailableIndexGetterCompoundAssign(t *testing.T) {
	// A compound assignment reads via the failable [] getter (T0709).
	errs := checkErrs(t, `
		type Box {
			int x;
			[]!(int k) int { if this.x < 0 { raise error("neg"); } return this.x; }
			[]=(int k, int v) { this.x = v; }
		}
		main!() {
			t := go! { b := Box(x: 0); b[0] += 5; b.x };
			v := (<-t)?!;
		}
	`)
	expectNoErrors(t, errs)
}

func TestT1386GoBlockNonFailableIndexGetterCompoundAssignRejected(t *testing.T) {
	// Reject twin: both accessors are total, so the compound assignment cannot
	// fail and neither can the body.
	errs := checkErrs(t, `
		type Box {
			int x;
			[](int k) int { return this.x; }
			[]=(int k, int v) { this.x = v; }
		}
		main!() {
			t := go! { b := Box(x: 0); b[0] += 5; b.x };
			v := (<-t)?!;
		}
	`)
	expectError(t, errs, t1386CannotFail)
}

func TestT1386GoBlockFailableIndexGetterIncDec(t *testing.T) {
	// `b[0]++` reads via the failable [] getter — the checkIncDecStmt half of
	// T0709, a distinct site from the compound-assignment one above.
	errs := checkErrs(t, `
		type Box {
			int x;
			[]!(int k) int { if this.x < 0 { raise error("neg"); } return this.x; }
			[]=(int k, int v) { this.x = v; }
		}
		main!() {
			t := go! { b := Box(x: 1); b[0]++; b.x };
			v := (<-t)?!;
		}
	`)
	expectNoErrors(t, errs)
}

func TestT1386GoBlockNonFailableIndexGetterIncDecRejected(t *testing.T) {
	errs := checkErrs(t, `
		type Box {
			int x;
			[](int k) int { return this.x; }
			[]=(int k, int v) { this.x = v; }
		}
		main!() {
			t := go! { b := Box(x: 1); b[0]++; b.x };
			v := (<-t)?!;
		}
	`)
	expectError(t, errs, t1386CannotFail)
}

func TestT1386GoBlockFailablePropertySetterIncDec(t *testing.T) {
	// `c.value++` writes back through the failable setter (T0712).
	errs := checkErrs(t, `
		type Counter {
			int _n;
			get value int { return this._n; }
			set value!(int v) { if v < 0 { raise error("neg"); } this._n = v; }
		}
		main!() {
			t := go! { Counter c = Counter(_n: 1); c.value++; c._n };
			v := (<-t)?!;
		}
	`)
	expectNoErrors(t, errs)
}

func TestT1386GoBlockFailablePropertyGetterIncDec(t *testing.T) {
	// The read half of T0712: a failable getter with a total setter.
	errs := checkErrs(t, `
		type Counter {
			int _n;
			get value! int { if this._n < 0 { raise error("neg"); } return this._n; }
			set value(int v) { this._n = v; }
		}
		main!() {
			t := go! { Counter c = Counter(_n: 1); c.value++; c._n };
			v := (<-t)?!;
		}
	`)
	expectNoErrors(t, errs)
}

func TestT1386GoBlockNonFailablePropertyIncDecRejected(t *testing.T) {
	errs := checkErrs(t, `
		type Counter {
			int _n;
			get value int { return this._n; }
			set value(int v) { this._n = v; }
		}
		main!() {
			t := go! { Counter c = Counter(_n: 1); c.value++; c._n };
			v := (<-t)?!;
		}
	`)
	expectError(t, errs, t1386CannotFail)
}

func TestT1386GoBlockFailableSliceSetterAssignment(t *testing.T) {
	// The `[:]=` half of T0708 — a distinct assignmentSetterCanError arm from
	// the named-property setter case above.
	errs := checkErrs(t, `
		type Box {
			int x;
			[:](int? low, int? high) int { return this.x; }
			[:]=!(int? low, int? high, int v) { if v < 0 { raise error("neg"); } this.x = v; }
		}
		main!() {
			t := go! { b := Box(x: 0); b[1:2] = 5; b.x };
			v := (<-t)?!;
		}
	`)
	expectNoErrors(t, errs)
}

func TestT1386GoBlockNonFailableSliceSetterAssignmentRejected(t *testing.T) {
	errs := checkErrs(t, `
		type Box {
			int x;
			[:](int? low, int? high) int { return this.x; }
			[:]=(int? low, int? high, int v) { this.x = v; }
		}
		main!() {
			t := go! { b := Box(x: 0); b[1:2] = 5; b.x };
			v := (<-t)?!;
		}
	`)
	expectError(t, errs, t1386CannotFail)
}

func TestT1386GoBlockFailableIndexSetterAssignment(t *testing.T) {
	// The `[]=` arm of assignmentSetterCanError — a plain index *store* through a
	// failable setter, distinct from the `[]` read arms above and from `[:]=`.
	errs := checkErrs(t, `
		type Box {
			int x;
			[](int k) int { return this.x; }
			[]=!(int k, int v) { if v < 0 { raise error("neg"); } this.x = v; }
		}
		main!() {
			t := go! { b := Box(x: 0); b[0] = 5; b.x };
			v := (<-t)?!;
		}
	`)
	expectNoErrors(t, errs)
}

func TestT1386GoBlockNonFailableIndexSetterAssignmentRejected(t *testing.T) {
	errs := checkErrs(t, `
		type Box {
			int x;
			[](int k) int { return this.x; }
			[]=(int k, int v) { this.x = v; }
		}
		main!() {
			t := go! { b := Box(x: 0); b[0] = 5; b.x };
			v := (<-t)?!;
		}
	`)
	expectError(t, errs, t1386CannotFail)
}

func TestT1386GoBlockFailableModuleLevelSetterAssignment(t *testing.T) {
	// The `*ast.IdentExpr` arm of assignmentSetterCanError: a same-file
	// module-level `set name!` reached by a bare `name = v` — no receiver, so it
	// resolves through the scope rather than a *types.Named.
	errs := checkErrs(t, `
		get value int { return 1; }
		set value!(int v) { if v < 0 { raise error("neg"); } }
		main!() {
			t := go! { value = 5; 0 };
			v := (<-t)?!;
		}
	`)
	expectNoErrors(t, errs)
}

func TestT1386GoBlockNonFailableModuleLevelSetterAssignmentRejected(t *testing.T) {
	errs := checkErrs(t, `
		get value int { return 1; }
		set value(int v) { }
		main!() {
			t := go! { value = 5; 0 };
			v := (<-t)?!;
		}
	`)
	expectError(t, errs, t1386CannotFail)
}

func TestT1386GoBlockFailableModuleLevelGetterCompoundAssign(t *testing.T) {
	// The `*ast.IdentExpr` arm of assignmentGetterCanError: `value += 5` reads
	// through the failable module-level getter before storing.
	errs := checkErrs(t, `
		get value! int { raise error("neg"); }
		set value(int v) { }
		main!() {
			t := go! { value += 5; 0 };
			v := (<-t)?!;
		}
	`)
	expectNoErrors(t, errs)
}

func TestT1386GoBlockNonFailableModuleLevelGetterCompoundAssignRejected(t *testing.T) {
	errs := checkErrs(t, `
		get value int { return 1; }
		set value(int v) { }
		main!() {
			t := go! { value += 5; 0 };
			v := (<-t)?!;
		}
	`)
	expectError(t, errs, t1386CannotFail)
}

// --- escape sites nested below the body's top-level statement list ---

func TestT1386GoBlockForInFailableGeneratorMethod(t *testing.T) {
	// The iterable is a method call, not a free function — the same
	// FailableExprs lookup, exercised through the member-call path.
	errs := checkErrs(t, `
		type Source {
			int n;
			rows!(bool f) stream[int] { if f { raise error(message: "x"); } yield this.n; }
		}
		main!() {
			t := go! { s := Source(n: 2); int q = 0; for x in s.rows(true) { q = q + x; } q };
			v := (<-t)?!;
		}
	`)
	expectNoErrors(t, errs)
}

func TestT1386GoBlockForInFailableGeneratorInsideIf(t *testing.T) {
	// The escape is nested inside an `if` body, not a top-level statement of the
	// go! block — the counter is checked across the whole body, not per statement.
	errs := checkErrs(t, `
		gen!(bool f) stream[int] { if f { raise error(message: "x"); } yield 1; }
		main!() {
			t := go! { int q = 0; if q == 0 { for x in gen(true) { q = q + x; } } q };
			v := (<-t)?!;
		}
	`)
	expectNoErrors(t, errs)
}

func TestT1386GoBlockUseFailableCloseInNestedBlock(t *testing.T) {
	// The use binding closes at the inner block's exit, still inside the body.
	errs := checkErrs(t, `
		type CloseError is error { int code; }
		type Res { int id; close!(~this) { raise CloseError(message: "cf", code: 1); } }
		main!() {
			t := go! { int q = 0; { use r := Res(id: 1); q = r.id; } q };
			v := (<-t)?!;
		}
	`)
	expectNoErrors(t, errs)
}

// --- non-propagating contexts record nothing and reject nothing ---
//
// recordFailableEscape returns false when the context cannot take the escape.
// For the two sites that panic instead of propagating (use-close, failable
// generator for-in) that means: no count, and — critically — no diagnostic.
// Before T1386 neither site consulted the failable context at all, so these
// guard against the routing accidentally turning into a rejection.

func TestT1386UseFailableCloseInNonFailableFunctionOk(t *testing.T) {
	checkOK(t, `
		type CloseError is error { int code; }
		type Res { int id; close!(~this) { raise CloseError(message: "cf", code: 1); } }
		main() {
			use r := Res(id: 1);
		}
	`)
}

func TestT1386ForInFailableGeneratorInNonFailableFunctionOk(t *testing.T) {
	checkOK(t, `
		gen!(bool f) stream[int] { if f { raise error(message: "x"); } yield 1; }
		main() {
			int q = 0;
			for x in gen(false) { q = q + x; }
		}
	`)
}

func TestT1386UseFailableCloseInPlainGoBlockOk(t *testing.T) {
	// A plain `go {}` body is a non-failable scope (T1217): the close error
	// panics in the goroutine rather than escaping, so nothing is recorded and
	// the spawn is still legal.
	checkOK(t, `
		type CloseError is error { int code; }
		type Res { int id; close!(~this) { raise CloseError(message: "cf", code: 1); } }
		main() {
			t := go { use r := Res(id: 1); r.id };
			v := <-t;
		}
	`)
}

func TestT1386ForInFailableGeneratorInPlainGoBlockOk(t *testing.T) {
	checkOK(t, `
		gen!(bool f) stream[int] { if f { raise error(message: "x"); } yield 1; }
		main() {
			t := go { int q = 0; for x in gen(false) { q = q + x; } q };
			v := <-t;
		}
	`)
}

// --- a nested go! body's escapes belong to the nested task ---

func TestT1386NestedGoBlockEscapeDoesNotCountForOuter(t *testing.T) {
	// The inner body's `?^` surfaces at the INNER task's `<-t2`; it is not an
	// escape out of the outer body, which therefore still cannot fail.
	errs := checkErrs(t, `
		foo!(int n) int { if n < 0 { raise error(message: "x"); } return n; }
		main!() {
			t := go! {
				t2 := go! { int b = foo(1)?^; b };
				int a = 0;
				a
			};
			v := (<-t)?!;
		}
	`)
	expectError(t, errs, t1386CannotFail)
}

func TestT1386NestedGoBlockRewindKeepsEarlierOuterEscape(t *testing.T) {
	// The outer escape happens BEFORE the inner block, so it is already inside
	// the inner block's snapshot. Rewinding to that snapshot must not roll it
	// back: the outer body can fail, the inner one cannot.
	errs := checkErrs(t, `
		foo!(int n) int { if n < 0 { raise error(message: "x"); } return n; }
		main!() {
			t := go! {
				int a = foo(1)?^;
				t2 := go! { 0 };
				a
			};
			v := (<-t)?!;
		}
	`)
	expectCannotFailCount(t, errs, 1)
}

func TestT1386SiblingGoBlocksAreScoredIndependently(t *testing.T) {
	// One escaping body next to a non-escaping one: the first body's escape must
	// not carry over to the second, in either order.
	errs := checkErrs(t, `
		foo!(int n) int { if n < 0 { raise error(message: "x"); } return n; }
		main!() {
			t1 := go! { int a = foo(1)?^; a };
			t2 := go! { 0 };
			v := (<-t1)?!;
			w := (<-t2)?!;
		}
	`)
	expectCannotFailCount(t, errs, 1)
}

func TestT1386NestedGoBlockOuterEscapeStillCounts(t *testing.T) {
	// The outer body's own `?^` is outside the inner body's window, so rewinding
	// the counter for the inner body must not swallow it.
	errs := checkErrs(t, `
		foo!(int n) int { if n < 0 { raise error(message: "x"); } return n; }
		main!() {
			t := go! {
				t2 := go! { int b = foo(1)?^; b };
				int a = foo(2)?^;
				a
			};
			v := (<-t)?!;
		}
	`)
	expectNoErrors(t, errs)
}

func TestT1386NestedGoBlockReceiveIsAnEscapeForOuter(t *testing.T) {
	// The complement of the rewind: consuming the inner task with `<-` IS an
	// escape for the outer body (the receive is an ordinary failable call, §17.2.1),
	// so neither body is rejected. Guards against the rewind over-reaching and
	// swallowing an escape recorded *after* the inner block closed.
	errs := checkErrs(t, `
		foo!(int n) int { if n < 0 { raise error(message: "x"); } return n; }
		main!() {
			t := go! {
				t2 := go! { int b = foo(1)?^; b };
				int a = <-t2;
				a
			};
			v := (<-t)?!;
		}
	`)
	expectNoErrors(t, errs)
}

func TestT1386GoBlockSpawningAFailableCallIsNotAnEscape(t *testing.T) {
	// The call form `go! foo()` spawns asynchronously — its error is delivered to
	// its own receiver, so the spawn is not the enclosing body's escape and the
	// outer body still cannot fail (§17.2.1). The inner call form has no body of
	// its own to score, so exactly one rejection.
	errs := checkErrs(t, `
		foo!(int n) int { if n < 0 { raise error(message: "x"); } return n; }
		main!() {
			t := go! { t2 := go! foo(1); 0 };
			v := (<-t)?!;
		}
	`)
	expectCannotFailCount(t, errs, 1)
}

func TestT1386GoBlockBindingAFailableFunctionIsNotAnEscape(t *testing.T) {
	// Failability is a property of *calling* a fallible producer, never of a
	// value (§7.1) — binding a reference to a failable function does not escape.
	errs := checkErrs(t, `
		foo!(int n) int { if n < 0 { raise error(message: "x"); } return n; }
		main!() {
			t := go! { f := foo; 0 };
			v := (<-t)?!;
		}
	`)
	expectError(t, errs, t1386CannotFail)
}

func TestT1386PlainGoInsideGoBlockUseCloseDoesNotCountForOuter(t *testing.T) {
	// A plain `go {}` nested in a `go! {}` is a non-failable scope, so its
	// use-close panics inside the inner goroutine — it is not an escape out of
	// the outer body, which therefore still cannot fail.
	errs := checkErrs(t, `
		type CloseError is error { int code; }
		type Res { int id; close!(~this) { raise CloseError(message: "cf", code: 1); } }
		main!() {
			t := go! { t2 := go { use r := Res(id: 1); r.id }; 0 };
			v := (<-t)?!;
		}
	`)
	expectError(t, errs, t1386CannotFail)
}

func TestT1386PlainGoInsideGoBlockForInDoesNotCountForOuter(t *testing.T) {
	errs := checkErrs(t, `
		gen!(bool f) stream[int] { if f { raise error(message: "x"); } yield 1; }
		main!() {
			t := go! { t2 := go { int q = 0; for x in gen(true) { q = q + x; } q }; 0 };
			v := (<-t)?!;
		}
	`)
	expectError(t, errs, t1386CannotFail)
}

// --- unwrapErrorOperator's diagnostic call sites ---
//
// The helper was renamed from unwrapGoOperator because the for-in escape check
// now uses it too. Its original callers are the two "you put the error operator
// on the spawn" diagnostics, whose plain-`go` arm and whose complex-callee
// fallbacks were never exercised.

func TestT1386PlainGoWithErrorOperatorOperandRejected(t *testing.T) {
	// The non-`!` arm: a plain `go` cannot carry an error operator at all.
	errs := checkErrs(t, `
		foo!(int n) int { if n < 0 { raise error(message: "x"); } return n; }
		main!() {
			t := go foo(1)?!;
			v := <-t;
		}
	`)
	expectError(t, errs, "the operand of `go` must be a function or method call, or a block")
}

func TestT1386GoBangWithErrorOperatorOnComplexCalleeRejected(t *testing.T) {
	// goCalleeName has no name to report for a callee that is neither an ident
	// nor a member, so the fix-it falls back to "this call".
	errs := checkErrs(t, `
		main!() {
			t := go! (|| -> 1)()?!;
			v := (<-t)?!;
		}
	`)
	expectError(t, errs, "apply the error operator to the receive, not the spawn")
	expectError(t, errs, "this call")
}

func TestT1386GoBangWithErrorOperatorOnNonCallRejected(t *testing.T) {
	// The operand is not a call at all, so goCalleeName's first guard fires.
	errs := checkErrs(t, `
		main!() {
			int x = 1;
			t := go! x?!;
			v := (<-t)?!;
		}
	`)
	expectError(t, errs, "apply the error operator to the receive, not the spawn")
	expectError(t, errs, "this call")
}

// --- lambda bodies are their own scope (T1379 sibling of T1217) ---

func TestT1386LambdaInGoBlockIsNotAFailableScope(t *testing.T) {
	// A lambda body is governed by its own (never failable) signature, so a
	// `raise` inside it is an error even inside a `go! {}` — and it must not
	// count as the goroutine body's escape. Mirrors the plain-`go {}` reset
	// (T1217); without the failableScope reset both checks were wrong.
	errs := checkErrs(t, `
		main!() {
			t := go! { f := || -> int { raise error(message: "x"); }; f() };
			v := (<-t)?!;
		}
	`)
	expectError(t, errs, "raise outside of failable function")
	expectError(t, errs, t1386CannotFail)
}

func TestT1386LambdaUseFailableCloseInGoBlockIsNotAnEscape(t *testing.T) {
	// The close error panics inside the (non-failable) lambda rather than
	// escaping into the task, so the goroutine body still cannot fail.
	errs := checkErrs(t, `
		type CloseError is error { int code; }
		type Res { int id; close!(~this) { raise CloseError(message: "cf", code: 1); } }
		main!() {
			t := go! { f := || -> int { use r := Res(id: 1); return r.id; }; f() };
			v := (<-t)?!;
		}
	`)
	expectError(t, errs, t1386CannotFail)
}

func TestT1386LambdaForInFailableGeneratorInGoBlockIsNotAnEscape(t *testing.T) {
	// Same for the for-in site: inside a (non-failable) lambda the generator's
	// mid-stream error panics rather than escaping into the task.
	errs := checkErrs(t, `
		gen!(bool f) stream[int] { if f { raise error(message: "x"); } yield 1; }
		main!() {
			t := go! { f := || -> int { int q = 0; for x in gen(true) { q = q + x; } return q; }; f() };
			v := (<-t)?!;
		}
	`)
	expectError(t, errs, t1386CannotFail)
}

func TestT1386LambdaExprBodyInGoBlockIsNotAFailableScope(t *testing.T) {
	// The expression-body arm of checkLambdaExpr (`|| -> expr`, no braces) shares
	// the same failableScope reset as the block-body arm above but runs through a
	// separate branch — a `?^` there is still outside a failable function, and
	// still not the goroutine body's escape.
	errs := checkErrs(t, `
		foo!(int n) int { if n < 0 { raise error(message: "x"); } return n; }
		main!() {
			t := go! { f := || -> foo(1)?^; f() };
			v := (<-t)?!;
		}
	`)
	expectError(t, errs, "error propagation (?^) used outside of failable function")
	expectError(t, errs, t1386CannotFail)
}

// --- a plain `go {}` body still rejects every rerouted escape site ---
//
// The operator/handler sites used to test canPropagateError() directly and now
// go through recordFailableEscape, whose false return must keep emitting the
// same diagnostic. A plain `go {}` body is the interesting context: the
// enclosing function may well be failable, so only the T1217 non-failable scope
// makes the escape illegal.

func TestT1386FailableBinaryOperatorInPlainGoBlockRejected(t *testing.T) {
	errs := checkErrs(t, checkedVec+`
		main!() {
			t := go { a := CheckedVec(x: 10); c := a + CheckedVec(x: 5); c.x };
			v := <-t;
		}
	`)
	expectError(t, errs, "failable operator must be in a failable function")
}

func TestT1386FailableIncDecOperatorInPlainGoBlockRejected(t *testing.T) {
	errs := checkErrs(t, checkedVec+`
		main!() {
			t := go { CheckedVec a = CheckedVec(x: 10); a++; a.x };
			v := <-t;
		}
	`)
	expectError(t, errs, "failable operator must be in a failable function")
}

func TestT1386FailableCompoundAssignInPlainGoBlockRejected(t *testing.T) {
	errs := checkErrs(t, checkedVec+`
		main!() {
			t := go { CheckedVec a = CheckedVec(x: 10); a += CheckedVec(x: 5); a.x };
			v := <-t;
		}
	`)
	expectError(t, errs, "failable operator in compound assignment must be in a failable function")
}

func TestT1386FailableSetterAssignmentInPlainGoBlockRejected(t *testing.T) {
	errs := checkErrs(t, `
		type Counter {
			int _n;
			get value int { return this._n; }
			set value!(int v) { if v < 0 { raise error("neg"); } this._n = v; }
		}
		main!() {
			t := go { c := Counter(_n: 0); c.value = 5; c._n };
			v := <-t;
		}
	`)
	expectError(t, errs, "failable setter assignment must be in a failable function")
}

func TestT1386FailableIndexGetterIncDecInPlainGoBlockRejected(t *testing.T) {
	errs := checkErrs(t, `
		type Box {
			int x;
			[]!(int k) int { if this.x < 0 { raise error("neg"); } return this.x; }
			[]=(int k, int v) { this.x = v; }
		}
		main!() {
			t := go { b := Box(x: 1); b[0]++; b.x };
			v := <-t;
		}
	`)
	expectError(t, errs, "failable index read in inc/dec must be in a failable function")
}

func TestT1386TypedHandlerNoMatchInPlainGoBlockRejected(t *testing.T) {
	errs := checkErrs(t, `
		type IoError is error { int code; }
		foo!() int { raise IoError(message: "fail", code: 1); }
		main!() {
			t := go { int v = foo() ? e is IoError { 0 }; v };
			v := <-t;
		}
	`)
	expectError(t, errs, "typed error handler in non-failable function")
}

// --- the new sites combined with the nested-go! rewind ---

func TestT1386NestedGoBlockUseCloseCountsOnlyForInner(t *testing.T) {
	// The inner body's use-close escape belongs to the inner task. Exactly one
	// rejection — the outer body, which has no escape of its own.
	errs := checkErrs(t, `
		type CloseError is error { int code; }
		type Res { int id; close!(~this) { raise CloseError(message: "cf", code: 1); } }
		main!() {
			t := go! { t2 := go! { use r := Res(id: 1); r.id }; 0 };
			v := (<-t)?!;
		}
	`)
	expectCannotFailCount(t, errs, 1)
}

func TestT1386NestedGoBlockForInGeneratorCountsOnlyForInner(t *testing.T) {
	errs := checkErrs(t, `
		gen!(bool f) stream[int] { if f { raise error(message: "x"); } yield 1; }
		main!() {
			t := go! { t2 := go! { int q = 0; for x in gen(true) { q = q + x; } q }; 0 };
			v := (<-t)?!;
		}
	`)
	expectCannotFailCount(t, errs, 1)
}

func TestT1386NestedGoBlockBothUseCloseAccepted(t *testing.T) {
	// Both bodies have their own use-close escape: the inner body's rewind must
	// not roll back the outer body's, so neither is rejected.
	errs := checkErrs(t, `
		type CloseError is error { int code; }
		type Res { int id; close!(~this) { raise CloseError(message: "cf", code: 1); } }
		main!() {
			t := go! {
				t2 := go! { use a := Res(id: 1); a.id };
				use b := Res(id: 2);
				b.id
			};
			v := (<-t)?!;
		}
	`)
	expectNoErrors(t, errs)
}

// --- the discarded (fire-and-forget) `go! { }` statement ---
//
// checkExprStmtFailable rejects a discarded `go!` whose *type* is a failable
// task, naming the callee via unwrapGoExprCall. Widening body-can-fail detection
// changed which of the two diagnostics these bodies get: a discarded
// `go! { use r := Res(); }` used to be "cannot fail", and is now correctly the
// fire-and-forget rejection. The block form is also unwrapGoExprCall's nil arm —
// the only one its callers never reached.

func TestT1386DiscardedGoBlockUseFailableCloseIsFireAndForget(t *testing.T) {
	// The use-close escape makes the task failable, so discarding it would
	// swallow the close error. The "cannot fail" guard must NOT fire here.
	errs := checkErrs(t, `
		type CloseError is error { int code; }
		type Res { int id; close!(~this) { raise CloseError(message: "cf", code: 1); } }
		main!() {
			go! { use r := Res(id: 1); r.id };
		}
	`)
	expectError(t, errs, "a fire-and-forget goroutine must be non-failable")
	expectCannotFailCount(t, errs, 0)
}

func TestT1386DiscardedGoBlockForInFailableGeneratorIsFireAndForget(t *testing.T) {
	// Same reclassification for the for-in escape site.
	errs := checkErrs(t, `
		gen!(bool f) stream[int] { if f { raise error(message: "x"); } yield 1; }
		main!() {
			go! { int s = 0; for x in gen(true) { s = s + x; } s };
		}
	`)
	expectError(t, errs, "a fire-and-forget goroutine must be non-failable")
	expectCannotFailCount(t, errs, 0)
}

func TestT1386DiscardedGoBlockThatCannotFailGetsBothDiagnostics(t *testing.T) {
	// The reject twin: a body with no escape at all is still scored "cannot
	// fail", and — because its type is a failable task regardless — is also
	// rejected as fire-and-forget. The two checks are independent, so widening
	// the escape sites must not collapse one into the other.
	errs := checkErrs(t, `
		main!() {
			go! { 1 };
		}
	`)
	expectCannotFailCount(t, errs, 1)
	expectError(t, errs, "a fire-and-forget goroutine must be non-failable")
}

func TestT1386DiscardedGoBlockHintFallsBackToThisCall(t *testing.T) {
	// The block form has no callee, so unwrapGoExprCall returns nil and
	// goCalleeName falls back to "this call" — unwrapGoExprCall's nil arm, which
	// no other test reaches. (The resulting hint text reads as the nonsensical
	// `go { this call(...)?!; }` — T1448.)
	errs := checkErrs(t, `
		gen!(bool f) stream[int] { if f { raise error(message: "x"); } yield 1; }
		main!() {
			go! { int s = 0; for x in gen(true) { s = s + x; } s };
		}
	`)
	expectError(t, errs, "`go { this call(...)?!; }`")
}

func TestT1386DiscardedGoBangCallHintNamesItsCallee(t *testing.T) {
	// Contrast with the block form above: the call form takes unwrapGoExprCall's
	// non-nil arm, so the hint names the callee and reads correctly.
	errs := checkErrs(t, `
		foo!(int n) int { if n < 0 { raise error(message: "x"); } return n; }
		main!() {
			go! foo(1);
		}
	`)
	expectError(t, errs, "a fire-and-forget goroutine must be non-failable")
	expectError(t, errs, "`go { foo(...)?!; }`")
}
