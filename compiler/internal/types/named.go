package types

// ParentRef represents a parent type reference in the inheritance chain.
// For non-generic parents, TypeArgs is nil. For generic parents like
// `type Foo[T] is Bar[T]`, TypeArgs holds the type arguments applied to the parent.
type ParentRef struct {
	Named    *Named
	TypeArgs []Type // nil for non-generic parents
}

// Named represents a named type: user-defined types and built-in primitives alike.
// int, bool, string are Named types just like Dog and Shape.
type Named struct {
	obj            *TypeName
	typeParams     []*TypeParam
	parents        []*ParentRef // inheritance via `is`
	fields         []*Field
	methods        []*Method
	isCopy         bool   // `copy meta — bitwise copy on assignment
	isClone        bool   // `clone meta — auto-generate clone() Self method
	hasDrop        bool   // type has a validated drop(~this) method or needs synthesized drop
	needsSynthDrop bool   // compiler should synthesize a drop method (no explicit drop)
	hasNew         bool   // type has a validated new() constructor method
	structural     bool   // `structural meta — allows structural interface satisfaction
	exported       bool   // `public meta — visible to other modules
	isValueType    bool   // all fields are `value placement — pass by value, no heap alloc
	isSerializable bool   // `serializable meta — auto-generate encode/decode methods
	isSendable     bool   // `sendable meta — values may be moved across goroutine boundaries
	isSharable     bool   // `sharable meta — &T references may be shared across goroutines
	notSendable    bool   // `not_sendable meta — opt-out of auto-derivation
	notSharable    bool   // `not_sharable meta — opt-out of auto-derivation
	doc            string // `doc meta — documentation string
	deprecated     string // `deprecated meta — empty means not deprecated
}

// NewNamed creates a new named type and sets the TypeName's type to it.
func NewNamed(obj *TypeName, typeParams []*TypeParam) *Named {
	n := &Named{obj: obj, typeParams: typeParams}
	obj.SetType(n)
	return n
}

func (n *Named) Obj() *TypeName           { return n.obj }
func (n *Named) TypeParams() []*TypeParam { return n.typeParams }
func (n *Named) Parents() []*ParentRef    { return n.parents }
func (n *Named) Fields() []*Field         { return n.fields }
func (n *Named) Methods() []*Method       { return n.methods }
func (n *Named) Underlying() Type         { return n }
func (n *Named) IsCopy() bool             { return n.isCopy }
func (n *Named) SetCopy(v bool)           { n.isCopy = v }
func (n *Named) IsClone() bool            { return n.isClone }
func (n *Named) SetClone(v bool)          { n.isClone = v }
func (n *Named) HasDrop() bool            { return n.hasDrop }
func (n *Named) SetHasDrop(v bool)        { n.hasDrop = v }
func (n *Named) NeedsSynthDrop() bool     { return n.needsSynthDrop }
func (n *Named) SetNeedsSynthDrop(v bool) { n.needsSynthDrop = v }
func (n *Named) HasNew() bool             { return n.hasNew }
func (n *Named) SetHasNew(v bool)         { n.hasNew = v }
func (n *Named) IsStructural() bool       { return n.structural }
func (n *Named) SetStructural(v bool)     { n.structural = v }
func (n *Named) IsValueType() bool        { return n.isValueType }
func (n *Named) SetIsValueType(v bool)    { n.isValueType = v }
func (n *Named) IsSerializable() bool     { return n.isSerializable }
func (n *Named) SetSerializable(v bool)   { n.isSerializable = v }
func (n *Named) IsSendable() bool         { return n.isSendable }
func (n *Named) SetSendable(v bool)       { n.isSendable = v }
func (n *Named) IsSharable() bool         { return n.isSharable }
func (n *Named) SetSharable(v bool)       { n.isSharable = v }
func (n *Named) IsNotSendable() bool      { return n.notSendable }
func (n *Named) SetNotSendable(v bool)    { n.notSendable = v }
func (n *Named) IsNotSharable() bool      { return n.notSharable }
func (n *Named) SetNotSharable(v bool)    { n.notSharable = v }
func (n *Named) IsExported() bool         { return n.exported }
func (n *Named) SetExported(v bool)       { n.exported = v }
func (n *Named) Doc() string              { return n.doc }
func (n *Named) SetDoc(s string)          { n.doc = s }
func (n *Named) Deprecated() string       { return n.deprecated }
func (n *Named) SetDeprecated(s string)   { n.deprecated = s }

