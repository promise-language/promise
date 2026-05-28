package main

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/promise-language/promise/compiler/internal/module"
)

// shimExcludedCommands are commands that always run on the current binary,
// never dispatched to another epoch's compiler.
var shimExcludedCommands = map[string]bool{
	"install": true,
	"sync":    true,
	"epochs":  true,
	"use":     true,
	"init":    true,
	"remove":  true,
}

// shimDispatch checks whether the current binary should delegate execution to
// a different epoch's compiler. Called at the very top of main(), before command
// parsing. Returns without action when:
//   - PROMISE_NO_SHIM=1 (prevents infinite recursion)
//   - no .promise.shim marker file next to binary (B0251: dev builds never shim)
//   - the command is in shimExcludedCommands
//   - the desired epoch matches the current binary's epoch
//
// Exception: PROMISE_EPOCH set to an absolute path always works (explicit
// developer override), even without a marker file.
//
// On epoch mismatch, execs into the target epoch's binary (Unix: syscall.Exec,
// Windows: os.StartProcess + wait). Sets PROMISE_NO_SHIM=1 in the child env.
func shimDispatch() {
	// 1. Guard: prevent infinite recursion.
	if os.Getenv("PROMISE_NO_SHIM") == "1" {
		return
	}

	// 2. Guard: only shim if marker file exists next to this binary (B0251).
	//    Only installed binaries (~/.promise/bin/promise) get .promise.shim
	//    from `promise install`. Dev builds and epoch binaries do not.
	markerPresent := hasShimMarker()

	// 2a. PROMISE_EPOCH as absolute path bypasses the marker check (explicit override).
	if env := os.Getenv("PROMISE_EPOCH"); env != "" && filepath.IsAbs(env) {
		if _, err := os.Stat(env); err != nil {
			fmt.Fprintf(os.Stderr, "error: PROMISE_EPOCH binary not found: %s\n", env)
			os.Exit(1)
		}
		shimExec(env, os.Args, shimEnv())
		return // unreachable on Unix (shimExec replaces process)
	}

	// 2b. No marker → not an installed binary; skip shimming entirely.
	if !markerPresent {
		return
	}

	// 3. Check for excluded commands (always run on current binary).
	if len(os.Args) >= 2 {
		if shimExcludedCommands[os.Args[1]] {
			return
		}
	}

	// 4. Determine the desired epoch.
	desiredEpoch := resolveDesiredEpoch()
	if desiredEpoch == "" {
		return // no epoch could be determined; fall through to normal execution
	}

	// 5. Determine this binary's epoch.
	myEpoch, err := module.CompilerEpoch(embeddedCatalog)
	if err != nil {
		return // can't determine own epoch; fall through
	}

	// 6. If same epoch, proceed normally.
	if desiredEpoch == myEpoch {
		return
	}

	// 7. Find the target epoch's binary.
	epochDir, err := module.EpochDir(desiredEpoch)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: cannot resolve epoch directory: %v\n", err)
		os.Exit(1)
	}
	binaryName := "promise"
	if runtime.GOOS == "windows" {
		binaryName = "promise.exe"
	}
	targetBin := filepath.Join(epochDir, "bin", binaryName)
	if _, err := os.Stat(targetBin); err != nil {
		fmt.Fprintf(os.Stderr, "epoch %q is not installed. Run: promise sync %s\n", desiredEpoch, desiredEpoch)
		os.Exit(1)
	}

	// 8. Exec into the target binary with PROMISE_NO_SHIM=1.
	shimExec(targetBin, os.Args, shimEnv())
}

// resolveDesiredEpoch determines the epoch the user wants, using the priority:
//  1. PROMISE_EPOCH env var (developer override)
//  2. FindConfig(cwd) → [module].epoch from promise.toml
//  3. ActiveEpoch() fallback (for single-file mode / scripts)
func resolveDesiredEpoch() string {
	// 1. Env var override.
	if env := os.Getenv("PROMISE_EPOCH"); env != "" {
		return env
	}

	// 2. Project config.
	cwd, err := os.Getwd()
	if err == nil {
		cfg, err := module.FindConfig(cwd)
		if err == nil && cfg != nil && cfg.Epoch != "" {
			return cfg.Epoch
		}
	}

	// 3. Active epoch fallback.
	epoch, err := module.ActiveEpoch()
	if err != nil {
		return "" // no epoch determinable
	}
	return epoch
}

// hasShimMarkerAt returns true if a .promise.shim marker file exists in binDir.
func hasShimMarkerAt(binDir string) bool {
	_, err := os.Stat(filepath.Join(binDir, ".promise.shim"))
	return err == nil
}

// hasShimMarker returns true if a .promise.shim marker file exists next to
// this binary. Only installed binaries (placed by `promise install`) have this
// marker. Dev builds and epoch binaries do not (B0251).
func hasShimMarker() bool {
	exe, err := os.Executable()
	if err != nil {
		return false
	}
	exe, err = filepath.EvalSymlinks(exe)
	if err != nil {
		return false
	}
	return hasShimMarkerAt(filepath.Dir(exe))
}

// shimEnv returns the current environment with PROMISE_NO_SHIM=1 added.
func shimEnv() []string {
	env := os.Environ()
	// Check if PROMISE_NO_SHIM is already set; replace it.
	for i, e := range env {
		if strings.HasPrefix(e, "PROMISE_NO_SHIM=") {
			env[i] = "PROMISE_NO_SHIM=1"
			return env
		}
	}
	return append(env, "PROMISE_NO_SHIM=1")
}
