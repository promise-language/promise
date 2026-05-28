package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	flowsdk "djabi.dev/go/flow_sdk"
	"djabi.dev/go/flow_sdk/runner"
	"djabi.dev/go/flow_sdk/tracker"
)

// harness wires a flow to mock tracker + runner HTTP servers backed by a shared
// in-memory item, so the step executors run end-to-end without a real claude.
type harness struct {
	t    *testing.T
	item *flowsdk.Item

	// runner-mock knobs
	agentResp   flowsdk.RunAgentResponse
	gitStatus   flowsdk.GitStatus
	statusFn    func() flowsdk.GitStatus // optional: vary status across calls
	verifyOK    bool
	pushOK      bool
	rebaseOK    bool
	captureNoOp bool   // capture-patch returns success but attaches no patch (empty diff / runner no-op)
	onAgentTurn func() // optional: simulate the agent's tracker-side MCP work during a turn

	patched      []map[string]json.RawMessage // recorded PATCH bodies
	parked       *flowsdk.ParkRequest         // last POST /park request, if any
	lastAgentReq flowsdk.RunAgentRequest      // last /agent request body
	agentCalls   int                          // number of /agent turns served
	wt           string                       // arena worktree (holds the bin/verify marker)
	f            *flow
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	h := &harness{
		t:    t,
		item: &flowsdk.Item{ID: "T0001", Title: "Do a thing", Status: flowsdk.StatusOpen},
		agentResp: flowsdk.RunAgentResponse{
			Success: true, LastText: "did the work", SessionID: "s1",
		},
		gitStatus: flowsdk.GitStatus{Branch: "master", Clean: false, LastCommit: "base1", Ahead: 0},
		verifyOK:  true,
		pushOK:    true,
		rebaseOK:  true,
	}
	trSrv := httptest.NewServer(http.HandlerFunc(h.trackerHandler))
	rnSrv := httptest.NewServer(http.HandlerFunc(h.runnerHandler))
	t.Cleanup(trSrv.Close)
	t.Cleanup(rnSrv.Close)

	c := flowsdk.Context{
		TrackerURL: trSrv.URL, RunnerURL: rnSrv.URL,
		ItemID: "T0001", InvocationID: "inv1", FlowName: flowName, Agent: "tester",
	}
	h.wt = t.TempDir()
	h.f = &flow{
		ctx: context.Background(), tr: tracker.New(c), rn: runner.New(c),
		id: "T0001", inv: "inv1", agent: "tester", worktree: h.wt, item: h.item,
	}
	// By default the agent's turn "runs bin/verify" and it passes: each turn writes a
	// passing marker. The implement step clears any stale marker before the turn and
	// then accepts only one the turn produced, so this models the happy path. Tests
	// exercising the verify gate override onAgentTurn (write a failing marker, write
	// none, or defer the passing write to a later turn).
	h.onAgentTurn = func() { h.writeVerifyMark(true) }
	return h
}

// writeVerifyMark writes a bin/verify success marker (gate-values.json) into the
// harness worktree. pass=false writes a marker carrying a failing metric.
func (h *harness) writeVerifyMark(pass bool) {
	h.t.Helper()
	dir := filepath.Join(h.wt, ".promise-home")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		h.t.Fatal(err)
	}
	gv := gateValues{Values: map[string]float64{
		"host_test_count": 10, "host_test_failures": 0, "host_leak_count": 0,
	}}
	if !pass {
		gv.Values["host_test_failures"] = 2
	}
	data, _ := json.Marshal(gv)
	if err := os.WriteFile(filepath.Join(dir, "gate-values.json"), data, 0o644); err != nil {
		h.t.Fatal(err)
	}
}

// ── tracker mock ─────────────────────────────────────────────────────────────

func (h *harness) trackerHandler(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/T0001"):
		h.writeItem(w)
	case r.Method == http.MethodPatch:
		var body map[string]json.RawMessage
		json.NewDecoder(r.Body).Decode(&body)
		h.patched = append(h.patched, body)
		h.applyPatch(body)
		h.writeItem(w)
	case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/park"):
		var body struct {
			Park flowsdk.ParkRequest `json:"park"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		h.parked = &body.Park
		h.writeItem(w)
	case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/questions"):
		h.item.Questions = append(h.item.Questions, flowsdk.Question{
			ID: "q1", AgentQuestion: flowsdk.AgentQuestion{Text: "?"},
		})
		h.writeItem(w)
	case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/suggestions"):
		var body struct {
			Suggestions []flowsdk.ItemSuggestion `json:"suggestions"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		h.item.Suggestions = append(h.item.Suggestions, body.Suggestions...)
		h.writeItem(w)
	default:
		http.Error(w, "unexpected: "+r.Method+" "+r.URL.Path, http.StatusNotFound)
	}
}

func (h *harness) writeItem(w http.ResponseWriter) {
	json.NewEncoder(w).Encode(h.item)
}

