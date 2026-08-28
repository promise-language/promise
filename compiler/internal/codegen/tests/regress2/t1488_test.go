package regress2

import (
	"strings"
	"testing"

	"github.com/promise-language/promise/compiler/internal/codegen/codegentest"
)

// TestT1488_ReturnHeapUserArrayElementDups pins the fix: a direct
// `return xs[i]` / `=> xs[i]` of a heap-user element out of a FIXED-SIZE array
// param must dup-on-read the array slot's owned instance. T0590 armed
// genArrayIndex's dup only at var-decl/assignment RHS sites (via
// dupHeapUserFieldAccess); the direct return/arrow escape never armed it, so the
// returned value aliased the array's owned slot → the array's element-walk drop
// and the caller's drop of the returned value double-freed at scope exit.
// genArrayIndex's dup path emits a `heapdup.copy` block that pal_allocs + memcpys
// the instance; assert it fires inside the returning function.
func TestT1488_ReturnHeapUserArrayElementDups(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Boxed { int v; }
		firstb(Boxed[2] xs) Boxed => xs[0];
		main() { }
	`)

	fn := codegentest.ExtractDefine(ir, "__user.firstb")
	if fn == "" {
		t.Fatalf("T1488: could not find @__user.firstb in IR:\n%s", ir)
	}
	if !strings.Contains(fn, "heapdup.copy") || !strings.Contains(fn, "@pal_alloc") {
		t.Errorf("T1488: `=> xs[0]` did not dup the heap-user array element "+
			"(no heapdup.copy/pal_alloc) — returned value aliases the array slot "+
			"→ double free at scope exit:\n%s", fn)
	}
}

// TestT1488_GenericReturnHeapUserArrayElementDups: the same escape through a
// GENERIC `first[T](T[2] xs) T => xs[0]` instantiated at T=Boxed must dup — the
// return-site arming applies typeSubst to the index target type so the generic
// code path is covered.
func TestT1488_GenericReturnHeapUserArrayElementDups(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Boxed { int v; }
		first[T](T[2] xs) T => xs[0];
		main() {
			Boxed[2] v = [Boxed(v: 5), Boxed(v: 6)];
			b := first[Boxed](v);
		}
	`)

	fn := codegentest.ExtractDefine(ir, "first[Boxed]")
	if fn == "" {
		t.Fatalf("T1488: could not find generic instance @\"first[Boxed]\" in IR:\n%s", ir)
	}
	if !strings.Contains(fn, "heapdup.copy") || !strings.Contains(fn, "@pal_alloc") {
		t.Errorf("T1488: generic `=> xs[0]` at T=Boxed did not dup the heap-user "+
			"array element → double free:\n%s", fn)
	}
}

// TestT1488_ReturnIntArrayElementNoDup: the arming is a permission consumed by
// genArrayIndex only when the element shape is a heap-user/Map-Set/droppable-enum;
// an `int[2]` element return is a plain scalar copy and must NOT emit a heap dup.
func TestT1488_ReturnIntArrayElementNoDup(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		firsti(int[2] xs) int => xs[0];
		main() { }
	`)

	fn := codegentest.ExtractDefine(ir, "__user.firsti")
	if fn == "" {
		t.Fatalf("T1488: could not find @__user.firsti in IR:\n%s", ir)
	}
	if strings.Contains(fn, "heapdup.copy") {
		t.Errorf("T1488: `=> xs[0]` on an int[2] array spuriously emitted a heap dup "+
			"(should be a plain scalar copy):\n%s", fn)
	}
}
