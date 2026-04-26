package codegen

import (
	"fmt"
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
func (c *Compiler) makeRuntimeString(s string) value.Value {
	data := constant.NewCharArrayFromString(s)
	globalName := fmt.Sprintf(".str.%d", c.strCounter)
	c.strCounter++
	global := c.module.NewGlobalDef(globalName, data)
	global.Immutable = true

	ptr := c.block.NewGetElementPtr(global.ContentType, global,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))

	return c.block.NewCall(c.funcs["promise_string_new"],
		ptr, constant.NewInt(irtypes.I64, int64(len(s))))
}

// convertToString converts a value to a string (i8*) for interpolation.
func (c *Compiler) convertToString(val value.Value, typ types.Type) value.Value {
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
		return c.block.NewCall(c.funcs["promise_int_to_string"], val)
	case types.TypU32, types.TypU16, types.TypU8:
		ext := c.block.NewZExt(val, irtypes.I64)
		return c.block.NewCall(c.funcs["promise_int_to_string"], ext)
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
		panic(fmt.Sprintf("codegen: cannot convert %s to string for interpolation", typ))
	}
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
	// Check for function reference
	if fn, ok := c.funcs[e.Name]; ok {
		return fn
	}
	// Local variable: load from alloca
	alloca, ok := c.locals[e.Name]
	if !ok {
		panic(fmt.Sprintf("codegen: undefined variable %q", e.Name))
	}
	return c.block.NewLoad(alloca.ElemType, alloca)
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

	// Non-native method call (future stages)
	panic(fmt.Sprintf("codegen: non-native operator %s.%s not yet implemented", named, op))
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
	default:
		panic(fmt.Sprintf("codegen: string operator %q not yet implemented", op))
	}
}

// --- Unary expressions ---

func (c *Compiler) genUnaryExpr(e *ast.UnaryExpr) value.Value {
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

// --- Range construction ---

// genRange constructs a Range struct { i64 start, i64 end, i1 inclusive }.
// For for-in loops, the struct fields are extracted by the loop codegen.
func (c *Compiler) genRange(e *ast.BinaryExpr) value.Value {
	start := c.genExpr(e.Left)
	end := c.genExpr(e.Right)
	inclusive := constant.NewInt(irtypes.I1, 0)
	if e.Op == ast.BinInclusiveRange {
		inclusive = constant.NewInt(irtypes.I1, 1)
	}

	// Pack into a Range struct: { i64, i64, i1 }
	rangeType := c.rangeStructType()
	alloca := c.block.NewAlloca(rangeType)
	startPtr := c.block.NewGetElementPtr(rangeType, alloca,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
	c.block.NewStore(start, startPtr)
	endPtr := c.block.NewGetElementPtr(rangeType, alloca,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 1))
	c.block.NewStore(end, endPtr)
	inclPtr := c.block.NewGetElementPtr(rangeType, alloca,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 2))
	c.block.NewStore(inclusive, inclPtr)

	return c.block.NewLoad(rangeType, alloca)
}

// rangeStructType returns the LLVM struct type for Range: { i64, i64, i1 }.
func (c *Compiler) rangeStructType() *irtypes.StructType {
	return irtypes.NewStruct(irtypes.I64, irtypes.I64, irtypes.I1)
}

// --- Call expressions ---

