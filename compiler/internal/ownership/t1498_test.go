package ownership

import (
	"strings"
	"testing"

	"github.com/promise-language/promise/compiler/internal/ast"
	"github.com/promise-language/promise/compiler/internal/types"
)

// T1498: a `move` of a binding declared OUTSIDE a loop, performed inside the
// loop body, was accepted with no diagnostic. Ownership walks each loop body
// exactly once from the pre-loop state, so the back edge — which re-reaches the
// move site with the variable already moved — was never checked. Codegen then
// emitted a second consume of a freed value: "invalid free (bad header magic)"
// for a plain move, or an unbounded allocation (MEMLIMIT) for a moved closure
// capture reading a freed string header.
//
// The straight-line form (`take(move s); take(move s);`) was already a hard
// error, so these tests lock in that the loop back edge counts as a re-reaching
// use too.

const t1498Sink = `
	take(string move s) int { return s.len; }
	consume(() -> string f) int { return f().len; }
`

// === Rejected: the loop back edge re-reaches the move ===

// Repro A from the item: a `move` to a `move` param inside a `while`.
func TestT1498PlainMoveInWhileLoop(t *testing.T) {
	errs := ownerErrs(t, t1498Sink+`
		f() {
			string v = "-" + "n";
			int total = 0;
			int i = 0;
			while (i < 3) {
				total += take(move v);
				i += 1;
			}
		}
	`)
	expectOwnerError(t, errs, "use of moved variable 'v': it is moved on every iteration of this loop")
}

// Repro B from the item: a `move` capture into a closure inside a `while`.
func TestT1498MoveCaptureInWhileLoop(t *testing.T) {
	errs := ownerErrs(t, t1498Sink+`
		f() {
			string suffix = "-" + "n";
			int total = 0;
			int i = 0;
			while (i < 3) {
				total += consume(move || -> suffix);
				i += 1;
			}
		}
	`)
	expectOwnerError(t, errs, "use of moved variable 'suffix': it is moved on every iteration of this loop")
}

// Classic `for` header form.
func TestT1498PlainMoveInClassicForLoop(t *testing.T) {
	errs := ownerErrs(t, t1498Sink+`
		f() {
			string v = "-" + "n";
			for int i = 0; i < 3; i += 1 {
				take(move v);
			}
		}
	`)
	expectOwnerError(t, errs, "use of moved variable 'v': it is moved on every iteration of this loop")
}

// for-in form: the moved name is an outer local, not the loop binding.
func TestT1498PlainMoveInForInLoop(t *testing.T) {
	errs := ownerErrs(t, t1498Sink+`
		f() {
			string v = "-" + "n";
			for x in 0..3 {
				take(move v);
			}
		}
	`)
	expectOwnerError(t, errs, "use of moved variable 'v': it is moved on every iteration of this loop")
}

// Infinite `for { … }` with a conditional break: the move still re-reaches the
// back edge on the non-breaking path.
func TestT1498PlainMoveInInfiniteLoop(t *testing.T) {
	errs := ownerErrs(t, t1498Sink+`
		f() {
			string v = "-" + "n";
			int i = 0;
			for {
				take(move v);
				i += 1;
				if i > 3 { break; }
			}
		}
	`)
	expectOwnerError(t, errs, "use of moved variable 'v': it is moved on every iteration of this loop")
}

// while-let form: the moved name is an outer local, not the unwrap binding.
func TestT1498PlainMoveInWhileLetLoop(t *testing.T) {
	errs := ownerErrs(t, t1498Sink+`
		f() {
			string v = "-" + "n";
			int? o = 5;
			while r := o {
				take(move v);
				o = none;
			}
		}
	`)
	expectOwnerError(t, errs, "use of moved variable 'v': it is moved on every iteration of this loop")
}

// A move inside a nested loop lands on BOTH frames, and is reported once — the
// innermost loop reports first and the name@pos dedup silences the outer one.
func TestT1498NestedLoopReportedOnce(t *testing.T) {
	errs := ownerErrs(t, t1498Sink+`
		f() {
			string v = "-" + "n";
			for a in 0..2 {
				for b in 0..2 {
					take(move v);
				}
			}
		}
	`)
	expectOwnerError(t, errs, "use of moved variable 'v': it is moved on every iteration of this loop")
	n := 0
	for _, e := range errs {
		if strings.Contains(e.Error(), "moved on every iteration") {
			n++
		}
	}
	if n != 1 {
		t.Errorf("expected exactly 1 loop-carried-move error, got %d: %v", n, errs)
	}
}

// Draining an outer-declared vector with a for-in on every iteration of an outer
// loop consumes it repeatedly.
func TestT1498ForInDrainOfOuterVector(t *testing.T) {
	errs := ownerErrs(t, `
		f() {
			v := string[]();
			v.push("a" + "b");
			for i in 0..3 {
				for s in v { }
			}
		}
	`)
	expectOwnerError(t, errs, "use of moved variable 'v': it is moved on every iteration of this loop")
}

