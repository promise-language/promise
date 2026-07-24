package sema

import (
	"github.com/promise-language/promise/compiler/internal/ast"
	"github.com/promise-language/promise/compiler/internal/types"
)

// recordStructuralReturnAlias computes, for a free function whose result is a
// non-value structural interface, a per-parameter fact: could the function's
// return value alias parameter i's heap box? Codegen (T1305) consults this to
// decide whether a discarded/inline structural return of such a call is a
// fresh-owned box (safe to drop at statement end) or an alias of a still-owned
// argument (which the caller already frees — dropping it too would double-free).
//
// The classifier is deliberately sound toward OVER-marking: only two return
// shapes are treated as provably fresh (mark nothing) —
//   - a constructor call (`return Widget(...)`), and
//   - an owned (non-borrow) index read of a structural container, which is
//     deep-cloned on escape (T1292, `return v[0]`).
//
// Every other return shape marks its reachable parameters "may alias"; anything
// unrecognized marks ALL parameters. When a parameter is marked (or the fact is
// absent entirely) codegen keeps the conservative reject — a possible leak, never
// a double-free. Nested lambda / go-block bodies are NOT descended (their returns
// belong to a different function context).
func (c *Checker) recordStructuralReturnAlias(d *ast.FuncDecl, fn *types.Func, sig *types.Signature) {
	if d == nil || d.Body == nil || fn == nil || sig == nil {
		return
	}
	named := namedOfType(sig.Result())
	if named == nil || !named.IsStructural() || named.IsValueType() {
		return
	}
	if len(sig.Params()) == 0 {
		return
	}

	paramIndex := make(map[string]int, len(sig.Params()))
	for i, p := range sig.Params() {
		if p.Name() != "" && p.Name() != "_" {
			paramIndex[p.Name()] = i
		}
	}

	alias := make([]bool, len(sig.Params()))
	ra := &returnAliasWalker{c: c, paramIndex: paramIndex, alias: alias}
	ra.walkBlock(d.Body)
	c.info.StructuralReturnAliasParams[fn] = alias
}

// returnAliasWalker finds every return statement reachable from a function body
// (without crossing a lambda / go-block boundary) and classifies its value.
type returnAliasWalker struct {
	c          *Checker
	paramIndex map[string]int
	alias      []bool
}

// markAll marks every parameter as a possible alias source (conservative).
func (ra *returnAliasWalker) markAll() {
	for i := range ra.alias {
		ra.alias[i] = true
	}
}

// walkBlock walks each statement of a block looking for return statements.
func (ra *returnAliasWalker) walkBlock(block *ast.Block) {
	if block == nil {
		return
	}
	for _, s := range block.Stmts {
		ra.walkStmt(s)
	}
}

// walkStmt recurses into every statement form that can (transitively) contain a
// return statement, and classifies the value of any return it finds.
func (ra *returnAliasWalker) walkStmt(s ast.Stmt) {
	switch st := s.(type) {
	case *ast.ReturnStmt:
		if st.Value != nil {
			ra.markAliasedParams(st.Value)
			ra.walkExpr(st.Value) // a return value may itself embed nested returns
		}
	case *ast.Block:
		ra.walkBlock(st)
	case *ast.IfStmt:
		ra.walkExpr(st.Cond)
		ra.walkExpr(st.Init)
		ra.walkBlock(st.Body)
		ra.walkStmt(st.Else)
	case *ast.WhileStmt:
		ra.walkExpr(st.Cond)
		ra.walkBlock(st.Body)
	case *ast.WhileUnwrapStmt:
		ra.walkExpr(st.Value)
		ra.walkBlock(st.Body)
	case *ast.ForInStmt:
		ra.walkExpr(st.Iterable)
		ra.walkBlock(st.Body)
	case *ast.ClassicForStmt:
		ra.walkExpr(st.InitValue)
		ra.walkExpr(st.Cond)
		ra.walkExpr(st.UpdateValue)
		ra.walkBlock(st.Body)
	case *ast.InfiniteLoop:
		ra.walkBlock(st.Body)
	case *ast.SelectStmt:
		for _, cs := range st.Cases {
			ra.walkExpr(cs.Channel)
			ra.walkExpr(cs.SendValue)
			for _, cbs := range cs.Body {
				ra.walkStmt(cbs)
			}
		}
		for _, ds := range st.Default {
			ra.walkStmt(ds)
		}
	case *ast.ExprStmt:
		ra.walkExpr(st.Expr)
	case *ast.TypedVarDecl:
		ra.walkExpr(st.Value)
	case *ast.InferredVarDecl:
		ra.walkExpr(st.Value)
	case *ast.DestructureVarDecl:
		ra.walkExpr(st.Value)
	case *ast.UseVarDecl:
		ra.walkExpr(st.Value)
	case *ast.AssignStmt:
		ra.walkExpr(st.Value)
	case *ast.RaiseStmt:
		ra.walkExpr(st.Value)
	case *ast.YieldStmt:
		ra.walkExpr(st.Value)
	case *ast.YieldDelegateStmt:
		ra.walkExpr(st.Value)
	case *ast.IncDecStmt:
		// no nested block-bearing expression
	}
}

