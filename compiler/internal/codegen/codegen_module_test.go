package codegen

import (
	"strings"
	"testing"

	antlr "github.com/antlr4-go/antlr/v4"
	"github.com/promise-language/promise/compiler/internal/ast"
	"github.com/promise-language/promise/compiler/internal/parser"
	"github.com/promise-language/promise/compiler/internal/sema"
	"github.com/promise-language/promise/compiler/internal/types"
)

// --- wasm_import codegen tests (T0035) ---

func TestWasmImportAttributes(t *testing.T) {
	ir := generateIRForTarget(t, `
		_fd_write(int fd) int `+"`extern(\"fd_write\") `wasm_import(\"wasi_snapshot_preview1\", \"fd_write\") `target(wasm)"+`;
		main() {}
	`, "wasm32-wasi")
	assertContains(t, ir, `"wasm-import-module"="wasi_snapshot_preview1"`)
	assertContains(t, ir, `"wasm-import-name"="fd_write"`)
	// WASM imports use direct return and params for primitives (not sret/i8*)
	assertContains(t, ir, "declare i64 @fd_write(i64 %fd)")
}

// When a single wasm32-wasi module emits BOTH the realtime clock (wallclock,
// clockid 0) and the monotonic clock (the scheduler deadline / nanotime path,
// clockid 1), the shared emitWasiClockTimeGet helper must still dedup the
// clock_time_get import to a single declaration while emitting both clockid
// arguments — the two paths differ only in the clockid constant (T1067).
func TestWallclockAndMonotonicShareOneClockImport(t *testing.T) {
	ir := generateIRForTarget(t, `
		_wallclock() int `+"`extern(\"promise_wallclock\")"+`;
		worker() int { return 7; }
		main() {
			int _x = _wallclock();
			Task[int] t = go worker();
			int _v = <-t;
		}
	`, "wasm32-wasi")
	if n := strings.Count(ir, "declare i32 @clock_time_get"); n != 1 {
		t.Errorf("clock_time_get must be declared exactly once across the realtime and monotonic paths, got %d:\n%s", n, ir)
	}
	// Realtime path uses clockid 0, monotonic path uses clockid 1 — both flow
	// through the one shared import.
	assertContains(t, ir, "call i32 @clock_time_get(i32 0,")
	assertContains(t, ir, "call i32 @clock_time_get(i32 1,")
}

// T0962: A main-file method taking a value-type parameter from another module
// (here std's Duration) must declare its stub and define its body with the SAME
// LLVM type. The stub is declared before the module is compiled, so the layout
// isn't in c.layouts yet; resolveType must compute it on demand rather than fall
// back to the generic {i8*,i8*} userValueType (which mismatched the body's
// %promise_Duration_v and crashed codegen with a store-type error).
func TestCrossModuleValueTypeParamLayout(t *testing.T) {
	ir := generateIR(t, `
		type Foo `+"`public"+` {
			int n;
			bar(Duration d) int => this.n + d.nanos;
		}
		main() { f := Foo(n: 1); int _x = f.bar(Duration.from_nanos(2)); }
	`)
	// Both the declaration and definition agree on the value-struct param type.
	assertContains(t, ir, "define i64 @Foo.bar(i8* %this, %promise_Duration_v %d)")
}

func TestWasmImportIgnoredOnNative(t *testing.T) {
	// On native target, wasm_import annotations should not produce IR attributes.
	// The function itself is filtered out by `target(wasm), so it won't appear at all.
	ir := generateIR(t, `
		_fd_write(int fd) int `+"`extern(\"fd_write\") `wasm_import(\"wasi_snapshot_preview1\", \"fd_write\") `target(wasm)"+`;
		main() {}
	`)
	assertNotContains(t, ir, "wasm-import-module")
}

func TestWasmExternWithoutImportStillSret(t *testing.T) {
	ir := generateIRForTarget(t, `
		_internal(int x) int `+"`extern(\"test_internal\") `target(wasm)"+`;
		main() {}
	`, "wasm32-wasi")
	// Externs without wasm_import annotation keep sret ABI on WASM
	assertContains(t, ir, "declare void @test_internal(i8* %sret, i8* %x)")
}

func TestUseVarDeclFailableInitAutoPropagate(t *testing.T) {
	// GitHub #3: a bare failable call as a `use` initializer must auto-propagate
	// and unwrap to the ok value before the store. Previously codegen panicked
	// storing the failable-result aggregate into the unwrapped slot.
	ir := generateIR(t, `
		type Res {
			int id;
			make!(int id) Res `+"`"+`factory { return Res(id: id); }
			close!(~this) {}
		}
		build!() int {
			use r := Res.make(7);
			return r.id;
		}
		main() {}
	`)
	// The failable factory returns the {tag, value, errptr} result aggregate ...
	assertContains(t, ir, "call { i1, { i8*, i8* }, i8* } @Res.make(")
	// ... which is unwrapped on the auto-propagation ok path ...
	assertContains(t, ir, "auto.ok")
	// ... and the unwrapped Res value (not the raw aggregate) is stored into the
	// `use` slot %r.
	assertContains(t, ir, "{ i8*, i8* }* %r")
}

