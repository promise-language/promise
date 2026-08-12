package codegen

import (
	"fmt"
	"strconv"

	"github.com/llir/llvm/ir"
	"github.com/llir/llvm/ir/constant"
	"github.com/llir/llvm/ir/enum"
	irtypes "github.com/llir/llvm/ir/types"
	"github.com/llir/llvm/ir/value"

	"github.com/promise-language/promise/compiler/internal/ast"
	"github.com/promise-language/promise/compiler/internal/sema"
	"github.com/promise-language/promise/compiler/internal/types"
)

// --- For-in loop ---

// forInIterableType returns the substituted iterable type of a for-in loop with
// one borrow layer stripped. A borrowed container (T0971) has the same runtime
// layout as the owned form, and genExpr(s.Iterable) already yields the same
// slice/map/string/range value (resolveType unwraps refs), so the type dispatch
// must run on the unwrapped type. Centralizes the strip for every site that
// re-derives the iterable type (genForInStmt, genForInRange, genForInMap).
func (c *Compiler) forInIterableType(s *ast.ForInStmt) types.Type {
	t := c.info.Types[s.Iterable]
	if c.typeSubst != nil {
		t = types.Substitute(t, c.typeSubst)
	}
	if mr, ok := t.(*types.MutRef); ok {
		return mr.Elem()
	}
	if sr, ok := t.(*types.SharedRef); ok {
		return sr.Elem()
	}
	return t
}

func (c *Compiler) genForInStmt(s *ast.ForInStmt) {
	iterableType := c.forInIterableType(s)

	if arr, ok := iterableType.(*types.Array); ok {
		c.genForInArray(s, arr)
	} else if elem, ok := types.AsVector(iterableType); ok {
		slicePtr := c.genExpr(s.Iterable)
		// T0109: Register a scope binding for temporary vectors returned by call
		// expressions (e.g., for elem in set.to_vector()). Variable-backed vectors
		// are dropped by their own scope bindings; only call results are orphaned.
		// Using a scope binding ensures cleanup on ALL exit paths (normal exit,
		// early return, break, panic) — not just after the loop.
		// T0494: extended from CallExpr-only to any tracked stmt temp so getter
		// MemberExpr results (e.g., `for k,v in resp.headers`) also survive the
		// for-in's lifetime. stmtTemps are NOT saved across block entry (unlike
		// heapTemps), so without this promotion the temp would be dropped by the
		// first body statement's cleanupStmtTemps and the loop would read freed
		// memory.
		if idx, isTracked := c.stmtTempMap[slicePtr]; isTracked && idx >= 0 {
			if dropFn, ok := c.funcs["Vector.drop"]; ok {
				tmpName := c.uniqueLocalName("__forin_vec_tmp")
				tmpAlloca := c.createEntryAlloca(irtypes.I8Ptr)
				tmpAlloca.SetName(tmpName)
				c.block.NewStore(slicePtr, tmpAlloca)
				dropFlag := c.createEntryAlloca(irtypes.I1)
				dropFlag.SetName(tmpName + ".dropflag")
				c.block.NewStore(constant.NewInt(irtypes.I1, 1), dropFlag)
				c.scopeBindings = append(c.scopeBindings, scopeBinding{
					kind:     bindingDropString, // reuse: same i8* alloca + void(i8*) drop pattern
					alloca:   tmpAlloca,
					named:    types.TypVector,
					valType:  iterableType,
					dropFlag: dropFlag,
					dropFunc: dropFn,
					varName:  tmpName,
				})
				// Claim the stmtTemp so it's not also dropped at statement end —
				// ownership transferred to the scope binding (prevents double-free).
				c.claimStringTemp(slicePtr)
			}
		}
		c.genForInVector(s, slicePtr, elem)
	} else if key, val, ok := types.AsMap(iterableType); ok {
		mapPtr := c.genExpr(s.Iterable)
		c.genForInMap(s, mapPtr, key, val)
	} else if elem, ok := types.AsChannel(iterableType); ok {
		chPtr := c.genExpr(s.Iterable)
		// T0502: Same lifetime-extension fix as the vector/string for-in
		// branches. When the iterable is a tracked stmt temp (getter result,
		// call result), promote it to a scope binding so the body's
		// cleanupStmtTemps doesn't free the channel mid-loop and the channel
		// is reliably dropped on all exit paths.
		if idx, isTracked := c.stmtTempMap[chPtr]; isTracked && idx >= 0 {
			resolvedChanElem := elem
			if c.typeSubst != nil && resolvedChanElem != nil {
				resolvedChanElem = types.Substitute(resolvedChanElem, c.typeSubst)
			}
			if resolvedChanElem != nil {
				// T0663: per-element-type drop walks any un-received buffered items.
				dropFn := c.getOrCreateChannelDrop(resolvedChanElem)
				tmpName := c.uniqueLocalName("__forin_ch_tmp")
				tmpAlloca := c.createEntryAlloca(irtypes.I8Ptr)
				tmpAlloca.SetName(tmpName)
				c.block.NewStore(chPtr, tmpAlloca)
				dropFlag := c.createEntryAlloca(irtypes.I1)
				dropFlag.SetName(tmpName + ".dropflag")
				c.block.NewStore(constant.NewInt(irtypes.I1, 1), dropFlag)
				c.scopeBindings = append(c.scopeBindings, scopeBinding{
					kind:     bindingDropString, // reuse: same i8* alloca + void(i8*) drop pattern
					alloca:   tmpAlloca,
					named:    types.TypChannel,
					valType:  iterableType,
					dropFlag: dropFlag,
					dropFunc: dropFn,
					varName:  tmpName,
				})
				c.claimStringTemp(chPtr)
			}
		}
		c.genForInChannel(s, chPtr, elem)
	} else if elem, ok := types.AsStream(iterableType); ok {
		genVal := c.genExpr(s.Iterable)
		// T0284: Failable generator factory called without explicit error handling.
		// Unwrap the result struct before passing to genForInGenerator.
		if c.info.FailableExprs[s.Iterable] {
			genVal = c.unwrapFailableGeneratorResult(genVal, s.Pos())
		}
		// T0088: Generators have their own cleanup (bindingGenerator). Clear all
		// pending heap temps to prevent __promise_iter_cleanup from running on
		// generator instances (which have a different layout than _FnIter).
		for i := range c.heapTemps {
			c.block.NewStore(constant.NewInt(irtypes.I1, 0), c.heapTemps[i].dropFlag)
		}
		c.genForInGenerator(s, genVal, elem)
	} else if elem, ok := types.AsRange(iterableType); ok {
		c.genForInRange(s, elem)
	} else {
		// String iteration
		named := extractNamed(iterableType)
		if named == types.TypString {
			strPtr := c.genExpr(s.Iterable)
			// T0494: Same lifetime-extension fix as the vector path. When the
			// iterable is a tracked stmt temp (call result, getter result,
			// string concat result, etc.), promote it to a scope binding so
			// the body's stmt-end cleanup doesn't free the string mid-loop.
			if idx, isTracked := c.stmtTempMap[strPtr]; isTracked && idx >= 0 {
				if dropFn, ok := c.funcs["promise_string_drop"]; ok {
					tmpName := c.uniqueLocalName("__forin_str_tmp")
					tmpAlloca := c.createEntryAlloca(irtypes.I8Ptr)
					tmpAlloca.SetName(tmpName)
					c.block.NewStore(strPtr, tmpAlloca)
					dropFlag := c.createEntryAlloca(irtypes.I1)
					dropFlag.SetName(tmpName + ".dropflag")
					c.block.NewStore(constant.NewInt(irtypes.I1, 1), dropFlag)
					c.scopeBindings = append(c.scopeBindings, scopeBinding{
						kind:     bindingDropString,
						alloca:   tmpAlloca,
						named:    types.TypString,
						valType:  iterableType,
						dropFlag: dropFlag,
						dropFunc: dropFn,
						varName:  tmpName,
					})
					c.claimStringTemp(strPtr)
				}
			}
			c.genForInString(s, strPtr)
			return
		}
		// Duck-typed for-in: check sema ForInKinds
		if kind, ok := c.info.ForInKinds[s]; ok {
			iterVal := c.genExpr(s.Iterable)
			switch kind {
			case sema.ForInNext:
				c.genForInCustomIter(s, iterVal, iterableType)
			case sema.ForInIter:
				c.genForInCustomStream(s, iterVal, iterableType)
			}
			return
		}
		panic(fmt.Sprintf("codegen: unsupported for-in iterable type %s", iterableType))
	}
}

