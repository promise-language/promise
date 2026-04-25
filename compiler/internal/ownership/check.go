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
	inUnsafe int
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

	saved := c.state
	c.state = make(StateMap)

	for _, p := range sig.Params() {
		if p.Name() != "" && p.Name() != "_" {
			c.state[p.Name()] = Owned
		}
	}

	c.checkBlock(d.Body)
	c.state = saved
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
		m := named.LookupMethod(md.Name)
		if m == nil || m.Sig() == nil {
			continue
		}
		c.checkMethodBody(md, m)
	}
}

func (c *Checker) checkMethodBody(md *ast.MethodDecl, m *types.Method) {
	saved := c.state
	c.state = make(StateMap)

	if m.Sig().Recv() != nil {
		c.state["this"] = Owned
	}
	for _, p := range m.Sig().Params() {
		if p.Name() != "" && p.Name() != "_" {
			c.state[p.Name()] = Owned
		}
	}

	c.checkBlock(md.Body)
	c.state = saved
}

// lookupFileScope finds an object in the file-level scope.
func (c *Checker) lookupFileScope(name string) types.Object {
	if fileScope, ok := c.info.Scopes[c.file]; ok {
		return fileScope.Lookup(name)
	}
	return nil
}
