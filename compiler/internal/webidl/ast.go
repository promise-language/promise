// Package webidl provides a lexer and recursive descent parser for Web IDL
// definitions. It produces an AST that can be converted to the shared binding
// IR for Promise code generation.
package webidl

// File is the top-level AST node representing a parsed .webidl file.
type File struct {
	Interfaces   []*Interface
	Dictionaries []*Dictionary
	Enums        []*Enum
	Callbacks    []*Callback
	Typedefs     []*Typedef
	Partials     []*PartialInterface
	PartialDicts []*PartialDictionary
	Mixins       []*Mixin
	Includes     []*IncludesStatement
}

// Interface represents a WebIDL interface declaration.
type Interface struct {
	Name     string
	Parent   string // inheritance parent (empty if none)
	Members  []Member
	ExtAttrs []*ExtAttr
	Doc      string
	Pos      Pos
}

// Dictionary represents a WebIDL dictionary declaration.
type Dictionary struct {
	Name     string
	Parent   string // inheritance parent (empty if none)
	Members  []*DictMember
	ExtAttrs []*ExtAttr
	Doc      string
	Pos      Pos
}

// DictMember is a field in a dictionary.
type DictMember struct {
	Name     string
	Type     *TypeRef
	Required bool
	Default  string // default value as string (empty if none)
	Pos      Pos
}

// Enum represents a WebIDL string-valued enum.
type Enum struct {
	Name   string
	Values []string
	Doc    string
	Pos    Pos
}

// Callback represents a WebIDL callback function type.
type Callback struct {
	Name   string
	Return *TypeRef
	Params []*Param
	Doc    string
	Pos    Pos
}

// Typedef represents a WebIDL type alias.
type Typedef struct {
	Name string
	Type *TypeRef
	Doc  string
	Pos  Pos
}

// PartialInterface represents a partial interface or partial mixin definition.
type PartialInterface struct {
	Name    string
	Members []Member
	IsMixin bool
	Pos     Pos
}

// PartialDictionary represents a partial dictionary definition.
type PartialDictionary struct {
	Name    string
	Members []*DictMember
	Pos     Pos
}

// Mixin represents a WebIDL interface mixin definition.
type Mixin struct {
	Name    string
	Members []Member
	Doc     string
	Pos     Pos
}

// IncludesStatement represents: Target includes Mixin;
type IncludesStatement struct {
	Target string
	Mixin  string
	Pos    Pos
}

// Member is a marker interface for items that appear inside an interface body.
type Member interface {
	member()
}

// Attribute represents a WebIDL attribute member.
type Attribute struct {
	Name     string
	Type     *TypeRef
	Readonly bool
	Static   bool
	Doc      string
	Pos      Pos
}

func (*Attribute) member() {}

// Operation represents a WebIDL operation (method) member.
type Operation struct {
	Name    string
	Params  []*Param
	Return  *TypeRef
	Static  bool
	Special string // "getter", "setter", "deleter", "stringifier" or ""
	Doc     string
	Pos     Pos
}

func (*Operation) member() {}

// Const represents a WebIDL constant member.
type Const struct {
	Name  string
	Type  *TypeRef
	Value string
	Pos   Pos
}

func (*Const) member() {}

// Constructor represents an explicit constructor operation.
type Constructor struct {
	Params []*Param
	Doc    string
	Pos    Pos
}

func (*Constructor) member() {}

// Iterable represents an iterable<T> or iterable<K, V> declaration.
type Iterable struct {
	KeyType   *TypeRef // nil for single-type iterable
	ValueType *TypeRef
	Pos       Pos
}

func (*Iterable) member() {}

// Param is a named function parameter.
type Param struct {
	Name     string
	Type     *TypeRef
	Optional bool
	Variadic bool
	Default  string // default value (empty if none)
}

// ExtAttr represents an extended attribute like [Exposed=Window].
type ExtAttr struct {
	Name  string
	Value string // RHS of = (empty if none)
}

// TypeRef represents a type reference in WebIDL.
type TypeRef struct {
	Kind     TypeRefKind
	Builtin  string     // for BuiltinType
	Name     string     // for NamedType
	Elem     *TypeRef   // for SequenceType, FrozenArrayType, ObservableArrayType, PromiseType, NullableType
	Key      *TypeRef   // for RecordType
	Value    *TypeRef   // for RecordType
	Members  []*TypeRef // for UnionType
	Nullable bool       // T? suffix
}

// TypeRefKind distinguishes type reference variants.
type TypeRefKind int

const (
	BuiltinType TypeRefKind = iota
	NamedType
	SequenceType
	FrozenArrayType
	ObservableArrayType
	PromiseType
	RecordType
	UnionType
)

// Pos represents a source position.
type Pos struct {
	File   string
	Line   int
	Column int
}
