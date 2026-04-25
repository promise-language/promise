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
	doc        string
	deprecated string
	isTest     bool // `test meta — marks test function
}

// NewFunc creates a new function object.
func NewFunc(pos Pos, name string, sig *Signature) *Func {
	return &Func{objBase: objBase{pos: pos, name: name, typ: sig}}
}

// SetType sets the type (Signature) for this function.
// Used when the signature is resolved after initial declaration.
func (f *Func) SetType(typ Type) {
	f.typ = typ
}

func (f *Func) Doc() string            { return f.doc }
func (f *Func) SetDoc(s string)        { f.doc = s }
func (f *Func) Deprecated() string     { return f.deprecated }
func (f *Func) SetDeprecated(s string) { f.deprecated = s }
func (f *Func) IsTest() bool           { return f.isTest }
func (f *Func) SetTest(v bool)         { f.isTest = v }

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

// Module represents an imported module alias (placeholder until Stage 9 module loading).
type Module struct {
	objBase
	path string // import path, e.g. "std/io"
}

// NewModule creates a new module object.
func NewModule(pos Pos, name string, path string) *Module {
	return &Module{objBase: objBase{pos: pos, name: name}, path: path}
}

// Path returns the import path of the module.
func (m *Module) Path() string { return m.path }
