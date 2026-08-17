package sema

import (
	"github.com/promise-language/promise/compiler/internal/ast"
	"github.com/promise-language/promise/compiler/internal/types"
)

// recordReturnHoldsReceiver sets the T1349 return-holds-receiver flag on sig when
// its body returns a value carrying the receiver via a captured-`this` lambda.
// Only meaningful for a non-Copy receiver: a Copy receiver is captured by value
// (an owned copy), so an escaping iterator over it is safe.
func (c *Checker) recordReturnHoldsReceiver(sig *types.Signature, body *ast.Block) {
	if sig == nil || body == nil {
		return
	}
	recv := sig.Recv()
	if recv == nil || isCopyField(recv.Type()) {
		return
	}
	if c.returnedExprHoldsThis(body) {
		sig.SetReturnHoldsReceiver(true)
	}
}

// returnedExprHoldsThis reports whether any top-level `return E` in body returns a
// value whose subtree contains a lambda that captures `this` — the
// `_FnIter[T](_next: move || { … this[i] … })` shape produced by `Vector.iter()`
// and every `Iterator` combinator (map/filter/flat_map/…). This is the
// definition-side half of the T1349 dangling-iterator escape check: combined with
// the receiver's ref-kind, ownership can decide at each call site whether the
// returned iterator borrows (RefNone/&this) or owns (~this) the receiver.
//
// Nested lambda / go-block bodies are NOT treated as return sites (their returns
// belong to a different function context), but a lambda appearing *as a returned
// value* is inspected for a `this` capture. The check is deliberately limited to
// the inline-lambda shape used by every std iterator; a method that hides the
// this-capture behind a helper function (`return build_iter(this)`) is not flagged
// — a documented limitation, not a soundness hole (the caller-side check stays
// conservative).
func (c *Checker) returnedExprHoldsThis(body *ast.Block) bool {
	w := &holdsThisWalker{c: c}
	w.walkBlock(body)
	return w.found
}

type holdsThisWalker struct {
	c     *Checker
	found bool
}

// lambdaCapturesThis reports whether lambda l captures the enclosing `this`.
func (w *holdsThisWalker) lambdaCapturesThis(l *ast.LambdaExpr) bool {
	for _, cv := range w.c.info.LambdaCaptures[l] {
		if cv.Obj != nil && cv.Obj.Name() == "this" {
			return true
		}
	}
	return false
}

// valueHoldsThis searches a returned VALUE expression for a `this`-capturing
// lambda, without descending into lambda bodies (a lambda's own body is a
// separate capture context; we only inspect the lambda node's captures).
func (w *holdsThisWalker) valueHoldsThis(e ast.Expr) bool {
	switch ex := e.(type) {
	case nil:
		return false
	case *ast.LambdaExpr:
		return w.lambdaCapturesThis(ex)
	case *ast.ParenExpr:
		return w.valueHoldsThis(ex.Expr)
	case *ast.CastExpr:
		return w.valueHoldsThis(ex.Expr)
	case *ast.CallExpr:
		if w.valueHoldsThis(ex.Callee) {
			return true
		}
		for _, a := range ex.Args {
			if w.valueHoldsThis(a.Value) {
				return true
			}
		}
	case *ast.TupleLit:
		for _, el := range ex.Elements {
			if w.valueHoldsThis(el) {
				return true
			}
		}
	case *ast.ArrayLit:
		for _, el := range ex.Elements {
			if w.valueHoldsThis(el) {
				return true
			}
		}
	case *ast.ArrayRepeatLit:
		if w.valueHoldsThis(ex.Value) {
			return true
		}
	}
	return false
}

// walkBlock walks each statement of a block looking for top-level returns.
func (w *holdsThisWalker) walkBlock(block *ast.Block) {
	if block == nil || w.found {
		return
	}
	for _, s := range block.Stmts {
		if w.found {
			return
		}
		w.walkStmt(s)
	}
}

// walkStmt recurses into every statement form that can (transitively) contain a
// top-level return, and tests the value of any return it finds. It stops at lambda
// / go boundaries (via walkExpr) so their returns are not attributed here.
func (w *holdsThisWalker) walkStmt(s ast.Stmt) {
	if w.found {
		return
	}
	switch st := s.(type) {
	case *ast.ReturnStmt:
		if st.Value != nil {
			if w.valueHoldsThis(st.Value) {
				w.found = true
				return
			}
			w.walkExpr(st.Value) // a return value may embed nested returns
		}
	case *ast.Block:
		w.walkBlock(st)
	case *ast.IfStmt:
		w.walkExpr(st.Cond)
		w.walkExpr(st.Init)
		w.walkBlock(st.Body)
		w.walkStmt(st.Else)
	case *ast.WhileStmt:
		w.walkExpr(st.Cond)
		w.walkBlock(st.Body)
	case *ast.WhileUnwrapStmt:
		w.walkExpr(st.Value)
		w.walkBlock(st.Body)
	case *ast.ForInStmt:
		w.walkExpr(st.Iterable)
		w.walkBlock(st.Body)
	case *ast.ClassicForStmt:
		w.walkExpr(st.InitValue)
		w.walkExpr(st.Cond)
		w.walkExpr(st.UpdateValue)
		w.walkBlock(st.Body)
	case *ast.InfiniteLoop:
		w.walkBlock(st.Body)
	case *ast.SelectStmt:
		for _, cs := range st.Cases {
			for _, cbs := range cs.Body {
				w.walkStmt(cbs)
			}
		}
		for _, ds := range st.Default {
			w.walkStmt(ds)
		}
	case *ast.ExprStmt:
		w.walkExpr(st.Expr)
	case *ast.TypedVarDecl:
		w.walkExpr(st.Value)
	case *ast.InferredVarDecl:
		w.walkExpr(st.Value)
	case *ast.DestructureVarDecl:
		w.walkExpr(st.Value)
	case *ast.UseVarDecl:
		w.walkExpr(st.Value)
	case *ast.AssignStmt:
		w.walkExpr(st.Value)
	}
}

// walkExpr descends into expression forms whose blocks share this function's
// return context (if/match/error-handler/unsafe) to reach nested returns. It STOPS
// at lambda and go blocks, whose returns belong to a different function.
func (w *holdsThisWalker) walkExpr(e ast.Expr) {
	if w.found {
		return
	}
	switch ex := e.(type) {
	case nil:
		return
	case *ast.IfExpr:
		w.walkBlock(ex.Then)
		w.walkBlock(ex.Else)
	case *ast.MatchExpr:
		for _, arm := range ex.Arms {
			w.walkExpr(arm.Body)
			w.walkBlock(arm.Block)
		}
	case *ast.ErrorHandlerExpr:
		w.walkExpr(ex.Expr)
		w.walkBlock(ex.Body)
		w.walkBlock(ex.ElseBody)
	case *ast.UnsafeExpr:
		w.walkBlock(ex.Body)
	case *ast.ParenExpr:
		w.walkExpr(ex.Expr)
	}
}
