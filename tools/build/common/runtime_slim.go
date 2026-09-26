package common

import (
	"fmt"
	"path/filepath"
	"strings"
)

// runtime_slim.go is the WASM test runtimes' counterpart of llvm_slim.go and
// musl_slim.go (T2169). It lets `bin/gate`, `bin/test` and `bin/prereqs` obtain
// `wasmtime` and `node` from the pinned prebuilts — the catalog's brotli-11
// blobs, or the pinned upstream archive on a catalog miss — instead of asking
// the host's PATH what it happens to have installed.
//
// These two were the last toolchain inputs taken from the host. The rule they
// now obey is T2108's: a build or a measurement must not vary with incidental
// host state. It bit harder here than anywhere else, because a missing runtime
// did not produce a wrong answer — it produced NO answer. `measureTargetSuite`
// reported an incomplete reason and measured nothing, so on a host without
// wasmtime the wasm gate said nothing whatever about the tree, and a wasm-only
// regression could land unnoticed. The version axis was worse still: `node`
// meant whatever the developer or the CI image installed, so the same tree
// passing under one version and failing under another was indistinguishable
// from a real regression.
//
// **HOST dependencies, not target ones.** This is the axis that differs from
// musl_slim.go / openssl_slim.go / compiler_rt_slim.go, whose `target` is the
// Linux target being linked FOR. Here the target is the machine that RUNS the
// runtime, so callers pass CurrentBuildTarget() — a darwin host testing
// wasm32-wasi wants the darwin wasmtime, not the wasm module's target.
//
// **One file each**, per prebuilts.toml: the runtime executable and nothing
// else. Neither invocation (`wasmtime <module>`, `node <harness.js> <module>`)
// needs a supporting tree.
//
// This is the BUILD-TOOL half. The compiler resolves the same two runtimes for
// itself from the content-addressed store (resolveRuntimeView in
// compiler/cmd/promise/llvm_cas.go), preferring this cache when it is already
// populated — exactly the split LLVM already has, and a forced one: `bin/gate`
// refuses to emit a measurement under a toolchain override, so the gate cannot
// hand the compiler a path through PROMISE_WASMTIME and the compiler must be
// able to find its own.

// WasmRuntimeDeps returns the WASM test runtimes, in a stable order. Adding one
// means touching this list and prebuilts.toml, in one place each.
func WasmRuntimeDeps() []string { return []string{"wasmtime", "node"} }

// runtimeFallbackHints is the upstream-archive download size quoted in
// ensureSlimBlobs's catalog-miss note, so the message says what the fallback
// actually costs rather than leaving a maintainer to guess.
var runtimeFallbackHints = map[string]string{
	"wasmtime": "~12 MB",
	"node":     "~30 MB",
}

// RuntimeExeName returns a runtime's executable file name on `target` — the
// prebuilts.toml `out` value for that cell ("node" / "node.exe").
//
// The target is named rather than read from the process for the reason
// docs/code-style.md gives under "Native and cross targets in tests": a
// per-platform answer derived from runtime.GOOS can only ever be checked on the
// platform the suite happens to run on, and the Windows branch here would
// otherwise be asserted nowhere.
func RuntimeExeName(dep, target string) string {
	if strings.HasPrefix(target, "windows-") {
		return dep + ".exe"
	}
	return dep
}

// RuntimeManifestName is the runtime-manifest logical name for a WASM runtime,
// e.g. "node" → "runtime-node".
//
// Unlike targetDepManifestName there is no arch or file component: these are
// host dependencies, so one host manifest describes exactly one of each, and
// the file name is the host's own spelling of it (RuntimeExeName). Keep in
// lockstep with runtimeManifestName in compiler/cmd/promise/llvm_cas.go —
// separate Go modules, so the format is duplicated by necessity (pinned by
// TestRuntimeManifestName on both sides).
func RuntimeManifestName(dep string) string { return "runtime-" + dep }

// EnsureWasmtime returns the path to the pinned `wasmtime` executable for this
// host, fetching it if this machine has not cached it yet. It is what the
// wasm32-wasi gate asks instead of "does this host have wasmtime".
func EnsureWasmtime(root string) (string, error) { return EnsureWasmRuntime(root, "wasmtime") }

// EnsureNode returns the path to the pinned `node` executable for this host,
// fetching it if this machine has not cached it yet. Node runs the wasm32-web
// harness (compiler/cmd/promise/wasm_web_harness.js).
func EnsureNode(root string) (string, error) { return EnsureWasmRuntime(root, "node") }

// EnsureWasmRuntime is the shared body of EnsureWasmtime / EnsureNode: fetch
// `dep` for this host through the slim-blob path every pinned dependency uses,
// then return the absolute path of the one executable it carries.
//
// Safe for concurrent invocation across processes (per-target flock +
// content-addressed `tools.ok`), same as EnsureLLVMBlobs.
func EnsureWasmRuntime(root, dep string) (string, error) {
	hint, ok := runtimeFallbackHints[dep]
	if !ok {
		return "", fmt.Errorf("%q is not a WASM test runtime (want one of %s)", dep, strings.Join(WasmRuntimeDeps(), ", "))
	}
	target := CurrentBuildTarget()
	dir, err := ensureSlimBlobs(root, dep, target, hint, ensureRuntimeSignature)
	if err != nil {
		return "", err
	}
	exe := filepath.Join(dir, RuntimeExeName(dep, target))
	if !Exists(exe) {
		return "", fmt.Errorf("pinned %s for %s did not materialize at %s", dep, target, exe)
	}
	return exe, nil
}
