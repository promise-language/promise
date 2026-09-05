package regress3

import (
	"testing"

	"github.com/promise-language/promise/compiler/internal/codegen/codegentest"
)

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
	ir := codegentest.GenerateIR(t, `
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
	fn := codegentest.ExtractDefine(ir, `"Map[int, Fn].[]"`)
	if fn == "" {
		t.Fatalf("Map[int, Fn].[] not found in IR")
	}
	// A deep-copy (alloc + memcpy) would zero the closure slot and corrupt the env.
	codegentest.AssertNotContains(t, fn, "heapdup.copy")
}

// Control: a plain (non-closure) heap struct with a droppable string field IS
// dup-safe, so the same `[]` read deep-copies it (heapdup.copy) — the bound local
// owns an independent copy. Guards against the fix over-suppressing the dup for
// ordinary heap structs.
func TestT1230_PlainStructMapReadDups(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type SBox { string s; }
		probe() {
			m := Map[int, SBox]();
			m[0] = SBox(s: "hi");
			b := m[0]!;
		}
		main() { probe(); }
	`)
	fn := codegentest.ExtractDefine(ir, `"Map[int, SBox].[]"`)
	if fn == "" {
		t.Fatalf("Map[int, SBox].[] not found in IR")
	}
	codegentest.AssertContains(t, fn, "heapdup.copy")
}

// The genAssignStmt borrow-suppression arm (`f = m[k]!` into an already-bound
// local) is a distinct code path from the var-decl arms. Reassigning a
// closure-nesting struct read must not deep-copy it into the target — the probe()
// caller frame would otherwise invoke a zeroed env. Assert the enclosing probe()
// function carries no struct deep-copy for the reassign.
func TestT1230_StructClosureMapReassignNoDup(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
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
	fn := codegentest.ExtractDefine(ir, "__user.probe")
	if fn == "" {
		t.Fatalf("probe not found in IR")
	}
	codegentest.AssertNotContains(t, fn, "heapdup.copy")
}

// T1260: a struct whose field is a VALUE-COPYING container of closures
// (`FnV { (() -> int)[] fns; }`) must ALSO be treated as un-dup-safe. The prior
// FirstFieldNestedClosure treated every std container as opaque, so the inner
// Vector[() -> int] was not seen and the Map read deep-copied the struct — the
// per-element closure clone zeroes the env (T0813) → null {fn,env} → SEGV on
// invoke. The fix recurses TypeArgs of value-copying containers, so the `[]`
// read is a borrow (no heapdup.copy) and the vector's closure envs stay intact.
func TestT1260_StructVecClosureMapReadNoDup(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
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
	fn := codegentest.ExtractDefine(ir, `"Map[int, FnV].[]"`)
	if fn == "" {
		t.Fatalf("Map[int, FnV].[] not found in IR")
	}
	// A deep-copy would clone the inner vector and zero each closure's env slot.
	codegentest.AssertNotContains(t, fn, "heapdup.copy")
}

// Control for T1260: a struct with a value-copying container of a NON-closure
// element (`IntBox { int[] xs; }`) stays dup-safe, so the `[]` read still
// deep-copies it (heapdup.copy). Guards the fix's TypeArgs recursion against
// over-suppressing the dup for ordinary value-copying containers.
func TestT1260_StructIntVecMapReadDups(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type IntBox { int[] xs; }
		probe() {
			m := Map[int, IntBox]();
			m[0] = IntBox(xs: [1, 2, 3]);
			b := m[0]!;
		}
		main() { probe(); }
	`)
	fn := codegentest.ExtractDefine(ir, `"Map[int, IntBox].[]"`)
	if fn == "" {
		t.Fatalf("Map[int, IntBox].[] not found in IR")
	}
	codegentest.AssertContains(t, fn, "heapdup.copy")
}

// T1260: a struct whose field is a MAP of closures (`FnM { map[int, () -> int] fns; }`)
// must also be un-dup-safe. Map reaches the by-value property through its
// Slot[K, V][] field rather than by being named (T1926), so this exercises the
// derived branch one hop deeper than the Vector case in
// TestT1260_StructVecClosureMapReadNoDup — the Map read is a borrow (no
// heapdup.copy), so the closure envs in the nested map survive.
func TestT1260_StructMapClosureMapReadNoDup(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
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
	fn := codegentest.ExtractDefine(ir, `"Map[int, FnM].[]"`)
	if fn == "" {
		t.Fatalf("Map[int, FnM].[] not found in IR")
	}
	codegentest.AssertNotContains(t, fn, "heapdup.copy")
}

// T1262: a BARE value-copying container of closures as the Map VALUE itself
// (`Map[int, (() -> int)[]]`, not a struct field). The `[]` deep-copied the value
// via dupVector's element-clone path, which zeroes each closure element's opaque
// env (the vecclonenull loop, T0813) → null {fn,env} → SEGV on invoke. The fix
// guards typeNeedsMatchDup with FirstFieldNestedClosureDeep so the value stays
// aliased — the `[]` body emits NEITHER a vector element-clone-null loop nor a
// deep-copy; the read is a borrow with envs intact.
func TestT1262_BareVecClosureMapReadNoDup(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		probe() {
			x := 7;
			m := Map[int, (() -> int)[]]();
			m[0] = [|| -> x];
			b := m[0]!;
			y := b[0]();
		}
		main() { probe(); }
	`)
	fn := codegentest.ExtractDefine(ir, `"Map[int, Vector[() -> int]].[]"`)
	if fn == "" {
		t.Fatalf("Map[int, Vector[() -> int]].[] not found in IR")
	}
	// The env-zeroing loop (dupVector's closure element clone) must not be emitted.
	codegentest.AssertNotContains(t, fn, "vecclonenull")
	// Nor a struct deep-copy.
	codegentest.AssertNotContains(t, fn, "heapdup.copy")
}

