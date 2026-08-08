package sema

import "testing"

// T1385 / §17.2 "Producing the result": a `go {}` / `go! {}` block yields its
// result either with a trailing expression or with `return <expr>`, and `T` is
// inferred either way. Before this, `return` inside a go block bound to the
// ENCLOSING FUNCTION: the block typed `void` (so `v := <-t` failed), a `return`
// was type-checked against the wrong result type, and mixing the two styles or
// falling off a value path silently yielded a zero.

// --- Explicit-return style types the BLOCK, not the enclosing function ---

func TestT1385ExplicitReturnTypesTheBlock(t *testing.T) {
	// A1: previously "cannot bind void to variable 'v'".
	checkOK(t, `
		main() {
			t := go { return 42; };
			v := <-t;
			print_line(v.to_string());
		}
	`)
}

func TestT1385ExplicitReturnInVoidFunction(t *testing.T) {
	// A4: previously rejected with the enclosing fn's "function does not return
	// a value" — the go block has its own result contract.
	expectNoErrors(t, checkErrs(t, `
		main() {
			t := go { return 42; };
			print_line((<-t).to_string());
		}
	`))
}

func TestT1385ReturnDoesNotBindToEnclosingFunctionResult(t *testing.T) {
	// A3: `return "s"` inside a go block in an `int`-returning function types the
	// block `task[string]` — it is NOT checked against the function's `int`.
	// `s.len` only resolves if T really is string.
	checkOK(t, `
		f() int {
			t := go { return "s"; };
			s := <-t;
			return s.len;
		}
		main() { print_line(f().to_string()); }
	`)
}

func TestT1385FailableBlockExplicitReturn(t *testing.T) {
	// A2: the `go!` form infers T from its returns too.
	checkOK(t, `
		produce!(int n) int { return n * 2; }
		main() {
			t := go! { u := produce(21)?^; return u + 100; };
			v := (<-t)?!;
			print_line(v.to_string());
		}
	`)
}

func TestT1385FailableBlockAutoPropagatedReturnValue(t *testing.T) {
	// `return produce(5);` — a bare failable call as the returned value
	// auto-propagates into the task and yields its success type.
	checkOK(t, `
		produce!(int n) int { return n * 2; }
		main() {
			t := go! { return produce(5); };
			v := (<-t)?!;
			print_line(v.to_string());
		}
	`)
}

func TestT1385BareFailableReturnValueInPlainGoRejected(t *testing.T) {
	// T0976 still applies inside a plain `go {}` — the body is a non-failable
	// scope, so a failable call as the returned value must be handled.
	errs := checkErrs(t, `
		produce!(int n) int { return n * 2; }
		main() {
			t := go { return produce(5); };
			print_line((<-t).to_string());
		}
	`)
	expectError(t, errs, "failable call must be handled")
}

// --- §17.2 no mixing ---

func TestT1385MixingTrailingExprWithReturnRejected(t *testing.T) {
	// M1: previously compiled and yielded 0 — neither 7 nor 2.
	errs := checkErrs(t, `
		f() int {
			c := true;
			t := go { if c { return 7; } 2 };
			return <-t;
		}
		main() { print_line(f().to_string()); }
	`)
	expectError(t, errs, "this block returns with `return`; a trailing expression is discarded")
	expectError(t, errs, "hint: did you mean `return <expr>;`?")
}

func TestT1385MixingInVoidFunctionRejectedWithTheBlocksDiagnostic(t *testing.T) {
	// M2: previously rejected only incidentally, via the enclosing fn's
	// "function does not return a value".
	errs := checkErrs(t, `
		main() {
			c := true;
			t := go { if c { return 7; } 2 };
			print_line((<-t).to_string());
		}
	`)
	expectError(t, errs, "a trailing expression is discarded")
	expectNoErrorContaining(t, errs, "function does not return a value")
}

func TestT1385MixingTrailingIfElseWithReturnRejected(t *testing.T) {
	// The trailing value-producing if/else form (T1389) is a trailing expression
	// for this rule too.
	errs := checkErrs(t, `
		main() {
			c := true;
			t := go {
				if c { return 7; }
				if c { 1 } else { 2 }
			};
			print_line((<-t).to_string());
		}
	`)
	expectError(t, errs, "a trailing expression is discarded")
}

