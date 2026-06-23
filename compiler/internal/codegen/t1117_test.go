package codegen

import "testing"

// T1117: By-value read of a Ref/Arc-bearing Map value must increment the
// refcount so the bound copy owns an independent count. The `Map[K,V].[]`
// match-destructure body previously returned V by alias for a *direct* Ref/Arc
// element (typeNeedsMatchDup == false), so an owning bind (`h := m[k]!`)
// decremented the shared count on drop and corrupted the slot the Map still
// owns (UAF). The fix makes typeNeedsMatchDup / cloneResolvedValue dup a bare
// Arc/Weak element, so the `[]` body bumps the refcount before returning.
//
// The struct{Ref} and enum{Ref} shapes are dup'd inside the `[]` body too, but
// by the rebased-in base fixes: T1110 makes Arc/Weak struct fields safe-to-dup,
// and T1113/T1119 deep-dup droppable enum container reads. These tests assert
// the dup fires once, in the `[]` body, for each element shape — and, crucially,
// that the bind/inline call site does NOT add a second dup (an earlier T1117
// stmt.go path double-dup'd the enum forms on top of the in-body dup → leak).

// Direct `Map[int, Ref[int]]`: typeNeedsMatchDup now returns true for a bare Arc
// element, so cloneResolvedValue emits dupArc (arcdup.inc + atomic refcount
// increment) inside the synthesized `[]` body. Without it the read aliased the
// slot's Arc handle with no refcount bump → UAF on re-read after the bind drops.
func TestT1117_DirectRefMapReadDupsArc(t *testing.T) {
	ir := generateIR(t, `
		probe() {
			m := Map[int, Ref[int]]();
			m[1] = Ref[int](7);
			h := m[1]!;
		}
		main() { probe(); }
	`)
	fn := extractDefine(ir, `"Map[int, Ref[int]].[]"`)
	if fn == "" {
		t.Fatalf("Map[int, Ref[int]].[] not found in IR")
	}
	assertContains(t, fn, "arcdup.inc")
	assertContains(t, fn, "atomicrmw add")
}

// `Map[int, struct{Ref}]`: making the Arc field safe-to-dup in heapTypeSafeToDup
// makes RefBox match-dup'able, so the `[]` body deep-dups the struct (heapdup.copy
// = alloc + memcpy) and its Arc field (arcdup.inc) instead of aliasing it. This
// was the form that segfaulted on first access before the fix.
func TestT1117_StructRefMapReadDups(t *testing.T) {
	ir := generateIR(t, `
		type RefBox { Ref[int] r; int n; }
		probe() {
			m := Map[int, RefBox]();
			m[1] = RefBox(r: Ref[int](7), n: 11);
			h := m[1]!;
		}
		main() { probe(); }
	`)
	fn := extractDefine(ir, `"Map[int, RefBox].[]"`)
	if fn == "" {
		t.Fatalf("Map[int, RefBox].[] not found in IR")
	}
	assertContains(t, fn, "heapdup.copy")
	assertContains(t, fn, "arcdup.inc")
}

// `Map[int, enum{Ref}]` read-back: the `[]` body deep-dups the enum variant
// fields (enumdup.done switch) and bumps the Ref's strong count (arcdup.inc +
// atomicrmw add) before returning an owned copy, so a bind/assign consumer that
// drops the value never corrupts the slot the Map still owns. Post-rebase this
// dup lives inside the `[]` body (base T1113/T1119), NOT at the call site —
// TestT1117_EnumRefNoCallSiteDup guards against re-adding a second one.
func TestT1117_EnumRefBindReadDups(t *testing.T) {
	ir := generateIR(t, `
		enum RefEnum {
		  V(Ref[int] r),
		  N,
		}
		probe() {
			m := Map[int, RefEnum]();
			m[1] = RefEnum.V(Ref[int](7));
			h := m[1]!;
		}
		main() { probe(); }
	`)
	fn := extractDefine(ir, `"Map[int, RefEnum].[]"`)
	if fn == "" {
		t.Fatalf("Map[int, RefEnum].[] not found in IR")
	}
	assertContains(t, fn, "enumdup.done")
	assertContains(t, fn, "arcdup.inc")
}

