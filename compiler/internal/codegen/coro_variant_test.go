package codegen

import (
	"sort"
	"strings"
	"testing"

	antlr "github.com/antlr4-go/antlr/v4"
	"github.com/llir/llvm/ir"
	"github.com/llir/llvm/ir/constant"
	irtypes "github.com/llir/llvm/ir/types"
	"github.com/llir/llvm/ir/value"
	"github.com/promise-language/promise/compiler/internal/ast"
	"github.com/promise-language/promise/compiler/internal/parser"
	"github.com/promise-language/promise/compiler/internal/sema"
	"github.com/promise-language/promise/compiler/internal/types"
)

// promise-language/promise#68: which functions get a coroutine variant.
// suspendingFuncs is exercised on a hand-built module so each case states its
// whole call graph.
func TestSuspendingFuncs(t *testing.T) {
	type fnSpec struct {
		name    string
		calls   []string // callee names; "coop" is promise_sched_coop_step, "~X" calls X through a bitcast
		capable bool
	}
	cases := []struct {
		name string
		fns  []fnSpec
		want []string
	}{
		{"a function that steps the scheduler", []fnSpec{
			{name: "a", calls: []string{"coop"}, capable: true},
		}, []string{"a"}},
		{"transitive through a capable caller", []fnSpec{
			{name: "a", calls: []string{"coop"}, capable: true},
			{name: "b", calls: []string{"a"}, capable: true},
			{name: "c", calls: []string{"b"}, capable: true},
		}, []string{"a", "b", "c"}},
		{"a cycle that reaches a seed", []fnSpec{
			{name: "a", calls: []string{"coop"}, capable: true},
			{name: "x", calls: []string{"y"}, capable: true},
			{name: "y", calls: []string{"x", "a"}, capable: true},
		}, []string{"a", "x", "y"}},
		{"a cycle that reaches no seed", []fnSpec{
			{name: "p", calls: []string{"q"}, capable: true},
			{name: "q", calls: []string{"p"}, capable: true},
		}, nil},
		{"a capable caller of a non-capable blocking function", []fnSpec{
			{name: "task_drop", calls: []string{"coop"}},
			{name: "f", calls: []string{"task_drop"}, capable: true},
		}, []string{"f"}},
		{"a function with no path to a seed", []fnSpec{
			{name: "a", calls: []string{"coop"}, capable: true},
			{name: "leaf", capable: true},
			{name: "n", calls: []string{"leaf"}, capable: true},
		}, []string{"a"}},
		{"non-capable functions are never in the set", []fnSpec{
			{name: "drain", calls: []string{"coop"}},
			{name: "wrapper", calls: []string{"drain"}},
		}, nil},
		{"a bitcast callee is a direct call", []fnSpec{
			{name: "a", calls: []string{"coop"}, capable: true},
			{name: "b", calls: []string{"~a"}, capable: true},
		}, []string{"a", "b"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := ir.NewModule()
			coop := m.NewFunc("promise_sched_coop_step", irtypes.I8)
			funcs := map[string]*ir.Func{}
			capable := map[string]bool{}
			for _, f := range tc.fns {
				funcs[f.name] = m.NewFunc(f.name, irtypes.Void)
				if f.capable {
					capable[f.name] = true
				}
			}
			for _, f := range tc.fns {
				blk := funcs[f.name].NewBlock(".entry")
				for _, callee := range f.calls {
					var target value.Value
					switch {
					case callee == "coop":
						target = coop
					case strings.HasPrefix(callee, "~"):
						target = constant.NewBitCast(funcs[callee[1:]], irtypes.NewPointer(irtypes.NewFunc(irtypes.Void)))
					default:
						target = funcs[callee]
					}
					blk.NewCall(target)
				}
				blk.NewRet(nil)
			}
			var got []string
			for name := range suspendingFuncs(m, coop, capable) {
				got = append(got, name)
			}
			sort.Strings(got)
			if strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Errorf("suspendingFuncs = %v, want %v", got, tc.want)
			}
		})
	}
}

