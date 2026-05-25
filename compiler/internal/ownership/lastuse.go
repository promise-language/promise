package ownership

import (
	"djabi.dev/go/promise_lang/internal/ast"
	"djabi.dev/go/promise_lang/internal/sema"
	"djabi.dev/go/promise_lang/internal/types"
)

// AnalyzeLastUses performs non-lexical lifetime (NLL) analysis to find early drop
// points. For each non-copy variable declared in a block, it determines the last
// statement that references the variable. If that statement is before the block's
// end, the variable can be dropped early (before scope exit). B0035.
//
// Returns a map from statement AST node to variable names that should be dropped
// immediately after that statement executes. Codegen uses this to emit drop calls
// at the last-use point rather than waiting for scope exit.
func AnalyzeLastUses(file *ast.File, info *sema.Info) map[ast.Stmt][]string {
	a := &lastUseAnalyzer{
		info:       info,
		earlyDrops: make(map[ast.Stmt][]string),
	}

	for _, decl := range file.Decls {
		switch d := decl.(type) {
		case *ast.FuncDecl:
			if d.Body != nil {
				a.analyzeBlock(d.Body)
			}
		case *ast.TypeDecl:
			for _, md := range d.Methods {
				if md.Body != nil {
					a.analyzeBlock(md.Body)
				}
			}
		case *ast.EnumDecl:
			for _, md := range d.Methods {
				if md.Body != nil {
					a.analyzeBlock(md.Body)
				}
			}
		}
	}

	return a.earlyDrops
}

type lastUseAnalyzer struct {
	info       *sema.Info
	earlyDrops map[ast.Stmt][]string
}

// analyzeBlock finds early drop points for non-copy variables declared in this block.
// Variables whose last use is before the final statement get registered for early drop.
// Also recurses into sub-blocks for variables declared in inner scopes.
func (a *lastUseAnalyzer) analyzeBlock(block *ast.Block) {
	if block == nil || len(block.Stmts) < 2 {
		// Need at least 2 statements: one to declare/use, one after for early drop to matter.
		// Still recurse into sub-blocks of single statements.
		if block != nil && len(block.Stmts) == 1 {
			a.analyzeStmtSubBlocks(block.Stmts[0])
		}
		return
	}

	stmts := block.Stmts

	// Collect non-copy variables declared in this block with their declaration index.
	type varInfo struct {
		name    string
		declIdx int
	}
	var vars []varInfo

	for i, stmt := range stmts {
		switch s := stmt.(type) {
		case *ast.TypedVarDecl:
			if s.Name != "_" && !a.isVarCopyType(s.Value) {
				vars = append(vars, varInfo{name: s.Name, declIdx: i})
			}
		case *ast.InferredVarDecl:
			if s.Name != "_" && !a.isVarCopyType(s.Value) {
				vars = append(vars, varInfo{name: s.Name, declIdx: i})
			}
		case *ast.DestructureVarDecl:
			for _, name := range s.Names {
				if name != "_" {
					// Destructured variables — check each binding.
					// We don't have per-binding types easily, so track all non-_
					// bindings and let codegen filter via dropBindings.
					vars = append(vars, varInfo{name: name, declIdx: i})
				}
			}
			// UseVarDecl: skip — use-bound vars need close() at scope exit, not early drop.
		}

		// Recurse into sub-blocks for inner scope variables.
		a.analyzeStmtSubBlocks(stmt)
	}

	if len(vars) == 0 {
		return
	}

	// For each variable, scan backwards from the block end to find the last statement
	// that references it. If it's before the final statement AND the statement type is
	// safe for early drop, register an early drop.
	for _, v := range vars {
		lastUseIdx := -1
		for i := len(stmts) - 1; i >= v.declIdx; i-- {
			if a.stmtReferencesVar(stmts[i], v.name) {
				lastUseIdx = i
				break
			}
		}

		if lastUseIdx == -1 {
			// Variable declared but never referenced (not even at declaration).
			// This shouldn't happen for InferredVarDecl since the declaration
			// itself references the name, but handle gracefully.
			continue
		}

		// Early drop only if the last use is strictly before the final statement.
		if lastUseIdx >= len(stmts)-1 {
			continue
		}

		// Safety check: only early-drop if the last-use statement is safe.
		// A statement is safe for early drop if it doesn't produce a stored
		// non-copy value that might hold a reference to the variable's data.
		if !a.isSafeForEarlyDrop(stmts[lastUseIdx], v.name) {
			continue
		}

		a.earlyDrops[stmts[lastUseIdx]] = append(a.earlyDrops[stmts[lastUseIdx]], v.name)
	}
}

