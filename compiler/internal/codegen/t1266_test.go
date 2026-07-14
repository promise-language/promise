package codegen

import "testing"

// T1266: third structural sibling of T1262/T1263. A fixed-size array whose elements
// are value-copying containers of closures SEGVs at 0x0 on invoke — reading an element
// through genArrayIndex deep-copies the inner container, and dupVector's element-clone
// loop zeroes each closure's opaque env (T0813) → null {fn,env} → SEGV. The fix guards
// the container branch of genArrayIndex with FirstFieldNestedClosureDeep so the read
// stays ALIASED (a borrow of the array's owned storage, envs intact); the borrow gates
// suppress the owning drop binding and reject escapes.

// Fixed-array index of a value-copying container of closures: the probe body must NOT
// emit dupVector's env-zeroing element-clone loop (vecclonenull).
func TestT1266_ArrayOfVecClosureReadNoDup(t *testing.T) {
	ir := generateIR(t, `
		probe() {
			x := 7;
			(() -> int)[][1] arr = [[|| -> x]];
			w := arr[0];
			y := w[0]();
		}
		main() { probe(); }
	`)
	fn := extractDefine(ir, "__user.probe")
	if fn == "" {
		t.Fatalf("probe not found in IR")
	}
	// A deep-copy of the inner vector would zero each closure element's env slot.
	assertNotContains(t, fn, "vecclonenull")
}

// Generic/monomorphized path: the same guard must fire when genArrayIndex runs with
// an active type substitution (c.typeSubst != nil). Here the array element type genuinely
// carries the type param — `(() -> T)[]` resolves to `(() -> int)[]` via types.Substitute
// — so the substitution branch of the guard is exercised (not the closed-type no-op). The
// mono instance `probe[int]` must still leave the element aliased (no env-zeroing dup).
func TestT1266_GenericArrayOfVecClosureReadNoDup(t *testing.T) {
	ir := generateIR(t, `
		probe[T]((() -> T)[][1] arr) T {
			w := arr[0];
			return w[0]();
		}
		main() {
			x := 7;
			(() -> int)[][1] a = [[|| -> x]];
			probe[int](a);
		}
	`)
	fn := extractDefine(ir, `probe[int]`)
	if fn == "" {
		t.Fatalf("probe[int] mono instance not found in IR")
	}
	assertNotContains(t, fn, "vecclonenull")
}

// Control: a fixed-array of a NON-closure value-copying container (`int[]`) stays
// dup-safe — the element read must still deep-copy the inner vector (vecdup.copy).
// Guards the fix against over-suppressing the dup, which would alias the array's
// stored buffer and break mutation isolation.
func TestT1266_ArrayOfIntVecReadDups(t *testing.T) {
	ir := generateIR(t, `
		probe() {
			int[][1] arr = [[1, 2, 3]];
			w := arr[0];
		}
		main() { probe(); }
	`)
	fn := extractDefine(ir, "__user.probe")
	if fn == "" {
		t.Fatalf("probe not found in IR")
	}
	assertContains(t, fn, "vecdup.copy")
}
