package codegen

import (
	"strings"
	"testing"

	"github.com/llir/llvm/ir"
	irtypes "github.com/llir/llvm/ir/types"
)

// T1222: a synthesized numbered coroutine ramp (`.goroutine.N` / `.generator.N`)
// spawned inside a generic instance method must be attributed to the SAME split
// unit as its enclosing method. Otherwise the ramp definition is orphaned in main
// IR while the method body lands in the instance `.bc`; a later compile serving the
// instance `.bc` from cache references `.goroutine.N` by a number the fresh main IR
// reassigns → undefined-symbol link error (isolated) or wrong-body UAF (batched).

// attributeCoroToEnclosing routes the ramp into the enclosing function's ownership
// unit. Instance ownership wins over module ownership; an unowned (plain-main or
// synthesized-default) enclosing function, or a nil c.fn, is a no-op.
func TestAttributeCoroToEnclosing(t *testing.T) {
	c := &Compiler{
		module:             ir.NewModule(),
		moduleOwnedFuncs:   map[string]string{},
		instanceOwnedFuncs: map[string]string{},
	}
	newFn := func(name string) *ir.Func {
		return c.module.NewFunc(name, irtypes.I8Ptr)
	}

	// Instance-owned enclosing method → ramp inherits the instance owner.
	enclosing := newFn("Box__int.send_value")
	c.instanceOwnedFuncs["Box__int.send_value"] = "Box__int"
	c.attributeCoroToEnclosing(".goroutine.0", enclosing)
	if got := c.instanceOwnedFuncs[".goroutine.0"]; got != "Box__int" {
		t.Errorf(".goroutine.0 instance owner = %q, want %q", got, "Box__int")
	}
	if _, ok := c.moduleOwnedFuncs[".goroutine.0"]; ok {
		t.Error(".goroutine.0 must not also be module-owned")
	}

	// Module-owned enclosing method → ramp inherits the module owner.
	enclosing = newFn("__mod_std_helper")
	c.moduleOwnedFuncs["__mod_std_helper"] = "std"
	c.attributeCoroToEnclosing(".goroutine.1", enclosing)
	if got := c.moduleOwnedFuncs[".goroutine.1"]; got != "std" {
		t.Errorf(".goroutine.1 module owner = %q, want %q", got, "std")
	}

	// Instance ownership takes precedence when the enclosing function is in both maps.
	enclosing = newFn("Both__int.m")
	c.instanceOwnedFuncs["Both__int.m"] = "Both__int"
	c.moduleOwnedFuncs["Both__int.m"] = "std"
	c.attributeCoroToEnclosing(".goroutine.2", enclosing)
	if got := c.instanceOwnedFuncs[".goroutine.2"]; got != "Both__int" {
		t.Errorf(".goroutine.2 instance owner = %q, want %q", got, "Both__int")
	}
	if _, ok := c.moduleOwnedFuncs[".goroutine.2"]; ok {
		t.Error(".goroutine.2 must not be module-owned (instance ownership wins)")
	}

	// Unowned enclosing function (plain main code) → no-op, ramp stays in main.
	enclosing = newFn("__user.run")
	c.attributeCoroToEnclosing(".goroutine.3", enclosing)
	if _, ok := c.instanceOwnedFuncs[".goroutine.3"]; ok {
		t.Error(".goroutine.3 must stay unowned for a plain-main enclosing function")
	}
	if _, ok := c.moduleOwnedFuncs[".goroutine.3"]; ok {
		t.Error(".goroutine.3 must stay unowned for a plain-main enclosing function")
	}

	// nil enclosing → no-op (no panic).
	c.attributeCoroToEnclosing(".goroutine.4", nil)
	if _, ok := c.instanceOwnedFuncs[".goroutine.4"]; ok {
		t.Error(".goroutine.4 must stay unowned when enclosing is nil")
	}
}

// rampOwnerFor returns the owner recorded in instanceOwnedFuncs for the first
// function whose name carries the given synthesized prefix (".goroutine." /
// ".generator."), plus that function's name. owner is "" if none is owned.
func rampOwnerFor(c *Compiler, prefix string) (name, owner string) {
	for fname, o := range c.instanceOwnedFuncs {
		if strings.HasPrefix(fname, prefix) {
			return fname, o
		}
	}
	return "", ""
}