// applyPatch mimics the tracker's PATCH semantics for the fields the flow writes:
// set the content field and upsert the checklist entry (clearing Stale).
func (h *harness) applyPatch(body map[string]json.RawMessage) {
	str := func(k string) string {
		var s string
		json.Unmarshal(body[k], &s)
		return s
	}
	if v, ok := body["plan"]; ok {
		json.Unmarshal(v, &h.item.Plan)
		h.upsert(flowsdk.ArtifactPlan)
	}
	if v, ok := body["review_summary"]; ok {
		json.Unmarshal(v, &h.item.ReviewSummary)
		h.upsert(flowsdk.ArtifactReview)
	}
	if v, ok := body["test_coverage"]; ok {
		json.Unmarshal(v, &h.item.TestCoverage)
		h.upsert(flowsdk.ArtifactCoverage)
	}
	if _, ok := body["commit"]; ok {
		h.item.CommitHash = flowsdk.GitHash(str("commit"))
		h.upsert(flowsdk.ArtifactCommit)
	}
	if _, ok := body["push"]; ok {
		h.item.PushedHash = flowsdk.GitHash(str("push"))
		h.upsert(flowsdk.ArtifactPush)
	}
	if v, ok := body["inspection"]; ok {
		var insp flowsdk.Inspection
		json.Unmarshal(v, &insp)
		h.item.Inspection = &insp
		h.upsert(flowsdk.ArtifactInspection)
	}
	if v, ok := body["status"]; ok {
		json.Unmarshal(v, &h.item.Status)
	}
	if v, ok := body["summary"]; ok {
		json.Unmarshal(v, &h.item.Summary)
	}
	if v, ok := body["require_artifacts"]; ok {
		var keys []flowsdk.ArtifactKey
		json.Unmarshal(v, &keys)
		for _, k := range keys {
			if h.item.Artifact(k) == nil {
				h.item.Artifacts = append(h.item.Artifacts, flowsdk.ItemArtifact{Key: k, Required: true})
			}
		}
	}
	if v, ok := body["mark_stale"]; ok {
		var keys []flowsdk.ArtifactKey
		json.Unmarshal(v, &keys)
		for _, k := range keys {
			for i := range h.item.Artifacts {
				if h.item.Artifacts[i].Key == k {
					h.item.Artifacts[i].Stale = true
				}
			}
		}
	}
}

func (h *harness) upsert(key flowsdk.ArtifactKey) {
	for i := range h.item.Artifacts {
		if h.item.Artifacts[i].Key == key {
			h.item.Artifacts[i].Stale = false
			h.item.Artifacts[i].ProducedAt = time.Now()
			return
		}
	}
	h.item.Artifacts = append(h.item.Artifacts, flowsdk.ItemArtifact{Key: key, ProducedAt: time.Now()})
}

// ── runner mock ──────────────────────────────────────────────────────────────

func (h *harness) runnerHandler(w http.ResponseWriter, r *http.Request) {
	p := r.URL.Path
	switch {
	case strings.HasSuffix(p, "/agent"):
		json.NewDecoder(r.Body).Decode(&h.lastAgentReq)
		h.agentCalls++
		if h.onAgentTurn != nil {
			h.onAgentTurn() // the agent's MCP/worktree side-effects (e.g. filing phases, passing verify)
		}
		json.NewEncoder(w).Encode(h.agentResp)
	case strings.HasSuffix(p, "/arena/worktree"):
		json.NewEncoder(w).Encode(flowsdk.Worktree{Path: "/wt", Branch: "master"})
	case strings.HasSuffix(p, "/arena/status"):
		gs := h.gitStatus
		if h.statusFn != nil {
			gs = h.statusFn()
		}
		json.NewEncoder(w).Encode(gs)
	case strings.HasSuffix(p, "/arena/check-ahead"):
		json.NewEncoder(w).Encode(flowsdk.CheckAheadResult{State: flowsdk.WorktreeAhead})
	case strings.HasSuffix(p, "/arena/clean-pull"):
		json.NewEncoder(w).Encode(flowsdk.ArenaResult{Success: true})
	case strings.HasSuffix(p, "/arena/commit"):
		h.gitStatus.LastCommit = "committed1" // a new commit appears
		json.NewEncoder(w).Encode(flowsdk.ArenaResult{Success: true})
	case strings.HasSuffix(p, "/arena/push"):
		json.NewEncoder(w).Encode(flowsdk.ArenaResult{Success: h.pushOK, Error: pushErr(h.pushOK)})
	case strings.HasSuffix(p, "/arena/rebase"):
		json.NewEncoder(w).Encode(flowsdk.ArenaResult{Success: h.rebaseOK, Error: rebaseErr(h.rebaseOK)})
	case strings.HasSuffix(p, "/arena/validate"):
		json.NewEncoder(w).Encode(flowsdk.ArenaResult{Success: h.verifyOK, Output: "verify", Error: verifyErr(h.verifyOK)})
	case strings.HasSuffix(p, "/arena/capture-patch"):
		// captureNoOp models a runner that reports success but attaches no non-empty
		// patch (empty worktree diff / silent no-op) — the implement step must catch it.
		if !h.captureNoOp {
			h.item.Patches = append(h.item.Patches, flowsdk.ItemPatch{Hash: "p1"})
		}
		json.NewEncoder(w).Encode(flowsdk.ArenaResult{Success: true})
	default:
		io.Copy(io.Discard, r.Body)
		http.Error(w, "unexpected runner op: "+p, http.StatusNotFound)
	}
}

func pushErr(ok bool) string {
	if ok {
		return ""
	}
	return "push rejected"
}
func verifyErr(ok bool) string {
	if ok {
		return ""
	}
	return "verify failed"
}
func rebaseErr(ok bool) string {
	if ok {
		return ""
	}
	return "rebase conflict"
}

// ── tests ────────────────────────────────────────────────────────────────────

func TestRun_SeedsAndRunsPlan(t *testing.T) {
	h := newHarness(t)
	h.gitStatus.Clean = true // plan step (first step) requires a clean, current tree
	res := h.f.run()
	if res.Step != flowsdk.StepName(flowsdk.ArtifactPlan) || res.Status != flowsdk.StepDone {
		t.Fatalf("result = %+v, want plan/done", res)
	}
	if h.item.Plan == "" {
		t.Error("plan artifact not recorded")
	}
	// The checklist was seeded with every canonical step.
	if len(h.item.Artifacts) < len(taskSteps) {
		t.Errorf("checklist not seeded: %d entries", len(h.item.Artifacts))
	}
}

func TestRun_Finalized(t *testing.T) {
	h := newHarness(t)
	for _, k := range taskSteps {
		setArtifactPresent(h.item, k)
		h.item.Artifacts = append(h.item.Artifacts, flowsdk.ItemArtifact{Key: k, Required: true})
	}
	res := h.f.run()
	if res.Status != flowsdk.StepSkipped {
		t.Fatalf("finalized run = %+v, want skipped", res)
	}
}

