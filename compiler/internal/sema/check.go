package sema

import (
	"time"

	"djabi.dev/go/promise_lang/internal/ast"
	"djabi.dev/go/promise_lang/internal/types"
)

// Checker performs semantic analysis on a parsed AST file.
type Checker struct {
	file               *ast.File
	info               *Info
	errors             []error
	isUniverseProvider bool                    // auto-detected: true when this file provides universe type implementations (std module)
	globScope          *types.Scope            // glob-import scope (child of Universe, parent of fileScope)
	fileScope          *types.Scope            // file-level scope (child of globScope, holds user declarations)
	scope              *types.Scope            // current scope during traversal
	curFunc            *types.Signature        // current function being checked (for return/raise)
	curFuncObj         *types.Func             // current function object (for clone-requirement recording, T0616)
	curMethodObj       *types.Method           // current method object (for clone-requirement recording, T0616)
	curType            *types.Named            // current type being defined/checked (for Self resolution)
	inNewBody          bool                    // true when checking a new() constructor body
	inFactoryBody      bool                    // true when checking a `factory method body
	factoryLocals      map[string]bool         // variables initialized from constructor calls in factory body
	inLoop             int                     // nesting depth of loop constructs
	lambdaDepth        int                     // nesting depth of lambdas (0 = not in lambda)
	lambdaCaptures     map[string]*CapturedVar // current lambda's captured vars (by name)
	lambdaScope        *types.Scope            // scope at lambda definition site (capture boundary)
	lambdaMove         bool                    // true if current lambda uses `move` keyword
	typeHint           types.Type              // expected type for numeric literal adaptation (propagated through arithmetic)
	inUnaryNeg         bool                    // true when checking operand of unary negation (for signed suffix range check)
	inGenerator        bool                    // true when checking a generator function body
	generatorElemType  types.Type              // T from stream[T] or Iterator[T] return type
	yieldFound         bool                    // true if at least one yield seen in current generator func
	modules            []*types.Module         // all modules from use declarations
	moduleScopes       map[string]*types.Scope // pre-loaded module scopes (catalog name or path → scope)
	target             TargetInfo              // compile target for `target(cond)` filtering (zero = no filtering)
	pendingNarrowings  []NarrowedVar           // post-divergence narrowings to apply before next statement
}

// selfType returns the current type as Self would resolve:
// for generic types, an Instance with the type's own params as args (e.g., Vector[T]);
// for non-generic types, the Named type directly.
func (c *Checker) selfType() types.Type {
	if c.curType == nil {
		return nil
	}
	if tparams := c.curType.TypeParams(); len(tparams) > 0 {
		args := make([]types.Type, len(tparams))
		for i, tp := range tparams {
			args[i] = tp
		}
		return types.NewInstance(c.curType, args)
	}
	return c.curType
}

// Check performs semantic analysis on the given AST file.
// It returns type information and any semantic errors found.
func Check(file *ast.File) (*Info, []error) {
	return CheckWithModules(file, nil)
}

// CheckWithModules performs semantic analysis with pre-loaded module scopes.
// moduleScopes maps module keys (catalog name or source path) to their exported scopes.
func CheckWithModules(file *ast.File, moduleScopes map[string]*types.Scope) (*Info, []error) {
	return CheckWithTarget(file, moduleScopes, TargetInfo{})
}

