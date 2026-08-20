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

	"github.com/promise-language/promise/compiler/internal/ast"
	"github.com/promise-language/promise/compiler/internal/sema"
	"github.com/promise-language/promise/compiler/internal/types"
)

// formatDurationNs formats a nanosecond duration as a human-readable string
// matching Duration.to_string() semantics: "Xns", "Xus"/"X.Yus", "Xms"/"X.Yms", "Xs"/"X.Ys".
func formatDurationNs(ns int64) string {
	if ns < 0 {
		return "-" + formatDurationNs(-ns)
	}
	if ns < 1_000 {
		return fmt.Sprintf("%dns", ns)
	}
	if ns < 1_000_000 {
		us := ns / 1_000
		frac := (ns % 1_000) / 100
		if frac == 0 {
			return fmt.Sprintf("%dus", us)
		}
		return fmt.Sprintf("%d.%dus", us, frac)
	}
	if ns < 1_000_000_000 {
		ms := ns / 1_000_000
		frac := (ns % 1_000_000) / 100_000
		if frac == 0 {
			return fmt.Sprintf("%dms", ms)
		}
		return fmt.Sprintf("%d.%dms", ms, frac)
	}
	s := ns / 1_000_000_000
	frac := (ns % 1_000_000_000) / 100_000_000
	if frac == 0 {
		return fmt.Sprintf("%ds", s)
	}
	return fmt.Sprintf("%d.%ds", s, frac)
}

// rawFuncAttr is a function attribute emitted as a bare keyword (no quotes).
// This is needed for attributes like "presplitcoroutine" which LLVM only
// recognizes as enum-style keywords, not as quoted string attributes.
type rawFuncAttr string

func (a rawFuncAttr) IsFuncAttribute() {}

func (a rawFuncAttr) String() string { return string(a) }

