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

// --- Binary expressions ---

// trackOperatorResult registers the result of a non-native user-defined operator
// call as a heap temp (T0918) so an inline (unbound) heap user-type result is
// dropped at statement end — exactly like an ordinary method-call result.
// trackHeapUserTypeResult self-filters: it ignores scalars, value types, copy
// types, structural/container/string results, so this is a no-op for native
// operator results (scalars / value-type structs). Borrow returns (T&/T~) are
// skipped — they are never owned temps (mirrors the T0649 CallExpr guard).
// Returns result unchanged for use as a tail call.
//
// Tracking is placed at the operator-call sites rather than in genExpr's
// BinaryExpr/UnaryExpr dispatch so the early-return special forms (short-circuit
// &&/||, elvis ?:, ranges) are never tracked — their results may alias owned
// locals (e.g. the default operand of `optional ?: owned_local`), and tracking
// those would double-free at statement end.
func (c *Compiler) trackOperatorResult(e ast.Expr, result value.Value) value.Value {
	rt := c.resolvedExprType(e)
	if rt != nil && isRefType(rt) {
		return result
	}
	c.trackHeapUserTypeResult(e, result)
	return result
}

func (c *Compiler) genBinaryExpr(e *ast.BinaryExpr) value.Value {
	// Short-circuit and special operators at the AST level
	switch e.Op {
	case ast.BinAnd:
		return c.genShortCircuitAnd(e)
	case ast.BinOr:
		return c.genShortCircuitOr(e)
	case ast.BinElvis:
		return c.genElvis(e)
	case ast.BinExclusiveRange, ast.BinInclusiveRange:
		return c.genRange(e)
	}

	// Type-system-driven path
	left := c.genExprAutoPropagate(e.Left)
	right := c.genExprAutoPropagate(e.Right)

	leftType := c.info.Types[e.Left]
	if c.typeSubst != nil {
		leftType = types.Substitute(leftType, c.typeSubst)
	}
	if c.selfSubst != nil {
		leftType = types.SubstituteSelf(leftType, c.selfSubst.iface, c.selfSubst.concrete)
	}
	named := extractNamed(leftType)
	if named == nil {
		if en := extractEnum(leftType); en != nil {
			// T0918: track the heap user-type result for inline (unbound) use.
			return c.trackOperatorResult(e, c.genEnumBinaryOp(e, en, leftType, left, right))
		}
		panic(fmt.Sprintf("codegen: cannot resolve Named type from %s for operator %s", leftType, e.Op))
	}

	op := e.Op.String()
	// Binary operator: select the 1-param variant so a type that also declares a
	// prefix-unary form of the same symbol (e.g. `-`) dispatches correctly (T0883).
	method := named.LookupBinaryMethod(op)
	if method == nil {
		method = named.LookupMethod(op)
	}
	if method == nil {
		panic(fmt.Sprintf("codegen: no method %q on type %s", op, named))
	}

	if method.IsNative() {
		// String operators dispatch to runtime intrinsics
		if named == types.TypString {
			return c.genStringOp(op, left, right)
		}
		return c.emitNativeOp(named, op, left, right)
	}

	// Non-native operator: dispatch as a method call.
	// Virtual dispatch when the type has a vtable (abstract/structural type or type with children).
	if c.needsVtable(named) {
		// T0918: track the heap user-type result for inline (unbound) use.
		return c.trackOperatorResult(e, c.genVirtualBinaryOp(e, named, method, left, right))
	}

	// Direct dispatch: call the concrete type's operator method (or the parent's,
	// when the operator is inherited — see resolveDirectDispatchOwner).
	mangledName := mangleMethodName(c.resolveDirectDispatchOwner(named, leftType, op), op, false)
	fn, ok := c.funcs[mangledName]
	if !ok {
		panic(fmt.Sprintf("codegen: undeclared operator method %s", mangledName))
	}

	var args []value.Value
	if method.Sig().Recv() != nil {
		if isThisReceiver(e.Left) {
			args = append(args, left)
		} else if named.IsValueType() {
			args = append(args, c.valueTypeReceiverPtr(left, leftType))
		} else {
			args = append(args, c.extractInstancePtr(left))
		}
	}
	// If right came from genThisExpr() (returns i8* receiver ptr) but the method expects a
	// value struct, wrap it as {null_vtable, instance_ptr}. This happens in synthesized default
	// method bodies like Priority.> containing "other < this", where 'this' appears as an
	// argument rather than the receiver.
	if isThisReceiver(e.Right) {
		var paramIdx int
		if method.Sig().Recv() != nil {
			paramIdx = 1
		}
		if paramIdx < len(fn.Params) {
			if st, ok := fn.Params[paramIdx].Typ.(*irtypes.StructType); ok {
				if _, rightIsPtr := right.Type().(*irtypes.PointerType); rightIsPtr {
					rightType := c.info.Types[e.Right]
					if c.typeSubst != nil {
						rightType = types.Substitute(rightType, c.typeSubst)
					}
					if c.selfSubst != nil {
						rightType = types.SubstituteSelf(rightType, c.selfSubst.iface, c.selfSubst.concrete)
					}
					rightNamed := extractNamed(rightType)
					if rightNamed != nil && rightNamed.IsValueType() {
						// Value-type `this`: the receiver i8* points at the value
						// struct itself (see valueTypeReceiverPtr), so load the
						// param directly rather than synthesizing {vtable, instance}.
						valPtr := c.block.NewBitCast(right, irtypes.NewPointer(st))
						right = c.block.NewLoad(st, valPtr)
					} else {
						// Heap type: `this` i8* IS the instance pointer; wrap it
						// as {null_vtable, instance_ptr}.
						alloca := c.createEntryAlloca(st)
						vtableField := c.block.NewGetElementPtr(st, alloca,
							constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
						c.block.NewStore(constant.NewNull(irtypes.I8Ptr), vtableField)
						instField := c.block.NewGetElementPtr(st, alloca,
							constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 1))
						c.block.NewStore(right, instField)
						right = c.block.NewLoad(st, alloca)
					}
				}
			}
		}
	}
	args = append(args, right)
	result := value.Value(c.block.NewCall(fn, args...))
	if method.Sig().CanError() {
		// T0984: failable operator returns {ok, value, err}; unwrap and propagate
		// the error to the (sema-guaranteed failable) enclosing scope.
		result = c.genAutoPropagateValue(result)
	}
	// T0918: track the heap user-type result for inline (unbound) use.
	return c.trackOperatorResult(e, result)
}

