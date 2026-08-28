package sema

import (
	"github.com/promise-language/promise/compiler/internal/ast"
	"github.com/promise-language/promise/compiler/internal/types"
)

// T1731: structural(protocol: true) — reserve protocol names and reject near-miss signatures.
//
// A method is a protocol near-miss (compilation error) when all three hold:
//  1. its name is a requirement of a protocol interface in scope, AND
//  2. its owning type does not satisfy that protocol, AND
//  3. its owning type satisfies NO interface in scope requiring a method of that name.
//
// Clause 3 gates this off ordinary code — a type satisfying a locally declared
// interface with the same method name is "explained" and nothing fires.

// protocolMethodKey identifies a method by name and kind (getter/setter/regular).
type protocolMethodKey struct {
	name     string
	isGetter bool
	isSetter bool
}

// validateProtocolAnnotations checks that structural(protocol: true) is only
// applied to structural types that declare at least one abstract method, and
// that protocol: true is not used on methods or enums.
func (c *Checker) validateProtocolAnnotations(file *ast.File) {
	for _, decl := range file.Decls {
		if c.info.FilteredDecls[decl] {
			continue
		}
		switch d := decl.(type) {
		case *ast.TypeDecl:
			v, found := extractProtocolParam(d.Annotations)
			if found && v {
				obj := c.declScope.Lookup(d.Name)
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
				if !named.IsAbstract() {
					c.errorf(d.Pos(), "`structural(protocol: true) requires at least one `abstract method on %s", d.Name)
				}
			}
			for _, md := range d.Methods {
				if v, found := extractProtocolParam(md.Annotations); found && v {
					c.errorf(md.Pos(), "`structural(protocol: true) can only be applied to a type, not a method")
				}
			}
		case *ast.EnumDecl:
			if v, found := extractProtocolParam(d.Annotations); found && v {
				c.errorf(d.Pos(), "`structural(protocol: true) can only be applied to a type, not an enum")
			}
			for _, md := range d.Methods {
				if v, found := extractProtocolParam(md.Annotations); found && v {
					c.errorf(md.Pos(), "`structural(protocol: true) can only be applied to a type, not a method")
				}
			}
		}
	}
}

// checkProtocolNearMisses implements the protocol near-miss check (T1731 + T1732).
// Runs after validateAbstractOverrides — all types, methods, and parent
// relationships are fully resolved.
//
// T1731: checks against protocol interfaces already in scope (std + imported modules).
// T1732: checks against protocol interfaces in unimported embedded catalog modules,
// using the trigger table to avoid unnecessary module loads.
func (c *Checker) checkProtocolNearMisses(file *ast.File) {
	// Collect all protocol interfaces in scope.
	protocols := c.collectProtocols()

	// Build method key -> protocols requiring that method (clause 1).
	protocolsByMethod := make(map[protocolMethodKey][]*types.Named)
	for _, p := range protocols {
		addAbstractMethodKeys(p, protocolsByMethod)
	}

	// Collect ALL abstract interfaces in scope (for clause 3).
	allAbstract := c.collectAbstractInterfaces()

	// Build method key -> all abstract interfaces requiring that method (clause 3).
	structuralByMethod := make(map[protocolMethodKey][]*types.Named)
	for _, s := range allAbstract {
		addAbstractMethodKeys(s, structuralByMethod)
	}

	// Check each type and enum in the file.
	for _, decl := range file.Decls {
		if c.info.FilteredDecls[decl] {
			continue
		}
		switch d := decl.(type) {
		case *ast.TypeDecl:
			c.checkTypeProtocolNearMisses(d, protocolsByMethod, structuralByMethod)
		case *ast.EnumDecl:
			c.checkEnumProtocolNearMisses(d, protocolsByMethod, structuralByMethod)
		}
	}
}

// addAbstractMethodKeys adds all abstract method keys from a structural
// interface (own + inherited) to the map.
func addAbstractMethodKeys(iface *types.Named, m map[protocolMethodKey][]*types.Named) {
	for _, method := range iface.Methods() {
		if !method.IsAbstract() {
			continue
		}
		key := protocolMethodKey{name: method.Name(), isGetter: method.IsGetter(), isSetter: method.IsSetter()}
		m[key] = appendUniqueNamed(m[key], iface)
	}
	for _, am := range iface.ParentAbstractMethods() {
		key := protocolMethodKey{name: am.Method.Name(), isGetter: am.Method.IsGetter(), isSetter: am.Method.IsSetter()}
		m[key] = appendUniqueNamed(m[key], iface)
	}
}