// --- §17.2 bare `return;` on a value-producing path ---

func TestT1385BareReturnOnValuePathExplicitStyleRejected(t *testing.T) {
	errs := checkErrs(t, `
		main() {
			c := true;
			t := go { if c { return; } return 42; };
			print_line((<-t).to_string());
		}
	`)
	expectError(t, errs, "missing return value (expected int)")
}

func TestT1385BareReturnOnValuePathTrailingStyleRejected(t *testing.T) {
	// R1: previously compiled and silently yielded 0.
	errs := checkErrs(t, `
		main() {
			c := true;
			t := go { if c { return; } 42 };
			print_line((<-t).to_string());
		}
	`)
	expectError(t, errs, "missing return value (expected int)")
}

func TestT1385BareReturnInVoidBlockAccepted(t *testing.T) {
	// In a `T = Void` block a bare return is an ordinary early exit.
	checkOK(t, `
		main() {
			c := true;
			t := go { if c { return; } print_line("no"); };
			<-t;
		}
	`)
}

// --- §17.2 all paths must produce the value in explicit-return style ---

func TestT1385ExplicitReturnFallThroughPathRejected(t *testing.T) {
	errs := checkErrs(t, `
		main() {
			c := true;
			t := go { if c { return 7; } };
			print_line((<-t).to_string());
		}
	`)
	expectError(t, errs, "goroutine block missing return statement")
}

func TestT1385ExplicitReturnFireAndForgetFallThroughRejected(t *testing.T) {
	// The all-paths rule applies to fire-and-forget blocks too — this is exactly
	// the shape that would otherwise reach codegen with no result store.
	errs := checkErrs(t, `
		main() {
			c := true;
			go { if c { return 7; } };
		}
	`)
	expectError(t, errs, "goroutine block missing return statement")
}

func TestT1385ExplicitReturnAllPathsAccepted(t *testing.T) {
	checkOK(t, `
		main() {
			c := true;
			t := go { if c { return 7; } return 2; };
			print_line((<-t).to_string());
		}
	`)
}

func TestT1385TrailingIfElseWhoseArmsAllReturnAccepted(t *testing.T) {
	// The block's last statement is an if/else, so blockValueType inspects it for
	// a trailing value — but both arms end in `return`, so it yields none. That
	// must read as "no trailing expression" (pure explicit-return style), NOT as a
	// mixing error, and blockReturns must see the if/else as covering all paths.
	checkOK(t, `
		main() {
			c := true;
			t := go { if c { return 7; } else { return 2; } };
			print_line((<-t).to_string());
		}
	`)
}

// --- Multi-return unification ---

func TestT1385IncompatibleReturnsRejected(t *testing.T) {
	errs := checkErrs(t, `
		main() {
			c := true;
			t := go { if c { return 7; } return "s"; };
			<-t;
		}
	`)
	// The unification runs through joinBranchTypes, but these are `return`
	// statements — the diagnostic must say so rather than blame "if/match arms".
	expectError(t, errs, "goroutine block `return` statements produce incompatible types 'int' and 'string'")
	expectNoErrorContaining(t, errs, "if/match arms")
}

func TestT1385NoneAndValueReturnsJoinToOptional(t *testing.T) {
	checkOK(t, `
		main() {
			c := true;
			t := go { if c { return 5; } return none; };
			v := <-t;
			if x := v { print_line(x.to_string()); }
		}
	`)
}

// --- A `return` in a lambda nested in a go block still binds to the lambda ---

func TestT1385LambdaReturnInsideGoBlockBindsToLambda(t *testing.T) {
	checkOK(t, `
		main() {
			t := go {
				fn := || -> int { return 7; };
				return fn() + 1;
			};
			print_line((<-t).to_string());
		}
	`)
}

