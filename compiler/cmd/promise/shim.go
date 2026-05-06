package main

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"djabi.dev/go/promise_lang/internal/module"
)

// shimExcludedCommands are commands that always run on the current binary,
// never dispatched to another epoch's compiler.
var shimExcludedCommands = map[string]bool{
	"install": true,
	"sync":    true,
	"epochs":  true,
	"use":     true,
	"init":    true,
}

// shimDispatch checks whether the current binary should delegate execution to
// a different epoch's compiler. Called at the very top of main(), before command
// parsing. Returns without action when:
//   - PROMISE_NO_SHIM=1 (prevents infinite recursion)
//   - the command is in shimExcludedCommands
//   - the desired epoch matches the current binary's epoch
//
// On epoch mismatch, execs into the target epoch's binary (Unix: syscall.Exec,
// Windows: os.StartProcess + wait). Sets PROMISE_NO_SHIM=1 in the child env.
func shimDispatch() {
	// 1. Guard: prevent infinite recursion.
	if os.Getenv("PROMISE_NO_SHIM") == "1" {
		return
	}

	// 2. Check for excluded commands (always run on current binary).
	if len(os.Args) >= 2 {
		if shimExcludedCommands[os.Args[1]] {
			return
		}
	}

	// 3. Determine the desired epoch.
	desiredEpoch := resolveDesiredEpoch()
	if desiredEpoch == "" {
		return // no epoch could be determined; fall through to normal execution
	}

	// 3a. PROMISE_EPOCH can be an absolute path to a binary — exec it directly.
	if filepath.IsAbs(desiredEpoch) {
		if _, err := os.Stat(desiredEpoch); err != nil {
			fmt.Fprintf(os.Stderr, "error: PROMISE_EPOCH binary not found: %s\n", desiredEpoch)
			os.Exit(1)
		}
		shimExec(desiredEpoch, os.Args, shimEnv())
		return // unreachable on Unix (shimExec replaces process)
	}

	// 4. Determine this binary's epoch.
	myEpoch, err := module.CompilerEpoch(embeddedCatalog)
	if err != nil {
		return // can't determine own epoch; fall through
	}

	// 5. If same epoch, proceed normally.
	if desiredEpoch == myEpoch {
		return
	}

	// 6. Find the target epoch's binary.
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

	// 7. Exec into the target binary with PROMISE_NO_SHIM=1.
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
