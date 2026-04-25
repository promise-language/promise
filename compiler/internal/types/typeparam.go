package types

import "strings"

// TypeParam represents a generic type parameter: T, K: Hashable, etc.
type TypeParam struct {
	obj         *TypeName
	constraints []Type // nil means unconstrained; supports T: A + B
	index       int    // position in the type parameter list
}

// NewTypeParam creates a new type parameter.
func NewTypeParam(obj *TypeName, constraint Type, index int) *TypeParam {
	tp := &TypeParam{obj: obj, index: index}
	if constraint != nil {
		tp.constraints = []Type{constraint}
	}
	obj.SetType(tp)
	return tp
}

func (tp *TypeParam) Obj() *TypeName { return tp.obj }

// Constraint returns the first constraint or nil. For single-constraint callers.
func (tp *TypeParam) Constraint() Type {
	if len(tp.constraints) == 0 {
		return nil
	}
	return tp.constraints[0]
}

// Constraints returns all constraints (may be nil for unconstrained).
func (tp *TypeParam) Constraints() []Type { return tp.constraints }
func (tp *TypeParam) Index() int          { return tp.index }
func (tp *TypeParam) Underlying() Type    { return tp }

// SetConstraint sets a single constraint for this type parameter.
func (tp *TypeParam) SetConstraint(c Type) {
	if c == nil {
		tp.constraints = nil
	} else {
		tp.constraints = []Type{c}
	}
}

// SetConstraints sets multiple constraints for this type parameter.
func (tp *TypeParam) SetConstraints(cs []Type) {
	tp.constraints = cs
}

func (tp *TypeParam) String() string {
	return tp.obj.Name()
}

// Instance represents an instantiated generic type: List[int], Map[string, int].
type Instance struct {
	origin   Type   // the uninstantiated *Named or *Enum
	typeArgs []Type // concrete type arguments
}

// NewInstance creates a new generic instantiation.
func NewInstance(origin Type, typeArgs []Type) *Instance {
	return &Instance{origin: origin, typeArgs: typeArgs}
}

func (i *Instance) Origin() Type     { return i.origin }
func (i *Instance) TypeArgs() []Type { return i.typeArgs }
func (i *Instance) Underlying() Type { return i }

func (i *Instance) String() string {
	var b strings.Builder
	b.WriteString(i.origin.String())
	b.WriteByte('[')
	for j, ta := range i.typeArgs {
		if j > 0 {
			b.WriteString(", ")
		}
		b.WriteString(ta.String())
	}
	b.WriteByte(']')
	return b.String()
}
