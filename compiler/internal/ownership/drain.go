package ownership

import (
	"github.com/promise-language/promise/compiler/internal/ast"
	"github.com/promise-language/promise/compiler/internal/types"
)

// drainReq is a deferred for-in-drain requirement: a generic body moves a
// for-in loop binding (over a native container whose element type is a bare
// TypeParam) out of the loop. Whether that move double-frees depends on the
// concrete type the TypeParam is bound to at each call site — a non-Copy,
// non-string element aliases the container's droppable storage and is dropped
// twice (source container + drain target). The verdict is therefore deferred to
// each concrete instantiation, validated by propagateDrainReqs against the
// existing GenericCallEdges (same mechanism as T0616 cloneability reqs). (T1035)
type drainReq struct {
	TypeParam types.Type // the for-in element TypeParam (substituted at call site)
	Pos       ast.Pos    // location of the move-out inside the generic body
	Binding   string     // the for-in loop variable name (for the error message)
}

// recordDrainReq appends a deferred drain requirement to the generic function or
// method currently being checked. No-op when not inside a generic body (a move
// of a for-in binding over a concrete container is already handled inline by the
// forInAliasBindings path). Deduped on (TypeParam, Pos, Binding). (T1035)
func (c *Checker) recordDrainReq(tp types.Type, pos ast.Pos, binding string) {
	if tp == nil {
		return
	}
	req := drainReq{TypeParam: tp, Pos: pos, Binding: binding}
	if c.curMethodObj != nil {
		for _, r := range c.methodDrainReqs[c.curMethodObj] {
			if r.Pos == pos && r.Binding == binding && types.Identical(r.TypeParam, tp) {
				return
			}
		}
		c.methodDrainReqs[c.curMethodObj] = append(c.methodDrainReqs[c.curMethodObj], req)
		return
	}
	if c.curFuncObj != nil {
		for _, r := range c.funcDrainReqs[c.curFuncObj] {
			if r.Pos == pos && r.Binding == binding && types.Identical(r.TypeParam, tp) {
				return
			}
		}
		c.funcDrainReqs[c.curFuncObj] = append(c.funcDrainReqs[c.curFuncObj], req)
	}
}

// propagateDrainReqs validates deferred for-in-drain requirements against
// concrete instantiations, propagating transitively across generic call edges to
// a fixed point. Structurally mirrors sema's propagateCloneReqs (T0616): for each
// recorded GenericCallEdge, substitute the callee's drainReq TypeParam through
// the edge's subst map; if the result is concrete and aliases the container, emit
// an error at the call site; if it is still a TypeParam, forward the requirement
// onto the caller so a transitive chain (`outer[U]` → `drain[U]`, instantiated
// `outer[Item]`) is caught at the outer concrete call site. (T1035)
func (c *Checker) propagateDrainReqs() {
	if len(c.info.GenericCallEdges) == 0 {
		return
	}
	emitted := make(map[string]bool)
	for iter := 0; iter < 64; iter++ {
		changed := false
		for _, edge := range c.info.GenericCallEdges {
			var calleeReqs []drainReq
			if edge.CalleeFunc != nil {
				calleeReqs = c.funcDrainReqs[edge.CalleeFunc]
			} else if edge.CalleeMethod != nil {
				calleeReqs = c.methodDrainReqs[edge.CalleeMethod]
			}
			if len(calleeReqs) == 0 {
				continue
			}
			for _, req := range calleeReqs {
				substituted := types.Substitute(req.TypeParam, edge.Subst)
				if types.ContainsTypeParam(substituted) {
					// Still generic — forward onto the caller so the eventual
					// concrete call site triggers validation.
					if c.addDrainReq(edge.CallerFunc, edge.CallerMethod,
						drainReq{TypeParam: substituted, Pos: req.Pos, Binding: req.Binding}) {
						changed = true
					}
					continue
				}
				if !concreteElementAliasesContainer(substituted) {
					// Copy / string instantiation — freely movable, no double-free.
					continue
				}
				key := edge.CallPos.String() + "|" + req.Binding + "|" + substituted.String()
				if emitted[key] {
					continue
				}
				emitted[key] = true
				c.errorf(edge.CallPos,
					"cannot instantiate generic with %s: the for-in loop binding '%s' (at %s) is moved out of its container, but %s aliases the container's element storage, so moving it double-frees when the container drops its elements. Call `.clone()` to take an independent copy, or `.pop()` / `.remove()` to take ownership of an element",
					substituted, req.Binding, req.Pos, substituted)
			}
		}
		if !changed {
			return
		}
	}
}

// addDrainReq forwards req onto the caller's requirement set if not already
// present (dedup on TypeParam/Pos/Binding). Returns true if a new requirement
// was added. (T1035)
func (c *Checker) addDrainReq(fn *types.Func, method *types.Method, req drainReq) bool {
	if fn != nil {
		for _, existing := range c.funcDrainReqs[fn] {
			if existing.Pos == req.Pos && existing.Binding == req.Binding &&
				types.Identical(existing.TypeParam, req.TypeParam) {
				return false
			}
		}
		c.funcDrainReqs[fn] = append(c.funcDrainReqs[fn], req)
		return true
	}
	if method != nil {
		for _, existing := range c.methodDrainReqs[method] {
			if existing.Pos == req.Pos && existing.Binding == req.Binding &&
				types.Identical(existing.TypeParam, req.TypeParam) {
				return false
			}
		}
		c.methodDrainReqs[method] = append(c.methodDrainReqs[method], req)
		return true
	}
	return false
}