// End-to-end: a generic type whose method spawns a go-block capturing `this`. The
// `.goroutine.N` ramp must be instance-owned, DEFINED in the instance IR, and only
// declared (never defined) in the split main IR. A plain single-build runtime test
// passes even when broken — this partitioning guard is the precise check.
func TestGoBlockThisRampInstanceOwned(t *testing.T) {
	r := compileResult(t, `
		type Box[T] {
			T value;
			send_value(this, Channel[T] out) {
				go { out.send(this.value); };
			}
		}
		main() {
			out := channel[int](capacity: 1);
			Box[int](value: 99).send_value(out);
		}
	`)

	rampName, owner := rampOwnerFor(r.compiler, ".goroutine.")
	if rampName == "" {
		t.Fatal("no .goroutine.N ramp was attributed to an instance (T1222 regression)")
	}
	// The enclosing method Box[int].send_value must own the ramp.
	if want := r.compiler.instanceOwnedFuncs["Box[int].send_value"]; owner != want || want == "" {
		t.Fatalf("ramp %s owner = %q, want method owner %q", rampName, owner, want)
	}

	instIRs := r.InstanceIRs()
	instIR, ok := instIRs[owner]
	if !ok {
		t.Fatalf("no instance IR for %q", owner)
	}
	if extractDefine(instIR, rampName) == "" {
		t.Errorf("ramp %s must be DEFINED in instance IR %q", rampName, owner)
	}

	mainIR, _ := r.SplitModuleIRs()
	if extractDefine(mainIR, rampName) != "" {
		t.Errorf("ramp %s must NOT be defined in main IR (orphaned ramp → T1222)", rampName)
	}
}

// Sibling site: `go extern_func()` inside a generic method builds a
// `.go_extern_wrap.*` shim that the ramp calls. Once the ramp is attributed to the
// instance, the wrapper must be co-located there too — otherwise the instance `.bc`
// references a wrapper orphaned in main IR (cross-program undefined symbol). The
// wrapper is instance-owned and DEFINED in the instance IR, never in main IR.
func TestGoExternWrapperInstanceOwned(t *testing.T) {
	r := compileResult(t, `
		get_data(int x) string `+"`"+`extern("test_get_data");
		type Box[T] {
			T value;
			spawn(this) task[string] { return go get_data(42); }
		}
		main() {
			b := Box[int](value: 1);
			t := b.spawn();
		}
	`)

	wrapName, owner := rampOwnerFor(r.compiler, ".go_extern_wrap.")
	if wrapName == "" {
		t.Fatal("no .go_extern_wrap.* shim was attributed to an instance (T1222 regression)")
	}

	instIRs := r.InstanceIRs()
	instIR, ok := instIRs[owner]
	if !ok {
		t.Fatalf("no instance IR for %q", owner)
	}
	if extractDefine(instIR, wrapName) == "" {
		t.Errorf("extern wrapper %s must be DEFINED in instance IR %q", wrapName, owner)
	}

	mainIR, _ := r.SplitModuleIRs()
	if extractDefine(mainIR, wrapName) != "" {
		t.Errorf("extern wrapper %s must NOT be defined in main IR (orphaned → T1222)", wrapName)
	}
}

// Sibling site: a generator method on a generic type creates a `.generator.N`
// helper via the same unowned NewFunc pattern. It must likewise be attributed to
// the enclosing instance so it is not orphaned in main IR.
func TestGeneratorHelperInstanceOwned(t *testing.T) {
	r := compileResult(t, `
		type Gen[T] {
			T value;
			produce(this) stream[T] {
				yield this.value;
			}
		}
		main() {
			g := Gen[int](value: 5);
			for x in g.produce() {}
		}
	`)

	helperName, owner := rampOwnerFor(r.compiler, ".generator.")
	if helperName == "" {
		t.Fatal("no .generator.N helper was attributed to an instance (T1222 regression)")
	}

	instIRs := r.InstanceIRs()
	instIR, ok := instIRs[owner]
	if !ok {
		t.Fatalf("no instance IR for %q", owner)
	}
	if extractDefine(instIR, helperName) == "" {
		t.Errorf("generator helper %s must be DEFINED in instance IR %q", helperName, owner)
	}

	mainIR, _ := r.SplitModuleIRs()
	if extractDefine(mainIR, helperName) != "" {
		t.Errorf("generator helper %s must NOT be defined in main IR (orphaned → T1222)", helperName)
	}
}

// coroRampName builds the ramp symbol: a bare `.kind.N` for an unowned (main-code)
// spawner, or `.kind.<enclosing>.N` when the spawner is a module/instance-owned
// function whose ramp lands in a cross-program cached `.bc`. Both branches are
// exercised so the qualification rule is pinned independent of the compile path.
func TestCoroRampName(t *testing.T) {
	if got := coroRampName("goroutine", "", 3); got != ".goroutine.3" {
		t.Errorf("bare ramp name = %q, want %q", got, ".goroutine.3")
	}
	if got := coroRampName("generator", "Box__int.produce", 0); got != ".generator.Box__int.produce.0" {
		t.Errorf("qualified ramp name = %q, want %q", got, ".generator.Box__int.produce.0")
	}
}