// A move inside an inner loop whose body always leaves on its first iteration
// still runs once per OUTER iteration, so the outer loop must report it — every
// enclosing frame records the site, not just the innermost one.
func TestT1498MoveInInnerLoopWithBreakReportedByOuter(t *testing.T) {
	errs := ownerErrs(t, t1498Sink+`
		f() {
			string v = "-" + "n";
			for a in 0..2 {
				for b in 0..2 {
					take(move v);
					break;
				}
			}
		}
	`)
	expectOwnerError(t, errs, "use of moved variable 'v': it is moved on every iteration of this loop")
}

// A `continue` after the move reaches the back edge directly, so a trailing
// `break` does not shield it.
func TestT1498ContinueAfterMoveStillReported(t *testing.T) {
	errs := ownerErrs(t, t1498Sink+`
		f(bool a) {
			string v = "-" + "n";
			for {
				take(move v);
				if a { continue; }
				break;
			}
		}
	`)
	expectOwnerError(t, errs, "use of moved variable 'v': it is moved on every iteration of this loop")
}

// === Accepted: ownership is re-established on every iteration ===

// The correct form from the item: declare the value inside the body.
func TestT1498LoopLocalDeclOK(t *testing.T) {
	ownerOK(t, t1498Sink+`
		f() {
			int i = 0;
			while (i < 3) {
				string v = "-" + "n";
				take(move v);
				i += 1;
			}
		}
	`)
}

// Move-and-reassign accumulator: the name is re-owned before the back edge.
func TestT1498MoveThenReassignOK(t *testing.T) {
	ownerOK(t, `
		grow(string move a, string b) string { return a + b; }
		f() {
			string acc = "" + "";
			for i in 0..3 {
				acc = grow(move acc, "x");
			}
		}
	`)
}

// The for-in binding itself is fresh per iteration, so moving it into a sink is
// legal — the `skip` list must exempt it.
func TestT1498ForInBindingMovedIntoSinkOK(t *testing.T) {
	ownerOK(t, t1498Sink+`
		f() {
			out := string[]();
			for s in ["a" + "b", "c" + "d"] {
				out.push(move s);
			}
		}
	`)
}

// `Map.[]=` shape: the move sits on a `return` arm inside `for { match … }`, so
// the body's fall-through path merges back to Owned and the back edge never sees
// a moved value. The existing divergence machinery already handles this.
func TestT1498MoveOnReturnArmOK(t *testing.T) {
	ownerOK(t, t1498Sink+`
		f(string move key, bool flag) {
			for i in 0..3 {
				if flag {
					take(move key);
					return;
				}
			}
		}
	`)
}

// The conditional-exit form: every path from the move leaves the loop, so the
// back edge never re-reaches it. This is the shape a whole-body "does the tail
// break?" predicate would wrongly reject.
func TestT1498MoveOnConditionalBreakArmOK(t *testing.T) {
	ownerOK(t, t1498Sink+`
		f(bool done) {
			string v = "-" + "n";
			int i = 0;
			while (i < 3) {
				if done {
					take(move v);
					break;
				}
				i += 1;
			}
		}
	`)
}

// Nested one level deeper, with the inner `if` itself ending the outer `if`.
func TestT1498MoveOnNestedConditionalReturnArmOK(t *testing.T) {
	ownerOK(t, t1498Sink+`
		f(bool a, bool b) {
			string v = "-" + "n";
			for i in 0..3 {
				if a {
					if b {
						take(move v);
						return;
					}
					break;
				}
			}
		}
	`)
}

// A match arm cannot `break`/`return` (the grammar rejects control flow inside an
// arm block), so an arm-carried move always falls through to the back edge.
func TestT1498MoveOnFallingThroughMatchArmReported(t *testing.T) {
	errs := ownerErrs(t, t1498Sink+`
		f(int k) {
			string v = "-" + "n";
			int i = 0;
			while (i < 3) {
				match k {
					0 => { take(move v); }
					_ => { }
				}
				i += 1;
			}
		}
	`)
	expectOwnerError(t, errs, "use of moved variable 'v': it is moved on every iteration of this loop")
}

// An inner loop that always leaves the FUNCTION shields the outer loop too — the
// outer body's end-state is restored to the pre-loop state by mergeLoopState.
func TestT1498MoveInInnerLoopWithReturnOK(t *testing.T) {
	ownerOK(t, t1498Sink+`
		f() {
			string v = "-" + "n";
			for a in 0..2 {
				for b in 0..2 {
					take(move v);
					return;
				}
			}
		}
	`)
}

