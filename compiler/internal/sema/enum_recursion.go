package sema

import (
	"djabi.dev/go/promise_lang/internal/ast"
	"djabi.dev/go/promise_lang/internal/types"
)

// enumReachesItself reports whether typ — used as an enum variant field type —
// transitively reaches `target` through codegen-inlined positions (Optional,
// Tuple, Array, inner Enum, Instance<Enum>) without going through a Named
// user type or std opaque container, both of which terminate the
// computeEnumInternalType recursion. (T0628)
func enumReachesItself(typ types.Type, target *types.Enum, seen map[*types.Enum]bool) bool {
	if typ == nil {
		return false
	}
	switch t := typ.(type) {
	case *types.Enum:
		if t == target {
			return true
		}
		if seen[t] {
			return false
		}
		seen[t] = true
		for _, v := range t.Variants() {
			for _, f := range v.Fields() {
				if enumReachesItself(f.Type(), target, seen) {
					return true
				}
			}
		}
	case *types.Optional:
		return enumReachesItself(t.Elem(), target, seen)
	case *types.Tuple:
		for _, e := range t.Elems() {
			if enumReachesItself(e, target, seen) {
				return true
			}
		}
	case *types.Array:
		return enumReachesItself(t.Elem(), target, seen)
	case *types.Instance:
		origin, ok := t.Origin().(*types.Enum)
		if !ok {
			// Named-origin Instance → opaque {i8*, i8*} / i8* in
			// llvmTypeForEnumFieldFromPromise, terminates recursion.
			return false
		}
		if origin == target {
			return true
		}
		if seen[origin] {
			return false
		}
		seen[origin] = true
		subst := types.BuildSubstMap(origin.TypeParams(), t.TypeArgs())
		for _, v := range origin.Variants() {
			for _, f := range v.Fields() {
				if enumReachesItself(types.Substitute(f.Type(), subst), target, seen) {
					return true
				}
			}
		}
	}
	return false
}

// validateEnumNoSelfRefRecursion rejects enums whose variant field types
// transitively reach the enum itself without container/heap indirection
// (Vector/Map/Set/user-Named wrapper). Without this, codegen's
// computeEnumInternalType infinitely recurses through
// llvmTypeForEnumFieldFromPromise → enumInternalTypeForField →
// computeEnumInternalType and stack-overflows. (T0628)
func (c *Checker) validateEnumNoSelfRefRecursion(file *ast.File) {
	for _, decl := range file.Decls {
		if c.info.FilteredDecls[decl] {
			continue
		}
		ed, ok := decl.(*ast.EnumDecl)
		if !ok {
			continue
		}
		obj := c.scope.Lookup(ed.Name)
		if obj == nil {
			continue
		}
		tn, ok := obj.(*types.TypeName)
		if !ok {
			continue
		}
		enum, ok := tn.Type().(*types.Enum)
		if !ok {
			continue
		}
		for vi, v := range enum.Variants() {
			for fi, f := range v.Fields() {
				if !enumReachesItself(f.Type(), enum, make(map[*types.Enum]bool)) {
					continue
				}
				pos := ed.Pos()
				if vi < len(ed.Variants) && fi < len(ed.Variants[vi].Fields) {
					pos = ed.Variants[vi].Fields[fi].Pos()
				}
				c.errorf(pos,
					"recursive enum %s: variant %s field '%s' of type %s reaches %s without container indirection — use %s[], map[K, %s], or Set[%s] to break the cycle",
					ed.Name, v.Name(), f.Name(), f.Type(), ed.Name, ed.Name, ed.Name, ed.Name)
			}
		}
	}
}
