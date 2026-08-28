package drop3

import (
	"strings"
	"testing"

	"github.com/promise-language/promise/compiler/internal/codegen"
	"github.com/promise-language/promise/compiler/internal/codegen/codegentest"
)

// --- Variable tests ---

func TestDiscardedIntPopNoDropBlock(t *testing.T) {
	// B0196: int pop should NOT emit discard.drop block (only strings need it).
	ir := codegentest.GenerateIR(t, `
		main() {
			int[] v = [1, 2, 3];
			v.pop();
		}
	`)
	mainFn := codegentest.ExtractFunction(ir, "main")
	codegentest.AssertNotContains(t, mainFn, "discard.drop")
}

func TestAllocatorAttributes(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Foo { int x; }
		main() { f := Foo(x: 1); }
	`)
	// Underlying libc declarations still present (emitted by PAL)
	codegentest.AssertContains(t, ir, "declare noalias i8* @malloc(i64 noundef %size) nounwind willreturn")
	codegentest.AssertContains(t, ir, "declare void @free(i8* nocapture noundef %ptr) nounwind willreturn")
	codegentest.AssertContains(t, ir, "declare noalias i8* @realloc(i8* nocapture noundef %ptr, i64 noundef %size) nounwind willreturn")
	// PAL wrappers defined
	codegentest.AssertContains(t, ir, "@pal_alloc(i64 %size)")
	codegentest.AssertContains(t, ir, "@pal_free(i8* %ptr)")
	codegentest.AssertContains(t, ir, "@pal_realloc(i8* %ptr, i64 %size)")
}

// T0858: an early `return;` in main() must still run scope cleanup for heap
// locals allocated before it. The bare-return path branches to the coroutine
// final-suspend block via emitScopeCleanup — verify the string drop is emitted
// (and no bare ret void) so the early-return path can never leak.
func TestMainEarlyReturnRunsScopeCleanup(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		main() {
			s := "abcdef".repeat(50);
			if s.len > 0 { return; }
			print_line(s);
		}
	`)
	body := codegentest.ExtractDefine(ir, ".goroutine.main")
	codegentest.AssertNotContains(t, body, "ret void")
	codegentest.AssertContains(t, body, "br label %final.suspend")
	codegentest.AssertContains(t, body, "promise_string_drop")
}

// T1159: fire-and-forget `go f(...)` with a non-void heap result must DROP the
// discarded result in the coroutine body instead of running the result-store
// machinery (there is no receiver and result_ptr stays null). Contrast: the
// task-handle form stores into G.result_ptr via a `store_result:` block. Guards
// the free-function fast path (genGoCallExpr).
func TestT1159_FastPathFireAndForgetDropsResult(t *testing.T) {
	// Fire-and-forget: result discarded → body drops the string, no store block.
	ffBody := codegentest.DefBody(t, codegentest.GenerateIR(t, `
		build(int x) string { return "v{x}"; }
		main() { go build(5); }
	`), "define i8* @.goroutine.0(")
	codegentest.AssertContains(t, ffBody, "@promise_string_drop") // discarded result dropped
	codegentest.AssertNotContains(t, ffBody, "store_result:")     // no result-buffer store machinery

	// Task handle: result received → body stores it, no unconditional drop.
	taskBody := codegentest.DefBody(t, codegentest.GenerateIR(t, `
		build(int x) string { return "v{x}"; }
		main() { t := go build(5); r := <-t; }
	`), "define i8* @.goroutine.0(")
	codegentest.AssertContains(t, taskBody, "store_result:")           // result stored into G.result_ptr
	codegentest.AssertNotContains(t, taskBody, "@promise_string_drop") // body does not drop — receiver owns it
}

// T0731 (was TestT0688_DerivedTrailingNoDup): a value-returning go-block whose
// trailing expression is DERIVED from a borrowed heap param (e.g. `v + "!"`)
// reads that param ASYNCHRONOUSLY inside the coroutine — after the caller has
// already dropped its `"a"+"b"` stmt-temp. T0688 believed this was safe (the
// coroutine builds a fresh value), but the read of the freed buffer is a real
// UAF; the old test only "passed" because it used a .rodata literal (`"hi"`),
// which is never freed. T0731 generalizes the spawn-side dup to ALL borrowed
// heap captured params on the value-block path, so the coroutine reads its own
// private copy regardless of how the body routes the param into the result. The
// dup IS therefore emitted here.
func TestT0731_DerivedTrailingDups(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		ngmake(string v) Task[string] {
			return go { v + "!" };
		}
		main() {
			task[string] x = ngmake("a" + "b");
			r := <-x;
		}
	`)
	ngmakeIR := codegentest.ExtractFunction(ir, "__user.ngmake")
	// The spawning function dups its borrowed string param via
	// promise_string_new before passing the value to the goroutine ramp, so the
	// coroutine's async `v + "!"` reads the owned copy, not the freed arg temp.
	codegentest.AssertContains(t, ngmakeIR, "@promise_string_new(")
	codegentest.AssertContains(t, ngmakeIR, "call i8* @.goroutine.")
}

// T0731: the same async-read UAF triggers when the borrowed heap param is
// aliased through a goroutine-local before flowing to the trailing value
// (`s := if b { v } else { … }; s`). T0688's bare-ident detection did not fire
// (the trailing ident `s` is a coroutine-local, not a capture), so no dup was
// emitted. The generalized fix dups the borrowed param regardless of the
// trailing form.
func TestT0731_AliasedTrailingDups(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		ngmake(string v, bool b) Task[string] {
			return go {
				s := if b { v } else { "other" };
				s
			};
		}
		main() {
			task[string] x = ngmake("a" + "b", true);
			r := <-x;
		}
	`)
	ngmakeIR := codegentest.ExtractFunction(ir, "__user.ngmake")
	codegentest.AssertContains(t, ngmakeIR, "@promise_string_new(")
	codegentest.AssertContains(t, ngmakeIR, "call i8* @.goroutine.")
}

// T0688: regression guard — Copy-type param (int) must NOT trigger any
// dup. The eligibility predicate goElemNeedsBorrowedCaptureDup returns
// false for primitives, so the spawning function has zero dup overhead.
func TestT0688_CopyParamNoDup(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		ngint(int v) Task[int] {
			return go { v };
		}
		main() {
			task[int] x = ngint(42);
			r := <-x;
		}
	`)
	ngintIR := codegentest.ExtractFunction(ir, "__user.ngint")
	// No dup of any kind should be emitted (no vecdup, no heapdup, no string
	// dup). The capture is the bare i64 value, passed directly to the
	// goroutine.
	codegentest.AssertNotContains(t, ngintIR, "vecdup.copy")
	codegentest.AssertNotContains(t, ngintIR, "heapdup.copy")
	codegentest.AssertNotContains(t, ngintIR, "@promise_string_new(")
	codegentest.AssertContains(t, ngintIR, "call i8* @.goroutine.")
}

// T1196: a FIRE-AND-FORGET VALUE block — the block has a trailing value
// (goIsVoid=false) but the whole `go {…}` is a discarded statement
// (goExprFireAndForget=true). The old guard `!goIsVoid && !goExprFireAndForget`
// excluded this branch from the dup exactly as it did the void branch. T1196's
// lifted guard dups the borrowed string param here too, so the coroutine's
// async `v + "!"` reads the owned copy rather than the freed arg temp. The dup
// (promise_string_new) precedes the goroutine ramp call.
func TestT1196_FireAndForgetValueBlockBorrowedParamDups(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		emit(string v, Channel[string] out) {
			go { out.send(v + "!"); v + "?" };
		}
		main() {
			out := channel[string](capacity: 1);
			emit("a" + "b", out);
			r := <-out;
			out.close();
		}
	`)
	emitIR := codegentest.ExtractFunction(ir, "__user.emit")
	codegentest.AssertContains(t, emitIR, "@promise_string_new(")
	codegentest.AssertContains(t, emitIR, "call i8* @.goroutine.")
	newIdx := strings.Index(emitIR, "@promise_string_new(")
	rampIdx := strings.Index(emitIR, "call i8* @.goroutine.")
	if newIdx < 0 || rampIdx < 0 || newIdx > rampIdx {
		t.Fatalf("expected promise_string_new before goroutine ramp in emit:\n%s", emitIR)
	}
}

