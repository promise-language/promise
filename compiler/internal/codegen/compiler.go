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

	// Self-substitution for default method synthesis on structural interfaces.
	// When non-nil, replaces occurrences of selfSubst.iface with selfSubst.concrete
	// in sema type lookups during codegen.
	selfSubst *selfSubstInfo

	// AST file reference for looking up default method bodies during synthesis.
	file *ast.File

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

// selfSubstInfo tracks a Self-type substitution for generating default method
// bodies from structural interfaces specialized to a concrete type.
type selfSubstInfo struct {
	iface    *types.Named // the structural interface (e.g., Equal)
	concrete *types.Named // the concrete implementing type (e.g., Point)
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
		file:            file,
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

	// String direct equality (codegen-emitted LLVM IR, replaces C runtime)
	c.defineStringDirectEqFunc()

	// Vector methods
	c.funcs["promise_vector_with_capacity"] = c.module.NewFunc("promise_vector_with_capacity",
		irtypes.I8Ptr,
		ir.NewParam("capacity", irtypes.I64),
		ir.NewParam("elem_size", irtypes.I64))

	c.funcs["promise_vector_push"] = c.module.NewFunc("promise_vector_push",
		irtypes.I8Ptr,
		ir.NewParam("slice", irtypes.I8Ptr),
		ir.NewParam("elem", irtypes.I8Ptr),
		ir.NewParam("elem_size", irtypes.I64))

	c.funcs["promise_vector_pop"] = c.module.NewFunc("promise_vector_pop",
		irtypes.I32,
		ir.NewParam("slice", irtypes.I8Ptr),
		ir.NewParam("out_elem", irtypes.I8Ptr),
		ir.NewParam("elem_size", irtypes.I64))

	// Realloc for vector growth, memmove for vector remove
	c.funcs["realloc"] = c.module.NewFunc("realloc",
		irtypes.I8Ptr,
		ir.NewParam("ptr", irtypes.I8Ptr),
		ir.NewParam("size", irtypes.I64))
	c.funcs["memmove"] = c.module.NewFunc("memmove",
		irtypes.I8Ptr,
		ir.NewParam("dst", irtypes.I8Ptr),
		ir.NewParam("src", irtypes.I8Ptr),
		ir.NewParam("n", irtypes.I64))

	// Vector contains/remove (codegen-emitted LLVM IR, replaces C runtime)
	c.defineVectorContainsFunc()
	c.defineVectorRemoveFunc()

	c.funcs["promise_string_trim"] = c.module.NewFunc("promise_string_trim",
		irtypes.I8Ptr,
		ir.NewParam("s", irtypes.I8Ptr))

	c.funcs["promise_string_split"] = c.module.NewFunc("promise_string_split",
		irtypes.I8Ptr,
		ir.NewParam("s", irtypes.I8Ptr),
		ir.NewParam("sep", irtypes.I8Ptr))

	// snprintf extern (needed by defineF64ToStringFunc)
	snprintfFn := c.module.NewFunc("snprintf", irtypes.I32,
		ir.NewParam("buf", irtypes.I8Ptr),
		ir.NewParam("size", irtypes.I64),
		ir.NewParam("fmt", irtypes.I8Ptr))
	snprintfFn.Sig.Variadic = true
	c.funcs["snprintf"] = snprintfFn

	// Value-to-string conversion (codegen-emitted LLVM IR, replaces C runtime)
	c.defineBoolToStringFunc()
	c.defineIntToStringFunc()
	c.defineUintToStringFunc()
	c.defineF64ToStringFunc()
	c.defineCharToStringFunc()

	c.funcs["promise_string_next_char"] = c.module.NewFunc("promise_string_next_char",
		irtypes.I32,
		ir.NewParam("s", irtypes.I8Ptr),
		ir.NewParam("pos", irtypes.NewPointer(irtypes.I64)))

	// RTTI intrinsic for runtime type checking
	c.funcs["promise_type_is"] = c.module.NewFunc("promise_type_is",
		irtypes.I32,
		ir.NewParam("variant_ptr", irtypes.I8Ptr),
		ir.NewParam("expected_id", irtypes.I32))

	// String hash function (codegen-emitted LLVM IR, replaces C runtime)
	c.defineStringHashFunc()

	// String equality comparison (codegen-emitted LLVM IR, replaces C runtime)
	c.defineStringEqFunc()
}

