package ownership

import (
	"strings"
	"testing"

	antlr "github.com/antlr4-go/antlr/v4"
	"github.com/promise-language/promise/compiler/internal/ast"
	"github.com/promise-language/promise/compiler/internal/parser"
	"github.com/promise-language/promise/compiler/internal/sema"
	"github.com/promise-language/promise/compiler/internal/types"
)

// T1303: a generic type defined in an imported module whose field-escaping body
// double-frees for a drop-bearing instantiation. The module body is never
// visited by the user unit's ownership pass (it isn't in c.file), so the inline
// checkGenericFieldMove path never fires. checkImportedGenericBodies re-runs the
// ownership body-check over each instantiated generic module type with the user
// unit's concrete instances injected, so the same field-move verdict rejects the
// unsafe escape where the concrete instantiation is visible (the user unit).

// parseFileT1303 parses source into a *ast.File, failing the test on parse errors.
func parseFileT1303(t *testing.T, name, src string) *ast.File {
	t.Helper()
	input := antlr.NewInputStream(src)
	lexer := parser.NewPromiseLexer(input)
	lexer.RemoveErrorListeners()
	stream := antlr.NewCommonTokenStream(lexer, antlr.TokenDefaultChannel)
	p := parser.NewPromiseParser(stream)
	p.RemoveErrorListeners()
	tree := p.CompilationUnit()
	file, buildErrs := ast.Build(name, tree)
	if len(buildErrs) > 0 {
		t.Fatalf("%s AST build errors: %v", name, buildErrs)
	}
	return file
}

// checkOwnershipCrossModule sema-checks a module source, exposes its exported
// scope to the user sema under the given path key, wires the module's File +
// SemaInfo into info.ModuleInfos, then runs the ownership Check over the user
// unit. Returns the combined sema + ownership errors.
func checkOwnershipCrossModule(t *testing.T, pathKey, moduleSrc, userSrc string) []error {
	t.Helper()

	// Sema-check the module (with std available).
	modFile := parseFileT1303(t, "boxmod.pr", moduleSrc)
	modFile.Uses = append([]*ast.UseDecl{{Alias: "_", CatalogName: "std"}}, modFile.Uses...)
	modInfo, modErrs := sema.CheckWithModules(modFile, map[string]*types.Scope{"std": getOwnerStdScope()})
	if len(modErrs) > 0 {
		t.Fatalf("module sema errors: %v", modErrs)
	}
	modScope := sema.ExportedScope(modInfo, modFile)

	// Sema-check the user unit with std + the module scope (keyed by path).
	userFile := parseFileT1303(t, "main.pr", userSrc)
	userFile.Uses = append([]*ast.UseDecl{{Alias: "_", CatalogName: "std"}}, userFile.Uses...)
	scopes := map[string]*types.Scope{"std": getOwnerStdScope(), pathKey: modScope}
	info, semaErrs := sema.CheckWithModules(userFile, scopes)
	allErrs := append([]error(nil), semaErrs...)
	if len(semaErrs) != 0 {
		return allErrs
	}
	info.ModuleInfos = map[string]*sema.ModuleInfo{
		pathKey: {Name: "boxmod", File: modFile, SemaInfo: modInfo},
	}
	allErrs = append(allErrs, Check(userFile, info)...)
	return allErrs
}

// --- The item's repro: getter returning `V?` out of a droppable owner ---

func TestT1303_CrossModuleGenericGetterDroppableFieldRejected(t *testing.T) {
	mod := "type Box[V] `public { V? _v; get val V? `public { return this._v; } }"
	user := `
use boxmod "./boxmod";
type Res { string name; drop(~this) {} }
make() {
    boxmod.Box[Res] b = boxmod.Box[Res](_v: none);
}
`
	errs := checkOwnershipCrossModule(t, "./boxmod", mod, user)
	expectOwnerError(t, errs, "cannot move field '_v' out of 'Box[Res]'")
}

// --- Constructor-field-init escape proves the full traversal is reused ---

func TestT1303_CrossModuleGenericCtorInitDroppableRejected(t *testing.T) {
	mod := `
type Holder[V] ` + "`" + `public { V _item; }
type Box[V] ` + "`" + `public { V _v; wrap(this) Holder[V] ` + "`" + `public { return Holder[V](_item: this._v); } }
`
	user := `
use boxmod "./boxmod";
type Res { int id; drop(~this) {} }
make() {
    boxmod.Box[Res] b = boxmod.Box[Res](_v: Res(id: 1));
}
`
	errs := checkOwnershipCrossModule(t, "./boxmod", mod, user)
	expectOwnerError(t, errs, "cannot move field '_v' out of 'Box[Res]'")
}