// appendUniqueNamed appends v to slice if not already present.
func appendUniqueNamed(slice []*types.Named, v *types.Named) []*types.Named {
	for _, s := range slice {
		if s == v {
			return slice
		}
	}
	return append(slice, v)
}

// checkTypeProtocolNearMisses checks all methods on a type declaration.
func (c *Checker) checkTypeProtocolNearMisses(td *ast.TypeDecl, protocolsByMethod, structuralByMethod map[protocolMethodKey][]*types.Named) {
	if hasProtocolOptOut(td.Annotations) {
		return
	}
	obj := c.declScope.Lookup(td.Name)
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
	// Don't check abstract types (they are declaring interface contracts,
	// not implementing one) or structural interfaces.
	if named.IsAbstract() || named.IsStructural() {
		return
	}

	for _, md := range td.Methods {
		if hasProtocolOptOut(md.Annotations) {
			continue
		}
		key := protocolMethodKey{name: md.Name, isGetter: md.IsGetter, isSetter: md.IsSetter}
		protos, hit := protocolsByMethod[key]
		if !hit {
			// T1732: no in-scope protocol requires this method name — check
			// the trigger table for unimported embedded modules.
			c.checkUnimportedTypeProtocolNearMiss(td, md, key, structuralByMethod, named)
			continue
		}

		// Clause 2: does the type satisfy any protocol requiring this method?
		satisfiesProto := false
		for _, p := range protos {
			if satisfiesProtocol(named, p) {
				satisfiesProto = true
				break
			}
		}
		if satisfiesProto {
			continue
		}

		// Clause 3: does the type satisfy ANY structural interface requiring
		// a method of this name?
		explained := false
		for _, si := range structuralByMethod[key] {
			if satisfiesProtocol(named, si) {
				explained = true
				break
			}
		}
		if explained {
			continue
		}

		// Near-miss — report against the first matching protocol.
		c.reportProtocolNearMiss(td.Name, md.Pos(), named, md, protos[0])
	}
}

// checkEnumProtocolNearMisses checks all methods on an enum declaration.
// types.Implements does not handle *Enum, so we use enumCouldSatisfyProtocol
// which performs a wildcard-based signature comparison.
func (c *Checker) checkEnumProtocolNearMisses(ed *ast.EnumDecl, protocolsByMethod, structuralByMethod map[protocolMethodKey][]*types.Named) {
	if hasProtocolOptOut(ed.Annotations) {
		return
	}
	obj := c.declScope.Lookup(ed.Name)
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

	for _, md := range ed.Methods {
		if hasProtocolOptOut(md.Annotations) {
			continue
		}
		key := protocolMethodKey{name: md.Name, isGetter: md.IsGetter, isSetter: md.IsSetter}
		protos, hit := protocolsByMethod[key]
		if !hit {
			// T1732: no in-scope protocol requires this method name — check
			// the trigger table for unimported embedded modules.
			c.checkUnimportedEnumProtocolNearMiss(ed, md, key, structuralByMethod, enum)
			continue
		}

		// Clause 2: does the enum satisfy any protocol requiring this method?
		satisfiesProto := false
		for _, p := range protos {
			if enumCouldSatisfyProtocol(enum, p) {
				satisfiesProto = true
				break
			}
		}
		if satisfiesProto {
			continue
		}

		// Clause 3: does the enum satisfy ANY interface requiring this method name?
		explained := false
		for _, si := range structuralByMethod[key] {
			if enumCouldSatisfyProtocol(enum, si) {
				explained = true
				break
			}
		}
		if explained {
			continue
		}

		c.reportEnumProtocolNearMiss(ed.Name, md.Pos(), enum, md, protos[0])
	}
}

// enumCouldSatisfyProtocol checks whether an enum could satisfy a protocol
// interface by checking all abstract methods with wildcard-based signature
// matching (since types.Implements does not handle *Enum).
func enumCouldSatisfyProtocol(enum *types.Enum, protocol *types.Named) bool {
	if !protocol.IsAbstract() {
		return false
	}
	for _, m := range protocol.Methods() {
		if !m.IsAbstract() {
			continue
		}
		if !enumMethodCouldSatisfy(enum, m, protocol) {
			return false
		}
	}
	for _, am := range protocol.ParentAbstractMethods() {
		if !enumMethodCouldSatisfy(enum, am.Method, am.Declarer) {
			return false
		}
	}
	return true
}

