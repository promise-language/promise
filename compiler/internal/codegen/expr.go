package codegen

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/llir/llvm/ir"
	"github.com/llir/llvm/ir/constant"
	"github.com/llir/llvm/ir/enum"
	irtypes "github.com/llir/llvm/ir/types"
	"github.com/llir/llvm/ir/value"

	"djabi.dev/go/promise_lang/internal/ast"
	"djabi.dev/go/promise_lang/internal/types"
)

// genExpr generates LLVM IR for an expression and returns the resulting value.
func (c *Compiler) genExpr(expr ast.Expr) value.Value {
	if expr == nil {
		return nil
	}
	switch e := expr.(type) {
	case *ast.IntLit:
		return c.genIntLit(e)
	case *ast.FloatLit:
		return c.genFloatLit(e)
	case *ast.BoolLit:
		return c.genBoolLit(e)
	case *ast.StringLit:
		return c.genStringLit(e)
	case *ast.CharLit:
		return c.genCharLit(e)
	case *ast.IdentExpr:
		return c.genIdentExpr(e)
	case *ast.ParenExpr:
		return c.genExpr(e.Expr)
	case *ast.BinaryExpr:
		return c.genBinaryExpr(e)
	case *ast.UnaryExpr:
		return c.genUnaryExpr(e)
	case *ast.CallExpr:
		return c.genCallExpr(e)
	case *ast.MemberExpr:
		return c.genMemberExpr(e)
	case *ast.ThisExpr:
		return c.genThisExpr()
	case *ast.IfExpr:
		return c.genIfExpr(e)
	case *ast.MatchExpr:
		return c.genMatchExpr(e)
	case *ast.ErrorPropagateExpr:
		return c.genErrorPropagateExpr(e)
	case *ast.ErrorUnwrapExpr:
		return c.genErrorUnwrapExpr(e)
	case *ast.ErrorHandlerExpr:
		return c.genErrorHandlerExpr(e)
	case *ast.TupleLit:
		return c.genTupleLit(e)
	case *ast.NoneLit:
		return c.genNoneLit(e)
	case *ast.ArrayLit:
		return c.genArrayLit(e)
	case *ast.MapLit:
		return c.genMapLit(e)
	case *ast.IndexExpr:
		return c.genIndexExpr(e)
	case *ast.SliceExpr:
		return c.genSliceExpr(e)
	case *ast.SliceTypeExpr:
		// Type expression in expression position; only used as constructor callee.
		// genCallExpr handles this via c.info.Types lookup, not genExpr.
		return nil
	case *ast.LambdaExpr:
		return c.genLambdaExpr(e)
	case *ast.OptionalChainExpr:
		return c.genOptionalChainExpr(e)
	case *ast.UnsafeExpr:
		c.genBlock(e.Body)
		return nil
	case *ast.IsExpr:
		return c.genIsExpr(e)
	case *ast.CastExpr:
		return c.genCastExpr(e)
	case *ast.GoExpr:
		return c.genGoExpr(e)
	default:
		panic(fmt.Sprintf("codegen: unhandled expression type %T", expr))
	}
}

// --- Literals ---

func (c *Compiler) genIntLit(e *ast.IntLit) value.Value {
	typ := c.info.Types[e]
	named := extractNamed(typ)
	if named == nil {
		named = types.TypInt
	}
	lt := llvmNamedType(named)
	intType, ok := lt.(*irtypes.IntType)
	if !ok {
		intType = irtypes.I64
	}
	raw := strings.ReplaceAll(e.Raw, "_", "")
	val, err := strconv.ParseInt(raw, 0, 64)
	if err != nil {
		// Try unsigned parse for large values
		uval, _ := strconv.ParseUint(raw, 0, 64)
		return constant.NewInt(intType, int64(uval))
	}
	return constant.NewInt(intType, val)
}

func (c *Compiler) genFloatLit(e *ast.FloatLit) value.Value {
	typ := c.info.Types[e]
	named := extractNamed(typ)
	if named == nil {
		named = types.TypF64
	}
	lt := llvmNamedType(named)
	floatType, ok := lt.(*irtypes.FloatType)
	if !ok {
		floatType = irtypes.Double
	}
	raw := strings.ReplaceAll(e.Raw, "_", "")
	val, _ := strconv.ParseFloat(raw, 64)
	return constant.NewFloat(floatType, val)
}

func (c *Compiler) genBoolLit(e *ast.BoolLit) value.Value {
	if e.Value {
		return constant.NewInt(irtypes.I1, 1)
	}
	return constant.NewInt(irtypes.I1, 0)
}

func (c *Compiler) genCharLit(e *ast.CharLit) value.Value {
	raw := e.Raw
	inner := raw[1 : len(raw)-1] // strip surrounding quotes
	var cp int32
	if len(inner) > 1 && inner[0] == '\\' {
		switch inner[1] {
		case 'n':
			cp = '\n'
		case 'r':
			cp = '\r'
		case 't':
			cp = '\t'
		case 'b':
			cp = '\b'
		case '\\':
			cp = '\\'
		case '\'':
			cp = '\''
		case '0':
			cp = 0
		default:
			cp = int32(inner[1])
		}
	} else {
		r, _ := utf8.DecodeRuneInString(inner)
		cp = int32(r)
	}
	return constant.NewInt(irtypes.I32, int64(cp))
}

func (c *Compiler) genStringLit(e *ast.StringLit) value.Value {
	if hasInterpolation(e.Parts) {
		return c.genInterpolatedString(e)
	}
	return c.genStaticString(e)
}

// genStaticString handles strings with no interpolation — compile-time constant path.
func (c *Compiler) genStaticString(e *ast.StringLit) value.Value {
	var buf strings.Builder
	for _, part := range e.Parts {
		switch p := part.(type) {
		case ast.StringText:
			buf.WriteString(p.Text)
		case ast.StringEscape:
			buf.WriteString(resolveEscape(p.Sequence))
		}
	}
	return c.makeRuntimeString(buf.String())
}

// genInterpolatedString handles strings with interpolation — runtime concatenation path.
func (c *Compiler) genInterpolatedString(e *ast.StringLit) value.Value {
	var parts []value.Value
	var staticBuf strings.Builder

	for _, part := range e.Parts {
		switch p := part.(type) {
		case ast.StringText:
			staticBuf.WriteString(p.Text)
		case ast.StringEscape:
			staticBuf.WriteString(resolveEscape(p.Sequence))
		case ast.StringInterp:
			// Skip interpolation with nil Expr (empty {} or parse failure —
			// sema reports the error; treat as empty string to avoid panic).
			if p.Expr == nil {
				continue
			}
			// Flush static buffer as a string
			if staticBuf.Len() > 0 {
				parts = append(parts, c.makeRuntimeString(staticBuf.String()))
				staticBuf.Reset()
			}
			// Evaluate expression and convert to string
			val := c.genExpr(p.Expr)
			strVal := c.convertToString(val, c.info.Types[p.Expr])
			parts = append(parts, strVal)
		}
	}
	// Flush remaining static text
	if staticBuf.Len() > 0 {
		parts = append(parts, c.makeRuntimeString(staticBuf.String()))
	}

	// Concatenate all parts
	if len(parts) == 0 {
		return c.makeRuntimeString("")
	}
	result := parts[0]
	for _, part := range parts[1:] {
		result = c.block.NewCall(c.funcs["promise_string_concat"], result, part)
	}
	return result
}

// makeRuntimeString creates a global string constant and calls promise_string_new.
// When compiling module code, names use a per-module counter so the constant
// names are stable (independent of how many string constants user code has).
func (c *Compiler) makeRuntimeString(s string) value.Value {
	data := constant.NewCharArrayFromString(s)
	var globalName string
	if c.compilingModule != "" {
		globalName = fmt.Sprintf(".str.__mod_%s.%d", c.compilingModule, c.moduleStrCounter)
		c.moduleStrCounter++
	} else {
		globalName = fmt.Sprintf(".str.%d", c.strCounter)
		c.strCounter++
	}
	global := c.module.NewGlobalDef(globalName, data)
	global.Immutable = true
	global.Linkage = enum.LinkagePrivate

	ptr := c.block.NewGetElementPtr(global.ContentType, global,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))

	return c.block.NewCall(c.funcs["promise_string_new"],
		ptr, constant.NewInt(irtypes.I64, int64(len(s))))
}

// convertToString converts a value to a string (i8*) for interpolation.
func (c *Compiler) convertToString(val value.Value, typ types.Type) value.Value {
	// Handle optional types: print inner value if present, "none" if absent.
	if opt, ok := typ.(*types.Optional); ok {
		flag := c.block.NewExtractValue(val, 0)
		someBlock := c.newBlock("interp.some")
		noneBlock := c.newBlock("interp.none")
		mergeBlock := c.newBlock("interp.merge")
		c.block.NewCondBr(flag, someBlock, noneBlock)

		c.block = someBlock
		innerVal := c.block.NewExtractValue(val, 1)
		someStr := c.convertToString(innerVal, opt.Elem())
		someEnd := c.block
		c.block.NewBr(mergeBlock)

		c.block = noneBlock
		noneStr := c.makeRuntimeString("none")
		noneEnd := c.block
		c.block.NewBr(mergeBlock)

		c.block = mergeBlock
		phi := c.block.NewPhi(ir.NewIncoming(someStr, someEnd), ir.NewIncoming(noneStr, noneEnd))
		return phi
	}

	named := extractNamed(typ)
	if named == nil {
		panic(fmt.Sprintf("codegen: cannot convert %s to string for interpolation", typ))
	}
	switch named {
	case types.TypString:
		return val // already a string
	case types.TypInt, types.TypI64:
		return c.block.NewCall(c.funcs["promise_int_to_string"], val)
	case types.TypI32:
		ext := c.block.NewSExt(val, irtypes.I64)
		return c.block.NewCall(c.funcs["promise_int_to_string"], ext)
	case types.TypI16:
		ext := c.block.NewSExt(val, irtypes.I64)
		return c.block.NewCall(c.funcs["promise_int_to_string"], ext)
	case types.TypI8:
		ext := c.block.NewSExt(val, irtypes.I64)
		return c.block.NewCall(c.funcs["promise_int_to_string"], ext)
	case types.TypUint, types.TypU64:
		return c.block.NewCall(c.funcs["promise_uint_to_string"], val)
	case types.TypU32, types.TypU16, types.TypU8:
		ext := c.block.NewZExt(val, irtypes.I64)
		return c.block.NewCall(c.funcs["promise_uint_to_string"], ext)
	case types.TypF64:
		return c.block.NewCall(c.funcs["promise_f64_to_string"], val)
	case types.TypF32:
		ext := c.block.NewFPExt(val, irtypes.Double)
		return c.block.NewCall(c.funcs["promise_f64_to_string"], ext)
	case types.TypBool:
		i8Val := c.block.NewZExt(val, irtypes.I8)
		return c.block.NewCall(c.funcs["promise_bool_to_string"], i8Val)
	case types.TypChar:
		return c.block.NewCall(c.funcs["promise_char_to_string"], val)
	default:
		// User-defined type: call format(Writer ~w)! via Builder
		if named.LookupMethod("format") == nil {
			panic(fmt.Sprintf("codegen: type %s has no format method for interpolation", typ))
		}
		return c.callFormatToString(val, typ, named)
	}
}

// callFormatToString creates a Builder, calls the type's format() method to write
// into it, then returns the resulting string from Builder.to_string().
func (c *Compiler) callFormatToString(val value.Value, typ types.Type, named *types.Named) value.Value {
	// 1. Create a Builder instance
	builderNamed := c.lookupNamedType("Builder")
	layout := c.layouts[builderNamed]
	if layout == nil {
		panic("codegen: Builder type layout not found")
	}
	instanceStructType := layout.Instance.LLVMType
	instancePtrType := layout.InstancePtrType

	// Compute size via GEP-from-null trick
	nullPtr := constant.NewNull(instancePtrType)
	sizePtr := c.block.NewGetElementPtr(instanceStructType, nullPtr,
		constant.NewInt(irtypes.I32, 1))
	sizeRaw := c.block.NewPtrToInt(sizePtr, c.ptrIntType())
	var size value.Value = sizeRaw
	if c.isWasm {
		size = c.block.NewZExt(sizeRaw, irtypes.I64)
	}

	rawPtr := c.block.NewCall(c.palAlloc, size)
	typedPtr := c.block.NewBitCast(rawPtr, instancePtrType)

	// Store type info pointer in _variant slot (field 0)
	variantFieldPtr := c.block.NewGetElementPtr(instanceStructType, typedPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
	variantPtrType := layout.Instance.Fields[0].LLVMType.(*irtypes.PointerType)
	if tiGlobal := c.typeInfoGlobals[builderNamed]; tiGlobal != nil {
		c.block.NewStore(c.block.NewBitCast(tiGlobal, variantPtrType), variantFieldPtr)
	} else {
		c.block.NewStore(constant.NewNull(variantPtrType), variantFieldPtr)
	}

	// Zero-init remaining fields before calling new()
	for _, f := range builderNamed.AllFields() {
		fieldIdx, ok := layout.InstanceFieldIndex[f.Name()]
		if !ok {
			continue
		}
		fieldPtr := c.block.NewGetElementPtr(instanceStructType, typedPtr,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(fieldIdx)))
		c.block.NewStore(c.zeroValue(layout.Instance.Fields[fieldIdx].LLVMType), fieldPtr)
	}

	// Call Builder.new(this, 16) — default capacity
	newFn := c.funcs["Builder.new"]
	c.block.NewCall(newFn, rawPtr, constant.NewInt(irtypes.I64, 16))

	// 2. Create Writer value struct {vtable_ptr, instance_ptr} from Builder
	writerVtable := c.getInterpBuilderWriterVtable()
	writerVal := c.block.NewInsertValue(
		constant.NewZeroInitializer(userValueType()),
		constant.NewBitCast(writerVtable, irtypes.I8Ptr), 0)
	writerVal = c.block.NewInsertValue(writerVal, rawPtr, 1)

	// 3. Get format method receiver from the user type value
	var receiver value.Value
	if named.IsValueType() {
		receiver = c.valueTypeReceiverPtr(val, typ)
	} else if _, ok := val.Type().(*irtypes.StructType); ok {
		// Value struct {vtable, instance} — extract instance ptr
		receiver = c.extractInstancePtr(val)
	} else {
		// Already i8* (this reference in a method body)
		receiver = val
	}

	// 4. Call TypeName.format(receiver, writer) — failable void returns {i1, i8*}
	formatResult := c.callFormatMethod(receiver, writerVal, val, named, typ)

	// 5. Handle failable result: panic on error
	tag := c.block.NewExtractValue(formatResult, 0)
	okBlock := c.newBlock("interp.format.ok")
	errBlock := c.newBlock("interp.format.err")
	c.block.NewCondBr(tag, errBlock, okBlock)

	c.block = errBlock
	errPtr := c.block.NewExtractValue(formatResult, 1)
	c.block.NewCall(c.funcs["promise_panic"], errPtr)
	c.block.NewUnreachable()

	c.block = okBlock

	// 6. Call Builder.to_string(builder_ptr) → string (i8*)
	toStringFn := c.funcs["Builder.to_string"]
	return c.block.NewCall(toStringFn, rawPtr)
}

