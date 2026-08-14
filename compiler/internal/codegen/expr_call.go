package codegen

import (
	"fmt"
	"math"

	"github.com/llir/llvm/ir"
	"github.com/llir/llvm/ir/constant"
	"github.com/llir/llvm/ir/enum"
	irtypes "github.com/llir/llvm/ir/types"
	"github.com/llir/llvm/ir/value"

	"github.com/promise-language/promise/compiler/internal/ast"
	"github.com/promise-language/promise/compiler/internal/sema"
	"github.com/promise-language/promise/compiler/internal/types"
)

// --- Call expressions ---

// genFunctionMemberIndirectCall dispatches `member(...)` indirectly through the
// fat pointer the callee (a function-typed field or getter) yields, resolving the
// signature under the active mono subst. Shared by the Named (T1253) and enum
// (T1258) direct-call arms of genCallExpr.
func (c *Compiler) genFunctionMemberIndirectCall(e *ast.CallExpr, sig *types.Signature) value.Value {
	// Resolve the signature under the active mono subst so the indirect dispatch
	// sees concrete param/result types when the owner is a generic instance
	// (matches the T1251 module path).
	resolvedSig := sig
	if c.typeSubst != nil {
		if s, ok := types.Substitute(sig, c.typeSubst).(*types.Signature); ok {
			resolvedSig = s
		}
	}
	closure := c.genExpr(e.Callee) // field load, or getter call
	var argVals []value.Value
	for _, arg := range e.Args {
		argVals = append(argVals, c.genCallArgExpr(arg.Value))
	}
	origArgVals := argVals // T0331: pre-coercion for alias check
	argVals = c.coerceIndirectCallArgs(resolvedSig, e.Args, argVals)
	result := c.genIndirectCall(closure, resolvedSig, argVals)
	result = c.emitReturnAliasCheck(result, resolvedSig, e.Args, origArgVals, e) // T0331
	return result
}

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
				// T1251: A module-level getter whose return type is a function type
				// (`get adder() -> int`) — the trailing `()` invokes the *returned
				// closure*, not the getter. Sema records the member in ModuleGetters
				// and types `lib.adder()` as the closure's result. Evaluate the getter
				// (genMemberExpr → genModuleGetterCall materializes the {fn,env} fat
				// pointer and tracks its env for cleanup) then dispatch indirectly.
				// Without this the default arm treats `()` as the getter's own call and
				// returns the closure struct where its result value is expected.
				if c.info.ModuleGetters[member] {
					calleeType := c.info.Types[e.Callee]
					if c.typeSubst != nil {
						calleeType = types.Substitute(calleeType, c.typeSubst)
					}
					if sig, ok := calleeType.(*types.Signature); ok {
						closure := c.genExpr(e.Callee)
						var argVals []value.Value
						for _, arg := range e.Args {
							argVals = append(argVals, c.genCallArgExpr(arg.Value))
						}
						origArgVals := argVals // T0331: pre-coercion for alias check
						argVals = c.coerceIndirectCallArgs(sig, e.Args, argVals)
						result := c.genIndirectCall(closure, sig, argVals)
						result = c.emitReturnAliasCheck(result, sig, e.Args, origArgVals, e) // T0331
						return result
					}
				}
				calleeType := c.info.Types[e.Callee]
				switch calleeType.(type) {
				case *types.Named, *types.Instance:
					// Module-qualified constructor: mod.Type(args)
					return c.genConstructorCallMono(e, calleeType)
				case *types.Enum:
					// Module-qualified enum — fall through to enum dispatch below
				default:
					// Module-qualified function call: mod.func(args)
					// Module-qualified generic call with inferred type args: mod.func(args)
					if inferred, ok := c.info.InferredTypeArgs[e]; ok {
						return c.genInferredGenericCall(e, inferred)
					}
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
		// Function-typed field or getter call: `this._next()` where _next is a
		// () -> T? field, or `l.adder()` where adder is a getter whose return type
		// is a function type (T1253). In both cases the trailing `()` invokes the
		// closure the member yields, not a method — dispatch indirectly through the
		// fat pointer. Without the getter arm the `()` falls through to
		// genMethodCall and panics with "no method adder on type Lib". genExpr
		// routes the getter to genGetterCall, which materializes the {fn,env} fat
		// pointer and (T1253) tracks its env for cleanup at statement end.
		if sig, ok := c.info.Types[e.Callee].(*types.Signature); ok {
			memberTargetType := c.info.Types[member.Target]
			if c.typeSubst != nil {
				memberTargetType = types.Substitute(memberTargetType, c.typeSubst)
			}
			if c.selfSubst != nil {
				memberTargetType = types.SubstituteSelf(memberTargetType, c.selfSubst.iface, c.selfSubst.concrete)
			}
			if named := extractNamed(memberTargetType); named != nil {
				isField := named.LookupField(member.Field) != nil
				isGetter := !isField && named.LookupGetter(member.Field) != nil
				if isField || isGetter {
					return c.genFunctionMemberIndirectCall(e, sig)
				}
			} else if enum := extractEnum(memberTargetType); enum != nil {
				// T1258: enum analog of T1253 — an enum getter whose return type is
				// a function type, invoked directly (`e.adder()`). extractNamed is
				// nil for *types.Enum / enum *types.Instance, so this arm dispatches
				// the trailing () indirectly through the getter's fat pointer instead
				// of falling through to genMethodCall (which panics). genExpr(e.Callee)
				// routes to genEnumGetterAccess, which materializes the {fn,env}
				// pointer and tracks the env for cleanup. Enums have no fields-by-name,
				// so only the getter case applies.
				if enum.LookupGetter(member.Field) != nil {
					return c.genFunctionMemberIndirectCall(e, sig)
				}
			}
		}
		// T0642: Inferred method-level type args. Sema recorded the inferred type
		// args; route through the generic method path which builds the mono name
		// + subst. Mirror the IndexExpr-path post-call handling — generic
		// structural default methods are compiled without selfSubst, so the
		// iterator's _parent isn't set; don't claim the receiver (B0213).
		if inferred, ok := c.info.InferredTypeArgs[e]; ok {
			savedReceiverClaim := c.pendingReceiverClaim
			c.pendingReceiverClaim = nil
			result := c.genGenericMethodCall(e, member, inferred.TypeArgs)
			heapBeforeTrack := len(c.heapTemps)
			c.maybeTrackIterTemp(e, result)
			if len(c.heapTemps) > heapBeforeTrack {
				c.claimAllEnvTemps()
			}
			c.pendingReceiverClaim = savedReceiverClaim
			return result
		}
		savedReceiverClaim := c.pendingReceiverClaim // T0130: save across nested calls
		c.pendingReceiverClaim = nil
		result := c.genMethodCall(e, member)
		heapBeforeTrack := len(c.heapTemps) // B0213: capture before maybeTrackIterTemp
		c.maybeTrackIterTemp(e, result)
		// T0130: Claim the receiver's heap temp ONLY when the method returns a
		// structural interface (combinator like filter/take/skip). Terminal operations
		// (count, collect, find) return non-structural types — their receiver should
		// be freed at statement end, not claimed.
		if c.pendingReceiverClaim != nil {
			callResultType := c.info.Types[e]
			if c.typeSubst != nil {
				callResultType = types.Substitute(callResultType, c.typeSubst)
			}
			if resultNamed := extractNamed(callResultType); resultNamed != nil && resultNamed.IsStructural() {
				c.claimHeapTemp(c.pendingReceiverClaim)
			}
		}
		// T0100/B0213: Claim env temps only when THIS call's result was tracked
		// as a new heapTemp (combinator that stores the lambda in the returned
		// iterator). Don't claim when the receiver evaluation created heapTemps —
		// terminal operations (for_each, any, fold) don't store lambdas.
		if len(c.heapTemps) > heapBeforeTrack {
			c.claimAllEnvTemps()
		}
		c.pendingReceiverClaim = savedReceiverClaim // T0130: restore
		return result
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
			// Arc constructor: Ref[T](value)
			if origin == types.TypArc {
				return c.genArcConstructor(e, inst)
			}
			// Mutex constructor: Mutex[T](value)
			if origin == types.TypMutex {
				return c.genMutexConstructor(e, inst)
			}
			return c.genConstructorCallMono(e, calleeType)
		}
	}
	if named, ok := calleeType.(*types.Named); ok {
		if _, isIdent := e.Callee.(*ast.IdentExpr); isIdent {
			return c.genConstructorCallMono(e, named)
		}
	}

	// Generic function/method call: callee is IndexExpr (identity[int](42) or obj.method[int](42)).
	// T0674: Only route to the generic-instantiation path when the indexed target's
	// recorded type is a generic Signature (mirrors sema's rule in checkIndexExpr:
	// `[...]` is a type-argument list only when the target is a generic func/method).
	// A value subscript that yields a callable — e.g. fns[0](x) where fns is a
	// Vector[(int) -> int], or h.fns[0](x) for a function-typed field — is NOT a
	// generic instantiation; it falls through to the closure-value call path below
	// (T0817), which materializes the {fn, env} fat pointer and dispatches indirectly.
	// Without this gate, fns[0](x) mangled a bogus generic name ("fns[int]") and
	// panicked, and h.fns[0](x) mis-routed to genGenericMethodCall ("no method fns").
	if idx, ok := e.Callee.(*ast.IndexExpr); ok {
		targetType := c.info.Types[idx.Target]
		if c.typeSubst != nil && targetType != nil {
			targetType = types.Substitute(targetType, c.typeSubst)
		}
		sig, isSig := targetType.(*types.Signature)
		if isSig && len(sig.TypeParams()) > 0 {
			if member, ok := idx.Target.(*ast.MemberExpr); ok {
				// Check if this is a module-qualified generic function call (json.encode_string[Config](...))
				// vs. an instance generic method call (box.transform[string](...))
				if ident, ok := member.Target.(*ast.IdentExpr); ok {
					if c.resolveModuleName(ident) != "" {
						return c.genModuleGenericFuncCall(e, idx, member.Field)
					}
				}
				savedReceiverClaim2 := c.pendingReceiverClaim // T0130
				c.pendingReceiverClaim = nil
				typeArgExprs := append([]ast.Expr{idx.Index}, idx.ExtraIndices...)
				typeArgs := make([]types.Type, len(typeArgExprs))
				for i, expr := range typeArgExprs {
					typeArgs[i] = c.info.Types[expr]
				}
				result := c.genGenericMethodCall(e, member, typeArgs)
				heapBeforeTrack2 := len(c.heapTemps) // B0213: capture before maybeTrackIterTemp
				c.maybeTrackIterTemp(e, result)
				// B0213: Don't claim receiver for generic method calls. Generic structural
				// default methods (map[R], zip[U], flat_map[R]) are compiled without selfSubst,
				// so _parent is not set on the returned _FnIter. The receiver stays as an
				// unclaimed heapTemp and is cleaned independently at statement end.
				// B0213: Only claim env temps when THIS call's result was tracked as a new
				// heapTemp (not when the receiver evaluation created heapTemps).
				if len(c.heapTemps) > heapBeforeTrack2 {
					c.claimAllEnvTemps()
				}
				c.pendingReceiverClaim = savedReceiverClaim2 // T0130
				return result
			}
			return c.genGenericFuncCall(e, idx)
		}
	}

	// Inferred generic function call: sema recorded the inferred type args.
	if inferred, ok := c.info.InferredTypeArgs[e]; ok {
		savedReceiverClaim3 := c.pendingReceiverClaim // T0130
		c.pendingReceiverClaim = nil
		result := c.genInferredGenericCall(e, inferred)
		heapBeforeTrack3 := len(c.heapTemps) // B0213: capture before maybeTrackIterTemp
		c.maybeTrackIterTemp(e, result)
		// B0213: Don't claim receiver for inferred generic calls (same rationale as
		// genGenericMethodCall — _parent not set for generic structural defaults).
		// Only claim env temps when THIS call's result was tracked.
		if len(c.heapTemps) > heapBeforeTrack3 {
			c.claimAllEnvTemps()
		}
		c.pendingReceiverClaim = savedReceiverClaim3 // T0130
		return result
	}

	// T0817: Closure call through a non-ident callee whose static type is a
	// function signature — e.g. a force-unwrapped optional closure `o!()`, or a
	// parenthesized `(expr)()`. genExpr materializes the fat pointer {fn, env};
	// dispatch it through the same indirect-call path as any other closure. Ident
	// callees fall through to the locals-based lambda path below (it loads the fat
	// pointer from the alloca and handles return-alias checks).
	if _, isIdent := e.Callee.(*ast.IdentExpr); !isIdent {
		calleeType := c.info.Types[e.Callee]
		if c.typeSubst != nil {
			calleeType = types.Substitute(calleeType, c.typeSubst)
		}
		if sig, ok := calleeType.(*types.Signature); ok {
			closure := c.genExpr(e.Callee)
			var argVals []value.Value
			for _, arg := range e.Args {
				argVals = append(argVals, c.genCallArgExpr(arg.Value))
			}
			origArgVals := argVals // T0331: pre-coercion for alias check
			argVals = c.coerceIndirectCallArgs(sig, e.Args, argVals)
			result := c.genIndirectCall(closure, sig, argVals)
			result = c.emitReturnAliasCheck(result, sig, e.Args, origArgVals, e) // T0331
			return result
		}
	}

	// Resolve callee first to detect MutRef params (B0149)
	ident, ok := e.Callee.(*ast.IdentExpr)
	if !ok {
		panic(fmt.Sprintf("codegen: unsupported callee type %T", e.Callee))
	}

	// Look up callee signature for MutRef param detection.
	// Extern functions use C ABI — skip MutRef pointer-passing for them.
	var calleeSig *types.Signature
	isExtern := false
	if _, ok := c.externs[ident.Name]; ok {
		isExtern = true
	}
	if !isExtern {
		if callee := c.lookupFunc(ident.Name); callee != nil {
			calleeSig, _ = callee.Type().(*types.Signature)
		}
	}

	// Evaluate arguments — pass address for MutRef params (B0149)
	var argVals []value.Value
	var argTypes []types.Type
	var variadicPTs []variadicPassthrough // B0203
	if calleeSig != nil {
		argVals, argTypes, variadicPTs = c.genCallArgsWithMutRef(e.Args, calleeSig.Params(), calleeSig.Result())
	} else {
		for _, arg := range e.Args {
			argVals = append(argVals, c.genCallArgExpr(arg.Value))
			argTypes = append(argTypes, c.info.Types[arg.Value])
		}
	}

	// Lambda call: callee is a local variable holding a fat pointer {i8*, i8*}
	if alloca, ok := c.locals[ident.Name]; ok {
		calleeType := c.info.Types[e.Callee]
		if sig, ok := calleeType.(*types.Signature); ok {
			closure := c.block.NewLoad(alloca.ElemType, alloca)
			origArgVals := argVals // T0331: pre-coercion for alias check
			argVals = c.coerceIndirectCallArgs(sig, e.Args, argVals)
			result := c.genIndirectCall(closure, sig, argVals)
			c.clearVariadicStaticFlags(variadicPTs)
			result = c.emitReturnAliasCheck(result, sig, e.Args, origArgVals, e) // T0331
			return result
		}
	}

	// Extern function — pack args into value structs, call, unpack return
	if isExtern {
		ext := c.externs[ident.Name]
		// Intercept netpoll wait operations — emit inline coro.suspend (T0232)
		// The PollDesc pointer is stored as a Promise int. argVals[0] may be
		// a raw i64 (field access) or a value struct {i8*, T_i*, i64} (local var).
		if ext.CName == "promise_netpoll_wait_read" {
			c.genNetpollWaitRead(c.extractI64FromIntArg(argVals[0]))
			c.clearVariadicStaticFlags(variadicPTs)
			return nil
		}
		if ext.CName == "promise_netpoll_wait_write" {
			c.genNetpollWaitWrite(c.extractI64FromIntArg(argVals[0]))
			c.clearVariadicStaticFlags(variadicPTs)
			return nil
		}
		result := c.genExternCall(ext, argVals, argTypes)
		c.clearVariadicStaticFlags(variadicPTs)
		return result
	}

	// Regular function call
	fn, ok := c.funcs[ident.Name]
	if !ok {
		panic(fmt.Sprintf("codegen: undefined function %q", ident.Name))
	}

	// Coerce arguments when crossing type boundaries
	origArgVals := argVals // B0345: save pre-coercion values for alias check
	if calleeSig != nil {
		argVals = c.coerceCallArgs(argVals, argTypes, calleeSig.Params(), e.Args, nil)
	}

	var result value.Value = c.block.NewCall(fn, argVals...)
	c.clearVariadicStaticFlags(variadicPTs)

	// B0345: If the return value aliases an argument, clear the argument's drop flag
	// to prevent double-free. E.g., identity(v) returns v's pointer — without this,
	// both v and the return value would be dropped at scope exit.
	result = c.emitReturnAliasCheck(result, calleeSig, e.Args, origArgVals, e)

	// T0092: Track string return values from functions with structural interface
	// parameters. When a function takes a structural interface param and returns
	// a string, the result is typically a new allocation (from format/encode/
	// to_string on the structural param). Track it so it's freed at statement end.
	// Note: if the function internally calls to_string() on a string-typed
	// structural param, the return aliases the input. This is safe for literals
	// (promise_string_drop is a no-op) and for encoder-style functions (return
	// is always a new allocation). A display(heap_string_var) pattern where the
	// return aliases the variable would require ownership tracking (T0061) for
	// full safety.
	if result != nil && result.Type() == irtypes.I8Ptr && c.tempTrackingEnabled {
		if calleeSig != nil && hasStructuralParam(calleeSig, c.typeSubst) {
			if rt := c.info.Types[e]; rt != nil && extractNamed(rt) == types.TypString {
				c.trackStringTemp(result)
			}
		}
	}

	return result
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
	// Check if the callee is an extern (C ABI — skip MutRef pointer-passing)
	key := moduleName + "." + funcName
	isExtern := false
	if _, ok := c.moduleExterns[key]; ok {
		isExtern = true
	}

	// Look up callee signature for MutRef param detection (B0149)
	var calleeSig *types.Signature
	if !isExtern {
		if sig, ok := c.info.Types[e.Callee].(*types.Signature); ok {
			calleeSig = sig
		}
	}

	var argVals []value.Value
	var argTypes []types.Type
	var variadicPTs []variadicPassthrough // B0203
	if calleeSig != nil {
		argVals, argTypes, variadicPTs = c.genCallArgsWithMutRef(e.Args, calleeSig.Params(), calleeSig.Result())
	} else {
		for _, arg := range e.Args {
			argVals = append(argVals, c.genCallArgExpr(arg.Value))
			argTypes = append(argTypes, c.info.Types[arg.Value])
		}
	}

	// Try module extern first
	if ext, ok := c.moduleExterns[key]; ok {
		result := c.genExternCall(ext, argVals, argTypes)
		c.clearVariadicStaticFlags(variadicPTs)
		return result
	}

	// Try module function
	fn, ok := c.moduleFuncs[key]
	if !ok {
		panic(fmt.Sprintf("codegen: undefined module function %s.%s", moduleName, funcName))
	}

	// Coerce arguments using the callee's signature from sema
	origArgVals := argVals // B0345
	if calleeSig != nil {
		argVals = c.coerceCallArgs(argVals, argTypes, calleeSig.Params(), e.Args, nil)
	}

	var result value.Value = c.block.NewCall(fn, argVals...)
	c.clearVariadicStaticFlags(variadicPTs)
	result = c.emitReturnAliasCheck(result, calleeSig, e.Args, origArgVals, e) // B0345
	return result
}

