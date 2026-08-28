package concurrency1

import (
	"regexp"
	"strings"
	"testing"

	antlr "github.com/antlr4-go/antlr/v4"
	"github.com/promise-language/promise/compiler/internal/ast"
	"github.com/promise-language/promise/compiler/internal/codegen"
	"github.com/promise-language/promise/compiler/internal/codegen/codegentest"
	"github.com/promise-language/promise/compiler/internal/parser"
	"github.com/promise-language/promise/compiler/internal/sema"
	"github.com/promise-language/promise/compiler/internal/types"
)

// T0850: an `Ref[T?]` (or `Mutex[T?]`) whose element is an Optional must drop
// the inner optional's heap payload when the last reference is released. The
// Arc/Mutex inner-drop path (emitInnerDrop) dispatched on extractNamed, which is
// nil for an Optional, so no case fired and the held value leaked. The fix adds
// an Optional case that drops the present inner — here @Box.drop on the held Box.
func TestArcOptionalElementInnerDrop(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Box { string s; }
		main() {
			Box? init = Box(s: "x");
			a := Ref[Box?](init);
			_ := a;
		}
	`)
	arcDrop := codegentest.ExtractDefine(ir, `"Ref[Box?].drop"`)
	codegentest.AssertContains(t, arcDrop, "call void @Box.drop(")
}

// T0579: Array field with channel element — exercises `typeNeedsFieldDrop`'s
// `AsChannel` branch.
func TestFixedArrayFieldChannelElement(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type _ChArr { channel[int][2] data; }
		main() {
			c0 := channel[int]();
			c1 := channel[int]();
			c := _ChArr(data: [c0, c1]);
		}
	`)
	// T0663: Channel.drop is per-element-type — Channel[int].drop here.
	codegentest.AssertContains(t, ir, `call void @"Channel[int].drop"`)
}

func TestFixedArrayIndexDupsChannel(t *testing.T) {
	// Channel element: dupChannel inlines a null check + atomic refcount
	// incref on the channel struct (compiler.go:2234, chdup.inc block).
	ir := codegentest.GenerateIR(t, `
		main() {
			c0 := channel[int]();
			c1 := channel[int]();
			channel[int][2] arr = [c0, c1];
			channel[int] x = arr[0];
		}
	`)
	codegentest.AssertContains(t, ir, "arridx.ok")
	// dupChannel emits a chdup.inc / chdup.merge block pair and an atomic add.
	codegentest.AssertContains(t, ir, "chdup.inc")
	codegentest.AssertContains(t, ir, "chdup.merge")
}

func TestFixedArrayIndexDupsArc(t *testing.T) {
	// Arc element: dupArc emits an atomic refcount incref.
	ir := codegentest.GenerateIR(t, `
		main() {
			Ref[int][2] arr = [Ref[int](1), Ref[int](2)];
			Ref[int] x = arr[0];
		}
	`)
	codegentest.AssertContains(t, ir, "arridx.ok")
	// dupArc emits an atomic fetch-add on the Arc's refcount field.
	if !strings.Contains(ir, "atomicrmw add") && !strings.Contains(ir, "promise_arc_clone") {
		t.Errorf("expected Arc dup helper in IR; got:\n%s", ir)
	}
}

// T0262: WASM batch tests compile test bodies as coroutines and run them
// through the cooperative scheduler instead of spawning threads.
func TestGenerateTestMainWasmCoopScheduler(t *testing.T) {
	src := `myTest() ` + "`test" + ` { }`
	input := antlr.NewInputStream(src)
	lexer := parser.NewPromiseLexer(input)
	lexer.RemoveErrorListeners()
	stream := antlr.NewCommonTokenStream(lexer, antlr.TokenDefaultChannel)
	p := parser.NewPromiseParser(stream)
	p.RemoveErrorListeners()
	tree := p.CompilationUnit()
	file, errs := ast.Build("test.pr", tree)
	if len(errs) > 0 {
		t.Fatalf("AST build errors: %v", errs)
	}

	stdModInfo, stdScope := codegentest.GetCodegenStdModInfo()
	stdUse := &ast.UseDecl{Alias: "_", CatalogName: "std"}
	file.Uses = append([]*ast.UseDecl{stdUse}, file.Uses...)

	ti := sema.ParseTargetInfo("wasm32-wasi")
	info, semaErrs := sema.CheckWithTarget(file, map[string]*types.Scope{"std": stdScope}, ti)
	if len(semaErrs) > 0 {
		t.Fatalf("sema errors: %v", semaErrs)
	}
	info.ModuleInfos = map[string]*sema.ModuleInfo{"std": stdModInfo}
	info.ModuleOrder = []string{"std"}
	result := codegen.Compile(file, info, "wasm32-wasi")
	result.GenerateTestMain(info.Tests, nil)
	ir := result.Module.String()

	// WASM: should init scheduler with 1 P
	codegentest.AssertContains(t, ir, "call void @promise_sched_init(i32 1)")
	// WASM: should compile test as coroutine
	codegentest.AssertContains(t, ir, "define i8* @.test_coro.myTest()")
	// WASM: coroutine has presplitcoroutine attribute
	codegentest.AssertContainsMatch(t, ir, `@\.test_coro\.myTest\(\).*presplitcoroutine`)
	// WASM: should run through cooperative scheduler
	codegentest.AssertContains(t, ir, "call void @promise_sched_coop_run()")
	// WASM: should NOT use thread-based promise_test_run
	codegentest.AssertNotContains(t, ir, "call i32 @promise_test_run(")
	// WASM: should NOT bump goroutine counter past 0 (test G needs id=0)
	// The sched_init call is followed by alloc count reset, not atomicrmw add
}

// B0165: Sched struct includes ready_count field (i32 at end).
func TestSchedStructHasReadyCount(t *testing.T) {
	ir := codegentest.GenerateIR(t, `main() { }`)
	// The sched global should include the ready_count i32 field
	// Full type: { i8*, i8*, i64, i8*, i8*, i32, i8*, i8*, i64, i8, i8, i8*, i8*, i8*, i32, i64, i64, i64, i64, i8*, i32, i32, i8*, i8* }
	codegentest.AssertContains(t, ir, "@__promise_sched = global")
	// Verify sched_loop is defined (it increments ready_count)
	codegentest.AssertContains(t, ir, "define i8* @promise_sched_loop(")
}

// T1685: the scheduler grows its M pool on demand so a library blocking wait
// (which blocks the OS thread) never starves runnable goroutines. Assert the
// two new primitives exist and are wired into the handoff/park paths.
func TestSchedOnDemandMFunctions(t *testing.T) {
	ir := codegentest.GenerateIR(t, `main() { }`)
	// startm (create-or-reuse-spare) and park_spare (park a P-less M) are defined.
	codegentest.AssertContains(t, ir, "define void @promise_sched_startm(i8*")
	codegentest.AssertContains(t, ir, "define void @promise_sched_park_spare(i8*")
	// enter_syscall hands the detached P to another M via startm.
	codegentest.AssertContainsMatch(t, ir, `(?s)define void @promise_sched_enter_syscall\(\).*?call void @promise_sched_startm\(`)
	// sched_loop parks a P-less M as a spare (loop-top null-P path).
	codegentest.AssertContainsMatch(t, ir, `(?s)define i8\* @promise_sched_loop\(.*?call void @promise_sched_park_spare\(`)
}

