// Package webidl provides a lexer and recursive descent parser for Web IDL
// definitions. It produces an AST that can be converted to the shared binding
// IR for Promise code generation.
package webidl

import "fmt"

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
	Namespaces   []*Namespace
	// PartialNamespaces are the partial namespace definitions, which Merge
	// applies to the namespaces of their names.
	PartialNamespaces []*Namespace
	// WindowAliases maps each identifier an interface's [LegacyWindowAlias]
	// names to that interface's name: `SVGMatrix` to `DOMMatrix`. Merge sets it
	// from the whole merged file; like Interface.Redeclared, nothing that later
	// reduces the file clears it, so an alias still names its interface after a
	// keep list drops that interface.
	WindowAliases map[string]string
}

// Append adds every definition of src to f, after f's own, so Merge sees the
// definitions of several parsed files as one file's. WindowAliases is not a
// definition: Merge sets it from the combined definitions afterwards.
func (f *File) Append(src *File) {
	f.Interfaces = append(f.Interfaces, src.Interfaces...)
	f.Dictionaries = append(f.Dictionaries, src.Dictionaries...)
	f.Enums = append(f.Enums, src.Enums...)
	f.Callbacks = append(f.Callbacks, src.Callbacks...)
	f.Typedefs = append(f.Typedefs, src.Typedefs...)
	f.Partials = append(f.Partials, src.Partials...)
	f.PartialDicts = append(f.PartialDicts, src.PartialDicts...)
	f.Mixins = append(f.Mixins, src.Mixins...)
	f.Includes = append(f.Includes, src.Includes...)
	f.Namespaces = append(f.Namespaces, src.Namespaces...)
	f.PartialNamespaces = append(f.PartialNamespaces, src.PartialNamespaces...)
}

// Interface represents a WebIDL interface declaration.
type Interface struct {
	Name     string
	Parent   string // inheritance parent (empty if none)
	Members  []Member
	ExtAttrs []*ExtAttr
	Doc      string
	Pos      Pos
	// Redeclared holds the identifiers of this interface's regular attributes
	// and operations that an interface inheriting from it, directly or not,
	// declares again as a regular attribute, regular operation or constant —
	// the members an interface prototype object carries. Merge sets it from the
	// whole merged file; nothing that later reduces the file clears it, so it
	// still describes the input after a keep list drops the redeclaring
	// interface.
	Redeclared map[string]bool
	// OverriddenBuiltIns holds the identifiers of this interface's regular
	// attributes and operations that a named property may hide on an object they
	// are called on: this interface, one it inherits from, or one inheriting
	// from it carries [LegacyOverrideBuiltIns], on its definition or a partial
	// one, so such an object's supported named properties are its own, over its
	// prototype chain. A [LegacyUnforgeable] member is never recorded: it lives
	// on the object, and a named property never hides it. Merge sets it from the
	// whole merged file; like Redeclared, nothing that later reduces the file
	// clears it.
	OverriddenBuiltIns map[string]bool
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
	Name     string
	Members  []Member
	IsMixin  bool
	ExtAttrs []*ExtAttr // written on the partial definition
	Pos      Pos
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

// Namespace represents a WebIDL namespace or partial namespace definition: an
// object whose members live on it rather than on instances. Its members are
// regular operations, readonly attributes and constants (parseNamespace).
type Namespace struct {
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
	ExtAttrs []*ExtAttr // the member's leading extended attributes
	// DeclaredBy is "maplike" or "setlike" for a member Merge expanded from
	// such a declaration, and "" for one the IDL writes.
	DeclaredBy string
	Doc        string
	Pos        Pos
}

func (*Attribute) member() {}

// Operation represents a WebIDL operation (method) member.
type Operation struct {
	Name     string
	Params   []*Param
	Return   *TypeRef
	Static   bool
	Special  string     // "getter", "setter", "deleter", "stringifier", "legacycaller" or ""
	ExtAttrs []*ExtAttr // the member's leading extended attributes
	// DeclaredBy is "maplike" or "setlike" for a member Merge expanded from
	// such a declaration, and "" for one the IDL writes.
	DeclaredBy string
	Doc        string
	Pos        Pos
}

func (*Operation) member() {}

// Const represents a WebIDL constant member.
type Const struct {
	Name  string
	Type  *TypeRef
	Value string
	Doc   string
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

// Iterable represents an iterable<T> or iterable<K, V> declaration, Async when
// written `async_iterable` (or the older `async iterable`). An async iterable's
// argument list is parsed but not kept: the declaration binds no member (#769).
type Iterable struct {
	KeyType   *TypeRef // nil for single-type iterable
	ValueType *TypeRef
	Async     bool
	Pos       Pos
}

func (*Iterable) member() {}

// Maplike represents a maplike<K, V> declaration, Readonly when written
// `readonly maplike`. Merge replaces it with the members it declares.
type Maplike struct {
	KeyType   *TypeRef
	ValueType *TypeRef
	Readonly  bool
	Doc       string
	Pos       Pos
}

func (*Maplike) member() {}

// Setlike represents a setlike<T> declaration, Readonly when written
// `readonly setlike`. Merge replaces it with the members it declares.
type Setlike struct {
	ValueType *TypeRef
	Readonly  bool
	Doc       string
	Pos       Pos
}

func (*Setlike) member() {}

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
	Elem     *TypeRef   // for SequenceType, FrozenArrayType, ObservableArrayType, PromiseType, AsyncSequenceType, NullableType
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
	AsyncSequenceType
)

// Pos represents a source position.
type Pos struct {
	File   string
	Line   int
	Column int
}

// String writes the position as file:line:column, or line:column when it names
// no file.
func (p Pos) String() string {
	if p.File != "" {
		return fmt.Sprintf("%s:%d:%d", p.File, p.Line, p.Column)
	}
	return fmt.Sprintf("%d:%d", p.Line, p.Column)
}
