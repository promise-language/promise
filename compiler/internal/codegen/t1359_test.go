package codegen

import (
	"strings"
	"testing"
)

// T1359: a DIRECT field assignment to a pure value type reached through a
// NON-addressable receiver (a getter member that returns a fresh value) must
// not panic codegen. genValueTypeReceiverAddr returns ok=false for the getter
// member, so genFieldPtr spills the getter result into a throwaway temp and
// GEPs into it — the field store lands on the temp and is discarded, matching
// the value-type-setter and heap-field-assign paths that already ship.
func TestT1359_NonAddressableValueFieldAssignSpills(t *testing.T) {
	ir := generateIR(t, `
		type Vec2 { int x `+"`value"+`; int y `+"`value"+`; }
		type Holder { get fresh Vec2 { return Vec2(x: 3, y: 4); } }
		caller() {
			h := Holder();
			h.fresh.x = 99;
		}
		main() { caller(); }
	`)
	body := extractFunction(ir, "__user.caller")
	if body == "" {
		t.Fatalf("expected @__user.caller in IR:\n%s", ir)
	}
	// The getter is invoked to produce the fresh receiver value.
	if !strings.Contains(body, "@Holder.fresh(") {
		t.Fatalf("expected getter call `@Holder.fresh(...)`:\n%s", body)
	}
	// The getter result is spilled into a Vec2 temp (nothing to write back to).
	if !strings.Contains(body, "alloca %promise_Vec2_v") {
		t.Fatalf("expected a spilled Vec2 receiver temp for the getter result:\n%s", body)
	}
	// The field store still lands (on the discarded temp): a GEP into the value
	// struct followed by a store of the constant 99.
	if !strings.Contains(body, "getelementptr %promise_Vec2_v") {
		t.Fatalf("expected a field GEP into the spilled Vec2 temp:\n%s", body)
	}
	if !strings.Contains(body, "store i64 99") {
		t.Fatalf("expected the field store of 99 into the temp:\n%s", body)
	}
}
