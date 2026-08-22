package codegen

import (
	"fmt"
	"regexp"
	"strings"
	"testing"

	antlr "github.com/antlr4-go/antlr/v4"
	"github.com/promise-language/promise/compiler/internal/ast"
	"github.com/promise-language/promise/compiler/internal/parser"
	"github.com/promise-language/promise/compiler/internal/sema"
	"github.com/promise-language/promise/compiler/internal/types"
)

// T1563 — socket deadlines, timeouts and cancellation.
//
// The reactor gained a deadline registry and a scan that expires it. These
// tests lock the IR shape that makes the feature work: that the scan exists,
// that the poller thread runs it on BOTH the event and the no-event path (a
// silent socket produces no events at all, so the no-event path is the only one
// a pure timeout can fire on), that it runs while the batch lock is held, and
// that none of it leaks into a WASM build, where there is no reactor.

// netDeadlineSrc declares just enough net-module externs to make
// defineNetPALBodies fire, which is what emits the reactor.
const netDeadlineSrc = "_open(int fd) int `extern(\"promise_net_netpoll_open\");\n" +
	"_close(int pd) `extern(\"promise_net_netpoll_close\");\n" +
	"_arm(int pd, int dir, int deadline) `extern(\"promise_net_netpoll_arm\");\n" +
	"_reason(int pd, int dir) int `extern(\"promise_net_netpoll_wake_reason\");\n" +
	"_cancelled(int pd) int `extern(\"promise_net_netpoll_cancelled\");\n" +
	"_cancel(int pd) `extern(\"promise_net_netpoll_cancel\");\n" +
	"_ref(int pd) `extern(\"promise_net_netpoll_ref\");\n" +
	"_unref(int pd) `extern(\"promise_net_netpoll_unref\");\n" +
	"main() {\n" +
	"  int pd = _open(3);\n" +
	"  _arm(pd, 0, 123);\n" +
	"  int _r = _reason(pd, 0);\n" +
	"  int _c = _cancelled(pd);\n" +
	"  _cancel(pd);\n" +
	"  _ref(pd);\n" +
	"  _unref(pd);\n" +
	"  _close(pd);\n" +
	"}\n"

func TestT1563_DeadlineRuntimeIsEmitted(t *testing.T) {
	ir := generateIRForTarget(t, netDeadlineSrc, "x86_64-unknown-linux-gnu")

	for _, fn := range []string{
		"@promise_netpoll_scan_deadlines(",
		"@promise_netpoll_arm(",
		"@promise_netpoll_wake_reason(",
		"@promise_netpoll_cancelled(",
		"@promise_netpoll_cancel(",
		"@promise_netpoll_ref(",
		"@promise_netpoll_unref(",
	} {
		if findDefinedFunc(ir, fn) == "" {
			t.Errorf("expected %s to be defined", fn)
		}
	}
	// The registry itself.
	assertContains(t, ir, "@__netpoll_registry_lock = global i8* null")
	assertContains(t, ir, "@__netpoll_deadline_head = global i8* null")
}

// The scan must run on both reactor paths. Skipping the no-event path would make
// a deadline on an otherwise silent socket never fire.
func TestT1563_ReactorLoopScansOnBothPaths(t *testing.T) {
	ir := generateIRForTarget(t, netDeadlineSrc, "x86_64-unknown-linux-gnu")
	body := findDefinedFunc(ir, "@promise_netpoll_loop(")
	if body == "" {
		t.Fatalf("expected promise_netpoll_loop to be defined")
	}
	if n := strings.Count(body, "call void @promise_netpoll_scan_deadlines()"); n != 2 {
		t.Errorf("expected the reactor loop to scan deadlines on both the event and no-event paths (2 calls), got %d:\n%s", n, body)
	}
	for _, blk := range []string{"no_events:", "event_done:"} {
		start := strings.Index(body, "\n"+blk)
		if start < 0 {
			t.Fatalf("block %s not found in reactor loop:\n%s", blk, body)
		}
		rest := body[start:]
		scan := strings.Index(rest, "@promise_netpoll_scan_deadlines")
		unlock := strings.Index(rest, "@pal_mutex_unlock")
		if scan < 0 {
			t.Errorf("block %s does not scan deadlines", blk)
			continue
		}
		// Lock order is batch -> registry -> pd. The scan takes the registry
		// lock, so it has to happen before the batch lock is released.
		if unlock >= 0 && unlock < scan {
			t.Errorf("block %s releases the batch lock before scanning deadlines", blk)
		}
	}
}

