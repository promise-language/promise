package ownership

import "github.com/promise-language/promise/compiler/internal/ast"

// blockDiverges reports whether a block's control flow leaves the enclosing
// function before falling through to the statement after it — its trailing
// statement always returns, raises, or is an infinite loop with no break. Used
// to exclude a branch's end-state from post-branch move-state merges: a move on
// such a path is unreachable by the code that runs when the path was NOT taken,
// so it must not poison that code. (T1134)
//
// Only return/raise (and structural combinations) count as divergence here.
// break/continue are deliberately NOT divergence for this predicate: they
// transfer the branch's current state to post-loop / loop-test code, so
// excluding them from a merge would drop a real move. (Break-carried state is
// not separately tracked, so the conservative merge must keep covering it.)
func blockDiverges(block *ast.Block) bool {
	if block == nil || len(block.Stmts) == 0 {
		return false
	}
	return stmtDiverges(block.Stmts[len(block.Stmts)-1])
}

// stmtsDiverge reports whether a statement slice (e.g. a select case body)
// leaves the function before its last statement falls through. (T1134)
func stmtsDiverge(stmts []ast.Stmt) bool {
	if len(stmts) == 0 {
		return false
	}
	return stmtDiverges(stmts[len(stmts)-1])
}

// stmtDiverges reports whether a statement always transfers control out of the
// enclosing function (return/raise, or a structurally diverging if/block/
// infinite-loop). Conservative: anything not provably diverging returns false,
// which preserves the existing merge behavior. (T1134)
func stmtDiverges(stmt ast.Stmt) bool {
	switch s := stmt.(type) {
	case *ast.ReturnStmt, *ast.RaiseStmt:
		return true
	case *ast.Block:
		return blockDiverges(s)
	case *ast.IfStmt:
		// Both arms must diverge (and an else must exist) for the if to never
		// fall through.
		return s.Else != nil && blockDiverges(s.Body) && stmtDiverges(s.Else)
	case *ast.InfiniteLoop:
		// An infinite loop without a break never falls through to following code.
		return !blockHasBreakOwnership(s.Body)
	default:
		return false
	}
}

// loopBodyExitsFunction reports whether a loop body never reaches the code after
// the loop via its own control flow: it always returns/raises (leaving the
// function) and contains neither a `break` nor a `continue` that could route
// control to post-loop code carrying the body's (possibly moved/borrowed)
// end-state. Only then is the pre-loop state the exact post-loop state. (T1134)
//
// `continue` matters as much as `break`: a body like
// `for x in xs { move(s); if c { continue; } return; }` can take the `continue`
// on every iteration, so the loop completes naturally and reaches post-loop code
// with `s` already moved — the divergent `return` never runs. Excluding such a
// body's end-state would be a use-after-move false negative, so the conservative
// merge must be kept whenever a break OR continue is present.
func loopBodyExitsFunction(body *ast.Block) bool {
	return blockDiverges(body) &&
		!blockHasBreakOwnership(body) &&
		!blockHasContinueOwnership(body)
}

// blockHasBreakOwnership reports whether a block contains a break that applies
// to an enclosing loop (not a nested loop's break). Mirror of sema's
// blockHasBreak, kept local to the ownership package. (T1134)
func blockHasBreakOwnership(block *ast.Block) bool {
	if block == nil {
		return false
	}
	for _, stmt := range block.Stmts {
		if stmtHasBreakOwnership(stmt) {
			return true
		}
	}
	return false
}

// stmtHasBreakOwnership reports whether a statement contains a break that
// applies to an enclosing loop. Does NOT recurse into nested loops since their
// breaks only apply to the inner loop. (T1134)
func stmtHasBreakOwnership(stmt ast.Stmt) bool {
	if stmt == nil {
		return false
	}
	switch s := stmt.(type) {
	case *ast.BreakStmt:
		return true
	case *ast.Block:
		return blockHasBreakOwnership(s)
	case *ast.IfStmt:
		if blockHasBreakOwnership(s.Body) {
			return true
		}
		return stmtHasBreakOwnership(s.Else)
	case *ast.ExprStmt:
		if me, ok := s.Expr.(*ast.MatchExpr); ok {
			for _, arm := range me.Arms {
				if arm.Block != nil && blockHasBreakOwnership(arm.Block) {
					return true
				}
			}
		}
		return false
	// Note: do NOT recurse into WhileStmt, ForInStmt, ClassicForStmt,
	// InfiniteLoop — break inside those only breaks the inner loop.
	default:
		return false
	}
}

// blockHasContinueOwnership reports whether a block contains a continue that
// applies to an enclosing loop (not a nested loop's continue). Mirror of the
// break helper. (T1134)
func blockHasContinueOwnership(block *ast.Block) bool {
	if block == nil {
		return false
	}
	for _, stmt := range block.Stmts {
		if stmtHasContinueOwnership(stmt) {
			return true
		}
	}
	return false
}

// stmtHasContinueOwnership reports whether a statement contains a continue that
// applies to an enclosing loop. Does NOT recurse into nested loops since their
// continues only apply to the inner loop. (T1134)
func stmtHasContinueOwnership(stmt ast.Stmt) bool {
	if stmt == nil {
		return false
	}
	switch s := stmt.(type) {
	case *ast.ContinueStmt:
		return true
	case *ast.Block:
		return blockHasContinueOwnership(s)
	case *ast.IfStmt:
		if blockHasContinueOwnership(s.Body) {
			return true
		}
		return stmtHasContinueOwnership(s.Else)
	case *ast.ExprStmt:
		if me, ok := s.Expr.(*ast.MatchExpr); ok {
			for _, arm := range me.Arms {
				if arm.Block != nil && blockHasContinueOwnership(arm.Block) {
					return true
				}
			}
		}
		return false
	// Note: do NOT recurse into WhileStmt, ForInStmt, ClassicForStmt,
	// InfiniteLoop — continue inside those only continues the inner loop.
	default:
		return false
	}
}