// enumMethodCouldSatisfy checks whether an enum has a method that could
// satisfy the given abstract method.
func enumMethodCouldSatisfy(enum *types.Enum, abstract *types.Method, declarer *types.Named) bool {
	var m *types.Method
	switch {
	case abstract.IsGetter():
		m = enum.LookupGetter(abstract.Name())
	default:
		m = enum.LookupMethod(abstract.Name())
	}
	if m == nil || m.IsAbstract() {
		return false
	}
	if abstract.IsFactory() != m.IsFactory() {
		return false
	}
	// Pass nil as replacement: typeMatchesWithWildcard treats Self as a
	// wildcard when replacement is nil, so the enum's own type (which is
	// *types.Enum, not *types.Named) can match Self-typed positions.
	return sigMatchesWithWildcards(m.Sig(), abstract.Sig(), declarer, nil)
}

// satisfiesProtocol reports whether concrete satisfies the given structural
// interface. For non-generic interfaces, delegates to types.Implements.
// For generic interfaces (with TypeParams), uses a relaxed check that treats
// the interface's type parameters as wildcards.
func satisfiesProtocol(concrete *types.Named, protocol *types.Named) bool {
	if len(protocol.TypeParams()) == 0 {
		return types.Implements(concrete, protocol)
	}
	return couldSatisfyGenericProtocol(concrete, protocol)
}

// couldSatisfyGenericProtocol checks whether the concrete type could satisfy
// a generic protocol for some instantiation of the protocol's type parameters.
// It checks that every abstract method has a matching concrete method with a
// compatible shape, treating TypeParams as wildcards.
func couldSatisfyGenericProtocol(concrete *types.Named, protocol *types.Named) bool {
	if !protocol.IsAbstract() {
		return false
	}
	// Check own abstract methods.
	for _, m := range protocol.Methods() {
		if !m.IsAbstract() {
			continue
		}
		if !methodCouldSatisfy(concrete, m, protocol) {
			return false
		}
	}
	// Check inherited abstract methods.
	for _, am := range protocol.ParentAbstractMethods() {
		if !methodCouldSatisfy(concrete, am.Method, am.Declarer) {
			return false
		}
	}
	return true
}

// methodCouldSatisfy checks whether the concrete type has a method that could
// satisfy the given abstract method, treating TypeParams as wildcards.
func methodCouldSatisfy(concrete *types.Named, abstract *types.Method, declarer *types.Named) bool {
	var m *types.Method
	switch {
	case abstract.IsGetter():
		m = concrete.LookupGetter(abstract.Name())
	case abstract.IsSetter():
		m = concrete.LookupSetter(abstract.Name())
	default:
		m = concrete.LookupMethod(abstract.Name())
	}
	if m == nil || m.IsAbstract() {
		return false
	}
	if abstract.IsFactory() != m.IsFactory() {
		return false
	}
	return sigMatchesWithWildcards(m.Sig(), abstract.Sig(), declarer, concrete)
}

// sigMatchesWithWildcards is like identicalSignaturesWithSelf but treats
// TypeParams in the abstract signature as wildcards (matching any concrete type).
// Used for generic protocol near-miss checking where we need to determine if a
// concrete method could satisfy a generic protocol for some type instantiation.
func sigMatchesWithWildcards(concrete, abstract *types.Signature, self, replacement *types.Named) bool {
	if concrete == nil || abstract == nil {
		return false
	}
	// Concrete must have at least as many params as the abstract requires.
	if len(concrete.Params()) < len(abstract.Params()) {
		return false
	}
	// Required params must match (with TypeParams as wildcards).
	for i := range abstract.Params() {
		if concrete.Params()[i].Ref() != abstract.Params()[i].Ref() {
			return false
		}
		if !typeMatchesWithWildcard(concrete.Params()[i].Type(), abstract.Params()[i].Type(), self, replacement) {
			return false
		}
	}
	// Extra concrete params must be omittable.
	for i := len(abstract.Params()); i < len(concrete.Params()); i++ {
		if concrete.Params()[i].HasDefault() {
			continue
		}
		if _, isOpt := concrete.Params()[i].Type().(*types.Optional); isOpt {
			continue
		}
		return false
	}
	// Failability: failable concrete cannot satisfy non-failable abstract.
	if concrete.CanError() && !abstract.CanError() {
		return false
	}
	// Return type.
	aResult := abstract.Result()
	cResult := concrete.Result()
	aVoid := aResult == nil || aResult == types.TypVoid
	cVoid := cResult == nil || cResult == types.TypVoid
	if aVoid && cVoid {
		return true
	}
	if aVoid || cVoid {
		return false
	}
	if typeMatchesWithWildcard(cResult, aResult, self, replacement) {
		return true
	}
	// T satisfies T? relaxation on return.
	if aOpt, ok := aResult.(*types.Optional); ok {
		if typeMatchesWithWildcard(cResult, aOpt.Elem(), self, replacement) {
			return true
		}
	}
	return false
}