// isSafeForEarlyDrop determines whether a variable can be safely dropped after
// the given statement. A statement is safe if it doesn't produce a stored non-copy
// value that might retain a reference/pointer to the variable's heap data.
//
// Safe statements:
//   - ExprStmt: result is discarded, no reference retention
//   - VarDecl/AssignStmt where the stored value is a COPY type (int, bool, etc.)
//   - ReturnStmt/RaiseStmt/YieldStmt: the variable is being consumed or the
//     function is exiting
//
// Unsafe statements (conservative — might hold a reference):
//   - VarDecl/AssignStmt where the stored value is NON-COPY (closure, user type,
//     string, etc.) — the new variable might hold a pointer to the original's data
//   - VarDecl/AssignStmt whose RHS contains a back-ref-carrier call on `name`
//     (e.g. `n := helper(vec, m.lock())`) — the helper may have stored the guard
//     in a longer-lived container, hiding the escape behind a copy-type return.
//     T0564.
//   - Control flow statements (if/while/for/select) — too complex, inner scope
//     interactions could cause LLVM IR domination issues
func (a *lastUseAnalyzer) isSafeForEarlyDrop(stmt ast.Stmt, name string) bool {
	switch s := stmt.(type) {
	case *ast.ExprStmt:
		// The call's own return value is discarded, but a sub-expression may
		// produce a back-ref carrier (e.g. MutexGuard[T] from m.lock()) that is
		// then stored elsewhere by an enclosing call (e.g. outer.push(m.lock())).
		// Such a stored value holds a back-pointer to `name`, so dropping `name`
		// early would leave a dangling pointer. T0557.
		if a.exprBackRefCapturesVar(s.Expr, name) {
			return false
		}
		return true

	case *ast.TypedVarDecl:
		// T0564: RHS may contain a back-ref-carrier call (e.g. m.lock()) buried
		// in a helper's argument list, with the helper returning a copy primitive
		// that masks the captured guard.
		if a.exprBackRefCapturesVar(s.Value, name) {
			return false
		}
		// Safe only if the produced value is a copy type (can't hold a reference).
		if s.Name == "_" {
			return true
		}
		typ := a.info.Types[s.Value]
		return typ != nil && isCopyType(typ) && !isStoredRefType(typ)

	case *ast.InferredVarDecl:
		// T0564: see TypedVarDecl above.
		if a.exprBackRefCapturesVar(s.Value, name) {
			return false
		}
		if s.Name == "_" {
			return true
		}
		typ := a.info.Types[s.Value]
		return typ != nil && isCopyType(typ) && !isStoredRefType(typ)

	case *ast.DestructureVarDecl:
		// Conservative: destructured values might be non-copy.
		return false

	case *ast.AssignStmt:
		// T0564: RHS may contain a back-ref-carrier call (e.g. m.lock()) buried
		// in a helper's argument list. Applies regardless of Op — compound
		// assigns evaluate the RHS the same way as simple assigns.
		if a.exprBackRefCapturesVar(s.Value, name) {
			return false
		}
		if s.Op != ast.OpAssign {
			// Compound assignment (+=, -=, etc.) doesn't store a new reference.
			return true
		}
		// Simple assignment: safe only if the value type is copy AND not a
		// stored borrow ref (T0381: `b = a.borrow` keeps `b` aliasing `a`'s
		// data, so dropping `a` early would invalidate `b`).
		typ := a.info.Types[s.Value]
		return typ != nil && isCopyType(typ) && !isStoredRefType(typ)

	case *ast.ReturnStmt, *ast.RaiseStmt, *ast.YieldStmt, *ast.YieldDelegateStmt:
		// Function is exiting or yielding — safe to drop before scope cleanup
		// (which would drop anyway).
		return true

	case *ast.IncDecStmt:
		// i++ / i-- — no reference retention.
		return true

	default:
		// Control flow and other complex statements — be conservative.
		return false
	}
}

