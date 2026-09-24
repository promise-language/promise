package main

import (
	"context"
	"os/exec"
	"runtime"
	"strings"
	"testing"
)

// TestHostTargetMatrix pins isHostTargetFor for every host this compiler runs
// on, not just the one running the test.
//
// The windows-amd64 row is the whole point. x86_64-pc-windows-msvc is a CROSS
// target on Linux and macOS, and on windows-amd64 it is that host's OWN target,
// so `promise run` executes what it built instead of refusing it — the fact
// T2206's black-box test contradicted, having been written where the Windows
// branch could not be seen. Naming the host makes that row assertable on every
// machine; reading it from runtime.GOOS, as this test used to, checks one.
func TestHostTargetMatrix(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		goos, goarch, triple string
		want                 bool
	}{
		// windows-amd64: its own triple is native, its arm64 sibling is not.
		{"windows", "amd64", "x86_64-pc-windows-msvc", true},
		{"windows", "amd64", "aarch64-pc-windows-msvc", false},
		{"windows", "amd64", "x86_64-unknown-linux-musl", false},
		{"windows", "amd64", "x86_64-apple-macosx10.15.0", false},
		// windows-arm64: the same triple, now foreign — the arch half of the
		// check is what separates these two rows.
		{"windows", "arm64", "aarch64-pc-windows-msvc", true},
		{"windows", "arm64", "x86_64-pc-windows-msvc", false},

		{"linux", "amd64", "x86_64-unknown-linux-musl", true},
		{"linux", "amd64", "x86_64-unknown-linux-gnu", true},
		{"linux", "amd64", "x86_64-pc-windows-msvc", false},
		{"linux", "amd64", "aarch64-unknown-linux-musl", false},
		{"linux", "arm64", "aarch64-unknown-linux-musl", true},
		{"linux", "arm64", "x86_64-unknown-linux-musl", false},

		// macOS carries a version suffix the comparison must ignore, and
		// spells arm64 both ways.
		{"darwin", "arm64", "arm64-apple-macosx15.0.0", true},
		{"darwin", "arm64", "arm64-apple-macosx14.0.0", true},
		{"darwin", "arm64", "aarch64-apple-macosx14.0.0", true},
		{"darwin", "arm64", "x86_64-apple-macosx10.15.0", false},
		{"darwin", "arm64", "aarch64-unknown-linux-musl", false},
		{"darwin", "arm64", "x86_64-pc-windows-msvc", false},
		{"darwin", "amd64", "x86_64-apple-macosx10.15.0", true},
		{"darwin", "amd64", "arm64-apple-macosx15.0.0", false},

		// An unknown host OS or arch matches nothing rather than defaulting to
		// "yes" — a host this compiler was never built for must not be handed
		// a foreign binary to exec.
		{"plan9", "amd64", "x86_64-unknown-linux-musl", false},
		{"linux", "riscv64", "x86_64-unknown-linux-musl", false},
		{"windows", "386", "x86_64-pc-windows-msvc", false},
	} {
		if got := isHostTargetFor(tc.goos, tc.goarch, tc.triple); got != tc.want {
			t.Errorf("isHostTargetFor(%q, %q, %q) = %v, want %v",
				tc.goos, tc.goarch, tc.triple, got, tc.want)
		}
	}

	// Never host, whoever is asking: an empty triple is not a triple, wasm is
	// dispatched to its own runtime before the host check, and a bogus string
	// must not match by accident.
	for _, goos := range []string{"windows", "linux", "darwin"} {
		for _, goarch := range []string{"amd64", "arm64"} {
			for _, triple := range []string{"", "wasm32-wasi", "wasm32-web", "totally-bogus"} {
				if isHostTargetFor(goos, goarch, triple) {
					t.Errorf("isHostTargetFor(%q, %q, %q) = true, want false", goos, goarch, triple)
				}
			}
		}
	}
}

// TestCanExecuteTargetMatrix is the same fact one level up, at the predicate
// `run`/`test`/`exec` actually consult: the identical triple is executable on
// one host and a hard error on another, and the message names the host that
// refused it.
func TestCanExecuteTargetMatrix(t *testing.T) {
	t.Parallel()
	if err := canExecuteTargetFor("windows", "amd64", "x86_64-pc-windows-msvc"); err != nil {
		t.Errorf("windows-amd64 refuses its own target: %v", err)
	}
	err := canExecuteTargetFor("linux", "amd64", "x86_64-pc-windows-msvc")
	if err == nil {
		t.Fatal("linux-amd64 accepted a windows binary; a non-host target must be a hard error")
	}
	for _, want := range []string{"cross-target execution is not supported", "linux-amd64"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %q, want it to contain %q", err, want)
		}
	}
	// wasm is executable from anywhere: its runtime is a shipped harness, not
	// the host CPU.
	for _, triple := range []string{"wasm32-wasi", "wasm32-web"} {
		if err := canExecuteTargetFor("windows", "amd64", triple); err != nil {
			t.Errorf("canExecuteTargetFor(windows, amd64, %q) = %v, want nil", triple, err)
		}
	}
}

