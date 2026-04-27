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
	case *ast.SliceExpr:
		return c.genSliceExpr(e)
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
	// Check for function reference — wrap in fat pointer if used as a value
	if fn, ok := c.funcs[e.Name]; ok {
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

// --- range construction ---

// genRange constructs a range struct { i64 start, i64 end, i1 inclusive }.
// For for-in loops, the struct fields are extracted by the loop codegen.
func (c *Compiler) genRange(e *ast.BinaryExpr) value.Value {
	start := c.genExpr(e.Left)
	end := c.genExpr(e.Right)
	inclusive := constant.NewInt(irtypes.I1, 0)
	if e.Op == ast.BinInclusiveRange {
		inclusive = constant.NewInt(irtypes.I1, 1)
	}

	// Pack into a range struct: { i64, i64, i1 }
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

// rangeStructType returns the LLVM struct type for range: { i64, i64, i1 }.
func (c *Compiler) rangeStructType() *irtypes.StructType {
	return irtypes.NewStruct(irtypes.I64, irtypes.I64, irtypes.I1)
}

// --- Call expressions ---

func (c *Compiler) genCallExpr(e *ast.CallExpr) value.Value {
	// Handle super() calls in constructor bodies
	if ident, ok := e.Callee.(*ast.IdentExpr); ok && ident.Name == "super" {
		return c.genSuperCall(e)
	}

	// Method call or enum variant constructor: callee is MemberExpr
	if member, ok := e.Callee.(*ast.MemberExpr); ok {
		// Handle std.X() — treat as a regular function call to X
		if ident, ok := member.Target.(*ast.IdentExpr); ok && ident.Name == "std" {
			return c.genStdCall(e, member.Field)
		}
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

// genStdCall handles std.X() calls — resolves X in std scope, bypassing user shadows.
func (c *Compiler) genStdCall(e *ast.CallExpr, funcName string) value.Value {
	var argVals []value.Value
	var argTypes []types.Type
	for _, arg := range e.Args {
		argVals = append(argVals, c.genExpr(arg.Value))
		argTypes = append(argTypes, c.info.Types[arg.Value])
		// Clear drop flag: argument is moved into the callee
		if ident, ok := arg.Value.(*ast.IdentExpr); ok {
			c.clearDropFlag(ident.Name)
		}
	}

	// Std extern function
	if ext, ok := c.stdExterns[funcName]; ok {
		return c.genExternCall(ext, argVals, argTypes)
	}

	// Std regular function — must be in stdFuncs
	fn, ok := c.stdFuncs[funcName]
	if !ok {
		panic(fmt.Sprintf("codegen: undefined std function %q", funcName))
	}

	// Look up signature from std scope directly (not general scope chain)
	if c.info.StdScope != nil {
		if obj := c.info.StdScope.Lookup(funcName); obj != nil {
			if callee, ok := obj.(*types.Func); ok {
				if sig, ok := callee.Type().(*types.Signature); ok {
					argVals = c.coerceCallArgs(argVals, argTypes, sig.Params())
				}
			}
		}
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
	var argTypes []types.Type
	for _, arg := range e.Args {
		argVals = append(argVals, c.genExpr(arg.Value))
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

// --- super() calls ---

// genSuperCall generates a super() call inside a new() constructor body.
// Calls the parent's new() (if parent has one) or sets parent fields directly.
func (c *Compiler) genSuperCall(e *ast.CallExpr) value.Value {
	named := c.currentNamed
	if named == nil || len(named.Parents()) == 0 {
		return nil // sema already validated
	}
	parent := named.Parents()[0]

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

		args := []value.Value{thisPtr}
		for _, arg := range e.Args {
			args = append(args, c.genExpr(arg.Value))
			if ident, ok := arg.Value.(*ast.IdentExpr); ok {
				c.clearDropFlag(ident.Name)
			}
		}
		result := c.block.NewCall(fn, args...)

		// If parent new is failable, propagate the error
		newMethod := parent.LookupMethod("new")
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
			provided[arg.Name] = c.genExpr(arg.Value)
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
		args := []value.Value{typedPtr}
		for _, arg := range e.Args {
			args = append(args, c.genExpr(arg.Value))
			if ident, ok := arg.Value.(*ast.IdentExpr); ok {
				c.clearDropFlag(ident.Name)
			}
		}
		newResult := c.block.NewCall(fn, args...)

		// If failable new, check error and wrap result
		newMethod := named.LookupMethod("new")
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
			if vtGlobal, ok := c.vtableGlobals[named]; ok && vtGlobal != nil {
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
		// Implicit constructor: match arguments to field names
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
				c.block.NewStore(val, fieldPtr)
			} else {
				c.block.NewStore(c.zeroValue(layout.Instance.Fields[fieldIdx].LLVMType), fieldPtr)
			}
		}
	}

	// Build value struct: { vtable_ptr, instance_ptr }
	var vtablePtr value.Value
	if vtGlobal, ok := c.vtableGlobals[named]; ok && vtGlobal != nil {
		vtablePtr = constant.NewBitCast(vtGlobal, irtypes.I8Ptr)
	} else {
		vtablePtr = constant.NewNull(irtypes.I8Ptr)
	}
	var valStruct value.Value = constant.NewUndef(userValueType())
	valStruct = c.block.NewInsertValue(valStruct, vtablePtr, 0)
	valStruct = c.block.NewInsertValue(valStruct, rawPtr, 1)
	return valStruct
}

// --- Member access ---

// genMemberExpr generates a field access on a user type instance or an enum variant value.
// TODO: range member access (e.g. r.start) is allowed by sema but will panic here
// because range has no user type layout — it uses a hardcoded { i64, i64, i1 } struct.
// When range field access is needed, add special-case handling similar to genStringLen.
func (c *Compiler) genMemberExpr(e *ast.MemberExpr) value.Value {
	targetType := c.info.Types[e.Target]
	// Apply typeSubst for mono context
	if c.typeSubst != nil {
		targetType = types.Substitute(targetType, c.typeSubst)
	}

	// Container .len property (string, slice, array, map)
	// Check both Instance wrappers (user code: Slice[int]) and bare Named (method body: this is TypSlice)
	if e.Field == "len" {
		named := extractNamed(targetType)
		if named == types.TypString {
			return c.genStringLen(e)
		}
		if _, ok := types.AsSlice(targetType); ok || named == types.TypSlice {
			return c.genSliceLen(e)
		}
		if types.IsMap(targetType) || named == types.TypMap {
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

	// Getter property: emit a method call with no args beyond receiver
	if g := named.LookupGetter(e.Field); g != nil {
		return c.genGetterCall(e, targetType, named, g)
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

	// Container native method dispatch (Slice, Map, string)
	if result, ok := c.genContainerMethodCall(e, member, targetType); ok {
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
		return c.genVirtualMethodCall(e, member, named, method)
	}

	// Direct dispatch: resolve method to a compile-time-known function.
	// For mono/generic types, use resolveTypeName (handles Instance → mono name).
	// For regular Named types with inheritance, use resolveMethodOwner to find
	// the parent that actually defines the method.
	var mangledName string
	ownerName := c.resolveMethodOwner(named, member.Field)
	if ownerName != named.Obj().Name() {
		mangledName = mangleMethodName(ownerName, member.Field, false)
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
		// Container types (Slice, Map, string) are already i8* pointers — pass directly.
		// `this` in a method body is also i8*.
		// Regular user types are value structs — extract the instance pointer.
		if _, isThis := member.Target.(*ast.ThisExpr); isThis {
			args = append(args, target)
		} else if isContainerType(targetType) {
			args = append(args, target)
		} else {
			args = append(args, c.extractInstancePtr(target))
		}
	}
	var argVals []value.Value
	var argTypes []types.Type
	for _, arg := range e.Args {
		argVals = append(argVals, c.genExpr(arg.Value))
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

// genGetterCall emits a call to a getter method (zero args beyond receiver).
// Uses virtual dispatch through the vtable when the static type needs it.
func (c *Compiler) genGetterCall(e *ast.MemberExpr, targetType types.Type, named *types.Named, getter *types.Method) value.Value {
	// Virtual dispatch for getter when static type needs vtable
	if c.needsVtable(named) && !getter.IsNative() {
		return c.genVirtualGetterCall(e, named, getter)
	}

	var mangledName string
	ownerName := c.resolveMethodOwner(named, e.Field)
	if ownerName != named.Obj().Name() {
		mangledName = mangleMethodName(ownerName, e.Field, false)
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
	} else {
		args = append(args, c.extractInstancePtr(target))
	}

	return c.block.NewCall(fn, args...)
}

// genVirtualGetterCall emits an indirect getter call through the vtable.
func (c *Compiler) genVirtualGetterCall(e *ast.MemberExpr, named *types.Named, getter *types.Method) value.Value {
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

	retType := irtypes.Type(irtypes.Void)
	if getter.Sig().Result() != nil {
		retType = c.resolveType(getter.Sig().Result())
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
	named *types.Named, method *types.Method) value.Value {

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

	// 4. Build the correct function type and bitcast
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

	// 5. Call — receiver is instance (i8*), not the value struct
	var args []value.Value
	if method.Sig().Recv() != nil {
		args = append(args, instance)
	}
	var argVals []value.Value
	var argTypes []types.Type
	for _, arg := range e.Args {
		argVals = append(argVals, c.genExpr(arg.Value))
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

// genContainerMethodCall dispatches native method calls on Slice, Map, and string.
// Returns (result, true) if handled, (nil, false) otherwise.
// Non-native methods (with Promise bodies) fall through to the regular call path.
// Handles both Instance wrappers (user code: Slice[int]) and bare Named types
// (method body: this is TypSlice) by resolving type args from typeSubst.
func (c *Compiler) genContainerMethodCall(e *ast.CallExpr, member *ast.MemberExpr, targetType types.Type) (value.Value, bool) {
	methodName := member.Field

	// Check if the method is native — only native methods are handled here.
	// Non-native methods (like is_empty()) fall through to the regular user method path.
	named := extractNamed(targetType)
	if named == types.TypSlice || named == types.TypMap || named == types.TypString {
		m := named.LookupMethod(methodName)
		if m == nil || !m.IsNative() {
			return nil, false // fall through to regular method dispatch
		}
	}

	// Slice methods: push, pop, contains, remove
	if elem, ok := types.AsSlice(targetType); ok {
		return c.genSliceMethodCall(e, member, elem, methodName), true
	}
	// Bare TypSlice (inside a method body on Slice): resolve T from typeSubst
	if named == types.TypSlice {
		if elem := c.resolveTypeParam(types.TypSlice.TypeParams()[0]); elem != nil {
			return c.genSliceMethodCall(e, member, elem, methodName), true
		}
	}

	// Map methods: contains, remove, keys, values
	if key, val, ok := types.AsMap(targetType); ok {
		return c.genMapMethodCall(e, member, key, val, methodName), true
	}
	// Bare TypMap (inside a method body on Map): resolve K, V from typeSubst
	if named == types.TypMap {
		tparams := types.TypMap.TypeParams()
		key := c.resolveTypeParam(tparams[0])
		val := c.resolveTypeParam(tparams[1])
		if key != nil && val != nil {
			return c.genMapMethodCall(e, member, key, val, methodName), true
		}
	}

	// String methods: contains, starts_with, ends_with, index_of, trim, split
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

func (c *Compiler) genSliceMethodCall(e *ast.CallExpr, member *ast.MemberExpr, elemType types.Type, method string) value.Value {
	slicePtr := c.genExpr(member.Target)
	elemLLVM := c.resolveType(elemType)
	elemSize := int64(llvmTypeSize(elemLLVM))

	switch method {
	case "push":
		argVal := c.genExpr(e.Args[0].Value)
		argAlloca := c.block.NewAlloca(elemLLVM)
		// Zero-initialize before store to clear padding bytes for memcmp correctness
		c.block.NewStore(constant.NewZeroInitializer(elemLLVM), argAlloca)
		c.block.NewStore(argVal, argAlloca)
		argPtr := c.block.NewBitCast(argAlloca, irtypes.I8Ptr)
		newSlice := c.block.NewCall(c.funcs["promise_slice_push"],
			slicePtr, argPtr, constant.NewInt(irtypes.I64, elemSize))
		// Store the (possibly reallocated) pointer back
		c.storeBackSlicePtr(member.Target, newSlice)
		return newSlice

	case "pop":
		outAlloca := c.block.NewAlloca(elemLLVM)
		outPtr := c.block.NewBitCast(outAlloca, irtypes.I8Ptr)
		found := c.block.NewCall(c.funcs["promise_slice_pop"],
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
		argVal := c.genExpr(e.Args[0].Value)
		argAlloca := c.block.NewAlloca(elemLLVM)
		// Zero-initialize before store to clear padding bytes for memcmp correctness
		c.block.NewStore(constant.NewZeroInitializer(elemLLVM), argAlloca)
		c.block.NewStore(argVal, argAlloca)
		argPtr := c.block.NewBitCast(argAlloca, irtypes.I8Ptr)
		// Use string equality for string elements
		var eqFn value.Value
		if extractNamed(elemType) == types.TypString {
			eqFn = c.block.NewBitCast(c.funcs["promise_eq_string"], irtypes.I8Ptr)
		} else {
			eqFn = constant.NewNull(irtypes.I8Ptr)
		}
		result := c.block.NewCall(c.funcs["promise_slice_contains"],
			slicePtr, argPtr, constant.NewInt(irtypes.I64, elemSize), eqFn)
		return c.block.NewTrunc(result, irtypes.I1)

	case "remove":
		idx := c.genExpr(e.Args[0].Value)
		c.block.NewCall(c.funcs["promise_slice_remove"],
			slicePtr, idx, constant.NewInt(irtypes.I64, elemSize))
		return nil

	default:
		panic(fmt.Sprintf("codegen: unknown slice method %s", method))
	}
}

// storeBackSlicePtr stores the new slice pointer back into the variable that holds the slice.
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

func (c *Compiler) genMapMethodCall(e *ast.CallExpr, member *ast.MemberExpr, keyType, valType types.Type, method string) value.Value {
	mapPtr := c.genExpr(member.Target)
	keyLLVM := c.resolveType(keyType)
	valLLVM := c.resolveType(valType)

	switch method {
	case "contains":
		argVal := c.genExpr(e.Args[0].Value)
		keyAlloca := c.block.NewAlloca(keyLLVM)
		c.block.NewStore(argVal, keyAlloca)
		keyPtr := c.block.NewBitCast(keyAlloca, irtypes.I8Ptr)
		result := c.block.NewCall(c.funcs["promise_map_contains"], mapPtr, keyPtr)
		return c.block.NewTrunc(result, irtypes.I1)

	case "remove":
		argVal := c.genExpr(e.Args[0].Value)
		keyAlloca := c.block.NewAlloca(keyLLVM)
		c.block.NewStore(argVal, keyAlloca)
		keyPtr := c.block.NewBitCast(keyAlloca, irtypes.I8Ptr)
		result := c.block.NewCall(c.funcs["promise_map_remove"], mapPtr, keyPtr)
		return c.block.NewTrunc(result, irtypes.I1)

	case "keys":
		keySize := int64(llvmTypeSize(keyLLVM))
		return c.block.NewCall(c.funcs["promise_map_keys"],
			mapPtr, constant.NewInt(irtypes.I64, keySize))

	case "values":
		valSize := int64(llvmTypeSize(valLLVM))
		return c.block.NewCall(c.funcs["promise_map_values"],
			mapPtr, constant.NewInt(irtypes.I64, valSize))

	default:
		panic(fmt.Sprintf("codegen: unknown map method %s", method))
	}
}

func (c *Compiler) genStringMethodCall(e *ast.CallExpr, member *ast.MemberExpr, method string) (value.Value, bool) {
	strPtr := c.genExpr(member.Target)

	switch method {
	case "contains":
		argVal := c.genExpr(e.Args[0].Value)
		result := c.block.NewCall(c.funcs["promise_string_contains"], strPtr, argVal)
		return c.block.NewTrunc(result, irtypes.I1), true

	case "starts_with":
		argVal := c.genExpr(e.Args[0].Value)
		result := c.block.NewCall(c.funcs["promise_string_starts_with"], strPtr, argVal)
		return c.block.NewTrunc(result, irtypes.I1), true

	case "ends_with":
		argVal := c.genExpr(e.Args[0].Value)
		result := c.block.NewCall(c.funcs["promise_string_ends_with"], strPtr, argVal)
		return c.block.NewTrunc(result, irtypes.I1), true

	case "index_of":
		argVal := c.genExpr(e.Args[0].Value)
		rawIdx := c.block.NewCall(c.funcs["promise_string_index_of"], strPtr, argVal)
		// Wrap in Optional: -1 → none, otherwise some(idx)
		isNeg := c.block.NewICmp(enum.IPredSLT, rawIdx, constant.NewInt(irtypes.I64, 0))
		optType := irtypes.NewStruct(irtypes.I1, irtypes.I64)
		someBlock := c.newBlock("indexof.some")
		noneBlock := c.newBlock("indexof.none")
		mergeBlock := c.newBlock("indexof.merge")
		c.block.NewCondBr(isNeg, noneBlock, someBlock)

		c.block = someBlock
		someOpt := c.wrapOptional(rawIdx, optType)
		c.block.NewBr(mergeBlock)
		someEnd := c.block

		c.block = noneBlock
		noneOpt := constant.NewZeroInitializer(optType)
		c.block.NewBr(mergeBlock)
		noneEnd := c.block

		c.block = mergeBlock
		phi := c.block.NewPhi(ir.NewIncoming(someOpt, someEnd), ir.NewIncoming(noneOpt, noneEnd))
		return phi, true

	case "trim":
		result := c.block.NewCall(c.funcs["promise_string_trim"], strPtr)
		return result, true

	case "split":
		argVal := c.genExpr(e.Args[0].Value)
		result := c.block.NewCall(c.funcs["promise_string_split"], strPtr, argVal)
		return result, true

	default:
		return nil, false
	}
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
	elem, ok := types.AsSlice(typ)
	if !ok {
		panic(fmt.Sprintf("codegen: array literal type is %T, want Slice instance", typ))
	}
	elemLLVM := c.resolveType(elem)
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

func (c *Compiler) genSliceExpr(e *ast.SliceExpr) value.Value {
	panic("codegen: slice expressions ([start:end]) not yet implemented")
}

func (c *Compiler) genIndexExpr(e *ast.IndexExpr) value.Value {
	targetType := c.info.Types[e.Target]
	if c.typeSubst != nil {
		targetType = types.Substitute(targetType, c.typeSubst)
	}

	if elem, ok := types.AsSlice(targetType); ok {
		return c.genSliceIndex(e, elem)
	}
	if key, val, ok := types.AsMap(targetType); ok {
		return c.genMapIndex(e, key, val)
	}
	panic(fmt.Sprintf("codegen: cannot index type %s", targetType))
}

func (c *Compiler) genSliceIndex(e *ast.IndexExpr, elemType types.Type) value.Value {
	slicePtr := c.genExpr(e.Target)
	idx := c.genExpr(e.Index)
	elemLLVM := c.resolveType(elemType)

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
	keyType, valType, ok := types.AsMap(typ)
	if !ok {
		panic(fmt.Sprintf("codegen: map literal type is %T, want Map instance", typ))
	}
	keyLLVM := c.resolveType(keyType)
	valLLVM := c.resolveType(valType)
	keySize := int64(llvmTypeSize(keyLLVM))
	valSize := int64(llvmTypeSize(valLLVM))

	// Determine hash/eq function pointers (string keys need content-based comparison)
	var hashFn, eqFn value.Value
	if extractNamed(keyType) == types.TypString {
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

func (c *Compiler) genMapIndex(e *ast.IndexExpr, keyType, valType types.Type) value.Value {
	mapPtr := c.genExpr(e.Target)
	keyVal := c.genExpr(e.Index)
	keyLLVM := c.resolveType(keyType)
	valLLVM := c.resolveType(valType)

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
			envFieldTypes[i] = captureType
			// Load the captured variable's current value from the enclosing scope
			if alloca, ok := c.locals[cv.Obj.Name()]; ok {
				captureVals[i] = c.block.NewLoad(captureType, alloca)
			} else {
				captureVals[i] = constant.NewZeroInitializer(captureType)
			}
			// For move captures, clear the drop flag in the enclosing scope
			if cv.ByMove {
				c.clearDropFlag(cv.Obj.Name())
			}
		}
		envStructType = irtypes.NewStruct(envFieldTypes...)

		// Allocate env struct on heap
		envSize := int64(llvmTypeSize(envStructType))
		rawPtr := c.block.NewCall(c.funcs["malloc"], constant.NewInt(irtypes.I64, envSize))
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
	savedLocals := c.locals
	savedCanError := c.canError
	savedRetType := c.currentRetType
	savedBlockCounter := c.blockCounter
	savedScopeBindings := c.scopeBindings
	savedDropFlags := c.dropFlags
	savedLoopScopeDepth := c.loopScopeDepth

	// Generate lambda body with fresh scope state
	c.fn = fn
	c.locals = make(map[string]*ir.InstAlloca)
	c.blockCounter = 0
	c.canError = false
	c.currentRetType = sig.Result()
	c.scopeBindings = nil
	c.dropFlags = make(map[string]*ir.InstAlloca)
	c.loopScopeDepth = 0

	entry := fn.NewBlock("entry")
	c.block = entry

	// Load captured variables from env struct into local allocas
	if len(captures) > 0 && envStructType != nil {
		typedEnvPtr := entry.NewBitCast(fn.Params[0], irtypes.NewPointer(envStructType))
		for i, cv := range captures {
			captureType := c.resolveType(cv.Obj.Type())
			fieldPtr := entry.NewGetElementPtr(envStructType, typedEnvPtr,
				constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(i)))
			val := entry.NewLoad(captureType, fieldPtr)
			alloca := entry.NewAlloca(captureType)
			alloca.SetName(cv.Obj.Name() + ".cap")
			entry.NewStore(val, alloca)
			c.locals[cv.Obj.Name()] = alloca
			// Register drop for move-captured droppable types
			if cv.ByMove {
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
		alloca.SetName(p.Name() + ".addr")
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
	c.locals = savedLocals
	c.canError = savedCanError
	c.currentRetType = savedRetType
	c.blockCounter = savedBlockCounter
	c.scopeBindings = savedScopeBindings
	c.dropFlags = savedDropFlags
	c.loopScopeDepth = savedLoopScopeDepth

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

		fieldIdx, ok := layout.InstanceFieldIndex[field.Name()]
		if !ok {
			panic(fmt.Sprintf("codegen: field %s not in instance layout for %s", field.Name(), typ))
		}

		typedPtr := c.block.NewBitCast(val, layout.InstancePtrType)
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
	entry := thunk.NewBlock("entry")

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

	// Extract instance pointer — `this` is already i8*, others are value structs
	var instance value.Value
	if _, isThis := expr.(*ast.ThisExpr); isThis {
		instance = subject
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
	// Extract instance pointer — `this` is already i8*, others are value structs
	var instance value.Value
	if _, isThis := e.Expr.(*ast.ThisExpr); isThis {
		instance = subject
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
