package types

// TODO(stage-8j): Unify compound container types (Slice, Array, Map) with Named types.
//
// Currently compound types are minimal structs with no fields, methods, inheritance,
// type parameters, or documentation. This creates an asymmetry where:
//   - Named types support methods, fields, inheritance, doc, deprecation, placement
//   - Compound types are hard-coded with special-case handling throughout sema and codegen
//     (e.g. .len is wired as a built-in property, not a real field/method)
//
// Promoting compound types to Named types (or a shared base) would:
//   - Allow user-defined methods and inheritance on containers
//   - Provide a natural place for documentation and IDE support (hover, signature help)
//   - Eliminate special-case branches in sema/expr.go and codegen/expr.go
//   - Enable a single codepath for field/method lookup across all types
//
// See Stage 8j in docs/stages.md for the full plan.

import (
	"fmt"
	"strings"
)

// Tuple represents a tuple type: (T1, T2, ...).
type Tuple struct {
	elems []Type
}

// NewTuple creates a new tuple type.
func NewTuple(elems []Type) *Tuple {
	return &Tuple{elems: elems}
}

func (t *Tuple) Elems() []Type    { return t.elems }
func (t *Tuple) Underlying() Type { return t }

func (t *Tuple) String() string {
	var b strings.Builder
	b.WriteByte('(')
	for i, e := range t.elems {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(e.String())
	}
	b.WriteByte(')')
	return b.String()
}

// Array represents a fixed-size array type: T[N].
type Array struct {
	elem Type
	size int64
}

// NewArray creates a new array type.
func NewArray(elem Type, size int64) *Array {
	return &Array{elem: elem, size: size}
}

func (a *Array) Elem() Type       { return a.elem }
func (a *Array) Size() int64      { return a.size }
func (a *Array) Underlying() Type { return a }

func (a *Array) String() string {
	return fmt.Sprintf("%s[%d]", a.elem.String(), a.size)
}

// Slice represents a dynamic slice type: T[].
type Slice struct {
	elem Type
}

// NewSlice creates a new slice type.
func NewSlice(elem Type) *Slice {
	return &Slice{elem: elem}
}

func (s *Slice) Elem() Type       { return s.elem }
func (s *Slice) Underlying() Type { return s }

func (s *Slice) String() string {
	return s.elem.String() + "[]"
}

// Map represents a map type: Map[K, V].
type Map struct {
	key Type
	val Type
}

// NewMap creates a new map type.
func NewMap(key, val Type) *Map {
	return &Map{key: key, val: val}
}

func (m *Map) Key() Type        { return m.key }
func (m *Map) Val() Type        { return m.val }
func (m *Map) Underlying() Type { return m }

func (m *Map) String() string {
	return fmt.Sprintf("Map[%s, %s]", m.key.String(), m.val.String())
}