// A body whose tail is a `break` runs at most once.
func TestT1498BodyEndingInBreakOK(t *testing.T) {
	ownerOK(t, t1498Sink+`
		f() {
			string v = "-" + "n";
			for i in 0..3 {
				take(move v);
				break;
			}
		}
	`)
}

// A body whose tail returns leaves the function on the first iteration.
func TestT1498BodyEndingInReturnOK(t *testing.T) {
	ownerOK(t, t1498Sink+`
		f() {
			string v = "-" + "n";
			for i in 0..3 {
				take(move v);
				return;
			}
		}
	`)
}

// A "move" of a Channel is a refcount bump in codegen, not an ownership
// transfer, so re-performing it per iteration is sound. This is the
// `for v in ch` inside a `go {}` block nested in an outer loop shape used across
// tests/concurrency. (Draining a channel does leave it Moved for POST-loop code —
// a pre-existing rule this test deliberately does not disturb — so it asserts
// only that the loop-carried-move diagnostic stays silent.)
func TestT1498ChannelIterInGoBlockInLoopOK(t *testing.T) {
	errs := ownerErrs(t, `
		f() {
			ch := channel[int](4);
			for i in 0..2 {
				go {
					for v in ch { }
				};
			}
		}
	`)
	expectNoOwnerError(t, errs, "moved on every iteration")
}

// === Move sites outside the body's statement list ===

// A move in the classic-`for` CONDITION re-runs on every iteration just like a
// body move. Its position is in the header, so no body statement contains it —
// containingStmtIndex returns -1 and the site is (correctly) assumed to reach the
// back edge. Contrast TestT1498MoveInWhileConditionNotYetReported below: the
// classic-for snapshot is taken before the header, the plain-`while` one is not.
func TestT1498MoveInClassicForConditionReported(t *testing.T) {
	errs := ownerErrs(t, t1498Sink+`
		f() {
			string v = "-" + "n";
			for int i = 0; take(move v) > 0; i += 1 {
				i += 0;
			}
		}
	`)
	expectOwnerError(t, errs, "use of moved variable 'v': it is moved on every iteration of this loop")
}

// A move in an `if` CONDITION: the if is the containing statement, but neither
// arm holds the position, so the "does every path from here leave the loop?" walk
// has nothing to descend into and reports. The condition is re-evaluated every
// iteration regardless of which arm runs, so reporting is right.
func TestT1498MoveInIfConditionReported(t *testing.T) {
	errs := ownerErrs(t, t1498Sink+`
		f() {
			string v = "-" + "n";
			int i = 0;
			while (i < 3) {
				if take(move v) > 0 { i += 1; }
				i += 1;
			}
		}
	`)
	expectOwnerError(t, errs, "use of moved variable 'v': it is moved on every iteration of this loop")
}

// A move inside a string interpolation: the interpolated expression carries a
// renumbered span that no enclosing statement contains, so the reachability walk
// falls back to "assume it reaches the back edge" — the safe direction.
func TestT1498MoveInStringInterpolationReported(t *testing.T) {
	errs := ownerErrs(t, t1498Sink+`
		f() {
			string v = "-" + "n";
			int i = 0;
			while (i < 3) {
				string s = "x{take(move v)}";
				i += 1;
			}
		}
	`)
	expectOwnerError(t, errs, "use of moved variable 'v': it is moved on every iteration of this loop")
}

// === Move sites the item's "Related" section named ===

// T1148's shape — `go f(move x)` of a binding declared outside the loop. That fix
// was scoped to the `go` call path; the loop-carried check now covers the same
// shape from the other side, and the move-site walk does not descend into the
// `go` body (its `break`/`return` would not leave the enclosing loop anyway).
func TestT1498MoveInGoBlockInLoopReported(t *testing.T) {
	errs := ownerErrs(t, t1498Sink+`
		f() {
			string v = "-" + "n";
			for i in 0..3 {
				go {
					take(move v);
				};
			}
		}
	`)
	expectOwnerError(t, errs, "use of moved variable 'v': it is moved on every iteration of this loop")
}

// The shape tests/e2e/tuples_test.pr's T0399 test used to spell out: repeatedly
// pushing one owned local into a vector. Pinned here so the reason that test had
// to switch to `Vector.filled` stays visible (see T1534).
func TestT1498VectorPushOfOuterLocalReported(t *testing.T) {
	errs := ownerErrs(t, `
		f() {
			out := string[]();
			s := "a" + "b";
			for i in 0..3 {
				out.push(move s);
			}
		}
	`)
	expectOwnerError(t, errs, "use of moved variable 's': it is moved on every iteration of this loop")
}

// A droppable user type, not just a string — the check keys off ownership state,
// not off any string-specific handling.
func TestT1498UserTypeMoveInLoopReported(t *testing.T) {
	errs := ownerErrs(t, `
		type R { int id; drop(~this) {} }
		eat(R move r) int { return r.id; }
		f() {
			r := R(id: 1);
			for i in 0..3 {
				eat(move r);
			}
		}
	`)
	expectOwnerError(t, errs, "use of moved variable 'r': it is moved on every iteration of this loop")
}

