package regress8

import (
	"strings"
	"testing"

	"github.com/promise-language/promise/compiler/internal/codegen/codegentest"
)

// T1354: a property setter on a pure value type previously wrote to a spilled
// temporary — genSetterCall passed valueTypeReceiverPtr(loaded copy), which
// allocated a fresh temp alloca and mutated that, discarding the caller's
// write. For an addressable local, genSetterCall now passes the address of the
// variable's own alloca (mirroring genFieldPtr's value-type path), so the
// setter mutates in place.

// The setter receives a bitcast of the local `%v` alloca directly, and there is
// no second value-struct alloca acting as a spill temp.
func TestT1354_ValueTypeSetterMutatesInPlace(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Vec2 {
			int x `+"`value"+`;
			int y `+"`value"+`;
			get sum int { return this.x + this.y; }
			set sum(int v) { this.x = v; this.y = 0; }
		}
		caller() {
			v := Vec2(x: 3, y: 4);
			v.sum = 17;
		}
		main() { caller(); }
	`)
	body := codegentest.ExtractFunction(ir, "__user.caller")
	if body == "" {
		t.Fatalf("expected @__user.caller in IR:\n%s", ir)
	}
	// The setter must receive the address of the caller's own `%v` alloca.
	if !strings.Contains(body, "bitcast %promise_Vec2_v* %v to i8*") {
		t.Fatalf("expected setter to receive `bitcast %%promise_Vec2_v* %%v to i8*` (in-place mutation):\n%s", body)
	}
	if !strings.Contains(body, "@Vec2.sum$set(i8*") {
		t.Fatalf("expected value-type setter call `@Vec2.sum$set(i8* ...)`:\n%s", body)
	}
	// Exactly one value-struct alloca — no spill temp for the receiver.
	if n := strings.Count(body, "alloca %promise_Vec2_v"); n != 1 {
		t.Fatalf("expected exactly 1 `alloca %%promise_Vec2_v` (no spill temp), got %d:\n%s", n, body)
	}
}

// Compound assignment (`+=`) through a value-type setter also mutates in place:
// the getter reads a copy (fine), the operator applies, and the setter writes
// back into the local's own alloca.
func TestT1354_ValueTypeSetterCompoundMutatesInPlace(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Vec2 {
			int x `+"`value"+`;
			int y `+"`value"+`;
			get sum int { return this.x + this.y; }
			set sum(int v) { this.x = v; this.y = 0; }
		}
		caller() {
			v := Vec2(x: 3, y: 4);
			v.sum += 10;
		}
		main() { caller(); }
	`)
	body := codegentest.ExtractFunction(ir, "__user.caller")
	if body == "" {
		t.Fatalf("expected @__user.caller in IR:\n%s", ir)
	}
	if !strings.Contains(body, "bitcast %promise_Vec2_v* %v to i8*") {
		t.Fatalf("expected compound setter to receive `bitcast %%promise_Vec2_v* %%v to i8*` (in-place mutation):\n%s", body)
	}
	if !strings.Contains(body, "@Vec2.sum$set(i8*") {
		t.Fatalf("expected value-type setter call `@Vec2.sum$set(i8* ...)`:\n%s", body)
	}
}

// Non-addressable receiver (a value type returned by a call) has nothing to
// write back to, so it keeps the spill via valueTypeReceiverPtr — it must still
// compile and produce a setter call.
func TestT1354_ValueTypeSetterNonAddressableKeepsSpill(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Vec2 {
			int x `+"`value"+`;
			int y `+"`value"+`;
			get sum int { return this.x + this.y; }
			set sum(int v) { this.x = v; this.y = 0; }
		}
		make_vec() Vec2 { return Vec2(x: 1, y: 2); }
		caller() {
			make_vec().sum = 99;
		}
		main() { caller(); }
	`)
	body := codegentest.ExtractFunction(ir, "__user.caller")
	if body == "" {
		t.Fatalf("expected @__user.caller in IR:\n%s", ir)
	}
	if !strings.Contains(body, "@Vec2.sum$set(i8*") {
		t.Fatalf("expected value-type setter call `@Vec2.sum$set(i8* ...)`:\n%s", body)
	}
	// The call result is spilled to a temp alloca (no `%v` local to target).
	if !strings.Contains(body, "alloca %promise_Vec2_v") {
		t.Fatalf("expected a spill alloca for the non-addressable receiver:\n%s", body)
	}
}