// callFormatMethod dispatches the format(Writer ~w)! call on the user type,
// using virtual dispatch when the type has children, direct dispatch otherwise.
func (c *Compiler) callFormatMethod(receiver, writerVal, originalVal value.Value,
	named *types.Named, typ types.Type) value.Value {

	// Failable void result type: {i1, i8*}
	resultType := irtypes.NewStruct(irtypes.I1, irtypes.I8Ptr)

	if c.needsVtable(named) {
		// Virtual dispatch through vtable
		slotIndex := named.VirtualMethodIndex("format", false)
		if slotIndex < 0 {
			panic(fmt.Sprintf("codegen: format method not in vtable for %s", named))
		}

		// Get vtable pointer from the original value
		var vtableRaw value.Value
		if _, ok := originalVal.Type().(*irtypes.StructType); ok {
			vtableRaw = c.extractVtablePtr(originalVal)
		} else {
			// this reference (i8*) — load vtable from variant→typeinfo chain
			variantPtr := c.loadVariantPtr(originalVal)
			typeinfoStruct := irtypes.NewStruct(irtypes.I8Ptr)
			typeinfoPtr := c.block.NewBitCast(variantPtr, irtypes.NewPointer(typeinfoStruct))
			vtableFieldPtr := c.block.NewGetElementPtr(typeinfoStruct, typeinfoPtr,
				constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
			vtableRaw = c.block.NewLoad(irtypes.I8Ptr, vtableFieldPtr)
		}

		vtablePtr := c.block.NewBitCast(vtableRaw, irtypes.NewPointer(irtypes.I8Ptr))
		fnSlotPtr := c.block.NewGetElementPtr(irtypes.I8Ptr, vtablePtr,
			constant.NewInt(irtypes.I32, int64(slotIndex)))
		fnRaw := c.block.NewLoad(irtypes.I8Ptr, fnSlotPtr)
		fnType := irtypes.NewFunc(resultType, irtypes.I8Ptr, userValueType())
		fnTyped := c.block.NewBitCast(fnRaw, irtypes.NewPointer(fnType))
		return c.block.NewCall(fnTyped, receiver, writerVal)
	}

	// Direct dispatch
	mangledName := mangleMethodName(c.resolveTypeName(typ), "format", false)
	fn, ok := c.funcs[mangledName]
	if !ok {
		panic(fmt.Sprintf("codegen: undeclared method %s for interpolation", mangledName))
	}
	return c.block.NewCall(fn, receiver, writerVal)
}

// getInterpBuilderWriterVtable returns the Writer vtable global for Builder,
// creating it lazily on first use. The vtable maps Writer's virtual methods
// (write, write_string) to Builder's implementations.
func (c *Compiler) getInterpBuilderWriterVtable() *ir.Global {
	if c.interpBuilderWriterVtable != nil {
		return c.interpBuilderWriterVtable
	}

	// Look up Writer to determine vtable slot ordering
	writerNamed := c.lookupNamedType("Writer")
	if writerNamed == nil {
		panic("codegen: Writer type not found for interpolation")
	}

	// Ensure default methods from Writer are synthesized for Builder
	builderNamed := c.lookupNamedType("Builder")
	if builderNamed != nil {
		c.ensureDefaultMethodsSynthesized(builderNamed, writerNamed)
	}

	methods := writerNamed.AllVirtualMethods()

	// Build vtable entries mapping Writer methods → Builder implementations
	var entries []constant.Constant
	for _, m := range methods {
		mangledName := mangleMethodName("Builder", m.Name(), m.IsSetter())
		fn, ok := c.funcs[mangledName]
		if !ok {
			panic(fmt.Sprintf("codegen: Builder.%s not found for Writer vtable", m.Name()))
		}
		entries = append(entries, constant.NewBitCast(fn, irtypes.I8Ptr))
	}

	arrayType := irtypes.NewArray(uint64(len(entries)), irtypes.I8Ptr)
	init := constant.NewArray(arrayType, entries...)
	global := c.module.NewGlobalDef("__interp_builder_writer_vtable", init)
	global.Immutable = true
	c.interpBuilderWriterVtable = global
	return global
}

// hasInterpolation checks if a string literal contains any interpolation parts.
func hasInterpolation(parts []ast.StringPart) bool {
	for _, part := range parts {
		if _, ok := part.(ast.StringInterp); ok {
			return true
		}
	}
	return false
}

// resolveEscape converts an escape sequence token to its string value.
// The seq parameter contains the full lexer token (e.g., `\n` for a newline escape).
func resolveEscape(seq string) string {
	// Strip leading backslash if present (lexer includes it in the token)
	if len(seq) > 1 && seq[0] == '\\' {
		seq = seq[1:]
	}
	switch seq {
	case "n":
		return "\n"
	case "t":
		return "\t"
	case "r":
		return "\r"
	case "b":
		return "\b"
	case "\\":
		return "\\"
	case "\"":
		return "\""
	case "0":
		return "\x00"
	case "{":
		return "{"
	default:
		return "\\" + seq
	}
}

// --- Identifiers ---

func (c *Compiler) genIdentExpr(e *ast.IdentExpr) value.Value {
	// Local variable: load from alloca (checked first to shadow module-level names)
	if alloca, ok := c.locals[e.Name]; ok {
		return c.block.NewLoad(alloca.ElemType, alloca)
	}
	// Module-level getter accessed without prefix (same file or glob import):
	// call the function with no args.
	if fn, ok := c.funcs[e.Name]; ok {
		if obj := c.lookupFunc(e.Name); obj != nil && obj.IsGetter() {
			return c.block.NewCall(fn)
		}
		if _, isSig := c.info.Types[e].(*types.Signature); isSig {
			// Named function used as first-class value: generate a thunk with
			// the env-first ABI so it can be called through genIndirectCall.
			thunk := c.getOrCreateThunk(fn, e.Name)
			fnPtr := c.block.NewBitCast(thunk, irtypes.I8Ptr)
			var closure value.Value = constant.NewUndef(closureType())
			closure = c.block.NewInsertValue(closure, fnPtr, 0)
			closure = c.block.NewInsertValue(closure, constant.NewNull(irtypes.I8Ptr), 1)
			return closure
		}
		return fn
	}
	panic(fmt.Sprintf("codegen: undefined variable %q", e.Name))
}

// --- Binary expressions ---

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
	left := c.genExpr(e.Left)
	right := c.genExpr(e.Right)

	leftType := c.info.Types[e.Left]
	if c.typeSubst != nil {
		leftType = types.Substitute(leftType, c.typeSubst)
	}
	if c.selfSubst != nil {
		leftType = types.SubstituteSelf(leftType, c.selfSubst.iface, c.selfSubst.concrete)
	}
	named := extractNamed(leftType)
	if named == nil {
		panic(fmt.Sprintf("codegen: cannot resolve Named type from %s for operator %s", leftType, e.Op))
	}

	op := e.Op.String()
	method := named.LookupMethod(op)
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
		return c.genVirtualBinaryOp(e, named, method, left, right)
	}

	// Direct dispatch: call the concrete type's operator method.
	ownerName := c.resolveMethodOwner(named, op)
	var mangledName string
	if ownerName != named.Obj().Name() {
		// Operator inherited from a parent. If the parent is structural, the method
		// was synthesized under the concrete type's name — use that, not the parent's.
		// (Mirrors the same logic in genMethodCall for structural inheritance.)
		if structParent := c.findStructuralOwner(named, op); structParent != nil {
			c.ensureDefaultMethodsSynthesized(named, structParent)
			mangledName = mangleMethodName(named.Obj().Name(), op, false)
		} else {
			mangledName = mangleMethodName(ownerName, op, false)
		}
	} else {
		mangledName = mangleMethodName(ownerName, op, false)
	}
	fn, ok := c.funcs[mangledName]
	if !ok {
		panic(fmt.Sprintf("codegen: undeclared operator method %s", mangledName))
	}

	var args []value.Value
	if method.Sig().Recv() != nil {
		if _, isThis := e.Left.(*ast.ThisExpr); isThis {
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
	if _, isThis := e.Right.(*ast.ThisExpr); isThis {
		var paramIdx int
		if method.Sig().Recv() != nil {
			paramIdx = 1
		}
		if paramIdx < len(fn.Params) {
			if st, ok := fn.Params[paramIdx].Typ.(*irtypes.StructType); ok {
				if _, rightIsPtr := right.Type().(*irtypes.PointerType); rightIsPtr {
					alloca := c.block.NewAlloca(st)
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
	args = append(args, right)
	return c.block.NewCall(fn, args...)
}

// genVirtualBinaryOp dispatches a non-native binary operator through the vtable.
// Used when the static type is abstract or has children requiring virtual dispatch.
// Mirrors genVirtualMethodCall but uses pre-evaluated left/right operands.
func (c *Compiler) genVirtualBinaryOp(e *ast.BinaryExpr, named *types.Named,
	method *types.Method, left, right value.Value) value.Value {

	op := e.Op.String()

	// Extract vtable and instance from left operand
	var vtableRaw, instance value.Value
	if _, isThis := e.Left.(*ast.ThisExpr); isThis {
		instance = left
		variantPtr := c.loadVariantPtr(left)
		typeinfoStruct := irtypes.NewStruct(irtypes.I8Ptr)
		typeinfoPtr := c.block.NewBitCast(variantPtr, irtypes.NewPointer(typeinfoStruct))
		vtableFieldPtr := c.block.NewGetElementPtr(typeinfoStruct, typeinfoPtr,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
		vtableRaw = c.block.NewLoad(irtypes.I8Ptr, vtableFieldPtr)
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

	operand := c.genExpr(e.Operand)
	operandType := c.info.Types[e.Operand]
	named := extractNamed(operandType)
	if named == nil {
		panic(fmt.Sprintf("codegen: cannot resolve Named type from %s for unary %s", operandType, e.Op))
	}

	op := e.Op.String()

	// For unary ops, look up the 0-param method variant
	method := c.lookupUnaryMethod(named, op)
	if method == nil {
		panic(fmt.Sprintf("codegen: no unary method %q on type %s", op, named))
	}

	if method.IsNative() {
		return c.emitNativeOp(named, op, operand, nil)
	}

	panic(fmt.Sprintf("codegen: non-native unary %s.%s not yet implemented", named, op))
}

// lookupUnaryMethod finds the 0-param variant of a method by name.
func (c *Compiler) lookupUnaryMethod(named *types.Named, op string) *types.Method {
	for _, m := range named.Methods() {
		if m.Name() == op && len(m.Sig().Params()) == 0 {
			return m
		}
	}
	return nil
}

// --- Short-circuit boolean operators ---

func (c *Compiler) genShortCircuitAnd(e *ast.BinaryExpr) value.Value {
	left := c.genExpr(e.Left)
	startBlock := c.block

	rightBlock := c.newBlock("and.rhs")
	mergeBlock := c.newBlock("and.merge")

	c.block.NewCondBr(left, rightBlock, mergeBlock)

	c.block = rightBlock
	right := c.genExpr(e.Right)
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
	left := c.genExpr(e.Left)
	startBlock := c.block

	rightBlock := c.newBlock("or.rhs")
	mergeBlock := c.newBlock("or.merge")

	c.block.NewCondBr(left, mergeBlock, rightBlock)

	c.block = rightBlock
	right := c.genExpr(e.Right)
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
// Layout: { i8* _vtable, T_i* _rtti, T start, T end, i1 inclusive }
func (c *Compiler) genRange(e *ast.BinaryExpr) value.Value {
	start := c.genExpr(e.Left)
	end := c.genExpr(e.Right)
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
	val = c.block.NewInsertValue(val, constant.NewNull(irtypes.I8Ptr), 0)                                          // vtable = null
	val = c.block.NewInsertValue(val, constant.NewNull(layout.Value.Fields[1].LLVMType.(*irtypes.PointerType)), 1) // rtti = null
	val = c.block.NewInsertValue(val, start, uint64(layout.ValueFieldIndex["start"]))                              // start
	val = c.block.NewInsertValue(val, end, uint64(layout.ValueFieldIndex["end"]))                                  // end
	val = c.block.NewInsertValue(val, inclusive, uint64(layout.ValueFieldIndex["inclusive"]))                      // inclusive
	return val
}

// --- Call expressions ---

func (c *Compiler) genCallExpr(e *ast.CallExpr) value.Value {
	// Handle super() calls in constructor bodies
	if ident, ok := e.Callee.(*ast.IdentExpr); ok && ident.Name == "super" {
		return c.genSuperCall(e)
	}

	// Method call or enum variant constructor: callee is MemberExpr
	if member, ok := e.Callee.(*ast.MemberExpr); ok {
		// Handle mod.func() / mod.Type() — qualified call to imported module
		if ident, ok := member.Target.(*ast.IdentExpr); ok {
			if modName := c.resolveModuleName(ident); modName != "" {
				calleeType := c.info.Types[e.Callee]
				switch calleeType.(type) {
				case *types.Named, *types.Instance:
					// Module-qualified constructor: mod.Type(args)
					return c.genConstructorCallMono(e, calleeType)
				case *types.Enum:
					// Module-qualified enum — fall through to enum dispatch below
				default:
					// Module-qualified function call: mod.func(args)
					return c.genModuleCall(e, modName, member.Field)
				}
			}
		}

		targetType := c.info.Types[member.Target]
		// Apply typeSubst for mono context
		if c.typeSubst != nil {
			targetType = types.Substitute(targetType, c.typeSubst)
		}
		if enumLayout := c.lookupEnumLayout(targetType); enumLayout != nil {
			if _, isVariant := enumLayout.VariantTag[member.Field]; isVariant {
				return c.genEnumVariantCallLayout(e, member, enumLayout)
			}
			// Not a variant — fall through to method dispatch
		}
		// Fallback for generic enum variant constructors in mono context:
		// target is bare *types.Enum; use the call result type (Instance after subst).
		if _, ok := targetType.(*types.Enum); ok {
			resultType := c.info.Types[e]
			if c.typeSubst != nil {
				resultType = types.Substitute(resultType, c.typeSubst)
			}
			if enumLayout := c.lookupEnumLayout(resultType); enumLayout != nil {
				if _, isVariant := enumLayout.VariantTag[member.Field]; isVariant {
					return c.genEnumVariantCallLayout(e, member, enumLayout)
				}
			}
		}
		// Function-typed field call: this._next() where _next is a () -> T? field.
		// Check if the member name is a field (not a method) on the target type,
		// and the field type is a Signature — treat as indirect call through the field.
		if sig, ok := c.info.Types[e.Callee].(*types.Signature); ok {
			memberTargetType := c.info.Types[member.Target]
			if c.typeSubst != nil {
				memberTargetType = types.Substitute(memberTargetType, c.typeSubst)
			}
			if c.selfSubst != nil {
				memberTargetType = types.SubstituteSelf(memberTargetType, c.selfSubst.iface, c.selfSubst.concrete)
			}
			if named := extractNamed(memberTargetType); named != nil {
				if named.LookupField(member.Field) != nil {
					closure := c.genExpr(e.Callee) // genMemberExpr loads the field
					var argVals []value.Value
					for _, arg := range e.Args {
						argVals = append(argVals, c.genCallArgExpr(arg.Value))
						if ident, ok := arg.Value.(*ast.IdentExpr); ok {
							c.clearDropFlag(ident.Name)
						}
					}
					return c.genIndirectCall(closure, sig, argVals)
				}
			}
		}
		return c.genMethodCall(e, member)
	}

	// Constructor call: callee resolves to a Named type or Instance
	calleeType := c.info.Types[e.Callee]
	if c.typeSubst != nil {
		calleeType = types.Substitute(calleeType, c.typeSubst)
	}
	if inst, ok := calleeType.(*types.Instance); ok {
		if origin, ok := inst.Origin().(*types.Named); ok {
			// Vector capacity constructor: T[](capacity: n)
			if origin == types.TypVector {
				return c.genVectorCapacityConstructor(e, inst)
			}
			// Channel constructor: channel[T](capacity: n) or channel[T]()
			if origin == types.TypChannel {
				return c.genChannelConstructor(e, inst)
			}
			return c.genConstructorCallMono(e, calleeType)
		}
	}
	if named, ok := calleeType.(*types.Named); ok {
		if _, isIdent := e.Callee.(*ast.IdentExpr); isIdent {
			return c.genConstructorCallMono(e, named)
		}
	}

	// Generic function/method call: callee is IndexExpr (identity[int](42) or obj.method[int](42))
	if idx, ok := e.Callee.(*ast.IndexExpr); ok {
		if member, ok := idx.Target.(*ast.MemberExpr); ok {
			// Check if this is a module-qualified generic function call (json.encode_string[Config](...))
			// vs. an instance generic method call (box.transform[string](...))
			if ident, ok := member.Target.(*ast.IdentExpr); ok {
				if c.resolveModuleName(ident) != "" {
					return c.genModuleGenericFuncCall(e, idx, member.Field)
				}
			}
			return c.genGenericMethodCall(e, idx, member)
		}
		return c.genGenericFuncCall(e, idx)
	}

	// Evaluate arguments
	var argVals []value.Value
	var argTypes []types.Type
	for _, arg := range e.Args {
		argVals = append(argVals, c.genCallArgExpr(arg.Value))
		argTypes = append(argTypes, c.info.Types[arg.Value])
		// Clear drop flag: argument is moved into the callee
		if ident, ok := arg.Value.(*ast.IdentExpr); ok {
			c.clearDropFlag(ident.Name)
		}
	}

	// Resolve callee
	ident, ok := e.Callee.(*ast.IdentExpr)
	if !ok {
		panic(fmt.Sprintf("codegen: unsupported callee type %T", e.Callee))
	}

	// Lambda call: callee is a local variable holding a fat pointer {i8*, i8*}
	if alloca, ok := c.locals[ident.Name]; ok {
		calleeType := c.info.Types[e.Callee]
		if sig, ok := calleeType.(*types.Signature); ok {
			closure := c.block.NewLoad(alloca.ElemType, alloca)
			return c.genIndirectCall(closure, sig, argVals)
		}
	}

	// Extern function — pack args into value structs, call, unpack return
	if ext, ok := c.externs[ident.Name]; ok {
		return c.genExternCall(ext, argVals, argTypes)
	}

	// Regular function call
	fn, ok := c.funcs[ident.Name]
	if !ok {
		panic(fmt.Sprintf("codegen: undefined function %q", ident.Name))
	}

	// Coerce arguments when crossing type boundaries
	if callee := c.lookupFunc(ident.Name); callee != nil {
		if sig, ok := callee.Type().(*types.Signature); ok {
			argVals = c.coerceCallArgs(argVals, argTypes, sig.Params())
		}
	}

	return c.block.NewCall(fn, argVals...)
}

// resolveModuleName checks if an IdentExpr refers to a module and returns
// the module's IR prefix (derived from GlobalIdentity) for IR symbol lookup.
// Returns "" if the ident is not a module.
func (c *Compiler) resolveModuleName(ident *ast.IdentExpr) string {
	if obj, ok := c.info.Objects[ident]; ok {
		if mod, ok := obj.(*types.Module); ok {
			// Map the module's path to its IR prefix for stable IR identity
			if prefix, ok := c.moduleCanonical[mod.Path()]; ok {
				return prefix
			}
			// Catalog modules have empty Path(); use catalog name as IR prefix.
			// Catalog names are simple identifiers that pass through SanitizeIRPrefix
			// unchanged, so catalogName == IRPrefix. This handles aliased imports
			// like `use json as j;` where mod.Name() = "j" but IR prefix = "json".
			if catName := mod.CatalogName(); catName != "" {
				return catName
			}
			return mod.Name()
		}
	}
	return ""
}

// genModuleCall handles mod.func() calls — resolves func in the module's IR functions.
func (c *Compiler) genModuleCall(e *ast.CallExpr, moduleName, funcName string) value.Value {
	var argVals []value.Value
	var argTypes []types.Type
	for _, arg := range e.Args {
		argVals = append(argVals, c.genCallArgExpr(arg.Value))
		argTypes = append(argTypes, c.info.Types[arg.Value])
		if ident, ok := arg.Value.(*ast.IdentExpr); ok {
			c.clearDropFlag(ident.Name)
		}
	}

	// Try module extern first
	key := moduleName + "." + funcName
	if ext, ok := c.moduleExterns[key]; ok {
		return c.genExternCall(ext, argVals, argTypes)
	}

	// Try module function
	fn, ok := c.moduleFuncs[key]
	if !ok {
		panic(fmt.Sprintf("codegen: undefined module function %s.%s", moduleName, funcName))
	}

	// Coerce arguments using the callee's signature from sema
	if sig, ok := c.info.Types[e.Callee].(*types.Signature); ok {
		argVals = c.coerceCallArgs(argVals, argTypes, sig.Params())
	}

	return c.block.NewCall(fn, argVals...)
}

// genModuleGetterCall handles mod.property access — calls the getter function with no args.
func (c *Compiler) genModuleGetterCall(e *ast.MemberExpr, moduleName, propName string) value.Value {
	key := moduleName + "." + propName
	fn, ok := c.moduleFuncs[key]
	if !ok {
		panic(fmt.Sprintf("codegen: undefined module getter %s.%s", moduleName, propName))
	}
	return c.block.NewCall(fn)
}

// genGenericFuncCall generates a call to a monomorphic generic function instance.
func (c *Compiler) genGenericFuncCall(e *ast.CallExpr, idx *ast.IndexExpr) value.Value {
	// Resolve all type arguments to build the mangled name
	ident, ok := idx.Target.(*ast.IdentExpr)
	if !ok {
		panic(fmt.Sprintf("codegen: generic function target is not IdentExpr: %T", idx.Target))
	}

	allTypeArgExprs := append([]ast.Expr{idx.Index}, idx.ExtraIndices...)
	mangledName := ident.Name + "["
	for i, argExpr := range allTypeArgExprs {
		typeArgType := c.info.Types[argExpr]
		if c.typeSubst != nil && typeArgType != nil {
			typeArgType = types.Substitute(typeArgType, c.typeSubst)
		}
		if i > 0 {
			mangledName += ", "
		}
		mangledName += typeArgStr(typeArgType)
	}
	mangledName += "]"

	fn, ok := c.funcs[mangledName]
	if !ok {
		panic(fmt.Sprintf("codegen: undefined monomorphic function %q", mangledName))
	}

	var argVals []value.Value
	var argTypes []types.Type
	for _, arg := range e.Args {
		argVals = append(argVals, c.genCallArgExpr(arg.Value))
		argTypes = append(argTypes, c.info.Types[arg.Value])
		// Clear drop flag: argument is moved into the callee
		if ident, ok := arg.Value.(*ast.IdentExpr); ok {
			c.clearDropFlag(ident.Name)
		}
	}

	// Coerce arguments when crossing type boundaries
	if callee := c.lookupFunc(ident.Name); callee != nil {
		if sig, ok := callee.Type().(*types.Signature); ok {
			argVals = c.coerceCallArgs(argVals, argTypes, sig.Params())
		}
	}

	return c.block.NewCall(fn, argVals...)
}

// genModuleGenericFuncCall generates a call to a monomorphized generic function
// that is qualified by a module name. Example: json.encode_string[Config](value)
// The mono function is stored in c.funcs as "encode_string[Config]" (no module prefix).
func (c *Compiler) genModuleGenericFuncCall(e *ast.CallExpr, idx *ast.IndexExpr, funcName string) value.Value {
	// Build mangled name: funcName[typeArg1, typeArg2, ...]
	allTypeArgExprs := append([]ast.Expr{idx.Index}, idx.ExtraIndices...)
	mangledName := funcName + "["
	for i, argExpr := range allTypeArgExprs {
		typeArgType := c.info.Types[argExpr]
		if c.typeSubst != nil && typeArgType != nil {
			typeArgType = types.Substitute(typeArgType, c.typeSubst)
		}
		if i > 0 {
			mangledName += ", "
		}
		mangledName += typeArgStr(typeArgType)
	}
	mangledName += "]"

	fn, ok := c.funcs[mangledName]
	if !ok {
		panic(fmt.Sprintf("codegen: undefined monomorphic module function %q", mangledName))
	}

	var argVals []value.Value
	var argTypes []types.Type
	for _, arg := range e.Args {
		argVals = append(argVals, c.genCallArgExpr(arg.Value))
		argTypes = append(argTypes, c.info.Types[arg.Value])
		if ident, ok := arg.Value.(*ast.IdentExpr); ok {
			c.clearDropFlag(ident.Name)
		}
	}

	// Coerce arguments when crossing type boundaries
	if callee := c.lookupFunc(funcName); callee != nil {
		if sig, ok := callee.Type().(*types.Signature); ok {
			argVals = c.coerceCallArgs(argVals, argTypes, sig.Params())
		}
	}

	return c.block.NewCall(fn, argVals...)
}

// genGenericMethodCall generates a call to a monomorphized generic method.
// Example: box.transform[string](fn) → "Box.transform[string]"(this, fn)
// Example: box.transform[string](fn) where box is Box[int] → "Box[int].transform[string]"(this, fn)
func (c *Compiler) genGenericMethodCall(e *ast.CallExpr, idx *ast.IndexExpr, member *ast.MemberExpr) value.Value {
	targetType := c.info.Types[member.Target]
	if c.typeSubst != nil {
		targetType = types.Substitute(targetType, c.typeSubst)
	}

	named := extractNamed(targetType)
	if named == nil {
		panic(fmt.Sprintf("codegen: cannot resolve type for generic method call on %T", targetType))
	}

	method := named.LookupMethod(member.Field)
	if method == nil {
		panic(fmt.Sprintf("codegen: no method %s on type %s", member.Field, named))
	}

	// Build mono method name: DefiningType.method[typearg1, typearg2]
	// Use the method's defining type (which may be a parent), not the target type.
	defOwnerName := c.resolveMethodOwner(named, member.Field)
	if defOwnerName != named.Obj().Name() {
		// Inherited — resolve mono parent name if the parent is generic
		defOwnerName = c.resolveMonoParentName(named, targetType, defOwnerName)
	} else {
		defOwnerName = c.resolveTypeName(targetType)
	}
	allTypeArgExprs := append([]ast.Expr{idx.Index}, idx.ExtraIndices...)
	mangledName := mangleMethodName(defOwnerName, member.Field, false) + "["
	for i, argExpr := range allTypeArgExprs {
		typeArgType := c.info.Types[argExpr]
		if c.typeSubst != nil && typeArgType != nil {
			typeArgType = types.Substitute(typeArgType, c.typeSubst)
		}
		if i > 0 {
			mangledName += ", "
		}
		mangledName += typeArgStr(typeArgType)
	}
	mangledName += "]"

	fn, ok := c.funcs[mangledName]
	if !ok {
		panic(fmt.Sprintf("codegen: undefined monomorphic method %q", mangledName))
	}

	// Generate receiver
	var args []value.Value
	if method.Sig().Recv() != nil {
		target := c.genExpr(member.Target)
		if _, isThis := member.Target.(*ast.ThisExpr); isThis {
			args = append(args, target)
		} else if isContainerType(targetType) {
			args = append(args, target)
		} else if isPrimitiveScalar(named) {
			args = append(args, target)
		} else if named.IsValueType() {
			args = append(args, c.valueTypeReceiverPtr(target, targetType))
		} else {
			args = append(args, c.extractInstancePtr(target))
		}
	}

	// Generate arguments
	var argVals []value.Value
	var argTypes []types.Type
	for _, arg := range e.Args {
		argVals = append(argVals, c.genCallArgExpr(arg.Value))
		argTypes = append(argTypes, c.info.Types[arg.Value])
		if ident, ok := arg.Value.(*ast.IdentExpr); ok {
			c.clearDropFlag(ident.Name)
		}
	}
	argVals = c.coerceCallArgs(argVals, argTypes, method.Sig().Params())
	args = append(args, argVals...)

	return c.block.NewCall(fn, args...)
}

// --- super() calls ---

// genSuperCall generates a super() call inside a new() constructor body.
// Calls the parent's new() (if parent has one) or sets parent fields directly.
func (c *Compiler) genSuperCall(e *ast.CallExpr) value.Value {
	named := c.currentNamed
	if named == nil || len(named.Parents()) == 0 {
		return nil // sema already validated
	}
	parent := named.Parents()[0].Named

	// Load the this pointer
	thisAlloca := c.locals["this"]
	thisPtr := c.block.NewLoad(irtypes.I8Ptr, thisAlloca)

	if parent.HasNew() {
		// Parent has explicit new() — call ParentType.new(this, args...)
		parentName := parent.Obj().Name()
		mangledName := mangleMethodName(parentName, "new", false)
		fn, ok := c.funcs[mangledName]
		if !ok {
			panic(fmt.Sprintf("codegen: undeclared parent constructor %s", mangledName))
		}

		var argVals []value.Value
		var argTypes []types.Type
		for _, arg := range e.Args {
			argVals = append(argVals, c.genCallArgExpr(arg.Value))
			argTypes = append(argTypes, c.info.Types[arg.Value])
			if ident, ok := arg.Value.(*ast.IdentExpr); ok {
				c.clearDropFlag(ident.Name)
			}
		}
		newMethod := parent.LookupMethod("new")
		if newMethod != nil {
			argVals = c.coerceCallArgs(argVals, argTypes, newMethod.Sig().Params())
		}
		args := append([]value.Value{thisPtr}, argVals...)
		result := c.block.NewCall(fn, args...)
		if newMethod != nil && newMethod.Sig().CanError() {
			tag := c.block.NewExtractValue(result, 0)
			errBlock := c.newBlock("super.err")
			okBlock := c.newBlock("super.ok")
			c.block.NewCondBr(tag, errBlock, okBlock)
			// Error path: propagate
			c.block = errBlock
			resultType := fn.Sig.RetType.(*irtypes.StructType)
			errVal := c.block.NewExtractValue(result, resultErrIdx(resultType))
			outerResultType := c.fn.Sig.RetType.(*irtypes.StructType)
			errResult := c.wrapError(errVal, outerResultType)
			c.block.NewRet(errResult)
			// Continue on ok path
			c.block = okBlock
		}
		return nil
	}

	// Parent has implicit constructor — set parent fields directly on `this`
	// Use the child's own layout since parent fields are part of the child's instance struct
	childLayout := c.lookupTypeLayout(named)
	if childLayout == nil {
		return nil
	}
	instanceStructType := childLayout.Instance.LLVMType
	instancePtrType := childLayout.InstancePtrType

	// Build map of provided field values
	provided := make(map[string]value.Value)
	for _, arg := range e.Args {
		if arg.Name != "" {
			provided[arg.Name] = c.genCallArgExpr(arg.Value)
			if ident, ok := arg.Value.(*ast.IdentExpr); ok {
				c.clearDropFlag(ident.Name)
			}
		}
	}

	// Set each parent field on the instance
	instancePtr := c.block.NewBitCast(thisPtr, instancePtrType)
	allFields := parent.AllFields()
	for _, f := range allFields {
		val, ok := provided[f.Name()]
		if !ok {
			// Use default if available, else zero
			if defExpr, hasDef := c.info.FieldDefaults[f]; hasDef {
				val = c.genExpr(defExpr)
			} else {
				val = c.zeroValue(c.resolveType(f.Type()))
			}
		}
		fieldIdx := childLayout.InstanceFieldIndex[f.Name()]
		fieldPtr := c.block.NewGetElementPtr(instanceStructType, instancePtr,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(fieldIdx)))
		c.block.NewStore(val, fieldPtr)
	}
	return nil
}

// --- Constructor calls ---

// genConstructorCallMono generates a heap-allocated instance of a user type.
// Handles both regular Named types and generic Instance types via lookupTypeLayout.
func (c *Compiler) genConstructorCallMono(e *ast.CallExpr, typ types.Type) value.Value {
	named := extractNamed(typ)
	layout := c.lookupTypeLayout(typ)
	if layout == nil {
		panic(fmt.Sprintf("codegen: no layout for type %s", typ))
	}

	// Value types: no heap allocation, build value struct with insertvalue chain
	if layout.IsValueType {
		return c.genValueTypeConstructor(e, named, layout, typ)
	}

	instanceStructType := layout.Instance.LLVMType
	instancePtrType := layout.InstancePtrType

	// Compute size via GEP-from-null trick
	nullPtr := constant.NewNull(instancePtrType)
	sizePtr := c.block.NewGetElementPtr(instanceStructType, nullPtr,
		constant.NewInt(irtypes.I32, 1))
	sizeRaw := c.block.NewPtrToInt(sizePtr, c.ptrIntType())
	var size value.Value = sizeRaw
	if c.isWasm {
		size = c.block.NewZExt(sizeRaw, irtypes.I64)
	}

	// Allocate
	rawPtr := c.block.NewCall(c.palAlloc, size)
	typedPtr := c.block.NewBitCast(rawPtr, instancePtrType)

	// Store type info pointer in _variant slot (field 0) for RTTI
	variantFieldPtr := c.block.NewGetElementPtr(instanceStructType, typedPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
	variantPtrType := layout.Instance.Fields[0].LLVMType.(*irtypes.PointerType)
	if tiGlobal := c.lookupTypeInfoGlobal(typ); tiGlobal != nil {
		tiPtr := c.block.NewBitCast(tiGlobal, variantPtrType)
		c.block.NewStore(tiPtr, variantFieldPtr)
	} else {
		c.block.NewStore(constant.NewNull(variantPtrType), variantFieldPtr)
	}

	// If the type has an explicit new() constructor, call it instead of field matching
	if named != nil && named.HasNew() {
		// Zero-init all fields first
		for _, f := range named.AllFields() {
			fieldIdx := layout.InstanceFieldIndex[f.Name()]
			fieldPtr := c.block.NewGetElementPtr(instanceStructType, typedPtr,
				constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(fieldIdx)))
			c.block.NewStore(c.zeroValue(layout.Instance.Fields[fieldIdx].LLVMType), fieldPtr)
		}

		// Call new() with instance ptr as receiver + user args
		mangledName := mangleMethodName(c.resolveTypeName(typ), "new", false)
		fn, ok := c.funcs[mangledName]
		if !ok {
			panic(fmt.Sprintf("codegen: undeclared new() for type %s (mangled: %s)", typ, mangledName))
		}
		var argVals []value.Value
		var argTypes []types.Type
		for _, arg := range e.Args {
			argVals = append(argVals, c.genCallArgExpr(arg.Value))
			argTypes = append(argTypes, c.info.Types[arg.Value])
			if ident, ok := arg.Value.(*ast.IdentExpr); ok {
				c.clearDropFlag(ident.Name)
			}
		}
		newMethod := named.LookupMethod("new")
		if newMethod != nil {
			argVals = c.coerceCallArgs(argVals, argTypes, newMethod.Sig().Params())
		}
		args := append([]value.Value{typedPtr}, argVals...)
		newResult := c.block.NewCall(fn, args...)

		// If failable new, check error and wrap result
		if newMethod == nil {
			newMethod = named.LookupMethod("new")
		}
		if newMethod != nil && newMethod.Sig().CanError() {
			// new() returned { i1, i8* } — check tag
			newResultType := newResult.Type().(*irtypes.StructType)
			tag := c.block.NewExtractValue(newResult, 0)

			errBlock := c.newBlock("new.err")
			okBlock := c.newBlock("new.ok")
			mergeBlock := c.newBlock("new.merge")
			c.block.NewCondBr(tag, errBlock, okBlock)

			// Error path: propagate error wrapped in constructor result type
			constructorResultType := computeResultType(userValueType())
			c.block = errBlock
			errVal := c.block.NewExtractValue(newResult, resultErrIdx(newResultType))
			errResult := c.wrapError(errVal, constructorResultType)
			c.block.NewBr(mergeBlock)

			// Ok path: build value struct and wrap
			c.block = okBlock
			var vtablePtr2 value.Value
			if vtGlobal := c.lookupVtableGlobal(typ); vtGlobal != nil {
				vtablePtr2 = constant.NewBitCast(vtGlobal, irtypes.I8Ptr)
			} else {
				vtablePtr2 = constant.NewNull(irtypes.I8Ptr)
			}
			var valStruct value.Value = constant.NewUndef(userValueType())
			valStruct = c.block.NewInsertValue(valStruct, vtablePtr2, 0)
			valStruct = c.block.NewInsertValue(valStruct, rawPtr, 1)
			okResult := c.wrapOk(valStruct, constructorResultType)
			c.block.NewBr(mergeBlock)

			// Merge: phi between error and ok results
			c.block = mergeBlock
			phi := c.block.NewPhi(ir.NewIncoming(errResult, errBlock), ir.NewIncoming(okResult, okBlock))
			return phi
		}
	} else {
		// Implicit constructor: match arguments to field names.
		// Build field-type lookup for optional wrapping.
		fieldTypeMap := make(map[string]types.Type)
		for _, f := range named.AllFields() {
			ft := f.Type()
			if c.typeSubst != nil {
				ft = types.Substitute(ft, c.typeSubst)
			}
			fieldTypeMap[f.Name()] = ft
		}

		// maybeWrapOptional wraps val in an optional struct when the field type
		// is T? but the expression produces a non-optional, non-none value.
		maybeWrapOptional := func(val value.Value, expr ast.Expr, fieldName string, fieldIdx int) value.Value {
			if _, isOpt := fieldTypeMap[fieldName].(*types.Optional); !isOpt {
				return val
			}
			exprType := c.info.Types[expr]
			if c.typeSubst != nil {
				exprType = types.Substitute(exprType, c.typeSubst)
			}
			if exprType == types.TypNone {
				return val
			}
			if _, exprOpt := exprType.(*types.Optional); exprOpt {
				return val
			}
			return c.wrapOptional(val, layout.Instance.Fields[fieldIdx].LLVMType.(*irtypes.StructType))
		}

		provided := make(map[string]bool)
		for _, arg := range e.Args {
			if arg.Name == "" {
				panic(fmt.Sprintf("codegen: positional constructor args not supported for %s", typ))
			}
			provided[arg.Name] = true
			fieldIdx, ok := layout.InstanceFieldIndex[arg.Name]
			if !ok {
				panic(fmt.Sprintf("codegen: unknown field %s on type %s", arg.Name, typ))
			}
			val := c.genCallArgExpr(arg.Value)
			val = maybeWrapOptional(val, arg.Value, arg.Name, fieldIdx)
			fieldPtr := c.block.NewGetElementPtr(instanceStructType, typedPtr,
				constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(fieldIdx)))
			c.block.NewStore(val, fieldPtr)
			// Clear drop flag: field value is moved into the constructor
			if ident, ok := arg.Value.(*ast.IdentExpr); ok {
				c.clearDropFlag(ident.Name)
			}
		}

		// Initialize omitted fields: evaluate default expression if present, otherwise zero-init.
		for _, f := range named.AllFields() {
			if provided[f.Name()] {
				continue
			}
			fieldIdx := layout.InstanceFieldIndex[f.Name()]
			fieldPtr := c.block.NewGetElementPtr(instanceStructType, typedPtr,
				constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(fieldIdx)))
			if defExpr, ok := c.info.FieldDefaults[f]; ok {
				val := c.genExpr(defExpr)
				val = maybeWrapOptional(val, defExpr, f.Name(), fieldIdx)
				c.block.NewStore(val, fieldPtr)
			} else {
				c.block.NewStore(c.zeroValue(layout.Instance.Fields[fieldIdx].LLVMType), fieldPtr)
			}
		}
	}

	// Build value struct: { vtable_ptr, instance_ptr }
	var vtablePtr value.Value
	if vtGlobal := c.lookupVtableGlobal(typ); vtGlobal != nil {
		vtablePtr = constant.NewBitCast(vtGlobal, irtypes.I8Ptr)
	} else {
		vtablePtr = constant.NewNull(irtypes.I8Ptr)
	}
	var valStruct value.Value = constant.NewUndef(userValueType())
	valStruct = c.block.NewInsertValue(valStruct, vtablePtr, 0)
	valStruct = c.block.NewInsertValue(valStruct, rawPtr, 1)
	return valStruct
}

// genValueTypeConstructor builds a value type by insertvalue chain — no heap allocation.
// Value struct layout: { i8* _vtable, T_i* _rtti, field1, field2, ... }
func (c *Compiler) genValueTypeConstructor(e *ast.CallExpr, named *types.Named, layout *TypeDeclLayout, typ types.Type) value.Value {
	valueStructType := layout.Value.LLVMType

	// Start with undef
	var val value.Value = constant.NewUndef(valueStructType)

	// Field 0: vtable pointer
	if vtGlobal := c.lookupVtableGlobal(typ); vtGlobal != nil {
		val = c.block.NewInsertValue(val, constant.NewBitCast(vtGlobal, irtypes.I8Ptr), 0)
	} else {
		val = c.block.NewInsertValue(val, constant.NewNull(irtypes.I8Ptr), 0)
	}

	// Field 1: RTTI pointer (global instance singleton)
	if rttiGlobal := c.lookupValueTypeRTTI(typ); rttiGlobal != nil {
		rttiPtr := c.block.NewBitCast(rttiGlobal, layout.Value.Fields[1].LLVMType)
		val = c.block.NewInsertValue(val, rttiPtr, 1)
	} else {
		val = c.block.NewInsertValue(val, constant.NewNull(layout.Value.Fields[1].LLVMType.(*irtypes.PointerType)), 1)
	}

	// If the type has an explicit new() constructor, alloca + store + call new() + load
	if named != nil && named.HasNew() {
		alloca := c.block.NewAlloca(valueStructType)
		c.block.NewStore(val, alloca)

		// Zero-init all user fields
		for _, f := range named.AllFields() {
			fieldIdx := layout.ValueFieldIndex[f.Name()]
			fieldPtr := c.block.NewGetElementPtr(valueStructType, alloca,
				constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(fieldIdx)))
			c.block.NewStore(c.zeroValue(layout.Value.Fields[fieldIdx].LLVMType), fieldPtr)
		}

		// Call new() with pointer to value struct as receiver
		mangledName := mangleMethodName(c.resolveTypeName(typ), "new", false)
		fn, ok := c.funcs[mangledName]
		if !ok {
			panic(fmt.Sprintf("codegen: undeclared new() for value type %s (mangled: %s)", typ, mangledName))
		}
		var argVals []value.Value
		var argTypes []types.Type
		for _, arg := range e.Args {
			argVals = append(argVals, c.genCallArgExpr(arg.Value))
			argTypes = append(argTypes, c.info.Types[arg.Value])
			if ident, ok := arg.Value.(*ast.IdentExpr); ok {
				c.clearDropFlag(ident.Name)
			}
		}
		newMethod := named.LookupMethod("new")
		if newMethod != nil {
			argVals = c.coerceCallArgs(argVals, argTypes, newMethod.Sig().Params())
		}
		thisPtr := c.block.NewBitCast(alloca, irtypes.I8Ptr)
		args := append([]value.Value{thisPtr}, argVals...)
		c.block.NewCall(fn, args...)
		return c.block.NewLoad(valueStructType, alloca)
	}

	// Implicit constructor: match arguments to field names
	fieldTypeMap := make(map[string]types.Type)
	for _, f := range named.AllFields() {
		ft := f.Type()
		if c.typeSubst != nil {
			ft = types.Substitute(ft, c.typeSubst)
		}
		fieldTypeMap[f.Name()] = ft
	}

	maybeWrapOptional := func(v value.Value, expr ast.Expr, fieldName string, fieldIdx int) value.Value {
		if _, isOpt := fieldTypeMap[fieldName].(*types.Optional); !isOpt {
			return v
		}
		exprType := c.info.Types[expr]
		if c.typeSubst != nil {
			exprType = types.Substitute(exprType, c.typeSubst)
		}
		if exprType == types.TypNone {
			return v
		}
		if _, exprOpt := exprType.(*types.Optional); exprOpt {
			return v
		}
		return c.wrapOptional(v, layout.Value.Fields[fieldIdx].LLVMType.(*irtypes.StructType))
	}

	provided := make(map[string]bool)
	for _, arg := range e.Args {
		if arg.Name == "" {
			panic(fmt.Sprintf("codegen: positional constructor args not supported for %s", typ))
		}
		provided[arg.Name] = true
		fieldIdx, ok := layout.ValueFieldIndex[arg.Name]
		if !ok {
			panic(fmt.Sprintf("codegen: unknown field %s on type %s", arg.Name, typ))
		}
		fieldVal := c.genCallArgExpr(arg.Value)
		fieldVal = maybeWrapOptional(fieldVal, arg.Value, arg.Name, fieldIdx)
		val = c.block.NewInsertValue(val, fieldVal, uint64(fieldIdx))
		if ident, ok := arg.Value.(*ast.IdentExpr); ok {
			c.clearDropFlag(ident.Name)
		}
	}

	// Initialize omitted fields: evaluate default expression if present, otherwise zero-init
	for _, f := range named.AllFields() {
		if provided[f.Name()] {
			continue
		}
		fieldIdx := layout.ValueFieldIndex[f.Name()]
		if defExpr, ok := c.info.FieldDefaults[f]; ok {
			defVal := c.genExpr(defExpr)
			defVal = maybeWrapOptional(defVal, defExpr, f.Name(), fieldIdx)
			val = c.block.NewInsertValue(val, defVal, uint64(fieldIdx))
		} else {
			val = c.block.NewInsertValue(val, c.zeroValue(layout.Value.Fields[fieldIdx].LLVMType), uint64(fieldIdx))
		}
	}

	return val
}

// --- Member access ---

// genMemberExpr generates a field access on a user type instance or an enum variant value.
func (c *Compiler) genMemberExpr(e *ast.MemberExpr) value.Value {
	// Module-level getter: mod.property → call getter function with no args.
	// Guard: only intercept when sema resolved this as a getter (non-Signature type).
	// A Signature type means it's a function reference (e.g., auto f = mod.func),
	// which should NOT be called implicitly.
	if ident, ok := e.Target.(*ast.IdentExpr); ok {
		if modName := c.resolveModuleName(ident); modName != "" {
			if _, isSig := c.info.Types[e].(*types.Signature); !isSig {
				return c.genModuleGetterCall(e, modName, e.Field)
			}
		}
	}

	targetType := c.info.Types[e.Target]
	// Apply typeSubst for mono context
	if c.typeSubst != nil {
		targetType = types.Substitute(targetType, c.typeSubst)
	}
	if c.selfSubst != nil {
		targetType = types.SubstituteSelf(targetType, c.selfSubst.iface, c.selfSubst.concrete)
	}

	// Container .len property (string, vector, fixed array)
	// Check both Instance wrappers (user code: Vector[int]) and bare Named (method body: this is TypVector)
	if e.Field == "len" {
		if arr, ok := targetType.(*types.Array); ok {
			return constant.NewInt(irtypes.I64, arr.Size())
		}
		named := extractNamed(targetType)
		if named == types.TypString {
			return c.genStringLen(e)
		}
		if _, ok := types.AsVector(targetType); ok || named == types.TypVector {
			return c.genVectorLen(e)
		}
	}

	// Native hash getter for Hashable interface on primitive types
	if e.Field == "hash" {
		named := extractNamed(targetType)
		if named != nil {
			if v, ok := c.genNativeHashGetter(e, named); ok {
				return v
			}
		}
	}

	// Enum variant access: Color.Red or Option[int].None
	// Check variant first; if the field is not a variant, check for enum getters.
	if enumLayout := c.lookupEnumLayout(targetType); enumLayout != nil {
		if _, isVariant := enumLayout.VariantTag[e.Field]; isVariant {
			return c.genEnumVariantValueLayout(enumLayout, e.Field)
		}
		// Not a variant — check for enum getter
		if result, ok := c.genEnumGetterAccess(e, targetType, enumLayout); ok {
			return result
		}
	}

	// For generic enum variants (e.g. Slot.Empty inside a generic type body),
	// the target type is a bare *types.Enum but the result type is an Instance
	// after mono substitution. Use the result type to find the layout.
	if _, ok := targetType.(*types.Enum); ok {
		resultType := c.info.Types[e]
		if c.typeSubst != nil {
			resultType = types.Substitute(resultType, c.typeSubst)
		}
		if enumLayout := c.lookupEnumLayout(resultType); enumLayout != nil {
			if _, isVariant := enumLayout.VariantTag[e.Field]; isVariant {
				return c.genEnumVariantValueLayout(enumLayout, e.Field)
			}
			if result, ok := c.genEnumGetterAccess(e, targetType, enumLayout); ok {
				return result
			}
		}
	}

	named := extractNamed(targetType)
	if named == nil {
		panic(fmt.Sprintf("codegen: cannot resolve type for member access on %T", targetType))
	}

	field := named.LookupField(e.Field)
	if field != nil {
		return c.genFieldAccess(e, targetType, field)
	}

	// Getter property: emit a method call with no args beyond receiver
	if g := named.LookupGetter(e.Field); g != nil {
		return c.genGetterCall(e, targetType, named, g)
	}

	panic(fmt.Sprintf("codegen: member %s on type %s is not a field (method references not yet supported)", e.Field, named))
}

// genVectorCapacityConstructor generates a Vector with pre-allocated capacity: T[](capacity: n) or T[]().
func (c *Compiler) genVectorCapacityConstructor(e *ast.CallExpr, inst *types.Instance) value.Value {
	// capacity defaults to 16 when no argument provided
	var capacity value.Value
	if len(e.Args) > 0 {
		capacity = c.genCallArgExpr(e.Args[0].Value)
	} else {
		capacity = constant.NewInt(irtypes.I64, 16)
	}

	// Determine element size
	elemType := inst.TypeArgs()[0]
	elemLLVM := c.resolveType(elemType)
	elemSize := int64(c.typeSize(elemLLVM))

	return c.block.NewCall(c.funcs["promise_vector_with_capacity"],
		capacity,
		constant.NewInt(irtypes.I64, elemSize))
}

// genChannelConstructor generates code for channel[T](capacity: n) or channel[T]().
// Calls @promise_channel_new(capacity, elem_size) → i8*.
func (c *Compiler) genChannelConstructor(e *ast.CallExpr, inst *types.Instance) value.Value {
	elemType := inst.TypeArgs()[0]
	elemLLVM := c.resolveType(elemType)
	elemSize := int64(c.typeSize(elemLLVM))

	// capacity defaults to 0 (unbuffered) when no argument provided
	var capacity value.Value
	if len(e.Args) > 0 {
		capArg := c.genCallArgExpr(e.Args[0].Value)
		// Argument is int? — unwrap the optional to get the int value.
		// If it's a bare int literal, sema may pass it as int? via AssignableTo.
		argType := c.info.Types[e.Args[0].Value]
		if _, isOpt := argType.(*types.Optional); isOpt {
			// Extract value from { i1, i64 } optional — field 1
			capacity = c.block.NewExtractValue(capArg, 1)
		} else {
			capacity = capArg
		}
	} else {
		capacity = constant.NewInt(irtypes.I64, 0)
	}

	return c.block.NewCall(c.funcs["promise_channel_new"],
		capacity,
		constant.NewInt(irtypes.I64, elemSize))
}

// genChannelMethodCall dispatches native method calls on channel[T].
func (c *Compiler) genChannelMethodCall(e *ast.CallExpr, member *ast.MemberExpr, elemType types.Type, method string) value.Value {
	chRaw := c.genExpr(member.Target)
	chanType := channelStructType()
	chPtr := c.block.NewBitCast(chRaw, irtypes.NewPointer(chanType))
	elemLLVM := c.resolveType(elemType)
	elemSize := int64(c.typeSize(elemLLVM))

	switch method {
	case "send":
		return c.genChannelSend(e, chRaw, chPtr, chanType, elemLLVM, elemSize)
	case "close":
		return c.genChannelClose(chRaw, chPtr, chanType)
	default:
		panic(fmt.Sprintf("codegen: unknown channel method %q", method))
	}
}

// genChannelSend generates code for ch.send(value).
// lock → wait-if-full → memcpy to buffer → signal → rendezvous wait if unbuffered → unlock
func (c *Compiler) genChannelSend(e *ast.CallExpr, chRaw value.Value, chPtr value.Value, chanType *irtypes.StructType, elemLLVM irtypes.Type, elemSize int64) value.Value {
	// Load mutex
	mtxFieldPtr := c.block.NewGetElementPtr(chanType, chPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(chanFieldMutex)))
	mtx := c.block.NewLoad(irtypes.I8Ptr, mtxFieldPtr)

	// Load cond vars
	neFieldPtr := c.block.NewGetElementPtr(chanType, chPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(chanFieldNotEmpty)))
	notEmpty := c.block.NewLoad(irtypes.I8Ptr, neFieldPtr)

	nfFieldPtr := c.block.NewGetElementPtr(chanType, chPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(chanFieldNotFull)))
	notFull := c.block.NewLoad(irtypes.I8Ptr, nfFieldPtr)

	// Lock mutex
	c.block.NewCall(c.palMutexLock, mtx)

	// Check closed before sending — panic if channel is closed
	closedPtr := c.block.NewGetElementPtr(chanType, chPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(chanFieldClosed)))
	closedVal := c.block.NewLoad(irtypes.I8, closedPtr)
	isClosed := c.block.NewICmp(enum.IPredEQ, closedVal, constant.NewInt(irtypes.I8, 1))

	sendClosedPanicBlock := c.newBlock("send.closed.panic")
	sendOkBlock := c.newBlock("send.ok")
	c.block.NewCondBr(isClosed, sendClosedPanicBlock, sendOkBlock)

	c.block = sendClosedPanicBlock
	c.block.NewCall(c.palMutexUnlock, mtx)
	panicMsg := c.makeGlobalString("send on closed channel")
	c.block.NewCall(c.funcs["promise_panic"], panicMsg)
	c.block.NewUnreachable()

	c.block = sendOkBlock

	// Wait while full: while count == capacity
	waitFullBlock := c.newBlock("send.waitfull")
	waitFullClosedBlock := c.newBlock("send.waitfull.closed")
	writeBlock := c.newBlock("send.write")

	c.block.NewBr(waitFullBlock)

	// waitfull: check count == capacity
	c.block = waitFullBlock
	countPtr := c.block.NewGetElementPtr(chanType, chPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(chanFieldCount)))
	count := c.block.NewLoad(irtypes.I64, countPtr)
	capPtr := c.block.NewGetElementPtr(chanType, chPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(chanFieldCapacity)))
	cap_ := c.block.NewLoad(irtypes.I64, capPtr)
	isFull := c.block.NewICmp(enum.IPredEQ, count, cap_)

	waitFullBodyBlock := c.newBlock("send.waitfull.body")
	c.block.NewCondBr(isFull, waitFullBodyBlock, writeBlock)

	if c.inCoroutine {
		// Goroutine mode: park on send_waiters + coro.suspend
		c.block = waitFullBodyBlock
		currentG := c.block.NewLoad(irtypes.I8Ptr, c.currentGGlobal)
		sendHeadPtr := c.block.NewGetElementPtr(chanType, chPtr,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(chanFieldSendWaitersHead)))
		sendTailPtr := c.block.NewGetElementPtr(chanType, chPtr,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(chanFieldSendWaitersTail)))
		c.block.NewCall(c.funcs["promise_waiter_enqueue"], sendHeadPtr, sendTailPtr, currentG)
		// Store mutex in G.park_mutex — scheduler releases after coro.suspend completes
		gTySend := goroutineStructType()
		sendGPtr := c.block.NewBitCast(currentG, irtypes.NewPointer(gTySend))
		sendPmField := c.block.NewGetElementPtr(gTySend, sendGPtr,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(gFieldParkMutex)))
		c.block.NewStore(mtx, sendPmField)

		suspResult := c.block.NewCall(c.coroSuspend, constant.None, constant.False)
		resumeBlk := c.newBlock("send.wait.resume")
		c.block.NewSwitch(suspResult, c.coroSuspendBlk,
			ir.NewCase(constant.NewInt(irtypes.I8, 0), resumeBlk),
			ir.NewCase(constant.NewInt(irtypes.I8, 1), c.coroCleanupBlk))

		// On resume: re-lock and check closed, then retry
		c.block = resumeBlk
		c.block.NewCall(c.palMutexLock, mtx)
		closedAfterWait := c.block.NewLoad(irtypes.I8, closedPtr)
		isClosedAfterWait := c.block.NewICmp(enum.IPredEQ, closedAfterWait, constant.NewInt(irtypes.I8, 1))
		c.block.NewCondBr(isClosedAfterWait, waitFullClosedBlock, waitFullBlock)
	} else {
		// Thread-blocking mode: cond_wait, then re-check closed flag
		c.block = waitFullBodyBlock
		c.block.NewCall(c.palCondWait, notFull, mtx)
		closedAfterWait := c.block.NewLoad(irtypes.I8, closedPtr)
		isClosedAfterWait := c.block.NewICmp(enum.IPredEQ, closedAfterWait, constant.NewInt(irtypes.I8, 1))
		c.block.NewCondBr(isClosedAfterWait, waitFullClosedBlock, waitFullBlock)
	}

	// waitfull.closed: channel was closed while we were waiting — panic
	c.block = waitFullClosedBlock
	c.block.NewCall(c.palMutexUnlock, mtx)
	panicMsg2 := c.makeGlobalString("send on closed channel")
	c.block.NewCall(c.funcs["promise_panic"], panicMsg2)
	c.block.NewUnreachable()

	// write: memcpy value into buffer[tail * elem_size]
	c.block = writeBlock

	// Alloca value and store
	argVal := c.genCallArgExpr(e.Args[0].Value)
	argAlloca := c.block.NewAlloca(elemLLVM)
	c.block.NewStore(argVal, argAlloca)
	argAsI8 := c.block.NewBitCast(argAlloca, irtypes.I8Ptr)

	// Calculate dest = buffer + tail * elem_size
	bufPtr := c.block.NewGetElementPtr(chanType, chPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(chanFieldBuffer)))
	buf := c.block.NewLoad(irtypes.I8Ptr, bufPtr)
	tailPtr := c.block.NewGetElementPtr(chanType, chPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(chanFieldTail)))
	tail := c.block.NewLoad(irtypes.I64, tailPtr)
	offset := c.block.NewMul(tail, constant.NewInt(irtypes.I64, elemSize))
	dest := c.block.NewGetElementPtr(irtypes.I8, buf, offset)

	// memcpy(dest, &value, elem_size)
	c.block.NewCall(c.funcs["llvm.memcpy"], dest, argAsI8,
		constant.NewInt(irtypes.I64, elemSize), constant.False)

	// tail = (tail + 1) % capacity
	capReload := c.block.NewLoad(irtypes.I64, capPtr)
	tailPlusOne := c.block.NewAdd(tail, constant.NewInt(irtypes.I64, 1))
	newTail := c.block.NewURem(tailPlusOne, capReload)
	c.block.NewStore(newTail, tailPtr)

	// count++
	countReload := c.block.NewLoad(irtypes.I64, countPtr)
	newCount := c.block.NewAdd(countReload, constant.NewInt(irtypes.I64, 1))
	c.block.NewStore(newCount, countPtr)

	// Wake a waiting receiver (handles both regular G and select SWN nodes)
	recvHeadPtr := c.block.NewGetElementPtr(chanType, chPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(chanFieldRecvWaitersHead)))
	recvTailPtr := c.block.NewGetElementPtr(chanType, chPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(chanFieldRecvWaitersTail)))
	c.block.NewCall(c.funcs["promise_waiter_wake_one"], recvHeadPtr, recvTailPtr, notEmpty)

	// If unbuffered: wait until receiver picks up the value
	unbufPtr := c.block.NewGetElementPtr(chanType, chPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(chanFieldUnbuffered)))
	unbufVal := c.block.NewLoad(irtypes.I8, unbufPtr)
	isUnbuf := c.block.NewICmp(enum.IPredEQ, unbufVal, constant.NewInt(irtypes.I8, 1))

	rendezvousBlock := c.newBlock("send.rendezvous")
	doneBlock := c.newBlock("send.done")
	c.block.NewCondBr(isUnbuf, rendezvousBlock, doneBlock)

	// rendezvous: wait while count > 0 && !closed
	c.block = rendezvousBlock
	rendezvousCheckBlock := c.newBlock("send.rv.check")
	c.block.NewBr(rendezvousCheckBlock)

	c.block = rendezvousCheckBlock
	rvCount := c.block.NewLoad(irtypes.I64, countPtr)
	rvHasItems := c.block.NewICmp(enum.IPredUGT, rvCount, constant.NewInt(irtypes.I64, 0))
	rvClosedVal := c.block.NewLoad(irtypes.I8, closedPtr)
	isOpen := c.block.NewICmp(enum.IPredEQ, rvClosedVal, constant.NewInt(irtypes.I8, 0))
	shouldWait := c.block.NewAnd(rvHasItems, isOpen)

	rendezvousWaitBlock := c.newBlock("send.rv.wait")
	c.block.NewCondBr(shouldWait, rendezvousWaitBlock, doneBlock)

	if c.inCoroutine {
		// Goroutine mode rendezvous: park + suspend
		c.block = rendezvousWaitBlock
		rvCurrentG := c.block.NewLoad(irtypes.I8Ptr, c.currentGGlobal)
		rvSendHead := c.block.NewGetElementPtr(chanType, chPtr,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(chanFieldSendWaitersHead)))
		rvSendTail := c.block.NewGetElementPtr(chanType, chPtr,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(chanFieldSendWaitersTail)))
		c.block.NewCall(c.funcs["promise_waiter_enqueue"], rvSendHead, rvSendTail, rvCurrentG)
		// Store mutex in G.park_mutex — scheduler releases after coro.suspend completes
		gTyRv := goroutineStructType()
		rvGPtr := c.block.NewBitCast(rvCurrentG, irtypes.NewPointer(gTyRv))
		rvPmField := c.block.NewGetElementPtr(gTyRv, rvGPtr,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(gFieldParkMutex)))
		c.block.NewStore(mtx, rvPmField)

		rvSuspResult := c.block.NewCall(c.coroSuspend, constant.None, constant.False)
		rvResumeBlk := c.newBlock("send.rv.resume")
		c.block.NewSwitch(rvSuspResult, c.coroSuspendBlk,
			ir.NewCase(constant.NewInt(irtypes.I8, 0), rvResumeBlk),
			ir.NewCase(constant.NewInt(irtypes.I8, 1), c.coroCleanupBlk))

		c.block = rvResumeBlk
		c.block.NewCall(c.palMutexLock, mtx)
		c.block.NewBr(rendezvousCheckBlock)
	} else {
		// Thread-blocking mode rendezvous: cond_wait
		c.block = rendezvousWaitBlock
		c.block.NewCall(c.palCondWait, notFull, mtx)
		c.block.NewBr(rendezvousCheckBlock)
	}

	// done: unlock
	c.block = doneBlock
	c.block.NewCall(c.palMutexUnlock, mtx)

	return nil
}

