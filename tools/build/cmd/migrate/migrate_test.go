package main

import (
	"encoding/json"
	"os/exec"
	"strings"
	"testing"
)

// The tracker record carries several strings that are disclosures: the lease's
// worktree (an absolute home path), the agent identifier (a machine name), the
// per-account cost. renderBody reads a closed set of fields, so none of them
// can reach a published body — this pins that, because the failure mode is
// silent and permanent.
func TestRenderBodyReadsOnlyDescriptionAndPlan(t *testing.T) {
	raw := []byte(`{
	  "id": "T9001",
	  "title": "a title",
	  "description": "the description",
	  "plan": "the plan",
	  "status": "open",
	  "flow_lease": {"agent": "linux_promise_2.sofia", "worktree": "/home/someone/prog/linux_promise_2"},
	  "cost_by_account": {"acct:b0a3bbd7-fa14-4ab9-96e8-51791bb25070": 11.31},
	  "events": [{"agent": "mac_promise_2.fmac", "text": "skipped agent"}]
	}`)
	var it Item
	if err := json.Unmarshal(raw, &it); err != nil {
		t.Fatal(err)
	}
	body := renderBody(it)

	for _, leak := range []string{
		"/home/someone", "linux_promise_2.sofia", "mac_promise_2.fmac",
		"acct:b0a3bbd7", "11.31", "flow_lease",
	} {
		if strings.Contains(body, leak) {
			t.Errorf("rendered body carries %q, which renderBody must never read:\n%s", leak, body)
		}
	}
	if !strings.Contains(body, "the description") || !strings.Contains(body, "the plan") {
		t.Errorf("rendered body lost its content:\n%s", body)
	}
	if !strings.Contains(body, "<!-- tracker:T9001 -->") {
		t.Errorf("rendered body has no tracker marker, so a repair pass cannot find it:\n%s", body)
	}
}

func TestRenderBodyOmitsPlanSectionWhenEmpty(t *testing.T) {
	body := renderBody(Item{ID: "T1", Description: "d", Plan: "   \n  "})
	if strings.Contains(body, "## Plan") {
		t.Errorf("empty plan produced a Plan heading:\n%s", body)
	}
}

func TestCitationsExcludesSelfAndDeduplicates(t *testing.T) {
	got := citations("T2184", "fixes T1952, see T1933 and T1952 again; also B0043 and D0009. Not T21840 or xT1111.")
	want := []string{"B0043", "D0009", "T1933", "T1952"}
	if len(got) != len(want) {
		t.Fatalf("citations = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("citations = %v, want %v", got, want)
		}
	}
}

func TestLiveIsOpenAndNeedsAnswerOnly(t *testing.T) {
	for _, tc := range []struct {
		status string
		live   bool
	}{
		{"open", true}, {"in_progress", true}, {"needs_answer", true},
		{"done", false}, {"wontfix", false}, {"duplicate", false},
		{"works_as_intended", false}, {"cant_reproduce", false},
	} {
		if got := (Item{Status: tc.status}).Live(); got != tc.live {
			t.Errorf("Live(%q) = %v, want %v", tc.status, got, tc.live)
		}
	}
}

// The guard parses a command string, so shellJoin has to produce one a shell
// splits back into exactly the arguments it was given. A title carrying a quote
// or a backtick is ordinary in this corpus, and a quoting bug would hand the
// guard a different command from the one that would run.
func TestShellJoinRoundTripsThroughSh(t *testing.T) {
	args := []string{
		"gh", "issue", "create",
		"--title", "`Vector[string?].push(move x)` leaks — it's 50 allocations; $HOME & \"quotes\"",
		"--body-file", "/tmp/a b/body.md",
		"--label", "type:bug",
	}
	line := shellJoin(args)
	out, err := exec.Command("/bin/sh", "-c", `printf '%s\0' `+line).Output()
	if err != nil {
		t.Fatalf("sh could not parse %q: %v", line, err)
	}
	got := strings.Split(strings.TrimSuffix(string(out), "\x00"), "\x00")
	if len(got) != len(args) {
		t.Fatalf("shellJoin(%v) split into %d args %q, want %d", args, len(got), got, len(args))
	}
	for i := range args {
		if got[i] != args[i] {
			t.Errorf("arg %d = %q, want %q", i, got[i], args[i])
		}
	}
}

// A refusal quotes the text it caught, so a summary that repeated it would be a
// second attempt to publish exactly what was just refused. normalizeReason is
// what keeps the quoted fragment out of the aggregate report.
func TestNormalizeReasonDropsTheQuotedFragment(t *testing.T) {
	for _, r := range []string{
		`blocked: disclosure refused (agent): an absolute home path names the machine's user — found "/Users/someone/"`,
		`blocked: disclosure refused (agent): a credential must never be published — found "gho_deadbeef"`,
	} {
		got := normalizeReason(r)
		if strings.Contains(got, "found") || strings.Contains(got, `"`) {
			t.Errorf("normalizeReason(%q) = %q, still carries the fragment", r, got)
		}
		if !strings.Contains(got, "disclosure refused") {
			t.Errorf("normalizeReason(%q) = %q, lost the reason", r, got)
		}
	}
}

// One item commonly carries several patches from the same step. Naming a file
// by item and step alone kept only the last — T2108's three implementation
// diffs became one file — so the name has to carry what distinguishes them.
func TestPatchFileNamesDoNotCollide(t *testing.T) {
	patches := []Patch{
		{Step: "implementation", Hash: "3a19804cf64db9f15cff6f17f215a1813a21e14b", Size: 116209},
		{Step: "implementation", Hash: "d4263497d3b5d34a3c5172d49c16db084a5c1c06", Size: 123496},
		{Step: "implementation", Hash: "e97600fd6a8d58c071f76378b238d9d02ab3e2f5", Size: 141877},
		{Step: "", Hash: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Size: 10},
	}
	seen := map[string]bool{}
	for _, p := range patches {
		n := patchFileName("T2108", p)
		if seen[n] {
			t.Fatalf("patch file name %q collides — a captured diff would be silently overwritten", n)
		}
		seen[n] = true
	}
	if len(seen) != len(patches) {
		t.Fatalf("got %d distinct names for %d patches", len(seen), len(patches))
	}
	if n := patchFileName("T1", Patch{Hash: "abc"}); !strings.Contains(n, "unknown") {
		t.Errorf("a patch with no step produced %q, want it marked unknown", n)
	}
}

// A guard that cannot answer must never be recorded as a pass.
func TestScreenFailsClosedWhenTheGuardIsMissing(t *testing.T) {
	v := screen(t.TempDir()+"/no-such-guard", t.TempDir(), Entry{ID: "T1", BodyPath: "bodies/T1.md"})
	if v.Outcome != "unexamined" {
		t.Errorf("missing guard produced outcome %q, want unexamined", v.Outcome)
	}
}
