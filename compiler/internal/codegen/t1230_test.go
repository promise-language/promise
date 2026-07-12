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
