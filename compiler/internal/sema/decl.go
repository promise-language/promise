package sema

import (
	"path/filepath"
	"strings"
	"time"

	"djabi.dev/go/promise_lang/internal/ast"
	"djabi.dev/go/promise_lang/internal/types"
)

// declare performs Pass 1: walk top-level declarations and insert names.
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

	c.scope = c.fileScope
	for _, decl := range file.Decls {
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

// mergeGlobImport dumps all exports from a module's scope into globScope.
// Glob-imported symbols go into globScope (parent of fileScope) so that
// user declarations in fileScope can shadow them without conflict.
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
		if existing := c.globScope.Lookup(name); existing != nil {
			if existing == obj {
				continue // already imported (idempotent glob import)
			}
			// Two different glob imports export the same name — that's a conflict.
			c.errorf(u.Pos(), "importing module '%s' as _ conflicts with existing symbol '%s'", modName, name)
			c.errorf(u.Pos(), "hint: use `use %s` or `use %s as <alias>` to avoid conflict", modName, modName)
			continue
		}
		c.globScope.Insert(obj)
	}
}

func (c *Checker) declareType(d *ast.TypeDecl) {
	if d.Name == "std" {
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

	if !c.matchesTarget(d.Annotations) {
		c.info.FilteredDecls[d] = true
		return
	}

	tn := types.NewTypeName(tpos(d.Pos()), d.Name, nil)
	if !c.insert(tn) {
		// Insertion failed (redeclaration error). Mark as filtered so that
		// defineType/check passes skip this declaration — if we allow
		// defineType to proceed, it would find the existing (e.g. std-imported)
		// type and incorrectly add this declaration's methods to it.
		c.info.FilteredDecls[d] = true
		return
	}
	tparams := c.declareTypeParams(d.TypeParams)
	types.NewNamed(tn, tparams)
	c.info.DeclHashes[tn] = HashTypeDecl(d)
}

func (c *Checker) declareEnum(d *ast.EnumDecl) {
	if d.Name == "std" {
		c.errorf(d.Pos(), "'std' is reserved for the standard library namespace")
		return
	}
	if !c.matchesTarget(d.Annotations) {
		c.info.FilteredDecls[d] = true
		return
	}
	tn := types.NewTypeName(tpos(d.Pos()), d.Name, nil)
	if !c.insert(tn) {
		// Insertion failed (redeclaration). Mark filtered so define/check passes skip it.
		c.info.FilteredDecls[d] = true
		return
	}
	tparams := c.declareTypeParams(d.TypeParams)
	types.NewEnum(tn, tparams)
	c.info.DeclHashes[tn] = HashEnumDecl(d)
}

func (c *Checker) declareFunc(d *ast.FuncDecl) {
	if d.Name == "std" {
		c.errorf(d.Pos(), "'std' is reserved for the standard library namespace")
		return
	}
	if !c.matchesTarget(d.Annotations) {
		c.info.FilteredDecls[d] = true
		return
	}
	// Module-level setters are stored under "name$set" to avoid collision with getters.
	scopeName := d.Name
	if d.IsSetter {
		scopeName = d.Name + "$set"
	}
	fn := types.NewFunc(tpos(d.Pos()), scopeName, nil)
	if !c.insert(fn) {
		// Insertion failed (redeclaration). Mark filtered so define/check passes skip it.
		c.info.FilteredDecls[d] = true
	}
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
	c.scope = c.fileScope
	for _, decl := range file.Decls {
		// Skip declarations that were rejected in the declare pass (redeclaration
		// errors) or filtered by `target(cond) — processing them would corrupt
		// existing types by adding methods to the wrong Named object.
		if c.info.FilteredDecls[decl] {
			continue
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
		if c.isUniverseProvider {
			// Universe provider (std module): reset members to clear stale type
			// references from a previous sema run in the same process (B0101).
			named.ResetMembers()
			for _, fd := range d.Fields {
				c.defineField(named, fd)
			}
			for _, md := range d.Methods {
				c.defineMethod(named, md, d.Name, true)
			}
		} else {
			// Non-std context (e.g. Go test helpers): skip already-registered
			// fields/methods to avoid duplicates.
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
		}
		// Mark HasNew for native types with a new() constructor
		if newMethod := lookupOwnMethod(named, "new"); newMethod != nil {
			named.SetHasNew(true)
		}
		// Validate and register drop() for native types (B0157)
		if dropMethod := named.LookupMethod("drop"); dropMethod != nil {
			c.validateDropMethod(named, dropMethod, d)
			named.SetHasDrop(true)
		}
		c.validateMetas(d.Annotations, TargetType)
		if c.hasAnnotation(d.Annotations, "public") {
			named.SetExported(true)
		}
		return
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

	// Process `clone: set flag and synthesize clone() Self method if not user-defined.
	// Validation of field cloneability is deferred to validateCloneTypes() after all types are defined.
	if c.hasAnnotation(d.Annotations, "clone") {
		named.SetClone(true)
		if named.IsCopy() {
			c.warnf(d.Pos(), "`clone is redundant on `copy type %s", d.Name)
		}
		if named.IsStructural() {
			c.errorf(d.Pos(), "`clone cannot be applied to structural type %s", d.Name)
		} else if lookupOwnMethod(named, "clone") == nil {
			md := c.synthesizeCloneMethod(named, d)
			d.Methods = append(d.Methods, md)
			c.defineMethod(named, md, d.Name)
		}
	}

	// Process `serializable: extract field annotations, synthesize encode/decode methods.
	if c.hasAnnotation(d.Annotations, "serializable") {
		c.processSerializableType(named, d)
	}

	// Process `sendable / `sharable / `not_sendable / `not_sharable: set flags.
	// Validation is deferred to validateSendableTypes() after all types are defined.
	if c.hasAnnotation(d.Annotations, "sendable") {
		named.SetSendable(true)
	}
	if c.hasAnnotation(d.Annotations, "sharable") {
		named.SetSharable(true)
	}
	if c.hasAnnotation(d.Annotations, "not_sendable") {
		named.SetNotSendable(true)
	}
	if c.hasAnnotation(d.Annotations, "not_sharable") {
		named.SetNotSharable(true)
	}

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

// propagateDrops runs a fixpoint pass after all types are defined: for each type
// without explicit drop(), if any field's type has HasDrop(), mark it as needing a
// compiler-synthesized drop. Must be a fixpoint loop since type A may contain type B
// which contains a droppable type — B gets auto-drop first, then A detects B has drop.
// B0158: auto-synthesize drop for types with droppable fields.
func (c *Checker) propagateDrops(file *ast.File) {
	changed := true
	for changed {
		changed = false
		for _, decl := range file.Decls {
			if c.info.FilteredDecls[decl] {
				continue
			}
			switch d := decl.(type) {
			case *ast.TypeDecl:
				obj := c.scope.Lookup(d.Name)
				if obj == nil {
					continue
				}
				tn, ok := obj.(*types.TypeName)
				if !ok {
					continue
				}
				named, ok := tn.Type().(*types.Named)
				if !ok {
					continue
				}
				// Skip types that already have drop, are copy, or value types.
				if named.HasDrop() || named.IsCopy() || named.IsValueType() {
					continue
				}
				// Check all fields (including inherited) for droppable types
				for _, f := range named.AllFields() {
					if fieldTypeHasDrop(f.Type()) {
						named.SetHasDrop(true)
						named.SetNeedsSynthDrop(true)
						changed = true
						break
					}
				}
			case *ast.EnumDecl:
				obj := c.scope.Lookup(d.Name)
				if obj == nil {
					continue
				}
				tn, ok := obj.(*types.TypeName)
				if !ok {
					continue
				}
				enum, ok := tn.Type().(*types.Enum)
				if !ok {
					continue
				}
				// Skip enums that already have drop or are copy.
				if enum.HasDrop() || enum.IsCopy() {
					continue
				}
				// T0102: Check all variant fields for droppable types
				for _, v := range enum.Variants() {
					for _, f := range v.Fields() {
						if fieldTypeHasDrop(f.Type()) {
							enum.SetHasDrop(true)
							enum.SetNeedsSynthDrop(true)
							changed = true
							break
						}
					}
					if enum.HasDrop() {
						break
					}
				}
			}
		}
	}
}

// fieldTypeHasDrop returns true if the type (possibly wrapped in Instance/SharedRef/MutRef)
// resolves to a Named type with HasDrop(), or is a string/vector type, or is a
// heap-allocated user type that needs pal_free (B0192). B0167.
// String/vector types don't have HasDrop() on the Named type, but their presence
// triggers synthesized drop for the containing type. The synthesized drop body
// handles string/vector fields and also pal_free's non-droppable heap user type
// fields (B0192). This enables cascading: types containing types with string
// fields or heap user type fields get proper instance struct cleanup via the
// synthesized drop chain.
func fieldTypeHasDrop(typ types.Type) bool {
	switch t := typ.(type) {
	case *types.Named:
		if t == types.TypString || t == types.TypVector || t == types.TypChannel {
			return true
		}
		if t.HasDrop() {
			return true
		}
		// B0192: Non-droppable heap user types still need pal_free.
		// Value types have inline data (no heap pointer), and structural
		// interfaces aren't concrete instances.
		return !t.IsValueType() && !t.IsStructural() && !isPrimitive(t)
	case *types.Enum:
		return t.HasDrop()
	case *types.Instance:
		if n, ok := t.Origin().(*types.Named); ok {
			if n == types.TypVector || n == types.TypChannel {
				return true
			}
			if n.HasDrop() {
				return true
			}
			// B0192: Same as above for generic instances.
			return !n.IsValueType() && !n.IsStructural() && !isPrimitive(n)
		}
		if e, ok := t.Origin().(*types.Enum); ok {
			return e.HasDrop()
		}
	case *types.Optional:
		// T0101: Optional wrapping a droppable type needs synthesized drop
		return fieldTypeHasDrop(t.Elem())
	case *types.Tuple:
		// T0371: Tuples with droppable fields need synthesized drop.
		// Enables enums like `Some((int, string) data)` to drop the string.
		for _, e := range t.Elems() {
			if fieldTypeHasDrop(e) {
				return true
			}
		}
		return false
	case *types.Array:
		// T0485: Fixed-size [N]T with a droppable element type needs synth drop
		// so the synth enum/struct drop walks each element.
		return fieldTypeHasDrop(t.Elem())
	case *types.Signature:
		// B0217: Function-typed fields hold closure fat pointers {fn_ptr, env_ptr}.
		// The env_ptr may be a heap-allocated capture struct that needs freeing.
		return true
	}
	return false
}

// isPrimitive returns true for built-in primitive scalar types (int, bool, etc.)
// that are never heap-allocated as standalone instances.
func isPrimitive(n *types.Named) bool {
	return n == types.TypInt || n == types.TypUint ||
		n == types.TypBool || n == types.TypChar ||
		n == types.TypI8 || n == types.TypI16 || n == types.TypI32 || n == types.TypI64 ||
		n == types.TypU8 || n == types.TypU16 || n == types.TypU32 || n == types.TypU64 ||
		n == types.TypF32 || n == types.TypF64 ||
		n == types.TypVoid || n == types.TypNone
}

// containsRef reports whether typ is or contains a SharedRef or MutRef.
// B0034: catches both direct references (string&) and wrapped forms (string&?).
func containsRef(typ types.Type) bool {
	switch t := typ.(type) {
	case *types.SharedRef, *types.MutRef:
		return true
	case *types.Optional:
		return containsRef(t.Elem())
	default:
		return false
	}
}

func (c *Checker) defineField(named *types.Named, fd *ast.FieldDecl) {
	typ := c.resolveType(fd.Type)
	if typ == nil {
		return
	}
	// B0034: reject reference-typed fields until lifetime tracking is implemented
	if containsRef(typ) {
		c.errorf(fd.Pos(), "reference type %s cannot be used as a field type (stored references are not yet supported)", typ)
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
	// Serialization annotations — stored on field for later use by processSerializableType.
	if c.hasAnnotation(fd.Annotations, "skip") {
		f.SetSkip(true)
	}
	if c.hasAnnotation(fd.Annotations, "include_none") {
		f.SetIncludeNone(true)
	}
	if c.hasAnnotation(fd.Annotations, "required") {
		f.SetRequired(true)
	}
	if c.hasAnnotation(fd.Annotations, "flatten") {
		f.SetFlatten(true)
	}
	if key := extractKey(fd.Annotations); key != "" {
		f.SetKeyName(key)
	}
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
		if md.IsSetter {
			c.errorf(md.Pos(), "`%s method %s.%s cannot be a setter", metaName, typeName, md.Name)
		}
		if isMono && md.IsGetter {
			c.errorf(md.Pos(), "`mono method %s.%s cannot be a getter", typeName, md.Name)
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
			params[i].SetDefaultExpr(p.Default) // stored on param for cross-module lookup
			c.info.ParamDefaults[params[i]] = p.Default
		}
		params[i].SetDoc(extractDoc(p.Annotations))
		params[i].SetLifetime(extractLifetime(p.Annotations))
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
			result = c.selfType()
		}
	}

	sig := types.NewSignature(recv, params, result, canError)
	if len(methodTParams) > 0 {
		sig.SetTypeParams(methodTParams)
	}
	resultLifetime := extractLifetime(md.Annotations)
	if resultLifetime != "" {
		sig.SetResultLifetime(resultLifetime)
	}
	c.validateLifetimes(sig, md.Annotations, md.Params)
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
			// B0034: reject reference-typed variant fields
			if containsRef(ft) {
				c.errorf(f.Pos(), "reference type %s cannot be used as a field type (stored references are not yet supported)", ft)
			}
			fields[i] = types.NewVarField(f.Name, ft)
		}
		variant := types.NewVariant(v.Name, fields)
		variant.SetDoc(extractDoc(v.Annotations))
		c.validateMetas(v.Annotations, TargetVariant)
		enum.AddVariant(variant)
	}

	// Resolve methods
	for _, md := range d.Methods {
		c.defineEnumMethod(enum, md, d.Name)
	}

	// Process meta annotations
	c.validateMetas(d.Annotations, TargetEnum)
	if c.hasAnnotation(d.Annotations, "copy") {
		enum.SetCopy(true)
		c.validateCopyEnum(enum, d)
	}
	if c.hasAnnotation(d.Annotations, "clone") {
		enum.SetClone(true)
		if enum.IsCopy() {
			c.warnf(d.Pos(), "`clone is redundant on `copy enum %s", d.Name)
		}
		if lookupOwnEnumMethod(enum, "clone") == nil {
			md := c.synthesizeEnumCloneMethod(enum, d)
			d.Methods = append(d.Methods, md)
			c.defineEnumMethod(enum, md, d.Name)
		}
	}
	// Validate drop() method if present (after copy processing so IsCopy() is set)
	if dropMethod := lookupOwnEnumMethod(enum, "drop"); dropMethod != nil {
		c.validateEnumDropMethod(enum, dropMethod, d)
		enum.SetHasDrop(true)
	}
	if c.hasAnnotation(d.Annotations, "public") {
		enum.SetExported(true)
	}
	enum.SetDoc(extractDoc(d.Annotations))
	enum.SetDeprecated(extractDeprecated(d.Annotations))

	if c.hasAnnotation(d.Annotations, "serializable") {
		c.processSerializableEnum(enum, d)
	}

	// Process `sendable / `sharable / `not_sendable / `not_sharable: set flags.
	// Validation is deferred to validateSendableTypes() after all types are defined.
	if c.hasAnnotation(d.Annotations, "sendable") {
		enum.SetSendable(true)
	}
	if c.hasAnnotation(d.Annotations, "sharable") {
		enum.SetSharable(true)
	}
	if c.hasAnnotation(d.Annotations, "not_sendable") {
		enum.SetNotSendable(true)
	}
	if c.hasAnnotation(d.Annotations, "not_sharable") {
		enum.SetNotSharable(true)
	}
}

func (c *Checker) defineEnumMethod(enum *types.Enum, md *ast.MethodDecl, enumName string) {
	sig := c.resolveEnumMethodSignature(enum, md)
	if sig == nil {
		return
	}

	abstract := c.hasAnnotation(md.Annotations, "abstract")
	native := c.hasAnnotation(md.Annotations, "native")

	// Enum methods cannot be abstract, native, global, or mono
	if abstract {
		c.errorf(md.Pos(), "enum method %s.%s cannot be abstract", enumName, md.Name)
	}
	if native {
		c.errorf(md.Pos(), "enum method %s.%s cannot be native", enumName, md.Name)
	}
	if c.hasAnnotation(md.Annotations, "global") {
		c.errorf(md.Pos(), "enum method %s.%s cannot be `global", enumName, md.Name)
	}
	if c.hasAnnotation(md.Annotations, "mono") {
		c.errorf(md.Pos(), "enum method %s.%s cannot be `mono", enumName, md.Name)
	}

	// Validate: generic enum methods cannot be getters or setters
	// (parity with the Named path in defineMethod).
	if len(sig.TypeParams()) > 0 {
		if md.IsGetter {
			c.errorf(md.Pos(), "generic method %s.%s cannot be a getter", enumName, md.Name)
		}
		if md.IsSetter {
			c.errorf(md.Pos(), "generic method %s.%s cannot be a setter", enumName, md.Name)
		}
	}

	// Methods must have a body
	if md.Body == nil {
		c.errorf(md.Pos(), "enum method %s.%s must have a body", enumName, md.Name)
	}

	placement := types.PlaceInstance // default
	isFactory := c.hasAnnotation(md.Annotations, "factory")
	if isFactory {
		placement = types.PlaceType
	}

	m := types.NewMethod(tpos(md.Pos()), md.Name, sig, placement, false, false)
	m.SetGetter(md.IsGetter)
	m.SetSetter(md.IsSetter)
	m.SetFactory(isFactory)
	if c.hasAnnotation(md.Annotations, "public") {
		m.SetExported(true)
	}
	c.validateMetas(md.Annotations, TargetMethod)
	m.SetDoc(extractDoc(md.Annotations))
	m.SetDeprecated(extractDeprecated(md.Annotations))
	enum.AddMethod(m)
}

func (c *Checker) resolveEnumMethodSignature(enum *types.Enum, md *ast.MethodDecl) *types.Signature {
	// Open type-params scope if generic method and create TypeParam objects.
	// Mirrors resolveMethodSignature for Named types so method-level type
	// parameters (e.g. transform[U]) resolve and are carried on the signature.
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

	// Resolve receiver — enum methods receive the enum type, unless factory.
	isFactory := c.hasAnnotation(md.Annotations, "factory")
	var recv *types.Param
	if isFactory {
		recv = nil // factory methods have no receiver
	} else if md.Receiver != nil {
		ref := resolveRefMod(md.Receiver.RefMod)
		recv = types.NewParam("this", enum, ref)
	} else {
		// Methods without explicit receiver default to value receiver
		recv = types.NewParam("this", enum, types.RefNone)
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
			params[i].SetDefaultExpr(p.Default)
			c.info.ParamDefaults[params[i]] = p.Default
		}
		params[i].SetDoc(extractDoc(p.Annotations))
		params[i].SetLifetime(extractLifetime(p.Annotations))
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

	sig := types.NewSignature(recv, params, result, canError)
	if len(methodTParams) > 0 {
		sig.SetTypeParams(methodTParams)
	}
	resultLifetime := extractLifetime(md.Annotations)
	if resultLifetime != "" {
		sig.SetResultLifetime(resultLifetime)
	}
	c.validateLifetimes(sig, md.Annotations, md.Params)
	return sig
}

func (c *Checker) defineFunc(d *ast.FuncDecl) {
	// Module-level setters are stored under "name$set" in scope.
	scopeName := d.Name
	if d.IsSetter {
		scopeName = d.Name + "$set"
	}
	obj := c.scope.Lookup(scopeName)
	if obj == nil {
		return
	}
	fn, ok := obj.(*types.Func)
	if !ok {
		return
	}

	// Validate getter/setter constraints
	if d.IsGetter || d.IsSetter {
		if len(d.TypeParams) > 0 {
			kind := "getter"
			if d.IsSetter {
				kind = "setter"
			}
			c.errorf(d.Pos(), "module-level %s '%s' cannot be generic", kind, d.Name)
		}
		if d.IsGetter && len(d.Params) > 0 {
			c.errorf(d.Pos(), "module-level getter '%s' must have no parameters", d.Name)
		}
		if d.IsGetter && d.ReturnType == nil {
			c.errorf(d.Pos(), "module-level getter '%s' must have a return type", d.Name)
		}
		if d.IsSetter && len(d.Params) != 1 {
			c.errorf(d.Pos(), "module-level setter '%s' must have exactly one parameter", d.Name)
		}
		fn.SetGetter(d.IsGetter)
		fn.SetSetter(d.IsSetter)
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
		// Extract and validate timeout annotation if present
		if timeoutStr, hasTimeout := extractTestTimeout(d.Annotations); hasTimeout {
			if _, err := time.ParseDuration(timeoutStr); err != nil {
				c.errorf(d.Pos(), "invalid timeout duration %q: %v", timeoutStr, err)
			} else {
				if c.info.TestTimeouts == nil {
					c.info.TestTimeouts = make(map[string]string)
				}
				c.info.TestTimeouts[d.Name] = timeoutStr
			}
		}

		c.validateTestExclude(d.Annotations)
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
			if d.Name == "main" {
				c.errorf(d.Pos(), "`test on main() requires expected=... (use a non-main name for batch test functions)")
			}
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
			if extractTestAllowLeaks(d.Annotations) {
				if c.info.TestAllowLeaks == nil {
					c.info.TestAllowLeaks = make(map[string]bool)
				}
				c.info.TestAllowLeaks[d.Name] = true
			}
		}
	}

	// Validate `wasm_import annotation
	c.validateWasmImport(d)

	// Handle `embed annotation on getters
	if embedPath, hasEmbed := extractEmbedPath(d.Annotations); hasEmbed {
		if !d.IsGetter {
			c.errorf(d.Pos(), "`embed can only be applied to module-level getters")
		} else if d.Body != nil {
			c.errorf(d.Pos(), "`embed getter '%s' must not have a body", d.Name)
		} else if embedPath == "" {
			c.errorf(d.Pos(), "`embed annotation requires a file path: `embed(\"path/to/file\")")
		} else if filepath.IsAbs(embedPath) {
			c.errorf(d.Pos(), "`embed path must be relative, got absolute path %q", embedPath)
		} else if sig != nil && sig.CanError() {
			c.errorf(d.Pos(), "`embed getter '%s' must not be failable (embedded data cannot fail)", d.Name)
		} else if sig != nil {
			// Determine embed kind from return type
			retType := sig.Result()
			isDirPath := strings.HasSuffix(embedPath, "...")
			isGlob := !isDirPath && isGlobPattern(embedPath)
			var kind EmbedKind
			valid := true
			switch {
			case isEmbeddedFilesType(retType):
				kind = EmbedDir
				if !isDirPath && !isGlob {
					c.errorf(d.Pos(), "`embed getter '%s' with EmbeddedFiles return type requires a directory path ending with '...' or a glob pattern (e.g., `embed(\"dir/...\") or `embed(\"*.txt\"))", d.Name)
					valid = false
				}
			case types.Identical(retType, types.TypString):
				kind = EmbedString
				if isDirPath {
					c.errorf(d.Pos(), "`embed getter '%s' returning string cannot use directory path ending with '...'; use EmbeddedFiles return type", d.Name)
					valid = false
				}
				if isGlob {
					c.errorf(d.Pos(), "`embed getter '%s' returning string cannot use glob pattern; use EmbeddedFiles return type", d.Name)
					valid = false
				}
			case isU8Vector(retType):
				kind = EmbedBytes
				if isDirPath {
					c.errorf(d.Pos(), "`embed getter '%s' returning u8[] cannot use directory path ending with '...'; use EmbeddedFiles return type", d.Name)
					valid = false
				}
				if isGlob {
					c.errorf(d.Pos(), "`embed getter '%s' returning u8[] cannot use glob pattern; use EmbeddedFiles return type", d.Name)
					valid = false
				}
			default:
				c.errorf(d.Pos(), "`embed getter '%s' must return string, u8[], or EmbeddedFiles, got %s", d.Name, retType)
				valid = false
			}
			compress := extractEmbedCompress(d.Annotations)
			if compress && kind == EmbedDir {
				c.errorf(d.Pos(), "`embed getter '%s' with directory or glob path cannot use compress: true (per-file compression for directory embeds is not supported)", d.Name)
				valid = false
			}
			if valid {
				if c.info.Embeds == nil {
					c.info.Embeds = make(map[*ast.FuncDecl]*EmbedInfo)
				}
				c.info.Embeds[d] = &EmbedInfo{
					Path:     embedPath,
					Kind:     kind,
					Compress: compress,
				}
			}
		}
	}
}

