package ownership

import (
	"strings"
	"testing"
)

// T1641 — `go {}` block capture of an owned droppable local transfers
// ownership (B0354 codegen) without marking it moved — outer use after
// spawn silently reads freed memory.

// The repro from the bug: a string captured into a go block, then used
// after the spawn. Must error.
func TestT1641StringCaptureUseAfterGoBlock(t *testing.T) {
	errs := ownerErrs(t, `
		test() {
			string s = "hello " + "x";
			t := go { print_line("in-go: {s}"); 1 };
			v := <-t;
			print_line("after: [{s}]");
		}
	`)
	expectOwnerError(t, errs, "use of moved variable 's'")
}

// A vector captured into a go block, then used after.
func TestT1641VectorCaptureUseAfterGoBlock(t *testing.T) {
	errs := ownerErrs(t, `
		test() {
			v := [1, 2, 3];
			t := go { print_line("{v.len}"); 1 };
			r := <-t;
			print_line("{v.len}");
		}
	`)
	expectOwnerError(t, errs, "use of moved variable 'v'")
}

// A heap user type with drop() captured into a go block, then used after.
func TestT1641HeapUserTypeCaptureUseAfterGoBlock(t *testing.T) {
	errs := ownerErrs(t, `
		type Resource {
			int id;
			drop(~this) {}
		}
		test() {
			r := Resource(id: 1);
			t := go { print_line("{r.id}"); 1 };
			x := <-t;
			print_line("{r.id}");
		}
	`)
	expectOwnerError(t, errs, "use of moved variable 'r'")
}

// Copy types (int, bool) are captured by copy — no move.
func TestT1641CopyTypeCaptureNotMoved(t *testing.T) {
	ownerOK(t, `
		test() {
			int n = 42;
			t := go { print_line("{n}"); 1 };
			v := <-t;
			print_line("{n}");
		}
	`)
}

// Channels are refcounted — shared via refcount bump, not moved.
func TestT1641ChannelCaptureNotMoved(t *testing.T) {
	ownerOK(t, `
		test() {
			done := channel[int](1);
			t := go { done.send(1); 1 };
			v := <-t;
			r := <-done;
		}
	`)
}

// A droppable capture used only inside the go block (no post-spawn use) is OK.
func TestT1641DroppableCaptureNoPostSpawnUseOK(t *testing.T) {
	ownerOK(t, `
		test() {
			s := "hello " + "x";
			t := go { print_line(s); 1 };
			v := <-t;
		}
	`)
}

// A droppable capture inside a loop body — second iteration captures an
// already-moved variable. Must be rejected by the loop-move machinery.
func TestT1641GoBlockInsideLoopCaptureRejected(t *testing.T) {
	errs := ownerErrs(t, `
		test() {
			s := "hello " + "x";
			for i in 0..2 {
				go { print_line(s); 1 };
			}
		}
	`)
	// The loop-move machinery should catch this.
	found := false
	for _, err := range errs {
		msg := err.Error()
		if strings.Contains(msg, "moved") || strings.Contains(msg, "move") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected move-related error for droppable capture in loop, got %v", errs)
	}
}

// An already-moved variable captured into a go block — the use-of-moved
// should fire on the in-block use, not cause a duplicate move marking.
func TestT1641AlreadyMovedCaptureInGoBlock(t *testing.T) {
	errs := ownerErrs(t, `
		test() {
			s := "hello " + "x";
			t1 := go { print_line(s); 1 };
			t2 := go { print_line(s); 1 };
		}
	`)
	expectOwnerError(t, errs, "use of moved variable 's'")
}

// Multiple droppable captures in the same go block — all should be moved.
func TestT1641MultipleDroppableCapturesMoved(t *testing.T) {
	errs := ownerErrs(t, `
		test() {
			s := "hello";
			v := [1, 2];
			t := go { print_line("{s} {v.len}"); 1 };
			r := <-t;
			print_line(s);
			print_line("{v.len}");
		}
	`)
	expectOwnerError(t, errs, "use of moved variable 's'")
	expectOwnerError(t, errs, "use of moved variable 'v'")
}

// `this` captured inside a go block in a method body should be skipped
// (not marked moved) — `this` is borrowed and has its own rules.
func TestT1641ThisCaptureInGoBlockSkipped(t *testing.T) {
	ownerOK(t, `
		type Widget {
			int id;
			run(this) int {
				t := go { print_line("{this.id}"); 1 };
				v := <-t;
				return this.id;
			}
		}
		test() {
			w := Widget(id: 5);
			w.run();
		}
	`)
}

// A borrowed droppable parameter captured into a go block is unsound
// (T1397), but the current fix deliberately skips it to avoid a confusing
// diagnostic. This test documents the current behaviour: no move error.
func TestT1641BorrowedDroppableParamCapturedInGoBlock(t *testing.T) {
	// Plain parameter `r` is Borrowed — checkGoDroppableCaptures skips it.
	// This is a known unsoundness (T1397); the test pins the current skip.
	ownerOK(t, `
		type Resource {
			int id;
			drop(~this) {}
		}
		worker(Resource r) int {
			t := go { print_line("{r.id}"); 1 };
			v := <-t;
			return r.id;
		}
		test() {
			r := Resource(id: 1);
			worker(r);
		}
	`)
}

// An owned (move) parameter that is droppable, captured into a go block,
// SHOULD be marked moved — the caller surrendered ownership.
func TestT1641OwnedParamCapturedInGoBlock(t *testing.T) {
	errs := ownerErrs(t, `
		type Resource {
			int id;
			drop(~this) {}
		}
		worker(Resource move r) int {
			t := go { print_line("{r.id}"); 1 };
			v := <-t;
			return r.id;
		}
		test() {
			r := Resource(id: 1);
			worker(move r);
		}
	`)
	expectOwnerError(t, errs, "use of moved variable 'r'")
}

// A closure (lambda) captured into a go block is handled by
// checkGoClosureCaptures, not checkGoDroppableCaptures — verify both
// coexist without double-move or missed-move for a mixed capture list.
func TestT1641ClosureAndDroppableMixedCapture(t *testing.T) {
	errs := ownerErrs(t, `
		test() {
			s := "hello " + "x";
			f := |int x| -> x + 1;
			t := go { print_line("{s} {f(1)}"); 1 };
			r := <-t;
			print_line(s);
		}
	`)
	expectOwnerError(t, errs, "use of moved variable 's'")
}

// A droppable capture in a conditional branch — only one path spawns.
// Post-branch use should still be rejected since the move is unconditional
// within the if-body and ownership is conservative.
func TestT1641DroppableCaptureInConditionalGoBlock(t *testing.T) {
	errs := ownerErrs(t, `
		test() {
			s := "hello " + "x";
			if true {
				t := go { print_line(s); 1 };
			}
			print_line(s);
		}
	`)
	expectOwnerError(t, errs, "use of moved variable 's'")
}