// arm() runs on every single park, so the global registry lock must stay off the
// hot path: it is only taken when the park actually carries a deadline. The
// deadline and wake reason themselves are published under pd.lock, which the
// park was taking anyway.
func TestT1563_ArmTakesRegistryOnlyWhenADeadlineIsSet(t *testing.T) {
	ir := generateIRForTarget(t, netDeadlineSrc, "x86_64-unknown-linux-gnu")
	body := findDefinedFunc(ir, "@promise_netpoll_arm(")
	if body == "" {
		t.Fatalf("expected promise_netpoll_arm to be defined")
	}
	split := strings.Index(body, "\n.link_path:")
	if split < 0 {
		t.Fatalf("expected an arm .link_path block:\n%s", body)
	}
	entry, linkPath := body[:split], body[split:]

	if strings.Contains(entry, "@__netpoll_registry_lock") {
		t.Errorf("a park with no deadline must not touch the global registry lock:\n%s", entry)
	}
	if !strings.Contains(linkPath, "@__netpoll_registry_lock") {
		t.Errorf("arm must take the registry lock to link a deadline:\n%s", linkPath)
	}
	if !strings.Contains(linkPath, "@__netpoll_deadline_head") {
		t.Errorf("arm must push onto the deadline registry:\n%s", linkPath)
	}
	// pd.lock is taken and released around the deadline + reason store.
	if n := strings.Count(entry, "call void @pal_mutex_lock"); n != 1 {
		t.Errorf("arm entry must take exactly pd.lock, got %d:\n%s", n, entry)
	}
	if n := strings.Count(entry, "call void @pal_mutex_unlock"); n != 1 {
		t.Errorf("arm entry must release pd.lock before branching, got %d unlocks:\n%s", n, entry)
	}
}

// close() no longer frees the PollDesc directly — it releases the socket
// reference, so a CancelHandle can outlive the stream without a use-after-free.
func TestT1563_CloseReleasesReferenceInsteadOfFreeing(t *testing.T) {
	ir := generateIRForTarget(t, netDeadlineSrc, "x86_64-unknown-linux-gnu")
	body := findDefinedFunc(ir, "@promise_netpoll_close(")
	if body == "" {
		t.Fatalf("expected promise_netpoll_close to be defined")
	}
	assertContains(t, body, "call void @promise_netpoll_unref(")
	if strings.Contains(body, "call void @pal_free") {
		t.Errorf("close must not free the PollDesc directly — unref owns that:\n%s", body)
	}

	unref := findDefinedFunc(ir, "@promise_netpoll_unref(")
	if unref == "" {
		t.Fatalf("expected promise_netpoll_unref to be defined")
	}
	assertContains(t, unref, "call void @pal_mutex_destroy")
	assertContains(t, unref, "call void @pal_cond_destroy")
	assertContains(t, unref, "call void @pal_free")
}

// Cancellation is sticky and links the PollDesc onto the registry, so the scan
// can retry the wake for a goroutine that parks just after the cancel lands.
func TestT1563_CancelIsStickyAndLinksRegistry(t *testing.T) {
	ir := generateIRForTarget(t, netDeadlineSrc, "x86_64-unknown-linux-gnu")
	body := findDefinedFunc(ir, "@promise_netpoll_cancel(")
	if body == "" {
		t.Fatalf("expected promise_netpoll_cancel to be defined")
	}
	assertContains(t, body, "@__netpoll_registry_lock")
	assertContains(t, body, "@__netpoll_deadline_head")
	// Both waiter slots are woken with the cancelled reason (2).
	if n := strings.Count(body, "store i32 2, i32* "); n != 2 {
		t.Errorf("cancel must record the cancelled wake reason for both directions, got %d:\n%s", n, body)
	}
}

// WASM has no reactor at all, so none of the deadline machinery may be emitted.
func TestT1563_NoDeadlineRuntimeOnWasm(t *testing.T) {
	ir := generateIRForTarget(t, netDeadlineSrc, "wasm32-wasi")
	for _, fn := range []string{
		"promise_netpoll_scan_deadlines",
		"promise_netpoll_arm",
		"promise_netpoll_cancel",
		"promise_netpoll_ref",
		"promise_netpoll_unref",
		"__netpoll_registry_lock",
		"__netpoll_deadline_head",
	} {
		if strings.Contains(ir, "@"+fn+" ") || strings.Contains(ir, "@"+fn+"(") {
			t.Errorf("WASM builds must not emit %s", fn)
		}
	}
	// The net-module externs themselves stay as bodyless declarations.
	if findDefinedFunc(ir, "@promise_net_netpoll_arm(") != "" {
		t.Errorf("WASM must leave promise_net_netpoll_arm as a bodyless declaration")
	}
}

