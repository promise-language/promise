package sema

import (
	"djabi.dev/go/promise_lang/internal/ast"
	"djabi.dev/go/promise_lang/internal/types"
)

// Checker performs semantic analysis on a parsed AST file.
type Checker struct {
	file              *ast.File
	info              *Info
	errors            []error
	stdScope          *types.Scope            // std library scope (child of Universe, parent of fileScope)
	fileScope         *types.Scope            // file-level scope (child of stdScope)
	scope             *types.Scope            // current scope during traversal
	curFunc           *types.Signature        // current function being checked (for return/raise)
	curType           *types.Named            // current type being defined/checked (for Self resolution)
	inNewBody         bool                    // true when checking a new() constructor body
	inFactoryBody     bool                    // true when checking a `factory method body
	factoryLocals     map[string]bool         // variables initialized from constructor calls in factory body
	inLoop            int                     // nesting depth of loop constructs
	lambdaDepth       int                     // nesting depth of lambdas (0 = not in lambda)
	lambdaCaptures    map[string]*CapturedVar // current lambda's captured vars (by name)
	lambdaScope       *types.Scope            // scope at lambda definition site (capture boundary)
	lambdaMove        bool                    // true if current lambda uses `move` keyword
	typeHint          types.Type              // expected type for numeric literal adaptation (propagated through arithmetic)
	inUnaryNeg        bool                    // true when checking operand of unary negation (for signed suffix range check)
	inGenerator       bool                    // true when checking a generator function body
	generatorElemType types.Type              // T from stream[T] or Iterator[T] return type
	yieldFound        bool                    // true if at least one yield seen in current generator func
	modules           []*types.Module         // all modules from use declarations
	moduleScopes      map[string]*types.Scope // pre-loaded module scopes (catalog name or path → scope)
}

// Check performs semantic analysis on the given AST file.
// It returns type information and any semantic errors found.
func Check(file *ast.File) (*Info, []error) {
	return CheckWithModules(file, nil)
}

// CheckWithModules performs semantic analysis with pre-loaded module scopes.
// moduleScopes maps module keys (catalog name or source path) to their exported scopes.
func CheckWithModules(file *ast.File, moduleScopes map[string]*types.Scope) (*Info, []error) {
	c := &Checker{
		moduleScopes: moduleScopes,
		file:         file,
		info: &Info{
			Types:                make(map[ast.Expr]types.Type),
			Objects:              make(map[*ast.IdentExpr]types.Object),
			Scopes:               make(map[ast.Node]*types.Scope),
			FieldDefaults:        make(map[*types.Field]ast.Expr),
			ParamDefaults:        make(map[*types.Param]ast.Expr),
			LambdaCaptures:       make(map[*ast.LambdaExpr][]*CapturedVar),
			OptionalNarrowings:   make(map[*ast.IfStmt]*OptionalNarrowing),
			FailableExprs:        make(map[ast.Expr]bool),
			AutoPropagateExprs:   make(map[ast.Expr]bool),
			FailableDestructures: make(map[*ast.DestructureVarDecl]bool),
			GeneratorFuncs:       make(map[ast.Node]types.Type),
		},
	}

	c.stdScope = types.NewScope(
		types.Universe, tpos(file.Pos()), tpos(file.End()), "std",
	)
	c.fileScope = types.NewScope(
		c.stdScope, tpos(file.Pos()), tpos(file.End()), "file",
	)
	c.scope = c.fileScope
	c.info.Scopes[file] = c.fileScope
	c.info.ScopeOrder = append(c.info.ScopeOrder, c.fileScope)
	c.info.StdScope = c.stdScope

	c.declare(file)              // Pass 1: collect all declarations
	c.define(file)               // Pass 2: resolve types, populate type structures
	c.validateConstructors(file) // Validate: constructor inheritance (after all types defined)
	c.validateBuiltins()         // Validate: .pr files declare all required operators/methods/fields
	c.check(file)                // Pass 3: type-check function/method bodies
	c.checkMissingReturn(file)   // Pass 4: verify non-void functions return

	return c.info, c.errors
}

// DeclareAndDefine runs only the first two sema passes: Declare and Define.
// It resolves type names, fields, methods, and signatures, but does NOT
// type-check function/method bodies or verify return paths.
// Used by `promise doc` which only needs type structure, not body correctness.
func DeclareAndDefine(file *ast.File) (*Info, []error) {
	return DeclareAndDefineWithModules(file, nil)
}

