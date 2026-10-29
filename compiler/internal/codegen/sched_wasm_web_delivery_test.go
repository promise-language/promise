package codegen

import (
	"fmt"
	"regexp"
	"strings"
	"testing"

	"github.com/llir/llvm/ir/constant"
	irtypes "github.com/llir/llvm/ir/types"

	"github.com/promise-language/promise/compiler/internal/wasmweb"
)

// Tests for wasm32-web delivery (docs/web-apps.md §7, §8, §8.1,
// §19 phase 2): the subscription table and the exported promise_web_enqueue.
// Like sched_wasm_web_test.go, these are Go IR-shape tests (§17.1); the
// behaviour is driven end to end under node, with the test playing the host,
// by cmd/promise's TestWebDeliveryQueueSemantics.
//
// Each subscription is backed by a real Channel[i32] (promise_channel_new),
// not a custom ring buffer — promise_web_enqueue writes into that channel's
// own buffer and wakes its recv-waiters the same way channel send does, so
// these tests check for channel-struct field operations (i64 head/tail/
// capacity/count, per compiler_runtime_types.go's channelStructType), not a
// separate ring implementation.

// The subscription table and its accessor functions exist, sized and named
// per this file's design (§14.1-style: one definition, referenced from the
// shared names in internal/wasmweb where the boundary is crossed).
func TestWasmWebSubscriptionTableExists(t *testing.T) {
	ir := generateIRForTarget(t, trivialWasmWebProgram, "wasm32-web")
	assertContains(t, ir, "@promise_web_subscriptions = ")
	assertContains(t, ir, "[256 x")
	// Non-wasm_import externs use Promise's generic C-ABI: sret return +
	// boxed (i8*) scalar params, not raw i32 — see wasmWebUnboxI32Param's
	// doc comment in sched_wasm_web_delivery.go.
	if !strings.Contains(ir, "define void @promise_web_subscribe(i8*") && !strings.Contains(ir, "define void @promise_web_subscribe(ptr") {
		t.Errorf("expected @promise_web_subscribe defined with sret ABI\ngot:\n%s", ir)
	}
	if !strings.Contains(ir, "define void @promise_web_unsubscribe(i8*") && !strings.Contains(ir, "define void @promise_web_unsubscribe(ptr") {
		t.Errorf("expected @promise_web_unsubscribe defined with a boxed i32 param\ngot:\n%s", ir)
	}
	if !strings.Contains(ir, "define void @promise_web_subscription_dropped(i8*") && !strings.Contains(ir, "define void @promise_web_subscription_dropped(ptr") {
		t.Errorf("expected @promise_web_subscription_dropped defined with sret ABI\ngot:\n%s", ir)
	}
}

// None of the delivery machinery exists on wasm32-wasi (§18 non-goal).
func TestWasmWebDeliveryAbsentOnWasi(t *testing.T) {
	ir := generateIRForTarget(t, trivialWasmWebProgram, "wasm32-wasi")
	assertNotContains(t, ir, "promise_web_subscriptions")
	assertNotContains(t, ir, wasmweb.SubscribeFunc)
	assertNotContains(t, ir, wasmweb.ExportEnqueue)
}

// promise_web_enqueue is exported with the documented signature (§14):
// (sub_id: i32, handle: i32) -> i32.
func TestWebEnqueueExportSignature(t *testing.T) {
	ir := generateIRForTarget(t, trivialWasmWebProgram, "wasm32-web")
	assertContains(t, ir, "define i32 @promise_web_enqueue(i32")
	enqueueFn := extractDefine(ir, wasmweb.ExportEnqueue)
	if enqueueFn == "" {
		t.Fatalf("expected @promise_web_enqueue to be defined\ngot:\n%s", ir)
	}
	if !strings.Contains(enqueueFn, "i32") {
		t.Errorf("expected promise_web_enqueue's second param to be i32 (handle)\ngot:\n%s", enqueueFn)
	}
}