// Both reactor mutexes are pal_alloc'd by netpoll_init, so both have to be
// destroyed at exit. They cannot be torn down inside promise_sched_shutdown —
// that function is emitted before the compiler knows the net module is imported
// — so the entry point calls promise_netpoll_shutdown right after it.
func TestT1563_ReactorLocksAreDestroyedAtShutdown(t *testing.T) {
	ir := generateIRForTarget(t, netDeadlineSrc, "x86_64-unknown-linux-gnu")

	body := findDefinedFunc(ir, "@promise_netpoll_shutdown(")
	if body == "" {
		t.Fatalf("expected promise_netpoll_shutdown to be defined")
	}
	for _, g := range []string{"@__netpoll_batch_lock", "@__netpoll_registry_lock"} {
		if !strings.Contains(body, g) {
			t.Errorf("promise_netpoll_shutdown must release %s:\n%s", g, body)
		}
	}
	if n := strings.Count(body, "call void @pal_mutex_destroy"); n != 2 {
		t.Errorf("expected both reactor locks destroyed, got %d pal_mutex_destroy calls:\n%s", n, body)
	}

	// And it must actually be reached, after the scheduler has joined the
	// reactor thread.
	main := findDefinedFunc(ir, "@main(")
	if main == "" {
		t.Fatalf("expected main to be defined")
	}
	sched := strings.Index(main, "call void @promise_sched_shutdown()")
	netpoll := strings.Index(main, "call void @promise_netpoll_shutdown()")
	if sched < 0 || netpoll < 0 {
		t.Fatalf("expected main to call both shutdown functions:\n%s", main)
	}
	if netpoll < sched {
		t.Errorf("the reactor locks must be destroyed after the reactor thread is joined:\n%s", main)
	}
}

// netpollBlock returns the instructions of the named basic block inside body
// (label excluded, up to the blank line that separates blocks), or "" when the
// block is not there.
func netpollBlock(body, label string) string {
	start := strings.Index(body, "\n"+label+":\n")
	if start < 0 {
		return ""
	}
	rest := body[start+len(label)+3:]
	if end := strings.Index(rest, "\n\n"); end >= 0 {
		return rest[:end]
	}
	return rest
}

// --- PollDesc layout, reference counting and lock order ---
//
// The tests above lock the reactor-level wiring. The ones below lock the
// PollDesc state that wiring reads and writes: every deadline/cancellation
// field is addressed by a hard-coded index, so a field reordering would
// silently make arm() write into refcount. They also pin the lock order the
// design comment in netpoll_deadline.go declares (BATCH -> REGISTRY -> pd.lock),
// which is the only thing keeping the reactor from deadlocking against a
// goroutine arming a deadline.

// pollDescFieldRegs returns every SSA register in body holding a pointer to the
// PollDesc field addressed by path (e.g. "i32 0, i32 10", or
// "i32 0, i32 6, i32 1" for one slot of a per-direction array).
func pollDescFieldRegs(body, path string) []string {
	re := regexp.MustCompile(`(%\w+) = getelementptr ` +
		regexp.QuoteMeta(pollDescStructType().String()) + `, [^\n]*, ` +
		regexp.QuoteMeta(path) + `\n`)
	var regs []string
	for _, m := range re.FindAllStringSubmatch(body, -1) {
		regs = append(regs, m[1])
	}
	return regs
}

// assertPollDescFieldStore fails unless body stores exactly want (the value
// operand, e.g. "i32 1") into the PollDesc field addressed by path.
func assertPollDescFieldStore(t *testing.T, body, path, want, what string) {
	t.Helper()
	regs := pollDescFieldRegs(body, path)
	if len(regs) == 0 {
		t.Errorf("%s: no PollDesc field access at %q", what, path)
		return
	}
	for _, reg := range regs {
		re := regexp.MustCompile(`store ([^\n]*?), [^\n]*\* ` + regexp.QuoteMeta(reg) + `\n`)
		m := re.FindStringSubmatch(body)
		if m == nil {
			continue
		}
		if m[1] == want {
			return
		}
		t.Errorf("%s: field %q is stored %q, want %q", what, path, m[1], want)
		return
	}
	t.Errorf("%s: field %q is never stored (want %q)", what, path, want)
}