// genChannelClose generates code for ch.close().
// lock → set closed=1 → broadcast both conds → unlock
func (c *Compiler) genChannelClose(chRaw value.Value, chPtr value.Value, chanType *irtypes.StructType) value.Value {
	// Load mutex
	mtxFieldPtr := c.block.NewGetElementPtr(chanType, chPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(chanFieldMutex)))
	mtx := c.block.NewLoad(irtypes.I8Ptr, mtxFieldPtr)

	// Lock
	c.block.NewCall(c.palMutexLock, mtx)

	// Check if already closed — panic on double-close
	closedPtr := c.block.NewGetElementPtr(chanType, chPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(chanFieldClosed)))
	closedVal := c.block.NewLoad(irtypes.I8, closedPtr)
	alreadyClosed := c.block.NewICmp(enum.IPredEQ, closedVal, constant.NewInt(irtypes.I8, 1))

	doubleClosePanic := c.newBlock("close.panic")
	closeOk := c.newBlock("close.ok")
	c.block.NewCondBr(alreadyClosed, doubleClosePanic, closeOk)

	c.block = doubleClosePanic
	c.block.NewCall(c.palMutexUnlock, mtx)
	panicMsg := c.makeGlobalString("close of closed channel")
	c.block.NewCall(c.funcs["promise_panic"], panicMsg)
	c.block.NewUnreachable()

	c.block = closeOk

	// Set closed = 1
	c.block.NewStore(constant.NewInt(irtypes.I8, 1), closedPtr)

	// Wake all goroutine waiters (send + recv)
	sendHeadPtr := c.block.NewGetElementPtr(chanType, chPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(chanFieldSendWaitersHead)))
	sendTailPtr := c.block.NewGetElementPtr(chanType, chPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(chanFieldSendWaitersTail)))
	c.block.NewCall(c.funcs["promise_waiter_wake_all"], sendHeadPtr, sendTailPtr)

	recvHeadPtr := c.block.NewGetElementPtr(chanType, chPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(chanFieldRecvWaitersHead)))
	recvTailPtr := c.block.NewGetElementPtr(chanType, chPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(chanFieldRecvWaitersTail)))
	c.block.NewCall(c.funcs["promise_waiter_wake_all"], recvHeadPtr, recvTailPtr)

	// Broadcast both cond vars to wake thread-blocked waiters
	neFieldPtr := c.block.NewGetElementPtr(chanType, chPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(chanFieldNotEmpty)))
	notEmpty := c.block.NewLoad(irtypes.I8Ptr, neFieldPtr)
	c.block.NewCall(c.palCondBroadcast, notEmpty)

	nfFieldPtr := c.block.NewGetElementPtr(chanType, chPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(chanFieldNotFull)))
	notFull := c.block.NewLoad(irtypes.I8Ptr, nfFieldPtr)
	c.block.NewCall(c.palCondBroadcast, notFull)

	// Unlock
	c.block.NewCall(c.palMutexUnlock, mtx)

	return nil
}

