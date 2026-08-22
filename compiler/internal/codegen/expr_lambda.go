package codegen

import (
	"fmt"

	"github.com/llir/llvm/ir"
	"github.com/llir/llvm/ir/constant"
	"github.com/llir/llvm/ir/enum"
	irtypes "github.com/llir/llvm/ir/types"
	"github.com/llir/llvm/ir/value"

	"github.com/promise-language/promise/compiler/internal/ast"
	"github.com/promise-language/promise/compiler/internal/sema"
	"github.com/promise-language/promise/compiler/internal/types"
)

// --- Lambda ---

func (c *Compiler) genLambdaExpr(e *ast.LambdaExpr) value.Value {
	sig, ok := c.info.Types[e].(*types.Signature)
	if !ok {
		panic("codegen: lambda expression type is not *types.Signature")
	}

	// Collect captures from sema info
	captures := c.info.LambdaCaptures[e]

	// Build LLVM function type — env pointer (i8*) is always the first parameter
	retType := irtypes.Type(irtypes.Void)
	if sig.Result() != nil {
		retType = c.resolveType(sig.Result())
	}

	params := []*ir.Param{ir.NewParam("env", irtypes.I8Ptr)}
	for _, p := range sig.Params() {
		params = append(params, ir.NewParam(p.Name(), c.resolveType(p.Type())))
	}

	// Create anonymous function. T1254: qualify the name with the enclosing
	// compilation unit's owner (instance/module) so lambdas created inside
	// monomorphized instance or module bodies get globally-unique names. This
	// lets each per-instance/per-module .bc keep external linkage without
	// colliding with identically-numbered lambdas baked into other cached .bc
	// files at link time.
	lambdaName := fmt.Sprintf(".lambda.%s%d", c.enclosingUnitPrefix(), c.lambdaCounter)
	c.lambdaCounter++
	fn := c.module.NewFunc(lambdaName, retType, params...)
	// T1254: route the lambda into the same compilation unit (.bc) as the
	// function that creates it. Without this, a lambda born inside a
	// monomorphized instance / module method body lands in the main IR while
	// its creating body lands in a cached instance/module .bc — and when that
	// .bc is served from cache (body generation skipped), the lambda is never
	// re-emitted, producing an undefined-symbol link error.
	c.adoptEnclosingCompilationUnit(fn)

	// Build env struct type and capture values from the enclosing scope BEFORE switching context
	var envStructType *irtypes.StructType
	var envPtr value.Value
	if len(captures) > 0 {
		// B0221: Field 0 is the env drop function pointer (i8*). Captures start at field 1.
		// This makes the env self-describing — cleanup code can load field 0 and call the
		// drop function to properly drop captured values before freeing the env struct.
		envFieldTypes := make([]irtypes.Type, len(captures)+1)
		envFieldTypes[0] = irtypes.I8Ptr // env drop fn pointer
		captureVals := make([]value.Value, len(captures))
		for i, cv := range captures {
			captureType := c.resolveType(cv.Obj.Type())
			// For 'this', use the alloca's element type (instance pointer) rather
			// than the sema type (value struct). The receiver is stored as a pointer
			// in method bodies, not as a full value struct.
			if alloca, ok := c.locals[cv.Obj.Name()]; ok {
				if cv.Obj.Name() == "this" {
					captureType = alloca.ElemType
				}
				captureVals[i] = c.block.NewLoad(captureType, alloca)
			} else {
				captureVals[i] = constant.NewZeroInitializer(captureType)
			}
			envFieldTypes[i+1] = captureType // +1 for header (B0221)
			// For move captures, clear the drop flag in the enclosing scope
			if cv.ByMove {
				c.clearDropFlag(cv.Obj.Name())
			}
		}
		envStructType = irtypes.NewStruct(envFieldTypes...)

		// B0221: Generate env drop function if any captures need dropping
		envDropFn := c.genEnvDropFunc(lambdaName, envStructType, captures)

		// Allocate env struct on heap
		envSize := int64(c.typeSize(envStructType))
		rawPtr := c.block.NewCall(c.palAlloc, constant.NewInt(irtypes.I64, envSize))
		typedEnvPtr := c.block.NewBitCast(rawPtr, irtypes.NewPointer(envStructType))

		// B0221: Store env drop fn pointer as field 0
		var envDropFnVal value.Value
		if envDropFn != nil {
			envDropFnVal = c.block.NewBitCast(envDropFn, irtypes.I8Ptr)
		} else {
			envDropFnVal = constant.NewNull(irtypes.I8Ptr)
		}
		dropFnField := c.block.NewGetElementPtr(envStructType, typedEnvPtr,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
		c.block.NewStore(envDropFnVal, dropFnField)

		// Store captured values into env struct (offset by 1 for header, B0221)
		for i, val := range captureVals {
			fieldPtr := c.block.NewGetElementPtr(envStructType, typedEnvPtr,
				constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(i+1)))
			c.block.NewStore(val, fieldPtr)
		}
		envPtr = rawPtr // i8*
	} else {
		envPtr = constant.NewNull(irtypes.I8Ptr)
	}

	// Save current state
	savedFn := c.fn
	savedBlock := c.block
	savedEntryBlock := c.entryBlock
	savedLocals := c.locals
	savedCanError := c.canError
	savedRetType := c.currentRetType
	savedBlockCounter := c.blockCounter
	savedScopeBindings := c.scopeBindings
	savedDropFlags := c.dropFlags
	savedCastSubjectMatch := c.castSubjectMatch // T0849: function-scoped, like dropFlags
	savedDropBindings := c.dropBindings         // B0035: must save/restore for NLL early drops
	savedLoopScopeDepth := c.loopScopeDepth
	savedWritebacks := c.lambdaWritebacks
	savedEnvOwnedCaptures := c.lambdaEnvOwnedCaptures // T1254
	savedGoExprFF2 := c.goExprFireAndForget
	savedStmtTemps := c.stmtTemps                       // T0073
	savedStmtTempMap := c.stmtTempMap                   // T0073
	savedHeapTemps := c.heapTemps                       // T0088
	savedHeapTempMap := c.heapTempMap                   // T0088
	savedEnvTemps := c.envTemps                         // T0100
	savedEnvTempMap := c.envTempMap                     // T0100
	savedEnumCtorTemps := c.enumCtorTemps               // B0267
	savedTempTracking := c.tempTrackingEnabled          // T0073
	savedLocalNameCount := c.localNameCount             // T0261
	savedPanicExitBlock := c.panicExitBlock             // T0262: clear in lambda (separate function)
	savedCoroutineReturnBlock := c.coroutineReturnBlock // T0262: clear in lambda (separate function)
	savedInCoroutine := c.inCoroutine                   // T0285: lambda is a separate function, not a coroutine
	savedCoroCleanup := c.coroCleanupBlk                // T0285: save coroutine blocks
	savedCoroSuspend := c.coroSuspendBlk                // T0285: save coroutine blocks
	savedDiscardedExpr := c.discardedExpr               // T1029: lambda body is not the discarded statement
	savedDiscardAliasArgPtrs := c.discardAliasArgPtrs   // T1029
	savedBlockTempFloors := c.resetBlockTempFloors()    // T1329: fresh function → floors from 0
	c.goExprFireAndForget = false                       // reset for inner statements (B0109)
	c.panicExitBlock = nil                              // T0262: lambda is a separate function
	c.coroutineReturnBlock = nil                        // T0262: lambda is a separate function
	c.inCoroutine = false                               // T0285: lambda is not a coroutine
	c.coroCleanupBlk = nil                              // T0285: no coroutine infrastructure
	c.coroSuspendBlk = nil                              // T0285: no coroutine infrastructure
	c.discardedExpr = nil                               // T1029: inner ExprStmts set their own
	c.discardAliasArgPtrs = nil                         // T1029

	// Generate lambda body with fresh scope state
	c.fn = fn
	c.locals = make(map[string]*ir.InstAlloca)
	c.localNameCount = make(map[string]int)
	c.blockCounter = 0
	c.canError = false
	c.currentRetType = sig.Result()
	savedBorrowedValueParams := c.borrowedValueParams // T0945
	c.setBorrowedValueParams(sig)                     // T0945: lambda body sees its own params
	c.scopeBindings = nil
	c.dropFlags = make(map[string]*ir.InstAlloca)
	c.castSubjectMatch = nil // T0849: fresh per lambda body; restored below
	c.dropBindings = make(map[string]scopeBinding)
	c.stmtTemps = nil                         // T0073
	c.stmtTempMap = make(map[value.Value]int) // T0073
	c.heapTemps = nil                         // T0088
	c.heapTempMap = make(map[value.Value]int) // T0088
	c.envTemps = nil                          // T0100
	c.envTempMap = make(map[value.Value]int)  // T0100
	c.enumCtorTemps = nil                     // B0267
	c.tempTrackingEnabled = true              // B0259: enable temp tracking in lambda bodies
	c.loopScopeDepth = 0
	c.lambdaWritebacks = nil
	c.lambdaEnvOwnedCaptures = nil // T1254: fresh per-lambda; populated in capture loop below

	entry := fn.NewBlock(".entry")
	c.block = entry
	c.entryBlock = entry

	// Load captured variables from env struct into local allocas
	if len(captures) > 0 && envStructType != nil {
		typedEnvPtr := entry.NewBitCast(fn.Params[0], irtypes.NewPointer(envStructType))
		for i, cv := range captures {
			// Use the env struct's field type — matches what was stored during capture
			// B0221: Field 0 is env drop fn; captures start at field i+1
			captureType := envStructType.Fields[i+1]
			fieldPtr := entry.NewGetElementPtr(envStructType, typedEnvPtr,
				constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(i+1)))
			val := entry.NewLoad(captureType, fieldPtr)
			alloca := entry.NewAlloca(captureType)
			alloca.SetName(c.uniqueLocalName(cv.Obj.Name() + ".cap"))
			entry.NewStore(val, alloca)
			c.locals[cv.Obj.Name()] = alloca
			// For move captures, register write-back so mutations persist across calls
			if cv.ByMove {
				c.lambdaWritebacks = append(c.lambdaWritebacks, lambdaWriteback{
					localAlloca: alloca,
					envFieldPtr: fieldPtr,
					elemType:    captureType,
				})
				// T0554: Do NOT register a scope-exit drop on the capture local. The env
				// drop function (genEnvDropFunc) is responsible for dropping all captured
				// values when the env struct is freed. Registering here would double-drop
				// (lambda-body exit + env_drop), causing segfaults for user types and
				// double-frees for any type with droppable fields.
				// B0229: Register reassignment-only drop for captured optional structural
				// interfaces (e.g., Iterator[R]? in flat_map). Only added to dropBindings,
				// NOT scopeBindings — the env drop function handles final cleanup, and
				// scope-exit drop would free a value that's been written back to the env.
				c.maybeRegisterCapturedOptionalStructuralDrop(cv.Obj.Name(), alloca, cv.Obj.Type())
				// T1254: record captures the env drop function will free, so a
				// `return <capture>` clones instead of handing back the raw pointer
				// (which env_drop would then double-free).
				if c.analyzeEnvCaptureDrop(cv).action != envDropNone {
					if c.lambdaEnvOwnedCaptures == nil {
						c.lambdaEnvOwnedCaptures = make(map[string]bool)
					}
					c.lambdaEnvOwnedCaptures[cv.Obj.Name()] = true
				}
			}
		}
	}

	// Allocate user parameters (offset by 1 due to env param)
	for i, p := range sig.Params() {
		if p.Name() == "" || p.Name() == "_" {
			continue
		}
		alloca := entry.NewAlloca(c.resolveType(p.Type()))
		alloca.SetName(c.uniqueLocalName(p.Name() + ".addr"))
		entry.NewStore(fn.Params[i+1], alloca) // +1 for env param
		c.locals[p.Name()] = alloca
	}

	// Generate body
	if e.Body != nil {
		c.genBlock(e.Body)
	} else if e.ExprBody != nil {
		val := c.genExpr(e.ExprBody)
		// T1634: a void-typed body expression (`|int y| -> print_line("{y}")`) still
		// yields a non-nil instruction, but it is not a value — feeding it to NewRet
		// emits the invalid `ret void %0`. Emit a bare `ret void` instead, while
		// running the same temp/scope cleanup so temps built by the expression (e.g.
		// the interpolated string argument) are still freed.
		// A nil val still falls through to the terminator path below, unchanged.
		_, voidRet := fn.Sig.RetType.(*irtypes.VoidType)
		if val != nil && c.block.Term == nil {
			if voidRet {
				val = nil
			} else {
				// T1254: `move || -> a` returning an env-owned droppable capture must
				// hand back an independent clone; env_drop frees the retained copy.
				val = c.maybeDupReturnedEnvCapture(val, e.ExprBody, sig.Result())
				// B0259: Clean up string/heap/env temps from the expression.
				// Claim the return value first so it's not freed.
				c.claimStringTemp(val)
				c.claimHeapTemp(val)
				c.claimEnvTemp(val)
			}
			// Store move-captured locals back into the env struct, exactly as the
			// fallthrough terminator below does. An expression body can mutate a
			// move-captured value through a `~this` method (`move |int y| ->
			// c.bump(y)`), and without the write-back the mutation is lost when the
			// call returns — so the expression form silently diverged from the
			// equivalent block form `move |int y| { c.bump(y); }`.
			c.emitLambdaWritebacks()
			c.cleanupStmtTemps()
			c.cleanupHeapTemps()
			c.cleanupEnvTemps()
			// Clean up capture bindings before returning
			if len(c.scopeBindings) > 0 {
				cap := c.emitScopeCleanup(0, false)
				c.emitCloseErrCheck(cap, 0)
			}
			c.block.NewRet(val)
		}
	}

	// Ensure terminator — clean up remaining capture bindings on fallthrough
	if c.block != nil && c.block.Term == nil {
		c.emitLambdaWritebacks()
		if len(c.scopeBindings) > 0 {
			cap := c.emitScopeCleanup(0, false)
			c.emitCloseErrCheck(cap, 0)
		}
		if _, ok := fn.Sig.RetType.(*irtypes.VoidType); ok {
			c.block.NewRet(nil)
		} else {
			c.block.NewRet(c.zeroValue(fn.Sig.RetType))
		}
	}

	// Restore state
	c.fn = savedFn
	c.block = savedBlock
	c.entryBlock = savedEntryBlock
	c.locals = savedLocals
	c.canError = savedCanError
	c.currentRetType = savedRetType
	c.blockCounter = savedBlockCounter
	c.scopeBindings = savedScopeBindings
	c.dropFlags = savedDropFlags
	c.castSubjectMatch = savedCastSubjectMatch // T0849
	c.dropBindings = savedDropBindings         // B0035: restore for NLL early drops
	c.loopScopeDepth = savedLoopScopeDepth
	c.lambdaWritebacks = savedWritebacks
	c.lambdaEnvOwnedCaptures = savedEnvOwnedCaptures // T1254
	c.goExprFireAndForget = savedGoExprFF2
	c.borrowedValueParams = savedBorrowedValueParams   // T0945
	c.stmtTemps = savedStmtTemps                       // T0073
	c.stmtTempMap = savedStmtTempMap                   // T0073
	c.heapTemps = savedHeapTemps                       // T0088
	c.heapTempMap = savedHeapTempMap                   // T0088
	c.envTemps = savedEnvTemps                         // T0100
	c.envTempMap = savedEnvTempMap                     // T0100
	c.enumCtorTemps = savedEnumCtorTemps               // B0267
	c.restoreBlockTempFloors(savedBlockTempFloors)     // T1329
	c.tempTrackingEnabled = savedTempTracking          // T0073
	c.localNameCount = savedLocalNameCount             // T0261
	c.panicExitBlock = savedPanicExitBlock             // T0262
	c.coroutineReturnBlock = savedCoroutineReturnBlock // T0262
	c.inCoroutine = savedInCoroutine                   // T0285
	c.coroCleanupBlk = savedCoroCleanup                // T0285
	c.coroSuspendBlk = savedCoroSuspend                // T0285
	c.discardedExpr = savedDiscardedExpr               // T1029
	c.discardAliasArgPtrs = savedDiscardAliasArgPtrs   // T1029

	// T0100: Track env temp for non-variable lambdas. If this lambda is
	// assigned to a variable, maybeRegisterEnvFree handles cleanup and the
	// env temp will be claimed. Otherwise, unclaimed envs are freed at statement end.
	if len(captures) > 0 {
		c.trackEnvTemp(envPtr)
	}

	// Return fat pointer: {fn_ptr as i8*, env_ptr}
	fnPtr := c.block.NewBitCast(fn, irtypes.I8Ptr)
	var closure value.Value = constant.NewUndef(closureType())
	closure = c.block.NewInsertValue(closure, fnPtr, 0)
	closure = c.block.NewInsertValue(closure, envPtr, 1)
	return closure
}