// An unknown, ended, or stale sub_id never traps — enqueue must be safe to
// call with any untrusted integer JS supplies. The ID is masked into the
// table before indexing, accepted only when it is positive and equals the
// slot's stored ID, and a rejected event's own handle goes back to the host
// to release. The behaviour is driven end to end under node by
// TestWebDeliveryQueueSemantics (cmd/promise).
func TestWebEnqueueRejectsUnknownIDs(t *testing.T) {
	ir := generateIRForTarget(t, trivialWasmWebProgram, "wasm32-web")
	enqueueFn := extractDefine(ir, wasmweb.ExportEnqueue)
	if enqueueFn == "" {
		t.Fatalf("expected @promise_web_enqueue to be defined\ngot:\n%s", ir)
	}
	for _, want := range []string{
		fmt.Sprintf("and i32 %%sub_id, %d", wasmWebMaxSubscriptions-1),
		"icmp sgt i32 %sub_id, 0",
		"icmp eq i32",
	} {
		if !strings.Contains(enqueueFn, want) {
			t.Errorf("expected promise_web_enqueue to check sub_id with %q before touching the channel\ngot:\n%s", want, enqueueFn)
		}
	}
	if !strings.Contains(enqueueFn, "ret i32 %handle") {
		t.Errorf("expected a rejection path returning the arriving handle\ngot:\n%s", enqueueFn)
	}
	// A terminated instance takes nothing more. Today the flag is set only once
	// no subscription is live, so no run-time test can reach this check with a
	// live ID.
	if !strings.Contains(enqueueFn, "@"+wasmweb.GlobalTerminated) {
		t.Errorf("expected promise_web_enqueue to reject once the instance has terminated\ngot:\n%s", enqueueFn)
	}
	if !strings.Contains(enqueueFn, fmt.Sprintf("ret i32 %d", wasmweb.EnqueueNoRelease)) {
		t.Errorf("expected an EnqueueNoRelease return path\ngot:\n%s", enqueueFn)
	}
}

// The generation a subscription ID carries wraps from the last value back to
// 1, never to 0, so no ID is ever 0 (a free slot) or negative. Reaching the
// wrap at run time takes 2^23 subscriptions, so the select that skips 0 is
// checked here.
func TestWebSubscribeGenerationSkipsZeroOnWrap(t *testing.T) {
	ir := generateIRForTarget(t, trivialWasmWebProgram, "wasm32-web")
	assertContains(t, ir, "@promise_web_subscription_generation = global i32 1")
	subscribeFn := extractDefine(ir, wasmweb.SubscribeFunc)
	if subscribeFn == "" {
		t.Fatalf("expected @promise_web_subscribe to be defined\ngot:\n%s", ir)
	}
	// llir spells a large constant in hex; take its spelling from llir itself.
	limit := constant.NewInt(irtypes.I32, wasmWebSubGenerationLimit).Ident()
	wrap := regexp.MustCompile(`(%\S+) = icmp eq i32 %\S+, ` + regexp.QuoteMeta(limit)).FindStringSubmatch(subscribeFn)
	if wrap == nil {
		t.Fatalf("expected the next generation to be compared with %d\ngot:\n%s", wasmWebSubGenerationLimit, subscribeFn)
	}
	if !strings.Contains(subscribeFn, "select i1 "+wrap[1]+", i32 1, i32 ") {
		t.Errorf("expected the wrapped generation to restart at 1\ngot:\n%s", subscribeFn)
	}
	if !regexp.MustCompile(`shl i32 %\S+, ` + fmt.Sprint(wasmWebSubSlotBits)).MatchString(subscribeFn) {
		t.Errorf("expected the ID to shift the generation above the slot bits\ngot:\n%s", subscribeFn)
	}
}

// The three overflow policies (§8.1) all appear as switch targets keyed off
// the subscription's stored policy field.
func TestWebEnqueueDispatchesAllThreePolicies(t *testing.T) {
	ir := generateIRForTarget(t, trivialWasmWebProgram, "wasm32-web")
	enqueueFn := extractDefine(ir, wasmweb.ExportEnqueue)
	if enqueueFn == "" {
		t.Fatalf("expected @promise_web_enqueue to be defined\ngot:\n%s", ir)
	}
	if !strings.Contains(enqueueFn, "switch i32") {
		t.Errorf("expected promise_web_enqueue to switch on the policy field\ngot:\n%s", enqueueFn)
	}
	for _, label := range []string{"drop_oldest", "drop_newest", "coalesce"} {
		if !strings.Contains(enqueueFn, label) {
			t.Errorf("expected a %q block in promise_web_enqueue\ngot:\n%s", label, enqueueFn)
		}
	}
	// Every overflow path must bump the drop counter (§8.1: "Drops are
	// counted per subscription and readable").
	if !strings.Contains(enqueueFn, "add i32") {
		t.Errorf("expected the dropped counter to be incremented on overflow\ngot:\n%s", enqueueFn)
	}
}

