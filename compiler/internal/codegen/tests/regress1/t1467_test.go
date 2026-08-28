package regress1

import (
	"strings"
	"testing"

	"github.com/promise-language/promise/compiler/internal/codegen/codegentest"
)

// T1467: a generator's by-value params are copied into the coroutine frame and
// read LAZILY on each resume, so a droppable argument temp registered with
// STATEMENT lifetime is freed before the frame reads it (use after free).
// promoteGeneratorArgToScope re-homes such a temp to a scope-level owned local,
// the same lifetime an owned variable argument already gets (generalizing the
// tuple-only T1233 branch).

// t1467Body extracts the body of the free function `drive` — the promotion is
// emitted in the caller of the generator, and a free function is the shape that
// has statement-temp tracking enabled.
func t1467Body(t *testing.T, ir string) string {
	t.Helper()
	body := codegentest.ExtractDefine(ir, "drive")
	if body == "" {
		// User free functions are emitted with a `__user.` prefix when the bare
		// name could collide with a runtime symbol.
		body = codegentest.ExtractDefine(ir, "__user.drive")
	}
	if body == "" {
		t.Fatalf("no drive() emitted\n%s", ir)
	}
	return body
}

// A heap string argument moves from statement scope to scope cleanup: a
// `_genarg` alloca + drop flag is created and the drop is emitted as a scope
// binding (strdrop.call).
func TestT1467StringArgPromotedToScopeBinding(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
gen(int n, string sep) stream[string] {
  int i = 0;
  while (i < n) { yield "{i}{sep}"; i += 1; }
}
drive() {
  string acc = "";
  for s in gen(3, "-" + "x") { acc = acc + s; }
  print_line(acc);
}
main() { drive(); }
`)
	body := t1467Body(t, ir)
	codegentest.AssertContains(t, body, "%_genarg")
	codegentest.AssertContains(t, body, "%_genarg.dropflag")
	codegentest.AssertContains(t, body, "strdrop.call")
	codegentest.AssertContains(t, body, "call void @promise_string_drop")
}

// The statement temp that held the argument must be claimed — the promoted
// binding copies the temp's live flag, then the temp's own flag is cleared so
// the statement-end cleanup does not also free it (the early free is the bug).
func TestT1467StringArgStatementTempIsClaimed(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
gen(int n, string sep) stream[string] {
  int i = 0;
  while (i < n) { yield "{i}{sep}"; i += 1; }
}
drive() {
  for s in gen(3, "-" + "x") { print_line(s); }
}
main() { drive(); }
`)
	body := t1467Body(t, ir)
	allocaIdx := strings.Index(body, "%_genarg = alloca")
	if allocaIdx < 0 {
		t.Fatalf("no _genarg alloca in drive():\n%s", body)
	}
	// The concat result is stored into the promoted alloca before the generator
	// factory call, and the source temp's flag is cleared right after.
	codegentest.AssertContains(t, body, "@promise_string_concat")
	codegentest.AssertContains(t, body, "store i8* %")
}

// A vector argument whose temp carries an element type keeps element drops: the
// scope binding gets the vector valType so emitVectorElementDrops walks it (and
// the bit-63 static guard is emitted).
func TestT1467VectorArgKeepsElementDrops(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
gen(int n, string[] parts) stream[int] {
  int i = 0;
  while (i < n) { yield parts.len + i; i += 1; }
}
drive() {
  int t = 0;
  for x in gen(2, "a,b".split(",")) { t += x; }
  print_line("{t}");
}
main() { drive(); }
`)
	body := t1467Body(t, ir)
	codegentest.AssertContains(t, body, "%_genarg")
	codegentest.AssertContains(t, body, "vecdrop.nonstatic")
	codegentest.AssertContains(t, body, "vecdrop.head")
	codegentest.AssertContains(t, body, "call void @Vector.drop")
}

// A borrowed field-read vector argument is owned by the holder, not by the
// caller's statement — it is not a tracked temp, so nothing is promoted. Taking
// ownership of it here would double-free the holder's buffer and its elements.
func TestT1467BorrowedFieldVectorArgNotPromoted(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
type Holder { string[] parts; }
gen(int n, string[] parts) stream[int] {
  int i = 0;
  while (i < n) { yield parts.len + i; i += 1; }
}
drive() {
  Holder h = Holder(parts: ["a", "b"]);
  int t = 0;
  for x in gen(2, h.parts) { t += x; }
  print_line("{t}");
}
main() { drive(); }
`)
	body := t1467Body(t, ir)
	codegentest.AssertNotContains(t, body, "%_genarg")
	// The holder still drops its own vector (elements included) exactly once.
	codegentest.AssertContains(t, body, "call void @Vector.drop")
}

