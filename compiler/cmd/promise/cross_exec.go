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
	switch {
	case isWasmWebTarget(target), isWasmTarget(target), isHostTarget(target):
		return nil
	default:
		return fmt.Errorf("cannot execute a %s binary on a %s-%s host: cross-target execution is not supported", target, runtime.GOOS, runtime.GOARCH)
	}
}

// isHostTarget reports whether the target triple matches the current host.
// The comparison checks OS and architecture components independently because
// the host triple may include version info (e.g. "arm64-apple-macosx15.0.0").
func isHostTarget(target string) bool {
	hostOS := runtime.GOOS     // "darwin", "linux", "windows"
	hostArch := runtime.GOARCH // "amd64", "arm64"

	switch hostOS {
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

	switch hostArch {
	case "amd64":
		return strings.HasPrefix(target, "x86_64")
	case "arm64":
		return strings.HasPrefix(target, "aarch64") || strings.HasPrefix(target, "arm64")
	default:
		return false
	}
}
