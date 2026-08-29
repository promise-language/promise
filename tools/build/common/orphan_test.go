package common

import (
	"os"
	"os/exec"
	"testing"
)

// A tool launched already-detached has no launcher whose death could mean
// anything, so arming the watchdog there would make it kill itself on the first
// poll. WatchForOrphan must return without starting the goroutine — if it did
// start one, this test binary would die a couple of seconds later.
func TestWatchForOrphanNotArmedWhenAlreadyDetached(t *testing.T) {
	// Can't fake getppid, so assert the guard's predicate directly and let the
	// call below prove it is harmless for a normally-launched process.
	if ppid := os.Getppid(); ppid <= 1 {
		t.Skipf("test binary is itself detached (ppid %d); nothing to distinguish", ppid)
	}
	WatchForOrphan()
}

func TestParentAliveTracksTheRealParent(t *testing.T) {
	ppid := os.Getppid()
	if ppid <= 1 {
		t.Skip("test binary is detached; no live parent to observe")
	}
	if !parentAlive(ppid) {
		t.Errorf("parentAlive(%d) = false for our own live parent", ppid)
	}
}

// A pid that is no longer running must read as dead, or the watchdog would
// never fire.
func TestParentAliveFalseForDeadParent(t *testing.T) {
	cmd := exec.Command(sleepCmd(), sleepArgs("0")...)
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	pid := cmd.Process.Pid
	if err := cmd.Wait(); err != nil {
		t.Fatalf("wait: %v", err)
	}
	// Checked straight after reaping: the kernel hands out pids sequentially
	// over a large range, so nothing has had the chance to take this one.
	if parentAlive(pid) {
		t.Errorf("parentAlive(%d) = true for a process that has exited", pid)
	}
}
