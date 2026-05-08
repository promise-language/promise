package ownership

import (
	"djabi.dev/go/promise_lang/internal/ast"
	"djabi.dev/go/promise_lang/internal/sema"
	"djabi.dev/go/promise_lang/internal/types"
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
	savedDeclOrder := c.declOrder
	savedNextOrder := c.nextOrder
	savedVarTypes := c.varTypes

	c.state = make(StateMap)
	c.borrows = NewBorrowSet()
	c.params = make(map[string]bool)
	c.curSig = sig
	c.pinned = make(map[string]bool)
	c.declOrder = make(map[string]int)
	c.nextOrder = 0
	c.varTypes = make(map[string]types.Type)

	for _, p := range sig.Params() {
		if p.Name() != "" && p.Name() != "_" {
			c.state[p.Name()] = Owned
			c.params[p.Name()] = true
			c.trackDeclOrder(p.Name(), p.Type())
		}
	}

	c.checkBlock(d.Body)
	c.checkDropOrderSafety()

	c.state = savedState
	c.borrows = savedBorrows
	c.params = savedParams
	c.pinned = savedPinned
	c.curSig = savedSig
	c.declOrder = savedDeclOrder
	c.nextOrder = savedNextOrder
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
	savedDeclOrder := c.declOrder
	savedNextOrder := c.nextOrder
	savedVarTypes := c.varTypes

	c.state = make(StateMap)
	c.borrows = NewBorrowSet()
	c.params = make(map[string]bool)
	c.curSig = m.Sig()
	c.pinned = make(map[string]bool)
	c.declOrder = make(map[string]int)
	c.nextOrder = 0
	c.varTypes = make(map[string]types.Type)

	if m.Sig().Recv() != nil {
		c.state["this"] = Owned
		c.params["this"] = true
		if m.Sig().Recv().Type() != nil {
			c.trackDeclOrder("this", m.Sig().Recv().Type())
		}
	}
	for _, p := range m.Sig().Params() {
		if p.Name() != "" && p.Name() != "_" {
			c.state[p.Name()] = Owned
			c.params[p.Name()] = true
			c.trackDeclOrder(p.Name(), p.Type())
		}
	}

	c.checkBlock(md.Body)
	c.checkDropOrderSafety()

	c.state = savedState
	c.borrows = savedBorrows
	c.params = savedParams
	c.curSig = savedSig
	c.pinned = savedPinned
	c.declOrder = savedDeclOrder
	c.nextOrder = savedNextOrder
	c.varTypes = savedVarTypes
}

// lookupFileScope finds an object in the file-level scope.
func (c *Checker) lookupFileScope(name string) types.Object {
	if fileScope, ok := c.info.Scopes[c.file]; ok {
		return fileScope.Lookup(name)
	}
	return nil
}
