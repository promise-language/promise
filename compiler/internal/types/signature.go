package types

import "strings"

// Param represents a function parameter.
type Param struct {
	name   string
	typ    Type
	ref    RefMod
	hasDef bool // true if parameter has a default value
}

// NewParam creates a new parameter.
func NewParam(name string, typ Type, ref RefMod) *Param {
	return &Param{name: name, typ: typ, ref: ref}
}

func (p *Param) Name() string     { return p.name }
func (p *Param) Type() Type       { return p.typ }
func (p *Param) Ref() RefMod      { return p.ref }
func (p *Param) HasDefault() bool { return p.hasDef }

// SetHasDefault marks this parameter as having a default value.
func (p *Param) SetHasDefault(v bool) { p.hasDef = v }

// Signature represents a function type: (params) -> result.
type Signature struct {
	recv       *Param       // receiver (nil for free functions)
	params     []*Param     // positional parameters
	result     Type         // return type (nil means void)
	canError   bool         // true if function returns T! (can raise errors)
	typeParams []*TypeParam // nil for non-generic functions
}

// NewSignature creates a new function signature.
func NewSignature(recv *Param, params []*Param, result Type, canError bool) *Signature {
	return &Signature{
		recv:     recv,
		params:   params,
		result:   result,
		canError: canError,
	}
}

func (s *Signature) Recv() *Param             { return s.recv }
func (s *Signature) Params() []*Param         { return s.params }
func (s *Signature) Result() Type             { return s.result }
func (s *Signature) CanError() bool           { return s.canError }
func (s *Signature) TypeParams() []*TypeParam { return s.typeParams }
func (s *Signature) Underlying() Type         { return s }

// SetTypeParams sets the type parameters for a generic function signature.
func (s *Signature) SetTypeParams(tps []*TypeParam) { s.typeParams = tps }

func (s *Signature) String() string {
	var b strings.Builder
	b.WriteByte('(')
	for i, p := range s.params {
		if i > 0 {
			b.WriteString(", ")
		}
		if p.typ != nil {
			b.WriteString(p.typ.String())
		}
		if p.ref != RefNone {
			b.WriteString(p.ref.String())
		}
	}
	b.WriteByte(')')
	if s.result != nil {
		b.WriteString(" -> ")
		b.WriteString(s.result.String())
	}
	if s.canError {
		b.WriteByte('!')
	}
	return b.String()
}
