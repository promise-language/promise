package types

// Type is the interface implemented by all resolved Promise types.
type Type interface {
	// Underlying returns the underlying type.
	// For Named types, this returns the Named itself (no unwrapping).
	// For all other types, it returns the type itself.
	Underlying() Type

	// String returns a human-readable representation.
	String() string
}

// Placement indicates which of the four LLVM structs a field or method
// belongs to in the four-struct model (Value/Instance/Variant/Type).
type Placement int

const (
	PlaceInstance Placement = iota // default — heap-allocated instance struct
	PlaceValue                     // `value — copied in value struct
	PlaceVariant                   // `variant — per-monomorphization
	PlaceType                      // `type — per-declaration
)

func (p Placement) String() string {
	switch p {
	case PlaceInstance:
		return "instance"
	case PlaceValue:
		return "value"
	case PlaceVariant:
		return "variant"
	case PlaceType:
		return "type"
	default:
		return "?"
	}
}

// RefMod represents a reference modifier on function parameters.
type RefMod int

const (
	RefNone   RefMod = iota
	RefShared        // &
	RefMut           // ~
)

func (r RefMod) String() string {
	switch r {
	case RefNone:
		return ""
	case RefShared:
		return "&"
	case RefMut:
		return "~"
	default:
		return "?"
	}
}
