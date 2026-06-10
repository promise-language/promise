package sema

import (
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"

	"github.com/promise-language/promise/compiler/internal/ast"
	"github.com/promise-language/promise/compiler/internal/types"
)

// suffixToType maps a numeric literal suffix to its corresponding type.
func suffixToType(suffix string) types.Type {
	switch suffix {
	case "i8":
		return types.TypI8
	case "i16":
		return types.TypI16
	case "i32":
		return types.TypI32
	case "i64":
		return types.TypI64
	case "u8":
		return types.TypU8
	case "u16":
		return types.TypU16
	case "u32":
		return types.TypU32
	case "u64":
		return types.TypU64
	case "i":
		return types.TypInt
	case "u":
		return types.TypUint
	case "f32":
		return types.TypF32
	case "f64":
		return types.TypF64
	}
	return nil
}

// isSignedIntSuffix reports whether suffix is a signed integer suffix.
func isSignedIntSuffix(suffix string) bool {
	return suffix == "i" || suffix == "i8" || suffix == "i16" || suffix == "i32" || suffix == "i64"
}

// validateIntRange checks that a raw integer literal value fits in the target
// type. When negated is true, the value is checked as -(raw) against the
// signed minimum. Returns an error message or "" if valid.
func validateIntRange(raw string, typ types.Type, negated bool) string {
	cleanRaw := strings.ReplaceAll(raw, "_", "")
	val, err := strconv.ParseUint(cleanRaw, 0, 64)
	if err != nil {
		// Try signed parse for edge cases
		sval, serr := strconv.ParseInt(cleanRaw, 0, 64)
		if serr != nil {
			return fmt.Sprintf("invalid integer literal: %s", raw)
		}
		val = uint64(sval)
	}

	var maxPos uint64
	var minNeg uint64 // absolute value of minimum (e.g. 128 for i8)
	switch typ {
	case types.TypI8:
		maxPos, minNeg = math.MaxInt8, 128
	case types.TypI16:
		maxPos, minNeg = math.MaxInt16, 32768
	case types.TypI32:
		maxPos, minNeg = math.MaxInt32, 2147483648
	case types.TypInt, types.TypI64:
		maxPos, minNeg = math.MaxInt64, 9223372036854775808
	case types.TypU8:
		maxPos = math.MaxUint8
	case types.TypU16:
		maxPos = math.MaxUint16
	case types.TypU32:
		maxPos = math.MaxUint32
	case types.TypUint, types.TypU64:
		maxPos = math.MaxUint64
	default:
		return ""
	}

	switch {
	case typ == types.TypUint || typ == types.TypU8 || typ == types.TypU16 || typ == types.TypU32 || typ == types.TypU64:
		if val > maxPos {
			return fmt.Sprintf("value %s overflows %s (max %d)", raw, typ, maxPos)
		}
	default: // signed
		if negated {
			if val > minNeg {
				return fmt.Sprintf("value -%s overflows %s (min -%d)", raw, typ, minNeg)
			}
		} else {
			if val > maxPos {
				return fmt.Sprintf("value %s overflows %s (max %d)", raw, typ, maxPos)
			}
		}
	}
	return ""
}

// isIntegerType reports whether t is any integer type (signed or unsigned).
func isIntegerType(t types.Type) bool {
	switch t {
	case types.TypInt, types.TypI8, types.TypI16, types.TypI32, types.TypI64,
		types.TypUint, types.TypU8, types.TypU16, types.TypU32, types.TypU64:
		return true
	}
	return false
}

// isFloatType reports whether t is any float type.
func isFloatType(t types.Type) bool {
	return t == types.TypF32 || t == types.TypF64
}

// isNumericType reports whether t is any numeric type.
func isNumericType(t types.Type) bool {
	return isIntegerType(t) || isFloatType(t)
}

// isScalarCastType reports whether t is a scalar type that can participate
// in as/as! casts: numeric types, char (i32 codepoint), and bool (i1).
func isScalarCastType(t types.Type) bool {
	return isNumericType(t) || t == types.TypChar || t == types.TypBool
}

// checkExprWithHint type-checks an expression with an optional expected type.
// The hint propagates through arithmetic expressions so that nested literals
// (e.g. 1 + 2 in `uint a = 1 + 2`) adapt to the expected type.
func (c *Checker) checkExprWithHint(expr ast.Expr, hint types.Type) types.Type {
	old := c.typeHint
	c.typeHint = hint
	typ := c.checkExpr(expr)
	c.typeHint = old
	return typ
}

// checkExpr type-checks an expression and returns its resolved type.
// The result is also recorded in c.info.Types.
func (c *Checker) checkExpr(expr ast.Expr) types.Type {
	if expr == nil {
		return nil
	}

	// Save and clear the type hint so it doesn't leak into unrelated
	// sub-expressions. Only numeric literals and transparent wrappers
	// (binary, unary, paren) use the saved hint.
	hint := c.typeHint
	c.typeHint = nil

	// T0685: Same save/clear pattern for the slice-type-ref permission. A bare
	// `T[]` is a type expression, only legitimate as a CallExpr.Callee or
	// MemberExpr.Target — granted by those two call sites and consumed below
	// by the SliceTypeExpr case. Cleared here so the permission cannot leak
	// across unrelated sub-expressions.
	allowSliceType := c.sliceTypeAllowed
	c.sliceTypeAllowed = false

	var typ types.Type

	switch e := expr.(type) {
	case *ast.IntLit:
		if e.Suffix != "" {
			typ = suffixToType(e.Suffix)
			if typ == nil {
				c.errorf(e.Pos(), "unknown numeric suffix '%s'", e.Suffix)
				typ = types.TypInt
			} else {
				if msg := validateIntRange(e.Raw, typ, c.inUnaryNeg); msg != "" {
					c.errorf(e.Pos(), "%s", msg)
				}
			}
		} else if hint != nil && isIntegerType(hint) {
			typ = hint
		} else {
			typ = types.TypInt
		}

	case *ast.FloatLit:
		if e.Suffix != "" {
			typ = suffixToType(e.Suffix)
			if typ == nil {
				c.errorf(e.Pos(), "unknown numeric suffix '%s'", e.Suffix)
				typ = types.TypF64
			}
		} else if hint != nil && isFloatType(hint) {
			typ = hint
		} else {
			typ = types.TypF64
		}

	case *ast.BoolLit:
		typ = types.TypBool

	case *ast.CharLit:
		typ = types.TypChar

	case *ast.StringLit:
		for _, part := range e.Parts {
			if interp, ok := part.(ast.StringInterp); ok {
				if interp.Expr == nil {
					if interp.Raw == "{}" {
						c.errorf(e.Pos(), "empty interpolation '{}' in string literal")
					}
				} else {
					c.checkExpr(interp.Expr)
					c.validateInterpolationType(c.info.Types[interp.Expr], interp.Expr)
				}
			}
		}
		typ = types.TypString

	case *ast.NoneLit:
		typ = types.TypNone

	case *ast.IdentExpr:
		typ = c.checkIdentExpr(e)

	case *ast.ThisExpr:
		typ = c.checkThisExpr(e)

	case *ast.ParenExpr:
		c.typeHint = hint // propagate through parentheses
		typ = c.checkExpr(e.Expr)

	case *ast.TupleLit:
		typ = c.checkTupleLit(e)

	case *ast.ArrayLit:
		typ = c.checkArrayLit(e, hint)

	case *ast.MapLit:
		typ = c.checkMapLit(e)

	case *ast.BinaryExpr:
		c.typeHint = hint // propagate through binary expressions
		typ = c.checkBinaryExpr(e)

	case *ast.UnaryExpr:
		c.typeHint = hint // propagate through unary expressions
		typ = c.checkUnaryExpr(e)

	case *ast.CallExpr:
		typ = c.checkCallExpr(e)

	case *ast.MemberExpr:
		typ = c.checkMemberExpr(e)

	case *ast.IndexExpr:
		typ = c.checkIndexExpr(e)

	case *ast.SliceExpr:
		typ = c.checkSliceExpr(e)

	case *ast.SliceTypeExpr:
		// T0685: Re-publish the snapshotted permission so checkSliceTypeExpr
		// (which is the consumer) can see it. Clear after so it doesn't leak.
		c.sliceTypeAllowed = allowSliceType
		typ = c.checkSliceTypeExpr(e)
		c.sliceTypeAllowed = false

	case *ast.OptionalChainExpr:
		typ = c.checkOptionalChainExpr(e)

	case *ast.IsExpr:
		typ = c.checkIsExpr(e)

	case *ast.CastExpr:
		typ = c.checkCastExpr(e)

	case *ast.ErrorPropagateExpr:
		typ = c.checkErrorPropagateExpr(e)

	case *ast.ErrorPanicExpr:
		typ = c.checkErrorPanicExpr(e)

	case *ast.OptionalUnwrapExpr:
		typ = c.checkOptionalUnwrapExpr(e)

	case *ast.AutoCloneExpr:
		typ = c.checkAutoCloneExpr(e)

	case *ast.ErrorHandlerExpr:
		typ = c.checkErrorHandlerExpr(e)

	case *ast.IfExpr:
		typ = c.checkIfExpr(e)

	case *ast.MatchExpr:
		typ = c.checkMatchExpr(e)

	case *ast.LambdaExpr:
		typ = c.checkLambdaExpr(e)

	case *ast.GoExpr:
		typ = c.checkGoExpr(e)

	case *ast.UnsafeExpr:
		typ = c.checkUnsafeExpr(e)

	case *ast.TypeRefExpr:
		typ = c.resolveType(e.Ref)

	default:
		c.errorf(expr.Pos(), "unsupported expression type")
	}

	c.recordType(expr, typ)
	return typ
}

func (c *Checker) checkIdentExpr(e *ast.IdentExpr) types.Type {
	// "present" and "absent" are contextual keywords — only special after `is`.
	// As standalone identifiers they are normal variables (looked up below).

	// Self resolves to the enclosing type
	if e.Name == "Self" {
		if c.curType == nil {
			c.errorf(e.Pos(), "Self can only be used inside a type body")
			return nil
		}
		return c.selfType()
	}

	obj := c.lookup(e.Name)
	if obj == nil {
		c.errorf(e.Pos(), "undefined: %s", e.Name)
		c.suggestForUndefinedIdent(e.Pos(), e.Name)
		return nil
	}
	// Module objects are valid as member-access targets (mod.func()),
	// but not as standalone values. Return nil type — checkMemberExpr
	// handles the dispatch when it sees a Module-typed target.
	if mod, ok := obj.(*types.Module); ok {
		if mod.Scope() == nil {
			c.errorf(e.Pos(), "module '%s' has no loaded scope", e.Name)
			return nil
		}
		c.recordObject(e, obj)
		return nil // not a value; checkMemberExpr handles qualified access
	}
	c.recordObject(e, obj)

	// Check for deprecated usage
	c.checkDeprecatedObj(e.Pos(), obj)

	// Module-level getter accessed without module prefix (same file or glob import):
	// treat as implicit call — return result type, not Signature.
	if fn, ok := obj.(*types.Func); ok && fn.IsGetter() {
		sig, ok := fn.Type().(*types.Signature)
		if ok && sig != nil {
			if sig.CanError() {
				c.info.FailableExprs[e] = true
			}
			return sig.Result()
		}
	}

	// Capture analysis: if inside a lambda, check if this variable is from an outer scope
	if c.lambdaDepth > 0 {
		c.checkLambdaCapture(e, obj)
	}

	return obj.Type()
}

// checkLambdaCapture detects and records when a lambda references an outer-scope variable.
func (c *Checker) checkLambdaCapture(e *ast.IdentExpr, obj types.Object) {
	// Only capture variables (not types, funcs, etc.)
	v, ok := obj.(*types.Var)
	if !ok {
		return
	}

	// Find the scope where this variable is declared
	_, declScope := c.scope.LookupParent(e.Name)
	if declScope == nil {
		return
	}

	// Check if the variable was declared outside the lambda boundary.
	// Walk from lambdaScope upward — if declScope is lambdaScope or an ancestor, it's outer.
	isOuter := false
	for s := c.lambdaScope; s != nil; s = s.Parent() {
		if s == declScope {
			isOuter = true
			break
		}
	}
	if !isOuter {
		return // variable is declared inside the lambda — no capture needed
	}

	// Already captured?
	if _, already := c.lambdaCaptures[e.Name]; already {
		return
	}

	// Determine capture mode
	byMove := c.lambdaMove
	if !byMove && !isCopyField(v.Type()) {
		c.errorf(e.Pos(), "cannot capture non-copy variable '%s' without move", e.Name)
		return
	}

	c.lambdaCaptures[e.Name] = &CapturedVar{
		Obj:    obj,
		ByMove: byMove,
	}
}

func (c *Checker) checkThisExpr(e *ast.ThisExpr) types.Type {
	if c.curFunc != nil && c.curFunc.Recv() != nil {
		return c.curFunc.Recv().Type()
	}
	// Inside a lambda inside a method: capture 'this' from outer scope
	if c.lambdaDepth > 0 {
		obj, _ := c.scope.LookupParent("this")
		if obj != nil {
			if v, ok := obj.(*types.Var); ok {
				if _, already := c.lambdaCaptures["this"]; !already {
					byMove := c.lambdaMove
					if !byMove && !isCopyField(v.Type()) {
						c.errorf(e.Pos(), "cannot capture 'this' without move")
						return v.Type()
					}
					c.lambdaCaptures["this"] = &CapturedVar{
						Obj:    obj,
						ByMove: byMove,
					}
				}
				return v.Type()
			}
		}
	}
	c.errorf(e.Pos(), "'this' used outside of a method")
	return nil
}

func (c *Checker) checkTupleLit(e *ast.TupleLit) types.Type {
	elems := make([]types.Type, len(e.Elements))
	for i, el := range e.Elements {
		elems[i] = c.checkExpr(el)
		if elems[i] == nil {
			return nil
		}
	}
	return types.NewTuple(elems)
}

func (c *Checker) checkArrayLit(e *ast.ArrayLit, hint types.Type) types.Type {
	if len(e.Elements) == 0 {
		// Empty array with a Vector hint (e.g. from variadic param) → use the hint type.
		if hint != nil {
			if _, ok := types.AsVector(hint); ok {
				return hint
			}
		}
		c.errorf(e.Pos(), "cannot infer type of empty array literal")
		return nil
	}

	elemType := c.checkExpr(e.Elements[0])
	if elemType == nil {
		return nil
	}
	// T0381: vector/array elements are owned by the container — strip
	// borrow refs so `[a.borrow]` produces a `string[]` (with the move
	// rejected later by ownership) rather than an unassignable `string&[]`.
	elemType = stripRef(elemType)

	for i := 1; i < len(e.Elements); i++ {
		et := c.checkExpr(e.Elements[i])
		if et == nil {
			continue
		}
		et = stripRef(et)
		if !types.Identical(et, elemType) {
			c.errorf(e.Elements[i].Pos(), "array element type mismatch: expected %s, got %s", elemType, et)
		}
	}

	// T0545: a vector/array literal whose element type transitively contains a
	// single-owner handle (e.g. [[go f()]] → Vector[Vector[Task]]) forces the
	// outer container to duplicate the handle on push/clone/realloc — unsound.
	c.reportContainerSingleOwnerNesting(e.Pos(), elemType)

	// If hint is a fixed-size array type, produce Array instead of Vector
	if arr, ok := hint.(*types.Array); ok {
		if int64(len(e.Elements)) != arr.Size() {
			c.errorf(e.Pos(), "array literal has %d elements but type %s requires %d",
				len(e.Elements), arr, arr.Size())
			return nil
		}
		return types.NewArray(elemType, arr.Size())
	}

	inst := types.NewVector(elemType)
	c.recordInstance(inst)
	return inst
}

