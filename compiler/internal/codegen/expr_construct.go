package codegen

import (
	"fmt"
	"math"
	"math/big"

	"github.com/llir/llvm/ir"
	"github.com/llir/llvm/ir/constant"
	"github.com/llir/llvm/ir/enum"
	irtypes "github.com/llir/llvm/ir/types"
	"github.com/llir/llvm/ir/value"

	"github.com/promise-language/promise/compiler/internal/ast"
	"github.com/promise-language/promise/compiler/internal/types"
)

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

	// T0135: Track allocation as heap temp so auto-propagation error paths
	// free it if a failable constructor argument fails.
	c.trackHeapTemp(rawPtr, c.palFree)

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
		// B0199: Look up new() method BEFORE processing args so we can check
		// parameter move semantics. Only clear caller's drop flag for move (~)
		// parameters. Borrow parameters get a copy (strdup for strings), so the
		// caller must keep its drop flag to free the original.
		newMethod := named.LookupMethod("new")
		var newParams []*types.Param
		if newMethod != nil {
			newParams = newMethod.Sig().Params()
		}

		var argVals []value.Value
		var argTypes []types.Type
		for i, arg := range e.Args {
			// T0552: Save enum ctor temp count so we can clear those added during
			// this arg's evaluation once the value is passed to new() (which stores
			// it into a field). Without clearing, the temp's scope-exit drop runs
			// and double-frees the variant payload the owner's synth drop now also
			// handles. Symmetric to the implicit-constructor field loop below.
			savedEnumTemps := len(c.enumCtorTemps)
			v := c.genCallArgExpr(arg.Value)
			argVals = append(argVals, v)
			argTypes = append(argTypes, c.info.Types[arg.Value])
			// B0199: For string-typed borrow params on types with HasDrop(),
			// the constructor body strdups the string (genAssignment detects no
			// drop flag on the param → dupString). The caller must keep its drop
			// flag so the original string is freed. For move params and non-string
			// params, clear the drop flag as before (direct pointer store).
			skipClear := false
			isMoveParam := true
			if newMethod != nil && i < len(newParams) {
				paramType := newParams[i].Type()
				_, isMutRef := paramType.(*types.MutRef)
				isMoveParam = isMutRef || newParams[i].Ref() == types.RefMut
				if !isMoveParam && extractNamed(paramType) == types.TypString && (named.HasDrop() || named.NeedsSynthDrop()) {
					skipClear = true
				}
			}
			if !skipClear {
				if ident, ok := arg.Value.(*ast.IdentExpr); ok {
					c.clearDropFlag(ident.Name)
				}
				// T0754: clear cast subject's drop flag — ownership moves it
				// at the owning-slot store, so the subject's scope-exit drop
				// must not fire on the same allocation new() now owns.
				// T0849: for the conditional `as` form, drop iff the cast failed.
				if ident := c.castSubjectMovableIdent(arg.Value); ident != nil {
					c.consumeCastSubjectDropFlag(arg.Value, ident.Name)
				}
				// B0301: Neutralize source optional for opt! args in new() constructors.
				c.neutralizeForceUnwrapSource(arg.Value)
			}
			// B0211: Don't claim string temp when the constructor will strdup
			// (skipClear=true: borrow string on type with drop/synth-drop).
			// The strdup creates an independent copy; the original temp should be freed.
			if !skipClear {
				c.claimStringTemp(v) // B0168: ownership transferred to new() args
			}
			// B0233: Claim heap temp — ownership transferred to new() constructor field.
			c.claimHeapTemp(v)
			c.claimEnvTemp(v) // T0100: claim env temp for closure args
			// T0552: Clear enum ctor temps created during arg evaluation when the
			// param consumes the enum by move — ownership transfers to new() and
			// then to whatever field it stores into. Borrow params don't take
			// ownership, so leave the temps in place for the caller's cleanup.
			// T1139: gate the clear on the arg's static type being an enum — only
			// then is a tracked enum-ctor temp the value actually being moved. A
			// non-enum arg that merely BORROWS an inline Enum.V(x) temp in a
			// sub-call leaves an intermediate the callee never receives; it must
			// stay tracked so the caller drops it at statement end, else it leaks.
			// Residual: an enum-typed arg produced by a call that internally
			// borrows a *different* enum-ctor temp still over-claims that inner
			// temp (the whole range is cleared rather than just the moved value's
			// backing temp) — lower priority, contrived nesting.
			if isMoveParam {
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
		}
		// B0233: Do NOT claim heap temp here. Let downstream consumers claim:
		// - Variable assignment (stmt.go genAssignment)
		// - ~ params (call site)
		// - Container store (push, send, field/index assign)
		// If nobody claims, cleanupHeapTemps frees the temp at statement end.

		if newMethod != nil {
			// T0418: Resolve T-typed params against the owner type's
			// concrete args (e.g., Box[int] → T → int) so generic
			// constructors with T? params don't double-wrap.
			ownerSubst := c.buildOwnerTypeArgSubst(typ)
			argVals = c.coerceCallArgs(argVals, argTypes, newMethod.Sig().Params(), e.Args, ownerSubst)
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
			// T0345: Swap heap-temp dropFunc from palFree to the type's full drop
			// so unclaimed/discarded instances free their transitive heap fields.
			c.updateConstructorTempDrop(rawPtr, named, typ)
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
		// B0210: Build a substitution map from the Instance type args so field types
		// are properly substituted even when c.typeSubst is nil (user code calling
		// a generic constructor directly, not inside a monomorphic method body).
		var localSubst map[*types.TypeParam]types.Type
		if inst, ok := typ.(*types.Instance); ok {
			if origin, ok := inst.Origin().(*types.Named); ok && len(origin.TypeParams()) > 0 {
				localSubst = types.BuildSubstMap(origin.TypeParams(), inst.TypeArgs())
			}
		}
		fieldTypeMap := make(map[string]types.Type)
		for _, f := range named.AllFields() {
			ft := f.Type()
			if c.typeSubst != nil {
				ft = types.Substitute(ft, c.typeSubst)
			}
			if localSubst != nil {
				ft = types.Substitute(ft, localSubst)
			}
			fieldTypeMap[f.Name()] = ft
		}

		// maybeWrapOptional wraps val in an optional struct when the field type
		// is T? but the expression produces a non-optional, non-none value.
		// Uses Identical (not "is exprOpt?") so a T? expr targeting a T?? field
		// still gets wrapped to match the slot's depth.
		maybeWrapOptional := func(val value.Value, expr ast.Expr, fieldName string, fieldIdx int) value.Value {
			fieldType := fieldTypeMap[fieldName]
			if _, isOpt := fieldType.(*types.Optional); !isOpt {
				return val
			}
			exprType := c.info.Types[expr]
			if c.typeSubst != nil {
				exprType = types.Substitute(exprType, c.typeSubst)
			}
			if exprType == types.TypNone {
				return val
			}
			if types.Identical(exprType, fieldType) {
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
			// T0552: Save enum ctor temp count so we can clear those added during
			// this arg's evaluation once the value is stored in the field. The
			// field becomes the unique owner of the enum data; without clearing,
			// the temp's scope-exit drop runs and double-frees the variant payload
			// the owner's synth drop now also handles.
			savedEnumTemps := len(c.enumCtorTemps)
			fieldIdx, ok := layout.InstanceFieldIndex[arg.Name]
			if !ok {
				panic(fmt.Sprintf("codegen: unknown field %s on type %s", arg.Name, typ))
			}
			// B0210: For optional fields, generate none values directly from the layout
			// type instead of resolveType(targetType), which may produce a wrong LLVM
			// type when TypeParams aren't fully substituted in all code paths.
			var val value.Value
			if _, isOpt := fieldTypeMap[arg.Name].(*types.Optional); isOpt {
				if _, isNone := arg.Value.(*ast.NoneLit); isNone {
					// Generate zero value directly from the layout's LLVM type
					val = c.zeroValue(layout.Instance.Fields[fieldIdx].LLVMType)
				} else {
					if ft, ok := fieldTypeMap[arg.Name].(*types.Optional); ok {
						c.targetType = ft
					}
					// T0411: Auto-dup string/container fields read from a droppable
					// owner so the new instance gets an independent copy.
					c.maybeEnableDupForConstructorArg(arg.Value, fieldTypeMap[arg.Name])
					val = c.genCallArgExpr(arg.Value)
					c.dupStringFieldAccess = false
					c.dupContainerFieldAccess = false
					c.dupHeapUserFieldAccess = false // T0847
					c.targetType = nil
					// T1174: `Wrapper(held: maybe)` where maybe is a match-borrowed
					// Optional[heap-user] binding aliases the subject's variant payload;
					// deep-clone the inner so the new instance owns an independent copy.
					val, _ = c.dupBorrowedHeapUserPayload(arg.Value, val)
				}
			} else {
				// T0411: Auto-dup string/container fields read from a droppable
				// owner so the new instance gets an independent copy.
				c.maybeEnableDupForConstructorArg(arg.Value, fieldTypeMap[arg.Name])
				val = c.genCallArgExpr(arg.Value)
				c.dupStringFieldAccess = false
				c.dupContainerFieldAccess = false
				c.dupHeapUserFieldAccess = false // T0847
				// T1174: match-borrowed Optional[heap-user] binding used to
				// initialize an owned field — deep-clone (see the optional-field
				// branch above).
				val, _ = c.dupBorrowedHeapUserPayload(arg.Value, val)
			}
			// T1279: A pure value type / primitive / string used to initialize a
			// structural-interface field must be view-boxed. The box is an owned heap
			// temp; the claimHeapTemp(val) in the store branch below transfers ownership
			// to the constructed instance, whose synthesized drop frees the box via
			// __promise_structural_drop. Placed before maybeWrapOptional so an optional
			// structural field (`Sink? s`) boxes-then-wraps (T1298). No-op when no
			// boxing is needed.
			ctorArgType := c.info.Types[arg.Value]
			if c.typeSubst != nil && ctorArgType != nil {
				ctorArgType = types.Substitute(ctorArgType, c.typeSubst)
			}
			ctorFieldType := fieldTypeMap[arg.Name]
			if c.typeSubst != nil {
				ctorFieldType = types.Substitute(ctorFieldType, c.typeSubst)
			}
			// T1316: clone-vs-move for a string→structural-interface field box (sibling
			// of T1282 at the return / move-param sites). When the field is a structural
			// interface (bare or Optional) and the source is an owned string — a `move`d
			// named var whose drop flag is cleared at the owning-slot store below, or an
			// owned frame temp — the box must TAKE the source pointer (no dupString clone),
			// else the released original heap string is orphaned (leak). Borrowed sources
			// (borrow param, literal) match neither and stay on the clone path. MUST be
			// computed before the coerce calls (which reassign val) and the store-branch
			// flag clear (both mutate the state read here).
			ctorBoxSrcOwned := false
			if extractNamed(ctorArgType) == types.TypString {
				structTarget := ctorFieldType
				if opt, isOpt := ctorFieldType.(*types.Optional); isOpt {
					structTarget = opt.Elem() // peel so `Showable?` decides like `Showable`
				}
				if sn := extractNamed(structTarget); sn != nil && sn.IsStructural() {
					if ident, ok := arg.Value.(*ast.IdentExpr); ok {
						ctorBoxSrcOwned = c.hasDropFlag(ident.Name) // owned var / move-param
					} else if _, tracked := c.stmtTempMap[val]; tracked {
						ctorBoxSrcOwned = true // owned frame temp — claimed below
					}
				}
			}
			srcStr := val // pre-box string pointer, for the owned-temp claim below
			// Bare structural-interface field: box directly (no-op for an Optional
			// field, since extractNamed doesn't peel Optional).
			c.boxSrcOwned = ctorBoxSrcOwned
			val = c.coerceToView(val, ctorArgType, ctorFieldType)
			// T1298: Optional structural-interface field (`Sink? s`): box into the
			// element view before maybeWrapOptional wraps it (no-op otherwise).
			val = c.coerceToOptionalElem(val, ctorArgType, ctorFieldType)
			c.boxSrcOwned = false
			// T1316: an owned frame temp handed to the box must be claimed so
			// statement-end cleanup doesn't double-free the pointer the box (→ the
			// instance's synth drop) now owns. Named idents instead have their drop
			// flag cleared at the owning-slot store below, so this is a no-op for them
			// (srcStr isn't in stmtTempMap).
			if ctorBoxSrcOwned {
				c.claimStringTemp(srcStr)
			}
			// T0101: Save pre-wrap value for string temp claiming on optional fields
			preWrapVal := val
			val = maybeWrapOptional(val, arg.Value, arg.Name, fieldIdx)
			fieldPtr := c.block.NewGetElementPtr(instanceStructType, typedPtr,
				constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(fieldIdx)))

			// T0095: String fields in types with synthesized drops require ownership.
			// If the source is a variable without a drop flag (e.g., a function
			// parameter without ~), dup the string so the type owns an independent
			// copy. This prevents double-free when both the caller's variable and
			// the type's synthesized drop try to free the same allocation.
			fType := fieldTypeMap[arg.Name]
			if c.typeSubst != nil {
				fType = types.Substitute(fType, c.typeSubst)
			}
			// T0101: Also handle string? fields — the inner string temp must be claimed.
			isOptionalString := false
			isStringField := extractNamed(fType) == types.TypString
			if !isStringField {
				if opt, ok := fType.(*types.Optional); ok && extractNamed(opt.Elem()) == types.TypString {
					isStringField = true
					isOptionalString = true
				}
			}
			if isStringField {
				// T0754: clear cast subject's drop flag — for string field
				// init via `name as! S` shapes, ownership moves the subject at
				// this owning-slot store. The branches below handle bare ident
				// RHS; cover the cast-RHS shape here.
				if castIdent := c.castSubjectMovableIdent(arg.Value); castIdent != nil {
					// T0849: for the conditional `as` form, drop iff the cast failed.
					c.consumeCastSubjectDropFlag(arg.Value, castIdent.Name)
				}
				if ident, ok := arg.Value.(*ast.IdentExpr); ok {
					if _, hasFlag := c.dropFlags[ident.Name]; hasFlag {
						// Has drop flag: move ownership (existing behavior)
						c.block.NewStore(val, fieldPtr)
						c.clearDropFlag(ident.Name)
					} else if !isOptionalString {
						// No drop flag (function param without ~): dup for exclusive ownership
						// Skip dup for optional strings — the inner value is already owned
						c.block.NewStore(c.dupString(val), fieldPtr)
					} else {
						c.block.NewStore(val, fieldPtr)
					}
				} else {
					// Expression result: claim temp, store directly
					c.block.NewStore(val, fieldPtr)
					// B0301: Neutralize source optional for opt! on string fields.
					c.neutralizeForceUnwrapSource(arg.Value)
					// For optional strings, claim the pre-wrap value (the raw i8* temp)
					if isOptionalString {
						c.claimStringTemp(preWrapVal)
					} else {
						c.claimStringTemp(val)
					}
				}
			} else {
				c.block.NewStore(val, fieldPtr)
				// Clear drop flag: field value is moved into the constructor
				if ident, ok := arg.Value.(*ast.IdentExpr); ok {
					c.clearDropFlag(ident.Name)
				}
				// T0754: clear cast subject's drop flag — ownership moves it
				// at the owning-slot store, so the subject's scope-exit drop
				// must not fire on the same allocation the field now owns.
				// T0849: for the conditional `as` form, drop iff the cast failed.
				if ident := c.castSubjectMovableIdent(arg.Value); ident != nil {
					c.consumeCastSubjectDropFlag(arg.Value, ident.Name)
				}
				// B0301: When arg is opt! (force-unwrap), neutralize the source optional's
				// present flag so its scope cleanup won't double-free the inner value
				// that now lives in the constructor field.
				c.neutralizeForceUnwrapSource(arg.Value)
				// T0353: For optional fields wrapping stmtTemp-tracked heap values
				// (Vector, Channel), the wrapped {i1, i8*} won't match the bare i8*
				// in stmtTempMap. Claim preWrapVal too.
				c.claimStringTemp(preWrapVal)
				// B0168: Claim string temp — ownership transferred to constructor field.
				c.claimStringTemp(val)
				// B0233: Claim heap temp — ownership transferred to constructor field.
				c.claimHeapTemp(val)
				// T0100: Claim env temp — closure env is now owned by the struct field.
				c.claimEnvTemp(val)
				// T0741: For an optional-closure field `(() -> T)? cb`, val is the
				// wrapped {i1, {fn,env}} optional, which claimEnvTemp can't match —
				// claim the bare pre-wrap closure {fn,env} so its env is owned by
				// the field (otherwise cleanupEnvTemps frees it early → dangling).
				c.claimEnvTemp(preWrapVal)
			}
			// T0498: Claim per-field optionalStringDup / optionalContainerDup for
			// Optional[X] field-reads from droppable owners. genFieldAccess sets
			// these to the bare inner-dup pointer; the wrapped {i1, ptr} struct
			// passed to claimStringTemp above won't match the stmtTempMap entry.
			// Without claiming per-arg, the next arg's genFieldAccess overwrites
			// these fields and earlier dups stay live → use-after-free at
			// cleanupStmtTemps after the constructor.
			if c.optionalStringDup != nil {
				c.claimStringTemp(c.optionalStringDup)
				c.optionalStringDup = nil
			}
			if c.optionalContainerDup != nil {
				c.claimStringTemp(c.optionalContainerDup)
				c.optionalContainerDup = nil
			}
			// T0552: Clear enum ctor temps created during this arg's evaluation —
			// the field is now the unique owner of the enum's variant data, so the
			// ctor temp's scope-exit drop must not fire. Without this, the
			// owner's synth drop (which T0552 makes drop the enum field) and the
			// temp drop both target the same heap allocation → double-free.
			// T1139: gate on the arg's static type being an enum — a non-enum field
			// arg that merely BORROWS an inline Enum.V(x) temp in a sub-call leaves
			// an intermediate the field never owns; it must stay tracked so the
			// caller drops it at statement end, else it leaks. Residual: an
			// enum-typed field arg produced by a call that internally borrows a
			// *different* enum-ctor temp still over-claims it (whole range cleared)
			// — lower priority, contrived nesting.
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
				// T1279/T1298: view-box a concrete default value into a structural-
				// interface field (bare or Optional) before the optional wrap.
				defArgType := c.info.Types[defExpr]
				defFieldType := f.Type()
				if c.typeSubst != nil {
					if defArgType != nil {
						defArgType = types.Substitute(defArgType, c.typeSubst)
					}
					defFieldType = types.Substitute(defFieldType, c.typeSubst)
				}
				val = c.coerceToView(val, defArgType, defFieldType)
				val = c.coerceToOptionalElem(val, defArgType, defFieldType)
				preWrapVal := val // T0353: needed for optional-wrapped stmtTemp claim
				val = maybeWrapOptional(val, defExpr, f.Name(), fieldIdx)
				c.block.NewStore(val, fieldPtr)
				c.claimStringTemp(preWrapVal) // T0353: claim bare i8* hidden inside the wrap
				c.claimStringTemp(val)        // B0168: ownership transferred to field
				c.claimHeapTemp(val)          // B0233: ownership transferred to field
			} else {
				c.block.NewStore(c.zeroValue(layout.Instance.Fields[fieldIdx].LLVMType), fieldPtr)
			}
		}
	}

	// T0128: In structural default methods on _FnIter, store the receiver (upstream
	// _FnIter instance) as _parent for chained iterator cleanup. This enables
	// iterCleanup to recursively free the entire combinator chain.
	if c.selfSubst != nil && named != nil {
		if c.selfSubst.concrete.Obj().Name() == "_FnIter" && named.Obj().Name() == "_FnIter" {
			if parentIdx, ok := layout.InstanceFieldIndex["_parent"]; ok {
				if thisAlloca, ok := c.locals["this"]; ok {
					thisPtr := c.block.NewLoad(irtypes.I8Ptr, thisAlloca)
					thisInt := c.block.NewPtrToInt(thisPtr, irtypes.I64)
					parentFieldPtr := c.block.NewGetElementPtr(instanceStructType, typedPtr,
						constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(parentIdx)))
					c.block.NewStore(thisInt, parentFieldPtr)
				}
			}
		}
	}

	// B0233: Do NOT claim heap temp here. Let downstream consumers claim:
	// - Variable assignment (stmt.go genAssignment)
	// - ~ params (call site)
	// - Container store (push, send, field/index assign)
	// If nobody claims, cleanupHeapTemps frees the temp at statement end.

	// T0345: Swap heap-temp dropFunc from palFree to the type's full drop so
	// unclaimed/discarded instances free their transitive heap fields. Covers
	// both non-failable HasNew and implicit-constructor paths.
	c.updateConstructorTempDrop(rawPtr, named, typ)

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