// genForInCustomIter handles for-in over any type with a next() T? method.
// Calls .next() in a loop via virtual dispatch (structural interface) or direct call (concrete type).
func (c *Compiler) genForInCustomIter(s *ast.ForInStmt, iterVal value.Value, iterType types.Type) {
	// Resolve element type from the next() return type
	named := extractNamed(iterType)
	if named == nil {
		panic(fmt.Sprintf("codegen: genForInCustomIter on non-named type %s", iterType))
	}
	nextMethod := named.LookupMethod("next")
	if nextMethod == nil {
		panic(fmt.Sprintf("codegen: type %s has no next() method", named))
	}

	// Resolve the optional return type: next() returns T?
	retType := nextMethod.Sig().Result()
	if inst, ok := iterType.(*types.Instance); ok {
		if origin, ok := inst.Origin().(*types.Named); ok && len(origin.TypeParams()) > 0 {
			subst := types.BuildSubstMap(origin.TypeParams(), inst.TypeArgs())
			retType = types.Substitute(retType, subst)
		}
	}
	if c.typeSubst != nil {
		retType = types.Substitute(retType, c.typeSubst)
	}
	optType, ok := retType.(*types.Optional)
	if !ok {
		panic(fmt.Sprintf("codegen: next() on %s does not return optional", named))
	}
	elemType := optType.Elem()
	elemLLVM := c.resolveType(elemType)
	optLLVM := c.resolveType(retType)

	// Store iterable value in alloca for repeated .next() calls
	iterAlloca := c.createEntryAlloca(iterVal.Type())
	iterAlloca.SetName(c.uniqueLocalName("iter.val"))
	c.block.NewStore(iterVal, iterAlloca)

	// Element binding
	elemAlloca := c.createEntryAlloca(elemLLVM)
	elemAlloca.SetName(c.uniqueLocalName(s.Binding))
	if s.Binding != "_" {
		c.locals[s.Binding] = elemAlloca
	}

	// Optional index variable
	if s.Index != "" && s.Index != "_" {
		indexAlloca := c.createEntryAlloca(irtypes.I64)
		indexAlloca.SetName(c.uniqueLocalName(s.Index))
		c.block.NewStore(constant.NewInt(irtypes.I64, 0), indexAlloca)
		c.locals[s.Index] = indexAlloca
	}

	headerBlk := c.newBlock("iter.header")
	bodyBlk := c.newBlock("iter.body")
	updateBlk := c.newBlock("iter.update")
	exitBlk := c.newBlock("iter.exit")

	c.block.NewBr(headerBlk)

	// Header: call .next(), check optional
	c.block = headerBlk
	curIter := c.block.NewLoad(iterVal.Type(), iterAlloca)
	nextResult := c.emitIterNext(curIter, iterType, named, nextMethod, optLLVM)

	// Check optional discriminant: field 0 is i1 (true=some, false=none)
	tag := c.block.NewExtractValue(nextResult, 0)
	isNone := c.block.NewICmp(enum.IPredEQ, tag, constant.NewInt(irtypes.I1, 0))
	c.block.NewCondBr(isNone, exitBlk, bodyBlk)

	// Body: extract value, bind, execute
	savedBreak := c.breakTarget
	savedContinue := c.continueTarget
	savedLoopScopeDepth := c.loopScopeDepth
	c.breakTarget = exitBlk
	c.continueTarget = updateBlk
	c.loopScopeDepth = len(c.scopeBindings)
	savedLoopTempFloor := c.enterLoopTempFloor() // T1331

	c.block = bodyBlk
	val := c.block.NewExtractValue(nextResult, 1)
	c.block.NewStore(val, elemAlloca)

	// T1440: next() hands back an owned T — Vector.iter()'s closure dups the
	// element, every combinator forwards an owned value, and sema rejects any
	// user next() that tries to return a borrow ("cannot return a borrowed
	// reference as owned"). Ownership analysis already assumes this: it exempts
	// iterator-based for-ins from the loop-binding move ban that native
	// container for-ins get (forInAliasingElementType, ownership/expr.go). So
	// the loop variable owns the element and must drop it once per iteration.
	//
	// Register the binding at loopScopeDepth so a heap element (string/Vector/user/Optional…)
	// is dropped on every exit: normal iteration end (below), break/continue
	// (genBreak/ContinueStmt → emitScopeCleanup(loopScopeDepth)), and early
	// return/raise (function unwind). maybeRegisterDrop no-ops for value elems,
	// so `int[]` iteration is unchanged. Mirrors T0671's channel for-in.
	bodyScopeStart := len(c.scopeBindings) // == c.loopScopeDepth
	c.maybeRegisterDrop(s.Binding, elemAlloca, elemType)
	c.genBlock(s.Body)

	if c.block != nil && c.block.Term == nil {
		if len(c.scopeBindings) > bodyScopeStart {
			cap := c.emitScopeCleanup(bodyScopeStart, false)
			c.emitCloseErrCheck(cap, bodyScopeStart)
		}
		c.block.NewBr(updateBlk)
	}
	c.scopeBindings = c.scopeBindings[:bodyScopeStart] // unconditional codegen-time pop

	// Update: increment index, branch back to header
	c.block = updateBlk
	if s.Index != "" && s.Index != "_" {
		idxAlloca := c.locals[s.Index]
		curIdx := c.block.NewLoad(irtypes.I64, idxAlloca)
		nextIdx := c.block.NewAdd(curIdx, constant.NewInt(irtypes.I64, 1))
		c.block.NewStore(nextIdx, idxAlloca)
	}
	c.block.NewBr(headerBlk)

	c.breakTarget = savedBreak
	c.continueTarget = savedContinue
	c.loopScopeDepth = savedLoopScopeDepth
	c.loopTempFloor = savedLoopTempFloor // T1331

	c.block = exitBlk
}

// genForInCustomStream handles for-in over any type with an iter() method.
// Calls .iter() to get an iterator, then delegates to genForInCustomIter.
func (c *Compiler) genForInCustomStream(s *ast.ForInStmt, streamVal value.Value, streamType types.Type) {
	named := extractNamed(streamType)
	if named == nil {
		panic(fmt.Sprintf("codegen: genForInCustomStream on non-named type %s", streamType))
	}
	iterMethod := named.LookupMethod("iter")
	if iterMethod == nil {
		panic(fmt.Sprintf("codegen: type %s has no iter() method", named))
	}

	// Resolve iter() return type
	iterRetType := iterMethod.Sig().Result()
	if inst, ok := streamType.(*types.Instance); ok {
		if origin, ok := inst.Origin().(*types.Named); ok && len(origin.TypeParams()) > 0 {
			subst := types.BuildSubstMap(origin.TypeParams(), inst.TypeArgs())
			iterRetType = types.Substitute(iterRetType, subst)
		}
	}
	if c.typeSubst != nil {
		iterRetType = types.Substitute(iterRetType, c.typeSubst)
	}

	// Call .iter() on the stream value
	iterResult := c.emitIterNext(streamVal, streamType, named, iterMethod, c.resolveType(iterRetType))

	// B0173/T0997: The .iter() call allocates a FRESH heap iterator instance that
	// is not tracked by statement-level cleanup (synthetic call, no AST node for
	// maybeTrackIterTemp). Register it as a scope binding — like the vector/string/
	// channel for-in branches above — so it is dropped exactly once on EVERY exit
	// path: normal completion, break, AND early return/raise from the loop body.
	// (return/raise unwind via the scope-cleanup stack and bypass any post-loop
	// inline cleanup, which would leak the iterator.) Registered BEFORE the loop so
	// it sits below loopScopeDepth: break/continue cleanup leaves it alone, and the
	// enclosing scope frees it once.
	//
	// Drop dispatches through __promise_structural_drop (RTTI: typeinfo.drop_fn_ptr)
	// for BOTH structural and concrete iterators — handling the closure-based _FnIter
	// (whose drop_fn calls iterCleanup to free the env + parent chain), a user-defined
	// structural iterator (e.g., NumIter) whose layout is NOT _FnIter-shaped, AND a
	// concrete iterator type. Using iterCleanup here would misread a user iterator's
	// fields as the _FnIter _parent pointer and recurse into garbage.
	//
	// T1076: concrete iterators previously used pal_free directly, which freed the
	// instance struct WITHOUT running the type's drop() — leaking any heap resource
	// the iterator owned (user drop() or synth field drop). __promise_structural_drop
	// dispatches via the instance's typeinfo drop_fn_ptr (user drop wrapped with
	// pal_free per B0247, or synth drop), falling back to pal_free when the type has
	// no drop — so it is correct for every concrete sub-case and strictly more correct
	// than the old pal_free.
	iterNamed := extractNamed(iterRetType)
	// T1000: value-type iterators are rejected in sema (checkDuckTypedForIn), so
	// the value branch here is unreachable — the guard stays as defensive code.
	if iterNamed != nil && !iterNamed.IsValueType() {
		if _, ok := iterResult.Type().(*irtypes.StructType); ok {
			instancePtr := c.block.NewExtractValue(iterResult, 1)
			dropFn := c.palFree
			if c.structuralDrop != nil {
				dropFn = c.structuralDrop
			}
			tmpName := c.uniqueLocalName("__forin_iter_tmp")
			tmpAlloca := c.createEntryAlloca(irtypes.I8Ptr)
			tmpAlloca.SetName(tmpName)
			c.block.NewStore(instancePtr, tmpAlloca)
			dropFlag := c.createEntryAlloca(irtypes.I1)
			dropFlag.SetName(tmpName + ".dropflag")
			c.block.NewStore(constant.NewInt(irtypes.I1, 1), dropFlag)
			c.scopeBindings = append(c.scopeBindings, scopeBinding{
				kind:     bindingDropString, // reuse: i8* alloca + void(i8*) drop pattern
				alloca:   tmpAlloca,
				valType:  iterRetType,
				dropFlag: dropFlag,
				dropFunc: dropFn,
				varName:  tmpName,
			})
		}
	}

	// Delegate to genForInCustomIter with the iterator value
	c.genForInCustomIter(s, iterResult, iterRetType)
}