// genEnumBinaryOp dispatches a user-defined binary operator declared on an enum
// (T0876). Enum operator methods receive the enum value via an i8* pointer
// (mirroring genEnumMethodCall), so the Named-type arg convention in
// genBinaryExpr does not apply.
func (c *Compiler) genEnumBinaryOp(e *ast.BinaryExpr, en *types.Enum, leftType types.Type, left, right value.Value) value.Value {
	op := e.Op.String()

	// Resolve the enum's mangled name (mono name for instances, monoCtx for
	// the origin enum inside a generic method body).
	enumName := en.Obj().Name()
	if inst, ok := leftType.(*types.Instance); ok {
		if _, ok := inst.Origin().(*types.Enum); ok {
			enumName = monoName(inst)
		}
	} else if c.monoCtx != nil {
		if origin, ok := c.monoCtx.origin.(*types.Enum); ok && en == origin {
			enumName = c.monoCtx.name
		}
	}

	// Binary operator: select the 1-param variant (T0883).
	method := en.LookupBinaryMethod(op)
	if method == nil {
		method = en.LookupMethod(op)
	}
	if method == nil {
		panic(fmt.Sprintf("codegen: no operator %q on enum %s", op, enumName))
	}
	mangledName := mangleMethodName(enumName, op, false)
	fn, ok := c.funcs[mangledName]
	if !ok {
		panic(fmt.Sprintf("codegen: undeclared enum operator method %s", mangledName))
	}

	// Receiver: pass an i8* pointer to the enum value.
	var args []value.Value
	if isThisReceiver(e.Left) {
		// `this` inside an enum method is already i8* pointing to the enum alloca.
		args = append(args, left)
	} else {
		alloca := c.entryBlock.NewAlloca(left.Type())
		alloca.SetName(c.uniqueLocalName("enum.this"))
		c.block.NewStore(left, alloca)
		args = append(args, c.block.NewBitCast(alloca, irtypes.I8Ptr))
	}

	// Operand: the method expects the enum value by value. If the right operand
	// is `this` (i8* receiver pointer), load the enum value from it.
	if isThisReceiver(e.Right) {
		if len(fn.Params) > 1 {
			if _, rightIsPtr := right.Type().(*irtypes.PointerType); rightIsPtr {
				valPtr := c.block.NewBitCast(right, irtypes.NewPointer(fn.Params[1].Typ))
				right = c.block.NewLoad(fn.Params[1].Typ, valPtr)
			}
		}
	}
	args = append(args, right)
	result := value.Value(c.block.NewCall(fn, args...))
	if method.Sig().CanError() {
		// T0984: unwrap the failable {ok, value, err} result and propagate the error.
		result = c.genAutoPropagateValue(result)
	}
	return result
}