// Control for T1262: a bare NON-closure vector as the Map value
// (`Map[int, int[]]`) stays dup-safe, so the `[]` read still deep-copies the
// vector (a vecdup element loop / buffer copy). Guards the fix against
// over-suppressing the dup for ordinary value-copying containers. Reading the
// element out and mutating it must not alias the map's stored vector.
func TestT1262_BareIntVecMapReadDups(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		probe() {
			m := Map[int, int[]]();
			m[0] = [1, 2, 3];
			b := m[0]!;
		}
		main() { probe(); }
	`)
	fn := codegentest.ExtractDefine(ir, `"Map[int, Vector[int]].[]"`)
	if fn == "" {
		t.Fatalf("Map[int, Vector[int]].[] not found in IR")
	}
	// A non-closure vector value is duped on read (dupVector inlines a buffer copy).
	codegentest.AssertContains(t, fn, "vecdup.copy")
}

// T1926: the same shape as TestT1260_StructVecClosureMapReadNoDup, but with a
// USER by-value container that supplies its OWN clone(). Before T1926 the
// by-value branch was an identity list of Vector/Map/Set, so MyVecC fell through
// to the clone()-bearing stop and the read deep-copied the struct — zeroing the
// closure env (T0813) → null {fn,env} → SEGV on invoke. No hand-written clone can
// rebuild an opaque env, and Map has always been judged by-value FIRST despite
// having a clone() of its own; the property is now derived through MyVecC's
// Vector field, so a user container is judged the same way and the read is a
// borrow.
func TestT1926_StructUserVecClosureMapReadNoDup(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type MyVecC[T] { T[] items; clone() Self { return MyVecC[T](items: []); } }
		type FnU { MyVecC[() -> int] fns; }
		probe() {
			x := 6;
			m := Map[int, FnU]();
			m[0] = FnU(fns: MyVecC[() -> int](items: [|| -> x]));
			b := m[0]!;
			y := b.fns.items[0]();
		}
		main() { probe(); }
	`)
	fn := codegentest.ExtractDefine(ir, "__user.probe")
	if fn == "" {
		t.Fatalf("probe not found in IR")
	}
	codegentest.AssertNotContains(t, fn, "heapdup.copy")
}

// Control for the above: the same user container over a NON-closure element
// stays dup-safe, so the `[]` read still deep-copies the struct. Guards the
// derivation against over-suppressing the dup for ordinary by-value containers.
func TestT1926_StructUserVecIntMapReadDups(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type MyVec[T] { T[] items; }
		type IntU { MyVec[int] xs; }
		probe() {
			m := Map[int, IntU]();
			m[0] = IntU(xs: MyVec[int](items: [1, 2, 3]));
			b := m[0]!;
		}
		main() { probe(); }
	`)
	fn := codegentest.ExtractDefine(ir, `"Map[int, IntU].[]"`)
	if fn == "" {
		t.Fatalf("Map[int, IntU].[] not found in IR")
	}
	codegentest.AssertContains(t, fn, "heapdup.copy")
}

// T1926: a by-value container that ALSO holds a closure in a plain field
// (`MixedBag[T] { T[] items; () -> int cb; }`) must stay un-dup-safe when
// instantiated over a harmless element. The by-value branch's TypeArgs recursion
// finds nothing in `[int]`, so this is the guard for that branch falling THROUGH
// to the ordinary field walk instead of returning nil — which is what the branch
// it replaced did.
func TestT1926_ByValueContainerFieldWalkFallthrough(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type MixedBag[T] { T[] items; () -> int cb; }
		type Outer { MixedBag[int] bag; }
		probe() {
			x := 8;
			m := Map[int, Outer]();
			m[0] = Outer(bag: MixedBag[int](items: [1], cb: || -> x));
			b := m[0]!;
			y := b.bag.cb();
		}
		main() { probe(); }
	`)
	fn := codegentest.ExtractDefine(ir, "__user.probe")
	if fn == "" {
		t.Fatalf("probe not found in IR")
	}
	codegentest.AssertNotContains(t, fn, "heapdup.copy")
}

