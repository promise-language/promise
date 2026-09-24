package ownership

import (
	"strings"
	"testing"
)

// T2011: a lambda `move` capture of a local that was ALREADY moved — by an
// earlier capture, a plain `move`, or one branch of an `if` — was accepted with
// no diagnostic. The capture site marked the name Moved but never looked at its
// prior state, and the lambda body resets every capture to Owned, so no ident
// read ever saw the stale value either. Both envs then owned (and freed) the
// same value: "invalid free (bad header magic)", or a MEMLIMIT runaway when the
// second env read a freed string header.
//
// The capture site also skipped two checks every other move performs (use-bound
// variables, live borrows) and, in the other direction, treated a Copy capture
// as a move — so an `int` read after a `move` lambda captured it was rejected.
// A move capture now goes through the same tail as tryMove (moveLocal).

const t2011Sink = `
	eat(string move s) int { return s.len; }
	use_it((int) -> string f) string { return f(0); }
`

// countOwnerErrors returns how many errors contain substr.
func countOwnerErrors(errs []error, substr string) int {
	n := 0
	for _, e := range errs {
		if strings.Contains(e.Error(), substr) {
			n++
		}
	}
	return n
}

// === Rejected: the captured local is already moved ===

// Repro 1 from the item: two bound lambdas capture the same string.
func TestT2011TwoLambdasMoveSameLocal(t *testing.T) {
	errs := ownerErrs(t, `
		test() {
			string suffix = "-" + "c";
			f := move |int k| -> "{k}{suffix}";
			g := move |int k| -> "{k}{suffix}";
		}
	`)
	expectOwnerError(t, errs, "use of moved variable 'suffix'")
}

// Repro 2 from the item: the same shape consumed inline as call arguments.
func TestT2011DoubleMoveCaptureInline(t *testing.T) {
	errs := ownerErrs(t, t2011Sink+`
		test() {
			string suffix = "-" + "c";
			string a = use_it(move |int k| -> "{k}{suffix}");
			string b = use_it(move |int k| -> "{k}{suffix}");
		}
	`)
	expectOwnerError(t, errs, "use of moved variable 'suffix'")
}

// A plain `move` first, then a capture.
func TestT2011MoveThenCapture(t *testing.T) {
	errs := ownerErrs(t, t2011Sink+`
		test() {
			string suffix = "-" + "c";
			int n = eat(move suffix);
			f := move |int k| -> "{k}{suffix}";
		}
	`)
	expectOwnerError(t, errs, "use of moved variable 'suffix'")
}

// A move on one branch merges to Moved, so a later capture is rejected exactly
// like a later read would be.
func TestT2011ConditionalMoveThenCapture(t *testing.T) {
	errs := ownerErrs(t, t2011Sink+`
		test(bool c) {
			string suffix = "-" + "c";
			if (c) {
				int n = eat(move suffix);
			}
			f := move |int k| -> "{k}{suffix}";
		}
	`)
	expectOwnerError(t, errs, "use of moved variable 'suffix'")
}

// An outer lambda forwards a capture to a nested one (the outer's capture set
// includes it), after an earlier lambda already took it.
func TestT2011NestedLambdaForwardsMovedCapture(t *testing.T) {
	errs := ownerErrs(t, `
		test() {
			string suffix = "-" + "c";
			f := move |int k| -> "{k}{suffix}";
			g := move |int k| -> string {
				h := move |int j| -> "{j}{suffix}";
				return h(k);
			};
		}
	`)
	expectOwnerError(t, errs, "use of moved variable 'suffix'")
}

// Inside one lambda body the capture is a fresh owned value; two inner lambdas
// capturing it is the same double move one frame down.
func TestT2011TwoInnerLambdasCaptureSameCapture(t *testing.T) {
	errs := ownerErrs(t, `
		test() {
			string suffix = "-" + "c";
			g := move |int k| -> string {
				h1 := move |int j| -> "{j}{suffix}";
				h2 := move |int j| -> "{j}{suffix}";
				return h1(k) + h2(k);
			};
		}
	`)
	expectOwnerError(t, errs, "use of moved variable 'suffix'")
}

// A lambda capture of a Channel transfers the handle into the env — it is not
// a refcount bump, so there is no refcountedShare carve-out here (capturing it
// twice segfaulted).
func TestT2011ChannelCapturedTwice(t *testing.T) {
	errs := ownerErrs(t, `
		test() {
			ch := channel[int](4);
			f := move |int k| -> ch.send(k);
			g := move |int k| -> ch.send(k);
		}
	`)
	expectOwnerError(t, errs, "use of moved variable 'ch'")
}

// Same for Ref[T].
func TestT2011RefCapturedTwice(t *testing.T) {
	errs := ownerErrs(t, `
		test() {
			Ref[string] r = Ref[string]("-" + "c");
			f := move |int k| -> "{k}{r.borrow}";
			g := move |int k| -> "{k}{r.borrow}";
		}
	`)
	expectOwnerError(t, errs, "use of moved variable 'r'")
}

// A captured-then-read local is still rejected at the read (unchanged).
func TestT2011CaptureThenReadRejected(t *testing.T) {
	errs := ownerErrs(t, `
		test() {
			string suffix = "-" + "c";
			f := move |int k| -> "{k}{suffix}";
			string t = suffix;
		}
	`)
	expectOwnerError(t, errs, "use of moved variable 'suffix'")
}

// === Rejected: the checks every other move already performed ===

