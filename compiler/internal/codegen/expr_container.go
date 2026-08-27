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

// genVectorLen loads the length from a vector/array header (masking off bit 63 static flag).
func (c *Compiler) genVectorLen(e *ast.MemberExpr) value.Value {
	slicePtr := c.genExprAutoPropagate(e.Target) // B0323
	headerType := vectorHeaderType()
	headerPtr := c.block.NewBitCast(slicePtr, irtypes.NewPointer(headerType))
	return loadVectorLen(c.block, headerPtr)
}

// genMapLen returns the length of a map via the runtime.
// foldWideToI64 folds a wide integer value (width a multiple of 64) down to a
// single i64 by XOR-ing its 64-bit limbs, so wide types can feed the uniform
// i64 hash interface (§3.5 of docs/large-integers.md).
func (c *Compiler) foldWideToI64(val value.Value, width uint64) value.Value {
	wide := val.Type().(*irtypes.IntType)
	// Lowest limb.
	var acc value.Value = c.block.NewTrunc(val, irtypes.I64)
	for shift := uint64(64); shift < width; shift += 64 {
		shifted := c.block.NewLShr(val, constant.NewInt(wide, int64(shift)))
		limb := c.block.NewTrunc(shifted, irtypes.I64)
		acc = c.block.NewXor(acc, limb)
	}
	return acc
}

