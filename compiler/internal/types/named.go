package types

// Named represents a named type: user-defined types and built-in primitives alike.
// int, bool, string are Named types just like Dog and Shape.
type Named struct {
	obj        *TypeName
	typeParams []*TypeParam
	parents    []*Named // inheritance via `is`
	fields     []*Field
	methods    []*Method
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
