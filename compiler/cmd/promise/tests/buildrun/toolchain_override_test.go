package buildrun

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/promise-language/promise/compiler/cmd/promise/clitest"
)

// fakeOptReporting writes an executable stub that answers `--version` with the
// given LLVM version string, so a test can drive the version gate without a real
// toolchain of that vintage.
func fakeOptReporting(t *testing.T, version string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell-script tool stubs are not executable on Windows")
	}
	dir := clitest.TempDir(t)
	path := filepath.Join(dir, "opt")
	script := "#!/bin/sh\necho \"LLVM version " + version + "\"\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestToolchainOverrideIsAnnouncedAndUsed is the end-to-end half of T2108's
// loudness rule: pointing PROMISE_OPT at a binary by hand must (a) actually use
// it and (b) say so on stderr, every run. The unit tests cover the banner
// function; this covers the thing an operator actually sees, including that the
// override reaches a real compile rather than being shadowed by a pinned tool.
func TestToolchainOverrideIsAnnouncedAndUsed(t *testing.T) {
	t.Parallel()
	bin := clitest.Bin(t)
	// A version the compiler must refuse — proof the override was consulted, and
	// the cheapest way to observe it without shipping a second real toolchain.
	fakeOpt := fakeOptReporting(t, "21.0.0")

	cmd := exec.Command(bin, "exec", `print_line("x")`)
	cmd.Env = append(os.Environ(), "PROMISE_OPT="+fakeOpt)
	out, err := cmd.CombinedOutput()
	text := string(out)

	if err == nil {
		t.Fatalf("expected failure: the override reports LLVM 21, below the minimum\n%s", text)
	}
	// (a) the override announced itself, naming the variable and the path.
	if !strings.Contains(text, "PROMISE_OPT="+fakeOpt) {
		t.Errorf("the override must be announced naming variable and path:\n%s", text)
	}
	if !strings.Contains(text, "does NOT use the pinned toolchain") {
		t.Errorf("the banner must say the build is not pinned:\n%s", text)
	}
	// (b) it was used: the version gate saw 21 from the stub.
	if !strings.Contains(text, "is too old") {
		t.Errorf("the overridden opt should have been version-checked:\n%s", text)
	}
	// The remedy names the pinned toolchain, never a package manager: installing
	// a system LLVM does not change what this build uses (T2108).
	for _, unwanted := range []string{"brew install", "apt install", "apt-get install"} {
		if strings.Contains(text, unwanted) {
			t.Errorf("the version error must not suggest a system LLVM (%q):\n%s", unwanted, text)
		}
	}
	if !strings.Contains(text, "doctor --repair") {
		t.Errorf("the version error should point at restaging the pinned toolchain:\n%s", text)
	}
}

// TestNoToolchainBannerWhenPinned is the other half: on the ordinary path the
// compiler says nothing about the toolchain. A banner that appeared on every
// build would train the reader to ignore the one that matters.
func TestNoToolchainBannerWhenPinned(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("skipping compile in short mode")
	}
	bin := clitest.Bin(t)

	cmd := exec.Command(bin, "exec", `print_line("pinned")`)
	env := os.Environ()
	for _, v := range []string{"PROMISE_OPT", "PROMISE_LLC", "PROMISE_LLD", "PROMISE_LD64LLD", "PROMISE_WASM_LD", "PROMISE_CLANG", "PROMISE_USE_CLANG"} {
		env = append(env, v+"=")
	}
	cmd.Env = env
	// Streams kept apart on purpose: the program's output is on stdout, while
	// stderr legitimately carries staging progress ("Waiting for promise
	// (materializing LLVM toolchain)...") on a cold cache. Merging them would
	// make this test fail for a reason that has nothing to do with what it
	// checks.
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		t.Skipf("no pinned toolchain available on this host: %v\nstdout: %s\nstderr: %s", err, stdout.String(), stderr.String())
	}
	if strings.Contains(stderr.String(), "toolchain override") {
		t.Errorf("a pinned build must not mention an override:\n%s", stderr.String())
	}
	if got := strings.TrimSpace(stdout.String()); got != "pinned" {
		t.Errorf("stdout = %q, want %q (stderr: %s)", got, "pinned", stderr.String())
	}
}
