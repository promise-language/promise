package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/promise-language/promise/compiler/internal/module"
)

// nowFunc returns the current time; overridable in tests so build-index produces
// deterministic `verified_at` stamps.
var nowFunc = time.Now

// runPackageCheckEpoch implements `promise package check-epoch [<epoch>]`: the
// module-owner self-verify (§9.9, the "`promise use E` → `promise test`" loop as
// one command). It verifies the module in the cwd against the epoch (default: the
// running compiler's epoch) and, on success, prints the publish hint (push an
// `epoch-<E>` git tag). Exits non-zero on failure.
func runPackageCheckEpoch(args []string) {
	if len(args) > 1 {
		fmt.Fprintln(os.Stderr, "usage: promise package check-epoch [epoch]")
		fmt.Fprintln(os.Stderr, "  Verifies THIS module (cwd) against an epoch; default is this compiler's epoch.")
		os.Exit(1)
	}

	dir, err := os.Getwd()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	if _, err := os.Stat(filepath.Join(dir, "promise.toml")); err != nil {
		fmt.Fprintln(os.Stderr, "error: no promise.toml in the current directory (run from the module root)")
		os.Exit(1)
	}

	epoch := ""
	if len(args) == 1 {
		epoch = args[0]
	} else {
		e, eerr := module.CompilerEpoch(embeddedCatalog)
		if eerr != nil {
			fmt.Fprintf(os.Stderr, "error: cannot determine this compiler's epoch: %v\n", eerr)
			os.Exit(1)
		}
		epoch = e
	}
	if epoch == module.ChannelNext {
		fmt.Fprintln(os.Stderr, "error: 'next' is a toolchain channel, not an epoch — pass a numeric epoch (e.g. 2026.2)")
		os.Exit(1)
	}
	if _, _, ok := module.ParseEpoch(epoch); !ok {
		fmt.Fprintf(os.Stderr, "error: %q is not a numeric epoch (expected YYYY.N)\n", epoch)
		os.Exit(1)
	}

	compilerBin, err := epochCompilerBin(epoch)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	fmt.Fprintf(os.Stderr, "Verifying this module against epoch %s...\n", epoch)
	ok, reason, verr := verifyLocalModuleCompat(compilerBin, dir, epoch)
	if verr != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", verr)
		os.Exit(1)
	}
	if !ok {
		fmt.Printf("✗ not compatible with epoch %s\n", epoch)
		if reason != "" {
			fmt.Println(indent(reason, "  "))
		}
		os.Exit(1)
	}

	fmt.Printf("✓ compatible with epoch %s\n", epoch)
	fmt.Printf("  publish:  git tag epoch-%s && git push --tags\n", epoch)
}

