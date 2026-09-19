package common

import (
	"slices"
	"strings"
	"testing"
)

// No test in this package asks the HOST what it has installed. Which programs a
// machine carries is a fact about the machine, so a test that consults it runs
// a different check on every one: `sh` as a stand-in runtime made the tests
// below green wherever Git's usr/bin was on PATH and red everywhere else, and
// they landed on a commit that bypassed the pre-commit gate, so no host without
// it ever ran them (T2166). The probe is answered here, once, for the whole
// package.
//
// The value is a sentinel rather than a plausible path: the product only asks
// whether it is empty, and anything that later tried to EXECUTE it should fail
// loudly rather than reach some real program.
func init() {
	findRuntime = func(name string) string { return "stubbed-runtime:" + name }
}

// stubRuntimeMissing makes the probe report one runtime absent — how the test
// ABOUT the probe states its premise, instead of hoping this host lacks a
// program with some unlikely name.
func stubRuntimeMissing(t *testing.T, name string) {
	t.Helper()
	saved := findRuntime
	findRuntime = func(n string) string {
		if n == name {
			return ""
		}
		return saved(n)
	}
	t.Cleanup(func() { findRuntime = saved })
}

// The cross-target suites take minutes and need a toolchain this host may not
// have, so the runner is a seam: these tests pin which summary field becomes
// which metric, which is the part that silently reports the wrong number.
//
// The fixtures name a runtime NO machine can have. That is the regression
// guard: if the probe ever goes back to consulting PATH directly, every test
// naming it fails on every host — where naming the real wasmtime/node would
// leave them green on any machine that happens to have those, which is the
// whole bug.
func TestTargetSuite_SummaryFieldsBecomeTheBaselinedMetrics(t *testing.T) {
	const summary = "10586 passed, 3 failed, 930 skipped, 2 leaked (814 files, 270.680s)"
	stub := func(root, target string) (string, error) { return summary, nil }

	stubGateBuild(t, &fakeBuild{})
	for _, tc := range []struct {
		suite targetSuite
		want  map[string]int64
	}{
		{targetSuite{target: "wasm32-wasi", runtime: "stub-runtime", prefix: "wasm"}, map[string]int64{
			"wasm_test_failures": 3, "wasm_leak_count": 2, "wasm_test_count": 10586,
		}},
		{targetSuite{target: "wasm32-web", runtime: "stub-runtime", prefix: "wasm_web"}, map[string]int64{
			"wasm_web_test_failures": 3, "wasm_web_leak_count": 2, "wasm_web_test_count": 10586,
		}},
	} {
		metrics, incomplete, err := measureTargetSuite(t.TempDir(), tc.suite, stub)
		if err != nil {
			t.Fatalf("%s: %v", tc.suite.target, err)
		}
		if incomplete != "" {
			t.Errorf("%s: incomplete = %q, want empty", tc.suite.target, incomplete)
			continue
		}
		if len(metrics) != len(tc.want) {
			t.Fatalf("%s: got %d metrics %+v, want %d", tc.suite.target, len(metrics), metrics, len(tc.want))
		}
		got := map[string]int64{}
		for _, m := range metrics {
			got[m.Name] = m.Int
		}
		for name, want := range tc.want {
			if got[name] != want {
				t.Errorf("%s: %s = %d, want %d", tc.suite.target, name, got[name], want)
			}
		}
	}
}

// A missing runtime is not zero failures. Reporting a clean number for a suite
// that never ran is the one failure mode an incomplete reason exists to
// prevent — a ratchet would take it as the best this tree has ever been.
func TestTargetSuite_AMissingRuntimeMeasuresNothing(t *testing.T) {
	stubRuntimeMissing(t, "no-such-runtime-anywhere")
	stub := func(root, target string) (string, error) {
		t.Error("the suite ran without its runtime present")
		return "", nil
	}
	suite := targetSuite{target: "wasm32-wasi", runtime: "no-such-runtime-anywhere", prefix: "wasm"}
	metrics, incomplete, err := measureTargetSuite(t.TempDir(), suite, stub)
	if err != nil {
		t.Fatal(err)
	}
	if len(metrics) != 0 {
		t.Errorf("metrics = %v, want none: nothing was measured", metrics)
	}
	if !strings.Contains(incomplete, "no-such-runtime-anywhere") {
		t.Errorf("incomplete = %q, want it to name the missing runtime", incomplete)
	}
}

