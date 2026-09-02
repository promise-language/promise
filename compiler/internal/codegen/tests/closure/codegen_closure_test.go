package closure

import (
	"fmt"
	"regexp"
	"strings"
	"testing"

	"github.com/promise-language/promise/compiler/internal/codegen/codegentest"
)

// --- Part E: Lambda tests ---

func TestLambdaExpr(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		main() {
			f := |int x| -> x + 1;
		}
	`)
	// Lambda function has env (i8*) as first parameter
	codegentest.AssertContainsMatch(t, ir, `define i64 @\.lambda\.\d+\(i8\* %env, i64 %x\)`)
	// Lambda returned as fat pointer {fn_ptr, env_ptr}
	codegentest.AssertContains(t, ir, "insertvalue { i8*, i8* }")
}

func TestLambdaCall(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		main() {
			f := |int x| -> x + 1;
			int y = f(42);
		}
	`)
	// Should extract fn and env from fat pointer, then call with env as first arg
	codegentest.AssertContains(t, ir, "extractvalue { i8*, i8* }")
	codegentest.AssertContains(t, ir, "call i64")
}

func TestLambdaBlock(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		main() {
			f := |int x| -> int { return x * 2; };
		}
	`)
	codegentest.AssertContainsMatch(t, ir, `define i64 @\.lambda\.\d+\(i8\* %env, i64 %x\)`)
	codegentest.AssertContains(t, ir, "mul i64")
}

func TestLambdaVoid(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		main() {
			f := |int x| -> void { return; };
		}
	`)
	codegentest.AssertContainsMatch(t, ir, `define void @\.lambda\.\d+\(i8\* %env, i64 %x\)`)
}

// T1634: an expression-body lambda whose body expression is void-typed used to
// feed the void call instruction to `ret`, emitting the invalid `ret void %0`
// (which `opt` rejected with a parse error pointing at the *next* label).
func TestLambdaVoidExprBody(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		main() {
			f := |int y| -> print_line("{y}");
			f(1);
		}
	`)
	codegentest.AssertContainsMatch(t, ir, `define void @\.lambda\.\d+\(i8\* %env, i64 %y\)`)
	codegentest.AssertContains(t, ir, "ret void")
	// `ret void` takes no operand — any `ret void %…` is malformed IR.
	codegentest.AssertNotContains(t, ir, "ret void %")
}

// T1634: the vector-literal symptom is the same lambda defect, not a vector bug.
func TestLambdaVoidExprBodyInVectorLiteral(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		main() {
			v := [|int y| -> print_line("{y}")];
		}
	`)
	codegentest.AssertNotContains(t, ir, "ret void %")
}

// T1634: the void body's temps (here the interpolated string) must still be
// cleaned up on the return path — dropping the claim/cleanup would leak.
func TestLambdaVoidExprBodyCleansTemps(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		main() {
			f := |int y| -> print_line("v={y}");
			f(1);
		}
	`)
	codegentest.AssertNotContains(t, ir, "ret void %")
	codegentest.AssertContains(t, ir, "@promise_string_drop")
}

func TestLambdaVariable(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		main() {
			f := |int x| -> x + 1;
		}
	`)
	// Lambda stored as fat pointer { i8*, i8* }
	codegentest.AssertContains(t, ir, "alloca { i8*, i8* }")
	codegentest.AssertContains(t, ir, "store { i8*, i8* }")
}

// --- Lambda Capture Tests ---

func TestLambdaCaptureInt(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		main() {
			int x = 42;
			f := |int y| -> x + y;
		}
	`)
	// Env struct should be allocated via malloc
	codegentest.AssertContains(t, ir, "call i8* @pal_alloc(i64")
	// Lambda function should have env param
	codegentest.AssertContainsMatch(t, ir, `define i64 @\.lambda\.\d+\(i8\* %env, i64 %y\)`)
	// Should load captured var from env struct inside lambda
	codegentest.AssertContains(t, ir, "cap")
}

func TestLambdaCaptureMultiple(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		main() {
			int a = 1;
			int b = 2;
			f := |int x| -> a + b + x;
		}
	`)
	// Env should be allocated
	codegentest.AssertContains(t, ir, "call i8* @pal_alloc(i64")
	// Lambda should have env param
	codegentest.AssertContainsMatch(t, ir, `define i64 @\.lambda\.\d+\(i8\* %env`)
}

func TestLambdaNoCaptures(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		main() {
			f := |int x| -> x + 1;
		}
	`)
	// No malloc for env — null env pointer
	codegentest.AssertContains(t, ir, "i8* null, 1")
}

func TestLambdaCaptureCall(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		main() {
			int x = 10;
			f := |int y| -> x + y;
			int result = f(5);
		}
	`)
	// Should extract fn and env from fat pointer for indirect call
	codegentest.AssertContains(t, ir, "extractvalue { i8*, i8* }")
	codegentest.AssertContains(t, ir, "call i64")
}

func TestLambdaNestedCapture(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		main() {
			int x = 10;
			f := |int a| -> int {
				g := |int b| -> x + b;
				return g(a);
			};
		}
	`)
	// Outer lambda should also capture x (propagated from inner)
	// Both lambdas should have env params and malloc for env
	// Count lambda functions that take i8* %env and return i64
	matches := regexp.MustCompile(`define i64 @\.lambda\.\d+\(i8\* %env`).FindAllString(ir, -1)
	if len(matches) < 2 {
		t.Errorf("expected at least 2 i64 lambda functions with env, got %d", len(matches))
	}
	// Two malloc calls — one for outer lambda env, one for inner
	codegentest.AssertContains(t, ir, "call i8* @pal_alloc(i64")
}

func TestLambdaEnvFree(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		main() {
			int x = 10;
			f := |int y| -> x + y;
		}
	`)
	// Env should be freed at scope exit
	codegentest.AssertContains(t, ir, "call void @pal_free(i8*")
}

func TestLambdaEnvFreeNullCheck(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		main() {
			f := |int x| -> x + 1;
		}
	`)
	// No-capture lambda: env is null, free should have null check
	codegentest.AssertContains(t, ir, "env.free")
	codegentest.AssertContains(t, ir, "env.skip")
}

// T0100: Lambda with captures passed directly as function argument — env
// should be freed at statement end via the env temp tracking mechanism.
func TestLambdaEnvTempCleanup(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		apply(int x, (int) -> int fn) int { return fn(x); }
		do_it() {
			int captured = 42;
			int result = apply(5, |int x| -> x + captured);
		}
		main() { do_it(); }
	`)
	// The lambda has a capture (captured) so env is allocated.
	// Since the lambda is passed directly as a function argument (not stored
	// in a variable), env temp tracking should free it at statement end.
	codegentest.AssertContains(t, ir, "env.tmp.drop")
	codegentest.AssertContains(t, ir, "env.tmp.exec")
	codegentest.AssertContains(t, ir, "call void @pal_free")
}