func (n *Named) String() string {
	return n.obj.Name()
}

// AddParent adds a parent type (inheritance via `is`).
// For non-generic parents, pass nil typeArgs.
func (n *Named) AddParent(parent *Named, typeArgs ...[]Type) {
	var ta []Type
	if len(typeArgs) > 0 {
		ta = typeArgs[0]
	}
	n.parents = append(n.parents, &ParentRef{Named: parent, TypeArgs: ta})
}

// InheritsFrom returns true if this type is the target type or transitively
// inherits from it. Used for error type validation (e.g., raise must produce
// a type that inherits from error).
func (n *Named) InheritsFrom(target *Named) bool {
	if n == target {
		return true
	}
	for _, p := range n.parents {
		if p.Named.InheritsFrom(target) {
			return true
		}
	}
	return false
}

// AddField adds a field to this type.
func (n *Named) AddField(f *Field) {
	n.fields = append(n.fields, f)
}

// AddMethod adds a method to this type.
func (n *Named) AddMethod(m *Method) {
	n.methods = append(n.methods, m)
}

// ResetMembers clears all fields, methods, parents, and flags on a Named type.
// Used when a universe type singleton is re-defined from source in a new sema run.
// Does NOT reset typeParams (those are part of the type's identity).
func (n *Named) ResetMembers() {
	n.parents = nil
	n.fields = nil
	n.methods = nil
	n.hasDrop = false
	n.hasNew = false
	n.isCopy = false
	n.isClone = false
	n.structural = false
	n.isValueType = false
	n.isSerializable = false
	n.exported = false
	n.doc = ""
	n.deprecated = ""
}

// NumFields returns the number of directly declared fields.
func (n *Named) NumFields() int { return len(n.fields) }

// NumMethods returns the number of directly declared methods.
func (n *Named) NumMethods() int { return len(n.methods) }

// AllFields returns all fields for this type's instance layout: inherited parent
// fields first (depth-first, concrete parent chain only), then own fields.
// If a child field shadows a parent field (same name), the parent field is omitted.
func (n *Named) AllFields() []*Field {
	var result []*Field
	// Collect inherited fields from the concrete parent chain.
	// Only the first parent with fields (or with parents that have fields) is followed.
	// Multiple concrete parents are rejected by sema.
	for _, p := range n.parents {
		pn := p.Named
		if pn.NumFields() > 0 || len(pn.parents) > 0 {
			result = append(result, pn.AllFields()...)
			break
		}
	}
	// Filter out parent fields shadowed by own fields (same name)
	ownNames := make(map[string]bool, len(n.fields))
	for _, f := range n.fields {
		ownNames[f.Name()] = true
	}
	filtered := result[:0]
	for _, f := range result {
		if !ownNames[f.Name()] {
			filtered = append(filtered, f)
		}
	}
	return append(filtered, n.fields...)
}

// LookupField searches for a field by name in this type and its parents.
// Returns nil if not found. Searches own fields first, then parents depth-first.
func (n *Named) LookupField(name string) *Field {
	for _, f := range n.fields {
		if f.name == name {
			return f
		}
	}
	for _, p := range n.parents {
		if f := p.Named.LookupField(name); f != nil {
			return f
		}
	}
	return nil
}

// LookupMethod searches for a regular method by name in this type and its parents.
// Skips getters and setters. Returns nil if not found.
func (n *Named) LookupMethod(name string) *Method {
	for _, m := range n.methods {
		if m.name == name && !m.isGetter && !m.isSetter {
			return m
		}
	}
	for _, p := range n.parents {
		if m := p.Named.LookupMethod(name); m != nil {
			return m
		}
	}
	return nil
}

