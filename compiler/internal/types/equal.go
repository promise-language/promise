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
		// Named child assignable to Instance parent via generic inheritance.
		// e.g., Doubler is assignable to Transformer[int] when Doubler is Transformer[int].
		if yi, ok := y.(*Instance); ok {
			if isNamedChildOfInstance(xn, yi) {
				return true
			}
		}
	}

	// Rule 4b: Instance child assignable to Instance parent via generic inheritance.
	// e.g., Range[int] is assignable to Stream[int] when Range is Stream[T].
	if xi, ok := x.(*Instance); ok {
		if yi, ok := y.(*Instance); ok {
			if isInstanceChild(xi, yi) {
				return true
			}
		}
		// Instance child assignable to non-generic Named parent
		if yn, ok := y.(*Named); ok {
			xo, _ := xi.Origin().(*Named)
			if xo != nil && isChild(xo, yn) {
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

	// Rule 9: structural interface satisfaction (meta-tag gated)
	// T is assignable to Interface if the interface is marked `structural
	// and T has concrete implementations for all of its abstract methods.
	// Without `structural, explicit `is is required.
	if yn, ok := y.(*Named); ok && yn.IsAbstract() && yn.IsStructural() {
		if Implements(x, yn) {
			return true
		}
	}

	return false
}

// isChild reports whether child inherits from parent (directly or transitively).
func isChild(child, parent *Named) bool {
	for _, p := range child.parents {
		if p.Named == parent {
			return true
		}
		if isChild(p.Named, parent) {
			return true
		}
	}
	return false
}

// isNamedChildOfInstance reports whether a Named child is assignable to an
// Instance parent. E.g., Doubler (Named) is Transformer[int] (Instance)
// when Doubler has ParentRef{Named: Transformer, TypeArgs: [int]}.
// Handles transitive chains: Leaf is Middle[int] is Base[T] → Leaf assignable to Base[int].
func isNamedChildOfInstance(child *Named, parent *Instance) bool {
	parentOrigin, _ := parent.Origin().(*Named)
	if parentOrigin == nil {
		return false
	}
	for _, p := range child.parents {
		if p.Named == parentOrigin && len(p.TypeArgs) > 0 {
			// Direct match: check that the concrete parent type args match
			parentTypeArgs := parent.TypeArgs()
			if len(p.TypeArgs) != len(parentTypeArgs) {
				continue
			}
			match := true
			for i, ta := range p.TypeArgs {
				if !Identical(ta, parentTypeArgs[i]) {
					match = false
					break
				}
			}
			if match {
				return true
			}
		}
		if p.Named == parentOrigin {
			continue // already checked above with type args
		}
		// Transitive: build intermediate instance if parent has type args
		if len(p.TypeArgs) > 0 {
			intermediate := NewInstance(p.Named, p.TypeArgs)
			if isInstanceChild(intermediate, parent) {
				return true
			}
		} else {
			// Non-generic intermediary — recurse
			if isNamedChildOfInstance(p.Named, parent) {
				return true
			}
		}
	}
	return false
}

// isInstanceChild reports whether child Instance inherits from parent Instance
// via generic inheritance. e.g., Range[int] inherits from Stream[int] when
// Range[T] is Stream[T] — we check that the child's origin has a ParentRef
// whose Named matches the parent's origin, and the substituted type args match.
func isInstanceChild(child, parent *Instance) bool {
	childOrigin, _ := child.Origin().(*Named)
	parentOrigin, _ := parent.Origin().(*Named)
	if childOrigin == nil || parentOrigin == nil {
		return false
	}
	for _, p := range childOrigin.parents {
		// Direct match: parent ref origin matches target origin
		if p.Named == parentOrigin {
			// Substitute child's type args into parent's type args
			subst := BuildSubstMap(childOrigin.TypeParams(), child.TypeArgs())
			if subst == nil && len(childOrigin.TypeParams()) > 0 {
				continue
			}
			match := true
			parentTypeArgs := parent.TypeArgs()
			if len(p.TypeArgs) != len(parentTypeArgs) {
				continue
			}
			for i, ta := range p.TypeArgs {
				resolved := Substitute(ta, subst)
				if !Identical(resolved, parentTypeArgs[i]) {
					match = false
					break
				}
			}
			if match {
				return true
			}
		}
		// Transitive: check if the parent ref's origin is itself an instance child
		if len(p.TypeArgs) > 0 {
			// Build intermediate instance with substituted type args
			subst := BuildSubstMap(childOrigin.TypeParams(), child.TypeArgs())
			substArgs := make([]Type, len(p.TypeArgs))
			for i, ta := range p.TypeArgs {
				substArgs[i] = Substitute(ta, subst)
			}
			intermediate := NewInstance(p.Named, substArgs)
			if isInstanceChild(intermediate, parent) {
				return true
			}
		} else {
			// Non-generic parent in chain — check if it inherits from parentOrigin
			if isChild(p.Named, parentOrigin) {
				return true
			}
		}
	}
	return false
}

// Implements reports whether type x implements interface iface.
// An interface is a Named type where all methods are abstract.
// The concrete type must provide methods with matching names AND signatures
// (same parameter types, return type, and error capability).
// Self-typed parameters in the interface are matched against the concrete type.
func Implements(x Type, iface *Named) bool {
	if !iface.IsAbstract() {
		return false
	}

	// Collect all abstract methods with their declaring interface (for correct Self substitution)
	abstractMethods := iface.allAbstractMethodsWithDeclarer()

	// x must provide concrete implementations for all abstract methods
	// with matching signatures (excluding receiver type).
	switch xt := x.(type) {
	case *Named:
		for _, am := range abstractMethods {
			// Use appropriate lookup based on method kind (getter vs setter vs regular)
			var m *Method
			if am.method.IsGetter() {
				m = xt.LookupGetter(am.method.name)
			} else if am.method.IsSetter() {
				m = xt.LookupSetter(am.method.name)
			} else {
				m = xt.LookupMethod(am.method.name)
			}
			if m == nil || m.abstract {
				return false
			}
			// Factory methods must match: factory satisfies factory, instance satisfies instance
			if am.method.IsFactory() != m.IsFactory() {
				return false
			}
			// Verify signatures match, substituting Self (the declaring interface) with concrete type (xt)
			if !identicalSignaturesWithSelf(m.sig, am.method.sig, am.declarer, xt) {
				return false
			}
		}
		return true
	case *Instance:
		// For Instance types (e.g., Vector[int]), check the origin Named type.
		if n, ok := xt.Origin().(*Named); ok {
			return Implements(n, iface)
		}
		return false
	default:
		return false
	}
}

// identicalSignaturesWithSelf compares two signatures for structural interface
// satisfaction, treating occurrences of the `self` type in the interface signature
// as equal to the `replacement` type in the concrete signature.
//
// Relaxed matching rules (concrete may be more specific than interface):
//   - Extra params: concrete may have more params if all extras have defaults or are optional types
//   - Failable: non-failable concrete satisfies failable interface (but not vice versa)
//   - Optional return: concrete returning T satisfies interface returning T? (but not vice versa)
func identicalSignaturesWithSelf(concrete, iface *Signature, self, replacement *Named) bool {
	// Concrete must have at least as many params as the interface requires
	if len(concrete.params) < len(iface.params) {
		return false
	}
	// Required params (those declared in the interface) must match exactly
	for i := range iface.params {
		if concrete.params[i].ref != iface.params[i].ref {
			return false
		}
		if !identicalWithSelf(concrete.params[i].typ, iface.params[i].typ, self, replacement) {
			return false
		}
	}
	// Extra concrete params must all be omittable (have default or be optional type)
	for i := len(iface.params); i < len(concrete.params); i++ {
		if concrete.params[i].HasDefault() {
			continue
		}
		if _, isOpt := concrete.params[i].typ.(*Optional); isOpt {
			continue
		}
		return false
	}
	// Failable: non-failable concrete can satisfy failable interface,
	// but failable concrete cannot satisfy non-failable interface.
	if concrete.canError && !iface.canError {
		return false
	}
	// Return type
	if concrete.result == nil && iface.result == nil {
		return true
	}
	if concrete.result == nil || iface.result == nil {
		return false
	}
	if identicalWithSelf(concrete.result, iface.result, self, replacement) {
		return true
	}
	// Non-optional concrete return satisfies optional interface return: T matches T?
	if ifaceOpt, ok := iface.result.(*Optional); ok {
		return identicalWithSelf(concrete.result, ifaceOpt.Elem(), self, replacement)
	}
	return false
}

// identicalWithSelf is like Identical but treats the interface type (self) as
// equal to the concrete implementing type (replacement).
func identicalWithSelf(x, y Type, self, replacement *Named) bool {
	if yn, ok := y.(*Named); ok && yn == self {
		if xn, ok := x.(*Named); ok && xn == replacement {
			return true
		}
	}
	return Identical(x, y)
}