// genVectorLen loads the length from a vector/array header.
func (c *Compiler) genVectorLen(e *ast.MemberExpr) value.Value {
	slicePtr := c.genExpr(e.Target)
	headerType := vectorHeaderType()
	headerPtr := c.block.NewBitCast(slicePtr, irtypes.NewPointer(headerType))
	lenPtr := c.block.NewGetElementPtr(headerType, headerPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
	return c.block.NewLoad(irtypes.I64, lenPtr)
}

// genMapLen returns the length of a map via the runtime.
// genNativeHashGetter emits native hash computation for primitive types.
// Returns (value, true) if the type has a native hash getter, (nil, false) otherwise.
// All primitive hashes use the Promise-implemented _fnv1a_hash function.
// String hash uses a codegen-emitted LLVM IR function (__promise_hash_string).
func (c *Compiler) genNativeHashGetter(e *ast.MemberExpr, named *types.Named) (value.Value, bool) {
	target := c.genExpr(e.Target)
	hashFn := c.funcs["_fnv1a_hash"]
	switch named {
	case types.TypInt, types.TypI64, types.TypUint, types.TypU64:
		// Already i64 — call _fnv1a_hash directly
		return c.block.NewCall(hashFn, target), true
	case types.TypI32:
		ext := c.block.NewSExt(target, irtypes.I64)
		return c.block.NewCall(hashFn, ext), true
	case types.TypU32:
		ext := c.block.NewZExt(target, irtypes.I64)
		return c.block.NewCall(hashFn, ext), true
	case types.TypI16:
		ext := c.block.NewSExt(target, irtypes.I64)
		return c.block.NewCall(hashFn, ext), true
	case types.TypU16:
		ext := c.block.NewZExt(target, irtypes.I64)
		return c.block.NewCall(hashFn, ext), true
	case types.TypI8:
		ext := c.block.NewSExt(target, irtypes.I64)
		return c.block.NewCall(hashFn, ext), true
	case types.TypU8:
		ext := c.block.NewZExt(target, irtypes.I64)
		return c.block.NewCall(hashFn, ext), true
	case types.TypBool:
		// Hardcoded hash constants for bool (avoids hashing through fnv1a)
		trueHash := constant.NewInt(irtypes.I64, 0x517cc1b727220a95)
		falseHash := constant.NewInt(irtypes.I64, 0x6c62272e07bb0142)
		return c.block.NewSelect(target, trueHash, falseHash), true
	case types.TypChar:
		ext := c.block.NewZExt(target, irtypes.I64)
		return c.block.NewCall(hashFn, ext), true
	case types.TypF64:
		// Bitcast double to i64 bits, then hash via Promise _fnv1a_hash
		bits := c.block.NewBitCast(target, irtypes.I64)
		return c.block.NewCall(hashFn, bits), true
	case types.TypF32:
		// Bitcast float to i32 bits, zero-extend to i64, then hash
		bits := c.block.NewBitCast(target, irtypes.I32)
		ext := c.block.NewZExt(bits, irtypes.I64)
		return c.block.NewCall(hashFn, ext), true
	case types.TypString:
		// String hash uses codegen-emitted LLVM IR function
		return c.block.NewCall(c.funcs["__promise_hash_string"], target), true
	default:
		return nil, false
	}
}

// genFieldAccess loads a field value from a user type instance.
// Uses lookupTypeLayout for layout-driven field types that work for both
// regular and monomorphic types.
func (c *Compiler) genFieldAccess(e *ast.MemberExpr, typ types.Type, field *types.Field) value.Value {
	layout := c.lookupTypeLayout(typ)
	if layout == nil {
		panic(fmt.Sprintf("codegen: no layout for type %s", typ))
	}

	// Value types: fields are in the value struct, not an instance struct
	if layout.IsValueType {
		fieldIdx, ok := layout.ValueFieldIndex[field.Name()]
		if !ok {
			panic(fmt.Sprintf("codegen: field %s not in value layout for %s", field.Name(), typ))
		}
		targetVal := c.genExpr(e.Target)
		// `this` in value type methods is an i8* pointing to value struct
		if _, isThis := e.Target.(*ast.ThisExpr); isThis {
			valuePtrType := irtypes.NewPointer(layout.Value.LLVMType)
			typedPtr := c.block.NewBitCast(targetVal, valuePtrType)
			fieldPtr := c.block.NewGetElementPtr(layout.Value.LLVMType, typedPtr,
				constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(fieldIdx)))
			return c.block.NewLoad(layout.Value.Fields[fieldIdx].LLVMType, fieldPtr)
		}
		// For non-this targets, the value is the full value struct — extractvalue
		return c.block.NewExtractValue(targetVal, uint64(fieldIdx))
	}

	fieldIdx, ok := layout.InstanceFieldIndex[field.Name()]
	if !ok {
		panic(fmt.Sprintf("codegen: field %s not in instance layout for %s", field.Name(), typ))
	}

	targetVal := c.genExpr(e.Target)
	// `this` in methods is already an i8* instance pointer, not a value struct
	var instance value.Value
	if _, isThis := e.Target.(*ast.ThisExpr); isThis {
		instance = targetVal
	} else {
		instance = c.extractInstancePtr(targetVal)
	}
	typedPtr := c.block.NewBitCast(instance, layout.InstancePtrType)

	fieldPtr := c.block.NewGetElementPtr(layout.Instance.LLVMType, typedPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(fieldIdx)))

	// Use layout field type (not llvmType(field.Type()) which fails for TypeParams)
	return c.block.NewLoad(layout.Instance.Fields[fieldIdx].LLVMType, fieldPtr)
}

// --- ThisExpr ---

func (c *Compiler) genThisExpr() value.Value {
	alloca, ok := c.locals["this"]
	if !ok {
		panic("codegen: 'this' used but not in method context")
	}
	return c.block.NewLoad(alloca.ElemType, alloca)
}

// --- Method calls ---

// genMethodCall generates a method call on a user type instance.
func (c *Compiler) genMethodCall(e *ast.CallExpr, member *ast.MemberExpr) value.Value {
	targetType := c.info.Types[member.Target]
	// Apply typeSubst for mono context
	if c.typeSubst != nil {
		targetType = types.Substitute(targetType, c.typeSubst)
	}
	// Apply selfSubst for default method synthesis
	if c.selfSubst != nil {
		targetType = types.SubstituteSelf(targetType, c.selfSubst.iface, c.selfSubst.concrete)
	}

	// Container native method dispatch (Vector, Map, string)
	if result, ok := c.genContainerMethodCall(e, member, targetType); ok {
		return result
	}

	// Enum method dispatch
	if result, ok := c.genEnumMethodCall(e, member, targetType); ok {
		return result
	}

	named := extractNamed(targetType)
	if named == nil {
		panic(fmt.Sprintf("codegen: cannot resolve type for method call on %T", targetType))
	}

	method := named.LookupMethod(member.Field)
	if method == nil {
		panic(fmt.Sprintf("codegen: no method %s on type %s", member.Field, named))
	}

	// Virtual dispatch: if the static type needs vtable and the method is not native,
	// emit an indirect call through the vtable so the correct override is called.
	if c.needsVtable(named) && !method.IsNative() {
		return c.genVirtualMethodCall(e, member, named, method, targetType)
	}

	// Direct dispatch: resolve method to a compile-time-known function.
	// For mono/generic types, use resolveTypeName (handles Instance → mono name).
	// For regular Named types with inheritance, use resolveMethodOwner to find
	// the parent that actually defines the method.
	var mangledName string
	ownerName := c.resolveMethodOwner(named, member.Field)
	if ownerName != named.Obj().Name() {
		// Method inherited from parent. Check if the parent is structural —
		// if so, use the concrete type's name (methods are synthesized per-concrete).
		if structParent := c.findStructuralOwner(named, member.Field); structParent != nil {
			concreteName := c.resolveTypeName(targetType)
			c.ensureDefaultMethodsSynthesized(named, structParent)
			mangledName = mangleMethodName(concreteName, member.Field, false)
		} else {
			// Non-structural parent: use the monomorphized parent name.
			monoOwner := c.resolveMonoParentName(named, targetType, ownerName)
			mangledName = mangleMethodName(monoOwner, member.Field, false)
		}
	} else {
		mangledName = mangleMethodName(c.resolveTypeName(targetType), member.Field, false)
	}

	fn, ok := c.funcs[mangledName]
	if !ok {
		panic(fmt.Sprintf("codegen: undeclared method %s", mangledName))
	}

	var args []value.Value
	if method.Sig().Recv() != nil {
		target := c.genExpr(member.Target)
		// Container types (Vector, Map, string) are already i8* pointers — pass directly.
		// `this` in a method body is also i8*.
		// Primitive scalars (int, f64, bool, char, etc.) are raw values — pass directly.
		// Value types: store to temp alloca, pass pointer (value semantics).
		// Regular user types are value structs — extract the instance pointer.
		if _, isThis := member.Target.(*ast.ThisExpr); isThis {
			args = append(args, target)
		} else if isContainerType(targetType) {
			args = append(args, target)
		} else if isPrimitiveScalar(named) {
			args = append(args, target)
		} else if named.IsValueType() {
			args = append(args, c.valueTypeReceiverPtr(target, targetType))
		} else {
			args = append(args, c.extractInstancePtr(target))
		}
	}
	var argVals []value.Value
	var argTypes []types.Type
	for _, arg := range e.Args {
		argVals = append(argVals, c.genCallArgExpr(arg.Value))
		argTypes = append(argTypes, c.info.Types[arg.Value])
		// Clear drop flag: argument is moved into the callee
		if ident, ok := arg.Value.(*ast.IdentExpr); ok {
			c.clearDropFlag(ident.Name)
		}
	}
	argVals = c.coerceCallArgs(argVals, argTypes, method.Sig().Params())
	args = append(args, argVals...)

	return c.block.NewCall(fn, args...)
}

// genEnumGetterAccess emits a getter call on an enum value (e.g., s.name where name is a getter on enum Shape).
// Returns (result, true) if the enum has a matching getter, (nil, false) otherwise.
func (c *Compiler) genEnumGetterAccess(e *ast.MemberExpr, targetType types.Type, layout *TypeDeclLayout) (value.Value, bool) {
	var enum *types.Enum
	var enumName string
	switch t := targetType.(type) {
	case *types.Enum:
		enum = t
		enumName = t.Obj().Name()
	case *types.Instance:
		if en, ok := t.Origin().(*types.Enum); ok {
			enum = en
			enumName = monoName(t)
		}
	}
	if enum == nil {
		return nil, false
	}
	getter := enum.LookupGetter(e.Field)
	if getter == nil {
		return nil, false
	}

	mangledName := mangleMethodName(enumName, e.Field, false)
	fn, ok := c.funcs[mangledName]
	if !ok {
		return nil, false
	}

	// Pass the enum value as receiver
	target := c.genExpr(e.Target)
	var ptr value.Value
	// `this` inside an enum method is already i8* pointing to the enum alloca — pass directly.
	if _, isThis := e.Target.(*ast.ThisExpr); isThis {
		ptr = target
	} else {
		alloca := c.entryBlock.NewAlloca(target.Type())
		alloca.SetName(c.uniqueLocalName("enum.getter"))
		c.block.NewStore(target, alloca)
		ptr = c.block.NewBitCast(alloca, irtypes.I8Ptr)
	}

	return c.block.NewCall(fn, ptr), true
}

// genEnumMethodCall generates a method call on an enum value.
// Returns (result, true) if the target is an enum with a matching method, (nil, false) otherwise.
func (c *Compiler) genEnumMethodCall(e *ast.CallExpr, member *ast.MemberExpr, targetType types.Type) (value.Value, bool) {
	var enum *types.Enum
	var enumName string

	switch t := targetType.(type) {
	case *types.Enum:
		enum = t
		enumName = t.Obj().Name()
	case *types.Instance:
		if en, ok := t.Origin().(*types.Enum); ok {
			enum = en
			enumName = monoName(t)
		}
	default:
		return nil, false
	}

	if enum == nil {
		return nil, false
	}

	method := enum.LookupMethod(member.Field)
	if method == nil {
		return nil, false
	}

	mangledName := mangleMethodName(enumName, member.Field, false)
	fn, ok := c.funcs[mangledName]
	if !ok {
		return nil, false
	}

	var args []value.Value
	if method.Sig().Recv() != nil {
		target := c.genExpr(member.Target)
		// `this` inside an enum method is already i8* pointing to the enum alloca — pass directly.
		if _, isThis := member.Target.(*ast.ThisExpr); isThis {
			args = append(args, target)
		} else {
			// Store the enum value to a temp alloca and pass pointer as i8*.
			// Use the actual LLVM type of the value (i32 for fieldless, struct for data enums).
			alloca := c.entryBlock.NewAlloca(target.Type())
			alloca.SetName(c.uniqueLocalName("enum.this"))
			c.block.NewStore(target, alloca)
			ptr := c.block.NewBitCast(alloca, irtypes.I8Ptr)
			args = append(args, ptr)
		}
	}
	var argVals []value.Value
	var argTypes []types.Type
	for _, arg := range e.Args {
		argVals = append(argVals, c.genExpr(arg.Value))
		argTypes = append(argTypes, c.info.Types[arg.Value])
		if ident, ok := arg.Value.(*ast.IdentExpr); ok {
			c.clearDropFlag(ident.Name)
		}
	}
	argVals = c.coerceCallArgs(argVals, argTypes, method.Sig().Params())
	args = append(args, argVals...)

	return c.block.NewCall(fn, args...), true
}

// genGetterCall emits a call to a getter method (zero args beyond receiver).
// Uses virtual dispatch through the vtable when the static type needs it.
func (c *Compiler) genGetterCall(e *ast.MemberExpr, targetType types.Type, named *types.Named, getter *types.Method) value.Value {
	// Global getter: no receiver, just call the function directly.
	if getter.Sig().Recv() == nil {
		mangledName := mangleMethodName(c.resolveTypeName(targetType), e.Field, false)
		fn, ok := c.funcs[mangledName]
		if !ok {
			panic(fmt.Sprintf("codegen: undeclared global getter %s", mangledName))
		}
		return c.block.NewCall(fn)
	}

	// Virtual dispatch for getter when static type needs vtable
	if c.needsVtable(named) && !getter.IsNative() {
		return c.genVirtualGetterCall(e, named, getter, targetType)
	}

	var mangledName string
	ownerName := c.resolveMethodOwner(named, e.Field)
	if ownerName != named.Obj().Name() {
		// Getter inherited from parent. Resolve to mono name if parent is generic.
		monoOwner := c.resolveMonoParentName(named, targetType, ownerName)
		mangledName = mangleMethodName(monoOwner, e.Field, false)
	} else {
		mangledName = mangleMethodName(c.resolveTypeName(targetType), e.Field, false)
	}

	fn, ok := c.funcs[mangledName]
	if !ok {
		panic(fmt.Sprintf("codegen: undeclared getter %s", mangledName))
	}

	var args []value.Value
	target := c.genExpr(e.Target)
	if _, isThis := e.Target.(*ast.ThisExpr); isThis {
		args = append(args, target)
	} else if isContainerType(targetType) {
		args = append(args, target)
	} else if isPrimitiveScalar(named) {
		args = append(args, target)
	} else if named.IsValueType() {
		args = append(args, c.valueTypeReceiverPtr(target, targetType))
	} else {
		args = append(args, c.extractInstancePtr(target))
	}

	return c.block.NewCall(fn, args...)
}

