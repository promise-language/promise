package sema

import (
	"djabi.dev/go/promise_lang/internal/ast"
	"djabi.dev/go/promise_lang/internal/types"
)

// resolveCallArgs validates and reorders arguments according to the
// positional-first, named-after rule. It modifies e.Args in place so
// that after resolution, args are in parameter declaration order with
// exactly len(params) entries (defaults/nones inserted for omitted params).
//
// params are the expected parameters. subst is non-nil for generic calls.
// callDesc is used in error messages (e.g. "function 'foo'", "constructor for Foo").
//
// Returns true if resolution succeeded without fatal errors (individual
// arg type errors are reported but don't prevent resolution).
func (c *Checker) resolveCallArgs(
	e *ast.CallExpr,
	params []*types.Param,
	callDesc string,
	subst map[*types.TypeParam]types.Type,
) bool {
	args := e.Args

	// Detect variadic signature.
	isVariadic := len(params) > 0 && params[len(params)-1].IsVariadic()
	variadicIdx := -1
	if isVariadic {
		variadicIdx = len(params) - 1
	}

	// 1. Validate positional-first ordering: once a named arg appears,
	//    all subsequent args must be named.
	seenNamed := false
	for _, arg := range args {
		if arg.Name != "" {
			seenNamed = true
		} else if seenNamed {
			c.errorf(arg.Pos(), "positional argument after named argument in call to %s", callDesc)
			for _, a := range args {
				c.checkExpr(a.Value)
			}
			return false
		}
	}

	// 2. Count positional args.
	positionalCount := 0
	for _, arg := range args {
		if arg.Name == "" {
			positionalCount++
		}
	}

	// 3. Check: too many args overall.
	// For variadic functions, positional args beyond the non-variadic params
	// are collected into the variadic parameter.
	nonVariadicCount := len(params)
	if isVariadic {
		nonVariadicCount = len(params) - 1
	}

	if !isVariadic && len(args) > len(params) {
		c.errorf(e.Pos(), "%s expects %d arguments, got %d",
			callDesc, len(params), len(args))
		for _, a := range args {
			c.checkExpr(a.Value)
		}
		return false
	}

	// 4. Build result: resolved[i] holds the arg for param[i], or nil if omitted.
	resolved := make([]*ast.Arg, len(params))

	// 4a. Fill positional args for non-variadic params.
	positionalForFixed := positionalCount
	if isVariadic && positionalForFixed > nonVariadicCount {
		positionalForFixed = nonVariadicCount
	}
	for i := 0; i < positionalForFixed && i < len(params); i++ {
		resolved[i] = args[i]
	}

	// 4b. Collect extra positional args for the variadic param.
	var variadicArgs []*ast.Arg
	if isVariadic && positionalCount > nonVariadicCount {
		variadicArgs = args[nonVariadicCount:positionalCount]
	}

	// 4c. Fill named args by name lookup.
	paramIndex := make(map[string]int, len(params))
	for i, p := range params {
		paramIndex[p.Name()] = i
	}

	for i := positionalCount; i < len(args); i++ {
		arg := args[i]
		idx, ok := paramIndex[arg.Name]
		if !ok {
			c.errorf(arg.Pos(), "unknown parameter '%s' in call to %s", arg.Name, callDesc)
			c.checkExpr(arg.Value)
			continue
		}
		if resolved[idx] != nil {
			c.errorf(arg.Pos(), "parameter '%s' already provided in call to %s", arg.Name, callDesc)
			c.checkExpr(arg.Value)
			continue
		}
		resolved[idx] = arg
	}

	// 4d. For variadic param: if extra positional args exist, wrap them in an ArrayLit.
	// Special case: if exactly one extra positional arg has type T[] (matching the
	// variadic param type), pass it directly without wrapping.
	if isVariadic && resolved[variadicIdx] == nil {
		if len(variadicArgs) == 1 {
			// Single arg: check if it's already a T[] — pass through directly.
			// Type-check the arg first to determine its type.
			paramType := params[variadicIdx].Type()
			if subst != nil {
				paramType = types.Substitute(paramType, subst)
			}
			argType := c.checkExprWithHint(variadicArgs[0].Value, paramType)
			if argType != nil && types.AssignableTo(argType, paramType) {
				// Direct T[] pass-through.
				resolved[variadicIdx] = variadicArgs[0]
				resolved[variadicIdx].Name = params[variadicIdx].Name()
			} else {
				// Single element of type T — wrap in array.
				arrayLit := &ast.ArrayLit{Elements: []ast.Expr{variadicArgs[0].Value}}
				resolved[variadicIdx] = &ast.Arg{
					Name:  params[variadicIdx].Name(),
					Value: arrayLit,
				}
			}
		} else if len(variadicArgs) > 1 {
			// Multiple args: wrap into an array literal.
			elems := make([]ast.Expr, len(variadicArgs))
			for i, a := range variadicArgs {
				elems[i] = a.Value
			}
			arrayLit := &ast.ArrayLit{Elements: elems}
			resolved[variadicIdx] = &ast.Arg{
				Name:  params[variadicIdx].Name(),
				Value: arrayLit,
			}
		}
		// If still nil (no variadicArgs), will be filled with empty array in step 5.
	}

	// 5. Check for missing required params; insert defaults/nones for optional params.
	requiredMissing := false
	for i, param := range params {
		if resolved[i] != nil {
			continue
		}
		paramType := param.Type()
		if subst != nil {
			paramType = types.Substitute(paramType, subst)
		}

		// Variadic param with no args → insert empty array literal.
		if param.IsVariadic() {
			resolved[i] = &ast.Arg{
				Name:  param.Name(),
				Value: &ast.ArrayLit{Elements: nil},
			}
			continue
		}

		if param.HasDefault() {
			// Has a default — insert the default expression as a synthetic arg.
			// First check the current file's ParamDefaults (fast path for local functions).
			// Fall back to the default stored on the param itself for cross-module calls
			// (e.g., calling a std module function from user code).
			var defExpr ast.Expr
			if expr, ok := c.info.ParamDefaults[param]; ok {
				defExpr = expr
			} else if raw := param.DefaultExpr(); raw != nil {
				defExpr, _ = raw.(ast.Expr)
			}
			if defExpr != nil {
				resolved[i] = &ast.Arg{Name: param.Name(), Value: defExpr}
			}
			continue
		}
		if _, isOpt := paramType.(*types.Optional); isOpt {
			// Optional type — insert a none literal.
			resolved[i] = &ast.Arg{Name: param.Name(), Value: &ast.NoneLit{}}
			continue
		}
		c.errorf(e.Pos(), "missing required argument '%s' in call to %s", param.Name(), callDesc)
		requiredMissing = true
	}

	if requiredMissing {
		for _, a := range args {
			c.checkExpr(a.Value)
		}
		return false
	}

	// 6. Type-check each resolved arg against its parameter.
	for i, param := range params {
		arg := resolved[i]
		if arg == nil {
			continue
		}
		paramType := param.Type()
		if subst != nil {
			paramType = types.Substitute(paramType, subst)
		}
		argType := c.checkExprWithHint(arg.Value, paramType)
		if argType != nil && paramType != nil && !types.AssignableTo(argType, paramType) {
			c.errorf(arg.Pos(), "cannot assign %s to parameter '%s' of type %s in %s",
				argType, param.Name(), paramType, callDesc)
		}
	}

	// 7. Set e.Args to resolved (full param order, with defaults/nones filled).
	e.Args = resolved

	return true
}

