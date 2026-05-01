package sema

import (
	"djabi.dev/go/promise_lang/internal/ast"
	"djabi.dev/go/promise_lang/internal/types"
)

// declare performs Pass 1: walk top-level declarations and insert names.
// Std library declarations go into stdScope; user declarations go into fileScope.
func (c *Checker) declare(file *ast.File) {
	// Process use declarations — create module objects and resolve scopes
	for _, u := range file.Uses {
		alias := u.Alias
		isGlob := alias == "_"

		mod := types.NewModule(tpos(u.Pos()), alias, u.Path)
		if u.CatalogName != "" {
			mod.SetCatalogName(u.CatalogName)
		}
		mod.SetGlob(isGlob)

		// Resolve module scope from pre-loaded scopes
		c.resolveModuleScope(u, mod)

		// Glob imports (as _) merge their exports into fileScope eagerly.
		// Non-glob imports are inserted as named module objects.
		if isGlob {
			c.mergeGlobImport(u, mod)
		} else {
			c.insert(mod)
		}

		c.modules = append(c.modules, mod)
	}

	for _, decl := range file.Decls {
		isStd := isDeclStd(decl)

		// Route std declarations to stdScope, user declarations to fileScope
		if isStd {
			c.scope = c.stdScope
		} else {
			c.scope = c.fileScope
		}

		switch d := decl.(type) {
		case *ast.TypeDecl:
			c.declareType(d)
		case *ast.EnumDecl:
			c.declareEnum(d)
		case *ast.FuncDecl:
			c.declareFunc(d)
		}
	}

	// Restore scope to fileScope
	c.scope = c.fileScope
}

// resolveModuleScope looks up the module's scope from pre-loaded moduleScopes.
func (c *Checker) resolveModuleScope(u *ast.UseDecl, mod *types.Module) {
	// Special case: "std" catalog module uses the built-in stdScope.
	// stdScope is a live reference populated during the declare pass,
	// so by the time qualified lookups happen (define/check), it's full.
	if mod.CatalogName() == "std" {
		mod.SetScope(c.stdScope)
		return
	}
	if c.moduleScopes == nil {
		if u.CatalogName != "" {
			c.errorf(u.Pos(), "unknown catalog module '%s'", u.CatalogName)
		}
		return
	}
	// Try catalog name first, then path
	key := u.CatalogName
	if key == "" {
		key = u.Path
	}
	if scope, ok := c.moduleScopes[key]; ok {
		mod.SetScope(scope)
	} else if u.CatalogName != "" {
		c.errorf(u.Pos(), "unknown catalog module '%s'", u.CatalogName)
	}
}

// mergeGlobImport dumps all exports from a module's scope into fileScope.
// Eagerly checks for name conflicts with existing declarations.
func (c *Checker) mergeGlobImport(u *ast.UseDecl, mod *types.Module) {
	scope := mod.Scope()
	if scope == nil {
		return // module has no scope (not loaded)
	}
	modName := u.CatalogName
	if modName == "" {
		modName = u.Path
	}
	for _, name := range scope.Names() {
		obj := scope.Lookup(name)
		// Only import `public symbols from modules
		if !isObjectExported(obj) {
			continue
		}
		if existing := c.fileScope.Lookup(name); existing != nil {
			c.errorf(u.Pos(), "importing module '%s' as _ conflicts with existing symbol '%s'", modName, name)
			c.errorf(u.Pos(), "hint: use `use %s` or `use %s as <alias>` to avoid conflict", modName, modName)
			continue
		}
		c.fileScope.Insert(obj)
	}
}

// isDeclStd returns true if a declaration has the IsStd flag set.
func isDeclStd(decl ast.Decl) bool {
	switch d := decl.(type) {
	case *ast.TypeDecl:
		return d.IsStd
	case *ast.EnumDecl:
		return d.IsStd
	case *ast.FuncDecl:
		return d.IsStd
	}
	return false
}