// genVirtualGetterCall emits an indirect getter call through the vtable.
func (c *Compiler) genVirtualGetterCall(e *ast.MemberExpr, named *types.Named, getter *types.Method, targetType types.Type) value.Value {
	receiverVal := c.genExpr(e.Target)

	var vtableRaw, instance value.Value
	if _, isThis := e.Target.(*ast.ThisExpr); isThis {
		instance = receiverVal
		variantPtr := c.loadVariantPtr(receiverVal)
		typeinfoStruct := irtypes.NewStruct(irtypes.I8Ptr)
		typeinfoPtr := c.block.NewBitCast(variantPtr, irtypes.NewPointer(typeinfoStruct))
		vtableFieldPtr := c.block.NewGetElementPtr(typeinfoStruct, typeinfoPtr,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
		vtableRaw = c.block.NewLoad(irtypes.I8Ptr, vtableFieldPtr)
	} else {
		vtableRaw = c.extractVtablePtr(receiverVal)
		instance = c.extractInstancePtr(receiverVal)
	}

	slotIndex := named.VirtualMethodIndex(e.Field, false) // getter, not setter
	if slotIndex < 0 {
		panic(fmt.Sprintf("codegen: getter %s not in vtable for %s", e.Field, named))
	}
	vtablePtr := c.block.NewBitCast(vtableRaw, irtypes.NewPointer(irtypes.I8Ptr))
	fnSlotPtr := c.block.NewGetElementPtr(irtypes.I8Ptr, vtablePtr,
		constant.NewInt(irtypes.I32, int64(slotIndex)))
	fnRaw := c.block.NewLoad(irtypes.I8Ptr, fnSlotPtr)

	// Substitute type params for generic instances (e.g. Transformer[int])
	var vtableSubst map[*types.TypeParam]types.Type
	if inst, ok := targetType.(*types.Instance); ok {
		origin, _ := inst.Origin().(*types.Named)
		if origin != nil && len(origin.TypeParams()) > 0 {
			vtableSubst = types.BuildSubstMap(origin.TypeParams(), inst.TypeArgs())
		}
	}
	resolveVtableType := func(t types.Type) irtypes.Type {
		if vtableSubst != nil {
			t = types.Substitute(t, vtableSubst)
		}
		return c.resolveType(t)
	}

	retType := irtypes.Type(irtypes.Void)
	if getter.Sig().Result() != nil {
		retType = resolveVtableType(getter.Sig().Result())
	}
	if getter.Sig().CanError() {
		retType = computeResultType(retType)
	}
	paramTypes := []irtypes.Type{irtypes.I8Ptr}
	funcType := irtypes.NewFunc(retType, paramTypes...)
	fnTyped := c.block.NewBitCast(fnRaw, irtypes.NewPointer(funcType))

	return c.block.NewCall(fnTyped, instance)
}

// genVirtualMethodCall emits an indirect call through the vtable.
// Reads vtable pointer from the value struct (field 0), indexes into it
// to get the function pointer, casts it, and calls.
func (c *Compiler) genVirtualMethodCall(e *ast.CallExpr, member *ast.MemberExpr,
	named *types.Named, method *types.Method, targetType types.Type) value.Value {

	// 1. Evaluate receiver
	receiverVal := c.genExpr(member.Target)

	// 2. Extract vtable and instance
	var vtableRaw, instance value.Value
	if _, isThis := member.Target.(*ast.ThisExpr); isThis {
		// `this` is already i8* — load vtable from typeinfo chain
		instance = receiverVal
		variantPtr := c.loadVariantPtr(receiverVal)
		// typeinfo field 0 is vtable_ptr
		typeinfoStruct := irtypes.NewStruct(irtypes.I8Ptr)
		typeinfoPtr := c.block.NewBitCast(variantPtr, irtypes.NewPointer(typeinfoStruct))
		vtableFieldPtr := c.block.NewGetElementPtr(typeinfoStruct, typeinfoPtr,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
		vtableRaw = c.block.NewLoad(irtypes.I8Ptr, vtableFieldPtr)
	} else {
		vtableRaw = c.extractVtablePtr(receiverVal)
		instance = c.extractInstancePtr(receiverVal)
	}

	// 3. Index into vtable — use the STATIC type's slot layout
	slotIndex := named.VirtualMethodIndex(member.Field, false) // regular method, not setter
	if slotIndex < 0 {
		panic(fmt.Sprintf("codegen: method %s not in vtable for %s", member.Field, named))
	}
	vtablePtr := c.block.NewBitCast(vtableRaw, irtypes.NewPointer(irtypes.I8Ptr))
	fnSlotPtr := c.block.NewGetElementPtr(irtypes.I8Ptr, vtablePtr,
		constant.NewInt(irtypes.I32, int64(slotIndex)))
	fnRaw := c.block.NewLoad(irtypes.I8Ptr, fnSlotPtr)

	// 4. Build the correct function type and bitcast.
	// If the static type is a generic instance (e.g. Transformer[int]),
	// substitute type params so T→int in method signatures.
	var vtableSubst map[*types.TypeParam]types.Type
	if inst, ok := targetType.(*types.Instance); ok {
		origin, _ := inst.Origin().(*types.Named)
		if origin != nil && len(origin.TypeParams()) > 0 {
			vtableSubst = types.BuildSubstMap(origin.TypeParams(), inst.TypeArgs())
		}
	}
	resolveVtableType := func(t types.Type) irtypes.Type {
		if vtableSubst != nil {
			t = types.Substitute(t, vtableSubst)
		}
		return c.resolveType(t)
	}

	retType := irtypes.Type(irtypes.Void)
	if method.Sig().Result() != nil {
		retType = resolveVtableType(method.Sig().Result())
	}
	if method.Sig().CanError() {
		retType = computeResultType(retType)
	}
	var paramTypes []irtypes.Type
	if method.Sig().Recv() != nil {
		paramTypes = append(paramTypes, irtypes.I8Ptr)
	}
	for _, p := range method.Sig().Params() {
		paramTypes = append(paramTypes, resolveVtableType(p.Type()))
	}
	funcType := irtypes.NewFunc(retType, paramTypes...)
	fnTyped := c.block.NewBitCast(fnRaw, irtypes.NewPointer(funcType))

	// 5. Call — receiver is instance (i8*), not the value struct
	var args []value.Value
	if method.Sig().Recv() != nil {
		args = append(args, instance)
	}
	var argVals []value.Value
	var argTypes []types.Type
	for _, arg := range e.Args {
		argVals = append(argVals, c.genCallArgExpr(arg.Value))
		argTypes = append(argTypes, c.info.Types[arg.Value])
		// Clear drop flag: argument is moved into the callee
		if ident, ok := arg.Value.(*ast.IdentExpr); ok {
			c.clearDropFlag(ident.Name)
		}
	}
	argVals = c.coerceCallArgs(argVals, argTypes, method.Sig().Params())
	args = append(args, argVals...)
	return c.block.NewCall(fnTyped, args...)
}

// genContainerMethodCall dispatches native method calls on Vector, Map, and string.
// Returns (result, true) if handled, (nil, false) otherwise.
// Non-native methods (with Promise bodies) fall through to the regular call path.
// Handles both Instance wrappers (user code: Vector[int]) and bare Named types
// (method body: this is TypVector) by resolving type args from typeSubst.
func (c *Compiler) genContainerMethodCall(e *ast.CallExpr, member *ast.MemberExpr, targetType types.Type) (value.Value, bool) {
	methodName := member.Field

	// Check if the method is native — only native methods are handled here.
	// Non-native methods fall through to the regular user method path.
	named := extractNamed(targetType)
	if named == types.TypVector || named == types.TypString || named == types.TypChannel {
		m := named.LookupMethod(methodName)
		if m == nil || !m.IsNative() {
			return nil, false // fall through to regular method dispatch
		}
	}

	// Vector methods: push, pop, contains, remove
	if elem, ok := types.AsVector(targetType); ok {
		return c.genVectorMethodCall(e, member, elem, methodName), true
	}
	// Bare TypVector (inside a method body on Vector): resolve T from typeSubst
	if named == types.TypVector {
		if elem := c.resolveTypeParam(types.TypVector.TypeParams()[0]); elem != nil {
			return c.genVectorMethodCall(e, member, elem, methodName), true
		}
	}

	// Channel methods: send, close
	if elem, ok := types.AsChannel(targetType); ok {
		return c.genChannelMethodCall(e, member, elem, methodName), true
	}
	if named == types.TypChannel {
		if elem := c.resolveTypeParam(types.TypChannel.TypeParams()[0]); elem != nil {
			return c.genChannelMethodCall(e, member, elem, methodName), true
		}
	}

	// String native methods: trim, split (contains/starts_with/ends_with/index_of are now pure Promise)
	if named == types.TypString {
		if result, ok := c.genStringMethodCall(e, member, methodName); ok {
			return result, true
		}
	}

	return nil, false
}

// resolveTypeParam looks up a type parameter in the current typeSubst map.
// Returns nil if not in a monomorphic context or the param is not mapped.
func (c *Compiler) resolveTypeParam(tp *types.TypeParam) types.Type {
	if c.typeSubst == nil {
		return nil
	}
	return c.typeSubst[tp]
}

func (c *Compiler) genVectorMethodCall(e *ast.CallExpr, member *ast.MemberExpr, elemType types.Type, method string) value.Value {
	slicePtr := c.genExpr(member.Target)
	elemLLVM := c.resolveType(elemType)
	elemSize := int64(c.typeSize(elemLLVM))

	switch method {
	case "push":
		argVal := c.genCallArgExpr(e.Args[0].Value)
		argAlloca := c.createEntryAlloca(elemLLVM)
		// Zero-initialize before store to clear padding bytes for memcmp correctness
		c.block.NewStore(constant.NewZeroInitializer(elemLLVM), argAlloca)
		c.block.NewStore(argVal, argAlloca)
		argPtr := c.block.NewBitCast(argAlloca, irtypes.I8Ptr)
		newSlice := c.block.NewCall(c.funcs["promise_vector_push"],
			slicePtr, argPtr, constant.NewInt(irtypes.I64, elemSize))
		// Store the (possibly reallocated) pointer back
		c.storeBackSlicePtr(member.Target, newSlice)
		return newSlice

	case "pop":
		outAlloca := c.createEntryAlloca(elemLLVM)
		outPtr := c.block.NewBitCast(outAlloca, irtypes.I8Ptr)
		found := c.block.NewCall(c.funcs["promise_vector_pop"],
			slicePtr, outPtr, constant.NewInt(irtypes.I64, elemSize))
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
		argVal := c.genCallArgExpr(e.Args[0].Value)
		argAlloca := c.createEntryAlloca(elemLLVM)
		// Zero-initialize before store to clear padding bytes for memcmp correctness
		c.block.NewStore(constant.NewZeroInitializer(elemLLVM), argAlloca)
		c.block.NewStore(argVal, argAlloca)
		argPtr := c.block.NewBitCast(argAlloca, irtypes.I8Ptr)
		// Use string equality for string elements
		var eqFn value.Value
		if extractNamed(elemType) == types.TypString {
			eqFn = c.block.NewBitCast(c.funcs["__promise_eq_string"], irtypes.I8Ptr)
		} else {
			eqFn = constant.NewNull(irtypes.I8Ptr)
		}
		result := c.block.NewCall(c.funcs["promise_vector_contains"],
			slicePtr, argPtr, constant.NewInt(irtypes.I64, elemSize), eqFn)
		return c.block.NewTrunc(result, irtypes.I1)

	case "remove":
		idx := c.genCallArgExpr(e.Args[0].Value)
		c.block.NewCall(c.funcs["promise_vector_remove"],
			slicePtr, idx, constant.NewInt(irtypes.I64, elemSize))
		return nil

	default:
		panic(fmt.Sprintf("codegen: unknown vector method %s", method))
	}
}

// storeBackSlicePtr stores the new vector pointer back into the variable that holds the vector.
// This is needed because push may realloc.
func (c *Compiler) storeBackSlicePtr(target ast.Expr, newPtr value.Value) {
	switch t := target.(type) {
	case *ast.IdentExpr:
		if alloca, ok := c.locals[t.Name]; ok {
			c.block.NewStore(newPtr, alloca)
		}
	case *ast.MemberExpr:
		fieldPtr := c.genFieldPtr(t)
		c.block.NewStore(newPtr, fieldPtr)
	case *ast.IndexExpr:
		panic("codegen: push on nested slice (e.g. slices[i].push) not yet supported")
	}
}

// genFieldPtr computes a pointer to a field on a user type instance.
// Used by storeBackSlicePtr and genMemberAssign.
func (c *Compiler) genFieldPtr(target *ast.MemberExpr) value.Value {
	targetType := c.info.Types[target.Target]
	if c.typeSubst != nil {
		targetType = types.Substitute(targetType, c.typeSubst)
	}
	if c.selfSubst != nil {
		targetType = types.SubstituteSelf(targetType, c.selfSubst.iface, c.selfSubst.concrete)
	}
	named := extractNamed(targetType)
	if named == nil {
		panic("codegen: cannot resolve type for field pointer")
	}

	layout := c.lookupTypeLayout(targetType)
	if layout == nil {
		panic(fmt.Sprintf("codegen: no layout for type %s", targetType))
	}

	field := named.LookupField(target.Field)
	if field == nil {
		panic(fmt.Sprintf("codegen: no field %s on type %s", target.Field, named))
	}

	// Value types: GEP directly into the variable's alloca or this pointer
	if layout.IsValueType {
		fieldIdx, ok := layout.ValueFieldIndex[field.Name()]
		if !ok {
			panic(fmt.Sprintf("codegen: field %s not in value layout for %s", field.Name(), named))
		}
		valuePtrType := irtypes.NewPointer(layout.Value.LLVMType)
		if _, isThis := target.Target.(*ast.ThisExpr); isThis {
			// this is an i8* pointing to the value struct
			thisVal := c.genExpr(target.Target)
			typedPtr := c.block.NewBitCast(thisVal, valuePtrType)
			return c.block.NewGetElementPtr(layout.Value.LLVMType, typedPtr,
				constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(fieldIdx)))
		}
		// For local variables, get the alloca directly
		if ident, ok := target.Target.(*ast.IdentExpr); ok {
			if alloca, ok := c.locals[ident.Name]; ok {
				typedPtr := c.block.NewBitCast(alloca, valuePtrType)
				return c.block.NewGetElementPtr(layout.Value.LLVMType, typedPtr,
					constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(fieldIdx)))
			}
		}
		panic(fmt.Sprintf("codegen: value type field assignment requires addressable target for %s.%s", named, field.Name()))
	}

	fieldIdx, ok := layout.InstanceFieldIndex[field.Name()]
	if !ok {
		panic(fmt.Sprintf("codegen: field %s not in layout for %s", field.Name(), named))
	}

	obj := c.genExpr(target.Target)
	var instance value.Value
	if _, isThis := target.Target.(*ast.ThisExpr); isThis {
		instance = obj
	} else {
		instance = c.extractInstancePtr(obj)
	}
	typedPtr := c.block.NewBitCast(instance, layout.InstancePtrType)

	return c.block.NewGetElementPtr(layout.Instance.LLVMType, typedPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(fieldIdx)))
}

func (c *Compiler) genStringMethodCall(e *ast.CallExpr, member *ast.MemberExpr, method string) (value.Value, bool) {
	// Factory methods (no receiver — target is a type name, not a value)
	if method == "from_bytes" {
		return c.genStringFromBytes(e), true
	}

	strPtr := c.genExpr(member.Target)

	switch method {
	case "trim":
		result := c.block.NewCall(c.funcs["promise_string_trim"], strPtr)
		return result, true

	case "split":
		argVal := c.genCallArgExpr(e.Args[0].Value)
		result := c.block.NewCall(c.funcs["promise_string_split"], strPtr, argVal)
		return result, true

	case "to_upper":
		result := c.block.NewCall(c.funcs["promise_string_to_upper"], strPtr)
		return result, true

	case "to_lower":
		result := c.block.NewCall(c.funcs["promise_string_to_lower"], strPtr)
		return result, true

	case "repeat":
		argVal := c.genCallArgExpr(e.Args[0].Value)
		result := c.block.NewCall(c.funcs["promise_string_repeat"], strPtr, argVal)
		return result, true

	case "bytes":
		return c.genStringBytes(strPtr), true

	case "byte_at":
		argVal := c.genCallArgExpr(e.Args[0].Value)
		return c.genStringByteAt(strPtr, argVal), true

	default:
		return nil, false
	}
}

// genStringFromBytes creates a string from a Vector[u8] (factory method).
// Reads the vector's count and data pointer, calls promise_string_new.
func (c *Compiler) genStringFromBytes(e *ast.CallExpr) value.Value {
	vecPtr := c.genCallArgExpr(e.Args[0].Value)
	// Clear drop flag for moved argument
	if ident, ok := e.Args[0].Value.(*ast.IdentExpr); ok {
		c.clearDropFlag(ident.Name)
	}

	// Vector layout: {i64 count, i64 capacity} header, then data at offset 16
	headerType := vectorHeaderType() // {i64, i64}
	hdrPtr := c.block.NewBitCast(vecPtr, irtypes.NewPointer(headerType))
	countPtr := c.block.NewGetElementPtr(headerType, hdrPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
	count := c.block.NewLoad(irtypes.I64, countPtr)

	// Data starts at offset vectorHeaderSize (16)
	dataPtr := c.block.NewGetElementPtr(irtypes.I8, vecPtr,
		constant.NewInt(irtypes.I64, int64(vectorHeaderSize)))

	return c.block.NewCall(c.funcs["promise_string_new"], dataPtr, count)
}

// genStringLen loads the length field from a string instance struct.
// String instance layout: { i8* _variant, i64 len, [0 x i8] data }
func (c *Compiler) genStringLen(e *ast.MemberExpr) value.Value {
	strPtr := c.genExpr(e.Target)
	// Build string instance struct type: { i8*, i64, [0 x i8] }
	strInstanceType := irtypes.NewStruct(
		irtypes.I8Ptr,                   // _variant
		irtypes.I64,                     // len
		irtypes.NewArray(0, irtypes.I8), // data (flexible array)
	)
	typedPtr := c.block.NewBitCast(strPtr, irtypes.NewPointer(strInstanceType))
	lenPtr := c.block.NewGetElementPtr(strInstanceType, typedPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 1))
	return c.block.NewLoad(irtypes.I64, lenPtr)
}

// genStringBytes creates a Vector[u8] from the string's raw bytes.
// Allocates a new vector, memcpys string data into it, sets count = string len.
func (c *Compiler) genStringBytes(strPtr value.Value) value.Value {
	strInstanceType := irtypes.NewStruct(
		irtypes.I8Ptr,                   // _variant
		irtypes.I64,                     // len
		irtypes.NewArray(0, irtypes.I8), // data (flexible array)
	)
	typedPtr := c.block.NewBitCast(strPtr, irtypes.NewPointer(strInstanceType))

	// Load string length
	lenPtr := c.block.NewGetElementPtr(strInstanceType, typedPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 1))
	strLen := c.block.NewLoad(irtypes.I64, lenPtr)

	// Get pointer to string data (field 2)
	dataPtr := c.block.NewGetElementPtr(strInstanceType, typedPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 2),
		constant.NewInt(irtypes.I32, 0))

	// Allocate vector with capacity = strLen, elem_size = 1
	vec := c.block.NewCall(c.funcs["promise_vector_with_capacity"],
		strLen, constant.NewInt(irtypes.I64, 1))

	// Copy string data into vector data area (offset 16 = vectorHeaderSize)
	headerSizeConst := constant.NewInt(irtypes.I64, int64(vectorHeaderSize))
	vecDataPtr := c.block.NewGetElementPtr(irtypes.I8, vec, headerSizeConst)
	c.block.NewCall(c.funcs["llvm.memcpy"], vecDataPtr, dataPtr, strLen, constant.False)

	// Set vector count = strLen
	headerType := vectorHeaderType() // {i64, i64}
	hdrPtr := c.block.NewBitCast(vec, irtypes.NewPointer(headerType))
	countPtr := c.block.NewGetElementPtr(headerType, hdrPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
	c.block.NewStore(strLen, countPtr)

	return vec
}

// genStringByteAt returns the raw byte at a given byte offset in the string.
// Unlike string[], this does NOT do UTF-8 decoding — it returns u8 directly.
func (c *Compiler) genStringByteAt(strPtr, index value.Value) value.Value {
	strInstanceType := irtypes.NewStruct(
		irtypes.I8Ptr,                   // _variant
		irtypes.I64,                     // len
		irtypes.NewArray(0, irtypes.I8), // data (flexible array)
	)
	typedPtr := c.block.NewBitCast(strPtr, irtypes.NewPointer(strInstanceType))

	// Get pointer to string data
	dataPtr := c.block.NewGetElementPtr(strInstanceType, typedPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 2),
		constant.NewInt(irtypes.I32, 0))

	// GEP to data[index], load byte
	bytePtr := c.block.NewGetElementPtr(irtypes.I8, dataPtr, index)
	return c.block.NewLoad(irtypes.I8, bytePtr)
}

// --- Enum variant values ---

// genEnumVariantValueLayout generates a fieldless enum variant value using layout dispatch.
func (c *Compiler) genEnumVariantValueLayout(layout *TypeDeclLayout, variantName string) value.Value {
	tag, ok := layout.VariantTag[variantName]
	if !ok {
		panic(fmt.Sprintf("codegen: variant %q not found in enum layout", variantName))
	}

	if layout.MaxVariantDataSize == 0 {
		return constant.NewInt(irtypes.I32, int64(tag))
	}

	internalType := layout.EnumInternalType.(*irtypes.StructType)
	var agg value.Value = constant.NewZeroInitializer(internalType)
	agg = c.block.NewInsertValue(agg, constant.NewInt(irtypes.I32, int64(tag)), 0)
	return agg
}

// genEnumVariantCallLayout generates a variant constructor call using layout dispatch.
func (c *Compiler) genEnumVariantCallLayout(e *ast.CallExpr, member *ast.MemberExpr, layout *TypeDeclLayout) value.Value {
	tag, ok := layout.VariantTag[member.Field]
	if !ok {
		panic(fmt.Sprintf("codegen: variant %q not found in enum layout", member.Field))
	}
	dataType := layout.VariantDataTypes[member.Field]

	internalType := layout.EnumInternalType.(*irtypes.StructType)
	alloca := c.block.NewAlloca(internalType)

	tagPtr := c.block.NewGetElementPtr(internalType, alloca,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
	c.block.NewStore(constant.NewInt(irtypes.I32, int64(tag)), tagPtr)

	if dataType != nil && len(e.Args) > 0 {
		dataPtr := c.block.NewGetElementPtr(internalType, alloca,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 1))
		typedDataPtr := c.block.NewBitCast(dataPtr, irtypes.NewPointer(dataType))

		for i, arg := range e.Args {
			val := c.genCallArgExpr(arg.Value)
			fieldPtr := c.block.NewGetElementPtr(dataType, typedDataPtr,
				constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(i)))
			c.block.NewStore(val, fieldPtr)
			// Clear drop flag: field value is moved into the enum variant
			if ident, ok := arg.Value.(*ast.IdentExpr); ok {
				c.clearDropFlag(ident.Name)
			}
		}
	}

	return c.block.NewLoad(internalType, alloca)
}

// --- Match expressions ---

// genMatchExpr generates a match expression. Dispatches to enum match (tag-based switch)
// or value match (literal comparison chain) based on subject type.
func (c *Compiler) genMatchExpr(e *ast.MatchExpr) value.Value {
	subject := c.genExpr(e.Subject)
	subjectType := c.info.Types[e.Subject]
	// Apply typeSubst for mono context
	if c.typeSubst != nil {
		subjectType = types.Substitute(subjectType, c.typeSubst)
	}

	if enumLayout := c.lookupEnumLayout(subjectType); enumLayout != nil {
		enum := extractEnum(subjectType)
		// If subject is i8* (e.g., `this` inside an enum method), load the enum value
		if subject.Type().Equal(irtypes.I8Ptr) {
			var loadType irtypes.Type
			if enumLayout.MaxVariantDataSize == 0 {
				loadType = irtypes.I32 // fieldless enum: tag only
			} else {
				loadType = enumLayout.EnumInternalType // data enum: {i32 tag, [N x i8] data}
			}
			typedPtr := c.block.NewBitCast(subject, irtypes.NewPointer(loadType))
			subject = c.block.NewLoad(loadType, typedPtr)
		}
		return c.genEnumMatch(e, subject, enum, enumLayout)
	}

	return c.genValueMatch(e, subject, subjectType)
}

// genEnumMatch generates a match expression on an enum value using an LLVM switch instruction.
func (c *Compiler) genEnumMatch(e *ast.MatchExpr, subject value.Value, enum *types.Enum, layout *TypeDeclLayout) value.Value {
	// Extract tag from subject
	var tag value.Value
	if layout.MaxVariantDataSize == 0 {
		tag = subject // fieldless enum, subject IS the tag
	} else {
		tag = c.block.NewExtractValue(subject, 0)
	}

	switchBlock := c.block
	mergeBlock := c.newBlock("match.end")

	var defaultTarget *ir.Block
	var cases []*ir.Case
	var arms []matchArmInfo

	for i, arm := range e.Arms {
		armBlock := c.newBlock(fmt.Sprintf("match.arm%d", i))

		switch p := arm.Pattern.(type) {
		case *ast.EnumVariantMatchPattern:
			tagVal := constant.NewInt(irtypes.I32, int64(layout.VariantTag[p.Variant]))
			cases = append(cases, &ir.Case{X: tagVal, Target: armBlock})

		case *ast.EnumDestructureMatchPattern:
			tagVal := constant.NewInt(irtypes.I32, int64(layout.VariantTag[p.Variant]))
			cases = append(cases, &ir.Case{X: tagVal, Target: armBlock})

		case *ast.ShortDestructureMatchPattern:
			tagVal := constant.NewInt(irtypes.I32, int64(layout.VariantTag[p.Name]))
			cases = append(cases, &ir.Case{X: tagVal, Target: armBlock})

		case *ast.WildcardMatchPattern:
			defaultTarget = armBlock

		case *ast.NameMatchPattern:
			defaultTarget = armBlock
		}

		// Generate arm body
		c.block = armBlock
		c.bindMatchPattern(arm.Pattern, subject, enum, layout)

		var armVal value.Value
		if arm.Body != nil {
			armVal = c.genExpr(arm.Body)
		} else if arm.Block != nil {
			armVal = c.genBlockValue(arm.Block)
		}

		armEnd := c.block
		if c.block.Term == nil {
			c.block.NewBr(mergeBlock)
		}

		arms = append(arms, matchArmInfo{val: armVal, end: armEnd, hasV: armVal != nil})
	}

	if defaultTarget == nil {
		// Exhaustive match — default case is unreachable.
		// We must NOT route to mergeBlock because the phi has no incoming for this edge.
		unreachableBlock := c.newBlock("match.unreachable")
		unreachableBlock.NewUnreachable()
		defaultTarget = unreachableBlock
	}

	switchBlock.NewSwitch(tag, defaultTarget, cases...)

	c.block = mergeBlock
	return buildMatchPhi(mergeBlock, arms)
}

