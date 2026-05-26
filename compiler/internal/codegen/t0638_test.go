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

// TestT0621_TaskFromVarMoveThenVectorRecv — the T0610 × T0638 cross-fix
// intersection that T0621's regression suite targets: a Task moved from a
// *named variable* into a Vector literal, then received via index
// (`Task[int] t = go w(); Task[int][] v = [t]; x := <-v[0]`). The existing
// T0638 tests only cover inline `[go w()]` elements; T0610's own test
// deliberately avoids `<-v[0]`. This locks both halves at the IR level:
//   - T0610: the moved ident's drop flag is cleared
//     (`store i1 false, i1* %t.dropflag`) — a real move-transfer into the
//     vector element, so the source var no longer co-owns the handle. The
//     *named* SSA value `%t.dropflag` ties this specifically to the from-var
//     path (inline elements have no source ident / drop flag).
//   - T0638: the Vector receive-operand in-bounds block (`index.ok`) nulls
//     the slot (`store i8* null, i8**`) so the scope-exit `Vector.drop`
//     element-walk reloads null and `Task[int].drop` no-ops.
//
// No textual-ordering assertion: LLVM lays the scope-exit `vecdrop.*` cleanup
// blocks both before and after the `index.ok` receive block, so a
// `strings.Index` ordering check would be brittle. The runtime
// no-double-free / zero-leak guarantee is in tests/concurrency/
// task_drop_test.pr (t0621_task_from_var et al.).
func TestT0621_TaskFromVarMoveThenVectorRecv(t *testing.T) {
	ir := generateIR(t, `
		w() int { return 7; }
		caller() {
			Task[int] t = go w();
			Task[int][] v = [t];
			x := <-v[0];
		}
		main() { caller(); }
	`)
	body := extractFunction(ir, "__user.caller")
	if body == "" {
		t.Fatalf("expected __user.caller in IR")
	}
	// T0610: the named source ident `t` has its drop flag cleared at the
	// move site (move-transfer into the vector literal).
	if !strings.Contains(body, "store i1 false, i1* %t.dropflag") {
		t.Errorf("expected T0610 move-transfer drop-flag clear "+
			"`store i1 false, i1* %%t.dropflag` for the from-var move:\n%s", body)
	}
	// T0638: the Vector receive-operand in-bounds block nulls the slot.
	okBlk := blockByPrefixT0638(body, "index.ok")
	if okBlk == "" {
		t.Fatalf("expected index.ok block (Vector receive operand):\n%s", body)
	}
	if !strings.Contains(okBlk, "store i8* null, i8**") {
		t.Errorf("expected T0638 slot-null `store i8* null, i8**` in index.ok block:\n%s", okBlk)
	}
	// The scope-exit Vector element drop walk must still be wired — the slot
	// is nulled, not the whole vector, so the drop walk runs and null-checks.
	if !strings.Contains(body, `@"Task[int].drop"`) {
		t.Errorf("expected @\"Task[int].drop\" scope-exit element drop walk:\n%s", body)
	}
}

