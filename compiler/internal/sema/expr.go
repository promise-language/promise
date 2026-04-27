package sema

import (
	"sort"

	"djabi.dev/go/promise_lang/internal/ast"
	"djabi.dev/go/promise_lang/internal/types"
)

// checkExpr type-checks an expression and returns its resolved type.
// The result is also recorded in c.info.Types.
func (c *Checker) checkExpr(expr ast.Expr) types.Type {
	if expr == nil {
		return nil
	}

	var typ types.Type

	switch e := expr.(type) {
	case *ast.IntLit:
		typ = types.TypInt

	case *ast.FloatLit:
		typ = types.TypF64

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
		typ = c.checkExpr(e.Expr)

	case *ast.TupleLit:
		typ = c.checkTupleLit(e)

	case *ast.ArrayLit:
		typ = c.checkArrayLit(e)

	case *ast.MapLit:
		typ = c.checkMapLit(e)

	case *ast.BinaryExpr:
		typ = c.checkBinaryExpr(e)

	case *ast.UnaryExpr:
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
	// Check for contextual keywords
	if e.Name == "present" || e.Name == "absent" {
		return types.TypBool
	}

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
	// Module aliases are placeholders — module loading is not yet implemented
	if _, ok := obj.(*types.Module); ok {
		c.errorf(e.Pos(), "module %s is not loaded (module loading not yet implemented)", e.Name)
		return nil
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

	inst := types.NewSlice(elemType)
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
	inst := types.NewMap(keyType, valType)
	c.recordInstance(inst)
	return inst
}

func (c *Checker) checkBinaryExpr(e *ast.BinaryExpr) types.Type {
	left := c.checkExpr(e.Left)
	right := c.checkExpr(e.Right)

	if left == nil || right == nil {
		return nil
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

	case ast.UnaryReceive:
		// <-expr: operand should be task[T] or channel[T], result is T
		if inst, ok := operand.(*types.Instance); ok {
			origin := inst.Origin()
			if origin == types.TypTask || origin == types.TypChannel {
				if len(inst.TypeArgs()) > 0 {
					return inst.TypeArgs()[0]
				}
			}
		}
		c.errorf(e.Pos(), "receive operator (<-) requires task[T] or channel[T], got %s", operand)
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

	// Check arguments against fields (implicit constructor)
	provided := make(map[string]bool)
	for _, arg := range e.Args {
		argType := c.checkExpr(arg.Value)
		if arg.Name != "" {
			provided[arg.Name] = true
			// Named argument — check field exists and type matches
			f := named.LookupField(arg.Name)
			if f == nil {
				c.errorf(arg.Pos(), "type %s has no field %s", named, arg.Name)
			} else if argType != nil && !types.AssignableTo(argType, f.Type()) {
				c.errorf(arg.Pos(), "cannot assign %s to field %s of type %s",
					argType, arg.Name, f.Type())
			}
		}
	}

	// Check that all required fields are provided.
	// A field is required if it has no default value and is not optional (T?).
	for _, f := range named.AllFields() {
		if provided[f.Name()] {
			continue
		}
		if f.HasDefault() {
			continue
		}
		if _, isOpt := f.Type().(*types.Optional); isOpt {
			continue
		}
		c.errorf(e.Pos(), "missing required field '%s' in constructor for %s", f.Name(), named)
	}
	return named
}

// checkNewConstructorCall validates a constructor call against the new() method's parameters.
// subst is non-nil for generic instantiations.
func (c *Checker) checkNewConstructorCall(e *ast.CallExpr, named *types.Named, subst map[*types.TypeParam]types.Type) types.Type {
	newMethod := named.LookupMethod("new")
	if newMethod == nil {
		return named
	}
	sig := newMethod.Sig()
	params := sig.Params()

	// Check argument count
	if len(e.Args) != len(params) {
		c.errorf(e.Pos(), "constructor for %s expects %d arguments, got %d",
			named, len(params), len(e.Args))
	}

	// Check each argument type
	for i, arg := range e.Args {
		argType := c.checkExpr(arg.Value)
		if i >= len(params) {
			continue
		}
		paramType := params[i].Type()
		if subst != nil {
			paramType = types.Substitute(paramType, subst)
		}
		if arg.Name != "" && arg.Name != params[i].Name() {
			c.errorf(arg.Pos(), "argument name '%s' does not match parameter '%s'",
				arg.Name, params[i].Name())
		}
		if argType != nil && paramType != nil && !types.AssignableTo(argType, paramType) {
			c.errorf(arg.Pos(), "cannot assign %s to parameter %s of type %s",
				argType, params[i].Name(), paramType)
		}
	}

	// If failable, return type includes error potential
	if sig.CanError() {
		return named // failable handled by caller (Point 5)
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
		params := newMethod.Sig().Params()
		if len(e.Args) != len(params) {
			c.errorf(e.Pos(), "super() expects %d arguments (parent %s new), got %d",
				len(params), parent, len(e.Args))
		}
		for i, arg := range e.Args {
			argType := c.checkExpr(arg.Value)
			if i >= len(params) {
				continue
			}
			if arg.Name != "" && arg.Name != params[i].Name() {
				c.errorf(arg.Pos(), "argument name '%s' does not match parameter '%s'",
					arg.Name, params[i].Name())
			}
			if argType != nil && params[i].Type() != nil && !types.AssignableTo(argType, params[i].Type()) {
				c.errorf(arg.Pos(), "cannot assign %s to parameter %s of type %s",
					argType, params[i].Name(), params[i].Type())
			}
		}
	} else {
		// Parent has implicit constructor — validate named args against parent's fields
		provided := make(map[string]bool)
		for _, arg := range e.Args {
			argType := c.checkExpr(arg.Value)
			if arg.Name != "" {
				provided[arg.Name] = true
				f := parent.LookupField(arg.Name)
				if f == nil {
					c.errorf(arg.Pos(), "parent type %s has no field %s", parent, arg.Name)
				} else if argType != nil && !types.AssignableTo(argType, f.Type()) {
					c.errorf(arg.Pos(), "cannot assign %s to field %s of type %s",
						argType, arg.Name, f.Type())
				}
			}
		}
		// Check that all required parent fields are provided
		for _, f := range parent.AllFields() {
			if provided[f.Name()] {
				continue
			}
			if f.HasDefault() {
				continue
			}
			if _, isOpt := f.Type().(*types.Optional); isOpt {
				continue
			}
			c.errorf(e.Pos(), "missing required field '%s' in super() call for parent %s", f.Name(), parent)
		}
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

	subst := types.BuildSubstMap(origin.TypeParams(), inst.TypeArgs())

	// If the type has an explicit new() constructor, route through parameter checking
	if origin.HasNew() {
		c.checkNewConstructorCall(e, origin, subst)
		return inst
	}

	provided := make(map[string]bool)
	for _, arg := range e.Args {
		argType := c.checkExpr(arg.Value)
		if arg.Name != "" {
			provided[arg.Name] = true
			f := origin.LookupField(arg.Name)
			if f == nil {
				c.errorf(arg.Pos(), "type %s has no field %s", inst, arg.Name)
			} else if argType != nil {
				fieldType := types.Substitute(f.Type(), subst)
				if !types.AssignableTo(argType, fieldType) {
					c.errorf(arg.Pos(), "cannot assign %s to field %s of type %s",
						argType, arg.Name, fieldType)
				}
			}
		}
	}

	// Check that all required fields are provided.
	for _, f := range origin.AllFields() {
		if provided[f.Name()] {
			continue
		}
		if f.HasDefault() {
			continue
		}
		fieldType := types.Substitute(f.Type(), subst)
		if _, isOpt := fieldType.(*types.Optional); isOpt {
			continue
		}
		c.errorf(e.Pos(), "missing required field '%s' in constructor for %s", f.Name(), inst)
	}
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

	// Check argument count
	if len(e.Args) != len(sig.Params()) {
		c.errorf(e.Pos(), "function expects %d arguments, got %d",
			len(sig.Params()), len(e.Args))
		// Continue checking what we can
	}

	// Check argument types
	n := len(e.Args)
	if n > len(sig.Params()) {
		n = len(sig.Params())
	}
	for i := 0; i < n; i++ {
		argType := c.checkExpr(e.Args[i].Value)
		if argType == nil {
			continue
		}
		paramType := sig.Params()[i].Type()
		if !types.AssignableTo(argType, paramType) {
			c.errorf(e.Args[i].Pos(), "argument type %s not assignable to parameter type %s",
				argType, paramType)
		}
	}
	// Check remaining args even if too many
	for i := n; i < len(e.Args); i++ {
		c.checkExpr(e.Args[i].Value)
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
		// Check for variant access (Enum.VariantName)
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
		// Arrays delegate to TypSlice for field/method lookup
		subst := types.BuildSubstMap(types.TypSlice.TypeParams(), []types.Type{t.Elem()})
		if f := types.TypSlice.LookupField(e.Field); f != nil {
			return types.Substitute(f.Type(), subst)
		}
		if g := types.TypSlice.LookupGetter(e.Field); g != nil {
			return types.Substitute(g.Sig().Result(), subst)
		}
		if m := types.TypSlice.LookupMethod(e.Field); m != nil {
			if e.Field == "push" || e.Field == "pop" || e.Field == "remove" {
				c.errorf(e.Pos(), "cannot %s on fixed-size array", e.Field)
				return nil
			}
			return types.Substitute(m.Sig(), subst)
		}
		c.errorf(e.Pos(), "type %s has no member %s", t, e.Field)
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
	// When target is a generic Named/Enum, treat [index] as type argument.
	switch t := target.(type) {
	case *types.Named:
		if len(t.TypeParams()) > 0 {
			return c.instantiateFromIndex(e, t, t.TypeParams())
		}
	case *types.Enum:
		if len(t.TypeParams()) > 0 {
			return c.instantiateFromIndex(e, t, t.TypeParams())
		}
	case *types.Signature:
		if len(t.TypeParams()) > 0 {
			return c.instantiateGenericFunc(e, t)
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
	// The inner expression's type is the success type (error is propagated)
	return inner
}

func (c *Checker) checkErrorUnwrapExpr(e *ast.ErrorUnwrapExpr) types.Type {
	inner := c.checkExpr(e.Expr)
	// Unwrap panics on error, returns success type
	return inner
}

func (c *Checker) checkErrorHandlerExpr(e *ast.ErrorHandlerExpr) types.Type {
	inner := c.checkExpr(e.Expr)

	c.openScope(e.Body, "error-handler")
	// Bind error variable if present
	if e.Binding != "" && e.Binding != "_" {
		c.insert(types.NewVar(tpos(e.Pos()), e.Binding, types.TypError))
	}
	c.checkBlock(e.Body)
	c.closeScope()

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
