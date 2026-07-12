package codegen

import "testing"

// T1230: reading a heap struct that holds a capturing closure FIELD out of a Map
// value (`fn := m[0]!` on a `Fn { () -> int f; }`) must NOT deep-copy the struct —
// the closure env (captured frame) is opaque and cannot be cloned, so dupHeapValueFields
// would zero the cloned closure slot (T0813), yielding a null {fn,env} fat pointer →
// SEGV on invoke. The fix makes heapTypeSafeToDup treat a closure-nesting field as
// un-dup-safe (via sema.FirstFieldNestedClosure), so typeNeedsMatchDup returns false
// and `Map[K,V].[]`'s `return v` yields a shallow alias with the env intact; ownership
// marks the local Borrowed so escapes are rejected.

// The `Map[int, Fn].[]` body must NOT emit a struct deep-copy (heapdup.copy) for the
// closure-nesting value — the element is returned by alias.
func TestT1230_StructClosureMapReadNoDup(t *testing.T) {
	ir := generateIR(t, `
		type Fn { () -> int f; }
		probe() {
			x := 5;
			m := Map[int, Fn]();
			m[0] = Fn(f: || -> x);
			fn := m[0]!;
			y := fn.f();
		}
		main() { probe(); }
	`)
	fn := extractDefine(ir, `"Map[int, Fn].[]"`)
	if fn == "" {
		t.Fatalf("Map[int, Fn].[] not found in IR")
	}
	// A deep-copy (alloc + memcpy) would zero the closure slot and corrupt the env.
	assertNotContains(t, fn, "heapdup.copy")
}

// Control: a plain (non-closure) heap struct with a droppable string field IS
// dup-safe, so the same `[]` read deep-copies it (heapdup.copy) — the bound local
// owns an independent copy. Guards against the fix over-suppressing the dup for
// ordinary heap structs.
func TestT1230_PlainStructMapReadDups(t *testing.T) {
	ir := generateIR(t, `
		type SBox { string s; }
		probe() {
			m := Map[int, SBox]();
			m[0] = SBox(s: "hi");
			b := m[0]!;
		}
		main() { probe(); }
	`)
	fn := extractDefine(ir, `"Map[int, SBox].[]"`)
	if fn == "" {
		t.Fatalf("Map[int, SBox].[] not found in IR")
	}
	assertContains(t, fn, "heapdup.copy")
}

// The genAssignStmt borrow-suppression arm (`f = m[k]!` into an already-bound
// local) is a distinct code path from the var-decl arms. Reassigning a
// closure-nesting struct read must not deep-copy it into the target — the probe()
// caller frame would otherwise invoke a zeroed env. Assert the enclosing probe()
// function carries no struct deep-copy for the reassign.
func TestT1230_StructClosureMapReassignNoDup(t *testing.T) {
	ir := generateIR(t, `
		type Fn { () -> int f; }
		probe() {
			x := 5;
			m := Map[int, Fn]();
			m[0] = Fn(f: || -> x);
			fn := Fn(f: || -> 0);
			fn = m[0]!;
			y := fn.f();
		}
		main() { probe(); }
	`)
	fn := extractDefine(ir, "__user.probe")
	if fn == "" {
		t.Fatalf("probe not found in IR")
	}
	assertNotContains(t, fn, "heapdup.copy")
}

// T1260: a struct whose field is a VALUE-COPYING container of closures
// (`FnV { (() -> int)[] fns; }`) must ALSO be treated as un-dup-safe. The prior
// FirstFieldNestedClosure treated every std container as opaque, so the inner
// Vector[() -> int] was not seen and the Map read deep-copied the struct — the
// per-element closure clone zeroes the env (T0813) → null {fn,env} → SEGV on
// invoke. The fix recurses TypeArgs of value-copying containers, so the `[]`
// read is a borrow (no heapdup.copy) and the vector's closure envs stay intact.
func TestT1260_StructVecClosureMapReadNoDup(t *testing.T) {
	ir := generateIR(t, `
		type FnV { (() -> int)[] fns; }
		probe() {
			x := 6;
			m := Map[int, FnV]();
			m[0] = FnV(fns: [|| -> x]);
			b := m[0]!;
			y := b.fns[0]();
		}
		main() { probe(); }
	`)
	fn := extractDefine(ir, `"Map[int, FnV].[]"`)
	if fn == "" {
		t.Fatalf("Map[int, FnV].[] not found in IR")
	}
	// A deep-copy would clone the inner vector and zero each closure's env slot.
	assertNotContains(t, fn, "heapdup.copy")
}