// === Reassignment: only an unconditional one re-owns the binding ===

// Reassigning on ONLY the branch that did not move leaves the other path moved at
// the back edge, so this must still be rejected. (Compare
// TestT1498MoveThenReassignOK, where the reassignment dominates the back edge.)
func TestT1498ConditionalReassignAfterMoveReported(t *testing.T) {
	errs := ownerErrs(t, t1498Sink+`
		f(bool c) {
			string v = "-" + "n";
			for i in 0..3 {
				take(move v);
				if c {
					v = "x" + "y";
				}
			}
		}
	`)
	expectOwnerError(t, errs, "use of moved variable 'v': it is moved on every iteration of this loop")
}

// The mirror image: a CONDITIONAL move followed by an UNCONDITIONAL reassignment
// re-owns the binding on every path, so the back edge always sees an owned value.
func TestT1498ConditionalMoveThenUnconditionalReassignOK(t *testing.T) {
	ownerOK(t, t1498Sink+`
		f(bool c) {
			string v = "-" + "n";
			for i in 0..3 {
				if c {
					take(move v);
				}
				v = "x" + "y";
			}
		}
	`)
}

// === Bindings the loop re-establishes itself ===

// The while-let binding is unwrapped fresh from the optional on every iteration,
// so moving it into a sink is legal — the `skip` list must exempt it just as it
// does the for-in binding.
func TestT1498WhileLetBindingMovedIntoSinkOK(t *testing.T) {
	ownerOK(t, t1498Sink+`
		f() {
			string? o = "a" + "b";
			while r := o {
				take(move r);
				o = none;
			}
		}
	`)
}

// `this` is a borrowed receiver, not a tracked local, so a `move` capture of it
// inside a loop is not a loop-carried move. Also exercises the method-body entry
// path, which resets the per-body dedup map separately from checkFuncDecl.
func TestT1498MoveCaptureOfThisInLoopOK(t *testing.T) {
	ownerOK(t, `
		type Holder {
			int n;
			get val int { return this.n; }
			run(this) {
				for i in 0..3 {
					f := move || -> this.val;
					x := f();
				}
			}
		}
	`)
}

// A loop-carried move inside a METHOD body is reported the same as one in a free
// function — checkMethodBody initialises the dedup map on its own path.
func TestT1498MoveInMethodBodyReported(t *testing.T) {
	errs := ownerErrs(t, t1498Sink+`
		type Holder {
			int n;
			run(this) {
				string v = "-" + "n";
				for i in 0..3 {
					take(move v);
				}
			}
		}
	`)
	expectOwnerError(t, errs, "use of moved variable 'v': it is moved on every iteration of this loop")
}

// Returning an outer local from inside the loop leaves the function on that path,
// so the back edge never re-reaches the move.
func TestT1498ReturnOfOuterLocalInLoopOK(t *testing.T) {
	ownerOK(t, `
		f() string {
			string v = "-" + "n";
			for i in 0..3 {
				if i == 1 { return v; }
			}
			return "z" + "z";
		}
	`)
}

// The move sits in the ELSE arm, which breaks — every path from it leaves the
// loop, so the walk descends into the else branch and clears it.
func TestT1498MoveInElseArmThatBreaksOK(t *testing.T) {
	ownerOK(t, t1498Sink+`
		f(bool c) {
			string v = "-" + "n";
			int i = 0;
			while (i < 3) {
				if c { i += 1; } else { take(move v); break; }
			}
		}
	`)
}

// === Diagnostic shape ===

// Two loop-carried moves in one body are both reported, in name order — map
// iteration is randomized, so the sort is what makes the output reproducible.
func TestT1498TwoNamesReportedInStableOrder(t *testing.T) {
	errs := ownerErrs(t, t1498Sink+`
		f() {
			string b = "b" + "b";
			string a = "a" + "a";
			for i in 0..3 {
				take(move b);
				take(move a);
			}
		}
	`)
	var names []string
	for _, e := range errs {
		msg := e.Error()
		if !strings.Contains(msg, "moved on every iteration") {
			continue
		}
		switch {
		case strings.Contains(msg, "'a'"):
			names = append(names, "a")
		case strings.Contains(msg, "'b'"):
			names = append(names, "b")
		}
	}
	if len(names) != 2 || names[0] != "a" || names[1] != "b" {
		t.Errorf("expected loop-carried-move errors for 'a' then 'b', got %v (all: %v)", names, errs)
	}
}

