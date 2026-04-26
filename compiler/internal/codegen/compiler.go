package codegen

import (
	"fmt"

	"github.com/llir/llvm/ir"
	"github.com/llir/llvm/ir/constant"
	irtypes "github.com/llir/llvm/ir/types"
	"github.com/llir/llvm/ir/value"

	"djabi.dev/go/promise_lang/internal/ast"
	"djabi.dev/go/promise_lang/internal/sema"
	"djabi.dev/go/promise_lang/internal/types"
)

// Compiler generates LLVM IR from a type-checked Promise AST.
type Compiler struct {
	module  *ir.Module
	info    *sema.Info
	fn      *ir.Func                         // current function being generated
	block   *ir.Block                        // current basic block
	locals  map[string]*ir.InstAlloca        // local variable allocas
	funcs   map[string]*ir.Func              // declared Promise functions by name
	layouts map[*types.Named]*TypeDeclLayout // type layouts for extern ABI
	externs map[string]*ExternFunc           // extern functions by Promise name

	// Loop control targets for break/continue
	breakTarget    *ir.Block
	continueTarget *ir.Block

	// String literal counter for unique global names
	strCounter int
}

// Compile generates an LLVM IR module from a type-checked Promise AST.
func Compile(file *ast.File, info *sema.Info) *CompileResult {
	c := &Compiler{
		module: ir.NewModule(),
		info:   info,
		funcs:  make(map[string]*ir.Func),
	}

	// Collect extern declarations and compute type layouts
	externList := collectExterns(file, info)
	c.layouts = computeLayouts(c.module, externList)

	// Build externs map by Promise name
	c.externs = make(map[string]*ExternFunc, len(externList))
	for _, ext := range externList {
		c.externs[ext.PromiseName] = ext
	}

	// Compute user type layouts (after built-in layouts are ready)
	c.computeUserTypeLayouts(file)

	c.declareIntrinsics()
	// declareExterns must run after computeUserTypeLayouts so that user type
	// layouts are available when resolving extern parameter/return types.
	c.declareExterns(externList, c.layouts)
	c.declareTypeMethods(file)
	c.declareFuncs(file)
	c.defineTypeMethods(file)
	c.defineFuncs(file)

	return &CompileResult{
		Module:  c.module,
		Layouts: c.layouts,
		Externs: externList,
	}
}

// declareIntrinsics declares compiler-intrinsic runtime functions (not user-declared externs).
func (c *Compiler) declareIntrinsics() {
	c.funcs["promise_panic"] = c.module.NewFunc("promise_panic",
		irtypes.Void, ir.NewParam("msg", irtypes.I8Ptr))

	// malloc for heap allocation of user type instances
	c.funcs["malloc"] = c.module.NewFunc("malloc",
		irtypes.I8Ptr, ir.NewParam("size", irtypes.I64))

	// String intrinsics — declared with i8* params/returns for internal use.
	// The C implementations (runtime_string.c) use typed promise_string_i* pointers.
	// This is ABI-compatible since all pointers are the same size; the type mismatch
	// is invisible at the linker level and avoids threading struct types through llvmNamedType.
	c.funcs["promise_string_new"] = c.module.NewFunc("promise_string_new",
		irtypes.I8Ptr,
		ir.NewParam("data", irtypes.I8Ptr),
		ir.NewParam("len", irtypes.I64))

	c.funcs["promise_string_concat"] = c.module.NewFunc("promise_string_concat",
		irtypes.I8Ptr,
		ir.NewParam("a", irtypes.I8Ptr),
		ir.NewParam("b", irtypes.I8Ptr))

	c.funcs["promise_string_eq"] = c.module.NewFunc("promise_string_eq",
		irtypes.I1,
		ir.NewParam("a", irtypes.I8Ptr),
		ir.NewParam("b", irtypes.I8Ptr))
}

