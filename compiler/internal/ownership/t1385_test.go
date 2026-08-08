package ownership

import "testing"

// T1385 / §17.2 explicit-return style: `return <expr>` inside a `go {}` / `go! {}`
// block became reachable for the first time, so it now flows through the ownership
// pass's return handling (checkExpr + tryMove + checkReturnRefSafety +
// checkReturnBorrowsLocal). Those checks keyed off the ENCLOSING function's result
// type, which is the wrong contract inside a go block: they bailed entirely when
// the enclosing fn was void and applied the borrow-result rules when it returned a
// borrow. currentReturnResult now prefers the task's element type.

// Moving an owned heap local out via `return` inside a go block is legal — the
// value escapes into the task's result buffer, so the local is consumed, not
// dropped. Must not be a false "use of moved variable" / borrow error.
func TestT1385GoBlockReturnMovesOwnedHeapLocal(t *testing.T) {
	ownerOK(t, `
		type R { int id; drop(~this) {} }
		main() {
			t := go {
				r := R(id: 1);
				return r;
			};
			v := <-t;
		}
	`)
}

func TestT1385GoBlockReturnMovesOwnedStringLocal(t *testing.T) {
	ownerOK(t, `
		main() {
			t := go {
				s := "a" + "b";
				return s;
			};
			v := <-t;
		}
	`)
}

// The moved-out local really is moved: using it after the return would be a
// use-after-move — but the return is the last statement, so the interesting case
// is the reverse order (use after a conditional return path is fine).
func TestT1385GoBlockUseAfterMoveIntoReturnRejected(t *testing.T) {
	errs := ownerErrs(t, `
		type R { int id; drop(~this) {} }
		consume(R move r) {}
		main() {
			t := go {
				r := R(id: 1);
				consume(r);
				return r;
			};
			v := <-t;
		}
	`)
	expectOwnerError(t, errs, "moved")
}

// Enclosing fn is VOID: previously checkReturnRefSafety bailed on
// curSig.Result() == nil, so nothing checked the go block's owned result. A
// borrowed single-owner handle escaping into the task must be rejected.
func TestT1385GoBlockReturnBorrowedHandleParamRejectedInVoidFunction(t *testing.T) {
	errs := ownerErrs(t, `
		spawn(task[int] h) {
			t := go { return h; };
			v := <-t;
		}
		main() {}
	`)
	expectOwnerError(t, errs, "cannot return borrowed parameter")
}

// A `move` parameter transfers ownership, so the same shape is legal.
func TestT1385GoBlockReturnMovedHandleParamAccepted(t *testing.T) {
	ownerOK(t, `
		spawn(task[int] move h) {
			t := go { return h; };
			v := <-t;
		}
		main() {}
	`)
}

// Enclosing fn returns a BORROW: previously checkReturnRefSafety took the
// borrow-result branch (origin/lifetime rules) for the go block's returns, which
// is the wrong contract — a task element is always owned.
func TestT1385GoBlockReturnLocalInBorrowReturningFunction(t *testing.T) {
	ownerOK(t, `
		type R { int id; drop(~this) {} }
		hold(R src) R& {
			t := go {
				r := R(id: 1);
				return r;
			};
			v := <-t;
			return src;
		}
		main() {}
	`)
}

// A `T = Void` go block: `return <void expr>;` is legal there but escapes no
// value, so no result contract applies. It must NOT fall back to the enclosing
// function's borrow result and run the origin/lifetime rules on it.
func TestT1385VoidGoBlockReturnInBorrowReturningFunction(t *testing.T) {
	ownerOK(t, `
		type R { int id; drop(~this) {} }
		hold(R src) R& {
			go { return print_line("x"); };
			return src;
		}
		main() {}
	`)
}

// A `return` inside a lambda nested in a go block binds to the LAMBDA, so the
// lambda's own signature still governs its return checks.
func TestT1385LambdaInsideGoBlockKeepsItsOwnReturnContract(t *testing.T) {
	ownerOK(t, `
		main() {
			t := go {
				fn := || -> int { return 7; };
				return fn() + 1;
			};
			v := <-t;
		}
	`)
}

