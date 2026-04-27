package codegen

import (
	"fmt"
	"runtime"

	"github.com/llir/llvm/ir"
	"github.com/llir/llvm/ir/constant"
	"github.com/llir/llvm/ir/enum"
	irtypes "github.com/llir/llvm/ir/types"
	"github.com/llir/llvm/ir/value"

	"djabi.dev/go/promise_lang/internal/ast"
	"djabi.dev/go/promise_lang/internal/sema"
	"djabi.dev/go/promise_lang/internal/types"
)

// Compiler generates LLVM IR from a type-checked Promise AST.
type Compiler struct {
	module      *ir.Module
	info        *sema.Info
	fn          *ir.Func                         // current function being generated
	block       *ir.Block                        // current basic block
	locals      map[string]*ir.InstAlloca        // local variable allocas
	funcs       map[string]*ir.Func              // declared Promise functions by name
	stdFuncs    map[string]*ir.Func              // std library functions by name (for std.X)
	stdExterns  map[string]*ExternFunc           // std library externs by name (for std.X)
	layouts     map[*types.Named]*TypeDeclLayout // type layouts for extern ABI
	enumLayouts map[*types.Enum]*TypeDeclLayout  // enum type layouts
	externs     map[string]*ExternFunc           // extern functions by Promise name

	// Monomorphization state
	monoLayouts     map[string]*TypeDeclLayout      // mono name → layout (user types)
	monoEnumLayouts map[string]*TypeDeclLayout      // mono name → layout (enums)
	typeSubst       map[*types.TypeParam]types.Type // nil outside mono codegen
	monoCtx         *monoContext                    // nil outside mono method codegen

	// Loop control targets for break/continue
	breakTarget    *ir.Block
	continueTarget *ir.Block

	// Error handling: true if current function is failable (returns result struct)
	canError bool

	// Return type of the current function/method (Promise-level, for coercion)
	currentRetType types.Type

	// Current type being compiled (set during method body generation)
	currentNamed *types.Named

	// String literal counter for unique global names
	strCounter int

	// Lambda counter for unique anonymous function names
	lambdaCounter int

	// Thunks for named function references used as first-class values.
	// Maps original function name to a wrapper with env-first ABI.
	thunks map[string]*ir.Func

	// Block counter for unique basic block names within a function
	blockCounter int

	// Target type for contextual type resolution (e.g., NoneLit needs Optional(T))
	targetType types.Type

	// RTTI: type ID assignment for Named types
	typeIDs         map[*types.Named]int32
	nextTypeID      int32
	typeInfoGlobals map[*types.Named]*ir.Global

	// VTable state
	hasChildren   map[*types.Named]bool        // true if any type declares `is ThisType`
	vtableGlobals map[*types.Named]*ir.Global  // type → @promise_vtable_TypeName
	viewVtables   map[viewVtableKey]*ir.Global // (concrete, view) → view-specific vtable

	// Scope cleanup state: stack of active bindings for automatic close()/drop() at scope exit
	scopeBindings  []scopeBinding
	loopScopeDepth int // scopeBindings depth at loop entry (for break/continue cleanup)

	// Drop flag tracking: maps variable name to its drop flag alloca (i1)
	dropFlags map[string]*ir.InstAlloca
}

// scopeBindingKind distinguishes close() bindings (use) from drop() bindings.
type scopeBindingKind int

const (
	bindingClose   scopeBindingKind = iota // use-bound: call close() at scope exit
	bindingDrop                            // droppable: call drop() at scope exit
	bindingFreeEnv                         // closure env: free env pointer at scope exit
)

// scopeBinding tracks a variable that needs cleanup at scope exit.
type scopeBinding struct {
	kind      scopeBindingKind
	alloca    *ir.InstAlloca
	closeFunc *ir.Func       // direct dispatch for close() (nil if virtual)
	dropFunc  *ir.Func       // direct dispatch for drop() (nil if virtual)
	named     *types.Named   // for virtual dispatch
	valType   types.Type     // original Promise type
	dropFlag  *ir.InstAlloca // i1: true=should drop (nil for close bindings)
	varName   string         // variable name (for drop flag lookup)
}

// viewVtableKey identifies a view-specific vtable for a (concrete, view) pair.
type viewVtableKey struct {
	concrete *types.Named
	view     *types.Named
}

