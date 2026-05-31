package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/promise-language/promise/tools/build/common"
)

func TestHasGoFiles(t *testing.T) {
	dir := t.TempDir()
	if hasGoFiles(dir) {
		t.Error("empty dir should report no Go files")
	}
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if hasGoFiles(dir) {
		t.Error("dir with only non-Go files should report no Go files")
	}
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !hasGoFiles(dir) {
		t.Error("dir with a .go file should report Go files")
	}
}

func TestHasGoFiles_MissingDir(t *testing.T) {
	if hasGoFiles(filepath.Join(t.TempDir(), "does-not-exist")) {
		t.Error("a missing dir should report no Go files (not panic)")
	}
}

func TestWriteFlowRootMarker(t *testing.T) {
	root := t.TempDir()
	writeFlowRootMarker(root)
	marker := filepath.Join(root, ".flow", "root")
	data, err := os.ReadFile(marker)
	if err != nil {
		t.Fatalf("marker not written: %v", err)
	}
	// The SDK's DiscoverWorktreeRoot only checks existence, but the content names
	// the root for human inspection — assert it points at the worktree root.
	if got := string(data); got != root+"\n" {
		t.Errorf("marker content = %q, want %q", got, root+"\n")
	}
}

func TestWriteFlowRootMarker_Idempotent(t *testing.T) {
	root := t.TempDir()
	writeFlowRootMarker(root)
	writeFlowRootMarker(root) // second call must not error or duplicate
	if _, err := os.Stat(filepath.Join(root, ".flow", "root")); err != nil {
		t.Fatalf("marker missing after repeated writes: %v", err)
	}
}

// TestBuildFlowsLeavesWorktreeClean pins T0750's regression: a flows rebuild must
// NOT mutate the committed flows/go.mod or flows/go.sum. The old code ran an
// unconditional build-time `go mod tidy` that rewrote go.mod into a state the
// subsequent readonly `go build` rejected on the Windows gate (and dirtied the
// worktree, triggering orchestrator skips). After the drift-gated fix the build is
// hermetic against the committed go.mod when there is no real import-graph drift —
// which is the case for the pinned flow/flow-sdk submodules.
//
// It is a guarded integration test: skipped in -short, when the flows module or its
// submodules aren't checked out, or when `go` is unavailable — so it never fails a
// minimal/offline environment that simply can't run it.
func TestBuildFlowsLeavesWorktreeClean(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test: invokes go build on the flows module")
	}
	root, err := common.FindRoot()
	if err != nil {
		t.Skipf("repo root not found (running outside the worktree): %v", err)
	}
	flowsDir := filepath.Join(root, "flows")
	if !common.Exists(filepath.Join(flowsDir, "go.mod")) {
		t.Skip("flows/ module not present")
	}
	// Both submodules must be checked out — buildFlows tolerates their absence by
	// skipping, which would make this test vacuously pass.
	if !common.Exists(filepath.Join(root, "flow-sdk", "go.mod")) ||
		!common.Exists(filepath.Join(root, "flow", "go.mod")) {
		t.Skip("flow submodules not checked out")
	}
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain unavailable")
	}

	// Snapshot go.mod and go.sum exactly as committed. Comparing content (rather than
	// git status) isolates the regression — no build-time mutation of these files —
	// independent of any other dirty paths in the worktree.
	modPath := filepath.Join(flowsDir, "go.mod")
	sumPath := filepath.Join(flowsDir, "go.sum")
	beforeMod, err := os.ReadFile(modPath)
	if err != nil {
		t.Fatalf("read flows/go.mod: %v", err)
	}
	beforeSum, beforeSumErr := os.ReadFile(sumPath) // err != nil ⇒ go.sum absent (the committed state)

	// force=true bypasses the up-to-date short-circuit so the build path actually runs.
	if err := buildFlows(root, true); err != nil {
		t.Fatalf("buildFlows failed: %v", err)
	}

	afterMod, err := os.ReadFile(modPath)
	if err != nil {
		t.Fatalf("read flows/go.mod after build: %v", err)
	}
	if string(beforeMod) != string(afterMod) {
		t.Errorf("flows/go.mod was mutated by the build (T0750 regression)\n--- before ---\n%s\n--- after ---\n%s", beforeMod, afterMod)
	}
	afterSum, afterSumErr := os.ReadFile(sumPath)
	switch {
	case beforeSumErr == nil && afterSumErr != nil:
		t.Error("flows/go.sum was removed by the build")
	case beforeSumErr != nil && afterSumErr == nil:
		t.Errorf("flows/go.sum was created by the build (T0750 regression); content:\n%s", afterSum)
	case beforeSumErr == nil && afterSumErr == nil && string(beforeSum) != string(afterSum):
		t.Error("flows/go.sum was mutated by the build (T0750 regression)")
	}
}

