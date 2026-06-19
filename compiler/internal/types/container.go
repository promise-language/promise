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

// AsVector extracts the element type from a Vector instance.
// Returns (elem, true) for Vector instances, (nil, false) otherwise.
func AsVector(t Type) (elem Type, ok bool) {
	if inst, ok := t.(*Instance); ok && inst.origin == TypVector {
		return inst.typeArgs[0], true
	}
	return nil, false
}

// AsArray extracts the element type and size from a fixed-size Array.
// Returns (elem, size, true) for Array types, (nil, 0, false) otherwise.
func AsArray(t Type) (elem Type, size int64, ok bool) {
	if arr, ok := t.(*Array); ok {
		return arr.elem, arr.size, true
	}
	return nil, 0, false
}

// NewArc creates an Arc type as Instance{TypArc, [elem]}.
func NewArc(elem Type) *Instance {
	return NewInstance(TypArc, []Type{elem})
}

// IsConfined reports whether typ's declaration is marked `confined (T0995). A
// `confined type may only be wrapped in a thread-confined Ref/Weak — one that
// uses a non-atomic counter and is rejected at goroutine boundaries.
func IsConfined(typ Type) bool {
	switch t := typ.(type) {
	case *Named:
		return t.IsConfined()
	case *Enum:
		return t.IsConfined()
	case *Instance:
		return IsConfined(t.origin)
	case *Optional:
		return IsConfined(t.Elem())
	}
	return false
}

// IsArc reports whether t is an Arc instance (Instance{TypArc, _}).
func IsArc(t Type) bool {
	inst, ok := t.(*Instance)
	return ok && inst.origin == TypArc
}

// AsArc extracts the element type from an Arc instance.
// Returns (elem, true) for Arc instances, (nil, false) otherwise.
func AsArc(t Type) (elem Type, ok bool) {
	if inst, ok := t.(*Instance); ok && inst.origin == TypArc {
		return inst.typeArgs[0], true
	}
	return nil, false
}

// NewWeak creates a Weak type as Instance{TypWeak, [elem]}.
func NewWeak(elem Type) *Instance {
	return NewInstance(TypWeak, []Type{elem})
}

// IsWeak reports whether t is a Weak instance (Instance{TypWeak, _}).
func IsWeak(t Type) bool {
	inst, ok := t.(*Instance)
	return ok && inst.origin == TypWeak
}

// AsWeak extracts the element type from a Weak instance.
// Returns (elem, true) for Weak instances, (nil, false) otherwise.
func AsWeak(t Type) (elem Type, ok bool) {
	if inst, ok := t.(*Instance); ok && inst.origin == TypWeak {
		return inst.typeArgs[0], true
	}
	return nil, false
}

// NewMutex creates a Mutex type as Instance{TypMutex, [elem]}.
func NewMutex(elem Type) *Instance {
	return NewInstance(TypMutex, []Type{elem})
}

// IsMutex reports whether t is a Mutex instance (Instance{TypMutex, _}).
func IsMutex(t Type) bool {
	inst, ok := t.(*Instance)
	return ok && inst.origin == TypMutex
}

// AsMutex extracts the element type from a Mutex instance.
// Returns (elem, true) for Mutex instances, (nil, false) otherwise.
func AsMutex(t Type) (elem Type, ok bool) {
	if inst, ok := t.(*Instance); ok && inst.origin == TypMutex {
		return inst.typeArgs[0], true
	}
	return nil, false
}

// NewMutexGuard creates a MutexGuard type as Instance{TypMutexGuard, [elem]}.
func NewMutexGuard(elem Type) *Instance {
	return NewInstance(TypMutexGuard, []Type{elem})
}

// IsMutexGuard reports whether t is a MutexGuard instance (Instance{TypMutexGuard, _}).
func IsMutexGuard(t Type) bool {
	inst, ok := t.(*Instance)
	return ok && inst.origin == TypMutexGuard
}

// AsMutexGuard extracts the element type from a MutexGuard instance.
// Returns (elem, true) for MutexGuard instances, (nil, false) otherwise.
func AsMutexGuard(t Type) (elem Type, ok bool) {
	if inst, ok := t.(*Instance); ok && inst.origin == TypMutexGuard {
		return inst.typeArgs[0], true
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

// IsTask reports whether t is a Task instance (Instance{TypTask, _}).
func IsTask(t Type) bool {
	inst, ok := t.(*Instance)
	return ok && inst.origin == TypTask
}

// AsTask extracts the element type from a Task instance.
// Returns (elem, true) for Task instances, (nil, false) otherwise.
func AsTask(t Type) (elem Type, ok bool) {
	if inst, ok := t.(*Instance); ok && inst.origin == TypTask {
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

// AsIterator extracts the element type from an Iterator instance.
// Returns (elem, true) for Iterator instances, (nil, false) otherwise.
func AsIterator(t Type) (elem Type, ok bool) {
	if inst, ok := t.(*Instance); ok && inst.origin == TypIter {
		return inst.typeArgs[0], true
	}
	return nil, false
}

// AsRange extracts the element type from a Range instance.
// Returns (elem, true) for Range instances, (nil, false) otherwise.
func AsRange(t Type) (elem Type, ok bool) {
	if inst, ok := t.(*Instance); ok && inst.origin == TypRange {
		return inst.typeArgs[0], true
	}
	return nil, false
}

// AsStream extracts the element type from a Stream instance.
// Returns (elem, true) for Stream instances, (nil, false) otherwise.
func AsStream(t Type) (elem Type, ok bool) {
	if inst, ok := t.(*Instance); ok && inst.origin == TypStream {
		return inst.typeArgs[0], true
	}
	return nil, false
}
