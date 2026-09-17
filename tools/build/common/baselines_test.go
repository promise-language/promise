package common

import "testing"

// fp makes a *float64 for a test baseline. It lived beside the commit gate
// until that tool was deleted; the baselines it builds are still what the
// judge's tests are about.
func fp(v float64) *float64 { return &v }

// The directions are the whole meaning of a ratchet, and each one is the
// opposite mistake from its neighbour: reading "up" as "must not increase"
// would pass every regression it exists to catch, silently, for as long as the
// metric kept getting worse.
func TestCheckRatchet_Directions(t *testing.T) {
	for _, tc := range []struct {
		direction        string
		baseline, actual float64
		want             bool
	}{
		{"up", 100, 101, true},   // more is better, and there is more
		{"up", 100, 100, true},   // equal holds the floor
		{"up", 100, 99, false},   // fewer tests than the best run is a regression
		{"down", 10, 9, true},    // fewer is better
		{"down", 10, 10, true},   // equal holds the ceiling
		{"down", 10, 11, false},  // more failures than the best run is a regression
		{"exact", 0, 0, true},    // pinned
		{"exact", 0, 1, false},   // any movement at all
		{"", 0, 999, true},       // informational: tracked, never enforced
		{"sideways", 0, 9, true}, // an unknown direction judges nothing, rather than guessing one
	} {
		if got := checkRatchet(tc.direction, tc.baseline, tc.actual); got != tc.want {
			t.Errorf("checkRatchet(%q, %v, %v) = %v, want %v",
				tc.direction, tc.baseline, tc.actual, got, tc.want)
		}
	}
}

// A person reading a refusal is told what the rule is, and the verb has to
// match the direction it renders — "must not increase" under a floor would
// send them to fix the opposite of what failed.
func TestRatchetVerb_MatchesTheDirection(t *testing.T) {
	for direction, want := range map[string]string{
		"up":    "must not decrease",
		"down":  "must not increase",
		"exact": "must not change",
		"":      "is informational",
	} {
		if got := ratchetVerb(direction); got != want {
			t.Errorf("ratchetVerb(%q) = %q, want %q", direction, got, want)
		}
	}
}

// The file this project judges against must parse, and carry the platform this
// run is on: a baselines file that silently read as empty would make every
// metric unjudged and every gate pass.
func TestLoadBaselines_ReadsThisProjectsFile(t *testing.T) {
	b, err := LoadBaselines("../../..")
	if err != nil {
		t.Fatal(err)
	}
	if len(b) == 0 {
		t.Fatal("baselines.json parsed to no platforms at all")
	}
	if _, ok := b[HostTarget()]; !ok {
		t.Errorf("baselines.json has no block for %s, so nothing measured here is judged", HostTarget())
	}
}
