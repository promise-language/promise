package codegen

import (
	"fmt"

	"github.com/llir/llvm/ir"
	"github.com/llir/llvm/ir/constant"
	irtypes "github.com/llir/llvm/ir/types"
	"github.com/llir/llvm/ir/value"

	"github.com/promise-language/promise/compiler/internal/ast"
	"github.com/promise-language/promise/compiler/internal/types"
)

// declareFuncs creates LLVM function declarations for all FuncDecl nodes with bodies (pass 1).
// Generic functions (with TypeParams) are skipped — handled by declareMonoFuncs.
// Std functions get mangled LLVM names (__std_X) to avoid collisions with user functions.
func (c *Compiler) declareFuncs(file *ast.File) {
	for _, decl := range file.Decls {
		fd, ok := decl.(*ast.FuncDecl)
		if !ok {
			continue
		}
		if c.info.FilteredDecls[decl] {
			continue // excluded by `target(cond) annotation for this build target
		}
		isEmbed := c.info.Embeds[fd] != nil
		if fd.Body == nil && !isEmbed {
			continue // extern — already handled by declareExterns
		}
		if len(fd.TypeParams) > 0 {
			continue // generic — handled by monomorphization
		}

		// Module-level setters are stored under "name$set" in sema scope.
		scopeName := fd.Name
		if fd.IsSetter {
			scopeName = fd.Name + "$set"
		}

		obj := c.lookupFunc(scopeName)
		if obj == nil {
			continue
		}
		sig, ok := obj.Type().(*types.Signature)
		if !ok {
			continue
		}

		retType := irtypes.Type(irtypes.Void)
		genInfo := c.info.GeneratorFuncs[fd]
		if genInfo != nil {
			if genInfo.CanError {
				retType = computeResultType(failableGeneratorValueType())
			} else {
				retType = generatorValueType()
			}
		} else if sig.Result() != nil {
			retType = c.resolveType(sig.Result())
		}
		if sig.CanError() && genInfo == nil {
			retType = computeResultType(retType)
		}

		var params []*ir.Param
		for _, p := range sig.Params() {
			params = append(params, ir.NewParam(p.Name(), c.resolveParamType(p)))
		}

		// C ABI requires main to return i32 and receive argc/argv
		if fd.Name == "main" {
			retType = irtypes.I32
			params = []*ir.Param{
				ir.NewParam("argc", irtypes.I32),
				ir.NewParam("argv", irtypes.NewPointer(irtypes.I8Ptr)),
			}
		}

		// User functions (except main) get a __user. prefix in LLVM IR to prevent
		// collisions with PAL/libc extern symbols (B0319). The dot in __user. is
		// invalid in C identifiers, structurally preventing any libc collision.
		// The c.funcs map key remains the Promise-level name for internal lookups.
		llvmName := scopeName
		if fd.Name != "main" {
			llvmName = "__user." + scopeName
		}

		fn := c.module.NewFunc(llvmName, retType, params...)
		c.funcs[scopeName] = fn
	}
}

