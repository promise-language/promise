package ownership

import "testing"

// T0846: MutexGuard.close(~this) frees the guard (its body is @MutexGuard.drop),
// so ownership must treat the call as a consume — any later use of the receiver
// is a use-after-free that must be rejected at compile time.

func TestT0846CloseThenBorrowIsMovedUse(t *testing.T) {
	errs := ownerErrs(t, `
		f() {
			m := Mutex[int](5);
			g := m.lock();
			g.close();
			x := g.borrow;
		}
	`)
	expectOwnerError(t, errs, "use of moved variable 'g'")
}

func TestT0846DoubleCloseIsMovedUse(t *testing.T) {
	errs := ownerErrs(t, `
		f() {
			m := Mutex[int](5);
			g := m.lock();
			g.close();
			g.close();
		}
	`)
	expectOwnerError(t, errs, "use of moved variable 'g'")
}

func TestT0846CloseOnBorrowedParamRejected(t *testing.T) {
	errs := ownerErrs(t, `
		consume(MutexGuard[int] g) {
			g.close();
		}
	`)
	expectOwnerError(t, errs, "cannot move borrowed parameter")
}

// Positive: close as the last use (no reuse) must still compile.
func TestT0846CloseLastUseOK(t *testing.T) {
	ownerOK(t, `
		f() {
			m := Mutex[int](5);
			x := g_read(m);
		}
		g_read(Mutex[int] m) int {
			g := m.lock();
			x := g.borrow;
			g.close();
			return x;
		}
	`)
}

// Positive: no explicit close — auto-drop at scope exit must compile.
func TestT0846AutoDropOK(t *testing.T) {
	ownerOK(t, `
		f() {
			m := Mutex[int](5);
			g := m.lock();
			x := g.borrow;
		}
	`)
}

// Guard specificity: the consume-on-close rule is scoped to MutexGuard.close.
// A user type with its own close(&this) method is an ordinary mutable-borrow
// call — calling it (even twice) must NOT mark the receiver Moved, so reuse
// stays legal. Pins that isConsumingNativeMethod does not over-consume.
func TestT0846UserCloseNotConsumed(t *testing.T) {
	ownerOK(t, `
		type Conn { int fd; close(&this) {} drop(~this){} }
		f() {
			c := Conn(fd: 1);
			c.close();
			c.close();
		}
	`)
}

// close() on a guard held in a struct field reuses the T0837 single-owner-handle
// rejection: when the owner is only borrowed, freeing the guard via close would
// double-free at the real owner's drop. member.Target is a MemberExpr here, so
// tryMoveConsume routes through rejectMemberHandleMoveOutOfBorrow.
func TestT0846CloseGuardFieldOutOfBorrowRejected(t *testing.T) {
	errs := ownerErrs(t, `
		type Holder { MutexGuard[int] g; drop(~this){} }
		f(Holder h) {
			h.g.close();
		}
	`)
	expectOwnerError(t, errs, "cannot move single-owner handle field 'g'")
}
