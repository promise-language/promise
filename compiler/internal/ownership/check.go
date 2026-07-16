package ownership

import (
	"github.com/promise-language/promise/compiler/internal/ast"
	"github.com/promise-language/promise/compiler/internal/sema"
	"github.com/promise-language/promise/compiler/internal/types"
)

// Checker performs ownership analysis on a type-checked AST.
type Checker struct {
	file     *ast.File
	info     *sema.Info
	errors   []error
	state    StateMap
	borrows  *BorrowSet       // active borrow tracking
	params   map[string]bool  // parameter names for current function
	curSig   *types.Signature // current function signature (for return checks)
	inUnsafe int
	pinned   map[string]bool // use-bound variables that cannot be moved

	// T0652: for-in loop binding names whose iterable is a native container
	// (Vector/Array/Map-value) of single-owner native handles (Mutex/MutexGuard/
	// Task). Moves of such bindings (`x := h`, `foo(h)`, `use x := h`, `return h`)
	// are rejected by tryMove/tryMoveConsume because the binding aliases the
	// container's slot — moving it leaves the container with a dangling pointer
	// that double-frees / hangs at scope-exit container drop. `<-h` direct
	// receive is unaffected (UnaryExpr does not go through tryMove; T0617
	// codegen already nulls the slot).
	forInSingleOwnerBindings map[string]bool

	// T0971/T0978: for-in loop binding names whose iterable is a native container
	// (Vector/Array/Map-value) of non-Copy, non-string elements — whether owned,
	// a plain-borrow parameter (`T[] src`), or a borrowed ref (`T[]&`/`T[]~`/
	// `.borrow`). The loop binding aliases the container's element storage, which
	// its owner still drops at scope exit, so moving the binding out
	// (`sink.push(x)`, `y := x`, `return x`, passing to a `~` param) would
	// double-free. Copy elements are value copies and string elements are cloned
	// per iteration (genForInVector dupStrings), so both stay freely movable and
	// are not flagged here. Single-owner native handles use the separate
	// forInSingleOwnerBindings set (T0652) so they keep their dedicated message.
	forInAliasBindings map[string]bool

	// T1147: for-in loop binding names that are OWNED per-iteration droppable
	// values (string elements; owned values yielded by iterators/channels/
	// generators) — NOT aliasing-into-a-container. Borrowing such a binding into
	// a `go` call lets the borrow escape into a goroutine that can outlive the
	// loop iteration → use-after-free. Disjoint from forInAliasBindings/
	// forInSingleOwnerBindings (those alias container storage and are sound).
	// T1151: this set also holds owned non-copy droppable locals declared *within*
	// a loop body (`for … { string y = …; go f(y); }`), added by the var-decl
	// handlers via flagLoopBodyOwnedLocal when loopDepth ≥ 1 — they have the same
	// iteration-bounded scope as a for-in binding. The whole-map snapshot taken in
	// enterLoopBody / restored in exitLoopBody removes such body locals at loop exit.
	forInOwnedDroppableBindings map[string]bool

	// Drop ordering: tracks declaration order for LIFO drop-order validation.
	// Variables are dropped in reverse declaration order at scope exit.
	// If a borrower is declared before its origin, the origin is dropped first
	// (LIFO), creating a dangling borrow during the borrower's remaining lifetime.
	declOrder map[string]int        // variable name → declaration order (0-based)
	nextOrder int                   // next declaration order to assign
	varTypes  map[string]types.Type // variable name → type (for drop check)

	// NLL borrow narrowing (T0164): maps statement → ref variable names whose
	// last use is that statement. After processing such a statement, borrows
	// held by those variables are expired.
	refLastUses map[ast.Stmt][]string

	// Lifetime analysis (B0033): tracks which parameter names appear as return
	// origins across the function body, for ambiguity detection (rule 4).
	returnOrigins map[string]ast.Pos

	// loopDepth is the current loop-body nesting depth; loop-body var-decls of
	// owned droppable locals at depth ≥ 1 are iteration-bounded, so borrowing
	// them into a `go f(arg)` call is rejected like a for-in binding (T1151).
	// Reset to 0 inside lambda / go-block bodies (those frames own their locals).
	loopDepth int

	// T1152: handle-var name → borrowed owned-droppable-local name. A
	// `t := go f(s)` handle borrows `s` (a function/block-scope local); the
	// goroutine may read `s` after it drops, so the handle must not escape `s`'s
	// scope. Entries make tryMove/tryMoveConsume reject the handle (or the inline
	// `go` temporary) at every consume/store/return site. This is the sibling of
	// the iteration-bounded for-in (T1147) and loop-body-local (T1151) rejections:
	// those bindings can never be safely borrowed into a `go` call (the goroutine
	// always outlives the iteration) and are rejected outright at the call site by
	// rejectGoCallLoopBindingBorrowEscape; a function-level local's borrow is sound
	// while the handle is awaited/dropped in scope and unsound only if the handle
	// escapes, which is what this map tracks.
	goHandleBorrowedLocal map[string]string

	// T1212: locals produced by a Some-wrap coercion of a BORROWED single-owner
	// handle (`Mutex[int]?? x = m` with m: Mutex[int]? borrowed). The outer
	// Optional is genuinely Owned (in-scope drop is sound — codegen clears the
	// inner alias's drop flag), but the inner handle is still borrowed, so an
	// ESCAPE (return/store/call-arg/send) aliases the caller's live handle and
	// double-frees (dupOptionalVectorElem has no clone). Sibling of
	// goHandleBorrowedLocal — same escape-only rejection model. Maps local name →
	// {source binding, handle kind} for the diagnostic.
	wrapCoercedHandleLocal map[string]wrapCoercedHandle

	// T1035: the generic function / method currently being checked (exactly one
	// is non-nil inside a body, both nil at file scope). Used to key deferred
	// for-in-drain requirements (funcDrainReqs/methodDrainReqs) so they can be
	// validated per concrete instantiation via the existing GenericCallEdges.
	curFuncObj   *types.Func
	curMethodObj *types.Method

	// forInTypeParamAliasBindings maps a for-in loop binding name to the bare
	// element *types.TypeParam it aliases. A bare-TypeParam element can't be
	// classified copy/non-copy in the generic body (the ownership pass checks
	// each body once with `T` unbound), so moving such a binding out is NOT
	// rejected inline — instead a drainReq is recorded and validated at each
	// concrete call site. Reset per-function with the same save/restore
	// discipline as forInAliasBindings. (T1035)
	forInTypeParamAliasBindings map[string]*types.TypeParam

	// guardMutexRoot maps a local MutexGuard variable name to the name of the
	// local Mutex it borrows (the receiver root of its `.lock()` call, or the
	// root inherited through a guard-to-guard alias `g2 := g`). Used at
	// container-store sites (T0665) to reject storing a guard into a container
	// declared before its Mutex: the container outlives the Mutex, so the
	// guard's scope-exit drop would unlock an already-destroyed Mutex (UAF).
	// Reset per-function with the same discipline as declOrder/varTypes.
	guardMutexRoot map[string]string

	// funcDrainReqs / methodDrainReqs accumulate deferred for-in-drain
	// requirements across the whole file (NOT reset per-function — initialized
	// once in Check). Each records that a generic body moves a for-in binding
	// over a bare-TypeParam container element out; propagateDrainReqs validates
	// them against concrete instantiations. (T1035)
	funcDrainReqs   map[*types.Func][]drainReq
	methodDrainReqs map[*types.Method][]drainReq

	// funcReturnHandleReqs / methodReturnHandleReqs accumulate deferred
	// return-borrowed-handle requirements across the whole file (NOT reset
	// per-function — initialized once in Check). Each records that a generic body
	// directly returns a borrowed param whose type is still a TypeParam;
	// propagateReturnHandleReqs validates them against concrete instantiations,
	// rejecting Optional-wrapped single-owner handles. (T1213)
	funcReturnHandleReqs   map[*types.Func][]returnHandleReq
	methodReturnHandleReqs map[*types.Method][]returnHandleReq

	// T1137: bare single-owner-handle args passed by borrow to a (possibly
	// generic) call that may return them. Keyed by call position (matches the
	// sema GenericCallEdge.CallPos). Each candidate's `reused` is flipped true
	// when its source local is used again after the call, within the same body;
	// propagateReturnHandleReqs then rejects only the reuse-after-alias case for
	// a callee whose returnHandleReq.Binding names that exact param. Persistent
	// across the whole file (init once in Check) — call positions are unique.
	aliasHandleReuses map[ast.Pos][]*aliasHandleReuse
	// pendingAliasLocals maps a caller-side local name to its live reuse
	// candidates for the current function body; the entries are the SAME
	// pointers stored in aliasHandleReuses (flipping `reused` here mutates
	// both). Reset per function/method body. T1255 scopes this map across
	// mutually-exclusive branches (clone before an alternative, union at the
	// merge) so a use in one branch cannot flip a candidate recorded in a
	// sibling branch, while a fall-through use still flips candidates from any
	// alternative.
	pendingAliasLocals map[string][]*aliasHandleReuse

	// loopFrames is a stack of per-loop-body frames used to detect loop-back-edge
	// reuse of an aliased single-owner handle (T1255). A candidate recorded inside
	// a loop body is flagged `reused` at loop exit unless its source local is
	// freshly rebound at the top level of that body (a dominating rebind yields a
	// fresh handle each iteration). This catches the silent-UAF loop case that the
	// flow-insensitive textual flip in checkIdentUse misses (the only textual use
	// of the source is the call arg itself, which is not a "later use"). Reset per
	// function/method body.
	loopFrames []*aliasLoopFrame
}