// genModuleGetterCall handles mod.property access — calls the getter function with no args.
func (c *Compiler) genModuleGetterCall(e *ast.MemberExpr, moduleName, propName string) value.Value {
	key := moduleName + "." + propName
	fn, ok := c.moduleFuncs[key]
	if !ok {
		panic(fmt.Sprintf("codegen: undefined module getter %s.%s", moduleName, propName))
	}
	result := c.block.NewCall(fn)
	retType := c.info.Types[e]
	if c.typeSubst != nil && retType != nil {
		retType = types.Substitute(retType, c.typeSubst)
	}
	if c.selfSubst != nil && retType != nil {
		retType = types.SubstituteSelf(retType, c.selfSubst.iface, c.selfSubst.concrete)
	}
	// T1240: A getter returning a function type (`get adder() -> int`) yields an
	// owned closure whose heap env must be freed. Track its env (field 1 of the
	// fat pointer) as an env temp so cleanupEnvTemps frees it when the result is
	// discarded; if it's bound to a variable, claimEnvTemp releases the temp and
	// maybeRegisterEnvFree takes over ownership (single free either way).
	if _, isSig := retType.(*types.Signature); isSig {
		envPtr := c.block.NewExtractValue(result, 1)
		c.trackEnvTemp(envPtr)
		return result
	}
	// T0137/T0486/T1250: track every heap-owning result kind (string, vector,
	// channel, Arc/Weak/Mutex/Task, heap user type) so a discarded temporary is
	// freed at statement end; the binding/assignment path claims it (claimStringTemp
	// / claimHeapTemp) so a single owner frees it either way. Shares the dispatch
	// with the instance-getter path (trackGetterResult) — the guards in the tracked
	// helpers make it a no-op for value/copy/static-container results.
	c.trackGetterResultByType(e, retType, result)
	return result
}

