package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	flowsdk "djabi.dev/go/flow_sdk"
)

func TestCmdLeaseRequiresID(t *testing.T) {
	t.Setenv(flowsdk.EnvContextPath, filepath.Join(t.TempDir(), "none.json"))
	if code := cmdLease(nil); code != 2 {
		t.Errorf("no id should exit 2, got %d", code)
	}
}

func TestCmdLeaseMissingRunnerURL(t *testing.T) {
	t.Chdir(t.TempDir()) // hermetic: no worktree marker / runner-info up the tree
	t.Setenv(flowsdk.EnvContextPath, filepath.Join(t.TempDir(), "none.json"))
	if code := cmdLease([]string{"T0001"}); code != 1 {
		t.Errorf("missing runner url should exit 1, got %d", code)
	}
}

func TestCmdLeasePostsToRunner(t *testing.T) {
	t.Chdir(t.TempDir()) // hermetic: don't discover a real worktree/runner up the tree
	var gotBody flowsdk.LeaseRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/lease" {
			t.Errorf("path %q", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer trtok" {
			t.Errorf("auth %q", r.Header.Get("Authorization"))
		}
		json.NewDecoder(r.Body).Decode(&gotBody)
		json.NewEncoder(w).Encode(flowsdk.LeaseResponse{InvocationID: "manual_1", Worktree: "/wt"})
	}))
	defer srv.Close()

	t.Setenv(flowsdk.EnvContextPath, filepath.Join(t.TempDir(), "none.json"))
	t.Setenv(flowsdk.EnvRunnerURL, srv.URL)
	t.Setenv(flowsdk.EnvTrackerToken, "trtok")

	if code := cmdLease([]string{"T0042"}); code != 0 {
		t.Fatalf("lease exit %d", code)
	}
	if gotBody.ItemID != "T0042" || gotBody.Flow != flowName {
		t.Errorf("request body %+v", gotBody)
	}
}

func TestCmdReleasePostsToRunner(t *testing.T) {
	t.Chdir(t.TempDir()) // hermetic: don't discover a real worktree/runner up the tree
	var hit string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hit = r.URL.Path
		if r.Header.Get("Authorization") != "Bearer nonce1" {
			t.Errorf("auth %q", r.Header.Get("Authorization"))
		}
		json.NewEncoder(w).Encode(flowsdk.ReleaseResponse{Released: "manual_1"})
	}))
	defer srv.Close()

	ctxFile := filepath.Join(t.TempDir(), "context.json")
	data, _ := json.Marshal(flowsdk.Context{
		RunnerURL: srv.URL, RunnerToken: "nonce1", InvocationID: "manual_1", ItemID: "T0042",
	})
	if err := os.WriteFile(ctxFile, data, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(flowsdk.EnvContextPath, ctxFile)

	if code := cmdRelease(nil); code != 0 {
		t.Fatalf("release exit %d", code)
	}
	if hit != "/v1/run/manual_1/release" {
		t.Errorf("released via %q", hit)
	}
}

func TestCmdReleaseNoLease(t *testing.T) {
	t.Chdir(t.TempDir()) // hermetic: no worktree marker / runner-info up the tree
	t.Setenv(flowsdk.EnvContextPath, filepath.Join(t.TempDir(), "none.json"))
	if code := cmdRelease(nil); code != 1 {
		t.Errorf("no lease should exit 1, got %d", code)
	}
}

func TestEnsureLiveLease_ReLeasesWhenRunnerMoved(t *testing.T) {
	wt := t.TempDir()
	var gotReq flowsdk.LeaseRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/lease" {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		json.NewDecoder(r.Body).Decode(&gotReq)
		json.NewEncoder(w).Encode(flowsdk.LeaseResponse{
			InvocationID: "manual_new", RunnerURL: r.Host, RunnerToken: "newtok", Worktree: wt,
		})
	}))
	defer srv.Close()
	// The current runner publishes a NEW url (a restart since the lease was taken).
	if err := flowsdk.WriteRunnerInfo(wt, flowsdk.RunnerInfo{RunnerURL: srv.URL, Agent: "a1"}); err != nil {
		t.Fatal(err)
	}

	// The lease still points at the OLD (now-dead) runner.
	c := flowsdk.Context{
		ItemID: "T0414", FlowName: flowName, Worktree: wt,
		RunnerURL: "http://127.0.0.1:1", RunnerToken: "oldtok", InvocationID: "manual_old",
	}
	got, err := ensureLiveLease(c)
	if err != nil {
		t.Fatal(err)
	}
	if gotReq.ItemID != "T0414" || gotReq.Flow != flowName || gotReq.Worktree != wt {
		t.Errorf("re-lease request = %+v", gotReq)
	}
	if got.InvocationID != "manual_new" || got.RunnerToken != "newtok" {
		t.Errorf("context not refreshed from the lease response: %+v", got)
	}
}

func TestEnsureLiveLease_NoopWhenLeaseIsCurrent(t *testing.T) {
	wt := t.TempDir()
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(500)
	}))
	defer srv.Close()
	if err := flowsdk.WriteRunnerInfo(wt, flowsdk.RunnerInfo{RunnerURL: srv.URL}); err != nil {
		t.Fatal(err)
	}
	// The lease already points at the current runner → no re-lease.
	c := flowsdk.Context{
		ItemID: "T0414", FlowName: flowName, Worktree: wt,
		RunnerURL: srv.URL, RunnerToken: "tok", InvocationID: "manual_old",
	}
	got, err := ensureLiveLease(c)
	if err != nil {
		t.Fatal(err)
	}
	if called {
		t.Error("must not re-lease when the lease already points at the current runner")
	}
	if got.InvocationID != "manual_old" || got.RunnerURL != srv.URL {
		t.Errorf("context should be unchanged: %+v", got)
	}
}

func TestEnsureLiveLease_NoopWhenNoRunnerInfo(t *testing.T) {
	wt := t.TempDir() // no .runner-flow.json anywhere up the tree
	c := flowsdk.Context{
		ItemID: "T0414", FlowName: flowName, Worktree: wt,
		RunnerURL: "http://127.0.0.1:1", RunnerToken: "tok", InvocationID: "manual_old",
	}
	got, err := ensureLiveLease(c)
	if err != nil {
		t.Fatal(err)
	}
	if got.InvocationID != "manual_old" || got.RunnerURL != "http://127.0.0.1:1" {
		t.Errorf("should be a no-op without runner-info: %+v", got)
	}
}