// Ring indices wrap via unsigned remainder, not unconditional increment —
// the whole point of a ring buffer. Channel head/tail/capacity are i64
// (channelStructType), so this arithmetic happens at i64, not i32.
func TestWebEnqueueWrapsRingIndices(t *testing.T) {
	ir := generateIRForTarget(t, trivialWasmWebProgram, "wasm32-web")
	enqueueFn := extractDefine(ir, wasmweb.ExportEnqueue)
	if enqueueFn == "" {
		t.Fatalf("expected @promise_web_enqueue to be defined\ngot:\n%s", ir)
	}
	if !strings.Contains(enqueueFn, "urem i64") {
		t.Errorf("expected ring index arithmetic to wrap via urem i64 (channel head/tail/capacity)\ngot:\n%s", enqueueFn)
	}
}

// promise_web_enqueue writes directly into the subscription's channel and
// wakes a parked receiver the same way channel send already does — via
// promise_waiter_wake_one on the channel's own recv-waiters list — rather
// than any invented wait/wake primitive.
func TestWebEnqueueWakesChannelRecvWaiters(t *testing.T) {
	ir := generateIRForTarget(t, trivialWasmWebProgram, "wasm32-web")
	enqueueFn := extractDefine(ir, wasmweb.ExportEnqueue)
	if enqueueFn == "" {
		t.Fatalf("expected @promise_web_enqueue to be defined\ngot:\n%s", ir)
	}
	if !strings.Contains(enqueueFn, "@promise_waiter_wake_one") {
		t.Errorf("expected promise_web_enqueue to wake parked receivers via promise_waiter_wake_one\ngot:\n%s", enqueueFn)
	}
	if !strings.Contains(enqueueFn, "@pal_mutex_lock") || !strings.Contains(enqueueFn, "@pal_mutex_unlock") {
		t.Errorf("expected promise_web_enqueue to lock/unlock the channel's own mutex\ngot:\n%s", enqueueFn)
	}
}

// promise_web_subscribe allocates the subscription's backing channel via the
// existing promise_channel_new (the same allocator every Channel[T](cap)
// literal uses) — the only allocation anywhere in the delivery path, and it
// must not be inside promise_web_enqueue (checked separately by
// TestWebEnqueueNeverAllocates below).
func TestWebSubscribeAllocatesChannel(t *testing.T) {
	ir := generateIRForTarget(t, trivialWasmWebProgram, "wasm32-web")
	subscribeFn := extractDefine(ir, wasmweb.SubscribeFunc)
	if subscribeFn == "" {
		t.Fatalf("expected @promise_web_subscribe to be defined\ngot:\n%s", ir)
	}
	if !strings.Contains(subscribeFn, "@promise_channel_new(") {
		t.Errorf("expected promise_web_subscribe to call promise_channel_new\ngot:\n%s", subscribeFn)
	}
	// Increments live registrations (§4.1) — a subscription makes the
	// program a reactor.
	if !strings.Contains(subscribeFn, "@promise_web_live_registrations") {
		t.Errorf("expected promise_web_subscribe to touch promise_web_live_registrations\ngot:\n%s", subscribeFn)
	}
}

// promise_web_channel hands back a subscription's channel pointer for
// web.pr to wrap as a Channel[i32] value (container types are raw i8*
// internally — see extern.go's isOpaqueContainerType handling).
func TestWebChannelFuncExists(t *testing.T) {
	ir := generateIRForTarget(t, trivialWasmWebProgram, "wasm32-web")
	// Container return types (Channel[i32]) are never sret, but the sub_id
	// param is still boxed — see TestWasmWebSubscriptionTableExists.
	if !strings.Contains(ir, "define i8* @promise_web_channel(i8*") && !strings.Contains(ir, "define ptr @promise_web_channel(ptr") {
		t.Errorf("expected @promise_web_channel(boxed i32) to be defined returning a raw pointer\ngot:\n%s", ir)
	}
}

