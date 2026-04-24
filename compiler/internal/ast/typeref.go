package ast

// NamedTypeRef represents a named type, possibly with type arguments: Int, List[T].
type NamedTypeRef struct {
	nodeBase
	Name     string
	TypeArgs []TypeRef
}

func (*NamedTypeRef) typeRefTag() {}

// TupleTypeRef represents a tuple type: (Int, String).
type TupleTypeRef struct {
	nodeBase
	Elements []TypeRef
}

func (*TupleTypeRef) typeRefTag() {}

// FunctionTypeRef represents a function type: (Int, Int) -> Bool.
type FunctionTypeRef struct {
	nodeBase
	Params []TypeRef
	Return TypeRef
}

func (*FunctionTypeRef) typeRefTag() {}

// SharedRefTypeRef represents a shared reference type: T&.
type SharedRefTypeRef struct {
	nodeBase
	Inner TypeRef
}

func (*SharedRefTypeRef) typeRefTag() {}

// MutRefTypeRef represents a mutable reference type: T~.
type MutRefTypeRef struct {
	nodeBase
	Inner TypeRef
}

func (*MutRefTypeRef) typeRefTag() {}

// PointerTypeRef represents a pointer type: T*.
type PointerTypeRef struct {
	nodeBase
	Inner TypeRef
}

func (*PointerTypeRef) typeRefTag() {}

// OptionalTypeRef represents an optional type: T?.
type OptionalTypeRef struct {
	nodeBase
	Inner TypeRef
}

func (*OptionalTypeRef) typeRefTag() {}

// SliceTypeRef represents a slice type: T[].
type SliceTypeRef struct {
	nodeBase
	Element TypeRef
}

func (*SliceTypeRef) typeRefTag() {}

// ArrayTypeRef represents a fixed-size array type: T[N].
type ArrayTypeRef struct {
	nodeBase
	Element TypeRef
	Size    string // raw int literal text
}

func (*ArrayTypeRef) typeRefTag() {}
