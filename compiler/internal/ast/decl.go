package ast

// File is the root AST node for a compilation unit.
type File struct {
	nodeBase
	Uses  []*UseDecl
	Decls []Decl
}

// UseDecl represents a use (import) declaration.
type UseDecl struct {
	nodeBase
	Alias string // local name, e.g. "io"
	Path  string // import path, e.g. "std/io"
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
	IsStd       bool // true if this declaration comes from the std library
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
	IsStd       bool // true if this declaration comes from the std library
}

func (*EnumDecl) declTag() {}

// EnumVariant represents a variant inside an enum declaration.
type EnumVariant struct {
	nodeBase
	Name   string
	Fields []*EnumField // nil for fieldless variants
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
	IsStd       bool // true if this declaration comes from the std library
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