// T1197: the via-block go-call path (`go obj.method(arg)`, dispatched through
// genGoCallExprViaBlock rather than genGoBlock) must dup borrowed heap captured
// params — both the receiver and the args — at spawn time. Otherwise the
// coroutine async-reads them after the spawning function returns and drops its
// borrowed-arg stmt-temps (UAF / heap corruption). Here `b` (borrowed Box, a
// no-drop heap user type) and `v` (borrowed string) are both dup'd before the
// goroutine ramp call.
func TestT1197_ViaBlockBorrowedParamDups(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type T1197Box {
			int n;
			derive(this, string v) string { return v + "!"; }
		}
		mk(T1197Box b, string v) Task[string] {
			return go b.derive(v);
		}
		main() {
			b := T1197Box(n: 1);
			task[string] x = mk(b, "a" + "b");
			r := <-x;
		}
	`)
	mkIR := codegentest.ExtractFunction(ir, "__user.mk")
	// The borrowed string arg `v` is dup'd via promise_string_new; the borrowed
	// heap-user receiver `b` is dup'd via a heapdup — both before the goroutine
	// ramp call.
	codegentest.AssertContains(t, mkIR, "@promise_string_new(")
	codegentest.AssertContains(t, mkIR, "heapdup.copy")
	codegentest.AssertContains(t, mkIR, "call i8* @.goroutine.")
}

// T1197: a borrowed Copy param (int) passed to a via-block go-call must NOT be
// dup'd — Copy types embed their data and never alias the caller's heap.
func TestT1197_ViaBlockCopyParamNoDup(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type T1197IntBox {
			int n;
			plus(this, int v) int { return this.n + v; }
		}
		mk(T1197IntBox b, int v) Task[int] {
			return go b.plus(v);
		}
		main() {
			b := T1197IntBox(n: 1);
			task[int] x = mk(b, 41);
			r := <-x;
		}
	`)
	mkIR := codegentest.ExtractFunction(ir, "__user.mk")
	codegentest.AssertNotContains(t, mkIR, "@promise_string_new(")
	codegentest.AssertContains(t, mkIR, "call i8* @.goroutine.")
}

// T1197: an OWNED heap local passed to a via-block go-call must not get a
// second dup — B0354's ownership-transfer already hands the local to the
// coroutine. An extra dup would leak the original.
func TestT1197_ViaBlockLocalOwnedNoDoubleDup(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type T1197EchoBox {
			int n;
			echo(this, string v) string { return v; }
		}
		mk(T1197EchoBox b) Task[string] {
			s := "x" + "y";
			return go b.echo(s);
		}
		main() {
			b := T1197EchoBox(n: 1);
			task[string] x = mk(b);
			r := <-x;
		}
	`)
	mkIR := codegentest.ExtractFunction(ir, "__user.mk")
	// The owned local `s` comes from promise_string_concat and is transferred
	// as-is; there must be no extra promise_string_new dup of it.
	codegentest.AssertContains(t, mkIR, "@promise_string_concat(")
	codegentest.AssertNotContains(t, mkIR, "@promise_string_new(")
}

// T1198: the fast path `genGoCallExpr` (bare-ident free-function `go f(v)`) must
// dup a borrowed heap value param of the spawning function — the coroutine reads
// it after the caller frees its own borrowed-arg stmt-temps. Sibling of T1197
// (via-block) on a distinct code path.
func TestT1198_FastPathBorrowedStringParamDups(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		derive(string v, Channel[string] out) {
			out.send(v + "!");
		}
		spawn(string v, Channel[string] out) {
			go derive(v, out);
		}
		main() {
			ch := channel[string](capacity: 1);
			spawn("a" + "b", ch);
			r := <-ch;
		}
	`)
	spawnIR := codegentest.ExtractFunction(ir, "__user.spawn")
	// The borrowed string param `v` is dup'd via promise_string_new before the
	// goroutine ramp call.
	codegentest.AssertContains(t, spawnIR, "@promise_string_new(")
	codegentest.AssertContains(t, spawnIR, "call i8* @.goroutine.")
}

// T1198: a borrowed Copy param (int) passed to a fast-path go-call must NOT be
// dup'd — Copy types embed their data and never alias the caller's heap.
func TestT1198_FastPathCopyParamNoDup(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		consume(int v, Channel[int] out) {
			out.send(v);
		}
		spawn(int v, Channel[int] out) {
			go consume(v, out);
		}
		main() {
			ch := channel[int](capacity: 1);
			spawn(41, ch);
			r := <-ch;
		}
	`)
	spawnIR := codegentest.ExtractFunction(ir, "__user.spawn")
	codegentest.AssertNotContains(t, spawnIR, "@promise_string_new(")
	codegentest.AssertContains(t, spawnIR, "call i8* @.goroutine.")
}

// T0084: Builder is freed after callFormatToString extracts the string
func TestCallFormatToStringBuilderDrop(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Pt {
			int x;
			format!(Writer ~w) { w.write_string("pt"); }
		}
		main() { Pt p = Pt(x: 1); string s = "{p}"; }
	`)
	// After Builder.to_string, Builder.drop should be called to free the Builder
	codegentest.AssertContains(t, ir, "Builder.to_string")
	codegentest.AssertContains(t, ir, "Builder.drop")
}

// B0347: Returning `this.v[i]` where `v: Vector[string]` via a method must
// dup the inner string so the caller owns it. The return-site sets
// `dupStringFieldAccess` for Optional[string] returns; `genMethodIndex`
// consumes it when the target is a container, producing a `promise_string_new`
// call inside the method body.
func TestContainerStringIndexReturnDupsInnerString(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Bag {
			string[] v;
			drop(~this) {}
			get_value(int i) string? {
				return this.v[i];
			}
		}
		main() {
			Bag b = Bag(v: string[]());
			b.v.push("value");
			string? r = b.get_value(0);
		}
	`)
	bodyStart := `define { i1, i8* } @Bag.get_value(`
	idx := strings.Index(ir, bodyStart)
	if idx < 0 {
		t.Fatalf("Bag.get_value definition not found in IR")
	}
	bodyEnd := strings.Index(ir[idx:], "\n}\n")
	if bodyEnd < 0 {
		t.Fatalf("could not find end of Bag.get_value body")
	}
	body := ir[idx : idx+bodyEnd]
	if !strings.Contains(body, "promise_string_new") {
		t.Errorf("expected promise_string_new in Bag.get_value body (dup of inner string); got:\n%s", body)
	}
}

func TestSliceAssignVectorDropsSourceBacking(t *testing.T) {
	// B0313: After slice assign, the source vector's backing array must be freed
	// via Vector.drop before clearing its drop flag.
	ir := codegentest.GenerateIR(t, `
		main() {
			string[] src = ["hello"];
			string[] v = ["a", "b", "c"];
			v[0:1] = src;
		}
	`)
	codegentest.AssertContains(t, ir, `call void @"Vector[string].[:]=`)
	// B0313: Vector.drop must follow the [:]=  call (shallow free of src backing).
	codegentest.AssertContainsMatch(t, ir, `(?s)Vector\[string\]\.\[:\]=.*?call void @Vector\.drop`)
}

// T1233: A plain (non-generator) function's tuple-by-value param BORROWS — its
// body registers NO callee-side tuple drop flag (superseding T0406's callee-drop
// that double-freed when the caller also owned the tuple). When the arg is a
// tuple-literal TEMP with a heap field, the CALLER frees it field-wise at
// statement end via the `tuptmp.drop` block (registerTupleStmtTemp). This test
// verifies both halves: caller-side temp drop present, callee-side drop absent.
func TestT1233PlainTupleParamBorrowsCallerDropsTemp(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		take((string, int) t) {}
		caller() {
			take(("a" + "b", 1));
		}
	`)
	// Caller drops the tuple-literal temp field-wise at statement end; the heap
	// string field's drop fires there.
	codegentest.AssertContains(t, ir, "tuptmp.drop")
	codegentest.AssertContains(t, ir, "call void @promise_string_drop(")
	// The callee `take` borrows — no tuple drop flag registered in its body.
	takeBody := ir[strings.Index(ir, "@__user.take("):]
	if idx := strings.Index(takeBody, "\ndefine "); idx >= 0 {
		takeBody = takeBody[:idx]
	}
	if strings.Contains(takeBody, "t.dropflag") {
		t.Errorf("plain tuple param should borrow, but callee body registers a tuple drop flag:\n%s", takeBody)
	}
}

// T1233: tupleArgIsCallerOwnedTemp classifies a tuple arg as an owned TEMP the
// caller must drop, or a BORROW owned elsewhere. A getter returning a tuple by
// value produces a FRESH owned tuple (MemberExpr + isGetterCallExpr) → the caller
// registers a temp drop; a plain struct field read (MemberExpr, not a getter)
// BORROWS → no caller temp. This test keeps each callee's only droppable tuple
// arg on that one branch, so `tuptmp.drop` presence/absence pins the decision.
func TestT1233GetterOwnedVsFieldReadBorrow(t *testing.T) {
	// Getter-returned tuple → owned temp → caller drops it (tuptmp.drop present).
	getterIR := codegentest.GenerateIR(t, `
		type Box {
			string s;
			get pair(string, int) { return (this.s + "!", 5); }
		}
		take((string, int) t) {}
		caller() {
			b := Box(s: "hi");
			take(b.pair);
		}
	`)
	codegentest.AssertContains(t, getterIR, "tuptmp.drop")

	// Plain field read → borrow → the holder owns/drops the field, so the call
	// site registers NO caller tuple temp (no tuptmp.drop anywhere in the module).
	fieldIR := codegentest.GenerateIR(t, `
		type Holder { (string, int) t; }
		take((string, int) t) {}
		caller() {
			h := Holder(t: ("c" + "d", 2));
			take(h.t);
		}
	`)
	codegentest.AssertNotContains(t, fieldIR, "tuptmp.drop")

	// User-defined non-native `[]` returns a FRESH owned tuple (IndexExpr +
	// isUserIndexExpr) → owned temp → caller drops it (tuptmp.drop present).
	userIndexIR := codegentest.GenerateIR(t, `
		type Box {
			string s;
			[](int i)(string, int) { return (this.s + "!", i); }
		}
		take((string, int) t) {}
		caller() {
			b := Box(s: "hi");
			take(b[0]);
		}
	`)
	codegentest.AssertContains(t, userIndexIR, "tuptmp.drop")

	// An owned tuple VARIABLE passed to a borrow param is a borrow at the call
	// site (IdentExpr → false): it drops via its own bindingDropTuple, not a
	// caller statement temp — so no `tuptmp.drop` is registered for the arg.
	identIR := codegentest.GenerateIR(t, `
		take((string, int) t) {}
		caller() {
			(string, int) v = ("e" + "f", 3);
			take(v);
		}
	`)
	codegentest.AssertNotContains(t, identIR, "tuptmp.drop")
}

// T1233: A bare discarded tuple-literal statement (dropDiscardedTuple) — the
// aggregate has no owning caller variable, so genTupleLit's claimed heap fields
// would orphan. The caller registers a statement temp so the field-wise drop
// fires at statement end. This exercises dropDiscardedTuple's happy path (the
// caller-owned-temp branch + registerTupleStmtTemp) that the arg-passing tests
// don't reach.
func TestT1233BareDiscardedTupleLiteralDrops(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		caller() {
			("a" + "b", 1);
		}
	`)
	codegentest.AssertContains(t, ir, "tuptmp.drop")
	codegentest.AssertContains(t, ir, "call void @promise_string_drop(")
}

