package ast

// BinaryExpr represents a binary expression: left op right.
type BinaryExpr struct {
	nodeBase
	Left  Expr
	Op    BinaryOp
	Right Expr
}

func (*BinaryExpr) exprTag() {}

// UnaryExpr represents a unary prefix expression: op operand.
type UnaryExpr struct {
	nodeBase
	Op      UnaryOp
	Operand Expr
}

func (*UnaryExpr) exprTag() {}

// CallExpr represents a function/method call: callee(args).
type CallExpr struct {
	nodeBase
	Callee Expr
	Args   []*Arg
}

func (*CallExpr) exprTag() {}

// Arg represents a call argument, optionally named.
type Arg struct {
	nodeBase
	Name  string // "" for positional
	Value Expr
}

// IndexExpr represents an index expression: target[index].
type IndexExpr struct {
	nodeBase
	Target Expr
	Index  Expr
}

func (*IndexExpr) exprTag() {}

// SliceExpr represents a slice expression: target[start:end].
type SliceExpr struct {
	nodeBase
	Target Expr
	Low    Expr // nil for [:high] and [:]
	High   Expr // nil for [low:] and [:]
}

func (*SliceExpr) exprTag() {}

// MemberExpr represents a member access: target.field.
type MemberExpr struct {
	nodeBase
	Target Expr
	Field  string
}

func (*MemberExpr) exprTag() {}

// OptionalChainExpr represents optional chaining: target?.field.
type OptionalChainExpr struct {
	nodeBase
	Target Expr
	Field  string
}

func (*OptionalChainExpr) exprTag() {}

// IsExpr represents a type check: expr is pattern.
type IsExpr struct {
	nodeBase
	Expr    Expr
	Pattern IsPattern
}

func (*IsExpr) exprTag() {}

// CastExpr represents a type cast: expr as Type or expr as! Type.
type CastExpr struct {
	nodeBase
	Expr  Expr
	Type  TypeRef
	Force bool // true for as!
}

func (*CastExpr) exprTag() {}

// ErrorPropagateExpr represents error propagation: expr?
type ErrorPropagateExpr struct {
	nodeBase
	Expr Expr
}

func (*ErrorPropagateExpr) exprTag() {}

// ErrorUnwrapExpr represents forced unwrap: expr!
type ErrorUnwrapExpr struct {
	nodeBase
	Expr Expr
}

func (*ErrorUnwrapExpr) exprTag() {}

// ErrorHandlerExpr represents an error handler: expr ? binding { body }
// With optional type filter: expr ? binding is TypeName { body }
// With optional else clause: expr ? binding is TypeName { body } else binding { body }
// With optional panic suffix: expr ? binding is TypeName { body }!
type ErrorHandlerExpr struct {
	nodeBase
	Expr           Expr
	Binding        string // "" if no binding, "_" for discard
	TypeName       string // "" if untyped handler; type name for typed handler (e.g. "IoError")
	Body           *Block
	ElseBinding    string // "" if no else binding; set when `else binding { }` present
	ElseBody       *Block // non-nil when else clause present
	PanicOnNomatch bool   // true when `!` suffix on typed handler
}

func (*ErrorHandlerExpr) exprTag() {}

// IfExpr represents an if expression (must have else): if cond { } else { }
type IfExpr struct {
	nodeBase
	Cond Expr
	Then *Block
	Else *Block
}

func (*IfExpr) exprTag() {}

// MatchExpr represents a match expression.
type MatchExpr struct {
	nodeBase
	Subject Expr
	Arms    []*MatchArm
}

func (*MatchExpr) exprTag() {}

// MatchArm represents a single arm in a match expression.
type MatchArm struct {
	nodeBase
	Pattern MatchPattern
	Guard   Expr   // nil if no guard
	Body    Expr   // expression body (one of Body/Block is set)
	Block   *Block // block body
}

// GoExpr represents a go expression: go expr or go { block }.
type GoExpr struct {
	nodeBase
	Expr  Expr   // nil if block form
	Block *Block // nil if expression form
}

func (*GoExpr) exprTag() {}

// UnsafeExpr represents an unsafe block: unsafe { body }.
type UnsafeExpr struct {
	nodeBase
	Body *Block
}

func (*UnsafeExpr) exprTag() {}

// LambdaExpr represents a lambda expression.
type LambdaExpr struct {
	nodeBase
	Move       bool
	Params     []*LambdaParam
	ReturnType TypeRef // nil if no annotation
	Body       *Block  // nil for expression body
	ExprBody   Expr    // nil for block body
}

func (*LambdaExpr) exprTag() {}

// LambdaParam represents a lambda parameter, typed or untyped.
type LambdaParam struct {
	nodeBase
	Type   TypeRef // nil for untyped
	RefMod RefModifier
	Name   string
}

// IntLit represents an integer literal.
type IntLit struct {
	nodeBase
	Raw string // original text, e.g. "0xFF", "42", "1_000"
}

func (*IntLit) exprTag() {}

// FloatLit represents a floating-point literal.
type FloatLit struct {
	nodeBase
	Raw string
}

func (*FloatLit) exprTag() {}

// BoolLit represents a boolean literal (true/false).
type BoolLit struct {
	nodeBase
	Value bool
}

func (*BoolLit) exprTag() {}

// NoneLit represents the none literal.
type NoneLit struct {
	nodeBase
}

func (*NoneLit) exprTag() {}

// CharLit represents a character literal.
type CharLit struct {
	nodeBase
	Raw string // original text including quotes
}

func (*CharLit) exprTag() {}

// StringKind distinguishes string literal types.
type StringKind int

const (
	StringRegular StringKind = iota
	StringRaw
	StringTriple
)

// StringLit represents a string literal, possibly with interpolation.
type StringLit struct {
	nodeBase
	Parts []StringPart // for regular strings with interpolation
	Raw   string       // original text including delimiters
	Kind  StringKind
}

func (*StringLit) exprTag() {}

// StringPart is a segment of an interpolated string.
type StringPart interface {
	stringPartTag()
}

// StringText is a literal text segment in a string.
type StringText struct {
	Text string
}

func (StringText) stringPartTag() {}

// StringEscape is an escape sequence segment in a string.
type StringEscape struct {
	Sequence string
}

func (StringEscape) stringPartTag() {}

// StringInterp is an interpolation segment in a string.
type StringInterp struct {
	Raw  string // text between { } (for debugging)
	Expr Expr   // parsed expression (nil if parse failed)
}

func (StringInterp) stringPartTag() {}

// IdentExpr represents an identifier used as an expression.
type IdentExpr struct {
	nodeBase
	Name string
}

func (*IdentExpr) exprTag() {}

// ThisExpr represents the this keyword.
type ThisExpr struct {
	nodeBase
}

func (*ThisExpr) exprTag() {}

// ParenExpr represents a parenthesized expression.
type ParenExpr struct {
	nodeBase
	Expr Expr
}

func (*ParenExpr) exprTag() {}

// TupleLit represents a tuple literal: (a, b, c).
type TupleLit struct {
	nodeBase
	Elements []Expr
}

func (*TupleLit) exprTag() {}

// ArrayLit represents an array literal: [a, b, c].
type ArrayLit struct {
	nodeBase
	Elements []Expr
}

func (*ArrayLit) exprTag() {}

// MapLit represents a map literal: { k: v, ... }.
type MapLit struct {
	nodeBase
	Entries []*MapEntry
}

func (*MapLit) exprTag() {}

// MapEntry represents a key-value pair in a map literal.
type MapEntry struct {
	nodeBase
	Key   Expr
	Value Expr
}
