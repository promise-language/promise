package codegen

import (
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"

	"github.com/llir/llvm/ir"
	"github.com/llir/llvm/ir/constant"
	"github.com/llir/llvm/ir/enum"
	irtypes "github.com/llir/llvm/ir/types"
	"github.com/llir/llvm/ir/value"

	"djabi.dev/go/promise_lang/internal/ast"
	"djabi.dev/go/promise_lang/internal/codegen/pal"
	"djabi.dev/go/promise_lang/internal/sema"
	"djabi.dev/go/promise_lang/internal/types"
)

// rawFuncAttr is a function attribute emitted as a bare keyword (no quotes).
// This is needed for attributes like "presplitcoroutine" which LLVM only
// recognizes as enum-style keywords, not as quoted string attributes.
type rawFuncAttr string

func (a rawFuncAttr) IsFuncAttribute() {}
func (a rawFuncAttr) String() string   { return string(a) }

// Compiler generates LLVM IR from a type-checked Promise AST.
type Compiler struct {
	module         *ir.Module
	info           *sema.Info
	fn             *ir.Func                         // current function being generated
	block          *ir.Block                        // current basic block
	entryBlock     *ir.Block                        // entry block of current function (for allocas)
	locals         map[string]*ir.InstAlloca        // local variable allocas
	localNameCount map[string]int                   // per-function alloca name counter for dedup
	funcs          map[string]*ir.Func              // declared Promise functions by name
	stdFuncs       map[string]*ir.Func              // std library functions by name (for std.X)
	stdExterns     map[string]*ExternFunc           // std library externs by name (for std.X)
	layouts        map[*types.Named]*TypeDeclLayout // type layouts for extern ABI
	enumLayouts    map[*types.Enum]*TypeDeclLayout  // enum type layouts
	externs        map[string]*ExternFunc           // extern functions by Promise name

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

	// Drop binding tracking: maps variable name to its scope binding (for reassignment drop)
	dropBindings map[string]scopeBinding

	// PAL (Platform Abstraction Layer) function references
	palWrite   *ir.Func // @pal_write(i32 fd, i8* buf, i64 len) → i64
	palExit    *ir.Func // @pal_exit(i32 code) → void [noreturn]
	palAlloc   *ir.Func // @pal_alloc(i64 size) → i8*
	palFree    *ir.Func // @pal_free(i8* ptr) → void
	palRealloc *ir.Func // @pal_realloc(i8* ptr, i64 size) → i8*

	// PAL threading primitives (Phase 5)
	palThreadCreate  *ir.Func // @pal_thread_create(i8* fn, i8* arg) → i8*
	palThreadJoin    *ir.Func // @pal_thread_join(i8* handle) → void
	palMutexInit     *ir.Func // @pal_mutex_init() → i8*
	palMutexLock     *ir.Func // @pal_mutex_lock(i8* mutex) → void
	palMutexUnlock   *ir.Func // @pal_mutex_unlock(i8* mutex) → void
	palMutexDestroy  *ir.Func // @pal_mutex_destroy(i8* mutex) → void
	palCondInit      *ir.Func // @pal_cond_init() → i8*
	palCondWait      *ir.Func // @pal_cond_wait(i8* cond, i8* mutex) → void
	palCondSignal    *ir.Func // @pal_cond_signal(i8* cond) → void
	palCondBroadcast *ir.Func // @pal_cond_broadcast(i8* cond) → void
	palCondDestroy   *ir.Func // @pal_cond_destroy(i8* cond) → void
	palUsleep        *ir.Func // @usleep(i32 usec) → i32

	// LLVM coroutine intrinsics (Phase 5c — M:N scheduler)
	coroId      *ir.Func // @llvm.coro.id(i32, i8*, i8*, i8*) → token
	coroAlloc   *ir.Func // @llvm.coro.alloc(token) → i1
	coroBegin   *ir.Func // @llvm.coro.begin(token, i8*) → i8*
	coroSize    *ir.Func // @llvm.coro.size.i64() → i64
	coroSuspend *ir.Func // @llvm.coro.suspend(token, i1) → i8
	coroEnd     *ir.Func // @llvm.coro.end(i8*, i1, token) → i1
	coroFree    *ir.Func // @llvm.coro.free(token, i8*) → i8*
	coroResume  *ir.Func // @llvm.coro.resume(i8*) → void
	coroDestroy *ir.Func // @llvm.coro.destroy(i8*) → void
	coroDone    *ir.Func // @llvm.coro.done(i8*) → i1

	// PAL scheduler primitives (Phase 5c)
	palNumCPUs *ir.Func // @pal_num_cpus() → i32

	// Scheduler globals (Phase 5c — M:N scheduler)
	currentGGlobal    *ir.Global // @__promise_current_g (TLS, i8*)
	currentPGlobal    *ir.Global // @__promise_current_p (TLS, i8*) — current P for local queue ops
	schedGlobal       *ir.Global // @__promise_sched (global Sched struct)
	panicJmpBufGlobal *ir.Global // @__promise_panic_jmpbuf (TLS, i8*) — setjmp buf for panic recovery
	inCoroutine       bool       // true when compiling inside a go block coroutine body
	coroCleanupBlk    *ir.Block  // coroutine cleanup block (destroy path: coro.free + free)
	coroSuspendBlk    *ir.Block  // coroutine suspend block (suspend path: coro.end + ret)

	// Main function AST — saved so wrapMainWithScheduler can compile it inline
	mainDecl *ast.FuncDecl

	// Go expression counter for unique trampoline function names
	goCounter int

	// Global constants for print/panic functions
	newlineGlobal     *ir.Global // "\n" (1 byte)
	panicPrefixGlobal *ir.Global // "panic: " (7 bytes)
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
// On macOS, dynamically detects the OS version via sw_vers to ensure the
// triple matches what clang expects (avoids module triple override warnings
// and potential ABI mismatches in coroutine lowering).
func HostTargetTriple() string {
	switch runtime.GOOS {
	case "darwin":
		arch := "x86_64"
		if runtime.GOARCH == "arm64" {
			arch = "arm64"
		}
		// Dynamically detect macOS version for correct triple
		if out, err := exec.Command("sw_vers", "-productVersion").Output(); err == nil {
			ver := strings.TrimSpace(string(out))
			// Use major.0.0 form (e.g. "26.3" → "26.0.0")
			parts := strings.Split(ver, ".")
			if len(parts) >= 1 {
				return arch + "-apple-macosx" + parts[0] + ".0.0"
			}
		}
		// Fallback if sw_vers fails
		if runtime.GOARCH == "arm64" {
			return "arm64-apple-macosx14.0.0"
		}
		return "x86_64-apple-macosx10.15.0"
	case "linux":
		// Default to musl for fully static binaries.
		// PROMISE_USE_CLANG=1 switches to gnu for dynamic glibc linking.
		libc := "musl"
		if os.Getenv("PROMISE_USE_CLANG") == "1" {
			libc = "gnu"
		}
		if runtime.GOARCH == "arm64" {
			return "aarch64-unknown-linux-" + libc
		}
		return "x86_64-unknown-linux-" + libc
	case "windows":
		if runtime.GOARCH == "arm64" {
			return "aarch64-pc-windows-msvc"
		}
		return "x86_64-pc-windows-msvc"
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
		dropBindings:    make(map[string]scopeBinding),
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

	// Add PAL-based function bodies to print/panic declarations.
	// Must run after declareIntrinsics (to-string funcs) and declareExterns (print funcs).
	c.definePALBodies()

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

	// Wrap user main() as G0 in the M:N scheduler
	c.wrapMainWithScheduler()

	return &CompileResult{
		Module:      c.module,
		Layouts:     c.layouts,
		EnumLayouts: c.enumLayouts,
		Externs:     externList,
		compiler:    c,
	}
}

// GenerateTestMain replaces the user's main() with a test runner that calls
// each test function via a codegen-emitted thread-based runner.
func (r *CompileResult) GenerateTestMain(tests []*types.Func) {
	c := r.compiler

	// Codegen-emitted test runner — replaces the C extern (fork/waitpid).
	// Runs each test in a thread via PAL. If the test panics, pal_exit
	// terminates the whole process (no fork isolation). Same as Go's testing.
	testRunFn := c.defineTestRunFunc()
	nanotimeFn := c.defineNanotimeFunc()
	testPrintFn := c.module.NewFunc("promise_test_print_result",
		irtypes.Void,
		ir.NewParam("name", irtypes.I8Ptr),
		ir.NewParam("failed", irtypes.I32),
		ir.NewParam("elapsed_ns", irtypes.I64),
	)
	testSummaryFn := c.module.NewFunc("promise_test_summary",
		irtypes.Void,
		ir.NewParam("passed", irtypes.I32),
		ir.NewParam("failed", irtypes.I32),
	)

	// Add codegen bodies (replaces C printf implementations)
	c.defineTestPrintResultBody(testPrintFn)
	c.defineTestSummaryBody(testSummaryFn)

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

	// Allocate array for failed test name pointers and a counter
	totalTests := len(tests)
	failedNamesArrayType := irtypes.NewArray(uint64(totalTests), irtypes.I8Ptr)
	failedNamesAlloca := entry.NewAlloca(failedNamesArrayType)
	failedCountAlloca := entry.NewAlloca(irtypes.I32)
	entry.NewStore(constant.NewInt(irtypes.I32, 0), failedCountAlloca)

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

		// Time the test: t0 = nanotime()
		t0 := entry.NewCall(nanotimeFn)

		// Call promise_test_run(fn) -> i32 (0=pass, 1=fail)
		result := entry.NewCall(testRunFn, fnPtr)

		// t1 = nanotime(); elapsed = t1 - t0
		t1 := entry.NewCall(nanotimeFn)
		elapsed := entry.NewSub(t1, t0)

		// Get name pointer
		namePtr := entry.NewGetElementPtr(
			constant.NewCharArrayFromString(nameStr+"\x00").Typ,
			nameGlobal,
			constant.NewInt(irtypes.I64, 0),
			constant.NewInt(irtypes.I64, 0),
		)

		// Print result with timing
		entry.NewCall(testPrintFn, namePtr, result, elapsed)

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

		// If failed, store name pointer in failedNames array
		failStoreBlock := mainFn.NewBlock(fmt.Sprintf("store_fail_%s", nameStr))
		skipStoreBlock := mainFn.NewBlock(fmt.Sprintf("skip_fail_%s", nameStr))
		isFail := entry.NewICmp(enum.IPredNE, result, constant.NewInt(irtypes.I32, 0))
		entry.NewCondBr(isFail, failStoreBlock, skipStoreBlock)

		// Store the name pointer at failedNames[failedCount], then increment
		failIdx := failStoreBlock.NewLoad(irtypes.I32, failedCountAlloca)
		failSlot := failStoreBlock.NewGetElementPtr(failedNamesArrayType, failedNamesAlloca,
			constant.NewInt(irtypes.I32, 0), failIdx)
		failStoreBlock.NewStore(namePtr, failSlot)
		failStoreBlock.NewStore(
			failStoreBlock.NewAdd(failIdx, constant.NewInt(irtypes.I32, 1)),
			failedCountAlloca,
		)
		failStoreBlock.NewBr(skipStoreBlock)

		// Continue from skipStoreBlock for the next test
		entry = skipStoreBlock
	}

	// Print summary
	finalPassed := entry.NewLoad(irtypes.I32, passedAlloca)
	finalFailed := entry.NewLoad(irtypes.I32, failedAlloca)
	entry.NewCall(testSummaryFn, finalPassed, finalFailed)

	// Print FAILED: list if any failures
	failedHeaderData := constant.NewCharArrayFromString("FAILED:\n")
	failedHeaderGlobal := c.module.NewGlobalDef(".str.failed_header", failedHeaderData)
	failedHeaderGlobal.Immutable = true
	failedIndentData := constant.NewCharArrayFromString("  ")
	failedIndentGlobal := c.module.NewGlobalDef(".str.failed_indent", failedIndentData)
	failedIndentGlobal.Immutable = true
	stdout := constant.NewInt(irtypes.I32, 1)

	hasFailures := entry.NewICmp(enum.IPredSGT, finalFailed, constant.NewInt(irtypes.I32, 0))
	printFailBlock := mainFn.NewBlock("print_failures")
	doneBlock := mainFn.NewBlock("done")
	entry.NewCondBr(hasFailures, printFailBlock, doneBlock)

	// Print "FAILED:\n" header
	headerPtr := printFailBlock.NewGetElementPtr(failedHeaderGlobal.ContentType, failedHeaderGlobal,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
	printFailBlock.NewCall(c.palWrite, stdout, headerPtr, constant.NewInt(irtypes.I64, 8))

	// Loop through failed names
	loopBlock := mainFn.NewBlock("fail_loop")
	loopEndBlock := mainFn.NewBlock("fail_loop_end")
	printFailBlock.NewBr(loopBlock)

	// Loop index phi
	idxPhi := loopBlock.NewPhi(ir.NewIncoming(constant.NewInt(irtypes.I32, 0), printFailBlock))

	// Load name pointer from array
	nameSlot := loopBlock.NewGetElementPtr(failedNamesArrayType, failedNamesAlloca,
		constant.NewInt(irtypes.I32, 0), idxPhi)
	failedNamePtr := loopBlock.NewLoad(irtypes.I8Ptr, nameSlot)

	// Print "  " + name + "\n"
	indentPtr := loopBlock.NewGetElementPtr(failedIndentGlobal.ContentType, failedIndentGlobal,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
	loopBlock.NewCall(c.palWrite, stdout, indentPtr, constant.NewInt(irtypes.I64, 2))
	failedNameLen := loopBlock.NewCall(c.funcs["strlen"], failedNamePtr)
	loopBlock.NewCall(c.palWrite, stdout, failedNamePtr, failedNameLen)
	nlPtr := loopBlock.NewGetElementPtr(c.newlineGlobal.ContentType, c.newlineGlobal,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
	loopBlock.NewCall(c.palWrite, stdout, nlPtr, constant.NewInt(irtypes.I64, 1))

	// Increment and check
	nextIdx := loopBlock.NewAdd(idxPhi, constant.NewInt(irtypes.I32, 1))
	totalFailedCount := loopBlock.NewLoad(irtypes.I32, failedCountAlloca)
	loopDone := loopBlock.NewICmp(enum.IPredSGE, nextIdx, totalFailedCount)
	idxPhi.Incs = append(idxPhi.Incs, ir.NewIncoming(nextIdx, loopBlock))
	loopBlock.NewCondBr(loopDone, loopEndBlock, loopBlock)

	loopEndBlock.NewBr(doneBlock)

	// Return 0 if all passed, 1 if any failed
	retHasFailures := doneBlock.NewICmp(enum.IPredSGT, finalFailed, constant.NewInt(irtypes.I32, 0))
	retVal := doneBlock.NewSelect(retHasFailures, constant.NewInt(irtypes.I32, 1), constant.NewInt(irtypes.I32, 0))
	doneBlock.NewRet(retVal)
}

// declareIntrinsics declares compiler-intrinsic runtime functions (not user-declared externs).
func (c *Compiler) declareIntrinsics() {
	panicFn := c.module.NewFunc("promise_panic",
		irtypes.Void, ir.NewParam("msg", irtypes.I8Ptr))
	panicFn.FuncAttrs = append(panicFn.FuncAttrs, enum.FuncAttrNoReturn, enum.FuncAttrNoUnwind)
	c.funcs["promise_panic"] = panicFn

	// PAL: emit platform-specific allocator primitives (needed by string/vector funcs below)
	p := pal.ForTarget(c.module.TargetTriple)
	c.palAlloc = p.EmitAlloc(c.module)
	c.palFree = p.EmitFree(c.module)
	c.palRealloc = p.EmitRealloc(c.module)

	// PAL: emit threading primitives (Phase 5 — needed by go/receive codegen)
	c.palThreadCreate = p.EmitThreadCreate(c.module)
	c.palThreadJoin = p.EmitThreadJoin(c.module)
	c.palMutexInit = p.EmitMutexInit(c.module)
	c.palMutexLock = p.EmitMutexLock(c.module)
	c.palMutexUnlock = p.EmitMutexUnlock(c.module)
	c.palMutexDestroy = p.EmitMutexDestroy(c.module)
	c.palCondInit = p.EmitCondInit(c.module)
	c.palCondWait = p.EmitCondWait(c.module)
	c.palCondSignal = p.EmitCondSignal(c.module)
	c.palCondBroadcast = p.EmitCondBroadcast(c.module)
	c.palCondDestroy = p.EmitCondDestroy(c.module)

	// usleep — POSIX function for brief polling delays in thread-blocking mode
	c.palUsleep = c.module.NewFunc("usleep", irtypes.I32, ir.NewParam("usec", irtypes.I32))
	c.palUsleep.FuncAttrs = append(c.palUsleep.FuncAttrs, enum.FuncAttrNoUnwind)

	// PAL: scheduler primitives (Phase 5c)
	c.palNumCPUs = p.EmitNumCPUs(c.module)

	// LLVM memcpy/memmove intrinsics (used instead of libc memcpy/memmove)
	c.funcs["llvm.memcpy"] = c.module.NewFunc("llvm.memcpy.p0i8.p0i8.i64",
		irtypes.Void,
		ir.NewParam("dest", irtypes.I8Ptr),
		ir.NewParam("src", irtypes.I8Ptr),
		ir.NewParam("len", irtypes.I64),
		ir.NewParam("isvolatile", irtypes.I1))
	c.funcs["llvm.memmove"] = c.module.NewFunc("llvm.memmove.p0i8.p0i8.i64",
		irtypes.Void,
		ir.NewParam("dest", irtypes.I8Ptr),
		ir.NewParam("src", irtypes.I8Ptr),
		ir.NewParam("len", irtypes.I64),
		ir.NewParam("isvolatile", irtypes.I1))

	// String new/concat (codegen-emitted LLVM IR, replaces C runtime)
	c.defineStringNewFunc()
	c.defineStringConcatFunc()

	// Memcmp (SIMD-accelerated; no @llvm.memcmp intrinsic exists)
	// Used by string equality, vector contains, and string split
	memcmpS1 := ir.NewParam("s1", irtypes.I8Ptr)
	memcmpS1.Attrs = append(memcmpS1.Attrs, enum.ParamAttrNoCapture, enum.ParamAttrNoUndef)
	memcmpS2 := ir.NewParam("s2", irtypes.I8Ptr)
	memcmpS2.Attrs = append(memcmpS2.Attrs, enum.ParamAttrNoCapture, enum.ParamAttrNoUndef)
	memcmpN := ir.NewParam("n", irtypes.I64)
	memcmpN.Attrs = append(memcmpN.Attrs, enum.ParamAttrNoUndef)
	memcmpFn := c.module.NewFunc("memcmp", irtypes.I32, memcmpS1, memcmpS2, memcmpN)
	memcmpFn.FuncAttrs = append(memcmpFn.FuncAttrs,
		enum.FuncAttrMustProgress, enum.FuncAttrNoUnwind,
		enum.FuncAttrReadOnly, enum.FuncAttrWillReturn, enum.FuncAttrArgMemOnly)
	c.funcs["memcmp"] = memcmpFn

	// String direct equality (codegen-emitted LLVM IR, replaces C runtime)
	c.defineStringDirectEqFunc()

	// Vector methods (codegen-emitted LLVM IR, replaces C runtime)
	c.defineVectorWithCapacityFunc()
	c.defineVectorPushFunc()
	c.defineVectorPopFunc()
	c.defineVectorContainsFunc()
	c.defineVectorRemoveFunc()

	// String trim/split (codegen-emitted LLVM IR, replaces C runtime)
	c.defineStringTrimFunc()
	c.defineStringSplitFunc()

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

	// String next_char UTF-8 decoder (codegen-emitted LLVM IR, replaces C runtime)
	c.defineStringNextCharFunc()

	// RTTI type check (codegen-emitted LLVM IR, replaces C runtime)
	c.defineTypeIsFunc()

	// String hash function (codegen-emitted LLVM IR, replaces C runtime)
	c.defineStringHashFunc()

	// String equality comparison (codegen-emitted LLVM IR, replaces C runtime)
	c.defineStringEqFunc()

	// Channel constructor (codegen-emitted LLVM IR)
	c.defineChannelNewFunc()

	// LLVM coroutine intrinsics (Phase 5c — M:N scheduler)
	c.declareCoroIntrinsics()

	// setjmp/longjmp (libc) — used for goroutine-level panic recovery.
	// Declared before scheduler functions since defineSchedLoopFunc uses them.
	// On POSIX, _setjmp/_longjmp don't save/restore signal masks (faster).
	setjmpFn := c.module.NewFunc("_setjmp", irtypes.I32,
		ir.NewParam("env", irtypes.I8Ptr))
	setjmpFn.FuncAttrs = append(setjmpFn.FuncAttrs, enum.FuncAttrNoUnwind)
	c.funcs["setjmp"] = setjmpFn

	longjmpFn := c.module.NewFunc("_longjmp", irtypes.Void,
		ir.NewParam("env", irtypes.I8Ptr),
		ir.NewParam("val", irtypes.I32))
	longjmpFn.FuncAttrs = append(longjmpFn.FuncAttrs, enum.FuncAttrNoReturn, enum.FuncAttrNoUnwind)
	c.funcs["longjmp"] = longjmpFn

	// Scheduler globals and functions (Phase 5c)
	c.defineSchedulerGlobals()
	c.defineGNewFunc()
	c.defineI64MaxFunc()
	c.defineLocalEnqueueFunc()
	c.defineLocalDequeueFunc()
	c.defineStealWorkFunc()
	c.defineSchedWakeMFunc()
	c.defineSchedFindRunnableFunc()
	c.defineSchedEnqueueFunc()
	c.defineGoroutineExitFunc()
	c.defineSchedParkMFunc()
	c.defineSchedLoopFunc()
	c.defineSysmonFunc()
	c.defineSchedInitFunc()
	c.defineSchedRunUntilMainFunc()
	c.defineSchedShutdownFunc()
	c.defineWaiterEnqueueFunc()
	c.defineWaiterDequeueFunc()
	c.defineWaiterWakeAllFunc()
	c.defineWaiterRemoveFunc()

	// PAL: emit platform-specific IO/exit primitives
	c.palWrite = p.EmitWrite(c.module)
	c.palExit = p.EmitExit(c.module)

	// strlen (libc) — needed by definePanicBody to get C string length
	strlenFn := c.module.NewFunc("strlen", irtypes.I64,
		ir.NewParam("s", irtypes.I8Ptr))
	strlenFn.FuncAttrs = append(strlenFn.FuncAttrs,
		enum.FuncAttrNoUnwind, enum.FuncAttrReadOnly, enum.FuncAttrWillReturn)
	c.funcs["strlen"] = strlenFn

}

// defineStringNewFunc emits an LLVM IR function that allocates and initializes
// a string instance. Replaces the C runtime promise_string_new.
// Allocates header (16 bytes) + data, copies data via @llvm.memcpy intrinsic.
// String instance layout: { i8* _variant, i64 len, [0 x i8] data }
func (c *Compiler) defineStringNewFunc() {
	dataParam := ir.NewParam("data", irtypes.I8Ptr)
	lenParam := ir.NewParam("len", irtypes.I64)
	fn := c.module.NewFunc("promise_string_new", irtypes.I8Ptr, dataParam, lenParam)

	strInstanceType := irtypes.NewStruct(
		irtypes.I8Ptr,                   // _variant
		irtypes.I64,                     // len
		irtypes.NewArray(0, irtypes.I8), // data (flexible array)
	)

	headerSize := constant.NewInt(irtypes.I64, 16)

	// OOM panic message
	oomMsg := constant.NewCharArrayFromString("out of memory\x00")
	oomGlobal := c.module.NewGlobalDef(
		fmt.Sprintf(".str.oom.%d", c.strCounter), oomMsg)
	c.strCounter++
	oomGlobal.Immutable = true

	// entry: allocate and null-check
	entry := fn.NewBlock("entry")
	allocSize := entry.NewAdd(headerSize, lenParam)
	rawPtr := entry.NewCall(c.palAlloc, allocSize)
	isNull := entry.NewICmp(enum.IPredEQ, rawPtr, constant.NewNull(irtypes.I8Ptr))

	oomBlk := fn.NewBlock("oom")
	initBlk := fn.NewBlock("init")
	entry.NewCondBr(isNull, oomBlk, initBlk)

	// oom: panic
	msgPtr := oomBlk.NewGetElementPtr(oomGlobal.ContentType, oomGlobal,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
	oomBlk.NewCall(c.funcs["promise_panic"], msgPtr)
	oomBlk.NewUnreachable()

	// init: store fields and copy data
	typedPtr := initBlk.NewBitCast(rawPtr, irtypes.NewPointer(strInstanceType))

	variantPtr := initBlk.NewGetElementPtr(strInstanceType, typedPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
	initBlk.NewStore(constant.NewNull(irtypes.I8Ptr), variantPtr)

	lenPtr := initBlk.NewGetElementPtr(strInstanceType, typedPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 1))
	initBlk.NewStore(lenParam, lenPtr)

	dataDst := initBlk.NewGetElementPtr(strInstanceType, typedPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 2),
		constant.NewInt(irtypes.I32, 0))
	initBlk.NewCall(c.funcs["llvm.memcpy"], dataDst, dataParam, lenParam, constant.False)

	initBlk.NewRet(rawPtr)

	c.funcs["promise_string_new"] = fn
}

// defineStringConcatFunc emits an LLVM IR function that concatenates two strings.
// Replaces the C runtime promise_string_concat.
// Loads lengths from both inputs, allocates header + total, copies both data regions.
// String instance layout: { i8* _variant, i64 len, [0 x i8] data }
func (c *Compiler) defineStringConcatFunc() {
	aParam := ir.NewParam("a", irtypes.I8Ptr)
	bParam := ir.NewParam("b", irtypes.I8Ptr)
	fn := c.module.NewFunc("promise_string_concat", irtypes.I8Ptr, aParam, bParam)

	strInstanceType := irtypes.NewStruct(
		irtypes.I8Ptr,                   // _variant
		irtypes.I64,                     // len
		irtypes.NewArray(0, irtypes.I8), // data (flexible array)
	)

	headerSize := constant.NewInt(irtypes.I64, 16)

	// OOM panic message
	oomMsg := constant.NewCharArrayFromString("out of memory\x00")
	oomGlobal := c.module.NewGlobalDef(
		fmt.Sprintf(".str.oom.%d", c.strCounter), oomMsg)
	c.strCounter++
	oomGlobal.Immutable = true

	// entry: load lengths, compute total, allocate, null-check
	entry := fn.NewBlock("entry")

	typedA := entry.NewBitCast(aParam, irtypes.NewPointer(strInstanceType))
	lenPtrA := entry.NewGetElementPtr(strInstanceType, typedA,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 1))
	lenA := entry.NewLoad(irtypes.I64, lenPtrA)

	typedB := entry.NewBitCast(bParam, irtypes.NewPointer(strInstanceType))
	lenPtrB := entry.NewGetElementPtr(strInstanceType, typedB,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 1))
	lenB := entry.NewLoad(irtypes.I64, lenPtrB)

	total := entry.NewAdd(lenA, lenB)
	allocSize := entry.NewAdd(headerSize, total)
	rawPtr := entry.NewCall(c.palAlloc, allocSize)
	isNull := entry.NewICmp(enum.IPredEQ, rawPtr, constant.NewNull(irtypes.I8Ptr))

	oomBlk := fn.NewBlock("oom")
	initBlk := fn.NewBlock("init")
	entry.NewCondBr(isNull, oomBlk, initBlk)

	// oom: panic
	msgPtr := oomBlk.NewGetElementPtr(oomGlobal.ContentType, oomGlobal,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
	oomBlk.NewCall(c.funcs["promise_panic"], msgPtr)
	oomBlk.NewUnreachable()

	// init: store header fields and copy both data regions
	typedNew := initBlk.NewBitCast(rawPtr, irtypes.NewPointer(strInstanceType))

	variantPtr := initBlk.NewGetElementPtr(strInstanceType, typedNew,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
	initBlk.NewStore(constant.NewNull(irtypes.I8Ptr), variantPtr)

	lenPtr := initBlk.NewGetElementPtr(strInstanceType, typedNew,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 1))
	initBlk.NewStore(total, lenPtr)

	dataDst := initBlk.NewGetElementPtr(strInstanceType, typedNew,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 2),
		constant.NewInt(irtypes.I32, 0))

	// Copy a's data
	dataA := initBlk.NewGetElementPtr(strInstanceType, typedA,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 2),
		constant.NewInt(irtypes.I32, 0))
	initBlk.NewCall(c.funcs["llvm.memcpy"], dataDst, dataA, lenA, constant.False)

	// Copy b's data after a's
	dstOffset := initBlk.NewGetElementPtr(irtypes.I8, dataDst, lenA)
	dataB := initBlk.NewGetElementPtr(strInstanceType, typedB,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 2),
		constant.NewInt(irtypes.I32, 0))
	initBlk.NewCall(c.funcs["llvm.memcpy"], dstOffset, dataB, lenB, constant.False)

	initBlk.NewRet(rawPtr)

	c.funcs["promise_string_concat"] = fn
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

	// Compare data using memcmp (SIMD-accelerated)
	dataPtrA := cmpDataBlk.NewGetElementPtr(strInstanceType, typedA,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 2),
		constant.NewInt(irtypes.I32, 0))
	dataPtrB := cmpDataBlk.NewGetElementPtr(strInstanceType, typedB,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 2),
		constant.NewInt(irtypes.I32, 0))

	equalBlk := fn.NewBlock("equal")
	neqBlk := fn.NewBlock("not_equal")

	cmpResult := cmpDataBlk.NewCall(c.funcs["memcmp"], dataPtrA, dataPtrB, lenA)
	isEqual := cmpDataBlk.NewICmp(enum.IPredEQ, cmpResult, constant.NewInt(irtypes.I32, 0))
	cmpDataBlk.NewCondBr(isEqual, equalBlk, neqBlk)

	neqBlk.NewRet(falseVal)
	equalBlk.NewRet(trueVal)

	c.funcs["promise_string_eq"] = fn
}