// TestRun_FinalizedFlagRefuses confirms the tracker's permanent finalized flag
// makes the flow refuse to run a step (skip), even when the flow's own artifact
// view still has required steps missing and no agent turn is taken.
func TestRun_FinalizedFlagRefuses(t *testing.T) {
	h := newHarness(t)
	h.item.Plan = "the plan" // only plan present; implementation onward "missing"
	for _, k := range taskSteps {
		h.item.Artifacts = append(h.item.Artifacts, flowsdk.ItemArtifact{Key: k, Required: true})
	}
	h.item.FinalizedFlag = true
	res := h.f.run()
	if res.Status != flowsdk.StepSkipped {
		t.Fatalf("finalized-flag run = %+v, want skipped (refused)", res)
	}
	if h.agentCalls != 0 {
		t.Errorf("agent turns = %d, want 0 (must not run a step when finalized)", h.agentCalls)
	}
}

// TestRun_PreflightRefusesForeignFlow confirms the generic ownership preflight
// makes the flow step aside (clean skip, no work, no checklist seeding) when the
// item selected a different flow.
func TestRun_PreflightRefusesForeignFlow(t *testing.T) {
	h := newHarness(t)
	h.item.Flow = "some-other-flow"
	res := h.f.run()
	if res.Status != flowsdk.StepSkipped {
		t.Fatalf("foreign-flow run = %+v, want skipped", res)
	}
	if h.agentCalls != 0 {
		t.Errorf("agent turns = %d, want 0 (must not work on a foreign-flow item)", h.agentCalls)
	}
	if len(h.item.Artifacts) != 0 {
		t.Errorf("checklist seeded (%d entries) — preflight must refuse before any write", len(h.item.Artifacts))
	}
}

// TestRun_PreflightRefusesFlowNone confirms an item opted out of flow processing
// (FlowNone) is left untouched.
func TestRun_PreflightRefusesFlowNone(t *testing.T) {
	h := newHarness(t)
	h.item.Flow = flowsdk.FlowNone
	res := h.f.run()
	if res.Status != flowsdk.StepSkipped {
		t.Fatalf("FlowNone run = %+v, want skipped", res)
	}
	if h.agentCalls != 0 {
		t.Errorf("agent turns = %d, want 0 (FlowNone opts out)", h.agentCalls)
	}
}

func TestStepPlan_RecordsPlan(t *testing.T) {
	h := newHarness(t)
	h.agentResp.LastText = "the plan text"
	res := h.f.stepPlan()
	if res.Status != flowsdk.StepDone {
		t.Fatalf("status = %q, want done", res.Status)
	}
	if h.item.Plan != "the plan text" {
		t.Errorf("plan = %q", h.item.Plan)
	}
}

func TestStepPlan_Question(t *testing.T) {
	h := newHarness(t)
	h.agentResp.Question = &flowsdk.AgentQuestion{Text: "which approach?"}
	res := h.f.stepPlan()
	if res.Status != flowsdk.StepDone {
		t.Fatalf("status = %q, want done (question parks via needs_answer)", res.Status)
	}
	if !h.item.NeedsAnswer() {
		t.Error("question was not recorded → item should be needs_answer")
	}
}

func TestStepPlan_AgentFailure(t *testing.T) {
	h := newHarness(t)
	h.agentResp = flowsdk.RunAgentResponse{Success: false, Failure: &flowsdk.AgentFailure{Reason: "no result"}}
	res := h.f.stepPlan()
	if res.Status != flowsdk.StepFailed {
		t.Fatalf("status = %q, want failed", res.Status)
	}
	if res.Error == "" {
		t.Error("expected an error reason on a failed step")
	}
}

func TestStepPlan_AgentClosedItem(t *testing.T) {
	h := newHarness(t)
	// The agent closes the item as infeasible mid-turn (via MCP); the flow detects
	// the terminal state on the post-turn refresh and exits done.
	h.item.Status = flowsdk.StatusWontfix
	res := h.f.stepPlan()
	if res.Status != flowsdk.StepDone {
		t.Fatalf("status = %q, want done", res.Status)
	}
	if h.item.Plan != "" {
		t.Error("should not record a plan for a closed item")
	}
}

func TestStepImplementation_CapturesPatch(t *testing.T) {
	h := newHarness(t)
	h.item.Plan = "the plan" // required prerequisite for the implement step
	res := h.f.stepImplementation()
	if res.Status != flowsdk.StepDone {
		t.Fatalf("status = %q, want done", res.Status)
	}
	if len(h.item.Patches) == 0 {
		t.Error("no patch captured")
	}
	if !h.item.ArtifactPresent(flowsdk.ArtifactImplementation) {
		t.Error("implementation artifact should be present (a captured non-empty patch)")
	}
}

// TestStepImplementation_CaptureNoPatchFails guards the stuck-loop regression: a
// capture-patch that succeeds but attaches no non-empty patch must FAIL the step
// (so the tracker retries/parks), not report done with the artifact still absent —
// which would make derivation re-pick `implementation` forever.
func TestStepImplementation_CaptureNoPatchFails(t *testing.T) {
	h := newHarness(t)
	h.item.Plan = "the plan"
	h.captureNoOp = true // runner reports success but attaches nothing
	res := h.f.stepImplementation()
	if res.Status != flowsdk.StepFailed {
		t.Fatalf("status = %q, want failed when no patch lands", res.Status)
	}
	if h.item.ArtifactPresent(flowsdk.ArtifactImplementation) {
		t.Error("implementation artifact must NOT be present when no patch landed")
	}
	if !strings.Contains(res.Error, "did not land") {
		t.Errorf("error = %q, want it to explain the artifact did not land", res.Error)
	}
}