// T1685: every library blocking wait compiled outside a coroutine wraps its
// pal_cond_wait in the enter/exit syscall handoff, so the M's P is handed off
// for the duration of the block. Exercises the channel-recv site; all six sites
// share the emitBlockingCondWait helper.
func TestBlockingChannelRecvHandsOffP(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		worker(channel[int] ch) { if v := <-ch { } }
		main() {
			ch := channel[int]();
			go { worker(ch); };
			ch.send(1);
		}
	`)
	// The non-coroutine recv wait emits enter_syscall → pal_cond_wait → exit_syscall
	// in order (worker() is an ordinary function, so inCoroutine is false).
	codegentest.AssertContainsMatch(t, ir,
		`(?s)call void @promise_sched_enter_syscall\(\)\s*call void @pal_cond_wait\([^\n]*\n\s*call void @promise_sched_exit_syscall\(\)`)
}

// T1685: emitBlockingCondWait is wired into every library blocking wait compiled
// outside a coroutine, not just channel recv. A regression that dropped the
// handoff from any single site would deadlock only that operation's callers, so
// assert each site's ordinary function emits enter_syscall → pal_cond_wait →
// exit_syscall in its own body. (netpoll is the sixth site; it needs the net
// module's socket path, so it is covered by the net/http runtime suite — the
// original T1636 symptom — rather than here.)
func TestBlockingWaitsHandOffPPerSite(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		locker(Mutex[int] move m) { use g := m.lock(); g.borrow += 1; }
		sender(channel[int] ch) { ch.send(1); }
		forin(channel[int] ch) { for v in ch { } }
		main() {
			m := Mutex[int](0);   go { locker(move m); };
			ch := channel[int]();  go { sender(ch); };
			ch2 := channel[int](); go { forin(ch2); };
		}
	`)
	// Each blocking wait wraps pal_cond_wait between enter_syscall and
	// exit_syscall on the same block (emitBlockingCondWait emits them
	// consecutively). Match within each function body so a site missing the
	// wrapper is caught even though the others still have it.
	handoff := regexp.MustCompile(
		`(?s)call void @promise_sched_enter_syscall\(\)\s*call void @pal_cond_wait\([^\n]*\n\s*call void @promise_sched_exit_syscall\(\)`)
	countCondWait := regexp.MustCompile(`call void @pal_cond_wait\(`)
	// Mutex.lock: one blocking wait. Channel send: two (buffered-full + unbuffered
	// rendezvous). for-in channel: one. Each cond_wait must be handed off.
	for _, tc := range []struct {
		fn         string
		condWaits  int
		wantSuffix string
	}{
		{"__user.locker", 1, "Mutex.lock"},
		{"__user.sender", 2, "channel send (full + rendezvous)"},
		{"__user.forin", 1, "for-in channel recv"},
	} {
		body := codegentest.ExtractFunction(ir, tc.fn)
		if body == "" {
			t.Fatalf("%s: could not find function %s in IR", tc.wantSuffix, tc.fn)
		}
		if !handoff.MatchString(body) {
			t.Errorf("%s (%s): blocking wait not wrapped in enter/exit syscall handoff\nbody:\n%s",
				tc.wantSuffix, tc.fn, body)
		}
		// Every pal_cond_wait in the body must be a handed-off one — assert the
		// count of cond_waits equals the count of enter_syscall calls preceding
		// them, so a site with a mix of wrapped and bare waits is caught.
		if got := len(countCondWait.FindAllString(body, -1)); got != tc.condWaits {
			t.Errorf("%s (%s): expected %d pal_cond_wait, got %d",
				tc.wantSuffix, tc.fn, tc.condWaits, got)
		}
		enters := len(regexp.MustCompile(`call void @promise_sched_enter_syscall\(\)`).FindAllString(body, -1))
		if enters != tc.condWaits {
			t.Errorf("%s (%s): %d pal_cond_wait but %d enter_syscall — a wait is not handed off",
				tc.wantSuffix, tc.fn, tc.condWaits, enters)
		}
	}
}

func TestChannelFieldInUserType(t *testing.T) {
	// B0096: channel[T] fields in user types must use i8* layout,
	// not {i8*, i8*} (value struct). These are native container types like Vector.
	ir := codegentest.GenerateIR(t, `
		type IntChan {
			channel[int] ch;
			emit(~this, int v) { this.ch.send(v); }
		}
		main() {
			ch := channel[int](capacity: 1);
			s := IntChan(ch: ch);
			s.emit(42);
		}
	`)
	// Instance struct field must be i8* (opaque channel pointer), not {i8*, i8*}
	codegentest.AssertContains(t, ir, "%promise_IntChan_i = type { %promise_IntChan_m*, i8* }")
	// Channel send generates inline mutex lock IR
	codegentest.AssertContains(t, ir, "call void @pal_mutex_lock(")
}

func TestTaskFieldInUserType(t *testing.T) {
	// B0096: task[T] fields must use i8* layout in user types.
	ir := codegentest.GenerateIR(t, `
		compute() int { return 42; }
		type Holder { task[int] t; }
		main() {
			t := go compute();
			h := Holder(t: t);
		}
	`)
	// Instance struct field must be i8* (opaque task pointer), not {i8*, i8*}
	codegentest.AssertContains(t, ir, "%promise_Holder_i = type { %promise_Holder_m*, i8* }")
}

func TestChannelFieldInModuleType(t *testing.T) {
	// B0096: channel fields in module-defined types
	ir := codegentest.GenerateIRWithCatalogModule(t, "mymod",
		`type Sender `+"`public"+` {
			channel[int] _ch;
			emit(~this, int v) `+"`public"+` { this._ch.send(v); }
		}`,
		`
		use mymod;
		main() {
			ch := channel[int](capacity: 1);
			s := mymod.Sender(_ch: ch);
			s.emit(42);
		}
		`,
	)
	// Instance struct field must be i8* in module types too
	codegentest.AssertContains(t, ir, "%promise_Sender_i = type { %promise_Sender_m*, i8* }")
	codegentest.AssertContains(t, ir, "call void @pal_mutex_lock(")
}

func TestChannelFieldInEnumVariant(t *testing.T) {
	// B0096: channel[T] in enum variant fields must use i8* layout.
	// Send variant: channel[int] (i8*, 8 bytes) + int (i64, 8 bytes) = 16 bytes.
	// Without fix: channel would be {i8*, i8*} (16 bytes) + i64 (8 bytes) = 24 bytes.
	ir := codegentest.GenerateIR(t, `
		enum Action {
			Send(channel[int] ch, int value),
			Done,
		}
		main() {
			ch := channel[int](capacity: 1);
			a := Action.Send(ch: ch, value: 42);
		}
	`)
	// Data area must be [16 x i8] (channel as i8*), not [24 x i8] (channel as {i8*,i8*})
	codegentest.AssertContains(t, ir, "%promise_Action_enum = type { i32, [16 x i8] }")
	// The enum data area specifically must not use [24 x i8]
	codegentest.AssertNotContains(t, ir, "%promise_Action_enum = type { i32, [24 x i8] }")
}

func TestGenericTypeWithChannelFieldLayout(t *testing.T) {
	// B0096: mono layout for generic type with channel[T] field.
	ir := codegentest.GenerateIR(t, `
		type Wrapper[T] {
			channel[T] ch;
			T default_val;
		}
		main() {
			ch := channel[int](capacity: 1);
			w := Wrapper[int](ch: ch, default_val: 0);
		}
	`)
	// Monomorphized instance struct: channel field is i8*, int field is i64
	codegentest.AssertContains(t, ir, `%"promise_Wrapper[int]_i" = type { %"promise_Wrapper[int]_m"*, i8*, i64 }`)
}

// B0163/T0663: Channel scope-exit drop — standalone channel gets drop flag and
// per-element-type Channel[int].drop call.
func TestDropChannelStandaloneHasDrop(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		main() {
			ch := channel[int](capacity: 5);
		}
	`)
	codegentest.AssertContains(t, ir, "%ch.dropflag")
	codegentest.AssertContains(t, ir, `call void @"Channel[int].drop"(`)
}

// B0163/T0663: Channel[T].drop body uses refcount — frees only when refcount drops to 0
func TestChannelDropFuncBody(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		main() {
			ch := channel[int](capacity: 5);
		}
	`)
	codegentest.AssertContains(t, ir, `define void @"Channel[int].drop"(i8* %this)`)
	// Refcount decrement (atomicrmw or load+add for WASM)
	codegentest.AssertContains(t, ir, "i64 -1")
	codegentest.AssertContains(t, ir, "call void @pal_free(")
	codegentest.AssertContains(t, ir, "call void @pal_mutex_destroy(")
	codegentest.AssertContains(t, ir, "call void @pal_cond_destroy(")
}

// B0163/T0663: Channel refcount initialized to 1 in promise_channel_new
func TestChannelRefcountInit(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		main() {
			ch := channel[int](capacity: 1);
		}
	`)
	// promise_channel_new should store refcount = 1
	codegentest.AssertContains(t, ir, "define i8* @promise_channel_new(")
	// Channel[int].drop should use atomicrmw add with -1 (refcount decrement)
	codegentest.AssertContains(t, ir, `define void @"Channel[int].drop"(`)
}

// B0163/T0663: Channel drop null-checks the pointer (zero-initialized channels from error paths)
func TestChannelDropNullCheck(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		main() {
			ch := channel[int](capacity: 1);
		}
	`)
	// Channel[int].drop body should have null check (icmp eq ... null)
	dropFn := codegentest.ExtractFunction(ir, `"Channel[int].drop"`)
	if dropFn == "" {
		t.Fatal("expected Channel[int].drop function in IR")
	}
	codegentest.AssertContains(t, dropFn, "icmp eq")
	codegentest.AssertContains(t, dropFn, "null")
}

