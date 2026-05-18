// Package bindgen provides a shared intermediate representation for WASM
// binding generation and a Promise code generator. The IR abstracts over
// source IDL formats (WIT, WebIDL) so the code generator is format-agnostic.
package bindgen

// Module represents a single binding module (one per WIT interface).
type Module struct {
	Name         string     // Promise module name (e.g. "wasi")
	ImportModule string     // WASM import module name (e.g. "wasi_snapshot_preview1")
	Types        []Type     // type definitions
	Functions    []Func     // free functions
	Resources    []Resource // resource types
	HasJsValue   bool       // true if any type reference resolves to JsValue
}

// Type represents a type definition in the binding IR.
type Type struct {
	Name   string
	Kind   TypeKind
	Fields []Field  // for Record, Flags
	Cases  []Case   // for Variant, Enum
	Target *TypeRef // for Alias
	Doc    string
}

// TypeKind distinguishes the shape of a type definition.
type TypeKind int

const (
	TypeRecord  TypeKind = iota // struct-like with named fields
	TypeEnum                    // fieldless enumeration
	TypeVariant                 // tagged union with optional payloads
	TypeFlags                   // named bitfield
	TypeAlias                   // type alias
)

// Field is a named field in a record or flags type.
type Field struct {
	Name string
	Type TypeRef
}

// Case is a variant or enum case.
type Case struct {
	Name string
	Type *TypeRef // nil for fieldless enum cases
}

// Func represents a function in the binding IR.
type Func struct {
	Name       string
	Params     []Param
	Results    []TypeRef
	ImportName string // WASM import name
	Kind       FuncKind
	OwnerType  string // for methods/constructors/statics
	Doc        string
}

// FuncKind distinguishes free functions from resource methods.
type FuncKind int

const (
	FuncFree FuncKind = iota
	FuncMethod
	FuncConstructor
	FuncStatic
)

// Param is a named function parameter.
type Param struct {
	Name string
	Type TypeRef
}

// Resource represents a resource type with methods.
type Resource struct {
	Name    string
	Methods []Func
	Drop    bool // has destructor
	Doc     string
}

// TypeRef represents a type reference.
type TypeRef struct {
	Kind     TypeRefKind
	Builtin  string    // for BuiltinKind
	Name     string    // for NamedKind
	Elem     *TypeRef  // for ListKind, OptionKind, OwnKind, BorrowKind
	Ok       *TypeRef  // for ResultKind (nil = void)
	Err      *TypeRef  // for ResultKind (nil = void)
	Elements []TypeRef // for TupleKind
}

// TypeRefKind distinguishes type reference shapes.
type TypeRefKind int

const (
	BuiltinKind TypeRefKind = iota
	NamedKind
	ListKind
	OptionKind
	ResultKind
	TupleKind
	OwnKind
	BorrowKind
)