func (c *Compiler) genVectorMethodCall(e *ast.CallExpr, member *ast.MemberExpr, elemType types.Type, method string) value.Value {
	// T0595/T1064: evaluate the receiver once into a vectorReceiver. For an
	// arr[i]/vov[i] receiver it captures the once-evaluated index (not an early
	// element slot); the push/pop/remove store-back recomputes the slot from a fresh
	// outer pointer, so an argument that reallocates the outer vector during argument
	// evaluation can't leave the write-back slot dangling. Held in a local (not a
	// Compiler field) so a nested vector method call during arg eval can't clobber it.
	rcv := c.evalVectorReceiver(member.Target)
	slicePtr := rcv.slicePtr
	elemLLVM := c.resolveType(elemType)
	elemSize := int64(c.typeSize(elemLLVM))

	switch method {
	case "push":
		// T0658: Hoist resolvedElem (was computed redundantly further down)
		// so the Optional-wrap below and the existing droppable-element dup
		// logic share one resolution.
		resolvedElem := elemType
		if c.typeSubst != nil {
			resolvedElem = types.Substitute(resolvedElem, c.typeSubst)
		}
		_, elemIsOpt := resolvedElem.(*types.Optional)

		// T0658: Set targetType to the resolved Optional element type so a
		// bare `none` arg lowers to a zero {i1,T} struct via genNoneLit
		// (mirrors the genAssignStmt index-assign path, stmt.go:5309-5316).
		// The push path never set this, so `v.push(none)` into a T?[] used
		// to return the i1 0 "void optional fallback".
		savedTarget := c.targetType
		if elemIsOpt {
			c.targetType = resolvedElem
		}
		// T0388: When the source is a field read on a droppable owner
		// (e.g. v.push(h.arr) where arr is a Vector/Channel/Arc/Weak/
		// Optional[these] field), set dupContainerFieldAccess so
		// genFieldAccess produces an independent dup tracked as a heap
		// temp. claimHeapTemp below then transfers ownership to the
		// vector. Module/native getters and enum-variant access take
		// other paths in genMemberExpr and never reach genFieldAccess,
		// so the flag is only consumed for actual struct field reads —
		// getter results are not double-dup'd. String fields use a
		// separate flag (dupStringFieldAccess) and are handled by the
		// post-load c.dupString(argVal) below; setting the container
		// flag is harmless for string-element pushes.
		// (Not borrow-gated — checks MemberExpr AST shape on an owned type.
		// Remains active post-T0438.)
		if _, isMember := e.Args[0].Value.(*ast.MemberExpr); isMember {
			c.dupContainerFieldAccess = true
		}
		// T0741: track enum ctor temps created while evaluating the pushed
		// element. When the element is moved (not dup'd) into the vector, the
		// vector becomes the sole owner, so these temps must be cleared — else
		// the temp's stmt-end synth drop and the vector element drop both free
		// the variant data (e.g. a closure env in a Vector[enum-with-closure]
		// element → double-free). Mirrors genVectorLit / genFixedArrayLit.
		savedEnumTemps := len(c.enumCtorTemps)
		argVal := c.genCallArgExpr(e.Args[0].Value)
		c.dupContainerFieldAccess = false
		c.targetType = savedTarget

		// T1279: A pure value type / primitive / string pushed into a
		// Vector[structural-interface] must be view-boxed (heap box + {vtable, box}) —
		// the raw value struct / scalar doesn't fit the {i8*, i8*} element slot. The box
		// is registered as an owned heap temp; claimHeapTemp(argVal) below transfers
		// ownership to the vector, and Vector.drop's element drop frees it via
		// __promise_structural_drop (T1284). No-op when no boxing is needed.
		// Placed before the elemIsOpt wrap so a Vector[structural?] element
		// boxes-then-wraps (T1298).
		pushArgType := c.info.Types[e.Args[0].Value]
		if c.typeSubst != nil && pushArgType != nil {
			pushArgType = types.Substitute(pushArgType, c.typeSubst)
		}
		// Bare structural element: box directly (no-op for an Optional element,
		// since extractNamed doesn't peel Optional).
		argVal = c.coerceToView(argVal, pushArgType, resolvedElem)
		// T1298: Optional structural element (`Sink?[]`): box into the element view
		// before the elemIsOpt wrap below (no-op otherwise).
		argVal = c.coerceToOptionalElem(argVal, pushArgType, resolvedElem)

		// T0658: Wrap a bare RHS into the Optional element struct when the
		// vector element type is Optional but the pushed expr is not (e.g.
		// `int?[] v = []; v.push(1)`). This is the push-side analog of T0615
		// (genVectorIndexAssign). Without it the raw scalar/pointer is stored
		// straight into the {i1,T} slot → "store operands are not compatible".
		// Predicate mirrors stmt.go:5832-5851 exactly, including
		// claim-before-wrap for string/native-handle/container temps whose
		// stmtTempMap tracking is by val-identity (lost once val is wrapped).
		// Heap user-type temps are still claimed correctly post-wrap by
		// claimHeapTemp's struct-extraction fallback (B0233), so excluded.
		if elemIsOpt {
			argExprType := c.info.Types[e.Args[0].Value]
			if c.typeSubst != nil && argExprType != nil {
				argExprType = types.Substitute(argExprType, c.typeSubst)
			}
			if argExprType != types.TypNone && !types.Identical(argExprType, resolvedElem) {
				if extractNamed(argExprType) == types.TypString ||
					types.IsVector(argExprType) || types.IsChannel(argExprType) ||
					types.IsArc(argExprType) || types.IsWeak(argExprType) ||
					types.IsAnyTask(argExprType) || types.IsMutex(argExprType) ||
					types.IsMutexGuard(argExprType) {
					c.claimStringTemp(argVal)
				}
				if st, ok := elemLLVM.(*irtypes.StructType); ok {
					argVal = c.wrapOptional(argVal, st)
				}
			}
		}

		// B0189: For string elements, dup before push to ensure exclusive ownership.
		// Each vector must independently own its string elements so that the element
		// drop loop in Vector.drop doesn't cause double-frees when strings are shared
		// between vectors (e.g., normalize() where parts[i] is pushed into result).
		// B0302: Extended to all droppable element types (vectors, channels, heap user
		// types). Without duplication, pushing the same value multiple times (e.g., in
		// Vector.filled) creates aliased pointers — the element-level drop on the outer
		// vector frees the same data N times (double/triple-free). Duplication ensures
		// each element is independently owned, matching the B0189 string pattern.
		if extractNamed(resolvedElem) == types.TypString {
			argVal = c.dupString(argVal)
			// Don't clear source drop flag — source retains its string.
			// Don't claim string temp — original temp is freed normally by cleanup.
		} else {
			// B0302: When the source is an ident with a drop flag AND the element
			// type is droppable, use a runtime check: if the flag is still true this
			// is the first push (move semantics — clear flag). If false, the variable
			// was already consumed (e.g., in a prior loop iteration of Vector.filled)
			// — dup the element to avoid aliased pointers that cause double-free.
			// For idents WITHOUT a drop flag (function params), always dup droppable
			// types. For non-ident sources, see the T0376 branch below.
			dupped := false
			if ident, ok := e.Args[0].Value.(*ast.IdentExpr); ok {
				if flagAlloca, hasFlag := c.dropFlags[ident.Name]; hasFlag {
					if c.pushElemNeedsDup(resolvedElem) {
						// Runtime branch: flag=true → first use (move), flag=false → dup
						flag := c.block.NewLoad(irtypes.I1, flagAlloca)
						moveBlock := c.newBlock("push.move")
						dupBlock := c.newBlock("push.dup")
						mergeBlock := c.newBlock("push.merge")
						c.block.NewCondBr(flag, moveBlock, dupBlock)

						c.block = moveBlock
						moveEnd := c.block
						c.block.NewBr(mergeBlock)

						// Generate dup INSIDE the dup block so the allocation only
						// happens when the value was already consumed.
						c.block = dupBlock
						dupVal := c.maybeDupPushElement(argVal, resolvedElem)
						dupEnd := c.block
						c.block.NewBr(mergeBlock)

						c.block = mergeBlock
						argVal = c.block.NewPhi(
							ir.NewIncoming(argVal, moveEnd),
							ir.NewIncoming(dupVal, dupEnd),
						)
						dupped = true
					}
					c.clearDropFlag(ident.Name)
				} else {
					// No drop flag (function parameter): always dup droppable types.
					if dupVal := c.maybeDupPushElement(argVal, resolvedElem); dupVal != nil {
						argVal = dupVal
						dupped = true
					}
				}
			} else if _, isIndex := e.Args[0].Value.(*ast.IndexExpr); isIndex {
				// T0376: IndexExpr source (e.g. this[i] in Vector.[:], arr[k]).
				// Returns a load — an alias to the element at the given index.
				// Without dup, the new vector's slot would alias the source
				// container's element pointer and the cleanup walks (vector +
				// source) would double-free. Dup so the new vector owns an
				// independent copy. Symmetric with the IdentExpr-without-flag
				// (function param) path. CallExpr / MemberExpr / literal
				// sources are left alone — CallExpr returns fresh allocations
				// (constructors, getters) whose ownership the vector inherits,
				// and MemberExpr field-access dup is left for a follow-up
				// because some MemberExpr forms (module getters, instance
				// getters) also return fresh values and can't be safely
				// distinguished from field access at this layer.
				//
				// T0387: Polymorphic element types (those needing a vtable) are
				// no longer carved out. dupHeapValue → maybeDupPushElement →
				// cloneHeapElement falls through to dupHeapValue, which now
				// dispatches via typeinfo.clone_fn_ptr to the runtime concrete
				// type's clone fn — independent copy with full subtype data.
				if dupVal := c.maybeDupPushElement(argVal, resolvedElem); dupVal != nil {
					argVal = dupVal
					dupped = true
				}
			}
			if !dupped {
				// Non-ident or non-droppable: clear drop flag as before
				if ident, ok := e.Args[0].Value.(*ast.IdentExpr); ok {
					c.clearDropFlag(ident.Name)
				}
			}
			// B0170: claim string temp — ownership transfers to vector
			c.claimStringTemp(argVal)
			// B0233: claim heap temp — ownership transfers to vector
			c.claimHeapTemp(argVal)
			// T0741: claim closure env — ownership transfers to vector; the
			// vector's element-drop loop now frees each pushed closure's env.
			c.claimEnvTemp(argVal)
			// T0741: when moved (not dup'd) into the vector, clear enum ctor
			// temps created during arg eval so the temp's synth drop doesn't
			// also free the variant data the vector element now owns.
			// T1139: gate on the element's static type being an enum — a non-enum
			// element arg that merely BORROWS an inline Enum.V(x) temp in a
			// sub-call leaves an intermediate the vector never owns; it must stay
			// tracked so the caller drops it at statement end, else it leaks.
			if !dupped {
				argEnumType := c.info.Types[e.Args[0].Value]
				if c.typeSubst != nil {
					argEnumType = types.Substitute(argEnumType, c.typeSubst)
				}
				if extractEnum(argEnumType) != nil {
					for j := savedEnumTemps; j < len(c.enumCtorTemps); j++ {
						c.block.NewStore(constant.NewInt(irtypes.I1, 0), c.enumCtorTemps[j].dropFlag)
					}
					c.enumCtorTemps = c.enumCtorTemps[:savedEnumTemps]
				}
			}
		}
		// COW: if static (.rodata), copy to heap first (T0062)
		cowSlice := c.block.NewCall(c.funcs["promise_vector_cow"],
			slicePtr, constant.NewInt(irtypes.I64, elemSize))
		argAlloca := c.createEntryAlloca(elemLLVM)
		// Zero-initialize before store to clear padding bytes for memcmp correctness
		c.block.NewStore(constant.NewZeroInitializer(elemLLVM), argAlloca)
		// T0661: For Optional element types {i1, T}, use field stores to preserve
		// the zeroinit padding bytes (7 bytes between i1 and T for i64 alignment).
		// A full struct store of a value built by insertvalue-from-undef carries
		// undefined padding that overwrites zeroinit. Must mirror the contains-side
		// field stores so memcmp agrees in all cases (inline and cross-function).
		if elemIsOpt {
			if st, ok := elemLLVM.(*irtypes.StructType); ok {
				zero32 := constant.NewInt(irtypes.I32, 0)
				one32 := constant.NewInt(irtypes.I32, 1)
				i1Val := c.block.NewExtractValue(argVal, 0)
				innerVal := c.block.NewExtractValue(argVal, 1)
				f0Ptr := c.block.NewGetElementPtr(st, argAlloca, zero32, zero32)
				f1Ptr := c.block.NewGetElementPtr(st, argAlloca, zero32, one32)
				c.block.NewStore(i1Val, f0Ptr)
				c.block.NewStore(innerVal, f1Ptr)
			}
		} else {
			c.block.NewStore(argVal, argAlloca)
		}
		argPtr := c.block.NewBitCast(argAlloca, irtypes.I8Ptr)
		newSlice := c.block.NewCall(c.funcs["promise_vector_push"],
			cowSlice, argPtr, constant.NewInt(irtypes.I64, elemSize))
		// Store the (possibly reallocated) pointer back
		c.storeVectorReceiverBack(rcv, newSlice)
		return newSlice

	case "pop":
		// COW: if static (.rodata), copy to heap first (T0062)
		cowSlice := c.block.NewCall(c.funcs["promise_vector_cow"],
			slicePtr, constant.NewInt(irtypes.I64, elemSize))
		c.storeVectorReceiverBack(rcv, cowSlice)
		outAlloca := c.createEntryAlloca(elemLLVM)
		outPtr := c.block.NewBitCast(outAlloca, irtypes.I8Ptr)
		found := c.block.NewCall(c.funcs["promise_vector_pop"],
			cowSlice, outPtr, constant.NewInt(irtypes.I64, elemSize))
		// Build Optional: {i1, T}
		optType := irtypes.NewStruct(irtypes.I1, elemLLVM)
		isFound := c.block.NewTrunc(found, irtypes.I1)
		someBlock := c.newBlock("pop.some")
		noneBlock := c.newBlock("pop.none")
		mergeBlock := c.newBlock("pop.merge")
		c.block.NewCondBr(isFound, someBlock, noneBlock)

		c.block = someBlock
		val := c.block.NewLoad(elemLLVM, outAlloca)
		someOpt := c.wrapOptional(val, optType)
		c.block.NewBr(mergeBlock)
		someEnd := c.block

		c.block = noneBlock
		noneOpt := constant.NewZeroInitializer(optType)
		c.block.NewBr(mergeBlock)
		noneEnd := c.block

		c.block = mergeBlock
		phi := c.block.NewPhi(ir.NewIncoming(someOpt, someEnd), ir.NewIncoming(noneOpt, noneEnd))
		return phi

	case "contains":
		// T0661: Mirror the T0658 push-case Optional-wrapping for the contains
		// path. When the resolved element type is Optional and the argument is a
		// bare (non-optional) value, genCallArgExpr returns the raw scalar while
		// argAlloca is {i1,T}* — the store panics. Fix: resolve elemType, detect
		// Optional, set c.targetType so genNoneLit emits a zero {i1,T} struct for
		// `v.contains(none)`, then wrap a bare scalar via wrapOptional. contains is
		// read-only so no claimStringTemp/claimHeapTemp/enumCtorTemps dance needed.
		resolvedContainsElem := elemType
		if c.typeSubst != nil {
			resolvedContainsElem = types.Substitute(resolvedContainsElem, c.typeSubst)
		}
		_, containsElemIsOpt := resolvedContainsElem.(*types.Optional)

		savedContainsTarget := c.targetType
		if containsElemIsOpt {
			c.targetType = resolvedContainsElem
		}
		argVal := c.genCallArgExpr(e.Args[0].Value)
		c.targetType = savedContainsTarget

		if containsElemIsOpt {
			argExprType := c.info.Types[e.Args[0].Value]
			if c.typeSubst != nil && argExprType != nil {
				argExprType = types.Substitute(argExprType, c.typeSubst)
			}
			if argExprType != types.TypNone && !types.Identical(argExprType, resolvedContainsElem) {
				if st, ok := elemLLVM.(*irtypes.StructType); ok {
					argVal = c.wrapOptional(argVal, st)
				}
			}
		}

		argAlloca := c.createEntryAlloca(elemLLVM)
		// Zero-initialize first to clear ALL bytes including struct padding.
		c.block.NewStore(constant.NewZeroInitializer(elemLLVM), argAlloca)
		// T0661: For Optional element types {i1, T}, store each field individually
		// instead of using a full struct store. A full struct store of a value
		// produced by insertvalue-from-undef carries undefined padding bytes (the
		// 7 bytes between i1 and i64 for alignment) that overwrite the zeroinit.
		// When push and contains execute in different functions, their separate
		// LLVM optimization contexts may produce different undefined padding,
		// causing memcmp to report inequality even when the logical values match.
		// Field stores leave the zeroinit padding untouched, guaranteeing that
		// both the vector element (from push) and the search argument have
		// identical zero padding for all supported element types.
		if containsElemIsOpt {
			if st, ok := elemLLVM.(*irtypes.StructType); ok {
				zero32 := constant.NewInt(irtypes.I32, 0)
				one32 := constant.NewInt(irtypes.I32, 1)
				i1Val := c.block.NewExtractValue(argVal, 0)
				innerVal := c.block.NewExtractValue(argVal, 1)
				f0Ptr := c.block.NewGetElementPtr(st, argAlloca, zero32, zero32)
				f1Ptr := c.block.NewGetElementPtr(st, argAlloca, zero32, one32)
				c.block.NewStore(i1Val, f0Ptr)
				c.block.NewStore(innerVal, f1Ptr)
			}
		} else {
			c.block.NewStore(argVal, argAlloca)
		}
		argPtr := c.block.NewBitCast(argAlloca, irtypes.I8Ptr)
		// Select the element comparison function:
		// • Optional elements: field-by-field comparison (ignores padding bytes)
		// • string elements: content equality via __promise_eq_string
		// • all others: memcmp (eq_fn = null)
		var eqFn value.Value
		if containsElemIsOpt {
			// T0661: Use a custom Optional equality function that compares the i1
			// presence flag and inner scalar value directly, bypassing memcmp.
			// memcmp fails cross-function on WASM because O1 decomposes
			// `store zeroinitializer` into per-field stores, leaving the 7 padding
			// bytes between i1 and i64 uninitialized — different stack frames
			// produce different garbage in those bytes → false inequality.
			eqFn = c.getOrEmitOptContainsEqFn(elemLLVM)
		} else if extractNamed(elemType) == types.TypString {
			eqFn = c.block.NewBitCast(c.funcs["__promise_eq_string"], irtypes.I8Ptr)
		} else {
			eqFn = constant.NewNull(irtypes.I8Ptr)
		}
		result := c.block.NewCall(c.funcs["promise_vector_contains"],
			slicePtr, argPtr, constant.NewInt(irtypes.I64, elemSize), eqFn)
		return c.block.NewTrunc(result, irtypes.I1)

	case "clone":
		// Deep-copy the vector: shallow memcpy of header+elements, then deep-clone
		// non-copy elements so the cloned vector owns independent copies. B0275.
		resolvedElem := elemType
		if c.typeSubst != nil {
			resolvedElem = types.Substitute(resolvedElem, c.typeSubst)
		}
		result := c.dupVector(slicePtr, elemSize)
		c.emitVectorElementCloneLoop(result, resolvedElem)
		return result

	case "remove":
		idx := c.genCallArgExpr(e.Args[0].Value)
		// COW: if static (.rodata), copy to heap first (T0062)
		cowSlice := c.block.NewCall(c.funcs["promise_vector_cow"],
			slicePtr, constant.NewInt(irtypes.I64, elemSize))
		c.storeVectorReceiverBack(rcv, cowSlice)

		// B0189: Drop the element being removed if it's droppable (e.g., string).
		// The remove operation shifts subsequent elements, overwriting the removed one.
		resolvedElem := elemType
		if c.typeSubst != nil {
			resolvedElem = types.Substitute(resolvedElem, c.typeSubst)
		}
		if c.variantFieldNeedsDrop(resolvedElem) {
			dataBase := c.block.NewGetElementPtr(irtypes.I8, cowSlice,
				constant.NewInt(irtypes.I64, int64(vectorHeaderSize)))
			dataTypedPtr := c.block.NewBitCast(dataBase, irtypes.NewPointer(elemLLVM))
			removedPtr := c.block.NewGetElementPtr(elemLLVM, dataTypedPtr, idx)
			removedVal := c.block.NewLoad(elemLLVM, removedPtr)
			c.emitVariantFieldDrop(removedVal, resolvedElem)
		}

		c.block.NewCall(c.funcs["promise_vector_remove"],
			cowSlice, idx, constant.NewInt(irtypes.I64, elemSize))
		return nil

	default:
		panic(fmt.Sprintf("codegen: unknown vector method %s", method))
	}
}

