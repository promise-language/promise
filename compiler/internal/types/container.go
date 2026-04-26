package types

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

// NewSlice creates a slice type as Instance{TypSlice, [elem]}.
func NewSlice(elem Type) *Instance {
	return NewInstance(TypSlice, []Type{elem})
}

// NewMap creates a map type as Instance{TypMap, [key, val]}.
func NewMap(key, val Type) *Instance {
	return NewInstance(TypMap, []Type{key, val})
}

// IsSlice reports whether t is a Slice instance (Instance{TypSlice, _}).
func IsSlice(t Type) bool {
	inst, ok := t.(*Instance)
	return ok && inst.origin == TypSlice
}

// AsSlice extracts the element type from a Slice instance or Array.
// Returns (elem, true) for Slice instances and Arrays, (nil, false) otherwise.
func AsSlice(t Type) (elem Type, ok bool) {
	if inst, ok := t.(*Instance); ok && inst.origin == TypSlice {
		return inst.typeArgs[0], true
	}
	if arr, ok := t.(*Array); ok {
		return arr.elem, true
	}
	return nil, false
}

// IsMap reports whether t is a Map instance (Instance{TypMap, _}).
func IsMap(t Type) bool {
	inst, ok := t.(*Instance)
	return ok && inst.origin == TypMap
}

// AsMap extracts key and value types from a Map instance.
// Returns (key, val, true) for Map instances, (nil, nil, false) otherwise.
func AsMap(t Type) (key, val Type, ok bool) {
	if inst, ok := t.(*Instance); ok && inst.origin == TypMap {
		return inst.typeArgs[0], inst.typeArgs[1], true
	}
	return nil, nil, false
}