// T0100: Lambda stored in a variable — env temp is claimed (drop flag cleared
// at runtime), so env.tmp.drop exists in IR but the runtime flag check prevents
// the actual free. The scope binding (env.free) handles cleanup instead.
func TestLambdaEnvTempClaimedForVariable(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		do_it() {
			int x = 10;
			f := |int y| -> x + y;
		}
		main() { do_it(); }
	`)
	// Lambda stored in a variable: env freed via scope binding (env.free).
	// The env temp is claimed (env.claim blocks emitted to clear drop flag).
	codegentest.AssertContains(t, ir, "env.free")
	codegentest.AssertContains(t, ir, "env.claim")
}

// T1239: a map literal whose value is a capturing closure must claim the
// closure's env temp. Map.[]= takes ~V value by move, so the map's drop owns the
// heap env; without the claim, statement-end cleanupEnvTemps double-frees it →
// segfault. genMapLit must emit env.claim blocks for the moved value (mirroring
// genArrayLit, T0741). This locks in the IR shape so a refactor can't drop it.
func TestMapLiteralClosureValueClaimsEnvTemp(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		do_it() {
			x := 5;
			m := { "k": move || -> x + 1 };
		}
		main() { do_it(); }
	`)
	// The capturing closure allocates a heap env; the map literal's []= move must
	// claim it (an env.claim block emitted to clear the temp's drop flag) so the
	// statement-end cleanup does not free what the map's drop now owns. Scope the
	// check to @do_it — env.claim appears throughout stdlib, so a whole-IR check
	// would not discriminate. Two env.claim blocks are expected: one from binding
	// the map to `m` (compares the map's instance ptr — never matches, a no-op at
	// runtime) and one from genMapLit's move claim against the closure env (the
	// fix). Without the fix only the binding claim exists → env temp survives →
	// double-free at statement-end cleanupEnvTemps.
	body := codegentest.ExtractFunction(ir, "__user.do_it")
	if body == "" {
		t.Fatal("could not extract @__user.do_it from IR")
	}
	claimBlocks := regexp.MustCompile(`(?m)^env\.claim\.\d+:`).FindAllString(body, -1)
	if len(claimBlocks) < 2 {
		t.Errorf("expected >= 2 env.claim blocks in @__user.do_it (binding claim + "+
			"genMapLit move claim), got %d:\n%s", len(claimBlocks), body)
	}
}

// T1239 multi-entry: a map literal with two capturing closures must claim BOTH
// env temps — genMapLit loops over entries and calls claimEnvTemp(valVal) per
// iteration. The second call sees two tracked env temps and emits two compare
// blocks (one per temp), producing more env.claim blocks than the single-entry
// case. Without the fix the second env temp is not claimed → double-free at
// statement-end cleanupEnvTemps when the second closure is evaluated.
func TestMapLiteralTwoClosureValuesClaimBothEnvTemps(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		do_two() {
			x := 5;
			y := 10;
			m := {
				"a": move || -> x + 1,
				"b": move || -> y + 2
			};
		}
		main() { do_two(); }
	`)
	body := codegentest.ExtractFunction(ir, "__user.do_two")
	if body == "" {
		t.Fatal("could not extract @__user.do_two from IR")
	}
	// For two capturing closures the loop produces:
	//   entry 1: claimEnvTemp(val1) with 1 envTemp in slice  → 1 env.claim block
	//   entry 2: claimEnvTemp(val2) with 2 envTemps in slice → 2 env.claim blocks
	//   binding: claimEnvTemp(mapVal) with 2 envTemps        → 2 env.claim blocks
	// Total ≥ 5 (exactly 5 for this pattern). A single-entry map produces 2.
	// We check ≥ 4 so the assertion is tight but not brittle to minor IR changes.
	claimBlocks := regexp.MustCompile(`(?m)^env\.claim\.\d+:`).FindAllString(body, -1)
	if len(claimBlocks) < 4 {
		t.Errorf("expected >= 4 env.claim blocks in @__user.do_two "+
			"(two closures × their env temp loop iterations), got %d:\n%s",
			len(claimBlocks), body)
	}
}

// T1160: a call returning a closure hands back a {fn_ptr, env_ptr} fat pointer.
// When the result is discarded, the env ptr must be registered as an env temp so
// cleanupEnvTemps frees the callee's heap env at statement end.
func TestClosureCallResultTrackedAsEnvTemp(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		make_adder(int x) () -> int { return || -> x + 1; }
		do_it() { make_adder(10); }
		main() { do_it(); }
	`)
	body := codegentest.ExtractFunction(ir, "__user.do_it")
	codegentest.AssertContains(t, body, "env.tmp.drop")
	codegentest.AssertContains(t, body, "env.tmp.exec")
}

// T1160: binding the call result transfers ownership to the variable's
// bindingFreeEnv — the env temp is claimed, not freed twice.
func TestClosureCallResultClaimedWhenBound(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		make_adder(int x) () -> int { return || -> x + 1; }
		do_it() { f := make_adder(10); }
		main() { do_it(); }
	`)
	body := codegentest.ExtractFunction(ir, "__user.do_it")
	codegentest.AssertContains(t, body, "env.claim")
	codegentest.AssertContains(t, body, "env.free")
}

// T1160/T1227: a call that may hand back a closure it does not own (closure-typed
// argument, or a receiver whose type transitively holds a closure field) must NOT
// be tracked — freeing the result would double-free the real owner.
func TestClosureCallResultAliasNotTracked(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Holder { () -> int cb; get_cb(this) () -> int { return this.cb; } }
		type Base { () -> int cb; }
		type Derived is Base { get_cb(this) () -> int { return this.cb; } }
		type VBase { int n; get_cb(this) () -> int { return || -> 1; } }
		type VDerived is VBase { () -> int cb; get_cb(this) () -> int { return this.cb; } }
		identity(() -> int f) () -> int { return f; }
		via_arg(() -> int g) { identity(g); }
		via_receiver(Holder h) { h.get_cb(); }
		via_inherited_receiver(Derived d) { d.get_cb(); }
		via_virtual_receiver(VBase b) { b.get_cb(); }
		main() {}
	`)
	// via_inherited_receiver: Derived declares no fields of its own, so the alias
	// filter must walk the parent chain (AllFields) to see Base.cb — otherwise the
	// borrowed env is freed here and double-freed by the owner's drop.
	//
	// via_virtual_receiver: VBase holds no closure at all, but it has children, so
	// the call dispatches through the vtable and can land on VDerived.get_cb, which
	// hands back a borrowed field. Every vtable-dispatched receiver is opaque here,
	// not just the structural/abstract ones.
	for _, fn := range []string{
		"__user.via_arg",
		"__user.via_receiver",
		"__user.via_inherited_receiver",
		"__user.via_virtual_receiver",
	} {
		body := codegentest.ExtractFunction(ir, fn)
		codegentest.AssertContains(t, body, "define") // guard: the body was actually found
		codegentest.AssertNotContains(t, body, "env.tmp.drop")
	}
}

// T1160: an indirect call through a closure VALUE (not a declared function) may
// hand back a closure the callee's env owns — the env's drop frees it, so tracking
// the result as a temp double-frees (`fatal: invalid free`). `call_it`'s only
// tracking candidate is `f()`, so its body must register no env temp at all.
func TestClosureValueCalleeResultNotTracked(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		call_it(() -> () -> int f) { f(); }
		main() {}
	`)
	body := codegentest.ExtractFunction(ir, "__user.call_it")
	codegentest.AssertContains(t, body, "define") // guard: the body was actually found
	codegentest.AssertNotContains(t, body, "env.tmp.drop")
}

// T1160: storing a closure call result into a field transfers ownership to the
// field — genAssignStmt claims the env temp (it previously segfaulted for a
// lambda-literal RHS, and would double-free a tracked call result). The
// overwritten field holds a named-function reference (null env, no env temp of
// its own), so the only claim in the body is the call result's.
func TestClosureResultIntoFieldClaimsEnvTemp(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Holder { () -> int cb; }
		zero() int { return 0; }
		make_adder(int x) () -> int { return || -> x + 1; }
		do_it() { h := Holder(cb: zero); h.cb = make_adder(5); }
		main() { do_it(); }
	`)
	codegentest.AssertContains(t, codegentest.ExtractFunction(ir, "__user.do_it"), "env.claim")
}

// T1160: a setter property never reaches the field-store path — genMemberAssign
// dispatches to the setter call and returns. The claim therefore lives in
// genAssignStmt alongside claimStringTemp/claimHeapTemp, otherwise the setter
// stores the env and statement-end cleanup frees it out from under the field.
func TestClosureResultIntoSetterClaimsEnvTemp(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Holder {
			() -> int slot;
			get cb () -> int { return this.slot; }
			set cb(() -> int f) { this.slot = f; }
		}
		zero() int { return 0; }
		make_adder(int x) () -> int { return || -> x + 1; }
		do_it() { h := Holder(slot: zero); h.cb = make_adder(5); }
		main() { do_it(); }
	`)
	codegentest.AssertContains(t, codegentest.ExtractFunction(ir, "__user.do_it"), "env.claim")
}

// T1160: storing a closure call result into a container element likewise claims
// the env temp.
func TestClosureResultIntoElementClaimsEnvTemp(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		zero() int { return 0; }
		make_adder(int x) () -> int { return || -> x + 1; }
		do_it() { v := [zero]; v[0] = make_adder(5); }
		main() { do_it(); }
	`)
	codegentest.AssertContains(t, codegentest.ExtractFunction(ir, "__user.do_it"), "env.claim")
}

