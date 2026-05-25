package sema

import (
	"djabi.dev/go/promise_lang/internal/ast"
	"djabi.dev/go/promise_lang/internal/types"
)

// inferTypeArgs attempts to infer type arguments for a generic function call.
// Given type parameters, formal parameters, and the types of the actual arguments
// (aligned to params for fixed params, with extra entries for variadic args),
// it returns the inferred type args in TypeParam index order.
// Returns nil if inference fails (not all type params could be bound,
// or conflicting bindings were found).
func inferTypeArgs(tparams []*types.TypeParam, params []*types.Param, argTypes []types.Type) []types.Type {
	bindings := make(map[*types.TypeParam]types.Type, len(tparams))

	isVariadic := len(params) > 0 && params[len(params)-1].IsVariadic()
	fixedCount := len(params)
	if isVariadic {
		fixedCount--
	}

	// Unify non-variadic params against their matched arg types.
	for i := 0; i < fixedCount && i < len(argTypes); i++ {
		if argTypes[i] == nil || params[i].Type() == nil {
			continue
		}
		if !unifyType(params[i].Type(), argTypes[i], bindings) {
			return nil
		}
	}

	// Unify variadic args.
	if isVariadic && len(argTypes) > fixedCount {
		variadicParam := params[len(params)-1]
		fullType := variadicParam.Type() // T[] (Vector[T])
		elemType := fullType
		if elem, ok := types.AsVector(fullType); ok {
			elemType = elem
		}

		extraArgs := argTypes[fixedCount:]
		if len(extraArgs) == 1 && extraArgs[0] != nil {
			// Single variadic arg — could be T[] (direct pass) or T (element).
			// Use structural check: if arg is a vector/array, unify as T[].
			_, isVec := types.AsVector(extraArgs[0])
			_, isArr := extraArgs[0].(*types.Array)
			if isVec || isArr {
				if !unifyType(fullType, extraArgs[0], bindings) {
					return nil
				}
			} else {
				if !unifyType(elemType, extraArgs[0], bindings) {
					return nil
				}
			}
		} else {
			// Multiple variadic args — each is an element of type T.
			for _, at := range extraArgs {
				if at == nil {
					continue
				}
				if !unifyType(elemType, at, bindings) {
					return nil
				}
			}
		}
	}

	// Check that all type params are bound.
	result := make([]types.Type, len(tparams))
	for i, tp := range tparams {
		bound, ok := bindings[tp]
		if !ok {
			return nil
		}
		result[i] = bound
	}
	return result
}

