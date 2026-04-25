package types

// BuildSubstMap creates a substitution map from type parameters to type arguments.
func BuildSubstMap(tparams []*TypeParam, targs []Type) map[*TypeParam]Type {
	if len(tparams) != len(targs) {
		return nil
	}
	m := make(map[*TypeParam]Type, len(tparams))
	for i, tp := range tparams {
		m[tp] = targs[i]
	}
	return m
}

// Substitute replaces all TypeParam occurrences in typ with the
// corresponding concrete types from the subst map.
// Returns the original type unchanged if no substitutions apply.
func Substitute(typ Type, subst map[*TypeParam]Type) Type {
	if typ == nil || len(subst) == 0 {
		return typ
	}
	return doSubst(typ, subst)
}

func doSubst(typ Type, subst map[*TypeParam]Type) Type {
	switch t := typ.(type) {
	case *TypeParam:
		if concrete, ok := subst[t]; ok {
			return concrete
		}
		return t

	case *Named:
		return t

	case *Enum:
		return t

	case *Instance:
		newArgs := substList(t.typeArgs, subst)
		if typeSliceEq(newArgs, t.typeArgs) {
			return t
		}
		return NewInstance(t.origin, newArgs)

	case *Signature:
		return substSignature(t, subst)

	case *Optional:
		inner := doSubst(t.elem, subst)
		if inner == t.elem {
			return t
		}
		return NewOptional(inner)

	case *SharedRef:
		inner := doSubst(t.elem, subst)
		if inner == t.elem {
			return t
		}
		return NewSharedRef(inner)

	case *MutRef:
		inner := doSubst(t.elem, subst)
		if inner == t.elem {
			return t
		}
		return NewMutRef(inner)

	case *Pointer:
		inner := doSubst(t.elem, subst)
		if inner == t.elem {
			return t
		}
		return NewPointer(inner)

	case *Tuple:
		newElems := substList(t.elems, subst)
		if typeSliceEq(newElems, t.elems) {
			return t
		}
		return NewTuple(newElems)

	case *Array:
		elem := doSubst(t.elem, subst)
		if elem == t.elem {
			return t
		}
		return NewArray(elem, t.size)

	case *Slice:
		elem := doSubst(t.elem, subst)
		if elem == t.elem {
			return t
		}
		return NewSlice(elem)

	case *Map:
		key := doSubst(t.key, subst)
		val := doSubst(t.val, subst)
		if key == t.key && val == t.val {
			return t
		}
		return NewMap(key, val)

	default:
		return typ
	}
}

func substSignature(sig *Signature, subst map[*TypeParam]Type) *Signature {
	changed := false

	var newRecv *Param
	if sig.recv != nil {
		rt := doSubst(sig.recv.typ, subst)
		if rt != sig.recv.typ {
			newRecv = NewParam(sig.recv.name, rt, sig.recv.ref)
			changed = true
		} else {
			newRecv = sig.recv
		}
	}

	newParams := make([]*Param, len(sig.params))
	for i, p := range sig.params {
		pt := doSubst(p.typ, subst)
		if pt != p.typ {
			newParams[i] = NewParam(p.name, pt, p.ref)
			changed = true
		} else {
			newParams[i] = p
		}
	}

	var newResult Type
	if sig.result != nil {
		newResult = doSubst(sig.result, subst)
		if newResult != sig.result {
			changed = true
		}
	}

	if !changed {
		return sig
	}
	return NewSignature(newRecv, newParams, newResult, sig.canError)
}

func substList(list []Type, subst map[*TypeParam]Type) []Type {
	result := make([]Type, len(list))
	for i, t := range list {
		result[i] = doSubst(t, subst)
	}
	return result
}

// ContainsTypeParam reports whether typ contains any TypeParam.
// Used to distinguish concrete instantiations (e.g., Box[int]) from
// non-concrete ones (e.g., Box[T]) that arise during type definition.
func ContainsTypeParam(typ Type) bool {
	if typ == nil {
		return false
	}
	switch t := typ.(type) {
	case *TypeParam:
		return true
	case *Instance:
		for _, arg := range t.typeArgs {
			if ContainsTypeParam(arg) {
				return true
			}
		}
	case *Optional:
		return ContainsTypeParam(t.elem)
	case *SharedRef:
		return ContainsTypeParam(t.elem)
	case *MutRef:
		return ContainsTypeParam(t.elem)
	case *Pointer:
		return ContainsTypeParam(t.elem)
	case *Tuple:
		for _, e := range t.elems {
			if ContainsTypeParam(e) {
				return true
			}
		}
	case *Array:
		return ContainsTypeParam(t.elem)
	case *Slice:
		return ContainsTypeParam(t.elem)
	case *Map:
		return ContainsTypeParam(t.key) || ContainsTypeParam(t.val)
	case *Signature:
		for _, p := range t.params {
			if ContainsTypeParam(p.typ) {
				return true
			}
		}
		return ContainsTypeParam(t.result)
	}
	return false
}

func typeSliceEq(a, b []Type) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