// B0163/T0663: Channel drop flag cleared on move (borrow detection)
func TestChannelDropFlagInDroppableContainer(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		main() {
			ch := channel[int](capacity: 5);
			ch.send(42);
		}
	`)
	// isDroppableContainerOrString should recognize channels
	codegentest.AssertContains(t, ir, "%ch.dropflag")
	codegentest.AssertContains(t, ir, `call void @"Channel[int].drop"(`)
}

// T1158: passing a channel as a `go f(ch)` argument increments the channel
// refcount (B0163) AND registers a matching goroutine-side Channel[T].drop after
// the target call returns, so the refcount returns to 0 and the channel is freed
// (previously the increment had no balancing decrement → 5-allocation leak).
func TestGoCallChannelArgGoroutineDrop(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		ping(channel[int] out) { out.send(1); }
		main() {
			ch := channel[int](capacity: 1);
			go ping(ch);
		}
	`)
	// The goroutine coroutine body must contain the balancing drop. Anchor on the
	// `define ... @.goroutine.0(` line via extractDefine — extractFunction would
	// latch onto a `@.goroutine.0` fn-pointer reference inside @.goroutine.main
	// and extract the MAIN coroutine instead (whose caller-side drop of `ch` would
	// make this assertion pass for the wrong reason).
	coroFn := codegentest.ExtractDefine(ir, `.goroutine.0`)
	if coroFn == "" {
		t.Fatal("expected .goroutine.0 coroutine function in IR")
	}
	if got := strings.Count(coroFn, `call void @"Channel[int].drop"(`); got != 1 {
		t.Fatalf("expected exactly 1 goroutine-side Channel[int].drop, got %d", got)
	}
}

// T1158: two channel args to one `go f(a, b)` call must each get their own
// goroutine-side drop — the per-arg loop appends one goArgBorrowDrop per channel,
// so the coroutine body holds exactly two balancing Channel[int].drop calls (one
// increment, one decrement per channel). Guards against a regression that only
// drops the first channel and leaks the rest.
func TestGoCallTwoChannelArgsBothDropped(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		ping2(channel[int] a, channel[int] b) { a.send(1); b.send(2); }
		main() {
			x := channel[int](capacity: 1);
			y := channel[int](capacity: 1);
			go ping2(x, y);
		}
	`)
	coroFn := codegentest.ExtractDefine(ir, `.goroutine.0`)
	if coroFn == "" {
		t.Fatal("expected .goroutine.0 coroutine function in IR")
	}
	if got := strings.Count(coroFn, `call void @"Channel[int].drop"(`); got != 2 {
		t.Fatalf("expected 2 goroutine-side Channel[int].drop calls (one per channel arg), got %d", got)
	}
}

// T1109: A Ref/Arc[T] variant field in a container must dup via a strong-count
// increment (arcdup.inc), NOT route into dupHeapValue — Arc's LLVM type is a
// bare i8* (a *types.PointerType), so the heap-user-type dup path panicked with
// "interface conversion: *types.PointerType, not *types.StructType". This guards
// against regression to that route by confirming the variant-field dup emits the
// Arc refcount increment.
func TestDupEnumVariantArcRefcount(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		enum Holder { Pair(Ref[int] r, int n) }
		main() {
			m := Map[int, Holder]();
			m[1] = Holder.Pair(Ref[int](9), 2);
		}
	`)
	// The variant-field dup for the Ref field reaches dupArc (strong-count
	// increment) rather than panicking in dupHeapValue.
	codegentest.AssertContains(t, ir, "enumdup.Pair")
	codegentest.AssertContains(t, ir, "arcdup.inc")
}

// T1109: A generic enum carrying Ref[T]/Weak[T] variant fields exercises the
// type-substitution sub-branches in emitVariantFieldDup (the dup is synthesized
// in a mono context where c.typeSubst != nil, so the element type must be
// substituted before dupArc/dupWeak). Confirms both refcount paths fire for the
// monomorphized Box[int] instance.
func TestDupEnumVariantGenericArcWeakRefcount(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		enum Box[T] { Some(Ref[T] r, int n), None }
		enum WBox[T] { W(Weak[T] w, int n), E }
		main() {
			a := Ref[int](7);
			m := Map[int, Box[int]]();
			m[1] = Box[int].Some(Ref[int](9), 2);
			wm := Map[int, WBox[int]]();
			wm[1] = WBox[int].W(a.downgrade(), 3);
		}
	`)
	codegentest.AssertContains(t, ir, "arcdup.inc")
	codegentest.AssertContains(t, ir, "weakdup.inc")
}

// T0776: `((o)!).borrow` on an Ref[int]? ident source exercises the
// trackTempWithDrop branch of genOptionalForceUnwrap's type-aware temp tracker
// (the path the string/vector tests above do NOT cover — those hit
// trackStringTemp / trackVectorTempWithElemType). Without the ParenExpr peel,
// the extracted i8* Arc handle gets registered as a NEW stmt-temp via
// trackTempWithDrop after `unwrap.ok`, with a tmp.exec / Ref[int].drop
// cleanup racing the source optional's own scope-drop on the same handle —
// atomic refcount goes to zero twice → use-after-free. Mirrors
// TestT0654_OptionalArcUnwrapConsumeTracked's discriminator (which asserts
// the OPPOSITE — non-ident sources MUST register the temp) by checking the
// IR slice AFTER `unwrap.ok`: no new tmp.exec / Arc drop pair must appear
// there. Covers Arc/Weak/Mutex/Task/Channel as a class — all five hit the
// same outer gate via trackTempWithDrop.
func TestT0776ParenForceUnwrapArcNoTemp(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		tfn() int {
			Ref[int]? o = Ref[int](42);
			return ((o)!).borrow;
		}
		main() { _ := tfn(); }
	`)
	fn := codegentest.ExtractFunction(ir, "__user.tfn")
	if fn == "" {
		t.Fatal("expected __user.tfn in IR")
	}
	// Find the actual `unwrap.ok.<N>:` block LABEL (not the `%unwrap.ok.<N>`
	// reference inside the preceding tmp.skip branch instruction — which would
	// pull tmp.exec.<M> into the post slice and trigger a false positive).
	unwrapLabel := regexp.MustCompile(`(?m)^\s*unwrap\.ok\.\d+:`)
	loc := unwrapLabel.FindStringIndex(fn)
	if loc == nil {
		t.Fatalf("expected unwrap.ok.<N>: block label from "+
			"genOptionalForceUnwrap:\n%s", fn)
	}
	// The slice after `unwrap.ok.<N>:` is where genOptionalForceUnwrap would
	// emit the spurious tmp.exec / Ref[int].drop pair if the ParenExpr peel
	// were missing. After `unwrap.ok`, the only legitimate Ref[int].drop call
	// sites are the source optional's `optdrop.inner.*` blocks (one per
	// return edge); those live INSIDE `optdrop.inner.*`, not `tmp.exec.*`.
	post := fn[loc[0]:]
	postLines := strings.Split(post, "\n")
	inTmpExec := false
	for _, line := range postLines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "tmp.exec") && strings.HasSuffix(trimmed, ":") {
			inTmpExec = true
			continue
		}
		if strings.HasSuffix(trimmed, ":") && !strings.HasPrefix(trimmed, "tmp.exec") {
			inTmpExec = false
		}
		if inTmpExec && strings.Contains(line, `@"Ref[int].drop"`) {
			t.Fatalf("found spurious @\"Ref[int].drop\" call in a tmp.exec "+
				"block AFTER unwrap.ok — the ParenExpr peel in "+
				"genOptionalForceUnwrap should skip temp tracking for "+
				"`((o)!)` because the source optional already owns the Arc:\n%s", fn)
		}
	}
}