// emitIterNext emits a call to a method on a value, using virtual dispatch
// for types that need vtables (structural interfaces) or direct dispatch otherwise.
// This is a synthetic method call (no AST nodes) used by duck-typed for-in iteration.
func (c *Compiler) emitIterNext(receiverVal value.Value, receiverType types.Type,
	named *types.Named, method *types.Method, retLLVM irtypes.Type) value.Value {

	// T1395: the for-in protocol supplies no arguments, but a concrete next()
	// may satisfy `Iterator[T]` with EXTRA trailing defaulted/optional params.
	// Fill them, or the callee reads a garbage value for each (e.g. a `step`
	// default that arrives as 0 makes the loop never advance).
	defaultArgs, defaultTypes := c.emitTrailingDefaultArgValues(
		method.Sig(), 0, c.buildOwnerTypeArgSubst(receiverType))

	if c.needsVtable(named) && !method.IsNative() {
		// Virtual dispatch: extract vtable + instance, call through vtable slot
		vtableRaw := c.extractVtablePtr(receiverVal)
		instance := c.extractInstancePtr(receiverVal)

		slotIndex := named.VirtualMethodIndex(method.Name(), false)
		if slotIndex < 0 {
			panic(fmt.Sprintf("codegen: method %s not in vtable for %s", method.Name(), named))
		}
		vtablePtr := c.block.NewBitCast(vtableRaw, irtypes.NewPointer(irtypes.I8Ptr))
		fnSlotPtr := c.block.NewGetElementPtr(irtypes.I8Ptr, vtablePtr,
			constant.NewInt(irtypes.I32, int64(slotIndex)))
		fnRaw := c.block.NewLoad(irtypes.I8Ptr, fnSlotPtr)

		// Build function type: (i8*, <defaulted params...>) -> retLLVM
		funcType := irtypes.NewFunc(retLLVM, append([]irtypes.Type{irtypes.I8Ptr}, defaultTypes...)...)
		fnTyped := c.block.NewBitCast(fnRaw, irtypes.NewPointer(funcType))

		return c.block.NewCall(fnTyped, append([]value.Value{instance}, defaultArgs...)...)
	}

	// Direct dispatch: call the concrete method function
	ownerName := c.resolveTypeName(receiverType)
	mangledName := mangleMethodName(ownerName, method.Name(), false)
	fn, ok := c.funcs[mangledName]
	if !ok {
		panic(fmt.Sprintf("codegen: undeclared method %s", mangledName))
	}

	// Extract instance pointer as receiver
	instance := c.extractInstancePtr(receiverVal)
	return c.block.NewCall(fn, append([]value.Value{instance}, defaultArgs...)...)
}

// genForInRange handles for-in over a Range[T] value type (e.g., 0..10, 'a'..'z').
// Extracts start/end/inclusive from the value type struct and uses a direct counter loop.
func (c *Compiler) genForInRange(s *ast.ForInStmt, elemType types.Type) {
	rangeVal := c.genExpr(s.Iterable)

	// Get the layout to find field indices. T0971: unwrap a borrowed Range so
	// its value-type layout resolves.
	iterableType := c.forInIterableType(s)
	layout := c.lookupTypeLayout(iterableType)
	if layout == nil {
		panic(fmt.Sprintf("codegen: no layout for range type %s", iterableType))
	}

	// Extract fields from value struct via extractvalue
	startIdx := uint64(layout.ValueFieldIndex["start"])
	endIdx := uint64(layout.ValueFieldIndex["end"])
	inclIdx := uint64(layout.ValueFieldIndex["inclusive"])
	start := c.block.NewExtractValue(rangeVal, startIdx)
	end := c.block.NewExtractValue(rangeVal, endIdx)
	inclusive := c.block.NewExtractValue(rangeVal, inclIdx)

	// Determine element LLVM type and comparison predicate
	elemLLVM := c.resolveType(elemType)
	ltPred := enum.IPredSLT // signed less-than by default
	named := extractNamed(elemType)
	if named != nil && classify(named) == CatUnsignedInt {
		ltPred = enum.IPredULT
	}

	counterAlloca := c.createEntryAlloca(elemLLVM)
	counterAlloca.SetName(c.uniqueLocalName(s.Binding))
	c.block.NewStore(start, counterAlloca)
	c.locals[s.Binding] = counterAlloca

	if s.Index != "" {
		indexAlloca := c.createEntryAlloca(irtypes.I64)
		indexAlloca.SetName(c.uniqueLocalName(s.Index))
		c.block.NewStore(constant.NewInt(irtypes.I64, 0), indexAlloca)
		c.locals[s.Index] = indexAlloca
	}

	headerBlock := c.newBlock("forin.header")
	bodyBlock := c.newBlock("forin.body")
	updateBlock := c.newBlock("forin.update")
	exitBlock := c.newBlock("forin.exit")

	c.block.NewBr(headerBlock)

	// Header: counter < end || (counter == end && inclusive)
	c.block = headerBlock
	counter := c.block.NewLoad(elemLLVM, counterAlloca)
	ltCond := c.block.NewICmp(ltPred, counter, end)
	eqCond := c.block.NewICmp(enum.IPredEQ, counter, end)
	inclAndEq := c.block.NewAnd(inclusive, eqCond)
	cond := c.block.NewOr(ltCond, inclAndEq)
	c.block.NewCondBr(cond, bodyBlock, exitBlock)

	savedBreak := c.breakTarget
	savedContinue := c.continueTarget
	savedLoopUseDepth := c.loopScopeDepth
	c.breakTarget = exitBlock
	c.continueTarget = updateBlock
	c.loopScopeDepth = len(c.scopeBindings)
	savedLoopTempFloor := c.enterLoopTempFloor() // T1331

	c.block = bodyBlock
	c.genBlock(s.Body)
	if c.block.Term == nil {
		c.block.NewBr(updateBlock)
	}

	// Update: increment counter
	c.block = updateBlock
	cur := c.block.NewLoad(elemLLVM, counterAlloca)
	one := constant.NewInt(elemLLVM.(*irtypes.IntType), 1)
	next := c.block.NewAdd(cur, one)
	c.block.NewStore(next, counterAlloca)

	if s.Index != "" {
		idxAlloca := c.locals[s.Index]
		curIdx := c.block.NewLoad(irtypes.I64, idxAlloca)
		nextIdx := c.block.NewAdd(curIdx, constant.NewInt(irtypes.I64, 1))
		c.block.NewStore(nextIdx, idxAlloca)
	}

	c.emitYieldCheck()
	c.block.NewBr(headerBlock)

	c.breakTarget = savedBreak
	c.continueTarget = savedContinue
	c.loopScopeDepth = savedLoopUseDepth
	c.loopTempFloor = savedLoopTempFloor // T1331
	c.block = exitBlock
}

// --- lookupLocalType resolves the declared type for a TypedVarDecl ---
// It checks the TypeRef AST node to detect Optional declarations,
// then resolves the type by looking up the variable in sema scopes.

