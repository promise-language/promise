package codegen

import (
	"fmt"

	"github.com/llir/llvm/ir/constant"
	"github.com/llir/llvm/ir/enum"
	irtypes "github.com/llir/llvm/ir/types"
	"github.com/llir/llvm/ir/value"

	"github.com/promise-language/promise/compiler/internal/ast"
	"github.com/promise-language/promise/compiler/internal/types"
)

// --- Increment / Decrement ---

// genIncDecStmt generates code for x++ or x-- statements.
func (c *Compiler) genIncDecStmt(s *ast.IncDecStmt) {
	c.genIncDecTarget(s.Target, s.IsInc)
}

// genIncDecTarget applies ++ or -- to the given expression target.
func (c *Compiler) genIncDecTarget(target ast.Expr, isInc bool) {
	op := "++"
	if !isInc {
		op = "--"
	}
	targetType := c.info.Types[target]
	if c.typeSubst != nil {
		targetType = types.Substitute(targetType, c.typeSubst)
	}
	if c.selfSubst != nil {
		targetType = types.SubstituteSelf(targetType, c.selfSubst.iface, c.selfSubst.concrete)
	}
	// targetType may be a primitive (native ++/--), a Named/value/enum user type
	// (non-native dispatch, T0880), or an enum (extractNamed returns nil). Dispatch
	// is handled per-target via emitUnaryOpResult, so no top-level Named is needed.

	switch t := target.(type) {
	case *ast.IdentExpr:
		alloca, ok := c.locals[t.Name]
		if !ok {
			panic(fmt.Sprintf("codegen: undefined variable %q in inc/dec", t.Name))
		}
		current := c.block.NewLoad(alloca.ElemType, alloca)
		result := c.emitUnaryOpResult(op, targetType, current, false)
		// T0880: x++ is `x = x.++()`. A non-native operator returns a NEW value,
		// so the old heap-owned value leaks unless dropped (zero-leak policy).
		// T0959: the old value may only be dropped when this local actually OWNS
		// its binding, so the drop-old is gated behind the runtime drop flag.
		// T1194: route through the flag-aware slot helper so a borrow-by-default
		// heap param (flag 0, caller-owned original) is not double-freed and the
		// fresh result is tracked for drop at scope exit. The helper also strips a
		// reference wrapper off targetType so a moved ref-typed local
		// (`Counter &r = owner;`, flag 1) drops its owned instance instead of
		// no-oping (which would leak it).
		c.dropOldUserValueAtIdentSlot(t.Name, alloca, targetType, result)
		c.block.NewStore(result, alloca)
	case *ast.MemberExpr:
		// T0712: property getter/setter dispatch. genFieldPtr panics ("no field")
		// for a property with no backing field; read via the getter, apply the op,
		// and write via the setter — mirroring genMemberAssign's compound path.
		recvType := c.info.Types[t.Target]
		if c.typeSubst != nil {
			recvType = types.Substitute(recvType, c.typeSubst)
		}
		if recvNamed := extractNamed(recvType); recvNamed != nil {
			if setter := recvNamed.LookupSetter(t.Field); setter != nil {
				getter := recvNamed.LookupGetter(t.Field)
				if getter == nil {
					panic(fmt.Sprintf("codegen: inc/dec on property %s.%s but no getter found", recvNamed, t.Field))
				}
				current := c.genGetterCall(t, recvType, recvNamed, getter)
				// A failable getter returns {i1, T, i8*}; unwrap + propagate the
				// error. T0923: detect failability from the getter signature, NOT
				// the result shape — since T0880 a non-native ++/-- operand can be
				// a user type whose Value struct is itself a *StructType, so the old
				// "struct ⇒ failable" heuristic misfired on non-failable user-type
				// getters. Mirrors the index path's indexMethod.Sig().CanError().
				// genSetterCall already auto-propagates the setter result via
				// propagateIfFailable (T0708).
				if getter.Sig().CanError() {
					current = c.genAutoPropagateValue(current)
				}
				result := c.emitUnaryOpResult(op, targetType, current, false)
				// Drop-old is handled inside the setter (it assigns the backing
				// field via genMemberAssign, which drops the old value).
				c.genSetterCall(t, recvType, recvNamed, setter, result)
				return
			}
		}
		// Load field, apply op, store back
		fieldPtr := c.genFieldPtr(t)
		llvmType := c.resolveType(targetType)
		current := c.block.NewLoad(llvmType, fieldPtr)
		result := c.emitUnaryOpResult(op, targetType, current, false)
		// T0880: drop the old heap-owned field value before overwriting it.
		c.dropOldUserValueAtPtr(fieldPtr, targetType, result)
		c.block.NewStore(result, fieldPtr)
	case *ast.IndexExpr:
		indexTargetType := c.info.Types[t.Target]
		if c.typeSubst != nil {
			indexTargetType = types.Substitute(indexTargetType, c.typeSubst)
		}
		indexNamed := extractNamed(indexTargetType)
		if indexNamed == nil {
			panic(fmt.Sprintf("codegen: inc/dec on index of unresolved type %s", indexTargetType))
		}
		indexMethod := indexNamed.LookupMethod("[]")
		assignMethod := indexNamed.LookupMethod("[]=")

		if indexMethod != nil && indexMethod.IsNative() && assignMethod != nil && assignMethod.IsNative() {
			// Native path: direct memory access (vectors)
			elem, ok := types.AsVector(indexTargetType)
			if !ok && indexNamed == types.TypVector && c.typeSubst != nil {
				tp := indexNamed.TypeParams()[0]
				elem, ok = c.typeSubst[tp], c.typeSubst[tp] != nil
			}
			if !ok {
				panic(fmt.Sprintf("codegen: inc/dec on index of non-vector native type %s", indexTargetType))
			}
			slicePtr := c.genExpr(t.Target)
			idx := c.genExpr(t.Index)
			elemLLVM := c.resolveType(elem)
			elemSize := int64(c.typeSize(elemLLVM))

			// COW: if static (.rodata), copy to heap first (T0062)
			cowSlice := c.block.NewCall(c.funcs["promise_vector_cow"],
				slicePtr, constant.NewInt(irtypes.I64, elemSize))
			c.storeBackSlicePtr(t.Target, cowSlice)

			headerType := vectorHeaderType()
			headerPtr := c.block.NewBitCast(cowSlice, irtypes.NewPointer(headerType))
			length := loadVectorLen(c.block, headerPtr)
			inBounds := c.block.NewICmp(enum.IPredULT, idx, length)
			okBlock := c.newBlock("incdec.index.ok")
			panicBlock := c.newBlock("incdec.index.oob")
			c.block.NewCondBr(inBounds, okBlock, panicBlock)

			c.block = panicBlock
			oobMsg := c.makeGlobalString("index out of bounds")
			c.block.NewCall(c.funcs["promise_panic"], oobMsg)
			c.emitPanicReturn()

			c.block = okBlock
			dataBase := c.block.NewGetElementPtr(irtypes.I8, cowSlice,
				constant.NewInt(irtypes.I64, int64(vectorHeaderSize)))
			dataTypedPtr := c.block.NewBitCast(dataBase, irtypes.NewPointer(elemLLVM))
			elemPtr := c.block.NewGetElementPtr(elemLLVM, dataTypedPtr, idx)
			current := c.block.NewLoad(elemLLVM, elemPtr)
			result := c.emitUnaryOpResult(op, targetType, current, false)
			// T0880: drop the old heap-owned element value before overwriting it.
			c.dropOldUserValueAtPtr(elemPtr, targetType, result)
			c.block.NewStore(result, elemPtr)
		} else if indexMethod != nil && assignMethod != nil {
			// Non-native: read via [], apply op, write via []=
			typeName := c.resolveTypeName(indexTargetType)
			getFnName := mangleMethodName(typeName, "[]", false)
			getFn, ok := c.funcs[getFnName]
			if !ok {
				panic(fmt.Sprintf("codegen: undeclared [] method %s", getFnName))
			}
			setFnName := mangleMethodName(typeName, "[]=", false)
			setFn, ok := c.funcs[setFnName]
			if !ok {
				panic(fmt.Sprintf("codegen: undeclared []= method %s", setFnName))
			}
			targetVal := c.genExpr(t.Target)
			keyVal := c.genExpr(t.Index)
			var instancePtr value.Value
			if isContainerType(indexTargetType) {
				instancePtr = targetVal
			} else {
				instancePtr = c.extractInstancePtr(targetVal)
			}
			// Read, inc/dec, write
			var optVal value.Value = c.block.NewCall(getFn, instancePtr, keyVal)
			if indexMethod.Sig().CanError() { // T0709: failable [] read propagates
				optVal = c.genAutoPropagateValue(optVal)
			}
			var current value.Value
			if _, isOpt := indexMethod.Sig().Result().(*types.Optional); isOpt {
				hasVal := c.block.NewExtractValue(optVal, 0)
				okBlock := c.newBlock("incdec.method.ok")
				panicBlock := c.newBlock("incdec.method.panic")
				c.block.NewCondBr(hasVal, okBlock, panicBlock)

				c.block = panicBlock
				panicMsg := c.makeGlobalString("inc/dec on missing key")
				c.block.NewCall(c.funcs["promise_panic"], panicMsg)
				c.emitPanicReturn()

				c.block = okBlock
				current = c.block.NewExtractValue(optVal, 1)
			} else {
				current = optVal
			}
			result := c.emitUnaryOpResult(op, targetType, current, false)
			// Drop-old is handled inside the []= setter (Map.[]= drops the old
			// slot value before storing), so no explicit drop here.
			setCall := c.block.NewCall(setFn, instancePtr, keyVal, result)
			c.propagateIfFailable(setCall) // T0708
		} else {
			panic(fmt.Sprintf("codegen: inc/dec on index of type %s without []/[]= methods", indexTargetType))
		}
	default:
		panic(fmt.Sprintf("codegen: unsupported inc/dec target %T", target))
	}
}
