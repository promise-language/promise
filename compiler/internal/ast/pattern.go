package ast

// EnumDestructureMatchPattern: Color.Custom(r, g, b)
type EnumDestructureMatchPattern struct {
	nodeBase
	Enum     string
	Variant  string
	Bindings []string
}

func (*EnumDestructureMatchPattern) matchPatternTag() {}

// EnumVariantMatchPattern: Dir.North
type EnumVariantMatchPattern struct {
	nodeBase
	Enum    string
	Variant string
}

func (*EnumVariantMatchPattern) matchPatternTag() {}

// TypeBindingMatchPattern: Circle c
type TypeBindingMatchPattern struct {
	nodeBase
	TypeName string
	Binding  string
}

func (*TypeBindingMatchPattern) matchPatternTag() {}

// ShortDestructureMatchPattern: Ok(val)
type ShortDestructureMatchPattern struct {
	nodeBase
	Name     string
	Bindings []string
}

func (*ShortDestructureMatchPattern) matchPatternTag() {}

// NameMatchPattern: val (binds value to name)
type NameMatchPattern struct {
	nodeBase
	Name string
}

func (*NameMatchPattern) matchPatternTag() {}

// LiteralMatchPattern wraps a literal expression used as a pattern.
type LiteralMatchPattern struct {
	nodeBase
	Value Expr
}

func (*LiteralMatchPattern) matchPatternTag() {}

// WildcardMatchPattern: _
type WildcardMatchPattern struct {
	nodeBase
}

func (*WildcardMatchPattern) matchPatternTag() {}

// DestructureIsPattern: Type(a, b, c) or Type[int](a, b, c) — used with `is` operator.
type DestructureIsPattern struct {
	nodeBase
	TypeName string
	TypeArgs []TypeRef
	Bindings []string
}

func (*DestructureIsPattern) isPatternTag() {}

// IdentIsPattern: TypeName or TypeName[int] — used with `is` operator.
type IdentIsPattern struct {
	nodeBase
	Name     string
	TypeArgs []TypeRef
}

func (*IdentIsPattern) isPatternTag() {}
