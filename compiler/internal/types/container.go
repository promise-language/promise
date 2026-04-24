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