// defineStringTrimFunc emits an LLVM IR function that returns a new string
// with leading and trailing whitespace removed.
// Whitespace: space (32), tab (9), newline (10), carriage return (13).
// String instance layout: { i8* _variant, i64 len, [0 x i8] data }
func (c *Compiler) defineStringTrimFunc() {
	sParam := ir.NewParam("s", irtypes.I8Ptr)
	fn := c.module.NewFunc("promise_string_trim", irtypes.I8Ptr, sParam)

	strInstanceType := irtypes.NewStruct(
		irtypes.I8Ptr,                   // _variant
		irtypes.I64,                     // len
		irtypes.NewArray(0, irtypes.I8), // data (flexible array)
	)

	zero64 := constant.NewInt(irtypes.I64, 0)
	one64 := constant.NewInt(irtypes.I64, 1)

	// entry: load len, get data pointer, alloca start/end
	entry := fn.NewBlock("entry")
	typedS := entry.NewBitCast(sParam, irtypes.NewPointer(strInstanceType))
	lenPtr := entry.NewGetElementPtr(strInstanceType, typedS,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 1))
	sLen := entry.NewLoad(irtypes.I64, lenPtr)
	dataPtr := entry.NewGetElementPtr(strInstanceType, typedS,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 2),
		constant.NewInt(irtypes.I32, 0))

	startA := entry.NewAlloca(irtypes.I64)
	entry.NewStore(zero64, startA)
	endA := entry.NewAlloca(irtypes.I64)
	entry.NewStore(sLen, endA)

	trimLeftHdr := fn.NewBlock("trim_left_hdr")
	trimLeftChk := fn.NewBlock("trim_left_chk")
	trimLeftAdv := fn.NewBlock("trim_left_adv")
	trimRightHdr := fn.NewBlock("trim_right_hdr")
	trimRightChk := fn.NewBlock("trim_right_chk")
	trimRightAdv := fn.NewBlock("trim_right_adv")
	buildResult := fn.NewBlock("build_result")

	entry.NewBr(trimLeftHdr)

	// trim_left_hdr: start < end?
	start := trimLeftHdr.NewLoad(irtypes.I64, startA)
	end := trimLeftHdr.NewLoad(irtypes.I64, endA)
	leftCond := trimLeftHdr.NewICmp(enum.IPredSLT, start, end)
	trimLeftHdr.NewCondBr(leftCond, trimLeftChk, trimRightHdr)

	// trim_left_chk: is data[start] whitespace?
	startVal := trimLeftChk.NewLoad(irtypes.I64, startA)
	bytePtr := trimLeftChk.NewGetElementPtr(irtypes.I8, dataPtr, startVal)
	b := trimLeftChk.NewLoad(irtypes.I8, bytePtr)
	isSp := trimLeftChk.NewICmp(enum.IPredEQ, b, constant.NewInt(irtypes.I8, 32))
	isTab := trimLeftChk.NewICmp(enum.IPredEQ, b, constant.NewInt(irtypes.I8, 9))
	isNL := trimLeftChk.NewICmp(enum.IPredEQ, b, constant.NewInt(irtypes.I8, 10))
	isCR := trimLeftChk.NewICmp(enum.IPredEQ, b, constant.NewInt(irtypes.I8, 13))
	ws1 := trimLeftChk.NewOr(isSp, isTab)
	ws2 := trimLeftChk.NewOr(ws1, isNL)
	isWs := trimLeftChk.NewOr(ws2, isCR)
	trimLeftChk.NewCondBr(isWs, trimLeftAdv, trimRightHdr)

	// trim_left_adv: start++
	startCur := trimLeftAdv.NewLoad(irtypes.I64, startA)
	nextStart := trimLeftAdv.NewAdd(startCur, one64)
	trimLeftAdv.NewStore(nextStart, startA)
	trimLeftAdv.NewBr(trimLeftHdr)

	// trim_right_hdr: end > start?
	endVal := trimRightHdr.NewLoad(irtypes.I64, endA)
	startVal2 := trimRightHdr.NewLoad(irtypes.I64, startA)
	rightCond := trimRightHdr.NewICmp(enum.IPredSGT, endVal, startVal2)
	trimRightHdr.NewCondBr(rightCond, trimRightChk, buildResult)

	// trim_right_chk: is data[end-1] whitespace?
	endVal2 := trimRightChk.NewLoad(irtypes.I64, endA)
	idxR := trimRightChk.NewSub(endVal2, one64)
	bytePtrR := trimRightChk.NewGetElementPtr(irtypes.I8, dataPtr, idxR)
	bR := trimRightChk.NewLoad(irtypes.I8, bytePtrR)
	isSpR := trimRightChk.NewICmp(enum.IPredEQ, bR, constant.NewInt(irtypes.I8, 32))
	isTabR := trimRightChk.NewICmp(enum.IPredEQ, bR, constant.NewInt(irtypes.I8, 9))
	isNLR := trimRightChk.NewICmp(enum.IPredEQ, bR, constant.NewInt(irtypes.I8, 10))
	isCRR := trimRightChk.NewICmp(enum.IPredEQ, bR, constant.NewInt(irtypes.I8, 13))
	wsR1 := trimRightChk.NewOr(isSpR, isTabR)
	wsR2 := trimRightChk.NewOr(wsR1, isNLR)
	isWsR := trimRightChk.NewOr(wsR2, isCRR)
	trimRightChk.NewCondBr(isWsR, trimRightAdv, buildResult)

	// trim_right_adv: end--
	endCur := trimRightAdv.NewLoad(irtypes.I64, endA)
	prevEnd := trimRightAdv.NewSub(endCur, one64)
	trimRightAdv.NewStore(prevEnd, endA)
	trimRightAdv.NewBr(trimRightHdr)

	// build_result: create new string from data[start..end]
	finalStart := buildResult.NewLoad(irtypes.I64, startA)
	finalEnd := buildResult.NewLoad(irtypes.I64, endA)
	newLen := buildResult.NewSub(finalEnd, finalStart)
	newDataPtr := buildResult.NewGetElementPtr(irtypes.I8, dataPtr, finalStart)
	result := buildResult.NewCall(c.funcs["promise_string_new"], newDataPtr, newLen)
	buildResult.NewRet(result)

	c.funcs["promise_string_trim"] = fn
}