func TestUseBoundBothCloseAndDropSuppressesUserDrop(t *testing.T) {
	// T0967 / language-design §16.4: for a `use`-bound value whose type defines
	// BOTH close() and a user drop(), only close() runs at scope exit — the user
	// drop() body is suppressed (use takes precedence) to avoid double-cleanup.
	// The instance is still freed (the heap memory is not user logic).
	ir := generateIR(t, `
		type Conn {
			int id;
			close!(~this) {}
			drop(~this) {}
		}
		main() {
			use c := Conn(id: 1);
		}
	`)
	// close() is dispatched on the use binding's scope exit.
	assertContains(t, ir, "call { i1, i8* } @Conn.close(")
	// The heap instance is reclaimed via pal_free on the close-free path.
	assertContains(t, ir, "close.free")
	// Crucially, the user drop() is NOT called on the close path. Scope the
	// assertion to main's body — the typeinfo drop$wrap (B0226) legitimately
	// calls @Conn.drop, but that is RTTI dispatch, not the use/close path.
	mainIR := extractFunc(ir, "main")
	if mainIR == "" {
		t.Fatal("could not extract main function from IR")
	}
	assertNotContains(t, mainIR, "call void @Conn.drop(")
}

func TestUseBoundCloseDropInsideMethodRestoresThis(t *testing.T) {
	// T0967: when the use-bound close+drop value lives inside a METHOD body, the
	// close-free suppression path (emitInstanceFieldDropsAndFree) temporarily
	// rebinds locals["this"] to the closing instance to drop its (droppable)
	// fields, then must restore the method's real `this`. A `string` field forces
	// the field-drop branch; the method reads `this.marker` AFTER the use scope so
	// the restored `this` must still point at Holder. This exercises the hadThis
	// save/restore branch that free-function call sites never reach.
	ir := generateIR(t, `
		type Res {
			string name;
			close!(~this) {}
			drop(~this) {}
		}
		type Holder {
			int marker;
			run(this) int {
				{
					use r := Res(name: "x");
				}
				return this.marker;
			}
		}
		main() {
			h := Holder(marker: 1);
			x := h.run();
		}
	`)
	run := extractFunc(ir, "Holder.run")
	if run == "" {
		t.Fatal("could not extract Holder.run from IR")
	}
	// close() ran on the use binding inside the method...
	assertContains(t, run, "@Res.close(")
	// ...and its droppable string field was reclaimed inline (suppression path),
	assertContains(t, run, "@promise_string_drop")
	// ...but the user Res.drop() body was NOT invoked (use takes precedence).
	assertNotContains(t, run, "call void @Res.drop(")
}

// T0340: same fix must apply for module-qualified generic calls. The
// module callee may not be visible via lookupFunc, so genModuleGenericFuncCall
// also checks c.info.Types[e.Callee] as a fallback — without it, the drop
// flag clear was missed on module-imported `~` callees.
func TestT0340_ModuleGenericFuncMutRefArgClearsDropFlag(t *testing.T) {
	ir := generateIRWithModule(t, "mylib",
		"consume[T](T move x) `public { }",
		`
		use mylib "./mylib";
		main() {
			s := "hello";
			mylib.consume[string](s);
		}
	`)
	assertContainsMatch(t, ir,
		`store i1 false, i1\* %s\.dropflag\s*\n\s*call void @"consume\[string\]"`)
}

// T1160: a module-qualified call (`mod.make_adder(5)`) has a MemberExpr callee
// whose "receiver" names a module, not a value type. closureResultMayAliasCallInput
// must recognize the module-name ident (via resolveModuleName) and return false so
// the fresh closure env IS tracked and freed at statement end. The e2e counterpart
// (tests/modules/module_closures_test.pr test_discarded_module_closure_result) tests
// the same thing at runtime + leak-detection; this Go unit test pins the IR shape to
// cover the module-receiver branch in the Go coverage profile.
func TestClosureModuleQualifiedCallResultTracked(t *testing.T) {
	ir := generateIRWithModule(t, "cblib",
		`make_adder(int x) () -> int `+"`"+`public { return move || -> x + 1; }`,
		`use cblib "./cblib";
		 discard_fresh() { cblib.make_adder(5); }
		 main() {}`,
	)
	// Module receiver → resolveModuleName hit → return false → result is tracked.
	assertContains(t, extractFunction(ir, "__user.discard_fresh"), "env.tmp.drop")
}

func TestHostTargetTripleInModule(t *testing.T) {
	ir := generateIR(t, `main() {}`)
	triple := HostTargetTriple()
	assertContains(t, ir, "target triple = \""+triple+"\"")
}

func TestModuleCallQualified(t *testing.T) {
	ir := generateIRWithModule(t, "mylib",
		"compute() int `public { return 42; }",
		`
		use mylib "./mylib";
		main() {
			x := mylib.compute();
		}
		`,
	)
	// Module function should have module-mangled name
	assertContains(t, ir, "define i64 @__mod_mylib_compute")
	// Call should use the mangled name
	assertContains(t, ir, "call i64 @__mod_mylib_compute")
}