// isStoredRefType reports whether a stored value of typ is a borrow ref
// (SharedRef/MutRef) — such values may alias the source's heap data, so the
// source cannot be safely dropped early. T0381: Arc.borrow / MutexGuard.borrow
// now return `T&`, and storing the result must hold the source alive.
func isStoredRefType(typ types.Type) bool {
	switch typ.(type) {
	case *types.SharedRef, *types.MutRef:
		return true
	}
	return false
}

// isBackRefCarrier reports whether typ is an opaque value type that internally
// stores a raw back-pointer to another value. Such carriers cannot be allowed
// to outlive the value they reference. MutexGuard[T] stores a pointer to its
// parent Mutex[T] and dereferences it in drop() to unlock. T0557.
func isBackRefCarrier(typ types.Type) bool {
	if typ == nil {
		return false
	}
	if _, ok := types.AsMutexGuard(typ); ok {
		return true
	}
	return false
}

// exprBackRefCapturesVar reports whether the expression tree contains a method
// call on the named variable whose return type is a back-ref carrier. Such a
// call produces a value that retains a pointer to `name`'s storage; if that
// value is then stored elsewhere (e.g. pushed into a container), early-dropping
// `name` would leave a dangling pointer. T0557.
//
// Scope: only walks expression sub-trees. Block-as-expression sub-blocks of
// IfExpr/MatchExpr/ErrorHandlerExpr/UnsafeExpr/LambdaExpr/GoExpr are NOT
// traversed — a call inside `if cond { m.lock() } else { ... }` is not
// currently detected.
func (a *lastUseAnalyzer) exprBackRefCapturesVar(expr ast.Expr, name string) bool {
	if expr == nil {
		return false
	}
	switch e := expr.(type) {
	case *ast.CallExpr:
		if mem, ok := e.Callee.(*ast.MemberExpr); ok {
			if id, ok := mem.Target.(*ast.IdentExpr); ok && id.Name == name {
				if isBackRefCarrier(a.info.Types[e]) {
					return true
				}
			}
		}
		if a.exprBackRefCapturesVar(e.Callee, name) {
			return true
		}
		for _, arg := range e.Args {
			if a.exprBackRefCapturesVar(arg.Value, name) {
				return true
			}
		}
		return false

	case *ast.MemberExpr:
		return a.exprBackRefCapturesVar(e.Target, name)

	case *ast.OptionalChainExpr:
		return a.exprBackRefCapturesVar(e.Target, name)

	case *ast.ParenExpr:
		return a.exprBackRefCapturesVar(e.Expr, name)

	case *ast.BinaryExpr:
		return a.exprBackRefCapturesVar(e.Left, name) || a.exprBackRefCapturesVar(e.Right, name)

	case *ast.UnaryExpr:
		return a.exprBackRefCapturesVar(e.Operand, name)

	case *ast.IndexExpr:
		if a.exprBackRefCapturesVar(e.Target, name) || a.exprBackRefCapturesVar(e.Index, name) {
			return true
		}
		for _, extra := range e.ExtraIndices {
			if a.exprBackRefCapturesVar(extra, name) {
				return true
			}
		}
		return false

	case *ast.SliceExpr:
		return a.exprBackRefCapturesVar(e.Target, name) ||
			a.exprBackRefCapturesVar(e.Low, name) ||
			a.exprBackRefCapturesVar(e.High, name)

	case *ast.CastExpr:
		return a.exprBackRefCapturesVar(e.Expr, name)

	case *ast.IsExpr:
		return a.exprBackRefCapturesVar(e.Expr, name)

	case *ast.ErrorPropagateExpr:
		return a.exprBackRefCapturesVar(e.Expr, name)

	case *ast.ErrorPanicExpr:
		return a.exprBackRefCapturesVar(e.Expr, name)

	case *ast.OptionalUnwrapExpr:
		return a.exprBackRefCapturesVar(e.Expr, name)

	case *ast.AutoCloneExpr: // T0605
		return a.exprBackRefCapturesVar(e.Expr, name)

	case *ast.ErrorHandlerExpr:
		return a.exprBackRefCapturesVar(e.Expr, name)

	case *ast.IfExpr:
		return a.exprBackRefCapturesVar(e.Cond, name)

	case *ast.MatchExpr:
		return a.exprBackRefCapturesVar(e.Subject, name)

	case *ast.TupleLit:
		for _, elem := range e.Elements {
			if a.exprBackRefCapturesVar(elem, name) {
				return true
			}
		}
		return false

	case *ast.ArrayLit:
		for _, elem := range e.Elements {
			if a.exprBackRefCapturesVar(elem, name) {
				return true
			}
		}
		return false

	case *ast.MapLit:
		for _, entry := range e.Entries {
			if a.exprBackRefCapturesVar(entry.Key, name) || a.exprBackRefCapturesVar(entry.Value, name) {
				return true
			}
		}
		return false
	}
	return false
}

