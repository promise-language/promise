package codegen

import (
	"regexp"
	"strings"
	"testing"
)

// T0643: `c.enumCtorTemps` (B0267 — drop-flag allocas for inline enum
// constructors) used to be reset only in the main function-body entry points
// (`defineFunc`/`defineMethodFunc`/`defineGeneratorFunc`). Other entry points
// that reset the sibling temp slices (`stmtTemps`/`heapTemps`/`envTemps`) did
// NOT clear `enumCtorTemps`, and `compilerState` did not save/restore it. That
// left stale entries — allocas/drop-flags owned by a *different* LLVM function
// — visible while generating an inner/other function body. When the new body
// drained `enumCtorTemps` (at an enum var-decl / reassignment / return), it
// stored into a drop-flag alloca that belonged to the outer function, yielding
// a cross-function SSA reference that opt's verifier rejects.
//
// The fix nils `enumCtorTemps` at the start of: `wrapMainWithScheduler`,
// `compileTestCoroutine`, `defineEmbedGetter`; save+nil+restores it in:
// `genLambdaExpr`, `genGoCallExprViaBlock`, `genGoBlock`; and adds it to
// `compilerState` + `saveState`/`restoreState`. T0634 already removed the only
// known source-level trigger for the leak; these tests pin the convention so a
// future change cannot silently regress.
//
// Each test compiles a program in which the outer function generates an inline
// enum constructor (seeds `enumCtorTemps`) and a *separate* LLVM function (a
// lambda body, a `go` block ramp, a test coroutine) is generated next. We
// then walk *every* function in the resulting IR and assert that any drop-flag
// store (`store i1 X, i1* %Y`) and inline-enum pointer-temp store
// (`store i8* X, i8** %Y`) references an alloca defined within the same
// function. The pre-fix bug shape — a drain of `enumCtorTemps` from an outer
// function — surfaces as a store to a `%Y` not defined locally.

var (
	t0643FnHeaderRE   = regexp.MustCompile(`^define\s+[^@]*@(\"[^\"]+\"|[^\s(]+)\s*\(`)
	t0643DefHdr       = regexp.MustCompile(`^\s*%(\"[^\"]+\"|[A-Za-z0-9._]+)\s*=`)
	t0643LabelHdr     = regexp.MustCompile(`^([A-Za-z._][A-Za-z0-9._]*):`)
	t0643UseRE        = regexp.MustCompile(`%(\"[^\"]+\"|[A-Za-z0-9._]+)`)
	t0643DropStore    = regexp.MustCompile(`store\s+i1\s+[^,]+,\s*i1\*\s*%(\"[^\"]+\"|[A-Za-z0-9._]+)`)
	t0643PtrStoreI8   = regexp.MustCompile(`store\s+i8\*\s+[^,]+,\s*i8\*\*\s*%(\"[^\"]+\"|[A-Za-z0-9._]+)`)
	t0643AllocaRE     = regexp.MustCompile(`^\s*%(\"[^\"]+\"|[A-Za-z0-9._]+)\s*=\s*alloca\s+(\S+?)(?:,|$)`)
	t0643DropFlagLoad = regexp.MustCompile(`load\s+i1\s*,\s*i1\*\s*%(\"[^\"]+\"|[A-Za-z0-9._]+)`)
)

// allFnBodiesT0643 returns a map from function name (as it appears in the
// `define ... @name(` header) to the function's body text (from the `define`
// line through the closing `}`).
func allFnBodiesT0643(ir string) map[string]string {
	out := map[string]string{}
	lines := strings.Split(ir, "\n")
	var curName string
	var curBody []string
	flush := func() {
		if curName != "" {
			out[curName] = strings.Join(curBody, "\n")
		}
		curName = ""
		curBody = nil
	}
	for _, ln := range lines {
		if curName == "" {
			if m := t0643FnHeaderRE.FindStringSubmatch(ln); m != nil && strings.Contains(ln, "{") {
				curName = m[1]
				curBody = []string{ln}
			}
			continue
		}
		curBody = append(curBody, ln)
		if strings.HasPrefix(ln, "}") {
			flush()
		}
	}
	flush()
	return out
}

