package ownership

import (
	"strings"
	"testing"
)

// T1640 — closure env ownership across a `go` boundary.
//
// R4: capturing a closure into a goroutine MOVES it, so a post-spawn use is a
// compile error rather than a silent read of memory the coroutine now owns.
// R5: only a binding that provably owns its heap env may cross; a borrow of
// someone else's env is rejected with a pointer to `move`.

// R4 — using a captured closure after the spawn is use-after-move.
func TestT1640ClosureUseAfterSpawnIsMoved(t *testing.T) {
	errs := ownerErrs(t, `
		test() {
			done := channel[int](1);
			f := |int x| -> x + 1;
			go {
				done.send(f(1));
			};
			print_line("{f(2)}");
		}
	`)
	expectOwnerError(t, errs, "use of moved variable 'f'")
}

// R4 — a closure reaching the goroutine ONLY through a nested lambda is moved
// too. Codegen's collectBlockIdents captures it via LambdaCaptures, so
// checkGoBlockCaptures must record it; otherwise the post-spawn use is a silent
// use-after-free.
func TestT1640NestedLambdaClosureCaptureIsMoved(t *testing.T) {
	errs := ownerErrs(t, `
		test() {
			done := channel[int](1);
			f := |int x| -> x + 1;
			go {
				inner := move || -> f(1);
				done.send(inner());
			};
			print_line("{f(2)}");
		}
	`)
	expectOwnerError(t, errs, "use of moved variable 'f'")
}

// R5 — a non-`move` closure parameter is a borrow; the goroutine may outlive the
// call, so capturing it would use the caller's freed env.
func TestT1640BorrowedClosureParamCapturedByGoRejected(t *testing.T) {
	errs := ownerErrs(t, `
		spawn((int) -> int f, channel[int] done) {
			go {
				done.send(f(1));
			};
		}
	`)
	expectOwnerError(t, errs, "it is a borrowed parameter, not an owned value")
}

// R5 — a `move` closure parameter owns its env and may cross.
func TestT1640MoveClosureParamCapturedByGoAccepted(t *testing.T) {
	ownerOK(t, `
		spawn((int) -> int move f, channel[int] done) {
			go {
				done.send(f(1));
			};
		}
	`)
}

// R5 — a lambda-literal local owns a fresh env and may cross.
func TestT1640LambdaLocalCapturedByGoAccepted(t *testing.T) {
	ownerOK(t, `
		test() {
			done := channel[int](1);
			int base = 10;
			f := |int x| -> x + base;
			go {
				done.send(f(1));
			};
		}
	`)
}

// R5 — a named-function reference has a null env; nothing to free, so it crosses.
func TestT1640NamedFunctionRefCapturedByGoAccepted(t *testing.T) {
	ownerOK(t, `
		double(int x) int { return x * 2; }
		test() {
			done := channel[int](1);
			(int) -> int f = double;
			go {
				done.send(f(3));
			};
		}
	`)
}

// R5 — a closure read out of an owning aggregate (T0812 borrow) aliases the
// aggregate's env; the aggregate still frees it, so it must not cross.
func TestT1640AggregateReadClosureCapturedByGoRejected(t *testing.T) {
	errs := ownerErrs(t, `
		type H {
			(int) -> int cb;
		}
		test() {
			done := channel[int](1);
			h := H(cb: |int x| -> x + 1);
			f := h.cb;
			go {
				done.send(f(1));
			};
		}
	`)
	expectOwnerError(t, errs, "it borrows an environment owned elsewhere")
}

// R5 — a closure whose env borrows a local receiver (T1349 taint) must not cross:
// the enclosing scope still drops the receiver.
func TestT1640ThisBorrowingClosureCapturedByGoRejected(t *testing.T) {
	errs := ownerErrs(t, `
		type Counter {
			int n;
			make_getter(this) () -> int {
				return move || -> this.n;
			}
		}
		test() {
			done := channel[int](1);
			c := Counter(n: 42);
			f := c.make_getter();
			go {
				done.send(f());
			};
		}
	`)
	expectOwnerError(t, errs, "its environment borrows local 'c'")
}

// R5, call form — a closure passed in a borrow slot of a `go` call is rejected.
func TestT1640ClosureGoCallArgumentBorrowRejected(t *testing.T) {
	errs := ownerErrs(t, `
		send_it((int) -> int f, channel[int] done) {
			done.send(f(1));
		}
		test() {
			done := channel[int](1);
			f := |int x| -> x + 1;
			go send_it(f, done);
		}
	`)
	expectOwnerError(t, errs, "cannot pass a borrow of closure 'f'")
}

