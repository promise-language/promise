// Command make builds all Promise build tools into bin/, plus the flow binaries
// into bin/flow/.
//
// Usage: go run ./tools/build/cmd/make
//
// This is the bootstrap entry point. It compiles every tool under
// tools/build/cmd/ (except itself) and places the binaries in bin/.
// Each binary gets the current tools source hash injected via ldflags
// so it can detect when it becomes stale.
//
// It also builds the project's flow binaries (the stateless per-step workflow
// executables under flows/) into bin/flow/ — see buildFlows. The flows live in
// their own Go module and depend on two git submodules wired in via local
// replaces: flow-sdk/ (the tracker backend) and flow/ (the OSS flow substrate);
// ./make checks them out with `git submodule update --init`.
package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/promise-language/promise/tools/build/common"
)

func main() {
	start := time.Now()

	root, err := common.FindRoot()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	hash, err := common.ToolsSourceHash(root)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error computing source hash: %v\n", err)
		os.Exit(1)
	}

	binDir := filepath.Join(root, "bin")

	// Discover all cmd/ subdirectories (excluding "make" itself).
	cmdDir := filepath.Join(root, "tools", "build", "cmd")
	entries, err := os.ReadDir(cmdDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error reading cmd/: %v\n", err)
		os.Exit(1)
	}

	var tools []string
	for _, e := range entries {
		if e.IsDir() && e.Name() != "make" {
			tools = append(tools, e.Name())
		}
	}

	if len(tools) == 0 {
		fmt.Println("no tools found to build")
		return
	}

	// Configure git hooks before any short-circuit. ./make is the bootstrap
	// entry point — running it once on a fresh clone enables hooks. Idempotent
	// and fast (a single `git config` call), so it's safe to do unconditionally.
	if err := common.RunSetup(root); err != nil {
		fmt.Fprintf(os.Stderr, "warning: git hooks setup failed: %v\n", err)
	}

	// Quick up-to-date check (skip with -force)
	args := common.NormalizeArgs(os.Args[1:])
	force := slices.Contains(args, "-force")
	hashFile := filepath.Join(binDir, ".tools.hash")
	upToDate := false
	if !force {
		if stored, err := os.ReadFile(hashFile); err == nil && strings.TrimSpace(string(stored)) == hash {
			allExist := true
			for _, name := range tools {
				if !common.Exists(filepath.Join(binDir, name+common.ExeSuffix())) {
					allExist = false
					break
				}
			}
			upToDate = allExist
		}
	}

	if upToDate {
		fmt.Printf("Tools up to date (%d tools, hash: %s..)\n", len(tools), hash[:12])
	} else if err := buildTools(root, binDir, tools, hash); err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(1)
	} else {
		// Write hash sidecar for up-to-date check
		os.WriteFile(hashFile, []byte(hash+"\n"), 0o644)
		// Invalidate gate values — tools changed, prior verify results are stale
		common.InvalidateGateValues(root)
	}

	// Flow binaries (flows/ → bin/flow/) are independent of the tools hash, so they
	// keep their own up-to-date check (bin/flow/.flows.hash over flows/ + flow-sdk/
	// + flow/). buildFlows skips the rebuild when none changed; -force rebuilds them.
	if err := buildFlows(root, force); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("done (%s)\n", time.Since(start).Round(time.Millisecond))
}

// buildTools compiles every discovered build tool into bin/. It returns an error
// (naming the failed count) if any tool fails to build.
func buildTools(root, binDir string, tools []string, hash string) error {
	fmt.Printf("Building %d tools (hash: %s..)\n", len(tools), hash[:12])

	ldflags := fmt.Sprintf("-s -w -X main.sourceHash=%s", hash)
	failed := 0
	for _, name := range tools {
		pkg := "./cmd/" + name
		out := filepath.Join(binDir, name+common.ExeSuffix())
		err := common.RunIn(
			filepath.Join(root, "tools", "build"),
			"go", "build", "-trimpath",
			"-ldflags", ldflags,
			"-o", out,
			pkg,
		)
		if err != nil {
			fmt.Fprintf(os.Stderr, "  FAIL %s: %v\n", name, err)
			failed++
			continue
		}
		info, _ := os.Stat(out)
		size := float64(info.Size()) / (1024 * 1024)
		fmt.Printf("  %-12s %.1f MB\n", name, size)
	}
	if failed > 0 {
		return fmt.Errorf("%d/%d tools failed", failed, len(tools))
	}
	fmt.Printf("%d tools built\n", len(tools))
	return nil
}

