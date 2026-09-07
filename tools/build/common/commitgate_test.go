package common

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// fp is a helper to create *float64 values for test baselines.
func fp(v float64) *float64 { return &v }

// testPlatform returns the current runtime platform string used by CheckCommitGate.
func testPlatform() string { return runtime.GOOS + "-" + runtime.GOARCH }

// setupGateTest creates a temp directory with baselines.json and a gate values
// sidecar, returning the root path. Uses the runtime platform for baselines.
func setupGateTest(t *testing.T, baselines Baselines, gv *GateValues) string {
	t.Helper()
	// A git repo: CheckCommitGate identifies the worktree via WorktreeHash,
	// which asks git for the project's file set.
	root := initBareGitRepo(t)

	// Write baselines.json.
	gatesDir := filepath.Join(root, "tools", "gates")
	os.MkdirAll(gatesDir, 0o755)
	data, _ := json.MarshalIndent(baselines, "", "  ")
	data = append(data, '\n')
	os.WriteFile(filepath.Join(gatesDir, "baselines.json"), data, 0o644)

	// Write the gate values sidecar through the real writer, so the tree
	// identity is stamped exactly as verify stamps it. Both files this helper
	// creates are excluded from the hash, so the write order does not matter.
	os.MkdirAll(filepath.Join(root, ".promise-home"), 0o755)
	gv.Timestamp = time.Now().UTC().Format(time.RFC3339)
	if gv.Platform == "" {
		gv.Platform = testPlatform()
	}
	if err := WriteGateValues(root, gv); err != nil {
		t.Fatalf("write gate values: %v", err)
	}

	return root
}

func TestCheckCommitGate_AllMatch(t *testing.T) {
	p := testPlatform()
	baselines := Baselines{
		p: {
			"host_test_count":    {Value: fp(100), Direction: "up", Updated: "2026-04-06"},
			"host_leak_count":    {Value: fp(0), Direction: "down", Updated: "2026-04-06"},
			"host_test_failures": {Value: fp(0), Direction: "exact", Updated: "2026-04-06"},
		},
	}
	gv := &GateValues{
		Values: map[string]float64{
			"host_test_count":    100,
			"host_leak_count":    0,
			"host_test_failures": 0,
		},
	}
	root := setupGateTest(t, baselines, gv)

	err := CheckCommitGate(root)
	if err != nil {
		t.Fatalf("expected nil error, got: %v", err)
	}
}

func TestCheckCommitGate_Improvement(t *testing.T) {
	p := testPlatform()
	baselines := Baselines{
		p: {
			"host_test_count": {Value: fp(100), Direction: "up", Updated: "2026-04-06"},
		},
	}
	gv := &GateValues{
		Values: map[string]float64{
			"host_test_count": 110,
		},
	}
	root := setupGateTest(t, baselines, gv)

	err := CheckCommitGate(root)
	if err != nil {
		t.Fatalf("expected nil error, got: %v", err)
	}

	// Verify baselines were updated.
	updated, err := LoadBaselines(root)
	if err != nil {
		t.Fatalf("load updated baselines: %v", err)
	}
	bl := updated[p]["host_test_count"]
	if bl.Value == nil || *bl.Value != 110 {
		t.Errorf("baseline value = %v, want 110", bl.Value)
	}
}

func TestCheckCommitGate_RegressionBlocked(t *testing.T) {
	p := testPlatform()
	baselines := Baselines{
		p: {
			"host_test_count": {Value: fp(100), Direction: "up", Updated: "2026-04-06"},
		},
	}
	gv := &GateValues{
		Values: map[string]float64{
			"host_test_count": 95,
		},
	}
	root := setupGateTest(t, baselines, gv)

	err := CheckCommitGate(root)
	if err == nil {
		t.Fatal("expected error for regression, got nil")
	}
}

func TestCheckCommitGate_ExactMetricRegression(t *testing.T) {
	p := testPlatform()
	baselines := Baselines{
		p: {
			"host_test_failures": {Value: fp(0), Direction: "exact", Updated: "2026-04-06"},
		},
	}
	gv := &GateValues{
		Values: map[string]float64{
			"host_test_failures": 1,
		},
	}
	root := setupGateTest(t, baselines, gv)

	err := CheckCommitGate(root)
	if err == nil {
		t.Fatal("expected error for exact metric regression, got nil")
	}
}

