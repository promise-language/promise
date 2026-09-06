package module

import (
	"sort"
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

// T1458: compileModules used to declare *and* define one module at a time, so
// std's monomorphized bodies were generated while a later module still had no
// method stubs. Each shape below panicked in codegen at a different dispatch
// site with a c.funcs miss; each now emits a direct call to the module-owned
// function from inside the std mono body that needs it.
//
// GenerateIRWithModule sets ModuleOrder = ["std", "./dispmod"] — exactly the
// ordering that made std lose, since std is compiled first.
//
// The shape T1458 was *filed* for — scan[T: Parse] reaching a module type's
// `factory parse` — is absent: T1740 had already fixed it with the
// forwardDeclareModuleMethod fallback in expr_methodcall.go, and
// tests/modules/module_cross_module_parse_test.pr covers it. Verified against
// the pre-fix compiler: that shape passes there, every shape below panics. The
// sites below have no such fallback, which is why the ordering had to be fixed
// structurally rather than one dispatch site at a time.

// expr_literal.go: "undeclared method Item.format for interpolation".
func TestT1458ModuleTypeFormatInStdGeneric(t *testing.T) {
	ir := codegentest.GenerateIRWithModule(t, "dispmod", `
		type Item `+"`public"+` {
		  int id `+"`public"+`;
		  format!(this, Writer ~w) `+"`public"+` {
		    w.write(this.id.to_string().bytes())?^;
		  }
		}
	`, `
		use dispmod "./dispmod";
		main() {
		  dispmod.Item[] items = [dispmod.Item(id: 1)];
		  print_line(items.to_string());
		}
	`)
	assertCallInside(t, ir, "Vector[Item].format", "call { i1, i8* } @__mod_dispmod_Item.format(")
}

// stmt_drop_emit.go: "drop method not in vtable for Key" — Key's synthesized
// drop is reached from inside Set/Map's monomorphization before its hash is, so
// that is the site that panics first. The assertions pin the hash and ==
// dispatches instead, since those are what the container actually needs from a
// key type and what the fix has to reach past the drop.
func TestT1458ModuleTypeHashInStdSet(t *testing.T) {
	ir := codegentest.GenerateIRWithModule(t, "dispmod", `
		type Key is Hashable `+"`public"+` {
		  string name `+"`public"+`;
		  get hash int `+"`public"+` { return this.name.hash; }
		  ==(this, Key other) bool `+"`public"+` { return this.name == other.name; }
		}
	`, `
		use dispmod "./dispmod";
		main() {
		  Set[dispmod.Key] s = Set[dispmod.Key]();
		  s.add(dispmod.Key(name: "a"));
		}
	`)
	assertCallInside(t, ir, "Map[Key, bool].[]=", "call i64 @__mod_dispmod_Key.hash(")
	assertCallInside(t, ir, "Map[Key, bool].contains", `call i1 @"__mod_dispmod_Key.=="(`)
}

// The same dispatches, reached through a map key rather than a set element.
func TestT1458ModuleTypeHashAsMapKey(t *testing.T) {
	ir := codegentest.GenerateIRWithModule(t, "dispmod", `
		type Key is Hashable `+"`public"+` {
		  string name `+"`public"+`;
		  get hash int `+"`public"+` { return this.name.hash; }
		  ==(this, Key other) bool `+"`public"+` { return this.name == other.name; }
		}
	`, `
		use dispmod "./dispmod";
		main() {
		  map[dispmod.Key, int] m = map[dispmod.Key, int]();
		  m[dispmod.Key(name: "a")] = 1;
		}
	`)
	assertCallInside(t, ir, "Map[Key, int].[]=", "call i64 @__mod_dispmod_Key.hash(")
}

// expr_operator.go: "undeclared operator method Score.<".
func TestT1458ModuleTypeOrderedInStdSort(t *testing.T) {
	ir := codegentest.GenerateIRWithModule(t, "dispmod", `
		type Score is Ordered `+"`public"+` {
		  int points `+"`public"+`;
		  <(this, Score other) bool `+"`public"+` { return this.points < other.points; }
		  ==(this, Score other) bool `+"`public"+` { return this.points == other.points; }
		}
	`, `
		use dispmod "./dispmod";
		main() {
		  dispmod.Score[] v = [dispmod.Score(points: 2), dispmod.Score(points: 1)];
		  dispmod.Score[] s = sort(move v);
		}
	`)
	assertCallInside(t, ir, "sort[Score]", `call i1 @"__mod_dispmod_Score.<"(`)
	// Ordered's `>` default is synthesized per-concrete and must reach the
	// module's `<` as well.
	assertCallInside(t, ir, "Score.>", `call i1 @"__mod_dispmod_Score.<"(`)
}

// stmt_drop_emit.go: "drop method not in vtable for Heapy", reached via the
// silent dropFunc == nil in stmt_drop_register.go. Vector[Heapy]'s element
// cleanup is monomorphized inside std and must reach the module's synthesized
// drop, or the string and vector fields leak.
func TestT1458ModuleTypeSynthesizedDropInStdGeneric(t *testing.T) {
	ir := codegentest.GenerateIRWithModule(t, "dispmod", `
		type Heapy `+"`public"+` {
		  string label `+"`public"+`;
		  int[] values `+"`public"+`;
		}
	`, `
		use dispmod "./dispmod";
		main() {
		  dispmod.Heapy[] v = [dispmod.Heapy(label: "a", values: [1, 2])];
		}
	`)
	codegentest.AssertContains(t, ir, "define void @__mod_dispmod_Heapy.drop(")
	assertCallInside(t, ir, "Vector[Heapy].[:]", "call void @__mod_dispmod_Heapy.drop(")
	// The synthesized drop must also land in the typeinfo's drop_fn_ptr slot
	// (field 1), since runtime drop dispatch for a heap value goes through it.
	codegentest.AssertContains(t, ir,
		`@promise_typeinfo___mod_dispmod_Heapy = constant { i8*, i8*, i8*, i32, i32 } `+
			`{ i8* null, i8* bitcast (void (i8*)* @__mod_dispmod_Heapy.drop to i8*),`)
}

// assertCallInside checks that the named function's body contains the given
// call. The enclosing function matters: these calls must land in the std
// monomorphization, which is what codegen could not emit before the fix.
func assertCallInside(t *testing.T, ir, fnName, call string) {
	t.Helper()
	body := codegentest.ExtractDefine(ir, fnName)
	if body == "" {
		t.Fatalf("function @%q not defined in IR", fnName)
	}
	if !strings.Contains(body, call) {
		t.Errorf("expected @%q to contain %q; body:\n%s", fnName, call, body)
	}
}

// --- The general form: one module's generic reaching a later module's type ---
//
// Everything above has std as the losing module, because std is what every
// program imports. But nothing in the old ordering was specific to std: any
// module declared before another lost the same way. These use two user modules
// so the failing caller is ordinary Promise code, and they reach the *getter*
// dispatch site (expr_field.go), which none of the std shapes above touch.

// labeledBound is the structural bound the genmod fixtures dispatch through. It
// is declared in genmod, satisfied structurally by typemod's Tag — the two
// modules never name each other, which is what keeps typemod ordered second.
const labeledBound = "type Labeled `structural `public {\n  get label string `abstract;\n}\n"

// tagModule is the later-ordered module: a plain type whose getter is what the
// earlier module's monomorphized body has to call. `shout` exists only to give
// typemod a string literal of its own, which the counter test looks for.
const tagModule = "type Tag `public {\n" +
	"  string text `public;\n" +
	"  get label string `public { return this.text; }\n" +
	"  get shout string `public { return \"typemod-shout\"; }\n" +
	"}\n"

// A generic *function* in genmod, monomorphized for typemod's Tag.
// Panicked: "codegen: undeclared getter Tag.label".
func TestT1458ModuleGenericFuncReachesLaterModuleGetter(t *testing.T) {
	ir := codegentest.GenerateIRWithTwoModules(t,
		"genmod", labeledBound+`
			describe[T: Labeled](T item) string `+"`public"+` {
			  return "<{item.label}>";
			}
		`,
		"typemod", tagModule,
		`
			use genmod "./genmod";
			use typemod "./typemod";
			main() {
			  print_line(genmod.describe[typemod.Tag](typemod.Tag(text: "x")));
			}
		`)
	assertCallInside(t, ir, "describe[Tag]", "call i8* @__mod_typemod_Tag.label(")
}

// The same reach from a generic *type*'s method body rather than a free
// function — a different mono path (defineMonoMethods, not defineMonoFuncs),
// and the one that carries a module-owned generic container over another
// module's element type. Panicked with the same undeclared getter.
func TestT1458ModuleGenericTypeReachesLaterModuleGetter(t *testing.T) {
	ir := codegentest.GenerateIRWithTwoModules(t,
		"genmod", labeledBound+`
			type Boxed[T: Labeled] `+"`public"+` {
			  T item `+"`public"+`;
			  get shown string `+"`public"+` { return this.item.label; }
			}
		`,
		"typemod", tagModule,
		`
			use genmod "./genmod";
			use typemod "./typemod";
			main() {
			  genmod.Boxed[typemod.Tag] b = genmod.Boxed[typemod.Tag](item: typemod.Tag(text: "x"));
			  print_line(b.shown);
			}
		`)
	assertCallInside(t, ir, "Boxed[Tag].shown", "call i8* @__mod_typemod_Tag.label(")
}

// compileTwoModuleDispatch compiles the generic-function shape above with an
// explicit ModuleOrder, or with none at all when order is nil — the fallback
// branch in compileModules that iterates c.info.ModuleInfos, a Go map, in
// whatever order the runtime picks. Returns the CompileResult so callers can
// look at the split module IRs as well as the whole-program IR.
func compileTwoModuleDispatch(t *testing.T, order []string) *codegen.CompileResult {
	t.Helper()

	genInfo, genScope := codegentest.ParseModuleSource(t, "genmod", labeledBound+`
		describe[T: Labeled](T item) string `+"`public"+` { return "<{item.label}>"; }
		banner() string `+"`public"+` { return "genmod-banner"; }
	`)
	typeInfo, typeScope := codegentest.ParseModuleSource(t, "typemod", tagModule)
	stdModInfo, stdScope := codegentest.GetCodegenStdModInfo()

	userSrc := `
		use genmod "./genmod";
		use typemod "./typemod";
		main() {
		  print_line(genmod.describe[typemod.Tag](typemod.Tag(text: "x")));
		  print_line(genmod.banner());
		}
	`
	input := antlr.NewInputStream(userSrc)
	lexer := parser.NewPromiseLexer(input)
	lexer.RemoveErrorListeners()
	stream := antlr.NewCommonTokenStream(lexer, antlr.TokenDefaultChannel)
	p := parser.NewPromiseParser(stream)
	p.RemoveErrorListeners()
	userFile, buildErrs := ast.Build("test.pr", p.CompilationUnit())
	if len(buildErrs) > 0 {
		t.Fatalf("user AST build errors: %v", buildErrs)
	}
	userFile.Uses = append([]*ast.UseDecl{{Alias: "_", CatalogName: "std"}}, userFile.Uses...)

	info, semaErrs := sema.CheckWithModules(userFile, map[string]*types.Scope{
		"std":       stdScope,
		"./genmod":  genScope,
		"./typemod": typeScope,
	})
	if len(semaErrs) > 0 {
		t.Fatalf("sema errors: %v", semaErrs)
	}
	info.ModuleInfos = map[string]*sema.ModuleInfo{
		"std":       stdModInfo,
		"./genmod":  genInfo,
		"./typemod": typeInfo,
	}
	info.ModuleOrder = order

	return codegen.Compile(userFile, info, "")
}

// Without a topological order, compileModules falls back to iterating the
// ModuleInfos map — so the modules arrive in a random order on every run. The
// two-sweep structure is what makes that harmless: declaring every module
// before defining any of them removes the ordering dependency entirely, rather
// than trading one fixed order for another. The old code panicked here whenever
// the map happened to yield genmod first.
func TestT1458ModulesWithoutTopologicalOrderStillCompile(t *testing.T) {
	// Repeat so a lucky map iteration order cannot pass this by accident.
	for i := 0; i < 8; i++ {
		ir := compileTwoModuleDispatch(t, nil).Module.String()
		assertCallInside(t, ir, "describe[Tag]", "call i8* @__mod_typemod_Tag.label(")
	}
}

// The per-module string-constant counters are reset at the start of each
// module's *define* sweep, not its declare sweep. Before the split the two were
// one pass and the placement did not matter; now it does, because body
// generation — the only thing that emits these constants — all happens in the
// second sweep. Had the reset stayed with declare, the define sweep would run
// with no reset between modules, so every module after the first would number
// its constants from wherever the previous module's bodies left off: a module's
// IR text, and with it its .bc cache key, would then move whenever an unrelated
// earlier module changed.
func TestT1458ModuleStringConstantsRestartPerModule(t *testing.T) {
	ir := compileTwoModuleDispatch(t, []string{"std", "./genmod", "./typemod"}).Module.String()

	// Each module's own constants start at .0 under its own prefix.
	for _, prefix := range []string{"__mod_std", "__mod_genmod", "__mod_typemod"} {
		if !strings.Contains(ir, "@.str."+prefix+".0 ") {
			t.Errorf("module %s has no @.str.%s.0 — its counter did not restart", prefix, prefix)
		}
	}
	// And no module string constant is emitted twice.
	seen := map[string]int{}
	for _, line := range strings.Split(ir, "\n") {
		if !strings.HasPrefix(line, "@.str.__mod_") {
			continue
		}
		seen[line[:strings.Index(line, " ")]]++
	}
	for name, count := range seen {
		if count > 1 {
			t.Errorf("%s defined %d times — module string counters collided", name, count)
		}
	}
}

// Ownership must follow the declaring module, not the referencing one. Tag.label
// is reached from inside genmod's monomorphized body, but it belongs to typemod:
// declareModulePhase runs under typemod's context via enterModuleContext, so
// moduleOwnedFuncs sends the body to typemod's .bc. Were it registered under
// whichever module happened to touch it first, the two modules' cached objects
// would each be wrong on their own.
func TestT1458ModuleMethodStaysOwnedByItsOwnModule(t *testing.T) {
	mainIR, moduleIRs := compileTwoModuleDispatch(t, []string{"std", "./genmod", "./typemod"}).SplitModuleIRs()

	const def = "define i8* @__mod_typemod_Tag.label("
	if !strings.Contains(moduleIRs["typemod"], def) {
		t.Errorf("typemod's IR does not define Tag.label; module IRs present: %v", moduleKeys(moduleIRs))
	}
	if strings.Contains(moduleIRs["genmod"], def) {
		t.Error("genmod's IR defines Tag.label — ownership followed the caller, not the declaring module")
	}
	if strings.Contains(mainIR, def) {
		t.Error("main IR defines Tag.label — the method was never claimed by its module")
	}
}

func moduleKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