// defineStringDirectEqFunc emits an LLVM IR function that compares two strings
// for equality. Used by the == and != operators. Takes direct i8* string pointers
// (not indirect like defineStringEqFunc which is used by Vector.contains).
// Returns i1 (true if equal). Replaces the C runtime promise_string_eq.
// String instance layout: { i8* _variant, i64 len, [0 x i8] data }
func (c *Compiler) defineStringDirectEqFunc() {
	aParam := ir.NewParam("a", irtypes.I8Ptr)
	bParam := ir.NewParam("b", irtypes.I8Ptr)
	fn := c.module.NewFunc("promise_string_eq", irtypes.I1, aParam, bParam)

	strInstanceType := irtypes.NewStruct(
		irtypes.I8Ptr,                   // _variant
		irtypes.I64,                     // len
		irtypes.NewArray(0, irtypes.I8), // data (flexible array)
	)

	zero64 := constant.NewInt(irtypes.I64, 0)
	one64 := constant.NewInt(irtypes.I64, 1)
	trueVal := constant.NewInt(irtypes.I1, 1)
	falseVal := constant.NewInt(irtypes.I1, 0)

	// Fast path: same pointer → equal
	entry := fn.NewBlock("entry")
	samePtr := entry.NewICmp(enum.IPredEQ, aParam, bParam)
	samePtrBlk := fn.NewBlock("same_ptr")
	checkLenBlk := fn.NewBlock("check_len")
	entry.NewCondBr(samePtr, samePtrBlk, checkLenBlk)

	samePtrBlk.NewRet(trueVal)

	// Compare lengths
	typedA := checkLenBlk.NewBitCast(aParam, irtypes.NewPointer(strInstanceType))
	lenPtrA := checkLenBlk.NewGetElementPtr(strInstanceType, typedA,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 1))
	lenA := checkLenBlk.NewLoad(irtypes.I64, lenPtrA)

	typedB := checkLenBlk.NewBitCast(bParam, irtypes.NewPointer(strInstanceType))
	lenPtrB := checkLenBlk.NewGetElementPtr(strInstanceType, typedB,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 1))
	lenB := checkLenBlk.NewLoad(irtypes.I64, lenPtrB)

	lenEq := checkLenBlk.NewICmp(enum.IPredEQ, lenA, lenB)
	lenNeqBlk := fn.NewBlock("len_neq")
	cmpDataBlk := fn.NewBlock("cmp_data")
	checkLenBlk.NewCondBr(lenEq, cmpDataBlk, lenNeqBlk)

	lenNeqBlk.NewRet(falseVal)

	// Get data pointers and set up byte comparison loop
	dataPtrA := cmpDataBlk.NewGetElementPtr(strInstanceType, typedA,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 2),
		constant.NewInt(irtypes.I32, 0))
	dataPtrB := cmpDataBlk.NewGetElementPtr(strInstanceType, typedB,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 2),
		constant.NewInt(irtypes.I32, 0))

	iAlloca := cmpDataBlk.NewAlloca(irtypes.I64)
	cmpDataBlk.NewStore(zero64, iAlloca)

	headerBlk := fn.NewBlock("loop.header")
	bodyBlk := fn.NewBlock("loop.body")
	equalBlk := fn.NewBlock("equal")
	neqBlk := fn.NewBlock("not_equal")

	cmpDataBlk.NewBr(headerBlk)

	// Loop header: check i < len
	iVal := headerBlk.NewLoad(irtypes.I64, iAlloca)
	cond := headerBlk.NewICmp(enum.IPredSLT, iVal, lenA)
	headerBlk.NewCondBr(cond, bodyBlk, equalBlk)

	// Loop body: compare byte at position i
	iCur := bodyBlk.NewLoad(irtypes.I64, iAlloca)
	bytePtrA := bodyBlk.NewGetElementPtr(irtypes.I8, dataPtrA, iCur)
	byteA := bodyBlk.NewLoad(irtypes.I8, bytePtrA)
	bytePtrB := bodyBlk.NewGetElementPtr(irtypes.I8, dataPtrB, iCur)
	byteB := bodyBlk.NewLoad(irtypes.I8, bytePtrB)
	byteEq := bodyBlk.NewICmp(enum.IPredEQ, byteA, byteB)
	nextI := bodyBlk.NewAdd(iCur, one64)
	bodyBlk.NewStore(nextI, iAlloca)
	bodyBlk.NewCondBr(byteEq, headerBlk, neqBlk)

	// Bytes differ → not equal
	neqBlk.NewRet(falseVal)

	// All bytes match → equal
	equalBlk.NewRet(trueVal)

	c.funcs["promise_string_eq"] = fn
}

// defineStringHashFunc emits an LLVM IR function that computes FNV-1a hash
// over the raw bytes of a string. Replaces the C runtime promise_hash_string_value.
// String instance layout: { i8* _variant, i64 len, [0 x i8] data }
func (c *Compiler) defineStringHashFunc() {
	ptrParam := ir.NewParam("ptr", irtypes.I8Ptr)
	fn := c.module.NewFunc("__promise_hash_string", irtypes.I64, ptrParam)

	strInstanceType := irtypes.NewStruct(
		irtypes.I8Ptr,                   // _variant
		irtypes.I64,                     // len
		irtypes.NewArray(0, irtypes.I8), // data (flexible array)
	)

	fnvOffset := constant.NewInt(irtypes.I64, -3750763034362895579) // 0xcbf29ce484222325
	fnvPrime := constant.NewInt(irtypes.I64, 1099511628211)         // 0x00000100000001b3
	zero64 := constant.NewInt(irtypes.I64, 0)
	one64 := constant.NewInt(irtypes.I64, 1)

	// Entry: null check
	entry := fn.NewBlock("entry")
	isNull := entry.NewICmp(enum.IPredEQ, ptrParam, constant.NewNull(irtypes.I8Ptr))
	nullBlk := fn.NewBlock("null")
	initBlk := fn.NewBlock("init")
	entry.NewCondBr(isNull, nullBlk, initBlk)

	// Null → return 0
	nullBlk.NewRet(zero64)

	// Init: load len and data pointer, set up loop variables
	typedPtr := initBlk.NewBitCast(ptrParam, irtypes.NewPointer(strInstanceType))
	lenPtr := initBlk.NewGetElementPtr(strInstanceType, typedPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 1))
	strLen := initBlk.NewLoad(irtypes.I64, lenPtr)
	dataPtr := initBlk.NewGetElementPtr(strInstanceType, typedPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 2),
		constant.NewInt(irtypes.I32, 0))

	// Alloca-based loop variables (consistent with rest of codegen)
	iAlloca := initBlk.NewAlloca(irtypes.I64)
	initBlk.NewStore(zero64, iAlloca)
	hAlloca := initBlk.NewAlloca(irtypes.I64)
	initBlk.NewStore(fnvOffset, hAlloca)

	headerBlk := fn.NewBlock("loop.header")
	bodyBlk := fn.NewBlock("loop.body")
	exitBlk := fn.NewBlock("exit")

	initBlk.NewBr(headerBlk)

	// Loop header: check i < len
	iVal := headerBlk.NewLoad(irtypes.I64, iAlloca)
	cond := headerBlk.NewICmp(enum.IPredSLT, iVal, strLen)
	headerBlk.NewCondBr(cond, bodyBlk, exitBlk)

	// Loop body: hash = (hash ^ byte) * prime; i++
	iCur := bodyBlk.NewLoad(irtypes.I64, iAlloca)
	bytePtr := bodyBlk.NewGetElementPtr(irtypes.I8, dataPtr, iCur)
	byteVal := bodyBlk.NewLoad(irtypes.I8, bytePtr)
	byteExt := bodyBlk.NewZExt(byteVal, irtypes.I64)
	hCur := bodyBlk.NewLoad(irtypes.I64, hAlloca)
	xored := bodyBlk.NewXor(hCur, byteExt)
	mulled := bodyBlk.NewMul(xored, fnvPrime)
	bodyBlk.NewStore(mulled, hAlloca)
	nextI := bodyBlk.NewAdd(iCur, one64)
	bodyBlk.NewStore(nextI, iAlloca)
	bodyBlk.NewBr(headerBlk)

	// Exit: return hash
	result := exitBlk.NewLoad(irtypes.I64, hAlloca)
	exitBlk.NewRet(result)

	c.funcs["__promise_hash_string"] = fn
}

