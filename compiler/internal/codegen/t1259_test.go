package codegen

import "testing"

// T1259: reading an enum whose variant carries a closure FIELD out of a by-value
// container (Vector or Map), destructuring the variant, and invoking the closure
// must NOT deep-copy the enum's closure env. The env (captured frame) is opaque and
// cannot be cloned; emitVariantFieldDup NULLS the cloned closure slot (T0813),
// yielding a null {fn,env} fat pointer → SEGV on invoke. Sibling of T1230 (struct
// closure field) and T1230's enum counterpart.
//
// Two halves:
//   1. `matchDupFieldSafe(*types.Signature)` reports un-safe, so `enumMatchDupSafe(E)`
//      is false → `Map.[]`'s internal match-destructure does NOT dup V=E (which would
//      zero the env). The read stays a borrow.
//   2. bindEnumDestructure marks a closure variant-field binding match-borrowed, and
//      maybeRegisterEnvFree honors that set for an ident source, so a downstream
//      `h := g` var-decl does NOT register an owning env-free binding (which would
//      double-free the env against the enum's own drop).

// The `Map[int, E].[]` body must NOT dup the closure-nesting enum value — a dup routes
// through dupEnumElementInPlace, whose emitVariantFieldDup Signature case zeroes the
// closure slot (enumdup.* + a zeroinitializer store), corrupting the returned env.
func TestT1259_EnumClosureMapReadNoDup(t *testing.T) {
	ir := generateIR(t, `
		enum E { Cb(() -> int f), Empty }
		probe() {
			x := 9;
			m := Map[int, E]();
			m[0] = E.Cb(f: || -> x);
			e := m[0]!;
			r := 0;
			match e {
				E.Cb(g) => { h := g; r = h(); },
				E.Empty => { r = 1; },
			}
		}
		main() { probe(); }
	`)
	fn := extractDefine(ir, `"Map[int, E].[]"`)
	if fn == "" {
		t.Fatalf("Map[int, E].[] not found in IR")
	}
	// A dup of the enum element would switch on the tag and dup variant fields.
	assertNotContains(t, fn, "enumdup.")
}

// The closure reached through a STRUCT-typed variant field (`Wrap(Holder w)` where
// `Holder { () -> int cb; }`) must also keep the enum a borrow on read. Here
// matchDupFieldSafe hits the heapTypeSafeToDup gate (T1230), not the direct
// *types.Signature case — so `Map.[]` still must NOT dup the enum value.
func TestT1259_EnumStructClosureMapReadNoDup(t *testing.T) {
	ir := generateIR(t, `
		type Holder { () -> int cb; }
		enum W { Wrap(Holder w), Empty }
		probe() {
			x := 4;
			m := Map[int, W]();
			m[0] = W.Wrap(w: Holder(cb: || -> x));
			e := m[0]!;
			r := 0;
			match e {
				W.Wrap(w) => { r = w.cb(); },
				W.Empty => { r = 1; },
			}
		}
		main() { probe(); }
	`)
	fn := extractDefine(ir, `"Map[int, W].[]"`)
	if fn == "" {
		t.Fatalf("Map[int, W].[] not found in IR")
	}
	assertNotContains(t, fn, "enumdup.")
}

// Control: a closure-free droppable enum (string variant field) IS shallow-dup-safe,
// so `Map.[]` still dups it on read (enumdup.*) — the returned value owns an
// independent string. Guards against the fix over-suppressing dup for ordinary
// droppable enums.
func TestT1259_StringEnumMapReadDups(t *testing.T) {
	ir := generateIR(t, `
		enum S { Text(string s), Empty }
		probe() {
			m := Map[int, S]();
			m[0] = S.Text(s: "hi");
			e := m[0]!;
		}
		main() { probe(); }
	`)
	fn := extractDefine(ir, `"Map[int, S].[]"`)
	if fn == "" {
		t.Fatalf("Map[int, S].[] not found in IR")
	}
	assertContains(t, fn, "enumdup.")
}
