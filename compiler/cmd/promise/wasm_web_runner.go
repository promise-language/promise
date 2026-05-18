package main

import (
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"djabi.dev/go/promise_lang/internal/module"
)

// embeddedWasmWebHarness is the Node.js script that runs wasm32-web binaries.
// See compiler/cmd/promise/wasm_web_harness.js for the source.
//
//go:embed wasm_web_harness.js
var embeddedWasmWebHarness []byte

// isWasmWebTarget returns true if the target triple is wasm32-web (browser
// host, no WASI imports — runs via Node + the embedded harness in tests).
func isWasmWebTarget(target string) bool {
	return strings.Contains(target, "wasm") && strings.Contains(target, "web")
}

// materializeWebHarness extracts the embedded Node harness to a stable cache
// path keyed by content hash, so repeated runs reuse the same file. Returns
// the absolute path to the materialized harness.
func materializeWebHarness() (string, error) {
	sum := sha256.Sum256(embeddedWasmWebHarness)
	hash := hex.EncodeToString(sum[:8]) // 16 hex chars

	promiseHome, err := module.PromiseHome()
	if err != nil {
		// Fall back to a per-process tempfile if PROMISE_HOME is unavailable.
		f, ferr := os.CreateTemp("", "promise-wasm-web-harness-*.js")
		if ferr != nil {
			return "", fmt.Errorf("materialize harness: %w (and %v)", err, ferr)
		}
		if _, werr := f.Write(embeddedWasmWebHarness); werr != nil {
			f.Close()
			os.Remove(f.Name())
			return "", werr
		}
		f.Close()
		return f.Name(), nil
	}

	cacheDir := filepath.Join(promiseHome, "cache", "wasm")
	harnessPath := filepath.Join(cacheDir, "web_harness_"+hash+".js")

	// Reuse if size matches — content-hashed name guarantees identical bytes.
	if info, err := os.Stat(harnessPath); err == nil && info.Size() == int64(len(embeddedWasmWebHarness)) {
		return harnessPath, nil
	}
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		return "", err
	}
	if err := os.WriteFile(harnessPath, embeddedWasmWebHarness, 0o644); err != nil {
		return "", err
	}
	return harnessPath, nil
}

// nodeMissingError returns a friendly install hint when the `node` executable
// can't be found on PATH. wasm32-web tests require Node.js (>= 20).
func nodeMissingError() error {
	var hint string
	switch runtime.GOOS {
	case "windows":
		hint = "winget install OpenJS.NodeJS"
	case "darwin":
		hint = "brew install node"
	default:
		hint = "sudo apt-get install nodejs (or https://nodejs.org/)"
	}
	return fmt.Errorf("node not found on PATH — install Node.js 20+: %s", hint)
}

// runWasmWeb constructs an *exec.Cmd that runs binaryPath under Node + the
// embedded harness, scoped to ctx for timeout enforcement. Callers attach
// stdin/stdout/stderr/process-group as they would for any other test binary.
//
// On lookup failure (node missing) or harness materialization failure, prints
// a friendly diagnostic to stderr and exits with code 1 — matching how
// wasmtime-missing failures are surfaced today (the existing wasmtime path
// just lets exec.LookPath fail at exec time).
func runWasmWeb(ctx context.Context, binaryPath string) *exec.Cmd {
	nodePath, err := exec.LookPath("node")
	if err != nil {
		fmt.Fprintln(os.Stderr, nodeMissingError())
		os.Exit(1)
	}
	harnessPath, err := materializeWebHarness()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: cannot materialize wasm32-web harness: %v\n", err)
		os.Exit(1)
	}
	return exec.CommandContext(ctx, nodePath, harnessPath, binaryPath)
}
