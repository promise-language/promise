package types

import "strings"

// TypeParam represents a generic type parameter: T, K: Hashable, etc.
type TypeParam struct {
	obj        *TypeName
	constraint Type // nil means unconstrained
	index      int  // position in the type parameter list
}

// NewTypeParam creates a new type parameter.
func NewTypeParam(obj *TypeName, constraint Type, index int) *TypeParam {
	tp := &TypeParam{obj: obj, constraint: constraint, index: index}
	obj.SetType(tp)
	return tp
}

func (tp *TypeParam) Obj() *TypeName   { return tp.obj }
func (tp *TypeParam) Constraint() Type { return tp.constraint }
func (tp *TypeParam) Index() int       { return tp.index }
func (tp *TypeParam) Underlying() Type { return tp }

// SetConstraint sets the constraint for this type parameter.
// Used when constraints are resolved after initial declaration.
func (tp *TypeParam) SetConstraint(c Type) {
	tp.constraint = c
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