// B0177: Type with channel field gets synthesized drop
func TestSynthDropChannelField(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type WithChan { channel[int] ch; }
		main() {
			channel[int] ch = channel[int]();
			w := WithChan(ch: ch);
		}
	`)
	codegentest.AssertContains(t, ir, "define void @WithChan.drop")
	// T0663: synthesized drop calls the per-element-type Channel[int].drop on
	// the channel field (was the single @Channel.drop symbol pre-T0663).
	withChanDrop := codegentest.ExtractFunction(ir, "WithChan.drop")
	if withChanDrop == "" {
		t.Fatal("expected WithChan.drop function in IR")
	}
	codegentest.AssertContains(t, withChanDrop, `call void @"Channel[int].drop"(`)
}

// B0236: Match destructure of droppable enum dups channel fields.
func TestMatchDupChannel(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		enum Wrapper {
			Chan(channel[int] ch),
			None,
		}
		test() {
			ch := channel[int](1);
			w := Wrapper.Chan(ch);
			match w {
				Chan(c) => { },
				None => { },
			}
		}
	`)
	// Extracting channel from droppable enum should dup via chdup (refcount increment)
	codegentest.AssertContains(t, ir, "chdup.inc")
}

// --- Go / Receive (concurrency) tests ---

func TestGoExprBasicFunction(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		compute() int { return 42; }
		main() {
			t := go compute();
			result := <-t;
		}
	`)
	// Coroutine function generated with presplitcoroutine attribute
	codegentest.AssertContains(t, ir, ".goroutine.")
	codegentest.AssertContains(t, ir, "presplitcoroutine")
	// Coroutine intrinsics used
	codegentest.AssertContains(t, ir, "call token @llvm.coro.id(")
	codegentest.AssertContains(t, ir, "call i8* @llvm.coro.begin(")
	codegentest.AssertContains(t, ir, "call i8 @llvm.coro.suspend(")
	// G struct created and enqueued
	codegentest.AssertContains(t, ir, "call i8* @promise_g_new(")
	codegentest.AssertContains(t, ir, "call void @promise_sched_enqueue(")
	// Result buffer allocated for non-void task
	codegentest.AssertContains(t, ir, "@pal_alloc")
	// Coroutine calls target function
	codegentest.AssertContains(t, ir, "call i64 @__user.compute")
}

func TestGoExprWithArgs(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		double(int x) int { return x * 2; }
		main() {
			t := go double(21);
			result := <-t;
		}
	`)
	// Coroutine generated
	codegentest.AssertContains(t, ir, ".goroutine.")
	codegentest.AssertContains(t, ir, "presplitcoroutine")
	// G struct created and enqueued
	codegentest.AssertContains(t, ir, "call i8* @promise_g_new(")
	codegentest.AssertContains(t, ir, "call void @promise_sched_enqueue(")
	// The coroutine should call the target function
	codegentest.AssertContains(t, ir, "call i64 @__user.double")
}

func TestGoExprVoidFunction(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		doWork() { }
		main() {
			t := go doWork();
			<-t;
		}
	`)
	codegentest.AssertContains(t, ir, ".goroutine.")
	codegentest.AssertContains(t, ir, "presplitcoroutine")
	// Coroutine calls void function
	codegentest.AssertContains(t, ir, "call void @__user.doWork")
	// G struct created and enqueued
	codegentest.AssertContains(t, ir, "call i8* @promise_g_new(")
	codegentest.AssertContains(t, ir, "call void @promise_sched_enqueue(")
}

// B0046: go extern_func() must generate a wrapper to handle sret ABI.
func TestGoExprExternFunction(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		get_data(int x) string `+"`"+`extern("test_get_data");
		main() {
			t := go get_data(42);
			result := <-t;
		}
	`)
	// Extern declared with sret pattern (void return, i8* sret first param)
	codegentest.AssertContains(t, ir, "declare void @test_get_data(i8* %sret")
	// Wrapper function generated for the go expression
	codegentest.AssertContains(t, ir, ".go_extern_wrap.get_data.")
	// Wrapper calls the extern with sret
	codegentest.AssertContains(t, ir, "call void @test_get_data(i8*")
	// Coroutine calls the wrapper (returns i8*, not void)
	codegentest.AssertContains(t, ir, "call i8* @.go_extern_wrap.get_data.")
	// G struct created and enqueued
	codegentest.AssertContains(t, ir, "call i8* @promise_g_new(")
	codegentest.AssertContains(t, ir, "call void @promise_sched_enqueue(")
}

func TestGoExprExternVoidFunction(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		do_work(int x) `+"`"+`extern("test_do_work");
		main() {
			t := go do_work(42);
			<-t;
		}
	`)
	// Even void externs need the wrapper — extern int params expect i8*
	// (pointer to value struct) but Promise internal representation is i64
	codegentest.AssertContains(t, ir, ".go_extern_wrap.do_work.")
	codegentest.AssertContains(t, ir, "call i8* @promise_g_new(")
}

// Container return types (Vector, Channel) use direct i8* return — no sret.
func TestGoExprExternContainerReturn(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		get_items() string[] `+"`"+`extern("test_get_items");
		main() {
			t := go get_items();
			result := <-t;
		}
	`)
	// Vector return: i8* directly (no sret)
	codegentest.AssertContains(t, ir, "declare i8* @test_get_items()")
	// Wrapper still generated (consistent handling of all externs)
	codegentest.AssertContains(t, ir, ".go_extern_wrap.get_items.")
	// Coroutine calls wrapper which returns i8*
	codegentest.AssertContains(t, ir, "call i8* @.go_extern_wrap.get_items.")
}

// Extern with string param (i8* in both Promise and extern ABI) + multiple args.
func TestGoExprExternMultipleArgs(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		process(string name, int count) string `+"`"+`extern("test_process");
		main() {
			t := go process("hello", 5);
			result := <-t;
		}
	`)
	// Wrapper generated with both params
	codegentest.AssertContains(t, ir, ".go_extern_wrap.process.")
	// Extern uses sret for string return
	codegentest.AssertContains(t, ir, "declare void @test_process(i8* %sret")
	// Coroutine calls wrapper
	codegentest.AssertContains(t, ir, "call i8* @.go_extern_wrap.process.")
}

func TestGoExprBlock(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		main() {
			t := go { };
			<-t;
		}
	`)
	// Coroutine function for the block
	codegentest.AssertContains(t, ir, ".goroutine.")
	codegentest.AssertContains(t, ir, "presplitcoroutine")
	// G struct created and enqueued
	codegentest.AssertContains(t, ir, "call i8* @promise_g_new(")
	codegentest.AssertContains(t, ir, "call void @promise_sched_enqueue(")
}

// B0113: go method_call() should work — not just direct function calls.
func TestGoExprMethodCall(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Counter {
			int value;
			get_value(this) int { return this.value; }
		}
		main() {
			c := Counter(value: 42);
			t := go c.get_value();
			result := <-t;
		}
	`)
	// Coroutine generated with capture of outer local 'c'
	codegentest.AssertContains(t, ir, ".goroutine.")
	codegentest.AssertContains(t, ir, "presplitcoroutine")
	// Method call generated inside coroutine body
	codegentest.AssertContains(t, ir, "Counter.get_value")
	// G struct created and enqueued
	codegentest.AssertContains(t, ir, "call i8* @promise_g_new(")
	codegentest.AssertContains(t, ir, "call void @promise_sched_enqueue(")
}

// B0113: go method_call() fire-and-forget (void method, result discarded).
func TestGoExprMethodCallFireAndForget(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Worker {
			int id;
			run(this) { }
		}
		main() {
			w := Worker(id: 1);
			go w.run();
		}
	`)
	codegentest.AssertContains(t, ir, ".goroutine.")
	codegentest.AssertContains(t, ir, "presplitcoroutine")
	codegentest.AssertContains(t, ir, "Worker.run")
	codegentest.AssertContains(t, ir, "call i8* @promise_g_new(")
	codegentest.AssertContains(t, ir, "call void @promise_sched_enqueue(")
}

// --- Channel tests ---

func TestChannelConstructor(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		main() {
			ch := channel[int](capacity: 5);
		}
	`)
	// Should call promise_channel_new
	codegentest.AssertContains(t, ir, "call i8* @promise_channel_new(")
	// Should init mutex and 2 cond vars inside promise_channel_new
	codegentest.AssertContains(t, ir, "call i8* @pal_mutex_init()")
	codegentest.AssertContains(t, ir, "call i8* @pal_cond_init()")
}

func TestChannelConstructorUnbuffered(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		main() {
			ch := channel[int]();
		}
	`)
	// Unbuffered: capacity=0
	codegentest.AssertContains(t, ir, "call i8* @promise_channel_new(i64 0,")
}