func TestStepImplementation_NoPlanFails(t *testing.T) {
	h := newHarness(t)
	h.item.Plan = "" // prerequisite missing
	res := h.f.stepImplementation()
	if res.Status != flowsdk.StepFailed {
		t.Fatalf("status = %q, want failed when no plan is recorded", res.Status)
	}
	if len(h.item.Patches) != 0 {
		t.Error("must not capture a patch when the plan prerequisite is missing")
	}
}

func TestStepImplementation_CaptureOnlyAfterVerify(t *testing.T) {
	// The happy path must require the verify marker before capturing: with the
	// default passing marker present, attempt 1 completes immediately (no reprompt).
	h := newHarness(t)
	h.item.Plan = "the plan"
	h.agentResp.LastText = "implemented the widget; verify green"
	res := h.f.stepImplementation()
	if res.Status != flowsdk.StepDone {
		t.Fatalf("status = %q (%s), want done with a passing verify marker", res.Status, res.Error)
	}
	if h.agentCalls != 1 {
		t.Errorf("agent turns = %d, want 1 (no reprompt when verify already passed)", h.agentCalls)
	}
	if h.item.Summary != "implemented the widget; verify green" {
		t.Errorf("resolution summary = %q, want the implementation turn's report", h.item.Summary)
	}
	if len(h.item.Patches) == 0 {
		t.Error("patch should be captured once verify passed")
	}
	if h.item.Status != flowsdk.StatusDone {
		t.Errorf("status = %q, want done once implementation completes + verify passes", h.item.Status)
	}
}

func TestStepImplementation_VerifyMissingParks(t *testing.T) {
	h := newHarness(t)
	h.item.Plan = "the plan"
	h.onAgentTurn = func() {} // the agent never makes verify pass (no marker produced)

	res := h.f.stepImplementation()
	if res.Status != flowsdk.StepFailed {
		t.Fatalf("status = %q, want failed when verify never passes", res.Status)
	}
	if h.parked == nil {
		t.Fatal("must park when verify cannot be made to pass within the attempt budget")
	}
	if h.parked.Kind != flowsdk.FlowFailureStep || h.parked.Transient || h.parked.ReleaseLease {
		t.Errorf("park should be a non-transient step failure that KEEPS the lease, got %+v", *h.parked)
	}
	if len(h.item.Patches) != 0 {
		t.Error("must NOT capture the implementation patch when verify never passed")
	}
	if h.item.Status == flowsdk.StatusDone {
		t.Error("must NOT mark the item done when verify never passed")
	}
	if h.agentCalls != maxImplementVerifyAttempts {
		t.Errorf("agent turns = %d, want %d (initial + reprompts up to the cap)", h.agentCalls, maxImplementVerifyAttempts)
	}
}

func TestStepImplementation_VerifyFailingMetricParks(t *testing.T) {
	h := newHarness(t)
	h.item.Plan = "the plan"
	h.onAgentTurn = func() { h.writeVerifyMark(false) } // verify runs but reports failures

	res := h.f.stepImplementation()
	if res.Status != flowsdk.StepFailed || h.parked == nil {
		t.Fatalf("a marker reporting failures must not complete the step; status=%q parked=%v", res.Status, h.parked != nil)
	}
	if len(h.item.Patches) != 0 {
		t.Error("must not capture a patch when the verify marker reports failures")
	}
}

func TestStepImplementation_StaleMarkerCleared(t *testing.T) {
	// A leftover passing marker from a previous item in a reused worktree must NOT
	// let the step complete without THIS item being verified: the step clears it
	// before the turn, so an agent that doesn't re-verify cannot pass on the stale one.
	h := newHarness(t)
	h.item.Plan = "the plan"
	h.writeVerifyMark(true)   // stale marker present at entry
	h.onAgentTurn = func() {} // the agent does not run verify this step

	res := h.f.stepImplementation()
	if res.Status != flowsdk.StepFailed || h.parked == nil {
		t.Fatalf("a stale marker must be cleared so the step parks without fresh verify; status=%q parked=%v", res.Status, h.parked != nil)
	}
	if len(h.item.Patches) != 0 {
		t.Error("must not capture a patch on the strength of a stale marker")
	}
	if h.item.Status == flowsdk.StatusDone {
		t.Error("must not mark done on the strength of a stale marker")
	}
}

func TestStepImplementation_VerifyPassesAfterReprompt(t *testing.T) {
	h := newHarness(t)
	h.item.Plan = "the plan"
	// The agent gets verify green on its 2nd turn (the first reprompt); the first
	// turn produces no passing marker.
	h.onAgentTurn = func() {
		if h.agentCalls >= 2 {
			h.writeVerifyMark(true)
		}
	}

	res := h.f.stepImplementation()
	if res.Status != flowsdk.StepDone {
		t.Fatalf("status = %q (%s), want done once verify passes", res.Status, res.Error)
	}
	if h.parked != nil {
		t.Errorf("must not park when verify eventually passes, got %+v", *h.parked)
	}
	if len(h.item.Patches) == 0 {
		t.Error("patch should be captured once verify passes")
	}
	if h.item.Status != flowsdk.StatusDone {
		t.Errorf("status = %q, want done after verify passes on a reprompt", h.item.Status)
	}
	if h.agentCalls != 2 {
		t.Errorf("agent turns = %d, want 2 (initial + one reprompt)", h.agentCalls)
	}
	// The reprompt must CONTINUE the session, not start fresh.
	if !h.lastAgentReq.Resume || h.lastAgentReq.FreshSession {
		t.Errorf("reprompt should resume the implementation session, got %+v", h.lastAgentReq)
	}
}