// wrapCoercedHandle records the provenance of a T1212 wrap-coerced borrowed
// single-owner handle local: `source` is the borrowed binding it aliases, and
// `kind` is its handle display name ("task"/"Mutex"/"MutexGuard").
type wrapCoercedHandle struct{ source, kind string }

// paramInitialState returns the initial ownership state for a function or
// method parameter. Non-`~` non-`&` non-Copy parameters (and plain `this`
// receivers) are `Borrowed`: reads stay legal, but the callee cannot move
// the value out (e.g., into a constructor field or a `~` callee). This
// matches the codegen contract — the caller's drop flag is only cleared at
// the call site for `~` arguments, so a callee-side move on a borrowed
// parameter would create a double-free (T0338).
//
// Setters consume their value parameter implicitly (codegen clears the
// caller's drop flag at the property assignment site), so setter params
// are treated as `Owned`.
func paramInitialState(p *types.Param, consuming bool) VarState {
	if isCopyType(p.Type()) {
		return Owned
	}
	if p.Ref() == types.RefMut {
		// `~T` param — caller transfers ownership; callee may consume.
		// `~this` receivers also land here (Owned for mutation tracking), but
		// consumption is still rejected by tryMove/tryMoveConsume (T0569/T0593).
		return Owned
	}
	if p.IsVariadic() {
		// Variadic params receive a synthesized vector that the callee owns
		// (the caller-side T[] is consumed at the call site, or a fresh one
		// is built from the variadic args). The callee may consume it.
		return Owned
	}
	if consuming {
		// Setter value parameter: caller's drop flag is cleared at the
		// property assignment site, so the callee owns the value.
		return Owned
	}
	return Borrowed
}