// Fire-and-forget: the returned value is discarded rather than escaping, and the
// ownership pass must not reject the move.
func TestT1385FireAndForgetGoBlockReturnAccepted(t *testing.T) {
	ownerOK(t, `
		type R { int id; drop(~this) {} }
		main() {
			go {
				r := R(id: 1);
				return r;
			};
		}
	`)
}

// --- The go-block result context is SCOPED: restored on exit from the body ---

// After the block, the enclosing function's own borrow-result rules must apply
// again. Without the restore, goBlockResult would still be the task's OWNED
// element and the origin/lifetime branch would be skipped entirely — silently
// admitting a dangling reference to a local.
func TestT1385ReturnAfterGoBlockUsesTheEnclosingBorrowRules(t *testing.T) {
	errs := ownerErrs(t, `
		type R { int id; drop(~this) {} }
		hold(R src) R& {
			t := go {
				r := R(id: 1);
				return r;
			};
			v := <-t;
			loc := R(id: 2);
			return loc;
		}
		main() {}
	`)
	expectOwnerError(t, errs, "cannot return reference to local variable")
}

// Same restore, from the `T = Void` state (which reports nil, the same value
// "no go block" would) — the distinction has to survive the exit either way.
func TestT1385ReturnAfterVoidGoBlockUsesTheEnclosingBorrowRules(t *testing.T) {
	errs := ownerErrs(t, `
		type R { int id; drop(~this) {} }
		hold(R src) R& {
			go { return print_line("x"); };
			loc := R(id: 2);
			return loc;
		}
		main() {}
	`)
	expectOwnerError(t, errs, "cannot return reference to local variable")
}

// --- Nested go blocks: each body's returns answer to its OWN task element ---

func TestT1385NestedGoBlockReturnsMoveOwnedLocals(t *testing.T) {
	ownerOK(t, `
		type R { int id; drop(~this) {} }
		main() {
			t := go {
				inner := go {
					r := R(id: 1);
					return r;
				};
				v := <-inner;
				return v;
			};
			w := <-t;
		}
	`)
}

// The inner block is fire-and-forget (its value is discarded) while the outer is
// awaited — the save/restore must hand each the right sink.
func TestT1385NestedFireAndForgetInsideAwaitedGoBlock(t *testing.T) {
	ownerOK(t, `
		type R { int id; drop(~this) {} }
		main() {
			t := go {
				go {
					inner := R(id: 1);
					return inner;
				};
				outer := R(id: 2);
				return outer;
			};
			v := <-t;
		}
	`)
}

// The inner block returning a BORROWED single-owner handle is still rejected
// even though the outer block's element type is something else entirely.
func TestT1385NestedGoBlockBorrowedHandleStillRejected(t *testing.T) {
	errs := ownerErrs(t, `
		spawn(task[int] h) {
			t := go {
				go { return h; };
				return 1;
			};
			v := <-t;
		}
		main() {}
	`)
	expectOwnerError(t, errs, "cannot return borrowed parameter")
}

// --- A `return` from inside a loop in a go block ---

// checkExpr resets loopDepth to 0 for the go-block body, so a `return` from
// inside a loop nested in that body must still move the owned local out cleanly.
func TestT1385GoBlockReturnFromInsideLoopMovesOwnedLocal(t *testing.T) {
	ownerOK(t, `
		type R { int id; drop(~this) {} }
		main() {
			t := go {
				for i in 0..3 {
					if i == 1 {
						r := R(id: i);
						return r;
					}
				}
				return R(id: 9);
			};
			v := <-t;
		}
	`)
}

// --- A go block inside a METHOD body ---

func TestT1385GoBlockReturnInsideMethodMovesOwnedLocal(t *testing.T) {
	ownerOK(t, `
		type R { int id; drop(~this) {} }
		type H {
			int base;
			m(this) int {
				t := go {
					r := R(id: 1);
					return r;
				};
				return (<-t).id + this.base;
			}
		}
		main() {}
	`)
}