// genNonNativeEnumCompoundOp dispatches a user-defined enum operator invoked by a
// compound assignment (`+=`, `-=`, etc.) where the operand type is an enum (T1015).
// Both operands are plain loaded values — neither is `this` — so this mirrors the
// non-`this` branch of genEnumBinaryOp combined with genNonNativeCompoundOp's
// failable handling. The result is NOT tracked as a statement temp: every
// genCompoundOp caller stores it into a location that takes ownership, so tracking
// here would double-free (matching genNonNativeCompoundOp's contract). A failable
// operator returns {ok, value, err}; the error is auto-propagated (sema guarantees
// the enclosing scope is failable via compoundOperatorCanError).
func (c *Compiler) genNonNativeEnumCompoundOp(en *types.Enum, operandType types.Type,
	op string, current, val value.Value) value.Value {

	// Resolve the enum's mangled name (mono name for instances, monoCtx for the
	// origin enum inside a generic method body) — same scheme as genEnumBinaryOp.
	enumName := en.Obj().Name()
	if inst, ok := operandType.(*types.Instance); ok {
		if _, ok := inst.Origin().(*types.Enum); ok {
			enumName = monoName(inst)
		}
	} else if c.monoCtx != nil {
		if origin, ok := c.monoCtx.origin.(*types.Enum); ok && en == origin {
			enumName = c.monoCtx.name
		}
	}

	// Binary operator: prefer the 1-param variant (T0883).
	method := en.LookupBinaryMethod(op)
	if method == nil {
		method = en.LookupMethod(op)
	}
	if method == nil {
		panic(fmt.Sprintf("codegen: no operator %q on enum %s for compound assignment", op, enumName))
	}
	mangledName := mangleMethodName(enumName, op, false)
	fn, ok := c.funcs[mangledName]
	if !ok {
		panic(fmt.Sprintf("codegen: undeclared enum operator method %s", mangledName))
	}

	// Receiver: pass an i8* pointer to the enum value (neither operand is `this`).
	alloca := c.entryBlock.NewAlloca(current.Type())
	alloca.SetName(c.uniqueLocalName("enum.this"))
	c.block.NewStore(current, alloca)
	args := []value.Value{c.block.NewBitCast(alloca, irtypes.I8Ptr)}
	// Operand: the method expects the enum value by value.
	args = append(args, val)

	result := value.Value(c.block.NewCall(fn, args...))
	if method.Sig().CanError() {
		result = c.genAutoPropagateValue(result)
	}
	return result
}

// genVirtualBinaryOp dispatches a non-native binary operator through the vtable.
// Used when the static type is abstract or has children requiring virtual dispatch.
// Mirrors genVirtualMethodCall but uses pre-evaluated left/right operands.
func (c *Compiler) genVirtualBinaryOp(e *ast.BinaryExpr, named *types.Named,
	method *types.Method, left, right value.Value) value.Value {
	result := c.genVirtualBinaryOpValues(named, e.Op.String(), method, left, right, isThisReceiver(e.Left))
	if method.Sig().CanError() {
		// T0984: unwrap the failable {ok, value, err} result and propagate the
		// error. Done here (not in genVirtualBinaryOpValues, which is shared with
		// genNonNativeCompoundOp's own auto-propagate) to avoid double-unwrapping.
		result = c.genAutoPropagateValue(result)
	}
	return result
}