func (c *Checker) checkMapLit(e *ast.MapLit) types.Type {
	if len(e.Entries) == 0 {
		c.errorf(e.Pos(), "cannot infer type of empty map literal")
		return nil
	}

	var keyType, valType types.Type
	for i, entry := range e.Entries {
		kt := c.checkExpr(entry.Key)
		vt := c.checkExpr(entry.Value)
		if i == 0 {
			keyType = kt
			valType = vt
			continue
		}
		if kt != nil && keyType != nil && !types.Identical(kt, keyType) {
			c.errorf(entry.Key.Pos(), "map key type mismatch: expected %s, got %s", keyType, kt)
		}
		if vt != nil && valType != nil && !types.Identical(vt, valType) {
			c.errorf(entry.Value.Pos(), "map value type mismatch: expected %s, got %s", valType, vt)
		}
	}

	if keyType == nil || valType == nil {
		return nil
	}
	c.validateConstraints(e.Pos(), types.TypMap, []types.Type{keyType, valType})
	// T0545: reject map literals whose key/value type nests a single-owner
	// handle inside another container (e.g. Map[int, Vector[Task]]).
	c.reportContainerSingleOwnerNesting(e.Pos(), keyType)
	c.reportContainerSingleOwnerNesting(e.Pos(), valType)

	inst := types.NewMap(keyType, valType)
	c.recordInstance(inst)
	return inst
}

func (c *Checker) checkBinaryExpr(e *ast.BinaryExpr) types.Type {
	hint := c.typeHint // save hint propagated from caller
	left := c.checkExpr(e.Left)
	c.typeHint = hint // restore for right operand
	right := c.checkExpr(e.Right)

	if left == nil || right == nil {
		return nil
	}

	// Adapt numeric operands when one side resolved to a non-default type
	// and the other defaulted to int/f64. Re-check the defaulted side with
	// a hint so that nested literals (e.g., `uint_var == 1 + 4`) adapt.
	if isIntegerType(left) && left != types.TypInt && right == types.TypInt {
		c.typeHint = left
		right = c.checkExpr(e.Right)
	}
	if isIntegerType(right) && right != types.TypInt && left == types.TypInt {
		c.typeHint = right
		left = c.checkExpr(e.Left)
	}
	if isFloatType(left) && left != types.TypF64 && right == types.TypF64 {
		c.typeHint = left
		right = c.checkExpr(e.Right)
	}
	if isFloatType(right) && right != types.TypF64 && left == types.TypF64 {
		c.typeHint = right
		left = c.checkExpr(e.Left)
	}

	c.checkSubExprFailable(e.Left)
	c.checkSubExprFailable(e.Right)

	switch e.Op {
	case ast.BinAnd, ast.BinOr:
		if !types.Identical(left, types.TypBool) {
			c.errorf(e.Left.Pos(), "operator %s requires bool, got %s", e.Op, left)
		}
		if !types.Identical(right, types.TypBool) {
			c.errorf(e.Right.Pos(), "operator %s requires bool, got %s", e.Op, right)
		}
		return types.TypBool

	case ast.BinElvis:
		// left must be T?, result is T
		opt, ok := left.(*types.Optional)
		if !ok {
			c.errorf(e.Left.Pos(), "operator ?: requires optional type, got %s", left)
			return right
		}
		inner := opt.Elem()
		if !types.AssignableTo(right, inner) {
			c.errorf(e.Right.Pos(), "operator ?: default type %s not assignable to %s", right, inner)
		}
		return inner

	case ast.BinExclusiveRange:
		return c.checkOperator(e.Pos(), left, "..", right)
	case ast.BinInclusiveRange:
		return c.checkOperator(e.Pos(), left, "..=", right)

	default:
		// Arithmetic and comparison: lookup operator method on left type
		return c.checkOperator(e.Pos(), left, e.Op.String(), right)
	}
}

// checkOperator looks up a binary operator method on the left type
// and validates the right operand matches.
func (c *Checker) checkOperator(pos ast.Pos, left types.Type, op string, right types.Type) types.Type {
	var named *types.Named
	var subst map[*types.TypeParam]types.Type

	// T0381: unwrap shared/mut refs so operators dispatch on the underlying type.
	// `a.borrow == 42` looks up `int.equal(int)` after stripping the `int&`.
	if sr, ok := left.(*types.SharedRef); ok {
		left = sr.Elem()
	}
	if mr, ok := left.(*types.MutRef); ok {
		left = mr.Elem()
	}

	switch t := left.(type) {
	case *types.Named:
		named = t
	case *types.Instance:
		origin, ok := t.Origin().(*types.Named)
		if !ok {
			c.errorf(pos, "operator %s not defined on type %s", op, left)
			return nil
		}
		named = origin
		subst = types.BuildSubstMap(origin.TypeParams(), t.TypeArgs())
	case *types.TypeParam:
		// Look up operator on constraint types
		for _, constraint := range t.Constraints() {
			if cn, ok := constraint.(*types.Named); ok {
				if m := cn.LookupMethod(op); m != nil {
					named = cn
					break
				}
			}
		}
		if named == nil {
			c.errorf(pos, "operator %s not defined on type parameter %s", op, left)
			return nil
		}
		// For TypeParam operators, the parameter type is Self (the constraint type).
		// The right operand must be the same TypeParam type.
		m := named.LookupMethod(op)
		sig := m.Sig()
		if len(sig.Params()) != 1 {
			c.errorf(pos, "operator %s has invalid signature", op)
			return nil
		}
		// Accept right operand if it's the same type parameter
		if !types.Identical(left, right) {
			c.errorf(pos, "operator %s: cannot use %s as %s", op, right, left)
			return nil
		}
		if sig.Result() != nil {
			// Substitute Self (the constraint type) with the actual TypeParam
			// so that Self-returning operators produce T, not the interface type.
			result := sig.Result()
			if rn, ok := result.(*types.Named); ok && rn == named {
				return left // return the TypeParam
			}
			return result
		}
		return types.TypVoid
	default:
		c.errorf(pos, "operator %s not defined on type %s", op, left)
		return nil
	}

	m := named.LookupMethod(op)
	if m == nil {
		c.errorf(pos, "operator %s not defined on type %s", op, left)
		return nil
	}

	sig := m.Sig()
	if subst != nil {
		sig = types.Substitute(sig, subst).(*types.Signature)
	}
	if len(sig.Params()) != 1 {
		c.errorf(pos, "operator %s has invalid signature", op)
		return nil
	}

	paramType := sig.Params()[0].Type()
	if !types.AssignableTo(right, paramType) {
		c.errorf(pos, "operator %s: cannot use %s as %s", op, right, paramType)
		return nil
	}

	if sig.Result() != nil {
		result := sig.Result()
		if inst, ok := result.(*types.Instance); ok {
			c.recordInstance(inst)
		}
		return result
	}
	return types.TypVoid
}

func (c *Checker) checkUnaryExpr(e *ast.UnaryExpr) types.Type {
	// Set inUnaryNeg so that suffixed integer literals (e.g. 128i8 in -128i8)
	// validate against the signed minimum instead of the signed maximum.
	// Save/restore to handle nested unary expressions correctly.
	savedNeg := c.inUnaryNeg
	if e.Op == ast.UnaryNeg {
		c.inUnaryNeg = true
	} else {
		c.inUnaryNeg = false
	}
	operand := c.checkExpr(e.Operand)
	c.inUnaryNeg = savedNeg
	if operand == nil {
		return nil
	}
	c.checkSubExprFailable(e.Operand)

	switch e.Op {
	case ast.UnaryNot:
		return c.checkUnaryOperator(e.Pos(), operand, "!")

	case ast.UnaryNeg:
		return c.checkUnaryOperator(e.Pos(), operand, "-")

	case ast.UnaryBitwiseNot:
		return c.checkUnaryOperator(e.Pos(), operand, "~")

	case ast.UnaryReceive:
		// <-expr: operand should be Task[T] or Channel[T]
		// Task[T] returns T, Channel[T] returns T? (none when closed+empty)
		if inst, ok := operand.(*types.Instance); ok {
			origin := inst.Origin()
			if origin == types.TypTask {
				if len(inst.TypeArgs()) > 0 {
					return inst.TypeArgs()[0]
				}
			}
			if origin == types.TypChannel {
				if len(inst.TypeArgs()) > 0 {
					return types.NewOptional(inst.TypeArgs()[0])
				}
			}
		}
		c.errorf(e.Pos(), "receive operator (<-) requires Task[T] or Channel[T], got %s", operand)
		return nil

	default:
		c.errorf(e.Pos(), "unsupported unary operator")
		return nil
	}
}

func (c *Checker) checkUnaryOperator(pos ast.Pos, operand types.Type, op string) types.Type {
	var named *types.Named
	var subst map[*types.TypeParam]types.Type

	// T0381: unwrap shared/mut refs so operators dispatch on the underlying type.
	if sr, ok := operand.(*types.SharedRef); ok {
		operand = sr.Elem()
	}
	if mr, ok := operand.(*types.MutRef); ok {
		operand = mr.Elem()
	}

	switch t := operand.(type) {
	case *types.Named:
		named = t
	case *types.Instance:
		origin, ok := t.Origin().(*types.Named)
		if !ok {
			c.errorf(pos, "operator %s not defined on type %s", op, operand)
			return nil
		}
		named = origin
		subst = types.BuildSubstMap(origin.TypeParams(), t.TypeArgs())
	default:
		c.errorf(pos, "operator %s not defined on type %s", op, operand)
		return nil
	}

	// Find the unary variant (0 params, not counting receiver)
	var m *types.Method
	for _, method := range named.Methods() {
		if method.Name() == op && len(method.Sig().Params()) == 0 {
			m = method
			break
		}
	}
	if m == nil {
		c.errorf(pos, "operator %s not defined on type %s", op, operand)
		return nil
	}

	result := m.Sig().Result()
	if subst != nil && result != nil {
		result = types.Substitute(result, subst)
	}
	if result != nil {
		return result
	}
	return types.TypVoid
}

// checkConstructorCall handles Type(field: value, ...) constructor calls.
func (c *Checker) checkConstructorCall(e *ast.CallExpr, named *types.Named) types.Type {
	// Prevent instantiation of abstract types
	if named.IsAbstract() {
		c.errorf(e.Pos(), "cannot instantiate abstract type %s", named)
		return named
	}

	// Type argument inference for generic type constructors: Box(value: 42) → Box[int].
	if len(named.TypeParams()) > 0 && !named.HasNew() {
		inst := c.inferConstructorCall(e, named)
		if inst != nil {
			return c.checkInstanceConstructorCall(e, inst)
		}
		// Inference failed — error already reported.
		for _, arg := range e.Args {
			c.checkExpr(arg.Value)
		}
		return nil
	}

	// Build substitution map for inherited fields from generic parents
	subst := c.buildParentSubstMap(named)

	// If the type has an explicit new() constructor, route through parameter checking
	if named.HasNew() {
		return c.checkNewConstructorCall(e, named, subst)
	}

	c.resolveImplicitConstructorArgs(e, named, subst)
	return named
}

// checkNewConstructorCall validates a constructor call against the new() method's parameters.
// subst is non-nil for generic instantiations.
func (c *Checker) checkNewConstructorCall(e *ast.CallExpr, named *types.Named, subst map[*types.TypeParam]types.Type) types.Type {
	newMethod := named.LookupMethod("new")
	if newMethod == nil {
		return named
	}

	callDesc := "constructor for " + named.String()
	c.resolveCallArgs(e, newMethod.Sig().Params(), callDesc, subst)

	if newMethod.Sig().CanError() {
		c.info.FailableExprs[e] = true
	}

	return named
}

// checkSuperCall validates a super(...) call inside a new() constructor body.
// super() calls the parent's new() or implicit constructor to initialize inherited fields.
func (c *Checker) checkSuperCall(e *ast.CallExpr) types.Type {
	if !c.inNewBody {
		c.errorf(e.Pos(), "super() can only be called inside a new() constructor")
		// Still type-check arguments
		for _, arg := range e.Args {
			c.checkExpr(arg.Value)
		}
		return types.TypVoid
	}
	if c.curType == nil {
		c.errorf(e.Pos(), "super() used outside of a type")
		return types.TypVoid
	}
	parentRefs := c.curType.Parents()
	if len(parentRefs) == 0 {
		c.errorf(e.Pos(), "super() called but type %s has no parent", c.curType)
		for _, arg := range e.Args {
			c.checkExpr(arg.Value)
		}
		return types.TypVoid
	}
	parent := parentRefs[0].Named // first parent is the concrete parent

	if parent.HasNew() {
		// Parent has explicit new() — validate args against parent's new() params
		newMethod := parent.LookupMethod("new")
		if newMethod == nil || newMethod.Sig() == nil {
			return types.TypVoid
		}
		callDesc := "super() (parent " + parent.String() + " new)"
		c.resolveCallArgs(e, newMethod.Sig().Params(), callDesc, nil)
	} else {
		// Parent has implicit constructor — validate named args against parent's fields
		c.resolveImplicitConstructorArgs(e, parent, nil)
	}
	return types.TypVoid
}

// checkInstanceConstructorCall handles constructor calls on generic instances: Box[int](value: 42).
func (c *Checker) checkInstanceConstructorCall(e *ast.CallExpr, inst *types.Instance) types.Type {
	origin, ok := inst.Origin().(*types.Named)
	if !ok {
		c.errorf(e.Pos(), "cannot construct %s", inst)
		return nil
	}

	// Prevent instantiation of abstract types
	if origin.IsAbstract() {
		c.errorf(e.Pos(), "cannot instantiate abstract type %s", inst)
		return inst
	}

	// Built-in types with special constructors managed by codegen.
	if origin == types.TypChannel {
		// Channel[T]() or Channel[T](capacity: n) — at most 1 arg
		if len(e.Args) > 1 {
			c.errorf(e.Pos(), "channel constructor expects at most 1 argument, got %d", len(e.Args))
		}
		for _, arg := range e.Args {
			argType := c.checkExpr(arg.Value)
			if argType != nil && !types.AssignableTo(argType, types.TypInt) {
				c.errorf(arg.Pos(), "cannot assign %s to parameter 'capacity' of type int in constructor for channel", argType)
			}
		}
		return inst
	}
	if origin == types.TypVector {
		// Vector[T]() or Vector[T](capacity: n) — at most 1 arg
		if len(e.Args) > 1 {
			c.errorf(e.Pos(), "Vector constructor expects at most 1 argument, got %d", len(e.Args))
		}
		for _, arg := range e.Args {
			argType := c.checkExpr(arg.Value)
			if argType != nil && !types.AssignableTo(argType, types.TypInt) {
				c.errorf(arg.Pos(), "cannot assign %s to parameter 'capacity' of type int in constructor for Vector", argType)
			}
		}
		return inst
	}
	if origin == types.TypArc {
		// Arc[T](value) — exactly 1 argument of type T
		if len(e.Args) != 1 {
			c.errorf(e.Pos(), "Arc constructor expects exactly 1 argument, got %d", len(e.Args))
			return inst
		}
		argType := c.checkExpr(e.Args[0].Value)
		elemType := inst.TypeArgs()[0]
		if argType != nil && !types.AssignableTo(argType, elemType) {
			c.errorf(e.Args[0].Pos(), "cannot assign %s to parameter 'value' of type %s in constructor for Arc", argType, elemType)
		}
		return inst
	}

	if origin == types.TypMutex {
		// Mutex[T](value) — exactly 1 argument of type T
		if len(e.Args) != 1 {
			c.errorf(e.Pos(), "Mutex constructor expects exactly 1 argument, got %d", len(e.Args))
			return inst
		}
		argType := c.checkExpr(e.Args[0].Value)
		elemType := inst.TypeArgs()[0]
		if argType != nil && !types.AssignableTo(argType, elemType) {
			c.errorf(e.Args[0].Pos(), "cannot assign %s to parameter 'value' of type %s in constructor for Mutex", argType, elemType)
		}
		return inst
	}

	if origin == types.TypMutexGuard {
		c.errorf(e.Pos(), "MutexGuard cannot be constructed directly; use Mutex.lock()")
		return inst
	}

	if origin == types.TypWeak {
		c.errorf(e.Pos(), "Weak cannot be constructed directly; use Arc.downgrade()")
		return inst
	}

	subst := types.BuildSubstMap(origin.TypeParams(), inst.TypeArgs())
	if subst == nil {
		subst = make(map[*types.TypeParam]types.Type)
	}

	// Merge parent type param substitutions for inherited fields from generic parents.
	// Recurse transitively: GLeaf[int] is GMid[T] is GBase[T] needs all params resolved.
	c.mergeParentSubstSema(origin, subst)

	// If the type has an explicit new() constructor, route through parameter checking
	if origin.HasNew() {
		c.checkNewConstructorCall(e, origin, subst)
		c.recordInstance(inst)
		c.recordType(e.Callee, inst)
		return inst
	}

	c.resolveImplicitConstructorArgs(e, origin, subst)

	// Record the Instance so collectUnresolvedInstances can find it in info.Types
	// when the Instance contains TypeParams (e.g., AppError[T] inside a generic
	// function body). Also record in info.Instances for concrete instances. (B0134)
	c.recordInstance(inst)
	c.recordType(e.Callee, inst)

	return inst
}