// T1160: a getter reaches trackHeapUserTypeResult as a MemberExpr, not a CallExpr,
// so the alias filter's getter arm carries the whole classification on its own: the
// receiver is the getter's target. A getter minting a fresh closure is tracked; one
// handing back the receiver's borrowed field is not.
func TestClosureGetterResultTracking(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Maker { int base; get adder () -> int { b := this.base; return move || -> b + 1; } }
		type Holder { () -> int cb; get callback () -> int { return this.cb; } }
		fresh() { m := Maker(base: 1); m.adder; }
		aliased(Holder h) { h.callback; }
		main() { fresh(); }
	`)
	codegentest.AssertContains(t, codegentest.ExtractFunction(ir, "__user.fresh"), "env.tmp.drop")
	aliased := codegentest.ExtractFunction(ir, "__user.aliased")
	codegentest.AssertContains(t, aliased, "define") // guard: the body was actually found
	codegentest.AssertNotContains(t, aliased, "env.tmp.drop")
}

// T1229/T1160: a user-defined operator returning a closure never reaches the
// T1160 alias filter — genExpr's BinaryExpr/UnaryExpr arms hand the result to
// trackClosureOperatorResult directly, which tracks it unconditionally. That is
// sound because an operator's `this` and operand are borrowed (there is no
// call-site move syntax for `a + b`), so the returned closure is always a fresh,
// owned {fn,env} pair — never an alias of an operand. Pins that the discarded
// result is freed rather than leaked; tests/e2e/closure_env_operator_test.pr
// enforces the same at runtime via the zero-leak check.
func TestClosureOperatorResultTracked(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Box { int v; +(this, Box o) () -> int { n := this.v + o.v; return move || -> n; } }
		discard(Box a, Box b) { a + b; }
		main() {}
	`)
	body := codegentest.ExtractFunction(ir, "__user.discard")
	codegentest.AssertContains(t, body, "define") // guard: the body was actually found
	codegentest.AssertContains(t, body, "env.tmp.drop")
}

// T1160: typeMentionsSignature must see a closure nested inside an argument's or a
// receiver's container/optional/array/enum/generic-instance type, not only a bare
// `() -> int`. Each callee here hands back the caller-owned closure it was given, so
// a missed shape would register the borrowed env as a temp and free it while the
// caller's drop frees it again (`fatal: invalid free` at runtime). `OriginBox[int]`
// is the case where the type args are closure-free and only the origin's own fields
// hold one.
func TestClosureCallResultNestedAliasNotTracked(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Box[T] { T v; }
		type OriginBox[T] { T key; () -> int cb; }
		enum Slot { Cb(() -> int f), Empty, }
		type VectorHolder { (() -> int)[] cbs; first(this) () -> int { return this.cbs[0]; } }
		from_vector((() -> int)[] v) () -> int { return v[0]; }
		from_map(map[string, () -> int] m) () -> int { return m["k"]!; }
		from_optional((() -> int)? o) () -> int { return o!; }
		from_array((() -> int)[2] a) () -> int { return a[0]; }
		from_instance(Box[() -> int] b) () -> int { return b.v; }
		from_instance_origin(OriginBox[int] b) () -> int { return b.cb; }
		from_enum(Slot s) () -> int {
			match s {
				Slot.Cb(f) => { return f; },
				Slot.Empty => { return || -> 0; },
			}
		}
		via_vector((() -> int)[] v) { from_vector(v); }
		via_map(map[string, () -> int] m) { from_map(m); }
		via_optional((() -> int)? o) { from_optional(o); }
		via_array((() -> int)[2] a) { from_array(a); }
		via_instance(Box[() -> int] b) { from_instance(b); }
		via_instance_origin(OriginBox[int] b) { from_instance_origin(b); }
		via_enum(Slot s) { from_enum(s); }
		via_nested_receiver(VectorHolder h) { h.first(); }
		main() {}
	`)
	for _, fn := range []string{
		"__user.via_vector",
		"__user.via_map",
		"__user.via_optional",
		"__user.via_array",
		"__user.via_instance",
		"__user.via_instance_origin",
		"__user.via_enum",
		"__user.via_nested_receiver",
	} {
		body := codegentest.ExtractFunction(ir, fn)
		codegentest.AssertContains(t, body, "define") // guard: the body was actually found
		codegentest.AssertNotContains(t, body, "env.tmp.drop")
	}
}

// T1160: self-referential types must terminate the field walk (the `seen` set) AND,
// having terminated, report "no closure here" so a genuinely fresh result is still
// freed. Without the guard this program hangs the compiler; with a guard that
// reported `true` on revisit, both results would go untracked and leak.
func TestClosureCallResultRecursiveTypeGuard(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Node { int v; Node? next; mk(this) () -> int { n := this.v; return move || -> n + 1; } }
		enum Tree { Leaf, Branch(Tree[] children), }
		mk_from_tree(Tree t, int x) () -> int { return move || -> x + 1; }
		via_node(Node n) { n.mk(); }
		via_tree(Tree t) { mk_from_tree(t, 5); }
		main() {}
	`)
	codegentest.AssertContains(t, codegentest.ExtractFunction(ir, "__user.via_node"), "env.tmp.drop")
	codegentest.AssertContains(t, codegentest.ExtractFunction(ir, "__user.via_tree"), "env.tmp.drop")
}

// T1160: a parenthesized function NAME in callee position. Without the paren peel,
// `(make_adder)` reads as an opaque fat pointer, the call is misclassified as
// indirect, and the fresh env leaks. `(f)()` for a real closure value must still be
// left untracked — covered by the e2e file.
func TestClosureParenWrappedCalleeResultTracked(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		make_adder(int x) () -> int { return move || -> x + 1; }
		do_it() { (make_adder)(5); }
		main() { do_it(); }
	`)
	codegentest.AssertContains(t, codegentest.ExtractFunction(ir, "__user.do_it"), "env.tmp.drop")
}

// T1160: a receiver reached through a borrow has type SharedRef(T)/MutRef(T), so
// typeMentionsSignature must unwrap the ref before it can find the closure field.
// Dropping either arm frees the borrowed env here and the owner's drop frees it
// again — `fatal: invalid free` / segfault, not a silent leak.
func TestClosureRefReceiverAliasNotTracked(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Holder { () -> int cb; get_cb(this) () -> int { return this.cb; } }
		via_shared(Holder h) { Holder& r = h; r.get_cb(); }
		via_mut(Holder~ h) { h.get_cb(); }
		main() {}
	`)
	for _, fn := range []string{"__user.via_shared", "__user.via_mut"} {
		body := codegentest.ExtractFunction(ir, fn)
		codegentest.AssertContains(t, body, "define") // guard: the body was actually found
		codegentest.AssertNotContains(t, body, "env.tmp.drop")
	}
}