func TestStepImplementation_QuestionDuringReprompt(t *testing.T) {
	// If the agent asks a question on a reprompt turn, the flow exits at once
	// (needs_answer) rather than continuing to park.
	h := newHarness(t)
	h.item.Plan = "the plan"
	// The agent never produces a passing marker; on the reprompt it asks a question.
	h.onAgentTurn = func() {
		if h.agentCalls >= 2 {
			h.agentResp.Question = &flowsdk.AgentQuestion{Text: "which approach?"}
		}
	}
	res := h.f.stepImplementation()
	if res.Status != flowsdk.StepDone {
		t.Fatalf("status = %q, want done (a question parks via needs_answer)", res.Status)
	}
	if h.parked != nil {
		t.Error("a question must not also trigger a verify park")
	}
	if !h.item.NeedsAnswer() {
		t.Error("the reprompt question should have been recorded → needs_answer")
	}
}

func TestFreshSessionSteps(t *testing.T) {
	// The plan step opens the flow's work and inspection is an independent
	// assessment — both must start a fresh agent session; other steps may resume.
	h := newHarness(t)
	if res := h.f.stepPlan(); res.Status != flowsdk.StepDone {
		t.Fatalf("stepPlan = %q (%s), want done", res.Status, res.Error)
	}
	if !h.lastAgentReq.FreshSession {
		t.Error("plan step must request a fresh agent session (FreshSession=true)")
	}

	hInspect := newHarness(t)
	if res := hInspect.f.stepInspection(); res.Status != flowsdk.StepDone {
		t.Fatalf("stepInspection = %q (%s), want done", res.Status, res.Error)
	}
	if !hInspect.lastAgentReq.FreshSession {
		t.Error("inspection step must request a fresh agent session (FreshSession=true)")
	}

	// A resumable step (review) must NOT force a fresh session.
	h2 := newHarness(t)
	if res := h2.f.stepReview(); res.Status != flowsdk.StepDone {
		t.Fatalf("stepReview = %q (%s), want done", res.Status, res.Error)
	}
	if h2.lastAgentReq.FreshSession {
		t.Error("a resumable step must not force a fresh session")
	}
}

func TestStepReview_RecordsSummaryAndStales(t *testing.T) {
	h := newHarness(t)
	// Downstream coverage already exists; the review turn changes code.
	setArtifactPresent(h.item, flowsdk.ArtifactCoverage)
	h.item.Artifacts = append(h.item.Artifacts, flowsdk.ItemArtifact{Key: flowsdk.ArtifactCoverage, Required: true})
	h.gitStatus.Modified = 0
	// Make before/after differ: status starts at Modified 0, the agent "edits".
	h.agentResp.LastText = "reviewed, fixed a bug"
	// Flip the runner status after the turn by mutating gitStatus mid-flight is
	// hard; instead start Modified=1 so before==after==1 (no change) — assert the
	// summary is recorded regardless.
	h.gitStatus.Modified = 1
	res := h.f.stepReview()
	if res.Status != flowsdk.StepDone {
		t.Fatalf("status = %q, want done", res.Status)
	}
	if h.item.ReviewSummary != "reviewed, fixed a bug" {
		t.Errorf("review_summary = %q", h.item.ReviewSummary)
	}
}

func TestStepCoverage_ParsesRating(t *testing.T) {
	h := newHarness(t)
	h.agentResp.LastText = "added tests.\nCOVERAGE: insufficient"
	res := h.f.stepCoverage()
	if res.Status != flowsdk.StepDone {
		t.Fatalf("status = %q, want done", res.Status)
	}
	if h.item.TestCoverage != flowsdk.CoverageInsufficient {
		t.Errorf("coverage = %q, want insufficient", h.item.TestCoverage)
	}
}

func TestStepCommit_HappyPath(t *testing.T) {
	h := newHarness(t)
	res := h.f.stepCommit()
	if res.Status != flowsdk.StepDone {
		t.Fatalf("status = %q (%s), want done", res.Status, res.Error)
	}
	if h.item.CommitHash != "committed1" {
		t.Errorf("commit = %q, want committed1", h.item.CommitHash)
	}
}

func TestStepCommit_VerifyFails(t *testing.T) {
	h := newHarness(t)
	h.verifyOK = false
	res := h.f.stepCommit()
	if res.Status != flowsdk.StepFailed {
		t.Fatalf("status = %q, want failed when verify fails", res.Status)
	}
	if h.item.CommitHash != "" {
		t.Error("must not commit when verify fails")
	}
}

func TestStepPush_PushesWhenAhead(t *testing.T) {
	h := newHarness(t)
	h.gitStatus.Ahead = 1
	h.gitStatus.LastCommit = "abc"
	res := h.f.stepPush()
	if res.Status != flowsdk.StepDone {
		t.Fatalf("status = %q (%s), want done", res.Status, res.Error)
	}
	if h.item.PushedHash != "abc" {
		t.Errorf("push = %q, want abc (the pushed commit hash)", h.item.PushedHash)
	}
}

func TestStepPush_NothingToPush(t *testing.T) {
	h := newHarness(t)
	h.gitStatus.Ahead = 0
	h.gitStatus.LastCommit = "abc"
	res := h.f.stepPush()
	if res.Status != flowsdk.StepDone {
		t.Fatalf("status = %q, want done (nothing to push is success)", res.Status)
	}
	if h.item.PushedHash == "" {
		t.Error("push artifact should be recorded even when nothing to push")
	}
}

func TestStepPush_RejectedMarksCommitStale(t *testing.T) {
	h := newHarness(t)
	h.gitStatus.Ahead = 1
	h.gitStatus.LastCommit = "abc"
	h.pushOK = false // the remote advanced — push is rejected
	// The commit artifact is present (the smart commit step produced it).
	h.item.CommitHash = "abc"
	h.item.Artifacts = []flowsdk.ItemArtifact{{Key: flowsdk.ArtifactCommit, Required: true}}

	res := h.f.stepPush()
	if res.Status != flowsdk.StepFailed {
		t.Fatalf("status = %q, want failed on a rejected push", res.Status)
	}
	if a := h.item.Artifact(flowsdk.ArtifactCommit); a == nil || !a.Stale {
		t.Error("a rejected push must mark the commit artifact stale (to rebase + re-commit)")
	}
	if h.item.PushedHash != "" {
		t.Error("the push artifact must not be recorded on a rejected push")
	}
}

