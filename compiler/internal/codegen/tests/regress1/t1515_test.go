package regress1

import (
	"strings"
	"testing"

	"github.com/promise-language/promise/compiler/internal/codegen/codegentest"
)

// T1515: a closure-env temp materialized while evaluating a for-in's iterable
// had STATEMENT lifetime, so the loop body's own statement-end drain freed it —
// then re-freed it on every following iteration, because the env drain (unlike
// every sibling drain site) never reset the drop flag it had just consumed.
// genForInIterable now re-homes such a temp to a scope binding, and
// cleanupEnvTempsFrom resets the flag so the drain is idempotent.
//
// Separately, an argument type that merely MENTIONED a closure used to suppress
// tracking of a closure-typed call RESULT outright, leaking the callee's env.
// argHidesForeignClosure narrows that to the arguments that can actually hide a
// closure some other owner frees.

// t1515Body extracts the body of the free function `drive` — the promotion is
// emitted in the caller, and a free function is the shape that has statement-temp
// tracking enabled.
func t1515Body(t *testing.T, ir string) string {
	t.Helper()
	body := codegentest.ExtractDefine(ir, "drive")
	if body == "" {
		body = codegentest.ExtractDefine(ir, "__user.drive")
	}
	if body == "" {
		t.Fatalf("no drive() emitted\n%s", ir)
	}
	return body
}

const t1515GeneratorSrc = `
mapped(int n, (int) -> string f) stream[string] {
  int i = 0;
  while (i < n) { yield f(i); i += 1; }
}
fwrap((int) -> string f) (int) -> string {
  string t = "-" + "F";
  return move |int k| -> "{k}{t}";
}
drive() {
  string suffix = "-" + "c";
  string acc = "";
  for s in mapped(2, fwrap(move |int k| -> "{k}{suffix}")) { acc = acc + s; }
  print_line(acc);
}
main() { drive(); }
`

const t1515DuckTypedSrc = `
type Mapper {
  int n;
  int i;
  (int) -> string f;
  next(~this) string? {
    if (this.i >= this.n) { return none; }
    string r = this.f(this.i);
    this.i = this.i + 1;
    return r;
  }
}
make_mapper(int n, (int) -> string move f) Mapper {
  return Mapper(n: n, i: 0, f: move f);
}
fwrap((int) -> string f) (int) -> string {
  string t = "-" + "F";
  return move |int k| -> "{k}{t}";
}
drive() {
  string suffix = "-" + "c";
  string acc = "";
  for s in make_mapper(2, fwrap(move |int k| -> "{k}{suffix}")) { acc = acc + s; }
  print_line(acc);
}
main() { drive(); }
`

// The intermediate lambda's env is re-homed to a scope binding: a `_forinenv`
// alloca + drop flag, freed through emitEnvFree's `env.free` diamond.
func TestT1515GeneratorIterableEnvPromotedToScopeBinding(t *testing.T) {
	body := t1515Body(t, codegentest.GenerateIR(t, t1515GeneratorSrc))
	codegentest.AssertContains(t, body, "%_forinenv")
	codegentest.AssertContains(t, body, "%_forinenv.dropflag")
	codegentest.AssertContains(t, body, "env.free")
}

// ...and no statement-end env drain is left inside the generator loop body, where
// it would land on the back-edge and free the env the coroutine frame still reads.
func TestT1515GeneratorLoopBodyHasNoEnvTempDrain(t *testing.T) {
	body := t1515Body(t, codegentest.GenerateIR(t, t1515GeneratorSrc))
	assertNoEnvDrainBetween(t, body, "gen.body", "gen.resume")
}

// The duck-typed `next()` path carries no coroutine at all and gets the same
// treatment — the promotion is a property of the for-in, not of generators.
func TestT1515DuckTypedIterableEnvPromotedToScopeBinding(t *testing.T) {
	body := t1515Body(t, codegentest.GenerateIR(t, t1515DuckTypedSrc))
	codegentest.AssertContains(t, body, "%_forinenv")
	codegentest.AssertContains(t, body, "env.free")
	assertNoEnvDrainBetween(t, body, "iter.body", "iter.update")
}

// assertNoEnvDrainBetween fails if an `env.tmp.drop` block label is defined
// anywhere between the first `startLabel`-prefixed block and the first
// `endLabel`-prefixed block that follows it — i.e. inside the loop body.
func assertNoEnvDrainBetween(t *testing.T, body, startLabel, endLabel string) {
	t.Helper()
	start := strings.Index(body, "\n"+startLabel)
	if start < 0 {
		t.Fatalf("no %s block in drive():\n%s", startLabel, body)
	}
	end := strings.Index(body[start:], "\n"+endLabel)
	if end < 0 {
		t.Fatalf("no %s block after %s in drive():\n%s", endLabel, startLabel, body)
	}
	region := body[start : start+end]
	if strings.Contains(region, "\nenv.tmp.drop") {
		t.Errorf("statement-end env drain inside the loop body (%s..%s):\n%s", startLabel, endLabel, region)
	}
}

