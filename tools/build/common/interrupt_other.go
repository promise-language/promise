//go:build !windows

package common

import (
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"
)

var interrupted atomic.Int32

func init() {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, os.Interrupt)
	go func() {
		for range ch {
			// The pipeline stops between steps rather than mid-step, so that a
			// half-finished format or build is never mistaken for a verdict.
			if interrupted.Swap(1) != 0 {
				// Second Ctrl+C: the user has asked twice. Stop arguing.
				KillChildren()
				os.Exit(130) // 128 + SIGINT
			}
			// Subprocesses run in their own process groups (isolateChild), so
			// the terminal's Ctrl+C reaches this process alone — forward it, or
			// the current step would run to completion before anyone noticed.
			interruptChildren()
		}
	}()
}

// interruptChildren SIGINTs each running subprocess tree, so the step in flight
// ends promptly and Interrupted() is seen at the next checkpoint.
func interruptChildren() {
	childMu.Lock()
	pids := make([]int, 0, len(children))
	for pid := range children {
		pids = append(pids, pid)
	}
	childMu.Unlock()

	for _, pid := range pids {
		signalTree(pid, syscall.SIGINT)
	}
}

// Interrupted returns true if the user has pressed Ctrl+C.
func Interrupted() bool {
	return interrupted.Load() != 0
}