func TestCheckCommitGate_LeakIncrease(t *testing.T) {
	p := testPlatform()
	baselines := Baselines{
		p: {
			"host_leak_count": {Value: fp(0), Direction: "down", Updated: "2026-04-06"},
		},
	}
	gv := &GateValues{
		Values: map[string]float64{
			"host_leak_count": 2,
		},
	}
	root := setupGateTest(t, baselines, gv)

	err := CheckCommitGate(root)
	if err == nil {
		t.Fatal("expected error for leak increase, got nil")
	}
}

func TestCheckCommitGate_UnknownPlatformCreatesEntries(t *testing.T) {
	// Baselines exist only for a different platform — current platform has no
	// entry. CheckCommitGate should auto-register gate values as informational.
	baselines := Baselines{
		"other-platform": {
			"host_test_count": {Value: fp(100), Direction: "up", Updated: "2026-04-06"},
		},
	}
	gv := &GateValues{
		Values: map[string]float64{
			"host_test_count": 50,
		},
	}
	root := setupGateTest(t, baselines, gv)

	err := CheckCommitGate(root)
	if err != nil {
		t.Fatalf("expected nil for unknown platform auto-register, got: %v", err)
	}

	// Verify auto-registration under current platform.
	updated, err := LoadBaselines(root)
	if err != nil {
		t.Fatalf("load updated baselines: %v", err)
	}
	p := testPlatform()
	bl, ok := updated[p]["host_test_count"]
	if !ok {
		t.Fatal("expected host_test_count to be auto-registered")
	}
	if bl.Type != "informational" {
		t.Errorf("type = %q, want informational", bl.Type)
	}
}

// TestCheckCommitGate_AgeDoesNotMatter is the reported false negative (T1962):
// verify blessed a tree, nothing changed, and the commit was refused purely
// because the clock had moved on.
func TestCheckCommitGate_AgeDoesNotMatter(t *testing.T) {
	p := testPlatform()
	baselines := Baselines{p: {"host_test_count": {Value: fp(100), Direction: "up"}}}
	gv := &GateValues{Values: map[string]float64{"host_test_count": 100}}
	root := setupGateTest(t, baselines, gv)

	// Age the sidecar by two hours without touching the tree.
	gvPath := filepath.Join(root, ".promise-home", gateValuesFile)
	old := time.Now().Add(-2 * time.Hour)
	os.Chtimes(gvPath, old, old)

	if err := CheckCommitGate(root); err != nil {
		t.Fatalf("expected an untouched tree to pass at any age, got: %v", err)
	}
}

// TestCheckCommitGate_WorktreeChanged is the false positive the time window
// silently admitted: a source edit made after verify, committed inside the
// old 10-minute window.
func TestCheckCommitGate_WorktreeChanged(t *testing.T) {
	p := testPlatform()
	baselines := Baselines{p: {"host_test_count": {Value: fp(100), Direction: "up"}}}
	gv := &GateValues{Values: map[string]float64{"host_test_count": 100}}
	root := setupGateTest(t, baselines, gv)

	// Edit a source file after the values were produced.
	os.WriteFile(filepath.Join(root, "source.txt"), []byte("edited"), 0o644)

	err := CheckCommitGate(root)
	if err == nil {
		t.Fatal("expected the gate to reject values describing a different tree, got nil")
	}
	if !strings.Contains(err.Error(), "worktree changed") {
		t.Errorf("expected 'worktree changed' in error, got: %v", err)
	}
}

// TestCheckCommitGate_BaselineRewriteDoesNotInvalidate pins the baselines.json
// exclusion from the worktree hash: a passing gate rewrites baselines on an
// improvement, so a second run over the same tree must still pass rather than
// report that the tree changed under it.
func TestCheckCommitGate_BaselineRewriteDoesNotInvalidate(t *testing.T) {
	p := testPlatform()
	baselines := Baselines{p: {"host_test_count": {Value: fp(100), Direction: "up"}}}
	gv := &GateValues{Values: map[string]float64{"host_test_count": 110}}
	root := setupGateTest(t, baselines, gv)

	if err := CheckCommitGate(root); err != nil {
		t.Fatalf("first run: %v", err)
	}
	if err := CheckCommitGate(root); err != nil {
		t.Fatalf("second run after the gate rewrote baselines.json: %v", err)
	}
}

func TestCheckRatchet(t *testing.T) {
	tests := []struct {
		dir      string
		baseline float64
		actual   float64
		want     bool
	}{
		{"up", 100, 100, true},
		{"up", 100, 110, true},
		{"up", 100, 95, false},
		{"down", 5, 5, true},
		{"down", 5, 3, true},
		{"down", 5, 8, false},
		{"exact", 0, 0, true},
		{"exact", 0, 1, false},
		{"exact", 5, 5, true},
		{"unknown", 0, 99, true},
	}
	for _, tt := range tests {
		got := checkRatchet(tt.dir, tt.baseline, tt.actual)
		if got != tt.want {
			t.Errorf("checkRatchet(%q, %v, %v) = %v, want %v",
				tt.dir, tt.baseline, tt.actual, got, tt.want)
		}
	}
}