// A `use`-bound variable is pinned: it cannot be moved out of the block via the
// explicit-return exit either — its close() owns the scope exit.
func TestT1385GoBlockReturnOfUseBoundVariableRejected(t *testing.T) {
	errs := ownerErrs(t, `
		type R { int id; close(~this) void {} }
		main() {
			t := go {
				use r := R(id: 1);
				return r;
			};
			v := <-t;
		}
	`)
	expectOwnerError(t, errs, "cannot move use-bound variable")
}

// Returning the SAME local on two mutually exclusive branches is one move per
// path, not a double move.
func TestT1385GoBlockReturnSameLocalOnBothBranches(t *testing.T) {
	ownerOK(t, `
		type R { int id; drop(~this) {} }
		main() {
			c := true;
			t := go {
				r := R(id: 1);
				if c { return r; }
				return r;
			};
			v := <-t;
		}
	`)
}

// --- A go block nested INSIDE a lambda (the reverse of the lambda-inside-go
// case above) ---

func TestT1385GoBlockInsideLambdaMovesOwnedLocal(t *testing.T) {
	ownerOK(t, `
		type R { int id; drop(~this) {} }
		main() {
			fn := || -> R {
				t := go { r := R(id: 1); return r; };
				return <-t;
			};
			v := fn();
		}
	`)
}

func TestT1385ReturnAfterGoBlockInsideLambdaUsesTheLambdaBorrowRules(t *testing.T) {
	// The lambda returns a borrow, so its own `return loc;` must still be caught
	// after the nested block's owned-result context has been restored.
	errs := ownerErrs(t, `
		type R { int id; drop(~this) {} }
		main() {
			fn := |R src| -> R& {
				t := go { r := R(id: 1); return r; };
				v := <-t;
				loc := R(id: 2);
				return loc;
			};
		}
	`)
	expectOwnerError(t, errs, "cannot return reference to local variable")
}

// --- A go block nested in a GENERATOR body ---

func TestT1385GoBlockInsideGeneratorMovesOwnedLocal(t *testing.T) {
	ownerOK(t, `
		type R { int id; drop(~this) {} }
		gen() stream[int] {
			t := go { r := R(id: 1); return r; };
			yield (<-t).id;
		}
		main() { for v in gen() {} }
	`)
}

// --- The FAILABLE spawn form: `go! {}` yields `failable_task[T]` ---

// goBlockResult is read out of the go expression's type, which is
// `failable_task[T]` here rather than `task[T]`. If that unwrap missed the
// failable form, the result would fall back to types.TypVoid, currentReturnResult
// would report nil, and the owned-result checks below would be SKIPPED — a
// borrowed handle would escape into the task unreported. The enclosing function
// is void, so nothing else supplies a result type to check against.
func TestT1385FailableGoBlockReturnBorrowedHandleParamRejected(t *testing.T) {
	errs := ownerErrs(t, `
		produce!(int n) int { return n; }
		spawn(task[int] h) {
			t := go! {
				produce(1)?^;
				return h;
			};
			v := (<-t)? e { };
		}
		main() {}
	`)
	expectOwnerError(t, errs, "cannot return borrowed parameter")
}

// The `move` counterpart, so the rejection above is attributable to the borrow
// and not to the failable spawn form itself.
func TestT1385FailableGoBlockReturnMovedHandleParamAccepted(t *testing.T) {
	ownerOK(t, `
		produce!(int n) int { return n; }
		spawn(task[int] move h) {
			t := go! {
				produce(1)?^;
				return h;
			};
			v := (<-t)? e { };
		}
		main() {}
	`)
}

// A `go!` block nested in a BORROW-returning function: the same wrong-contract
// hazard as the plain-`go` case above, but reached through the failable unwrap.
func TestT1385FailableGoBlockReturnLocalInBorrowReturningFunction(t *testing.T) {
	ownerOK(t, `
		type R { int id; drop(~this) {} }
		produce!(int n) int { return n; }
		hold(R src) R& {
			t := go! {
				produce(1)?^;
				r := R(id: 1);
				return r;
			};
			v := (<-t)? e { };
			return src;
		}
		main() {}
	`)
}