func (c *Compiler) genCallExpr(e *ast.CallExpr) value.Value {
	// Method call or enum variant constructor: callee is MemberExpr
	if member, ok := e.Callee.(*ast.MemberExpr); ok {
		targetType := c.info.Types[member.Target]
		// Apply typeSubst for mono context
		if c.typeSubst != nil {
			targetType = types.Substitute(targetType, c.typeSubst)
		}
		if enumLayout := c.lookupEnumLayout(targetType); enumLayout != nil {
			return c.genEnumVariantCallLayout(e, member, enumLayout)
		}
		return c.genMethodCall(e, member)
	}

	// Constructor call: callee resolves to a Named type or Instance
	calleeType := c.info.Types[e.Callee]
	if inst, ok := calleeType.(*types.Instance); ok {
		if _, ok := inst.Origin().(*types.Named); ok {
			return c.genConstructorCallMono(e, calleeType)
		}
	}
	if named, ok := calleeType.(*types.Named); ok {
		if _, isIdent := e.Callee.(*ast.IdentExpr); isIdent {
			return c.genConstructorCallMono(e, named)
		}
	}

	// Generic function call: callee is IndexExpr (identity[int](42))
	if idx, ok := e.Callee.(*ast.IndexExpr); ok {
		return c.genGenericFuncCall(e, idx)
	}

	// Evaluate arguments
	var argVals []value.Value
	var argTypes []types.Type
	for _, arg := range e.Args {
		argVals = append(argVals, c.genExpr(arg.Value))
		argTypes = append(argTypes, c.info.Types[arg.Value])
	}

	// Resolve callee
	ident, ok := e.Callee.(*ast.IdentExpr)
	if !ok {
		panic(fmt.Sprintf("codegen: unsupported callee type %T", e.Callee))
	}

	// Lambda call: callee is a local variable holding a function pointer (i8*)
	if alloca, ok := c.locals[ident.Name]; ok {
		calleeType := c.info.Types[e.Callee]
		if sig, ok := calleeType.(*types.Signature); ok {
			fnPtr := c.block.NewLoad(alloca.ElemType, alloca)
			return c.genIndirectCall(fnPtr, sig, argVals)
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

	return c.block.NewCall(fn, argVals...)
}

// genGenericFuncCall generates a call to a monomorphic generic function instance.
func (c *Compiler) genGenericFuncCall(e *ast.CallExpr, idx *ast.IndexExpr) value.Value {
	// Resolve the type argument to build the mangled name
	typeArgType := c.info.Types[idx.Index]
	// Apply typeSubst so generic-in-generic calls resolve correctly
	if c.typeSubst != nil && typeArgType != nil {
		typeArgType = types.Substitute(typeArgType, c.typeSubst)
	}

	ident, ok := idx.Target.(*ast.IdentExpr)
	if !ok {
		panic(fmt.Sprintf("codegen: generic function target is not IdentExpr: %T", idx.Target))
	}

	mangledName := ident.Name + "__" + typeArgSuffix(typeArgType)

	fn, ok := c.funcs[mangledName]
	if !ok {
		panic(fmt.Sprintf("codegen: undefined monomorphic function %q", mangledName))
	}

	var argVals []value.Value
	for _, arg := range e.Args {
		argVals = append(argVals, c.genExpr(arg.Value))
	}

	return c.block.NewCall(fn, argVals...)
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

	instanceStructType := layout.Instance.LLVMType
	instancePtrType := layout.InstancePtrType

	// Compute size via GEP-from-null trick
	nullPtr := constant.NewNull(instancePtrType)
	sizePtr := c.block.NewGetElementPtr(instanceStructType, nullPtr,
		constant.NewInt(irtypes.I32, 1))
	size := c.block.NewPtrToInt(sizePtr, irtypes.I64)

	// Allocate
	rawPtr := c.block.NewCall(c.funcs["malloc"], size)
	typedPtr := c.block.NewBitCast(rawPtr, instancePtrType)

	// Store type info pointer in _variant slot (field 0) for RTTI
	variantFieldPtr := c.block.NewGetElementPtr(instanceStructType, typedPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
	variantPtrType := layout.Instance.Fields[0].LLVMType.(*irtypes.PointerType)
	if named != nil {
		if tiGlobal, ok := c.typeInfoGlobals[named]; ok {
			tiPtr := c.block.NewBitCast(tiGlobal, variantPtrType)
			c.block.NewStore(tiPtr, variantFieldPtr)
		} else {
			c.block.NewStore(constant.NewNull(variantPtrType), variantFieldPtr)
		}
	} else {
		c.block.NewStore(constant.NewNull(variantPtrType), variantFieldPtr)
	}

	// Build set of provided field names
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
		val := c.genExpr(arg.Value)
		fieldPtr := c.block.NewGetElementPtr(instanceStructType, typedPtr,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(fieldIdx)))
		c.block.NewStore(val, fieldPtr)
	}

	// Zero-initialize any fields not provided — use layout field types (not llvmType(f.Type()))
	for _, f := range named.AllFields() {
		if provided[f.Name()] {
			continue
		}
		fieldIdx := layout.InstanceFieldIndex[f.Name()]
		fieldPtr := c.block.NewGetElementPtr(instanceStructType, typedPtr,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(fieldIdx)))
		c.block.NewStore(c.zeroValue(layout.Instance.Fields[fieldIdx].LLVMType), fieldPtr)
	}

	return rawPtr
}

