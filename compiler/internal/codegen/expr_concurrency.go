package codegen

import (
	"fmt"
	"sort"

	"github.com/llir/llvm/ir"
	"github.com/llir/llvm/ir/constant"
	"github.com/llir/llvm/ir/enum"
	irtypes "github.com/llir/llvm/ir/types"
	"github.com/llir/llvm/ir/value"

	"github.com/promise-language/promise/compiler/internal/ast"
	"github.com/promise-language/promise/compiler/internal/types"
)

// genReceiveTaskSlotPtr computes the element l-value slot pointer (i8**) for a
// `<-coll[i]` task-receive operand, so genReceiveTask can null the slot after
// it frees the G (T0638). Mirrors the element-pointer computation in
// genArrayIndex / genVectorIndex WITHOUT a bounds check — the receive's own
// operand eval already bounds-checked with the same index, so reaching here
// means the index is in range and c.block is the in-bounds block. Returns
// (ptr,true) for fixed-array and Vector Task elements; (nil,false) otherwise.
func (c *Compiler) genReceiveTaskSlotPtr(e *ast.IndexExpr) (value.Value, bool) {
	targetType := c.info.Types[e.Target]
	if c.typeSubst != nil {
		targetType = types.Substitute(targetType, c.typeSubst)
	}
	if ref, ok := targetType.(*types.MutRef); ok {
		targetType = ref.Elem()
	}
	if ref, ok := targetType.(*types.SharedRef); ok {
		targetType = ref.Elem()
	}

	// Fixed-size array: GEP into the array alloca/field slot.
	if arr, ok := targetType.(*types.Array); ok {
		basePtr := c.genArrayBasePtr(e.Target, arr)
		idx := c.genExpr(e.Index)
		elemLLVM := c.resolveType(arr.Elem())
		arrType := irtypes.NewArray(uint64(arr.Size()), elemLLVM)
		return c.block.NewGetElementPtr(arrType, basePtr,
			constant.NewInt(irtypes.I32, 0), idx), true
	}

	// Vector: GEP into the heap data buffer past the fixed-size header. Mirrors
	// genVectorIndex's read path (genExprAutoPropagate — no COW; Task vectors
	// are always heap, never .rodata).
	named := extractNamed(targetType)
	var elemType types.Type
	if elem, ok := types.AsVector(targetType); ok {
		elemType = elem
	} else if named == types.TypVector && c.typeSubst != nil {
		tp := named.TypeParams()[0]
		if elem, ok := c.typeSubst[tp]; ok {
			elemType = elem
		}
	}
	if elemType != nil {
		slicePtr := c.genExprAutoPropagate(e.Target) // B0323
		idx := c.genExpr(e.Index)
		elemLLVM := c.resolveType(elemType)
		dataBase := c.block.NewGetElementPtr(irtypes.I8, slicePtr,
			constant.NewInt(irtypes.I64, int64(vectorHeaderSize)))
		dataTypedPtr := c.block.NewBitCast(dataBase, irtypes.NewPointer(elemLLVM))
		return c.block.NewGetElementPtr(elemLLVM, dataTypedPtr, idx), true
	}

	return nil, false
}

// --- Go expression (concurrency) ---

// genGoExpr generates code for a `go expr` expression.
// It creates an LLVM coroutine, wraps it in a G, and enqueues it on the M:N scheduler.
func (c *Compiler) genGoExpr(e *ast.GoExpr) value.Value {
	if e.Expr != nil {
		callExpr, ok := e.Expr.(*ast.CallExpr)
		if !ok {
			// Unreachable: sema rejects non-call `go` operands (T1149). This
			// guards the sema/codegen contract — reaching it means a check was
			// skipped upstream.
			panic(fmt.Sprintf("codegen: internal error: go operand should be a call after sema, got %T", e.Expr))
		}
		return c.genGoCallExpr(callExpr, e.Failable)
	}
	// go { block } form
	return c.genGoBlock(e)
}

// goArgBorrowDrop records a heap argument temporary whose ownership belongs to a
// goroutine frame and must be dropped on the goroutine side (T1098). Only borrow
// params are recorded — for move params the callee consumes and drops the
// temporary at its own scope exit.
type goArgBorrowDrop struct {
	paramIdx int        // index into coroFn.Params holding the argument value
	dropFunc *ir.Func   // concrete drop function (promise_string_drop, Vector[T].drop, T.drop)
	elemType types.Type // vector element type for element drops (nil for non-vectors)
	isStruct bool       // coro param is a value struct {vtable, instance} → drop field 1 (heap user type)
	isEnum   bool       // T1154: coro param is an enum value struct; spill to alloca and pass &slot to the enum drop fn
	capType  types.Type // T1198: dup'd borrowed-param capture — drop by type via emitVariantFieldDrop
}

// emitGoArgBorrowDrops frees the heap argument temporaries owned by a goroutine
// frame for borrow params (T1098). Emitted in the coroutine body immediately
// after the target call returns. Returns the current block (drop loops for
// vector elements split the control flow). Reuses emitVectorElementDropLoop by
// pointing c.fn/c.entryBlock/c.block at the coroutine's frame for the duration.
func (c *Compiler) emitGoArgBorrowDrops(coroFn *ir.Func, entry, cur *ir.Block, drops []goArgBorrowDrop) *ir.Block {
	if len(drops) == 0 {
		return cur
	}
	savedFn, savedBlock, savedEntry := c.fn, c.block, c.entryBlock
	c.fn, c.entryBlock, c.block = coroFn, entry, cur
	defer func() { c.fn, c.block, c.entryBlock = savedFn, savedBlock, savedEntry }()

	for _, d := range drops {
		param := coroFn.Params[d.paramIdx]
		switch {
		case d.capType != nil:
			// T1198: a dup'd borrowed-param capture (see genGoCallExpr). Drop by type
			// via the canonical drop-by-type helper, which handles string/vector/Map/
			// Set/heap-user/polymorphic uniformly (incl. no-drop pal_free). It operates
			// on c.block and may split blocks / create entry allocas via c.entryBlock —
			// the frame swap above already points c.fn/c.entryBlock/c.block at coroFn.
			c.emitVariantFieldDrop(param, d.capType)
		case d.isEnum:
			// T1154: enum value passed by value into the coro frame — spill it to a
			// frame slot so the synthesized enum drop fn (which takes a pointer to
			// the value-struct layout and switches on the tag) can free the variant
			// payload exactly once.
			slot := c.createEntryAlloca(param.Type())
			c.block.NewStore(param, slot)
			c.block.NewCall(d.dropFunc, c.block.NewBitCast(slot, irtypes.I8Ptr))
		case d.isStruct:
			// Heap user type passed as a value struct {vtable, instance}: drop
			// the heap instance pointer.
			inst := c.block.NewExtractValue(param, 1)
			var instI8 value.Value = inst
			if inst.Type() != irtypes.I8Ptr {
				instI8 = c.block.NewBitCast(inst, irtypes.I8Ptr)
			}
			c.block.NewCall(d.dropFunc, instI8)
		default:
			// i8* string/vector: drop droppable elements (vectors) then free the buffer.
			var ptr value.Value = param
			if param.Type() != irtypes.I8Ptr {
				ptr = c.block.NewBitCast(param, irtypes.I8Ptr)
			}
			if d.elemType != nil {
				c.emitVectorElementDropLoop(ptr, d.elemType)
			}
			c.block.NewCall(d.dropFunc, ptr)
		}
	}
	return c.block
}

