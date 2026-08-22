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

	// returnHoldsReceiver (T1349) is true when a `return E` in the body returns a
	// value whose subtree contains a lambda that captures `this` — the
	// `_FnIter[T](_next: <lambda capturing this>)` shape used by `Vector.iter()`
	// and every `Iterator` combinator (map/filter/flat_map/…). Combined with the
	// receiver's ref-kind it lets ownership decide, at each call site, whether the
	// returned iterator borrows (RefNone/&this) or owns (~this) the receiver — the
	// basis for rejecting an iterator that borrows a local and escapes its scope.
	returnHoldsReceiver bool
}

// NewSignature creates a new function signature. A TypVoid result is normalized
// to nil (T1634): "returns nothing" gets exactly one representation, so a
// signature written `(int) -> void` and one inferred from a void-typed lambda
// body compare Identical. Every construction site — sema/decl.go,
// sema/resolve.go, sema/expr.go, types/subst.go, codegen/stmt_forin.go — routes
// through here. TypVoid used as a *value* type elsewhere (a go-block result, an
// enum-field fallback, a `Task[void]` element) never passes through this
// function and is untouched.
func NewSignature(recv *Param, params []*Param, result Type, canError bool) *Signature {
	if result == Type(TypVoid) {
		result = nil
	}
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

// ReturnHoldsReceiver reports whether the body returns a value that holds its
// receiver via a captured-`this` lambda (T1349). See the field comment.
func (s *Signature) ReturnHoldsReceiver() bool { return s.returnHoldsReceiver }

// SetReturnHoldsReceiver records the T1349 return-holds-receiver fact.
func (s *Signature) SetReturnHoldsReceiver(v bool) { s.returnHoldsReceiver = v }

func (s *Signature) String() string {
	var b strings.Builder
	if s.canError {
		// T1445: the failability marker sits on the producer, not on the result —
		// §9.6 spells it `!(int) -> int`, never `(int) -> int!` (there is no `int!`
		// value type, §17.2.1).
		b.WriteByte('!')
	}
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
	b.WriteString(" -> ")
	if s.result != nil {
		b.WriteString(s.result.String())
	} else {
		// T1634: a nil result is "returns nothing" — render it as `void` rather
		// than dropping the arrow, which made `(int) -> void` print as the bare
		// `(int)` and read as a tuple in diagnostics.
		b.WriteString("void")
	}
	return b.String()
}
