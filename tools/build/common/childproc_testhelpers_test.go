package common

import (
	"fmt"
	"io"
	"runtime"
	"time"
)

// trackedCount and trackedHas read the child registry under its own lock, so a
// test never races the bookkeeping it is asserting on.
func trackedCount() int {
	childMu.Lock()
	defer childMu.Unlock()
	return len(children)
}

func trackedHas(pid int) bool {
	childMu.Lock()
	defer childMu.Unlock()
	_, ok := children[pid]
	return ok
}

// sleepCmd/sleepArgs give a "run for N seconds and exit 0" command on every
// platform. Windows has no sleep(1); ping's inter-packet delay is the usual
// stand-in, and needs one extra packet to wait N whole seconds.
func sleepCmd() string {
	if runtime.GOOS == "windows" {
		return "cmd"
	}
	return "sleep"
}

func sleepArgs(seconds string) []string {
	if runtime.GOOS == "windows" {
		n, err := time.ParseDuration(seconds + "s")
		if err != nil {
			n = time.Second
		}
		return []string{"/c", fmt.Sprintf("ping -n %d 127.0.0.1 > NUL", int(n.Seconds())+1)}
	}
	return []string{seconds}
}

// fscanPid reads a single decimal pid from r.
func fscanPid(r io.Reader, pid *int) (int, error) {
	return fmt.Fscan(r, pid)
}

// waitGone reports whether pid stopped being alive within d.
func waitGone(pid int, d time.Duration) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if !processAlive(pid) {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return !processAlive(pid)
}