// CheckWithTarget performs semantic analysis with pre-loaded module scopes and a specific
// build target. Declarations annotated with `target(cond) are filtered: declarations whose
// condition does not match target are skipped entirely (not declared, not type-checked).
// Use ParseTargetInfo to derive a TargetInfo from an LLVM target triple.
func CheckWithTarget(file *ast.File, moduleScopes map[string]*types.Scope, target TargetInfo) (*Info, []error) {
	c := &Checker{
		moduleScopes: moduleScopes,
		target:       target,
		file:         file,
		info: &Info{
			Types:                    make(map[ast.Expr]types.Type),
			Objects:                  make(map[*ast.IdentExpr]types.Object),
			Scopes:                   make(map[ast.Node]*types.Scope),
			FieldDefaults:            make(map[*types.Field]ast.Expr),
			ParamDefaults:            make(map[*types.Param]ast.Expr),
			LambdaCaptures:           make(map[*ast.LambdaExpr][]*CapturedVar),
			OptionalNarrowings:       make(map[*ast.IfStmt]*OptionalNarrowing),
			IsDestructureNarrowings:  make(map[*ast.IfStmt]*IsDestructureNarrowing),
			IsPatternTypes:           make(map[ast.IsPattern]types.Type),
			ErrorHandlerTypes:        make(map[*ast.ErrorHandlerExpr]types.Type),
			FailableExprs:            make(map[ast.Expr]bool),
			AutoPropagateExprs:       make(map[ast.Expr]bool),
			OptionalRecoveryHandlers: make(map[ast.Expr]bool),
			OptionalHandlers:         make(map[ast.Expr]bool),
			FailableDestructures:     make(map[*ast.DestructureVarDecl]bool),
			ForInKinds:               make(map[*ast.ForInStmt]ForInKind),
			GeneratorFuncs:           make(map[ast.Node]*GeneratorInfo),
			FilteredDecls:            make(map[ast.Decl]bool),
			DeclHashes:               make(map[*types.TypeName]string),
			InferredTypeArgs:         make(map[*ast.CallExpr]*InferredCall),
			FuncCloneReqs:            make(map[*types.Func][]CloneabilityRequirement),
			MethodCloneReqs:          make(map[*types.Method][]CloneabilityRequirement),
		},
	}

	c.globScope = types.NewScope(
		types.Universe, tpos(file.Pos()), tpos(file.End()), "glob",
	)
	c.fileScope = types.NewScope(
		c.globScope, tpos(file.Pos()), tpos(file.End()), "file",
	)
	c.scope = c.fileScope
	c.info.Scopes[file] = c.fileScope
	// fileScope before globScope: user declarations (fileScope) take priority over
	// glob-imported symbols (globScope) in codegen's ScopeOrder-based lookups.
	c.info.ScopeOrder = append(c.info.ScopeOrder, c.fileScope)
	c.info.ScopeOrder = append(c.info.ScopeOrder, c.globScope)

	tPass := time.Now()
	c.declare(file) // Pass 1: collect all declarations
	c.info.Timings.Declare = time.Since(tPass)

	c.populateUniverseTypes() // Populate non-native universe type pointers (TypError, TypMap, etc.)

	tPass = time.Now()
	c.define(file)                // Pass 2: resolve types, populate type structures
	c.propagateDrops(file)        // B0158: auto-synthesize drop for types with droppable fields
	c.validateCloneTypes(file)    // T0154: validate `clone field types (after all types defined)
	c.validateSendableTypes(file) // T0158: validate `sendable/`sharable field types
	c.validateConstructors(file)  // Validate: constructor inheritance (after all types defined)
	c.validateBuiltins()          // Validate: .pr files declare all required operators/methods/fields
	c.info.Timings.Define = time.Since(tPass)

	tPass = time.Now()
	c.check(file) // Pass 3: type-check function/method bodies
	c.propagateCloneReqs()
	c.info.Timings.Check = time.Since(tPass)

	tPass = time.Now()
	c.checkMissingReturn(file) // Pass 4: verify non-void functions return
	c.info.Timings.Verify = time.Since(tPass)

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
	return DeclareAndDefineWithTarget(file, moduleScopes, TargetInfo{})
}