// writeFile writes content to path, creating any missing parent directories.
func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// TestTidyFlowsIfDrift_Clean covers T0750's common no-drift path: a tidy module
// makes `go mod tidy -diff` exit 0, so tidyFlowsIfDrift writes nothing — go.mod is
// byte-for-byte unchanged and no go.sum is created. This is the hermetic,
// mutation-free behavior the Windows gate needs.
func TestTidyFlowsIfDrift_Clean(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain unavailable")
	}
	t.Setenv("GOPROXY", "off") // hermetic: a no-drift module never needs the network
	dir := t.TempDir()
	const goMod = "module driftcheck\n\ngo 1.26\n"
	writeFile(t, filepath.Join(dir, "go.mod"), goMod)
	writeFile(t, filepath.Join(dir, "main.go"), "package main\n\nfunc main() {}\n")

	if err := tidyFlowsIfDrift(dir); err != nil {
		t.Fatalf("tidyFlowsIfDrift on a clean module: %v", err)
	}
	after, err := os.ReadFile(filepath.Join(dir, "go.mod"))
	if err != nil {
		t.Fatalf("read go.mod: %v", err)
	}
	if string(after) != goMod {
		t.Errorf("clean module go.mod was mutated\n--- before ---\n%s\n--- after ---\n%s", goMod, after)
	}
	if common.Exists(filepath.Join(dir, "go.sum")) {
		t.Error("clean module: go.sum was created (no dependencies ⇒ none expected)")
	}
}

// TestTidyFlowsIfDrift_Drift covers T0750's self-heal path: when the module
// genuinely drifts — here an unused require, kept fully offline via a local
// replace — `go mod tidy -diff` exits non-zero, so tidyFlowsIfDrift falls through
// to a real `go mod tidy` that heals go.mod (drops the unused require). This is
// the rare path that preserves the old always-tidy behavior on genuine drift.
func TestTidyFlowsIfDrift_Drift(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain unavailable")
	}
	t.Setenv("GOPROXY", "off") // the require is satisfied by a local replace ⇒ no network
	dir := t.TempDir()
	// A nested module reachable only via a local replace — no proxy, fully offline.
	writeFile(t, filepath.Join(dir, "x", "go.mod"), "module example.local/x\n\ngo 1.26\n")
	writeFile(t, filepath.Join(dir, "x", "x.go"), "package x\n")
	// main.go does NOT import example.local/x, so the require is unused ⇒ -diff drift.
	writeFile(t, filepath.Join(dir, "go.mod"),
		"module driftcheck\n\ngo 1.26\n\nrequire example.local/x v0.0.0\n\nreplace example.local/x => ./x\n")
	writeFile(t, filepath.Join(dir, "main.go"), "package main\n\nfunc main() {}\n")

	if err := tidyFlowsIfDrift(dir); err != nil {
		t.Fatalf("tidyFlowsIfDrift on a drifted module: %v", err)
	}
	after, err := os.ReadFile(filepath.Join(dir, "go.mod"))
	if err != nil {
		t.Fatalf("read go.mod: %v", err)
	}
	if strings.Contains(string(after), "require example.local/x") {
		t.Errorf("drift was not healed: the unused require is still present\n--- go.mod ---\n%s", after)
	}
}

