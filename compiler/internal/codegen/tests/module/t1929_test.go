package module

import (
	"regexp"
	"strings"
	"testing"

	"github.com/promise-language/promise/compiler/internal/codegen/codegentest"
)

// T1929: a generic `native` std type instantiated only inside a *module* body —
// here MutexGuard[int], reached through a Mutex[int] local in a module method —
// used to get a per-instance MutexGuard[int].drop stub that nothing in codegen
// ever defines, and a promise_typeinfo_MutexGuard[int] pointing at it. That is
// an undefined symbol: ld.lld hid it by discarding the unreferenced typeinfo
// section under --gc-sections, lld-link reported it and the Windows gate went
// red. MutexGuard's drop is the single shared MutexGuard.drop symbol (T0561),
// so the typeinfo must name that instead.

// t1929Sources returns a module whose method body is the only place
// MutexGuard[int] is instantiated, plus the user file that calls it.
func t1929Sources() (modSrc, userSrc string) {
	return `
type Counter ` + "`public" + ` {
  int start ` + "`public" + `;
  bump(this, int by) int ` + "`public" + ` {
    Mutex[int] mx = Mutex[int](this.start);
    MutexGuard[int] g = mx.lock();
    g.borrow = g.borrow + by;
    return g.borrow;
  }
}
`, `
use m "./m";
main() {
  m.Counter c = m.Counter(start: 40);
  int n = c.bump(2);
}
`
}

// typeInfoFnPtr matches a function pointer baked into a typeinfo global's
// initializer: drop_fn_ptr is a `void (i8*)*` cast (B0226), clone_fn_ptr an
// `i8* (i8*)*` cast (T0387). The vtable field never matches — it is a cast of
// a global array, not of a function.
var typeInfoFnPtr = regexp.MustCompile(`bitcast \((?:void|i8\*) \(i8\*\)\* @"?([^"()]+?)"? to i8\*\)`)

// definedFuncs collects every function the IR actually defines a body for.
func definedFuncs(ir string) map[string]bool {
	defined := map[string]bool{}
	re := regexp.MustCompile(`(?m)^define [^@]*@"?([^"(]+)"?\(`)
	for _, m := range re.FindAllStringSubmatch(ir, -1) {
		defined[m[1]] = true
	}
	return defined
}

// assertTypeInfoFnPtrsDefined scans every typeinfo global in the IR and fails
// on any function pointer whose target has no body in this module.
func assertTypeInfoFnPtrsDefined(t *testing.T, ir string) {
	t.Helper()
	defined := definedFuncs(ir)
	checked := 0
	for _, line := range strings.Split(ir, "\n") {
		if !strings.HasPrefix(line, "@promise_typeinfo_") && !strings.HasPrefix(line, `@"promise_typeinfo_`) {
			continue
		}
		globalName := line[:strings.Index(line, " =")]
		for _, m := range typeInfoFnPtr.FindAllStringSubmatch(line, -1) {
			checked++
			if !defined[m[1]] {
				t.Errorf("%s: typeinfo function pointer targets body-less function %q", globalName, m[1])
			}
		}
	}
	if checked == 0 {
		t.Fatal("no typeinfo function pointers found — the check would pass vacuously")
	}
}

// The general invariant: no typeinfo may point at a function this module only
// declares. A declared-but-never-defined target is an undefined symbol whose
// diagnosis is left to the linker, and only some linkers give it.
func TestTypeInfoFnPtrsAreAllDefined(t *testing.T) {
	modSrc, userSrc := t1929Sources()
	ir := codegentest.GenerateIRWithModule(t, "m", modSrc, userSrc)
	assertTypeInfoFnPtrsDefined(t, ir)
}

// The other edge of verifyNoBodylessTypeInfoDrops: the Arc family (Ref/Weak/
// Mutex) keeps its per-instance drop stubs, which are legitimately body-less
// when the typeinfo is emitted and are filled in lazily by getOrCreate*Drop at
// the first drop site. A module-only Ref[int]/Weak[int] instantiation must
// neither trip the verifier (a false positive here is a compile abort) nor
// leave the lazily-defined drops body-less in the final IR.
func TestArcFamilyModuleOnlyInstanceDropsAreDefined(t *testing.T) {
	modSrc := `
type Holder ` + "`public" + ` {
  int start ` + "`public" + `;
  observe(this) int ` + "`public" + ` {
    Ref[int] r = Ref[int](this.start);
    Weak[int] _w = r.downgrade();
    return r.borrow;
  }
}
`
	userSrc := `
use m "./m";
main() {
  m.Holder h = m.Holder(start: 7);
  int n = h.observe();
}
`
	ir := codegentest.GenerateIRWithModule(t, "m", modSrc, userSrc)
	assertTypeInfoFnPtrsDefined(t, ir)

	defined := definedFuncs(ir)
	for _, drop := range []string{"Ref[int].drop", "Weak[int].drop"} {
		if !defined[drop] {
			t.Errorf("expected the lazily-defined %s to have a body once compilation finishes", drop)
		}
	}
}

// And the specific shape the bug produced: the guard instance's typeinfo names
// the shared origin drop, never a per-instance one.
func TestMonoTypeInfoUsesSharedMutexGuardDrop(t *testing.T) {
	modSrc, userSrc := t1929Sources()
	ir := codegentest.GenerateIRWithModule(t, "m", modSrc, userSrc)

	if strings.Contains(ir, `@"MutexGuard[int].drop"`) {
		t.Error("codegen emitted a per-instance MutexGuard[int].drop; MutexGuard is dropped " +
			"through the shared @MutexGuard.drop symbol (T0561), so nothing defines it")
	}

	var tiLine string
	for _, line := range strings.Split(ir, "\n") {
		if strings.HasPrefix(line, `@"promise_typeinfo_MutexGuard[int]"`) {
			tiLine = line
			break
		}
	}
	if tiLine == "" {
		t.Fatal("expected a typeinfo global for the module-instantiated MutexGuard[int]")
	}
	if !strings.Contains(tiLine, "@MutexGuard.drop") {
		t.Errorf("promise_typeinfo_MutexGuard[int] should carry the shared origin drop, got: %s", tiLine)
	}
}
