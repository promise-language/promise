package sema

import "testing"

// T1640: closures/function values are sendable (R1), every closure capture must
// itself be sendable (R2), and a `go` block's capture set is recorded for
// ownership (checkGoBlockCaptures).

// R1 — a closure captured by a `go {}` block crosses the boundary.
func TestT1640ClosureCapturedByGoBlock(t *testing.T) {
	expectNoErrors(t, checkErrs(t, `
		main() {
			done := channel[int](1);
			f := |int x| -> x + 1;
			go {
				done.send(f(1));
			};
		}
	`))
}

// R1 — a closure passed as a `go` call argument crosses the boundary.
func TestT1640ClosureAsGoCallArgument(t *testing.T) {
	expectNoErrors(t, checkErrs(t, `
		run_it((int) -> int move f, channel[int] done) {
			done.send(f(1));
		}
		main() {
			done := channel[int](1);
			f := |int x| -> x + 1;
			go run_it(move f, done);
		}
	`))
}

// R1 — Channel[(int) -> int] is a legal channel element type.
func TestT1640ChannelOfClosureIsSendable(t *testing.T) {
	expectNoErrors(t, checkErrs(t, `
		main() {
			ch := channel[(int) -> int](1);
			f := |int x| -> x + 1;
			ch.send(move f);
		}
	`))
}

// R1 — a named function reference (zero captures, null env) crosses too.
func TestT1640NamedFunctionReferenceIsSendable(t *testing.T) {
	expectNoErrors(t, checkErrs(t, `
		double(int x) int { return x * 2; }
		main() {
			done := channel[int](1);
			(int) -> int f = double;
			go {
				done.send(f(3));
			};
		}
	`))
}

// R2 — a non-sendable value captured by a lambda is rejected at the capture site,
// because the boundary check can no longer see it once the closure type erases it.
func TestT1640NonSendableCaptureRejected(t *testing.T) {
	errs := checkErrs(t, `
		type Cfg `+"`confined"+` {
			int n;
		}
		main() {
			r := Ref[Cfg](Cfg(n: 1));
			f := move || -> 1;
			g := move || -> r.get.n;
		}
	`)
	expectError(t, errs, "cannot capture non-sendable value 'r'")
}

// R2 — the same rejection fires for a `not_sendable`-tagged type.
func TestT1640NotSendableCaptureRejected(t *testing.T) {
	errs := checkErrs(t, `
		type Handle `+"`not_sendable"+` {
			int fd;
		}
		main() {
			h := Handle(fd: 3);
			f := move || -> h.fd;
		}
	`)
	expectError(t, errs, "cannot capture non-sendable value 'h'")
}

// R2 — a non-sendable value reaching a lambda only through a NESTED lambda is
// rejected too, via the capture-propagation site.
func TestT1640NonSendableNestedLambdaCaptureRejected(t *testing.T) {
	errs := checkErrs(t, `
		type Handle `+"`not_sendable"+` {
			int fd;
		}
		main() {
			h := Handle(fd: 3);
			outer := move || {
				inner := move || -> h.fd;
				return inner();
			};
		}
	`)
	expectError(t, errs, "cannot capture non-sendable value 'h'")
}

// checkGoBlockCaptures walks a nested lambda's capture list, mirroring codegen's
// collectBlockIdents: a value reaching the goroutine ONLY through a nested lambda
// is captured by the coroutine, so it must pass the boundary's sendability gate.
// Before T1640 the LambdaExpr arm skipped these entirely.
func TestT1640NestedLambdaCaptureCheckedAtGoBoundary(t *testing.T) {
	errs := checkErrs(t, `
		type Handle `+"`not_sendable"+` {
			int fd;
		}
		main() {
			h := Handle(fd: 3);
			done := channel[int](1);
			go {
				inner := move || -> h.fd;
				done.send(inner());
			};
		}
	`)
	expectError(t, errs, "non-sendable")
}

