package common

import (
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
)

// runTracked starts cmd in its own process group, waits for it, and keeps it in
// the registry for as long as it runs. Every helper in exec.go that spawns a
// long-running command goes through this; the short probes (`git rev-parse`,
// `xcrun --show-sdk-path`) do not, since they cannot outlive us meaningfully.
func runTracked(cmd *exec.Cmd) error {
	isolateChild(cmd)
	if err := cmd.Start(); err != nil {
		return err
	}
	pid := cmd.Process.Pid
	childMu.Lock()
	children[pid] = cmd
	childMu.Unlock()

	err := cmd.Wait()

	childMu.Lock()
	delete(children, pid)
	childMu.Unlock()
	return err
}

// KillChildren terminates every subprocess this tool started and still has
// running, along with everything those subprocesses spawned. Safe to call more
// than once, and safe to call when nothing is running.
func KillChildren() {
	childMu.Lock()
	pids := make([]int, 0, len(children))
	for pid := range children {
		pids = append(pids, pid)
	}
	childMu.Unlock()

	for _, pid := range pids {
		killTree(pid)
	}
}
