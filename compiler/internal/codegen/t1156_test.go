package codegen

import "testing"

// T1156: `t := go obj.method(Enum.V(heap_payload))` routes through the via-block
// path (genGoCallExprViaBlock), which generates the whole call — including inline
// enum-ctor args — inside the coroutine body. Before the fix that path ran
// cleanupStmtTemps but never the statement-end enum-ctor drop loop, so a BORROW
// param's cloned payload leaked. The coro body must now drop it via the synthesized
// per-enum drop fn (mirroring stmt.go's statement-end enum cleanup).
func TestT1156_GoMethodEnumPayloadBorrowDropsInGoroutine(t *testing.T) {
	ir := generateIR(t, `
		enum Msg { Text(string body), Empty, }
		type P {
			int d;
			process(this, Msg m) int {
				return match m { Msg.Text(b) => b.len, Msg.Empty => 0, };
			}
		}
		probe() {
			p := P(d: 0);
			t := go p.process(Msg.Text("hi".clone()));
			_ := <-t;
		}
		main() { probe(); }
	`)
	coro := extractDefine(ir, ".goroutine.0")
	if coro == "" {
		t.Fatalf(".goroutine.0 not found in IR")
	}
	// The via-block coro body must drop the inline enum-ctor payload.
	assertContains(t, coro, "call void @Msg.drop")
}

// T1156: the MOVE-param variant of the via-block path must NOT emit a goroutine-side
// enum drop — the callee consumes and drops the by-value enum (a second drop would
// be a double-free). The enum-ctor drop loop is gated on the drop flag, which normal
// call codegen clears for consumed move args.
func TestT1156_GoMethodEnumPayloadMoveNoGoroutineDrop(t *testing.T) {
	ir := generateIR(t, `
		enum Msg { Text(string body), Empty, }
		type P {
			int d;
			process(this, Msg move m) int {
				return match m { Msg.Text(b) => b.len, Msg.Empty => 0, };
			}
		}
		probe() {
			p := P(d: 0);
			t := go p.process(Msg.Text("hi".clone()));
			_ := <-t;
		}
		main() { probe(); }
	`)
	coro := extractDefine(ir, ".goroutine.0")
	if coro == "" {
		t.Fatalf(".goroutine.0 not found in IR")
	}
	assertNotContains(t, coro, "call void @Msg.drop")
}