func TestVariadicParamDropAtScopeExit(t *testing.T) {
	// B0191: Variadic parameter vectors must be freed at scope exit.
	ir := codegentest.GenerateIR(t, `
		sum(...int nums) int {
			int total = 0;
			for n in nums { total += n; }
			return total;
		}
		main() {
			sum(1, 2, 3);
		}
	`)
	codegentest.AssertContains(t, ir, "call void @Vector.drop(i8*")
}

func TestVariadicMethodParamDropAtScopeExit(t *testing.T) {
	// B0191: Variadic method parameter vectors must be freed at scope exit.
	ir := codegentest.GenerateIR(t, `
		type Adder {
			int base;

			addAll(this, ...int values) int {
				return this.base;
			}
		}
		main() {
			a := Adder(base: 10);
			a.addAll(1, 2, 3);
		}
	`)
	codegentest.AssertContains(t, ir, "call void @Vector.drop(i8*")
}

// T1170: a fixed-array element of a match-borrowed payload (`a[0]` where `a`
// binds `string[N]`) escaping to an outer local (`out = a[0]`) must be cloned on
// read (genArrayIndex, driven by the dup flag genAssignStmt sets for a
// borrow-marked array-index RHS), so `out` owns an independent copy that survives
// the subject's synth enum drop.
func TestT1170ArrayElementEscapeDupsOnStore(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		enum Holder { Pair(string[2] a), Empty }
		esc() string {
			Holder h = Holder.Pair(a: ["x" + "1", "y" + "2"]);
			string out = "";
			if h is Pair(a) { out = a[0]; }
			return out;
		}
		main() { s := esc(); }
	`)
	fn := codegentest.ExtractFunction(ir, "__user.esc")
	// The escaping array element string is cloned via dupString.
	codegentest.AssertContains(t, fn, "strdup.copy")
}

// T1171: a whole fixed-Array-of-heap-user variant payload (`Row[2] value`) that
// ESCAPES `if is`/`match` (return / store-to-outer / consuming arg / constructor
// field) must be element-wise deep-cloned via dupBorrowedHeapUserPayload's Array
// branch (arrayElemNeedsEscapeDup) — otherwise the escaped [N x {vtable,instance}]
// aggregate aliases the subject's variant payload, which the subject's synth enum
// drop frees at scope exit (UAF / SIGSEGV). Each element clone lowers to a
// dupHeapValue `heapdup.copy` block, and the aggregate is rebuilt with N
// `insertvalue`s (one per element).
func TestT1171ArrayHeapUserPayloadEscapeDups(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Row { string name; }
		enum Box { Some(Row[2] value), Empty }
		esc() Row[2] {
			Box b = Box.Some(value: [Row(name: "a" + "b"), Row(name: "c" + "d")]);
			if b is Some(value) { return value; }
			return [Row(name: "x"), Row(name: "y")];
		}
		main() { r := esc(); }
	`)
	fn := codegentest.ExtractFunction(ir, "__user.esc")
	// Escaping the borrowed Array[Row] payload deep-clones each element.
	codegentest.AssertContains(t, fn, "heapdup.copy")
	// One insertvalue per array element rebuilds the cloned aggregate at the sink.
	if n := strings.Count(fn, "insertvalue"); n < 2 {
		t.Fatalf("expected >= 2 insertvalue (one per array element), got %d\n%s", n, fn)
	}
}

// T1171 over-application guard: an in-scope-only Array[heap-user] binding must
// stay a zero-copy borrow (no dup) — the subject outlives the narrowing and its
// synth enum drop frees each element exactly once. The dup is gated to explicit
// escape sites, so no `heapdup.copy` is emitted here. An over-eager dup would also
// leak.
func TestT1171ArrayHeapUserPayloadInScopeNoDup(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Row { string name; }
		enum Box { Some(Row[2] value), Empty }
		rd() int {
			Box b = Box.Some(value: [Row(name: "a" + "b"), Row(name: "c" + "d")]);
			int out = 0;
			if b is Some(value) { out = value[0].name.len; }
			return out;
		}
		main() { x := rd(); }
	`)
	fn := codegentest.ExtractFunction(ir, "__user.rd")
	codegentest.AssertNotContains(t, fn, "heapdup.copy")
}

// T1176: reading a whole fixed-Array[heap-user] struct field out by value and
// returning it (`return w.rows`) must element-wise deep-clone. genFieldAccess
// routes the array field through dupHeapFieldForEscape's array branch, which
// extractvalue/dupHeapValue/insertvalue's each element. Before the fix the
// [N x {vtable,instance}] aggregate was aliased and the owner's synth drop freed
// each element at scope exit while the returned copy still pointed in (UAF).
// The clone lowers to a per-element dupHeapValue `heapdup.copy` block.
func TestT1176ArrayHeapUserFieldEscapeDupsOnReturn(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Row { string name; }
		type Wrap { Row[2] rows; }
		esc() Row[2] {
			Wrap w = Wrap(rows: [Row(name: "de" + "ep"), Row(name: "x" + "x")]);
			return w.rows;
		}
		main() { r := esc(); }
	`)
	fn := codegentest.ExtractFunction(ir, "__user.esc")
	// Each escaping array element is deep-cloned via dupHeapValue.
	codegentest.AssertContains(t, fn, "heapdup.copy")
	// One insertvalue per element re-assembles the cloned array aggregate
	// ([2 x {vtable,instance}] — value structs are unnamed in the test harness).
	if n := strings.Count(fn, "insertvalue [2 x { i8*, i8* }]"); n < 2 {
		t.Errorf("expected >=2 insertvalue into the cloned array aggregate (one per element), got %d\n%s", n, fn)
	}
}