// LookupUnaryMethod finds the 0-parameter (prefix-unary) variant of an operator
// method by name, walking is-parents and structural-interface parents. Distinct
// from LookupMethod, which returns the first same-named method regardless of arity
// (so it can return a binary variant when both `-`/0-param and `-`/1-param exist).
// A type may declare both a unary and a binary variant of the same operator
// symbol; the two get distinct vtable slots and IR names via the "$unary"
// discriminator (T0883).
func (n *Named) LookupUnaryMethod(name string) *Method {
	for _, m := range n.methods {
		if m.name == name && !m.isGetter && !m.isSetter && len(m.sig.Params()) == 0 {
			return m
		}
	}
	for _, p := range n.parents {
		if m := p.Named.LookupUnaryMethod(name); m != nil {
			return m
		}
	}
	return nil
}

// LookupBinaryMethod finds the 1-parameter (binary) variant of an operator method
// by name, walking is-parents and structural-interface parents. Mirror of
// LookupUnaryMethod for the binary side: when a type declares both a unary `-`
// (0 params) and a binary `-` (1 param), LookupMethod returns whichever is
// declared first, so operator dispatch must disambiguate by arity (T0883).
func (n *Named) LookupBinaryMethod(name string) *Method {
	for _, m := range n.methods {
		if m.name == name && !m.isGetter && !m.isSetter && len(m.sig.Params()) == 1 {
			return m
		}
	}
	for _, p := range n.parents {
		if m := p.Named.LookupBinaryMethod(name); m != nil {
			return m
		}
	}
	return nil
}

// LookupAnyMethod searches for any method (regular, getter, or setter) by name.
// Used by passes that need to find methods regardless of kind (e.g., codegen, returns).
func (n *Named) LookupAnyMethod(name string) *Method {
	for _, m := range n.methods {
		if m.name == name {
			return m
		}
	}
	for _, p := range n.parents {
		if m := p.Named.LookupAnyMethod(name); m != nil {
			return m
		}
	}
	return nil
}

// LookupGetter searches for a getter by name in this type and its parents.
func (n *Named) LookupGetter(name string) *Method {
	for _, m := range n.methods {
		if m.isGetter && m.name == name {
			return m
		}
	}
	for _, p := range n.parents {
		if m := p.Named.LookupGetter(name); m != nil {
			return m
		}
	}
	return nil
}

// LookupSetter searches for a setter by name in this type and its parents.
func (n *Named) LookupSetter(name string) *Method {
	for _, m := range n.methods {
		if m.isSetter && m.name == name {
			return m
		}
	}
	for _, p := range n.parents {
		if m := p.Named.LookupSetter(name); m != nil {
			return m
		}
	}
	return nil
}

// IsAbstract returns true if this type has any abstract methods,
// either directly or inherited (and not overridden).
func (n *Named) IsAbstract() bool {
	// Check own abstract methods
	for _, m := range n.methods {
		if m.abstract {
			return true
		}
	}
	// Check inherited abstract methods not overridden by a concrete method here
	for _, p := range n.parents {
		for _, am := range p.Named.allAbstractMethods() {
			own := n.lookupOwnMethodBySlotKey(methodSlotKey(am))
			if own == nil || own.abstract {
				return true
			}
		}
	}
	return false
}

// lookupOwnMethodBySlotKey searches this type's directly declared methods
// using the vtable slot key, which distinguishes getter/setter/regular methods
// with the same name.
func (n *Named) lookupOwnMethodBySlotKey(key string) *Method {
	for _, m := range n.methods {
		if methodSlotKey(m) == key {
			return m
		}
	}
	return nil
}

// IsUnaryOperatorName reports whether name is an operator symbol that has a
// prefix-unary form (negation `-`, logical not `!`, bitwise not `~`). Such a
// symbol may be declared in both a unary (0-param) and binary (1-param) variant
// on one type, so the variants must be disambiguated by arity (T0883).
func IsUnaryOperatorName(name string) bool {
	return name == "-" || name == "!" || name == "~"
}

