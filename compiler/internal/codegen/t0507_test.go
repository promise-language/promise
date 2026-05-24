package codegen

import (
	"strings"
	"testing"
)

// T0507: When a non-generic type inherits its drop() from a parent (rather than
// defining its own) and adds its own droppable fields, those fields were never
// dropped — call sites resolved to the parent's drop name via resolveMethodOwner,
// silently skipping the child's own field cleanup. Non-generic complement of T0468.
// The fix synthesizes a per-type drop that drops the child's own fields and
// tail-calls the immediate drop parent's drop.

// extractPlainDefinition returns the IR text from `define ... @<name>(` (no
// surrounding quotes) to the next `\n}\n`. Used for non-generic type names which
// LLVM emits without quotes (e.g. `@_NgBox.drop` vs the generic `@"Box[int].drop"`).
func extractPlainDefinition(ir, name string) string {
	marker := "define void @" + name + "("
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

// TestT0507_InheritedDropSynthesized — _T0507Box inherits drop from _T0507Logger
// and adds a string field. The synthesized _T0507Box.drop must:
//  1. Be defined (so emitFieldDrops can call it from _T0507Holder.drop).
//  2. Drop the own field (label) via promise_string_drop.
//  3. Tail-call _T0507Logger.drop so the parent's user body and parent fields run.
func TestT0507_InheritedDropSynthesized(t *testing.T) {
	ir := generateIR(t, `
		type _T0507Logger {
		  string s;
		  drop(~this) {}
		}
		type _T0507Box is _T0507Logger {
		  string label;
		}
		type _T0507Holder {
		  _T0507Box data;
		}
		main() {
		  s := "x";
		  h := _T0507Holder(data: _T0507Box(s: "y", label: s));
		}
	`)

	body := extractPlainDefinition(ir, "_T0507Box.drop")
	if body == "" {
		t.Fatalf("expected synthesized _T0507Box.drop in IR")
	}
	// Must drop the own string field.
	if !strings.Contains(body, "@promise_string_drop") {
		t.Errorf("expected _T0507Box.drop to call @promise_string_drop for own label field\nbody:\n%s", body)
	}
	// Must tail-call the parent's drop so parent fields are dropped too.
	if !strings.Contains(body, "@_T0507Logger.drop(") {
		t.Errorf("expected _T0507Box.drop to call @_T0507Logger.drop\nbody:\n%s", body)
	}

	// _T0507Holder.drop must now route through _T0507Box.drop, not call
	// _T0507Logger.drop directly (which would skip the label field).
	holderBody := extractPlainDefinition(ir, "_T0507Holder.drop")
	if holderBody == "" {
		t.Fatalf("expected _T0507Holder.drop in IR")
	}
	if !strings.Contains(holderBody, "@_T0507Box.drop(") {
		t.Errorf("expected _T0507Holder.drop to call @_T0507Box.drop\nbody:\n%s", holderBody)
	}
}

// TestT0507_InheritedDropVtableSlot — the vtable for a non-generic inherited-drop
// instance must point to the synthesized child drop (so virtual dispatch reaches
// the child's own field cleanup). Pre-fix, the vtable preferred the parent's
// drop, leaking the child's fields when drop was dispatched virtually.
func TestT0507_InheritedDropVtableSlot(t *testing.T) {
	ir := generateIR(t, `
		type _T0507VLogger {
		  string s;
		  drop(~this) {}
		}
		type _T0507VBox is _T0507VLogger {
		  string label;
		}
		main() {
		  s := "x";
		  b := _T0507VBox(s: "y", label: s);
		}
	`)

	// The vtable for _T0507VBox must reference _T0507VBox.drop (not _T0507VLogger.drop).
	const vtableMarker = "@promise_vtable__T0507VBox = constant"
	idx := strings.Index(ir, vtableMarker)
	if idx < 0 {
		t.Fatalf("expected vtable for _T0507VBox")
	}
	vtableLine := ir[idx:]
	if eol := strings.Index(vtableLine, "\n"); eol >= 0 {
		vtableLine = vtableLine[:eol]
	}
	if !strings.Contains(vtableLine, "@_T0507VBox.drop") {
		t.Errorf("expected vtable to reference @_T0507VBox.drop (child synth):\n%s", vtableLine)
	}
}

// TestT0507_InheritedDropTypeInfo — the typeinfo's drop_fn_ptr (B0226) for a
// non-generic inherited-drop type must point to the child's drop wrapper, not
// the parent's. Runtime drop dispatch through typeinfo (e.g. for untyped error
// catches) would otherwise skip the child's field cleanup.
func TestT0507_InheritedDropTypeInfo(t *testing.T) {
	ir := generateIR(t, `
		type _T0507TLogger {
		  string s;
		  drop(~this) {}
		}
		type _T0507TBox is _T0507TLogger {
		  string label;
		}
		main() {
		  s := "x";
		  b := _T0507TBox(s: "y", label: s);
		}
	`)

	// The typeinfo for _T0507TBox must reference _T0507TBox.drop$wrap.
	const tiMarker = "@promise_typeinfo__T0507TBox = constant"
	idx := strings.Index(ir, tiMarker)
	if idx < 0 {
		t.Fatalf("expected typeinfo for _T0507TBox")
	}
	tiLine := ir[idx:]
	if eol := strings.Index(tiLine, "\n"); eol >= 0 {
		tiLine = tiLine[:eol]
	}
	if !strings.Contains(tiLine, "@_T0507TBox.drop$wrap") {
		t.Errorf("expected typeinfo drop_fn_ptr to be @_T0507TBox.drop$wrap (child wrap):\n%s", tiLine)
	}
}

// TestT0507_InheritedDropGenericParent — non-generic child inheriting from a
// generic parent instance. The synth must tail-call the parent's mono name
// (e.g. _T0507GenLogger[int].drop) — not the origin name (_T0507GenLogger.drop)
// — otherwise the parent's drop is silently skipped and parent's fields leak.
func TestT0507_InheritedDropGenericParent(t *testing.T) {
	ir := generateIR(t, `
		type _T0507GenLogger[T] {
		  T val;
		  string note;
		  drop(~this) {}
		}
		type _T0507NonGenChild is _T0507GenLogger[int] {
		  string label;
		}
		main() {
		  n := "n";
		  l := "l";
		  c := _T0507NonGenChild(val: 7, note: n, label: l);
		}
	`)

	body := extractPlainDefinition(ir, "_T0507NonGenChild.drop")
	if body == "" {
		t.Fatalf("expected synthesized _T0507NonGenChild.drop in IR")
	}
	// Must tail-call the parent's mono name (_T0507GenLogger[int].drop), not
	// the origin name (_T0507GenLogger.drop).
	if !strings.Contains(body, `@"_T0507GenLogger[int].drop"`) {
		t.Errorf("expected child drop to call @\"_T0507GenLogger[int].drop\" (mono parent):\n%s", body)
	}
	if strings.Contains(body, "@_T0507GenLogger.drop(") {
		t.Errorf("child drop must NOT call @_T0507GenLogger.drop (origin name, not mono):\n%s", body)
	}
}

// TestT0507_InheritedDropChain — three-level non-generic inheritance (C is B is A)
// with the drop on A and own fields on B and C. Each level must synthesize a drop
// that drops its own fields and forwards to the immediate parent — not directly to A.
// Without this, B's field would leak when freeing through C.
func TestT0507_InheritedDropChain(t *testing.T) {
	ir := generateIR(t, `
		type _T0507A { string anchor; drop(~this) {} }
		type _T0507B is _T0507A { string mid; }
		type _T0507C is _T0507B { string outer; }
		main() {
		  a := "a";
		  m := "m";
		  o := "o";
		  c := _T0507C(anchor: a, mid: m, outer: o);
		}
	`)

	cBody := extractPlainDefinition(ir, "_T0507C.drop")
	if cBody == "" {
		t.Fatalf("expected _T0507C.drop in IR")
	}
	// C must call B (immediate parent), not A directly.
	if !strings.Contains(cBody, "@_T0507B.drop(") {
		t.Errorf("expected _T0507C.drop to call @_T0507B.drop (immediate parent), got:\n%s", cBody)
	}
	if strings.Contains(cBody, "@_T0507A.drop(") {
		t.Errorf("_T0507C.drop must NOT call _T0507A.drop directly (would skip B's fields):\n%s", cBody)
	}

	bBody := extractPlainDefinition(ir, "_T0507B.drop")
	if bBody == "" {
		t.Fatalf("expected _T0507B.drop in IR")
	}
	// B must call A.
	if !strings.Contains(bBody, "@_T0507A.drop(") {
		t.Errorf("expected _T0507B.drop to call @_T0507A.drop:\n%s", bBody)
	}
}

// TestT0507_InheritedDropModule — non-generic inherited-drop type defined in a
// catalog module. Exercises declareInheritedModuleDrops / defineInheritedModuleDrops
// (the module-compile variant of the T0507 synth), which run with
// c.compilingModule != "" and register the synthesized drop under BOTH the
// module-prefixed mangled name and the plain alias so resolveDropOwner finds it.
// Parent has a heap field (string marker) — T0553 ensures the parent's own
// drop auto-appends the field drop for that.
func TestT0507_InheritedDropModule(t *testing.T) {
	ir := generateIRWithModule(t, "modlib",
		`
			type _T0507ModLogger `+"`"+`public {
			  string marker;
			  drop(~this) {}
			}
			type _T0507ModBox is _T0507ModLogger `+"`"+`public {
			  string label;
			}
		`,
		`
			use modlib "./modlib";
			main() {
			  m := "m";
			  s := "x";
			  b := modlib._T0507ModBox(marker: m, label: s);
			}
		`,
	)

	// Module-prefixed synth must exist with body that drops own field and calls
	// the module-prefixed parent drop.
	body := extractPlainDefinition(ir, "__mod_modlib__T0507ModBox.drop")
	if body == "" {
		t.Fatalf("expected synthesized __mod_modlib__T0507ModBox.drop in IR")
	}
	if !strings.Contains(body, "@promise_string_drop") {
		t.Errorf("expected module-defined child drop to call @promise_string_drop for own label:\n%s", body)
	}
	if !strings.Contains(body, "@__mod_modlib__T0507ModLogger.drop(") {
		t.Errorf("expected module-defined child drop to call module-prefixed parent drop:\n%s", body)
	}

	// Vtable for the module child must reference the synth (child's mangled name).
	const vtableMarker = "@promise_vtable___mod_modlib__T0507ModBox = constant"
	idx := strings.Index(ir, vtableMarker)
	if idx < 0 {
		t.Fatalf("expected vtable for module child")
	}
	vtableLine := ir[idx:]
	if eol := strings.Index(vtableLine, "\n"); eol >= 0 {
		vtableLine = vtableLine[:eol]
	}
	if !strings.Contains(vtableLine, "@__mod_modlib__T0507ModBox.drop") {
		t.Errorf("expected module-child vtable to reference its own synth drop:\n%s", vtableLine)
	}
}

// TestT0507_NoSynthForOwnDrop — sanity check: a type that defines its OWN drop
// must not get a synth (the user body already handles field cleanup via
// emitFieldDrops). needsInheritedDropSynth must return false for this case.
// Without this guard, the synth would duplicate field drops + create infinite
// recursion (call the user's drop which calls the synth's parent...).
func TestT0507_NoSynthForOwnDrop(t *testing.T) {
	ir := generateIR(t, `
		type _T0507OwnLogger {
		  string s;
		  drop(~this) {}
		}
		type _T0507OwnChild is _T0507OwnLogger {
		  string label;
		  drop(~this) {}
		}
		main() {
		  s := "x";
		  c := _T0507OwnChild(s: "y", label: s);
		}
	`)

	// _T0507OwnChild has its own drop, so there must be exactly one definition,
	// which is the user-written body (which auto-appends field drops). It must
	// not be a synth that tail-calls the parent (that would be the T0507 path).
	body := extractPlainDefinition(ir, "_T0507OwnChild.drop")
	if body == "" {
		t.Fatalf("expected _T0507OwnChild.drop in IR")
	}
	// Own drop must drop own field (label) via emitFieldDrops.
	if !strings.Contains(body, "@promise_string_drop") {
		t.Errorf("expected user-written drop to auto-append @promise_string_drop:\n%s", body)
	}
	// Own drop must NOT contain a tail-call to the parent (that's the synth path).
	if strings.Contains(body, "@_T0507OwnLogger.drop(") {
		t.Errorf("user-written drop must NOT tail-call parent's drop (T0507 synth path):\n%s", body)
	}
}

// TestT0507_NoSynthWhenNoInheritedDrop — when a child inherits from a parent that
// has NO drop, the child must not get a T0507 synth (there's no drop to inherit
// from). needsInheritedDropSynth's HasDrop check ensures this.
func TestT0507_NoSynthWhenNoInheritedDrop(t *testing.T) {
	ir := generateIR(t, `
		type _T0507NoDrop {
		  int x;
		}
		type _T0507Inheriting is _T0507NoDrop {
		  string label;
		}
		main() {
		  s := "x";
		  c := _T0507Inheriting(x: 1, label: s);
		}
	`)

	// _T0507Inheriting must not have a T0507 synth drop. The B0158 path
	// (NeedsSynthDrop) handles types like this — they get a synth that drops
	// own fields + pal_free without a parent tail-call. Confirm there's no
	// orphan call to a non-existent parent drop.
	body := extractPlainDefinition(ir, "_T0507Inheriting.drop")
	if body == "" {
		// Either B0158 synth or no synth — either is acceptable for this case.
		return
	}
	// If synth exists, it MUST NOT call _T0507NoDrop.drop (which doesn't exist).
	if strings.Contains(body, "@_T0507NoDrop.drop(") {
		t.Errorf("_T0507Inheriting.drop must not call non-existent parent drop:\n%s", body)
	}
}