// A `~` mutable-borrow capture stays rejected at the go boundary (T1589) — the
// record helper preserved that gate when checkGoBlockCaptures was refactored.
func TestT1640MutRefCaptureStillRejectedAtGoBoundary(t *testing.T) {
	errs := checkErrs(t, `
		spawn(int~ n, channel[int] done) {
			go {
				done.send(n);
			};
		}
	`)
	expectError(t, errs, "cannot capture mutable borrow 'n'")
}

// A naked `&closure` stays non-sharable: Ref[(int) -> int] is still refused,
// because sharing a heap env without a refcount aliases it across goroutines.
func TestT1640ClosureStillNotSharable(t *testing.T) {
	errs := checkErrs(t, `
		main() {
			a := Ref[(int) -> int](|int x| -> x + 1);
		}
	`)
	expectError(t, errs, "not sharable")
}

// T1653: an optional-wrapped closure captured by a `go {}` block is gated out —
// B0354's capture transfer re-derives the goroutine-side binding from the outer
// binding's ELEMENT type, so the env-free is never registered and the env leaks.
// The bare closure and the `go f(move opt)` call form both work.
func TestT1640OptionalClosureGoBlockCaptureGated(t *testing.T) {
	errs := checkErrs(t, `
		main() {
			done := channel[int](1);
			int base = 7;
			((int) -> int)? f = |int x| -> x + base;
			go {
				done.send(f!(1));
			};
		}
	`)
	expectError(t, errs, "cannot capture optional closure 'f'")
}

// T1651: scopeContains compared the three line cases independently, so a `go {}`
// written on ONE line swallowed its whole line — declarations preceding the `go`
// keyword were classified as in-block, and every gate keyed off this walk
// (sendability, T1589's mutable borrow, T1640's env-ownership recording) silently
// did not run. A single-line spelling must diagnose exactly like a multi-line one.
func TestT1640SingleLineGoBlockStillChecksCaptures(t *testing.T) {
	errs := checkErrs(t, `
		type Handle `+"`not_sendable"+` {
			int fd;
		}
		main() {
			done := channel[int](1);
			{ h := Handle(fd: 3); go { done.send(h.fd); }; }
		}
	`)
	expectError(t, errs, "cannot send non-sendable variable 'h'")
}

// The LambdaExpr arm of checkGoBlockCaptures runs the FULL `record` gate, not
// just the sendability half: a value reaching the goroutine only through a
// nested lambda is captured by the coroutine (codegen's collectBlockIdents pulls
// in LambdaCaptures), so the T1653 optional-closure gate must fire there too.
// The bare-capture form is covered by TestT1640OptionalClosureGoBlockCaptureGated;
// this pins the nested path, which is the one codegen reaches by a different
// route and which would otherwise leak the optional's env silently.
func TestT1640OptionalClosureNestedLambdaCaptureGated(t *testing.T) {
	errs := checkErrs(t, `
		main() {
			done := channel[int](1);
			int base = 7;
			((int) -> int)? f = |int x| -> x + base;
			go {
				inner := move || -> f!(1);
				done.send(inner());
			};
		}
	`)
	expectError(t, errs, "cannot capture optional closure 'f'")
}

// A Vector of closures is sendable, because Vector derives sendability from its
// element type and a Signature element is now sendable. This is the shape a
// subscription API actually needs — a list of registered callbacks handed to a
// goroutine (docs/wasm-web-callbacks.md Phase 3).
func TestT1640VectorOfClosuresIsSendable(t *testing.T) {
	expectNoErrors(t, checkErrs(t, `
		main() {
			done := channel[int](1);
			((int) -> int)[] fns = [];
			fns.push(move |int x| -> x + 1);
			go {
				done.send(fns[0](1));
			};
		}
	`))
}

// A Map keyed to closures is sendable for the same reason — the dispatch-table
// shape of an event registry.
func TestT1640MapOfClosuresIsSendable(t *testing.T) {
	expectNoErrors(t, checkErrs(t, `
		main() {
			done := channel[int](1);
			map[string, (int) -> int] handlers = {:};
			handlers["a"] = move |int x| -> x + 1;
			go {
				done.send(handlers["a"]!(1));
			};
		}
	`))
}