// Compiler generates LLVM IR from a type-checked Promise AST.
type Compiler struct {
	module         *ir.Module
	info           *sema.Info
	rootInfo       *sema.Info                       // original user sema info — never swapped, used for full module tree search
	fn             *ir.Func                         // current function being generated
	block          *ir.Block                        // current basic block
	entryBlock     *ir.Block                        // entry block of current function (for allocas)
	locals         map[string]*ir.InstAlloca        // local variable allocas
	localNameCount map[string]int                   // per-function alloca name counter for dedup
	funcs          map[string]*ir.Func              // declared Promise functions by name
	layouts        map[*types.Named]*TypeDeclLayout // type layouts for extern ABI
	enumLayouts    map[*types.Enum]*TypeDeclLayout  // enum type layouts
	externs        map[string]*ExternFunc           // extern functions by Promise name

	// Monomorphization state
	monoLayouts         map[string]*TypeDeclLayout      // mono name → layout (user types)
	monoEnumLayouts     map[string]*TypeDeclLayout      // mono name → layout (enums)
	monoVtableGlobals   map[string]*ir.Global           // mono name → vtable global
	monoTypeInfoGlobals map[string]*ir.Global           // mono name → typeinfo global
	monoTypeIDs         map[string]int32                // mono name → type ID (for generic is-checks)
	monoValueTypeRTTI   map[string]*ir.Global           // mono name → value type RTTI instance
	typeSubst           map[*types.TypeParam]types.Type // nil outside mono codegen
	monoCtx             *monoContext                    // nil outside mono method codegen

	// Self-substitution for default method synthesis on structural interfaces.
	// When non-nil, replaces occurrences of selfSubst.iface with selfSubst.concrete
	// in sema type lookups during codegen.
	selfSubst *selfSubstInfo

	// AST file reference for looking up default method bodies during synthesis.
	file *ast.File

	// Module codegen state
	moduleFuncs      map[string]*ir.Func         // "irprefix.funcname" → IR func (cross-module calls)
	moduleExterns    map[string]*ExternFunc      // "irprefix.funcname" → extern (cross-module externs)
	compilingModule  string                      // non-empty when compiling a module's declarations (IR prefix)
	moduleOwnedFuncs map[string]string           // IR func name → module IR prefix (for separate compilation)
	moduleCanonical  map[string]string           // module path or import name → IR prefix (for alias→prefix mapping)
	moduleInfos      map[string]*sema.ModuleInfo // B0212: cached from main info for cross-module lookups

	// T1395: parameter default expression → the sema Info that type-checked it.
	// A default is spliced into the call site's argument list, so it can be
	// emitted while a DIFFERENT compilation unit's Info is active (e.g. std's
	// `scan[T]` filling a user type's trailing default). Only the declaring Info
	// holds the expression's recorded types/objects.
	paramDefaultInfos map[ast.Expr]*sema.Info

	// Instance BC codegen state
	instanceOwnedFuncs map[string]string // IR func name → mono instance name (e.g., "Box[int]")
	cachedInstances    map[string]bool   // mono instance names whose .bc is already cached
	spiralInstances    map[string]bool   // mono instance names generated by spiral expansion guard (no-expand)

	// Coverage instrumentation state (T0030)
	coverageEnabled bool             // true when -coverage flag is set
	coverageRegions []CoverageRegion // metadata per instrumented region
	coverageGlobals []*ir.Global     // i64 global counter per region

	// Loop control targets for break/continue
	breakTarget    *ir.Block
	continueTarget *ir.Block

	// Error handling: true if current function is failable (returns result struct)
	canError bool

	// Return type of the current function/method (Promise-level, for coercion)
	currentRetType types.Type

	// Current type being compiled (set during method body generation)
	currentNamed *types.Named

	// T0604: Current enum being compiled for drop — set when compiling an enum's
	// explicit drop body so variant field drops can be emitted after the user code.
	currentDropEnum *types.Enum

	// String literal counter for unique global names (main code)
	strCounter int

	// Per-module string literal counter: reset to 0 for each module compilation so
	// module string constant names are stable (independent of main code's strCounter).
	moduleStrCounter int

	// Array literal counter for unique global names (.rodata static vectors)
	arrCounter int

	// Per-module array literal counter for stable names within module compilations.
	moduleArrCounter int

	// Cache for C-string globals (null-terminated, used for panic messages etc.).
	// Keyed by content, returns the ir.Global. This deduplicates strings like
	// "out of memory" and "string index out of bounds" so they have stable names
	// across compilations regardless of mono instance ordering.
	cstrGlobals     map[string]*ir.Global
	fileNameGlobals map[string]*ir.Global // cache of per-file filename C string globals for panic_at

	// Lambda counter for unique anonymous function names
	lambdaCounter int

	// Lambda capture write-back: for move-captured variables, writes local values
	// back to the env struct before return so mutations persist across calls.
	lambdaWritebacks []lambdaWriteback

	// T1254: names of move-captured variables in the lambda body currently being
	// generated whose value is owned by the env struct and freed by the env drop
	// function (droppable heap captures: string/vector/heap-user/etc.). Returning
	// such a capture must hand back an independent clone — the env retains its own
	// copy (for repeat calls) and env_drop frees it, so returning the raw captured
	// pointer would double-free (caller frees the returned value + env_drop frees
	// the same allocation). Populated per-lambda in genLambdaExpr; saved/restored
	// so nested lambdas see only their own captures.
	lambdaEnvOwnedCaptures map[string]bool

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

	// T0387: synthesized clone functions for polymorphic dispatch in dupHeapValue.
	// Each concrete heap user type gets a clone fn that allocs + memcpy's using its
	// own static layout, then dups droppable sub-fields. Reached via the
	// clone_fn_ptr slot in typeinfo when dupHeapValue dispatches polymorphically.
	typeCloneFns     map[*types.Named]*ir.Func
	monoTypeCloneFns map[string]*ir.Func

	// VTable state
	hasChildren               map[*types.Named]bool           // true if any type declares `is ThisType`
	directChildren            map[*types.Named][]*types.Named // T1160: parent → types that declare `is Parent` (generic children included, unlike hasChildren)
	vtableGlobals             map[*types.Named]*ir.Global     // type → @promise_vtable_TypeName
	viewVtables               map[viewVtableKey]*ir.Global    // (concrete, view) → view-specific vtable
	valueTypeRTTI             map[*types.Named]*ir.Global     // value type → global RTTI instance (field 1 of value struct)
	interpBuilderWriterVtable *ir.Global                      // lazy: Builder→Writer vtable for string interpolation

	// Scope cleanup state: stack of active bindings for automatic close()/drop() at scope exit
	scopeBindings  []scopeBinding
	loopScopeDepth int // scopeBindings depth at loop entry (for break/continue cleanup)
	// T1331: temp-array lengths {stmt, heap, env, enum} captured at the innermost
	// enclosing loop's entry. A break/continue drains unclaimed temps down to this
	// floor — dropping temps created inside the loop body while preserving temps
	// owned by expressions that enclose the loop (e.g. a sibling call argument when
	// the loop is nested inside a block-value arg). Saved/restored per loop like
	// loopScopeDepth so nested loops each drain to their own entry depth.
	loopTempFloor [4]int

	// MutRef parameter pointers: maps param name to the pointer passed by the caller.
	// MutRef params are passed as pointers to the caller's alloca, enabling
	// write-back semantics (e.g., vector push with reallocation).
	mutRefPtrs  map[string]value.Value
	mutRefTypes map[string]irtypes.Type // inner LLVM type for load instructions

	// Drop flag tracking: maps variable name to its drop flag alloca (i1)
	dropFlags map[string]*ir.InstAlloca

	// T0849: per-subject `i1` downcast-success flag from a non-Force RTTI cast
	// (`x as T`) of an owned movable local. Set in genCastExpr, consumed at a
	// consuming site (return / owning-slot store) to make the subject's
	// scope-exit drop conditional: drop iff the downcast failed (`!isMatch`).
	castSubjectMatch map[string]value.Value

	// T0617: for-in loop binding name → alloca holding the current iteration's
	// container slot address (i8**), for single-owner-handle (Task) Vector/array
	// element loops. `<-handle` nulls *slot so the container's scope-exit drop
	// no-ops the consumed slot (mirrors the T0638 IndexExpr slot-null). Scoped
	// via per-loop save/restore in genForInVector/genForInArray (self-cleaning).
	forInHandleSlotPtr map[string]*ir.InstAlloca

	// Drop binding tracking: maps variable name to its scope binding (for reassignment drop)
	dropBindings map[string]scopeBinding

	// T0073: Statement-level temporary tracking for heap-allocated strings.
	// Tracks string temporaries from subexpressions (e.g., `42.to_string()` in
	// `assert(42.to_string() == "42")`) and drops them at statement end.
	stmtTemps                     []stmtTemp
	stmtTempMap                   map[value.Value]int            // SSA value → index in stmtTemps (-1 = claimed)
	mergeBoundStructFlag          map[value.Value]*ir.InstAlloca // T1211: value-struct/heap-user-type/Map match/if merge phi → i1 alloca holding its PER-PATH ownership flag (1 on owned-arm paths, 0 on borrowed-arm paths). These results are not i8*, so they never enter stmtTempMap; this parallel map lets captureLiveTempFlag thread the per-path bit into a bound local's drop flag (applyBoundMergeFlag), preventing a borrowed-path double-free. No drop obligation is attached (the arm-level heapTemp still self-cleans on the discard path)
	tempTrackingEnabled           bool                           // T0084: true in free functions + user method bodies
	dupStringFieldAccess          bool                           // T0095: when true, genFieldAccess dups string fields from droppable types
	boxSrcOwned                   bool                           // T1282: source of a string→structural box is an owned move; box takes the pointer (no dupString clone)
	dupContainerFieldAccess       bool                           // B0219: when true, genFieldAccess dups vector/channel fields from droppable types
	borrowBlockResult             bool                           // T0792: when true, genBlockValue treats its last expr as a borrow (no dup, no own) — set for error-handler recovery/else bodies whose result type is a ref (`T&`/`T~`)
	blockValueOwnedResult         bool                           // T1107: set by genBlockValue when its last-expr result is an owned heap temp moved out of the block (a claimed string/vector/handle temp, or an owned-local ident whose drop flag it cleared) — read immediately after by genIfExpr / genMatchArmValue to register the merge phi as an owned stmt temp
	blockValueOwnedFlag           value.Value                    // T1208: live per-path i1 ownership flag captured by genBlockValue when its last-expr result was a nested tracked temp (an if/match phi with its own per-path flag phi), loaded BEFORE claimStringTemp neutralized it — read alongside blockValueOwnedResult so trackMergeResultTemp threads the real per-path flag instead of the whole-arm constant (nil → fall back to the blockValueOwnedResult constant)
	suppressMergeResultTemp       bool                           // T1107: when true, trackMergeResultTemp skips registering the match/if phi as a caller stmt temp — set while evaluating a `go`-call argument, where ownership transfer of a conditional heap root into the goroutine frame is handled by the T1106 go-arg machinery (a caller statement-end drop would race the goroutine's async read)
	matchBorrowedIdents           map[string]bool                // T0485: idents bound by match destructure as borrows (no drop binding); T0672: also tuple-destructure locals from a borrow source (struct field / container index); if-let/while-let/force-unwrap must not transfer ownership
	borrowOptionalLocals          map[string]bool                // T1085: optional locals bound from a non-owning borrow (RTTI downcast `x as T` / `T&`/`T~` RHS) — their inner aliases an external owner, so a non-diverging heap-user-type handler unwrap (`o? { ... }`) must NOT neutralize+track the merged phi (the present arm is a borrow that would double-free)
	dupTupleFieldAccess           bool                           // T0370: when true, genVectorIndex dups droppable tuple elements on read
	dupHeapUserFieldAccess        bool                           // T0398: when true, genVectorIndex deep-clones heap user-type elements on read
	optionalStringDup             value.Value                    // B0190: pending dup from B0181 optional path; consumed by genOptionalForceUnwrap
	optionalContainerDup          value.Value                    // T0366: pending dup from Optional[Vector|Channel|Arc|Weak] field-read path
	optionalTupleDup              value.Value                    // T0397: pending dup from Map[K,(droppable,...)] index path; consumed by genOptionalForceUnwrap
	optionalHeapDup               value.Value                    // T0440: pending dup from Map[K, heap-user-type] index path; consumed by genOptionalForceUnwrap
	optionalFieldString           bool                           // B0190: set by genFieldAccess when loading a string? field from a droppable type
	optionalFieldVector           bool                           // T0354: set by genFieldAccess when loading a T[]? field from a droppable type
	optionalUnwrapContainerBorrow bool                           // T1143: set on the plain (no-dup) path of genOptionalForceUnwrap when the source is `container[k]!` — the inner aliases the container's slot and is borrowed (no dup), so trackHeapUserTypeResult must NOT register it as an owned temp (the container's drop frees it; tracking double-frees)
	returningBorrowedUnwrap       bool                           // T1302: set by genReturnStmt while evaluating a borrow-typed (`T&`/`T~`) return whose value force-unwraps a `this.field!` Optional — suppresses genOptionalForceUnwrap's T0428 Case 3B dup (the caller borrows the aliased inner and never frees it, so the dup would leak)

	// T0088: Statement-level tracking for heap-allocated droppable instances.
	// Tracks constructor results (e.g., _FnIter[T]) in iterator chains and drops
	// unclaimed instances at statement end to prevent memory leaks.
	heapTemps            []heapTemp
	heapTempMap          map[value.Value]int // instance i8* → index in heapTemps (-1 = claimed)
	lastClaimedDropFunc  *ir.Func            // T0127: drop func from last claimHeapTemp (nil if none claimed)
	pendingReceiverClaim value.Value         // T0130: deferred receiver claim from method dispatch

	// T0347: deferred binding-drop-flag clear for `r := this.method()` chains
	// where the method does `return this`. Set at the let/assign site, drained
	// after maybeRegisterDrop has stored i1 1 into the new binding's drop flag.
	pendingThisAliasClear *thisAliasClearReq

	// T1029: discarded-statement aliasing. discardedExpr is the outermost expression
	// of the current discarded ExprStmt (nil otherwise). When a call within a
	// discarded statement returns a value aliasing an owned-local ident arg, the
	// source local — not the result temp — must remain the single owner (it outlives
	// the statement). emitReturnAliasCheckSubst suppresses the source drop-flag clear
	// and records the arg's instance pointer in discardAliasArgPtrs; the result
	// temp's flag is cleared instead at the tracking site (emitDiscardAliasClears),
	// so the aliased allocation is freed once at scope exit rather than at statement
	// end (use-after-free). Cleared in nested-body contexts (genLambdaExpr,
	// genBlockValue, saveState) so only the discarded statement's own straight-line
	// expression records pointers.
	discardedExpr       ast.Expr
	discardAliasArgPtrs []value.Value

	// T1311: instance pointers of a structural-returning call's owned args, recorded
	// by emitReturnAliasCheckSubst (which bails early for structural returns before
	// the normal arg-alias loop) and consumed by maybeTrackIterTemp to clear the
	// aliasing result temp's flag. Short-lived per-call handoff: set immediately
	// before the call returns to genCallExpr, consumed right after in
	// maybeTrackIterTemp — not part of saveState. emitReturnAliasCheckSubst resets
	// it at entry, but not every dispatch path passes through there before reaching
	// maybeTrackIterTemp (container-method dispatch early-returns from genMethodCall),
	// so the reset alone does not guarantee freshness. pendingStructuralArgAliasCall
	// pins the ptrs to the exact CallExpr that recorded them: maybeTrackIterTemp only
	// consumes them when its own call `e` matches, so a stale value from an earlier
	// free-function structural discard can never be applied to an unrelated later call.
	pendingStructuralArgAliasPtrs []value.Value
	pendingStructuralArgAliasCall ast.Expr

	// T0100: Statement-level tracking for closure env pointers.
	// Tracks env structs from lambda expressions passed directly as function
	// arguments (not stored in variables). Unclaimed envs are freed at statement end.
	envTemps   []envTemp
	envTempMap map[value.Value]int // env i8* → index in envTemps (-1 = claimed)

	// B0285: Suppress match-dup during enum clone method compilation.
	// The synthesized clone body uses `match this` to destructure variant fields,
	// then explicitly calls .clone() on non-copy fields. Without suppression,
	// match-dup also clones fields (because the enum has drop), causing double-clone
	// and leaked intermediates. For recursive types this causes stack overflow.
	suppressMatchDup bool

	// T0428: Set to true when compiling a method whose receiver is owned (~this).
	// False for borrowed-this methods. Used by neutralizeMemberOptionalField and
	// genOptionalForceUnwrap to distinguish Cases 3A and 3B.
	thisRecvIsOwned bool

	// T0897: borrowed value parameters of the operator method currently being
	// compiled. Operator operands are borrowed (not moved) by design, so
	// `return other` would hand back a value aliasing the caller's still-live
	// operand → double-free. genReturnStmt deep-clones a returned bare operand
	// named in this set. nil for non-operator functions/methods.
	currentOpValueParams map[string]bool

	// T0945: borrowed value parameters of ANY function/method currently being
	// compiled — a plain (non-`~`) or `&` value parameter whose owner stays in
	// the caller. An inline elvis `(a ?: b)` whose left operand is such a param
	// must NOT free its some-path result: the inner aliases the caller's still-
	// owned value, so freeing it double-frees ("bad header magic") when the
	// caller drops the param. Populated per function via setBorrowedValueParams.
	borrowedValueParams map[string]bool

	// B0290: Tracks enums currently being processed by dupEnumElementInPlace
	// to detect recursive types and prevent infinite codegen.
	enumDupInProgress map[*types.Enum]bool

	// B0267/B0269: Inline enum constructor temp tracking. Entry-block allocas store
	// the enum pointer (bitcast to i8*) and a drop flag. Set by genEnumVariantCallLayout,
	// cleared by variable assignment, cleaned up at statement end. Supports multiple
	// inline enum constructors in the same statement (e.g., two enum args in one call).
	enumCtorTemps []enumCtorTemp

	// T1329: statement-boundary drains inside a block-value body (if/match arm,
	// `?` handler) skip temps below these floor indices — they are SIBLING temps
	// from the enclosing expression that were materialized before the block body
	// and must outlive it (the enclosing call re-reads them after the merge). Set
	// by genBlockValue to the tracking depths at block entry; 0 outside a
	// block-value body. cleanupStmtLevelTemps passes them to the *From drains.
	blockTempFloorStmt int
	blockTempFloorHeap int
	blockTempFloorEnv  int
	blockTempFloorEnum int

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
	coroEnd     *ir.Func // @llvm.coro.end(i8*, i1, token) → void
	coroFree    *ir.Func // @llvm.coro.free(token, i8*) → i8*
	coroResume  *ir.Func // @llvm.coro.resume(i8*) → void
	coroDestroy *ir.Func // @llvm.coro.destroy(i8*) → void
	coroDone    *ir.Func // @llvm.coro.done(i8*) → i1

	// PAL scheduler primitives (Phase 5c)
	palNumCPUs *ir.Func // @pal_num_cpus() → i32

	// PAL file I/O primitives (Phase D)
	palFileOpen                *ir.Func // @pal_file_open(i8* path, i32 mode) → i32
	palFileRead                *ir.Func // @pal_file_read(i32 fd, i8* buf, i64 len) → i64
	palFileWrite               *ir.Func // @pal_file_write(i32 fd, i8* buf, i64 len) → i64
	palFileClose               *ir.Func // @pal_file_close(i32 fd) → i32
	palPipeRead                *ir.Func // @pal_pipe_read(i32 fd, i8* buf, i64 len) → i64 (HANDLE-based on Windows)
	palPipeWrite               *ir.Func // @pal_pipe_write(i32 fd, i8* buf, i64 len) → i64 (HANDLE-based on Windows)
	palPipeClose               *ir.Func // @pal_pipe_close(i32 fd) → i32 (HANDLE-based on Windows)
	palFileSeek                *ir.Func // @pal_file_seek(i32 fd, i64 offset, i32 whence) → i64
	palFileStatSize            *ir.Func // @pal_file_stat_size(i8* path) → i64
	palFileRemove              *ir.Func // @pal_file_remove(i8* path) → i32
	palFileExists              *ir.Func // @pal_file_exists(i8* path) → i32
	palFileMkdir               *ir.Func // @pal_file_mkdir(i8* path) → i32
	palDirRemove               *ir.Func // @pal_dir_remove(i8* path) → i32
	palDirExists               *ir.Func // @pal_dir_exists(i8* path) → i32
	palDirOpen                 *ir.Func // @pal_dir_open(i8* path) → i8*
	palDirNextName             *ir.Func // @pal_dir_next_name(i8* handle) → i8*
	palDirClose                *ir.Func // @pal_dir_close(i8* handle) → void
	palErrno                   *ir.Func // @pal_errno() → i32
	palFileStat                *ir.Func // @pal_file_stat(i8* path, i64* out, i32 follow) → i32
	palGetEnv                  *ir.Func // @pal_getenv(i8* name) → i8* (value or null)
	palGetCwd                  *ir.Func // @pal_getcwd(i8* buf, i64 len) → i8* (buf or null)
	palSetEnv                  *ir.Func // @pal_setenv(i8* name, i8* value) → i32
	palUnsetEnv                *ir.Func // @pal_unsetenv(i8* name) → i32
	palChdir                   *ir.Func // @pal_chdir(i8* path) → i32
	palSpawn                   *ir.Func // @pal_spawn(i8* program, i8** argv, i32* out_stdout_fd, i32* out_stderr_fd) → i32
	palReadPipe                *ir.Func // @pal_read_pipe(i32 fd, i8** out_buf, i64* out_len) → void
	palWaitPid                 *ir.Func // @pal_wait_pid(i32 pid) → i32
	palSpawnStreaming          *ir.Func // @pal_spawn_streaming(..., i32* out_stdin_fd, i32* out_stdout_fd, i32* out_stderr_fd) → i32
	palSpawnEnv                *ir.Func // @pal_spawn_env(i8* program, i8** argv, i8** envp, i8* cwd, i32* out_stdout_fd, i32* out_stderr_fd) → i32
	palSpawnStreamingEnv       *ir.Func // @pal_spawn_streaming_env(..., i8** envp, i8* cwd, ...) → i32
	palKill                    *ir.Func // @pal_kill(i32 pid, i32 signal) → i32
	palExecReplace             *ir.Func // @pal_exec_replace(i8* path, i8** argv) → i32 (T0770)
	palGetEnviron              *ir.Func // @pal_get_environ() → i8**
	palGetUserInfo             *ir.Func // @pal_get_user_info(i8** out_name, i8** out_dir, i32* out_uid, i32* out_gid) → i32
	palGetHostname             *ir.Func // @pal_get_hostname(i8* buf, i64 len) → i8*
	palSignalInit              *ir.Func // @pal_signal_init() → i32 (rd_fd or -1)
	palSignalRegister          *ir.Func // @pal_signal_register(i32 signum) → i32
	palStackOverflowInit       *ir.Func // @pal_stack_overflow_init() → void
	palStackOverflowThreadInit *ir.Func // @pal_stack_overflow_thread_init() → void

	// PAL socket primitives (T0069)
	palSocketCreate      *ir.Func // @pal_socket_create(i32 domain, i32 type, i32 protocol) → i32
	palSocketBind        *ir.Func // @pal_socket_bind(i32 fd, i8* addr, i32 addrlen) → i32
	palSocketListen      *ir.Func // @pal_socket_listen(i32 fd, i32 backlog) → i32
	palSocketAccept      *ir.Func // @pal_socket_accept(i32 fd, i8* addr, i32* addrlen) → i32
	palSocketConnect     *ir.Func // @pal_socket_connect(i32 fd, i8* addr, i32 addrlen) → i32
	palSocketSend        *ir.Func // @pal_socket_send(i32 fd, i8* buf, i64 len, i32 flags) → i64
	palSocketRecv        *ir.Func // @pal_socket_recv(i32 fd, i8* buf, i64 len, i32 flags) → i64
	palSocketClose       *ir.Func // @pal_socket_close(i32 fd) → i32
	palSocketSetOpt      *ir.Func // @pal_socket_setopt(i32 fd, i32 level, i32 opt, i8* val, i32 len) → i32
	palSocketShutdown    *ir.Func // @pal_socket_shutdown(i32 fd, i32 how) → i32
	palSocketSetNonBlock *ir.Func // @pal_socket_set_nonblock(i32 fd) → i32
	palSocketGetError    *ir.Func // @pal_socket_get_error(i32 fd) → i32
	palGetAddrInfo       *ir.Func // @pal_getaddrinfo(i8* host, i8* port, i8* hints, i8** result) → i32
	palFreeAddrInfo      *ir.Func // @pal_freeaddrinfo(i8* result) → void

	// PAL IO reactor primitives (T0070)
	palReactorCreate *ir.Func // @pal_reactor_create() → i32
	palReactorAdd    *ir.Func // @pal_reactor_add(i32 rfd, i32 fd, i8* userdata) → i32
	palReactorRemove *ir.Func // @pal_reactor_remove(i32 rfd, i32 fd) → i32
	palReactorPoll   *ir.Func // @pal_reactor_poll(i32 rfd, i8* events_buf, i32 max_events, i32 timeout_ms) → i32
	palReactorClose  *ir.Func // @pal_reactor_close(i32 rfd) → i32

	// PAL high-level socket address operations (T0071)
	palSocketBindAddr     *ir.Func // @pal_socket_bind_addr(i32 fd, i8* host, i32 port) → i32
	palSocketConnectAddr  *ir.Func // @pal_socket_connect_addr(i32 fd, i8* host, i32 port) → i32
	palSocketAcceptAddr   *ir.Func // @pal_socket_accept_addr(i32 listen_fd) → i32
	palSocketGetLocalPort *ir.Func // @pal_socket_get_local_port(i32 fd) → i32

	// Signal pipe globals (NOT TLS — shared across all threads)
	signalPipeRdFd *ir.Global // @__promise_signal_pipe_rd (i32)

	// Command-line argument globals (populated from main's argc/argv)
	argcGlobal *ir.Global // @__promise_argc (i32)
	argvGlobal *ir.Global // @__promise_argv (i8**)

	// Spawn result TLS globals (cached between _os_spawn and _os_spawn_stdout_fd/stderr_fd)
	spawnStdoutFd *ir.Global // @__promise_spawn_stdout_fd (TLS, i32)
	spawnStderrFd *ir.Global // @__promise_spawn_stderr_fd (TLS, i32)
	spawnStdinFd  *ir.Global // @__promise_spawn_stdin_fd (TLS, i32)

	// Scheduler globals (Phase 5c — M:N scheduler)
	currentGGlobal        *ir.Global  // @__promise_current_g (TLS, i8*)
	currentPGlobal        *ir.Global  // @__promise_current_p (TLS, i8*) — current P for local queue ops
	currentMGlobal        *ir.Global  // @__promise_current_m (TLS, i8*) — current M for syscall handoff
	schedGlobal           *ir.Global  // @__promise_sched (global Sched struct)
	testPanicMsgGlobal    *ir.Global  // @__promise_test_panic_msg (non-TLS, i8*) — panic msg for test recovery
	testPanicTypeGlobal   *ir.Global  // @__promise_test_panic_type (non-TLS, i8) — 0=none, 1=rodata, 2=heap (T0275)
	testDoneGlobal        *ir.Global  // @__promise_test_done (non-TLS, i32) — set to 1 by trampoline on completion
	testDeadlineGlobal    *ir.Global  // @__promise_test_deadline (non-TLS, i64) — WASM cooperative test deadline; 0 = disabled (T0680)
	testTimedOutGlobal    *ir.Global  // @__promise_test_timed_out (non-TLS, i8) — set to 1 by coop scheduler on deadline (T0680)
	panicFlagGlobal       *ir.Global  // @__promise_panic_flag (TLS, i8) — 1 = panic in flight
	panicMsgTlsGlobal     *ir.Global  // @__promise_panic_msg (TLS, i8*) — C string pointer to panic message
	panicTypeTlsGlobal    *ir.Global  // @__promise_panic_type (TLS, i8) — 1=.rodata, 2=heap-allocated
	panicExitBlock        *ir.Block   // B0228: if set, emitPanicReturn branches here instead of ret (coroutine context)
	coroutineReturnBlock  *ir.Block   // B0353: if set, goroutine return branches here instead of ret
	inCoroutine           bool        // true when compiling inside a go block coroutine body
	goExprFireAndForget   bool        // true when go expr result is discarded (no <-task receiver)
	elvisResultConsumed   bool        // T0954: true when an inline elvis `?:` result is the operand of a consuming `<-` await
	elvisResultBound      bool        // T0952: true when an elvis `?:` result is bound directly to a variable/assignment target (claims the result temp and owns it unconditionally)
	elvisResultReturned   bool        // T0982: true when an elvis `?:` result is the return expression (escapes to the caller). Like elvisResultBound, forces none-path default neutralization for handle/heap results, but does NOT create a per-path elvisBoundDropFlag (no binding consumes it; the returned result temp is claimed by claimStringTemp/claimHeapTemp).
	elvisBoundDropFlag    value.Value // T0933/T0940/T0981: per-path drop flag (phi[someOwnsInner,noneOwned]) for a bound elvis `m := a ?: b`; consumed by the var-decl binding to replace maybeRegisterDrop's unconditional owning drop. nil otherwise. (T0940 generalizes the earlier T0933 heap-user-only `elvisBoundOwned`.)
	elvisResultOwnsForced bool        // T1166: true when an elvis `?:` result is assigned to a member/index target (an owned field/element with no per-slot drop flag). genElvis clones a borrowed operand on the some/none path so the result is unconditionally owned; the container's field/element drop is then correct (no double-free of a caller/container-owned inner).

	// T1353: staged, pre-evaluated receiver value for a compound member assignment
	// (`a.b += x`). When non-nil, the getter/setter receiver-eval sites consume it
	// via memberReceiver instead of re-evaluating target.Target, so the receiver is
	// emitted exactly once (and before the RHS). Set/consumed synchronously within
	// genMemberCompoundAssign; nil at all other times.
	stagedMemberReceiver value.Value

	// T1421: pre-evaluated values for synthetic AST nodes. genOptionalChainExpr
	// delegates its present-path member access to the full genMemberExpr machinery
	// (native getters, virtual dispatch, value-type receivers, etc.) by staging the
	// already-extracted inner value under a synthetic target node. genExpr returns
	// the staged value verbatim when it sees that node. nil/empty at all other times.
	stagedExprValues map[ast.Expr]value.Value

	// T1356: staged in-place i8* address of a value-type compound-assign receiver
	// reached through a side-effecting subscript (`vs[next()].sum += x`). The getter
	// read loads a value copy from this address while the setter write consumes the
	// same address, so the subscript's side effects fire exactly once. Set/consumed
	// synchronously within genMemberCompoundAssign; nil at all other times.
	stagedMemberReceiverAddr value.Value

	coroCleanupBlk *ir.Block // coroutine cleanup block (destroy path: coro.free + free)
	coroSuspendBlk *ir.Block // coroutine suspend block (suspend path: coro.end + ret)

	// T0668: maps each Task[T].drop func to its paired Task[T].free_after_done
	// func so temp/binding drop sites that only know the drop func can route
	// through the cooperative park-suspend join (emitTaskJoinAndFreeByDropFn).
	taskFreeAfterDone map[*ir.Func]*ir.Func
	// T0668: shared private deadlock message global for the WASM Task[T].drop
	// cooperative-step spin pump (created once, reused across instantiations).
	taskDeadlockMsgGlobal *ir.Global

	// Main function AST — saved so wrapMainWithScheduler can compile it inline
	mainDecl *ast.FuncDecl

	// T0262: Test function ASTs — saved so WASM batch tests can compile bodies
	// as coroutines in GenerateTestMain for cooperative scheduling.
	testDecls map[string]*ast.FuncDecl

	// Go expression counter for unique trampoline function names
	goCounter int

	// Generator state
	inGenerator           bool        // true when compiling inside a generator coroutine body
	generatorCanError     bool        // true when the generator body can propagate errors (B0023)
	generatorYieldSlot    value.Value // yield_slot parameter (i8*) of current generator coro
	generatorErrorSlot    value.Value // error_slot alloca (i8*) for failable generators (B0023)
	generatorCoroId       value.Value // coro.id token for current generator
	generatorCleanup      *ir.Block   // cleanup block for current generator
	generatorSuspend      *ir.Block   // suspend block for current generator (ramp exit)
	generatorFinalSuspend *ir.Block   // final suspend block (for early return)
	generatorCounter      int         // counter for unique generator function names

	// Failable go-block state (T1384) — active while compiling a `go! {}`
	// coroutine body. An escaping error (bare failable call, `?^`, `raise`)
	// is captured into the goroutine's result buffer as a failable
	// {ok,value,err} aggregate and branches to the coroutine's final suspend,
	// instead of the `ret wrapError(...)` used by ordinary failable functions
	// (invalid in a coroutine ramp whose return type is i8*).
	inFailableGoBlock           bool                // inside a `go! {}` coroutine body (failable scope)
	failableGoBlockAggType      *irtypes.StructType // {i1,T,i8*} aggregate stored into G.result_ptr
	failableGoBlockFinalSuspend *ir.Block           // branch target after storing an escaping error
	// One-shot: set by genGoBlock before evaluating a `go! {}` value body so
	// genBlockValue yields (not discards) a trailing bare failable call's
	// auto-propagated success value. Read-and-cleared at genBlockValue entry so
	// only the outermost block (the go! body) sees it — nested arm blocks don't.
	goBlockTrailingWantValue bool

	// T1427: on a `go! {}` value exit the computed heap success value is claimed away
	// from temp cleanup for the store into G.result_ptr. A failing use-binding close()
	// diverts that exit (emitCloseErrCheck → emitFailableGoBlockError), skipping the
	// store — so the value must be dropped on the divert or it leaks. Set tightly
	// around the emitScopeCleanup/emitCloseErrCheck pair on the two value exits.
	goResultDivertVal  value.Value
	goResultDivertType types.Type
	// T1427: success type T of the `go! {}` value body, threaded to genBlockValue so
	// the outermost body block can drop its trailing result on a close-error divert.
	// One-shot: read-and-cleared at genBlockValue entry (nested arm blocks see nil).
	goBlockResultDropType types.Type

	// T1392: the raw result LLVM type of a value-producing NON-failable `go {}`
	// body (nil when not in one, or when the body is void/fire-and-forget). A bare
	// `return` in such a body is an early exit that carries no trailing value; without
	// a defined store, `<-t` reads the uninitialized G.result_ptr buffer (poison).
	// genReturnStmt's coroutine branch stores this type's zero on that exit. The
	// failable analog uses failableGoBlockAggType instead.
	goBlockValueResultLLVM irtypes.Type

	// noinline wrappers around coro.resume/done/destroy — used by generator consumers
	// to hide the pattern from LLVM's coro-elide pass (which incorrectly stack-allocates
	// generator frames when it sees ramp+resume+done+destroy in the same function).
	genResume         *ir.Func             // @__promise_gen_resume(i8*) → void [noinline]
	genDone           *ir.Func             // @__promise_gen_done(i8*) → i1 [noinline]
	genDestroy        *ir.Func             // @__promise_gen_destroy(i8*) → void [noinline]
	iterCleanup       *ir.Func             // @__promise_iter_cleanup(i8*) → void (T0088: free env + instance)
	structuralDrop    *ir.Func             // @__promise_structural_drop(i8*) → void (B0270: RTTI-based drop for structural iface instances)
	structuralClone   *ir.Func             // @__promise_structural_clone(i8*) → i8* (T1284: RTTI-based deep clone for structural iface instances)
	noValueTypeInfo   *ir.Global           // @promise_typeinfo_novalue: shared null-drop typeinfo for primitive structural boxes (T1276)
	stringBoxTypeInfo *ir.Global           // @promise_typeinfo_stringbox: typeinfo whose drop_fn frees the cloned string + box (T1280)
	stringBoxDrop     *ir.Func             // @__promise_string_box_drop(i8*): drops the boxed string clone then frees the box (T1280)
	stringBoxClone    *ir.Func             // @__promise_string_box_clone(i8*)→i8*: deep-copies a boxed string for structural clone/slice (T1284)
	flatBoxTypeInfos  map[int64]*ir.Global // per-size null-drop typeinfo carrying a flat malloc+memcpy clone_fn for primitive/value structural boxes (T1284)
	flatBoxClones     map[int64]*ir.Func   // per-size @__promise_flat_box_clone_<size>(i8*)→i8* (T1284)

	// Target triple and platform flags
	target                string     // LLVM target triple
	isWasm                bool       // true if targeting wasm32
	isWasmWeb             bool       // true if targeting wasm32-web (browser/Node host, no WASI)
	isWindows             bool       // true if targeting windows-msvc
	windowsRuntimeEmitted bool       // T0772: guards one-time emission of the Windows crt0 + TLS/chkstk/_fltused support
	debugAllocator        bool       // scribble malloc'd (0xAA) + poison freed (0xDE) memory for UAF / uninit-read detection (debug builds)
	memoryLimitAccounting bool       // T0689: emit memory-limit counter + helpers (test binaries with -memory-limit > 0)
	needsNetpoll          bool       // true if net module imported — netpoll_init needed at startup (T0071)
	needsTLS              bool       // true if tls module bridged — OpenSSL archives linked on Linux (T0077)
	netpollBatchLock      *ir.Global // @__netpoll_batch_lock — held by reactor during event processing; close waits on it (B0324)
	nextDebugID           int        // counter for emitDebugPrint global names

	// Global constants for print/panic functions
	newlineGlobal     *ir.Global // "\n" (1 byte)
	panicPrefixGlobal *ir.Global // "panic: " (7 bytes)
}

