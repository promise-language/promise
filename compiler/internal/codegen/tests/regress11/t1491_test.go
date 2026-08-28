package regress11

import (
	"strings"
	"testing"

	"github.com/promise-language/promise/compiler/internal/codegen/codegentest"
)

// TestT1491_ReturnHeapUserVectorElementDups pins the Vector sibling of T1488: a
// direct `return xs[i]` / `=> xs[i]` of a heap-user element out of a Vector param
// must dup-on-read the vector slot's owned instance. T0398/T0903 armed
// genVectorIndex's dup only at var-decl/assignment RHS sites (via
// dupHeapUserFieldAccess); the direct return/arrow escape never armed it, so the
// returned value aliased the vector's owned slot → the vector's element-walk drop
// and the caller's drop of the returned value double-freed at scope exit.
// genVectorIndex's dup path emits a `heapdup.copy` block that pal_allocs + memcpys
// the instance; assert it fires inside the returning function.
func TestT1491_ReturnHeapUserVectorElementDups(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Boxed { int v; }
		firstv(Boxed[] xs) Boxed => xs[0];
		main() { }
	`)

	fn := codegentest.ExtractDefine(ir, "__user.firstv")
	if fn == "" {
		t.Fatalf("T1491: could not find @__user.firstv in IR:\n%s", ir)
	}
	if !strings.Contains(fn, "heapdup.copy") || !strings.Contains(fn, "@pal_alloc") {
		t.Errorf("T1491: `=> xs[0]` did not dup the heap-user vector element "+
			"(no heapdup.copy/pal_alloc) — returned value aliases the vector slot "+
			"→ double free at scope exit:\n%s", fn)
	}
}

// TestT1491_GenericReturnHeapUserVectorElementDups: the same escape through a
// GENERIC `first[T](T[] xs) T => xs[0]` instantiated at T=Boxed must dup — the
// return-site arming applies typeSubst to the index target type so the generic
// code path is covered.
func TestT1491_GenericReturnHeapUserVectorElementDups(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Boxed { int v; }
		first[T](T[] xs) T => xs[0];
		main() {
			Boxed[] v = [Boxed(v: 5), Boxed(v: 6)];
			b := first[Boxed](v);
		}
	`)

	fn := codegentest.ExtractDefine(ir, "first[Boxed]")
	if fn == "" {
		t.Fatalf("T1491: could not find generic instance @\"first[Boxed]\" in IR:\n%s", ir)
	}
	if !strings.Contains(fn, "heapdup.copy") || !strings.Contains(fn, "@pal_alloc") {
		t.Errorf("T1491: generic `=> xs[0]` at T=Boxed did not dup the heap-user "+
			"vector element → double free:\n%s", fn)
	}
}

// TestT1491_ReturnIntVectorElementNoDup: the arming is a permission consumed by
// genVectorIndex only when the element shape is a heap-user/Map-Set; an `int[]`
// element return is a plain scalar copy and must NOT emit a heap dup.
func TestT1491_ReturnIntVectorElementNoDup(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		firsti(int[] xs) int => xs[0];
		main() { }
	`)

	fn := codegentest.ExtractDefine(ir, "__user.firsti")
	if fn == "" {
		t.Fatalf("T1491: could not find @__user.firsti in IR:\n%s", ir)
	}
	if strings.Contains(fn, "heapdup.copy") {
		t.Errorf("T1491: `=> xs[0]` on an int[] vector spuriously emitted a heap dup "+
			"(should be a plain scalar copy):\n%s", fn)
	}
}

// TestT1491_ReturnEnumVectorElementNoDoubleClone: a droppable-enum element
// returned out of a Vector must NOT be cloned twice. The post-hoc cloneEnumValue
// block (stmt_control.go) already handles droppable-enum vector elements; the
// T1491 arm deliberately EXCLUDES the droppable-enum shape (enumElemNeedsDupOnRead)
// so we don't emit a second clone/dup (which would leak). Assert the returning
// function has at most one enum-clone site (cloneEnumValue emits an
// `enum.clone.tmp` alloca per call) and no heap dup from the field-access arm.
func TestT1491_ReturnEnumVectorElementNoDoubleClone(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		enum Msg { Text(string); Ping; }
		firstm(Msg[] xs) Msg => xs[0];
		main() { }
	`)

	fn := codegentest.ExtractDefine(ir, "__user.firstm")
	if fn == "" {
		t.Fatalf("T1491: could not find @__user.firstm in IR:\n%s", ir)
	}
	if n := strings.Count(fn, "enum.clone.tmp"); n > 1 {
		t.Errorf("T1491: droppable-enum vector element return emitted %d clone sites "+
			"(expected at most 1) — the Vector arm must exclude enumElemNeedsDupOnRead:\n%s", n, fn)
	}
	if strings.Contains(fn, "heapdup.copy") {
		t.Errorf("T1491: droppable-enum vector element return emitted a heapdup.copy — "+
			"the T1491 arm wrongly fired on an enum element (double-clone/leak):\n%s", fn)
	}
}

