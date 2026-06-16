package codegen

import "testing"

// T0917: clone-on-`return this` whose static return type is an abstract base
// (`needsVtable` true). Fix B builds the returned value struct's vtable by loading
// the concrete subtype's vtable from the receiver instance's RTTI
// (variant→typeinfo→vtable_ptr) instead of `lookupVtableGlobal(<abstract base>)`,
// whose abstract method slots are null. The buggy path embedded a bitcast of the
// abstract base's vtable global into the body; the fixed path must not reference
// it (it loads from the instance instead).
func TestAbstractReturnThisLoadsConcreteVtableFromInstance(t *testing.T) {
	ir := generateIR(t, `
		type SAdd { get tag string `+"`"+`abstract; }
		type SAddItem is SAdd {
			string name;
			get tag string { return this.name; }
			dup() SAdd { return this; }
		}
		main() { x := SAddItem(name: "hi"); m := x.dup(); }
	`)
	body := extractDefine(ir, "SAddItem.dup")
	// Clone still fires (borrowed `return this`).
	assertContains(t, body, "heapdup")
	// Fix B: the abstract base's vtable global is NOT baked into the value struct —
	// the vtable is loaded from the instance's RTTI. (The global still exists
	// elsewhere in the module; it must just not be referenced here.)
	assertNotContains(t, body, "promise_vtable_SAdd")
	assertContains(t, ir, "@promise_vtable_SAdd")
}

// T0917: the `hasChildren` half of `needsVtable` (a non-abstract base with a child
// override). `lookupVtableGlobal(Base)` is non-null here, so the pre-fix path did
// not segfault — it embedded the base vtable and silently mis-dispatched to the
// base method. Fix B loads the instance vtable so the override resolves; the base
// vtable global must not be baked into the `return this` body.
func TestNonAbstractBaseReturnThisLoadsInstanceVtable(t *testing.T) {
	ir := generateIR(t, `
		type Base {
			string id;
			get tag string { return this.id; }
			dup() Base { return this; }
		}
		type Child is Base {
			get tag string { return "c"; }
		}
		main() { c := Child(id: "x"); m := c.dup(); }
	`)
	body := extractDefine(ir, "Base.dup")
	assertContains(t, body, "heapdup")
	assertNotContains(t, body, "promise_vtable_Base")
}

// T0917: an abstract operator `return this` (`+(SAdd) SAdd`) flows through the same
// `wrapThisReturnValue` heap-type branch — it must also avoid the abstract base
// vtable global (the buggy path segfaulted on the null abstract slot at the call
// site). Confirms Fix B covers the operator shape, not just method calls.
func TestAbstractOperatorReturnThisLoadsInstanceVtable(t *testing.T) {
	ir := generateIR(t, `
		type SAdd { get tag string `+"`"+`abstract; }
		type SAddItem is SAdd {
			string name;
			get tag string { return this.name; }
			+(SAdd other) SAdd { return this; }
		}
		main() { a := SAddItem(name: "a"); b := SAddItem(name: "b"); m := a + b; }
	`)
	body := extractFunction(ir, `"SAddItem.+"`)
	assertNotContains(t, body, "promise_vtable_SAdd")
}

// T0917: a `~this` (owned) abstract-return `return this` is a genuine ownership
// transfer — Fix B's instance-vtable load still applies (the result must carry the
// concrete vtable), but the T0893 clone must NOT fire (no double-ownership).
func TestOwnedAbstractReturnThisDoesNotClone(t *testing.T) {
	ir := generateIR(t, `
		type SAdd { get tag string `+"`"+`abstract; }
		type SAddItem is SAdd {
			string name;
			get tag string { return this.name; }
			consume(~this) SAdd { return this; }
		}
		main() { x := SAddItem(name: "hi"); m := x.consume(); }
	`)
	body := extractDefine(ir, "SAddItem.consume")
	assertNotContains(t, body, "heapdup")
	assertNotContains(t, body, "promise_vtable_SAdd")
}