// hostTargetTriple returns the LLVM target triple for the host platform.
// Uses stable minimum deployment targets to avoid version mismatch warnings
// between IR modules and clang-compiled object files.
func HostTargetTriple() string {
	switch runtime.GOOS {
	case "darwin":
		if runtime.GOARCH == "arm64" {
			return "arm64-apple-macosx11.0.0"
		}
		return "x86_64-apple-macosx10.15.0"
	case "linux":
		if runtime.GOARCH == "arm64" {
			return "aarch64-unknown-linux-gnu"
		}
		return "x86_64-unknown-linux-gnu"
	default:
		return "x86_64-unknown-linux-gnu"
	}
}

// Compile generates an LLVM IR module from a type-checked Promise AST.
func Compile(file *ast.File, info *sema.Info) *CompileResult {
	module := ir.NewModule()
	module.TargetTriple = HostTargetTriple()

	c := &Compiler{
		module:          module,
		info:            info,
		funcs:           make(map[string]*ir.Func),
		stdFuncs:        make(map[string]*ir.Func),
		stdExterns:      make(map[string]*ExternFunc),
		monoLayouts:     make(map[string]*TypeDeclLayout),
		monoEnumLayouts: make(map[string]*TypeDeclLayout),
		typeIDs:         make(map[*types.Named]int32),
		nextTypeID:      1, // 0 reserved for "no type info"
		typeInfoGlobals: make(map[*types.Named]*ir.Global),
		hasChildren:     make(map[*types.Named]bool),
		vtableGlobals:   make(map[*types.Named]*ir.Global),
		viewVtables:     make(map[viewVtableKey]*ir.Global),
		dropFlags:       make(map[string]*ir.InstAlloca),
		thunks:          make(map[string]*ir.Func),
	}

	// Collect extern declarations and compute type layouts
	externList := collectExterns(file, info)
	c.layouts = computeLayouts(c.module, externList)

	// Build externs map by Promise name
	c.externs = make(map[string]*ExternFunc, len(externList))
	for _, ext := range externList {
		c.externs[ext.PromiseName] = ext
	}

	// Compute enum layouts (before user types, so enum fields resolve correctly)
	c.enumLayouts = make(map[*types.Enum]*TypeDeclLayout)
	c.computeEnumLayouts(file)

	// Compute user type layouts (after built-in and enum layouts are ready)
	c.computeUserTypeLayouts(file)

	// Compute monomorphic layouts for all concrete generic instantiations
	monoInstances := collectMonoInstances(info)
	c.computeMonoLayouts(monoInstances)
	monoFuncInstances := collectMonoFuncInstances(info)

	c.declareIntrinsics()
	// declareExterns must run after computeUserTypeLayouts so that user type
	// layouts are available when resolving extern parameter/return types.
	c.declareExterns(externList, c.layouts)

	// Declare method stubs before vtable/typeinfo emission (vtable needs function pointers)
	c.declareTypeMethods(file)
	c.declareMonoMethods(file, monoInstances)

	// Compute vtable info and emit vtable globals (after method stubs are declared)
	c.computeVtableInfo(file)
	c.emitVtableGlobals(file)

	// Emit RTTI type info globals (after vtable globals, since typeinfo includes vtable ptr)
	c.emitTypeInfoGlobals(file)

	c.declareFuncs(file)
	c.declareMonoFuncs(file, monoFuncInstances)
	c.defineTypeMethods(file)
	c.defineMonoMethods(file, monoInstances)
	c.defineFuncs(file)
	c.defineMonoFuncs(file, monoFuncInstances)

	return &CompileResult{
		Module:      c.module,
		Layouts:     c.layouts,
		EnumLayouts: c.enumLayouts,
		Externs:     externList,
		compiler:    c,
	}
}

