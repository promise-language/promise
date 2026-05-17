package bindgen

import "fmt"

// Canonical ABI type flattening for the WebAssembly Component Model.
// Converts compound WIT types to flat i32/i64/f32/f64 params for core WASM.
// See: https://github.com/WebAssembly/component-model/blob/main/design/mvp/CanonicalABI.md

// FlatType represents a canonical ABI core value type.
type FlatType int

const (
	FlatI32 FlatType = iota
	FlatI64
	FlatF32
	FlatF64
)

// MaxFlatParams is the canonical ABI limit for flat parameters.
// If a function's flattened params exceed this, a pointer is used instead.
const MaxFlatParams = 16

// MaxFlatResults is the canonical ABI limit for flat results.
// If a function's flattened results exceed this, a retptr is used.
const MaxFlatResults = 1

// FlatParam is a flattened parameter in the canonical ABI.
type FlatParam struct {
	Name string
	Type FlatType
}

// flattenType recursively flattens a TypeRef into canonical ABI core types.
func flattenType(ref TypeRef) []FlatType {
	switch ref.Kind {
	case BuiltinKind:
		return flattenBuiltin(ref.Builtin)
	case NamedKind:
		// Named types (records, enums, variants, flags) need type resolution.
		// Without full type info, treat as i32 (conservative).
		return []FlatType{FlatI32}
	case ListKind:
		return []FlatType{FlatI32, FlatI32} // (ptr, len)
	case OptionKind:
		// discriminant + flatten(T)
		inner := flattenType(*ref.Elem)
		result := make([]FlatType, 0, 1+len(inner))
		result = append(result, FlatI32) // discriminant
		result = append(result, inner...)
		return result
	case ResultKind:
		// discriminant + max(flatten(ok), flatten(err))
		var okFlat, errFlat []FlatType
		if ref.Ok != nil {
			okFlat = flattenType(*ref.Ok)
		}
		if ref.Err != nil {
			errFlat = flattenType(*ref.Err)
		}
		result := []FlatType{FlatI32} // discriminant
		wider := okFlat
		if len(errFlat) > len(okFlat) {
			wider = errFlat
		}
		result = append(result, wider...)
		return result
	case TupleKind:
		var result []FlatType
		for _, e := range ref.Elements {
			result = append(result, flattenType(e)...)
		}
		return result
	case OwnKind, BorrowKind:
		return []FlatType{FlatI32} // resource handle
	default:
		return []FlatType{FlatI32}
	}
}

// flattenBuiltin maps a WIT builtin type to its canonical ABI flat representation.
func flattenBuiltin(builtin string) []FlatType {
	switch builtin {
	case "u8", "u16", "u32", "s8", "s16", "s32", "bool", "char":
		return []FlatType{FlatI32}
	case "u64", "s64":
		return []FlatType{FlatI64}
	case "f32":
		return []FlatType{FlatF32}
	case "f64":
		return []FlatType{FlatF64}
	case "string":
		return []FlatType{FlatI32, FlatI32} // (ptr, len)
	default:
		return []FlatType{FlatI32}
	}
}

// flatPromiseType returns the Promise type name for a FlatType.
func flatPromiseType(ft FlatType) string {
	switch ft {
	case FlatI32:
		return "i32"
	case FlatI64:
		return "i64"
	case FlatF32:
		return "f32"
	case FlatF64:
		return "f64"
	default:
		return "i32"
	}
}

// flattenParams computes the canonical ABI flat parameter list.
// Returns (flatParams, usePtr). If usePtr is true, the function should use a single
// i32 pointer parameter instead of flat params (total exceeds MaxFlatParams).
func flattenParams(params []Param) (flat []FlatParam, usePtr bool) {
	for _, p := range params {
		types := flattenType(p.Type)
		if len(types) == 1 {
			flat = append(flat, FlatParam{Name: p.Name, Type: types[0]})
		} else {
			for i, ft := range types {
				name := flatParamName(p.Name, i, len(types))
				flat = append(flat, FlatParam{Name: name, Type: ft})
			}
		}
	}
	if len(flat) > MaxFlatParams {
		return nil, true
	}
	return flat, false
}

// flattenResults computes the canonical ABI flat result list.
// Returns (flatResults, useRetPtr). If useRetPtr is true, the function should add
// a retptr as its last parameter and return void.
func flattenResults(results []TypeRef) (flat []FlatType, useRetPtr bool) {
	for _, r := range results {
		// For result<T, E>, flatten the whole thing (discriminant + payload)
		flat = append(flat, flattenType(r)...)
	}
	if len(flat) > MaxFlatResults {
		return flat, true
	}
	return flat, false
}

// flatParamName generates a name for a flat parameter component.
func flatParamName(baseName string, index, total int) string {
	if total == 2 {
		if index == 0 {
			return baseName + "_ptr"
		}
		return baseName + "_len"
	}
	if index == 0 {
		return baseName + "_tag"
	}
	return fmt.Sprintf("%s_%d", baseName, index-1)
}

// needsCanonicalLowering returns true if a TypeRef requires canonical ABI
// lowering (i.e., is not a simple scalar that passes directly).
func needsCanonicalLowering(ref TypeRef) bool {
	if ref.Kind == BuiltinKind {
		return ref.Builtin == "string"
	}
	if ref.Kind == OwnKind || ref.Kind == BorrowKind {
		return false // resource handles are already i32
	}
	if ref.Kind == ListKind || ref.Kind == OptionKind || ref.Kind == ResultKind ||
		ref.Kind == TupleKind || ref.Kind == NamedKind {
		return true
	}
	return false
}