// T1176: a no-drop-but-pal-free element array field parity case — a heap-user
// type with only scalar fields has no synth drop but is still pal_free'd, so
// arrayElemNeedsEscapeDup's isHeapUserNoDropPalFree branch must still deep-clone each
// escaping element. A value-type element would be copied by value and route
// past arrayElemNeedsEscapeDup, so the presence of `heapdup.copy` confirms the no-drop
// heap branch is taken.
func TestT1176ArrayNoDropHeapUserFieldEscapeDups(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type P { int x; }
		type Wrap { P[2] cells; }
		esc() P[2] {
			Wrap w = Wrap(cells: [P(x: 11), P(x: 22)]);
			return w.cells;
		}
		main() { r := esc(); }
	`)
	fn := codegentest.ExtractFunction(ir, "__user.esc")
	codegentest.AssertContains(t, fn, "heapdup.copy")
}

// T1176 over-application guard: an in-scope read of a fixed-Array[heap-user]
// field element (no escape → no dup flag set) must NOT clone. This proves the
// escape dup is gated on an owning sink and in-scope borrows stay zero-copy
// (an over-eager clone would also leak).
func TestT1176ArrayHeapUserFieldInScopeNoDup(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Row { string name; }
		type Wrap { Row[2] rows; }
		rd() int {
			Wrap w = Wrap(rows: [Row(name: "de" + "ep"), Row(name: "x" + "x")]);
			return w.rows[0].name.len;
		}
		main() { x := rd(); }
	`)
	fn := codegentest.ExtractFunction(ir, "__user.rd")
	codegentest.AssertNotContains(t, fn, "heapdup.copy")
}

// T1176 gate negative: escaping a value-element array field (`int[2]`) out of a
// DROPPABLE owner must NOT clone — arrayElemNeedsEscapeDup recognizes the array but its
// element is neither a droppable heap-user nor a no-drop-pal-free type, so it
// returns false (the `int[]`/value-array fall-through) and the field is copied
// by value. The owner is made droppable by its sibling string field, so the
// escape sink runs setDupFlagsForFieldAccess → arrayElemNeedsEscapeDup for real; the
// absence of `heapdup.copy` proves value arrays are left untouched.
func TestT1176ValueArrayFieldEscapeNoDup(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type VW { int[2] a; string s; }
		esc() int[2] {
			VW w = VW(a: [7, 8], s: "x" + "y");
			return w.a;
		}
		main() { r := esc(); }
	`)
	fn := codegentest.ExtractFunction(ir, "__user.esc")
	codegentest.AssertNotContains(t, fn, "heapdup.copy")
}

// T0137: String embed getter result assigned to variable — drop flag must NOT be cleared.
func TestEmbedStringGetterDrop(t *testing.T) {
	file, info := codegentest.ParseWithStd(t, `
		get greeting string `+"`embed(\"greeting.txt\")"+`;
		test_it() `+"`test"+` {
			string a = greeting;
		}
	`)
	for _, embed := range info.Embeds {
		embed.Data = []byte("hello")
	}
	result := codegen.Compile(file, info, "")
	ir := result.Module.String()
	// The variable should have a string drop at scope exit.
	codegentest.AssertContains(t, ir, "@promise_string_drop")
	// The drop flag must NOT be cleared to false immediately after being set to true.
	// Before the fix, the IR had: store i1 true, %a.dropflag; store i1 false, %a.dropflag
	codegentest.AssertNotContains(t, ir, "store i1 true, i1* %a.dropflag\n\tstore i1 false, i1* %a.dropflag")
}

// T0137: Bytes embed getter result assigned to variable — drop flag must NOT be cleared.
func TestEmbedBytesGetterDrop(t *testing.T) {
	file, info := codegentest.ParseWithStd(t, `
		get binary u8[] `+"`embed(\"data.bin\")"+`;
		test_it() `+"`test"+` {
			u8[] d = binary;
		}
	`)
	for _, embed := range info.Embeds {
		embed.Data = []byte{0xDE, 0xAD}
	}
	result := codegen.Compile(file, info, "")
	ir := result.Module.String()
	// The variable should have a vector drop at scope exit.
	codegentest.AssertContains(t, ir, "@Vector.drop")
	// The drop flag must NOT be cleared to false immediately after being set to true.
	codegentest.AssertNotContains(t, ir, "store i1 true, i1* %d.dropflag\n\tstore i1 false, i1* %d.dropflag")
}

// B0175: Heap temp claim on method receiver in chained calls
func TestHeapTempClaimOnMethodReceiver(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Counter is Iterator[int] {
			int n;
			next(~this) int? {
				if this.n <= 0 { return none; }
				this.n = this.n - 1;
				return this.n;
			}
		}
		test() {
			c := Counter(n: 100);
			Iterator[int] result = c.filter(|int x| -> bool { return x % 2 == 0; }).take(3);
			int sum = 0;
			for x in result {
				sum = sum + x;
			}
		}
		main() {}
	`)
	// The chained call c.filter(...).take(3) must claim the filter result
	// (intermediate heap temp) before calling .take(3) on it.
	// Both the filter result and the take result get heap.claim blocks.
	codegentest.AssertContains(t, ir, "heap.claim")
}

// B0187: Reassignment of structural interface variable must claim heap temp
func TestHeapTempClaimOnReassignment(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Counter is Iterator[int] {
			int n;
			next(~this) int? {
				if this.n <= 0 { return none; }
				this.n = this.n - 1;
				return this.n;
			}
		}
		test() {
			c := Counter(n: 10);
			Iterator[int] it = c.take(5);
			it = c.take(3);
			int sum = 0;
			for x in it {
				sum = sum + x;
			}
		}
		main() {}
	`)
	// The reassignment `it = c.take(3)` must generate a heap.claim block
	// to prevent the new iterator instance from being double-freed
	// (once at statement end via cleanupHeapTemps, again at scope exit via emitFreeCall).
	codegentest.AssertContains(t, ir, "heap.claim")
}

// B0211: Discarded constructor call for a heap user type without drop should emit pal_free.
func TestDropDiscardedHeapTypeConstructor(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Pt {
			int x;
			int y;
		}
		test() {
			Pt(x: 1, y: 2);
		}
	`)
	codegentest.AssertContains(t, ir, "discard.heap.free")
	codegentest.AssertContains(t, ir, "call void @pal_free")
}

// B0211: Discarded constructor call for a heap user type WITH drop should call drop.
func TestDropDiscardedHeapTypeConstructorWithDrop(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Res {
			int id;
			drop(~this) {}
		}
		test() {
			Res(id: 1);
		}
	`)
	codegentest.AssertContains(t, ir, "discard.heap.free")
	codegentest.AssertContains(t, ir, "call void @Res.drop")
}

// B0211: Discarded method call returning a heap type should NOT emit discard.heap.free
// (only constructor calls are safe to free — method returns may share instance pointers).
func TestNoDropDiscardedMethodReturnHeapType(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Pt {
			int x;
			int y;
		}
		make_pt() Pt {
			return Pt(x: 1, y: 2);
		}
		test() {
			make_pt();
		}
	`)
	testFn := codegentest.ExtractFunction(ir, "__user.test")
	if strings.Contains(testFn, "discard.heap.free") {
		t.Fatalf("expected test function to NOT contain discard.heap.free for method return\ngot:\n%s", testFn)
	}
}

// B0211: String temp should NOT be claimed when constructor will strdup (NeedsSynthDrop).
func TestStringTempNotClaimedForSynthDrop(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Named {
			string name;
			new(~this, string name) {
				this.name = name;
			}
		}
		make_name() string {
			return "hello";
		}
		test() {
			Named n = Named(name: make_name());
		}
	`)
	// The string from make_name() should be freed (not claimed by constructor).
	// Look for promise_string_drop in the test function — indicates the temp is freed.
	codegentest.AssertContains(t, ir, "promise_string_drop")
}

// B0233: Constructor temps passed to non-~ methods should be freed at statement end.
// The constructor should NOT claim the heap temp — only downstream consumers should.
func TestConstructorTempFreedAtStmtEnd(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Point { int x; int y; }
		check(Point p) bool { return p.x == 0; }
		main() {
			check(Point(x: 0, y: 0));
		}
	`)
	// The Point(x: 0, y: 0) temp should be freed at statement end via heap.drop,
	// NOT claimed by the constructor. The heap.drop block calls pal_free on the
	// unclaimed temp.
	codegentest.AssertContains(t, ir, "heap.drop")
	codegentest.AssertContains(t, ir, "heap.exec")
}

// B0233: Constructor temps assigned to variables should still be claimed.
func TestConstructorTempClaimedOnAssign(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Point { int x; int y; }
		main() {
			Point p = Point(x: 1, y: 2);
		}
	`)
	// Variable assignment claims the heap temp — heap.claim block should exist.
	codegentest.AssertContains(t, ir, "heap.claim")
}