// GenerateTestMain replaces the user's main() with a test runner that calls
// each `test function via promise_test_run for fork-based isolation.
func (r *CompileResult) GenerateTestMain(tests []*types.Func) {
	c := r.compiler

	// Declare test runner C functions
	testRunFn := c.module.NewFunc("promise_test_run",
		irtypes.I32,
		ir.NewParam("fn", irtypes.I8Ptr),
	)
	testPrintFn := c.module.NewFunc("promise_test_print_result",
		irtypes.Void,
		ir.NewParam("name", irtypes.I8Ptr),
		ir.NewParam("failed", irtypes.I32),
	)
	testSummaryFn := c.module.NewFunc("promise_test_summary",
		irtypes.Void,
		ir.NewParam("passed", irtypes.I32),
		ir.NewParam("failed", irtypes.I32),
	)

	// Remove existing main if present, then create test main
	// The existing main is already compiled. We replace it with a new one.
	mainFn := c.funcs["main"]
	if mainFn != nil {
		// Clear existing blocks
		mainFn.Blocks = nil
	} else {
		mainFn = c.module.NewFunc("main", irtypes.I32)
		c.funcs["main"] = mainFn
	}

	entry := mainFn.NewBlock("entry")

	// Allocate counters: passed and failed
	passedAlloca := entry.NewAlloca(irtypes.I32)
	failedAlloca := entry.NewAlloca(irtypes.I32)
	entry.NewStore(constant.NewInt(irtypes.I32, 0), passedAlloca)
	entry.NewStore(constant.NewInt(irtypes.I32, 0), failedAlloca)

	for _, test := range tests {
		// Look up the IR function — tests are always user code
		testFn := c.funcs[test.Name()]
		if testFn == nil {
			continue
		}

		// Create global string constant for the test name
		nameStr := test.Name()
		nameGlobal := c.module.NewGlobalDef(
			fmt.Sprintf(".test_name_%s", nameStr),
			constant.NewCharArrayFromString(nameStr+"\x00"),
		)
		nameGlobal.Immutable = true

		// Bitcast test function to i8* for promise_test_run
		fnPtr := entry.NewBitCast(testFn, irtypes.I8Ptr)

		// Call promise_test_run(fn) -> i32 (0=pass, 1=fail)
		result := entry.NewCall(testRunFn, fnPtr)

		// Get name pointer
		namePtr := entry.NewGetElementPtr(
			constant.NewCharArrayFromString(nameStr+"\x00").Typ,
			nameGlobal,
			constant.NewInt(irtypes.I64, 0),
			constant.NewInt(irtypes.I64, 0),
		)

		// Print result
		entry.NewCall(testPrintFn, namePtr, result)

		// Update counters
		currentPassed := entry.NewLoad(irtypes.I32, passedAlloca)
		currentFailed := entry.NewLoad(irtypes.I32, failedAlloca)

		// result == 0 means passed
		isPass := entry.NewICmp(enum.IPredEQ, result, constant.NewInt(irtypes.I32, 0))
		passIncr := entry.NewAdd(currentPassed, constant.NewInt(irtypes.I32, 1))
		failIncr := entry.NewAdd(currentFailed, constant.NewInt(irtypes.I32, 1))
		newPassed := entry.NewSelect(isPass, passIncr, currentPassed)
		newFailed := entry.NewSelect(isPass, currentFailed, failIncr)
		entry.NewStore(newPassed, passedAlloca)
		entry.NewStore(newFailed, failedAlloca)
	}

	// Print summary
	finalPassed := entry.NewLoad(irtypes.I32, passedAlloca)
	finalFailed := entry.NewLoad(irtypes.I32, failedAlloca)
	entry.NewCall(testSummaryFn, finalPassed, finalFailed)

	// Return 0 if all passed, 1 if any failed
	hasFailures := entry.NewICmp(enum.IPredSGT, finalFailed, constant.NewInt(irtypes.I32, 0))
	retVal := entry.NewSelect(hasFailures, constant.NewInt(irtypes.I32, 1), constant.NewInt(irtypes.I32, 0))
	entry.NewRet(retVal)
}

