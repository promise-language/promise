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

// MergeParentSubst augments subst with the bindings a type's parent chain
// establishes: each ParentRef's type arguments, resolved under subst, keyed by the
// PARENT's own type params, recursively up the chain. E.g. for `Derived[T] is
// Base[T]` with subst = {Derived.T → int} it adds {Base.T → int}.
//
// It is what makes an INHERITED field resolvable under a substitution built from
// the CHILD's params. AllFields prepends a parent's fields unsubstituted, so an
// inherited field's declared type is expressed in the parent's *TypeParam — a
// different object from the child's, which a child-only BuildSubstMap leaves
// untouched. A field walk that skips this merge silently reads the inherited field
// as "still generic" and misses whatever the type argument put there. (T1970)
//
// subst must be non-nil; MergeParentSubst writes into it. Callers that may have a
// nil map (BuildSubstMap returns nil on an arity mismatch) should use FieldSubstMap.
func MergeParentSubst(n *Named, subst map[*TypeParam]Type) {
	for _, pr := range n.Parents() {
		if len(pr.TypeArgs) == 0 {
			// Non-generic parent — still recurse for its parents.
			MergeParentSubst(pr.Named, subst)
			continue
		}
		resolvedArgs := make([]Type, len(pr.TypeArgs))
		for i, ta := range pr.TypeArgs {
			resolvedArgs[i] = Substitute(ta, subst)
		}
		for k, v := range BuildSubstMap(pr.Named.TypeParams(), resolvedArgs) {
			subst[k] = v
		}
		// Recurse into the parent's parents for transitive chains.
		MergeParentSubst(pr.Named, subst)
	}
}

// FieldSubstMap returns the substitution that resolves the declared type of every
// field in n.AllFields(): n's own type params bound to typeArgs, plus the
// parent-chain bindings MergeParentSubst adds. Pass a nil typeArgs for a
// non-generic n, or for a generic one walked with its params unbound.
//
// Never returns nil, so the result is always safe to write into. (T1970)
func FieldSubstMap(n *Named, typeArgs []Type) map[*TypeParam]Type {
	// BuildSubstMap always allocates a fresh map, so taking ownership of it and
	// letting MergeParentSubst extend it in place is safe. It returns nil on an
	// arity mismatch, which is the case that needs the map made here instead.
	subst := BuildSubstMap(n.TypeParams(), typeArgs)
	if subst == nil {
		subst = make(map[*TypeParam]Type)
	}
	MergeParentSubst(n, subst)
	return subst
}

// AllFieldTypes returns the type of every field in n's instance layout, in
// AllFields order, each resolved through FieldSubstMap. This is the form every
// TYPE-directed field walk wants.
//
// AllFields itself deliberately stays declared-type and pointer-stable: sema keys
// field defaults on the *Field pointer (Info.FieldDefaults) and codegen looks them
// up while iterating AllFields, so returning substituted copies there would
// silently break inherited field defaults. (T1970)
func AllFieldTypes(n *Named, typeArgs []Type) []Type {
	fields := n.AllFields()
	if len(fields) == 0 {
		return nil
	}
	subst := FieldSubstMap(n, typeArgs)
	out := make([]Type, 0, len(fields))
	for _, f := range fields {
		out = append(out, Substitute(f.Type(), subst))
	}
	return out
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

	default:
		return typ
	}
}