// genVirtualBinaryOpValues is the value-based core of genVirtualBinaryOp: it
// dispatches a non-native binary operator through the vtable given pre-evaluated
// operands. leftIsThis reports whether the left operand is the method receiver
// (`this`). Shared by genBinaryExpr (plain `a + b`) and genNonNativeCompoundOp
// (compound `a += b`, where neither operand is `this`), so the vtable dispatch
// logic lives in one place (T0715).
func (c *Compiler) genVirtualBinaryOpValues(named *types.Named, op string,
	method *types.Method, left, right value.Value, leftIsThis bool) value.Value {

	// Extract vtable and instance from left operand
	var vtableRaw, instance value.Value
	if leftIsThis {
		instance = left
		vtableRaw = c.loadVtablePtrFromInstance(left)
	} else {
		vtableRaw = c.extractVtablePtr(left)
		instance = c.extractInstancePtr(left)
	}

	// Index into vtable
	slotIndex := named.VirtualMethodIndex(op, false)
	if slotIndex < 0 {
		panic(fmt.Sprintf("codegen: operator %s not in vtable for %s", op, named))
	}
	vtablePtr := c.block.NewBitCast(vtableRaw, irtypes.NewPointer(irtypes.I8Ptr))
	fnSlotPtr := c.block.NewGetElementPtr(irtypes.I8Ptr, vtablePtr,
		constant.NewInt(irtypes.I32, int64(slotIndex)))
	fnRaw := c.block.NewLoad(irtypes.I8Ptr, fnSlotPtr)

	// Build the function type and bitcast
	retType := irtypes.Type(irtypes.Void)
	if method.Sig().Result() != nil {
		retType = c.resolveType(method.Sig().Result())
	}
	if method.Sig().CanError() {
		retType = computeResultType(retType)
	}
	var paramTypes []irtypes.Type
	if method.Sig().Recv() != nil {
		paramTypes = append(paramTypes, irtypes.I8Ptr)
	}
	for _, p := range method.Sig().Params() {
		paramTypes = append(paramTypes, c.resolveType(p.Type()))
	}
	funcType := irtypes.NewFunc(retType, paramTypes...)
	fnTyped := c.block.NewBitCast(fnRaw, irtypes.NewPointer(funcType))

	// Call with instance ptr + right operand
	var args []value.Value
	if method.Sig().Recv() != nil {
		args = append(args, instance)
	}
	args = append(args, right)
	return c.block.NewCall(fnTyped, args...)
}

// genNonNativeCompoundOp dispatches a user-defined (non-native) binary operator
// invoked by a compound assignment (`+=`, `-=`, etc.) and returns the result
// value (T0715). Both operands are plain loaded values — neither is `this` — so
// the receiver/argument ABI is simpler than genBinaryExpr's: no `this`-receiver
// or `this`-argument special cases. The result is NOT tracked as a statement
// temp; every genCompoundOp caller stores it into a location that takes ownership
// (alloca, field, setter, or container slot), so tracking here would double-free
// (matching the native-path comment in genCompoundOp). A failable operator
// returns {ok, value, err}; the error is auto-propagated (sema guarantees the
// enclosing scope is failable via compoundOperatorCanError).
func (c *Compiler) genNonNativeCompoundOp(named *types.Named, operandType types.Type,
	method *types.Method, op string, current, val value.Value) value.Value {

	var result value.Value
	if c.needsVtable(named) {
		// Virtual dispatch when the operand type is abstract / structural / has
		// children. Neither operand is `this`.
		result = c.genVirtualBinaryOpValues(named, op, method, current, val, false)
	} else {
		// Direct dispatch: resolve the mangled name exactly as genBinaryExpr does
		// (mono name for generic instances, structural-default synthesis under the
		// concrete name, mono-parent resolution for inherited operators).
		mangledName := mangleMethodName(c.resolveDirectDispatchOwner(named, operandType, op), op, false)
		fn, ok := c.funcs[mangledName]
		if !ok {
			panic(fmt.Sprintf("codegen: undeclared operator method %s", mangledName))
		}

		var args []value.Value
		if method.Sig().Recv() != nil {
			if named.IsValueType() {
				args = append(args, c.valueTypeReceiverPtr(current, operandType))
			} else {
				args = append(args, c.extractInstancePtr(current))
			}
		}
		args = append(args, val)
		result = c.block.NewCall(fn, args...)
	}

	if method.Sig().CanError() {
		// Failable operator: unwrap the {ok, value, err} result, propagating the
		// error to the (sema-guaranteed failable) enclosing scope.
		result = c.genAutoPropagateValue(result)
	}
	return result
}