// T1926: the `seen`-ordering guard. A struct with TWO fields of the SAME user
// by-value container origin, one benign and one holding closures, must judge the
// second on its own type arguments. The by-value branch recurses TypeArgs before
// the field walk below it marks seen[origin], so field `b` is not swallowed by
// the mark left behind while walking field `a`.
func TestT1926_TwoFieldsSameByValueOriginSeenOrdering(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type MyVec[T] { T[] items; }
		type Pair { MyVec[int] a; MyVec[() -> int] b; }
		probe() {
			x := 9;
			m := Map[int, Pair]();
			m[0] = Pair(a: MyVec[int](items: [1]), b: MyVec[() -> int](items: [|| -> x]));
			p := m[0]!;
			y := p.b.items[0]();
		}
		main() { probe(); }
	`)
	fn := codegentest.ExtractDefine(ir, "__user.probe")
	if fn == "" {
		t.Fatalf("probe not found in IR")
	}
	codegentest.AssertNotContains(t, fn, "heapdup.copy")
}

// T1926: the by-value property may be reached through a TUPLE field rather than
// a plain one — `TupBag[T] { (int, T[]) pair; }` owns a buffer inside its tuple.
// The dup-safety walk has to descend tuple elements, or the struct holding it is
// judged dup-safe and the Map read deep-copies the nested closures (env zeroed →
// null {fn,env} → SEGV on invoke). The read must be a borrow.
func TestT1926_TupleHeldBufferOfClosuresNoDup(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type TupBag[T] { (int, T[]) pair; }
		type OuterT { TupBag[() -> int] b; }
		probe() {
			x := 12;
			m := Map[int, OuterT]();
			m[0] = OuterT(b: TupBag[() -> int](pair: (1, [|| -> x])));
			o := m[0]!;
			(n, fns) := o.b.pair;
			y := fns[0]();
		}
		main() { probe(); }
	`)
	fn := codegentest.ExtractDefine(ir, "__user.probe")
	if fn == "" {
		t.Fatalf("probe not found in IR")
	}
	codegentest.AssertNotContains(t, fn, "heapdup.copy")
}

// Control for the above: the same tuple-held buffer over a NON-closure element
// stays dup-safe, so the read is still an owned deep copy. Pins that descending
// tuples does not blanket-suppress the dup.
func TestT1926_TupleHeldBufferOfIntsMapReadDups(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type TupBag[T] { (int, T[]) pair; }
		type OuterI { TupBag[int] b; }
		probe() {
			m := Map[int, OuterI]();
			m[0] = OuterI(b: TupBag[int](pair: (1, [1, 2])));
			o := m[0]!;
		}
		main() { probe(); }
	`)
	fn := codegentest.ExtractDefine(ir, `"Map[int, OuterI].[]"`)
	if fn == "" {
		t.Fatalf("Map[int, OuterI].[] not found in IR")
	}
	codegentest.AssertContains(t, fn, "heapdup.copy")
}

// T1926/T1970: the by-value property survives INHERITANCE for the closure gate.
// `DerF[T] is BaseF[T]` reaches its buffer through the parent's field, which
// AllFields() reports typed in the PARENT's type parameter — the walk still sees
// a `duplicates_elements instance there, so the struct is un-dup-safe and the
// read is a borrow. (The type-argument occurrence check in
// duplicatingContainerElemTypes does NOT survive the same shape — that is T1970,
// a separate escape; this test pins the half that works so a fix for T1970
// cannot silently regress it.)
func TestT1926_InheritedBufferOfClosuresNoDup(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type BaseF[T] { T[] items; }
		type DerF[T] is BaseF[T] { int n; }
		type WrapF { DerF[() -> int] f; }
		probe() {
			x := 21;
			m := Map[int, WrapF]();
			m[0] = WrapF(f: DerF[() -> int](items: [|| -> x], n: 1));
			w := m[0]!;
			y := w.f.items[0]();
		}
		main() { probe(); }
	`)
	fn := codegentest.ExtractDefine(ir, "__user.probe")
	if fn == "" {
		t.Fatalf("probe not found in IR")
	}
	codegentest.AssertNotContains(t, fn, "heapdup.copy")
}