func (c *Compiler) lookupLocalType(s *ast.TypedVarDecl) types.Type {
	// Only need special handling for Optional declarations
	optRef, ok := s.Type.(*ast.OptionalTypeRef)
	if !ok {
		return nil // use expression type
	}
	// Always resolve the declared type from the AST so nested OptionalTypeRef
	// (T??, T???) preserves its full depth even when the value expr is itself
	// Optional (e.g. `T?? b = a` where a:T?). Using exprType here would collapse
	// the alloca to T?, mismatching sema and breaking unwraps.
	if t := c.resolveTypeRefToType(optRef); t != nil {
		return t
	}
	return c.lookupVarType(s.Name)
}

// resolveTypeRefToType resolves an AST TypeRef to a types.Type.
// Mirrors sema.Checker.resolveType so codegen can re-derive types for AST refs
// that don't have a direct Types[expr] entry (e.g., the target of `as!`).
func (c *Compiler) resolveTypeRefToType(ref ast.TypeRef) types.Type {
	switch r := ref.(type) {
	case *ast.NamedTypeRef:
		// If typeSubst is active, check if this name matches a TypeParam in the
		// substitution map. This avoids finding the wrong TypeParam from a different
		// generic type's scope during synthesized method body generation.
		if c.typeSubst != nil {
			for tp, concrete := range c.typeSubst {
				if tp.Obj().Name() == r.Name {
					return concrete
				}
			}
		}
		var base types.Type
		// Check Universe scope first (primitives)
		if obj, _ := types.Universe.LookupParent(r.Name); obj != nil {
			if tn, ok := obj.(*types.TypeName); ok {
				base = tn.Type()
			}
		}
		// Check file scope (user-defined types)
		if base == nil {
			for _, scope := range c.info.ScopeOrder {
				if obj := scope.Lookup(r.Name); obj != nil {
					if tn, ok := obj.(*types.TypeName); ok {
						base = tn.Type()
						break
					}
				}
			}
		}
		if base == nil {
			return nil
		}
		if len(r.TypeArgs) == 0 {
			return base
		}
		args := make([]types.Type, len(r.TypeArgs))
		for i, ta := range r.TypeArgs {
			args[i] = c.resolveTypeRefToType(ta)
			if args[i] == nil {
				return nil
			}
		}
		return types.NewInstance(base, args)
	case *ast.QualifiedTypeRef:
		// Module-qualified types (e.g. `vmod.Shower`): resolve the module alias to
		// its *types.Module, then look up the member in the module's own scope —
		// mirroring sema.resolveQualifiedType. T1462: the previous unqualified
		// `scope.Lookup(r.Name)` scan never found module member types (they live in
		// the module scope, not the consumer's file scopes), so it returned nil.
		// For a `mod.Iface s = concrete;` binding that nil collapsed declType to the
		// concrete RHS type, making maybeRegisterDrop emit a full heap drop for `s`
		// that double-freed the box the concrete owner also frees.
		var base types.Type
		for _, scope := range c.info.ScopeOrder {
			if obj := scope.Lookup(r.Module); obj != nil {
				if mod, ok := obj.(*types.Module); ok {
					if ms := mod.Scope(); ms != nil {
						if member := ms.Lookup(r.Name); member != nil {
							if tn, ok := member.(*types.TypeName); ok {
								base = tn.Type()
							}
						}
					}
				}
				break
			}
		}
		// Fallback: bare-name scan across file scopes (robustness for refs whose
		// module object isn't reachable via ScopeOrder).
		if base == nil {
			for _, scope := range c.info.ScopeOrder {
				if obj := scope.Lookup(r.Name); obj != nil {
					if tn, ok := obj.(*types.TypeName); ok {
						base = tn.Type()
						break
					}
				}
			}
		}
		if base == nil {
			return nil
		}
		if len(r.TypeArgs) == 0 {
			return base
		}
		args := make([]types.Type, len(r.TypeArgs))
		for i, ta := range r.TypeArgs {
			args[i] = c.resolveTypeRefToType(ta)
			if args[i] == nil {
				return nil
			}
		}
		return types.NewInstance(base, args)
	case *ast.OptionalTypeRef:
		inner := c.resolveTypeRefToType(r.Inner)
		if inner != nil {
			return types.NewOptional(inner)
		}
	case *ast.SliceTypeRef:
		elem := c.resolveTypeRefToType(r.Element)
		if elem != nil {
			return types.NewVector(elem)
		}
	case *ast.ArrayTypeRef:
		elem := c.resolveTypeRefToType(r.Element)
		if elem == nil {
			return nil
		}
		size, err := strconv.ParseInt(r.Size, 10, 64)
		if err != nil {
			return nil
		}
		return types.NewArray(elem, size)
	case *ast.SharedRefTypeRef:
		inner := c.resolveTypeRefToType(r.Inner)
		if inner != nil {
			return types.NewSharedRef(inner)
		}
	case *ast.MutRefTypeRef:
		inner := c.resolveTypeRefToType(r.Inner)
		if inner != nil {
			return types.NewMutRef(inner)
		}
	case *ast.PointerTypeRef:
		inner := c.resolveTypeRefToType(r.Inner)
		if inner != nil {
			return types.NewPointer(inner)
		}
	case *ast.TupleTypeRef:
		elems := make([]types.Type, len(r.Elements))
		for i, e := range r.Elements {
			elems[i] = c.resolveTypeRefToType(e)
			if elems[i] == nil {
				return nil
			}
		}
		return types.NewTuple(elems)
	case *ast.FunctionTypeRef:
		params := make([]*types.Param, len(r.Params))
		for i, p := range r.Params {
			pt := c.resolveTypeRefToType(p)
			if pt == nil {
				return nil
			}
			params[i] = types.NewParam("", pt, types.RefNone)
		}
		var result types.Type
		if r.Return != nil {
			if named, ok := r.Return.(*ast.NamedTypeRef); ok && named.Name == "void" && len(named.TypeArgs) == 0 {
				// result stays nil
			} else {
				result = c.resolveTypeRefToType(r.Return)
				if result == nil {
					return nil
				}
			}
		}
		return types.NewSignature(nil, params, result, false)
	}
	return nil
}

// lookupVarType finds a variable's declared type by walking sema scopes.
func (c *Compiler) lookupVarType(name string) types.Type {
	for _, scope := range c.info.ScopeOrder {
		if obj := scope.Lookup(name); obj != nil {
			if v, ok := obj.(*types.Var); ok {
				typ := v.Type()
				if c.typeSubst != nil {
					typ = types.Substitute(typ, c.typeSubst)
				}
				return typ
			}
		}
	}
	return nil
}

// uniqueLocalName returns a unique LLVM name for a local variable alloca.
// On first use of a name within a function, returns it unchanged.
// On subsequent uses (shadowing in inner scopes), appends a numeric suffix.
func (c *Compiler) uniqueLocalName(name string) string {
	n := c.localNameCount[name]
	c.localNameCount[name] = n + 1
	if n == 0 {
		return name
	}
	return fmt.Sprintf("%s.%d", name, n)
}

// --- For-in over vectors ---

