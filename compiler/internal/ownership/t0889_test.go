package ownership

import (
	"testing"

	antlr "github.com/antlr4-go/antlr/v4"
	"github.com/promise-language/promise/compiler/internal/ast"
	"github.com/promise-language/promise/compiler/internal/parser"
	"github.com/promise-language/promise/compiler/internal/sema"
	"github.com/promise-language/promise/compiler/internal/types"
)

// T0889: a var-decl whose initializer is `m := d.method()` where the method body
// is `return this` aliases m and d to the same instance. Registering m for NLL
// early drop (B0035) would free the shared instance at m's last use while d is
// still read afterward — use-after-free. AnalyzeLastUses must therefore NOT
// register such a var for early drop.

// analyzeLastUsesForSrc parses + sema-checks src, then runs the NLL last-use
// analysis and returns the set of variable names registered for early drop.
func analyzeLastUsesForSrc(t *testing.T, src string) map[string]bool {
	t.Helper()

	input := antlr.NewInputStream(src)
	lexer := parser.NewPromiseLexer(input)
	lexer.RemoveErrorListeners()
	stream := antlr.NewCommonTokenStream(lexer, antlr.TokenDefaultChannel)
	p := parser.NewPromiseParser(stream)
	p.RemoveErrorListeners()
	tree := p.CompilationUnit()
	file, buildErrs := ast.Build("test.pr", tree)
	if len(buildErrs) > 0 {
		t.Fatalf("AST build errors: %v", buildErrs)
	}

	stdUse := &ast.UseDecl{Alias: "_", CatalogName: "std"}
	file.Uses = append([]*ast.UseDecl{stdUse}, file.Uses...)

	info, semaErrs := sema.CheckWithModules(file, map[string]*types.Scope{"std": getOwnerStdScope()})
	if len(semaErrs) > 0 {
		t.Fatalf("sema errors: %v", semaErrs)
	}

	drops := AnalyzeLastUses(file, info)
	names := make(map[string]bool)
	for _, vs := range drops {
		for _, v := range vs {
			names[v] = true
		}
	}
	return names
}

// The aliasing result must NOT be early-dropped: its last use is non-final
// (followed by two more statements), so without the T0889 guard it would be.
func TestT0889AliasedResultNotEarlyDropped(t *testing.T) {
	names := analyzeLastUsesForSrc(t, `
		type DB { int v; dup() DB { return this; } drop(~this){} }
		f() {
			d := DB(v: 11);
			m := d.dup();
			a := m.v;
			b := d.v;
		}
	`)
	if names["m"] {
		t.Errorf("aliasing result 'm' was registered for early drop; want suppressed")
	}
}

// Negative boundary: a fresh-value return (not `return this`) still aliases
// nothing, but the same call shape conservatively suppresses early drop. Either
// way 'm' must not corrupt 'd'; assert the guard keeps 'm' out of early drop so
// the suppression is observable and pinned.
func TestT0889FreshValueResultSuppressed(t *testing.T) {
	names := analyzeLastUsesForSrc(t, `
		type DB { int v; dup() DB { return DB(v: this.v); } drop(~this){} }
		f() {
			d := DB(v: 11);
			m := d.dup();
			a := m.v;
			b := d.v;
		}
	`)
	if names["m"] {
		t.Errorf("method-call result 'm' was registered for early drop; want suppressed")
	}
}

// Boundary: a copy-type result from the same call shape is unaffected by the
// guard (copy types are never tracked for drop), so 'n' simply never appears.
// A non-aliasing non-copy local with a non-final last use is still early-dropped
// normally — pins that the guard is narrow (only the method/operator-result case).
func TestT0889PlainLocalStillEarlyDropped(t *testing.T) {
	names := analyzeLastUsesForSrc(t, `
		f() {
			s := "hello".repeat(2);
			a := s.len;
			b := 1;
		}
	`)
	if !names["s"] {
		t.Errorf("non-aliasing local 's' should still be early-dropped; got %v", names)
	}
}

// Chained `return this`: m := d.dup().dup() walks the chain back to d (the
// codegen alias-clear follows the same chain via chainOriginExpr). The result
// still aliases d, so its early drop must be suppressed even through the chain —
// exercises the inner-CallExpr `continue` arm of aliasReceiverOrigin.
func TestT0889ChainedReturnThisSuppressed(t *testing.T) {
	names := analyzeLastUsesForSrc(t, `
		type DB { int v; dup() DB { return this; } drop(~this){} }
		f() {
			d := DB(v: 11);
			m := d.dup().dup();
			a := m.v;
			b := d.v;
		}
	`)
	if names["m"] {
		t.Errorf("chained aliasing result 'm' was registered for early drop; want suppressed")
	}
}