// TestWindowsAmd64AdvertisesNothingItCannotRun is the premise of the skip in
// tests/buildrun/windows_cross_link_test.go stated as an assertion, and the
// reason option (2) of T2206 — "assert against a triple that is cross
// everywhere" — does not exist.
//
// A CLI-level cross-execution refusal needs a target that links AND cannot run.
// On windows-amd64 there is none: every advertised target is the host's own or
// a wasm one, and anything else is turned away by the flag gate long before
// execution. Linux is the control — there the windows triple is exactly such a
// target, which is why the refusal test runs there. If a future release adds a
// cross-linkable target to the Windows set (T0530/T0532), this fails and the
// skip can be replaced by a real assertion.
func TestWindowsAmd64AdvertisesNothingItCannotRun(t *testing.T) {
	t.Parallel()
	for _, ts := range supportedTargetsFor("x86_64-pc-windows-msvc") {
		if err := canExecuteTargetFor("windows", "amd64", ts.Triple); err != nil {
			t.Errorf("windows-amd64 advertises %q but cannot run it: %v\n"+
				"  the CLI can now reach the cross-execution refusal on Windows — "+
				"replace the skip in TestWindowsCrossRunRefusesToExecute with this target",
				ts.Triple, err)
		}
	}

	// The control: on linux-amd64 the same set does contain one, so the
	// assertion above is about Windows and not about an empty loop.
	unrunnable := 0
	for _, ts := range supportedTargetsFor("x86_64-unknown-linux-musl") {
		if canExecuteTargetFor("linux", "amd64", ts.Triple) != nil {
			unrunnable++
		}
	}
	if unrunnable != 1 {
		t.Errorf("linux-amd64 advertises %d targets it cannot run, want exactly 1 (x86_64-pc-windows-msvc)", unrunnable)
	}
}

// TestHostPredicatesUseTheRunningHost keeps the parameterized forms above from
// passing while the real callers get a different answer: the wrappers must
// forward runtime.GOOS/GOARCH and nothing else.
func TestHostPredicatesUseTheRunningHost(t *testing.T) {
	t.Parallel()
	for _, triple := range []string{
		"x86_64-pc-windows-msvc", "x86_64-unknown-linux-musl",
		"aarch64-unknown-linux-musl", "arm64-apple-macosx15.0.0",
		"wasm32-wasi", "totally-bogus",
	} {
		want := isHostTargetFor(runtime.GOOS, runtime.GOARCH, triple)
		if got := isHostTarget(triple); got != want {
			t.Errorf("isHostTarget(%q) = %v, want the %s-%s answer %v",
				triple, got, runtime.GOOS, runtime.GOARCH, want)
		}
		wantErr := canExecuteTargetFor(runtime.GOOS, runtime.GOARCH, triple)
		gotErr := canExecuteTarget(triple)
		if (gotErr == nil) != (wantErr == nil) {
			t.Errorf("canExecuteTarget(%q) = %v, want the %s-%s answer %v",
				triple, gotErr, runtime.GOOS, runtime.GOARCH, wantErr)
		}
	}
}

func TestCrossExecCommandHostTarget(t *testing.T) {
	t.Parallel()
	// The host target should produce a command without error.
	host := hostTargetForTest()
	if host == "" {
		t.Skip("cannot determine host target for this platform")
	}
	cmd, err := crossExecCommand(context.Background(), host, "/bin/echo", "hi")
	if err != nil {
		t.Fatalf("unexpected error for host target: %v", err)
	}
	if cmd == nil {
		t.Fatal("expected non-nil command")
	}
}

func TestCrossExecCommandWasm(t *testing.T) {
	t.Parallel()
	cmd, err := crossExecCommand(context.Background(), "wasm32-wasi", "/tmp/test.wasm")
	if err != nil {
		t.Fatalf("unexpected error for wasm target: %v", err)
	}
	if cmd.Path == "" {
		t.Fatal("expected non-empty command path")
	}
}

