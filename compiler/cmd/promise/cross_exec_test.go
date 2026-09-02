package main

import (
	"context"
	"os/exec"
	"runtime"
	"strings"
	"testing"
)

func TestIsHostTarget(t *testing.T) {
	t.Parallel()
	cases := []struct {
		triple string
		want   bool
	}{
		// The host target must always be recognized as host.
		{"", false}, // empty is not a valid triple
	}

	// Build expected host cases dynamically so the test works on any platform.
	switch runtime.GOOS {
	case "darwin":
		switch runtime.GOARCH {
		case "arm64":
			cases = append(cases,
				struct {
					triple string
					want   bool
				}{"arm64-apple-macosx15.0.0", true},
				struct {
					triple string
					want   bool
				}{"arm64-apple-macosx14.0.0", true},
				struct {
					triple string
					want   bool
				}{"x86_64-apple-macosx10.15.0", false},
				struct {
					triple string
					want   bool
				}{"aarch64-unknown-linux-musl", false},
				struct {
					triple string
					want   bool
				}{"x86_64-pc-windows-msvc", false},
			)
		case "amd64":
			cases = append(cases,
				struct {
					triple string
					want   bool
				}{"x86_64-apple-macosx10.15.0", true},
				struct {
					triple string
					want   bool
				}{"arm64-apple-macosx15.0.0", false},
			)
		}
	case "linux":
		switch runtime.GOARCH {
		case "amd64":
			cases = append(cases,
				struct {
					triple string
					want   bool
				}{"x86_64-unknown-linux-musl", true},
				struct {
					triple string
					want   bool
				}{"x86_64-unknown-linux-gnu", true},
				struct {
					triple string
					want   bool
				}{"aarch64-unknown-linux-musl", false},
				struct {
					triple string
					want   bool
				}{"arm64-apple-macosx15.0.0", false},
			)
		case "arm64":
			cases = append(cases,
				struct {
					triple string
					want   bool
				}{"aarch64-unknown-linux-musl", true},
				struct {
					triple string
					want   bool
				}{"x86_64-unknown-linux-musl", false},
			)
		}
	case "windows":
		switch runtime.GOARCH {
		case "amd64":
			cases = append(cases,
				struct {
					triple string
					want   bool
				}{"x86_64-pc-windows-msvc", true},
				struct {
					triple string
					want   bool
				}{"aarch64-unknown-linux-musl", false},
			)
		}
	}

	// Always-false cases regardless of platform.
	cases = append(cases,
		struct {
			triple string
			want   bool
		}{"wasm32-wasi", false},
		struct {
			triple string
			want   bool
		}{"wasm32-web", false},
		struct {
			triple string
			want   bool
		}{"totally-bogus", false},
	)

	for _, c := range cases {
		if got := isHostTarget(c.triple); got != c.want {
			t.Errorf("isHostTarget(%q) = %v, want %v (GOOS=%s GOARCH=%s)",
				c.triple, got, c.want, runtime.GOOS, runtime.GOARCH)
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
// emulator that may or may not be installed. docs/distribution.md §1.1/§4
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
	if _, err := exec.LookPath("node"); err != nil {
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