// R5, call form — `move` transfers the env into the goroutine and is accepted.
func TestT1640ClosureGoCallArgumentMoveAccepted(t *testing.T) {
	ownerOK(t, `
		send_it((int) -> int move f, channel[int] done) {
			done.send(f(1));
		}
		test() {
			done := channel[int](1);
			f := |int x| -> x + 1;
			go send_it(move f, done);
		}
	`)
}

// R5, call form — a TEMPORARY closure argument is rejected. The spawning scope
// frees the temp env at statement end: in a borrow slot the goroutine then reads
// freed memory, and in a consuming slot the callee frees it a second time
// ("fatal: invalid free"). `move` cannot rescue it — it applies only to a named
// binding — so the temp must be bound to a local. Lifting this is T1654.
func TestT1640InlineLambdaGoCallArgumentRejected(t *testing.T) {
	errs := ownerErrs(t, `
		send_it((int) -> int move f, channel[int] done) {
			done.send(f(1));
		}
		test() {
			done := channel[int](1);
			go send_it(move |int x| -> x + 1, done);
		}
	`)
	expectOwnerError(t, errs, "cannot pass a temporary closure across a 'go' boundary")
}

// R5, call form — a call-result temporary is rejected for the same reason, and in
// a CONSUMING slot too (where no "requires `move`" diagnostic would fire).
func TestT1640CallResultGoCallArgumentRejected(t *testing.T) {
	errs := ownerErrs(t, `
		make_cb(int base) (int) -> int {
			return move |int x| -> x + base;
		}
		send_it((int) -> int move f, channel[int] done) {
			done.send(f(1));
		}
		test() {
			done := channel[int](1);
			go send_it(make_cb(1000), done);
		}
	`)
	expectOwnerError(t, errs, "cannot pass a temporary closure across a 'go' boundary")
}

// R5, call form — a closure read out of an aggregate has no ident root either,
// but it aliases an env the aggregate still frees, so it gets the borrow wording.
func TestT1640AggregateReadGoCallArgumentRejected(t *testing.T) {
	errs := ownerErrs(t, `
		type H {
			(int) -> int cb;
		}
		send_it((int) -> int f, channel[int] done) {
			done.send(f(1));
		}
		test() {
			done := channel[int](1);
			h := H(cb: |int x| -> x + 1);
			go send_it(h.cb, done);
		}
	`)
	expectOwnerError(t, errs, "cannot pass a borrowed closure across a 'go' boundary")
}

// R4 + T1255 — capturing the same closure on every iteration moves it twice; the
// existing loop-move machinery catches it because R4 registers a move site.
func TestT1640ClosureCapturedInLoopRejected(t *testing.T) {
	errs := ownerErrs(t, `
		test() {
			done := channel[int](4);
			f := |int x| -> x + 1;
			for i in 0..3 {
				go {
					done.send(f(i));
				};
			}
		}
	`)
	expectOwnerError(t, errs, "'f'")
}

// A non-closure capture is unaffected: a string still transfers by B0354 without
// R5 gating, so the pre-T1640 behaviour is preserved.
func TestT1640NonClosureCaptureUnaffected(t *testing.T) {
	ownerOK(t, `
		test() {
			done := channel[int](1);
			s := "hello";
			go {
				done.send(s.len);
			};
		}
	`)
}

// T1651 + R4 — the env-ownership rules must not depend on source layout. With the
// declaration, the `go` block and the post-spawn use all on ONE line, the capture
// was previously classified as an in-block declaration, so it was never recorded
// and the post-spawn call read the env the coroutine had already freed.
func TestT1640SingleLineGoBlockClosureStillMoved(t *testing.T) {
	errs := ownerErrs(t, `
		test() {
			done := channel[int](1);
			int base = 10;
			{ f := |int x| -> x + base; go { done.send(f(1)); }; print_line("{f(2)}"); }
		}
	`)
	expectOwnerError(t, errs, "use of moved variable 'f'")
}

// --- R5's fail-closed allowlist: freshOwnedClosureExpr's accepted shapes ---
//
// R5 accepts a binding only when its RHS provably produced a FRESH owned heap
// env. Each accepted shape below has an owning `bindingFreeEnv` in codegen, so
// transferring it into the coroutine frame frees the env exactly once; each
// rejected shape aliases an env whose real owner would free it out from under
// the goroutine. The two halves are tested together because the failure mode of
// a missed *rejection* is a use-after-free, while the failure mode of a missed
// *acceptance* is a compile error that blocks a legitimate subscription API.