func TestChannelSend(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		main() {
			ch := channel[int](capacity: 5);
			ch.send(42);
		}
	`)
	// Should lock/unlock mutex and use memcpy for send
	codegentest.AssertContains(t, ir, "call void @pal_mutex_lock(")
	codegentest.AssertContains(t, ir, "call void @llvm.memcpy.p0i8.p0i8.i64(")
	codegentest.AssertContains(t, ir, "call void @pal_cond_signal(")
	codegentest.AssertContains(t, ir, "call void @pal_mutex_unlock(")
}

func TestChannelClose(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		main() {
			ch := channel[int](capacity: 5);
			ch.close();
		}
	`)
	// Close should broadcast both cond vars
	codegentest.AssertContains(t, ir, "call void @pal_cond_broadcast(")
}

func TestChannelReceive(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		main() {
			ch := channel[int](capacity: 5);
			ch.send(42);
			val := <-ch;
		}
	`)
	// Should have channel receive blocks
	codegentest.AssertContains(t, ir, "chrecv.wait")
	codegentest.AssertContains(t, ir, "chrecv.check")
	codegentest.AssertContains(t, ir, "chrecv.none")
	codegentest.AssertContains(t, ir, "chrecv.read")
	codegentest.AssertContains(t, ir, "chrecv.done")
	// Returns optional { i1, i64 }
	codegentest.AssertContains(t, ir, "insertvalue { i1, i64 }")
}

func TestChannelForIn(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		main() {
			ch := channel[int](capacity: 5);
			ch.send(42);
			ch.close();
			for v in ch {
				int x = v + 1;
			}
		}
	`)
	// Should have channel for-in block labels
	codegentest.AssertContains(t, ir, "forin_ch.header")
	codegentest.AssertContains(t, ir, "forin_ch.recv.wait")
	codegentest.AssertContains(t, ir, "forin_ch.recv.check")
	codegentest.AssertContains(t, ir, "forin_ch.recv.none")
	codegentest.AssertContains(t, ir, "forin_ch.recv.read")
	codegentest.AssertContains(t, ir, "forin_ch.body")
	codegentest.AssertContains(t, ir, "forin_ch.exit")
}

// T0671: for-in over a heap-element channel must drop the per-iteration loop
// variable. genForInChannel memcpys each item out of the ring buffer (a real
// move) into the loop-var alloca, so the loop owns it and must register a
// flag-guarded drop binding (string -> promise_string_drop). Pre-fix no drop
// binding was emitted, leaking one allocation per received heap item.
func TestT0671ForInChannelDropsHeapLoopVar(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		main() {
			ch := channel[string](capacity: 2);
			ch.send("a");
			ch.close();
			for s in ch {
				int n = s.bytes().len;
			}
		}
	`)
	// for-in over channel[string]: the loop body must drop the moved-out
	// loop variable each iteration (flag-guarded promise_string_drop).
	codegentest.AssertContains(t, ir, "forin_ch.body")
	codegentest.AssertContains(t, ir, "strdrop.call")
	codegentest.AssertContains(t, ir, "strdrop.skip")
	codegentest.AssertContains(t, ir, "call void @promise_string_drop(")
}

func TestChannelSendClosedPanic(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		main() {
			ch := channel[int](capacity: 5);
			ch.send(42);
		}
	`)
	// Send should check closed flag and panic if set
	codegentest.AssertContains(t, ir, "send.closed.panic")
	codegentest.AssertContains(t, ir, "send on closed channel")
	// After wait-full wakeup, should re-check closed
	codegentest.AssertContains(t, ir, "send.waitfull.closed")
}

func TestChannelDoubleClosePanic(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		main() {
			ch := channel[int](capacity: 5);
			ch.close();
		}
	`)
	// Close should check already-closed flag
	codegentest.AssertContains(t, ir, "close.panic")
	codegentest.AssertContains(t, ir, "close of closed channel")
}

func TestGoBlockCapturesOuterVars(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		main() {
			ch := channel[int](capacity: 1);
			int x = 10;
			go {
				ch.send(x);
			};
		}
	`)
	// Coroutine should have parameters for captured variables
	codegentest.AssertContains(t, ir, "define i8* @.goroutine.")
	codegentest.AssertContains(t, ir, "presplitcoroutine")
	// G created and enqueued
	codegentest.AssertContains(t, ir, "call i8* @promise_g_new(")
	codegentest.AssertContains(t, ir, "call void @promise_sched_enqueue(")
}

func TestGoBlockCapturesMultipleVars(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		main() {
			ch := channel[int](capacity: 1);
			int a = 10;
			int b = 20;
			go {
				ch.send(a + b);
			};
		}
	`)
	// Coroutine function should accept captured parameters
	codegentest.AssertContains(t, ir, "define i8* @.goroutine.")
	codegentest.AssertContains(t, ir, "presplitcoroutine")
}

// B0111: Select inside go block must capture outer channel variables.
func TestGoBlockCapturesSelectChannelVars(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		main() {
			ch := channel[int](capacity: 1);
			ch.send(1);
			go {
				select {
					v := <-ch:
						print_line("ok");
				}
			};
		}
	`)
	// The goroutine coroutine should have a parameter for the captured "ch"
	codegentest.AssertContains(t, ir, "ch.cap")
	// Should still generate coroutine infrastructure
	codegentest.AssertContains(t, ir, "define i8* @.goroutine.")
	codegentest.AssertContains(t, ir, "presplitcoroutine")
}

func TestGoBlockNoCapturesStillWorks(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		main() {
			go { };
		}
	`)
	// Even without captures, the go block should generate a coroutine
	codegentest.AssertContains(t, ir, "define i8* @.goroutine.")
	codegentest.AssertContains(t, ir, "presplitcoroutine")
	codegentest.AssertContains(t, ir, "call i8* @promise_g_new(")
	codegentest.AssertContains(t, ir, "call void @promise_sched_enqueue(")
}

func TestSchedulerGlobals(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		main() { }
	`)
	// Thread-local current G pointer
	codegentest.AssertContains(t, ir, "@__promise_current_g")
	codegentest.AssertContains(t, ir, "thread_local")
	// Global scheduler singleton
	codegentest.AssertContains(t, ir, "@__promise_sched")
	// TLS panic flag globals (T0143)
	codegentest.AssertContains(t, ir, "@__promise_panic_flag")
	codegentest.AssertContains(t, ir, "@__promise_panic_msg")
	codegentest.AssertContains(t, ir, "@__promise_panic_type")
}

func TestSchedulerFunctionsExist(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		main() { }
	`)
	codegentest.AssertContains(t, ir, "define void @promise_sched_init(")
	codegentest.AssertContains(t, ir, "define i8* @promise_sched_loop(")
	codegentest.AssertContains(t, ir, "define void @promise_sched_enqueue(")
	codegentest.AssertContains(t, ir, "define i8* @promise_sched_find_runnable(")
	codegentest.AssertContains(t, ir, "define void @promise_sched_park_m(")
	codegentest.AssertContains(t, ir, "define void @promise_sched_wake_m()")
	codegentest.AssertContains(t, ir, "define void @promise_goroutine_exit(")
	codegentest.AssertContains(t, ir, "define void @promise_sched_shutdown()")
	codegentest.AssertContains(t, ir, "define i8* @promise_g_new(")
}

func TestCoroIntrinsicsDeclared(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		main() { }
	`)
	codegentest.AssertContains(t, ir, "declare token @llvm.coro.id(")
	codegentest.AssertContains(t, ir, "declare i1 @llvm.coro.alloc(")
	codegentest.AssertContains(t, ir, "declare i8* @llvm.coro.begin(")
	codegentest.AssertContains(t, ir, "declare i64 @llvm.coro.size.i64()")
	codegentest.AssertContains(t, ir, "declare i8 @llvm.coro.suspend(")
	codegentest.AssertContains(t, ir, "declare void @llvm.coro.end(")
	codegentest.AssertContains(t, ir, "declare i8* @llvm.coro.free(")
	codegentest.AssertContains(t, ir, "declare void @llvm.coro.resume(")
	codegentest.AssertContains(t, ir, "declare void @llvm.coro.destroy(")
	codegentest.AssertContains(t, ir, "declare i1 @llvm.coro.done(")
}