// DeclareAndDefineWithModules runs Declare + Define with pre-loaded module scopes.
func DeclareAndDefineWithModules(file *ast.File, moduleScopes map[string]*types.Scope) (*Info, []error) {
	c := &Checker{
		moduleScopes: moduleScopes,
		file:         file,
		info: &Info{
			Types:                make(map[ast.Expr]types.Type),
			Objects:              make(map[*ast.IdentExpr]types.Object),
			Scopes:               make(map[ast.Node]*types.Scope),
			FieldDefaults:        make(map[*types.Field]ast.Expr),
			ParamDefaults:        make(map[*types.Param]ast.Expr),
			LambdaCaptures:       make(map[*ast.LambdaExpr][]*CapturedVar),
			OptionalNarrowings:   make(map[*ast.IfStmt]*OptionalNarrowing),
			FailableExprs:        make(map[ast.Expr]bool),
			AutoPropagateExprs:   make(map[ast.Expr]bool),
			FailableDestructures: make(map[*ast.DestructureVarDecl]bool),
			GeneratorFuncs:       make(map[ast.Node]types.Type),
		},
	}

	c.stdScope = types.NewScope(
		types.Universe, tpos(file.Pos()), tpos(file.End()), "std",
	)
	c.fileScope = types.NewScope(
		c.stdScope, tpos(file.Pos()), tpos(file.End()), "file",
	)
	c.scope = c.fileScope
	c.info.Scopes[file] = c.fileScope
	c.info.ScopeOrder = append(c.info.ScopeOrder, c.fileScope)
	c.info.StdScope = c.stdScope

	c.declare(file)              // Pass 1: collect all declarations
	c.define(file)               // Pass 2: resolve types, populate type structures
	c.validateConstructors(file) // Validate: constructor inheritance

	return c.info, c.errors
}

// tpos converts an ast.Pos to a types.Pos.
func tpos(p ast.Pos) types.Pos {
	return types.Pos{File: p.File, Line: p.Line, Column: p.Column}
}

// openScope creates a new child scope and makes it the current scope.
func (c *Checker) openScope(node ast.Node, comment string) {
	s := types.NewScope(c.scope, tpos(node.Pos()), tpos(node.End()), comment)
	c.scope = s
	c.info.Scopes[node] = s
	c.info.ScopeOrder = append(c.info.ScopeOrder, s)
}

// closeScope pops back to the parent scope.
func (c *Checker) closeScope() {
	c.scope = c.scope.Parent()
}

// lookup searches for a name in the current scope chain.
func (c *Checker) lookup(name string) types.Object {
	obj, _ := c.scope.LookupParent(name)
	return obj
}

// checkNoShadow reports an error if name shadows a variable in any parent scope.
func (c *Checker) checkNoShadow(name string, pos ast.Pos) {
	if name == "_" {
		return
	}
	for s := c.scope.Parent(); s != nil; s = s.Parent() {
		if obj := s.Lookup(name); obj != nil {
			if _, isVar := obj.(*types.Var); isVar {
				c.errorf(pos, "'%s' shadows declaration at %s", name, obj.Pos())
				return
			}
		}
	}
}

// insert adds an object to the current scope.
// Returns true on success, false and reports error on duplicate.
func (c *Checker) insert(obj types.Object) bool {
	if existing := c.scope.Insert(obj); existing != nil {
		p := obj.Pos()
		c.errorf(ast.Pos{File: p.File, Line: p.Line, Column: p.Column},
			"%s redeclared in this scope (previous at %s)", obj.Name(), existing.Pos())
		return false
	}
	return true
}

// check performs Pass 3: type-check all function and method bodies.
func (c *Checker) check(file *ast.File) {
	for _, decl := range file.Decls {
		// Route scope so std decl bodies resolve names against stdScope
		if isDeclStd(decl) {
			c.scope = c.stdScope
		} else {
			c.scope = c.fileScope
		}

		switch d := decl.(type) {
		case *ast.FuncDecl:
			c.checkFuncDecl(d)
		case *ast.TypeDecl:
			c.checkTypeDecl(d)
		case *ast.EnumDecl:
			c.checkEnumDecl(d)
		}
	}
	c.scope = c.fileScope
}

