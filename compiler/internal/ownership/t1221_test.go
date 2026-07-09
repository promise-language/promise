package ownership

import "testing"

// T1221: Consuming a heap field of a borrowed receiver (`this.field`) into an
// owning sink (Channel.send, a `~`/move param) is INTENDED to be allowed and
// cloned by codegen — matching the general `~T` path where a `this`-field
// consume is dup'd, not rejected. The runtime UAF this item reported was a
// codegen gap (Channel.send memcpy'd the raw value with no clone), fixed in
// genChannelSend. These ownership tests lock in the intended asymmetry: a
// consume of a *parameter*'s field still requires an explicit `move`, while a
// consume of the *receiver*'s field does not.

// Consuming a param's heap field into send is rejected (needs `move`).
func TestT1221_SendParamFieldRequiresMove(t *testing.T) {
	errs := ownerErrs(t, `
		type HB { int n; string label; }
		send_param(HB b, Channel[string] out) {
			out.send(b.label);
		}
	`)
	expectOwnerError(t, errs, "consuming 'b.label' requires `move b.label`")
}

// Consuming the receiver's heap field into send is accepted (codegen clones it).
func TestT1221_SendThisFieldOK(t *testing.T) {
	ownerOK(t, `
		type HB { int n; string label;
			send_this(this, Channel[string] out) {
				out.send(this.label);
			}
		}
	`)
}

// The `~this` (consuming receiver) form is likewise accepted.
func TestT1221_SendMoveThisFieldOK(t *testing.T) {
	ownerOK(t, `
		type HB { int n; string label;
			send_this(~this, Channel[string] out) {
				out.send(this.label);
			}
		}
	`)
}

// Escape hatch: consuming a param's field is accepted once `move` is explicit —
// codegen then dups so the arg and the owner don't double-free (the runtime
// counterpart is send_plain_local_string / the this-field tests).
func TestT1221_SendMoveParamFieldOK(t *testing.T) {
	ownerOK(t, `
		type HB { int n; string label; }
		send_param(HB b, Channel[string] out) {
			out.send(move b.label);
		}
	`)
}

// A container this-field consume (not just string) is also accepted — the same
// asymmetry as the string case, and the reachable Vector/Optional[Vector] dup
// paths are runtime-covered in tests/concurrency/channel_send_this_field_test.pr.
func TestT1221_SendThisContainerFieldOK(t *testing.T) {
	ownerOK(t, `
		type HB { int n; string[] items;
			send_this(this, Channel[string[]] out) {
				out.send(this.items);
			}
		}
	`)
}
