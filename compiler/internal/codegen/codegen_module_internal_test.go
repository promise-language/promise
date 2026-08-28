// Tests lifted back out of tests/module (T1776): they reach into codegen's
// unexported internals, so they have to be compiled as part of package
// codegen. They use the private helper copies in codegen_helpers_test.go.
package codegen

import (
	"testing"

	antlr "github.com/antlr4-go/antlr/v4"
	"github.com/promise-language/promise/compiler/internal/ast"
	"github.com/promise-language/promise/compiler/internal/parser"
	"github.com/promise-language/promise/compiler/internal/sema"
	"github.com/promise-language/promise/compiler/internal/types"
)

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