// newChecker builds a Checker with all persistent (non-per-function) maps
// initialized. Shared by Check and the T1303 cross-module pass, which builds a
// sub-checker over an imported module's file.
func newChecker(file *ast.File, info *sema.Info) *Checker {
	return &Checker{
		file:                   file,
		info:                   info,
		refLastUses:            AnalyzeRefLastUses(file, info), // T0164: NLL borrow narrowing
		funcDrainReqs:          make(map[*types.Func][]drainReq),
		methodDrainReqs:        make(map[*types.Method][]drainReq),
		funcReturnHandleReqs:   make(map[*types.Func][]returnHandleReq),
		methodReturnHandleReqs: make(map[*types.Method][]returnHandleReq),
		aliasHandleReuses:      make(map[ast.Pos][]*aliasHandleReuse), // T1137
	}
}

// Check performs ownership analysis on the given file using sema results.
// Returns any ownership errors found. Also populates info.EarlyDrops with
// NLL last-use analysis results for early drop insertion in codegen (B0035).
func Check(file *ast.File, info *sema.Info) []error {
	c := newChecker(file, info)
	c.check()
	// T1035: validate deferred for-in-drain requirements against concrete
	// instantiations (appends to c.errors). Runs after the full body pass so
	// every generic body's drainReqs are recorded first.
	c.propagateDrainReqs()
	// T1213: validate deferred return-borrowed-handle requirements against
	// concrete instantiations. Also runs after the full body pass so every
	// generic body's returnHandleReqs are recorded first.
	c.propagateReturnHandleReqs()
	// T1303: re-check imported generic module type bodies against this unit's
	// concrete instantiations so a field-escape that double-frees for a
	// drop-bearing instantiation (visible only here, not in the module unit)
	// is rejected.
	c.checkImportedGenericBodies()
	// B0035: Run NLL last-use analysis after ownership check.
	info.EarlyDrops = AnalyzeLastUses(file, info)
	return c.errors
}

func (c *Checker) check() {
	for _, decl := range c.file.Decls {
		switch d := decl.(type) {
		case *ast.FuncDecl:
			c.checkFuncDecl(d)
		case *ast.TypeDecl:
			c.checkTypeDecl(d)
		case *ast.EnumDecl:
			c.checkEnumDecl(d)
		}
	}
}