// A closure ALIAS is rejected even though its state is Owned: `g` copies the fat
// pointer out of `f` without duplicating the env, so `f` still frees it. This is
// the fail-closed default — no recognised fresh-owned RHS, so no crossing.
func TestT1640ClosureAliasLocalCapturedByGoRejected(t *testing.T) {
	errs := ownerErrs(t, `
		test() {
			done := channel[int](1);
			f := |int x| -> x + 1;
			(int) -> int g = f;
			go {
				done.send(g(1));
			};
		}
	`)
	expectOwnerError(t, errs, "it is not known to own its environment")
}

// A closure received from a channel owns its env — the sender moved it in, and
// the receive binding is the only owner. `(<-ch)!` also exercises the
// Optional-unwrap and paren peeling on the way to the receive.
func TestT1640ChannelReceivedClosureCapturedByGoAccepted(t *testing.T) {
	ownerOK(t, `
		test() {
			ch := channel[(int) -> int](1);
			done := channel[int](1);
			f := |int x| -> x + 1;
			ch.send(move f);
			g := (<-ch)!;
			go {
				done.send(g(1));
			};
		}
	`)
}

// A getter returns a fresh owned value, unlike a plain field read of the same
// shape (which aliases the aggregate's env and is rejected above).
func TestT1640GetterClosureCapturedByGoAccepted(t *testing.T) {
	ownerOK(t, `
		type Registry {
			int base;
			get callback (int) -> int { return move |int x| -> x + 1; }
		}
		test() {
			done := channel[int](1);
			r := Registry(base: 1);
			f := r.callback;
			go {
				done.send(f(1));
			};
		}
	`)
}

// The enum arm of isGetterMember: an enum getter is a fresh owned value too.
func TestT1640EnumGetterClosureCapturedByGoAccepted(t *testing.T) {
	ownerOK(t, `
		enum Kind {
			a,
			b,
			get handler (int) -> int { return move |int x| -> x + 2; }
		}
		test() {
			done := channel[int](1);
			k := Kind.a;
			h := k.handler;
			go {
				done.send(h(1));
			};
		}
	`)
}

// A user-defined non-native `[]` is a call in disguise — it RETURNS a fresh
// closure, so the binding owns the env.
func TestT1640UserIndexClosureCapturedByGoAccepted(t *testing.T) {
	ownerOK(t, `
		type Table {
			int base;
			[](int i) (int) -> int { return move |int x| -> x + i; }
		}
		test() {
			done := channel[int](1);
			tbl := Table(base: 0);
			f := tbl[5];
			go {
				done.send(f(1));
			};
		}
	`)
}

// Native container indexing is the opposite: `fns[0]` aliases the vector's
// storage, so the vector still owns (and frees) the env.
func TestT1640NativeVectorIndexClosureCapturedByGoRejected(t *testing.T) {
	errs := ownerErrs(t, `
		test() {
			done := channel[int](1);
			((int) -> int)[] fns = [];
			fns.push(move |int x| -> x + 1);
			f := fns[0];
			go {
				done.send(f(1));
			};
		}
	`)
	expectOwnerError(t, errs, "it borrows an environment owned elsewhere")
}

// Parenthesising the lambda must not defeat the allowlist — the peeling arms
// exist so a shape is classified by what it PRODUCES, not how it is spelled.
func TestT1640ParenthesizedLambdaCapturedByGoAccepted(t *testing.T) {
	ownerOK(t, `
		test() {
			done := channel[int](1);
			f := (|int x| -> x + 1);
			go {
				done.send(f(1));
			};
		}
	`)
}

// `?^` propagation peels to the underlying call, which is fresh-owned.
func TestT1640ErrorPropagatedClosureCapturedByGoAccepted(t *testing.T) {
	ownerOK(t, `
		make_cb!() (int) -> int {
			return move |int x| -> x + 1;
		}
		test!() {
			done := channel[int](1);
			f := make_cb()?^;
			go {
				done.send(f(1));
			};
		}
	`)
}

// `?!` (panic-on-error) peels the same way.
func TestT1640ErrorPanicClosureCapturedByGoAccepted(t *testing.T) {
	ownerOK(t, `
		make_cb!() (int) -> int {
			return move |int x| -> x + 1;
		}
		test() {
			done := channel[int](1);
			f := make_cb()?!;
			go {
				done.send(f(1));
			};
		}
	`)
}

