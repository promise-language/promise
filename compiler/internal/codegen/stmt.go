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
	case *ast.Block:
		c.genBlock(s)
	default:
		panic(fmt.Sprintf("codegen: unhandled statement type %T", stmt))
	}
}

// --- Variable declarations ---

func (c *Compiler) genTypedVarDecl(s *ast.TypedVarDecl) {
	typ := c.info.Types[s.Value]
	lt := c.resolveType(typ)
	alloca := c.block.NewAlloca(lt)
	alloca.SetName(s.Name)
	val := c.genExpr(s.Value)
	c.block.NewStore(val, alloca)
	c.locals[s.Name] = alloca
}

func (c *Compiler) genInferredVarDecl(s *ast.InferredVarDecl) {
	typ := c.info.Types[s.Value]
	lt := c.resolveType(typ)
	alloca := c.block.NewAlloca(lt)
	alloca.SetName(s.Name)
	val := c.genExpr(s.Value)
	c.block.NewStore(val, alloca)
	c.locals[s.Name] = alloca
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
			c.block.NewStore(val, alloca)
			return
		}
		// Compound assignment: load current value, apply operator, store result
		current := c.block.NewLoad(alloca.ElemType, alloca)
		result := c.genCompoundOp(s.Op, current, val)
		c.block.NewStore(result, alloca)

	case *ast.MemberExpr:
		c.genMemberAssign(target, s.Op, val)

	default:
		panic(fmt.Sprintf("codegen: unsupported assignment target %T", s.Target))
	}
}

// genMemberAssign handles assignment to a field on a user type instance.
func (c *Compiler) genMemberAssign(target *ast.MemberExpr, op ast.AssignOp, val value.Value) {
	targetType := c.info.Types[target.Target]
	named := extractNamed(targetType)
	if named == nil {
		panic("codegen: cannot resolve type for member assignment")
	}

	layout := c.layouts[named]
	if layout == nil {
		panic(fmt.Sprintf("codegen: no layout for type %s", named))
	}

	field := named.LookupField(target.Field)
	if field == nil {
		panic(fmt.Sprintf("codegen: no field %s on type %s", target.Field, named))
	}

	fieldIdx, ok := layout.InstanceFieldIndex[field.Name()]
	if !ok {
		panic(fmt.Sprintf("codegen: field %s not in layout for %s", field.Name(), named))
	}

	// Get pointer to the instance
	obj := c.genExpr(target.Target)
	typedPtr := c.block.NewBitCast(obj, layout.InstancePtrType)

	// GEP to the field
	fieldPtr := c.block.NewGetElementPtr(layout.Instance.LLVMType, typedPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(fieldIdx)))

	if op == ast.OpAssign {
		c.block.NewStore(val, fieldPtr)
		return
	}

	// Compound assignment: load, compute, store
	fieldLLVMType := llvmType(field.Type())
	current := c.block.NewLoad(fieldLLVMType, fieldPtr)
	result := c.genCompoundOp(op, current, val)
	c.block.NewStore(result, fieldPtr)
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
	if s.Value == nil {
		c.block.NewRet(nil)
	} else {
		val := c.genExpr(s.Value)
		c.block.NewRet(val)
	}
}

// --- If statement ---

func (c *Compiler) genIfStmt(s *ast.IfStmt) {
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

// --- For-in loop (range iteration) ---

func (c *Compiler) genForInStmt(s *ast.ForInStmt) {
	// Generate the range value
	rangeVal := c.genExpr(s.Iterable)

	// Extract fields: start, end, inclusive
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

	// Allocate loop counter (the binding variable)
	counterAlloca := c.block.NewAlloca(irtypes.I64)
	counterAlloca.SetName(s.Binding)
	c.block.NewStore(start, counterAlloca)
	c.locals[s.Binding] = counterAlloca

	// Allocate index variable if present
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

	// Header: check counter against end
	c.block = headerBlock
	counter := c.block.NewLoad(irtypes.I64, counterAlloca)
	// For exclusive range (..): counter < end
	// For inclusive range (..=): counter <= end
	ltCond := c.block.NewICmp(enum.IPredSLT, counter, end)
	eqCond := c.block.NewICmp(enum.IPredEQ, counter, end)
	inclAndEq := c.block.NewAnd(inclusive, eqCond)
	cond := c.block.NewOr(ltCond, inclAndEq)
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
