package codegen

import (
	"fmt"

	"github.com/llir/llvm/ir"
	"github.com/llir/llvm/ir/constant"
	"github.com/llir/llvm/ir/enum"
	irtypes "github.com/llir/llvm/ir/types"
	"github.com/llir/llvm/ir/value"

	"djabi.dev/go/promise_lang/internal/ast"
	"djabi.dev/go/promise_lang/internal/sema"
	"djabi.dev/go/promise_lang/internal/types"
)

// genBlock generates LLVM IR for a block of statements.
func (c *Compiler) genBlock(block *ast.Block) {
	if block == nil {
		return
	}
	savedScopeLen := len(c.scopeBindings)
	for _, stmt := range block.Stmts {
		if c.block == nil || c.block.Term != nil {
			break // block already terminated (return, break, etc.)
		}
		c.genStmt(stmt)
	}
	// Emit cleanup calls for scope bindings added in this block (fall-through exit)
	if c.block != nil && c.block.Term == nil && len(c.scopeBindings) > savedScopeLen {
		c.emitScopeCleanup(savedScopeLen)
	}
	c.scopeBindings = c.scopeBindings[:savedScopeLen]
}

// genStmt generates LLVM IR for a single statement.
func (c *Compiler) genStmt(stmt ast.Stmt) {
	switch s := stmt.(type) {
	case *ast.ExprStmt:
		c.genExpr(s.Expr)
	case *ast.ReturnStmt:
		c.genReturnStmt(s)
	case *ast.TypedVarDecl:
		c.genTypedVarDecl(s)
	case *ast.InferredVarDecl:
		c.genInferredVarDecl(s)
	case *ast.AssignStmt:
		c.genAssignStmt(s)
	case *ast.IfStmt:
		c.genIfStmt(s)
	case *ast.WhileStmt:
		c.genWhileStmt(s)
	case *ast.WhileUnwrapStmt:
		c.genWhileUnwrapStmt(s)
	case *ast.ForInStmt:
		c.genForInStmt(s)
	case *ast.ClassicForStmt:
		c.genClassicForStmt(s)
	case *ast.InfiniteLoop:
		c.genInfiniteLoop(s)
	case *ast.BreakStmt:
		c.genBreakStmt()
	case *ast.ContinueStmt:
		c.genContinueStmt()
	case *ast.RaiseStmt:
		c.genRaiseStmt(s)
	case *ast.DestructureVarDecl:
		c.genDestructureVarDecl(s)
	case *ast.UseVarDecl:
		c.genUseVarDecl(s)
	case *ast.IncDecStmt:
		c.genIncDecStmt(s)
	case *ast.SelectStmt:
		c.genSelectStmt(s)
	case *ast.Block:
		c.genBlock(s)
	default:
		panic(fmt.Sprintf("codegen: unhandled statement type %T", stmt))
	}
}

// --- Variable declarations ---

func (c *Compiler) genTypedVarDecl(s *ast.TypedVarDecl) {
	// Uninitialized optional: `T? x;` — zero-init (none)
	if s.Value == nil {
		declType := c.resolveTypeRefToType(s.Type)
		if declType == nil {
			return
		}
		lt := c.resolveType(declType)
		alloca := c.block.NewAlloca(lt)
		alloca.SetName(c.uniqueLocalName(s.Name))
		c.block.NewStore(constant.NewZeroInitializer(lt), alloca)
		c.locals[s.Name] = alloca
		return
	}

	// Resolve the declared type (from sema's type annotation)
	declType := c.lookupLocalType(s)
	exprType := c.info.Types[s.Value]

	// Use declared type for alloca when available (handles NoneLit → Optional)
	var lt irtypes.Type
	if declType != nil {
		lt = c.resolveType(declType)
	} else {
		lt = c.resolveType(exprType)
	}
	alloca := c.block.NewAlloca(lt)
	alloca.SetName(c.uniqueLocalName(s.Name))

	// Set targetType for contextual type resolution (NoneLit needs Optional(T))
	if declType != nil {
		c.targetType = declType
	}
	val := c.genExpr(s.Value)
	c.targetType = nil

	// Wrap value in Optional if declared type is Optional but expr is not
	if declType != nil {
		if _, isOpt := declType.(*types.Optional); isOpt {
			if _, isNone := exprType.(*types.Named); isNone && exprType == types.TypNone {
				// NoneLit already handled via targetType
			} else if _, exprOpt := exprType.(*types.Optional); !exprOpt {
				val = c.wrapOptional(val, lt.(*irtypes.StructType))
			}
		}
	}

	// Coerce value struct vtable when crossing type boundaries (e.g. Dog → Animal)
	coerceTarget := declType
	if coerceTarget == nil {
		// For non-Optional typed declarations, look up the declared type from sema scopes
		coerceTarget = c.lookupVarType(s.Name)
	}
	if coerceTarget != nil {
		val = c.coerceToView(val, exprType, coerceTarget)
	}

	c.block.NewStore(val, alloca)
	c.locals[s.Name] = alloca
	// Use declared type if available, otherwise fall back to expression type
	dropType := declType
	if dropType == nil {
		dropType = exprType
	}
	c.maybeRegisterDrop(s.Name, alloca, dropType)
	c.maybeRegisterEnvFree(s.Name, alloca, dropType)
}

func (c *Compiler) genInferredVarDecl(s *ast.InferredVarDecl) {
	typ := c.info.Types[s.Value]
	if c.typeSubst != nil {
		typ = types.Substitute(typ, c.typeSubst)
	}
	lt := c.resolveType(typ)
	alloca := c.block.NewAlloca(lt)
	alloca.SetName(c.uniqueLocalName(s.Name))
	val := c.genExpr(s.Value)
	c.block.NewStore(val, alloca)
	c.locals[s.Name] = alloca
	c.maybeRegisterDrop(s.Name, alloca, typ)
	c.maybeRegisterEnvFree(s.Name, alloca, typ)
}

// genDestructureVarDecl handles tuple destructuring: (a, b) := expr
func (c *Compiler) genDestructureVarDecl(s *ast.DestructureVarDecl) {
	tupleVal := c.genExpr(s.Value)
	tupleType := c.info.Types[s.Value]
	if c.typeSubst != nil {
		tupleType = types.Substitute(tupleType, c.typeSubst)
	}
	tup, ok := tupleType.(*types.Tuple)
	if !ok {
		panic(fmt.Sprintf("codegen: destructure value type is %T, want *types.Tuple", tupleType))
	}
	for i, name := range s.Names {
		if name == "_" {
			continue
		}
		elemType := c.resolveType(tup.Elems()[i])
		alloca := c.block.NewAlloca(elemType)
		alloca.SetName(c.uniqueLocalName(name))
		c.block.NewStore(c.block.NewExtractValue(tupleVal, uint64(i)), alloca)
		c.locals[name] = alloca
	}
}

// --- use binding ---

func (c *Compiler) genUseVarDecl(s *ast.UseVarDecl) {
	typ := c.info.Types[s.Value]
	if c.typeSubst != nil {
		typ = types.Substitute(typ, c.typeSubst)
	}
	lt := c.resolveType(typ)
	alloca := c.block.NewAlloca(lt)
	alloca.SetName(c.uniqueLocalName(s.Name))
	val := c.genExpr(s.Value)
	c.block.NewStore(val, alloca)
	c.locals[s.Name] = alloca

	// Track for scope-exit close() insertion
	named := extractNamed(typ)
	binding := scopeBinding{
		kind:    bindingClose,
		alloca:  alloca,
		named:   named,
		valType: typ,
	}
	// Resolve close function for direct dispatch
	if named != nil && (!c.needsVtable(named) || named.LookupMethod("close").IsNative()) {
		ownerName := c.resolveMethodOwner(named, "close")
		mangledName := mangleMethodName(ownerName, "close", false)
		if fn, ok := c.funcs[mangledName]; ok {
			binding.closeFunc = fn
		}
	}
	c.scopeBindings = append(c.scopeBindings, binding)
}

// --- drop binding ---

// maybeRegisterDrop checks if a variable's type has a drop() method and, if so,
// registers a drop binding: allocates a drop flag (i1, initially true), resolves
// the drop function, and appends a scopeBinding.
func (c *Compiler) maybeRegisterDrop(varName string, alloca *ir.InstAlloca, typ types.Type) {
	named := extractNamed(typ)
	if named == nil || !named.HasDrop() {
		return
	}

	// Allocate drop flag: i1, initialized to true (should drop)
	dropFlag := c.block.NewAlloca(irtypes.I1)
	dropFlag.SetName(c.uniqueLocalName(varName + ".dropflag"))
	c.block.NewStore(constant.NewInt(irtypes.I1, 1), dropFlag)
	c.dropFlags[varName] = dropFlag

	binding := scopeBinding{
		kind:     bindingDrop,
		alloca:   alloca,
		named:    named,
		valType:  typ,
		dropFlag: dropFlag,
		varName:  varName,
	}

	// Resolve drop function for direct dispatch
	if !c.needsVtable(named) || named.LookupMethod("drop").IsNative() {
		ownerName := c.resolveMethodOwner(named, "drop")
		mangledName := mangleMethodName(ownerName, "drop", false)
		if fn, ok := c.funcs[mangledName]; ok {
			binding.dropFunc = fn
		}
	}

	c.scopeBindings = append(c.scopeBindings, binding)
	c.dropBindings[varName] = binding
}

// clearDropFlag sets a variable's drop flag to false (indicating the value has been moved).
func (c *Compiler) clearDropFlag(name string) {
	if flag, ok := c.dropFlags[name]; ok {
		c.block.NewStore(constant.NewInt(irtypes.I1, 0), flag)
	}
}

// emitScopeCleanup emits cleanup calls for all scope bindings from fromIdx onwards,
// in reverse order (LIFO). Close bindings call close(), drop bindings check the
// drop flag and conditionally call drop().
func (c *Compiler) emitScopeCleanup(fromIdx int) {
	for i := len(c.scopeBindings) - 1; i >= fromIdx; i-- {
		b := c.scopeBindings[i]
		switch b.kind {
		case bindingClose:
			c.emitCloseCall(b)
		case bindingDrop:
			c.emitDropCall(b)
		case bindingFreeEnv:
			c.emitEnvFree(b)
		}
	}
}

// emitCloseCall emits a close() call for a use-bound variable (direct or virtual dispatch).
func (c *Compiler) emitCloseCall(b scopeBinding) {
	val := c.block.NewLoad(b.alloca.ElemType, b.alloca)

	if b.closeFunc != nil {
		// Direct dispatch — extract instance pointer and call
		instance := c.extractInstancePtr(val)
		c.block.NewCall(b.closeFunc, instance)
	} else if b.named != nil {
		// Virtual dispatch through vtable
		vtableRaw := c.extractVtablePtr(val)
		instance := c.extractInstancePtr(val)

		slotIndex := b.named.VirtualMethodIndex("close", false)
		if slotIndex < 0 {
			panic(fmt.Sprintf("codegen: close method not in vtable for %s", b.named))
		}
		vtablePtr := c.block.NewBitCast(vtableRaw, irtypes.NewPointer(irtypes.I8Ptr))
		fnSlotPtr := c.block.NewGetElementPtr(irtypes.I8Ptr, vtablePtr,
			constant.NewInt(irtypes.I32, int64(slotIndex)))
		fnRaw := c.block.NewLoad(irtypes.I8Ptr, fnSlotPtr)

		closeMethod := b.named.LookupMethod("close")
		retType := irtypes.Type(irtypes.Void)
		if closeMethod.Sig().CanError() {
			retType = computeResultType(retType)
		}
		funcType := irtypes.NewFunc(retType, irtypes.I8Ptr)
		fnTyped := c.block.NewBitCast(fnRaw, irtypes.NewPointer(funcType))
		c.block.NewCall(fnTyped, instance)
	}
}

// emitDropCall emits a conditional drop() call for a droppable variable.
// Checks the drop flag; if true (not moved), calls drop().
func (c *Compiler) emitDropCall(b scopeBinding) {
	if b.dropFlag == nil {
		// No drop flag — unconditional drop
		c.emitDropCallDirect(b)
		return
	}

	flag := c.block.NewLoad(irtypes.I1, b.dropFlag)
	dropBlock := c.newBlock("drop.call")
	skipBlock := c.newBlock("drop.skip")
	c.block.NewCondBr(flag, dropBlock, skipBlock)

	c.block = dropBlock
	c.emitDropCallDirect(b)
	c.block.NewBr(skipBlock)

	c.block = skipBlock
}

// emitDropCallDirect emits the actual drop() call (direct or virtual dispatch).
func (c *Compiler) emitDropCallDirect(b scopeBinding) {
	val := c.block.NewLoad(b.alloca.ElemType, b.alloca)

	if b.dropFunc != nil {
		instance := c.extractInstancePtr(val)
		c.block.NewCall(b.dropFunc, instance)
	} else if b.named != nil {
		vtableRaw := c.extractVtablePtr(val)
		instance := c.extractInstancePtr(val)

		slotIndex := b.named.VirtualMethodIndex("drop", false)
		if slotIndex < 0 {
			panic(fmt.Sprintf("codegen: drop method not in vtable for %s", b.named))
		}
		vtablePtr := c.block.NewBitCast(vtableRaw, irtypes.NewPointer(irtypes.I8Ptr))
		fnSlotPtr := c.block.NewGetElementPtr(irtypes.I8Ptr, vtablePtr,
			constant.NewInt(irtypes.I32, int64(slotIndex)))
		fnRaw := c.block.NewLoad(irtypes.I8Ptr, fnSlotPtr)

		funcType := irtypes.NewFunc(irtypes.Void, irtypes.I8Ptr)
		fnTyped := c.block.NewBitCast(fnRaw, irtypes.NewPointer(funcType))
		c.block.NewCall(fnTyped, instance)
	}
}

// emitEnvFree frees a closure's env struct at scope exit.
// Checks the drop flag (has the closure been moved?) and null-checks the env pointer.
func (c *Compiler) emitEnvFree(b scopeBinding) {
	if b.dropFlag == nil {
		return
	}
	flag := c.block.NewLoad(irtypes.I1, b.dropFlag)
	freeBlock := c.newBlock("env.free")
	skipBlock := c.newBlock("env.skip")
	c.block.NewCondBr(flag, freeBlock, skipBlock)

	c.block = freeBlock
	// Load closure, extract env ptr (field 1 of fat pointer)
	closure := c.block.NewLoad(b.alloca.ElemType, b.alloca)
	envPtr := c.block.NewExtractValue(closure, 1)
	// If non-null, free the env struct
	isNull := c.block.NewICmp(enum.IPredEQ, envPtr, constant.NewNull(irtypes.I8Ptr))
	callBlock := c.newBlock("env.free.call")
	c.block.NewCondBr(isNull, skipBlock, callBlock)

	c.block = callBlock
	c.block.NewCall(c.palFree, envPtr)
	c.block.NewBr(skipBlock)

	c.block = skipBlock
}

