//go:build !windows

package common

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// The two Ctrl+C paths differ in more than the signal they send: the first one
// ends the step in flight and lets the tool reach its next checkpoint, so it
// must NOT latch the shutdown — the tool still has a summary to print and, on a
// `verify`, a lock to release. Assert both halves, and assert the child died of
// the signal we sent rather than of anything else.
func TestInterruptChildrenSignalsChildrenWithoutLatchingShutdown(t *testing.T) {
	before := trackedCount()

	// Safe with an empty registry — it runs on a signal, whenever one arrives.
	interruptChildren()

	cmd := exec.Command(sleepCmd(), sleepArgs("60")...)
	if err := startTracked(cmd); err != nil {
		t.Fatalf("startTracked: %v", err)
	}
	pid := cmd.Process.Pid
	done := make(chan error, 1)
	go func() { done <- waitTracked(cmd) }()

	interruptChildren()

	var err error
	select {
	case err = <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("interruptChildren did not stop the running child")
	}
	assertSignaled(t, err, syscall.SIGINT)

	if trackedHas(pid) {
		t.Errorf("pid %d still registered after it was interrupted", pid)
	}
	// Not a teardown: the next step must still be able to spawn.
	next := exec.Command(sleepCmd(), sleepArgs("60")...)
	if err := startTracked(next); err != nil {
		t.Fatalf("startTracked after interruptChildren: %v", err)
	}
	killTree(next.Process.Pid)
	_ = waitTracked(next)

	if after := trackedCount(); after != before {
		t.Errorf("registry leaked: %d entries before, %d after", before, after)
	}
}

