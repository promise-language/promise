package codegen

import (
	"strings"
	"testing"
)

// TestT1130_VectorMapElementReadDeepClones pins the fix: a `Map` element read
// back out of a `Vector[Map[K, V]]` via `got := v[i]` must be deep-cloned through
// the element's `Map.clone()`. Map/Set are excluded from isDroppableHeapUserType
// (T0440), so the generic heap-user dup-on-read branch skips them — but when Map is
// the ELEMENT of a native Vector (whose `[]` does not dup), the bare read aliases
// the container's slot: got's drop + the vector's element walk double-free the Map.
func TestT1130_VectorMapElementReadDeepClones(t *testing.T) {
	ir := generateIR(t, `
		enum Tree { Leaf(int v), Branch(Tree[] kids), }
		caller() int {
			inner := Map[int, Tree]();
			inner[0] = Tree.Leaf(v: 1);
			maps := Vector[Map[int, Tree]]();
			maps.push(move inner);
			got := maps[0];
			return got.len;
		}
		main() { x := caller(); }
	`)
	body := extractDefine(ir, "__user.caller")
	if body == "" {
		t.Fatalf("expected @caller in IR")
	}
	if !strings.Contains(body, `@"Map[int, Tree].clone"`) {
		t.Errorf("T1130: `got := v[i]` of a Vector[Map[...]] element not deep-cloned "+
			"on read (no Map.clone call) → aliases the container slot, double-free at "+
			"scope exit:\n%s", body)
	}
}

// TestT1130_AssignVectorMapElementDeepClones pins the assignment form `got = v[i]`
// (genAssignStmt bareIdxRhs path) — same deep-clone requirement as the var-decl form.
func TestT1130_AssignVectorMapElementDeepClones(t *testing.T) {
	ir := generateIR(t, `
		enum Tree { Leaf(int v), Branch(Tree[] kids), }
		caller() int {
			inner := Map[int, Tree]();
			inner[0] = Tree.Leaf(v: 1);
			maps := Vector[Map[int, Tree]]();
			maps.push(move inner);
			got := Map[int, Tree]();
			got = maps[0];
			return got.len;
		}
		main() { x := caller(); }
	`)
	body := extractDefine(ir, "__user.caller")
	if body == "" {
		t.Fatalf("expected @caller in IR")
	}
	if !strings.Contains(body, `@"Map[int, Tree].clone"`) {
		t.Errorf("T1130: `got = v[i]` of a Vector[Map[...]] element not deep-cloned "+
			"on read (no Map.clone call) → aliases the container slot, double-free:\n%s", body)
	}
}

// TestT1130_TypedVarDeclVectorMapElementDeepClones pins the typed-var-decl form
// `Map[K,V] got = v[i]` (genTypedVarDecl flag site) — the third read-back site,
// distinct from the `:=` and `=` forms. Without the flag set here the read aliases
// the container's slot → double-free at scope exit (observed as a SIGSEGV).
func TestT1130_TypedVarDeclVectorMapElementDeepClones(t *testing.T) {
	ir := generateIR(t, `
		enum Tree { Leaf(int v), Branch(Tree[] kids), }
		caller() int {
			inner := Map[int, Tree]();
			inner[0] = Tree.Leaf(v: 1);
			maps := Vector[Map[int, Tree]]();
			maps.push(move inner);
			Map[int, Tree] got = maps[0];
			return got.len;
		}
		main() { x := caller(); }
	`)
	body := extractDefine(ir, "__user.caller")
	if body == "" {
		t.Fatalf("expected @caller in IR")
	}
	if !strings.Contains(body, `@"Map[int, Tree].clone"`) {
		t.Errorf("T1130: `Map[K,V] got = v[i]` of a Vector[Map[...]] element not deep-"+
			"cloned on read (no Map.clone call) → aliases the container slot, double-free:\n%s", body)
	}
}

// TestT1130_ArrayMapElementReadDeepClones pins the genArrayIndex arm: a Map element
// read back out of a fixed-size array `[m0, m1]` via `got := arr[i]` must deep-clone
// through Map.clone(). Arrays have no internal match-dup (unlike the Map/Vector []
// bodies), so a bare read aliases the array slot → double-free at scope exit. This
// is the array analogue of the Vector native-index path.
func TestT1130_ArrayMapElementReadDeepClones(t *testing.T) {
	ir := generateIR(t, `
		enum Tree { Leaf(int v), Branch(Tree[] kids), }
		caller() int {
			m0 := Map[int, Tree]();
			m0[0] = Tree.Leaf(v: 1);
			m1 := Map[int, Tree]();
			m1[0] = Tree.Leaf(v: 2);
			arr := [m0, m1];
			got := arr[0];
			return got.len;
		}
		main() { x := caller(); }
	`)
	body := extractDefine(ir, "__user.caller")
	if body == "" {
		t.Fatalf("expected @caller in IR")
	}
	if !strings.Contains(body, `@"Map[int, Tree].clone"`) {
		t.Errorf("T1130: `got := arr[i]` of a fixed-size array Map element not deep-"+
			"cloned on read (no Map.clone call) → aliases the array slot, double-free:\n%s", body)
	}
}