// maybeRegisterEnvFree registers a scope binding to free the closure's env struct
// at scope exit. Only applies to variables whose type is *types.Signature (function values).
func (c *Compiler) maybeRegisterEnvFree(varName string, alloca *ir.InstAlloca, typ types.Type) {
	if _, ok := typ.(*types.Signature); !ok {
		return
	}
	dropFlag := c.block.NewAlloca(irtypes.I1)
	dropFlag.SetName(c.uniqueLocalName(varName + ".dropflag"))
	c.block.NewStore(constant.NewInt(irtypes.I1, 1), dropFlag)
	c.dropFlags[varName] = dropFlag

	c.scopeBindings = append(c.scopeBindings, scopeBinding{
		kind:     bindingFreeEnv,
		alloca:   alloca,
		dropFlag: dropFlag,
		varName:  varName,
	})
}

// --- Assignment ---

func (c *Compiler) genAssignStmt(s *ast.AssignStmt) {
	// For compound index assignments, defer RHS evaluation to ensure correct
	// evaluation order: target → key → RHS (not RHS → target → key).
	if s.Op != ast.OpAssign {
		if idx, ok := s.Target.(*ast.IndexExpr); ok {
			c.genCompoundIndexAssign(idx, s.Op, s.Value)
			return
		}
	}

	val := c.genExpr(s.Value)

	switch target := s.Target.(type) {
	case *ast.IdentExpr:
		alloca, ok := c.locals[target.Name]
		if !ok {
			panic(fmt.Sprintf("codegen: undefined variable %q in assignment", target.Name))
		}
		if s.Op == ast.OpAssign {
			// Drop old value before reassignment (if target is droppable)
			if binding, ok := c.dropBindings[target.Name]; ok {
				// Skip self-assignment (would drop then store dangling pointer)
				if ident, ok := s.Value.(*ast.IdentExpr); ok && ident.Name == target.Name {
					return
				}
				c.emitDropCall(binding)
				// Reset drop flag: new value is now owned
				c.block.NewStore(constant.NewInt(irtypes.I1, 1), binding.dropFlag)
			}

			// Coerce value struct vtable when crossing type boundaries
			exprType := c.info.Types[s.Value]
			targetType := c.info.Types[target]
			if c.typeSubst != nil {
				exprType = types.Substitute(exprType, c.typeSubst)
				targetType = types.Substitute(targetType, c.typeSubst)
			}
			val = c.coerceToView(val, exprType, targetType)

			// Wrap value in Optional if target is Optional but expr is not
			if _, isOpt := targetType.(*types.Optional); isOpt {
				if exprType == types.TypNone {
					// none: already handled by genExpr (zeroinit)
				} else if _, exprOpt := exprType.(*types.Optional); !exprOpt {
					val = c.wrapOptional(val, alloca.ElemType.(*irtypes.StructType))
				}
			}

			c.block.NewStore(val, alloca)
			// Clear drop flag on RHS if it's being moved
			if ident, ok := s.Value.(*ast.IdentExpr); ok {
				c.clearDropFlag(ident.Name)
			}
			return
		}
		// Compound assignment: load current value, apply operator, store result
		current := c.block.NewLoad(alloca.ElemType, alloca)
		result := c.genCompoundOp(s.Op, current, val)
		c.block.NewStore(result, alloca)

	case *ast.MemberExpr:
		c.genMemberAssign(target, s.Op, val)
		// Clear drop flag on RHS if it's being moved via simple assign
		if s.Op == ast.OpAssign {
			if ident, ok := s.Value.(*ast.IdentExpr); ok {
				c.clearDropFlag(ident.Name)
			}
		}

	case *ast.IndexExpr:
		c.genIndexAssign(target, s.Op, val)
		// Clear drop flag on RHS if it's being moved via simple assign
		if s.Op == ast.OpAssign {
			if ident, ok := s.Value.(*ast.IdentExpr); ok {
				c.clearDropFlag(ident.Name)
			}
		}

	case *ast.SliceExpr:
		c.genSliceAssign(target, val)
		if s.Op == ast.OpAssign {
			if ident, ok := s.Value.(*ast.IdentExpr); ok {
				c.clearDropFlag(ident.Name)
			}
		}

	default:
		panic(fmt.Sprintf("codegen: unsupported assignment target %T", s.Target))
	}
}

// genMemberAssign handles assignment to a field on a user type instance.
// If the member is a setter property, emits a setter call instead.
// Uses lookupTypeLayout for layout-driven field types that work for both
// regular and monomorphic types.
func (c *Compiler) genMemberAssign(target *ast.MemberExpr, op ast.AssignOp, val value.Value) {
	// Check for setter property
	targetType := c.info.Types[target.Target]
	if c.typeSubst != nil {
		targetType = types.Substitute(targetType, c.typeSubst)
	}
	named := extractNamed(targetType)
	if named != nil {
		if setter := named.LookupSetter(target.Field); setter != nil {
			if op != ast.OpAssign {
				// Compound assignment (+=, -=, etc.): read via getter, apply op, write via setter
				getter := named.LookupGetter(target.Field)
				if getter == nil {
					panic(fmt.Sprintf("codegen: compound assignment to setter %s.%s but no getter found", named, target.Field))
				}
				current := c.genGetterCall(target, targetType, named, getter)
				val = c.genCompoundOp(op, current, val)
			}
			c.genSetterCall(target, targetType, named, setter, val)
			return
		}
	}

	fieldPtr := c.genFieldPtr(target)

	if op == ast.OpAssign {
		c.block.NewStore(val, fieldPtr)
		return
	}

	// Compound assignment: resolve field LLVM type for load
	layout := c.lookupTypeLayout(targetType)
	field := named.LookupField(target.Field)
	fieldIdx := layout.InstanceFieldIndex[field.Name()]
	fieldLLVMType := layout.Instance.Fields[fieldIdx].LLVMType
	current := c.block.NewLoad(fieldLLVMType, fieldPtr)
	result := c.genCompoundOp(op, current, val)
	c.block.NewStore(result, fieldPtr)
}

// genSetterCall emits a call to a setter method.
// Uses virtual dispatch through the vtable when the static type needs it.
func (c *Compiler) genSetterCall(target *ast.MemberExpr, targetType types.Type, named *types.Named, setter *types.Method, val value.Value) {
	// Virtual dispatch for setter when static type needs vtable
	if c.needsVtable(named) && !setter.IsNative() {
		c.genVirtualSetterCall(target, named, setter, val)
		return
	}

	var mangledName string
	ownerName := c.resolveMethodOwner(named, target.Field)
	if ownerName != named.Obj().Name() {
		mangledName = mangleMethodName(ownerName, target.Field, true)
	} else {
		mangledName = mangleMethodName(c.resolveTypeName(targetType), target.Field, true)
	}

	fn, ok := c.funcs[mangledName]
	if !ok {
		panic(fmt.Sprintf("codegen: undeclared setter %s", mangledName))
	}

	var args []value.Value
	recv := c.genExpr(target.Target)
	if _, isThis := target.Target.(*ast.ThisExpr); isThis {
		args = append(args, recv)
	} else if isContainerType(targetType) {
		args = append(args, recv)
	} else {
		args = append(args, c.extractInstancePtr(recv))
	}
	args = append(args, val)
	c.block.NewCall(fn, args...)
}