// DeclareAndDefineWithTarget runs Declare + Define with pre-loaded module scopes
// and target filtering for `target(cond)` annotations.
func DeclareAndDefineWithTarget(file *ast.File, moduleScopes map[string]*types.Scope, target TargetInfo) (*Info, []error) {
	c := &Checker{
		moduleScopes: moduleScopes,
		target:       target,
		file:         file,
		info: &Info{
			Types:                    make(map[ast.Expr]types.Type),
			Objects:                  make(map[*ast.IdentExpr]types.Object),
			Scopes:                   make(map[ast.Node]*types.Scope),
			FieldDefaults:            make(map[*types.Field]ast.Expr),
			ParamDefaults:            make(map[*types.Param]ast.Expr),
			LambdaCaptures:           make(map[*ast.LambdaExpr][]*CapturedVar),
			OptionalNarrowings:       make(map[*ast.IfStmt]*OptionalNarrowing),
			IsDestructureNarrowings:  make(map[*ast.IfStmt]*IsDestructureNarrowing),
			IsPatternTypes:           make(map[ast.IsPattern]types.Type),
			ErrorHandlerTypes:        make(map[*ast.ErrorHandlerExpr]types.Type),
			FailableExprs:            make(map[ast.Expr]bool),
			AutoPropagateExprs:       make(map[ast.Expr]bool),
			OptionalRecoveryHandlers: make(map[ast.Expr]bool),
			OptionalHandlers:         make(map[ast.Expr]bool),
			FailableDestructures:     make(map[*ast.DestructureVarDecl]bool),
			ForInKinds:               make(map[*ast.ForInStmt]ForInKind),
			GeneratorFuncs:           make(map[ast.Node]*GeneratorInfo),
			FilteredDecls:            make(map[ast.Decl]bool),
			DeclHashes:               make(map[*types.TypeName]string),
			InferredTypeArgs:         make(map[*ast.CallExpr]*InferredCall),
			FuncCloneReqs:            make(map[*types.Func][]CloneabilityRequirement),
			MethodCloneReqs:          make(map[*types.Method][]CloneabilityRequirement),
		},
	}

	c.globScope = types.NewScope(
		types.Universe, tpos(file.Pos()), tpos(file.End()), "glob",
	)
	c.fileScope = types.NewScope(
		c.globScope, tpos(file.Pos()), tpos(file.End()), "file",
	)
	c.scope = c.fileScope
	c.info.Scopes[file] = c.fileScope
	c.info.ScopeOrder = append(c.info.ScopeOrder, c.fileScope)
	c.info.ScopeOrder = append(c.info.ScopeOrder, c.globScope)

	c.declare(file)               // Pass 1: collect all declarations
	c.populateUniverseTypes()     // Populate non-native universe type pointers (TypError, TypMap, etc.)
	c.define(file)                // Pass 2: resolve types, populate type structures
	c.propagateDrops(file)        // B0158: auto-synthesize drop for types with droppable fields
	c.validateCloneTypes(file)    // T0154: validate `clone field types (after all types defined)
	c.validateSendableTypes(file) // T0158: validate `sendable/`sharable field types
	c.validateConstructors(file)  // Validate: constructor inheritance

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
		// Skip declarations that were rejected in the declare pass (redeclaration
		// errors) or filtered by `target(cond) — their symbols don't exist in scope.
		if c.info.FilteredDecls[decl] {
			continue
		}
		c.scope = c.fileScope

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

	// Module-level setters are stored under "name$set" in scope.
	scopeName := d.Name
	if d.IsSetter {
		scopeName = d.Name + "$set"
	}
	obj := c.lookup(scopeName)
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
	savedFuncObj := c.curFuncObj
	savedMethodObj := c.curMethodObj
	c.curFuncObj = fn
	c.curMethodObj = nil
	defer func() {
		c.curFunc = saved
		c.curFuncObj = savedFuncObj
		c.curMethodObj = savedMethodObj
	}()

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
		c.info.GeneratorFuncs[d] = &GeneratorInfo{ElemType: c.generatorElemType, CanError: sig.CanError()}
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
		} else if m.Placement() == types.PlaceType {
			// `global methods: no Self, no this
			savedCurType := c.curType
			c.curType = nil
			c.checkMethodBody(d.Name, md, m)
			c.curType = savedCurType
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
	savedFuncObj := c.curFuncObj
	savedMethodObj := c.curMethodObj
	c.curFuncObj = nil
	c.curMethodObj = m
	defer func() {
		c.curFunc = saved
		c.curFuncObj = savedFuncObj
		c.curMethodObj = savedMethodObj
	}()

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

	// For generic methods, open type param scope so body can reference method-level type params
	if len(m.Sig().TypeParams()) > 0 {
		c.openScope(md, "methodtypeparams:"+typeName+"."+md.Name)
		for _, tp := range m.Sig().TypeParams() {
			c.insert(tp.Obj())
		}
		defer c.closeScope()
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
		c.info.GeneratorFuncs[md] = &GeneratorInfo{ElemType: c.generatorElemType, CanError: m.Sig().CanError()}
	} else if c.inGenerator && !c.yieldFound {
		c.errorf(md.Pos(), "method %s returns stream[%s] but contains no yield statements", md.Name, c.generatorElemType)
	}

	c.inGenerator = savedInGenerator
	c.generatorElemType = savedGenElemType
	c.yieldFound = savedYieldFound
}