// typeMatchesWithWildcard reports whether concrete matches abstract, where
// TypeParams in abstract are treated as wildcards (matching any type).
func typeMatchesWithWildcard(concrete, abstract types.Type, self, replacement *types.Named) bool {
	if abstract == nil {
		return concrete == nil
	}
	// TypeParam in abstract matches anything.
	if _, ok := abstract.(*types.TypeParam); ok {
		return true
	}
	// Self substitution. When replacement is nil (enum check), treat Self
	// as a wildcard — the enum's own type always satisfies Self.
	if n, ok := abstract.(*types.Named); ok && n == self {
		if replacement == nil {
			return true // Self matches any type in enum context
		}
		if cn, ok := concrete.(*types.Named); ok && cn == replacement {
			return true
		}
		if ci, ok := concrete.(*types.Instance); ok {
			if origin, ok := ci.Origin().(*types.Named); ok && origin == replacement {
				return true
			}
		}
	}
	// Recurse into Optional.
	if aOpt, ok := abstract.(*types.Optional); ok {
		if cOpt, ok := concrete.(*types.Optional); ok {
			return typeMatchesWithWildcard(cOpt.Elem(), aOpt.Elem(), self, replacement)
		}
		return false
	}
	// Recurse into SharedRef / MutRef.
	if aRef, ok := abstract.(*types.SharedRef); ok {
		if cRef, ok := concrete.(*types.SharedRef); ok {
			return typeMatchesWithWildcard(cRef.Elem(), aRef.Elem(), self, replacement)
		}
		return false
	}
	if aRef, ok := abstract.(*types.MutRef); ok {
		if cRef, ok := concrete.(*types.MutRef); ok {
			return typeMatchesWithWildcard(cRef.Elem(), aRef.Elem(), self, replacement)
		}
		return false
	}
	return types.Identical(concrete, abstract)
}

// collectProtocols returns all protocol interfaces visible in scope.
func (c *Checker) collectProtocols() []*types.Named {
	var result []*types.Named
	seen := make(map[*types.Named]bool)
	// Scan globScope (std as _), declScope (module's own types).
	for _, scope := range []*types.Scope{c.globScope, c.declScope} {
		for _, name := range scope.Names() {
			obj := scope.Lookup(name)
			tn, ok := obj.(*types.TypeName)
			if !ok {
				continue
			}
			named, ok := tn.Type().(*types.Named)
			if !ok {
				continue
			}
			if named.IsProtocol() && !seen[named] {
				result = append(result, named)
				seen[named] = true
			}
		}
	}
	// Scan loaded module scopes.
	for _, mod := range c.modules {
		scope := c.lookupModuleScope(mod)
		if scope == nil {
			continue
		}
		for _, name := range scope.Names() {
			obj := scope.Lookup(name)
			tn, ok := obj.(*types.TypeName)
			if !ok {
				continue
			}
			named, ok := tn.Type().(*types.Named)
			if !ok {
				continue
			}
			if named.IsProtocol() && !seen[named] {
				result = append(result, named)
				seen[named] = true
			}
		}
	}
	return result
}