// genGoCallExpr handles `go func(args...)` — the common case.
// For non-IdentExpr callees (method calls, module calls, etc.), delegates to
// genGoCallExprViaBlock which uses the full codegen context inside the coroutine body.
func (c *Compiler) genGoCallExpr(callExpr *ast.CallExpr, failable bool) value.Value {
	// Complex callees (method calls, module calls, generic calls, etc.)
	// need the full codegen context — use block-style coroutine (B0113).
	ident, ok := callExpr.Callee.(*ast.IdentExpr)
	if !ok {
		return c.genGoCallExprViaBlock(callExpr, failable)
	}

	// T1024: A bare-ident callee that resolves to a generic free function with
	// INFERRED type args has no plain entry in c.funcs (only the monomorphized
	// `gget__int` exists), so resolveGoTarget would panic. Route it through the
	// block-style path — the same path the explicit-type-arg form (IndexExpr
	// callee) already takes — which builds the call via genExpr → the inferred
	// generic-call codegen.
	if _, ok := c.info.InferredTypeArgs[callExpr]; ok {
		return c.genGoCallExprViaBlock(callExpr, failable)
	}

	// 1. Resolve result type T from sema.
	//
	// T1379: for `go! f()` the goroutine's stored result is the failable
	// aggregate `{i1 ok, T value, i8* err}` that the failable target function
	// returns — the receive (`<-t`) surfaces it exactly like a failable call.
	// So the result buffer holds the aggregate (never void, even when T is
	// void), and the coroutine stores the call's return value verbatim.
	callResultType := c.info.Types[callExpr]
	isVoid := (callResultType == nil || callResultType == types.TypVoid)
	var resultLLVM irtypes.Type = irtypes.Void
	if failable {
		var inner irtypes.Type = irtypes.Void
		if !isVoid {
			inner = c.resolveType(callResultType)
		}
		resultLLVM = computeResultType(inner)
		isVoid = false // the aggregate is always a stored, non-void result
	} else if !isVoid {
		resultLLVM = c.resolveType(callResultType)
	}

	// T1098: resolve the callee signature so we can distinguish move (consume)
	// params from borrow params for argument-temporary ownership transfer.
	var calleeParams []*types.Param
	if fn := c.lookupFunc(ident.Name); fn != nil {
		if sig, ok := fn.Type().(*types.Signature); ok {
			calleeParams = sig.Params()
		}
	}

	// 2. Evaluate arguments in caller scope
	//
	// T1098: A heap-allocated argument temporary (e.g. `s.clone()`, `Box(...)`)
	// is created in the caller's scope but is consumed by the goroutine, which may
	// not run until after the spawning statement completes. The caller's
	// end-of-statement cleanup must therefore NOT free it — ownership belongs to
	// the goroutine frame. For each argument we identify the SINGLE owned
	// temporary that the argument value represents (its root), clear that one
	// temporary's caller-side drop flag, and — for borrow params — record a
	// goroutine-side drop emitted after the target call returns. Move params get
	// no goroutine-side drop: the callee consumes and drops the temporary at its
	// own scope exit. Intermediates the argument expression created but does NOT
	// yield (e.g. the trim() result in `s.trim().clone()`) keep their flags and
	// are freed by the caller — they never enter the goroutine.
	var argVals []value.Value
	var argLLVMTypes []irtypes.Type
	var argTypes []types.Type
	var argBorrowDrops []goArgBorrowDrop
	// T1108/T1154: snapshot enum-ctor temps so we can drop them from the
	// caller's statement-end cleanup after the loop — a synchronous statement-end
	// drop would be a use-after-free since the goroutine may reference the payload
	// after the spawning statement. T1154: for an inline enum-ctor arg whose
	// payload is droppable and the param is a borrow, the goroutine frame takes
	// ownership and drops via the synthesized enum drop fn after the call returns
	// (see the per-arg enum branch + emitGoArgBorrowDrops). Move params are
	// consumed and dropped by the callee.
	savedGoEnumTemps := len(c.enumCtorTemps)
	for i, arg := range callExpr.Args {
		savedHeap := len(c.heapTemps)
		savedStmt := len(c.stmtTemps)
		savedArgEnum := len(c.enumCtorTemps)
		// T1106/T1107: don't register a top-level match/if phi arg as a caller stmt
		// temp — the go-arg machinery below transfers it into the goroutine frame.
		savedSuppress := c.suppressMergeResultTemp
		c.suppressMergeResultTemp = true
		v := c.genCallArgExpr(arg.Value)
		c.suppressMergeResultTemp = savedSuppress
		argVals = append(argVals, v)
		argLLVMTypes = append(argLLVMTypes, v.Type())
		argTypes = append(argTypes, c.info.Types[arg.Value])

		// Identify the argument's single root owned temporary with a statically
		// known drop. Only unambiguous roots are transferred:
		//   - a string/vector temp whose tracked SSA value IS the argument value
		//     (covers `s.clone()`, and `s.trim().clone()` where the trim()
		//     intermediate correctly stays with the caller);
		//   - a lone heap user-type temp from a direct constructor (covers
		//     `Box(s: s.clone())` — the inner string is a claimed stmt temp, so
		//     exactly one new heap temp remains).
		// T1106: conditional/polymorphic args (match/if expressions, nested
		// constructors) yield a runtime phi over temporaries with possibly
		// different concrete drops — no single static root. These are handled by
		// the Case A / Case B branches below via runtime drop dispatch. T1154: a
		// top-level inline enum-ctor arg with a droppable payload IS a single static
		// root: it is transferred to the goroutine frame via the enum branch below.
		newHeap := c.heapTemps[savedHeap:]
		newStmt := c.stmtTemps[savedStmt:]
		newEnum := c.enumCtorTemps[savedArgEnum:]

		// T1157: whether the argument's OUTER type is an enum. An inline enum-ctor
		// arg with a heap-user-type payload (`Wrap.One(Box(...))`) leaves the inner
		// Box temp in newHeap (already claimed by the enum ctor), so the
		// `len(newHeap)==1` heap-root branch would mis-route to the isStruct path
		// using the inner type's drop fn and extracting the wrong field ([16 x i8]
		// payload data, not a pointer) → invalid IR. Routing on argIsEnum keeps the
		// enum as the root and drops via the synthesized per-enum drop fn.
		argIsEnum := false
		if at := argTypes[i]; at != nil {
			switch t := at.(type) {
			case *types.Enum:
				argIsEnum = true
			case *types.Instance:
				_, argIsEnum = t.Origin().(*types.Enum)
			}
		}

		var d goArgBorrowDrop
		d.paramIdx = i
		var rootFlag *ir.InstAlloca
		if idx, ok := c.stmtTempMap[v]; ok && idx >= 0 {
			st := c.stmtTemps[idx]
			rootFlag, d.dropFunc, d.elemType = st.dropFlag, st.dropFunc, st.elemType
			c.stmtTempMap[v] = -1
		} else if len(newEnum) == 1 && argIsEnum {
			// T1154/T1157: the arg's outer type is an enum and it is an inline enum
			// constructor — the enum is the root. Drop via the synthesized per-enum
			// drop fn, which switches on the tag and recurses into the payload drop
			// (string/vector OR a heap-user-type payload's T.drop). Gated on
			// argIsEnum so a nested ctor like `Box(Msg.Text(...))` (outer = heap user
			// type) keeps its heap-root handling. Routed here regardless of newHeap: a
			// heap-user-type payload (`Wrap.One(Box(...))`) leaves the inner Box temp
			// in newHeap — already claimed (caller flag cleared) by the enum ctor's
			// claimHeapTemp — so routing it to the isStruct branch (T1157) extracted
			// the wrong field ([16 x i8] data, not a pointer) and ran the wrong
			// (inner) drop fn → crash. The enum is passed by value into the coro
			// frame; its drop fn takes a pointer to the value-struct layout, so the
			// goroutine spills the param and drops.
			et := newEnum[0]
			rootFlag, d.dropFunc = et.dropFlag, et.dropFunc
			d.isEnum = true
		} else if len(newHeap) == 1 {
			ht := newHeap[0]
			rootFlag, d.dropFunc, d.elemType = ht.dropFlag, ht.dropFunc, ht.elemType
			_, d.isStruct = v.Type().(*irtypes.StructType)
		}
		if rootFlag == nil || d.dropFunc == nil {
			isMove := i < len(calleeParams) && calleeParams[i].Ref() == types.RefMut

			// T1106 Case A: a heap value-struct argument whose concrete type is not
			// statically known — a runtime phi over match/if arms (each possibly a
			// different concrete type) or a nested constructor (multiple live heap
			// temps). A single static dropFunc would run the WRONG concrete drop on
			// the non-selected arm, so the goroutine-side drop dispatches at runtime
			// through the value's typeinfo drop_fn_ptr via __promise_structural_drop.
			// claimHeapTemp runtime-compares v's instance pointer against each tracked
			// heap temp and clears exactly the live arm's caller flag (non-taken arms
			// hold null; nested-ctor inner temps were already claimed into the outer
			// at construction). Move params: the callee consumes and drops via the
			// same structural dispatch, so no goroutine-side drop is recorded.
			if _, ok := v.Type().(*irtypes.StructType); ok && len(newHeap) >= 1 && c.structuralDrop != nil {
				c.claimHeapTemp(v)
				if !isMove {
					argBorrowDrops = append(argBorrowDrops, goArgBorrowDrop{
						paramIdx: i, dropFunc: c.structuralDrop, isStruct: true,
					})
				}
				continue
			}

			// T1106 Case B: a string/vector argument whose root is a runtime phi over
			// multiple owned temps (`if c { s1.clone() } else { s2.clone() }`). The
			// drop is homogeneous (all string, or same-element vector), so a single
			// static dropFunc over the selected temp suffices. clearMatchingStmtTemps
			// clears the live arm's caller flag by runtime comparison; intermediates
			// (e.g. the trim() result in `s.trim().clone()`) have a different pointer
			// and stay with the caller.
			if _, ok := v.Type().(*irtypes.StructType); !ok && len(newStmt) >= 1 {
				at := argTypes[i]
				_, isVec := types.AsVector(at)
				if at != nil && (extractNamed(at) == types.TypString || isVec) {
					rep := newStmt[len(newStmt)-1]
					c.clearMatchingStmtTemps(v, newStmt)
					if !isMove {
						argBorrowDrops = append(argBorrowDrops, goArgBorrowDrop{
							paramIdx: i, dropFunc: rep.dropFunc, elemType: rep.elemType,
						})
					}
					continue
				}
			}

			// T1198: a bare-ident borrowed heap value param of the spawning function
			// (no owned temp, no drop flag) is async-read by the coroutine after the
			// caller frees its own borrowed-arg stmt-temps → UAF. Dup it at spawn time
			// so the goroutine owns a private copy, and record a goroutine-side drop of
			// the dup (the caller's borrow is untouched → no double-free; the dup is
			// freed → no leak). Sibling of T0688/T0731 (value-block) and T1197
			// (via-block); this is the fast bare-ident free-function path. Skip moves
			// (handled below — the callee consumes them).
			if id, ok := arg.Value.(*ast.IdentExpr); ok && !isMove && c.borrowedValueParams[id.Name] {
				capType := argTypes[i]
				if c.typeSubst != nil && capType != nil {
					capType = types.Substitute(capType, c.typeSubst) // monomorphization
				}
				// Channels are refcounted (B0163 loop below) — sharing the pointer is
				// fine. Copy/Arc/Task/value types embed data and never alias caller heap.
				if _, isCh := types.AsChannel(capType); !isCh && goElemNeedsBorrowedCaptureDup(capType) {
					argVals[i] = c.dupBorrowedCaptureForResult(argVals[i], capType)
					argBorrowDrops = append(argBorrowDrops, goArgBorrowDrop{paramIdx: i, capType: capType})
					continue
				}
			}

			// T1148: a moved NAMED variable root (e.g. `go f(move x)` where x is a
			// local/loop variable bound to a heap value) has no temporary to
			// transfer, but its caller-side drop flag must still be cleared — the
			// move param's callee consumes and drops it. Without this, both the
			// caller's scope/loop teardown AND the callee free it → double free.
			if isMove {
				if ident, ok := arg.Value.(*ast.IdentExpr); ok {
					c.clearDropFlag(ident.Name)
				}
			}
			continue // plain ident/literal, or no single static root to transfer
		}

		// Claim the root for the goroutine frame: caller cleanup must skip it.
		if c.block != nil && c.block.Term == nil {
			c.block.NewStore(constant.NewInt(irtypes.I1, 0), rootFlag)
		}

		// Move (consume) param: the callee owns and drops the temporary itself,
		// so the goroutine emits no drop (that would be a double-free).
		if i < len(calleeParams) && calleeParams[i].Ref() == types.RefMut {
			continue
		}
		// Borrow param: the goroutine frame drops the temporary after the call.
		argBorrowDrops = append(argBorrowDrops, d)
	}
	// T1108/T1154: remove inline enum-constructor temps produced for go-call args
	// from the caller's statement-end cleanup — ownership is transferred to the
	// goroutine frame (borrow params drop via the synthesized enum drop fn after
	// the call; move params are consumed by the callee), not freed by a
	// synchronous statement-end drop that could race the goroutine's read.
	c.enumCtorTemps = c.enumCtorTemps[:savedGoEnumTemps]

	// B0163: Increment refcount for channel arguments passed to go calls.
	chanTypeDC := channelStructType()
	for i, arg := range callExpr.Args {
		if ident, ok := arg.Value.(*ast.IdentExpr); ok {
			if binding, ok := c.dropBindings[ident.Name]; ok {
				elemType, isCh := types.AsChannel(binding.valType)
				if isCh || binding.named == types.TypChannel {
					chPtr := c.block.NewBitCast(argVals[i], irtypes.NewPointer(chanTypeDC))
					rcField := c.block.NewGetElementPtr(chanTypeDC, chPtr,
						constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(chanFieldRefcount)))
					c.emitAtomicAdd(c.block, rcField, constant.NewInt(irtypes.I64, 1), irtypes.I64)

					// T1158: balance the increment with a goroutine-side drop. The
					// goroutine borrows the channel (a plain ident is never a `move`,
					// which would not be an *ast.IdentExpr); without a matching
					// decrement the refcount never reaches 0 and the channel + its
					// buffers leak (5 allocations). Channel[T].drop's atomic refcount
					// gates the actual free, so caller and goroutine drops are safe in
					// any order. Resolve the element type so buffered T items drop too.
					if elemType == nil && binding.named == types.TypChannel && c.typeSubst != nil {
						if tp := types.TypChannel.TypeParams(); len(tp) > 0 {
							elemType = c.typeSubst[tp[0]]
						}
					}
					if c.typeSubst != nil && elemType != nil {
						elemType = types.Substitute(elemType, c.typeSubst)
					}
					argBorrowDrops = append(argBorrowDrops, goArgBorrowDrop{
						paramIdx: i, dropFunc: c.getOrCreateChannelDrop(elemType),
					})
				}
			}
		}
	}

	// 3. Resolve the target function
	targetFn, ext := c.resolveGoTarget(callExpr)

	// If target is an extern, generate a wrapper to handle sret/ABI coercion.
	// Extern functions use void return + sret pointer for struct returns, which
	// is incompatible with the coroutine body's direct call + store pattern.
	if ext != nil {
		targetFn = c.genGoExternWrapper(ext, argLLVMTypes, argTypes, resultLLVM, isVoid)
	}

	// 4. Create coroutine wrapper function
	coroName := coroRampName("goroutine", c.coroEnclosingQualifier(c.fn), c.goCounter) // T1222: qualify by enclosing to keep symbol unique across split units
	c.goCounter++

	var coroParams []*ir.Param
	for i := range argVals {
		coroParams = append(coroParams, ir.NewParam(fmt.Sprintf("arg.%d", i), argLLVMTypes[i]))
	}
	coroFn := c.module.NewFunc(coroName, irtypes.I8Ptr, coroParams...)
	coroFn.FuncAttrs = append(coroFn.FuncAttrs, rawFuncAttr("presplitcoroutine"))
	c.attributeCoroToEnclosing(coroName, c.fn) // T1222: same split unit as spawner

	// 5. Build coroutine body
	entry := coroFn.NewBlock(".entry")

	// Coroutine preamble
	coroId := entry.NewCall(c.coroId,
		constant.NewInt(irtypes.I32, 0),
		constant.NewNull(irtypes.I8Ptr),
		constant.NewNull(irtypes.I8Ptr),
		constant.NewNull(irtypes.I8Ptr))

	need := entry.NewCall(c.coroAlloc, coroId)
	allocBlk := coroFn.NewBlock("coro.alloc")
	startBlk := coroFn.NewBlock("coro.start")
	entry.NewCondBr(need, allocBlk, startBlk)

	coroSizeVal := allocBlk.NewCall(c.coroSize)
	var coroSizeArg value.Value = coroSizeVal
	if c.isWasm {
		coroSizeArg = allocBlk.NewZExt(coroSizeVal, irtypes.I64)
	}
	mem := allocBlk.NewCall(c.palAlloc, coroSizeArg)
	allocBlk.NewBr(startBlk)

	phiMem := startBlk.NewPhi(
		ir.NewIncoming(constant.NewNull(irtypes.I8Ptr), entry),
		ir.NewIncoming(mem, allocBlk))
	hdl := startBlk.NewCall(c.coroBegin, coroId, phiMem)

	// Initial suspend
	initResult := startBlk.NewCall(c.coroSuspend, constant.None, constant.False)

	suspendBlk := coroFn.NewBlock("coro.suspend")
	bodyBlk := coroFn.NewBlock("body")
	cleanupBlk := coroFn.NewBlock("cleanup")

	startBlk.NewSwitch(initResult, suspendBlk,
		ir.NewCase(constant.NewInt(irtypes.I8, 0), bodyBlk),
		ir.NewCase(constant.NewInt(irtypes.I8, 1), cleanupBlk))

	// Suspend: ramp returns handle
	suspendBlk.NewRet(hdl)

	// Body: call target function with args (preserved in coro frame)
	var callArgs []value.Value
	for i := range coroParams {
		callArgs = append(callArgs, coroFn.Params[i])
	}

	// T0147: Create panic exit block for go-call coroutine.
	// Transfers panic state from TLS to G struct, clears TLS, branches to final suspend.
	goPanicExitDC := coroFn.NewBlock("go.panic_exit")

	if !isVoid {
		result := bodyBlk.NewCall(targetFn, callArgs...)

		// T1098: drop borrow-param argument temporaries owned by this goroutine
		// frame. Promise uses panic-via-flag (not unwinding), so the call always
		// returns and every exit path runs this.
		bodyBlk = c.emitGoArgBorrowDrops(coroFn, startBlk, bodyBlk, argBorrowDrops)

		// T0147: Check panic flag after call — skip result store if panicked.
		dcFlag := bodyBlk.NewLoad(irtypes.I8, c.panicFlagGlobal)
		dcIsPanic := bodyBlk.NewICmp(enum.IPredNE, dcFlag, constant.NewInt(irtypes.I8, 0))
		dcOkBlk := coroFn.NewBlock("go.call_ok")
		bodyBlk.NewCondBr(dcIsPanic, goPanicExitDC, dcOkBlk)
		bodyBlk = dcOkBlk

		if c.goExprFireAndForget {
			// T1159: fire-and-forget non-void has no receiver and result_ptr is null
			// (the caller skips the buffer at the `!c.goExprFireAndForget` gate below).
			// Drop the discarded result here so a value-returning go-spawn doesn't leak.
			// emitVariantFieldDrop is the canonical drop-by-type (string/struct/vector/
			// closure/…), the same helper task drop uses on the result buffer. It operates
			// on c.block and may create new blocks (c.newBlock) / entry allocas
			// (c.entryBlock) — both must target coroFn's frame, not the outer caller's,
			// so swap c.fn/c.block/c.entryBlock around it (the fast path otherwise threads
			// blocks via the local bodyBlk without touching c.fn).
			savedFn, savedBlock, savedEntry := c.fn, c.block, c.entryBlock
			c.fn, c.block, c.entryBlock = coroFn, bodyBlk, startBlk
			c.emitVariantFieldDrop(result, callResultType)
			bodyBlk = c.block
			c.fn, c.block, c.entryBlock = savedFn, savedBlock, savedEntry
		} else {
			// Store result via G.result_ptr (set by caller before enqueue).
			gTy := goroutineStructType()
			currentG := bodyBlk.NewLoad(irtypes.I8Ptr, c.currentGGlobal)
			gPtr := bodyBlk.NewBitCast(currentG, irtypes.NewPointer(gTy))
			rpField := bodyBlk.NewGetElementPtr(gTy, gPtr,
				constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(gFieldResultPtr)))
			rpVal := bodyBlk.NewLoad(irtypes.I8Ptr, rpField)
			rpNotNull := bodyBlk.NewICmp(enum.IPredNE, rpVal, constant.NewNull(irtypes.I8Ptr))
			storeResultBlk := coroFn.NewBlock("store_result")
			afterStoreBlk := coroFn.NewBlock("after_store")
			bodyBlk.NewCondBr(rpNotNull, storeResultBlk, afterStoreBlk)

			typedRP := storeResultBlk.NewBitCast(rpVal, irtypes.NewPointer(resultLLVM))
			storeResultBlk.NewStore(result, typedRP)
			storeResultBlk.NewBr(afterStoreBlk)

			bodyBlk = afterStoreBlk
		}
	} else {
		bodyBlk.NewCall(targetFn, callArgs...)

		// T1098: drop borrow-param argument temporaries (see non-void branch).
		bodyBlk = c.emitGoArgBorrowDrops(coroFn, startBlk, bodyBlk, argBorrowDrops)

		// T0147: Check panic flag after call.
		dcFlag := bodyBlk.NewLoad(irtypes.I8, c.panicFlagGlobal)
		dcIsPanic := bodyBlk.NewICmp(enum.IPredNE, dcFlag, constant.NewInt(irtypes.I8, 0))
		dcOkBlk := coroFn.NewBlock("go.call_ok")
		bodyBlk.NewCondBr(dcIsPanic, goPanicExitDC, dcOkBlk)
		bodyBlk = dcOkBlk
	}

	// Final suspend: yield back to scheduler so it can see coro.done()=true
	// before destroying the coroutine frame.
	finalSuspBlk := coroFn.NewBlock("final.suspend")
	bodyBlk.NewBr(finalSuspBlk)

	// T0147: Define panic exit block — transfer panic state from TLS to G struct.
	{
		gTy := goroutineStructType()
		peCurrentG := goPanicExitDC.NewLoad(irtypes.I8Ptr, c.currentGGlobal)
		peGPtr := goPanicExitDC.NewBitCast(peCurrentG, irtypes.NewPointer(gTy))

		pePanicType := goPanicExitDC.NewLoad(irtypes.I8, c.panicTypeTlsGlobal)
		pePanickedField := goPanicExitDC.NewGetElementPtr(gTy, peGPtr,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(gFieldPanicked)))
		goPanicExitDC.NewStore(pePanicType, pePanickedField)

		pePanicMsg := goPanicExitDC.NewLoad(irtypes.I8Ptr, c.panicMsgTlsGlobal)
		pePanicMsgField := goPanicExitDC.NewGetElementPtr(gTy, peGPtr,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(gFieldPanicMsg)))
		goPanicExitDC.NewStore(pePanicMsg, pePanicMsgField)

		goPanicExitDC.NewStore(constant.NewInt(irtypes.I8, 0), c.panicFlagGlobal)
		goPanicExitDC.NewStore(constant.NewNull(irtypes.I8Ptr), c.panicMsgTlsGlobal)
		goPanicExitDC.NewStore(constant.NewInt(irtypes.I8, 0), c.panicTypeTlsGlobal)

		goPanicExitDC.NewBr(finalSuspBlk)
	}

	// Cleanup: free coroutine memory (only reached via destroy path)
	coroMem := cleanupBlk.NewCall(c.coroFree, coroId, hdl)
	needFree := cleanupBlk.NewICmp(enum.IPredNE, coroMem, constant.NewNull(irtypes.I8Ptr))
	freeBlk := coroFn.NewBlock("coro.free")
	doneBlk := coroFn.NewBlock("coro.done")
	cleanupBlk.NewCondBr(needFree, freeBlk, doneBlk)

	freeBlk.NewCall(c.palFree, coroMem)
	freeBlk.NewBr(doneBlk)

	// Done: single coro.end (both final-suspend exit and cleanup converge here)
	doneBlk.NewCall(c.coroEnd, hdl, constant.False, constant.None)
	doneBlk.NewRet(hdl)

	// Final suspend switch: default/i8 0 → doneBlk (skip free, just coro.end+ret)
	// i8 1 (destroy) → cleanup (free frame then coro.end+ret)
	finalResult := finalSuspBlk.NewCall(c.coroSuspend, constant.None, constant.True)
	finalSuspBlk.NewSwitch(finalResult, doneBlk,
		ir.NewCase(constant.NewInt(irtypes.I8, 0), doneBlk),
		ir.NewCase(constant.NewInt(irtypes.I8, 1), cleanupBlk))

	// 6. Caller: call ramp, create G, set up result storage, enqueue
	handle := c.block.NewCall(coroFn, argVals...)
	gRaw := c.block.NewCall(c.funcs["promise_g_new"], handle)

	if !c.goExprFireAndForget {
		gTy := goroutineStructType()
		gPtr := c.block.NewBitCast(gRaw, irtypes.NewPointer(gTy))
		rpField := c.block.NewGetElementPtr(gTy, gPtr,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(gFieldResultPtr)))
		if !isVoid {
			// Task[T]: allocate result buffer and store in G.result_ptr.
			// The coroutine body stores the result here; the receiver loads + frees it.
			resultSize := constant.NewInt(irtypes.I64, int64(c.typeSize(resultLLVM)))
			resultBuf := c.block.NewCall(c.palAlloc, resultSize)
			c.block.NewStore(resultBuf, rpField)
		} else {
			// Void task: set result_ptr to sentinel (0x1) so goroutine_exit
			// knows this is a task and won't free G (caller frees via <-task)
			sentinel := c.block.NewIntToPtr(constant.NewInt(c.ptrIntType(), 1), irtypes.I8Ptr)
			c.block.NewStore(sentinel, rpField)
		}
	}
	// Fire-and-forget (void or non-void): result_ptr stays null (from
	// promise_g_new), so goroutine_exit frees the G struct. The coro body
	// null-checks result_ptr before storing (B0109).

	c.block.NewCall(c.funcs["promise_sched_enqueue"], gRaw)

	return gRaw
}

// resolveGoTarget resolves the IR function for a call expression used in `go func()`.
// Returns the target function and, if it's an extern, the ExternFunc info.
func (c *Compiler) resolveGoTarget(callExpr *ast.CallExpr) (*ir.Func, *ExternFunc) {
	if ident, ok := callExpr.Callee.(*ast.IdentExpr); ok {
		if ext, ok := c.externs[ident.Name]; ok {
			return ext.IRFunc, ext
		}
		if fn, ok := c.funcs[ident.Name]; ok {
			return fn, nil
		}
	}
	// Method call or complex callee — wrap in a thunk
	// For now, only support direct function calls
	panic(fmt.Sprintf("codegen: go expression callee %T not yet supported", callExpr.Callee))
}