// ptrIntType returns i32 for wasm32, i64 for 64-bit targets.
func (c *Compiler) ptrIntType() *irtypes.IntType {
	if c.isWasm {
		return irtypes.I32
	}
	return irtypes.I64
}

// ptrSize returns the pointer byte size (4 for wasm32, 8 for 64-bit).
func (c *Compiler) ptrSize() int {
	if c.isWasm {
		return 4
	}
	return 8
}

// ptrSizeConst returns a constant for the pointer size in the pointer-width int type.
func (c *Compiler) ptrSizeConst() *constant.Int {
	if c.isWasm {
		return constant.NewInt(irtypes.I32, 4)
	}
	return constant.NewInt(irtypes.I64, 8)
}

// typeSize returns the byte size of an LLVM type on the current target.
func (c *Compiler) typeSize(typ irtypes.Type) int {
	return llvmTypeSizeWithPtr(typ, c.ptrSize())
}

// typeAlign returns the alignment of an LLVM type on the current target.
func (c *Compiler) typeAlign(typ irtypes.Type) int {
	return llvmTypeAlignWithPtr(typ, c.ptrSize())
}

// scopeBindingKind distinguishes close() bindings (use) from drop() bindings.
type scopeBindingKind int

const (
	bindingClose        scopeBindingKind = iota // use-bound: call close() at scope exit
	bindingDrop                                 // droppable: call drop() at scope exit
	bindingDropString                           // string: call promise_string_drop (alloca is i8*, not value struct)
	bindingDropEnum                             // enum: call enum drop (alloca ptr bitcast to i8*) T0102
	bindingDropOptional                         // optional: check has-value flag, then drop inner value (T0101)
	bindingDropTuple                            // tuple value: walk fields and drop droppables at scope exit (T0371)
	bindingDropArray                            // fixed-size array: walk elements and drop droppables at scope exit (T0389)
	bindingFree                                 // heap-only: call pal_free (no drop method, just free the instance)
	bindingFreeEnv                              // closure env: free env pointer at scope exit
	bindingGenerator                            // generator: destroy coroutine + free yield slot at scope exit
)

// scopeBinding tracks a variable that needs cleanup at scope exit.
type scopeBinding struct {
	kind            scopeBindingKind
	alloca          *ir.InstAlloca
	closeFunc       *ir.Func       // direct dispatch for close() (nil if virtual)
	closeIsFailable bool           // true if close() method returns error
	dropFunc        *ir.Func       // direct dispatch for drop() (nil if virtual)
	named           *types.Named   // for virtual dispatch
	valType         types.Type     // original Promise type
	dropFlag        *ir.InstAlloca // i1: true=should drop (nil for close bindings)
	varName         string         // variable name (for drop flag lookup)
	monoSynthDrop   bool           // B0202: mono-time synthesized drop (skip post-drop pal_free)
	rttiDrop        bool           // B0226: dispatch drop via RTTI typeinfo drop_fn_ptr
	// Generator cleanup
	generatorHandle    *ir.InstAlloca // coro handle alloca (for destroy)
	generatorSlot      *ir.InstAlloca // yield slot alloca (for free)
	generatorErrorSlot *ir.InstAlloca // error slot alloca (for free, failable generators only, B0023)
}

// stmtTemp tracks a heap-allocated string/vector/channel temporary from a subexpression (T0073).
// Entry-block allocas are initialized to null/false so branch-produced temps
// have defined values on all paths.
type stmtTemp struct {
	alloca      *ir.InstAlloca // entry-block i8* alloca, initialized to null
	dropFlag    *ir.InstAlloca // entry-block i1, initialized to false
	dropFunc    *ir.Func       // B0219: drop function to call (promise_string_drop, Vector.drop, Channel[T].drop)
	elemType    types.Type     // T0109: vector element type for string-element drops (nil for non-vectors)
	arrType     *types.Array   // T1181: fixed-array temp — alloca holds [N x T] storage; cleanup walks elements & drops each (dropFunc nil)
	tupleType   *types.Tuple   // T1233: tuple temp — alloca holds the tuple aggregate; cleanup walks fields via emitVariantFieldDrop & drops each droppable one (dropFunc nil)
	perPathFlag bool           // T1208: dropFlag holds a genuine PER-PATH i1 (a flagPhi from an elvis/merge result — owned on one path, borrowed on another), not a compile-time constant. When true, an enclosing merge phi must thread this temp's live flag rather than a whole-arm constant, else it would drop a borrowed value on the borrowed path (use-after-free)
}

// heapTemp tracks a heap-allocated droppable instance from a constructor call (T0088).
// When the constructor result is stored in a named variable, the temp is "claimed"
// and not freed at statement end. Unclaimed temps are dropped at statement end.
type heapTemp struct {
	alloca   *ir.InstAlloca // entry-block i8* alloca (instance pointer)
	dropFlag *ir.InstAlloca // entry-block i1, initialized to false
	dropFunc *ir.Func       // concrete drop function to call
	elemType types.Type     // T0369: vector element type — when set, cleanupHeapTemps walks droppable elements before calling dropFunc. nil for non-vector heap temps.
}

// enumCtorTemp tracks an inline enum constructor alloca with droppable variant data (B0267/B0269).
type enumCtorTemp struct {
	alloca   *ir.InstAlloca // entry-block i8* alloca (stores bitcast of enum alloca)
	dropFlag *ir.InstAlloca // entry-block i1 alloca
	dropFunc *ir.Func       // enum drop function (takes i8*)
}

// thisAliasClearReq carries the value and return type from a let/assign site
// where the RHS is a chained method call rooted at `this`. The actual runtime
// alias check is emitted only after maybeRegisterDrop has set up the binding's
// drop flag (so we have a flag to clear). T0347.
type thisAliasClearReq struct {
	val     value.Value
	retType types.Type
}

// envTemp tracks a heap-allocated closure env pointer from a lambda expression (T0100).
// When the lambda is stored in a named variable, the env temp is "claimed" (the
// variable's scope binding handles freeing). Unclaimed envs are freed at statement end.
type envTemp struct {
	alloca   *ir.InstAlloca // entry-block i8* alloca (env pointer)
	dropFlag *ir.InstAlloca // entry-block i1, initialized to false
}

