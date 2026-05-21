package types

// IsCopy reports whether values of the given type are implicitly copied on
// assignment rather than moved. Used to gate implicit borrow decay (Rules 8b/8c
// in AssignableTo) — non-Copy decay would silently duplicate ownership of heap
// data and is rejected in favor of explicit `.clone()` or borrow propagation.
//
// Copy types: numeric/bool/char/none/void primitives; references (`T&`/`T~`)
// which are pointer-sized; user-defined types/enums marked `copy; tuples,
// arrays and optionals whose element types are themselves Copy; instances of
// generic types whose origin is Copy. TypeParams are conservatively NOT Copy
// at sema time — generic code that needs ownership transfer must use
// `.clone()` or accept a borrow.
func IsCopy(typ Type) bool {
	if typ == nil {
		return false
	}
	switch typ {
	case TypInt, TypI8, TypI16, TypI32, TypI64,
		TypUint, TypU8, TypU16, TypU32, TypU64,
		TypF32, TypF64,
		TypBool, TypChar, TypNone, TypVoid:
		return true
	}
	switch t := typ.(type) {
	case *SharedRef, *MutRef:
		return true
	case *Named:
		return t.IsCopy()
	case *Enum:
		return t.IsCopy()
	case *Tuple:
		for _, elem := range t.Elems() {
			if !IsCopy(elem) {
				return false
			}
		}
		return true
	case *Optional:
		return IsCopy(t.Elem())
	case *Array:
		return IsCopy(t.Elem())
	case *Instance:
		switch o := t.Origin().(type) {
		case *Named:
			return o.IsCopy()
		case *Enum:
			return o.IsCopy()
		}
	}
	return false
}