// --- Env Drop Function Generation (B0221) ---

// envDropAction describes what cleanup a captured value needs in the env drop function.
type envDropAction int

const (
	envDropNone               envDropAction = iota
	envDropCallFn                           // call dropFn(i8*) — string, vector, channel (handles free internally)
	envDropClosureEnv                       // extract env from closure {i8*,i8*}, env-drop-or-free
	envDropUserValue                        // extract inst from value {i8*,i8*}, pal_free — heap user type without drop
	envDropUserValueDrop                    // extract inst from value {i8*,i8*}, call cleanup fn (synth drop incl. pal_free, or $wrap, or palFree)
	envDropOptionalStructural               // B0229: optional structural iface — check has_value, extract inst, cleanup
	envDropStructural                       // T1344: non-optional structural iface — extract inst, RTTI drop dispatch
)

type envFieldDrop struct {
	action envDropAction
	dropFn *ir.Func
}

// analyzeEnvCaptureDrop determines the drop action for a single captured variable.
// Applies type substitution so the analysis uses concrete (monomorphized) types.
func (c *Compiler) analyzeEnvCaptureDrop(cv *sema.CapturedVar) envFieldDrop {
	typ := cv.Obj.Type()
	if c.typeSubst != nil {
		typ = types.Substitute(typ, c.typeSubst)
	}
	if c.selfSubst != nil {
		typ = types.SubstituteSelf(typ, c.selfSubst.iface, c.selfSubst.concrete)
	}
	// T1440: a `this` capture never transfers ownership to the closure, so
	// env_drop must leave it alone:
	//   - borrowed `this` (e.g. `Vector[T].iter()`): the receiver belongs to the
	//     caller, which drops it via its own scope binding. Freeing it here is
	//     the double-free T1440 reported.
	//   - `~this` (e.g. the `Iterator[T]` combinators): the receiver has no drop
	//     binding inside the method either — defineMethodFunc only allocates
	//     `this.addr`. A consumed upstream iterator is reclaimed through
	//     `_FnIter._parent` by `__promise_iter_cleanup` (T0128).
	// Sema forces `move` on every `this` capture (checkThisExpr) even though
	// nothing is transferred, so `cv.ByMove` cannot be used to tell the two
	// apart; the name check is the discriminator.
	//
	// This is the single place that decision is made — the heap-user-type and
	// structural branches below rely on it rather than re-testing the name.
	if cv.Obj.Name() == "this" {
		return envFieldDrop{envDropNone, nil}
	}
	// String/Vector/Channel → call specific drop function (i8* field, drop handles free)
	named := extractNamed(typ)
	if named == types.TypString {
		if fn := c.funcs["promise_string_drop"]; fn != nil {
			return envFieldDrop{envDropCallFn, fn}
		}
	}
	if _, ok := types.AsVector(typ); ok || (named != nil && named == types.TypVector) {
		if fn := c.funcs["Vector.drop"]; fn != nil {
			return envFieldDrop{envDropCallFn, fn}
		}
	}
	if elemType, ok := types.AsChannel(typ); ok || (named != nil && named == types.TypChannel) {
		// T0663: per-element-type drop walks any un-received buffered items.
		if ok {
			return envFieldDrop{envDropCallFn, c.getOrCreateChannelDrop(elemType)}
		}
	}
	// Arc/Weak/Mutex/MutexGuard → call per-element-type drop function (i8* field, same pattern as string/vector/channel)
	if elemType, ok := types.AsArc(typ); ok || (named != nil && named == types.TypArc) {
		if ok {
			return envFieldDrop{envDropCallFn, c.getOrCreateArcDrop(elemType)}
		}
	}
	if elemType, ok := types.AsWeak(typ); ok || (named != nil && named == types.TypWeak) {
		if ok {
			return envFieldDrop{envDropCallFn, c.getOrCreateWeakDrop(elemType)}
		}
	}
	if elemType, ok := types.AsMutex(typ); ok || (named != nil && named == types.TypMutex) {
		if ok {
			return envFieldDrop{envDropCallFn, c.getOrCreateMutexDrop(elemType)}
		}
	}
	if _, ok := types.AsMutexGuard(typ); ok || (named != nil && named == types.TypMutexGuard) {
		if fn := c.funcs["MutexGuard.drop"]; fn != nil {
			return envFieldDrop{envDropCallFn, fn}
		}
	}
	// T0503: Task[T]/FailableTask[T] capture → per-instantiation drop (spin-wait + free).
	if elemType, ok, taskFail := types.AsAnyTaskFailable(typ); ok || (named != nil && types.IsTaskLikeOrigin(named)) {
		if ok {
			return envFieldDrop{envDropCallFn, c.getOrCreateTaskDrop(elemType, taskFail)}
		}
	}

	// Closure (Signature) → free inner env
	if _, ok := typ.(*types.Signature); ok {
		return envFieldDrop{envDropClosureEnv, nil}
	}

	// Heap user type — need to free instance (and call drop if it has one).
	// `this` captures already returned above (never owned by the closure).
	//
	// T0554: Use resolveDropFuncForTemp to get the correct cleanup function.
	// For synthesized drops, the bare drop already includes pal_free. For
	// explicit user drops, $wrap is returned which calls drop + pal_free.
	// Either way, env_drop just calls this function (no separate pal_free).
	if named != nil && !named.IsValueType() && !named.IsCopy() && !isPrimitiveScalar(named) && !named.IsStructural() {
		if dropFn := c.resolveDropFuncForTemp(named, typ); dropFn != nil {
			return envFieldDrop{envDropUserValueDrop, dropFn}
		}
		return envFieldDrop{envDropUserValue, nil}
	}

	// B0243: Optional structural interface (e.g., Iterator[T]?) — use RTTI-based drop dispatch.
	// The concrete type is unknown at compile time, so we can't use __promise_iter_cleanup
	// (which assumes _FnIter layout). Instead, we dispatch through typeinfo.drop_fn_ptr.
	if opt, ok := typ.(*types.Optional); ok {
		elem := opt.Elem()
		if c.typeSubst != nil {
			elem = types.Substitute(elem, c.typeSubst)
		}
		innerNamed := extractNamed(elem)
		if innerNamed != nil && innerNamed.IsStructural() && !innerNamed.IsValueType() {
			return envFieldDrop{envDropOptionalStructural, nil}
		}
	}

	// T1344: non-optional structural interface (e.g., Iterator[T]) captured by
	// move — dispatch drop through RTTI, same as the optional case but without
	// the has_value guard. Any *move*-captured non-`this` structural value is
	// guaranteed owned: ownership analysis (T0338, ownership/expr.go) rejects
	// move-capturing a borrowed parameter or borrowed value, and `this` captures
	// already returned above. So dropping here can never double-free the
	// caller's instance.
	if named != nil && named.IsStructural() && !named.IsValueType() {
		return envFieldDrop{envDropStructural, nil}
	}

	return envFieldDrop{envDropNone, nil}
}

