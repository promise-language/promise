package common

import (
	"runtime"
	"slices"
	"strconv"
	"testing"
)

// The whole point of the pair is that their product is the machine's capacity,
// not its square. A regression here is invisible in a passing suite and only
// shows up as a host in swap, so pin it.
func TestGoTestConcurrencyProductStaysNearNumCPU(t *testing.T) {
	p, parallel := goTestConcurrency()
	n := runtime.NumCPU()

	if p < 2 {
		t.Errorf("p = %d, want at least 2 so packages still overlap (T1776)", p)
	}
	if parallel < 2 {
		t.Errorf("parallel = %d, want at least 2", parallel)
	}
	// Ceiling arithmetic can overshoot by less than one factor; anything beyond
	// double the core count is oversubscription of the kind this exists to stop.
	if product := p * parallel; product > 2*n {
		t.Errorf("p*parallel = %d×%d = %d, more than twice NumCPU (%d)",
			p, parallel, product, n)
	}
	if product := p * parallel; product < n {
		t.Errorf("p*parallel = %d×%d = %d, less than NumCPU (%d) — leaves the machine idle",
			p, parallel, product, n)
	}
}

func TestGoTestConcurrencyArgsAreWellFormed(t *testing.T) {
	args := goTestConcurrencyArgs()
	want := []string{"-p", "-parallel"}
	for _, flag := range want {
		i := slices.Index(args, flag)
		if i < 0 {
			t.Fatalf("goTestConcurrencyArgs() = %v, missing %s", args, flag)
		}
		if i+1 >= len(args) {
			t.Fatalf("%s has no value in %v", flag, args)
		}
		v, err := strconv.Atoi(args[i+1])
		if err != nil || v < 1 {
			t.Errorf("%s = %q, want a positive integer", flag, args[i+1])
		}
	}
}