// runPackageBuildIndex implements `promise package build-index <catalog-dir>
// <epoch> [-report]`: the community-catalog CI matrix builder (§9.9–§9.10). For
// every module listed in <catalog-dir>/modules.toml it resolves + verifies the
// epoch-appropriate commit under the epoch-<E> compiler (the same engine the
// client uses), writes the verified commits into index/<epoch>.json, and
// regenerates matrix.md. This is the authoritative compat record that removes the
// per-user local test run for listed modules.
//
// Exits non-zero when any listed module fails (so CI surfaces it) — except with
// -report (the §9.10 pre-release nudge), which prints the unsupported list and
// exits 0 so it never blocks the release.
func runPackageBuildIndex(args []string) {
	report := false
	var pos []string
	for _, a := range args {
		switch a {
		case "-report", "--report":
			report = true
		default:
			pos = append(pos, a)
		}
	}
	if len(pos) != 2 {
		fmt.Fprintln(os.Stderr, "usage: promise package build-index <catalog-dir> <epoch> [-report]")
		fmt.Fprintln(os.Stderr, "  Verifies every module in <catalog-dir>/modules.toml against <epoch>,")
		fmt.Fprintln(os.Stderr, "  writes index/<epoch>.json, and regenerates matrix.md.")
		os.Exit(1)
	}
	catalogDir, epoch := pos[0], pos[1]

	if epoch == module.ChannelNext {
		fmt.Fprintln(os.Stderr, "error: 'next' is a toolchain channel, not an epoch — pass the numeric epoch it implements")
		os.Exit(1)
	}
	if _, _, ok := module.ParseEpoch(epoch); !ok {
		fmt.Fprintf(os.Stderr, "error: %q is not a numeric epoch (expected YYYY.N)\n", epoch)
		os.Exit(1)
	}

	data, err := os.ReadFile(filepath.Join(catalogDir, "modules.toml"))
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: cannot read %s: %v\n", filepath.Join(catalogDir, "modules.toml"), err)
		os.Exit(1)
	}
	cc, err := module.ParseCommunityModules(data)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	compilerBin, err := epochCompilerBin(epoch)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	names := make([]string, 0, len(cc.Modules))
	for name := range cc.Modules {
		names = append(names, name)
	}
	sort.Strings(names)

	fmt.Printf("Building compatibility index for epoch %s (%d modules):\n\n", epoch, len(names))
	idx := &module.CompatIndex{Epoch: epoch, Modules: make(map[string]module.IndexEntry)}
	stamp := nowFunc().UTC().Format(time.RFC3339)
	var failed []string
	for _, name := range names {
		url := cc.Modules[name].URL
		commit, rerr := resolveEpochAware(compilerBin, epoch, name, url, "", func(string) {})
		if rerr != nil {
			failed = append(failed, name)
			fmt.Printf("  ✗ %s — no version compatible with epoch %s\n", name, epoch)
			continue
		}
		idx.Modules[name] = module.IndexEntry{
			Commit:       commit,
			Tag:          tagForCommit(url, commit),
			VerifiedAt:   stamp,
			CompilerHash: module.CompilerHash(),
		}
		fmt.Printf("  ✓ %s → %s\n", name, shortCommit(commit))
	}

	if err := module.SaveCompatIndex(catalogDir, idx); err != nil {
		fmt.Fprintf(os.Stderr, "error: writing index: %v\n", err)
		os.Exit(1)
	}
	if err := writeMatrix(catalogDir, cc); err != nil {
		fmt.Fprintf(os.Stderr, "error: writing matrix: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("\nVerified %d of %d modules for epoch %s.\n", len(idx.Modules), len(names), epoch)
	fmt.Printf("Wrote index/%s.json and matrix.md\n", epoch)

	if len(failed) > 0 {
		fmt.Printf("\n%d module(s) have no version compatible with epoch %s:\n", len(failed), epoch)
		for _, name := range failed {
			fmt.Printf("  - %s\n", name)
		}
		if report {
			// Pre-release nudge (§9.10): surface the gap, but never fail the run —
			// it cannot force an unmaintained module to update.
			fmt.Println("\n(pre-release report — these authors should publish an epoch-" + epoch + " tag)")
			return
		}
		os.Exit(1)
	}
}

// tagForCommit returns the epoch-* tag in url's repo that points at commit, or ""
// when the commit was resolved via a stable/HEAD fallback (no matching tag). Used
// to record the human-readable tag alongside the pinned commit in the index.
func tagForCommit(url, commit string) string {
	epochTags, _, err := module.ListRepoTags(url)
	if err != nil {
		return ""
	}
	for _, t := range epochTags {
		if t.Commit == commit {
			return t.Tag
		}
	}
	return ""
}

// writeMatrix regenerates matrix.md — the published module × epoch compatibility
// grid — from all per-epoch index files. A ✓ marks a module verified for that
// epoch; — marks no recorded verified version (untested or failed). The index
// files are the single source of truth; this is a derived view.
func writeMatrix(catalogDir string, cc *module.CommunityCatalog) error {
	epochs, err := module.IndexedEpochs(catalogDir)
	if err != nil {
		return err
	}

	// Module rows: the union of listed modules and any recorded in an index.
	nameSet := make(map[string]bool)
	for name := range cc.Modules {
		nameSet[name] = true
	}
	indices := make(map[string]*module.CompatIndex)
	for _, ep := range epochs {
		idx, lerr := module.LoadCompatIndex(catalogDir, ep)
		if lerr != nil {
			return lerr
		}
		indices[ep] = idx
		if idx != nil {
			for name := range idx.Modules {
				nameSet[name] = true
			}
		}
	}
	names := make([]string, 0, len(nameSet))
	for name := range nameSet {
		names = append(names, name)
	}
	sort.Strings(names)

	var b strings.Builder
	b.WriteString("# Community module compatibility matrix\n\n")
	b.WriteString("Generated by `promise package build-index`. ✓ = verified for that epoch; — = no verified version.\n\n")

	b.WriteString("| module |")
	for _, ep := range epochs {
		fmt.Fprintf(&b, " %s |", ep)
	}
	b.WriteString("\n|---|")
	for range epochs {
		b.WriteString("---|")
	}
	b.WriteString("\n")

	for _, name := range names {
		fmt.Fprintf(&b, "| %s |", name)
		for _, ep := range epochs {
			mark := "—"
			if idx := indices[ep]; idx != nil {
				if _, ok := idx.Verified(name); ok {
					mark = "✓"
				}
			}
			fmt.Fprintf(&b, " %s |", mark)
		}
		b.WriteString("\n")
	}

	return os.WriteFile(filepath.Join(catalogDir, "matrix.md"), []byte(b.String()), 0644)
}