// genGenericCallArgs evaluates arguments for a monomorphic generic free/module
// function call. It substitutes the callee signature's params with the call's
// type-arg subst so genCallArgsWithMutRef sees concrete param types — without
// this, `~`/`&` generic-instance (and string/Vector) params are passed by
// value while the monomorphic callee expects a pointer, producing an ABI
// mismatch and a runtime segfault (T0639). When the signature can't be
// resolved it falls back to the plain by-value loop (old behavior). This makes
// the generic call paths use the same MutRef-aware arg generation +
// T0087/B0201 ownership transfer as the non-generic path (genCallExpr).
func (c *Compiler) genGenericCallArgs(args []*ast.Arg, sig *types.Signature, subst map[*types.TypeParam]types.Type) ([]value.Value, []types.Type, []variadicPassthrough) {
	if sig == nil {
		var argVals []value.Value
		var argTypes []types.Type
		for _, arg := range args {
			argVals = append(argVals, c.genCallArgExpr(arg.Value))
			c.dupStringFieldAccess = false
			c.dupContainerFieldAccess = false
			c.dupHeapUserFieldAccess = false // T0403
			argTypes = append(argTypes, c.exprType(arg.Value))
		}
		return argVals, argTypes, nil
	}
	params := sig.Params()
	result := sig.Result()
	if len(subst) > 0 {
		params = make([]*types.Param, len(sig.Params()))
		for i, p := range sig.Params() {
			np := types.NewParam(p.Name(), types.Substitute(p.Type(), subst), p.Ref())
			np.SetVariadic(p.IsVariadic())
			params[i] = np
		}
		result = types.Substitute(result, subst)
	}
	// T1233: pass the (substituted) result so genCallArgsWithMutRef can detect a
	// generator callee — its stream[T] param lifetime outlives the call statement.
	return c.genCallArgsWithMutRef(args, params, result)
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

	// T0639: Resolve the callee signature + per-call type-arg subst BEFORE arg
	// generation so genGenericCallArgs can pass `~`/`&` params by pointer with
	// concrete (substituted) param types. T0418: the subst also resolves
	// generic params like T? at the call site even when the outer mono context
	// (c.typeSubst) doesn't cover the callee's TypeParams.
	var calleeSig *types.Signature
	var callSubst map[*types.TypeParam]types.Type
	if callee := c.lookupFunc(ident.Name); callee != nil {
		if sig, sOk := callee.Type().(*types.Signature); sOk {
			calleeSig = sig
			callSubst = c.buildCallTypeArgSubst(sig.TypeParams(), allTypeArgExprs)
		}
	}
	if calleeSig == nil {
		if sig, sOk := c.info.Types[e.Callee].(*types.Signature); sOk {
			calleeSig = sig
		}
	}

	argVals, argTypes, variadicPTs := c.genGenericCallArgs(e.Args, calleeSig, callSubst)
	origArgVals := argVals // T0331: save pre-coercion values for alias check

	if calleeSig != nil {
		argVals = c.coerceCallArgs(argVals, argTypes, calleeSig.Params(), e.Args, callSubst)
	}

	var result value.Value = c.block.NewCall(fn, argVals...)
	c.clearVariadicStaticFlags(variadicPTs)
	result = c.emitReturnAliasCheckSubst(result, calleeSig, e.Args, origArgVals, callSubst, e) // T0331/T0418
	return result
}

