package types

import "strings"

// Param represents a function parameter.
type Param struct {
	name        string
	typ         Type
	ref         RefMod
	hasDef      bool        // true if parameter has a default value
	defaultExpr interface{} // AST expression for the default value (ast.Expr, stored as interface{} to avoid import cycle)
	doc         string      // `doc meta annotation
	isVariadic  bool        // true for ...T params (receives T[])
	lifetime    string      // `lifetime(name) meta annotation — explicit lifetime group
}

// NewParam creates a new parameter.
func NewParam(name string, typ Type, ref RefMod) *Param {
	return &Param{name: name, typ: typ, ref: ref}
}

func (p *Param) Name() string     { return p.name }
func (p *Param) Type() Type       { return p.typ }
func (p *Param) Ref() RefMod      { return p.ref }
func (p *Param) HasDefault() bool { return p.hasDef }
func (p *Param) Doc() string      { return p.doc }
func (p *Param) IsVariadic() bool { return p.isVariadic }

// SetHasDefault marks this parameter as having a default value.
func (p *Param) SetHasDefault(v bool) { p.hasDef = v }

// SetDefaultExpr stores the AST default expression for cross-module default lookup.
// expr should be an ast.Expr; stored as interface{} to avoid import cycle.
func (p *Param) SetDefaultExpr(expr interface{}) { p.defaultExpr = expr }

// DefaultExpr returns the stored AST default expression, or nil if not set.
// Callers in sema should cast to ast.Expr.
func (p *Param) DefaultExpr() interface{} { return p.defaultExpr }

// SetDoc sets the documentation string from a `doc annotation.
func (p *Param) SetDoc(s string) { p.doc = s }

// SetVariadic marks this parameter as variadic (...T).
func (p *Param) SetVariadic(v bool) { p.isVariadic = v }

// Lifetime returns the explicit lifetime name from a `lifetime annotation, or "".
func (p *Param) Lifetime() string { return p.lifetime }

// SetLifetime sets the explicit lifetime name from a `lifetime annotation.
func (p *Param) SetLifetime(s string) { p.lifetime = s }

// Signature represents a function type: (params) -> result.
type Signature struct {
	recv           *Param       // receiver (nil for free functions)
	params         []*Param     // positional parameters
	result         Type         // return type (nil means void)
	canError       bool         // true if function returns T! (can raise errors)
	typeParams     []*TypeParam // nil for non-generic functions
	resultLifetime string       // `lifetime(name) on function — lifetime of the return reference
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

// IsVariadic returns true if the last parameter is variadic (...T).
func (s *Signature) IsVariadic() bool {
	n := len(s.params)
	return n > 0 && s.params[n-1].IsVariadic()
}

// SetTypeParams sets the type parameters for a generic function signature.
func (s *Signature) SetTypeParams(tps []*TypeParam) { s.typeParams = tps }

// ResultLifetime returns the explicit lifetime name for the return type, or "".
func (s *Signature) ResultLifetime() string { return s.resultLifetime }

// SetResultLifetime sets the explicit lifetime name for the return type.
func (s *Signature) SetResultLifetime(l string) { s.resultLifetime = l }

func (s *Signature) String() string {
	var b strings.Builder
	b.WriteByte('(')
	for i, p := range s.params {
		if i > 0 {
			b.WriteString(", ")
		}
		if p.isVariadic {
			b.WriteString("...")
			// Variadic stores T[] internally; display the element type T.
			if elem, ok := AsVector(p.typ); ok {
				b.WriteString(elem.String())
			} else if p.typ != nil {
				b.WriteString(p.typ.String())
			}
		} else {
			if p.typ != nil {
				b.WriteString(p.typ.String())
			}
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