// genGoCallExprViaBlock handles `go expr()` where the callee is not a simple
// function name — method calls (obj.method()), module-qualified calls (mod.func()),
// generic calls with explicit type args (identity[int]()), etc. (B0113)
//
// Uses the genGoBlock pattern: captures outer locals, creates a coroutine with
// full codegen context, and generates the call via genExpr inside the body.
// Unlike genGoBlock, supports non-void results for Task[T].
func (c *Compiler) genGoCallExprViaBlock(callExpr *ast.CallExpr, failable bool) value.Value {
	// 1. Determine result type. T1379: `go! obj.method()` stores the failable
	// aggregate {i1 ok, T value, i8* err} returned by the failable target — the
	// same lowering the fast path (genGoCallExpr) uses, surfaced at `<-t`.
	callResultType := c.info.Types[callExpr]
	isVoid := (callResultType == nil || callResultType == types.TypVoid)
	var resultLLVM irtypes.Type = irtypes.Void
	if failable {
		var inner irtypes.Type = irtypes.Void
		if !isVoid {
			inner = c.resolveType(callResultType)
		}
		resultLLVM = computeResultType(inner)
		isVoid = false
	} else if !isVoid {
		resultLLVM = c.resolveType(callResultType)
	}

	// 2. Collect outer variables referenced in the call expression.
	// Wrap call in a synthetic block so we can reuse collectBlockIdents.
	syntheticBlock := &ast.Block{
		Stmts: []ast.Stmt{&ast.ExprStmt{Expr: callExpr}},
	}
	captureNames, captureIdents := c.collectBlockIdents(syntheticBlock, c.locals)

	// 3. Load captured values in caller scope
	var captureVals []value.Value
	var captureLLVMTypes []irtypes.Type
	var thisSnapshot *goThisSnapshot // T1261: coro-side setup for a captured `this` (sibling of T1219's genGoBlock)
	for _, name := range captureNames {
		if name == "this" {
			// T1261: capture a private snapshot of the receiver — the coroutine can
			// outlive the spawning method's borrowed receiver temp, so it must never
			// alias `this` (else re-derefing `this.field` inside the coro is a UAF).
			// Mirrors genGoBlock's T1219 handling; runs in the enclosing method's
			// context (before the saveState/context switch), where c.locals["this"],
			// c.monoCtx, c.currentNamed and c.typeSubst are still valid.
			snapVal, snapType, snap := c.snapshotThisForGoBlock()
			captureVals = append(captureVals, snapVal)
			captureLLVMTypes = append(captureLLVMTypes, snapType)
			thisSnapshot = snap
			continue
		}
		alloca := c.locals[name]
		elemType := alloca.ElemType
		val := c.block.NewLoad(elemType, alloca)
		captureVals = append(captureVals, val)
		captureLLVMTypes = append(captureLLVMTypes, elemType)
	}

	// B0163: Increment refcount for captured channel variables and collect their types.
	chanTypeVB := channelStructType()
	capturedChanTypesVB := make(map[string]types.Type)
	for i, name := range captureNames {
		if binding, ok := c.dropBindings[name]; ok {
			if _, isCh := types.AsChannel(binding.valType); isCh || binding.named == types.TypChannel {
				chPtr := c.block.NewBitCast(captureVals[i], irtypes.NewPointer(chanTypeVB))
				rcField := c.block.NewGetElementPtr(chanTypeVB, chPtr,
					constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(chanFieldRefcount)))
				c.emitAtomicAdd(c.block, rcField, constant.NewInt(irtypes.I64, 1), irtypes.I64)
				capturedChanTypesVB[name] = binding.valType
			}
		}
	}

	// B0354: Collect droppable non-channel captures for ownership transfer.
	capturedDroppablesVB := make(map[string]types.Type)
	// T1640: names whose OUTER binding was a closure env-free. The goroutine-side
	// re-registration must reproduce that KIND; it cannot be re-derived from
	// valType, which holds the ELEMENT type for an optional binding.
	capturedEnvOwnersVB := make(map[string]bool)
	for _, name := range captureNames {
		if _, isChannel := capturedChanTypesVB[name]; isChannel {
			continue
		}
		if binding, ok := c.dropBindings[name]; ok {
			capturedDroppablesVB[name] = binding.valType
			if binding.kind == bindingFreeEnv {
				capturedEnvOwnersVB[name] = true
			}
		}
	}

	// T1197: dup borrowed heap captured params (receiver + args) so the coroutine
	// owns private copies. The captures are read inside the coroutine, which may
	// run long after the spawning function returns and drops its borrowed-arg
	// stmt-temps — without a spawn-side dup those reads alias freed memory (UAF /
	// heap corruption). Borrowed value params carry no outer drop binding, so
	// B0354's ownership-transfer never covers them; mirror T0688's value-block dup.
	// Applies to void, non-void, and fire-and-forget alike — the async read of the
	// captures is independent of the result type.
	for idx, name := range captureNames {
		if !c.borrowedValueParams[name] {
			continue // only borrowed value params of the spawning function
		}
		if _, hasChan := capturedChanTypesVB[name]; hasChan {
			continue // refcounted channel — sharing the pointer is fine
		}
		if _, hasDrop := capturedDroppablesVB[name]; hasDrop {
			continue // owned local — B0354 already transfers ownership
		}
		ident := captureIdents[name]
		if ident == nil {
			continue // no representative ident (e.g. lambda-only capture)
		}
		capType := c.info.Types[ident]
		if c.typeSubst != nil && capType != nil {
			capType = types.Substitute(capType, c.typeSubst) // monomorphization
		}
		if !goElemNeedsBorrowedCaptureDup(capType) {
			continue // Copy / Arc / Task / value types don't alias caller heap
		}
		captureVals[idx] = c.dupBorrowedCaptureForResult(captureVals[idx], capType)
		capturedDroppablesVB[name] = capType // goroutine owns the dup → B0354 drop
	}

	// 4. Create coroutine function with captured values as parameters
	coroName := coroRampName("goroutine", c.coroEnclosingQualifier(c.fn), c.goCounter) // T1222: qualify by enclosing to keep symbol unique across split units
	c.goCounter++
	var coroParams []*ir.Param
	for i, name := range captureNames {
		coroParams = append(coroParams, ir.NewParam(name+".cap", captureLLVMTypes[i]))
	}
	coroFn := c.module.NewFunc(coroName, irtypes.I8Ptr, coroParams...)
	coroFn.FuncAttrs = append(coroFn.FuncAttrs, rawFuncAttr("presplitcoroutine"))
	c.attributeCoroToEnclosing(coroName, c.fn) // T1222: same split unit as spawner

	// 5. Save and switch context
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
	savedInCoroutine := c.inCoroutine
	savedCoroCleanup := c.coroCleanupBlk
	savedCoroSuspend := c.coroSuspendBlk
	savedPanicExitBlock := c.panicExitBlock
	savedCoroutineReturnBlock := c.coroutineReturnBlock
	savedGoExprFF := c.goExprFireAndForget
	savedLocalNameCount := c.localNameCount           // T0261
	savedStmtTemps := c.stmtTemps                     // T0594: stmtTemps must not leak from coroutine body into outer function
	savedStmtTempMap := c.stmtTempMap                 // T0594: allocas created inside coroutine body live in a different function
	savedEnumCtorTemps := c.enumCtorTemps             // B0267: enumCtorTemps must not leak from coroutine body into outer function
	savedHeapTemps := c.heapTemps                     // T1105: isolate coro-body heap temps from the outer fn
	savedHeapTempMap := c.heapTempMap                 // T1105
	savedEnvTemps := c.envTemps                       // T1105: isolate coro-body closure env temps from the outer fn
	savedEnvTempMap := c.envTempMap                   // T1105
	savedBorrowedValueParams := c.borrowedValueParams // T0945
	savedInFailableGoBlock := c.inFailableGoBlock     // T1384: a nested `go x()` inside a `go! {}` body must not inherit the outer sink
	savedBlockTempFloors := c.resetBlockTempFloors()  // T1329: fresh function → floors from 0
	c.fn = coroFn
	c.locals = make(map[string]*ir.InstAlloca)
	c.localNameCount = make(map[string]int)
	c.blockCounter = 0
	c.canError = false
	c.currentRetType = types.TypVoid
	c.borrowedValueParams = nil // T0945: coroutine body has no user value params
	c.scopeBindings = nil
	c.dropFlags = make(map[string]*ir.InstAlloca)
	c.castSubjectMatch = nil // T0849: fresh per function body; restored below
	c.dropBindings = make(map[string]scopeBinding)
	c.loopScopeDepth = 0
	c.inCoroutine = true
	c.inFailableGoBlock = false               // T1384: the call form handles failability via the failable aggregate it stores, not the sink
	c.stmtTemps = nil                         // T0594: fresh temp state for coroutine body
	c.stmtTempMap = make(map[value.Value]int) // T0594
	c.enumCtorTemps = nil                     // B0267
	c.heapTemps = nil                         // T1105: coro-body heap temps reference coroFn allocas
	c.heapTempMap = make(map[value.Value]int) // T1105
	c.envTemps = nil                          // T1105
	c.envTempMap = make(map[value.Value]int)  // T1105

	// 6. Coroutine preamble
	entry := coroFn.NewBlock(".entry")
	c.block = entry

	coroId := entry.NewCall(c.coroId,
		constant.NewInt(irtypes.I32, 0),
		constant.NewNull(irtypes.I8Ptr),
		constant.NewNull(irtypes.I8Ptr),
		constant.NewNull(irtypes.I8Ptr))

	need := entry.NewCall(c.coroAlloc, coroId)
	allocBlk := coroFn.NewBlock("coro.alloc")
	startBlk := coroFn.NewBlock("coro.start")
	entry.NewCondBr(need, allocBlk, startBlk)

	coroSizeVal := allocBlk.NewCall(c.coroSize)
	var coroSizeArg value.Value = coroSizeVal
	if c.isWasm {
		coroSizeArg = allocBlk.NewZExt(coroSizeVal, irtypes.I64)
	}
	mem := allocBlk.NewCall(c.palAlloc, coroSizeArg)
	allocBlk.NewBr(startBlk)

	phiMem := startBlk.NewPhi(
		ir.NewIncoming(constant.NewNull(irtypes.I8Ptr), entry),
		ir.NewIncoming(mem, allocBlk))
	hdl := startBlk.NewCall(c.coroBegin, coroId, phiMem)

	// Store captured params into allocas (after coro.begin → part of frame)
	var thisValAlloca *ir.InstAlloca // T1261: value-struct alloca backing a captured `this`
	for i, name := range captureNames {
		if thisSnapshot != nil && name == "this" {
			// T1261: the captured param is a value struct (heap: {vtable,instance};
			// value type: the full value struct). Store it in a frame alloca, then
			// derive the i8* `this` that field access expects:
			//   heap type  → instance pointer (value struct field 1)
			//   value type → address of the value struct in the coroutine frame
			// The value-struct alloca backs the goroutine's private-copy drop.
			// Mirrors genGoBlock's T1219 site (expr.go).
			valAlloca := startBlk.NewAlloca(captureLLVMTypes[i])
			valAlloca.SetName(c.uniqueLocalName("this.val.addr"))
			startBlk.NewStore(coroFn.Params[i], valAlloca)

			i8Alloca := startBlk.NewAlloca(irtypes.I8Ptr)
			i8Alloca.SetName(c.uniqueLocalName("this.addr"))
			var thisI8 value.Value
			if thisSnapshot.isValueType {
				thisI8 = startBlk.NewBitCast(valAlloca, irtypes.I8Ptr)
			} else {
				thisI8 = startBlk.NewExtractValue(coroFn.Params[i], 1)
			}
			startBlk.NewStore(thisI8, i8Alloca)
			c.locals["this"] = i8Alloca
			thisValAlloca = valAlloca
			continue
		}
		alloca := startBlk.NewAlloca(captureLLVMTypes[i])
		alloca.SetName(c.uniqueLocalName(name + ".addr"))
		startBlk.NewStore(coroFn.Params[i], alloca)
		c.locals[name] = alloca
	}

	// B0163: Register drop bindings for captured channel variables inside the goroutine.
	c.entryBlock = startBlk
	c.block = startBlk

	// T1261: register the drop for the goroutine's private heap snapshot of `this`
	// so it is freed at coroutine scope exit (value-type snapshots are Copy — no
	// drop). Mirrors genGoBlock's T1219 site.
	if thisSnapshot != nil && !thisSnapshot.isValueType && thisValAlloca != nil {
		c.maybeRegisterDrop("this", thisValAlloca, thisSnapshot.resolvedType)
	}

	for _, name := range captureNames {
		if chanValType, ok := capturedChanTypesVB[name]; ok {
			alloca := c.locals[name]
			c.maybeRegisterDrop(name, alloca, chanValType)
		}
	}

	// B0354: Register drop bindings for non-channel droppable captures.
	for _, name := range captureNames {
		if valType, ok := capturedDroppablesVB[name]; ok {
			alloca := c.locals[name]
			c.registerGoCaptureOwnership(name, alloca, valType, capturedEnvOwnersVB[name])
		}
	}

	// Initial suspend
	initSuspBlk := coroFn.NewBlock("coro.init.suspend")
	startBlk.NewBr(initSuspBlk)

	initResult := initSuspBlk.NewCall(c.coroSuspend, constant.None, constant.False)

	suspendBlk := coroFn.NewBlock("coro.suspend")
	bodyBlk := coroFn.NewBlock("body")
	cleanupBlk := coroFn.NewBlock("cleanup")
	doneBlk := coroFn.NewBlock("coro.done")

	initSuspBlk.NewSwitch(initResult, suspendBlk,
		ir.NewCase(constant.NewInt(irtypes.I8, 0), bodyBlk),
		ir.NewCase(constant.NewInt(irtypes.I8, 1), cleanupBlk))

	suspendBlk.NewRet(hdl)

	c.coroCleanupBlk = cleanupBlk
	c.coroSuspendBlk = doneBlk

	// 7. Body: generate the call expression and optionally store result
	c.block = bodyBlk
	c.entryBlock = startBlk

	// B0228: Create panic exit block for the go block coroutine.
	// When a panic occurs, transfer panic state to G struct and branch to final suspend.
	goPanicExitBlk := coroFn.NewBlock("go.panic_exit")
	c.panicExitBlock = goPanicExitBlk

	result := c.genExpr(callExpr)

	// Clear panic exit block after body generation
	c.panicExitBlock = nil

	if !isVoid && result != nil && !savedGoExprFF {
		// T1159: only transfer+claim the result for a task handle. For fire-and-forget
		// the caller allocates no result buffer (G.result_ptr stays null below), so don't
		// store/claim — cleanupStmtTemps/cleanupHeapTemps/cleanupEnvTemps below drop the
		// discarded result instead. (Without this, the body would deref a null result_ptr
		// and the claim would suppress the very cleanup that should free the result.)
		// Store result via G.result_ptr (set by caller before enqueue)
		gTy := goroutineStructType()
		currentG := c.block.NewLoad(irtypes.I8Ptr, c.currentGGlobal)
		gPtr := c.block.NewBitCast(currentG, irtypes.NewPointer(gTy))
		rpField := c.block.NewGetElementPtr(gTy, gPtr,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(gFieldResultPtr)))
		rpVal := c.block.NewLoad(irtypes.I8Ptr, rpField)
		typedRP := c.block.NewBitCast(rpVal, irtypes.NewPointer(resultLLVM))
		c.block.NewStore(result, typedRP)
		// T0594: claim the result stmtTemp — ownership transferred to G.result_ptr.
		c.claimStringTemp(result)
		c.claimHeapTemp(result) // T1105: heap-struct result moved into G.result_ptr
		c.claimEnvTemp(result)  // T1105: closure result moved into G.result_ptr
	}

	// T0594: Clean up any remaining stmtTemps from the coroutine body before restoring
	// the outer function's context. Without this, temps created by genExpr (e.g., string
	// return values tracked by trackStringTemp) would be orphaned inside the coroutine.
	c.cleanupStmtTemps()
	c.cleanupHeapTemps() // T1105: drop orphaned trailing-expr heap intermediates inside the coro
	c.cleanupEnvTemps()  // T1105

	// T1156: Drop inline enum-ctor temps produced for the call's arguments inside
	// the coro body (via-block analogue of stmt.go's statement-end enum cleanup).
	// Borrow params keep the flag set → dropped here exactly once; move params had
	// the flag cleared by normal call codegen → skipped (callee consumes them).
	if c.block != nil && c.block.Term == nil {
		for _, et := range c.enumCtorTemps {
			flag := c.block.NewLoad(irtypes.I1, et.dropFlag)
			dropBlk := c.newBlock("enum.ctor.drop")
			skipBlk := c.newBlock("enum.ctor.skip")
			c.block.NewCondBr(flag, dropBlk, skipBlk)
			c.block = dropBlk
			ptr := c.block.NewLoad(irtypes.I8Ptr, et.alloca)
			c.block.NewCall(et.dropFunc, ptr)
			c.block.NewBr(skipBlk)
			c.block = skipBlk
		}
		c.enumCtorTemps = c.enumCtorTemps[:0]
	}

	// B0163: Emit cleanup for captured channel drop bindings.
	if c.block != nil && c.block.Term == nil && len(c.scopeBindings) > 0 {
		c.emitScopeCleanup(0, false)
	}

	// T0147: Per-call panic checks in genExpr now handle panic detection.
	// The call expression at line above goes through genExpr → case *ast.CallExpr
	// which calls emitPanicCheck() → emitPanicReturn() → branches to panicExitBlock.

	// Final suspend
	finalSuspBlk := coroFn.NewBlock("final.suspend")
	// T0148: Final panic check after body + scope cleanup.
	// Catches panics from drop functions during scope cleanup that per-call checks miss.
	if c.block != nil && c.block.Term == nil {
		finalFlag := c.block.NewLoad(irtypes.I8, c.panicFlagGlobal)
		finalIsPanic := c.block.NewICmp(enum.IPredNE, finalFlag, constant.NewInt(irtypes.I8, 0))
		c.block.NewCondBr(finalIsPanic, goPanicExitBlk, finalSuspBlk)
	}

	// B0228: Define the go block panic exit block body.
	// Transfer panic state from TLS to G struct, clear TLS flag, branch to final suspend.
	{
		gTy := goroutineStructType()
		peCurrentG := goPanicExitBlk.NewLoad(irtypes.I8Ptr, c.currentGGlobal)
		peGPtr := goPanicExitBlk.NewBitCast(peCurrentG, irtypes.NewPointer(gTy))

		// Load TLS panic type and store in G.panicked
		pePanicType := goPanicExitBlk.NewLoad(irtypes.I8, c.panicTypeTlsGlobal)
		pePanickedField := goPanicExitBlk.NewGetElementPtr(gTy, peGPtr,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(gFieldPanicked)))
		goPanicExitBlk.NewStore(pePanicType, pePanickedField)

		// Load TLS panic msg and store in G.panic_msg
		pePanicMsg := goPanicExitBlk.NewLoad(irtypes.I8Ptr, c.panicMsgTlsGlobal)
		pePanicMsgField := goPanicExitBlk.NewGetElementPtr(gTy, peGPtr,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(gFieldPanicMsg)))
		goPanicExitBlk.NewStore(pePanicMsg, pePanicMsgField)

		// Clear TLS panic flag and msg
		goPanicExitBlk.NewStore(constant.NewInt(irtypes.I8, 0), c.panicFlagGlobal)
		goPanicExitBlk.NewStore(constant.NewNull(irtypes.I8Ptr), c.panicMsgTlsGlobal)
		goPanicExitBlk.NewStore(constant.NewInt(irtypes.I8, 0), c.panicTypeTlsGlobal)

		// Branch to final suspend (properly ends the coroutine)
		goPanicExitBlk.NewBr(finalSuspBlk)
	}

	// Cleanup: free coroutine memory (only reached via destroy path)
	coroMem := cleanupBlk.NewCall(c.coroFree, coroId, hdl)
	needFree := cleanupBlk.NewICmp(enum.IPredNE, coroMem, constant.NewNull(irtypes.I8Ptr))
	freeBlk := coroFn.NewBlock("coro.free")
	cleanupBlk.NewCondBr(needFree, freeBlk, doneBlk)

	freeBlk.NewCall(c.palFree, coroMem)
	freeBlk.NewBr(doneBlk)

	// Done: single coro.end
	doneBlk.NewCall(c.coroEnd, hdl, constant.False, constant.None)
	doneBlk.NewRet(hdl)

	// Final suspend switch
	finalResult := finalSuspBlk.NewCall(c.coroSuspend, constant.None, constant.True)
	finalSuspBlk.NewSwitch(finalResult, doneBlk,
		ir.NewCase(constant.NewInt(irtypes.I8, 0), doneBlk),
		ir.NewCase(constant.NewInt(irtypes.I8, 1), cleanupBlk))

	// 8. Restore context
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
	c.inCoroutine = savedInCoroutine
	c.inFailableGoBlock = savedInFailableGoBlock // T1384
	c.coroCleanupBlk = savedCoroCleanup
	c.coroSuspendBlk = savedCoroSuspend
	c.panicExitBlock = savedPanicExitBlock
	c.coroutineReturnBlock = savedCoroutineReturnBlock
	c.goExprFireAndForget = savedGoExprFF
	c.borrowedValueParams = savedBorrowedValueParams // T0945
	c.localNameCount = savedLocalNameCount           // T0261
	c.stmtTemps = savedStmtTemps                     // T0594: restore outer function's temp state
	c.stmtTempMap = savedStmtTempMap                 // T0594
	c.enumCtorTemps = savedEnumCtorTemps             // B0267
	c.heapTemps = savedHeapTemps                     // T1105
	c.heapTempMap = savedHeapTempMap                 // T1105
	c.envTemps = savedEnvTemps                       // T1105
	c.envTempMap = savedEnvTempMap                   // T1105
	c.restoreBlockTempFloors(savedBlockTempFloors)   // T1329

	// B0354: Clear outer drop flags for captured droppable non-channel variables.
	for name := range capturedDroppablesVB {
		c.clearDropFlag(name)
	}

	// 9. Caller: call coroutine ramp → get handle, create G, enqueue
	handle := c.block.NewCall(coroFn, captureVals...)
	gRaw := c.block.NewCall(c.funcs["promise_g_new"], handle)

	if !savedGoExprFF { // T1159: fire-and-forget allocates no result buffer (mirrors the fast path's `!c.goExprFireAndForget` gate in genGoCallExpr)
		gTy := goroutineStructType()
		gPtr := c.block.NewBitCast(gRaw, irtypes.NewPointer(gTy))
		rpField := c.block.NewGetElementPtr(gTy, gPtr,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(gFieldResultPtr)))
		if !isVoid {
			// Task[T]: allocate result buffer and store in G.result_ptr
			resultSize := constant.NewInt(irtypes.I64, int64(c.typeSize(resultLLVM)))
			resultBuf := c.block.NewCall(c.palAlloc, resultSize)
			c.block.NewStore(resultBuf, rpField)
		} else {
			// Void task: set result_ptr to sentinel (0x1)
			sentinel := c.block.NewIntToPtr(constant.NewInt(c.ptrIntType(), 1), irtypes.I8Ptr)
			c.block.NewStore(sentinel, rpField)
		}
	}

	c.block.NewCall(c.funcs["promise_sched_enqueue"], gRaw)

	return gRaw
}