// A plain free-function call (callee is not a MemberExpr) has no receiver to
// alias, so its result is early-dropped normally — pins the `!ok` (no receiver)
// arm of aliasReceiverOrigin and keeps the guard from over-suppressing.
func TestT0889FreeFunctionResultStillEarlyDropped(t *testing.T) {
	names := analyzeLastUsesForSrc(t, `
		type DB { int v; drop(~this){} }
		make() DB { return DB(v: 7); }
		f() {
			m := make();
			a := m.v;
			b := 1;
		}
	`)
	if !names["m"] {
		t.Errorf("free-function result 'm' should still be early-dropped; got %v", names)
	}
}

// aliasReceiverOrigin is a pure AST-shape function mirroring codegen's
// chainOriginExpr (method chains) and the operator receiver-origin. This table
// pins every arm directly — including the nil-returning forms (free call, special
// binary operators, channel receive) the source-level tests above cannot all reach.
func TestT0889AliasReceiverOrigin(t *testing.T) {
	d := &ast.IdentExpr{Name: "d"}
	this := &ast.ThisExpr{}

	// call(target) builds `target.m()`.
	call := func(target ast.Expr) *ast.CallExpr {
		return &ast.CallExpr{Callee: &ast.MemberExpr{Target: target, Field: "m"}}
	}

	tests := []struct {
		name string
		in   ast.Expr
		want ast.Expr // nil means "expect nil origin"
	}{
		{"method call on ident", call(d), d},
		{"method call on this", call(this), this},
		{"chained method calls", call(call(d)), d},
		{"paren-wrapped receiver", call(&ast.ParenExpr{Expr: d}), d},
		{"paren-wrapped chain", &ast.ParenExpr{Expr: call(call(d))}, d},
		{"free function call", &ast.CallExpr{Callee: d}, nil},
		{"binary add returns left", &ast.BinaryExpr{Op: ast.BinAdd, Left: d, Right: d}, d},
		{"binary and is nil", &ast.BinaryExpr{Op: ast.BinAnd, Left: d, Right: d}, nil},
		{"binary or is nil", &ast.BinaryExpr{Op: ast.BinOr, Left: d, Right: d}, nil},
		{"binary elvis is nil", &ast.BinaryExpr{Op: ast.BinElvis, Left: d, Right: d}, nil},
		{"exclusive range is nil", &ast.BinaryExpr{Op: ast.BinExclusiveRange, Left: d, Right: d}, nil},
		{"inclusive range is nil", &ast.BinaryExpr{Op: ast.BinInclusiveRange, Left: d, Right: d}, nil},
		{"unary neg returns operand", &ast.UnaryExpr{Op: ast.UnaryNeg, Operand: d}, d},
		{"channel receive is nil", &ast.UnaryExpr{Op: ast.UnaryReceive, Operand: d}, nil},
		{"bare ident has no origin", d, nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := aliasReceiverOrigin(tc.in)
			if got != tc.want {
				t.Errorf("aliasReceiverOrigin = %T(%v), want %T(%v)", got, got, tc.want, tc.want)
			}
		})
	}
}

// initMayAliasReceiver has two early guards before the origin switch: a nil
// initializer and a copy-typed initializer both short-circuit to false. The
// var-decl call site never passes either (isVarCopyType short-circuits first),
// so these defensive arms need direct coverage.
func TestT0889InitMayAliasReceiverGuards(t *testing.T) {
	ident := &ast.IdentExpr{Name: "d"}
	copyCall := &ast.CallExpr{Callee: &ast.MemberExpr{Target: ident, Field: "m"}}

	a := &lastUseAnalyzer{info: &sema.Info{Types: map[ast.Expr]types.Type{
		copyCall: types.TypInt, // copy type
	}}}

	if a.initMayAliasReceiver(nil) {
		t.Errorf("nil initializer must not be treated as aliasing")
	}
	if a.initMayAliasReceiver(copyCall) {
		t.Errorf("copy-typed initializer must not be treated as aliasing")
	}
}
