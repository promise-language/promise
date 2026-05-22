package codegen

import (
	"strings"
	"testing"
)

// T0468: When a generic field type inherits its drop() from a generic parent
// (rather than defining its own), emitFieldDrops could not find the drop
// function under the child's mono name and silently skipped the call, leaking
// the child's own heap fields. The fix synthesizes a per-instance drop on the
// child that drops the child's own fields and tail-calls the parent's mono drop.

// extractDefinition returns the IR text from `define ... @"<qname>"(` to the
// next `\n}\n`. Unlike extractFunction it anchors on the definition keyword,
// so call sites of the same name don't disturb the search.
func extractDefinition(ir, qname string) string {
	marker := `define void @"` + qname + `"(`
	start := strings.Index(ir, marker)
	if start < 0 {
		return ""
	}
	rest := ir[start:]
	end := strings.Index(rest, "\n}\n")
	if end < 0 {
		return rest
	}
	return rest[:end+2]
}

// TestT0468_InheritedDropSynthesized — Box[int] inherits drop from Logger[int]
// and adds a string field. The synthesized Box[int].drop must:
//  1. Be defined (so emitFieldDrops can call it from Holder[int].drop).
//  2. Drop the own field (label) via promise_string_drop.
//  3. Tail-call Logger[int].drop so the parent's user body and parent fields run.
func TestT0468_InheritedDropSynthesized(t *testing.T) {
	ir := generateIR(t, `
		type _T0468Logger[T] {
		  T val;
		  drop(~this) {}
		}
		type _T0468Box[T] is _T0468Logger[T] {
		  string label;
		}
		type _T0468Holder[T] {
		  _T0468Box[T] data;
		}
		main() {
		  s := "x";
		  h := _T0468Holder[int](data: _T0468Box[int](val: 1, label: s));
		}
	`)

	body := extractDefinition(ir, "_T0468Box[int].drop")
	if body == "" {
		t.Fatalf("expected synthesized _T0468Box[int].drop in IR")
	}
	// Must drop the own string field.
	if !strings.Contains(body, "@promise_string_drop") {
		t.Errorf("expected _T0468Box[int].drop to call @promise_string_drop for own label field\nbody:\n%s", body)
	}
	// Must tail-call the parent's mono drop so parent fields are dropped too.
	if !strings.Contains(body, `@"_T0468Logger[int].drop"`) {
		t.Errorf("expected _T0468Box[int].drop to call @\"_T0468Logger[int].drop\"\nbody:\n%s", body)
	}

	// Holder[int].drop must now route through Box[int].drop, not pal_free directly.
	holderBody := extractDefinition(ir, "_T0468Holder[int].drop")
	if holderBody == "" {
		t.Fatalf("expected _T0468Holder[int].drop in IR")
	}
	if !strings.Contains(holderBody, `@"_T0468Box[int].drop"`) {
		t.Errorf("expected _T0468Holder[int].drop to call @\"_T0468Box[int].drop\"\nbody:\n%s", holderBody)
	}
}

// TestT0468_InheritedDropVtableSlot — the vtable for an inherited-drop generic
// instance must point to the synthesized child drop (so virtual dispatch reaches
// the child's own field cleanup). Pre-fix, the vtable preferred the parent's
// mono drop, leaking the child's fields when drop was dispatched virtually.
func TestT0468_InheritedDropVtableSlot(t *testing.T) {
	ir := generateIR(t, `
		type _T0468VLogger[T] {
		  T val;
		  drop(~this) {}
		}
		type _T0468VBox[T] is _T0468VLogger[T] {
		  string label;
		}
		main() {
		  s := "x";
		  b := _T0468VBox[int](val: 1, label: s);
		}
	`)

	// The mono vtable for Box[int] must reference Box[int].drop (not Logger[int].drop).
	const vtableMarker = `@"promise_vtable__T0468VBox[int]" = constant`
	idx := strings.Index(ir, vtableMarker)
	if idx < 0 {
		t.Fatalf("expected mono vtable for _T0468VBox[int]")
	}
	vtableLine := ir[idx:]
	if eol := strings.Index(vtableLine, "\n"); eol >= 0 {
		vtableLine = vtableLine[:eol]
	}
	if !strings.Contains(vtableLine, `@"_T0468VBox[int].drop"`) {
		t.Errorf("expected vtable to reference @\"_T0468VBox[int].drop\" (child synth):\n%s", vtableLine)
	}
}

