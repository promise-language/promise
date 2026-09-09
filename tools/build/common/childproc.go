package common

import (
	"errors"
	"os/exec"
	"sync"
)

// A build tool is a supervisor: nearly all of its wall time is spent inside
// `go test`, `go build` or `bin/promise test`, and each of those spawns a tree
// of its own. Nothing in the OS ties that tree's lifetime to ours, so when the
// tool is killed outright — a harness step timeout, `kill -9` on a run that
// looked stuck — the tree keeps running with nobody reading its output. On a
// host where several worktrees share one machine that is not merely untidy:
// verify is serialized on a host-global lock, so an orphaned run holds the lock
// for its full ~40min while live runs starve behind it (T1450), and its test
// fan-out keeps competing for RAM (T1817).
//
// So every long-running subprocess is started in its own process group and
// recorded here, and the tool takes the whole tree down with it: on Ctrl+C, and
// when it notices its own launcher has died (see orphan.go).
var (
	childMu  sync.Mutex
	children = map[int]*exec.Cmd{}
	// killing latches the decision to tear the tree down. It is read and
	// written under childMu, the same lock a spawn holds, which is what makes
	// the registry a complete picture of what is running: a start either
	// finishes registering before the sweep reads it, or finds the latch and
	// never spawns at all. Never cleared — every caller exits the process
	// immediately afterwards, so there is no "after" in which to start one.
	killing bool
)

// errStopping is what a spawn reports once the teardown has been latched. The
// tool is on its way out, so a subprocess started now would have nobody to
// report to and nobody to kill it.
var errStopping = errors.New("build tool is shutting down")

// runTracked starts cmd in its own process group, waits for it, and keeps it in
// the registry for as long as it runs. Every helper in exec.go that spawns a
// long-running command goes through this; the short probes (`git rev-parse`,
// `xcrun --show-sdk-path`) do not, since they cannot outlive us meaningfully.
func runTracked(cmd *exec.Cmd) error {
	if err := startTracked(cmd); err != nil {
		return err
	}
	return waitTracked(cmd)
}

// startTracked spawns cmd and registers it, or reports errStopping without
// spawning anything at all if the tree is already being torn down.
//
// The spawn happens *under* childMu on purpose. Registering after the start
// instead leaves a window in which the child exists but the registry cannot see
// it, and every KillChildren caller exits the process on the line after the
// sweep — so a child caught in that window is not merely killed late, it is
// never killed at all, and survives as exactly the orphan this file exists to
// prevent (T1961). Holding the lock across the fork closes the window from both
// ends: the sweep waits for an in-flight spawn to register, and a spawn that
// arrives after the sweep sees the latch. The cost is that KillChildren can
// block for the length of one fork/exec, which is the right trade against a
// 40-minute orphan holding the host-global verify lock.
//
// Splitting the spawn from the wait also gives a caller a point at which the
// child is known to be reachable by KillChildren — the tests need that
// ordering, and observing it from outside would mean racing the very
// bookkeeping they are asserting on.
func startTracked(cmd *exec.Cmd) error {
	isolateChild(cmd)

	childMu.Lock()
	defer childMu.Unlock()
	if killing {
		return errStopping
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	children[cmd.Process.Pid] = cmd
	return nil
}

// waitTracked waits for a command that startTracked returned nil for, and
// unregisters it once it has exited.
func waitTracked(cmd *exec.Cmd) error {
	pid := cmd.Process.Pid
	err := cmd.Wait()

	childMu.Lock()
	delete(children, pid)
	childMu.Unlock()
	return err
}

// trackedPIDs snapshots the registry so that callers signal off the lock: a
// signal delivery is a syscall, and killTree on Windows spawns a taskkill of
// its own, neither of which should hold up a concurrent spawn.
func trackedPIDs() []int {
	childMu.Lock()
	defer childMu.Unlock()

	pids := make([]int, 0, len(children))
	for pid := range children {
		pids = append(pids, pid)
	}
	return pids
}

// KillChildren terminates every subprocess this tool started and still has
// running, along with everything those subprocesses spawned. Safe to call more
// than once, and safe to call when nothing is running. A subprocess that is
// starting concurrently is covered too: latching first means it either lands in
// the registry swept below, or is refused before it spawns.
func KillChildren() {
	childMu.Lock()
	killing = true
	childMu.Unlock()

	for _, pid := range trackedPIDs() {
		killTree(pid)
	}
}