// matchArmInfo tracks a match arm's result value and final block for PHI construction.
type matchArmInfo struct {
	val  value.Value
	end  *ir.Block
	hasV bool
}

// buildMatchPhi constructs a PHI node at mergeBlock from collected match arm info.
// Arms that branch to mergeBlock but produce no value get a null placeholder.
// Returns nil if no arm produces a value (match used as statement).
func buildMatchPhi(mergeBlock *ir.Block, arms []matchArmInfo) value.Value {
	// Filter out void-typed values — they cannot participate in phi nodes.
	for i := range arms {
		if arms[i].val != nil {
			if _, isVoid := arms[i].val.Type().(*irtypes.VoidType); isVoid {
				arms[i].val = nil
				arms[i].hasV = false
			}
		}
	}

	hasAnyValue := false
	for _, a := range arms {
		if a.hasV {
			hasAnyValue = true
			break
		}
	}
	if !hasAnyValue {
		return nil
	}

	// Find a representative non-nil value type for zero-filling arms without values.
	var valType irtypes.Type
	for _, a := range arms {
		if a.hasV && a.val != nil {
			valType = a.val.Type()
			break
		}
	}

	var incomings []*ir.Incoming
	for _, a := range arms {
		// Skip arms that don't branch to mergeBlock (e.g. early return/break)
		branchesToMerge := false
		if a.end.Term != nil {
			if br, ok := a.end.Term.(*ir.TermBr); ok && br.Target == mergeBlock {
				branchesToMerge = true
			}
		}
		if !branchesToMerge {
			continue
		}
		v := a.val
		if v == nil && valType != nil {
			v = constant.NewZeroInitializer(valType)
		} else if v == nil {
			v = constant.NewNull(irtypes.I8Ptr)
		}
		incomings = append(incomings, &ir.Incoming{X: v, Pred: a.end})
	}
	if len(incomings) > 0 {
		return mergeBlock.NewPhi(incomings...)
	}
	return nil
}

// genValueMatch generates a match expression on a non-enum value using comparison chains.
func (c *Compiler) genValueMatch(e *ast.MatchExpr, subject value.Value, subjectType types.Type) value.Value {
	mergeBlock := c.newBlock("match.end")

	named := extractNamed(subjectType)

	var arms []matchArmInfo

	for i, arm := range e.Arms {
		switch p := arm.Pattern.(type) {
		case *ast.LiteralMatchPattern:
			lit := c.genExpr(p.Value)

			var cond value.Value
			if named != nil {
				method := named.LookupMethod("==")
				if method != nil && method.IsNative() {
					if named == types.TypString {
						cond = c.genStringOp("==", subject, lit)
					} else {
						cond = c.emitNativeOp(named, "==", subject, lit)
					}
				}
			}
			if cond == nil {
				panic(fmt.Sprintf("codegen: cannot compare match subject of type %s", subjectType))
			}

			armBlock := c.newBlock(fmt.Sprintf("match.arm%d", i))
			nextBlock := c.newBlock(fmt.Sprintf("match.next%d", i))
			c.block.NewCondBr(cond, armBlock, nextBlock)

			c.block = armBlock
			var armVal value.Value
			if arm.Body != nil {
				armVal = c.genExpr(arm.Body)
			} else if arm.Block != nil {
				armVal = c.genBlockValue(arm.Block)
			}
			armEnd := c.block
			if c.block.Term == nil {
				c.block.NewBr(mergeBlock)
			}
			arms = append(arms, matchArmInfo{val: armVal, end: armEnd, hasV: armVal != nil})

			c.block = nextBlock

		case *ast.WildcardMatchPattern, *ast.NameMatchPattern:
			// Bind name pattern variable (needed before evaluating guard)
			bindBlock := c.newBlock(fmt.Sprintf("match.bind%d", i))
			c.block.NewBr(bindBlock)
			c.block = bindBlock

			if np, ok := p.(*ast.NameMatchPattern); ok && np.Name != "_" {
				lt := subject.Type()
				alloca := c.createEntryAlloca(lt)
				alloca.SetName(c.uniqueLocalName(np.Name))
				c.block.NewStore(subject, alloca)
				c.locals[np.Name] = alloca
			}

			// If there's a guard, evaluate it and conditionally branch
			if arm.Guard != nil {
				guardVal := c.genExpr(arm.Guard)
				armBlock := c.newBlock(fmt.Sprintf("match.arm%d", i))
				nextBlock := c.newBlock(fmt.Sprintf("match.next%d", i))
				c.block.NewCondBr(guardVal, armBlock, nextBlock)

				c.block = armBlock
				var armVal value.Value
				if arm.Body != nil {
					armVal = c.genExpr(arm.Body)
				} else if arm.Block != nil {
					armVal = c.genBlockValue(arm.Block)
				}
				armEnd := c.block
				if c.block.Term == nil {
					c.block.NewBr(mergeBlock)
				}
				arms = append(arms, matchArmInfo{val: armVal, end: armEnd, hasV: armVal != nil})

				c.block = nextBlock
				// Guard failed — continue to next arm (don't return early)
			} else {
				// No guard — unconditional default arm
				var armVal value.Value
				if arm.Body != nil {
					armVal = c.genExpr(arm.Body)
				} else if arm.Block != nil {
					armVal = c.genBlockValue(arm.Block)
				}
				armEnd := c.block
				if c.block.Term == nil {
					c.block.NewBr(mergeBlock)
				}
				arms = append(arms, matchArmInfo{val: armVal, end: armEnd, hasV: armVal != nil})

				// After an unguarded wildcard/name, no more arms need checking
				c.block = mergeBlock
				return buildMatchPhi(mergeBlock, arms)
			}
		}
	}

	// If we fell through without a default, branch to merge
	if c.block.Term == nil {
		c.block.NewBr(mergeBlock)
	}

	c.block = mergeBlock
	return buildMatchPhi(mergeBlock, arms)
}

// bindMatchPattern binds pattern variables from a match arm into the current scope.
func (c *Compiler) bindMatchPattern(pat ast.MatchPattern, subject value.Value, enum *types.Enum, layout *TypeDeclLayout) {
	switch p := pat.(type) {
	case *ast.EnumDestructureMatchPattern:
		c.bindEnumDestructure(p.Bindings, p.Variant, subject, enum, layout)

	case *ast.ShortDestructureMatchPattern:
		c.bindEnumDestructure(p.Bindings, p.Name, subject, enum, layout)

	case *ast.NameMatchPattern:
		if p.Name != "_" {
			lt := subject.Type()
			alloca := c.createEntryAlloca(lt)
			c.block.NewStore(subject, alloca)
			c.locals[p.Name] = alloca
		}

	case *ast.EnumVariantMatchPattern:
		// No bindings for fieldless variant patterns

	case *ast.WildcardMatchPattern:
		// No bindings
	}
}

// bindEnumDestructure extracts variant data fields and binds them to local variables.
func (c *Compiler) bindEnumDestructure(bindings []string, variantName string, subject value.Value, enum *types.Enum, layout *TypeDeclLayout) {
	variant := enum.LookupVariant(variantName)
	if variant == nil || variant.NumFields() == 0 {
		return
	}

	dataType := layout.VariantDataTypes[variantName]
	if dataType == nil {
		return
	}

	// Alloca the subject struct and GEP to data area.
	// EnumInternalType is guaranteed to be a struct here because we returned early
	// above when variant has no fields (which is the only case where it would be i32).
	internalType := layout.EnumInternalType.(*irtypes.StructType)
	alloca := c.createEntryAlloca(internalType)
	c.block.NewStore(subject, alloca)

	dataPtr := c.block.NewGetElementPtr(internalType, alloca,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 1))
	typedDataPtr := c.block.NewBitCast(dataPtr, irtypes.NewPointer(dataType))

	for i, binding := range bindings {
		if binding == "_" {
			continue
		}
		if i >= variant.NumFields() {
			break
		}
		// Use layout data type fields (already substituted for mono types)
		fieldType := dataType.Fields[i]
		fieldPtr := c.block.NewGetElementPtr(dataType, typedDataPtr,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(i)))
		val := c.block.NewLoad(fieldType, fieldPtr)

		bindAlloca := c.createEntryAlloca(fieldType)
		c.block.NewStore(val, bindAlloca)
		c.locals[binding] = bindAlloca
	}
}

// --- If expressions ---

func (c *Compiler) genIfExpr(e *ast.IfExpr) value.Value {
	cond := c.genExpr(e.Cond)

	thenBlock := c.newBlock("if.then")
	elseBlock := c.newBlock("if.else")
	mergeBlock := c.newBlock("if.merge")

	c.block.NewCondBr(cond, thenBlock, elseBlock)

	// Then branch
	c.block = thenBlock
	thenVal := c.genBlockValue(e.Then)
	thenEnd := c.block
	if c.block.Term == nil {
		c.block.NewBr(mergeBlock)
	}

	// Else branch
	c.block = elseBlock
	elseVal := c.genBlockValue(e.Else)
	elseEnd := c.block
	if c.block.Term == nil {
		c.block.NewBr(mergeBlock)
	}

	c.block = mergeBlock

	// If both branches produce values, create a phi node
	if thenVal != nil && elseVal != nil {
		phi := mergeBlock.NewPhi(
			&ir.Incoming{X: thenVal, Pred: thenEnd},
			&ir.Incoming{X: elseVal, Pred: elseEnd},
		)
		return phi
	}

	return nil
}

// --- Error handling expressions ---

// genErrorPropagateExpr generates the `expr?` operator.
// Evaluates the inner failable call, checks the tag, propagates the error
// to the caller on error, or extracts the Ok value on success.
func (c *Compiler) genErrorPropagateExpr(e *ast.ErrorPropagateExpr) value.Value {
	result := c.genExpr(e.Expr)
	calleeResultType := result.Type().(*irtypes.StructType)

	tag := c.block.NewExtractValue(result, 0)

	propagateBlock := c.newBlock("error.propagate")
	okBlock := c.newBlock("error.ok")
	c.block.NewCondBr(tag, propagateBlock, okBlock)

	// Error path: cleanup scope bindings, extract error, wrap in caller's result type, early return
	c.block = propagateBlock
	if len(c.scopeBindings) > 0 {
		c.emitScopeCleanup(0)
	}
	errVal := c.block.NewExtractValue(result, resultErrIdx(calleeResultType))
	callerResultType := c.currentResultType()
	c.block.NewRet(c.wrapError(errVal, callerResultType))

	// Ok path: extract value
	c.block = okBlock
	if !isVoidResult(calleeResultType) {
		return c.block.NewExtractValue(result, 1)
	}
	return nil
}

// genErrorUnwrapExpr generates the `expr!` operator.
// Evaluates the inner failable call, panics on error, or extracts the Ok value.
// Also handles optional unwrap: T? ! → T, panic on none.
func (c *Compiler) genErrorUnwrapExpr(e *ast.ErrorUnwrapExpr) value.Value {
	// Optional unwrap: T? ! → extract T, panic on none
	if c.info.OptionalUnwraps[e] {
		return c.genOptionalForceUnwrap(e.Expr)
	}

	result := c.genExpr(e.Expr)
	resultType := result.Type().(*irtypes.StructType)

	tag := c.block.NewExtractValue(result, 0)

	panicBlock := c.newBlock("error.panic")
	okBlock := c.newBlock("error.ok")
	c.block.NewCondBr(tag, panicBlock, okBlock)

	// Error: call promise_panic, unreachable
	c.block = panicBlock
	errMsg := c.block.NewExtractValue(result, resultErrIdx(resultType))
	c.block.NewCall(c.funcs["promise_panic"], errMsg)
	c.block.NewUnreachable()

	// Ok: extract value
	c.block = okBlock
	if !isVoidResult(resultType) {
		return c.block.NewExtractValue(result, 1)
	}
	return nil
}

// genErrorHandlerExpr generates the `expr ? binding { body }` operator.
// Evaluates the inner failable call, runs the handler on error (with optional
// error binding), or extracts the Ok value on success. Merges with phi if
// both branches produce values.
//
// For typed handlers (`? e is IoError { ... }`), an RTTI check is performed on
// the error instance. If the check fails, the error is propagated (in failable
// functions) or causes a panic (in non-failable functions).
func (c *Compiler) genErrorHandlerExpr(e *ast.ErrorHandlerExpr) value.Value {
	// Optional handler: T? ? { recovery } → T
	if c.info.OptionalHandlers[e] {
		return c.genOptionalHandlerExpr(e)
	}

	result := c.genExpr(e.Expr)
	resultType := result.Type().(*irtypes.StructType)

	tag := c.block.NewExtractValue(result, 0)

	handlerBlock := c.newBlock("error.handler")
	okBlock := c.newBlock("error.ok")
	mergeBlock := c.newBlock("error.merge")
	c.block.NewCondBr(tag, handlerBlock, okBlock)

	// Handler block
	c.block = handlerBlock
	errVal := c.block.NewExtractValue(result, resultErrIdx(resultType))

	var noMatchVal value.Value
	var noMatchEnd *ir.Block

	// For typed handlers, perform RTTI check before entering the handler body
	if e.TypeName != "" {
		targetNamed := c.lookupNamedType(e.TypeName)
		if targetNamed == nil {
			panic(fmt.Sprintf("codegen: undefined type %s in error handler", e.TypeName))
		}
		targetID := c.assignTypeID(targetNamed)

		variantPtr := c.loadVariantPtr(errVal)
		rttiResult := c.block.NewCall(c.funcs["promise_type_is"],
			variantPtr, constant.NewInt(irtypes.I32, int64(targetID)))
		typeMatch := c.block.NewICmp(enum.IPredNE, rttiResult, constant.NewInt(irtypes.I32, 0))

		matchBlock := c.newBlock("error.typed.match")
		noMatchBlock := c.newBlock("error.typed.nomatch")
		c.block.NewCondBr(typeMatch, matchBlock, noMatchBlock)

		// No-match path: else body, panic (!), or propagate
		c.block = noMatchBlock
		if e.ElseBody != nil {
			// else clause: bind error and run else body
			if e.ElseBinding != "" && e.ElseBinding != "_" {
				elseValStruct := c.reconstructErrorValue(errVal)
				alloca := c.block.NewAlloca(userValueType())
				alloca.SetName(c.uniqueLocalName(e.ElseBinding))
				c.block.NewStore(elseValStruct, alloca)
				c.locals[e.ElseBinding] = alloca
			}
			noMatchVal = c.genBlockValue(e.ElseBody)
			elseDiverged := c.block.Term != nil
			if !elseDiverged {
				noMatchEnd = c.block
				c.block.NewBr(mergeBlock)
			}
		} else if e.PanicOnNomatch {
			// Explicit ! suffix: panic on non-matching error
			c.block.NewCall(c.funcs["promise_panic"], errVal)
			c.block.NewUnreachable()
		} else if c.canError {
			if len(c.scopeBindings) > 0 {
				c.emitScopeCleanup(0)
			}
			callerResultType := c.currentResultType()
			c.block.NewRet(c.wrapError(errVal, callerResultType))
		} else {
			// Should not be reached — sema rejects typed handlers in
			// non-failable functions without else or !
			panicMsg := c.makeGlobalString("unhandled error type")
			c.block.NewCall(c.funcs["promise_panic"], panicMsg)
			c.block.NewUnreachable()
		}

		// Match path: continue to bind and run handler body
		c.block = matchBlock
	}

	// Bind the error variable as a value struct {vtable_ptr, instance_ptr}
	if e.Binding != "" && e.Binding != "_" {
		valStruct := c.reconstructErrorValue(errVal)
		alloca := c.block.NewAlloca(userValueType())
		alloca.SetName(c.uniqueLocalName(e.Binding))
		c.block.NewStore(valStruct, alloca)
		c.locals[e.Binding] = alloca
	}
	handlerVal := c.genBlockValue(e.Body)
	handlerEnd := c.block
	if c.block.Term == nil {
		c.block.NewBr(mergeBlock)
	}

	// Ok path: extract value
	c.block = okBlock
	var okVal value.Value
	if !isVoidResult(resultType) {
		okVal = c.block.NewExtractValue(result, 1)
	}

	// Optional recovery: wrap ok value as some(T), non-recovering paths produce none.
	if c.info.OptionalRecoveryHandlers[e] {
		semaType := c.info.Types[e]
		if c.typeSubst != nil {
			semaType = types.Substitute(semaType, c.typeSubst)
		}
		optLLVM := c.resolveType(semaType)
		optStructType, _ := optLLVM.(*irtypes.StructType)

		// Wrap ok value as some(T) in the ok block.
		if optStructType != nil && okVal != nil {
			okVal = c.wrapOptional(okVal, optStructType)
		}
		c.block.NewBr(mergeBlock)
		okEnd := c.block

		noneVal := c.zeroValue(optLLVM)

		// Wrap handler value in its block (before its br to merge).
		var handlerOptVal value.Value = noneVal
		handlerReachesMerge := false
		if handlerEnd.Term != nil {
			if _, isBr := handlerEnd.Term.(*ir.TermBr); isBr {
				handlerReachesMerge = true
				if handlerVal != nil {
					if _, isVoid := handlerVal.Type().(*irtypes.VoidType); !isVoid {
						// Insert wrapOptional before the existing br terminator.
						savedBlock := c.block
						c.block = handlerEnd
						handlerEnd.Term = nil // remove br temporarily
						handlerOptVal = c.wrapOptional(handlerVal, optStructType)
						c.block.NewBr(mergeBlock) // re-add br
						c.block = savedBlock
					}
				}
			}
		}

		// Wrap noMatch value in its block.
		var noMatchOptVal value.Value = noneVal
		noMatchReachesMerge := false
		if noMatchEnd != nil {
			noMatchReachesMerge = true
			if noMatchVal != nil {
				if _, isVoid := noMatchVal.Type().(*irtypes.VoidType); !isVoid {
					savedBlock := c.block
					c.block = noMatchEnd
					noMatchEnd.Term = nil
					noMatchOptVal = c.wrapOptional(noMatchVal, optStructType)
					c.block.NewBr(mergeBlock)
					c.block = savedBlock
				}
			}
		}

		c.block = mergeBlock
		var incomings []*ir.Incoming
		incomings = append(incomings, &ir.Incoming{X: okVal, Pred: okEnd})
		if handlerReachesMerge {
			incomings = append(incomings, &ir.Incoming{X: handlerOptVal, Pred: handlerEnd})
		}
		if noMatchReachesMerge {
			incomings = append(incomings, &ir.Incoming{X: noMatchOptVal, Pred: noMatchEnd})
		}

		if len(incomings) > 1 {
			return mergeBlock.NewPhi(incomings...)
		}
		return okVal
	}

	c.block.NewBr(mergeBlock)
	okEnd := c.block

	// Merge with phi if both paths produce compatible values.
	// Treat void-typed values as nil (void call results cannot participate in phi).
	c.block = mergeBlock
	if handlerVal != nil {
		if _, isVoid := handlerVal.Type().(*irtypes.VoidType); isVoid {
			handlerVal = nil
		}
	}
	if noMatchVal != nil {
		if _, isVoid := noMatchVal.Type().(*irtypes.VoidType); isVoid {
			noMatchVal = nil
		}
	}
	if okVal != nil && handlerVal != nil {
		incomings := []*ir.Incoming{
			{X: okVal, Pred: okEnd},
			{X: handlerVal, Pred: handlerEnd},
		}
		if noMatchEnd != nil && noMatchVal != nil {
			incomings = append(incomings, &ir.Incoming{X: noMatchVal, Pred: noMatchEnd})
		}
		return mergeBlock.NewPhi(incomings...)
	}
	// okVal defined in okBlock doesn't dominate mergeBlock when handler also
	// reaches mergeBlock. Use a phi with a zero default from the handler path.
	if okVal != nil && handlerEnd.Term != nil {
		if _, isBr := handlerEnd.Term.(*ir.TermBr); isBr {
			zeroVal := c.zeroValue(okVal.Type())
			incomings := []*ir.Incoming{
				{X: okVal, Pred: okEnd},
				{X: zeroVal, Pred: handlerEnd},
			}
			if noMatchEnd != nil {
				noMatchZero := c.zeroValue(okVal.Type())
				incomings = append(incomings, &ir.Incoming{X: noMatchZero, Pred: noMatchEnd})
			}
			return mergeBlock.NewPhi(incomings...)
		}
	}
	return okVal
}

// reconstructErrorValue builds a value struct {vtable_ptr, instance_ptr} from a raw i8* error pointer.
func (c *Compiler) reconstructErrorValue(errPtr value.Value) value.Value {
	variantPtr := c.loadVariantPtr(errPtr)
	typeinfoStruct := irtypes.NewStruct(irtypes.I8Ptr)
	typeinfoPtr := c.block.NewBitCast(variantPtr, irtypes.NewPointer(typeinfoStruct))
	vtableFieldPtr := c.block.NewGetElementPtr(typeinfoStruct, typeinfoPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
	vtablePtr := c.block.NewLoad(irtypes.I8Ptr, vtableFieldPtr)
	var valStruct value.Value = constant.NewZeroInitializer(userValueType())
	valStruct = c.block.NewInsertValue(valStruct, vtablePtr, 0)
	valStruct = c.block.NewInsertValue(valStruct, errPtr, 1)
	return valStruct
}

// --- Tuple ---

func (c *Compiler) genTupleLit(e *ast.TupleLit) value.Value {
	lt := c.resolveType(c.info.Types[e])
	structType, ok := lt.(*irtypes.StructType)
	if !ok {
		panic(fmt.Sprintf("codegen: tuple type resolved to %T, want StructType", lt))
	}
	var agg value.Value = constant.NewZeroInitializer(structType)
	for i, elem := range e.Elements {
		agg = c.block.NewInsertValue(agg, c.genExpr(elem), uint64(i))
	}
	return agg
}

// --- Optional ---

func (c *Compiler) genNoneLit(e *ast.NoneLit) value.Value {
	if c.targetType != nil {
		lt := c.resolveType(c.targetType)
		return c.zeroValue(lt)
	}
	return constant.NewInt(irtypes.I1, 0) // void optional fallback
}

// wrapOptional wraps a value into an optional struct: { true, val }.
func (c *Compiler) wrapOptional(val value.Value, optType *irtypes.StructType) value.Value {
	var agg value.Value = constant.NewUndef(optType)
	agg = c.block.NewInsertValue(agg, constant.NewInt(irtypes.I1, 1), 0)
	agg = c.block.NewInsertValue(agg, val, 1)
	return agg
}

// wrapReturnOptional wraps val in an Optional struct if retType is Optional
// but the expression type is a non-optional, non-none value.
func (c *Compiler) wrapReturnOptional(val value.Value, expr ast.Expr, retType types.Type) value.Value {
	if retType == nil {
		return val
	}
	if _, isOpt := retType.(*types.Optional); !isOpt {
		return val
	}
	exprType := c.info.Types[expr]
	if c.typeSubst != nil {
		exprType = types.Substitute(exprType, c.typeSubst)
	}
	// NoneLit already produces the correct zero value via targetType
	if exprType == types.TypNone {
		return val
	}
	// Already Optional — no wrapping needed
	if _, exprOpt := exprType.(*types.Optional); exprOpt {
		return val
	}
	lt := c.resolveType(retType)
	if st, ok := lt.(*irtypes.StructType); ok {
		return c.wrapOptional(val, st)
	}
	return val
}

func (c *Compiler) genElvis(e *ast.BinaryExpr) value.Value {
	optVal := c.genExpr(e.Left)

	// Extract the present flag (field 0)
	flag := c.block.NewExtractValue(optVal, 0)

	someBlock := c.newBlock("elvis.some")
	noneBlock := c.newBlock("elvis.none")
	mergeBlock := c.newBlock("elvis.merge")

	c.block.NewCondBr(flag, someBlock, noneBlock)

	// Some path: extract inner value
	c.block = someBlock
	someVal := c.block.NewExtractValue(optVal, 1)
	c.block.NewBr(mergeBlock)
	someEnd := c.block

	// None path: evaluate default
	c.block = noneBlock
	defaultVal := c.genExpr(e.Right)
	noneEnd := c.block
	c.block.NewBr(mergeBlock)

	// Merge
	c.block = mergeBlock
	return mergeBlock.NewPhi(
		&ir.Incoming{X: someVal, Pred: someEnd},
		&ir.Incoming{X: defaultVal, Pred: noneEnd},
	)
}

// --- Vector / Array Literal ---

const vectorHeaderSize = 16

func vectorHeaderType() *irtypes.StructType {
	return irtypes.NewStruct(irtypes.I64, irtypes.I64)
}

func (c *Compiler) genArrayLit(e *ast.ArrayLit) value.Value {
	typ := c.info.Types[e]
	if c.typeSubst != nil {
		typ = types.Substitute(typ, c.typeSubst)
	}

	// Fixed-size array: stack-allocated [N x T]
	if arr, ok := typ.(*types.Array); ok {
		return c.genFixedArrayLit(e, arr)
	}

	elem, ok := types.AsVector(typ)
	if !ok {
		panic(fmt.Sprintf("codegen: array literal type is %T, want Vector instance or Array", typ))
	}
	elemLLVM := c.resolveType(elem)
	elemSize := int64(c.typeSize(elemLLVM))
	n := int64(len(e.Elements))

	// Total allocation: header (16 bytes) + n * elemSize
	totalSize := int64(vectorHeaderSize) + n*elemSize

	// malloc
	rawPtr := c.block.NewCall(c.palAlloc,
		constant.NewInt(irtypes.I64, totalSize))

	// Store len and cap via header GEP
	headerType := vectorHeaderType()
	headerPtr := c.block.NewBitCast(rawPtr, irtypes.NewPointer(headerType))
	lenPtr := c.block.NewGetElementPtr(headerType, headerPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
	c.block.NewStore(constant.NewInt(irtypes.I64, n), lenPtr)

	capPtr := c.block.NewGetElementPtr(headerType, headerPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 1))
	c.block.NewStore(constant.NewInt(irtypes.I64, n), capPtr)

	// Store elements: ptr + 16 bytes (header), then index by element type
	dataBase := c.block.NewGetElementPtr(irtypes.I8, rawPtr,
		constant.NewInt(irtypes.I64, int64(vectorHeaderSize)))
	dataTypedPtr := c.block.NewBitCast(dataBase, irtypes.NewPointer(elemLLVM))

	for i, elemExpr := range e.Elements {
		val := c.genCallArgExpr(elemExpr)
		elemPtr := c.block.NewGetElementPtr(elemLLVM, dataTypedPtr,
			constant.NewInt(irtypes.I64, int64(i)))
		c.block.NewStore(val, elemPtr)
	}

	return rawPtr // i8*
}

// genFixedArrayLit generates a stack-allocated fixed-size array literal.
// Returns the full [N x T] value (not a pointer).
func (c *Compiler) genFixedArrayLit(e *ast.ArrayLit, arr *types.Array) value.Value {
	elemLLVM := c.resolveType(arr.Elem())
	arrType := irtypes.NewArray(uint64(arr.Size()), elemLLVM)

	tmp := c.block.NewAlloca(arrType)
	for i, elemExpr := range e.Elements {
		val := c.genCallArgExpr(elemExpr)
		ptr := c.block.NewGetElementPtr(arrType, tmp,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(i)))
		c.block.NewStore(val, ptr)
	}
	return c.block.NewLoad(arrType, tmp)
}