// === Move sites other than a `move`-marked call argument ===
//
// noteLoopMoveSite is called from tryMove, tryMoveConsume and checkLambdaExpr,
// and each of those has many callers. The tests above all reach it through a
// `move`-marked call argument; the ones below cover the other statement and
// expression forms that consume a name, so a future refactor that reroutes one
// of them (say, binding a new tryMove* wrapper) cannot silently drop its move
// site.

// Binding an owned local to a new typed variable moves it (strings are non-Copy),
// so the second iteration binds a freed value. stmt.go's typed-var-decl path.
func TestT1498TypedVarDeclFromOuterLocalInLoopReported(t *testing.T) {
	errs := ownerErrs(t, `
		f() {
			string v = "-" + "n";
			for i in 0..3 {
				string w = v;
			}
		}
	`)
	expectOwnerError(t, errs, "use of moved variable 'v': it is moved on every iteration of this loop")
}

// Same through the inferred (`:=`) var-decl path, which reaches tryMove from a
// different branch than the typed one.
func TestT1498InferredVarDeclFromOuterLocalInLoopReported(t *testing.T) {
	errs := ownerErrs(t, `
		f() {
			v := "-" + "n";
			for i in 0..3 {
				w := v;
			}
		}
	`)
	expectOwnerError(t, errs, "use of moved variable 'v': it is moved on every iteration of this loop")
}

// Plain assignment to an existing binding — checkAssignStmt's move of the RHS.
func TestT1498AssignFromOuterLocalInLoopReported(t *testing.T) {
	errs := ownerErrs(t, `
		f() {
			string v = "-" + "n";
			string w = "x" + "y";
			for i in 0..3 {
				w = v;
			}
		}
	`)
	expectOwnerError(t, errs, "use of moved variable 'v': it is moved on every iteration of this loop")
}

// A bare owned local as a collection-literal element is consumed into the new
// container (T0338), so building the literal in a loop consumes it every
// iteration. Both the vector and the tuple form route through their own
// tryMoveConsume(elem) call in checkExpr.
func TestT1498CollectionLiteralElementFromOuterLocalInLoopReported(t *testing.T) {
	errs := ownerErrs(t, `
		f() {
			string v = "-" + "n";
			for i in 0..3 {
				a := [v];
			}
		}
	`)
	expectOwnerError(t, errs, "use of moved variable 'v': it is moved on every iteration of this loop")

	errs = ownerErrs(t, `
		f() {
			string v = "-" + "n";
			for i in 0..3 {
				p := (v, 1);
			}
		}
	`)
	expectOwnerError(t, errs, "use of moved variable 'v': it is moved on every iteration of this loop")
}

// `ch.send(move v)` consumes the SENT value. The refcountedShare carve-out keys
// off the type of the thing being moved, not off the channel, so sending an
// outer-declared string in a loop is still reported — the exemption must not
// leak from the receiver to the argument.
func TestT1498ChannelSendOfOuterStringInLoopReported(t *testing.T) {
	errs := ownerErrs(t, `
		f() {
			ch := channel[string](4);
			string v = "x" + "y";
			for i in 0..3 {
				ch.send(move v);
			}
		}
	`)
	expectOwnerError(t, errs, "use of moved variable 'v': it is moved on every iteration of this loop")
}

// `yield x` consumes the yielded value (YieldStmt → tryMoveConsume), so a
// generator that yields a `move` parameter from inside its loop hands the same
// owned value out on every iteration. This is the shape
// tests/e2e/generators_test.pr's `t1306_gen_of` used to spell out; pinned here so
// the reason that helper had to be restructured stays visible.
func TestT1498YieldOfOuterLocalInGeneratorLoopReported(t *testing.T) {
	errs := ownerErrs(t, `
		g(string move x, int n) stream[string] {
			int i = 0;
			while i < n {
				yield x;
				i = i + 1;
			}
		}
	`)
	expectOwnerError(t, errs, "use of moved variable 'x': it is moved on every iteration of this loop")
}

// The corrected generator shape: the yielded value is built inside the body, so
// each iteration yields its own.
func TestT1498YieldOfLoopLocalInGeneratorOK(t *testing.T) {
	ownerOK(t, `
		g(int n) stream[string] {
			int i = 0;
			while i < n {
				string v = "a" + "b";
				yield v;
				i = i + 1;
			}
		}
	`)
}

// `close()` on a MutexGuard is a consuming native method (T0846 —
// isConsumingNativeMethod routes the RECEIVER through tryMoveConsume), so
// closing a guard acquired outside the loop frees it on every iteration.
func TestT1498ConsumingNativeCloseOfOuterGuardReported(t *testing.T) {
	errs := ownerErrs(t, `
		f() {
			m := Mutex[int](0);
			g := m.lock();
			for i in 0..3 {
				g.close();
			}
		}
	`)
	expectOwnerError(t, errs, "use of moved variable 'g': it is moved on every iteration of this loop")
}