// T1160: a structural-interface receiver has no Named fields to walk, so the
// IsStructural arm — not the field walk — is what suppresses tracking. Today
// needsVtable also covers it (an abstract structural type always dispatches
// virtually), so this pins the shape rather than the arm in isolation.
func TestClosureStructuralReceiverAliasNotTracked(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type HasCb `+"`"+`structural { get_cb() () -> int `+"`"+`abstract; }
		type CbImpl is HasCb { () -> int cb; get_cb(this) () -> int { return this.cb; } }
		via_structural(HasCb h) { h.get_cb(); }
		main() {}
	`)
	body := codegentest.ExtractFunction(ir, "__user.via_structural")
	codegentest.AssertContains(t, body, "define") // guard: the body was actually found
	codegentest.AssertNotContains(t, body, "env.tmp.drop")
}

// T1160: typeMentionsSignature's *types.Tuple arm. The callee reads the caller's
// closure out of the tuple and hands it back, so tracking the result would free an
// env the tuple owns. Runtime coverage is blocked by T1233 (a tuple holding a
// capturing closure never drops the env at all), so this pins the arm at IR level.
func TestClosureTupleArgAliasNotTracked(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		first_of_tuple((() -> int, int) t) () -> int { (f, n) := t; return f; }
		via_tuple((() -> int, int) t) { first_of_tuple(t); }
		main() {}
	`)
	body := codegentest.ExtractFunction(ir, "__user.via_tuple")
	codegentest.AssertContains(t, body, "define") // guard: the body was actually found
	codegentest.AssertNotContains(t, body, "env.tmp.drop")
}

// T1160: typeMentionsSignature's *types.Tuple arm must fall through to `return
// false` when no element is a closure. The tuple arg here is closure-free, so the
// callee cannot be handing back one of its elements — the fresh result stays
// tracked. A tuple arm that reported `true` unconditionally would leak instead
// (the conservative-filter failure mode), which no double-free test would catch.
func TestClosureFreeTupleArgResultTracked(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		mk_from_tuple((int, string) t, int x) () -> int { return move || -> x + 1; }
		fresh() { mk_from_tuple((1, "a"), 10); }
		main() { fresh(); }
	`)
	codegentest.AssertContains(t, codegentest.ExtractFunction(ir, "__user.fresh"), "env.tmp.drop")
}

// T1160: a `this` receiver. resolvedExprType(ThisExpr) yields the owning type, so
// the alias filter classifies a self-call exactly as it does a named receiver:
// Counter holds no closure (fresh result, tracked), Holder hands back its borrowed
// `cb` (not tracked). Every other receiver test passes the receiver in from outside.
func TestClosureThisReceiverResultTracking(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Counter {
			int base;
			mk(this) () -> int { n := this.base; return move || -> n + 1; }
			fresh(this) { this.mk(); }
		}
		type Holder {
			() -> int cb;
			get_cb(this) () -> int { return this.cb; }
			get callback () -> int { return this.cb; }
			aliased(this) { this.get_cb(); }
			aliased_getter(this) { this.callback; }
		}
		main() {}
	`)
	codegentest.AssertContains(t, codegentest.ExtractFunction(ir, "Counter.fresh"), "env.tmp.drop")
	for _, fn := range []string{"Holder.aliased", "Holder.aliased_getter"} {
		body := codegentest.ExtractFunction(ir, fn)
		codegentest.AssertContains(t, body, "define") // guard: the body was actually found
		codegentest.AssertNotContains(t, body, "env.tmp.drop")
	}
}

// T1160: reassigning an existing closure local from a call result. genAssignStmt's
// IdentExpr arm has claimed env temps since the lambda-literal days, but only a
// lambda literal ever reached it — a call result now can too, and without the claim
// statement-end cleanup frees the env the variable owns. The optional-typed target
// exercises claimEnvTemp's nested-fat-pointer recursion (T0814) from the plain-assign
// caller rather than the var-decl one.
func TestClosureResultReassignedToLocalClaimsEnvTemp(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		make_adder(int x) () -> int { return move || -> x + 1; }
		to_local() { f := || -> 0; f = make_adder(5); }
		to_optional() { (() -> int)? o = none; o = make_adder(5); }
		main() { to_local(); to_optional(); }
	`)
	codegentest.AssertContains(t, codegentest.ExtractFunction(ir, "__user.to_local"), "env.claim")
	codegentest.AssertContains(t, codegentest.ExtractFunction(ir, "__user.to_optional"), "env.claim")
}

// T1239: a map literal moves its value into the map's Slot via `[]=`, so the map's
// drop owns the closure env. genMapLit claimed heap and string temps but never env
// temps, so statement-end cleanupEnvTemps freed the env the map still held —
// segfault. A capturing lambda literal has hit this since env temps existed; T1160
// widened it to closure-returning call results by tracking them at all. Both forms
// must emit the claim; genArrayLit's element path (T0741) is the model.
func TestClosureIntoMapLiteralClaimsEnvTemp(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		make_adder(int x) () -> int { return move || -> x + 1; }
		from_call() { m := { "k": make_adder(5) }; }
		from_lambda() { x := 5; m := { "k": move || -> x + 1 }; }
		main() { from_call(); from_lambda(); }
	`)
	codegentest.AssertContains(t, codegentest.ExtractFunction(ir, "__user.from_call"), "env.claim")
	codegentest.AssertContains(t, codegentest.ExtractFunction(ir, "__user.from_lambda"), "env.claim")
}

// T1160: a tuple literal moves its element into the tuple (genTupleLit's T0741
// claim), so statement-end cleanup must not free the env. The claim predates
// T1160 but only a lambda literal ever reached it; a tracked call result now does
// too. Runtime coverage is blocked by T1233 (a tuple holding a capturing closure
// never drops the env), so this pins the claim at IR level — once T1233 lands the
// e2e file gets the leak-checked version.
func TestClosureIntoTupleLiteralClaimsEnvTemp(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		make_adder(int x) () -> int { return move || -> x + 1; }
		from_call() { t := (make_adder(5), 1); }
		main() { from_call(); }
	`)
	codegentest.AssertContains(t, codegentest.ExtractFunction(ir, "__user.from_call"), "env.claim")
}

// T1160: the `false` tail of typeMentionsSignature's SharedRef/MutRef arms. The
// alias tests pin that a borrowed receiver holding a closure suppresses tracking;
// this pins the other direction — a borrowed receiver with no closure field must
// still unwrap to the Named and conclude "fresh", or the result silently leaks. A
// ref arm that reported `true` unconditionally would pass every alias test.
func TestClosureRefReceiverFreshResultTracked(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Maker { int base; mk(this) () -> int { b := this.base; return move || -> b + 1; } }
		via_shared(Maker m) { Maker& r = m; r.mk(); }
		via_mut(Maker~ m) { m.mk(); }
		main() {}
	`)
	codegentest.AssertContains(t, codegentest.ExtractFunction(ir, "__user.via_shared"), "env.tmp.drop")
	codegentest.AssertContains(t, codegentest.ExtractFunction(ir, "__user.via_mut"), "env.tmp.drop")
}

// T1160: a default method on a structural interface is generated once per concrete
// type with `selfSubst` active, so `resolvedExprType(this)` must substitute `Self`
// before the alias filter inspects the receiver. Without the substitution the
// receiver reads as the structural interface, whose `IsStructural()` arm suppresses
// tracking — the fresh result would leak in `Impl.discard_fresh`. The mirror case
// (the concrete type's closure field handed back through the interface's getter)
// must stay untracked.
func TestClosureStructuralDefaultMethodResultTracking(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Maker `+"`"+`structural {
			get base int `+"`"+`abstract;
			mk(this) () -> int { b := this.base; return move || -> b + 1; }
			discard_fresh(this) { this.mk(); }
		}
		type Impl is Maker { int n; get base int { return this.n; } }
		type Holder `+"`"+`structural {
			get cb () -> int `+"`"+`abstract;
			discard_own(this) { this.cb; }
		}
		type CbImpl is Holder { () -> int f; get cb () -> int { return this.f; } }
		main() { i := Impl(n: 1); i.discard_fresh(); c := CbImpl(f: || -> 1); c.discard_own(); }
	`)
	// extractDefine, not extractFunction: both names appear as call operands inside
	// @main, which precedes their definitions in the IR.
	codegentest.AssertContains(t, codegentest.ExtractDefine(ir, "Impl.discard_fresh"), "env.tmp.drop")
	aliased := codegentest.ExtractDefine(ir, "CbImpl.discard_own")
	codegentest.AssertContains(t, aliased, "define") // guard: the body was actually found
	codegentest.AssertNotContains(t, aliased, "env.tmp.drop")
}

// T1160: a user-defined `[]=` is a method call, not a native element store, so the
// env temp is claimed by genAssignStmt's index arm before the setter runs. The
// paired `[]` getter borrows the receiver's element and is an IndexExpr — a shape
// the alias filter leaves untracked via its `default` arm, so the borrowed env is
// never freed here.
func TestClosureUserIndexSetterClaimsEnvTemp(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Slots {
			(() -> int)[] items;
			[](this, int i) () -> int { return this.items[i]; }
			[]=(~this, int i, () -> int move f) { this.items[i] = f; }
		}
		zero() int { return 0; }
		make_adder(int x) () -> int { return move || -> x + 1; }
		do_it() { s := Slots(items: [zero]); s[0] = make_adder(5); }
		discard_getter(Slots s) { s[0]; }
		main() { do_it(); }
	`)
	codegentest.AssertContains(t, codegentest.ExtractFunction(ir, "__user.do_it"), "env.claim")
	getter := codegentest.ExtractFunction(ir, "__user.discard_getter")
	codegentest.AssertContains(t, getter, "define") // guard: the body was actually found
	codegentest.AssertNotContains(t, getter, "env.tmp.drop")
}