// isVarCopyType returns true if the expression's resolved type is a copy type.
func (a *lastUseAnalyzer) isVarCopyType(expr ast.Expr) bool {
	if expr == nil {
		return true // no value → treat as copy (won't be dropped)
	}
	typ := a.info.Types[expr]
	if typ == nil {
		return true // unresolved → be conservative, don't track
	}
	return isCopyType(typ)
}

// analyzeStmtSubBlocks recurses into sub-blocks of control flow statements
// to analyze inner-scope variables for early drops.
func (a *lastUseAnalyzer) analyzeStmtSubBlocks(stmt ast.Stmt) {
	switch s := stmt.(type) {
	case *ast.IfStmt:
		a.analyzeBlock(s.Body)
		if s.Else != nil {
			a.analyzeStmtSubBlocks(s.Else)
		}
	case *ast.WhileStmt:
		a.analyzeBlock(s.Body)
	case *ast.WhileUnwrapStmt:
		a.analyzeBlock(s.Body)
	case *ast.ForInStmt:
		a.analyzeBlock(s.Body)
	case *ast.ClassicForStmt:
		a.analyzeBlock(s.Body)
	case *ast.InfiniteLoop:
		a.analyzeBlock(s.Body)
	case *ast.SelectStmt:
		// Select case bodies are []Stmt, not *Block. We'd need to wrap them
		// to analyze. Skip for now — select cases are typically short.
	case *ast.Block:
		a.analyzeBlock(s)
	}
}

// stmtReferencesVar returns true if the statement (including any nested
// expressions and sub-blocks) contains a reference to the named variable.
func (a *lastUseAnalyzer) stmtReferencesVar(stmt ast.Stmt, name string) bool {
	if stmt == nil {
		return false
	}
	switch s := stmt.(type) {
	case *ast.ExprStmt:
		return a.exprReferencesVar(s.Expr, name)

	case *ast.TypedVarDecl:
		return s.Name == name || a.exprReferencesVar(s.Value, name)

	case *ast.InferredVarDecl:
		return s.Name == name || a.exprReferencesVar(s.Value, name)

	case *ast.DestructureVarDecl:
		for _, n := range s.Names {
			if n == name {
				return true
			}
		}
		return a.exprReferencesVar(s.Value, name)

	case *ast.UseVarDecl:
		return s.Name == name || a.exprReferencesVar(s.Value, name)

	case *ast.AssignStmt:
		return a.exprReferencesVar(s.Target, name) || a.exprReferencesVar(s.Value, name)

	case *ast.ReturnStmt:
		return a.exprReferencesVar(s.Value, name)

	case *ast.RaiseStmt:
		return a.exprReferencesVar(s.Value, name)

	case *ast.YieldStmt:
		return a.exprReferencesVar(s.Value, name)

	case *ast.YieldDelegateStmt:
		return a.exprReferencesVar(s.Value, name)

	case *ast.IncDecStmt:
		return a.exprReferencesVar(s.Target, name)

	case *ast.IfStmt:
		if a.exprReferencesVar(s.Cond, name) || a.exprReferencesVar(s.Init, name) {
			return true
		}
		if a.blockReferencesVar(s.Body, name) {
			return true
		}
		if s.Else != nil {
			return a.stmtReferencesVar(s.Else, name)
		}
		return false

	case *ast.WhileStmt:
		return a.exprReferencesVar(s.Cond, name) || a.blockReferencesVar(s.Body, name)

	case *ast.WhileUnwrapStmt:
		return a.exprReferencesVar(s.Value, name) || a.blockReferencesVar(s.Body, name)

	case *ast.ForInStmt:
		return a.exprReferencesVar(s.Iterable, name) || a.blockReferencesVar(s.Body, name)

	case *ast.ClassicForStmt:
		if a.exprReferencesVar(s.InitValue, name) || a.exprReferencesVar(s.Cond, name) {
			return true
		}
		if a.blockReferencesVar(s.Body, name) {
			return true
		}
		if s.UpdateValue != nil && a.exprReferencesVar(s.UpdateValue, name) {
			return true
		}
		if s.UpdateTarget != nil && a.exprReferencesVar(s.UpdateTarget, name) {
			return true
		}
		return false

	case *ast.InfiniteLoop:
		return a.blockReferencesVar(s.Body, name)

	case *ast.SelectStmt:
		for _, sc := range s.Cases {
			if a.exprReferencesVar(sc.Channel, name) {
				return true
			}
			if sc.SendValue != nil && a.exprReferencesVar(sc.SendValue, name) {
				return true
			}
			for _, bodyStmt := range sc.Body {
				if a.stmtReferencesVar(bodyStmt, name) {
					return true
				}
			}
		}
		if s.Default != nil {
			for _, bodyStmt := range s.Default {
				if a.stmtReferencesVar(bodyStmt, name) {
					return true
				}
			}
		}
		return false

	case *ast.Block:
		return a.blockReferencesVar(s, name)

	case *ast.BreakStmt, *ast.ContinueStmt:
		return false
	}
	return false
}