// --- Negative: Copy instantiation stays allowed cross-module ---

func TestT1303_CrossModuleGenericCopyInstantiationAllowed(t *testing.T) {
	mod := "type Box[V] `public { V? _v; get val V? `public { return this._v; } }"
	user := `
use boxmod "./boxmod";
make() {
    boxmod.Box[int] b = boxmod.Box[int](_v: none);
}
`
	errs := checkOwnershipCrossModule(t, "./boxmod", mod, user)
	if len(errs) > 0 {
		t.Errorf("unexpected errors for Copy cross-module instantiation: %v", errs)
	}
}

// --- Negative: structural-interface view instantiation stays allowed ---

func TestT1303_CrossModuleGenericStructuralViewAllowed(t *testing.T) {
	mod := `
type Sink ` + "`" + `public ` + "`" + `structural { emit(this, int x) int ` + "`" + `abstract; }
type Slot[V] ` + "`" + `public { V? _v; get val V? ` + "`" + `public { return this._v; } }
`
	user := `
use boxmod "./boxmod";
type Counter { int base ` + "`" + `value; emit(this, int x) int { return this.base + x; } }
make() {
    boxmod.Slot[boxmod.Sink] s = boxmod.Slot[boxmod.Sink](_v: Counter(base: 5));
}
`
	errs := checkOwnershipCrossModule(t, "./boxmod", mod, user)
	for _, e := range errs {
		if strings.Contains(e.Error(), "cannot move field") {
			t.Errorf("unexpected field-move rejection for structural view: %v", errs)
		}
	}
}

// --- Edge: module imported but its generic type is never instantiated ---
// The user unit has generic instances (an in-file generic + a non-generic
// module type), so the pass runs past its Instances>0 guard, but none of them
// are concrete instantiations of a module-defined generic, so byOrigin stays
// empty and the pass early-returns without re-checking any module body.

func TestT1303_CrossModuleGenericNeverInstantiatedNoRecheck(t *testing.T) {
	mod := "type Plain `public { int x; }\n" +
		"type Box[V] `public { V? _v; get val V? `public { return this._v; } }"
	user := `
use boxmod "./boxmod";
type Wrap[T] { T v; }
make() {
    boxmod.Plain p = boxmod.Plain(x: 1);
    Wrap[int] w = Wrap[int](v: 3);
}
`
	errs := checkOwnershipCrossModule(t, "./boxmod", mod, user)
	if len(errs) > 0 {
		t.Errorf("unexpected errors when module generic is never instantiated: %v", errs)
	}
}

// --- Edge: user file declares an enum alongside the rejected instantiation ---
// Exercises the enum arm of fileDeclaredNameds (the in-file exclusion set the
// cross-module pass builds so it doesn't double-report in-file generics) while
// the module-defined Box[Res] escape is still rejected.

func TestT1303_CrossModuleUserEnumDeclHandled(t *testing.T) {
	mod := "type Box[V] `public { V? _v; get val V? `public { return this._v; } }"
	user := `
use boxmod "./boxmod";
enum Color { red; green; }
type Res { string name; drop(~this) {} }
make() {
    boxmod.Box[Res] b = boxmod.Box[Res](_v: none);
    Color c = Color.red;
}
`
	errs := checkOwnershipCrossModule(t, "./boxmod", mod, user)
	expectOwnerError(t, errs, "cannot move field '_v' out of 'Box[Res]'")
}

// --- A pre-existing user-unit ownership error is preserved and not clobbered ---
// The dedup set seeded from c.errors (so the cross-module pass never duplicates
// an already-emitted diagnostic) runs over a non-empty error slice: the user
// unit has a use-after-move AND triggers the cross-module field-move rejection.
// Both must surface.

func TestT1303_CrossModulePriorOwnershipErrorPreserved(t *testing.T) {
	mod := "type Box[V] `public { V? _v; get val V? `public { return this._v; } }"
	user := `
use boxmod "./boxmod";
type Res { string name; drop(~this) {} }
make() {
    string s = "hi";
    string t2 = s;
    string u = s;
    boxmod.Box[Res] b = boxmod.Box[Res](_v: none);
}
`
	errs := checkOwnershipCrossModule(t, "./boxmod", mod, user)
	expectOwnerError(t, errs, "use of moved variable 's'")
	expectOwnerError(t, errs, "cannot move field '_v' out of 'Box[Res]'")
}

