package codegen

import (
	"strings"
	"testing"
)

// TestT1129_RecursiveEnumMapReadSynthClone pins the fix: a recursive enum
// (`Tree.Branch(Tree[] kids)`) stored in a Map gets a compiler-synthesized
// recursive clone (`@Tree.clone`) so the `Map[int, Tree].[]` read-back deep-copies
// the value instead of aliasing the slot. Pre-fix the read returned an alias the
// Map still owned → double-free / segfault on `m[k]!` + match. Two invariants:
//   - `Tree.clone` is *defined* (a real recursive clone body), not just declared.
//   - The synthesized clone recurses via a *call* to itself (through the Vector
//     element clone loop), not by inline unrolling — that is what makes
//     depth-≥2 trees copy correctly without infinite codegen.
func TestT1129_RecursiveEnumMapReadSynthClone(t *testing.T) {
	ir := generateIR(t, `
		enum Tree { Leaf(int v), Branch(Tree[] kids), }
		caller() int {
			outer := Map[int, Tree]();
			kids := Vector[Tree]();
			kids.push(Tree.Leaf(v: 1));
			outer[1] = Tree.Branch(kids: move kids);
			got := outer[1]!;
			n := 0;
			match got { Branch(ks) => { n = ks.len; }, Leaf(_) => {} }
			return n;
		}
		main() { x := caller(); }
	`)

	clone := extractDefine(ir, "Tree.clone")
	if clone == "" {
		t.Fatalf("T1129: expected a *defined* @Tree.clone (synthesized recursive clone), got none:\n%s", ir)
	}
	// The synthesized clone must recurse through a call to itself (via the Vector
	// element clone loop) rather than inline unrolling.
	if !strings.Contains(clone, "@Tree.clone") {
		t.Errorf("T1129: synthesized Tree.clone does not recurse via a call to @Tree.clone "+
			"(would mean shallow/inline copy → wrong at depth ≥ 2):\n%s", clone)
	}

	// The Map read-back path must route through the synthesized clone.
	if !strings.Contains(ir, "call %promise_Tree_enum @Tree.clone") {
		t.Errorf("T1129: no call to @Tree.clone — Map[int, Tree] read-back does not "+
			"deep-clone the recursive enum (would alias the slot → UAF):\n%s", ir)
	}
}

// TestT1129_VectorIndexReadDupsDroppableEnum pins the Vector/Array analog: a bare
// `got := v[i]` of a droppable enum must deep-clone on read (cloneResolvedValue),
// else `got` aliases the vector slot and got's drop + the vector's element walk
// double-free the variant data (silent for leaf enums, fatal for recursive ones).
func TestT1129_VectorIndexReadDupsDroppableEnum(t *testing.T) {
	ir := generateIR(t, `
		enum Tree { Leaf(int v), Branch(Tree[] kids), }
		caller() int {
			v := Vector[Tree]();
			kids := Vector[Tree]();
			kids.push(Tree.Leaf(v: 1));
			v.push(Tree.Branch(kids: move kids));
			got := v[0];
			n := 0;
			match got { Branch(ks) => { n = ks.len; }, Leaf(_) => {} }
			return n;
		}
		main() { x := caller(); }
	`)
	body := extractDefine(ir, "__user.caller")
	if body == "" {
		t.Fatalf("expected @caller in IR")
	}
	if !strings.Contains(body, "call %promise_Tree_enum @Tree.clone") {
		t.Errorf("T1129: `got := v[0]` of a droppable enum not deep-cloned on read "+
			"(no @Tree.clone call) → aliases the slot, double-free at scope exit:\n%s", body)
	}
}

// TestT1129_ArrayIndexReadDupsDroppableEnum pins the fixed-size-array analog of the
// Vector dup-on-read: `got := arr[i]` of a droppable enum routes through
// genArrayIndex's clone branch (distinct codegen from genVectorIndex), so the
// binding owns independent variant data. Without it, got aliases the array slot and
// got's drop + the array's element walk double-free the variant data.
func TestT1129_ArrayIndexReadDupsDroppableEnum(t *testing.T) {
	ir := generateIR(t, `
		enum Tree { Leaf(int v), Branch(Tree[] kids), }
		caller() int {
			kids := Vector[Tree]();
			kids.push(Tree.Leaf(v: 1));
			arr := [Tree.Branch(kids: move kids), Tree.Leaf(v: 9)];
			got := arr[0];
			n := 0;
			match got { Branch(ks) => { n = ks.len; }, Leaf(_) => {} }
			return n;
		}
		main() { x := caller(); }
	`)
	body := extractDefine(ir, "__user.caller")
	if body == "" {
		t.Fatalf("expected @caller in IR")
	}
	if !strings.Contains(body, "call %promise_Tree_enum @Tree.clone") {
		t.Errorf("T1129: `got := arr[0]` of a droppable enum not deep-cloned on read "+
			"(no @Tree.clone call) → aliases the array slot, double-free at scope exit:\n%s", body)
	}
}