// resolveDropFuncForTemp returns the cleanup function for a heap-allocated
// type's temporary instance (B0211). Returns nil for value types, copy types,
// and primitive scalars that don't heap-allocate.
func (c *Compiler) resolveDropFuncForTemp(named *types.Named, typ types.Type) *ir.Func {
	if named == nil || named.IsValueType() || named.IsCopy() || isPrimitiveScalar(named) {
		return nil
	}
	// Types with explicit drop method
	if named.HasDrop() {
		resolvedTyp := typ
		if c.typeSubst != nil {
			resolvedTyp = types.Substitute(typ, c.typeSubst)
		}
		explicitDrop := !named.NeedsSynthDrop()
		ownerName := named.Obj().Name()
		if inst, ok := resolvedTyp.(*types.Instance); ok {
			ownerName = monoName(inst)
		} else if explicitDrop {
			ownerName = c.resolveDropOwner(named)
		}
		mangledName := mangleMethodName(ownerName, "drop", false)
		if fn, ok := c.funcs[mangledName]; ok {
			// B0325: Explicit user drops don't include pal_free — wrap with $wrap
			// so the cleanup path frees the instance after calling drop.
			// Synthesized drops already include pal_free. T1344: native drops
			// self-free too, so they must not be wrapped.
			if explicitDrop && !dropIsNative(named) {
				return c.getOrCreateDropWrap(mangledName, fn)
			}
			return fn
		}
	}
	// Types with synthesized drop
	if named.NeedsSynthDrop() {
		resolvedTyp := typ
		if c.typeSubst != nil {
			resolvedTyp = types.Substitute(typ, c.typeSubst)
		}
		ownerName := named.Obj().Name()
		if inst, ok := resolvedTyp.(*types.Instance); ok {
			ownerName = monoName(inst)
		}
		mangledName := mangleMethodName(ownerName, "drop", false)
		if fn, ok := c.funcs[mangledName]; ok {
			return fn
		}
	}
	// Mono instance with codegen-detected synth drop (B0202)
	if inst, ok := typ.(*types.Instance); ok && monoInstNeedsSynthDrop(inst) {
		monoDropName := mangleMethodName(monoName(inst), "drop", false)
		if fn, ok := c.funcs[monoDropName]; ok {
			return fn
		}
	}
	// Heap type without drop: use pal_free
	return c.palFree
}