// The lock/close-per-iteration form real code writes: the guard is acquired
// inside the body, so each iteration owns and closes its own.
func TestT1498ConsumingNativeCloseOfLoopLocalGuardOK(t *testing.T) {
	ownerOK(t, `
		f() {
			m := Mutex[int](0);
			for i in 0..3 {
				g := m.lock();
				g.close();
			}
		}
	`)
}

// `raise` consumes its value and leaves the function, so a raise of an
// outer-declared local inside a loop never re-reaches the back edge —
// stmtAlwaysLeavesLoop falls through to stmtDiverges, which covers RaiseStmt.
func TestT1498RaiseOfOuterLocalInLoopOK(t *testing.T) {
	ownerOK(t, `
		type MyErr is error { }
		f!() {
			e := MyErr(message: "x" + "y");
			for i in 0..3 {
				if i == 1 { raise e; }
			}
		}
	`)
}

// A loop inside a LAMBDA body is checked with its own frames, so a
// loop-carried move of a lambda-local is reported just like one in a function
// body.
func TestT1498LoopInsideLambdaBodyReported(t *testing.T) {
	errs := ownerErrs(t, t1498Sink+`
		f() {
			g := || -> int {
				string v = "-" + "n";
				int n = 0;
				for i in 0..3 {
					n += take(move v);
				}
				return n;
			};
		}
	`)
	expectOwnerError(t, errs, "use of moved variable 'v': it is moved on every iteration of this loop")
}

// === Bindings destructured fresh on each iteration ===

// An `if is` enum destructure binds a fresh view of the payload each iteration
// (the enum itself stays intact — tests/e2e/loop_move_test.pr checks that at
// runtime), so moving the binding is not a loop-carried move of the enum.
func TestT1498EnumDestructureBindingMovedInLoopOK(t *testing.T) {
	ownerOK(t, t1498Sink+`
		enum E { A(string s), B }
		f(E move e) {
			for i in 0..3 {
				if e is A(s) { take(move s); }
			}
		}
	`)
}

// Same for a match-arm binding.
func TestT1498MatchArmBindingMovedInLoopOK(t *testing.T) {
	ownerOK(t, t1498Sink+`
		enum E { A(string s), B }
		f(E move e) {
			for i in 0..3 {
				match e {
					E.A(s) => { take(move s); },
					E.B => { },
				}
			}
		}
	`)
}

// === Interaction with the pre-existing straight-line diagnostic ===

// A `continue` BEFORE the move does not disqualify a trailing `break`: the
// reachability walk starts at the move's own statement, and the continue path
// never reaches it. Contrast TestT1498ContinueAfterMoveStillReported.
func TestT1498ContinueBeforeMoveDoesNotDisqualifyBreakOK(t *testing.T) {
	ownerOK(t, t1498Sink+`
		f(bool c) {
			string v = "-" + "n";
			for i in 0..3 {
				if c { continue; }
				take(move v);
				break;
			}
		}
	`)
}

// A move inside a bare nested block that breaks — movePathAlwaysLeavesLoop's
// *ast.Block descent, reached without an intervening `if`.
func TestT1498MoveInBareBlockThatBreaksOK(t *testing.T) {
	ownerOK(t, t1498Sink+`
		f() {
			string v = "-" + "n";
			for i in 0..3 {
				{
					take(move v);
					break;
				}
			}
		}
	`)
}

// Two loops moving the same name: the first gets the loop-carried diagnostic,
// the second sees the name already Moved at ITS entry state and falls through to
// the pre-existing plain "use of moved variable". Both fire — the new check
// layers on top of the old one rather than replacing it.
func TestT1498SecondLoopFallsBackToPlainMovedDiagnostic(t *testing.T) {
	errs := ownerErrs(t, t1498Sink+`
		f() {
			string v = "-" + "n";
			for i in 0..3 {
				take(move v);
			}
			for i in 0..3 {
				take(move v);
			}
		}
	`)
	expectOwnerError(t, errs, "use of moved variable 'v': it is moved on every iteration of this loop")
	loopCarried, plain := 0, 0
	for _, e := range errs {
		if strings.Contains(e.Error(), "moved on every iteration") {
			loopCarried++
		} else if strings.Contains(e.Error(), "use of moved variable 'v'") {
			plain++
		}
	}
	if loopCarried != 1 || plain != 1 {
		t.Errorf("expected 1 loop-carried + 1 plain moved error, got %d/%d: %v", loopCarried, plain, errs)
	}
}