// B0233: Constructor temps passed to vector push should be claimed.
func TestConstructorTempClaimedOnPush(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Point { int x; int y; }
		main() {
			Point[] v = [Point(x: 1, y: 2)];
			v.push(Point(x: 3, y: 4));
		}
	`)
	// Push claims the heap temp — heap.claim block should exist.
	codegentest.AssertContains(t, ir, "heap.claim")
}

// B0237: Constructor temps passed as map literal values should be claimed.
func TestConstructorTempClaimedInMapLiteral(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Point { int x; int y; }
		main() {
			map[string, Point] m = { "a": Point(x: 1, y: 2) };
		}
	`)
	// Map literal initialization claims the heap temp.
	codegentest.AssertContains(t, ir, "heap.claim")
}

// B0280: Map literal with identifier key must clear key's drop flag too.
func TestMapLitClearsDropFlagOnKey(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		main() {
			string k = "mykey";
			map[string, int] m = { k: 42 };
		}
	`)
	// k is a string variable with a drop flag — it should be cleared
	codegentest.AssertContains(t, ir, "k.dropflag")
	codegentest.AssertContains(t, ir, "store i1 false, i1* %k.dropflag")
}

// T0736: A map literal whose value is a bare heap sub-expression (string concat,
// to_string(), split(), ...) registers a string/vector *stmt-temp*, not a heap
// temp. genMapLit must claim that stmt-temp (clear its drop flag) after the []=
// move — otherwise the caller's stmt-temp cleanup drops the string while the
// map's scope-exit drop drops it again → double-free ("invalid free"). The
// ident-`clearDropFlag` (B0280) and `claimHeapTemp` paths don't cover stmt-temps.
func TestT0736MapLitClaimsHeapStringValueTemp(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		main() {
			map[string, string] m = {"k": "a" + "b"};
		}
	`)
	// Scope to the user's main goroutine body — the stdlib also defines and
	// calls Map[string, string].[]= elsewhere. The call site of .goroutine.main
	// (in the entry main) precedes its definition, so locate the definition
	// directly rather than via extractFunction.
	start := strings.Index(ir, "define i8* @.goroutine.main")
	if start < 0 {
		t.Fatal("expected .goroutine.main")
	}
	rest := ir[start:]
	end := strings.Index(rest, "\n}\n")
	if end < 0 {
		t.Fatal("expected closing brace")
	}
	body := rest[:end+2]
	codegentest.AssertContains(t, body, "call i8* @promise_string_concat")
	// The concat value is moved into the map via []=, then its stmt-temp drop
	// flag is cleared (claimed) — the []= call is immediately followed by a
	// flag-clearing store. Without the fix the next line is instead the
	// `store { i8*, i8* } ... %m` map binding (no claim), and this fails.
	codegentest.AssertContainsMatch(t, body,
		`(?s)call void @"Map\[string, string\]\.\[\]="\([^\n]*\)\n\s*store i1 false, i1\*`)
}

// T0735: A map literal used directly as a borrowed function argument must
// register a stmt-temp drop binding so the map instance + its _buckets vector
// are freed at statement end. Pre-fix, genMapConstructor never called
// trackHeapTemp, so unclaimed map literal temps leaked 2 allocations.
// The fix mirrors genConstructorCallMono (T0135 + T0345): trackHeapTemp with
// palFree as the safe default, then updateConstructorTempDrop swaps in the
// type's full synth drop after new() completes.
func TestT0735_MapLitArgTracksHeapTemp(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		borrow_map(Map[string, int] m) int { return 0; }
		main() {
			int x = borrow_map({"a": 1, "b": 2});
		}
	`)
	start := strings.Index(ir, "define i8* @.goroutine.main")
	if start < 0 {
		t.Fatal("expected .goroutine.main")
	}
	rest := ir[start:]
	end := strings.Index(rest, "\n}\n")
	if end < 0 {
		t.Fatal("expected closing brace")
	}
	body := rest[:end+2]
	// After the borrow_map call, the unclaimed map temp must flow through a
	// heap.drop block that calls Map[string, int].drop (the synthesized drop
	// walks _buckets and pal_frees the instance — without the swap, just a
	// pal_free of the instance would leak the buckets vector buffer).
	codegentest.AssertContains(t, body, "call i64 @__user.borrow_map(")
	codegentest.AssertContains(t, body, "heap.drop")
	codegentest.AssertContains(t, body, "heap.exec")
	codegentest.AssertContains(t, body, `call void @"Map[string, int].drop"`)
}

// T0735: A map literal used as a method-call receiver (rvalue temp) — same
// stmt-temp drop registration must apply. `{...}.len` returns a primitive but
// the receiver map still needs cleanup.
func TestT0735_MapLitMethodReceiverTracksHeapTemp(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		main() {
			int n = {"a": 1, "b": 2}.len;
		}
	`)
	start := strings.Index(ir, "define i8* @.goroutine.main")
	if start < 0 {
		t.Fatal("expected .goroutine.main")
	}
	rest := ir[start:]
	end := strings.Index(rest, "\n}\n")
	if end < 0 {
		t.Fatal("expected closing brace")
	}
	body := rest[:end+2]
	codegentest.AssertContains(t, body, `call i64 @"Map[string, int].len"`)
	codegentest.AssertContains(t, body, "heap.drop")
	codegentest.AssertContains(t, body, `call void @"Map[string, int].drop"`)
}

// T0735: Map literal bound to a local first must still work — the local's
// regular bindingDrop (registered by genAssignment) handles the cleanup, and
// claimHeapTemp at the assignment site clears the heap-temp flag so the
// instance isn't double-freed. Verifies the existing local-binding path is
// undisturbed by the new stmt-temp registration.
func TestT0735_MapLitLocalStillDropped(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		main() {
			map[string, int] m = {"a": 1, "b": 2};
		}
	`)
	start := strings.Index(ir, "define i8* @.goroutine.main")
	if start < 0 {
		t.Fatal("expected .goroutine.main")
	}
	rest := ir[start:]
	end := strings.Index(rest, "\n}\n")
	if end < 0 {
		t.Fatal("expected closing brace")
	}
	body := rest[:end+2]
	// The local m's drop still fires (via bindingDrop), so Map.drop must
	// appear in the body. The heap-temp flag should also be cleared at the
	// assignment site so the heap.drop path doesn't double-free.
	codegentest.AssertContains(t, body, `call void @"Map[string, int].drop"`)
	codegentest.AssertContains(t, body, "heap.claim")
}

// T0735: A map literal passed to a `~Map` parameter (consume-arg) must be
// claimed at the call site — the callee owns and drops, so the caller's
// heap-temp drop flag must be cleared to avoid double-free. Verifies the
// claim path on the move-arg ABI.
func TestT0735_MapLitMoveArgClaimed(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		consume(Map[string, int] move m) int { return m.len; }
		main() {
			int x = consume({"a": 1});
		}
	`)
	start := strings.Index(ir, "define i8* @.goroutine.main")
	if start < 0 {
		t.Fatal("expected .goroutine.main")
	}
	rest := ir[start:]
	end := strings.Index(rest, "\n}\n")
	if end < 0 {
		t.Fatal("expected closing brace")
	}
	body := rest[:end+2]
	codegentest.AssertContains(t, body, "call i64 @__user.consume(")
	// claim must fire before the call returns so the heap.drop at statement
	// end sees flag=false; callee runs Map.drop itself.
	codegentest.AssertContains(t, body, "heap.claim")
}