// updateConstructorTempDrop swaps the heap temp's dropFunc from palFree (the safe
// error-during-arg-eval default registered at T0135 by genConstructorCallMono) to
// the type's full drop after construction completes successfully (T0345). Without
// this swap, an unclaimed instance discarded or passed as a plain (non-`~`) arg
// would only get its top-level allocation freed at statement end, leaking any
// transitive heap fields (vector buffers, map storage, string allocations, etc.).
func (c *Compiler) updateConstructorTempDrop(rawPtr value.Value, named *types.Named, typ types.Type) {
	if rawPtr == nil || named == nil {
		return
	}
	idx, ok := c.heapTempMap[rawPtr]
	if !ok || idx < 0 {
		return
	}
	fullDrop := c.resolveDropFuncForTemp(named, typ)
	if fullDrop == nil || fullDrop == c.palFree {
		return
	}
	c.heapTemps[idx].dropFunc = fullDrop
}

// genValueTypeConstructor builds a value type by insertvalue chain — no heap allocation.
// Value struct layout: { i8* _vtable, field1, field2, ... }
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

	// If the type has an explicit new() constructor, alloca + store + call new() + load
	if named != nil && named.HasNew() {
		alloca := c.createEntryAlloca(valueStructType)
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
			// B0301: Neutralize source optional for opt! args.
			c.neutralizeForceUnwrapSource(arg.Value)
		}
		newMethod := named.LookupMethod("new")
		if newMethod != nil {
			// T0418: Resolve T-typed params against the owner type's
			// concrete args for generic value types.
			ownerSubst := c.buildOwnerTypeArgSubst(typ)
			argVals = c.coerceCallArgs(argVals, argTypes, newMethod.Sig().Params(), e.Args, ownerSubst)
		}
		thisPtr := c.block.NewBitCast(alloca, irtypes.I8Ptr)
		args := append([]value.Value{thisPtr}, argVals...)
		c.block.NewCall(fn, args...)
		return c.block.NewLoad(valueStructType, alloca)
	}

	// Implicit constructor: match arguments to field names
	// B0210: Build a local substitution map from the Instance type args so field types
	// are properly substituted even when c.typeSubst is nil.
	var vtLocalSubst map[*types.TypeParam]types.Type
	if inst, ok := typ.(*types.Instance); ok {
		if origin, ok := inst.Origin().(*types.Named); ok && len(origin.TypeParams()) > 0 {
			vtLocalSubst = types.BuildSubstMap(origin.TypeParams(), inst.TypeArgs())
		}
	}
	fieldTypeMap := make(map[string]types.Type)
	for _, f := range named.AllFields() {
		ft := f.Type()
		if c.typeSubst != nil {
			ft = types.Substitute(ft, c.typeSubst)
		}
		if vtLocalSubst != nil {
			ft = types.Substitute(ft, vtLocalSubst)
		}
		fieldTypeMap[f.Name()] = ft
	}

	maybeWrapOptional := func(v value.Value, expr ast.Expr, fieldName string, fieldIdx int) value.Value {
		fieldType := fieldTypeMap[fieldName]
		if _, isOpt := fieldType.(*types.Optional); !isOpt {
			return v
		}
		exprType := c.info.Types[expr]
		if c.typeSubst != nil {
			exprType = types.Substitute(exprType, c.typeSubst)
		}
		if exprType == types.TypNone {
			return v
		}
		// Use Identical (not "is exprOpt?") so a T? expr targeting a T?? field
		// still gets wrapped to match the slot's depth.
		if types.Identical(exprType, fieldType) {
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
		// B0210: For optional fields, generate none values directly from the layout
		// type instead of resolveType(targetType), which may produce a wrong LLVM
		// type when TypeParams aren't fully substituted in all code paths.
		var fieldVal value.Value
		if _, isOpt := fieldTypeMap[arg.Name].(*types.Optional); isOpt {
			if _, isNone := arg.Value.(*ast.NoneLit); isNone {
				fieldVal = c.zeroValue(layout.Value.Fields[fieldIdx].LLVMType)
			} else {
				if ft, ok := fieldTypeMap[arg.Name].(*types.Optional); ok {
					c.targetType = ft
				}
				fieldVal = c.genCallArgExpr(arg.Value)
				c.targetType = nil
				fieldVal = maybeWrapOptional(fieldVal, arg.Value, arg.Name, fieldIdx)
			}
		} else {
			fieldVal = c.genCallArgExpr(arg.Value)
			fieldVal = maybeWrapOptional(fieldVal, arg.Value, arg.Name, fieldIdx)
		}
		val = c.block.NewInsertValue(val, fieldVal, uint64(fieldIdx))
		if ident, ok := arg.Value.(*ast.IdentExpr); ok {
			c.clearDropFlag(ident.Name)
		}
		// B0301: Neutralize source optional for opt! args in value-type constructors.
		c.neutralizeForceUnwrapSource(arg.Value)
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

// --- Tuple ---

func (c *Compiler) genTupleLit(e *ast.TupleLit) value.Value {
	lt := c.resolveType(c.info.Types[e])
	structType, ok := lt.(*irtypes.StructType)
	if !ok {
		panic(fmt.Sprintf("codegen: tuple type resolved to %T, want StructType", lt))
	}
	var agg value.Value = constant.NewZeroInitializer(structType)
	for i, elem := range e.Elements {
		savedEnumTemps := len(c.enumCtorTemps)
		elemVal := c.genExpr(elem)
		agg = c.block.NewInsertValue(agg, elemVal, uint64(i))
		// B0242: Clear drop flags for ident elements consumed by the tuple.
		// When a dup'd match binding is embedded in a tuple (e.g., (k, v)),
		// ownership transfers to the tuple — the binding must not be dropped
		// at arm-scope cleanup. No-op if the ident has no drop flag.
		if ident, ok := elem.(*ast.IdentExpr); ok {
			c.clearDropFlag(ident.Name)
		}
		// T0784: same for `x as!/as T` element — cast subject is moved into
		// the tuple slot, so suppress its scope-exit drop binding. T0849: for
		// the conditional `as` form, drop iff the downcast failed.
		if ident := c.castSubjectMovableIdent(elem); ident != nil {
			c.consumeCastSubjectDropFlag(elem, ident.Name)
		}
		// T1073: `(o!, ..)` — force-unwrap moves the inner out of the source
		// optional into the tuple slot (which the tuple's drop frees). Neutralize
		// the source optional's present flag so its scope-exit drop doesn't
		// double-free the moved inner.
		c.neutralizeForceUnwrapElem(elem)
		// T0371: Claim heap-tracked field temps so they are not double-freed at
		// stmt end (their ownership is now in the tuple slot). Mirrors the
		// pattern used in genArrayLit / genMapLit. Without these claims:
		//   - string/vector/channel stmt-temps would self-clean at stmt end,
		//     leaving a dangling pointer in the tuple slot (case D/E garbage).
		//   - heap user-type temps would be freed at stmt end, then dropped
		//     again when the tuple is consumed (case A double-free).
		c.claimStringTemp(elemVal) // strings, vectors, channels, arcs, mutexes
		c.claimHeapTemp(elemVal)   // heap user-type instances
		c.claimEnvTemp(elemVal)    // T0741: closure env (tuple owns it now)
		// Clear enum ctor temps created during this element's evaluation so
		// the tuple is the unique owner of the enum's variant data.
		// T1139: gate on the element's static type being an enum — a non-enum
		// element that merely BORROWS an inline Enum.V(x) temp in a sub-call
		// leaves an intermediate the tuple never owns; it must stay tracked so
		// the caller drops it at statement end, else it leaks.
		elemEnumType := c.info.Types[elem]
		if c.typeSubst != nil {
			elemEnumType = types.Substitute(elemEnumType, c.typeSubst)
		}
		if extractEnum(elemEnumType) != nil {
			for j := savedEnumTemps; j < len(c.enumCtorTemps); j++ {
				c.block.NewStore(constant.NewInt(irtypes.I1, 0), c.enumCtorTemps[j].dropFlag)
			}
			c.enumCtorTemps = c.enumCtorTemps[:savedEnumTemps]
		}
	}
	return agg
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
	_, elemIsOpt := elem.(*types.Optional)

	// Try static .rodata path: all elements must be compile-time constants.
	// T1297: skip the static path for Optional element types — tryConstantExpr
	// would emit a bare i64/i1 constant for a {i1,T} slot (IR type mismatch);
	// optional-element vectors take the heap path so each element can be
	// Some-wrapped below.
	if !elemIsOpt {
		if consts := c.tryConstantElements(e.Elements, elem, elemLLVM); consts != nil {
			return c.genStaticVectorLit(int64(len(e.Elements)), elemLLVM, consts)
		}
	}

	elemSize := int64(c.typeSize(elemLLVM))
	n := int64(len(e.Elements))

	// Total allocation: header (16 bytes) + n * elemSize
	totalSize := int64(vectorHeaderSize) + n*elemSize

	// malloc
	rawPtr := c.block.NewCall(c.palAlloc,
		constant.NewInt(irtypes.I64, totalSize))

	// B0201/B0359: Track the vector allocation as a heap temp. This serves two
	// purposes: (1) if a failable element evaluation triggers error auto-propagation,
	// the vector is freed (B0201); (2) when a vector literal is passed directly as
	// a non-variadic function argument (e.g., foo(["hello"])), the caller frees the
	// buffer at statement end via cleanupHeapTemps (B0359). Variable assignments
	// claim the temp via claimHeapTemp, preventing double-free. This code only
	// runs on the heap path — static (.rodata) vectors return earlier via
	// genStaticVectorLit and are never tracked.
	// T0369: Use Vector.drop with element-type info so transient cleanup walks
	// droppable elements (string concats, nested vectors, channels, heap user
	// types, enum heap variant data) before freeing the buffer. The helper
	// returns false when the walk is suppressed (elemType transitively contains
	// a droppable tuple — see T0371): in that path the per-element claims below
	// are skipped so each tracked temp self-cleans up at stmt end instead of
	// being orphaned by a buffer-only Vector.drop.
	walkEnabled := c.trackVectorHeapTempWithElemType(rawPtr, elem)

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
		savedEnumTemps := len(c.enumCtorTemps)
		// T1215: a Vector index element (`[payload[i]]`) read into this literal
		// aliases the source vector's element buffer — dup-on-read so the new
		// vector owns an independent copy, else both vectors' element drops free
		// the same allocation ("fatal: invalid free (bad header magic)"). No-op
		// for non-index elements (armDupForVectorIndexArg only arms for `v[i]`).
		c.armDupForVectorIndexArg(elemExpr)
		// T1297: set targetType to the Optional element type so a bare `none`
		// element lowers to a zero {i1,T} struct via genNoneLit (mirrors the
		// T0658 Vector.push path).
		savedTarget := c.targetType
		if elemIsOpt {
			c.targetType = elem
		}
		val := c.genCallArgExpr(elemExpr)
		c.targetType = savedTarget
		c.dupStringFieldAccess = false
		c.dupContainerFieldAccess = false
		c.dupHeapUserFieldAccess = false
		elemExprType := c.info.Types[elemExpr]
		if c.typeSubst != nil && elemExprType != nil {
			elemExprType = types.Substitute(elemExprType, c.typeSubst)
		}
		// T1558: coerce every element to the vector's element type before the
		// store — matching the Vector.push path (expr_container.go). A value-type
		// element boxes into a {vtable, box} structural view; a subtype element
		// gets its view-specific vtable. Without this, a value-type element's wide
		// value struct is stored straight into the {i8*, i8*} slot → codegen panic
		// ("store operands are not compatible"), a check/build disagreement.
		// No-op for an identical element type or an Optional elem (extractNamed
		// doesn't peel Optional — the coerceToOptionalElem below handles the
		// under-optional box). The heap box coerceToView registers is claimed
		// below via claimHeapTemp(storeVal) so the vector owns it exactly once.
		storeVal := c.coerceToView(val, elemExprType, elem)
		// T1297: wrap a bare non-optional element into the Optional element
		// struct when the vector element type is Optional but the element expr
		// is not (`int?[] = [1, none, 3]`, `int[]?[] = [[1,2]]`).
		if elemIsOpt {
			// T1298: box a structural-view element into the Optional element view
			// before wrapping (no-op for a non-structural or already-matching elem).
			storeVal = c.coerceToOptionalElem(storeVal, elemExprType, elem)
			if elemExprType != types.TypNone && !types.Identical(elemExprType, elem) {
				if st, ok := elemLLVM.(*irtypes.StructType); ok {
					storeVal = c.wrapOptional(storeVal, st)
				}
			}
		}
		elemPtr := c.block.NewGetElementPtr(elemLLVM, dataTypedPtr,
			constant.NewInt(irtypes.I64, int64(i)))
		c.block.NewStore(storeVal, elemPtr)
		if walkEnabled {
			// T0610: An ident element of a type that Vector.drop's element-walk
			// frees is *moved* into the vector (ownership marks it Moved). The
			// vector now owns it, so the source variable's scope-exit drop
			// binding must be suppressed — otherwise both free the same
			// allocation (double-free / SEGV for Mutex/Task/heap-user/string/
			// nested-vector). Mirrors genTupleLit (B0242) / genMapLit (B0280).
			// Type-gated to exactly the set emitVectorElementDropLoop walks
			// (stmt.go:3640-3644): clearing for element types it does NOT walk
			// (e.g. Optional[heap-user]) would orphan the source's allocation
			// → leak. No-op for Copy/borrow idents (no drop flag).
			if ident, ok := elemExpr.(*ast.IdentExpr); ok && !isRefType(elem) {
				if extractNamed(elem) == types.TypString ||
					c.vecElemNeedsEnumDrop(elem) ||
					c.vecElemNeedsUserTypeDrop(elem) ||
					c.tupleNeedsDrop(elem) ||
					c.vecElemNeedsOptionalDrop(elem) ||
					c.vecElemNeedsStructuralDrop(elem) || // T1558: structural-view element moved into vector
					isSignatureElem(elem) { // T1237: closure element moved into vector
					c.clearDropFlag(ident.Name)
				}
			}
			// T0784: same gating for `x as!/as T` element — Vector.drop walks the
			// element-type and would free the slot, so the cast subject's
			// scope-exit drop binding must be suppressed to avoid a double-free.
			if !isRefType(elem) {
				if extractNamed(elem) == types.TypString ||
					c.vecElemNeedsEnumDrop(elem) ||
					c.vecElemNeedsUserTypeDrop(elem) ||
					c.tupleNeedsDrop(elem) ||
					c.vecElemNeedsOptionalDrop(elem) ||
					c.vecElemNeedsStructuralDrop(elem) { // T1558: `x as Tagged` cast subject moved into vector
					if ident := c.castSubjectMovableIdent(elemExpr); ident != nil {
						// T0849: conditional `as` form drops iff the cast failed.
						c.consumeCastSubjectDropFlag(elemExpr, ident.Name)
					}
				}
			}
			// T1073: `[o!]` — force-unwrap moves the inner out of the source
			// optional into the vector slot (which Vector.drop frees on the
			// walkEnabled path, the enclosing branch). Neutralize the source
			// optional's present flag so its scope-exit drop doesn't double-free
			// the moved inner. Only correct under walkEnabled: when false,
			// Vector.drop does NOT free elements, so the source must keep ownership.
			c.neutralizeForceUnwrapElem(elemExpr)
			// B0233: Claim heap temp — element ownership transferred to vector literal.
			// T1558: claim on the coerced storeVal (not the raw val): for a value-type
			// element, coerceToView above heap-boxed it into a {vtable, box} view and
			// registered the box as a heap temp — its owner is the box pointer in
			// storeVal's field 1, which the raw value struct does not carry. For every
			// other element type storeVal aliases val's field-1 pointer, so the runtime
			// claim matches identically.
			c.claimHeapTemp(storeVal)
			// T0366: Also claim string/vector/channel stmt-temps. trackVectorTempWithElemType
			// (called by CallExpr / ?! / ?^ / ! / ? e {} for Vector results) registers in
			// stmtTemps, not heapTemps — claimHeapTemp doesn't see them. Without claiming,
			// the caller's stmt-temp cleanup runs Vector.drop while the gather buffer (owned
			// by the variadic callee) also drops each element → double-free.
			c.claimStringTemp(val)
			// T0741: claim closure env — element ownership transferred to vector;
			// the vector's element-drop loop now frees each closure's env.
			c.claimEnvTemp(val)
			// B0281: Clear enum ctor temps created during this element's evaluation.
			// Same issue as map literals: the enum value is stored by LLVM value,
			// so both the temp alloca and the vector slot share inner pointers.
			// Only clear temps added since savedEnumTemps to avoid clobbering
			// temps from outer expressions.
			// T1139: gate on the element's static type being an enum — a non-enum
			// element that merely BORROWS an inline Enum.V(x) temp in a sub-call
			// leaves an intermediate the vector never owns; it must stay tracked
			// so the caller drops it at statement end, else it leaks.
			elemEnumType := c.info.Types[elemExpr]
			if c.typeSubst != nil {
				elemEnumType = types.Substitute(elemEnumType, c.typeSubst)
			}
			if extractEnum(elemEnumType) != nil {
				for j := savedEnumTemps; j < len(c.enumCtorTemps); j++ {
					c.block.NewStore(constant.NewInt(irtypes.I1, 0), c.enumCtorTemps[j].dropFlag)
				}
				c.enumCtorTemps = c.enumCtorTemps[:savedEnumTemps]
			}
		}
		// T0369: When walk is suppressed, leave heap temps / string stmt-temps /
		// enum ctor temps tracked. They self-clean at stmt end so nothing is
		// orphaned by the buffer-only Vector.drop. The vector slot retains the
		// pointer, but ownership has not been transferred — both the slot and
		// the original tracker reference the same heap value, and the buffer
		// free does NOT walk them, so each tracker drops its own value once.
	}

	return rawPtr // i8*
}

// tryConstantElements checks if all array literal elements are compile-time constants
// (int, float, bool, char literals). Returns a slice of LLVM constants or nil if any
// element is non-constant.
func (c *Compiler) tryConstantElements(elements []ast.Expr, elemType types.Type, elemLLVM irtypes.Type) []constant.Constant {
	if len(elements) == 0 {
		return []constant.Constant{} // empty static vector
	}
	consts := make([]constant.Constant, 0, len(elements))
	for _, expr := range elements {
		cv := c.tryConstantExpr(expr, elemType, elemLLVM)
		if cv == nil {
			return nil
		}
		consts = append(consts, cv)
	}
	return consts
}

// tryConstantExpr attempts to evaluate an expression as a compile-time constant.
// Returns nil if the expression is not a constant literal.
func (c *Compiler) tryConstantExpr(expr ast.Expr, elemType types.Type, elemLLVM irtypes.Type) constant.Constant {
	switch e := expr.(type) {
	case *ast.IntLit:
		intType, ok := elemLLVM.(*irtypes.IntType)
		if !ok {
			intType = irtypes.I64
		}
		// T1418: parse at full precision. The old strconv.ParseInt/ParseUint pair
		// truncated to 64 bits and constant.NewInt then SIGN-EXTENDED the int64
		// into the wide element type, so every u128/i128/i256/i512 element above
		// int64.max was silently written wrong (2^63..2^64-1 got all-ones high
		// bits; >= 2^64 became -1 because the ParseUint range error was dropped).
		cv, parsed := constIntFromRaw(intType, e.Raw)
		if !parsed {
			return nil // unreachable post-sema; fall back to the heap path
		}
		return cv
	case *ast.FloatLit:
		floatType, ok := elemLLVM.(*irtypes.FloatType)
		if !ok {
			floatType = irtypes.Double
		}
		return constFloatFromRaw(floatType, e.Raw)
	case *ast.BoolLit:
		return constBool(e.Value)
	case *ast.CharLit:
		return constCharFromRaw(e.Raw)
	case *ast.UnaryExpr:
		// Handle negative literals: -42, -3.14
		if e.Op == ast.UnaryNeg {
			inner := c.tryConstantExpr(e.Operand, elemType, elemLLVM)
			if inner == nil {
				return nil
			}
			switch v := inner.(type) {
			case *constant.Int:
				// T1418: negate at full precision — v.X.Int64() truncated wide
				// magnitudes (-i128.min folded to +1).
				return &constant.Int{Typ: v.Typ, X: new(big.Int).Neg(v.X)}
			case *constant.Float:
				neg, _ := v.X.Float64()
				return constant.NewFloat(v.Typ, -neg)
			}
		}
		return nil
	}
	return nil
}

// genStaticVectorLit emits a static .rodata global for a vector literal with all-constant elements.
// Vector layout: {i64 len|bit63, i64 cap, [N x elemType] data}
// Returns i8* pointer to the global.
func (c *Compiler) genStaticVectorLit(n int64, elemLLVM irtypes.Type, consts []constant.Constant) value.Value {
	arrType := irtypes.NewArray(uint64(n), elemLLVM)

	// Build the global struct type: {i64, i64, [N x T]}
	globalType := irtypes.NewStruct(irtypes.I64, irtypes.I64, arrType)

	// Length with static flag (bit 63) set
	staticLen := n | math.MinInt64

	// Build array constant
	arrConst := constant.NewArray(arrType, consts...)

	init := constant.NewStruct(globalType,
		constant.NewInt(irtypes.I64, staticLen), // len | bit63
		constant.NewInt(irtypes.I64, n),         // cap
		arrConst,                                // data
	)

	var globalName string
	if c.compilingModule != "" {
		globalName = fmt.Sprintf(".arr.__mod_%s.%d", c.compilingModule, c.moduleArrCounter)
		c.moduleArrCounter++
	} else {
		globalName = fmt.Sprintf(".arr.%d", c.arrCounter)
		c.arrCounter++
	}
	global := c.module.NewGlobalDef(globalName, init)
	global.Immutable = true
	global.Linkage = enum.LinkagePrivate

	return c.block.NewBitCast(global, irtypes.I8Ptr)
}

// genFixedArrayLit generates a stack-allocated fixed-size array literal.
// Returns the full [N x T] value (not a pointer).
func (c *Compiler) genFixedArrayLit(e *ast.ArrayLit, arr *types.Array) value.Value {
	elemLLVM := c.resolveType(arr.Elem())
	arrType := irtypes.NewArray(uint64(arr.Size()), elemLLVM)

	// T0389: When the array's element type is droppable, the new bindingDropArray
	// (registered by maybeRegisterDrop) takes ownership of each slot at scope
	// exit. To avoid double-free, claim element temps so stmt-end cleanup
	// doesn't also free them. Gating on variantFieldNeedsDrop keeps non-droppable
	// element types (e.g. Optional[string]) on their pre-T0389 path where the
	// source variables drop normally — without this gate, clearing an ident's
	// drop flag would orphan its inner allocation since no array binding fires.
	resolvedElem := arr.Elem()
	if c.typeSubst != nil {
		resolvedElem = types.Substitute(resolvedElem, c.typeSubst)
	}
	claim := c.variantFieldNeedsDrop(resolvedElem)
	_, elemIsOpt := resolvedElem.(*types.Optional)

	tmp := c.createEntryAlloca(arrType)
	for i, elemExpr := range e.Elements {
		savedEnumTemps := len(c.enumCtorTemps)
		// T1297: set targetType to the Optional element type so a bare `none`
		// element lowers to a zero {i1,T} struct via genNoneLit (mirrors the
		// T0658 Vector.push path).
		savedTarget := c.targetType
		if elemIsOpt {
			c.targetType = resolvedElem
		}
		val := c.genCallArgExpr(elemExpr)
		c.targetType = savedTarget
		// T1297: wrap a bare non-optional element into the Optional element
		// struct when the array element type is Optional but the element expr
		// is not (`int?[3] = [1, none, 3]`, `int[]?[2] = [[1,2],[3,4]]`). The
		// claim logic below still operates on the raw `val` (ownership tracking
		// is by raw-value identity), mirroring the T0658 push path exactly.
		storeVal := val
		if elemIsOpt {
			argExprType := c.info.Types[elemExpr]
			if c.typeSubst != nil && argExprType != nil {
				argExprType = types.Substitute(argExprType, c.typeSubst)
			}
			if argExprType != types.TypNone && !types.Identical(argExprType, resolvedElem) {
				storeVal = c.coerceToOptionalElem(val, argExprType, resolvedElem)
				if st, ok := elemLLVM.(*irtypes.StructType); ok {
					storeVal = c.wrapOptional(storeVal, st)
				}
			}
		}
		ptr := c.block.NewGetElementPtr(arrType, tmp,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(i)))
		c.block.NewStore(storeVal, ptr)
		if !claim {
			continue
		}
		// Element ownership transfers to the array binding — claim temps and
		// clear ident drop flags so they are not double-freed at stmt end or
		// at the source variable's scope exit. Mirrors genTupleLit.
		if ident, ok := elemExpr.(*ast.IdentExpr); ok {
			c.clearDropFlag(ident.Name)
		}
		c.claimStringTemp(val) // strings, vectors, channels, arcs, mutexes
		c.claimHeapTemp(val)   // heap user-type instances
		c.claimEnvTemp(val)    // T0741: closure env (array owns it now)
		// Clear enum ctor temps created during this element's evaluation so
		// the array is the unique owner of the enum's variant data.
		// T1139: gate on the element's static type being an enum — a non-enum
		// element that merely BORROWS an inline Enum.V(x) temp in a sub-call
		// leaves an intermediate the array never owns; it must stay tracked so
		// the caller drops it at statement end, else it leaks.
		elemEnumType := c.info.Types[elemExpr]
		if c.typeSubst != nil {
			elemEnumType = types.Substitute(elemEnumType, c.typeSubst)
		}
		if extractEnum(elemEnumType) != nil {
			for j := savedEnumTemps; j < len(c.enumCtorTemps); j++ {
				c.block.NewStore(constant.NewInt(irtypes.I1, 0), c.enumCtorTemps[j].dropFlag)
			}
			c.enumCtorTemps = c.enumCtorTemps[:savedEnumTemps]
		}
	}
	return c.block.NewLoad(arrType, tmp)
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
			savedEnumTemps := len(c.enumCtorTemps)
			keyVal := c.genExpr(entry.Key)
			valVal := c.genExpr(entry.Value)
			c.block.NewCall(setFn, instancePtr, keyVal, valVal)
			// B0280: Clear drop flags for values moved into the map via []=.
			// The []= method takes ~K key and ~V value (move semantics), so
			// ownership transfers to the map. Without this, the caller's
			// scope-exit cleanup double-drops the value (use-after-free).
			if ident, ok := entry.Value.(*ast.IdentExpr); ok {
				c.clearDropFlag(ident.Name)
			}
			if ident, ok := entry.Key.(*ast.IdentExpr); ok {
				c.clearDropFlag(ident.Name)
			}
			// T0784: same for `x as!/as T` key/value — cast subject is moved
			// into the map slot via []=, so suppress its scope-exit drop.
			// T0849: for the conditional `as` form, drop iff the downcast failed.
			if ident := c.castSubjectMovableIdent(entry.Value); ident != nil {
				c.consumeCastSubjectDropFlag(entry.Value, ident.Name)
			}
			if ident := c.castSubjectMovableIdent(entry.Key); ident != nil {
				c.consumeCastSubjectDropFlag(entry.Key, ident.Name)
			}
			// T1073: `{k!: v!}` — force-unwrap of a droppable inner moves it out of
			// the source optional into the map slot (which the map's drop frees via
			// []=). Neutralize the source optional's present flag so its scope-exit
			// drop doesn't double-free the moved inner.
			c.neutralizeForceUnwrapElem(entry.Value)
			c.neutralizeForceUnwrapElem(entry.Key)
			// Claim heap temps: user type instances passed as map values
			// transfer ownership to the map. Without this, the heap temp
			// cleanup would free the instance, leaving a dangling pointer
			// in the map's Slot enum data.
			c.claimHeapTemp(valVal)
			c.claimHeapTemp(keyVal)
			// T0736: Also claim string/vector stmt-temps for the moved key/value.
			// trackStringTemp / trackVectorTempWithElemType (string concat,
			// to_string(), split(), vector-returning calls) register in stmtTemps,
			// NOT heapTemps — claimHeapTemp doesn't see them. The []= method moves
			// ~K key / ~V value into the map, so without claiming, the caller's
			// stmt-temp cleanup drops the string/vector while the map's scope-exit
			// drop also drops it → double-free ("invalid free (bad header magic)").
			// Only the ident path above is currently covered; a bare heap
			// sub-expression ({"k": a + b}) needs this. Mirrors the genArrayLit
			// element path (T0366).
			c.claimStringTemp(valVal)
			c.claimStringTemp(keyVal)
			// T1239: also claim the closure env temp. Map.[]= takes ~V value by
			// move, so the map's drop owns the closure's heap env. Without this
			// the statement-end cleanupEnvTemps double-frees it → segfault.
			// Mirrors genArrayLit (T0741). The key claim is defensive (a closure
			// can't be Hashable, so not a valid map key today) but costs nothing —
			// claimEnvTemp is a no-op for non-closure values. A capturing lambda
			// literal value has always hit this; T1160 widened the trigger to
			// closure-returning call results.
			c.claimEnvTemp(valVal)
			c.claimEnvTemp(keyVal)
			// B0281: Clear enum ctor temps created during this entry's evaluation.
			// Map.[]= copies the enum value by LLVM value into the map's Slot.
			// Both the temp alloca and the Slot share the same inner pointers
			// (string ptrs, map instance ptrs, etc.). If the temp is dropped
			// at statement end, it frees data the map still references →
			// use-after-free / stack overflow on cleanup.
			// Only clear temps added since savedEnumTemps to avoid clobbering
			// temps from outer expressions (e.g., prior function arguments).
			// T1139: gate on either the key OR the value's static type being an
			// enum — the snapshot covers both entry.Key and entry.Value, so clear
			// the range only if at least one slot is enum-typed. When neither is
			// an enum (e.g. {"k": inspect(Holder.Has(...))}), a borrow
			// intermediate enum-ctor temp evaluated inside the entry must stay
			// tracked so the caller drops it at statement end, else it leaks.
			// Residual: the mixed case (enum value + non-enum key that borrows a
			// *different* enum-ctor temp) still over-claims that inner temp — the
			// whole range is cleared rather than just the moved value's backing
			// temp. Lower priority, contrived nesting.
			keyEnumType := c.info.Types[entry.Key]
			valEnumType := c.info.Types[entry.Value]
			if c.typeSubst != nil {
				keyEnumType = types.Substitute(keyEnumType, c.typeSubst)
				valEnumType = types.Substitute(valEnumType, c.typeSubst)
			}
			if extractEnum(keyEnumType) != nil || extractEnum(valEnumType) != nil {
				for i := savedEnumTemps; i < len(c.enumCtorTemps); i++ {
					c.block.NewStore(constant.NewInt(irtypes.I1, 0), c.enumCtorTemps[i].dropFlag)
				}
				c.enumCtorTemps = c.enumCtorTemps[:savedEnumTemps]
			}
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

	// T0735: Track allocation as heap temp so unclaimed map literals used as rvalue
	// temporaries (function args, method-call receivers) are dropped at statement end.
	// Registered with palFree as the safe default; updateConstructorTempDrop below
	// swaps it for Map[K,V].drop after new() completes. Mirrors genConstructorCallMono
	// (T0135 + T0345).
	c.trackHeapTemp(rawPtr, c.palFree)

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

	// T0735: Swap dropFn palFree → Map[K,V].drop (synthesized; walks _buckets vector,
	// then pal_frees the instance). Without this, cleanupHeapTemps would just pal_free
	// the instance and leak the _buckets vector buffer.
	c.updateConstructorTempDrop(rawPtr, origin, inst)

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