// declareIntrinsics declares compiler-intrinsic runtime functions (not user-declared externs).
func (c *Compiler) declareIntrinsics() {
	c.funcs["promise_panic"] = c.module.NewFunc("promise_panic",
		irtypes.Void, ir.NewParam("msg", irtypes.I8Ptr))

	// malloc/free for heap allocation
	c.funcs["malloc"] = c.module.NewFunc("malloc",
		irtypes.I8Ptr, ir.NewParam("size", irtypes.I64))
	c.funcs["free"] = c.module.NewFunc("free",
		irtypes.Void, ir.NewParam("ptr", irtypes.I8Ptr))

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

	// Map intrinsics — type-erased runtime hash table
	c.funcs["promise_map_new"] = c.module.NewFunc("promise_map_new",
		irtypes.I8Ptr,
		ir.NewParam("key_size", irtypes.I64),
		ir.NewParam("val_size", irtypes.I64),
		ir.NewParam("hash_fn", irtypes.I8Ptr),
		ir.NewParam("eq_fn", irtypes.I8Ptr))

	c.funcs["promise_map_set"] = c.module.NewFunc("promise_map_set",
		irtypes.Void,
		ir.NewParam("m", irtypes.I8Ptr),
		ir.NewParam("key", irtypes.I8Ptr),
		ir.NewParam("val", irtypes.I8Ptr))

	c.funcs["promise_map_get"] = c.module.NewFunc("promise_map_get",
		irtypes.I8Ptr,
		ir.NewParam("m", irtypes.I8Ptr),
		ir.NewParam("key", irtypes.I8Ptr))

	c.funcs["promise_map_len"] = c.module.NewFunc("promise_map_len",
		irtypes.I64,
		ir.NewParam("m", irtypes.I8Ptr))

	c.funcs["promise_map_iter_next"] = c.module.NewFunc("promise_map_iter_next",
		irtypes.I32,
		ir.NewParam("m", irtypes.I8Ptr),
		ir.NewParam("state", irtypes.NewPointer(irtypes.I64)),
		ir.NewParam("key_out", irtypes.I8Ptr),
		ir.NewParam("val_out", irtypes.I8Ptr))

	// Map methods
	c.funcs["promise_map_remove"] = c.module.NewFunc("promise_map_remove",
		irtypes.I32,
		ir.NewParam("m", irtypes.I8Ptr),
		ir.NewParam("key", irtypes.I8Ptr))

	c.funcs["promise_map_contains"] = c.module.NewFunc("promise_map_contains",
		irtypes.I8,
		ir.NewParam("m", irtypes.I8Ptr),
		ir.NewParam("key", irtypes.I8Ptr))

	c.funcs["promise_map_keys"] = c.module.NewFunc("promise_map_keys",
		irtypes.I8Ptr,
		ir.NewParam("m", irtypes.I8Ptr),
		ir.NewParam("key_size", irtypes.I64))

	c.funcs["promise_map_values"] = c.module.NewFunc("promise_map_values",
		irtypes.I8Ptr,
		ir.NewParam("m", irtypes.I8Ptr),
		ir.NewParam("val_size", irtypes.I64))

	// Slice methods
	c.funcs["promise_slice_push"] = c.module.NewFunc("promise_slice_push",
		irtypes.I8Ptr,
		ir.NewParam("slice", irtypes.I8Ptr),
		ir.NewParam("elem", irtypes.I8Ptr),
		ir.NewParam("elem_size", irtypes.I64))

	c.funcs["promise_slice_pop"] = c.module.NewFunc("promise_slice_pop",
		irtypes.I32,
		ir.NewParam("slice", irtypes.I8Ptr),
		ir.NewParam("out_elem", irtypes.I8Ptr),
		ir.NewParam("elem_size", irtypes.I64))

	c.funcs["promise_slice_contains"] = c.module.NewFunc("promise_slice_contains",
		irtypes.I8,
		ir.NewParam("slice", irtypes.I8Ptr),
		ir.NewParam("elem", irtypes.I8Ptr),
		ir.NewParam("elem_size", irtypes.I64),
		ir.NewParam("eq_fn", irtypes.I8Ptr))

	c.funcs["promise_slice_remove"] = c.module.NewFunc("promise_slice_remove",
		irtypes.Void,
		ir.NewParam("slice", irtypes.I8Ptr),
		ir.NewParam("index", irtypes.I64),
		ir.NewParam("elem_size", irtypes.I64))

	// String methods
	c.funcs["promise_string_contains"] = c.module.NewFunc("promise_string_contains",
		irtypes.I8,
		ir.NewParam("s", irtypes.I8Ptr),
		ir.NewParam("sub", irtypes.I8Ptr))

	c.funcs["promise_string_starts_with"] = c.module.NewFunc("promise_string_starts_with",
		irtypes.I8,
		ir.NewParam("s", irtypes.I8Ptr),
		ir.NewParam("prefix", irtypes.I8Ptr))

	c.funcs["promise_string_ends_with"] = c.module.NewFunc("promise_string_ends_with",
		irtypes.I8,
		ir.NewParam("s", irtypes.I8Ptr),
		ir.NewParam("suffix", irtypes.I8Ptr))

	c.funcs["promise_string_index_of"] = c.module.NewFunc("promise_string_index_of",
		irtypes.I64,
		ir.NewParam("s", irtypes.I8Ptr),
		ir.NewParam("sub", irtypes.I8Ptr))

	c.funcs["promise_string_trim"] = c.module.NewFunc("promise_string_trim",
		irtypes.I8Ptr,
		ir.NewParam("s", irtypes.I8Ptr))

	c.funcs["promise_string_split"] = c.module.NewFunc("promise_string_split",
		irtypes.I8Ptr,
		ir.NewParam("s", irtypes.I8Ptr),
		ir.NewParam("sep", irtypes.I8Ptr))

	// Realloc for slice growth
	c.funcs["realloc"] = c.module.NewFunc("realloc",
		irtypes.I8Ptr,
		ir.NewParam("ptr", irtypes.I8Ptr),
		ir.NewParam("size", irtypes.I64))

	// Value-to-string conversion for string interpolation
	c.funcs["promise_int_to_string"] = c.module.NewFunc("promise_int_to_string",
		irtypes.I8Ptr, ir.NewParam("x", irtypes.I64))

	c.funcs["promise_f64_to_string"] = c.module.NewFunc("promise_f64_to_string",
		irtypes.I8Ptr, ir.NewParam("x", irtypes.Double))

	c.funcs["promise_bool_to_string"] = c.module.NewFunc("promise_bool_to_string",
		irtypes.I8Ptr, ir.NewParam("x", irtypes.I8))

	c.funcs["promise_char_to_string"] = c.module.NewFunc("promise_char_to_string",
		irtypes.I8Ptr, ir.NewParam("cp", irtypes.I32))

	c.funcs["promise_string_next_char"] = c.module.NewFunc("promise_string_next_char",
		irtypes.I32,
		ir.NewParam("s", irtypes.I8Ptr),
		ir.NewParam("pos", irtypes.NewPointer(irtypes.I64)))

	// RTTI intrinsic for runtime type checking
	c.funcs["promise_type_is"] = c.module.NewFunc("promise_type_is",
		irtypes.I32,
		ir.NewParam("variant_ptr", irtypes.I8Ptr),
		ir.NewParam("expected_id", irtypes.I32))

	// String hash/eq for map keys (dereferences i8* to hash/compare content)
	c.funcs["promise_hash_string"] = c.module.NewFunc("promise_hash_string",
		irtypes.I64,
		ir.NewParam("key", irtypes.I8Ptr),
		ir.NewParam("key_size", irtypes.I64))

	c.funcs["promise_eq_string"] = c.module.NewFunc("promise_eq_string",
		irtypes.I32,
		ir.NewParam("a", irtypes.I8Ptr),
		ir.NewParam("b", irtypes.I8Ptr),
		ir.NewParam("key_size", irtypes.I64))
}