// A field reordering would repoint every hard-coded index in netpoll.go and
// netpoll_deadline.go at the wrong slot, with no compile error anywhere.
func TestT1563_PollDescFieldIndicesMatchTheStruct(t *testing.T) {
	pd := pollDescStructType()
	want := []struct {
		idx  int
		typ  string
		name string
	}{
		{pdFieldFd, "i32", "fd"},
		{pdFieldReadG, "i8*", "read_g"},
		{pdFieldWriteG, "i8*", "write_g"},
		{pdFieldLock, "i8*", "lock"},
		{pdFieldCond, "i8*", "cond"},
		{pdFieldDeadline, "[2 x i64]", "deadline"},
		{pdFieldReason, "[2 x i32]", "reason"},
		{pdFieldDlNext, "i8*", "dl_next"},
		{pdFieldLinked, "i32", "linked"},
		{pdFieldRefcount, "i32", "refcount"},
		{pdFieldCancelled, "i32", "cancelled"},
	}
	if len(pd.Fields) != 12 {
		t.Fatalf("PollDesc has %d fields, want 12", len(pd.Fields))
	}
	for _, w := range want {
		if got := pd.Fields[w.idx].String(); got != w.typ {
			t.Errorf("PollDesc field %d (%s) is %s, want %s", w.idx, w.name, got, w.typ)
		}
	}
	// The direction constants index the two-element arrays, and the reason
	// codes are mirrored by _wake_ready/_wake_timeout/_wake_cancelled in
	// modules/net/net.pr — net_test.pr asserts the Promise side of the mirror.
	if netpollDirRead != 0 || netpollDirWrite != 1 {
		t.Errorf("park directions must be 0/1, got read=%d write=%d", netpollDirRead, netpollDirWrite)
	}
	if netpollReasonReady != 0 || netpollReasonTimeout != 1 || netpollReasonCancelled != 2 {
		t.Errorf("wake reasons must be 0/1/2, got ready=%d timeout=%d cancelled=%d",
			netpollReasonReady, netpollReasonTimeout, netpollReasonCancelled)
	}
}

// A PollDesc starts owned by its socket alone, uncancelled, off the registry and
// with no deadline in either direction. Anything else and the very first scan
// would either time out a fresh socket or walk a garbage next pointer.
func TestT1563_OpenInitializesDeadlineState(t *testing.T) {
	ir := generateIRForTarget(t, netDeadlineSrc, "x86_64-unknown-linux-gnu")
	body := findDefinedFunc(ir, "@promise_netpoll_open(")
	if body == "" {
		t.Fatalf("expected promise_netpoll_open to be defined")
	}

	for _, dir := range []int{netpollDirRead, netpollDirWrite} {
		assertPollDescFieldStore(t, body,
			fmt.Sprintf("i32 0, i32 %d, i32 %d", pdFieldDeadline, dir), "i64 0", "open")
		assertPollDescFieldStore(t, body,
			fmt.Sprintf("i32 0, i32 %d, i32 %d", pdFieldReason, dir), "i32 0", "open")
	}
	assertPollDescFieldStore(t, body, fmt.Sprintf("i32 0, i32 %d", pdFieldDlNext), "i8* null", "open")
	assertPollDescFieldStore(t, body, fmt.Sprintf("i32 0, i32 %d", pdFieldLinked), "i32 0", "open")
	assertPollDescFieldStore(t, body, fmt.Sprintf("i32 0, i32 %d", pdFieldRefcount), "i32 1", "open")
	assertPollDescFieldStore(t, body, fmt.Sprintf("i32 0, i32 %d", pdFieldCancelled), "i32 0", "open")

	// The allocation has to cover the fields it then writes.
	if !strings.Contains(body, "call i8* @pal_alloc(i64 88)") {
		t.Errorf("expected an 88-byte PollDesc allocation (12-field layout):\n%s", body)
	}
}

// close() wakes both slots with the cancelled reason and clears both deadlines
// before dropping the socket reference. Leaving a deadline armed on a closed
// PollDesc that a CancelHandle keeps alive would make the scan walk it forever.
func TestT1563_CloseCancelsWaitersAndClearsDeadlines(t *testing.T) {
	ir := generateIRForTarget(t, netDeadlineSrc, "x86_64-unknown-linux-gnu")
	body := findDefinedFunc(ir, "@promise_netpoll_close(")
	if body == "" {
		t.Fatalf("expected promise_netpoll_close to be defined")
	}

	for _, dir := range []int{netpollDirRead, netpollDirWrite} {
		assertPollDescFieldStore(t, body,
			fmt.Sprintf("i32 0, i32 %d, i32 %d", pdFieldReason, dir), "i32 2", "close")
		assertPollDescFieldStore(t, body,
			fmt.Sprintf("i32 0, i32 %d, i32 %d", pdFieldDeadline, dir), "i64 0", "close")
	}
	// The deadlines are cleared under pd.lock, and the reference is only
	// dropped after the in-flight event batch has drained (B0324).
	clear := strings.LastIndex(body, "store i64 0,")
	unlockPd := strings.Index(body[clear:], "call void @pal_mutex_unlock")
	batch := strings.Index(body, "@__netpoll_batch_lock")
	unref := strings.Index(body, "call void @promise_netpoll_unref(")
	if clear < 0 || unlockPd < 0 || batch < 0 || unref < 0 {
		t.Fatalf("close is missing one of clear/unlock/batch/unref:\n%s", body)
	}
	if !(clear < batch && batch < unref) {
		t.Errorf("close must clear deadlines, drain the event batch, then unref:\n%s", body)
	}
	if clear+unlockPd > batch {
		t.Errorf("close must release pd.lock before waiting on the batch lock:\n%s", body)
	}
}

