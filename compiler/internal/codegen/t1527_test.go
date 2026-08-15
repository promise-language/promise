package codegen

import (
	"bytes"
	"strings"
	"testing"
)

// T1527: a value newtype (fieldless child of a pure value type) stays a pure
// value type. It shares its parent's value struct verbatim — so an upcast is a
// no-op — while keeping its own RTTI identity, and dispatches statically.

const t1527Src = `
	type Hash128 {
		u128 value ` + "`value" + `;
		get half u128 => this.value / 2u128;
	}
	type EntityId is Hash128 {
		get tag int => 7;
	}
	widen(Hash128 h) u128 => h.half;
	main() {
		e := EntityId(value: 8u128);
		Hash128 h = e;
		widen(e);
		e.half;
		e.tag;
		h.value;
	}
`

func TestT1527ValueNewtypeSharesParentValueStruct(t *testing.T) {
	ir := generateIR(t, t1527Src)
	// The child declares no value struct of its own — it reuses the parent's,
	// which is what makes every upcast a no-op.
	assertNotContains(t, ir, "%promise_EntityId_v = type")
	assertContains(t, ir, "%promise_Hash128_v = type { i8*, i128 }")
	// It still gets its own RTTI identity (own _t/_m/_i + typeinfo + rtti global).
	assertContains(t, ir, "%promise_EntityId_i = type")
	assertContains(t, ir, "@promise_typeinfo_EntityId")
	assertContains(t, ir, "@promise_rtti_EntityId")
}

func TestT1527ValueNewtypeConstructorIsInlineNoAlloc(t *testing.T) {
	ir := generateIR(t, t1527Src)
	// Construction is an insertvalue chain into the shared value struct, held
	// in a stack slot — no instance allocation for the child or its parent.
	assertContains(t, ir, "insertvalue %promise_Hash128_v")
	assertContains(t, ir, "alloca %promise_Hash128_v")
	assertNotContains(t, ir, "%promise_EntityId_i*")
	assertNotContains(t, ir, "%promise_Hash128_i*")
}

func TestT1527ValueNewtypeDispatchesStatically(t *testing.T) {
	ir := generateIR(t, t1527Src)
	// The inherited getter is a direct call to the parent's function, and the
	// child's own getter a direct call to its own — a value parent that gains a
	// child must not flip to virtual dispatch.
	assertContains(t, ir, "call i128 @Hash128.half(")
	assertContains(t, ir, "call i64 @EntityId.tag(")
	assertNotContains(t, ir, "vtable.slot")
}

func TestT1527ValueNewtypeUpcastNeedsNoCoercion(t *testing.T) {
	ir := generateIR(t, t1527Src)
	// Assigning the child to a parent-typed local and passing it to a
	// parent-typed parameter both use the value as-is: the parent's struct type
	// appears at the store and the call, with no per-upcast repack.
	assertContains(t, ir, "store %promise_Hash128_v")
	assertContains(t, ir, "call i128 @__user.widen(%promise_Hash128_v")
}

func TestT1527ValueNewtypeChainSharesRootValueStruct(t *testing.T) {
	ir := generateIR(t, `
		type A { int x `+"`value"+`; }
		type B is A {}
		type C is B {}
		main() { c := C(x: 1); A a = c; a.x; }
	`)
	assertContains(t, ir, "%promise_A_v = type { i8*, i64 }")
	assertNotContains(t, ir, "%promise_B_v = type")
	assertNotContains(t, ir, "%promise_C_v = type")
	assertContains(t, ir, "@promise_rtti_C")
}

// The shared value struct is reachable from both the parent's and the child's
// layout, so the generated C header must still define it exactly once — a
// repeated typedef would not compile.
func TestT1527ValueNewtypeHeaderDefinesSharedStructOnce(t *testing.T) {
	result := compileResult(t, t1527Src)

	var buf bytes.Buffer
	if err := GenerateHeader(&buf, result.Layouts, result.EnumLayouts, result.Externs); err != nil {
		t.Fatalf("GenerateHeader error: %v", err)
	}
	header := buf.String()
	if got := strings.Count(header, "} promise_Hash128_v;"); got != 1 {
		t.Errorf("expected the shared value struct to be defined once, got %d definitions", got)
	}
	if strings.Contains(header, "} promise_EntityId_v;") {
		t.Errorf("child must not define a value struct of its own")
	}
}