// --- Member access ---

// genMemberExpr generates a field access on a user type instance or an enum variant value.
func (c *Compiler) genMemberExpr(e *ast.MemberExpr) value.Value {
	targetType := c.info.Types[e.Target]
	// Apply typeSubst for mono context
	if c.typeSubst != nil {
		targetType = types.Substitute(targetType, c.typeSubst)
	}

	// Container .len property (string, slice, array, map)
	if e.Field == "len" {
		if named := extractNamed(targetType); named == types.TypString {
			return c.genStringLen(e)
		}
		switch targetType.(type) {
		case *types.Slice, *types.Array:
			return c.genSliceLen(e)
		case *types.Map:
			return c.genMapLen(e)
		}
	}

	// Enum variant access: Color.Red or Option[int].None
	if enumLayout := c.lookupEnumLayout(targetType); enumLayout != nil {
		return c.genEnumVariantValueLayout(enumLayout, e.Field)
	}

	named := extractNamed(targetType)
	if named == nil {
		panic(fmt.Sprintf("codegen: cannot resolve type for member access on %T", targetType))
	}

	field := named.LookupField(e.Field)
	if field != nil {
		return c.genFieldAccess(e, targetType, field)
	}

	panic(fmt.Sprintf("codegen: member %s on type %s is not a field (method references not yet supported)", e.Field, named))
}

// genSliceLen loads the length from a slice/array header.
func (c *Compiler) genSliceLen(e *ast.MemberExpr) value.Value {
	slicePtr := c.genExpr(e.Target)
	headerType := sliceHeaderType()
	headerPtr := c.block.NewBitCast(slicePtr, irtypes.NewPointer(headerType))
	lenPtr := c.block.NewGetElementPtr(headerType, headerPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
	return c.block.NewLoad(irtypes.I64, lenPtr)
}

// genMapLen returns the length of a map via the runtime.
func (c *Compiler) genMapLen(e *ast.MemberExpr) value.Value {
	mapPtr := c.genExpr(e.Target)
	return c.block.NewCall(c.funcs["promise_map_len"], mapPtr)
}

// genFieldAccess loads a field value from a user type instance.
// Uses lookupTypeLayout for layout-driven field types that work for both
// regular and monomorphic types.
func (c *Compiler) genFieldAccess(e *ast.MemberExpr, typ types.Type, field *types.Field) value.Value {
	layout := c.lookupTypeLayout(typ)
	if layout == nil {
		panic(fmt.Sprintf("codegen: no layout for type %s", typ))
	}

	fieldIdx, ok := layout.InstanceFieldIndex[field.Name()]
	if !ok {
		panic(fmt.Sprintf("codegen: field %s not in instance layout for %s", field.Name(), typ))
	}

	target := c.genExpr(e.Target)
	typedPtr := c.block.NewBitCast(target, layout.InstancePtrType)

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
	named := extractNamed(targetType)
	if named == nil {
		panic(fmt.Sprintf("codegen: cannot resolve type for method call on %T", targetType))
	}

	method := named.LookupMethod(member.Field)
	if method == nil {
		panic(fmt.Sprintf("codegen: no method %s on type %s", member.Field, named))
	}

	// Resolve method mangling: for inherited methods, find the defining type.
	// For mono/generic types, use resolveTypeName (handles Instance → mono name).
	// For regular Named types with inheritance, use resolveMethodOwner to find
	// the parent that actually defines the method.
	var mangledName string
	ownerName := c.resolveMethodOwner(named, member.Field)
	if ownerName != named.Obj().Name() {
		// Inherited from a parent — use parent's name directly
		mangledName = ownerName + "." + member.Field
	} else {
		// Defined on this type — use resolveTypeName for mono awareness
		mangledName = c.resolveTypeName(targetType) + "." + member.Field
	}

	fn, ok := c.funcs[mangledName]
	if !ok {
		panic(fmt.Sprintf("codegen: undeclared method %s", mangledName))
	}

	// Build args: receiver first (if method has one), then regular args
	var args []value.Value
	if method.Sig().Recv() != nil {
		target := c.genExpr(member.Target)
		args = append(args, target)
	}
	for _, arg := range e.Args {
		args = append(args, c.genExpr(arg.Value))
	}

	return c.block.NewCall(fn, args...)
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
			val := c.genExpr(arg.Value)
			fieldPtr := c.block.NewGetElementPtr(dataType, typedDataPtr,
				constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(i)))
			c.block.NewStore(val, fieldPtr)
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
	var incomings []*ir.Incoming

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
			c.genBlock(arm.Block)
		}

		armEnd := c.block
		if c.block.Term == nil {
			c.block.NewBr(mergeBlock)
		}

		if armVal != nil {
			incomings = append(incomings, &ir.Incoming{X: armVal, Pred: armEnd})
		}
	}

	if defaultTarget == nil {
		defaultTarget = mergeBlock
	}

	switchBlock.NewSwitch(tag, defaultTarget, cases...)

	c.block = mergeBlock
	if len(incomings) > 0 {
		return mergeBlock.NewPhi(incomings...)
	}
	return nil
}