// A CancelHandle can outlive its socket, so the PollDesc is only destroyed when
// the last reference goes. Freeing on the first unref is a use-after-free.
func TestT1563_UnrefFreesOnlyOnTheLastReference(t *testing.T) {
	ir := generateIRForTarget(t, netDeadlineSrc, "x86_64-unknown-linux-gnu")
	body := findDefinedFunc(ir, "@promise_netpoll_unref(")
	if body == "" {
		t.Fatalf("expected promise_netpoll_unref to be defined")
	}

	if !strings.Contains(body, "sub i32 ") {
		t.Errorf("unref must decrement the refcount:\n%s", body)
	}
	if !strings.Contains(body, "icmp sgt i32 ") {
		t.Errorf("unref must branch on the post-decrement count:\n%s", body)
	}

	alive := netpollBlock(body, ".alive")
	if alive == "" {
		t.Fatalf("expected an unref .alive block:\n%s", body)
	}
	for _, forbidden := range []string{"@pal_free", "@pal_mutex_destroy", "@pal_cond_destroy"} {
		if strings.Contains(alive, forbidden) {
			t.Errorf("unref must not call %s while references remain:\n%s", forbidden, alive)
		}
	}
	if n := strings.Count(alive, "@pal_mutex_unlock"); n != 2 {
		t.Errorf("the .alive path must release both pd.lock and the registry lock, got %d unlocks:\n%s", n, alive)
	}

	destroy := netpollBlock(body, ".destroy")
	if destroy == "" {
		t.Fatalf("expected an unref .destroy block:\n%s", body)
	}
	for _, want := range []string{"@pal_mutex_destroy", "@pal_cond_destroy", "@pal_free"} {
		if !strings.Contains(destroy, want) {
			t.Errorf("the last unref must call %s:\n%s", want, destroy)
		}
	}
}

// The last unref has to take the PollDesc off the deadline registry before
// freeing it, or the next scan dereferences freed memory.
func TestT1563_UnrefUnlinksFromTheRegistryBeforeFreeing(t *testing.T) {
	ir := generateIRForTarget(t, netDeadlineSrc, "x86_64-unknown-linux-gnu")
	body := findDefinedFunc(ir, "@promise_netpoll_unref(")
	if body == "" {
		t.Fatalf("expected promise_netpoll_unref to be defined")
	}
	for _, blk := range []string{".unlink_init:", ".unlink_loop:", ".unlink_check:", ".unlink_remove:", ".unlink_next:"} {
		if !strings.Contains(body, "\n"+blk) {
			t.Errorf("unref is missing the %s block — a freed PollDesc would stay on the registry:\n%s", blk, body)
		}
	}
	if !strings.Contains(netpollBlock(body, ".unlink_init"), "@__netpoll_deadline_head") {
		t.Errorf("the unlink walk must start at the registry head:\n%s", body)
	}
	remove := netpollBlock(body, ".unlink_remove")
	if !strings.Contains(remove, "store i32 0,") {
		t.Errorf("unlinking must clear pd.linked:\n%s", remove)
	}
	// Walking the whole list, not just the head, is what makes unlinking a
	// mid-list PollDesc work — several sockets carry deadlines at once.
	if !strings.Contains(netpollBlock(body, ".unlink_next"), fmt.Sprintf("i32 0, i32 %d", pdFieldDlNext)) {
		t.Errorf("the unlink walk must advance through dl_next:\n%s", body)
	}
}

