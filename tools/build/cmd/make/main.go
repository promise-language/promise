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
