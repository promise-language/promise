// Package wit provides a lexer and recursive descent parser for WebAssembly
// Interface Type (WIT) definitions. It produces an AST that can be converted
// to the shared binding IR for Promise code generation.
package wit

// File is the top-level AST node representing a parsed .wit file.
type File struct {
	Package    *Package
	Interfaces []*Interface
	Worlds     []*World
}

// Package represents a WIT package declaration: package namespace:name@version;
type Package struct {
	Namespace string // e.g. "wasi"
	Name      string // e.g. "filesystem"
	Version   string // e.g. "0.2.0" (may be empty)
	Pos       Pos
}

// Interface represents a WIT interface block.
type Interface struct {
	Name  string
	Items []InterfaceItem
	Doc   string
	Pos   Pos
}

// InterfaceItem is a marker interface for items that appear inside an interface block.
type InterfaceItem interface {
	interfaceItem()
}

// World represents a WIT world block.
type World struct {
	Name    string
	Imports []*WorldItem
	Exports []*WorldItem
	Doc     string
	Pos     Pos
}

// WorldItem represents a single import or export in a world.
type WorldItem struct {
	Name      string // interface name or inline function name
	Interface string // fully-qualified interface reference (e.g. "wasi:filesystem/types")
	Func      *Func  // non-nil for inline function imports/exports
	Pos       Pos
}

// Func represents a WIT function declaration.
type Func struct {
	Name    string
	Kind    FuncKind
	Params  []*Param
	Results *Results
	Doc     string
	Pos     Pos
}

func (*Func) interfaceItem() {}

// FuncKind distinguishes free functions from resource methods.
type FuncKind int

const (
	FuncFree        FuncKind = iota // free function
	FuncMethod                      // method (self: borrow<R>)
	FuncStatic                      // static method
	FuncConstructor                 // constructor
)

// Param is a named function parameter.
type Param struct {
	Name string
	Type *TypeRef
}

// Results represents the return type(s) of a function.
type Results struct {
	Named []*Param // named results (rare)
	Anon  *TypeRef // anonymous single result
}

// Record represents a WIT record type.
type Record struct {
	Name   string
	Fields []*Field
	Doc    string
	Pos    Pos
}

func (*Record) interfaceItem() {}

// Field is a named field in a record.
type Field struct {
	Name string
	Type *TypeRef
}

// Variant represents a WIT variant type (tagged union).
type Variant struct {
	Name  string
	Cases []*Case
	Doc   string
	Pos   Pos
}

func (*Variant) interfaceItem() {}

// Case is a variant case with optional payload type.
type Case struct {
	Name string
	Type *TypeRef // nil if no payload
}

// Enum represents a WIT fieldless enum.
type Enum struct {
	Name  string
	Cases []string
	Doc   string
	Pos   Pos
}

func (*Enum) interfaceItem() {}

// Flags represents a WIT flags type (named bitfield).
type Flags struct {
	Name  string
	Flags []string
	Doc   string
	Pos   Pos
}

func (*Flags) interfaceItem() {}

// Resource represents a WIT resource type.
type Resource struct {
	Name    string
	Methods []*Func
	Doc     string
	Pos     Pos
}

func (*Resource) interfaceItem() {}

// TypeAlias represents a WIT type alias: type name = target;
type TypeAlias struct {
	Name   string
	Target *TypeRef
	Doc    string
	Pos    Pos
}

func (*TypeAlias) interfaceItem() {}

// Use represents a use-statement importing types from another interface.
type Use struct {
	Path  string // e.g. "wasi:io/streams"
	Names []UseName
	Pos   Pos
}

func (*Use) interfaceItem() {}

// UseName is a single type in a use-statement, with optional rename.
type UseName struct {
	Name string
	As   string // empty if not renamed
}

// TypeRef represents a type reference in WIT.
type TypeRef struct {
	Kind     TypeRefKind
	Builtin  string     // for BuiltinType
	Name     string     // for NamedType
	Elem     *TypeRef   // for ListType, OptionType, OwnType, BorrowType
	Ok       *TypeRef   // for ResultType (nil means void ok)
	Err      *TypeRef   // for ResultType (nil means void err)
	Elements []*TypeRef // for TupleType
}

// TypeRefKind distinguishes type reference variants.
type TypeRefKind int

const (
	BuiltinType TypeRefKind = iota
	NamedType
	ListType
	OptionType
	ResultType
	TupleType
	OwnType
	BorrowType
)

// Pos represents a source position.
type Pos struct {
	File   string
	Line   int
	Column int
}