// blockReferencesVar returns true if any statement in the block references the variable.
func (a *lastUseAnalyzer) blockReferencesVar(block *ast.Block, name string) bool {
	if block == nil {
		return false
	}
	for _, stmt := range block.Stmts {
		if a.stmtReferencesVar(stmt, name) {
			return true
		}
	}
	return false
}

// exprReferencesVar returns true if the expression tree contains a reference
// to the named variable.
func (a *lastUseAnalyzer) exprReferencesVar(expr ast.Expr, name string) bool {
	if expr == nil {
		return false
	}
	switch e := expr.(type) {
	case *ast.IdentExpr:
		return e.Name == name

	case *ast.ThisExpr:
		return name == "this"

	case *ast.CallExpr:
		if a.exprReferencesVar(e.Callee, name) {
			return true
		}
		for _, arg := range e.Args {
			if a.exprReferencesVar(arg.Value, name) {
				return true
			}
		}
		return false

	case *ast.BinaryExpr:
		return a.exprReferencesVar(e.Left, name) || a.exprReferencesVar(e.Right, name)

	case *ast.UnaryExpr:
		return a.exprReferencesVar(e.Operand, name)

	case *ast.IndexExpr:
		if a.exprReferencesVar(e.Target, name) || a.exprReferencesVar(e.Index, name) {
			return true
		}
		for _, extra := range e.ExtraIndices {
			if a.exprReferencesVar(extra, name) {
				return true
			}
		}
		return false

	case *ast.SliceExpr:
		return a.exprReferencesVar(e.Target, name) ||
			a.exprReferencesVar(e.Low, name) ||
			a.exprReferencesVar(e.High, name)

	case *ast.SliceTypeExpr:
		return a.exprReferencesVar(e.Inner, name)

	case *ast.MemberExpr:
		return a.exprReferencesVar(e.Target, name)

	case *ast.OptionalChainExpr:
		return a.exprReferencesVar(e.Target, name)

	case *ast.IsExpr:
		return a.exprReferencesVar(e.Expr, name)

	case *ast.CastExpr:
		return a.exprReferencesVar(e.Expr, name)

	case *ast.ErrorPropagateExpr:
		return a.exprReferencesVar(e.Expr, name)

	case *ast.ErrorPanicExpr:
		return a.exprReferencesVar(e.Expr, name)

	case *ast.OptionalUnwrapExpr:
		return a.exprReferencesVar(e.Expr, name)

	case *ast.AutoCloneExpr: // T0605
		return a.exprReferencesVar(e.Expr, name)

	case *ast.ErrorHandlerExpr:
		return a.exprReferencesVar(e.Expr, name) || a.blockReferencesVar(e.Body, name)

	case *ast.IfExpr:
		return a.exprReferencesVar(e.Cond, name) ||
			a.blockReferencesVar(e.Then, name) ||
			a.blockReferencesVar(e.Else, name)

	case *ast.MatchExpr:
		if a.exprReferencesVar(e.Subject, name) {
			return true
		}
		for _, arm := range e.Arms {
			if a.patternReferencesVar(arm.Pattern, name) {
				return true
			}
			if arm.Guard != nil && a.exprReferencesVar(arm.Guard, name) {
				return true
			}
			if arm.Body != nil && a.exprReferencesVar(arm.Body, name) {
				return true
			}
			if arm.Block != nil && a.blockReferencesVar(arm.Block, name) {
				return true
			}
		}
		return false

	case *ast.GoExpr:
		if e.Expr != nil && a.exprReferencesVar(e.Expr, name) {
			return true
		}
		return a.blockReferencesVar(e.Block, name)

	case *ast.UnsafeExpr:
		return a.blockReferencesVar(e.Body, name)

	case *ast.LambdaExpr:
		// For lambdas, only captures matter — the lambda body runs later,
		// and captures transfer ownership at the lambda creation site.
		if captures := a.info.LambdaCaptures[e]; len(captures) > 0 {
			for _, cv := range captures {
				if cv.Obj.Name() == name {
					return true
				}
			}
		}
		return false

	case *ast.ParenExpr:
		return a.exprReferencesVar(e.Expr, name)

	case *ast.TupleLit:
		for _, elem := range e.Elements {
			if a.exprReferencesVar(elem, name) {
				return true
			}
		}
		return false

	case *ast.ArrayLit:
		for _, elem := range e.Elements {
			if a.exprReferencesVar(elem, name) {
				return true
			}
		}
		return false

	case *ast.MapLit:
		for _, entry := range e.Entries {
			if a.exprReferencesVar(entry.Key, name) || a.exprReferencesVar(entry.Value, name) {
				return true
			}
		}
		return false

	case *ast.StringLit:
		// String literals with interpolation can reference variables: "{x}".
		for _, part := range e.Parts {
			if interp, ok := part.(ast.StringInterp); ok {
				if a.exprReferencesVar(interp.Expr, name) {
					return true
				}
			}
		}
		return false

	case *ast.IntLit, *ast.FloatLit, *ast.BoolLit,
		*ast.NoneLit, *ast.CharLit:
		return false
	}
	return false
}

