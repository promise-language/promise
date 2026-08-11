package codegen

import (
	"strings"
	"testing"
)

// T1460: emitViewMethodAdapter fills a concrete method's extra trailing params
// with their default expressions. saveState() saves but does not clear the
// caller's temp/scope state, so those defaults were compiled against the
// CALLER's live codegen state: their temps landed in the caller's lists (which
// restoreState then discards → leak) and an unwind path walked the caller's
// scope bindings (→ loads of caller allocas inside the adapter, malformed IR).
// The adapter now gets its own function-body state and drains its default temps
// right after the forwarding call returns.

const t1460TaggerIface = `
type Tagger ` + "`structural" + ` {
  tag(this) string ` + "`abstract" + `;
}
`

// A heap-allocating string default must be freed inside the adapter, guarded by
// a runtime alias check against the value the concrete method handed back.
func TestT1460ViewAdapterDropsHeapStringDefault(t *testing.T) {
	ir := generateIR(t, t1460TaggerIface+`
type HeapParent {
  int v;
  tag(this, string suffix = "-" + "end") string `+"`public"+` => "p{this.v}{suffix}";
}
main() {
  HeapParent p = HeapParent(v: 1);
  print_line(p.tag());
  Tagger t = p;
  print_line(t.tag());
}
`)
	adapter := extractDefine(ir, "HeapParent.tag$view_adapt")
	if adapter == "" {
		t.Fatalf("no view adapter emitted\n%s", ir)
	}
	assertContains(t, adapter, "@promise_string_concat")
	assertContains(t, adapter, "call void @promise_string_drop")
	assertContains(t, adapter, "adapt.alias.clear")
}

// The default's unwind path (a constructor emits a panic check) must reference
// only the adapter's own allocas — never the caller's named-local drop flags.
func TestT1460ViewAdapterHeapUserDefaultDoesNotReferenceCallerAllocas(t *testing.T) {
	ir := generateIR(t, t1460TaggerIface+`
type Boxed `+"`public"+` {
  int n;
}
type UserDefP {
  int v;
  tag(this, Boxed b = Boxed(n: 7)) string `+"`public"+` => "u{b.n}";
}
main() {
  UserDefP p = UserDefP(v: 1);
  print_line(p.tag());
  Tagger t = p;
  print_line(t.tag());
}
`)
	adapter := extractDefine(ir, "UserDefP.tag$view_adapt")
	if adapter == "" {
		t.Fatalf("no view adapter emitted\n%s", ir)
	}
	// The constructed default is freed on both the normal and the unwind path.
	assertContains(t, adapter, "call void @pal_free")
	if strings.Contains(adapter, ".dropflag") {
		t.Errorf("adapter references a caller-scope drop flag (malformed IR):\n%s", adapter)
	}
	if strings.Contains(adapter, "%p.") || strings.Contains(adapter, "* %p\n") {
		t.Errorf("adapter references the caller's %%p alloca (malformed IR):\n%s", adapter)
	}
}

// A tuple default carries no i8* drop function — it must be registered as a
// field-wise tuple temp (T1233) so its droppable elements are freed.
func TestT1460ViewAdapterTupleDefaultDrainsFieldWise(t *testing.T) {
	ir := generateIR(t, t1460TaggerIface+`
type TupP {
  int v;
  tag(this, (int, string) pr = (1, "a" + "b")) string `+"`public"+` {
    (n, s) := pr;
    return "t{n}{s}";
  }
}
main() {
  TupP p = TupP(v: 1);
  print_line(p.tag());
  Tagger t = p;
  print_line(t.tag());
}
`)
	adapter := extractDefine(ir, "TupP.tag$view_adapt")
	if adapter == "" {
		t.Fatalf("no view adapter emitted\n%s", ir)
	}
	assertContains(t, adapter, "tuptmp.drop")
	assertContains(t, adapter, "call void @promise_string_drop")
}