// declareFuncs creates LLVM function declarations for all FuncDecl nodes with bodies (pass 1).
func (c *Compiler) declareFuncs(file *ast.File) {
	for _, decl := range file.Decls {
		fd, ok := decl.(*ast.FuncDecl)
		if !ok {
			continue
		}
		if fd.Body == nil {
			continue // extern — already handled by declareExterns
		}

		obj := c.lookupFunc(fd.Name)
		if obj == nil {
			continue
		}
		sig, ok := obj.Type().(*types.Signature)
		if !ok {
			continue
		}

		retType := irtypes.Type(irtypes.Void)
		if sig.Result() != nil {
			retType = llvmType(sig.Result())
		}

		var params []*ir.Param
		for _, p := range sig.Params() {
			params = append(params, ir.NewParam(p.Name(), llvmType(p.Type())))
		}

		// C ABI requires main to return i32
		if fd.Name == "main" {
			retType = irtypes.I32
		}

		fn := c.module.NewFunc(fd.Name, retType, params...)
		c.funcs[fd.Name] = fn
	}
}

// defineFuncs generates function bodies for all FuncDecl nodes with bodies (pass 2).
func (c *Compiler) defineFuncs(file *ast.File) {
	for _, decl := range file.Decls {
		fd, ok := decl.(*ast.FuncDecl)
		if !ok {
			continue
		}
		if fd.Body == nil {
			continue // native function — no body to generate
		}
		fn, ok := c.funcs[fd.Name]
		if !ok {
			continue
		}
		c.defineFunc(fd, fn)
	}
}

// defineFunc generates the body of a single function.
func (c *Compiler) defineFunc(fd *ast.FuncDecl, fn *ir.Func) {
	c.fn = fn
	c.locals = make(map[string]*ir.InstAlloca)

	entry := fn.NewBlock("entry")
	c.block = entry

	// Allocate parameters and store incoming values
	obj := c.lookupFunc(fd.Name)
	if obj == nil {
		return
	}
	sig, ok := obj.Type().(*types.Signature)
	if !ok {
		return
	}
	for i, p := range sig.Params() {
		if p.Name() == "" || p.Name() == "_" {
			continue
		}
		alloca := entry.NewAlloca(llvmType(p.Type()))
		alloca.SetName(p.Name() + ".addr")
		entry.NewStore(fn.Params[i], alloca)
		c.locals[p.Name()] = alloca
	}

	c.genBlock(fd.Body)

	// Ensure the function ends with a terminator
	if c.block != nil && c.block.Term == nil {
		if _, ok := fn.Sig.RetType.(*irtypes.VoidType); ok {
			c.block.NewRet(nil)
		} else {
			// Return zero value for non-void functions missing a return
			c.block.NewRet(c.zeroValue(fn.Sig.RetType))
		}
	}
}

// lookupFunc finds a function object in sema info by name.
func (c *Compiler) lookupFunc(name string) *types.Func {
	// Walk the file scope looking for the function
	for _, scope := range c.info.Scopes {
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
	default:
		return constant.NewInt(irtypes.I64, 0)
	}
}

// newBlock creates a new basic block in the current function.
func (c *Compiler) newBlock(name string) *ir.Block {
	return c.fn.NewBlock(name)
}

// computeUserTypeLayouts computes layouts for all user-declared types in the file.
func (c *Compiler) computeUserTypeLayouts(file *ast.File) {
	for _, decl := range file.Decls {
		td, ok := decl.(*ast.TypeDecl)
		if !ok {
			continue
		}
		named := c.lookupNamedType(td.Name)
		if named == nil {
			continue
		}
		if _, exists := c.layouts[named]; exists {
			continue // skip built-in types
		}
		c.layouts[named] = computeUserTypeLayout(c.module, named, c.layouts)
	}
}