func substSignature(sig *Signature, subst map[*TypeParam]Type) *Signature {
	changed := false

	// If the signature has typeParams that are being substituted,
	// we must strip them (producing a concrete signature).
	if len(sig.typeParams) > 0 {
		for _, tp := range sig.typeParams {
			if _, ok := subst[tp]; ok {
				changed = true
				break
			}
		}
	}

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
	newSig := NewSignature(newRecv, newParams, newResult, sig.canError)
	// T1349: the return-holds-receiver fact is a property of the body shape, not
	// of the concrete type args — preserve it through monomorphization so the
	// call-site signature (e.g. Vector[int].iter()) still carries it.
	newSig.returnHoldsReceiver = sig.returnHoldsReceiver
	// Preserve method-level type params that are NOT being substituted.
	// When substituting type-level params on a generic method's signature,
	// the method's own TypeParams must carry through.
	if len(sig.typeParams) > 0 {
		anySubstituted := false
		for _, tp := range sig.typeParams {
			if _, ok := subst[tp]; ok {
				anySubstituted = true
				break
			}
		}
		if !anySubstituted {
			newSig.SetTypeParams(sig.typeParams)
		}
	}
	return newSig
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
	case *Tuple:
		for _, e := range t.elems {
			if ContainsTypeParam(e) {
				return true
			}
		}
	case *Array:
		return ContainsTypeParam(t.elem)
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

// SubstituteSelf replaces occurrences of iface with concrete in a type.
// Used for default method synthesis: sema records types using the structural
// interface type (e.g., Equal), but codegen needs the concrete type (e.g., Point).
func SubstituteSelf(typ Type, iface, concrete *Named) Type {
	if typ == nil || iface == nil || concrete == nil {
		return typ
	}
	return doSelfSubst(typ, iface, concrete)
}

func doSelfSubst(typ Type, iface, concrete *Named) Type {
	switch t := typ.(type) {
	case *Named:
		if t == iface {
			return concrete
		}
		return t

	case *Enum:
		return t

	case *Instance:
		changed := false
		newArgs := make([]Type, len(t.typeArgs))
		for i, a := range t.typeArgs {
			newArgs[i] = doSelfSubst(a, iface, concrete)
			if newArgs[i] != a {
				changed = true
			}
		}
		if !changed {
			return t
		}
		return NewInstance(t.origin, newArgs)

	case *Signature:
		return selfSubstSignature(t, iface, concrete)

	case *Optional:
		inner := doSelfSubst(t.elem, iface, concrete)
		if inner == t.elem {
			return t
		}
		return NewOptional(inner)

	case *SharedRef:
		inner := doSelfSubst(t.elem, iface, concrete)
		if inner == t.elem {
			return t
		}
		return NewSharedRef(inner)

	case *MutRef:
		inner := doSelfSubst(t.elem, iface, concrete)
		if inner == t.elem {
			return t
		}
		return NewMutRef(inner)

	case *Tuple:
		changed := false
		elems := make([]Type, len(t.elems))
		for i, e := range t.elems {
			elems[i] = doSelfSubst(e, iface, concrete)
			if elems[i] != e {
				changed = true
			}
		}
		if !changed {
			return t
		}
		return NewTuple(elems)

	case *Array:
		elem := doSelfSubst(t.elem, iface, concrete)
		if elem == t.elem {
			return t
		}
		return NewArray(elem, t.size)

	default:
		return typ
	}
}

func selfSubstSignature(sig *Signature, iface, concrete *Named) *Signature {
	changed := false

	var newRecv *Param
	if sig.recv != nil {
		rt := doSelfSubst(sig.recv.typ, iface, concrete)
		if rt != sig.recv.typ {
			newRecv = NewParam(sig.recv.name, rt, sig.recv.ref)
			changed = true
		} else {
			newRecv = sig.recv
		}
	}

	newParams := make([]*Param, len(sig.params))
	for i, p := range sig.params {
		pt := doSelfSubst(p.typ, iface, concrete)
		if pt != p.typ {
			newParams[i] = NewParam(p.name, pt, p.ref)
			changed = true
		} else {
			newParams[i] = p
		}
	}

	var newResult Type
	if sig.result != nil {
		newResult = doSelfSubst(sig.result, iface, concrete)
		if newResult != sig.result {
			changed = true
		}
	}

	if !changed {
		return sig
	}
	newSig := NewSignature(newRecv, newParams, newResult, sig.canError)
	newSig.returnHoldsReceiver = sig.returnHoldsReceiver // T1349
	return newSig
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