func (c *Compiler) genForInVector(s *ast.ForInStmt, slicePtr value.Value, elemType types.Type) {
	elemLLVM := c.resolveType(elemType)

	// Load length from header (masked)
	headerType := vectorHeaderType()
	headerPtr := c.block.NewBitCast(slicePtr, irtypes.NewPointer(headerType))
	length := loadVectorLen(c.block, headerPtr)

	// Counter alloca
	counterAlloca := c.createEntryAlloca(irtypes.I64)
	c.block.NewStore(constant.NewInt(irtypes.I64, 0), counterAlloca)

	// Element binding alloca
	elemAlloca := c.createEntryAlloca(elemLLVM)
	elemAlloca.SetName(c.uniqueLocalName(s.Binding))
	c.locals[s.Binding] = elemAlloca

	// B0277: For string elements, register a drop binding so dup'd strings are
	// freed when the loop variable is not moved. The flag starts at 0 (no value
	// to drop before the first iteration).
	dupStrings := s.Binding != "_" && extractNamed(elemType) == types.TypString
	if dupStrings {
		c.maybeRegisterDrop(s.Binding, elemAlloca, elemType)
		c.block.NewStore(constant.NewInt(irtypes.I1, 0), c.dropFlags[s.Binding])
	}

	// T0617: for a Task-element loop, record the current iteration's slot
	// address so `<-handle` (genReceiveTask) can null the consumed slot —
	// otherwise the Vector's scope-exit element drop reloads the freed G
	// and Task[T].drop double-frees → segfault. Per-iteration (not whole
	// vector) so un-awaited slots are still dropped once (T0503).
	// T1434: IsAnyTask covers failable_task[T] too — a `go! {}` handle in a
	// Vector slot double-frees identically (segfault on Linux, hang on macOS).
	isTaskElem := s.Binding != "_" && types.IsAnyTask(elemType)
	var slotPtrAlloca *ir.InstAlloca
	if isTaskElem {
		slotPtrAlloca = c.createEntryAlloca(irtypes.NewPointer(elemLLVM))
	}

	// Index variable if present
	if s.Index != "" {
		indexAlloca := c.createEntryAlloca(irtypes.I64)
		indexAlloca.SetName(c.uniqueLocalName(s.Index))
		c.block.NewStore(constant.NewInt(irtypes.I64, 0), indexAlloca)
		c.locals[s.Index] = indexAlloca
	}

	headerBlock := c.newBlock("forin.header")
	bodyBlock := c.newBlock("forin.body")
	updateBlock := c.newBlock("forin.update")
	exitBlock := c.newBlock("forin.exit")

	c.block.NewBr(headerBlock)

	// Header: counter < length
	c.block = headerBlock
	counter := c.block.NewLoad(irtypes.I64, counterAlloca)
	cond := c.block.NewICmp(enum.IPredULT, counter, length)
	c.block.NewCondBr(cond, bodyBlock, exitBlock)

	// Body: load element, store to binding
	savedBreak := c.breakTarget
	savedContinue := c.continueTarget
	savedLoopUseDepth := c.loopScopeDepth
	c.breakTarget = exitBlock
	c.continueTarget = updateBlock
	c.loopScopeDepth = len(c.scopeBindings)
	savedLoopTempFloor := c.enterLoopTempFloor() // T1331

	// T0617: scope the slot-ptr map entry to this loop (save/restore, like
	// breakTarget) so nesting (`for x in v1 { for x in v2 {} }`) is safe and
	// it self-cleans across functions.
	var prevSlot *ir.InstAlloca
	var hadPrevSlot bool
	if isTaskElem {
		prevSlot, hadPrevSlot = c.forInHandleSlotPtr[s.Binding]
		c.forInHandleSlotPtr[s.Binding] = slotPtrAlloca
	}

	c.block = bodyBlock

	// B0277: Drop previous iteration's dup'd string if not moved, then dup new.
	if dupStrings {
		dropFlag := c.dropFlags[s.Binding]
		flag := c.block.NewLoad(irtypes.I1, dropFlag)
		dropBlk := c.newBlock("forin.str.drop")
		loadBlk := c.newBlock("forin.str.load")
		c.block.NewCondBr(flag, dropBlk, loadBlk)

		c.block = dropBlk
		oldVal := c.block.NewLoad(irtypes.I8Ptr, elemAlloca)
		c.block.NewCall(c.funcs["promise_string_drop"], oldVal)
		c.block.NewBr(loadBlk)

		c.block = loadBlk
	}

	dataBase := c.block.NewGetElementPtr(irtypes.I8, slicePtr,
		constant.NewInt(irtypes.I64, int64(vectorHeaderSize)))
	dataTypedPtr := c.block.NewBitCast(dataBase, irtypes.NewPointer(elemLLVM))
	curCounter := c.block.NewLoad(irtypes.I64, counterAlloca)
	elemPtr := c.block.NewGetElementPtr(elemLLVM, dataTypedPtr, curCounter)
	if isTaskElem {
		c.block.NewStore(elemPtr, slotPtrAlloca) // T0617
	}
	var elemVal value.Value = c.block.NewLoad(elemLLVM, elemPtr)

	if dupStrings {
		elemVal = c.dupString(elemVal)
		c.block.NewStore(elemVal, elemAlloca)
		c.block.NewStore(constant.NewInt(irtypes.I1, 1), c.dropFlags[s.Binding])
	} else {
		c.block.NewStore(elemVal, elemAlloca)
	}

	c.genBlock(s.Body)
	if c.block.Term == nil {
		c.block.NewBr(updateBlock)
	}

	// Update: increment counter (and index if present)
	c.block = updateBlock
	cur := c.block.NewLoad(irtypes.I64, counterAlloca)
	next := c.block.NewAdd(cur, constant.NewInt(irtypes.I64, 1))
	c.block.NewStore(next, counterAlloca)

	if s.Index != "" {
		idxAlloca := c.locals[s.Index]
		curIdx := c.block.NewLoad(irtypes.I64, idxAlloca)
		nextIdx := c.block.NewAdd(curIdx, constant.NewInt(irtypes.I64, 1))
		c.block.NewStore(nextIdx, idxAlloca)
	}

	c.emitYieldCheck()
	c.block.NewBr(headerBlock)

	c.breakTarget = savedBreak
	c.continueTarget = savedContinue
	c.loopScopeDepth = savedLoopUseDepth
	c.loopTempFloor = savedLoopTempFloor // T1331
	c.block = exitBlock

	if isTaskElem { // T0617: restore the scoped slot-ptr map entry
		if hadPrevSlot {
			c.forInHandleSlotPtr[s.Binding] = prevSlot
		} else {
			delete(c.forInHandleSlotPtr, s.Binding)
		}
	}
}

// --- For-in over fixed-size arrays ---

// genForInArray iterates a fixed-size array with a compile-time-known length.
func (c *Compiler) genForInArray(s *ast.ForInStmt, arr *types.Array) {
	basePtr := c.genArrayBasePtr(s.Iterable, arr)
	elemLLVM := c.resolveType(arr.Elem())
	arrType := irtypes.NewArray(uint64(arr.Size()), elemLLVM)
	length := constant.NewInt(irtypes.I64, arr.Size())

	// Counter alloca
	counterAlloca := c.createEntryAlloca(irtypes.I64)
	c.block.NewStore(constant.NewInt(irtypes.I64, 0), counterAlloca)

	// Element binding alloca
	elemAlloca := c.createEntryAlloca(elemLLVM)
	elemAlloca.SetName(c.uniqueLocalName(s.Binding))
	c.locals[s.Binding] = elemAlloca

	// B0279: For string elements, register a drop binding so dup'd strings are
	// freed when the loop variable is not moved. The flag starts at 0 (no value
	// to drop before the first iteration).
	dupStrings := s.Binding != "_" && extractNamed(arr.Elem()) == types.TypString
	if dupStrings {
		c.maybeRegisterDrop(s.Binding, elemAlloca, arr.Elem())
		c.block.NewStore(constant.NewInt(irtypes.I1, 0), c.dropFlags[s.Binding])
	}

	// T0617: for a Task-element loop, record the current iteration's slot
	// address so `<-handle` (genReceiveTask) can null the consumed slot —
	// otherwise the array's scope-exit element drop reloads the freed G and
	// Task[T].drop double-frees → segfault. Per-iteration (not whole array)
	// so un-awaited slots are still dropped once (T0503).
	// T1434: IsAnyTask covers failable_task[T] too — see genForInVector.
	isTaskElem := s.Binding != "_" && types.IsAnyTask(arr.Elem())
	var slotPtrAlloca *ir.InstAlloca
	if isTaskElem {
		slotPtrAlloca = c.createEntryAlloca(irtypes.NewPointer(elemLLVM))
	}

	// Index variable if present
	if s.Index != "" {
		indexAlloca := c.createEntryAlloca(irtypes.I64)
		indexAlloca.SetName(c.uniqueLocalName(s.Index))
		c.block.NewStore(constant.NewInt(irtypes.I64, 0), indexAlloca)
		c.locals[s.Index] = indexAlloca
	}

	headerBlock := c.newBlock("forin.header")
	bodyBlock := c.newBlock("forin.body")
	updateBlock := c.newBlock("forin.update")
	exitBlock := c.newBlock("forin.exit")

	c.block.NewBr(headerBlock)

	// Header: counter < length
	c.block = headerBlock
	counter := c.block.NewLoad(irtypes.I64, counterAlloca)
	cond := c.block.NewICmp(enum.IPredULT, counter, length)
	c.block.NewCondBr(cond, bodyBlock, exitBlock)

	// Body: load element, store to binding
	savedBreak := c.breakTarget
	savedContinue := c.continueTarget
	savedLoopUseDepth := c.loopScopeDepth
	c.breakTarget = exitBlock
	c.continueTarget = updateBlock
	c.loopScopeDepth = len(c.scopeBindings)
	savedLoopTempFloor := c.enterLoopTempFloor() // T1331

	// T0617: scope the slot-ptr map entry to this loop (save/restore, like
	// breakTarget) so nesting is safe and it self-cleans across functions.
	var prevSlot *ir.InstAlloca
	var hadPrevSlot bool
	if isTaskElem {
		prevSlot, hadPrevSlot = c.forInHandleSlotPtr[s.Binding]
		c.forInHandleSlotPtr[s.Binding] = slotPtrAlloca
	}

	c.block = bodyBlock

	// B0279: Drop previous iteration's dup'd string if not moved, then dup new.
	if dupStrings {
		dropFlag := c.dropFlags[s.Binding]
		flag := c.block.NewLoad(irtypes.I1, dropFlag)
		dropBlk := c.newBlock("forin.str.drop")
		loadBlk := c.newBlock("forin.str.load")
		c.block.NewCondBr(flag, dropBlk, loadBlk)

		c.block = dropBlk
		oldVal := c.block.NewLoad(irtypes.I8Ptr, elemAlloca)
		c.block.NewCall(c.funcs["promise_string_drop"], oldVal)
		c.block.NewBr(loadBlk)

		c.block = loadBlk
	}

	curCounter := c.block.NewLoad(irtypes.I64, counterAlloca)
	elemPtr := c.block.NewGetElementPtr(arrType, basePtr,
		constant.NewInt(irtypes.I32, 0), curCounter)
	if isTaskElem {
		c.block.NewStore(elemPtr, slotPtrAlloca) // T0617
	}
	var elem value.Value = c.block.NewLoad(elemLLVM, elemPtr)

	if dupStrings {
		elem = c.dupString(elem)
		c.block.NewStore(elem, elemAlloca)
		c.block.NewStore(constant.NewInt(irtypes.I1, 1), c.dropFlags[s.Binding])
	} else {
		c.block.NewStore(elem, elemAlloca)
	}

	c.genBlock(s.Body)
	if c.block.Term == nil {
		c.block.NewBr(updateBlock)
	}

	// Update: increment counter (and index if present)
	c.block = updateBlock
	cur := c.block.NewLoad(irtypes.I64, counterAlloca)
	next := c.block.NewAdd(cur, constant.NewInt(irtypes.I64, 1))
	c.block.NewStore(next, counterAlloca)

	if s.Index != "" {
		idxAlloca := c.locals[s.Index]
		curIdx := c.block.NewLoad(irtypes.I64, idxAlloca)
		nextIdx := c.block.NewAdd(curIdx, constant.NewInt(irtypes.I64, 1))
		c.block.NewStore(nextIdx, idxAlloca)
	}

	c.emitYieldCheck()
	c.block.NewBr(headerBlock)

	c.breakTarget = savedBreak
	c.continueTarget = savedContinue
	c.loopScopeDepth = savedLoopUseDepth
	c.loopTempFloor = savedLoopTempFloor // T1331
	c.block = exitBlock

	if isTaskElem { // T0617: restore the scoped slot-ptr map entry
		if hadPrevSlot {
			c.forInHandleSlotPtr[s.Binding] = prevSlot
		} else {
			delete(c.forInHandleSlotPtr, s.Binding)
		}
	}
}