// buildFlows builds each flow binary under flows/ into bin/flow/<name>. The flows
// are a separate Go module (flows/go.mod) depending on two local submodules wired
// in via replaces: flow-sdk/ (the tracker backend) and flow/ (the OSS flow
// substrate). bin/flow/ is gitignored; the submodules are not.
//
// Unavailable submodules are non-fatal: the core tool build above already
// succeeded, so ./make warns and skips the flows rather than breaking the whole
// build for someone with no access to the submodule hosts. A genuine flow COMPILE
// error (submodules present) is fatal — that is a real regression in committed
// flow source.
//
// The flow binaries are only (re)built when the flow source or either submodule
// changed: their combined hash is compared against bin/flow/.flows.hash and the
// go-build loop is skipped on a match (unless force). The hash is computed AFTER
// the submodule checkout so a submodule pin bump is detected.
func buildFlows(root string, force bool) error {
	if os.Getenv("PROMISE_SKIP_FLOWS") != "" {
		return nil // CI runners set this to avoid the doomed flow-sdk SSH fetch (T0788)
	}
	flowsDir := filepath.Join(root, "flows")
	if !common.Exists(filepath.Join(flowsDir, "go.mod")) {
		return nil // no flows module — nothing to build
	}
	if err := ensureFlowSubmodules(root); err != nil {
		fmt.Fprintf(os.Stderr, "warning: skipping flow binaries — flow submodules unavailable: %v\n", err)
		return nil
	}
	writeFlowRootMarker(root) // best-effort: lets hand-run flows locate the worktree root

	binFlow := filepath.Join(root, "bin", "flow")
	if err := os.MkdirAll(binFlow, 0o755); err != nil {
		return fmt.Errorf("mkdir bin/flow: %w", err)
	}

	// Discover the flow packages (subdirs of flows/ that contain .go files); the
	// internal/ helper packages and any stray non-package directory are skipped
	// rather than built as flow binaries. This is pure directory reading — no `go`
	// invocation — so it is safe to do before the up-to-date short-circuit.
	entries, err := os.ReadDir(flowsDir)
	if err != nil {
		return fmt.Errorf("read flows/: %w", err)
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() || e.Name() == "internal" || !hasGoFiles(filepath.Join(flowsDir, e.Name())) {
			continue
		}
		names = append(names, e.Name())
	}
	if len(names) == 0 {
		return nil // no flow packages — nothing to build
	}

	// Up-to-date check FIRST — before any `go` invocation (tidy, buildhash, build).
	// Skip the rebuild when none of flows/, flow-sdk/, flow/ changed and every
	// expected binary is present. Mirrors the .tools.hash short-circuit in main, so
	// a no-op ./make (run before every flow spawn) pays neither `go mod tidy` nor a
	// per-flow go-build each time.
	hash, herr := common.FlowsSourceHash(root)
	hashFile := filepath.Join(binFlow, ".flows.hash")
	if !force && herr == nil {
		if stored, rerr := os.ReadFile(hashFile); rerr == nil && strings.TrimSpace(string(stored)) == hash {
			allExist := true
			for _, name := range names {
				if !common.Exists(filepath.Join(binFlow, name+common.ExeSuffix())) {
					allExist = false
					break
				}
			}
			if allExist {
				fmt.Printf("Flows up to date (%d flows, hash: %s..)\n", len(names), hash[:12])
				return nil
			}
		}
	}

	// A rebuild is needed. Heal flows/go.mod only on genuine import-graph drift —
	// never mutate it on every build (T0750). See tidyFlowsIfDrift.
	if err := tidyFlowsIfDrift(flowsDir); err != nil {
		return err
	}

	// Compute the flow source hash to bake into each binary, so it can detect at
	// runtime when flows/, flow-sdk/, or flow/ changed since it was built — the same
	// staleness self-check the other bin/ tools have. The flows module is separate
	// (it depends on the flow-sdk/ and flow/ submodules), so make cannot import its
	// hasher; it runs the flows module's own buildhash helper instead, guaranteeing
	// the build-time and runtime hashes are computed by identical code. ldflags omits
	// -s -w so the flow binaries stay debuggable (see .vscode launch config).
	flowHash, err := common.RunOutputIn(flowsDir, "go", "run", "-mod=readonly", "./internal/buildhash")
	if err != nil {
		return fmt.Errorf("compute flow source hash: %w", err)
	}
	ldflags := "-X main.sourceHash=" + flowHash

	for _, name := range names {
		out := filepath.Join(binFlow, name+common.ExeSuffix())
		// -mod=readonly makes the build immune to whatever GOFLAGS the runner sets
		// and guarantees it can never silently mutate go.mod/go.sum (T0750).
		if err := common.RunIn(flowsDir, "go", "build", "-mod=readonly", "-ldflags", ldflags, "-o", out, "./"+name); err != nil {
			dumpFlowsBuildContext(flowsDir) // best-effort: make a recurrence self-explaining
			return fmt.Errorf("build flow %s: %w", name, err)
		}
		fmt.Printf("  flow/%-7s built\n", name)
	}
	fmt.Printf("%d flows built\n", len(names))

	// Record the hash only after a fully successful build, so a mid-way failure
	// forces a retry next run rather than being masked by a matching sidecar.
	// Recompute it AFTER the drift-gated tidy so it reflects any heal it applied —
	// the same tree the next up-to-date check will hash (in the common clean case
	// nothing was mutated, so this just re-hashes the unchanged tree). If hashing
	// failed, skip writing — next run recomputes and rebuilds.
	if finalHash, ferr := common.FlowsSourceHash(root); ferr == nil {
		_ = os.WriteFile(hashFile, []byte(finalHash+"\n"), 0o644)
	}
	return nil
}