// A suite that printed no summary is a gate that could not answer, which is an
// error — distinct from an incomplete run, which answered about less.
func TestTargetSuite_NoSummaryIsAnError(t *testing.T) {
	stubGateBuild(t, &fakeBuild{})
	stub := func(root, target string) (string, error) { return "nothing parseable here", nil }
	suite := targetSuite{target: "wasm32-wasi", runtime: "stub-runtime", prefix: "wasm"}
	metrics, _, err := measureTargetSuite(t.TempDir(), suite, stub)
	if err == nil {
		t.Fatalf("a suite that printed no summary measured %+v", metrics)
	}
	if !strings.Contains(err.Error(), "no summary line") {
		t.Errorf("error = %v, want it to name the missing summary line", err)
	}
}

// Every gate the registry lists must carry a summary: the listing is read by a
// person deciding which gate to ask for, and a blank line tells them nothing.
func TestContractGates_EveryGateHasASummary(t *testing.T) {
	for _, name := range ContractGateNames() {
		if strings.TrimSpace(ContractGateSummary(name)) == "" {
			t.Errorf("gate %q has no summary", name)
		}
	}
}

// The gates added for the scheduled measurements stay OUT of integration. That
// composition is what a landing decision waits on, and the wasm suite alone
// runs longer than every host gate put together.
func TestIntegration_ExcludesTheScheduledGates(t *testing.T) {
	reach := map[string]bool{}
	var walk func(string)
	walk = func(name string) {
		if reach[name] {
			return
		}
		reach[name] = true
		for _, p := range contractGates[name].parts {
			walk(p)
		}
	}
	walk("integration")
	for _, name := range []string{"tested:wasm", "tested:wasm-web", "tested:stress", "covered", "size:wasm", "install:thin", "latest-invariant"} {
		if reach[name] {
			t.Errorf("integration reaches %q — a landing decision must not wait on it", name)
		}
		if !IsContractGate(name) {
			t.Errorf("%q is not a gate, so nothing can ask for it by name", name)
		}
	}
}

// The two names that are both a contract gate and an older subcommand must
// answer to BOTH spellings while the schedules that use the older one still
// exist. Routing a bare `bin/gate install` to a contract gate that refuses its
// arguments would take those schedules red without measuring anything.
func TestLegacySubcommands_AnswerToBothSpellings(t *testing.T) {
	for name := range legacySubcommands {
		if !IsContractGate(name) {
			t.Errorf("%q is in legacySubcommands but is not a contract gate, so the exception protects nothing", name)
		}
		if hasEnvelopeFlag([]string{name}) {
			t.Errorf("a bare %q was read as an envelope request", name)
		}
		for _, spelling := range []string{"--envelope", "-envelope"} {
			if !hasEnvelopeFlag([]string{name, spelling}) {
				t.Errorf("%q %s was not read as an envelope request", name, spelling)
			}
		}
	}
	// Every OTHER contract gate answers only to the contract spelling: the
	// exception is a migration seam, not a precedent.
	for _, name := range ContractGateNames() {
		if legacySubcommands[name] {
			continue
		}
		if slices.Contains([]string{"test", "wasm-test", "wasm-web-test", "wasm-size", "go-test", "stress", "coverage", "schema"}, name) {
			t.Errorf("gate %q shares a name with an older subcommand and is not declared in legacySubcommands, so one of the two is unreachable", name)
		}
	}
}