func TestModuleCallDoesNotCollideWithUser(t *testing.T) {
	ir := generateIRWithModule(t, "mylib",
		"compute() int `public { return 42; }",
		`
		use mylib "./mylib";
		compute() int { return 99; }
		main() {
			x := compute();
			y := mylib.compute();
		}
		`,
	)
	// Both functions exist with different names
	assertContains(t, ir, "define i64 @__user.compute")
	assertContains(t, ir, "define i64 @__mod_mylib_compute")
	// User call goes to @__user.compute, module call to @__mod_mylib_compute
	assertContains(t, ir, "call i64 @__user.compute()")
	assertContains(t, ir, "call i64 @__mod_mylib_compute()")
}

func TestModuleTypeConstructor(t *testing.T) {
	ir := generateIRWithModule(t, "geo",
		`type Point `+"`public"+` { int x; int y; }`,
		`
		use geo "./geo";
		main() {
			geo.Point p = geo.Point(x: 1, y: 2);
		}
		`,
	)
	// Module type should have layout and constructor
	assertContains(t, ir, "Point")
	// Constructor stores field values
	assertContains(t, ir, "store i64 1")
	assertContains(t, ir, "store i64 2")
}

func TestModuleMethodCall(t *testing.T) {
	ir := generateIRWithModule(t, "counter",
		`type Counter `+"`public"+` {
			int value;
			get_value(this) int `+"`public"+` { return this.value; }
		}`,
		`
		use counter "./counter";
		main() {
			counter.Counter c = counter.Counter(value: 42);
			int v = c.get_value();
		}
		`,
	)
	// Module method should be defined with module-prefixed name
	assertContains(t, ir, "define i64 @__mod_counter_Counter.get_value")
	// Call should use the module-prefixed method
	assertContains(t, ir, "call i64 @__mod_counter_Counter.get_value")
}

func TestModuleGlobImportCall(t *testing.T) {
	ir := generateIRWithModule(t, "helpers",
		"greet() int `public { return 1; }",
		`
		use _ "./helpers";
		main() {
			int x = greet();
		}
		`,
	)
	// Glob-imported function should resolve to module-prefixed IR name
	assertContains(t, ir, "define i64 @__mod_helpers_greet")
	assertContains(t, ir, "call i64 @__mod_helpers_greet")
}

func TestModuleFuncWithParams(t *testing.T) {
	ir := generateIRWithModule(t, "math",
		"add(int a, int b) int `public { return a + b; }",
		`
		use math "./math";
		main() {
			int x = math.add(3, 4);
		}
		`,
	)
	assertContains(t, ir, "define i64 @__mod_math_add(i64 %a, i64 %b)")
	assertContains(t, ir, "call i64 @__mod_math_add(i64 3, i64 4)")
}

func TestModuleVoidFunc(t *testing.T) {
	ir := generateIRWithModule(t, "logger",
		"noop() `public {}",
		`
		use logger "./logger";
		main() {
			logger.noop();
		}
		`,
	)
	assertContains(t, ir, "define void @__mod_logger_noop()")
	assertContains(t, ir, "call void @__mod_logger_noop()")
}

func TestModuleFailableFunc(t *testing.T) {
	ir := generateIRWithModule(t, "parser",
		`
		parse!(int x) int `+"`public"+` {
			return x;
		}
		`,
		`
		use parser "./parser";
		main!() {
			int v = parser.parse(10)?^;
		}
		`,
	)
	// Failable function should return a result struct { i1, i64, i8* }
	assertContains(t, ir, "define { i1, i64, i8* } @__mod_parser_parse(i64 %x)")
	assertContains(t, ir, "call { i1, i64, i8* } @__mod_parser_parse(i64 10)")
}

func TestModuleExternFunc(t *testing.T) {
	ir := generateIRWithModule(t, "ffi",
		`
		cfunc(int x) `+"`public `extern(\"test_cfunc\")"+`;
		wrapper(int x) int `+"`public"+` { return x; }
		`,
		`
		use ffi "./ffi";
		main() {
			ffi.cfunc(1);
			int y = ffi.wrapper(2);
		}
		`,
	)
	// Extern should be declared (not defined)
	assertContains(t, ir, "declare void @test_cfunc")
	// Wrapper should be a module function
	assertContains(t, ir, "define i64 @__mod_ffi_wrapper")
}

func TestModuleEnumVariant(t *testing.T) {
	ir := generateIRWithModule(t, "shapes",
		`
		enum Shape `+"`public"+` {
			Circle(int radius),
			Rect(int w, int h),
		}
		`,
		`
		use shapes "./shapes";
		main() {
			shapes.Shape s = shapes.Shape.Circle(radius: 5);
		}
		`,
	)
	// Enum layout should exist
	assertContains(t, ir, "Shape")
	// Variant constructor stores the tag and payload
	assertContains(t, ir, "store i64 5")
}

func TestModuleGlobImportType(t *testing.T) {
	ir := generateIRWithModule(t, "models",
		`
		type Item `+"`public"+` {
			int id;
			get_id(this) int `+"`public"+` { return this.id; }
		}
		`,
		`
		use _ "./models";
		main() {
			Item it = Item(id: 7);
			int v = it.get_id();
		}
		`,
	)
	// Type layout and method should use module-prefixed names
	assertContains(t, ir, "define i64 @__mod_models_Item.get_id")
	assertContains(t, ir, "call i64 @__mod_models_Item.get_id")
	// Constructor stores the field value
	assertContains(t, ir, "store i64 7")
}