// isEmbeddedFilesType returns true if typ is the EmbeddedFiles type (T0031).
func isEmbeddedFilesType(typ types.Type) bool {
	if types.TypEmbeddedFiles == nil {
		return false
	}
	if named, ok := typ.(*types.Named); ok {
		return named == types.TypEmbeddedFiles
	}
	return false
}

// isU8Vector returns true if typ is Vector[u8] (i.e., u8[]).
func isU8Vector(typ types.Type) bool {
	inst, ok := typ.(*types.Instance)
	if !ok {
		return false
	}
	named := inst.Origin()
	if named != types.TypVector {
		return false
	}
	args := inst.TypeArgs()
	return len(args) == 1 && types.Identical(args[0], types.TypU8)
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
			params[i].SetDefaultExpr(p.Default) // stored on param for cross-module lookup
			c.info.ParamDefaults[params[i]] = p.Default
		}
		params[i].SetDoc(extractDoc(p.Annotations))
		params[i].SetLifetime(extractLifetime(p.Annotations))
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

	sig := types.NewSignature(nil, params, result, canError)
	resultLifetime := extractLifetime(d.Annotations)
	if resultLifetime != "" {
		sig.SetResultLifetime(resultLifetime)
	}
	c.validateLifetimes(sig, d.Annotations, d.Params)
	return sig
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