// isMemberPropertyCall detects when a field/getter is being called as a method
// (e.g. v.len()) and emits a helpful error. Returns true if the error was emitted.
func (c *Checker) isMemberPropertyCall(e *ast.CallExpr) bool {
	mem, ok := e.Callee.(*ast.MemberExpr)
	if !ok {
		return false
	}

	// Check for module-level getter being called as a function: mod.getter()
	if ident, ok := mem.Target.(*ast.IdentExpr); ok {
		if obj := c.lookup(ident.Name); obj != nil {
			if mod, ok := obj.(*types.Module); ok {
				if scope := mod.Scope(); scope != nil {
					if fobj := scope.Lookup(mem.Field); fobj != nil {
						if fn, ok := fobj.(*types.Func); ok && fn.IsGetter() {
							c.errorf(e.Pos(), "'%s' is a property on module '%s', not a function — remove ()", mem.Field, mod.Name())
							return true
						}
					}
				}
			}
		}
	}

	// Check the target type of the member expression to see if the field is
	// a property (field/getter), not a type/module namespace access.
	targetType := c.info.Types[mem.Target]
	if targetType == nil {
		return false
	}
	// Unwrap references
	if ref, ok := targetType.(*types.MutRef); ok {
		targetType = ref.Elem()
	}
	if ref, ok := targetType.(*types.SharedRef); ok {
		targetType = ref.Elem()
	}
	// Check if the member is a field or getter on the target type
	var isProperty bool
	switch tt := targetType.(type) {
	case *types.Named:
		isProperty = tt.LookupField(mem.Field) != nil || tt.LookupGetter(mem.Field) != nil
	case *types.Instance:
		if origin, ok := tt.Origin().(*types.Named); ok {
			isProperty = origin.LookupField(mem.Field) != nil || origin.LookupGetter(mem.Field) != nil
		}
	case *types.Array:
		isProperty = types.TypVector.LookupField(mem.Field) != nil || types.TypVector.LookupGetter(mem.Field) != nil
	}
	if isProperty {
		c.errorf(e.Pos(), "'%s' is a property on %s, not a method — remove ()", mem.Field, targetType)
		return true
	}
	return false
}

func (c *Checker) checkCallExpr(e *ast.CallExpr) types.Type {
	// Handle super() calls in constructor bodies
	if ident, ok := e.Callee.(*ast.IdentExpr); ok && ident.Name == "super" {
		return c.checkSuperCall(e)
	}

	// T0685: Bare `T[]` is a legitimate type ref when it's the callee
	// (e.g., `int[]()` empty-vector construction). Grant the permission
	// before recursing; checkExpr snapshots+clears it so it doesn't leak.
	c.sliceTypeAllowed = true
	calleeType := c.checkExpr(e.Callee)
	if calleeType == nil {
		return nil
	}

	// T0482: gate Vector.push of an implicit-clone (non-consuming) source
	// whose element type transitively owns a single-owner handle nested in a
	// user-type field / enum variant.
	c.checkPushNestedHandleArg(e)

	// Same-file getter called as function: greeting() → error
	if ident, ok := e.Callee.(*ast.IdentExpr); ok {
		if obj := c.lookup(ident.Name); obj != nil {
			if fn, ok := obj.(*types.Func); ok && fn.IsGetter() {
				c.errorf(e.Pos(), "'%s' is a property, not a function — remove ()", ident.Name)
				return nil
			}
		}
	}

	// Handle constructor calls: Type(field: value, ...)
	// But detect when a field/getter is being called as a method (e.g. v.len()).
	switch t := calleeType.(type) {
	case *types.Named:
		if c.isMemberPropertyCall(e) {
			return nil
		}
		return c.checkConstructorCall(e, t)
	case *types.Instance:
		if c.isMemberPropertyCall(e) {
			return nil
		}
		return c.checkInstanceConstructorCall(e, t)
	case *types.Enum:
		// Enum constructors aren't called directly (use Enum.Variant syntax)
		c.errorf(e.Pos(), "cannot construct enum %s directly; use Enum.Variant syntax", t)
		return nil
	}

	sig, ok := calleeType.(*types.Signature)
	if !ok || sig == nil {
		c.errorf(e.Pos(), "cannot call non-function type %s", calleeType)
		return nil
	}

	// T0846: `drop` is the compiler-managed destructor — a reserved name with a
	// fixed `drop(~this)` signature (validateDropMethod). It runs automatically
	// at scope exit; calling it explicitly double-frees (user/heap types) or
	// panics codegen (Vector/MutexGuard). Reject it uniformly here, before
	// codegen. A receiver with no drop method never reaches this point: the
	// `.drop` member fails to resolve in checkMemberExpr and we already returned
	// nil above, so the normal "no member" path is preserved.
	if mem, ok := e.Callee.(*ast.MemberExpr); ok && mem.Field == "drop" && sig.Recv() != nil {
		c.errorf(e.Pos(), "cannot call 'drop' explicitly; drop runs automatically at scope exit — to release early, let the value go out of scope, move it into a `~` parameter, or use `close()` where available")
		return nil
	}

	// Build call description for error messages.
	callDesc := "function"
	if ident, ok := e.Callee.(*ast.IdentExpr); ok {
		callDesc = "function '" + ident.Name + "'"
	} else if mem, ok := e.Callee.(*ast.MemberExpr); ok {
		callDesc = "method '" + mem.Field + "'"
	}

	// Type argument inference: if the signature has type params, try to infer
	// type arguments from the call arguments before proceeding.
	if len(sig.TypeParams()) > 0 {
		inferred := c.inferAndInstantiateCall(e, sig)
		if inferred == nil {
			// Inference failed — error already reported. Still check args
			// so downstream doesn't see unchecked expressions.
			for _, arg := range e.Args {
				c.checkExpr(arg.Value)
			}
			return nil
		}
		sig = inferred
	}

	c.resolveCallArgs(e, sig.Params(), callDesc, nil)

	if sig.CanError() {
		c.info.FailableExprs[e] = true
	}

	if sig.Result() != nil {
		return sig.Result()
	}
	return types.TypVoid
}

func (c *Checker) checkMemberExpr(e *ast.MemberExpr) types.Type {
	// Handle module-qualified access: mod.symbol
	if ident, ok := e.Target.(*ast.IdentExpr); ok {
		if obj := c.lookup(ident.Name); obj != nil {
			if mod, ok := obj.(*types.Module); ok {
				return c.resolveModuleMember(e, mod)
			}
		}
	}

	// T0685: Bare `T[]` is a legitimate type ref when it's a MemberExpr
	// target (e.g., `int[].filled(...)` static factory). Grant the
	// permission before recursing; checkExpr snapshots+clears it.
	c.sliceTypeAllowed = true
	target := c.checkExpr(e.Target)
	if target == nil {
		return nil
	}
	// B0323: Register auto-propagation for failable call targets in member access.
	// Without this, codegen receives the raw failable tuple {i1, value, error}
	// instead of the unwrapped value, causing invalid IR.
	c.checkVarDeclFailable(e.Target)
	// Unwrap references — member access transparently delegates to inner type
	if ref, ok := target.(*types.MutRef); ok {
		target = ref.Elem()
	}
	if ref, ok := target.(*types.SharedRef); ok {
		target = ref.Elem()
	}

	switch t := target.(type) {
	case *types.Named:
		// Build substitution for inherited members from generic parents
		parentSubst := c.buildParentSubstMap(t)
		// Check fields first, then getters, then methods
		if f := t.LookupField(e.Field); f != nil {
			if f.Deprecated() != "" {
				c.warnf(e.Pos(), "use of deprecated field '%s'", e.Field)
			}
			return types.Substitute(f.Type(), parentSubst)
		}
		if g := t.LookupGetter(e.Field); g != nil {
			if g.Deprecated() != "" {
				c.warnf(e.Pos(), "use of deprecated getter '%s'", e.Field)
			}
			if g.Sig().CanError() {
				c.info.FailableExprs[e] = true
			}
			// T0629: getter access on `this` inside a generic type's body
			// resolves to *types.Named (not Instance), bypassing the
			// Instance-branch edge recording. Mirror the T0627 method-branch
			// fix below: record an identity-subst edge so the getter body's
			// clone reqs propagate to the caller; the eventual concrete call
			// site (Instance branch / resolveInstanceMember) supplies real
			// args and triggers validation in propagateCloneReqs. Getters
			// cannot have their own TypeParams, so no method-TypeParam guard
			// is needed.
			if len(t.TypeParams()) > 0 {
				subst := make(map[*types.TypeParam]types.Type, len(t.TypeParams())+len(parentSubst))
				for _, tp := range t.TypeParams() {
					subst[tp] = tp
				}
				for k, v := range parentSubst {
					subst[k] = v
				}
				c.info.GenericCallEdges = append(c.info.GenericCallEdges, GenericCallEdge{
					CallerFunc:   c.curFuncObj,
					CallerMethod: c.curMethodObj,
					CalleeMethod: g,
					Subst:        subst,
					CallPos:      e.Pos(),
				})
			}
			return types.Substitute(g.Sig().Result(), parentSubst)
		}
		if m := t.LookupMethod(e.Field); m != nil {
			if m.Deprecated() != "" {
				c.warnf(e.Pos(), "use of deprecated method '%s'", e.Field)
			}
			// T0627: methods inside a generic type's body access `this` as
			// *types.Named (not Instance), so the Instance-branch edge
			// recording is bypassed for `this.method()` calls. Record an
			// identity-subst edge here so callee clone reqs propagate to the
			// caller; the eventual concrete call site (Instance branch)
			// supplies real args and triggers validation in
			// propagateCloneReqs. Generic-method calls (`this.method[U]()`)
			// flow through inferAndInstantiateCall and don't need this path.
			if len(t.TypeParams()) > 0 && len(m.Sig().TypeParams()) == 0 {
				subst := make(map[*types.TypeParam]types.Type, len(t.TypeParams())+len(parentSubst))
				for _, tp := range t.TypeParams() {
					subst[tp] = tp
				}
				for k, v := range parentSubst {
					subst[k] = v
				}
				c.info.GenericCallEdges = append(c.info.GenericCallEdges, GenericCallEdge{
					CallerFunc:   c.curFuncObj,
					CallerMethod: c.curMethodObj,
					CalleeMethod: m,
					Subst:        subst,
					CallPos:      e.Pos(),
				})
			}
			return types.Substitute(m.Sig(), parentSubst)
		}
		c.errorf(e.Pos(), "type %s has no field or method %s", t, e.Field)
		return nil

	case *types.Enum:
		// For generic enums, auto-instantiate by resolving type param names from scope.
		// E.g., Slot.Empty inside map[K, V] body resolves K, V from scope → Slot[K, V].Empty
		if len(t.TypeParams()) > 0 {
			args := make([]types.Type, len(t.TypeParams()))
			allFound := true
			for i, tp := range t.TypeParams() {
				obj := c.lookup(tp.Obj().Name())
				if obj == nil {
					allFound = false
					break
				}
				tn, ok := obj.(*types.TypeName)
				if !ok {
					allFound = false
					break
				}
				args[i] = tn.Type()
			}
			if allFound {
				inst := types.NewInstance(t, args)
				c.recordInstance(inst)
				subst := types.BuildSubstMap(t.TypeParams(), args)
				return c.resolveEnumMemberInst(e.Pos(), t, e.Field, subst, inst)
			}
		}
		// Check for variant access (Enum.VariantName) — non-generic or unresolvable
		if v := t.LookupVariant(e.Field); v != nil {
			if v.NumFields() == 0 {
				return t
			}
			params := make([]*types.Param, v.NumFields())
			for i, f := range v.Fields() {
				params[i] = types.NewParam(f.Name(), f.Type(), types.RefNone)
			}
			return types.NewSignature(nil, params, t, false)
		}
		if g := t.LookupGetter(e.Field); g != nil {
			if g.Deprecated() != "" {
				c.warnf(e.Pos(), "use of deprecated getter '%s'", e.Field)
			}
			if g.Sig().CanError() {
				c.info.FailableExprs[e] = true
			}
			return g.Sig().Result()
		}
		if m := t.LookupMethod(e.Field); m != nil {
			if m.Deprecated() != "" {
				c.warnf(e.Pos(), "use of deprecated method '%s'", e.Field)
			}
			return m.Sig()
		}
		c.errorf(e.Pos(), "enum %s has no variant or method %s", t, e.Field)
		return nil

	case *types.Instance:
		return c.resolveInstanceMember(e, e.Pos(), t, e.Field)

	case *types.Array:
		// Arrays delegate to TypVector for field/method lookup
		// T0545: array.clone() with a single-owner-handle element is unsound
		// (delegates to the Vector clone path).
		if e.Field == "clone" {
			if c.checkContainerNotCloneable(e.Pos(), t, []types.Type{t.Elem()}, "cloned") {
				return nil
			}
			// T0616: defer the check to the call site when the element type
			// references a TypeParam (generic indirection bypass).
			if (c.curFuncObj != nil || c.curMethodObj != nil) && types.ContainsTypeParam(t.Elem()) {
				c.recordCloneReq(t.Elem(), e.Pos(), "Array[T].clone()")
			}
		}
		subst := types.BuildSubstMap(types.TypVector.TypeParams(), []types.Type{t.Elem()})
		if f := types.TypVector.LookupField(e.Field); f != nil {
			return types.Substitute(f.Type(), subst)
		}
		if g := types.TypVector.LookupGetter(e.Field); g != nil {
			if g.Sig().CanError() {
				c.info.FailableExprs[e] = true
			}
			return types.Substitute(g.Sig().Result(), subst)
		}
		if m := types.TypVector.LookupMethod(e.Field); m != nil {
			if e.Field == "push" || e.Field == "pop" || e.Field == "remove" {
				c.errorf(e.Pos(), "cannot %s on fixed-size array", e.Field)
				return nil
			}
			return types.Substitute(m.Sig(), subst)
		}
		c.errorf(e.Pos(), "type %s has no member %s", t, e.Field)
		return nil

	case *types.TypeParam:
		for _, constraint := range t.Constraints() {
			if cn, ok := constraint.(*types.Named); ok {
				if m := cn.LookupMethod(e.Field); m != nil {
					if m.IsFactory() {
						// Factory: substitute Self→T in return type
						sig := m.Sig()
						result := sig.Result()
						if rn, ok := result.(*types.Named); ok && rn == cn {
							result = t // Self → TypeParam
						}
						return types.NewSignature(nil, sig.Params(), result, sig.CanError())
					}
					return m.Sig()
				}
				if g := cn.LookupGetter(e.Field); g != nil {
					return g.Sig().Result()
				}
			}
		}
		c.errorf(e.Pos(), "type parameter %s has no method or getter %s", t, e.Field)
		return nil

	default:
		c.errorf(e.Pos(), "cannot access member on type %s", target)
		return nil
	}
}