// --- Index Expression ---

func (c *Compiler) genSliceExpr(e *ast.SliceExpr) value.Value {
	targetType := c.info.Types[e.Target]
	if c.typeSubst != nil {
		targetType = types.Substitute(targetType, c.typeSubst)
	}

	named := extractNamed(targetType)
	if named == nil {
		panic(fmt.Sprintf("codegen: cannot slice type %s", targetType))
	}
	m := named.LookupMethod("[:]")
	if m == nil {
		panic(fmt.Sprintf("codegen: no [:] method on type %s", named))
	}

	target := c.genExpr(e.Target)

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
	if isContainerType(targetType) {
		instancePtr = target
	} else if named != nil && named.IsValueType() {
		instancePtr = c.valueTypeReceiverPtr(target, targetType)
	} else {
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
	arrVal := c.genExpr(target)
	elemLLVM := c.resolveType(arr.Elem())
	arrType := irtypes.NewArray(uint64(arr.Size()), elemLLVM)
	tmp := c.block.NewAlloca(arrType)
	c.block.NewStore(arrVal, tmp)
	return tmp
}

// genArrayIndex handles arr[i] for fixed-size arrays with bounds checking.
func (c *Compiler) genArrayIndex(e *ast.IndexExpr, arr *types.Array) value.Value {
	basePtr := c.genArrayBasePtr(e.Target, arr)
	idx := c.genExpr(e.Index)
	elemLLVM := c.resolveType(arr.Elem())
	arrType := irtypes.NewArray(uint64(arr.Size()), elemLLVM)

	// Bounds check: idx < N
	size := constant.NewInt(irtypes.I64, arr.Size())
	inBounds := c.block.NewICmp(enum.IPredULT, idx, size)
	okBlock := c.newBlock("arridx.ok")
	panicBlock := c.newBlock("arridx.oob")
	c.block.NewCondBr(inBounds, okBlock, panicBlock)

	c.block = panicBlock
	oobMsg := c.makeGlobalString("array index out of bounds")
	c.block.NewCall(c.funcs["promise_panic"], oobMsg)
	c.block.NewUnreachable()

	c.block = okBlock
	elemPtr := c.block.NewGetElementPtr(arrType, basePtr,
		constant.NewInt(irtypes.I32, 0), idx)
	return c.block.NewLoad(elemLLVM, elemPtr)
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
	strInstanceType := irtypes.NewStruct(
		irtypes.I8Ptr,                   // _variant
		irtypes.I64,                     // len
		irtypes.NewArray(0, irtypes.I8), // data (flexible array)
	)
	typedPtr := c.block.NewBitCast(strPtr, irtypes.NewPointer(strInstanceType))

	// Load string length
	lenPtr := c.block.NewGetElementPtr(strInstanceType, typedPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 1))
	length := c.block.NewLoad(irtypes.I64, lenPtr)

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
	dataPtr := c.block.NewGetElementPtr(strInstanceType, typedPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 2),
		constant.NewInt(irtypes.I32, 0))
	sliceDataPtr := c.block.NewGetElementPtr(irtypes.I8, dataPtr, start)

	// Create new string via promise_string_new
	return c.block.NewCall(c.funcs["promise_string_new"], sliceDataPtr, sliceLen)
}

// genMethodIndex calls the monomorphized [] method on a user type.
func (c *Compiler) genMethodIndex(e *ast.IndexExpr, targetType types.Type) value.Value {
	// Resolve mangled method name
	mangledName := mangleMethodName(c.resolveTypeName(targetType), "[]", false)
	fn, ok := c.funcs[mangledName]
	if !ok {
		panic(fmt.Sprintf("codegen: undeclared [] method %s", mangledName))
	}

	target := c.genExpr(e.Target)
	keyVal := c.genExpr(e.Index)

	// Extract instance pointer: container types (Vector, Map) are already i8*,
	// value types store to temp alloca, regular user types extract instance ptr.
	named := extractNamed(targetType)
	var instancePtr value.Value
	if isContainerType(targetType) {
		instancePtr = target
	} else if named != nil && named.IsValueType() {
		instancePtr = c.valueTypeReceiverPtr(target, targetType)
	} else {
		instancePtr = c.extractInstancePtr(target)
	}

	return c.block.NewCall(fn, instancePtr, keyVal)
}

// genStringIndex implements string byte indexing: s[i] returns the byte at position i
// as a char (i32), zero-extended from i8. This is byte indexing (like Go's string[i]),
// not character indexing. UTF-8 decoding is handled separately by for-in loops.
// String instance layout: { i8* _variant, i64 len, [0 x i8] data }
func (c *Compiler) genStringIndex(e *ast.IndexExpr) value.Value {
	strPtr := c.genExpr(e.Target)
	idx := c.genExpr(e.Index)

	strInstanceType := irtypes.NewStruct(
		irtypes.I8Ptr,                   // _variant
		irtypes.I64,                     // len
		irtypes.NewArray(0, irtypes.I8), // data (flexible array)
	)
	typedPtr := c.block.NewBitCast(strPtr, irtypes.NewPointer(strInstanceType))

	// Load len for bounds check
	lenPtr := c.block.NewGetElementPtr(strInstanceType, typedPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 1))
	length := c.block.NewLoad(irtypes.I64, lenPtr)

	// Bounds check (unsigned comparison handles negative indices too)
	inBounds := c.block.NewICmp(enum.IPredULT, idx, length)
	okBlock := c.newBlock("stridx.ok")
	panicBlock := c.newBlock("stridx.oob")
	c.block.NewCondBr(inBounds, okBlock, panicBlock)

	// Out of bounds: panic
	c.block = panicBlock
	oobMsg := c.makeGlobalString("string index out of bounds")
	c.block.NewCall(c.funcs["promise_panic"], oobMsg)
	c.block.NewUnreachable()

	// In bounds: load byte, zero-extend to i32 (char)
	c.block = okBlock
	dataPtr := c.block.NewGetElementPtr(strInstanceType, typedPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 2),
		constant.NewInt(irtypes.I32, 0))
	bytePtr := c.block.NewGetElementPtr(irtypes.I8, dataPtr, idx)
	byteVal := c.block.NewLoad(irtypes.I8, bytePtr)
	return c.block.NewZExt(byteVal, irtypes.I32)
}