// defineFuncs generates function bodies for all FuncDecl nodes with bodies (pass 2).
// Generic functions (with TypeParams) are skipped — handled by defineMonoFuncs.
func (c *Compiler) defineFuncs(file *ast.File) {
	// T0262: Build set of test function names for WASM coroutine compilation.
	// On WASM, test function bodies are compiled as coroutines in GenerateTestMain
	// (with inCoroutine=true) so channel ops use cooperative scheduling instead of
	// thread-blocking pal_cond_wait which deadlocks on single-threaded WASM.
	var wasmTestFuncs map[string]bool
	if c.isWasm && len(c.info.Tests) > 0 {
		wasmTestFuncs = make(map[string]bool, len(c.info.Tests))
		for _, t := range c.info.Tests {
			wasmTestFuncs[t.Name()] = true
		}
		if c.testDecls == nil {
			c.testDecls = make(map[string]*ast.FuncDecl, len(c.info.Tests))
		}
	}

	for _, decl := range file.Decls {
		fd, ok := decl.(*ast.FuncDecl)
		if !ok {
			continue
		}
		if c.info.FilteredDecls[decl] {
			continue // excluded by `target(cond) annotation for this build target
		}
		// Handle embed getters — body is nil but we generate it from embedded data
		if embedInfo := c.info.Embeds[fd]; embedInfo != nil {
			fn := c.funcs[fd.Name]
			if fn != nil {
				c.defineEmbedGetter(fd, fn, embedInfo)
			}
			continue
		}
		if fd.Body == nil {
			continue // native function — no body to generate
		}
		if len(fd.TypeParams) > 0 {
			continue // generic — handled by monomorphization
		}
		// T0262: On WASM, skip test function body compilation — bodies are compiled
		// as coroutines in GenerateTestMain for cooperative scheduling.
		if wasmTestFuncs != nil && wasmTestFuncs[fd.Name] {
			c.testDecls[fd.Name] = fd
			continue
		}
		// Skip main — its body is compiled inline inside .goroutine.main
		// by wrapMainWithScheduler (with inCoroutine=true for proper channel ops).
		if fd.Name == "main" {
			c.mainDecl = fd
			continue
		}
		// Module-level setters are stored under "name$set" in sema scope.
		scopeName := fd.Name
		if fd.IsSetter {
			scopeName = fd.Name + "$set"
		}
		fn := c.funcs[scopeName]
		if fn == nil {
			continue
		}
		// Generator functions get special compilation
		if genInfo := c.info.GeneratorFuncs[fd]; genInfo != nil {
			c.defineGeneratorFunc(fd, fn, genInfo.ElemType)
			continue
		}
		c.defineFunc(fd, fn)
	}
}

