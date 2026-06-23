package codegen

import (
	"strings"
	"testing"
)

// TestT1110_MapEnumStructRefMatchDup pins the codegen fix for T1110: a struct
// carrying a Ref, wrapped in an enum variant, stored in a Map, double-freed
// during scope-exit cleanup. Map[K,V].[] is Promise code whose internal
// `match this._buckets[h] { Used(k, v) => return v }` must dup the destructured
// value so the returned Holder is an independent copy — for the nested Ref that
// means a refcount bump (dupArc → an `arcdup.inc` block). Pre-fix two predicates
// gated the dup off: heapTypeSafeToDup rejected the Ref-bearing struct P (its Ref
// field has an explicit drop), and matchFieldNeedsDup never recognized a
// droppable enum without clone(). Without the bump the returned Holder aliases
// the map's owned Ref → double-free when both drop.
//
// The runtime no-double-free proof is in tests/e2e/map_enum_struct_ref_test.pr.
func TestT1110_MapEnumStructRefMatchDup(t *testing.T) {
	ir := generateIR(t, `
		type P { Ref[int] r; int n; }
		enum Holder { Pair(P p) }
		caller() int {
			m := Map[int, Holder]();
			m[1] = Holder.Pair(P(Ref[int](9), 2));
			v := m[1]!;
			total := 0;
			match v { Pair(p) => { total = p.n; } }
			return total;
		}
		main() { x := caller(); }
	`)
	body := extractDefine(ir, `"Map[int, Holder].[]"`)
	if body == "" {
		t.Fatalf("expected @\"Map[int, Holder].[]\" in IR")
	}
	// The match destructure of the Used(k, v) slot must dup the Holder value,
	// which deep-copies the variant's P struct and bumps its Ref refcount.
	if !strings.Contains(body, "arcdup.inc") {
		t.Errorf("T1110: Map[int, Holder].[] does not bump the nested Ref refcount "+
			"(no arcdup.inc) — the match-destructured enum value aliases the map's "+
			"owned Ref → double-free:\n%s", body)
	}
}

// TestT1110_StructWithRefMatchDup pins the narrower struct-with-Ref case
// (Map[int, P]): heapTypeSafeToDup must treat the Ref field as dup-safe so the
// containing struct is duppable and the returned P bumps the Ref refcount.
func TestT1110_StructWithRefMatchDup(t *testing.T) {
	ir := generateIR(t, `
		type P { Ref[int] r; int n; }
		caller() int {
			m := Map[int, P]();
			m[1] = P(Ref[int](9), 2);
			v := m[1]!;
			return v.n;
		}
		main() { x := caller(); }
	`)
	body := extractDefine(ir, `"Map[int, P].[]"`)
	if body == "" {
		t.Fatalf("expected @\"Map[int, P].[]\" in IR")
	}
	if !strings.Contains(body, "arcdup.inc") {
		t.Errorf("T1110: Map[int, P].[] does not bump the Ref refcount (no "+
			"arcdup.inc) — the match-destructured struct aliases the map's owned "+
			"Ref → double-free:\n%s", body)
	}
}

// TestT1110_MatchDupFieldKinds exercises every accepting branch of
// enumMatchDupSafe / matchDupFieldSafe: a Ref-bearing variant paired with each
// shallow-dup-safe field kind (string, non-droppable vector, generic type
// param, nested enum, tuple). For all of them the read-back Holder must still
// bump the nested Ref refcount (arcdup.inc) — proving matchFieldNeedsDup chose
// the shallow-dup path rather than aliasing. (Runtime no-double-free proof:
// tests/e2e/map_enum_struct_ref_test.pr.)
func TestT1110_MatchDupFieldKinds(t *testing.T) {
	cases := []struct {
		name   string
		mapKey string // mangled Map[].[] owner key
		src    string
	}{
		{
			name:   "StringField", // matchDupFieldSafe string branch
			mapKey: `"Map[int, HS].[]"`,
			src: `
				type P { Ref[int] r; int n; }
				enum HS { V(P p, string s) }
				caller() int {
					m := Map[int, HS]();
					m[1] = HS.V(P(Ref[int](9), 2), "hi");
					v := m[1]!;
					total := 0;
					match v { V(p, s) => { total = p.n; } }
					return total;
				}
				main() { x := caller(); }`,
		},
		{
			name:   "VectorNonDroppableField", // vector branch (non-droppable elem → safe)
			mapKey: `"Map[int, HV].[]"`,
			src: `
				type P { Ref[int] r; int n; }
				enum HV { V(P p, int[] xs) }
				caller() int {
					m := Map[int, HV]();
					m[1] = HV.V(P(Ref[int](5), 1), [10, 20]);
					v := m[1]!;
					total := 0;
					match v { V(p, xs) => { total = p.n; } }
					return total;
				}
				main() { x := caller(); }`,
		},
		{
			name:   "ChannelField", // matchDupFieldSafe channel branch
			mapKey: `"Map[int, HC].[]"`,
			src: `
				type P { Ref[int] r; int n; }
				enum HC { V(P p, channel[int] ch) }
				caller() int {
					m := Map[int, HC]();
					c := channel[int](1);
					m[1] = HC.V(P(Ref[int](9), 2), move c);
					v := m[1]!;
					total := 0;
					match v { V(p, ch) => { total = p.n; } }
					return total;
				}
				main() { x := caller(); }`,
		},
		{
			name:   "GenericEnum", // enumMatchDupSafe Instance/BuildSubstMap path
			mapKey: `"Map[int, Box[P]].[]"`,
			src: `
				type P { Ref[int] r; int n; }
				enum Box[T] { Some(T v) }
				caller() int {
					m := Map[int, Box[P]]();
					m[1] = Box[P].Some(P(Ref[int](7), 3));
					v := m[1]!;
					total := 0;
					match v { Some(p) => { total = p.n; } }
					return total;
				}
				main() { x := caller(); }`,
		},
		{
			name:   "NestedEnum", // matchDupFieldSafe nested-enum recursion + seen set
			mapKey: `"Map[int, Outer].[]"`,
			src: `
				type P { Ref[int] r; int n; }
				enum Inner { I(P p) }
				enum Outer { O(Inner i) }
				caller() int {
					m := Map[int, Outer]();
					m[1] = Outer.O(Inner.I(P(Ref[int](4), 8)));
					v := m[1]!;
					total := 0;
					match v { O(i) => { total = 1; } }
					return total;
				}
				main() { x := caller(); }`,
		},
		{
			name:   "TupleField", // matchDupFieldSafe tuple branch
			mapKey: `"Map[int, HT].[]"`,
			src: `
				enum HT { T2((Ref[int], int) t) }
				caller() int {
					m := Map[int, HT]();
					m[1] = HT.T2((Ref[int](6), 9));
					v := m[1]!;
					total := 0;
					match v { T2(t) => { total = 1; } }
					return total;
				}
				main() { x := caller(); }`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ir := generateIR(t, tc.src)
			body := extractDefine(ir, tc.mapKey)
			if body == "" {
				t.Fatalf("expected @%s in IR", tc.mapKey)
			}
			if !strings.Contains(body, "arcdup.inc") {
				t.Errorf("T1110 %s: %s does not bump the nested Ref refcount "+
					"(no arcdup.inc) — match-destructured value aliases the map's "+
					"owned Ref → double-free:\n%s", tc.name, tc.mapKey, body)
			}
		})
	}
}