// declareFuncs creates LLVM function declarations for all FuncDecl nodes with bodies (pass 1).
// Generic functions (with TypeParams) are skipped — handled by declareMonoFuncs.
// Std functions get mangled LLVM names (__std_X) to avoid collisions with user functions.
func (c *Compiler) declareFuncs(file *ast.File) {
	for _, decl := range file.Decls {
		fd, ok := decl.(*ast.FuncDecl)
		if !ok {
			continue
		}
		if fd.Body == nil {
			continue // extern — already handled by declareExterns
		}
		if len(fd.TypeParams) > 0 {
			continue // generic — handled by monomorphization
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
			retType = c.resolveType(sig.Result())
		}
		if sig.CanError() {
			retType = computeResultType(retType)
		}

		var params []*ir.Param
		for _, p := range sig.Params() {
			params = append(params, ir.NewParam(p.Name(), c.resolveType(p.Type())))
		}

		// C ABI requires main to return i32 (overrides canError)
		if fd.Name == "main" {
			retType = irtypes.I32
		}

		// Std functions use mangled LLVM names to avoid collision with user functions
		irName := fd.Name
		if fd.IsStd {
			irName = "__std_" + fd.Name
		}

		fn := c.module.NewFunc(irName, retType, params...)

		if fd.IsStd {
			// Always register in stdFuncs for std.X access
			c.stdFuncs[fd.Name] = fn
			// Also register in funcs if no user function shadows it
			if _, shadowed := c.funcs[fd.Name]; !shadowed {
				c.funcs[fd.Name] = fn
			}
		} else {
			c.funcs[fd.Name] = fn
		}
	}
}

// defineFuncs generates function bodies for all FuncDecl nodes with bodies (pass 2).
// Generic functions (with TypeParams) are skipped — handled by defineMonoFuncs.
func (c *Compiler) defineFuncs(file *ast.File) {
	for _, decl := range file.Decls {
		fd, ok := decl.(*ast.FuncDecl)
		if !ok {
			continue
		}
		if fd.Body == nil {
			continue // native function — no body to generate
		}
		if len(fd.TypeParams) > 0 {
			continue // generic — handled by monomorphization
		}
		// Std functions use stdFuncs map; user functions use funcs map
		var fn *ir.Func
		if fd.IsStd {
			fn = c.stdFuncs[fd.Name]
		} else {
			fn = c.funcs[fd.Name]
		}
		if fn == nil {
			continue
		}
		c.defineFunc(fd, fn)
	}
}

