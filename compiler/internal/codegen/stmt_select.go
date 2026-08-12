package codegen

import (
	"fmt"

	"github.com/llir/llvm/ir"
	"github.com/llir/llvm/ir/constant"
	"github.com/llir/llvm/ir/enum"
	irtypes "github.com/llir/llvm/ir/types"
	"github.com/llir/llvm/ir/value"

	"github.com/promise-language/promise/compiler/internal/ast"
	"github.com/promise-language/promise/compiler/internal/types"
)

// genSelectStmt generates LLVM IR for a select statement.
// Implements Go-style lock-all-channels protocol:
// 1. Evaluate all channel expressions
// 2. Lock all channels sorted by address (deadlock prevention)
// 3. Check which cases can proceed (non-blocking)
// 4. If one can: execute it, unlock all
// 5. If none + default: unlock all, execute default
// 6. If none + no default: park on waiter lists, suspend, dispatch on wake
func (c *Compiler) genSelectStmt(s *ast.SelectStmt) {
	nCases := len(s.Cases)
	chanType := channelStructType()

	// Step 1: Evaluate channel expressions and gather info
	type selectCaseInfo struct {
		chRaw         value.Value
		chPtr         value.Value
		isSend        bool
		sendValueExpr ast.Expr
		binding       string
		elemLLVM      irtypes.Type
		elemSize      int64
	}

	caseInfos := make([]selectCaseInfo, nCases)
	for i, sc := range s.Cases {
		chRaw := c.genExpr(sc.Channel)
		chPtr := c.block.NewBitCast(chRaw, irtypes.NewPointer(chanType))

		semaType := c.info.Types[sc.Channel]
		inst := semaType.(*types.Instance)
		elemType := inst.TypeArgs()[0]
		elemLLVM := c.resolveType(elemType)
		elemSize := int64(c.typeSize(elemLLVM))

		caseInfos[i] = selectCaseInfo{
			chRaw:         chRaw,
			chPtr:         chPtr,
			isSend:        sc.IsSend,
			sendValueExpr: sc.SendValue,
			binding:       sc.Binding,
			elemLLVM:      elemLLVM,
			elemSize:      elemSize,
		}
	}

	// Step 2: Sort channel pointers by address and lock all.
	i8PtrTy := irtypes.I8Ptr
	arrType := irtypes.NewArray(uint64(nCases), i8PtrTy)
	chArr := c.createEntryAlloca(arrType)

	for i, ci := range caseInfos {
		ptr := c.block.NewGetElementPtr(arrType, chArr,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(i)))
		c.block.NewStore(ci.chRaw, ptr)
	}

	// Inline bubble sort by pointer address (for deadlock prevention)
	if nCases > 1 {
		for pass := 0; pass < nCases-1; pass++ {
			for j := 0; j < nCases-1-pass; j++ {
				ptrA := c.block.NewGetElementPtr(arrType, chArr,
					constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(j)))
				ptrB := c.block.NewGetElementPtr(arrType, chArr,
					constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(j+1)))
				valA := c.block.NewLoad(i8PtrTy, ptrA)
				valB := c.block.NewLoad(i8PtrTy, ptrB)
				intA := c.block.NewPtrToInt(valA, c.ptrIntType())
				intB := c.block.NewPtrToInt(valB, c.ptrIntType())
				needSwap := c.block.NewICmp(enum.IPredUGT, intA, intB)

				swapBlk := c.newBlock(fmt.Sprintf("select.sort.swap.%d.%d", pass, j))
				contBlk := c.newBlock(fmt.Sprintf("select.sort.cont.%d.%d", pass, j))
				c.block.NewCondBr(needSwap, swapBlk, contBlk)

				c.block = swapBlk
				c.block.NewStore(valB, ptrA)
				c.block.NewStore(valA, ptrB)
				c.block.NewBr(contBlk)

				c.block = contBlk
			}
		}
	}

	// Lock all channels in sorted order (skip duplicates).
	// lockStartBlk is the entry point for the retry loop when blocking select
	// yields and needs to re-lock + re-check all cases.
	lockStartBlk := c.newBlock("select.lock.start")
	c.block.NewBr(lockStartBlk)
	c.block = lockStartBlk

	for i := 0; i < nCases; i++ {
		ptr := c.block.NewGetElementPtr(arrType, chArr,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(i)))
		chRawSorted := c.block.NewLoad(i8PtrTy, ptr)
		chPtrSorted := c.block.NewBitCast(chRawSorted, irtypes.NewPointer(chanType))
		mtxPtr := c.block.NewGetElementPtr(chanType, chPtrSorted,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(chanFieldMutex)))
		mtx := c.block.NewLoad(i8PtrTy, mtxPtr)

		if i > 0 {
			// Skip if same channel as previous (avoid double-lock)
			prevPtr := c.block.NewGetElementPtr(arrType, chArr,
				constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(i-1)))
			prevRaw := c.block.NewLoad(i8PtrTy, prevPtr)
			isSame := c.block.NewICmp(enum.IPredEQ, chRawSorted, prevRaw)
			lockBlk := c.newBlock(fmt.Sprintf("select.lock.%d", i))
			skipBlk := c.newBlock(fmt.Sprintf("select.lock.skip.%d", i))
			c.block.NewCondBr(isSame, skipBlk, lockBlk)
			c.block = lockBlk
			c.block.NewCall(c.palMutexLock, mtx)
			c.block.NewBr(skipBlk)
			c.block = skipBlk
		} else {
			c.block.NewCall(c.palMutexLock, mtx)
		}
	}

	// Step 3: Try each case to see if it can proceed
	mergeBlk := c.newBlock("select.merge")
	caseExecBlks := make([]*ir.Block, nCases)
	for i := range nCases {
		caseExecBlks[i] = c.newBlock(fmt.Sprintf("select.case%d.exec", i))
	}

	// After trying all: default or park or merge
	var afterTryBlk *ir.Block
	var defaultBlk *ir.Block
	if s.Default != nil {
		defaultBlk = c.newBlock("select.default")
		afterTryBlk = defaultBlk
	} else if c.inCoroutine {
		afterTryBlk = c.newBlock("select.park")
	} else if !c.isWasm {
		// Non-coroutine context (e.g., batch tests): poll-retry fallback (B0045)
		afterTryBlk = c.newBlock("select.poll")
	} else {
		// T1220: WASM non-coroutine — pump the cooperative scheduler instead of
		// silently falling through to mergeBlk (mirrors the B0045 poll fallback,
		// but pal_usleep can't pump the single-threaded coop scheduler).
		afterTryBlk = c.newBlock("select.wasm.wait")
	}

	// Generate try-check chain
	firstTryBlk := c.newBlock("select.try0")
	c.block.NewBr(firstTryBlk)
	c.block = firstTryBlk

	for i, ci := range caseInfos {
		var nextCheck *ir.Block
		if i+1 < nCases {
			nextCheck = c.newBlock(fmt.Sprintf("select.try%d", i+1))
		} else {
			nextCheck = afterTryBlk
		}

		if ci.isSend {
			countPtr := c.block.NewGetElementPtr(chanType, ci.chPtr,
				constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(chanFieldCount)))
			count := c.block.NewLoad(irtypes.I64, countPtr)
			capPtr := c.block.NewGetElementPtr(chanType, ci.chPtr,
				constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(chanFieldCapacity)))
			cap_ := c.block.NewLoad(irtypes.I64, capPtr)
			notFull := c.block.NewICmp(enum.IPredULT, count, cap_)
			closedPtr := c.block.NewGetElementPtr(chanType, ci.chPtr,
				constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(chanFieldClosed)))
			closedVal := c.block.NewLoad(irtypes.I8, closedPtr)
			isOpen := c.block.NewICmp(enum.IPredEQ, closedVal, constant.NewInt(irtypes.I8, 0))
			canSend := c.block.NewAnd(notFull, isOpen)
			c.block.NewCondBr(canSend, caseExecBlks[i], nextCheck)
		} else {
			countPtr := c.block.NewGetElementPtr(chanType, ci.chPtr,
				constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(chanFieldCount)))
			count := c.block.NewLoad(irtypes.I64, countPtr)
			hasItems := c.block.NewICmp(enum.IPredUGT, count, constant.NewInt(irtypes.I64, 0))
			closedPtr := c.block.NewGetElementPtr(chanType, ci.chPtr,
				constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(chanFieldClosed)))
			closedVal := c.block.NewLoad(irtypes.I8, closedPtr)
			isClosed := c.block.NewICmp(enum.IPredEQ, closedVal, constant.NewInt(irtypes.I8, 1))
			canRecv := c.block.NewOr(hasItems, isClosed)
			c.block.NewCondBr(canRecv, caseExecBlks[i], nextCheck)
		}

		if i+1 < nCases {
			c.block = nextCheck
		}
	}

	// Helper: generate unlock-all code
	unlockAll := func() {
		for j := nCases - 1; j >= 0; j-- {
			ptr := c.block.NewGetElementPtr(arrType, chArr,
				constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(j)))
			chRawSorted := c.block.NewLoad(i8PtrTy, ptr)

			if j < nCases-1 {
				// Skip if same as next (since we're going in reverse)
				nextPtr := c.block.NewGetElementPtr(arrType, chArr,
					constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(j+1)))
				nextRaw := c.block.NewLoad(i8PtrTy, nextPtr)
				isSame := c.block.NewICmp(enum.IPredEQ, chRawSorted, nextRaw)
				unlockBlk := c.newBlock(fmt.Sprintf("select.unlock.%d", j))
				skipBlk := c.newBlock(fmt.Sprintf("select.unlock.skip.%d", j))
				c.block.NewCondBr(isSame, skipBlk, unlockBlk)
				c.block = unlockBlk
				chPtrSorted := c.block.NewBitCast(chRawSorted, irtypes.NewPointer(chanType))
				mtxPtr := c.block.NewGetElementPtr(chanType, chPtrSorted,
					constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(chanFieldMutex)))
				mtx := c.block.NewLoad(i8PtrTy, mtxPtr)
				c.block.NewCall(c.palMutexUnlock, mtx)
				c.block.NewBr(skipBlk)
				c.block = skipBlk
			} else {
				chPtrSorted := c.block.NewBitCast(chRawSorted, irtypes.NewPointer(chanType))
				mtxPtr := c.block.NewGetElementPtr(chanType, chPtrSorted,
					constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(chanFieldMutex)))
				mtx := c.block.NewLoad(i8PtrTy, mtxPtr)
				c.block.NewCall(c.palMutexUnlock, mtx)
			}
		}
	}

	// Helper: generate send execution code for a case
	execSend := func(ci selectCaseInfo, prefix string) {
		argVal := c.genExpr(ci.sendValueExpr)
		// The send value's bits are memcpy'd into the channel buffer below,
		// transferring ownership. Ownership marks the send value Moved at the
		// select-send site (B0341 / T0784 ownership changes), so static
		// use-after-move is rejected — but ownership does not touch the runtime
		// drop flag. Without clearing it here, the source local's scope-exit
		// drop and the channel buffer both free the same allocation →
		// double-free / SEGV. Mirror genChannelSend, which clears both the bare
		// IdentExpr and the cast-subject cases.
		//
		// T0799: bare IdentExpr send (`select { ch.send(s): ... }` with no cast).
		if ident, ok := ci.sendValueExpr.(*ast.IdentExpr); ok {
			c.clearDropFlag(ident.Name)
		}
		// T0784: cast-of-borrow send (`select { ch.send(x as!/as T): ... }`) —
		// the cast is a view over an owned local with the same double-free shape.
		// T0849: for the conditional `as` form, drop iff the downcast failed.
		if ident := c.castSubjectMovableIdent(ci.sendValueExpr); ident != nil {
			c.consumeCastSubjectDropFlag(ci.sendValueExpr, ident.Name)
		}
		// T1073: `select { ch.send(o!): ... }` — force-unwrap moves the inner out
		// of the source optional into the channel buffer (which the receiver/drop
		// frees). Neutralize the source optional's present flag so its scope-exit
		// drop doesn't double-free the moved inner.
		c.neutralizeForceUnwrapElem(ci.sendValueExpr)
		// T0799: a freshly produced send value (`select { ch.send("a" + "b"): ... }`)
		// is a tracked statement temp. Its bits are memcpy'd into the buffer below,
		// so ownership transfers to the channel — claim it (mirror genChannelSend's
		// B0170/B0233 claims) or cleanupStmtTemps would free the buffer copy at
		// select-statement end → use-after-free / double-free.
		c.claimStringTemp(argVal)
		c.claimHeapTemp(argVal)
		argAlloca := c.createEntryAlloca(ci.elemLLVM)
		c.block.NewStore(argVal, argAlloca)
		argAsI8 := c.block.NewBitCast(argAlloca, i8PtrTy)

		bufPtr := c.block.NewGetElementPtr(chanType, ci.chPtr,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(chanFieldBuffer)))
		buf := c.block.NewLoad(i8PtrTy, bufPtr)
		tailPtr := c.block.NewGetElementPtr(chanType, ci.chPtr,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(chanFieldTail)))
		tail := c.block.NewLoad(irtypes.I64, tailPtr)
		offset := c.block.NewMul(tail, constant.NewInt(irtypes.I64, ci.elemSize))
		dest := c.block.NewGetElementPtr(irtypes.I8, buf, offset)
		c.block.NewCall(c.funcs["llvm.memcpy"], dest, argAsI8,
			constant.NewInt(irtypes.I64, ci.elemSize), constant.False)

		capPtr := c.block.NewGetElementPtr(chanType, ci.chPtr,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(chanFieldCapacity)))
		cap_ := c.block.NewLoad(irtypes.I64, capPtr)
		tailPlusOne := c.block.NewAdd(tail, constant.NewInt(irtypes.I64, 1))
		newTail := c.block.NewURem(tailPlusOne, cap_)
		c.block.NewStore(newTail, tailPtr)

		countPtr := c.block.NewGetElementPtr(chanType, ci.chPtr,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(chanFieldCount)))
		countVal := c.block.NewLoad(irtypes.I64, countPtr)
		newCount := c.block.NewAdd(countVal, constant.NewInt(irtypes.I64, 1))
		c.block.NewStore(newCount, countPtr)

		// Wake a waiting receiver (handles both regular G and select SWN nodes)
		recvHeadPtr := c.block.NewGetElementPtr(chanType, ci.chPtr,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(chanFieldRecvWaitersHead)))
		recvTailPtr := c.block.NewGetElementPtr(chanType, ci.chPtr,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(chanFieldRecvWaitersTail)))
		nePtr := c.block.NewGetElementPtr(chanType, ci.chPtr,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(chanFieldNotEmpty)))
		ne := c.block.NewLoad(i8PtrTy, nePtr)
		c.block.NewCall(c.funcs["promise_waiter_wake_one"], recvHeadPtr, recvTailPtr, ne)
	}

	// Helper: generate recv execution code for a case
	execRecv := func(ci selectCaseInfo, prefix string) {
		optType := irtypes.NewStruct(irtypes.I1, ci.elemLLVM)
		countPtr := c.block.NewGetElementPtr(chanType, ci.chPtr,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(chanFieldCount)))
		count := c.block.NewLoad(irtypes.I64, countPtr)
		isEmpty := c.block.NewICmp(enum.IPredEQ, count, constant.NewInt(irtypes.I64, 0))

		noneBlk := c.newBlock(prefix + ".none")
		readBlk := c.newBlock(prefix + ".read")
		doneBlk := c.newBlock(prefix + ".done")
		c.block.NewCondBr(isEmpty, noneBlk, readBlk)

		c.block = noneBlk
		noneVal := constant.NewZeroInitializer(optType)
		c.block.NewBr(doneBlk)

		c.block = readBlk
		bufPtr := c.block.NewGetElementPtr(chanType, ci.chPtr,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(chanFieldBuffer)))
		buf := c.block.NewLoad(i8PtrTy, bufPtr)
		headPtr := c.block.NewGetElementPtr(chanType, ci.chPtr,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(chanFieldHead)))
		head := c.block.NewLoad(irtypes.I64, headPtr)
		offset := c.block.NewMul(head, constant.NewInt(irtypes.I64, ci.elemSize))
		src := c.block.NewGetElementPtr(irtypes.I8, buf, offset)
		rAlloca := c.createEntryAlloca(ci.elemLLVM)
		rAsI8 := c.block.NewBitCast(rAlloca, i8PtrTy)
		c.block.NewCall(c.funcs["llvm.memcpy"], rAsI8, src,
			constant.NewInt(irtypes.I64, ci.elemSize), constant.False)
		rVal := c.block.NewLoad(ci.elemLLVM, rAlloca)

		capPtr := c.block.NewGetElementPtr(chanType, ci.chPtr,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(chanFieldCapacity)))
		cap_ := c.block.NewLoad(irtypes.I64, capPtr)
		headPlusOne := c.block.NewAdd(head, constant.NewInt(irtypes.I64, 1))
		newHead := c.block.NewURem(headPlusOne, cap_)
		c.block.NewStore(newHead, headPtr)

		countRead := c.block.NewLoad(irtypes.I64, countPtr)
		newCount := c.block.NewSub(countRead, constant.NewInt(irtypes.I64, 1))
		c.block.NewStore(newCount, countPtr)

		// Wake a waiting sender (handles both regular G and select SWN nodes)
		sendHeadPtr := c.block.NewGetElementPtr(chanType, ci.chPtr,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(chanFieldSendWaitersHead)))
		sendTailPtr := c.block.NewGetElementPtr(chanType, ci.chPtr,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(chanFieldSendWaitersTail)))
		nfPtr := c.block.NewGetElementPtr(chanType, ci.chPtr,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(chanFieldNotFull)))
		nf := c.block.NewLoad(i8PtrTy, nfPtr)
		c.block.NewCall(c.funcs["promise_waiter_wake_one"], sendHeadPtr, sendTailPtr, nf)

		// Wake a rendezvous-parked sender (T0312)
		rvSendHeadPtr := c.block.NewGetElementPtr(chanType, ci.chPtr,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(chanFieldRvWaitersHead)))
		rvSendTailPtr := c.block.NewGetElementPtr(chanType, ci.chPtr,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(chanFieldRvWaitersTail)))
		c.block.NewCall(c.funcs["promise_waiter_wake_one"], rvSendHeadPtr, rvSendTailPtr, nf)

		someVal := c.block.NewInsertValue(constant.NewZeroInitializer(optType), constant.True, 0)
		someVal2 := c.block.NewInsertValue(someVal, rVal, 1)
		someBlk := c.block // capture for phi predecessor
		c.block.NewBr(doneBlk)

		c.block = doneBlk
		recvPhi := c.block.NewPhi(
			&ir.Incoming{X: noneVal, Pred: noneBlk},
			&ir.Incoming{X: someVal2, Pred: someBlk},
		)

		if ci.binding != "_" {
			alloca := c.createEntryAlloca(optType)
			alloca.SetName(c.uniqueLocalName(ci.binding))
			c.block.NewStore(recvPhi, alloca)
			c.locals[ci.binding] = alloca
		}
	}

	// Step 4: Generate case execution blocks (non-blocking path)
	for i, ci := range caseInfos {
		c.block = caseExecBlks[i]
		savedScopeLen := len(c.scopeBindings)

		prefix := fmt.Sprintf("select.c%d", i)
		if ci.isSend {
			execSend(ci, prefix)
		} else {
			execRecv(ci, prefix)
		}

		unlockAll()

		for _, stmt := range s.Cases[i].Body {
			if c.block.Term != nil {
				break
			}
			c.genStmt(stmt)
		}
		if c.block != nil && c.block.Term == nil && len(c.scopeBindings) > savedScopeLen {
			cap := c.emitScopeCleanup(savedScopeLen, false)
			c.emitCloseErrCheck(cap, savedScopeLen)
		}
		c.scopeBindings = c.scopeBindings[:savedScopeLen]
		if c.block != nil && c.block.Term == nil {
			c.block.NewBr(mergeBlk)
		}
	}

	// Step 5: Default block
	if defaultBlk != nil {
		c.block = defaultBlk
		savedScopeLen := len(c.scopeBindings)
		unlockAll()
		for _, stmt := range s.Default {
			if c.block.Term != nil {
				break
			}
			c.genStmt(stmt)
		}
		if c.block != nil && c.block.Term == nil && len(c.scopeBindings) > savedScopeLen {
			cap := c.emitScopeCleanup(savedScopeLen, false)
			c.emitCloseErrCheck(cap, savedScopeLen)
		}
		c.scopeBindings = c.scopeBindings[:savedScopeLen]
		if c.block != nil && c.block.Term == nil {
			c.block.NewBr(mergeBlk)
		}
	}

	// Step 6: Blocking select (no default, coroutine mode) — waiter-list parking.
	// Uses SelectWaiterNode (SWN) entries that are layout-compatible with G at
	// fields 0-4, allowing them to coexist on channel waiter lists. A per-select
	// mutex (select_mutex) prevents enqueue-before-suspend races and provides
	// wake-once semantics via G.select_case CAS under the mutex.
	//
	// Protocol:
	//   1. Create select_mutex, lock it
	//   2. Set G.select_case = -1
	//   3. Store select_mutex in G.park_mutex (BEFORE enqueue — prevents race
	//      where a waker dequeues SWN and reads G.park_mutex before we set it)
	//   4. For each case: alloca SWN, init, enqueue on channel's waiter list
	//   5. Unlock all channel mutexes
	//   6. coro.suspend → scheduler unlocks select_mutex (via park_mutex)
	//   7. Channel wake code dequeues SWN, calls select_try_wake (wake-once)
	//   8. On resume: lock all channels, remove remaining SWNs, dispatch on G.select_case
	if s.Default == nil && c.inCoroutine {
		c.block = afterTryBlk

		gTy := goroutineStructType()
		swnTy := selectWaiterNodeType()
		currentG := c.block.NewLoad(i8PtrTy, c.currentGGlobal)
		gTyped := c.block.NewBitCast(currentG, irtypes.NewPointer(gTy))

		// 1. Create select_mutex and lock it
		selectMtx := c.block.NewCall(c.palMutexInit)
		c.block.NewCall(c.palMutexLock, selectMtx)

		// 2. Set G.select_case = -1 (unclaimed)
		scField := c.block.NewGetElementPtr(gTy, gTyped,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(gFieldSelectCase)))
		neg1 := constant.NewInt(irtypes.I32, 0xFFFFFFFF) // -1 as unsigned i32
		c.block.NewStore(neg1, scField)

		// 3. Store select_mutex in G.park_mutex BEFORE enqueueing SWNs.
		// This ensures that any waker that dequeues an SWN will see a valid
		// select_mutex in G.park_mutex (not null). The select_mutex is locked,
		// so the waker blocks in select_try_wake until the scheduler unlocks it
		// after coro.suspend — preventing the enqueue-before-suspend race.
		pmField := c.block.NewGetElementPtr(gTy, gTyped,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(gFieldParkMutex)))
		c.block.NewStore(selectMtx, pmField)

		// 4. For each case: alloca SWN, init, enqueue on channel's waiter list
		swnAllocas := make([]value.Value, nCases)
		for i, ci := range caseInfos {
			swn := c.createEntryAlloca(swnTy)
			swnAllocas[i] = swn

			// Initialize SWN fields. Fields 0,2,3 are padding (set to null).
			// Field 4 (next) is set to null by select_waiter_enqueue.
			for _, padIdx := range []int64{0, 2, 3} {
				padF := c.block.NewGetElementPtr(swnTy, swn,
					constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, padIdx))
				c.block.NewStore(constant.NewNull(i8PtrTy), padF)
			}
			// field 1 (kind) = 0xFF sentinel
			kindF := c.block.NewGetElementPtr(swnTy, swn,
				constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 1))
			c.block.NewStore(constant.NewInt(irtypes.I8, swnKindSentinel), kindF)
			// field 5 (g) = currentG
			gF := c.block.NewGetElementPtr(swnTy, swn,
				constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(swnFieldG)))
			c.block.NewStore(currentG, gF)
			// field 6 (case_index) = i
			ciF := c.block.NewGetElementPtr(swnTy, swn,
				constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(swnFieldCaseIndex)))
			c.block.NewStore(constant.NewInt(irtypes.I32, int64(i)), ciF)
			// field 7 (select_mutex) = selectMtx
			smF := c.block.NewGetElementPtr(swnTy, swn,
				constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(swnFieldSelectMutex)))
			c.block.NewStore(selectMtx, smF)

			// Enqueue SWN on the appropriate channel waiter list
			swnRaw := c.block.NewBitCast(swn, i8PtrTy)
			if ci.isSend {
				headPtr := c.block.NewGetElementPtr(chanType, ci.chPtr,
					constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(chanFieldSendWaitersHead)))
				tailPtr := c.block.NewGetElementPtr(chanType, ci.chPtr,
					constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(chanFieldSendWaitersTail)))
				c.block.NewCall(c.funcs["promise_select_waiter_enqueue"], headPtr, tailPtr, swnRaw)
			} else {
				headPtr := c.block.NewGetElementPtr(chanType, ci.chPtr,
					constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(chanFieldRecvWaitersHead)))
				tailPtr := c.block.NewGetElementPtr(chanType, ci.chPtr,
					constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(chanFieldRecvWaitersTail)))
				c.block.NewCall(c.funcs["promise_select_waiter_enqueue"], headPtr, tailPtr, swnRaw)
			}
		}

		// 5. Unlock all channel mutexes
		unlockAll()

		// 6. coro.suspend — G.park_mutex already set (step 3), scheduler unlocks after suspend
		suspResult := c.block.NewCall(c.coroSuspend, constant.None, constant.False)
		resumeBlk := c.newBlock("select.resume")
		c.block.NewSwitch(suspResult, c.coroSuspendBlk,
			ir.NewCase(constant.NewInt(irtypes.I8, 0), resumeBlk),
			ir.NewCase(constant.NewInt(irtypes.I8, 1), c.coroCleanupBlk))

		// 8. On resume: lock all channels, remove SWNs, dispatch on G.select_case
		c.block = resumeBlk

		// Re-lock all channels in sorted order (same code as lockStartBlk but inline)
		for i := 0; i < nCases; i++ {
			ptr := c.block.NewGetElementPtr(arrType, chArr,
				constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(i)))
			chRawSorted := c.block.NewLoad(i8PtrTy, ptr)
			chPtrSorted := c.block.NewBitCast(chRawSorted, irtypes.NewPointer(chanType))
			mtxPtr := c.block.NewGetElementPtr(chanType, chPtrSorted,
				constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(chanFieldMutex)))
			mtx := c.block.NewLoad(i8PtrTy, mtxPtr)

			if i > 0 {
				prevPtr := c.block.NewGetElementPtr(arrType, chArr,
					constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(i-1)))
				prevRaw := c.block.NewLoad(i8PtrTy, prevPtr)
				isSame := c.block.NewICmp(enum.IPredEQ, chRawSorted, prevRaw)
				lockBlk := c.newBlock(fmt.Sprintf("select.wake.lock.%d", i))
				skipBlk := c.newBlock(fmt.Sprintf("select.wake.lock.skip.%d", i))
				c.block.NewCondBr(isSame, skipBlk, lockBlk)
				c.block = lockBlk
				c.block.NewCall(c.palMutexLock, mtx)
				c.block.NewBr(skipBlk)
				c.block = skipBlk
			} else {
				c.block.NewCall(c.palMutexLock, mtx)
			}
		}

		// Remove all SWNs from channel waiter lists (cleanup)
		for i, ci := range caseInfos {
			swnRaw := c.block.NewBitCast(swnAllocas[i], i8PtrTy)
			if ci.isSend {
				headPtr := c.block.NewGetElementPtr(chanType, ci.chPtr,
					constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(chanFieldSendWaitersHead)))
				tailPtr := c.block.NewGetElementPtr(chanType, ci.chPtr,
					constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(chanFieldSendWaitersTail)))
				c.block.NewCall(c.funcs["promise_waiter_remove"], headPtr, tailPtr, swnRaw)
			} else {
				headPtr := c.block.NewGetElementPtr(chanType, ci.chPtr,
					constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(chanFieldRecvWaitersHead)))
				tailPtr := c.block.NewGetElementPtr(chanType, ci.chPtr,
					constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(chanFieldRecvWaitersTail)))
				c.block.NewCall(c.funcs["promise_waiter_remove"], headPtr, tailPtr, swnRaw)
			}
		}

		// Destroy select_mutex — no longer needed after SWN cleanup.
		// All channel mutexes are held, so no concurrent select_try_wake can
		// be in progress. The scheduler already unlocked it after suspend.
		c.block.NewCall(c.palMutexDestroy, selectMtx)

		// Read G.select_case to determine which case won
		wonCase := c.block.NewLoad(irtypes.I32, scField)

		// Generate wake-path case execution blocks
		// Each block: execute the send/recv, unlock all, run body, branch to merge
		wakeCaseBlks := make([]*ir.Block, nCases)
		var switchCases []*ir.Case
		for i := range nCases {
			wakeCaseBlks[i] = c.newBlock(fmt.Sprintf("select.wake.case%d", i))
			switchCases = append(switchCases, ir.NewCase(
				constant.NewInt(irtypes.I32, int64(i)), wakeCaseBlks[i]))
		}

		// Default for switch: unreachable (select_case must be a valid index)
		unreachableBlk := c.newBlock("select.wake.unreachable")
		c.block.NewSwitch(wonCase, unreachableBlk, switchCases...)
		unreachableBlk.NewUnreachable()

		// B0110: Create a retry block for wake-path send cases whose
		// send condition is no longer valid. Between the wake (receiver
		// drains a slot) and re-locking channels, another sender may
		// have filled the freed slot. When this happens, unlock all
		// channels and retry from the lock+try-check chain.
		wakeRetryBlk := c.newBlock("select.wake.retry")
		c.block = wakeRetryBlk
		unlockAll()
		c.block.NewBr(lockStartBlk)

		for i, ci := range caseInfos {
			c.block = wakeCaseBlks[i]
			savedScopeLen := len(c.scopeBindings)

			prefix := fmt.Sprintf("select.wk%d", i)
			if ci.isSend {
				// B0110: Re-check send condition after wake — between wake
				// and re-lock, another sender may have filled the freed slot.
				countPtr := c.block.NewGetElementPtr(chanType, ci.chPtr,
					constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(chanFieldCount)))
				count := c.block.NewLoad(irtypes.I64, countPtr)
				capPtr := c.block.NewGetElementPtr(chanType, ci.chPtr,
					constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(chanFieldCapacity)))
				cap_ := c.block.NewLoad(irtypes.I64, capPtr)
				notFull := c.block.NewICmp(enum.IPredULT, count, cap_)
				closedPtr := c.block.NewGetElementPtr(chanType, ci.chPtr,
					constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(chanFieldClosed)))
				closedVal := c.block.NewLoad(irtypes.I8, closedPtr)
				isOpen := c.block.NewICmp(enum.IPredEQ, closedVal, constant.NewInt(irtypes.I8, 0))
				canSend := c.block.NewAnd(notFull, isOpen)
				sendOkBlk := c.newBlock(prefix + ".send.ok")
				c.block.NewCondBr(canSend, sendOkBlk, wakeRetryBlk)
				c.block = sendOkBlk
				execSend(ci, prefix)
			} else {
				execRecv(ci, prefix)
			}

			unlockAll()

			for _, stmt := range s.Cases[i].Body {
				if c.block.Term != nil {
					break
				}
				c.genStmt(stmt)
			}
			if c.block != nil && c.block.Term == nil && len(c.scopeBindings) > savedScopeLen {
				cap := c.emitScopeCleanup(savedScopeLen, false)
				c.emitCloseErrCheck(cap, savedScopeLen)
			}
			c.scopeBindings = c.scopeBindings[:savedScopeLen]
			if c.block != nil && c.block.Term == nil {
				c.block.NewBr(mergeBlk)
			}
		}

	}

	// Thread-blocking poll fallback for non-coroutine context (B0045).
	// When no case is immediately ready and we can't park (not a coroutine),
	// unlock all channels, yield to let goroutines make progress, then
	// re-lock and retry the try-check chain.
	if s.Default == nil && !c.inCoroutine && !c.isWasm {
		c.block = afterTryBlk
		unlockAll()
		c.block.NewCall(c.palUsleep, constant.NewInt(irtypes.I32, 100))
		c.block.NewBr(lockStartBlk)
	}

	// T1220: WASM non-coroutine blocking select — pump the cooperative scheduler
	// instead of silently falling through to mergeBlk (sibling of T1200/T1218).
	// pal_usleep can't pump the single-threaded coop scheduler, so use
	// emitWasmCoopWaitPump: on progress re-lock + re-test every case, on
	// coop_step==2 emit a signature-correct early-return (per-test deadline),
	// on coop_step==0 take the terminal deadlock exit(2).
	if s.Default == nil && !c.inCoroutine && c.isWasm {
		c.block = afterTryBlk
		unlockAll()
		c.emitWasmCoopWaitPump(lockStartBlk)
	}

	c.block = mergeBlk
}