// --- For-in over channels ---

// genForInChannel loops receiving from a channel until it returns none (closed+empty).
// for v in ch { ... }  ≡  loop { val := <-ch; if val is none: break; v := unwrap(val); ... }
func (c *Compiler) genForInChannel(s *ast.ForInStmt, chRaw value.Value, elemType types.Type) {
	elemLLVM := c.resolveType(elemType)
	elemSize := int64(c.typeSize(elemLLVM))

	chanType := channelStructType()
	chPtr := c.block.NewBitCast(chRaw, irtypes.NewPointer(chanType))

	// Element binding alloca
	elemAlloca := c.createEntryAlloca(elemLLVM)
	elemAlloca.SetName(c.uniqueLocalName(s.Binding))
	c.locals[s.Binding] = elemAlloca

	headerBlock := c.newBlock("forin_ch.header")
	recvWaitBlock := c.newBlock("forin_ch.recv.wait")
	recvCheckBlock := c.newBlock("forin_ch.recv.check")
	recvNoneBlock := c.newBlock("forin_ch.recv.none")
	recvReadBlock := c.newBlock("forin_ch.recv.read")
	bodyBlock := c.newBlock("forin_ch.body")
	exitBlock := c.newBlock("forin_ch.exit")

	c.block.NewBr(headerBlock)

	// header: lock mutex, then enter receive wait loop
	c.block = headerBlock
	mtxFieldPtr := c.block.NewGetElementPtr(chanType, chPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(chanFieldMutex)))
	mtx := c.block.NewLoad(irtypes.I8Ptr, mtxFieldPtr)
	neFieldPtr := c.block.NewGetElementPtr(chanType, chPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(chanFieldNotEmpty)))
	notEmpty := c.block.NewLoad(irtypes.I8Ptr, neFieldPtr)
	nfFieldPtr := c.block.NewGetElementPtr(chanType, chPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(chanFieldNotFull)))
	notFull := c.block.NewLoad(irtypes.I8Ptr, nfFieldPtr)

	c.block.NewCall(c.palMutexLock, mtx)
	c.block.NewBr(recvWaitBlock)

	// recv.wait: while count==0 && !closed → wait
	c.block = recvWaitBlock
	countPtr := c.block.NewGetElementPtr(chanType, chPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(chanFieldCount)))
	count := c.block.NewLoad(irtypes.I64, countPtr)
	isEmpty := c.block.NewICmp(enum.IPredEQ, count, constant.NewInt(irtypes.I64, 0))
	closedPtr := c.block.NewGetElementPtr(chanType, chPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(chanFieldClosed)))
	closedVal := c.block.NewLoad(irtypes.I8, closedPtr)
	isOpen := c.block.NewICmp(enum.IPredEQ, closedVal, constant.NewInt(irtypes.I8, 0))
	shouldWait := c.block.NewAnd(isEmpty, isOpen)

	recvWaitBodyBlock := c.newBlock("forin_ch.recv.wait.body")
	c.block.NewCondBr(shouldWait, recvWaitBodyBlock, recvCheckBlock)

	if c.inCoroutine {
		// Goroutine mode: park on recv_waiters + coro.suspend
		c.block = recvWaitBodyBlock
		currentG := c.block.NewLoad(irtypes.I8Ptr, c.currentGGlobal)
		recvHeadPtr := c.block.NewGetElementPtr(chanType, chPtr,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(chanFieldRecvWaitersHead)))
		recvTailPtr := c.block.NewGetElementPtr(chanType, chPtr,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(chanFieldRecvWaitersTail)))
		c.block.NewCall(c.funcs["promise_waiter_enqueue"], recvHeadPtr, recvTailPtr, currentG)
		// Store mutex in G.park_mutex — scheduler releases after coro.suspend completes
		gTyForIn := goroutineStructType()
		forInGPtr := c.block.NewBitCast(currentG, irtypes.NewPointer(gTyForIn))
		forInPmField := c.block.NewGetElementPtr(gTyForIn, forInGPtr,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(gFieldParkMutex)))
		c.block.NewStore(mtx, forInPmField)

		suspResult := c.block.NewCall(c.coroSuspend, constant.None, constant.False)
		resumeBlk := c.newBlock("forin_ch.recv.resume")
		c.block.NewSwitch(suspResult, c.coroSuspendBlk,
			ir.NewCase(constant.NewInt(irtypes.I8, 0), resumeBlk),
			ir.NewCase(constant.NewInt(irtypes.I8, 1), c.coroCleanupBlk))

		c.block = resumeBlk
		c.block.NewCall(c.palMutexLock, mtx)
		c.block.NewBr(recvWaitBlock)
	} else if c.isWasm {
		// T1200: pump the cooperative scheduler instead of the no-op cond_wait so a
		// non-coroutine `for v in channel` receiver yields to its sender; on progress
		// recheck emptiness. Same fix as the `<-c` receive in genReceiveChannel.
		c.block = recvWaitBodyBlock
		c.emitWasmCoopWaitPump(recvWaitBlock)
	} else {
		// Thread-blocking mode: cond_wait, loop
		c.block = recvWaitBodyBlock
		c.block.NewCall(c.palCondWait, notEmpty, mtx)
		c.block.NewBr(recvWaitBlock)
	}

	// recv.check: if empty → exit (channel closed), else → read
	c.block = recvCheckBlock
	countAgain := c.block.NewLoad(irtypes.I64, countPtr)
	stillEmpty := c.block.NewICmp(enum.IPredEQ, countAgain, constant.NewInt(irtypes.I64, 0))
	c.block.NewCondBr(stillEmpty, recvNoneBlock, recvReadBlock)

	// recv.none: unlock and exit loop
	c.block = recvNoneBlock
	c.block.NewCall(c.palMutexUnlock, mtx)
	c.block.NewBr(exitBlock)

	// recv.read: read value from buffer, advance head, count--, wake sender, unlock, enter body
	c.block = recvReadBlock
	bufPtr := c.block.NewGetElementPtr(chanType, chPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(chanFieldBuffer)))
	buf := c.block.NewLoad(irtypes.I8Ptr, bufPtr)
	headPtr := c.block.NewGetElementPtr(chanType, chPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(chanFieldHead)))
	head := c.block.NewLoad(irtypes.I64, headPtr)
	offset := c.block.NewMul(head, constant.NewInt(irtypes.I64, elemSize))
	src := c.block.NewGetElementPtr(irtypes.I8, buf, offset)

	resultAsI8 := c.block.NewBitCast(elemAlloca, irtypes.I8Ptr)
	c.block.NewCall(c.funcs["llvm.memcpy"], resultAsI8, src,
		constant.NewInt(irtypes.I64, elemSize), constant.False)

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

	// Wake a rendezvous-parked sender (T0312): count is now decremented, so an
	// unbuffered sender's rendezvous wait may be complete. genReceiveChannel wakes
	// rv_waiters here too; `for v in ch` omitted it, so a `go`-spawned coroutine
	// sender over an unbuffered channel parked on rv_waiters was never woken and
	// deadlocked (host and WASM). Mirror the `<-c` wake to fix it.
	rvWakeHeadPtr := c.block.NewGetElementPtr(chanType, chPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(chanFieldRvWaitersHead)))
	rvWakeTailPtr := c.block.NewGetElementPtr(chanType, chPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(chanFieldRvWaitersTail)))
	c.block.NewCall(c.funcs["promise_waiter_wake_one"], rvWakeHeadPtr, rvWakeTailPtr, notFull)

	// Unlock
	c.block.NewCall(c.palMutexUnlock, mtx)

	// Fall into body
	c.block.NewBr(bodyBlock)

	// body: execute loop body
	savedBreak := c.breakTarget
	savedContinue := c.continueTarget
	savedLoopUseDepth := c.loopScopeDepth
	c.breakTarget = exitBlock
	c.continueTarget = headerBlock
	c.loopScopeDepth = len(c.scopeBindings)
	savedLoopTempFloor := c.enterLoopTempFloor() // T1331

	c.block = bodyBlock
	// T0671: the received element is moved out of the ring buffer (recv advanced
	// head / decremented count), so the loop variable owns it. Register a drop
	// binding at loopScopeDepth so a heap element (string/Vector/user/Arc/Optional…)
	// is dropped on every exit: normal iteration end (below), break/continue
	// (genBreak/ContinueStmt → emitScopeCleanup(loopScopeDepth)), and early
	// return/raise (function unwind). maybeRegisterDrop no-ops for value elems
	// (Channel[int] unchanged). Disjoint from T0663's Channel.drop, which only
	// walks still-buffered [head,head+count) items (received items already left it).
	bodyScopeStart := len(c.scopeBindings) // == c.loopScopeDepth
	c.maybeRegisterDrop(s.Binding, elemAlloca, elemType)
	c.genBlock(s.Body)
	if c.block != nil && c.block.Term == nil {
		if len(c.scopeBindings) > bodyScopeStart {
			cap := c.emitScopeCleanup(bodyScopeStart, false)
			c.emitCloseErrCheck(cap, bodyScopeStart)
		}
		c.emitYieldCheck()
		c.block.NewBr(headerBlock)
	}
	c.scopeBindings = c.scopeBindings[:bodyScopeStart] // unconditional codegen-time pop

	c.breakTarget = savedBreak
	c.continueTarget = savedContinue
	c.loopScopeDepth = savedLoopUseDepth
	c.loopTempFloor = savedLoopTempFloor // T1331
	c.block = exitBlock
}