// T0610: A vector literal whose element is a moved local variable of a type
// Vector.drop's element-walk frees (heap-user-with-drop, string, droppable
// enum, Mutex/Task, nested vector) must clear the source ident's drop flag —
// otherwise the source variable's scope-exit drop AND Vector.drop's element
// walk free the same allocation (double-free / SEGV). Mirrors genTupleLit
// (B0242) / genMapLit (B0280).
func TestVectorLitMoveFromVarClearsDropFlag(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type _Box { string label; drop(~this) {} }
		main() {
			_Box b0 = _Box(label: "a");
			_Box[] v = [b0];
		}
	`)
	// b0 is moved into the vector literal — its drop flag must be cleared so
	// Vector.drop's element walk becomes the sole owner.
	codegentest.AssertContains(t, ir, "store i1 false, i1* %b0.dropflag")
}

// T0610: a droppable tuple bound to a *variable* and moved into a vector
// literal must clear the source ident's drop flag — exercising the
// tupleNeedsDrop arm of the type-gate. Existing tuple-in-vector-literal
// tests only use inline tuple constructors (no ident move), so this is the
// sole IR coverage of the tupleNeedsDrop branch. Without the clear, the
// tuple field is freed by both the var's bindingDropTuple and Vector.drop's
// element walk → double-free (verified at runtime against baseline).
func TestVectorLitMoveFromVarTupleClearsDropFlag(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type _Box { string label; drop(~this) {} }
		main() {
			(int, _Box) t = (1, _Box(label: "a"));
			(int, _Box)[] v = [t];
		}
	`)
	// The tuple var has its own drop flag (bindingDropTuple, T0371).
	codegentest.AssertContains(t, ir, "%t.dropflag = alloca i1")
	// Moved into the vector literal — the tupleNeedsDrop arm must clear it so
	// Vector.drop's element walk becomes the sole owner of the tuple's _Box.
	codegentest.AssertContains(t, ir, "store i1 false, i1* %t.dropflag")
}

// T0610: a plain heap user type with NO drop method (and no droppable
// fields) moved from a variable into a vector literal must still clear the
// source ident's drop flag — exercising the "needs pal_free" arm of
// vecElemNeedsUserTypeDrop (stmt.go:3983). All other T0610 tests use a type
// WITH an explicit drop; this is the sole coverage of the pal_free-only
// element path. Without the clear, both the var's scope-exit pal_free and
// Vector.drop's element walk free the same allocation → double-free
// (verified at runtime against baseline).
func TestVectorLitMoveFromVarPlainHeapClearsDropFlag(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type _Plain { int x; int y; }
		main() {
			_Plain p = _Plain(x: 1, y: 2);
			_Plain[] v = [p];
		}
	`)
	codegentest.AssertContains(t, ir, "%p.dropflag = alloca i1")
	codegentest.AssertContains(t, ir, "store i1 false, i1* %p.dropflag")
}

// B0245: Vector[UserType] drop should emit element drop loop for heap user types.
func TestVectorUserTypeElementDrop(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Foo { int x; }
		main() {
			Foo[] v = [];
			v.push(Foo(x: 1));
		}
	`)
	// Should have vector element drop loop (vecdrop.head) for user type elements
	codegentest.AssertContains(t, ir, "vecdrop.head")
	codegentest.AssertContains(t, ir, "call void @pal_free(")
}

// B0245: Debug — check what IR is generated for Vector[Foo] with full std
func TestVectorUserTypeElementDropWithPush(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Foo { int x; }
		test() {
			Foo[] v = [];
			v.push(Foo(x: 1));
			v.push(Foo(x: 2));
		}
	`)
	// Check for element drop loop: vecdrop.head is the loop header
	if !strings.Contains(ir, "vecdrop.head") {
		// Print the main function IR for debugging
		lines := strings.Split(ir, "\n")
		inFunc := false
		for _, line := range lines {
			if strings.Contains(line, "define") && strings.Contains(line, "@test(") {
				inFunc = true
			}
			if inFunc {
				t.Logf("%s", line)
				if line == "}" {
					break
				}
			}
		}
		t.Errorf("expected vecdrop.head element drop loop for Vector[Foo]")
	}
}

// B0257: Vector element drop loop must call both the user type's drop method
// and pal_free to free the instance memory.
func TestVectorUserTypeDropCallsFree(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Res { int id; drop(~this) {} }
		test() {
			Res[] v = [];
			v.push(Res(id: 1));
		}
	`)
	// Element drop loop header must exist
	codegentest.AssertContains(t, ir, "vecdrop.head")
	// The loop body must call Res.drop AND pal_free (not just drop)
	codegentest.AssertContains(t, ir, "call void @Res.drop(")
	// pal_free must appear in the element drop loop (for the instance memory)
	codegentest.AssertContains(t, ir, "call void @pal_free(")
}

// T0109: Vector-producing call expressions (e.g., split()) are tracked as stmt temps.
func TestVectorCallExprStmtTempTracking(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		main() {
			int n = "a b c".split(" ").len;
		}
	`)
	// The vector temp from split() should be tracked and dropped.
	codegentest.AssertContains(t, ir, "call void @Vector.drop(")
}

// B0325: Field access on a type with explicit drop must use the $wrap function
// (drop + pal_free), not just the raw drop function.
func TestFieldAccessExplicitDropUsesWrap(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Resource { int id; drop(~this) {} }
		make_resource!() Resource { return Resource(id: 42); }
		test!() {
			int v = make_resource()?!.id;
		}
	`)
	codegentest.AssertContains(t, ir, "heap.drop")
	// Must use the $wrap function that calls drop + pal_free
	codegentest.AssertContains(t, ir, "Resource.drop$wrap")
}

// === Clone annotation (T0154) ===

func TestCloneSynthesizesCloneMethod(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Doc `+"`clone"+` {
			string title;
			int pages;
		}
		test() {
			d := Doc(title: "hi", pages: 1);
			d2 := d.clone();
		}
	`)
	// The synthesized clone method should exist and call promise_string_new (dupString)
	codegentest.AssertContains(t, ir, "Doc.clone")
}

func TestCloneStringNativeMethod(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		test() {
			s := "hello";
			s2 := s.clone();
		}
	`)
	// string.clone() calls promise_string_new (dupString)
	codegentest.AssertContains(t, ir, "promise_string_new")
}

func TestCloneVectorNativeMethod(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		test() {
			v := [1, 2, 3];
			v2 := v.clone();
		}
	`)
	// Vector.clone() calls pal_alloc (dupVector)
	codegentest.AssertContains(t, ir, "pal_alloc")
}

func TestCloneStringVectorDupsElements(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		test() {
			v := ["a", "b"];
			v2 := v.clone();
		}
	`)
	// String vector clone should have the string dup loop
	codegentest.AssertContains(t, ir, "vecdup_str.head")
	codegentest.AssertContains(t, ir, "promise_string_new")
}

// B0275: Vector.clone() must deep-clone heap user type elements.
func TestCloneVectorHeapTypeCallsClone(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Foo `+"`"+`clone {
			string name;
		}
		test() {
			v := [Foo(name: "a")];
			v2 := v.clone();
		}
	`)
	// Should have the clone loop calling Foo.clone
	codegentest.AssertContains(t, ir, "vecclone.head")
	codegentest.AssertContains(t, ir, "Foo.clone")
}

// B0276: dupHeapValueFields must deep-clone vector fields with droppable elements.
// When a heap type without clone() has a string[] field and is dup'd via
// dupHeapValue (e.g., as a vector element during clone), the string[] field
// must be deep-cloned, not shallow-copied.
func TestDupHeapValueFieldsDeepClonesVectorStrings(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Container {
			string[] names;
			int id;
		}
		test() {
			v := [Container(names: ["a", "b"], id: 1)];
			v2 := v.clone();
		}
	`)
	// Vector[Container].clone() → emitVectorElementCloneLoop → cloneHeapElement
	// → dupHeapValue → dupHeapValueFields → should deep-clone the string[] field.
	codegentest.AssertContains(t, ir, "vecdup_str.head")
}

