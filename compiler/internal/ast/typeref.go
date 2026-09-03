package ast

// NamedTypeRef represents a named type, possibly with type arguments: Int, List[T].
type NamedTypeRef struct {
	nodeBase
	Name     string
	TypeArgs []TypeRef
}

func (*NamedTypeRef) typeRefTag() {}

// QualifiedTypeRef represents a module-qualified type: mod.Type, mod.Type[T].
type QualifiedTypeRef struct {
	nodeBase
	Module   string
	Name     string
	TypeArgs []TypeRef
}

func (*QualifiedTypeRef) typeRefTag() {}

// TupleTypeRef represents a tuple type: (Int, String).
type TupleTypeRef struct {
	nodeBase
	Elements []TypeRef
}

func (*TupleTypeRef) typeRefTag() {}

// FunctionTypeRef represents a function type: (Int, Int) -> Bool.
// CanError records the `!` prefix of a failable function type — `!(int) -> int`
// (T1634). The marker is a prefix on the producer, not a suffix on the result:
// there is no `int!` value type (§9.6, §17.2.1).
type FunctionTypeRef struct {
	nodeBase
	Params   []TypeRef
	Return   TypeRef
	CanError bool
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