func (c *Compiler) genVectorIndex(e *ast.IndexExpr, elemType types.Type) value.Value {
	slicePtr := c.genExpr(e.Target)
	idx := c.genExpr(e.Index)
	elemLLVM := c.resolveType(elemType)

	// Bounds check: load len, compare index
	headerType := vectorHeaderType()
	headerPtr := c.block.NewBitCast(slicePtr, irtypes.NewPointer(headerType))
	lenPtr := c.block.NewGetElementPtr(headerType, headerPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
	length := c.block.NewLoad(irtypes.I64, lenPtr)

	inBounds := c.block.NewICmp(enum.IPredULT, idx, length)
	okBlock := c.newBlock("index.ok")
	panicBlock := c.newBlock("index.oob")
	c.block.NewCondBr(inBounds, okBlock, panicBlock)

	// Out of bounds: panic
	c.block = panicBlock
	oobMsg := c.makeGlobalString("index out of bounds")
	c.block.NewCall(c.funcs["promise_panic"], oobMsg)
	c.block.NewUnreachable()

	// In bounds: load element
	c.block = okBlock
	dataBase := c.block.NewGetElementPtr(irtypes.I8, slicePtr,
		constant.NewInt(irtypes.I64, int64(vectorHeaderSize)))
	dataTypedPtr := c.block.NewBitCast(dataBase, irtypes.NewPointer(elemLLVM))
	elemPtr := c.block.NewGetElementPtr(elemLLVM, dataTypedPtr, idx)
	return c.block.NewLoad(elemLLVM, elemPtr)
}

// makeGlobalString creates a global null-terminated string constant and returns an i8* to it.
// fnv1aStr computes a 32-bit FNV-1a hash of a string for content-based naming.
func fnv1aStr(s string) uint32 {
	h := uint32(2166136261)
	for i := 0; i < len(s); i++ {
		h ^= uint32(s[i])
		h *= 16777619
	}
	return h
}

// getCStrGlobal returns a deduplicated immutable global for a null-terminated
// C string. Content-based naming (.cstr.<hash>) makes these stable across
// compilations regardless of which mono instances are present.
func (c *Compiler) getCStrGlobal(s string) *ir.Global {
	global, ok := c.cstrGlobals[s]
	if !ok {
		data := constant.NewCharArrayFromString(s + "\x00")
		globalName := fmt.Sprintf(".cstr.%x", fnv1aStr(s))
		global = c.module.NewGlobalDef(globalName, data)
		global.Immutable = true
		global.Linkage = enum.LinkagePrivate
		c.cstrGlobals[s] = global
	}
	return global
}

func (c *Compiler) makeGlobalString(s string) value.Value {
	global := c.getCStrGlobal(s)
	return c.block.NewGetElementPtr(global.ContentType, global,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
}

// --- Map ---

// genMapLit creates a map instance via its new() constructor, then inserts each entry
// via the monomorphized []= method. Map is now a Promise-implemented user type.
func (c *Compiler) genMapLit(e *ast.MapLit) value.Value {
	typ := c.info.Types[e]
	if c.typeSubst != nil {
		typ = types.Substitute(typ, c.typeSubst)
	}
	_, _, ok := types.AsMap(typ)
	if !ok {
		panic(fmt.Sprintf("codegen: map literal type is %T, want Map instance", typ))
	}
	inst, ok := typ.(*types.Instance)
	if !ok {
		panic(fmt.Sprintf("codegen: map literal type is %T, want Instance", typ))
	}

	// Construct the map (allocate + call new()) — reuse genConstructorCallMono logic
	mapVal := c.genMapConstructor(inst)

	// Insert entries via monomorphized []= method
	if len(e.Entries) > 0 {
		name := monoName(inst)
		setFnName := mangleMethodName(name, "[]=", false)
		setFn, ok := c.funcs[setFnName]
		if !ok {
			panic(fmt.Sprintf("codegen: undeclared map []= method %s", setFnName))
		}
		instancePtr := c.extractInstancePtr(mapVal)
		for _, entry := range e.Entries {
			keyVal := c.genExpr(entry.Key)
			valVal := c.genExpr(entry.Value)
			c.block.NewCall(setFn, instancePtr, keyVal, valVal)
		}
	}

	return mapVal
}

// genMapConstructor allocates a map instance and calls its new() constructor.
func (c *Compiler) genMapConstructor(inst *types.Instance) value.Value {
	layout := c.lookupTypeLayout(inst)
	if layout == nil {
		panic(fmt.Sprintf("codegen: no layout for map type %s", inst))
	}

	instanceStructType := layout.Instance.LLVMType
	instancePtrType := layout.InstancePtrType

	// Compute size via GEP-from-null trick
	nullPtr := constant.NewNull(instancePtrType)
	sizePtr := c.block.NewGetElementPtr(instanceStructType, nullPtr,
		constant.NewInt(irtypes.I32, 1))
	sizeRaw := c.block.NewPtrToInt(sizePtr, c.ptrIntType())
	var size value.Value = sizeRaw
	if c.isWasm {
		size = c.block.NewZExt(sizeRaw, irtypes.I64)
	}

	// Allocate
	rawPtr := c.block.NewCall(c.palAlloc, size)
	typedPtr := c.block.NewBitCast(rawPtr, instancePtrType)

	// Store type info pointer in _variant slot (field 0)
	variantFieldPtr := c.block.NewGetElementPtr(instanceStructType, typedPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
	variantPtrType := layout.Instance.Fields[0].LLVMType.(*irtypes.PointerType)
	if tiGlobal := c.lookupTypeInfoGlobal(inst); tiGlobal != nil {
		tiPtr := c.block.NewBitCast(tiGlobal, variantPtrType)
		c.block.NewStore(tiPtr, variantFieldPtr)
	} else {
		c.block.NewStore(constant.NewNull(variantPtrType), variantFieldPtr)
	}

	// Zero-init all fields
	origin := inst.Origin().(*types.Named)
	for _, f := range origin.AllFields() {
		fieldIdx := layout.InstanceFieldIndex[f.Name()]
		fieldPtr := c.block.NewGetElementPtr(instanceStructType, typedPtr,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(fieldIdx)))
		c.block.NewStore(c.zeroValue(layout.Instance.Fields[fieldIdx].LLVMType), fieldPtr)
	}

	// Call new() constructor
	name := monoName(inst)
	mangledName := mangleMethodName(name, "new", false)
	fn, ok := c.funcs[mangledName]
	if !ok {
		panic(fmt.Sprintf("codegen: undeclared new() for map type %s (mangled: %s)", inst, mangledName))
	}
	c.block.NewCall(fn, typedPtr)

	// Build value struct { vtable_ptr, instance_ptr }
	var vtablePtr value.Value
	if vtableGlobal := c.lookupVtableGlobal(inst); vtableGlobal != nil {
		vtablePtr = c.block.NewBitCast(vtableGlobal, irtypes.I8Ptr)
	} else {
		vtablePtr = constant.NewNull(irtypes.I8Ptr)
	}

	var valStruct value.Value = constant.NewZeroInitializer(userValueType())
	valStruct = c.block.NewInsertValue(valStruct, vtablePtr, 0)
	valStruct = c.block.NewInsertValue(valStruct, c.block.NewBitCast(typedPtr, irtypes.I8Ptr), 1)
	return valStruct
}

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

	// Create anonymous function
	lambdaName := fmt.Sprintf(".lambda.%d", c.lambdaCounter)
	c.lambdaCounter++
	fn := c.module.NewFunc(lambdaName, retType, params...)

	// Build env struct type and capture values from the enclosing scope BEFORE switching context
	var envStructType *irtypes.StructType
	var envPtr value.Value
	if len(captures) > 0 {
		envFieldTypes := make([]irtypes.Type, len(captures))
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
			envFieldTypes[i] = captureType
			// For move captures, clear the drop flag in the enclosing scope
			if cv.ByMove {
				c.clearDropFlag(cv.Obj.Name())
			}
		}
		envStructType = irtypes.NewStruct(envFieldTypes...)

		// Allocate env struct on heap
		envSize := int64(c.typeSize(envStructType))
		rawPtr := c.block.NewCall(c.palAlloc, constant.NewInt(irtypes.I64, envSize))
		typedEnvPtr := c.block.NewBitCast(rawPtr, irtypes.NewPointer(envStructType))

		// Store captured values into env struct
		for i, val := range captureVals {
			fieldPtr := c.block.NewGetElementPtr(envStructType, typedEnvPtr,
				constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(i)))
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
	savedLoopScopeDepth := c.loopScopeDepth
	savedWritebacks := c.lambdaWritebacks

	// Generate lambda body with fresh scope state
	c.fn = fn
	c.locals = make(map[string]*ir.InstAlloca)
	c.localNameCount = make(map[string]int)
	c.blockCounter = 0
	c.canError = false
	c.currentRetType = sig.Result()
	c.scopeBindings = nil
	c.dropFlags = make(map[string]*ir.InstAlloca)
	c.dropBindings = make(map[string]scopeBinding)
	c.loopScopeDepth = 0
	c.lambdaWritebacks = nil

	entry := fn.NewBlock(".entry")
	c.block = entry
	c.entryBlock = entry

	// Load captured variables from env struct into local allocas
	if len(captures) > 0 && envStructType != nil {
		typedEnvPtr := entry.NewBitCast(fn.Params[0], irtypes.NewPointer(envStructType))
		for i, cv := range captures {
			// Use the env struct's field type — matches what was stored during capture
			captureType := envStructType.Fields[i]
			fieldPtr := entry.NewGetElementPtr(envStructType, typedEnvPtr,
				constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(i)))
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
				c.maybeRegisterDrop(cv.Obj.Name(), alloca, cv.Obj.Type())
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
		if val != nil && c.block.Term == nil {
			// Clean up capture bindings before returning
			if len(c.scopeBindings) > 0 {
				c.emitScopeCleanup(0)
			}
			c.block.NewRet(val)
		}
	}

	// Ensure terminator — clean up remaining capture bindings on fallthrough
	if c.block != nil && c.block.Term == nil {
		c.emitLambdaWritebacks()
		if len(c.scopeBindings) > 0 {
			c.emitScopeCleanup(0)
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
	c.loopScopeDepth = savedLoopScopeDepth
	c.lambdaWritebacks = savedWritebacks

	// Return fat pointer: {fn_ptr as i8*, env_ptr}
	fnPtr := c.block.NewBitCast(fn, irtypes.I8Ptr)
	var closure value.Value = constant.NewUndef(closureType())
	closure = c.block.NewInsertValue(closure, fnPtr, 0)
	closure = c.block.NewInsertValue(closure, envPtr, 1)
	return closure
}

// --- Optional Chaining ---

// genOptionalChainExpr generates x?.field — checks if the optional is present,
// accesses the field on the inner value in the some-block, returns none in the none-block.
func (c *Compiler) genOptionalChainExpr(e *ast.OptionalChainExpr) value.Value {
	optVal := c.genExpr(e.Target)

	// Extract flag (field 0)
	flag := c.block.NewExtractValue(optVal, 0)

	someBlock := c.newBlock("optchain.some")
	noneBlock := c.newBlock("optchain.none")
	mergeBlock := c.newBlock("optchain.merge")

	c.block.NewCondBr(flag, someBlock, noneBlock)

	// Some: extract inner value, access field, wrap in Optional
	c.block = someBlock
	innerVal := c.block.NewExtractValue(optVal, 1)

	// Resolve the inner type from sema
	targetType := c.info.Types[e.Target]
	if c.typeSubst != nil {
		targetType = types.Substitute(targetType, c.typeSubst)
	}
	optType := targetType.(*types.Optional)
	innerType := optType.Elem()

	// Access field on inner value
	fieldVal := c.genFieldOnValue(innerVal, innerType, e.Field)

	// Determine the result Optional type from sema
	resultType := c.info.Types[e]
	if c.typeSubst != nil {
		resultType = types.Substitute(resultType, c.typeSubst)
	}
	resultLLVM := c.resolveType(resultType).(*irtypes.StructType)

	someResult := c.wrapOptional(fieldVal, resultLLVM)
	c.block.NewBr(mergeBlock)
	someEnd := c.block

	// None: zeroinit Optional
	c.block = noneBlock
	noneResult := constant.NewZeroInitializer(resultLLVM)
	c.block.NewBr(mergeBlock)
	noneEnd := c.block

	// Merge
	c.block = mergeBlock
	return mergeBlock.NewPhi(
		&ir.Incoming{X: someResult, Pred: someEnd},
		&ir.Incoming{X: noneResult, Pred: noneEnd},
	)
}

// genFieldOnValue accesses a field or getter on a value of a known type.
// For fields on user types (i8* pointers), it does bitcast + GEP.
// For getters, it emits a direct call to the getter method.
func (c *Compiler) genFieldOnValue(val value.Value, typ types.Type, fieldName string) value.Value {
	named := extractNamed(typ)
	if named == nil {
		panic(fmt.Sprintf("codegen: cannot access field %s on type %s", fieldName, typ))
	}

	field := named.LookupField(fieldName)
	if field != nil {
		layout := c.lookupTypeLayout(typ)
		if layout == nil {
			panic(fmt.Sprintf("codegen: no layout for type %s", typ))
		}

		// Value types: fields are in the value struct
		if layout.IsValueType {
			fieldIdx, ok := layout.ValueFieldIndex[field.Name()]
			if !ok {
				panic(fmt.Sprintf("codegen: field %s not in value layout for %s", field.Name(), typ))
			}
			return c.block.NewExtractValue(val, uint64(fieldIdx))
		}

		fieldIdx, ok := layout.InstanceFieldIndex[field.Name()]
		if !ok {
			panic(fmt.Sprintf("codegen: field %s not in instance layout for %s", field.Name(), typ))
		}

		// val is a value struct {vtable_ptr, instance_ptr} — extract the instance pointer
		instance := c.extractInstancePtr(val)
		typedPtr := c.block.NewBitCast(instance, layout.InstancePtrType)
		fieldPtr := c.block.NewGetElementPtr(layout.Instance.LLVMType, typedPtr,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(fieldIdx)))

		return c.block.NewLoad(layout.Instance.Fields[fieldIdx].LLVMType, fieldPtr)
	}

	// Getter property: emit a direct call with the value as receiver
	if g := named.LookupGetter(fieldName); g != nil {
		mangledName := mangleMethodName(c.resolveTypeName(typ), fieldName, false)
		fn, ok := c.funcs[mangledName]
		if !ok {
			panic(fmt.Sprintf("codegen: undeclared getter %s", mangledName))
		}
		// Global getter: no receiver
		if g.Sig().Recv() == nil {
			return c.block.NewCall(fn)
		}
		// val is a value struct — pass it directly (getters expect the value struct as receiver)
		return c.block.NewCall(fn, val)
	}

	panic(fmt.Sprintf("codegen: no field or getter %s on type %s", fieldName, named))
}

// genIndirectCall calls a function through a fat pointer {i8* fn, i8* env}.
// Extracts the function pointer and env pointer, then calls with env as the first arg.
func (c *Compiler) genIndirectCall(closure value.Value, sig *types.Signature, args []value.Value) value.Value {
	retType := irtypes.Type(irtypes.Void)
	if sig.Result() != nil {
		retType = c.resolveType(sig.Result())
	}

	// Function type includes env (i8*) as first parameter
	paramTypes := []irtypes.Type{irtypes.I8Ptr}
	for _, p := range sig.Params() {
		paramTypes = append(paramTypes, c.resolveType(p.Type()))
	}

	funcType := irtypes.NewFunc(retType, paramTypes...)
	funcPtrType := irtypes.NewPointer(funcType)

	// Extract fn and env from fat pointer
	fnRaw := c.block.NewExtractValue(closure, 0)
	envPtr := c.block.NewExtractValue(closure, 1)

	typedFnPtr := c.block.NewBitCast(fnRaw, funcPtrType)

	// Call with env as first arg, then user args
	callArgs := make([]value.Value, 0, len(args)+1)
	callArgs = append(callArgs, envPtr)
	callArgs = append(callArgs, args...)
	return c.block.NewCall(typedFnPtr, callArgs...)
}

// getOrCreateThunk returns a trampoline function with the env-first ABI that
// forwards to the given named function. This allows named function references
// to be called through the same fat-pointer indirect call path as lambdas.
func (c *Compiler) getOrCreateThunk(fn *ir.Func, name string) *ir.Func {
	if thunk, ok := c.thunks[name]; ok {
		return thunk
	}

	// Build thunk params: env (i8*) + original function params
	params := []*ir.Param{ir.NewParam("env", irtypes.I8Ptr)}
	for _, p := range fn.Params {
		params = append(params, ir.NewParam(p.LocalName, p.Typ))
	}

	thunkName := ".thunk." + name
	thunk := c.module.NewFunc(thunkName, fn.Sig.RetType, params...)
	entry := thunk.NewBlock(".entry")

	// Forward call to original function, skipping the env param
	callArgs := make([]value.Value, len(fn.Params))
	for i := range fn.Params {
		callArgs[i] = thunk.Params[i+1]
	}

	if _, isVoid := fn.Sig.RetType.(*irtypes.VoidType); isVoid {
		entry.NewCall(fn, callArgs...)
		entry.NewRet(nil)
	} else {
		result := entry.NewCall(fn, callArgs...)
		entry.NewRet(result)
	}

	c.thunks[name] = thunk
	return thunk
}

// --- is/as expressions ---

// genIsExpr generates code for `expr is Pattern`.
func (c *Compiler) genIsExpr(e *ast.IsExpr) value.Value {
	switch p := e.Pattern.(type) {
	case *ast.IdentIsPattern:
		return c.genIsIdentPattern(e.Expr, p)
	case *ast.DestructureIsPattern:
		panic("codegen: destructure is-pattern not yet implemented")
	default:
		panic(fmt.Sprintf("codegen: unhandled is-pattern type %T", e.Pattern))
	}
}

func (c *Compiler) genIsIdentPattern(expr ast.Expr, p *ast.IdentIsPattern) value.Value {
	// Optional: x is present / x is absent
	if p.Name == "present" {
		optVal := c.genExpr(expr)
		return c.block.NewExtractValue(optVal, 0) // i1 flag field
	}
	if p.Name == "absent" {
		optVal := c.genExpr(expr)
		flag := c.block.NewExtractValue(optVal, 0)
		return c.block.NewXor(flag, constant.NewInt(irtypes.I1, 1)) // negate
	}

	// Check if the subject is an optional type — unwrap before checking inner type
	exprType := c.info.Types[expr]
	if c.typeSubst != nil {
		exprType = types.Substitute(exprType, c.typeSubst)
	}
	if opt, ok := exprType.(*types.Optional); ok {
		return c.genIsOptionalType(expr, p.Name, opt)
	}

	// Check if the subject is an enum type — use tag comparison
	if enumLayout := c.lookupEnumLayout(exprType); enumLayout != nil {
		return c.genIsEnumVariant(expr, p.Name, enumLayout)
	}

	// Named type check via RTTI
	return c.genIsNamedType(expr, p.Name)
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
	instance := c.extractInstancePtr(inner)
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

func (c *Compiler) genIsEnumVariant(expr ast.Expr, variantName string, layout *TypeDeclLayout) value.Value {
	if _, ok := layout.VariantTag[variantName]; !ok {
		panic(fmt.Sprintf("codegen: unknown enum variant %s", variantName))
	}
	subject := c.genExpr(expr)
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

	// Extract instance pointer — `this` is already i8*, others are value structs.
	// For value type `this`, extract the RTTI instance pointer (field 1) first.
	var instance value.Value
	if _, isThis := expr.(*ast.ThisExpr); isThis {
		instance = c.extractInstancePtrForThis(subject)
	} else {
		instance = c.extractInstancePtr(subject)
	}
	variantPtr := c.loadVariantPtr(instance)

	// Call promise_type_is(variant_ptr, expected_id) and convert i32 result to i1
	result := c.block.NewCall(c.funcs["promise_type_is"],
		variantPtr, constant.NewInt(irtypes.I32, int64(targetID)))
	return c.block.NewICmp(enum.IPredNE, result, constant.NewInt(irtypes.I32, 0))
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

// extractInstancePtrForThis extracts the instance/RTTI pointer from a `this` value.
// For regular types, `this` (i8*) IS the instance pointer.
// For value types, `this` (i8*) points to the value struct — field 1 is the RTTI instance pointer.
func (c *Compiler) extractInstancePtrForThis(thisVal value.Value) value.Value {
	if c.currentNamed != nil && c.currentNamed.IsValueType() {
		layout := c.lookupTypeLayout(c.currentNamed)
		if layout != nil {
			valuePtrType := irtypes.NewPointer(layout.Value.LLVMType)
			typedPtr := c.block.NewBitCast(thisVal, valuePtrType)
			rttiFieldPtr := c.block.NewGetElementPtr(layout.Value.LLVMType, typedPtr,
				constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 1))
			return c.block.NewLoad(layout.Value.Fields[1].LLVMType, rttiFieldPtr)
		}
	}
	return thisVal
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

	// Resolve the target Named type from the TypeRef
	targetRef, ok := e.Type.(*ast.NamedTypeRef)
	if !ok {
		panic(fmt.Sprintf("codegen: unsupported cast target type %T", e.Type))
	}
	targetNamed := c.lookupNamedType(targetRef.Name)
	if targetNamed == nil {
		panic(fmt.Sprintf("codegen: undefined type %s in cast", targetRef.Name))
	}

	subject := c.genExpr(e.Expr)

	// Primitive scalar casts (numeric, char, bool) — compile-time conversions, no RTTI needed
	srcType := c.info.Types[e.Expr]
	srcNamed := extractNamed(srcType)
	if srcNamed != nil && isPrimitiveScalar(srcNamed) && isPrimitiveScalar(targetNamed) {
		return c.emitScalarCast(subject, srcNamed, targetNamed)
	}

	targetID := c.assignTypeID(targetNamed)

	// Extract instance pointer — `this` is already i8*, others are value structs.
	// For value type `this`, extract the RTTI instance pointer (field 1) first.
	var instance value.Value
	if _, isThis := e.Expr.(*ast.ThisExpr); isThis {
		instance = c.extractInstancePtrForThis(subject)
	} else {
		instance = c.extractInstancePtr(subject)
	}
	variantPtr := c.loadVariantPtr(instance)

	result := c.block.NewCall(c.funcs["promise_type_is"],
		variantPtr, constant.NewInt(irtypes.I32, int64(targetID)))
	isMatch := c.block.NewICmp(enum.IPredNE, result, constant.NewInt(irtypes.I32, 0))

	if e.Force {
		// as! — panic if no match, return the value struct directly
		okBlock := c.newBlock("cast.ok")
		panicBlock := c.newBlock("cast.panic")
		c.block.NewCondBr(isMatch, okBlock, panicBlock)

		c.block = panicBlock
		panicMsg := c.makeGlobalString("cast failed: as! type mismatch")
		c.block.NewCall(c.funcs["promise_panic"], panicMsg)
		c.block.NewUnreachable()

		c.block = okBlock
		return subject // same value struct, type is verified
	}

	// as — wrap in Optional { i1, { i8*, i8* } }. User types use value struct representation.
	someBlock := c.newBlock("cast.some")
	noneBlock := c.newBlock("cast.none")
	mergeBlock := c.newBlock("cast.merge")
	c.block.NewCondBr(isMatch, someBlock, noneBlock)

	c.block = someBlock
	optType := irtypes.NewStruct(irtypes.I1, userValueType())
	someResult := c.wrapOptional(subject, optType)
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

// genOptionalHandlerExpr generates code for `optExpr ? { recovery }`.
// Checks the optional flag, runs the handler on none, extracts inner value on some.
func (c *Compiler) genOptionalHandlerExpr(e *ast.ErrorHandlerExpr) value.Value {
	optVal := c.genExpr(e.Expr)
	flag := c.block.NewExtractValue(optVal, 0)

	noneBlock := c.newBlock("opt.none")
	someBlock := c.newBlock("opt.some")
	mergeBlock := c.newBlock("opt.merge")
	c.block.NewCondBr(flag, someBlock, noneBlock)

	// None path: run handler body
	c.block = noneBlock
	handlerVal := c.genBlockValue(e.Body)
	handlerDiverged := c.block.Term != nil
	handlerEnd := c.block
	if !handlerDiverged {
		c.block.NewBr(mergeBlock)
	}

	// Some path: extract inner value
	c.block = someBlock
	okVal := c.block.NewExtractValue(optVal, 1)
	c.block.NewBr(mergeBlock)
	someEnd := c.block

	c.block = mergeBlock

	// If handler diverges, no phi needed - only the some path reaches merge
	if handlerDiverged {
		return okVal
	}

	// Both paths reach merge - phi merge the values
	if handlerVal != nil && okVal != nil {
		return c.block.NewPhi(
			&ir.Incoming{X: okVal, Pred: someEnd},
			&ir.Incoming{X: handlerVal, Pred: handlerEnd},
		)
	}
	return okVal
}

// genOptionalForceUnwrap generates code for T? → T, panicking on none.
// Used by `as!` on optionals and `x!` on optionals.
func (c *Compiler) genOptionalForceUnwrap(expr ast.Expr) value.Value {
	optVal := c.genExpr(expr)
	flag := c.block.NewExtractValue(optVal, 0)

	okBlock := c.newBlock("unwrap.ok")
	panicBlock := c.newBlock("unwrap.panic")
	c.block.NewCondBr(flag, okBlock, panicBlock)

	c.block = panicBlock
	panicMsg := c.makeGlobalString("unwrap failed: optional is none")
	c.block.NewCall(c.funcs["promise_panic"], panicMsg)
	c.block.NewUnreachable()

	c.block = okBlock
	return c.block.NewExtractValue(optVal, 1)
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

// --- Go expression (concurrency) ---

// genGoExpr generates code for a `go expr` expression.
// It creates an LLVM coroutine, wraps it in a G, and enqueues it on the M:N scheduler.
func (c *Compiler) genGoExpr(e *ast.GoExpr) value.Value {
	if e.Expr != nil {
		callExpr, ok := e.Expr.(*ast.CallExpr)
		if !ok {
			panic(fmt.Sprintf("codegen: go expression with non-call expr %T not supported", e.Expr))
		}
		return c.genGoCallExpr(callExpr)
	}
	// go { block } form
	return c.genGoBlock(e.Block)
}

// genGoCallExpr handles `go func(args...)` — the common case.
func (c *Compiler) genGoCallExpr(callExpr *ast.CallExpr) value.Value {
	// 1. Resolve result type T from sema
	callResultType := c.info.Types[callExpr]
	isVoid := (callResultType == nil || callResultType == types.TypVoid)
	var resultLLVM irtypes.Type = irtypes.Void
	if !isVoid {
		resultLLVM = c.resolveType(callResultType)
	}

	// 2. Evaluate arguments in caller scope
	var argVals []value.Value
	var argLLVMTypes []irtypes.Type
	var argTypes []types.Type
	for _, arg := range callExpr.Args {
		v := c.genCallArgExpr(arg.Value)
		argVals = append(argVals, v)
		argLLVMTypes = append(argLLVMTypes, v.Type())
		argTypes = append(argTypes, c.info.Types[arg.Value])
		if ident, ok := arg.Value.(*ast.IdentExpr); ok {
			c.clearDropFlag(ident.Name)
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
	coroName := fmt.Sprintf(".goroutine.%d", c.goCounter)
	c.goCounter++

	var coroParams []*ir.Param
	for i := range argVals {
		coroParams = append(coroParams, ir.NewParam(fmt.Sprintf("arg.%d", i), argLLVMTypes[i]))
	}
	coroFn := c.module.NewFunc(coroName, irtypes.I8Ptr, coroParams...)
	coroFn.FuncAttrs = append(coroFn.FuncAttrs, rawFuncAttr("presplitcoroutine"))

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

	if !isVoid {
		result := bodyBlk.NewCall(targetFn, callArgs...)
		// Store result via G.result_ptr (set by caller before enqueue)
		gTy := goroutineStructType()
		currentG := bodyBlk.NewLoad(irtypes.I8Ptr, c.currentGGlobal)
		gPtr := bodyBlk.NewBitCast(currentG, irtypes.NewPointer(gTy))
		rpField := bodyBlk.NewGetElementPtr(gTy, gPtr,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(gFieldResultPtr)))
		rpVal := bodyBlk.NewLoad(irtypes.I8Ptr, rpField)
		typedRP := bodyBlk.NewBitCast(rpVal, irtypes.NewPointer(resultLLVM))
		bodyBlk.NewStore(result, typedRP)
	} else {
		bodyBlk.NewCall(targetFn, callArgs...)
	}

	// Final suspend: yield back to scheduler so it can see coro.done()=true
	// before destroying the coroutine frame.
	finalSuspBlk := coroFn.NewBlock("final.suspend")
	bodyBlk.NewBr(finalSuspBlk)

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

	{
		gTy := goroutineStructType()
		gPtr := c.block.NewBitCast(gRaw, irtypes.NewPointer(gTy))
		rpField := c.block.NewGetElementPtr(gTy, gPtr,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(gFieldResultPtr)))
		if !isVoid {
			// Allocate result buffer and store in G.result_ptr
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

// genGoExternWrapper generates a thin wrapper function around an extern call
// for use in go expressions. The wrapper takes Promise-internal argument types
// and returns the Promise-internal result type, handling sret/ABI coercion
// internally via genExternCall. This is needed because extern IR functions use
// void return + sret pointer for struct returns, which is incompatible with
// the coroutine body's direct call + store pattern (B0046).
func (c *Compiler) genGoExternWrapper(ext *ExternFunc, argLLVMTypes []irtypes.Type, argTypes []types.Type, resultLLVM irtypes.Type, isVoid bool) *ir.Func {
	wrapName := fmt.Sprintf(".go_extern_wrap.%s.%d", ext.PromiseName, c.goCounter)

	var params []*ir.Param
	for i, ty := range argLLVMTypes {
		params = append(params, ir.NewParam(fmt.Sprintf("arg.%d", i), ty))
	}

	retType := irtypes.Type(irtypes.Void)
	if !isVoid {
		retType = resultLLVM
	}
	wrapFn := c.module.NewFunc(wrapName, retType, params...)

	saved := c.saveState()
	defer c.restoreState(saved)

	c.fn = wrapFn
	entry := wrapFn.NewBlock("entry")
	c.block = entry
	c.entryBlock = entry
	c.locals = make(map[string]*ir.InstAlloca)
	c.localNameCount = make(map[string]int)
	c.dropFlags = make(map[string]*ir.InstAlloca)
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
// Returns a sorted, deduplicated list of names that exist in outerLocals.
func collectBlockIdents(block *ast.Block, outerLocals map[string]*ir.InstAlloca) []string {
	seen := make(map[string]bool)
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
		case *ast.ErrorUnwrapExpr:
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
			// Lambda captures are handled separately; skip inner references
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
	return names
}

// genGoBlock handles `go { block }` — wraps the block in a void function and spawns it.
// Captures outer local variables referenced in the block and passes them through the arg pack.
func (c *Compiler) genGoBlock(block *ast.Block) value.Value {
	// Collect outer variables referenced in the block
	captureNames := collectBlockIdents(block, c.locals)

	// Load captured values and collect their types BEFORE switching context
	var captureVals []value.Value
	var captureLLVMTypes []irtypes.Type
	for _, name := range captureNames {
		alloca := c.locals[name]
		elemType := alloca.ElemType
		val := c.block.NewLoad(elemType, alloca)
		captureVals = append(captureVals, val)
		captureLLVMTypes = append(captureLLVMTypes, elemType)
	}

	// Create coroutine function with captured values as parameters
	coroName := fmt.Sprintf(".goroutine.%d", c.goCounter)
	c.goCounter++
	var coroParams []*ir.Param
	for i, name := range captureNames {
		coroParams = append(coroParams, ir.NewParam(name+".cap", captureLLVMTypes[i]))
	}
	coroFn := c.module.NewFunc(coroName, irtypes.I8Ptr, coroParams...)
	coroFn.FuncAttrs = append(coroFn.FuncAttrs, rawFuncAttr("presplitcoroutine"))

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
	savedLoopScopeDepth := c.loopScopeDepth
	savedInCoroutine := c.inCoroutine
	savedCoroCleanup := c.coroCleanupBlk
	savedCoroSuspend := c.coroSuspendBlk

	c.fn = coroFn
	c.locals = make(map[string]*ir.InstAlloca)
	c.localNameCount = make(map[string]int)
	c.blockCounter = 0
	c.canError = false
	c.currentRetType = types.TypVoid
	c.scopeBindings = nil
	c.dropFlags = make(map[string]*ir.InstAlloca)
	c.dropBindings = make(map[string]scopeBinding)
	c.loopScopeDepth = 0
	c.inCoroutine = true

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
	for i, name := range captureNames {
		alloca := startBlk.NewAlloca(captureLLVMTypes[i])
		alloca.SetName(c.uniqueLocalName(name + ".addr"))
		startBlk.NewStore(coroFn.Params[i], alloca)
		c.locals[name] = alloca
	}

	// Initial suspend — wait to be scheduled
	initResult := startBlk.NewCall(c.coroSuspend, constant.None, constant.False)

	suspendBlk := coroFn.NewBlock("coro.suspend")
	bodyBlk := coroFn.NewBlock("body")
	cleanupBlk := coroFn.NewBlock("cleanup")
	// Create doneBlk early so intermediate coro.suspend switches can reference it.
	// Instructions are added after the body is compiled.
	doneBlk := coroFn.NewBlock("coro.done")

	startBlk.NewSwitch(initResult, suspendBlk,
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
	c.genBlock(block)

	// Final suspend: yield back to scheduler so it can see coro.done()=true
	// before destroying the coroutine frame.
	finalSuspBlk := coroFn.NewBlock("final.suspend")
	if c.block != nil && c.block.Term == nil {
		c.block.NewBr(finalSuspBlk)
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
	c.loopScopeDepth = savedLoopScopeDepth
	c.inCoroutine = savedInCoroutine
	c.coroCleanupBlk = savedCoroCleanup
	c.coroSuspendBlk = savedCoroSuspend

	// Caller: call coroutine ramp → get handle, create G, enqueue
	handle := c.block.NewCall(coroFn, captureVals...)
	gRaw := c.block.NewCall(c.funcs["promise_g_new"], handle)

	// Set result_ptr to sentinel (0x1) so goroutine_exit knows this is a task
	// and won't free G (caller frees via <-task)
	gTy := goroutineStructType()
	gPtr := c.block.NewBitCast(gRaw, irtypes.NewPointer(gTy))
	rpField := c.block.NewGetElementPtr(gTy, gPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(gFieldResultPtr)))
	sentinel := c.block.NewIntToPtr(constant.NewInt(c.ptrIntType(), 1), irtypes.I8Ptr)
	c.block.NewStore(sentinel, rpField)

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
	return c.genReceiveTask(e, inst)
}

// genReceiveTask generates code for `<-task` — waits for goroutine G to complete, returns T.
// The task handle is now a G pointer (i8*). Checks G.done and loads from G.result_ptr.
func (c *Compiler) genReceiveTask(e *ast.UnaryExpr, inst *types.Instance) value.Value {
	gRaw := c.genExpr(e.Operand)

	var innerType types.Type
	if len(inst.TypeArgs()) > 0 {
		innerType = inst.TypeArgs()[0]
	}
	isVoid := (innerType == nil || innerType == types.TypVoid)

	var resultLLVM irtypes.Type = irtypes.Void
	if !isVoid {
		resultLLVM = c.resolveType(innerType)
	}

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
		// Goroutine-mode: use sched.done_lock to protect done + done_waiters
		// atomically. Hold the lock across coro.suspend via G.park_mutex so
		// the scheduler releases it after suspend completes — this prevents
		// the enqueue-before-suspend race.
		currentG := c.block.NewLoad(irtypes.I8Ptr, c.currentGGlobal)
		currentGPtr := c.block.NewBitCast(currentG, irtypes.NewPointer(gTy))

		// Load and lock sched.done_lock
		schedTy := schedStructType()
		doneLockField := c.block.NewGetElementPtr(schedTy, c.schedGlobal,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(schedFieldDoneLock)))
		doneLock := c.block.NewLoad(irtypes.I8Ptr, doneLockField)
		c.block.NewCall(c.palMutexLock, doneLock)

		// Re-check G.done under lock
		recheckDone := c.block.NewLoad(irtypes.I8, doneField)
		recheckIsDone := c.block.NewICmp(enum.IPredNE, recheckDone, constant.NewInt(irtypes.I8, 0))
		doneUnderLockBlk := c.newBlock("task.done_under_lock")
		parkBlk := c.newBlock("task.park")
		c.block.NewCondBr(recheckIsDone, doneUnderLockBlk, parkBlk)

		// task.done_under_lock: target already done — unlock and proceed
		c.block = doneUnderLockBlk
		c.block.NewCall(c.palMutexUnlock, doneLock)
		c.block.NewBr(readyBlk)

		// task.park: set status = waiting, prepend to done_waiters, park_mutex = done_lock, suspend
		c.block = parkBlk
		curStatusField := c.block.NewGetElementPtr(gTy, currentGPtr,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(gFieldStatus)))
		c.block.NewStore(constant.NewInt(irtypes.I8, gStatusWaiting), curStatusField)

		// Prepend current G to target G's done_waiters list
		dwField := c.block.NewGetElementPtr(gTy, gPtr,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(gFieldDoneWaiters)))
		oldHead := c.block.NewLoad(irtypes.I8Ptr, dwField)
		curWaitNextField := c.block.NewGetElementPtr(gTy, currentGPtr,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(gFieldWaitNext)))
		c.block.NewStore(oldHead, curWaitNextField)
		c.block.NewStore(currentG, dwField)

		// Store done_lock as park_mutex — scheduler will release after suspend
		pmField := c.block.NewGetElementPtr(gTy, currentGPtr,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(gFieldParkMutex)))
		c.block.NewStore(doneLock, pmField)

		// Suspend (lock held — scheduler releases it)
		suspResult := c.block.NewCall(c.coroSuspend, constant.None, constant.False)
		resumeBlk := c.newBlock("task.resume")
		c.block.NewSwitch(suspResult, c.coroSuspendBlk,
			ir.NewCase(constant.NewInt(irtypes.I8, 0), resumeBlk),
			ir.NewCase(constant.NewInt(irtypes.I8, 1), c.coroCleanupBlk))
		resumeBlk.NewBr(readyBlk)
	} else {
		// Thread-blocking mode: poll G.done in a loop.
		// goroutine_exit sets G.done = 1 atomically; we just spin until we see it.
		// A brief usleep(100) avoids burning CPU in a tight loop.
		checkBlk := c.newBlock("task.check")
		spinBlk := c.newBlock("task.spin")
		doneBlk := c.newBlock("task.threaddone")

		c.block.NewBr(checkBlk)

		// check: reload done flag
		c.block = checkBlk
		doneVal2 := c.block.NewLoad(irtypes.I8, doneField)
		isDone2 := c.block.NewICmp(enum.IPredNE, doneVal2, constant.NewInt(irtypes.I8, 0))
		c.block.NewCondBr(isDone2, doneBlk, spinBlk)

		// spin: brief sleep then recheck
		c.block = spinBlk
		c.block.NewCall(c.palUsleep, constant.NewInt(irtypes.I32, 100))
		c.block.NewBr(checkBlk)

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
	c.block.NewCall(c.funcs["promise_panic"], panicMsg)
	c.block.NewUnreachable()

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

	if isVoid {
		return nil
	}
	return resultVal
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
	} else {
		// Thread-blocking mode: cond_wait, loop
		c.block = waitBodyBlock
		c.block.NewCall(c.palCondWait, notEmpty, mtx)
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

	// Read value via alloca + memcpy
	resultAlloca := c.block.NewAlloca(elemLLVM)
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