// tidyFlowsIfDrift brings flows/go.mod and go.sum into a buildable state WITHOUT
// mutating them in the common case. The committed flows/go.mod is the source of
// truth and, for the pinned flow/flow-sdk submodules, is already complete (do
// reaches only stdlib + the two locally-replaced modules; flow/go.mod's
// go-github/yaml are pruned out). In that case `go mod tidy -diff` writes nothing
// and exits 0 (offline-clean, no network), so the subsequent readonly build runs
// hermetically and identically on every platform.
//
// Only a submodule bump that genuinely changes the import graph makes go.mod
// drift; -diff detects that and exits non-zero, and we self-heal with a real
// `go mod tidy`. An old Go without the -diff flag also exits non-zero and falls
// through to the real tidy, preserving prior behavior on that rare path.
//
// This replaced an UNCONDITIONAL build-time `go mod tidy` (T0750): on the Windows
// gate that tidy rewrote go.mod into a state the readonly `go build` then rejected
// ("updates to go.mod needed; to update it: go mod tidy"), and it dirtied the
// worktree (triggering orchestrator "worktree is dirty" skips).
func tidyFlowsIfDrift(flowsDir string) error {
	if _, diffErr := common.RunOutputIn(flowsDir, "go", "mod", "tidy", "-diff"); diffErr != nil {
		if err := common.RunIn(flowsDir, "go", "mod", "tidy"); err != nil {
			return fmt.Errorf("go mod tidy (flows): %w", err)
		}
	}
	return nil
}

// dumpFlowsBuildContext prints, to stderr, the environment and module state that
// most often explains a flows `go build` failure (T0750): the relevant `go env`
// knobs, the current flows/go.mod, and `go mod tidy -diff` output. It is
// best-effort — every probe's error is swallowed — so a recurrence is
// self-explaining instead of a bare `exit status 1`, especially on CI runners
// (e.g. Windows) we cannot reproduce locally.
func dumpFlowsBuildContext(flowsDir string) {
	fmt.Fprintln(os.Stderr, "--- flows build failed; dumping context (T0750) ---")
	if env, err := common.RunOutputIn(flowsDir, "go", "env", "GOFLAGS", "GOPROXY", "GOTOOLCHAIN", "GOMODCACHE"); err == nil {
		fmt.Fprintf(os.Stderr, "go env GOFLAGS/GOPROXY/GOTOOLCHAIN/GOMODCACHE:\n%s\n", env)
	}
	if mod, err := os.ReadFile(filepath.Join(flowsDir, "go.mod")); err == nil {
		fmt.Fprintf(os.Stderr, "flows/go.mod:\n%s\n", mod)
	}
	// `go mod tidy -diff` exits non-zero on drift (and on an old Go without the
	// flag), so capture combined output directly — the diff text is exactly what
	// we want printed even when the command exits non-zero.
	diffCmd := exec.Command("go", "mod", "tidy", "-diff")
	diffCmd.Dir = flowsDir
	if diff, _ := diffCmd.CombinedOutput(); len(diff) > 0 {
		fmt.Fprintf(os.Stderr, "go mod tidy -diff:\n%s\n", diff)
	}
	fmt.Fprintln(os.Stderr, "--- end flows build context ---")
}