// genVirtualSetterCall emits an indirect setter call through the vtable.
func (c *Compiler) genVirtualSetterCall(target *ast.MemberExpr, named *types.Named, setter *types.Method, val value.Value) {
	receiverVal := c.genExpr(target.Target)

	var vtableRaw, instance value.Value
	if _, isThis := target.Target.(*ast.ThisExpr); isThis {
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

	slotIndex := named.VirtualMethodIndex(target.Field, true) // setter slot
	if slotIndex < 0 {
		panic(fmt.Sprintf("codegen: setter %s not in vtable for %s", target.Field, named))
	}
	vtablePtr := c.block.NewBitCast(vtableRaw, irtypes.NewPointer(irtypes.I8Ptr))
	fnSlotPtr := c.block.NewGetElementPtr(irtypes.I8Ptr, vtablePtr,
		constant.NewInt(irtypes.I32, int64(slotIndex)))
	fnRaw := c.block.NewLoad(irtypes.I8Ptr, fnSlotPtr)

	// Setter signature: (i8* receiver, ValueType val) → void
	valType := c.resolveType(setter.Sig().Params()[0].Type())
	paramTypes := []irtypes.Type{irtypes.I8Ptr, valType}
	retType := irtypes.Type(irtypes.Void)
	if setter.Sig().CanError() {
		retType = computeResultType(retType)
	}
	funcType := irtypes.NewFunc(retType, paramTypes...)
	fnTyped := c.block.NewBitCast(fnRaw, irtypes.NewPointer(funcType))

	c.block.NewCall(fnTyped, instance, val)
}

// genCompoundOp applies a compound assignment operator through the type system.
func (c *Compiler) genCompoundOp(op ast.AssignOp, current, val value.Value) value.Value {
	// Map compound op to binary operator name
	var binOp string
	switch op {
	case ast.OpAddAssign:
		binOp = "+"
	case ast.OpSubAssign:
		binOp = "-"
	case ast.OpMulAssign:
		binOp = "*"
	case ast.OpDivAssign:
		binOp = "/"
	case ast.OpModAssign:
		binOp = "%"
	default:
		panic(fmt.Sprintf("codegen: unsupported compound assignment %s", op))
	}

	// Use the type of the current value to look up the operator method
	named := c.namedFromLLVMType(current.Type())
	if named == nil {
		panic("codegen: cannot determine type for compound assignment")
	}

	method := named.LookupMethod(binOp)
	if method == nil {
		panic(fmt.Sprintf("codegen: no method %q on type %s for compound assignment", binOp, named))
	}

	if method.IsNative() {
		return c.emitNativeOp(named, binOp, current, val)
	}

	panic(fmt.Sprintf("codegen: non-native compound op %s.%s not yet implemented", named, binOp))
}

// --- Increment / Decrement ---

// genIncDecStmt generates code for x++ or x-- statements.
func (c *Compiler) genIncDecStmt(s *ast.IncDecStmt) {
	c.genIncDecTarget(s.Target, s.IsInc)
}

// genIncDecTarget applies ++ or -- to the given expression target.
func (c *Compiler) genIncDecTarget(target ast.Expr, isInc bool) {
	op := "++"
	if !isInc {
		op = "--"
	}
	targetType := c.info.Types[target]
	if c.typeSubst != nil {
		targetType = types.Substitute(targetType, c.typeSubst)
	}
	named := extractNamed(targetType)
	if named == nil {
		panic(fmt.Sprintf("codegen: cannot resolve Named type for inc/dec on %s", targetType))
	}

	switch t := target.(type) {
	case *ast.IdentExpr:
		alloca, ok := c.locals[t.Name]
		if !ok {
			panic(fmt.Sprintf("codegen: undefined variable %q in inc/dec", t.Name))
		}
		current := c.block.NewLoad(alloca.ElemType, alloca)
		result := c.emitNativeOp(named, op, current, nil)
		c.block.NewStore(result, alloca)
	case *ast.MemberExpr:
		// Load field, apply op, store back
		fieldPtr := c.genFieldPtr(t)
		fieldType := c.info.Types[target]
		llvmType := c.resolveType(fieldType)
		current := c.block.NewLoad(llvmType, fieldPtr)
		result := c.emitNativeOp(named, op, current, nil)
		c.block.NewStore(result, fieldPtr)
	case *ast.IndexExpr:
		indexTargetType := c.info.Types[t.Target]
		if c.typeSubst != nil {
			indexTargetType = types.Substitute(indexTargetType, c.typeSubst)
		}
		indexNamed := extractNamed(indexTargetType)
		if indexNamed == nil {
			panic(fmt.Sprintf("codegen: inc/dec on index of unresolved type %s", indexTargetType))
		}
		indexMethod := indexNamed.LookupMethod("[]")
		assignMethod := indexNamed.LookupMethod("[]=")

		if indexMethod != nil && indexMethod.IsNative() && assignMethod != nil && assignMethod.IsNative() {
			// Native path: direct memory access (vectors)
			elem, ok := types.AsVector(indexTargetType)
			if !ok && indexNamed == types.TypVector && c.typeSubst != nil {
				tp := indexNamed.TypeParams()[0]
				elem, ok = c.typeSubst[tp], c.typeSubst[tp] != nil
			}
			if !ok {
				panic(fmt.Sprintf("codegen: inc/dec on index of non-vector native type %s", indexTargetType))
			}
			slicePtr := c.genExpr(t.Target)
			idx := c.genExpr(t.Index)
			elemLLVM := c.resolveType(elem)

			headerType := vectorHeaderType()
			headerPtr := c.block.NewBitCast(slicePtr, irtypes.NewPointer(headerType))
			lenPtr := c.block.NewGetElementPtr(headerType, headerPtr,
				constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
			length := c.block.NewLoad(irtypes.I64, lenPtr)
			inBounds := c.block.NewICmp(enum.IPredULT, idx, length)
			okBlock := c.newBlock("incdec.index.ok")
			panicBlock := c.newBlock("incdec.index.oob")
			c.block.NewCondBr(inBounds, okBlock, panicBlock)

			c.block = panicBlock
			oobMsg := c.makeGlobalString("index out of bounds")
			c.block.NewCall(c.funcs["promise_panic"], oobMsg)
			c.block.NewUnreachable()

			c.block = okBlock
			dataBase := c.block.NewGetElementPtr(irtypes.I8, slicePtr,
				constant.NewInt(irtypes.I64, int64(vectorHeaderSize)))
			dataTypedPtr := c.block.NewBitCast(dataBase, irtypes.NewPointer(elemLLVM))
			elemPtr := c.block.NewGetElementPtr(elemLLVM, dataTypedPtr, idx)
			current := c.block.NewLoad(elemLLVM, elemPtr)
			result := c.emitNativeOp(named, op, current, nil)
			c.block.NewStore(result, elemPtr)
		} else if indexMethod != nil && assignMethod != nil {
			// Non-native: read via [], apply op, write via []=
			typeName := c.resolveTypeName(indexTargetType)
			getFnName := mangleMethodName(typeName, "[]", false)
			getFn, ok := c.funcs[getFnName]
			if !ok {
				panic(fmt.Sprintf("codegen: undeclared [] method %s", getFnName))
			}
			setFnName := mangleMethodName(typeName, "[]=", false)
			setFn, ok := c.funcs[setFnName]
			if !ok {
				panic(fmt.Sprintf("codegen: undeclared []= method %s", setFnName))
			}
			targetVal := c.genExpr(t.Target)
			keyVal := c.genExpr(t.Index)
			var instancePtr value.Value
			if isContainerType(indexTargetType) {
				instancePtr = targetVal
			} else {
				instancePtr = c.extractInstancePtr(targetVal)
			}
			// Read, inc/dec, write
			optVal := c.block.NewCall(getFn, instancePtr, keyVal)
			hasVal := c.block.NewExtractValue(optVal, 0)
			okBlock := c.newBlock("incdec.method.ok")
			panicBlock := c.newBlock("incdec.method.panic")
			c.block.NewCondBr(hasVal, okBlock, panicBlock)

			c.block = panicBlock
			panicMsg := c.makeGlobalString("inc/dec on missing key")
			c.block.NewCall(c.funcs["promise_panic"], panicMsg)
			c.block.NewUnreachable()

			c.block = okBlock
			current := c.block.NewExtractValue(optVal, 1)
			result := c.emitNativeOp(named, op, current, nil)
			c.block.NewCall(setFn, instancePtr, keyVal, result)
		} else {
			panic(fmt.Sprintf("codegen: inc/dec on index of type %s without []/[]= methods", indexTargetType))
		}
	default:
		panic(fmt.Sprintf("codegen: unsupported inc/dec target %T", target))
	}
}

// namedFromLLVMType reverse-maps an LLVM type to the most common Promise Named type.
// Used for compound assignments where we need the type system for operator dispatch.
// NOTE: Does not handle pointer types (i8*) — compound assignment on string/user-type
// fields is not supported until those types define operator methods.
func (c *Compiler) namedFromLLVMType(typ irtypes.Type) *types.Named {
	switch typ {
	case irtypes.I64:
		return types.TypInt
	case irtypes.I32:
		return types.TypI32
	case irtypes.I16:
		return types.TypI16
	case irtypes.I8:
		return types.TypI8
	case irtypes.I1:
		return types.TypBool
	case irtypes.Double:
		return types.TypF64
	case irtypes.Float:
		return types.TypF32
	}
	return nil
}

// --- Return ---

func (c *Compiler) genReturnStmt(s *ast.ReturnStmt) {
	// Clear drop flag for returned variable (it's being moved out, not dropped)
	if s.Value != nil {
		if ident, ok := s.Value.(*ast.IdentExpr); ok {
			c.clearDropFlag(ident.Name)
		}
	}
	// Emit cleanup for all active scope bindings before returning
	if len(c.scopeBindings) > 0 {
		c.emitScopeCleanup(0)
	}

	// Set targetType so NoneLit can resolve to the correct Optional struct
	retType := c.currentRetType
	if retType != nil && c.typeSubst != nil {
		retType = types.Substitute(retType, c.typeSubst)
	}
	if retType != nil && c.selfSubst != nil {
		retType = types.SubstituteSelf(retType, c.selfSubst.iface, c.selfSubst.concrete)
	}

	if c.canError {
		resultType := c.currentResultType()
		if s.Value == nil {
			c.block.NewRet(c.wrapOk(nil, resultType))
		} else {
			c.targetType = retType
			val := c.genExpr(s.Value)
			c.targetType = nil
			// Wrap value in Optional if return type is Optional but expr is not
			val = c.wrapReturnOptional(val, s.Value, retType)
			// Coerce value struct vtable when returning through a parent type
			if retType != nil {
				exprType := c.info.Types[s.Value]
				if c.typeSubst != nil {
					exprType = types.Substitute(exprType, c.typeSubst)
				}
				if c.selfSubst != nil {
					exprType = types.SubstituteSelf(exprType, c.selfSubst.iface, c.selfSubst.concrete)
				}
				val = c.coerceToView(val, exprType, retType)
			}
			c.block.NewRet(c.wrapOk(val, resultType))
		}
		return
	}
	if s.Value == nil {
		c.block.NewRet(nil)
	} else {
		c.targetType = retType
		val := c.genExpr(s.Value)
		c.targetType = nil
		// Wrap value in Optional if return type is Optional but expr is not
		val = c.wrapReturnOptional(val, s.Value, retType)
		// Coerce value struct vtable when returning through a parent type
		if retType != nil {
			exprType := c.info.Types[s.Value]
			if c.typeSubst != nil {
				exprType = types.Substitute(exprType, c.typeSubst)
			}
			if c.selfSubst != nil {
				exprType = types.SubstituteSelf(exprType, c.selfSubst.iface, c.selfSubst.concrete)
			}
			val = c.coerceToView(val, exprType, retType)
		}
		c.block.NewRet(val)
	}
}

// --- Raise ---

func (c *Compiler) genRaiseStmt(s *ast.RaiseStmt) {
	// Emit close() for all active use bindings before raising
	if len(c.scopeBindings) > 0 {
		c.emitScopeCleanup(0)
	}

	errVal := c.genExpr(s.Value)
	resultType := c.currentResultType()
	c.block.NewRet(c.wrapError(errVal, resultType))
}

// --- If statement ---

func (c *Compiler) genIfStmt(s *ast.IfStmt) {
	if s.Binding != "" {
		c.genIfUnwrapStmt(s)
		return
	}

	// Check for optional narrowing
	if narrow := c.info.OptionalNarrowings[s]; narrow != nil {
		c.genIfNarrowStmt(s, narrow)
		return
	}

	cond := c.genExpr(s.Cond)

	thenBlock := c.newBlock("if.then")
	mergeBlock := c.newBlock("if.end")

	var elseBlock *ir.Block
	if s.Else != nil {
		elseBlock = c.newBlock("if.else")
		c.block.NewCondBr(cond, thenBlock, elseBlock)
	} else {
		c.block.NewCondBr(cond, thenBlock, mergeBlock)
	}

	// Then branch
	c.block = thenBlock
	c.genBlock(s.Body)
	if c.block.Term == nil {
		c.block.NewBr(mergeBlock)
	}

	// Else branch
	if s.Else != nil {
		c.block = elseBlock
		c.genStmt(s.Else)
		if c.block.Term == nil {
			c.block.NewBr(mergeBlock)
		}
	}

	c.block = mergeBlock
}

// genIfNarrowStmt handles if-statements that narrow optional variables.
// Supports single narrowing, compound narrowing (&&), and negated narrowing (!cc).
func (c *Compiler) genIfNarrowStmt(s *ast.IfStmt, narrow *sema.OptionalNarrowing) {
	if narrow.Negated {
		c.genNegatedNarrowStmt(s, narrow)
		return
	}
	if len(narrow.Vars) > 1 {
		c.genCompoundNarrowStmt(s, narrow)
		return
	}

	// Single variable narrowing
	v := narrow.Vars[0]
	alloca := c.locals[v.VarName]
	optVal := c.block.NewLoad(alloca.ElemType, alloca)
	flag := c.block.NewExtractValue(optVal, 0)

	thenBlock := c.newBlock("narrow.then")
	mergeBlock := c.newBlock("narrow.end")

	var elseBlock *ir.Block
	if s.Else != nil {
		elseBlock = c.newBlock("narrow.else")
		c.block.NewCondBr(flag, thenBlock, elseBlock)
	} else {
		c.block.NewCondBr(flag, thenBlock, mergeBlock)
	}

	// Then: shadow the variable with the unwrapped inner value
	c.block = thenBlock
	innerVal := c.block.NewExtractValue(optVal, 1)
	innerAlloca := c.block.NewAlloca(innerVal.Type())
	c.block.NewStore(innerVal, innerAlloca)
	prev := c.locals[v.VarName]
	c.locals[v.VarName] = innerAlloca

	c.genBlock(s.Body)
	if c.block.Term == nil {
		c.block.NewBr(mergeBlock)
	}
	c.locals[v.VarName] = prev

	if s.Else != nil {
		c.block = elseBlock
		c.genStmt(s.Else)
		if c.block.Term == nil {
			c.block.NewBr(mergeBlock)
		}
	}

	c.block = mergeBlock
}

// genNegatedNarrowStmt handles `if !cc { A } else { B }` — narrowing in else branch.
func (c *Compiler) genNegatedNarrowStmt(s *ast.IfStmt, narrow *sema.OptionalNarrowing) {
	v := narrow.Vars[0]
	alloca := c.locals[v.VarName]
	optVal := c.block.NewLoad(alloca.ElemType, alloca)
	flag := c.block.NewExtractValue(optVal, 0)

	thenBlock := c.newBlock("narrow.then")
	mergeBlock := c.newBlock("narrow.end")

	var elseBlock *ir.Block
	if s.Else != nil {
		elseBlock = c.newBlock("narrow.else")
	}

	// flag=true (present) → else (narrowed), flag=false (absent) → then (not narrowed)
	if s.Else != nil {
		c.block.NewCondBr(flag, elseBlock, thenBlock)
	} else {
		c.block.NewCondBr(flag, mergeBlock, thenBlock)
	}

	// Then: cc is none — no narrowing
	c.block = thenBlock
	c.genBlock(s.Body)
	if c.block.Term == nil {
		c.block.NewBr(mergeBlock)
	}

	// Else: cc is present — shadow with unwrapped value
	if s.Else != nil {
		c.block = elseBlock
		innerVal := c.block.NewExtractValue(optVal, 1)
		innerAlloca := c.block.NewAlloca(innerVal.Type())
		c.block.NewStore(innerVal, innerAlloca)
		prev := c.locals[v.VarName]
		c.locals[v.VarName] = innerAlloca

		c.genStmt(s.Else)
		if c.block.Term == nil {
			c.block.NewBr(mergeBlock)
		}
		c.locals[v.VarName] = prev
	}

	c.block = mergeBlock
}

// genCompoundNarrowStmt handles `if a && b { ... }` — both narrowed in then-block.
// Generates nested flag checks with short-circuit evaluation.
func (c *Compiler) genCompoundNarrowStmt(s *ast.IfStmt, narrow *sema.OptionalNarrowing) {
	mergeBlock := c.newBlock("narrow.end")
	var elseBlock *ir.Block
	if s.Else != nil {
		elseBlock = c.newBlock("narrow.else")
	}

	// Load all optional values and chain flag checks
	type optInfo struct {
		optVal value.Value
		v      sema.NarrowedVar
	}
	opts := make([]optInfo, len(narrow.Vars))
	for i, v := range narrow.Vars {
		alloca := c.locals[v.VarName]
		optVal := c.block.NewLoad(alloca.ElemType, alloca)
		flag := c.block.NewExtractValue(optVal, 0)
		opts[i] = optInfo{optVal: optVal, v: v}

		if i < len(narrow.Vars)-1 {
			// Not the last: chain to next check
			nextCheck := c.newBlock(fmt.Sprintf("narrow.check.%d", i+1))
			failTarget := elseBlock
			if failTarget == nil {
				failTarget = mergeBlock
			}
			c.block.NewCondBr(flag, nextCheck, failTarget)
			c.block = nextCheck
		} else {
			// Last: branch to then or else/merge
			thenBlock := c.newBlock("narrow.then")
			failTarget := elseBlock
			if failTarget == nil {
				failTarget = mergeBlock
			}
			c.block.NewCondBr(flag, thenBlock, failTarget)
			c.block = thenBlock
		}
	}

	// Then: shadow all variables with unwrapped values
	prevLocals := make(map[string]*ir.InstAlloca, len(opts))
	for _, info := range opts {
		innerVal := c.block.NewExtractValue(info.optVal, 1)
		innerAlloca := c.block.NewAlloca(innerVal.Type())
		c.block.NewStore(innerVal, innerAlloca)
		prevLocals[info.v.VarName] = c.locals[info.v.VarName]
		c.locals[info.v.VarName] = innerAlloca
	}

	c.genBlock(s.Body)
	if c.block.Term == nil {
		c.block.NewBr(mergeBlock)
	}

	// Restore all
	for name, prev := range prevLocals {
		c.locals[name] = prev
	}

	if s.Else != nil {
		c.block = elseBlock
		c.genStmt(s.Else)
		if c.block.Term == nil {
			c.block.NewBr(mergeBlock)
		}
	}

	c.block = mergeBlock
}

// genIfUnwrapStmt handles if-unwrap: if val := optExpr { } else { }
// Evaluates the optional, checks the present flag, binds the unwrapped value in the then block.
func (c *Compiler) genIfUnwrapStmt(s *ast.IfStmt) {
	optVal := c.genExpr(s.Init)

	// Extract flag (field 0 of { i1, T } struct)
	flag := c.block.NewExtractValue(optVal, 0)

	thenBlock := c.newBlock("ifunwrap.then")
	mergeBlock := c.newBlock("ifunwrap.end")

	var elseBlock *ir.Block
	if s.Else != nil {
		elseBlock = c.newBlock("ifunwrap.else")
		c.block.NewCondBr(flag, thenBlock, elseBlock)
	} else {
		c.block.NewCondBr(flag, thenBlock, mergeBlock)
	}

	// Then: unwrap value, bind to local (scoped to then-block only)
	c.block = thenBlock
	innerVal := c.block.NewExtractValue(optVal, 1)
	innerType := innerVal.Type()
	alloca := c.block.NewAlloca(innerType)
	alloca.SetName(c.uniqueLocalName(s.Binding))
	c.block.NewStore(innerVal, alloca)
	prev, hadPrev := c.locals[s.Binding]
	c.locals[s.Binding] = alloca

	c.genBlock(s.Body)
	if c.block.Term == nil {
		c.block.NewBr(mergeBlock)
	}

	// Remove binding from scope (it's only visible in the then-block)
	if hadPrev {
		c.locals[s.Binding] = prev
	} else {
		delete(c.locals, s.Binding)
	}

	// Else (optional)
	if s.Else != nil {
		c.block = elseBlock
		c.genStmt(s.Else)
		if c.block.Term == nil {
			c.block.NewBr(mergeBlock)
		}
	}

	c.block = mergeBlock
}

// --- While loop ---

func (c *Compiler) genWhileStmt(s *ast.WhileStmt) {
	headerBlock := c.newBlock("while.header")
	bodyBlock := c.newBlock("while.body")
	exitBlock := c.newBlock("while.exit")

	c.block.NewBr(headerBlock)

	// Header: evaluate condition
	c.block = headerBlock
	cond := c.genExpr(s.Cond)
	c.block.NewCondBr(cond, bodyBlock, exitBlock)

	// Body
	savedBreak := c.breakTarget
	savedContinue := c.continueTarget
	savedLoopUseDepth := c.loopScopeDepth
	c.breakTarget = exitBlock
	c.continueTarget = headerBlock
	c.loopScopeDepth = len(c.scopeBindings)

	c.block = bodyBlock
	c.genBlock(s.Body)
	if c.block.Term == nil {
		c.block.NewBr(headerBlock)
	}

	c.breakTarget = savedBreak
	c.continueTarget = savedContinue
	c.loopScopeDepth = savedLoopUseDepth
	c.block = exitBlock
}

// genWhileUnwrapStmt handles while-unwrap: while val := optExpr { }
// Each iteration evaluates the optional; loop continues while present.
func (c *Compiler) genWhileUnwrapStmt(s *ast.WhileUnwrapStmt) {
	headerBlock := c.newBlock("whileunwrap.header")
	bodyBlock := c.newBlock("whileunwrap.body")
	exitBlock := c.newBlock("whileunwrap.exit")

	c.block.NewBr(headerBlock)

	// Header: evaluate optional, check flag
	c.block = headerBlock
	optVal := c.genExpr(s.Value)
	flag := c.block.NewExtractValue(optVal, 0)
	c.block.NewCondBr(flag, bodyBlock, exitBlock)

	// Body: unwrap value, bind to local
	savedBreak := c.breakTarget
	savedContinue := c.continueTarget
	savedLoopUseDepth := c.loopScopeDepth
	c.breakTarget = exitBlock
	c.continueTarget = headerBlock
	c.loopScopeDepth = len(c.scopeBindings)

	c.block = bodyBlock
	innerVal := c.block.NewExtractValue(optVal, 1)
	innerType := innerVal.Type()
	alloca := c.block.NewAlloca(innerType)
	alloca.SetName(c.uniqueLocalName(s.Binding))
	c.block.NewStore(innerVal, alloca)
	prev, hadPrev := c.locals[s.Binding]
	c.locals[s.Binding] = alloca

	c.genBlock(s.Body)
	if c.block.Term == nil {
		c.block.NewBr(headerBlock)
	}

	// Remove binding from scope (it's only visible in the loop body)
	if hadPrev {
		c.locals[s.Binding] = prev
	} else {
		delete(c.locals, s.Binding)
	}

	c.breakTarget = savedBreak
	c.continueTarget = savedContinue
	c.loopScopeDepth = savedLoopUseDepth
	c.block = exitBlock
}

// --- For-in loop ---

func (c *Compiler) genForInStmt(s *ast.ForInStmt) {
	iterableType := c.info.Types[s.Iterable]
	if c.typeSubst != nil {
		iterableType = types.Substitute(iterableType, c.typeSubst)
	}

	if elem, ok := types.AsVector(iterableType); ok {
		slicePtr := c.genExpr(s.Iterable)
		c.genForInVector(s, slicePtr, elem)
	} else if key, val, ok := types.AsMap(iterableType); ok {
		mapPtr := c.genExpr(s.Iterable)
		c.genForInMap(s, mapPtr, key, val)
	} else if elem, ok := types.AsChannel(iterableType); ok {
		chPtr := c.genExpr(s.Iterable)
		c.genForInChannel(s, chPtr, elem)
	} else {
		// String iteration
		named := extractNamed(iterableType)
		if named == types.TypString {
			strPtr := c.genExpr(s.Iterable)
			c.genForInString(s, strPtr)
			return
		}
		// range iteration (existing behavior)
		c.genForInRange(s)
	}
}

// genForInRange handles for-in over a range (e.g., 0..10).
func (c *Compiler) genForInRange(s *ast.ForInStmt) {
	rangeVal := c.genExpr(s.Iterable)

	rangeType := c.rangeStructType()
	rangeAlloca := c.block.NewAlloca(rangeType)
	c.block.NewStore(rangeVal, rangeAlloca)

	startPtr := c.block.NewGetElementPtr(rangeType, rangeAlloca,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
	start := c.block.NewLoad(irtypes.I64, startPtr)

	endPtr := c.block.NewGetElementPtr(rangeType, rangeAlloca,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 1))
	end := c.block.NewLoad(irtypes.I64, endPtr)

	inclPtr := c.block.NewGetElementPtr(rangeType, rangeAlloca,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 2))
	inclusive := c.block.NewLoad(irtypes.I1, inclPtr)

	counterAlloca := c.block.NewAlloca(irtypes.I64)
	counterAlloca.SetName(c.uniqueLocalName(s.Binding))
	c.block.NewStore(start, counterAlloca)
	c.locals[s.Binding] = counterAlloca

	if s.Index != "" {
		indexAlloca := c.block.NewAlloca(irtypes.I64)
		indexAlloca.SetName(c.uniqueLocalName(s.Index))
		c.block.NewStore(constant.NewInt(irtypes.I64, 0), indexAlloca)
		c.locals[s.Index] = indexAlloca
	}

	headerBlock := c.newBlock("forin.header")
	bodyBlock := c.newBlock("forin.body")
	updateBlock := c.newBlock("forin.update")
	exitBlock := c.newBlock("forin.exit")

	c.block.NewBr(headerBlock)

	c.block = headerBlock
	counter := c.block.NewLoad(irtypes.I64, counterAlloca)
	ltCond := c.block.NewICmp(enum.IPredSLT, counter, end)
	eqCond := c.block.NewICmp(enum.IPredEQ, counter, end)
	inclAndEq := c.block.NewAnd(inclusive, eqCond)
	cond := c.block.NewOr(ltCond, inclAndEq)
	c.block.NewCondBr(cond, bodyBlock, exitBlock)

	savedBreak := c.breakTarget
	savedContinue := c.continueTarget
	savedLoopUseDepth := c.loopScopeDepth
	c.breakTarget = exitBlock
	c.continueTarget = updateBlock
	c.loopScopeDepth = len(c.scopeBindings)

	c.block = bodyBlock
	c.genBlock(s.Body)
	if c.block.Term == nil {
		c.block.NewBr(updateBlock)
	}

	c.block = updateBlock
	cur := c.block.NewLoad(irtypes.I64, counterAlloca)
	next := c.block.NewAdd(cur, constant.NewInt(irtypes.I64, 1))
	c.block.NewStore(next, counterAlloca)

	if s.Index != "" {
		idxAlloca := c.locals[s.Index]
		curIdx := c.block.NewLoad(irtypes.I64, idxAlloca)
		nextIdx := c.block.NewAdd(curIdx, constant.NewInt(irtypes.I64, 1))
		c.block.NewStore(nextIdx, idxAlloca)
	}

	c.emitYieldCheck()
	c.block.NewBr(headerBlock)

	c.breakTarget = savedBreak
	c.continueTarget = savedContinue
	c.loopScopeDepth = savedLoopUseDepth
	c.block = exitBlock
}

// --- Classic for loop ---

func (c *Compiler) genClassicForStmt(s *ast.ClassicForStmt) {
	// Init: declare the loop variable
	if s.InitValue != nil {
		typ := c.info.Types[s.InitValue]
		lt := c.resolveType(typ)
		alloca := c.block.NewAlloca(lt)
		alloca.SetName(c.uniqueLocalName(s.InitName))
		val := c.genExpr(s.InitValue)
		c.block.NewStore(val, alloca)
		c.locals[s.InitName] = alloca
	}

	headerBlock := c.newBlock("for.header")
	bodyBlock := c.newBlock("for.body")
	updateBlock := c.newBlock("for.update")
	exitBlock := c.newBlock("for.exit")

	c.block.NewBr(headerBlock)

	// Header: evaluate condition
	c.block = headerBlock
	cond := c.genExpr(s.Cond)
	c.block.NewCondBr(cond, bodyBlock, exitBlock)

	// Body
	savedBreak := c.breakTarget
	savedContinue := c.continueTarget
	savedLoopUseDepth := c.loopScopeDepth
	c.breakTarget = exitBlock
	c.continueTarget = updateBlock
	c.loopScopeDepth = len(c.scopeBindings)

	c.block = bodyBlock
	c.genBlock(s.Body)
	if c.block.Term == nil {
		c.block.NewBr(updateBlock)
	}

	// Update
	c.block = updateBlock
	if s.UpdateIncDec {
		// Inc/dec update: target++ or target--
		c.genIncDecTarget(s.UpdateTarget, s.UpdateIsInc)
	} else if s.UpdateTarget != nil {
		// Compound update: target op= value
		updateVal := c.genExpr(s.UpdateValue)
		ident, ok := s.UpdateTarget.(*ast.IdentExpr)
		if ok {
			alloca, ok := c.locals[ident.Name]
			if ok {
				if s.UpdateOp == ast.OpAssign {
					c.block.NewStore(updateVal, alloca)
				} else {
					current := c.block.NewLoad(alloca.ElemType, alloca)
					result := c.genCompoundOp(s.UpdateOp, current, updateVal)
					c.block.NewStore(result, alloca)
				}
			}
		}
	} else if s.UpdateValue != nil {
		// Expression-only update
		c.genExpr(s.UpdateValue)
	}
	c.emitYieldCheck()
	c.block.NewBr(headerBlock)

	c.breakTarget = savedBreak
	c.continueTarget = savedContinue
	c.loopScopeDepth = savedLoopUseDepth
	c.block = exitBlock
}

// --- Infinite loop ---

func (c *Compiler) genInfiniteLoop(s *ast.InfiniteLoop) {
	bodyBlock := c.newBlock("loop.body")
	exitBlock := c.newBlock("loop.exit")

	c.block.NewBr(bodyBlock)

	savedBreak := c.breakTarget
	savedContinue := c.continueTarget
	savedLoopUseDepth := c.loopScopeDepth
	c.breakTarget = exitBlock
	c.continueTarget = bodyBlock
	c.loopScopeDepth = len(c.scopeBindings)

	c.block = bodyBlock
	c.genBlock(s.Body)
	if c.block != nil && c.block.Term == nil {
		c.emitYieldCheck()
		c.block.NewBr(bodyBlock)
	}

	c.breakTarget = savedBreak
	c.continueTarget = savedContinue
	c.loopScopeDepth = savedLoopUseDepth
	c.block = exitBlock
}

// --- Cooperative preemption yield check ---

// emitYieldCheck emits an inline cooperative yield check at loop back-edges.
// Only active when c.inCoroutine is true (inside goroutine/main coroutine).
// Checks G.preempt flag set by sysmon; if set, clears it, re-enqueues self,
// and calls coro.suspend to yield to the scheduler.
func (c *Compiler) emitYieldCheck() {
	if !c.inCoroutine {
		return
	}
	if c.block == nil || c.block.Term != nil {
		return
	}

	gTy := goroutineStructType()

	// Load current G
	curG := c.block.NewLoad(irtypes.I8Ptr, c.currentGGlobal)
	gPtr := c.block.NewBitCast(curG, irtypes.NewPointer(gTy))

	// Load G.preempt
	preemptField := c.block.NewGetElementPtr(gTy, gPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(gFieldPreempt)))
	preemptVal := c.block.NewLoad(irtypes.I8, preemptField)
	shouldYield := c.block.NewICmp(enum.IPredNE, preemptVal, constant.NewInt(irtypes.I8, 0))

	yieldBlk := c.newBlock("yield")
	continueBlk := c.newBlock("yield.cont")
	c.block.NewCondBr(shouldYield, yieldBlk, continueBlk)

	// yield: clear preempt, set status=runnable, enqueue self, coro.suspend
	c.block = yieldBlk
	c.block.NewStore(constant.NewInt(irtypes.I8, 0), preemptField)

	// Re-enqueue self so we get scheduled again
	c.block.NewCall(c.funcs["promise_sched_enqueue"], curG)

	// Suspend — scheduler will resume us later
	suspResult := c.block.NewCall(c.coroSuspend, constant.None, constant.False)
	c.block.NewSwitch(suspResult, c.coroSuspendBlk,
		ir.NewCase(constant.NewInt(irtypes.I8, 0), continueBlk),
		ir.NewCase(constant.NewInt(irtypes.I8, 1), c.coroCleanupBlk))

	// yield.cont: continue with the loop
	c.block = continueBlk
}

