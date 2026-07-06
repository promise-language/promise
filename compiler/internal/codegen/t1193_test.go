package codegen

import "testing"

// T1193: a user-defined operator whose body returns a whole borrowed native
// *handle* operand (Vector/Channel/Arc/Weak) as an owned value used to crash the
// compiler — cloneOwnedReturnAlias fell through to dupHeapValue, which assumes
// the {vtable,instance} value struct and panics on the bare i8* handle
// (interface conversion PointerType→StructType). The fix routes the
// native-handle family through maybeDupPushElement, which dups each correctly.
// A Vector operand deep-copies its buffer → a vecdup block in the operator body.
func TestOperatorReturnVectorOperandDups(t *testing.T) {
	ir := generateIR(t, `
		type S { int z; %(int[] other) int[] { return other; } }
		main() { x := S(z: 1); int[] base = [10, 20]; r := x % base; }
	`)
	assertContains(t, extractFunction(ir, `"S.%"`), "vecdup")
}

// T1193: a Channel operand returned bare bumps the channel refcount rather than
// panicking — a chdup block appears in the operator body.
func TestOperatorReturnChannelOperandDups(t *testing.T) {
	ir := generateIR(t, `
		type S { int z; %(channel[int] other) channel[int] { return other; } }
		main() { x := S(z: 1); c := channel[int](); r := x % c; }
	`)
	assertContains(t, extractFunction(ir, `"S.%"`), "chdup")
}

// T1193: a Ref[T] (Arc) operand returned bare bumps the strong refcount rather
// than panicking — an arcdup block appears in the operator body. Ref is a native
// i8* handle in the isContainerType family, so it hits the new dispatch branch
// (not Vector/Channel) and confirms maybeDupPushElement covers the whole family.
func TestOperatorReturnRefOperandDups(t *testing.T) {
	ir := generateIR(t, `
		type S { int z; %(Ref[int] other) Ref[int] { return other; } }
		main() { x := S(z: 1); a := Ref[int](5); r := x % a; }
	`)
	assertContains(t, extractFunction(ir, `"S.%"`), "arcdup")
}

// T1193: a Weak[T] operand returned bare bumps the weak refcount — a weakdup
// block appears. Weak is the last member of the isContainerType native-handle
// family with a real dup, so this closes the dispatch-branch coverage.
func TestOperatorReturnWeakOperandDups(t *testing.T) {
	ir := generateIR(t, `
		type S { int z; %(Weak[int] other) Weak[int] { return other; } }
		main() { }
	`)
	assertContains(t, extractFunction(ir, `"S.%"`), "weakdup")
}

// T1193 (non-regression): a plain heap user-type operand still routes through
// the well-tested dupHeapValue path (T0893) — it emits a heapdup, not the
// container dup markers.
func TestOperatorReturnUserTypeOperandStillHeapDups(t *testing.T) {
	body := extractFunction(generateIR(t, `
		type U { int v; }
		type S { int z; %(U other) U { return other; } }
		main() { x := S(z: 1); u := U(v: 2); r := x % u; }
	`), `"S.%"`)
	assertContains(t, body, "heapdup")
	assertNotContains(t, body, "vecdup")
	assertNotContains(t, body, "chdup")
}

// T1193 (non-regression): a value-type operand is embedded, not a handle, so
// returning it must NOT clone (no vecdup/heapdup).
func TestOperatorReturnValueTypeOperandDoesNotDup(t *testing.T) {
	body := extractFunction(generateIR(t, `
		type P { int x `+"`value"+`; }
		type S { int z; %(P other) P { return other; } }
		main() { x := S(z: 1); p := P(x: 2); r := x % p; }
	`), `"S.%"`)
	assertNotContains(t, body, "vecdup")
	assertNotContains(t, body, "heapdup")
}