// coroEnclosingQualifier returns the enclosing name to embed in a ramp symbol ONLY
// for module/instance-owned spawners (whose ramp is cached across programs and can
// otherwise collide on a bare counter). nil and plain-main spawners return "" so the
// ramp keeps its bare, stable name. This pins every branch of the qualifier — the
// helper is otherwise only reached through the full compile path, leaving the
// module-owned branch uncovered by the end-to-end tests below.
func TestCoroEnclosingQualifier(t *testing.T) {
	c := &Compiler{
		module:             ir.NewModule(),
		moduleOwnedFuncs:   map[string]string{},
		instanceOwnedFuncs: map[string]string{},
	}
	newFn := func(name string) *ir.Func { return c.module.NewFunc(name, irtypes.I8Ptr) }

	if got := c.coroEnclosingQualifier(nil); got != "" {
		t.Errorf("nil enclosing qualifier = %q, want \"\"", got)
	}

	inst := newFn("Box__int.send_value")
	c.instanceOwnedFuncs["Box__int.send_value"] = "Box__int"
	if got := c.coroEnclosingQualifier(inst); got != "Box__int.send_value" {
		t.Errorf("instance-owned qualifier = %q, want %q", got, "Box__int.send_value")
	}

	mod := newFn("__mod_std_Random.ints")
	c.moduleOwnedFuncs["__mod_std_Random.ints"] = "std"
	if got := c.coroEnclosingQualifier(mod); got != "__mod_std_Random.ints" {
		t.Errorf("module-owned qualifier = %q, want %q", got, "__mod_std_Random.ints")
	}

	// Both maps → instance ownership wins (matches attributeCoroToEnclosing).
	both := newFn("Both__int.m")
	c.instanceOwnedFuncs["Both__int.m"] = "Both__int"
	c.moduleOwnedFuncs["Both__int.m"] = "std"
	if got := c.coroEnclosingQualifier(both); got != "Both__int.m" {
		t.Errorf("dual-owned qualifier = %q, want instance name %q", got, "Both__int.m")
	}

	if got := c.coroEnclosingQualifier(newFn("__user.run")); got != "" {
		t.Errorf("plain-main qualifier = %q, want \"\"", got)
	}
}

// Sibling site: a `go recv.method(...)` whose callee is a method call routes through
// genGoCallExprViaBlock (not the bare-ident fast path). Its `.goroutine.N` ramp is
// created via the same unowned NewFunc pattern and must be attributed to the enclosing
// instance method. Captures a local (not `this`) to isolate the T1222 split-unit
// attribution from the separate viaBlock `this`-snapshot UAF tracked as T1261.
func TestGoCallExprViaBlockRampInstanceOwned(t *testing.T) {
	r := compileResult(t, `
		type Box[T] {
			T value;
			relay(this, Channel[T] out, T v) {
				go out.send(v);
			}
		}
		main() {
			out := channel[int](capacity: 1);
			Box[int](value: 1).relay(out, 7);
		}
	`)

	rampName, owner := rampOwnerFor(r.compiler, ".goroutine.")
	if rampName == "" {
		t.Fatal("no .goroutine.N ramp was attributed to an instance (T1222 regression)")
	}
	if want := r.compiler.instanceOwnedFuncs["Box[int].relay"]; owner != want || want == "" {
		t.Fatalf("ramp %s owner = %q, want method owner %q", rampName, owner, want)
	}

	instIRs := r.InstanceIRs()
	instIR, ok := instIRs[owner]
	if !ok {
		t.Fatalf("no instance IR for %q", owner)
	}
	if extractDefine(instIR, rampName) == "" {
		t.Errorf("viaBlock ramp %s must be DEFINED in instance IR %q", rampName, owner)
	}

	mainIR, _ := r.SplitModuleIRs()
	if extractDefine(mainIR, rampName) != "" {
		t.Errorf("viaBlock ramp %s must NOT be defined in main IR (orphaned → T1222)", rampName)
	}
}

// Sibling site: a `go free_func(...)` with a bare-ident callee routes through the
// fast-path genGoCallExpr, whose `.goroutine.N` ramp is created via the same unowned
// NewFunc pattern. When the spawner is a generic instance method the ramp must be
// attributed to that instance too, or the cached instance `.bc` references a ramp
// defined only in main IR.
func TestGoCallExprFastPathRampInstanceOwned(t *testing.T) {
	r := compileResult(t, `
		tick(Channel[int] sig) { sig.send(1); }
		type Box[T] {
			T value;
			spawn(this, Channel[int] sig) {
				go tick(sig);
			}
		}
		main() {
			sig := channel[int](capacity: 1);
			Box[int](value: 1).spawn(sig);
		}
	`)

	rampName, owner := rampOwnerFor(r.compiler, ".goroutine.")
	if rampName == "" {
		t.Fatal("no .goroutine.N ramp was attributed to an instance (T1222 regression)")
	}
	if want := r.compiler.instanceOwnedFuncs["Box[int].spawn"]; owner != want || want == "" {
		t.Fatalf("ramp %s owner = %q, want method owner %q", rampName, owner, want)
	}

	instIRs := r.InstanceIRs()
	instIR, ok := instIRs[owner]
	if !ok {
		t.Fatalf("no instance IR for %q", owner)
	}
	if extractDefine(instIR, rampName) == "" {
		t.Errorf("fast-path ramp %s must be DEFINED in instance IR %q", rampName, owner)
	}

	mainIR, _ := r.SplitModuleIRs()
	if extractDefine(mainIR, rampName) != "" {
		t.Errorf("fast-path ramp %s must NOT be defined in main IR (orphaned → T1222)", rampName)
	}
}