// unifyType attempts to match paramType against argType and extract TypeParam
// bindings. Returns false if there's a conflict (TypeParam already bound to a
// different type).
func unifyType(paramType, argType types.Type, bindings map[*types.TypeParam]types.Type) bool {
	if paramType == nil || argType == nil {
		return true
	}

	switch pt := paramType.(type) {
	case *types.TypeParam:
		if existing, ok := bindings[pt]; ok {
			return types.Identical(existing, argType)
		}
		bindings[pt] = argType
		return true

	case *types.Instance:
		// Match against Instance with same origin.
		if ai, ok := argType.(*types.Instance); ok {
			if types.Identical(pt.Origin(), ai.Origin()) {
				ptArgs := pt.TypeArgs()
				aiArgs := ai.TypeArgs()
				for i := 0; i < len(ptArgs) && i < len(aiArgs); i++ {
					if !unifyType(ptArgs[i], aiArgs[i], bindings) {
						return false
					}
				}
				return true
			}
		}
		// Array arg matches Vector[T] param: treat array as Vector[elem].
		if arr, ok := argType.(*types.Array); ok {
			if pt.Origin() == types.TypVector && len(pt.TypeArgs()) == 1 {
				return unifyType(pt.TypeArgs()[0], arr.Elem(), bindings)
			}
		}
		return true // different structure, no inference but no conflict

	case *types.Optional:
		if ao, ok := argType.(*types.Optional); ok {
			return unifyType(pt.Elem(), ao.Elem(), bindings)
		}
		// T assigned to T? — also valid for inference
		return unifyType(pt.Elem(), argType, bindings)

	case *types.SharedRef:
		if ar, ok := argType.(*types.SharedRef); ok {
			return unifyType(pt.Elem(), ar.Elem(), bindings)
		}
		// Value assigned to ref — implicit coercion
		return unifyType(pt.Elem(), argType, bindings)

	case *types.MutRef:
		if ar, ok := argType.(*types.MutRef); ok {
			return unifyType(pt.Elem(), ar.Elem(), bindings)
		}
		return unifyType(pt.Elem(), argType, bindings)

	case *types.Pointer:
		if ap, ok := argType.(*types.Pointer); ok {
			return unifyType(pt.Elem(), ap.Elem(), bindings)
		}
		return true

	case *types.Signature:
		as, ok := argType.(*types.Signature)
		if !ok {
			return true
		}
		ptParams := pt.Params()
		asParams := as.Params()
		for i := 0; i < len(ptParams) && i < len(asParams); i++ {
			if !unifyType(ptParams[i].Type(), asParams[i].Type(), bindings) {
				return false
			}
		}
		if pt.Result() != nil && as.Result() != nil {
			return unifyType(pt.Result(), as.Result(), bindings)
		}
		return true

	case *types.Tuple:
		at, ok := argType.(*types.Tuple)
		if !ok {
			return true
		}
		ptElems := pt.Elems()
		atElems := at.Elems()
		for i := 0; i < len(ptElems) && i < len(atElems); i++ {
			if !unifyType(ptElems[i], atElems[i], bindings) {
				return false
			}
		}
		return true

	case *types.Array:
		if aa, ok := argType.(*types.Array); ok {
			return unifyType(pt.Elem(), aa.Elem(), bindings)
		}
		return true

	default:
		return true // concrete type (Named, Enum, etc.), nothing to infer
	}
}

// peekArgType determines the type of an expression without full type-checking.
// Used during type argument inference to avoid side effects from full checking
// (e.g., lambda scope creation, capture recording, duplicate error reports).
// Returns nil for expressions whose type can't be determined without full checking.
func (c *Checker) peekArgType(expr ast.Expr) types.Type {
	// If already checked, return the recorded type.
	if t, ok := c.info.Types[expr]; ok {
		return t
	}

	switch e := expr.(type) {
	case *ast.IntLit:
		if e.Suffix != "" {
			return suffixToType(e.Suffix)
		}
		return types.TypInt
	case *ast.FloatLit:
		if e.Suffix != "" {
			return suffixToType(e.Suffix)
		}
		return types.TypF64
	case *ast.BoolLit:
		return types.TypBool
	case *ast.CharLit:
		return types.TypChar
	case *ast.StringLit:
		return types.TypString
	case *ast.NoneLit:
		return types.TypNone
	case *ast.IdentExpr:
		obj := c.lookup(e.Name)
		if obj == nil {
			return nil
		}
		// Skip non-value objects.
		switch obj.(type) {
		case *types.Module, *types.TypeName:
			return nil
		}
		// For getters, return the result type (not the signature).
		if fn, ok := obj.(*types.Func); ok && fn.IsGetter() {
			if sig, ok := fn.Type().(*types.Signature); ok {
				return sig.Result()
			}
		}
		return obj.Type()
	case *ast.ParenExpr:
		return c.peekArgType(e.Expr)
	case *ast.UnaryExpr:
		return c.peekArgType(e.Operand)
	}
	return nil
}

