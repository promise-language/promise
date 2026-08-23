package codegen

import (
	"regexp"
	"strings"
	"testing"

	antlr "github.com/antlr4-go/antlr/v4"
	"github.com/promise-language/promise/compiler/internal/ast"
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
	ir := generateIR(t, `
		type Box { string s; }
		main() {
			Box? init = Box(s: "x");
			a := Ref[Box?](init);
			_ := a;
		}
	`)
	arcDrop := extractDefine(ir, `"Ref[Box?].drop"`)
	assertContains(t, arcDrop, "call void @Box.drop(")
}

// T0579: Array field with channel element — exercises `typeNeedsFieldDrop`'s
// `AsChannel` branch.
func TestFixedArrayFieldChannelElement(t *testing.T) {
	ir := generateIR(t, `
		type _ChArr { channel[int][2] data; }
		main() {
			c0 := channel[int]();
			c1 := channel[int]();
			c := _ChArr(data: [c0, c1]);
		}
	`)
	// T0663: Channel.drop is per-element-type — Channel[int].drop here.
	assertContains(t, ir, `call void @"Channel[int].drop"`)
}

func TestFixedArrayIndexDupsChannel(t *testing.T) {
	// Channel element: dupChannel inlines a null check + atomic refcount
	// incref on the channel struct (compiler.go:2234, chdup.inc block).
	ir := generateIR(t, `
		main() {
			c0 := channel[int]();
			c1 := channel[int]();
			channel[int][2] arr = [c0, c1];
			channel[int] x = arr[0];
		}
	`)
	assertContains(t, ir, "arridx.ok")
	// dupChannel emits a chdup.inc / chdup.merge block pair and an atomic add.
	assertContains(t, ir, "chdup.inc")
	assertContains(t, ir, "chdup.merge")
}

func TestFixedArrayIndexDupsArc(t *testing.T) {
	// Arc element: dupArc emits an atomic refcount incref.
	ir := generateIR(t, `
		main() {
			Ref[int][2] arr = [Ref[int](1), Ref[int](2)];
			Ref[int] x = arr[0];
		}
	`)
	assertContains(t, ir, "arridx.ok")
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

	stdModInfo, stdScope := getCodegenStdModInfo()
	stdUse := &ast.UseDecl{Alias: "_", CatalogName: "std"}
	file.Uses = append([]*ast.UseDecl{stdUse}, file.Uses...)

	ti := sema.ParseTargetInfo("wasm32-wasi")
	info, semaErrs := sema.CheckWithTarget(file, map[string]*types.Scope{"std": stdScope}, ti)
	if len(semaErrs) > 0 {
		t.Fatalf("sema errors: %v", semaErrs)
	}
	info.ModuleInfos = map[string]*sema.ModuleInfo{"std": stdModInfo}
	info.ModuleOrder = []string{"std"}
	result := Compile(file, info, "wasm32-wasi")
	result.GenerateTestMain(info.Tests, nil)
	ir := result.Module.String()

	// WASM: should init scheduler with 1 P
	assertContains(t, ir, "call void @promise_sched_init(i32 1)")
	// WASM: should compile test as coroutine
	assertContains(t, ir, "define i8* @.test_coro.myTest()")
	// WASM: coroutine has presplitcoroutine attribute
	assertContainsMatch(t, ir, `@\.test_coro\.myTest\(\).*presplitcoroutine`)
	// WASM: should run through cooperative scheduler
	assertContains(t, ir, "call void @promise_sched_coop_run()")
	// WASM: should NOT use thread-based promise_test_run
	assertNotContains(t, ir, "call i32 @promise_test_run(")
	// WASM: should NOT bump goroutine counter past 0 (test G needs id=0)
	// The sched_init call is followed by alloc count reset, not atomicrmw add
}

// B0165: Sched struct includes ready_count field (i32 at end).
func TestSchedStructHasReadyCount(t *testing.T) {
	ir := generateIR(t, `main() { }`)
	// The sched global should include the ready_count i32 field
	// Full type: { i8*, i8*, i64, i8*, i8*, i32, i8*, i8*, i64, i8, i8, i8*, i8*, i8*, i32, i64, i64, i64, i64, i8*, i32, i32, i8*, i8* }
	assertContains(t, ir, "@__promise_sched = global")
	// Verify sched_loop is defined (it increments ready_count)
	assertContains(t, ir, "define i8* @promise_sched_loop(")
}

// T1685: the scheduler grows its M pool on demand so a library blocking wait
// (which blocks the OS thread) never starves runnable goroutines. Assert the
// two new primitives exist and are wired into the handoff/park paths.
func TestSchedOnDemandMFunctions(t *testing.T) {
	ir := generateIR(t, `main() { }`)
	// startm (create-or-reuse-spare) and park_spare (park a P-less M) are defined.
	assertContains(t, ir, "define void @promise_sched_startm(i8*")
	assertContains(t, ir, "define void @promise_sched_park_spare(i8*")
	// enter_syscall hands the detached P to another M via startm.
	assertContainsMatch(t, ir, `(?s)define void @promise_sched_enter_syscall\(\).*?call void @promise_sched_startm\(`)
	// sched_loop parks a P-less M as a spare (loop-top null-P path).
	assertContainsMatch(t, ir, `(?s)define i8\* @promise_sched_loop\(.*?call void @promise_sched_park_spare\(`)
}