// TestCrossExecCommandNoHostProbing pins the rule that makes this dispatcher
// safe: a non-host native target is a hard error, never a fall back to an
// emulator that may or may not be installed. docs/distribution.md#what-is-always-in-the-binary-and-what-is-fetched-on-demand/distribution.md#the-dependency-store
// requires every dependency to be a manifest-named blob, so behaviour must not
// change with what the machine happens to have on PATH. An earlier draft of
// this code probed for wine64/wine and qemu-* — that is exactly what must not
// come back.
func TestCrossExecCommandNoHostProbing(t *testing.T) {
	t.Parallel()
	crossTargets := []string{
		"x86_64-pc-windows-msvc",
		"aarch64-pc-windows-msvc",
		"riscv64-unknown-linux-musl",
		"arm64-apple-macosx14.0.0",
		"x86_64-apple-macosx10.15.0",
		"x86_64-unknown-linux-musl",
		"aarch64-unknown-linux-musl",
	}
	for _, target := range crossTargets {
		t.Run(target, func(t *testing.T) {
			t.Parallel()
			if isHostTarget(target) {
				t.Skip("native on this host; nothing to assert")
			}
			cmd, err := crossExecCommand(context.Background(), target, "/tmp/test")
			if err == nil {
				t.Fatalf("succeeded on %s-%s; a non-host target must be a hard error, not an emulator fallback",
					runtime.GOOS, runtime.GOARCH)
			}
			if cmd != nil {
				t.Error("returned a command alongside an error")
			}
			if got := err.Error(); !strings.Contains(got, "cross-target execution is not supported") {
				t.Errorf("error = %q, want the unsupported-cross-execution message", got)
			}
			// The error must not offer an install-this-tool escape hatch.
			for _, banned := range []string{"wine", "qemu", "path"} {
				if strings.Contains(strings.ToLower(err.Error()), banned) {
					t.Errorf("error mentions %q: %s", banned, err)
				}
			}
		})
	}
}

// TestCanExecuteTargetAgreesWithDispatch keeps the cheap pre-flight predicate
// and the real dispatcher from drifting apart — stress uses the former to fail
// before compiling, so a disagreement would let it compile and then die.
func TestCanExecuteTargetAgreesWithDispatch(t *testing.T) {
	t.Parallel()
	targets := []string{
		"wasm32-wasi", "wasm32-web", "x86_64-pc-windows-msvc",
		"riscv64-unknown-linux-musl", hostTargetForTest(),
	}
	for _, target := range targets {
		if target == "" {
			continue // no known host triple for this platform
		}
		t.Run(target, func(t *testing.T) {
			t.Parallel()
			if isWasmWebTarget(target) {
				skipWithoutNode(t)
			}
			preflight := canExecuteTarget(target)
			_, err := crossExecCommand(context.Background(), target, "/tmp/test")
			if (preflight == nil) != (err == nil) {
				t.Errorf("canExecuteTarget=%v disagrees with crossExecCommand err=%v", preflight, err)
			}
		})
	}
}

func TestCrossExecCommandWasmWeb(t *testing.T) {
	t.Parallel()
	skipWithoutNode(t)
	cmd, err := crossExecCommand(context.Background(), "wasm32-web", "/tmp/test.wasm")
	if err != nil {
		t.Fatalf("unexpected error for wasm-web target: %v", err)
	}
	if cmd == nil {
		t.Fatal("expected non-nil command for wasm-web target")
	}
}

func TestCrossExecCommandHostArgs(t *testing.T) {
	t.Parallel()
	host := hostTargetForTest()
	if host == "" {
		t.Skip("cannot determine host target for this platform")
	}
	cmd, err := crossExecCommand(context.Background(), host, "/bin/echo", "a", "b", "c")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Args[0] is the path, then the user args follow.
	if len(cmd.Args) != 4 {
		t.Errorf("expected 4 args (path + 3), got %d: %v", len(cmd.Args), cmd.Args)
	}
}

// TestCrossExecCommandWasmArgs pins that guest argv is forwarded to wasmtime
// rather than silently dropped, and that wasm32-web — whose Node harness has
// nowhere to put argv — refuses instead of pretending it worked.
func TestCrossExecCommandWasmArgs(t *testing.T) {
	t.Parallel()
	cmd, err := crossExecCommand(context.Background(), "wasm32-wasi", "/tmp/t.wasm", "a", "b")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(cmd.Args) != 4 || cmd.Args[2] != "a" || cmd.Args[3] != "b" {
		t.Errorf("wasmtime args = %v, want the guest argv forwarded", cmd.Args)
	}

	if _, err := crossExecCommand(context.Background(), "wasm32-web", "/tmp/t.wasm", "a"); err == nil {
		t.Error("wasm32-web accepted program arguments it cannot forward; want an explicit error")
	}
}

// skipWithoutNode skips a test that would construct a wasm32-web command.
// runWasmWeb resolves the Node binary eagerly and calls os.Exit when it is
// absent, so reaching it on a machine without Node would kill the whole test
// binary rather than fail one test. A test states its own world.
func skipWithoutNode(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("node"); err != nil { // path-ok: skips for the absent runtime itself, which is the legitimate form
		t.Skip("node not installed; wasm32-web command construction needs it")
	}
}

// hostTargetForTest returns a known host triple for the current platform.
func hostTargetForTest() string {
	switch runtime.GOOS {
	case "darwin":
		if runtime.GOARCH == "arm64" {
			return "arm64-apple-macosx15.0.0"
		}
		return "x86_64-apple-macosx10.15.0"
	case "linux":
		if runtime.GOARCH == "arm64" {
			return "aarch64-unknown-linux-musl"
		}
		return "x86_64-unknown-linux-musl"
	case "windows":
		return "x86_64-pc-windows-msvc"
	}
	return ""
}