// pushElemNeedsDup returns true if the element type is a non-string droppable type
// that would need duplication on push (to prevent aliased pointers). B0302.
func (c *Compiler) pushElemNeedsDup(resolvedElem types.Type) bool {
	// T0399: tuples with droppable fields need dup on push.
	if _, isTup := resolvedElem.(*types.Tuple); isTup {
		return c.tupleNeedsDrop(resolvedElem)
	}
	// T1174/T1183: Optional elements whose inner value aliases heap
	// (Optional[heap-user], Optional[string], Optional[Vector|Channel|Arc|Weak|
	// tuple|nested-Optional]) must be deep-cloned on push (kept in sync with
	// maybeDupPushElement's Optional branch). extractNamed does not see through
	// the Optional wrapper, so without this the container/enum/user checks below
	// all miss it.
	if _, _, ok := c.optionalPushElemNeedsDup(resolvedElem); ok {
		return true
	}
	named := extractNamed(resolvedElem)
	if named == nil {
		if en := extractEnum(resolvedElem); en != nil {
			if _, ok := c.funcs[c.enumCloneFuncName(en, resolvedElem)]; ok {
				return true
			}
			return c.vecElemNeedsEnumDrop(resolvedElem)
		}
		return false
	}
	if _, isVec := types.AsVector(resolvedElem); isVec || named == types.TypVector {
		return true
	}
	if _, isCh := types.AsChannel(resolvedElem); isCh || named == types.TypChannel {
		return true
	}
	// T0508: Arc/Weak need dup (ref-count increment); make explicit so the
	// catch-all below doesn't accidentally exclude them if IsValueType/IsCopy
	// flags change.
	if _, isArc := types.AsArc(resolvedElem); isArc || named == types.TypArc {
		return true
	}
	if _, isWeak := types.AsWeak(resolvedElem); isWeak || named == types.TypWeak {
		return true
	}
	// T0508: Mutex/MutexGuard/Task are single-owner native handles — no dup
	// semantics. Move-only push; the ownership system prevents reuse.
	if _, isMutex := types.AsMutex(resolvedElem); isMutex || named == types.TypMutex {
		return false
	}
	if _, isMG := types.AsMutexGuard(resolvedElem); isMG || named == types.TypMutexGuard {
		return false
	}
	if _, isTask := types.AsAnyTask(resolvedElem); isTask || types.IsTaskLikeOrigin(named) {
		return false
	}
	return !named.IsValueType() && !named.IsCopy() && !isPrimitiveScalar(named) && !named.IsStructural()
}

// maybeDupPushElement checks if a vector element type is a non-string droppable
// type that needs duplication on push. Returns the duplicated value, or nil if
// no duplication is needed (primitive/Copy/string types). B0302.
func (c *Compiler) maybeDupPushElement(argVal value.Value, resolvedElem types.Type) value.Value {
	// T0399: Tuples with droppable fields need a deep clone on push. Without
	// this, v2.push(v[i]) for Vector[(string, int)] aliases v's heap-string
	// pointers — both vectors' drop walks would free the same memory. Pure-value
	// tuples (no droppable fields) need no dup.
	if tup, isTup := resolvedElem.(*types.Tuple); isTup {
		if c.tupleNeedsDrop(resolvedElem) {
			return c.dupTupleValue(argVal, tup)
		}
		return nil
	}

	// T1045: a bare closure element (Vector[() -> int]) is a *types.Signature —
	// extractNamed/extractEnum are both nil, so without this it falls through to
	// the `return nil` below as if it were a primitive/Copy. But the vector's
	// element-drop loop frees each pushed closure's heap env, so a shallow copy
	// (e.g. result.push(this[i]) in Vector.[:]) aliases the env across both
	// vectors → double-free at drop. A closure env CANNOT be deep-cloned (the
	// captured frame is opaque); null the {fn,env} fat pointer so the source keeps
	// sole ownership and the clone holds an empty closure. Symmetric with
	// emitVariantFieldDup's Signature case and emitVectorClosureNullLoop.
	if _, isSig := resolvedElem.(*types.Signature); isSig {
		return constant.NewZeroInitializer(argVal.Type())
	}

	// T1174: Optional[heap-user-type] element — deep-clone the inner heap value so
	// each vector slot owns an independent copy. The named/enum branches below
	// don't see through the Optional wrapper (extractNamed returns nil), so a
	// pushed Optional[Row] would otherwise alias the source payload (a
	// match-borrowed `if b is Has(maybe)` binding pushed into a vector, or the
	// element read in a Vector[Row?] slice) and double-free when both the source
	// and the vector drop. dupHeapValue is null-safe (handles the `none` slot via
	// a phi) and dispatches through typeinfo clone_fn for polymorphic subtypes.
	if inner, ok := c.optionalHeapDupElem(resolvedElem); ok {
		innerVal := c.block.NewExtractValue(argVal, 1)
		dup := c.dupHeapValue(innerVal, inner)
		return c.block.NewInsertValue(argVal, dup, 1)
	}

	// T1183: Optional[string] / Optional[Vector|Channel|Arc|Weak|tuple|
	// nested-Optional] element — inner ALSO aliases heap. The optionalHeapDupElem
	// branch above only matches heap-user inners, so these fell through to the
	// `return nil` below and aliased the source payload (whole-array escape of
	// Optional[string][N], or Vector[Optional[string]].push). Deep-clone via
	// dupOptionalVectorElem (present/absent split + per-inner dispatch).
	if opt, inner, ok := c.optionalPushElemNeedsDup(resolvedElem); ok {
		return c.dupOptionalVectorElem(argVal, opt, inner)
	}

	named := extractNamed(resolvedElem)

	// Check for droppable enum types (B0244/B0290 pattern)
	if named == nil {
		if en := extractEnum(resolvedElem); en != nil {
			if _, ok := c.funcs[c.enumCloneFuncName(en, resolvedElem)]; ok {
				cloned, _ := c.cloneEnumValue(argVal, resolvedElem)
				return cloned
			}
			if c.vecElemNeedsEnumDrop(resolvedElem) {
				// Droppable enum without clone — dup variant fields via alloca round-trip
				alloca := c.createEntryAlloca(argVal.Type())
				c.block.NewStore(argVal, alloca)
				c.dupEnumElementInPlace(alloca, resolvedElem)
				return c.block.NewLoad(argVal.Type(), alloca)
			}
		}
		return nil // primitive/Copy
	}

	// Vector element: shallow dup + recursive element clone
	if innerElem, isVec := types.AsVector(resolvedElem); isVec {
		innerLLVM := c.resolveType(innerElem)
		innerSize := int64(c.typeSize(innerLLVM))
		dup := c.dupVector(argVal, innerSize)
		c.emitVectorElementCloneLoop(dup, innerElem)
		return dup
	}
	if named == types.TypVector {
		return c.dupVector(argVal, 0)
	}

	// Channel element: dup (increment ref count)
	if _, isCh := types.AsChannel(resolvedElem); isCh || named == types.TypChannel {
		return c.dupChannel(argVal)
	}

	// T0508: Ref[T] — strong-count increment (non-atomic when `confined, T0995).
	if arcElem, isArc := types.AsArc(resolvedElem); isArc || named == types.TypArc {
		if c.typeSubst != nil && arcElem != nil {
			arcElem = types.Substitute(arcElem, c.typeSubst)
		}
		return c.dupArc(argVal, arcElem)
	}

	// T0508: Weak[T] — atomic weak-count increment.
	if elem, isWeak := types.AsWeak(resolvedElem); isWeak {
		resolvedWeakElem := elem
		if c.typeSubst != nil {
			resolvedWeakElem = types.Substitute(resolvedWeakElem, c.typeSubst)
		}
		return c.dupWeak(argVal, resolvedWeakElem)
	}

	// T0508: Single-owner native handles (Mutex/MutexGuard/Task) have no dup
	// semantics. Ownership rejects double-moves of non-Copy values, so the
	// runtime dup branch is unreachable in valid programs — return nil.
	if _, isMutex := types.AsMutex(resolvedElem); isMutex || named == types.TypMutex {
		return nil
	}
	if _, isMG := types.AsMutexGuard(resolvedElem); isMG || named == types.TypMutexGuard {
		return nil
	}
	if _, isTask := types.AsAnyTask(resolvedElem); isTask || types.IsTaskLikeOrigin(named) {
		return nil
	}

	// T1284: structural-interface element — the {vtable, instance} view boxes a
	// heap instance; deep-clone it via RTTI so each vector slot owns an
	// independent box (else result.push(this[i]) in Vector.[:] aliases the source
	// box and the structural-aware element drop double-frees).
	if named.IsStructural() && !named.IsValueType() {
		return c.cloneStructuralView(argVal)
	}

	// Heap user type with drop: clone via clone method or dupHeapValue fallback
	if !named.IsValueType() && !named.IsCopy() && !isPrimitiveScalar(named) && !named.IsStructural() {
		return c.cloneHeapElement(argVal, resolvedElem, named)
	}

	return nil // value/Copy type — no dup needed
}

// storeBackSlicePtr stores the new vector pointer back into the variable that holds the vector.
// This is needed because push may realloc.
func (c *Compiler) storeBackSlicePtr(target ast.Expr, newPtr value.Value) {
	switch t := target.(type) {
	case *ast.IdentExpr:
		if ptr, ok := c.mutRefPtrs[t.Name]; ok {
			// MutRef param: store through the caller's pointer (B0149)
			c.block.NewStore(newPtr, ptr)
		} else if alloca, ok := c.locals[t.Name]; ok {
			c.block.NewStore(newPtr, alloca)
		}
	case *ast.MemberExpr:
		fieldPtr := c.genFieldPtr(t)
		c.block.NewStore(newPtr, fieldPtr)
	case *ast.IndexExpr:
		// T0595: nested slice receiver (arr[i].push / slices[i].push) reached via a
		// path that did NOT go through storeVectorReceiverBack. The Vector method-call
		// path recomputes the slot itself via genIndexSlotPtrWithIndex (using the
		// once-evaluated index, T1064), so it never lands here; this recompute is a
		// defensive fallback. NOTE: it re-evaluates e.Index — sound only for a
		// side-effect-free index.
		// T1065: a user-defined `[]` operator returns a Vector by VALUE (an rvalue
		// temporary), not an addressable element slot. There is no slot to write the
		// grown pointer back into, so skip the store-back — matching how a Vector
		// returned from any other method (e.g. `get_vec(0).push(x)`) behaves. Only a
		// genuine array/Vector index has an addressable slot.
		if !c.indexTargetIsArrayOrVector(t) {
			return
		}
		slotPtr := c.genIndexSlotPtr(t)
		c.block.NewStore(newPtr, slotPtr)
	}
}

// emitIndexBoundsCheck branches on idx < length (unsigned). On the false path it
// emits an out-of-bounds panic with msg and returns; execution continues in a
// fresh block named "<prefix>.ok". Shared by array index read, vector index
// assign, and the T0595 nested-slice store-back so the OOB shape lives in one
// place.
func (c *Compiler) emitIndexBoundsCheck(idx, length value.Value, prefix, msg string) {
	inBounds := c.block.NewICmp(enum.IPredULT, idx, length)
	okBlock := c.newBlock(prefix + ".ok")
	panicBlock := c.newBlock(prefix + ".oob")
	c.block.NewCondBr(inBounds, okBlock, panicBlock)

	c.block = panicBlock
	oobMsg := c.makeGlobalString(msg)
	c.block.NewCall(c.funcs["promise_panic"], oobMsg)
	c.emitPanicReturn()

	c.block = okBlock
}