// B0289: emitVectorElementCloneLoop → cloneHeapElement must check type-arg safety
// before calling clone(). When vector elements are Map[string, NonCloneableEnum],
// Map.clone() would shallow-copy the enum values → double-free. The fix falls
// back to dupHeapValue instead.
func TestVectorCloneLoopSkipsUnsafeMapClone(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		enum JsonNode {
			Null,
			Text(string value),
			Dict(map[string, JsonNode] fields),
		}
		test() {
			map[string, JsonNode][] maps = [{"k": JsonNode.Text(value: "v")}];
			map[string, JsonNode][] maps2 = maps.clone();
		}
	`)
	// T1129: JsonNode (recursive, Map-bearing) now has a synthesized recursive
	// clone, so Map[string, JsonNode] IS deep-cloneable. Vector[Map[...]].clone()
	// → emitVectorElementCloneLoop → cloneHeapElement now correctly routes through
	// Map[string, JsonNode].clone() (whose internal match-dup recurses via
	// @JsonNode.clone). This supersedes the old B0289 dupHeapValue fallback, whose
	// recursion guard shallow-copied the inner Map → double-free. jn3/jn4 runtime
	// tests confirm this path is leak-free.
	if codegentest.ExtractDefine(ir, "JsonNode.clone") == "" {
		t.Errorf("T1129: expected a synthesized @JsonNode.clone for the recursive enum:\n%s", ir)
	}
	if !strings.Contains(ir, "Map[string, JsonNode].clone") {
		t.Errorf("T1129: Vector[Map[string, JsonNode]].clone() should deep-clone elements "+
			"via Map.clone() now that JsonNode is cloneable:\n%s", ir)
	}
}

// B0289: When vector elements are Map[string, int] (safe type args),
// cloneHeapElement should still call Map.clone().
func TestVectorCloneLoopCallsSafeMapClone(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		test() {
			map[string, int][] maps = [{"a": 1}];
			map[string, int][] maps2 = maps.clone();
		}
	`)
	// Map[string, int] — both type args are safe, clone should be called.
	codegentest.AssertContains(t, ir, "Map[string, int].clone")
}

// B0302: Pushing a vector into a vector-of-vectors must dup the inner vector
// to ensure exclusive ownership. Without dup, filled() creates aliased pointers
// that cause double-free on the outer vector's element-level drop.
func TestVectorPushDupsDroppableElement(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		main() {
			int[][] v = [];
			int[] inner = [1, 2, 3];
			v.push(inner);
		}
	`)
	// The push should dup the inner vector (vecdup.copy block from dupVector)
	codegentest.AssertContains(t, ir, "vecdup.copy")
}

// B0343: Map []= with borrow-string key must dup so the map owns the key.
func TestMapIndexAssignDupBorrowKey(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Sink {
			map[string, string] m;
			put(~this, string k, string v) {
				this.m[k] = v;
			}
		}
		test() {
			Sink s = Sink(m: map[string, string]());
			s.put("a", "b");
		}
	`)
	// The key "k" (borrow param, no drop flag) must be dup'd at the []= site
	codegentest.AssertContains(t, ir, "strdup.copy")
}

// B0343: Map []= with owned-string key (has drop flag) clears the flag.
func TestMapIndexAssignClearKeyDropFlag(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		test() {
			map[string, string] m = map[string, string]();
			for k, v in m {
				map[string, string] dst = map[string, string]();
				dst[k] = v;
			}
		}
	`)
	// Key k has a drop flag from B0343; dst[k] = v clears it
	codegentest.AssertContains(t, ir, "k.dropflag")
	codegentest.AssertContains(t, ir, "forin.key.drop")
}

// B0355: MemberExpr (field access) used as map key must be dup'd — the struct
// still owns the pointer, so the map needs an independent copy.
func TestMapIndexAssignDupBorrowKeyMemberExpr(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Row { string name; }
		test() {
			Row r = Row(name: "hello");
			map[string, int] m = map[string, int]();
			m[r.name] = 1;
		}
	`)
	codegentest.AssertContains(t, ir, "strdup.copy")
}

// B0355: MemberExpr (field access) used as map value must be dup'd — the struct
// still owns the pointer, so the map needs an independent copy.
func TestMapIndexAssignDupBorrowValueMemberExpr(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Row { string name; }
		test() {
			Row r = Row(name: "world");
			map[string, string] m = map[string, string]();
			m["k"] = r.name;
		}
	`)
	codegentest.AssertContains(t, ir, "strdup.copy")
}

// T0157: Weak[T] drop function has correct structure: null check, atomic decrement weak_count.
func TestWeakDropFunctionBody(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		main() {
			a := Ref[int](42);
			w := a.downgrade();
		}
	`)
	// Weak drop should null-check and decrement weak_count
	codegentest.AssertContains(t, ir, "define void @\"Weak[int].drop\"")
	codegentest.AssertContains(t, ir, "decwc:")
	codegentest.AssertContains(t, ir, "free:")
}

func TestWeakCloneChainFreshSSA(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		main() {
			Ref[int] a = Ref[int](99);
			Weak[int] w = a.downgrade().clone();
		}
	`)
	// Weak clone in a chain must produce fresh SSA value
	codegentest.AssertContains(t, ir, "ptrtoint")
	codegentest.AssertContains(t, ir, "inttoptr")
	codegentest.AssertContains(t, ir, "%w.dropflag")
}

// T0157: Weak[T].clone() atomically increments weak_count.
func TestWeakCloneIR(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		main() {
			a := Ref[int](42);
			w := a.downgrade();
			w2 := w.clone();
		}
	`)
	// Both w and w2 should have drop flags
	codegentest.AssertContains(t, ir, "%w.dropflag")
	codegentest.AssertContains(t, ir, "%w2.dropflag")
	// Both should call Weak[int].drop at scope exit
	codegentest.AssertContains(t, ir, `call void @"Weak[int].drop"(`)
}

// T0157: dupWeak — reading a Weak[T] field from a droppable type increments weak refcount.
func TestDupWeakFieldFromDroppable(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Holder {
			Weak[int] w;
			drop(~this) {}
		}
		main() {
			a := Ref[int](42);
			h := Holder(w: a.downgrade());
			Weak[int] copy = h.w;
		}
	`)
	// Should produce weakdup block for weak_count increment (numeric suffix varies)
	codegentest.AssertContainsMatch(t, ir, `weakdup\.inc\.\d+:`)
	codegentest.AssertContainsMatch(t, ir, `weakdup\.merge\.\d+:`)
}

// T0838 regression guard: a handler binding of a HEAP-USER optional field must
// NOT neutralize the owner field — genOptionalHandlerExpr makes an independent
// dup (T0775), so the owner keeps & frees the original and the bound local owns
// the dup. handlerResultIsNativeHandle returns false for heap-user types, so the
// T0775 dup-and-don't-neutralize contract is preserved (no `store i1 false`).
func TestT0838HeapUserHandlerBindingPreservesDup(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Payload { int v; drop(~this) {} }
		type PayloadHolder { Payload? p; drop(~this) {} }
		main() {
			h := PayloadHolder(p: Payload(v: 21));
			Payload got = h.p? _ { Payload(v: 0) };
		}
	`)
	gmain := codegentest.ExtractDefine(ir, ".goroutine.main")
	// handlerResultIsNativeHandle is false for heap-user types, so the T0775
	// dup-and-don't-neutralize contract holds: genOptionalHandlerExpr makes an
	// independent dup (the binding's sole owner) and the owner's optional field
	// is NOT neutralized — the owner keeps & frees the original. So there is no
	// GEP-into-optional-struct present-flag clear in the goroutine body.
	codegentest.AssertNotContainsMatch(t, gmain,
		`getelementptr \{ i1, [^}]*\}, \{ i1, [^}]*\}\* %\d+, i32 0, i32 0\s*\n\s*store i1 false`)
}

// T0381: chained `.borrow.field` access dispatches through genMemberExpr's
// SharedRef unwrap — the inner member-access on `T&` looks up the field on
// the underlying `T`. Without the unwrap, the field-resolution path would
// fail to find the field on the SharedRef wrapper.
func TestT0381_ChainedBorrowFieldAccess(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Pt { int x; }
		main() {
			a := Ref[Pt](Pt(x: 7));
			x := a.borrow.x;
		}
	`)
	// The Ref[Pt] type and its drop should appear; sema/codegen lowering
	// of `.borrow.x` would fail without the SharedRef unwrap in genMemberExpr
	// because the field 'x' is not present on the SharedRef wrapper itself.
	codegentest.AssertContains(t, ir, "Ref[Pt].drop")
}

// T0381: a `T&`-typed local that is later reassigned to an owned `T`
// must register its drop binding using the underlying owned type — the
// SharedRef strip in maybeRegisterDrop ensures the proper drop function
// is dispatched (e.g., per-element drops for `string[]`).
func TestT0381_BorrowLocalReassignedToOwnedDrops(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		main() {
			v := string[]();
			v.push("hello");
			a := Ref[string[]](v);
			string[]& borrowed = a.borrow;
			borrowed = ["owned"];
		}
	`)
	// The reassignment makes `borrowed` an owned vector; on scope exit
	// we should see a call into Vector.drop (proves maybeRegisterDrop
	// saw past the SharedRef and registered an owned-vector drop).
	codegentest.AssertContains(t, ir, "call void @Vector.drop")
}

