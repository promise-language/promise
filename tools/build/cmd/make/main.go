// Command make builds all Promise build tools into bin/.
//
// Usage: go run ./tools/build/cmd/make
//
// This is the bootstrap entry point. It compiles every tool under
// tools/build/cmd/ (except itself) and places the binaries in bin/.
// Each binary gets the current tools source hash injected via ldflags
// so it can detect when it becomes stale.
package main

import (
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

	// Deferred, not called at the end of main: this function has a success path
	// that returns early (no tools found), and any future one would silently skip
	// a tail call — the hook would be wired up, make.local would be written, and
	// the tooling would never sync. A defer runs on every path that returns, and
	// is skipped by os.Exit, which every failure path here uses. That is exactly
	// the rule: run on success, never on failure.
	defer runLocalHook(root, os.Args[1:])

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

	fmt.Printf("done (%s)\n", time.Since(start).Round(time.Millisecond))
}

// runLocalHook runs <root>/make.local if this clone has one, so that building
// promise's own tools also refreshes the workspace-provided tooling installed
// alongside them. `workspace setup` writes that file (it is gitignored, never
// authored here) and only writes it to projects whose ./make calls it.
//
// Three properties are deliberate:
//
//   - It runs only when the file exists and is executable, so a clone that was
//     never provisioned is unaffected — this is not a new dependency.
//   - A hook failure never fails the build. The build above already succeeded,
//     and failing here would attribute someone else's problem to it.
//   - The original arguments are passed through, so `./make -force` forces the
//     workspace build too: someone who distrusts this build output has no reason
//     to trust the tooling's, and the tooling is where a wrong up-to-date answer
//     does the most damage, since every project's binaries come from it.
//
// It is skipped when stderr is not a terminal, keeping CI and scripted builds to
// exactly the work they asked for.
func runLocalHook(root string, args []string) {
	hook := filepath.Join(root, "make.local")
	info, err := os.Stat(hook)
	if err != nil || info.IsDir() || info.Mode()&0o111 == 0 {
		return
	}
	if stat, err := os.Stderr.Stat(); err != nil || stat.Mode()&os.ModeCharDevice == 0 {
		return
	}
	cmd := exec.Command(hook, args...)
	cmd.Dir = root
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "\n./make: local hook failed (the build itself succeeded)\n")
		fmt.Fprintf(os.Stderr, "        re-run it alone with: %s\n\n", hook)
	}
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