// closeErrCapture holds entry-block allocas used to capture the first failable
// close() error during scope cleanup. Used only when c.canError && !errorInFlight.
type closeErrCapture struct {
	flag *ir.InstAlloca // i1: true if a close error was captured
	val  *ir.InstAlloca // i8*: the captured error value
}

// viewVtableKey identifies a view-specific vtable for a (concrete, view) pair.
// For generic types, concreteName includes type args (e.g., "Entity__int").
type viewVtableKey struct {
	concreteName string
	view         *types.Named
}

// lambdaWriteback tracks a move-captured variable that needs its local value
// written back to the env struct on function exit, so mutations persist across calls.
type lambdaWriteback struct {
	localAlloca *ir.InstAlloca // local alloca in the lambda body
	envFieldPtr value.Value    // pointer into the env struct field
	elemType    irtypes.Type   // element type for load/store
}

// emitLambdaWritebacks stores local alloca values back to the env struct
// so that mutations to move-captured variables persist across lambda calls.
func (c *Compiler) emitLambdaWritebacks() {
	for _, wb := range c.lambdaWritebacks {
		val := c.block.NewLoad(wb.elemType, wb.localAlloca)
		c.block.NewStore(val, wb.envFieldPtr)
	}
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

// CollectMonoInstances returns all concrete generic type instances for the given sema info.
// Exported for use in main.go to enumerate instances before codegen (for cache key computation).
func CollectMonoInstances(info *sema.Info) []*types.Instance {
	return collectMonoInstances(info, make(map[string]bool))
}

// MonoName returns the mangled name for a generic type instance (e.g., "Box[int]").
// Exported for use in main.go to construct per-instance cache keys.
func MonoName(inst *types.Instance) string {
	return monoName(inst)
}

// CompileOptions configures optional compilation behavior.
type CompileOptions struct {
	CachedInstances       map[string]bool // mono instance names whose .bc is already cached
	CoverageEnabled       bool            // instrument code for test coverage (T0030)
	DebugAllocator        bool            // scribble malloc'd (0xAA) + poison freed (0xDE) memory for UAF / uninit-read detection (debug builds)
	MemoryLimitAccounting bool            // T0689: emit memory-limit counter + accounting bodies + helpers (test builds with -memory-limit > 0)
}

// CompileWithCache is like Compile but skips method body codegen for instances
// whose .bc files are already cached. cachedInstances maps mono instance names
// (e.g., "Box[int]") to true. Nil is treated the same as an empty map.
func CompileWithCache(file *ast.File, info *sema.Info, target string, cachedInstances map[string]bool) *CompileResult {
	return compile(file, info, target, &CompileOptions{CachedInstances: cachedInstances})
}

// CompileWithOptions generates LLVM IR with the given options.
func CompileWithOptions(file *ast.File, info *sema.Info, target string, opts *CompileOptions) *CompileResult {
	return compile(file, info, target, opts)
}

// Compile generates LLVM IR from a type-checked Promise AST.
func Compile(file *ast.File, info *sema.Info, target string) *CompileResult {
	return compile(file, info, target, nil)
}

// WasmLinkerMajorVersion, when set, returns the major LLVM version of the wasm
// LTO linker (wasm-ld). cmd/promise installs this so the emitted wasm32
// DataLayout can match the linker's expectations (T0764). Codegen never shells
// out to LLVM tools itself; this hook lets the version flow in from the CLI.
// Nil or a 0 return falls back to the long-standing pre-LLVM-23 layout.
var WasmLinkerMajorVersion func() int

// WasmDataLayout, when set, returns the wasm32 target DataLayout string that the
// build toolchain's own LLVM (opt/llc/wasm-ld share one target machine) actually
// uses for the given triple, probed once per triple by the CLI. Empty return =>
// fall back to the version-gated wasmDataLayout below. This makes the module
// layout equal the backend's by construction, fixing the i128 offset disagreement
// that silently miscompiled wide-int value types on wasm32 (T1544): the version
// guess produced a narrow layout without an i128 entry, so opt aligned i128 to 8
// (size 24) while llc/wasm-ld aligned it to 16 (size 32), and Vector's byte-sized
// writes and typed-GEP reads disagreed on element stride.
var WasmDataLayout func(triple string) string

// wasmDataLayout returns the wasm32 target DataLayout string matching the wasm
// LTO linker's LLVM major version. LLVM 23 added reference-type address spaces
// (p10=funcref, p20=externref), i128:128, and a non-integral pointer spec
// (ni:1:10:20) to the wasm32 target; its wasm-ld LTO backend aborts with
// "Target-incompatible DataLayout" if the module's layout lacks them. LLVM 22
// and earlier use the narrower layout and in turn reject the LLVM-23 string, so
// the two cannot share one layout — select by detected linker version (T0764).
// An unknown version (0) keeps the pre-23 layout. This is now the fallback path
// used only when the WasmDataLayout probe above is unavailable or fails (T1544).
func wasmDataLayout(llvmMajor int) string {
	if llvmMajor >= 23 {
		return "e-m:e-p:32:32-p10:8:8-p20:8:8-i64:64-i128:128-n32:64-S128-ni:1:10:20"
	}
	return "e-m:e-p:32:32-i64:64-n32:64-S128"
}

func compile(file *ast.File, info *sema.Info, target string, opts *CompileOptions) *CompileResult {
	module := ir.NewModule()
	if target == "" {
		target = HostTargetTriple()
	}
	module.TargetTriple = target
	if strings.Contains(target, "wasm32") {
		layout := ""
		if WasmDataLayout != nil {
			layout = WasmDataLayout(target)
		}
		if layout == "" { // probe unavailable/failed → version-gated guess (T0764)
			major := 0
			if WasmLinkerMajorVersion != nil {
				major = WasmLinkerMajorVersion()
			}
			layout = wasmDataLayout(major)
		}
		module.DataLayout = layout
	}

	c := &Compiler{
		module:    module,
		info:      info,
		rootInfo:  info,
		target:    target,
		isWasm:    strings.Contains(target, "wasm"),
		isWasmWeb: strings.Contains(target, "wasm") && strings.Contains(target, "web"),
		isWindows: strings.Contains(target, "windows"),
		funcs:     make(map[string]*ir.Func),

		monoLayouts:         make(map[string]*TypeDeclLayout),
		monoEnumLayouts:     make(map[string]*TypeDeclLayout),
		monoVtableGlobals:   make(map[string]*ir.Global),
		monoTypeInfoGlobals: make(map[string]*ir.Global),
		monoTypeIDs:         make(map[string]int32),
		monoValueTypeRTTI:   make(map[string]*ir.Global),

		typeIDs:              make(map[*types.Named]int32),
		nextTypeID:           1, // 0 reserved for "no type info"
		typeInfoGlobals:      make(map[*types.Named]*ir.Global),
		typeCloneFns:         make(map[*types.Named]*ir.Func),
		monoTypeCloneFns:     make(map[string]*ir.Func),
		hasChildren:          make(map[*types.Named]bool),
		directChildren:       make(map[*types.Named][]*types.Named),
		vtableGlobals:        make(map[*types.Named]*ir.Global),
		viewVtables:          make(map[viewVtableKey]*ir.Global),
		valueTypeRTTI:        make(map[*types.Named]*ir.Global),
		dropFlags:            make(map[string]*ir.InstAlloca),
		forInHandleSlotPtr:   make(map[string]*ir.InstAlloca),
		dropBindings:         make(map[string]scopeBinding),
		stmtTempMap:          make(map[value.Value]int),
		mergeBoundStructFlag: make(map[value.Value]*ir.InstAlloca), // T1211
		heapTempMap:          make(map[value.Value]int),
		envTempMap:           make(map[value.Value]int),
		thunks:               make(map[string]*ir.Func),
		taskFreeAfterDone:    make(map[*ir.Func]*ir.Func), // T0668
		file:                 file,
		moduleFuncs:          make(map[string]*ir.Func),
		moduleExterns:        make(map[string]*ExternFunc),
		moduleOwnedFuncs:     make(map[string]string),
		moduleCanonical:      make(map[string]string),
		instanceOwnedFuncs:   make(map[string]string),
		spiralInstances:      make(map[string]bool),
		cstrGlobals:          make(map[string]*ir.Global),
		fileNameGlobals:      make(map[string]*ir.Global),
	}

	// Apply options
	if opts != nil {
		c.cachedInstances = opts.CachedInstances
		c.coverageEnabled = opts.CoverageEnabled
		c.debugAllocator = opts.DebugAllocator
		c.memoryLimitAccounting = opts.MemoryLimitAccounting
	}

	c.buildParamDefaultInfos() // T1395

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
	// Pre-compute enum layouts for all imported modules first, so cross-module
	// enum types (e.g. JsonValue from json used as Map value in std) have their
	// named LLVM struct types available during monomorphization.
	if c.info.ModuleInfos != nil {
		for _, modInfo := range c.info.ModuleInfos {
			savedInfo := c.info
			c.info = modInfo.SemaInfo
			c.computeEnumLayouts(modInfo.File)
			c.info = savedInfo
		}
	}
	c.computeEnumLayouts(file)

	// Compute user type layouts and monomorphic instance layouts together so that
	// generic value-type instances used as fields (e.g. Outer { Pt[int] inner; })
	// are laid out before their containing user types. (T0565)
	monoInstances := collectMonoInstances(info, c.spiralInstances)
	c.computeAllTypeLayouts(file, monoInstances)
	monoFuncInstances := collectMonoFuncInstances(info, monoInstances)
	monoMethodInstances := collectMonoMethodInstances(info, monoInstances)
	crossResolveFuncMethodInstances(info, &monoFuncInstances, &monoMethodInstances)

	// Second pass: resolve type instances that appear inside generic function/method
	// bodies. Their substitution maps come from func/method instances, not type instances,
	// so they were missed by collectMonoInstances. (B0134)
	extraInstances := resolveTypeInstancesFromFuncInstances(info, monoInstances, monoFuncInstances, monoMethodInstances, c.spiralInstances)
	if len(extraInstances) > 0 {
		c.computeMonoLayouts(extraInstances)
		monoInstances = append(monoInstances, extraInstances...)
	}

	c.declareIntrinsics()
	c.declareMathIntrinsics()
	// declareExterns must run after computeUserTypeLayouts so that user type
	// layouts are available when resolving extern parameter/return types.
	c.declareExterns(externList, c.layouts)

	// Declare method stubs before vtable/typeinfo emission (vtable needs function pointers)
	c.declareTypeMethods(file)
	c.declareEnumMethods(file)
	c.declareMonoMethods(file, monoInstances)
	c.declareMonoEnumMethods(file, monoInstances)
	c.declareMonoSynthesizedDefaults(monoInstances)         // structural parent defaults
	c.declareSynthesizedDrops(file)                         // B0158: auto-synthesized drops (non-generic)
	c.declareSynthesizedEnumDrops(file)                     // T0102: auto-synthesized enum drops (non-generic)
	c.declareSynthesizedMonoDrops(file, monoInstances)      // B0158: auto-synthesized drops (generic)
	c.declareSynthesizedMonoEnumDrops(file, monoInstances)  // T0102: auto-synthesized enum drops (generic)
	c.declareSynthesizedEnumClones(file)                    // T1129: recursive enum clones (non-generic)
	c.declareSynthesizedMonoEnumClones(file, monoInstances) // T1129: recursive enum clones (generic)
	c.declareMonoInheritedDrops(monoInstances)              // T0468: drops inherited from generic parents
	c.declareInheritedDrops(file)                           // T0507: drops inherited from non-generic parents

	// T0862: non-generic types implementing a generic structural interface
	// (e.g. `is Box[int]`) need their inherited default methods declared before
	// the vtable is built — otherwise the vtable slot is left null and dispatch
	// through the interface view segfaults. Bodies are generated later (after
	// compileModules) by defineGenericStructuralDefaults.
	c.declareGenericStructuralDefaults(file)

	// Compute vtable info and emit vtable globals (after method stubs are declared)
	c.computeVtableInfo(file)
	c.computeMonoVtableInfo(monoInstances)
	c.emitVtableGlobals(file)
	c.emitMonoVtableGlobals(monoInstances)

	// Emit RTTI type info globals (after vtable globals, since typeinfo includes vtable ptr)
	c.emitTypeInfoGlobals(file)
	c.emitMonoTypeInfoGlobals(monoInstances)

	c.declareFuncs(file)
	c.declareMonoFuncs(file, monoFuncInstances)
	c.declareMonoMethodInstances(file, monoMethodInstances)

	// Compile imported modules into the same IR module (inline strategy).
	// Must run before definePALBodies/defineMathBodies/defineF64ToStringBridge so that
	// std module externs (promise_print_string, promise_nanotime, etc.) and functions
	// (_f64_to_str) are declared and available in c.module.Funcs / c.funcs.
	c.compileModules()
	// Add PAL-based function bodies to print/panic/time declarations.
	// Must run after compileModules so that std-declared externs (from std/io.pr,
	// std/time.pr, std/math.pr) are already in c.module.Funcs for lookup.
	c.definePALBodies()
	c.defineMathBodies()
	c.defineF64ToStringBridge() // bridge promise_f64_to_string → Promise _f64_to_str
	c.defineF32ToStringBridge() // bridge promise_f32_to_string → Promise _f32_to_str
	c.defineFileIOBodies()      // bridge io module externs → PAL file I/O functions
	c.defineOSBodies()          // bridge os module externs → PAL OS functions
	c.defineNetPALBodies()      // bridge net module externs → PAL socket functions (T0069)
	c.defineTLSPALBodies()      // bridge tls module externs → PAL OpenSSL functions (T0077)
	c.defineTimeBodies()        // bridge time module wall-clock extern → realtime clock (T0962)

	c.defineTypeMethods(file)
	c.defineEnumMethods(file)
	c.defineMonoMethods(file, monoInstances)
	c.defineMonoEnumMethods(file, monoInstances)
	c.defineMonoSynthesizedDefaults(monoInstances)         // structural parent defaults
	c.defineGenericStructuralDefaults(file)                // T0862: non-generic impls of generic structural interfaces
	c.defineSynthesizedDrops(file)                         // B0158: auto-synthesized drops (non-generic)
	c.defineSynthesizedEnumDrops(file)                     // T0102: auto-synthesized enum drops (non-generic)
	c.defineSynthesizedMonoDrops(file, monoInstances)      // B0158: auto-synthesized drops (generic)
	c.defineSynthesizedMonoEnumDrops(file, monoInstances)  // T0102: auto-synthesized enum drops (generic)
	c.defineSynthesizedEnumClones(file)                    // T1129: recursive enum clones (non-generic)
	c.defineSynthesizedMonoEnumClones(file, monoInstances) // T1129: recursive enum clones (generic)
	c.defineMonoInheritedDrops(monoInstances)              // T0468: drops inherited from generic parents
	c.defineInheritedDrops(file)                           // T0507: drops inherited from non-generic parents
	c.defineFuncs(file)
	c.defineMonoFuncs(file, monoFuncInstances)
	c.defineMonoMethodInstances(file, monoMethodInstances)

	// Wrap user main() as G0 in the M:N scheduler
	c.wrapMainWithScheduler()

	// T1089: group the codegen-emitted runtime helpers into the synthetic
	// __runtime module so SplitModuleIRs caches them like __mod_std. Must run
	// after all runtime bodies (including wrapMainWithScheduler) are emitted.
	c.tagRuntimeFuncs()

	return &CompileResult{
		Module:          c.module,
		Layouts:         c.layouts,
		EnumLayouts:     c.enumLayouts,
		Externs:         externList,
		CoverageRegions: c.coverageRegions,
		compiler:        c,
	}
}

// GenerateTestMain replaces the user's main() with a test runner that calls
// each test function via a codegen-emitted thread-based runner.
// On non-WASM targets, per-test panic recovery allows subsequent tests to run
// after a panic. The panic message is printed indented under the FAIL line.
// testTimeouts maps test function names to their computed timeout in nanoseconds.
// A timeout of 0 means no per-test timeout enforcement.
func (r *CompileResult) GenerateTestMain(tests []*types.Func, testTimeouts map[string]int64) {
	c := r.compiler

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
		ir.NewParam("skipped", irtypes.I32),
		ir.NewParam("leaked", irtypes.I32),
		ir.NewParam("timed_out", irtypes.I32),
		ir.NewParam("ignored", irtypes.I32),
		ir.NewParam("stale", irtypes.I32),
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
		mainFn = c.module.NewFunc("main", irtypes.I32,
			ir.NewParam("argc", irtypes.I32),
			ir.NewParam("argv", irtypes.NewPointer(irtypes.I8Ptr)))
		c.funcs["main"] = mainFn
	}

	entry := mainFn.NewBlock(".entry")

	// Store argc/argv into globals for os.args() / os.executable()
	entry.NewStore(mainFn.Params[0], c.argcGlobal)
	entry.NewStore(mainFn.Params[1], c.argvGlobal)

	// Register VEH handler for stack overflow detection (B0010) and crash
	// handling on Windows (B0148). Must be before sched_init which creates threads.
	entry.NewCall(c.palStackOverflowInit)

	// Initialize the M:N scheduler so goroutines spawned by test functions
	// get picked up by worker Ms. Without this, `go` blocks in batch tests
	// enqueue Gs that never get scheduled → deadlock (B0042).
	if c.isWasm {
		// T0262: Initialize cooperative scheduler with 1 P (single-threaded WASM).
		// No thread creation (sched_init returns early on WASM), no spin-wait.
		// Don't bump goroutine_counter — test G needs id=0 so goroutine_exit
		// sets sched.main_done, allowing sched_coop_run to exit.
		entry.NewCall(c.funcs["promise_sched_init"], constant.NewInt(irtypes.I32, 1))
	} else {
		numCPUs := entry.NewCall(c.palNumCPUs)
		entry.NewCall(c.funcs["promise_sched_init"], numCPUs)

		// Reserve G.id=0 for the main goroutine. In batch test mode there is
		// no main goroutine (G0), so bump the counter past 0. Without this,
		// the first goroutine spawned by a test gets id=0 and promise_panic
		// treats it as the main goroutine → exits instead of recovering (B0130).
		schedTy := schedStructType()
		counterField := entry.NewGetElementPtr(schedTy, c.schedGlobal,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(schedFieldGoroutineCounter)))
		c.emitAtomicAdd(entry, counterField, constant.NewInt(irtypes.I64, 1), irtypes.I64)

		// B0165: Wait for all worker threads to finish init (pal_stack_overflow_thread_init
		// allocates via pal_alloc on each thread), then reset alloc count to 0.
		// This excludes all scheduler allocations from per-test leak detection.
		readyField := entry.NewGetElementPtr(schedTy, c.schedGlobal,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(schedFieldReadyCount)))
		spinHeader := mainFn.NewBlock("sched_ready_spin")
		spinDone := mainFn.NewBlock("sched_ready_done")
		entry.NewBr(spinHeader)

		readyVal := spinHeader.NewAtomicRMW(enum.AtomicOpAdd, readyField, constant.NewInt(irtypes.I32, 0), enum.AtomicOrderingMonotonic)
		allReady := spinHeader.NewICmp(enum.IPredSGE, readyVal, numCPUs)
		spinYield := mainFn.NewBlock("sched_ready_yield")
		spinHeader.NewCondBr(allReady, spinDone, spinYield)

		// Yield briefly (100μs) before re-checking
		spinYield.NewCall(c.palUsleep, constant.NewInt(irtypes.I32, 100))
		spinYield.NewBr(spinHeader)

		entry = spinDone
	}

	// Initialize IO reactor if the net module is imported (T0071).
	// Must be after sched_init and before alloc count reset.
	// Event buffer is pre-allocated in netpoll_init and passed to the reactor
	// thread, so no sleep needed (B0326). The reactor thread does not call
	// pal_stack_overflow_thread_init (unlike scheduler worker threads).
	if c.needsNetpoll {
		if initFn, ok := c.funcs["promise_netpoll_init"]; ok {
			entry.NewCall(initFn)
		}
	}

	// B0165: Reset alloc count to 0 after scheduler init so scheduler
	// allocations don't leak into per-test leak detection.
	// This covers both WASM (no scheduler) and native (post-spin-wait).
	for _, g := range c.module.Globals {
		if g.Name() == "__promise_alloc_count" {
			if c.isWasm {
				entry.NewStore(constant.NewInt(irtypes.I64, 0), g)
			} else {
				entry.NewAtomicRMW(enum.AtomicOpXChg, g, constant.NewInt(irtypes.I64, 0), enum.AtomicOrderingMonotonic)
			}
			break
		}
	}

	// Allocate counters: passed and failed
	passedAlloca := entry.NewAlloca(irtypes.I32)
	failedAlloca := entry.NewAlloca(irtypes.I32)
	entry.NewStore(constant.NewInt(irtypes.I32, 0), passedAlloca)
	entry.NewStore(constant.NewInt(irtypes.I32, 0), failedAlloca)

	// Leak detection: look up alloc count global (T0020)
	var allocCountGlobal *ir.Global
	for _, g := range c.module.Globals {
		if g.Name() == "__promise_alloc_count" {
			allocCountGlobal = g
			break
		}
	}
	leakedCountAlloca := entry.NewAlloca(irtypes.I32)
	entry.NewStore(constant.NewInt(irtypes.I32, 0), leakedCountAlloca)
	timedOutCountAlloca := entry.NewAlloca(irtypes.I32)
	entry.NewStore(constant.NewInt(irtypes.I32, 0), timedOutCountAlloca)
	ignoredLeaksAlloca := entry.NewAlloca(irtypes.I32)
	entry.NewStore(constant.NewInt(irtypes.I32, 0), ignoredLeaksAlloca)

	// Leak message string constants (created once, used per-test) (T0020)
	var leakPrefixGlobal, leakSuffixGlobal *ir.Global
	if allocCountGlobal != nil {
		leakPrefixData := constant.NewCharArrayFromString("  leak: ")
		leakPrefixGlobal = c.module.NewGlobalDef(".str.leak_prefix", leakPrefixData)
		leakPrefixGlobal.Immutable = true
		leakPrefixGlobal.Linkage = enum.LinkagePrivate

		leakSuffixData := constant.NewCharArrayFromString(" allocations not freed\n")
		leakSuffixGlobal = c.module.NewGlobalDef(".str.leak_suffix", leakSuffixData)
		leakSuffixGlobal.Immutable = true
		leakSuffixGlobal.Linkage = enum.LinkagePrivate
	}

	// Count tests excluded for this target
	targetInfo := sema.ParseTargetInfo(c.target)
	skippedCount := 0
	for _, test := range tests {
		if excludes, ok := c.info.TestExcludes[test.Name()]; ok {
			for _, ex := range excludes {
				if sema.MatchTargetIdent(targetInfo, ex) {
					skippedCount++
					break
				}
			}
		}
	}

	// Allocate array for failed test name pointers and a counter
	totalTests := len(tests)
	failedNamesArrayType := irtypes.NewArray(uint64(totalTests), irtypes.I8Ptr)
	failedNamesAlloca := entry.NewAlloca(failedNamesArrayType)
	failedCountAlloca := entry.NewAlloca(irtypes.I32)
	entry.NewStore(constant.NewInt(irtypes.I32, 0), failedCountAlloca)

	// Allocate array for stale allow_leaks test name pointers and a counter
	staleNamesAlloca := entry.NewAlloca(failedNamesArrayType)
	staleCountAlloca := entry.NewAlloca(irtypes.I32)
	entry.NewStore(constant.NewInt(irtypes.I32, 0), staleCountAlloca)

	// Shared global for panic context prefix (used by all tests)
	panicIndentData := constant.NewCharArrayFromString("  panic: ")
	panicIndentGlobal := c.module.NewGlobalDef(".str.panic_indent", panicIndentData)
	panicIndentGlobal.Immutable = true
	panicIndentGlobal.Linkage = enum.LinkagePrivate

	// Shared global for timeout context prefix
	timeoutIndentData := constant.NewCharArrayFromString("  timeout: exceeded ")
	timeoutIndentGlobal := c.module.NewGlobalDef(".str.timeout_indent", timeoutIndentData)
	timeoutIndentGlobal.Immutable = true
	timeoutIndentGlobal.Linkage = enum.LinkagePrivate
	timeoutSuffixData := constant.NewCharArrayFromString(" limit\n")
	timeoutSuffixGlobal := c.module.NewGlobalDef(".str.timeout_suffix", timeoutSuffixData)
	timeoutSuffixGlobal.Immutable = true
	timeoutSuffixGlobal.Linkage = enum.LinkagePrivate

	for _, test := range tests {
		// Skip tests excluded for this target
		if excludes, ok := c.info.TestExcludes[test.Name()]; ok {
			excluded := false
			for _, ex := range excludes {
				if sema.MatchTargetIdent(targetInfo, ex) {
					excluded = true
					break
				}
			}
			if excluded {
				continue
			}
		}

		nameStr := test.Name()

		// Look up the test — on WASM, use saved FuncDecl; on native, use compiled IR function
		var testFn *ir.Func
		var testFd *ast.FuncDecl
		if c.isWasm {
			testFd = c.testDecls[nameStr]
			if testFd == nil {
				continue
			}
		} else {
			testFn = c.funcs[nameStr]
			if testFn == nil {
				continue
			}
		}

		// Create global string constant for the test name
		nameGlobal := c.module.NewGlobalDef(
			fmt.Sprintf(".test_name_%s", nameStr),
			constant.NewCharArrayFromString(nameStr+"\x00"),
		)
		nameGlobal.Immutable = true
		nameGlobal.Linkage = enum.LinkagePrivate

		// Look up per-test timeout (0 = no per-test timeout)
		var timeoutNs int64
		if testTimeouts != nil {
			timeoutNs = testTimeouts[nameStr]
		}
		timeoutConst := constant.NewInt(irtypes.I64, timeoutNs)

		// Compile-time-formatted timeout duration string for TIMEOUT context message (T1199).
		timeoutDurStr := formatDurationNs(timeoutNs)
		timeoutDurData := constant.NewCharArrayFromString(timeoutDurStr)
		timeoutDurGlobal := c.module.NewGlobalDef(fmt.Sprintf(".str.timeout_dur_%s", nameStr), timeoutDurData)
		timeoutDurGlobal.Immutable = true
		timeoutDurGlobal.Linkage = enum.LinkagePrivate

		// T0689: snapshot start counter and set memory limit before each test.
		// Only emitted when accounting is enabled (the helper symbol is only
		// declared by pal.EmitMemoryLimitHelpers in that case).
		if c.memoryLimitAccounting && r.testMemoryLimits != nil {
			limitBytes := r.testMemoryLimits[nameStr]
			for _, f := range c.module.Funcs {
				if f.Name() == "__promise_memory_set_test_state" {
					entry.NewCall(f, constant.NewInt(irtypes.I64, limitBytes))
					break
				}
			}
		}

		// T0275: Reset panic type before each test to prevent stale type=2 from
		// a previous timed-out test from adjusting the leak delta incorrectly.
		// (WASM also resets inside its scheduler-reset block below.)
		entry.NewStore(constant.NewInt(irtypes.I8, 0), c.testPanicTypeGlobal)

		// Snapshot alloc count before test for leak detection (T0020)
		var allocSnapshot value.Value
		if allocCountGlobal != nil {
			if c.isWasm {
				allocSnapshot = entry.NewLoad(irtypes.I64, allocCountGlobal)
			} else {
				allocSnapshot = entry.NewAtomicRMW(enum.AtomicOpAdd, allocCountGlobal, constant.NewInt(irtypes.I64, 0), enum.AtomicOrderingMonotonic)
			}
		}

		// Time the test: t0 = nanotime()
		t0 := entry.NewCall(nanotimeFn)

		// Execute the test
		var result value.Value
		if c.isWasm {
			// T0262: Compile test body as coroutine and run through cooperative scheduler.
			// This ensures channel ops use coro.suspend (not pal_cond_wait which deadlocks
			// on single-threaded WASM) and goroutines spawned by the test are scheduled.
			coroFn := c.compileTestCoroutine(nameStr, testFd)

			// Reset scheduler state for this test
			schedTy := schedStructType()
			mainDoneField := entry.NewGetElementPtr(schedTy, c.schedGlobal,
				constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(schedFieldMainDone)))
			entry.NewStore(constant.NewInt(irtypes.I8, 0), mainDoneField)
			counterField := entry.NewGetElementPtr(schedTy, c.schedGlobal,
				constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(schedFieldGoroutineCounter)))
			entry.NewStore(constant.NewInt(irtypes.I64, 0), counterField)

			// Clear test panic msg/type from previous test
			entry.NewStore(constant.NewNull(irtypes.I8Ptr), c.testPanicMsgGlobal)
			entry.NewStore(constant.NewInt(irtypes.I8, 0), c.testPanicTypeGlobal)

			// T0680 Part 2: arm the per-test deadline for the cooperative scheduler.
			// timeoutNs is a compile-time constant; >0 stores nanotime()+timeout
			// (reusing the t0 start capture above), 0 disables in-binary enforcement
			// (the outer process backstop still applies). Always clear the flag.
			if timeoutNs > 0 {
				deadline := entry.NewAdd(t0, constant.NewInt(irtypes.I64, timeoutNs))
				entry.NewStore(deadline, c.testDeadlineGlobal)
			} else {
				entry.NewStore(constant.NewInt(irtypes.I64, 0), c.testDeadlineGlobal)
			}
			entry.NewStore(constant.NewInt(irtypes.I8, 0), c.testTimedOutGlobal)

			// Create G0, enqueue, run cooperative scheduler
			handle := entry.NewCall(coroFn)
			g0 := entry.NewCall(c.funcs["promise_g_new"], handle)
			entry.NewCall(c.funcs["promise_sched_enqueue"], g0)
			entry.NewCall(c.funcs["promise_sched_coop_run"])

			// Determine result: timeout(2) > panic-fail(1) > pass(0). testTimedOut is
			// set by promise_sched_coop_step when the armed deadline is reached; the
			// leak/print/counter paths below already special-case result==2 (T0680).
			entry.NewStore(constant.NewInt(irtypes.I64, 0), c.testDeadlineGlobal) // disarm
			timedOut := entry.NewLoad(irtypes.I8, c.testTimedOutGlobal)
			isTimedOut := entry.NewICmp(enum.IPredNE, timedOut, constant.NewInt(irtypes.I8, 0))
			panicMsg := entry.NewLoad(irtypes.I8Ptr, c.testPanicMsgGlobal)
			hasPanic := entry.NewICmp(enum.IPredNE, panicMsg, constant.NewNull(irtypes.I8Ptr))
			failResult := entry.NewSelect(hasPanic, constant.NewInt(irtypes.I32, 1), constant.NewInt(irtypes.I32, 0))
			result = entry.NewSelect(isTimedOut, constant.NewInt(irtypes.I32, 2), failResult)
		} else {
			// Native: run test in a thread via promise_test_run
			fnPtr := entry.NewBitCast(testFn, irtypes.I8Ptr)
			result = entry.NewCall(testRunFn, fnPtr, timeoutConst)
		}

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

		// === Leak detection: run BEFORE printing so we can compute effective result ===
		// Skip for timed-out tests (thread still running → racy read)
		allowLeaks := false
		if allocCountGlobal != nil {
			allowLeaks = c.info.TestAllowLeaks[nameStr]
		}

		// effectiveResult: 0=pass, 1=fail, 2=timeout, 3=leak
		// LEAK (3) only when result==0, hasLeak, and !allowLeaks
		var effectiveResult value.Value
		var hasLeakPhi value.Value // i1: whether this test leaked (for printing detail)
		var deltaPhi value.Value   // i64: allocation delta (for printing detail)

		if allocCountGlobal != nil {
			leakCheckBlk := mainFn.NewBlock(fmt.Sprintf("leak_check_%s", nameStr))
			skipLeakCheckBlk := mainFn.NewBlock(fmt.Sprintf("skip_leak_check_%s", nameStr))
			afterLeakDetectBlk := mainFn.NewBlock(fmt.Sprintf("after_leak_detect_%s", nameStr))

			// Only check leaks if test didn't timeout (result != 2)
			isNotTimeout := entry.NewICmp(enum.IPredNE, result, constant.NewInt(irtypes.I32, 2))
			entry.NewCondBr(isNotTimeout, leakCheckBlk, skipLeakCheckBlk)

			// Wait for all goroutines to complete cleanup before reading alloc
			// count. goroutine_exit increments gs_completed after all frees
			// (coro.destroy + pal_free(G)); the drain spin-wait loop checks
			// gs_created == gs_completed.
			if !c.isWasm && c.schedGlobal != nil {
				schedTy := schedStructType()
				gsCreatedField := leakCheckBlk.NewGetElementPtr(schedTy, c.schedGlobal,
					constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(schedFieldGsCreated)))
				gsCompletedField := leakCheckBlk.NewGetElementPtr(schedTy, c.schedGlobal,
					constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(schedFieldGsCompleted)))

				drainDone := mainFn.NewBlock(fmt.Sprintf("drain_done_%s", nameStr))
				// B0320: Acquire ordering pairs with Release on gs_completed
				// increment in goroutine_exit, ensuring alloc_count decrements
				// are visible when the fast path observes drain complete.
				created0 := leakCheckBlk.NewAtomicRMW(enum.AtomicOpAdd, gsCreatedField, constant.NewInt(irtypes.I64, 0), enum.AtomicOrderingAcquire)
				completed0 := leakCheckBlk.NewAtomicRMW(enum.AtomicOpAdd, gsCompletedField, constant.NewInt(irtypes.I64, 0), enum.AtomicOrderingAcquire)
				fastDone := leakCheckBlk.NewICmp(enum.IPredEQ, created0, completed0)

				// B0315: Spin-wait with usleep instead of condvar wait. The
				// condvar-based approach had a lost-wakeup race on ARM64 where
				// the last goroutine's broadcast could fire before the main
				// thread entered cond_wait, causing an indefinite hang.
				drainSlow := mainFn.NewBlock(fmt.Sprintf("drain_slow_%s", nameStr))
				leakCheckBlk.NewCondBr(fastDone, drainDone, drainSlow)

				drainLoop := mainFn.NewBlock(fmt.Sprintf("drain_gs_%s", nameStr))
				// B0315: Counter for periodic wake_m nudge to prevent lost-wakeup
				// race where an M parks after find_runnable returns null but before
				// a newly enqueued goroutine's wake_m call finds idle Ms.
				counterAlloca := drainSlow.NewAlloca(irtypes.I32)
				drainSlow.NewStore(constant.NewInt(irtypes.I32, 0), counterAlloca)
				drainSlow.NewBr(drainLoop)

				created := drainLoop.NewAtomicRMW(enum.AtomicOpAdd, gsCreatedField, constant.NewInt(irtypes.I64, 0), enum.AtomicOrderingAcquire)
				completed := drainLoop.NewAtomicRMW(enum.AtomicOpAdd, gsCompletedField, constant.NewInt(irtypes.I64, 0), enum.AtomicOrderingAcquire)
				allDone := drainLoop.NewICmp(enum.IPredEQ, created, completed)

				drainWait := mainFn.NewBlock(fmt.Sprintf("drain_wait_%s", nameStr))
				drainLoop.NewCondBr(allDone, drainDone, drainWait)

				// Increment counter; every 100 iterations (~10ms) nudge idle Ms
				cnt := drainWait.NewLoad(irtypes.I32, counterAlloca)
				nextCnt := drainWait.NewAdd(cnt, constant.NewInt(irtypes.I32, 1))
				drainWait.NewStore(nextCnt, counterAlloca)
				shouldNudge := drainWait.NewICmp(enum.IPredEQ, nextCnt, constant.NewInt(irtypes.I32, 100))

				drainNudge := mainFn.NewBlock(fmt.Sprintf("drain_nudge_%s", nameStr))
				drainSleep := mainFn.NewBlock(fmt.Sprintf("drain_sleep_%s", nameStr))
				drainWait.NewCondBr(shouldNudge, drainNudge, drainSleep)

				// Reset counter and wake an idle M so it picks up queued goroutines
				drainNudge.NewStore(constant.NewInt(irtypes.I32, 0), counterAlloca)
				drainNudge.NewCall(c.funcs["promise_sched_wake_m"])
				drainNudge.NewBr(drainSleep)

				drainSleep.NewCall(c.palUsleep, constant.NewInt(irtypes.I32, 100)) // 100μs
				drainSleep.NewBr(drainLoop)

				leakCheckBlk = drainDone
			}

			// Read current alloc count and compute delta
			var currentAlloc value.Value
			if c.isWasm {
				currentAlloc = leakCheckBlk.NewLoad(irtypes.I64, allocCountGlobal)
			} else {
				currentAlloc = leakCheckBlk.NewAtomicRMW(enum.AtomicOpAdd, allocCountGlobal, constant.NewInt(irtypes.I64, 0), enum.AtomicOrderingMonotonic)
			}
			rawDelta := leakCheckBlk.NewSub(currentAlloc, allocSnapshot)
			// T0275: Discount heap-allocated panic msg from leak delta.
			// The msg is freed later in the print path, but the alloc count
			// was already captured. Adjustment prevents false LEAK for panics.
			panicType := leakCheckBlk.NewLoad(irtypes.I8, c.testPanicTypeGlobal)
			isHeapPanic := leakCheckBlk.NewICmp(enum.IPredEQ, panicType, constant.NewInt(irtypes.I8, 2))
			panicAdj := leakCheckBlk.NewSelect(isHeapPanic, constant.NewInt(irtypes.I64, 1), constant.NewInt(irtypes.I64, 0))
			delta := leakCheckBlk.NewSub(rawDelta, panicAdj)
			hasLeak := leakCheckBlk.NewICmp(enum.IPredSGT, delta, constant.NewInt(irtypes.I64, 0))

			// Compute effective result: upgrade pass→leak when hasLeak && !allowLeaks
			var effectiveInLeakPath value.Value
			if allowLeaks {
				effectiveInLeakPath = result // allow_leaks: keep original result
			} else {
				isOrigPass := leakCheckBlk.NewICmp(enum.IPredEQ, result, constant.NewInt(irtypes.I32, 0))
				shouldUpgrade := leakCheckBlk.NewAnd(isOrigPass, hasLeak)
				effectiveInLeakPath = leakCheckBlk.NewSelect(shouldUpgrade, constant.NewInt(irtypes.I32, 3), result)
			}
			leakCheckBlk.NewBr(afterLeakDetectBlk)

			// Skip path: timeout → no leak check, effectiveResult = result
			skipLeakCheckBlk.NewBr(afterLeakDetectBlk)

			// Merge: phi nodes for effectiveResult, hasLeak, delta
			effectiveResult = afterLeakDetectBlk.NewPhi(
				ir.NewIncoming(effectiveInLeakPath, leakCheckBlk),
				ir.NewIncoming(result, skipLeakCheckBlk),
			)
			hasLeakPhi = afterLeakDetectBlk.NewPhi(
				ir.NewIncoming(hasLeak, leakCheckBlk),
				ir.NewIncoming(constant.NewBool(false), skipLeakCheckBlk),
			)
			deltaPhi = afterLeakDetectBlk.NewPhi(
				ir.NewIncoming(delta, leakCheckBlk),
				ir.NewIncoming(constant.NewInt(irtypes.I64, 0), skipLeakCheckBlk),
			)

			entry = afterLeakDetectBlk
		} else {
			// No alloc tracking: effectiveResult = result, no leak possible
			effectiveResult = result
		}

		// === Print result with effective result code ===
		entry.NewCall(testPrintFn, namePtr, effectiveResult, elapsed)

		// === Print context for FAIL (panic message) ===
		{
			afterPanicBlk := mainFn.NewBlock(fmt.Sprintf("after_panic_%s", nameStr))
			isOrigFail := entry.NewICmp(enum.IPredEQ, result, constant.NewInt(irtypes.I32, 1))
			checkPanicBlk := mainFn.NewBlock(fmt.Sprintf("check_panic_%s", nameStr))
			entry.NewCondBr(isOrigFail, checkPanicBlk, afterPanicBlk)

			panicMsg := checkPanicBlk.NewLoad(irtypes.I8Ptr, c.testPanicMsgGlobal)
			hasPanicMsg := checkPanicBlk.NewICmp(enum.IPredNE, panicMsg, constant.NewNull(irtypes.I8Ptr))
			printPanicBlk := mainFn.NewBlock(fmt.Sprintf("print_panic_%s", nameStr))
			checkPanicBlk.NewCondBr(hasPanicMsg, printPanicBlk, afterPanicBlk)

			stdout := constant.NewInt(irtypes.I32, 1)
			indentPtr := printPanicBlk.NewGetElementPtr(panicIndentGlobal.ContentType, panicIndentGlobal,
				constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
			printPanicBlk.NewCall(c.palWrite, stdout, indentPtr, constant.NewInt(irtypes.I64, 9))
			msgLen := printPanicBlk.NewCall(c.funcs["strlen"], panicMsg)
			printPanicBlk.NewCall(c.palWrite, stdout, panicMsg, msgLen)
			nlPtr := printPanicBlk.NewGetElementPtr(c.newlineGlobal.ContentType, c.newlineGlobal,
				constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
			printPanicBlk.NewCall(c.palWrite, stdout, nlPtr, constant.NewInt(irtypes.I64, 1))

			// T0275: Free heap-allocated panic msg (type==2), skip for rodata (type==1)
			panicType := printPanicBlk.NewLoad(irtypes.I8, c.testPanicTypeGlobal)
			isHeap := printPanicBlk.NewICmp(enum.IPredEQ, panicType, constant.NewInt(irtypes.I8, 2))
			freePanicBlk := mainFn.NewBlock(fmt.Sprintf("free_panic_msg_%s", nameStr))
			afterFreeBlk := mainFn.NewBlock(fmt.Sprintf("after_free_panic_%s", nameStr))
			printPanicBlk.NewCondBr(isHeap, freePanicBlk, afterFreeBlk)

			freePanicBlk.NewCall(c.palFree, panicMsg)
			freePanicBlk.NewBr(afterFreeBlk)

			// Clear both globals after handling
			afterFreeBlk.NewStore(constant.NewNull(irtypes.I8Ptr), c.testPanicMsgGlobal)
			afterFreeBlk.NewStore(constant.NewInt(irtypes.I8, 0), c.testPanicTypeGlobal)
			afterFreeBlk.NewBr(afterPanicBlk)

			entry = afterPanicBlk
		}

		// === Print context for TIMEOUT ===
		{
			afterTimeoutCtxBlk := mainFn.NewBlock(fmt.Sprintf("after_timeout_ctx_%s", nameStr))
			isOrigTimeout := entry.NewICmp(enum.IPredEQ, result, constant.NewInt(irtypes.I32, 2))
			printTimeoutCtxBlk := mainFn.NewBlock(fmt.Sprintf("print_timeout_ctx_%s", nameStr))
			entry.NewCondBr(isOrigTimeout, printTimeoutCtxBlk, afterTimeoutCtxBlk)

			// Print "  timeout: exceeded <dur> limit\n" using compile-time-formatted duration (T1199)
			stdout := constant.NewInt(irtypes.I32, 1)
			toIndentPtr := printTimeoutCtxBlk.NewGetElementPtr(timeoutIndentGlobal.ContentType, timeoutIndentGlobal,
				constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
			printTimeoutCtxBlk.NewCall(c.palWrite, stdout, toIndentPtr, constant.NewInt(irtypes.I64, 20))
			toDurPtr := printTimeoutCtxBlk.NewGetElementPtr(timeoutDurGlobal.ContentType, timeoutDurGlobal,
				constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
			printTimeoutCtxBlk.NewCall(c.palWrite, stdout, toDurPtr, constant.NewInt(irtypes.I64, int64(len(timeoutDurStr))))
			toSuffixPtr := printTimeoutCtxBlk.NewGetElementPtr(timeoutSuffixGlobal.ContentType, timeoutSuffixGlobal,
				constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
			printTimeoutCtxBlk.NewCall(c.palWrite, stdout, toSuffixPtr, constant.NewInt(irtypes.I64, 7))
			printTimeoutCtxBlk.NewBr(afterTimeoutCtxBlk)

			entry = afterTimeoutCtxBlk
		}

		// === Print leak detail and update leak counters ===
		if allocCountGlobal != nil {
			printLeakDetailBlk := mainFn.NewBlock(fmt.Sprintf("print_leak_detail_%s", nameStr))
			noLeakDetailBlk := mainFn.NewBlock(fmt.Sprintf("no_leak_detail_%s", nameStr))
			afterLeakDetailBlk := mainFn.NewBlock(fmt.Sprintf("after_leak_detail_%s", nameStr))

			entry.NewCondBr(hasLeakPhi, printLeakDetailBlk, noLeakDetailBlk)

			// Print "  leak: <N> allocations not freed\n"
			c.emitLeakMessage(printLeakDetailBlk, deltaPhi, leakPrefixGlobal, leakSuffixGlobal)

			if allowLeaks {
				// allow_leaks: increment ignored counter (T0067)
				curIgnored := printLeakDetailBlk.NewLoad(irtypes.I32, ignoredLeaksAlloca)
				printLeakDetailBlk.NewStore(
					printLeakDetailBlk.NewAdd(curIgnored, constant.NewInt(irtypes.I32, 1)),
					ignoredLeaksAlloca,
				)
			} else {
				// No allow_leaks: increment leaked counter (T0067)
				curLeaked := printLeakDetailBlk.NewLoad(irtypes.I32, leakedCountAlloca)
				printLeakDetailBlk.NewStore(
					printLeakDetailBlk.NewAdd(curLeaked, constant.NewInt(irtypes.I32, 1)),
					leakedCountAlloca,
				)
			}
			printLeakDetailBlk.NewBr(afterLeakDetailBlk)

			if allowLeaks {
				// allow_leaks but no leak: print stale tag warning (T0067)
				c.emitStaleAllowLeaksWarning(noLeakDetailBlk, nameStr)
				staleIdx := noLeakDetailBlk.NewLoad(irtypes.I32, staleCountAlloca)
				staleSlot := noLeakDetailBlk.NewGetElementPtr(failedNamesArrayType, staleNamesAlloca,
					constant.NewInt(irtypes.I32, 0), staleIdx)
				noLeakDetailBlk.NewStore(namePtr, staleSlot)
				noLeakDetailBlk.NewStore(
					noLeakDetailBlk.NewAdd(staleIdx, constant.NewInt(irtypes.I32, 1)),
					staleCountAlloca,
				)
			}
			noLeakDetailBlk.NewBr(afterLeakDetailBlk)

			entry = afterLeakDetailBlk
		}

		// === Update counters: 4-way based on effectiveResult ===
		// pass(0) → passed++, fail(1) → failed++, timeout(2) → timedOut++, leak(3) → leaked already incremented above
		currentPassed := entry.NewLoad(irtypes.I32, passedAlloca)
		currentFailed := entry.NewLoad(irtypes.I32, failedAlloca)
		currentTimedOut := entry.NewLoad(irtypes.I32, timedOutCountAlloca)

		isPass := entry.NewICmp(enum.IPredEQ, effectiveResult, constant.NewInt(irtypes.I32, 0))
		isFail := entry.NewICmp(enum.IPredEQ, effectiveResult, constant.NewInt(irtypes.I32, 1))
		isTimeout := entry.NewICmp(enum.IPredEQ, effectiveResult, constant.NewInt(irtypes.I32, 2))

		newPassed := entry.NewSelect(isPass, entry.NewAdd(currentPassed, constant.NewInt(irtypes.I32, 1)), currentPassed)
		newFailed := entry.NewSelect(isFail, entry.NewAdd(currentFailed, constant.NewInt(irtypes.I32, 1)), currentFailed)
		newTimedOut := entry.NewSelect(isTimeout, entry.NewAdd(currentTimedOut, constant.NewInt(irtypes.I32, 1)), currentTimedOut)

		entry.NewStore(newPassed, passedAlloca)
		entry.NewStore(newFailed, failedAlloca)
		entry.NewStore(newTimedOut, timedOutCountAlloca)

		// === Store in failedNames if effectiveResult != 0 ===
		failStoreBlock := mainFn.NewBlock(fmt.Sprintf("store_fail_%s", nameStr))
		skipStoreBlock := mainFn.NewBlock(fmt.Sprintf("skip_fail_%s", nameStr))
		isNonPass := entry.NewICmp(enum.IPredNE, effectiveResult, constant.NewInt(irtypes.I32, 0))
		entry.NewCondBr(isNonPass, failStoreBlock, skipStoreBlock)

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
	finalLeaked := entry.NewLoad(irtypes.I32, leakedCountAlloca)
	finalTimedOut := entry.NewLoad(irtypes.I32, timedOutCountAlloca)
	finalIgnored := entry.NewLoad(irtypes.I32, ignoredLeaksAlloca)
	finalStale := entry.NewLoad(irtypes.I32, staleCountAlloca)
	entry.NewCall(testSummaryFn, finalPassed, finalFailed, constant.NewInt(irtypes.I32, int64(skippedCount)), finalLeaked, finalTimedOut, finalIgnored, finalStale)

	// Print FAILED: list if any failures
	failedHeaderData := constant.NewCharArrayFromString("FAILED:\n")
	failedHeaderGlobal := c.module.NewGlobalDef(".str.failed_header", failedHeaderData)
	failedHeaderGlobal.Immutable = true
	failedHeaderGlobal.Linkage = enum.LinkagePrivate
	failedIndentData := constant.NewCharArrayFromString("  ")
	failedIndentGlobal := c.module.NewGlobalDef(".str.failed_indent", failedIndentData)
	failedIndentGlobal.Immutable = true
	failedIndentGlobal.Linkage = enum.LinkagePrivate
	staleHeaderData := constant.NewCharArrayFromString("STALE ALLOW_LEAKS:\n")
	staleHeaderGlobal := c.module.NewGlobalDef(".str.stale_header", staleHeaderData)
	staleHeaderGlobal.Immutable = true
	staleHeaderGlobal.Linkage = enum.LinkagePrivate
	stdout := constant.NewInt(irtypes.I32, 1)

	finalFailedCount := entry.NewLoad(irtypes.I32, failedCountAlloca)
	hasFailures := entry.NewICmp(enum.IPredSGT, finalFailedCount, constant.NewInt(irtypes.I32, 0))
	printFailBlock := mainFn.NewBlock("print_failures")
	checkStaleBlock := mainFn.NewBlock("check_stale")
	doneBlock := mainFn.NewBlock("done")
	entry.NewCondBr(hasFailures, printFailBlock, checkStaleBlock)

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

	loopEndBlock.NewBr(checkStaleBlock)

	// Print STALE ALLOW_LEAKS: list if any stale tags
	hasStale := checkStaleBlock.NewICmp(enum.IPredSGT, finalStale, constant.NewInt(irtypes.I32, 0))
	printStaleBlock := mainFn.NewBlock("print_stale")
	checkStaleBlock.NewCondBr(hasStale, printStaleBlock, doneBlock)

	// Print "STALE ALLOW_LEAKS:\n" header
	staleHdrPtr := printStaleBlock.NewGetElementPtr(staleHeaderGlobal.ContentType, staleHeaderGlobal,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
	printStaleBlock.NewCall(c.palWrite, stdout, staleHdrPtr, constant.NewInt(irtypes.I64, 19))

	// Loop through stale names
	staleLoopBlock := mainFn.NewBlock("stale_loop")
	staleLoopEndBlock := mainFn.NewBlock("stale_loop_end")
	printStaleBlock.NewBr(staleLoopBlock)

	staleIdxPhi := staleLoopBlock.NewPhi(ir.NewIncoming(constant.NewInt(irtypes.I32, 0), printStaleBlock))

	staleNameSlot := staleLoopBlock.NewGetElementPtr(failedNamesArrayType, staleNamesAlloca,
		constant.NewInt(irtypes.I32, 0), staleIdxPhi)
	staleNamePtr := staleLoopBlock.NewLoad(irtypes.I8Ptr, staleNameSlot)

	staleIndentPtr := staleLoopBlock.NewGetElementPtr(failedIndentGlobal.ContentType, failedIndentGlobal,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
	staleLoopBlock.NewCall(c.palWrite, stdout, staleIndentPtr, constant.NewInt(irtypes.I64, 2))
	staleNameLen := staleLoopBlock.NewCall(c.funcs["strlen"], staleNamePtr)
	staleLoopBlock.NewCall(c.palWrite, stdout, staleNamePtr, staleNameLen)
	staleNlPtr := staleLoopBlock.NewGetElementPtr(c.newlineGlobal.ContentType, c.newlineGlobal,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
	staleLoopBlock.NewCall(c.palWrite, stdout, staleNlPtr, constant.NewInt(irtypes.I64, 1))

	staleNextIdx := staleLoopBlock.NewAdd(staleIdxPhi, constant.NewInt(irtypes.I32, 1))
	totalStaleCount := staleLoopBlock.NewLoad(irtypes.I32, staleCountAlloca)
	staleLoopDone := staleLoopBlock.NewICmp(enum.IPredSGE, staleNextIdx, totalStaleCount)
	staleIdxPhi.Incs = append(staleIdxPhi.Incs, ir.NewIncoming(staleNextIdx, staleLoopBlock))
	staleLoopBlock.NewCondBr(staleLoopDone, staleLoopEndBlock, staleLoopBlock)

	staleLoopEndBlock.NewBr(doneBlock)

	// Shut down the scheduler (join worker Ms) before exiting
	if !c.isWasm {
		doneBlock.NewCall(c.funcs["promise_sched_shutdown"])
	}

	// Emit coverage data if coverage is enabled (T0030)
	doneBlock = c.emitCoverageOutput(doneBlock, mainFn)

	// Return 0 if all passed, 1 if any failed, leaked, or timed out (T0067)
	hasFailed := doneBlock.NewICmp(enum.IPredSGT, finalFailed, constant.NewInt(irtypes.I32, 0))
	hasLeakedUntagged := doneBlock.NewICmp(enum.IPredSGT, finalLeaked, constant.NewInt(irtypes.I32, 0))
	hasTimedOut := doneBlock.NewICmp(enum.IPredSGT, finalTimedOut, constant.NewInt(irtypes.I32, 0))
	retHasFailures := doneBlock.NewOr(hasFailed, doneBlock.NewOr(hasLeakedUntagged, hasTimedOut))
	retVal := doneBlock.NewSelect(retHasFailures, constant.NewInt(irtypes.I32, 1), constant.NewInt(irtypes.I32, 0))

	// On Windows, call ExitProcess to avoid CRT cleanup crashes during
	// thread teardown (STATUS_ACCESS_VIOLATION in TLS callbacks). B0148.
	if c.isWindows && !c.isWasm {
		doneBlock.NewCall(c.palExit, retVal)
		doneBlock.NewUnreachable()
	} else {
		doneBlock.NewRet(retVal)
	}

	// WASM: emit the entry point if not already present (test-only files have
	// no user main). On wasm32-wasi this is @_start; on wasm32-web it is
	// @_initialize (called from JS / Node harness — there is no WASI runtime).
	if c.isWasm {
		entryName := "_start"
		if c.isWasmWeb {
			entryName = "_initialize"
		}
		hasEntry := false
		for _, f := range c.module.Funcs {
			if f.Name() == entryName {
				hasEntry = true
				break
			}
		}
		if !hasEntry {
			c.emitWasmStart(mainFn)
		}
	}

	// Windows: emit the self-contained crt0 entry + CRT-replacement runtime
	// support so test binaries link with no MSVC/SDK files (T0772).
	if c.isWindows && !c.isWasm {
		c.emitWindowsEntry(mainFn)
	}
}

// compileTestCoroutine compiles a test function body as a coroutine for WASM
// cooperative scheduling (T0262). Returns the coroutine ramp function.
//
// This mirrors the main() coroutine pattern in sched.go (wrapMainWithScheduler)
// and the go-block coroutine pattern in expr.go. The test body is compiled with
// inCoroutine=true so channel ops use coro.suspend + waiter-list parking instead
// of thread-blocking pal_cond_wait.
func (c *Compiler) compileTestCoroutine(nameStr string, fd *ast.FuncDecl) *ir.Func {
	coroFn := c.module.NewFunc(fmt.Sprintf(".test_coro.%s", nameStr), irtypes.I8Ptr)
	coroFn.FuncAttrs = append(coroFn.FuncAttrs, rawFuncAttr("presplitcoroutine"))

	// Save compiler state
	saved := c.saveState()
	savedInCoroutine := c.inCoroutine
	savedCoroCleanup := c.coroCleanupBlk
	savedCoroSuspend := c.coroSuspendBlk
	savedPanicExitBlock := c.panicExitBlock
	savedGoExprFF := c.goExprFireAndForget
	savedCoroutineReturnBlock := c.coroutineReturnBlock
	c.goExprFireAndForget = false

	// Reset state for coroutine compilation
	c.fn = coroFn
	c.locals = make(map[string]*ir.InstAlloca)
	// A `~`-param helper compiled just before this test (e.g. `bump(T ~v)`) leaves
	// stale mutRefPtrs/mutRefTypes entries. genIdentExpr consults mutRefPtrs BEFORE
	// locals, so a same-named local in the test body (`v := ...`) would load through
	// the helper's pointer with the helper's LLVM type — emitting an ill-typed
	// `load`/`extractvalue` that fails opt on WASM. Test bodies take no params, so
	// clearing both maps here is always correct (T1585).
	c.mutRefPtrs = nil
	c.mutRefTypes = nil
	c.localNameCount = make(map[string]int)
	c.blockCounter = 0
	c.canError = false
	c.currentRetType = nil
	c.scopeBindings = nil
	c.dropFlags = make(map[string]*ir.InstAlloca)
	c.castSubjectMatch = nil
	c.dropBindings = make(map[string]scopeBinding)
	c.stmtTemps = nil
	c.stmtTempMap = make(map[value.Value]int)
	c.heapTemps = nil
	c.heapTempMap = make(map[value.Value]int)
	c.envTemps = nil
	c.envTempMap = make(map[value.Value]int)
	c.enumCtorTemps = nil // B0267
	c.tempTrackingEnabled = true
	c.loopScopeDepth = 0
	c.loopTempFloor = [4]int{} // T1331
	c.inCoroutine = true

	// --- Coroutine preamble ---
	coroEntry := coroFn.NewBlock(".entry")
	c.block = coroEntry

	coroId := coroEntry.NewCall(c.coroId,
		constant.NewInt(irtypes.I32, 0),
		constant.NewNull(irtypes.I8Ptr),
		constant.NewNull(irtypes.I8Ptr),
		constant.NewNull(irtypes.I8Ptr))

	need := coroEntry.NewCall(c.coroAlloc, coroId)
	allocBlk := coroFn.NewBlock("coro.alloc")
	startBlk := coroFn.NewBlock("coro.start")
	coroEntry.NewCondBr(need, allocBlk, startBlk)

	coroSizeVal := allocBlk.NewCall(c.coroSize)
	// WASM: coro.size returns i32, pal_alloc takes i64
	coroSizeArg := allocBlk.NewZExt(coroSizeVal, irtypes.I64)
	mem := allocBlk.NewCall(c.palAlloc, coroSizeArg)
	allocBlk.NewBr(startBlk)

	phiMem := startBlk.NewPhi(
		ir.NewIncoming(constant.NewNull(irtypes.I8Ptr), coroEntry),
		ir.NewIncoming(mem, allocBlk))
	hdl := startBlk.NewCall(c.coroBegin, coroId, phiMem)

	// Initial suspend — in a separate block so createEntryAlloca can append
	// allocas to startBlk BEFORE the suspend point (coro-split needs allocas
	// to precede coro.suspend to properly spill them to the frame).
	initSuspBlk := coroFn.NewBlock("coro.init.suspend")
	startBlk.NewBr(initSuspBlk)

	initResult := initSuspBlk.NewCall(c.coroSuspend, constant.None, constant.False)

	suspendBlk := coroFn.NewBlock("coro.suspend")
	bodyBlk := coroFn.NewBlock("body")
	cleanupBlk := coroFn.NewBlock("cleanup")
	doneBlk := coroFn.NewBlock("coro.done")
	finalSuspBlk := coroFn.NewBlock("final.suspend")

	initSuspBlk.NewSwitch(initResult, suspendBlk,
		ir.NewCase(constant.NewInt(irtypes.I8, 0), bodyBlk),
		ir.NewCase(constant.NewInt(irtypes.I8, 1), cleanupBlk))

	suspendBlk.NewRet(hdl)

	// Set cleanup and suspend blocks for mid-body coro.suspend switches
	c.coroCleanupBlk = cleanupBlk
	c.coroSuspendBlk = doneBlk

	// --- Body ---
	c.block = bodyBlk
	c.entryBlock = startBlk // allocas go in startBlk (part of coroutine frame)

	// Create panic exit block for this test coroutine
	testPanicExitBlk := coroFn.NewBlock("test.panic_exit")
	c.panicExitBlock = testPanicExitBlk
	c.coroutineReturnBlock = finalSuspBlk

	c.genBlock(fd.Body)

	// Clear panic exit and coroutine return blocks after body generation
	c.panicExitBlock = nil
	c.coroutineReturnBlock = nil

	// Emit scope cleanup for any bindings registered before genBlock (e.g. channels)
	if c.block != nil && c.block.Term == nil && len(c.scopeBindings) > 0 {
		c.emitScopeCleanup(0, false)
	}

	// T0148: Final panic check after body + scope cleanup
	if c.block != nil && c.block.Term == nil {
		finalFlag := c.block.NewLoad(irtypes.I8, c.panicFlagGlobal)
		finalIsPanic := c.block.NewICmp(enum.IPredNE, finalFlag, constant.NewInt(irtypes.I8, 0))
		c.block.NewCondBr(finalIsPanic, testPanicExitBlk, finalSuspBlk)
	}

	// --- Panic exit block ---
	// Copy panic msg+type to test harness globals (for batch test main to read/free),
	// clear G struct panic fields (so goroutine_exit doesn't double-free — T0275),
	// clear TLS state, then branch to final suspend.
	{
		// Copy msg and type to test harness globals
		pePanicMsg := testPanicExitBlk.NewLoad(irtypes.I8Ptr, c.panicMsgTlsGlobal)
		testPanicExitBlk.NewStore(pePanicMsg, c.testPanicMsgGlobal)
		pePanicType := testPanicExitBlk.NewLoad(irtypes.I8, c.panicTypeTlsGlobal)
		testPanicExitBlk.NewStore(pePanicType, c.testPanicTypeGlobal)

		// Clear G struct so goroutine_exit doesn't free (test harness owns the msg now)
		gTy := goroutineStructType()
		peCurrentG := testPanicExitBlk.NewLoad(irtypes.I8Ptr, c.currentGGlobal)
		peGPtr := testPanicExitBlk.NewBitCast(peCurrentG, irtypes.NewPointer(gTy))

		pePanickedField := testPanicExitBlk.NewGetElementPtr(gTy, peGPtr,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(gFieldPanicked)))
		testPanicExitBlk.NewStore(constant.NewInt(irtypes.I8, 0), pePanickedField)

		pePanicMsgField := testPanicExitBlk.NewGetElementPtr(gTy, peGPtr,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(gFieldPanicMsg)))
		testPanicExitBlk.NewStore(constant.NewNull(irtypes.I8Ptr), pePanicMsgField)

		// Clear TLS
		testPanicExitBlk.NewStore(constant.NewInt(irtypes.I8, 0), c.panicFlagGlobal)
		testPanicExitBlk.NewStore(constant.NewNull(irtypes.I8Ptr), c.panicMsgTlsGlobal)
		testPanicExitBlk.NewStore(constant.NewInt(irtypes.I8, 0), c.panicTypeTlsGlobal)

		testPanicExitBlk.NewBr(finalSuspBlk)
	}

	// --- Cleanup: free coroutine memory (only reached via destroy path) ---
	coroMem := cleanupBlk.NewCall(c.coroFree, coroId, hdl)
	needFree := cleanupBlk.NewICmp(enum.IPredNE, coroMem, constant.NewNull(irtypes.I8Ptr))
	freeBlk := coroFn.NewBlock("coro.free")
	cleanupBlk.NewCondBr(needFree, freeBlk, doneBlk)

	freeBlk.NewCall(c.palFree, coroMem)
	freeBlk.NewBr(doneBlk)

	// Done: single coro.end
	doneBlk.NewCall(c.coroEnd, hdl, constant.False, constant.None)
	doneBlk.NewRet(hdl)

	// Final suspend switch
	finalResult := finalSuspBlk.NewCall(c.coroSuspend, constant.None, constant.True)
	finalSuspBlk.NewSwitch(finalResult, doneBlk,
		ir.NewCase(constant.NewInt(irtypes.I8, 0), doneBlk),
		ir.NewCase(constant.NewInt(irtypes.I8, 1), cleanupBlk))

	// --- Restore compiler state ---
	c.restoreState(saved)
	c.inCoroutine = savedInCoroutine
	c.coroCleanupBlk = savedCoroCleanup
	c.coroSuspendBlk = savedCoroSuspend
	c.panicExitBlock = savedPanicExitBlock
	c.goExprFireAndForget = savedGoExprFF
	c.coroutineReturnBlock = savedCoroutineReturnBlock

	return coroFn
}

// compilerState captures the mutable compiler fields that defineMethodFunc overwrites.
// Used to save/restore state when synthesizing default methods during another function's codegen.
type compilerState struct {
	fn                   *ir.Func
	block                *ir.Block
	entryBlock           *ir.Block
	locals               map[string]*ir.InstAlloca
	localNameCount       map[string]int
	dropFlags            map[string]*ir.InstAlloca
	castSubjectMatch     map[string]value.Value    // T0849: function-scoped, like dropFlags
	forInHandleSlotPtr   map[string]*ir.InstAlloca // T0617
	dropBindings         map[string]scopeBinding
	blockCounter         int
	canError             bool
	currentRetType       types.Type
	currentNamed         *types.Named
	scopeBindings        []scopeBinding
	loopScopeDepth       int
	loopTempFloor        [4]int // T1331
	selfSubst            *selfSubstInfo
	targetType           types.Type
	typeSubst            map[*types.TypeParam]types.Type
	monoCtx              *monoContext
	lambdaWritebacks     []lambdaWriteback
	stmtTemps            []stmtTemp
	stmtTempMap          map[value.Value]int
	mergeBoundStructFlag map[value.Value]*ir.InstAlloca // T1211
	heapTemps            []heapTemp
	heapTempMap          map[value.Value]int
	envTemps             []envTemp
	envTempMap           map[value.Value]int
	enumCtorTemps        []enumCtorTemp // B0267
	blockTempFloorStmt   int            // T1329
	blockTempFloorHeap   int            // T1329
	blockTempFloorEnv    int            // T1329
	blockTempFloorEnum   int            // T1329
	tempTrackingEnabled  bool
	panicExitBlock       *ir.Block       // T0262: prevent cross-function block references
	coroutineReturnBlock *ir.Block       // T0262: prevent cross-function block references
	thisRecvIsOwned      bool            // T0428: true when current method has ~this receiver
	currentOpValueParams map[string]bool // T0897: borrowed value params of the current operator
	borrowedValueParams  map[string]bool // T0945: borrowed value params of the current function/method
	discardedExpr        ast.Expr        // T1029
	discardAliasArgPtrs  []value.Value   // T1029
}

func (c *Compiler) saveState() compilerState {
	s := compilerState{
		fn:                   c.fn,
		block:                c.block,
		entryBlock:           c.entryBlock,
		locals:               c.locals,
		localNameCount:       c.localNameCount,
		dropFlags:            c.dropFlags,
		castSubjectMatch:     c.castSubjectMatch,   // T0849
		forInHandleSlotPtr:   c.forInHandleSlotPtr, // T0617
		dropBindings:         c.dropBindings,
		blockCounter:         c.blockCounter,
		canError:             c.canError,
		currentRetType:       c.currentRetType,
		currentNamed:         c.currentNamed,
		scopeBindings:        c.scopeBindings,
		loopScopeDepth:       c.loopScopeDepth,
		loopTempFloor:        c.loopTempFloor, // T1331
		selfSubst:            c.selfSubst,
		targetType:           c.targetType,
		typeSubst:            c.typeSubst,
		monoCtx:              c.monoCtx,
		lambdaWritebacks:     c.lambdaWritebacks,
		stmtTemps:            c.stmtTemps,
		stmtTempMap:          c.stmtTempMap,
		mergeBoundStructFlag: c.mergeBoundStructFlag, // T1211
		heapTemps:            c.heapTemps,
		heapTempMap:          c.heapTempMap,
		envTemps:             c.envTemps,
		envTempMap:           c.envTempMap,
		enumCtorTemps:        c.enumCtorTemps,      // B0267
		blockTempFloorStmt:   c.blockTempFloorStmt, // T1329
		blockTempFloorHeap:   c.blockTempFloorHeap, // T1329
		blockTempFloorEnv:    c.blockTempFloorEnv,  // T1329
		blockTempFloorEnum:   c.blockTempFloorEnum, // T1329
		tempTrackingEnabled:  c.tempTrackingEnabled,
		panicExitBlock:       c.panicExitBlock,
		coroutineReturnBlock: c.coroutineReturnBlock,
		thisRecvIsOwned:      c.thisRecvIsOwned,
		currentOpValueParams: c.currentOpValueParams,
		borrowedValueParams:  c.borrowedValueParams,
		discardedExpr:        c.discardedExpr,       // T1029
		discardAliasArgPtrs:  c.discardAliasArgPtrs, // T1029
	}
	// T1029: a nested function/coroutine body is not the discarded statement; do not
	// inherit the outer's discarded-expr marker. restoreState brings it back.
	c.discardedExpr = nil
	c.discardAliasArgPtrs = nil
	// T0262: clear coroutine-specific blocks to prevent cross-function references
	// when saveState is used before switching c.fn to a different LLVM function.
	c.panicExitBlock = nil
	c.coroutineReturnBlock = nil
	// T0945: a nested function body must not inherit the outer's borrowed value
	// params (an elvis there owns/borrows by its own param list). Define sites
	// repopulate it; restoreState brings the outer's set back afterward.
	c.borrowedValueParams = nil
	// T1329: a nested function/coroutine body defined inside a block-value body is
	// a separate function; its statement boundaries must drain from 0, not inherit
	// the outer block's sibling-temp floor. restoreState brings the floors back.
	c.blockTempFloorStmt = 0
	c.blockTempFloorHeap = 0
	c.blockTempFloorEnv = 0
	c.blockTempFloorEnum = 0
	// T1331: a nested function/coroutine body has no enclosing loop of its own;
	// its break/continue drains from 0 until its own loops set a floor.
	c.loopTempFloor = [4]int{}
	return s
}

func (c *Compiler) restoreState(s compilerState) {
	c.fn = s.fn
	c.block = s.block
	c.entryBlock = s.entryBlock
	c.locals = s.locals
	c.localNameCount = s.localNameCount
	c.dropFlags = s.dropFlags
	c.castSubjectMatch = s.castSubjectMatch     // T0849
	c.forInHandleSlotPtr = s.forInHandleSlotPtr // T0617
	c.dropBindings = s.dropBindings
	c.stmtTemps = s.stmtTemps
	c.stmtTempMap = s.stmtTempMap
	c.mergeBoundStructFlag = s.mergeBoundStructFlag // T1211
	c.heapTemps = s.heapTemps
	c.heapTempMap = s.heapTempMap
	c.envTemps = s.envTemps
	c.envTempMap = s.envTempMap
	c.enumCtorTemps = s.enumCtorTemps           // B0267
	c.blockTempFloorStmt = s.blockTempFloorStmt // T1329
	c.blockTempFloorHeap = s.blockTempFloorHeap // T1329
	c.blockTempFloorEnv = s.blockTempFloorEnv   // T1329
	c.blockTempFloorEnum = s.blockTempFloorEnum // T1329
	c.tempTrackingEnabled = s.tempTrackingEnabled
	c.blockCounter = s.blockCounter
	c.canError = s.canError
	c.currentRetType = s.currentRetType
	c.currentNamed = s.currentNamed
	c.scopeBindings = s.scopeBindings
	c.loopScopeDepth = s.loopScopeDepth
	c.loopTempFloor = s.loopTempFloor // T1331
	c.selfSubst = s.selfSubst
	c.targetType = s.targetType
	c.typeSubst = s.typeSubst
	c.monoCtx = s.monoCtx
	c.lambdaWritebacks = s.lambdaWritebacks
	c.panicExitBlock = s.panicExitBlock
	c.coroutineReturnBlock = s.coroutineReturnBlock
	c.thisRecvIsOwned = s.thisRecvIsOwned
	c.currentOpValueParams = s.currentOpValueParams
	c.borrowedValueParams = s.borrowedValueParams
	c.discardedExpr = s.discardedExpr             // T1029
	c.discardAliasArgPtrs = s.discardAliasArgPtrs // T1029
}

// resetBlockTempFloors zeroes the block-value temp floors for a nested function
// body (lambda / coroutine / generator) whose temp-tracking arrays are reset
// fresh, returning the outer floors so the caller can restore them on exit.
// saveState/restoreState already do this; these are for the manual save/restore
// paths (genLambdaExpr, go-block coroutines) that don't route through saveState.
// Without the reset a nested body defined inside a block-value body would inherit
// a stale, non-zero floor and skip draining its own statement-boundary temps
// (T1329).
func (c *Compiler) resetBlockTempFloors() [4]int {
	saved := [4]int{c.blockTempFloorStmt, c.blockTempFloorHeap, c.blockTempFloorEnv, c.blockTempFloorEnum}
	c.blockTempFloorStmt, c.blockTempFloorHeap, c.blockTempFloorEnv, c.blockTempFloorEnum = 0, 0, 0, 0
	return saved
}

// restoreBlockTempFloors restores the floors saved by resetBlockTempFloors (T1329).
func (c *Compiler) restoreBlockTempFloors(saved [4]int) {
	c.blockTempFloorStmt, c.blockTempFloorHeap = saved[0], saved[1]
	c.blockTempFloorEnv, c.blockTempFloorEnum = saved[2], saved[3]
}

// findTypeDeclAnyFile searches for a TypeDecl by name in c.file first,
// then in all loaded module files. Returns the decl and (if found in a module)
// the module's sema.Info so callers can switch context for body generation.
func (c *Compiler) findTypeDeclAnyFile(name string) (*ast.TypeDecl, *sema.Info) {
	td, _, info := c.findTypeDeclAnyFileWithFile(name)
	return td, info
}

// findTypeDeclAnyFileWithFile is like findTypeDeclAnyFile but also returns the
// *ast.File the TypeDecl was found in. Callers that must feed a module file into
// another file-parameterized helper (e.g. declareStructuralDefaultStubs) need the
// file, not just the sema info. The returned file is c.file when the type is in
// the current file (with a nil Info meaning "use current context").
func (c *Compiler) findTypeDeclAnyFileWithFile(name string) (*ast.TypeDecl, *ast.File, *sema.Info) {
	if td := c.findTypeDecl(c.file, name); td != nil {
		return td, c.file, nil // nil Info means "use current context"
	}
	// Always search from rootInfo (original user sema info) so that module lookups
	// work correctly even when c.info has been temporarily swapped during synthesis.
	root := c.rootInfo
	if root == nil {
		root = c.info
	}
	if root != nil {
		for _, modInfo := range root.ModuleInfos {
			if modInfo.File != nil {
				if td := c.findTypeDecl(modInfo.File, name); td != nil {
					return td, modInfo.File, modInfo.SemaInfo
				}
			}
		}
	}
	return nil, nil, nil
}