// indexTargetIsArrayOrVector reports whether e.Target is a fixed-size array or a
// Vector — i.e. an index whose element slot genIndexSlotPtr can address. Mirrors
// the type-unwrap prologue of genIndexSlotPtr. Used to gate the single-eval
// receiver path in genVectorMethodCall (T0595).
func (c *Compiler) indexTargetIsArrayOrVector(e *ast.IndexExpr) bool {
	t := c.info.Types[e.Target]
	if c.typeSubst != nil {
		t = types.Substitute(t, c.typeSubst)
	}
	if ref, ok := t.(*types.MutRef); ok {
		t = ref.Elem()
	}
	if ref, ok := t.(*types.SharedRef); ok {
		t = ref.Elem()
	}
	if _, ok := t.(*types.Array); ok {
		return true
	}
	if _, ok := types.AsVector(t); ok {
		return true
	}
	return extractNamed(t) == types.TypVector && c.typeSubst != nil
}

// indexTargetIsReallocatablePlace reports whether an index receiver's outer
// container (e.g. `vov` in `vov[i].push(...)`) is an addressable NAMED place — a
// local/mutref ident or a real owned struct field — that a method ARGUMENT could
// reach and reallocate during argument evaluation (T1064). For such a place the
// store-back must recompute the element slot from a freshly-loaded outer pointer,
// and re-addressing it via genExpr is side-effect-free so the recompute is sound.
//
// An rvalue outer (a call/getter result, e.g. `get_vov()[0].push(x)`) is NOT a
// place: no external alias can reach it to reallocate it during argument
// evaluation, AND re-evaluating it would spawn a fresh temporary — so the caller
// captures the early slot ONCE instead (matching the pre-T1064 T0595 behavior;
// re-evaluating a call rvalue at store-back would double-free / leak). Nested
// index targets (`vovv[i][j]`) are likewise routed to the early-slot path: a
// recompute would re-evaluate the inner index, so they keep their pre-T1064
// behavior rather than risk a double-eval of an impure inner index.
func (c *Compiler) indexTargetIsReallocatablePlace(target ast.Expr) bool {
	switch e := target.(type) {
	case *ast.IdentExpr:
		if _, ok := c.mutRefPtrs[e.Name]; ok {
			return true
		}
		_, ok := c.locals[e.Name]
		return ok
	case *ast.MemberExpr:
		// A real owned field re-addresses purely; a getter/module-getter or a
		// borrow-returning member produces a fresh rvalue (mirrors the T1295
		// addressability guards in optionalPayloadReceiverSlot).
		return !c.isGetterCallExpr(target) && !c.isBorrowedExpr(target)
	}
	return false
}

// optionalPayloadReceiverSlot handles a Vector-method receiver of the form
// `place!` (an OptionalUnwrapExpr over an addressable place holding an
// Optional[Vector[T]]). It returns the loaded inner-Vector pointer plus a
// pointer to the optional's PAYLOAD field (struct index 1) — the slot a
// relocating method (push/pop/remove) writes the grown pointer back into.
// Emits the `!` presence-check panic (mirrors genOptionalForceUnwrap). Returns
// ok=false for any non-addressable / non-Optional[Vector] shape so the caller
// falls back to the rvalue path (read method receivers like `v[0]!.contains(x)`
// / `v[0]!.clone()` also take this path — slot is simply unused; the `.len`
// getter is dispatched elsewhere and never reaches here). T1295.
func (c *Compiler) optionalPayloadReceiverSlot(target ast.Expr) (slicePtr, slot value.Value, ok bool) {
	unwrap, isUnwrap := target.(*ast.OptionalUnwrapExpr)
	if !isUnwrap {
		return nil, nil, false
	}
	inner := unwrap.Expr
	for {
		if p, isParen := inner.(*ast.ParenExpr); isParen {
			inner = p.Expr
			continue
		}
		break
	}

	// The unwrapped place must hold an Optional[Vector[...]] (payload lowers to i8*).
	innerType := c.info.Types[inner]
	if c.typeSubst != nil {
		innerType = types.Substitute(innerType, c.typeSubst)
	}
	optType, isOpt := innerType.(*types.Optional)
	if !isOpt {
		return nil, nil, false
	}
	if !types.IsVector(optType.Elem()) {
		return nil, nil, false
	}

	// Address the optional's in-memory storage for each addressable place kind.
	var optPtr value.Value
	switch e := inner.(type) {
	case *ast.IdentExpr:
		if ptr, has := c.mutRefPtrs[e.Name]; has {
			optPtr = ptr
		} else if alloca, has := c.locals[e.Name]; has {
			optPtr = alloca
		} else {
			return nil, nil, false
		}
	case *ast.MemberExpr:
		// Only a plain owned field is addressable — a getter call or a
		// borrow-returning member is not (mirrors the T1289 guards).
		if c.isGetterCallExpr(inner) || c.isBorrowedExpr(inner) {
			return nil, nil, false
		}
		optPtr = c.genFieldPtr(e)
	case *ast.IndexExpr:
		if !c.indexTargetIsArrayOrVector(e) {
			return nil, nil, false
		}
		optPtr = c.genIndexSlotPtr(e)
	default:
		return nil, nil, false
	}

	optLLVM, structOk := c.resolveType(optType).(*irtypes.StructType)
	if !structOk {
		return nil, nil, false
	}

	// `!` presence check (identical to genOptionalForceUnwrap).
	flagPtr := c.block.NewGetElementPtr(optLLVM, optPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
	flag := c.block.NewLoad(irtypes.I1, flagPtr)
	okBlock := c.newBlock("unwrap.ok")
	panicBlock := c.newBlock("unwrap.panic")
	c.block.NewCondBr(flag, okBlock, panicBlock)

	c.block = panicBlock
	panicMsg := c.makeGlobalString("unwrap failed: optional is none")
	c.block.NewCall(c.funcs["promise_panic"], panicMsg)
	c.emitPanicReturn()

	c.block = okBlock
	payloadPtr := c.block.NewGetElementPtr(optLLVM, optPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 1))
	slicePtr = c.block.NewLoad(irtypes.I8Ptr, payloadPtr)
	return slicePtr, payloadPtr, true
}

// vectorReceiver holds everything storeVectorReceiverBack needs to write a
// grown/relocated Vector pointer back into a push/pop/remove receiver. For an
// arr[i]/vov[i] receiver it deliberately does NOT carry an early-computed element
// slot: an argument expression can reallocate the outer vector during argument
// evaluation, freeing the buffer that slot points into (T1064). Instead the slot
// is recomputed from a freshly-loaded outer pointer at store-back time, reusing
// idxVal so the (possibly impure) index is still evaluated exactly once.
type vectorReceiver struct {
	target   ast.Expr       // ident/field store-back fallback (plain v.push(x))
	slicePtr value.Value    // loaded inner Vector pointer (read once, up front)
	slot     value.Value    // directly-captured non-dangling slot (T1295 payload / T1370 temp alloca)
	idxExpr  *ast.IndexExpr // set for an arr[i]/vov[i] receiver → recompute slot at store-back
	idxVal   value.Value    // index evaluated exactly once (impure-index safety)
}

// evalVectorReceiver evaluates a Vector method-call receiver. For an arr[i] /
// vov[i] receiver it evaluates the index EXACTLY ONCE (idxVal) and loads the inner
// Vector pointer, but returns idxExpr/idxVal instead of an early element slot: the
// store-back must recompute the slot from a fresh outer pointer, because an
// argument that reallocates the outer vector would leave an early slot dangling
// (T1064). Recomputing with the saved idxVal keeps an impure index single-eval —
// re-evaluating e.Index would yield a DIFFERENT slot (use-after-free + leak).
// For non-index receivers it falls back to genExprAutoPropagate.
func (c *Compiler) evalVectorReceiver(target ast.Expr) vectorReceiver {
	// T1295: `place!.push/pop/remove` on an addressable Optional[Vector[T]] place.
	// The captured slot is the optional's payload field — addressable stack/heap
	// storage, never a freeable vector buffer — so a relocating method stores the
	// grown pointer straight into it (else the write-back is dropped and the fresh
	// COW/realloc buffer leaks).
	if sp, slotPtr, ok := c.optionalPayloadReceiverSlot(target); ok {
		return vectorReceiver{target: target, slicePtr: sp, slot: slotPtr}
	}
	if idxExpr, ok := target.(*ast.IndexExpr); ok && c.indexTargetIsArrayOrVector(idxExpr) {
		// T0648: suppress whole-container field dup while evaluating the outer
		// target (matches the genVectorIndex/genArrayIndex read path this replaces);
		// we want the real slot, not a clone of the outer field.
		savedDupContainer := c.dupContainerFieldAccess
		c.dupContainerFieldAccess = false
		// Evaluate the index once, then compute the slot with it purely to load the
		// inner Vector pointer. The early slot is intentionally discarded: an
		// argument that reallocates the outer vector would free the buffer it points
		// into (T1064). storeVectorReceiverBack recomputes the slot from a fresh
		// outer pointer using idxVal.
		idxVal := c.genExpr(idxExpr.Index)
		slot := c.genIndexSlotPtrWithIndex(idxExpr, idxVal)
		c.dupContainerFieldAccess = savedDupContainer
		// The slot holds the inner Vector's i8* (resolveType(Vector[T]) == i8*),
		// matching what cow/push/pop/remove consume below.
		slicePtr := c.block.NewLoad(irtypes.I8Ptr, slot)
		if c.indexTargetIsReallocatablePlace(idxExpr.Target) {
			// Addressable outer place (local/mutref/field): an argument could
			// reallocate it during arg eval, so discard the early slot and
			// recompute it from a fresh outer pointer at store-back (T1064).
			return vectorReceiver{
				target:   target,
				slicePtr: slicePtr,
				idxExpr:  idxExpr,
				idxVal:   idxVal,
			}
		}
		// Rvalue outer (call/getter result) or nested index: no external alias
		// can reallocate it during arg eval, and re-evaluating it at store-back
		// would spawn a fresh temporary (double-free) — so keep the early slot
		// and store through it once, exactly as before T1064.
		return vectorReceiver{target: target, slicePtr: slicePtr, slot: slot}
	}
	val := c.genExprAutoPropagate(target) // B0323
	// T1370: rvalue receiver (a Vector returned by a call/getter, not an
	// addressable place). If it is a tracked statement temp, hand back its backing
	// alloca as the write-back slot so a relocating `push` stores the grown buffer
	// where cleanupStmtTemps will read it. Otherwise the temp's drop frees the stale
	// pre-realloc pointer (double-free / "bad header magic") or leaks the new buffer.
	if idx, ok := c.stmtTempMap[val]; ok && idx >= 0 {
		return vectorReceiver{target: target, slicePtr: val, slot: c.stmtTemps[idx].alloca}
	}
	return vectorReceiver{target: target, slicePtr: val}
}

