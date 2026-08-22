package codegen

import (
	"fmt"
	"regexp"
	"strings"
	"testing"
)

// The park that arm() hands off to (genNetpollWait, T0232), covered here for
// T1563.
//
// arm() publishes the deadline under pd.lock and then releases it; the park
// re-takes pd.lock to publish the waiter. That gap is the race the whole
// deadline design is written around — the scan deliberately leaves an expired
// deadline armed when it finds no waiter — so the shape of the park is part of
// the deadline contract rather than an unrelated detail.
//
// The park also holds one half of a mirror. arm(), cancel() and the deadline
// scan select a direction with a runtime index into pd.deadline/pd.reason; the
// park picks its waiter slot with a compile-time constant. Nothing but these
// tests keeps the two halves agreeing, and a mismatch would hand a write
// timeout to a reader with no compile error anywhere.

// netParkSrc parks in both directions from main (a coroutine, so the real
// goroutine-parking path) and from a plain function (not a coroutine, so the
// thread-blocking fallback — which is what modules/net actually compiles to,
// since read(), write() and _park_ready() are all ordinary functions).
const netParkSrc = "_open(int fd) int `extern(\"promise_net_netpoll_open\");\n" +
	"_close(int pd) `extern(\"promise_net_netpoll_close\");\n" +
	"_arm(int pd, int dir, int deadline) `extern(\"promise_net_netpoll_arm\");\n" +
	"_wait_read(int pd) `extern(\"promise_netpoll_wait_read\");\n" +
	"_wait_write(int pd) `extern(\"promise_netpoll_wait_write\");\n" +
	"_park_off_coroutine(int pd) {\n" +
	"  _wait_read(pd);\n" +
	"}\n" +
	"main() {\n" +
	"  int pd = _open(3);\n" +
	"  _arm(pd, 0, 123);\n" +
	"  _wait_read(pd);\n" +
	"  _arm(pd, 1, 123);\n" +
	"  _wait_write(pd);\n" +
	"  _park_off_coroutine(pd);\n" +
	"  _close(pd);\n" +
	"}\n"

// onlyPollDescFieldReg returns the single SSA register addressing the PollDesc
// field at idx, failing if there is not exactly one.
func onlyPollDescFieldReg(t *testing.T, body string, idx int, what string) string {
	t.Helper()
	regs := pollDescFieldRegs(body, fmt.Sprintf("i32 0, i32 %d", idx))
	if len(regs) != 1 {
		t.Fatalf("%s: expected exactly one access to PollDesc field %d, got %d", what, idx, len(regs))
	}
	return regs[0]
}

// A park in direction d must publish its waiter in the slot that the wake path
// for direction d clears: read parks into read_g, write parks into write_g.
func TestT1563_ParkPublishesTheWaiterInTheArmedDirection(t *testing.T) {
	ir := generateIRForTarget(t, netParkSrc, "x86_64-unknown-linux-gnu")
	body := findDefinedFunc(ir, "@.goroutine.main(")
	if body == "" {
		t.Fatalf("expected the main coroutine to be defined")
	}

	// The park is emitted inline, so each slot is touched exactly once in main.
	readReg := onlyPollDescFieldReg(t, body, pdFieldReadG, "read park")
	writeReg := onlyPollDescFieldReg(t, body, pdFieldWriteG, "write park")

	// A goroutine park stores the real G. That is what tells the reactor — and
	// the deadline scan — to enqueue it rather than signal the cond var.
	for _, slot := range []struct {
		reg  string
		what string
	}{{readReg, "read_g"}, {writeReg, "write_g"}} {
		re := regexp.MustCompile(`(%\w+) = load i8\*, i8\*\* @__promise_current_g\n\s*store i8\* (%\w+), i8\*\* ` +
			regexp.QuoteMeta(slot.reg) + `\n`)
		m := re.FindStringSubmatch(body)
		if m == nil {
			t.Errorf("%s is not published with the current G:\n%s", slot.what, body)
			continue
		}
		if m[1] != m[2] {
			t.Errorf("%s stores %s, want the loaded current G %s", slot.what, m[2], m[1])
		}
	}

	// Source order is arm-then-park in each direction, and the read park comes
	// before the write one. A park emitted ahead of its arm would publish the
	// waiter before the deadline exists, and the scan would never fire it.
	armIdx := strings.Index(body, "@promise_net_netpoll_arm")
	if armIdx < 0 {
		t.Fatalf("expected an arm call before the park:\n%s", body)
	}
	readIdx := strings.Index(body, readReg+" = getelementptr")
	writeIdx := strings.Index(body, writeReg+" = getelementptr")
	if armIdx > readIdx {
		t.Errorf("the read park is emitted before the deadline is armed")
	}
	if readIdx > writeIdx {
		t.Errorf("the read and write parks are emitted out of order")
	}
}

