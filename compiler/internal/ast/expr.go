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
	Move  bool // true if written `move <expr>` — consumes a named binding (§6.2)
}

// IndexExpr represents an index expression: target[index] or
// a multi-param generic instantiation: Type[A, B].
type IndexExpr struct {
	nodeBase
	Target       Expr
	Index        Expr
	ExtraIndices []Expr
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

// SliceTypeExpr represents a slice type in expression position: T[].
// Semantically equivalent to Vector[T] as a constructor reference.
type SliceTypeExpr struct {
	nodeBase
	Inner Expr
}

func (*SliceTypeExpr) exprTag() {}

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

// TypeRefExpr wraps a TypeRef for use as an Expr inside IndexExpr.Index /
// ExtraIndices when the type argument came from the typeInstCallExpr grammar
// rule (e.g. Vector[string?]() or identity[int?](42)).
type TypeRefExpr struct {
	nodeBase
	Ref TypeRef
}

func (*TypeRefExpr) exprTag() {}

// ErrorPropagateExpr represents explicit error propagation: expr?^
type ErrorPropagateExpr struct {
	nodeBase
	Expr Expr
}

func (*ErrorPropagateExpr) exprTag() {}

// ErrorPanicExpr represents error panic/forced unwrap of failable: expr?!
type ErrorPanicExpr struct {
	nodeBase
	Expr Expr
}

func (*ErrorPanicExpr) exprTag() {}

// OptionalUnwrapExpr represents optional force-unwrap: expr!
type OptionalUnwrapExpr struct {
	nodeBase
	Expr Expr
}

func (*OptionalUnwrapExpr) exprTag() {}

// AutoCloneExpr is a synth-only intrinsic that produces an owned deep copy of
// Expr. It is emitted by synthesizeCloneMethod for `clone-type fields whose
// declared type contains a TypeParam, and is lowered type-directed at mono
// codegen (copy → bit-copy, string/vector/channel/enum/heap-user → deep clone,
// optional → none-check + recurse). It is never produced by the parser. (T0605)
type AutoCloneExpr struct {
	nodeBase
	Expr Expr
}

func (*AutoCloneExpr) exprTag() {}

// ErrorHandlerExpr represents an error handler: expr ? binding { body }
// With optional type filter: expr ? binding is TypeName { body }
// With optional else clause: expr ? binding is TypeName { body } else binding { body }
// With optional panic suffix: expr ? binding is TypeName { body }!
type ErrorHandlerExpr struct {
	nodeBase
	Expr           Expr
	Binding        string    // "" if no binding, "_" for discard
	TypeName       string    // "" if untyped handler; type name for typed handler (e.g. "IoError")
	TypeArgs       []TypeRef // non-nil for generic typed handlers (e.g. DataError[string])
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
	Raw    string // numeric text without suffix, e.g. "0xFF", "42", "1_000"
	Suffix string // "" for unsuffixed, "u8", "i32", etc.
}

func (*IntLit) exprTag() {}

// FloatLit represents a floating-point literal.
type FloatLit struct {
	nodeBase
	Raw    string // numeric text without suffix
	Suffix string // "" for unsuffixed, "f32", "f64"
}

func (*FloatLit) exprTag() {}

// splitNumericSuffix splits a numeric literal token text into the numeric
// part and an optional type suffix (e.g., "42u8" → "42", "u8").
func splitNumericSuffix(text string) (raw, suffix string) {
	// Known suffixes ordered longest-first for correct matching.
	suffixes := []string{
		"i16", "i32", "i64", "u16", "u32", "u64", "f32", "f64",
		"i8", "u8",
		"i", "u",
	}
	for _, s := range suffixes {
		if len(text) > len(s) && text[len(text)-len(s):] == s {
			return text[:len(text)-len(s)], s
		}
	}
	return text, ""
}

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

// EmptyBraceLit represents a bare `{}` in expression position — never valid;
// sema emits a guiding error pointing to `{:}` / `[]` / `Set[T]()`. (T0866)
type EmptyBraceLit struct {
	nodeBase
}

func (*EmptyBraceLit) exprTag() {}

// MapEntry represents a key-value pair in a map literal.
type MapEntry struct {
	nodeBase
	Key   Expr
	Value Expr
}