// Without the scheduler's step function (a non-WASM module) nothing can block.
func TestSuspendingFuncsWithoutCoopStep(t *testing.T) {
	m := ir.NewModule()
	f := m.NewFunc("f", irtypes.Void)
	f.NewBlock(".entry").NewRet(nil)
	if got := suspendingFuncs(m, nil, map[string]bool{"f": true}); len(got) != 0 {
		t.Errorf("suspendingFuncs without coop_step = %v, want none", got)
	}
}

// coroVariantProgram blocks through a plain helper (recv_from), two calls deep
// (twice), through a method (Sub.next) and through a failable helper (checked),
// all called from a goroutine body; add never blocks.
const coroVariantProgram = `
recv_from(Channel[int] ch) int? {
  return <-ch;
}

twice(Channel[int] ch) int? {
  return recv_from(ch);
}

add(int a, int b) int {
  return a + b;
}

checked!(Channel[int] ch) int {
  v := (<-ch) ?: 0;
  if v < 0 {
    raise error("negative");
  }
  return v;
}

type Sub {
  Channel[int] _channel;

  next() int? {
    return <-this._channel;
  }
}

main() {
  ch := channel[int](4);
  t := go {
    a := twice(ch);
    s := Sub(_channel: channel[int](1));
    b := s.next();
    c := checked(ch)? e {
      0
    };
    add(c, 1)
  };
  ch.send(1);
  ch.send(2);
  ch.send(3);
  <-t;
}
`

// On wasm32-web a function that can block gets a coroutine variant taking its
// parameters plus the caller's result slot; the plain function stays, with its
// nested wait, for non-coroutine callers.
func TestCoroVariantEmittedForBlockingHelper(t *testing.T) {
	ir := generateIRForTarget(t, coroVariantProgram, "wasm32-web")
	assertContains(t, ir, "define i8* @__user.recv_from$coro(i8* %ch, { i1, i64 }* %result.slot) presplitcoroutine")
	plain := extractDefine(ir, "__user.recv_from")
	if plain == "" {
		t.Fatalf("the plain @__user.recv_from is gone\n%s", ir)
	}
	if !strings.Contains(plain, "call i8 @promise_sched_coop_step()") {
		t.Errorf("the plain @__user.recv_from should keep its nested wait for non-coroutine callers\n%s", plain)
	}
	variant := extractDefine(ir, "__user.recv_from$coro")
	if strings.Contains(variant, "@promise_sched_coop_step") {
		t.Errorf("the variant must park, not step the scheduler\n%s", variant)
	}
	if !strings.Contains(variant, "call i8 @llvm.coro.suspend(token none, i1 false)") {
		t.Errorf("the variant's receive should park with coro.suspend\n%s", variant)
	}
}

// A goroutine body calls the variant and suspends until it is done, instead of
// calling the plain function.
func TestCoroutineAwaitsBlockingCallee(t *testing.T) {
	ir := generateIRForTarget(t, coroVariantProgram, "wasm32-web")
	ramp := extractDefine(ir, ".goroutine.0")
	if ramp == "" {
		t.Fatalf("goroutine ramp not found\n%s", ir)
	}
	for _, want := range []string{
		"call i8* @__user.twice$coro(",
		"call i8* @Sub.next$coro(",
		"call i8* @__user.checked$coro(",
		"call i1 @__promise_gen_done(",
		"call void @__promise_gen_resume(",
		"call void @__promise_gen_destroy(",
	} {
		if !strings.Contains(ramp, want) {
			t.Errorf("goroutine ramp missing %q\n%s", want, ramp)
		}
	}
	for _, plain := range []string{"@__user.twice(", "@Sub.next(", "@__user.checked("} {
		if strings.Contains(ramp, "call "+plainRetType(plain)+" "+plain) {
			t.Errorf("goroutine ramp still calls the plain %s", plain)
		}
	}
	// add never blocks: no variant, a plain call.
	assertNotContains(t, ir, "@__user.add$coro")
	if !strings.Contains(ramp, "call i64 @__user.add(") {
		t.Errorf("goroutine ramp should call the plain @__user.add\n%s", ramp)
	}
}