// A use AFTER the loop still reports the plain diagnostic — mergeLoopState keeps
// the body's Moved end-state, which the new check reads but does not consume.
func TestT1498UseAfterLoopStillReportsPlainMoved(t *testing.T) {
	errs := ownerErrs(t, t1498Sink+`
		f() {
			string v = "-" + "n";
			for i in 0..3 {
				take(move v);
			}
			take(move v);
		}
	`)
	expectOwnerError(t, errs, "use of moved variable 'v': it is moved on every iteration of this loop")
	plain := 0
	for _, e := range errs {
		if !strings.Contains(e.Error(), "moved on every iteration") &&
			strings.Contains(e.Error(), "use of moved variable 'v'") {
			plain++
		}
	}
	if plain != 1 {
		t.Errorf("expected the post-loop use to still report plain use-after-move, got %d: %v", plain, errs)
	}
}

// === Known gaps, pinned so the fix flips a test rather than adding one ===

// T1536: a move in a plain `while` CONDITION is NOT reported — checkWhileStmt
// walks the condition before cloning the entry state, so the name already reads
// Moved at "entry". This still miscompiles (repro B's unbounded allocation), so
// this test pins the current wrong acceptance; flip it to expectOwnerError when
// T1536 lands. The classic-for header, whose snapshot IS taken first, is covered
// by TestT1498MoveInClassicForConditionReported.
func TestT1498MoveInWhileConditionNotYetReported(t *testing.T) {
	errs := ownerErrs(t, t1498Sink+`
		f() {
			string v = "-" + "n";
			int i = 0;
			while (take(move v) > 0 && i < 3) {
				i += 1;
			}
		}
	`)
	expectNoOwnerError(t, errs, "moved on every iteration")
}

// T1535: refcountedShare exempts EVERY Arc/Weak move, including an explicit
// `sink(move r)` into a consuming parameter — which does hand over the single
// refcount and so is genuinely loop-carried. Pinned for the same reason as
// T1536's test: closing T1535 should flip these to expectOwnerError.
func TestT1498ArcAndWeakConsumingMoveExemptKnownGap(t *testing.T) {
	errs := ownerErrs(t, `
		sink_ref(Ref[int] move r) int { return 1; }
		f() {
			r := Ref[int](5);
			for i in 0..3 {
				sink_ref(move r);
			}
		}
	`)
	expectNoOwnerError(t, errs, "moved on every iteration")

	errs = ownerErrs(t, `
		sink_weak(Weak[int] move w) int { return 1; }
		f() {
			r := Ref[int](5);
			w := r.downgrade();
			for i in 0..3 {
				sink_weak(move w);
			}
		}
	`)
	expectNoOwnerError(t, errs, "moved on every iteration")
}

// T1560: `<-t` frees the Task's G but ownership never marks the handle moved, so
// neither the straight-line double await nor the loop form is diagnosed — both
// die with `fatal: double free` at runtime. This is the loopmove.go header's
// first precision limit ("the mustUse `<-t` discharge is not hooked"), and the
// straight-line half shows the gap is wider than loops. Flip both to
// expectOwnerError when T1560 lands.
func TestT1498AwaitOfOuterTaskNotYetReported(t *testing.T) {
	errs := ownerErrs(t, `
		work() int { return 1; }
		f() {
			t := go work();
			for i in 0..3 {
				int r = <-t;
			}
		}
	`)
	expectNoOwnerError(t, errs, "moved on every iteration")

	errs = ownerErrs(t, `
		work() int { return 1; }
		f() {
			t := go work();
			int a = <-t;
			int b = <-t;
		}
	`)
	expectNoOwnerError(t, errs, "use of moved variable")
}

// T1561: the classic-`for` UPDATE clause is checked as a bare expression —
// checkClassicForStmt never sets its target back to Owned — so a reassignment
// there does not shield the move even though it runs before the back edge. The
// equivalent `while` form (reassign as the last body statement) is accepted and
// runs correctly, so this rejection is a false positive. Flip to ownerOK when
// T1561 lands.
func TestT1498ClassicForUpdateReassignFalsePositive(t *testing.T) {
	errs := ownerErrs(t, t1498Sink+`
		f() {
			string v = "-" + "n";
			for int i = 0; i < 3; v = "x" + "y" {
				take(move v);
			}
		}
	`)
	expectOwnerError(t, errs, "use of moved variable 'v': it is moved on every iteration of this loop")
}

// === Unit coverage for the defensive branches ===
//
// The guards below are unreachable from any source program the checker accepts
// (every reportLoopCarriedMoves call site sits inside enterLoopBody/exitLoopBody
// with a non-nil body, and no walk visits one move site twice), so they are
// exercised directly — same white-box style as t1349_test.go.