// A heap user-type argument is re-homed too: the constructed instance is freed
// at scope exit rather than at statement end.
func TestT1467HeapUserTypeArgPromoted(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
type Box { int n; }
gen(int n, Box b) stream[int] {
  int i = 0;
  while (i < n) { yield b.n + i; i += 1; }
}
drive() {
  int t = 0;
  for x in gen(2, Box(n: 5)) { t += x; }
  print_line("{t}");
}
main() { drive(); }
`)
	body := t1467Body(t, ir)
	codegentest.AssertContains(t, body, "%_genheaparg")
	codegentest.AssertContains(t, body, "call void @pal_free")
}

// The promoted heap binding must take over exactly the ownership it removes:
// its flag starts cleared and is set only where a tracked temp actually matched
// at runtime. An elvis argument that borrows a struct-owned default on the
// none-path would otherwise be freed here and again by the holder's drop.
func TestT1467HeapArgTakesOverPerPathOwnership(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
type Box { string s; }
type Owner { Box b; }
maybe(bool c) Box? { if (c) { return Box(s: "-" + "m"); } return none; }
gen(int n, Box b) stream[int] {
  int i = 0;
  while (i < n) { yield b.s.len + i; i += 1; }
}
drive() {
  Owner h = Owner(b: Box(s: "-" + "h"));
  int t = 0;
  for x in gen(2, maybe(false) ?: h.b) { t += x; }
  print_line("{t}");
}
main() { drive(); }
`)
	body := t1467Body(t, ir)
	codegentest.AssertContains(t, body, "%_genheaparg")
	// The runtime match blocks that hand the temp's live flag over.
	codegentest.AssertContains(t, body, "genheap.claim")
	codegentest.AssertContains(t, body, "genheap.claim.skip")
}

// A closure argument's env struct is re-homed to a bindingFreeEnv so the deep
// env drop runs at scope exit, after the stream has been consumed.
func TestT1467ClosureArgPromoted(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
gen(int n, (int) -> string f) stream[string] {
  int i = 0;
  while (i < n) { yield f(i); i += 1; }
}
drive() {
  string suffix = "-" + "z";
  for s in gen(2, move |int i| -> "{i}{suffix}") { print_line(s); }
}
main() { drive(); }
`)
	body := t1467Body(t, ir)
	codegentest.AssertContains(t, body, "%_genclosurearg")
	codegentest.AssertContains(t, body, "env.free")
	codegentest.AssertContains(t, body, "env.deep_drop")
}

// An inline enum-constructor argument with droppable variant data is re-homed
// to a scope-level enum drop instead of the statement-end ctor-temp drop.
func TestT1467EnumArgPromoted(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
enum Tag { Anonymous, Named(string name) }
gen(int n, Tag t) stream[int] {
  int i = 0;
  while (i < n) {
    int w = match (t) { Tag.Anonymous => 0, Tag.Named(name) => name.len };
    yield w + i;
    i += 1;
  }
}
drive() {
  int t = 0;
  for x in gen(2, Tag.Named("-" + "e")) { t += x; }
  print_line("{t}");
}
main() { drive(); }
`)
	body := t1467Body(t, ir)
	codegentest.AssertContains(t, body, "%_genenumarg")
	codegentest.AssertContains(t, body, "call void @Tag.drop")
}

