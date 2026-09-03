package types

// Optional represents an optional type: T?.
type Optional struct {
	elem Type
}

// NewOptional creates a new optional type.
func NewOptional(elem Type) *Optional {
	return &Optional{elem: elem}
}

func (o *Optional) Elem() Type       { return o.elem }
func (o *Optional) Underlying() Type { return o }

func (o *Optional) String() string {
	return o.elem.String() + "?"
}

// SharedRef represents a shared borrow: T&.
type SharedRef struct {
	elem Type
}

// NewSharedRef creates a new shared reference type.
func NewSharedRef(elem Type) *SharedRef {
	return &SharedRef{elem: elem}
}

func (r *SharedRef) Elem() Type       { return r.elem }
func (r *SharedRef) Underlying() Type { return r }

func (r *SharedRef) String() string {
	return r.elem.String() + "&"
}

// MutRef represents a mutable borrow: T~.
type MutRef struct {
	elem Type
}

// NewMutRef creates a new mutable reference type.
func NewMutRef(elem Type) *MutRef {
	return &MutRef{elem: elem}
}

func (r *MutRef) Elem() Type       { return r.elem }
func (r *MutRef) Underlying() Type { return r }

func (r *MutRef) String() string {
	return r.elem.String() + "~"
}
