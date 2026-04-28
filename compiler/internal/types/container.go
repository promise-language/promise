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

// NewVector creates a Vector type as Instance{TypVector, [elem]}.
func NewVector(elem Type) *Instance {
	return NewInstance(TypVector, []Type{elem})
}

// NewMap creates a map type as Instance{TypMap, [key, val]}.
func NewMap(key, val Type) *Instance {
	return NewInstance(TypMap, []Type{key, val})
}

// IsVector reports whether t is a Vector instance (Instance{TypVector, _}).
func IsVector(t Type) bool {
	inst, ok := t.(*Instance)
	return ok && inst.origin == TypVector
}

// AsVector extracts the element type from a Vector instance or Array.
// Returns (elem, true) for Vector instances and Arrays, (nil, false) otherwise.
func AsVector(t Type) (elem Type, ok bool) {
	if inst, ok := t.(*Instance); ok && inst.origin == TypVector {
		return inst.typeArgs[0], true
	}
	if arr, ok := t.(*Array); ok {
		return arr.elem, true
	}
	return nil, false
}

// IsChannel reports whether t is a channel instance (Instance{TypChannel, _}).
func IsChannel(t Type) bool {
	inst, ok := t.(*Instance)
	return ok && inst.origin == TypChannel
}

// AsChannel extracts the element type from a channel instance.
// Returns (elem, true) for channel instances, (nil, false) otherwise.
func AsChannel(t Type) (elem Type, ok bool) {
	if inst, ok := t.(*Instance); ok && inst.origin == TypChannel {
		return inst.typeArgs[0], true
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