// collectAbstractInterfaces returns all abstract types (structural or not)
// visible in scope. Used for clause 3: a type satisfying ANY interface
// requiring a method of that name is "explained".
func (c *Checker) collectAbstractInterfaces() []*types.Named {
	var result []*types.Named
	seen := make(map[*types.Named]bool)
	for _, scope := range []*types.Scope{c.globScope, c.declScope} {
		for _, name := range scope.Names() {
			obj := scope.Lookup(name)
			tn, ok := obj.(*types.TypeName)
			if !ok {
				continue
			}
			named, ok := tn.Type().(*types.Named)
			if !ok {
				continue
			}
			if named.IsAbstract() && !seen[named] {
				result = append(result, named)
				seen[named] = true
			}
		}
	}
	for _, mod := range c.modules {
		scope := c.lookupModuleScope(mod)
		if scope == nil {
			continue
		}
		for _, name := range scope.Names() {
			obj := scope.Lookup(name)
			tn, ok := obj.(*types.TypeName)
			if !ok {
				continue
			}
			named, ok := tn.Type().(*types.Named)
			if !ok {
				continue
			}
			if named.IsAbstract() && !seen[named] {
				result = append(result, named)
				seen[named] = true
			}
		}
	}
	return result
}

// lookupModuleScope returns the scope for a module, or nil if not loaded.
func (c *Checker) lookupModuleScope(mod *types.Module) *types.Scope {
	return mod.Scope()
}

// reportEnumProtocolNearMiss emits a near-miss diagnostic for an enum method.
func (c *Checker) reportEnumProtocolNearMiss(typeName string, pos ast.Pos, enum *types.Enum, md *ast.MethodDecl, protocol *types.Named) {
	// Find the abstract method on the protocol.
	var abstract *types.Method
	var declarer *types.Named
	for _, m := range protocol.Methods() {
		if m.IsAbstract() && m.Name() == md.Name && m.IsGetter() == md.IsGetter && m.IsSetter() == md.IsSetter {
			abstract = m
			declarer = protocol
			break
		}
	}
	if abstract == nil {
		for _, am := range protocol.ParentAbstractMethods() {
			if am.Method.Name() == md.Name && am.Method.IsGetter() == md.IsGetter && am.Method.IsSetter() == md.IsSetter {
				abstract = am.Method
				declarer = am.Declarer
				break
			}
		}
	}
	if abstract == nil {
		return
	}

	// Look up the concrete method on the enum.
	var concrete *types.Method
	switch {
	case md.IsGetter:
		concrete = enum.LookupGetter(md.Name)
	default:
		concrete = enum.LookupMethod(md.Name)
	}
	if concrete == nil {
		return
	}

	c.errorf(pos, "type %s has method '%s' matching protocol %s but does not satisfy it: incompatible signature (expected %s%s, found %s%s)",
		typeName, md.Name, declarer,
		md.Name, abstract.Sig(), md.Name, concrete.Sig())
}

// --- T1732: unimported embedded module protocol near-miss ---

// checkUnimportedTypeProtocolNearMiss checks a single type method against the
// trigger table for unimported embedded modules (T1732).
func (c *Checker) checkUnimportedTypeProtocolNearMiss(td *ast.TypeDecl, md *ast.MethodDecl, key protocolMethodKey, structuralByMethod map[protocolMethodKey][]*types.Named, named *types.Named) {
	if c.protocolTriggers == nil || c.protocolModuleLoader == nil {
		return
	}
	triggers, ok := c.protocolTriggers[key.name]
	if !ok {
		return
	}

	// Filter to triggers matching the getter/setter kind.
	var matching []ProtocolTriggerEntry
	for _, t := range triggers {
		if t.IsGetter == key.isGetter && t.IsSetter == key.isSetter {
			matching = append(matching, t)
		}
	}
	if len(matching) == 0 {
		return
	}

	// Filter out modules already loaded (handled by the in-scope T1731 path).
	var unloaded []ProtocolTriggerEntry
	for _, t := range matching {
		if _, loaded := c.moduleScopes[t.Module]; !loaded {
			unloaded = append(unloaded, t)
		}
	}
	if len(unloaded) == 0 {
		return
	}

	// Clause 3 gate: is this method name explained by ANY in-scope abstract
	// interface? If so, the name is spoken for — don't fire.
	for _, si := range structuralByMethod[key] {
		if satisfiesProtocol(named, si) {
			return
		}
	}

	// On-demand load each trigger module and check against its protocols.
	for _, t := range unloaded {
		scope := c.loadProtocolScope(t.Module)
		if scope == nil {
			continue
		}
		protocol := c.findProtocolInScope(scope, key)
		if protocol == nil {
			continue
		}
		if satisfiesProtocol(named, protocol) {
			continue
		}
		c.reportProtocolNearMissQualified(td.Name, md.Pos(), named, md, protocol, t.Module)
		return
	}
}

