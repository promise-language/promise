package types

// Field represents a field declaration in a type.
type Field struct {
	pos         Pos
	name        string
	typ         Type
	placement   Placement
	isRaw       bool   // `raw meta — type name is an LLVM type identifier
	hasDef      bool   // has a default value expression
	isFinal     bool   // `final — immutable after construction
	exported    bool   // `public — visible to other modules
	skip        bool   // `skip — excluded from serialization
	includeNone bool   // `include_none — encode none as null instead of omitting
	required    bool   // `required — error on missing key during decode
	flatten     bool   // `flatten — inline nested fields into parent during encode/decode
	keyName     string // `key("name") — wire name override for serialization
	doc         string
	deprecated  string
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

func (f *Field) Pos() Pos               { return f.pos }
func (f *Field) Name() string           { return f.name }
func (f *Field) Type() Type             { return f.typ }
func (f *Field) Placement() Placement   { return f.placement }
func (f *Field) IsRaw() bool            { return f.isRaw }
func (f *Field) HasDefault() bool       { return f.hasDef }
func (f *Field) IsFinal() bool          { return f.isFinal }
func (f *Field) SetFinal(v bool)        { f.isFinal = v }
func (f *Field) IsExported() bool       { return f.exported }
func (f *Field) SetExported(v bool)     { f.exported = v }
func (f *Field) Doc() string            { return f.doc }
func (f *Field) SetDoc(s string)        { f.doc = s }
func (f *Field) Deprecated() string     { return f.deprecated }
func (f *Field) SetDeprecated(s string) { f.deprecated = s }
func (f *Field) Skip() bool             { return f.skip }
func (f *Field) SetSkip(v bool)         { f.skip = v }
func (f *Field) IncludeNone() bool      { return f.includeNone }
func (f *Field) SetIncludeNone(v bool)  { f.includeNone = v }
func (f *Field) Required() bool         { return f.required }
func (f *Field) SetRequired(v bool)     { f.required = v }
func (f *Field) Flatten() bool          { return f.flatten }
func (f *Field) SetFlatten(v bool)      { f.flatten = v }
func (f *Field) KeyName() string        { return f.keyName }
func (f *Field) SetKeyName(s string)    { f.keyName = s }

// Method represents a method declaration in a type.
type Method struct {
	pos        Pos
	name       string
	sig        *Signature
	placement  Placement
	abstract   bool // `abstract — no body, must be overridden
	native     bool // `native — implementation provided by runtime
	isGetter   bool // getter — accessed without (), returns value type
	isSetter   bool // setter — called on assignment to property
	isFactory  bool // `factory — static constructor, no receiver
	exported   bool // `public — visible to other modules
	doc        string
	deprecated string
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

func (m *Method) Pos() Pos               { return m.pos }
func (m *Method) Name() string           { return m.name }
func (m *Method) Sig() *Signature        { return m.sig }
func (m *Method) Placement() Placement   { return m.placement }
func (m *Method) IsAbstract() bool       { return m.abstract }
func (m *Method) IsNative() bool         { return m.native }
func (m *Method) IsGetter() bool         { return m.isGetter }
func (m *Method) IsSetter() bool         { return m.isSetter }
func (m *Method) IsFactory() bool        { return m.isFactory }
func (m *Method) SetGetter(v bool)       { m.isGetter = v }
func (m *Method) SetSetter(v bool)       { m.isSetter = v }
func (m *Method) SetFactory(v bool)      { m.isFactory = v }
func (m *Method) IsExported() bool       { return m.exported }
func (m *Method) SetExported(v bool)     { m.exported = v }
func (m *Method) Doc() string            { return m.doc }
func (m *Method) SetDoc(s string)        { m.doc = s }
func (m *Method) Deprecated() string     { return m.deprecated }
func (m *Method) SetDeprecated(s string) { m.deprecated = s }

// IsUnaryOperator reports whether this method is the prefix-unary (0-param)
// variant of an operator symbol that also has a binary form (`-`, `!`, `~`).
// Such methods get a "$unary" discriminator in their vtable slot and IR name so
// they never collide with the binary variant (T0883).
func (m *Method) IsUnaryOperator() bool {
	return IsUnaryOperatorName(m.name) && !m.isGetter && !m.isSetter && len(m.sig.Params()) == 0
}
