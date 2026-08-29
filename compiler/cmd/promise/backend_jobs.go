package main

import (
	"os"
	"runtime"
	"strconv"
	"sync"
)

// backendJobsEnv carries a compile-job budget from a parent promise process to
// its children. The multi-file test runner divides its own budget among the
// per-file children it spawns, so a nested fan-out costs the host the same
// number of concurrent backend processes as a single compile would (T1817).
const backendJobsEnv = "PROMISE_BACKEND_JOBS"

// backendJobLimit is the maximum number of LLVM backend processes (opt, llc)
// this process may have running at once.
//
// Without a bound, compileAndLinkSeparate starts one goroutine per module IR
// and one per generic instance, each spawning its own opt/llc. A program with
// many instantiations therefore spawns dozens of concurrent backend processes.
// That buys nothing even standalone — the machine has NumCPU cores either way —
// and it is ruinous under the test suite, where the fan-out is nested inside a
// per-file fan-out inside `go test`'s own parallelism (T1817).
//
// Defaults to NumCPU. A parent promise process passes a smaller share down via
// backendJobsEnv; a human can override either with the same variable.
func backendJobLimit() int {
	if v := os.Getenv(backendJobsEnv); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return runtime.NumCPU()
}

var (
	backendSemOnce sync.Once
	backendSem     chan struct{}
)

// acquireBackendJob blocks until a backend slot is free and returns the release
// function. Callers must call it — `defer acquireBackendJob()()` at the top of a
// goroutine that is about to spawn opt/llc.
func acquireBackendJob() func() {
	backendSemOnce.Do(func() {
		backendSem = make(chan struct{}, backendJobLimit())
	})
	backendSem <- struct{}{}
	return func() { <-backendSem }
}

// childBackendJobs divides this process's backend budget among children it is
// about to run concurrently, so the whole tree stays within one budget. Never
// returns less than 1: a child that cannot run a single backend process could
// not compile at all.
func childBackendJobs(children int) int {
	if children < 1 {
		children = 1
	}
	n := backendJobLimit() / children
	if n < 1 {
		n = 1
	}
	return n
}
