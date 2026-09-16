package buildrun

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/promise-language/promise/compiler/cmd/promise/clitest"
)

// T2086 (reported twice — T1966 is the same defect): `promise build -target
// <triple> file.pr` on a loose single file (no promise.toml) must resolve
// `target(cond) against the triple that was asked for, not against the host.
//
// buildToFile compiled the project branch with the requested target and the
// single-file branch with the host, so only a file living outside a project
// could be filtered for the wrong platform — which is why every catalog module
// and every tests/catalog/ fixture kept passing.
//
// The two directions fail differently and so are two tests. A declaration that
// exists only on the requested target disappears, and the build fails loudly
// with `undefined:`. A declaration that exists on *both* targets in filtered
// variants silently picks the host one, and the build succeeds — producing a
// correctly-targeted binary that runs the wrong code.

// TestBuildLooseFileFiltersForRequestedTarget covers the loud direction: a
// declaration gated to the requested target must exist when building for it.
//
// Both sub-targets are exercised because the two conditions match through
// different TargetInfo fields: `target(wasm) matches on OS, `target(web) on the
// WASM sub-target Env. Env is the one both reports actually hit — a `target(web)
// binding surface under -target wasm32-web — and nothing else in the suite
// builds a loose file for wasm32-web.
func TestBuildLooseFileFiltersForRequestedTarget(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("skipping build integration test in short mode")
	}
	bin := clitest.Bin(t)

	for _, tc := range []struct{ triple, cond string }{
		{"wasm32-wasi", "wasm"},
		{"wasm32-web", "web"},
	} {
		t.Run(tc.triple, func(t *testing.T) {
			t.Parallel()

			// No promise.toml: this is the single-file path, and t.TempDir()
			// has no project anywhere above it.
			dir := t.TempDir()
			src := "gated() int `target(" + tc.cond + ") { return 7; }\n" +
				"main() { print_line(gated().to_string()); }\n"
			if err := os.WriteFile(filepath.Join(dir, "main.pr"), []byte(src), 0644); err != nil {
				t.Fatal(err)
			}

			cmd := exec.Command(bin, "build", "-target", tc.triple, "-o", "out.wasm", "main.pr")
			cmd.Dir = dir
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("build -target %s failed (want exit 0): %v\n%s", tc.triple, err, out)
			}
			// Belt and braces: sema errors are printed but a non-fatal set
			// would not fail the build, and `undefined: gated` is the exact
			// host-filtered symptom this test exists to catch.
			if strings.Contains(string(out), "undefined:") {
				t.Errorf("`target(%s) declaration was filtered out when building for %s:\n%s",
					tc.cond, tc.triple, out)
			}
			if _, err := os.Stat(filepath.Join(dir, "out.wasm")); err != nil {
				t.Errorf("expected out.wasm: %v\noutput: %s", err, out)
			}
		})
	}
}

// TestBuildLooseFileLinksTheTargetVariant covers the silent direction: with
// both variants of a filtered pair present, the binary must contain the
// requested target's variant and not the host's. It also checks that a build
// with no -target still picks the native variant, since buildToFile defaults
// the triple to the host before the frontend runs.
func TestBuildLooseFileLinksTheTargetVariant(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("skipping build integration test in short mode")
	}
	bin := clitest.Bin(t)

	// Markers are deliberately distinctive: a bare "wasm" also occurs in an
	// unrelated part of a .wasm file, so scanning for it would false-positive.
	const wasmMarker = "T2086-wasm-variant"
	const nativeMarker = "T2086-native-variant"

	dir := t.TempDir()
	src := "which() string `target(wasm)  { return \"" + wasmMarker + "\"; }\n" +
		"which() string `target(!wasm) { return \"" + nativeMarker + "\"; }\n" +
		"main() { print_line(which()); }\n"
	if err := os.WriteFile(filepath.Join(dir, "pick.pr"), []byte(src), 0644); err != nil {
		t.Fatal(err)
	}

	buildCmd := exec.Command(bin, "build", "-target", "wasm32-wasi", "-o", "pick.wasm", "pick.pr")
	buildCmd.Dir = dir
	out, err := buildCmd.CombinedOutput()
	if err != nil {
		t.Fatalf("build -target wasm32-wasi failed: %v\n%s", err, out)
	}

	// The losing variant is dropped in sema, so its literal is never emitted at
	// all — presence/absence in the artifact is exact. Reading the bytes keeps
	// this test free of a wasm runtime, which this package does not require.
	wasmBin, err := os.ReadFile(filepath.Join(dir, "pick.wasm"))
	if err != nil {
		t.Fatalf("read pick.wasm: %v\noutput: %s", err, out)
	}
	if !bytes.Contains(wasmBin, []byte(wasmMarker)) {
		t.Errorf("wasm32-wasi binary does not contain the `target(wasm) variant (%s)", wasmMarker)
	}
	if bytes.Contains(wasmBin, []byte(nativeMarker)) {
		t.Errorf("wasm32-wasi binary contains the host `target(!wasm) variant (%s) — "+
			"the frontend filtered for the host instead of the requested target", nativeMarker)
	}

	// No -target: the host default must be unchanged. This also exercises
	// runRun's single-file path, which shares buildToFile with build.
	runCmd := exec.Command(bin, "run", "pick.pr")
	runCmd.Dir = dir
	runOut, err := runCmd.CombinedOutput()
	if err != nil {
		t.Fatalf("run pick.pr failed: %v\n%s", err, runOut)
	}
	if !strings.Contains(string(runOut), nativeMarker) {
		t.Errorf("host build output = %q, want it to contain %q", runOut, nativeMarker)
	}
}
