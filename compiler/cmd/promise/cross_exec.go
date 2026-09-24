package main

import (
	"context"
	"fmt"
	"os/exec"
	"runtime"
	"strings"
)

// crossExecCommand builds an exec.Cmd to run a compiled binary for the given
// target on the current host, unifying the dispatch that `run`, `exec`, `test`
// and `stress` previously each open-coded.
//
// The compiler never probes the host for emulators, toolchains, or any other
// installed software. Per docs/distribution.md §1.1/§4 every heavy dependency
// is a content-addressed blob named by the embedded manifest and verified by
// sha256 — behaviour must not vary with what happens to be installed on the
// machine, and a missing dependency must never be treated as absent-but-
// optional. A binary built for a non-host native target is therefore simply
// not executable here: that is a hard error, never a silent fall back to
// whatever the host happens to provide. Executing cross-built binaries is the
// job of the cross-compile test matrix (T0537), which runs them on real
// targets rather than emulating them locally.
func crossExecCommand(ctx context.Context, target, binaryPath string, args ...string) (*exec.Cmd, error) {
	switch {
	case isWasmWebTarget(target):
		// The Node harness bootstraps the module itself and has nowhere to put
		// guest argv, so refuse rather than silently dropping the arguments.
		if len(args) > 0 {
			return nil, fmt.Errorf("cannot pass program arguments to a %s binary: the Node harness does not forward argv", target)
		}
		return runWasmWeb(ctx, binaryPath), nil
	case isWasmTarget(target):
		// wasmtime forwards trailing arguments to the guest as argv.
		return exec.CommandContext(ctx, "wasmtime", append([]string{binaryPath}, args...)...), nil
	case isHostTarget(target):
		return exec.CommandContext(ctx, binaryPath, args...), nil
	default:
		return nil, canExecuteTarget(target)
	}
}

// canExecuteTarget reports whether binaries built for target can run on this
// host, without constructing (and therefore without materializing the harness
// for) a command. Callers use it to fail before doing expensive work.
func canExecuteTarget(target string) error {
	return canExecuteTargetFor(runtime.GOOS, runtime.GOARCH, target)
}

// canExecuteTargetFor is canExecuteTarget with the host named rather than read
// from the process, so a caller's platform is a parameter of the answer instead
// of a property of wherever the question happened to be asked. See
// isHostTargetFor for why that matters here.
func canExecuteTargetFor(goos, goarch, target string) error {
	switch {
	case isWasmWebTarget(target), isWasmTarget(target), isHostTargetFor(goos, goarch, target):
		return nil
	default:
		return fmt.Errorf("cannot execute a %s binary on a %s-%s host: cross-target execution is not supported", target, goos, goarch)
	}
}

// isHostTarget reports whether the target triple matches the current host.
// The comparison checks OS and architecture components independently because
// the host triple may include version info (e.g. "arm64-apple-macosx15.0.0").
func isHostTarget(target string) bool {
	return isHostTargetFor(runtime.GOOS, runtime.GOARCH, target)
}

// isHostTargetFor is isHostTarget with the host named rather than read from the
// process, so every host's branch is evaluated wherever the suite runs.
//
// The windows branch is the one that cannot otherwise be checked on the Linux
// and macOS hosts that run almost every build: x86_64-pc-windows-msvc is a
// cross target there and this host's OWN target on windows-amd64, where `run`
// correctly executes what it just built. T2206 was a black-box test asserting
// the refusal for that triple unconditionally — green on every non-Windows run,
// and impossible to satisfy on a Windows one. Same shape, and the same reason,
// as clitest's buildCommandsFor (T2152).
func isHostTargetFor(goos, goarch, target string) bool {
	switch goos { // "darwin", "linux", "windows"
	case "darwin":
		if !isDarwinTarget(target) {
			return false
		}
	case "linux":
		if !strings.Contains(target, "linux") {
			return false
		}
	case "windows":
		if !isWindowsTarget(target) {
			return false
		}
	default:
		return false
	}

	switch goarch { // "amd64", "arm64"
	case "amd64":
		return strings.HasPrefix(target, "x86_64")
	case "arm64":
		return strings.HasPrefix(target, "aarch64") || strings.HasPrefix(target, "arm64")
	default:
		return false
	}
}