// genValueMatch generates a match expression on a non-enum value using comparison chains.
func (c *Compiler) genValueMatch(e *ast.MatchExpr, subject value.Value, subjectType types.Type) value.Value {
	mergeBlock := c.newBlock("match.end")
	var incomings []*ir.Incoming

	named := extractNamed(subjectType)

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
				c.genBlock(arm.Block)
			}
			armEnd := c.block
			if c.block.Term == nil {
				c.block.NewBr(mergeBlock)
			}
			if armVal != nil {
				incomings = append(incomings, &ir.Incoming{X: armVal, Pred: armEnd})
			}

			c.block = nextBlock

		case *ast.WildcardMatchPattern, *ast.NameMatchPattern:
			// Default arm: always matches
			armBlock := c.newBlock(fmt.Sprintf("match.arm%d", i))
			c.block.NewBr(armBlock)

			c.block = armBlock
			if np, ok := p.(*ast.NameMatchPattern); ok && np.Name != "_" {
				lt := subject.Type()
				alloca := c.block.NewAlloca(lt)
				alloca.SetName(np.Name)
				c.block.NewStore(subject, alloca)
				c.locals[np.Name] = alloca
			}

			var armVal value.Value
			if arm.Body != nil {
				armVal = c.genExpr(arm.Body)
			} else if arm.Block != nil {
				c.genBlock(arm.Block)
			}
			armEnd := c.block
			if c.block.Term == nil {
				c.block.NewBr(mergeBlock)
			}
			if armVal != nil {
				incomings = append(incomings, &ir.Incoming{X: armVal, Pred: armEnd})
			}

			// After a wildcard/name pattern, no more arms need checking
			c.block = mergeBlock
			if len(incomings) > 0 {
				return mergeBlock.NewPhi(incomings...)
			}
			return nil
		}
	}

	// If we fell through without a default, branch to merge
	if c.block.Term == nil {
		c.block.NewBr(mergeBlock)
	}

	c.block = mergeBlock
	if len(incomings) > 0 {
		return mergeBlock.NewPhi(incomings...)
	}
	return nil
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
			alloca := c.block.NewAlloca(lt)
			alloca.SetName(p.Name)
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
	alloca := c.block.NewAlloca(internalType)
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

		bindAlloca := c.block.NewAlloca(fieldType)
		bindAlloca.SetName(binding)
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
	c.genBlock(e.Then)
	var thenVal value.Value
	if len(e.Then.Stmts) > 0 {
		if es, ok := e.Then.Stmts[len(e.Then.Stmts)-1].(*ast.ExprStmt); ok {
			thenVal = c.genExpr(es.Expr)
		}
	}
	thenEnd := c.block
	if c.block.Term == nil {
		c.block.NewBr(mergeBlock)
	}

	// Else branch
	c.block = elseBlock
	c.genBlock(e.Else)
	var elseVal value.Value
	if len(e.Else.Stmts) > 0 {
		if es, ok := e.Else.Stmts[len(e.Else.Stmts)-1].(*ast.ExprStmt); ok {
			elseVal = c.genExpr(es.Expr)
		}
	}
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

	// Error path: extract error, wrap in caller's result type, early return
	c.block = propagateBlock
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
func (c *Compiler) genErrorUnwrapExpr(e *ast.ErrorUnwrapExpr) value.Value {
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
func (c *Compiler) genErrorHandlerExpr(e *ast.ErrorHandlerExpr) value.Value {
	result := c.genExpr(e.Expr)
	resultType := result.Type().(*irtypes.StructType)

	tag := c.block.NewExtractValue(result, 0)

	handlerBlock := c.newBlock("error.handler")
	okBlock := c.newBlock("error.ok")
	mergeBlock := c.newBlock("error.merge")
	c.block.NewCondBr(tag, handlerBlock, okBlock)

	// Handler block: bind error variable, generate body
	c.block = handlerBlock
	if e.Binding != "" && e.Binding != "_" {
		errVal := c.block.NewExtractValue(result, resultErrIdx(resultType))
		alloca := c.block.NewAlloca(irtypes.I8Ptr)
		alloca.SetName(e.Binding)
		c.block.NewStore(errVal, alloca)
		c.locals[e.Binding] = alloca
	}
	c.genBlock(e.Body)
	var handlerVal value.Value
	if len(e.Body.Stmts) > 0 {
		if es, ok := e.Body.Stmts[len(e.Body.Stmts)-1].(*ast.ExprStmt); ok {
			handlerVal = c.genExpr(es.Expr)
		}
	}
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
	c.block.NewBr(mergeBlock)
	okEnd := c.block

	// Merge with phi if both paths produce values
	c.block = mergeBlock
	if okVal != nil && handlerVal != nil {
		return mergeBlock.NewPhi(
			&ir.Incoming{X: okVal, Pred: okEnd},
			&ir.Incoming{X: handlerVal, Pred: handlerEnd},
		)
	}
	return okVal
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

// --- Slice / Array Literal ---

const sliceHeaderSize = 16

func sliceHeaderType() *irtypes.StructType {
	return irtypes.NewStruct(irtypes.I64, irtypes.I64)
}

func (c *Compiler) genArrayLit(e *ast.ArrayLit) value.Value {
	typ := c.info.Types[e]
	if c.typeSubst != nil {
		typ = types.Substitute(typ, c.typeSubst)
	}
	sliceType, ok := typ.(*types.Slice)
	if !ok {
		panic(fmt.Sprintf("codegen: array literal type is %T, want *types.Slice", typ))
	}
	elemLLVM := c.resolveType(sliceType.Elem())
	elemSize := int64(llvmTypeSize(elemLLVM))
	n := int64(len(e.Elements))

	// Total allocation: header (16 bytes) + n * elemSize
	totalSize := int64(sliceHeaderSize) + n*elemSize

	// malloc
	rawPtr := c.block.NewCall(c.funcs["malloc"],
		constant.NewInt(irtypes.I64, totalSize))

	// Store len and cap via header GEP
	headerType := sliceHeaderType()
	headerPtr := c.block.NewBitCast(rawPtr, irtypes.NewPointer(headerType))
	lenPtr := c.block.NewGetElementPtr(headerType, headerPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
	c.block.NewStore(constant.NewInt(irtypes.I64, n), lenPtr)

	capPtr := c.block.NewGetElementPtr(headerType, headerPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 1))
	c.block.NewStore(constant.NewInt(irtypes.I64, n), capPtr)

	// Store elements: ptr + 16 bytes (header), then index by element type
	dataBase := c.block.NewGetElementPtr(irtypes.I8, rawPtr,
		constant.NewInt(irtypes.I64, int64(sliceHeaderSize)))
	dataTypedPtr := c.block.NewBitCast(dataBase, irtypes.NewPointer(elemLLVM))

	for i, elemExpr := range e.Elements {
		val := c.genExpr(elemExpr)
		elemPtr := c.block.NewGetElementPtr(elemLLVM, dataTypedPtr,
			constant.NewInt(irtypes.I64, int64(i)))
		c.block.NewStore(val, elemPtr)
	}

	return rawPtr // i8*
}

// --- Index Expression ---

func (c *Compiler) genIndexExpr(e *ast.IndexExpr) value.Value {
	targetType := c.info.Types[e.Target]
	if c.typeSubst != nil {
		targetType = types.Substitute(targetType, c.typeSubst)
	}

	switch t := targetType.(type) {
	case *types.Slice:
		return c.genSliceIndex(e, t)
	case *types.Array:
		return c.genSliceIndex(e, types.NewSlice(t.Elem()))
	case *types.Map:
		return c.genMapIndex(e, t)
	default:
		panic(fmt.Sprintf("codegen: cannot index type %s", targetType))
	}
}

func (c *Compiler) genSliceIndex(e *ast.IndexExpr, sliceType *types.Slice) value.Value {
	slicePtr := c.genExpr(e.Target)
	idx := c.genExpr(e.Index)
	elemLLVM := c.resolveType(sliceType.Elem())

	// Bounds check: load len, compare index
	headerType := sliceHeaderType()
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
		constant.NewInt(irtypes.I64, int64(sliceHeaderSize)))
	dataTypedPtr := c.block.NewBitCast(dataBase, irtypes.NewPointer(elemLLVM))
	elemPtr := c.block.NewGetElementPtr(elemLLVM, dataTypedPtr, idx)
	return c.block.NewLoad(elemLLVM, elemPtr)
}

// makeGlobalString creates a global null-terminated string constant and returns an i8* to it.
func (c *Compiler) makeGlobalString(s string) value.Value {
	data := constant.NewCharArrayFromString(s + "\x00")
	globalName := fmt.Sprintf(".str.%d", c.strCounter)
	c.strCounter++
	global := c.module.NewGlobalDef(globalName, data)
	global.Immutable = true
	return c.block.NewGetElementPtr(global.ContentType, global,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
}

// --- Map ---

func (c *Compiler) genMapLit(e *ast.MapLit) value.Value {
	typ := c.info.Types[e]
	if c.typeSubst != nil {
		typ = types.Substitute(typ, c.typeSubst)
	}
	mapType, ok := typ.(*types.Map)
	if !ok {
		panic(fmt.Sprintf("codegen: map literal type is %T, want *types.Map", typ))
	}
	keyLLVM := c.resolveType(mapType.Key())
	valLLVM := c.resolveType(mapType.Val())
	keySize := int64(llvmTypeSize(keyLLVM))
	valSize := int64(llvmTypeSize(valLLVM))

	// Determine hash/eq function pointers (string keys need content-based comparison)
	var hashFn, eqFn value.Value
	if extractNamed(mapType.Key()) == types.TypString {
		hashFn = c.block.NewBitCast(c.funcs["promise_hash_string"], irtypes.I8Ptr)
		eqFn = c.block.NewBitCast(c.funcs["promise_eq_string"], irtypes.I8Ptr)
	} else {
		hashFn = constant.NewNull(irtypes.I8Ptr)
		eqFn = constant.NewNull(irtypes.I8Ptr)
	}

	// Create map
	mapPtr := c.block.NewCall(c.funcs["promise_map_new"],
		constant.NewInt(irtypes.I64, keySize),
		constant.NewInt(irtypes.I64, valSize),
		hashFn, eqFn)

	// Insert entries
	for _, entry := range e.Entries {
		keyVal := c.genExpr(entry.Key)
		valVal := c.genExpr(entry.Value)

		keyAlloca := c.block.NewAlloca(keyLLVM)
		c.block.NewStore(keyVal, keyAlloca)
		keyPtr := c.block.NewBitCast(keyAlloca, irtypes.I8Ptr)

		valAlloca := c.block.NewAlloca(valLLVM)
		c.block.NewStore(valVal, valAlloca)
		valPtr := c.block.NewBitCast(valAlloca, irtypes.I8Ptr)

		c.block.NewCall(c.funcs["promise_map_set"], mapPtr, keyPtr, valPtr)
	}

	return mapPtr
}

func (c *Compiler) genMapIndex(e *ast.IndexExpr, mapType *types.Map) value.Value {
	mapPtr := c.genExpr(e.Target)
	keyVal := c.genExpr(e.Index)
	keyLLVM := c.resolveType(mapType.Key())
	valLLVM := c.resolveType(mapType.Val())

	// Alloca key for type-erased call
	keyAlloca := c.block.NewAlloca(keyLLVM)
	c.block.NewStore(keyVal, keyAlloca)
	keyPtr := c.block.NewBitCast(keyAlloca, irtypes.I8Ptr)

	// Call promise_map_get
	resultPtr := c.block.NewCall(c.funcs["promise_map_get"], mapPtr, keyPtr)

	// Check if NULL
	isNull := c.block.NewICmp(enum.IPredEQ, resultPtr, constant.NewNull(irtypes.I8Ptr))

	someBlock := c.newBlock("map.found")
	noneBlock := c.newBlock("map.notfound")
	mergeBlock := c.newBlock("map.merge")

	c.block.NewCondBr(isNull, noneBlock, someBlock)

	// Found: load value, wrap in Optional
	c.block = someBlock
	typedPtr := c.block.NewBitCast(resultPtr, irtypes.NewPointer(valLLVM))
	loadedVal := c.block.NewLoad(valLLVM, typedPtr)
	optType := irtypes.NewStruct(irtypes.I1, valLLVM)
	someOpt := c.wrapOptional(loadedVal, optType)
	c.block.NewBr(mergeBlock)
	someEnd := c.block

	// Not found: none
	c.block = noneBlock
	noneOpt := constant.NewZeroInitializer(optType)
	c.block.NewBr(mergeBlock)
	noneEnd := c.block

	// Merge
	c.block = mergeBlock
	return mergeBlock.NewPhi(
		&ir.Incoming{X: someOpt, Pred: someEnd},
		&ir.Incoming{X: noneOpt, Pred: noneEnd},
	)
}

// --- Lambda ---

func (c *Compiler) genLambdaExpr(e *ast.LambdaExpr) value.Value {
	sig, ok := c.info.Types[e].(*types.Signature)
	if !ok {
		panic("codegen: lambda expression type is not *types.Signature")
	}

	// Build LLVM function type
	retType := irtypes.Type(irtypes.Void)
	if sig.Result() != nil {
		retType = c.resolveType(sig.Result())
	}

	var params []*ir.Param
	for _, p := range sig.Params() {
		params = append(params, ir.NewParam(p.Name(), c.resolveType(p.Type())))
	}

	// Create anonymous function
	lambdaName := fmt.Sprintf(".lambda.%d", c.lambdaCounter)
	c.lambdaCounter++
	fn := c.module.NewFunc(lambdaName, retType, params...)

	// Save current state
	savedFn := c.fn
	savedBlock := c.block
	savedLocals := c.locals
	savedCanError := c.canError

	// Generate lambda body
	c.fn = fn
	c.locals = make(map[string]*ir.InstAlloca)
	c.canError = false

	entry := fn.NewBlock("entry")
	c.block = entry

	// Allocate parameters
	for i, p := range sig.Params() {
		if p.Name() == "" || p.Name() == "_" {
			continue
		}
		alloca := entry.NewAlloca(c.resolveType(p.Type()))
		alloca.SetName(p.Name() + ".addr")
		entry.NewStore(fn.Params[i], alloca)
		c.locals[p.Name()] = alloca
	}

	// Generate body
	if e.Body != nil {
		c.genBlock(e.Body)
	} else if e.ExprBody != nil {
		val := c.genExpr(e.ExprBody)
		if val != nil && c.block.Term == nil {
			c.block.NewRet(val)
		}
	}

	// Ensure terminator
	if c.block != nil && c.block.Term == nil {
		if _, ok := fn.Sig.RetType.(*irtypes.VoidType); ok {
			c.block.NewRet(nil)
		} else {
			c.block.NewRet(c.zeroValue(fn.Sig.RetType))
		}
	}

	// Restore state
	c.fn = savedFn
	c.block = savedBlock
	c.locals = savedLocals
	c.canError = savedCanError

	// Return function pointer as i8*
	return c.block.NewBitCast(fn, irtypes.I8Ptr)
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

// genFieldOnValue accesses a field on a value of a known type.
// For user types (i8* pointers), it does bitcast + GEP. For other types, panics.
func (c *Compiler) genFieldOnValue(val value.Value, typ types.Type, fieldName string) value.Value {
	named := extractNamed(typ)
	if named == nil {
		panic(fmt.Sprintf("codegen: cannot access field %s on type %s", fieldName, typ))
	}

	field := named.LookupField(fieldName)
	if field == nil {
		panic(fmt.Sprintf("codegen: no field %s on type %s", fieldName, named))
	}

	layout := c.lookupTypeLayout(typ)
	if layout == nil {
		panic(fmt.Sprintf("codegen: no layout for type %s", typ))
	}

	fieldIdx, ok := layout.InstanceFieldIndex[field.Name()]
	if !ok {
		panic(fmt.Sprintf("codegen: field %s not in instance layout for %s", field.Name(), typ))
	}

	typedPtr := c.block.NewBitCast(val, layout.InstancePtrType)
	fieldPtr := c.block.NewGetElementPtr(layout.Instance.LLVMType, typedPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(fieldIdx)))

	return c.block.NewLoad(layout.Instance.Fields[fieldIdx].LLVMType, fieldPtr)
}

// genIndirectCall calls a function through a function pointer (i8*).
func (c *Compiler) genIndirectCall(fnPtr value.Value, sig *types.Signature, args []value.Value) value.Value {
	retType := irtypes.Type(irtypes.Void)
	if sig.Result() != nil {
		retType = c.resolveType(sig.Result())
	}

	var paramTypes []irtypes.Type
	for _, p := range sig.Params() {
		paramTypes = append(paramTypes, c.resolveType(p.Type()))
	}

	funcType := irtypes.NewFunc(retType, paramTypes...)
	funcPtrType := irtypes.NewPointer(funcType)

	typedFnPtr := c.block.NewBitCast(fnPtr, funcPtrType)
	return c.block.NewCall(typedFnPtr, args...)
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

	// Check if the subject is an enum type — use tag comparison
	exprType := c.info.Types[expr]
	if c.typeSubst != nil {
		exprType = types.Substitute(exprType, c.typeSubst)
	}
	if enumLayout := c.lookupEnumLayout(exprType); enumLayout != nil {
		return c.genIsEnumVariant(expr, p.Name, enumLayout)
	}

	// Named type check via RTTI
	return c.genIsNamedType(expr, p.Name)
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

	variantPtr := c.loadVariantPtr(subject)

	// Call promise_type_is(variant_ptr, expected_id) and convert i32 result to i1
	result := c.block.NewCall(c.funcs["promise_type_is"],
		variantPtr, constant.NewInt(irtypes.I32, int64(targetID)))
	return c.block.NewICmp(enum.IPredNE, result, constant.NewInt(irtypes.I32, 0))
}

// loadVariantPtr loads the _variant pointer (RTTI info) from a user type instance.
// All user types are i8*; the first field of any instance struct is the variant pointer.
func (c *Compiler) loadVariantPtr(subject value.Value) value.Value {
	variantPtrStruct := irtypes.NewStruct(irtypes.I8Ptr)
	typedPtr := c.block.NewBitCast(subject, irtypes.NewPointer(variantPtrStruct))
	variantFieldPtr := c.block.NewGetElementPtr(variantPtrStruct, typedPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
	return c.block.NewLoad(irtypes.I8Ptr, variantFieldPtr)
}

// genCastExpr generates code for `expr as Type` and `expr as! Type`.
func (c *Compiler) genCastExpr(e *ast.CastExpr) value.Value {
	// Resolve the target Named type from the TypeRef
	targetRef, ok := e.Type.(*ast.NamedTypeRef)
	if !ok {
		panic(fmt.Sprintf("codegen: unsupported cast target type %T", e.Type))
	}
	targetNamed := c.lookupNamedType(targetRef.Name)
	if targetNamed == nil {
		panic(fmt.Sprintf("codegen: undefined type %s in cast", targetRef.Name))
	}
	targetID := c.assignTypeID(targetNamed)

	subject := c.genExpr(e.Expr)
	variantPtr := c.loadVariantPtr(subject)

	result := c.block.NewCall(c.funcs["promise_type_is"],
		variantPtr, constant.NewInt(irtypes.I32, int64(targetID)))
	isMatch := c.block.NewICmp(enum.IPredNE, result, constant.NewInt(irtypes.I32, 0))

	if e.Force {
		// as! — panic if no match, return the value directly
		okBlock := c.newBlock("cast.ok")
		panicBlock := c.newBlock("cast.panic")
		c.block.NewCondBr(isMatch, okBlock, panicBlock)

		c.block = panicBlock
		panicMsg := c.makeGlobalString("cast failed: as! type mismatch")
		c.block.NewCall(c.funcs["promise_panic"], panicMsg)
		c.block.NewUnreachable()

		c.block = okBlock
		return subject // same i8* pointer, type is verified
	}

	// as — wrap in Optional { i1, i8* }. All Named types use the four-struct ABI
	// where instances are heap-allocated i8* pointers, so the optional payload is always i8*.
	someBlock := c.newBlock("cast.some")
	noneBlock := c.newBlock("cast.none")
	mergeBlock := c.newBlock("cast.merge")
	c.block.NewCondBr(isMatch, someBlock, noneBlock)

	c.block = someBlock
	optType := irtypes.NewStruct(irtypes.I1, irtypes.I8Ptr)
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