// defineFunc generates the body of a single function.
func (c *Compiler) defineFunc(fd *ast.FuncDecl, fn *ir.Func) {
	c.fn = fn
	c.locals = make(map[string]*ir.InstAlloca)
	c.localNameCount = make(map[string]int)
	c.dropFlags = make(map[string]*ir.InstAlloca)
	c.castSubjectMatch = nil
	c.dropBindings = make(map[string]scopeBinding)
	c.stmtTemps = nil                         // T0073
	c.stmtTempMap = make(map[value.Value]int) // T0073
	c.heapTemps = nil                         // T0088
	c.heapTempMap = make(map[value.Value]int) // T0088
	c.envTemps = nil                          // T0100
	c.envTempMap = make(map[value.Value]int)  // T0100
	// T0084: Enable temp tracking for all free functions (user and module).
	// Previously limited to user code only (T0073); now extended to module code
	// so that statement-level string temps in module methods (e.g., this.to_string()
	// inside format() methods) are cleaned up.
	c.tempTrackingEnabled = true
	c.mutRefPtrs = nil
	c.mutRefTypes = nil
	c.scopeBindings = nil // T0085: reset scope bindings for each new function
	c.loopScopeDepth = 0
	c.loopTempFloor = [4]int{} // T1331
	c.blockCounter = 0
	c.enumCtorTemps = nil        // B0267: prevent cross-function alloca leak
	c.matchBorrowedIdents = nil  // T0485: clear cross-function stale entries
	c.borrowOptionalLocals = nil // T1085: clear cross-function stale entries
	c.currentOpValueParams = nil // T0897: free functions are never operators

	entry := fn.NewBlock(".entry")
	c.block = entry
	c.entryBlock = entry

	// Allocate parameters and store incoming values
	obj := c.lookupFunc(fd.Name)
	if obj == nil {
		return
	}
	sig, ok := obj.Type().(*types.Signature)
	if !ok {
		return
	}
	c.canError = sig.CanError()
	c.currentRetType = sig.Result()
	c.setBorrowedValueParams(sig) // T0945

	for i, p := range sig.Params() {
		if p.Name() == "" || p.Name() == "_" {
			continue
		}
		if _, isMutRef := p.Type().(*types.MutRef); isMutRef {
			// MutRef param: caller passes a pointer to its alloca.
			// The param value IS a pointer to the caller's storage —
			// reads load through it, writes store through it.
			innerType := c.resolveType(p.Type())
			if c.mutRefPtrs == nil {
				c.mutRefPtrs = make(map[string]value.Value)
				c.mutRefTypes = make(map[string]irtypes.Type)
			}
			c.mutRefPtrs[p.Name()] = fn.Params[i]
			c.mutRefTypes[p.Name()] = innerType
		} else {
			alloca := entry.NewAlloca(c.resolveType(p.Type()))
			alloca.SetName(c.uniqueLocalName(p.Name() + ".addr"))
			entry.NewStore(fn.Params[i], alloca)
			c.locals[p.Name()] = alloca

			// T0087: Register drop binding for ~ (move) parameters.
			// The callee takes ownership and must drop at scope exit.
			if p.Ref() == types.RefMut {
				paramType := p.Type()
				if c.typeSubst != nil {
					paramType = types.Substitute(paramType, c.typeSubst)
				}
				c.maybeRegisterDrop(p.Name(), alloca, paramType)
				c.maybeRegisterStructuralParamFree(p.Name(), alloca, paramType) // T0861
				// T1237: a closure-typed `move` param owns its heap env — register an
				// owning bindingFreeEnv (like a closure local) so it is freed at scope
				// exit. Moving the closure out (into a local/field/container/return)
				// clears this binding's drop flag, transferring ownership. The caller
				// transfers ownership by claiming the env temp / clearing the source
				// var's flag at the call site (genCallArgsWithMutRef).
				c.maybeRegisterEnvFree(p.Name(), alloca, paramType, nil)
			}

			// B0191: Register drop binding for variadic parameters.
			// Variadic params receive an owned Vector[T] that must be freed at scope exit.
			if p.IsVariadic() {
				paramType := p.Type()
				if c.typeSubst != nil {
					paramType = types.Substitute(paramType, c.typeSubst)
				}
				c.maybeRegisterDrop(p.Name(), alloca, paramType)
			}

			// T1233: A plain (non-`move`) tuple-by-value param BORROWS — the
			// ownership checker permits the caller to reuse the tuple after the
			// call, so a callee-side drop here would double-free (a hard crash for
			// a closure env, silent UB for strings/vectors). The caller owns the
			// tuple: an owned tuple variable via its own bindingDropTuple, a tuple
			// literal/temp via a caller-side statement temp (registerTupleStmtTemp
			// in genCallArgsWithMutRef). Supersedes T0406's callee-drop.

			// T1194: a borrow-by-default heap param reassigned to a fresh owned
			// value inside the body needs a function-scoped drop obligation (flag
			// starts at 0 — caller owns the original). No-op unless reassigned.
			paramType := p.Type()
			if c.typeSubst != nil {
				paramType = types.Substitute(paramType, c.typeSubst)
			}
			c.maybeRegisterBorrowParamReassignDrop(p.Name(), alloca, paramType, p.Ref(), fd.Body)
		}
	}

	// Coverage: instrument function entry (skip test functions and main)
	if c.shouldInstrument() && !c.isTestFunc(fd.Name) && fd.Name != "main" {
		pos := fd.Pos()
		end := fd.End()
		idx := c.addCoverageRegion(pos.File, pos.Line, end.Line, fd.Name, "function")
		c.emitCoverageIncrement(idx)
	}

	c.genBlock(fd.Body)

	// Ensure the function ends with a terminator
	if c.block != nil && c.block.Term == nil {
		// T0087: Clean up ~ parameter drop bindings on implicit return
		if len(c.scopeBindings) > 0 {
			c.emitScopeCleanup(0, false)
		}
		if c.canError {
			resultType := c.currentResultType()
			if isVoidResult(resultType) {
				c.block.NewRet(c.wrapOk(nil, resultType))
			} else {
				c.block.NewRet(c.wrapOk(c.zeroValue(resultType.Fields[1]), resultType))
			}
		} else if _, ok := fn.Sig.RetType.(*irtypes.VoidType); ok {
			c.block.NewRet(nil)
		} else {
			c.block.NewRet(c.zeroValue(fn.Sig.RetType))
		}
	}
}

