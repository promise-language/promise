package codegen

import (
	"strings"
	"testing"
)

// T1113: the codegen backstop. A single-owner native handle (Task/Mutex/
// MutexGuard) is a bare i8* owning one allocation with NO dup semantics. The
// original T1109 fix left these as silent no-op shallow copies in the structural
// dup paths (emitVariantFieldDup for enum variant fields, dupHeapValueFields for
// user-type fields) — a shallow copy aliases the source handle and double-frees /
// UAFs at drop. The fix replaces the no-op with emitSingleOwnerHandleDupPanic,
// converting silent corruption into a clear panic if any path ever reaches it.
//
// The reachable user paths are now rejected at the ownership pass
// (rejectIndexExprSingleOwnerMove), so these would not compile end-to-end. The
// codegen test helper (generateIR) runs parse → sema → codegen but NOT the
// ownership pass, so it can drive the dup path directly and prove the backstop
// emits the panic (not a silent shallow copy) for every handle type and at both
// structural-dup call sites. The compile-failure regressions live in the Go
// ownership tests (TestT1113_* in ownership_test.go); the positive refcounted-
// nesting runtime regression lives in tests/e2e/t1113_nested_handle_read_test.pr.

// Enum variant field path (emitVariantFieldDup): a Map element that is an enum
// whose variant carries a handle, read by value and match-destructured, routes
// the variant-field dup through emitVariantFieldDup. Each handle must hit the
// panic backstop, never a silent shallow copy.
func TestT1113_EnumVariantHandleDupPanics(t *testing.T) {
	cases := []struct {
		name    string
		decl    string // enum decl
		element string // map value type
		handle  string // expected handle name in the panic message
	}{
		{"Mutex", `enum H { M(Mutex[int] m, int n) }`, "H", "Mutex"},
		{"Task", `enum H { T(Task[int] t, int n) }`, "H", "Task"},
		{"MutexGuard", `enum H { G(MutexGuard[int] g, int n) }`, "H", "MutexGuard"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ir := generateIR(t, tc.decl+`
				caller() {
					m := Map[int, `+tc.element+`]();
					x := m[1]!;
				}
				main() { caller(); }
			`)
			want := "internal: cannot duplicate single-owner handle " + tc.handle
			if !strings.Contains(ir, want) {
				t.Errorf("T1113: expected backstop panic %q for a %s enum-variant field dup; "+
					"none found — emitVariantFieldDup silently shallow-copied the handle "+
					"(double-free/UAF at drop). IR:\n%s", want, tc.handle, ir)
			}
		})
	}
}

// User-type field path (dupHeapValueFields): a Map element that is a struct with
// a handle field, read by value, routes the field dup through dupHeapValueFields.
// Each handle must hit the panic backstop.
func TestT1113_StructFieldHandleDupPanics(t *testing.T) {
	cases := []struct {
		name   string
		decl   string
		handle string
	}{
		{"Mutex", `type S { Mutex[int] m; int n; }`, "Mutex"},
		{"Task", `type S { Task[int] t; int n; }`, "Task"},
		{"MutexGuard", `type S { MutexGuard[int] g; int n; }`, "MutexGuard"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ir := generateIR(t, tc.decl+`
				caller() {
					m := Map[int, S]();
					x := m[1]!;
				}
				main() { caller(); }
			`)
			want := "internal: cannot duplicate single-owner handle " + tc.handle
			if !strings.Contains(ir, want) {
				t.Errorf("T1113: expected backstop panic %q for a %s struct-field dup; "+
					"none found — dupHeapValueFields silently shallow-copied the handle "+
					"(double-free/UAF at drop). IR:\n%s", want, tc.handle, ir)
			}
		})
	}
}