// inferAndInstantiateCall attempts to infer type arguments for a call to a
// generic function or method. Returns the substituted (concrete) signature,
// or nil if inference fails (error already reported).
func (c *Checker) inferAndInstantiateCall(e *ast.CallExpr, sig *types.Signature) *types.Signature {
	tparams := sig.TypeParams()
	params := sig.Params()

	// Collect argument types using lightweight peek to avoid side effects.
	argTypes := c.collectArgTypesForInference(e, params)

	// Attempt inference.
	typeArgs := inferTypeArgs(tparams, params, argTypes)
	if typeArgs == nil {
		// Build a descriptive name for the error message.
		name := "generic function"
		if ident, ok := e.Callee.(*ast.IdentExpr); ok {
			name = "'" + ident.Name + "'"
		} else if mem, ok := e.Callee.(*ast.MemberExpr); ok {
			name = "'" + mem.Field + "'"
		}
		c.errorf(e.Pos(), "cannot infer type arguments for %s; provide explicit type arguments", name)
		return nil
	}

	// Validate constraints.
	for i, tp := range tparams {
		for _, constraint := range tp.Constraints() {
			if !types.AssignableTo(typeArgs[i], constraint) {
				if cn, ok := constraint.(*types.Named); ok {
					if !types.Implements(typeArgs[i], cn) {
						c.errorf(e.Pos(), "inferred type %s does not satisfy constraint %s", typeArgs[i], constraint)
						return nil
					}
				} else {
					c.errorf(e.Pos(), "inferred type %s does not satisfy constraint %s", typeArgs[i], constraint)
					return nil
				}
			}
		}
	}

	// Build substitution and substitute the signature.
	subst := types.BuildSubstMap(tparams, typeArgs)
	monoSig := types.Substitute(sig, subst).(*types.Signature)

	// T0616: record a generic call edge for the cloneability-requirement
	// propagation pass to validate after Pass 3 completes.
	c.checkCallSiteCloneReqs(e, subst)

	// Record instance for monomorphization.
	c.recordInferredCallInstance(e, typeArgs, monoSig)

	// Record inferred type args for codegen to build mangled names.
	funcName := ""
	if ident, ok := e.Callee.(*ast.IdentExpr); ok {
		funcName = ident.Name
	} else if mem, ok := e.Callee.(*ast.MemberExpr); ok {
		funcName = mem.Field
	}
	c.info.InferredTypeArgs[e] = &InferredCall{
		TypeArgs: typeArgs,
		FuncName: funcName,
	}

	return monoSig
}

// collectArgTypesForInference collects argument types aligned to parameter
// positions using lightweight type peeking. For non-variadic params, argTypes[i]
// corresponds to params[i]. Extra entries are appended for variadic args.
func (c *Checker) collectArgTypesForInference(e *ast.CallExpr, params []*types.Param) []types.Type {
	isVariadic := len(params) > 0 && params[len(params)-1].IsVariadic()
	fixedCount := len(params)
	if isVariadic {
		fixedCount--
	}

	// Build param name → index map for named arg matching.
	paramIndex := make(map[string]int, len(params))
	for i, p := range params {
		paramIndex[p.Name()] = i
	}

	result := make([]types.Type, fixedCount)
	var variadicArgs []types.Type

	positionalIdx := 0
	for _, arg := range e.Args {
		argType := c.peekArgType(arg.Value)
		if arg.Name == "" {
			// Positional arg.
			if positionalIdx < fixedCount {
				result[positionalIdx] = argType
			} else if isVariadic {
				variadicArgs = append(variadicArgs, argType)
			}
			positionalIdx++
		} else {
			// Named arg.
			if idx, ok := paramIndex[arg.Name]; ok {
				if idx < fixedCount {
					result[idx] = argType
				} else if isVariadic {
					variadicArgs = append(variadicArgs, argType)
				}
			}
		}
	}

	// Append variadic args after fixed params.
	result = append(result, variadicArgs...)
	return result
}