// A `sendable`-annotated generic instantiated with a function type is accepted:
// validateSendableInstance re-runs isSendableType on the concrete argument, and
// R1 makes a Signature pass. Before T1640 this was a hard error.
func TestT1640SendableGenericInstantiatedWithClosure(t *testing.T) {
	expectNoErrors(t, checkErrs(t, `
		type Box[T] `+"`sendable"+` {
			T value;
		}
		main() {
			b := Box[(int) -> int](value: move |int x| -> x + 1);
		}
	`))
}

// A `not_sendable` value must not slip through the optional-chain arm of the
// go-block walk, which reaches the receiver of `?.` and nothing else.
func TestT1640OptionalChainCaptureStillChecked(t *testing.T) {
	errs := checkErrs(t, `
		type Handle `+"`not_sendable"+` {
			int fd;
			get id int { return this.fd; }
		}
		main() {
			done := channel[int](1);
			Handle? h = Handle(fd: 3);
			go {
				int? v = h?.id;
				done.send(v!);
			};
		}
	`)
	expectError(t, errs, "cannot send non-sendable variable 'h'")
}

// A closure that captures a `not_sendable` value is rejected at the CAPTURE site
// (R2), independently of any goroutine: once a function value is sendable, the
// env may be moved into a coroutine long after the capture, and the boundary
// cannot see through the erased signature to check it there.
func TestT1640NonSendableCaptureRejectedWithoutAnyGoroutine(t *testing.T) {
	errs := checkErrs(t, `
		type Handle `+"`not_sendable"+` {
			int fd;
		}
		main() {
			h := Handle(fd: 3);
			f := move || -> h.fd;
		}
	`)
	expectError(t, errs, "every capture must be sendable")
}

// A NESTED optional is peeled all the way down before the T1653 gate decides:
// `((int) -> int)??` owns exactly the same single heap env as the bare closure,
// so it must be gated identically rather than slipping through as "not an
// optional closure".
func TestT1640NestedOptionalClosureGoBlockCaptureGated(t *testing.T) {
	errs := checkErrs(t, `
		main() {
			done := channel[int](1);
			(((int) -> int)?)? f = none;
			go {
				done.send(f!!(1));
			};
		}
	`)
	expectError(t, errs, "cannot capture optional closure 'f'")
}

// A nested lambda that captures `this` is skipped by the go-block walk: `this` is
// a borrow of the receiver, not an owned capture the coroutine can take, and the
// enclosing method's own capture check already governs it. This is the shape
// `web.on` needs (a method registering a callback that reads its receiver), so
// the skip must not turn into a spurious boundary error.
func TestT1640ThisCaptureThroughNestedLambdaAllowedAtGoBoundary(t *testing.T) {
	expectNoErrors(t, checkErrs(t, `
		type Counter {
			int n;
			spawn(this, channel[int] done) {
				go {
					inner := move || -> this.n;
					done.send(inner());
				};
			}
		}
	`))
}

// A capture referenced MORE THAN ONCE inside the block is recorded once. The
// dedupe matters beyond tidiness: GoCaptures drives ownership's R4 move-marking,
// and a duplicated entry would re-run the move (and its loop-move bookkeeping)
// against an already-moved binding.
func TestT1640RepeatedClosureCaptureRecordedOnce(t *testing.T) {
	expectNoErrors(t, checkErrs(t, `
		main() {
			done := channel[int](2);
			int base = 1;
			f := |int x| -> x + base;
			go {
				done.send(f(1));
				done.send(f(2));
			};
		}
	`))
}

// R2 applies to a `this` capture too: a method of a `not_sendable` type cannot
// hand its receiver to a closure, because that closure value is sendable and the
// env could be moved into a goroutine long after this point. This is the third
// capture-recording site, and it shares rejectNonSendableCapture with the other
// two precisely so it cannot drift.
func TestT1640NonSendableThisCaptureRejected(t *testing.T) {
	errs := checkErrs(t, `
		type Handle `+"`not_sendable"+` {
			int fd;
			make_reader(this) () -> int {
				return move || -> this.fd;
			}
		}
	`)
	expectError(t, errs, "cannot capture non-sendable value 'this' of type Handle")
}