// --- For-in over maps ---

// genForInMap iterates a Promise-implemented map by calling keys() and values()
// to produce vectors, then looping over them in parallel.
func (c *Compiler) genForInMap(s *ast.ForInStmt, mapVal value.Value, keyType, valType types.Type) {
	keyLLVM := c.resolveType(keyType)
	valLLVM := c.resolveType(valType)

	// Resolve monomorphized type name for method lookup. T0971: unwrap a borrowed
	// map (map[K,V]&) so the underlying Instance resolves.
	iterType := c.forInIterableType(s)
	inst, ok := iterType.(*types.Instance)
	if !ok {
		panic(fmt.Sprintf("codegen: for-in map target is %T, want Instance", iterType))
	}
	name := monoName(inst)

	// Call keys() and values() methods
	keysFnName := mangleMethodName(name, "keys", false)
	keysFn := c.funcs[keysFnName]
	valuesFnName := mangleMethodName(name, "values", false)
	valuesFn := c.funcs[valuesFnName]
	if keysFn == nil || valuesFn == nil {
		panic(fmt.Sprintf("codegen: undeclared map keys/values method for %s", name))
	}

	instancePtr := c.extractInstancePtr(mapVal)
	keysVec := c.block.NewCall(keysFn, instancePtr)
	valsVec := c.block.NewCall(valuesFn, instancePtr)

	// Get length from keys vector
	headerType := vectorHeaderType()
	headerPtr := c.block.NewBitCast(keysVec, irtypes.NewPointer(headerType))
	length := loadVectorLen(c.block, headerPtr)

	// Counter alloca
	counterAlloca := c.createEntryAlloca(irtypes.I64)
	c.block.NewStore(constant.NewInt(irtypes.I64, 0), counterAlloca)

	twoBindings := s.Index != "" // for k, v in map

	// B0343: Determine which bindings need string dup to prevent double-free.
	// keys()/values() return vectors with cloned strings. Without dup, the
	// iteration variable shares the heap pointer with the vector element.
	// emitVectorElementDropLoop would double-free strings that were moved.
	isKeyStr := extractNamed(keyType) == types.TypString
	isValStr := extractNamed(valType) == types.TypString
	var dupKeyStr, dupValStr bool
	var keyDropName, valDropName string
	var keyStrAlloca, valStrAlloca *ir.InstAlloca

	if twoBindings {
		// Separate key and value allocas
		keyAlloca := c.createEntryAlloca(keyLLVM)
		keyAlloca.SetName(c.uniqueLocalName(s.Index))
		c.locals[s.Index] = keyAlloca

		valAlloca := c.createEntryAlloca(valLLVM)
		valAlloca.SetName(c.uniqueLocalName(s.Binding))
		c.locals[s.Binding] = valAlloca

		// B0343: Register drop bindings for string keys/values.
		dupKeyStr = s.Index != "_" && isKeyStr
		dupValStr = s.Binding != "_" && isValStr
		if dupKeyStr {
			keyDropName = s.Index
			keyStrAlloca = keyAlloca
			c.maybeRegisterDrop(keyDropName, keyStrAlloca, keyType)
			c.block.NewStore(constant.NewInt(irtypes.I1, 0), c.dropFlags[keyDropName])
		}
		if dupValStr {
			valDropName = s.Binding
			valStrAlloca = valAlloca
			c.maybeRegisterDrop(valDropName, valStrAlloca, valType)
			c.block.NewStore(constant.NewInt(irtypes.I1, 0), c.dropFlags[valDropName])
		}
	} else {
		// Single binding: (K, V) tuple
		tupleType := irtypes.NewStruct(keyLLVM, valLLVM)
		bindingAlloca := c.createEntryAlloca(tupleType)
		bindingAlloca.SetName(c.uniqueLocalName(s.Binding))
		c.locals[s.Binding] = bindingAlloca

		// B0343: Hidden allocas for string lifecycle tracking in single-binding case.
		dupKeyStr = isKeyStr
		dupValStr = isValStr
		if dupKeyStr {
			keyDropName = "__forin_key"
			keyStrAlloca = c.createEntryAlloca(keyLLVM)
			c.maybeRegisterDrop(keyDropName, keyStrAlloca, keyType)
			c.block.NewStore(constant.NewInt(irtypes.I1, 0), c.dropFlags[keyDropName])
		}
		if dupValStr {
			valDropName = "__forin_val"
			valStrAlloca = c.createEntryAlloca(valLLVM)
			c.maybeRegisterDrop(valDropName, valStrAlloca, valType)
			c.block.NewStore(constant.NewInt(irtypes.I1, 0), c.dropFlags[valDropName])
		}
	}

	headerBlock := c.newBlock("forin.header")
	bodyBlock := c.newBlock("forin.body")
	updateBlock := c.newBlock("forin.update")
	exitBlock := c.newBlock("forin.exit")

	c.block.NewBr(headerBlock)

	// Header: compare counter < length
	c.block = headerBlock
	counter := c.block.NewLoad(irtypes.I64, counterAlloca)
	cond := c.block.NewICmp(enum.IPredULT, counter, length)
	c.block.NewCondBr(cond, bodyBlock, exitBlock)

	// Body: load key[i] and value[i], build tuple
	savedBreak := c.breakTarget
	savedContinue := c.continueTarget
	savedLoopUseDepth := c.loopScopeDepth
	c.breakTarget = exitBlock
	c.continueTarget = updateBlock
	c.loopScopeDepth = len(c.scopeBindings)
	savedLoopTempFloor := c.enterLoopTempFloor() // T1331

	c.block = bodyBlock

	// B0343: Drop previous iteration's dup'd strings if not moved.
	if dupKeyStr {
		dropFlag := c.dropFlags[keyDropName]
		flag := c.block.NewLoad(irtypes.I1, dropFlag)
		dropBlk := c.newBlock("forin.key.drop")
		afterBlk := c.newBlock("forin.key.after")
		c.block.NewCondBr(flag, dropBlk, afterBlk)
		c.block = dropBlk
		oldKey := c.block.NewLoad(irtypes.I8Ptr, keyStrAlloca)
		c.block.NewCall(c.funcs["promise_string_drop"], oldKey)
		c.block.NewBr(afterBlk)
		c.block = afterBlk
	}
	if dupValStr {
		dropFlag := c.dropFlags[valDropName]
		flag := c.block.NewLoad(irtypes.I1, dropFlag)
		dropBlk := c.newBlock("forin.val.drop")
		afterBlk := c.newBlock("forin.val.after")
		c.block.NewCondBr(flag, dropBlk, afterBlk)
		c.block = dropBlk
		oldVal := c.block.NewLoad(irtypes.I8Ptr, valStrAlloca)
		c.block.NewCall(c.funcs["promise_string_drop"], oldVal)
		c.block.NewBr(afterBlk)
		c.block = afterBlk
	}

	idx := c.block.NewLoad(irtypes.I64, counterAlloca)

	// Load key from keys vector
	keyDataBase := c.block.NewGetElementPtr(irtypes.I8, keysVec,
		constant.NewInt(irtypes.I64, int64(vectorHeaderSize)))
	keyDataPtr := c.block.NewBitCast(keyDataBase, irtypes.NewPointer(keyLLVM))
	keyElemPtr := c.block.NewGetElementPtr(keyLLVM, keyDataPtr, idx)
	var key value.Value = c.block.NewLoad(keyLLVM, keyElemPtr)

	// Load value from values vector
	valDataBase := c.block.NewGetElementPtr(irtypes.I8, valsVec,
		constant.NewInt(irtypes.I64, int64(vectorHeaderSize)))
	valDataPtr := c.block.NewBitCast(valDataBase, irtypes.NewPointer(valLLVM))
	valElemPtr := c.block.NewGetElementPtr(valLLVM, valDataPtr, idx)
	var val value.Value = c.block.NewLoad(valLLVM, valElemPtr)

	// B0343: Dup strings for independent ownership.
	if dupKeyStr {
		key = c.dupString(key)
	}
	if dupValStr {
		val = c.dupString(val)
	}

	if twoBindings {
		// Store key and value to separate allocas
		c.block.NewStore(key, c.locals[s.Index])
		c.block.NewStore(val, c.locals[s.Binding])
	} else {
		// Build and store (K, V) tuple
		tupleType := irtypes.NewStruct(keyLLVM, valLLVM)
		var tuple value.Value = constant.NewZeroInitializer(tupleType)
		tuple = c.block.NewInsertValue(tuple, key, 0)
		tuple = c.block.NewInsertValue(tuple, val, 1)
		c.block.NewStore(tuple, c.locals[s.Binding])
		// B0343: Store to hidden tracking allocas.
		if dupKeyStr {
			c.block.NewStore(key, keyStrAlloca)
		}
		if dupValStr {
			c.block.NewStore(val, valStrAlloca)
		}
	}

	// B0343: Set drop flags for dup'd strings.
	if dupKeyStr {
		c.block.NewStore(constant.NewInt(irtypes.I1, 1), c.dropFlags[keyDropName])
	}
	if dupValStr {
		c.block.NewStore(constant.NewInt(irtypes.I1, 1), c.dropFlags[valDropName])
	}

	c.genBlock(s.Body)
	if c.block.Term == nil {
		c.block.NewBr(updateBlock)
	}

	// Update: increment counter
	c.block = updateBlock
	curCount := c.block.NewLoad(irtypes.I64, counterAlloca)
	nextCount := c.block.NewAdd(curCount, constant.NewInt(irtypes.I64, 1))
	c.block.NewStore(nextCount, counterAlloca)
	c.emitYieldCheck()
	c.block.NewBr(headerBlock)

	c.breakTarget = savedBreak
	c.continueTarget = savedContinue
	c.loopScopeDepth = savedLoopUseDepth
	c.loopTempFloor = savedLoopTempFloor // T1331
	c.block = exitBlock

	// B0214: Drop the temporary keys and values vectors after the loop.
	// keys() and values() return freshly heap-allocated vectors that must be freed.
	// B0244: values() match-destructures Slot.Used(_, v), which deep-clones all
	// droppable values (strings dup'd, enums cloned, heap types cloned). The values
	// vector contains independent copies, so all element types must be dropped.
	vectorDropFn := c.funcs["Vector.drop"]
	c.emitVectorElementDropLoop(keysVec, keyType)
	c.block.NewCall(vectorDropFn, keysVec)
	c.emitVectorElementDropLoop(valsVec, valType)
	c.block.NewCall(vectorDropFn, valsVec)
}