// A use-bound variable needs its close() at scope exit; moving it into an env
// double-closed it (exit status 134).
func TestT2011UseBoundCaptureRejected(t *testing.T) {
	errs := ownerErrs(t, `
		type Res {
			int id;
			close() {}
		}
		test() {
			use r := Res(id: 1);
			f := move |int k| -> k + r.id;
		}
	`)
	expectOwnerError(t, errs, "cannot move use-bound variable 'r'")
}

// Moving a local into an env while a stored borrow of it is still live.
func TestT2011CaptureWhileBorrowedRejected(t *testing.T) {
	errs := ownerErrs(t, `
		get_ref(string s) string& { return s; }
		test() {
			string suffix = "-" + "c";
			string &r = get_ref(suffix);
			f := move |int k| -> "{k}{suffix}";
			string &r2 = r;
		}
	`)
	expectOwnerError(t, errs, "cannot move 'suffix' while it is borrowed")
}

// A borrowed `T&` local is Copy by type, so the Borrowed check must run before
// the Copy skip — otherwise this T0338 rejection would be lost.
func TestT2011BorrowedRefLocalCaptureStillRejected(t *testing.T) {
	errs := ownerErrs(t, `
		get_ref(string s) string& { return s; }
		test() {
			string suffix = "-" + "c";
			string &r = get_ref(suffix);
			f := move |int k| -> "{k}{r}";
		}
	`)
	expectOwnerError(t, errs, "cannot move-capture borrowed value 'r' into a lambda")
}

// === Accepted ===

// A Copy capture is a copy (language-design language-design.md#borrowing-and-moving): capturing it into two
// `move` lambdas and reading it afterwards are all fine.
func TestT2011CopyCapturedTwiceThenReadOK(t *testing.T) {
	ownerOK(t, `
		type P {
			int x `+"`"+`value;
			int y `+"`"+`value;
		}
		test() {
			int n = 5;
			P p = P(x: 1, y: 2);
			f := move |int k| -> k + n + p.x;
			g := move |int k| -> k + n + p.y;
			int total = f(1) + g(1) + n + p.x;
		}
	`)
}

// A Copy capture inside a loop is not a loop-carried move (T1498 used to flag
// it on every iteration, and again at the read after the loop).
func TestT2011CopyCaptureInLoopOK(t *testing.T) {
	ownerOK(t, t2011Sink+`
		test() {
			int n = 5;
			int i = 0;
			while (i < 2) {
				string s = use_it(move |int k| -> "{k}{n}");
				i += 1;
			}
			int m = n;
		}
	`)
}

// Reassigning the moved local makes it Owned again, so it can be captured anew.
func TestT2011ReassignThenRecaptureOK(t *testing.T) {
	ownerOK(t, `
		test() {
			string suffix = "-" + "c";
			f := move |int k| -> "{k}{suffix}";
			suffix = "-" + "d";
			g := move |int k| -> "{k}{suffix}";
		}
	`)
}

// A local declared inside the loop body is fresh each iteration.
func TestT2011CaptureOfLoopLocalOK(t *testing.T) {
	ownerOK(t, t2011Sink+`
		test() {
			int i = 0;
			while (i < 2) {
				string suffix = "-" + "c";
				string s = use_it(move |int k| -> "{k}{suffix}");
				i += 1;
			}
		}
	`)
}

// A `go` block that binds the local inside it takes ownership (T1641), so a
// later move capture is a second move of the same value.
func TestT2011GoBlockMoveThenCapture(t *testing.T) {
	errs := ownerErrs(t, `
		test() {
			string suffix = "-" + "c";
			t := go {
				string w = suffix;
				return w.len;
			};
			f := move |int k| -> "{k}{suffix}";
			int n = <-t;
		}
	`)
	expectOwnerError(t, errs, "use of moved variable 'suffix'")
}

// === moveLocal: tryMove checks Borrowed ahead of the use-bound pin ===

// Extracting moveLocal put tryMove's Borrowed early-return ahead of the pinned
// check (tryMoveConsume already had that order). The two can only coincide when
// a sibling scope reuses a `use`-bound name — sema rejects shadowing — and
// there the pin belongs to the other, dead binding: the borrowed closure alias
// must get the same verdict it gets with no `use` in the function at all,
// instead of "cannot move use-bound variable 'f'".
func TestT2011TryMoveBorrowedAheadOfSiblingPinOK(t *testing.T) {
	ownerOK(t, `
		type Res {
			int id;
			close() {}
		}
		type CbHolder { () -> int cb; }
		make_cb(int n) CbHolder { return CbHolder(cb: move || -> n); }
		test() {
			h := make_cb(5);
			if (true) {
				use f := Res(id: 1);
			}
			if (true) {
				f := h.cb;
				g := f;
				int r = g();
			}
		}
	`)
}

// === Loop form (T1498) is unchanged and reported once ===

func TestT2011LoopCaptureReportedOnce(t *testing.T) {
	errs := ownerErrs(t, t2011Sink+`
		test() {
			string suffix = "-" + "c";
			int i = 0;
			while (i < 2) {
				string s = use_it(move |int k| -> "{k}{suffix}");
				i += 1;
			}
		}
	`)
	if n := countOwnerErrors(errs, "'suffix'"); n != 1 {
		t.Fatalf("expected exactly 1 error about 'suffix', got %d: %v", n, errs)
	}
	expectOwnerError(t, errs, "use of moved variable 'suffix': it is moved on every iteration of this loop")
}