// T1160: a paren wrapped around the whole CALL (`(make_adder(5));`) must still free
// the fresh env. Today genExpr recurses straight through ParenExpr (expr.go), so the
// tracker only ever sees the inner CallExpr and the alias filter's own ParenExpr peel
// arm never runs — verified by deleting that arm, which leaves this test passing.
// The test therefore pins the user-visible behavior, not the arm; if paren handling
// ever moves into the tracker, the peel arm becomes load-bearing and this test starts
// guarding it. TestClosureParenWrappedCalleeResultTracked covers the other paren
// (`(make_adder)(5)`), which isClosureValueCallee genuinely does peel.
func TestClosureParenWrappedCallResultTracked(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		make_adder(int x) () -> int { return move || -> x + 1; }
		do_it() { (make_adder(5)); }
		main() { do_it(); }
	`)
	codegentest.AssertContains(t, codegentest.ExtractFunction(ir, "__user.do_it"), "env.tmp.drop")
}

// The MemberExpr target is `this`, not an external receiver — the claim has to fire
// on the mut-this field store too, or statement-end cleanup frees the env the field
// now owns.
func TestClosureResultIntoThisFieldClaimsEnvTemp(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		make_adder(int x) () -> int { return move || -> x + 1; }
		type SelfAssign {
			() -> int cb;
			install(~this) { this.cb = make_adder(5); }
		}
		main() {}
	`)
	codegentest.AssertContains(t, codegentest.ExtractFunction(ir, "SelfAssign.install"), "env.claim")
}

// T0812: reading a closure out of an owning aggregate (struct/optional field,
// container element) borrows the aggregate's heap env — the local must NOT get an
// owning env-free binding, otherwise both the local and the aggregate's drop free
// the same env (double-free / UAF). A fresh closure literal still owns its env.
func TestClosureFieldReadBorrowsEnvNoFree(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type CbHolder { () -> int cb; }
		read_field(CbHolder h) int {
			f := h.cb;
			return f();
		}
		fresh_local() int {
			s := "a" + "b";
			g := move || -> s.len;
			return g();
		}
		main() {}
	`)
	// The field-read local `f` gets no drop flag and no env-free binding —
	// it borrows h's env (h.drop frees it exactly once).
	codegentest.AssertNotContains(t, ir, "%f.dropflag")
	// A fresh closure literal still owns its env: drop flag + env.free binding.
	codegentest.AssertContains(t, ir, "%g.dropflag")
	codegentest.AssertContains(t, ir, "env.free")
}

// T0911: reassigning a closure-typed local that owns a heap env must free the
// old env before the store (the drop-old logic previously ignored the
// bindingFreeEnv / dropFlags env cleanup, leaking the old owned env). The
// reassignment must emit a guarded env-free sequence.
func TestClosureReassignFreesOldEnv(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		reassign() int {
			s := "captured";
			() -> int f = move || -> s.len + 1;
			t := "other";
			f = move || -> t.len + 2;
			return f();
		}
		main() {}
	`)
	// The reassignment emits the alias-guarded old-env free blocks.
	codegentest.AssertContains(t, ir, "reassign.env.free")
	codegentest.AssertContains(t, ir, "reassign.env.call")
	codegentest.AssertContains(t, ir, "reassign.env.merge")
}

// T0911: literal closure self-assignment (`f = f`) must take the early-return
// guard — the local keeps owning its env, so NO env-free blocks are emitted and
// the post-store clearDropFlag never runs (which would otherwise zero the env
// drop flag and leak the env at scope exit).
func TestClosureSelfAssignNoEnvFree(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		self_assign() int {
			s := "captured";
			() -> int f = move || -> s.len + 1;
			f = f;
			return f();
		}
		main() {}
	`)
	// Early return before any env-free blocks are emitted for the self-assign.
	codegentest.AssertNotContains(t, ir, "reassign.env.free")
	codegentest.AssertNotContains(t, ir, "reassign.env.call")
}

func TestNamedFuncRefThunk(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		add(int x) int { return x + 1; }
		main() {
			f := add;
			int y = f(42);
		}
	`)
	// Should generate a thunk for the named function reference
	codegentest.AssertContains(t, ir, "define i64 @.thunk.add(i8* %env, i64 %x)")
	// Fat pointer should use thunk, not raw @add
	codegentest.AssertContains(t, ir, ".thunk.add")
	// Should call through indirect call path
	codegentest.AssertContains(t, ir, "extractvalue { i8*, i8* }")
}

func TestThisCaptureInMethodLambda(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Counter {
			int count;
			make_fn() () -> int {
				return move || -> int {
					return this.count;
				};
			}
		}
		main() {
			c := Counter(count: 10);
		}
	`)
	// Method should return a fat pointer (closure)
	codegentest.AssertContains(t, ir, "define { i8*, i8* } @Counter.make_fn")
	// The lambda builds a fat pointer with env
	codegentest.AssertContains(t, ir, "insertvalue { i8*, i8* }")
}

// T0813: a struct with a closure (function-value) field reaches
// dupHeapValueFields via a non-sema-gated implicit dup path (fixed-array index
// dup here). The closure env cannot be deep-cloned, so the cloned slot must be
// nulled (zeroinitializer store) rather than left aliasing the source's env —
// otherwise both droppable owners free the same env → double-free.
func TestFixedArrayIndexNullsClosureField(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type _Cb { () -> int cb; drop(~this) {} }
		main() {
			_Cb[2] arr = [_Cb(cb: move || -> 1), _Cb(cb: move || -> 2)];
			_Cb x = arr[0];
		}
	`)
	// dupHeapValueFields: memcpy the instance, then null the closure slot.
	codegentest.AssertContains(t, ir, "call void @llvm.memcpy")
	codegentest.AssertContains(t, ir, "store { i8*, i8* } zeroinitializer, { i8*, i8* }*")
}

// B0217: Function-typed field with captured env gets synthesized drop that frees env
func TestDropSynthesizedFuncFieldEnv(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Executor {
			(int) -> void action;
		}
		main() {
			e := Executor(action: move |int x| {
				int _ = x * 2;
			});
		}
	`)
	// Executor gets a synthesized drop that null-checks and frees the closure env
	codegentest.AssertContains(t, ir, "define void @Executor.drop")
	codegentest.AssertContains(t, ir, "funcfield.env.free")
	codegentest.AssertContains(t, ir, "funcfield.env.skip")
	codegentest.AssertContains(t, ir, "call void @pal_free(") // frees env + instance
}

// B0217: Function-typed field without captures (null env) — synthesized drop with null check
func TestDropSynthesizedFuncFieldNullEnv(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Wrapper {
			() -> int getter;
		}
		main() {
			w := Wrapper(getter: || -> int { return 42; });
		}
	`)
	// Wrapper gets synthesized drop with null-check on env pointer
	codegentest.AssertContains(t, ir, "define void @Wrapper.drop")
	codegentest.AssertContains(t, ir, "funcfield.env.free")
	codegentest.AssertContains(t, ir, "funcfield.env.skip")
}