// patternReferencesVar checks if a match pattern contains a reference to the variable.
// Most patterns create new bindings rather than referencing existing variables,
// but ExpressionMatchPattern and LiteralMatchPattern contain expressions.
func (a *lastUseAnalyzer) patternReferencesVar(pat ast.MatchPattern, name string) bool {
	if pat == nil {
		return false
	}
	switch p := pat.(type) {
	case *ast.ExpressionMatchPattern:
		return a.exprReferencesVar(p.Expr, name)
	case *ast.LiteralMatchPattern:
		return a.exprReferencesVar(p.Value, name)
	}
	return false
}

// AnalyzeRefLastUses finds last-use points for reference-type variables (SharedRef/MutRef).
// Used by the ownership checker for NLL borrow narrowing (T0164): borrows expire at the
// last use of the borrower variable rather than at scope exit.
// Returns a map from statement AST node to ref variable names whose last use is that
// statement. Unlike AnalyzeLastUses, this includes last-statement cases because borrows
// don't automatically expire at scope exit.
func AnalyzeRefLastUses(file *ast.File, info *sema.Info) map[ast.Stmt][]string {
	a := &lastUseAnalyzer{
		info:       info,
		earlyDrops: make(map[ast.Stmt][]string), // unused, satisfies struct
	}
	result := make(map[ast.Stmt][]string)

	for _, decl := range file.Decls {
		switch d := decl.(type) {
		case *ast.FuncDecl:
			if d.Body != nil {
				a.analyzeRefBlock(d.Body, result)
			}
		case *ast.TypeDecl:
			for _, md := range d.Methods {
				if md.Body != nil {
					a.analyzeRefBlock(md.Body, result)
				}
			}
		case *ast.EnumDecl:
			for _, md := range d.Methods {
				if md.Body != nil {
					a.analyzeRefBlock(md.Body, result)
				}
			}
		}
	}
	return result
}