// Only entry points that hold both locks decide the order, and every one of them
// has to agree: registry first, then pd.lock. The reactor's scan runs under the
// batch lock, which sits above both.
func TestT1563_RegistryLockIsTakenBeforePollDescLock(t *testing.T) {
	ir := generateIRForTarget(t, netDeadlineSrc, "x86_64-unknown-linux-gnu")
	for _, fn := range []string{
		"@promise_netpoll_cancel(",
		"@promise_netpoll_ref(",
		"@promise_netpoll_unref(",
		"@promise_netpoll_scan_deadlines(",
	} {
		body := findDefinedFunc(ir, fn)
		if body == "" {
			t.Fatalf("expected %s to be defined", fn)
		}
		registry := strings.Index(body, "@__netpoll_registry_lock")
		pdLock := strings.Index(body, fmt.Sprintf("i32 0, i32 %d\n", pdFieldLock))
		if registry < 0 {
			t.Errorf("%s never takes the registry lock:\n%s", fn, body)
			continue
		}
		if pdLock >= 0 && pdLock < registry {
			t.Errorf("%s takes pd.lock before the registry lock — ABBA deadlock with arm():\n%s", fn, body)
		}
	}
}

// Reading the wake reason and the cancelled flag happens on the park path, so
// they take pd.lock only — never the process-wide registry lock.
func TestT1563_ReadersTakeOnlyThePollDescLock(t *testing.T) {
	ir := generateIRForTarget(t, netDeadlineSrc, "x86_64-unknown-linux-gnu")
	for _, fn := range []string{"@promise_netpoll_wake_reason(", "@promise_netpoll_cancelled("} {
		body := findDefinedFunc(ir, fn)
		if body == "" {
			t.Fatalf("expected %s to be defined", fn)
		}
		if strings.Contains(body, "@__netpoll_registry_lock") {
			t.Errorf("%s must stay off the global registry lock:\n%s", fn, body)
		}
		if n := strings.Count(body, "call void @pal_mutex_lock"); n != 1 {
			t.Errorf("%s must take pd.lock exactly once, got %d:\n%s", fn, n, body)
		}
		if n := strings.Count(body, "call void @pal_mutex_unlock"); n != 1 {
			t.Errorf("%s must release pd.lock exactly once, got %d:\n%s", fn, n, body)
		}
	}
}

// The scan is the timer. It must read a monotonic clock, expire both directions
// with the timeout reason, wake cancelled waiters with the cancelled reason, and
// drop PollDescs that carry neither so the list stays short.
func TestT1563_ScanExpiresBothDirectionsAndUnlinksIdlePollDescs(t *testing.T) {
	ir := generateIRForTarget(t, netDeadlineSrc, "x86_64-unknown-linux-gnu")
	body := findDefinedFunc(ir, "@promise_netpoll_scan_deadlines(")
	if body == "" {
		t.Fatalf("expected promise_netpoll_scan_deadlines to be defined")
	}

	// One clock reading for the whole pass — re-reading per node would let a
	// long list expire inconsistently.
	if n := strings.Count(body, "@.promise_nanotime_raw()"); n != 1 {
		t.Errorf("the scan must read a monotonic clock exactly once per pass, got %d:\n%s", n, body)
	}

	for _, blk := range []string{
		".scan_read_cancel.wake", ".scan_write_cancel.wake",
		".scan_read_timeout.wake", ".scan_write_timeout.wake",
	} {
		if !strings.Contains(body, "\n"+blk+":") {
			t.Errorf("the scan is missing the %s block:\n%s", blk, body)
		}
	}
	// Cancellation is processed before expiry, so a socket that is both
	// cancelled and past its deadline reports cancelled: the cancel pass clears
	// the waiter slot and the timeout pass then finds nothing to wake.
	if a, b := strings.Index(body, ".scan_read_cancel.wake:"), strings.Index(body, ".scan_read_timeout.wake:"); a > b {
		t.Errorf("the scan must run the cancellation pass before the timeout pass:\n%s", body)
	}
	if got := strings.Count(body, "store i32 2, i32* "); got != 2 {
		t.Errorf("the scan must record the cancelled reason for both directions, got %d:\n%s", got, body)
	}
	if got := strings.Count(body, "store i32 1, i32* "); got != 2 {
		t.Errorf("the scan must record the timeout reason for both directions, got %d:\n%s", got, body)
	}
	// A deadline only expires when it is armed AND due — dl != 0 && dl <= now.
	if n := strings.Count(body, "icmp sle i64 "); n != 2 {
		t.Errorf("the scan must compare both deadlines against now, got %d:\n%s", n, body)
	}

	keep, unlink := netpollBlock(body, ".keep"), netpollBlock(body, ".unlink")
	if keep == "" || unlink == "" {
		t.Fatalf("expected the scan to have both .keep and .unlink blocks:\n%s", body)
	}
	if !strings.Contains(unlink, "store i32 0,") {
		t.Errorf("unlinking must clear pd.linked so a later arm() can relink:\n%s", unlink)
	}
	for _, blk := range []struct{ name, text string }{{".keep", keep}, {".unlink", unlink}} {
		if !strings.Contains(blk.text, "call void @pal_mutex_unlock") {
			t.Errorf("the scan must release pd.lock on the %s path:\n%s", blk.name, blk.text)
		}
	}
}