// genGoExternWrapper generates a thin wrapper function around an extern call
// for use in go expressions. The wrapper takes Promise-internal argument types
// and returns the Promise-internal result type, handling sret/ABI coercion
// internally via genExternCall. This is needed because extern IR functions use
// void return + sret pointer for struct returns, which is incompatible with
// the coroutine body's direct call + store pattern (B0046).
func (c *Compiler) genGoExternWrapper(ext *ExternFunc, argLLVMTypes []irtypes.Type, argTypes []types.Type, resultLLVM irtypes.Type, isVoid bool) *ir.Func {
	// T1222: the go-block ramp is attributed to (and lands in) the same split unit
	// as its enclosing function. The ramp calls this wrapper, so the wrapper must be
	// co-located too — otherwise a `go extern()` inside a generic instance method
	// leaves the wrapper orphaned in main IR while the ramp goes to the instance
	// `.bc` → cross-program undefined-symbol at link. c.fn is still the enclosing
	// function here (genGoExternWrapper is called before the ramp is built and before
	// saveState switches c.fn), so qualify + attribute exactly like coroRampName.
	enclQual := c.coroEnclosingQualifier(c.fn)
	var wrapName string
	if enclQual == "" {
		wrapName = fmt.Sprintf(".go_extern_wrap.%s.%d", ext.PromiseName, c.goCounter)
	} else {
		wrapName = fmt.Sprintf(".go_extern_wrap.%s.%s.%d", ext.PromiseName, enclQual, c.goCounter)
	}

	var params []*ir.Param
	for i, ty := range argLLVMTypes {
		params = append(params, ir.NewParam(fmt.Sprintf("arg.%d", i), ty))
	}

	retType := irtypes.Type(irtypes.Void)
	if !isVoid {
		retType = resultLLVM
	}
	wrapFn := c.module.NewFunc(wrapName, retType, params...)
	c.attributeCoroToEnclosing(wrapName, c.fn) // T1222: same split unit as the ramp that calls it

	saved := c.saveState()
	defer c.restoreState(saved)

	c.fn = wrapFn
	entry := wrapFn.NewBlock("entry")
	c.block = entry
	c.entryBlock = entry
	c.locals = make(map[string]*ir.InstAlloca)
	c.localNameCount = make(map[string]int)
	c.dropFlags = make(map[string]*ir.InstAlloca)
	c.castSubjectMatch = nil // T0849: fresh per function body (saved/restored via saveState)
	c.dropBindings = make(map[string]scopeBinding)
	c.scopeBindings = nil

	var argVals []value.Value
	for i := range ext.ParamTypes {
		argVals = append(argVals, wrapFn.Params[i])
	}

	result := c.genExternCall(ext, argVals, argTypes)
	if result != nil && !isVoid {
		c.block.NewRet(result)
	} else {
		c.block.NewRet(nil)
	}

	return wrapFn
}

// collectBlockIdents walks an AST block and collects all IdentExpr names referenced.
// Returns a sorted, deduplicated list of names that exist in outerLocals, plus a
// map from each captured name to the first *ast.IdentExpr seen for it (T0731:
// used to resolve the capture's sema type via c.info.Types for the spawn-side
// borrowed-heap-param dup). Names collected only through a LambdaExpr capture set
// (which has no representative IdentExpr) are absent from the ident map.
func (c *Compiler) collectBlockIdents(block *ast.Block, outerLocals map[string]*ir.InstAlloca) ([]string, map[string]*ast.IdentExpr) {
	seen := make(map[string]bool)
	idents := make(map[string]*ast.IdentExpr)
	var walkExpr func(e ast.Expr)
	var walkStmt func(s ast.Stmt)

	walkExpr = func(e ast.Expr) {
		if e == nil {
			return
		}
		switch e := e.(type) {
		case *ast.IdentExpr:
			if _, ok := outerLocals[e.Name]; ok {
				seen[e.Name] = true
				if _, recorded := idents[e.Name]; !recorded {
					idents[e.Name] = e
				}
			}
		case *ast.ThisExpr:
			// T1219: `this` referenced inside a `go { }` block within a method.
			// The block is compiled into a separate coroutine that does not
			// inherit the method's `this`; thread the receiver into the arg pack
			// (marked via the "this" outer local) so genThisExpr resolves it.
			// No representative IdentExpr is recorded — genGoBlock builds the
			// capture value directly (a private snapshot, not the live receiver).
			if _, ok := outerLocals["this"]; ok {
				seen["this"] = true
			}
		case *ast.BinaryExpr:
			walkExpr(e.Left)
			walkExpr(e.Right)
		case *ast.UnaryExpr:
			walkExpr(e.Operand)
		case *ast.CallExpr:
			walkExpr(e.Callee)
			for _, arg := range e.Args {
				walkExpr(arg.Value)
			}
		case *ast.IndexExpr:
			walkExpr(e.Target)
			walkExpr(e.Index)
		case *ast.SliceExpr:
			walkExpr(e.Target)
			walkExpr(e.Low)
			walkExpr(e.High)
		case *ast.SliceTypeExpr:
			walkExpr(e.Inner)
		case *ast.MemberExpr:
			walkExpr(e.Target)
		case *ast.OptionalChainExpr:
			walkExpr(e.Target)
		case *ast.IsExpr:
			walkExpr(e.Expr)
		case *ast.CastExpr:
			walkExpr(e.Expr)
		case *ast.ErrorPropagateExpr:
			walkExpr(e.Expr)
		case *ast.ErrorPanicExpr:
			walkExpr(e.Expr)
		case *ast.OptionalUnwrapExpr:
			walkExpr(e.Expr)
		case *ast.AutoCloneExpr: // T0605
			walkExpr(e.Expr)
		case *ast.ErrorHandlerExpr:
			walkExpr(e.Expr)
			if e.Body != nil {
				for _, s := range e.Body.Stmts {
					walkStmt(s)
				}
			}
		case *ast.IfExpr:
			walkExpr(e.Cond)
			if e.Then != nil {
				for _, s := range e.Then.Stmts {
					walkStmt(s)
				}
			}
			if e.Else != nil {
				for _, s := range e.Else.Stmts {
					walkStmt(s)
				}
			}
		case *ast.MatchExpr:
			walkExpr(e.Subject)
			for _, arm := range e.Arms {
				// T1698: the `match true { <expr> => … }` dispatch form puts an
				// arbitrary expression in the PATTERN. Without this the outer local
				// it names never entered the arg pack, and generating the pattern
				// inside the coroutine panicked `undefined variable`. Every other
				// pattern shape binds names rather than referencing them.
				switch p := arm.Pattern.(type) {
				case *ast.ExpressionMatchPattern:
					walkExpr(p.Expr)
				case *ast.LiteralMatchPattern:
					walkExpr(p.Value)
				}
				walkExpr(arm.Body)
				if arm.Guard != nil {
					walkExpr(arm.Guard)
				}
				if arm.Block != nil {
					for _, s := range arm.Block.Stmts {
						walkStmt(s)
					}
				}
			}
		case *ast.StringLit:
			for _, part := range e.Parts {
				if interp, ok := part.(ast.StringInterp); ok {
					walkExpr(interp.Expr)
				}
			}
		case *ast.TupleLit:
			for _, elem := range e.Elements {
				walkExpr(elem)
			}
		case *ast.ArrayLit:
			for _, elem := range e.Elements {
				walkExpr(elem)
			}
		case *ast.ArrayRepeatLit:
			walkExpr(e.Value)
			walkExpr(e.Count)
		case *ast.MapLit:
			for _, entry := range e.Entries {
				walkExpr(entry.Key)
				walkExpr(entry.Value)
			}
		case *ast.GoExpr:
			if e.Expr != nil {
				walkExpr(e.Expr)
			}
			if e.Block != nil {
				for _, s := range e.Block.Stmts {
					walkStmt(s)
				}
			}
		case *ast.LambdaExpr:
			// T0740: a lambda inside a `go { }` block is compiled into a *separate*
			// coroutine function, so any outer-function local the lambda captures
			// must first be passed into the coroutine arg pack — otherwise
			// genLambdaExpr finds no alloca for the name and zero-initializes the
			// capture. Sema already computed the lambda's capture set (transitively
			// including nested-lambda captures via checkLambdaExpr's propagation);
			// collect those whose name is an outer local. We do NOT recurse into the
			// lambda body: sema's no-shadow rule guarantees bound names never alias
			// an outerLocals name, and block-locals are excluded by the outerLocals
			// filter (they are already in scope inside the coroutine).
			for _, cv := range c.info.LambdaCaptures[e] {
				name := cv.Obj.Name()
				if _, ok := outerLocals[name]; ok {
					seen[name] = true
				}
			}
		case *ast.ParenExpr:
			walkExpr(e.Expr)
		case *ast.UnsafeExpr:
			if e.Body != nil {
				for _, s := range e.Body.Stmts {
					walkStmt(s)
				}
			}
		}
	}

	walkStmt = func(s ast.Stmt) {
		if s == nil {
			return
		}
		switch s := s.(type) {
		case *ast.ExprStmt:
			walkExpr(s.Expr)
		case *ast.InferredVarDecl:
			walkExpr(s.Value)
		case *ast.TypedVarDecl:
			walkExpr(s.Value)
		case *ast.AssignStmt:
			walkExpr(s.Target)
			walkExpr(s.Value)
		case *ast.ReturnStmt:
			walkExpr(s.Value)
		case *ast.RaiseStmt:
			walkExpr(s.Value)
		case *ast.YieldStmt:
			walkExpr(s.Value)
		case *ast.IfStmt:
			walkExpr(s.Cond)
			walkExpr(s.Init)
			if s.Body != nil {
				for _, st := range s.Body.Stmts {
					walkStmt(st)
				}
			}
			if s.Else != nil {
				walkStmt(s.Else)
			}
		case *ast.ForInStmt:
			walkExpr(s.Iterable)
			if s.Body != nil {
				for _, st := range s.Body.Stmts {
					walkStmt(st)
				}
			}
		case *ast.ClassicForStmt:
			walkExpr(s.InitValue)
			walkExpr(s.Cond)
			walkExpr(s.UpdateTarget)
			walkExpr(s.UpdateValue)
			if s.Body != nil {
				for _, st := range s.Body.Stmts {
					walkStmt(st)
				}
			}
		case *ast.WhileStmt:
			walkExpr(s.Cond)
			if s.Body != nil {
				for _, st := range s.Body.Stmts {
					walkStmt(st)
				}
			}
		case *ast.WhileUnwrapStmt:
			walkExpr(s.Value)
			if s.Body != nil {
				for _, st := range s.Body.Stmts {
					walkStmt(st)
				}
			}
		case *ast.DestructureVarDecl:
			walkExpr(s.Value)
		case *ast.UseVarDecl:
			walkExpr(s.Value)
		case *ast.YieldDelegateStmt:
			walkExpr(s.Value)
		case *ast.InfiniteLoop:
			if s.Body != nil {
				for _, st := range s.Body.Stmts {
					walkStmt(st)
				}
			}
		case *ast.IncDecStmt:
			walkExpr(s.Target)
		case *ast.SelectStmt:
			for _, sc := range s.Cases {
				walkExpr(sc.Channel)
				walkExpr(sc.SendValue)
				for _, st := range sc.Body {
					walkStmt(st)
				}
			}
			for _, st := range s.Default {
				walkStmt(st)
			}
		case *ast.Block:
			for _, st := range s.Stmts {
				walkStmt(st)
			}
		}
	}

	for _, s := range block.Stmts {
		walkStmt(s)
	}

	names := make([]string, 0, len(seen))
	for name := range seen {
		names = append(names, name)
	}
	sort.Strings(names)
	return names, idents
}

// goElemNeedsBorrowedCaptureDup reports whether a value-block trailing value
// whose source is a borrowed captured parameter (no outer drop binding) must
// be dup'd before being stored into G.result_ptr. Without the dup, the loaded
// pointer aliases the caller's stmt-temp; the caller drops the temp
// immediately after spawning the goroutine, so the awaiter would load freed
// memory and the receiver's owned drop would double-free (T0688).
// Eligible heap types: string, Vector, droppable/no-drop heap user types.
// Excluded: Copy types (int/bool/...), Channel/Arc/Weak (refcounted — share
// is fine), Task/Mutex/MutexGuard (single-owner handles with no dup
// semantics), value types (embedded data, no heap aliasing).
func goElemNeedsBorrowedCaptureDup(goElem types.Type) bool {
	if goElem == nil {
		return false
	}
	named := extractNamed(goElem)
	if named == types.TypString {
		return true
	}
	if _, ok := types.AsVector(goElem); ok || named == types.TypVector {
		return true
	}
	// T0732: Map[K,V] and Set[T] are heap user types but are excluded from
	// isDroppableHeapUserType / isHeapUserNoDropPalFree by T0440's early
	// returns. That gating targets the dup-on-read clone() path
	// (cloneHeapElement), whose Promise-level clone() can be shallow for
	// nested-heap value args. The spawn-side T0688 dup does NOT use clone() —
	// it uses dupHeapValue (memcpy + field-wise deep dup), which correctly
	// deep-copies Map (vector of Slot enums via dupEnumElementInPlace) and Set
	// (recursive Map dup). So they ARE eligible here.
	if isMapOrSetType(goElem) {
		return true
	}
	if isDroppableHeapUserType(goElem) || isHeapUserNoDropPalFree(goElem) {
		return true
	}
	return false
}

// dupBorrowedCaptureForResult emits a dup of a borrowed-captured trailing
// value for the value-block path of `go { v }` (T0688). Dispatches by element
// type and uses c.block for IR emission (the dup helpers update c.block to a
// post-dup merge block, which the caller uses for the subsequent store).
// Vector[T] uses dupVector + emitVectorElementCloneLoop for a deep copy
// (heap element types like string would otherwise alias the original).
func (c *Compiler) dupBorrowedCaptureForResult(val value.Value, goElem types.Type) value.Value {
	named := extractNamed(goElem)
	switch {
	case named == types.TypString:
		return c.dupString(val)
	case types.IsVector(goElem):
		// extractNamed returns TypVector for both Vector[T] Instance AND bare
		// TypVector — check IsVector first so we use the right element size.
		vecElem, _ := types.AsVector(goElem)
		elemSize := int64(c.typeSize(c.resolveType(vecElem)))
		dup := c.dupVector(val, elemSize)
		c.emitVectorElementCloneLoop(dup, vecElem)
		return dup
	case named == types.TypVector:
		// Bare TypVector (element type unknown — rarely reached here since
		// `Vector` without type args is usually rejected by sema, but mirrors
		// the existing pattern in compiler.go).
		return c.dupVector(val, 0)
	case isMapOrSetType(goElem):
		// T0732: Map/Set deep-copy via dupHeapValue (memcpy + field-wise dup),
		// NOT the Promise clone() method. dupHeapValue walks the instance:
		// Map's _buckets vector clones each Slot enum element (key+value) via
		// dupEnumElementInPlace; Set's _map field recurses through dupHeapValue.
		return c.dupHeapValue(val, goElem)
	case isDroppableHeapUserType(goElem) || isHeapUserNoDropPalFree(goElem):
		return c.dupHeapValue(val, goElem)
	}
	return val
}

// goThisSnapshot records how a `this` captured by a `go { }` block (T1219) is
// set up on the coroutine side. nil means the default capture path (a plain
// scalar load — primitive-scalar receivers are Copy) is already safe.
type goThisSnapshot struct {
	isValueType  bool       // value-type receiver → copy value struct, no drop
	resolvedType types.Type // concrete receiver type (drop dispatch, heap only)
}

// snapshotThisForGoBlock builds a private snapshot of the method receiver for a
// `go { }` block capture (T1219). A goroutine can outlive the receiver's owner,
// so the coroutine must never alias `this`: heap receivers are deep-copied
// (dupHeapValue, like the T1196 borrowed-param dup) and owned+dropped by the
// goroutine; value-type receivers are copied by value into the coroutine frame
// (Copy, no heap, no drop); primitive-scalar receivers are already Copy so the
// default load suffices. Returns the capture value, its LLVM type, and the
// coroutine-side setup metadata (nil for the primitive-scalar / default path).
// Runs in the enclosing method's codegen context (before the coroutine switch).
func (c *Compiler) snapshotThisForGoBlock() (value.Value, irtypes.Type, *goThisSnapshot) {
	thisAlloca := c.locals["this"]
	resolvedType := c.currentNamedType()
	named := extractNamed(resolvedType)

	// Primitive-scalar receiver: `this` is the scalar value itself (Copy) — the
	// default load is a safe self-contained snapshot, no coroutine-side setup.
	if named == nil || isPrimitiveScalar(named) {
		val := c.block.NewLoad(thisAlloca.ElemType, thisAlloca)
		return val, thisAlloca.ElemType, nil
	}

	layout := c.lookupTypeLayout(resolvedType)
	if layout == nil || layout.Value == nil {
		panic(fmt.Sprintf("codegen: no layout for `this` receiver %s in go block", resolvedType))
	}

	if named.IsValueType() {
		// Value type: `this` is an i8* to the caller's value struct on the method
		// frame — copy it by value into the coroutine frame (Copy, no drop).
		thisI8 := c.block.NewLoad(thisAlloca.ElemType, thisAlloca)
		vsType := layout.Value.LLVMType
		typedPtr := c.block.NewBitCast(thisI8, irtypes.NewPointer(vsType))
		vstruct := c.block.NewLoad(vsType, typedPtr)
		return vstruct, vsType, &goThisSnapshot{isValueType: true, resolvedType: resolvedType}
	}

	// Heap type: `this` is an i8* instance pointer. Reconstruct the
	// `{vtable, instance}` value struct and deep-copy it (dupHeapValue), so the
	// goroutine owns a private instance it drops at scope exit.
	thisI8 := c.block.NewLoad(thisAlloca.ElemType, thisAlloca)
	vtable := c.loadVtablePtrFromInstance(thisI8)
	vsType := irtypes.NewStruct(irtypes.I8Ptr, irtypes.I8Ptr)
	vstruct := c.block.NewInsertValue(constant.NewZeroInitializer(vsType), vtable, 0)
	vstruct = c.block.NewInsertValue(vstruct, thisI8, 1)
	dupVS := c.dupHeapValue(vstruct, resolvedType)
	return dupVS, vsType, &goThisSnapshot{isValueType: false, resolvedType: resolvedType}
}