// promise_web_enqueue must never call pal_alloc or promise_channel_new — it
// has to be safe from any context, including a context with no Promise stack
// underneath it at all (§14).
func TestWebEnqueueNeverAllocates(t *testing.T) {
	ir := generateIRForTarget(t, trivialWasmWebProgram, "wasm32-web")
	enqueueFn := extractDefine(ir, wasmweb.ExportEnqueue)
	if enqueueFn == "" {
		t.Fatalf("expected @promise_web_enqueue to be defined\ngot:\n%s", ir)
	}
	if strings.Contains(enqueueFn, "pal_alloc") || strings.Contains(enqueueFn, "promise_channel_new") {
		t.Errorf("promise_web_enqueue must never allocate (§14)\ngot:\n%s", enqueueFn)
	}
}

// promise_web_unsubscribe marks the slot free and decrements live
// registrations — the mirror image of subscribe, restoring §4.1's
// ordinary-command behaviour once the last subscription closes (§13). It
// does not free the channel itself (§12 — the Promise-side Channel[i32]
// value owns that; web.pr calls unsubscribe before that value drops).
func TestWebUnsubscribeClearsSlotAndDecrementsLiveness(t *testing.T) {
	ir := generateIRForTarget(t, trivialWasmWebProgram, "wasm32-web")
	unsubscribeFn := extractDefine(ir, wasmweb.UnsubscribeFunc)
	if unsubscribeFn == "" {
		t.Fatalf("expected @promise_web_unsubscribe to be defined\ngot:\n%s", ir)
	}
	if strings.Contains(unsubscribeFn, "pal_free") {
		t.Errorf("promise_web_unsubscribe must not free the channel itself (§12 — the Promise-side value owns it)\ngot:\n%s", unsubscribeFn)
	}
	if !strings.Contains(unsubscribeFn, "@promise_web_live_registrations") {
		t.Errorf("expected promise_web_unsubscribe to touch promise_web_live_registrations\ngot:\n%s", unsubscribeFn)
	}
}

// The registry in internal/wasmweb is the contract sema enforces on every
// `extern naming a delivery primitive (T2234). It is sound only if a
// declaration of exactly the registered signature calls codegen's definition in
// the definition's own shape: declare and call every registered symbol on
// wasm32-web, then require each call's return and argument types to equal the
// definition's.
func TestWasmWebRegisteredSignaturesMatchDefinitions(t *testing.T) {
	args := map[string]string{"i32": "0i32"}
	var decls, calls strings.Builder
	for i, s := range wasmweb.Signatures() {
		fmt.Fprintf(&decls, "_w%d%s `extern(%q);\n", i, s, s.Sym)
		var callArgs []string
		for _, p := range s.Params {
			a, ok := args[p.Type]
			if !ok {
				t.Fatalf("%s: no test argument for parameter type %s — add one", s.Sym, p.Type)
			}
			callArgs = append(callArgs, a)
		}
		fmt.Fprintf(&calls, "  _w%d(%s);\n", i, strings.Join(callArgs, ", "))
	}
	ir := generateIRForTarget(t, decls.String()+"main() {\n"+calls.String()+"}\n", "wasm32-web")

	// shape is the return type and parameter types of one define or call.
	shape := func(ret, list string) string {
		var types []string
		for _, p := range strings.Split(list, ",") {
			if f := strings.Fields(p); len(f) > 0 {
				types = append(types, f[0])
			}
		}
		return ret + " (" + strings.Join(types, ", ") + ")"
	}
	for _, s := range wasmweb.Signatures() {
		sym := regexp.QuoteMeta(s.Sym)
		def := regexp.MustCompile(`(?m)^define (?:\w+ )*?(\S+) @` + sym + `\(([^)]*)\)`).FindStringSubmatch(ir)
		if def == nil {
			t.Errorf("%s is registered but not defined on wasm32-web", s.Sym)
			continue
		}
		want := shape(def[1], def[2])
		uses := regexp.MustCompile(`call (\S+) @`+sym+`\(([^)]*)\)`).FindAllStringSubmatch(ir, -1)
		if len(uses) == 0 {
			t.Errorf("%s: no call emitted for the registered declaration", s.Sym)
		}
		for _, u := range uses {
			if got := shape(u[1], u[2]); got != want {
				t.Errorf("%s: a declaration of %s calls it as %s, but it is defined as %s", s.Sym, s, got, want)
			}
		}
	}
}