// A `T~` param is passed as a POINTER to the caller's storage (B0149), so the
// adapter must materialize the default into a slot and forward its address —
// passing the value raw produced a type-mismatched call. The callee only borrows,
// so the adapter still drops the value after the call returns.
func TestT1460ViewAdapterMutRefDefaultPassedByAddress(t *testing.T) {
	ir := generateIR(t, t1460TaggerIface+`
type MutRefP {
  int v;
  tag(this, string ~s = "-" + "mut") string `+"`public"+` => "r{this.v}{s}";
}
main() {
  MutRefP p = MutRefP(v: 1);
  print_line(p.tag());
  Tagger t = p;
  print_line(t.tag());
}
`)
	adapter := extractDefine(ir, "MutRefP.tag$view_adapt")
	if adapter == "" {
		t.Fatalf("no view adapter emitted\n%s", ir)
	}
	// The concrete method takes i8**; the adapter must forward an alloca, not the
	// i8* the concat produced.
	assertContains(t, adapter, "alloca i8*")
	assertContains(t, adapter, "@MutRefP.tag(i8* %this, i8** ")
	// The mutable borrow does not transfer ownership — the adapter frees the default.
	assertContains(t, adapter, "call void @promise_string_drop")
}

// A void concrete has no result, so nothing can alias the default: the drain must
// free it unconditionally and emit no alias check.
func TestT1460ViewAdapterVoidReturnDrainsWithoutAliasCheck(t *testing.T) {
	ir := generateIR(t, `
type Runner `+"`structural"+` {
  run(this) `+"`abstract"+`;
}
type VoidP {
  int v;
  run(this, string s = "-" + "v") `+"`public"+` {
    print_line(s);
  }
}
main() {
  VoidP p = VoidP(v: 1);
  p.run();
  Runner r = p;
  r.run();
}
`)
	adapter := extractDefine(ir, "VoidP.run$view_adapt")
	if adapter == "" {
		t.Fatalf("no view adapter emitted\n%s", ir)
	}
	assertContains(t, adapter, "call void @promise_string_drop")
	assertNotContains(t, adapter, "adapt.alias.clear")
}

// A failable void returns {i1, i8*} — field 1 is the ERROR pointer, not a value.
// Treating it as an alias source would clear the default's drop flag whenever the
// error pointer happened to match, so the drain must skip the alias check here.
func TestT1460ViewAdapterFailableVoidSkipsAliasCheck(t *testing.T) {
	ir := generateIR(t, `
type FailRunner `+"`structural"+` {
  run!(this) `+"`abstract"+`;
}
type FailVoidP {
  int v;
  run!(this, string s = "-" + "fv") `+"`public"+` {
    if (this.v < 0) {
      raise error("negative");
    }
    print_line(s);
  }
}
main() {
  FailVoidP p = FailVoidP(v: 1);
  p.run()?! ;
  FailRunner r = p;
  r.run()?! ;
}
`)
	adapter := extractDefine(ir, "FailVoidP.run$view_adapt")
	if adapter == "" {
		t.Fatalf("no view adapter emitted\n%s", ir)
	}
	assertContains(t, adapter, "call void @promise_string_drop")
	assertNotContains(t, adapter, "adapt.alias.clear")
}

// An int result is not a pointer, so extractAliasPtr yields nothing and the
// adapter frees the default unconditionally.
func TestT1460ViewAdapterNonPointerReturnDrainsWithoutAliasCheck(t *testing.T) {
	ir := generateIR(t, `
type Counter `+"`structural"+` {
  count(this) int `+"`abstract"+`;
}
type IntP {
  int v;
  count(this, string s = "ab" + "c") int `+"`public"+` => s.len + this.v;
}
main() {
  IntP p = IntP(v: 1);
  print_line("{p.count()}");
  Counter c = p;
  print_line("{c.count()}");
}
`)
	adapter := extractDefine(ir, "IntP.count$view_adapt")
	if adapter == "" {
		t.Fatalf("no view adapter emitted\n%s", ir)
	}
	assertContains(t, adapter, "call void @promise_string_drop")
	assertNotContains(t, adapter, "adapt.alias.clear")
}

