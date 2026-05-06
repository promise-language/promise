package types

import "strings"

// VarField represents a field within an enum variant.
type VarField struct {
	name string // empty for positional fields
	typ  Type
}

// NewVarField creates a new variant field.
func NewVarField(name string, typ Type) *VarField {
	return &VarField{name: name, typ: typ}
}

func (vf *VarField) Name() string { return vf.name }
func (vf *VarField) Type() Type   { return vf.typ }

// Variant represents a single variant of an enum.
type Variant struct {
	name   string
	fields []*VarField
	doc    string // `doc meta annotation
}

// NewVariant creates a new enum variant.
func NewVariant(name string, fields []*VarField) *Variant {
	return &Variant{name: name, fields: fields}
}

func (v *Variant) Name() string        { return v.name }
func (v *Variant) Fields() []*VarField { return v.fields }
func (v *Variant) NumFields() int      { return len(v.fields) }
func (v *Variant) Doc() string         { return v.doc }

// SetDoc sets the documentation string from a `doc annotation.
func (v *Variant) SetDoc(s string) { v.doc = s }

// Enum represents an enum/ADT type.
type Enum struct {
	obj            *TypeName
	typeParams     []*TypeParam
	variants       []*Variant
	methods        []*Method
	isCopy         bool   // `copy meta — bitwise copy on assignment
	isSerializable bool   // `serializable meta — auto-generate encode/decode
	serializeTag   string // `serializable(tag: "kind") — custom discriminator key (default "type")
	exported       bool   // `public meta — visible to other modules
	doc            string // `doc meta — documentation string
	deprecated     string // `deprecated meta — empty means not deprecated
	hasDrop        bool   // true if any variant has fields needing cleanup (T0102)
	needsSynthDrop bool   // true if compiler should synthesize a drop function (T0102)
}

// NewEnum creates a new enum type and sets the TypeName's type to it.
func NewEnum(obj *TypeName, typeParams []*TypeParam) *Enum {
	e := &Enum{obj: obj, typeParams: typeParams}
	obj.SetType(e)
	return e
}

func (e *Enum) Obj() *TypeName           { return e.obj }
func (e *Enum) TypeParams() []*TypeParam { return e.typeParams }
func (e *Enum) Variants() []*Variant     { return e.variants }
func (e *Enum) Methods() []*Method       { return e.methods }
func (e *Enum) Underlying() Type         { return e }
func (e *Enum) IsCopy() bool             { return e.isCopy }
func (e *Enum) SetCopy(v bool)           { e.isCopy = v }
func (e *Enum) IsSerializable() bool     { return e.isSerializable }
func (e *Enum) SetSerializable(v bool)   { e.isSerializable = v }
func (e *Enum) SerializeTag() string     { return e.serializeTag }
func (e *Enum) SetSerializeTag(s string) { e.serializeTag = s }
func (e *Enum) IsExported() bool         { return e.exported }
func (e *Enum) SetExported(v bool)       { e.exported = v }
func (e *Enum) Doc() string              { return e.doc }
func (e *Enum) SetDoc(s string)          { e.doc = s }
func (e *Enum) Deprecated() string       { return e.deprecated }
func (e *Enum) SetDeprecated(s string)   { e.deprecated = s }
func (e *Enum) HasDrop() bool            { return e.hasDrop }
func (e *Enum) SetHasDrop(v bool)        { e.hasDrop = v }
func (e *Enum) NeedsSynthDrop() bool     { return e.needsSynthDrop }
func (e *Enum) SetNeedsSynthDrop(v bool) { e.needsSynthDrop = v }

func (e *Enum) String() string {
	return e.obj.Name()
}

// AddVariant adds a variant to this enum.
func (e *Enum) AddVariant(v *Variant) {
	e.variants = append(e.variants, v)
}

// AddMethod adds a method to this enum.
func (e *Enum) AddMethod(m *Method) {
	e.methods = append(e.methods, m)
}

// LookupVariant searches for a variant by name.
func (e *Enum) LookupVariant(name string) *Variant {
	for _, v := range e.variants {
		if v.name == name {
			return v
		}
	}
	return nil
}

// LookupMethod searches for a non-getter, non-setter method by name.
func (e *Enum) LookupMethod(name string) *Method {
	for _, m := range e.methods {
		if m.name == name && !m.isGetter && !m.isSetter {
			return m
		}
	}
	return nil
}

// LookupGetter searches for a getter method by name.
func (e *Enum) LookupGetter(name string) *Method {
	for _, m := range e.methods {
		if m.name == name && m.isGetter {
			return m
		}
	}
	return nil
}

// LookupAnyMethod searches for any method (getter, setter, or regular) by name.
func (e *Enum) LookupAnyMethod(name string) *Method {
	for _, m := range e.methods {
		if m.name == name {
			return m
		}
	}
	return nil
}

func (v *Variant) String() string {
	if len(v.fields) == 0 {
		return v.name
	}
	var b strings.Builder
	b.WriteString(v.name)
	b.WriteByte('(')
	for i, f := range v.fields {
		if i > 0 {
			b.WriteString(", ")
		}
		if f.name != "" {
			b.WriteString(f.typ.String())
			b.WriteByte(' ')
			b.WriteString(f.name)
		} else {
			b.WriteString(f.typ.String())
		}
	}
	b.WriteByte(')')
	return b.String()
}
