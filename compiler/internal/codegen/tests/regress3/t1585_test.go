package regress3

import (
	"strings"
	"testing"

	"github.com/antlr4-go/antlr/v4"
	"github.com/promise-language/promise/compiler/internal/ast"
	"github.com/promise-language/promise/compiler/internal/codegen"
	"github.com/promise-language/promise/compiler/internal/codegen/codegentest"
	"github.com/promise-language/promise/compiler/internal/parser"
	"github.com/promise-language/promise/compiler/internal/sema"
	"github.com/promise-language/promise/compiler/internal/types"
)

// T1585: a value-type element read out of a vector/array index and passed to a
// `~` (mut-ref) parameter must write back to the caller's element, not a
// discarded materialization temp. genMutRefArg previously had no *ast.IndexExpr
// case, so `v[0]` fell through to the materialize-into-a-fresh-alloca fallback:
// the callee mutated the temp, which was dropped at statement end and the write
// was silently lost. The fix passes the element slot pointer (genIndexSlotPtr),
// mirroring the value-type `~this` receiver path (T1358).

// A value-type Vector element passed to a `~` param is handed the element slot
// pointer directly — no per-arg spill temp.
func TestT1585_VectorIndexMutRefArgInPlace(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Plain { int n `+"`value"+`; }
		bump(Plain ~p) { p.n = 21; }
		caller() {
			Plain[] v = [Plain(n: 1)];
			bump(v[0]);
		}
		main() { caller(); }
	`)
	body := codegentest.ExtractFunction(ir, "__user.caller")
	if body == "" {
		t.Fatalf("expected @__user.caller in IR:\n%s", ir)
	}
	// The mut-ref arg is passed as a value-struct pointer (the element slot).
	if !strings.Contains(body, "call void @__user.bump(%promise_Plain_v*") {
		t.Fatalf("expected `bump` called with a %%promise_Plain_v* element slot ptr:\n%s", body)
	}
	// The slot pointer is a GEP into the vector's heap buffer (offset 16 past the
	// header), NOT a store into a fresh alloca — so there is no spill temp for the
	// argument. The materialize fallback would emit `alloca %promise_Plain_v`.
	if n := strings.Count(body, "alloca %promise_Plain_v"); n != 0 {
		t.Fatalf("expected 0 `alloca %%promise_Plain_v` (element slot passed in place, no spill), got %d:\n%s", n, body)
	}
}

// A value-type fixed-array element passed to a `~` param likewise passes the
// element slot pointer (GEP into the array storage), not a spill.
func TestT1585_ArrayIndexMutRefArgInPlace(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Plain { int n `+"`value"+`; }
		bump(Plain ~p) { p.n = 21; }
		caller() {
			Plain[2] a = [Plain(n: 1), Plain(n: 2)];
			bump(a[0]);
		}
		main() { caller(); }
	`)
	body := codegentest.ExtractFunction(ir, "__user.caller")
	if body == "" {
		t.Fatalf("expected @__user.caller in IR:\n%s", ir)
	}
	if !strings.Contains(body, "call void @__user.bump(%promise_Plain_v*") {
		t.Fatalf("expected `bump` called with a %%promise_Plain_v* element slot ptr:\n%s", body)
	}
	// The array storage is a single `[2 x %promise_Plain_v]` alloca; the element
	// slot is a GEP into it. No extra per-arg spill `alloca %promise_Plain_v`.
	if n := strings.Count(body, "alloca %promise_Plain_v\n"); n != 0 {
		t.Fatalf("expected 0 scalar `alloca %%promise_Plain_v` spill temps, got %d:\n%s", n, body)
	}
	if !strings.Contains(body, "alloca [2 x %promise_Plain_v]") {
		t.Fatalf("expected the array storage `alloca [2 x %%promise_Plain_v]`:\n%s", body)
	}
}

