package common

import (
	"fmt"
	"os"
	"time"
)

// orphanExitCode is what a tool exits with when its launcher died under it.
// Distinct from 1 (the work ran and failed) and from 75 (the verify lock timed
// out), because nothing was verified either way and the run must not be read as
// a verdict on the tree.
const orphanExitCode = 3

// orphanPollInterval is how often WatchForOrphan re-checks. The check is a
// getppid(2) — cheap enough that this could be far more frequent; 2s is chosen
// so an orphan wastes seconds rather than the ~40min of a full suite.
const orphanPollInterval = 2 * time.Second

// WatchForOrphan exits the tool if the process that launched it dies.
//
// Without this, killing a harness step (or a run that looked stuck) kills the
// wrapper shell and leaves the tool itself running as an orphan — finishing a
// full suite that nobody is reading, holding the host-global verify lock, and
// competing for the machine with whatever replaced it. Orphans accumulate, and
// each one deepens the lock queue for live runs until every subsequent step
// fails waiting (T1450).
//
// A tool launched already-detached (ppid 1 — launchd, systemd, a deliberate
// daemonized run) is not armed: there is no launcher whose death could mean
// anything.
func WatchForOrphan() {
	startPPID := os.Getppid()
	if startPPID <= 1 {
		return
	}
	go func() {
		for {
			time.Sleep(orphanPollInterval)
			if parentAlive(startPPID) {
				continue
			}
			fmt.Fprintf(os.Stderr,
				"\nerror: the process that launched this run (pid %d) exited; "+
					"stopping rather than continuing as an orphan (T1450)\n", startPPID)
			KillChildren()
			os.Exit(orphanExitCode)
		}
	}()
}