// checkUnimportedEnumProtocolNearMiss checks a single enum method against the
// trigger table for unimported embedded modules (T1732).
func (c *Checker) checkUnimportedEnumProtocolNearMiss(ed *ast.EnumDecl, md *ast.MethodDecl, key protocolMethodKey, structuralByMethod map[protocolMethodKey][]*types.Named, enum *types.Enum) {
	if c.protocolTriggers == nil || c.protocolModuleLoader == nil {
		return
	}
	triggers, ok := c.protocolTriggers[key.name]
	if !ok {
		return
	}

	var matching []ProtocolTriggerEntry
	for _, t := range triggers {
		if t.IsGetter == key.isGetter && t.IsSetter == key.isSetter {
			matching = append(matching, t)
		}
	}
	if len(matching) == 0 {
		return
	}

	var unloaded []ProtocolTriggerEntry
	for _, t := range matching {
		if _, loaded := c.moduleScopes[t.Module]; !loaded {
			unloaded = append(unloaded, t)
		}
	}
	if len(unloaded) == 0 {
		return
	}

	// Clause 3 gate.
	for _, si := range structuralByMethod[key] {
		if enumCouldSatisfyProtocol(enum, si) {
			return
		}
	}

	for _, t := range unloaded {
		scope := c.loadProtocolScope(t.Module)
		if scope == nil {
			continue
		}
		protocol := c.findProtocolInScope(scope, key)
		if protocol == nil {
			continue
		}
		if enumCouldSatisfyProtocol(enum, protocol) {
			continue
		}
		c.reportEnumProtocolNearMissQualified(ed.Name, md.Pos(), enum, md, protocol, t.Module)
		return
	}
}

// loadProtocolScope loads a module's declarations on demand and caches the result.
// Both successes and failures are cached to avoid retrying failed loads.
func (c *Checker) loadProtocolScope(moduleName string) *types.Scope {
	if c.loadedProtocolScopes == nil {
		c.loadedProtocolScopes = make(map[string]*types.Scope)
	}
	if scope, ok := c.loadedProtocolScopes[moduleName]; ok {
		return scope // may be nil (cached failure)
	}
	scope, err := c.protocolModuleLoader(moduleName)
	if err != nil || scope == nil {
		c.loadedProtocolScopes[moduleName] = nil // cache the failure
		return nil
	}
	c.loadedProtocolScopes[moduleName] = scope
	return scope
}

// findProtocolInScope searches a module scope for a protocol interface that
// declares an abstract method matching the given key.
func (c *Checker) findProtocolInScope(scope *types.Scope, key protocolMethodKey) *types.Named {
	for _, name := range scope.Names() {
		obj := scope.Lookup(name)
		tn, ok := obj.(*types.TypeName)
		if !ok {
			continue
		}
		named, ok := tn.Type().(*types.Named)
		if !ok {
			continue
		}
		if !named.IsProtocol() {
			continue
		}
		// Check if this protocol declares an abstract method matching the key.
		for _, m := range named.Methods() {
			if m.IsAbstract() && m.Name() == key.name && m.IsGetter() == key.isGetter && m.IsSetter() == key.isSetter {
				return named
			}
		}
		for _, am := range named.ParentAbstractMethods() {
			if am.Method.Name() == key.name && am.Method.IsGetter() == key.isGetter && am.Method.IsSetter() == key.isSetter {
				return named
			}
		}
	}
	return nil
}