// --- Break / Continue ---

func (c *Compiler) genBreakStmt() {
	if c.breakTarget != nil {
		// Close use bindings added within the loop body
		if len(c.scopeBindings) > c.loopScopeDepth {
			c.emitScopeCleanup(c.loopScopeDepth)
		}
		c.block.NewBr(c.breakTarget)
	}
}

func (c *Compiler) genContinueStmt() {
	if c.continueTarget != nil {
		// Close use bindings added within the loop body
		if len(c.scopeBindings) > c.loopScopeDepth {
			c.emitScopeCleanup(c.loopScopeDepth)
		}
		c.block.NewBr(c.continueTarget)
	}
}

// --- Index assignment ---

// genIndexAssign handles assignment to a container element: arr[i] = val, m[k] = val.
func (c *Compiler) genIndexAssign(target *ast.IndexExpr, op ast.AssignOp, val value.Value) {
	targetType := c.info.Types[target.Target]
	if c.typeSubst != nil {
		targetType = types.Substitute(targetType, c.typeSubst)
	}

	named := extractNamed(targetType)
	if named != nil {
		if m := named.LookupMethod("[]="); m != nil {
			if m.IsNative() {
				c.genNativeIndexAssign(target, targetType, op, val)
				return
			}
			c.genMethodIndexAssign(target, targetType, val)
			return
		}
	}
	panic(fmt.Sprintf("codegen: cannot assign to index of type %s", targetType))
}

// genNativeIndexAssign dispatches native []= implementations for built-in types.
func (c *Compiler) genNativeIndexAssign(target *ast.IndexExpr, targetType types.Type, op ast.AssignOp, val value.Value) {
	if elem, ok := types.AsVector(targetType); ok {
		c.genVectorIndexAssign(target, elem, op, val)
		return
	}
	// Inside monomorphized method body: targetType is Named(Vector) not Instance(Vector[T]).
	named := extractNamed(targetType)
	if named == types.TypVector && c.typeSubst != nil {
		tp := named.TypeParams()[0]
		if elem, ok := c.typeSubst[tp]; ok {
			c.genVectorIndexAssign(target, elem, op, val)
			return
		}
	}
	panic(fmt.Sprintf("codegen: no native []= implementation for type %s", targetType))
}

// genMethodIndexAssign calls the monomorphized []= method on a user type.
func (c *Compiler) genMethodIndexAssign(target *ast.IndexExpr, targetType types.Type, val value.Value) {
	mangledName := mangleMethodName(c.resolveTypeName(targetType), "[]=", false)
	fn, ok := c.funcs[mangledName]
	if !ok {
		panic(fmt.Sprintf("codegen: undeclared []= method %s", mangledName))
	}

	targetVal := c.genExpr(target.Target)
	keyVal := c.genExpr(target.Index)

	var instancePtr value.Value
	if isContainerType(targetType) {
		instancePtr = targetVal
	} else {
		instancePtr = c.extractInstancePtr(targetVal)
	}

	c.block.NewCall(fn, instancePtr, keyVal, val)
}