// TestT0621_ChannelFromVarMoveThenVectorRecv — the channel-side IR lock for
// the T0621 from-var × Vector-receive path, the non-regression counterpart of
// TestT0621_TaskFromVarMoveThenVectorRecv. The existing
// TestT0638_ChannelArrayRecvDoesNotNullSlot only covers an *inline* producer
// into a fixed *array*; this covers a channel moved from a *named variable*
// into a *Vector literal*, then received via index:
//   - T0610: the channel source ident `c` has its drop flag cleared
//     (`store i1 false, i1* %c.dropflag`) — a real move-transfer into the
//     vector element, so the source var no longer co-owns the channel.
//   - T0638 non-regression: genReceiveChannel must NOT null the slot (unlike
//     genReceiveTask) — the channel stays Vector-owned, so the receive-operand
//     in-bounds block (`index.ok`) must contain no `store i8* null, i8**`.
//   - The scope-exit Vector element drop walk (`@"Channel[int].drop"`, T0663
//     per-element-type) must still be wired so the still-owned channel is
//     dropped exactly once.
//
// This is the IR guarantee behind t0621_chan_from_var / _multi / _partial in
// tests/concurrency/task_drop_test.pr.
func TestT0621_ChannelFromVarMoveThenVectorRecv(t *testing.T) {
	ir := generateIR(t, `
		producer() Channel[int] {
			ch := channel[int](capacity: 1);
			ch.send(99);
			ch.close();
			return ch;
		}
		caller() {
			Channel[int] c = producer();
			Channel[int][] v = [c];
			x := <-v[0];
		}
		main() { caller(); }
	`)
	body := extractFunction(ir, "__user.caller")
	if body == "" {
		t.Fatalf("expected __user.caller in IR")
	}
	// T0610: the named channel source ident `c` has its drop flag cleared at
	// the move site (move-transfer into the vector literal).
	if !strings.Contains(body, "store i1 false, i1* %c.dropflag") {
		t.Errorf("expected T0610 move-transfer drop-flag clear "+
			"`store i1 false, i1* %%c.dropflag` for the channel from-var move:\n%s", body)
	}
	// T0638 non-regression: the Vector receive-operand in-bounds block must
	// NOT null the slot — genReceiveChannel does not free the channel, so the
	// slot must stay valid for the scope-exit Vector.drop → Channel[int].drop.
	okBlk := blockByPrefixT0638(body, "index.ok")
	if okBlk == "" {
		t.Fatalf("expected index.ok block (Vector receive operand):\n%s", body)
	}
	if strings.Contains(okBlk, "store i8* null, i8**") {
		t.Errorf("channel Vector receive operand block must NOT null the slot "+
			"(genReceiveChannel does not free the channel; slot needed for "+
			"scope-exit drop):\n%s", okBlk)
	}
	// T0663: the scope-exit Vector element drop walk must still call the
	// per-element-type Channel[int].drop on the still-owned channel (dropped
	// exactly once — no double-free, no leak).
	if !strings.Contains(body, `call void @"Channel[int].drop"(`) {
		t.Errorf("expected `call void @\"Channel[int].drop\"(` scope-exit element drop walk:\n%s", body)
	}
}

// T0617: `<-handle` where `handle` is a for-in loop binding over a
// Vector[Task]/Task[N] element loop. genForInVector/genForInArray bind the
// loop var by value-copy of the slot's G ptr with no drop binding; the
// receive frees the G but the T0638 IndexExpr slot-null does NOT fire for the
// loop-binding IdentExpr operand. Without the new for-in slot-null the
// container still owns every dangling slot → scope-exit Vector[Task].drop /
// array element-drop double-frees → segfault. genForInVector/genForInArray
// now record each iteration's slot in c.forInHandleSlotPtr; genReceiveTask
// nulls it (symmetric to T0638). The runtime no-double-free / zero-leak
// guarantee is verified by tests/concurrency/task_drop_test.pr (t0617_*).
//
// No textual-ordering assertion: LLVM lays the scope-exit `vecdrop.*`
// cleanup blocks *before* the `forin.body` loop, so a `strings.Index`
// ordering check would be brittle (same rationale as
// TestT0621_TaskFromVarMoveThenVectorRecv).

