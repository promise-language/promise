package codegen

import (
	"strings"
	"testing"
)

// T1053: mutating a container now goes through a `~` mutable-borrow receiver
// (`out: Set[string]~`), so a droppable field-read argument (`out.add(this.label)`)
// must still be duped — the owner keeps its copy of the field. The bug was that
// buildOwnerTypeArgSubst asserted the receiver was a bare *types.Instance and
// returned an empty subst when the receiver was a MutRef/SharedRef borrow, so the
// method's `T move` param reached the dup logic as the unsubstituted TypeParam `T`
// and the field-read dup (T0366/T1223) was skipped → UAF on owner drop.
func TestT1053_FieldDupThroughMutRefReceiver(t *testing.T) {
	ir := generateIR(t, `
		type Owner {
			string label;
			feed(this, Set[string]~ out) {
				out.add(this.label);
			}
		}
		main() {
			s := Set[string]();
			o := Owner(label: "hi");
			o.feed(s);
		}
	`)
	feed := extractFunction(ir, "Owner.feed")
	if feed == "" {
		t.Fatalf("expected Owner.feed in IR:\n%s", ir)
	}
	// The field-read `this.label` moved into Set.add(T move) must be duped
	// (promise_string_new) so the set owns its own copy — otherwise the stored key
	// aliases the owner's buffer and double-frees.
	if !strings.Contains(feed, "@promise_string_new") {
		t.Errorf("expected field-read dup (@promise_string_new) in Owner.feed — the mut-ref receiver must not skip the T0366 dup:\n%s", feed)
	}
}