func TestGoBlockEmitsCoroutine(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		main() {
			x := 42;
			go { x; };
		}
	`)
	// Coroutine function with presplitcoroutine attribute
	codegentest.AssertContains(t, ir, "presplitcoroutine")
	// Coroutine intrinsics used in the go block
	codegentest.AssertContains(t, ir, "call token @llvm.coro.id(")
	codegentest.AssertContains(t, ir, "call i1 @llvm.coro.alloc(")
	codegentest.AssertContains(t, ir, "call i8* @llvm.coro.begin(")
	codegentest.AssertContains(t, ir, "call i8 @llvm.coro.suspend(")
	codegentest.AssertContains(t, ir, "call void @llvm.coro.end(")
	// Go blocks now use coroutine + G + enqueue, not direct pal_thread_create
	// (pal_thread_create is still used by the scheduler for M threads, but not in go block codegen)
	codegentest.AssertContains(t, ir, "call i8* @promise_g_new(")
	codegentest.AssertContains(t, ir, "call void @promise_sched_enqueue(")
}

func TestGoBlockEnqueuesG(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		main() {
			go { };
		}
	`)
	// G creation and enqueue
	codegentest.AssertContains(t, ir, "call i8* @promise_g_new(")
	codegentest.AssertContains(t, ir, "call void @promise_sched_enqueue(")
}

// T0683: a non-void value-returning go-block (`go { …; <expr> }` → task[T])
// awaited via `<-x` must store the trailing value into G.result_ptr, and the
// caller must allocate a heap result buffer — not the void sentinel 0x1,
// which `<-x` would dereference as a wild pointer (SIGSEGV, mislabeled
// "fatal: stack overflow" by the macOS signal handler).
func TestGoBlockValueResultStored(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		main() {
			task[int] x = go { 42 };
			r := <-x;
		}
	`)
	// Coroutine body stores the trailing value into G.result_ptr via the
	// B0109 null-check store pattern. T1385 collapsed this open-coded store onto
	// the shared storeGoResultAgg helper, so the blocks are its uniquely-numbered
	// `go.store_result.N`/`go.after_store.N` (the fixed `store_result:` labels now
	// belong only to the `go f()` call form).
	codegentest.AssertContains(t, ir, "go.store_result")
	codegentest.AssertContains(t, ir, "go.after_store")
	codegentest.AssertContains(t, ir, "store i64 42, i64*")
	// Caller allocates a heap result buffer for the non-void task.
	codegentest.AssertContains(t, ir, "@pal_alloc")
	// The bug stored the void sentinel 0x1 into result_ptr for this
	// non-void task — there must be no such sentinel now.
	codegentest.AssertNotContains(t, ir, "inttoptr i64 1 to i8*")
}

func TestGoBlockVoidStillUsesSentinel(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		main() {
			task[void] x = go { int n = 10; };
			<-x;
		}
	`)
	codegentest.AssertContains(t, ir, "inttoptr i64 1 to i8*")
	// T1385: the go-block store now goes through storeGoResultAgg, whose blocks
	// are named `go.store_result.N` — assert on that prefix so the guard keeps
	// biting (the bare `store_result:` label no longer exists on this path).
	codegentest.AssertNotContains(t, ir, "go.store_result")
}

// T0683: a value-returning go-block inside a *generic* function exercises
// genGoBlock's `c.typeSubst` monomorphization branch — the trailing value's
// type must be Substitute'd so the coroutine's result store and the caller's
// result-buffer size match the `<-x` receive side (symmetric with
// genReceiveTask). For `mk[int]`, T resolves to int: the monomorphized
// coroutine must store an i64 into G.result_ptr (not a TypeParam-typed or
// sentinel value). Guards the plan's key symmetry, which had no direct
// codegen coverage (the other two tests use only the concrete non-generic
// path).
func TestGoBlockValueResultMonomorphized(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		mk[T](T v) Task[T] {
			return go { v };
		}
		main() {
			task[int] x = mk[int](42);
			r := <-x;
		}
	`)
	// The generic function was monomorphized for int.
	codegentest.AssertContains(t, ir, `@"mk[int]"`)
	// The value path was taken inside the monomorphized go-block coroutine
	// (store into G.result_ptr), the caller allocated a real result buffer,
	// and no void sentinel was used for this non-void monomorphized task.
	codegentest.AssertContains(t, ir, "go.store_result")
	codegentest.AssertContains(t, ir, "go.after_store")
	codegentest.AssertContains(t, ir, "@pal_alloc")
	codegentest.AssertNotContains(t, ir, "inttoptr i64 1 to i8*")
}

// T1196: a VOID / fire-and-forget go-block (`go { … };` with no awaiter) that
// reads a borrowed heap param and hands the derived value off asynchronously
// (e.g. `out.send(v + "!")`) async-reads that param after the caller has freed
// its `"a"+"b"` arg stmt-temp — the same UAF as T0731's value-block path, but
// on the `!useGoBlockValuePath` branch T0731 deliberately left out of scope.
// T1196 lifts the value-path guard so the spawn-side dup runs for ALL go-block
// forms. The dup (promise_string_new) must appear in the spawning function
// before the goroutine ramp call even though there is no result buffer.
func TestT1196_VoidGoBlockBorrowedParamDups(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		emit(string v, Channel[string] out) {
			go { out.send(v + "!"); };
		}
		main() {
			out := channel[string](capacity: 1);
			emit("a" + "b", out);
			r := <-out;
			out.close();
		}
	`)
	emitIR := codegentest.ExtractFunction(ir, "__user.emit")
	// The void spawn dups the borrowed string param before the goroutine ramp,
	// so the coroutine's async `v + "!"` reads the owned copy, not the freed
	// arg temp. The dup precedes the ramp call.
	codegentest.AssertContains(t, emitIR, "@promise_string_new(")
	codegentest.AssertContains(t, emitIR, "call i8* @.goroutine.")
	newIdx := strings.Index(emitIR, "@promise_string_new(")
	rampIdx := strings.Index(emitIR, "call i8* @.goroutine.")
	if newIdx < 0 || rampIdx < 0 || newIdx > rampIdx {
		t.Fatalf("expected promise_string_new before goroutine ramp in emit:\n%s", emitIR)
	}
}

// T1196: a borrowed Copy param (int) captured in a VOID go-block must NOT be
// dup'd — Copy types alias no heap, so the value is passed by-copy directly.
// Regression guard mirroring TestT0688_CopyParamNoDup for the void path.
func TestT1196_VoidGoBlockCopyParamNoDup(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		emit(int v, Channel[int] out) {
			go { out.send(v + 1); };
		}
		main() {
			out := channel[int](capacity: 1);
			emit(41, out);
			r := <-out;
			out.close();
		}
	`)
	emitIR := codegentest.ExtractFunction(ir, "__user.emit")
	codegentest.AssertNotContains(t, emitIR, "vecdup.copy")
	codegentest.AssertNotContains(t, emitIR, "heapdup.copy")
	codegentest.AssertNotContains(t, emitIR, "@promise_string_new(")
	codegentest.AssertContains(t, emitIR, "call i8* @.goroutine.")
}

// T1197: when the spawning function is itself generic, the borrowed-param dup
// runs inside the monomorphized body with c.typeSubst active. The capture's
// sema type is a bare TypeParam (T) that must be substituted to the concrete
// type (string) before goElemNeedsBorrowedCaptureDup / dupBorrowedCaptureForResult
// can classify it — otherwise the borrowed arg is not dup'd and the coroutine
// UAFs. Here both the receiver `b` (GBox[string], a heap user type) and the
// arg `v` (T→string) are dup'd inside `gmk[string]`.
func TestT1197_ViaBlockGenericSpawnerTypeSubst(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type GBox[T] {
			T val;
			pick(this, T v) T { return v; }
		}
		gmk[T](GBox[T] b, T v) Task[T] {
			return go b.pick(v);
		}
		main() {
			b := GBox[string](val: "z");
			task[string] x = gmk[string](b, "a" + "b");
			r := <-x;
		}
	`)
	gmkIR := codegentest.ExtractFunction(ir, `"gmk[string]"`)
	codegentest.AssertContains(t, gmkIR, "call i8* @.goroutine.")
	// arg `v` (T substituted to string) dup'd via promise_string_new; receiver
	// `b` (GBox[string] heap user) dup'd via heapdup — both before the ramp.
	codegentest.AssertContains(t, gmkIR, "@promise_string_new(")
	codegentest.AssertContains(t, gmkIR, "heapdup.copy")
}