// The statement-end env drain resets the flag it just consumed, so a drain that
// does land on a loop back-edge frees at most once. Every `env.tmp.done` block
// must carry the reset.
func TestT1515EnvTempDrainResetsDropFlag(t *testing.T) {
	body := t1515Body(t, codegentest.GenerateIR(t, `
take((int) -> string f) string { return f(1); }
drive() {
  string suffix = "-" + "c";
  int i = 0;
  while (i < 3) {
    print_line(take(move |int k| -> "{k}{suffix}"));
    i += 1;
  }
}
main() { drive(); }
`))
	done := codegentest.BlockByPrefixT0638(body, "env.tmp.done")
	if done == "" {
		t.Fatalf("no env.tmp.done block in drive():\n%s", body)
	}
	if !strings.Contains(done, "store i1 false") {
		t.Errorf("env.tmp.done does not reset the drop flag:\n%s", done)
	}
}

// A call whose only closure-mentioning argument is a TOP-LEVEL closure is no
// longer suppressed: the result env is claimed (the runtime pointer match that
// settles an identity-style hand-back) and then registered as a statement temp,
// so an inline-consumed wrapper result is freed exactly once.
func TestT1515ClosureArgCallResultIsClaimedThenTracked(t *testing.T) {
	body := t1515Body(t, codegentest.GenerateIR(t, `
fwrap((int) -> string f) (int) -> string {
  string t = "-" + "F";
  return move |int k| -> "{k}{t}";
}
plain(int n, (int) -> string f) string { return f(n); }
drive() {
  string suffix = "-" + "c";
  print_line(plain(1, fwrap(move |int k| -> "{k}{suffix}")));
}
main() { drive(); }
`))
	claim := strings.Index(body, "\nenv.claim")
	if claim < 0 {
		t.Fatalf("no env.claim block for the wrapper result in drive():\n%s", body)
	}
	if !strings.Contains(body[claim:], "\nenv.tmp.drop") {
		t.Errorf("wrapper result env is not registered as a temp after the claim:\n%s", body)
	}
}

// The narrowing is only for a top-level closure argument. An argument that merely
// MENTIONS a closure — here a vector of closures the caller owns — stays
// conservative, so the borrowed element handed back is not freed as a temp.
func TestT1515ClosureMentioningArgStaysConservative(t *testing.T) {
	// `drive` takes the vector as a borrowed param so its body contains no other
	// closure machinery — any env drain here would be the borrowed element's.
	body := t1515Body(t, codegentest.GenerateIR(t, `
first((() -> int)[] v)() -> int { return v[0]; }
mk(int x)() -> int { return move || -> x + 1; }
drive((() -> int)[] v) { first(v); }
main() { drive([mk(6)]); }
`))
	if strings.Contains(body, "\nenv.tmp.drop") {
		t.Errorf("borrowed closure element registered as an env temp:\n%s", body)
	}
}

