package ownership

import "testing"

// T0940: the elvis `?:` move-at-ownership contract (T0936) is representation-agnostic
// — it consumes BOTH operands for Map/Set/heap-user results exactly as for
// Vector/string. This is the precondition that makes the codegen-side none-path
// neutralization sound: orphaning a selected owned-local default is safe because any
// later reuse of that operand is already a compile error. These tests lock the
// ownership-pass side for the non-Vector/non-string representations.

// Reusing a Map default after a bound elvis is a move error.
func TestT0940ReuseMapDefaultAfterBoundElvis(t *testing.T) {
	errs := ownerErrs(t, `
		test() {
			map[string, int]? a = none;
			map[string, int] b = map[string, int](); b["k"] = 9;
			m := a ?: b;
			k := b.len;
		}
	`)
	expectOwnerError(t, errs, "use of moved variable 'b'")
}

// Reusing the Map optional source after a bound elvis is a move error.
func TestT0940ReuseMapOptionalAfterBoundElvis(t *testing.T) {
	errs := ownerErrs(t, `
		test() {
			map[string, int] av = map[string, int](); av["x"] = 1;
			map[string, int]? a = av;
			map[string, int] b = map[string, int](); b["k"] = 9;
			map[string, int] c = map[string, int](); c["j"] = 7;
			m := a ?: b;
			n := a ?: c;
		}
	`)
	expectOwnerError(t, errs, "use of moved variable 'a'")
}

// Reusing a heap-user-type default after a bound elvis is a move error.
func TestT0940ReuseHeapUserDefaultAfterBoundElvis(t *testing.T) {
	errs := ownerErrs(t, `
		type HeapBox { int[] data; }
		test() {
			HeapBox? a = none;
			HeapBox b = HeapBox(data: []); b.data.push(7);
			m := a ?: b;
			k := b.data.len;
		}
	`)
	expectOwnerError(t, errs, "use of moved variable 'b'")
}

// Single-use bound Map elvis must still compile — the shape existing `?:` users rely
// on, now exercised for the value-struct representation.
func TestT0940SingleUseBoundMapOK(t *testing.T) {
	ownerOK(t, `
		test() {
			map[string, int]? a = none;
			map[string, int] b = map[string, int](); b["k"] = 9;
			m := a ?: b;
			k := m.len;
		}
	`)
}