func TestModuleGlobImportMultipleSymbols(t *testing.T) {
	ir := generateIRWithModule(t, "utils",
		`
		foo() int `+"`public"+` { return 1; }
		bar() int `+"`public"+` { return 2; }
		`,
		`
		use _ "./utils";
		main() {
			int a = foo();
			int b = bar();
		}
		`,
	)
	// Both glob-imported functions should resolve to module-prefixed names
	assertContains(t, ir, "define i64 @__mod_utils_foo")
	assertContains(t, ir, "define i64 @__mod_utils_bar")
	assertContains(t, ir, "call i64 @__mod_utils_foo()")
	assertContains(t, ir, "call i64 @__mod_utils_bar()")
}

func TestMultipleModules(t *testing.T) {
	ir := generateIRWithTwoModules(t,
		"alpha", "get_a() int `public { return 1; }",
		"beta", "get_b() int `public { return 2; }",
		`
		use alpha "./alpha";
		use beta "./beta";
		main() {
			int a = alpha.get_a();
			int b = beta.get_b();
		}
		`,
	)
	assertContains(t, ir, "define i64 @__mod_alpha_get_a")
	assertContains(t, ir, "define i64 @__mod_beta_get_b")
	assertContains(t, ir, "call i64 @__mod_alpha_get_a")
	assertContains(t, ir, "call i64 @__mod_beta_get_b")
}

func TestModuleTypeGlobalsPrefixed(t *testing.T) {
	ir := generateIRWithModule(t, "shapes",
		`type Circle `+"`public"+` {
			int radius;
			area(this) int `+"`public"+` { return this.radius; }
		}`,
		`
		use shapes "./shapes";
		main() {
			shapes.Circle c = shapes.Circle(radius: 5);
			int a = c.area();
		}
		`,
	)
	// RTTI/typeinfo globals should be prefixed with __mod_shapes_
	assertContains(t, ir, "@promise_typeinfo___mod_shapes_Circle")
	// std library types (e.g., int) should NOT have module prefix
	assertNotContains(t, ir, "__mod_shapes_int")
}

func TestModuleSplitModuleIRs(t *testing.T) {
	mod1Info, mod1Scope := parseModuleSource(t, "alpha", "get_a() int `public { return 1; }")
	mod2Info, mod2Scope := parseModuleSource(t, "beta", "get_b() int `public { return 2; }")
	stdModInfo, stdScope := getCodegenStdModInfo()

	userSrc := `
		use alpha "./alpha";
		use beta "./beta";
		main() {
			int a = alpha.get_a();
			int b = beta.get_b();
		}
	`
	userInput := antlr.NewInputStream(userSrc)
	userLexer := parser.NewPromiseLexer(userInput)
	userLexer.RemoveErrorListeners()
	userStream := antlr.NewCommonTokenStream(userLexer, antlr.TokenDefaultChannel)
	userP := parser.NewPromiseParser(userStream)
	userP.RemoveErrorListeners()
	userTree := userP.CompilationUnit()
	userFile, buildErrs := ast.Build("test.pr", userTree)
	if len(buildErrs) > 0 {
		t.Fatalf("user AST build errors: %v", buildErrs)
	}

	// Inject use std as _
	stdUse := &ast.UseDecl{Alias: "_", CatalogName: "std"}
	userFile.Uses = append([]*ast.UseDecl{stdUse}, userFile.Uses...)

	moduleScopes := map[string]*types.Scope{
		"std":     stdScope,
		"./alpha": mod1Scope,
		"./beta":  mod2Scope,
	}
	info, semaErrs := sema.CheckWithModules(userFile, moduleScopes)
	if len(semaErrs) > 0 {
		t.Fatalf("sema errors: %v", semaErrs)
	}
	info.ModuleInfos = map[string]*sema.ModuleInfo{
		"std":     stdModInfo,
		"./alpha": mod1Info,
		"./beta":  mod2Info,
	}
	info.ModuleOrder = []string{"std", "./alpha", "./beta"}

	result := Compile(userFile, info, "")
	mainIR, moduleIRs := result.SplitModuleIRs()

	// Should produce separate IRs for std, alpha, beta, and the synthetic
	// __runtime module (T1089: codegen-emitted runtime helpers).
	if len(moduleIRs) != 4 {
		t.Fatalf("expected 4 module IRs (std, alpha, beta, __runtime), got %d", len(moduleIRs))
	}
	if _, ok := moduleIRs[runtimeModuleName]; !ok {
		t.Fatalf("expected %q in moduleIRs", runtimeModuleName)
	}
	alphaIR, ok := moduleIRs["alpha"]
	if !ok {
		t.Fatal("expected 'alpha' in moduleIRs")
	}
	betaIR, ok := moduleIRs["beta"]
	if !ok {
		t.Fatal("expected 'beta' in moduleIRs")
	}

	// alpha IR: has alpha's function body, beta's function is a declaration
	assertContains(t, alphaIR, "define i64 @__mod_alpha_get_a")
	assertNotContains(t, alphaIR, "define i64 @__mod_beta_get_b")

	// beta IR: has beta's function body, alpha's function is a declaration
	assertContains(t, betaIR, "define i64 @__mod_beta_get_b")
	assertNotContains(t, betaIR, "define i64 @__mod_alpha_get_a")

	// main IR: all module function bodies are declarations, not definitions
	assertNotContains(t, mainIR, "define i64 @__mod_alpha_get_a")
	assertNotContains(t, mainIR, "define i64 @__mod_beta_get_b")
	// main IR should still declare (not define) the module functions
	assertContains(t, mainIR, "declare i64 @__mod_alpha_get_a")
	assertContains(t, mainIR, "declare i64 @__mod_beta_get_b")
}

