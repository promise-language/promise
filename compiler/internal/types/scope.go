package types

import "sort"

// Scope represents a lexical scope with a parent chain.
type Scope struct {
	parent   *Scope
	children []*Scope
	elems    map[string]Object
	pos, end Pos
	comment  string
}

// NewScope creates a new scope.
func NewScope(parent *Scope, pos, end Pos, comment string) *Scope {
	s := &Scope{
		parent:  parent,
		elems:   make(map[string]Object),
		pos:     pos,
		end:     end,
		comment: comment,
	}
	if parent != nil {
		parent.children = append(parent.children, s)
	}
	return s
}

func (s *Scope) Parent() *Scope     { return s.parent }
func (s *Scope) Children() []*Scope { return s.children }
func (s *Scope) Pos() Pos           { return s.pos }
func (s *Scope) End() Pos           { return s.end }
func (s *Scope) Comment() string    { return s.comment }
func (s *Scope) Len() int           { return len(s.elems) }

// Lookup searches for an object by name in this scope only.
// Returns nil if not found.
func (s *Scope) Lookup(name string) Object {
	return s.elems[name]
}

// LookupParent searches for an object by name, walking up the parent chain.
// Returns the object and the scope it was found in, or nil, nil.
func (s *Scope) LookupParent(name string) (Object, *Scope) {
	for scope := s; scope != nil; scope = scope.parent {
		if obj := scope.elems[name]; obj != nil {
			return obj, scope
		}
	}
	return nil, nil
}

// Insert inserts an object into this scope.
// If an object with the same name already exists, it returns the existing object
// and the new one is not inserted.
func (s *Scope) Insert(obj Object) Object {
	name := obj.Name()
	if existing := s.elems[name]; existing != nil {
		return existing
	}
	s.elems[name] = obj
	obj.setParent(s)
	return nil
}

// Names returns all names in this scope, sorted alphabetically.
func (s *Scope) Names() []string {
	names := make([]string, 0, len(s.elems))
	for name := range s.elems {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
