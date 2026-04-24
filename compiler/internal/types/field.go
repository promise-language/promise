package types

// Field represents a field declaration in a type.
type Field struct {
	pos       Pos
	name      string
	typ       Type
	placement Placement
	isRaw     bool // `raw meta — type name is an LLVM type identifier
	hasDef    bool // has a default value expression
}

// NewField creates a new field.
func NewField(pos Pos, name string, typ Type, placement Placement, isRaw, hasDef bool) *Field {
	return &Field{
		pos:       pos,
		name:      name,
		typ:       typ,
		placement: placement,
		isRaw:     isRaw,
		hasDef:    hasDef,
	}
}

func (f *Field) Pos() Pos             { return f.pos }
func (f *Field) Name() string         { return f.name }
func (f *Field) Type() Type           { return f.typ }
func (f *Field) Placement() Placement { return f.placement }
func (f *Field) IsRaw() bool          { return f.isRaw }
func (f *Field) HasDefault() bool     { return f.hasDef }

// Method represents a method declaration in a type.
type Method struct {
	pos       Pos
	name      string
	sig       *Signature
	placement Placement
	abstract  bool // `abstract — no body, must be overridden
	native    bool // `native — implementation provided by runtime
}

// NewMethod creates a new method.
func NewMethod(pos Pos, name string, sig *Signature, placement Placement, abstract, native bool) *Method {
	return &Method{
		pos:       pos,
		name:      name,
		sig:       sig,
		placement: placement,
		abstract:  abstract,
		native:    native,
	}
}

func (m *Method) Pos() Pos             { return m.pos }
func (m *Method) Name() string         { return m.name }
func (m *Method) Sig() *Signature      { return m.sig }
func (m *Method) Placement() Placement { return m.placement }
func (m *Method) IsAbstract() bool     { return m.abstract }
func (m *Method) IsNative() bool       { return m.native }