// --- Only field-move verdicts are lifted out of the isolated re-check ---
// The module's generic type body carries an unrelated ownership violation (a
// use-after-move in a second method). Module bodies are sema-checked but never
// ownership-checked in the user compilation, so this passes module sema; the
// isolated re-check surfaces it, but the pass must drop every non-field-move
// diagnostic and lift ONLY the field-move verdict. Guards the isFieldMoveVerdict
// filter (crossmodfieldmove.go) against leaking conservative module diagnostics.

func TestT1303_CrossModuleOnlyFieldMoveVerdictLifted(t *testing.T) {
	mod := "type Box[V] `public { V? _v; " +
		"get val V? `public { return this._v; } " +
		"leak(this, V move a) `public { V b = a; V c = a; } }"
	user := `
use boxmod "./boxmod";
type Res { string name; drop(~this) {} }
make() {
    boxmod.Box[Res] b = boxmod.Box[Res](_v: none);
}
`
	errs := checkOwnershipCrossModule(t, "./boxmod", mod, user)
	expectOwnerError(t, errs, "cannot move field '_v' out of 'Box[Res]'")
	for _, e := range errs {
		if strings.Contains(e.Error(), "use of moved variable") {
			t.Errorf("non-field-move module diagnostic leaked out of isolated re-check: %v", errs)
		}
	}
}

// --- Multi-module: a module whose generic type is never instantiated is skipped ---
// Two module infos participate; only boxmod's Box[Res] is instantiated. othermod
// defines a generic type that isn't instantiated, so its module info yields no
// targets and is skipped, while boxmod's escape is still rejected. Exercises the
// per-module targets==0 continue with more than one module info present.

func TestT1303_MultiModuleUninstantiatedGenericSkipped(t *testing.T) {
	boxSrc := "type Box[V] `public { V? _v; get val V? `public { return this._v; } }"
	otherSrc := "type Other[V] `public { V _o; }"
	user := `
use boxmod "./boxmod";
use othermod "./othermod";
type Res { string name; drop(~this) {} }
make() {
    boxmod.Box[Res] b = boxmod.Box[Res](_v: none);
}
`
	boxFile := parseFileT1303(t, "boxmod.pr", boxSrc)
	boxFile.Uses = append([]*ast.UseDecl{{Alias: "_", CatalogName: "std"}}, boxFile.Uses...)
	boxInfo, e1 := sema.CheckWithModules(boxFile, map[string]*types.Scope{"std": getOwnerStdScope()})
	if len(e1) > 0 {
		t.Fatalf("boxmod sema errors: %v", e1)
	}
	otherFile := parseFileT1303(t, "othermod.pr", otherSrc)
	otherFile.Uses = append([]*ast.UseDecl{{Alias: "_", CatalogName: "std"}}, otherFile.Uses...)
	otherInfo, e2 := sema.CheckWithModules(otherFile, map[string]*types.Scope{"std": getOwnerStdScope()})
	if len(e2) > 0 {
		t.Fatalf("othermod sema errors: %v", e2)
	}
	userFile := parseFileT1303(t, "main.pr", user)
	userFile.Uses = append([]*ast.UseDecl{{Alias: "_", CatalogName: "std"}}, userFile.Uses...)
	scopes := map[string]*types.Scope{
		"std":        getOwnerStdScope(),
		"./boxmod":   sema.ExportedScope(boxInfo, boxFile),
		"./othermod": sema.ExportedScope(otherInfo, otherFile),
	}
	info, semaErrs := sema.CheckWithModules(userFile, scopes)
	if len(semaErrs) > 0 {
		t.Fatalf("user sema errors: %v", semaErrs)
	}
	info.ModuleInfos = map[string]*sema.ModuleInfo{
		"./boxmod":   {Name: "boxmod", File: boxFile, SemaInfo: boxInfo},
		"./othermod": {Name: "othermod", File: otherFile, SemaInfo: otherInfo},
	}
	errs := Check(userFile, info)
	expectOwnerError(t, errs, "cannot move field '_v' out of 'Box[Res]'")
}
