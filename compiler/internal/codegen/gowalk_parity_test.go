package codegen

import (
	"go/ast"
	"go/parser"
	"go/token"
	"sort"
	"testing"
)

// Two independent walkers decide what a `go { … }` block captures, and they are
// required to agree exactly:
//
//   - sema's `checkGoBlockCaptures` (internal/sema/sendable.go) decides whether a
//     capture is LEGAL (the sendability gate, T1589's mutable-borrow rejection)
//     and records the capture set into `Info.GoCaptures`, which since T1640 is the
//     only record ownership has of it — R4 (the capture moves the closure) and R5
//     (only an owning binding may cross) both key off it.
//   - codegen's `collectBlockIdents` (expr_concurrency.go) decides whether the
//     value's heap env is TRANSFERRED into the coroutine frame, clearing the
//     spawner's drop flag.
//
// An AST shape codegen walks but sema does not is therefore a use-after-free by
// construction: codegen hands the env to the goroutine while ownership, never
// told of the capture, lets the defining scope keep using it. That has now
// happened twice — T1651 (`scopeContains` misclassifying a one-line `go {}`) and
// T1658 (bare block, match-arm guard) — so this test makes the next
// divergence a test failure instead of a miscompile.
//
// LIMITATION: this compares the SET of `case *ast.X` arms, not what the arms do.
// An arm present in both walkers but recursing into different children still
// slips through — T1698 was exactly that (both had `*ast.MatchExpr`, neither
// walked `arm.Pattern`, so a capture reached only through the
// `match true { <expr> => … }` dispatch form panicked codegen). The arm-by-arm
// behavioural coverage lives in internal/sema/t1640_capture_walk_test.go and
// internal/ownership/t1640_capture_walk_test.go.
//
// One intra-arm divergence is deliberate and benign: codegen's `*ast.GoExpr` arm
// recurses into a NESTED go block (its captures must reach the outer coroutine's
// arg pack too) while sema's does not — sema runs its own
// checkGoBlockCaptures/checkGoExprSendable on the inner GoExpr, which gates and
// records the capture against the inner block's scope.
//
// The real fix for the class is a single shared walker (or codegen consuming
// `Info.GoCaptures` instead of re-deriving it) — tracked as T1674.

// goWalkArmParityAllowlist names arms codegen walks that sema deliberately does
// not. Neither names an OWNED capture, so neither can produce the env divergence
// above — one is a borrow, the other a type reference:
//
//   - ThisExpr: `this` is a BORROW of the receiver, not an owned capture. Codegen
//     threads it into the arg pack (T1219) as a private snapshot; there is no
//     heap env to transfer and no drop flag to clear, so it cannot produce the
//     env divergence this test guards. It DOES leave a separate hole — a
//     `not_sendable` receiver crosses the boundary ungated, because running the
//     sendability check on `this` means running it WITHOUT R4/R5, which are wrong
//     for a borrow. Tracked as T1699 (see also T1657/T1650 for the `this`-capture
//     lifetime residuals).
//   - SliceTypeExpr: `Inner` is by definition a TYPE reference (T0685), never a
//     variable, so walking it can never reach a capture. Codegen's arm is
//     harmless; sema has nothing to record.
var goWalkArmParityAllowlist = map[string]string{
	"ThisExpr":      "borrowed receiver, not an owned capture (T1219; sendability is T1657/T1650)",
	"SliceTypeExpr": "Inner is a type reference, never a variable (T0685)",
}

// typeSwitchArms returns every `*ast.X` type-switch case arm inside the named
// top-level function, including arms in closures declared in its body.
func typeSwitchArms(t *testing.T, path, funcName string) map[string]bool {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	var body *ast.BlockStmt
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if ok && fn.Name.Name == funcName {
			body = fn.Body
			break
		}
	}
	if body == nil {
		t.Fatalf("%s: function %s not found (renamed? then update this guard)", path, funcName)
	}
	arms := make(map[string]bool)
	ast.Inspect(body, func(n ast.Node) bool {
		clause, ok := n.(*ast.CaseClause)
		if !ok {
			return true
		}
		for _, expr := range clause.List {
			star, ok := expr.(*ast.StarExpr)
			if !ok {
				continue
			}
			sel, ok := star.X.(*ast.SelectorExpr)
			if !ok {
				continue
			}
			pkg, ok := sel.X.(*ast.Ident)
			if !ok || pkg.Name != "ast" {
				continue
			}
			arms[sel.Sel.Name] = true
		}
		return true
	})
	if len(arms) == 0 {
		t.Fatalf("%s: %s has no *ast.X type-switch arms (walk restructured? then update this guard)", path, funcName)
	}
	return arms
}

func TestGoBlockWalkersAgreeOnArms(t *testing.T) {
	semaArms := typeSwitchArms(t, "../sema/sendable.go", "checkGoBlockCaptures")
	codegenArms := typeSwitchArms(t, "expr_concurrency.go", "collectBlockIdents")

	var missing []string
	for arm := range codegenArms {
		if semaArms[arm] || goWalkArmParityAllowlist[arm] != "" {
			continue
		}
		missing = append(missing, arm)
	}
	sort.Strings(missing)
	for _, arm := range missing {
		t.Errorf("codegen's collectBlockIdents walks *ast.%s but sema's checkGoBlockCaptures does not: "+
			"a capture reached only through that shape gets its heap env transferred into the coroutine "+
			"frame while ownership never records the capture — a use-after-free (T1658). Add the mirroring "+
			"arm to checkGoBlockCaptures, or add *ast.%s to goWalkArmParityAllowlist with the reason it "+
			"can never name a captured variable.", arm, arm)
	}

	// The other direction. It is the less dangerous one — a shape sema records but
	// codegen never puts in the coroutine arg pack fails LOUDLY (`panic: codegen:
	// undefined variable`, T1698's signature) rather than silently freeing an env
	// under a running goroutine — but "agree exactly" is the contract, so guard it
	// too. There is no allowlist here: every arm sema walks names a value that must
	// reach the coroutine.
	var unpacked []string
	for arm := range semaArms {
		if !codegenArms[arm] {
			unpacked = append(unpacked, arm)
		}
	}
	sort.Strings(unpacked)
	for _, arm := range unpacked {
		t.Errorf("sema's checkGoBlockCaptures walks *ast.%s but codegen's collectBlockIdents does not: "+
			"a capture reached only through that shape is gated and recorded (so ownership marks it Moved) "+
			"while its name never enters the coroutine arg pack — codegen panics `undefined variable` on it "+
			"(T1698). Add the mirroring arm to collectBlockIdents.", arm)
	}

	// The allowlist must not outlive its entries: a stale name here would silently
	// excuse a future arm that reuses it.
	for arm, why := range goWalkArmParityAllowlist {
		if !codegenArms[arm] {
			t.Errorf("goWalkArmParityAllowlist has *ast.%s (%s) but collectBlockIdents no longer walks it; "+
				"drop the entry", arm, why)
		}
	}
}