// genStringOp dispatches a string binary operator to the appropriate runtime intrinsic.
func (c *Compiler) genStringOp(op string, left, right value.Value) value.Value {
	switch op {
	case "+":
		return c.block.NewCall(c.funcs["promise_string_concat"], left, right)
	case "==":
		return c.block.NewCall(c.funcs["promise_string_eq"], left, right)
	case "!=":
		eq := c.block.NewCall(c.funcs["promise_string_eq"], left, right)
		return c.block.NewXor(eq, constant.NewInt(irtypes.I1, 1))
	case "<":
		cmp := c.block.NewCall(c.funcs["promise_string_compare"], left, right)
		return c.block.NewICmp(enum.IPredSLT, cmp, constant.NewInt(irtypes.I32, 0))
	case ">":
		cmp := c.block.NewCall(c.funcs["promise_string_compare"], left, right)
		return c.block.NewICmp(enum.IPredSGT, cmp, constant.NewInt(irtypes.I32, 0))
	case "<=":
		cmp := c.block.NewCall(c.funcs["promise_string_compare"], left, right)
		return c.block.NewICmp(enum.IPredSLE, cmp, constant.NewInt(irtypes.I32, 0))
	case ">=":
		cmp := c.block.NewCall(c.funcs["promise_string_compare"], left, right)
		return c.block.NewICmp(enum.IPredSGE, cmp, constant.NewInt(irtypes.I32, 0))
	default:
		panic(fmt.Sprintf("codegen: string operator %q not yet implemented", op))
	}
}

// --- Unary expressions ---

func (c *Compiler) genUnaryExpr(e *ast.UnaryExpr) value.Value {
	// Intercept receive operator (<-task) before normal unary dispatch
	if e.Op == ast.UnaryReceive {
		return c.genReceiveExpr(e)
	}

	operand := c.genExprAutoPropagate(e.Operand)
	operandType := c.info.Types[e.Operand]
	if c.typeSubst != nil {
		operandType = types.Substitute(operandType, c.typeSubst)
	}
	if c.selfSubst != nil {
		operandType = types.SubstituteSelf(operandType, c.selfSubst.iface, c.selfSubst.concrete)
	}

	// T0918: track the heap user-type result for inline (unbound) use. Placed in
	// genUnaryExpr (not emitUnaryOpResult, which is shared with genIncDecTarget's
	// ++/-- statement targets) so only prefix-unary expression results are
	// tracked. The receive operator (<-) returned above and is never reached.
	return c.trackOperatorResult(e, c.emitUnaryOpResult(e.Op.String(), operandType, operand, isThisReceiver(e.Operand)))
}

// emitUnaryOpResult dispatches a unary operator (prefix `-`/`!`/`~` from
// genUnaryExpr, or `++`/`--` from genIncDecTarget) on operandType and returns
// the result value. isThis reports whether the operand is the method receiver
// (`this`). Centralizes the native/enum/virtual/direct dispatch so both the
// prefix-unary path (T0878) and the inc/dec path (T0880) share one
// implementation.
func (c *Compiler) emitUnaryOpResult(op string, operandType types.Type, operand value.Value, isThis bool) value.Value {
	named := extractNamed(operandType)
	if named == nil {
		// Enum operands dispatch via the i8*-receiver convention (T0878),
		// mirroring genEnumBinaryOp.
		if en := extractEnum(operandType); en != nil {
			return c.genEnumUnaryOp(op, en, operandType, operand, isThis)
		}
		panic(fmt.Sprintf("codegen: cannot resolve Named type from %s for unary %s", operandType, op))
	}

	// For unary ops, look up the 0-param method variant
	method := c.lookupUnaryMethod(named, op)
	if method == nil {
		panic(fmt.Sprintf("codegen: no unary method %q on type %s", op, named))
	}

	if method.IsNative() {
		return c.emitNativeOp(named, op, operand, nil)
	}

	// Non-native unary operator: dispatch as a method call (T0878), mirroring
	// genBinaryExpr's receiver handling but with no second operand.
	var result value.Value
	if c.needsVtable(named) {
		result = c.genVirtualUnaryOp(op, named, method, operand, isThis)
	} else {
		// Direct dispatch: call the concrete type's operator method. Resolve the
		// mangled name exactly as genBinaryExpr does (mono name for generic
		// instances, structural-default synthesis under the concrete name).
		// mangleMethodNameForMethod keeps the "$unary" discriminator (T0883).
		mangledName := mangleMethodNameForMethod(
			c.resolveDirectDispatchOwner(named, operandType, op), method)
		fn, ok := c.funcs[mangledName]
		if !ok {
			panic(fmt.Sprintf("codegen: undeclared operator method %s", mangledName))
		}

		var args []value.Value
		if method.Sig().Recv() != nil {
			if isThis {
				args = append(args, operand)
			} else if named.IsValueType() {
				args = append(args, c.valueTypeReceiverPtr(operand, operandType))
			} else {
				args = append(args, c.extractInstancePtr(operand))
			}
		}
		result = c.block.NewCall(fn, args...)
	}

	if method.Sig().CanError() {
		// T0984: failable unary/inc-dec operator returns {ok, value, err}; unwrap
		// and propagate the error to the (sema-guaranteed failable) enclosing scope.
		// Shared by prefix `-`/`!`/`~` (genUnaryExpr) and `++`/`--` (genIncDecTarget).
		result = c.genAutoPropagateValue(result)
	}
	return result
}