// methodSlotKey returns a deduplication key for vtable slot assignment.
// Getter and setter with the same name occupy distinct slots, as do the unary
// and binary variants of the same operator symbol (T0883).
func methodSlotKey(m *Method) string {
	if m.isSetter {
		return m.name + "$set"
	}
	if m.IsUnaryOperator() {
		return m.name + "$unary"
	}
	return m.name
}

// AllVirtualMethods returns an ordered, deduplicated list of all non-native methods
// across the inheritance hierarchy. Parent methods come first (depth-first, left-to-right),
// then own methods. Each method slot key appears exactly once at its first-introduced position.
// A getter and setter with the same name occupy separate slots.
// Used for vtable slot assignment.
func (n *Named) AllVirtualMethods() []*Method {
	seen := make(map[string]bool)
	var result []*Method
	for _, p := range n.parents {
		for _, m := range p.Named.AllVirtualMethods() {
			key := methodSlotKey(m)
			if !seen[key] {
				seen[key] = true
				result = append(result, m)
			}
		}
	}
	for _, m := range n.methods {
		if m.IsNative() {
			continue
		}
		if len(m.Sig().TypeParams()) > 0 {
			continue // generic methods cannot be virtual — direct dispatch only
		}
		key := methodSlotKey(m)
		if !seen[key] {
			seen[key] = true
			result = append(result, m)
		}
	}
	return result
}

// VirtualMethodIndex returns the vtable slot index for a method by name and kind, or -1.
// For setters, pass isSetter=true to find the setter slot (not the getter slot).
func (n *Named) VirtualMethodIndex(name string, isSetter bool) int {
	key := name
	if isSetter {
		key = name + "$set"
	}
	for i, m := range n.AllVirtualMethods() {
		if methodSlotKey(m) == key {
			return i
		}
	}
	return -1
}

// VirtualUnaryMethodIndex returns the vtable slot index for the prefix-unary
// (0-param) variant of an operator method, or -1. Distinct from
// VirtualMethodIndex(name, false), which finds the binary variant's slot when
// both exist (T0883).
func (n *Named) VirtualUnaryMethodIndex(name string) int {
	key := name + "$unary"
	for i, m := range n.AllVirtualMethods() {
		if methodSlotKey(m) == key {
			return i
		}
	}
	return -1
}

// VirtualSlotIndexForMethod returns the vtable slot index for a specific method,
// or -1. Unlike the name-based lookups, it keys off the method's own slot,
// correctly handling operators whose slot key is plain (`++`/`--`) versus those
// that carry a `$unary` discriminator (`-`/`!`/`~`, T0883). Used for non-native
// unary operator dispatch (T0880).
func (n *Named) VirtualSlotIndexForMethod(m *Method) int {
	key := methodSlotKey(m)
	for i, vm := range n.AllVirtualMethods() {
		if methodSlotKey(vm) == key {
			return i
		}
	}
	return -1
}

// allAbstractMethods returns all abstract methods from this type
// and its parents (used internally by IsAbstract).
func (n *Named) allAbstractMethods() []*Method {
	var result []*Method
	for _, am := range n.allAbstractMethodsWithDeclarer() {
		result = append(result, am.method)
	}
	return result
}

// abstractMethodInfo pairs an abstract method with the interface that declared it.
// The declarer is the Self type that should be used for signature comparison.
type abstractMethodInfo struct {
	method   *Method
	declarer *Named
}

// allAbstractMethodsWithDeclarer returns all abstract methods from this type
// and its parents, paired with their declaring interface.
func (n *Named) allAbstractMethodsWithDeclarer() []abstractMethodInfo {
	var result []abstractMethodInfo
	for _, m := range n.methods {
		if m.abstract {
			result = append(result, abstractMethodInfo{method: m, declarer: n})
		}
	}
	for _, p := range n.parents {
		for _, am := range p.Named.allAbstractMethodsWithDeclarer() {
			own := n.lookupOwnMethodBySlotKey(methodSlotKey(am.method))
			if own == nil {
				result = append(result, am)
			}
		}
	}
	return result
}
