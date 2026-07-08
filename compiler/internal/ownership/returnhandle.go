package ownership

import (
	"github.com/promise-language/promise/compiler/internal/ast"
	"github.com/promise-language/promise/compiler/internal/types"
)

// returnHandleReq is a deferred return-borrowed-handle requirement: a generic
// body directly returns a borrowed (non-`move`) parameter whose static type is
// a (bare or Optional-wrapping) TypeParam. Whether returning it is unsound
// depends on the instantiation — an Optional-wrapped single-owner handle
// (Mutex[int]?, task[int]??, …) aliases the caller's live value and double-frees
// (dupOptionalVectorElem shares the one handle and leaves the source drop flag
// set), while a bare handle is made safe by codegen's return-alias flag
// clearing. The verdict is therefore deferred to each concrete instantiation,
// validated by propagateReturnHandleReqs against the existing GenericCallEdges
// (same mechanism as the T1035 for-in-drain reqs). (T1213)
type returnHandleReq struct {
	ParamType types.Type // the returned param's TypeParam type (substituted at call site)
	Pos       ast.Pos    // location of the `return x` inside the generic body
	Binding   string     // the returned parameter name (for the error message)
}

// optionalWrappedSingleOwnerHandle returns the handle kind when t is a
// single-owner handle wrapped in >=1 Optional layer (Mutex[int]?, task[int]??),
// else "". A BARE handle (depth 0) is intentionally excluded: codegen's
// return-alias check clears the source drop flag for it, so the bare-handle
// generic instantiation is safe (t1102_generic_identity_mutex). Only the
// Optional-wrapped form hits the unfixable dupOptionalVectorElem path. T1213.
func optionalWrappedSingleOwnerHandle(t types.Type) string {
	if optionalDepthType(t) == 0 {
		return ""
	}
	return singleOwnerHandleKind(peelOptional(t))
}

// recordReturnHandleReq appends a deferred return-handle requirement to the
// generic function or method currently being checked. No-op when not inside a
// generic body. Deduped on (Pos, Binding, ParamType). (T1213)
func (c *Checker) recordReturnHandleReq(pt types.Type, pos ast.Pos, binding string) {
	if pt == nil {
		return
	}
	req := returnHandleReq{ParamType: pt, Pos: pos, Binding: binding}
	if c.curMethodObj != nil {
		for _, r := range c.methodReturnHandleReqs[c.curMethodObj] {
			if r.Pos == pos && r.Binding == binding && types.Identical(r.ParamType, pt) {
				return
			}
		}
		c.methodReturnHandleReqs[c.curMethodObj] = append(c.methodReturnHandleReqs[c.curMethodObj], req)
		return
	}
	if c.curFuncObj != nil {
		for _, r := range c.funcReturnHandleReqs[c.curFuncObj] {
			if r.Pos == pos && r.Binding == binding && types.Identical(r.ParamType, pt) {
				return
			}
		}
		c.funcReturnHandleReqs[c.curFuncObj] = append(c.funcReturnHandleReqs[c.curFuncObj], req)
	}
}

// propagateReturnHandleReqs validates deferred return-handle requirements
// against concrete instantiations, propagating transitively across generic call
// edges to a fixed point. Structurally mirrors propagateDrainReqs (T1035): for
// each recorded GenericCallEdge, substitute the callee's returnHandleReq
// ParamType through the edge's subst map; if the result is still generic,
// forward the requirement onto the caller so a transitive chain (`outer[V]` →
// `mid[U]`, instantiated `outer[Mutex[int]?]`) is caught at the outer concrete
// call site; if the result is an Optional-wrapped single-owner handle, emit an
// error at the call site; otherwise (bare handle, int, string, Vector, …) the
// return is safe, so skip. (T1213)
func (c *Checker) propagateReturnHandleReqs() {
	if len(c.info.GenericCallEdges) == 0 {
		return
	}
	emitted := make(map[string]bool)
	for iter := 0; iter < 64; iter++ {
		changed := false
		for _, edge := range c.info.GenericCallEdges {
			var calleeReqs []returnHandleReq
			if edge.CalleeFunc != nil {
				calleeReqs = c.funcReturnHandleReqs[edge.CalleeFunc]
			} else if edge.CalleeMethod != nil {
				calleeReqs = c.methodReturnHandleReqs[edge.CalleeMethod]
			}
			if len(calleeReqs) == 0 {
				continue
			}
			for _, req := range calleeReqs {
				substituted := types.Substitute(req.ParamType, edge.Subst)
				if types.ContainsTypeParam(substituted) {
					// Still generic — forward onto the caller so the eventual
					// concrete call site triggers validation.
					if c.addReturnHandleReq(edge.CallerFunc, edge.CallerMethod,
						returnHandleReq{ParamType: substituted, Pos: req.Pos, Binding: req.Binding}) {
						changed = true
					}
					continue
				}
				k := optionalWrappedSingleOwnerHandle(substituted)
				if k == "" {
					// Bare handle (codegen return-alias clears the source flag),
					// or a freely-returnable type (int/string/Vector/heap user
					// type) — no double-free.
					continue
				}
				key := edge.CallPos.String() + "|" + req.Binding + "|" + substituted.String()
				if emitted[key] {
					continue
				}
				emitted[key] = true
				c.errorf(edge.CallPos,
					"cannot instantiate generic with %s: the parameter '%s' is returned as owned (at %s), but a `%s` handle has no clone and the caller still owns it — returning it aliases the caller's live value and double-frees. Declare the parameter with `move` to transfer ownership",
					substituted, req.Binding, req.Pos, k)
			}
		}
		if !changed {
			return
		}
	}
}

// addReturnHandleReq forwards req onto the caller's requirement set if not
// already present (dedup on Pos/Binding/ParamType). Returns true if a new
// requirement was added. (T1213)
func (c *Checker) addReturnHandleReq(fn *types.Func, method *types.Method, req returnHandleReq) bool {
	if fn != nil {
		for _, existing := range c.funcReturnHandleReqs[fn] {
			if existing.Pos == req.Pos && existing.Binding == req.Binding &&
				types.Identical(existing.ParamType, req.ParamType) {
				return false
			}
		}
		c.funcReturnHandleReqs[fn] = append(c.funcReturnHandleReqs[fn], req)
		return true
	}
	if method != nil {
		for _, existing := range c.methodReturnHandleReqs[method] {
			if existing.Pos == req.Pos && existing.Binding == req.Binding &&
				types.Identical(existing.ParamType, req.ParamType) {
				return false
			}
		}
		c.methodReturnHandleReqs[method] = append(c.methodReturnHandleReqs[method], req)
		return true
	}
	return false
}