func TestCheckCommitGate_PendingPopulated(t *testing.T) {
	p := testPlatform()
	baselines := Baselines{
		p: {
			"host_test_count": {Direction: "up"}, // Pending: no Value
		},
	}
	gv := &GateValues{
		Values: map[string]float64{
			"host_test_count": 200,
		},
	}
	root := setupGateTest(t, baselines, gv)

	err := CheckCommitGate(root)
	if err != nil {
		t.Fatalf("expected nil error for pending populate, got: %v", err)
	}

	// Verify value was populated.
	updated, err := LoadBaselines(root)
	if err != nil {
		t.Fatalf("load updated baselines: %v", err)
	}
	bl := updated[p]["host_test_count"]
	if bl.Value == nil || *bl.Value != 200 {
		t.Errorf("baseline value = %v, want 200", bl.Value)
	}
}

func TestCheckCommitGate_InformationalIgnored(t *testing.T) {
	p := testPlatform()
	baselines := Baselines{
		p: {
			"some_metric": {Type: "informational"},
		},
	}
	gv := &GateValues{
		Values: map[string]float64{
			"some_metric": 999,
		},
	}
	root := setupGateTest(t, baselines, gv)

	err := CheckCommitGate(root)
	if err != nil {
		t.Fatalf("expected nil for informational metric, got: %v", err)
	}
}

func TestCheckCommitGate_UnknownValueAutoRegistered(t *testing.T) {
	p := testPlatform()
	baselines := Baselines{
		p: {
			"host_test_count": {Value: fp(100), Direction: "up", Updated: "2026-04-06"},
		},
	}
	gv := &GateValues{
		Values: map[string]float64{
			"host_test_count": 100,
			"new_metric":      42,
		},
	}
	root := setupGateTest(t, baselines, gv)

	err := CheckCommitGate(root)
	if err != nil {
		t.Fatalf("expected nil for auto-registered metric, got: %v", err)
	}

	// Verify new metric was auto-registered as informational.
	updated, err := LoadBaselines(root)
	if err != nil {
		t.Fatalf("load updated baselines: %v", err)
	}
	bl, ok := updated[p]["new_metric"]
	if !ok {
		t.Fatal("expected new_metric to be registered")
	}
	if bl.Type != "informational" {
		t.Errorf("type = %q, want informational", bl.Type)
	}
}

// startExceptionServer spins up an httptest server that returns the supplied
// exceptions on /api/gate-exceptions. Cleanup resets trackerURLOverride.
func startExceptionServer(t *testing.T, exceptions []GateException) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/gate-exceptions" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(exceptions)
	}))
	prev := trackerURLOverride
	trackerURLOverride = srv.URL
	t.Cleanup(func() {
		trackerURLOverride = prev
		srv.Close()
	})
	return srv
}

func TestMatchExceptionPlatform(t *testing.T) {
	cases := []struct {
		excPlatform string
		currentOS   string
		want        bool
	}{
		{"darwin", "darwin", true},
		{"linux", "darwin", false},
		{"*", "darwin", true},
		{"*", "linux", true},
		{"", "darwin", false},
	}
	for _, c := range cases {
		got := matchExceptionPlatform(c.excPlatform, c.currentOS)
		if got != c.want {
			t.Errorf("matchExceptionPlatform(%q, %q) = %v, want %v",
				c.excPlatform, c.currentOS, got, c.want)
		}
	}
}

func TestCheckCommitGate_ExceptionGranted(t *testing.T) {
	p := testPlatform()
	startExceptionServer(t, []GateException{{
		Gate:     "commitgate",
		Metric:   "host_test_count",
		Platform: runtime.GOOS,
		BugID:    "B0123",
		Reason:   "known regression",
	}})
	baselines := Baselines{
		p: {"host_test_count": {Value: fp(100), Direction: "up", Updated: "2026-04-06"}},
	}
	gv := &GateValues{Values: map[string]float64{"host_test_count": 95}}
	root := setupGateTest(t, baselines, gv)

	if err := CheckCommitGate(root); err != nil {
		t.Fatalf("expected nil error (regression excepted), got: %v", err)
	}
}

