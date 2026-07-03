package codegen

import (
	"strings"
	"testing"
)

// T0987: `m[k] += x` where V is a heap user type with a non-native operator
// SEGV'd (double free) on overwrite. Map.[] returns the stored heap value
// ALIASED (not dup'd), unlike strings/enum-payloads which it dups. Map.[]=
// already drops the overwritten value, so genMethodCompoundAssign dropping
// `current` directly for that case freed the same instance twice. The fix
// (aliasedMapHeapValue guard) suppresses that direct drop for the Map-aliased
// heap-user-type case only.

// Heap user-type map value: the compound body must NOT emit a `compound.userdrop`
// block on `current` — Map.[]= owns the drop of the overwritten value.
func TestT0987_HeapUserMapCompoundNoUserDrop(t *testing.T) {
	ir := generateIR(t, `
		type Bag {
			int[] items;
			+(Bag o) Bag { return Bag(items: []); }
			drop(~this) {}
		}
		caller() {
			m := map[string, Bag]();
			m["a"] = Bag(items: [1]);
			m["a"] += Bag(items: [2]);
		}
		main() { caller(); }
	`)
	body := extractFunction(ir, "__user.caller")
	if body == "" {
		t.Fatalf("expected @__user.caller in IR:\n%s", ir)
	}
	if !strings.Contains(body, "mapcomp.ok") {
		t.Fatalf("expected the map compound read (`mapcomp.ok`) in caller:\n%s", body)
	}
	if strings.Contains(body, "compound.userdrop") {
		t.Fatalf("did not expect a `compound.userdrop` block for a Map-aliased heap-user-type compound (would double-free with Map.[]=):\n%s", body)
	}
}

// Mono-body coverage: `slots[k] += delta` inside a generic method compiled for a
// concrete droppable V drives aliasedMapHeapValue through the active typeSubst
// (V -> Bag). The mono method body must still suppress the direct
// `compound.userdrop` (Map.[]= owns the drop) — proving the substitution branch
// resolves the value type correctly inside monomorphized bodies.
func TestT0987_MonoBodyMapCompoundNoUserDrop(t *testing.T) {
	ir := generateIR(t, `
		type Addable { +(Self other) Self `+"`"+`abstract; }
		type Bag {
			int[] items;
			+(Bag o) Bag { return Bag(items: []); }
			drop(~this) {}
		}
		type Registry[V: Addable] {
			map[string, V] slots;
			bump(~this, string k, V move delta) Registry[V] {
				this.slots[k] += delta;
				return this;
			}
		}
		main() {
			r := Registry[Bag](slots: map[string, Bag]());
			r.slots["a"] = Bag(items: [1]);
			r2 := r.bump("a", Bag(items: [2]));
		}
	`)
	body := extractFunction(ir, `"Registry[Bag].bump"`)
	if body == "" {
		t.Fatalf("expected @\"Registry[Bag].bump\" (monomorphized method) in IR:\n%s", ir)
	}
	if strings.Contains(body, "compound.userdrop") {
		t.Fatalf("did not expect a `compound.userdrop` block in the mono method body for a Map-aliased droppable heap value (would double-free with Map.[]=):\n%s", body)
	}
}

// Regression guard: a pal_free-only heap user type (non-native operator, but no
// explicit/synth drop — all-scalar fields) as a map value MUST still get the
// direct `compound.userdrop` on `current`. Unlike the droppable-heap case,
// Map.[]='s overwrite-drop does not free this aliased old instance, so
// suppressing the direct drop leaks it. The guard is restricted to
// isDroppableHeapUserType precisely to keep this path emitting the drop.
func TestT0987_PalFreeHeapMapCompoundStillDrops(t *testing.T) {
	ir := generateIR(t, `
		type Counter {
			int n;
			+(Counter o) Counter { return Counter(n: this.n + o.n); }
		}
		caller() {
			m := map[string, Counter]();
			m["a"] = Counter(n: 1);
			m["a"] += Counter(n: 4);
		}
		main() { caller(); }
	`)
	body := extractFunction(ir, "__user.caller")
	if body == "" {
		t.Fatalf("expected @__user.caller in IR:\n%s", ir)
	}
	if !strings.Contains(body, "compound.userdrop") {
		t.Fatalf("expected a `compound.userdrop` block for a pal_free-only heap-user-type map compound (the direct drop is the only path that frees the aliased old value):\n%s", body)
	}
}

// Regression guard: the string map value still drops the old getter-returned
// value (Map.[] dups the string, so `current` is an owned temp that must be
// freed). The fix must not suppress this.
func TestT0987_StringMapCompoundStillDropsOld(t *testing.T) {
	ir := generateIR(t, `
		caller() {
			m := map[string, string]();
			m["a"] = "x";
			m["a"] += "y";
		}
		main() { caller(); }
	`)
	body := extractFunction(ir, "__user.caller")
	if body == "" {
		t.Fatalf("expected @__user.caller in IR:\n%s", ir)
	}
	if !strings.Contains(body, "compound.strdrop") {
		t.Fatalf("expected a `compound.strdrop` block for a string map compound (dup'd `current` must be freed):\n%s", body)
	}
}