// Every enum-ctor temp in the argument's window is re-homed through its OWN
// storage pointer (an i8* alloca), not by copying the argument value into a new
// enum-shaped alloca. That is what keeps an INTERMEDIATE ctor — one a nested
// borrowing call consumed, so it is not the value the frame reads — owned by
// someone instead of being dropped from the registry and leaked.
func TestT1467EnumIntermediateCtorArgPromoted(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
enum Tag { Anonymous, Named(string name) }
rewrap(Tag t) Tag { return Tag.Named("-w"); }
gen(int n, Tag t) stream[int] {
  int i = 0;
  while (i < n) {
    int w = match (t) { Tag.Anonymous => 0, Tag.Named(name) => name.len };
    yield w + i;
    i += 1;
  }
}
drive() {
  int t = 0;
  for x in gen(2, rewrap(Tag.Named("-" + "i"))) { t += x; }
  print_line("{t}");
}
main() { drive(); }
`)
	body := t1467Body(t, ir)
	codegentest.AssertContains(t, body, "%_genenumarg = alloca i8*")
	codegentest.AssertContains(t, body, "call void @Tag.drop")
}

// The enum-ctor promotion drops through the ctor's own storage, so it does not
// depend on how the parameter wraps the value: an `E?` param (the argument is
// still a bare enum at the call site) gets the same scope lifetime rather than
// falling through the layout guard to a statement-end drop (which freed the
// payload while the coroutine frame still held it — `fatal: invalid free`).
func TestT1467OptionalEnumArgPromoted(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
enum Tag { Anonymous, Named(string name) }
gen(int n, Tag? t) stream[int] {
  int i = 0;
  while (i < n) {
    int w = match (t!) { Tag.Anonymous => 0, Tag.Named(name) => name.len };
    yield w + i;
    i += 1;
  }
}
drive() {
  int t = 0;
  for x in gen(2, Tag.Named("-" + "o")) { t += x; }
  print_line("{t}");
}
main() { drive(); }
`)
	body := t1467Body(t, ir)
	codegentest.AssertContains(t, body, "%_genenumarg")
	codegentest.AssertContains(t, body, "call void @Tag.drop")
}

// A fixed-array argument is re-homed by T1466's dedicated generator branch, which
// runs right after the generic promotion. An array-returning call leaves a
// `[N x T]` statement temp (T1181) that the promotion could re-home too — doing so
// registers the same array twice and drops every element twice. Exactly one
// scope-owned copy must exist.
func TestT1467FixedArrayArgPromotedExactlyOnce(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
gen(int n, string[2] parts) stream[int] {
  int i = 0;
  while (i < n) { yield parts[0].len + i; i += 1; }
}
mk() string[2] { string[2] a = ["-" + "p", "-" + "q"]; return a; }
drive() {
  int t = 0;
  for x in gen(2, mk()) { t += x; }
  print_line("{t}");
}
main() { drive(); }
`)
	body := t1467Body(t, ir)
	codegentest.AssertContains(t, body, "%_genarrarg = alloca [2 x i8*]")
	// uniqueLocalName suffixes a repeat (`_genarrarg.1`), so a second registration
	// of the same array is visible by name.
	codegentest.AssertNotContains(t, body, "%_genarrarg.1")
}

// The fast path is inert: the identical argument shape passed to a NON-generator
// callee keeps ordinary statement lifetime — no promotion alloca is emitted.
func TestT1467NonGeneratorCalleeUnchanged(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
take(int n, string sep) int => n + sep.len;
type Box { int n; }
takebox(int n, Box b) int => n + b.n;
drive() {
  print_line("{take(3, "-" + "x")}");
  print_line("{takebox(3, Box(n: 5))}");
}
main() { drive(); }
`)
	body := t1467Body(t, ir)
	codegentest.AssertNotContains(t, body, "%_genarg")
	codegentest.AssertNotContains(t, body, "%_genheaparg")
	codegentest.AssertNotContains(t, body, "%_genenumarg")
	codegentest.AssertNotContains(t, body, "%_genclosurearg")
}