func (c *Checker) declareType(d *ast.TypeDecl) {
	if !d.IsStd && d.Name == "std" {
		c.errorf(d.Pos(), "'std' is reserved for the standard library namespace")
		return
	}

	// Native type: look up existing type from Universe scope instead of creating a new one.
	if c.hasAnnotation(d.Annotations, "native") {
		obj := types.Universe.Lookup(d.Name)
		if obj == nil {
			c.errorf(d.Pos(), "native type '%s' not found in universe", d.Name)
			return
		}
		// Insert into current scope so define() can find it via scope.Lookup.
		// Scope.Insert returns existing if already present (no error).
		c.scope.Insert(obj)
		return
	}

	// Non-native std type redeclaring a universe type (e.g., map[K,V], error):
	// reuse the universe Named singleton so that identity checks (TypMap,
	// TypError, etc.) continue to work. The define pass will add fields,
	// methods, and type-param constraints from the source declaration.
	if d.IsStd {
		if obj := types.Universe.Lookup(d.Name); obj != nil {
			c.scope.Insert(obj)
			return
		}
	}

	tn := types.NewTypeName(tpos(d.Pos()), d.Name, nil)
	if !c.insert(tn) {
		return
	}
	tparams := c.declareTypeParams(d.TypeParams)
	types.NewNamed(tn, tparams)
}

func (c *Checker) declareEnum(d *ast.EnumDecl) {
	if !d.IsStd && d.Name == "std" {
		c.errorf(d.Pos(), "'std' is reserved for the standard library namespace")
		return
	}
	tn := types.NewTypeName(tpos(d.Pos()), d.Name, nil)
	if !c.insert(tn) {
		return
	}
	tparams := c.declareTypeParams(d.TypeParams)
	types.NewEnum(tn, tparams)
}

func (c *Checker) declareFunc(d *ast.FuncDecl) {
	if !d.IsStd && d.Name == "std" {
		c.errorf(d.Pos(), "'std' is reserved for the standard library namespace")
		return
	}
	fn := types.NewFunc(tpos(d.Pos()), d.Name, nil)
	c.insert(fn)
}

// declareTypeParams creates TypeParam objects from AST type parameters.
// These are not inserted into any scope yet — that happens in define when needed.
func (c *Checker) declareTypeParams(astParams []*ast.TypeParam) []*types.TypeParam {
	if len(astParams) == 0 {
		return nil
	}
	result := make([]*types.TypeParam, len(astParams))
	for i, ap := range astParams {
		tn := types.NewTypeName(tpos(ap.Pos()), ap.Name, nil)
		// Constraints are resolved later in define pass
		result[i] = types.NewTypeParam(tn, nil, i)
	}
	return result
}

// define performs Pass 2: resolve type structures, populate fields/methods/variants.
func (c *Checker) define(file *ast.File) {
	for _, decl := range file.Decls {
		// Set scope to match where the decl was declared
		if isDeclStd(decl) {
			c.scope = c.stdScope
		} else {
			c.scope = c.fileScope
		}

		switch d := decl.(type) {
		case *ast.TypeDecl:
			c.defineType(d)
		case *ast.EnumDecl:
			c.defineEnum(d)
		case *ast.FuncDecl:
			c.defineFunc(d)
		}
	}
	c.scope = c.fileScope
}