// genEnumUnaryOp dispatches a user-defined unary operator declared on an enum
// (T0878 prefix, T0880 ++/--). Enum operator methods receive the enum value via
// an i8* pointer (mirroring genEnumBinaryOp), so the Named-type receiver
// convention in emitUnaryOpResult does not apply. isThis reports whether the
// operand is the method receiver.
func (c *Compiler) genEnumUnaryOp(op string, en *types.Enum, operandType types.Type, operand value.Value, isThis bool) value.Value {
	// Resolve the enum's mangled name (mono name for instances, monoCtx for the
	// origin enum inside a generic method body).
	enumName := en.Obj().Name()
	if inst, ok := operandType.(*types.Instance); ok {
		if _, ok := inst.Origin().(*types.Enum); ok {
			enumName = monoName(inst)
		}
	} else if c.monoCtx != nil {
		if origin, ok := c.monoCtx.origin.(*types.Enum); ok && en == origin {
			enumName = c.monoCtx.name
		}
	}

	method := en.LookupUnaryMethod(op)
	if method == nil {
		panic(fmt.Sprintf("codegen: no operator %q on enum %s", op, enumName))
	}
	mangledName := mangleMethodNameForMethod(enumName, method)
	fn, ok := c.funcs[mangledName]
	if !ok {
		panic(fmt.Sprintf("codegen: undeclared enum operator method %s", mangledName))
	}

	// Receiver: pass an i8* pointer to the enum value. The original binding still
	// owns/drops its data — this borrow matches genEnumBinaryOp's convention.
	var args []value.Value
	if isThis {
		args = append(args, operand)
	} else {
		alloca := c.entryBlock.NewAlloca(operand.Type())
		alloca.SetName(c.uniqueLocalName("enum.this"))
		c.block.NewStore(operand, alloca)
		args = append(args, c.block.NewBitCast(alloca, irtypes.I8Ptr))
	}
	result := value.Value(c.block.NewCall(fn, args...))
	if method.Sig().CanError() {
		// T0984: unwrap the failable {ok, value, err} result and propagate the error.
		result = c.genAutoPropagateValue(result)
	}
	return result
}

// genVirtualUnaryOp dispatches a non-native unary operator through the vtable
// (T0878 prefix, T0880 ++/--). Used when the static type is abstract or has
// children requiring virtual dispatch. Mirrors genVirtualBinaryOp without a
// right operand. isThis reports whether the operand is the method receiver.
func (c *Compiler) genVirtualUnaryOp(op string, named *types.Named,
	method *types.Method, operand value.Value, isThis bool) value.Value {

	// Extract vtable and instance from the operand.
	var vtableRaw, instance value.Value
	if isThis {
		instance = operand
		vtableRaw = c.loadVtablePtrFromInstance(operand)
	} else {
		vtableRaw = c.extractVtablePtr(operand)
		instance = c.extractInstancePtr(operand)
	}

	// Index into the vtable via the method's own slot. For `-`/`!`/`~` this is the
	// unary ($unary) slot — distinct from the binary `-` slot (T0883); for
	// `++`/`--` it is the plain operator slot (T0880).
	slotIndex := named.VirtualSlotIndexForMethod(method)
	if slotIndex < 0 {
		panic(fmt.Sprintf("codegen: operator %s not in vtable for %s", op, named))
	}
	vtablePtr := c.block.NewBitCast(vtableRaw, irtypes.NewPointer(irtypes.I8Ptr))
	fnSlotPtr := c.block.NewGetElementPtr(irtypes.I8Ptr, vtablePtr,
		constant.NewInt(irtypes.I32, int64(slotIndex)))
	fnRaw := c.block.NewLoad(irtypes.I8Ptr, fnSlotPtr)

	// Build the function type (i8* receiver only) and bitcast.
	retType := irtypes.Type(irtypes.Void)
	if method.Sig().Result() != nil {
		retType = c.resolveType(method.Sig().Result())
	}
	if method.Sig().CanError() {
		retType = computeResultType(retType)
	}
	var paramTypes []irtypes.Type
	if method.Sig().Recv() != nil {
		paramTypes = append(paramTypes, irtypes.I8Ptr)
	}
	funcType := irtypes.NewFunc(retType, paramTypes...)
	fnTyped := c.block.NewBitCast(fnRaw, irtypes.NewPointer(funcType))

	var args []value.Value
	if method.Sig().Recv() != nil {
		args = append(args, instance)
	}
	return c.block.NewCall(fnTyped, args...)
}

