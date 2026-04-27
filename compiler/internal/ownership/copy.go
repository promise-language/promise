package ownership

import "djabi.dev/go/promise_lang/internal/types"

// isCopyType returns true if the given type is implicitly copied on assignment
// rather than moved. Primitives (int, float, bool, char, none, void) are copy.
// References (&T, ~T) are pointer-sized and freely copyable.
// User-defined types marked with `copy meta are also copy.
// NOTE: keep in sync with sema.isCopyField — same logic, separate package.
func isCopyType(typ types.Type) bool {
	if typ == nil {
		return false
	}
	switch typ {
	case types.TypInt, types.TypI8, types.TypI16, types.TypI32, types.TypI64,
		types.TypUint, types.TypU8, types.TypU16, types.TypU32, types.TypU64,
		types.TypF32, types.TypF64,
		types.TypBool, types.TypChar, types.TypNone, types.TypVoid:
		return true
	}
	switch t := typ.(type) {
	case *types.SharedRef, *types.MutRef:
		return true
	case *types.Named:
		return t.IsCopy()
	case *types.Enum:
		return t.IsCopy()
	case *types.Tuple:
		for _, elem := range t.Elems() {
			if !isCopyType(elem) {
				return false
			}
		}
		return true
	case *types.Optional:
		return isCopyType(t.Elem())
	}
	return false
}
