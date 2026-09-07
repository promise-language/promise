package codegen

import (
	"fmt"

	"github.com/llir/llvm/ir"
	"github.com/llir/llvm/ir/constant"
	irtypes "github.com/llir/llvm/ir/types"
	"github.com/llir/llvm/ir/value"

	"github.com/promise-language/promise/compiler/internal/types"
)

// T1885 — value-level shims for the `native` container/handle operations.
//
// A `native` method has no LLVM body: it is open-coded at each AST call site. That
// is fine for a direct call, but a vtable slot needs a real function pointer, and a
// heap box needs a real drop_fn/clone_fn. Both need the operation expressed over a
// receiver VALUE rather than an *ast.CallExpr. These helpers are that expression,
// and every path that duplicates a native container/handle — the AST call sites, the
// variant/field dup walk, the view vtable shim, the structural box clone — routes
// through them, so there is exactly one implementation of each operation.

// dupVectorDeep produces an independently-owned copy of the vector at ptr: a shallow
// header+element memcpy (dupVector) followed by a deep clone of every non-copy
// element (B0275), so the copy shares nothing with the original. Null-safe.
func (c *Compiler) dupVectorDeep(ptr value.Value, elemType types.Type) value.Value {
	if c.typeSubst != nil {
		elemType = types.Substitute(elemType, c.typeSubst)
	}
	dup := c.dupVector(ptr, int64(c.typeSize(c.resolveType(elemType))))
	c.emitVectorElementCloneLoopNullable(dup, elemType)
	return dup
}

// nativeDupSupported reports whether emitNativeDupValue knows how to produce an
// independently-owned copy of a value of typ. False for the single-owner handles
// (Mutex, MutexGuard, Task), which own exactly one allocation and have no dup
// semantics at all (T1113).
//
// The cases below must stay in lockstep with emitNativeDupValue's — a predicate
// that admits a type the emitter then declines would leave the caller holding a
// nil value, and one that rejects a type the emitter handles silently downgrades
// an owning box to an aliasing one.
func nativeDupSupported(typ types.Type) bool {
	named := extractNamed(typ)
	if named == types.TypString {
		return true
	}
	if _, ok := types.AsVector(typ); ok {
		return true
	}
	if _, ok := types.AsArc(typ); ok || named == types.TypArc {
		return true
	}
	if _, ok := types.AsWeak(typ); ok {
		return true
	}
	if _, ok := types.AsChannel(typ); ok || named == types.TypChannel {
		return true
	}
	return false
}

// emitNativeDupValue emits, against the current block, the operation that produces
// an independently-owned copy of the native container/handle value recv, returning
// the copy. Reports false when typ has no dup semantics (see nativeDupSupported) —
// the caller must then arrange not to need one.
//
// This is the body of `string.clone`, `Vector[T].clone`, `Ref[T].clone` and
// `Weak[T].clone` (the four `native` clones in modules/std), plus the channel
// refcount bump the field/variant dup walk needs.
func (c *Compiler) emitNativeDupValue(recv value.Value, typ types.Type) (value.Value, bool) {
	if c.typeSubst != nil {
		typ = types.Substitute(typ, c.typeSubst)
	}
	named := extractNamed(typ)
	if named == types.TypString {
		return c.dupString(recv), true
	}
	if elemType, ok := types.AsVector(typ); ok {
		return c.dupVectorDeep(recv, elemType), true
	}
	if elemType, ok := types.AsArc(typ); ok || named == types.TypArc {
		return c.dupArc(recv, elemType), true
	}
	if elemType, ok := types.AsWeak(typ); ok {
		return c.dupWeak(recv, elemType), true
	}
	if _, ok := types.AsChannel(typ); ok || named == types.TypChannel {
		return c.dupChannel(recv), true
	}
	return nil, false
}