// declareTypeMethods creates LLVM function stubs for all methods with bodies (pass 1).
func (c *Compiler) declareTypeMethods(file *ast.File) {
	for _, decl := range file.Decls {
		td, ok := decl.(*ast.TypeDecl)
		if !ok {
			continue
		}
		named := c.lookupNamedType(td.Name)
		if named == nil {
			continue
		}

		for _, md := range td.Methods {
			if md.Body == nil {
				continue // abstract or native
			}
			m := named.LookupMethod(md.Name)
			if m == nil || m.Sig() == nil {
				continue
			}

			mangledName := td.Name + "." + md.Name

			var params []*ir.Param
			if m.Sig().Recv() != nil {
				params = append(params, ir.NewParam("this", irtypes.I8Ptr))
			}
			for _, p := range m.Sig().Params() {
				params = append(params, ir.NewParam(p.Name(), llvmType(p.Type())))
			}

			retType := irtypes.Type(irtypes.Void)
			if m.Sig().Result() != nil {
				retType = llvmType(m.Sig().Result())
			}

			fn := c.module.NewFunc(mangledName, retType, params...)
			c.funcs[mangledName] = fn
		}
	}
}

// defineTypeMethods generates method bodies (pass 2).
func (c *Compiler) defineTypeMethods(file *ast.File) {
	for _, decl := range file.Decls {
		td, ok := decl.(*ast.TypeDecl)
		if !ok {
			continue
		}
		named := c.lookupNamedType(td.Name)
		if named == nil {
			continue
		}

		for _, md := range td.Methods {
			if md.Body == nil {
				continue
			}
			m := named.LookupMethod(md.Name)
			if m == nil || m.Sig() == nil {
				continue
			}

			mangledName := td.Name + "." + md.Name
			fn, ok := c.funcs[mangledName]
			if !ok {
				continue
			}

			c.defineMethodFunc(md, m, fn)
		}
	}
}

// defineMethodFunc generates the body of a single method.
func (c *Compiler) defineMethodFunc(md *ast.MethodDecl, m *types.Method, fn *ir.Func) {
	c.fn = fn
	c.locals = make(map[string]*ir.InstAlloca)

	entry := fn.NewBlock("entry")
	c.block = entry

	paramIdx := 0

	// Allocate receiver as "this"
	if m.Sig().Recv() != nil {
		alloca := entry.NewAlloca(irtypes.I8Ptr)
		alloca.SetName("this.addr")
		entry.NewStore(fn.Params[paramIdx], alloca)
		c.locals["this"] = alloca
		paramIdx++
	}

	// Allocate regular parameters
	for _, p := range m.Sig().Params() {
		if p.Name() == "" || p.Name() == "_" {
			paramIdx++
			continue
		}
		lt := llvmType(p.Type())
		alloca := entry.NewAlloca(lt)
		alloca.SetName(p.Name() + ".addr")
		entry.NewStore(fn.Params[paramIdx], alloca)
		c.locals[p.Name()] = alloca
		paramIdx++
	}

	c.genBlock(md.Body)

	// Ensure the function ends with a terminator
	if c.block != nil && c.block.Term == nil {
		if _, ok := fn.Sig.RetType.(*irtypes.VoidType); ok {
			c.block.NewRet(nil)
		} else {
			c.block.NewRet(c.zeroValue(fn.Sig.RetType))
		}
	}
}

// lookupNamedType finds a Named type in sema info by name.
func (c *Compiler) lookupNamedType(name string) *types.Named {
	for _, scope := range c.info.Scopes {
		if obj := scope.Lookup(name); obj != nil {
			if tn, ok := obj.(*types.TypeName); ok {
				if named, ok := tn.Type().(*types.Named); ok {
					return named
				}
			}
		}
	}
	return nil
}

// emitNativeOp dispatches a native operator to the LLVM instruction table.
// right is nil for unary operations.
func (c *Compiler) emitNativeOp(named *types.Named, op string, left, right value.Value) value.Value {
	cat := classify(named)
	if cat == CatUnknown {
		panic(fmt.Sprintf("codegen: native method %q on non-primitive type %s", op, named))
	}
	emitter := lookupNativeOp(cat, op)
	if emitter == nil {
		panic(fmt.Sprintf("codegen: no native emitter for %s.%s", named, op))
	}
	return emitter(c.block, left, right)
}