// collectFnLocalsT0643 returns the set of locally-defined SSA names within a
// function body (parameter names, SSA definitions, and block labels). The
// pre-fix bug surfaces as a store to a name NOT in this set.
func collectFnLocalsT0643(body string) map[string]bool {
	defs := map[string]bool{}
	// Function parameters live between the first `(` and its matching `)` on
	// the header line(s). Walk forward, tracking paren depth.
	if open := strings.Index(body, "("); open >= 0 {
		depth := 0
		end := -1
		for i := open; i < len(body); i++ {
			switch body[i] {
			case '(':
				depth++
			case ')':
				depth--
				if depth == 0 {
					end = i
				}
			}
			if end >= 0 {
				break
			}
		}
		if end > open {
			for _, p := range t0643UseRE.FindAllStringSubmatch(body[open:end], -1) {
				defs[p[1]] = true
			}
		}
	}
	for _, line := range strings.Split(body, "\n") {
		if m := t0643DefHdr.FindStringSubmatch(line); m != nil {
			defs[m[1]] = true
		}
		if m := t0643LabelHdr.FindStringSubmatch(line); m != nil {
			defs[m[1]] = true
		}
	}
	return defs
}

func isNumericNameT0643(s string) bool {
	if s == "" {
		return false
	}
	for _, ch := range s {
		if ch < '0' || ch > '9' {
			return false
		}
	}
	return true
}

// assertNoStaleEnumCtorStores walks every function in the IR and asserts that
// every drop-flag store and pointer-temp store references an alloca defined
// locally. The pre-fix bug surfaces here.
//
// Limitations: this only catches cross-function leaks where the leaked SSA
// name is unique (named or named-with-suffix). The drop-flag allocas at
// expr.go:5977-5978 are created without `SetName()` and thus auto-numbered
// (`%6`, `%7`, ...). A numeric-name collision (outer's `%6` colliding with
// the inner function's local `%6`) would not be caught structurally —
// detecting that requires opt's verifier or runtime semantics. The named
// shape is still useful coverage: any future change that names the alloca
// (or routes through a named entry like `enum.this`) becomes diagnosable.
func assertNoStaleEnumCtorStores(t *testing.T, ir string) {
	t.Helper()
	for fnName, body := range allFnBodiesT0643(ir) {
		defs := collectFnLocalsT0643(body)
		for _, re := range []*regexp.Regexp{t0643DropStore, t0643PtrStoreI8} {
			for _, m := range re.FindAllStringSubmatch(body, -1) {
				name := m[1]
				if isNumericNameT0643(name) {
					continue // numeric SSA names are auto-renumbered locally
				}
				if !defs[name] {
					t.Errorf("function %s: store to %%%s — alloca not defined locally (cross-function leak, T0643):\n  %s",
						fnName, name, m[0])
				}
			}
		}
	}
}

// collectFnAllocaTypesT0643 returns a map from local alloca-name to its element
// type (the `T` in `%X = alloca T`). LLVM 22 uses opaque pointers, which makes
// the IR text accept e.g. `load i1, i1* %X` even when `%X = alloca i8*` — opt's
// verifier won't catch the resulting type mismatch. The pre-fix bug shape in
// `wrapMainWithScheduler` surfaces exactly here: a leaked drain rendered with a
// numeric name that collides with a local alloca of a different type.
func collectFnAllocaTypesT0643(body string) map[string]string {
	out := map[string]string{}
	for _, line := range strings.Split(body, "\n") {
		if m := t0643AllocaRE.FindStringSubmatch(line); m != nil {
			out[m[1]] = m[2]
		}
	}
	return out
}

// assertDropFlagLoadsConsistent walks every function and asserts that every
// `load i1, i1* %X` references an alloca whose element type is `i1`. A drain
// that leaked from another function would reference a name whose local alloca
// has a different type (e.g. `alloca i8*`). Because LLVM 22 uses opaque
// pointers, opt's verifier accepts the cross-type load; this structural check
// is what catches the leak.
func assertDropFlagLoadsConsistent(t *testing.T, ir string) {
	t.Helper()
	for fnName, body := range allFnBodiesT0643(ir) {
		allocaTypes := collectFnAllocaTypesT0643(body)
		for _, m := range t0643DropFlagLoad.FindAllStringSubmatch(body, -1) {
			name := m[1]
			if ty, ok := allocaTypes[name]; ok && ty != "i1" {
				t.Errorf("function %s: load i1 from %%%s but local alloca is %q (cross-function leak via numeric-name collision, T0643):\n  %s",
					fnName, name, ty, m[0])
			}
		}
	}
}

// TestT0643_EnumCtorInLambdaBody — outer function generates an inline enum
// constructor (seeds `enumCtorTemps`), then defines a lambda whose body also
// constructs an enum value. The lambda body lives in a *different* LLVM
// function; the fix nils `enumCtorTemps` at lambda entry and restores at exit.
func TestT0643_EnumCtorInLambdaBody(t *testing.T) {
	ir := generateIR(t, `
		enum E { Some(int v), None }
		caller() {
			E outer = E.Some(1);
			f := || -> E { return E.Some(2); };
			E inner = f();
		}
		main() { caller(); }
	`)
	assertNoStaleEnumCtorStores(t, ir)
	assertDropFlagLoadsConsistent(t, ir)
}