// defineStringEqFunc emits an LLVM IR function that compares two string keys
// by content. Replaces the C runtime promise_eq_string. Used by Vector.contains
// for string elements. Parameters are indirect pointers (pointer to slot
// containing a string pointer), matching the generic comparator ABI.
// String instance layout: { i8* _variant, i64 len, [0 x i8] data }
func (c *Compiler) defineStringEqFunc() {
	aParam := ir.NewParam("a", irtypes.I8Ptr)
	bParam := ir.NewParam("b", irtypes.I8Ptr)
	keySizeParam := ir.NewParam("key_size", irtypes.I64)
	fn := c.module.NewFunc("__promise_eq_string", irtypes.I32, aParam, bParam, keySizeParam)

	strInstanceType := irtypes.NewStruct(
		irtypes.I8Ptr,                   // _variant
		irtypes.I64,                     // len
		irtypes.NewArray(0, irtypes.I8), // data (flexible array)
	)

	zero32 := constant.NewInt(irtypes.I32, 0)
	one32 := constant.NewInt(irtypes.I32, 1)
	zero64 := constant.NewInt(irtypes.I64, 0)
	one64 := constant.NewInt(irtypes.I64, 1)

	// Entry: dereference indirect pointers to get actual string pointers
	entry := fn.NewBlock("entry")
	ptrPtrA := entry.NewBitCast(aParam, irtypes.NewPointer(irtypes.I8Ptr))
	pa := entry.NewLoad(irtypes.I8Ptr, ptrPtrA)
	ptrPtrB := entry.NewBitCast(bParam, irtypes.NewPointer(irtypes.I8Ptr))
	pb := entry.NewLoad(irtypes.I8Ptr, ptrPtrB)

	// Fast path: same pointer → equal
	samePtr := entry.NewICmp(enum.IPredEQ, pa, pb)
	samePtrBlk := fn.NewBlock("same_ptr")
	checkNullBlk := fn.NewBlock("check_null")
	entry.NewCondBr(samePtr, samePtrBlk, checkNullBlk)

	samePtrBlk.NewRet(one32)

	// Null check: if either is null → not equal
	aNull := checkNullBlk.NewICmp(enum.IPredEQ, pa, constant.NewNull(irtypes.I8Ptr))
	bNull := checkNullBlk.NewICmp(enum.IPredEQ, pb, constant.NewNull(irtypes.I8Ptr))
	eitherNull := checkNullBlk.NewOr(aNull, bNull)
	nullBlk := fn.NewBlock("null")
	checkLenBlk := fn.NewBlock("check_len")
	checkNullBlk.NewCondBr(eitherNull, nullBlk, checkLenBlk)

	nullBlk.NewRet(zero32)

	// Compare lengths
	typedA := checkLenBlk.NewBitCast(pa, irtypes.NewPointer(strInstanceType))
	lenPtrA := checkLenBlk.NewGetElementPtr(strInstanceType, typedA,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 1))
	lenA := checkLenBlk.NewLoad(irtypes.I64, lenPtrA)

	typedB := checkLenBlk.NewBitCast(pb, irtypes.NewPointer(strInstanceType))
	lenPtrB := checkLenBlk.NewGetElementPtr(strInstanceType, typedB,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 1))
	lenB := checkLenBlk.NewLoad(irtypes.I64, lenPtrB)

	lenEq := checkLenBlk.NewICmp(enum.IPredEQ, lenA, lenB)
	lenNeqBlk := fn.NewBlock("len_neq")
	cmpDataBlk := fn.NewBlock("cmp_data")
	checkLenBlk.NewCondBr(lenEq, cmpDataBlk, lenNeqBlk)

	lenNeqBlk.NewRet(zero32)

	// Get data pointers and set up byte comparison loop
	dataPtrA := cmpDataBlk.NewGetElementPtr(strInstanceType, typedA,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 2),
		constant.NewInt(irtypes.I32, 0))
	dataPtrB := cmpDataBlk.NewGetElementPtr(strInstanceType, typedB,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 2),
		constant.NewInt(irtypes.I32, 0))

	iAlloca := cmpDataBlk.NewAlloca(irtypes.I64)
	cmpDataBlk.NewStore(zero64, iAlloca)

	headerBlk := fn.NewBlock("loop.header")
	bodyBlk := fn.NewBlock("loop.body")
	equalBlk := fn.NewBlock("equal")
	neqBlk := fn.NewBlock("not_equal")

	cmpDataBlk.NewBr(headerBlk)

	// Loop header: check i < len
	iVal := headerBlk.NewLoad(irtypes.I64, iAlloca)
	cond := headerBlk.NewICmp(enum.IPredSLT, iVal, lenA)
	headerBlk.NewCondBr(cond, bodyBlk, equalBlk)

	// Loop body: compare byte at position i
	iCur := bodyBlk.NewLoad(irtypes.I64, iAlloca)
	bytePtrA := bodyBlk.NewGetElementPtr(irtypes.I8, dataPtrA, iCur)
	byteA := bodyBlk.NewLoad(irtypes.I8, bytePtrA)
	bytePtrB := bodyBlk.NewGetElementPtr(irtypes.I8, dataPtrB, iCur)
	byteB := bodyBlk.NewLoad(irtypes.I8, bytePtrB)
	byteEq := bodyBlk.NewICmp(enum.IPredEQ, byteA, byteB)
	nextI := bodyBlk.NewAdd(iCur, one64)
	bodyBlk.NewStore(nextI, iAlloca)
	bodyBlk.NewCondBr(byteEq, headerBlk, neqBlk)

	// Bytes differ → return 0
	neqBlk.NewRet(zero32)

	// All bytes match → return 1
	equalBlk.NewRet(one32)

	c.funcs["__promise_eq_string"] = fn
}