// TestT1491_ReturnDroppableHeapUserVectorElementDups exercises the
// isDroppableHeapUserType predicate of the Vector arm (distinct from the
// isHeapUserNoDropPalFree Boxed case): a heap-user with an inner droppable
// (string) field returned out of a Vector must dup, else the vector's
// element-walk drop frees the inner string the returned alias still points at
// → SIGSEGV/UAF at scope exit.
func TestT1491_ReturnDroppableHeapUserVectorElementDups(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type WithStr { string s; }
		firsts(WithStr[] xs) WithStr => xs[0];
		main() { }
	`)

	fn := codegentest.ExtractDefine(ir, "__user.firsts")
	if fn == "" {
		t.Fatalf("T1491: could not find @__user.firsts in IR:\n%s", ir)
	}
	if !strings.Contains(fn, "heapdup.copy") || !strings.Contains(fn, "@pal_alloc") {
		t.Errorf("T1491: `=> xs[0]` on a droppable heap-user (inner string) vector "+
			"element did not dup → the inner string is freed under the returned alias "+
			"(UAF/SIGSEGV):\n%s", fn)
	}
}

// TestT1491_ReturnMapVectorElementDups exercises the isMapOrSetType predicate of
// the Vector arm: a Map element returned out of a Vector must dup, else the
// vector's drop frees the map buffer the returned alias still references →
// double free at scope exit. Unlike the bare/droppable heap-user cases (which
// dup via dupHeapValue → heapdup.copy), a Map/Set element routes through
// cloneHeapElement → the element's synthesized `.clone` method (genVectorIndex
// T1130). setDupFlagsForFieldAccess does NOT arm dupHeapUserFieldAccess for a
// bare Map (Map/Set are excluded from every dup predicate there), so this clone
// is driven solely by the T1491 isMapOrSetType arm — assert it fires.
func TestT1491_ReturnMapVectorElementDups(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		firstmap(map[string, int][] xs) map[string, int] => xs[0];
		main() { }
	`)

	fn := codegentest.ExtractDefine(ir, "__user.firstmap")
	if fn == "" {
		t.Fatalf("T1491: could not find @__user.firstmap in IR:\n%s", ir)
	}
	if !strings.Contains(fn, ".clone\"(") && !strings.Contains(fn, ".clone(") {
		t.Errorf("T1491: `=> xs[0]` on a map[K,V] vector element did not clone "+
			"(isMapOrSetType predicate) → returned map aliases the vector slot, "+
			"double free at scope exit:\n%s", fn)
	}
}

// TestT1491_ReturnStringVectorElementNoHeapDup: string is none of
// isDroppableHeapUserType / isHeapUserNoDropPalFree / isMapOrSetType, so the
// T1491 arm must NOT fire. Strings dup via the independent B0189
// dupStringFieldAccess path (a string clone, not a heapdup.copy). Assert the
// field-access arm did not spuriously emit a heap dup for a string element
// (which would double-dup alongside the B0189 path).
func TestT1491_ReturnStringVectorElementNoHeapDup(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		firststr(string[] xs) string => xs[0];
		main() { }
	`)

	fn := codegentest.ExtractDefine(ir, "__user.firststr")
	if fn == "" {
		t.Fatalf("T1491: could not find @__user.firststr in IR:\n%s", ir)
	}
	if strings.Contains(fn, "heapdup.copy") {
		t.Errorf("T1491: `=> xs[0]` on a string[] vector element emitted a heapdup.copy — "+
			"the T1491 arm wrongly fired on a string element (strings dup via the "+
			"separate B0189 path):\n%s", fn)
	}
}