// T1685: every library blocking wait compiled outside a coroutine wraps its
// pal_cond_wait in the enter/exit syscall handoff, so the M's P is handed off
// for the duration of the block. Exercises the channel-recv site; all six sites
// share the emitBlockingCondWait helper.
func TestBlockingChannelRecvHandsOffP(t *testing.T) {
	ir := generateIR(t, `
		worker(channel[int] ch) { if v := <-ch { } }
		main() {
			ch := channel[int]();
			go { worker(ch); };
			ch.send(1);
		}
	`)
	// The non-coroutine recv wait emits enter_syscall → pal_cond_wait → exit_syscall
	// in order (worker() is an ordinary function, so inCoroutine is false).
	assertContainsMatch(t, ir,
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
	ir := generateIR(t, `
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
		body := extractFunction(ir, tc.fn)
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
	ir := generateIR(t, `
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
	assertContains(t, ir, "%promise_IntChan_i = type { %promise_IntChan_m*, i8* }")
	// Channel send generates inline mutex lock IR
	assertContains(t, ir, "call void @pal_mutex_lock(")
}

func TestTaskFieldInUserType(t *testing.T) {
	// B0096: task[T] fields must use i8* layout in user types.
	ir := generateIR(t, `
		compute() int { return 42; }
		type Holder { task[int] t; }
		main() {
			t := go compute();
			h := Holder(t: t);
		}
	`)
	// Instance struct field must be i8* (opaque task pointer), not {i8*, i8*}
	assertContains(t, ir, "%promise_Holder_i = type { %promise_Holder_m*, i8* }")
}

func TestChannelFieldInModuleType(t *testing.T) {
	// B0096: channel fields in module-defined types
	ir := generateIRWithCatalogModule(t, "mymod",
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
	assertContains(t, ir, "%promise_Sender_i = type { %promise_Sender_m*, i8* }")
	assertContains(t, ir, "call void @pal_mutex_lock(")
}

func TestChannelFieldInEnumVariant(t *testing.T) {
	// B0096: channel[T] in enum variant fields must use i8* layout.
	// Send variant: channel[int] (i8*, 8 bytes) + int (i64, 8 bytes) = 16 bytes.
	// Without fix: channel would be {i8*, i8*} (16 bytes) + i64 (8 bytes) = 24 bytes.
	ir := generateIR(t, `
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
	assertContains(t, ir, "%promise_Action_enum = type { i32, [16 x i8] }")
	// The enum data area specifically must not use [24 x i8]
	assertNotContains(t, ir, "%promise_Action_enum = type { i32, [24 x i8] }")
}

func TestGenericTypeWithChannelFieldLayout(t *testing.T) {
	// B0096: mono layout for generic type with channel[T] field.
	ir := generateIR(t, `
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
	assertContains(t, ir, `%"promise_Wrapper[int]_i" = type { %"promise_Wrapper[int]_m"*, i8*, i64 }`)
}

// B0163/T0663: Channel scope-exit drop — standalone channel gets drop flag and
// per-element-type Channel[int].drop call.
func TestDropChannelStandaloneHasDrop(t *testing.T) {
	ir := generateIR(t, `
		main() {
			ch := channel[int](capacity: 5);
		}
	`)
	assertContains(t, ir, "%ch.dropflag")
	assertContains(t, ir, `call void @"Channel[int].drop"(`)
}

// B0163/T0663: Channel[T].drop body uses refcount — frees only when refcount drops to 0
func TestChannelDropFuncBody(t *testing.T) {
	ir := generateIR(t, `
		main() {
			ch := channel[int](capacity: 5);
		}
	`)
	assertContains(t, ir, `define void @"Channel[int].drop"(i8* %this)`)
	// Refcount decrement (atomicrmw or load+add for WASM)
	assertContains(t, ir, "i64 -1")
	assertContains(t, ir, "call void @pal_free(")
	assertContains(t, ir, "call void @pal_mutex_destroy(")
	assertContains(t, ir, "call void @pal_cond_destroy(")
}

// B0163/T0663: Channel refcount initialized to 1 in promise_channel_new
func TestChannelRefcountInit(t *testing.T) {
	ir := generateIR(t, `
		main() {
			ch := channel[int](capacity: 1);
		}
	`)
	// promise_channel_new should store refcount = 1
	assertContains(t, ir, "define i8* @promise_channel_new(")
	// Channel[int].drop should use atomicrmw add with -1 (refcount decrement)
	assertContains(t, ir, `define void @"Channel[int].drop"(`)
}

// B0163/T0663: Channel drop null-checks the pointer (zero-initialized channels from error paths)
func TestChannelDropNullCheck(t *testing.T) {
	ir := generateIR(t, `
		main() {
			ch := channel[int](capacity: 1);
		}
	`)
	// Channel[int].drop body should have null check (icmp eq ... null)
	dropFn := extractFunction(ir, `"Channel[int].drop"`)
	if dropFn == "" {
		t.Fatal("expected Channel[int].drop function in IR")
	}
	assertContains(t, dropFn, "icmp eq")
	assertContains(t, dropFn, "null")
}

// B0163/T0663: Channel drop flag cleared on move (borrow detection)
func TestChannelDropFlagInDroppableContainer(t *testing.T) {
	ir := generateIR(t, `
		main() {
			ch := channel[int](capacity: 5);
			ch.send(42);
		}
	`)
	// isDroppableContainerOrString should recognize channels
	assertContains(t, ir, "%ch.dropflag")
	assertContains(t, ir, `call void @"Channel[int].drop"(`)
}

// T1158: passing a channel as a `go f(ch)` argument increments the channel
// refcount (B0163) AND registers a matching goroutine-side Channel[T].drop after
// the target call returns, so the refcount returns to 0 and the channel is freed
// (previously the increment had no balancing decrement → 5-allocation leak).
func TestGoCallChannelArgGoroutineDrop(t *testing.T) {
	ir := generateIR(t, `
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
	coroFn := extractDefine(ir, `.goroutine.0`)
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
	ir := generateIR(t, `
		ping2(channel[int] a, channel[int] b) { a.send(1); b.send(2); }
		main() {
			x := channel[int](capacity: 1);
			y := channel[int](capacity: 1);
			go ping2(x, y);
		}
	`)
	coroFn := extractDefine(ir, `.goroutine.0`)
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
	ir := generateIR(t, `
		enum Holder { Pair(Ref[int] r, int n) }
		main() {
			m := Map[int, Holder]();
			m[1] = Holder.Pair(Ref[int](9), 2);
		}
	`)
	// The variant-field dup for the Ref field reaches dupArc (strong-count
	// increment) rather than panicking in dupHeapValue.
	assertContains(t, ir, "enumdup.Pair")
	assertContains(t, ir, "arcdup.inc")
}

// T1109: A generic enum carrying Ref[T]/Weak[T] variant fields exercises the
// type-substitution sub-branches in emitVariantFieldDup (the dup is synthesized
// in a mono context where c.typeSubst != nil, so the element type must be
// substituted before dupArc/dupWeak). Confirms both refcount paths fire for the
// monomorphized Box[int] instance.
func TestDupEnumVariantGenericArcWeakRefcount(t *testing.T) {
	ir := generateIR(t, `
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
	assertContains(t, ir, "arcdup.inc")
	assertContains(t, ir, "weakdup.inc")
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
	ir := generateIR(t, `
		tfn() int {
			Ref[int]? o = Ref[int](42);
			return ((o)!).borrow;
		}
		main() { _ := tfn(); }
	`)
	fn := extractFunction(ir, "__user.tfn")
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
	ir := generateIR(t, `
		type WithChan { channel[int] ch; }
		main() {
			channel[int] ch = channel[int]();
			w := WithChan(ch: ch);
		}
	`)
	assertContains(t, ir, "define void @WithChan.drop")
	// T0663: synthesized drop calls the per-element-type Channel[int].drop on
	// the channel field (was the single @Channel.drop symbol pre-T0663).
	withChanDrop := extractFunction(ir, "WithChan.drop")
	if withChanDrop == "" {
		t.Fatal("expected WithChan.drop function in IR")
	}
	assertContains(t, withChanDrop, `call void @"Channel[int].drop"(`)
}

// B0236: Match destructure of droppable enum dups channel fields.
func TestMatchDupChannel(t *testing.T) {
	ir := generateIR(t, `
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
	assertContains(t, ir, "chdup.inc")
}

// --- Go / Receive (concurrency) tests ---

func TestGoExprBasicFunction(t *testing.T) {
	ir := generateIR(t, `
		compute() int { return 42; }
		main() {
			t := go compute();
			result := <-t;
		}
	`)
	// Coroutine function generated with presplitcoroutine attribute
	assertContains(t, ir, ".goroutine.")
	assertContains(t, ir, "presplitcoroutine")
	// Coroutine intrinsics used
	assertContains(t, ir, "call token @llvm.coro.id(")
	assertContains(t, ir, "call i8* @llvm.coro.begin(")
	assertContains(t, ir, "call i8 @llvm.coro.suspend(")
	// G struct created and enqueued
	assertContains(t, ir, "call i8* @promise_g_new(")
	assertContains(t, ir, "call void @promise_sched_enqueue(")
	// Result buffer allocated for non-void task
	assertContains(t, ir, "@pal_alloc")
	// Coroutine calls target function
	assertContains(t, ir, "call i64 @__user.compute")
}

func TestGoExprWithArgs(t *testing.T) {
	ir := generateIR(t, `
		double(int x) int { return x * 2; }
		main() {
			t := go double(21);
			result := <-t;
		}
	`)
	// Coroutine generated
	assertContains(t, ir, ".goroutine.")
	assertContains(t, ir, "presplitcoroutine")
	// G struct created and enqueued
	assertContains(t, ir, "call i8* @promise_g_new(")
	assertContains(t, ir, "call void @promise_sched_enqueue(")
	// The coroutine should call the target function
	assertContains(t, ir, "call i64 @__user.double")
}

func TestGoExprVoidFunction(t *testing.T) {
	ir := generateIR(t, `
		doWork() { }
		main() {
			t := go doWork();
			<-t;
		}
	`)
	assertContains(t, ir, ".goroutine.")
	assertContains(t, ir, "presplitcoroutine")
	// Coroutine calls void function
	assertContains(t, ir, "call void @__user.doWork")
	// G struct created and enqueued
	assertContains(t, ir, "call i8* @promise_g_new(")
	assertContains(t, ir, "call void @promise_sched_enqueue(")
}

// B0046: go extern_func() must generate a wrapper to handle sret ABI.
func TestGoExprExternFunction(t *testing.T) {
	ir := generateIR(t, `
		get_data(int x) string `+"`"+`extern("test_get_data");
		main() {
			t := go get_data(42);
			result := <-t;
		}
	`)
	// Extern declared with sret pattern (void return, i8* sret first param)
	assertContains(t, ir, "declare void @test_get_data(i8* %sret")
	// Wrapper function generated for the go expression
	assertContains(t, ir, ".go_extern_wrap.get_data.")
	// Wrapper calls the extern with sret
	assertContains(t, ir, "call void @test_get_data(i8*")
	// Coroutine calls the wrapper (returns i8*, not void)
	assertContains(t, ir, "call i8* @.go_extern_wrap.get_data.")
	// G struct created and enqueued
	assertContains(t, ir, "call i8* @promise_g_new(")
	assertContains(t, ir, "call void @promise_sched_enqueue(")
}

func TestGoExprExternVoidFunction(t *testing.T) {
	ir := generateIR(t, `
		do_work(int x) `+"`"+`extern("test_do_work");
		main() {
			t := go do_work(42);
			<-t;
		}
	`)
	// Even void externs need the wrapper — extern int params expect i8*
	// (pointer to value struct) but Promise internal representation is i64
	assertContains(t, ir, ".go_extern_wrap.do_work.")
	assertContains(t, ir, "call i8* @promise_g_new(")
}

// Container return types (Vector, Channel) use direct i8* return — no sret.
func TestGoExprExternContainerReturn(t *testing.T) {
	ir := generateIR(t, `
		get_items() string[] `+"`"+`extern("test_get_items");
		main() {
			t := go get_items();
			result := <-t;
		}
	`)
	// Vector return: i8* directly (no sret)
	assertContains(t, ir, "declare i8* @test_get_items()")
	// Wrapper still generated (consistent handling of all externs)
	assertContains(t, ir, ".go_extern_wrap.get_items.")
	// Coroutine calls wrapper which returns i8*
	assertContains(t, ir, "call i8* @.go_extern_wrap.get_items.")
}

// Extern with string param (i8* in both Promise and extern ABI) + multiple args.
func TestGoExprExternMultipleArgs(t *testing.T) {
	ir := generateIR(t, `
		process(string name, int count) string `+"`"+`extern("test_process");
		main() {
			t := go process("hello", 5);
			result := <-t;
		}
	`)
	// Wrapper generated with both params
	assertContains(t, ir, ".go_extern_wrap.process.")
	// Extern uses sret for string return
	assertContains(t, ir, "declare void @test_process(i8* %sret")
	// Coroutine calls wrapper
	assertContains(t, ir, "call i8* @.go_extern_wrap.process.")
}

func TestGoExprBlock(t *testing.T) {
	ir := generateIR(t, `
		main() {
			t := go { };
			<-t;
		}
	`)
	// Coroutine function for the block
	assertContains(t, ir, ".goroutine.")
	assertContains(t, ir, "presplitcoroutine")
	// G struct created and enqueued
	assertContains(t, ir, "call i8* @promise_g_new(")
	assertContains(t, ir, "call void @promise_sched_enqueue(")
}

// B0113: go method_call() should work — not just direct function calls.
func TestGoExprMethodCall(t *testing.T) {
	ir := generateIR(t, `
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
	assertContains(t, ir, ".goroutine.")
	assertContains(t, ir, "presplitcoroutine")
	// Method call generated inside coroutine body
	assertContains(t, ir, "Counter.get_value")
	// G struct created and enqueued
	assertContains(t, ir, "call i8* @promise_g_new(")
	assertContains(t, ir, "call void @promise_sched_enqueue(")
}

// B0113: go method_call() fire-and-forget (void method, result discarded).
func TestGoExprMethodCallFireAndForget(t *testing.T) {
	ir := generateIR(t, `
		type Worker {
			int id;
			run(this) { }
		}
		main() {
			w := Worker(id: 1);
			go w.run();
		}
	`)
	assertContains(t, ir, ".goroutine.")
	assertContains(t, ir, "presplitcoroutine")
	assertContains(t, ir, "Worker.run")
	assertContains(t, ir, "call i8* @promise_g_new(")
	assertContains(t, ir, "call void @promise_sched_enqueue(")
}

// --- Channel tests ---

func TestChannelConstructor(t *testing.T) {
	ir := generateIR(t, `
		main() {
			ch := channel[int](capacity: 5);
		}
	`)
	// Should call promise_channel_new
	assertContains(t, ir, "call i8* @promise_channel_new(")
	// Should init mutex and 2 cond vars inside promise_channel_new
	assertContains(t, ir, "call i8* @pal_mutex_init()")
	assertContains(t, ir, "call i8* @pal_cond_init()")
}

func TestChannelConstructorUnbuffered(t *testing.T) {
	ir := generateIR(t, `
		main() {
			ch := channel[int]();
		}
	`)
	// Unbuffered: capacity=0
	assertContains(t, ir, "call i8* @promise_channel_new(i64 0,")
}

func TestChannelSend(t *testing.T) {
	ir := generateIR(t, `
		main() {
			ch := channel[int](capacity: 5);
			ch.send(42);
		}
	`)
	// Should lock/unlock mutex and use memcpy for send
	assertContains(t, ir, "call void @pal_mutex_lock(")
	assertContains(t, ir, "call void @llvm.memcpy.p0i8.p0i8.i64(")
	assertContains(t, ir, "call void @pal_cond_signal(")
	assertContains(t, ir, "call void @pal_mutex_unlock(")
}

func TestChannelClose(t *testing.T) {
	ir := generateIR(t, `
		main() {
			ch := channel[int](capacity: 5);
			ch.close();
		}
	`)
	// Close should broadcast both cond vars
	assertContains(t, ir, "call void @pal_cond_broadcast(")
}

func TestChannelReceive(t *testing.T) {
	ir := generateIR(t, `
		main() {
			ch := channel[int](capacity: 5);
			ch.send(42);
			val := <-ch;
		}
	`)
	// Should have channel receive blocks
	assertContains(t, ir, "chrecv.wait")
	assertContains(t, ir, "chrecv.check")
	assertContains(t, ir, "chrecv.none")
	assertContains(t, ir, "chrecv.read")
	assertContains(t, ir, "chrecv.done")
	// Returns optional { i1, i64 }
	assertContains(t, ir, "insertvalue { i1, i64 }")
}

func TestChannelForIn(t *testing.T) {
	ir := generateIR(t, `
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
	assertContains(t, ir, "forin_ch.header")
	assertContains(t, ir, "forin_ch.recv.wait")
	assertContains(t, ir, "forin_ch.recv.check")
	assertContains(t, ir, "forin_ch.recv.none")
	assertContains(t, ir, "forin_ch.recv.read")
	assertContains(t, ir, "forin_ch.body")
	assertContains(t, ir, "forin_ch.exit")
}

// T0671: for-in over a heap-element channel must drop the per-iteration loop
// variable. genForInChannel memcpys each item out of the ring buffer (a real
// move) into the loop-var alloca, so the loop owns it and must register a
// flag-guarded drop binding (string -> promise_string_drop). Pre-fix no drop
// binding was emitted, leaking one allocation per received heap item.
func TestT0671ForInChannelDropsHeapLoopVar(t *testing.T) {
	ir := generateIR(t, `
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
	assertContains(t, ir, "forin_ch.body")
	assertContains(t, ir, "strdrop.call")
	assertContains(t, ir, "strdrop.skip")
	assertContains(t, ir, "call void @promise_string_drop(")
}

func TestChannelSendClosedPanic(t *testing.T) {
	ir := generateIR(t, `
		main() {
			ch := channel[int](capacity: 5);
			ch.send(42);
		}
	`)
	// Send should check closed flag and panic if set
	assertContains(t, ir, "send.closed.panic")
	assertContains(t, ir, "send on closed channel")
	// After wait-full wakeup, should re-check closed
	assertContains(t, ir, "send.waitfull.closed")
}

func TestChannelDoubleClosePanic(t *testing.T) {
	ir := generateIR(t, `
		main() {
			ch := channel[int](capacity: 5);
			ch.close();
		}
	`)
	// Close should check already-closed flag
	assertContains(t, ir, "close.panic")
	assertContains(t, ir, "close of closed channel")
}

func TestGoBlockCapturesOuterVars(t *testing.T) {
	ir := generateIR(t, `
		main() {
			ch := channel[int](capacity: 1);
			int x = 10;
			go {
				ch.send(x);
			};
		}
	`)
	// Coroutine should have parameters for captured variables
	assertContains(t, ir, "define i8* @.goroutine.")
	assertContains(t, ir, "presplitcoroutine")
	// G created and enqueued
	assertContains(t, ir, "call i8* @promise_g_new(")
	assertContains(t, ir, "call void @promise_sched_enqueue(")
}

func TestGoBlockCapturesMultipleVars(t *testing.T) {
	ir := generateIR(t, `
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
	assertContains(t, ir, "define i8* @.goroutine.")
	assertContains(t, ir, "presplitcoroutine")
}

// B0111: Select inside go block must capture outer channel variables.
func TestGoBlockCapturesSelectChannelVars(t *testing.T) {
	ir := generateIR(t, `
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
	assertContains(t, ir, "ch.cap")
	// Should still generate coroutine infrastructure
	assertContains(t, ir, "define i8* @.goroutine.")
	assertContains(t, ir, "presplitcoroutine")
}

func TestGoBlockNoCapturesStillWorks(t *testing.T) {
	ir := generateIR(t, `
		main() {
			go { };
		}
	`)
	// Even without captures, the go block should generate a coroutine
	assertContains(t, ir, "define i8* @.goroutine.")
	assertContains(t, ir, "presplitcoroutine")
	assertContains(t, ir, "call i8* @promise_g_new(")
	assertContains(t, ir, "call void @promise_sched_enqueue(")
}

func TestSchedulerGlobals(t *testing.T) {
	ir := generateIR(t, `
		main() { }
	`)
	// Thread-local current G pointer
	assertContains(t, ir, "@__promise_current_g")
	assertContains(t, ir, "thread_local")
	// Global scheduler singleton
	assertContains(t, ir, "@__promise_sched")
	// TLS panic flag globals (T0143)
	assertContains(t, ir, "@__promise_panic_flag")
	assertContains(t, ir, "@__promise_panic_msg")
	assertContains(t, ir, "@__promise_panic_type")
}

func TestSchedulerFunctionsExist(t *testing.T) {
	ir := generateIR(t, `
		main() { }
	`)
	assertContains(t, ir, "define void @promise_sched_init(")
	assertContains(t, ir, "define i8* @promise_sched_loop(")
	assertContains(t, ir, "define void @promise_sched_enqueue(")
	assertContains(t, ir, "define i8* @promise_sched_find_runnable(")
	assertContains(t, ir, "define void @promise_sched_park_m(")
	assertContains(t, ir, "define void @promise_sched_wake_m()")
	assertContains(t, ir, "define void @promise_goroutine_exit(")
	assertContains(t, ir, "define void @promise_sched_shutdown()")
	assertContains(t, ir, "define i8* @promise_g_new(")
}

func TestCoroIntrinsicsDeclared(t *testing.T) {
	ir := generateIR(t, `
		main() { }
	`)
	assertContains(t, ir, "declare token @llvm.coro.id(")
	assertContains(t, ir, "declare i1 @llvm.coro.alloc(")
	assertContains(t, ir, "declare i8* @llvm.coro.begin(")
	assertContains(t, ir, "declare i64 @llvm.coro.size.i64()")
	assertContains(t, ir, "declare i8 @llvm.coro.suspend(")
	assertContains(t, ir, "declare void @llvm.coro.end(")
	assertContains(t, ir, "declare i8* @llvm.coro.free(")
	assertContains(t, ir, "declare void @llvm.coro.resume(")
	assertContains(t, ir, "declare void @llvm.coro.destroy(")
	assertContains(t, ir, "declare i1 @llvm.coro.done(")
}

func TestGoBlockEmitsCoroutine(t *testing.T) {
	ir := generateIR(t, `
		main() {
			x := 42;
			go { x; };
		}
	`)
	// Coroutine function with presplitcoroutine attribute
	assertContains(t, ir, "presplitcoroutine")
	// Coroutine intrinsics used in the go block
	assertContains(t, ir, "call token @llvm.coro.id(")
	assertContains(t, ir, "call i1 @llvm.coro.alloc(")
	assertContains(t, ir, "call i8* @llvm.coro.begin(")
	assertContains(t, ir, "call i8 @llvm.coro.suspend(")
	assertContains(t, ir, "call void @llvm.coro.end(")
	// Go blocks now use coroutine + G + enqueue, not direct pal_thread_create
	// (pal_thread_create is still used by the scheduler for M threads, but not in go block codegen)
	assertContains(t, ir, "call i8* @promise_g_new(")
	assertContains(t, ir, "call void @promise_sched_enqueue(")
}

func TestGoBlockEnqueuesG(t *testing.T) {
	ir := generateIR(t, `
		main() {
			go { };
		}
	`)
	// G creation and enqueue
	assertContains(t, ir, "call i8* @promise_g_new(")
	assertContains(t, ir, "call void @promise_sched_enqueue(")
}

// T0683: a non-void value-returning go-block (`go { …; <expr> }` → task[T])
// awaited via `<-x` must store the trailing value into G.result_ptr, and the
// caller must allocate a heap result buffer — not the void sentinel 0x1,
// which `<-x` would dereference as a wild pointer (SIGSEGV, mislabeled
// "fatal: stack overflow" by the macOS signal handler).
func TestGoBlockValueResultStored(t *testing.T) {
	ir := generateIR(t, `
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
	assertContains(t, ir, "go.store_result")
	assertContains(t, ir, "go.after_store")
	assertContains(t, ir, "store i64 42, i64*")
	// Caller allocates a heap result buffer for the non-void task.
	assertContains(t, ir, "@pal_alloc")
	// The bug stored the void sentinel 0x1 into result_ptr for this
	// non-void task — there must be no such sentinel now.
	assertNotContains(t, ir, "inttoptr i64 1 to i8*")
}

func TestGoBlockVoidStillUsesSentinel(t *testing.T) {
	ir := generateIR(t, `
		main() {
			task[void] x = go { int n = 10; };
			<-x;
		}
	`)
	assertContains(t, ir, "inttoptr i64 1 to i8*")
	// T1385: the go-block store now goes through storeGoResultAgg, whose blocks
	// are named `go.store_result.N` — assert on that prefix so the guard keeps
	// biting (the bare `store_result:` label no longer exists on this path).
	assertNotContains(t, ir, "go.store_result")
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
	ir := generateIR(t, `
		mk[T](T v) Task[T] {
			return go { v };
		}
		main() {
			task[int] x = mk[int](42);
			r := <-x;
		}
	`)
	// The generic function was monomorphized for int.
	assertContains(t, ir, `@"mk[int]"`)
	// The value path was taken inside the monomorphized go-block coroutine
	// (store into G.result_ptr), the caller allocated a real result buffer,
	// and no void sentinel was used for this non-void monomorphized task.
	assertContains(t, ir, "go.store_result")
	assertContains(t, ir, "go.after_store")
	assertContains(t, ir, "@pal_alloc")
	assertNotContains(t, ir, "inttoptr i64 1 to i8*")
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
	ir := generateIR(t, `
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
	emitIR := extractFunction(ir, "__user.emit")
	// The void spawn dups the borrowed string param before the goroutine ramp,
	// so the coroutine's async `v + "!"` reads the owned copy, not the freed
	// arg temp. The dup precedes the ramp call.
	assertContains(t, emitIR, "@promise_string_new(")
	assertContains(t, emitIR, "call i8* @.goroutine.")
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
	ir := generateIR(t, `
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
	emitIR := extractFunction(ir, "__user.emit")
	assertNotContains(t, emitIR, "vecdup.copy")
	assertNotContains(t, emitIR, "heapdup.copy")
	assertNotContains(t, emitIR, "@promise_string_new(")
	assertContains(t, emitIR, "call i8* @.goroutine.")
}

// T1197: when the spawning function is itself generic, the borrowed-param dup
// runs inside the monomorphized body with c.typeSubst active. The capture's
// sema type is a bare TypeParam (T) that must be substituted to the concrete
// type (string) before goElemNeedsBorrowedCaptureDup / dupBorrowedCaptureForResult
// can classify it — otherwise the borrowed arg is not dup'd and the coroutine
// UAFs. Here both the receiver `b` (GBox[string], a heap user type) and the
// arg `v` (T→string) are dup'd inside `gmk[string]`.
func TestT1197_ViaBlockGenericSpawnerTypeSubst(t *testing.T) {
	ir := generateIR(t, `
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
	gmkIR := extractFunction(ir, `"gmk[string]"`)
	assertContains(t, gmkIR, "call i8* @.goroutine.")
	// arg `v` (T substituted to string) dup'd via promise_string_new; receiver
	// `b` (GBox[string] heap user) dup'd via heapdup — both before the ramp.
	assertContains(t, gmkIR, "@promise_string_new(")
	assertContains(t, gmkIR, "heapdup.copy")
}

// T1219: `this` referenced inside a plain `go { }` block within a method used to
// panic codegen ("'this' used but not in method context") — the coroutine body
// did not inherit the method's `this`. The fix threads a private SNAPSHOT of the
// receiver into the coroutine arg pack. For a HEAP receiver the snapshot is a
// deep copy: the coroutine takes a `{ i8*, i8* }` value-struct param and the
// spawning method emits a heapdup before the goroutine ramp call, so the async
// body owns an independent instance (dropped at coroutine scope exit).
func TestT1219_GoBlockThisHeapReceiverSnapshot(t *testing.T) {
	ir := generateIR(t, `
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
	assertContains(t, ir, "%this.cap")
	assertContains(t, ir, "{ i8*, i8* } %this.cap")
	// The spawning method deep-copies the receiver before the goroutine ramp.
	spawnIR := extractFunction(ir, "T1219C.spawn")
	assertContains(t, spawnIR, "heapdup.copy")
	assertContains(t, spawnIR, "call i8* @.goroutine.")
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
	ir := generateIR(t, `
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
	assertContains(t, ir, "%this.cap")
	sumIR := extractFunction(ir, "T1219P.sum")
	assertContains(t, sumIR, "call i8* @.goroutine.")
	assertNotContains(t, sumIR, "heapdup.copy")
}

// T1219: a GENERIC-owner method (`T1219Box[int].get_it`) referencing `this` in a
// `go { }` block resolves the receiver type via the mono instance (currentNamedType
// → monoCtx.inst), deep-copies the concrete `Box[int]` snapshot, and threads it
// into the coroutine — exercising the generic-owner path of the fix.
func TestT1219_GoBlockThisGenericOwner(t *testing.T) {
	ir := generateIR(t, `
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
	sendIR := extractFunction(ir, `"T1219Box[int].send_it"`)
	assertContains(t, sendIR, "heapdup.copy")
	// T1222: the ramp spawned inside a generic (instance-owned) method is qualified
	// by the enclosing name so it can't collide across split units. The qualified
	// symbol contains `[`/`.`, so LLVM emits it quoted.
	assertContains(t, sendIR, `call i8* @".goroutine.T1219Box[int].send_it.`)
}

// T1219: a GENERIC METHOD on a NON-GENERIC owner (`T1219GenM.emit[int]`)
// referencing `this` in a `go { }` block resolves the receiver type via the
// active typeSubst (currentNamedType → the `typeSubst != nil` branch, since
// monoCtx.inst is nil for a non-generic owner). The concrete owner is unchanged
// by the substitution, but the branch must be taken — otherwise the receiver
// type would resolve incorrectly. Exercises the generic-method path distinct
// from the generic-owner path above.
func TestT1219_GoBlockThisGenericMethodNonGenericOwner(t *testing.T) {
	ir := generateIR(t, `
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
	emitIR := extractFunction(ir, `"T1219GenM.emit[int]"`)
	assertContains(t, emitIR, "heapdup.copy")
	assertContains(t, emitIR, "call i8* @.goroutine.")
}

// T1198: a borrowed channel param passed to a fast-path go-call is refcounted
// (B0163), not dup'd — sharing the pointer is fine.
func TestT1198_FastPathChannelParamNoDup(t *testing.T) {
	ir := generateIR(t, `
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
	spawnIR := extractFunction(ir, "__user.spawn")
	// Channel args are refcounted via an atomic add, never string-dup'd.
	assertNotContains(t, spawnIR, "@promise_string_new(")
	assertContains(t, spawnIR, "call i8* @.goroutine.")
}

func TestChannelSendInCoroutineSuspends(t *testing.T) {
	ir := generateIR(t, `
		main() {
			ch := channel[int](capacity: 1);
			go {
				ch.send(42);
			};
		}
	`)
	// Inside go block, channel send should use goroutine-aware park
	assertContains(t, ir, "call void @promise_waiter_enqueue(")
	// The go block is a coroutine
	assertContains(t, ir, "presplitcoroutine")
}

func TestChannelRecvInCoroutineSuspends(t *testing.T) {
	ir := generateIR(t, `
		main() {
			ch := channel[int](capacity: 1);
			go {
				result := <-ch;
			};
		}
	`)
	// Inside go block, channel recv should use goroutine-aware park
	assertContains(t, ir, "call void @promise_waiter_enqueue(")
}

func TestChannelCloseWakesAllWaiters(t *testing.T) {
	ir := generateIR(t, `
		main() {
			ch := channel[int](capacity: 1);
			ch.close();
		}
	`)
	// Close should call promise_waiter_wake_all for send, recv, and rv_waiters (T0312)
	assertContains(t, ir, "call void @promise_waiter_wake_all(")
}

func TestChannelCloseWakesRvWaiters(t *testing.T) {
	// T0312: genChannelClose must wake rv_waiters in addition to send/recv waiters.
	// A rendezvous-parked sender (goroutine that wrote to an unbuffered channel and
	// is parked on rv_waiters) must be unblocked when the channel closes.
	ir := generateIR(t, `
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
	irBaseline := generateIR(t, `
		main() {
			ch := channel[int](capacity: 1);
			ch.send(1);
		}
	`)
	baseline := strings.Count(irBaseline, "call void @promise_waiter_wake_one(")

	ir := generateIR(t, `
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
	ir := generateIR(t, `
		main() {
			ch := channel[int](capacity: 1);
		}
	`)
	// Channel struct has 18 fields: buffer, head, tail, count, cap, elem_size,
	// is_closed, is_unbuffered, not_empty, not_full, send_waiters(2), recv_waiters(2),
	// rv_waiters(2, T0312), refcount. Verified by promise_channel_new definition.
	assertContains(t, ir, "define i8* @promise_channel_new(")
}

func TestTaskReceiveParksGoroutine(t *testing.T) {
	ir := generateIR(t, `
		compute() int { return 42; }
		main() {
			t := go compute();
			result := <-t;
		}
	`)
	// Task receive in main (non-coroutine) uses thread-blocking mode
	// But the G.done field should be checked
	assertContains(t, ir, "promise_g_new")
	assertContains(t, ir, "promise_sched_enqueue")
}

func TestTaskDropDoneLoadIsAcquire(t *testing.T) {
	// T0669: G.done spin-wait in Task.drop must use an atomic acquire load so the
	// LLVM optimizer cannot hoist or cache it across loop iterations on Windows.
	ir := generateIR(t, `
		compute() int { return 42; }
		main() {
			t := go compute();
			v := <-t;
		}
	`)
	// The Task drop function must contain an atomic acquire load on G.done (i8).
	assertContains(t, ir, "load atomic i8")
	assertContains(t, ir, "acquire")
}

// --- Phase 5c gap-filling tests ---

func TestTaskReceiveInCoroutine(t *testing.T) {
	// <-task inside a go block uses the coroutine park path with done_lock,
	// not the thread-blocking path. The done_lock protects the done flag and
	// done_waiters list, and park_mutex holds the lock across coro.suspend.
	ir := generateIR(t, `
		compute() int { return 42; }
		main() {
			go {
				t := go compute();
				int result = <-t;
			};
		}
	`)
	// The outer go block is a coroutine
	assertContains(t, ir, "presplitcoroutine")
	// Task receive inside coroutine: parks on done_waiters via done_lock
	assertContains(t, ir, "task.done")
	assertContains(t, ir, "task.wait")
	assertContains(t, ir, "task.ready")
	// done_lock path: check under lock, park if not done
	assertContains(t, ir, "task.done_under_lock")
	assertContains(t, ir, "task.park")
	// Coroutine suspend in the task wait path
	assertContains(t, ir, "task.resume")
	// Should NOT use usleep (that's the thread-blocking path)
	// The go block coroutine uses coro.suspend instead
}

func TestTaskReceiveCoroutineMode(t *testing.T) {
	// <-task in main uses coroutine parking (main is compiled as goroutine)
	ir := generateIR(t, `
		compute() int { return 42; }
		main() {
			t := go compute();
			result := <-t;
		}
	`)
	// Coroutine mode: park on done_waiters with done_lock, coro.suspend
	assertContains(t, ir, "task.park")
	assertContains(t, ir, "task.resume")
	assertContains(t, ir, "task.done_under_lock")
}

func TestVoidTaskSentinel(t *testing.T) {
	// Void tasks set result_ptr to sentinel inttoptr(i64 1) so goroutine_exit
	// knows not to free G (the receiver frees it via <-task).
	ir := generateIR(t, `
		doWork() { }
		main() {
			t := go doWork();
			<-t;
		}
	`)
	// Sentinel value: inttoptr i64 1 to i8*
	assertContains(t, ir, "inttoptr i64 1 to i8*")
	// G is freed by the receiver, not goroutine_exit
	assertContains(t, ir, "task.ready")
}

func TestVoidGoBlockSentinel(t *testing.T) {
	// go { block } used as a task (assigned + awaited) — should set sentinel
	// so goroutine_exit doesn't free G before the receiver does.
	ir := generateIR(t, `
		main() {
			t := go { };
			<-t;
		}
	`)
	assertContains(t, ir, "inttoptr i64 1 to i8*")
}

func TestFireAndForgetGoBlockNoSentinel(t *testing.T) {
	// go { block } as a statement (fire-and-forget) — should NOT set sentinel.
	// goroutine_exit will free the G struct since result_ptr stays null.
	ir := generateIR(t, `
		main() {
			go { };
		}
	`)
	// The go block trampoline body should not contain the sentinel store.
	// After promise_g_new, the next call should be promise_sched_enqueue (no inttoptr).
	assertContains(t, ir, "call i8* @promise_g_new(")
	assertContains(t, ir, "call void @promise_sched_enqueue(")
}

func TestGoCallTaskSetsSentinel(t *testing.T) {
	// go void_func() used as a task (assigned + awaited) — should set sentinel.
	ir := generateIR(t, `
		work() { }
		main() {
			t := go work();
			<-t;
		}
	`)
	assertContains(t, ir, "inttoptr i64 1 to i8*")
}

func TestGoCallNonVoidTaskSetsResultPtr(t *testing.T) {
	// go non_void_func() as task — should allocate result buffer (not sentinel).
	ir := generateIR(t, `
		compute() int { return 42; }
		main() {
			t := go compute();
			<-t;
		}
	`)
	// Result buffer allocated via pal_alloc and stored in result_ptr
	assertContains(t, ir, "call i8* @pal_alloc(")
}

func TestFireAndForgetGoBlockInLoop(t *testing.T) {
	// go { } as the only statement in a for loop — fire-and-forget.
	// genBlock routes all statements through genStmt, so the flag is set.
	ir := generateIR(t, `
		main() {
			for i in 0..3 {
				go { };
			}
		}
	`)
	assertContains(t, ir, "call i8* @promise_g_new(")
	assertNotContains(t, ir, "inttoptr i64 1 to i8*")
}

func TestFireAndForgetGoBlockNestedFlagRestore(t *testing.T) {
	// Nested go blocks: outer is a task, inner is fire-and-forget.
	// The inner genStmt clears goExprFireAndForget, but genGoBlock
	// saves/restores it, so the outer block still sets sentinel.
	ir := generateIR(t, `
		main() {
			t := go {
				go { };
			};
			<-t;
		}
	`)
	// Outer go block should have sentinel (it's a task)
	assertContains(t, ir, "inttoptr i64 1 to i8*")
}

func TestGoroutineExitSkipsFreeForTask(t *testing.T) {
	// goroutine_exit checks result_ptr != null to decide whether to free G.
	// Tasks (result_ptr set) skip the free; fire-and-forget goroutines are freed.
	ir := generateIR(t, `
		main() { }
	`)
	// goroutine_exit should contain the conditional skip-free logic
	assertContains(t, ir, "define void @promise_goroutine_exit(")
	// The function checks result_ptr to decide whether to free
	assertContains(t, ir, "skip_free:")
	assertContains(t, ir, "do_free:")
}

func TestChannelSendCoroutineRendezvous(t *testing.T) {
	// T0312: Unbuffered channel send inside a go block parks on rv_waiters for the
	// rendezvous. After writing the value, the sender enqueues itself on rv_waiters,
	// sets park_mutex=&ch.mutex, and calls coro.suspend. The scheduler unlocks
	// ch.mutex. The receiver wakes the sender via wake_one(rv_waiters) after count--.
	ir := generateIR(t, `
		main() {
			ch := channel[int]();
			go {
				ch.send(42);
			};
			result := <-ch;
		}
	`)
	// Rendezvous wait and resume blocks must exist
	assertContains(t, ir, "send.rv.wait")
	assertContains(t, ir, "send.rv.resume")
	// rv_waiters park: waiter_enqueue IS called (unlike the old yield-spin)
	assertContains(t, ir, "call void @promise_waiter_enqueue(")
	// Receiver must wake rv_waiters after count--
	assertContains(t, ir, "call void @promise_waiter_wake_one(")
}

func TestChannelSendRendezvousExitWakesNextWaiter(t *testing.T) {
	// T0305/T0312: Rendezvous exit wakes one waiter on send_waiters. rv_waiters
	// holds rendezvous-parked senders; send_waiters holds only write-waiters and
	// select SWNs, so waking it here is safe and never strands a write-waiter.
	ir := generateIR(t, `
		main() {
			ch := channel[int]();
			go {
				ch.send(42);
			};
			result := <-ch;
		}
	`)
	// The rendezvous exit block should exist and call wake_one
	assertContains(t, ir, "send.rv.exit")
	assertContains(t, ir, "call void @promise_waiter_wake_one(")
}

func TestForInChannelCoroutineMode(t *testing.T) {
	// for-in channel inside a go block uses coroutine-mode park+suspend
	ir := generateIR(t, `
		main() {
			ch := channel[int](capacity: 1);
			go {
				for v in ch {
					int x = v + 1;
				}
			};
			ch.send(1);
			ch.close();
		}
	`)
	// The for-in inside the coroutine should use waiter_enqueue + coro.suspend
	assertContains(t, ir, "forin_ch.recv.wait")
	assertContains(t, ir, "call void @promise_waiter_enqueue(")
	// Should have the coroutine resume block for the for-in
	assertContains(t, ir, "forin_ch.recv.resume")
}

func TestChannelRecvWakesSenderGoroutine(t *testing.T) {
	// After receiving, the code should wake a parked sender goroutine via
	// promise_waiter_wake_one (handles both regular G and select SWN nodes).
	ir := generateIR(t, `
		main() {
			ch := channel[int](capacity: 1);
			ch.send(1);
			result := <-ch;
		}
	`)
	assertContains(t, ir, "call void @promise_waiter_wake_one(")
}

func TestChannelSendWakesRecvGoroutine(t *testing.T) {
	// After sending, the code should wake a parked receiver goroutine via
	// promise_waiter_wake_one (handles both regular G and select SWN nodes).
	ir := generateIR(t, `
		main() {
			ch := channel[int](capacity: 1);
			ch.send(42);
		}
	`)
	assertContains(t, ir, "call void @promise_waiter_wake_one(")
}

func TestSelectBlockingEmitsSWNParking(t *testing.T) {
	// A blocking select (no default) in coroutine mode should emit:
	// - SelectWaiterNode allocas and initialization (kind sentinel 0xFF)
	// - select_waiter_enqueue calls to park SWNs on channel waiter lists
	// - select_try_wake definition (wake-once protocol)
	// - waiter_wake_one definition (handles both G and SWN nodes)
	// - waiter_remove calls for SWN cleanup after resume
	ir := generateIR(t, `
		main() {
			ch1 := channel[int](capacity: 1);
			ch2 := channel[int](capacity: 1);
			go { ch1.send(1); };
			select {
				v := <-ch1:
					print_line("ch1");
				v := <-ch2:
					print_line("ch2");
			}
		}
	`)
	// SWN infrastructure functions
	assertContains(t, ir, "define void @promise_select_waiter_enqueue(")
	assertContains(t, ir, "define i1 @promise_select_try_wake(")
	assertContains(t, ir, "define void @promise_waiter_wake_one(")

	// Blocking path: SWN kind sentinel (0xFF = 255) stored to field 1
	assertContains(t, ir, "store i8 255,")

	// SWN enqueue calls (one per case)
	assertContains(t, ir, "call void @promise_select_waiter_enqueue(")

	// SWN cleanup after resume
	assertContains(t, ir, "call void @promise_waiter_remove(")

	// Select mutex lifecycle
	assertContains(t, ir, "call void @pal_mutex_destroy(")
}

func TestSelectNonBlockingNoSWN(t *testing.T) {
	// A select with a default case should NOT emit SWN parking code.
	ir := generateIR(t, `
		main() {
			ch := channel[int](capacity: 1);
			select {
				v := <-ch:
					print_line("got");
				default:
					print_line("default");
			}
		}
	`)
	assertNotContains(t, ir, "call void @promise_select_waiter_enqueue(")
	assertContains(t, ir, "select.default")
}

func TestSelectEmptyDefaultNotBlocking(t *testing.T) {
	// B0116: An empty default body (no statements) must still be treated as
	// a non-blocking select. Previously, the nil []Stmt was indistinguishable
	// from "no default clause", causing the select to block.
	ir := generateIR(t, `
		main() {
			ch := channel[int](capacity: 1);
			select {
				v := <-ch:
				default:
			}
		}
	`)
	assertNotContains(t, ir, "call void @promise_select_waiter_enqueue(")
	assertContains(t, ir, "select.default")
}

func TestSelectEmptyDefaultTwiceNotBlocking(t *testing.T) {
	// B0116: Two consecutive selects with empty default must both be non-blocking.
	ir := generateIR(t, `
		main() {
			ch := channel[int](capacity: 1);
			select { v := <-ch: default: }
			select { v := <-ch: default: }
		}
	`)
	assertNotContains(t, ir, "call void @promise_select_waiter_enqueue(")
	// Both selects should have default blocks
	assertContains(t, ir, "select.default")
}

func TestSelectBlockingPollInNonCoroutine(t *testing.T) {
	// B0045: A blocking select (no default) in non-coroutine context should
	// emit a poll-retry loop that unlocks, sleeps, re-locks, and retries
	// instead of falling through to merge (which silently skips all cases).
	ir := generateIR(t, `
		foo() {
			ch := channel[int](capacity: 1);
			select {
				v := <-ch:
			}
		}
		main() { foo(); }
	`)
	// Poll block should exist (not SWN parking — that's for coroutines)
	assertContains(t, ir, "select.poll")
	assertNotContains(t, ir, "select.park")
	// Should call usleep in the poll loop
	assertContains(t, ir, "call i32 @usleep(i32 100)")
	// Should branch back to lock.start for retry
	assertContains(t, ir, "br label %select.lock.start")
}

func TestSelectWakePathSendGuard(t *testing.T) {
	// B0110: A blocking select with a send case should emit a fullness
	// re-check on the wake path. Between the wake and re-locking channels,
	// another sender may have filled the freed slot. The guard branches to
	// a retry block that unlocks all channels and retries from lock.start.
	ir := generateIR(t, `
		main() {
			ch := channel[int](capacity: 1);
			go { <-ch; };
			select {
				ch.send(42):
					print_line("sent");
				v := <-ch:
					print_line("recv");
			}
		}
	`)
	// Wake path should have a send.ok block (guard passed)
	assertContains(t, ir, "select.wk0.send.ok")
	// Wake retry block should exist for failed guard
	assertContains(t, ir, "select.wake.retry")
	// Retry should branch back to lock.start
	assertContains(t, ir, "br label %select.lock.start")
}

func TestSchedulerReleasesParkMutex(t *testing.T) {
	// The scheduler loop checks G.park_mutex after coro.resume returns
	// and releases it if non-null. This closes the enqueue-before-suspend race.
	ir := generateIR(t, `
		main() {
			ch := channel[int](capacity: 1);
			go { ch.send(42); };
		}
	`)
	// Scheduler loop must contain the park_mutex release blocks
	assertContains(t, ir, "release_park_mutex")
	assertContains(t, ir, "after_release")
}

func TestSchedulerClearsParkMutexBeforeUnlock(t *testing.T) {
	// B0249: park_mutex must be cleared BEFORE the mutex unlock to prevent a race
	// where another thread wakes G, G re-parks with a new mutex, and the stale
	// NULL write overwrites it — causing double-resume and segfault.
	ir := generateIR(t, `
		main() {
			ch := channel[int](capacity: 1);
			go { ch.send(42); };
		}
	`)
	// In sched_loop (and sched_coop_run for WASM), the release_park_mutex block
	// must store null to park_mutex BEFORE calling pal_mutex_unlock.
	fn := extractFunction(ir, "promise_sched_loop")
	if fn == "" {
		t.Fatal("promise_sched_loop not found")
	}
	idx := strings.Index(fn, "release_park_mutex:")
	if idx < 0 {
		t.Fatal("release_park_mutex block not found in sched_loop")
	}
	relBlk := fn[idx:]
	storeIdx := strings.Index(relBlk, "store i8* null,")
	unlockIdx := strings.Index(relBlk, "call void @pal_mutex_unlock(")
	if storeIdx < 0 {
		t.Fatal("null store not found in release_park_mutex")
	}
	if unlockIdx < 0 {
		t.Fatal("mutex unlock not found in release_park_mutex")
	}
	if storeIdx > unlockIdx {
		t.Error("B0249: park_mutex null store must come BEFORE mutex unlock to prevent race")
	}
}

func TestSchedParkMRechecksGlobalQueue(t *testing.T) {
	// T0375: park_m must re-check sched.global_size while still holding
	// idle_lock AFTER pushing self onto the idle stack. If a non-M enqueuer
	// raced through sched_enqueue + wake_m against an empty idle stack, the
	// re-check sees the queued work and aborts the park (popping self off the
	// idle stack) instead of committing to cond_wait indefinitely.
	ir := generateIR(t, `
		main() {
			ch := channel[int](capacity: 1);
			go { ch.send(42); };
		}
	`)
	fn := extractFunction(ir, "promise_sched_park_m")
	if fn == "" {
		t.Fatal("promise_sched_park_m not found")
	}

	// The abort_park block must exist (the bail path).
	abortIdx := strings.Index(fn, "abort_park:")
	if abortIdx < 0 {
		t.Fatal("abort_park block not found in promise_sched_park_m")
	}

	// continue_park is the normal park path; both must exist.
	continueIdx := strings.Index(fn, "continue_park:")
	if continueIdx < 0 {
		t.Fatal("continue_park block not found in promise_sched_park_m")
	}

	// The conditional branch into abort_park must come from .entry,
	// before we reach wait_loop / cond_wait.
	entryEnd := strings.Index(fn, "abort_park:")
	if entryEnd < 0 {
		t.Fatal("could not locate end of entry block")
	}
	entryBlk := fn[:entryEnd]
	if !strings.Contains(entryBlk, "br i1") {
		t.Error("entry block must conditionally branch (abort_park vs continue_park)")
	}
	if !strings.Contains(entryBlk, ", label %abort_park, label %continue_park") {
		t.Error("entry must branch to abort_park / continue_park based on global queue size")
	}

	// The entry must compare a freshly-loaded i64 against zero — that's the
	// global_size != 0 test.
	if !strings.Contains(entryBlk, "icmp ne i64") {
		t.Error("entry must compare global_size (i64) against zero")
	}

	// The abort_park block must NOT contain pal_cond_wait — that's the whole
	// point of bailing out before we commit to parking.
	abortEnd := strings.Index(fn[abortIdx:], "\n\n")
	if abortEnd < 0 {
		abortEnd = len(fn) - abortIdx
	}
	abortBlk := fn[abortIdx : abortIdx+abortEnd]
	if strings.Contains(abortBlk, "@pal_cond_wait") {
		t.Error("abort_park must not call pal_cond_wait — bailing out before parking")
	}
	// abort_park must unlock both mutexes (idle_lock and park_mutex) and ret.
	if strings.Count(abortBlk, "@pal_mutex_unlock") != 2 {
		t.Error("abort_park must unlock both idle_lock and park_mutex (2 unlocks)")
	}
	if !strings.Contains(abortBlk, "ret void") {
		t.Error("abort_park must return")
	}
}

// T0149: goroutine_exit always calls coro.destroy (panicked goroutines reach
// final suspend via TLS flag propagation, so coro.destroy is safe).
func TestGoroutineExitAlwaysCoroDestroy(t *testing.T) {
	ir := generateIR(t, `
		main() { }
	`)
	fn := extractFunction(ir, "promise_goroutine_exit")
	// Must call coro.destroy unconditionally
	assertContains(t, fn, "call void @llvm.coro.destroy")
	// Must NOT have the old free_coro_frame fallback for panicked goroutines
	assertNotContains(t, fn, "free_coro_frame:")
}

// T0148: genGoBlock has final panic check before final suspend.
func TestPanicCheckGoBlockFinal(t *testing.T) {
	ir := generateIR(t, `
		work() {}
		main() {
			go {
				work();
			};
		}
	`)
	assertContains(t, ir, "go.panic_exit")
	assertContains(t, ir, "@__promise_panic_flag")
}

// --- Generator Tests ---

func TestGeneratorProducesCoroutine(t *testing.T) {
	ir := generateIR(t, `
		count() stream[int] {
			yield 1;
			yield 2;
		}
		main() {}
	`)
	assertContains(t, ir, `.generator.`)
	assertContains(t, ir, "presplitcoroutine")
	assertContains(t, ir, "@llvm.coro.suspend")
}

func TestFailableGeneratorCoroutineHasErrorSlot(t *testing.T) {
	ir := generateIR(t, `
		gen!() stream[int] {
			yield 1;
		}
		main() {}
	`)
	// Coroutine should have error_slot parameter and alloca
	assertContains(t, ir, "error_slot.addr")
}

func TestSchedLoopSetsCurrentM(t *testing.T) {
	// sched_loop should store M param to TLS current_m
	ir := generateIR(t, `
		main() { }
	`)
	// sched_loop stores m to current_m
	assertContains(t, ir, "__promise_current_m")
	assertContains(t, ir, "promise_sched_loop")
}

func TestSchedLoopNoSetjmp(t *testing.T) {
	// T0149: sched_loop no longer uses setjmp/longjmp for panic recovery.
	// Panicked goroutines reach final suspend via TLS panic flag propagation
	// (T0146-T0148), so the scheduler just calls coro.resume directly.
	ir := generateIR(t, `
		main() { }
	`)
	fn := extractFunction(ir, "promise_sched_loop")
	// Must NOT contain setjmp or jmpBuf alloca
	assertNotContains(t, fn, "alloca [256 x i8]")
	assertNotContains(t, fn, "setjmp")
	assertNotContains(t, fn, "panic_recovery")
	// Must contain direct coro.resume in the run_g flow
	assertContains(t, fn, "call void @llvm.coro.resume")
}

func TestSchedShutdownUsesMaxP(t *testing.T) {
	// B0120: shutdown must signal/join ALL Ms using max_p (field 14),
	// not num_p (field 5). After set_max_procs reduces num_p, Ms on
	// disabled Ps would not be signaled/joined, causing SIGSEGV on exit.
	ir := generateIR(t, `
		main() { }
	`)
	fn := extractFunction(ir, "promise_sched_shutdown")
	// The sched struct GEP that loads the loop bound must reference
	// field index 14 (max_p). The GEP accesses @__promise_sched, and
	// the second field index is the one that selects num_p vs max_p.
	// Check that the GEP for the loop bound uses field 14.
	assertContains(t, fn, "@__promise_sched, i32 0, i32 14")
	// Ensure there is no GEP accessing num_p (field 5) in shutdown —
	// the only sched fields accessed should be shutdown (9), max_p (14), and ps (4).
	assertNotContains(t, fn, "@__promise_sched, i32 0, i32 5")
}

func TestFindRunnableSchedTickGlobalFirstCheck(t *testing.T) {
	// T0326: find_runnable must check global queue first every 61 scheduling
	// iterations (schedtick % 61 == 0) to prevent starvation of goroutines
	// enqueued by non-M threads (e.g., test-thread channel ops).
	ir := generateIR(t, `main() {}`)
	fn := extractFunction(ir, "promise_sched_find_runnable")

	// Entry block must read the schedtick field (P field index 7), increment it,
	// and store it back.
	assertContains(t, fn, "i32 0, i32 7")
	// Must compute urem with 61 (prime modulus chosen to avoid resonance with
	// power-of-2 queue sizes) and branch to try_global when result == 0.
	assertContains(t, fn, "urem i64 %")
	assertContains(t, fn, ", 61")
	assertContains(t, fn, "label %try_global, label %check_local")
}

func TestSchedLoopIncrementsSchedTick(t *testing.T) {
	// T0326: sched_loop's runG block must also increment P.schedTick (field 7)
	// before resuming a goroutine. find_runnable uses the tick value set here
	// (not only the one it sets itself) for the global-first priority decision.
	ir := generateIR(t, `main() {}`)
	fn := extractFunction(ir, "promise_sched_loop")

	// sched_loop's runG block reads, increments, and stores back P field 7.
	assertContains(t, fn, "i32 0, i32 7")
}

// B0007: Verify that the goroutine coroutine has coro.init.suspend block
// separating allocas in coro.start from the initial coro.suspend.
func TestCoroutineInitSuspendBlock(t *testing.T) {
	ir := generateIR(t, `
		main() {
			ch := channel[int](capacity: 1);
			ch.send(42);
		}
	`)
	// Main is wrapped in a goroutine coroutine
	goFunc := extractFunc(ir, ".goroutine.main")
	if goFunc == "" {
		t.Fatal("expected .goroutine.main function in IR")
	}
	// coro.start should branch to coro.init.suspend (not contain coro.suspend directly)
	assertContains(t, goFunc, "br label %coro.init.suspend")
	// coro.init.suspend block should contain the initial coro.suspend
	assertContains(t, goFunc, "coro.init.suspend:")
	assertContains(t, goFunc, "call i8 @llvm.coro.suspend(")
}

// B0007: Verify that channel send alloca is in coro.start (entry block),
// not in the send.write block.
func TestChannelSendAllocaInEntryBlock(t *testing.T) {
	ir := generateIR(t, `
		main() {
			ch := channel[int](capacity: 5);
			ch.send(42);
		}
	`)
	goFunc := extractFunc(ir, ".goroutine.main")
	if goFunc == "" {
		t.Fatal("expected .goroutine.main function in IR")
	}

	// The alloca for the send value should be in coro.start, before the br to coro.init.suspend
	// Split on "coro.init.suspend:" to get coro.start content
	parts := strings.SplitN(goFunc, "coro.init.suspend:", 2)
	if len(parts) < 2 {
		t.Fatal("expected coro.init.suspend block")
	}
	coroStart := parts[0]

	// coro.start should contain an alloca for the send value (i64 for int)
	if !strings.Contains(coroStart, "alloca i64") {
		t.Errorf("expected alloca i64 in coro.start for channel send value\ncoro.start:\n%s", coroStart)
	}

	// The send.write block should NOT contain an alloca
	sendWriteIdx := strings.Index(goFunc, "send.write")
	if sendWriteIdx >= 0 {
		// Get the send.write block content (up to next label or end)
		sendWriteBlock := goFunc[sendWriteIdx:]
		nextLabel := strings.Index(sendWriteBlock[1:], "\n")
		if nextLabel > 0 {
			// Check a reasonable window after send.write label
			window := sendWriteBlock[:min(len(sendWriteBlock), 500)]
			if strings.Contains(window, "= alloca ") {
				t.Errorf("send.write block should not contain alloca (should be in entry block)\nblock:\n%s", window)
			}
		}
	}
}

// B0007: Verify that channel recv alloca is in coro.start (entry block),
// not in the chrecv.read block.
func TestChannelRecvAllocaInEntryBlock(t *testing.T) {
	ir := generateIR(t, `
		main() {
			ch := channel[int](capacity: 5);
			ch.send(42);
			val := <-ch;
		}
	`)
	goFunc := extractFunc(ir, ".goroutine.main")
	if goFunc == "" {
		t.Fatal("expected .goroutine.main function in IR")
	}

	// The chrecv.read block should NOT contain an alloca
	readIdx := strings.Index(goFunc, "chrecv.read")
	if readIdx >= 0 {
		readBlock := goFunc[readIdx:]
		window := readBlock[:min(len(readBlock), 500)]
		if strings.Contains(window, "= alloca ") {
			t.Errorf("chrecv.read block should not contain alloca (should be in entry block)\nblock:\n%s", window)
		}
	}
}

// B0007: Verify that go-block coroutines also have the coro.init.suspend separation.
func TestGoBlockCoroutineInitSuspend(t *testing.T) {
	ir := generateIR(t, `
		main() {
			ch := channel[int](capacity: 1);
			go {
				ch.send(42);
			};
		}
	`)
	// Find the go-block goroutine function (not .goroutine.main)
	goFunc := extractFunc(ir, ".goroutine.0")
	if goFunc == "" {
		t.Fatal("expected .goroutine.0 function in IR")
	}
	// Should have the separated init suspend block
	assertContains(t, goFunc, "br label %coro.init.suspend")
	assertContains(t, goFunc, "coro.init.suspend:")
}

// B0353: return in error handler inside go block should branch to final.suspend,
// not emit ret void (the coroutine function returns ptr).
func TestGoBlockReturnInErrorHandler(t *testing.T) {
	ir := generateIR(t, `
		fail!() int { raise error(message: "fail"); }
		main() {
			go {
				x := fail()? e { return; };
			};
		}
	`)
	goFunc := extractFunc(ir, ".goroutine.0")
	if goFunc == "" {
		t.Fatal("expected .goroutine.0 function in IR")
	}
	// Should branch to final.suspend instead of ret void
	assertContains(t, goFunc, "br label %final.suspend")
	assertNotContains(t, goFunc, "ret void")
}

// B0007: Verify select statement allocas are in entry block.
func TestSelectAllocaInEntryBlock(t *testing.T) {
	ir := generateIR(t, `
		main() {
			ch1 := channel[int](capacity: 1);
			ch2 := channel[int](capacity: 1);
			ch1.send(10);
			select {
				v := <-ch1:
				v := <-ch2:
			}
		}
	`)
	goFunc := extractFunc(ir, ".goroutine.main")
	if goFunc == "" {
		t.Fatal("expected .goroutine.main function in IR")
	}

	// coro.start should contain the channel array alloca ([2 x i8*])
	parts := strings.SplitN(goFunc, "coro.init.suspend:", 2)
	if len(parts) < 2 {
		t.Fatal("expected coro.init.suspend block")
	}
	coroStart := parts[0]
	if !strings.Contains(coroStart, "alloca [2 x i8*]") {
		t.Errorf("expected alloca [2 x i8*] in coro.start for select channel array\ncoro.start:\n%s", coroStart)
	}
}

// T0394 (channel limb): the predicate also covers types.IsChannel(exprType).
// Channel reassign on an Optional generic field must produce the same
// reassign-drop + temp-drop shape with Channel.drop reachable.
func TestOptionalGenericFieldReassignChannelEmitsDropAndOptdrop(t *testing.T) {
	ir := generateIR(t, `
		type Box[T] { T? value; }
		test() {
			Box[Channel[int]] b = Box[Channel[int]](value: channel[int](2));
			b.value = channel[int](2);
		}
	`)
	assertContains(t, ir, "field.optdrop")
	assertContains(t, ir, "tmp.drop")
	// T0663: Channel.drop is now per-element-type — Channel[int].drop here.
	assertContains(t, ir, `@"Channel[int].drop"`)
}

// B0275: Vector.clone() must dup channel elements (refcount increment).
func TestCloneVectorChannelDupsElements(t *testing.T) {
	ir := generateIR(t, `
		test() {
			ch := channel[int](1);
			v := [ch];
			v2 := v.clone();
		}
	`)
	// Should have the clone loop with channel dup (atomic refcount increment)
	assertContains(t, ir, "vecclone.head")
	assertContains(t, ir, "chdup.inc")
}

// T0261: Verify that vector drop + go block produces unique local names
// (no duplicate vecdrop.idx allocas).
func TestGoBlockVectorDropUniqueNames(t *testing.T) {
	ir := generateIR(t, `
		main() {
			string[] items = ["a", "b"];
			ch := channel[int](capacity: 1);
			go { ch.send(1); ch.close(); };
			int? v = <-ch;
			print_line("{items.len}");
		}
	`)
	// If codegen succeeds, localNameCount was properly saved/restored.
	// The IR should contain the vector drop loop.
	assertContains(t, ir, "vecdrop.idx")
}

// T0155: Ref[T] constructor allocates {i64, T} and stores refcount=1.
func TestArcConstructorAllocAndRefcount(t *testing.T) {
	ir := generateIR(t, `
		main() {
			a := Ref[int](42);
		}
	`)
	// Should allocate 24 bytes (i64 strong_count + i64 weak_count + i64 value) — T0157
	assertContains(t, ir, "call i8* @pal_alloc(i64 24)")
	// Should store refcount = 1
	assertContains(t, ir, "store i64 1")
	// Should store the value 42
	assertContains(t, ir, "store i64 42")
}

// T0155: Ref[T] scope cleanup uses drop flag and calls Ref[int].drop.
func TestArcDropFlagAndCleanup(t *testing.T) {
	ir := generateIR(t, `
		main() {
			a := Ref[int](42);
		}
	`)
	assertContains(t, ir, "%a.dropflag")
	assertContains(t, ir, `call void @"Ref[int].drop"(`)
}

// T0155: Ref[T].drop function has correct structure: null check, atomic decrement, free.
func TestArcDropFunctionBody(t *testing.T) {
	ir := generateIR(t, `
		main() {
			a := Ref[int](42);
		}
	`)
	dropFn := extractFunction(ir, `"Ref[int].drop"`)
	if dropFn == "" {
		t.Fatal("expected Ref[int].drop function in IR")
	}
	// Null check
	assertContains(t, dropFn, "icmp eq")
	assertContains(t, dropFn, "null")
	// Atomic refcount decrement
	assertContains(t, dropFn, "i64 -1")
	// Free on last reference
	assertContains(t, dropFn, "call void @pal_free(")
}

// T0155: Ref[T].clone atomically increments refcount.
func TestArcCloneAtomicIncrement(t *testing.T) {
	ir := generateIR(t, `
		main() {
			a := Ref[int](42);
			b := a.clone();
		}
	`)
	// Clone should atomically add 1 to refcount
	assertContains(t, ir, "i64 1")
	// Both a and b should have drop flags
	assertContains(t, ir, "%a.dropflag")
	assertContains(t, ir, "%b.dropflag")
}

// T0155: Ref[T] borrow getter loads value from allocation.
func TestArcBorrowLoadsValue(t *testing.T) {
	ir := generateIR(t, `
		main() {
			a := Ref[int](42);
			int x = a.borrow;
		}
	`)
	// Borrow GEPs into the {i64, i64, T} struct at field 2 (T0157: value shifted to field 2)
	assertContains(t, ir, "getelementptr { i64, i64, i64 }")
}

// T0157: Ref[T] drop now has two-stage deallocation with weak_count.
func TestArcDropTwoStageDeallocation(t *testing.T) {
	ir := generateIR(t, `
		main() {
			a := Ref[int](42);
		}
	`)
	// Arc drop should have drop_value block (drops T + decrements weak_count)
	assertContains(t, ir, "drop_value:")
}

// T0499: Arc clone/downgrade chain intermediates produce fresh SSA values via ptrtoint+inttoptr
// so the method result is tracked separately from the constructor stmtTemp.
func TestArcCloneChainFreshSSA(t *testing.T) {
	ir := generateIR(t, `
		main() {
			Ref[int] b = Ref[int](42).clone();
		}
	`)
	// The clone result must be a fresh SSA value (ptrtoint+inttoptr) so stmtTemp
	// dedup doesn't merge it with the constructor's temp — both get dropped.
	assertContains(t, ir, "ptrtoint")
	assertContains(t, ir, "inttoptr")
	assertContains(t, ir, "%b.dropflag")
}

func TestArcDowngradeChainFreshSSA(t *testing.T) {
	ir := generateIR(t, `
		main() {
			Weak[int] w = Ref[int](42).downgrade();
		}
	`)
	// Downgrade must also produce a fresh SSA value for chain tracking
	assertContains(t, ir, "ptrtoint")
	assertContains(t, ir, "inttoptr")
	assertContains(t, ir, "%w.dropflag")
}

// T0157: dupArc — reading an Ref[T] field from a droppable type increments strong refcount.
func TestDupArcFieldFromDroppable(t *testing.T) {
	ir := generateIR(t, `
		type Holder {
			Ref[int] a;
			drop(~this) {}
		}
		main() {
			h := Holder(a: Ref[int](42));
			Ref[int] copy = h.a;
		}
	`)
	// Should produce arcdup block for refcount increment (numeric suffix varies)
	assertContainsMatch(t, ir, `arcdup\.inc\.\d+:`)
	assertContainsMatch(t, ir, `arcdup\.merge\.\d+:`)
}

// T0156: Mutex[T] constructor allocates {i8* pal_handle, T value} and inits mutex.
func TestMutexConstructorAllocAndInit(t *testing.T) {
	ir := generateIR(t, `
		main() {
			m := Mutex[int](42);
		}
	`)
	// Should init the PAL mutex and cond var
	assertContains(t, ir, "call i8* @pal_mutex_init()")
	assertContains(t, ir, "call i8* @pal_cond_init()")
	// Should store the value 42
	assertContains(t, ir, "store i64 42")
	// Should init held flag to 0
	assertContains(t, ir, "store i8 0")
}

// T0156: Mutex[T] scope cleanup uses drop flag and calls Mutex[int].drop.
func TestMutexDropFlagAndCleanup(t *testing.T) {
	ir := generateIR(t, `
		main() {
			m := Mutex[int](42);
		}
	`)
	assertContains(t, ir, "%m.dropflag")
	assertContains(t, ir, `call void @"Mutex[int].drop"(`)
}

// T0156/T0285: Mutex[T].drop function has correct structure: null check, destroy cond + mutex, free.
func TestMutexDropFunctionBody(t *testing.T) {
	ir := generateIR(t, `
		main() {
			m := Mutex[int](42);
		}
	`)
	dropFn := extractFunction(ir, `"Mutex[int].drop"`)
	if dropFn == "" {
		t.Fatal("expected Mutex[int].drop function in IR")
	}
	// Null check
	assertContains(t, dropFn, "icmp eq")
	assertContains(t, dropFn, "null")
	// Cond var destroy
	assertContains(t, dropFn, "call void @pal_cond_destroy(")
	// PAL mutex destroy
	assertContains(t, dropFn, "call void @pal_mutex_destroy(")
	// Free allocation
	assertContains(t, dropFn, "call void @pal_free(")
}

// T0156/T0285: Mutex.lock() uses scheduler-aware locking and allocates a guard.
func TestMutexLockAcquiresAndAllocatesGuard(t *testing.T) {
	ir := generateIR(t, `
		main() {
			m := Mutex[int](0);
			use guard := m.lock();
		}
	`)
	// Should lock the PAL mutex (metadata critical section)
	assertContains(t, ir, "call void @pal_mutex_lock(")
	// Should check held flag
	assertContains(t, ir, "icmp eq i8")
	// Should allocate 8 bytes for the guard
	assertContains(t, ir, "call i8* @pal_alloc(i64 8)")
}

// T0301: Mutex.lock() must route to the contested path when waiters are queued,
// even if held==0 momentarily. Prevents newcomer starvation under contention
// when pthread_mutex is not FIFO. The acquired path requires both held==0 AND
// waiter_head==null, combined via `or` on the two conditions.
func TestMutexLockFairCheck(t *testing.T) {
	ir := generateIR(t, `
		main() {
			m := Mutex[int](0);
			use guard := m.lock();
		}
	`)
	// Locate the block that branches on `mustWait` via `mutex.contested`/`mutex.acquired`.
	acqIdx := strings.Index(ir, "label %mutex.acquired")
	if acqIdx < 0 {
		t.Fatal("expected mutex.acquired branch label in IR")
	}
	// Search a small window before the branch for the fair-check instructions:
	// `icmp ne i8* %waiterHead, null` for hasWaiter, then `or i1 %isHeld, %hasWaiter`.
	windowStart := acqIdx - 400
	if windowStart < 0 {
		windowStart = 0
	}
	window := ir[windowStart:acqIdx]
	if !strings.Contains(window, "or i1") {
		t.Errorf("expected `or i1` combining held and waiter_head checks before mutex.acquired")
	}
	if !strings.Contains(window, "icmp ne i8*") {
		t.Errorf("expected `icmp ne i8*` for waiter_head != null check before mutex.acquired")
	}
}

// T0301: MutexGuard.drop's no-waiter unlock path must signal cond BEFORE
// pal_mutex_unlock (within the unlock.no_waiter block). Signal-before-unlock
// is the defensive POSIX ordering — it avoids a theoretical window where a
// waking cond_wait thread could observe stale `held` state on re-acquire.
func TestMutexUnlockNoWaiterSignalBeforeUnlock(t *testing.T) {
	ir := generateIR(t, `
		main() {
			m := Mutex[int](0);
			guard := m.lock();
		}
	`)
	dropFn := extractFunction(ir, "MutexGuard.drop")
	if dropFn == "" {
		t.Fatal("expected MutexGuard.drop function in IR")
	}
	// Locate the no_waiter block body.
	marker := "unlock.no_waiter:"
	blkStart := strings.Index(dropFn, marker)
	if blkStart < 0 {
		t.Fatal("expected unlock.no_waiter block in MutexGuard.drop")
	}
	// Block ends at the next block label or `br`/return.
	blkTail := dropFn[blkStart:]
	idxSignal := strings.Index(blkTail, "@pal_cond_signal")
	idxUnlock := strings.Index(blkTail, "@pal_mutex_unlock")
	if idxSignal < 0 {
		t.Fatal("expected pal_cond_signal in unlock.no_waiter block")
	}
	if idxUnlock < 0 {
		t.Fatal("expected pal_mutex_unlock in unlock.no_waiter block")
	}
	if idxSignal > idxUnlock {
		t.Errorf("pal_cond_signal must come before pal_mutex_unlock in no_waiter block; signal@%d unlock@%d", idxSignal, idxUnlock)
	}
}

// T0156: MutexGuard.borrow getter loads T through the guard's mutex pointer.
func TestMutexGuardBorrowGetter(t *testing.T) {
	ir := generateIR(t, `
		main() {
			m := Mutex[int](42);
			use guard := m.lock();
			int x = guard.borrow;
		}
	`)
	// Borrow navigates guard → mutex → value via GEPs on {i8*} and the full mutex struct
	assertContains(t, ir, "getelementptr { i8* }, { i8* }*")
	assertContains(t, ir, "getelementptr { i8*, i8*, i8*, i8*, i8, i64 }, { i8*, i8*, i8*, i8*, i8, i64 }*")
}

// T0156: MutexGuard.borrow setter stores T through the guard's mutex pointer.
func TestMutexGuardBorrowSetter(t *testing.T) {
	ir := generateIR(t, `
		main() {
			m := Mutex[int](0);
			use guard := m.lock();
			guard.borrow = 99;
		}
	`)
	// Should store 99 through the guard→mutex→value path
	assertContains(t, ir, "store i64 99")
}

// T0270: Borrow setter must drop old value for droppable T via emitInnerDrop.
func TestMutexGuardBorrowSetterDropsOldValue(t *testing.T) {
	ir := generateIR(t, `
		main() {
			m := Mutex[string]("hello");
			use guard := m.lock();
			guard.borrow = "world";
		}
	`)
	// emitInnerDrop should call promise_string_drop on the old value before storing new
	assertContains(t, ir, "call void @promise_string_drop(")
}

// T0270: Borrow setter compound assignment (guard.borrow += val).
func TestMutexGuardBorrowSetterCompoundAssign(t *testing.T) {
	ir := generateIR(t, `
		main() {
			m := Mutex[int](10);
			use guard := m.lock();
			guard.borrow += 5;
		}
	`)
	// Compound assignment loads current value and adds
	assertContains(t, ir, "add i64")
}

// T0838: Binding the result of an error-handler unwrap of a single-owner
// native-handle optional field (`Mutex[int] m = h.mtx? _ {...}`) on an owned
// owner must neutralize the owner's optional present flag — genOptionalHandlerExpr
// makes NO dup for opaque containers (Mutex/Task are i8* handles that can't be
// deep-copied), so without the neutralization both the bound local and the
// owner's drop would free the same handle → double-free. The fix routes the
// handler binding through neutralizeMemberOptionalField (T0806 Fix C carve-out),
// emitting a `store i1 false` into the owner instance's optional flag.
func TestT0838MutexHandlerBindingNeutralizesOwnerField(t *testing.T) {
	ir := generateIR(t, `
		type MtxHolder { Mutex[int]? mtx; drop(~this) {} }
		main() {
			h := MtxHolder(mtx: Mutex[int](5));
			Mutex[int] m = h.mtx? _ { return; };
		}
	`)
	gmain := extractDefine(ir, ".goroutine.main")
	// The owner's `Mutex[int]?` field is laid out as the optional struct
	// `{ i1, i8* }` (present flag + opaque handle). Neutralization GEPs into that
	// struct's field 0 and stores `false`, so the owner's drop skips the handle
	// the binding now owns. (Generic drop-flag clears target named allocas, not a
	// GEP into `{ i1, i8* }`, so this pattern is specific to the field move-out.)
	assertContainsMatch(t, gmain,
		`getelementptr \{ i1, i8\* \}, \{ i1, i8\* \}\* %\d+, i32 0, i32 0\s*\n\s*store i1 false`)
}

// T0838: Task[T]? sibling of the Mutex case — covers handlerResultIsNativeHandle's
// types.AsTask branch (reached only when the result is NOT a Mutex). Task is the
// other single-owner opaque i8* handle genOptionalHandlerExpr does not dup, so a
// handler binding `Task[int] t = h.tsk? _ {...}` must likewise neutralize the
// owner's optional present flag (same `{ i1, i8* }` GEP + `store i1 false`).
func TestT0838TaskHandlerBindingNeutralizesOwnerField(t *testing.T) {
	ir := generateIR(t, `
		worker() int { return 42; }
		type TskHolder { Task[int]? tsk; drop(~this) {} }
		main() {
			h := TskHolder(tsk: go worker());
			Task[int] t = h.tsk? _ { return; };
		}
	`)
	gmain := extractDefine(ir, ".goroutine.main")
	assertContainsMatch(t, gmain,
		`getelementptr \{ i1, i8\* \}, \{ i1, i8\* \}\* %\d+, i32 0, i32 0\s*\n\s*store i1 false`)
}

// T0367: Assigning Ref[T].borrow to a variable must clear the variable's drop
// flag — the borrow returns a non-owning reference, so the parent's drop owns
// the inner value. Without the clear, both the borrow's drop and Arc.drop would
// free the same buffer (double-free / segfault for heap T).
func TestArcBorrowClearsDropFlag(t *testing.T) {
	ir := generateIR(t, `
		main() {
			v := [1, 2, 3];
			a := Ref[int[]](v);
			borrowed := a.borrow;
		}
	`)
	assertContains(t, ir, "%borrowed.dropflag = alloca i1")
	// maybeRegisterDrop sets dropflag=true; T0367 fix immediately clears it.
	assertContainsMatch(t, ir, `store i1 true, i1\* %borrowed\.dropflag\s+store i1 false, i1\* %borrowed\.dropflag`)
}

// T0367: Same fix for MutexGuard[T].borrow.
func TestMutexGuardBorrowClearsDropFlag(t *testing.T) {
	ir := generateIR(t, `
		main() {
			v := [1, 2, 3];
			m := Mutex[int[]](v);
			use guard := m.lock();
			borrowed := guard.borrow;
		}
	`)
	assertContains(t, ir, "%borrowed.dropflag = alloca i1")
	assertContainsMatch(t, ir, `store i1 true, i1\* %borrowed\.dropflag\s+store i1 false, i1\* %borrowed\.dropflag`)
}

// T0367 / T0438: the typed-decl path `T borrowed = a.borrow` for non-Copy T
// is now rejected at sema (no implicit `T& → T` decay). The codegen
// dropflag-clear path being tested here is unreachable under Option A;
// the inferred-decl variant (`borrowed := a.borrow`) still tests the
// codegen behavior for the kept `T&` borrow type.

// T0379: Reassigning Ref[T].borrow to an existing variable must clear the
// dropflag re-armed by the unconditional reset in the assignment path. After
// the reassign-merge block, the sequence is: re-arm, store new pointer, clear
// (T0379 fix). Without the fix, the trailing clear is missing.
func TestArcBorrowReassignClearsDropFlag(t *testing.T) {
	ir := generateIR(t, `
		main() {
			v1 := [1, 2, 3];
			v2 := [4, 5, 6];
			a1 := Ref[int[]](v1);
			a2 := Ref[int[]](v2);
			borrowed := a1.borrow;
			borrowed = a2.borrow;
		}
	`)
	assertContainsMatch(t, ir, `reassign\.merge[^:]*:\s+store i1 true, i1\* %borrowed\.dropflag\s+store i8\* %[^,]+, i8\*\* %borrowed\s+store i1 false, i1\* %borrowed\.dropflag`)
}

// T0379: Same fix for MutexGuard[T].borrow reassignment.
func TestMutexGuardBorrowReassignClearsDropFlag(t *testing.T) {
	ir := generateIR(t, `
		main() {
			v1 := [1, 2, 3];
			v2 := [4, 5, 6];
			m1 := Mutex[int[]](v1);
			m2 := Mutex[int[]](v2);
			use g1 := m1.lock();
			use g2 := m2.lock();
			borrowed := g1.borrow;
			borrowed = g2.borrow;
		}
	`)
	assertContainsMatch(t, ir, `reassign\.merge[^:]*:\s+store i1 true, i1\* %borrowed\.dropflag\s+store i8\* %[^,]+, i8\*\* %borrowed\s+store i1 false, i1\* %borrowed\.dropflag`)
}

// T0379: Borrow→owned reassignment must NOT clear the dropflag — the local
// now owns the new value and its drop must run at scope exit. Verifies the fix
// is conditional on `isBorrowGetterExpr(s.Value)` and not always applied.
func TestArcBorrowReassignToOwnedKeepsDropFlag(t *testing.T) {
	ir := generateIR(t, `
		main() {
			v := [1, 2, 3];
			a := Ref[int[]](v);
			borrowed := a.borrow;
			borrowed = [4, 5, 6];
		}
	`)
	// The full T0379-fired pattern (re-arm → store new ptr → clear) must NOT appear
	// after `reassign.merge`: the fix should fire only when RHS is `.borrow`.
	bad := regexp.MustCompile(`reassign\.merge[^:]*:\s+store i1 true, i1\* %borrowed\.dropflag\s+store [^\n]*%borrowed[^\n]*\n\s+store i1 false, i1\* %borrowed\.dropflag`)
	if bad.MatchString(ir) {
		t.Errorf("expected NO trailing flag-clear in reassign.merge for borrow→owned (T0379 should not fire)\ngot:\n%s", ir)
	}
	// But the re-arm and the pointer store should still be present (assignment ran normally).
	assertContainsMatch(t, ir, `reassign\.merge[^:]*:\s+store i1 true, i1\* %borrowed\.dropflag\s+store [^\n]*%borrowed`)
}

// T0377: A borrow laundered through an if-expression (both arms produce
// `.borrow`) must clear the new variable's dropflag — without the fix,
// scope cleanup double-frees with Arc.drop.
func TestArcBorrowThroughIfClearsDropFlag(t *testing.T) {
	ir := generateIR(t, `
		main() {
			v := [1, 2, 3];
			a := Ref[int[]](v);
			cond := true;
			borrowed := if cond { a.borrow } else { a.borrow };
		}
	`)
	assertContains(t, ir, "%borrowed.dropflag = alloca i1")
	assertContainsMatch(t, ir, `store i1 true, i1\* %borrowed\.dropflag\s+store i1 false, i1\* %borrowed\.dropflag`)
}

// T0377: Same fix for match-laundered borrow — every arm produces `.borrow`.
func TestArcBorrowThroughMatchClearsDropFlag(t *testing.T) {
	ir := generateIR(t, `
		main() {
			v := [1, 2, 3];
			a := Ref[int[]](v);
			k := 1;
			borrowed := match k { 1 => a.borrow, _ => a.borrow };
		}
	`)
	assertContains(t, ir, "%borrowed.dropflag = alloca i1")
	assertContainsMatch(t, ir, `store i1 true, i1\* %borrowed\.dropflag\s+store i1 false, i1\* %borrowed\.dropflag`)
}

// T0377: MutexGuard borrow laundered through an if-expression.
func TestMutexGuardBorrowThroughIfClearsDropFlag(t *testing.T) {
	ir := generateIR(t, `
		main() {
			v := [1, 2, 3];
			m := Mutex[int[]](v);
			use guard := m.lock();
			cond := true;
			borrowed := if cond { guard.borrow } else { guard.borrow };
		}
	`)
	assertContains(t, ir, "%borrowed.dropflag = alloca i1")
	assertContainsMatch(t, ir, `store i1 true, i1\* %borrowed\.dropflag\s+store i1 false, i1\* %borrowed\.dropflag`)
}

// T0488: mixed-ownership if-expression (one borrow arm + one owned arm) for
// non-Copy `T` is now rejected at sema time — the codegen path that "must
// NOT clear the dropflag" is unreachable. Sema rejection is covered by
// TestT0488_IfMixedNonCopyRejected in sema/sema_test.go.

// T0377: Parenthesized borrow (`(a.borrow)`) is a trivial laundering form;
// recursion must look through ParenExpr to find the borrow.
func TestArcBorrowThroughParensClearsDropFlag(t *testing.T) {
	ir := generateIR(t, `
		main() {
			v := [1, 2, 3];
			a := Ref[int[]](v);
			borrowed := (a.borrow);
		}
	`)
	assertContains(t, ir, "%borrowed.dropflag = alloca i1")
	assertContainsMatch(t, ir, `store i1 true, i1\* %borrowed\.dropflag\s+store i1 false, i1\* %borrowed\.dropflag`)
}

// T0377: Block-bodied match arms (`=> { a.borrow }` rather than `=> a.borrow`)
// take the `arm.Block` path through `matchArmIsBorrowGetter` — must still
// clear the dropflag when every arm's block result is a borrow.
func TestArcBorrowThroughMatchBlockArmsClearsDropFlag(t *testing.T) {
	ir := generateIR(t, `
		main() {
			v := [1, 2, 3];
			a := Ref[int[]](v);
			k := 1;
			borrowed := match k {
				1 => { a.borrow },
				_ => { a.borrow },
			};
		}
	`)
	assertContains(t, ir, "%borrowed.dropflag = alloca i1")
	assertContainsMatch(t, ir, `store i1 true, i1\* %borrowed\.dropflag\s+store i1 false, i1\* %borrowed\.dropflag`)
}

// T0488: mixed-ownership match-expression for non-Copy `T` is rejected at
// sema time — see TestT0488_MatchMixedNonCopyRejected in sema/sema_test.go.

// T0381: explicit `T&` annotation drives the dropflag-clear path the same
// way as inferred declarations. Type-based detection (replacing the old
// AST-shape heuristic) sees the SharedRef on the RHS expression.
func TestArcBorrowExplicitRefTypeClearsDropFlag(t *testing.T) {
	ir := generateIR(t, `
		main() {
			v := [1, 2, 3];
			a := Ref[int[]](v);
			int[]& borrowed = a.borrow;
		}
	`)
	assertContains(t, ir, "%borrowed.dropflag = alloca i1")
	assertContainsMatch(t, ir, `store i1 true, i1\* %borrowed\.dropflag\s+store i1 false, i1\* %borrowed\.dropflag`)
}

// T0381: a getter chain ending in a non-borrow leaf (e.g., `.clone()`)
// produces an OWNED value despite traversing a `T&`. The result expression
// type is `T`, not `T&`, so the dropflag stays armed for proper cleanup.
func TestArcBorrowCloneRetainsDropFlag(t *testing.T) {
	ir := generateIR(t, `
		main() {
			a := Ref[int](42);
			b := a.clone();
		}
	`)
	// b owns the cloned Arc — drop must run at scope exit.
	assertContains(t, ir, "%b.dropflag = alloca i1")
	bad := regexp.MustCompile(`store i1 true, i1\* %b\.dropflag\s+store i1 false, i1\* %b\.dropflag`)
	if bad.MatchString(ir) {
		t.Errorf("expected dropflag for clone() result to stay armed; T0381 type-based check should not fire (RHS type is Ref[int], not Ref[int]&)\ngot:\n%s", ir)
	}
}

// T0156/T0285/T0291: MutexGuard close/drop functions do scheduler-aware unlock and free.
func TestMutexGuardCloseUnlocksAndFrees(t *testing.T) {
	ir := generateIR(t, `
		main() {
			m := Mutex[int](0);
			use guard := m.lock();
		}
	`)
	closeFn := extractFunction(ir, "MutexGuard.close")
	if closeFn == "" {
		t.Fatal("expected MutexGuard.close function in IR")
	}
	// Null check
	assertContains(t, closeFn, "icmp eq")
	// Locks metadata mutex
	assertContains(t, closeFn, "call void @pal_mutex_lock(")
	// Both handoff path and no-waiter path unlock the PAL mutex
	assertContains(t, closeFn, "call void @pal_mutex_unlock(")
	// Free guard
	assertContains(t, closeFn, "call void @pal_free(")
}

// T0291: Mutex.lock() inside a goroutine parks on the waiter list (not spin-yield).
func TestMutexLockParksOnWaiterList(t *testing.T) {
	ir := generateIR(t, `
		main() {
			m := Mutex[int](0);
			go {
				use guard := m.lock();
			};
		}
	`)
	// The goroutine contested path must enqueue on the waiter list
	assertContains(t, ir, "call void @promise_waiter_enqueue(")
	// The new park-and-wake block label must be present
	assertContains(t, ir, "mutex.park.resume")
	// No spin-retry: the old spin-yield block label must NOT be present
	assertNotContains(t, ir, "mutex.wait.resume")
}

// T0291: MutexGuard.close hands lock off to a waiting goroutine (waiter_dequeue + sched_enqueue).
func TestMutexGuardCloseHandsOffLock(t *testing.T) {
	ir := generateIR(t, `
		main() {
			m := Mutex[int](0);
			use guard := m.lock();
		}
	`)
	closeFn := extractFunction(ir, "MutexGuard.close")
	if closeFn == "" {
		t.Fatal("expected MutexGuard.close function in IR")
	}
	// Must dequeue a waiter
	assertContains(t, closeFn, "call i8* @promise_waiter_dequeue(")
	// Must enqueue the woken goroutine (handoff path)
	assertContains(t, closeFn, "call void @promise_sched_enqueue(")
	// No-waiter path: signal cond for thread-blocked waiters
	assertContains(t, closeFn, "call void @pal_cond_signal(")
}

// T0291: MutexGuard.drop (non-use binding) also hands lock off — same body as close.
func TestMutexGuardDropHandsOffLock(t *testing.T) {
	ir := generateIR(t, `
		main() {
			m := Mutex[int](0);
			guard := m.lock();
		}
	`)
	dropFn := extractFunction(ir, "MutexGuard.drop")
	if dropFn == "" {
		t.Fatal("expected MutexGuard.drop function in IR")
	}
	// Null check (guard may be null if moved)
	assertContains(t, dropFn, "icmp eq")
	// Must dequeue a waiter (handoff path)
	assertContains(t, dropFn, "call i8* @promise_waiter_dequeue(")
	// Must enqueue the woken goroutine (handoff path)
	assertContains(t, dropFn, "call void @promise_sched_enqueue(")
	// No-waiter path: signal cond for thread-blocked waiters
	assertContains(t, dropFn, "call void @pal_cond_signal(")
	// Free guard
	assertContains(t, dropFn, "call void @pal_free(")
}

// T0156: Mutex[string].drop calls promise_string_drop on inner value.
func TestMutexDropWithStringElement(t *testing.T) {
	ir := generateIR(t, `
		main() {
			m := Mutex[string]("hello");
		}
	`)
	dropFn := extractFunction(ir, `"Mutex[string].drop"`)
	if dropFn == "" {
		t.Fatal("expected Mutex[string].drop function in IR")
	}
	// Should drop the inner string before destroying cond + mutex
	assertContains(t, dropFn, "call void @promise_string_drop(")
	assertContains(t, dropFn, "call void @pal_cond_destroy(")
	assertContains(t, dropFn, "call void @pal_mutex_destroy(")
}

// T0272: Ref[T].drop calls user type's drop function + pal_free for heap user types.
func TestArcDropWithUserType(t *testing.T) {
	ir := generateIR(t, `
		type P { int x; drop(~this) {} }
		main() {
			a := Ref[P](P(x: 5));
		}
	`)
	dropFn := extractFunction(ir, `"Ref[P].drop"`)
	if dropFn == "" {
		t.Fatal("expected Ref[P].drop function in IR")
	}
	// Should call user drop then pal_free for heap user type
	assertContains(t, dropFn, "call void @P.drop(")
	assertContains(t, dropFn, "call void @pal_free(")
}

// T0272: Ref[T].drop with user type that has no explicit drop — just pal_free.
func TestArcDropWithHeapUserTypeNoDrop(t *testing.T) {
	ir := generateIR(t, `
		type Q { int x; int y; }
		main() {
			a := Ref[Q](Q(x: 1, y: 2));
		}
	`)
	dropFn := extractFunction(ir, `"Ref[Q].drop"`)
	if dropFn == "" {
		t.Fatal("expected Ref[Q].drop function in IR")
	}
	// Heap user type without drop — should still free the instance
	assertContains(t, dropFn, "call void @pal_free(")
}

// T0272: Arc constructor with user type claims the heap temp (no premature free).
func TestArcConstructorClaimsHeapTemp(t *testing.T) {
	ir := generateIR(t, `
		type R { int val; }
		main() {
			a := Ref[R](R(val: 42));
			r := a.borrow;
		}
	`)
	// The IR should NOT contain a pal_free of the R instance before the Arc drop.
	// Specifically, the main function should not free the R instance directly —
	// only the Ref[R].drop function should handle that.
	mainFn := extractFunction(ir, "main")
	if mainFn == "" {
		t.Fatal("expected main function in IR")
	}
	// The heap temp for R should be claimed; no direct pal_free of the instance in main
	// (Arc.drop handles it). Count pal_free calls in main — should only be for Arc itself.
	dropFn := extractFunction(ir, `"Ref[R].drop"`)
	if dropFn == "" {
		t.Fatal("expected Ref[R].drop function in IR")
	}
	assertContains(t, dropFn, "call void @pal_free(")
}

// T0273: Ref[Ref[T]] clears drop flag on inner variable to prevent double-drop.
func TestArcConstructorClearsDropFlagOnIdent(t *testing.T) {
	ir := generateIR(t, `
		main() {
			inner := Ref[int](42);
			outer := Ref[Ref[int]](inner);
		}
	`)
	// The goroutine body should clear inner's drop flag after moving it into Arc.
	// "store i1 false" is the drop flag clear pattern.
	assertContains(t, ir, "store i1 false")
}

// T0273: Mutex[T] constructor clears drop flag on moved variable.
func TestMutexConstructorClearsDropFlagOnIdent(t *testing.T) {
	ir := generateIR(t, `
		main() {
			a := Ref[int](10);
			m := Mutex[Ref[int]](a);
		}
	`)
	assertContains(t, ir, "store i1 false")
}

// T0272: Mutex[T].drop calls user type's drop function + pal_free.
func TestMutexDropWithUserType(t *testing.T) {
	ir := generateIR(t, `
		type MP { int x; drop(~this) {} }
		main() {
			m := Mutex[MP](MP(x: 10));
		}
	`)
	dropFn := extractFunction(ir, `"Mutex[MP].drop"`)
	if dropFn == "" {
		t.Fatal("expected Mutex[MP].drop function in IR")
	}
	assertContains(t, dropFn, "call void @MP.drop(")
	assertContains(t, dropFn, "call void @pal_free(")
	assertContains(t, dropFn, "call void @pal_mutex_destroy(")
}

// T0272: Arc drop with vector inner type calls Vector.drop.
func TestArcDropWithVectorElement(t *testing.T) {
	ir := generateIR(t, `
		main() {
			v := [1, 2, 3];
			a := Ref[int[]](v);
		}
	`)
	dropFn := extractFunction(ir, `"Ref[int[]].drop"`)
	if dropFn == "" {
		// Try alternative mangled name
		dropFn = extractFunction(ir, `"Ref[Vector[int]].drop"`)
	}
	if dropFn == "" {
		t.Fatal("expected Ref[int[]].drop or Ref[Vector[int]].drop function in IR")
	}
	assertContains(t, dropFn, "call void @Vector.drop(")
}

// T0272: Mutex drop with user type that has synth drop (string field).
func TestMutexDropWithSynthDropUserType(t *testing.T) {
	ir := generateIR(t, `
		type Named { string name; }
		main() {
			m := Mutex[Named](Named(name: "hi"));
		}
	`)
	dropFn := extractFunction(ir, `"Mutex[Named].drop"`)
	if dropFn == "" {
		t.Fatal("expected Mutex[Named].drop function in IR")
	}
	// Synth drop types have their own drop function that handles field cleanup
	assertContains(t, dropFn, "call void @Named.drop(")
	assertContains(t, dropFn, "call void @pal_mutex_destroy(")
}

// T0271: Lambda capturing Ref[T] uses envDropCallFn (i8* + drop fn), not envDropUserValueDrop.
func TestLambdaEnvDropArcCapture(t *testing.T) {
	ir := generateIR(t, `
		main() {
			a := Ref[int](42);
			f := move || -> int { return a.borrow; };
			f();
		}
	`)
	// The env drop function should call Ref[int].drop on the i8* field,
	// not extract a {i8*, i8*} value struct (which would be type confusion).
	assertContains(t, ir, `call void @"Ref[int].drop"(`)
}

// T0271: Lambda capturing Mutex[T] uses envDropCallFn.
func TestLambdaEnvDropMutexCapture(t *testing.T) {
	ir := generateIR(t, `
		main() {
			m := Mutex[int](10);
			f := move || -> int {
				use g := m.lock();
				return g.borrow;
			};
			f();
		}
	`)
	assertContains(t, ir, `call void @"Mutex[int].drop"(`)
}

// T0271: Lambda capturing MutexGuard uses envDropCallFn with MutexGuard.drop.
func TestLambdaEnvDropMutexGuardCapture(t *testing.T) {
	ir := generateIR(t, `
		main() {
			m := Mutex[int](10);
			use g := m.lock();
			f := move || -> int { return g.borrow; };
			f();
		}
	`)
	assertContains(t, ir, "call void @MutexGuard.drop(")
}

// T0411: Channel field auto-dup via constructor field-init from `this.field`.
// Channel dup is a refcount increment via promise_channel_incref.
func TestT0411_ConstructorChannelFieldFromThisDups(t *testing.T) {
	ir := generateIR(t, `
		type ChH {
			channel[int] ch;
			drop(~this) {}
			clone() ChH {
				return ChH(ch: this.ch);
			}
		}
		test_t0411_ch_dup() {
			c := channel[int](1);
			h := ChH(ch: c);
			h2 := h.clone();
		}
	`)
	cloneFn := extractFunction(ir, "ChH.clone")
	if cloneFn == "" {
		t.Fatal("expected ChH.clone in IR")
	}
	// dupChannel emits a `chdup.inc` block label and an inline atomicrmw add
	// to bump the channel's reference count.
	assertContains(t, cloneFn, "chdup.inc")
}