// genInferredGenericCall generates a call to a monomorphic generic function
// where the type arguments were inferred by sema (not explicit in the AST).
func (c *Compiler) genInferredGenericCall(e *ast.CallExpr, inferred *sema.InferredCall) value.Value {
	// Build mangled name from inferred type args.
	mangledName := inferred.FuncName + "["
	for i, ta := range inferred.TypeArgs {
		if c.typeSubst != nil {
			ta = types.Substitute(ta, c.typeSubst)
		}
		if i > 0 {
			mangledName += ", "
		}
		mangledName += typeArgStr(ta)
	}
	mangledName += "]"

	fn, ok := c.funcs[mangledName]
	if !ok {
		panic(fmt.Sprintf("codegen: undefined inferred monomorphic function %q", mangledName))
	}

	// T0639: Resolve the callee signature + inferred-type-arg subst BEFORE arg
	// generation so genGenericCallArgs passes `~`/`&` params by pointer with
	// concrete (substituted) param types.
	var calleeSig *types.Signature
	var callSubst map[*types.TypeParam]types.Type
	if callee := c.lookupFunc(inferred.FuncName); callee != nil {
		if sig, sOk := callee.Type().(*types.Signature); sOk {
			calleeSig = sig
			callSubst = c.buildInferredCallSubst(sig.TypeParams(), inferred.TypeArgs)
		}
	}
	if calleeSig == nil {
		if sig, sOk := c.info.Types[e.Callee].(*types.Signature); sOk {
			calleeSig = sig
		}
	}

	argVals, argTypes, variadicPTs := c.genGenericCallArgs(e.Args, calleeSig, callSubst)
	origArgVals := argVals // T0331: save pre-coercion values for alias check

	if calleeSig != nil {
		argVals = c.coerceCallArgs(argVals, argTypes, calleeSig.Params(), e.Args, callSubst)
	}

	var result value.Value = c.block.NewCall(fn, argVals...)
	c.clearVariadicStaticFlags(variadicPTs)
	result = c.emitReturnAliasCheckSubst(result, calleeSig, e.Args, origArgVals, callSubst, e) // T0331/T0418
	return result
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

	// T0639: Resolve the callee signature + per-call type-arg subst BEFORE arg
	// generation so genGenericCallArgs passes `~`/`&` params by pointer with
	// concrete (substituted) param types.
	var calleeSig *types.Signature
	var callSubst map[*types.TypeParam]types.Type
	if callee := c.lookupFunc(funcName); callee != nil {
		if sig, sOk := callee.Type().(*types.Signature); sOk {
			calleeSig = sig
			callSubst = c.buildCallTypeArgSubst(sig.TypeParams(), allTypeArgExprs)
		}
	}
	// Module-qualified callee may not be visible via lookupFunc; fall back to
	// the callee expression's type recorded by sema.
	if calleeSig == nil {
		if sig, sOk := c.info.Types[e.Callee].(*types.Signature); sOk {
			calleeSig = sig
		}
	}

	argVals, argTypes, variadicPTs := c.genGenericCallArgs(e.Args, calleeSig, callSubst)
	origArgVals := argVals // T0331: save pre-coercion values for alias check

	if calleeSig != nil {
		argVals = c.coerceCallArgs(argVals, argTypes, calleeSig.Params(), e.Args, callSubst)
	}

	var result value.Value = c.block.NewCall(fn, argVals...)
	c.clearVariadicStaticFlags(variadicPTs)
	result = c.emitReturnAliasCheckSubst(result, calleeSig, e.Args, origArgVals, callSubst, e) // T0331/T0418
	return result
}

