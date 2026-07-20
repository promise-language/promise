package common

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// TestPromiseTestTimeoutArgs verifies that the per-test timeout flags carry a
// 3× scale on WASM targets (T1334) while host runs keep the bare 10s default.
func TestPromiseTestTimeoutArgs(t *testing.T) {
	cases := []struct {
		target string
		want   []string
	}{
		{"", []string{"-timeout", "10"}},
		{"wasm32-wasi", []string{"-timeout", "10", "-timeout-scale", "3"}},
		{"wasm32-web", []string{"-timeout", "10", "-timeout-scale", "3"}},
	}
	for _, c := range cases {
		got := promiseTestTimeoutArgs(c.target)
		if !slices.Equal(got, c.want) {
			t.Errorf("promiseTestTimeoutArgs(%q) = %v, want %v", c.target, got, c.want)
		}
	}
}

// TestRunFlowsGoTests_NoFlowsModule verifies that RunFlowsGoTests skips without
// error when flows/go.mod does not exist.
func TestRunFlowsGoTests_NoFlowsModule(t *testing.T) {
	root := t.TempDir()
	skipped, err := RunFlowsGoTests(root)
	if err != nil {
		t.Fatalf("RunFlowsGoTests: unexpected error: %v", err)
	}
	if !skipped {
		t.Error("RunFlowsGoTests: expected skipped=true when flows/go.mod absent")
	}
}

// TestRunFlowsGoTests_NoSDK verifies that RunFlowsGoTests skips when flows/go.mod
// exists but flow-sdk/go.mod does not.
func TestRunFlowsGoTests_NoSDK(t *testing.T) {
	root := t.TempDir()
	flowsDir := filepath.Join(root, "flows")
	if err := os.MkdirAll(flowsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(flowsDir, "go.mod"), []byte("module example.com/flows\n\ngo 1.21\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// flow-sdk/ is absent — should skip with warning, not error.
	skipped, err := RunFlowsGoTests(root)
	if err != nil {
		t.Fatalf("RunFlowsGoTests: unexpected error: %v", err)
	}
	if !skipped {
		t.Error("RunFlowsGoTests: expected skipped=true when flow-sdk/go.mod absent")
	}
}

// TestRunToolsGoTests_TrivialModule verifies that RunToolsGoTests succeeds on a
// minimal Go module placed at tools/build/ inside a temp root. Using a temp
// module (rather than the real repo) avoids infinite recursion: running
// go test ./... on the real tools/build would re-invoke this test, spawning an
// unbounded chain of go test subprocesses.
func TestRunToolsGoTests_TrivialModule(t *testing.T) {
	root := t.TempDir()
	toolsDir := filepath.Join(root, "tools", "build")
	if err := os.MkdirAll(toolsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(toolsDir, "go.mod"), []byte("module example.com/tools\n\ngo 1.21\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Trivial test file so go test ./... has something to run.
	if err := os.WriteFile(filepath.Join(toolsDir, "noop_test.go"), []byte("package tools\n\nimport \"testing\"\n\nfunc TestNoop(t *testing.T) { t.Log(\"noop\") }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := RunToolsGoTests(root); err != nil {
		t.Fatalf("RunToolsGoTests: %v", err)
	}
}