func TestT1385LambdaReturnTypeMismatchStillCheckedAgainstLambda(t *testing.T) {
	// The lambda's own signature governs its returns — a string return from an
	// `int` lambda must be rejected even though the enclosing go block's T is
	// inferred from ITS returns.
	errs := checkErrs(t, `
		main() {
			t := go {
				fn := || -> int { return "s"; };
				return fn();
			};
			print_line((<-t).to_string());
		}
	`)
	expectError(t, errs, "cannot return string from function returning int")
}

// --- Trailing style is unchanged ---

func TestT1385TrailingStyleStillAccepted(t *testing.T) {
	checkOK(t, `
		main() {
			t := go { 42 };
			print_line((<-t).to_string());
		}
	`)
}

// --- The go-block return context is SCOPED: it is installed on entry to the
// body and restored on exit, so a `return` outside the body is unaffected. ---

func TestT1385ReturnAfterGoBlockStillBindsToTheEnclosingFunction(t *testing.T) {
	// checkGoExpr must restore c.goBlock — otherwise the function's own `return`
	// would be swallowed by the block's (already finished) inference and its
	// result-type mismatch would go unreported.
	errs := checkErrs(t, `
		f() int {
			t := go { return "s"; };
			<-t;
			return "x";
		}
		main() { print_line(f().to_string()); }
	`)
	expectError(t, errs, "cannot return string from function returning int")
	expectNoErrorContaining(t, errs, "goroutine block")
}

func TestT1385ReturnAfterGoBlockInMethodStillBindsToTheMethod(t *testing.T) {
	// checkMethodBody clears/restores the context alongside curFunc (the sibling
	// of the checkFuncDecl case above).
	errs := checkErrs(t, `
		type H {
			int base;
			m(this) int {
				t := go { return "s"; };
				<-t;
				return "x";
			}
		}
		main() { print_line(H(base: 1).m().to_string()); }
	`)
	expectError(t, errs, "cannot return string from function returning int")
}

func TestT1385GoBlockReturnInsideMethodTypesTheBlock(t *testing.T) {
	// The positive form: inside a method, the block's T is still inferred from
	// its own returns, independently of the method's result type.
	checkOK(t, `
		type H {
			int base;
			m(this) int {
				t := go { return "s"; };
				return (<-t).len + this.base;
			}
		}
		main() { print_line(H(base: 1).m().to_string()); }
	`)
}

// --- Nested go blocks: each body owns its own returns ---

func TestT1385NestedGoBlocksEachOwnTheirReturns(t *testing.T) {
	checkOK(t, `
		main() {
			t := go {
				inner := go { return 5; };
				return (<-inner) + 1;
			};
			print_line((<-t).to_string());
		}
	`)
}

func TestT1385NestedGoBlockMismatchDoesNotEscapeToTheOuterBlock(t *testing.T) {
	// The INNER block's returns are incompatible; the outer block's single return
	// is fine. Exactly one diagnostic, and the outer inference must still resolve
	// (no cascade) — this is what the save/restore around the nested checkGoExpr
	// buys.
	errs := checkErrs(t, `
		main() {
			c := true;
			t := go {
				inner := go {
					if c { return 1; }
					return "s";
				};
				return <-inner;
			};
			<-t;
		}
	`)
	expectError(t, errs, "goroutine block `return` statements produce incompatible types 'int' and 'string'")
	expectNoErrorContaining(t, errs, "missing return statement")
	expectNoErrorContaining(t, errs, "trailing expression is discarded")
}

// --- The unification diagnostic names the go block on every message site ---

func TestT1385ReturnValueVoidMismatchUsesTheGoBlockSubject(t *testing.T) {
	// One return produces a value, the other a void expression. joinBranchTypes'
	// void-mismatch message is a SECOND site that has to carry the go-block
	// subject rather than blame "if/match arms".
	errs := checkErrs(t, `
		main() {
			c := true;
			t := go {
				if c { return 5; }
				return print_line("x");
			};
			<-t;
		}
	`)
	expectError(t, errs, "goroutine block `return` statements produce incompatible types 'int' and 'void'")
	expectNoErrorContaining(t, errs, "if/match arms")
}