func TestCheckCommitGate_ExceptionPlatformWildcard(t *testing.T) {
	p := testPlatform()
	startExceptionServer(t, []GateException{{
		Gate:     "commitgate",
		Metric:   "host_test_count",
		Platform: "*",
		BugID:    "B0124",
		Reason:   "wildcard exception",
	}})
	baselines := Baselines{
		p: {"host_test_count": {Value: fp(100), Direction: "up", Updated: "2026-04-06"}},
	}
	gv := &GateValues{Values: map[string]float64{"host_test_count": 90}}
	root := setupGateTest(t, baselines, gv)

	if err := CheckCommitGate(root); err != nil {
		t.Fatalf("expected nil error (wildcard exception), got: %v", err)
	}
}

func TestCheckCommitGate_ExceptionPlatformMismatch(t *testing.T) {
	p := testPlatform()
	otherOS := "linux"
	if runtime.GOOS == "linux" {
		otherOS = "darwin"
	}
	startExceptionServer(t, []GateException{{
		Gate:     "commitgate",
		Metric:   "host_test_count",
		Platform: otherOS,
		BugID:    "B0125",
		Reason:   "wrong-platform exception",
	}})
	baselines := Baselines{
		p: {"host_test_count": {Value: fp(100), Direction: "up", Updated: "2026-04-06"}},
	}
	gv := &GateValues{Values: map[string]float64{"host_test_count": 95}}
	root := setupGateTest(t, baselines, gv)

	if err := CheckCommitGate(root); err == nil {
		t.Fatal("expected error (platform mismatch should NOT except), got nil")
	}
}

func TestCheckCommitGate_TrackerUnreachable(t *testing.T) {
	prev := trackerURLOverride
	trackerURLOverride = ""
	t.Cleanup(func() { trackerURLOverride = prev })

	p := testPlatform()
	baselines := Baselines{
		p: {"host_test_count": {Value: fp(100), Direction: "up", Updated: "2026-04-06"}},
	}
	gv := &GateValues{Values: map[string]float64{"host_test_count": 95}}
	root := setupGateTest(t, baselines, gv)

	if err := CheckCommitGate(root); err == nil {
		t.Fatal("expected error (no exceptions available), got nil")
	}
}

func TestCheckCommitGate_ExceptionMixedWithHardFailure(t *testing.T) {
	p := testPlatform()
	// Server returns an exception only when queried for host_leak_count;
	// for any other metric it returns an empty list.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		metric := r.URL.Query().Get("metric")
		if metric == "host_leak_count" {
			_ = json.NewEncoder(w).Encode([]GateException{{
				Gate:     "commitgate",
				Metric:   "host_leak_count",
				Platform: runtime.GOOS,
				BugID:    "B0126",
				Reason:   "leak exception",
			}})
			return
		}
		_ = json.NewEncoder(w).Encode([]GateException{})
	}))
	t.Cleanup(srv.Close)
	prev := trackerURLOverride
	trackerURLOverride = srv.URL
	t.Cleanup(func() { trackerURLOverride = prev })

	baselines := Baselines{
		p: {
			"host_leak_count": {Value: fp(0), Direction: "down", Updated: "2026-04-06"},
			"host_test_count": {Value: fp(100), Direction: "up", Updated: "2026-04-06"},
		},
	}
	gv := &GateValues{Values: map[string]float64{
		"host_leak_count": 3,
		"host_test_count": 80,
	}}
	root := setupGateTest(t, baselines, gv)

	if err := CheckCommitGate(root); err == nil {
		t.Fatal("expected error (one hard failure remains), got nil")
	}
}

func TestFindTrackerURL_FromMcpJson(t *testing.T) {
	prev := trackerURLOverride
	trackerURLOverride = ""
	t.Cleanup(func() { trackerURLOverride = prev })

	root := t.TempDir()
	mcp := `{"mcpServers":{"tracker":{"type":"http","url":"http://example.test:9121/mcp"}}}`
	os.WriteFile(filepath.Join(root, ".mcp.json"), []byte(mcp), 0o644)

	got := findTrackerURL(root)
	if got != "http://example.test:9121" {
		t.Errorf("findTrackerURL = %q, want %q", got, "http://example.test:9121")
	}
}

func TestFindTrackerURL_Missing(t *testing.T) {
	prev := trackerURLOverride
	trackerURLOverride = ""
	t.Cleanup(func() { trackerURLOverride = prev })

	if got := findTrackerURL(t.TempDir()); got != "" {
		t.Errorf("findTrackerURL with no .mcp.json = %q, want empty", got)
	}
}

