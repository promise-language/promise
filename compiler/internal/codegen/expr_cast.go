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

// --- is/as expressions ---

// genIsExpr generates code for `expr is Pattern`.
func (c *Compiler) genIsExpr(e *ast.IsExpr) value.Value {
	switch p := e.Pattern.(type) {
	case *ast.IdentIsPattern:
		return c.genIsIdentPattern(e.Expr, p)
	case *ast.DestructureIsPattern:
		return c.genIsDestructurePattern(e.Expr, p)
	default:
		panic(fmt.Sprintf("codegen: unhandled is-pattern type %T", e.Pattern))
	}
}

func (c *Compiler) genIsIdentPattern(expr ast.Expr, p *ast.IdentIsPattern) value.Value {
	// Optional: x is present / x is absent
	if p.Name == "present" || p.Name == "absent" {
		optVal := c.genExpr(expr)
		flag := c.block.NewExtractValue(optVal, 0) // i1 flag field

		// B0288: Drop temporary optional data for call-like expressions.
		// When a method/function call returns T? with droppable inner T
		// (enum, string, vector), the data portion leaks unless dropped.
		// Only drop for expressions that produce fresh temporaries (calls,
		// optional chains). Field/index/ident expressions share ownership
		// with the parent variable — dropping would double-free.
		switch expr.(type) {
		case *ast.CallExpr, *ast.OptionalChainExpr, *ast.ErrorHandlerExpr:
			c.dropTempOptionalInner(expr, optVal, flag)
		}

		if p.Name == "absent" {
			return c.block.NewXor(flag, constant.NewInt(irtypes.I1, 1))
		}
		return flag
	}

	// Check if the subject is an optional type — unwrap before checking inner type
	exprType := c.info.Types[expr]
	if c.typeSubst != nil {
		exprType = types.Substitute(exprType, c.typeSubst)
	}
	if opt, ok := exprType.(*types.Optional); ok {
		// Generic pattern with resolved type
		if resolved := c.info.IsPatternTypes[p]; resolved != nil {
			return c.genIsOptionalTypeResolved(expr, resolved, opt)
		}
		return c.genIsOptionalType(expr, p.Name, opt)
	}

	// Check if the subject is an enum type — use tag comparison
	if enumLayout := c.lookupEnumLayout(exprType); enumLayout != nil {
		return c.genIsEnumVariant(expr, p.Name, enumLayout)
	}

	// Generic pattern with resolved type — use type ID directly
	if resolved := c.info.IsPatternTypes[p]; resolved != nil {
		return c.genIsResolvedType(expr, resolved)
	}

	// Named type check via RTTI
	return c.genIsNamedType(expr, p.Name)
}

// dropTempOptionalInner drops the inner value of a temporary optional struct.
// B0288: When `expr is present/absent` evaluates a non-ident expression (e.g., method call)
// returning T?, the data portion of the {i1, T} struct is abandoned. If T is a droppable
// type (enum with heap data, string, vector), the inner value must be conditionally dropped
// (only when the flag indicates presence) to prevent leaks.
func (c *Compiler) dropTempOptionalInner(expr ast.Expr, optVal value.Value, flag value.Value) {
	if c.block == nil || c.block.Term != nil {
		return
	}
	exprType := c.info.Types[expr]
	if exprType == nil {
		return
	}
	if c.typeSubst != nil {
		exprType = types.Substitute(exprType, c.typeSubst)
	}
	opt, ok := exprType.(*types.Optional)
	if !ok {
		return
	}
	elem := opt.Elem()
	if c.typeSubst != nil {
		elem = types.Substitute(elem, c.typeSubst)
	}

	// Determine what kind of drop is needed.
	innerEnum := extractEnum(elem)
	innerNamed := extractNamed(elem)

	if innerEnum != nil {
		// Enum with droppable variants — call the synthesized enum drop function.
		if !c.enumInstanceHasDrop(elem, innerEnum) {
			return
		}
		enumName := innerEnum.Obj().Name()
		if inst, ok := elem.(*types.Instance); ok {
			enumName = monoName(inst)
		} else if c.typeSubst != nil {
			resolved := types.Substitute(elem, c.typeSubst)
			if inst, ok := resolved.(*types.Instance); ok {
				enumName = monoName(inst)
			}
		}
		mangledName := mangleMethodName(enumName, "drop", false)
		dropFunc, exists := c.funcs[mangledName]
		if !exists || dropFunc == nil {
			return
		}

		innerVal := c.block.NewExtractValue(optVal, 1)
		alloca := c.createEntryAlloca(innerVal.Type())
		c.block.NewStore(innerVal, alloca)

		dropBlock := c.newBlock("is.temp.drop")
		skipBlock := c.newBlock("is.temp.skip")
		c.block.NewCondBr(flag, dropBlock, skipBlock)

		c.block = dropBlock
		ptr := c.block.NewBitCast(alloca, irtypes.I8Ptr)
		c.block.NewCall(dropFunc, ptr)
		c.block.NewBr(skipBlock)

		c.block = skipBlock
	} else if innerNamed == types.TypString {
		// String — call promise_string_drop.
		innerVal := c.block.NewExtractValue(optVal, 1)
		dropBlock := c.newBlock("is.temp.drop")
		skipBlock := c.newBlock("is.temp.skip")
		c.block.NewCondBr(flag, dropBlock, skipBlock)

		c.block = dropBlock
		c.block.NewCall(c.funcs["promise_string_drop"], innerVal)
		c.block.NewBr(skipBlock)

		c.block = skipBlock
	} else if innerNamed != nil {
		// Vector or channel — call their drop.
		var dropFunc *ir.Func
		var isContainer bool
		if _, isVec := types.AsVector(elem); isVec || innerNamed == types.TypVector {
			dropFunc = c.funcs["Vector.drop"]
			isContainer = true
		} else if chElem, isCh := types.AsChannel(elem); isCh || innerNamed == types.TypChannel {
			// T0663: per-element-type drop walks any un-received buffered items.
			if chElem != nil {
				dropFunc = c.getOrCreateChannelDrop(chElem)
				isContainer = true
			}
		} else if innerNamed.HasDrop() || innerNamed.NeedsSynthDrop() {
			// B0288: User type with explicit drop() or synthesized drop.
			ownerName := innerNamed.Obj().Name()
			resolvedElem := elem
			if c.typeSubst != nil {
				resolvedElem = types.Substitute(elem, c.typeSubst)
			}
			if inst, ok := resolvedElem.(*types.Instance); ok {
				ownerName = monoName(inst)
			} else if innerNamed.HasDrop() && !innerNamed.NeedsSynthDrop() {
				ownerName = c.resolveDropOwner(innerNamed)
			}
			mangledName := mangleMethodName(ownerName, "drop", false)
			dropFunc = c.funcs[mangledName]
		}
		if dropFunc != nil {
			innerVal := c.block.NewExtractValue(optVal, 1)
			dropBlock := c.newBlock("is.temp.drop")
			skipBlock := c.newBlock("is.temp.skip")
			c.block.NewCondBr(flag, dropBlock, skipBlock)

			c.block = dropBlock
			if isContainer {
				c.block.NewCall(dropFunc, innerVal)
			} else {
				// User type: inner is value struct {vtable, instance} — extract instance ptr.
				instance := c.extractInstancePtr(innerVal)
				nullCheck := c.block.NewICmp(enum.IPredEQ, instance, constant.NewNull(irtypes.I8Ptr))
				execBlock := c.newBlock("is.temp.exec")
				nullSkip := c.newBlock("is.temp.null")
				c.block.NewCondBr(nullCheck, nullSkip, execBlock)

				c.block = execBlock
				c.block.NewCall(dropFunc, instance)
				// B0159: Free the instance struct after drop() completes.
				if !innerNamed.NeedsSynthDrop() {
					c.block.NewCall(c.palFree, instance)
				}
				c.block.NewBr(nullSkip)

				c.block = nullSkip
			}
			c.block.NewBr(skipBlock)

			c.block = skipBlock
		}
	}
}