// TestT0468_InheritedDropChain — three-level inheritance (C is B is A) with the
// drop on A and own fields on B and C. Each level must synthesize a drop that
// drops its own fields and forwards to the immediate parent — not directly to A.
// Without this, B's field would leak when freeing through C.
func TestT0468_InheritedDropChain(t *testing.T) {
	ir := generateIR(t, `
		type _T0468A[T] { T anchor; drop(~this) {} }
		type _T0468B[T] is _T0468A[T] { string mid; }
		type _T0468C[T] is _T0468B[T] { string outer; }
		main() {
		  m := "m";
		  o := "o";
		  c := _T0468C[int](anchor: 1, mid: m, outer: o);
		}
	`)

	cBody := extractDefinition(ir, "_T0468C[int].drop")
	if cBody == "" {
		t.Fatalf("expected _T0468C[int].drop in IR")
	}
	// C must call B (immediate parent), not A directly.
	if !strings.Contains(cBody, `@"_T0468B[int].drop"`) {
		t.Errorf("expected _T0468C[int].drop to call @\"_T0468B[int].drop\" (immediate parent), got:\n%s", cBody)
	}
	if strings.Contains(cBody, `@"_T0468A[int].drop"`) {
		t.Errorf("_T0468C[int].drop must NOT call _T0468A[int].drop directly (would skip B's fields):\n%s", cBody)
	}

	bBody := extractDefinition(ir, "_T0468B[int].drop")
	if bBody == "" {
		t.Fatalf("expected _T0468B[int].drop in IR")
	}
	// B must call A.
	if !strings.Contains(bBody, `@"_T0468A[int].drop"`) {
		t.Errorf("expected _T0468B[int].drop to call @\"_T0468A[int].drop\":\n%s", bBody)
	}
}

// TestT0468_InheritedDropNonGenericParent — generic child whose drop is
// inherited from a *non-generic* parent. The synth must tail-call the parent's
// plain (non-mono) drop name. Covers findMonoParentName's `len(TypeArgs) == 0`
// branch through resolveMonoParentName, complementing the chain test which
// only exercises generic parents.
func TestT0468_InheritedDropNonGenericParent(t *testing.T) {
	ir := generateIR(t, `
		type _T0468NG { string note; drop(~this) {} }
		type _T0468GenChild[T] is _T0468NG { T extra; string label; }
		main() {
		  n := "n";
		  s := "s";
		  c := _T0468GenChild[int](note: n, extra: 1, label: s);
		}
	`)

	body := extractDefinition(ir, "_T0468GenChild[int].drop")
	if body == "" {
		t.Fatalf("expected synthesized _T0468GenChild[int].drop in IR")
	}
	// Must drop the child's own string field (label).
	if !strings.Contains(body, "@promise_string_drop") {
		t.Errorf("expected child drop to call @promise_string_drop for own field:\n%s", body)
	}
	// Must tail-call the parent's plain (non-mono) drop name. LLVM IR omits
	// quotes around simple identifiers (no brackets), so the call appears as
	// `@_T0468NG.drop` not `@"_T0468NG.drop"`.
	if !strings.Contains(body, "@_T0468NG.drop(") {
		t.Errorf("expected child drop to call @_T0468NG.drop (non-generic parent):\n%s", body)
	}
	// Must NOT have a mono'd parent suffix (no `_T0468NG[...]`).
	if strings.Contains(body, `@"_T0468NG[`) {
		t.Errorf("non-generic parent must not be mono-mangled:\n%s", body)
	}
}
