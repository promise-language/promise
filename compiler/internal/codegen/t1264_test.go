package codegen

import "testing"

// T1264: an enum variant carrying a VALUE-COPYING container of closures
// (`E.Fns(Vector[() -> int] fs)`) stored in a Map, read back (`m[0]!`),
// destructured, and a closure invoked. The Map's `[]` deep-copied the enum value;
// dupVector's element-clone path (reached via the enum-dup walk over the variant
// field) zeroes each closure element's opaque env (T0813) → null {fn,env} → SEGV
// on invoke. The fix widens matchDupFieldSafe to consult FirstFieldNestedClosureDeep
// (single source of truth with typeNeedsMatchDup and the borrow gates), so the enum
// stays dup-un-safe → `Map[int, E].[]` leaves the variant data ALIASED (no
// enum-dup walk) and the read is a true borrow with envs intact.
//
// See matchDupFieldSafe / bindEnumDestructure (codegen/expr.go),
// identBorrowsMatchBorrowedClosure / maybeRegisterEnvFree (codegen/stmt.go),
// FirstFieldNestedClosureDeep (sema/clone.go).

// `Map[int, E].[]` for a Vector-of-closures variant must NOT emit the enum-dup walk
// (enumdup) — the read stays a borrow, so the closure envs are not cloned/zeroed.
func TestT1264_EnumVecClosureMapReadNoDup(t *testing.T) {
	ir := generateIR(t, `
		enum E { Fns(Vector[() -> int] fs), Empty }
		probe() {
			x := 5;
			m := Map[int, E]();
			m[0] = E.Fns(fs: [|| -> x]);
			e := m[0]!;
			y := 0;
			match e { E.Fns(gs) => { y = gs[0](); }, E.Empty => {}, }
		}
		main() { probe(); }
	`)
	fn := extractDefine(ir, `"Map[int, E].[]"`)
	if fn == "" {
		t.Fatalf("Map[int, E].[] not found in IR")
	}
	// An enum-dup walk over the variant field would clone the inner vector and zero
	// each closure's env slot.
	assertNotContains(t, fn, "enumdup")
}

// A direct closure variant field (`E.Cb(() -> int f)`, T1259) is likewise
// dup-un-safe: `Map[int, E].[]` must not emit the enum-dup walk.
func TestT1264_EnumDirectClosureMapReadNoDup(t *testing.T) {
	ir := generateIR(t, `
		enum E { Cb(() -> int f), None }
		probe() {
			x := 5;
			m := Map[int, E]();
			m[0] = E.Cb(f: || -> x);
			e := m[0]!;
			y := 0;
			match e { E.Cb(g) => { y = g(); }, E.None => {}, }
		}
		main() { probe(); }
	`)
	fn := extractDefine(ir, `"Map[int, E].[]"`)
	if fn == "" {
		t.Fatalf("Map[int, E].[] not found in IR")
	}
	assertNotContains(t, fn, "enumdup")
}

// A GENERIC enum whose variant carries a closure container (`Box[T].Fns(Vector[() ->
// T] fs)`), monomorphized at `Box[int]`, must also stay dup-un-safe: matchDupFieldSafe
// runs on the substituted variant field (`Vector[() -> int]`), so `Map[int,
// Box[int]].[]` must not emit the enum-dup walk. Pins that the Deep predicate sees
// through the type-param substitution rather than treating `() -> T` as opaque.
func TestT1264_GenericEnumClosureContainerMapReadNoDup(t *testing.T) {
	ir := generateIR(t, `
		enum Box[T] { Fns(Vector[() -> T] fs), Empty }
		probe() {
			x := 5;
			m := Map[int, Box[int]]();
			m[0] = Box[int].Fns(fs: [|| -> x]);
			e := m[0]!;
			y := 0;
			match e { Box[int].Fns(gs) => { hs := gs; y = hs[0](); }, Box[int].Empty => {}, }
		}
		main() { probe(); }
	`)
	fn := extractDefine(ir, `"Map[int, Box[int]].[]"`)
	if fn == "" {
		t.Fatalf("Map[int, Box[int]].[] not found in IR")
	}
	assertNotContains(t, fn, "enumdup")
}

// Control: a variant carrying a value-copying container of a NON-closure element
// (`E.Words(Vector[string] ws)`) stays dup-safe, so the `[]` read STILL emits the
// enum-dup walk — the bound local owns an independent deep copy. Guards the fix's
// Deep-predicate recursion against over-suppressing ordinary containers.
func TestT1264_EnumStringVecMapReadDups(t *testing.T) {
	ir := generateIR(t, `
		enum EW { Words(Vector[string] ws), Empty }
		probe() {
			m := Map[int, EW]();
			m[0] = EW.Words(ws: ["a"]);
			e := m[0]!;
		}
		main() { probe(); }
	`)
	fn := extractDefine(ir, `"Map[int, EW].[]"`)
	if fn == "" {
		t.Fatalf("Map[int, EW].[] not found in IR")
	}
	assertContains(t, fn, "enumdup")
}

// Control: a variant carrying a REFCOUNTED handle (`E.R(Ref[int] r)`) stays
// dup-safe — FirstFieldNestedClosureDeep keeps Ref opaque, so the `[]` read still
// emits the enum-dup walk (a refcount-increment copy). Pins that the fix does not
// over-suppress refcounted nesting.
func TestT1264_EnumRefMapReadDups(t *testing.T) {
	ir := generateIR(t, `
		enum ER { R(Ref[int] r), None }
		probe() {
			m := Map[int, ER]();
			m[0] = ER.R(r: Ref[int](1));
			e := m[0]!;
		}
		main() { probe(); }
	`)
	fn := extractDefine(ir, `"Map[int, ER].[]"`)
	if fn == "" {
		t.Fatalf("Map[int, ER].[] not found in IR")
	}
	assertContains(t, fn, "enumdup")
}
