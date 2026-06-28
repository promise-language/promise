package codegen

import (
	"strings"
	"testing"

	"github.com/llir/llvm/ir"
	irtypes "github.com/llir/llvm/ir/types"
)

// T1089: classifier that decides which hand-written runtime helpers get grouped
// into the synthetic __runtime module (and excluded from the B0314 check).
func TestIsRuntimeFunc(t *testing.T) {
	// Every reserved runtime prefix must match.
	runtime := []string{
		"promise_sched_init", "promise_string_concat",
		"__promise_iter_cleanup", "__promise_main_body",
		"pal_alloc", "pal_mutex_lock",
		"llvm.coro.begin", "strlen", "memcpy", "memset",
	}
	for _, n := range runtime {
		if !isRuntimeFunc(n) {
			t.Errorf("isRuntimeFunc(%q) = false, want true", n)
		}
	}
	// User/module/instance functions and near-miss prefixes must not match —
	// a false positive would strip a user body into the shared runtime module.
	notRuntime := []string{
		"main", "Box__int.foo", "__mod_std_print_line", "user_func",
		"", "promise", "pal", "promisestring", "mem",
	}
	for _, n := range notRuntime {
		if isRuntimeFunc(n) {
			t.Errorf("isRuntimeFunc(%q) = true, want false", n)
		}
	}
}

// T1089: tagRuntimeFuncs branch coverage on a hand-built module — every skip
// condition (declaration-only, user main body, non-runtime, already module- or
// instance-owned) and the tag-to-__runtime path.
func TestTagRuntimeFuncs(t *testing.T) {
	c := &Compiler{
		module:             ir.NewModule(),
		moduleOwnedFuncs:   map[string]string{},
		instanceOwnedFuncs: map[string]string{},
	}
	addFn := func(name string, withBody bool) {
		fn := c.module.NewFunc(name, irtypes.Void)
		if withBody {
			fn.NewBlock("entry").NewRet(nil)
		}
	}

	addFn("promise_sched_init", true)    // runtime helper w/ body → tagged
	addFn("pal_alloc", false)            // declaration only → skipped
	addFn(userMainBodyFunc, true)        // holds user code → excluded
	addFn("main", true)                  // non-runtime → skipped
	addFn("promise_string_concat", true) // already module-owned → unchanged
	c.moduleOwnedFuncs["promise_string_concat"] = "std"
	addFn("promise_box_helper", true) // instance-owned → kept on instance
	c.instanceOwnedFuncs["promise_box_helper"] = "Box__int"

	c.tagRuntimeFuncs()

	if got := c.moduleOwnedFuncs["promise_sched_init"]; got != runtimeModuleName {
		t.Errorf("promise_sched_init owner = %q, want %q", got, runtimeModuleName)
	}
	if _, ok := c.moduleOwnedFuncs["pal_alloc"]; ok {
		t.Error("declaration-only pal_alloc must not be tagged")
	}
	if _, ok := c.moduleOwnedFuncs[userMainBodyFunc]; ok {
		t.Errorf("%s must stay in main IR (holds user code)", userMainBodyFunc)
	}
	if _, ok := c.moduleOwnedFuncs["main"]; ok {
		t.Error("non-runtime main must not be tagged")
	}
	if got := c.moduleOwnedFuncs["promise_string_concat"]; got != "std" {
		t.Errorf("module-owned helper owner = %q, want unchanged %q", got, "std")
	}
	if got, ok := c.moduleOwnedFuncs["promise_box_helper"]; ok {
		t.Errorf("instance-owned helper must not be moved to a module, got owner %q", got)
	}
}

// T1089: end-to-end placement after the split — runtime helpers must be
// *defined* in the __runtime IR and only *declared* in main, while the
// program-specific @__promise_main_body must be defined in main (NOT in
// __runtime), or the user's main code would be compiled into the shared,
// content-cached runtime module and reused across unrelated programs.
func TestRuntimeModuleSplitPlacement(t *testing.T) {
	r := compileResult(t, `main!() { print_line("hi"); }`)
	mainIR, mods := r.SplitModuleIRs()

	rt, ok := mods[runtimeModuleName]
	if !ok {
		t.Fatalf("expected %q module IR", runtimeModuleName)
	}

	// Runtime helpers: defined in __runtime, declared (not defined) in main.
	for _, fn := range []string{"promise_sched_init", "pal_alloc"} {
		if extractDefine(rt, fn) == "" {
			t.Errorf("%s should be defined in __runtime IR", fn)
		}
		if extractDefine(mainIR, fn) != "" {
			t.Errorf("%s should NOT be defined in main IR (only declared)", fn)
		}
		if !strings.Contains(mainIR, "declare ") || !strings.Contains(mainIR, "@"+fn) {
			t.Errorf("%s should be declared in main IR", fn)
		}
	}

	// User main body: defined in main, never in __runtime.
	if extractDefine(mainIR, userMainBodyFunc) == "" {
		t.Errorf("%s must be defined in main IR", userMainBodyFunc)
	}
	if extractDefine(rt, userMainBodyFunc) != "" {
		t.Errorf("%s must NOT leak into the shared __runtime module", userMainBodyFunc)
	}
}
