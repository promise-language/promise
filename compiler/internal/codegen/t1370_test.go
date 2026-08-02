package codegen

import (
	"regexp"
	"strings"
	"testing"
)

// T1370: push() on an rvalue Vector receiver (a Vector returned by a
// method/getter/call, not an addressable place) must relocate safely. The
// receiver is registered as a statement temp whose drop reads a backing alloca;
// a relocating push realloc's the buffer, so the grown pointer MUST be stored
// back into that same alloca or cleanupStmtTemps frees the stale pre-realloc
// pointer (double-free / "bad header magic") or leaks the grown buffer.
//
// The fix makes evalVectorReceiver hand the temp's alloca back as the write-back
// slot. This test pins that the push result is stored back into the alloca that
// received the getter result — i.e. the write-back is no longer dropped.
func TestT1370RvalueVectorPushStoresBack(t *testing.T) {
	ir := generateIR(t, `
		type IntBox {
			Vector[int][] _data;
			new(~this) { this._data = [[1, 2, 3]]; }
			get_vec(int i) Vector[int] => this._data[i];
		}
		run() {
			b := IntBox();
			b.get_vec(0).push(99);
		}
		main() {}
	`)

	assertContains(t, ir, "call i8* @promise_vector_push(")

	// Scope all matching to the user run() body — LLVM register numbers restart
	// per-function, so a whole-module search could match a store in an unrelated
	// function that happens to reuse the same %N register name.
	body := funcBody(t, ir, "run")

	// The getter result is tracked as a statement temp: its pointer is stored
	// into a backing alloca slot (`store i8* <getter>, i8** <slot>`). The fix
	// makes push's grown pointer store back into that SAME slot so the temp's
	// drop frees the live buffer, not the stale pre-realloc one. Tie the
	// store-back to the exact slot (register numbering alone is not enough — a
	// coincidental register collision elsewhere must not satisfy the assertion).
	getterRe := regexp.MustCompile(`(%\w+) = call i8\* @IntBox\.get_vec\(`)
	gm := getterRe.FindStringSubmatch(body)
	if gm == nil {
		t.Fatalf("no IntBox.get_vec result register found in run() body:\n%s", body)
	}
	slotRe := regexp.MustCompile(`store i8\* ` + regexp.QuoteMeta(gm[1]) + `, (i8\*\* %\w+)`)
	sm := slotRe.FindStringSubmatch(body)
	if sm == nil {
		t.Fatalf("getter result %s is not tracked in a temp slot:\n%s", gm[1], body)
	}
	slot := sm[1] // e.g. "i8** %8"

	pushRe := regexp.MustCompile(`(%\w+) = call i8\* @promise_vector_push\(`)
	pm := pushRe.FindStringSubmatch(body)
	if pm == nil {
		t.Fatalf("no promise_vector_push result register found in run() body:\n%s", body)
	}
	pushResult := pm[1]

	storeBack := "store i8* " + pushResult + ", " + slot
	if !strings.Contains(body, storeBack) {
		t.Fatalf("push result %s is not stored back into the receiver temp slot %s "+
			"(%q not found) — the relocated buffer would be dropped/leaked "+
			"(T1370 regression):\n%s", pushResult, slot, storeBack, body)
	}
}
