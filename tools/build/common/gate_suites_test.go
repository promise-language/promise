package common

import (
	"encoding/json"
	"errors"
	"maps"
	"path/filepath"
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
// whether staging SUCCEEDED, and anything that later tried to EXECUTE it should
// fail loudly rather than reach some real program.
//
// Since T2169 the seam stages the PINNED runtime rather than probing PATH, so
// the stub stands in for a download too: no test here may reach the network any
// more than it may reach the host's installed set.
func init() {
	findRuntime = func(root, dep string) (string, error) { return "stubbed-runtime:" + dep, nil }
}

// stubRuntimeMissing makes the probe report one runtime unobtainable — how the
// test ABOUT the probe states its premise, instead of hoping this host lacks a
// program with some unlikely name.
func stubRuntimeMissing(t *testing.T, name string) {
	t.Helper()
	saved := findRuntime
	findRuntime = func(root, dep string) (string, error) {
		if dep == name {
			return "", errors.New("no blob hosted for this target")
		}
		return saved(root, dep)
	}
	t.Cleanup(func() { findRuntime = saved })
}

// suiteRecords is a `promise test --json` stream over files under root: one
// record per (file, test, status), in that order.
func suiteRecords(root string, records ...[3]string) string {
	var b strings.Builder
	for _, r := range records {
		line, _ := json.Marshal(map[string]any{
			"file": filepath.Join(root, r[0]), "test": r[1], "status": r[2], "elapsed": 0.01,
			"context": "panic: " + r[1] + " went wrong",
		})
		b.Write(line)
		b.WriteByte('\n')
	}
	return b.String()
}

// The cross-target suites take minutes and need a toolchain this host may not
// have, so the runner is a seam: these tests pin which record becomes which
// metric, which is the part that silently reports the wrong number.
//
// The fixtures name a runtime NO machine can have. That is the regression
// guard: if the probe ever goes back to consulting PATH directly, every test
// naming it fails on every host — where naming the real wasmtime/node would
// leave them green on any machine that happens to have those, which is the
// whole bug.
func TestTargetSuite_RecordsBecomeTheMetrics(t *testing.T) {
	stubGateBuild(t, &fakeBuild{})
	for _, prefix := range []string{"wasm", "wasm_web"} {
		root := t.TempDir()
		jsonl := suiteRecords(root,
			[3]string{"tests/a.pr", "ok_one", "pass"}, [3]string{"tests/a.pr", "ok_two", "pass"},
			[3]string{"tests/a.pr", "broken", "fail"}, [3]string{"tests/b.pr", "slow", "timeout"},
			[3]string{"tests/b.pr", "leaky", "leak"}, [3]string{"tests/b.pr", "skipped", "excluded"})
		stub := func(root, target string) (string, error) { return jsonl, errors.New("exit status 1") }
		suite := targetSuite{target: "wasm32-wasi", runtime: "stub-runtime", prefix: prefix}
		m, err := measureTargetSuite(root, suite, stub)
		if err != nil {
			t.Fatalf("%s: %v", prefix, err)
		}
		if m.incomplete != "" {
			t.Errorf("%s: incomplete = %q, want empty", prefix, m.incomplete)
		}
		want := map[string]int64{
			"failed_tests": 2, prefix + "_failed_tests": 2,
			"leaked_tests": 1, prefix + "_leaked_tests": 1,
			"test_count": 2, prefix + "_test_count": 2,
		}
		got := map[string]int64{}
		for _, metric := range m.metrics {
			got[metric.Name] = metric.Int
		}
		if !maps.Equal(got, want) {
			t.Errorf("%s: metrics = %v, want %v", prefix, got, want)
		}
		// A group per file, a member per record that did not pass, carrying
		// the runner's own words.
		if len(m.groups) != 2 || m.groups[0].Name != "tests/a.pr" || len(m.groups[1].Members) != 3 {
			t.Fatalf("%s: groups = %+v", prefix, m.groups)
		}
		if mem := m.groups[0].Members[0]; mem.Name != "broken" || mem.Outcome != OutcomeFail || mem.Detail != "panic: broken went wrong" {
			t.Errorf("%s: member = %+v", prefix, mem)
		}
	}
}

// A runtime that could not be staged is not zero failures. Reporting a clean
// number for a suite that never ran is the one failure mode an incomplete
// reason exists to prevent — a ratchet would take it as the best this tree has
// ever been.
func TestTargetSuite_AMissingRuntimeMeasuresNothing(t *testing.T) {
	stubRuntimeMissing(t, "no-such-runtime-anywhere")
	stub := func(root, target string) (string, error) {
		t.Error("the suite ran without its runtime present")
		return "", nil
	}
	suite := targetSuite{target: "wasm32-wasi", runtime: "no-such-runtime-anywhere", prefix: "wasm"}
	m, err := measureTargetSuite(t.TempDir(), suite, stub)
	if err != nil {
		t.Fatal(err)
	}
	incomplete := m.incomplete
	if len(m.metrics) != 0 {
		t.Errorf("metrics = %v, want none: nothing was measured", m.metrics)
	}
	if !strings.Contains(incomplete, "no-such-runtime-anywhere") {
		t.Errorf("incomplete = %q, want it to name the missing runtime", incomplete)
	}
	// The reason has to say the PINNED runtime could not be staged. "not
	// installed" would send its reader to install one, which since T2169 changes
	// nothing about what the gate would run.
	if !strings.Contains(incomplete, "pinned") || !strings.Contains(incomplete, "staged") {
		t.Errorf("incomplete = %q, want it to report that the pinned runtime could not be staged", incomplete)
	}
	// And it must carry the underlying cause: "could not be staged" with no why
	// leaves an offline host and an unpublished blob looking identical.
	if !strings.Contains(incomplete, "no blob hosted for this target") {
		t.Errorf("incomplete = %q, want it to carry the staging error", incomplete)
	}
}

// A suite that reported no records is a gate that could not answer, which is an
// error — distinct from an incomplete run, which answered about less.
func TestTargetSuite_NoRecordsIsAnError(t *testing.T) {
	stubGateBuild(t, &fakeBuild{})
	stub := func(root, target string) (string, error) {
		return "nothing parseable here", errors.New("exit status 2")
	}
	suite := targetSuite{target: "wasm32-wasi", runtime: "stub-runtime", prefix: "wasm"}
	m, err := measureTargetSuite(t.TempDir(), suite, stub)
	if err == nil {
		t.Fatalf("a suite that reported no records measured %+v", m.metrics)
	}
	if !strings.Contains(err.Error(), "no test records") || !strings.Contains(err.Error(), "exit status 2") {
		t.Errorf("error = %v, want it to name the missing records and the runner's exit", err)
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