// A goroutine spawned as `go f(args)` runs f itself, so its ramp awaits f's
// variant, with the await slot allocated before the initial suspend (B0007).
func TestGoCallFormAwaitsBlockingTarget(t *testing.T) {
	ir := generateIRForTarget(t, `
recv_from(Channel[int] ch) int? {
  return <-ch;
}

main() {
  ch := channel[int](1);
  t := go recv_from(ch);
  ch.send(1);
  <-t;
}
`, "wasm32-web")
	ramp := extractDefine(ir, ".goroutine.0")
	if ramp == "" {
		t.Fatalf("goroutine ramp not found\n%s", ir)
	}
	if !strings.Contains(ramp, "call i8* @__user.recv_from$coro(") {
		t.Errorf("the go-call ramp should await @__user.recv_from$coro\n%s", ramp)
	}
	if strings.Contains(ramp, "call { i1, i64 } @__user.recv_from(") {
		t.Errorf("the go-call ramp must not call the plain @__user.recv_from\n%s", ramp)
	}
	start, _, ok := strings.Cut(ramp, "coro.init.suspend:")
	if !ok || !strings.Contains(start, "alloca { i1, i64 }") {
		t.Errorf("the await slot should be allocated in coro.start, before the initial suspend\n%s", ramp)
	}
}

// plainRetType is the LLVM return type of the plain functions of
// coroVariantProgram, for spelling their call instructions.
func plainRetType(callee string) string {
	if callee == "@__user.checked(" {
		return "{ i1, i64, i8* }"
	}
	return "{ i1, i64 }"
}

// A variant awaits the variants of the blocking functions it calls, so a park
// two calls deep still suspends the goroutine.
func TestCoroVariantAwaitsNestedVariant(t *testing.T) {
	ir := generateIRForTarget(t, coroVariantProgram, "wasm32-web")
	twice := extractDefine(ir, "__user.twice$coro")
	if twice == "" {
		t.Fatalf("@__user.twice$coro not found\n%s", ir)
	}
	if !strings.Contains(twice, "call i8* @__user.recv_from$coro(") {
		t.Errorf("@__user.twice$coro should await @__user.recv_from$coro\n%s", twice)
	}
	if strings.Contains(twice, "call { i1, i64 } @__user.recv_from(") {
		t.Errorf("@__user.twice$coro must not call the plain @__user.recv_from\n%s", twice)
	}
	assertContains(t, ir, "define i8* @Sub.next$coro(i8* %this, { i1, i64 }* %result.slot) presplitcoroutine")
}

// A failable function's variant routes `raise` and `return` into the caller's
// slot and the final suspend — a coroutine ramp has no `ret` of the result.
func TestCoroVariantFailableResultGoesThroughSlot(t *testing.T) {
	ir := generateIRForTarget(t, coroVariantProgram, "wasm32-web")
	checked := extractDefine(ir, "__user.checked$coro")
	if checked == "" {
		t.Fatalf("@__user.checked$coro not found\n%s", ir)
	}
	if strings.Contains(checked, "ret { i1, i64, i8* }") {
		t.Errorf("the failable variant must not ret its result\n%s", checked)
	}
	if n := strings.Count(checked, "store { i1, i64, i8* } "); n < 2 {
		t.Errorf("the failable variant should store both its error and its success into the slot (got %d stores)\n%s", n, checked)
	}
	if !strings.Contains(checked, "{ i1, i64, i8* }* %result.slot") {
		t.Errorf("the failable variant should store through %%result.slot\n%s", checked)
	}
}

// Only wasm32-web gets variants: wasm32-wasi (#718) and native targets keep
// today's code.
func TestCoroVariantOnlyOnWasmWeb(t *testing.T) {
	for _, target := range []string{"wasm32-wasi", ""} {
		var ir string
		if target == "" {
			ir = generateIR(t, coroVariantProgram)
		} else {
			ir = generateIRForTarget(t, coroVariantProgram, target)
		}
		if strings.Contains(ir, "$coro") {
			t.Errorf("target %q emitted a coroutine variant", target)
		}
	}
}