// lookupFunc finds a function object in sema info by name.
func (c *Compiler) lookupFunc(name string) *types.Func {
	// Walk all recorded scopes
	for _, scope := range c.info.ScopeOrder {
		if obj := scope.Lookup(name); obj != nil {
			if fn, ok := obj.(*types.Func); ok {
				return fn
			}
		}
	}
	return nil
}

// zeroValue returns the zero/default value for an LLVM type.
func (c *Compiler) zeroValue(typ irtypes.Type) constant.Constant {
	switch t := typ.(type) {
	case *irtypes.IntType:
		return constant.NewInt(t, 0)
	case *irtypes.FloatType:
		return constant.NewFloat(t, 0.0)
	case *irtypes.PointerType:
		return constant.NewNull(t)
	case *irtypes.StructType:
		return constant.NewZeroInitializer(t)
	case *irtypes.ArrayType:
		// T1172: a fixed-array return type ([N x T]) needs a zero aggregate, not
		// the i64-0 default — otherwise the panic-cleanup return (emitPanicReturn)
		// emits `ret i64 0` in an array-returning function → malformed IR.
		return constant.NewZeroInitializer(t)
	case *irtypes.VectorType:
		return constant.NewZeroInitializer(t)
	default:
		return constant.NewInt(irtypes.I64, 0)
	}
}

// currentResultType returns the result struct type of the current failable function.
func (c *Compiler) currentResultType() *irtypes.StructType {
	return c.fn.Sig.RetType.(*irtypes.StructType)
}

// wrapOk builds an Ok result struct: { false, val, null } or { false, null } for void.
func (c *Compiler) wrapOk(val value.Value, resultType *irtypes.StructType) value.Value {
	var agg value.Value = constant.NewUndef(resultType)
	agg = c.block.NewInsertValue(agg, constant.NewInt(irtypes.I1, 0), 0)
	if isVoidResult(resultType) {
		agg = c.block.NewInsertValue(agg, constant.NewNull(irtypes.I8Ptr), 1)
	} else {
		agg = c.block.NewInsertValue(agg, val, 1)
		agg = c.block.NewInsertValue(agg, constant.NewNull(irtypes.I8Ptr), 2)
	}
	return agg
}

// wrapError builds an Error result struct: { true, zero, errVal } or { true, errVal } for void.
func (c *Compiler) wrapError(errVal value.Value, resultType *irtypes.StructType) value.Value {
	var agg value.Value = constant.NewUndef(resultType)
	agg = c.block.NewInsertValue(agg, constant.NewInt(irtypes.I1, 1), 0)
	if isVoidResult(resultType) {
		agg = c.block.NewInsertValue(agg, errVal, 1)
	} else {
		agg = c.block.NewInsertValue(agg, c.zeroValue(resultType.Fields[1]), 1)
		agg = c.block.NewInsertValue(agg, errVal, 2)
	}
	return agg
}

// newBlock creates a new basic block in the current function.
func (c *Compiler) newBlock(name string) *ir.Block {
	c.blockCounter++
	return c.fn.NewBlock(fmt.Sprintf("%s.%d", name, c.blockCounter))
}

// createEntryAlloca creates an alloca in the function's entry block.
// This ensures the alloca dominates all uses, which is required by LLVM's
// verifier across all versions. The entry block dominates every block in
// the function, so allocas placed here are always valid.
func (c *Compiler) createEntryAlloca(elemType irtypes.Type) *ir.InstAlloca {
	return c.entryBlock.NewAlloca(elemType)
}

// mangleModuleFuncName produces a module-qualified LLVM function name.
func mangleModuleFuncName(moduleName, funcName string) string {
	return "__mod_" + moduleName + "_" + funcName
}

// mangleModuleMethodName produces a module-qualified LLVM method name.
func mangleModuleMethodName(moduleName, typeName, methodName string, isSetter bool) string {
	base := mangleMethodName(typeName, methodName, isSetter)
	return "__mod_" + moduleName + "_" + base
}