// resolveInstanceMember resolves field/method/variant access on a generic Instance.
func (c *Checker) resolveInstanceMember(expr ast.Expr, pos ast.Pos, inst *types.Instance, name string) types.Type {
	switch origin := inst.Origin().(type) {
	case *types.Named:
		// T0545: clone()/filled() on a container (Vector/Map/Set) whose
		// element/key/value type transitively contains a single-owner handle
		// (Task/Mutex/MutexGuard) is unsound — those handles are move-only with
		// no clone semantics, and duplicating them double-frees at drop. Reject
		// at resolution so method-value references are caught too.
		if name == "clone" || name == "filled" {
			if elemTypes := singleOwnerContainerElemTypes(origin, inst.TypeArgs()); elemTypes != nil {
				opName := "cloned"
				if name == "filled" {
					opName = "filled"
				}
				if c.checkContainerNotCloneable(pos, inst, elemTypes, opName) {
					return nil
				}
				// T0616: when checking a generic body, record a deferred check
				// for each element type that references a TypeParam so the
				// concrete call site can reject `T = Task[..]` / `Mutex[..]` /
				// `MutexGuard[..]`. The direct gate above only fires when args
				// are already concrete; generic indirection slips through it
				// because generic bodies are checked once with unbound params.
				if c.curFuncObj != nil || c.curMethodObj != nil {
					originName := "container"
					if obj := origin.Obj(); obj != nil {
						originName = obj.Name()
					}
					for _, et := range elemTypes {
						if types.ContainsTypeParam(et) {
							c.recordCloneReq(et, pos, fmt.Sprintf("%s[...].%s()", originName, name))
						}
					}
				}
			}
		}
		subst := types.BuildSubstMap(origin.TypeParams(), inst.TypeArgs())
		if f := origin.LookupField(name); f != nil {
			if f.Deprecated() != "" {
				c.warnf(pos, "use of deprecated field '%s'", name)
			}
			if !hasOwnField(origin, name) {
				subst = c.composeParentSubst(origin, inst.TypeArgs(), name, memberField)
			}
			return types.Substitute(f.Type(), subst)
		}
		if g := origin.LookupGetter(name); g != nil {
			if g.Deprecated() != "" {
				c.warnf(pos, "use of deprecated getter '%s'", name)
			}
			if g.Sig().CanError() {
				c.info.FailableExprs[expr] = true
			}
			if !hasOwnGetter(origin, name) {
				subst = c.composeParentSubst(origin, inst.TypeArgs(), name, memberGetter)
			}
			// T0629: getter access on a parametric receiver from concrete
			// code doesn't flow through inferAndInstantiateCall, so record
			// the edge here using the owner's substitution (mirrors the
			// T0616 method branch below). Getters cannot have their own
			// TypeParams, so no method-TypeParam guard is needed.
			if len(subst) > 0 {
				c.info.GenericCallEdges = append(c.info.GenericCallEdges, GenericCallEdge{
					CallerFunc:   c.curFuncObj,
					CallerMethod: c.curMethodObj,
					CalleeMethod: g,
					Subst:        subst,
					CallPos:      pos,
				})
			}
			return types.Substitute(g.Sig().Result(), subst)
		}
		if m := origin.LookupMethod(name); m != nil {
			if m.Deprecated() != "" {
				c.warnf(pos, "use of deprecated method '%s'", name)
			}
			if !hasOwnMethod(origin, name) {
				subst = c.composeParentSubst(origin, inst.TypeArgs(), name, memberMethod)
			}
			// T0616: non-generic methods on parametric receivers don't go
			// through inferAndInstantiateCall, so record the edge here using
			// the owner's substitution. Methods that have their own
			// TypeParams are handled by inferAndInstantiateCall, which
			// merges the owner subst with the method subst — see
			// checkCallSiteCloneReqs.
			if len(subst) > 0 && len(m.Sig().TypeParams()) == 0 {
				c.info.GenericCallEdges = append(c.info.GenericCallEdges, GenericCallEdge{
					CallerFunc:   c.curFuncObj,
					CallerMethod: c.curMethodObj,
					CalleeMethod: m,
					Subst:        subst,
					CallPos:      pos,
				})
			}
			return types.Substitute(m.Sig(), subst)
		}
		c.errorf(pos, "type %s has no field or method %s", inst, name)
		return nil

	case *types.Enum:
		subst := types.BuildSubstMap(origin.TypeParams(), inst.TypeArgs())
		// Override the result type so variant values/constructors return Instance, not raw Enum
		return c.resolveEnumMemberInst(pos, origin, name, subst, inst)

	default:
		c.errorf(pos, "cannot access member on type %s", inst)
		return nil
	}
}

type memberKind int

const (
	memberField memberKind = iota
	memberGetter
	memberMethod
)

// hasOwnField checks if a Named type has the field directly declared (not inherited).
func hasOwnField(n *types.Named, name string) bool {
	for _, f := range n.Fields() {
		if f.Name() == name {
			return true
		}
	}
	return false
}

// hasOwnGetter checks if a Named type has the getter directly declared (not inherited).
func hasOwnGetter(n *types.Named, name string) bool {
	for _, m := range n.Methods() {
		if m.IsGetter() && m.Name() == name {
			return true
		}
	}
	return false
}

// hasOwnMethod checks if a Named type has the method directly declared (not inherited).
func hasOwnMethod(n *types.Named, name string) bool {
	for _, m := range n.Methods() {
		if m.Name() == name && !m.IsGetter() && !m.IsSetter() {
			return true
		}
	}
	return false
}

// composeParentSubst builds a composed substitution map for accessing a member
// inherited from a generic parent. Walks the parent chain to find the declaring
// type, composing type parameter substitutions through each ParentRef.
//
// Example: Range[T] is Stream[T], Stream has method next() T?
// For Range[int].next(), builds {Stream.T → int} by composing:
//
//	Range.T → int (from inst type args) + Stream.T → Range.T (from ParentRef)
func (c *Checker) composeParentSubst(origin *types.Named, childTypeArgs []types.Type, name string, kind memberKind) map[*types.TypeParam]types.Type {
	childSubst := types.BuildSubstMap(origin.TypeParams(), childTypeArgs)
	return composeParentSubstWalk(origin, childSubst, name, kind)
}

func composeParentSubstWalk(n *types.Named, currentSubst map[*types.TypeParam]types.Type, name string, kind memberKind) map[*types.TypeParam]types.Type {
	for _, pr := range n.Parents() {
		parent := pr.Named
		var found bool
		switch kind {
		case memberField:
			found = parent.LookupField(name) != nil
		case memberGetter:
			found = parent.LookupGetter(name) != nil
		case memberMethod:
			found = parent.LookupMethod(name) != nil
		}
		if !found {
			continue
		}

		if len(pr.TypeArgs) == 0 || len(parent.TypeParams()) == 0 {
			return currentSubst
		}

		// Compose: substitute child's type args into parent's type args,
		// then build a map from parent's type params to the resolved args
		resolvedParentArgs := make([]types.Type, len(pr.TypeArgs))
		for i, ta := range pr.TypeArgs {
			resolvedParentArgs[i] = types.Substitute(ta, currentSubst)
		}
		parentSubst := types.BuildSubstMap(parent.TypeParams(), resolvedParentArgs)

		// If the member is directly on this parent, return the parent's substitution
		switch kind {
		case memberField:
			if hasOwnField(parent, name) {
				return parentSubst
			}
		case memberGetter:
			if hasOwnGetter(parent, name) {
				return parentSubst
			}
		case memberMethod:
			if hasOwnMethod(parent, name) {
				return parentSubst
			}
		}

		// Member is further up the chain — recurse
		return composeParentSubstWalk(parent, parentSubst, name, kind)
	}
	return currentSubst
}

// buildParentSubstMap builds a substitution map from generic parent type params
// to their concrete type args for a Named type. Recursively walks the entire parent
// chain so transitive inheritance (Leaf is Middle[int] is Base[T]) resolves all params.
func (c *Checker) buildParentSubstMap(named *types.Named) map[*types.TypeParam]types.Type {
	subst := make(map[*types.TypeParam]types.Type)
	c.mergeParentSubstSema(named, subst)
	if len(subst) == 0 {
		return nil
	}
	return subst
}

// mergeParentSubstSema recursively adds parent type param mappings to subst.
func (c *Checker) mergeParentSubstSema(named *types.Named, subst map[*types.TypeParam]types.Type) {
	for _, pr := range named.Parents() {
		if len(pr.TypeArgs) == 0 {
			// Non-generic parent — still recurse for its parents.
			c.mergeParentSubstSema(pr.Named, subst)
			continue
		}
		resolvedArgs := make([]types.Type, len(pr.TypeArgs))
		for i, ta := range pr.TypeArgs {
			resolvedArgs[i] = types.Substitute(ta, subst)
		}
		parentMap := types.BuildSubstMap(pr.Named.TypeParams(), resolvedArgs)
		for k, v := range parentMap {
			subst[k] = v
		}
		// Recurse into parent's parents for transitive chains.
		c.mergeParentSubstSema(pr.Named, subst)
	}
}

// resolveEnumMemberInst resolves variant/method on a generic enum, using inst as return type.
func (c *Checker) resolveEnumMemberInst(pos ast.Pos, enum *types.Enum, name string, subst map[*types.TypeParam]types.Type, inst *types.Instance) types.Type {
	if v := enum.LookupVariant(name); v != nil {
		if v.NumFields() == 0 {
			return inst
		}
		params := make([]*types.Param, v.NumFields())
		for i, f := range v.Fields() {
			params[i] = types.NewParam(f.Name(), types.Substitute(f.Type(), subst), types.RefNone)
		}
		return types.NewSignature(nil, params, inst, false)
	}
	if g := enum.LookupGetter(name); g != nil {
		if g.Deprecated() != "" {
			c.warnf(pos, "use of deprecated getter '%s'", name)
		}
		// T0629: enum getter access. Both the via-`this` path (identity
		// subst from checkMemberExpr's *types.Enum auto-instantiation) and
		// the concrete-code path (concrete subst from resolveInstanceMember's
		// enum-origin branch) converge here. Record an edge so the getter
		// body's clone reqs propagate; the eventual concrete call site
		// triggers validation in propagateCloneReqs. Getters cannot have
		// their own TypeParams, so no method-TypeParam guard is needed.
		if len(subst) > 0 {
			c.info.GenericCallEdges = append(c.info.GenericCallEdges, GenericCallEdge{
				CallerFunc:   c.curFuncObj,
				CallerMethod: c.curMethodObj,
				CalleeMethod: g,
				Subst:        subst,
				CallPos:      pos,
			})
		}
		return types.Substitute(g.Sig().Result(), subst)
	}
	if m := enum.LookupMethod(name); m != nil {
		if m.Deprecated() != "" {
			c.warnf(pos, "use of deprecated method '%s'", name)
		}
		// T0629: enum non-generic method access — same path convergence as
		// the getter branch above. Generic methods (`m[U]()`) flow through
		// inferAndInstantiateCall, so guard on no method TypeParams.
		if len(subst) > 0 && len(m.Sig().TypeParams()) == 0 {
			c.info.GenericCallEdges = append(c.info.GenericCallEdges, GenericCallEdge{
				CallerFunc:   c.curFuncObj,
				CallerMethod: c.curMethodObj,
				CalleeMethod: m,
				Subst:        subst,
				CallPos:      pos,
			})
		}
		return types.Substitute(m.Sig(), subst)
	}
	c.errorf(pos, "enum %s has no variant or method %s", inst, name)
	return nil
}

func (c *Checker) checkIndexExpr(e *ast.IndexExpr) types.Type {
	target := c.checkExpr(e.Target)
	if target == nil {
		return nil
	}
	// B0323: Register auto-propagation for failable call targets in index access.
	c.checkVarDeclFailable(e.Target)

	// Generic instantiation: Type[Arg] or func[Arg] in expression context.
	// Only treat [index] as type argument when the target is a type name
	// reference (e.g., Vector[int]), NOT a value of a generic type
	// (e.g., this[i] inside Vector[T]'s method body).
	isTypeRef := false
	switch t := e.Target.(type) {
	case *ast.IdentExpr:
		if obj, found := c.info.Objects[t]; found {
			_, isTypeRef = obj.(*types.TypeName)
		}
	case *ast.MemberExpr:
		// Module-qualified type: mathlib.Pair[int, string]
		switch typ := target.(type) {
		case *types.Named:
			isTypeRef = len(typ.TypeParams()) > 0
		case *types.Enum:
			isTypeRef = len(typ.TypeParams()) > 0
		}
	}
	if isTypeRef {
		switch t := target.(type) {
		case *types.Named:
			if len(t.TypeParams()) > 0 {
				return c.instantiateFromIndex(e, t, t.TypeParams())
			}
		case *types.Enum:
			if len(t.TypeParams()) > 0 {
				return c.instantiateFromIndex(e, t, t.TypeParams())
			}
		}
	}
	// Generic function instantiation: func[Arg]
	if sig, ok := target.(*types.Signature); ok && sig != nil {
		if len(sig.TypeParams()) > 0 {
			return c.instantiateGenericFunc(e, sig)
		}
	}

	// Multi-index only valid for generic instantiation (handled above).
	if len(e.ExtraIndices) > 0 {
		c.errorf(e.Pos(), "multiple indices not supported for indexing")
		return nil
	}

	index := c.checkExpr(e.Index)

	// Unwrap MutRef/SharedRef for indexing (auto-deref through borrows)
	if ref, ok := target.(*types.MutRef); ok {
		target = ref.Elem()
	}
	if ref, ok := target.(*types.SharedRef); ok {
		target = ref.Elem()
	}

	// Method dispatch: look up [] on Named/Instance types
	var named *types.Named
	var subst map[*types.TypeParam]types.Type

	switch t := target.(type) {
	case *types.Named:
		named = t
	case *types.Instance:
		if origin, ok := t.Origin().(*types.Named); ok {
			named = origin
			subst = types.BuildSubstMap(origin.TypeParams(), t.TypeArgs())
		}
	}

	if named != nil {
		if m := named.LookupMethod("[]"); m != nil {
			sig := m.Sig()
			if subst != nil {
				sig = types.Substitute(sig, subst).(*types.Signature)
			}
			if len(sig.Params()) >= 1 {
				paramType := sig.Params()[0].Type()
				if index != nil && !types.AssignableTo(index, paramType) {
					c.errorf(e.Index.Pos(), "index type mismatch: expected %s, got %s", paramType, index)
				}
			}
			if sig.Result() != nil {
				return sig.Result()
			}
			return types.TypVoid
		}
	}

	// Fallback: Array (structural, not Named)
	if arr, ok := target.(*types.Array); ok {
		if index != nil && !types.Identical(index, types.TypInt) {
			c.errorf(e.Index.Pos(), "array index must be int, got %s", index)
		}
		return arr.Elem()
	}

	c.errorf(e.Pos(), "cannot index type %s", target)
	return nil
}