// TestT0617_VectorForInRecvNullsSlot — `for h in v { x := <-h }` over a
// `Vector[Task[int]]` must null the current iteration's slot inside the
// `forin.body` block (the new for-in slot-null), and the scope-exit Vector
// element drop walk (@"Task[int].drop") must still be wired so the nulled
// slot is reloaded and Task[int].drop no-ops.
func TestT0617_VectorForInRecvNullsSlot(t *testing.T) {
	ir := generateIR(t, `
		w() int { return 7; }
		caller() {
			Vector[Task[int]] v = [go w(), go w()];
			for h in v { x := <-h; }
		}
		main() { caller(); }
	`)
	body := extractFunction(ir, "__user.caller")
	if body == "" {
		t.Fatalf("expected __user.caller in IR")
	}
	bodyBlk := blockByPrefixT0638(body, "forin.body")
	if bodyBlk == "" {
		t.Fatalf("expected forin.body block (for-in receive operand):\n%s", body)
	}
	// The T0617 slot-null: a store of null i8* into the recorded slot,
	// emitted at the receive site inside the loop body. Absent before the fix.
	if !strings.Contains(bodyBlk, "store i8* null, i8**") {
		t.Errorf("expected slot-null `store i8* null, i8**` in forin.body block (T0617):\n%s", bodyBlk)
	}
	// The scope-exit Vector element drop walk must still be present — the slot
	// is nulled per-iteration, not the whole vector, so the drop walk runs and
	// Task[int].drop null-checks (T0503 preserved).
	if !strings.Contains(body, `@"Task[int].drop"`) {
		t.Errorf("expected @\"Task[int].drop\" scope-exit element drop walk:\n%s", body)
	}
}

// TestT0617_ArrayForInRecvNullsSlot — same for the fixed-array path:
// `Task[int][2] ts = [...]; for h in ts { <-h }`.
func TestT0617_ArrayForInRecvNullsSlot(t *testing.T) {
	ir := generateIR(t, `
		w() int { return 7; }
		caller() {
			Task[int][2] ts = [go w(), go w()];
			for h in ts { x := <-h; }
		}
		main() { caller(); }
	`)
	body := extractFunction(ir, "__user.caller")
	if body == "" {
		t.Fatalf("expected __user.caller in IR")
	}
	bodyBlk := blockByPrefixT0638(body, "forin.body")
	if bodyBlk == "" {
		t.Fatalf("expected forin.body block (array for-in receive operand):\n%s", body)
	}
	if !strings.Contains(bodyBlk, "store i8* null, i8**") {
		t.Errorf("expected slot-null `store i8* null, i8**` in forin.body block (T0617 array):\n%s", bodyBlk)
	}
	if !strings.Contains(body, `@"Task[int].drop"`) {
		t.Errorf("expected @\"Task[int].drop\" scope-exit element drop walk:\n%s", body)
	}
}

// TestT0617_ForInChannelRecvDoesNotNullSlot — non-regression. genReceiveChannel
// does NOT free the channel and never consults c.forInHandleSlotPtr, so the
// channel for-in loop body must NOT null the slot — the slot must stay valid
// for the scope-exit Vector.drop → Channel[int].drop (no double-free, no leak).
func TestT0617_ForInChannelRecvDoesNotNullSlot(t *testing.T) {
	ir := generateIR(t, `
		producer() Channel[int] {
			ch := channel[int](capacity: 1);
			ch.send(99);
			ch.close();
			return ch;
		}
		caller() {
			Vector[Channel[int]] cs = [producer()];
			for c in cs { x := <-c; }
		}
		main() { caller(); }
	`)
	body := extractFunction(ir, "__user.caller")
	if body == "" {
		t.Fatalf("expected __user.caller in IR")
	}
	bodyBlk := blockByPrefixT0638(body, "forin.body")
	if bodyBlk == "" {
		t.Fatalf("expected forin.body block (channel for-in receive operand):\n%s", body)
	}
	if strings.Contains(bodyBlk, "store i8* null, i8**") {
		t.Errorf("channel for-in body must NOT null the slot "+
			"(genReceiveChannel does not free the channel; slot needed for "+
			"scope-exit drop):\n%s", bodyBlk)
	}
	// T0663: the scope-exit Vector element drop walk must still call the
	// per-element-type Channel[int].drop on the still-owned channel (dropped
	// exactly once — no double-free, no leak).
	if !strings.Contains(body, `call void @"Channel[int].drop"(`) {
		t.Errorf("expected `call void @\"Channel[int].drop\"(` scope-exit element drop walk:\n%s", body)
	}
}