func (c *Checker) defineType(d *ast.TypeDecl) {
	obj := c.scope.Lookup(d.Name)
	if obj == nil {
		return // error in declare
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

	isNative := c.hasAnnotation(d.Annotations, "native")

	// Open type-params scope if generic
	if len(named.TypeParams()) > 0 {
		c.openScope(d, "typeparams:"+d.Name)
		for _, tp := range named.TypeParams() {
			c.insert(tp.Obj())
		}
		// Resolve constraints only for non-native types (native types already have params)
		if !isNative {
			c.resolveTypeParamConstraints(d.TypeParams, named.TypeParams())
		}
		defer c.closeScope()
	}

	if isNative {
		// Native type: only process fields and methods, skip inheritance.
		// Skip already-registered fields/methods to avoid duplicates when the
		// same global Named singleton is processed multiple times (e.g. across
		// test runs in the same process).
		for _, fd := range d.Fields {
			if named.LookupField(fd.Name) == nil {
				c.defineField(named, fd)
			}
		}
		for _, md := range d.Methods {
			if !c.nativeMethodExists(named, md) {
				c.defineMethod(named, md, d.Name, true)
			}
		}
		// Mark HasNew for native types with a new() constructor
		if newMethod := lookupOwnMethod(named, "new"); newMethod != nil {
			named.SetHasNew(true)
		}
		c.validateMetas(d.Annotations, TargetType)
		if c.hasAnnotation(d.Annotations, "public") {
			named.SetExported(true)
		}
		return
	}

	// For universe type singletons being redeclared from source (e.g., map),
	// reset accumulated fields/methods from previous sema runs to avoid duplicates
	// and stale type references (e.g., a Slot enum pointer from a prior test run).
	if d.IsStd && types.Universe.Lookup(d.Name) != nil {
		named.ResetMembers()
	}

	// Resolve parent types (is clauses)
	for _, inh := range d.Inherits {
		pt := c.resolveType(inh)
		if pt == nil {
			continue
		}
		switch p := pt.(type) {
		case *types.Named:
			named.AddParent(p)
		case *types.Instance:
			origin, ok := p.Origin().(*types.Named)
			if !ok {
				c.errorf(inh.Pos(), "parent type must be a named type, got %s", pt)
				continue
			}
			named.AddParent(origin, p.TypeArgs())
			// Record the instance for monomorphization if type args are concrete
			allConcrete := true
			for _, ta := range p.TypeArgs() {
				if types.ContainsTypeParam(ta) {
					allConcrete = false
					break
				}
			}
			if allConcrete {
				c.recordInstance(p)
			}
		default:
			c.errorf(inh.Pos(), "parent type must be a named type, got %s", pt)
		}
	}

	// Validate: at most one parent may have fields (concrete parent).
	// Use AllFields() to catch transitively-inherited fields (e.g., B is A where A has fields).
	concreteCount := 0
	for _, pr := range named.Parents() {
		if len(pr.Named.AllFields()) > 0 {
			concreteCount++
		}
	}
	if concreteCount > 1 {
		c.errorf(d.Pos(), "type %s has multiple concrete parents; at most one parent may have fields", d.Name)
	}

	// Set structural flag before defining methods (factory validation needs it)
	if c.hasAnnotation(d.Annotations, "structural") {
		named.SetStructural(true)
	}

	// Resolve fields
	for _, fd := range d.Fields {
		c.defineField(named, fd)
	}

	// Resolve methods
	for _, md := range d.Methods {
		c.defineMethod(named, md, d.Name)
	}

	// Process meta annotations
	c.validateMetas(d.Annotations, TargetType)
	if c.hasAnnotation(d.Annotations, "copy") {
		named.SetCopy(true)
		c.validateCopyType(named, d)
	}
	if c.hasAnnotation(d.Annotations, "structural") {
		named.SetStructural(true)
	}
	if c.hasAnnotation(d.Annotations, "public") {
		named.SetExported(true)
	}
	named.SetDoc(extractDoc(d.Annotations))
	named.SetDeprecated(extractDeprecated(d.Annotations))

	// Detect and validate value types (all fields are `value placement).
	// Must run after field/meta processing, before drop/new validation.
	c.detectValueType(named, d)

	// Validate drop() method if present (after copy processing so IsCopy() is set)
	if dropMethod := named.LookupMethod("drop"); dropMethod != nil {
		c.validateDropMethod(named, dropMethod, d)
		named.SetHasDrop(true)
	}

	// Validate new() constructor if present (own methods only, not inherited)
	if newMethod := lookupOwnMethod(named, "new"); newMethod != nil {
		c.validateNewMethod(named, newMethod, d)
		named.SetHasNew(true)
	}

}

func (c *Checker) defineField(named *types.Named, fd *ast.FieldDecl) {
	typ := c.resolveType(fd.Type)
	if typ == nil {
		return
	}
	placement := c.resolvePlacement(fd.Annotations)
	isRaw := c.hasAnnotation(fd.Annotations, "raw")
	hasDef := fd.Default != nil

	f := types.NewField(tpos(fd.Pos()), fd.Name, typ, placement, isRaw, hasDef)
	if c.hasAnnotation(fd.Annotations, "final") {
		f.SetFinal(true)
	}
	if c.hasAnnotation(fd.Annotations, "public") {
		f.SetExported(true)
	}
	c.validateMetas(fd.Annotations, TargetField)
	f.SetDoc(extractDoc(fd.Annotations))
	f.SetDeprecated(extractDeprecated(fd.Annotations))
	named.AddField(f)
}

func (c *Checker) defineMethod(named *types.Named, md *ast.MethodDecl, typeName string, isNativeType ...bool) {
	sig := c.resolveMethodSignature(named, md)
	if sig == nil {
		return
	}

	placement := c.resolvePlacement(md.Annotations)
	abstract := c.hasAnnotation(md.Annotations, "abstract")
	native := c.hasAnnotation(md.Annotations, "native")

	// Validate: abstract/native methods must not have a body
	if abstract && md.Body != nil {
		c.errorf(md.Pos(), "abstract method %s.%s must not have a body", typeName, md.Name)
	}
	if native && md.Body != nil {
		c.errorf(md.Pos(), "native method %s.%s must not have a body", typeName, md.Name)
	}
	// Non-abstract, non-native methods must have a body
	if !abstract && !native && md.Body == nil {
		c.errorf(md.Pos(), "method %s.%s must have a body (or be marked `abstract or `native)", typeName, md.Name)
	}

	// Validate: generic methods cannot be abstract, native, getter, or setter
	if len(sig.TypeParams()) > 0 {
		if abstract {
			c.errorf(md.Pos(), "generic method %s.%s cannot be abstract", typeName, md.Name)
		}
		if native {
			c.errorf(md.Pos(), "generic method %s.%s cannot be native", typeName, md.Name)
		}
		if md.IsGetter {
			c.errorf(md.Pos(), "generic method %s.%s cannot be a getter", typeName, md.Name)
		}
		if md.IsSetter {
			c.errorf(md.Pos(), "generic method %s.%s cannot be a setter", typeName, md.Name)
		}
	}

	isFactory := c.hasAnnotation(md.Annotations, "factory")
	isGlobal := c.hasAnnotation(md.Annotations, "global")
	isMono := c.hasAnnotation(md.Annotations, "mono")
	// `factory implies `variant placement
	if isFactory {
		placement = types.PlaceVariant
	}

	// Validate `global and `mono methods
	if isGlobal || isMono {
		metaName := "global"
		if isMono {
			metaName = "mono"
		}
		if isFactory {
			c.errorf(md.Pos(), "`%s and `factory are mutually exclusive on %s.%s", metaName, typeName, md.Name)
		}
		if md.Receiver != nil {
			c.errorf(md.Pos(), "`%s method %s.%s must not declare a receiver", metaName, typeName, md.Name)
		}
		if md.IsGetter || md.IsSetter {
			c.errorf(md.Pos(), "`%s method %s.%s cannot be a getter or setter", metaName, typeName, md.Name)
		}
		if isGlobal && len(named.TypeParams()) > 0 {
			c.errorf(md.Pos(), "`global method %s.%s cannot be on a generic type — use `mono instead", typeName, md.Name)
		}
	}
	if isGlobal && isMono {
		c.errorf(md.Pos(), "`global and `mono are mutually exclusive on %s.%s", typeName, md.Name)
	}

	m := types.NewMethod(tpos(md.Pos()), md.Name, sig, placement, abstract, native)
	m.SetGetter(md.IsGetter)
	m.SetSetter(md.IsSetter)
	m.SetFactory(isFactory)
	// Block defining a setter on a `final field
	if md.IsSetter {
		if f := named.LookupField(md.Name); f != nil && f.IsFinal() {
			c.errorf(md.Pos(), "cannot define setter for `final field '%s'", md.Name)
		}
	}
	// Validate factory method
	if isFactory {
		nativeType := len(isNativeType) > 0 && isNativeType[0]
		c.validateFactoryMethod(named, m, md, nativeType)
	}
	if c.hasAnnotation(md.Annotations, "public") {
		m.SetExported(true)
	}
	c.validateMetas(md.Annotations, TargetMethod)
	m.SetDoc(extractDoc(md.Annotations))
	m.SetDeprecated(extractDeprecated(md.Annotations))
	named.AddMethod(m)
}

// nativeMethodExists checks if a native type already has a method with the same
// name AND arity as the AST method declaration. This is arity-aware so that
// binary -(T other) and unary -() can coexist on the same type.
func (c *Checker) nativeMethodExists(named *types.Named, md *ast.MethodDecl) bool {
	arity := len(md.Params)
	for _, m := range named.Methods() {
		if m.Name() == md.Name && len(m.Sig().Params()) == arity {
			return true
		}
	}
	return false
}

func (c *Checker) resolveMethodSignature(named *types.Named, md *ast.MethodDecl) *types.Signature {
	// Open type-params scope if generic method and create TypeParam objects
	var methodTParams []*types.TypeParam
	if len(md.TypeParams) > 0 {
		c.openScope(md, "methodtypeparams:"+md.Name)
		methodTParams = make([]*types.TypeParam, len(md.TypeParams))
		for i, tp := range md.TypeParams {
			tn := types.NewTypeName(tpos(tp.Pos()), tp.Name, nil)
			methodTParams[i] = types.NewTypeParam(tn, nil, i)
			c.insert(tn)
		}
		c.resolveTypeParamConstraints(md.TypeParams, methodTParams)
		defer c.closeScope()
	}

	// Resolve receiver
	var recv *types.Param
	isFactory := c.hasAnnotation(md.Annotations, "factory")
	isGlobal := c.hasAnnotation(md.Annotations, "global")
	isMono := c.hasAnnotation(md.Annotations, "mono")
	if isFactory || isGlobal || isMono {
		// Factory, `global, and `mono methods have no receiver
		recv = nil
	} else if md.Receiver != nil {
		ref := resolveRefMod(md.Receiver.RefMod)
		recv = types.NewParam("this", named, ref)
	} else {
		// Methods without explicit receiver default to value receiver
		recv = types.NewParam("this", named, types.RefNone)
	}

	// Resolve parameters
	params := make([]*types.Param, len(md.Params))
	for i, p := range md.Params {
		pt := c.resolveType(p.Type)
		if pt == nil {
			return nil
		}
		if p.IsVariadic {
			vecType := types.NewVector(pt)
			c.recordInstance(vecType)
			params[i] = types.NewParam(p.Name, vecType, types.RefNone)
			params[i].SetVariadic(true)
		} else {
			params[i] = types.NewParam(p.Name, pt, resolveRefMod(p.RefMod))
		}
		if p.Default != nil {
			params[i].SetHasDefault(true)
			c.info.ParamDefaults[params[i]] = p.Default
		}
		params[i].SetDoc(extractDoc(p.Annotations))
		c.validateMetas(p.Annotations, TargetParam)
	}
	c.validateVariadicParams(md.Params, params, "method '"+md.Name+"'")

	// Resolve return type
	var result types.Type
	var canError bool
	if md.ReturnType != nil {
		if md.ReturnType.Type != nil {
			result = c.resolveType(md.ReturnType.Type)
			if result == nil {
				return nil
			}
		}
		canError = md.ReturnType.CanError
	}

	// Abstract factory methods get implicit Self return type
	abstract := c.hasAnnotation(md.Annotations, "abstract")
	if isFactory && abstract {
		if result == nil {
			result = named
		}
	}

	sig := types.NewSignature(recv, params, result, canError)
	if len(methodTParams) > 0 {
		sig.SetTypeParams(methodTParams)
	}
	return sig
}

func (c *Checker) defineEnum(d *ast.EnumDecl) {
	obj := c.scope.Lookup(d.Name)
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

	// Open type-params scope if generic
	if len(enum.TypeParams()) > 0 {
		c.openScope(d, "typeparams:"+d.Name)
		for _, tp := range enum.TypeParams() {
			c.insert(tp.Obj())
		}
		c.resolveTypeParamConstraints(d.TypeParams, enum.TypeParams())
		defer c.closeScope()
	}

	// Resolve variants
	for _, v := range d.Variants {
		fields := make([]*types.VarField, len(v.Fields))
		for i, f := range v.Fields {
			ft := c.resolveType(f.Type)
			if ft == nil {
				ft = types.TypVoid // fallback to avoid nil
			}
			fields[i] = types.NewVarField(f.Name, ft)
		}
		variant := types.NewVariant(v.Name, fields)
		variant.SetDoc(extractDoc(v.Annotations))
		c.validateMetas(v.Annotations, TargetVariant)
		enum.AddVariant(variant)
	}

	// Process meta annotations
	c.validateMetas(d.Annotations, TargetEnum)
	if c.hasAnnotation(d.Annotations, "copy") {
		enum.SetCopy(true)
		c.validateCopyEnum(enum, d)
	}
	if c.hasAnnotation(d.Annotations, "public") {
		enum.SetExported(true)
	}
	enum.SetDoc(extractDoc(d.Annotations))
	enum.SetDeprecated(extractDeprecated(d.Annotations))
}

func (c *Checker) defineFunc(d *ast.FuncDecl) {
	obj := c.scope.Lookup(d.Name)
	if obj == nil {
		return
	}
	fn, ok := obj.(*types.Func)
	if !ok {
		return
	}

	// Open type-params scope if generic and create TypeParam objects
	var tparams []*types.TypeParam
	if len(d.TypeParams) > 0 {
		c.openScope(d, "typeparams:"+d.Name)
		tparams = make([]*types.TypeParam, len(d.TypeParams))
		for i, tp := range d.TypeParams {
			tn := types.NewTypeName(tpos(tp.Pos()), tp.Name, nil)
			tparams[i] = types.NewTypeParam(tn, nil, i)
			c.insert(tn)
		}
		// Resolve constraints now that params are in scope
		c.resolveTypeParamConstraints(d.TypeParams, tparams)
		defer c.closeScope()
	}

	sig := c.resolveFuncSignature(d)
	if sig != nil {
		if len(tparams) > 0 {
			sig.SetTypeParams(tparams)
		}
		fn.SetType(sig)
	}

	// Process meta annotations
	c.validateMetas(d.Annotations, TargetFunc)
	fn.SetDoc(extractDoc(d.Annotations))
	fn.SetDeprecated(extractDeprecated(d.Annotations))
	if c.hasAnnotation(d.Annotations, "public") {
		fn.SetExported(true)
	}
	if c.hasAnnotation(d.Annotations, "test") {
		if expected, ok := extractTestExpected(d.Annotations); ok {
			// `test(expected="...") — e2e output test on main()
			if d.Name != "main" {
				c.errorf(d.Pos(), "`test(expected=...) can only be applied to main()")
			}
			if c.info.HasExpectOutput {
				c.errorf(d.Pos(), "duplicate `test(expected=...) annotation")
			}
			c.info.ExpectOutput = expected
			c.info.HasExpectOutput = true
			c.info.ExcludeTargets = extractTestExclude(d.Annotations)
		} else {
			// `test — unit test function
			if sig != nil {
				if len(sig.Params()) > 0 {
					c.errorf(d.Pos(), "test function '%s' must have no parameters", d.Name)
				}
				if sig.Result() != nil && !types.Identical(sig.Result(), types.TypVoid) {
					c.errorf(d.Pos(), "test function '%s' must not have a return type", d.Name)
				}
				if sig.CanError() {
					c.errorf(d.Pos(), "test function '%s' must not be failable", d.Name)
				}
				if len(sig.TypeParams()) > 0 {
					c.errorf(d.Pos(), "test function '%s' must not be generic", d.Name)
				}
			}
			fn.SetTest(true)
			c.info.Tests = append(c.info.Tests, fn)
			if excludes := extractTestExclude(d.Annotations); len(excludes) > 0 {
				if c.info.TestExcludes == nil {
					c.info.TestExcludes = make(map[string][]string)
				}
				c.info.TestExcludes[d.Name] = excludes
			}
		}
	}
}

func (c *Checker) resolveFuncSignature(d *ast.FuncDecl) *types.Signature {
	// Resolve parameters
	params := make([]*types.Param, len(d.Params))
	for i, p := range d.Params {
		pt := c.resolveType(p.Type)
		if pt == nil {
			return nil
		}
		if p.IsVariadic {
			// Variadic param: declared as ...T, stored as T[] internally
			vecType := types.NewVector(pt)
			c.recordInstance(vecType)
			params[i] = types.NewParam(p.Name, vecType, types.RefNone)
			params[i].SetVariadic(true)
		} else {
			params[i] = types.NewParam(p.Name, pt, resolveRefMod(p.RefMod))
		}
		if p.Default != nil {
			params[i].SetHasDefault(true)
			c.info.ParamDefaults[params[i]] = p.Default
		}
		params[i].SetDoc(extractDoc(p.Annotations))
		c.validateMetas(p.Annotations, TargetParam)
	}
	c.validateVariadicParams(d.Params, params, "function '"+d.Name+"'")

	// Resolve return type
	var result types.Type
	var canError bool
	if d.ReturnType != nil {
		if d.ReturnType.Type != nil {
			result = c.resolveType(d.ReturnType.Type)
			if result == nil {
				return nil
			}
		}
		canError = d.ReturnType.CanError
	}

	return types.NewSignature(nil, params, result, canError)
}

// resolveTypeParamConstraints resolves constraints for type parameters.
// Supports multiple constraints: T: A + B resolves all constraint types.
func (c *Checker) resolveTypeParamConstraints(astParams []*ast.TypeParam, tparams []*types.TypeParam) {
	for i, ap := range astParams {
		if len(ap.Constraint) == 0 {
			continue
		}
		var resolved []types.Type
		for _, cr := range ap.Constraint {
			ct := c.resolveType(cr)
			if ct != nil {
				resolved = append(resolved, ct)
			}
		}
		if len(resolved) > 0 {
			tparams[i].SetConstraints(resolved)
		}
	}
}

// lookupOwnMethod searches only the type's directly declared methods (not inherited).
func lookupOwnMethod(named *types.Named, name string) *types.Method {
	for _, m := range named.Methods() {
		if m.Name() == name && !m.IsGetter() && !m.IsSetter() {
			return m
		}
	}
	return nil
}

// resolvePlacement extracts placement from meta annotations.
func (c *Checker) resolvePlacement(annotations []*ast.MetaAnnotation) types.Placement {
	for _, ann := range annotations {
		switch ann.Name {
		case "value":
			return types.PlaceValue
		case "variant":
			return types.PlaceVariant
		case "global":
			return types.PlaceType
		case "mono":
			return types.PlaceVariant
		case "instance":
			return types.PlaceInstance
		}
	}
	return types.PlaceInstance
}

// hasAnnotation checks if a specific annotation is present.
func (c *Checker) hasAnnotation(annotations []*ast.MetaAnnotation, name string) bool {
	for _, ann := range annotations {
		if ann.Name == name {
			return true
		}
	}
	return false
}
