package ownership

import (
	"fmt"
	"sort"

	"github.com/promise-language/promise/compiler/internal/ast"
	"github.com/promise-language/promise/compiler/internal/types"
)

// T1498 — loop-carried moves.
//
// Every loop checker walks its body EXACTLY ONCE, starting from the pre-loop
// state, then merges the body's end-state back (see mergeLoopState). A `move` of
// a name declared OUTSIDE the loop flips c.state[name] = Moved inside that single
// walk, which is correct for post-loop code but says nothing about the back edge:
// iteration 2 re-reaches the same move site with the variable already moved, and
// codegen happily emits a second consume of a freed value (double free, or — for
// a moved closure capture — a read of a freed string header and an unbounded
// allocation).
//
// The straight-line form of the same shape (`take(move s); take(move s);`) is
// already a hard "use of moved variable" error, so this is a hole in the
// back-edge coverage, not a semantic difference. Detection is a state comparison
// at the back edge rather than a new dataflow pass: after the body is checked and
// before mergeLoopState, any name that was Owned at loop entry, is Moved at body
// end, and has a recorded move site from which the back edge is reachable, is
// consumed on every iteration.
//
// Precision limits (biased toward false positives, per this package's existing
// convention — a rejected valid program is a diagnostic, a missed move is a
// use-after-free):
//   - Only the three move sites that call noteLoopMoveSite yield diagnostics; the
//     mustUse `<-t` discharge is not hooked, so that shape stays undetected
//     (false-negative direction, no regression).
//   - A move inside a `while` CONDITION is not reported: the condition is checked
//     before the entry-state snapshot, so the name already reads Moved at entry.
//     The classic-for condition and update clauses ARE covered (their snapshot is
//     taken before the header). Same false-negative direction.
//   - refcountedShare exempts every move of a Channel/Arc/Weak, including an
//     explicit `sink(move ch)` into a consuming parameter — which does transfer
//     the single refcount and so IS loop-carried. Narrowing that needs the move
//     site to carry whether it is a sharing or a consuming move (T1535).

// loopMoveSite is one recorded move of a name inside one loop body: the position
// drives the diagnostic and the back-edge reachability test, the type drives the
// refcountedShare carve-out.
type loopMoveSite struct {
	pos ast.Pos
	typ types.Type
}

// noteLoopMoveSite records that `name` is moved at `pos` inside every loop body
// currently being checked. No-op outside a loop.
//
// Every ENCLOSING frame gets the site, not just the innermost one: a move whose
// innermost loop body always leaves on its first iteration (`for … { take(move
// v); break; }`) still re-executes once per iteration of an OUTER loop, so the
// outer frame has to see it too. Duplicate positions are dropped so a site is
// recorded once per frame.
func (c *Checker) noteLoopMoveSite(name string, pos ast.Pos, typ types.Type) {
	if name == "" || name == "_" || len(c.loopFrames) == 0 {
		return
	}
	for _, frame := range c.loopFrames {
		if frame.moveSites == nil {
			frame.moveSites = make(map[string][]loopMoveSite)
		}
		dup := false
		for _, s := range frame.moveSites[name] {
			if s.pos == pos {
				dup = true
				break
			}
		}
		if !dup {
			frame.moveSites[name] = append(frame.moveSites[name], loopMoveSite{pos: pos, typ: typ})
		}
	}
}

// refcountedShare reports whether a "move" of this type is lowered as a refcount
// bump rather than an ownership transfer — Channel, Arc and Weak all share the
// pointer instead of consuming it (see expr_concurrency.go, "refcounted —
// sharing the pointer is fine"). Re-performing such a move on every iteration is
// sound, so these are exempt. Required for the `for v in ch` inside a `go {}`
// block nested in an outer loop shape used across tests/concurrency.
func refcountedShare(t types.Type) bool {
	if t == nil {
		return false
	}
	if _, ok := types.AsChannel(t); ok {
		return true
	}
	if _, ok := types.AsArc(t); ok {
		return true
	}
	if _, ok := types.AsWeak(t); ok {
		return true
	}
	return false
}

// movePathAlwaysLeavesLoop reports whether EVERY path from the move at `pos`
// leaves the enclosing loop — break/return/raise — before reaching the back
// edge, so the move runs at most once per entry into the loop.
//
// The walk descends through the statement list to the block that lexically
// contains `pos`, and at each level asks whether the rest of that block — the
// containing statement INCLUDED, since the move may sit inside the very
// `return`/`break` that leaves — always leaves. That covers both the body-tail
// form (`take(move v); break;`) and the far more common conditional form
// (`if done { take(move v); break; }`) that a whole-body predicate would reject.
//
// Conservative in the safe direction: an unlocatable position (invalid/renumbered
// spans, e.g. inside a string interpolation), a move inside a nested loop, or one
// inside a `go`/lambda body yields false — the move is assumed to reach the back
// edge, which flags a valid program at worst and never masks a double free. A
// nested loop is deliberately never descended into: a `break` in there leaves
// THAT loop, not this one, and the statements after it decide this back edge.
func movePathAlwaysLeavesLoop(stmts []ast.Stmt, pos ast.Pos) bool {
	idx := containingStmtIndex(stmts, pos)
	if idx < 0 {
		return false
	}
	if stmtsAlwaysLeaveLoop(stmts[idx:]) {
		return true
	}
	switch s := stmts[idx].(type) {
	case *ast.Block:
		return movePathAlwaysLeavesLoop(s.Stmts, pos)
	case *ast.IfStmt:
		// The move sits in the condition/init when neither arm contains it — the
		// header is re-evaluated every iteration, so it never escapes.
		if s.Body != nil && posWithin(s.Body, pos) {
			return movePathAlwaysLeavesLoop(s.Body.Stmts, pos)
		}
		if s.Else != nil && posWithin(s.Else, pos) {
			return movePathAlwaysLeavesLoop([]ast.Stmt{s.Else}, pos)
		}
		return false
	}
	// Match arms are deliberately not descended into: the grammar rejects
	// `break`/`return`/`raise` inside an arm block, so no arm can leave the loop.
	return false
}