// The narrowing is decided on the argument EXPRESSION, not on its type alone. A
// closure reached through a FIELD, through `this`, as a container ELEMENT, or as
// the result of a call that itself hands back a borrowed view is not an env temp
// and is not an IdentExpr, so neither the runtime claim nor
// emitReturnAliasCheckSubst's borrow clone can settle the identical hand-back —
// tracking the result would free the owner's env a second time. Those shapes keep
// the conservative pre-T1515 answer.
func TestT1515BorrowedClosureArgShapesStayConservative(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
type Holder {
  () -> int cb;
  (() -> int)[] cbs;
  get_cb(this) () -> int { return this.cbs[0]; }
  drive_this(this) { identity(this.cb); }
}
identity(() -> int f) () -> int { return f; }
drive_field(Holder h) { identity(h.cb); }
drive_element((() -> int)[] v) { identity(v[0]); }
drive_borrowed_call(Holder h) { identity(h.get_cb()); }
main() {}
`)
	for _, fn := range []string{
		"__user.drive_field",
		"__user.drive_element",
		"__user.drive_borrowed_call",
		"Holder.drive_this",
	} {
		body := codegentest.ExtractFunction(ir, fn)
		codegentest.AssertContains(t, body, "define") // guard: the body was actually found
		codegentest.AssertNotContains(t, body, "env.tmp.drop")
	}
}

// --- the argument filter: which shapes the narrowing admits --------------------

// t1515IdentityPrelude is the fixture the argument-filter tests share: `identity`
// hands its argument straight back, so whether the call's closure result is
// registered as an env temp is decided entirely by argHidesForeignClosure.
const t1515IdentityPrelude = `
identity(() -> int f) () -> int { return f; }
mk(int x) () -> int { return move || -> x + 1; }
`

// A closure argument arriving under a `~` mut-ref layer is still a top-level
// closure — peelRefsAndOptional strips the wrapper, so the result is tracked and
// the T1269 borrow clone keeps the caller's binding the sole owner. Without the
// peel the shape stays conservative and a genuinely fresh result leaks.
//
// (The env this particular hand-back yields is the null one T2010 substitutes;
// what is asserted here is that the result reaches the tracking site at all.)
func TestT1515MutRefClosureArgIsPeeled(t *testing.T) {
	body := codegentest.ExtractFunction(codegentest.GenerateIR(t, t1515IdentityPrelude+`
drive((() -> int) ~f) () -> int { return identity(f); }
main() {}
`), "__user.drive")
	codegentest.AssertContains(t, body, "define") // guard: the body was actually found
	codegentest.AssertContains(t, body, "alias.borrow.clone")
	codegentest.AssertContains(t, body, "env.tmp.drop")
}

// Same for a `&` shared-ref binding: `T&` is a view of the same fat pointer, so
// the argument underneath is a top-level closure. The source here is an OWNED
// local, which is correct both before and after the narrowing — this test locks
// the peel branch, it is not a pre/post guard. A `&` local over a BORROWED
// closure param is not covered because that binding is broken on its own (it
// segfaults with no call at all) and T1515 turns this variant of it into a double
// free — T2012, whose fix should re-check the IdentExpr admission.
func TestT1515SharedRefClosureArgIsPeeled(t *testing.T) {
	body := codegentest.ExtractFunction(codegentest.GenerateIR(t, t1515IdentityPrelude+`
drive() { g := mk(6); (() -> int)& r = g; print_line("{identity(r)()}"); }
main() { drive(); }
`), "__user.drive")
	codegentest.AssertContains(t, body, "define") // guard: the body was actually found
	codegentest.AssertContains(t, body, "env.tmp.drop")
}

// An argument that is a closure RETURNING a closure can hand the callee a second,
// DISTINCT closure — one the argument's own env may still free, which the runtime
// claim (a match on the argument's own env pointer) can never neutralize. Only
// the top-level closure type is admitted; this one keeps the conservative answer,
// so `outer`'s own result is not registered as a temp.
func TestT1515ClosureReturningClosureArgStaysConservative(t *testing.T) {
	body := codegentest.ExtractFunction(codegentest.GenerateIR(t, `
outer(() -> (() -> int) mk) () -> int { return mk(); }
drive() { print_line("{outer(|| -> || -> 5)()}"); }
main() { drive(); }
`), "__user.drive")
	codegentest.AssertContains(t, body, "define") // guard: the body was actually found
	codegentest.AssertNotContains(t, body, "env.tmp.drop")
}

// T1029 inside the new closure branch: in a DISCARDED statement the aliased
// source local outlives the statement and stays sole owner, so
// emitReturnAliasCheckSubst records the source's env pointer instead of clearing
// its flag and the RESULT temp's flag is cleared on a runtime match. The i8*
// stmtTemp path in expr.go cannot produce this block for a closure result (that
// path requires a bare i8*, and a closure result is a `{i8*, i8*}` struct), so a
// `discard.alias.clear` here is the closure branch's.
func TestT1515DiscardedClosureAliasClearsResultFlag(t *testing.T) {
	body := codegentest.ExtractFunction(codegentest.GenerateIR(t, t1515IdentityPrelude+`
drive() { g := mk(6); identity(g); print_line("{g()}"); }
main() { drive(); }
`), "__user.drive")
	codegentest.AssertContains(t, body, "define") // guard: the body was actually found
	codegentest.AssertContains(t, body, "discard.alias.clear")
}

// --- the promotion reaches every for-in branch, not just the generator one -----

// genForInIterable is on the for-in statement: each iterable branch evaluates
// through it, so each re-homes a closure-env temp the iterable materialized. The
// lambda here is consumed by a callee that takes no closure onward, so T1467's
// generator-argument promotion never sees the env and only the for-in re-homing
// can keep it alive.
func TestT1515EveryForInBranchPromotesIterableEnv(t *testing.T) {
	const prelude = `
counted((int) -> string f) int { return f(1).len; }
map_of(int n) map[string, int] { map[string, int] m = {:}; int i = 0; while (i < n) { m["k{i}"] = i; i += 1; } return m; }
string_of(int n) string { return "ab".repeat(n); }
array_of(int n) int[3] { int[3] a = [n, n + 1, n + 2]; return a; }
list_of(int n) int[] { int[] v = []; int i = 0; while (i < n) { v.push(i); i += 1; } return v; }
`
	for _, tc := range []struct{ name, loop string }{
		{"vector", `for x in list_of(counted(move |int k| -> "{k}{s}")) { total = total + x; }`},
		{"map", `for k, v in map_of(counted(move |int k| -> "{k}{s}")) { total = total + v; }`},
		{"string", `for c in string_of(counted(move |int k| -> "{k}{s}")) { total = total + 1; }`},
		{"range", `for i in 0..counted(move |int k| -> "{k}{s}") { total = total + i; }`},
		{"array", `for x in array_of(counted(move |int k| -> "{k}{s}")) { total = total + x; }`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := t1515Body(t, codegentest.GenerateIR(t, prelude+`
drive() {
  string s = "-" + "c";
  int total = 0;
  `+tc.loop+`
  print_line("{total}");
}
main() { drive(); }
`))
			codegentest.AssertContains(t, body, "%_forinenv")
			codegentest.AssertContains(t, body, "env.free")
		})
	}
}