func (c *Checker) checkFuncDecl(d *ast.FuncDecl) {
	if d.Body == nil {
		return
	}
	obj := c.lookupFileScope(d.Name)
	if obj == nil {
		return
	}
	fn, ok := obj.(*types.Func)
	if !ok {
		return
	}
	sig, ok := fn.Type().(*types.Signature)
	if !ok || sig == nil {
		return
	}

	savedState := c.state
	savedBorrows := c.borrows
	savedParams := c.params
	savedSig := c.curSig
	savedPinned := c.pinned
	savedForInSingleOwner := c.forInSingleOwnerBindings
	savedForInAlias := c.forInAliasBindings
	savedForInTypeParamAlias := c.forInTypeParamAliasBindings
	savedForInOwnedDroppable := c.forInOwnedDroppableBindings
	savedDeclOrder := c.declOrder
	savedNextOrder := c.nextOrder
	savedVarTypes := c.varTypes
	savedReturnOrigins := c.returnOrigins
	savedLoopDepth := c.loopDepth
	savedGoHandleBorrowed := c.goHandleBorrowedLocal
	savedWrapCoercedHandle := c.wrapCoercedHandleLocal
	savedGuardMutexRoot := c.guardMutexRoot
	savedFuncObj := c.curFuncObj
	savedMethodObj := c.curMethodObj

	c.state = make(StateMap)
	c.borrows = NewBorrowSet()
	c.params = make(map[string]bool)
	c.curSig = sig
	c.pinned = make(map[string]bool)
	c.forInSingleOwnerBindings = make(map[string]bool)
	c.forInAliasBindings = make(map[string]bool)
	c.forInTypeParamAliasBindings = make(map[string]*types.TypeParam)
	c.forInOwnedDroppableBindings = make(map[string]bool)
	c.declOrder = make(map[string]int)
	c.nextOrder = 0
	c.varTypes = make(map[string]types.Type)
	c.returnOrigins = nil
	c.loopDepth = 0
	c.goHandleBorrowedLocal = make(map[string]string)
	c.wrapCoercedHandleLocal = make(map[string]wrapCoercedHandle)
	c.guardMutexRoot = make(map[string]string)
	c.pendingAliasLocals = make(map[string][]*aliasHandleReuse) // T1137
	c.loopFrames = nil                                          // T1255
	c.curFuncObj = fn
	c.curMethodObj = nil

	consuming := d.IsSetter
	for _, p := range sig.Params() {
		if p.Name() != "" && p.Name() != "_" {
			c.state[p.Name()] = paramInitialState(p, consuming)
			c.params[p.Name()] = true
			c.trackDeclOrder(p.Name(), p.Type())
		}
	}

	c.checkBlock(d.Body)
	c.checkDropOrderSafety()
	c.checkReturnAmbiguity()

	c.state = savedState
	c.borrows = savedBorrows
	c.params = savedParams
	c.pinned = savedPinned
	c.forInSingleOwnerBindings = savedForInSingleOwner
	c.forInAliasBindings = savedForInAlias
	c.forInTypeParamAliasBindings = savedForInTypeParamAlias
	c.forInOwnedDroppableBindings = savedForInOwnedDroppable
	c.curSig = savedSig
	c.declOrder = savedDeclOrder
	c.nextOrder = savedNextOrder
	c.returnOrigins = savedReturnOrigins
	c.varTypes = savedVarTypes
	c.loopDepth = savedLoopDepth
	c.goHandleBorrowedLocal = savedGoHandleBorrowed
	c.wrapCoercedHandleLocal = savedWrapCoercedHandle
	c.guardMutexRoot = savedGuardMutexRoot
	c.curFuncObj = savedFuncObj
	c.curMethodObj = savedMethodObj
}

func (c *Checker) checkTypeDecl(d *ast.TypeDecl) {
	obj := c.lookupFileScope(d.Name)
	if obj == nil {
		return
	}
	tn, ok := obj.(*types.TypeName)
	if !ok {
		return
	}
	named, ok := tn.Type().(*types.Named)
	if !ok {
		return
	}

	for _, md := range d.Methods {
		if md.Body == nil {
			continue
		}
		var m *types.Method
		if md.IsGetter {
			m = named.LookupGetter(md.Name)
		} else if md.IsSetter {
			m = named.LookupSetter(md.Name)
		} else {
			m = named.LookupMethod(md.Name)
		}
		if m == nil || m.Sig() == nil {
			continue
		}
		c.checkMethodBody(md, m)
	}
}