// nativeShimName is the LLVM name of the synthesized concrete-signature function
// standing in for a `native` method on the concrete keyed by `key`. The `$native`
// suffix keeps it out of every mangleMethodName lookup, so no existing dispatch
// path can pick it up by accident.
func nativeShimName(key, method string) string {
	return fmt.Sprintf("%s.%s$native", key, method)
}

// getOrEmitNativeMethodFunc emits (once) a real LLVM function with the CONCRETE
// method's signature for a `native` method whose operation emitNativeDupValue
// knows — today `clone() Self` on string, Vector[T], Ref[T] and Weak[T].
//
// T1885: a `native` method has no body, so getOrEmitViewVtable's c.funcs lookup
// missed and the slot was emitted as null; calling `clone()` through a boxed
// `Cloneable` then jumped to address 0. With a real function the slot is filled and
// the ordinary adapter machinery (receiver unbox + covariant return re-box) applies
// unchanged.
func (c *Compiler) getOrEmitNativeMethodFunc(key string, fromType types.Type, m *types.Method) (*ir.Func, bool) {
	sig := m.Sig()
	if m.Name() != "clone" || m.IsGetter() || m.IsSetter() ||
		sig.Recv() == nil || len(sig.Params()) != 0 || sig.CanError() {
		return nil, false
	}
	resolved := fromType
	if c.typeSubst != nil {
		resolved = types.Substitute(resolved, c.typeSubst)
	}
	if !nativeDupSupported(resolved) {
		return nil, false
	}
	name := nativeShimName(key, m.Name())
	if fn, ok := c.funcs[name]; ok {
		return fn, true
	}

	// All four concretes are opaque i8* handles (string included), so the concrete
	// signature is i8* (i8*).
	recv := ir.NewParam("this", irtypes.I8Ptr)
	fn := c.module.NewFunc(name, irtypes.I8Ptr, recv)
	c.funcs[name] = fn

	saved := c.saveState()
	defer c.restoreState(saved)
	c.beginSynthesizedBody(fn)

	dup, ok := c.emitNativeDupValue(recv, resolved)
	if !ok {
		// nativeDupSupported already vetted this; keep the IR well-formed regardless.
		dup = recv
	}
	c.block.NewRet(dup)
	return fn, true
}

// beginSynthesizedBody prepares the compiler to emit a fresh function body that is
// NOT a continuation of the caller's. saveState() must already have been called:
// it saves the caller's temp/scope state but does not clear it, so without this the
// synthesized body's temps would land in the caller's lists (discarded by
// restoreState → leak) and any unwind path would walk the caller's scopeBindings,
// emitting loads of the caller's allocas inside this function (malformed IR).
// Mirrors the reset emitViewMethodAdapter performs for the same reason (T1460).
func (c *Compiler) beginSynthesizedBody(fn *ir.Func) {
	c.fn = fn
	c.locals = make(map[string]*ir.InstAlloca)
	c.localNameCount = make(map[string]int)
	c.scopeBindings = nil
	c.dropFlags = make(map[string]*ir.InstAlloca)
	c.dropBindings = make(map[string]scopeBinding)
	c.castSubjectMatch = nil
	c.forInHandleSlotPtr = make(map[string]*ir.InstAlloca)
	c.lambdaWritebacks = nil
	c.loopScopeDepth = 0
	c.canError = false
	c.currentRetType = nil
	c.stmtTemps = nil
	c.stmtTempMap = make(map[value.Value]int)
	c.heapTemps = nil
	c.heapTempMap = make(map[value.Value]int)
	c.envTemps = nil
	c.envTempMap = make(map[value.Value]int)
	c.enumCtorTemps = nil
	c.tempTrackingEnabled = true

	entry := fn.NewBlock(".entry")
	c.block = entry
	c.entryBlock = entry
}

// emitPanicCString emits a promise_panic call with a literal message.
func (c *Compiler) emitPanicCString(msg string) {
	g := c.getCStrGlobal(msg)
	ptr := c.block.NewGetElementPtr(g.ContentType, g,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
	c.block.NewCall(c.funcs["promise_panic"], ptr)
}