// genGenericMethodCall generates a call to a monomorphized generic method.
// Example: box.transform[string](fn) → "Box.transform[string]"(this, fn)
// Example: box.transform[string](fn) where box is Box[int] → "Box[int].transform[string]"(this, fn)
//
// typeArgs are the concrete method-level type arguments (already extracted from
// either an explicit IndexExpr or sema's InferredTypeArgs). c.typeSubst is
// applied to each arg before mangling.
func (c *Compiler) genGenericMethodCall(e *ast.CallExpr, member *ast.MemberExpr, typeArgs []types.Type) value.Value {
	targetType := c.info.Types[member.Target]
	if c.typeSubst != nil {
		targetType = types.Substitute(targetType, c.typeSubst)
	}

	// T0636: generic method on a generic enum instance (or via `this` inside a
	// generic enum body). Enums don't have a Named layout, so route to the
	// enum-specific call path.
	if extractEnum(targetType) != nil {
		return c.genGenericEnumMethodCall(e, member, typeArgs, targetType)
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
	mangledName := mangleMethodName(defOwnerName, member.Field, false) + "["
	for i, ta := range typeArgs {
		if c.typeSubst != nil && ta != nil {
			ta = types.Substitute(ta, c.typeSubst)
		}
		if i > 0 {
			mangledName += ", "
		}
		mangledName += typeArgStr(ta)
	}
	mangledName += "]"

	fn, ok := c.funcs[mangledName]
	if !ok {
		panic(fmt.Sprintf("codegen: undefined monomorphic method %q", mangledName))
	}

	// Generate receiver
	var args []value.Value
	if method.Sig().Recv() != nil {
		// T1358: A value-type non-`this` receiver on a `~this` (mutable-borrow)
		// method must pass the caller's in-place storage address so field/setter
		// mutations reach the caller's variable — not a spilled copy. See
		// genMethodCall for the full rationale.
		if named.IsValueType() && !isThisReceiver(member.Target) {
			args = append(args, c.genValueTypeReceiverArg(member.Target, targetType,
				method.Sig().Recv().Ref() == types.RefMut))
		} else {
			target := c.genExprAutoPropagate(member.Target) // B0323
			// T0130: Defer receiver claim — only claim if method produces a new iterator
			// (combinator). Terminal operations (count, collect, find) don't capture the
			// receiver, so the heap temp should be freed at statement end.
			c.pendingReceiverClaim = target
			if isThisReceiver(member.Target) {
				args = append(args, target)
			} else if isContainerType(targetType) {
				args = append(args, target)
			} else if isPrimitiveScalar(named) {
				args = append(args, target)
			} else {
				instancePtr := c.extractInstancePtr(target)
				args = append(args, instancePtr)
				// B0258: Track method chain intermediate for cleanup at statement end.
				c.trackChainIntermediateReceiver(member.Target, target, instancePtr, named, targetType)
			}
		}
	}

	// Generate arguments
	// T0418: Combine owner-type subst (Box[int].T → int + parents) with
	// method-level subst (transform[string].T → string).
	// T1223: compute the subst BEFORE evaluating args and route arg-gen through
	// genGenericCallArgs so genCallArgsWithMutRef sees CONCRETE param types. Without
	// this, a `T move` param (e.g. Set[string].add(T move elem)) reaches
	// maybeEnableDupForMutRefArg as the unsubstituted TypeParam `T`, so the field-read
	// dup that every move sink arms (T0366) is skipped — `out.add(this.label)` then
	// stores an alias of the owner's inner buffer into the set → UAF when the owner drops.
	ownerSubst := c.buildOwnerTypeArgSubst(targetType)
	methodSubst := c.buildInferredCallSubst(method.Sig().TypeParams(), typeArgs)
	combined := mergeSubstMaps(ownerSubst, methodSubst)
	argVals, argTypes, variadicPTs := c.genGenericCallArgs(e.Args, method.Sig(), combined)
	origArgVals := argVals // B0345: save pre-coercion values
	argVals = c.coerceCallArgs(argVals, argTypes, method.Sig().Params(), e.Args, combined)
	args = append(args, argVals...)

	var result value.Value = c.block.NewCall(fn, args...)
	c.clearVariadicStaticFlags(variadicPTs)
	result = c.emitReturnAliasCheckSubst(result, method.Sig(), e.Args, origArgVals, combined, e) // B0345/T0418
	return result
}

// genGenericEnumMethodCall generates a call to a monomorphized generic method
// whose receiver is a generic enum instance (T0636). It mirrors the receiver
// convention of genEnumMethodCall (pass `this` directly; otherwise store to a
// temp alloca, bitcast to i8*, and enum-drop fresh temporaries after the call)
// and the mono-name/subst construction of genGenericMethodCall.
//
// typeArgs are the concrete method-level type arguments (already extracted from
// either an explicit IndexExpr or sema's InferredTypeArgs). c.typeSubst is
// applied to each arg before mangling.
func (c *Compiler) genGenericEnumMethodCall(e *ast.CallExpr, member *ast.MemberExpr, typeArgs []types.Type, targetType types.Type) value.Value {
	// T0639: a ~/& generic-enum-instance receiver arrives wrapped in a
	// MutRef/SharedRef; unwrap so the enum + monoName resolve instead of
	// hitting the "cannot resolve enum" panic.
	if ref, ok := targetType.(*types.MutRef); ok {
		return c.genGenericEnumMethodCall(e, member, typeArgs, ref.Elem())
	}
	if ref, ok := targetType.(*types.SharedRef); ok {
		return c.genGenericEnumMethodCall(e, member, typeArgs, ref.Elem())
	}
	var enum *types.Enum
	var enumName string
	switch t := targetType.(type) {
	case *types.Enum:
		enum = t
		enumName = t.Obj().Name()
		// Inside a mono enum method body, `this` is the origin enum — use the
		// monomorphized instance name (mirrors genEnumMethodCall).
		if c.monoCtx != nil {
			if origin, ok := c.monoCtx.origin.(*types.Enum); ok && t == origin {
				enumName = c.monoCtx.name
			}
		}
	case *types.Instance:
		if en, ok := t.Origin().(*types.Enum); ok {
			enum = en
			enumName = monoName(t)
		}
	}
	if enum == nil {
		panic(fmt.Sprintf("codegen: cannot resolve enum for generic method call on %T", targetType))
	}

	method := enum.LookupMethod(member.Field)
	if method == nil {
		panic(fmt.Sprintf("codegen: no method %s on enum %s", member.Field, enumName))
	}

	// Build mono method name: EnumMonoName.method[typearg1, typearg2]
	// (consistent with monoMethodInstanceName).
	mangledName := mangleMethodName(enumName, member.Field, false) + "["
	for i, ta := range typeArgs {
		if c.typeSubst != nil && ta != nil {
			ta = types.Substitute(ta, c.typeSubst)
		}
		if i > 0 {
			mangledName += ", "
		}
		mangledName += typeArgStr(ta)
	}
	mangledName += "]"

	fn, ok := c.funcs[mangledName]
	if !ok {
		panic(fmt.Sprintf("codegen: undefined monomorphic enum method %q", mangledName))
	}

	// Generate receiver using the enum convention (mirrors genEnumMethodCall).
	var args []value.Value
	var tempEnumPtr value.Value // non-nil when receiver needs post-call drop
	if method.Sig().Recv() != nil {
		prevEnumTemps := len(c.enumCtorTemps)
		target := c.genExprAutoPropagate(member.Target) // B0323
		enumCtorTracked := len(c.enumCtorTemps) > prevEnumTemps
		if isThisReceiver(member.Target) {
			args = append(args, target)
		} else {
			alloca := c.entryBlock.NewAlloca(target.Type())
			alloca.SetName(c.uniqueLocalName("enum.this"))
			c.block.NewStore(target, alloca)
			ptr := c.block.NewBitCast(alloca, irtypes.I8Ptr)
			args = append(args, ptr)
			// T0660: a borrow-return receiver (`Tagged&`/`Tagged~`) aliases
			// the owner's payload (e.g. `ev.at(0)` shares ev.items[0]'s
			// string); dropping the synthesized receiver temp would
			// double-free what the owner still frees at scope exit.
			if c.freshEnumReceiverNeedsDrop(member.Target) && !enumCtorTracked {
				tempEnumPtr = ptr
			}
		}
	}

	// T0418/T0636: owner-enum subst (Box[int].T → int) merged with the
	// method-level subst (transform[string].U → string).
	// T1223: compute the subst BEFORE evaluating args and route arg-gen through
	// genGenericCallArgs so genCallArgsWithMutRef sees CONCRETE param types (a raw
	// `T move` param would otherwise skip the field-read dup every move sink arms).
	var ownerSubst map[*types.TypeParam]types.Type
	if inst, ok := targetType.(*types.Instance); ok {
		if origin, ok := inst.Origin().(*types.Enum); ok && len(origin.TypeParams()) > 0 {
			ownerSubst = types.BuildSubstMap(origin.TypeParams(), inst.TypeArgs())
		}
	}
	methodSubst := c.buildInferredCallSubst(method.Sig().TypeParams(), typeArgs)
	combined := mergeSubstMaps(ownerSubst, methodSubst)
	argVals, argTypes, variadicPTs := c.genGenericCallArgs(e.Args, method.Sig(), combined)
	origArgVals := argVals // B0345
	argVals = c.coerceCallArgs(argVals, argTypes, method.Sig().Params(), e.Args, combined)
	args = append(args, argVals...)

	var result value.Value = c.block.NewCall(fn, args...)
	c.clearVariadicStaticFlags(variadicPTs)
	result = c.emitReturnAliasCheckSubst(result, method.Sig(), e.Args, origArgVals, combined, e) // B0345/T0418

	// Drop temp enum receiver if it was a fresh temporary (mirrors genEnumMethodCall).
	if tempEnumPtr != nil && c.enumInstanceHasDrop(targetType, enum) {
		dropName := mangleMethodName(enumName, "drop", false)
		if dropFn, ok := c.funcs[dropName]; ok {
			c.block.NewCall(dropFn, tempEnumPtr)
		} else if c.moduleInfos != nil {
			if dropFn := c.forwardDeclareModuleEnumDrop(enum, enumName, dropName); dropFn != nil {
				c.block.NewCall(dropFn, tempEnumPtr)
			}
		}
	}

	return result
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

	// For a generic parent (`Child[T] is Base[T]`), resolve the inheritance
	// type args under the current substitution so we can both name the
	// monomorphized parent constructor and coerce the args (T0474).
	parentRef := named.Parents()[0]
	var resolvedParentArgs []types.Type
	if len(parentRef.TypeArgs) > 0 && len(parent.TypeParams()) > 0 {
		resolvedParentArgs = make([]types.Type, len(parentRef.TypeArgs))
		for i, ta := range parentRef.TypeArgs {
			if c.typeSubst != nil {
				ta = types.Substitute(ta, c.typeSubst)
			}
			resolvedParentArgs[i] = ta
		}
	}

	// Load the this pointer
	thisAlloca := c.locals["this"]
	thisPtr := c.block.NewLoad(irtypes.I8Ptr, thisAlloca)

	if parent.HasNew() {
		// Parent has explicit new() — call ParentType.new(this, args...)
		parentName := parent.Obj().Name()
		if resolvedParentArgs != nil {
			parentName = monoName(types.NewInstance(parent, resolvedParentArgs))
		}
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
			// T0754: clear cast subject's drop flag — ownership moves it at
			// the owning-slot store (super()'s parent constructor takes the
			// arg into the parent's field), so the subject must not also drop.
			// T0849: for the conditional `as` form, drop iff the downcast failed.
			if ident := c.castSubjectMovableIdent(arg.Value); ident != nil {
				c.consumeCastSubjectDropFlag(arg.Value, ident.Name)
			}
		}
		newMethod := parent.LookupMethod("new")
		if newMethod != nil {
			// T0418/T0474: Build subst for parent's TypeParams from the resolved
			// inheritance type args (e.g., `type Foo[T] is Bar[T]` → Bar.T → resolved T).
			var superSubst map[*types.TypeParam]types.Type
			if resolvedParentArgs != nil {
				superSubst = types.BuildSubstMap(parent.TypeParams(), resolvedParentArgs)
			}
			argVals = c.coerceCallArgs(argVals, argTypes, newMethod.Sig().Params(), e.Args, superSubst)
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
			// T0754: clear cast subject's drop flag — ownership moves it at
			// the owning-slot store (parent implicit-ctor field-init), so
			// the subject must not also drop at scope exit.
			// T0849: for the conditional `as` form, drop iff the downcast failed.
			if ident := c.castSubjectMovableIdent(arg.Value); ident != nil {
				c.consumeCastSubjectDropFlag(arg.Value, ident.Name)
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

// genMutRefArg returns a pointer to the caller's storage for a MutRef argument (B0149).
// This is used at call sites to pass the address of a variable (or forward a
// MutRef param pointer) instead of loading and passing the value.
func (c *Compiler) genMutRefArg(expr ast.Expr) value.Value {
	switch e := expr.(type) {
	case *ast.IdentExpr:
		// If the variable is itself a MutRef param, forward its pointer
		if ptr, ok := c.mutRefPtrs[e.Name]; ok {
			return ptr
		}
		// Otherwise, pass the alloca address (pointer to local variable)
		if alloca, ok := c.locals[e.Name]; ok {
			return alloca
		}
		// T1272: a bare module-level getter ident (`emit(a_builder, …)`) is not an
		// lvalue — it constructs a fresh temporary. Fall through to the default
		// materialize-and-track path so the temp is dropped at statement end.
	case *ast.MemberExpr:
		// T1272: a getter member (`mod.prop`, `owner.getter`) is not a field — it
		// produces a fresh temporary. genFieldPtr panics on a getter, so materialize
		// instead. genCallArgExpr registers the getter's heap result as a statement
		// temp (with the existing alias filters), so the temp is dropped at
		// statement end. Only a genuine field flows to genFieldPtr.
		if !c.isMemberGetter(e) {
			return c.genFieldPtr(e)
		}
	}
	// Fallback (and non-lvalue getter idents/members): evaluate normally and store
	// to a temp alloca. genCallArgExpr registers any heap-owning result as a
	// statement temp, so the materialized value is dropped at statement end (T1272).
	val := c.genCallArgExpr(expr)
	tmp := c.createEntryAlloca(val.Type())
	c.block.NewStore(val, tmp)
	return tmp
}

// maybeEnableDupForMutRefArg sets dupStringFieldAccess or dupContainerFieldAccess
// when an arg about to be evaluated is a field read on a droppable owner that's
// being passed to a `~` (consuming) param. Without this, the field's inner
// buffer is shared between the owner and the callee — both end up freeing it.
// T0366.
//
// T0403: Also sets dupHeapUserFieldAccess when the arg is a direct IndexExpr
// against a Vector[heap-user-type]. Without this, `f(v[0])` aliases v's
// element instance pointer — the callee's `~T` drop and v's element walk
// would double-free. Direct IndexExpr only (matching the var-decl-site
// policy in genTyped/InferredVarDecl) avoids orphan-clone leaks for chains
// like `f(v[0].method())`.
func (c *Compiler) maybeEnableDupForMutRefArg(arg ast.Expr, paramType types.Type) {
	pt := paramType
	if c.typeSubst != nil {
		pt = types.Substitute(pt, c.typeSubst)
	}
	if isRefType(pt) {
		return
	}
	// T1170: a match-borrowed Optional/Array-of-heap binding (`consume(move maybe)`)
	// passed to a ~ (move) param escapes into the callee. Clone so the subject's
	// synth enum drop doesn't free the value the callee now owns (mirrors the
	// store/return escape paths). genIdentExpr performs the actual dup; the move-arg
	// site claims the produced optionalStringDup/optionalContainerDup into the callee.
	if ident, ok := arg.(*ast.IdentExpr); ok && c.matchBorrowedIdents != nil &&
		c.matchBorrowedIdents[ident.Name] && isVariantPayloadBorrowShape(pt) {
		c.setDupFlagsForFieldAccess(pt)
		return
	}
	// T1146: `consume(m[k]!)` — inline unwrap of a container index passed to a
	// move (~) param. Dup so the callee's consume-drop and the map's slot drop
	// don't free the same instance. Mirrors the var-binding form (stmt.go:1133).
	if isUnwrappedContainerIndex(arg) {
		c.dupHeapUserFieldAccess = true
		return
	}
	// T0403/T1175/T1215: IndexExpr against a Vector passed to a `~` param.
	// `f(v[i])` returns a value that aliases the vector's element buffer — the
	// callee's consume-drop and v's element drop then free the same allocation.
	// Arm the matching dup-on-read (heap-user, Optional[heap-user], droppable-enum,
	// string, or container element) so genVectorIndex/genArrayIndex produce an
	// owned copy. T1215 added the string/container element arms (previously only
	// heap-user/enum were handled, so a `string[]`/`Vector[...][]` element double-
	// freed). Peels parens + non-consuming casts (mirrors the constructor path).
	if c.armDupForVectorIndexArg(arg) {
		return
	}
	mem, ok := arg.(*ast.MemberExpr)
	if !ok {
		return
	}
	// T1011: a narrowed enum variant field arg passed to a `~` param needs the
	// same dup-on-escape as a struct field — gate on the subject enum being
	// droppable (the enum owner is an *types.Enum, which ownerHasOrSynthDrop below
	// does not recognize).
	if matched, droppable := c.narrowedVariantFieldDroppable(mem); matched {
		if droppable {
			c.setDupFlagsForFieldAccess(pt)
		}
		return
	}
	ownerType := c.info.Types[mem.Target]
	if c.typeSubst != nil && ownerType != nil {
		ownerType = types.Substitute(ownerType, c.typeSubst)
	}
	ownerNamed := extractNamed(ownerType)
	// T0513: also accept mono-synthesized drop on generic instances.
	if !c.ownerHasOrSynthDrop(ownerType, ownerNamed) {
		return
	}
	// T0487: covers string, Optional[string], Vector|Channel|Arc|Weak, and
	// Optional[Vector|Channel|Arc|Weak] in one place.
	c.setDupFlagsForFieldAccess(pt)
}

// armDupForVectorIndexArg arms the appropriate dup-on-read flag when `arg`
// (peeling parens + non-consuming casts) is a Vector index `v[i]` whose element
// is a heap type that would otherwise ALIAS the vector's element buffer when the
// value escapes into an owning sink — a constructor field-init, a `~` (move)
// param, or an enum variant payload. Without the dup both the owning sink's drop
// and the vector's element drop free the same allocation (double-free at scope
// exit → "fatal: invalid free (bad header magic)" on macOS, silent over-free
// elsewhere). genVectorIndex consumes the matching flag (B0204 string / T0383
// container / T0398 heap-user / T1129 enum) to produce an independent copy.
// Returns true if a flag was armed. Before T1215 only the heap-user/enum element
// shapes were handled here (T0847), so a `string[]`/`Vector[...][]` element read
// into an owned field, move param, or enum payload aliased the source and
// double-freed. T1215.
func (c *Compiler) armDupForVectorIndexArg(arg ast.Expr) bool {
	probe := arg
	for {
		if p, ok := probe.(*ast.ParenExpr); ok {
			probe = p.Expr
			continue
		}
		if cast, ok := probe.(*ast.CastExpr); ok {
			probe = cast.Expr
			continue
		}
		break
	}
	idx, ok := probe.(*ast.IndexExpr)
	if !ok {
		return false
	}
	targetType := c.info.Types[idx.Target]
	if c.typeSubst != nil && targetType != nil {
		targetType = types.Substitute(targetType, c.typeSubst)
	}
	if _, isVec := types.AsVector(targetType); !isVec {
		return false
	}
	argType := c.info.Types[idx]
	if c.typeSubst != nil && argType != nil {
		argType = types.Substitute(argType, c.typeSubst)
	}
	// Heap-user / Optional[heap-user] / droppable-enum element → deep clone.
	_, optHeap := c.optionalHeapDupElem(argType)
	if isDroppableHeapUserType(argType) || optHeap || c.enumElemNeedsDupOnRead(argType) {
		c.dupHeapUserFieldAccess = true
		return true
	}
	// T1287: structural-interface element → deep clone the {vtable, instance} box.
	// Without this, `a[i] = b[j]` (or a constructor/`~`-param escape) stores b[j]'s
	// box aliased into the sink; a's and b's element drop loops (T1284) then free the
	// same box → double-free. genVectorIndex consumes the flag via cloneStructuralView.
	if isNonValueStructuralType(argType) {
		c.dupHeapUserFieldAccess = true
		return true
	}
	// string / Optional[string] / Vector|Channel|Arc|Weak element → string/container dup.
	savedStr, savedCont := c.dupStringFieldAccess, c.dupContainerFieldAccess
	c.setDupFlagsForFieldAccess(argType)
	return c.dupStringFieldAccess != savedStr || c.dupContainerFieldAccess != savedCont
}

// maybeEnableDupForConstructorArg sets dupStringFieldAccess or
// dupContainerFieldAccess when a constructor field-init arg is a field read
// on a droppable owner. Without this, the field's inner buffer is shared
// between the owner and the new instance — both end up freeing it. Mirrors
// maybeEnableDupForMutRefArg (T0366) for the constructor field-init path.
// T0411.
func (c *Compiler) maybeEnableDupForConstructorArg(arg ast.Expr, fieldType types.Type) {
	// T1170: a match-borrowed Optional/Array-of-heap binding (`Wrapper(held: maybe)`)
	// initializing an owned constructor field escapes into the new instance. Clone so
	// the subject's synth enum drop doesn't free the value the field now owns (mirrors
	// the move-param / store / return escape paths).
	if ident, ok := arg.(*ast.IdentExpr); ok && c.matchBorrowedIdents != nil && c.matchBorrowedIdents[ident.Name] {
		ft := fieldType
		if c.typeSubst != nil {
			ft = types.Substitute(ft, c.typeSubst)
		}
		if isVariantPayloadBorrowShape(ft) {
			c.setDupFlagsForFieldAccess(ft)
		}
		return
	}
	// T1146: `Holder(res: m[k]!)` — inline unwrap of a container index used to
	// initialize an owned field. Same double-free as the move-param case.
	if isUnwrappedContainerIndex(arg) {
		c.dupHeapUserFieldAccess = true
		return
	}
	// T0847/T1175/T1215: peel parens + non-consuming casts to find a container-
	// element IndexExpr subject, then dup-on-read for `Holder(held: v[0])` /
	// `Holder(s: strings[0])` / `Holder(held: v[0] as! C)`. Covers heap-user,
	// Optional[heap-user], droppable-enum, string, and container element shapes
	// (T1215 added the string/container arms, which previously aliased the
	// vector's element buffer → double-free). Mirrors maybeEnableDupForMutRefArg.
	if c.armDupForVectorIndexArg(arg) {
		return
	}
	mem, ok := arg.(*ast.MemberExpr)
	if !ok {
		return
	}
	ft := fieldType
	if c.typeSubst != nil {
		ft = types.Substitute(ft, c.typeSubst)
	}
	// T1011: a narrowed enum variant field arg initializing a constructor field
	// needs the same dup-on-escape as a struct field — gate on the subject enum
	// being droppable (the enum owner is an *types.Enum, which ownerHasOrSynthDrop
	// below does not recognize).
	if matched, droppable := c.narrowedVariantFieldDroppable(mem); matched {
		if droppable {
			c.setDupFlagsForFieldAccess(ft)
		}
		return
	}
	ownerType := c.info.Types[mem.Target]
	if c.typeSubst != nil && ownerType != nil {
		ownerType = types.Substitute(ownerType, c.typeSubst)
	}
	ownerNamed := extractNamed(ownerType)
	// T0513: also accept mono-synthesized drop on generic instances.
	if !c.ownerHasOrSynthDrop(ownerType, ownerNamed) {
		return
	}
	// T0487: covers string, Optional[string], Vector|Channel|Arc|Weak, and
	// Optional[Vector|Channel|Arc|Weak] in one place.
	c.setDupFlagsForFieldAccess(ft)
}

// genCallArgsWithMutRef evaluates call arguments with MutRef-awareness (B0149).
// For MutRef params, passes the address of the caller's storage instead of the value.
// When the arg needs no coercion and is a simple lvalue, passes the alloca directly.
// Otherwise, evaluates the value, stores in a temp alloca, and passes the temp.
func (c *Compiler) genCallArgsWithMutRef(args []*ast.Arg, params []*types.Param, calleeResult types.Type) ([]value.Value, []types.Type, []variadicPassthrough) {
	var argVals []value.Value
	var argTypes []types.Type
	var variadicPTs []variadicPassthrough // B0203: passthrough vectors needing len restored after call
	// T1233: a generator call returns a stream[T]; its by-value params are copied
	// into the coroutine frame and read LAZILY during iteration, which outlives
	// this call statement. A caller-side statement temp (dropped at statement end)
	// would free a tuple arg's heap fields before the generator reads them (UAF).
	// For such args we instead register a SCOPE-level owned drop (see below).
	_, calleeIsGenerator := types.AsStream(calleeResult)
	for i, arg := range args {
		if i < len(params) {
			if _, isMutRef := params[i].Type().(*types.MutRef); isMutRef {
				argType := c.exprType(arg.Value)
				paramInner := params[i].Type().(*types.MutRef).Elem()
				// Check if the arg type matches the param inner type exactly
				// (no view coercion needed). If so, pass the alloca directly.
				if types.Identical(argType, paramInner) || types.Identical(argType, params[i].Type()) {
					argVals = append(argVals, c.genMutRefArg(arg.Value))
				} else {
					// Coercion needed (e.g., Builder → Writer view).
					// Evaluate normally, coerce, store in temp alloca, pass temp.
					val := c.genCallArgExpr(arg.Value)
					val = c.coerceToView(val, argType, params[i].Type())
					innerType := c.resolveType(params[i].Type())
					tmp := c.createEntryAlloca(innerType)
					c.block.NewStore(val, tmp)
					argVals = append(argVals, tmp)
				}
				argTypes = append(argTypes, c.exprType(arg.Value))
				continue
			}
		}
		// T0366: For `~` (move) params, when the arg is a field read on a droppable
		// owner, set the dup flag so genFieldAccess produces an independent copy.
		// Without this, the inner buffer is shared between the caller's owner field
		// and the callee — the callee frees it, then the owner's drop frees it again.
		// Only meaningful for auto-dup container types (string, Vector, Channel, etc.).
		isMutRefParam := i < len(params) && params[i].Ref() == types.RefMut
		if isMutRefParam {
			c.maybeEnableDupForMutRefArg(arg.Value, params[i].Type())
		}
		// T1108: snapshot enum-ctor temps before evaluating the arg so a move
		// param can claim (untrack) any inline enum-constructor temp produced
		// during this arg's evaluation — the callee consumes & drops it, so the
		// caller's statement-end drop must not also fire (double-free). Borrow
		// params leave the temp tracked → caller drops it at statement end.
		savedEnumTemps := len(c.enumCtorTemps)
		// T1467: same purpose for the heap-instance and closure-env registries —
		// promoteGeneratorArgToScope needs to know whether THIS argument's
		// evaluation materialized a temp before it re-homes one.
		savedHeapTemps, savedEnvTemps := len(c.heapTemps), len(c.envTemps)
		v := c.genCallArgExpr(arg.Value)
		c.dupStringFieldAccess = false
		c.dupContainerFieldAccess = false
		c.dupHeapUserFieldAccess = false // T0403
		// T1174: `consume(move maybe)` where maybe is a match-borrowed
		// Optional[heap-user] binding passed to a `~` (move) param aliases the
		// subject's variant payload; deep-clone the inner so the callee owns an
		// independent copy and the subject's synth enum drop still frees the original
		// exactly once. Borrow (`&`) params leave the alias intact (no escape).
		if isMutRefParam {
			v, _ = c.dupBorrowedHeapUserPayload(arg.Value, v)
		}
		argVals = append(argVals, v)
		argTypes = append(argTypes, c.exprType(arg.Value))
		// T0087: For ~ (move) params, transfer ownership to callee.
		// Clear caller's drop flag and claim string/heap temps so they're not double-freed.
		if isMutRefParam {
			if ident, ok := arg.Value.(*ast.IdentExpr); ok {
				c.clearDropFlag(ident.Name)
			}
			// T0754: a cast subject is consumed by ownership at `~`-param sites
			// — clear the subject's drop flag so it doesn't double-free with the
			// callee's consume drop. Mirrors the IdentExpr branch above.
			// T0849: for the conditional `as` form, drop iff the downcast failed.
			if ident := c.castSubjectMovableIdent(arg.Value); ident != nil {
				c.consumeCastSubjectDropFlag(arg.Value, ident.Name)
			}
			// T1224: `consume(move r!)` where r is an Optional single-owner handle —
			// the force-unwrap moves the inner out and the callee's move param drops
			// it, so the source optional's present flag must be cleared or its
			// scope-exit drop double-frees the same handle → segfault. Mirrors the
			// neutralizeForceUnwrapSource call in every constructor arg path (B0301).
			// Self-gating: no-ops unless arg.Value is a force-unwrap / force-cast /
			// optional error-handler, so plain-ident/temp/literal moves are unaffected.
			c.neutralizeForceUnwrapSource(arg.Value)
			c.claimStringTemp(v)
			c.claimHeapTemp(v) // B0201: prevent double-free for vector literals passed to ~ params
			c.claimEnvTemp(v)  // T1237: closure env ownership transfers to the callee's move param
			// T0522: When the arg is a field-access dup wrapped in an Optional
			// struct, claimStringTemp/claimHeapTemp can't match — `v` is the
			// outer struct, but the inner dup pointer is tracked separately via
			// optionalStringDup/optionalContainerDup. Claim the inner dup so
			// the callee owns it after the consume call and the caller's stmt
			// cleanup doesn't double-free.
			if c.optionalStringDup != nil {
				c.claimStringTemp(c.optionalStringDup)
				c.optionalStringDup = nil
			}
			if c.optionalContainerDup != nil {
				c.claimStringTemp(c.optionalContainerDup)
				c.optionalContainerDup = nil
			}
			// T1108: claim (untrack) the inline enum-constructor temp evaluated
			// for this move-param arg — the callee now owns and drops it. Gate on
			// the arg's static type being an enum: only then is a tracked enum-ctor
			// temp the value actually being moved into the callee. When the arg is
			// a non-enum expression that merely BORROWS an enum-ctor temp in a
			// sub-call (e.g. `take(inspect(Enum.V(x)))` where `inspect(Enum)` is a
			// borrow param returning a non-enum), that inner temp is an
			// intermediate the callee never receives — it must stay tracked so the
			// caller drops it at statement end, else it leaks.
			argEnumType := c.info.Types[arg.Value]
			if c.typeSubst != nil {
				argEnumType = types.Substitute(argEnumType, c.typeSubst)
			}
			if extractEnum(argEnumType) != nil {
				for j := savedEnumTemps; j < len(c.enumCtorTemps); j++ {
					c.block.NewStore(constant.NewInt(irtypes.I1, 0), c.enumCtorTemps[j].dropFlag)
				}
				c.enumCtorTemps = c.enumCtorTemps[:savedEnumTemps]
			}
		}
		// T1233: A plain (borrow) tuple-by-value param no longer drops its arg
		// (the callee-side T0406 drop was removed — a plain tuple param borrows).
		// When the arg is a tuple TEMP (literal / call result — no owning caller
		// variable), the caller must drop it after the call returns, else its heap
		// fields (closure envs, strings, vectors) leak. An owned tuple variable
		// keeps its own bindingDropTuple and a borrowed source is owned elsewhere —
		// neither needs a caller temp (see tupleArgIsCallerOwnedTemp).
		if i < len(params) && !isMutRefParam && !params[i].IsVariadic() {
			paramType := params[i].Type()
			if c.typeSubst != nil {
				paramType = types.Substitute(paramType, c.typeSubst)
			}
			if tup, isTuple := paramType.(*types.Tuple); isTuple && c.tupleNeedsDrop(tup) && c.tupleArgIsCallerOwnedTemp(arg.Value) {
				if calleeIsGenerator {
					// T1233: the generator borrows its param but reads it lazily
					// (frame outlives this statement), so a statement-end drop is
					// too early → UAF. Give the temp a SCOPE-level owned drop (a
					// synthetic owned local) — the same lifetime an owned tuple
					// variable gets, which the borrow model already handles: the
					// tuple stays alive until the enclosing scope exits (after the
					// for-in loop consuming the stream), then drops exactly once.
					name := c.uniqueLocalName("_gentuparg")
					argAlloca := c.createEntryAlloca(v.Type())
					argAlloca.SetName(name)
					c.block.NewStore(v, argAlloca)
					c.maybeRegisterDrop(name, argAlloca, tup)
				} else {
					c.registerTupleStmtTemp(v, tup)
				}
			} else if calleeIsGenerator {
				// T1467: generalize T1233 beyond tuples — EVERY droppable by-value
				// arg temp is read lazily by the coroutine frame, so a statement-end
				// drop is too early (UAF / double-free / leak, depending on which
				// temp registry holds it). Re-home it to a scope-level owned local,
				// the same lifetime an owned variable argument already gets. Runs
				// after the move-param claim block above so a `~`-param arg (already
				// transferred to the callee) is never re-registered, and leaves
				// fixed arrays to the T1466 block below — the sole owner of that
				// shape, which also covers the array literal that has no temp yet.
				c.promoteGeneratorArgToScope(v, paramType, arg.Value, savedHeapTemps, savedEnvTemps, savedEnumTemps)
			}
		}
		// T1466: A fixed-size array literal (or owned-array call result) passed to a
		// plain (borrow) array-by-value param is copied as an [N x T] aggregate and
		// borrowed by the callee — nothing frees its heap-allocating elements
		// (strings/vectors/heap-user). Register it as an element-wise-drop statement
		// temp so the caller frees them after the call, mirroring the T1233 tuple case
		// above. move (RefMut) params are excluded: the callee owns and drops via
		// bindingDropArray, and an already-tracked call-result temp was claimed above.
		if i < len(params) && !isMutRefParam && !params[i].IsVariadic() {
			paramType := params[i].Type()
			if c.typeSubst != nil {
				paramType = types.Substitute(paramType, c.typeSubst)
			}
			if arr, isArr := paramType.(*types.Array); isArr &&
				c.variantFieldNeedsDrop(arr.Elem()) && c.tupleArgIsCallerOwnedTemp(arg.Value) {
				if calleeIsGenerator {
					// Generator borrows its param but reads it lazily (frame outlives
					// this statement) — a statement-end drop is too early (UAF). Give
					// the temp a scope-level owned drop, mirroring the tuple case.
					// A call-result array (`gen(mk())`) was already registered as a
					// statement-end temp by the T1181 CallExpr path; claim it here so
					// that too-early drop doesn't fire — the scope drop below owns it.
					// No-op for an array literal (never statement-tracked).
					c.claimStringTemp(v)
					name := c.uniqueLocalName("_genarrarg")
					argAlloca := c.createEntryAlloca(v.Type())
					argAlloca.SetName(name)
					c.block.NewStore(v, argAlloca)
					c.maybeRegisterDrop(name, argAlloca, arr)
				} else {
					c.trackArrayTemp(v, arr)
				}
			}
		}
		// B0203: Variadic passthrough — set static flag (bit 63) on the vector's
		// len field so the callee's scope-exit drop skips element drops and buffer free.
		// Passthrough is detected when the arg is NOT an ArrayLit (ArrayLit means
		// sema synthesized a fresh vector for inline variadic args).
		// Skip if the vector is already static (.rodata) — the memory is read-only
		// and bit 63 is already set, so the callee's drop will skip anyway.
		if i < len(params) && params[i].IsVariadic() {
			if _, isArrayLit := arg.Value.(*ast.ArrayLit); isArrayLit {
				// B0201: Claim the heap temp for freshly synthesized variadic vectors.
				// The callee takes ownership and drops the vector at scope exit.
				c.claimHeapTemp(v)
			} else {
				headerType := vectorHeaderType()
				headerPtr := c.block.NewBitCast(v, irtypes.NewPointer(headerType))
				lenPtr := c.block.NewGetElementPtr(headerType, headerPtr,
					constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
				rawLen := c.block.NewLoad(irtypes.I64, lenPtr)
				// Check if already static (bit 63 set) — skip if so
				bit63 := c.block.NewAnd(rawLen, constant.NewInt(irtypes.I64, math.MinInt64))
				isStatic := c.block.NewICmp(enum.IPredNE, bit63, constant.NewInt(irtypes.I64, 0))
				setBlock := c.newBlock("variadic.setflag")
				skipBlock := c.newBlock("variadic.skipflag")
				c.block.NewCondBr(isStatic, skipBlock, setBlock)
				// Set bit 63
				c.block = setBlock
				flaggedLen := c.block.NewOr(rawLen, constant.NewInt(irtypes.I64, math.MinInt64))
				c.block.NewStore(flaggedLen, lenPtr)
				c.block.NewBr(skipBlock)
				// Continue
				c.block = skipBlock
				variadicPTs = append(variadicPTs, variadicPassthrough{lenPtr: lenPtr, savedLen: rawLen})
			}
		}
	}
	return argVals, argTypes, variadicPTs
}

// variadicPassthrough tracks a vector whose static flag was temporarily set
// for variadic passthrough (B0203).
type variadicPassthrough struct {
	lenPtr   value.Value // pointer to the vector's len field
	savedLen value.Value // original len value before setting bit 63
}

// clearVariadicStaticFlags restores original len values on vectors that were
// temporarily marked static for variadic passthrough (B0203). Only restores
// vectors that were originally non-static (static vectors in .rodata are
// read-only and were never modified).
func (c *Compiler) clearVariadicStaticFlags(passthroughs []variadicPassthrough) {
	for _, pt := range passthroughs {
		// Check if the saved len had bit 63 set (originally static). If so,
		// the vector is .rodata and we never modified it — skip the store.
		bit63 := c.block.NewAnd(pt.savedLen, constant.NewInt(irtypes.I64, math.MinInt64))
		wasStatic := c.block.NewICmp(enum.IPredNE, bit63, constant.NewInt(irtypes.I64, 0))
		restoreBlock := c.newBlock("variadic.restore")
		doneBlock := c.newBlock("variadic.restored")
		c.block.NewCondBr(wasStatic, doneBlock, restoreBlock)
		c.block = restoreBlock
		c.block.NewStore(pt.savedLen, pt.lenPtr)
		c.block.NewBr(doneBlock)
		c.block = doneBlock
	}
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

// coerceIndirectCallArgs applies the same param-type coercion to closure-call
// arguments that regular calls get via coerceCallArgs — most importantly,
// optional wrapping (bare `T` → `{i1,T}`, and `none` → zeroinitialized `{i1,T}`).
// Without it, an optional argument would be passed as a bare scalar while
// genIndirectCall types the function pointer with the `{i1,T}` aggregate param,
// producing a type-mismatched call that LLVM's x86 backend tolerates but the
// WASM backend lowers to invalid code (T0849).
func (c *Compiler) coerceIndirectCallArgs(sig *types.Signature, args []*ast.Arg, argVals []value.Value) []value.Value {
	if sig == nil || len(argVals) == 0 {
		return argVals
	}
	argTypes := make([]types.Type, len(argVals))
	for i := range argVals {
		if i < len(args) {
			argTypes[i] = c.info.Types[args[i].Value]
		}
	}
	return c.coerceCallArgs(argVals, argTypes, sig.Params(), args, nil)
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