// analyzeRefBlock finds last-use points for reference-type variables declared in this block.
// Registers expiry for ALL last-use points (including the final statement) because
// borrows don't automatically expire at scope exit — they need explicit expiry.
func (a *lastUseAnalyzer) analyzeRefBlock(block *ast.Block, result map[ast.Stmt][]string) {
	if block == nil || len(block.Stmts) == 0 {
		return
	}
	stmts := block.Stmts

	// Recurse into sub-blocks for inner-scope ref variables.
	for _, stmt := range stmts {
		a.analyzeRefStmtSubBlocks(stmt, result)
	}

	// Collect reference-type variable declarations in this block.
	type varInfo struct {
		name    string
		declIdx int
	}
	var refVars []varInfo

	for i, stmt := range stmts {
		switch s := stmt.(type) {
		case *ast.TypedVarDecl:
			if s.Name != "_" && isRefTypeRef(s.Type) {
				refVars = append(refVars, varInfo{name: s.Name, declIdx: i})
			}
		case *ast.InferredVarDecl:
			if s.Name != "_" && a.isVarRefType(s.Value) {
				refVars = append(refVars, varInfo{name: s.Name, declIdx: i})
			}
		case *ast.DestructureVarDecl:
			// T0548: destructured locals from MemberExpr/IndexExpr sources
			// hold shared borrows on the source's root variable (registered in
			// checkDestructureVarDecl). Non-Copy locals need last-use tracking
			// so the borrow expires at the borrower's last use rather than
			// scope exit — otherwise destructure-then-consume-parent (after
			// the locals' last use) would falsely reject.
			// T0570: peel ParenExpr so paren-wrapped sources (`(h.pair)`,
			// `(arr[0])`) still get last-use narrowing — otherwise consume-
			// after-last-use is incorrectly rejected.
			switch unwrapDestructureParens(s.Value).(type) {
			case *ast.MemberExpr, *ast.IndexExpr:
				var elems []types.Type
				if tup, ok := a.info.Types[s.Value].(*types.Tuple); ok {
					elems = tup.Elems()
				}
				for j, name := range s.Names {
					if name == "_" {
						continue
					}
					if j < len(elems) && isCopyType(elems[j]) {
						continue
					}
					refVars = append(refVars, varInfo{name: name, declIdx: i})
				}
			}
		}
	}

	if len(refVars) == 0 {
		return
	}

	for _, v := range refVars {
		lastUseIdx := -1
		for i := len(stmts) - 1; i >= v.declIdx; i-- {
			if a.stmtReferencesVar(stmts[i], v.name) {
				lastUseIdx = i
				break
			}
		}
		if lastUseIdx == -1 {
			continue
		}
		// Register ALL last-use points (including last stmt).
		result[stmts[lastUseIdx]] = append(result[stmts[lastUseIdx]], v.name)
	}
}

// analyzeRefStmtSubBlocks recurses into sub-blocks for ref last-use analysis.
func (a *lastUseAnalyzer) analyzeRefStmtSubBlocks(stmt ast.Stmt, result map[ast.Stmt][]string) {
	switch s := stmt.(type) {
	case *ast.IfStmt:
		a.analyzeRefBlock(s.Body, result)
		if s.Else != nil {
			a.analyzeRefStmtSubBlocks(s.Else, result)
		}
	case *ast.WhileStmt:
		a.analyzeRefBlock(s.Body, result)
	case *ast.WhileUnwrapStmt:
		a.analyzeRefBlock(s.Body, result)
	case *ast.ForInStmt:
		a.analyzeRefBlock(s.Body, result)
	case *ast.ClassicForStmt:
		a.analyzeRefBlock(s.Body, result)
	case *ast.InfiniteLoop:
		a.analyzeRefBlock(s.Body, result)
	case *ast.Block:
		a.analyzeRefBlock(s, result)
	}
}

// isRefTypeRef returns true if the type reference is a shared or mutable reference type.
func isRefTypeRef(tr ast.TypeRef) bool {
	switch tr.(type) {
	case *ast.SharedRefTypeRef, *ast.MutRefTypeRef:
		return true
	}
	return false
}

// isVarRefType returns true if the expression's resolved type is a reference type.
func (a *lastUseAnalyzer) isVarRefType(expr ast.Expr) bool {
	if expr == nil {
		return false
	}
	typ := a.info.Types[expr]
	if typ == nil {
		return false
	}
	switch typ.(type) {
	case *types.SharedRef, *types.MutRef:
		return true
	}
	return false
}