func TestModuleIRPrefixUsedForIR(t *testing.T) {
	// Verify that when the user alias differs from the IRPrefix,
	// the IR uses the IRPrefix (derived from GlobalIdentity), not the alias.
	mod1Info, mod1Scope := parseModuleSource(t, "myalias", "helper() int `public { return 42; }")
	// Override GlobalIdentity and IRPrefix to simulate a remote module
	mod1Info.GlobalIdentity = "github.com/alice/mylib"
	mod1Info.IRPrefix = "github_com_alice_mylib_abc123"

	stdModInfo, stdScope := getCodegenStdModInfo()

	userSrc := `
		use myalias "./myalias";
		main() {
			int x = myalias.helper();
		}
	`
	userInput := antlr.NewInputStream(userSrc)
	userLexer := parser.NewPromiseLexer(userInput)
	userLexer.RemoveErrorListeners()
	userStream := antlr.NewCommonTokenStream(userLexer, antlr.TokenDefaultChannel)
	userP := parser.NewPromiseParser(userStream)
	userP.RemoveErrorListeners()
	userTree := userP.CompilationUnit()
	userFile, buildErrs := ast.Build("test.pr", userTree)
	if len(buildErrs) > 0 {
		t.Fatalf("user AST build errors: %v", buildErrs)
	}

	// Inject use std as _
	stdUse := &ast.UseDecl{Alias: "_", CatalogName: "std"}
	userFile.Uses = append([]*ast.UseDecl{stdUse}, userFile.Uses...)

	moduleScopes := map[string]*types.Scope{
		"std":       stdScope,
		"./myalias": mod1Scope,
	}
	info, semaErrs := sema.CheckWithModules(userFile, moduleScopes)
	if len(semaErrs) > 0 {
		t.Fatalf("sema errors: %v", semaErrs)
	}
	info.ModuleInfos = map[string]*sema.ModuleInfo{
		"std":       stdModInfo,
		"./myalias": mod1Info,
	}
	info.ModuleOrder = []string{"std", "./myalias"}

	result := Compile(userFile, info, "")
	ir := result.Module.String()

	// IR should use IRPrefix, not the alias "myalias"
	assertContains(t, ir, "define i64 @__mod_github_com_alice_mylib_abc123_helper")
	assertNotContains(t, ir, "__mod_myalias_")
}

func TestCatalogModuleCallQualified(t *testing.T) {
	ir := generateIRWithCatalogModule(t, "json",
		"parse() int `public { return 1; }",
		`
		use json;
		main() {
			int x = json.parse();
		}
		`,
	)
	assertContains(t, ir, "define i64 @__mod_json_parse")
	assertContains(t, ir, "call i64 @__mod_json_parse")
}

func TestCatalogModuleAliasedCall(t *testing.T) {
	// Regression test: aliased catalog imports must use the catalog name
	// as IR prefix, not the alias.
	ir := generateIRWithCatalogModule(t, "json",
		"parse() int `public { return 1; }",
		`
		use json as j;
		main() {
			int x = j.parse();
		}
		`,
	)
	assertContains(t, ir, "define i64 @__mod_json_parse")
	assertContains(t, ir, "call i64 @__mod_json_parse")
	assertNotContains(t, ir, "__mod_j_")
}

func TestCatalogModuleTypeQualified(t *testing.T) {
	ir := generateIRWithCatalogModule(t, "json",
		"type Value `public { int x; }",
		`
		use json;
		main() {
			v := json.Value(x: 42);
		}
		`,
	)
	assertContains(t, ir, "promise_typeinfo___mod_json_Value")
}

func TestCatalogModuleGlobImport(t *testing.T) {
	ir := generateIRWithCatalogModule(t, "json",
		"parse() int `public { return 1; }",
		`
		use json as _;
		main() {
			int x = parse();
		}
		`,
	)
	assertContains(t, ir, "define i64 @__mod_json_parse")
	assertContains(t, ir, "call i64 @__mod_json_parse")
}

// --- Stage 8m: use bindings ---

func TestUseVarDeclBasic(t *testing.T) {
	ir := generateIR(t, `
		type Resource {
			int id;
			close() { }
		}
		main() {
			use r := Resource(id: 1);
			int x = r.id;
		}
	`)
	// use binding should generate a close() call at end of scope
	assertContains(t, ir, "call void @Resource.close")
}