// TestT0643_EnumCtorInGoBlock — outer function seeds `enumCtorTemps`, then a
// `go {...}` block body constructs another enum value. The go block becomes a
// separate coroutine function; the fix saves+nils+restores `enumCtorTemps`
// across `genGoBlock`.
func TestT0643_EnumCtorInGoBlock(t *testing.T) {
	ir := generateIR(t, `
		enum E { Some(int v), None }
		caller() {
			E outer = E.Some(1);
			go {
				E inner = E.Some(2);
			};
		}
		main() { caller(); }
	`)
	assertNoStaleEnumCtorStores(t, ir)
	assertDropFlagLoadsConsistent(t, ir)
}

// TestT0643_EnumCtorInGoCallBlock — same shape but using `go f()` (the
// `genGoCallExprViaBlock` entry point that wraps a *call expression* in a
// coroutine). Confirms `enumCtorTemps` is saved across this entry point too.
func TestT0643_EnumCtorInGoCallBlock(t *testing.T) {
	ir := generateIR(t, `
		enum E { Some(int v), None }
		worker() { E inner = E.Some(2); }
		caller() {
			E outer = E.Some(1);
			go worker();
		}
		main() { caller(); }
	`)
	assertNoStaleEnumCtorStores(t, ir)
	assertDropFlagLoadsConsistent(t, ir)
}

// TestT0643_EnumCtorAcrossTestCoroutines — two batch tests, each constructing
// an enum value. `compileTestCoroutine` was missing the `enumCtorTemps` reset;
// the fix adds it. Each test body is a separate coroutine function — a stale
// enum-ctor temp from t1's body would surface as a drop-flag store inside t2's
// coroutine.
func TestT0643_EnumCtorAcrossTestCoroutines(t *testing.T) {
	ir := generateIR(t, `
		enum E { Some(int v), None }
		t1() `+"`test"+` {
			E a = E.Some(1);
		}
		t2() `+"`test"+` {
			E b = E.Some(2);
		}
	`)
	assertNoStaleEnumCtorStores(t, ir)
	assertDropFlagLoadsConsistent(t, ir)
}

// TestT0643_EnumCtorInMainBody — exercises `wrapMainWithScheduler` (sched.go).
// A helper function `return E.Some("...")` terminates its block before the
// statement-end drain can clear `enumCtorTemps` (the drain at stmt.go:441-452
// only runs when the block is not terminated). After `defineFuncs` finishes,
// `enumCtorTemps` still holds the helper's drop-flag alloca. Then
// `wrapMainWithScheduler` regenerates main's body inside the `.goroutine.main`
// coroutine (via `c.genBlock(c.mainDecl.Body)`). Without the reset at
// sched.go:2246, the helper's stale entry would drain inside `.goroutine.main`
// — emitting `load i1, i1* %N` against a numeric-name that collides with a
// local alloca of a *different* element type (`i8*` for the local enum
// instance slot). Opaque pointers in LLVM 22 hide the type mismatch from opt's
// verifier, so we use `assertDropFlagLoadsConsistent` to detect it
// structurally. The enum is declared with a `string` payload so it has a
// real drop function (without one, `enumCtorTemps` would not be seeded at all).
func TestT0643_EnumCtorInMainBody(t *testing.T) {
	ir := generateIR(t, `
		enum E { Some(string s), None }
		helper() E { return E.Some("hello"); }
		main() {
			helper();
			E e = E.Some("world");
			print_line("hi");
		}
	`)
	assertNoStaleEnumCtorStores(t, ir)
	assertDropFlagLoadsConsistent(t, ir)
}

// TestT0643_EnumCtorAfterMoveLambdaCapture — a lambda capturing by move uses
// `saveState`/`restoreState` indirectly through monomorphization helpers when
// the captured variable has a generic type. The fix adds `enumCtorTemps` to
// `compilerState`. Verifies the IR remains well-formed across the boundary.
func TestT0643_EnumCtorAfterMoveLambdaCapture(t *testing.T) {
	ir := generateIR(t, `
		enum E { Some(int v), None }
		type Box[T] { T val; get_val(this) T { return this.val; } }
		caller() {
			E outer = E.Some(1);
			b := Box[int](val: 7);
			f := move || -> int { return b.get_val(); };
			x := f();
		}
		main() { caller(); }
	`)
	assertNoStaleEnumCtorStores(t, ir)
	assertDropFlagLoadsConsistent(t, ir)
}