// genIsOptionalType generates code for `optExpr is TypeName` where optExpr has type T?.
// For primitive/string optionals (no RTTI), this is equivalent to a presence check.
// For user types with RTTI, this checks presence AND performs RTTI on the unwrapped value.
func (c *Compiler) genIsOptionalType(expr ast.Expr, typeName string, opt *types.Optional) value.Value {
	optVal := c.genExpr(expr)
	flag := c.block.NewExtractValue(optVal, 0) // i1 presence flag

	elem := opt.Elem()
	// For enums, primitives, and strings there is no subtyping,
	// so T? is T is equivalent to T? is present — just check the flag.
	if c.lookupEnumLayout(elem) != nil {
		return flag
	}
	named := extractNamed(elem)
	if named != nil && (isPrimitiveScalar(named) || named == types.TypString) {
		return flag
	}

	// User type with RTTI: check presence AND type via RTTI on the unwrapped value.
	// We need branching to avoid accessing RTTI on a none value.
	targetNamed := c.lookupNamedType(typeName)
	if targetNamed == nil {
		panic(fmt.Sprintf("codegen: undefined type %s in is-expression", typeName))
	}
	targetID := c.assignTypeID(targetNamed)

	fn := c.block.Parent
	thenBlock := fn.NewBlock("")
	elseBlock := fn.NewBlock("")
	mergeBlock := fn.NewBlock("")

	c.block.NewCondBr(flag, thenBlock, elseBlock)

	// Then: extract inner value and do RTTI check
	c.block = thenBlock
	inner := c.block.NewExtractValue(optVal, 1)
	instance := c.instancePtrForRTTI(inner, elem)
	variantPtr := c.loadVariantPtr(instance)
	result := c.block.NewCall(c.funcs["promise_type_is"],
		variantPtr, constant.NewInt(irtypes.I32, int64(targetID)))
	rttiResult := c.block.NewICmp(enum.IPredNE, result, constant.NewInt(irtypes.I32, 0))
	c.block.NewBr(mergeBlock)
	thenExit := c.block

	// Else: not present → false
	c.block = elseBlock
	c.block.NewBr(mergeBlock)
	elseExit := c.block

	// Merge
	c.block = mergeBlock
	phi := c.block.NewPhi(ir.NewIncoming(rttiResult, thenExit), ir.NewIncoming(constant.NewInt(irtypes.I1, 0), elseExit))
	return phi
}

// genNarrowedVariantField reads a named payload field of an enum value that was
// narrowed to a variant via `if x is Variant` (T0993). Mirrors the field-extract
// logic in bindIsDestructureEnum but for a single, non-destructive read: the
// subject is left intact (this is a borrow), so no drop flag / dup is involved.
func (c *Compiler) genNarrowedVariantField(e *ast.MemberExpr, access *sema.VariantFieldAccess) value.Value {
	subject := c.genExpr(e.Target)

	targetType := access.TargetType
	if c.typeSubst != nil {
		targetType = types.Substitute(targetType, c.typeSubst)
	}
	enumLayout := c.lookupEnumLayout(targetType)
	if enumLayout == nil {
		panic(fmt.Sprintf("codegen: no enum layout for %s", targetType))
	}
	// A `this` enum receiver is an i8* pointer — load to a by-value enum.
	subject = c.enumThisSubject(subject, enumLayout)

	dataType := enumLayout.VariantDataTypes[access.VariantName]
	if dataType == nil {
		panic(fmt.Sprintf("codegen: no variant data layout for %s.%s", targetType, access.VariantName))
	}

	internalType := enumLayout.EnumInternalType.(*irtypes.StructType)
	alloca := c.createEntryAlloca(internalType)
	c.block.NewStore(subject, alloca)

	dataPtr := c.block.NewGetElementPtr(internalType, alloca,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 1))
	typedDataPtr := c.block.NewBitCast(dataPtr, irtypes.NewPointer(dataType))

	idx := access.FieldIndex
	if idx >= len(dataType.Fields) {
		panic(fmt.Sprintf("codegen: variant field index %d out of range for %s.%s", idx, targetType, access.VariantName))
	}
	fieldType := dataType.Fields[idx]
	fieldPtr := c.block.NewGetElementPtr(dataType, typedDataPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(idx)))
	val := c.block.NewLoad(fieldType, fieldPtr)

	// T1011: A non-destructive narrowed read is a borrow by default — an in-scope
	// read returns the raw load (zero-copy). But when the result escapes the
	// narrowing scope (return, var-decl, assignment, consuming arg, constructor
	// field), the consumer site sets a dup-on-escape flag exactly as it does for a
	// normal struct field read. Honor those flags via the shared dupHeapFieldForEscape
	// so the escaping value is an independent copy; otherwise it aliases the subject's
	// payload, which the subject's synth enum drop frees at scope exit → use-after-free
	// / double-free. Gated on the subject enum being droppable (otherwise the field is
	// never freed and a copy would leak).
	if !c.tempTrackingEnabled {
		return val
	}
	fType := access.FieldType
	if c.typeSubst != nil {
		fType = types.Substitute(fType, c.typeSubst)
	}
	if dup, ok := c.dupHeapFieldForEscape(val, fType, c.enumTargetDroppable(targetType)); ok {
		return dup
	}
	return val
}

// narrowedVariantFieldDroppable reports whether a MemberExpr is a non-destructive
// narrowed enum variant field read (`if x is V { x.field }`, T0993) and, if so,
// whether the subject enum is droppable — in which case genNarrowedVariantField
// dups the field for escape, just like a struct field on a droppable owner. The
// dup-flag-setting helpers (constructor/`~`-param args) and isStringFieldDup share
// this so all consumer sites treat a narrowed variant field identically to a
// struct field. T1011.
func (c *Compiler) narrowedVariantFieldDroppable(mem *ast.MemberExpr) (matched bool, droppable bool) {
	access := c.info.NarrowedVariantField[mem]
	if access == nil {
		return false, false
	}
	targetType := access.TargetType
	if c.typeSubst != nil {
		targetType = types.Substitute(targetType, c.typeSubst)
	}
	return true, c.enumTargetDroppable(targetType)
}

// enumTargetDroppable reports whether a resolved narrowing target type is a
// droppable enum — i.e. its synth drop frees variant payload, so a heap variant
// field escaping the narrowing scope must be cloned rather than aliased. The
// single source for the narrowed-field droppable predicate, shared by
// narrowedVariantFieldDroppable and genNarrowedVariantField. T1011.
func (c *Compiler) enumTargetDroppable(targetType types.Type) bool {
	enum := extractEnum(targetType)
	return enum != nil && c.enumInstanceHasDrop(targetType, enum)
}

// enumThisSubject converts a `this` enum receiver (an i8* pointer returned by
// genThisExpr inside an enum method/getter) into the by-value enum value that
// tag extraction and destructuring expect. Any non-i8* subject (a by-value enum
// from a local or parameter) is returned unchanged. Mirrors the i8*-handling in
// genMatchExpr.
func (c *Compiler) enumThisSubject(subject value.Value, layout *TypeDeclLayout) value.Value {
	if !subject.Type().Equal(irtypes.I8Ptr) {
		return subject
	}
	var loadType irtypes.Type
	if layout.MaxVariantDataSize == 0 {
		loadType = irtypes.I32 // fieldless enum: tag only
	} else {
		loadType = layout.EnumInternalType // data enum: {i32 tag, [N x i8] data}
	}
	typedPtr := c.block.NewBitCast(subject, irtypes.NewPointer(loadType))
	return c.block.NewLoad(loadType, typedPtr)
}