func TestUseVarDeclMultiple(t *testing.T) {
	ir := generateIR(t, `
		type Resource {
			int id;
			close() { }
		}
		main() {
			use a := Resource(id: 1);
			use b := Resource(id: 2);
			int x = a.id + b.id;
		}
	`)
	// Both resources should have close() calls
	assertContains(t, ir, "call void @Resource.close")
	// Count that there are at least 2 close calls
	count := strings.Count(ir, "call void @Resource.close")
	if count < 2 {
		t.Errorf("expected at least 2 close calls, got %d\nIR:\n%s", count, ir)
	}
}

func TestUseVarDeclWithReturn(t *testing.T) {
	ir := generateIR(t, `
		type Resource {
			int id;
			close() { }
		}
		make_resource() int {
			use r := Resource(id: 42);
			return r.id;
		}
		main() {
			int v = make_resource();
		}
	`)
	// close() should appear before the return instruction in make_resource
	assertContains(t, ir, "call void @Resource.close")
	assertContains(t, ir, "define i64 @__user.make_resource")
}

func TestUseVarDeclInNestedBlock(t *testing.T) {
	ir := generateIR(t, `
		type Resource {
			int id;
			close() { }
		}
		main() {
			use outer := Resource(id: 1);
			if true {
				use inner := Resource(id: 2);
				int x = inner.id;
			}
			int y = outer.id;
		}
	`)
	// Both outer and inner resources should generate close() calls
	count := strings.Count(ir, "call void @Resource.close")
	if count < 2 {
		t.Errorf("expected at least 2 close calls (inner + outer), got %d\nIR:\n%s", count, ir)
	}
}

// T0106: use binding frees the instance after close()
func TestUseVarDeclFreesInstance(t *testing.T) {
	ir := generateIR(t, `
		type Resource {
			int id;
			close() { }
		}
		main() {
			use r := Resource(id: 1);
			int x = r.id;
		}
	`)
	// After close(), the instance should be freed via pal_free
	assertContains(t, ir, "call void @Resource.close")
	assertContains(t, ir, "close.free")
	assertContains(t, ir, "call void @pal_free(")
}

// --- Coverage gap tests ---

// Virtual close dispatch through vtable (type has children → needs vtable)
func TestUseVarVirtualCloseDispatch(t *testing.T) {
	ir := generateIR(t, `
		type Base {
			int id;
			close() { }
		}
		type Child is Base {
			close() { }
		}
		main() {
			use r := Base(id: 1);
			int x = r.id;
		}
	`)
	// Base has children → needs vtable → virtual close dispatch
	assertContains(t, ir, "@promise_vtable_Base")
}

// Virtual close with failable close() method (parent type with child)
func TestUseVarVirtualCloseDispatchFailable(t *testing.T) {
	ir := generateIR(t, `
		type Conn {
			int fd;
			close!() { }
		}
		type TcpConn is Conn {
			close!() { }
		}
		main() {
			use c := Conn(fd: 3);
			int x = c.fd;
		}
	`)
	// Conn has children → needs vtable → virtual close dispatch
	assertContains(t, ir, "@promise_vtable_Conn")
}

func TestCompileWithCacheNilEqualToCompile(t *testing.T) {
	// CompileWithCache with nil cachedInstances must produce the same IR as Compile.
	file, info := parseWithStd(t, boxWithGetMethod+`
		main() { b := Box[int](value: 1); }
	`)
	r1 := Compile(file, info, "")
	r2 := CompileWithCache(file, info, "", nil)
	if r1.Module.String() != r2.Module.String() {
		t.Error("CompileWithCache(nil) produced different IR than Compile")
	}
}

func TestCompileWithCacheSkipsInstanceBody(t *testing.T) {
	// When Box[int] is listed as cached, its method body must not be generated
	// (so it won't appear in InstanceIRs).
	file, info := parseWithStd(t, boxWithGetMethod+`
		main() {
			b := Box[int](value: 42);
			int x = b.get();
		}
	`)

	// Full compile: Box[int] must appear in InstanceIRs (body was generated).
	rFull := Compile(file, info, "")
	fullIRs := rFull.InstanceIRs()
	if _, ok := fullIRs["Box[int]"]; !ok {
		t.Skipf("Box[int] not in InstanceIRs on full compile; keys: %v", mapKeys(fullIRs))
	}

	// Cached compile: Box[int] body must be skipped → not in InstanceIRs.
	rCached := CompileWithCache(file, info, "", map[string]bool{"Box[int]": true})
	cachedIRs := rCached.InstanceIRs()
	if _, ok := cachedIRs["Box[int]"]; ok {
		t.Error("Box[int] should not appear in InstanceIRs when marked as cached")
	}
}

func TestCompileWithCacheOnlySkipsCachedInstances(t *testing.T) {
	// Marking Box[int] as cached must not affect Box[string].
	file, info := parseWithStd(t, boxWithGetMethod+`
		main() {
			a := Box[int](value: 1);
			b := Box[string](value: "hi");
			int x = a.get();
			string y = b.get();
		}
	`)

	rCached := CompileWithCache(file, info, "", map[string]bool{"Box[int]": true})
	cachedIRs := rCached.InstanceIRs()

	// Box[int] was cached → no body, not in InstanceIRs
	if _, ok := cachedIRs["Box[int]"]; ok {
		t.Error("Box[int] should not appear in InstanceIRs when marked as cached")
	}
	// Box[string] was NOT cached → body generated, must be in InstanceIRs
	if _, ok := cachedIRs["Box[string]"]; !ok {
		t.Errorf("Box[string] should appear in InstanceIRs (not cached); keys: %v", mapKeys(cachedIRs))
	}
}