// genVectorIndexAssign handles vec[i] = val with bounds check.
func (c *Compiler) genVectorIndexAssign(target *ast.IndexExpr, elemType types.Type, op ast.AssignOp, val value.Value) {
	slicePtr := c.genExpr(target.Target)
	idx := c.genExpr(target.Index)
	elemLLVM := c.resolveType(elemType)

	// Bounds check
	headerType := vectorHeaderType()
	headerPtr := c.block.NewBitCast(slicePtr, irtypes.NewPointer(headerType))
	lenPtr := c.block.NewGetElementPtr(headerType, headerPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
	length := c.block.NewLoad(irtypes.I64, lenPtr)

	inBounds := c.block.NewICmp(enum.IPredULT, idx, length)
	okBlock := c.newBlock("indexassign.ok")
	panicBlock := c.newBlock("indexassign.oob")
	c.block.NewCondBr(inBounds, okBlock, panicBlock)

	c.block = panicBlock
	oobMsg := c.makeGlobalString("index out of bounds")
	c.block.NewCall(c.funcs["promise_panic"], oobMsg)
	c.block.NewUnreachable()

	c.block = okBlock
	dataBase := c.block.NewGetElementPtr(irtypes.I8, slicePtr,
		constant.NewInt(irtypes.I64, int64(vectorHeaderSize)))
	dataTypedPtr := c.block.NewBitCast(dataBase, irtypes.NewPointer(elemLLVM))
	elemPtr := c.block.NewGetElementPtr(elemLLVM, dataTypedPtr, idx)

	if op == ast.OpAssign {
		c.block.NewStore(val, elemPtr)
		return
	}

	// Compound assignment
	current := c.block.NewLoad(elemLLVM, elemPtr)
	result := c.genCompoundOp(op, current, val)
	c.block.NewStore(result, elemPtr)
}

// genCompoundIndexAssign handles compound index assignments (arr[i] += val, m[k] += val)
// with correct evaluation order: target → key → RHS.
func (c *Compiler) genCompoundIndexAssign(target *ast.IndexExpr, op ast.AssignOp, valueExpr ast.Expr) {
	targetType := c.info.Types[target.Target]
	if c.typeSubst != nil {
		targetType = types.Substitute(targetType, c.typeSubst)
	}

	named := extractNamed(targetType)
	if named != nil {
		if m := named.LookupMethod("[]="); m != nil {
			if m.IsNative() {
				// Native compound assign (vectors)
				elem, ok := types.AsVector(targetType)
				if !ok && named == types.TypVector && c.typeSubst != nil {
					tp := named.TypeParams()[0]
					elem, ok = c.typeSubst[tp], c.typeSubst[tp] != nil
				}
				if ok {
					slicePtr := c.genExpr(target.Target)
					idx := c.genExpr(target.Index)
					val := c.genExpr(valueExpr)
					c.genVectorCompoundAssign(slicePtr, idx, elem, op, val)
					return
				}
			} else {
				// Non-native: read via [], apply op, write via []=
				c.genMethodCompoundAssign(target, targetType, op, valueExpr)
				return
			}
		}
	}
	panic(fmt.Sprintf("codegen: cannot compound-assign to index of type %s", targetType))
}

// genMethodCompoundAssign handles compound assignment (e.g. m[k] += v) on non-native types
// by calling [] to read, applying the operator, then calling []= to write.
func (c *Compiler) genMethodCompoundAssign(target *ast.IndexExpr, targetType types.Type, op ast.AssignOp, valueExpr ast.Expr) {
	typeName := c.resolveTypeName(targetType)

	getFnName := mangleMethodName(typeName, "[]", false)
	getFn, ok := c.funcs[getFnName]
	if !ok {
		panic(fmt.Sprintf("codegen: undeclared [] method %s", getFnName))
	}
	setFnName := mangleMethodName(typeName, "[]=", false)
	setFn, ok := c.funcs[setFnName]
	if !ok {
		panic(fmt.Sprintf("codegen: undeclared []= method %s", setFnName))
	}

	targetVal := c.genExpr(target.Target)
	keyVal := c.genExpr(target.Index)
	val := c.genExpr(valueExpr)

	var instancePtr value.Value
	if isContainerType(targetType) {
		instancePtr = targetVal
	} else {
		instancePtr = c.extractInstancePtr(targetVal)
	}

	// Call [] to get current value (returns V? for maps)
	optVal := c.block.NewCall(getFn, instancePtr, keyVal)

	// Check has_value flag (field 0 of optional struct)
	hasVal := c.block.NewExtractValue(optVal, 0)
	okBlock := c.newBlock("mapcomp.ok")
	panicBlock := c.newBlock("mapcomp.panic")
	c.block.NewCondBr(hasVal, okBlock, panicBlock)

	c.block = panicBlock
	panicMsg := c.makeGlobalString("compound assignment on missing key")
	c.block.NewCall(c.funcs["promise_panic"], panicMsg)
	c.block.NewUnreachable()

	c.block = okBlock
	current := c.block.NewExtractValue(optVal, 1)
	result := c.genCompoundOp(op, current, val)

	c.block.NewCall(setFn, instancePtr, keyVal, result)
}

// genVectorCompoundAssign handles vec[i] += val with bounds check and pre-evaluated operands.
func (c *Compiler) genVectorCompoundAssign(slicePtr, idx value.Value, elemType types.Type, op ast.AssignOp, val value.Value) {
	elemLLVM := c.resolveType(elemType)

	headerType := vectorHeaderType()
	headerPtr := c.block.NewBitCast(slicePtr, irtypes.NewPointer(headerType))
	lenPtr := c.block.NewGetElementPtr(headerType, headerPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
	length := c.block.NewLoad(irtypes.I64, lenPtr)

	inBounds := c.block.NewICmp(enum.IPredULT, idx, length)
	okBlock := c.newBlock("slicecomp.ok")
	panicBlock := c.newBlock("slicecomp.oob")
	c.block.NewCondBr(inBounds, okBlock, panicBlock)

	c.block = panicBlock
	oobMsg := c.makeGlobalString("index out of bounds")
	c.block.NewCall(c.funcs["promise_panic"], oobMsg)
	c.block.NewUnreachable()

	c.block = okBlock
	dataBase := c.block.NewGetElementPtr(irtypes.I8, slicePtr,
		constant.NewInt(irtypes.I64, int64(vectorHeaderSize)))
	dataTypedPtr := c.block.NewBitCast(dataBase, irtypes.NewPointer(elemLLVM))
	elemPtr := c.block.NewGetElementPtr(elemLLVM, dataTypedPtr, idx)

	current := c.block.NewLoad(elemLLVM, elemPtr)
	result := c.genCompoundOp(op, current, val)
	c.block.NewStore(result, elemPtr)
}

// --- Slice assignment ---

// genSliceAssign handles assignment to a slice target: v[a:b] = val.
func (c *Compiler) genSliceAssign(target *ast.SliceExpr, val value.Value) {
	targetType := c.info.Types[target.Target]
	if c.typeSubst != nil {
		targetType = types.Substitute(targetType, c.typeSubst)
	}

	named := extractNamed(targetType)
	if named == nil {
		panic(fmt.Sprintf("codegen: cannot slice-assign to type %s", targetType))
	}
	m := named.LookupMethod("[:]=")
	if m == nil {
		panic(fmt.Sprintf("codegen: no [:]=  method on type %s", named))
	}

	targetVal := c.genExpr(target.Target)

	// Generate optional int arguments for low and high bounds
	optIntType := irtypes.NewStruct(irtypes.I1, irtypes.I64)
	low := c.genSliceBound(target.Low, optIntType)
	high := c.genSliceBound(target.High, optIntType)

	mangledName := mangleMethodName(c.resolveTypeName(targetType), "[:]=", false)
	fn, ok := c.funcs[mangledName]
	if !ok {
		panic(fmt.Sprintf("codegen: undeclared [:]=  method %s", mangledName))
	}

	var instancePtr value.Value
	if isContainerType(targetType) {
		instancePtr = targetVal
	} else {
		instancePtr = c.extractInstancePtr(targetVal)
	}

	c.block.NewCall(fn, instancePtr, low, high, val)
}

// --- lookupLocalType resolves the declared type for a TypedVarDecl ---
// It checks the TypeRef AST node to detect Optional declarations,
// then resolves the type by looking up the variable in sema scopes.

func (c *Compiler) lookupLocalType(s *ast.TypedVarDecl) types.Type {
	// Only need special handling for Optional declarations
	optRef, ok := s.Type.(*ast.OptionalTypeRef)
	if !ok {
		return nil // use expression type
	}

	exprType := c.info.Types[s.Value]
	if c.typeSubst != nil && exprType != nil {
		exprType = types.Substitute(exprType, c.typeSubst)
	}

	// If value is NoneLit, resolve the inner type from the AST OptionalTypeRef
	if exprType == types.TypNone || exprType == nil {
		innerType := c.resolveTypeRefToType(optRef.Inner)
		if innerType != nil {
			return types.NewOptional(innerType)
		}
		// Fallback: search sema scopes
		return c.lookupVarType(s.Name)
	}

	// Value has a concrete type — wrap in Optional
	if _, isOpt := exprType.(*types.Optional); isOpt {
		return exprType // already Optional
	}
	return types.NewOptional(exprType)
}

// resolveTypeRefToType resolves an AST TypeRef to a types.Type.
// Handles named types (primitives and user types) by looking up in Universe and sema scopes.
func (c *Compiler) resolveTypeRefToType(ref ast.TypeRef) types.Type {
	switch r := ref.(type) {
	case *ast.NamedTypeRef:
		// Check Universe scope first (primitives)
		if obj, _ := types.Universe.LookupParent(r.Name); obj != nil {
			if tn, ok := obj.(*types.TypeName); ok {
				return tn.Type()
			}
		}
		// Check sema scopes (user-defined types)
		for _, scope := range c.info.Scopes {
			if obj := scope.Lookup(r.Name); obj != nil {
				if tn, ok := obj.(*types.TypeName); ok {
					return tn.Type()
				}
			}
		}
	case *ast.OptionalTypeRef:
		inner := c.resolveTypeRefToType(r.Inner)
		if inner != nil {
			return types.NewOptional(inner)
		}
	}
	return nil
}

// lookupVarType finds a variable's declared type by walking sema scopes.
func (c *Compiler) lookupVarType(name string) types.Type {
	for _, scope := range c.info.Scopes {
		if obj := scope.Lookup(name); obj != nil {
			if v, ok := obj.(*types.Var); ok {
				typ := v.Type()
				if c.typeSubst != nil {
					typ = types.Substitute(typ, c.typeSubst)
				}
				return typ
			}
		}
	}
	return nil
}

// uniqueLocalName returns a unique LLVM name for a local variable alloca.
// On first use of a name within a function, returns it unchanged.
// On subsequent uses (shadowing in inner scopes), appends a numeric suffix.
func (c *Compiler) uniqueLocalName(name string) string {
	n := c.localNameCount[name]
	c.localNameCount[name] = n + 1
	if n == 0 {
		return name
	}
	return fmt.Sprintf("%s.%d", name, n)
}

// --- For-in over vectors ---

func (c *Compiler) genForInVector(s *ast.ForInStmt, slicePtr value.Value, elemType types.Type) {
	elemLLVM := c.resolveType(elemType)

	// Load length from header
	headerType := vectorHeaderType()
	headerPtr := c.block.NewBitCast(slicePtr, irtypes.NewPointer(headerType))
	lenPtr := c.block.NewGetElementPtr(headerType, headerPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
	length := c.block.NewLoad(irtypes.I64, lenPtr)

	// Counter alloca
	counterAlloca := c.block.NewAlloca(irtypes.I64)
	c.block.NewStore(constant.NewInt(irtypes.I64, 0), counterAlloca)

	// Element binding alloca
	elemAlloca := c.block.NewAlloca(elemLLVM)
	elemAlloca.SetName(c.uniqueLocalName(s.Binding))
	c.locals[s.Binding] = elemAlloca

	// Index variable if present
	if s.Index != "" {
		indexAlloca := c.block.NewAlloca(irtypes.I64)
		indexAlloca.SetName(c.uniqueLocalName(s.Index))
		c.block.NewStore(constant.NewInt(irtypes.I64, 0), indexAlloca)
		c.locals[s.Index] = indexAlloca
	}

	headerBlock := c.newBlock("forin.header")
	bodyBlock := c.newBlock("forin.body")
	updateBlock := c.newBlock("forin.update")
	exitBlock := c.newBlock("forin.exit")

	c.block.NewBr(headerBlock)

	// Header: counter < length
	c.block = headerBlock
	counter := c.block.NewLoad(irtypes.I64, counterAlloca)
	cond := c.block.NewICmp(enum.IPredULT, counter, length)
	c.block.NewCondBr(cond, bodyBlock, exitBlock)

	// Body: load element, store to binding
	savedBreak := c.breakTarget
	savedContinue := c.continueTarget
	savedLoopUseDepth := c.loopScopeDepth
	c.breakTarget = exitBlock
	c.continueTarget = updateBlock
	c.loopScopeDepth = len(c.scopeBindings)

	c.block = bodyBlock
	dataBase := c.block.NewGetElementPtr(irtypes.I8, slicePtr,
		constant.NewInt(irtypes.I64, int64(vectorHeaderSize)))
	dataTypedPtr := c.block.NewBitCast(dataBase, irtypes.NewPointer(elemLLVM))
	curCounter := c.block.NewLoad(irtypes.I64, counterAlloca)
	elemPtr := c.block.NewGetElementPtr(elemLLVM, dataTypedPtr, curCounter)
	elem := c.block.NewLoad(elemLLVM, elemPtr)
	c.block.NewStore(elem, elemAlloca)

	c.genBlock(s.Body)
	if c.block.Term == nil {
		c.block.NewBr(updateBlock)
	}

	// Update: increment counter (and index if present)
	c.block = updateBlock
	cur := c.block.NewLoad(irtypes.I64, counterAlloca)
	next := c.block.NewAdd(cur, constant.NewInt(irtypes.I64, 1))
	c.block.NewStore(next, counterAlloca)

	if s.Index != "" {
		idxAlloca := c.locals[s.Index]
		curIdx := c.block.NewLoad(irtypes.I64, idxAlloca)
		nextIdx := c.block.NewAdd(curIdx, constant.NewInt(irtypes.I64, 1))
		c.block.NewStore(nextIdx, idxAlloca)
	}

	c.emitYieldCheck()
	c.block.NewBr(headerBlock)

	c.breakTarget = savedBreak
	c.continueTarget = savedContinue
	c.loopScopeDepth = savedLoopUseDepth
	c.block = exitBlock
}

// --- For-in over channels ---

// genForInChannel loops receiving from a channel until it returns none (closed+empty).
// for v in ch { ... }  ≡  loop { val := <-ch; if val is none: break; v := unwrap(val); ... }
func (c *Compiler) genForInChannel(s *ast.ForInStmt, chRaw value.Value, elemType types.Type) {
	elemLLVM := c.resolveType(elemType)
	elemSize := int64(llvmTypeSize(elemLLVM))

	chanType := channelStructType()
	chPtr := c.block.NewBitCast(chRaw, irtypes.NewPointer(chanType))

	// Element binding alloca
	elemAlloca := c.block.NewAlloca(elemLLVM)
	elemAlloca.SetName(c.uniqueLocalName(s.Binding))
	c.locals[s.Binding] = elemAlloca

	headerBlock := c.newBlock("forin_ch.header")
	recvWaitBlock := c.newBlock("forin_ch.recv.wait")
	recvCheckBlock := c.newBlock("forin_ch.recv.check")
	recvNoneBlock := c.newBlock("forin_ch.recv.none")
	recvReadBlock := c.newBlock("forin_ch.recv.read")
	bodyBlock := c.newBlock("forin_ch.body")
	exitBlock := c.newBlock("forin_ch.exit")

	c.block.NewBr(headerBlock)

	// header: lock mutex, then enter receive wait loop
	c.block = headerBlock
	mtxFieldPtr := c.block.NewGetElementPtr(chanType, chPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(chanFieldMutex)))
	mtx := c.block.NewLoad(irtypes.I8Ptr, mtxFieldPtr)
	neFieldPtr := c.block.NewGetElementPtr(chanType, chPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(chanFieldNotEmpty)))
	notEmpty := c.block.NewLoad(irtypes.I8Ptr, neFieldPtr)
	nfFieldPtr := c.block.NewGetElementPtr(chanType, chPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(chanFieldNotFull)))
	notFull := c.block.NewLoad(irtypes.I8Ptr, nfFieldPtr)

	c.block.NewCall(c.palMutexLock, mtx)
	c.block.NewBr(recvWaitBlock)

	// recv.wait: while count==0 && !closed → wait
	c.block = recvWaitBlock
	countPtr := c.block.NewGetElementPtr(chanType, chPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(chanFieldCount)))
	count := c.block.NewLoad(irtypes.I64, countPtr)
	isEmpty := c.block.NewICmp(enum.IPredEQ, count, constant.NewInt(irtypes.I64, 0))
	closedPtr := c.block.NewGetElementPtr(chanType, chPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(chanFieldClosed)))
	closedVal := c.block.NewLoad(irtypes.I8, closedPtr)
	isOpen := c.block.NewICmp(enum.IPredEQ, closedVal, constant.NewInt(irtypes.I8, 0))
	shouldWait := c.block.NewAnd(isEmpty, isOpen)

	recvWaitBodyBlock := c.newBlock("forin_ch.recv.wait.body")
	c.block.NewCondBr(shouldWait, recvWaitBodyBlock, recvCheckBlock)

	if c.inCoroutine {
		// Goroutine mode: park on recv_waiters + coro.suspend
		c.block = recvWaitBodyBlock
		currentG := c.block.NewLoad(irtypes.I8Ptr, c.currentGGlobal)
		recvHeadPtr := c.block.NewGetElementPtr(chanType, chPtr,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(chanFieldRecvWaitersHead)))
		recvTailPtr := c.block.NewGetElementPtr(chanType, chPtr,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(chanFieldRecvWaitersTail)))
		c.block.NewCall(c.funcs["promise_waiter_enqueue"], recvHeadPtr, recvTailPtr, currentG)
		// Store mutex in G.park_mutex — scheduler releases after coro.suspend completes
		gTyForIn := goroutineStructType()
		forInGPtr := c.block.NewBitCast(currentG, irtypes.NewPointer(gTyForIn))
		forInPmField := c.block.NewGetElementPtr(gTyForIn, forInGPtr,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(gFieldParkMutex)))
		c.block.NewStore(mtx, forInPmField)

		suspResult := c.block.NewCall(c.coroSuspend, constant.None, constant.False)
		resumeBlk := c.newBlock("forin_ch.recv.resume")
		c.block.NewSwitch(suspResult, c.coroSuspendBlk,
			ir.NewCase(constant.NewInt(irtypes.I8, 0), resumeBlk),
			ir.NewCase(constant.NewInt(irtypes.I8, 1), c.coroCleanupBlk))

		c.block = resumeBlk
		c.block.NewCall(c.palMutexLock, mtx)
		c.block.NewBr(recvWaitBlock)
	} else {
		// Thread-blocking mode: cond_wait, loop
		c.block = recvWaitBodyBlock
		c.block.NewCall(c.palCondWait, notEmpty, mtx)
		c.block.NewBr(recvWaitBlock)
	}

	// recv.check: if empty → exit (channel closed), else → read
	c.block = recvCheckBlock
	countAgain := c.block.NewLoad(irtypes.I64, countPtr)
	stillEmpty := c.block.NewICmp(enum.IPredEQ, countAgain, constant.NewInt(irtypes.I64, 0))
	c.block.NewCondBr(stillEmpty, recvNoneBlock, recvReadBlock)

	// recv.none: unlock and exit loop
	c.block = recvNoneBlock
	c.block.NewCall(c.palMutexUnlock, mtx)
	c.block.NewBr(exitBlock)

	// recv.read: read value from buffer, advance head, count--, wake sender, unlock, enter body
	c.block = recvReadBlock
	bufPtr := c.block.NewGetElementPtr(chanType, chPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(chanFieldBuffer)))
	buf := c.block.NewLoad(irtypes.I8Ptr, bufPtr)
	headPtr := c.block.NewGetElementPtr(chanType, chPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(chanFieldHead)))
	head := c.block.NewLoad(irtypes.I64, headPtr)
	offset := c.block.NewMul(head, constant.NewInt(irtypes.I64, elemSize))
	src := c.block.NewGetElementPtr(irtypes.I8, buf, offset)

	resultAsI8 := c.block.NewBitCast(elemAlloca, irtypes.I8Ptr)
	c.block.NewCall(c.funcs["llvm.memcpy"], resultAsI8, src,
		constant.NewInt(irtypes.I64, elemSize), constant.False)

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

	// Wake a waiting sender: try goroutine waiter first, then cond_signal
	sendHeadPtr := c.block.NewGetElementPtr(chanType, chPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(chanFieldSendWaitersHead)))
	sendTailPtr := c.block.NewGetElementPtr(chanType, chPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(chanFieldSendWaitersTail)))
	sendWaiter := c.block.NewCall(c.funcs["promise_waiter_dequeue"], sendHeadPtr, sendTailPtr)
	hasSendWaiter := c.block.NewICmp(enum.IPredNE, sendWaiter, constant.NewNull(irtypes.I8Ptr))

	wakeSendBlk := c.newBlock("forin_ch.wake.send")
	signalSendBlk := c.newBlock("forin_ch.signal.send")
	afterSignalBlk := c.newBlock("forin_ch.after.signal")
	c.block.NewCondBr(hasSendWaiter, wakeSendBlk, signalSendBlk)

	// Wake parked sender goroutine
	c.block = wakeSendBlk
	gTy := goroutineStructType()
	gPtrTy := irtypes.NewPointer(gTy)
	senderTyped := c.block.NewBitCast(sendWaiter, gPtrTy)
	senderStatusPtr := c.block.NewGetElementPtr(gTy, senderTyped,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(gFieldStatus)))
	c.block.NewStore(constant.NewInt(irtypes.I8, gStatusRunnable), senderStatusPtr)
	c.block.NewCall(c.funcs["promise_sched_enqueue"], sendWaiter)
	c.block.NewBr(afterSignalBlk)

	// Fallback: signal cond var for thread-blocked senders
	c.block = signalSendBlk
	c.block.NewCall(c.palCondSignal, notFull)
	c.block.NewBr(afterSignalBlk)

	c.block = afterSignalBlk

	// Unlock
	c.block.NewCall(c.palMutexUnlock, mtx)

	// Fall into body
	c.block.NewBr(bodyBlock)

	// body: execute loop body
	savedBreak := c.breakTarget
	savedContinue := c.continueTarget
	savedLoopUseDepth := c.loopScopeDepth
	c.breakTarget = exitBlock
	c.continueTarget = headerBlock
	c.loopScopeDepth = len(c.scopeBindings)

	c.block = bodyBlock
	c.genBlock(s.Body)
	if c.block.Term == nil {
		c.emitYieldCheck()
		c.block.NewBr(headerBlock)
	}

	c.breakTarget = savedBreak
	c.continueTarget = savedContinue
	c.loopScopeDepth = savedLoopUseDepth
	c.block = exitBlock
}