// reportProtocolNearMissQualified emits a near-miss diagnostic for a type method
// against a protocol from an unimported module, qualifying the protocol name
// with the module name (e.g., "http.Handler").
func (c *Checker) reportProtocolNearMissQualified(typeName string, pos ast.Pos, named *types.Named, md *ast.MethodDecl, protocol *types.Named, moduleName string) {
	var abstract *types.Method
	for _, m := range protocol.Methods() {
		if m.IsAbstract() && m.Name() == md.Name && m.IsGetter() == md.IsGetter && m.IsSetter() == md.IsSetter {
			abstract = m
			break
		}
	}
	if abstract == nil {
		for _, am := range protocol.ParentAbstractMethods() {
			if am.Method.Name() == md.Name && am.Method.IsGetter() == md.IsGetter && am.Method.IsSetter() == md.IsSetter {
				abstract = am.Method
				break
			}
		}
	}
	if abstract == nil {
		return
	}

	var concrete *types.Method
	switch {
	case md.IsGetter:
		concrete = named.LookupGetter(md.Name)
	case md.IsSetter:
		concrete = named.LookupSetter(md.Name)
	default:
		concrete = named.LookupMethod(md.Name)
	}
	if concrete == nil {
		return
	}

	qualifiedName := moduleName + "." + protocol.Obj().Name()
	c.errorf(pos, "type %s has method '%s' matching protocol %s but does not satisfy it: incompatible signature (expected %s%s, found %s%s)",
		typeName, md.Name, qualifiedName,
		md.Name, abstract.Sig(), md.Name, concrete.Sig())
}

// reportEnumProtocolNearMissQualified emits a near-miss diagnostic for an enum
// method against a protocol from an unimported module.
func (c *Checker) reportEnumProtocolNearMissQualified(typeName string, pos ast.Pos, enum *types.Enum, md *ast.MethodDecl, protocol *types.Named, moduleName string) {
	var abstract *types.Method
	for _, m := range protocol.Methods() {
		if m.IsAbstract() && m.Name() == md.Name && m.IsGetter() == md.IsGetter && m.IsSetter() == md.IsSetter {
			abstract = m
			break
		}
	}
	if abstract == nil {
		for _, am := range protocol.ParentAbstractMethods() {
			if am.Method.Name() == md.Name && am.Method.IsGetter() == md.IsGetter && am.Method.IsSetter() == md.IsSetter {
				abstract = am.Method
				break
			}
		}
	}
	if abstract == nil {
		return
	}

	var concrete *types.Method
	switch {
	case md.IsGetter:
		concrete = enum.LookupGetter(md.Name)
	default:
		concrete = enum.LookupMethod(md.Name)
	}
	if concrete == nil {
		return
	}

	qualifiedName := moduleName + "." + protocol.Obj().Name()
	c.errorf(pos, "type %s has method '%s' matching protocol %s but does not satisfy it: incompatible signature (expected %s%s, found %s%s)",
		typeName, md.Name, qualifiedName,
		md.Name, abstract.Sig(), md.Name, concrete.Sig())
}

// reportProtocolNearMiss emits the near-miss diagnostic. Uses the same
// expected-vs-found shape as reportOverrideMismatch so the diagnostic reads
// identically to the explicit-is path.
func (c *Checker) reportProtocolNearMiss(typeName string, pos ast.Pos, named *types.Named, md *ast.MethodDecl, protocol *types.Named) {
	// Find the abstract method on the protocol that matches this method name/kind.
	var abstract *types.Method
	var declarer *types.Named
	for _, m := range protocol.Methods() {
		if m.IsAbstract() && m.Name() == md.Name && m.IsGetter() == md.IsGetter && m.IsSetter() == md.IsSetter {
			abstract = m
			declarer = protocol
			break
		}
	}
	if abstract == nil {
		for _, am := range protocol.ParentAbstractMethods() {
			if am.Method.Name() == md.Name && am.Method.IsGetter() == md.IsGetter && am.Method.IsSetter() == md.IsSetter {
				abstract = am.Method
				declarer = am.Declarer
				break
			}
		}
	}
	if abstract == nil {
		return
	}

	// Look up the concrete method for its signature.
	var concrete *types.Method
	switch {
	case md.IsGetter:
		concrete = named.LookupGetter(md.Name)
	case md.IsSetter:
		concrete = named.LookupSetter(md.Name)
	default:
		concrete = named.LookupMethod(md.Name)
	}
	if concrete == nil {
		return
	}

	// Substitute type params to show what the protocol expects for this concrete type.
	subst := c.buildParentSubstMap(named)
	substAbstract := types.Substitute(abstract.Sig(), subst).(*types.Signature)

	c.errorf(pos, "type %s has method '%s' matching protocol %s but does not satisfy it: incompatible signature (expected %s%s, found %s%s)",
		typeName, md.Name, declarer,
		md.Name, substAbstract, md.Name, concrete.Sig())
}