// walkExpr descends into expression forms whose blocks share this function's
// return context (if/match/error-handler/unsafe) to find nested return
// statements, and through composite expressions to reach them. It STOPS at
// lambda and go blocks, whose returns belong to a different function.
func (ra *returnAliasWalker) walkExpr(e ast.Expr) {
	switch ex := e.(type) {
	case nil:
		return
	case *ast.IfExpr:
		ra.walkBlock(ex.Then)
		ra.walkBlock(ex.Else)
	case *ast.MatchExpr:
		ra.walkExpr(ex.Subject)
		for _, arm := range ex.Arms {
			ra.walkExpr(arm.Guard)
			ra.walkExpr(arm.Body)
			ra.walkBlock(arm.Block)
		}
	case *ast.ErrorHandlerExpr:
		ra.walkExpr(ex.Expr)
		ra.walkBlock(ex.Body)
		ra.walkBlock(ex.ElseBody)
	case *ast.UnsafeExpr:
		ra.walkBlock(ex.Body)
	case *ast.BinaryExpr:
		ra.walkExpr(ex.Left)
		ra.walkExpr(ex.Right)
	case *ast.UnaryExpr:
		ra.walkExpr(ex.Operand)
	case *ast.CallExpr:
		ra.walkExpr(ex.Callee)
		for _, a := range ex.Args {
			ra.walkExpr(a.Value)
		}
	case *ast.IndexExpr:
		ra.walkExpr(ex.Target)
		ra.walkExpr(ex.Index)
		for _, xi := range ex.ExtraIndices {
			ra.walkExpr(xi)
		}
	case *ast.SliceExpr:
		ra.walkExpr(ex.Target)
		ra.walkExpr(ex.Low)
		ra.walkExpr(ex.High)
	case *ast.MemberExpr:
		ra.walkExpr(ex.Target)
	case *ast.OptionalChainExpr:
		ra.walkExpr(ex.Target)
	case *ast.CastExpr:
		ra.walkExpr(ex.Expr)
	case *ast.IsExpr:
		ra.walkExpr(ex.Expr)
	case *ast.ParenExpr:
		ra.walkExpr(ex.Expr)
	case *ast.ErrorPropagateExpr:
		ra.walkExpr(ex.Expr)
	case *ast.ErrorPanicExpr:
		ra.walkExpr(ex.Expr)
	case *ast.OptionalUnwrapExpr:
		ra.walkExpr(ex.Expr)
	case *ast.AutoCloneExpr:
		ra.walkExpr(ex.Expr)
	case *ast.TupleLit:
		for _, el := range ex.Elements {
			ra.walkExpr(el)
		}
	case *ast.ArrayLit:
		for _, el := range ex.Elements {
			ra.walkExpr(el)
		}
	case *ast.MapLit:
		for _, en := range ex.Entries {
			ra.walkExpr(en.Key)
			ra.walkExpr(en.Value)
		}
		// LambdaExpr and GoExpr are intentionally NOT descended: their bodies are
		// separate return contexts. Leaf literals/idents have no nested returns.
	}
}