func TestInstanceOwnedFuncsTrackedEvenWhenCached(t *testing.T) {
	// instanceOwnedFuncs tagging must happen regardless of cachedInstances,
	// so that SplitModuleIRs can strip instance-owned functions from module/main IRs.
	file, info := parseWithStd(t, boxWithGetMethod+`
		main() {
			b := Box[int](value: 42);
			int x = b.get();
		}
	`)
	r := CompileWithCache(file, info, "", map[string]bool{"Box[int]": true})
	c := r.compiler

	foundBoxInt := false
	for _, instName := range c.instanceOwnedFuncs {
		if instName == "Box[int]" {
			foundBoxInt = true
			break
		}
	}
	if !foundBoxInt {
		t.Errorf("Box[int] not in instanceOwnedFuncs even when cached; map = %v", c.instanceOwnedFuncs)
	}
}

// --- Module-level getter/setter codegen tests ---

func TestModuleLevelGetterCodegen(t *testing.T) {
	ir := generateIR(t, `
		get answer int { return 42; }
		main() { int v = answer; }
	`)
	// Getter should generate a zero-arg function returning i64
	assertContains(t, ir, "define i64 @__user.answer()")
	// Usage should call the getter (no args)
	assertContains(t, ir, "call i64 @__user.answer()")
}

func TestModuleLevelSetterCodegen(t *testing.T) {
	ir := generateIR(t, `
		get counter int { return 0; }
		set counter(int value) {}
		main() { counter = 42; }
	`)
	// Setter stored as counter$set, takes one i64 param
	assertContains(t, ir, "define void @__user.counter$set(i64")
	// Assignment should call the setter
	assertContains(t, ir, "call void @__user.counter$set(i64")
}

func TestModuleLevelCompoundAssignCodegen(t *testing.T) {
	ir := generateIR(t, `
		get counter int { return 0; }
		set counter(int value) {}
		main() { counter += 5; }
	`)
	// Should call getter then setter
	assertContains(t, ir, "call i64 @__user.counter()")
	assertContains(t, ir, "call void @__user.counter$set(i64")
}

func TestModuleLevelGetterDistinctFromSetter(t *testing.T) {
	ir := generateIR(t, `
		get val int { return 0; }
		set val(int v) {}
		main() {
			int x = val;
			val = 10;
		}
	`)
	// Getter and setter should be distinct LLVM functions
	assertContains(t, ir, "define i64 @__user.val()")
	assertContains(t, ir, "define void @__user.val$set(i64")
}

func TestModuleSplitStringConstantsPreserved(t *testing.T) {
	// When SplitModuleIRs() splits the IR, string constants (private globals)
	// must remain as definitions in the module IR, not be stripped to extern.
	// This is the core B0005 fix: each .bc is self-contained for strings.
	result := compileResultWithModule(t, "mymod",
		`greet(string name) string `+"`public"+` { return "Hello, {name}!"; }`,
		`
		use mymod "./mymod";
		main() { string s = mymod.greet("World"); }
		`,
	)
	_, moduleIRs := result.SplitModuleIRs()

	modIR, ok := moduleIRs["mymod"]
	if !ok {
		t.Fatal("expected 'mymod' in moduleIRs")
	}

	// Module IR must contain at least one string constant as a private definition.
	foundPrivateStr := false
	for _, line := range strings.Split(modIR, "\n") {
		if strings.HasPrefix(line, "@.str.") && strings.Contains(line, "private constant") {
			foundPrivateStr = true
		}
		// No string constant should be an extern declaration
		if strings.HasPrefix(line, "@.str.") && !strings.Contains(line, "private") &&
			strings.Contains(line, " = ") && strings.Contains(line, "constant") {
			t.Errorf("module IR has non-private string constant: %s", line)
		}
	}
	if !foundPrivateStr {
		t.Error("module IR must contain at least one private string constant definition")
	}
}

func TestModuleSplitNonPrivateGlobalsStripped(t *testing.T) {
	// In module IR, non-private globals (vtables, RTTI) must be converted to
	// extern declarations. Only private globals (strings) stay as definitions.
	result := compileResultWithModule(t, "shapes",
		`
		type Shape `+"`public"+` {
			string label;
			info(this) string { return "shape: {this.label}"; }
		}
		`,
		`
		use shapes "./shapes";
		main() {
			s := shapes.Shape(label: "box");
			print_line(s.info());
		}
		`,
	)
	_, moduleIRs := result.SplitModuleIRs()

	modIR, ok := moduleIRs["shapes"]
	if !ok {
		t.Fatal("expected 'shapes' in moduleIRs")
	}

	// Vtable/typeinfo globals should be extern declarations in module IR
	for _, line := range strings.Split(modIR, "\n") {
		trimmed := strings.TrimSpace(line)
		// Check vtable globals are NOT defined (they live in main IR)
		if strings.HasPrefix(trimmed, "@promise_vtable_") &&
			strings.Contains(trimmed, " = ") &&
			!strings.Contains(trimmed, "external") {
			// Allow if it's an extern declaration (no init = "external" or just "declare")
			if strings.Contains(trimmed, "constant") || strings.Contains(trimmed, "global") {
				t.Errorf("module IR should not define vtable global (should be extern): %s", trimmed)
			}
		}
	}
}