// genEnvDropFunc generates a per-closure env drop function that drops each captured
// value that needs dropping before freeing the env struct. Returns nil if no captures
// need dropping (callers will use pal_free directly via the null header check).
// The env struct layout is: { i8* env_drop_fn, capture0, capture1, ... }.
//
// Handles: strings, vectors, channels, heap user types (with/without drop),
// and closure captures (frees inner env). Skips `this` captures (borrowed, not owned).

// adoptEnclosingCompilationUnit tags a newly-created helper function (lambda or
// env-drop) with the same instance/module ownership as the function currently
// being generated (c.fn). This ensures the helper travels into the same split .bc
// as its creator instead of the main IR (T1254). When the creator's body lives in
// a per-instance or per-module .bc that can be served from cache (body generation
// skipped), the helper must be self-contained in that same .bc — otherwise it is
// never emitted on a cache hit → undefined symbol at link. The helper keeps
// external linkage but its name is owner-qualified (see enclosingUnitPrefix) so
// copies baked into separately-cached objects cannot collide at link time.
func (c *Compiler) adoptEnclosingCompilationUnit(fn *ir.Func) {
	if c.fn == nil {
		return
	}
	encl := c.fn.Name()
	if owner, ok := c.instanceOwnedFuncs[encl]; ok {
		c.instanceOwnedFuncs[fn.Name()] = owner
	}
	if owner, ok := c.moduleOwnedFuncs[encl]; ok {
		c.moduleOwnedFuncs[fn.Name()] = owner
	}
}