// T0741 Part B: a struct closure field with heap captures must deep-drop its
// env (drop captured values via the env's field-0 drop fn) instead of a shallow
// pal_free. emitFuncFieldEnvFree now routes through emitEnvDropOrFree, which
// emits the env.deep_drop / env.shallow_free branch.
func TestDropClosureStructFieldDeepDrops(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type CbHolder {
			() -> int cb;
		}
		make_cb(int n) CbHolder {
			s := "cap" + "tured";
			return CbHolder(cb: move || -> s.len + n);
		}
		main() {
			h := make_cb(5);
		}
	`)
	codegentest.AssertContains(t, ir, "define void @CbHolder.drop")
	codegentest.AssertContains(t, ir, "funcfield.env.free")
	// Deep drop: load the env's field-0 drop fn and call it (drops captures),
	// else fall back to pal_free. The presence of these blocks (not a bare
	// pal_free in funcfield.env.free) is the Part-B fix.
	codegentest.AssertContains(t, ir, "env.deep_drop")
	codegentest.AssertContains(t, ir, "env.shallow_free")
}

// T0739: a value-returning go-block whose trailing expression is a CAPTURING
// closure (≥1 capture → heap env struct) is the envTemps sibling of the T0686
// heapTemps bug. Edits 1–4 isolate the coroutine's env temp from the outer fn
// (fixes the WASM `%0`-is-coro.id-token compile failure — guarded by the e2e
// run, since generateIR of the closure case doesn't reproduce the `%0` misuse).
// Edit 5 teaches emitVariantFieldDrop a *types.Signature case so the
// dropped-not-awaited form (`task[() -> int] x = go { || -> base + 2 };` with
// no `<-x`) frees the closure's heap env via Task[() -> int].drop instead of
// leaking it. The `closure.env.free` block is absent on buggy master and proves
// edit 5 is wired into the Task drop path.
func TestT0739_ClosureResultDropFreesEnv(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		main() {
			base := 1;
			task[() -> int] x = go { || -> base + 2 };
		}
	`)
	// Edit 5: the dropped Task[() -> int] routes its closure result through
	// emitVariantFieldDrop's new *types.Signature case → emitEnvDropOrFree,
	// which emits the closure.env.free block. Absent on buggy master.
	codegentest.AssertContains(t, ir, "closure.env.free")
	// Positive value-path guards: the closure is still stored into G.result_ptr
	// inside the inner coroutine, and the caller allocates a real result buffer.
	codegentest.AssertContains(t, ir, "go.store_result")
	codegentest.AssertContains(t, ir, "@pal_alloc")
}

// T1105 (env sibling): `go obj.method(...)` returning a CAPTURING closure (≥1
// capture → heap env struct) exercises the envTemps isolation added alongside
// the heapTemps fix. Without isolating envTemps/envTempMap the closure's env
// temp (whose alloca lives in the inner `.goroutine.N` frame) leaked into the
// outer `.goroutine.main` coroutine, mis-serializing its coro.id token as `%0`.
func TestT1105_GoMethodClosureResultNoTokenLoad(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type W { f(this, int x) () -> int { return || -> x + 1; } }
		main() { W w = W(); t := go w.f(5); g := <-t; }
	`)
	defStart := strings.Index(ir, "define i8* @.goroutine.main(")
	if defStart < 0 {
		t.Fatal("expected a .goroutine.main coroutine definition in the IR")
	}
	body := ir[defStart:]
	if end := strings.Index(body, "\n}\n"); end >= 0 {
		body = body[:end+2]
	}
	codegentest.AssertContains(t, body, "call token @llvm.coro.id") // %0 is the token
	codegentest.AssertNotContains(t, body, "i1* %0")                // no env drop-flag load from the token
	codegentest.AssertNotContains(t, body, "i8** %0")               // no env pointer load from the token
	// Positive guards: the via-block coroutine still exists and the caller
	// allocates a real result buffer (the ViaBlock path stores into G.result_ptr
	// inline, so there is no separate `store_result:` block as in the go-block form).
	codegentest.AssertContains(t, ir, "define i8* @.goroutine.")
	codegentest.AssertContains(t, ir, "@pal_alloc")
}

// T0688: a value-returning go-block whose trailing expression is a bare
// reference to a captured BORROWED heap parameter (no outer drop binding)
// must dup the value at spawn time. Without the dup, the coroutine reads
// the param after the caller's stmt-temp has been dropped — UAF / double-free.
// The dup is emitted in the spawning function's IR (outside the coroutine),
// while the param is still valid, before the goroutine is enqueued.
func TestT0688_BareCapturedHeapParamDups(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		ngmake(string v) Task[string] {
			return go { v };
		}
		main() {
			task[string] x = ngmake("a" + "b");
			r := <-x;
		}
	`)
	// The spawning function (ngmake) dups its borrowed string param via
	// promise_string_new before passing the value to the goroutine ramp.
	// The dup IR lives between ngmake's load of v.addr and the call to
	// .goroutine.N.
	ngmakeIR := codegentest.ExtractFunction(ir, "__user.ngmake")
	codegentest.AssertContains(t, ngmakeIR, "@promise_string_new(")
	codegentest.AssertContains(t, ngmakeIR, "call i8* @.goroutine.")
}

// T0688: a value-returning go-block capturing a heap LOCAL (already owned by
// the outer scope, has a drop binding) must NOT add an extra dup — the
// existing B0354 ownership-transfer machinery handles it correctly. Adding a
// dup here would leak the original.
func TestT0688_BareCapturedLocalNoExtraDup(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		m() Task[string] {
			s := "x" + "y";
			return go { s };
		}
		main() {
			task[string] x = m();
			r := <-x;
		}
	`)
	mIR := codegentest.ExtractFunction(ir, "__user.m")
	// m allocates the concat result via promise_string_concat — that's the
	// captured local. There must NOT be an additional promise_string_new
	// for a dup of the captured local.
	codegentest.AssertContains(t, mIR, "@promise_string_concat(")
	codegentest.AssertNotContains(t, mIR, "@promise_string_new(")
}

// T0688: Vector[T] dispatch branch in dupBorrowedCaptureForResult. The
// spawning function must emit a vector dup (pal_alloc + memcpy of header +
// data) before passing the value to the goroutine — without it the awaiter
// would store the dangling vector pointer into G.result_ptr.
func TestT0688_BareCapturedVectorParamDups(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		ngvec(Vector[int] v) Task[Vector[int]] {
			return go { v };
		}
		main() {
			task[Vector[int]] x = ngvec([1, 2, 3]);
			r := <-x;
		}
	`)
	ngvecIR := codegentest.ExtractFunction(ir, "__user.ngvec")
	// dupVector emits vecdup.copy / vecdup.merge labels — the spawning
	// function must contain them before the goroutine call.
	codegentest.AssertContains(t, ngvecIR, "vecdup.copy")
	codegentest.AssertContains(t, ngvecIR, "vecdup.merge")
	codegentest.AssertContains(t, ngvecIR, "call i8* @.goroutine.")
}