func (c *Checker) checkEnumDecl(d *ast.EnumDecl) {
	obj := c.lookupFileScope(d.Name)
	if obj == nil {
		return
	}
	tn, ok := obj.(*types.TypeName)
	if !ok {
		return
	}
	enum, ok := tn.Type().(*types.Enum)
	if !ok {
		return
	}

	for _, md := range d.Methods {
		if md.Body == nil {
			continue
		}
		var m *types.Method
		if md.IsGetter {
			m = enum.LookupGetter(md.Name)
		} else {
			m = enum.LookupMethod(md.Name)
		}
		if m == nil || m.Sig() == nil {
			continue
		}
		c.checkMethodBody(md, m)
	}
}

func (c *Checker) checkMethodBody(md *ast.MethodDecl, m *types.Method) {
	savedState := c.state
	savedBorrows := c.borrows
	savedParams := c.params
	savedSig := c.curSig
	savedPinned := c.pinned
	savedForInSingleOwner := c.forInSingleOwnerBindings
	savedForInAlias := c.forInAliasBindings
	savedForInTypeParamAlias := c.forInTypeParamAliasBindings
	savedForInOwnedDroppable := c.forInOwnedDroppableBindings
	savedDeclOrder := c.declOrder
	savedNextOrder := c.nextOrder
	savedVarTypes := c.varTypes
	savedReturnOrigins := c.returnOrigins
	savedLoopDepth := c.loopDepth
	savedGoHandleBorrowed := c.goHandleBorrowedLocal
	savedWrapCoercedHandle := c.wrapCoercedHandleLocal
	savedGuardMutexRoot := c.guardMutexRoot
	savedFuncObj := c.curFuncObj
	savedMethodObj := c.curMethodObj

	c.state = make(StateMap)
	c.borrows = NewBorrowSet()
	c.params = make(map[string]bool)
	c.curSig = m.Sig()
	c.pinned = make(map[string]bool)
	c.forInSingleOwnerBindings = make(map[string]bool)
	c.forInAliasBindings = make(map[string]bool)
	c.forInTypeParamAliasBindings = make(map[string]*types.TypeParam)
	c.forInOwnedDroppableBindings = make(map[string]bool)
	c.declOrder = make(map[string]int)
	c.nextOrder = 0
	c.varTypes = make(map[string]types.Type)
	c.returnOrigins = nil
	c.loopDepth = 0
	c.goHandleBorrowedLocal = make(map[string]string)
	c.wrapCoercedHandleLocal = make(map[string]wrapCoercedHandle)
	c.guardMutexRoot = make(map[string]string)
	c.pendingAliasLocals = make(map[string][]*aliasHandleReuse) // T1137
	c.loopFrames = nil                                          // T1255
	c.curFuncObj = nil
	c.curMethodObj = m

	if m.Sig().Recv() != nil {
		c.state["this"] = paramInitialState(m.Sig().Recv(), false)
		c.params["this"] = true
		if m.Sig().Recv().Type() != nil {
			c.trackDeclOrder("this", m.Sig().Recv().Type())
		}
	}
	consuming := md.IsSetter
	for _, p := range m.Sig().Params() {
		if p.Name() != "" && p.Name() != "_" {
			c.state[p.Name()] = paramInitialState(p, consuming)
			c.params[p.Name()] = true
			c.trackDeclOrder(p.Name(), p.Type())
		}
	}

	c.checkBlock(md.Body)
	c.checkDropOrderSafety()
	c.checkReturnAmbiguity()

	c.state = savedState
	c.borrows = savedBorrows
	c.params = savedParams
	c.curSig = savedSig
	c.pinned = savedPinned
	c.forInSingleOwnerBindings = savedForInSingleOwner
	c.forInAliasBindings = savedForInAlias
	c.forInTypeParamAliasBindings = savedForInTypeParamAlias
	c.forInOwnedDroppableBindings = savedForInOwnedDroppable
	c.declOrder = savedDeclOrder
	c.nextOrder = savedNextOrder
	c.varTypes = savedVarTypes
	c.returnOrigins = savedReturnOrigins
	c.loopDepth = savedLoopDepth
	c.goHandleBorrowedLocal = savedGoHandleBorrowed
	c.wrapCoercedHandleLocal = savedWrapCoercedHandle
	c.guardMutexRoot = savedGuardMutexRoot
	c.curFuncObj = savedFuncObj
	c.curMethodObj = savedMethodObj
}

// lookupFileScope finds an object in the file-level scope.
func (c *Checker) lookupFileScope(name string) types.Object {
	if fileScope, ok := c.info.Scopes[c.file]; ok {
		return fileScope.Lookup(name)
	}
	return nil
}