// The park hands pd.lock to whoever wakes it: it stores the lock in
// G.park_mutex and suspends while still holding it, and the scheduler releases
// it once the suspend has completed. Unlocking here instead would reopen the
// wake-before-suspend race the deadline scan relies on being closed — a scan
// that found the waiter would wake a goroutine that had not parked yet.
func TestT1563_ParkHoldsThePollDescLockAcrossTheSuspend(t *testing.T) {
	ir := generateIRForTarget(t, netParkSrc, "x86_64-unknown-linux-gnu")
	body := findDefinedFunc(ir, "@.goroutine.main(")
	if body == "" {
		t.Fatalf("expected the main coroutine to be defined")
	}

	// G.park_mutex is set to the same value the park passed to pal_mutex_lock.
	re := regexp.MustCompile(`call void @pal_mutex_lock\(i8\* (%\w+)\)\n(?:[^\n]*\n)*?\s*%\w+ = getelementptr ` +
		regexp.QuoteMeta(goroutineStructType().String()) +
		fmt.Sprintf(`, [^\n]*, i32 0, i32 %d\n\s*store i8\* (%%\w+), `, gFieldParkMutex))
	matches := re.FindAllStringSubmatch(body, -1)
	if len(matches) != 2 {
		t.Fatalf("expected both parks to publish pd.lock as G.park_mutex, got %d:\n%s", len(matches), body)
	}
	for i, m := range matches {
		if m[1] != m[2] {
			t.Errorf("park %d sets G.park_mutex to %s, want the PollDesc lock %s", i, m[2], m[1])
		}
	}

	// Nothing releases pd.lock between publishing the waiter and suspending.
	readReg := onlyPollDescFieldReg(t, body, pdFieldReadG, "read park")
	start := strings.Index(body, ", i8** "+readReg+"\n")
	if start < 0 {
		t.Fatalf("read waiter store not found:\n%s", body)
	}
	suspend := strings.Index(body[start:], "@llvm.coro.suspend")
	if suspend < 0 {
		t.Fatalf("no coro.suspend after the read waiter store:\n%s", body)
	}
	if strings.Contains(body[start:start+suspend], "@pal_mutex_unlock") {
		t.Errorf("the park releases pd.lock before suspending:\n%s", body[start:start+suspend])
	}
}

// Off a coroutine — which is where modules/net actually parks — the park blocks
// the thread on the PollDesc condition variable and marks the slot with a
// sentinel instead of a G. That sentinel is what makes the deadline scan signal
// the cond var rather than enqueue address 0x1 on the run queue, so every
// timed-out read() depends on this path and the scan's matching branch agreeing
// on it.
func TestT1563_ThreadBlockingParkWaitsOnThePollDescCondVar(t *testing.T) {
	ir := generateIRForTarget(t, netParkSrc, "x86_64-unknown-linux-gnu")
	body := findDefinedFunc(ir, "@__user._park_off_coroutine(")
	if body == "" {
		t.Fatalf("expected the off-coroutine park function to be defined")
	}
	if strings.Contains(body, "@llvm.coro.suspend") {
		t.Errorf("an ordinary function must not park with coro.suspend:\n%s", body)
	}

	slot := onlyPollDescFieldReg(t, body, pdFieldReadG, "read park")
	sentinel := fmt.Sprintf("inttoptr i64 %d to i8*", netpollCondWaiterSentinel)
	re := regexp.MustCompile(`(%\w+) = ` + regexp.QuoteMeta(sentinel) + `\n\s*store i8\* (%\w+), i8\*\* ` +
		regexp.QuoteMeta(slot) + `\n`)
	m := re.FindStringSubmatch(body)
	if m == nil {
		t.Fatalf("the thread-blocking park must mark the slot with the cond-waiter sentinel:\n%s", body)
	}
	if m[1] != m[2] {
		t.Errorf("waiter slot stores %s, want the sentinel %s", m[2], m[1])
	}

	// It waits on pd.cond holding pd.lock, then clears the slot and unlocks.
	// The wait is wrapped in the T1685 syscall handoff: this park blocks the OS
	// thread, so it must hand this M's P off before blocking or num_cpus
	// concurrent socket reads wedge the whole scheduler (the T1636 symptom).
	// netpoll is the sixth emitBlockingCondWait site and the only one not
	// covered by TestBlockingWaitsHandOffPPerSite, so the handoff calls are
	// asserted as part of this park's shape rather than merely tolerated.
	lockReg := onlyPollDescFieldReg(t, body, pdFieldLock, "pd.lock")
	condReg := onlyPollDescFieldReg(t, body, pdFieldCond, "pd.cond")
	waitRe := regexp.MustCompile(`(%\w+) = load i8\*, i8\*\* ` + regexp.QuoteMeta(condReg) +
		`\n\s*call void @promise_sched_enter_syscall\(\)` +
		`\n\s*call void @pal_cond_wait\(i8\* (%\w+), i8\* (%\w+)\)` +
		`\n\s*call void @promise_sched_exit_syscall\(\)` +
		`\n\s*store i8\* null, i8\*\* ` + regexp.QuoteMeta(slot) +
		`\n\s*call void @pal_mutex_unlock\(i8\* (%\w+)\)\n`)
	w := waitRe.FindStringSubmatch(body)
	if w == nil {
		t.Fatalf("expected cond_wait on pd.cond followed by clearing the slot and unlocking:\n%s", body)
	}
	if w[1] != w[2] {
		t.Errorf("cond_wait waits on %s, want pd.cond %s", w[2], w[1])
	}
	lockLoad := regexp.MustCompile(`(%\w+) = load i8\*, i8\*\* ` + regexp.QuoteMeta(lockReg)).FindStringSubmatch(body)
	if lockLoad == nil {
		t.Fatalf("pd.lock is never loaded:\n%s", body)
	}
	if w[3] != lockLoad[1] || w[4] != lockLoad[1] {
		t.Errorf("cond_wait/unlock use %s/%s, want pd.lock %s", w[3], w[4], lockLoad[1])
	}
}