// defineFunc generates the body of a single function.
func (c *Compiler) defineFunc(fd *ast.FuncDecl, fn *ir.Func) {
	c.fn = fn
	c.locals = make(map[string]*ir.InstAlloca)
	c.dropFlags = make(map[string]*ir.InstAlloca)
	c.blockCounter = 0

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
	c.canError = sig.CanError()
	c.currentRetType = sig.Result()

	for i, p := range sig.Params() {
		if p.Name() == "" || p.Name() == "_" {
			continue
		}
		alloca := entry.NewAlloca(c.resolveType(p.Type()))
		alloca.SetName(p.Name() + ".addr")
		entry.NewStore(fn.Params[i], alloca)
		c.locals[p.Name()] = alloca
	}

	c.genBlock(fd.Body)

	// Ensure the function ends with a terminator
	if c.block != nil && c.block.Term == nil {
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
	for _, scope := range c.info.Scopes {
		if obj := scope.Lookup(name); obj != nil {
			if fn, ok := obj.(*types.Func); ok {
				return fn
			}
		}
	}
	// Check std scope
	if c.info.StdScope != nil {
		if obj := c.info.StdScope.Lookup(name); obj != nil {
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

// computeUserTypeLayouts computes layouts for all user-declared types in the file.
// Generic types (with TypeParams) are skipped — they're handled by computeMonoLayouts.
// Uses topological ordering to ensure parent layouts are computed before children.
func (c *Compiler) computeUserTypeLayouts(file *ast.File) {
	// Collect all user type decls that need layouts
	pending := make(map[string]*types.Named)
	var names []string
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
			continue // skip built-in types with pre-computed layouts
		}
		if len(named.TypeParams()) > 0 {
			continue // generic — handled by monomorphization
		}
		if isNativeTypeDecl(td) {
			continue // native types have special codegen layout handling
		}
		pending[td.Name] = named
		names = append(names, td.Name)
	}

	// Compute layouts with dependency resolution (parents before children)
	computed := make(map[string]bool)
	var compute func(name string)
	compute = func(name string) {
		if computed[name] {
			return
		}
		named := pending[name]
		if named == nil {
			return
		}
		// Ensure parent layouts are computed first
		for _, p := range named.Parents() {
			pName := p.Obj().Name()
			if _, ok := pending[pName]; ok {
				compute(pName)
			}
		}
		c.layouts[named] = computeUserTypeLayout(c.module, named, c.layouts)
		computed[name] = true
	}
	for _, name := range names {
		compute(name)
	}
}

// declareTypeMethods creates LLVM function stubs for all methods with bodies (pass 1).
// Generic types are skipped — their methods are handled by declareMonoMethods.
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
		if len(named.TypeParams()) > 0 {
			continue // generic — handled by monomorphization
		}

		for _, md := range td.Methods {
			if md.Body == nil {
				continue // abstract or native
			}
			m := c.lookupAnyMethod(named, md.Name, md.IsGetter, md.IsSetter)
			if m == nil || m.Sig() == nil {
				continue
			}

			mangledName := mangleMethodName(td.Name, md.Name, md.IsSetter)

			var params []*ir.Param
			if m.Sig().Recv() != nil {
				params = append(params, ir.NewParam("this", irtypes.I8Ptr))
			}
			for _, p := range m.Sig().Params() {
				params = append(params, ir.NewParam(p.Name(), c.resolveType(p.Type())))
			}

			retType := irtypes.Type(irtypes.Void)
			if m.Sig().Result() != nil {
				retType = c.resolveType(m.Sig().Result())
			}
			if m.Sig().CanError() {
				retType = computeResultType(retType)
			}

			fn := c.module.NewFunc(mangledName, retType, params...)
			c.funcs[mangledName] = fn
		}
	}
}

// defineTypeMethods generates method bodies (pass 2).
// Generic types are skipped — their methods are handled by defineMonoMethods.
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
		if len(named.TypeParams()) > 0 {
			continue // generic — handled by monomorphization
		}

		for _, md := range td.Methods {
			if md.Body == nil {
				continue
			}
			m := c.lookupAnyMethod(named, md.Name, md.IsGetter, md.IsSetter)
			if m == nil || m.Sig() == nil {
				continue
			}

			mangledName := mangleMethodName(td.Name, md.Name, md.IsSetter)
			fn, ok := c.funcs[mangledName]
			if !ok {
				continue
			}

			c.defineMethodFunc(md, m, fn, named)
		}
	}
}

