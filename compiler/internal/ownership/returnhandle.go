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
	Pos       ast.Pos    // location of the `return x` / launder inside the generic body
	Binding   string     // the source parameter name (for the error message)
	Laundered bool       // T1214: recorded at a launder binding (`T y = x`), not a direct return
}

// aliasHandleReuse records a caller-side single-owner handle local passed by
// borrow as an argument to a (possibly generic) call that may return that
// param. T1137: for a BARE (depth-0) handle the callee's return aliases the
// source local; codegen's return-alias flag clear makes a SINGLE use of the
// result sound, but reusing the source local after the call is a
// use-after-free (the handle was already consumed by the first await). The
// verdict is deferred to propagateReturnHandleReqs, which fires only when the
// callee's returnHandleReq.Binding names this exact param (calleeParam) AND the
// source was reused. Matching on calleeParam ties the reused local to the
// specific returned param — `pick_first(t1,t2)` returning `a` rejects reusing
// t1 but allows reusing t2.
type aliasHandleReuse struct {
	calleeParam string // callee param name receiving this arg (matches returnHandleReq.Binding)
	localName   string // caller-side local passed as that arg
	kind        string // "task"/"Mutex"/"MutexGuard"
	reused      bool   // set when localName is used again after the call
}

// recordAliasHandleReuseCandidates records, for each BARE single-owner-handle
// argument passed by borrow into a call, a reuse candidate keyed by the call
// position. If the callee turns out to return that exact param (a
// returnHandleReq whose Binding matches the candidate's calleeParam) and the
// caller reuses the source local after the call, propagateReturnHandleReqs
// converts the resulting UAF into a compile error. Recording is harmless for
// concrete callees and for callees that don't return the arg: they carry no
// matching returnHandleReq, so the post-pass never fires. Only bare (depth-0)
// handles are recorded — Optional-wrapped and non-handle types are handled by
// the existing T1213/T1214 rejections. Move/consuming params (`~`, RefMut,
// variadic, explicit call-site `move`) are skipped: they consume the arg, so a
// later use is already rejected as a moved-variable use. T1137.
func (c *Checker) recordAliasHandleReuseCandidates(e *ast.CallExpr, sig *types.Signature) {
	params := sig.Params()
	for i, arg := range e.Args {
		if i >= len(params) {
			break
		}
		p := params[i]
		if arg.Move || paramBorrowKind(p) == BorrowMut || p.Ref() == types.RefMut || p.IsVariadic() {
			continue // consumed — a later use is already a moved-variable error
		}
		id, ok := arg.Value.(*ast.IdentExpr)
		if !ok {
			continue
		}
		if _, tracked := c.state[id.Name]; !tracked {
			continue
		}
		t := c.info.Types[id]
		k := singleOwnerHandleKind(t)
		if k == "" || optionalDepthType(t) != 0 {
			continue // not a BARE (depth-0) single-owner handle
		}
		cand := &aliasHandleReuse{calleeParam: p.Name(), localName: id.Name, kind: k}
		c.aliasHandleReuses[e.Pos()] = append(c.aliasHandleReuses[e.Pos()], cand)
		c.pendingAliasLocals[id.Name] = append(c.pendingAliasLocals[id.Name], cand)
	}
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

// recordLaunderedHandleReq records a deferred handle requirement when `value`
// aliases ("launders") a borrowed (non-`move`) parameter of still-generic
// TypeParam type into a fresh binding (`T y = x`, `y := x`, or a `_` discard).
// Concretely such a launder is already rejected outright ("cannot move borrowed
// parameter"), but the generic body is checked once with `T` unbound, so no
// inline reject fires. At each concrete instantiation with an Optional-wrapped
// single-owner handle, the owned alias double-frees the caller's live handle at
// its scope-exit drop (or when consumed) — independent of whether it is later
// returned. Defer the verdict to each concrete call site via the same
// GenericCallEdges machinery as the direct-return T1213 case. Bare handles
// (depth 0) and non-handle types are made safe / are freely copyable, so
// propagateReturnHandleReqs skips them. Applies equally to a `_` discard
// binding: codegen still materializes a droppable owned temp for the discard,
// so the alias double-frees at scope exit exactly as for a named local.
// (T1214/T1216)
func (c *Checker) recordLaunderedHandleReq(value ast.Expr, pos ast.Pos) {
	src, ok := value.(*ast.IdentExpr)
	if !ok {
		return
	}
	if !c.params[src.Name] || c.state[src.Name] != Borrowed {
		return
	}
	pt := c.info.Types[src]
	if !types.ContainsTypeParam(pt) {
		return
	}
	c.recordReturnHandleReq2(pt, pos, src.Name, true)
}

// recordReturnHandleReq appends a deferred return-handle requirement for the
// direct-return T1213 shape (`return x`). See recordReturnHandleReq2.
func (c *Checker) recordReturnHandleReq(pt types.Type, pos ast.Pos, binding string) {
	c.recordReturnHandleReq2(pt, pos, binding, false)
}

// recordReturnHandleReq2 appends a deferred return-handle requirement to the
// generic function or method currently being checked. No-op when not inside a
// generic body. Deduped on (Pos, Binding, ParamType). `laundered` distinguishes
// the launder-binding shape (T1214) from the direct return (T1213) for the error
// message. (T1213/T1214)
func (c *Checker) recordReturnHandleReq2(pt types.Type, pos ast.Pos, binding string, laundered bool) {
	if pt == nil {
		return
	}
	req := returnHandleReq{ParamType: pt, Pos: pos, Binding: binding, Laundered: laundered}
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
						returnHandleReq{ParamType: substituted, Pos: req.Pos, Binding: req.Binding, Laundered: req.Laundered}) {
						changed = true
					}
					continue
				}
				k := optionalWrappedSingleOwnerHandle(substituted)
				if k == "" {
					// T1137: a depth-0 BARE single-owner handle. A single use of
					// the result is made safe by codegen's return-alias flag clear;
					// reusing the source local after the aliasing call is a UAF (the
					// handle was already consumed by that clear). Emit ONLY when the
					// caller reuses the source local matched to the specific returned
					// param (calleeParam == req.Binding gives the pick_first
					// precision). A freely-returnable type (int/string/Vector/heap
					// user type) has bareK == "" and is skipped entirely.
					//
					// Restricted to the DIRECT-return shape (`return x`, !Laundered):
					// only that path routes through codegen's source-drop-flag clear,
					// which is what consumes the source and makes a later reuse a UAF.
					// The LAUNDER shape (`T y = x; …`) is T1214's domain — its
					// bare-handle instantiations do NOT consume the source (the
					// discard/launder form returns without aliasing it into the
					// caller's result, so the source stays owned and reuse is sound —
					// t1214_launder_discard_bare_mutex, t1216_discard_*_bare_mutex),
					// and its genuinely-unsound Optional-wrapped forms are already
					// rejected by the k != "" branch below.
					if bareK := singleOwnerHandleKind(substituted); bareK != "" && !req.Laundered {
						for _, cand := range c.aliasHandleReuses[edge.CallPos] {
							if cand.calleeParam != req.Binding || !cand.reused {
								continue
							}
							key := edge.CallPos.String() + "|reuse|" + cand.localName
							if emitted[key] {
								continue
							}
							emitted[key] = true
							c.errorf(edge.CallPos,
								"cannot reuse %s handle '%s' after '%s' returns it as owned (at %s): the aliasing call consumed the handle and a `%s` handle has no clone — use the result exactly once, or declare the parameter with `move` to transfer ownership",
								bareK, cand.localName, req.Binding, req.Pos, bareK)
						}
					}
					continue
				}
				key := edge.CallPos.String() + "|" + req.Binding + "|" + substituted.String()
				if emitted[key] {
					continue
				}
				emitted[key] = true
				if req.Laundered {
					// T1214: the param was aliased into an owned local (`T y = x`).
					// The owned alias double-frees the caller's live handle at its
					// scope-exit drop (or when consumed), independent of return.
					c.errorf(edge.CallPos,
						"cannot instantiate generic with %s: the parameter '%s' is aliased into an owned local (at %s), but a `%s` handle has no clone and the caller still owns it — the alias double-frees the caller's live value. Declare the parameter with `move` to transfer ownership",
						substituted, req.Binding, req.Pos, k)
					continue
				}
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
