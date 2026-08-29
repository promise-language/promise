package main

import (
	"runtime"
	"testing"
)

func TestBackendJobLimitDefaultsToNumCPU(t *testing.T) {
	t.Setenv(backendJobsEnv, "")
	if got, want := backendJobLimit(), runtime.NumCPU(); got != want {
		t.Errorf("backendJobLimit() = %d, want NumCPU %d", got, want)
	}
}

func TestBackendJobLimitHonoursEnv(t *testing.T) {
	t.Setenv(backendJobsEnv, "3")
	if got := backendJobLimit(); got != 3 {
		t.Errorf("backendJobLimit() = %d, want 3", got)
	}
}

// A malformed or nonsensical budget must not disable the backend: falling back
// to NumCPU keeps compiling, where honouring a 0 or -1 would deadlock on an
// empty semaphore.
func TestBackendJobLimitRejectsNonPositiveAndGarbage(t *testing.T) {
	for _, v := range []string{"0", "-1", "many", "3.5", " 3"} {
		t.Run(v, func(t *testing.T) {
			t.Setenv(backendJobsEnv, v)
			if got, want := backendJobLimit(), runtime.NumCPU(); got != want {
				t.Errorf("backendJobLimit() = %d for %q, want NumCPU %d", got, v, want)
			}
		})
	}
}

func TestChildBackendJobsDividesTheBudget(t *testing.T) {
	t.Setenv(backendJobsEnv, "12")
	tests := []struct {
		children int
		want     int
	}{
		{1, 12},
		{2, 6},
		{5, 2}, // truncates: the tree stays at or under the budget, never over
		{12, 1},
		{100, 1}, // a child that cannot run one backend process could not compile
		{0, 12},  // nonsense child counts must not produce a zero budget
		{-1, 12},
	}
	for _, tc := range tests {
		if got := childBackendJobs(tc.children); got != tc.want {
			t.Errorf("childBackendJobs(%d) = %d, want %d", tc.children, got, tc.want)
		}
	}
}

// The point of the split is that the whole tree costs the host one budget, not
// one per child — that is the invariant T1817 was about.
func TestChildBackendJobsKeepsTreeWithinOneBudget(t *testing.T) {
	t.Setenv(backendJobsEnv, "12")
	for children := 1; children <= 12; children++ {
		if total := children * childBackendJobs(children); total > 12 {
			t.Errorf("%d children × %d jobs = %d, exceeds the budget of 12",
				children, childBackendJobs(children), total)
		}
	}
}

func TestAcquireBackendJobBoundsConcurrency(t *testing.T) {
	// backendSem is created once per process, so this asserts against whatever
	// limit is in force rather than setting its own.
	limit := backendJobLimit()

	releases := make([]func(), 0, limit)
	for range limit {
		releases = append(releases, acquireBackendJob())
	}

	// The next acquire must block until one of those is released.
	got := make(chan struct{})
	go func() {
		release := acquireBackendJob()
		close(got)
		release()
	}()

	select {
	case <-got:
		t.Fatalf("acquireBackendJob() returned with all %d slots held", limit)
	default:
	}

	releases[0]()
	<-got // blocks (and fails the test by timeout) if the release did not free a slot

	for _, release := range releases[1:] {
		release()
	}
}