// TestTidyFlowsIfDrift_UnhealableReturnsError covers T0750's failure contract: if
// `go mod tidy -diff` reports drift but the real `go mod tidy` self-heal then also
// fails (here an imported require whose local replace target is missing), the error
// must propagate so buildFlows fails loudly rather than proceeding to a broken
// build. The error is wrapped with the "go mod tidy (flows)" context.
func TestTidyFlowsIfDrift_UnhealableReturnsError(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain unavailable")
	}
	t.Setenv("GOPROXY", "off") // failure is a missing local replace dir, not a network miss
	dir := t.TempDir()
	// main.go imports example.local/x, but its replace target does not exist, so
	// both `go mod tidy -diff` and the real `go mod tidy` fail to load the module.
	writeFile(t, filepath.Join(dir, "go.mod"),
		"module driftcheck\n\ngo 1.26\n\nrequire example.local/x v0.0.0\n\nreplace example.local/x => ./does-not-exist\n")
	writeFile(t, filepath.Join(dir, "main.go"), "package main\n\nimport _ \"example.local/x\"\n\nfunc main() {}\n")

	err := tidyFlowsIfDrift(dir)
	if err == nil {
		t.Fatal("tidyFlowsIfDrift must return an error when the self-heal tidy fails")
	}
	if !strings.Contains(err.Error(), "go mod tidy (flows)") {
		t.Errorf("error missing the expected context wrap: %v", err)
	}
}

// captureStderr redirects os.Stderr to a temp file while fn runs and returns
// everything written to it. dumpFlowsBuildContext writes directly to os.Stderr
// (including subprocess output it forwards there via common.RunOutputIn, which
// reads os.Stderr at call time), so swapping the *os.File is the simplest
// deadlock-free way to capture it. The defer is a panic safety net; the inline
// restore is what the happy path relies on. Not safe under t.Parallel(); these
// tests run sequentially.
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "stderr-*")
	if err != nil {
		t.Fatalf("create temp: %v", err)
	}
	orig := os.Stderr
	os.Stderr = f
	defer func() { os.Stderr = orig }()
	fn()
	os.Stderr = orig
	if err := f.Close(); err != nil {
		t.Fatalf("close temp: %v", err)
	}
	data, err := os.ReadFile(f.Name())
	if err != nil {
		t.Fatalf("read temp: %v", err)
	}
	return string(data)
}

// TestDumpFlowsBuildContext covers the T0750 diagnostic dump on the happy path:
// given a flowsDir with a readable go.mod, it must print the bracketing markers
// and the go.mod contents to stderr. This is the self-explaining output a Windows
// recurrence relies on, so it must actually emit what it claims.
func TestDumpFlowsBuildContext(t *testing.T) {
	dir := t.TempDir()
	// A unique token in go.mod lets us assert the contents were actually dumped,
	// not just the "flows/go.mod:" header. Minimal + valid so `go mod tidy -diff`
	// (run by the dump) stays a no-op and never touches the network.
	const marker = "promisemake_dumpmarker"
	goMod := "module " + marker + "\n\ngo 1.26\n"
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte(goMod), 0o644); err != nil {
		t.Fatal(err)
	}

	out := captureStderr(t, func() { dumpFlowsBuildContext(dir) })

	for _, want := range []string{
		"--- flows build failed; dumping context (T0750) ---", // header (always)
		"--- end flows build context ---",                     // footer (always)
		"flows/go.mod:",                                       // go.mod section header
		marker,                                                // the go.mod was actually read out
	} {
		if !strings.Contains(out, want) {
			t.Errorf("dump output missing %q\n--- output ---\n%s", want, out)
		}
	}
	// go is available in this module's own test env, so the env probe must fire.
	if _, err := exec.LookPath("go"); err == nil && !strings.Contains(out, "go env") {
		t.Errorf("dump output missing the go env section\n--- output ---\n%s", out)
	}
}

// TestDumpFlowsBuildContext_MissingGoMod covers the best-effort branch: a flowsDir
// with no go.mod must NOT panic and must skip the go.mod section (os.ReadFile
// errors) while still emitting the bracketing markers. This exercises the
// ReadFile-error path that the happy-path test does not.
func TestDumpFlowsBuildContext_MissingGoMod(t *testing.T) {
	dir := t.TempDir() // deliberately empty: no go.mod

	out := captureStderr(t, func() { dumpFlowsBuildContext(dir) })

	if !strings.Contains(out, "--- flows build failed; dumping context (T0750) ---") {
		t.Errorf("dump output missing header\n--- output ---\n%s", out)
	}
	if !strings.Contains(out, "--- end flows build context ---") {
		t.Errorf("dump output missing footer\n--- output ---\n%s", out)
	}
	if strings.Contains(out, "flows/go.mod:") {
		t.Errorf("dump must skip the go.mod section when go.mod is absent\n--- output ---\n%s", out)
	}
}