// storeGoResultAgg null-checks the current goroutine's G.result_ptr and stores
// `agg` through it — the failable {ok,value,err} aggregate for a `go! {}` block,
// or the raw success value for a plain `go {}` one. The single store lowering
// shared by every producer of a goroutine result: the trailing-expression value,
// the §17.2 explicit-return value (T1385), the escaping-error path, and the
// bare-return zero (T1392). T1384. Must be called on a live (non-terminated) block.
func (c *Compiler) storeGoResultAgg(agg value.Value) {
	// The buffer is `pal_alloc(typeSize(bufTy))`, and the store below types its
	// pointer from `agg` — so a value whose LLVM type is WIDER than the buffer
	// would write past the allocation instead of failing IR verification. (The
	// pre-T1385 open-coded store typed the pointer from the buffer instead, so a
	// mismatch surfaced as an `opt` verifier error; keep an equivalent tripwire.)
	// Every result-producing exit agrees with the buffer by construction, so a
	// mismatch here is a codegen bug, not a user error.
	if bufTy := c.goResultBufferType(); bufTy != nil && !agg.Type().Equal(bufTy) {
		panic(fmt.Sprintf("codegen: goroutine result store type %s does not match the result buffer type %s",
			agg.Type(), bufTy))
	}
	gTy := goroutineStructType()
	currentG := c.block.NewLoad(irtypes.I8Ptr, c.currentGGlobal)
	gPtr := c.block.NewBitCast(currentG, irtypes.NewPointer(gTy))
	rpField := c.block.NewGetElementPtr(gTy, gPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(gFieldResultPtr)))
	rpVal := c.block.NewLoad(irtypes.I8Ptr, rpField)
	rpNotNull := c.block.NewICmp(enum.IPredNE, rpVal, constant.NewNull(irtypes.I8Ptr))
	// Unique block names: a coroutine can invoke this helper multiple times (one
	// per escaping-error site, plus the success store, plus one per `return
	// <expr>` exit) — duplicate labels in one function produce broken IR.
	storeResultBlk := c.newBlock("go.store_result")
	afterStoreBlk := c.newBlock("go.after_store")
	c.block.NewCondBr(rpNotNull, storeResultBlk, afterStoreBlk)

	typedRP := storeResultBlk.NewBitCast(rpVal, irtypes.NewPointer(agg.Type()))
	storeResultBlk.NewStore(agg, typedRP)
	storeResultBlk.NewBr(afterStoreBlk)

	c.block = afterStoreBlk
}

// goResultBufferType returns the LLVM type of the result buffer the enclosing
// `go {}` / `go! {}` body's caller allocated — the failable {ok,value,err}
// aggregate for a `go! {}` body, the raw success type for a value-producing
// plain `go {}` one. nil when the current body has no buffer (plain void,
// fire-and-forget, generator, or not a go block at all).
func (c *Compiler) goResultBufferType() irtypes.Type {
	if c.inFailableGoBlock && c.failableGoBlockAggType != nil {
		return c.failableGoBlockAggType
	}
	return c.goBlockValueResultLLVM // nil unless a value-producing plain `go {}`
}

// storeGoResultDefault stores a DEFINED default into the goroutine's result
// buffer on an exit that carries no value of its own — the {ok, zero, null}
// aggregate for a `go! {}` body, the raw success type's zero for a
// value-producing plain `go {}` one. Leaving the caller-allocated buffer
// uninitialized is what handed `<-t` poison before T1392, so every valueless
// exit of a body that HAS a buffer must come through here. A no-op when there
// is no buffer to define (plain void body, fire-and-forget, generator) and on
// an already-terminated block. Shared by the bare-`return` exit (genReturnStmt)
// and genGoBlock's fall-through.
func (c *Compiler) storeGoResultDefault() {
	if c.block == nil || c.block.Term != nil {
		return
	}
	if c.inFailableGoBlock {
		var okVal value.Value
		if !isVoidResult(c.failableGoBlockAggType) {
			okVal = c.zeroValue(c.failableGoBlockAggType.Fields[1])
		}
		c.storeGoResultAgg(c.wrapOk(okVal, c.failableGoBlockAggType))
		return
	}
	if c.goBlockValueResultLLVM != nil {
		c.storeGoResultAgg(c.zeroValue(c.goBlockValueResultLLVM))
	}
}

// emitFailableGoBlockError wraps errVal into the failable-task aggregate, stores
// it into the goroutine's G.result_ptr, and branches to the coroutine's final
// suspend. The `<-t` receive loads the aggregate raw and surfaces the error like
// any failable call. The caller has already run stmt-temp + scope cleanup on the
// error path (matching emitGeneratorError's contract) — this helper must NOT
// duplicate it. T1384.
func (c *Compiler) emitFailableGoBlockError(errVal value.Value) {
	agg := c.wrapError(errVal, c.failableGoBlockAggType)
	c.storeGoResultAgg(agg)
	c.block.NewBr(c.failableGoBlockFinalSuspend)
}