// lookupCallee resolves a CallExpr's callee to a (Func, Method) pair so
// call-site validation (cloneability requirements, etc.) can consult the
// callee's per-decl bookkeeping. Mirrors the dispatch in
// recordInferredCallInstance and instantiateGenericFunc.
func (c *Checker) lookupCallee(e *ast.CallExpr) (*types.Func, *types.Method) {
	switch callee := e.Callee.(type) {
	case *ast.IdentExpr:
		if obj := c.lookup(callee.Name); obj != nil {
			if fn, ok := obj.(*types.Func); ok {
				return fn, nil
			}
		}
	case *ast.MemberExpr:
		// Module-qualified function call: mod.func(args)
		if ident, ok := callee.Target.(*ast.IdentExpr); ok {
			if obj := c.info.Objects[ident]; obj != nil {
				if mod, ok := obj.(*types.Module); ok && mod.Scope() != nil {
					if fnObj := mod.Scope().Lookup(callee.Field); fnObj != nil {
						if fn, ok := fnObj.(*types.Func); ok {
							return fn, nil
						}
					}
				}
			}
		}
		// Method call: obj.method(args)
		targetType := c.info.Types[callee.Target]
		if ref, ok := targetType.(*types.MutRef); ok {
			targetType = ref.Elem()
		}
		if ref, ok := targetType.(*types.SharedRef); ok {
			targetType = ref.Elem()
		}
		var owner *types.Named
		switch tt := targetType.(type) {
		case *types.Named:
			owner = tt
		case *types.Instance:
			if n, ok := tt.Origin().(*types.Named); ok {
				owner = n
			}
		}
		if owner != nil {
			if method := owner.LookupMethod(callee.Field); method != nil {
				return nil, method
			}
		}
	}
	return nil, nil
}