// --- For-in over strings ---

func (c *Compiler) genForInString(s *ast.ForInStmt, strPtr value.Value) {
	// Alloca for byte position
	posAlloca := c.createEntryAlloca(irtypes.I64)
	c.block.NewStore(constant.NewInt(irtypes.I64, 0), posAlloca)

	// Index variable if present
	if s.Index != "" {
		indexAlloca := c.createEntryAlloca(irtypes.I64)
		indexAlloca.SetName(c.uniqueLocalName(s.Index))
		c.block.NewStore(constant.NewInt(irtypes.I64, 0), indexAlloca)
		c.locals[s.Index] = indexAlloca
	}

	headerBlock := c.newBlock("forin.str.header")
	bodyBlock := c.newBlock("forin.str.body")
	exitBlock := c.newBlock("forin.str.exit")

	c.block.NewBr(headerBlock)

	// Header: call promise_string_next_char, check for -1
	c.block = headerBlock
	cp := c.block.NewCall(c.funcs["promise_string_next_char"], strPtr, posAlloca)
	done := c.block.NewICmp(enum.IPredEQ, cp, constant.NewInt(irtypes.I32, -1))
	c.block.NewCondBr(done, exitBlock, bodyBlock)

	// Body: bind char to loop variable
	savedBreak := c.breakTarget
	savedContinue := c.continueTarget
	savedLoopUseDepth := c.loopScopeDepth
	c.breakTarget = exitBlock
	c.continueTarget = headerBlock
	c.loopScopeDepth = len(c.scopeBindings)
	savedLoopTempFloor := c.enterLoopTempFloor() // T1331

	c.block = bodyBlock
	alloca := c.createEntryAlloca(irtypes.I32)
	alloca.SetName(c.uniqueLocalName(s.Binding))
	c.block.NewStore(cp, alloca)
	c.locals[s.Binding] = alloca

	c.genBlock(s.Body)

	// Increment index after body, before looping back
	if s.Index != "" && c.block.Term == nil {
		idxAlloca := c.locals[s.Index]
		curIdx := c.block.NewLoad(irtypes.I64, idxAlloca)
		nextIdx := c.block.NewAdd(curIdx, constant.NewInt(irtypes.I64, 1))
		c.block.NewStore(nextIdx, idxAlloca)
	}

	if c.block.Term == nil {
		c.emitYieldCheck()
		c.block.NewBr(headerBlock)
	}

	c.breakTarget = savedBreak
	c.continueTarget = savedContinue
	c.loopScopeDepth = savedLoopUseDepth
	c.loopTempFloor = savedLoopTempFloor // T1331
	c.block = exitBlock
}