// storeVectorReceiverBack writes a grown/relocated Vector pointer back into its
// receiver. For an arr[i]/vov[i] receiver it recomputes the element slot from the
// current (post-argument) outer pointer + the once-evaluated index (T1064). When a
// non-dangling slot was pre-captured (T1295 optional payload / T1370 temp alloca)
// it stores through that slot directly; otherwise it defers to storeBackSlicePtr
// for an ident/field target (plain v.push(x)).
func (c *Compiler) storeVectorReceiverBack(rcv vectorReceiver, newPtr value.Value) {
	if rcv.idxExpr != nil {
		// T1064: recompute the element slot from the current (post-argument) outer
		// pointer + the once-evaluated index, so an argument that reallocated the
		// outer vector can't leave the write-back slot dangling. Suppress
		// dupContainerFieldAccess exactly as the early read does — for a field outer
		// (self.vov[i]) we want the real element slot, not a clone of the field.
		savedDupContainer := c.dupContainerFieldAccess
		c.dupContainerFieldAccess = false
		c.block.NewStore(newPtr, c.genIndexSlotPtrWithIndex(rcv.idxExpr, rcv.idxVal))
		c.dupContainerFieldAccess = savedDupContainer
		return
	}
	if rcv.slot != nil {
		c.block.NewStore(newPtr, rcv.slot)
		return
	}
	c.storeBackSlicePtr(rcv.target, newPtr)
}

// genIndexSlotPtr returns a pointer to the element slot of a fixed-size array or
// Vector at e.Index, bounds-checked. Used by storeBackSlicePtr to write a grown
// nested Vector's pointer back into its slot (T0595). Mirrors the element-pointer
// computation in genArrayIndex / genVectorIndexAssign. Re-evaluates e.Index.
func (c *Compiler) genIndexSlotPtr(e *ast.IndexExpr) value.Value {
	return c.genIndexSlotPtrWithIndex(e, nil)
}

// genIndexSlotPtrWithIndex is genIndexSlotPtr with an optional pre-evaluated index.
// When idx is non-nil it is used verbatim instead of re-evaluating e.Index, so a
// caller that reads the slot early and recomputes it after argument evaluation
// (T1064 store-back) evaluates a possibly-impure index exactly once. The container
// target is re-addressed on each call so the recompute uses the current (possibly
// reallocated) outer pointer.
func (c *Compiler) genIndexSlotPtrWithIndex(e *ast.IndexExpr, idx value.Value) value.Value {
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

	// Fixed-size array (Vector[int][2]): GEP into the array storage.
	if arr, ok := targetType.(*types.Array); ok {
		basePtr := c.genArrayBasePtr(e.Target, arr)
		if idx == nil {
			idx = c.genExpr(e.Index)
		}
		elemLLVM := c.resolveType(arr.Elem())
		arrType := irtypes.NewArray(uint64(arr.Size()), elemLLVM)
		c.emitIndexBoundsCheck(idx, constant.NewInt(irtypes.I64, arr.Size()),
			"arridx", "array index out of bounds")
		return c.block.NewGetElementPtr(arrType, basePtr,
			constant.NewInt(irtypes.I32, 0), idx)
	}

	// Vector (Vector[int][]): GEP into the heap buffer after the header. The outer
	// vector is always heap-allocated (a vector-of-vectors can never be a .rodata
	// static literal — T0062 statics require compile-time-constant scalar elements).
	// The inner push never reallocates the outer, but an ARGUMENT to the method can
	// (T1064) — so the store-back caller re-addresses e.Target here to pick up the
	// current outer pointer rather than trusting an early-captured slot.
	elemType, ok := types.AsVector(targetType)
	if !ok && extractNamed(targetType) == types.TypVector && c.typeSubst != nil {
		elemType = c.resolveTypeParam(types.TypVector.TypeParams()[0])
		ok = elemType != nil
	}
	if !ok {
		panic(fmt.Sprintf("codegen: storeBackSlicePtr index target is not array/vector: %s", targetType))
	}
	slicePtr := c.genExpr(e.Target)
	if idx == nil {
		idx = c.genExpr(e.Index)
	}
	elemLLVM := c.resolveType(elemType)
	headerPtr := c.block.NewBitCast(slicePtr, irtypes.NewPointer(vectorHeaderType()))
	length := loadVectorLen(c.block, headerPtr)
	c.emitIndexBoundsCheck(idx, length, "nestedpush", "index out of bounds")
	dataBase := c.block.NewGetElementPtr(irtypes.I8, slicePtr,
		constant.NewInt(irtypes.I64, int64(vectorHeaderSize)))
	dataTypedPtr := c.block.NewBitCast(dataBase, irtypes.NewPointer(elemLLVM))
	return c.block.NewGetElementPtr(elemLLVM, dataTypedPtr, idx)
}

// isVariantPayloadBorrowShape reports whether a type has the shape that
// matchBindingIsBorrow marks match-borrowed for a droppable enum variant payload:
// Optional-of-anything or fixed-Array-of-anything. This is the gate the T1170
// read/escape-side dup uses — it precisely selects enum-variant Optional/Array
// payload bindings while EXCLUDING the bare-heap (string/Vector/…) bindings that
// T0672 also places in matchBorrowedIdents (if-let / while-let / container-index /
// tuple-destructure borrow sources). Those bare-heap bindings are already owned
// copies produced by their source's own dup-on-read (e.g. Map's `[]` body), so
// dup'ing them again would leak — hence they must not take the T1170 escape dup.
func isVariantPayloadBorrowShape(t types.Type) bool {
	if t == nil {
		return false
	}
	if _, ok := t.(*types.Optional); ok {
		return true
	}
	if _, ok := t.(*types.Array); ok {
		return true
	}
	return false
}

// isUnwrappedContainerIndex reports whether expr is `container[k]!` — an
// OptionalUnwrapExpr whose inner (peeling ParenExpr) is an IndexExpr. When such
// an inline unwrap escapes into an OWNING context (move-param arg, constructor
// field-init, return), the unwrapped element aliases the container's slot, so
// without a dup the owning sink AND the container's drop free the same instance
// (double-free). Setting dupHeapUserFieldAccess routes it through genMethodIndex's
// deep-dup, mirroring the var-binding form (genTypedVarDecl, stmt.go). The dup is
// gated inside genMethodIndex on the droppable-no-clone Map shape, so enabling the
// flag here is safe for any other shape (it is reset after genExpr; clone-bearing
// V dups internally and never double-dups). T1146.
func isUnwrappedContainerIndex(expr ast.Expr) bool {
	unwrap, ok := expr.(*ast.OptionalUnwrapExpr)
	if !ok {
		return false
	}
	inner := unwrap.Expr
	for {
		p, ok := inner.(*ast.ParenExpr)
		if !ok {
			break
		}
		inner = p.Expr
	}
	_, ok = inner.(*ast.IndexExpr)
	return ok
}

// --- Vector / Array Literal ---

const vectorHeaderSize = 16

func vectorHeaderType() *irtypes.StructType {
	return irtypes.NewStruct(irtypes.I64, irtypes.I64)
}

// vectorLenMask is 0x7FFFFFFFFFFFFFFF — masks off the static flag (bit 63).
var vectorLenMask = constant.NewInt(irtypes.I64, 0x7FFFFFFFFFFFFFFF)