// defineStringSplitFunc emits an LLVM IR function that splits a string by a
// separator and returns a vector (slice) of string pointers.
// Phase 1: count separator occurrences using memcmp.
// Phase 2: allocate vector {i64 len, i64 cap, data...} and fill with substrings.
// Empty separator returns a single-element slice containing the whole string.
// String instance layout: { i8* _variant, i64 len, [0 x i8] data }
func (c *Compiler) defineStringSplitFunc() {
	sParam := ir.NewParam("s", irtypes.I8Ptr)
	sepParam := ir.NewParam("sep", irtypes.I8Ptr)
	fn := c.module.NewFunc("promise_string_split", irtypes.I8Ptr, sParam, sepParam)

	strInstanceType := irtypes.NewStruct(
		irtypes.I8Ptr,                   // _variant
		irtypes.I64,                     // len
		irtypes.NewArray(0, irtypes.I8), // data (flexible array)
	)

	zero64 := constant.NewInt(irtypes.I64, 0)
	one64 := constant.NewInt(irtypes.I64, 1)
	ptrSize := constant.NewInt(irtypes.I64, 8) // sizeof(pointer)
	headerSize := constant.NewInt(irtypes.I64, 16)
	vectorHeaderType := irtypes.NewStruct(irtypes.I64, irtypes.I64) // {len, cap}

	// OOM panic message
	oomMsg := constant.NewCharArrayFromString("out of memory\x00")
	oomGlobal := c.module.NewGlobalDef(
		fmt.Sprintf(".str.oom.%d", c.strCounter), oomMsg)
	c.strCounter++
	oomGlobal.Immutable = true

	// entry: load string fields, set up allocas
	entry := fn.NewBlock("entry")

	typedS := entry.NewBitCast(sParam, irtypes.NewPointer(strInstanceType))
	sLenPtr := entry.NewGetElementPtr(strInstanceType, typedS,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 1))
	sLen := entry.NewLoad(irtypes.I64, sLenPtr)
	sDataPtr := entry.NewGetElementPtr(strInstanceType, typedS,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 2),
		constant.NewInt(irtypes.I32, 0))

	typedSep := entry.NewBitCast(sepParam, irtypes.NewPointer(strInstanceType))
	sepLenPtr := entry.NewGetElementPtr(strInstanceType, typedSep,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 1))
	sepLen := entry.NewLoad(irtypes.I64, sepLenPtr)
	sepDataPtr := entry.NewGetElementPtr(strInstanceType, typedSep,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 2),
		constant.NewInt(irtypes.I32, 0))

	// All allocas in entry block
	countA := entry.NewAlloca(irtypes.I64)
	entry.NewStore(one64, countA) // count starts at 1
	iA := entry.NewAlloca(irtypes.I64)
	entry.NewStore(zero64, iA)
	posA := entry.NewAlloca(irtypes.I64)
	entry.NewStore(zero64, posA)
	idxA := entry.NewAlloca(irtypes.I64)
	entry.NewStore(zero64, idxA)

	sepEmpty := entry.NewICmp(enum.IPredEQ, sepLen, zero64)

	// Create all blocks
	countHdr := fn.NewBlock("count_hdr")
	countBody := fn.NewBlock("count_body")
	countMatch := fn.NewBlock("count_match")
	countNext := fn.NewBlock("count_next")
	allocBlk := fn.NewBlock("alloc")
	oomBlk := fn.NewBlock("oom")
	initHdr := fn.NewBlock("init_hdr")
	emptySep := fn.NewBlock("empty_sep")
	splitInit := fn.NewBlock("split_init")
	splitHdr := fn.NewBlock("split_hdr")
	splitBody := fn.NewBlock("split_body")
	splitMatch := fn.NewBlock("split_match")
	splitNext := fn.NewBlock("split_next")
	splitTail := fn.NewBlock("split_tail")
	doneBlk := fn.NewBlock("done")

	entry.NewCondBr(sepEmpty, allocBlk, countHdr)

	// ===== Phase 1: Count separators =====

	// count_hdr: i <= sLen - sepLen?
	iVal := countHdr.NewLoad(irtypes.I64, iA)
	limit := countHdr.NewSub(sLen, sepLen)
	countCond := countHdr.NewICmp(enum.IPredSLE, iVal, limit)
	countHdr.NewCondBr(countCond, countBody, allocBlk)

	// count_body: memcmp
	iVal2 := countBody.NewLoad(irtypes.I64, iA)
	curPtr := countBody.NewGetElementPtr(irtypes.I8, sDataPtr, iVal2)
	cmpResult := countBody.NewCall(c.funcs["memcmp"], curPtr, sepDataPtr, sepLen)
	isMatch := countBody.NewICmp(enum.IPredEQ, cmpResult, constant.NewInt(irtypes.I32, 0))
	countBody.NewCondBr(isMatch, countMatch, countNext)

	// count_match: count++, i += sepLen - 1
	cnt := countMatch.NewLoad(irtypes.I64, countA)
	cnt1 := countMatch.NewAdd(cnt, one64)
	countMatch.NewStore(cnt1, countA)
	iCur := countMatch.NewLoad(irtypes.I64, iA)
	skipI := countMatch.NewAdd(iCur, sepLen)
	skipIM1 := countMatch.NewSub(skipI, one64)
	countMatch.NewStore(skipIM1, iA)
	countMatch.NewBr(countNext)

	// count_next: i++
	iVal3 := countNext.NewLoad(irtypes.I64, iA)
	iNext := countNext.NewAdd(iVal3, one64)
	countNext.NewStore(iNext, iA)
	countNext.NewBr(countHdr)

	// ===== Phase 2: Allocate slice =====

	// alloc: malloc(16 + count * 8)
	count := allocBlk.NewLoad(irtypes.I64, countA)
	dataSize := allocBlk.NewMul(count, ptrSize)
	totalSize := allocBlk.NewAdd(headerSize, dataSize)
	rawSlice := allocBlk.NewCall(c.palAlloc, totalSize)
	isNull := allocBlk.NewICmp(enum.IPredEQ, rawSlice, constant.NewNull(irtypes.I8Ptr))
	allocBlk.NewCondBr(isNull, oomBlk, initHdr)

	// oom: panic
	msgPtr := oomBlk.NewGetElementPtr(oomGlobal.ContentType, oomGlobal,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
	oomBlk.NewCall(c.funcs["promise_panic"], msgPtr)
	oomBlk.NewUnreachable()

	// init_hdr: store len and cap
	hdrPtr := initHdr.NewBitCast(rawSlice, irtypes.NewPointer(vectorHeaderType))
	lenField := initHdr.NewGetElementPtr(vectorHeaderType, hdrPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
	initHdr.NewStore(count, lenField)
	capField := initHdr.NewGetElementPtr(vectorHeaderType, hdrPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 1))
	initHdr.NewStore(count, capField)
	sepEmpty2 := initHdr.NewICmp(enum.IPredEQ, sepLen, zero64)
	initHdr.NewCondBr(sepEmpty2, emptySep, splitInit)

	// ===== Phase 2a: Empty separator → single-element result =====

	elemBase := emptySep.NewGetElementPtr(irtypes.I8, rawSlice, headerSize)
	elemTyped := emptySep.NewBitCast(elemBase, irtypes.NewPointer(irtypes.I8Ptr))
	wholeStr := emptySep.NewCall(c.funcs["promise_string_new"], sDataPtr, sLen)
	emptySep.NewStore(wholeStr, elemTyped)
	emptySep.NewBr(doneBlk)

	// ===== Phase 2b: Split loop =====

	// split_init: reset loop vars
	splitInit.NewStore(zero64, iA)
	splitInit.NewStore(zero64, posA)
	splitInit.NewStore(zero64, idxA)
	splitInit.NewBr(splitHdr)

	// split_hdr: i <= sLen - sepLen?
	siVal := splitHdr.NewLoad(irtypes.I64, iA)
	sLimit := splitHdr.NewSub(sLen, sepLen)
	sCond := splitHdr.NewICmp(enum.IPredSLE, siVal, sLimit)
	splitHdr.NewCondBr(sCond, splitBody, splitTail)

	// split_body: memcmp
	siVal2 := splitBody.NewLoad(irtypes.I64, iA)
	sCurPtr := splitBody.NewGetElementPtr(irtypes.I8, sDataPtr, siVal2)
	sCmpResult := splitBody.NewCall(c.funcs["memcmp"], sCurPtr, sepDataPtr, sepLen)
	sIsMatch := splitBody.NewICmp(enum.IPredEQ, sCmpResult, constant.NewInt(irtypes.I32, 0))
	splitBody.NewCondBr(sIsMatch, splitMatch, splitNext)

	// split_match: create substring, store in result
	pos := splitMatch.NewLoad(irtypes.I64, posA)
	idx := splitMatch.NewLoad(irtypes.I64, idxA)
	matchI := splitMatch.NewLoad(irtypes.I64, iA)
	subLen := splitMatch.NewSub(matchI, pos)
	subPtr := splitMatch.NewGetElementPtr(irtypes.I8, sDataPtr, pos)
	newStr := splitMatch.NewCall(c.funcs["promise_string_new"], subPtr, subLen)
	// Store at rawSlice + 16 + idx * 8
	elemOff := splitMatch.NewMul(idx, ptrSize)
	elemOff2 := splitMatch.NewAdd(headerSize, elemOff)
	elemPtr := splitMatch.NewGetElementPtr(irtypes.I8, rawSlice, elemOff2)
	elemPtrTyped := splitMatch.NewBitCast(elemPtr, irtypes.NewPointer(irtypes.I8Ptr))
	splitMatch.NewStore(newStr, elemPtrTyped)
	// Update pos = i + sepLen, idx++
	newPos := splitMatch.NewAdd(matchI, sepLen)
	splitMatch.NewStore(newPos, posA)
	nextIdx := splitMatch.NewAdd(idx, one64)
	splitMatch.NewStore(nextIdx, idxA)
	// i += sepLen - 1 (split_next adds 1 more)
	skipSI := splitMatch.NewAdd(matchI, sepLen)
	skipSIM1 := splitMatch.NewSub(skipSI, one64)
	splitMatch.NewStore(skipSIM1, iA)
	splitMatch.NewBr(splitNext)

	// split_next: i++
	siVal3 := splitNext.NewLoad(irtypes.I64, iA)
	siNext := splitNext.NewAdd(siVal3, one64)
	splitNext.NewStore(siNext, iA)
	splitNext.NewBr(splitHdr)

	// split_tail: store final substring from pos to sLen
	tailPos := splitTail.NewLoad(irtypes.I64, posA)
	tailIdx := splitTail.NewLoad(irtypes.I64, idxA)
	tailLen := splitTail.NewSub(sLen, tailPos)
	tailPtr := splitTail.NewGetElementPtr(irtypes.I8, sDataPtr, tailPos)
	tailStr := splitTail.NewCall(c.funcs["promise_string_new"], tailPtr, tailLen)
	tailElemOff := splitTail.NewMul(tailIdx, ptrSize)
	tailElemOff2 := splitTail.NewAdd(headerSize, tailElemOff)
	tailElemPtr := splitTail.NewGetElementPtr(irtypes.I8, rawSlice, tailElemOff2)
	tailElemPtrTyped := splitTail.NewBitCast(tailElemPtr, irtypes.NewPointer(irtypes.I8Ptr))
	splitTail.NewStore(tailStr, tailElemPtrTyped)
	splitTail.NewBr(doneBlk)

	// done: return slice
	doneBlk.NewRet(rawSlice)

	c.funcs["promise_string_split"] = fn
}

