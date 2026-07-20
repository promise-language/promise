package codegen

import (
	"strings"
	"testing"

	"github.com/antlr4-go/antlr/v4"

	"github.com/promise-language/promise/compiler/internal/ast"
	"github.com/promise-language/promise/compiler/internal/parser"
	"github.com/promise-language/promise/compiler/internal/sema"
	"github.com/promise-language/promise/compiler/internal/types"
)

// T1335: a value-position `if`/`match` where ONE arm diverges (return/raise) and
// another reachable arm produces no value (empty/void block) previously slipped
// past sema with a nil result type, then panicked codegen at `genTypedVarDecl`
// ("nil value for typed var decl"). Sema now rejects these inputs, so codegen is
// never reached. These tests assert the frontend reports a sema error (and thus
// never hands a nil value to codegen) — running the frontend must not panic.

// frontendErrs runs parse + sema (as the real compiler front-end does) and
// returns the accumulated sema errors WITHOUT calling t.Fatalf on them, so a
// test can assert that a given input is rejected before codegen.
func frontendErrs(t *testing.T, src string) []error {
	t.Helper()
	_, stdScope := getCodegenStdModInfo()

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

	stdUse := &ast.UseDecl{Alias: "_", CatalogName: "std"}
	file.Uses = append([]*ast.UseDecl{stdUse}, file.Uses...)

	_, errs = sema.CheckWithModules(file, map[string]*types.Scope{"std": stdScope})
	return errs
}

func assertRejected(t *testing.T, src, substr string) {
	t.Helper()
	errs := frontendErrs(t, src)
	if len(errs) == 0 {
		t.Fatalf("expected sema to reject input (would otherwise panic codegen), got no errors")
	}
	for _, e := range errs {
		if strings.Contains(e.Error(), substr) {
			return
		}
	}
	t.Fatalf("expected a sema error containing %q, got: %v", substr, errs)
}

func TestT1335MatchDivergeVoidRejectedBeforeCodegen(t *testing.T) {
	assertRejected(t, `
		f(bool b) int { int r = match b { true => { return 1 }, _ => {} }; return r; }
		main() {}
	`, "produces no value")
}

func TestT1335IfDivergeVoidRejectedBeforeCodegen(t *testing.T) {
	assertRejected(t, `
		g(bool b) int { int r = if b { return 1 } else {}; return r; }
		main() {}
	`, "produces no value")
}
