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

// Pointer represents a raw pointer: T* (unsafe only).
type Pointer struct {
	elem Type
}

// NewPointer creates a new pointer type.
func NewPointer(elem Type) *Pointer {
	return &Pointer{elem: elem}
}

func (p *Pointer) Elem() Type       { return p.elem }
func (p *Pointer) Underlying() Type { return p }

func (p *Pointer) String() string {
	return p.elem.String() + "*"
}
