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
}

// Check performs ownership analysis on the given file using sema results.
// Returns any ownership errors found.
func Check(file *ast.File, info *sema.Info) []error {
	c := &Checker{
		file: file,
		info: info,
	}
	c.check()
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

	c.state = make(StateMap)
	c.borrows = NewBorrowSet()
	c.params = make(map[string]bool)
	c.curSig = sig
	c.pinned = make(map[string]bool)

	for _, p := range sig.Params() {
		if p.Name() != "" && p.Name() != "_" {
			c.state[p.Name()] = Owned
			c.params[p.Name()] = true
		}
	}

	c.checkBlock(d.Body)

	c.state = savedState
	c.borrows = savedBorrows
	c.params = savedParams
	c.pinned = savedPinned
	c.curSig = savedSig
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

	c.state = make(StateMap)
	c.borrows = NewBorrowSet()
	c.params = make(map[string]bool)
	c.curSig = m.Sig()
	c.pinned = make(map[string]bool)

	if m.Sig().Recv() != nil {
		c.state["this"] = Owned
		c.params["this"] = true
	}
	for _, p := range m.Sig().Params() {
		if p.Name() != "" && p.Name() != "_" {
			c.state[p.Name()] = Owned
			c.params[p.Name()] = true
		}
	}

	c.checkBlock(md.Body)

	c.state = savedState
	c.borrows = savedBorrows
	c.params = savedParams
	c.curSig = savedSig
	c.pinned = savedPinned
}

// lookupFileScope finds an object in the file-level scope.
func (c *Checker) lookupFileScope(name string) types.Object {
	if fileScope, ok := c.info.Scopes[c.file]; ok {
		return fileScope.Lookup(name)
	}
	return nil
}