// T0688: heap user type dispatch branch in dupBorrowedCaptureForResult. The
// spawning function must emit a heapdup (pal_alloc + memcpy of the instance
// plus deep-clone of any droppable sub-fields like nested strings) before
// passing the value to the goroutine.
func TestT0688_BareCapturedHeapUserTypeDups(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type T0688DupBox {
			string name;
			int value;
		}
		ngbox(T0688DupBox b) Task[T0688DupBox] {
			return go { b };
		}
		main() {
			task[T0688DupBox] x = ngbox(T0688DupBox(name: "n", value: 1));
			r := <-x;
		}
	`)
	ngboxIR := codegentest.ExtractFunction(ir, "__user.ngbox")
	// dupHeapValue emits heapdup.copy / heapdup.merge labels — the spawning
	// function must contain them before the goroutine call.
	codegentest.AssertContains(t, ngboxIR, "heapdup.copy")
	codegentest.AssertContains(t, ngboxIR, "heapdup.merge")
	codegentest.AssertContains(t, ngboxIR, "call i8* @.goroutine.")
}

// T0732: Map[K,V] dispatch branch in dupBorrowedCaptureForResult. Map/Set are
// heap user types excluded from isDroppableHeapUserType / isHeapUserNoDropPalFree
// by T0440, so the T0688 fix missed them — a bare-captured borrowed Map param
// returned from a value-block segfaulted (double-free of the dangling stmt-temp).
// The fix routes Map through dupHeapValue (memcpy + field-wise deep dup), which
// emits the heapdup.copy / heapdup.merge labels in the spawning function.
func TestT0688_BareCapturedMapParamDups(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		ngmap(Map[string, int] m) Task[Map[string, int]] {
			return go { m };
		}
		main() {
			task[Map[string, int]] x = ngmap({"a": 1});
			r := <-x;
		}
	`)
	ngmapIR := codegentest.ExtractFunction(ir, "__user.ngmap")
	codegentest.AssertContains(t, ngmapIR, "heapdup.copy")
	codegentest.AssertContains(t, ngmapIR, "heapdup.merge")
	codegentest.AssertContains(t, ngmapIR, "call i8* @.goroutine.")
}

// T0732: Set[T] dispatch branch in dupBorrowedCaptureForResult. Like Map, Set
// is excluded from the T0440-gated predicates; the fix recognizes it via
// isMapOrSetType and routes it through dupHeapValue, whose static path
// recursively deep-dups Set's nested Map[T,bool] field.
func TestT0688_BareCapturedSetParamDups(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		ngset(Set[int] s) Task[Set[int]] {
			return go { s };
		}
		main() {
			Set[int] s = Set[int]();
			s.add(1);
			task[Set[int]] x = ngset(s);
			r := <-x;
		}
	`)
	ngsetIR := codegentest.ExtractFunction(ir, "__user.ngset")
	codegentest.AssertContains(t, ngsetIR, "heapdup.copy")
	codegentest.AssertContains(t, ngsetIR, "heapdup.merge")
	codegentest.AssertContains(t, ngsetIR, "call i8* @.goroutine.")
}

func TestPALGetEnvGetCwdDefined(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		main() { }
	`)
	// PAL getenv/getcwd wrappers are always declared
	codegentest.AssertContains(t, ir, "@pal_getenv")
	codegentest.AssertContains(t, ir, "@pal_getcwd")
}

// B0226: Typeinfo should include drop_fn_ptr at field 1.
func TestTypeInfoDropFnPtr(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Droppable {
			int x;
			drop(~this) {}
		}
		main() {
			Droppable d = Droppable(x: 1);
		}
	`)
	// Typeinfo should reference the drop function
	codegentest.AssertContains(t, ir, "promise_typeinfo_Droppable")
	codegentest.AssertContains(t, ir, "@Droppable.drop")
}

// T0271: Lambda capturing Weak[T] uses envDropCallFn.
func TestLambdaEnvDropWeakCapture(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		check(Weak[int] w) bool {
			if upgraded := w.upgrade() {
				return true;
			}
			return false;
		}
		main() {
			a := Ref[int](42);
			w := a.downgrade();
			f := move || -> bool { return check(w.clone()); };
			f();
		}
	`)
	codegentest.AssertContains(t, ir, `call void @"Weak[int].drop"(`)
}

// T0554: env_drop for a captured user type with an explicit drop method calls
// T.drop$wrap (drop + pal_free) — not the bare T.drop followed by a separate
// pal_free on the instance, which would double-free.
func TestLambdaEnvDropUserTypeExplicitDropUsesWrap(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type _T554EX {
			string label;
			drop(~this) {}
		}
		main() {
			b := _T554EX(label: "x");
			f := move || -> string { return b.label; };
			f();
		}
	`)
	envDrop := codegentest.FindEnvDropContaining(ir, "_T554EX")
	if envDrop == "" {
		t.Fatal("expected an env_drop referencing _T554EX")
	}
	codegentest.AssertContains(t, envDrop, "call void @_T554EX.drop$wrap(")
	// Must NOT call _T554EX.drop directly outside the $wrap helper (that would
	// double-drop with $wrap inside).
	if strings.Contains(envDrop, "call void @_T554EX.drop(") {
		t.Errorf("env_drop should call $wrap, not bare drop:\n%s", envDrop)
	}
}

// T0554: env_drop for a captured user type with synthesized drop (no explicit
// drop, only droppable fields) calls the bare T.drop — synth drops already
// include pal_free, so calling pal_free again would double-free.
func TestLambdaEnvDropUserTypeSynthDropNoExtraPalFree(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type _T554SY {
			string label;
		}
		main() {
			b := _T554SY(label: "x");
			f := move || -> string { return b.label; };
			f();
		}
	`)
	envDrop := codegentest.FindEnvDropContaining(ir, "_T554SY")
	if envDrop == "" {
		t.Fatal("expected an env_drop referencing _T554SY")
	}
	codegentest.AssertContains(t, envDrop, "call void @_T554SY.drop(")
	// Count pal_free calls — must be exactly 1 (for the env struct itself).
	// A second pal_free on the instance would double-free with the synth drop.
	count := strings.Count(envDrop, "call void @pal_free(")
	if count != 1 {
		t.Errorf("expected exactly 1 pal_free call (for env), got %d:\n%s", count, envDrop)
	}
}

// T0554: env_drop for a captured user type with NO droppable fields and NO
// drop method falls through resolveDropFuncForTemp to palFree as the cleanup
// fn — single pal_free on the instance plus a pal_free on the env struct.
func TestLambdaEnvDropUserTypeNoDropUsesPalFree(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type _T554NO {
			int v;
			bool flag;
		}
		main() {
			p := _T554NO(v: 42, flag: true);
			f := move || -> int { return p.v; };
			f();
		}
	`)
	// This env_drop has no type-name marker (uses palFree for cleanup), so we
	// look for one that does NOT call any user T.drop, has the user-type Value
	// layout {i8*, i8*}, and has exactly 2 pal_free calls.
	idx := 0
	var envDrop string
	for idx < 500 {
		needle := fmt.Sprintf(".lambda.%d.env_drop", idx)
		if !strings.Contains(ir, "@"+needle+"(") {
			idx++
			continue
		}
		body := codegentest.ExtractFunction(ir, needle)
		// User-type value struct capture: { i8*, i8* } payload + 2 pal_free calls.
		if strings.Contains(body, "{ i8*, { i8*, i8* } }") &&
			strings.Count(body, "call void @pal_free(") == 2 {
			envDrop = body
			break
		}
		idx++
	}
	if envDrop == "" {
		t.Fatal("expected an env_drop with 2 pal_free calls on a user value capture")
	}
}

// T0554: the lambda BODY for a move-captured user type must NOT register a
// scope-exit drop on the capture local. Before the fix, the body called the
// captured type's drop (or pal_free) on the local copy, which then ran AGAIN
// in env_drop → segfault on user types, double-free on droppable fields.
func TestLambdaBodyNoDropOnMoveCapture(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type _T554BO {
			string label;
		}
		main() {
			b := _T554BO(label: "x");
			f := move || -> string { return b.label; };
			f();
		}
	`)
	// Find the lambda function (not env_drop) that has _T554BO in its body —
	// the user-defined lambda captures _T554BO.
	idx := 0
	var body string
	for idx < 500 {
		needle := fmt.Sprintf(".lambda.%d", idx)
		if !strings.Contains(ir, "@"+needle+"(") {
			idx++
			continue
		}
		fnBody := codegentest.ExtractFunction(ir, needle)
		// Skip env_drop and pick the lambda body that loads from a _T554BO
		// instance pointer.
		if !strings.Contains(fnBody, ".env_drop") &&
			strings.Contains(fnBody, "_T554BO") {
			body = fnBody
			break
		}
		idx++
	}
	if body == "" {
		t.Fatal("expected a lambda body referencing _T554BO")
	}
	// The lambda body must not invoke the captured type's drop (it would
	// duplicate env_drop's cleanup).
	if strings.Contains(body, "call void @_T554BO.drop(") ||
		strings.Contains(body, "call void @_T554BO.drop$wrap(") {
		t.Errorf("lambda body should not drop captured user type:\n%s", body)
	}
}