// checkEnumDecl type-checks method bodies in an enum declaration.
func (c *Checker) checkEnumDecl(d *ast.EnumDecl) {
	obj := c.lookup(d.Name)
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

	// For generic enums, open type param scope so method bodies can reference T, K, V, etc.
	if len(enum.TypeParams()) > 0 {
		c.openScope(d, "typeparams:"+d.Name)
		for _, tp := range enum.TypeParams() {
			c.insert(tp.Obj())
		}
		defer c.closeScope()
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
		c.checkMethodBody(d.Name, md, m)
	}
}

// checkBlock type-checks a block of statements.
// Detects unreachable code after statements that always exit (return/raise/break/continue).
func (c *Checker) checkBlock(block *ast.Block) {
	dead := false
	narrowScopes := 0
	for _, stmt := range block.Stmts {
		if dead {
			c.errorf(stmt.Pos(), "unreachable code")
			break // report once per block
		}
		c.checkStmt(stmt)
		// Post-divergence narrowing: if an if-statement produced narrowings
		// (e.g., `if x is absent { return; }`), open a scope and shadow the
		// narrowed variables for all subsequent statements in this block.
		if len(c.pendingNarrowings) > 0 {
			c.openScope(block, "post-narrow")
			for _, v := range c.pendingNarrowings {
				c.scope.Insert(types.NewVar(tpos(stmt.Pos()), v.VarName, v.InnerType))
			}
			c.pendingNarrowings = nil
			narrowScopes++
		}
		dead = c.stmtAlwaysExits(stmt)
	}
	for range narrowScopes {
		c.closeScope()
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

// populateUniverseTypes sets non-native universe type pointers (TypError, TypMap,
// TypRange, TypIter, TypStream) from the current scope. Called after Pass 1 (declare)
// so Named objects exist. When compiling the std module, the types are found in the
// file scope (declared by std itself). For non-std compilations, they are found in
// the glob scope (imported from std via `use std as _`).
//
// Also installs sugar aliases (map, iter, stream) into the Universe scope and
// auto-detects whether this compilation is the universe provider (std module)
// for native type ResetMembers behavior.
func (c *Checker) populateUniverseTypes() {
	// Check if this file declares the non-native universe types (i.e., this is
	// the std module). We look in the file scope specifically — if found there,
	// the type was declared in THIS file, not inherited from a parent scope.
	isStd := c.fileScope.Lookup("error") != nil

	populate := func(name string, target **types.Named) {
		var obj types.Object
		if isStd {
			// Std module: use the freshly-declared type from this file.
			// Must update even if *target is already set (B0101: second sema
			// run creates new Named objects that replace stale ones).
			obj = c.fileScope.Lookup(name)
		} else if *target != nil {
			return // already populated from a prior std compilation
		} else {
			// Non-std: look up from full scope chain (glob scope from std import).
			obj, _ = c.scope.LookupParent(name)
		}
		if obj == nil {
			return
		}
		tn, ok := obj.(*types.TypeName)
		if !ok {
			return
		}
		n, ok := tn.Type().(*types.Named)
		if !ok {
			return
		}
		*target = n
	}

	populate("error", &types.TypError)
	populate("Map", &types.TypMap)
	populate("Range", &types.TypRange)
	populate("Iterator", &types.TypIter)
	populate("Stream", &types.TypStream)
	populate("EmbeddedFile", &types.TypEmbeddedFile)
	populate("EmbeddedFiles", &types.TypEmbeddedFiles)

	if isStd {
		c.isUniverseProvider = true
	}

	// Install or update sugar aliases in Universe scope.
	installAlias := func(alias string, target *types.Named) {
		if target == nil {
			return
		}
		newType := target.Obj().Type()
		if existing := types.Universe.Lookup(alias); existing != nil {
			// Update existing alias to point to the (possibly new) target.
			existing.(*types.TypeName).SetType(newType)
		} else {
			tn := types.NewTypeName(types.Pos{}, alias, newType)
			types.Universe.Insert(tn)
		}
	}
	installAlias("map", types.TypMap)
	installAlias("iter", types.TypIter)
	installAlias("stream", types.TypStream)
}