// checkCallSiteCloneReqs records a generic call edge so the propagation pass
// can carry callee cloneability requirements onto the caller, validate them
// against the concrete substitution, and emit errors attributed to the call
// site (T0616). All validation happens in propagateCloneReqs after Pass 3 so
// forward references and mutual recursion are handled by a single fixed-point
// iteration without duplicate errors.
//
// For method calls on a generic receiver, the owner's substitution is merged
// into subst so requirements that mix owner and method TypeParams (e.g.
// `Box[T].combine[U](Vector[(T, U)]) { return v.clone(); }`) resolve fully at
// the call site.
func (c *Checker) checkCallSiteCloneReqs(e *ast.CallExpr, subst map[*types.TypeParam]types.Type) {
	if len(subst) == 0 {
		return
	}
	fn, method := c.lookupCallee(e)
	if fn == nil && method == nil {
		return
	}
	if method != nil {
		if ownerSubst := c.ownerSubstForMethodCall(e.Callee); len(ownerSubst) > 0 {
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

// ownerSubstForMethodCall returns the owner's substitution map when callee is
// a method on a generic Instance receiver, else nil. Used by
// checkCallSiteCloneReqs to merge owner TypeParams into the call's method
// subst (T0616).
func (c *Checker) ownerSubstForMethodCall(callee ast.Expr) map[*types.TypeParam]types.Type {
	mem, ok := callee.(*ast.MemberExpr)
	if !ok {
		return nil
	}
	targetType := c.info.Types[mem.Target]
	if ref, ok := targetType.(*types.MutRef); ok {
		targetType = ref.Elem()
	}
	if ref, ok := targetType.(*types.SharedRef); ok {
		targetType = ref.Elem()
	}
	inst, ok := targetType.(*types.Instance)
	if !ok {
		return nil
	}
	named, ok := inst.Origin().(*types.Named)
	if !ok {
		return nil
	}
	return types.BuildSubstMap(named.TypeParams(), inst.TypeArgs())
}

// recordInferredCallInstance records a FuncInstance or MethodInstance for
// monomorphization based on the callee expression of an inferred generic call.
func (c *Checker) recordInferredCallInstance(e *ast.CallExpr, typeArgs []types.Type, monoSig *types.Signature) {
	switch callee := e.Callee.(type) {
	case *ast.IdentExpr:
		// Generic function: identity(42) → identity[int]
		obj := c.lookup(callee.Name)
		if fn, ok := obj.(*types.Func); ok {
			c.info.FuncInstances = append(c.info.FuncInstances, &FuncInstance{
				Func:     fn,
				TypeArgs: typeArgs,
				Sig:      monoSig,
			})
		}
	case *ast.MemberExpr:
		// Check if module-qualified: mod.func(args)
		if ident, ok := callee.Target.(*ast.IdentExpr); ok {
			if obj := c.info.Objects[ident]; obj != nil {
				if mod, ok := obj.(*types.Module); ok && mod.Scope() != nil {
					if fnObj := mod.Scope().Lookup(callee.Field); fnObj != nil {
						if fn, ok := fnObj.(*types.Func); ok {
							c.info.FuncInstances = append(c.info.FuncInstances, &FuncInstance{
								Func:     fn,
								TypeArgs: typeArgs,
								Sig:      monoSig,
							})
							return
						}
					}
				}
			}
		}
		// Method call: obj.method(args)
		targetType := c.info.Types[callee.Target]
		if ref, ok := targetType.(*types.MutRef); ok {
			targetType = ref.Elem()
		}
		if ref, ok := targetType.(*types.SharedRef); ok {
			targetType = ref.Elem()
		}
		var owner *types.Named
		var ownerInst *types.Instance
		switch tt := targetType.(type) {
		case *types.Named:
			owner = tt
		case *types.Instance:
			if n, ok := tt.Origin().(*types.Named); ok {
				owner = n
				ownerInst = tt
			}
		}
		if owner != nil {
			if method := owner.LookupMethod(callee.Field); method != nil {
				defOwner := findMethodDefiner(owner, callee.Field)
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
		}
	}
}

// inferConstructorCall attempts to infer type arguments for a generic type's
// implicit constructor from the call arguments. Returns the instantiated type,
// or nil if inference fails (error already reported).
func (c *Checker) inferConstructorCall(e *ast.CallExpr, named *types.Named) *types.Instance {
	// Collect arg names and types using peek.
	argNames := make([]string, len(e.Args))
	argTypes := make([]types.Type, len(e.Args))
	for i, arg := range e.Args {
		argNames[i] = arg.Name
		argTypes[i] = c.peekArgType(arg.Value)
	}

	typeArgs := inferConstructorTypeArgs(named, argNames, argTypes)
	if typeArgs == nil {
		c.errorf(e.Pos(), "cannot infer type arguments for %s; provide explicit type arguments", named)
		return nil
	}

	// Validate constraints.
	c.validateConstraints(e.Pos(), named, typeArgs)
	c.validateSendableInstance(e.Pos(), named, typeArgs)
	c.validateSingleOwnerContainerInstance(e.Pos(), named, typeArgs)

	inst := types.NewInstance(named, typeArgs)
	c.recordInstance(inst)

	// Update the callee's recorded type so codegen sees the Instance, not the
	// uninstantiated Named type. This is critical — codegen dispatches on
	// c.info.Types[e.Callee] to find the correct mono layout.
	c.recordType(e.Callee, inst)

	return inst
}

// inferConstructorTypeArgs infers type arguments for a generic type constructor
// from the constructor arguments. Works for implicit constructors (field-based).
// Handles both positional args (matched by field order) and named args (matched by name).
func inferConstructorTypeArgs(named *types.Named, argNames []string, argTypes []types.Type) []types.Type {
	tparams := named.TypeParams()
	if len(tparams) == 0 {
		return nil
	}

	fields := named.AllFields()
	bindings := make(map[*types.TypeParam]types.Type, len(tparams))

	// Build field name → index map for named arg lookup.
	fieldIndex := make(map[string]int, len(fields))
	for i, f := range fields {
		fieldIndex[f.Name()] = i
	}

	// Match each argument against its corresponding field type.
	for i := range argNames {
		if i >= len(argTypes) || argTypes[i] == nil {
			continue
		}
		var fieldType types.Type
		if argNames[i] == "" {
			// Positional arg — match by field order.
			if i < len(fields) {
				fieldType = fields[i].Type()
			}
		} else {
			// Named arg — match by field name.
			if idx, ok := fieldIndex[argNames[i]]; ok && idx < len(fields) {
				fieldType = fields[idx].Type()
			}
		}
		if fieldType != nil {
			if !unifyType(fieldType, argTypes[i], bindings) {
				return nil
			}
		}
	}

	// Check that all type params are bound.
	result := make([]types.Type, len(tparams))
	for i, tp := range tparams {
		bound, ok := bindings[tp]
		if !ok {
			return nil
		}
		result[i] = bound
	}
	return result
}
