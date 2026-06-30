package codegen

import "testing"

// T1016: A pure `value type used as an enum variant field (e.g. a Set/map key,
// which lowers to a generic Slot enum) must lay out the variant data slot using
// the type's embedded value struct (%promise_T_v), not the boxed {i8*,i8*}
// layout. The mismatch previously panicked codegen in valueTypeReceiverPtr /
// genEnumVariantCallLayout when storing a fat {i8*,i8*} into a %promise_T_v* slot.
func TestT1016ValueTypeAsEnumVariantField(t *testing.T) {
	// Generic enum with a value-type field — the Slot[K]/Slot[K,V] shape that
	// Set[T]/map[T,_] use internally for their keys.
	ir := generateIR(t, `
		type Money {
			int cents `+"`value"+`;
			==(Money other) bool => this.cents == other.cents;
			get hash int => this.cents;
		}
		enum Slot[K] {
			Empty,
			Used(K key),
		}
		main() {
			Slot[Money] s = Slot[Money].Used(Money(cents: 7));
			match s {
				Used(k) => print_line(k.cents.to_string()),
				Empty => print_line("empty"),
			}
		}
	`)

	// Reaching here means generateIR did not panic — the store-type mismatch
	// that previously crashed valueTypeReceiverPtr is gone.
	// The value type's embedded value struct must exist.
	assertContains(t, ir, "%promise_Money_v = type { i8*, i64 }")
	// The enum variant data struct must embed the value struct, not box it as
	// {i8*,i8*}. Construction stores a %promise_Money_v into this slot.
	assertContains(t, ir, "{ %promise_Money_v }")
}

// T1016: A multi-word value type (3 ints → wider than the 16-byte {i8*,i8*})
// used as an enum variant field must size the variant data array to the value
// struct, not the boxed layout — otherwise the data array under-sizes and the
// payload overflows.
func TestT1016MultiFieldValueTypeAsEnumVariantField(t *testing.T) {
	ir := generateIR(t, `
		type Point3 {
			int x `+"`value"+`;
			int y `+"`value"+`;
			int z `+"`value"+`;
			==(Point3 other) bool => this.x == other.x && this.y == other.y && this.z == other.z;
			get hash int => this.x;
		}
		enum Slot[K] {
			Empty,
			Used(K key),
		}
		main() {
			Slot[Point3] s = Slot[Point3].Used(Point3(x: 1, y: 2, z: 3));
			match s {
				Used(k) => print_line(k.x.to_string()),
				Empty => print_line("empty"),
			}
		}
	`)

	// Embedded value struct: vtable ptr + three i64 fields.
	assertContains(t, ir, "%promise_Point3_v = type { i8*, i64, i64, i64 }")
	// Variant data struct embeds the full value struct.
	assertContains(t, ir, "{ %promise_Point3_v }")
}

// T1016: A *generic* value type instance used as a generic enum variant field
// exercises the *types.Instance branch of llvmTypeForEnumFieldFromPromise /
// ensureValueTypeLayout — the embedded value struct must come from the mono
// layout (computed on demand), not the boxed {i8*,i8*} layout.
func TestT1016GenericValueTypeAsEnumVariantField(t *testing.T) {
	ir := generateIR(t, `
		type Pair[T] {
			T a `+"`value"+`;
			T b `+"`value"+`;
		}
		enum Box[V] {
			Empty,
			Full(V val),
		}
		main() {
			Box[Pair[int]] b = Box[Pair[int]].Full(Pair(a: 3, b: 4));
			match b {
				Full(p) => print_line(p.a.to_string()),
				Empty => print_line("empty"),
			}
		}
	`)

	// The monomorphized value struct: vtable ptr + two i64 fields.
	assertContains(t, ir, `%"promise_Pair[int]_v" = type { i8*, i64, i64 }`)
	// The variant data struct embeds the mono value struct, not {i8*,i8*}.
	assertContains(t, ir, `{ %"promise_Pair[int]_v" }`)
}

// T1016: A nested value type (a value type whose field is itself a value type)
// used as an enum variant field forces ensureValueTypeLayout to recurse into
// AllFields so the inner value struct is laid out before the outer one.
func TestT1016NestedValueTypeAsEnumVariantField(t *testing.T) {
	ir := generateIR(t, `
		type Money {
			int cents `+"`value"+`;
			==(Money other) bool => this.cents == other.cents;
			get hash int => this.cents;
		}
		type Wallet {
			Money primary `+"`value"+`;
			int id `+"`value"+`;
		}
		enum Slot[K] {
			Empty,
			Used(K key),
		}
		main() {
			Slot[Wallet] s = Slot[Wallet].Used(Wallet(primary: Money(cents: 9), id: 1));
			match s {
				Used(k) => print_line(k.id.to_string()),
				Empty => print_line("empty"),
			}
		}
	`)

	// Inner value struct laid out first.
	assertContains(t, ir, "%promise_Money_v = type { i8*, i64 }")
	// Outer value struct embeds the inner value struct directly + the i64 id.
	assertContains(t, ir, "%promise_Wallet_v = type { i8*, %promise_Money_v, i64 }")
	// Variant data struct embeds the outer value struct.
	assertContains(t, ir, "{ %promise_Wallet_v }")
}