// When the interface supplies leading params, only the TRAILING extras are
// synthesized: the adapter must forward its own parameter untouched and own just
// the default it created.
func TestT1460ViewAdapterForwardsIfaceParamsAndOwnsOnlyTheDefault(t *testing.T) {
	ir := generateIR(t, `
type Joiner `+"`structural"+` {
  join(this, string head) string `+"`abstract"+`;
}
type JoinP {
  int v;
  join(this, string head, string tail = "-" + "t") string `+"`public"+` => "{head}{tail}";
}
main() {
  JoinP p = JoinP(v: 1);
  print_line(p.join("h"));
  Joiner j = p;
  print_line(j.join("h"));
}
`)
	adapter := extractDefine(ir, "JoinP.join$view_adapt")
	if adapter == "" {
		t.Fatalf("no view adapter emitted\n%s", ir)
	}
	// The interface's `head` arrives as %p0 and is forwarded straight through.
	assertContains(t, adapter, "i8* %p0")
	assertContains(t, adapter, "@JoinP.join(i8* %this, i8* %p0")
	// Exactly one default was synthesized, so exactly one drop is emitted.
	if got := strings.Count(adapter, "call void @promise_string_drop"); got != 1 {
		t.Errorf("want 1 drop for the single synthesized default, got %d:\n%s", got, adapter)
	}
}

// The adapter is a standalone function even when first emitted while the compiler
// is inside a goroutine's coroutine body — its default must not branch to the
// enclosing coroutine's suspend/cleanup blocks.
func TestT1460ViewAdapterEmittedInsideCoroutineHasNoCoroState(t *testing.T) {
	ir := generateIR(t, `
type Tagger2 `+"`structural"+` {
  tag(this) string `+"`abstract"+`;
}
type CoroP {
  int v;
  tag(this, string s = "-" + "co") string `+"`public"+` => "c{this.v}{s}";
}
main() {
  CoroP direct = CoroP(v: 5);
  print_line(direct.tag());
  ch := channel[string](capacity: 1);
  go {
    CoroP p = CoroP(v: 5);
    Tagger2 t = p;
    ch.send(t.tag());
    ch.close();
  };
  for got in ch {
    print_line(got);
  }
}
`)
	adapter := extractDefine(ir, "CoroP.tag$view_adapt")
	if adapter == "" {
		t.Fatalf("no view adapter emitted\n%s", ir)
	}
	// It still owns and frees its default...
	assertContains(t, adapter, "call void @promise_string_drop")
	// ...but carries none of the enclosing coroutine's machinery.
	assertNotContains(t, adapter, "llvm.coro")
	assertNotContains(t, adapter, "coro.suspend")
	assertNotContains(t, adapter, "coro.cleanup")
}

// A default whose value carries a heap closure env is drained from the env-temp
// list; when the method hands that closure back, the env pointer is what the
// alias check must compare (field 1 of the fat pointer).
func TestT1460ViewAdapterClosureEnvDefaultDrainedAndAliasChecked(t *testing.T) {
	ir := generateIR(t, `
type EnvCaller `+"`structural"+` {
  call(this) int `+"`abstract"+`;
}
type EnvMaker `+"`structural"+` {
  make(this) (int) -> int `+"`abstract"+`;
}
make_adder(int n) (int) -> int {
  return |int x| -> x + n;
}
type EnvP {
  int v;
  call(this, (int) -> int f = make_adder(10)) int `+"`public"+` => f(1);
}
type EnvAliasP {
  int v;
  make(this, (int) -> int f = make_adder(10)) (int) -> int `+"`public"+` => f;
}
main() {
  EnvP p = EnvP(v: 1);
  print_line("{p.call()}");
  EnvCaller c = p;
  print_line("{c.call()}");

  EnvAliasP q = EnvAliasP(v: 1);
  g := q.make();
  print_line("{g(1)}");
  EnvMaker m = q;
  h := m.make();
  print_line("{h(1)}");
}
`)
	// Not returned: the adapter owns the env and frees it, with no alias check
	// (an int result cannot alias a pointer).
	dropper := extractDefine(ir, "EnvP.call$view_adapt")
	if dropper == "" {
		t.Fatalf("no view adapter emitted for EnvP.call\n%s", ir)
	}
	assertContains(t, dropper, "env.tmp.drop")
	assertNotContains(t, dropper, "adapt.alias.clear")

	// Returned: the caller owns the env from here, so the temp's flag is cleared
	// when the returned fat pointer's env field matches.
	aliaser := extractDefine(ir, "EnvAliasP.make$view_adapt")
	if aliaser == "" {
		t.Fatalf("no view adapter emitted for EnvAliasP.make\n%s", ir)
	}
	assertContains(t, aliaser, "env.tmp.drop")
	assertContains(t, aliaser, "adapt.alias.clear")
}

