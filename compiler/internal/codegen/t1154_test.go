package codegen

import "testing"

// T1154: `t := go f(Enum.V(heap_payload))` with a BORROW param must transfer
// ownership of the inline enum-constructor temporary's droppable payload into
// the goroutine frame — the coroutine body spills the by-value enum param to a
// frame slot and calls the synthesized per-enum drop fn after the target call
// returns. Before the fix the enum-ctor temp was removed from the caller's
// statement-end cleanup with no replacement goroutine-side drop → leak.
func TestT1154_GoCallEnumPayloadBorrowDropsInGoroutine(t *testing.T) {
	ir := generateIR(t, `
		enum Msg { Text(string body), Empty, }
		msg_len(Msg m) int {
			return match m { Msg.Text(b) => b.len, Msg.Empty => 0, };
		}
		probe() {
			t := go msg_len(Msg.Text("hello".clone()));
			_ := <-t;
		}
		main() { probe(); }
	`)
	coro := extractDefine(ir, ".goroutine.0")
	if coro == "" {
		t.Fatalf(".goroutine.0 not found in IR")
	}
	// The goroutine body calls the target then drops the enum payload via the
	// synthesized enum drop fn.
	assertContains(t, coro, "@__user.msg_len")
	assertContains(t, coro, "call void @Msg.drop")
}

// T1154: the MOVE-param variant is consumed and dropped by the callee, so the
// goroutine must NOT emit a second enum drop (that would be a double-free). The
// enum-ctor temp is still removed from caller cleanup (no statement-end drop).
func TestT1154_GoCallEnumPayloadMoveNoGoroutineDrop(t *testing.T) {
	ir := generateIR(t, `
		enum Msg { Text(string body), Empty, }
		msg_len(Msg move m) int {
			return match m { Msg.Text(b) => b.len, Msg.Empty => 0, };
		}
		probe() {
			t := go msg_len(Msg.Text("hello".clone()));
			_ := <-t;
		}
		main() { probe(); }
	`)
	coro := extractDefine(ir, ".goroutine.0")
	if coro == "" {
		t.Fatalf(".goroutine.0 not found in IR")
	}
	assertContains(t, coro, "@__user.msg_len")
	// No goroutine-side enum drop for a move param — the callee owns and drops it.
	assertNotContains(t, coro, "call void @Msg.drop")
}

// T1154: a VECTOR (non-string) droppable enum payload must route through the
// per-enum drop fn (the isEnum branch), NOT be mistaken for a bare heap-vector
// root. The synthesized Msg.drop switches on the tag and frees the vector buffer;
// the goroutine must call it (and must NOT call Vector.drop directly on the param).
func TestT1154_GoCallEnumVectorPayloadDropsViaEnumDrop(t *testing.T) {
	ir := generateIR(t, `
		enum Msg { Text(string body), List(int[] items), Empty, }
		msg_len(Msg m) int {
			return match m { Msg.Text(b) => b.len, Msg.List(items) => items.len, Msg.Empty => 0, };
		}
		probe() {
			t := go msg_len(Msg.List([1, 2, 3]));
			_ := <-t;
		}
		main() { probe(); }
	`)
	coro := extractDefine(ir, ".goroutine.0")
	if coro == "" {
		t.Fatalf(".goroutine.0 not found in IR")
	}
	assertContains(t, coro, "@__user.msg_len")
	// Dropped via the synthesized per-enum drop fn, which internally frees the vector.
	assertContains(t, coro, "call void @Msg.drop")
}