// End-to-end over the real handler: a subprocess is in its own process group
// (isolateChild), so the terminal's Ctrl+C never reaches it — the forwarding in
// init() is the only thing that does. Deliver an actual SIGINT to this process
// and assert the child dies of it.
//
// Only the first Ctrl+C is exercised here: the second one calls os.Exit(130),
// which would take this test binary with it, so it is guarded against below and
// covered out-of-process by TestSecondInterruptKillsTheTreeAndExits.
func TestSigintHandlerForwardsToChildren(t *testing.T) {
	if Interrupted() {
		t.Fatal("already interrupted: a second SIGINT would exit(130) and end the run")
	}
	t.Cleanup(func() { interrupted.Store(0) })

	cmd := exec.Command(sleepCmd(), sleepArgs("60")...)
	if err := startTracked(cmd); err != nil {
		t.Fatalf("startTracked: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- waitTracked(cmd) }()

	if err := syscall.Kill(os.Getpid(), syscall.SIGINT); err != nil {
		t.Fatalf("raising SIGINT: %v", err)
	}

	var err error
	select {
	case err = <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("SIGINT was not forwarded to the running child")
	}
	assertSignaled(t, err, syscall.SIGINT)

	// The child's death is ordered after the handler's Swap, so this is a read
	// of settled state, not a poll.
	if !Interrupted() {
		t.Error("Interrupted() is false after a SIGINT that reached the children")
	}
}

// assertSignaled reports whether a finished command died of sig, so that a
// child killed by something else (a stray SIGKILL, an exit of its own) cannot
// be read as the signal under test having been delivered.
func assertSignaled(t *testing.T, err error, sig syscall.Signal) {
	t.Helper()

	var exit *exec.ExitError
	if !errors.As(err, &exit) {
		t.Fatalf("child exited with %v, want death by %v", err, sig)
	}
	status, ok := exit.Sys().(syscall.WaitStatus)
	if !ok {
		t.Fatalf("wait status is %T, want syscall.WaitStatus", exit.Sys())
	}
	if !status.Signaled() || status.Signal() != sig {
		t.Errorf("child exited %v, want signaled with %v", exit, sig)
	}
}

// interruptHelperEnv names the mode a helper subprocess runs in; empty means
// "this is the ordinary test binary, skip".
const interruptHelperEnv = "PROMISE_TEST_INTERRUPT_HELPER"

// TestInterruptChildHelperProcess is not a test — it is the subprocess that
// TestInterruptToolHelperProcess tracks, standing in for the `go test` a real
// tool would be waiting on. It ignores SIGINT, so it survives the first Ctrl+C
// and only the SIGKILL sweep of the second one can end it; that is what makes
// the two paths distinguishable from outside.
func TestInterruptChildHelperProcess(t *testing.T) {
	if os.Getenv(interruptHelperEnv) != "child" {
		t.Skip("helper process; driven by TestSecondInterruptKillsTheTreeAndExits")
	}

	signal.Ignore(os.Interrupt)
	// Printed after the disposition is in place, so a reader of this line knows
	// a SIGINT sent from here on cannot kill this process. Announcing it before
	// would leave the interrupt racing the setup — which is the whole failure
	// this test exists to detect, so it must not be able to happen here.
	fmt.Println("READY")

	time.Sleep(60 * time.Second)
	t.Fatal("still running: nothing tore this process down")
}

// TestInterruptToolHelperProcess is not a test — it is the tool process that
// TestSecondInterruptKillsTheTreeAndExits drives. The second Ctrl+C ends in
// os.Exit(130), which cannot be observed in-process.
func TestInterruptToolHelperProcess(t *testing.T) {
	if os.Getenv(interruptHelperEnv) != "tool" {
		t.Skip("helper process; driven by TestSecondInterruptKillsTheTreeAndExits")
	}

	cmd := exec.Command(os.Args[0], "-test.run=^TestInterruptChildHelperProcess$", "-test.v")
	cmd.Env = append(os.Environ(), interruptHelperEnv+"=child")
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := startTracked(cmd); err != nil {
		t.Fatalf("startTracked: %v", err)
	}
	// Read the child's readiness before anything waits on it: Wait closes this
	// pipe, so the read has to complete first.
	awaitHelperLine(t, bufio.NewScanner(out), "READY")
	go func() { _ = waitTracked(cmd) }()
	fmt.Printf("CHILD %d\n", cmd.Process.Pid)

	// Report the first Ctrl+C back before the driver sends the second. The
	// handler's channel holds one signal, so a second delivered before the
	// first is consumed would be dropped and the teardown would never run;
	// Interrupted() flips after that receive, which makes it the edge to wait
	// on. The loop only reads state the handler has already published.
	deadline := time.Now().Add(30 * time.Second)
	for !Interrupted() && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}
	if !Interrupted() {
		t.Fatal("first SIGINT never reached the handler")
	}
	fmt.Println("INTERRUPTED")

	// The second SIGINT lands here and never returns. Outliving it is the
	// failure, and the driver reports it by exit code.
	time.Sleep(60 * time.Second)
	t.Fatal("still running: the second SIGINT did not tear the tool down")
}

// The invariant this whole file serves — "a tool takes its subprocesses with
// it" (docs/build-tools.md §Process Lifetime) — is only observable from
// outside, because the path ends in os.Exit. Drive a real tool process through
// both Ctrl+Cs and assert what a user would see: it exits 130, and the tree it
// had running is gone.
func TestSecondInterruptKillsTheTreeAndExits(t *testing.T) {
	helper := exec.Command(os.Args[0], "-test.run=^TestInterruptToolHelperProcess$", "-test.v")
	helper.Env = append(os.Environ(), interruptHelperEnv+"=tool")
	out, err := helper.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := helper.Start(); err != nil {
		t.Fatalf("starting helper: %v", err)
	}
	exited := make(chan error, 1)
	go func() { exited <- helper.Wait() }()
	defer func() {
		// A helper still running here failed the run; don't leave it, or the
		// child it is tracking, behind for whatever comes next.
		_ = helper.Process.Kill()
	}()

	lines := bufio.NewScanner(out)
	child := helperPID(t, lines, "CHILD ")
	if !processAlive(child) {
		t.Fatalf("helper's child %d is not running", child)
	}
	// If the sweep under test does not happen, the child outlives this test —
	// exactly the orphan the assertions below report. Don't hand it on to
	// whatever runs next.
	t.Cleanup(func() { killTree(child) })

	// First Ctrl+C: the step ends, the tool does not.
	signalHelper(t, helper, syscall.SIGINT)
	awaitHelperLine(t, lines, "INTERRUPTED")
	select {
	case err := <-exited:
		t.Fatalf("helper exited on the first SIGINT (%v); it must reach its next checkpoint", err)
	default:
	}
	if !processAlive(child) {
		t.Fatal("the first SIGINT killed a child that ignores it; the sweep under test is no longer what kills it")
	}

	// Second Ctrl+C: the user has asked twice.
	signalHelper(t, helper, syscall.SIGINT)
	select {
	case err := <-exited:
		var exit *exec.ExitError
		if !errors.As(err, &exit) {
			t.Fatalf("helper exited with %v, want status 130", err)
		}
		if got := exit.ExitCode(); got != 130 {
			t.Errorf("helper exit code = %d, want 130 (128 + SIGINT)", got)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("helper did not exit on the second SIGINT")
	}

	// The child ignores SIGINT, so its death is attributable to the teardown
	// sweep and to nothing else.
	if !waitGone(child, 10*time.Second) {
		t.Errorf("child %d survived the tool it was started by", child)
	}
}

// helperPID reads the line beginning with prefix and parses the pid after it.
func helperPID(t *testing.T, lines *bufio.Scanner, prefix string) int {
	t.Helper()

	pid, err := strconv.Atoi(awaitHelperLine(t, lines, prefix))
	if err != nil {
		t.Fatalf("parsing %q line: %v", prefix, err)
	}
	return pid
}

// awaitHelperLine returns the remainder of the first helper line starting with
// prefix, skipping the test framework's own output. A helper that dies without
// printing it ends the read, which is a failure rather than a wait.
func awaitHelperLine(t *testing.T, lines *bufio.Scanner, prefix string) string {
	t.Helper()

	for lines.Scan() {
		if rest, ok := strings.CutPrefix(lines.Text(), prefix); ok {
			return strings.TrimSpace(rest)
		}
	}
	if err := lines.Err(); err != nil {
		t.Fatalf("reading helper output: %v", err)
	}
	t.Fatalf("helper output ended before %q", prefix)
	return ""
}

func signalHelper(t *testing.T, helper *exec.Cmd, sig syscall.Signal) {
	t.Helper()

	if err := helper.Process.Signal(sig); err != nil {
		t.Fatalf("signalling helper with %v: %v", sig, err)
	}
}