// A HEAP (non-value) user-type element does NOT get the in-place slot-ptr
// treatment: mutRefIndexArgAddressable's `named.IsValueType()` guard is false, so
// the arg stays on the materialize fallback. This is sound for field mutation
// because the temp holds the shared instance pointer (verified at runtime), and
// it exercises the false branch of the value-type guard. (Whole-value
// reassignment through such a param does NOT write back — tracked in T1587.)
func TestT1585_HeapElementMutRefArgUsesFallback(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Heap { int n; }
		bump(Heap ~p) { p.n = 21; }
		caller() {
			Heap[] v = [Heap(n: 1)];
			bump(v[0]);
		}
		main() { caller(); }
	`)
	body := codegentest.ExtractFunction(ir, "__user.caller")
	if body == "" {
		t.Fatalf("expected @__user.caller in IR:\n%s", ir)
	}
	// A heap user type is passed by its fat-pointer Value struct `{ i8*, i8* }`
	// (vtable + instance ptr), not a named value-struct slot. The fallback
	// materializes the loaded element into a per-arg spill `alloca { i8*, i8* }` and
	// passes that temp — NOT a GEP straight into the vector buffer as the value-type
	// slot path does. Field mutation still reaches the caller because the temp holds
	// the shared instance pointer.
	if !strings.Contains(body, "call void @__user.bump({ i8*, i8* }*") {
		t.Fatalf("expected `bump` called with a heap fat-pointer `{ i8*, i8* }*`:\n%s", body)
	}
	if !strings.Contains(body, "alloca { i8*, i8* }") {
		t.Fatalf("expected the heap element to use the materialize fallback (`alloca { i8*, i8* }` spill temp):\n%s", body)
	}
}

// T1585 (WASM regression): compileTestCoroutine must reset mutRefPtrs/mutRefTypes.
// A `~`-param helper (`bump(Vec2 ~v)`, or here `bump_first(Vec2[] ~v)`) compiled
// just before a test leaves a stale mutRefPtrs["v"]. genIdentExpr consults
// mutRefPtrs BEFORE locals, so a same-named local `v` in a later test body would
// load through the helper's stale pointer with the helper's LLVM type — emitting
// an ill-typed `load i8*, i8** %v` + `extractvalue i8* ...` that fails `opt` on
// WASM (extractvalue operand must be aggregate type). The test coroutine body is
// param-less, so clearing both maps is always correct.
func TestT1585_TestCoroutineResetsMutRefPtrs(t *testing.T) {
	ir := generateWasmTestIR(t, `
		type Vec2 { int x `+"`value"+`; int y `+"`value"+`; }
		bump(Vec2 ~v) { v.x = 40; }
		bump_first(Vec2[] ~v) { bump(v[0]); }
		local_regression() `+"`test"+` {
			v := Vec2(x: 3, y: 4);
			v.x = 10;
			assert(v.x == 10, "x");
		}
	`)
	// extractDefine (not extractFunction): the coroutine name also appears as a
	// call operand in the test runner, so a first-`@name(` match would latch onto
	// the caller instead of the definition.
	body := codegentest.ExtractDefine(ir, ".test_coro.local_regression")
	if body == "" {
		t.Fatalf("expected @.test_coro.local_regression coroutine in IR:\n%s", ir)
	}
	// The stale-mutRefPtr bug produced `load i8*, i8** %v` then `extractvalue i8*`.
	// The correct read loads the whole value struct and extractvalues that.
	if strings.Contains(body, "load i8*, i8** %v") {
		t.Fatalf("stale mutRefPtrs['v'] leaked into the test coroutine (ill-typed value load):\n%s", body)
	}
	if strings.Contains(body, "extractvalue i8* ") {
		t.Fatalf("ill-typed `extractvalue i8*` in test coroutine (mutRefPtrs not reset):\n%s", body)
	}
	if !strings.Contains(body, "extractvalue %promise_Vec2_v ") {
		t.Fatalf("expected well-typed `extractvalue %%promise_Vec2_v` value-field read:\n%s", body)
	}
}

// generateWasmTestIR compiles src as a WASM batch-test module (test bodies become
// coroutines) and returns the full module IR. Mirrors TestGenerateTestMainWasm*.
func generateWasmTestIR(t *testing.T, src string) string {
	t.Helper()
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
	return result.Module.String()
}