func TestFindTrackerURL_MalformedJson(t *testing.T) {
	prev := trackerURLOverride
	trackerURLOverride = ""
	t.Cleanup(func() { trackerURLOverride = prev })

	root := t.TempDir()
	os.WriteFile(filepath.Join(root, ".mcp.json"), []byte("{not json"), 0o644)
	if got := findTrackerURL(root); got != "" {
		t.Errorf("findTrackerURL with malformed json = %q, want empty", got)
	}
}

func TestFindTrackerURL_NoTrackerEntry(t *testing.T) {
	prev := trackerURLOverride
	trackerURLOverride = ""
	t.Cleanup(func() { trackerURLOverride = prev })

	root := t.TempDir()
	os.WriteFile(filepath.Join(root, ".mcp.json"),
		[]byte(`{"mcpServers":{"other":{"url":"http://x/mcp"}}}`), 0o644)
	if got := findTrackerURL(root); got != "" {
		t.Errorf("findTrackerURL without tracker entry = %q, want empty", got)
	}
}

func TestQueryGateExceptions_Non200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)
	if got := queryGateExceptions(srv.URL, "commitgate", "any"); got != nil {
		t.Errorf("queryGateExceptions on 500 = %v, want nil", got)
	}
}

func TestQueryGateExceptions_MalformedJson(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte("not json"))
	}))
	t.Cleanup(srv.Close)
	if got := queryGateExceptions(srv.URL, "commitgate", "any"); got != nil {
		t.Errorf("queryGateExceptions on malformed json = %v, want nil", got)
	}
}

func TestQueryGateExceptions_TransportError(t *testing.T) {
	// Closed server URL → connection refused → http.Get returns error.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	srv.Close()
	if got := queryGateExceptions(srv.URL, "commitgate", "any"); got != nil {
		t.Errorf("queryGateExceptions on transport error = %v, want nil", got)
	}
}

func TestCheckCommitGate_FloatValue(t *testing.T) {
	p := testPlatform()
	baselines := Baselines{
		p: {
			"coverage": {Value: fp(85.5), Direction: "up", Updated: "2026-04-06"},
		},
	}
	gv := &GateValues{
		Values: map[string]float64{
			"coverage": 86.2,
		},
	}
	root := setupGateTest(t, baselines, gv)

	err := CheckCommitGate(root)
	if err != nil {
		t.Fatalf("expected nil for float improvement, got: %v", err)
	}

	updated, err := LoadBaselines(root)
	if err != nil {
		t.Fatalf("load updated baselines: %v", err)
	}
	bl := updated[p]["coverage"]
	if bl.Value == nil || *bl.Value != 86.2 {
		t.Errorf("baseline value = %v, want 86.2", bl.Value)
	}
}

// TestCheckCommitGate_NoWorktreeIdentity covers the gate refusing to run at
// all when it cannot establish what tree it is looking at. Passing without an
// identity would be the old failure mode in a new shape: a gate that vouches
// for a tree it never identified.
func TestCheckCommitGate_NoWorktreeIdentity(t *testing.T) {
	// Clear TMPDIR for the same reason as TestWorktreeHash_NotAGitRepo: under
	// bin/verify it points inside the real work tree, where git ls-files works.
	t.Setenv("TMPDIR", "")
	root := t.TempDir()

	err := CheckCommitGate(root)
	if err == nil {
		t.Fatal("expected an error with no worktree identity available, got nil")
	}
	if !strings.Contains(err.Error(), "identify worktree") {
		t.Errorf("expected 'identify worktree' in error, got: %v", err)
	}
}

// TestCheckCommitGate_StagingDoesNotInvalidate is the workflow the commit
// skill relies on: verify, then git add, then the gate. Staging moves the
// index, not the worktree, so the gate must still pass — otherwise every
// commit would need a verify run wedged between `git add` and `git commit`.
func TestCheckCommitGate_StagingDoesNotInvalidate(t *testing.T) {
	p := testPlatform()
	baselines := Baselines{p: {"host_test_count": {Value: fp(100), Direction: "up", Updated: "2026-04-06"}}}
	gv := &GateValues{Values: map[string]float64{"host_test_count": 100}}
	root := setupGateTest(t, baselines, gv)
	os.WriteFile(filepath.Join(root, "source.txt"), []byte("verified content"), 0o644)

	// Re-stamp so the sidecar describes the tree including source.txt, as a
	// verify run over this tree would have.
	if err := WriteGateValues(root, gv); err != nil {
		t.Fatalf("write gate values: %v", err)
	}
	if err := RunIn(root, "git", "add", "-A"); err != nil {
		t.Fatalf("git add: %v", err)
	}

	if err := CheckCommitGate(root); err != nil {
		t.Fatalf("expected staging to leave the gate values valid, got: %v", err)
	}
}
