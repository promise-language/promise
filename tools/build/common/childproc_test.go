package common

import (
	"errors"
	"os/exec"
	"runtime"
	"sync"
	"testing"
	"time"
)

func TestRunTrackedRegistersAndUnregisters(t *testing.T) {
	before := trackedCount()

	cmd := exec.Command(sleepCmd(), sleepArgs("2")...)
	if err := startTracked(cmd); err != nil {
		t.Fatalf("startTracked: %v", err)
	}
	// startTracked has returned, so the child exists and is registered — no
	// polling, and nothing to race against the bookkeeping being asserted on.
	pid := cmd.Process.Pid
	if !trackedHas(pid) {
		t.Errorf("pid %d not registered while running", pid)
	}

	if err := waitTracked(cmd); err != nil {
		t.Fatalf("waitTracked: %v", err)
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
	t.Cleanup(resetKilling)

	// The child prints its grandchild's pid, then waits on it. Killing only the
	// child would leave the grandchild running.
	cmd := exec.Command("sh", "-c", "sleep 60 & echo $! ; wait")
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	// Start on this goroutine: when startTracked returns the child is in the
	// registry, so the KillChildren below cannot run ahead of its registration
	// (T1961). Only the wait needs a goroutine.
	if err := startTracked(cmd); err != nil {
		t.Fatalf("startTracked: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- waitTracked(cmd) }()

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

// A spawn that lands after the sweep already read the registry is the one
// KillChildren cannot see — and every caller exits the process on the next
// line, so such a child is never killed at all and survives as an orphan
// holding the host-global verify lock. The registry state it would observe is
// exactly the one set up here; the answer is that it is not spawned.
func TestStartTrackedRefusesOnceKillChildrenHasRun(t *testing.T) {
	t.Cleanup(resetKilling)
	before := trackedCount()

	KillChildren()

	cmd := exec.Command(sleepCmd(), sleepArgs("60")...)
	err := startTracked(cmd)
	if cmd.Process != nil {
		// Unreachable on a passing run, and the assertion below would report
		// it anyway — but not before this stops a 60s sleeper being left
		// behind for every test that follows.
		killTree(cmd.Process.Pid)
		t.Fatalf("startTracked spawned pid %d after KillChildren", cmd.Process.Pid)
	}
	if !errors.Is(err, errStopping) {
		t.Fatalf("startTracked after KillChildren = %v, want %v", err, errStopping)
	}
	if after := trackedCount(); after != before {
		t.Errorf("refused start still registered: %d entries before, %d after", before, after)
	}
}

func TestKillChildrenWithNothingRunning(t *testing.T) {
	t.Cleanup(resetKilling)

	// Must not panic or block — it runs on the interrupt path, where the
	// registry may well be empty.
	KillChildren()
	KillChildren()
}

// runTracked is the production entry point, so the refusal has to hold there
// too — and it has to be a refusal, not a start whose error is ignored: waiting
// on a command that never spawned dereferences a nil Process.
func TestRunTrackedRefusesOnceKillChildrenHasRun(t *testing.T) {
	t.Cleanup(resetKilling)
	before := trackedCount()

	KillChildren()

	cmd := exec.Command(sleepCmd(), sleepArgs("60")...)
	err := runTracked(cmd)
	if cmd.Process != nil {
		killTree(cmd.Process.Pid)
		t.Fatalf("runTracked spawned pid %d after KillChildren", cmd.Process.Pid)
	}
	if !errors.Is(err, errStopping) {
		t.Fatalf("runTracked after KillChildren = %v, want %v", err, errStopping)
	}
	if after := trackedCount(); after != before {
		t.Errorf("refused start still registered: %d entries before, %d after", before, after)
	}
}

// A command that runs and fails must leave the registry as clean as one that
// succeeded. A stale entry is worse than a leak here: pids are reused, so a
// later KillChildren would SIGKILL the process group of whatever inherited the
// number.
func TestWaitTrackedUnregistersAfterAFailingCommand(t *testing.T) {
	before := trackedCount()

	cmd := exec.Command(exitCmd(), exitArgs(3)...)
	if err := startTracked(cmd); err != nil {
		t.Fatalf("startTracked: %v", err)
	}
	pid := cmd.Process.Pid

	var exit *exec.ExitError
	err := waitTracked(cmd)
	if !errors.As(err, &exit) {
		t.Fatalf("waitTracked = %v, want an ExitError", err)
	}
	if got := exit.ExitCode(); got != 3 {
		t.Errorf("exit code = %d, want 3", got)
	}
	if trackedHas(pid) {
		t.Errorf("pid %d still registered after it failed", pid)
	}
	if after := trackedCount(); after != before {
		t.Errorf("registry leaked: %d entries before, %d after", before, after)
	}
}

// The failure this file exists to prevent is a spawn that lands *while* the
// sweep is running: registering after the fork leaves a window in which the
// child exists and the registry does not know it, and every KillChildren caller
// exits the process on the next line, so a child caught there is never killed
// at all (T1961). Run the two against each other repeatedly. Whatever each
// spawner got back, nothing it started may still be running once the sweep has
// returned — a survivor sleeps for 60s, which the bounded drain below turns
// into a failure instead of a hang.
func TestKillChildrenLeavesNoSpawnRacingTheSweep(t *testing.T) {
	const (
		rounds   = 3
		spawners = 6
	)

	for round := range rounds {
		func() {
			defer resetKilling()
			before := trackedCount()

			// One child that is definitely registered before the sweep, so the
			// round always exercises a real kill and never passes by refusing
			// everything.
			pre := exec.Command(sleepCmd(), sleepArgs("60")...)
			if err := startTracked(pre); err != nil {
				t.Fatalf("round %d: startTracked: %v", round, err)
			}
			preDone := make(chan error, 1)
			go func() { preDone <- waitTracked(pre) }()

			var (
				wg      sync.WaitGroup
				mu      sync.Mutex
				spawned int
				refused int
			)
			race := make(chan struct{})
			for range spawners {
				wg.Add(1)
				go func() {
					defer wg.Done()
					<-race
					cmd := exec.Command(sleepCmd(), sleepArgs("60")...)
					err := startTracked(cmd)
					if errors.Is(err, errStopping) {
						if cmd.Process != nil {
							killTree(cmd.Process.Pid)
							t.Errorf("round %d: refused start still spawned pid %d",
								round, cmd.Process.Pid)
						}
						mu.Lock()
						refused++
						mu.Unlock()
						return
					}
					if err != nil {
						t.Errorf("round %d: startTracked: %v", round, err)
						return
					}
					mu.Lock()
					spawned++
					mu.Unlock()
					_ = waitTracked(cmd)
				}()
			}

			close(race)
			KillChildren()

			drained := make(chan struct{})
			go func() { wg.Wait(); close(drained) }()
			select {
			case <-drained:
			case <-time.After(30 * time.Second):
				t.Fatalf("round %d: a subprocess spawned across KillChildren outlived it", round)
			}
			select {
			case <-preDone:
			case <-time.After(30 * time.Second):
				t.Fatalf("round %d: KillChildren did not stop the child it had registered", round)
			}

			mu.Lock()
			defer mu.Unlock()
			if spawned+refused != spawners {
				t.Errorf("round %d: %d spawned + %d refused, want %d accounted for",
					round, spawned, refused, spawners)
			}
			// Every process is dead and every wait has returned, so the
			// registry must be back where it started — no entry survives to
			// name a pid the OS is free to hand out again.
			if after := trackedCount(); after != before {
				t.Errorf("round %d: registry leaked: %d entries before, %d after", round, before, after)
			}
			// The latch is one-way: once the sweep has run, nothing else starts.
			late := exec.Command(sleepCmd(), sleepArgs("60")...)
			if err := startTracked(late); !errors.Is(err, errStopping) {
				if late.Process != nil {
					killTree(late.Process.Pid)
				}
				t.Errorf("round %d: startTracked after the sweep = %v, want %v", round, err, errStopping)
			}
		}()
	}
}