// lookupUnaryMethod finds the 0-param variant of a method by name, walking
// is-parents and structural-interface parents (T0881) so inherited unary
// operators dispatch the same way binary operators do.
func (c *Compiler) lookupUnaryMethod(named *types.Named, op string) *types.Method {
	return named.LookupUnaryMethod(op)
}

// --- Short-circuit boolean operators ---

func (c *Compiler) genShortCircuitAnd(e *ast.BinaryExpr) value.Value {
	left := c.genExprAutoPropagate(e.Left)
	startBlock := c.block

	rightBlock := c.newBlock("and.rhs")
	mergeBlock := c.newBlock("and.merge")

	c.block.NewCondBr(left, rightBlock, mergeBlock)

	c.block = rightBlock
	right := c.genExprAutoPropagate(e.Right)
	rightEnd := c.block
	c.block.NewBr(mergeBlock)

	c.block = mergeBlock
	phi := mergeBlock.NewPhi(
		&ir.Incoming{X: constant.NewInt(irtypes.I1, 0), Pred: startBlock},
		&ir.Incoming{X: right, Pred: rightEnd},
	)
	return phi
}

func (c *Compiler) genShortCircuitOr(e *ast.BinaryExpr) value.Value {
	left := c.genExprAutoPropagate(e.Left)
	startBlock := c.block

	rightBlock := c.newBlock("or.rhs")
	mergeBlock := c.newBlock("or.merge")

	c.block.NewCondBr(left, mergeBlock, rightBlock)

	c.block = rightBlock
	right := c.genExprAutoPropagate(e.Right)
	rightEnd := c.block
	c.block.NewBr(mergeBlock)

	c.block = mergeBlock
	phi := mergeBlock.NewPhi(
		&ir.Incoming{X: constant.NewInt(irtypes.I1, 1), Pred: startBlock},
		&ir.Incoming{X: right, Pred: rightEnd},
	)
	return phi
}

// --- range construction ---

// genRange constructs a Range[T] value type struct via insertvalue chain.
// Layout: { i8* _vtable, T start, T end, i1 inclusive }
func (c *Compiler) genRange(e *ast.BinaryExpr) value.Value {
	start := c.genExprAutoPropagate(e.Left)
	end := c.genExprAutoPropagate(e.Right)
	inclusive := constant.NewInt(irtypes.I1, 0)
	if e.Op == ast.BinInclusiveRange {
		inclusive = constant.NewInt(irtypes.I1, 1)
	}

	// Look up the mono value type layout for Range[T]
	resultType := c.info.Types[e]
	if c.typeSubst != nil {
		resultType = types.Substitute(resultType, c.typeSubst)
	}
	layout := c.lookupTypeLayout(resultType)
	if layout == nil {
		panic(fmt.Sprintf("codegen: no layout for range type %s", resultType))
	}
	valueStructType := layout.Value.LLVMType

	// Build value struct via insertvalue
	var val value.Value = constant.NewUndef(valueStructType)
	val = c.block.NewInsertValue(val, constant.NewNull(irtypes.I8Ptr), 0)                     // vtable = null
	val = c.block.NewInsertValue(val, start, uint64(layout.ValueFieldIndex["start"]))         // start
	val = c.block.NewInsertValue(val, end, uint64(layout.ValueFieldIndex["end"]))             // end
	val = c.block.NewInsertValue(val, inclusive, uint64(layout.ValueFieldIndex["inclusive"])) // inclusive
	return val
}