// TestCrossModulePropagation verifies that instances of types from module B
// created inside module A are propagated to B. Map[string, int] works even
// though Slot[string, int] (a generic enum in std) is only reachable through
// Map's fields.
func TestCrossModulePropagation(t *testing.T) {
	ir := generateIR(t, `
		main() {
			m := {"a": 1};
		}
	`)

	// Map layout must exist
	assertContains(t, ir, "Map[string, int]")
}

// TestCrossModuleGenericMethodCallsGenericFunc verifies B0344: when a generic
// method in module B calls a generic function in module A, the func instance
// (which contains TypeParams in B's sema) gets resolved via the concrete method
// instance from user code.
func TestCrossModuleGenericMethodCallsGenericFunc(t *testing.T) {
	ir := generateIRWithDependentModules(t,
		"helper",
		`transform[T](T val) T `+"`public"+` { return val; }`,
		"caller",
		`use helper "./helper";
		type Wrapper[V] `+"`public"+` {
			V _value;
			apply[T](this, T extra) T `+"`public"+` {
				return helper.transform[T](extra);
			}
		}`,
		`
		use caller "./caller";
		main() {
			caller.Wrapper[string] w = caller.Wrapper[string](_value: "hi");
			int result = w.apply[int](42);
		}
		`,
	)

	// The mono func instance transform[int] must be defined
	assertContains(t, ir, `define i64 @"transform[int]"`)
	// The mono method apply[int] must be defined for Wrapper[string]
	assertContains(t, ir, `@"Wrapper[string].apply[int]"`)
}

// TestCrossModuleGenericMethodCallsGenericFuncMultipleInstances verifies that
// cross-module resolution handles multiple concrete instantiations (B0344).
func TestCrossModuleGenericMethodCallsGenericFuncMultipleInstances(t *testing.T) {
	ir := generateIRWithDependentModules(t,
		"conv",
		`identity[T](T val) T `+"`public"+` { return val; }`,
		"box",
		`use conv "./conv";
		type Box[V] `+"`public"+` {
			V _data;
			unwrap[T](this, T fallback) T `+"`public"+` {
				return conv.identity[T](fallback);
			}
		}`,
		`
		use box "./box";
		main() {
			box.Box[int] b1 = box.Box[int](_data: 1);
			int r1 = b1.unwrap[int](10);
			box.Box[string] b2 = box.Box[string](_data: "x");
			string r2 = b2.unwrap[string]("y");
		}
		`,
	)

	// Both concrete instantiations must exist
	assertContains(t, ir, `define i64 @"identity[int]"`)
	assertContains(t, ir, `@"identity[string]"`)
	assertContains(t, ir, `@"Box[int].unwrap[int]"`)
	assertContains(t, ir, `@"Box[string].unwrap[string]"`)
}

// T0436 Issue 2 (defineModuleTypeMethods path): a borrowed-this method on a
// module type, declared after a ~this method on the same module type, must dup
// rather than clear the caller's flag. To expose stale thisRecvIsOwned from a
// prior ~this method, the test puts a generic type with a ~this method in a
// FIRST module. After mod1 is compiled, defineMonoMethods for that generic's
// ~this method leaves thisRecvIsOwned=true. mod2 is compiled next, and without
// the fix, defineModuleTypeMethods inherits the stale flag.
func TestT0436ModuleTypeBorrowedThisAfterOwnedDups(t *testing.T) {
	ir := generateIRWithTwoModules(t,
		"t0436a",
		`
		type GenBox[T] `+"`public"+` {
			T item;
			consume(~this) `+"`public"+` {}
		}
		`,
		"t0436b",
		`
		type Box2 `+"`public"+` { int n; drop(~this) {} }
		type Holder2 `+"`public"+` {
			Box2? data;
			get_n(this) int `+"`public"+` {
				b := this.data!;
				return b.n;
			}
		}
		`,
		`
		use t0436a "./t0436a";
		use t0436b "./t0436b";
		main() {
			// Force a generic instantiation in mod1 so its ~this consume() compiles
			// via defineMonoMethods → defineMethodFunc, leaving thisRecvIsOwned=true.
			gb := t0436a.GenBox[int](item: 1);
			h := t0436b.Holder2(data: t0436b.Box2(n: 3));
			n := h.get_n();
		}
		`,
	)
	// get_n is in mod2, compiled via defineModuleTypeMethods AFTER mod1.
	// Must dup the heap value (pal_alloc + memcpy), not clear the caller's flag.
	getNFn := extractFunction(ir, "__mod_t0436b_Holder2.get_n")
	if getNFn == "" {
		t.Fatal("expected __mod_t0436b_Holder2.get_n in IR")
	}
	assertContains(t, getNFn, "call i8* @pal_alloc")
	assertContains(t, getNFn, "call void @llvm.memcpy")
}