func TestT1385ReturnBorrowedAndOwnedMixUsesTheGoBlockSubject(t *testing.T) {
	// The third message site: one return yields an explicit borrow, the other a
	// freshly-owned value of the same non-Copy type.
	errs := checkErrs(t, `
		type Box { string s; }
		g(Box b) {
			c := true;
			t := go {
				Box& r = b;
				if c { return r; }
				return Box(s: "new");
			};
			<-t;
		}
		main() {}
	`)
	expectError(t, errs, "goroutine block `return` statements mix borrowed and owned non-Copy 'Box'")
	expectNoErrorContaining(t, errs, "if/match arms")
}

// --- Sibling subtypes join to their nearest common ancestor ---

func TestT1385SiblingSubtypeReturnsJoinToTheParent(t *testing.T) {
	// `.name` only resolves if T really joined to Animal (rather than sticking at
	// Dog or erroring out).
	checkOK(t, `
		type Animal { string name; }
		type Dog is Animal {}
		type Cat is Animal {}
		main() {
			c := true;
			t := go {
				if c { return Dog(name: "d"); }
				return Cat(name: "c");
			};
			print_line((<-t).name);
		}
	`)
}

// --- `return <void expr>` keeps the block at T = Void ---

func TestT1385ReturnVoidExpressionKeepsTheBlockVoid(t *testing.T) {
	// `return f();` where f yields no value is legal (the same rule a void
	// function follows), so the block stays `task[Void]` — and a bare `return;`
	// on a sibling path is then an ordinary early exit, not "missing return
	// value". Binding the result would fail if T were anything but Void.
	checkOK(t, `
		main() {
			c := true;
			t := go {
				if c { return; }
				return print_line("x");
			};
			<-t;
		}
	`)
}

func TestT1385ReturnVoidExpressionIsNotATrailingValue(t *testing.T) {
	// A void-typed `return` makes hasValueRet true, so the no-mixing and
	// all-paths rules engage — but T is Void, so neither fires here and the
	// block is accepted with no result to bind.
	expectNoErrors(t, checkErrs(t, `
		main() {
			go { return print_line("x"); };
		}
	`))
}

// --- An erroring return expression must not cascade ---

func TestT1385UntypeableReturnValueReportsOnlyItsOwnError(t *testing.T) {
	// The return value fails to type, so the block's T stays unknown. That must
	// surface as exactly the one real diagnostic — not as a follow-on "missing
	// return statement" / "incompatible types" cascade from the §17.2 rules
	// running against a half-inferred T.
	errs := checkErrs(t, `
		main() {
			t := go { return nonexistent_function(); };
			<-t;
		}
	`)
	expectError(t, errs, "undefined: nonexistent_function")
	expectNoErrorContaining(t, errs, "goroutine block")
	expectNoErrorContaining(t, errs, "missing return value")
}

// --- The all-paths rule delegates to blockReturns, so every construct that
// construct decides about has to read correctly through the go block. ---

func TestT1385AllPathsThroughMatchArmsAccepted(t *testing.T) {
	// An exhaustive match whose every arm returns covers all paths — the block is
	// in explicit-return style with no fall-through, so no "missing return".
	checkOK(t, `
		enum E { A(int n), B(int n) }
		main() {
			e := E.A(n: 1);
			t := go {
				match e {
					A(n) => { return n; },
					B(n) => { return n * 2; },
				}
			};
			print_line((<-t).to_string());
		}
	`)
}

func TestT1385ReturnOnlyInsideALoopIsNotAllPaths(t *testing.T) {
	// A loop body may run zero times, so a `return` inside one does not cover the
	// fall-through — the same rule a function body follows.
	errs := checkErrs(t, `
		main() {
			t := go {
				for i in 0..3 {
					if i == 1 { return i; }
				}
			};
			print_line((<-t).to_string());
		}
	`)
	expectError(t, errs, "goroutine block missing return statement")
}

// --- The bare-`return;` rule applies to the `go!` form too ---

func TestT1385GobangBareReturnOnValuePathRejected(t *testing.T) {
	// The deferred bare-return verdict reads T off the failable block's inferred
	// element, not its {ok,err} aggregate.
	errs := checkErrs(t, `
		produce!(int n) int { return n * 2; }
		main() {
			c := true;
			t := go! {
				base := produce(1)?^;
				if c { return; }
				return base;
			};
			print_line(((<-t)?!).to_string());
		}
	`)
	expectError(t, errs, "missing return value (expected int)")
}

