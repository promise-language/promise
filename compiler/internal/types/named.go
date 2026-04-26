package types

// Named represents a named type: user-defined types and built-in primitives alike.
// int, bool, string are Named types just like Dog and Shape.
type Named struct {
	obj        *TypeName
	typeParams []*TypeParam
	parents    []*Named // inheritance via `is`
	fields     []*Field
	methods    []*Method
	isCopy     bool   // `copy meta — bitwise copy on assignment
	structural bool   // `structural meta — allows structural interface satisfaction
	doc        string // `doc meta — documentation string
	deprecated string // `deprecated meta — empty means not deprecated
}

// NewNamed creates a new named type and sets the TypeName's type to it.
func NewNamed(obj *TypeName, typeParams []*TypeParam) *Named {
	n := &Named{obj: obj, typeParams: typeParams}
	obj.SetType(n)
	return n
}

func (n *Named) Obj() *TypeName           { return n.obj }
func (n *Named) TypeParams() []*TypeParam { return n.typeParams }
func (n *Named) Parents() []*Named        { return n.parents }
func (n *Named) Fields() []*Field         { return n.fields }
func (n *Named) Methods() []*Method       { return n.methods }
func (n *Named) Underlying() Type         { return n }
func (n *Named) IsCopy() bool             { return n.isCopy }
func (n *Named) SetCopy(v bool)           { n.isCopy = v }
func (n *Named) IsStructural() bool       { return n.structural }
func (n *Named) SetStructural(v bool)     { n.structural = v }
func (n *Named) Doc() string              { return n.doc }
func (n *Named) SetDoc(s string)          { n.doc = s }
func (n *Named) Deprecated() string       { return n.deprecated }
func (n *Named) SetDeprecated(s string)   { n.deprecated = s }

func (n *Named) String() string {
	return n.obj.Name()
}

// AddParent adds a parent type (inheritance via `is`).
func (n *Named) AddParent(parent *Named) {
	n.parents = append(n.parents, parent)
}

// AddField adds a field to this type.
func (n *Named) AddField(f *Field) {
	n.fields = append(n.fields, f)
}

// AddMethod adds a method to this type.
func (n *Named) AddMethod(m *Method) {
	n.methods = append(n.methods, m)
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
		if p.NumFields() > 0 || len(p.parents) > 0 {
			result = append(result, p.AllFields()...)
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
		if f := p.LookupField(name); f != nil {
			return f
		}
	}
	return nil
}

// LookupMethod searches for a method by name in this type and its parents.
// Returns nil if not found. Searches own methods first, then parents depth-first.
// Child methods override parent methods with the same name.
func (n *Named) LookupMethod(name string) *Method {
	for _, m := range n.methods {
		if m.name == name {
			return m
		}
	}
	for _, p := range n.parents {
		if m := p.LookupMethod(name); m != nil {
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
		for _, am := range p.allAbstractMethods() {
			own := n.lookupOwnMethod(am.name)
			if own == nil || own.abstract {
				return true
			}
		}
	}
	return false
}

// lookupOwnMethod searches only this type's directly declared methods.
func (n *Named) lookupOwnMethod(name string) *Method {
	for _, m := range n.methods {
		if m.name == name {
			return m
		}
	}
	return nil
}

// AllVirtualMethods returns an ordered, deduplicated list of all non-native methods
// across the inheritance hierarchy. Parent methods come first (depth-first, left-to-right),
// then own methods. Each method name appears exactly once at its first-introduced position.
// Used for vtable slot assignment.
func (n *Named) AllVirtualMethods() []*Method {
	seen := make(map[string]bool)
	var result []*Method
	for _, p := range n.parents {
		for _, m := range p.AllVirtualMethods() {
			if !seen[m.Name()] {
				seen[m.Name()] = true
				result = append(result, m)
			}
		}
	}
	for _, m := range n.methods {
		if m.IsNative() {
			continue
		}
		if !seen[m.Name()] {
			seen[m.Name()] = true
			result = append(result, m)
		}
	}
	return result
}

// VirtualMethodIndex returns the vtable slot index for a method name, or -1 if not found.
func (n *Named) VirtualMethodIndex(name string) int {
	for i, m := range n.AllVirtualMethods() {
		if m.Name() == name {
			return i
		}
	}
	return -1
}

// allAbstractMethods returns all abstract methods from this type
// and its parents (used internally by IsAbstract).
func (n *Named) allAbstractMethods() []*Method {
	var result []*Method
	for _, m := range n.methods {
		if m.abstract {
			result = append(result, m)
		}
	}
	for _, p := range n.parents {
		for _, pm := range p.allAbstractMethods() {
			// Only include if not overridden by a concrete method in n
			own := n.lookupOwnMethod(pm.name)
			if own == nil {
				result = append(result, pm)
			}
		}
	}
	return result
}