// A goroutine awaits blocking generic functions, enum methods, generic type
// methods and generic methods of types and enums (explicit and inferred type
// arguments) the same way.
func TestCoroutineAwaitsGenericAndEnumCallees(t *testing.T) {
	ir := generateIRForTarget(t, `
recv_any[T](Channel[T] ch) T? {
  return <-ch;
}

enum Src {
  Chan(Channel[int] ch),

  next() int? {
    return match this {
      Chan(ch) => <-ch,
    };
  }

  take[U](Channel[U] ch) U? {
    return <-ch;
  }
}

type Box {
  int id;

  take[U](Channel[U] ch) U? {
    return <-ch;
  }
}

type Holder[T] {
  Channel[T] ch;

  take() T? {
    return <-this.ch;
  }
}

main() {
  ch := channel[int](8);
  t := go {
    a := recv_any[int](ch);
    b := recv_any(ch);
    s := Src.Chan(channel[int](1));
    c := s.next();
    h := Holder[int](ch: channel[int](1));
    d := h.take();
    bx := Box(id: 1);
    e := bx.take[int](ch);
    f := bx.take(ch);
    g := s.take[int](ch);
    k := s.take(ch);
    (a ?: 0) + (b ?: 0) + (c ?: 0) + (d ?: 0) + (e ?: 0) + (f ?: 0) + (g ?: 0) + (k ?: 0)
  };
  ch.send(1);
  ch.send(2);
  <-t;
}
`, "wasm32-web")
	ramp := extractDefine(ir, ".goroutine.0")
	if ramp == "" {
		t.Fatalf("goroutine ramp not found\n%s", ir)
	}
	for call, want := range map[string]int{
		`call i8* @"recv_any[int]$coro"(`:    2,
		`call i8* @Src.next$coro(`:           1,
		`call i8* @"Holder[int].take$coro"(`: 1,
		`call i8* @"Box.take[int]$coro"(`:    2,
		`call i8* @"Src.take[int]$coro"(`:    2,
	} {
		if n := strings.Count(ramp, call); n != want {
			t.Errorf("goroutine ramp has %d of %q, want %d\n%s", n, call, want, ramp)
		}
	}
}

// boxBlocksThroughHelper's Box[int].get blocks only through helper, so whether
// the Box[int] unit has coroutine variants depends on a body outside Box's
// declaration.
const boxBlocksThroughHelper = `
type Box[T] {
  T v;

  get(Channel[int] ch) int {
    return helper(ch);
  }
}

helper(Channel[int] ch) int {
  return (<-ch) ?: 0;
}

main() {
  ch := channel[int](1);
  t := go {
    b := Box[int](v: 1);
    b.get(ch)
  };
  ch.send(41);
  <-t;
}
`

// An instance unit reports the variants it defines and calls, for its object
// cache key, and none once its callee no longer blocks.
func TestInstanceCoroVariants(t *testing.T) {
	got := compileForTarget(t, boxBlocksThroughHelper, "wasm32-web", nil).InstanceCoroVariants()
	want := "Box[int].get$coro,__user.helper$coro"
	if strings.Join(got["Box[int]"], ",") != want {
		t.Errorf("Box[int] coroutine variants = %v, want %s", got["Box[int]"], want)
	}
	nonBlocking := strings.Replace(boxBlocksThroughHelper, "return (<-ch) ?: 0;", "return 1;", 1)
	if got := compileForTarget(t, nonBlocking, "wasm32-web", nil).InstanceCoroVariants(); len(got["Box[int]"]) != 0 {
		t.Errorf("Box[int] reports coroutine variants %v with a helper that cannot block", got["Box[int]"])
	}
}

// On wasm32-web an instance whose object is cached still has its body
// generated: the variants that body turns out to have are part of the object's
// cache key, so the cached object is only reusable once they are known.
func TestWasmWebGeneratesCachedInstanceBodies(t *testing.T) {
	r := compileForTarget(t, boxBlocksThroughHelper, "wasm32-web", &CompileOptions{CachedInstances: map[string]bool{"Box[int]": true}})
	ir := r.Module.String()
	for _, fn := range []string{"Box[int].get", "Box[int].get$coro"} {
		if extractDefine(ir, fn) == "" {
			t.Errorf("%s has no body although it is in Box[int]'s unit", fn)
		}
	}
}