// defineStringNextCharFunc emits an LLVM IR function that decodes one UTF-8
// codepoint from a string at position *pos, advances *pos past the consumed
// bytes, and returns the codepoint as i32. Returns -1 when *pos >= len (EOF).
// String instance layout: { i8* _variant, i64 len, [0 x i8] data }
func (c *Compiler) defineStringNextCharFunc() {
	sParam := ir.NewParam("s", irtypes.I8Ptr)
	posParam := ir.NewParam("pos", irtypes.NewPointer(irtypes.I64))
	fn := c.module.NewFunc("promise_string_next_char", irtypes.I32, sParam, posParam)

	strInstanceType := irtypes.NewStruct(
		irtypes.I8Ptr,                   // _variant
		irtypes.I64,                     // len
		irtypes.NewArray(0, irtypes.I8), // data (flexible array)
	)

	one32 := constant.NewInt(irtypes.I32, 1)

	// entry: load len, data pointer, *pos, allocas for cp/n/loopI
	entry := fn.NewBlock("entry")

	typedS := entry.NewBitCast(sParam, irtypes.NewPointer(strInstanceType))
	sLenPtr := entry.NewGetElementPtr(strInstanceType, typedS,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 1))
	sLen := entry.NewLoad(irtypes.I64, sLenPtr)
	sDataPtr := entry.NewGetElementPtr(strInstanceType, typedS,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 2),
		constant.NewInt(irtypes.I32, 0))

	posVal := entry.NewLoad(irtypes.I64, posParam)

	// Allocas in entry block
	cpA := entry.NewAlloca(irtypes.I32)
	nA := entry.NewAlloca(irtypes.I32)
	loopIA := entry.NewAlloca(irtypes.I32)

	atEnd := entry.NewICmp(enum.IPredSGE, posVal, sLen)

	retEof := fn.NewBlock("ret_eof")
	decode := fn.NewBlock("decode")
	set1 := fn.NewBlock("set_1byte")
	chk2 := fn.NewBlock("chk_2byte")
	set2 := fn.NewBlock("set_2byte")
	chk3 := fn.NewBlock("chk_3byte")
	set3 := fn.NewBlock("set_3byte")
	set4 := fn.NewBlock("set_4byte")
	contLoop := fn.NewBlock("cont_loop")
	contHdr := fn.NewBlock("cont_hdr")
	contBound := fn.NewBlock("cont_bound")
	contBody := fn.NewBlock("cont_body")
	contDone := fn.NewBlock("cont_done")

	entry.NewCondBr(atEnd, retEof, decode)

	// ret_eof: return -1
	retEof.NewRet(constant.NewInt(irtypes.I32, -1))

	// decode: load first byte, classify
	bytePtr := decode.NewGetElementPtr(irtypes.I8, sDataPtr, posVal)
	b0 := decode.NewLoad(irtypes.I8, bytePtr)
	b0ext := decode.NewZExt(b0, irtypes.I32)
	isAscii := decode.NewICmp(enum.IPredULT, b0ext, constant.NewInt(irtypes.I32, 0x80))
	decode.NewCondBr(isAscii, set1, chk2)

	// set_1byte: cp = b0, n = 1
	set1.NewStore(b0ext, cpA)
	set1.NewStore(one32, nA)
	set1.NewBr(contLoop)

	// chk_2byte: b0 < 0xE0?
	is2byte := chk2.NewICmp(enum.IPredULT, b0ext, constant.NewInt(irtypes.I32, 0xE0))
	chk2.NewCondBr(is2byte, set2, chk3)

	// set_2byte: cp = b0 & 0x1F, n = 2
	masked2 := set2.NewAnd(b0ext, constant.NewInt(irtypes.I32, 0x1F))
	set2.NewStore(masked2, cpA)
	set2.NewStore(constant.NewInt(irtypes.I32, 2), nA)
	set2.NewBr(contLoop)

	// chk_3byte: b0 < 0xF0?
	is3byte := chk3.NewICmp(enum.IPredULT, b0ext, constant.NewInt(irtypes.I32, 0xF0))
	chk3.NewCondBr(is3byte, set3, set4)

	// set_3byte: cp = b0 & 0x0F, n = 3
	masked3 := set3.NewAnd(b0ext, constant.NewInt(irtypes.I32, 0x0F))
	set3.NewStore(masked3, cpA)
	set3.NewStore(constant.NewInt(irtypes.I32, 3), nA)
	set3.NewBr(contLoop)

	// set_4byte: cp = b0 & 0x07, n = 4
	masked4 := set4.NewAnd(b0ext, constant.NewInt(irtypes.I32, 0x07))
	set4.NewStore(masked4, cpA)
	set4.NewStore(constant.NewInt(irtypes.I32, 4), nA)
	set4.NewBr(contLoop)

	// cont_loop: initialize loop index i = 1
	contLoop.NewStore(one32, loopIA)
	contLoop.NewBr(contHdr)

	// cont_hdr: i < n?
	iVal := contHdr.NewLoad(irtypes.I32, loopIA)
	nVal := contHdr.NewLoad(irtypes.I32, nA)
	cond1 := contHdr.NewICmp(enum.IPredSLT, iVal, nVal)
	contHdr.NewCondBr(cond1, contBound, contDone)

	// cont_bound: *pos + i < sLen?
	iVal2 := contBound.NewLoad(irtypes.I32, loopIA)
	iExt := contBound.NewSExt(iVal2, irtypes.I64)
	absPos := contBound.NewAdd(posVal, iExt)
	cond2 := contBound.NewICmp(enum.IPredSLT, absPos, sLen)
	contBound.NewCondBr(cond2, contBody, contDone)

	// cont_body: cp = (cp << 6) | (data[absPos] & 0x3F); i++
	absPosBody := contBody.NewLoad(irtypes.I32, loopIA)
	absPosExt := contBody.NewSExt(absPosBody, irtypes.I64)
	absPosCalc := contBody.NewAdd(posVal, absPosExt)
	contBytePtr := contBody.NewGetElementPtr(irtypes.I8, sDataPtr, absPosCalc)
	contByte := contBody.NewLoad(irtypes.I8, contBytePtr)
	contByteExt := contBody.NewZExt(contByte, irtypes.I32)
	masked := contBody.NewAnd(contByteExt, constant.NewInt(irtypes.I32, 0x3F))
	cp := contBody.NewLoad(irtypes.I32, cpA)
	shifted := contBody.NewShl(cp, constant.NewInt(irtypes.I32, 6))
	newCp := contBody.NewOr(shifted, masked)
	contBody.NewStore(newCp, cpA)
	iCur := contBody.NewLoad(irtypes.I32, loopIA)
	iNext := contBody.NewAdd(iCur, one32)
	contBody.NewStore(iNext, loopIA)
	contBody.NewBr(contHdr)

	// cont_done: *pos += n, return cp
	nFinal := contDone.NewLoad(irtypes.I32, nA)
	nExt := contDone.NewSExt(nFinal, irtypes.I64)
	newPos := contDone.NewAdd(posVal, nExt)
	contDone.NewStore(newPos, posParam)
	cpFinal := contDone.NewLoad(irtypes.I32, cpA)
	contDone.NewRet(cpFinal)

	c.funcs["promise_string_next_char"] = fn
}