// arm() clears a deadline by publishing 0, and does so without the registry
// lock. That only works because the scan unlinks a PollDesc whose slots are both
// clear — otherwise a cleared deadline would keep the node on the list forever.
func TestT1563_ArmClearsTheDeadlineAndResetsTheReason(t *testing.T) {
	ir := generateIRForTarget(t, netDeadlineSrc, "x86_64-unknown-linux-gnu")
	body := findDefinedFunc(ir, "@promise_netpoll_arm(")
	if body == "" {
		t.Fatalf("expected promise_netpoll_arm to be defined")
	}
	// The deadline and reason slots are selected with the dir parameter, so one
	// emission serves read and write.
	if !strings.Contains(body, fmt.Sprintf("i32 0, i32 %d, i32 %%dir\n", pdFieldDeadline)) {
		t.Errorf("arm must select the deadline slot with the dir parameter:\n%s", body)
	}
	assertPollDescFieldStore(t, body, fmt.Sprintf("i32 0, i32 %d, i32 %%dir", pdFieldReason), "i32 0", "arm")
	if !strings.Contains(body, "store i64 %deadline,") {
		t.Errorf("arm must publish the caller's deadline verbatim (0 = none):\n%s", body)
	}
}

// The Promise-visible externs are thin bridges: unwrap the boxed int, call the
// runtime entry point. A mis-wired bridge would cancel the wrong PollDesc.
func TestT1563_NetExternBridgesCallTheirRuntimeEntryPoints(t *testing.T) {
	ir := generateIRForTarget(t, netDeadlineSrc, "x86_64-unknown-linux-gnu")
	for _, bridge := range []struct{ extern, target string }{
		{"@promise_net_netpoll_arm(", "call void @promise_netpoll_arm("},
		{"@promise_net_netpoll_wake_reason(", "call i32 @promise_netpoll_wake_reason("},
		{"@promise_net_netpoll_cancelled(", "call i32 @promise_netpoll_cancelled("},
		{"@promise_net_netpoll_cancel(", "call void @promise_netpoll_cancel("},
		{"@promise_net_netpoll_ref(", "call void @promise_netpoll_ref("},
		{"@promise_net_netpoll_unref(", "call void @promise_netpoll_unref("},
	} {
		body := findDefinedFunc(ir, bridge.extern)
		if body == "" {
			t.Errorf("expected a bridge body for %s", bridge.extern)
			continue
		}
		if !strings.Contains(body, bridge.target) {
			t.Errorf("%s must call %q:\n%s", bridge.extern, bridge.target, body)
		}
	}
	// wake_reason and cancelled return an int through the sret parameter, so
	// the i32 result has to be widened, not reinterpreted.
	for _, fn := range []string{"@promise_net_netpoll_wake_reason(", "@promise_net_netpoll_cancelled("} {
		body := findDefinedFunc(ir, fn)
		if !strings.Contains(body, "sext i32 ") {
			t.Errorf("%s must sign-extend its i32 result into the Promise int:\n%s", fn, body)
		}
	}
}

// The module IR text is hashed into the build cache key, so anything driving
// emission order has to be deterministic — the bridge bodies are emitted from an
// ordered slice rather than a map for exactly this reason.
func TestT1563_NetBridgeEmissionIsDeterministic(t *testing.T) {
	first := generateIRForTarget(t, netDeadlineSrc, "x86_64-unknown-linux-gnu")
	for i := 0; i < 3; i++ {
		if next := generateIRForTarget(t, netDeadlineSrc, "x86_64-unknown-linux-gnu"); next != first {
			t.Fatalf("IR generation is not deterministic — the build cache key would flap between builds (run %d)", i+2)
		}
	}
	// And the bridges keep their declared order within the module.
	prev := -1
	for _, fn := range []string{
		"@promise_net_netpoll_cancel(",
		"@promise_net_netpoll_ref(",
		"@promise_net_netpoll_unref(",
	} {
		at := strings.Index(first, "define void "+fn)
		if at < 0 {
			t.Fatalf("expected %s to be defined", fn)
		}
		if at < prev {
			t.Errorf("bridge bodies must be emitted in slice order (cancel, ref, unref); %s is out of place", fn)
		}
		prev = at
	}
}