// A program in which nothing blocks through a call gets no variant at all.
func TestCoroVariantNotEmittedWhenNothingBlocks(t *testing.T) {
	ir := generateIRForTarget(t, trivialWasmWebProgram, "wasm32-web")
	assertNotContains(t, ir, "$coro")
}

// A goroutine awaits a blocking module function's variant, generic or not, and
// the variant is compiled in the module's split unit beside the plain function.
func TestCoroutineAwaitsModuleFunction(t *testing.T) {
	alphaInfo, alphaScope := parseModuleSource(t, "alpha", `
recv_one(Channel[int] ch) int? `+"`public"+` {
  return <-ch;
}

recv_any[T](Channel[T] ch) T? `+"`public"+` {
  return <-ch;
}
`)
	stdModInfo, stdScope := getCodegenStdModInfo()
	src := `
use alpha "./alpha";

main() {
  ch := channel[int](1);
  t := go {
    (alpha.recv_one(ch) ?: 0) + (alpha.recv_any[int](ch) ?: 0)
  };
  ch.send(1);
  ch.send(2);
  <-t;
}
`
	p := parser.NewPromiseParser(antlr.NewCommonTokenStream(parser.NewPromiseLexer(antlr.NewInputStream(src)), antlr.TokenDefaultChannel))
	file, errs := ast.Build("test.pr", p.CompilationUnit())
	if len(errs) > 0 {
		t.Fatalf("AST build errors: %v", errs)
	}
	file.Uses = append([]*ast.UseDecl{{Alias: "_", CatalogName: "std"}}, file.Uses...)
	info, semaErrs := sema.CheckWithModules(file, map[string]*types.Scope{"std": stdScope, "./alpha": alphaScope})
	if len(semaErrs) > 0 {
		t.Fatalf("sema errors: %v", semaErrs)
	}
	info.ModuleInfos = map[string]*sema.ModuleInfo{"std": stdModInfo, "./alpha": alphaInfo}
	info.ModuleOrder = []string{"std", "./alpha"}

	mainIR, moduleIRs := Compile(file, info, "wasm32-web").SplitModuleIRs()
	ramp := extractDefine(mainIR, ".goroutine.0")
	for _, call := range []string{"call i8* @__mod_alpha_recv_one$coro(", `call i8* @"recv_any[int]$coro"(`} {
		if !strings.Contains(ramp, call) {
			t.Errorf("the goroutine ramp has no %q\n%s", call, ramp)
		}
	}
	if extractDefine(mainIR, "__mod_alpha_recv_one$coro") != "" {
		t.Errorf("main IR defines the module function's variant")
	}
	if extractDefine(moduleIRs["alpha"], "__mod_alpha_recv_one$coro") == "" {
		t.Errorf("module alpha's IR does not define the variant\n%s", moduleIRs["alpha"])
	}
}

// parkKindsProgram has one helper per way a function can block, each called
// from a goroutine.
const parkKindsProgram = `
recv_one(Channel[int] ch) int {
  return (<-ch) ?: 0;
}

send_one(Channel[int] ch, int v) {
  ch.send(v);
}

select_one(Channel[int] x, Channel[int] y) int {
  int r = 0;
  select {
    v := <-x:
      r = v!;
    v := <-y:
      r = v!;
  }
  return r;
}

await_one(task[int] move t) int {
  return <-t;
}

join_one(Channel[int] ch) {
  _ := go {
    ch.send(1);
  };
}

lock_one(Ref[Mutex[int]] m) int {
  use guard := m.borrow.lock();
  return guard.borrow;
}

main() {
  ch := channel[int](4);
  x := channel[int](1);
  m := Ref[Mutex[int]](Mutex[int](0));
  mc := m.clone();
  t := go {
    a := recv_one(ch);
    send_one(ch, 1);
    b := select_one(ch, x);
    inner := go {
      3
    };
    c := await_one(move inner);
    join_one(ch);
    d := lock_one(mc);
    a + b + c + d
  };
  ch.send(1);
  ch.send(2);
  <-t;
}
`

