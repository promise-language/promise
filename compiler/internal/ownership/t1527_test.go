package ownership

import "testing"

// T1527: a value newtype — a fieldless child of a pure `value type — is itself
// a value type and therefore Copy. Ownership has its own value-type/copy
// predicates (isCopyType, and the IsValueType guards in expr.go), so the late
// classification has to be visible here too: a newtype must never be tracked as
// moved or droppable.

const t1527ValueTypes = `
	type Hash128 { u128 value ` + "`value" + `; }
	type EntityId is Hash128 {}
`

func TestT1527ValueNewtypeUsableAfterCopy(t *testing.T) {
	ownerOK(t, t1527ValueTypes+`
		take(Hash128 h) u128 => h.value;
		main() {
			e := EntityId(value: 1u128);
			Hash128 h = e;
			take(e);
			take(e);
			print_line(e.value.to_string());
			print_line(h.value.to_string());
		}
	`)
}

// Reading a newtype out of a container is a copy, not a move — the element must
// not need the dup-on-read treatment a heap element gets (T0590).
func TestT1527ValueNewtypeContainerElementIsCopy(t *testing.T) {
	ownerOK(t, t1527ValueTypes+`
		main() {
			EntityId[] ids = [EntityId(value: 1u128), EntityId(value: 2u128)];
			EntityId first = ids[0];
			print_line(first.value.to_string());
			print_line(ids[0].value.to_string());
			print_line(ids.len.to_string());
		}
	`)
}

// A newtype held in an ordinary heap type's field is copied out, so the field
// stays readable afterwards and the owner is not partially moved.
func TestT1527ValueNewtypeHeapFieldIsCopy(t *testing.T) {
	ownerOK(t, t1527ValueTypes+`
		type Booking { EntityId id; int count; }
		main() {
			b := Booking(id: EntityId(value: 3u128), count: 1);
			EntityId copy = b.id;
			print_line(copy.value.to_string());
			print_line(b.id.value.to_string());
		}
	`)
}

// A newtype sitting beside genuinely droppable fields must not disturb the
// owner's drop path: reading it out is a copy, while the droppable siblings
// stay owned by the record and are still usable afterwards.
func TestT1527ValueNewtypeBesideDroppableFields(t *testing.T) {
	ownerOK(t, t1527ValueTypes+`
		type Record { EntityId tag; string name; int[] nums; }
		main() {
			r := Record(tag: EntityId(value: 3u128), name: "hi", nums: [1, 2, 3]);
			EntityId copy = r.tag;
			print_line(copy.value.to_string());
			print_line(r.tag.value.to_string());
			print_line(r.name);
			print_line(r.nums.len.to_string());
		}
	`)
}

// Returning a newtype out of a function is not an escape of borrowed data — it
// is a value copy, the same as returning the parent.
func TestT1527ValueNewtypeReturnedByValue(t *testing.T) {
	ownerOK(t, t1527ValueTypes+`
		widen(EntityId e) Hash128 => e;
		make() EntityId => EntityId(value: 7u128);
		main() {
			e := make();
			Hash128 h = widen(e);
			print_line(h.value.to_string());
			print_line(e.value.to_string());
		}
	`)
}
