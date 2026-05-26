package codegen

import (
	"strings"
	"testing"
)

// T0663: Channel.drop must drop heap-allocated buffered items before freeing
// the ring buffer. Pre-fix, the single @Channel.drop symbol freed the ring
// buffer as raw bytes, leaking one allocation per un-received heap item.
// The fix makes Channel.drop per-element-type (like Arc/Mutex/Task) and walks
// [head, head+count) mod capacity dropping each element first.

// extractDefinedFunc returns the body of the function whose definition line is
// `define ... @"<name>"(`. Unlike extractFunction, it anchors on the `define`
// (not the first textual `@name(` match) — necessary here because lazily
// created drop functions are emitted after their call sites in the IR text.
func extractDefinedFunc(ir, sig string) string {
	start := strings.Index(ir, sig)
	if start < 0 {
		return ""
	}
	rest := ir[start:] // sig already begins at the `define` line
	if end := strings.Index(rest, "\n}\n"); end >= 0 {
		return rest[:end+2]
	}
	return rest
}

// Channel[string].drop must contain the ring-buffer element-drop loop:
// a urem (slot = (head+idx) % capacity) and a promise_string_drop call,
// and these must appear before the ring buffer pal_free.
func TestT0663ChannelStringDropWalksHeapElements(t *testing.T) {
	ir := generateIR(t, `
		mk(int n) string { return "v" + n.to_string(); }
		main() {
			c := channel[string](capacity: 2);
			c.send(mk(1));
		}
	`)
	assertContains(t, ir, `define void @"Channel[string].drop"(i8* %this)`)

	dropFn := extractDefinedFunc(ir, `define void @"Channel[string].drop"(`)
	if dropFn == "" {
		t.Fatalf("expected Channel[string].drop function in IR")
	}
	// Ring-buffer element walk: modulo indexing + per-element string drop.
	assertContains(t, dropFn, "chdrop.head")
	assertContains(t, dropFn, "urem i64")
	assertContains(t, dropFn, "call void @promise_string_drop(")

	// The element walk must run BEFORE the ring buffer is freed as raw bytes,
	// otherwise the element pointers would be read from freed memory.
	strDropIdx := strings.Index(dropFn, "call void @promise_string_drop(")
	bufFreeIdx := strings.Index(dropFn, "call void @pal_free(")
	if strDropIdx < 0 || bufFreeIdx < 0 || strDropIdx > bufFreeIdx {
		t.Errorf("expected promise_string_drop before the ring-buffer pal_free "+
			"(string idx=%d, pal_free idx=%d):\n%s", strDropIdx, bufFreeIdx, dropFn)
	}
}

// Channel[int].drop (value element) must NOT emit an element-drop loop —
// preserves the pre-T0663 codegen for the overwhelmingly common case.
func TestT0663ChannelIntNoElementLoop(t *testing.T) {
	ir := generateIR(t, `
		main() {
			c := channel[int](capacity: 4);
			c.send(7);
		}
	`)
	assertContains(t, ir, `define void @"Channel[int].drop"(i8* %this)`)

	dropFn := extractDefinedFunc(ir, `define void @"Channel[int].drop"(`)
	if dropFn == "" {
		t.Fatalf("expected Channel[int].drop function in IR")
	}
	if strings.Contains(dropFn, "chdrop.head") || strings.Contains(dropFn, "urem i64") {
		t.Errorf("Channel[int].drop must NOT contain an element-drop loop "+
			"(int is a value type):\n%s", dropFn)
	}
	// Still does the channel's own teardown.
	assertContains(t, dropFn, "call void @pal_free(")
	assertContains(t, dropFn, "call void @pal_mutex_destroy(")
}

