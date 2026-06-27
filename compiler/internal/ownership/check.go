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
	// T1151 (follow-up): generalize this to owned droppable locals declared
	// *within* a loop body (`for … { string y = …; go f(y); }`), which have the
	// same iteration-bounded scope but a different declaration shape.
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
}

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

// Check performs ownership analysis on the given file using sema results.
// Returns any ownership errors found. Also populates info.EarlyDrops with
// NLL last-use analysis results for early drop insertion in codegen (B0035).
func Check(file *ast.File, info *sema.Info) []error {
	c := &Checker{
		file:        file,
		info:        info,
		refLastUses: AnalyzeRefLastUses(file, info), // T0164: NLL borrow narrowing
	}
	c.check()
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
	savedForInOwnedDroppable := c.forInOwnedDroppableBindings
	savedDeclOrder := c.declOrder
	savedNextOrder := c.nextOrder
	savedVarTypes := c.varTypes
	savedReturnOrigins := c.returnOrigins

	c.state = make(StateMap)
	c.borrows = NewBorrowSet()
	c.params = make(map[string]bool)
	c.curSig = sig
	c.pinned = make(map[string]bool)
	c.forInSingleOwnerBindings = make(map[string]bool)
	c.forInAliasBindings = make(map[string]bool)
	c.forInOwnedDroppableBindings = make(map[string]bool)
	c.declOrder = make(map[string]int)
	c.nextOrder = 0
	c.varTypes = make(map[string]types.Type)
	c.returnOrigins = nil

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
	c.forInOwnedDroppableBindings = savedForInOwnedDroppable
	c.curSig = savedSig
	c.declOrder = savedDeclOrder
	c.nextOrder = savedNextOrder
	c.returnOrigins = savedReturnOrigins
	c.varTypes = savedVarTypes
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
	savedForInOwnedDroppable := c.forInOwnedDroppableBindings
	savedDeclOrder := c.declOrder
	savedNextOrder := c.nextOrder
	savedVarTypes := c.varTypes
	savedReturnOrigins := c.returnOrigins

	c.state = make(StateMap)
	c.borrows = NewBorrowSet()
	c.params = make(map[string]bool)
	c.curSig = m.Sig()
	c.pinned = make(map[string]bool)
	c.forInSingleOwnerBindings = make(map[string]bool)
	c.forInAliasBindings = make(map[string]bool)
	c.forInOwnedDroppableBindings = make(map[string]bool)
	c.declOrder = make(map[string]int)
	c.nextOrder = 0
	c.varTypes = make(map[string]types.Type)
	c.returnOrigins = nil

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
	c.forInOwnedDroppableBindings = savedForInOwnedDroppable
	c.declOrder = savedDeclOrder
	c.nextOrder = savedNextOrder
	c.varTypes = savedVarTypes
	c.returnOrigins = savedReturnOrigins
}

// lookupFileScope finds an object in the file-level scope.
func (c *Checker) lookupFileScope(name string) types.Object {
	if fileScope, ok := c.info.Scopes[c.file]; ok {
		return fileScope.Lookup(name)
	}
	return nil
}