// markAliasedParams classifies a single return VALUE expression: which parameters
// could its result alias? It marks nothing only for provably-fresh shapes.
func (ra *returnAliasWalker) markAliasedParams(e ast.Expr) {
	switch ex := e.(type) {
	case nil:
		return
	case *ast.ParenExpr:
		ra.markAliasedParams(ex.Expr)
	case *ast.CastExpr:
		// A view cast aliases its subject.
		ra.markAliasedParams(ex.Expr)
	case *ast.IfExpr:
		ra.markBranch(ex.Then)
		ra.markBranch(ex.Else)
	case *ast.MatchExpr:
		for _, arm := range ex.Arms {
			if arm.Body != nil {
				ra.markAliasedParams(arm.Body)
			}
			if arm.Block != nil {
				ra.markBranch(arm.Block)
			}
		}
	case *ast.IdentExpr:
		if idx, ok := ra.paramIndex[ex.Name]; ok {
			ra.alias[idx] = true
			return
		}
		// A local: no provenance analysis in v1 — conservatively assume it could
		// alias any parameter (leak on discard, never a double-free).
		ra.markAll()
	case *ast.IndexExpr:
		// An owned (non-borrow) index read of a structural container is deep-cloned
		// on escape (T1292) — a fresh box, aliases nothing. A borrow-typed read
		// aliases the container root.
		if ra.isBorrowType(ra.c.info.Types[ex]) {
			ra.markRoot(ex)
		}
	case *ast.MemberExpr:
		// A borrow-typed member read aliases the owner root. A direct owned field
		// read of a heap/structural field is itself an alias of the owner's field
		// (not cloned), so mark the root conservatively.
		ra.markRoot(ex)
	case *ast.CallExpr:
		if ra.isConstructorCall(ex) {
			return // fresh construction, aliases nothing
		}
		ra.markAll()
	default:
		ra.markAll()
	}
}

// markBranch classifies the tail (value) expression of a block-valued branch.
// A branch that diverges (its last statement is return/raise/break/continue)
// produces no value and contributes no alias; its return is walked separately.
// An indeterminate tail is treated conservatively (mark all).
func (ra *returnAliasWalker) markBranch(block *ast.Block) {
	if block == nil {
		return
	}
	if tail := blockValueExpr(block); tail != nil {
		ra.markAliasedParams(tail)
		return
	}
	if !lastStmtDiverges(block) {
		ra.markAll()
	}
}

// markRoot walks an index/member/paren/cast chain down to its base identifier and
// marks it if it is a parameter; otherwise (a local or non-ident base) marks all.
func (ra *returnAliasWalker) markRoot(e ast.Expr) {
	for {
		switch ex := e.(type) {
		case *ast.ParenExpr:
			e = ex.Expr
		case *ast.CastExpr:
			e = ex.Expr
		case *ast.IndexExpr:
			e = ex.Target
		case *ast.SliceExpr:
			e = ex.Target
		case *ast.MemberExpr:
			e = ex.Target
		case *ast.OptionalChainExpr:
			e = ex.Target
		case *ast.IdentExpr:
			if idx, ok := ra.paramIndex[ex.Name]; ok {
				ra.alias[idx] = true
				return
			}
			ra.markAll()
			return
		default:
			ra.markAll()
			return
		}
	}
}

// isBorrowType reports whether t is a borrow reference (T& / T~).
func (ra *returnAliasWalker) isBorrowType(t types.Type) bool {
	switch t.(type) {
	case *types.SharedRef, *types.MutRef:
		return true
	}
	return false
}

// isConstructorCall reports whether call is a type/enum constructor invocation
// (its callee resolves to a Named/Instance type), which always produces a fresh
// owned value.
func (ra *returnAliasWalker) isConstructorCall(call *ast.CallExpr) bool {
	t := ra.c.info.Types[call.Callee]
	switch t.(type) {
	case *types.Named, *types.Instance:
		return true
	}
	return false
}

// blockValueExpr returns the tail (value) expression of a block — the expression
// of its last expression statement — mirroring blockValueType. Returns nil when
// the block does not end in a value expression (e.g. it diverges).
func blockValueExpr(block *ast.Block) ast.Expr {
	if block == nil || len(block.Stmts) == 0 {
		return nil
	}
	last := block.Stmts[len(block.Stmts)-1]
	if es, ok := last.(*ast.ExprStmt); ok {
		return es.Expr
	}
	if ifS, ok := last.(*ast.IfStmt); ok && ifS.Else != nil {
		return blockValueExpr(ifS.Body)
	}
	return nil
}

// lastStmtDiverges reports whether a block's last statement transfers control out
// of the block (so the block yields no fall-through value).
func lastStmtDiverges(block *ast.Block) bool {
	if block == nil || len(block.Stmts) == 0 {
		return false
	}
	switch block.Stmts[len(block.Stmts)-1].(type) {
	case *ast.ReturnStmt, *ast.RaiseStmt, *ast.BreakStmt, *ast.ContinueStmt:
		return true
	}
	return false
}