// enclosingUnitPrefix returns a name qualifier ("<owner>.") identifying the
// per-instance or per-module compilation unit that owns the function currently
// being generated (c.fn), or "" when it is plain main-IR code. Used to give
// helper functions (lambdas) globally-unique names so their bodies, once routed
// into an owner's cached .bc, cannot collide with identically-numbered helpers
// from other cached objects (T1254).
func (c *Compiler) enclosingUnitPrefix() string {
	if c.fn == nil {
		return ""
	}
	encl := c.fn.Name()
	if owner, ok := c.instanceOwnedFuncs[encl]; ok {
		return owner + "."
	}
	if owner, ok := c.moduleOwnedFuncs[encl]; ok {
		return owner + "."
	}
	return ""
}

func (c *Compiler) genEnvDropFunc(lambdaName string, envStructType *irtypes.StructType, captures []*sema.CapturedVar) *ir.Func {
	// Analyze each capture to determine drop action
	actions := make([]envFieldDrop, len(captures))
	hasAnyAction := false
	for i, cv := range captures {
		actions[i] = c.analyzeEnvCaptureDrop(cv)
		if actions[i].action != envDropNone {
			hasAnyAction = true
		}
	}
	if !hasAnyAction {
		return nil
	}

	dropFnName := lambdaName + ".env_drop"
	dropFn := c.module.NewFunc(dropFnName, irtypes.Void, ir.NewParam("env", irtypes.I8Ptr))
	// T1254: keep the env-drop helper in the same .bc as its lambda/creator.
	c.adoptEnclosingCompilationUnit(dropFn)

	curBlock := dropFn.NewBlock(".entry")
	typedPtr := curBlock.NewBitCast(dropFn.Params[0], irtypes.NewPointer(envStructType))

	blockIdx := 0
	for i := range captures {
		act := actions[i]
		if act.action == envDropNone {
			continue
		}

		fieldIdx := int64(i + 1) // +1 for env_drop_fn header
		fieldPtr := curBlock.NewGetElementPtr(envStructType, typedPtr,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, fieldIdx))
		fieldVal := curBlock.NewLoad(envStructType.Fields[i+1], fieldPtr)

		nextBlk := dropFn.NewBlock(fmt.Sprintf("next.%d", blockIdx))

		switch act.action {
		case envDropCallFn:
			// i8* field (string/vector/channel): null-check, call drop fn
			isNull := curBlock.NewICmp(enum.IPredEQ, fieldVal, constant.NewNull(irtypes.I8Ptr))
			dropBlk := dropFn.NewBlock(fmt.Sprintf("drop.%d", blockIdx))
			curBlock.NewCondBr(isNull, nextBlk, dropBlk)
			dropBlk.NewCall(act.dropFn, fieldVal)
			dropBlk.NewBr(nextBlk)

		case envDropClosureEnv:
			// Closure fat pointer {fn_ptr, env_ptr}: extract env, env-drop-or-free
			innerEnvPtr := curBlock.NewExtractValue(fieldVal, 1)
			isNull := curBlock.NewICmp(enum.IPredEQ, innerEnvPtr, constant.NewNull(irtypes.I8Ptr))
			checkBlk := dropFn.NewBlock(fmt.Sprintf("clo.check.%d", blockIdx))
			curBlock.NewCondBr(isNull, nextBlk, checkBlk)
			// Load inner env's drop fn header
			envHeaderType := irtypes.NewStruct(irtypes.I8Ptr)
			typedHdr := checkBlk.NewBitCast(innerEnvPtr, irtypes.NewPointer(envHeaderType))
			hdrField := checkBlk.NewGetElementPtr(envHeaderType, typedHdr,
				constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
			innerDropRaw := checkBlk.NewLoad(irtypes.I8Ptr, hdrField)
			hasInnerDrop := checkBlk.NewICmp(enum.IPredNE, innerDropRaw, constant.NewNull(irtypes.I8Ptr))
			deepBlk := dropFn.NewBlock(fmt.Sprintf("clo.deep.%d", blockIdx))
			shallowBlk := dropFn.NewBlock(fmt.Sprintf("clo.free.%d", blockIdx))
			checkBlk.NewCondBr(hasInnerDrop, deepBlk, shallowBlk)
			// Deep drop: call inner env's drop function
			innerDropFnType := irtypes.NewFunc(irtypes.Void, irtypes.I8Ptr)
			typedInnerDrop := deepBlk.NewBitCast(innerDropRaw, irtypes.NewPointer(innerDropFnType))
			deepBlk.NewCall(typedInnerDrop, innerEnvPtr)
			deepBlk.NewBr(nextBlk)
			// Shallow free: just pal_free the inner env
			shallowBlk.NewCall(c.palFree, innerEnvPtr)
			shallowBlk.NewBr(nextBlk)

		case envDropUserValue:
			// User type value struct {vtable, instance}: extract instance, null-check, pal_free
			instPtr := curBlock.NewExtractValue(fieldVal, 1)
			isNull := curBlock.NewICmp(enum.IPredEQ, instPtr, constant.NewNull(irtypes.I8Ptr))
			freeBlk := dropFn.NewBlock(fmt.Sprintf("ufree.%d", blockIdx))
			curBlock.NewCondBr(isNull, nextBlk, freeBlk)
			freeBlk.NewCall(c.palFree, instPtr)
			freeBlk.NewBr(nextBlk)

		case envDropUserValueDrop:
			// User type value struct {vtable, instance}: extract instance, null-check, call cleanup fn.
			// T0554: dropFn is from resolveDropFuncForTemp — synth drops include pal_free,
			// explicit-drop $wrap calls drop + pal_free. Either way, do NOT call pal_free
			// separately or we double-free.
			instPtr := curBlock.NewExtractValue(fieldVal, 1)
			isNull := curBlock.NewICmp(enum.IPredEQ, instPtr, constant.NewNull(irtypes.I8Ptr))
			dropBlk := dropFn.NewBlock(fmt.Sprintf("udrop.%d", blockIdx))
			curBlock.NewCondBr(isNull, nextBlk, dropBlk)
			dropBlk.NewCall(act.dropFn, instPtr)
			dropBlk.NewBr(nextBlk)

		case envDropOptionalStructural:
			// B0243: Optional structural iface {i1 has_value, {i8* vtable, i8* instance}}:
			// check has_value, extract instance, RTTI-based drop dispatch.
			// The concrete type is unknown, so we load drop_fn from typeinfo.
			hasVal := curBlock.NewExtractValue(fieldVal, 0)
			innerBlk := dropFn.NewBlock(fmt.Sprintf("optst.inner.%d", blockIdx))
			curBlock.NewCondBr(hasVal, innerBlk, nextBlk)
			innerVal := innerBlk.NewExtractValue(fieldVal, 1)
			instPtr := innerBlk.NewExtractValue(innerVal, 1)
			isNull := innerBlk.NewICmp(enum.IPredEQ, instPtr, constant.NewNull(irtypes.I8Ptr))
			rttiBlk := dropFn.NewBlock(fmt.Sprintf("optst.rtti.%d", blockIdx))
			innerBlk.NewCondBr(isNull, nextBlk, rttiBlk)

			// Load variant ptr (typeinfo) from instance[0]
			instStructType := irtypes.NewStruct(irtypes.I8Ptr)
			typedInst := rttiBlk.NewBitCast(instPtr, irtypes.NewPointer(instStructType))
			variantField := rttiBlk.NewGetElementPtr(instStructType, typedInst,
				constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
			variantPtr := rttiBlk.NewLoad(irtypes.I8Ptr, variantField)

			// Load drop_fn_ptr from typeinfo[1]
			typeinfoType := irtypes.NewStruct(irtypes.I8Ptr, irtypes.I8Ptr)
			typedTI := rttiBlk.NewBitCast(variantPtr, irtypes.NewPointer(typeinfoType))
			dropFnField := rttiBlk.NewGetElementPtr(typeinfoType, typedTI,
				constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 1))
			dropFnRaw := rttiBlk.NewLoad(irtypes.I8Ptr, dropFnField)
			hasDropFn := rttiBlk.NewICmp(enum.IPredNE, dropFnRaw, constant.NewNull(irtypes.I8Ptr))

			callDropBlk := dropFn.NewBlock(fmt.Sprintf("optst.drop.%d", blockIdx))
			justFreeBlk := dropFn.NewBlock(fmt.Sprintf("optst.free.%d", blockIdx))
			rttiBlk.NewCondBr(hasDropFn, callDropBlk, justFreeBlk)

			// Has drop: call it (handles free for synth/native drops)
			dropFnType := irtypes.NewFunc(irtypes.Void, irtypes.I8Ptr)
			typedDropFn := callDropBlk.NewBitCast(dropFnRaw, irtypes.NewPointer(dropFnType))
			callDropBlk.NewCall(typedDropFn, instPtr)
			callDropBlk.NewBr(nextBlk)

			// No drop: just free the instance
			justFreeBlk.NewCall(c.palFree, instPtr)
			justFreeBlk.NewBr(nextBlk)

		case envDropStructural:
			// T1344: non-optional structural iface value {i8* vtable, i8* instance}:
			// extract instance, RTTI-based drop dispatch. Identical to
			// envDropOptionalStructural minus the has_value check.
			instPtr := curBlock.NewExtractValue(fieldVal, 1)
			isNull := curBlock.NewICmp(enum.IPredEQ, instPtr, constant.NewNull(irtypes.I8Ptr))
			rttiBlk := dropFn.NewBlock(fmt.Sprintf("st.rtti.%d", blockIdx))
			curBlock.NewCondBr(isNull, nextBlk, rttiBlk)

			// Load variant ptr (typeinfo) from instance[0]
			instStructType := irtypes.NewStruct(irtypes.I8Ptr)
			typedInst := rttiBlk.NewBitCast(instPtr, irtypes.NewPointer(instStructType))
			variantField := rttiBlk.NewGetElementPtr(instStructType, typedInst,
				constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
			variantPtr := rttiBlk.NewLoad(irtypes.I8Ptr, variantField)

			// Load drop_fn_ptr from typeinfo[1]
			typeinfoType := irtypes.NewStruct(irtypes.I8Ptr, irtypes.I8Ptr)
			typedTI := rttiBlk.NewBitCast(variantPtr, irtypes.NewPointer(typeinfoType))
			dropFnField := rttiBlk.NewGetElementPtr(typeinfoType, typedTI,
				constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 1))
			dropFnRaw := rttiBlk.NewLoad(irtypes.I8Ptr, dropFnField)
			hasDropFn := rttiBlk.NewICmp(enum.IPredNE, dropFnRaw, constant.NewNull(irtypes.I8Ptr))

			callDropBlk := dropFn.NewBlock(fmt.Sprintf("st.drop.%d", blockIdx))
			justFreeBlk := dropFn.NewBlock(fmt.Sprintf("st.free.%d", blockIdx))
			rttiBlk.NewCondBr(hasDropFn, callDropBlk, justFreeBlk)

			// Has drop: call it (handles free for synth/native drops)
			dropFnType := irtypes.NewFunc(irtypes.Void, irtypes.I8Ptr)
			typedDropFn := callDropBlk.NewBitCast(dropFnRaw, irtypes.NewPointer(dropFnType))
			callDropBlk.NewCall(typedDropFn, instPtr)
			callDropBlk.NewBr(nextBlk)

			// No drop: just free the instance
			justFreeBlk.NewCall(c.palFree, instPtr)
			justFreeBlk.NewBr(nextBlk)
		}

		curBlock = nextBlk
		blockIdx++
	}

	// Free the env struct itself
	curBlock.NewCall(c.palFree, dropFn.Params[0])
	curBlock.NewRet(nil)

	return dropFn
}