func TestStepPush_VerifyFailsReactivatesCommit(t *testing.T) {
	h := newHarness(t)
	h.gitStatus.Ahead = 1
	h.gitStatus.LastCommit = "abc"
	h.verifyOK = false // bin/verify --wasm fails on the rebased state — must NOT reach origin
	h.item.CommitHash = "abc"
	h.item.Artifacts = []flowsdk.ItemArtifact{{Key: flowsdk.ArtifactCommit, Required: true}}

	res := h.f.stepPush()
	if res.Status != flowsdk.StepFailed {
		t.Fatalf("status = %q, want failed when the pre-push verify fails", res.Status)
	}
	if a := h.item.Artifact(flowsdk.ArtifactCommit); a == nil || !a.Stale {
		t.Error("a failed pre-push verify must reactivate the commit step (mark it stale)")
	}
	if h.item.PushedHash != "" {
		t.Error("must not push (or record a push) when the pre-push verify fails")
	}
}

// TestStepCommit_RebaseFailsNonConflict covers a rebase that fails WITHOUT leaving
// an in-flight conflict (no in-progress rebase / no unmerged paths) — e.g. no
// upstream. Nothing for an agent to resolve, so the step fails directly with no
// agent turn.
func TestStepCommit_RebaseFailsNonConflict(t *testing.T) {
	h := newHarness(t)
	h.rebaseOK = false // rebase fails, but the worktree shows no conflict state
	res := h.f.stepCommit()
	if res.Status != flowsdk.StepFailed {
		t.Fatalf("status = %q, want failed on a non-conflict rebase failure", res.Status)
	}
	if h.item.CommitHash != "" {
		t.Error("commit artifact must not be recorded when the rebase fails")
	}
	if h.agentCalls != 0 {
		t.Errorf("agentCalls = %d, want 0 — a non-conflict rebase failure must not spend an agent turn", h.agentCalls)
	}
}

// TestStepCommit_RebaseConflictResolved covers the smart-resolution path: the rebase
// stops on a conflict (in-progress rebase + unmerged paths), and the agent turn
// resolves it, finishes the rebase, and re-verifies. The step then records the commit.
func TestStepCommit_RebaseConflictResolved(t *testing.T) {
	h := newHarness(t)
	h.rebaseOK = false
	h.gitStatus.Conflicts = true
	h.gitStatus.GitInProgress = "rebase" // the runner left the rebase in progress
	// The resolving agent turn finishes the rebase and re-verifies: clear the conflict
	// state and write a passing verify marker.
	h.onAgentTurn = func() {
		h.gitStatus.Conflicts = false
		h.gitStatus.GitInProgress = ""
		h.writeVerifyMark(true)
	}
	res := h.f.stepCommit()
	if res.Status != flowsdk.StepDone {
		t.Fatalf("status = %q (%s), want done after the conflict was resolved", res.Status, res.Error)
	}
	if h.agentCalls == 0 {
		t.Error("a rebase conflict must drive an agent turn to resolve it")
	}
	if h.item.CommitHash == "" {
		t.Error("commit artifact must be recorded after a resolved rebase conflict")
	}
}

// TestStepCommit_RebaseConflictUnresolvedParks covers a conflict the agent never
// resolves: the in-flight rebase state persists across every attempt. The step
// exhausts its budget, parks the item, and records no commit.
func TestStepCommit_RebaseConflictUnresolvedParks(t *testing.T) {
	h := newHarness(t)
	h.rebaseOK = false
	h.gitStatus.Conflicts = true
	h.gitStatus.GitInProgress = "rebase"
	h.onAgentTurn = func() {} // the agent never resolves it — conflict state persists
	res := h.f.stepCommit()
	if res.Status != flowsdk.StepFailed {
		t.Fatalf("status = %q, want failed when the conflict can't be resolved", res.Status)
	}
	if h.parked == nil {
		t.Error("an unresolvable rebase conflict must park the item")
	}
	if h.item.CommitHash != "" {
		t.Error("commit artifact must not be recorded when the conflict is unresolved")
	}
	if h.agentCalls != maxRebaseResolveAttempts {
		t.Errorf("agentCalls = %d, want %d (one initial turn + reprompts)", h.agentCalls, maxRebaseResolveAttempts)
	}
}

func TestStepInspection_RecordsVerdictAndSuggestions(t *testing.T) {
	h := newHarness(t)
	h.agentResp.LastText = "ok.\n```json\n" + `{"verdict":"pass","quality":"good","completeness":"full","summary":"clean","suggestions":[{"title":"follow up","type":"task","key":"k1"}]}` + "\n```"
	res := h.f.stepInspection()
	if res.Status != flowsdk.StepDone {
		t.Fatalf("status = %q, want done", res.Status)
	}
	if h.item.Inspection == nil || h.item.Inspection.Verdict != flowsdk.VerdictPass {
		t.Errorf("inspection = %+v, want pass", h.item.Inspection)
	}
	if len(h.item.Suggestions) != 1 {
		t.Errorf("got %d suggestions, want 1", len(h.item.Suggestions))
	}
}

func TestPrepareWorktree_SkipsWhenImplementationPresent(t *testing.T) {
	h := newHarness(t)
	setArtifactPresent(h.item, flowsdk.ArtifactImplementation)
	h.f.item = h.item
	// Post-implementation tree is intentionally dirty: the preamble must not touch
	// it and must let the step proceed (nil), even for the plan step.
	if res := h.f.prepareWorktree(flowsdk.ArtifactPlan); res != nil {
		t.Errorf("should skip (return nil) when implementation is present, got %+v", *res)
	}
	if h.parked != nil {
		t.Error("must not park when implementation is present")
	}
}

