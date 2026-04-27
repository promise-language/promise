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
	for _, stmt := range block.Stmts {
		if c.block == nil || c.block.Term != nil {
			break // block already terminated (return, break, etc.)
		}
		c.genStmt(stmt)
	}
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

// --- Assignment ---

func (c *Compiler) genAssignStmt(s *ast.AssignStmt) {
	val := c.genExpr(s.Value)

	switch target := s.Target.(type) {
	case *ast.IdentExpr:
		alloca, ok := c.locals[target.Name]
		if !ok {
			panic(fmt.Sprintf("codegen: undefined variable %q in assignment", target.Name))
		}
		if s.Op == ast.OpAssign {
			// Coerce value struct vtable when crossing type boundaries
			exprType := c.info.Types[s.Value]
			targetType := c.info.Types[target]
			val = c.coerceToView(val, exprType, targetType)
			c.block.NewStore(val, alloca)
			return
		}
		// Compound assignment: load current value, apply operator, store result
		current := c.block.NewLoad(alloca.ElemType, alloca)
		result := c.genCompoundOp(s.Op, current, val)
		c.block.NewStore(result, alloca)

	case *ast.MemberExpr:
		c.genMemberAssign(target, s.Op, val)

	case *ast.IndexExpr:
		c.genIndexAssign(target, s.Op, val)

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

// genSetterCall emits a direct call to a setter method.
func (c *Compiler) genSetterCall(target *ast.MemberExpr, targetType types.Type, named *types.Named, setter *types.Method, val value.Value) {
	var mangledName string
	ownerName := c.resolveMethodOwner(named, target.Field)
	if ownerName != named.Obj().Name() {
		mangledName = ownerName + "." + target.Field
	} else {
		mangledName = c.resolveTypeName(targetType) + "." + target.Field
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
	if c.canError {
		resultType := c.currentResultType()
		if s.Value == nil {
			c.block.NewRet(c.wrapOk(nil, resultType))
		} else {
			val := c.genExpr(s.Value)
			// Coerce value struct vtable when returning through a parent type
			if c.currentRetType != nil {
				exprType := c.info.Types[s.Value]
				val = c.coerceToView(val, exprType, c.currentRetType)
			}
			c.block.NewRet(c.wrapOk(val, resultType))
		}
		return
	}
	if s.Value == nil {
		c.block.NewRet(nil)
	} else {
		val := c.genExpr(s.Value)
		// Coerce value struct vtable when returning through a parent type
		if c.currentRetType != nil {
			exprType := c.info.Types[s.Value]
			val = c.coerceToView(val, exprType, c.currentRetType)
		}
		c.block.NewRet(val)
	}
}

// --- Raise ---

func (c *Compiler) genRaiseStmt(s *ast.RaiseStmt) {
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
	c.breakTarget = exitBlock
	c.continueTarget = headerBlock

	c.block = bodyBlock
	c.genBlock(s.Body)
	if c.block.Term == nil {
		c.block.NewBr(headerBlock)
	}

	c.breakTarget = savedBreak
	c.continueTarget = savedContinue
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
	c.breakTarget = exitBlock
	c.continueTarget = headerBlock

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
	c.block = exitBlock
}

// --- For-in loop ---

func (c *Compiler) genForInStmt(s *ast.ForInStmt) {
	iterableType := c.info.Types[s.Iterable]
	if c.typeSubst != nil {
		iterableType = types.Substitute(iterableType, c.typeSubst)
	}

	if elem, ok := types.AsSlice(iterableType); ok {
		slicePtr := c.genExpr(s.Iterable)
		c.genForInSlice(s, slicePtr, elem)
	} else if key, val, ok := types.AsMap(iterableType); ok {
		mapPtr := c.genExpr(s.Iterable)
		c.genForInMap(s, mapPtr, key, val)
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
	c.breakTarget = exitBlock
	c.continueTarget = updateBlock

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
	c.breakTarget = exitBlock
	c.continueTarget = updateBlock

	c.block = bodyBlock
	c.genBlock(s.Body)
	if c.block.Term == nil {
		c.block.NewBr(updateBlock)
	}

	// Update
	c.block = updateBlock
	if s.UpdateTarget != nil {
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
	c.block = exitBlock
}

// --- Infinite loop ---

func (c *Compiler) genInfiniteLoop(s *ast.InfiniteLoop) {
	bodyBlock := c.newBlock("loop.body")
	exitBlock := c.newBlock("loop.exit")

	c.block.NewBr(bodyBlock)

	savedBreak := c.breakTarget
	savedContinue := c.continueTarget
	c.breakTarget = exitBlock
	c.continueTarget = bodyBlock

	c.block = bodyBlock
	c.genBlock(s.Body)
	if c.block.Term == nil {
		c.block.NewBr(bodyBlock)
	}

	c.breakTarget = savedBreak
	c.continueTarget = savedContinue
	c.block = exitBlock
}

// --- Break / Continue ---

func (c *Compiler) genBreakStmt() {
	if c.breakTarget != nil {
		c.block.NewBr(c.breakTarget)
	}
}

func (c *Compiler) genContinueStmt() {
	if c.continueTarget != nil {
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

	if elem, ok := types.AsSlice(targetType); ok {
		c.genSliceIndexAssign(target, elem, op, val)
	} else if key, valT, ok := types.AsMap(targetType); ok {
		if op != ast.OpAssign {
			c.genMapCompoundAssign(target, key, valT, op, val)
		} else {
			c.genMapIndexAssign(target, key, valT, val)
		}
	} else {
		panic(fmt.Sprintf("codegen: cannot assign to index of type %s", targetType))
	}
}

// genSliceIndexAssign handles arr[i] = val with bounds check.
func (c *Compiler) genSliceIndexAssign(target *ast.IndexExpr, elemType types.Type, op ast.AssignOp, val value.Value) {
	slicePtr := c.genExpr(target.Target)
	idx := c.genExpr(target.Index)
	elemLLVM := c.resolveType(elemType)

	// Bounds check
	headerType := sliceHeaderType()
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
		constant.NewInt(irtypes.I64, int64(sliceHeaderSize)))
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

// genMapIndexAssign handles m[k] = val via promise_map_set.
func (c *Compiler) genMapIndexAssign(target *ast.IndexExpr, keyType, valType types.Type, val value.Value) {
	mapPtr := c.genExpr(target.Target)
	keyVal := c.genExpr(target.Index)
	keyLLVM := c.resolveType(keyType)
	valLLVM := c.resolveType(valType)

	keyAlloca := c.block.NewAlloca(keyLLVM)
	c.block.NewStore(keyVal, keyAlloca)
	keyPtr := c.block.NewBitCast(keyAlloca, irtypes.I8Ptr)

	valAlloca := c.block.NewAlloca(valLLVM)
	c.block.NewStore(val, valAlloca)
	valPtr := c.block.NewBitCast(valAlloca, irtypes.I8Ptr)

	c.block.NewCall(c.funcs["promise_map_set"], mapPtr, keyPtr, valPtr)
}

// genMapCompoundAssign handles m["key"] += val by getting, applying op, and setting back.
//
// BUG: val is evaluated by the caller (genAssignStmt) before this function runs,
// so the evaluation order is RHS → map target → key. The correct semantic order
// should be map target → key → RHS. This matters when expressions have side effects
// (e.g. m[f()] += g()  would call g() before f()). Fixing this requires refactoring
// genAssignStmt to defer RHS evaluation for compound index assignments.
func (c *Compiler) genMapCompoundAssign(target *ast.IndexExpr, keyType, valType types.Type, op ast.AssignOp, val value.Value) {
	mapPtr := c.genExpr(target.Target)
	keyVal := c.genExpr(target.Index)
	keyLLVM := c.resolveType(keyType)
	valLLVM := c.resolveType(valType)

	// Alloca key
	keyAlloca := c.block.NewAlloca(keyLLVM)
	c.block.NewStore(keyVal, keyAlloca)
	keyPtr := c.block.NewBitCast(keyAlloca, irtypes.I8Ptr)

	// Get current value
	resultPtr := c.block.NewCall(c.funcs["promise_map_get"], mapPtr, keyPtr)
	isNull := c.block.NewICmp(enum.IPredEQ, resultPtr, constant.NewNull(irtypes.I8Ptr))
	okBlock := c.newBlock("mapcomp.ok")
	panicBlock := c.newBlock("mapcomp.panic")
	c.block.NewCondBr(isNull, panicBlock, okBlock)

	// Panic block: compound assignment requires existing key
	c.block = panicBlock
	panicMsg := c.makeGlobalString("compound assignment on missing map key")
	c.block.NewCall(c.funcs["promise_panic"], panicMsg)
	c.block.NewUnreachable()

	// OK: load current, apply op, store back
	c.block = okBlock
	typedPtr := c.block.NewBitCast(resultPtr, irtypes.NewPointer(valLLVM))
	current := c.block.NewLoad(valLLVM, typedPtr)
	result := c.genCompoundOp(op, current, val)

	// Store via promise_map_set
	valAlloca := c.block.NewAlloca(valLLVM)
	c.block.NewStore(result, valAlloca)
	valPtr := c.block.NewBitCast(valAlloca, irtypes.I8Ptr)
	c.block.NewCall(c.funcs["promise_map_set"], mapPtr, keyPtr, valPtr)
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

// --- For-in over slices ---

func (c *Compiler) genForInSlice(s *ast.ForInStmt, slicePtr value.Value, elemType types.Type) {
	elemLLVM := c.resolveType(elemType)

	// Load length from header
	headerType := sliceHeaderType()
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
	c.breakTarget = exitBlock
	c.continueTarget = updateBlock

	c.block = bodyBlock
	dataBase := c.block.NewGetElementPtr(irtypes.I8, slicePtr,
		constant.NewInt(irtypes.I64, int64(sliceHeaderSize)))
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
	c.block = exitBlock
}

// --- For-in over maps ---

func (c *Compiler) genForInMap(s *ast.ForInStmt, mapPtr value.Value, keyType, valType types.Type) {
	keyLLVM := c.resolveType(keyType)
	valLLVM := c.resolveType(valType)

	// Build tuple type for the binding (K, V)
	tupleType := irtypes.NewStruct(keyLLVM, valLLVM)

	// State alloca (i64, starts at 0)
	stateAlloca := c.block.NewAlloca(irtypes.I64)
	c.block.NewStore(constant.NewInt(irtypes.I64, 0), stateAlloca)

	// Key and value output allocas for iter_next
	keyOutAlloca := c.block.NewAlloca(keyLLVM)
	valOutAlloca := c.block.NewAlloca(valLLVM)

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

	// Header: call promise_map_iter_next
	c.block = headerBlock
	keyOutPtr := c.block.NewBitCast(keyOutAlloca, irtypes.I8Ptr)
	valOutPtr := c.block.NewBitCast(valOutAlloca, irtypes.I8Ptr)
	hasNext := c.block.NewCall(c.funcs["promise_map_iter_next"],
		mapPtr, stateAlloca, keyOutPtr, valOutPtr)
	cond := c.block.NewICmp(enum.IPredNE, hasNext, constant.NewInt(irtypes.I32, 0))
	c.block.NewCondBr(cond, bodyBlock, exitBlock)

	// Body: build tuple from key/val outputs, store to binding
	savedBreak := c.breakTarget
	savedContinue := c.continueTarget
	c.breakTarget = exitBlock
	c.continueTarget = updateBlock

	c.block = bodyBlock
	key := c.block.NewLoad(keyLLVM, keyOutAlloca)
	val := c.block.NewLoad(valLLVM, valOutAlloca)
	var tuple value.Value = constant.NewZeroInitializer(tupleType)
	tuple = c.block.NewInsertValue(tuple, key, 0)
	tuple = c.block.NewInsertValue(tuple, val, 1)
	c.block.NewStore(tuple, bindingAlloca)

	c.genBlock(s.Body)
	if c.block.Term == nil {
		c.block.NewBr(updateBlock)
	}

	// Update: increment index if present
	c.block = updateBlock
	if s.Index != "" {
		idxAlloca := c.locals[s.Index]
		curIdx := c.block.NewLoad(irtypes.I64, idxAlloca)
		nextIdx := c.block.NewAdd(curIdx, constant.NewInt(irtypes.I64, 1))
		c.block.NewStore(nextIdx, idxAlloca)
	}
	c.block.NewBr(headerBlock)

	c.breakTarget = savedBreak
	c.continueTarget = savedContinue
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
	c.breakTarget = exitBlock
	c.continueTarget = headerBlock

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
	c.block = exitBlock
}