// `Map[int, enum{Ref}]` INLINE form (`match m[k]!`): the `[]` body returns an
// owned (refcount-bumped) copy that the match consumes and drops, so it stays
// balanced. The call site (probe) must NOT emit its own enum dup — the dup
// belongs in the `[]` body. This guards against a call-site dup leaking the
// inline temporary's extra refcount.
func TestT1117_EnumRefInlineReadNoDup(t *testing.T) {
	ir := generateIR(t, `
		enum RefEnum {
		  V(Ref[int] r),
		  N,
		}
		probe() {
			m := Map[int, RefEnum]();
			m[1] = RefEnum.V(Ref[int](7));
			match m[1]! {
			  RefEnum.V(r) => { assert(r.borrow == 7, "x"); },
			  _ => {},
			}
		}
		main() { probe(); }
	`)
	fn := extractFunction(ir, "__user.probe")
	if fn == "" {
		t.Fatalf("__user.probe not found in IR")
	}
	assertNotContains(t, fn, "enumdup")
}

// Direct `Map[int, Weak[int]]`: the Weak sibling of the Arc case. typeNeedsMatchDup
// now returns true for a bare Weak element, so cloneResolvedValue emits dupWeak
// (weakdup.inc + atomic weak-count increment) inside the synthesized `[]` body.
func TestT1117_DirectWeakMapReadDupsWeak(t *testing.T) {
	ir := generateIR(t, `
		probe() {
			r := Ref[int](7);
			m := Map[int, Weak[int]]();
			m[1] = r.downgrade();
			h := m[1]!;
		}
		main() { probe(); }
	`)
	fn := extractDefine(ir, `"Map[int, Weak[int]].[]"`)
	if fn == "" {
		t.Fatalf("Map[int, Weak[int]].[] not found in IR")
	}
	assertContains(t, fn, "weakdup.inc")
	assertContains(t, fn, "atomicrmw add")
}

// `Map[int, struct{Weak}]`: the Weak-field branch of heapTypeSafeToDup makes the
// struct match-dup'able, so the `[]` body deep-dups the struct (heapdup.copy) and
// its Weak field via dupWeak (weakdup.inc) instead of aliasing it — the Weak
// analog of TestT1117_StructRefMapReadDups.
func TestT1117_StructWeakMapReadDups(t *testing.T) {
	ir := generateIR(t, `
		type WeakBox { Weak[int] w; int n; }
		probe() {
			r := Ref[int](7);
			m := Map[int, WeakBox]();
			m[1] = WeakBox(w: r.downgrade(), n: 11);
			h := m[1]!;
		}
		main() { probe(); }
	`)
	fn := extractDefine(ir, `"Map[int, WeakBox].[]"`)
	if fn == "" {
		t.Fatalf("Map[int, WeakBox].[] not found in IR")
	}
	assertContains(t, fn, "heapdup.copy")
	assertContains(t, fn, "weakdup.inc")
}

// Regression guard for the rebase double-dup leak. The enum Ref dup happens
// exactly once, inside `Map[int, RefEnum].[]` (see TestT1117_EnumRefBindReadDups).
// Neither the fresh bind (`x := m[1]!`) nor the reassignment (`x = m[2]!`) call
// site may emit a *second* enum dup: an earlier T1117 stmt.go path
// (containerReadEnumNeedsDup, set at the var-decl and assign sites) added a
// call-site dup on top of the in-body dup, double-incrementing the Ref's strong
// count → leak. The `[]` body already returns an owned copy the assignment
// simply drops/overwrites.
func TestT1117_EnumRefNoCallSiteDup(t *testing.T) {
	ir := generateIR(t, `
		enum RefEnum {
		  V(Ref[int] r),
		  N,
		}
		probe() {
			m := Map[int, RefEnum]();
			m[1] = RefEnum.V(Ref[int](7));
			m[2] = RefEnum.V(Ref[int](8));
			x := m[1]!;
			x = m[2]!;
		}
		main() { probe(); }
	`)
	fn := extractFunction(ir, "__user.probe")
	if fn == "" {
		t.Fatalf("__user.probe not found in IR")
	}
	assertNotContains(t, fn, "enumdup")
}
