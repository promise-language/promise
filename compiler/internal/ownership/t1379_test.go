package ownership

import "testing"

// T1379: `<-t` on a failable_task[T] is a consuming receive that delivers the
// result once. A single receive is sound and, like a plain task, the ownership
// pass leaves the borrowed-source rejection to rejectBorrowedTaskAwait (shared
// via the IsAnyTask gate). Full must-use linearity for failable_task — including
// rejecting a second receive as use-after-move — is a separate follow-up
// (tracked alongside T1379); the plain `task[T]` tracker does not reject a
// double receive today either, so failable_task deliberately matches it here.

func TestT1379_FailableTaskSingleReceiveOK(t *testing.T) {
	ownerOK(t, `
		produce!(int x) int { return x; }
		test() {
			t := go! produce(5);
			a := (<-t)?!;
		}
	`)
}

// A failable_task awaited out of a borrowed owner is rejected — the receive is
// consuming, so the real owner would double-free. Shares rejectBorrowedTaskAwait
// with plain task via the IsAnyTask gate.
func TestT1379_FailableTaskBorrowedAwaitRejected(t *testing.T) {
	errs := ownerErrs(t, `
		produce!(int x) int { return x; }
		consume(failable_task[int] tk) {
			a := (<-tk)?!;
		}
	`)
	expectOwnerError(t, errs, "<-")
}

// Moving a borrowed failable_task parameter into a fresh owned binding is
// rejected — the handle has no clone. This drives singleOwnerHandleKind's
// dedicated "failable_task" arm (distinct display name from a plain task), so
// the diagnostic vocabulary and the deep alias-safety gate treat it as a
// single-owner handle just like task[T].
func TestT1379_FailableTaskMoveBorrowedParamRejected(t *testing.T) {
	errs := ownerErrs(t, `
		consume(failable_task[int] tk) {
			failable_task[int] c = tk;
		}
	`)
	expectOwnerError(t, errs, "cannot move borrowed parameter 'tk'")
}