// checkFuncDecl type-checks a function body.
func (c *Checker) checkFuncDecl(d *ast.FuncDecl) {
	if d.Body == nil {
		return // native or abstract
	}

	obj := c.lookup(d.Name)
	if obj == nil {
		return // error already reported
	}
	fn, ok := obj.(*types.Func)
	if !ok {
		return
	}
	if fn.Type() == nil {
		return // signature couldn't be resolved
	}
	sig, ok := fn.Type().(*types.Signature)
	if !ok || sig == nil {
		return
	}

	saved := c.curFunc
	c.curFunc = sig
	defer func() { c.curFunc = saved }()

	// Open type-param scope if generic (so T resolves during body checking)
	if len(sig.TypeParams()) > 0 {
		c.openScope(d, "typeparams:"+d.Name)
		for _, tp := range sig.TypeParams() {
			c.insert(tp.Obj())
		}
		defer c.closeScope()
	}

	// Detect generator: return type is stream[T]
	savedInGenerator := c.inGenerator
	savedGenElemType := c.generatorElemType
	savedYieldFound := c.yieldFound
	c.inGenerator = false
	c.generatorElemType = nil
	c.yieldFound = false

	if sig.Result() != nil {
		if elem, ok := types.AsStream(sig.Result()); ok {
			c.inGenerator = true
			c.generatorElemType = elem
		}
	}

	if c.inGenerator && sig.CanError() {
		c.errorf(d.Pos(), "generator functions cannot be failable")
	}

	c.openScope(d.Body, "func:"+d.Name)

	// Bind parameters into scope
	for _, p := range sig.Params() {
		if p.Name() != "" && p.Name() != "_" {
			c.checkNoShadow(p.Name(), d.Pos())
			c.insert(types.NewVar(tpos(d.Pos()), p.Name(), p.Type()))
		}
	}

	c.checkBlock(d.Body)
	c.closeScope()

	// Record generator function if yields were found
	if c.inGenerator && c.yieldFound {
		c.info.GeneratorFuncs[d] = c.generatorElemType
	} else if c.inGenerator && !c.yieldFound {
		c.errorf(d.Pos(), "function %s returns stream[%s] but contains no yield statements", d.Name, c.generatorElemType)
	}

	c.inGenerator = savedInGenerator
	c.generatorElemType = savedGenElemType
	c.yieldFound = savedYieldFound
}

// checkTypeDecl type-checks method bodies in a type declaration.
func (c *Checker) checkTypeDecl(d *ast.TypeDecl) {
	obj := c.lookup(d.Name)
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
	savedType := c.curType
	c.curType = named
	defer func() { c.curType = savedType }()

	// For generic types, open type param scope so method bodies can reference T, K, V, etc.
	if len(named.TypeParams()) > 0 {
		c.openScope(d, "typeparams:"+d.Name)
		for _, tp := range named.TypeParams() {
			c.insert(tp.Obj())
		}
		defer c.closeScope()
	}

	// Type-check field default expressions and record them.
	for _, fd := range d.Fields {
		if fd.Default == nil {
			continue
		}
		f := named.LookupField(fd.Name)
		if f == nil {
			continue
		}
		defType := c.checkExpr(fd.Default)
		if defType != nil && f.Type() != nil {
			if !types.AssignableTo(defType, f.Type()) {
				c.errorf(fd.Default.Pos(), "cannot use %s as default for field %s of type %s", defType, fd.Name, f.Type())
			}
		}
		c.info.FieldDefaults[f] = fd.Default
	}

	for _, md := range d.Methods {
		if md.Body == nil {
			continue
		}
		m := lookupMethodByKind(named, md)
		if m == nil || m.Sig() == nil {
			continue
		}
		if md.Name == "new" {
			savedInNew := c.inNewBody
			c.inNewBody = true
			c.checkMethodBody(d.Name, md, m)
			c.inNewBody = savedInNew
		} else if m.IsFactory() {
			savedInFactory := c.inFactoryBody
			savedFactoryLocals := c.factoryLocals
			c.inFactoryBody = true
			c.factoryLocals = make(map[string]bool)
			c.checkMethodBody(d.Name, md, m)
			c.inFactoryBody = savedInFactory
			c.factoryLocals = savedFactoryLocals
		} else {
			c.checkMethodBody(d.Name, md, m)
		}
	}
}

// lookupMethodByKind dispatches to the appropriate typed lookup based on the
// AST method declaration's getter/setter flags, avoiding the same-name collision
// that LookupAnyMethod causes when both a getter and setter share a field name.
func lookupMethodByKind(named *types.Named, md *ast.MethodDecl) *types.Method {
	if md.IsGetter {
		return named.LookupGetter(md.Name)
	}
	if md.IsSetter {
		return named.LookupSetter(md.Name)
	}
	return named.LookupMethod(md.Name)
}

