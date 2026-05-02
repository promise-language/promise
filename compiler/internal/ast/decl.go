package ast

// File is the root AST node for a compilation unit.
type File struct {
	nodeBase
	Uses  []*UseDecl
	Decls []Decl
}

// UseDecl represents a use (import) declaration.
// Two forms:
//   - Catalog:  use json; / use json as j; / use json as _;
//   - Sourced:  use parser "github.com/..."; / use _ "github.com/...";
type UseDecl struct {
	nodeBase
	Alias string // effective local name: "json", "j", "_" (glob import)
	Path  string // "" for catalog, otherwise location string (URL or path)
	// CatalogName is the catalog module name (first IDENT in catalog form).
	// Empty for sourced imports.
	CatalogName string
}

func (*UseDecl) declTag() {}

// TypeDecl represents a type declaration.
type TypeDecl struct {
	nodeBase
	Name        string
	TypeParams  []*TypeParam
	Inherits    []TypeRef
	Annotations []*MetaAnnotation
	Fields      []*FieldDecl
	Methods     []*MethodDecl
}

func (*TypeDecl) declTag() {}

// FieldDecl represents a field inside a type declaration.
type FieldDecl struct {
	nodeBase
	Type        TypeRef
	Name        string
	Annotations []*MetaAnnotation
	Default     Expr // nil if no default value
}

// MethodDecl represents a method inside a type declaration.
// Getters and setters are also represented as MethodDecl with IsGetter/IsSetter flags.
type MethodDecl struct {
	nodeBase
	Name        string // identifier or operator symbol like "+"
	TypeParams  []*TypeParam
	Receiver    *ReceiverParam // nil if no receiver
	Params      []*Param
	ReturnType  *ReturnTypeSpec // nil if no return type
	Annotations []*MetaAnnotation
	Body        *Block // nil for abstract methods
	IsGetter    bool   // true for getter declarations (get name Type { ... })
	IsSetter    bool   // true for setter declarations (set name(Type param) { ... })
}

// EnumDecl represents an enum declaration.
type EnumDecl struct {
	nodeBase
	Name        string
	TypeParams  []*TypeParam
	Annotations []*MetaAnnotation
	Variants    []*EnumVariant
}

func (*EnumDecl) declTag() {}

// EnumVariant represents a variant inside an enum declaration.
type EnumVariant struct {
	nodeBase
	Name        string
	Fields      []*EnumField      // nil for fieldless variants
	Annotations []*MetaAnnotation // `doc, `deprecated on variants
}

// EnumField represents a field inside an enum variant.
type EnumField struct {
	nodeBase
	Type TypeRef
	Name string
}

// FuncDecl represents a top-level function declaration.
type FuncDecl struct {
	nodeBase
	Name        string
	TypeParams  []*TypeParam
	Params      []*Param
	ReturnType  *ReturnTypeSpec // nil if no return type
	Annotations []*MetaAnnotation
	Body        *Block
}

func (*FuncDecl) declTag() {}

// TypeParam represents a generic type parameter.
type TypeParam struct {
	nodeBase
	Name       string
	Constraint []TypeRef // from typeConstraint: T: A + B → [A, B]
}

// ReturnTypeSpec represents a function return type with optional error marker.
type ReturnTypeSpec struct {
	nodeBase
	Type     TypeRef
	CanError bool // true if trailing !
}

// Param represents a function/method parameter.
type Param struct {
	nodeBase
	Type        TypeRef
	RefMod      RefModifier
	Name        string // "_" for discard
	Annotations []*MetaAnnotation
	Default     Expr // nil if no default
	IsVariadic  bool // true for ...T params
}

// ReceiverParam represents a method receiver (this).
type ReceiverParam struct {
	nodeBase
	RefMod RefModifier // & or ~ or none
}

// MetaAnnotation represents a backtick annotation like `deprecated.
type MetaAnnotation struct {
	nodeBase
	Name   string
	Params []*MetaParam
}

// MetaParam represents a parameter inside a meta annotation.
type MetaParam struct {
	nodeBase
	Name  string // "" for positional
	Value Expr
}