// isTypeRefExpr returns true if e is a valid type reference (not a value expression).
func (c *Checker) isTypeRefExpr(e ast.Expr) bool {
	switch t := e.(type) {
	case *ast.IdentExpr:
		obj, found := c.info.Objects[t]
		if !found {
			return false
		}
		_, ok := obj.(*types.TypeName)
		return ok
	case *ast.MemberExpr:
		switch c.info.Types[t].(type) {
		case *types.Named, *types.Enum, *types.Instance:
			return true
		}
	case *ast.IndexExpr:
		switch c.info.Types[t].(type) {
		case *types.Named, *types.Enum, *types.Instance:
			return true
		}
	case *ast.SliceTypeExpr:
		return true
	case *ast.TupleLit:
		if len(t.Elements) == 0 {
			return false
		}
		for _, el := range t.Elements {
			if !c.isTypeRefExpr(el) {
				return false
			}
		}
		return true
	}
	return false
}

// checkSliceTypeExpr handles T[] in expression position — desugars to Vector[T].
func (c *Checker) checkSliceTypeExpr(e *ast.SliceTypeExpr) types.Type {
	// T0685: `T[]` is a type expression, not a value. It's only legitimate
	// as the Callee of a CallExpr (`int[]()`) or the Target of a MemberExpr
	// (`int[].filled(...)`). Anywhere else (var decl RHS, function arg,
	// return value, tuple element, etc.) is a sema error. Without this
	// guard, codegen returns nil for the bare form and panics with a nil
	// store. The permission is granted by checkCallExpr/checkMemberExpr
	// before recursing into Callee/Target.
	if !c.sliceTypeAllowed {
		// Still resolve the Inner so its symbols are recorded for downstream
		// passes. Use resolveTypeRef (not checkExpr) so a nested SliceTypeExpr
		// in Inner (e.g., `int[][]` bare) doesn't re-trigger this same error
		// — we already know the outer is wrong; one error is enough.
		c.resolveTypeRef(e.Inner)
		c.errorf(e.Pos(), "bare 'T[]' is not a value; use 'T[]()' to construct an empty vector, or 'T[].filled(...)' for a prefilled one")
		return nil
	}

	// T0685: Inner of a SliceTypeExpr is, by definition, a type ref. Route
	// through resolveTypeRef so nested type-only shapes (SliceTypeExpr,
	// TupleLit, IndexExpr-as-instantiation) propagate the permission
	// uniformly instead of relying on the checkExpr snapshot chain (which
	// loses the flag when crossing a TupleLit element boundary).
	inner := c.resolveTypeRef(e.Inner)
	if inner == nil {
		// T0710: resolveTypeRef now self-reports for an undefined inner ident
		// (and bad slice/tuple shapes), so a nil here always implies an error
		// was already accumulated. No re-walk needed.
		return nil
	}

	if !c.isTypeRefExpr(e.Inner) {
		c.errorf(e.Pos(), "expected type name before [], got value expression")
		return nil
	}

	inst := types.NewVector(inner)
	c.recordInstance(inst)
	return inst
}

func (c *Checker) checkSliceExpr(e *ast.SliceExpr) types.Type {
	target := c.checkExpr(e.Target)
	if target == nil {
		return nil
	}
	// B0323: Register auto-propagation for failable call targets in slice access.
	c.checkVarDeclFailable(e.Target)

	// Unwrap MutRef/SharedRef for slicing (auto-deref through borrows)
	if ref, ok := target.(*types.MutRef); ok {
		target = ref.Elem()
	}
	if ref, ok := target.(*types.SharedRef); ok {
		target = ref.Elem()
	}

	// Method dispatch: look up [:] on Named/Instance types
	var named *types.Named
	var subst map[*types.TypeParam]types.Type

	switch t := target.(type) {
	case *types.Named:
		named = t
	case *types.Instance:
		if origin, ok := t.Origin().(*types.Named); ok {
			named = origin
			subst = types.BuildSubstMap(origin.TypeParams(), t.TypeArgs())
		}
	}

	if named != nil {
		if m := named.LookupMethod("[:]"); m != nil {
			// T0482: slicing always element-dups (the [:] method copies each
			// element into the new container — there is no move form). If the
			// element type transitively owns a single-owner handle
			// (Task/Mutex/MutexGuard) — directly (Vector[Task]) or nested in a
			// user-type field / enum variant (Vector[Holder] where
			// Holder{Task}) — the dup shallow-copies the handle pointer and
			// double-frees at drop. Reject at sema (covers the T0387
			// polymorphic-slice shape too).
			var sliceElem types.Type
			if ev, ok := types.AsVector(target); ok {
				sliceElem = ev
			} else if ea, _, ok := types.AsArray(target); ok {
				sliceElem = ea
			}
			if sliceElem != nil {
				if off := firstNestedSingleOwnerHandle(sliceElem, nil); off != nil {
					c.errorf(e.Pos(), "%s cannot be sliced: it contains %s, a single-owner handle with no clone() semantics (single-owner handles are move-only)",
						target, off)
					return nil
				}
			}
			sig := m.Sig()
			if subst != nil {
				sig = types.Substitute(sig, subst).(*types.Signature)
			}
			params := sig.Params()

			// Validate low bound against first param type
			if e.Low != nil {
				low := c.checkExpr(e.Low)
				if low != nil && len(params) >= 1 {
					paramType := params[0].Type()
					if !types.AssignableTo(low, paramType) {
						c.errorf(e.Low.Pos(), "slice bound type mismatch: expected %s, got %s", paramType, low)
					}
				}
			}
			// Validate high bound against second param type
			if e.High != nil {
				high := c.checkExpr(e.High)
				if high != nil && len(params) >= 2 {
					paramType := params[1].Type()
					if !types.AssignableTo(high, paramType) {
						c.errorf(e.High.Pos(), "slice bound type mismatch: expected %s, got %s", paramType, high)
					}
				}
			}

			if sig.Result() != nil {
				return sig.Result()
			}
			return types.TypVoid
		}
	}

	c.errorf(e.Pos(), "type %s does not support slicing", target)
	return nil
}

// resolveTypeArg resolves a generic type argument and guarantees a sema error
// is accumulated on failure. resolveTypeRef self-reports for every nil-return
// path it currently has (undefined ident, bad slice/tuple shape, value-tuple
// fallback, etc.). This wrapper is a defensive backstop (T0710): if some future
// resolveTypeRef path ever returns nil quietly, we still surface a sema error
// here — guaranteeing os.Exit(1) before codegen — instead of letting a nil type
// flow into monomorphization and panic at typeArgStr ("nil type in generic type
// argument"). The len(c.errors) check ensures we never double-report on top of
// a specific diagnostic already emitted downstream (e.g. "undefined: X").
func (c *Checker) resolveTypeArg(expr ast.Expr) types.Type {
	n := len(c.errors)
	typ := c.resolveTypeRef(expr)
	if typ == nil && len(c.errors) == n {
		c.errorf(expr.Pos(), "cannot resolve type argument")
	}
	return typ
}

// instantiateFromIndex handles Type[Arg] or Type[A, B] in expression context as generic instantiation.
// The index expressions are reinterpreted as type arguments.
func (c *Checker) instantiateFromIndex(e *ast.IndexExpr, origin types.Type, tparams []*types.TypeParam) types.Type {
	// Collect all type arguments: Index + ExtraIndices
	var typeArgs []types.Type
	typeArg := c.resolveTypeArg(e.Index)
	if typeArg == nil {
		return nil
	}
	typeArgs = append(typeArgs, typeArg)
	for _, extra := range e.ExtraIndices {
		arg := c.resolveTypeArg(extra)
		if arg == nil {
			return nil
		}
		typeArgs = append(typeArgs, arg)
	}

	if len(tparams) != len(typeArgs) {
		c.errorf(e.Pos(), "type %s expects %d type arguments, got %d", origin, len(tparams), len(typeArgs))
		return nil
	}

	c.validateConstraints(e.Pos(), origin, typeArgs)
	c.validateSendableInstance(e.Pos(), origin, typeArgs)
	c.validateSingleOwnerContainerInstance(e.Pos(), origin, typeArgs)
	inst := types.NewInstance(origin, typeArgs)
	c.recordInstance(inst)
	return inst
}

// instantiateGenericFunc handles func[Arg] or func[A, B] in expression context
// as generic function instantiation. Returns the substituted signature.
func (c *Checker) instantiateGenericFunc(e *ast.IndexExpr, sig *types.Signature) types.Type {
	// Collect all type arguments: Index + ExtraIndices
	var typeArgs []types.Type
	typeArg := c.resolveTypeArg(e.Index)
	if typeArg == nil {
		return nil
	}
	typeArgs = append(typeArgs, typeArg)
	for _, extra := range e.ExtraIndices {
		arg := c.resolveTypeArg(extra)
		if arg == nil {
			return nil
		}
		typeArgs = append(typeArgs, arg)
	}

	tparams := sig.TypeParams()
	if len(tparams) != len(typeArgs) {
		c.errorf(e.Pos(), "function expects %d type arguments, got %d", len(tparams), len(typeArgs))
		return nil
	}

	// Build substitution map and substitute the signature
	subst := types.BuildSubstMap(tparams, typeArgs)
	monoSig := types.Substitute(sig, subst).(*types.Signature)

	// T0616: validate cloneability requirements for explicit-typearg refs
	// (e.g. `foo[Task[int]]` as a value or partial application).
	c.checkExplicitTypeArgsCloneReqs(e, subst)

	// Record instance for monomorphization
	switch t := e.Target.(type) {
	case *ast.IdentExpr:
		// Generic function instantiation: func[Arg]
		obj := c.lookup(t.Name)
		if fn, ok := obj.(*types.Func); ok {
			c.info.FuncInstances = append(c.info.FuncInstances, &FuncInstance{
				Func:     fn,
				TypeArgs: typeArgs,
				Sig:      monoSig,
			})
		}
	case *ast.MemberExpr:
		// Check if this is a module-qualified generic function call: mod.func[Arg]
		if ident, ok := t.Target.(*ast.IdentExpr); ok {
			if obj := c.info.Objects[ident]; obj != nil {
				if mod, ok := obj.(*types.Module); ok && mod.Scope() != nil {
					// Module-qualified generic function: mod.func[Arg]
					if fnObj := mod.Scope().Lookup(t.Field); fnObj != nil {
						if fn, ok := fnObj.(*types.Func); ok {
							c.info.FuncInstances = append(c.info.FuncInstances, &FuncInstance{
								Func:     fn,
								TypeArgs: typeArgs,
								Sig:      monoSig,
							})
							break
						}
					}
				}
			}
		}
		// Generic method instantiation: obj.method[Arg]
		targetType := c.info.Types[t.Target]
		if ref, ok := targetType.(*types.MutRef); ok {
			targetType = ref.Elem()
		}
		if ref, ok := targetType.(*types.SharedRef); ok {
			targetType = ref.Elem()
		}
		var owner *types.Named
		var ownerEnum *types.Enum
		var ownerInst *types.Instance
		switch tt := targetType.(type) {
		case *types.Named:
			owner = tt
		case *types.Enum:
			ownerEnum = tt
		case *types.Instance:
			switch n := tt.Origin().(type) {
			case *types.Named:
				owner = n
				ownerInst = tt
			case *types.Enum:
				ownerEnum = n
				ownerInst = tt
			}
		}
		// T0639: a generic method invoked on a bare generic owner — e.g. via
		// `this` inside the owner's own method body — has ownerInst == nil, so
		// the recorded MethodInstance would be declared/defined under the bare
		// owner name (NBox.m[Arg]) while the call site builds the per-instance
		// name (NBox[int].m[Arg]) via monoCtx. Synthesize the owner's
		// self-instance so the unresolved→per-instance mono resolution produces
		// the matching name.
		if owner != nil && ownerInst == nil && len(owner.TypeParams()) > 0 {
			ownerInst = selfInstanceOf(owner, owner.TypeParams())
		} else if ownerEnum != nil && ownerInst == nil && len(ownerEnum.TypeParams()) > 0 {
			ownerInst = selfInstanceOf(ownerEnum, ownerEnum.TypeParams())
		}
		if owner != nil {
			if method := owner.LookupMethod(t.Field); method != nil {
				// Find the type that actually declares the method (may be a parent).
				// This is needed for codegen to find the AST MethodDecl.
				defOwner := findMethodDefiner(owner, t.Field)
				// If the method is inherited, OwnerInst must reflect the defining
				// type's instantiation (e.g. Iterator[int] when calling map[R] on
				// _FnIter[int] or on Counter which is Iterator[int]).
				defInst := ownerInst
				if defOwner != owner {
					defInst = findParentInstance(owner, ownerInst, defOwner)
				}
				c.info.MethodInstances = append(c.info.MethodInstances, &MethodInstance{
					Owner:     defOwner,
					OwnerInst: defInst,
					Method:    method,
					TypeArgs:  typeArgs,
					Sig:       monoSig,
				})
			}
		} else if ownerEnum != nil {
			// T0636: generic method on a generic enum instance (or via `this`
			// inside a generic enum body). Enums have no inheritance, so there
			// is no defining-parent resolution to perform.
			if method := ownerEnum.LookupMethod(t.Field); method != nil {
				c.info.MethodInstances = append(c.info.MethodInstances, &MethodInstance{
					OwnerEnum: ownerEnum,
					OwnerInst: ownerInst,
					Method:    method,
					TypeArgs:  typeArgs,
					Sig:       monoSig,
				})
			}
		}
	}

	return monoSig
}

// checkExplicitTypeArgsCloneReqs records a generic call edge for explicit
// type-arg refs (e.g. `foo[Task[int]]`) so the propagation post-pass can
// validate cloneability requirements at the call site (T0616).
func (c *Checker) checkExplicitTypeArgsCloneReqs(e *ast.IndexExpr, subst map[*types.TypeParam]types.Type) {
	if len(subst) == 0 {
		return
	}
	var fn *types.Func
	var method *types.Method
	switch t := e.Target.(type) {
	case *ast.IdentExpr:
		if obj := c.lookup(t.Name); obj != nil {
			if f, ok := obj.(*types.Func); ok {
				fn = f
			}
		}
	case *ast.MemberExpr:
		if ident, ok := t.Target.(*ast.IdentExpr); ok {
			if obj := c.info.Objects[ident]; obj != nil {
				if mod, ok := obj.(*types.Module); ok && mod.Scope() != nil {
					if fnObj := mod.Scope().Lookup(t.Field); fnObj != nil {
						if f, ok := fnObj.(*types.Func); ok {
							fn = f
						}
					}
				}
			}
		}
		if fn == nil {
			targetType := c.info.Types[t.Target]
			if ref, ok := targetType.(*types.MutRef); ok {
				targetType = ref.Elem()
			}
			if ref, ok := targetType.(*types.SharedRef); ok {
				targetType = ref.Elem()
			}
			var owner *types.Named
			var ownerEnum *types.Enum
			switch tt := targetType.(type) {
			case *types.Named:
				owner = tt
			case *types.Enum:
				ownerEnum = tt
			case *types.Instance:
				switch n := tt.Origin().(type) {
				case *types.Named:
					owner = n
				case *types.Enum:
					ownerEnum = n
				}
			}
			if owner != nil {
				if m := owner.LookupMethod(t.Field); m != nil {
					method = m
				}
			} else if ownerEnum != nil {
				// T0636: keep T0616 clone-req propagation symmetric for
				// generic methods on generic enum instances.
				if m := ownerEnum.LookupMethod(t.Field); m != nil {
					method = m
				}
			}
		}
	}
	if fn == nil && method == nil {
		return
	}
	if method != nil {
		if ownerSubst := c.ownerSubstForMethodCall(e.Target); len(ownerSubst) > 0 {
			merged := make(map[*types.TypeParam]types.Type, len(subst)+len(ownerSubst))
			for k, v := range subst {
				merged[k] = v
			}
			for k, v := range ownerSubst {
				merged[k] = v
			}
			subst = merged
		}
	}
	c.info.GenericCallEdges = append(c.info.GenericCallEdges, GenericCallEdge{
		CallerFunc:   c.curFuncObj,
		CallerMethod: c.curMethodObj,
		CalleeFunc:   fn,
		CalleeMethod: method,
		Subst:        subst,
		CallPos:      e.Pos(),
	})
}