func (c *Checker) checkMethodBody(typeName string, md *ast.MethodDecl, m *types.Method) {
	saved := c.curFunc
	c.curFunc = m.Sig()
	defer func() { c.curFunc = saved }()

	// Detect generator: return type is stream[T]
	savedInGenerator := c.inGenerator
	savedGenElemType := c.generatorElemType
	savedYieldFound := c.yieldFound
	c.inGenerator = false
	c.generatorElemType = nil
	c.yieldFound = false

	if m.Sig().Result() != nil {
		if elem, ok := types.AsStream(m.Sig().Result()); ok {
			c.inGenerator = true
			c.generatorElemType = elem
		}
	}

	if c.inGenerator && m.Sig().CanError() {
		c.errorf(md.Pos(), "generator methods cannot be failable")
	}

	c.openScope(md.Body, "method:"+typeName+"."+md.Name)

	// Bind receiver as "this"
	if m.Sig().Recv() != nil {
		c.insert(types.NewVar(tpos(md.Pos()), "this", m.Sig().Recv().Type()))
	}
	// Bind parameters
	for _, p := range m.Sig().Params() {
		if p.Name() != "" && p.Name() != "_" {
			c.checkNoShadow(p.Name(), md.Pos())
			c.insert(types.NewVar(tpos(md.Pos()), p.Name(), p.Type()))
		}
	}

	c.checkBlock(md.Body)
	c.closeScope()

	// Record generator method if yields were found
	if c.inGenerator && c.yieldFound {
		c.info.GeneratorFuncs[md] = c.generatorElemType
	} else if c.inGenerator && !c.yieldFound {
		c.errorf(md.Pos(), "method %s returns stream[%s] but contains no yield statements", md.Name, c.generatorElemType)
	}

	c.inGenerator = savedInGenerator
	c.generatorElemType = savedGenElemType
	c.yieldFound = savedYieldFound
}

// checkEnumDecl type-checks method bodies in an enum declaration.
func (c *Checker) checkEnumDecl(d *ast.EnumDecl) {
	// Enum methods are not yet supported in the AST
	// This is a placeholder for future implementation
}

// checkBlock type-checks a block of statements.
// Detects unreachable code after statements that always exit (return/raise/break/continue).
func (c *Checker) checkBlock(block *ast.Block) {
	dead := false
	for _, stmt := range block.Stmts {
		if dead {
			c.errorf(stmt.Pos(), "unreachable code")
			break // report once per block
		}
		c.checkStmt(stmt)
		dead = c.stmtAlwaysExits(stmt)
	}
}

// stmtAlwaysExits reports whether a statement guarantees control never falls through.
// Used for unreachable code detection. Broader than stmtReturns (in returns.go)
// because break/continue also prevent fallthrough.
func (c *Checker) stmtAlwaysExits(stmt ast.Stmt) bool {
	switch s := stmt.(type) {
	case *ast.ReturnStmt:
		return true
	case *ast.RaiseStmt:
		return true
	case *ast.BreakStmt:
		return true
	case *ast.ContinueStmt:
		return true
	case *ast.Block:
		if len(s.Stmts) == 0 {
			return false
		}
		return c.stmtAlwaysExits(s.Stmts[len(s.Stmts)-1])
	case *ast.IfStmt:
		if s.Else == nil {
			return false
		}
		return c.blockAlwaysExits(s.Body) && c.stmtAlwaysExits(s.Else)
	case *ast.ExprStmt:
		if me, ok := s.Expr.(*ast.MatchExpr); ok {
			for _, arm := range me.Arms {
				if arm.Block == nil || !c.blockAlwaysExits(arm.Block) {
					return false
				}
			}
			subjectType := c.info.Types[me.Subject]
			return c.matchIsExhaustive(me, subjectType)
		}
		return false
	case *ast.InfiniteLoop:
		return !c.blockHasBreak(s.Body)
	default:
		return false
	}
}

// blockAlwaysExits reports whether a block's last statement always exits.
func (c *Checker) blockAlwaysExits(block *ast.Block) bool {
	if block == nil || len(block.Stmts) == 0 {
		return false
	}
	return c.stmtAlwaysExits(block.Stmts[len(block.Stmts)-1])
}