// loadVectorLen loads the vector length from the header with bit 63 masked off.
// Bit 63 is the static flag (set for .rodata vectors, clear for heap vectors).
func loadVectorLen(b *ir.Block, headerPtr value.Value) value.Value {
	headerType := vectorHeaderType()
	lenPtr := b.NewGetElementPtr(headerType, headerPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
	raw := b.NewLoad(irtypes.I64, lenPtr)
	return b.NewAnd(raw, vectorLenMask)
}

// loadVectorLenRaw loads the raw vector length from the header with bit 63 intact.
func loadVectorLenRaw(b *ir.Block, headerPtr value.Value) value.Value {
	headerType := vectorHeaderType()
	lenPtr := b.NewGetElementPtr(headerType, headerPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
	return b.NewLoad(irtypes.I64, lenPtr)
}

// --- Index Expression ---

func (c *Compiler) genSliceExpr(e *ast.SliceExpr) value.Value {
	targetType := c.info.Types[e.Target]
	if c.typeSubst != nil {
		targetType = types.Substitute(targetType, c.typeSubst)
	}
	// Unwrap MutRef/SharedRef for slicing (auto-deref through borrows)
	if ref, ok := targetType.(*types.MutRef); ok {
		targetType = ref.Elem()
	}
	if ref, ok := targetType.(*types.SharedRef); ok {
		targetType = ref.Elem()
	}

	named := extractNamed(targetType)
	if named == nil {
		panic(fmt.Sprintf("codegen: cannot slice type %s", targetType))
	}
	m := named.LookupMethod("[:]")
	if m == nil {
		panic(fmt.Sprintf("codegen: no [:] method on type %s", named))
	}

	target := c.genExprAutoPropagate(e.Target) // B0323

	// Generate optional int arguments for low and high bounds
	optIntType := irtypes.NewStruct(irtypes.I1, irtypes.I64)
	low := c.genSliceBound(e.Low, optIntType)
	high := c.genSliceBound(e.High, optIntType)

	if m.IsNative() {
		return c.genNativeSlice(named, targetType, target, low, high)
	}

	// Non-native: call monomorphized [:] method
	mangledName := mangleMethodName(c.resolveTypeName(targetType), "[:]", false)
	fn, ok := c.funcs[mangledName]
	if !ok {
		panic(fmt.Sprintf("codegen: undeclared [:] method %s", mangledName))
	}

	var instancePtr value.Value
	switch {
	case isThisReceiver(e.Target):
		// T0745: `this` (incl. paren-wrapped) is already the i8* receiver ptr.
		instancePtr = target
	case isContainerType(targetType):
		instancePtr = target
	case named != nil && named.IsValueType():
		instancePtr = c.valueTypeReceiverPtr(target, targetType)
	default:
		instancePtr = c.extractInstancePtr(target)
	}

	return c.block.NewCall(fn, instancePtr, low, high)
}

// genSliceBound generates an optional int value for a slice bound expression.
// If expr is nil, returns none ({i1 false, i64 0}). Otherwise wraps the value.
// If the expression already produces an optional (int?), passes it through directly.
func (c *Compiler) genSliceBound(expr ast.Expr, optType *irtypes.StructType) value.Value {
	if expr == nil {
		return constant.NewZeroInitializer(optType)
	}
	val := c.genExpr(expr)
	// If the expression type is already optional, pass through directly.
	exprType := c.info.Types[expr]
	if c.typeSubst != nil {
		exprType = types.Substitute(exprType, c.typeSubst)
	}
	if _, isOpt := exprType.(*types.Optional); isOpt {
		return val
	}
	return c.wrapOptional(val, optType)
}

func (c *Compiler) genIndexExpr(e *ast.IndexExpr) value.Value {
	targetType := c.info.Types[e.Target]
	if c.typeSubst != nil {
		targetType = types.Substitute(targetType, c.typeSubst)
	}
	// Unwrap MutRef/SharedRef for indexing (auto-deref through borrows)
	if ref, ok := targetType.(*types.MutRef); ok {
		targetType = ref.Elem()
	}
	if ref, ok := targetType.(*types.SharedRef); ok {
		targetType = ref.Elem()
	}

	// Fixed-size array indexing
	if arr, ok := targetType.(*types.Array); ok {
		return c.genArrayIndex(e, arr)
	}

	named := extractNamed(targetType)
	if named != nil {
		if m := named.LookupMethod("[]"); m != nil {
			if m.IsNative() {
				return c.genNativeIndex(e, named, targetType)
			}
			return c.genMethodIndex(e, targetType)
		}
	}

	panic(fmt.Sprintf("codegen: cannot index type %s", targetType))
}

// genArrayBasePtr returns a pointer to the base of a fixed-size array.
// For identifier targets, returns the alloca directly (needed for index assignment).
// For struct field targets, returns a pointer to the field in the instance.
// For other expressions, allocas a temp and stores the value.
func (c *Compiler) genArrayBasePtr(target ast.Expr, arr *types.Array) value.Value {
	if ident, ok := target.(*ast.IdentExpr); ok {
		if alloca, ok := c.locals[ident.Name]; ok {
			return alloca
		}
	}
	// Struct field: return pointer to the field directly (not a copy)
	if memberExpr, ok := target.(*ast.MemberExpr); ok {
		return c.genFieldPtr(memberExpr)
	}
	arrVal := c.genExprAutoPropagate(target) // B0323
	elemLLVM := c.resolveType(arr.Elem())
	arrType := irtypes.NewArray(uint64(arr.Size()), elemLLVM)
	tmp := c.createEntryAlloca(arrType)
	c.block.NewStore(arrVal, tmp)
	return tmp
}

// genArrayIndex handles arr[i] for fixed-size arrays with bounds checking.
func (c *Compiler) genArrayIndex(e *ast.IndexExpr, arr *types.Array) value.Value {
	// T1711: Save and suppress dup-on-read flags before evaluating the target
	// to prevent nested index expressions from consuming them prematurely.
	savedDupString := c.dupStringFieldAccess
	savedDupTuple := c.dupTupleFieldAccess
	savedDupHeapUser := c.dupHeapUserFieldAccess
	savedDupContainer := c.dupContainerFieldAccess
	c.dupStringFieldAccess = false
	c.dupTupleFieldAccess = false
	c.dupHeapUserFieldAccess = false
	c.dupContainerFieldAccess = false
	basePtr := c.genArrayBasePtr(e.Target, arr)
	c.dupStringFieldAccess = savedDupString
	c.dupTupleFieldAccess = savedDupTuple
	c.dupHeapUserFieldAccess = savedDupHeapUser
	c.dupContainerFieldAccess = savedDupContainer
	idx := c.genExpr(e.Index)
	elemLLVM := c.resolveType(arr.Elem())
	arrType := irtypes.NewArray(uint64(arr.Size()), elemLLVM)

	// Bounds check: idx < N
	c.emitIndexBoundsCheck(idx, constant.NewInt(irtypes.I64, arr.Size()),
		"arridx", "array index out of bounds")
	elemPtr := c.block.NewGetElementPtr(arrType, basePtr,
		constant.NewInt(irtypes.I32, 0), idx)
	val := c.block.NewLoad(elemLLVM, elemPtr)

	// T0590: Dup-on-read for fixed-size arrays. Mirrors the Vector dup-on-read
	// branches in genVectorIndex (B0204/T0370/T0383/T0398/T0412) plus the Optional
	// extract+dup+insert branches from genMethodIndex (B0347/T0397/T0440/T0366).
	// Without these dups, slot reads alias the array's owned data — combined with
	// drop-on-overwrite (T0583), `arr[1] = arr[0]` and `let x = arr[0]; arr[0] = c;`
	// produce double-frees at scope exit. Bare types dup directly; Optional types
	// extract inner, dup, re-insert, and set the optional*Dup sentinel.
	elemType := arr.Elem()
	if c.typeSubst != nil {
		elemType = types.Substitute(elemType, c.typeSubst)
	}

	// String element (B0204 analogue)
	if c.dupStringFieldAccess && c.tempTrackingEnabled && extractNamed(elemType) == types.TypString {
		c.dupStringFieldAccess = false // consume the flag
		dup := c.dupString(val)
		c.trackStringTemp(dup)
		return dup
	}

	// Optional[string] element (B0347 analogue)
	if c.dupStringFieldAccess && c.tempTrackingEnabled {
		if opt, ok := elemType.(*types.Optional); ok && extractNamed(opt.Elem()) == types.TypString {
			c.dupStringFieldAccess = false // consume the flag
			innerStr := c.block.NewExtractValue(val, 1)
			dup := c.dupString(innerStr)
			c.trackStringTemp(dup)
			c.optionalStringDup = dup
			return c.block.NewInsertValue(val, dup, 1)
		}
	}

	// Droppable tuple element (T0370 analogue)
	if c.dupTupleFieldAccess && c.tempTrackingEnabled {
		if tup, ok := elemType.(*types.Tuple); ok && c.tupleNeedsDrop(elemType) {
			c.dupTupleFieldAccess = false // consume the flag
			return c.dupTupleValue(val, tup)
		}
	}

	// Optional[Tuple<droppable>] element (T0397 analogue)
	if c.dupTupleFieldAccess && c.tempTrackingEnabled {
		if opt, ok := elemType.(*types.Optional); ok {
			inner := opt.Elem()
			if c.typeSubst != nil {
				inner = types.Substitute(inner, c.typeSubst)
			}
			if tup, isTup := inner.(*types.Tuple); isTup && c.tupleNeedsDrop(inner) {
				c.dupTupleFieldAccess = false // consume the flag
				innerTup := c.block.NewExtractValue(val, 1)
				dup := c.dupTupleValue(innerTup, tup)
				c.optionalTupleDup = dup
				return c.block.NewInsertValue(val, dup, 1)
			}
		}
	}

	// Droppable heap user element (T0398 analogue)
	if c.dupHeapUserFieldAccess && c.tempTrackingEnabled {
		if isDroppableHeapUserType(elemType) {
			if named := extractNamed(elemType); named != nil {
				c.dupHeapUserFieldAccess = false // consume the flag
				return c.cloneHeapElement(val, elemType, named)
			}
		}
	}

	// T0590: Heap user without explicit drop (pal_free-only path) — same dup-on-
	// read need as the drop branch above. `isDroppableHeapUserType` excludes types
	// with no drop/synth-drop because the Map clone path relies on that gate, but
	// arrays have no internal match-dup so we dup unconditionally for any heap
	// user type. `dupHeapValue` handles the no-droppable-field layout fine
	// (pal_alloc + memcpy + sub-field dup, with no sub-fields to dup for _Bare).
	if c.dupHeapUserFieldAccess && c.tempTrackingEnabled && isHeapUserNoDropPalFree(elemType) {
		c.dupHeapUserFieldAccess = false // consume the flag
		return c.dupHeapValue(val, elemType)
	}

	// T1129: Droppable-enum element (extractNamed is nil for enums, so the heap-user
	// branches above skip them). Mirrors the genVectorIndex enum branch — without
	// this, `got := arr[i]` aliases the array slot and got's drop + the array's
	// element walk double-free the variant data (fatal for recursive enums).
	// cloneResolvedValue deep-clones via the synthesized/explicit/shallow path.
	if c.dupHeapUserFieldAccess && c.tempTrackingEnabled &&
		c.enumElemNeedsDupOnRead(elemType) {
		c.dupHeapUserFieldAccess = false // consume the flag
		return c.cloneResolvedValue(val, elemType)
	}

	// T1130: Map/Set element read-back from a fixed-size array. Map/Set are excluded
	// from isDroppableHeapUserType (T0440), so the heap-user branch above skips them —
	// but arrays have no internal match-dup, so `got := arr[i]` aliases the array slot.
	// Deep-clone via the element's clone() so the binding owns an independent Map/Set.
	if c.dupHeapUserFieldAccess && c.tempTrackingEnabled && isMapOrSetType(elemType) {
		if named := extractNamed(elemType); named != nil {
			c.dupHeapUserFieldAccess = false // consume the flag
			return c.cloneHeapElement(val, elemType, named)
		}
	}

	// Optional[heap-user-type] element (T0440 analogue, relaxed for arrays).
	// The genMethodIndex gate restricts to `drop && !clone` because Map.[]'s
	// body internally dups V via match-destructure for clone-bearing types —
	// duping again at the call site would double-allocate. Arrays have no
	// internal dup in `genArrayIndex`, so the gate is dropped: any droppable
	// heap user (with or without clone, with or without drop) needs dup here.
	// `dupHeapValue` is null-safe internally and dispatches to the type's
	// typeinfo clone fn for polymorphic types (T0387).
	if c.dupHeapUserFieldAccess && c.tempTrackingEnabled {
		if opt, ok := elemType.(*types.Optional); ok {
			inner := opt.Elem()
			if c.typeSubst != nil {
				inner = types.Substitute(inner, c.typeSubst)
			}
			if isDroppableHeapUserType(inner) || isHeapUserNoDropPalFree(inner) {
				c.dupHeapUserFieldAccess = false // consume the flag
				innerVal := c.block.NewExtractValue(val, 1)
				dup := c.dupHeapValue(innerVal, inner)
				c.optionalHeapDup = dup
				return c.block.NewInsertValue(val, dup, 1)
			}
		}
	}

	// T1341: Optional[structural-interface] element read from a fixed array
	// (`Showable? g = a[0]`). The {vtable, instance} view boxes a heap instance;
	// isDroppableHeapUserType / isHeapUserNoDropPalFree both exclude structural, so
	// the branch above skips it and the read aliases the array's box. But the local's
	// optional drop binding (bindingDropOptional rttiDrop) AND the array's element
	// drop both free that box via __promise_structural_drop → double-free. Deep-clone
	// the box on read via dupOptionalVectorElem (whose structural case clones only on
	// the present path, so a `none` element isn't clone-dispatched on a null instance).
	// Mirrors the genVectorIndex T1291 branch.
	if c.dupHeapUserFieldAccess && c.tempTrackingEnabled {
		if opt, ok := elemType.(*types.Optional); ok {
			inner := opt.Elem()
			if c.typeSubst != nil {
				inner = types.Substitute(inner, c.typeSubst)
			}
			if isNonValueStructuralType(inner) {
				c.dupHeapUserFieldAccess = false // consume the flag
				return c.dupOptionalVectorElem(val, opt, inner)
			}
		}
	}

	// Container element: Vector / Channel / Arc / Weak (T0383 analogue)
	if c.dupContainerFieldAccess && c.tempTrackingEnabled {
		// T1266: a fixed-array element that is itself a value-copying container transitively
		// nesting a closure must NOT be duped — dupVector's element-clone loop zeroes each
		// closure's opaque env (T0813) → null {fn,env} → SEGV on invoke. Leave it ALIASED
		// (a borrow of the array's owned storage, env intact); the borrow gates
		// (isClosureAggregateBorrow / closureAggregateBorrowSource) suppress the owning drop
		// binding and reject escapes, keeping this in lockstep. Mirrors the genVectorIndex
		// T1263 guard; FirstFieldNestedClosureDeep keeps Ref/Weak/… opaque, so Ref[…][] /
		// int[] elements keep deep-copying.
		resolvedContainerElem := elemType
		if c.typeSubst != nil {
			resolvedContainerElem = types.Substitute(elemType, c.typeSubst)
		}
		if sema.FirstFieldNestedClosureDeep(resolvedContainerElem) != nil {
			c.dupContainerFieldAccess = false // consume the flag
			return val
		}
		if innerElem, isVec := types.AsVector(elemType); isVec {
			c.dupContainerFieldAccess = false // consume the flag
			innerLLVM := c.resolveType(innerElem)
			innerSize := int64(c.typeSize(innerLLVM))
			dup := c.dupVector(val, innerSize)
			c.emitVectorElementCloneLoop(dup, innerElem)
			c.trackVectorTempWithElemType(dup, innerElem)
			return dup
		}
		if extractNamed(elemType) == types.TypVector {
			c.dupContainerFieldAccess = false
			dup := c.dupVector(val, 0)
			c.trackVectorTemp(dup)
			return dup
		}
		if chElem, isCh := types.AsChannel(elemType); isCh {
			c.dupContainerFieldAccess = false
			dup := c.dupChannel(val)
			c.trackChannelTempWithElemType(dup, chElem) // T0663
			return dup
		}
		if arcElem, isArc := types.AsArc(elemType); isArc {
			c.dupContainerFieldAccess = false
			resolvedArcElem := arcElem
			if c.typeSubst != nil {
				resolvedArcElem = types.Substitute(arcElem, c.typeSubst)
			}
			dup := c.dupArc(val, resolvedArcElem)
			c.trackTempWithDrop(dup, c.getOrCreateArcDrop(resolvedArcElem))
			return dup
		}
		if weakElem, isWeak := types.AsWeak(elemType); isWeak {
			c.dupContainerFieldAccess = false
			resolvedWeakElem := weakElem
			if c.typeSubst != nil {
				resolvedWeakElem = types.Substitute(weakElem, c.typeSubst)
			}
			dup := c.dupWeak(val, resolvedWeakElem)
			c.trackTempWithDrop(dup, c.getOrCreateWeakDrop(resolvedWeakElem))
			return dup
		}
	}

	// Optional[Vector|Channel|Arc|Weak] element (T0366 analogue)
	if c.dupContainerFieldAccess && c.tempTrackingEnabled {
		if opt, ok := elemType.(*types.Optional); ok {
			inner := opt.Elem()
			if c.typeSubst != nil {
				inner = types.Substitute(inner, c.typeSubst)
			}
			if innerElem, isVec := types.AsVector(inner); isVec {
				c.dupContainerFieldAccess = false
				innerLLVM := c.resolveType(innerElem)
				innerSize := int64(c.typeSize(innerLLVM))
				innerVec := c.block.NewExtractValue(val, 1)
				dup := c.dupVector(innerVec, innerSize)
				// T0939: dup is null on the optional's `none` path — guard the clone loop.
				c.emitVectorElementCloneLoopNullable(dup, innerElem)
				c.trackVectorTempWithElemType(dup, innerElem)
				c.optionalContainerDup = dup
				return c.block.NewInsertValue(val, dup, 1)
			}
			if chElem, isCh := types.AsChannel(inner); isCh {
				c.dupContainerFieldAccess = false
				innerCh := c.block.NewExtractValue(val, 1)
				dup := c.dupChannel(innerCh)
				c.trackChannelTempWithElemType(dup, chElem) // T0663
				c.optionalContainerDup = dup
				return c.block.NewInsertValue(val, dup, 1)
			}
			if arcElem, isArc := types.AsArc(inner); isArc {
				c.dupContainerFieldAccess = false
				innerArc := c.block.NewExtractValue(val, 1)
				resolvedArcElem := arcElem
				if c.typeSubst != nil {
					resolvedArcElem = types.Substitute(arcElem, c.typeSubst)
				}
				dup := c.dupArc(innerArc, resolvedArcElem)
				c.trackTempWithDrop(dup, c.getOrCreateArcDrop(resolvedArcElem))
				c.optionalContainerDup = dup
				return c.block.NewInsertValue(val, dup, 1)
			}
			if weakElem, isWeak := types.AsWeak(inner); isWeak {
				c.dupContainerFieldAccess = false
				innerWeak := c.block.NewExtractValue(val, 1)
				resolvedWeakElem := weakElem
				if c.typeSubst != nil {
					resolvedWeakElem = types.Substitute(weakElem, c.typeSubst)
				}
				dup := c.dupWeak(innerWeak, resolvedWeakElem)
				c.trackTempWithDrop(dup, c.getOrCreateWeakDrop(resolvedWeakElem))
				c.optionalContainerDup = dup
				return c.block.NewInsertValue(val, dup, 1)
			}
		}
	}

	return val
}

// genNativeIndex dispatches native [] implementations for built-in types.
func (c *Compiler) genNativeIndex(e *ast.IndexExpr, named *types.Named, targetType types.Type) value.Value {
	if named == types.TypString {
		return c.genStringIndex(e)
	}
	if elem, ok := types.AsVector(targetType); ok {
		return c.genVectorIndex(e, elem)
	}
	// Inside monomorphized method body: targetType is Named(Vector) not Instance(Vector[T]).
	// Get element type from typeSubst.
	if named == types.TypVector && c.typeSubst != nil {
		tp := named.TypeParams()[0]
		if elem, ok := c.typeSubst[tp]; ok {
			return c.genVectorIndex(e, elem)
		}
	}
	panic(fmt.Sprintf("codegen: no native [] implementation for type %s", named))
}

// genNativeSlice dispatches native [:] implementations for built-in types.
func (c *Compiler) genNativeSlice(named *types.Named, targetType types.Type, target, low, high value.Value) value.Value {
	if named == types.TypString {
		return c.genStringSlice(target, low, high)
	}
	panic(fmt.Sprintf("codegen: no native [:] implementation for type %s", named))
}

// genStringSlice implements string[start:end] by extracting a substring.
// Bounds are optional ints ({i1, i64}). Defaults: start=0, end=len.
func (c *Compiler) genStringSlice(strPtr, low, high value.Value) value.Value {
	instType := strInstanceType()
	typedPtr := c.block.NewBitCast(strPtr, irtypes.NewPointer(instType))

	// Load string length (masking off literal flag)
	length := loadStringLen(c.block, typedPtr, instType)

	// Resolve start: if present use value, else 0
	lowPresent := c.block.NewExtractValue(low, 0)
	lowVal := c.block.NewExtractValue(low, 1)
	start := c.block.NewSelect(lowPresent, lowVal, constant.NewInt(irtypes.I64, 0))

	// Resolve end: if present use value, else len
	highPresent := c.block.NewExtractValue(high, 0)
	highVal := c.block.NewExtractValue(high, 1)
	end := c.block.NewSelect(highPresent, highVal, length)

	// Compute slice length
	sliceLen := c.block.NewSub(end, start)

	// Get data pointer offset by start
	dataPtr := c.block.NewGetElementPtr(instType, typedPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 2),
		constant.NewInt(irtypes.I32, 0))
	sliceDataPtr := c.block.NewGetElementPtr(irtypes.I8, dataPtr, start)

	// Create new string via promise_string_new
	return c.block.NewCall(c.funcs["promise_string_new"], sliceDataPtr, sliceLen)
}

// genStringIndex implements string byte indexing: s[i] returns the byte at position i
// as a char (i32), zero-extended from i8. This is byte indexing (like Go's string[i]),
// not character indexing. UTF-8 decoding is handled separately by for-in loops.
// String instance layout: { i8* _variant, i64 len, [0 x i8] data }
func (c *Compiler) genStringIndex(e *ast.IndexExpr) value.Value {
	strPtr := c.genExprAutoPropagate(e.Target) // B0323
	idx := c.genExpr(e.Index)

	instType := strInstanceType()
	typedPtr := c.block.NewBitCast(strPtr, irtypes.NewPointer(instType))

	// Load len for bounds check (masking off literal flag)
	length := loadStringLen(c.block, typedPtr, instType)

	// Bounds check (unsigned comparison handles negative indices too)
	inBounds := c.block.NewICmp(enum.IPredULT, idx, length)
	okBlock := c.newBlock("stridx.ok")
	panicBlock := c.newBlock("stridx.oob")
	c.block.NewCondBr(inBounds, okBlock, panicBlock)

	// Out of bounds: panic
	c.block = panicBlock
	oobMsg := c.makeGlobalString("string index out of bounds")
	c.block.NewCall(c.funcs["promise_panic"], oobMsg)
	c.emitPanicReturn()

	// In bounds: load byte, zero-extend to i32 (char)
	c.block = okBlock
	dataPtr := c.block.NewGetElementPtr(instType, typedPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 2),
		constant.NewInt(irtypes.I32, 0))
	bytePtr := c.block.NewGetElementPtr(irtypes.I8, dataPtr, idx)
	byteVal := c.block.NewLoad(irtypes.I8, bytePtr)
	return c.block.NewZExt(byteVal, irtypes.I32)
}