func (c *Compiler) genIsEnumVariant(expr ast.Expr, variantName string, layout *TypeDeclLayout) value.Value {
	if _, ok := layout.VariantTag[variantName]; !ok {
		panic(fmt.Sprintf("codegen: unknown enum variant %s", variantName))
	}
	subject := c.genExpr(expr)
	// A `this` enum receiver is an i8* pointer — load the value before tag extraction.
	subject = c.enumThisSubject(subject, layout)
	// Extract tag
	var tag value.Value
	if layout.MaxVariantDataSize == 0 {
		tag = subject // fieldless enum: value IS the tag
	} else {
		tag = c.block.NewExtractValue(subject, 0)
	}
	expectedTag := constant.NewInt(irtypes.I32, int64(layout.VariantTag[variantName]))
	return c.block.NewICmp(enum.IPredEQ, tag, expectedTag)
}

func (c *Compiler) genIsNamedType(expr ast.Expr, typeName string) value.Value {
	subject := c.genExpr(expr)

	// Look up target type and its type ID
	targetNamed := c.lookupNamedType(typeName)
	if targetNamed == nil {
		panic(fmt.Sprintf("codegen: undefined type %s in is-expression", typeName))
	}
	targetID := c.assignTypeID(targetNamed)

	// Extract instance pointer for RTTI query.
	// For value types, use the compile-time-known RTTI global (no field in value struct).
	exprType := c.info.Types[expr]
	if c.typeSubst != nil {
		exprType = types.Substitute(exprType, c.typeSubst)
	}
	var instance value.Value
	if isThisReceiver(expr) {
		instance = c.extractInstancePtrForThis(subject)
	} else {
		instance = c.instancePtrForRTTI(subject, exprType)
	}
	variantPtr := c.loadVariantPtr(instance)

	// Call promise_type_is(variant_ptr, expected_id) and convert i32 result to i1
	result := c.block.NewCall(c.funcs["promise_type_is"],
		variantPtr, constant.NewInt(irtypes.I32, int64(targetID)))
	return c.block.NewICmp(enum.IPredNE, result, constant.NewInt(irtypes.I32, 0))
}

// genIsResolvedType generates an RTTI type check for a sema-resolved type
// (supports both *types.Named and *types.Instance from generic is-patterns).
func (c *Compiler) genIsResolvedType(expr ast.Expr, resolved types.Type) value.Value {
	subject := c.genExpr(expr)

	targetID, ok := c.resolveTypeID(resolved)
	if !ok {
		panic(fmt.Sprintf("codegen: cannot resolve type ID for %s in is-expression", resolved))
	}

	exprType := c.info.Types[expr]
	if c.typeSubst != nil {
		exprType = types.Substitute(exprType, c.typeSubst)
	}
	var instance value.Value
	if isThisReceiver(expr) {
		instance = c.extractInstancePtrForThis(subject)
	} else {
		instance = c.instancePtrForRTTI(subject, exprType)
	}
	variantPtr := c.loadVariantPtr(instance)

	result := c.block.NewCall(c.funcs["promise_type_is"],
		variantPtr, constant.NewInt(irtypes.I32, int64(targetID)))
	return c.block.NewICmp(enum.IPredNE, result, constant.NewInt(irtypes.I32, 0))
}

// genIsOptionalTypeResolved generates code for `optExpr is Type[args]` where optExpr
// has type T? and the target type is a sema-resolved generic instance.
func (c *Compiler) genIsOptionalTypeResolved(expr ast.Expr, resolved types.Type, opt *types.Optional) value.Value {
	optVal := c.genExpr(expr)
	flag := c.block.NewExtractValue(optVal, 0)

	elem := opt.Elem()
	if c.lookupEnumLayout(elem) != nil {
		return flag
	}
	named := extractNamed(elem)
	if named != nil && (isPrimitiveScalar(named) || named == types.TypString) {
		return flag
	}

	targetID, ok := c.resolveTypeID(resolved)
	if !ok {
		panic(fmt.Sprintf("codegen: cannot resolve type ID for %s in is-expression", resolved))
	}

	fn := c.block.Parent
	thenBlock := fn.NewBlock("")
	elseBlock := fn.NewBlock("")
	mergeBlock := fn.NewBlock("")

	c.block.NewCondBr(flag, thenBlock, elseBlock)

	c.block = thenBlock
	inner := c.block.NewExtractValue(optVal, 1)
	instance := c.instancePtrForRTTI(inner, elem)
	variantPtr := c.loadVariantPtr(instance)
	rttiResult := c.block.NewCall(c.funcs["promise_type_is"],
		variantPtr, constant.NewInt(irtypes.I32, int64(targetID)))
	rttiCheck := c.block.NewICmp(enum.IPredNE, rttiResult, constant.NewInt(irtypes.I32, 0))
	c.block.NewBr(mergeBlock)
	thenExit := c.block

	c.block = elseBlock
	c.block.NewBr(mergeBlock)
	elseExit := c.block

	c.block = mergeBlock
	phi := c.block.NewPhi(ir.NewIncoming(rttiCheck, thenExit), ir.NewIncoming(constant.NewInt(irtypes.I1, 0), elseExit))
	return phi
}

// genIsDestructurePattern generates the bool check for a destructure is-pattern
// (e.g., `x is Circle(r)`). When used inside an if-condition, the actual field
// binding is handled by genIfDestructureIsStmt. Outside if-conditions, this just
// returns the type/variant check result without binding any variables.
func (c *Compiler) genIsDestructurePattern(expr ast.Expr, p *ast.DestructureIsPattern) value.Value {
	exprType := c.info.Types[expr]
	if c.typeSubst != nil {
		exprType = types.Substitute(exprType, c.typeSubst)
	}

	// Enum variant check
	if enumLayout := c.lookupEnumLayout(exprType); enumLayout != nil {
		if _, ok := enumLayout.VariantTag[p.TypeName]; ok {
			return c.genIsEnumVariant(expr, p.TypeName, enumLayout)
		}
	}

	// Generic type with resolved type — use type ID directly
	if resolved := c.info.IsPatternTypes[p]; resolved != nil {
		return c.genIsResolvedType(expr, resolved)
	}

	// Named type check via RTTI
	return c.genIsNamedType(expr, p.TypeName)
}

// extractInstancePtr extracts the i8* instance pointer (field 1) from a user type value struct.
func (c *Compiler) extractInstancePtr(val value.Value) value.Value {
	return c.block.NewExtractValue(val, 1)
}

// extractVtablePtr extracts the i8* vtable pointer (field 0) from a user type value struct.
func (c *Compiler) extractVtablePtr(val value.Value) value.Value {
	return c.block.NewExtractValue(val, 0)
}

// valueTypeReceiverPtr creates a temp alloca for a value type receiver and returns
// an i8* pointer to it. Methods on value types receive a pointer to the value struct.
func (c *Compiler) valueTypeReceiverPtr(val value.Value, typ types.Type) value.Value {
	layout := c.lookupTypeLayout(typ)
	if layout == nil {
		panic(fmt.Sprintf("codegen: no layout for value type receiver %s", typ))
	}
	tmp := c.createEntryAlloca(layout.Value.LLVMType)
	c.block.NewStore(val, tmp)
	return c.block.NewBitCast(tmp, irtypes.I8Ptr)
}