// resolveImplicitConstructorArgs validates and reorders arguments for an implicit
// constructor call (Type(field: value, ...)) using field declarations as parameters.
// This is separate from resolveCallArgs because implicit constructors match against
// fields (with field defaults and optional types) rather than function parameters,
// and codegen handles omitted fields with its own default/zero-init loop.
func (c *Checker) resolveImplicitConstructorArgs(
	e *ast.CallExpr,
	named *types.Named,
	subst map[*types.TypeParam]types.Type,
) bool {
	fields := named.AllFields()
	args := e.Args

	// 1. Validate positional-first ordering.
	seenNamed := false
	for _, arg := range args {
		if arg.Name != "" {
			seenNamed = true
		} else if seenNamed {
			c.errorf(arg.Pos(), "positional argument after named argument in constructor for %s", named)
			for _, a := range args {
				c.checkExpr(a.Value)
			}
			return false
		}
	}

	// 2. Count positional args.
	positionalCount := 0
	for _, arg := range args {
		if arg.Name == "" {
			positionalCount++
		}
	}

	// 3. Check: too many args overall.
	if len(args) > len(fields) {
		c.errorf(e.Pos(), "constructor for %s expects at most %d arguments, got %d",
			named, len(fields), len(args))
		for _, a := range args {
			c.checkExpr(a.Value)
		}
		return false
	}

	// 4. Build result: resolved[i] holds the arg for field[i], or nil if omitted.
	resolved := make([]*ast.Arg, len(fields))

	// 4a. Fill positional args: first N fields in declaration order.
	for i := 0; i < positionalCount; i++ {
		resolved[i] = args[i]
	}

	// 4b. Fill named args by field name lookup.
	fieldIndex := make(map[string]int, len(fields))
	for i, f := range fields {
		fieldIndex[f.Name()] = i
	}

	for i := positionalCount; i < len(args); i++ {
		arg := args[i]
		idx, ok := fieldIndex[arg.Name]
		if !ok {
			c.errorf(arg.Pos(), "type %s has no field '%s'", named, arg.Name)
			c.checkExpr(arg.Value)
			continue
		}
		if resolved[idx] != nil {
			c.errorf(arg.Pos(), "field '%s' already provided in constructor for %s", arg.Name, named)
			c.checkExpr(arg.Value)
			continue
		}
		resolved[idx] = arg
	}

	// 5. Check for missing required fields.
	requiredMissing := false
	for i, f := range fields {
		if resolved[i] != nil {
			continue
		}
		if f.HasDefault() {
			continue
		}
		fieldType := f.Type()
		if subst != nil {
			fieldType = types.Substitute(fieldType, subst)
		}
		if _, isOpt := fieldType.(*types.Optional); isOpt {
			continue
		}
		c.errorf(e.Pos(), "missing required field '%s' in constructor for %s", f.Name(), named)
		requiredMissing = true
	}

	if requiredMissing {
		// Still type-check provided args for better error reporting.
		for _, a := range args {
			c.checkExpr(a.Value)
		}
		return false
	}

	// 6. Type-check each resolved arg against its field type.
	for i, f := range fields {
		arg := resolved[i]
		if arg == nil {
			continue
		}
		fieldType := f.Type()
		if subst != nil {
			fieldType = types.Substitute(fieldType, subst)
		}
		argType := c.checkExprWithHint(arg.Value, fieldType)
		if argType != nil && !types.AssignableTo(argType, fieldType) {
			c.errorf(arg.Pos(), "cannot assign %s to field '%s' of type %s",
				argType, f.Name(), fieldType)
		}
	}

	// 7. Reorder e.Args: keep only provided args, in field declaration order,
	// with Name set on each so codegen can do field-name lookup.
	reordered := make([]*ast.Arg, 0, len(args))
	for i, f := range fields {
		if resolved[i] != nil {
			resolved[i].Name = f.Name()
			reordered = append(reordered, resolved[i])
		}
	}
	e.Args = reordered

	return true
}

// validateVariadicParams checks that variadic parameters (...T) are used correctly:
// - at most one variadic param
// - must be the last parameter
// - cannot have a default value
// - cannot have a ref modifier (& or ~)
func (c *Checker) validateVariadicParams(astParams []*ast.Param, params []*types.Param, callDesc string) {
	variadicCount := 0
	for i, p := range astParams {
		if !p.IsVariadic {
			continue
		}
		variadicCount++
		if variadicCount > 1 {
			c.errorf(p.Pos(), "at most one variadic parameter allowed in %s", callDesc)
		}
		if i != len(astParams)-1 {
			c.errorf(p.Pos(), "variadic parameter must be the last parameter in %s", callDesc)
		}
		if p.Default != nil {
			c.errorf(p.Pos(), "variadic parameter '%s' cannot have a default value", p.Name)
		}
		if p.RefMod != ast.RefNone {
			c.errorf(p.Pos(), "variadic parameter '%s' cannot have a reference modifier", p.Name)
		}
		_ = params // params slice already has the correct types set
	}
}