// T1634: the write-back that stores move-captured locals back into the env
// struct was only emitted on the lambda's fallthrough/block path. A void
// expression body terminates with its own `ret`, so without the write-back a
// `move |int y| -> c.bump(y)` silently discarded the mutation while the
// equivalent block form kept it. The reload-and-store back into the env field
// must precede the `ret void`.
func TestLambdaVoidExprBodyWritesBackMoveCapture(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type C { int n `+"`"+`value; bump(~this, int y) { this.n = this.n + y; } }
		call2((int) -> void f) { f(1); f(2); }
		main() { c := C(n: 0); call2(move |int y| -> c.bump(y)); }
	`)
	fn := extractDefineMatch(t, ir, `@\.lambda\.\d+\(i8\* %env, i64 %y\)`)
	codegentest.AssertContains(t, fn, "%c.cap")
	// Reload the mutated local and store it back through the env field pointer.
	codegentest.AssertContainsMatch(t, fn, `load %promise_C_v, %promise_C_v\* %c\.cap\n\tstore %promise_C_v %\d+, %promise_C_v\* %\d+`)
	codegentest.AssertNotContains(t, fn, "ret void %")
}

// T1634: a void expression body that move-captures a *droppable* value must
// still get an env drop function — the captured string is owned by the env and
// freed when the closure dies, not by the lambda body.
func TestLambdaVoidExprBodyMoveCapturedStringHasEnvDrop(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		sink(string s) { }
		call1((int) -> void f) { f(1); }
		main() { s := "owned string"; call1(move |int y| -> sink(s)); }
	`)
	codegentest.AssertContainsMatch(t, ir, `define void @\.lambda\.\d+\.env_drop\(i8\* %env\)`)
	envDrop := extractDefineMatch(t, ir, `@\.lambda\.\d+\.env_drop\(i8\* %env\)`)
	codegentest.AssertContains(t, envDrop, "call void @promise_string_drop")
	codegentest.AssertContains(t, envDrop, "call void @pal_free")
	codegentest.AssertNotContains(t, ir, "ret void %")
}

// T1634: a void expression body whose expression is a *method* call on a
// non-move (borrowed) capture terminates cleanly too — the void-return branch
// must not depend on the capture being by-move.
func TestLambdaVoidExprBodyBorrowedCapture(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		main() {
			int base = 10;
			f := |int y| -> print_line("{base + y}");
			f(1);
		}
	`)
	codegentest.AssertContainsMatch(t, ir, `define void @\.lambda\.\d+\(i8\* %env, i64 %y\)`)
	codegentest.AssertNotContains(t, ir, "ret void %")
}

// T1634: a no-parameter void expression-body lambda — `|| -> <void expr>` — is
// the degenerate case of the same path.
func TestLambdaVoidExprBodyNoParams(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		main() {
			() -> void f = || -> print_line("hi");
			f();
		}
	`)
	codegentest.AssertContainsMatch(t, ir, `define void @\.lambda\.\d+\(i8\* %env\)`)
	codegentest.AssertNotContains(t, ir, "ret void %")
}

// T1661: A lambda with a ~ (MutRef) parameter must use pointer-typed LLVM
// params so mutations through the pointer are visible to the caller.
func TestLambdaMutRefParamUsesPointerType(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		main() {
			int[] log = [];
			(int[]~) -> void fn = |int[]~ v| { v.push(5); };
			fn(log);
		}
	`)
	// The lambda function type should have a pointer param (i8**) for the ~ vector,
	// not a bare i8* (which would be a by-value copy).
	codegentest.AssertContainsMatch(t, ir, `define void @\.lambda\.\d+\(i8\* %env, i8\*\* %v\)`)
}

// T1661: When a lambda is defined inside a function with a ~ param of the same
// name, the lambda must NOT reference the outer function's MutRef parameter.
// The lambda's own param setup must shadow the outer's mutRefPtrs entry.
func TestLambdaMutRefPtrsSaveRestore(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		call(int[] ~ v, (int[]) -> void fn) { fn(v); }
		main() {
			int[] log = [1, 2];
			call(log, |int[] v| { print_line("{v.len}"); });
		}
	`)
	// The lambda should have a value param (i8*), not a pointer param (i8**)
	codegentest.AssertContainsMatch(t, ir, `define void @\.lambda\.\d+\(i8\* %env, i8\* %v\)`)
	// The `call` function should have a pointer param for the ~ vector
	codegentest.AssertContains(t, ir, "define void @__user.call(i8** %v,")
}

// T1661: An indirect call via a struct field holding a closure with a ~ param
// must use the MutRef calling convention (pointer param in the LLVM call type).
func TestLambdaMutRefFieldIndirectCall(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Callback { (int[]~) -> void fn; }
		main() {
			int[] log = [];
			cb := Callback(fn: |int[]~ v| { v.push(7); });
			cb.fn(log);
		}
	`)
	// The lambda function should have a pointer param for the ~ vector
	codegentest.AssertContainsMatch(t, ir, `define void @\.lambda\.\d+\(i8\* %env, i8\*\* %v\)`)
}

// T1661: A string ~ param in a lambda must use pointer passing so mutations
// (e.g. concatenation that rewrites the value struct) are visible to the caller.
func TestLambdaMutRefStringParam(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		main() {
			string s = "hello";
			(string~) -> void fn = |string~ v| { v = v + " world"; };
			fn(s);
		}
	`)
	codegentest.AssertContainsMatch(t, ir, `define void @\.lambda\.\d+\(i8\* %env, .+\* %v\)`)
}

// T1661: Nested lambdas each with their own ~ params must not interfere with
// each other's mutRefPtrs — the inner lambda saves and restores correctly.
func TestLambdaMutRefNestedLambdas(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		main() {
			int[] outer = [];
			(int[]~) -> void outer_fn = |int[]~ a| {
				a.push(1);
				int[] inner = [];
				(int[]~) -> void inner_fn = |int[]~ b| {
					b.push(2);
				};
				inner_fn(inner);
			};
			outer_fn(outer);
		}
	`)
	// Both lambdas should have pointer params for their ~ vectors
	// (regex matches two distinct lambda definitions with i8** params)
	matches := regexp.MustCompile(`define void @\.lambda\.\d+\(i8\* %env, i8\*\*`).FindAllString(ir, -1)
	if len(matches) < 2 {
		t.Fatalf("expected at least 2 lambda defs with i8** params, got %d", len(matches))
	}
}

// T1661: The rtti.go coercion skip — when a ~ param is passed at an indirect
// call site where coercion would normally apply, it must be skipped.
func TestLambdaMutRefCoercionSkip(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Base { int x; }
		type Child is Base { int y; }
		apply(Base ~ b, (Base~) -> void fn) { fn(b); }
		main() {
			c := Child(x: 1, y: 2);
			apply(c, |Base~ b| { b.x = 10; });
		}
	`)
	// Should compile without error — coercion must not attempt to wrap
	// a pointer value as an interface value struct
	codegentest.AssertContains(t, ir, "define")
}

// extractDefineMatch returns the body of the first `define ... @<pat>(...)`
// whose signature matches pat, which is a regexp over the text starting at the
// `@`. Generated lambda names carry an unpredictable counter, so they cannot be
// looked up by literal name the way extractFunction does.
func extractDefineMatch(t *testing.T, ir, pat string) string {
	t.Helper()
	re := regexp.MustCompile(`define [^\n]*` + pat + ` \{\n`)
	loc := re.FindStringIndex(ir)
	if loc == nil {
		t.Fatalf("no function definition matching %s in IR", pat)
	}
	rest := ir[loc[0]:]
	end := strings.Index(rest, "\n}\n")
	if end < 0 {
		return rest
	}
	return rest[:end+2]
}