// genValueTypeReceiverAddr returns an i8* to the in-place storage of a value-type
// l-value receiver — a local, `this`, a nested value-type member (`o.inner`), or a
// container element (`vs[0]`) — so a field store or setter mutates the real
// storage rather than a spilled copy (T1356). Returns ok=false for a
// non-addressable receiver (e.g. a value type returned by a call), where the
// caller must fall back to a spill (nothing to write back to).
func (c *Compiler) genValueTypeReceiverAddr(recv ast.Expr) (value.Value, bool) {
	if isThisReceiver(recv) {
		// `this` is already an i8* pointing at the value struct.
		return c.genExpr(recv), true
	}
	switch e := recv.(type) {
	case *ast.IdentExpr:
		if alloca, ok := c.locals[e.Name]; ok {
			return c.block.NewBitCast(alloca, irtypes.I8Ptr), true
		}
		if ptr, ok := c.mutRefPtrs[e.Name]; ok { // value-type MutRef param
			return c.block.NewBitCast(ptr, irtypes.I8Ptr), true
		}
		return nil, false
	case *ast.MemberExpr:
		// Only a real field is addressable in place; a getter member produces a
		// fresh temporary, so fall back to the spill. genFieldPtr returns an
		// in-place pointer to the field's storage — for a value-type field on a
		// heap parent it GEPs into the instance; on a value parent it recurses
		// back through this helper, terminating as each step strips one member.
		parentType := c.info.Types[e.Target]
		if c.typeSubst != nil {
			parentType = types.Substitute(parentType, c.typeSubst)
		}
		if c.selfSubst != nil { // mirror genFieldPtr so a `Self`-typed parent resolves
			parentType = types.SubstituteSelf(parentType, c.selfSubst.iface, c.selfSubst.concrete)
		}
		if pn := extractNamed(parentType); pn == nil || pn.LookupField(e.Field) == nil {
			return nil, false
		}
		return c.block.NewBitCast(c.genFieldPtr(e), irtypes.I8Ptr), true
	case *ast.IndexExpr:
		// genIndexSlotPtr returns an element pointer for arrays and vectors.
		// COW is unnecessary: a value-type-element vector literal is always
		// heap-allocated (T0062 statics require compile-time-constant scalar
		// elements), so in-place mutation never touches read-only memory and a
		// COW guard would be a guaranteed no-op.
		return c.block.NewBitCast(c.genIndexSlotPtr(e), irtypes.I8Ptr), true
	default:
		return nil, false
	}
}

// genValueTypeReceiverArg produces the i8* receiver argument for a value-type
// method call on a non-`this` receiver. For a `~this` (mutable-borrow) receiver
// on an addressable l-value (local, value-type field, container element), it
// passes the caller's in-place storage address so field/setter mutations reach
// the caller's variable (T1358); otherwise it spills the receiver value into a
// temp and passes the temp's pointer (value semantics). Evaluates recv exactly
// once in each path.
func (c *Compiler) genValueTypeReceiverArg(recv ast.Expr, recvType types.Type, mut bool) value.Value {
	if mut {
		if addr, ok := c.genValueTypeReceiverAddr(recv); ok {
			return addr
		}
	}
	val := c.genExprAutoPropagate(recv)
	return c.valueTypeReceiverPtr(val, recvType)
}

// extractInstancePtrForThis extracts the instance/RTTI pointer from a `this` value.
// For regular types, `this` (i8*) IS the instance pointer.
// For value types, the RTTI pointer is not stored in the value struct — use the
// compile-time-known RTTI global directly.
func (c *Compiler) extractInstancePtrForThis(thisVal value.Value) value.Value {
	if c.currentNamed != nil && c.currentNamed.IsValueType() {
		if rttiGlobal := c.lookupValueTypeRTTI(c.currentNamed); rttiGlobal != nil {
			return c.block.NewBitCast(rttiGlobal, irtypes.I8Ptr)
		}
	}
	return thisVal
}

// instancePtrForRTTI returns the instance pointer for RTTI queries (is-checks, casts).
// For regular types, field 1 of the value struct is the instance pointer.
// For value types, the RTTI pointer is not in the value struct — use the compile-time-known global.
func (c *Compiler) instancePtrForRTTI(val value.Value, typ types.Type) value.Value {
	named := extractNamed(typ)
	if named != nil && named.IsValueType() {
		if rttiGlobal := c.lookupValueTypeRTTI(typ); rttiGlobal != nil {
			return c.block.NewBitCast(rttiGlobal, irtypes.I8Ptr)
		}
	}
	return c.extractInstancePtr(val)
}