// findMethodDefiner walks the parent chain to find the Named type that actually
// declares the method (has it in its own Methods(), not inherited). Returns the
// original type if not found in any parent (fallback).
func findMethodDefiner(named *types.Named, methodName string) *types.Named {
	for _, m := range named.Methods() {
		if m.Name() == methodName {
			return named
		}
	}
	for _, pr := range named.Parents() {
		if pr.Named.LookupMethod(methodName) != nil {
			return findMethodDefiner(pr.Named, methodName)
		}
	}
	return named // fallback
}

// findParentInstance walks the parent chain of ownerNamed (substituting with
// ownerInst when non-nil) to find the instantiation of targetParent.
// Example: findParentInstance(_FnIter, Instance{_FnIter,[int]}, Iterator)
//
//	→ Instance{Iterator,[int]}
//
// Example: findParentInstance(Counter, nil, Iterator) where Counter is Iterator[int]
//
//	→ Instance{Iterator,[int]}
func findParentInstance(ownerNamed *types.Named, ownerInst *types.Instance, targetParent *types.Named) *types.Instance {
	var subst map[*types.TypeParam]types.Type
	if ownerInst != nil && len(ownerNamed.TypeParams()) > 0 {
		subst = types.BuildSubstMap(ownerNamed.TypeParams(), ownerInst.TypeArgs())
	}
	for _, pr := range ownerNamed.Parents() {
		if pr.Named == targetParent && len(pr.TypeArgs) > 0 {
			resolvedArgs := make([]types.Type, len(pr.TypeArgs))
			for i, ta := range pr.TypeArgs {
				if subst != nil {
					resolvedArgs[i] = types.Substitute(ta, subst)
				} else {
					resolvedArgs[i] = ta
				}
			}
			for _, arg := range resolvedArgs {
				if types.ContainsTypeParam(arg) {
					return nil
				}
			}
			return types.NewInstance(targetParent, resolvedArgs)
		}
		// Note: ownerInst is nil in the recursive call, so multi-hop generic chains
		// (e.g. A[T] is B[T] is C[T]) lose the intermediate substitution and return nil.
		// This is a known limitation: only 2-level generic inheritance chains are handled
		// correctly. For 3+ levels, defInst falls back to ownerInst (the concrete caller's
		// instantiation), causing a codegen lookup miss. In practice, std's inheritance
		// chains are at most 2 levels deep, so this path is not currently triggered.
		if result := findParentInstance(pr.Named, nil, targetParent); result != nil {
			return result
		}
	}
	return nil
}

// resolveTypeRef resolves an expression as a type reference.
// Used for type arguments in generic instantiations (e.g., the "int" in func[int]).
func (c *Checker) resolveTypeRef(expr ast.Expr) types.Type {
	if ident, ok := expr.(*ast.IdentExpr); ok {
		obj := c.lookup(ident.Name)
		if obj == nil {
			// T0710: a type argument naming an undefined identifier must be a
			// hard sema error. resolveTypeRef is the single chokepoint every
			// type-arg path funnels through, so reporting here guarantees
			// os.Exit(1) before codegen (which would otherwise panic with
			// "nil type in generic type argument"). Mirrors checkIdentExpr.
			c.errorf(ident.Pos(), "undefined: %s", ident.Name)
			c.suggestForUndefinedIdent(ident.Pos(), ident.Name)
			return nil
		}
		typ := obj.Type()
		c.recordType(expr, typ)
		c.recordObject(ident, obj)
		return typ
	}
	if tre, ok := expr.(*ast.TypeRefExpr); ok {
		t := c.resolveType(tre.Ref)
		if t != nil {
			c.recordType(expr, t)
		}
		return t
	}
	// T0685: SliceTypeExpr (`T[]`) is a valid type ref. Grant the
	// per-position slice-type permission so checkSliceTypeExpr's
	// bare-value guard doesn't fire when we're explicitly resolving
	// a type (e.g., `Wrap[string[]]` type arg, or the Inner of an
	// outer SliceTypeExpr).
	if slice, ok := expr.(*ast.SliceTypeExpr); ok {
		prev := c.sliceTypeAllowed
		c.sliceTypeAllowed = true
		typ := c.checkSliceTypeExpr(slice)
		c.sliceTypeAllowed = prev
		if typ != nil {
			c.recordType(expr, typ)
		}
		return typ
	}
	// T0685: TupleLit in a type-ref position is a tuple type. Recurse
	// element-wise through resolveTypeRef so each element retains
	// type-ref semantics (e.g., `(string[], int)` as a type ref).
	if tup, ok := expr.(*ast.TupleLit); ok && len(tup.Elements) > 0 {
		elems := make([]types.Type, len(tup.Elements))
		allTypeRef := true
		for i, el := range tup.Elements {
			elems[i] = c.resolveTypeRef(el)
			if elems[i] == nil {
				return nil
			}
			if !c.isTypeRefExpr(el) {
				allTypeRef = false
			}
		}
		if allTypeRef {
			t := types.NewTuple(elems)
			c.recordType(expr, t)
			return t
		}
		// Not all elements were type refs — fall through to checkExpr so the
		// usual value-tuple semantics + error reporting apply.
	}
	// Fallback: check the expression normally
	return c.checkExpr(expr)
}

func (c *Checker) checkOptionalChainExpr(e *ast.OptionalChainExpr) types.Type {
	target := c.checkExpr(e.Target)
	if target == nil {
		return nil
	}

	opt, ok := target.(*types.Optional)
	if !ok {
		c.errorf(e.Pos(), "optional chaining requires optional type, got %s", target)
		return nil
	}

	inner := opt.Elem()

	switch t := inner.(type) {
	case *types.Named:
		if f := t.LookupField(e.Field); f != nil {
			return types.NewOptional(f.Type())
		}
		if g := t.LookupGetter(e.Field); g != nil {
			return types.NewOptional(g.Sig().Result())
		}
		if m := t.LookupMethod(e.Field); m != nil {
			return types.NewOptional(m.Sig())
		}
		c.errorf(e.Pos(), "type %s has no field or method %s", t, e.Field)
		return nil

	case *types.Instance:
		result := c.resolveInstanceMember(e, e.Pos(), t, e.Field)
		if result != nil {
			return types.NewOptional(result)
		}
		return nil

	default:
		c.errorf(e.Pos(), "cannot access field on type %s", inner)
		return nil
	}
}

func (c *Checker) checkIsExpr(e *ast.IsExpr) types.Type {
	subjectType := c.checkExpr(e.Expr)
	// Validate pattern references existing types
	switch p := e.Pattern.(type) {
	case *ast.IdentIsPattern:
		// "present" and "absent" are contextual keywords for optional checking
		if p.Name == "present" || p.Name == "absent" {
			if len(p.TypeArgs) > 0 {
				c.errorf(p.Pos(), "'%s' does not take type arguments", p.Name)
			}
			break
		}
		// B0115: reject `is` type checks on primitive subjects (no RTTI)
		if subjectType != nil {
			if named, ok := subjectType.(*types.Named); ok && isPrimitiveOrString(named) {
				c.errorf(e.Pos(), "cannot use 'is' type check on primitive type %s", named.Obj().Name())
				break
			}
		}
		// Generic type pattern: resolve via typeRef
		if len(p.TypeArgs) > 0 {
			ref := &ast.NamedTypeRef{Name: p.Name, TypeArgs: p.TypeArgs}
			resolved := c.resolveType(ref)
			if resolved != nil {
				c.info.IsPatternTypes[p] = resolved
			}
			break
		}
		obj := c.lookup(p.Name)
		if obj != nil {
			// Reject `enumVar is EnumType` — the `is` operator is for inheritance checks,
			// not enum variant testing. But allow `optionalEnumVar is EnumType` which is
			// a valid optional-presence check (the optional wraps an enum value).
			if _, ok := obj.Type().(*types.Enum); ok {
				subjectIsEnum := false
				if subjectType != nil {
					switch st := subjectType.Underlying().(type) {
					case *types.Enum:
						subjectIsEnum = true
					case *types.Instance:
						_, subjectIsEnum = st.Origin().(*types.Enum)
					}
				}
				if subjectIsEnum {
					c.errorf(p.Pos(), "cannot use 'is' to check against enum type %s; use 'match' to test specific variants", p.Name)
				}
			}
		} else {
			// Check if the subject is an enum and the name is a variant.
			// Handles both direct Enum types and generic Instance types (e.g., Option[int]).
			isEnumVariant := false
			if subjectType != nil {
				switch st := subjectType.Underlying().(type) {
				case *types.Enum:
					if st.LookupVariant(p.Name) != nil {
						isEnumVariant = true
					}
				case *types.Instance:
					if en, ok := st.Origin().(*types.Enum); ok {
						if en.LookupVariant(p.Name) != nil {
							isEnumVariant = true
						}
					}
				}
			}
			if !isEnumVariant {
				c.errorf(p.Pos(), "undefined type: %s", p.Name)
				c.suggestForUndefinedType(p.Pos(), p.Name)
			}
		}
	case *ast.DestructureIsPattern:
		// B0115: reject `is` type checks on primitive subjects (no RTTI)
		if subjectType != nil {
			if named, ok := subjectType.(*types.Named); ok && isPrimitiveOrString(named) {
				c.errorf(e.Pos(), "cannot use 'is' type check on primitive type %s", named.Obj().Name())
				break
			}
		}
		c.checkDestructureIsPattern(p, subjectType)
	}
	return types.TypBool
}

// checkDestructureIsPattern validates a destructure is-pattern (e.g., `x is Circle(r)`)
// against the subject type. Works for enum variants and named types.
func (c *Checker) checkDestructureIsPattern(p *ast.DestructureIsPattern, subjectType types.Type) {
	if subjectType == nil {
		return
	}

	// Check if it's an enum variant of the subject type (enum variants never have type args)
	if len(p.TypeArgs) == 0 {
		var enum *types.Enum
		switch st := subjectType.Underlying().(type) {
		case *types.Enum:
			enum = st
		case *types.Instance:
			if e, ok := st.Origin().(*types.Enum); ok {
				enum = e
			}
		}
		if enum != nil {
			if v := enum.LookupVariant(p.TypeName); v != nil {
				if len(p.Bindings) != v.NumFields() {
					c.errorf(p.Pos(), "variant %s has %d fields, got %d bindings",
						p.TypeName, v.NumFields(), len(p.Bindings))
				}
				return
			}
		}
	}

	// Generic type destructure: resolve via typeRef
	if len(p.TypeArgs) > 0 {
		ref := &ast.NamedTypeRef{Name: p.TypeName, TypeArgs: p.TypeArgs}
		resolved := c.resolveType(ref)
		if resolved == nil {
			return
		}
		c.info.IsPatternTypes[p] = resolved
		// Validate field count against the resolved type
		if inst, ok := resolved.(*types.Instance); ok {
			if named, ok := inst.Origin().(*types.Named); ok {
				allFields := named.AllFields()
				if len(p.Bindings) != len(allFields) {
					c.errorf(p.Pos(), "type %s has %d fields, got %d bindings",
						p.TypeName, len(allFields), len(p.Bindings))
				}
			}
		}
		return
	}

	// Not an enum variant — look up as a named type
	obj := c.lookup(p.TypeName)
	if obj == nil {
		c.errorf(p.Pos(), "undefined type: %s", p.TypeName)
		c.suggestForUndefinedType(p.Pos(), p.TypeName)
		return
	}
	tn, ok := obj.(*types.TypeName)
	if !ok {
		c.errorf(p.Pos(), "%s is not a type", p.TypeName)
		return
	}
	named, ok := tn.Type().(*types.Named)
	if !ok {
		c.errorf(p.Pos(), "%s is not a struct type", p.TypeName)
		return
	}
	allFields := named.AllFields()
	if len(p.Bindings) != len(allFields) {
		c.errorf(p.Pos(), "type %s has %d fields, got %d bindings",
			p.TypeName, len(allFields), len(p.Bindings))
	}
}

func (c *Checker) checkCastExpr(e *ast.CastExpr) types.Type {
	c.checkExpr(e.Expr)
	target := c.resolveType(e.Type)
	if target == nil {
		return nil
	}

	// Scalar casts (numeric, char, bool) always succeed — return target type directly (not optional)
	srcType := c.info.Types[e.Expr]
	if isScalarCastType(srcType) && isScalarCastType(target) {
		return target
	}

	if e.Force {
		// as! returns the target type directly (may panic)
		return target
	}
	// as returns optional
	return types.NewOptional(target)
}

func (c *Checker) checkErrorPropagateExpr(e *ast.ErrorPropagateExpr) types.Type {
	inner := c.checkExpr(e.Expr)
	if c.curFunc == nil || !c.curFunc.CanError() {
		c.errorf(e.Pos(), "error propagation (?^) used outside of failable function")
	}
	if !c.info.FailableExprs[e.Expr] {
		c.errorf(e.Pos(), "error propagation (?^) requires a failable expression")
	}
	// The inner expression's type is the success type (error is propagated)
	return inner
}

func (c *Checker) checkErrorPanicExpr(e *ast.ErrorPanicExpr) types.Type {
	inner := c.checkExpr(e.Expr)
	if !c.info.FailableExprs[e.Expr] {
		c.errorf(e.Pos(), "error panic (?!) requires a failable expression")
	}
	// Panic on error, returns success type
	return inner
}

func (c *Checker) checkOptionalUnwrapExpr(e *ast.OptionalUnwrapExpr) types.Type {
	inner := c.checkExpr(e.Expr)
	if c.info.FailableExprs[e.Expr] {
		c.errorf(e.Pos(), "use ?! to panic on failable error (! is for optional unwrap)")
		if c.curFunc != nil && c.curFunc.CanError() {
			c.hintf(e.Pos(), "in a failable function, bare call() auto-propagates errors — no operator needed")
		}
	}
	if opt, ok := inner.(*types.Optional); ok {
		return opt.Elem()
	}
	c.errorf(e.Pos(), "unwrap (!) requires an optional expression")
	return inner
}

// checkAutoCloneExpr type-checks the synth-only AutoCloneExpr intrinsic
// (T0605). It is a transparent wrapper: the result type is exactly the inner
// expression's type. At generic-check time the inner is a TypeParam-containing
// type; codegen lowers it type-directed once the concrete substitution is
// known. The inner type is recorded by checkExpr's recordType tail.
func (c *Checker) checkAutoCloneExpr(e *ast.AutoCloneExpr) types.Type {
	return c.checkExpr(e.Expr)
}