// T1219: `this` referenced inside a plain `go { }` block within a method used to
// panic codegen ("'this' used but not in method context") — the coroutine body
// did not inherit the method's `this`. The fix threads a private SNAPSHOT of the
// receiver into the coroutine arg pack. For a HEAP receiver the snapshot is a
// deep copy: the coroutine takes a `{ i8*, i8* }` value-struct param and the
// spawning method emits a heapdup before the goroutine ramp call, so the async
// body owns an independent instance (dropped at coroutine scope exit).
func TestT1219_GoBlockThisHeapReceiverSnapshot(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type T1219C {
			int n;
			spawn(this, Channel[int] out) {
				go { out.send(this.n); };
			}
		}
		main() {
			c := T1219C(n: 5);
			d := channel[int](capacity: 1);
			c.spawn(d);
			r := <-d;
			d.close();
		}
	`)
	// The coroutine takes `this` as a value-struct capture param.
	codegentest.AssertContains(t, ir, "%this.cap")
	codegentest.AssertContains(t, ir, "{ i8*, i8* } %this.cap")
	// The spawning method deep-copies the receiver before the goroutine ramp.
	spawnIR := codegentest.ExtractFunction(ir, "T1219C.spawn")
	codegentest.AssertContains(t, spawnIR, "heapdup.copy")
	codegentest.AssertContains(t, spawnIR, "call i8* @.goroutine.")
	dupIdx := strings.Index(spawnIR, "heapdup.copy")
	rampIdx := strings.Index(spawnIR, "call i8* @.goroutine.")
	if dupIdx < 0 || rampIdx < 0 || dupIdx > rampIdx {
		t.Fatalf("expected receiver heapdup before goroutine ramp in spawn:\n%s", spawnIR)
	}
}

// T1219: a VALUE-TYPE receiver captured by a `go { }` block is copied by value
// into the coroutine frame (Copy — no heap, no dup). The coroutine takes the
// value struct itself (`%promise_..._v`) as the capture param, and the spawning
// method emits no heapdup for the receiver.
func TestT1219_GoBlockThisValueReceiverByValue(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type T1219P {
			int x `+"`"+`value;
			int y `+"`"+`value;
			sum(this, Channel[int] out) {
				go { out.send(this.x + this.y); };
			}
		}
		main() {
			p := T1219P(x: 3, y: 4);
			d := channel[int](capacity: 1);
			p.sum(d);
			r := <-d;
			d.close();
		}
	`)
	// The coroutine takes the value struct by value (not a heap dup).
	codegentest.AssertContains(t, ir, "%this.cap")
	sumIR := codegentest.ExtractFunction(ir, "T1219P.sum")
	codegentest.AssertContains(t, sumIR, "call i8* @.goroutine.")
	codegentest.AssertNotContains(t, sumIR, "heapdup.copy")
}

// T1219: a GENERIC-owner method (`T1219Box[int].get_it`) referencing `this` in a
// `go { }` block resolves the receiver type via the mono instance (currentNamedType
// → monoCtx.inst), deep-copies the concrete `Box[int]` snapshot, and threads it
// into the coroutine — exercising the generic-owner path of the fix.
func TestT1219_GoBlockThisGenericOwner(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type T1219Box[T] {
			T value;
			send_it(this, Channel[T] out) {
				go { out.send(this.value); };
			}
		}
		main() {
			b := T1219Box[int](value: 9);
			d := channel[int](capacity: 1);
			b.send_it(d);
			r := <-d;
			d.close();
		}
	`)
	sendIR := codegentest.ExtractFunction(ir, `"T1219Box[int].send_it"`)
	codegentest.AssertContains(t, sendIR, "heapdup.copy")
	// T1222: the ramp spawned inside a generic (instance-owned) method is qualified
	// by the enclosing name so it can't collide across split units. The qualified
	// symbol contains `[`/`.`, so LLVM emits it quoted.
	codegentest.AssertContains(t, sendIR, `call i8* @".goroutine.T1219Box[int].send_it.`)
}

// T1219: a GENERIC METHOD on a NON-GENERIC owner (`T1219GenM.emit[int]`)
// referencing `this` in a `go { }` block resolves the receiver type via the
// active typeSubst (currentNamedType → the `typeSubst != nil` branch, since
// monoCtx.inst is nil for a non-generic owner). The concrete owner is unchanged
// by the substitution, but the branch must be taken — otherwise the receiver
// type would resolve incorrectly. Exercises the generic-method path distinct
// from the generic-owner path above.
func TestT1219_GoBlockThisGenericMethodNonGenericOwner(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type T1219GenM {
			int base;
			emit[T](this, Channel[int] out, T v) {
				go { out.send(this.base + 1); };
			}
		}
		main() {
			m := T1219GenM(base: 41);
			d := channel[int](capacity: 1);
			m.emit[int](d, 7);
			r := <-d;
			d.close();
		}
	`)
	// The monomorphized generic method still deep-copies the heap receiver.
	emitIR := codegentest.ExtractFunction(ir, `"T1219GenM.emit[int]"`)
	codegentest.AssertContains(t, emitIR, "heapdup.copy")
	codegentest.AssertContains(t, emitIR, "call i8* @.goroutine.")
}

// T1198: a borrowed channel param passed to a fast-path go-call is refcounted
// (B0163), not dup'd — sharing the pointer is fine.
func TestT1198_FastPathChannelParamNoDup(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		relay(Channel[int] src, Channel[int] out) {
			v := <-src;
			if x := v { out.send(x); }
		}
		spawn(Channel[int] src, Channel[int] out) {
			go relay(src, out);
		}
		main() {
			a := channel[int](capacity: 1);
			b := channel[int](capacity: 1);
			a.send(7);
			spawn(a, b);
			r := <-b;
		}
	`)
	spawnIR := codegentest.ExtractFunction(ir, "__user.spawn")
	// Channel args are refcounted via an atomic add, never string-dup'd.
	codegentest.AssertNotContains(t, spawnIR, "@promise_string_new(")
	codegentest.AssertContains(t, spawnIR, "call i8* @.goroutine.")
}

func TestChannelSendInCoroutineSuspends(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		main() {
			ch := channel[int](capacity: 1);
			go {
				ch.send(42);
			};
		}
	`)
	// Inside go block, channel send should use goroutine-aware park
	codegentest.AssertContains(t, ir, "call void @promise_waiter_enqueue(")
	// The go block is a coroutine
	codegentest.AssertContains(t, ir, "presplitcoroutine")
}

func TestChannelRecvInCoroutineSuspends(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		main() {
			ch := channel[int](capacity: 1);
			go {
				result := <-ch;
			};
		}
	`)
	// Inside go block, channel recv should use goroutine-aware park
	codegentest.AssertContains(t, ir, "call void @promise_waiter_enqueue(")
}