// Reassignment must CLEAR a stale ownership record: `f` owned a fresh env at its
// declaration, but after `f = g` it only aliases `g`'s. Without the clear, the
// goroutine would be handed an env `g` still frees.
func TestT1640ClosureReassignedToAliasCapturedByGoRejected(t *testing.T) {
	errs := ownerErrs(t, `
		test() {
			done := channel[int](1);
			(int) -> int g = move |int x| -> x + 2;
			(int) -> int f = move |int x| -> x + 1;
			f = g;
			go {
				done.send(f(1));
			};
		}
	`)
	expectOwnerError(t, errs, "it is not known to own its environment")
}

// Reassignment to a FRESH lambda re-establishes ownership, so the spawn is fine.
func TestT1640ClosureReassignedToLambdaCapturedByGoAccepted(t *testing.T) {
	ownerOK(t, `
		test() {
			done := channel[int](1);
			(int) -> int f = move |int x| -> x + 1;
			f = move |int x| -> x + 2;
			go {
				done.send(f(1));
			};
		}
	`)
}

// `go v.push(f)` — an element-STORING native call. R5 defers to the existing
// consuming-slot diagnostic here rather than emitting its own, so the reported
// error must still be the `move` demand and not a duplicate borrow complaint.
func TestT1640GoCallElementStoringNativeDefersToMoveDiagnostic(t *testing.T) {
	errs := ownerErrs(t, `
		test() {
			((int) -> int)[] fns = [];
			f := |int x| -> x + 1;
			go fns.push(f);
		}
	`)
	expectOwnerError(t, errs, "consuming 'f' requires `move f`")
	for _, e := range errs {
		if strings.Contains(e.Error(), "cannot pass a borrow of closure") {
			t.Fatalf("R5 should defer to the consuming diagnostic, got a duplicate: %v", e)
		}
	}
}

// A non-closure capture reaching the goroutine through a nested lambda is not
// subject to R5 at all — isClosureType gates the rules to function values, so a
// string still takes the plain B0354 transfer.
func TestT1640NestedLambdaNonClosureCaptureUnaffected(t *testing.T) {
	ownerOK(t, `
		test() {
			done := channel[int](1);
			s := "hello";
			go {
				inner := move || -> s.len;
				done.send(inner());
			};
		}
	`)
}

// A fixed-size ARRAY of closures is native storage, like a vector: `arr[0]`
// aliases the array's element, so the array still frees the env.
func TestT1640ArrayIndexClosureCapturedByGoRejected(t *testing.T) {
	errs := ownerErrs(t, `
		test() {
			done := channel[int](1);
			((int) -> int)[2] arr = [|int x| -> x + 1, |int x| -> x + 2];
			g := arr[0];
			go {
				done.send(g(1));
			};
		}
	`)
	expectOwnerError(t, errs, "it borrows an environment owned elsewhere")
}

// A user-defined `[]` reached through a `~` (mutable-borrow) receiver is still a
// call: the ref is peeled before the operator is looked up, so the result is
// fresh-owned and may cross.
func TestT1640UserIndexThroughMutRefCapturedByGoAccepted(t *testing.T) {
	ownerOK(t, `
		type Table {
			int base;
			[](int i) (int) -> int { return move |int x| -> x + i; }
		}
		spawn_from_mut(Table~ t, channel[int] done) {
			f := t[5];
			go {
				done.send(f(1));
			};
		}
	`)
}

// The same capture used twice inside the block is moved ONCE: the post-spawn use
// must still be a single use-of-moved diagnostic, not one per in-block reference.
func TestT1640RepeatedClosureCaptureMovedOnce(t *testing.T) {
	errs := ownerErrs(t, `
		test() {
			done := channel[int](2);
			int base = 1;
			f := |int x| -> x + base;
			go {
				done.send(f(1));
				done.send(f(2));
			};
			print_line("{f(3)}");
		}
	`)
	expectOwnerError(t, errs, "use of moved variable 'f'")
	moved := 0
	for _, e := range errs {
		if strings.Contains(e.Error(), "use of moved variable 'f'") {
			moved++
		}
	}
	if moved != 1 {
		t.Fatalf("expected exactly 1 use-of-moved diagnostic for 'f', got %d: %v", moved, errs)
	}
}