func (c *Checker) checkErrorHandlerExpr(e *ast.ErrorHandlerExpr) types.Type {
	inner := c.checkExpr(e.Expr)
	if !c.info.FailableExprs[e.Expr] {
		// Not failable — check if it's an optional handler (T? ? { } → T)
		if opt, ok := inner.(*types.Optional); ok {
			// Optional handler: no error binding, no typed handler, no else
			if e.TypeName != "" || e.ElseBody != nil {
				c.errorf(e.Pos(), "optional handler does not support typed patterns or else clauses")
			}
			c.info.OptionalHandlers[e] = true
			c.openScope(e.Body, "optional-handler")
			c.checkBlock(e.Body)
			c.closeScope()
			return opt.Elem()
		}
		c.errorf(e.Pos(), "handler (?) requires a failable or optional expression")
	}

	// Validate else/! only on typed handlers
	if e.TypeName == "" && (e.ElseBody != nil || e.PanicOnNomatch) {
		c.errorf(e.Pos(), "else clause and '!' are only valid on typed error handlers (? e is T { })")
	}

	// Determine binding type: specific error subtype or generic error
	var bindingType types.Type = types.TypError
	if e.TypeName != "" {
		// Typed handlers in non-failable functions need else or ! to be exhaustive.
		// Without them, non-matching errors have nowhere to go.
		isExhaustive := e.ElseBody != nil || e.PanicOnNomatch
		if !isExhaustive && (c.curFunc == nil || !c.curFunc.CanError()) {
			c.errorf(e.Pos(), "typed error handler in non-failable function; add 'else { }', '!' suffix, or make function failable")
		}
		// Generic typed handler: resolve via typeRef (e.g., DataError[string])
		if len(e.TypeArgs) > 0 {
			ref := &ast.NamedTypeRef{Name: e.TypeName, TypeArgs: e.TypeArgs}
			resolved := c.resolveType(ref)
			if resolved != nil {
				c.info.ErrorHandlerTypes[e] = resolved
				// Validate the resolved type inherits from error
				switch rt := resolved.(type) {
				case *types.Instance:
					if named, ok := rt.Origin().(*types.Named); ok {
						if !named.InheritsFrom(types.TypError) {
							c.errorf(e.Pos(), "%s does not inherit from error", e.TypeName)
						} else {
							bindingType = resolved
						}
					}
				default:
					c.errorf(e.Pos(), "%s is not a type", e.TypeName)
				}
			}
		} else {
			obj := c.lookup(e.TypeName)
			if obj == nil {
				c.errorf(e.Pos(), "undefined type: %s", e.TypeName)
				c.suggestForUndefinedType(e.Pos(), e.TypeName)
			} else if tn, ok := obj.(*types.TypeName); ok && tn.Type() != nil {
				if named, ok := tn.Type().(*types.Named); ok {
					if !named.InheritsFrom(types.TypError) {
						c.errorf(e.Pos(), "%s does not inherit from error", e.TypeName)
					} else {
						bindingType = named
					}
				} else {
					c.errorf(e.Pos(), "%s is not a type", e.TypeName)
				}
			} else {
				c.errorf(e.Pos(), "%s is not a type", e.TypeName)
			}
		}
	}

	c.openScope(e.Body, "error-handler")
	// Bind error variable if present
	if e.Binding != "" && e.Binding != "_" {
		c.checkNoShadow(e.Binding, e.Pos())
		c.insert(types.NewVar(tpos(e.Pos()), e.Binding, bindingType))
	}
	c.checkBlock(e.Body)
	c.closeScope()

	// Type-check else clause
	if e.ElseBody != nil {
		c.openScope(e.ElseBody, "error-else")
		if e.ElseBinding != "" && e.ElseBinding != "_" {
			c.checkNoShadow(e.ElseBinding, e.Pos())
			c.insert(types.NewVar(tpos(e.Pos()), e.ElseBinding, types.TypError))
		}
		c.checkBlock(e.ElseBody)
		c.closeScope()
	}

	return inner
}

func (c *Checker) checkIfExpr(e *ast.IfExpr) types.Type {
	cond := c.checkExpr(e.Cond)
	if cond != nil && !types.Identical(cond, types.TypBool) {
		c.errorf(e.Cond.Pos(), "if condition must be bool, got %s", cond)
	}

	c.openScope(e.Then, "if-then")
	c.checkBlock(e.Then)
	c.closeScope()

	thenType := c.blockValueType(e.Then)

	c.openScope(e.Else, "if-else")
	c.checkBlock(e.Else)
	c.closeScope()

	elseType := c.blockValueType(e.Else)

	// T0381: if one branch is a borrow (`T&`/`T~`) and the other is owned
	// (`T`), the result is owned — a single dropflag cannot represent
	// ownership that varies by arm, so the conservative choice is the
	// owned form. Otherwise, prefer the then-branch type.
	return c.joinBranchTypes(thenType, elseType, e.Pos())
}

// joinBranchTypes unifies two arm types of an if/match expression. When one
// arm produces `T&`/`T~` and another produces `T`, we strip the borrow to
// the owned form so downstream codegen treats the result as owned (T0381).
//
// T0488: silent decay is unsound for non-Copy `T` — the borrow arm's runtime
// value is the parent Arc/Mutex inner pointer (no clone), so consuming the
// joined value as owned `T` causes UAF/double-free on scope exit. Mirrors
// T0438's Rules 8b/8c by gating the decay on `IsCopy(elem)`. For non-Copy
// elements we emit a sema error and continue with the owned form to avoid
// cascading "cannot assign T& to T" diagnostics.
func (c *Checker) joinBranchTypes(a, b types.Type, pos ast.Pos) types.Type {
	if a == nil {
		return b
	}
	if b == nil {
		return a
	}
	stripped := func(t types.Type) types.Type {
		switch r := t.(type) {
		case *types.SharedRef:
			return r.Elem()
		case *types.MutRef:
			return r.Elem()
		}
		return t
	}
	aIsRef := false
	bIsRef := false
	switch a.(type) {
	case *types.SharedRef, *types.MutRef:
		aIsRef = true
	}
	switch b.(type) {
	case *types.SharedRef, *types.MutRef:
		bIsRef = true
	}
	if aIsRef != bIsRef {
		var refSide types.Type
		if aIsRef {
			refSide = a
		} else {
			refSide = b
		}
		elem := stripped(refSide)
		if !types.IsCopy(elem) {
			c.errorf(pos,
				"if/match arms mix borrowed and owned non-Copy '%s'; "+
					"call .clone() on the borrow arm or change all arms to produce '%s&'",
				elem, elem)
			return elem
		}
		if aIsRef {
			return stripped(a)
		}
		return a
	}
	return a
}

func (c *Checker) checkMatchExpr(e *ast.MatchExpr) types.Type {
	subjectType := c.checkExpr(e.Subject)

	var resultType types.Type
	for _, arm := range e.Arms {
		c.openScope(arm, "match-arm")
		// T0299: Rewrite module-qualified enum patterns before type-checking.
		// The parser creates ExpressionMatchPattern for mod.Type.Variant because
		// ANTLR's prediction prefers the expression alternative. Detect and rewrite
		// to EnumVariantMatchPattern/EnumDestructureMatchPattern before checkMatchPattern
		// would type-check the expression (which would fail for destructure bindings).
		c.rewriteQualifiedEnumPattern(arm)
		c.checkMatchPattern(arm.Pattern, e.Subject, subjectType)
		// B0328: Resolve bare variant names to enum variant patterns.
		// The parser creates NameMatchPattern for bare identifiers like "Red".
		// When matching on an enum subject, if the name matches a variant,
		// rewrite to EnumVariantMatchPattern so codegen emits a proper switch
		// case (not a catch-all default).
		if np, ok := arm.Pattern.(*ast.NameMatchPattern); ok && subjectType != nil {
			if enum := extractEnum(subjectType); enum != nil {
				if enum.LookupVariant(np.Name) != nil {
					evp := &ast.EnumVariantMatchPattern{
						Enum:    enum.Obj().Name(),
						Variant: np.Name,
					}
					evp.SetPosEnd(np.Pos(), np.End())
					arm.Pattern = evp
				}
			}
		}
		c.insertPatternBindings(arm.Pattern, subjectType)

		if arm.Guard != nil {
			gt := c.checkExpr(arm.Guard)
			if gt != nil && !types.Identical(gt, types.TypBool) {
				c.errorf(arm.Guard.Pos(), "match guard must be bool, got %s", gt)
			}
		}

		var armType types.Type
		if arm.Body != nil {
			armType = c.checkExpr(arm.Body)
		} else if arm.Block != nil {
			c.checkBlock(arm.Block)
			// B0126: extract block result type from the last expression
			// statement, so match expressions with block arms are correctly typed.
			armType = c.blockValueType(arm.Block)
		}

		c.closeScope()

		if resultType == nil {
			resultType = armType
		} else {
			// T0381: unify ref/owned mismatches across arms.
			resultType = c.joinBranchTypes(resultType, armType, e.Pos())
		}
	}

	// Check exhaustiveness
	c.checkMatchExhaustiveness(e, subjectType)

	return resultType
}

// blockValueType returns the type of a block's result value — the type of the
// last expression statement. Returns nil if the block doesn't end with an
// expression. Handles IfStmt as the last statement by recursing into its
// then body.
func (c *Checker) blockValueType(block *ast.Block) types.Type {
	if block == nil || len(block.Stmts) == 0 {
		return nil
	}
	last := block.Stmts[len(block.Stmts)-1]
	if es, ok := last.(*ast.ExprStmt); ok {
		return c.info.Types[es.Expr]
	}
	if ifS, ok := last.(*ast.IfStmt); ok && ifS.Else != nil {
		return c.blockValueType(ifS.Body)
	}
	return nil
}

func (c *Checker) checkMatchPattern(pat ast.MatchPattern, subject ast.Expr, subjectType types.Type) {
	if pat == nil {
		return
	}

	switch p := pat.(type) {
	case *ast.EnumDestructureMatchPattern:
		enum, ok := c.resolveEnumForPattern(p.Module, p.Enum, p.Pos())
		if !ok {
			return
		}
		v := enum.LookupVariant(p.Variant)
		if v == nil {
			c.errorf(p.Pos(), "enum %s has no variant %s", p.Enum, p.Variant)
			return
		}
		if len(p.Bindings) != v.NumFields() {
			c.errorf(p.Pos(), "variant %s.%s has %d fields, got %d bindings",
				p.Enum, p.Variant, v.NumFields(), len(p.Bindings))
		}
		// T0482/T0623: a binding that copies out a variant field owning a
		// single-owner handle double-frees unless the subject is an owned
		// local that can be moved (then the binding takes ownership).
		c.checkDestructureNoHandleField(p.Pos(), subject, subjectType, v, p.Bindings, enumDestructureSubst(subjectType, enum))

	case *ast.EnumVariantMatchPattern:
		enum, ok := c.resolveEnumForPattern(p.Module, p.Enum, p.Pos())
		if !ok {
			return
		}
		if enum.LookupVariant(p.Variant) == nil {
			c.errorf(p.Pos(), "enum %s has no variant %s", p.Enum, p.Variant)
		}

	case *ast.TypeBindingMatchPattern:
		obj := c.lookup(p.TypeName)
		if obj == nil {
			c.errorf(p.Pos(), "undefined type: %s", p.TypeName)
			c.suggestForUndefinedType(p.Pos(), p.TypeName)
		}

	case *ast.ShortDestructureMatchPattern:
		// Short form: Ok(val) — check if it's a variant of the match subject enum
		if subjectType != nil {
			var enum *types.Enum
			switch st := subjectType.(type) {
			case *types.Enum:
				enum = st
			case *types.Instance:
				if e, ok := st.Origin().(*types.Enum); ok {
					enum = e
				}
			}
			if enum != nil {
				if v := enum.LookupVariant(p.Name); v != nil {
					if len(p.Bindings) != v.NumFields() {
						c.errorf(p.Pos(), "variant %s has %d fields, got %d bindings",
							p.Name, v.NumFields(), len(p.Bindings))
					}
					// T0482/T0623: same handle-copy double-free / move-out gate as
					// the qualified EnumDestructureMatchPattern form.
					c.checkDestructureNoHandleField(p.Pos(), subject, subjectType, v, p.Bindings, enumDestructureSubst(subjectType, enum))
					return
				}
			}
		}
		// Fallback: look up as a standalone name
		obj := c.lookup(p.Name)
		if obj == nil {
			c.errorf(p.Pos(), "undefined: %s", p.Name)
			c.suggestForUndefinedIdent(p.Pos(), p.Name)
		}

	case *ast.LiteralMatchPattern:
		c.checkExpr(p.Value)

	case *ast.NameMatchPattern:
		// Simple binding — no validation needed

	case *ast.WildcardMatchPattern:
		// Always valid

	case *ast.ExpressionMatchPattern:
		exprType := c.checkExpr(p.Expr)
		if exprType != nil && subjectType != nil && !types.Identical(exprType, subjectType) {
			c.errorf(p.Pos(), "match expression pattern type %s does not match subject type %s", exprType, subjectType)
		}
	}
}

// insertPatternBindings inserts variables from match pattern bindings into the current scope.
// Called after checkMatchPattern has validated the pattern structure.
func (c *Checker) insertPatternBindings(pat ast.MatchPattern, subjectType types.Type) {
	if pat == nil {
		return
	}

	switch p := pat.(type) {
	case *ast.ShortDestructureMatchPattern:
		c.insertDestructureBindings(p.Pos(), p.Bindings, c.lookupVariantFields(p.Name, subjectType))

	case *ast.EnumDestructureMatchPattern:
		c.insertEnumDestructureBindings(p, subjectType)

	case *ast.NameMatchPattern:
		if p.Name != "_" && subjectType != nil {
			c.insert(types.NewVar(tpos(p.Pos()), p.Name, subjectType))
		}

	case *ast.TypeBindingMatchPattern:
		if p.Binding != "_" {
			obj := c.lookup(p.TypeName)
			if obj != nil {
				if tn, ok := obj.(*types.TypeName); ok && tn.Type() != nil {
					c.insert(types.NewVar(tpos(p.Pos()), p.Binding, tn.Type()))
				}
			}
		}
	}
}

// lookupVariantFields returns the field types for a variant matched via short destructure.
// Handles both direct Enum and generic Instance subjects.
func (c *Checker) lookupVariantFields(variantName string, subjectType types.Type) []types.Type {
	if subjectType == nil {
		return nil
	}
	var enum *types.Enum
	var subst map[*types.TypeParam]types.Type

	switch st := subjectType.(type) {
	case *types.Enum:
		enum = st
	case *types.Instance:
		if e, ok := st.Origin().(*types.Enum); ok {
			enum = e
			subst = types.BuildSubstMap(e.TypeParams(), st.TypeArgs())
		}
	}
	if enum == nil {
		return nil
	}
	v := enum.LookupVariant(variantName)
	if v == nil {
		return nil
	}
	fieldTypes := make([]types.Type, v.NumFields())
	for i, f := range v.Fields() {
		ft := f.Type()
		if subst != nil {
			ft = types.Substitute(ft, subst)
		}
		fieldTypes[i] = ft
	}
	return fieldTypes
}

// insertDestructureBindings inserts bindings with corresponding field types into scope.
func (c *Checker) insertDestructureBindings(pos ast.Pos, bindings []string, fieldTypes []types.Type) {
	if fieldTypes == nil {
		return
	}
	n := len(bindings)
	if n > len(fieldTypes) {
		n = len(fieldTypes)
	}
	for i := 0; i < n; i++ {
		if bindings[i] != "_" {
			c.insert(types.NewVar(tpos(pos), bindings[i], fieldTypes[i]))
		}
	}
}

// rewriteQualifiedEnumPattern detects ExpressionMatchPattern nodes that contain
// module-qualified enum variant patterns (mod.Type.Variant or mod.Type.Variant(a,b))
// and rewrites them to EnumVariantMatchPattern / EnumDestructureMatchPattern.
// This must run BEFORE checkMatchPattern to prevent the expression type-checker
// from treating destructure bindings as undefined variable references.
func (c *Checker) rewriteQualifiedEnumPattern(arm *ast.MatchArm) {
	ep, ok := arm.Pattern.(*ast.ExpressionMatchPattern)
	if !ok {
		return
	}

	// Case 1: mod.Type.Variant(bindings) — call expression on 3-part member access
	if call, ok := ep.Expr.(*ast.CallExpr); ok {
		mod, enum, variant, ok := c.extractQualifiedEnumChain(call.Callee)
		if !ok {
			return
		}
		// Verify bindings are all simple positional identifiers
		var bindings []string
		for _, arg := range call.Args {
			if arg.Name != "" {
				return // named args aren't destructure bindings
			}
			ident, ok := arg.Value.(*ast.IdentExpr)
			if !ok {
				return // not a destructure pattern
			}
			bindings = append(bindings, ident.Name)
		}
		p := &ast.EnumDestructureMatchPattern{
			Module:   mod,
			Enum:     enum,
			Variant:  variant,
			Bindings: bindings,
		}
		p.SetPosEnd(ep.Pos(), ep.End())
		arm.Pattern = p
		return
	}

	// Case 2: mod.Type.Variant — 3-part member access chain
	mod, enum, variant, ok := c.extractQualifiedEnumChain(ep.Expr)
	if !ok {
		return
	}
	p := &ast.EnumVariantMatchPattern{
		Module:  mod,
		Enum:    enum,
		Variant: variant,
	}
	p.SetPosEnd(ep.Pos(), ep.End())
	arm.Pattern = p
}

