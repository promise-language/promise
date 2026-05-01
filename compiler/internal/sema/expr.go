package sema

import (
	"sort"

	"djabi.dev/go/promise_lang/internal/ast"
	"djabi.dev/go/promise_lang/internal/types"
)

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

	var typ types.Type

	switch e := expr.(type) {
	case *ast.IntLit:
		if hint != nil && isIntegerType(hint) {
			typ = hint
		} else {
			typ = types.TypInt
		}

	case *ast.FloatLit:
		if hint != nil && isFloatType(hint) {
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
			if interp, ok := part.(ast.StringInterp); ok && interp.Expr != nil {
				c.checkExpr(interp.Expr)
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
		typ = c.checkArrayLit(e)

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

	case *ast.OptionalChainExpr:
		typ = c.checkOptionalChainExpr(e)

	case *ast.IsExpr:
		typ = c.checkIsExpr(e)

	case *ast.CastExpr:
		typ = c.checkCastExpr(e)

	case *ast.ErrorPropagateExpr:
		typ = c.checkErrorPropagateExpr(e)

	case *ast.ErrorUnwrapExpr:
		typ = c.checkErrorUnwrapExpr(e)

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
		return c.curType
	}

	obj := c.lookup(e.Name)
	if obj == nil {
		c.errorf(e.Pos(), "undefined: %s", e.Name)
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
	if c.curFunc == nil || c.curFunc.Recv() == nil {
		c.errorf(e.Pos(), "'this' used outside of a method")
		return nil
	}
	return c.curFunc.Recv().Type()
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

func (c *Checker) checkArrayLit(e *ast.ArrayLit) types.Type {
	if len(e.Elements) == 0 {
		c.errorf(e.Pos(), "cannot infer type of empty array literal")
		return nil
	}

	elemType := c.checkExpr(e.Elements[0])
	if elemType == nil {
		return nil
	}

	for i := 1; i < len(e.Elements); i++ {
		et := c.checkExpr(e.Elements[i])
		if et == nil {
			continue
		}
		if !types.Identical(et, elemType) {
			c.errorf(e.Elements[i].Pos(), "array element type mismatch: expected %s, got %s", elemType, et)
		}
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
		return sig.Result()
	}
	return types.TypVoid
}

func (c *Checker) checkUnaryExpr(e *ast.UnaryExpr) types.Type {
	operand := c.checkExpr(e.Operand)
	if operand == nil {
		return nil
	}

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

	// If the type has an explicit new() constructor, route through parameter checking
	if named.HasNew() {
		return c.checkNewConstructorCall(e, named, nil)
	}

	c.resolveImplicitConstructorArgs(e, named, nil)
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
	parents := c.curType.Parents()
	if len(parents) == 0 {
		c.errorf(e.Pos(), "super() called but type %s has no parent", c.curType)
		for _, arg := range e.Args {
			c.checkExpr(arg.Value)
		}
		return types.TypVoid
	}
	parent := parents[0] // first parent is the concrete parent

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
			c.checkExpr(arg.Value)
		}
		return inst
	}
	if origin == types.TypVector {
		for _, arg := range e.Args {
			c.checkExpr(arg.Value)
		}
		return inst
	}

	subst := types.BuildSubstMap(origin.TypeParams(), inst.TypeArgs())

	// If the type has an explicit new() constructor, route through parameter checking
	if origin.HasNew() {
		c.checkNewConstructorCall(e, origin, subst)
		return inst
	}

	c.resolveImplicitConstructorArgs(e, origin, subst)
	return inst
}

func (c *Checker) checkCallExpr(e *ast.CallExpr) types.Type {
	// Handle super() calls in constructor bodies
	if ident, ok := e.Callee.(*ast.IdentExpr); ok && ident.Name == "super" {
		return c.checkSuperCall(e)
	}

	calleeType := c.checkExpr(e.Callee)
	if calleeType == nil {
		return nil
	}

	// Handle constructor calls: Type(field: value, ...)
	switch t := calleeType.(type) {
	case *types.Named:
		return c.checkConstructorCall(e, t)
	case *types.Instance:
		return c.checkInstanceConstructorCall(e, t)
	case *types.Enum:
		// Enum constructors aren't called directly (use Enum.Variant syntax)
		c.errorf(e.Pos(), "cannot construct enum %s directly; use Enum.Variant syntax", t)
		return nil
	}

	sig, ok := calleeType.(*types.Signature)
	if !ok {
		c.errorf(e.Pos(), "cannot call non-function type %s", calleeType)
		return nil
	}

	// Build call description for error messages.
	callDesc := "function"
	if ident, ok := e.Callee.(*ast.IdentExpr); ok {
		callDesc = "function '" + ident.Name + "'"
	} else if mem, ok := e.Callee.(*ast.MemberExpr); ok {
		callDesc = "method '" + mem.Field + "'"
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
	// Handle std.X — resolve against stdScope directly
	if ident, ok := e.Target.(*ast.IdentExpr); ok && ident.Name == "std" {
		return c.resolveStdMember(e)
	}

	// Handle module-qualified access: mod.symbol
	if ident, ok := e.Target.(*ast.IdentExpr); ok {
		if obj := c.lookup(ident.Name); obj != nil {
			if mod, ok := obj.(*types.Module); ok {
				return c.resolveModuleMember(e, mod)
			}
		}
	}

	target := c.checkExpr(e.Target)
	if target == nil {
		return nil
	}

	switch t := target.(type) {
	case *types.Named:
		// Check fields first, then getters, then methods
		if f := t.LookupField(e.Field); f != nil {
			if f.Deprecated() != "" {
				c.warnf(e.Pos(), "use of deprecated field '%s'", e.Field)
			}
			return f.Type()
		}
		if g := t.LookupGetter(e.Field); g != nil {
			if g.Deprecated() != "" {
				c.warnf(e.Pos(), "use of deprecated getter '%s'", e.Field)
			}
			return g.Sig().Result()
		}
		if m := t.LookupMethod(e.Field); m != nil {
			if m.Deprecated() != "" {
				c.warnf(e.Pos(), "use of deprecated method '%s'", e.Field)
			}
			return m.Sig()
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
		if m := t.LookupMethod(e.Field); m != nil {
			if m.Deprecated() != "" {
				c.warnf(e.Pos(), "use of deprecated method '%s'", e.Field)
			}
			return m.Sig()
		}
		c.errorf(e.Pos(), "enum %s has no variant or method %s", t, e.Field)
		return nil

	case *types.Instance:
		return c.resolveInstanceMember(e.Pos(), t, e.Field)

	case *types.Array:
		// Arrays delegate to TypVector for field/method lookup
		subst := types.BuildSubstMap(types.TypVector.TypeParams(), []types.Type{t.Elem()})
		if f := types.TypVector.LookupField(e.Field); f != nil {
			return types.Substitute(f.Type(), subst)
		}
		if g := types.TypVector.LookupGetter(e.Field); g != nil {
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
func (c *Checker) resolveInstanceMember(pos ast.Pos, inst *types.Instance, name string) types.Type {
	switch origin := inst.Origin().(type) {
	case *types.Named:
		subst := types.BuildSubstMap(origin.TypeParams(), inst.TypeArgs())
		if f := origin.LookupField(name); f != nil {
			if f.Deprecated() != "" {
				c.warnf(pos, "use of deprecated field '%s'", name)
			}
			return types.Substitute(f.Type(), subst)
		}
		if g := origin.LookupGetter(name); g != nil {
			if g.Deprecated() != "" {
				c.warnf(pos, "use of deprecated getter '%s'", name)
			}
			return types.Substitute(g.Sig().Result(), subst)
		}
		if m := origin.LookupMethod(name); m != nil {
			if m.Deprecated() != "" {
				c.warnf(pos, "use of deprecated method '%s'", name)
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
	if m := enum.LookupMethod(name); m != nil {
		if m.Deprecated() != "" {
			c.warnf(pos, "use of deprecated method '%s'", name)
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

	// Generic instantiation: Type[Arg] or func[Arg] in expression context.
	// Only treat [index] as type argument when the target is a type name
	// reference (e.g., Vector[int]), NOT a value of a generic type
	// (e.g., this[i] inside Vector[T]'s method body).
	isTypeRef := false
	if ident, ok := e.Target.(*ast.IdentExpr); ok {
		if obj, found := c.info.Objects[ident]; found {
			_, isTypeRef = obj.(*types.TypeName)
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
	if sig, ok := target.(*types.Signature); ok {
		if len(sig.TypeParams()) > 0 {
			return c.instantiateGenericFunc(e, sig)
		}
	}

	index := c.checkExpr(e.Index)

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

func (c *Checker) checkSliceExpr(e *ast.SliceExpr) types.Type {
	target := c.checkExpr(e.Target)
	if target == nil {
		return nil
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

// instantiateFromIndex handles Type[Arg] in expression context as generic instantiation.
// The index expression is reinterpreted as a type argument.
func (c *Checker) instantiateFromIndex(e *ast.IndexExpr, origin types.Type, tparams []*types.TypeParam) types.Type {
	// The index is a type name used as a type argument
	typeArg := c.resolveTypeRef(e.Index)
	if typeArg == nil {
		return nil
	}

	if len(tparams) != 1 {
		c.errorf(e.Pos(), "type %s expects %d type arguments, got 1", origin, len(tparams))
		return nil
	}

	c.validateConstraints(e.Pos(), origin, []types.Type{typeArg})
	inst := types.NewInstance(origin, []types.Type{typeArg})
	c.recordInstance(inst)
	return inst
}

// instantiateGenericFunc handles func[Arg] in expression context as generic function instantiation.
// Returns the substituted signature (with TypeParams stripped).
func (c *Checker) instantiateGenericFunc(e *ast.IndexExpr, sig *types.Signature) types.Type {
	// Resolve the type argument from the index expression
	typeArg := c.resolveTypeRef(e.Index)
	if typeArg == nil {
		c.errorf(e.Index.Pos(), "cannot resolve type argument")
		return nil
	}

	tparams := sig.TypeParams()
	if len(tparams) != 1 {
		c.errorf(e.Pos(), "function expects %d type arguments, got 1", len(tparams))
		return nil
	}

	// Build substitution map and substitute the signature
	subst := types.BuildSubstMap(tparams, []types.Type{typeArg})
	monoSig := types.Substitute(sig, subst).(*types.Signature)

	// Look up the original function object for FuncInstance recording
	if ident, ok := e.Target.(*ast.IdentExpr); ok {
		obj := c.lookup(ident.Name)
		if fn, ok := obj.(*types.Func); ok {
			c.info.FuncInstances = append(c.info.FuncInstances, &FuncInstance{
				Func:     fn,
				TypeArgs: []types.Type{typeArg},
				Sig:      monoSig,
			})
		}
	}

	return monoSig
}

// resolveTypeRef resolves an expression as a type reference.
// Used for type arguments in generic instantiations (e.g., the "int" in func[int]).
func (c *Checker) resolveTypeRef(expr ast.Expr) types.Type {
	if ident, ok := expr.(*ast.IdentExpr); ok {
		obj := c.lookup(ident.Name)
		if obj == nil {
			return nil
		}
		typ := obj.Type()
		c.recordType(expr, typ)
		c.recordObject(ident, obj)
		return typ
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
		result := c.resolveInstanceMember(e.Pos(), t, e.Field)
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
			break
		}
		obj := c.lookup(p.Name)
		if obj == nil {
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
			}
		}
	case *ast.DestructureIsPattern:
		obj := c.lookup(p.TypeName)
		if obj == nil {
			c.errorf(p.Pos(), "undefined type: %s", p.TypeName)
		}
	}
	return types.TypBool
}

func (c *Checker) checkCastExpr(e *ast.CastExpr) types.Type {
	c.checkExpr(e.Expr)
	target := c.resolveType(e.Type)
	if target == nil {
		return nil
	}

	// Numeric casts always succeed — return target type directly (not optional)
	srcType := c.info.Types[e.Expr]
	if isNumericType(srcType) && isNumericType(target) {
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
		c.errorf(e.Pos(), "error propagation (?) used outside of failable function")
	}
	if !c.info.FailableExprs[e.Expr] {
		c.errorf(e.Pos(), "error propagation (?) requires a failable expression")
	}
	// The inner expression's type is the success type (error is propagated)
	return inner
}

func (c *Checker) checkErrorUnwrapExpr(e *ast.ErrorUnwrapExpr) types.Type {
	inner := c.checkExpr(e.Expr)
	if !c.info.FailableExprs[e.Expr] {
		c.errorf(e.Pos(), "error unwrap (!) requires a failable expression")
	}
	// Unwrap panics on error, returns success type
	return inner
}

func (c *Checker) checkErrorHandlerExpr(e *ast.ErrorHandlerExpr) types.Type {
	inner := c.checkExpr(e.Expr)
	if !c.info.FailableExprs[e.Expr] {
		c.errorf(e.Pos(), "error handler requires a failable expression")
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
		obj := c.lookup(e.TypeName)
		if obj == nil {
			c.errorf(e.Pos(), "undefined type: %s", e.TypeName)
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

	var thenType types.Type
	if len(e.Then.Stmts) > 0 {
		if es, ok := e.Then.Stmts[len(e.Then.Stmts)-1].(*ast.ExprStmt); ok {
			thenType = c.info.Types[es.Expr]
		}
	}

	c.openScope(e.Else, "if-else")
	c.checkBlock(e.Else)
	c.closeScope()

	// Return the then-branch type (both branches should match, but we check what we can)
	return thenType
}

func (c *Checker) checkMatchExpr(e *ast.MatchExpr) types.Type {
	subjectType := c.checkExpr(e.Subject)

	var resultType types.Type
	for _, arm := range e.Arms {
		c.openScope(arm, "match-arm")
		c.checkMatchPattern(arm.Pattern, subjectType)
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
		}

		c.closeScope()

		if resultType == nil {
			resultType = armType
		}
	}

	// Check exhaustiveness
	c.checkMatchExhaustiveness(e, subjectType)

	return resultType
}

func (c *Checker) checkMatchPattern(pat ast.MatchPattern, subjectType types.Type) {
	if pat == nil {
		return
	}

	switch p := pat.(type) {
	case *ast.EnumDestructureMatchPattern:
		obj := c.lookup(p.Enum)
		if obj == nil {
			c.errorf(p.Pos(), "undefined: %s", p.Enum)
			return
		}
		tn, ok := obj.(*types.TypeName)
		if !ok {
			c.errorf(p.Pos(), "%s is not a type", p.Enum)
			return
		}
		enum, ok := tn.Type().(*types.Enum)
		if !ok {
			c.errorf(p.Pos(), "%s is not an enum type", p.Enum)
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

	case *ast.EnumVariantMatchPattern:
		obj := c.lookup(p.Enum)
		if obj == nil {
			c.errorf(p.Pos(), "undefined: %s", p.Enum)
			return
		}
		tn, ok := obj.(*types.TypeName)
		if !ok {
			return
		}
		enum, ok := tn.Type().(*types.Enum)
		if !ok {
			c.errorf(p.Pos(), "%s is not an enum type", p.Enum)
			return
		}
		if enum.LookupVariant(p.Variant) == nil {
			c.errorf(p.Pos(), "enum %s has no variant %s", p.Enum, p.Variant)
		}

	case *ast.TypeBindingMatchPattern:
		obj := c.lookup(p.TypeName)
		if obj == nil {
			c.errorf(p.Pos(), "undefined type: %s", p.TypeName)
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
					return
				}
			}
		}
		// Fallback: look up as a standalone name
		obj := c.lookup(p.Name)
		if obj == nil {
			c.errorf(p.Pos(), "undefined: %s", p.Name)
		}

	case *ast.LiteralMatchPattern:
		c.checkExpr(p.Value)

	case *ast.NameMatchPattern:
		// Simple binding — no validation needed

	case *ast.WildcardMatchPattern:
		// Always valid
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

// insertEnumDestructureBindings handles Enum.Variant(a, b) pattern bindings.
// Uses subjectType to build a substitution map for generic enum instances.
func (c *Checker) insertEnumDestructureBindings(p *ast.EnumDestructureMatchPattern, subjectType types.Type) {
	obj := c.lookup(p.Enum)
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

// resolveStdMember handles std.X expressions by looking up X in the std scope.
// TODO: test std.X used as a value expression (not a callee) when first-class functions are supported
func (c *Checker) resolveStdMember(e *ast.MemberExpr) types.Type {
	if c.stdScope == nil {
		c.errorf(e.Pos(), "std library is not available")
		return nil
	}
	obj := c.stdScope.Lookup(e.Field)
	if obj == nil {
		c.errorf(e.Pos(), "std has no member '%s'", e.Field)
		return nil
	}
	return obj.Type()
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