func TestPrepareWorktree_PlanDirtyParksTransientRelease(t *testing.T) {
	h := newHarness(t)
	h.item.Type = flowsdk.ItemPlan
	h.f.item = h.item
	h.gitStatus.Clean = false // dirty arena before planning
	res := h.f.prepareWorktree(flowsdk.ArtifactPlan)
	if res == nil || res.Status != flowsdk.StepFailed {
		t.Fatalf("dirty plan arena must not start the step; got %+v", res)
	}
	if h.parked == nil {
		t.Fatal("dirty plan arena must park")
	}
	if h.parked.Kind != flowsdk.FlowFailureArenaSetup || !h.parked.Transient || !h.parked.ReleaseLease {
		t.Errorf("park should be transient arena-setup with release_lease, got %+v", *h.parked)
	}
}

func TestPrepareWorktree_PlanCleanSyncsAndProceeds(t *testing.T) {
	h := newHarness(t)
	h.item.Type = flowsdk.ItemPlan
	h.f.item = h.item
	h.gitStatus = flowsdk.GitStatus{Branch: "master", Clean: true, Behind: 2, LastCommit: "base1"}
	if res := h.f.prepareWorktree(flowsdk.ArtifactPlan); res != nil {
		t.Errorf("clean (behind) plan arena should sync and proceed (nil), got %+v", *res)
	}
	if h.parked != nil {
		t.Errorf("clean plan arena must not park, got %+v", *h.parked)
	}
}

func TestPrepareWorktree_NonPlanDirtyProceeds(t *testing.T) {
	h := newHarness(t)
	// A task's pre-implementation step stays best-effort: a dirty tree must not
	// block it (and must never park/release — that could discard partial work).
	h.gitStatus.Clean = false
	if res := h.f.prepareWorktree(flowsdk.ArtifactImplementation); res != nil {
		t.Errorf("non-plan dirty tree should proceed (nil), got %+v", *res)
	}
	if h.parked != nil {
		t.Error("non-plan step must not park on a dirty tree")
	}
}

func TestStep_UnknownKey(t *testing.T) {
	h := newHarness(t)
	res := h.f.step(flowsdk.ArtifactKey("bogus"))
	if res.Status != flowsdk.StepFailed {
		t.Fatalf("unknown step status = %q, want failed", res.Status)
	}
}

func TestStepPhases_FilesAndRecords(t *testing.T) {
	h := newHarness(t)
	h.item.Type = flowsdk.ItemPlan
	// The agent files the phase tasks + sets the plan's phases during its turn.
	h.onAgentTurn = func() { h.item.Phases = []flowsdk.ItemID{"T0002", "T0003"} }
	res := h.f.stepPhases()
	if res.Status != flowsdk.StepDone {
		t.Fatalf("status = %q (%s), want done", res.Status, res.Error)
	}
	if !h.item.ArtifactPresent(flowsdk.ArtifactPhases) {
		t.Error("phases artifact should be present once the phases are filed")
	}
}

func TestStepPhases_NoPhasesFiledFails(t *testing.T) {
	h := newHarness(t)
	h.item.Type = flowsdk.ItemPlan
	// Agent turn succeeds but files nothing → the step fails (tracker retries).
	res := h.f.stepPhases()
	if res.Status != flowsdk.StepFailed {
		t.Fatalf("status = %q, want failed when no phases were filed", res.Status)
	}
}

func TestRun_PlanSeedsPlanSubset(t *testing.T) {
	h := newHarness(t)
	h.item.Type = flowsdk.ItemPlan
	h.f.item = h.item
	h.gitStatus.Clean = true // plan step requires a clean, current tree to start
	res := h.f.run()         // first run seeds the checklist + runs the plan step
	if res.Status == flowsdk.StepFailed {
		t.Fatalf("plan run failed: %s", res.Error)
	}
	// A plan seeds ONLY the plan subset — never the code-lifecycle artifacts.
	for _, k := range h.item.Artifacts {
		if k.Key == flowsdk.ArtifactImplementation || k.Key == flowsdk.ArtifactCommit {
			t.Errorf("plan must not seed code artifact %q", k.Key)
		}
	}
	if h.item.Artifact(flowsdk.ArtifactPhases) == nil {
		t.Error("plan should seed the phases artifact")
	}
}

func TestStepReview_CodeChangedReopensDownstream(t *testing.T) {
	h := newHarness(t)
	// Downstream coverage already produced; review changes code → re-open it.
	setArtifactPresent(h.item, flowsdk.ArtifactCoverage)
	h.item.Artifacts = append(h.item.Artifacts, flowsdk.ItemArtifact{Key: flowsdk.ArtifactCoverage, Required: true})
	calls := 0
	h.statusFn = func() flowsdk.GitStatus {
		calls++
		if calls == 1 {
			return flowsdk.GitStatus{Modified: 0, LastCommit: "x"} // before
		}
		return flowsdk.GitStatus{Modified: 3, LastCommit: "x"} // after — the agent edited
	}
	res := h.f.stepReview()
	if res.Status != flowsdk.StepDone {
		t.Fatalf("status = %q, want done", res.Status)
	}
	if !staleMarked(h, flowsdk.ArtifactCoverage) {
		t.Error("coverage should be marked stale after a code-changing review")
	}
}

