package types

// Identical reports whether two types are identical.
//
// Named and Enum types use nominal identity (pointer comparison).
// All other types use structural equality.
func Identical(x, y Type) bool {
	if x == y {
		return true
	}
	if x == nil || y == nil {
		return false
	}

	switch xt := x.(type) {
	case *Named:
		// Nominal: same pointer only (handled by x == y above)
		return false

	case *Enum:
		// Nominal: same pointer only
		return false

	case *Signature:
		yt, ok := y.(*Signature)
		if !ok {
			return false
		}
		return identicalSignatures(xt, yt)

	case *Tuple:
		yt, ok := y.(*Tuple)
		if !ok {
			return false
		}
		if len(xt.elems) != len(yt.elems) {
			return false
		}
		for i := range xt.elems {
			if !Identical(xt.elems[i], yt.elems[i]) {
				return false
			}
		}
		return true

	case *Array:
		yt, ok := y.(*Array)
		if !ok {
			return false
		}
		return xt.size == yt.size && Identical(xt.elem, yt.elem)

	case *Slice:
		yt, ok := y.(*Slice)
		if !ok {
			return false
		}
		return Identical(xt.elem, yt.elem)

	case *Map:
		yt, ok := y.(*Map)
		if !ok {
			return false
		}
		return Identical(xt.key, yt.key) && Identical(xt.val, yt.val)

	case *Optional:
		yt, ok := y.(*Optional)
		if !ok {
			return false
		}
		return Identical(xt.elem, yt.elem)

	case *SharedRef:
		yt, ok := y.(*SharedRef)
		if !ok {
			return false
		}
		return Identical(xt.elem, yt.elem)

	case *MutRef:
		yt, ok := y.(*MutRef)
		if !ok {
			return false
		}
		return Identical(xt.elem, yt.elem)

	case *Pointer:
		yt, ok := y.(*Pointer)
		if !ok {
			return false
		}
		return Identical(xt.elem, yt.elem)

	case *TypeParam:
		// TypeParam identity is by declaration (pointer)
		return false

	case *Instance:
		yt, ok := y.(*Instance)
		if !ok {
			return false
		}
		if !Identical(xt.origin, yt.origin) {
			return false
		}
		if len(xt.typeArgs) != len(yt.typeArgs) {
			return false
		}
		for i := range xt.typeArgs {
			if !Identical(xt.typeArgs[i], yt.typeArgs[i]) {
				return false
			}
		}
		return true
	}

	return false
}

func identicalSignatures(x, y *Signature) bool {
	if len(x.params) != len(y.params) {
		return false
	}
	for i := range x.params {
		if x.params[i].ref != y.params[i].ref {
			return false
		}
		if !Identical(x.params[i].typ, y.params[i].typ) {
			return false
		}
	}
	if x.canError != y.canError {
		return false
	}
	// Compare results
	if x.result == nil && y.result == nil {
		return true
	}
	if x.result == nil || y.result == nil {
		return false
	}
	return Identical(x.result, y.result)
}

// AssignableTo reports whether a value of type x is assignable to a variable of type y.
func AssignableTo(x, y Type) bool {
	// Rule 1: identical types are always assignable
	if Identical(x, y) {
		return true
	}

	// Rule 2: T is assignable to T? (optional wrapping)
	if opt, ok := y.(*Optional); ok {
		if Identical(x, opt.elem) {
			return true
		}
	}

	// Rule 3: none is assignable to any T?
	if _, ok := y.(*Optional); ok {
		if xn, ok := x.(*Named); ok && xn == TypNone {
			return true
		}
	}

	// Rule 4: child type assignable to parent (inheritance)
	if xn, ok := x.(*Named); ok {
		if yn, ok := y.(*Named); ok {
			if isChild(xn, yn) {
				return true
			}
		}
	}

	// Rule 5: TypeParam assignable to any of its constraints
	if tp, ok := x.(*TypeParam); ok {
		for _, c := range tp.Constraints() {
			if Identical(c, y) {
				return true
			}
		}
	}

	// Rule 6: T is assignable to T& (implicit shared borrow coercion)
	if sr, ok := y.(*SharedRef); ok {
		if AssignableTo(x, sr.elem) {
			return true
		}
	}

	// Rule 7: T is assignable to T~ (implicit mutable borrow coercion)
	if mr, ok := y.(*MutRef); ok {
		if AssignableTo(x, mr.elem) {
			return true
		}
	}

	// Rule 8: T~ is assignable to T& (mutable ref coerces to shared ref)
	if sr, ok := y.(*SharedRef); ok {
		if mr, ok := x.(*MutRef); ok {
			if Identical(mr.elem, sr.elem) {
				return true
			}
		}
	}

	return false
}

// isChild reports whether child inherits from parent (directly or transitively).
func isChild(child, parent *Named) bool {
	for _, p := range child.parents {
		if p == parent {
			return true
		}
		if isChild(p, parent) {
			return true
		}
	}
	return false
}

// Implements reports whether type x implements interface iface.
// An interface is a Named type where all methods are abstract.
func Implements(x Type, iface *Named) bool {
	if !iface.IsAbstract() {
		return false
	}

	// Collect all abstract methods from the interface
	abstractMethods := iface.allAbstractMethods()

	// x must provide concrete implementations for all abstract methods
	switch xt := x.(type) {
	case *Named:
		for _, am := range abstractMethods {
			m := xt.LookupMethod(am.name)
			if m == nil || m.abstract {
				return false
			}
		}
		return true
	default:
		return false
	}
}
