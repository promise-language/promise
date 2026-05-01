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
	exported   bool // `public meta — visible to other modules
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
func (f *Func) IsExported() bool       { return f.exported }
func (f *Func) SetExported(v bool)     { f.exported = v }

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

// Module represents an imported module with its exported scope.
type Module struct {
	objBase
	path        string // import path: "" for catalog, "github.com/..." or "./path" for sourced
	catalogName string // catalog module name (empty for sourced)
	scope       *Scope // the module's exported symbols
	isGlob      bool   // true if imported with `as _` (unqualified access)
}

// NewModule creates a new module object.
func NewModule(pos Pos, name string, path string) *Module {
	return &Module{objBase: objBase{pos: pos, name: name}, path: path}
}

// Path returns the import path of the module.
func (m *Module) Path() string { return m.path }

// CatalogName returns the catalog module name (empty for sourced imports).
func (m *Module) CatalogName() string { return m.catalogName }

// SetCatalogName sets the catalog module name.
func (m *Module) SetCatalogName(name string) { m.catalogName = name }

// Scope returns the module's exported symbol scope.
func (m *Module) Scope() *Scope { return m.scope }

// SetScope sets the module's exported symbol scope.
func (m *Module) SetScope(s *Scope) { m.scope = s }

// IsGlob returns true if the module was imported with `as _`.
func (m *Module) IsGlob() bool { return m.isGlob }

// SetGlob marks the module as a glob import.
func (m *Module) SetGlob(v bool) { m.isGlob = v }
