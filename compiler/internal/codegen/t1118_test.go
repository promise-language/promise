package codegen

import (
	"strings"
	"testing"
)

// T1118: An enum whose variant payload is a *container* (a Map, or a Vector of
// droppable elements) stored inside another Map double-freed on read-back via
// `m[k]!` + match. The read returned a value that aliased the container's owned
// inner buffer, so both the returned copy and the map's owned copy freed the
// same heap block → `fatal: invalid free (bad header magic)`.
//
// Map[K,V].[]'s synthesized `Used(k, v) => return v` match-destructure must dup
// the returned enum value. For an enum without a clone() method the dup is gated
// by matchDupFieldSafe → enumMatchDupSafe. Before the fix a Map/droppable-Vector
// variant field made the whole enum non-dup-safe → no dup → alias → double-free.
// The fix admits these field shapes (whose deep-copy machinery already exists in
// emitVariantFieldDup) while the `seen` cycle guard keeps recursive enums excluded.
//
// The runtime no-double-free proof is in tests/e2e/enum_container_dup_test.pr.

// TestT1118_MapEnumWithMapField pins the Map-bearing variant case: the read-back
// of a Map[int, MapHolder] where MapHolder.M carries a Map[int, int] must deep-copy
// the inner map via its clone(), so the returned MapHolder owns an independent map.
func TestT1118_MapEnumWithMapField(t *testing.T) {
	ir := generateIR(t, `
		enum MapHolder { M(Map[int, int] inner) }
		caller() int {
			outer := Map[int, MapHolder]();
			inner := Map[int, int]();
			inner[1] = 100;
			outer[1] = MapHolder.M(move inner);
			v := outer[1]!;
			total := 0;
			match v { M(m) => { total = m[1]!; } }
			return total;
		}
		main() { x := caller(); }
	`)
	body := extractDefine(ir, `"Map[int, MapHolder].[]"`)
	if body == "" {
		t.Fatalf("expected @\"Map[int, MapHolder].[]\" in IR")
	}
	// The match-destructured MapHolder value must deep-copy its Map variant field
	// via the inner map's clone() — otherwise the returned value aliases the slot's
	// owned inner Map and both free it.
	if !strings.Contains(body, `@"Map[int, int].clone"`) {
		t.Errorf("T1118: Map[int, MapHolder].[] does not deep-copy the inner Map "+
			"(no call to @\"Map[int, int].clone\") — the match-destructured enum "+
			"value aliases the map's owned inner Map → double-free:\n%s", body)
	}
}

// TestT1118_MapEnumWithDroppableVector pins the droppable-Vector variant case:
// VecHolder.V carries a Vector[string]; on read-back the variant field must be
// deep-copied element-by-element (the per-element string dup loop), not aliased.
func TestT1118_MapEnumWithDroppableVector(t *testing.T) {
	ir := generateIR(t, `
		enum VecHolder { V(string[] items) }
		caller() int {
			outer := Map[int, VecHolder]();
			v := Vector[string]();
			v.push("a");
			outer[1] = VecHolder.V(move v);
			got := outer[1]!;
			total := 0;
			match got { V(items) => { total = items.len; } }
			return total;
		}
		main() { x := caller(); }
	`)
	body := extractDefine(ir, `"Map[int, VecHolder].[]"`)
	if body == "" {
		t.Fatalf("expected @\"Map[int, VecHolder].[]\" in IR")
	}
	// The droppable vector field must be deep-copied via dupVector +
	// emitVectorElementCloneLoop — the per-element string dup loop (vecdup_str).
	if !strings.Contains(body, "vecdup_str") {
		t.Errorf("T1118: Map[int, VecHolder].[] does not deep-copy the droppable "+
			"vector elements (no vecdup_str loop) — the match-destructured enum "+
			"value aliases the map's owned Vector buffer → double-free:\n%s", body)
	}
}