func (c *Compiler) genVectorIndex(e *ast.IndexExpr, elemType types.Type) value.Value {
	// T0648: a vector index only ever wants ONE element. If the target is a
	// Vector[Vector|Channel|Arc|Weak] field on an owner-droppable type,
	// genFieldAccess would consume dupContainerFieldAccess to deep-clone the
	// ENTIRE outer container and track it as a stmt-temp; the index then reads
	// one element out of that clone, scope cleanup drops the whole clone
	// (incl. that element), and the returned/bound inner pointer dangles
	// (panic / SIGSEGV). Suppress the whole-field dup for the target eval so
	// the element-level dup (the dupContainerFieldAccess branch below, T0383)
	// makes the owned copy instead. Mirrors the T0500 save/restore pattern.
	// T1711: Save and suppress ALL dup-on-read flags before evaluating the target.
	// The target may contain nested index expressions (e.g. v[i].split(x)[j])
	// that would consume the flag meant for THIS index's element read.
	savedDupString := c.dupStringFieldAccess
	savedDupTuple := c.dupTupleFieldAccess
	savedDupHeapUser := c.dupHeapUserFieldAccess
	savedDupContainer := c.dupContainerFieldAccess
	c.dupStringFieldAccess = false
	c.dupTupleFieldAccess = false
	c.dupHeapUserFieldAccess = false
	c.dupContainerFieldAccess = false
	slicePtr := c.genExprAutoPropagate(e.Target) // B0323
	c.dupStringFieldAccess = savedDupString
	c.dupTupleFieldAccess = savedDupTuple
	c.dupHeapUserFieldAccess = savedDupHeapUser
	c.dupContainerFieldAccess = savedDupContainer
	idx := c.genExpr(e.Index)
	elemLLVM := c.resolveType(elemType)

	// Bounds check: load len (masked), compare index
	headerType := vectorHeaderType()
	headerPtr := c.block.NewBitCast(slicePtr, irtypes.NewPointer(headerType))
	length := loadVectorLen(c.block, headerPtr)

	inBounds := c.block.NewICmp(enum.IPredULT, idx, length)
	okBlock := c.newBlock("index.ok")
	panicBlock := c.newBlock("index.oob")
	c.block.NewCondBr(inBounds, okBlock, panicBlock)

	// Out of bounds: panic
	c.block = panicBlock
	oobMsg := c.makeGlobalString("index out of bounds")
	c.block.NewCall(c.funcs["promise_panic"], oobMsg)
	c.emitPanicReturn()

	// In bounds: load element
	c.block = okBlock
	dataBase := c.block.NewGetElementPtr(irtypes.I8, slicePtr,
		constant.NewInt(irtypes.I64, int64(vectorHeaderSize)))
	dataTypedPtr := c.block.NewBitCast(dataBase, irtypes.NewPointer(elemLLVM))
	elemPtr := c.block.NewGetElementPtr(elemLLVM, dataTypedPtr, idx)
	val := c.block.NewLoad(elemLLVM, elemPtr)

	// B0204: Dup-on-read for Vector[string] index access. When the result will
	// be stored in a variable (signaled by dupStringFieldAccess), dup the string
	// so the variable owns an independent copy. This makes it safe to drop old
	// elements on overwrite without use-after-free through aliased locals.
	if c.dupStringFieldAccess && c.tempTrackingEnabled && extractNamed(elemType) == types.TypString {
		c.dupStringFieldAccess = false // consume the flag
		dup := c.dupString(val)
		c.trackStringTemp(dup)
		return dup
	}

	// T0370: Dup-on-read for Vector[(droppable, ...)] index access. Without this,
	// `t := v[0]` aliases v's element data — t's bindingDropTuple and v's element
	// walk would both drop the same heap allocations. Symmetric with the string
	// branch above (B0204).
	if c.dupTupleFieldAccess && c.tempTrackingEnabled {
		if tup, ok := elemType.(*types.Tuple); ok && c.tupleNeedsDrop(elemType) {
			c.dupTupleFieldAccess = false // consume the flag
			return c.dupTupleValue(val, tup)
		}
	}

	// T0398: Dup-on-read for Vector[heap-user-type] index access. Without this,
	// `b := v[0]` aliases v's element instance pointer — b's drop binding and
	// v's element walk would free the same instance. Symmetric with the string
	// branch above (B0204) and the tuple branch (T0370). cloneHeapElement calls
	// the type's clone() method when available, falls back to dupHeapValue
	// (alloc + memcpy + recursive sub-field dup via dupHeapValueFields).
	// The flag is set only when the call site (var-decl or vec-to-vec
	// assign) directly consumes the index expression, so the clone is
	// guaranteed to be moved into a binding/slot — no orphaned clone leaks.
	// Note: for polymorphic element types (e.g. Vector[Shape] containing Circle),
	// dupHeapValue uses the static element layout for memcpy size — same
	// limitation as B0204/T0370/T0376.
	// (Not borrow-gated — triggered by dupHeapUserFieldAccess flag set at the
	// var-decl AST site. Remains active post-T0438.)
	if c.dupHeapUserFieldAccess && c.tempTrackingEnabled {
		resolvedElem := elemType
		if c.typeSubst != nil {
			resolvedElem = types.Substitute(elemType, c.typeSubst)
		}
		if isDroppableHeapUserType(resolvedElem) {
			if named := extractNamed(resolvedElem); named != nil {
				c.dupHeapUserFieldAccess = false // consume the flag
				return c.cloneHeapElement(val, resolvedElem, named)
			}
		}
		// T0908/T0898: Heap user element with NO explicit/synthesized drop
		// (pal_free-only path) needs the same dup-on-read as the droppable branch
		// above. The droppable gate (isDroppableHeapUserType) excludes no-drop types
		// for the T0440 Map-clone reason, so a truly-no-drop heap element (no heap
		// fields, no drop method) would otherwise alias the source slot — combined
		// with the no-drop drop-old in genVectorIndexAssign and the synth-drop owner
		// free, both the destination and the source slot would free the same
		// instance (double-free). dupHeapValue does alloc + memcpy (no sub-fields to
		// dup for a field-less heap type). Mirrors genArrayIndex's T0590 no-drop
		// branch. Required so the drop-on-overwrite added to genVectorIndexAssign
		// doesn't free a slot still aliased by a local.
		if isHeapUserNoDropPalFree(resolvedElem) {
			c.dupHeapUserFieldAccess = false // consume the flag
			return c.dupHeapValue(val, resolvedElem)
		}
		// T1129: Dup-on-read for Vector[droppable-enum] index access (extractNamed is
		// nil for enums, so the heap-user branches above skip them). Without this,
		// `got := v[i]` aliases v's element slot — got's drop and v's element walk both
		// free the same variant data: a double-free that is silent for leaf enums but
		// segfaults for recursive ones (whose drop recurses into the freed buffer).
		// cloneResolvedValue deep-clones uniformly — recursive/container-bearing enums
		// via their synthesized clone (T1129), shallow-dup-safe enums via
		// dupEnumElementInPlace (T1110), clone-bearing enums via clone(). The flag is
		// set only when the var-decl/assign site consumes the index result, so the
		// owned clone is always moved into the binding — no orphan leak.
		if c.enumElemNeedsDupOnRead(resolvedElem) {
			c.dupHeapUserFieldAccess = false // consume the flag
			return c.cloneResolvedValue(val, resolvedElem)
		}
		// T1130: Map/Set element read-back from a Vector's native index. Map/Set are
		// deliberately excluded from isDroppableHeapUserType (T0440, because the Map/Set
		// container's own `[]` body dups V internally), so the heap-user branch above
		// skips them — but here Map/Set is the ELEMENT and the Vector's native `[]` does
		// NOT dup, so `got := v[i]` aliases the container's element. got's drop and the
		// vector's element walk would then free the same Map/Set → double-free.
		// cloneHeapElement deep-clones via the element's clone() (its type-arg safety
		// gating already covers recursive/container-bearing element types).
		if isMapOrSetType(resolvedElem) {
			if named := extractNamed(resolvedElem); named != nil {
				c.dupHeapUserFieldAccess = false // consume the flag
				return c.cloneHeapElement(val, resolvedElem, named)
			}
		}
		// T1287: bare structural-interface element read (`x := v[i]`). The native `[]`
		// returns the {vtable, instance} view aliasing the vector's element box; without
		// a clone, x's box and the vector's element walk (T1284) drop the same instance,
		// and dropping the old box on overwrite (`v[i] = w`, genVectorIndexAssign T1287)
		// leaves x dangling (UAF). cloneStructuralView deep-clones the box via
		// __promise_structural_clone (RTTI). Track the cloned instance as a heap temp so
		// the var-decl claims it (claimHeapTemp) and registers an RTTI free binding
		// (maybeRegisterStructuralFree) — mirrors the heap-user (T0398) / enum (T1129)
		// dup-on-read branches above.
		if isNonValueStructuralType(resolvedElem) {
			c.dupHeapUserFieldAccess = false // consume the flag
			dup := c.cloneStructuralView(val)
			instPtr := c.extractInstancePtr(dup)
			c.trackHeapTemp(instPtr, c.palFree)
			return dup
		}
	}

	// T0383: Dup-on-read for Vector[Vector|Channel|Arc|Weak] index access. The
	// var-decl path sets dupContainerFieldAccess for these types (mirrors B0219
	// for fields). Without dup, `t := vec[i]` aliases vec's element buffer and
	// drop-on-write at the same slot (vec[i] = X) would create a UAF through t.
	// Symmetric with the string branch (B0204) and tuple branch (T0370).
	// (Not borrow-gated — triggered by dupContainerFieldAccess flag set at the
	// var-decl AST site, not by a borrow type on the RHS. Remains active post-T0438.)
	if c.dupContainerFieldAccess && c.tempTrackingEnabled {
		// T1263: a vector element that is itself a value-copying container transitively
		// nesting a closure must NOT be duped — dupVector's element-clone loop zeroes each
		// closure's opaque env (T0813) → null {fn,env} → SEGV on invoke. Leave it ALIASED
		// (a borrow of the outer vector, env intact); the borrow gates
		// (isClosureAggregateBorrow / closureAggregateBorrowSource) suppress the owning
		// drop binding and reject escapes, keeping this in lockstep. FirstFieldNestedClosureDeep
		// treats a top-level container as a FIELD (recurses TypeArgs) yet keeps Ref/Weak/…
		// opaque, so Vector[Ref[…]] / Vector[int] are unaffected.
		resolvedContainerElem := elemType
		if c.typeSubst != nil {
			resolvedContainerElem = types.Substitute(elemType, c.typeSubst)
		}
		if sema.FirstFieldNestedClosureDeep(resolvedContainerElem) != nil {
			c.dupContainerFieldAccess = false // consume the flag
			return val
		}
		if innerElem, isVec := types.AsVector(elemType); isVec {
			c.dupContainerFieldAccess = false // consume the flag
			innerLLVM := c.resolveType(innerElem)
			innerSize := int64(c.typeSize(innerLLVM))
			dup := c.dupVector(val, innerSize)
			c.emitVectorElementCloneLoop(dup, innerElem)
			c.trackVectorTemp(dup)
			return dup
		}
		if extractNamed(elemType) == types.TypVector {
			c.dupContainerFieldAccess = false
			dup := c.dupVector(val, 0)
			c.trackVectorTemp(dup)
			return dup
		}
		if chElem, isCh := types.AsChannel(elemType); isCh {
			c.dupContainerFieldAccess = false
			dup := c.dupChannel(val)
			c.trackChannelTempWithElemType(dup, chElem) // T0663
			return dup
		}
		if arcElem, isArc := types.AsArc(elemType); isArc {
			c.dupContainerFieldAccess = false
			resolvedArcElem := arcElem
			if c.typeSubst != nil {
				resolvedArcElem = types.Substitute(arcElem, c.typeSubst)
			}
			dup := c.dupArc(val, resolvedArcElem)
			c.trackTempWithDrop(dup, c.getOrCreateArcDrop(resolvedArcElem))
			return dup
		}
		if weakElem, isWeak := types.AsWeak(elemType); isWeak {
			c.dupContainerFieldAccess = false
			resolvedWeakElem := weakElem
			if c.typeSubst != nil {
				resolvedWeakElem = types.Substitute(weakElem, c.typeSubst)
			}
			dup := c.dupWeak(val, resolvedWeakElem)
			c.trackTempWithDrop(dup, c.getOrCreateWeakDrop(resolvedWeakElem))
			return dup
		}
	}

	// T0620: Dup-on-read for Vector[T?] where T is droppable. The var-decl path
	// sets dup flags for Optional[string/Vector/Channel/Arc/Weak/heap-user/tuple]
	// (stmt.go:726-794), but the bare-type branches above don't match Optional
	// element types. When we detect an Optional with droppable inner and a matching
	// flag, consume the flag and deep-dup the inner value so the variable owns an
	// independent copy — preventing double-free between the variable's Optional
	// drop binding and the vector's element drop loop (now enabled by Gap B fix).
	if opt, ok := elemType.(*types.Optional); ok && c.tempTrackingEnabled {
		innerElem := opt.Elem()
		if c.typeSubst != nil {
			innerElem = types.Substitute(innerElem, c.typeSubst)
		}
		if c.typeNeedsFieldDrop(innerElem) {
			flagConsumed := false
			if c.dupStringFieldAccess && extractNamed(innerElem) == types.TypString {
				c.dupStringFieldAccess = false
				flagConsumed = true
			} else if c.dupContainerFieldAccess {
				if types.IsVector(innerElem) || types.IsChannel(innerElem) ||
					types.IsArc(innerElem) || types.IsWeak(innerElem) ||
					extractNamed(innerElem) == types.TypVector ||
					extractNamed(innerElem) == types.TypChannel {
					c.dupContainerFieldAccess = false
					flagConsumed = true
				}
			} else if c.dupTupleFieldAccess {
				if _, isTup := innerElem.(*types.Tuple); isTup {
					c.dupTupleFieldAccess = false
					flagConsumed = true
				}
			} else if c.dupHeapUserFieldAccess {
				// T1291: a non-value structural inner boxes a heap instance (not
				// matched by isDroppableHeapUserType, which excludes structural). Its
				// element drop is now active (T1291), so an aliased read into an owning
				// Optional local would double-free — deep-clone the box on read via
				// dupOptionalVectorElem's structural case.
				if isDroppableHeapUserType(innerElem) || isHeapUserNoDropPalFree(innerElem) ||
					isNonValueStructuralType(innerElem) {
					c.dupHeapUserFieldAccess = false
					flagConsumed = true
				}
			}
			if flagConsumed {
				return c.dupOptionalVectorElem(val, opt, innerElem)
			}
		}
	}

	return val
}