// defineVectorContainsFunc emits an LLVM IR function that searches a vector for
// an element. Replaces the C runtime promise_vector_contains.
// For string elements, uses the eq_fn comparator (__promise_eq_string).
// For other types, does byte-by-byte comparison (equivalent to memcmp).
// Vector layout: {i64 len, i64 cap, [data...]} with 16-byte header.
func (c *Compiler) defineVectorContainsFunc() {
	sliceParam := ir.NewParam("slice", irtypes.I8Ptr)
	elemParam := ir.NewParam("elem", irtypes.I8Ptr)
	elemSizeParam := ir.NewParam("elem_size", irtypes.I64)
	eqFnParam := ir.NewParam("eq_fn", irtypes.I8Ptr)
	fn := c.module.NewFunc("promise_vector_contains", irtypes.I8,
		sliceParam, elemParam, elemSizeParam, eqFnParam)

	headerType := vectorHeaderType() // {i64, i64}

	zero64 := constant.NewInt(irtypes.I64, 0)
	one64 := constant.NewInt(irtypes.I64, 1)
	foundVal := constant.NewInt(irtypes.I8, 1)
	notFoundVal := constant.NewInt(irtypes.I8, 0)
	headerSizeConst := constant.NewInt(irtypes.I64, int64(vectorHeaderSize))

	// Entry: load len from header, init loop counter
	entry := fn.NewBlock("entry")
	hdrPtr := entry.NewBitCast(sliceParam, irtypes.NewPointer(headerType))
	lenPtr := entry.NewGetElementPtr(headerType, hdrPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
	vecLen := entry.NewLoad(irtypes.I64, lenPtr)
	iAlloca := entry.NewAlloca(irtypes.I64)
	entry.NewStore(zero64, iAlloca)

	loopHeader := fn.NewBlock("loop.header")
	loopBody := fn.NewBlock("loop.body")
	callEq := fn.NewBlock("call_eq")
	cmpBytes := fn.NewBlock("cmp_bytes")
	byteHeader := fn.NewBlock("byte.header")
	byteBody := fn.NewBlock("byte.body")
	loopNext := fn.NewBlock("loop.next")
	found := fn.NewBlock("found")
	notFound := fn.NewBlock("not_found")

	entry.NewBr(loopHeader)

	// loop.header: check i < len
	iVal := loopHeader.NewLoad(irtypes.I64, iAlloca)
	cond := loopHeader.NewICmp(enum.IPredSLT, iVal, vecLen)
	loopHeader.NewCondBr(cond, loopBody, notFound)

	// loop.body: compute element address, check eq_fn
	iCur := loopBody.NewLoad(irtypes.I64, iAlloca)
	offset := loopBody.NewMul(iCur, elemSizeParam)
	dataOffset := loopBody.NewAdd(offset, headerSizeConst)
	curPtr := loopBody.NewGetElementPtr(irtypes.I8, sliceParam, dataOffset)
	isNull := loopBody.NewICmp(enum.IPredEQ, eqFnParam, constant.NewNull(irtypes.I8Ptr))
	loopBody.NewCondBr(isNull, cmpBytes, callEq)

	// call_eq: cast eq_fn to function pointer and call
	eqFnType := irtypes.NewFunc(irtypes.I32, irtypes.I8Ptr, irtypes.I8Ptr, irtypes.I64)
	eqFnCast := callEq.NewBitCast(eqFnParam, irtypes.NewPointer(eqFnType))
	eqResult := callEq.NewCall(eqFnCast, curPtr, elemParam, elemSizeParam)
	eqNonZero := callEq.NewICmp(enum.IPredNE, eqResult, constant.NewInt(irtypes.I32, 0))
	callEq.NewCondBr(eqNonZero, found, loopNext)

	// cmp_bytes: byte-by-byte comparison (replaces memcmp)
	jAlloca := cmpBytes.NewAlloca(irtypes.I64)
	cmpBytes.NewStore(zero64, jAlloca)
	cmpBytes.NewBr(byteHeader)

	// byte.header: check j < elem_size
	jVal := byteHeader.NewLoad(irtypes.I64, jAlloca)
	byteCond := byteHeader.NewICmp(enum.IPredSLT, jVal, elemSizeParam)
	byteHeader.NewCondBr(byteCond, byteBody, found)

	// byte.body: compare single byte
	jCur := byteBody.NewLoad(irtypes.I64, jAlloca)
	curBytePtr := byteBody.NewGetElementPtr(irtypes.I8, curPtr, jCur)
	curByte := byteBody.NewLoad(irtypes.I8, curBytePtr)
	elemBytePtr := byteBody.NewGetElementPtr(irtypes.I8, elemParam, jCur)
	elemByte := byteBody.NewLoad(irtypes.I8, elemBytePtr)
	byteEq := byteBody.NewICmp(enum.IPredEQ, curByte, elemByte)
	nextJ := byteBody.NewAdd(jCur, one64)
	byteBody.NewStore(nextJ, jAlloca)
	byteBody.NewCondBr(byteEq, byteHeader, loopNext)

	// loop.next: increment i, loop back
	iNext := loopNext.NewLoad(irtypes.I64, iAlloca)
	iInc := loopNext.NewAdd(iNext, one64)
	loopNext.NewStore(iInc, iAlloca)
	loopNext.NewBr(loopHeader)

	// found / not_found
	found.NewRet(foundVal)
	notFound.NewRet(notFoundVal)

	c.funcs["promise_vector_contains"] = fn
}

// defineVectorRemoveFunc emits an LLVM IR function that removes an element
// from a vector at a given index by shifting subsequent elements left.
// Replaces the C runtime promise_vector_remove.
// Uses memmove for the shift and decrements the length field.
func (c *Compiler) defineVectorRemoveFunc() {
	sliceParam := ir.NewParam("slice", irtypes.I8Ptr)
	indexParam := ir.NewParam("index", irtypes.I64)
	elemSizeParam := ir.NewParam("elem_size", irtypes.I64)
	fn := c.module.NewFunc("promise_vector_remove", irtypes.Void,
		sliceParam, indexParam, elemSizeParam)

	headerType := vectorHeaderType() // {i64, i64}

	zero64 := constant.NewInt(irtypes.I64, 0)
	one64 := constant.NewInt(irtypes.I64, 1)
	headerSizeConst := constant.NewInt(irtypes.I64, int64(vectorHeaderSize))

	// Entry: load len, bounds check
	entry := fn.NewBlock("entry")
	hdrPtr := entry.NewBitCast(sliceParam, irtypes.NewPointer(headerType))
	lenPtr := entry.NewGetElementPtr(headerType, hdrPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
	vecLen := entry.NewLoad(irtypes.I64, lenPtr)

	isNeg := entry.NewICmp(enum.IPredSLT, indexParam, zero64)
	isOver := entry.NewICmp(enum.IPredSGE, indexParam, vecLen)
	oob := entry.NewOr(isNeg, isOver)

	panicBlk := fn.NewBlock("panic")
	checkShift := fn.NewBlock("check_shift")
	entry.NewCondBr(oob, panicBlk, checkShift)

	// panic: call promise_panic with out-of-bounds message
	panicMsg := constant.NewCharArrayFromString("vector remove: index out of bounds\x00")
	globalName := fmt.Sprintf(".str.vecremove.%d", c.strCounter)
	c.strCounter++
	panicGlobal := c.module.NewGlobalDef(globalName, panicMsg)
	panicGlobal.Immutable = true
	msgPtr := panicBlk.NewGetElementPtr(panicGlobal.ContentType, panicGlobal,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
	panicBlk.NewCall(c.funcs["promise_panic"], msgPtr)
	panicBlk.NewUnreachable()

	// check_shift: compute data base, check if shift needed
	dataBase := checkShift.NewGetElementPtr(irtypes.I8, sliceParam, headerSizeConst)
	lenMinus1 := checkShift.NewSub(vecLen, one64)
	needsShift := checkShift.NewICmp(enum.IPredSLT, indexParam, lenMinus1)

	doShift := fn.NewBlock("do_shift")
	decLen := fn.NewBlock("dec_len")
	checkShift.NewCondBr(needsShift, doShift, decLen)

	// do_shift: memmove elements left
	dstOffset := doShift.NewMul(indexParam, elemSizeParam)
	dst := doShift.NewGetElementPtr(irtypes.I8, dataBase, dstOffset)
	idxPlus1 := doShift.NewAdd(indexParam, one64)
	srcOffset := doShift.NewMul(idxPlus1, elemSizeParam)
	src := doShift.NewGetElementPtr(irtypes.I8, dataBase, srcOffset)
	remaining := doShift.NewSub(vecLen, idxPlus1)
	moveSize := doShift.NewMul(remaining, elemSizeParam)
	doShift.NewCall(c.funcs["memmove"], dst, src, moveSize)
	doShift.NewBr(decLen)

	// dec_len: decrement length
	newLen := decLen.NewSub(vecLen, one64)
	decLen.NewStore(newLen, lenPtr)
	decLen.NewRet(nil)

	c.funcs["promise_vector_remove"] = fn
}

// defineBoolToStringFunc emits an LLVM IR function that converts a boolean (i8)
// to its string representation ("true" or "false").
// Replaces the C runtime promise_bool_to_string.
func (c *Compiler) defineBoolToStringFunc() {
	xParam := ir.NewParam("x", irtypes.I8)
	fn := c.module.NewFunc("promise_bool_to_string", irtypes.I8Ptr, xParam)

	// Global string constants
	trueData := constant.NewCharArrayFromString("true")
	trueGlobal := c.module.NewGlobalDef(fmt.Sprintf(".str.bool.true.%d", c.strCounter), trueData)
	c.strCounter++
	trueGlobal.Immutable = true

	falseData := constant.NewCharArrayFromString("false")
	falseGlobal := c.module.NewGlobalDef(fmt.Sprintf(".str.bool.false.%d", c.strCounter), falseData)
	c.strCounter++
	falseGlobal.Immutable = true

	entry := fn.NewBlock("entry")
	trueBlk := fn.NewBlock("true")
	falseBlk := fn.NewBlock("false")

	isTrue := entry.NewICmp(enum.IPredNE, xParam, constant.NewInt(irtypes.I8, 0))
	entry.NewCondBr(isTrue, trueBlk, falseBlk)

	// true block
	truePtr := trueBlk.NewGetElementPtr(trueGlobal.ContentType, trueGlobal,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
	trueStr := trueBlk.NewCall(c.funcs["promise_string_new"],
		truePtr, constant.NewInt(irtypes.I64, 4))
	trueBlk.NewRet(trueStr)

	// false block
	falsePtr := falseBlk.NewGetElementPtr(falseGlobal.ContentType, falseGlobal,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
	falseStr := falseBlk.NewCall(c.funcs["promise_string_new"],
		falsePtr, constant.NewInt(irtypes.I64, 5))
	falseBlk.NewRet(falseStr)

	c.funcs["promise_bool_to_string"] = fn
}

// defineIntToStringFunc emits an LLVM IR function that converts a signed int64
// to its decimal string representation. Uses a divide-by-10 loop writing digits
// backwards into a 20-byte stack buffer, then calls promise_string_new.
// Replaces the C runtime promise_int_to_string.
func (c *Compiler) defineIntToStringFunc() {
	xParam := ir.NewParam("x", irtypes.I64)
	fn := c.module.NewFunc("promise_int_to_string", irtypes.I8Ptr, xParam)

	bufType := irtypes.NewArray(20, irtypes.I8)
	zero64 := constant.NewInt(irtypes.I64, 0)
	one64 := constant.NewInt(irtypes.I64, 1)
	ten64 := constant.NewInt(irtypes.I64, 10)
	ascii0 := constant.NewInt(irtypes.I64, 48) // '0'

	// Global "0" constant for zero case
	zeroData := constant.NewCharArrayFromString("0")
	zeroGlobal := c.module.NewGlobalDef(fmt.Sprintf(".str.zero.%d", c.strCounter), zeroData)
	c.strCounter++
	zeroGlobal.Immutable = true

	// entry: allocas and zero check
	entry := fn.NewBlock("entry")
	buf := entry.NewAlloca(bufType)
	posAlloca := entry.NewAlloca(irtypes.I64)
	entry.NewStore(constant.NewInt(irtypes.I64, 19), posAlloca)
	valAlloca := entry.NewAlloca(irtypes.I64)
	negAlloca := entry.NewAlloca(irtypes.I1)
	entry.NewStore(constant.False, negAlloca)

	isZero := entry.NewICmp(enum.IPredEQ, xParam, zero64)
	zeroCase := fn.NewBlock("zero_case")
	checkNeg := fn.NewBlock("check_neg")
	entry.NewCondBr(isZero, zeroCase, checkNeg)

	// zero_case: return "0"
	zeroPtr := zeroCase.NewGetElementPtr(zeroGlobal.ContentType, zeroGlobal,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
	zeroStr := zeroCase.NewCall(c.funcs["promise_string_new"],
		zeroPtr, one64)
	zeroCase.NewRet(zeroStr)

	// check_neg: test if negative
	negate := fn.NewBlock("negate")
	startLoop := fn.NewBlock("start_loop")
	isNeg := checkNeg.NewICmp(enum.IPredSLT, xParam, zero64)
	checkNeg.NewCondBr(isNeg, negate, startLoop)

	// negate: abs = 0 - x, store abs and neg=true
	absVal := negate.NewSub(zero64, xParam)
	negate.NewStore(absVal, valAlloca)
	negate.NewStore(constant.True, negAlloca)
	digitLoop := fn.NewBlock("digit_loop")
	negate.NewBr(digitLoop)

	// start_loop: store x directly
	startLoop.NewStore(xParam, valAlloca)
	startLoop.NewBr(digitLoop)

	// digit_loop: extract digit, store to buffer
	cur := digitLoop.NewLoad(irtypes.I64, valAlloca)
	digit := digitLoop.NewURem(cur, ten64)
	ch64 := digitLoop.NewAdd(digit, ascii0)
	ch8 := digitLoop.NewTrunc(ch64, irtypes.I8)
	pos := digitLoop.NewLoad(irtypes.I64, posAlloca)
	slot := digitLoop.NewGetElementPtr(bufType, buf, zero64, pos)
	digitLoop.NewStore(ch8, slot)
	posDec := digitLoop.NewSub(pos, one64)
	digitLoop.NewStore(posDec, posAlloca)
	next := digitLoop.NewUDiv(cur, ten64)
	digitLoop.NewStore(next, valAlloca)
	more := digitLoop.NewICmp(enum.IPredNE, next, zero64)
	checkSign := fn.NewBlock("check_sign")
	digitLoop.NewCondBr(more, digitLoop, checkSign)

	// check_sign: prepend '-' if negative
	negFlag := checkSign.NewLoad(irtypes.I1, negAlloca)
	addSign := fn.NewBlock("add_sign")
	done := fn.NewBlock("done")
	checkSign.NewCondBr(negFlag, addSign, done)

	// add_sign: store '-' at current pos
	signPos := addSign.NewLoad(irtypes.I64, posAlloca)
	signSlot := addSign.NewGetElementPtr(bufType, buf, zero64, signPos)
	addSign.NewStore(constant.NewInt(irtypes.I8, 45), signSlot) // '-'
	signPosDec := addSign.NewSub(signPos, one64)
	addSign.NewStore(signPosDec, posAlloca)
	addSign.NewBr(done)

	// done: compute start and length, call promise_string_new
	finalPos := done.NewLoad(irtypes.I64, posAlloca)
	start := done.NewAdd(finalPos, one64)
	strLen := done.NewSub(constant.NewInt(irtypes.I64, 20), start)
	dataPtr := done.NewGetElementPtr(bufType, buf, zero64, start)
	result := done.NewCall(c.funcs["promise_string_new"], dataPtr, strLen)
	done.NewRet(result)

	c.funcs["promise_int_to_string"] = fn
}

// defineUintToStringFunc emits an LLVM IR function that converts an unsigned int64
// to its decimal string representation. Same as signed but without negative handling.
// This is a NEW function (not replacing an existing extern) that fixes the u64 sign bug.
func (c *Compiler) defineUintToStringFunc() {
	xParam := ir.NewParam("x", irtypes.I64)
	fn := c.module.NewFunc("promise_uint_to_string", irtypes.I8Ptr, xParam)

	bufType := irtypes.NewArray(20, irtypes.I8)
	zero64 := constant.NewInt(irtypes.I64, 0)
	one64 := constant.NewInt(irtypes.I64, 1)
	ten64 := constant.NewInt(irtypes.I64, 10)
	ascii0 := constant.NewInt(irtypes.I64, 48)

	// Global "0" constant for zero case
	zeroData := constant.NewCharArrayFromString("0")
	zeroGlobal := c.module.NewGlobalDef(fmt.Sprintf(".str.uzero.%d", c.strCounter), zeroData)
	c.strCounter++
	zeroGlobal.Immutable = true

	// entry: allocas and zero check
	entry := fn.NewBlock("entry")
	buf := entry.NewAlloca(bufType)
	posAlloca := entry.NewAlloca(irtypes.I64)
	entry.NewStore(constant.NewInt(irtypes.I64, 19), posAlloca)
	valAlloca := entry.NewAlloca(irtypes.I64)
	entry.NewStore(xParam, valAlloca)

	isZero := entry.NewICmp(enum.IPredEQ, xParam, zero64)
	zeroCase := fn.NewBlock("zero_case")
	digitLoop := fn.NewBlock("digit_loop")
	entry.NewCondBr(isZero, zeroCase, digitLoop)

	// zero_case: return "0"
	zeroPtr := zeroCase.NewGetElementPtr(zeroGlobal.ContentType, zeroGlobal,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
	zeroStr := zeroCase.NewCall(c.funcs["promise_string_new"],
		zeroPtr, one64)
	zeroCase.NewRet(zeroStr)

	// digit_loop: extract digit, store to buffer
	cur := digitLoop.NewLoad(irtypes.I64, valAlloca)
	digit := digitLoop.NewURem(cur, ten64)
	ch64 := digitLoop.NewAdd(digit, ascii0)
	ch8 := digitLoop.NewTrunc(ch64, irtypes.I8)
	pos := digitLoop.NewLoad(irtypes.I64, posAlloca)
	slot := digitLoop.NewGetElementPtr(bufType, buf, zero64, pos)
	digitLoop.NewStore(ch8, slot)
	posDec := digitLoop.NewSub(pos, one64)
	digitLoop.NewStore(posDec, posAlloca)
	next := digitLoop.NewUDiv(cur, ten64)
	digitLoop.NewStore(next, valAlloca)
	more := digitLoop.NewICmp(enum.IPredNE, next, zero64)
	done := fn.NewBlock("done")
	digitLoop.NewCondBr(more, digitLoop, done)

	// done: compute start and length, call promise_string_new
	finalPos := done.NewLoad(irtypes.I64, posAlloca)
	start := done.NewAdd(finalPos, one64)
	strLen := done.NewSub(constant.NewInt(irtypes.I64, 20), start)
	dataPtr := done.NewGetElementPtr(bufType, buf, zero64, start)
	result := done.NewCall(c.funcs["promise_string_new"], dataPtr, strLen)
	done.NewRet(result)

	c.funcs["promise_uint_to_string"] = fn
}

// defineF64ToStringFunc emits an LLVM IR function that converts a double to its
// string representation using libc snprintf with "%g" format.
// Replaces the C runtime promise_f64_to_string.
func (c *Compiler) defineF64ToStringFunc() {
	xParam := ir.NewParam("x", irtypes.Double)
	fn := c.module.NewFunc("promise_f64_to_string", irtypes.I8Ptr, xParam)

	bufType := irtypes.NewArray(64, irtypes.I8)

	// Global format string "%g\0"
	fmtData := constant.NewCharArrayFromString("%g\x00")
	fmtGlobal := c.module.NewGlobalDef(fmt.Sprintf(".str.fmt.g.%d", c.strCounter), fmtData)
	c.strCounter++
	fmtGlobal.Immutable = true

	entry := fn.NewBlock("entry")
	buf := entry.NewAlloca(bufType)
	bufPtr := entry.NewGetElementPtr(bufType, buf,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
	fmtPtr := entry.NewGetElementPtr(fmtGlobal.ContentType, fmtGlobal,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))

	len32 := entry.NewCall(c.funcs["snprintf"],
		bufPtr, constant.NewInt(irtypes.I64, 64), fmtPtr, xParam)
	len64 := entry.NewSExt(len32, irtypes.I64)
	result := entry.NewCall(c.funcs["promise_string_new"], bufPtr, len64)
	entry.NewRet(result)

	c.funcs["promise_f64_to_string"] = fn
}

// defineCharToStringFunc emits an LLVM IR function that converts a Unicode
// codepoint (i32) to its UTF-8 encoded string representation.
// Uses cascading range checks with bitwise encoding.
// Replaces the C runtime promise_char_to_string.
func (c *Compiler) defineCharToStringFunc() {
	cpParam := ir.NewParam("cp", irtypes.I32)
	fn := c.module.NewFunc("promise_char_to_string", irtypes.I8Ptr, cpParam)

	bufType := irtypes.NewArray(4, irtypes.I8)
	zero32 := constant.NewInt(irtypes.I32, 0)

	entry := fn.NewBlock("entry")
	buf := entry.NewAlloca(bufType)

	// Check: cp < 0x80?
	oneByte := fn.NewBlock("one_byte")
	check2 := fn.NewBlock("check_2")
	lt80 := entry.NewICmp(enum.IPredULT, cpParam, constant.NewInt(irtypes.I32, 0x80))
	entry.NewCondBr(lt80, oneByte, check2)

	// one_byte: buf[0] = (i8)cp; return string_new(buf, 1)
	b0_1 := oneByte.NewTrunc(cpParam, irtypes.I8)
	slot0_1 := oneByte.NewGetElementPtr(bufType, buf, zero32, zero32)
	oneByte.NewStore(b0_1, slot0_1)
	bufPtr1 := oneByte.NewGetElementPtr(bufType, buf, zero32, zero32)
	r1 := oneByte.NewCall(c.funcs["promise_string_new"], bufPtr1, constant.NewInt(irtypes.I64, 1))
	oneByte.NewRet(r1)

	// check_2: cp < 0x800?
	twoByte := fn.NewBlock("two_byte")
	check3 := fn.NewBlock("check_3")
	lt800 := check2.NewICmp(enum.IPredULT, cpParam, constant.NewInt(irtypes.I32, 0x800))
	check2.NewCondBr(lt800, twoByte, check3)

	// two_byte: buf[0] = 0xC0 | (cp >> 6); buf[1] = 0x80 | (cp & 0x3F)
	sh6_2 := twoByte.NewLShr(cpParam, constant.NewInt(irtypes.I32, 6))
	b0_2 := twoByte.NewOr(sh6_2, constant.NewInt(irtypes.I32, 0xC0))
	b0_2_8 := twoByte.NewTrunc(b0_2, irtypes.I8)
	slot0_2 := twoByte.NewGetElementPtr(bufType, buf, zero32, zero32)
	twoByte.NewStore(b0_2_8, slot0_2)

	m1_2 := twoByte.NewAnd(cpParam, constant.NewInt(irtypes.I32, 0x3F))
	b1_2 := twoByte.NewOr(m1_2, constant.NewInt(irtypes.I32, 0x80))
	b1_2_8 := twoByte.NewTrunc(b1_2, irtypes.I8)
	slot1_2 := twoByte.NewGetElementPtr(bufType, buf, zero32, constant.NewInt(irtypes.I32, 1))
	twoByte.NewStore(b1_2_8, slot1_2)

	bufPtr2 := twoByte.NewGetElementPtr(bufType, buf, zero32, zero32)
	r2 := twoByte.NewCall(c.funcs["promise_string_new"], bufPtr2, constant.NewInt(irtypes.I64, 2))
	twoByte.NewRet(r2)

	// check_3: cp < 0x10000?
	threeByte := fn.NewBlock("three_byte")
	fourByte := fn.NewBlock("four_byte")
	lt10000 := check3.NewICmp(enum.IPredULT, cpParam, constant.NewInt(irtypes.I32, 0x10000))
	check3.NewCondBr(lt10000, threeByte, fourByte)

	// three_byte: 3-byte UTF-8 encoding
	sh12_3 := threeByte.NewLShr(cpParam, constant.NewInt(irtypes.I32, 12))
	b0_3 := threeByte.NewOr(sh12_3, constant.NewInt(irtypes.I32, 0xE0))
	b0_3_8 := threeByte.NewTrunc(b0_3, irtypes.I8)
	slot0_3 := threeByte.NewGetElementPtr(bufType, buf, zero32, zero32)
	threeByte.NewStore(b0_3_8, slot0_3)

	sh6_3 := threeByte.NewLShr(cpParam, constant.NewInt(irtypes.I32, 6))
	m1_3 := threeByte.NewAnd(sh6_3, constant.NewInt(irtypes.I32, 0x3F))
	b1_3 := threeByte.NewOr(m1_3, constant.NewInt(irtypes.I32, 0x80))
	b1_3_8 := threeByte.NewTrunc(b1_3, irtypes.I8)
	slot1_3 := threeByte.NewGetElementPtr(bufType, buf, zero32, constant.NewInt(irtypes.I32, 1))
	threeByte.NewStore(b1_3_8, slot1_3)

	m2_3 := threeByte.NewAnd(cpParam, constant.NewInt(irtypes.I32, 0x3F))
	b2_3 := threeByte.NewOr(m2_3, constant.NewInt(irtypes.I32, 0x80))
	b2_3_8 := threeByte.NewTrunc(b2_3, irtypes.I8)
	slot2_3 := threeByte.NewGetElementPtr(bufType, buf, zero32, constant.NewInt(irtypes.I32, 2))
	threeByte.NewStore(b2_3_8, slot2_3)

	bufPtr3 := threeByte.NewGetElementPtr(bufType, buf, zero32, zero32)
	r3 := threeByte.NewCall(c.funcs["promise_string_new"], bufPtr3, constant.NewInt(irtypes.I64, 3))
	threeByte.NewRet(r3)

	// four_byte: 4-byte UTF-8 encoding
	sh18_4 := fourByte.NewLShr(cpParam, constant.NewInt(irtypes.I32, 18))
	b0_4 := fourByte.NewOr(sh18_4, constant.NewInt(irtypes.I32, 0xF0))
	b0_4_8 := fourByte.NewTrunc(b0_4, irtypes.I8)
	slot0_4 := fourByte.NewGetElementPtr(bufType, buf, zero32, zero32)
	fourByte.NewStore(b0_4_8, slot0_4)

	sh12_4 := fourByte.NewLShr(cpParam, constant.NewInt(irtypes.I32, 12))
	m1_4 := fourByte.NewAnd(sh12_4, constant.NewInt(irtypes.I32, 0x3F))
	b1_4 := fourByte.NewOr(m1_4, constant.NewInt(irtypes.I32, 0x80))
	b1_4_8 := fourByte.NewTrunc(b1_4, irtypes.I8)
	slot1_4 := fourByte.NewGetElementPtr(bufType, buf, zero32, constant.NewInt(irtypes.I32, 1))
	fourByte.NewStore(b1_4_8, slot1_4)

	sh6_4 := fourByte.NewLShr(cpParam, constant.NewInt(irtypes.I32, 6))
	m2_4 := fourByte.NewAnd(sh6_4, constant.NewInt(irtypes.I32, 0x3F))
	b2_4 := fourByte.NewOr(m2_4, constant.NewInt(irtypes.I32, 0x80))
	b2_4_8 := fourByte.NewTrunc(b2_4, irtypes.I8)
	slot2_4 := fourByte.NewGetElementPtr(bufType, buf, zero32, constant.NewInt(irtypes.I32, 2))
	fourByte.NewStore(b2_4_8, slot2_4)

	m3_4 := fourByte.NewAnd(cpParam, constant.NewInt(irtypes.I32, 0x3F))
	b3_4 := fourByte.NewOr(m3_4, constant.NewInt(irtypes.I32, 0x80))
	b3_4_8 := fourByte.NewTrunc(b3_4, irtypes.I8)
	slot3_4 := fourByte.NewGetElementPtr(bufType, buf, zero32, constant.NewInt(irtypes.I32, 3))
	fourByte.NewStore(b3_4_8, slot3_4)

	bufPtr4 := fourByte.NewGetElementPtr(bufType, buf, zero32, zero32)
	r4 := fourByte.NewCall(c.funcs["promise_string_new"], bufPtr4, constant.NewInt(irtypes.I64, 4))
	fourByte.NewRet(r4)

	c.funcs["promise_char_to_string"] = fn
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

// compilerState captures the mutable compiler fields that defineMethodFunc overwrites.
// Used to save/restore state when synthesizing default methods during another function's codegen.
type compilerState struct {
	fn             *ir.Func
	block          *ir.Block
	locals         map[string]*ir.InstAlloca
	dropFlags      map[string]*ir.InstAlloca
	blockCounter   int
	canError       bool
	currentRetType types.Type
	currentNamed   *types.Named
	scopeBindings  []scopeBinding
	loopScopeDepth int
	selfSubst      *selfSubstInfo
	targetType     types.Type
}

func (c *Compiler) saveState() compilerState {
	return compilerState{
		fn:             c.fn,
		block:          c.block,
		locals:         c.locals,
		dropFlags:      c.dropFlags,
		blockCounter:   c.blockCounter,
		canError:       c.canError,
		currentRetType: c.currentRetType,
		currentNamed:   c.currentNamed,
		scopeBindings:  c.scopeBindings,
		loopScopeDepth: c.loopScopeDepth,
		selfSubst:      c.selfSubst,
		targetType:     c.targetType,
	}
}

func (c *Compiler) restoreState(s compilerState) {
	c.fn = s.fn
	c.block = s.block
	c.locals = s.locals
	c.dropFlags = s.dropFlags
	c.blockCounter = s.blockCounter
	c.canError = s.canError
	c.currentRetType = s.currentRetType
	c.currentNamed = s.currentNamed
	c.scopeBindings = s.scopeBindings
	c.loopScopeDepth = s.loopScopeDepth
	c.selfSubst = s.selfSubst
	c.targetType = s.targetType
}

// synthesizeDefaultMethods generates LLVM functions for default methods from
// a structural interface that a concrete type does not override.
// Called lazily when a view vtable is needed for (concrete, iface).
func (c *Compiler) synthesizeDefaultMethods(concrete, iface *types.Named) {
	// Find the interface's AST TypeDecl to get method bodies
	ifaceTD := c.findTypeDecl(c.file, iface.Obj().Name())
	if ifaceTD == nil {
		return
	}

	concreteName := concrete.Obj().Name()

	for _, md := range ifaceTD.Methods {
		if md.Body == nil {
			continue // abstract method — skip
		}

		// Check if concrete already has this method (override)
		ifaceMethod := c.lookupAnyMethod(iface, md.Name, md.IsGetter, md.IsSetter)
		if ifaceMethod == nil || ifaceMethod.IsAbstract() {
			continue
		}
		concreteMethod := c.lookupAnyMethod(concrete, md.Name, md.IsGetter, md.IsSetter)
		if concreteMethod != nil && !concreteMethod.IsAbstract() {
			continue // concrete type overrides the default
		}

		mangledName := mangleMethodName(concreteName, md.Name, md.IsSetter)
		if _, exists := c.funcs[mangledName]; exists {
			continue // already synthesized
		}

		sig := ifaceMethod.Sig()

		// Save all compiler state — we're in the middle of codegen for another function
		saved := c.saveState()
		c.selfSubst = &selfSubstInfo{iface: iface, concrete: concrete}

		var params []*ir.Param
		if sig.Recv() != nil {
			params = append(params, ir.NewParam("this", irtypes.I8Ptr))
		}
		for _, p := range sig.Params() {
			params = append(params, ir.NewParam(p.Name(), c.resolveType(p.Type())))
		}

		retType := irtypes.Type(irtypes.Void)
		if sig.Result() != nil {
			retType = c.resolveType(sig.Result())
		}
		if sig.CanError() {
			retType = computeResultType(retType)
		}

		fn := c.module.NewFunc(mangledName, retType, params...)
		c.funcs[mangledName] = fn // register BEFORE body generation (prevents recursion)

		// Generate the body with selfSubst active
		c.defineMethodFunc(md, ifaceMethod, fn, concrete)

		// Restore compiler state
		c.restoreState(saved)
	}

	// Recurse into parent interfaces (e.g., Ordered inherits != default from Equal)
	for _, parent := range iface.Parents() {
		if parent.IsStructural() {
			c.synthesizeDefaultMethods(concrete, parent)
		}
	}
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