// --- A go block nested INSIDE a lambda (the reverse of the lambda-inside-go
// case above): checkLambdaExpr clears the context on entry, and checkGoExpr
// installs its own inside — each level must keep its own returns. ---

func TestT1385GoBlockInsideLambdaTypesTheBlock(t *testing.T) {
	// `.len` only resolves if the block's T came from its own `return "s"`, and
	// the lambda's `int` result governs the lambda's return.
	checkOK(t, `
		main() {
			fn := || -> int {
				t := go { return "s"; };
				return (<-t).len;
			};
			print_line(fn().to_string());
		}
	`)
}

func TestT1385ReturnAfterGoBlockInsideLambdaBindsToTheLambda(t *testing.T) {
	errs := checkErrs(t, `
		main() {
			fn := || -> int {
				t := go { return "s"; };
				<-t;
				return "x";
			};
			print_line(fn().to_string());
		}
	`)
	expectError(t, errs, "cannot return string from function returning int")
	expectNoErrorContaining(t, errs, "goroutine block")
}

// --- Inside a generic function the block's T is the type parameter ---

func TestT1385GenericFunctionGoBlockReturnsTheTypeParam(t *testing.T) {
	checkOK(t, `
		wrap[T](T x) T {
			t := go { return x; };
			return <-t;
		}
		main() { print_line(wrap[int](5).to_string()); }
	`)
}

// --- A go block nested in a GENERATOR body ---

// A generator's own `return` is special (bare = stop producing, a value is an
// error — `yield` is the value channel). A `return <expr>` inside a go block
// nested in that generator must NOT hit either rule: c.goBlock short-circuits
// checkReturnStmt ahead of the generator/curFunc handling, so the block types
// from its own returns and `<-t` is the element type, not `stream[int]`.
func TestT1385GoBlockInsideGeneratorTypesTheBlock(t *testing.T) {
	checkOK(t, `
		gen() stream[int] {
			t := go { return 42; };
			yield <-t;
			yield 2;
		}
		main() {
			for v in gen() { print_line(v.to_string()); }
		}
	`)
}

// The mirror image: once the go block is closed, `return` binds to the GENERATOR
// again, so a value there is still the generator's error and a bare one is still
// the legal "stop producing" exit.
func TestT1385ReturnAfterGoBlockInsideGeneratorBindsToTheGenerator(t *testing.T) {
	checkOK(t, `
		gen() stream[int] {
			t := go { return 42; };
			yield <-t;
			return;
		}
		main() {
			for v in gen() { print_line(v.to_string()); }
		}
	`)
	errs := checkErrs(t, `
		gen() stream[int] {
			t := go { return 42; };
			yield <-t;
			return 7;
		}
		main() {
			for v in gen() { print_line(v.to_string()); }
		}
	`)
	expectError(t, errs, "cannot return a value from a generator")
}

// A go block inside a generator is subject to the §17.2 rules like any other —
// the generator context must not suppress them.
func TestT1385GoBlockInsideGeneratorStillEnforcesTheMixingRule(t *testing.T) {
	errs := checkErrs(t, `
		gen() stream[int] {
			c := true;
			t := go { if c { return 7; } 2 };
			yield <-t;
		}
		main() {
			for v in gen() { print_line(v.to_string()); }
		}
	`)
	expectError(t, errs, "a trailing expression is discarded")
	expectNoErrorContaining(t, errs, "cannot return a value from a generator")
}

// --- A generic METHOD on a generic TYPE ---

// buildMethodInstanceSubst merges the owner's and the method's type params; the
// block's inferred T is the method's param here, so the go-block return context
// has to survive that two-level substitution.
func TestT1385GenericMethodOnGenericTypeGoBlockReturn(t *testing.T) {
	checkOK(t, `
		type GBox[T] {
			T v;
			relay[U](this, U extra) U {
				t := go { return extra; };
				return <-t;
			}
		}
		main() {
			b := GBox[int](v: 1);
			print_line(b.relay[string]("ok"));
		}
	`)
}