// ensureFlowSubmodules makes the flow submodules available at <root>/flow-sdk
// (tracker backend) and <root>/flow (OSS flow substrate), wired into the flows
// module via flows/go.mod's local replaces. It runs `git submodule update --init`
// for both, which on a fresh clone registers and checks them out, and otherwise
// checks out the pinned gitlink commit (a no-op, no network, when already at it).
//
// Submodules are PINNED — update deliberately checks out the recorded commit
// rather than fast-forwarding, so the flow-sdk/flow pair stays the tested,
// reproducible combination until the gitlink is intentionally bumped.
//
// It returns an error only when a submodule is absent AND the checkout fails (no
// access to the host, offline), so buildFlows can warn and skip the flows rather
// than break the core build. When both are already present it is fast and never
// fails the build.
func ensureFlowSubmodules(root string) error {
	const sdk, oss = "flow-sdk", "flow"
	haveSDK := common.Exists(filepath.Join(root, sdk, "go.mod"))
	haveOSS := common.Exists(filepath.Join(root, oss, "go.mod"))
	if haveSDK && haveOSS {
		// Both checked out — refresh to the pinned commits (cheap, offline-safe:
		// no fetch when the gitlink already matches the checkout). A failure here
		// (e.g. a dirty submodule) is non-fatal; the existing checkout is used.
		if err := runGit(root, "submodule", "update", "--init", "--", sdk, oss); err != nil {
			fmt.Fprintf(os.Stderr, "warning: flow submodule refresh failed (using existing checkout): %v\n", err)
		}
		return nil
	}
	// At least one is missing — initialize and fetch it. A failure here IS
	// returned so buildFlows skips the flows (the caller treats it as a warning).
	fmt.Println("Initializing flow submodules (flow-sdk/, flow/)")
	return runGit(root, "submodule", "update", "--init", "--", sdk, oss)
}

// gitTimeout bounds each flow submodule git operation. The runner runs ./make
// before every flow spawn, so an UNBOUNDED git network hang here (a host
// partitioned, not refused) would stall flow dispatch indefinitely. A bounded op
// fails loudly instead.
const gitTimeout = 90 * time.Second

// runGit runs `git args...` in dir with stdout/stderr attached, bounded by
// gitTimeout. A timeout is reported distinctly so the cause is obvious.
func runGit(dir string, args ...string) error {
	ctx, cancel := context.WithTimeout(context.Background(), gitTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return fmt.Errorf("git %s: timed out after %s", strings.Join(args, " "), gitTimeout)
		}
		return fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
	}
	return nil
}

// writeFlowRootMarker writes the permanent worktree-root marker the flow SDK
// walks up to find (DiscoverWorktreeRoot: <root>/.flow/root). Best-effort — a
// runner pins the worktree via FLOW_WORKTREE, so this only aids hand-run flows.
// .flow/ is kept out of git by the committed .gitignore entry (the SDK also
// excludes it via .git/info/exclude when it writes a lease).
func writeFlowRootMarker(root string) {
	flowDir := filepath.Join(root, ".flow")
	if err := os.MkdirAll(flowDir, 0o755); err != nil {
		return
	}
	_ = os.WriteFile(filepath.Join(flowDir, "root"), []byte(root+"\n"), 0o644)
}

// hasGoFiles reports whether dir contains at least one .go file (so a stray
// non-package directory under flows/ is skipped rather than failing the build).
func hasGoFiles(dir string) bool {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".go") {
			return true
		}
	}
	return false
}