// Every way a function can block parks its variant: a channel receive and send,
// select, awaiting a task, joining an un-awaited one, a contended Mutex lock. No
// variant steps the scheduler, directly or through a plain function that does,
// while each plain twin still does — so the helper really blocks.
func TestCoroVariantsNeverPump(t *testing.T) {
	r := compileForTarget(t, parkKindsProgram, "wasm32-web", nil)
	c := r.compiler
	every := make(map[string]bool, len(r.Module.Funcs))
	for _, f := range r.Module.Funcs {
		every[f.Name()] = true
	}
	pumps := suspendingFuncs(r.Module, c.funcs["promise_sched_coop_step"], every)
	for _, helper := range []string{"recv_one", "send_one", "select_one", "await_one", "join_one", "lock_one"} {
		plain := "__user." + helper
		if !pumps[plain] {
			t.Errorf("%s does not step the scheduler, so it does not exercise a park", plain)
		}
		variant := c.coroVariantFns[plain]
		if variant == nil || len(variant.Blocks) == 0 {
			t.Errorf("%s has no coroutine variant", plain)
			continue
		}
		if pumps[variant.Name()] {
			t.Errorf("%s steps the scheduler instead of parking", variant.Name())
		}
	}
}

// A failable constructor whose parent constructor blocks gets a variant, which
// awaits the parent's variant through super() and routes the parent's error into
// its own result slot: the variant's ramp has no `ret` of the result.
func TestFailableSuperInCoroVariant(t *testing.T) {
	ir := generateIRForTarget(t, `
type Base {
  int value;

  new!(~this, Channel[int] move ch) {
    v := (<-ch) ?: 0;
    if v < 0 {
      raise error("negative");
    }
    this.value = v;
  }
}

type Child is Base {
  new!(~this, Channel[int] move ch) {
    super(ch: move ch);
  }
}

main() {
  ch := channel[int](1);
  ch.send(1);
  k := Child(ch: move ch)? e {
    return;
  };
}
`, "wasm32-web")
	child := extractDefine(ir, "Child.new$coro")
	if child == "" {
		t.Fatalf("Child.new has no coroutine variant\n%s", ir)
	}
	if !strings.Contains(child, "call i8* @Base.new$coro(") {
		t.Errorf("Child.new$coro should await Base.new$coro through super()\n%s", child)
	}
	if strings.Contains(child, "ret { i1, i8* }") {
		t.Errorf("Child.new$coro returns the result instead of storing it\n%s", child)
	}
	if !strings.Contains(child, "{ i1, i8* }* %result.slot") {
		t.Errorf("Child.new$coro should store its result through %%result.slot\n%s", child)
	}
}

// Every way out of a variant with a result — return, raise, falling off the end,
// the panic exit — stores into the caller's slot before it reaches the final
// suspend, so the caller never loads a slot nothing wrote. A failable void
// function's success is falling off its end.
func TestCoroVariantEveryExitStoresResult(t *testing.T) {
	r := compileForTarget(t, coroVariantProgram+`
wait_open!(Channel[int] ch) {
  v := (<-ch) ?: -1;
  if v < 0 {
    raise error("closed");
  }
}

spawn_wait_open(Channel[int] ch) {
  t := go {
    ok := true;
    wait_open(ch)? e {
      ok = false;
    };
    ok
  };
  <-t;
}
`, "wasm32-web", nil)
	for _, plain := range []string{"__user.recv_from", "__user.checked", "__user.wait_open"} {
		variant := r.compiler.coroVariantFns[plain]
		if variant == nil || len(variant.Blocks) == 0 {
			t.Errorf("%s has no coroutine variant", plain)
			continue
		}
		slot := variant.Params[len(variant.Params)-1]
		exits := 0
		for _, b := range variant.Blocks {
			br, ok := b.Term.(*ir.TermBr)
			if !ok || br.Target.Ident() != "%final.suspend" {
				continue
			}
			exits++
			stored := false
			for _, in := range b.Insts {
				if st, ok := in.(*ir.InstStore); ok && st.Dst == slot {
					stored = true
				}
			}
			if !stored {
				t.Errorf("%s: block %s reaches the final suspend without storing the result\n%s", variant.Name(), b.Name(), variant.LLString())
			}
		}
		if exits == 0 {
			t.Errorf("%s: no block reaches the final suspend", variant.Name())
		}
	}
}