func TestT1498RefcountedShareClassification(t *testing.T) {
	cases := []struct {
		name string
		typ  types.Type
		want bool
	}{
		{"nil", nil, false},
		{"string", types.TypString, false},
		{"channel", types.NewInstance(types.TypChannel, []types.Type{types.TypInt}), true},
		{"arc", types.NewArc(types.TypInt), true},
		{"weak", types.NewWeak(types.TypInt), true},
	}
	for _, tc := range cases {
		if got := refcountedShare(tc.typ); got != tc.want {
			t.Errorf("refcountedShare(%s) = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestT1498PosWithinRejectsUnspannedAndForeignNodes(t *testing.T) {
	pos := ast.Pos{File: "test.pr", Line: 3, Column: 4}

	bare := &ast.BreakStmt{} // zero-valued span — a node built without positions
	if posWithin(bare, pos) {
		t.Error("unspanned node must not be reported as containing a position")
	}

	noEnd := &ast.BreakStmt{}
	noEnd.SetPosEnd(ast.Pos{File: "test.pr", Line: 1, Column: 0}, ast.Pos{})
	if posWithin(noEnd, pos) {
		t.Error("node with an invalid end must not be reported as containing a position")
	}

	foreign := &ast.BreakStmt{}
	foreign.SetPosEnd(
		ast.Pos{File: "other.pr", Line: 1, Column: 0},
		ast.Pos{File: "other.pr", Line: 9, Column: 0},
	)
	if posWithin(foreign, pos) {
		t.Error("node from another file must not be reported as containing a position")
	}
}

func TestT1498NoteLoopMoveSiteDedupsPerFrame(t *testing.T) {
	first := ast.Pos{File: "test.pr", Line: 3, Column: 4}
	second := ast.Pos{File: "test.pr", Line: 5, Column: 4}

	// Outside any loop there is no frame to record into — must be a silent no-op.
	c := &Checker{}
	c.noteLoopMoveSite("v", first, types.TypString)

	frame := &aliasLoopFrame{}
	c.loopFrames = []*aliasLoopFrame{frame}
	c.noteLoopMoveSite("", first, types.TypString)  // unnamed
	c.noteLoopMoveSite("_", first, types.TypString) // discard
	c.noteLoopMoveSite("v", first, types.TypString)
	c.noteLoopMoveSite("v", first, types.TypString) // same site again — dropped
	c.noteLoopMoveSite("v", second, types.TypString)

	if len(frame.moveSites) != 1 {
		t.Fatalf("expected sites for exactly one name, got %v", frame.moveSites)
	}
	if got := frame.moveSites["v"]; len(got) != 2 || got[0].pos != first || got[1].pos != second {
		t.Errorf("expected the two distinct sites once each, got %v", got)
	}
}

func TestT1498ReportLoopCarriedMovesGuards(t *testing.T) {
	body := &ast.Block{}
	body.SetPosEnd(
		ast.Pos{File: "test.pr", Line: 2, Column: 0},
		ast.Pos{File: "test.pr", Line: 8, Column: 0},
	)
	site := ast.Pos{File: "test.pr", Line: 4, Column: 6}
	frame := func() *aliasLoopFrame {
		return &aliasLoopFrame{moveSites: map[string][]loopMoveSite{
			"v": {{pos: site, typ: types.TypString}},
		}}
	}

	// No enclosing frame, and a nil body: both must return without reporting.
	c := &Checker{state: StateMap{"v": Moved}}
	c.reportLoopCarriedMoves(body, StateMap{"v": Owned})
	c.loopFrames = []*aliasLoopFrame{frame()}
	c.reportLoopCarriedMoves(nil, StateMap{"v": Owned})
	if len(c.errors) != 0 {
		t.Fatalf("guarded calls must not report, got %v", c.errors)
	}

	// With a frame and a body, the same site is reported once even though the
	// dedup map starts nil — it is lazily initialised on first use.
	c = &Checker{state: StateMap{"v": Moved}, loopFrames: []*aliasLoopFrame{frame()}}
	c.reportLoopCarriedMoves(body, StateMap{"v": Owned})
	c.reportLoopCarriedMoves(body, StateMap{"v": Owned})
	if len(c.errors) != 1 {
		t.Fatalf("expected exactly 1 error across two calls, got %v", c.errors)
	}
	if !strings.Contains(c.errors[0].Error(), "moved on every iteration of this loop") {
		t.Errorf("unexpected message: %v", c.errors[0])
	}

	// A name the loop re-establishes, `this`, `_`, and a name not owned at entry
	// are all skipped.
	for _, tc := range []struct {
		name  string
		entry StateMap
		skip  []string
	}{
		{"skipped binding", StateMap{"v": Owned}, []string{"v"}},
		{"not owned at entry", StateMap{"v": Borrowed}, nil},
		{"not declared before loop", StateMap{}, nil},
	} {
		c = &Checker{state: StateMap{"v": Moved}, loopFrames: []*aliasLoopFrame{frame()}}
		c.reportLoopCarriedMoves(body, tc.entry, tc.skip...)
		if len(c.errors) != 0 {
			t.Errorf("%s: expected no report, got %v", tc.name, c.errors)
		}
	}
}