// genGoBlock handles `go { block }` — wraps the block in a void function and spawns it.
// Captures outer local variables referenced in the block and passes them through the arg pack.
func (c *Compiler) genGoBlock(e *ast.GoExpr) value.Value {
	block := e.Block

	// T0683: Resolve the task's result type. sema types `go { …; <expr> }`
	// as task[T] where T is the trailing ExprStmt's type; a void block is
	// task[void]. Substitute typeSubst so the buffer/store type matches the
	// `<-task` receive side under monomorphization (symmetric with
	// genReceiveTask). A value-returning block must store its trailing value
	// into G.result_ptr; the void path is unchanged.
	goResultType := c.info.Types[e]
	if c.typeSubst != nil && goResultType != nil {
		goResultType = types.Substitute(goResultType, c.typeSubst)
	}
	// T1384: `go! { … }` produces a failable_task[T] whose result buffer holds
	// the failable aggregate {i1 ok, T value, i8* err} — the same shape the call
	// form stores, surfaced by `<-t`. goIsVoid now means "no success value T"
	// (for both task kinds); goResultLLVM is the aggregate type for a failable
	// task (always non-void) and the plain success type for a non-failable task.
	goElem, goHasElem, goFailable := types.AsAnyTaskFailable(goResultType)
	goIsVoid := !goHasElem || goElem == nil || goElem == types.TypVoid
	var successLLVM irtypes.Type = irtypes.Void
	if !goIsVoid {
		successLLVM = c.resolveType(goElem)
	}
	var goResultLLVM irtypes.Type = irtypes.Void
	if goFailable {
		goResultLLVM = computeResultType(successLLVM) // {i1,T,i8*}; always non-void
	} else if !goIsVoid {
		goResultLLVM = successLLVM
	}

	// Collect outer variables referenced in the block
	captureNames, captureIdents := c.collectBlockIdents(block, c.locals)

	// Load captured values and collect their types BEFORE switching context
	var captureVals []value.Value
	var captureLLVMTypes []irtypes.Type
	var thisSnapshot *goThisSnapshot // T1219: coro-side setup for a captured `this`
	for _, name := range captureNames {
		if name == "this" {
			// T1219: capture a private snapshot of the receiver — a goroutine can
			// outlive the receiver's owner, so it must never alias `this`.
			snapVal, snapType, snap := c.snapshotThisForGoBlock()
			captureVals = append(captureVals, snapVal)
			captureLLVMTypes = append(captureLLVMTypes, snapType)
			thisSnapshot = snap
			continue
		}
		alloca := c.locals[name]
		elemType := alloca.ElemType
		val := c.block.NewLoad(elemType, alloca)
		captureVals = append(captureVals, val)
		captureLLVMTypes = append(captureLLVMTypes, elemType)
	}

	// B0163: Increment refcount for captured channel variables and collect their types.
	// The goroutine shares the channel pointer with the outer scope,
	// so both need to call Channel.drop — refcounting prevents double-free.
	chanTypeGB := channelStructType()
	capturedChanTypes := make(map[string]types.Type) // name → sema type for channels
	for i, name := range captureNames {
		if binding, ok := c.dropBindings[name]; ok {
			if _, isCh := types.AsChannel(binding.valType); isCh || binding.named == types.TypChannel {
				chPtr := c.block.NewBitCast(captureVals[i], irtypes.NewPointer(chanTypeGB))
				rcField := c.block.NewGetElementPtr(chanTypeGB, chPtr,
					constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(chanFieldRefcount)))
				c.emitAtomicAdd(c.block, rcField, constant.NewInt(irtypes.I64, 1), irtypes.I64)
				capturedChanTypes[name] = binding.valType
			}
		}
	}

	// B0354: Collect droppable non-channel captures for ownership transfer.
	// Strings, vectors, heap user types with drop, etc. — the goroutine takes
	// ownership; the outer scope's drop flag is cleared after spawn.
	capturedDroppables := make(map[string]types.Type)
	// T1640: names whose OUTER binding was a closure env-free — see the twin
	// comment in genGoCallExprViaBlock.
	capturedEnvOwners := make(map[string]bool)
	for _, name := range captureNames {
		if _, isChannel := capturedChanTypes[name]; isChannel {
			continue
		}
		if binding, ok := c.dropBindings[name]; ok {
			capturedDroppables[name] = binding.valType
			if binding.kind == bindingFreeEnv {
				capturedEnvOwners[name] = true
			}
		}
	}

	// T0731 (generalizes T0688), T1196 (extends to void/fire-and-forget):
	// reading a BORROWED heap captured parameter inside an asynchronously-
	// scheduled coroutine is NEVER safe — the caller may free the underlying
	// buffer (e.g. a `"a"+"b"` stmt-temp) before the coroutine ever runs. T0688
	// only dup'd when the value-block's trailing expression was a bare ident
	// naming the param; but the same UAF/double-free triggers when the param is
	// aliased through a goroutine-local first (`s := if b { v } else { … }; s`),
	// derived via an expression (`v + "!"`), or read incidentally. Rather than
	// trace the block dataflow, we conservatively dup EVERY borrowed heap
	// captured param: the goroutine then owns a private deep copy regardless of
	// how the body routes the param. This applies to ALL go-block forms — the
	// awaited value-block path (T0731) AND the void / fire-and-forget path
	// (T1196), where a body that hands a param-derived value off asynchronously
	// (e.g. `go { out.send(v + "!"); }`) reads freed memory just the same. Each
	// dup is added to capturedDroppables so the existing B0354 ownership-
	// transfer machinery registers a goroutine-side drop binding at depth 0
	// (alongside the captured-channel drops). On BOTH the value and void /
	// fire-and-forget paths, a non-escaping copy is freed at coroutine scope
	// exit by the depth-0 emitScopeCleanup below (the same site that frees
	// captured channels); an escaping copy (e.g. the value-block trailing
	// value moved into G.result_ptr) has its drop flag cleared at the move
	// site, so that cleanup skips it — no leak, no double-free either way.
	// The outer clearDropFlag below is a harmless no-op (borrowed
	// params have no outer drop flag). Cost: one deep copy per borrowed heap
	// capture even when unused — negligible versus the soundness guarantee.
	// Excluded: channel captures (refcounted share), owned locals (already in
	// capturedDroppables — B0354 handles them), and Copy/value/Arc/Task types
	// (goElemNeedsBorrowedCaptureDup returns false).
	for idx, name := range captureNames {
		if c.borrowedValueParams == nil || !c.borrowedValueParams[name] {
			continue // not a borrowed value param (owned local / non-param)
		}
		if _, hasChan := capturedChanTypes[name]; hasChan {
			continue // channel: shared via refcount, no dup
		}
		if _, hasDroppable := capturedDroppables[name]; hasDroppable {
			continue // owned local: B0354 already transfers ownership
		}
		capType := c.info.Types[captureIdents[name]]
		if c.typeSubst != nil && capType != nil {
			capType = types.Substitute(capType, c.typeSubst) // mirror goResultType
		}
		if !goElemNeedsBorrowedCaptureDup(capType) {
			continue // Copy/channel/Arc/Task/value type — no heap aliasing
		}
		captureVals[idx] = c.dupBorrowedCaptureForResult(captureVals[idx], capType)
		capturedDroppables[name] = capType // goroutine now owns it → B0354 drop
	}

	// Create coroutine function with captured values as parameters
	coroName := coroRampName("goroutine", c.coroEnclosingQualifier(c.fn), c.goCounter) // T1222: qualify by enclosing to keep symbol unique across split units
	c.goCounter++
	var coroParams []*ir.Param
	for i, name := range captureNames {
		coroParams = append(coroParams, ir.NewParam(name+".cap", captureLLVMTypes[i]))
	}
	coroFn := c.module.NewFunc(coroName, irtypes.I8Ptr, coroParams...)
	coroFn.FuncAttrs = append(coroFn.FuncAttrs, rawFuncAttr("presplitcoroutine"))
	c.attributeCoroToEnclosing(coroName, c.fn) // T1222: same split unit as spawner

	// Save and switch context
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
	savedInCoroutine := c.inCoroutine
	savedCoroCleanup := c.coroCleanupBlk
	savedCoroSuspend := c.coroSuspendBlk
	savedPanicExitBlock := c.panicExitBlock
	savedCoroutineReturnBlock := c.coroutineReturnBlock
	savedGoExprFF := c.goExprFireAndForget
	savedLocalNameCount := c.localNameCount                           // T0261
	savedEnumCtorTemps := c.enumCtorTemps                             // B0267
	savedStmtTemps := c.stmtTemps                                     // T0683/T0594: isolate coro-body temps from the outer fn
	savedStmtTempMap := c.stmtTempMap                                 // T0683/T0594
	savedHeapTemps := c.heapTemps                                     // T0686: isolate coro-body heap temps from the outer fn
	savedHeapTempMap := c.heapTempMap                                 // T0686
	savedEnvTemps := c.envTemps                                       // T0739: isolate coro-body closure env temps from the outer fn
	savedEnvTempMap := c.envTempMap                                   // T0739
	savedBorrowedValueParams := c.borrowedValueParams                 // T0945
	savedDiscardedExpr := c.discardedExpr                             // T1029: coro body is not the discarded statement
	savedDiscardAliasArgPtrs := c.discardAliasArgPtrs                 // T1029
	savedInFailableGoBlock := c.inFailableGoBlock                     // T1384
	savedFailableGoBlockAggType := c.failableGoBlockAggType           // T1384
	savedFailableGoBlockFinalSuspend := c.failableGoBlockFinalSuspend // T1384
	savedGoBlockValueResultLLVM := c.goBlockValueResultLLVM           // T1392
	c.goExprFireAndForget = false                                     // reset for inner statements (B0109)
	c.discardedExpr = nil                                             // T1029: inner ExprStmts set their own
	c.discardAliasArgPtrs = nil                                       // T1029
	// T1329: save the block-value temp floors; reset to 0 only on the value path
	// (below), where the coro body's temp arrays are reset fresh. Restored at the
	// end. Kept off the aligned save block above so its length doesn't widen the
	// gofmt comment columns.
	savedBlockTempFloors := [4]int{c.blockTempFloorStmt, c.blockTempFloorHeap, c.blockTempFloorEnv, c.blockTempFloorEnum}

	// T0683: Only a non-void, awaited (`<-task`) block needs its trailing
	// value stored into G.result_ptr. A void block has no value; a
	// fire-and-forget value block (`go { 42 };` as a bare statement) has its
	// value discarded — both take the unchanged void genBlock path, whose
	// per-statement temp cleanup frees a heap trailing value (no leak), and
	// the caller leaves result_ptr null/sentinel as before. This keeps the
	// void path byte-for-byte identical to pre-T0683.
	useGoBlockValuePath := !goIsVoid && !savedGoExprFF

	c.fn = coroFn
	c.locals = make(map[string]*ir.InstAlloca)
	c.localNameCount = make(map[string]int)
	c.blockCounter = 0
	c.canError = false
	// T1385: §17.2 explicit-return style — a `return <expr>` inside the body
	// yields the GOROUTINE's result, so the shared return machinery's retType
	// (Optional wrap, view coercion, field/index dup decisions, `none`
	// resolution) must be the block's success type T, not void. A block with no
	// result stays void. currentRetType is read only by genReturnStmt.
	if goIsVoid {
		c.currentRetType = types.TypVoid
	} else {
		c.currentRetType = goElem
	}
	c.scopeBindings = nil
	c.dropFlags = make(map[string]*ir.InstAlloca)
	c.castSubjectMatch = nil // T0849: fresh per function body; restored below
	c.dropBindings = make(map[string]scopeBinding)
	c.loopScopeDepth = 0
	c.inCoroutine = true
	c.inFailableGoBlock = false    // T1384: (re)activated below only for the `go! {}` body
	c.goBlockValueResultLLVM = nil // T1392: (re)activated below only for a value-producing non-failable body
	c.enumCtorTemps = nil          // B0267
	c.borrowedValueParams = nil    // T0945: coroutine body has no user value params
	if useGoBlockValuePath {
		// T1329: fresh function → statement boundaries drain from 0, not the outer
		// block-value floor. Only on this path (temp arrays are reset fresh here);
		// the void path shares the outer temp state and stays unchanged. Restored
		// unconditionally below (a no-op when this branch didn't run).
		c.blockTempFloorStmt, c.blockTempFloorHeap, c.blockTempFloorEnv, c.blockTempFloorEnum = 0, 0, 0, 0
		// T0683/T0594: fresh temp state for the coroutine body so its temps
		// (which reference coroFn allocas) cannot leak into the outer fn.
		c.stmtTemps = nil
		c.stmtTempMap = make(map[value.Value]int)
		// T0686: same isolation for heap-instance temps — genBlockValue does
		// not save/restore heapTemps the way genBlock does (T0088), so a
		// heap-struct trailing value (e.g. `go { Box(...) }`) would otherwise
		// leak its coroFn alloca/dropFlag into the outer fn and serialize as
		// `%0` (the coro.id token) in the outer cleanupHeapTemps.
		c.heapTemps = nil
		c.heapTempMap = make(map[value.Value]int)
		// T0739: same isolation for closure env temps — a capturing-closure
		// trailing value (e.g. `go { || -> base + 2 }`) would otherwise leak
		// its coroFn env alloca/dropFlag into the outer fn and serialize as
		// `%0` (the coro.id token) in the outer cleanupEnvTemps.
		c.envTemps = nil
		c.envTempMap = make(map[value.Value]int)
	}

	// --- Coroutine preamble ---
	entry := coroFn.NewBlock(".entry")
	c.block = entry

	coroId := entry.NewCall(c.coroId,
		constant.NewInt(irtypes.I32, 0),
		constant.NewNull(irtypes.I8Ptr),
		constant.NewNull(irtypes.I8Ptr),
		constant.NewNull(irtypes.I8Ptr))

	need := entry.NewCall(c.coroAlloc, coroId)
	allocBlk := coroFn.NewBlock("coro.alloc")
	startBlk := coroFn.NewBlock("coro.start")
	entry.NewCondBr(need, allocBlk, startBlk)

	coroSizeVal := allocBlk.NewCall(c.coroSize)
	var coroSizeArg value.Value = coroSizeVal
	if c.isWasm {
		coroSizeArg = allocBlk.NewZExt(coroSizeVal, irtypes.I64)
	}
	mem := allocBlk.NewCall(c.palAlloc, coroSizeArg)
	allocBlk.NewBr(startBlk)

	phiMem := startBlk.NewPhi(
		ir.NewIncoming(constant.NewNull(irtypes.I8Ptr), entry),
		ir.NewIncoming(mem, allocBlk))
	hdl := startBlk.NewCall(c.coroBegin, coroId, phiMem)

	// Store captured params into allocas (after coro.begin → part of frame)
	var thisValAlloca *ir.InstAlloca // T1219: value-struct alloca backing a captured `this`
	for i, name := range captureNames {
		if thisSnapshot != nil && name == "this" {
			// T1219: the captured param is a value struct (heap: {vtable,instance};
			// value type: the full value struct). Store it in a frame alloca, then
			// derive the i8* `this` that genThisExpr/field access expect:
			//   heap type  → instance pointer (value struct field 1)
			//   value type → address of the value struct in the coroutine frame
			// The value-struct alloca backs the goroutine's private-copy drop.
			valAlloca := startBlk.NewAlloca(captureLLVMTypes[i])
			valAlloca.SetName(c.uniqueLocalName("this.val.addr"))
			startBlk.NewStore(coroFn.Params[i], valAlloca)

			i8Alloca := startBlk.NewAlloca(irtypes.I8Ptr)
			i8Alloca.SetName(c.uniqueLocalName("this.addr"))
			var thisI8 value.Value
			if thisSnapshot.isValueType {
				thisI8 = startBlk.NewBitCast(valAlloca, irtypes.I8Ptr)
			} else {
				thisI8 = startBlk.NewExtractValue(coroFn.Params[i], 1)
			}
			startBlk.NewStore(thisI8, i8Alloca)
			c.locals["this"] = i8Alloca
			thisValAlloca = valAlloca
			continue
		}
		alloca := startBlk.NewAlloca(captureLLVMTypes[i])
		alloca.SetName(c.uniqueLocalName(name + ".addr"))
		startBlk.NewStore(coroFn.Params[i], alloca)
		c.locals[name] = alloca
	}

	// B0163: Register drop bindings for captured channel variables inside the goroutine.
	// This ensures Channel.drop is called when the goroutine finishes, decrementing the refcount.
	// Set both entryBlock and block to startBlk so allocas and stores land in the right place.
	c.entryBlock = startBlk
	c.block = startBlk

	// T1219: register the drop for the goroutine's private heap snapshot of `this`
	// so it is freed at coroutine scope exit (value-type snapshots are Copy — no
	// drop). Mirrors the borrowed-capture dup ownership transfer (T1196/B0354).
	if thisSnapshot != nil && !thisSnapshot.isValueType && thisValAlloca != nil {
		c.maybeRegisterDrop("this", thisValAlloca, thisSnapshot.resolvedType)
	}

	for _, name := range captureNames {
		if chanValType, ok := capturedChanTypes[name]; ok {
			alloca := c.locals[name]
			c.maybeRegisterDrop(name, alloca, chanValType)
		}
	}

	// B0354: Register drop bindings for non-channel droppable captures.
	// Goroutine assumes ownership; outer drop flag is cleared after spawn.
	for _, name := range captureNames {
		if valType, ok := capturedDroppables[name]; ok {
			alloca := c.locals[name]
			c.registerGoCaptureOwnership(name, alloca, valType, capturedEnvOwners[name])
		}
	}

	// Initial suspend — in a separate block so that createEntryAlloca can
	// append allocas to startBlk BEFORE the suspend point. coro-split needs
	// allocas to precede coro.suspend to properly spill them to the frame.
	initSuspBlk := coroFn.NewBlock("coro.init.suspend")
	startBlk.NewBr(initSuspBlk)

	initResult := initSuspBlk.NewCall(c.coroSuspend, constant.None, constant.False)

	suspendBlk := coroFn.NewBlock("coro.suspend")
	bodyBlk := coroFn.NewBlock("body")
	cleanupBlk := coroFn.NewBlock("cleanup")
	// Create doneBlk early so intermediate coro.suspend switches can reference it.
	// Instructions are added after the body is compiled.
	doneBlk := coroFn.NewBlock("coro.done")
	// B0353: Create finalSuspBlk early so return statements inside the body
	// can branch here. Instructions are added after the body is compiled.
	finalSuspBlk := coroFn.NewBlock("final.suspend")

	initSuspBlk.NewSwitch(initResult, suspendBlk,
		ir.NewCase(constant.NewInt(irtypes.I8, 0), bodyBlk),
		ir.NewCase(constant.NewInt(irtypes.I8, 1), cleanupBlk))

	// Suspend: ramp returns coroutine handle
	suspendBlk.NewRet(hdl)

	// Set cleanup and suspend blocks for mid-body coro.suspend switches.
	// Cleanup = destroy path (coro.free + free). Suspend = default case (coro.end + ret).
	// Per LLVM coroutine ABI, intermediate coro.suspend default cases must go to the
	// suspend block, NOT the cleanup block — otherwise the frame is freed on park.
	c.coroCleanupBlk = cleanupBlk
	c.coroSuspendBlk = doneBlk

	// --- Body: compile user block ---
	c.block = bodyBlk
	c.entryBlock = startBlk // allocas go in startBlk (after coro.begin) to be part of coroutine frame

	// B0228: Create panic exit block for this go block coroutine.
	goPanicExitBlk2 := coroFn.NewBlock("go.panic_exit")
	c.panicExitBlock = goPanicExitBlk2
	c.coroutineReturnBlock = finalSuspBlk // B0353

	// T1384: activate the failable-go-block error sink for the `go! {}` body. An
	// escaping error (bare failable call, `?^`, `raise`) stores a failable
	// aggregate into G.result_ptr and branches to finalSuspBlk instead of
	// `ret wrapError(...)` (invalid in this coroutine ramp). Cleared after the
	// body is generated; restored to the outer value below.
	var failAggTy *irtypes.StructType
	if goFailable {
		failAggTy = goResultLLVM.(*irtypes.StructType)
		c.inFailableGoBlock = true
		c.failableGoBlockAggType = failAggTy
		c.failableGoBlockFinalSuspend = finalSuspBlk
	} else if useGoBlockValuePath {
		// T1392: a bare `return` in a value-producing NON-failable `go {}` body must
		// store a defined zero into G.result_ptr (the failable path uses the aggregate
		// sink above). goResultLLVM here is the raw success type.
		c.goBlockValueResultLLVM = goResultLLVM
	}

	if !useGoBlockValuePath {
		// Void or fire-and-forget: discard the trailing value. genBlock's
		// per-statement genStmt cleanup frees a heap trailing value, so a
		// fire-and-forget value block (`go { "x"+"y" };`) does not leak.
		c.genBlock(block)
		// T1384: a void-failable block (`go! { work()!; }`, T==void) reaching
		// the end without an escaping error stores the {ok=false} void aggregate
		// so `<-t` sees success. (An error already stored its aggregate + branched
		// to finalSuspBlk via the sink.) Plain-void and fire-and-forget paths
		// (goFailable=false) are untouched.
		if goFailable && c.block != nil && c.block.Term == nil {
			c.storeGoResultAgg(c.wrapOk(nil, failAggTy))
		}
	} else {
		// T0683: non-void awaited block — capture the trailing-expression
		// value and store it into the caller-allocated G.result_ptr buffer.
		// genBlockValue claims `result` and drops block locals after, so the
		// value is safe to store here.
		// T1384: for a failable block, a trailing bare failable call
		// (`go! { produce(5) }`) must yield its auto-propagated success value as
		// the block result rather than discard it — signal genBlockValue.
		if goFailable {
			c.goBlockTrailingWantValue = true
			// T1427: thread the trailing result's success type so genBlockValue can
			// drop a claimed heap result on a failing use-close divert. goElem is the
			// already-substituted block result type; nil-safe on non-value paths.
			c.goBlockResultDropType = goElem
		}
		result := c.genBlockValue(block)
		if result != nil && c.block != nil && c.block.Term == nil {
			// T1384: for a failable task the buffer holds the {ok,value,err}
			// aggregate — wrap the trailing success value as {ok=false, value,
			// null}. The raw `result` is kept for the claim* calls below so heap
			// payload ownership transfer is unchanged.
			storeVal := result
			if goFailable {
				storeVal = c.wrapOk(result, failAggTy)
			}
			// B0109 null-check store pattern (mirrors genGoCallExpr): the
			// caller allocates result_ptr for an awaited task. The null
			// check is defensive — symmetric with the working call form.
			// T1385: shared with the explicit-return store in genReturnStmt.
			c.storeGoResultAgg(storeVal)
		} else {
			// T1385: no trailing value on a live fall-through. Sema requires every
			// path of an explicit-return block to produce a value, so this is
			// unreachable at RUNTIME — but it is still EMITTED, for a trailing
			// if/else whose arms all `return`: genIfStmtValue leaves a live (but
			// predecessor-less) merge block behind. Store the defined default
			// anyway rather than leave G.result_ptr undefined on any path out of
			// here — that is exactly the class of defect T1392 fixed, and `opt`
			// deletes the dead block.
			c.storeGoResultDefault()
		}
		// T0594: ownership of `result` transferred to G.result_ptr — claim
		// so the coroutine body's stmt-temp cleanup doesn't free it, then
		// drop any orphaned temps from the trailing expression.
		// claimStringTemp emits a flag store, so skip it on a dead block
		// (e.g. trailing expr panicked); cleanupStmtTemps self-guards.
		if c.block != nil && c.block.Term == nil {
			c.claimStringTemp(result)
			// T0686: a heap-struct result is moved into G.result_ptr — claim it
			// so the coroutine body's heap-temp cleanup below doesn't free it.
			c.claimHeapTemp(result)
			// T0739: a capturing-closure result is moved into G.result_ptr —
			// claim its env temp so the coroutine body's env-temp cleanup below
			// doesn't free it (claimEnvTemp extracts field 1 of the fat pointer).
			c.claimEnvTemp(result)
		}
		c.cleanupStmtTemps()
		// T0686: drop any orphaned trailing-expr heap intermediates inside the
		// coroutine (self-guards on a dead block).
		c.cleanupHeapTemps()
		// T0739: drop any orphaned trailing-expr closure env intermediates
		// inside the coroutine (self-guards on a dead block).
		c.cleanupEnvTemps()
	}

	// Clear panic exit block and coroutine return block after body generation
	c.panicExitBlock = nil
	c.coroutineReturnBlock = nil
	// T1384: deactivate the failable-go-block sink before the shared tail (channel
	// drops, final panic check, final suspend) so those stores don't route through
	// it. Restored to the outer value in the restore block below.
	c.inFailableGoBlock = false
	c.failableGoBlockAggType = nil
	c.failableGoBlockFinalSuspend = nil
	c.goBlockValueResultLLVM = nil // T1392

	// B0163: Emit cleanup for captured channel drop bindings registered before genBlock.
	// genBlock only cleans up bindings added within its scope, so we must handle
	// pre-block bindings (captured channels) here before the final suspend.
	if c.block != nil && c.block.Term == nil && len(c.scopeBindings) > 0 {
		c.emitScopeCleanup(0, false)
	}

	// T0147: Per-call panic checks in genExpr now handle panic detection.
	// Calls within the block go through genExpr → case *ast.CallExpr
	// which calls emitPanicCheck() → emitPanicReturn() → branches to panicExitBlock.

	// Final suspend: yield back to scheduler so it can see coro.done()=true
	// before destroying the coroutine frame.
	// T0148: Final panic check after body + scope cleanup.
	// Catches panics from drop functions during scope cleanup that per-call checks miss.
	if c.block != nil && c.block.Term == nil {
		finalFlag := c.block.NewLoad(irtypes.I8, c.panicFlagGlobal)
		finalIsPanic := c.block.NewICmp(enum.IPredNE, finalFlag, constant.NewInt(irtypes.I8, 0))
		c.block.NewCondBr(finalIsPanic, goPanicExitBlk2, finalSuspBlk)
	}

	// B0228: Define the go block panic exit block body (same as first go block variant).
	{
		gTy := goroutineStructType()
		peCurrentG := goPanicExitBlk2.NewLoad(irtypes.I8Ptr, c.currentGGlobal)
		peGPtr := goPanicExitBlk2.NewBitCast(peCurrentG, irtypes.NewPointer(gTy))

		pePanicType := goPanicExitBlk2.NewLoad(irtypes.I8, c.panicTypeTlsGlobal)
		pePanickedField := goPanicExitBlk2.NewGetElementPtr(gTy, peGPtr,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(gFieldPanicked)))
		goPanicExitBlk2.NewStore(pePanicType, pePanickedField)

		pePanicMsg := goPanicExitBlk2.NewLoad(irtypes.I8Ptr, c.panicMsgTlsGlobal)
		pePanicMsgField := goPanicExitBlk2.NewGetElementPtr(gTy, peGPtr,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(gFieldPanicMsg)))
		goPanicExitBlk2.NewStore(pePanicMsg, pePanicMsgField)

		goPanicExitBlk2.NewStore(constant.NewInt(irtypes.I8, 0), c.panicFlagGlobal)
		goPanicExitBlk2.NewStore(constant.NewNull(irtypes.I8Ptr), c.panicMsgTlsGlobal)
		goPanicExitBlk2.NewStore(constant.NewInt(irtypes.I8, 0), c.panicTypeTlsGlobal)

		goPanicExitBlk2.NewBr(finalSuspBlk)
	}

	// --- Cleanup: free coroutine memory (only reached via destroy path) ---
	coroMem := cleanupBlk.NewCall(c.coroFree, coroId, hdl)
	needFree := cleanupBlk.NewICmp(enum.IPredNE, coroMem, constant.NewNull(irtypes.I8Ptr))
	freeBlk := coroFn.NewBlock("coro.free")
	cleanupBlk.NewCondBr(needFree, freeBlk, doneBlk)

	freeBlk.NewCall(c.palFree, coroMem)
	freeBlk.NewBr(doneBlk)

	// Done: single coro.end (both final-suspend exit and cleanup converge here)
	doneBlk.NewCall(c.coroEnd, hdl, constant.False, constant.None)
	doneBlk.NewRet(hdl)

	// Final suspend switch: default/i8 0 → doneBlk (skip free, just coro.end+ret)
	// i8 1 (destroy) → cleanup (free frame then coro.end+ret)
	finalResult := finalSuspBlk.NewCall(c.coroSuspend, constant.None, constant.True)
	finalSuspBlk.NewSwitch(finalResult, doneBlk,
		ir.NewCase(constant.NewInt(irtypes.I8, 0), doneBlk),
		ir.NewCase(constant.NewInt(irtypes.I8, 1), cleanupBlk))

	// Restore context
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
	c.inCoroutine = savedInCoroutine
	c.inFailableGoBlock = savedInFailableGoBlock                     // T1384
	c.failableGoBlockAggType = savedFailableGoBlockAggType           // T1384
	c.failableGoBlockFinalSuspend = savedFailableGoBlockFinalSuspend // T1384
	c.goBlockValueResultLLVM = savedGoBlockValueResultLLVM           // T1392
	c.coroCleanupBlk = savedCoroCleanup
	c.coroSuspendBlk = savedCoroSuspend
	c.panicExitBlock = savedPanicExitBlock
	c.coroutineReturnBlock = savedCoroutineReturnBlock
	c.goExprFireAndForget = savedGoExprFF
	c.borrowedValueParams = savedBorrowedValueParams // T0945
	c.discardedExpr = savedDiscardedExpr             // T1029
	c.discardAliasArgPtrs = savedDiscardAliasArgPtrs // T1029
	c.localNameCount = savedLocalNameCount           // T0261
	c.enumCtorTemps = savedEnumCtorTemps             // B0267
	if useGoBlockValuePath {
		c.stmtTemps = savedStmtTemps     // T0683/T0594: restore outer fn temp state
		c.stmtTempMap = savedStmtTempMap // T0683/T0594
		c.heapTemps = savedHeapTemps     // T0686: restore outer fn heap temp state
		c.heapTempMap = savedHeapTempMap // T0686
		c.envTemps = savedEnvTemps       // T0739: restore outer fn closure env temp state
		c.envTempMap = savedEnvTempMap   // T0739
	}
	c.restoreBlockTempFloors(savedBlockTempFloors) // T1329 (no-op on the void path)

	// B0354: Clear outer drop flags for captured droppable non-channel variables.
	// Ownership has been transferred to the goroutine.
	for name := range capturedDroppables {
		c.clearDropFlag(name)
	}

	// Caller: call coroutine ramp → get handle, create G, enqueue
	handle := c.block.NewCall(coroFn, captureVals...)
	gRaw := c.block.NewCall(c.funcs["promise_g_new"], handle)

	if !c.goExprFireAndForget {
		gTy := goroutineStructType()
		gPtr := c.block.NewBitCast(gRaw, irtypes.NewPointer(gTy))
		rpField := c.block.NewGetElementPtr(gTy, gPtr,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(gFieldResultPtr)))
		if goIsVoid && !goFailable {
			// Void task: set result_ptr to sentinel (0x1) so goroutine_exit
			// knows the receiver will free G (via <-task). Without this,
			// goroutine_exit would free the G and the receiver would access
			// freed memory.
			sentinel := c.block.NewIntToPtr(constant.NewInt(c.ptrIntType(), 1), irtypes.I8Ptr)
			c.block.NewStore(sentinel, rpField)
		} else {
			// T0683: non-void task — allocate the result buffer the
			// coroutine body stores into and the <-task receiver loads +
			// frees (mirrors genGoCallExpr). result_ptr != null also tells
			// goroutine_exit not to free G (the receiver owns it).
			// T1384: a void-failable task (`go! { work(); }`, T==void) also holds
			// a real {i1,i8*} aggregate buffer (goResultLLVM), never the sentinel —
			// the receive expects and frees it, matching the call form.
			resultSize := constant.NewInt(irtypes.I64, int64(c.typeSize(goResultLLVM)))
			resultBuf := c.block.NewCall(c.palAlloc, resultSize)
			c.block.NewStore(resultBuf, rpField)
		}
	}
	// Fire-and-forget (void or non-void): result_ptr stays null (from
	// promise_g_new), so goroutine_exit frees the G struct when the
	// goroutine completes. The non-void value is discarded by the void
	// genBlock path above (no buffer, no leak).

	c.block.NewCall(c.funcs["promise_sched_enqueue"], gRaw)

	return gRaw
}

// --- Receive expression (<-task / <-channel) ---

// genReceiveExpr generates code for `<-expr` — dispatches to task or channel receive.
func (c *Compiler) genReceiveExpr(e *ast.UnaryExpr) value.Value {
	operandType := c.info.Types[e.Operand]
	if c.typeSubst != nil {
		operandType = types.Substitute(operandType, c.typeSubst)
	}

	inst, ok := operandType.(*types.Instance)
	if !ok {
		panic(fmt.Sprintf("codegen: receive operand type %T is not Instance", operandType))
	}

	origin := inst.Origin()
	if origin == types.TypChannel {
		return c.genReceiveChannel(e, inst)
	}
	// T1381: `<-tasks` where tasks : failable_task[T][] is a drain (sema records
	// the result type as Vector[T] and marks it failable). The operand is a
	// Vector instance whose element is a FailableTask.
	if origin == types.TypVector {
		return c.genDrainTasks(e, inst)
	}
	return c.genReceiveTask(e, inst)
}