// A value newtype used as an enum variant payload is laid out from the enum
// pass, which runs BEFORE the topological type-layout pass — so the parent's
// shared value struct has to be reachable from that path too (T1016's
// ensureValueTypeLayout), not only from the topological one. The child is
// declared before its parent so the recursion, not declaration order, is what
// gets the parent laid out first.
func TestT1527ValueNewtypeAsEnumVariantPayload(t *testing.T) {
	ir := generateIR(t, `
		type Seat is Ticket {
			get tag int => 7;
		}
		type Ticket { int n `+"`value"+`; }
		enum Slot[K] {
			Empty,
			Used(K key),
		}
		main() {
			Slot[Seat] s = Slot[Seat].Used(Seat(n: 3));
			match s {
				Used(k) => print_line(k.tag.to_string()),
				Empty => print_line("empty"),
			}
		}
	`)
	// The parent's struct is the one the variant slot embeds — the child never
	// declares one of its own, so a boxed {i8*,i8*} slot here would mean the
	// newtype lost its value layout.
	assertContains(t, ir, "%promise_Ticket_v = type { i8*, i64 }")
	assertNotContains(t, ir, "%promise_Seat_v = type")
	assertContains(t, ir, "{ %promise_Ticket_v }")
}

// A newtype used as another value type's `value field must be EMBEDDED as the
// parent's shared struct, not boxed behind a pointer — the containing type's
// layout is what proves the sharing survives one level of nesting.
func TestT1527ValueNewtypeAsValueFieldEmbedsParentStruct(t *testing.T) {
	ir := generateIR(t, `
		type V { int n `+"`value"+`; get d int => this.n * 2; }
		type W is V {}
		type Wrap { W w `+"`value"+`; }
		main() { x := Wrap(w: W(n: 4)); x.w.d; }
	`)
	assertContains(t, ir, "%promise_V_v = type { i8*, i64 }")
	assertContains(t, ir, "%promise_Wrap_v = type { i8*, %promise_V_v }")
	assertNotContains(t, ir, "%promise_W_v = type")
}

// An inherited `~this` method has to mutate the CHILD's storage in place (the
// T1358 receiver ABI): the child's alloca IS the parent's struct, so the address
// is passed straight through with no spill to a temp — a copy would silently
// drop the write.
func TestT1527ValueNewtypeInheritedMutatorUsesCallerStorage(t *testing.T) {
	ir := generateIR(t, `
		type V { int n `+"`value"+`; set_n(~this, int v) { this.n = v; } }
		type W is V {}
		main() { w := W(n: 1); w.set_n(5); w.n; }
	`)
	// One stack slot, typed as the shared parent struct, and it is that slot's
	// own address that reaches the inherited mutator.
	assertContains(t, ir, "%w = alloca %promise_V_v")
	assertNotContains(t, ir, "alloca %promise_W_v")
	assertContains(t, ir, "bitcast %promise_V_v* %w to i8*")
	assertContains(t, ir, "call void @V.set_n(")
}

// A value newtype whose value parent lives in another module: the parent's
// layout is imported (no pending entry in the layout pass), so the child must
// still end up sharing the parent's value struct rather than declaring its own.
func TestT1527ValueNewtypeOfModuleValueParent(t *testing.T) {
	ir := generateIRWithModule(t, "mylib",
		`
		type Nanos `+"`public"+` {
			int n `+"`value"+`;
			get doubled int `+"`public"+` => this.n * 2;
		}
		`,
		`
		use mylib "./mylib";
		type Timeout is mylib.Nanos {}
		main() {
			t := Timeout(n: 21);
			t.doubled;
			mylib.Nanos parent = t;
			parent.n;
		}
	`)
	assertContains(t, ir, "%promise_Nanos_v = type { i8*, i64 }")
	assertNotContains(t, ir, "%promise_Timeout_v = type")
	assertContains(t, ir, "@promise_rtti_Timeout")
	assertContains(t, ir, "call i64 @__mod_mylib_Nanos.doubled(")
}
