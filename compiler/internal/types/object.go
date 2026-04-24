package types

// Object represents a named entity in a scope: variable, function, type, or label.
type Object interface {
	Name() string
	Type() Type
	Pos() Pos
	Parent() *Scope
	setParent(*Scope)
}

// objBase holds fields common to all Object implementations.
type objBase struct {
	name   string
	typ    Type
	pos    Pos
	parent *Scope
}

func (o *objBase) Name() string       { return o.name }
func (o *objBase) Type() Type         { return o.typ }
func (o *objBase) Pos() Pos           { return o.pos }
func (o *objBase) Parent() *Scope     { return o.parent }
func (o *objBase) setParent(s *Scope) { o.parent = s }

// Var represents a variable or field binding.
type Var struct {
	objBase
}

// NewVar creates a new variable object.
func NewVar(pos Pos, name string, typ Type) *Var {
	return &Var{objBase: objBase{pos: pos, name: name, typ: typ}}
}

// Func represents a function declaration.
type Func struct {
	objBase
}

// NewFunc creates a new function object.
func NewFunc(pos Pos, name string, sig *Signature) *Func {
	return &Func{objBase: objBase{pos: pos, name: name, typ: sig}}
}

// TypeName represents a type declaration.
type TypeName struct {
	objBase
}

// NewTypeName creates a new type name object.
// The typ field is set later via SetType once the full type is constructed.
func NewTypeName(pos Pos, name string, typ Type) *TypeName {
	return &TypeName{objBase: objBase{pos: pos, name: name, typ: typ}}
}

// SetType sets the type for this type name. Used during construction
// when the Named/Enum type is created after the TypeName.
func (tn *TypeName) SetType(typ Type) {
	tn.typ = typ
}

// Label represents a break/continue target.
type Label struct {
	objBase
}

// NewLabel creates a new label object.
func NewLabel(pos Pos, name string) *Label {
	return &Label{objBase: objBase{pos: pos, name: name}}
}