// genDrainTasks generates code for `<-tasks` where tasks : failable_task[T][]
// (§17.2.1). It consumes the vector, awaits every task in index order, and
// collects the successes into a fresh Vector[T]. It returns the failable
// aggregate `{ i1 is_error, i8* vec, i8* err }` — succeeding (is_error=0, vec)
// only if every task succeeded, else failing (is_error=1, err) with the FIRST
// error by index. Remaining errors are discharged via emitFailableErrorDrop (not
// silently dropped); on the error path the partially-collected result vector is
// dropped leak-free. The aggregate shape matches computeResultType(i8*), so the
// ordinary failable-surfacing machinery (auto-propagate / `?!` / `?^` / `? e {}`
// / `return`) consumes it exactly like any failable call. (T1381)
func (c *Compiler) genDrainTasks(e *ast.UnaryExpr, inst *types.Instance) value.Value {
	// inst is Vector[FailableTask[T]] — extract the success element type T.
	elemFT := inst.TypeArgs()[0]
	if c.typeSubst != nil {
		elemFT = types.Substitute(elemFT, c.typeSubst)
	}
	succType, _ := types.AsFailableTask(elemFT)
	isVoidSucc := succType == nil || succType == types.TypVoid
	var succLLVM irtypes.Type = irtypes.Void
	var succSize int64
	if !isVoidSucc {
		succLLVM = c.resolveType(succType)
		succSize = int64(c.typeSize(succLLVM))
	}
	// Result aggregate: { i1 is_error, i8* vec, i8* err } — Vector[T] resolves to
	// i8*, so the value field is i8*.
	aggType := computeResultType(irtypes.I8Ptr)

	// Evaluate the source vector (an i8* to { i64 len, i64 cap, [i8* G ...] }).
	// genExprAutoPropagate unwraps a failable operand (e.g. `<-make_handles()`
	// where make_handles is itself failable) to the raw vector before draining;
	// for a plain ident/temp operand it is a no-op over genExpr.
	vecRaw := c.genExprAutoPropagate(e.Operand)
	// The drain consumes the whole vector: suppress the source's scope-exit /
	// statement-temp drop so its element FailableTask.drop's don't double-free the
	// G's we join+free below, and its storage isn't double-freed (we free it via
	// Vector.drop after the loop). clearDropFlag covers a named binding; the
	// claimStringTemp covers a call-result temp (both are no-ops otherwise).
	if ident, ok := e.Operand.(*ast.IdentExpr); ok {
		c.clearDropFlag(ident.Name)
	}
	c.claimStringTemp(vecRaw)

	// Source length + element (G-pointer) data base.
	srcHdrPtr := c.block.NewBitCast(vecRaw, irtypes.NewPointer(vectorHeaderType()))
	srcLen := loadVectorLen(c.block, srcHdrPtr)
	srcDataBase := c.block.NewGetElementPtr(irtypes.I8, vecRaw,
		constant.NewInt(irtypes.I64, int64(vectorHeaderSize)))
	srcDataPtr := c.block.NewBitCast(srcDataBase, irtypes.NewPointer(irtypes.I8Ptr))

	// Accumulator Vector[T]: a fresh empty heap block { len=0, cap=0 }.
	accBuf := c.block.NewCall(c.palAlloc, constant.NewInt(irtypes.I64, int64(vectorHeaderSize)))
	accHdrPtr := c.block.NewBitCast(accBuf, irtypes.NewPointer(vectorHeaderType()))
	accLenField := c.block.NewGetElementPtr(vectorHeaderType(), accHdrPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
	c.block.NewStore(constant.NewInt(irtypes.I64, 0), accLenField)
	accCapField := c.block.NewGetElementPtr(vectorHeaderType(), accHdrPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 1))
	c.block.NewStore(constant.NewInt(irtypes.I64, 0), accCapField)

	// Mutable state across the loop: the (grown) accumulator pointer, the first
	// error, and whether an error was seen.
	accAlloca := c.createEntryAlloca(irtypes.I8Ptr)
	c.block.NewStore(accBuf, accAlloca)
	errAlloca := c.createEntryAlloca(irtypes.I8Ptr)
	c.block.NewStore(constant.NewNull(irtypes.I8Ptr), errAlloca)
	hasErrAlloca := c.createEntryAlloca(irtypes.I1)
	c.block.NewStore(constant.NewInt(irtypes.I1, 0), hasErrAlloca)
	var valAlloca *ir.InstAlloca
	if !isVoidSucc {
		valAlloca = c.createEntryAlloca(succLLVM)
	}
	idxAlloca := c.createEntryAlloca(irtypes.I64)
	c.block.NewStore(constant.NewInt(irtypes.I64, 0), idxAlloca)

	loopHead := c.newBlock("drain.head")
	loopBody := c.newBlock("drain.body")
	loopDone := c.newBlock("drain.done")
	c.block.NewBr(loopHead)

	// Head: i < len ?
	c.block = loopHead
	idx := c.block.NewLoad(irtypes.I64, idxAlloca)
	cond := c.block.NewICmp(enum.IPredULT, idx, srcLen)
	c.block.NewCondBr(cond, loopBody, loopDone)

	// Body: await tasks[i], collect success or capture/discharge error.
	c.block = loopBody
	idx2 := c.block.NewLoad(irtypes.I64, idxAlloca)
	gSlot := c.block.NewGetElementPtr(irtypes.I8Ptr, srcDataPtr, idx2)
	gRaw := c.block.NewLoad(irtypes.I8Ptr, gSlot)

	agg := c.emitTaskAwaitLoadFree(gRaw, succType, true, func() {})
	aggStructType := agg.Type().(*irtypes.StructType)
	tag := c.block.NewExtractValue(agg, 0)

	elemOkBlk := c.newBlock("drain.elem.ok")
	elemErrBlk := c.newBlock("drain.elem.err")
	elemContBlk := c.newBlock("drain.elem.cont")
	c.block.NewCondBr(tag, elemErrBlk, elemOkBlk)

	// ok: push the success value onto the accumulator (capturing the grown ptr).
	c.block = elemOkBlk
	if !isVoidSucc {
		okVal := c.block.NewExtractValue(agg, 1)
		c.block.NewStore(constant.NewZeroInitializer(succLLVM), valAlloca)
		c.block.NewStore(okVal, valAlloca)
		argPtr := c.block.NewBitCast(valAlloca, irtypes.I8Ptr)
		acc := c.block.NewLoad(irtypes.I8Ptr, accAlloca)
		newAcc := c.block.NewCall(c.funcs["promise_vector_push"], acc, argPtr,
			constant.NewInt(irtypes.I64, succSize))
		c.block.NewStore(newAcc, accAlloca)
	}
	c.block.NewBr(elemContBlk)

	// err: keep the FIRST error; discharge (drop) any later error.
	c.block = elemErrBlk
	errVal := c.block.NewExtractValue(agg, resultErrIdx(aggStructType))
	hadErr := c.block.NewLoad(irtypes.I1, hasErrAlloca)
	firstErrBlk := c.newBlock("drain.err.first")
	laterErrBlk := c.newBlock("drain.err.later")
	c.block.NewCondBr(hadErr, laterErrBlk, firstErrBlk)

	c.block = firstErrBlk
	c.block.NewStore(constant.NewInt(irtypes.I1, 1), hasErrAlloca)
	c.block.NewStore(errVal, errAlloca)
	c.block.NewBr(elemContBlk)

	c.block = laterErrBlk
	c.emitFailableErrorDrop(errVal) // remaining errors discharged, not swallowed
	c.block.NewBr(elemContBlk)

	// cont: i++
	c.block = elemContBlk
	nextIdx := c.block.NewAdd(idx2, constant.NewInt(irtypes.I64, 1))
	c.block.NewStore(nextIdx, idxAlloca)
	c.block.NewBr(loopHead)

	// Done: free the drained source storage (elements already consumed), then
	// build the result aggregate.
	c.block = loopDone
	if dropFn, ok := c.funcs["Vector.drop"]; ok {
		c.block.NewCall(dropFn, vecRaw)
	}
	accVec := c.block.NewLoad(irtypes.I8Ptr, accAlloca)
	hasErr := c.block.NewLoad(irtypes.I1, hasErrAlloca)
	firstErr := c.block.NewLoad(irtypes.I8Ptr, errAlloca)

	buildOkBlk := c.newBlock("drain.build.ok")
	buildErrBlk := c.newBlock("drain.build.err")
	buildDoneBlk := c.newBlock("drain.build.done")
	c.block.NewCondBr(hasErr, buildErrBlk, buildOkBlk)

	// Success: return { 0, accVec, null }. The surfacing machinery tracks the
	// extracted vector as a droppable temp (keyed on the drain expr's Vector[T]
	// success type), so it is not tracked here.
	c.block = buildOkBlk
	okAgg := c.wrapSuccessResult(accVec, aggType)
	okEnd := c.block
	c.block.NewBr(buildDoneBlk)

	// Error: drop the partially-collected result vector (leak-free), return
	// { 1, _, firstErr }.
	c.block = buildErrBlk
	if !isVoidSucc {
		c.emitVectorElementDropLoop(accVec, succType)
	}
	if dropFn, ok := c.funcs["Vector.drop"]; ok {
		c.block.NewCall(dropFn, accVec)
	}
	errAgg := c.wrapError(firstErr, aggType)
	errEnd := c.block
	c.block.NewBr(buildDoneBlk)

	c.block = buildDoneBlk
	return c.block.NewPhi(
		ir.NewIncoming(okAgg, okEnd),
		ir.NewIncoming(errAgg, errEnd),
	)
}

// unwrapTaskOptionalSource peels a `<-` operand of the shape `(src!)` or
// `(src? _ { ... })` down to the underlying force-unwrap source expression,
// returning nil for any other shape. The source may be a bare `*ast.IdentExpr`
// (owned-local optional Task, T0956) or a `*ast.MemberExpr` field access
// (optional Task field, T0806). genReceiveTask routes the result through
// neutralizeOptionalCastSource to clear the source optional's present flag after
// the receive consumes (frees) the task handle extracted from the optional.
func unwrapTaskOptionalSource(expr ast.Expr) ast.Expr {
	for {
		p, ok := expr.(*ast.ParenExpr)
		if !ok {
			break
		}
		expr = p.Expr
	}
	var inner ast.Expr
	switch e := expr.(type) {
	case *ast.OptionalUnwrapExpr:
		inner = e.Expr
	case *ast.ErrorHandlerExpr:
		inner = e.Expr
	default:
		return nil
	}
	for {
		p, ok := inner.(*ast.ParenExpr)
		if !ok {
			break
		}
		inner = p.Expr
	}
	switch inner.(type) {
	case *ast.IdentExpr, *ast.MemberExpr:
		return inner
	}
	return nil
}

// genReceiveTask generates code for `<-task` — waits for goroutine G to complete, returns T.
// The task handle is now a G pointer (i8*). Checks G.done and loads from G.result_ptr.
func (c *Compiler) genReceiveTask(e *ast.UnaryExpr, inst *types.Instance) value.Value {
	// T0954: `<-(a ?: b)` — when the await operand (peeling parens) is an inline
	// elvis, signal genElvis so it neutralizes the none-path default's owner on
	// the none block (path-conditionally). `<-` binds tighter than `?:`, so an
	// awaited elvis is always written `<-(a ?: b)` — the paren peel is required.
	prevElvisConsumed := c.elvisResultConsumed
	if be, ok := unwrapDestructureParens(e.Operand).(*ast.BinaryExpr); ok && be.Op == ast.BinElvis {
		c.elvisResultConsumed = true
	}
	gRaw := c.genExpr(e.Operand)
	c.elvisResultConsumed = prevElvisConsumed
	// T0503: `<-t` consumes the task — clear the scope-exit drop flag so the
	// receive's own pal_free(G) isn't followed by a double-free at scope exit.
	// Same for tracked getter temps (e.g. `<-obj.task_getter`).
	if ident, ok := e.Operand.(*ast.IdentExpr); ok {
		c.clearDropFlag(ident.Name)
	}
	// T0560: `<-h.field` consumes the task field. After T0560 wired field-drop
	// for Task[T], the field's scope-exit drop would double-free the G we just
	// freed here. Null the field so the field-drop's null check no-ops.
	// Only applies when target.Field is an actual field (not a getter).
	if member, ok := e.Operand.(*ast.MemberExpr); ok {
		targetType := c.info.Types[member.Target]
		if c.typeSubst != nil {
			targetType = types.Substitute(targetType, c.typeSubst)
		}
		if c.selfSubst != nil {
			targetType = types.SubstituteSelf(targetType, c.selfSubst.iface, c.selfSubst.concrete)
		}
		if named := extractNamed(targetType); named != nil && named.LookupField(member.Field) != nil {
			fieldPtr := c.genFieldPtr(member)
			c.block.NewStore(constant.NewNull(irtypes.I8Ptr), fieldPtr)
		}
	}
	// T0638: `<-coll[i]` consumes the indexed task. The receive frees the G
	// (pal_free below); without nulling the slot, the array/Vector scope-exit
	// element drop reloads the dangling pointer and Task[T].drop's only a
	// null-check → use-after-free / double-free → segfault. Null the slot so
	// the element drop no-ops. Mirrors the T0560 `<-h.field` field-null path.
	// Per-slot (not whole-collection): `Task[int][2]` with only ts[0] received
	// must still drop ts[1]. genReceiveChannel is intentionally untouched —
	// channel receive does not free the channel, so its slot must stay valid.
	if idxExpr, ok := e.Operand.(*ast.IndexExpr); ok {
		if slotPtr, ok := c.genReceiveTaskSlotPtr(idxExpr); ok {
			c.block.NewStore(constant.NewNull(irtypes.I8Ptr), slotPtr)
		}
	}
	// T0617: `<-handle` where `handle` is a for-in loop binding over a
	// Vector[Task]/Task[N] element loop. genForInVector/genForInArray record
	// the current iteration's slot address; null it here so the container's
	// scope-exit element drop reloads null and Task[T].drop no-ops (it null-
	// checks). Symmetric to the T0638 IndexExpr slot-null above; per-slot, so
	// un-awaited slots are still dropped once (T0503). genReceiveChannel never
	// consults this map — channel receive doesn't free the channel.
	if ident, ok := e.Operand.(*ast.IdentExpr); ok {
		if slotPtrAlloca, ok := c.forInHandleSlotPtr[ident.Name]; ok {
			slotPtr := c.block.NewLoad(irtypes.NewPointer(irtypes.I8Ptr), slotPtrAlloca)
			c.block.NewStore(constant.NewNull(irtypes.I8Ptr), slotPtr)
		}
	}
	// T0806/T0956: `<-(o!)` / `<-(o? _ { ... })` consumes the task extracted from
	// a force-unwrapped optional. The receive frees the G below; without clearing
	// the source optional's present flag, the source's scope-exit optional drop
	// reloads and joins+frees the same G → double-free → segfault.
	// neutralizeOptionalCastSource handles both shapes: the owned-local bare-ident
	// `o!` (T0956 — clears the local's optional present flag, field 0) and the
	// optional member field `h.tsk!` (T0806 — delegates to
	// neutralizeMemberOptionalField, which carries the Mutex/Task carve-out). Only
	// Task optionals reach genReceiveTask, so the unconditional ident-flag clear is
	// always correct here. Borrowed-source variants are already rejected at
	// ownership (T0953), so this codegen path only sees owned sources.
	if inner := unwrapTaskOptionalSource(e.Operand); inner != nil {
		c.neutralizeOptionalCastSource(inner)
	}
	c.claimStringTemp(gRaw)

	// T1379: `<-t` on a failable_task[T] loads the failable aggregate
	// {i1 ok, T value, i8* err} that the goroutine stored, and returns it raw so
	// the surrounding surfacing machinery (auto-propagate / `?!` / `?^` / `? e {}`)
	// consumes it exactly like a failable call. The buffer therefore always holds
	// the aggregate (even for T = void).
	failable := inst.Origin() == types.TypFailableTask

	var innerType types.Type
	if len(inst.TypeArgs()) > 0 {
		innerType = inst.TypeArgs()[0]
	}

	// T0547: If the operand is a captured task in the current lambda, the
	// env field still holds the G pointer because emitLambdaWritebacks ran
	// at return-statement entry (stmt.go:6351) — before this receive — and
	// wrote local→env. After pal_free(G) the env field would dangle, so
	// env_drop's Task[T].drop would spin-wait on freed memory (segfault or
	// infinite spin). Null the local alloca and env field after each pal_free
	// so env_drop sees null and no-ops. Both Task[T].drop (compiler.go:2793)
	// and envDropCallFn (expr.go:8855) already null-check. This is
	// operand-shape-specific, so it is passed as the afterFree callback to the
	// shared await helper (a no-op for the `<-tasks` drain over a Vector).
	nullCapturedEnvField := func() {
		ident, ok := e.Operand.(*ast.IdentExpr)
		if !ok {
			return
		}
		alloca, found := c.locals[ident.Name]
		if !found {
			return
		}
		for _, wb := range c.lambdaWritebacks {
			if wb.localAlloca == alloca {
				c.block.NewStore(constant.NewNull(irtypes.I8Ptr), alloca)
				c.block.NewStore(constant.NewNull(irtypes.I8Ptr), wb.envFieldPtr)
				return
			}
		}
	}

	return c.emitTaskAwaitLoadFree(gRaw, innerType, failable, nullCapturedEnvField)
}