// defineStringHashFunc emits an LLVM IR function that computes FNV-1a hash
// over the raw bytes of a string. Replaces the C runtime promise_hash_string_value.
// String instance layout: { i8* _variant, i64 len, [0 x i8] data }
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

	// Get data pointers and compare via memcmp
	dataPtrA := cmpDataBlk.NewGetElementPtr(strInstanceType, typedA,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 2),
		constant.NewInt(irtypes.I32, 0))
	dataPtrB := cmpDataBlk.NewGetElementPtr(strInstanceType, typedB,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 2),
		constant.NewInt(irtypes.I32, 0))

	equalBlk := fn.NewBlock("equal")
	neqBlk := fn.NewBlock("not_equal")

	cmpResult := cmpDataBlk.NewCall(c.funcs["memcmp"], dataPtrA, dataPtrB, lenA)
	isEqual := cmpDataBlk.NewICmp(enum.IPredEQ, cmpResult, constant.NewInt(irtypes.I32, 0))
	cmpDataBlk.NewCondBr(isEqual, equalBlk, neqBlk)

	// Bytes differ → return 0
	neqBlk.NewRet(zero32)

	// All bytes match → return 1
	equalBlk.NewRet(one32)

	c.funcs["__promise_eq_string"] = fn
}

// defineVectorWithCapacityFunc emits an LLVM IR function that allocates a vector
// with len=0 and the given capacity. Replaces the C runtime promise_vector_with_capacity.
// Vector layout: {i64 len, i64 cap, [data...]} with 16-byte header.
func (c *Compiler) defineVectorWithCapacityFunc() {
	capParam := ir.NewParam("capacity", irtypes.I64)
	elemSizeParam := ir.NewParam("elem_size", irtypes.I64)
	fn := c.module.NewFunc("promise_vector_with_capacity", irtypes.I8Ptr,
		capParam, elemSizeParam)

	headerType := vectorHeaderType() // {i64, i64}
	zero64 := constant.NewInt(irtypes.I64, 0)
	headerSizeConst := constant.NewInt(irtypes.I64, int64(vectorHeaderSize))

	// Entry: clamp negative capacity to 0, compute alloc size
	entry := fn.NewBlock("entry")
	isNeg := entry.NewICmp(enum.IPredSLT, capParam, zero64)
	clampedCap := entry.NewSelect(isNeg, zero64, capParam)
	dataSize := entry.NewMul(clampedCap, elemSizeParam)
	allocSize := entry.NewAdd(headerSizeConst, dataSize)
	raw := entry.NewCall(c.palAlloc, allocSize)
	isNull := entry.NewICmp(enum.IPredEQ, raw, constant.NewNull(irtypes.I8Ptr))

	oom := fn.NewBlock("oom")
	init := fn.NewBlock("init")
	entry.NewCondBr(isNull, oom, init)

	// OOM: panic
	panicMsg := constant.NewCharArrayFromString("out of memory\x00")
	globalName := fmt.Sprintf(".str.vecwithcap.%d", c.strCounter)
	c.strCounter++
	panicGlobal := c.module.NewGlobalDef(globalName, panicMsg)
	panicGlobal.Immutable = true
	msgPtr := oom.NewGetElementPtr(panicGlobal.ContentType, panicGlobal,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
	oom.NewCall(c.funcs["promise_panic"], msgPtr)
	oom.NewUnreachable()

	// Init: store len=0, cap
	hdrPtr := init.NewBitCast(raw, irtypes.NewPointer(headerType))
	lenPtr := init.NewGetElementPtr(headerType, hdrPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
	init.NewStore(zero64, lenPtr)
	capPtr := init.NewGetElementPtr(headerType, hdrPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 1))
	init.NewStore(clampedCap, capPtr)
	init.NewRet(raw)

	c.funcs["promise_vector_with_capacity"] = fn
}

// defineVectorPushFunc emits an LLVM IR function that appends an element to a vector.
// Returns the (possibly reallocated) vector pointer.
// Replaces the C runtime promise_vector_push.
// Vector layout: {i64 len, i64 cap, [data...]} with 16-byte header.
func (c *Compiler) defineVectorPushFunc() {
	sliceParam := ir.NewParam("slice", irtypes.I8Ptr)
	elemParam := ir.NewParam("elem", irtypes.I8Ptr)
	elemSizeParam := ir.NewParam("elem_size", irtypes.I64)
	fn := c.module.NewFunc("promise_vector_push", irtypes.I8Ptr,
		sliceParam, elemParam, elemSizeParam)

	headerType := vectorHeaderType() // {i64, i64}
	zero64 := constant.NewInt(irtypes.I64, 0)
	one64 := constant.NewInt(irtypes.I64, 1)
	four64 := constant.NewInt(irtypes.I64, 4)
	headerSizeConst := constant.NewInt(irtypes.I64, int64(vectorHeaderSize))

	// Entry: load len and cap, check if growth needed
	entry := fn.NewBlock("entry")
	hdrPtr := entry.NewBitCast(sliceParam, irtypes.NewPointer(headerType))
	lenPtr := entry.NewGetElementPtr(headerType, hdrPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
	vecLen := entry.NewLoad(irtypes.I64, lenPtr)
	capPtr := entry.NewGetElementPtr(headerType, hdrPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 1))
	vecCap := entry.NewLoad(irtypes.I64, capPtr)
	needGrow := entry.NewICmp(enum.IPredSGE, vecLen, vecCap)

	grow := fn.NewBlock("grow")
	copyBlk := fn.NewBlock("copy")
	entry.NewCondBr(needGrow, grow, copyBlk)

	// Grow: realloc with cap*2 (or 4 if cap==0)
	isZeroCap := grow.NewICmp(enum.IPredEQ, vecCap, zero64)
	doubledCap := grow.NewMul(vecCap, constant.NewInt(irtypes.I64, 2))
	newCap := grow.NewSelect(isZeroCap, four64, doubledCap)
	newDataSize := grow.NewMul(newCap, elemSizeParam)
	newAllocSize := grow.NewAdd(headerSizeConst, newDataSize)
	newPtr := grow.NewCall(c.palRealloc, sliceParam, newAllocSize)
	isNull := grow.NewICmp(enum.IPredEQ, newPtr, constant.NewNull(irtypes.I8Ptr))

	oom := fn.NewBlock("oom")
	updateCap := fn.NewBlock("update_cap")
	grow.NewCondBr(isNull, oom, updateCap)

	// OOM: panic
	panicMsg := constant.NewCharArrayFromString("out of memory\x00")
	globalName := fmt.Sprintf(".str.vecpush.%d", c.strCounter)
	c.strCounter++
	panicGlobal := c.module.NewGlobalDef(globalName, panicMsg)
	panicGlobal.Immutable = true
	msgPtr := oom.NewGetElementPtr(panicGlobal.ContentType, panicGlobal,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
	oom.NewCall(c.funcs["promise_panic"], msgPtr)
	oom.NewUnreachable()

	// Update cap: store new capacity in reallocated header
	newHdrPtr := updateCap.NewBitCast(newPtr, irtypes.NewPointer(headerType))
	newCapPtr := updateCap.NewGetElementPtr(headerType, newHdrPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 1))
	updateCap.NewStore(newCap, newCapPtr)
	updateCap.NewBr(copyBlk)

	// Copy: phi to merge original/reallocated pointers
	vecPtr := copyBlk.NewPhi(
		&ir.Incoming{X: sliceParam, Pred: entry},
		&ir.Incoming{X: newPtr, Pred: updateCap})
	curHdrPtr := copyBlk.NewPhi(
		&ir.Incoming{X: hdrPtr, Pred: entry},
		&ir.Incoming{X: newHdrPtr, Pred: updateCap})

	// Compute destination: data area + len * elem_size
	offset := copyBlk.NewMul(vecLen, elemSizeParam)
	dataOffset := copyBlk.NewAdd(headerSizeConst, offset)
	dest := copyBlk.NewGetElementPtr(irtypes.I8, vecPtr, dataOffset)
	copyBlk.NewCall(c.funcs["llvm.memcpy"], dest, elemParam, elemSizeParam, constant.False)

	// Increment length
	newLen := copyBlk.NewAdd(vecLen, one64)
	curLenPtr := copyBlk.NewGetElementPtr(headerType, curHdrPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
	copyBlk.NewStore(newLen, curLenPtr)
	copyBlk.NewRet(vecPtr)

	c.funcs["promise_vector_push"] = fn
}

// defineVectorPopFunc emits an LLVM IR function that removes and returns the last
// element from a vector. Returns 1 if successful, 0 if empty.
// Replaces the C runtime promise_vector_pop.
// Vector layout: {i64 len, i64 cap, [data...]} with 16-byte header.
func (c *Compiler) defineVectorPopFunc() {
	sliceParam := ir.NewParam("slice", irtypes.I8Ptr)
	outElemParam := ir.NewParam("out_elem", irtypes.I8Ptr)
	elemSizeParam := ir.NewParam("elem_size", irtypes.I64)
	fn := c.module.NewFunc("promise_vector_pop", irtypes.I32,
		sliceParam, outElemParam, elemSizeParam)

	headerType := vectorHeaderType() // {i64, i64}
	zero64 := constant.NewInt(irtypes.I64, 0)
	one64 := constant.NewInt(irtypes.I64, 1)
	headerSizeConst := constant.NewInt(irtypes.I64, int64(vectorHeaderSize))

	// Entry: load len, check if empty
	entry := fn.NewBlock("entry")
	hdrPtr := entry.NewBitCast(sliceParam, irtypes.NewPointer(headerType))
	lenPtr := entry.NewGetElementPtr(headerType, hdrPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
	vecLen := entry.NewLoad(irtypes.I64, lenPtr)
	isEmpty := entry.NewICmp(enum.IPredEQ, vecLen, zero64)

	emptyBlk := fn.NewBlock("empty")
	doPopBlk := fn.NewBlock("do_pop")
	entry.NewCondBr(isEmpty, emptyBlk, doPopBlk)

	// Empty: return 0
	emptyBlk.NewRet(constant.NewInt(irtypes.I32, 0))

	// Do pop: decrement len, copy last element out
	newLen := doPopBlk.NewSub(vecLen, one64)
	doPopBlk.NewStore(newLen, lenPtr)
	offset := doPopBlk.NewMul(newLen, elemSizeParam)
	dataOffset := doPopBlk.NewAdd(headerSizeConst, offset)
	src := doPopBlk.NewGetElementPtr(irtypes.I8, sliceParam, dataOffset)
	doPopBlk.NewCall(c.funcs["llvm.memcpy"], outElemParam, src, elemSizeParam, constant.False)
	doPopBlk.NewRet(constant.NewInt(irtypes.I32, 1))

	c.funcs["promise_vector_pop"] = fn
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

	// cmp_bytes: compare via memcmp
	cmpResult := cmpBytes.NewCall(c.funcs["memcmp"], curPtr, elemParam, elemSizeParam)
	isEqual := cmpBytes.NewICmp(enum.IPredEQ, cmpResult, constant.NewInt(irtypes.I32, 0))
	cmpBytes.NewCondBr(isEqual, found, loopNext)

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
	doShift.NewCall(c.funcs["llvm.memmove"], dst, src, moveSize, constant.False)
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
		// Skip user main — its body is compiled inline inside .goroutine.main
		// by wrapMainWithScheduler (with inCoroutine=true for proper channel ops).
		if fd.Name == "main" && !fd.IsStd {
			c.mainDecl = fd
			continue
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
	c.localNameCount = make(map[string]int)
	c.dropFlags = make(map[string]*ir.InstAlloca)
	c.dropBindings = make(map[string]scopeBinding)
	c.blockCounter = 0

	entry := fn.NewBlock("entry")
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

	for i, p := range sig.Params() {
		if p.Name() == "" || p.Name() == "_" {
			continue
		}
		alloca := entry.NewAlloca(c.resolveType(p.Type()))
		alloca.SetName(c.uniqueLocalName(p.Name() + ".addr"))
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

// createEntryAlloca creates an alloca in the function's entry block.
// This ensures the alloca dominates all uses, which is required by LLVM's
// verifier across all versions. The entry block dominates every block in
// the function, so allocas placed here are always valid.
func (c *Compiler) createEntryAlloca(elemType irtypes.Type) *ir.InstAlloca {
	return c.entryBlock.NewAlloca(elemType)
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
	c.localNameCount = make(map[string]int)
	c.dropFlags = make(map[string]*ir.InstAlloca)
	c.dropBindings = make(map[string]scopeBinding)
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
	c.entryBlock = entry

	paramIdx := 0

	// Allocate receiver as "this"
	if m.Sig().Recv() != nil {
		alloca := entry.NewAlloca(irtypes.I8Ptr)
		alloca.SetName(c.uniqueLocalName("this.addr"))
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
		alloca.SetName(c.uniqueLocalName(p.Name() + ".addr"))
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
	entryBlock     *ir.Block
	locals         map[string]*ir.InstAlloca
	dropFlags      map[string]*ir.InstAlloca
	dropBindings   map[string]scopeBinding
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
		entryBlock:     c.entryBlock,
		locals:         c.locals,
		dropFlags:      c.dropFlags,
		dropBindings:   c.dropBindings,
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
	c.entryBlock = s.entryBlock
	c.locals = s.locals
	c.dropFlags = s.dropFlags
	c.dropBindings = s.dropBindings
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

// --- Channel infrastructure ---

// Channel struct field indices.
const (
	chanFieldBuffer     = 0  // i8*  ring buffer
	chanFieldElemSize   = 1  // i64  element size
	chanFieldCapacity   = 2  // i64  ring buffer capacity (always >= 1)
	chanFieldCount      = 3  // i64  current element count
	chanFieldHead       = 4  // i64  read index
	chanFieldTail       = 5  // i64  write index
	chanFieldClosed     = 6  // i8   0=open, 1=closed
	chanFieldUnbuffered = 7  // i8   1 if user requested capacity=0
	chanFieldMutex      = 8  // i8*  PAL mutex handle
	chanFieldNotEmpty   = 9  // i8*  cond var: signaled when items added or closed
	chanFieldNotFull    = 10 // i8*  cond var: signaled when items removed

	// Goroutine waiter lists (Phase 5c: M:N scheduler)
	chanFieldSendWaitersHead = 11 // i8*  head of parked sender Gs
	chanFieldSendWaitersTail = 12 // i8*  tail of parked sender Gs
	chanFieldRecvWaitersHead = 13 // i8*  head of parked receiver Gs
	chanFieldRecvWaitersTail = 14 // i8*  tail of parked receiver Gs
)

// channelStructType returns the LLVM struct type for a channel.
// Layout: { i8*, i64, i64, i64, i64, i64, i8, i8, i8*, i8*, i8*, i8*, i8*, i8*, i8* } — 15 fields
func channelStructType() *irtypes.StructType {
	return irtypes.NewStruct(
		irtypes.I8Ptr, // buffer
		irtypes.I64,   // elem_size
		irtypes.I64,   // capacity
		irtypes.I64,   // count
		irtypes.I64,   // head
		irtypes.I64,   // tail
		irtypes.I8,    // closed
		irtypes.I8,    // unbuffered
		irtypes.I8Ptr, // mutex
		irtypes.I8Ptr, // not_empty cond
		irtypes.I8Ptr, // not_full cond
		irtypes.I8Ptr, // send_waiters_head
		irtypes.I8Ptr, // send_waiters_tail
		irtypes.I8Ptr, // recv_waiters_head
		irtypes.I8Ptr, // recv_waiters_tail
	)
}

// defineChannelNewFunc emits @promise_channel_new(i64 %capacity, i64 %elem_size) → i8*
// Allocates and initializes a channel struct with ring buffer, mutex, and 2 cond vars.
func (c *Compiler) defineChannelNewFunc() {
	capParam := ir.NewParam("capacity", irtypes.I64)
	elemSzParam := ir.NewParam("elem_size", irtypes.I64)
	fn := c.module.NewFunc("promise_channel_new", irtypes.I8Ptr, capParam, elemSzParam)

	chanType := channelStructType()

	entry := fn.NewBlock("entry")

	// Allocate channel struct
	structSize := constant.NewInt(irtypes.I64, int64(llvmTypeSize(chanType)))
	rawPtr := entry.NewCall(c.palAlloc, structSize)
	chPtr := entry.NewBitCast(rawPtr, irtypes.NewPointer(chanType))

	// actual_cap = max(capacity, 1) — even unbuffered channels need 1-slot buffer
	isZero := entry.NewICmp(enum.IPredEQ, capParam, constant.NewInt(irtypes.I64, 0))
	actualCap := entry.NewSelect(isZero, constant.NewInt(irtypes.I64, 1), capParam)

	// Allocate ring buffer: actual_cap * elem_size
	bufSize := entry.NewMul(actualCap, elemSzParam)
	bufPtr := entry.NewCall(c.palAlloc, bufSize)

	// Store buffer
	bufField := entry.NewGetElementPtr(chanType, chPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(chanFieldBuffer)))
	entry.NewStore(bufPtr, bufField)

	// Store elem_size
	esField := entry.NewGetElementPtr(chanType, chPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(chanFieldElemSize)))
	entry.NewStore(elemSzParam, esField)

	// Store capacity = actual_cap
	capField := entry.NewGetElementPtr(chanType, chPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(chanFieldCapacity)))
	entry.NewStore(actualCap, capField)

	// Store count = 0
	countField := entry.NewGetElementPtr(chanType, chPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(chanFieldCount)))
	entry.NewStore(constant.NewInt(irtypes.I64, 0), countField)

	// Store head = 0
	headField := entry.NewGetElementPtr(chanType, chPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(chanFieldHead)))
	entry.NewStore(constant.NewInt(irtypes.I64, 0), headField)

	// Store tail = 0
	tailField := entry.NewGetElementPtr(chanType, chPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(chanFieldTail)))
	entry.NewStore(constant.NewInt(irtypes.I64, 0), tailField)

	// Store closed = 0
	closedField := entry.NewGetElementPtr(chanType, chPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(chanFieldClosed)))
	entry.NewStore(constant.NewInt(irtypes.I8, 0), closedField)

	// Store unbuffered = (capacity == 0) ? 1 : 0
	unbufVal := entry.NewSelect(isZero, constant.NewInt(irtypes.I8, 1), constant.NewInt(irtypes.I8, 0))
	unbufField := entry.NewGetElementPtr(chanType, chPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(chanFieldUnbuffered)))
	entry.NewStore(unbufVal, unbufField)

	// Init mutex
	mtx := entry.NewCall(c.palMutexInit)
	mtxField := entry.NewGetElementPtr(chanType, chPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(chanFieldMutex)))
	entry.NewStore(mtx, mtxField)

	// Init not_empty cond var
	notEmpty := entry.NewCall(c.palCondInit)
	neField := entry.NewGetElementPtr(chanType, chPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(chanFieldNotEmpty)))
	entry.NewStore(notEmpty, neField)

	// Init not_full cond var
	notFull := entry.NewCall(c.palCondInit)
	nfField := entry.NewGetElementPtr(chanType, chPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(chanFieldNotFull)))
	entry.NewStore(notFull, nfField)

	// Init goroutine waiter lists to null
	nullPtr := constant.NewNull(irtypes.I8Ptr)
	for _, idx := range []int{chanFieldSendWaitersHead, chanFieldSendWaitersTail,
		chanFieldRecvWaitersHead, chanFieldRecvWaitersTail} {
		field := entry.NewGetElementPtr(chanType, chPtr,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(idx)))
		entry.NewStore(nullPtr, field)
	}

	entry.NewRet(rawPtr)

	c.funcs["promise_channel_new"] = fn
}