// A channel default is a droppable resource with its own free path — the drain
// must reach it just like a string or a vector.
func TestT1460ViewAdapterChannelDefaultIsDropped(t *testing.T) {
	ir := generateIR(t, t1460TaggerIface+`
type ChanP {
  int v;
  tag(this, channel[int] c = channel[int](capacity: 2)) string `+"`public"+` {
    c.close();
    return "C{this.v}";
  }
}
main() {
  ChanP p = ChanP(v: 1);
  print_line(p.tag());
  Tagger t = p;
  print_line(t.tag());
}
`)
	adapter := extractDefine(ir, "ChanP.tag$view_adapt")
	if adapter == "" {
		t.Fatalf("no view adapter emitted\n%s", ir)
	}
	assertContains(t, adapter, "@promise_channel_new")
	assertContains(t, adapter, `@"Channel[int].drop"`)
	// Guarded by the same runtime alias check as any other pointer-valued default.
	assertContains(t, adapter, "adapt.alias.clear")
}

// A trailing `move` param is consumed by the callee, so the adapter must not free
// it. The grammar puts `= expression` on regularParam only, so a move param can
// reach the adapter only as an optional with no default — filled with a
// zeroinitializer `none`, which owns nothing to begin with.
func TestT1460ViewAdapterMoveParamIsNotDroppedByTheAdapter(t *testing.T) {
	ir := generateIR(t, t1460TaggerIface+`
type MoveP {
  int v;
  tag(this, string? move s) string `+"`public"+` => "O{this.v}";
}
main() {
  MoveP p = MoveP(v: 1);
  Tagger t = p;
  print_line(t.tag());
}
`)
	adapter := extractDefine(ir, "MoveP.tag$view_adapt")
	if adapter == "" {
		t.Fatalf("no view adapter emitted\n%s", ir)
	}
	// The consumed param is passed as `none` and the adapter drops nothing.
	assertContains(t, adapter, "zeroinitializer")
	assertNotContains(t, adapter, "promise_string_drop")
	assertNotContains(t, adapter, "adapt.alias.clear")
}

// A .rodata string literal default allocates nothing, so the adapter must stay
// exactly as it was — a forward and a return, no drop machinery.
func TestT1460ViewAdapterNonAllocatingDefaultEmitsNoDrop(t *testing.T) {
	ir := generateIR(t, t1460TaggerIface+`
type LitP {
  int v;
  tag(this, string suffix = "-end") string `+"`public"+` => "l{this.v}{suffix}";
}
main() {
  LitP p = LitP(v: 1);
  print_line(p.tag());
  Tagger t = p;
  print_line(t.tag());
}
`)
	adapter := extractDefine(ir, "LitP.tag$view_adapt")
	if adapter == "" {
		t.Fatalf("no view adapter emitted\n%s", ir)
	}
	assertNotContains(t, adapter, "promise_string_drop")
	assertNotContains(t, adapter, "adapt.alias.clear")
	assertNotContains(t, adapter, "alloca")
}