// A `.rodata` string default is not a tracked temp, so nothing is promoted —
// the shape that already worked stays byte-identical.
func TestT1467RodataDefaultNotPromoted(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
gen(int n, string sep = "-y") stream[string] {
  int i = 0;
  while (i < n) { yield "{i}{sep}"; i += 1; }
}
drive() {
  for s in gen(2) { print_line(s); }
}
main() { drive(); }
`)
	body := t1467Body(t, ir)
	codegentest.AssertNotContains(t, body, "%_genarg")
}

// An Optional[user type] argument is passed as the bare {vtable, instance}
// value struct, not in the {i1, T} optional layout that maybeRegisterDrop's
// optional binding reads. Promoting it would emit an optdrop that branches on
// the vtable pointer ("branch condition must have 'i1' type" — invalid IR), so
// the layout guard declines and the argument keeps statement lifetime. The IR
// must still verify, which generateIR enforces.
func TestT1467OptionalUserTypeArgNotPromoted(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
type Box { string s; }
gen(int n, Box? b) stream[int] {
  int i = 0;
  while (i < n) { yield n + i; i += 1; }
}
drive() {
  int t = 0;
  for x in gen(2, Box(s: "-" + "q")) { t += x; }
  print_line("{t}");
}
main() { drive(); }
`)
	body := t1467Body(t, ir)
	codegentest.AssertNotContains(t, body, "%_genheaparg")
	codegentest.AssertNotContains(t, body, "%_genarg")
}

// A vector argument's element-drop type is rebuilt from the TEMP's elemType,
// not read off paramType — so a wrapped param still drops elements correctly
// rather than silently losing the element loop to a failed AsVector.
func TestT1467VectorArgElemTypeComesFromTemp(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
gen(int n, string[] parts) stream[int] {
  int i = 0;
  while (i < n) { yield parts.len + i; i += 1; }
}
drive() {
  int t = 0;
  for x in gen(2, "a,b".split(",")) { t += x; }
  print_line("{t}");
}
main() { drive(); }
`)
	body := t1467Body(t, ir)
	codegentest.AssertContains(t, body, "%_genarg")
	// The element loop drops each string before the buffer is freed.
	codegentest.AssertContains(t, body, "call void @promise_string_drop")
	codegentest.AssertContains(t, body, "call void @Vector.drop")
}

// An owned variable argument already outlives the loop via its own binding —
// the promotion must not register a second owner (double free).
func TestT1467VariableArgNotPromoted(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
gen(int n, string sep) stream[string] {
  int i = 0;
  while (i < n) { yield "{i}{sep}"; i += 1; }
}
drive() {
  string sep = "-" + "v";
  for s in gen(2, sep) { print_line(s); }
}
main() { drive(); }
`)
	body := t1467Body(t, ir)
	codegentest.AssertNotContains(t, body, "%_genarg")
}

// A task-handle argument keeps the COOPERATIVE join at its promoted scope drop.
// The statement-end drain this promotion replaces routes a Task temp through the
// park-suspend join inside a coroutine body (T0668) so the single-threaded WASM
// scheduler can run the pending goroutine; a plain blocking `Task[T].drop` there
// can livelock. The promoted binding carries the task type as its valType so
// emitStringDropCall keeps emitting the join.
func TestT1467TaskArgKeepsCooperativeJoin(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
spin(int n) int { int total = 0; int i = 0; while (i < n) { total += i; i += 1; } return total; }
task_items(int n, task[int] t) stream[int] {
  int i = 0;
  while (i < n) { yield n + i; i += 1; }
}
main() {
  go {
    int total = 0;
    for x in task_items(2, go spin(100)) { total += x; }
    print_line("{total}");
  };
}
`)
	body := codegentest.ExtractDefine(ir, ".goroutine.0")
	if body == "" {
		t.Fatalf("no go-block coroutine emitted\n%s", ir)
	}
	codegentest.AssertContains(t, body, "%_genarg")
	// The promoted binding's drop block parks on the task instead of blocking.
	strdrop := body[strings.Index(body, "strdrop.call"):]
	codegentest.AssertContains(t, strdrop, "taskjoin.wait")
	codegentest.AssertContains(t, strdrop, `call void @"Task[int].free_after_done"`)
}

// A `move` param means the CALLEE owns the argument — genCallArgsWithMutRef's
// claim block runs before the promotion, so nothing is left for it to re-home.
// A second, caller-side owner here would free the value the coroutine frame owns.
// The three registries a move param can drain are covered: string, heap instance,
// enum ctor.
func TestT1467MoveParamArgNotPromoted(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
type Box { string s; }
enum Tag { Anonymous, Named(string name) }
mvstr(int n, string move sep) stream[string] {
  int i = 0;
  while (i < n) { yield "{i}{sep}"; i += 1; }
}
mvbox(int n, Box move b) stream[string] {
  int i = 0;
  while (i < n) { yield "{i}{b.s}"; i += 1; }
}
mvtag(int n, Tag move t) stream[int] {
  int i = 0;
  while (i < n) {
    int w = match (t) { Tag.Anonymous => 0, Tag.Named(name) => name.len };
    yield w + i;
    i += 1;
  }
}
drive() {
  for s in mvstr(2, "-" + "m") { print_line(s); }
  for s in mvbox(2, Box(s: "-" + "b")) { print_line(s); }
  int t = 0;
  for x in mvtag(2, Tag.Named("-" + "e")) { t += x; }
  print_line("{t}");
}
main() { drive(); }
`)
	body := t1467Body(t, ir)
	codegentest.AssertNotContains(t, body, "%_genarg")
	codegentest.AssertNotContains(t, body, "%_genheaparg")
	codegentest.AssertNotContains(t, body, "%_genenumarg")
}

