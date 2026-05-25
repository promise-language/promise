package codegen

import (
	"strings"
	"testing"
)

// T0638: `<-coll[i]` where the indexed element IS the Task/handle. The receive
// frees the G (pal_free); without nulling the slot, the collection's
// scope-exit element drop reloads the dangling pointer and Task[int].drop
// (only a null-check) double-frees → segfault. genReceiveTask now nulls the
// array/Vector element slot after operand eval so the element drop no-ops.
//
// These tests assert the structural signature: the receive-operand in-bounds
// block (`arridx.ok` for fixed arrays, `index.ok` for Vector) contains a
// `store i8* null, i8**` (the slot-null) — absent pre-fix. The runtime
// no-double-free / zero-leak guarantee is verified by the Promise e2e tests
// in tests/concurrency/task_drop_test.pr.

// blockByPrefixT0638 returns the basic block whose label line starts with
// prefix (e.g. "arridx.ok"), from the label line up to the next blank line.
// Searches for the label *definition* ("\n<prefix>...:") so it doesn't match
// "label %<prefix>" branch references.
func blockByPrefixT0638(body, prefix string) string {
	idx := strings.Index(body, "\n"+prefix)
	if idx < 0 {
		return ""
	}
	rest := body[idx+1:]
	if end := strings.Index(rest, "\n\n"); end >= 0 {
		return rest[:end]
	}
	return rest
}

// TestT0638_TaskArrayRecvNullsSlot — `<-ts[0]` on a `Task[int][2]` fixed array
// must null the indexed slot in `%ts` inside the in-bounds operand block, and
// the scope-exit array drop walk (@"Task[int].drop") must still be wired.
func TestT0638_TaskArrayRecvNullsSlot(t *testing.T) {
	ir := generateIR(t, `
		w() int { return 7; }
		caller() {
			Task[int][2] ts = [go w(), go w()];
			x := <-ts[0];
		}
		main() { caller(); }
	`)
	body := extractFunction(ir, "__user.caller")
	if body == "" {
		t.Fatalf("expected __user.caller in IR")
	}
	okBlk := blockByPrefixT0638(body, "arridx.ok")
	if okBlk == "" {
		t.Fatalf("expected arridx.ok block (array receive operand):\n%s", body)
	}
	// The T0638 slot-null: a store of null i8* into the array element slot,
	// emitted right after the operand G load. Absent before the fix.
	if !strings.Contains(okBlk, "store i8* null, i8**") {
		t.Errorf("expected slot-null `store i8* null, i8**` in arridx.ok block (T0638):\n%s", okBlk)
	}
	// The scope-exit array element drop walk must still be present — the slot
	// is nulled, not the whole array, so the drop walk runs and null-checks.
	if !strings.Contains(body, `@"Task[int].drop"`) {
		t.Errorf("expected @\"Task[int].drop\" scope-exit element drop walk:\n%s", body)
	}
	// Ordering: the slot-null must precede the scope-exit Task drop walk so the
	// drop reloads null and no-ops (the consume/double-free regression guard).
	nullIdx := strings.Index(body, "store i8* null, i8**")
	dropIdx := strings.Index(body, `@"Task[int].drop"`)
	if nullIdx < 0 || dropIdx < 0 || nullIdx > dropIdx {
		t.Errorf("expected slot-null (idx %d) before scope-exit Task[int].drop (idx %d):\n%s",
			nullIdx, dropIdx, body)
	}
}

// TestT0638_TaskArraySingleElemRecvNullsSlot — single-element array
// (`Task[int][1]`) — proves the fix is not OOB/length-specific.
func TestT0638_TaskArraySingleElemRecvNullsSlot(t *testing.T) {
	ir := generateIR(t, `
		w() int { return 7; }
		caller() {
			Task[int][1] ts = [go w()];
			x := <-ts[0];
		}
		main() { caller(); }
	`)
	body := extractFunction(ir, "__user.caller")
	if body == "" {
		t.Fatalf("expected __user.caller in IR")
	}
	okBlk := blockByPrefixT0638(body, "arridx.ok")
	if okBlk == "" {
		t.Fatalf("expected arridx.ok block:\n%s", body)
	}
	if !strings.Contains(okBlk, "store i8* null, i8**") {
		t.Errorf("expected slot-null in arridx.ok for Task[int][1] (T0638):\n%s", okBlk)
	}
}

// TestT0638_TaskVectorRecvNullsSlot — `<-v[0]` on a `Vector[Task[int]]` must
// null the indexed slot in the heap data buffer inside the `index.ok` block.
func TestT0638_TaskVectorRecvNullsSlot(t *testing.T) {
	ir := generateIR(t, `
		w() int { return 7; }
		caller() {
			Vector[Task[int]] v = [go w(), go w()];
			x := <-v[0];
		}
		main() { caller(); }
	`)
	body := extractFunction(ir, "__user.caller")
	if body == "" {
		t.Fatalf("expected __user.caller in IR")
	}
	okBlk := blockByPrefixT0638(body, "index.ok")
	if okBlk == "" {
		t.Fatalf("expected index.ok block (Vector receive operand):\n%s", body)
	}
	if !strings.Contains(okBlk, "store i8* null, i8**") {
		t.Errorf("expected slot-null `store i8* null, i8**` in index.ok block (T0638 Vector):\n%s", okBlk)
	}
	if !strings.Contains(body, `@"Task[int].drop"`) {
		t.Errorf("expected @\"Task[int].drop\" scope-exit element drop walk:\n%s", body)
	}
}

// TestT0638_ChannelArrayRecvDoesNotNullSlot — non-regression. genReceiveChannel
// does NOT free the channel, so the slot must stay valid for the scope-exit
// drop. The T0638 slot-null is genReceiveTask-only — the channel array's
// receive-operand block must NOT contain a slot-null store.
func TestT0638_ChannelArrayRecvDoesNotNullSlot(t *testing.T) {
	ir := generateIR(t, `
		producer() Channel[int] {
			ch := channel[int](capacity: 1);
			ch.send(99);
			ch.close();
			return ch;
		}
		caller() {
			Channel[int][1] cs = [producer()];
			x := <-cs[0];
		}
		main() { caller(); }
	`)
	body := extractFunction(ir, "__user.caller")
	if body == "" {
		t.Fatalf("expected __user.caller in IR")
	}
	okBlk := blockByPrefixT0638(body, "arridx.ok")
	if okBlk == "" {
		t.Fatalf("expected arridx.ok block (channel array receive operand):\n%s", body)
	}
	if strings.Contains(okBlk, "store i8* null, i8**") {
		t.Errorf("channel array receive operand block must NOT null the slot "+
			"(genReceiveChannel does not free the channel; slot needed for scope-exit drop):\n%s", okBlk)
	}
}
