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
// their own Go module and depend on the flow SDK, which is fetched on demand
// into flow-sdk/ (gitignored, not a git submodule).
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

	"github.com/p5e-ia/promise-lang/tools/build/common"
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

	// Flow binaries (flows/ → bin/flow/) build on demand regardless of the tools
	// up-to-date check: their source and the flow SDK are independent of the tools
	// hash, and `go build` is incremental, so this is a no-op when nothing changed.
	if err := buildFlows(root); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("\ndone (%s)\n", time.Since(start).Round(time.Millisecond))
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

// flowSDKRepo is the git URL of the flow SDK (djabi.dev/go/flow_sdk). Its vanity
// module path has no public proxy / go-get resolver, so it is fetched on demand
// into <root>/flow-sdk and wired in via the flows module's local replace (see
// flows/go.mod) — NOT a git submodule. flow-sdk/ is gitignored.
const flowSDKRepo = "ssh://hfe/git/tracker_flow_sdk.git"

// buildFlows builds each flow binary under flows/ into bin/flow/<name>. The flows
// are a separate Go module (flows/go.mod) depending on the flow SDK, which is
// fetched on demand into flow-sdk/. Both flow-sdk/ and bin/flow/ are gitignored.
//
// A missing/unfetchable SDK is non-fatal: the core tool build above already
// succeeded, so ./make warns and skips the flows rather than breaking the whole
// build for someone with no access to the SDK host. A genuine flow COMPILE error
// (SDK present) is fatal — that is a real regression in committed flow source.
func buildFlows(root string) error {
	flowsDir := filepath.Join(root, "flows")
	if !common.Exists(filepath.Join(flowsDir, "go.mod")) {
		return nil // no flows module — nothing to build
	}
	if err := ensureFlowSDK(root); err != nil {
		fmt.Fprintf(os.Stderr, "warning: skipping flow binaries — flow SDK unavailable: %v\n", err)
		return nil
	}
	writeFlowRootMarker(root) // best-effort: lets hand-run flows locate the worktree root

	binFlow := filepath.Join(root, "bin", "flow")
	if err := os.MkdirAll(binFlow, 0o755); err != nil {
		return fmt.Errorf("mkdir bin/flow: %w", err)
	}

	entries, err := os.ReadDir(flowsDir)
	if err != nil {
		return fmt.Errorf("read flows/: %w", err)
	}
	built := 0
	for _, e := range entries {
		if !e.IsDir() || !hasGoFiles(filepath.Join(flowsDir, e.Name())) {
			continue
		}
		name := e.Name()
		out := filepath.Join(binFlow, name+common.ExeSuffix())
		if err := common.RunIn(flowsDir, "go", "build", "-o", out, "./"+name); err != nil {
			return fmt.Errorf("build flow %s: %w", name, err)
		}
		fmt.Printf("  flow/%-7s built\n", name)
		built++
	}
	if built > 0 {
		fmt.Printf("%d flow binary(ies) built\n", built)
	}
	return nil
}

// ensureFlowSDK makes the flow SDK available at <root>/flow-sdk. It clones it on
// first use; when already present it does a best-effort fast-forward (warn, never
// fail) so the flows track SDK changes without breaking an offline build. It only
// returns an error when the SDK is absent AND the clone fails (so buildFlows can
// skip the flows entirely rather than break the core build).
func ensureFlowSDK(root string) error {
	sdk := filepath.Join(root, "flow-sdk")
	if !common.Exists(filepath.Join(sdk, "go.mod")) {
		// A stray/partial flow-sdk/ (e.g. an interrupted clone) has no go.mod and
		// would make `git clone` fail forever ("destination path already exists and
		// is not empty"), wedging flow builds. Clear it so the clone can recover.
		if common.Exists(sdk) {
			if err := os.RemoveAll(sdk); err != nil {
				return fmt.Errorf("remove stale flow-sdk/: %w", err)
			}
		}
		fmt.Printf("Fetching flow SDK (%s) -> flow-sdk/\n", flowSDKRepo)
		return runGit(root, "clone", flowSDKRepo, sdk)
	}
	if err := runGit(sdk, "pull", "--ff-only"); err != nil {
		fmt.Fprintf(os.Stderr, "warning: flow SDK fast-forward failed (using existing checkout): %v\n", err)
	}
	return nil
}

// gitTimeout bounds each flow-SDK git operation. The runner runs ./make before
// every flow spawn, so an UNBOUNDED git network hang here (hfe partitioned, not
// refused) would stall flow dispatch indefinitely — re-introducing the very stall
// class the flow redesign removes. A bounded op fails loudly instead.
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