// stmtsAlwaysLeaveLoop reports whether executing `stmts` in order always
// transfers control out of the enclosing loop without reaching its back edge.
// A `continue` anywhere in the sequence disqualifies it outright — continue
// jumps straight TO the back edge, which is exactly the edge under test (same
// reasoning as the continue barrier in loopFreshBoundNames).
func stmtsAlwaysLeaveLoop(stmts []ast.Stmt) bool {
	if len(stmts) == 0 {
		return false
	}
	for _, s := range stmts {
		if stmtHasContinueOwnership(s) {
			return false
		}
	}
	return stmtAlwaysLeavesLoop(stmts[len(stmts)-1])
}

// stmtAlwaysLeavesLoop is stmtDiverges (return/raise/no-break infinite loop)
// widened with `break`: leaving the LOOP is enough here, where the T1134 merge
// predicate needs the stronger "leaves the function". Nested loops are not
// inspected — a break inside one applies to that loop, not to ours.
func stmtAlwaysLeavesLoop(stmt ast.Stmt) bool {
	switch s := stmt.(type) {
	case *ast.BreakStmt:
		return true
	case *ast.Block:
		return stmtsAlwaysLeaveLoop(s.Stmts)
	case *ast.IfStmt:
		return s.Else != nil && s.Body != nil &&
			stmtsAlwaysLeaveLoop(s.Body.Stmts) && stmtAlwaysLeavesLoop(s.Else)
	}
	return stmtDiverges(stmt)
}

// containingStmtIndex returns the index of the statement whose source span
// contains `pos`, or -1. Statement spans within one list are ordered and
// non-overlapping, so at most one matches.
func containingStmtIndex(stmts []ast.Stmt, pos ast.Pos) int {
	for i, s := range stmts {
		if posWithin(s, pos) {
			return i
		}
	}
	return -1
}

// posWithin reports whether `pos` lies inside node `n`'s source span. Requires
// both endpoints to be valid and the files to match, so a node built without
// span information reads as "not containing" rather than as a wrong match.
func posWithin(n ast.Node, pos ast.Pos) bool {
	start, end := n.Pos(), n.End()
	if !start.IsValid() || !end.IsValid() || start.File != pos.File {
		return false
	}
	if pos.Line < start.Line || (pos.Line == start.Line && pos.Column < start.Column) {
		return false
	}
	if pos.Line > end.Line || (pos.Line == end.Line && pos.Column > end.Column) {
		return false
	}
	return true
}

// reportLoopCarriedMoves flags every name that the loop body moves on each
// iteration. Called after the body walk and before mergeLoopState, while the
// body's end-state is still live in c.state and the body's aliasLoopFrame is
// still on c.loopFrames.
//
// `entry` is the pre-body state clone; `skip` names bindings the loop itself
// re-establishes per iteration (for-in binding/index, while-let binding), which
// are set Owned before the clone is taken and so would otherwise look
// pre-existing.
func (c *Checker) reportLoopCarriedMoves(body *ast.Block, entry StateMap, skip ...string) {
	n := len(c.loopFrames)
	if n == 0 || body == nil {
		return
	}
	if c.loopMoveReported == nil {
		c.loopMoveReported = make(map[string]bool)
	}
	skipped := make(map[string]bool, len(skip))
	for _, s := range skip {
		skipped[s] = true
	}
	moveSites := c.loopFrames[n-1].moveSites
	// Sorted so a body with several loop-carried moves reports them in a stable
	// order (map iteration is randomized), same discipline as
	// reportUndischargedMustUse.
	names := make([]string, 0, len(moveSites))
	for name := range moveSites {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if skipped[name] || name == "this" || name == "_" {
			continue
		}
		// The `ok` is load-bearing: Owned is the zero value of VarState, so a
		// plain `entry[name] != Owned` would treat every loop-body-local and
		// match-arm binding as declared before the loop.
		st, declaredBeforeLoop := entry[name]
		if !declaredBeforeLoop || st != Owned || c.state[name] != Moved {
			continue
		}
		for _, site := range moveSites[name] {
			if refcountedShare(site.typ) || movePathAlwaysLeavesLoop(body.Stmts, site.pos) {
				continue
			}
			// One diagnostic per name per loop, at the first site the back edge
			// re-reaches. The name@pos dedup additionally keeps an enclosing loop
			// from repeating a site the inner loop already reported.
			key := fmt.Sprintf("%s@%s:%d:%d", name, site.pos.File, site.pos.Line, site.pos.Column)
			if !c.loopMoveReported[key] {
				c.loopMoveReported[key] = true
				c.errorf(site.pos, "use of moved variable '%s': it is moved on every iteration of this loop; declare it inside the loop body or reassign it after the move", name)
			}
			break
		}
	}
}