// loadVariantPtr loads the _variant pointer (RTTI info) from a user type instance.
// The instance must be an i8* pointer; the first field of any instance struct is the variant pointer.
func (c *Compiler) loadVariantPtr(subject value.Value) value.Value {
	variantPtrStruct := irtypes.NewStruct(irtypes.I8Ptr)
	typedPtr := c.block.NewBitCast(subject, irtypes.NewPointer(variantPtrStruct))
	variantFieldPtr := c.block.NewGetElementPtr(variantPtrStruct, typedPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
	return c.block.NewLoad(irtypes.I8Ptr, variantFieldPtr)
}

// loadVtablePtrFromInstance loads the dispatch vtable pointer for an i8* instance
// pointer by following the instance's variant→typeinfo chain (instance[0] →
// typeinfo[0] = vtable_ptr). Centralizes the load shared by virtual call sites,
// RTTI error reconstruction, and the abstract-return `return this` value-struct
// build (T0917) — keep a single implementation so the typeinfo layout assumption
// lives in one place.
func (c *Compiler) loadVtablePtrFromInstance(instance value.Value) value.Value {
	variantPtr := c.loadVariantPtr(instance)
	typeinfoStruct := irtypes.NewStruct(irtypes.I8Ptr) // field 0: vtable_ptr
	typeinfoPtr := c.block.NewBitCast(variantPtr, irtypes.NewPointer(typeinfoStruct))
	vtableFieldPtr := c.block.NewGetElementPtr(typeinfoStruct, typeinfoPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
	return c.block.NewLoad(irtypes.I8Ptr, vtableFieldPtr)
}

// genCastExpr generates code for `expr as Type` and `expr as! Type`.
func (c *Compiler) genCastExpr(e *ast.CastExpr) value.Value {
	// Optional unwrap: T? as! T → extract inner value, panic on none.
	if e.Force {
		srcType := c.info.Types[e.Expr]
		if opt, ok := srcType.(*types.Optional); ok {
			targetType := c.resolveTypeRefToType(e.Type)
			if targetType != nil && types.Identical(opt.Elem(), targetType) {
				return c.genOptionalForceUnwrap(e.Expr)
			}
		}
	}

	// Resolve the target Named type from the TypeRef.
	//
	// A borrow/reference target (`x as! T&` / `x as! T~`) is an RTTI-checked
	// reborrow: peel the ref to the underlying named type, do the same RTTI cast,
	// and treat the result as a borrow (no ownership transfer, no drop
	// responsibility) — mirroring how borrow-typed return values are handled. The
	// downstream RTTI/result paths are already borrow-agnostic (the result is the
	// bare `{vtable, instance}` value struct, which is exactly a user-type borrow).
	// T0848.
	targetTypeRef := e.Type
	targetIsBorrow := false
	switch ref := targetTypeRef.(type) {
	case *ast.SharedRefTypeRef:
		targetTypeRef, targetIsBorrow = ref.Inner, true
	case *ast.MutRefTypeRef:
		targetTypeRef, targetIsBorrow = ref.Inner, true
	}
	// T1884: resolve target type via resolveTypeRefToType so all TypeRef kinds
	// (SliceTypeRef, QualifiedTypeRef, etc.) work, not just NamedTypeRef.
	targetType := c.resolveTypeRefToType(targetTypeRef)
	if targetType == nil {
		panic(fmt.Sprintf("codegen: cannot resolve cast target type %T", targetTypeRef))
	}
	targetNamed := extractNamed(targetType)
	if targetNamed == nil {
		panic(fmt.Sprintf("codegen: cast target type %s has no Named type", targetType))
	}

	// T0761: RTTI cast whose subject is itself an Optional (`opt as! Subtype` /
	// `opt as Target`). genExpr yields the `{ i1, {i8*,i8*} }` optional
	// representation, not the bare `{vtable, instance}` value struct the result
	// paths below assume — unwrap the inner value first. (The same-type
	// `T? as! T` unwrap is short-circuited above.)
	if srcType := c.info.Types[e.Expr]; srcType != nil {
		if c.typeSubst != nil {
			srcType = types.Substitute(srcType, c.typeSubst)
		}
		// T0850: a borrowed optional (`T?&` — e.g. `Ref[T?].borrow` or a
		// `Mutex[T?]` guard's `.borrow`) has srcType SharedRef/MutRef-of-Optional.
		// genExpr auto-derefs the borrow to the loaded `{i1,{i8*,i8*}}` optional, so
		// it must route through the optional-subject path too — otherwise the
		// non-optional RTTI path feeds the optional value to wrapOptional and panics
		// with an insertvalue/store type mismatch. The inner is owned by the external
		// owner (the Arc/Mutex payload), so flag borrowSource → dup, no neutralize.
		borrowSource := false
		switch ref := srcType.(type) {
		case *types.SharedRef:
			if opt, ok := ref.Elem().(*types.Optional); ok {
				srcType, borrowSource = opt, true
			}
		case *types.MutRef:
			if opt, ok := ref.Elem().(*types.Optional); ok {
				srcType, borrowSource = opt, true
			}
		}
		if opt, ok := srcType.(*types.Optional); ok {
			return c.genOptionalCastExpr(e, opt, targetNamed, borrowSource)
		}
	}

	subject := c.genExpr(e.Expr)

	// Primitive scalar casts (numeric, char, bool) — compile-time conversions, no RTTI needed
	srcType := c.info.Types[e.Expr]
	srcNamed := extractNamed(srcType)
	if srcNamed != nil && isPrimitiveScalar(srcNamed) && isPrimitiveScalar(targetNamed) {
		return c.emitScalarCast(subject, srcNamed, targetNamed)
	}

	// T1884: Structural interface downcast — extract concrete value from structural box.
	// Use vtable pointer comparison: each (concrete, structural) pair has a unique view
	// vtable, so comparing field 0 of the fat value against the expected view vtable for
	// (target, source_structural) correctly identifies the concrete type for all boxed
	// kinds (primitives, strings, value types, heap user types). RTTI would fail for
	// primitive/string boxes whose flat-box typeinfo carries a per-size ID rather than
	// the concrete type's ID.
	if c.typeSubst != nil {
		srcType = types.Substitute(srcType, c.typeSubst)
	}
	if srcNamed != nil && isStructuralView(srcNamed) {
		return c.genStructuralDowncast(e, subject, srcNamed, targetNamed, targetType, targetIsBorrow)
	}

	targetID := c.assignTypeID(targetNamed)

	// Extract instance pointer for RTTI query.
	// For value types, use the compile-time-known RTTI global (no field in value struct).
	var instance value.Value
	if isThisReceiver(e.Expr) {
		instance = c.extractInstancePtrForThis(subject)
	} else {
		instance = c.instancePtrForRTTI(subject, srcType)
	}
	variantPtr := c.loadVariantPtr(instance)

	result := c.block.NewCall(c.funcs["promise_type_is"],
		variantPtr, constant.NewInt(irtypes.I32, int64(targetID)))
	isMatch := c.block.NewICmp(enum.IPredNE, result, constant.NewInt(irtypes.I32, 0))

	// T0747: For a `this` receiver, genExpr produced a bare instance i8*, not a
	// {vtable, instance} value struct, so the result paths below (return /
	// wrapOptional / downstream field access) would get an i8* where a value
	// struct is required → invalid IR or a codegen panic. Rebuild the value
	// struct, loading the vtable from the object's typeinfo chain — the same
	// reconstruction used for virtual dispatch on a `this` receiver
	// (genVirtualBinaryOp). RTTI casts apply only to reference types (value types
	// have no `is` parents, so `this as! T` in a value-type method is
	// sema-rejected); the value-type guard keeps that unreachable path untouched.
	castResult := subject
	if isThisReceiver(e.Expr) && (c.currentNamed == nil || !c.currentNamed.IsValueType()) {
		typeinfoStruct := irtypes.NewStruct(irtypes.I8Ptr)
		typeinfoPtr := c.block.NewBitCast(variantPtr, irtypes.NewPointer(typeinfoStruct))
		vtableFieldPtr := c.block.NewGetElementPtr(typeinfoStruct, typeinfoPtr,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
		vtableRaw := c.block.NewLoad(irtypes.I8Ptr, vtableFieldPtr)
		var vs value.Value = constant.NewUndef(userValueType())
		vs = c.block.NewInsertValue(vs, vtableRaw, 0)
		vs = c.block.NewInsertValue(vs, instance, 1) // instance == this i8* for reference types
		castResult = vs
	}

	if e.Force {
		// as! — panic if no match, return the value struct directly
		okBlock := c.newBlock("cast.ok")
		panicBlock := c.newBlock("cast.panic")
		c.block.NewCondBr(isMatch, okBlock, panicBlock)

		c.block = panicBlock
		panicMsg := c.makeGlobalString("cast failed: as! type mismatch")
		c.block.NewCall(c.funcs["promise_panic"], panicMsg)
		c.emitPanicReturn()

		c.block = okBlock
		return castResult // same value struct, type is verified
	}

	// T0849: if the subject is a movable owned local/`~`-param, record the runtime
	// downcast success flag so a consuming site (return / owning-slot store) can
	// drop the subject iff the cast failed (None). castSubjectMovableIdent peels
	// parens/chained casts to the innermost subject ident that carries a drop flag.
	// `as` is a conditional move: the subject's instance is aliased into `some`
	// only on success, untouched on failure — so an unconditional clear/keep is
	// wrong in both consuming contexts. consumeCastSubjectDropFlag reuses isMatch.
	// A borrow target (`x as T&`) is a reborrow with no ownership transfer, so the
	// subject's instance is never moved into `some` — skip the conditional
	// drop-flag handoff that would otherwise let a consuming site drop the subject
	// (T0848). castSubjectMovableIdent already returns nil for borrow-param
	// subjects; the guard makes the no-move invariant explicit and protects the
	// `ownedLocal as T&` shape, where the local must remain owned.
	if ident := c.castSubjectMovableIdent(e); !targetIsBorrow && ident != nil {
		if c.castSubjectMatch == nil {
			c.castSubjectMatch = map[string]value.Value{}
		}
		c.castSubjectMatch[ident.Name] = isMatch
	}

	// as — wrap in Optional { i1, { i8*, i8* } }. User types use value struct representation.
	someBlock := c.newBlock("cast.some")
	noneBlock := c.newBlock("cast.none")
	mergeBlock := c.newBlock("cast.merge")
	c.block.NewCondBr(isMatch, someBlock, noneBlock)

	c.block = someBlock
	optType := irtypes.NewStruct(irtypes.I1, userValueType())
	someResult := c.wrapOptional(castResult, optType)
	c.block.NewBr(mergeBlock)
	someEnd := c.block

	c.block = noneBlock
	noneResult := constant.NewZeroInitializer(optType)
	c.block.NewBr(mergeBlock)
	noneEnd := c.block

	c.block = mergeBlock
	phi := c.block.NewPhi(
		&ir.Incoming{X: someResult, Pred: someEnd},
		&ir.Incoming{X: noneResult, Pred: noneEnd},
	)
	return phi
}

// genOptionalCastExpr generates code for an RTTI cast whose subject is itself an
// Optional: `opt as! Subtype` (force) or `opt as Target` (optional). T0761.
//
// genExpr(e.Expr) yields the `{ i1, {i8*,i8*} }` optional representation. RTTI
// casts apply only to reference types, so the inner (field 1) is always the
// `{i8*,i8*}` userValueType() value struct. We extract it, give the cast result
// a clean ownership story for the inner across every source shape (see the
// ownership block below), then mirror the non-optional cast paths:
//
//   - Force (`as!`): on none OR type mismatch → panic; otherwise return the
//     inner value struct (its view-vtable dispatches correctly, like the
//     non-optional `as!` path).
//   - Optional (`as`): on none OR mismatch → none; otherwise move the inner into
//     `some` and neutralize the source inside the match-only block.
//
// Inner-ownership is reconciled by three coordinated pieces, keyed on the source
// shape so exactly one of them frees the inner on every (present×match) path:
//   - aliasing sources (container element `v[i]`, borrowed `this.field`): the
//     inner is borrowed from an external owner the cast cannot neutralize, so we
//     dup it into an owned copy up front (the external owner still frees the
//     original).
//   - owned sources (the duped copy above, or a call-result temp): the result
//     owns the inner; the `as` path registers it as a heap temp so it is freed
//     on present+mismatch (claimed by the binding on match).
//   - local ident/member sources: the source's own drop binding owns the inner;
//     neutralizeOptionalCastSource (as) / neutralizeForceUnwrapSource at the
//     binding (force, B0293) clears it on the match path only.
//
// T0850: borrowSource is set when the subject is a borrowed optional (`T?&`,
// e.g. `Ref[T?].borrow`). A borrow's inner is owned by an external owner (the
// Arc/Mutex payload) the cast can neither move nor neutralize, so all three
// ownership decisions collapse to the aliasing case: dup the inner up front,
// never neutralize the source (a borrow getter is a MemberExpr whose leaf is a
// getter — neutralizing would mis-resolve it as a field), and let the result own
// the dup (heap-temp tracked so present+mismatch frees it, match claims it).
func (c *Compiler) genOptionalCastExpr(e *ast.CastExpr, opt *types.Optional, targetNamed *types.Named, borrowSource bool) value.Value {
	// T0761: scalar optional subject (`int? as f64` / `char? as! int`). The inner
	// (optional field 1) is a bare scalar, not a `{vtable,instance}` value struct,
	// so the RTTI path below would extractvalue a non-aggregate and panic. Mirror
	// the non-optional scalar path (emitScalarCast): unwrap, convert, (re)wrap.
	elem := opt.Elem()
	if c.typeSubst != nil {
		elem = types.Substitute(elem, c.typeSubst)
	}
	if elemNamed := extractNamed(elem); elemNamed != nil &&
		isPrimitiveScalar(elemNamed) && isPrimitiveScalar(targetNamed) {
		return c.genOptionalScalarCastExpr(e, elemNamed, targetNamed)
	}

	targetID := c.assignTypeID(targetNamed)

	// T0761: Take full ownership control of the subject's inner rather than
	// relying on the binding context's ambient field/index dup (which fires only
	// for an Optional[heap-user-type] LHS and only dups *some* source shapes —
	// container elements but not synth-drop fields — making ownership of `inner`
	// unpredictable from here). Suppress the ambient heap-user-type dup so genExpr
	// yields the raw aliased inner, then dup aliasing sources uniformly below.
	savedDupHeap := c.dupHeapUserFieldAccess
	c.dupHeapUserFieldAccess = false
	optVal := c.genExpr(e.Expr)
	c.dupHeapUserFieldAccess = savedDupHeap

	flag := c.block.NewExtractValue(optVal, 0)
	var inner value.Value = c.block.NewExtractValue(optVal, 1) // {i8*,i8*} value struct

	// For an aliasing source — a container element (`v[i]`) or a borrowed
	// `this.field` — `inner` is borrowed from an external owner that the cast
	// neither moves nor neutralizes (the container/caller still frees it). Dup it
	// so the cast result owns an independent copy, mirroring genOptionalForceUnwrap's
	// borrowed-`this.field` dup. Local-rooted ident/member sources are neutralized
	// instead (B0293), and owned temps (call results) own their inner outright, so
	// neither is duped. dupHeapValue is null-safe, so this is correct even when the
	// optional is none (the dup is a no-op on a null instance).
	if borrowSource || c.optionalCastSourceAliasesExternalOwner(e.Expr) {
		elem := opt.Elem()
		if c.typeSubst != nil {
			elem = types.Substitute(elem, c.typeSubst)
		}
		if named := extractNamed(elem); named != nil && !named.IsValueType() &&
			!named.IsCopy() && !isPrimitiveScalar(named) && named != types.TypString &&
			!types.IsVector(elem) && !types.IsChannel(elem) && !named.IsStructural() &&
			!isOpaqueContainerType(elem) {
			inner = c.dupHeapValue(inner, elem)
		}
	}

	if e.Force {
		// as! — panic on none, then panic on type mismatch, else return inner.
		presentBlock := c.newBlock("optcast.present")
		nonePanicBlock := c.newBlock("optcast.nonepanic")
		c.block.NewCondBr(flag, presentBlock, nonePanicBlock)

		c.block = nonePanicBlock
		nonePanicMsg := c.makeGlobalString("cast failed: optional is none")
		c.block.NewCall(c.funcs["promise_panic"], nonePanicMsg)
		c.emitPanicReturn()

		c.block = presentBlock
		instance := c.instancePtrForRTTI(inner, opt.Elem())
		variantPtr := c.loadVariantPtr(instance)
		result := c.block.NewCall(c.funcs["promise_type_is"],
			variantPtr, constant.NewInt(irtypes.I32, int64(targetID)))
		isMatch := c.block.NewICmp(enum.IPredNE, result, constant.NewInt(irtypes.I32, 0))

		okBlock := c.newBlock("optcast.ok")
		mismatchBlock := c.newBlock("optcast.mismatch")
		c.block.NewCondBr(isMatch, okBlock, mismatchBlock)

		c.block = mismatchBlock
		mismatchMsg := c.makeGlobalString("cast failed: as! type mismatch")
		c.block.NewCall(c.funcs["promise_panic"], mismatchMsg)
		c.emitPanicReturn()

		c.block = okBlock
		return inner // value struct, type verified; source neutralized (ident/member) or duped (aliasing) above
	}

	// as — none on absent OR mismatch, conditionally move inner into some.
	optType := irtypes.NewStruct(irtypes.I1, userValueType())
	checkBlock := c.newBlock("optcast.check")
	someBlock := c.newBlock("optcast.some")
	noneBlock := c.newBlock("optcast.none")
	mergeBlock := c.newBlock("optcast.merge")
	c.block.NewCondBr(flag, checkBlock, noneBlock)

	c.block = checkBlock
	instance := c.instancePtrForRTTI(inner, opt.Elem())
	variantPtr := c.loadVariantPtr(instance)
	result := c.block.NewCall(c.funcs["promise_type_is"],
		variantPtr, constant.NewInt(irtypes.I32, int64(targetID)))
	isMatch := c.block.NewICmp(enum.IPredNE, result, constant.NewInt(irtypes.I32, 0))
	// T0761: When the cast result owns `inner` (an owned-temp call result, or an
	// aliasing source we duped above) there is no external owner to free it on the
	// present+mismatch path → leak. Register `inner` as a heap temp here (we are in
	// checkBlock, so the present flag is known true and `inner`'s instance is
	// valid): claimHeapTemp at the binding transfers ownership on match, and
	// cleanupHeapTemps frees it on mismatch — exactly the non-optional cast's
	// behavior. Ident/local-member sources are skipped: their inner is owned by the
	// source's own drop binding (neutralized only on the match path), so tracking
	// here would double-free.
	if borrowSource || c.optionalCastResultOwnsInner(e.Expr) {
		elem := opt.Elem()
		if c.typeSubst != nil {
			elem = types.Substitute(elem, c.typeSubst)
		}
		if dropFunc := c.resolveDropFuncForTemp(extractNamed(elem), elem); dropFunc != nil {
			instPtr := c.extractInstancePtr(inner)
			if instPtr.Type() != irtypes.I8Ptr {
				instPtr = c.block.NewBitCast(instPtr, irtypes.I8Ptr)
			}
			c.trackHeapTemp(instPtr, dropFunc)
		}
	}
	c.block.NewCondBr(isMatch, someBlock, noneBlock)

	c.block = someBlock
	// Conditional move: only on present+match does the result take ownership of
	// the inner; clear the source optional's present flag so its drop becomes a
	// no-op. On none/mismatch the source keeps & frees the inner.
	// T0850: a borrowed optional has no local present flag to clear (the inner was
	// duped above; the external owner keeps & frees the original), so skip.
	if !borrowSource {
		c.neutralizeOptionalCastSource(e.Expr)
	}
	someResult := c.wrapOptional(inner, optType)
	c.block.NewBr(mergeBlock)
	someEnd := c.block

	c.block = noneBlock
	noneResult := constant.NewZeroInitializer(optType)
	c.block.NewBr(mergeBlock)
	noneEnd := c.block

	c.block = mergeBlock
	phi := c.block.NewPhi(
		&ir.Incoming{X: someResult, Pred: someEnd},
		&ir.Incoming{X: noneResult, Pred: noneEnd},
	)
	return phi
}

// genOptionalScalarCastExpr lowers a scalar-to-scalar cast whose subject is an
// Optional (`int? as f64` force, or `int? as f64` optional). T0761. Scalars are
// “ `copy “ with no drop, so there is no ownership/neutralization to do — the
// inner is a bare scalar (optional field 1), not a value struct.
//
//   - Force (`as!`): panic on none, else convert and return the scalar.
//   - Optional (`as`): none on absent; present → some(convert(inner)).
func (c *Compiler) genOptionalScalarCastExpr(e *ast.CastExpr, srcNamed, targetNamed *types.Named) value.Value {
	optVal := c.genExpr(e.Expr)
	flag := c.block.NewExtractValue(optVal, 0)
	inner := c.block.NewExtractValue(optVal, 1) // bare scalar (not a value struct)

	if e.Force {
		presentBlock := c.newBlock("optcast.present")
		nonePanicBlock := c.newBlock("optcast.nonepanic")
		c.block.NewCondBr(flag, presentBlock, nonePanicBlock)

		c.block = nonePanicBlock
		nonePanicMsg := c.makeGlobalString("cast failed: optional is none")
		c.block.NewCall(c.funcs["promise_panic"], nonePanicMsg)
		c.emitPanicReturn()

		c.block = presentBlock
		return c.emitScalarCast(inner, srcNamed, targetNamed)
	}

	// as — none on absent; present → some(convert(inner)).
	optType := irtypes.NewStruct(irtypes.I1, llvmNamedType(targetNamed))
	someBlock := c.newBlock("optcast.some")
	noneBlock := c.newBlock("optcast.none")
	mergeBlock := c.newBlock("optcast.merge")
	c.block.NewCondBr(flag, someBlock, noneBlock)

	c.block = someBlock
	converted := c.emitScalarCast(inner, srcNamed, targetNamed)
	someResult := c.wrapOptional(converted, optType)
	c.block.NewBr(mergeBlock)
	someEnd := c.block

	c.block = noneBlock
	noneResult := constant.NewZeroInitializer(optType)
	c.block.NewBr(mergeBlock)
	noneEnd := c.block

	c.block = mergeBlock
	return c.block.NewPhi(
		&ir.Incoming{X: someResult, Pred: someEnd},
		&ir.Incoming{X: noneResult, Pred: noneEnd},
	)
}

// neutralizeOptionalCastSource clears the present flag of an Optional cast
// source (`opt as Target`) so the source's drop skips the inner value once the
// cast result has taken ownership of it (T0761). Shared with the force-unwrap
// path: handles owned-ident and owned-member sources (peeling ParenExpr),
// reusing neutralizeMemberOptionalField for the member case. Temp/call-result
// sources fall through as a no-op (their inner stays owned by their own temp
// tracking), identical to the existing opt!-on-temp behavior.
func (c *Compiler) neutralizeOptionalCastSource(expr ast.Expr) {
	for {
		p, ok := expr.(*ast.ParenExpr)
		if !ok {
			break
		}
		expr = p.Expr
	}
	switch src := expr.(type) {
	case *ast.IdentExpr:
		alloca, ok := c.locals[src.Name]
		if !ok {
			return
		}
		optType, ok := alloca.ElemType.(*irtypes.StructType)
		if !ok || len(optType.Fields) < 2 {
			return
		}
		flagPtr := c.block.NewGetElementPtr(optType, alloca,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
		c.block.NewStore(constant.NewInt(irtypes.I1, 0), flagPtr)
	case *ast.MemberExpr:
		c.neutralizeMemberOptionalField(src)
	}
}

// optionalCastSourceAliasesExternalOwner reports whether an Optional cast
// subject's inner is borrowed from an external owner that the cast neither moves
// nor neutralizes, so genOptionalCastExpr must dup the inner to give the result
// an independent copy (otherwise both the external owner and the result free it
// → double-free). Two such shapes, mirroring genOptionalForceUnwrap's dup cases:
//   - a container-element access (`v[i]`): the container's drop frees the
//     element; neutralizeOptionalCastSource doesn't handle IndexExpr.
//   - a borrowed-`this.field` access inside a `&this` method (T0428 Case 3B):
//     neutralizeMemberOptionalField deliberately skips it (can't clear a present
//     flag through a borrowed receiver), so the caller still owns the original.
//
// Local-rooted ident/member sources are neutralized instead (their owner's drop
// is skipped on the match path), and owned temps (call results) own their inner
// outright, so none of those is duped. ParenExpr is peeled. T0761.
func (c *Compiler) optionalCastSourceAliasesExternalOwner(expr ast.Expr) bool {
	switch e := unwrapDestructureParens(expr).(type) {
	case *ast.IndexExpr:
		return true
	case *ast.MemberExpr:
		return isThisReceiver(e.Target) && !c.thisRecvIsOwned
	}
	return false
}

// optionalCastResultOwnsInner reports whether the cast result owns `inner` (so
// the `as` path must free it on the present+mismatch branch via a heap temp).
// The result owns `inner` for every source EXCEPT a local-rooted ident or member
// optional, whose own drop binding frees the inner (neutralized only on the
// match path). Aliasing sources (index / borrowed-`this.field`) are duped into an
// owned copy by genOptionalCastExpr, and owned temps (call results) own their
// inner outright — both are owned. ParenExpr is peeled. T0761.
func (c *Compiler) optionalCastResultOwnsInner(expr ast.Expr) bool {
	switch e := unwrapDestructureParens(expr).(type) {
	case *ast.IdentExpr:
		return false
	case *ast.MemberExpr:
		// Borrowed-`this.field` is duped (owned); a local-owner field is neutralized.
		return isThisReceiver(e.Target) && !c.thisRecvIsOwned
	}
	return true
}

// emitScalarCast emits LLVM IR for a primitive scalar type conversion.
// Handles int↔int (trunc/sext/zext), float↔float (fptrunc/fpext),
// int→float (sitofp/uitofp), float→int (fptosi/fptoui),
// char↔int (trunc/zext — char is i32 codepoint),
// bool→int/char (zext), int/char→bool (icmp ne 0), float→bool (fcmp one 0.0),
// bool→float (uitofp).
func (c *Compiler) emitScalarCast(val value.Value, src, dst *types.Named) value.Value {
	srcLLVM := llvmNamedType(src)
	dstLLVM := llvmNamedType(dst)

	srcInt, srcIsInt := srcLLVM.(*irtypes.IntType)
	dstInt, dstIsInt := dstLLVM.(*irtypes.IntType)
	_, srcIsFloat := srcLLVM.(*irtypes.FloatType)
	dstFloat, dstIsFloat := dstLLVM.(*irtypes.FloatType)

	dstIsBool := dst == types.TypBool

	switch {
	case srcIsInt && dstIsInt:
		if srcInt.BitSize == dstInt.BitSize {
			return val // same width: no-op (e.g., int ↔ uint, char ↔ i32)
		} else if dstIsBool {
			// int/char → bool: non-zero = true (icmp ne, not trunc)
			zero := constant.NewInt(srcInt, 0)
			return c.block.NewICmp(enum.IPredNE, val, zero)
		} else if srcInt.BitSize > dstInt.BitSize {
			return c.block.NewTrunc(val, dstInt)
		} else if isSignedType(src) {
			return c.block.NewSExt(val, dstInt)
		} else {
			return c.block.NewZExt(val, dstInt)
		}
	case srcIsFloat && dstIsFloat:
		srcFloat := srcLLVM.(*irtypes.FloatType)
		if srcFloat == dstFloat {
			return val
		} else if srcFloat == irtypes.Float {
			return c.block.NewFPExt(val, dstFloat)
		}
		return c.block.NewFPTrunc(val, dstFloat)
	case srcIsInt && dstIsFloat:
		if isSignedType(src) {
			return c.block.NewSIToFP(val, dstFloat)
		}
		return c.block.NewUIToFP(val, dstFloat)
	case srcIsFloat && dstIsInt:
		if dstIsBool {
			// float → bool: non-zero = true (une handles NaN as truthy)
			zero := constant.NewFloat(srcLLVM.(*irtypes.FloatType), 0.0)
			return c.block.NewFCmp(enum.FPredUNE, val, zero)
		}
		if isSignedType(dst) {
			return c.block.NewFPToSI(val, dstInt)
		}
		return c.block.NewFPToUI(val, dstInt)
	default:
		panic(fmt.Sprintf("codegen: unsupported scalar cast %s → %s", src, dst))
	}
}

// genStructuralDowncast generates code for `expr as! Target` / `expr as Target`
// where expr is a structural interface value. T1884.
//
// Uses vtable pointer comparison instead of RTTI: each (concrete, structural)
// pair has a unique view vtable emitted during boxing, so comparing field 0 of
// the fat value against the expected view vtable for (target, srcStructural)
// identifies the concrete type for all boxed kinds — primitives, strings, value
// types, heap user types, and opaque containers. After the match, the concrete
// value is extracted from the box via unboxStructuralCast.
func (c *Compiler) genStructuralDowncast(e *ast.CastExpr, subject value.Value, srcNamed, targetNamed *types.Named, targetType types.Type, _ bool) value.Value {
	// Get or emit the view vtable for (target, srcStructural).
	viewVtable := c.getOrEmitViewVtable(targetNamed, srcNamed, targetType)
	expectedVtable := constant.NewBitCast(viewVtable, irtypes.I8Ptr)

	// Compare vtable pointers (field 0 of the fat value) for exact type match.
	actualVtable := c.extractVtablePtr(subject)
	isMatch := c.block.NewICmp(enum.IPredEQ, actualVtable, expectedVtable)

	if e.Force {
		// as! — panic on mismatch, then unbox.
		okBlock := c.newBlock("cast.ok")
		panicBlock := c.newBlock("cast.panic")
		c.block.NewCondBr(isMatch, okBlock, panicBlock)

		c.block = panicBlock
		panicMsg := c.makeGlobalString("cast failed: as! type mismatch")
		c.block.NewCall(c.funcs["promise_panic"], panicMsg)
		c.emitPanicReturn()

		c.block = okBlock
		return c.unboxStructuralCast(subject, targetNamed, targetType)
	}

	// as — wrap in Optional with the correct inner LLVM type.
	someBlock := c.newBlock("cast.some")
	noneBlock := c.newBlock("cast.none")
	mergeBlock := c.newBlock("cast.merge")
	c.block.NewCondBr(isMatch, someBlock, noneBlock)

	c.block = someBlock
	castResult := c.unboxStructuralCast(subject, targetNamed, targetType)
	innerLLVM := c.structuralDowncastLLVMType(targetNamed, targetType)
	optType := irtypes.NewStruct(irtypes.I1, innerLLVM)
	someResult := c.wrapOptional(castResult, optType)
	c.block.NewBr(mergeBlock)
	someEnd := c.block

	c.block = noneBlock
	noneResult := constant.NewZeroInitializer(optType)
	c.block.NewBr(mergeBlock)
	noneEnd := c.block

	c.block = mergeBlock
	phi := c.block.NewPhi(
		&ir.Incoming{X: someResult, Pred: someEnd},
		&ir.Incoming{X: noneResult, Pred: noneEnd},
	)
	return phi
}

// unboxStructuralCast extracts the concrete value from a structural interface
// fat value ({view_vtable, instance_ptr}). The extraction depends on how the
// concrete type was boxed — see boxForStructuralView / boxValueTypeForStructuralView.
func (c *Compiler) unboxStructuralCast(subject value.Value, targetNamed *types.Named, targetType types.Type) value.Value {
	instancePtr := c.extractInstancePtr(subject) // field 1 of fat value

	if isPrimitiveScalar(targetNamed) {
		// Box layout: {i8* typeinfo, scalarT}. Extract the scalar from field 1.
		scalarType := llvmNamedType(targetNamed)
		boxType := irtypes.NewStruct(irtypes.I8Ptr, scalarType)
		typedBox := c.block.NewBitCast(instancePtr, irtypes.NewPointer(boxType))
		scalarField := c.block.NewGetElementPtr(boxType, typedBox,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 1))
		return c.block.NewLoad(scalarType, scalarField)
	}

	if targetNamed == types.TypString {
		// Box layout: {i8* typeinfo, i8* string_ptr}. Extract and dup the string
		// for ownership — the box still owns its original copy and will be freed
		// by the structural interface's drop.
		boxType := irtypes.NewStruct(irtypes.I8Ptr, irtypes.I8Ptr)
		typedBox := c.block.NewBitCast(instancePtr, irtypes.NewPointer(boxType))
		strField := c.block.NewGetElementPtr(boxType, typedBox,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 1))
		strPtr := c.block.NewLoad(irtypes.I8Ptr, strField)
		return c.dupString(strPtr)
	}

	if isOpaqueContainerType(targetType) {
		// Opaque containers (Vector, Channel, Task, etc.) are boxed as raw i8*.
		// The instance pointer IS the container pointer.
		return instancePtr
	}

	if targetNamed.IsValueType() {
		// Value type box: heap-allocated copy of the value struct with field 0
		// overwritten with typeinfo. Load the full struct, restore the vtable.
		layout := c.lookupTypeLayout(targetType)
		if layout != nil && layout.Value != nil {
			valType := layout.Value.LLVMType
			typedBox := c.block.NewBitCast(instancePtr, irtypes.NewPointer(valType))
			loaded := c.block.NewLoad(valType, typedBox)
			// Restore field 0: load the concrete vtable from the typeinfo chain.
			vtablePtr := c.loadVtablePtrFromInstance(instancePtr)
			return c.block.NewInsertValue(loaded, vtablePtr, 0)
		}
	}

	// Heap user type: instance ptr IS the real instance (no separate box).
	// Reconstruct the concrete {vtable, instance} value struct.
	vtablePtr := c.loadVtablePtrFromInstance(instancePtr)
	var vs value.Value = constant.NewUndef(userValueType())
	vs = c.block.NewInsertValue(vs, vtablePtr, 0)
	vs = c.block.NewInsertValue(vs, instancePtr, 1)
	return vs
}

// structuralDowncastLLVMType returns the LLVM type of the unboxed result for a
// structural interface downcast. Used to construct the correct Optional type for
// the `as` (non-force) path. T1884.
func (c *Compiler) structuralDowncastLLVMType(targetNamed *types.Named, targetType types.Type) irtypes.Type {
	if isPrimitiveScalar(targetNamed) {
		return llvmNamedType(targetNamed)
	}
	if targetNamed == types.TypString {
		return irtypes.I8Ptr
	}
	if isOpaqueContainerType(targetType) {
		return irtypes.I8Ptr
	}
	if targetNamed.IsValueType() {
		if layout := c.lookupTypeLayout(targetType); layout != nil && layout.Value != nil {
			return layout.Value.LLVMType
		}
	}
	return userValueType()
}
