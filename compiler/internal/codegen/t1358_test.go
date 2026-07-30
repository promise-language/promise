package codegen

import (
	"strings"
	"testing"
)

// T1358: a value-type method with a `~this` (mutable-borrow) receiver must
// mutate the CALLER's storage, not a spilled copy. The call-site receiver ABI
// previously always spilled the value-type receiver into a fresh temp and passed
// the temp's pointer as `this`, so any field/setter write inside the method
// landed on the throwaway copy. The fix passes the caller's in-place storage
// address (via genValueTypeReceiverAddr) for a `~this` receiver, while a plain
// `this` (RefNone) receiver still spills (value semantics preserved).

// A `~this` value-type method call on a local receiver passes a bitcast of the
// local's own alloca as `this` — no per-call spill temp.
func TestT1358_MutThisReceiverInPlace(t *testing.T) {
	ir := generateIR(t, `
		type Vec2 { int x `+"`value"+`; int y `+"`value"+`;
			set_x(~this, int v) { this.x = v; } }
		caller() {
			a := Vec2(x: 1, y: 2);
			a.set_x(9);
		}
		main() { caller(); }
	`)
	body := extractFunction(ir, "__user.caller")
	if body == "" {
		t.Fatalf("expected @__user.caller in IR:\n%s", ir)
	}
	// The receiver is the local's own storage: bitcast of %a, passed to set_x.
	if !strings.Contains(body, "bitcast %promise_Vec2_v* %a to i8*") {
		t.Fatalf("expected the local's alloca (%%a) passed as `this` in place:\n%s", body)
	}
	if !strings.Contains(body, "@Vec2.set_x(i8*") {
		t.Fatalf("expected value-type `~this` method call `@Vec2.set_x(i8* ...)`:\n%s", body)
	}
	// Only the local `a` allocates a Vec2 value — no separate spill temp for the
	// receiver.
	if n := strings.Count(body, "alloca %promise_Vec2_v"); n != 1 {
		t.Fatalf("expected exactly 1 `alloca %%promise_Vec2_v` (the local, no spill), got %d:\n%s", n, body)
	}
}

// A `~this` value-type method carrying its own method-level type param routes
// through genGenericMethodCall (the type-arg call path), which must ALSO pass the
// local's in-place storage — not a spill — so the write reaches the caller.
func TestT1358_MutThisGenericMethodInPlace(t *testing.T) {
	ir := generateIR(t, `
		type Box[T] { T val `+"`value"+`;
			put[U](~this, T move v) { this.val = v; } }
		caller() {
			b := Box[int](val: 1);
			b.put[bool](7);
		}
		main() { caller(); }
	`)
	body := extractFunction(ir, "__user.caller")
	if body == "" {
		t.Fatalf("expected @__user.caller in IR:\n%s", ir)
	}
	// The generic-method receiver is the local's own alloca, passed in place.
	if !strings.Contains(body, `bitcast %"promise_Box[int]_v"* %b to i8*`) {
		t.Fatalf("expected the local's alloca (%%b) passed as `this` in place:\n%s", body)
	}
	if !strings.Contains(body, `@"Box[int].put[bool]"(i8*`) {
		t.Fatalf("expected generic `~this` method call `@\"Box[int].put[bool]\"(i8* ...)`:\n%s", body)
	}
	// Only the local `b` allocates the value struct — no separate spill temp.
	if n := strings.Count(body, `alloca %"promise_Box[int]_v"`); n != 1 {
		t.Fatalf("expected exactly 1 Box[int] alloca (the local, no spill), got %d:\n%s", n, body)
	}
}

// A `~this` value-type method invoked on a NON-addressable receiver (a getter
// result, which is a fresh copy) must fall back to spilling — genValueTypeReceiverAddr
// returns ok=false, so genValueTypeReceiverArg spills into a temp and the caller's
// real storage is untouched (value semantics preserved).
func TestT1358_MutThisNonAddressableReceiverSpills(t *testing.T) {
	ir := generateIR(t, `
		type Vec2 { int x `+"`value"+`; int y `+"`value"+`;
			set_x(~this, int v) { this.x = v; } }
		type Holder { Vec2 v `+"`value"+`;
			get inner Vec2 { return this.v; } }
		caller() {
			h := Holder(v: Vec2(x: 1, y: 2));
			h.inner.set_x(9);
		}
		main() { caller(); }
	`)
	body := extractFunction(ir, "__user.caller")
	if body == "" {
		t.Fatalf("expected @__user.caller in IR:\n%s", ir)
	}
	if !strings.Contains(body, "@Vec2.set_x(i8*") {
		t.Fatalf("expected value-type `~this` method call `@Vec2.set_x(i8* ...)`:\n%s", body)
	}
	// The getter yields a temp Vec2 that is spilled for the receiver — at least one
	// standalone Vec2 alloca exists (the getter-result spill), distinct from Holder.
	if !strings.Contains(body, "alloca %promise_Vec2_v") {
		t.Fatalf("expected a spilled Vec2 receiver temp (getter result), got none:\n%s", body)
	}
}

// A plain `this` (RefNone) value-type method call still spills the receiver into
// a fresh temp — value semantics are preserved for read-only methods.
func TestT1358_PlainThisReceiverSpills(t *testing.T) {
	ir := generateIR(t, `
		type Vec2 { int x `+"`value"+`; int y `+"`value"+`;
			get_x(this) int { return this.x; } }
		caller() {
			a := Vec2(x: 1, y: 2);
			z := a.get_x();
		}
		main() { caller(); }
	`)
	body := extractFunction(ir, "__user.caller")
	if body == "" {
		t.Fatalf("expected @__user.caller in IR:\n%s", ir)
	}
	if !strings.Contains(body, "@Vec2.get_x(i8*") {
		t.Fatalf("expected value-type method call `@Vec2.get_x(i8* ...)`:\n%s", body)
	}
	// Two Vec2 allocas: the local `a` plus the spill temp for the receiver.
	if n := strings.Count(body, "alloca %promise_Vec2_v"); n != 2 {
		t.Fatalf("expected 2 `alloca %%promise_Vec2_v` (local + receiver spill), got %d:\n%s", n, body)
	}
}