func TestMarkPresentStale_OnlyPresent(t *testing.T) {
	h := newHarness(t)
	setArtifactPresent(h.item, flowsdk.ArtifactReview) // content present
	h.item.Artifacts = append(h.item.Artifacts, flowsdk.ItemArtifact{Key: flowsdk.ArtifactReview, Required: true})
	// commit is absent. markPresentStale should mark review but not commit.
	h.f.item = h.item
	h.f.markPresentStale(flowsdk.ArtifactReview, flowsdk.ArtifactCommit)
	if !staleMarked(h, flowsdk.ArtifactReview) {
		t.Error("present review should be marked stale")
	}
	// Verify the PATCH body listed only review.
	var sawCommit bool
	for _, b := range h.patched {
		if raw, ok := b["mark_stale"]; ok {
			var keys []flowsdk.ArtifactKey
			json.Unmarshal(raw, &keys)
			for _, k := range keys {
				if k == flowsdk.ArtifactCommit {
					sawCommit = true
				}
			}
		}
	}
	if sawCommit {
		t.Error("absent commit must not be marked stale")
	}
}

func TestMarkPresentStale_NonePresentIsNoop(t *testing.T) {
	h := newHarness(t)
	h.f.item = h.item
	h.f.markPresentStale(flowsdk.ArtifactReview, flowsdk.ArtifactCommit)
	for _, b := range h.patched {
		if _, ok := b["mark_stale"]; ok {
			t.Error("no PATCH should be sent when nothing downstream is present")
		}
	}
}

func staleMarked(h *harness, key flowsdk.ArtifactKey) bool {
	a := h.item.Artifact(key)
	return a != nil && a.Stale
}

func TestStep_DispatchesToExecutor(t *testing.T) {
	// Each artifact key routes to its executor; verify the dispatch + reported step.
	for _, key := range []flowsdk.ArtifactKey{
		flowsdk.ArtifactPlan, flowsdk.ArtifactImplementation, flowsdk.ArtifactReview,
		flowsdk.ArtifactCoverage, flowsdk.ArtifactCommit, flowsdk.ArtifactPush, flowsdk.ArtifactInspection,
	} {
		t.Run(string(key), func(t *testing.T) {
			h := newHarness(t)
			h.item.Plan = "the plan" // implement step requires a plan prerequisite
			h.gitStatus.Ahead = 1
			h.agentResp.LastText = "ok.\n```json\n{\"verdict\":\"pass\"}\n```\nCOVERAGE: adequate"
			res := h.f.step(key)
			if res.Step != flowsdk.StepName(key) {
				t.Errorf("step(%s) reported step %q", key, res.Step)
			}
			if res.Status != flowsdk.StepDone {
				t.Errorf("step(%s) = %q (%s), want done", key, res.Status, res.Error)
			}
		})
	}
}

func TestStepCoverage_CodeChangedReopensDownstream(t *testing.T) {
	h := newHarness(t)
	setArtifactPresent(h.item, flowsdk.ArtifactCommit)
	h.item.Artifacts = append(h.item.Artifacts, flowsdk.ItemArtifact{Key: flowsdk.ArtifactCommit, Required: true})
	h.agentResp.LastText = "added tests\nCOVERAGE: adequate"
	calls := 0
	h.statusFn = func() flowsdk.GitStatus {
		calls++
		if calls == 1 {
			return flowsdk.GitStatus{Modified: 0, LastCommit: "x"}
		}
		return flowsdk.GitStatus{Modified: 5, LastCommit: "x"}
	}
	res := h.f.stepCoverage()
	if res.Status != flowsdk.StepDone {
		t.Fatalf("status = %q, want done", res.Status)
	}
	if !staleMarked(h, flowsdk.ArtifactCommit) {
		t.Error("commit should be marked stale after a code-changing coverage step")
	}
}

func TestCmdRun_EndToEnd(t *testing.T) {
	h := newHarness(t)
	h.gitStatus.Clean = true // plan step (first step) requires a clean, current tree
	// Write a real .flow/context.json lease in a temp worktree pointing at the
	// mock servers, then drive the actual cmdRun entry point.
	wt := t.TempDir()
	c := flowsdk.Context{
		TrackerURL: h.f.tr.BaseURL, RunnerURL: h.f.rn.BaseURL,
		RunnerToken: "n", ItemID: "T0001", InvocationID: "inv1", FlowName: flowName, Agent: "tester",
	}
	if err := c.Save(wt); err != nil {
		t.Fatal(err)
	}
	t.Setenv(flowsdk.EnvWorktree, wt)
	t.Setenv(flowsdk.EnvContextPath, wt+"/"+flowsdk.ContextRelPath)
	if code := cmdRun(); code != 0 {
		t.Fatalf("cmdRun exit = %d, want 0", code)
	}
	if h.item.Plan == "" {
		t.Error("cmdRun should have produced the plan artifact")
	}
}

func TestCmdRun_NoContextErrors(t *testing.T) {
	t.Setenv(flowsdk.EnvContextPath, t.TempDir()+"/missing.json")
	t.Setenv(flowsdk.EnvTrackerURL, "")
	t.Setenv(flowsdk.EnvItemID, "")
	t.Setenv(flowsdk.EnvWorktree, t.TempDir())
	if code := cmdRun(); code == 0 {
		t.Error("cmdRun with no lease should exit non-zero")
	}
}

func TestCmdStatus_AgainstMockTracker(t *testing.T) {
	t.Chdir(t.TempDir()) // hermetic: don't discover a real worktree/runner up the tree
	h := newHarness(t)
	setArtifactPresent(h.item, flowsdk.ArtifactPlan)
	h.item.Artifacts = append(h.item.Artifacts, flowsdk.ItemArtifact{Key: flowsdk.ArtifactPlan, Required: true})
	trURL := h.f.tr.BaseURL
	t.Setenv(flowsdk.EnvTrackerURL, trURL)
	t.Setenv(flowsdk.EnvItemID, "T0001")
	t.Setenv(flowsdk.EnvContextPath, t.TempDir()+"/nope.json") // force file-less load
	if code := cmdStatus([]string{"T0001", "--json"}); code != 0 {
		t.Fatalf("cmdStatus exit = %d, want 0", code)
	}
}