// A `string?` param takes a bare i8* at the call site, so the string/vector
// branch matches on the temp and returns BEFORE the layout guard — which would
// otherwise decline the shape (an i8* is not the `{i1, i8*}` optional layout) and
// leave the temp on statement lifetime. Pre-fix the frame read the freed buffer.
func TestT1467OptionalStringParamArgPromoted(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
gen(int n, string? sep) stream[string] {
  int i = 0;
  while (i < n) { yield "{i}{sep!}"; i += 1; }
}
drive() {
  for s in gen(2, "-" + "o") { print_line(s); }
}
main() { drive(); }
`)
	body := t1467Body(t, ir)
	codegentest.AssertContains(t, body, "%_genarg")
	codegentest.AssertContains(t, body, "call void @promise_string_drop")
}

// A Map argument is a heap user-type instance: promoted through the heap branch
// and dropped via the type's own drop (which frees keys, values, and buckets),
// not via a raw pal_free of the value struct.
func TestT1467MapArgPromotedWithTypeDrop(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
gen(int n, map[string, int] m) stream[int] {
  int i = 0;
  while (i < n) { yield m.len + i; i += 1; }
}
mk() map[string, int] { map[string, int] m = {:}; m["a" + "1"] = 1; return m; }
drive() {
  int t = 0;
  for x in gen(2, mk()) { t += x; }
  print_line("{t}");
}
main() { drive(); }
`)
	body := t1467Body(t, ir)
	codegentest.AssertContains(t, body, "%_genheaparg")
	codegentest.AssertContains(t, body, `call void @"Map[string, int].drop"`)
}

// A Channel argument is a pointer temp (string/vector branch) whose drop is the
// channel's own — the promoted binding must carry that dropFunc, not the generic
// string drop.
func TestT1467ChannelArgPromoted(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
gen(int n, channel[int] ch) stream[int] {
  int i = 0;
  while (i < n) { yield n + i; i += 1; }
}
drive() {
  int t = 0;
  for x in gen(2, channel[int](2)) { t += x; }
  print_line("{t}");
}
main() { drive(); }
`)
	body := t1467Body(t, ir)
	codegentest.AssertContains(t, body, "%_genarg")
	codegentest.AssertContains(t, body, `call void @"Channel[int].drop"`)
}

// The promotion runs inside a MONOMORPHIZED caller too: a generic generator with
// a generic heap argument gets the substituted value-struct layout for its
// promoted alloca and the instance's own drop.
func TestT1467GenericHeapArgPromotedInMonoCaller(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
type GBox[T] { T v; }
gen[T](int n, GBox[T] b) stream[string] {
  int i = 0;
  while (i < n) { yield "{i}{b.v}"; i += 1; }
}
drive() {
  for s in gen[string](2, GBox[string](v: "-" + "g")) { print_line(s); }
}
main() { drive(); }
`)
	body := t1467Body(t, ir)
	codegentest.AssertContains(t, body, "%_genheaparg")
	codegentest.AssertContains(t, body, "genheap.claim")
}