// Control for T1260: a struct with a value-copying container of a NON-closure
// element (`IntBox { int[] xs; }`) stays dup-safe, so the `[]` read still
// deep-copies it (heapdup.copy). Guards the fix's TypeArgs recursion against
// over-suppressing the dup for ordinary value-copying containers.
func TestT1260_StructIntVecMapReadDups(t *testing.T) {
	ir := generateIR(t, `
		type IntBox { int[] xs; }
		probe() {
			m := Map[int, IntBox]();
			m[0] = IntBox(xs: [1, 2, 3]);
			b := m[0]!;
		}
		main() { probe(); }
	`)
	fn := extractDefine(ir, `"Map[int, IntBox].[]"`)
	if fn == "" {
		t.Fatalf("Map[int, IntBox].[] not found in IR")
	}
	assertContains(t, fn, "heapdup.copy")
}

// T1260: a struct whose field is a MAP of closures (`FnM { map[int, () -> int] fns; }`)
// must also be un-dup-safe. Exercises the TypMap arm of isValueCopyingContainerNamed
// (distinct from the Vector arm in TestT1260_StructVecClosureMapReadNoDup) — the Map
// read is a borrow (no heapdup.copy), so the closure envs in the nested map survive.
func TestT1260_StructMapClosureMapReadNoDup(t *testing.T) {
	ir := generateIR(t, `
		type FnM { map[int, () -> int] fns; }
		probe() {
			x := 6;
			fns := Map[int, () -> int]();
			fns[0] = || -> x;
			m := Map[int, FnM]();
			m[0] = FnM(fns: move fns);
			b := m[0]!;
		}
		main() { probe(); }
	`)
	fn := extractDefine(ir, `"Map[int, FnM].[]"`)
	if fn == "" {
		t.Fatalf("Map[int, FnM].[] not found in IR")
	}
	assertNotContains(t, fn, "heapdup.copy")
}

// T1262: a BARE value-copying container of closures as the Map VALUE itself
// (`Map[int, (() -> int)[]]`, not a struct field). The `[]` deep-copied the value
// via dupVector's element-clone path, which zeroes each closure element's opaque
// env (the vecclonenull loop, T0813) → null {fn,env} → SEGV on invoke. The fix
// guards typeNeedsMatchDup with FirstFieldNestedClosureDeep so the value stays
// aliased — the `[]` body emits NEITHER a vector element-clone-null loop nor a
// deep-copy; the read is a borrow with envs intact.
func TestT1262_BareVecClosureMapReadNoDup(t *testing.T) {
	ir := generateIR(t, `
		probe() {
			x := 7;
			m := Map[int, (() -> int)[]]();
			m[0] = [|| -> x];
			b := m[0]!;
			y := b[0]();
		}
		main() { probe(); }
	`)
	fn := extractDefine(ir, `"Map[int, Vector[() -> int]].[]"`)
	if fn == "" {
		t.Fatalf("Map[int, Vector[() -> int]].[] not found in IR")
	}
	// The env-zeroing loop (dupVector's closure element clone) must not be emitted.
	assertNotContains(t, fn, "vecclonenull")
	// Nor a struct deep-copy.
	assertNotContains(t, fn, "heapdup.copy")
}

// Control for T1262: a bare NON-closure vector as the Map value
// (`Map[int, int[]]`) stays dup-safe, so the `[]` read still deep-copies the
// vector (a vecdup element loop / buffer copy). Guards the fix against
// over-suppressing the dup for ordinary value-copying containers. Reading the
// element out and mutating it must not alias the map's stored vector.
func TestT1262_BareIntVecMapReadDups(t *testing.T) {
	ir := generateIR(t, `
		probe() {
			m := Map[int, int[]]();
			m[0] = [1, 2, 3];
			b := m[0]!;
		}
		main() { probe(); }
	`)
	fn := extractDefine(ir, `"Map[int, Vector[int]].[]"`)
	if fn == "" {
		t.Fatalf("Map[int, Vector[int]].[] not found in IR")
	}
	// A non-closure vector value is duped on read (dupVector inlines a buffer copy).
	assertContains(t, fn, "vecdup.copy")
}