// --- For-in over maps ---

// genForInMap iterates a Promise-implemented map by calling keys() and values()
// to produce vectors, then looping over them in parallel.
func (c *Compiler) genForInMap(s *ast.ForInStmt, mapVal value.Value, keyType, valType types.Type) {
	keyLLVM := c.resolveType(keyType)
	valLLVM := c.resolveType(valType)

	// Resolve monomorphized type name for method lookup
	iterType := c.info.Types[s.Iterable]
	if c.typeSubst != nil {
		iterType = types.Substitute(iterType, c.typeSubst)
	}
	inst, ok := iterType.(*types.Instance)
	if !ok {
		panic(fmt.Sprintf("codegen: for-in map target is %T, want Instance", iterType))
	}
	name := monoName(inst)

	// Call keys() and values() methods
	keysFnName := mangleMethodName(name, "keys", false)
	keysFn := c.funcs[keysFnName]
	valuesFnName := mangleMethodName(name, "values", false)
	valuesFn := c.funcs[valuesFnName]
	if keysFn == nil || valuesFn == nil {
		panic(fmt.Sprintf("codegen: undeclared map keys/values method for %s", name))
	}

	instancePtr := c.extractInstancePtr(mapVal)
	keysVec := c.block.NewCall(keysFn, instancePtr)
	valsVec := c.block.NewCall(valuesFn, instancePtr)

	// Get length from keys vector
	headerType := vectorHeaderType()
	headerPtr := c.block.NewBitCast(keysVec, irtypes.NewPointer(headerType))
	lenPtr := c.block.NewGetElementPtr(headerType, headerPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
	length := c.block.NewLoad(irtypes.I64, lenPtr)

	// Build tuple type for the binding (K, V)
	tupleType := irtypes.NewStruct(keyLLVM, valLLVM)

	// Counter alloca
	counterAlloca := c.block.NewAlloca(irtypes.I64)
	c.block.NewStore(constant.NewInt(irtypes.I64, 0), counterAlloca)

	// Binding alloca for the (K, V) tuple
	bindingAlloca := c.block.NewAlloca(tupleType)
	bindingAlloca.SetName(c.uniqueLocalName(s.Binding))
	c.locals[s.Binding] = bindingAlloca

	// Index variable if present
	if s.Index != "" {
		indexAlloca := c.block.NewAlloca(irtypes.I64)
		indexAlloca.SetName(c.uniqueLocalName(s.Index))
		c.block.NewStore(constant.NewInt(irtypes.I64, 0), indexAlloca)
		c.locals[s.Index] = indexAlloca
	}

	headerBlock := c.newBlock("forin.header")
	bodyBlock := c.newBlock("forin.body")
	updateBlock := c.newBlock("forin.update")
	exitBlock := c.newBlock("forin.exit")

	c.block.NewBr(headerBlock)

	// Header: compare counter < length
	c.block = headerBlock
	counter := c.block.NewLoad(irtypes.I64, counterAlloca)
	cond := c.block.NewICmp(enum.IPredULT, counter, length)
	c.block.NewCondBr(cond, bodyBlock, exitBlock)

	// Body: load key[i] and value[i], build tuple
	savedBreak := c.breakTarget
	savedContinue := c.continueTarget
	savedLoopUseDepth := c.loopScopeDepth
	c.breakTarget = exitBlock
	c.continueTarget = updateBlock
	c.loopScopeDepth = len(c.scopeBindings)

	c.block = bodyBlock
	idx := c.block.NewLoad(irtypes.I64, counterAlloca)

	// Load key from keys vector
	keyDataBase := c.block.NewGetElementPtr(irtypes.I8, keysVec,
		constant.NewInt(irtypes.I64, int64(vectorHeaderSize)))
	keyDataPtr := c.block.NewBitCast(keyDataBase, irtypes.NewPointer(keyLLVM))
	keyElemPtr := c.block.NewGetElementPtr(keyLLVM, keyDataPtr, idx)
	key := c.block.NewLoad(keyLLVM, keyElemPtr)

	// Load value from values vector
	valDataBase := c.block.NewGetElementPtr(irtypes.I8, valsVec,
		constant.NewInt(irtypes.I64, int64(vectorHeaderSize)))
	valDataPtr := c.block.NewBitCast(valDataBase, irtypes.NewPointer(valLLVM))
	valElemPtr := c.block.NewGetElementPtr(valLLVM, valDataPtr, idx)
	val := c.block.NewLoad(valLLVM, valElemPtr)

	var tuple value.Value = constant.NewZeroInitializer(tupleType)
	tuple = c.block.NewInsertValue(tuple, key, 0)
	tuple = c.block.NewInsertValue(tuple, val, 1)
	c.block.NewStore(tuple, bindingAlloca)

	c.genBlock(s.Body)
	if c.block.Term == nil {
		c.block.NewBr(updateBlock)
	}

	// Update: increment counter (and index if present)
	c.block = updateBlock
	curCount := c.block.NewLoad(irtypes.I64, counterAlloca)
	nextCount := c.block.NewAdd(curCount, constant.NewInt(irtypes.I64, 1))
	c.block.NewStore(nextCount, counterAlloca)
	if s.Index != "" {
		idxAlloca := c.locals[s.Index]
		curIdx := c.block.NewLoad(irtypes.I64, idxAlloca)
		nextIdx := c.block.NewAdd(curIdx, constant.NewInt(irtypes.I64, 1))
		c.block.NewStore(nextIdx, idxAlloca)
	}
	c.emitYieldCheck()
	c.block.NewBr(headerBlock)

	c.breakTarget = savedBreak
	c.continueTarget = savedContinue
	c.loopScopeDepth = savedLoopUseDepth
	c.block = exitBlock
}

// --- For-in over strings ---

func (c *Compiler) genForInString(s *ast.ForInStmt, strPtr value.Value) {
	// Alloca for byte position
	posAlloca := c.block.NewAlloca(irtypes.I64)
	c.block.NewStore(constant.NewInt(irtypes.I64, 0), posAlloca)

	// Index variable if present
	if s.Index != "" {
		indexAlloca := c.block.NewAlloca(irtypes.I64)
		indexAlloca.SetName(c.uniqueLocalName(s.Index))
		c.block.NewStore(constant.NewInt(irtypes.I64, 0), indexAlloca)
		c.locals[s.Index] = indexAlloca
	}

	headerBlock := c.newBlock("forin.str.header")
	bodyBlock := c.newBlock("forin.str.body")
	exitBlock := c.newBlock("forin.str.exit")

	c.block.NewBr(headerBlock)

	// Header: call promise_string_next_char, check for -1
	c.block = headerBlock
	cp := c.block.NewCall(c.funcs["promise_string_next_char"], strPtr, posAlloca)
	done := c.block.NewICmp(enum.IPredEQ, cp, constant.NewInt(irtypes.I32, -1))
	c.block.NewCondBr(done, exitBlock, bodyBlock)

	// Body: bind char to loop variable
	savedBreak := c.breakTarget
	savedContinue := c.continueTarget
	savedLoopUseDepth := c.loopScopeDepth
	c.breakTarget = exitBlock
	c.continueTarget = headerBlock
	c.loopScopeDepth = len(c.scopeBindings)

	c.block = bodyBlock
	alloca := c.block.NewAlloca(irtypes.I32)
	alloca.SetName(c.uniqueLocalName(s.Binding))
	c.block.NewStore(cp, alloca)
	c.locals[s.Binding] = alloca

	c.genBlock(s.Body)

	// Increment index after body, before looping back
	if s.Index != "" && c.block.Term == nil {
		idxAlloca := c.locals[s.Index]
		curIdx := c.block.NewLoad(irtypes.I64, idxAlloca)
		nextIdx := c.block.NewAdd(curIdx, constant.NewInt(irtypes.I64, 1))
		c.block.NewStore(nextIdx, idxAlloca)
	}

	if c.block.Term == nil {
		c.emitYieldCheck()
		c.block.NewBr(headerBlock)
	}

	c.breakTarget = savedBreak
	c.continueTarget = savedContinue
	c.loopScopeDepth = savedLoopUseDepth
	c.block = exitBlock
}

// genSelectStmt generates LLVM IR for a select statement.
// Implements Go-style lock-all-channels protocol:
// 1. Evaluate all channel expressions
// 2. Lock all channels sorted by address (deadlock prevention)
// 3. Check which cases can proceed (non-blocking)
// 4. If one can: execute it, unlock all
// 5. If none + default: unlock all, execute default
// 6. If none + no default: park on waiter lists, suspend, dispatch on wake
func (c *Compiler) genSelectStmt(s *ast.SelectStmt) {
	nCases := len(s.Cases)
	chanType := channelStructType()

	// Step 1: Evaluate channel expressions and gather info
	type selectCaseInfo struct {
		chRaw         value.Value
		chPtr         value.Value
		isSend        bool
		sendValueExpr ast.Expr
		binding       string
		elemLLVM      irtypes.Type
		elemSize      int64
	}

	caseInfos := make([]selectCaseInfo, nCases)
	for i, sc := range s.Cases {
		chRaw := c.genExpr(sc.Channel)
		chPtr := c.block.NewBitCast(chRaw, irtypes.NewPointer(chanType))

		semaType := c.info.Types[sc.Channel]
		inst := semaType.(*types.Instance)
		elemType := inst.TypeArgs()[0]
		elemLLVM := c.resolveType(elemType)
		elemSize := int64(llvmTypeSize(elemLLVM))

		caseInfos[i] = selectCaseInfo{
			chRaw:         chRaw,
			chPtr:         chPtr,
			isSend:        sc.IsSend,
			sendValueExpr: sc.SendValue,
			binding:       sc.Binding,
			elemLLVM:      elemLLVM,
			elemSize:      elemSize,
		}
	}

	// Step 2: Sort channel pointers by address and lock all.
	i8PtrTy := irtypes.I8Ptr
	arrType := irtypes.NewArray(uint64(nCases), i8PtrTy)
	chArr := c.block.NewAlloca(arrType)

	for i, ci := range caseInfos {
		ptr := c.block.NewGetElementPtr(arrType, chArr,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(i)))
		c.block.NewStore(ci.chRaw, ptr)
	}

	// Inline bubble sort by pointer address (for deadlock prevention)
	if nCases > 1 {
		for pass := 0; pass < nCases-1; pass++ {
			for j := 0; j < nCases-1-pass; j++ {
				ptrA := c.block.NewGetElementPtr(arrType, chArr,
					constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(j)))
				ptrB := c.block.NewGetElementPtr(arrType, chArr,
					constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(j+1)))
				valA := c.block.NewLoad(i8PtrTy, ptrA)
				valB := c.block.NewLoad(i8PtrTy, ptrB)
				intA := c.block.NewPtrToInt(valA, irtypes.I64)
				intB := c.block.NewPtrToInt(valB, irtypes.I64)
				needSwap := c.block.NewICmp(enum.IPredUGT, intA, intB)

				swapBlk := c.newBlock(fmt.Sprintf("select.sort.swap.%d.%d", pass, j))
				contBlk := c.newBlock(fmt.Sprintf("select.sort.cont.%d.%d", pass, j))
				c.block.NewCondBr(needSwap, swapBlk, contBlk)

				c.block = swapBlk
				c.block.NewStore(valB, ptrA)
				c.block.NewStore(valA, ptrB)
				c.block.NewBr(contBlk)

				c.block = contBlk
			}
		}
	}

	// Lock all channels in sorted order (skip duplicates)
	for i := 0; i < nCases; i++ {
		ptr := c.block.NewGetElementPtr(arrType, chArr,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(i)))
		chRawSorted := c.block.NewLoad(i8PtrTy, ptr)
		chPtrSorted := c.block.NewBitCast(chRawSorted, irtypes.NewPointer(chanType))
		mtxPtr := c.block.NewGetElementPtr(chanType, chPtrSorted,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(chanFieldMutex)))
		mtx := c.block.NewLoad(i8PtrTy, mtxPtr)

		if i > 0 {
			// Skip if same channel as previous (avoid double-lock)
			prevPtr := c.block.NewGetElementPtr(arrType, chArr,
				constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(i-1)))
			prevRaw := c.block.NewLoad(i8PtrTy, prevPtr)
			isSame := c.block.NewICmp(enum.IPredEQ, chRawSorted, prevRaw)
			lockBlk := c.newBlock(fmt.Sprintf("select.lock.%d", i))
			skipBlk := c.newBlock(fmt.Sprintf("select.lock.skip.%d", i))
			c.block.NewCondBr(isSame, skipBlk, lockBlk)
			c.block = lockBlk
			c.block.NewCall(c.palMutexLock, mtx)
			c.block.NewBr(skipBlk)
			c.block = skipBlk
		} else {
			c.block.NewCall(c.palMutexLock, mtx)
		}
	}

	// Step 3: Try each case to see if it can proceed
	mergeBlk := c.newBlock("select.merge")
	caseExecBlks := make([]*ir.Block, nCases)
	for i := range nCases {
		caseExecBlks[i] = c.newBlock(fmt.Sprintf("select.case%d.exec", i))
	}

	// After trying all: default or park or merge
	var afterTryBlk *ir.Block
	var defaultBlk *ir.Block
	if s.Default != nil {
		defaultBlk = c.newBlock("select.default")
		afterTryBlk = defaultBlk
	} else if c.inCoroutine {
		afterTryBlk = c.newBlock("select.park")
	} else {
		afterTryBlk = mergeBlk
	}

	// Generate try-check chain
	firstTryBlk := c.newBlock("select.try0")
	c.block.NewBr(firstTryBlk)
	c.block = firstTryBlk

	for i, ci := range caseInfos {
		var nextCheck *ir.Block
		if i+1 < nCases {
			nextCheck = c.newBlock(fmt.Sprintf("select.try%d", i+1))
		} else {
			nextCheck = afterTryBlk
		}

		if ci.isSend {
			countPtr := c.block.NewGetElementPtr(chanType, ci.chPtr,
				constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(chanFieldCount)))
			count := c.block.NewLoad(irtypes.I64, countPtr)
			capPtr := c.block.NewGetElementPtr(chanType, ci.chPtr,
				constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(chanFieldCapacity)))
			cap_ := c.block.NewLoad(irtypes.I64, capPtr)
			notFull := c.block.NewICmp(enum.IPredULT, count, cap_)
			closedPtr := c.block.NewGetElementPtr(chanType, ci.chPtr,
				constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(chanFieldClosed)))
			closedVal := c.block.NewLoad(irtypes.I8, closedPtr)
			isOpen := c.block.NewICmp(enum.IPredEQ, closedVal, constant.NewInt(irtypes.I8, 0))
			canSend := c.block.NewAnd(notFull, isOpen)
			c.block.NewCondBr(canSend, caseExecBlks[i], nextCheck)
		} else {
			countPtr := c.block.NewGetElementPtr(chanType, ci.chPtr,
				constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(chanFieldCount)))
			count := c.block.NewLoad(irtypes.I64, countPtr)
			hasItems := c.block.NewICmp(enum.IPredUGT, count, constant.NewInt(irtypes.I64, 0))
			closedPtr := c.block.NewGetElementPtr(chanType, ci.chPtr,
				constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(chanFieldClosed)))
			closedVal := c.block.NewLoad(irtypes.I8, closedPtr)
			isClosed := c.block.NewICmp(enum.IPredEQ, closedVal, constant.NewInt(irtypes.I8, 1))
			canRecv := c.block.NewOr(hasItems, isClosed)
			c.block.NewCondBr(canRecv, caseExecBlks[i], nextCheck)
		}

		if i+1 < nCases {
			c.block = nextCheck
		}
	}

	// Helper: generate unlock-all code
	unlockAll := func() {
		for j := nCases - 1; j >= 0; j-- {
			ptr := c.block.NewGetElementPtr(arrType, chArr,
				constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(j)))
			chRawSorted := c.block.NewLoad(i8PtrTy, ptr)

			if j < nCases-1 {
				// Skip if same as next (since we're going in reverse)
				nextPtr := c.block.NewGetElementPtr(arrType, chArr,
					constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(j+1)))
				nextRaw := c.block.NewLoad(i8PtrTy, nextPtr)
				isSame := c.block.NewICmp(enum.IPredEQ, chRawSorted, nextRaw)
				unlockBlk := c.newBlock(fmt.Sprintf("select.unlock.%d", j))
				skipBlk := c.newBlock(fmt.Sprintf("select.unlock.skip.%d", j))
				c.block.NewCondBr(isSame, skipBlk, unlockBlk)
				c.block = unlockBlk
				chPtrSorted := c.block.NewBitCast(chRawSorted, irtypes.NewPointer(chanType))
				mtxPtr := c.block.NewGetElementPtr(chanType, chPtrSorted,
					constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(chanFieldMutex)))
				mtx := c.block.NewLoad(i8PtrTy, mtxPtr)
				c.block.NewCall(c.palMutexUnlock, mtx)
				c.block.NewBr(skipBlk)
				c.block = skipBlk
			} else {
				chPtrSorted := c.block.NewBitCast(chRawSorted, irtypes.NewPointer(chanType))
				mtxPtr := c.block.NewGetElementPtr(chanType, chPtrSorted,
					constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(chanFieldMutex)))
				mtx := c.block.NewLoad(i8PtrTy, mtxPtr)
				c.block.NewCall(c.palMutexUnlock, mtx)
			}
		}
	}

	// Helper: generate send execution code for a case
	execSend := func(ci selectCaseInfo, prefix string) {
		argVal := c.genExpr(ci.sendValueExpr)
		argAlloca := c.block.NewAlloca(ci.elemLLVM)
		c.block.NewStore(argVal, argAlloca)
		argAsI8 := c.block.NewBitCast(argAlloca, i8PtrTy)

		bufPtr := c.block.NewGetElementPtr(chanType, ci.chPtr,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(chanFieldBuffer)))
		buf := c.block.NewLoad(i8PtrTy, bufPtr)
		tailPtr := c.block.NewGetElementPtr(chanType, ci.chPtr,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(chanFieldTail)))
		tail := c.block.NewLoad(irtypes.I64, tailPtr)
		offset := c.block.NewMul(tail, constant.NewInt(irtypes.I64, ci.elemSize))
		dest := c.block.NewGetElementPtr(irtypes.I8, buf, offset)
		c.block.NewCall(c.funcs["llvm.memcpy"], dest, argAsI8,
			constant.NewInt(irtypes.I64, ci.elemSize), constant.False)

		capPtr := c.block.NewGetElementPtr(chanType, ci.chPtr,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(chanFieldCapacity)))
		cap_ := c.block.NewLoad(irtypes.I64, capPtr)
		tailPlusOne := c.block.NewAdd(tail, constant.NewInt(irtypes.I64, 1))
		newTail := c.block.NewURem(tailPlusOne, cap_)
		c.block.NewStore(newTail, tailPtr)

		countPtr := c.block.NewGetElementPtr(chanType, ci.chPtr,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(chanFieldCount)))
		countVal := c.block.NewLoad(irtypes.I64, countPtr)
		newCount := c.block.NewAdd(countVal, constant.NewInt(irtypes.I64, 1))
		c.block.NewStore(newCount, countPtr)

		// Wake a waiting receiver
		recvHeadPtr := c.block.NewGetElementPtr(chanType, ci.chPtr,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(chanFieldRecvWaitersHead)))
		recvTailPtr := c.block.NewGetElementPtr(chanType, ci.chPtr,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(chanFieldRecvWaitersTail)))
		recvWaiter := c.block.NewCall(c.funcs["promise_waiter_dequeue"], recvHeadPtr, recvTailPtr)
		hasRecvWaiter := c.block.NewICmp(enum.IPredNE, recvWaiter, constant.NewNull(i8PtrTy))
		wakeBlk := c.newBlock(prefix + ".wake")
		signalBlk := c.newBlock(prefix + ".signal")
		afterBlk := c.newBlock(prefix + ".after")
		c.block.NewCondBr(hasRecvWaiter, wakeBlk, signalBlk)

		c.block = wakeBlk
		gTy := goroutineStructType()
		wTyped := c.block.NewBitCast(recvWaiter, irtypes.NewPointer(gTy))
		wStatusPtr := c.block.NewGetElementPtr(gTy, wTyped,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(gFieldStatus)))
		c.block.NewStore(constant.NewInt(irtypes.I8, gStatusRunnable), wStatusPtr)
		c.block.NewCall(c.funcs["promise_sched_enqueue"], recvWaiter)
		c.block.NewBr(afterBlk)

		c.block = signalBlk
		nePtr := c.block.NewGetElementPtr(chanType, ci.chPtr,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(chanFieldNotEmpty)))
		ne := c.block.NewLoad(i8PtrTy, nePtr)
		c.block.NewCall(c.palCondSignal, ne)
		c.block.NewBr(afterBlk)

		c.block = afterBlk
	}

	// Helper: generate recv execution code for a case
	execRecv := func(ci selectCaseInfo, prefix string) {
		optType := irtypes.NewStruct(irtypes.I1, ci.elemLLVM)
		countPtr := c.block.NewGetElementPtr(chanType, ci.chPtr,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(chanFieldCount)))
		count := c.block.NewLoad(irtypes.I64, countPtr)
		isEmpty := c.block.NewICmp(enum.IPredEQ, count, constant.NewInt(irtypes.I64, 0))

		noneBlk := c.newBlock(prefix + ".none")
		readBlk := c.newBlock(prefix + ".read")
		doneBlk := c.newBlock(prefix + ".done")
		c.block.NewCondBr(isEmpty, noneBlk, readBlk)

		c.block = noneBlk
		noneVal := constant.NewZeroInitializer(optType)
		c.block.NewBr(doneBlk)

		c.block = readBlk
		bufPtr := c.block.NewGetElementPtr(chanType, ci.chPtr,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(chanFieldBuffer)))
		buf := c.block.NewLoad(i8PtrTy, bufPtr)
		headPtr := c.block.NewGetElementPtr(chanType, ci.chPtr,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(chanFieldHead)))
		head := c.block.NewLoad(irtypes.I64, headPtr)
		offset := c.block.NewMul(head, constant.NewInt(irtypes.I64, ci.elemSize))
		src := c.block.NewGetElementPtr(irtypes.I8, buf, offset)
		rAlloca := c.block.NewAlloca(ci.elemLLVM)
		rAsI8 := c.block.NewBitCast(rAlloca, i8PtrTy)
		c.block.NewCall(c.funcs["llvm.memcpy"], rAsI8, src,
			constant.NewInt(irtypes.I64, ci.elemSize), constant.False)
		rVal := c.block.NewLoad(ci.elemLLVM, rAlloca)

		capPtr := c.block.NewGetElementPtr(chanType, ci.chPtr,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(chanFieldCapacity)))
		cap_ := c.block.NewLoad(irtypes.I64, capPtr)
		headPlusOne := c.block.NewAdd(head, constant.NewInt(irtypes.I64, 1))
		newHead := c.block.NewURem(headPlusOne, cap_)
		c.block.NewStore(newHead, headPtr)

		countRead := c.block.NewLoad(irtypes.I64, countPtr)
		newCount := c.block.NewSub(countRead, constant.NewInt(irtypes.I64, 1))
		c.block.NewStore(newCount, countPtr)

		// Wake a waiting sender
		sendHeadPtr := c.block.NewGetElementPtr(chanType, ci.chPtr,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(chanFieldSendWaitersHead)))
		sendTailPtr := c.block.NewGetElementPtr(chanType, ci.chPtr,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(chanFieldSendWaitersTail)))
		sendWaiter := c.block.NewCall(c.funcs["promise_waiter_dequeue"], sendHeadPtr, sendTailPtr)
		hasSendWaiter := c.block.NewICmp(enum.IPredNE, sendWaiter, constant.NewNull(i8PtrTy))
		wakeBlk := c.newBlock(prefix + ".wsend")
		signalBlk := c.newBlock(prefix + ".ssend")
		afterBlk := c.newBlock(prefix + ".afterwk")
		c.block.NewCondBr(hasSendWaiter, wakeBlk, signalBlk)

		c.block = wakeBlk
		gTy := goroutineStructType()
		wTyped := c.block.NewBitCast(sendWaiter, irtypes.NewPointer(gTy))
		wStatusPtr := c.block.NewGetElementPtr(gTy, wTyped,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(gFieldStatus)))
		c.block.NewStore(constant.NewInt(irtypes.I8, gStatusRunnable), wStatusPtr)
		c.block.NewCall(c.funcs["promise_sched_enqueue"], sendWaiter)
		c.block.NewBr(afterBlk)

		c.block = signalBlk
		nfPtr := c.block.NewGetElementPtr(chanType, ci.chPtr,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(chanFieldNotFull)))
		nf := c.block.NewLoad(i8PtrTy, nfPtr)
		c.block.NewCall(c.palCondSignal, nf)
		c.block.NewBr(afterBlk)

		c.block = afterBlk
		someVal := c.block.NewInsertValue(constant.NewZeroInitializer(optType), constant.True, 0)
		someVal2 := c.block.NewInsertValue(someVal, rVal, 1)
		c.block.NewBr(doneBlk)

		c.block = doneBlk
		recvPhi := c.block.NewPhi(
			&ir.Incoming{X: noneVal, Pred: noneBlk},
			&ir.Incoming{X: someVal2, Pred: afterBlk},
		)

		if ci.binding != "_" {
			alloca := c.block.NewAlloca(optType)
			alloca.SetName(c.uniqueLocalName(ci.binding))
			c.block.NewStore(recvPhi, alloca)
			c.locals[ci.binding] = alloca
		}
	}

	// Step 4: Generate case execution blocks (non-blocking path)
	for i, ci := range caseInfos {
		c.block = caseExecBlks[i]
		savedScopeLen := len(c.scopeBindings)

		prefix := fmt.Sprintf("select.c%d", i)
		if ci.isSend {
			execSend(ci, prefix)
		} else {
			execRecv(ci, prefix)
		}

		unlockAll()

		for _, stmt := range s.Cases[i].Body {
			if c.block.Term != nil {
				break
			}
			c.genStmt(stmt)
		}
		if c.block != nil && c.block.Term == nil && len(c.scopeBindings) > savedScopeLen {
			c.emitScopeCleanup(savedScopeLen)
		}
		c.scopeBindings = c.scopeBindings[:savedScopeLen]
		if c.block != nil && c.block.Term == nil {
			c.block.NewBr(mergeBlk)
		}
	}

	// Step 5: Default block
	if defaultBlk != nil {
		c.block = defaultBlk
		savedScopeLen := len(c.scopeBindings)
		unlockAll()
		for _, stmt := range s.Default {
			if c.block.Term != nil {
				break
			}
			c.genStmt(stmt)
		}
		if c.block != nil && c.block.Term == nil && len(c.scopeBindings) > savedScopeLen {
			c.emitScopeCleanup(savedScopeLen)
		}
		c.scopeBindings = c.scopeBindings[:savedScopeLen]
		if c.block != nil && c.block.Term == nil {
			c.block.NewBr(mergeBlk)
		}
	}

	// Step 6: Blocking park (no default, coroutine mode)
	if s.Default == nil && c.inCoroutine {
		// afterTryBlk is the park block
		c.block = afterTryBlk

		currentG := c.block.NewLoad(i8PtrTy, c.currentGGlobal)
		gTy := goroutineStructType()
		gPtrTy := irtypes.NewPointer(gTy)
		gTyped := c.block.NewBitCast(currentG, gPtrTy)

		// Set G.select_case = -1
		selCasePtr := c.block.NewGetElementPtr(gTy, gTyped,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(gFieldSelectCase)))
		c.block.NewStore(constant.NewInt(irtypes.I32, 0xFFFFFFFF), selCasePtr)

		// Enqueue on all waiter lists
		for _, ci := range caseInfos {
			var headPtr, tailPtr value.Value
			if ci.isSend {
				headPtr = c.block.NewGetElementPtr(chanType, ci.chPtr,
					constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(chanFieldSendWaitersHead)))
				tailPtr = c.block.NewGetElementPtr(chanType, ci.chPtr,
					constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(chanFieldSendWaitersTail)))
			} else {
				headPtr = c.block.NewGetElementPtr(chanType, ci.chPtr,
					constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(chanFieldRecvWaitersHead)))
				tailPtr = c.block.NewGetElementPtr(chanType, ci.chPtr,
					constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(chanFieldRecvWaitersTail)))
			}
			c.block.NewCall(c.funcs["promise_waiter_enqueue"], headPtr, tailPtr, currentG)
		}

		// Unlock all channels and suspend
		pmField := c.block.NewGetElementPtr(gTy, gTyped,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(gFieldParkMutex)))
		c.block.NewStore(constant.NewNull(i8PtrTy), pmField)
		unlockAll()

		suspResult := c.block.NewCall(c.coroSuspend, constant.None, constant.False)
		resumeBlk := c.newBlock("select.resume")
		c.block.NewSwitch(suspResult, c.coroSuspendBlk,
			ir.NewCase(constant.NewInt(irtypes.I8, 0), resumeBlk),
			ir.NewCase(constant.NewInt(irtypes.I8, 1), c.coroCleanupBlk))

		// On resume: re-lock all, remove from all waiter lists, find ready case
		c.block = resumeBlk

		// Re-lock all channels in sorted order
		for i := 0; i < nCases; i++ {
			ptr := c.block.NewGetElementPtr(arrType, chArr,
				constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(i)))
			chRawSorted := c.block.NewLoad(i8PtrTy, ptr)

			if i > 0 {
				prevPtr := c.block.NewGetElementPtr(arrType, chArr,
					constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(i-1)))
				prevRaw := c.block.NewLoad(i8PtrTy, prevPtr)
				isSame := c.block.NewICmp(enum.IPredEQ, chRawSorted, prevRaw)
				lockBlk := c.newBlock(fmt.Sprintf("select.relock.%d", i))
				skipBlk := c.newBlock(fmt.Sprintf("select.relock.skip.%d", i))
				c.block.NewCondBr(isSame, skipBlk, lockBlk)
				c.block = lockBlk
				chPtrSorted := c.block.NewBitCast(chRawSorted, irtypes.NewPointer(chanType))
				mtxPtr := c.block.NewGetElementPtr(chanType, chPtrSorted,
					constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(chanFieldMutex)))
				mtx := c.block.NewLoad(i8PtrTy, mtxPtr)
				c.block.NewCall(c.palMutexLock, mtx)
				c.block.NewBr(skipBlk)
				c.block = skipBlk
			} else {
				chPtrSorted := c.block.NewBitCast(chRawSorted, irtypes.NewPointer(chanType))
				mtxPtr := c.block.NewGetElementPtr(chanType, chPtrSorted,
					constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(chanFieldMutex)))
				mtx := c.block.NewLoad(i8PtrTy, mtxPtr)
				c.block.NewCall(c.palMutexLock, mtx)
			}
		}

		// Remove from all waiter lists
		for _, ci := range caseInfos {
			var headPtr, tailPtr value.Value
			if ci.isSend {
				headPtr = c.block.NewGetElementPtr(chanType, ci.chPtr,
					constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(chanFieldSendWaitersHead)))
				tailPtr = c.block.NewGetElementPtr(chanType, ci.chPtr,
					constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(chanFieldSendWaitersTail)))
			} else {
				headPtr = c.block.NewGetElementPtr(chanType, ci.chPtr,
					constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(chanFieldRecvWaitersHead)))
				tailPtr = c.block.NewGetElementPtr(chanType, ci.chPtr,
					constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(chanFieldRecvWaitersTail)))
			}
			c.block.NewCall(c.funcs["promise_waiter_remove"], headPtr, tailPtr, currentG)
		}

		// Try each case again — one should be ready
		// Pre-create all try blocks upfront to avoid subtle block-management issues
		wakeTryBlks := make([]*ir.Block, nCases)
		for i := range nCases {
			wakeTryBlks[i] = c.newBlock(fmt.Sprintf("select.wk.try%d", i))
		}
		c.block.NewBr(wakeTryBlks[0])

		for i, ci := range caseInfos {
			c.block = wakeTryBlks[i]

			var nextTryBlk *ir.Block
			if i+1 < nCases {
				nextTryBlk = wakeTryBlks[i+1]
			} else {
				nextTryBlk = mergeBlk // fallback — shouldn't happen
			}

			wakeExecBlk := c.newBlock(fmt.Sprintf("select.wk.c%d.exec", i))

			if ci.isSend {
				countPtr := c.block.NewGetElementPtr(chanType, ci.chPtr,
					constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(chanFieldCount)))
				count := c.block.NewLoad(irtypes.I64, countPtr)
				capPtr := c.block.NewGetElementPtr(chanType, ci.chPtr,
					constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(chanFieldCapacity)))
				cap_ := c.block.NewLoad(irtypes.I64, capPtr)
				notFull := c.block.NewICmp(enum.IPredULT, count, cap_)
				closedPtr := c.block.NewGetElementPtr(chanType, ci.chPtr,
					constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(chanFieldClosed)))
				closedVal := c.block.NewLoad(irtypes.I8, closedPtr)
				isOpen := c.block.NewICmp(enum.IPredEQ, closedVal, constant.NewInt(irtypes.I8, 0))
				canSend := c.block.NewAnd(notFull, isOpen)
				c.block.NewCondBr(canSend, wakeExecBlk, nextTryBlk)
			} else {
				countPtr := c.block.NewGetElementPtr(chanType, ci.chPtr,
					constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(chanFieldCount)))
				count := c.block.NewLoad(irtypes.I64, countPtr)
				hasItems := c.block.NewICmp(enum.IPredUGT, count, constant.NewInt(irtypes.I64, 0))
				closedPtr := c.block.NewGetElementPtr(chanType, ci.chPtr,
					constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(chanFieldClosed)))
				closedVal := c.block.NewLoad(irtypes.I8, closedPtr)
				isClosed := c.block.NewICmp(enum.IPredEQ, closedVal, constant.NewInt(irtypes.I8, 1))
				canRecv := c.block.NewOr(hasItems, isClosed)
				c.block.NewCondBr(canRecv, wakeExecBlk, nextTryBlk)
			}

			c.block = wakeExecBlk
			savedScopeLen := len(c.scopeBindings)

			prefix := fmt.Sprintf("select.wk.c%d", i)
			if ci.isSend {
				execSend(ci, prefix)
			} else {
				execRecv(ci, prefix)
			}

			unlockAll()

			for _, stmt := range s.Cases[i].Body {
				if c.block.Term != nil {
					break
				}
				c.genStmt(stmt)
			}
			if c.block != nil && c.block.Term == nil && len(c.scopeBindings) > savedScopeLen {
				c.emitScopeCleanup(savedScopeLen)
			}
			c.scopeBindings = c.scopeBindings[:savedScopeLen]
			if c.block != nil && c.block.Term == nil {
				c.block.NewBr(mergeBlk)
			}
		}
	}

	c.block = mergeBlk
}