// A program that never imports net must not pay for — or even mention — the
// reactor teardown.
func TestT1563_NoNetpollShutdownWithoutTheNetModule(t *testing.T) {
	ir := generateIRForTarget(t, "main() { print_line(\"hi\"); }\n", "x86_64-unknown-linux-gnu")
	for _, sym := range []string{
		"promise_netpoll_shutdown",
		"promise_netpoll_scan_deadlines",
		"__netpoll_registry_lock",
		"__netpoll_deadline_head",
	} {
		if strings.Contains(ir, sym) {
			t.Errorf("a program that does not import net must not emit %s", sym)
		}
	}
}

// netDeadlineTestSrc is netDeadlineSrc reshaped as a batch test, to exercise the
// second entry point that has to tear the reactor down.
const netDeadlineTestSrc = "_open(int fd) int `extern(\"promise_net_netpoll_open\");\n" +
	"_close(int pd) `extern(\"promise_net_netpoll_close\");\n" +
	"_arm(int pd, int dir, int deadline) `extern(\"promise_net_netpoll_arm\");\n" +
	"_cancel(int pd) `extern(\"promise_net_netpoll_cancel\");\n" +
	"_ref(int pd) `extern(\"promise_net_netpoll_ref\");\n" +
	"_unref(int pd) `extern(\"promise_net_netpoll_unref\");\n" +
	"netTest() `test {\n" +
	"  int pd = _open(3);\n" +
	"  _arm(pd, 0, 123);\n" +
	"  _cancel(pd);\n" +
	"  _ref(pd);\n" +
	"  _unref(pd);\n" +
	"  _close(pd);\n" +
	"}\n"

// Test binaries get their entry point from GenerateTestMain, not
// wrapMainWithScheduler, so the reactor teardown has to be wired into both. A
// miss here leaks the two reactor mutexes out of every test process that
// touches the net module — and since they are allocated before the per-test
// alloc-count reset (B0326), leak enforcement would not catch it.
func TestT1563_TestBinaryAlsoDestroysReactorLocks(t *testing.T) {
	ir := generateTestMainIRForTarget(t, netDeadlineTestSrc, "x86_64-unknown-linux-gnu")

	main := findDefinedFunc(ir, "@main(")
	if main == "" {
		t.Fatalf("expected a test-binary main to be defined")
	}
	sched := strings.Index(main, "call void @promise_sched_shutdown()")
	netpoll := strings.Index(main, "call void @promise_netpoll_shutdown()")
	if sched < 0 {
		t.Fatalf("expected the test main to shut the scheduler down:\n%s", main)
	}
	if netpoll < 0 {
		t.Fatalf("the test main must destroy the reactor locks too:\n%s", main)
	}
	if netpoll < sched {
		t.Errorf("the reactor locks must be destroyed after the reactor thread is joined:\n%s", main)
	}
}

// A test binary that never touches net must not reference the teardown either.
func TestT1563_TestBinaryWithoutNetSkipsReactorTeardown(t *testing.T) {
	ir := generateTestMainIRForTarget(t, "plainTest() `test { int x = 1; }\n", "x86_64-unknown-linux-gnu")
	if strings.Contains(ir, "promise_netpoll_shutdown") {
		t.Errorf("a test binary that does not import net must not emit promise_netpoll_shutdown")
	}
}

// generateTestMainIRForTarget compiles src for target and appends the batch-test
// entry point, mirroring what `promise test` produces.
func generateTestMainIRForTarget(t *testing.T, src, target string) string {
	t.Helper()
	input := antlr.NewInputStream(src)
	lexer := parser.NewPromiseLexer(input)
	lexer.RemoveErrorListeners()
	stream := antlr.NewCommonTokenStream(lexer, antlr.TokenDefaultChannel)
	p := parser.NewPromiseParser(stream)
	p.RemoveErrorListeners()
	file, errs := ast.Build("test.pr", p.CompilationUnit())
	if len(errs) > 0 {
		t.Fatalf("AST build errors: %v", errs)
	}

	stdModInfo, stdScope := getCodegenStdModInfo()
	file.Uses = append([]*ast.UseDecl{{Alias: "_", CatalogName: "std"}}, file.Uses...)

	info, semaErrs := sema.CheckWithTarget(file, map[string]*types.Scope{"std": stdScope}, sema.ParseTargetInfo(target))
	if len(semaErrs) > 0 {
		t.Fatalf("sema errors: %v", semaErrs)
	}
	info.ModuleInfos = map[string]*sema.ModuleInfo{"std": stdModInfo}
	info.ModuleOrder = []string{"std"}

	result := Compile(file, info, target)
	result.GenerateTestMain(info.Tests, nil)
	return result.Module.String()
}