// emitTaskAwaitLoadFree emits the per-task await + result handling shared by
// `<-t` (genReceiveTask) and `<-tasks` drain (genDrainTasks): wait for G to
// finish (cooperative park-suspend in a coroutine, or a thread-blocking / WASM
// coop-step spin otherwise), re-panic if the goroutine panicked, load the result
// from G.result_ptr, and free the result buffer + G struct. gRaw is the raw i8*
// G handle. For a failable task it returns the raw {i1, T, i8*} aggregate; for a
// plain non-void task the T value (registered as a droppable statement temp); nil
// for void. afterFree runs after each pal_free(G) to null any operand-specific
// captured-env alias (T0547) — the drain passes a no-op since it consumes the
// whole vector. (T1379/T1381)
func (c *Compiler) emitTaskAwaitLoadFree(gRaw value.Value, innerType types.Type, failable bool, afterFree func()) value.Value {
	tVoid := (innerType == nil || innerType == types.TypVoid)
	isVoid := tVoid

	var resultLLVM irtypes.Type = irtypes.Void
	if failable {
		var inner irtypes.Type = irtypes.Void
		if !tVoid {
			inner = c.resolveType(innerType)
		}
		resultLLVM = computeResultType(inner)
		isVoid = false // the aggregate is always loaded
	} else if !isVoid {
		resultLLVM = c.resolveType(innerType)
	}

	// T0680 Part 2: set by the WASM non-coroutine spin below when coop_step
	// reports a per-test deadline (stepR==2). When non-nil the result is produced
	// by a phi merging the normal load path with a zeroinitializer from this
	// timeout block (see the merge after loadResultBlk).
	var taskTimeoutBlk *ir.Block

	gTy := goroutineStructType()
	gPtr := c.block.NewBitCast(gRaw, irtypes.NewPointer(gTy))

	// Check if G is already done
	doneField := c.block.NewGetElementPtr(gTy, gPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(gFieldDone)))
	doneVal := c.block.NewLoad(irtypes.I8, doneField)
	isDone := c.block.NewICmp(enum.IPredNE, doneVal, constant.NewInt(irtypes.I8, 0))

	alreadyDone := c.newBlock("task.done")
	waitBlk := c.newBlock("task.wait")
	readyBlk := c.newBlock("task.ready")

	c.block.NewCondBr(isDone, alreadyDone, waitBlk)

	alreadyDone.NewBr(readyBlk)

	// Wait for G to complete
	c.block = waitBlk
	if c.inCoroutine {
		// T0668: shared cooperative park-suspend emitter — re-check G.done
		// under sched.done_lock, park the current G on the target G's
		// done_waiters (woken by promise_goroutine_exit), hold the lock across
		// coro.suspend via G.park_mutex (scheduler releases it after suspend —
		// prevents the enqueue-before-suspend race). The un-awaited Task-drop
		// join (emitTaskJoinAndFree) uses the same emitter, so the
		// receive-join and the drop-join cannot diverge.
		c.emitCoroTaskParkSuspendWait(gPtr, doneField, readyBlk)
	} else {
		// Thread-blocking mode: poll G.done in a loop.
		// goroutine_exit sets G.done = 1 atomically; we just spin until we see it.
		// On host: a brief usleep(100) avoids burning CPU in a tight loop.
		// T0687: On WASM (single-threaded cooperative scheduler), pal_usleep is a
		// no-op — the pending `go {…}` G sits in P0's run queue and never runs,
		// causing a permanent deadlock. Mirror the T0668 fix to Task[T].drop:
		// pump promise_sched_coop_step() instead, and terminate genuine deadlocks
		// (no runnable G AND target G still not done) with the shared message.
		checkBlk := c.newBlock("task.check")
		spinBlk := c.newBlock("task.spin")
		doneBlk := c.newBlock("task.threaddone")

		c.block.NewBr(checkBlk)

		// check: reload done flag (atomic acquire on WASM — T0669 parity:
		// prevents LLVM from hoisting the load above promise_sched_coop_step
		// and converging the spin into an infinite loop).
		c.block = checkBlk
		doneLoad2 := c.block.NewLoad(irtypes.I8, doneField)
		if c.isWasm {
			doneLoad2.Atomic = true
			doneLoad2.Ordering = enum.AtomicOrderingAcquire
			doneLoad2.Align = 1
		}
		isDone2 := c.block.NewICmp(enum.IPredNE, doneLoad2, constant.NewInt(irtypes.I8, 0))
		c.block.NewCondBr(isDone2, doneBlk, spinBlk)

		c.block = spinBlk
		if c.isWasm {
			// T0687: pump the cooperative scheduler one step. Returns i8:
			// non-zero = ran/advanced a G (progress possible), 0 = no runnable G.
			stepFn := c.funcs["promise_sched_coop_step"]
			stepR := c.block.NewCall(stepFn)
			coopRecheckBlk := c.newBlock("task.coop_recheck")
			deadlockBlk := c.newBlock("task.deadlock")
			spinProgressBlk := c.newBlock("task.progress")
			// T0680 Part 2: stepR==2 = per-test deadline reached. Break the spin
			// and yield a dead zeroinitializer result (the test is being torn down
			// and its return is discarded; G is intentionally not freed — a leak,
			// but result==2 skips the leak check). Prevents a livelock nested under
			// this await from spinning coop_step→2 forever.
			taskTimeoutBlk = c.newBlock("task.timed_out")
			isTimeout := c.block.NewICmp(enum.IPredEQ, stepR, constant.NewInt(irtypes.I8, 2))
			c.block.NewCondBr(isTimeout, taskTimeoutBlk, spinProgressBlk)

			c.block = spinProgressBlk
			madeProgress := c.block.NewICmp(enum.IPredNE, stepR, constant.NewInt(irtypes.I8, 0))
			c.block.NewCondBr(madeProgress, checkBlk, coopRecheckBlk)

			// No runnable G — re-check G.done. If the awaited G is still not
			// done it can never complete (nothing left to run) → genuine deadlock.
			rdLoad := coopRecheckBlk.NewLoad(irtypes.I8, doneField)
			rdLoad.Atomic = true
			rdLoad.Ordering = enum.AtomicOrderingAcquire
			rdLoad.Align = 1
			rdDone := coopRecheckBlk.NewICmp(enum.IPredNE, rdLoad, constant.NewInt(irtypes.I8, 0))
			coopRecheckBlk.NewCondBr(rdDone, checkBlk, deadlockBlk)

			msg := c.getTaskDeadlockMsgGlobal()
			msgPtr := deadlockBlk.NewGetElementPtr(msg.ContentType, msg,
				constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
			deadlockBlk.NewCall(c.palWrite, constant.NewInt(irtypes.I32, 2), msgPtr,
				constant.NewInt(irtypes.I64, 45))
			deadlockBlk.NewCall(c.palExit, constant.NewInt(irtypes.I32, 2))
			deadlockBlk.NewUnreachable()
		} else {
			// host: brief sleep then recheck
			c.block.NewCall(c.palUsleep, constant.NewInt(irtypes.I32, 100))
			c.block.NewBr(checkBlk)
		}

		c.block = doneBlk
		c.block.NewBr(readyBlk)
	}

	// ready: check if goroutine panicked, then load result, free G
	c.block = readyBlk

	// Check G.panicked — if the goroutine panicked, re-panic in current goroutine
	panickedField := c.block.NewGetElementPtr(gTy, gPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(gFieldPanicked)))
	panickedVal := c.block.NewLoad(irtypes.I8, panickedField)
	isPanicked := c.block.NewICmp(enum.IPredNE, panickedVal, constant.NewInt(irtypes.I8, 0))

	rePanicBlk := c.newBlock("task.repanic")
	loadResultBlk := c.newBlock("task.load_result")
	c.block.NewCondBr(isPanicked, rePanicBlk, loadResultBlk)

	// rePanicBlk: goroutine panicked — load panic_msg, free G, re-panic
	c.block = rePanicBlk
	panicMsgField := c.block.NewGetElementPtr(gTy, gPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(gFieldPanicMsg)))
	panicMsg := c.block.NewLoad(irtypes.I8Ptr, panicMsgField)
	c.block.NewCall(c.palFree, gRaw)
	afterFree()
	c.block.NewCall(c.funcs["promise_panic"], panicMsg)
	c.emitPanicReturn()

	// loadResultBlk: normal path — load result, free G
	c.block = loadResultBlk
	var resultVal value.Value
	if !isVoid {
		rpField := c.block.NewGetElementPtr(gTy, gPtr,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(gFieldResultPtr)))
		rpVal := c.block.NewLoad(irtypes.I8Ptr, rpField)
		typedRP := c.block.NewBitCast(rpVal, irtypes.NewPointer(resultLLVM))
		resultVal = c.block.NewLoad(resultLLVM, typedRP)
		// Free result buffer
		c.block.NewCall(c.palFree, rpVal)
	}

	// Free G struct
	c.block.NewCall(c.palFree, gRaw)
	afterFree()

	// T0680 Part 2: no WASM deadline break emitted — original single-path return.
	if taskTimeoutBlk == nil {
		if isVoid {
			return nil
		}
		if failable {
			// T1379: return the {ok,value,err} aggregate raw. The surfacing
			// machinery extracts + tracks the success value exactly like a failable
			// call; tracking the aggregate as a plain-T temp here would be wrong.
			return resultVal
		}
		// T1150: register the received heap result as a droppable statement temp so
		// it is freed at statement end when consumed inline (no named binding owns
		// it). c.block is loadResultBlk here — where resultVal is live and the return
		// flows from. Claim-on-consume sites clear the flag when ownership transfers.
		c.trackReceivedTaskResult(resultVal, innerType)
		return resultVal
	}

	// WASM: merge the normal load path with the deadline-timeout path. The timeout
	// block yields a zeroinitializer (the value is dead — teardown discards it) and
	// deliberately does not free G (may still be running → tolerated teardown leak).
	loadDoneBlk := c.block
	mergeBlk := c.newBlock("task.recv_merge")
	loadDoneBlk.NewBr(mergeBlk)

	c.block = taskTimeoutBlk
	afterFree()
	c.block.NewBr(mergeBlk)

	c.block = mergeBlk
	if isVoid {
		return nil
	}
	resultPhi := mergeBlk.NewPhi(
		ir.NewIncoming(resultVal, loadDoneBlk),
		ir.NewIncoming(constant.NewZeroInitializer(resultLLVM), taskTimeoutBlk),
	)
	if failable {
		// T1379: return the failable aggregate raw (see the non-timeout path).
		return resultPhi
	}
	// T1150 (see above): register the merged result as a droppable statement temp.
	c.trackReceivedTaskResult(resultPhi, innerType)
	return resultPhi
}

// genReceiveChannel generates code for `<-channel[T]` — returns T? (optional).
// lock → wait while empty && !closed → if closed+empty: return none → read value → return Some(value)
func (c *Compiler) genReceiveChannel(e *ast.UnaryExpr, inst *types.Instance) value.Value {
	chRaw := c.genExpr(e.Operand)

	elemType := inst.TypeArgs()[0]
	elemLLVM := c.resolveType(elemType)
	elemSize := int64(c.typeSize(elemLLVM))
	optType := irtypes.NewStruct(irtypes.I1, elemLLVM) // { i1, T }

	chanType := channelStructType()
	chPtr := c.block.NewBitCast(chRaw, irtypes.NewPointer(chanType))

	// Load mutex and cond vars
	mtxFieldPtr := c.block.NewGetElementPtr(chanType, chPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(chanFieldMutex)))
	mtx := c.block.NewLoad(irtypes.I8Ptr, mtxFieldPtr)

	neFieldPtr := c.block.NewGetElementPtr(chanType, chPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(chanFieldNotEmpty)))
	notEmpty := c.block.NewLoad(irtypes.I8Ptr, neFieldPtr)

	nfFieldPtr := c.block.NewGetElementPtr(chanType, chPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(chanFieldNotFull)))
	notFull := c.block.NewLoad(irtypes.I8Ptr, nfFieldPtr)

	// Lock
	c.block.NewCall(c.palMutexLock, mtx)

	// Wait while count == 0 && !closed
	waitBlock := c.newBlock("chrecv.wait")
	checkBlock := c.newBlock("chrecv.check")
	noneBlock := c.newBlock("chrecv.none")
	readBlock := c.newBlock("chrecv.read")
	doneBlock := c.newBlock("chrecv.done")

	c.block.NewBr(waitBlock)

	// wait: check count == 0 && !closed
	c.block = waitBlock
	countPtr := c.block.NewGetElementPtr(chanType, chPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(chanFieldCount)))
	count := c.block.NewLoad(irtypes.I64, countPtr)
	isEmpty := c.block.NewICmp(enum.IPredEQ, count, constant.NewInt(irtypes.I64, 0))
	closedPtr := c.block.NewGetElementPtr(chanType, chPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(chanFieldClosed)))
	closedVal := c.block.NewLoad(irtypes.I8, closedPtr)
	isOpen := c.block.NewICmp(enum.IPredEQ, closedVal, constant.NewInt(irtypes.I8, 0))
	shouldWait := c.block.NewAnd(isEmpty, isOpen)

	waitBodyBlock := c.newBlock("chrecv.wait.body")
	c.block.NewCondBr(shouldWait, waitBodyBlock, checkBlock)

	if c.inCoroutine {
		// Goroutine mode: park on recv_waiters + coro.suspend
		c.block = waitBodyBlock
		currentG := c.block.NewLoad(irtypes.I8Ptr, c.currentGGlobal)
		recvHeadPtr := c.block.NewGetElementPtr(chanType, chPtr,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(chanFieldRecvWaitersHead)))
		recvTailPtr := c.block.NewGetElementPtr(chanType, chPtr,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(chanFieldRecvWaitersTail)))
		c.block.NewCall(c.funcs["promise_waiter_enqueue"], recvHeadPtr, recvTailPtr, currentG)
		// Store mutex in G.park_mutex — scheduler releases after coro.suspend completes
		gTyRecv := goroutineStructType()
		recvGPtr := c.block.NewBitCast(currentG, irtypes.NewPointer(gTyRecv))
		recvPmField := c.block.NewGetElementPtr(gTyRecv, recvGPtr,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(gFieldParkMutex)))
		c.block.NewStore(mtx, recvPmField)

		suspResult := c.block.NewCall(c.coroSuspend, constant.None, constant.False)
		resumeBlk := c.newBlock("chrecv.wait.resume")
		c.block.NewSwitch(suspResult, c.coroSuspendBlk,
			ir.NewCase(constant.NewInt(irtypes.I8, 0), resumeBlk),
			ir.NewCase(constant.NewInt(irtypes.I8, 1), c.coroCleanupBlk))

		// On resume: re-lock and retry
		c.block = resumeBlk
		c.block.NewCall(c.palMutexLock, mtx)
		c.block.NewBr(waitBlock)
	} else if c.isWasm {
		// T1200: pump the cooperative scheduler instead of the no-op cond_wait so a
		// non-coroutine receiver yields to its sender; on progress recheck emptiness.
		c.block = waitBodyBlock
		c.emitWasmCoopWaitPump(waitBlock)
	} else {
		// Thread-blocking mode: cond_wait (with syscall handoff, T1685), loop
		c.block = waitBodyBlock
		c.emitBlockingCondWait(notEmpty, mtx)
		c.block.NewBr(waitBlock)
	}

	// check: if count == 0 && closed → none, else → read
	c.block = checkBlock
	countAgain := c.block.NewLoad(irtypes.I64, countPtr)
	stillEmpty := c.block.NewICmp(enum.IPredEQ, countAgain, constant.NewInt(irtypes.I64, 0))
	c.block.NewCondBr(stillEmpty, noneBlock, readBlock)

	// none: return { false, zeroinit }
	c.block = noneBlock
	c.block.NewCall(c.palMutexUnlock, mtx)
	noneVal := constant.NewZeroInitializer(optType)
	c.block.NewBr(doneBlock)

	// read: memcpy from buffer[head], advance head, count--, wake sender
	c.block = readBlock
	bufPtr := c.block.NewGetElementPtr(chanType, chPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(chanFieldBuffer)))
	buf := c.block.NewLoad(irtypes.I8Ptr, bufPtr)
	headPtr := c.block.NewGetElementPtr(chanType, chPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(chanFieldHead)))
	head := c.block.NewLoad(irtypes.I64, headPtr)
	offset := c.block.NewMul(head, constant.NewInt(irtypes.I64, elemSize))
	src := c.block.NewGetElementPtr(irtypes.I8, buf, offset)

	// Read value via alloca + memcpy (entry-block alloca to avoid stack growth in loops)
	resultAlloca := c.createEntryAlloca(elemLLVM)
	resultAsI8 := c.block.NewBitCast(resultAlloca, irtypes.I8Ptr)
	c.block.NewCall(c.funcs["llvm.memcpy"], resultAsI8, src,
		constant.NewInt(irtypes.I64, elemSize), constant.False)
	resultVal := c.block.NewLoad(elemLLVM, resultAlloca)

	// head = (head + 1) % capacity
	capPtr := c.block.NewGetElementPtr(chanType, chPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(chanFieldCapacity)))
	cap_ := c.block.NewLoad(irtypes.I64, capPtr)
	headPlusOne := c.block.NewAdd(head, constant.NewInt(irtypes.I64, 1))
	newHead := c.block.NewURem(headPlusOne, cap_)
	c.block.NewStore(newHead, headPtr)

	// count--
	countRead := c.block.NewLoad(irtypes.I64, countPtr)
	newCount := c.block.NewSub(countRead, constant.NewInt(irtypes.I64, 1))
	c.block.NewStore(newCount, countPtr)

	// Wake a waiting sender (handles both regular G and select SWN nodes)
	sendHeadPtr := c.block.NewGetElementPtr(chanType, chPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(chanFieldSendWaitersHead)))
	sendTailPtr := c.block.NewGetElementPtr(chanType, chPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(chanFieldSendWaitersTail)))
	c.block.NewCall(c.funcs["promise_waiter_wake_one"], sendHeadPtr, sendTailPtr, notFull)

	// Wake a rendezvous-parked sender (T0312): count is now 0, so the sender's
	// rendezvous wait is complete. Waking rv_waiters lets it proceed without spinning.
	rvWakeHeadPtr := c.block.NewGetElementPtr(chanType, chPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(chanFieldRvWaitersHead)))
	rvWakeTailPtr := c.block.NewGetElementPtr(chanType, chPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(chanFieldRvWaitersTail)))
	c.block.NewCall(c.funcs["promise_waiter_wake_one"], rvWakeHeadPtr, rvWakeTailPtr, notFull)

	// Unlock
	c.block.NewCall(c.palMutexUnlock, mtx)

	// Build Some: { true, value }
	someVal := c.block.NewInsertValue(constant.NewZeroInitializer(optType), constant.True, 0)
	someVal2 := c.block.NewInsertValue(someVal, resultVal, 1)
	someBlk := c.block // capture current block for phi predecessor
	c.block.NewBr(doneBlock)

	// done: phi to select none or some
	c.block = doneBlock
	phi := c.block.NewPhi(
		&ir.Incoming{X: noneVal, Pred: noneBlock},
		&ir.Incoming{X: someVal2, Pred: someBlk},
	)

	return phi
}

// registerGoCaptureOwnership registers the goroutine-side owning binding for a
// capture whose ownership B0354 transfers into the coroutine frame. Shared by the
// two spawn paths (genGoCallExprViaBlock and genGoBlock) so they cannot drift.
//
// T1640 (R3): a closure capture needs maybeRegisterEnvFree, not maybeRegisterDrop
// — the latter routes through extractNamed, which is nil for a *types.Signature,
// so it returns without registering anything and the moved env would leak (the
// outer drop flag is cleared after the spawn, so nobody else frees it). The
// closure's fat pointer was copied into the coroutine frame by value, so passing a
// nil valueExpr is correct: this is a transfer, never an aggregate/match borrow
// (ownership's R5 already rejected any binding that did not own its env).
//
// outerWasEnvFree, not valType, selects the env path: an optional binding carries
// its ELEMENT type in valType, so an `((int) -> int)?` capture would otherwise be
// given an env-free binding over an Optional-struct alloca and emitEnvFree would
// extractvalue a fat pointer out of it.
func (c *Compiler) registerGoCaptureOwnership(name string, alloca *ir.InstAlloca, valType types.Type, outerWasEnvFree bool) {
	if outerWasEnvFree {
		c.maybeRegisterEnvFree(name, alloca, valType, nil)
		return
	}
	c.maybeRegisterDrop(name, alloca, valType)
}