// --- LLVM Coroutine Intrinsics (Phase 5c) ---

// declareCoroIntrinsics declares all LLVM coroutine intrinsics needed for the M:N scheduler.
func (c *Compiler) declareCoroIntrinsics() {
	// @llvm.coro.id(i32 align, i8* promise, i8* coroaddr, i8* fnaddrs) → token
	c.coroId = c.module.NewFunc("llvm.coro.id", irtypes.Token,
		ir.NewParam("align", irtypes.I32),
		ir.NewParam("promise", irtypes.I8Ptr),
		ir.NewParam("coroaddr", irtypes.I8Ptr),
		ir.NewParam("fnaddrs", irtypes.I8Ptr))

	// @llvm.coro.alloc(token %id) → i1
	c.coroAlloc = c.module.NewFunc("llvm.coro.alloc", irtypes.I1,
		ir.NewParam("id", irtypes.Token))

	// @llvm.coro.begin(token %id, i8* %mem) → i8*
	c.coroBegin = c.module.NewFunc("llvm.coro.begin", irtypes.I8Ptr,
		ir.NewParam("id", irtypes.Token),
		ir.NewParam("mem", irtypes.I8Ptr))

	// @llvm.coro.size.i64() → i64
	c.coroSize = c.module.NewFunc("llvm.coro.size.i64", irtypes.I64)

	// @llvm.coro.suspend(token %save, i1 %final) → i8
	c.coroSuspend = c.module.NewFunc("llvm.coro.suspend", irtypes.I8,
		ir.NewParam("save", irtypes.Token),
		ir.NewParam("final", irtypes.I1))

	// @llvm.coro.end(i8* %handle, i1 %unwind, token %bundle) → i1
	c.coroEnd = c.module.NewFunc("llvm.coro.end", irtypes.I1,
		ir.NewParam("handle", irtypes.I8Ptr),
		ir.NewParam("unwind", irtypes.I1),
		ir.NewParam("bundle", irtypes.Token))

	// @llvm.coro.free(token %id, i8* %handle) → i8*
	c.coroFree = c.module.NewFunc("llvm.coro.free", irtypes.I8Ptr,
		ir.NewParam("id", irtypes.Token),
		ir.NewParam("handle", irtypes.I8Ptr))

	// @llvm.coro.resume(i8* %handle) → void
	c.coroResume = c.module.NewFunc("llvm.coro.resume", irtypes.Void,
		ir.NewParam("handle", irtypes.I8Ptr))

	// @llvm.coro.destroy(i8* %handle) → void
	c.coroDestroy = c.module.NewFunc("llvm.coro.destroy", irtypes.Void,
		ir.NewParam("handle", irtypes.I8Ptr))

	// @llvm.coro.done(i8* %handle) → i1
	c.coroDone = c.module.NewFunc("llvm.coro.done", irtypes.I1,
		ir.NewParam("handle", irtypes.I8Ptr))
}
