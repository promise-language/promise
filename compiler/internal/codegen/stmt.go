package codegen

import (
	"fmt"

	"github.com/llir/llvm/ir"
	"github.com/llir/llvm/ir/constant"
	"github.com/llir/llvm/ir/enum"
	irtypes "github.com/llir/llvm/ir/types"
	"github.com/llir/llvm/ir/value"

	"djabi.dev/go/promise_lang/internal/ast"
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
	case *ast.Block:
		c.genBlock(s)
	default:
		panic(fmt.Sprintf("codegen: unhandled statement type %T", stmt))
	}
}

// --- Variable declarations ---

func (c *Compiler) genTypedVarDecl(s *ast.TypedVarDecl) {
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
	alloca.SetName(s.Name)

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
	alloca.SetName(s.Name)
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
		alloca.SetName(name)
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
	alloca.SetName(s.Name)
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
	dropFlag.SetName(varName + ".dropflag")
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
	dropFlag.SetName(varName + ".dropflag")
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
			// TODO(drop): When reassigning a droppable variable, the old value
			// should be dropped before storing the new one. Currently the old
			// value is silently overwritten, which leaks it. The ownership
			// checker allows this (resurrects the variable), so this is a
			// resource leak, not a soundness bug. Fix in a future stage.

			// Coerce value struct vtable when crossing type boundaries
			exprType := c.info.Types[s.Value]
			targetType := c.info.Types[target]
			if c.typeSubst != nil {
				exprType = types.Substitute(exprType, c.typeSubst)
				targetType = types.Substitute(targetType, c.typeSubst)
			}
			val = c.coerceToView(val, exprType, targetType)
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
		// Load indexed element, apply op, store back (vector only)
		indexTargetType := c.info.Types[t.Target]
		if c.typeSubst != nil {
			indexTargetType = types.Substitute(indexTargetType, c.typeSubst)
		}
		elem, ok := types.AsVector(indexTargetType)
		if !ok {
			panic(fmt.Sprintf("codegen: inc/dec on index of non-vector type %s", indexTargetType))
		}
		slicePtr := c.genExpr(t.Target)
		idx := c.genExpr(t.Index)
		elemLLVM := c.resolveType(elem)

		// Bounds check
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
	alloca.SetName(s.Binding)
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
	alloca.SetName(s.Binding)
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
	counterAlloca.SetName(s.Binding)
	c.block.NewStore(start, counterAlloca)
	c.locals[s.Binding] = counterAlloca

	if s.Index != "" {
		indexAlloca := c.block.NewAlloca(irtypes.I64)
		indexAlloca.SetName(s.Index)
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
		alloca.SetName(s.InitName)
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
	if c.block.Term == nil {
		c.block.NewBr(bodyBlock)
	}

	c.breakTarget = savedBreak
	c.continueTarget = savedContinue
	c.loopScopeDepth = savedLoopUseDepth
	c.block = exitBlock
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

	if elem, ok := types.AsVector(targetType); ok {
		c.genVectorIndexAssign(target, elem, op, val)
	} else if key, valT, ok := types.AsMap(targetType); ok {
		c.genMapIndexAssign(target, key, valT, val)
	} else {
		panic(fmt.Sprintf("codegen: cannot assign to index of type %s", targetType))
	}
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

// genMapIndexAssign handles m[k] = val via the monomorphized []= method.
func (c *Compiler) genMapIndexAssign(target *ast.IndexExpr, keyType, valType types.Type, val value.Value) {
	targetType := c.info.Types[target.Target]
	if c.typeSubst != nil {
		targetType = types.Substitute(targetType, c.typeSubst)
	}
	inst, ok := targetType.(*types.Instance)
	if !ok {
		panic(fmt.Sprintf("codegen: map index-assign target is %T, want Instance", targetType))
	}

	name := monoName(inst)
	setFnName := mangleMethodName(name, "[]=", false)
	setFn, ok := c.funcs[setFnName]
	if !ok {
		panic(fmt.Sprintf("codegen: undeclared map []= method %s", setFnName))
	}

	mapVal := c.genExpr(target.Target)
	keyVal := c.genExpr(target.Index)
	instancePtr := c.extractInstancePtr(mapVal)

	c.block.NewCall(setFn, instancePtr, keyVal, val)
}

// genCompoundIndexAssign handles compound index assignments (arr[i] += val, m[k] += val)
// with correct evaluation order: target → key → RHS.
func (c *Compiler) genCompoundIndexAssign(target *ast.IndexExpr, op ast.AssignOp, valueExpr ast.Expr) {
	targetType := c.info.Types[target.Target]
	if c.typeSubst != nil {
		targetType = types.Substitute(targetType, c.typeSubst)
	}

	if elem, ok := types.AsVector(targetType); ok {
		slicePtr := c.genExpr(target.Target)
		idx := c.genExpr(target.Index)
		val := c.genExpr(valueExpr)
		c.genVectorCompoundAssign(slicePtr, idx, elem, op, val)
	} else if _, valT, ok := types.AsMap(targetType); ok {
		mapVal := c.genExpr(target.Target)
		keyVal := c.genExpr(target.Index)
		val := c.genExpr(valueExpr)
		inst := targetType.(*types.Instance)
		c.genMapCompoundAssign(inst, mapVal, keyVal, valT, op, val)
	} else {
		panic(fmt.Sprintf("codegen: cannot compound-assign to index of type %s", targetType))
	}
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

// genMapCompoundAssign handles m["key"] += val by calling [] to get, applying op, then []=.
func (c *Compiler) genMapCompoundAssign(inst *types.Instance, mapVal, keyVal value.Value, valType types.Type, op ast.AssignOp, val value.Value) {
	valLLVM := c.resolveType(valType)
	name := monoName(inst)

	getFnName := mangleMethodName(name, "[]", false)
	getFn, ok := c.funcs[getFnName]
	if !ok {
		panic(fmt.Sprintf("codegen: undeclared map [] method %s", getFnName))
	}
	setFnName := mangleMethodName(name, "[]=", false)
	setFn, ok := c.funcs[setFnName]
	if !ok {
		panic(fmt.Sprintf("codegen: undeclared map []= method %s", setFnName))
	}

	instancePtr := c.extractInstancePtr(mapVal)

	// Call [] to get V? (optional)
	optVal := c.block.NewCall(getFn, instancePtr, keyVal)

	// Check has_value flag (field 0 of optional struct)
	hasVal := c.block.NewExtractValue(optVal, 0)
	okBlock := c.newBlock("mapcomp.ok")
	panicBlock := c.newBlock("mapcomp.panic")
	c.block.NewCondBr(hasVal, okBlock, panicBlock)

	// Panic: compound assignment requires existing key
	c.block = panicBlock
	panicMsg := c.makeGlobalString("compound assignment on missing map key")
	c.block.NewCall(c.funcs["promise_panic"], panicMsg)
	c.block.NewUnreachable()

	// OK: extract value, apply op, call []=
	c.block = okBlock
	optType := irtypes.NewStruct(irtypes.I1, valLLVM)
	_ = optType
	current := c.block.NewExtractValue(optVal, 1)
	result := c.genCompoundOp(op, current, val)

	c.block.NewCall(setFn, instancePtr, keyVal, result)
}

// --- lookupLocalType resolves the declared type for a TypedVarDecl ---
// It checks the TypeRef AST node to detect Optional declarations,
// then resolves the type by looking up the variable in sema scopes.

func (c *Compiler) lookupLocalType(s *ast.TypedVarDecl) types.Type {
	// Only need special handling for Optional declarations
	if _, ok := s.Type.(*ast.OptionalTypeRef); !ok {
		return nil // use expression type
	}

	exprType := c.info.Types[s.Value]
	if c.typeSubst != nil && exprType != nil {
		exprType = types.Substitute(exprType, c.typeSubst)
	}

	// If value is NoneLit, look up the declared type from sema scopes
	if exprType == types.TypNone || exprType == nil {
		return c.lookupVarType(s.Name)
	}

	// Value has a concrete type — wrap in Optional
	if _, isOpt := exprType.(*types.Optional); isOpt {
		return exprType // already Optional
	}
	return types.NewOptional(exprType)
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
	elemAlloca.SetName(s.Binding)
	c.locals[s.Binding] = elemAlloca

	// Index variable if present
	if s.Index != "" {
		indexAlloca := c.block.NewAlloca(irtypes.I64)
		indexAlloca.SetName(s.Index)
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
	elemAlloca.SetName(s.Binding)
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
		c.block.NewCall(c.palMutexUnlock, mtx)

		suspResult := c.block.NewCall(c.coroSuspend, constant.None, constant.False)
		resumeBlk := c.newBlock("forin_ch.recv.resume")
		c.block.NewSwitch(suspResult, c.coroCleanupBlk,
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
	bindingAlloca.SetName(s.Binding)
	c.locals[s.Binding] = bindingAlloca

	// Index variable if present
	if s.Index != "" {
		indexAlloca := c.block.NewAlloca(irtypes.I64)
		indexAlloca.SetName(s.Index)
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
		indexAlloca.SetName(s.Index)
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
	alloca.SetName(s.Binding)
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
		c.block.NewBr(headerBlock)
	}

	c.breakTarget = savedBreak
	c.continueTarget = savedContinue
	c.loopScopeDepth = savedLoopUseDepth
	c.block = exitBlock
}