// Channel[Box].drop (heap user type without explicit drop) must pal_free the
// extracted instance pointer of each buffered element (emitVariantFieldDrop's
// B0218 heap-user path).
func TestT0663ChannelUserTypeDropFreesInstances(t *testing.T) {
	ir := generateIR(t, `
		type Box { int v; new(~this, int v) { this.v = v; } }
		main() {
			c := channel[Box](capacity: 2);
			c.send(Box(1));
		}
	`)
	assertContains(t, ir, `define void @"Channel[Box].drop"(i8* %this)`)
	dropFn := extractDefinedFunc(ir, `define void @"Channel[Box].drop"(`)
	if dropFn == "" {
		t.Fatalf("expected Channel[Box].drop function in IR")
	}
	assertContains(t, dropFn, "chdrop.head")
	// Heap user type element → free its instance during the walk.
	assertContains(t, dropFn, "call void @pal_free(")
}

// Nested Channel[Channel[int]].drop must walk its buffered inner channels and
// dispatch to the per-element-type Channel[int].drop (recursion-safe; the
// inner element narrows to a value type so it terminates).
func TestT0663NestedChannelDropDispatchesInner(t *testing.T) {
	ir := generateIR(t, `
		main() {
			outer := channel[channel[int]](capacity: 1);
			inner := channel[int](capacity: 1);
			outer.send(inner);
		}
	`)
	assertContains(t, ir, `define void @"Channel[Channel[int]].drop"(i8* %this)`)
	outerDrop := extractDefinedFunc(ir, `define void @"Channel[Channel[int]].drop"(`)
	if outerDrop == "" {
		t.Fatalf("expected Channel[Channel[int]].drop in IR")
	}
	assertContains(t, outerDrop, "chdrop.head")
	// Each buffered inner channel dropped via its own per-element-type drop.
	assertContains(t, outerDrop, `call void @"Channel[int].drop"(`)
}

// The single non-parameterized @Channel.drop symbol must no longer exist —
// every channel drop is now per element type.
func TestT0663NoLegacyChannelDropSymbol(t *testing.T) {
	ir := generateIR(t, `
		main() {
			c := channel[int](capacity: 1);
			c.send(1);
		}
	`)
	assertNotContains(t, ir, "define void @Channel.drop(")
	assertNotContains(t, ir, "call void @Channel.drop(")
}

// Channel struct-field reassignment must drop the displaced old channel via the
// per-element-type drop (genMemberAssign Channel branch). Pre-T0663 this site
// looked up the now-deleted single @Channel.drop symbol; the displaced channel
// (and its buffered heap items) would leak entirely. Covers a call site not
// exercised by the local-var / synthesized-field tests.
func TestT0663ChannelFieldReassignDropsOldChannel(t *testing.T) {
	ir := generateIR(t, `
		type Cell { channel[string] ch; }
		mk(int n) string { return "v" + n.to_string(); }
		main() {
			c1 := channel[string](capacity: 2);
			c1.send(mk(1));
			cell := Cell(ch: c1);
			c2 := channel[string](capacity: 2);
			cell.ch = c2;
		}
	`)
	// The reassign emits a same-pointer guard then drops the old channel value
	// through the per-element-type drop (which itself walks buffered strings).
	assertContains(t, ir, "field.chdrop")
	assertContains(t, ir, `call void @"Channel[string].drop"(`)
	assertNotContains(t, ir, "call void @Channel.drop(")
}

// Optional[Channel[string]] local dropped without unwrapping must dispatch to
// the per-element-type Channel[string].drop (maybeRegisterOptionalDrop branch),
// and that drop must contain the heap-element walk.
func TestT0663OptionalChannelScopeExitDrop(t *testing.T) {
	ir := generateIR(t, `
		mk(int n) string { return "v" + n.to_string(); }
		fresh() Channel[string]? {
			c := channel[string](capacity: 2);
			c.send(mk(1));
			return c;
		}
		main() {
			oc := fresh();
		}
	`)
	assertContains(t, ir, `call void @"Channel[string].drop"(`)
	assertContains(t, ir, `define void @"Channel[string].drop"(i8* %this)`)
	dropFn := extractDefinedFunc(ir, `define void @"Channel[string].drop"(`)
	if dropFn == "" {
		t.Fatalf("expected Channel[string].drop function in IR")
	}
	// The optional path must still reach the element-drop walk.
	assertContains(t, dropFn, "chdrop.head")
	assertContains(t, dropFn, "call void @promise_string_drop(")
	assertNotContains(t, ir, "call void @Channel.drop(")
}
