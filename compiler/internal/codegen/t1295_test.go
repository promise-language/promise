package codegen

import (
	"regexp"
	"strings"
	"testing"
)

// T1295: Mutating an inner container through a mutable optional-unwrap place
// (`v[i]!.push(x)`) on a Vector[Optional[Vector[T]]] must write the grown
// pointer back into the optional's PAYLOAD field (struct index 1), not discard
// it. Before the fix, evalVectorReceiver treated the `place!` receiver as an
// rvalue with a nil write-back slot, so storeBackSlicePtr had no case for it:
// the mutation was lost and the fresh COW/realloc buffer leaked. The fix
// captures the optional payload field as the slot, so push's result stores
// straight back through the index-1 GEP.
func TestT1295OptionalVectorMutateWritesBackToPayload(t *testing.T) {
	ir := generateIR(t, `
		mutate() {
			int[]?[] v = [];
			int[]? a = [1, 2];
			v.push(move a);
			v[0]!.push(7);
		}
	`)
	fn := extractFunction(ir, "__user.mutate")
	if fn == "" {
		t.Fatalf("could not extract @__user.mutate from IR:\n%s", ir)
	}
	// Scope to the `!`-unwrap success block so the earlier `v.push(move a)`
	// optional-payload access can't match the write-back assertion.
	if idx := strings.Index(fn, "unwrap.ok"); idx >= 0 {
		fn = fn[idx:]
	} else {
		t.Fatalf("expected an unwrap.ok block for `v[0]!`, got:\n%s", fn)
	}

	// The payload field is addressed via `getelementptr {i1,i8*} ..., i32 0, i32 1`
	// and the grown pointer from promise_vector_push is stored back through it.
	payloadGEP := regexp.MustCompile(`(%\d+) = getelementptr \{ i1, i8\* \}, \{ i1, i8\* \}\* %\d+, i32 0, i32 1`)
	m := payloadGEP.FindStringSubmatch(fn)
	if m == nil {
		t.Fatalf("expected a GEP into the optional payload field (index 1) inside unwrap.ok, got:\n%s", fn)
	}
	payloadReg := m[1]

	// The push result must be stored back through that exact payload pointer —
	// this is the write-back that fixes both the lost mutation and the leak.
	writeBack := regexp.MustCompile(`store i8\* %\d+, i8\*\* ` + regexp.QuoteMeta(payloadReg) + `\b`)
	if !writeBack.MatchString(fn) {
		t.Fatalf("expected grown pointer stored back through payload GEP %s, got:\n%s", payloadReg, fn)
	}
}

// assertPayloadWriteBack scopes IR to the `!`-unwrap success block and asserts
// the grown Vector pointer is stored straight back through the optional payload
// GEP (index 1). Shared by the per-place-kind cases below (T1295).
func assertPayloadWriteBack(t *testing.T, fn string) {
	t.Helper()
	if idx := strings.Index(fn, "unwrap.ok"); idx >= 0 {
		fn = fn[idx:]
	} else {
		t.Fatalf("expected an unwrap.ok block, got:\n%s", fn)
	}
	payloadGEP := regexp.MustCompile(`(%\d+) = getelementptr \{ i1, i8\* \}, \{ i1, i8\* \}\* %[\w.]+, i32 0, i32 1`)
	m := payloadGEP.FindStringSubmatch(fn)
	if m == nil {
		t.Fatalf("expected a GEP into the optional payload field (index 1) inside unwrap.ok, got:\n%s", fn)
	}
	writeBack := regexp.MustCompile(`store i8\* %\d+, i8\*\* ` + regexp.QuoteMeta(m[1]) + `\b`)
	if !writeBack.MatchString(fn) {
		t.Fatalf("expected grown pointer stored back through payload GEP %s, got:\n%s", m[1], fn)
	}
}

// T1295: the local-variable place kind (`a!.push(x)` where `a` is an
// Optional[Vector[T]] local) must resolve the alloca and write back through the
// payload slot (the IdentExpr/locals branch of optionalPayloadReceiverSlot).
func TestT1295OptionalVectorMutateLocalVarWriteBack(t *testing.T) {
	ir := generateIR(t, `
		mutate() {
			int[]? a = [1, 2];
			a!.push(7);
		}
	`)
	fn := extractFunction(ir, "__user.mutate")
	if fn == "" {
		t.Fatalf("could not extract @__user.mutate from IR:\n%s", ir)
	}
	assertPayloadWriteBack(t, fn)
}

// T1295: the struct-field place kind (`h.f!.push(x)`) must address the field via
// genFieldPtr and write back through the payload slot (the MemberExpr branch).
func TestT1295OptionalVectorMutateFieldWriteBack(t *testing.T) {
	ir := generateIR(t, `
		type Holder { int[]? f; }
		mutate() {
			h := Holder(f: [1, 2]);
			h.f!.push(7);
		}
	`)
	fn := extractFunction(ir, "__user.mutate")
	if fn == "" {
		t.Fatalf("could not extract @__user.mutate from IR:\n%s", ir)
	}
	assertPayloadWriteBack(t, fn)
}

// T1295: a parenthesized place (`(a)!.push(x)`) must strip the ParenExpr wrapper
// and still resolve the underlying local, exercising the ParenExpr unwrap loop.
func TestT1295OptionalVectorMutateParenWriteBack(t *testing.T) {
	ir := generateIR(t, `
		mutate() {
			int[]? a = [1, 2];
			(a)!.push(7);
		}
	`)
	fn := extractFunction(ir, "__user.mutate")
	if fn == "" {
		t.Fatalf("could not extract @__user.mutate from IR:\n%s", ir)
	}
	assertPayloadWriteBack(t, fn)
}