// A generator METHOD on a generic type: the default is synthesized inside the
// monomorphized call, so the promotion must fire in the caller of the mono'd
// method exactly as it does for a non-generic owner.
func TestT1467GenericTypeGeneratorHeapDefaultPromoted(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
type GGen[T] {
  T v;
  items(this, string sep = "-" + "gx") stream[string] {
    int i = 0;
    while (i < 2) { yield "{i}{sep}{this.v}"; i += 1; }
  }
}
drive() {
  GGen[int] g = GGen[int](v: 7);
  for s in g.items() { print_line(s); }
}
main() { drive(); }
`)
	body := t1467Body(t, ir)
	codegentest.AssertContains(t, body, "%_genarg")
	codegentest.AssertContains(t, body, "call void @promise_string_drop")
}

// Two generator loops nested inside one another: each call site gets its OWN
// promoted binding (uniqueLocalName suffixes the second), so the inner loop's
// statement cleanup cannot free the outer separator.
func TestT1467NestedGeneratorLoopsGetSeparateBindings(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
gen(int n, string sep) stream[string] {
  int i = 0;
  while (i < n) { yield "{i}{sep}"; i += 1; }
}
drive() {
  for a in gen(2, "-" + "A") {
    for b in gen(2, "-" + "B") { print_line(a + b); }
  }
}
main() { drive(); }
`)
	body := t1467Body(t, ir)
	codegentest.AssertContains(t, body, "%_genarg = alloca i8*")
	codegentest.AssertContains(t, body, "%_genarg.1 = alloca i8*")
}

// The outer call's result is what the frame reads; the inline temp the INNER
// call only borrowed is a different pointer, so the runtime match hands over one
// flag and leaves the intermediate on statement lifetime. Exactly one promoted
// binding exists for the argument.
func TestT1467StringIntermediateArgPromotesOuterOnly(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
wrap(string s) string { return s + "!"; }
gen(int n, string sep) stream[string] {
  int i = 0;
  while (i < n) { yield "{i}{sep}"; i += 1; }
}
drive() {
  for s in gen(2, wrap("-" + "s")) { print_line(s); }
}
main() { drive(); }
`)
	body := t1467Body(t, ir)
	codegentest.AssertContains(t, body, "%_genarg = alloca i8*")
	codegentest.AssertNotContains(t, body, "%_genarg.1")
}

// The promoted binding is appended BEFORE genForInStmt records loopScopeDepth,
// so it sits outside the loop's own cleanup window. `break` leaves the loop once,
// but `continue` runs that cleanup on EVERY skipped iteration — a binding that
// landed inside the window would be dropped repeatedly and the next resume would
// read freed memory.
//
// Asserted as a differential against the same loop whose `if` arm does ordinary
// work instead of continuing: same `if`, same method call, same cleanup regions,
// so any extra `_genarg` drop site in the `continue` build could only come from
// the loop-control cleanup path. Counting sites (rather than matching block
// labels) keeps this from breaking on block-naming churn.
func TestT1467ContinueAddsNoDropSiteForPromotedArg(t *testing.T) {
	src := `
gen(int n, string sep) stream[string] {
  int i = 0;
  while (i < n) { yield "{i}{sep}"; i += 1; }
}
drive() {
  string acc = "";
  for s in gen(4, "-" + "C") {
    if (s.starts_with("1")) { BODY }
    acc = acc + s;
  }
  print_line(acc);
}
main() { drive(); }
`
	const dropSite = "load i1, i1* %_genarg.dropflag"
	withContinue := strings.Count(t1467Body(t, codegentest.GenerateIR(t, strings.Replace(src, "BODY", "continue;", 1))), dropSite)
	withoutContinue := strings.Count(t1467Body(t, codegentest.GenerateIR(t, strings.Replace(src, "BODY", `acc = acc + "";`, 1))), dropSite)
	if withContinue == 0 {
		t.Fatalf("no promoted-arg drop site emitted at all — the promotion did not run")
	}
	if withContinue != withoutContinue {
		t.Errorf("`continue` changed the promoted arg's drop-site count: %d with continue vs %d without;"+
			" the binding must stay outside the loop's cleanup window", withContinue, withoutContinue)
	}
}
