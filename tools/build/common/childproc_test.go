package common

import (
	"os/exec"
	"runtime"
	"testing"
	"time"
)

func TestRunTrackedRegistersAndUnregisters(t *testing.T) {
	before := trackedCount()

	started := make(chan int, 1)
	done := make(chan error, 1)
	cmd := exec.Command(sleepCmd(), sleepArgs("2")...)
	go func() {
		// Report the pid as soon as the process exists, so the assertion below
		// races against the process running, not against it starting.
		go func() {
			for range 200 {
				if cmd.Process != nil {
					started <- cmd.Process.Pid
					return
				}
				time.Sleep(5 * time.Millisecond)
			}
			started <- 0
		}()
		done <- runTracked(cmd)
	}()

	pid := <-started
	if pid == 0 {
		t.Fatal("command never started")
	}
	if !trackedHas(pid) {
		t.Errorf("pid %d not registered while running", pid)
	}

	if err := <-done; err != nil {
		t.Fatalf("runTracked: %v", err)
	}
	if trackedHas(pid) {
		t.Errorf("pid %d still registered after it exited", pid)
	}
	if after := trackedCount(); after != before {
		t.Errorf("registry leaked: %d entries before, %d after", before, after)
	}
}

// The reason children get their own process group is so that KillChildren can
// reach what they spawned — a `go test` whose package binaries would otherwise
// survive it. Assert on the grandchild, not just the child.
func TestKillChildrenKillsTheWholeTree(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("tree kill on Windows goes through taskkill; covered by T1450")
	}

	// The child prints its grandchild's pid, then waits on it. Killing only the
	// child would leave the grandchild running.
	cmd := exec.Command("sh", "-c", "sleep 60 & echo $! ; wait")
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- runTracked(cmd) }()

	var grandchild int
	if _, err := fscanPid(out, &grandchild); err != nil {
		t.Fatalf("reading grandchild pid: %v", err)
	}
	if !processAlive(grandchild) {
		t.Fatalf("grandchild %d not running", grandchild)
	}

	KillChildren()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("KillChildren did not stop the child")
	}
	if waitGone(grandchild, 10*time.Second) {
		return
	}
	t.Errorf("grandchild %d survived KillChildren", grandchild)
}

func TestKillChildrenWithNothingRunning(t *testing.T) {
	// Must not panic or block — it runs on the interrupt path, where the
	// registry may well be empty.
	KillChildren()
	KillChildren()
}
