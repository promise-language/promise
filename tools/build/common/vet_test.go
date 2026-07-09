package common

import (
	"os"
	"path/filepath"
	"testing"
)

// TestRunFlowsVet_NoFlowsModule verifies that RunFlowsVet skips without error
// when flows/go.mod does not exist (e.g. on main without the flows branch).
func TestRunFlowsVet_NoFlowsModule(t *testing.T) {
	root := t.TempDir()
	skipped, err := RunFlowsVet(root)
	if err != nil {
		t.Fatalf("RunFlowsVet: unexpected error: %v", err)
	}
	if !skipped {
		t.Error("RunFlowsVet: expected skipped=true when flows/go.mod absent")
	}
}

// TestRunFlowsVet_NoSDK verifies that RunFlowsVet skips (with a warning) when
// flows/go.mod exists but flow-sdk/go.mod does not.
func TestRunFlowsVet_NoSDK(t *testing.T) {
	root := t.TempDir()
	flowsDir := filepath.Join(root, "flows")
	if err := os.MkdirAll(flowsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(flowsDir, "go.mod"), []byte("module example.com/flows\n\ngo 1.21\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// flow-sdk/ is absent — should skip with warning, not error.
	skipped, err := RunFlowsVet(root)
	if err != nil {
		t.Fatalf("RunFlowsVet: unexpected error: %v", err)
	}
	if !skipped {
		t.Error("RunFlowsVet: expected skipped=true when flow-sdk/go.mod absent")
	}
}

// TestRunToolsVet_RealRepo verifies that RunToolsVet succeeds against the real
// tools/build module (which always exists in this repository).
func TestRunToolsVet_RealRepo(t *testing.T) {
	root, err := FindRoot()
	if err != nil {
		t.Skip("not inside the promise repo:", err)
	}
	if err := RunToolsVet(root); err != nil {
		t.Fatalf("RunToolsVet: %v", err)
	}
}