// T0411: Constructor field-init that reads a string field from a droppable
// owner (`Type(label: this.label)`) must dup the string so the new instance
// owns an independent copy. Without the dup, both the source's drop and the
// new instance's drop free the same buffer → double-free.
func TestT0411_ConstructorStringFieldFromThisDups(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type CB {
			string label;
			drop(~this) {}
			clone() CB {
				return CB(label: this.label);
			}
		}
		test_t0411_dup() {
			c := CB(label: "hi");
			c2 := c.clone();
		}
	`)
	cloneFn := codegentest.ExtractFunction(ir, "CB.clone")
	if cloneFn == "" {
		t.Fatal("expected CB.clone in IR")
	}
	// The clone body must dup the string when initializing the new CB —
	// i.e., a call to promise_string_new must appear inside CB.clone.
	codegentest.AssertContains(t, cloneFn, "call i8* @promise_string_new(")
}

// T0411: Vector field auto-dup via constructor field-init from `this.field`.
// Mirrors TestT0411_ConstructorStringFieldFromThisDups but for the
// dupContainerFieldAccess path on a Vector field.
func TestT0411_ConstructorVectorFieldFromThisDups(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type V {
			int[] items;
			drop(~this) {}
			clone() V {
				return V(items: this.items);
			}
		}
		test_t0411_vec_dup() {
			a := V(items: [1, 2, 3]);
			b := a.clone();
		}
	`)
	cloneFn := codegentest.ExtractFunction(ir, "V.clone")
	if cloneFn == "" {
		t.Fatal("expected V.clone in IR")
	}
	// dupContainerFieldAccess for a Vector field routes through dupVector,
	// which emits a `vecdup.copy` block label. Without the T0411 fix, the
	// field would be a direct store with no dup logic.
	codegentest.AssertContains(t, cloneFn, "vecdup.copy")
}

// T0847: Constructor field-init that reads a Vector element directly into an
// owning (non-borrow) field slot (`Holder(held: v[0])`) must dup-on-read.
// Without the dup, the element pointer is aliased into the new instance's
// owning field — both v (element walk in Vector.drop) and the holder (synth
// field drop) free the same instance → double-free SEGV. Mirrors T0403 (the
// ~-param call-arg path); maybeEnableDupForConstructorArg's IndexExpr branch
// sets dupHeapUserFieldAccess, consumed by genVectorIndex → cloneHeapElement.
func TestT0847_ConstructorVectorElementDups(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Item { string label; drop(~this) {} }
		type Holder { Item held; drop(~this) {} }
		test_t0847() {
			v := Item[]();
			v.push(Item(label: "x"));
			h := Holder(held: v[0]);
		}
	`)
	// cloneHeapElement → dupHeapValue: allocate a new instance and memcpy the
	// data. Without the fix the element would be a direct aliasing store.
	codegentest.AssertContains(t, ir, "call i8* @pal_alloc")
	codegentest.AssertContains(t, ir, "call void @llvm.memcpy")
}

// T0847 (paren variant): a parenthesized container-element ctor arg
// `Holder(held: (v[0]))` must peel the ParenExpr to reach the IndexExpr and
// still dup-on-read. Exercises maybeEnableDupForConstructorArg's ParenExpr peel.
func TestT0847_ConstructorParenVectorElementDups(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Item { string label; drop(~this) {} }
		type Holder { Item held; drop(~this) {} }
		test_t0847_paren() {
			v := Item[]();
			v.push(Item(label: "x"));
			h := Holder(held: (v[0]));
		}
	`)
	// Paren peel reaches the IndexExpr → dup-on-read fires (allocate + memcpy).
	codegentest.AssertContains(t, ir, "call i8* @pal_alloc")
	codegentest.AssertContains(t, ir, "call void @llvm.memcpy")
}

// --- T1031: aliasing a borrowed-return into a new owner dups at the call site -

// T1031: `Node b = ident(a)` where `ident(Node n) Node { return n; }` returns its
// by-value borrow param aliases a's heap instance. Because a remains owned (it is
// co-dropped at scope exit), the new owner b must receive an INDEPENDENT
// allocation — otherwise both drop the shared instance (double-free / UAF). The
// fix clones into the source local's storage at the call site, gated on a runtime
// `retPtr == argPtr` alias check. The callee itself returns the bare alias (no
// clone), so functions that relocate their param before returning (sort's COW)
// stay untouched.
func TestReturnBorrowParamDupsHeapUserType(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Node { int v; drop(~this){} }
		ident(Node n) Node { return n; }
		run() Node {
			Node a = Node(v: 1);
			Node b = ident(a);
			return b;
		}
	`)
	// The callee returns the bare alias — no clone.
	callee := codegentest.ExtractDefine(ir, "__user.ident")
	if callee == "" {
		t.Fatalf("ident callee not found in IR:\n%s", ir)
	}
	codegentest.AssertNotContains(t, callee, "heapdup.copy")
	// The caller clones into the source's storage under a runtime alias guard.
	caller := codegentest.ExtractDefine(ir, "__user.run")
	if caller == "" {
		t.Fatalf("run caller not found in IR:\n%s", ir)
	}
	codegentest.AssertContains(t, caller, "alias.dup")
	codegentest.AssertContains(t, caller, "heapdup.copy")
	codegentest.AssertContains(t, caller, "@pal_alloc")
}

// T1031: a returned by-value string param is deep-copied at the call site.
func TestReturnBorrowParamDupsString(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		ident(string s) string { return s; }
		run() string {
			string a = "x".repeat(2);
			string b = ident(a);
			return b;
		}
	`)
	callee := codegentest.ExtractDefine(ir, "__user.ident")
	if callee == "" {
		t.Fatalf("ident callee not found in IR:\n%s", ir)
	}
	codegentest.AssertNotContains(t, callee, "strdup.copy")
	caller := codegentest.ExtractDefine(ir, "__user.run")
	if caller == "" {
		t.Fatalf("run caller not found in IR:\n%s", ir)
	}
	codegentest.AssertContains(t, caller, "alias.dup")
	codegentest.AssertContains(t, caller, "strdup.copy")
}

// T1031: a returned by-value vector param is deep-copied at the call site.
func TestReturnBorrowParamDupsVector(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		ident(int[] v) int[] { return v; }
		run() int[] {
			int[] a = [];
			int[] b = ident(a);
			return b;
		}
	`)
	caller := codegentest.ExtractDefine(ir, "__user.run")
	if caller == "" {
		t.Fatalf("run caller not found in IR:\n%s", ir)
	}
	codegentest.AssertContains(t, caller, "alias.dup")
	codegentest.AssertContains(t, caller, "vecdup.copy")
}

// T1031: a moved (`move`) param is owned by the callee — the call site must NOT
// emit the aliasing clone (the source is consumed, not co-owned).
func TestReturnMovedParamNoDup(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Node { int v; drop(~this){} }
		consume(Node move n) Node { return n; }
		run() Node {
			Node a = Node(v: 1);
			Node b = consume(move a);
			return b;
		}
	`)
	caller := codegentest.ExtractDefine(ir, "__user.run")
	if caller == "" {
		t.Fatalf("run caller not found in IR:\n%s", ir)
	}
	codegentest.AssertNotContains(t, caller, "alias.dup")
}

// T1031/T1017: a DISCARDED call whose heap-user-type result aliases a still-live
// local takes the discard path (clearDiscardedAliasTempFlag), NOT the assignment
// clone path. Heap user-type results are tracked as heapTemps keyed by an
// extractvalue SSA value distinct from the freshly-extracted retPtr, so the
// function must scan the tracked heap temps (loading each temp's stored instance
// pointer) and clear the matching temp's flag — keeping the live local the sole
// owner, dropped once at scope exit. This exercises the heap-temp scan branch
// (the vector/stmtTemp discard case in TestT1017DiscardedAliasClearsResultTemp
// returns earlier via the direct stmtTempMap lookup).
func TestReturnBorrowParamDiscardedHeapUserTypeScansHeapTemps(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Node { int v; drop(~this){} }
		ident(Node n) Node { return n; }
		run() int {
			Node n = Node(v: 11);
			ident(n);
			return n.v;
		}
	`)
	caller := codegentest.ExtractDefine(ir, "__user.run")
	if caller == "" {
		t.Fatalf("run caller not found in IR:\n%s", ir)
	}
	// Discard path (not the assignment clone path).
	codegentest.AssertContains(t, caller, "alias.discard.clear")
	codegentest.AssertNotContains(t, caller, "alias.dup")
}
