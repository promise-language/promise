package codegen

import "testing"

// T1139: The move-consuming "claim (untrack) enum-ctor temps" pattern shared by
// every codegen slot site must be gated on the moved slot's static type being an
// enum. When the slot value is a NON-enum expression that internally passes an
// inline `Enum.Variant(...)` temp BY BORROW to a sub-call, that inner enum temp
// is an intermediate the slot never receives — it must stay tracked so the caller
// drops it at statement end. Untracking it (the pre-gate behavior) removed its
// statement-end drop and leaked the payload.

// Non-enum constructor field whose value is a borrow-call over an inline
// enum-ctor temp: the inner Holder.Has temp must remain tracked → the caller
// emits a statement-end enum.ctor.drop. (Implicit-ctor field-loop site.)
func TestEnumCtorTempNonEnumCtorFieldKeepsDrop(t *testing.T) {
	ir := generateIR(t, `
		type Resource { string name; drop(~this) {} }
		enum Holder { Has(Resource r), Empty, }
		make_resource() Resource { return Resource(name: "x"); }
		inspect(Holder h) Resource {
			return match h {
				Holder.Has(r) => Resource(name: r.name),
				Holder.Empty => Resource(name: "none"),
			};
		}
		type Box { Resource r; }
		test() {
			b := Box(r: inspect(Holder.Has(r: make_resource())));
		}
	`)
	// The borrow intermediate enum temp must still be dropped at statement end.
	assertContains(t, ir, "enum.ctor.drop")
	assertContains(t, ir, "call void @Holder.drop")
}

// Enum-typed constructor field built directly from an inline enum-ctor temp: the
// field owns the enum (its synth drop frees it), so the caller must NOT emit a
// synchronous enum.ctor.drop (that would double-free). The gate's true branch
// claims/truncates the temp — confirming the existing no-double-free behavior is
// preserved.
func TestEnumCtorTempEnumTypedCtorFieldNoCallerDrop(t *testing.T) {
	ir := generateIR(t, `
		type Resource { string name; drop(~this) {} }
		enum Holder { Has(Resource r), Empty, }
		make_resource() Resource { return Resource(name: "x"); }
		type EnumBox { Holder h; }
		test() {
			b := EnumBox(h: Holder.Has(r: make_resource()));
		}
	`)
	assertNotContains(t, ir, "enum.ctor.drop")
}