// extractQualifiedEnumChain checks if an expression is a 3-part member access
// chain (a.B.C) where a is a module and B is an enum in that module with variant C.
// Returns (module, enum, variant, true) if valid, or ("", "", "", false) otherwise.
func (c *Checker) extractQualifiedEnumChain(expr ast.Expr) (string, string, string, bool) {
	// Must be MemberExpr: ?.Variant
	outer, ok := expr.(*ast.MemberExpr)
	if !ok {
		return "", "", "", false
	}
	variant := outer.Field

	// Target must be MemberExpr: ?.Enum
	inner, ok := outer.Target.(*ast.MemberExpr)
	if !ok {
		return "", "", "", false
	}
	enumName := inner.Field

	// Target must be IdentExpr: module
	ident, ok := inner.Target.(*ast.IdentExpr)
	if !ok {
		return "", "", "", false
	}
	modName := ident.Name

	// Verify: modName is a module, enumName is an enum in it, variant exists
	modObj := c.lookup(modName)
	if modObj == nil {
		return "", "", "", false
	}
	mod, ok := modObj.(*types.Module)
	if !ok {
		return "", "", "", false
	}
	if mod.Scope() == nil {
		return "", "", "", false
	}
	typeObj := mod.Scope().Lookup(enumName)
	if typeObj == nil {
		return "", "", "", false
	}
	tn, ok := typeObj.(*types.TypeName)
	if !ok {
		return "", "", "", false
	}
	enum, ok := tn.Type().(*types.Enum)
	if !ok {
		return "", "", "", false
	}
	if enum.LookupVariant(variant) == nil {
		return "", "", "", false
	}
	return modName, enumName, variant, true
}

// resolveEnumForPattern resolves an enum type from a pattern, handling module-qualified names.
func (c *Checker) resolveEnumForPattern(module, name string, pos ast.Pos) (*types.Enum, bool) {
	var obj types.Object
	if module != "" {
		modObj := c.lookup(module)
		if modObj == nil {
			c.errorf(pos, "undefined: %s", module)
			return nil, false
		}
		mod, ok := modObj.(*types.Module)
		if !ok {
			c.errorf(pos, "%s is not a module", module)
			return nil, false
		}
		if mod.Scope() == nil {
			c.errorf(pos, "module '%s' has no loaded scope", module)
			return nil, false
		}
		obj = mod.Scope().Lookup(name)
		if obj == nil {
			c.errorf(pos, "module '%s' has no type '%s'", module, name)
			return nil, false
		}
	} else {
		obj = c.lookup(name)
		if obj == nil {
			c.errorf(pos, "undefined: %s", name)
			c.suggestForUndefinedIdent(pos, name)
			return nil, false
		}
	}
	tn, ok := obj.(*types.TypeName)
	if !ok {
		c.errorf(pos, "%s is not a type", name)
		return nil, false
	}
	enum, ok := tn.Type().(*types.Enum)
	if !ok {
		c.errorf(pos, "%s is not an enum type", name)
		return nil, false
	}
	return enum, true
}

// insertEnumDestructureBindings handles Enum.Variant(a, b) pattern bindings.
// Uses subjectType to build a substitution map for generic enum instances.
// Errors are not emitted here — checkMatchPattern already validated the pattern.
func (c *Checker) insertEnumDestructureBindings(p *ast.EnumDestructureMatchPattern, subjectType types.Type) {
	// Silent lookup — errors were already emitted by checkMatchPattern.
	var obj types.Object
	if p.Module != "" {
		modObj := c.lookup(p.Module)
		if modObj == nil {
			return
		}
		mod, ok := modObj.(*types.Module)
		if !ok || mod.Scope() == nil {
			return
		}
		obj = mod.Scope().Lookup(p.Enum)
	} else {
		obj = c.lookup(p.Enum)
	}
	if obj == nil {
		return
	}
	tn, ok := obj.(*types.TypeName)
	if !ok {
		return
	}
	enum, ok := tn.Type().(*types.Enum)
	if !ok {
		return
	}
	v := enum.LookupVariant(p.Variant)
	if v == nil {
		return
	}

	// Build substitution map if the subject is a generic instance of this enum
	var subst map[*types.TypeParam]types.Type
	if inst, ok := subjectType.(*types.Instance); ok {
		if origin, ok := inst.Origin().(*types.Enum); ok && origin == enum {
			subst = types.BuildSubstMap(origin.TypeParams(), inst.TypeArgs())
		}
	}

	n := len(p.Bindings)
	if n > v.NumFields() {
		n = v.NumFields()
	}
	for i := 0; i < n; i++ {
		if p.Bindings[i] != "_" {
			ft := v.Fields()[i].Type()
			if subst != nil {
				ft = types.Substitute(ft, subst)
			}
			c.insert(types.NewVar(tpos(p.Pos()), p.Bindings[i], ft))
		}
	}
}

func (c *Checker) checkLambdaExpr(e *ast.LambdaExpr) types.Type {
	params := make([]*types.Param, len(e.Params))
	for i, p := range e.Params {
		var pt types.Type
		if p.Type != nil {
			pt = c.resolveType(p.Type)
		}
		if pt == nil {
			c.errorf(p.Pos(), "lambda parameter %s requires a type annotation", p.Name)
			return nil
		}
		params[i] = types.NewParam(p.Name, pt, resolveRefMod(p.RefMod))
	}

	var result types.Type
	if e.ReturnType != nil {
		result = c.resolveType(e.ReturnType)
	}

	sig := types.NewSignature(nil, params, result, false)

	// Save lambda capture state
	savedLambdaDepth := c.lambdaDepth
	savedLambdaCaptures := c.lambdaCaptures
	savedLambdaScope := c.lambdaScope
	savedLambdaMove := c.lambdaMove

	c.lambdaDepth++
	c.lambdaCaptures = make(map[string]*CapturedVar)
	c.lambdaScope = c.scope // scope at lambda definition site
	c.lambdaMove = e.Move

	// Type-check body
	saved := c.curFunc
	c.curFunc = sig
	defer func() { c.curFunc = saved }()

	if e.Body != nil {
		c.openScope(e.Body, "lambda")
		for _, p := range params {
			if p.Name() != "" && p.Name() != "_" {
				c.checkNoShadow(p.Name(), e.Pos())
				c.insert(types.NewVar(tpos(e.Pos()), p.Name(), p.Type()))
			}
		}
		c.checkBlock(e.Body)
		c.closeScope()
	} else if e.ExprBody != nil {
		// Expression body — bind params into scope so they're accessible
		c.openScope(e, "lambda")
		for _, p := range params {
			if p.Name() != "" && p.Name() != "_" {
				c.checkNoShadow(p.Name(), e.Pos())
				c.insert(types.NewVar(tpos(e.Pos()), p.Name(), p.Type()))
			}
		}
		bodyType := c.checkExpr(e.ExprBody)
		c.closeScope()
		if result == nil && bodyType != nil {
			// Infer return type from expression body
			sig = types.NewSignature(nil, params, bodyType, false)
		}
	}

	// Record captured variables for this lambda in deterministic order (by name).
	// Map iteration order is non-deterministic; sorting ensures reproducible env struct layout.
	captureNames := make([]string, 0, len(c.lambdaCaptures))
	for name := range c.lambdaCaptures {
		captureNames = append(captureNames, name)
	}
	sort.Strings(captureNames)
	captures := make([]*CapturedVar, 0, len(captureNames))
	for _, name := range captureNames {
		captures = append(captures, c.lambdaCaptures[name])
	}
	c.info.LambdaCaptures[e] = captures

	// Restore lambda capture state
	c.lambdaDepth = savedLambdaDepth
	c.lambdaCaptures = savedLambdaCaptures
	c.lambdaScope = savedLambdaScope
	c.lambdaMove = savedLambdaMove

	// Propagate inner lambda captures to the enclosing lambda.
	// If the inner lambda captured a variable from a grandparent scope,
	// the enclosing lambda must also capture it to make it available.
	if c.lambdaDepth > 0 && len(captures) > 0 {
		for _, cv := range captures {
			name := cv.Obj.Name()
			if _, already := c.lambdaCaptures[name]; already {
				continue
			}
			// Check if this variable is also from outside the enclosing lambda
			_, declScope := c.scope.LookupParent(name)
			if declScope == nil {
				continue
			}
			isOuter := false
			for s := c.lambdaScope; s != nil; s = s.Parent() {
				if s == declScope {
					isOuter = true
					break
				}
			}
			if !isOuter {
				continue
			}
			// Enclosing lambda must also capture this variable
			byMove := c.lambdaMove
			if !byMove {
				if v, ok := cv.Obj.(*types.Var); ok && !isCopyField(v.Type()) {
					c.errorf(e.Pos(), "cannot capture non-copy variable '%s' without move", name)
					continue
				}
			}
			c.lambdaCaptures[name] = &CapturedVar{
				Obj:    cv.Obj,
				ByMove: byMove,
			}
		}
	}

	return sig
}

func (c *Checker) checkGoExpr(e *ast.GoExpr) types.Type {
	var innerType types.Type
	if e.Expr != nil {
		innerType = c.checkExpr(e.Expr)
		// Expression form: check argument types are sendable
		c.checkGoExprSendable(e.Expr)
	} else if e.Block != nil {
		c.openScope(e.Block, "go")
		c.checkBlock(e.Block)
		c.closeScope()
		// Block form: infer T from last expression statement
		if len(e.Block.Stmts) > 0 {
			if es, ok := e.Block.Stmts[len(e.Block.Stmts)-1].(*ast.ExprStmt); ok {
				innerType = c.info.Types[es.Expr]
			}
		}
		// Block form: check captured variables are sendable
		c.checkGoBlockSendable(e)
	}
	if innerType == nil {
		innerType = types.TypVoid
	}
	return types.NewInstance(types.TypTask, []types.Type{innerType})
}

func (c *Checker) checkUnsafeExpr(e *ast.UnsafeExpr) types.Type {
	c.openScope(e.Body, "unsafe")
	c.checkBlock(e.Body)
	c.closeScope()
	// Unsafe block type is the last expression's type (if any)
	return types.TypVoid
}

// resolveModuleMember resolves a qualified access like mod.symbol against
// the module's exported scope.
func (c *Checker) resolveModuleMember(e *ast.MemberExpr, mod *types.Module) types.Type {
	scope := mod.Scope()
	if scope == nil {
		c.errorf(e.Pos(), "module '%s' has no loaded scope", mod.Name())
		return nil
	}
	obj := scope.Lookup(e.Field)
	if obj == nil {
		c.errorf(e.Pos(), "module '%s' has no exported member '%s'", mod.Name(), e.Field)
		return nil
	}

	// Check visibility: only `public members are accessible from other modules
	if !isObjectExported(obj) {
		c.errorf(e.Pos(), "'%s' is private to module '%s'", e.Field, mod.Name())
		return nil
	}

	// Record the resolved object for codegen
	if ident, ok := e.Target.(*ast.IdentExpr); ok {
		c.recordObject(ident, mod)
	}

	// Module-level getter: treat as implicit call — return result type, not Signature.
	if fn, ok := obj.(*types.Func); ok && fn.IsGetter() {
		sig, ok := fn.Type().(*types.Signature)
		if ok && sig != nil {
			if sig.CanError() {
				c.info.FailableExprs[e] = true
			}
			return sig.Result()
		}
	}

	return obj.Type()
}

// isObjectExported returns true if the given object has the `public annotation.
func isObjectExported(obj types.Object) bool {
	switch o := obj.(type) {
	case *types.Func:
		return o.IsExported()
	case *types.TypeName:
		switch t := o.Type().(type) {
		case *types.Named:
			return t.IsExported()
		case *types.Enum:
			return t.IsExported()
		}
	}
	return false
}

// validateInterpolationType checks that a type used in string interpolation
// can be converted to a string. Primitives are handled by built-in codegen;
// user types must implement the Format structural interface (have a format method).
// TypeParams are allowed unconditionally — validation deferred to monomorphization.
func (c *Checker) validateInterpolationType(typ types.Type, node ast.Expr) {
	if typ == nil {
		return
	}
	// Unwrap optional layers
	inner := typ
	for {
		if opt, ok := inner.(*types.Optional); ok {
			inner = opt.Elem()
		} else {
			break
		}
	}
	// TypeParam: allow unconditionally — at monomorphization time the concrete
	// type will be substituted, and codegen will validate it has format().
	if _, ok := inner.(*types.TypeParam); ok {
		return
	}
	// Tuple: allow — codegen formats each element individually.
	if _, ok := inner.(*types.Tuple); ok {
		return
	}
	// Enum types: always allowed; codegen synthesizes variant-name output.
	if semaExtractEnum(inner) != nil {
		return
	}
	named := semaExtractNamed(inner)
	if named == nil {
		c.errorf(node.Pos(), "type %s cannot be used in string interpolation", inner)
		return
	}
	// Primitives and string are handled by built-in codegen conversions
	if isPrimitiveOrString(named) {
		return
	}
	// User types must have a format() method (Format structural interface)
	if named.LookupMethod("format") != nil {
		return
	}
	c.errorf(node.Pos(), "type %s cannot be used in string interpolation (does not implement Format)", named)
}

// semaExtractNamed unwraps Instance/SharedRef/MutRef to get the underlying *Named type.
func semaExtractNamed(typ types.Type) *types.Named {
	switch t := typ.(type) {
	case *types.Named:
		return t
	case *types.Instance:
		if n, ok := t.Origin().(*types.Named); ok {
			return n
		}
	case *types.SharedRef:
		return semaExtractNamed(t.Elem())
	case *types.MutRef:
		return semaExtractNamed(t.Elem())
	}
	return nil
}

// semaExtractEnum unwraps Instance/SharedRef/MutRef to get the underlying *Enum type.
func semaExtractEnum(typ types.Type) *types.Enum {
	switch t := typ.(type) {
	case *types.Enum:
		return t
	case *types.Instance:
		if e, ok := t.Origin().(*types.Enum); ok {
			return e
		}
	case *types.SharedRef:
		return semaExtractEnum(t.Elem())
	case *types.MutRef:
		return semaExtractEnum(t.Elem())
	}
	return nil
}

// stripRef unwraps SharedRef/MutRef to expose the underlying owned type.
// Used at sites that take ownership (vector/array literals, return values
// without explicit ref type) so a `T&` value flows as `T` for type matching.
// Movement out of the borrow is still rejected by the ownership pass.
func stripRef(typ types.Type) types.Type {
	switch t := typ.(type) {
	case *types.SharedRef:
		return t.Elem()
	case *types.MutRef:
		return t.Elem()
	}
	return typ
}

// isPrimitiveOrString returns true for built-in scalar types and string,
// which have hardcoded string conversion in codegen.
func isPrimitiveOrString(n *types.Named) bool {
	switch n {
	case types.TypInt, types.TypI8, types.TypI16, types.TypI32, types.TypI64,
		types.TypUint, types.TypU8, types.TypU16, types.TypU32, types.TypU64,
		types.TypF32, types.TypF64, types.TypBool, types.TypChar, types.TypString:
		return true
	}
	return false
}