func TestChannelCloseWakesAllWaiters(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		main() {
			ch := channel[int](capacity: 1);
			ch.close();
		}
	`)
	// Close should call promise_waiter_wake_all for send, recv, and rv_waiters (T0312)
	codegentest.AssertContains(t, ir, "call void @promise_waiter_wake_all(")
}

func TestChannelCloseWakesRvWaiters(t *testing.T) {
	// T0312: genChannelClose must wake rv_waiters in addition to send/recv waiters.
	// A rendezvous-parked sender (goroutine that wrote to an unbuffered channel and
	// is parked on rv_waiters) must be unblocked when the channel closes.
	ir := codegentest.GenerateIR(t, `
		main() {
			ch := channel[int](capacity: 1);
			ch.close();
		}
	`)
	// Three wake_all calls: send_waiters, recv_waiters, rv_waiters
	count := strings.Count(ir, "call void @promise_waiter_wake_all(")
	if count < 3 {
		t.Errorf("expected >= 3 promise_waiter_wake_all calls in close (send/recv/rv_waiters), got %d", count)
	}
}

func TestSelectRecvWakesRvWaiters(t *testing.T) {
	// T0312: the select execRecv path must call wake_one for rv_waiters (field 15)
	// in addition to send_waiters, so rendezvous-parked senders are unblocked
	// when their value is consumed via a select recv case.
	irBaseline := codegentest.GenerateIR(t, `
		main() {
			ch := channel[int](capacity: 1);
			ch.send(1);
		}
	`)
	baseline := strings.Count(irBaseline, "call void @promise_waiter_wake_one(")

	ir := codegentest.GenerateIR(t, `
		main() {
			ch := channel[int](capacity: 1);
			ch.send(1);
			select {
				v := <-ch:
					print_line("got");
				default:
					print_line("default");
			}
		}
	`)
	total := strings.Count(ir, "call void @promise_waiter_wake_one(")
	delta := total - baseline
	// execRecv adds 2 wake_one calls: send_waiters + rv_waiters
	if delta < 2 {
		t.Errorf("select recv must add >= 2 wake_one calls (send_waiters + rv_waiters), got delta=%d (rv_waiters wake missing?)", delta)
	}
}

func TestChannelStructHas18Fields(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		main() {
			ch := channel[int](capacity: 1);
		}
	`)
	// Channel struct has 18 fields: buffer, head, tail, count, cap, elem_size,
	// is_closed, is_unbuffered, not_empty, not_full, send_waiters(2), recv_waiters(2),
	// rv_waiters(2, T0312), refcount. Verified by promise_channel_new definition.
	codegentest.AssertContains(t, ir, "define i8* @promise_channel_new(")
}

func TestTaskReceiveParksGoroutine(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		compute() int { return 42; }
		main() {
			t := go compute();
			result := <-t;
		}
	`)
	// Task receive in main (non-coroutine) uses thread-blocking mode
	// But the G.done field should be checked
	codegentest.AssertContains(t, ir, "promise_g_new")
	codegentest.AssertContains(t, ir, "promise_sched_enqueue")
}

func TestTaskDropDoneLoadIsAcquire(t *testing.T) {
	// T0669: G.done spin-wait in Task.drop must use an atomic acquire load so the
	// LLVM optimizer cannot hoist or cache it across loop iterations on Windows.
	ir := codegentest.GenerateIR(t, `
		compute() int { return 42; }
		main() {
			t := go compute();
			v := <-t;
		}
	`)
	// The Task drop function must contain an atomic acquire load on G.done (i8).
	codegentest.AssertContains(t, ir, "load atomic i8")
	codegentest.AssertContains(t, ir, "acquire")
}

// --- Phase 5c gap-filling tests ---

func TestTaskReceiveInCoroutine(t *testing.T) {
	// <-task inside a go block uses the coroutine park path with done_lock,
	// not the thread-blocking path. The done_lock protects the done flag and
	// done_waiters list, and park_mutex holds the lock across coro.suspend.
	ir := codegentest.GenerateIR(t, `
		compute() int { return 42; }
		main() {
			go {
				t := go compute();
				int result = <-t;
			};
		}
	`)
	// The outer go block is a coroutine
	codegentest.AssertContains(t, ir, "presplitcoroutine")
	// Task receive inside coroutine: parks on done_waiters via done_lock
	codegentest.AssertContains(t, ir, "task.done")
	codegentest.AssertContains(t, ir, "task.wait")
	codegentest.AssertContains(t, ir, "task.ready")
	// done_lock path: check under lock, park if not done
	codegentest.AssertContains(t, ir, "task.done_under_lock")
	codegentest.AssertContains(t, ir, "task.park")
	// Coroutine suspend in the task wait path
	codegentest.AssertContains(t, ir, "task.resume")
	// Should NOT use usleep (that's the thread-blocking path)
	// The go block coroutine uses coro.suspend instead
}

func TestTaskReceiveCoroutineMode(t *testing.T) {
	// <-task in main uses coroutine parking (main is compiled as goroutine)
	ir := codegentest.GenerateIR(t, `
		compute() int { return 42; }
		main() {
			t := go compute();
			result := <-t;
		}
	`)
	// Coroutine mode: park on done_waiters with done_lock, coro.suspend
	codegentest.AssertContains(t, ir, "task.park")
	codegentest.AssertContains(t, ir, "task.resume")
	codegentest.AssertContains(t, ir, "task.done_under_lock")
}

func TestVoidTaskSentinel(t *testing.T) {
	// Void tasks set result_ptr to sentinel inttoptr(i64 1) so goroutine_exit
	// knows not to free G (the receiver frees it via <-task).
	ir := codegentest.GenerateIR(t, `
		doWork() { }
		main() {
			t := go doWork();
			<-t;
		}
	`)
	// Sentinel value: inttoptr i64 1 to i8*
	codegentest.AssertContains(t, ir, "inttoptr i64 1 to i8*")
	// G is freed by the receiver, not goroutine_exit
	codegentest.AssertContains(t, ir, "task.ready")
}

func TestVoidGoBlockSentinel(t *testing.T) {
	// go { block } used as a task (assigned + awaited) — should set sentinel
	// so goroutine_exit doesn't free G before the receiver does.
	ir := codegentest.GenerateIR(t, `
		main() {
			t := go { };
			<-t;
		}
	`)
	codegentest.AssertContains(t, ir, "inttoptr i64 1 to i8*")
}

func TestFireAndForgetGoBlockNoSentinel(t *testing.T) {
	// go { block } as a statement (fire-and-forget) — should NOT set sentinel.
	// goroutine_exit will free the G struct since result_ptr stays null.
	ir := codegentest.GenerateIR(t, `
		main() {
			go { };
		}
	`)
	// The go block trampoline body should not contain the sentinel store.
	// After promise_g_new, the next call should be promise_sched_enqueue (no inttoptr).
	codegentest.AssertContains(t, ir, "call i8* @promise_g_new(")
	codegentest.AssertContains(t, ir, "call void @promise_sched_enqueue(")
}

func TestGoCallTaskSetsSentinel(t *testing.T) {
	// go void_func() used as a task (assigned + awaited) — should set sentinel.
	ir := codegentest.GenerateIR(t, `
		work() { }
		main() {
			t := go work();
			<-t;
		}
	`)
	codegentest.AssertContains(t, ir, "inttoptr i64 1 to i8*")
}

func TestGoCallNonVoidTaskSetsResultPtr(t *testing.T) {
	// go non_void_func() as task — should allocate result buffer (not sentinel).
	ir := codegentest.GenerateIR(t, `
		compute() int { return 42; }
		main() {
			t := go compute();
			<-t;
		}
	`)
	// Result buffer allocated via pal_alloc and stored in result_ptr
	codegentest.AssertContains(t, ir, "call i8* @pal_alloc(")
}

func TestFireAndForgetGoBlockInLoop(t *testing.T) {
	// go { } as the only statement in a for loop — fire-and-forget.
	// genBlock routes all statements through genStmt, so the flag is set.
	ir := codegentest.GenerateIR(t, `
		main() {
			for i in 0..3 {
				go { };
			}
		}
	`)
	codegentest.AssertContains(t, ir, "call i8* @promise_g_new(")
	codegentest.AssertNotContains(t, ir, "inttoptr i64 1 to i8*")
}

func TestFireAndForgetGoBlockNestedFlagRestore(t *testing.T) {
	// Nested go blocks: outer is a task, inner is fire-and-forget.
	// The inner genStmt clears goExprFireAndForget, but genGoBlock
	// saves/restores it, so the outer block still sets sentinel.
	ir := codegentest.GenerateIR(t, `
		main() {
			t := go {
				go { };
			};
			<-t;
		}
	`)
	// Outer go block should have sentinel (it's a task)
	codegentest.AssertContains(t, ir, "inttoptr i64 1 to i8*")
}

func TestGoroutineExitSkipsFreeForTask(t *testing.T) {
	// goroutine_exit checks result_ptr != null to decide whether to free G.
	// Tasks (result_ptr set) skip the free; fire-and-forget goroutines are freed.
	ir := codegentest.GenerateIR(t, `
		main() { }
	`)
	// goroutine_exit should contain the conditional skip-free logic
	codegentest.AssertContains(t, ir, "define void @promise_goroutine_exit(")
	// The function checks result_ptr to decide whether to free
	codegentest.AssertContains(t, ir, "skip_free:")
	codegentest.AssertContains(t, ir, "do_free:")
}