// mangleMethodName returns the mangled IR function name for a method, appending
// a "$set" suffix for setters to avoid collisions with same-name getters.
func mangleMethodName(typeName, methodName string, isSetter bool) string {
	if isSetter {
		return typeName + "." + methodName + "$set"
	}
	return typeName + "." + methodName
}

// lookupAnyMethod finds a method, getter, or setter by name, dispatching to
// the appropriate typed lookup based on the AST declaration's getter/setter flags.
func (c *Compiler) lookupAnyMethod(named *types.Named, name string, isGetter, isSetter bool) *types.Method {
	if isGetter {
		return named.LookupGetter(name)
	}
	if isSetter {
		return named.LookupSetter(name)
	}
	return named.LookupMethod(name)
}

// defineMethodFunc generates the body of a single method.
func (c *Compiler) defineMethodFunc(md *ast.MethodDecl, m *types.Method, fn *ir.Func, ownerNamed ...*types.Named) {
	c.fn = fn
	c.locals = make(map[string]*ir.InstAlloca)
	c.dropFlags = make(map[string]*ir.InstAlloca)
	c.blockCounter = 0
	c.canError = m.Sig().CanError()
	c.currentRetType = m.Sig().Result()
	savedNamed := c.currentNamed
	if len(ownerNamed) > 0 {
		c.currentNamed = ownerNamed[0]
	}
	defer func() { c.currentNamed = savedNamed }()

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
		lt := c.resolveType(p.Type())
		alloca := entry.NewAlloca(lt)
		alloca.SetName(p.Name() + ".addr")
		entry.NewStore(fn.Params[paramIdx], alloca)
		c.locals[p.Name()] = alloca
		paramIdx++
	}

	c.genBlock(md.Body)

	// For drop() methods: after the user body, automatically drop all fields that have drop()
	if md.Name == "drop" && c.block != nil && c.block.Term == nil && len(ownerNamed) > 0 {
		c.emitFieldDrops(ownerNamed[0])
	}

	// Ensure the function ends with a terminator
	if c.block != nil && c.block.Term == nil {
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

// emitFieldDrops emits drop() calls for all fields of a type that themselves have drop().
// Called at the end of a user-defined drop() method to ensure fields are cleaned up.
// Fields are dropped in reverse declaration order.
func (c *Compiler) emitFieldDrops(named *types.Named) {
	layout := c.lookupTypeLayout(named)
	if layout == nil {
		return
	}

	// Load the receiver (this is i8* in method context)
	thisAlloca, ok := c.locals["this"]
	if !ok {
		return
	}
	thisPtr := c.block.NewLoad(thisAlloca.ElemType, thisAlloca)
	typedPtr := c.block.NewBitCast(thisPtr, layout.InstancePtrType)

	fields := named.AllFields()
	for i := len(fields) - 1; i >= 0; i-- {
		f := fields[i]
		fieldNamed := extractNamed(f.Type())
		if fieldNamed == nil || !fieldNamed.HasDrop() {
			continue
		}

		fieldIdx, ok := layout.InstanceFieldIndex[f.Name()]
		if !ok {
			continue
		}

		// Load the field value (a value struct: {vtable_ptr, instance_ptr})
		fieldPtr := c.block.NewGetElementPtr(layout.Instance.LLVMType, typedPtr,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(fieldIdx)))
		fieldVal := c.block.NewLoad(layout.Instance.Fields[fieldIdx].LLVMType, fieldPtr)
		fieldInstance := c.extractInstancePtr(fieldVal)

		// Resolve and call field type's drop() method
		ownerName := c.resolveMethodOwner(fieldNamed, "drop")
		mangledName := mangleMethodName(ownerName, "drop", false)
		if dropFn, ok := c.funcs[mangledName]; ok {
			c.block.NewCall(dropFn, fieldInstance)
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
	if c.info.StdScope != nil {
		if obj := c.info.StdScope.Lookup(name); obj != nil {
			if tn, ok := obj.(*types.TypeName); ok {
				if named, ok := tn.Type().(*types.Named); ok {
					return named
				}
			}
		}
	}
	return nil
}

// lookupEnumType finds an Enum type in sema info by name.
func (c *Compiler) lookupEnumType(name string) *types.Enum {
	for _, scope := range c.info.Scopes {
		if obj := scope.Lookup(name); obj != nil {
			if tn, ok := obj.(*types.TypeName); ok {
				if enum, ok := tn.Type().(*types.Enum); ok {
					return enum
				}
			}
		}
	}
	if c.info.StdScope != nil {
		if obj := c.info.StdScope.Lookup(name); obj != nil {
			if tn, ok := obj.(*types.TypeName); ok {
				if enum, ok := tn.Type().(*types.Enum); ok {
					return enum
				}
			}
		}
	}
	return nil
}

// computeEnumLayouts computes layouts for all enum declarations in the file.
// Generic enums (with TypeParams) are skipped — handled by computeMonoLayouts.
func (c *Compiler) computeEnumLayouts(file *ast.File) {
	for _, decl := range file.Decls {
		ed, ok := decl.(*ast.EnumDecl)
		if !ok {
			continue
		}
		enum := c.lookupEnumType(ed.Name)
		if enum == nil {
			continue
		}
		if len(enum.TypeParams()) > 0 {
			continue // generic — handled by monomorphization
		}
		c.enumLayouts[enum] = computeEnumLayout(c.module, enum)
	}
}

// lookupTypeLayout finds the layout for a user type, handling Instance and monoCtx.
func (c *Compiler) lookupTypeLayout(typ types.Type) *TypeDeclLayout {
	if inst, ok := typ.(*types.Instance); ok {
		return c.monoLayouts[monoName(inst)]
	}
	if n := extractNamed(typ); n != nil {
		// Inside a mono method body, the origin Named maps to the mono layout
		if c.monoCtx != nil {
			if origin, ok := c.monoCtx.origin.(*types.Named); ok && n == origin {
				return c.monoLayouts[c.monoCtx.name]
			}
		}
		return c.layouts[n]
	}
	return nil
}

// lookupEnumLayout finds the layout for an enum, handling Instance and monoCtx.
func (c *Compiler) lookupEnumLayout(typ types.Type) *TypeDeclLayout {
	if inst, ok := typ.(*types.Instance); ok {
		return c.monoEnumLayouts[monoName(inst)]
	}
	if e := extractEnum(typ); e != nil {
		if c.monoCtx != nil {
			if origin, ok := c.monoCtx.origin.(*types.Enum); ok && e == origin {
				return c.monoEnumLayouts[c.monoCtx.name]
			}
		}
		return c.enumLayouts[e]
	}
	return nil
}

// resolveTypeName returns the mangled type name for method dispatch.
func (c *Compiler) resolveTypeName(typ types.Type) string {
	if inst, ok := typ.(*types.Instance); ok {
		return monoName(inst)
	}
	if c.monoCtx != nil {
		if n, ok := typ.(*types.Named); ok {
			if origin, ok := c.monoCtx.origin.(*types.Named); ok && n == origin {
				return c.monoCtx.name
			}
		}
	}
	if n := extractNamed(typ); n != nil {
		return n.Obj().Name()
	}
	return ""
}

// resolveMethodOwner returns the type name of the type that actually defines the given method.
// If the method is overridden in the child, returns the child's name. If inherited,
// walks up the parent chain to find the defining type.
func (c *Compiler) resolveMethodOwner(named *types.Named, methodName string) string {
	// Check own methods first
	for _, m := range named.Methods() {
		if m.Name() == methodName {
			return named.Obj().Name()
		}
	}
	// Walk parents
	for _, p := range named.Parents() {
		if p.LookupMethod(methodName) != nil {
			return c.resolveMethodOwner(p, methodName)
		}
	}
	return named.Obj().Name() // fallback
}

// computeVtableInfo scans all type declarations and marks types that have children.
// A type has children if any other type inherits from it (directly or transitively).
func (c *Compiler) computeVtableInfo(file *ast.File) {
	for _, decl := range file.Decls {
		td, ok := decl.(*ast.TypeDecl)
		if !ok {
			continue
		}
		named := c.lookupNamedType(td.Name)
		if named == nil {
			continue
		}
		if len(named.TypeParams()) > 0 {
			continue
		}
		var markParents func(n *types.Named)
		markParents = func(n *types.Named) {
			for _, p := range n.Parents() {
				c.hasChildren[p] = true
				markParents(p)
			}
		}
		markParents(named)
	}
}

// needsVtable reports whether a type needs virtual dispatch.
// True if the type has children (someone inherits from it) or is abstract.
func (c *Compiler) needsVtable(named *types.Named) bool {
	return c.hasChildren[named] || named.IsAbstract()
}

// isNativeTypeDecl checks if a type declaration has the `native annotation.
func isNativeTypeDecl(td *ast.TypeDecl) bool {
	for _, ann := range td.Annotations {
		if ann.Name == "native" {
			return true
		}
	}
	return false
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
