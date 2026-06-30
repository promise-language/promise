package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	"github.com/promise-language/promise/compiler/internal/module"
)

// shortCommit truncates a commit SHA to 12 chars for display.
func shortCommit(c string) string {
	if len(c) > 12 {
		return c[:12]
	}
	return c
}

// lastLines returns the last n non-empty lines of s, joined — used to surface a
// dependency's verification failure compactly without dumping a full build log.
func lastLines(s string, n int) string {
	var lines []string
	for _, l := range strings.Split(s, "\n") {
		if strings.TrimSpace(l) != "" {
			lines = append(lines, l)
		}
	}
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

// moduleHasTests reports whether modDir carries any `_test.pr` files — the
// empirical compatibility gate (§9.9) is run via `promise test`, which discovers
// " `test " functions in `_test.pr` files.
func moduleHasTests(modDir string) (bool, error) {
	files, err := module.CollectModuleSources(modDir, true)
	if err != nil {
		return false, err
	}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.pr") {
			return true, nil
		}
	}
	return false, nil
}

// verifyModuleCompat establishes whether the module at (url, commit) is compatible
// with `epoch` per §9.9: built with the epoch's compiler it compiles and 100% of
// its " `test " functions pass (a parse/type error is a compile failure →
// incompatible). compilerBin is the epoch-E compiler used to run the tests.
//
// Modules with no `*_test.pr` files are verified by compilation only (§9.9 ad-hoc
// tier policy): `promise emit-ir` is run on the source; if it exits 0 the module
// is marked compatible (compile-only) and warn is called with an advisory message.
// A module whose source fails to compile is still incompatible (§9.10).
//
// The verdict is cached locally (§9.9, ad-hoc tier) keyed by url@commit#epoch and
// invalidated on a compiler-build change, so repeat adds and the diamond-dedup
// common case do not re-run tests. A module's pinned transitive deps are verified
// against the SAME project epoch first (§9.8/§9.10 apply transitively), so a
// failing dep surfaces as a clean gate rather than a raw compiler error buried in
// the dependency's source.
func verifyModuleCompat(compilerBin, url, commit, epoch string, visiting map[string]bool, warn func(string)) (ok bool, reason string, err error) {
	if v, found := module.LookupCompat(url, commit, epoch); found {
		return v.Compatible, v.FailReason, nil
	}

	// Break dependency cycles: a module currently being verified higher in the
	// recursion is treated as satisfied — its own verdict is decided by its own
	// (eventual) test run, not re-entered here.
	key := module.NormalizeURL(url) + "@" + commit + "#" + epoch
	if visiting[key] {
		return true, "", nil
	}
	visiting[key] = true
	defer delete(visiting, key)

	modDir, err := module.ResolveRemoteModule(url, commit)
	if err != nil {
		return false, "", fmt.Errorf("fetching %s@%s: %w", url, shortCommit(commit), err)
	}

	// Pre-verify pinned transitive deps against the project's epoch (§9.8). The
	// build uses these pins (§9.5); a dep with no compatible version makes this
	// module incompatible too (§9.10 applies transitively).
	cfg, perr := module.ParseConfig(filepath.Join(modDir, "promise.toml"))
	if perr != nil {
		reason = fmt.Sprintf("invalid promise.toml: %v", perr)
		_ = module.SaveCompat(&module.CompatVerdict{URL: url, Commit: commit, Epoch: epoch, Compatible: false, FailReason: reason})
		return false, reason, nil
	}
	depsOK, depReason, derr := verifyDeps(compilerBin, cfg, epoch, visiting, warn)
	if derr != nil {
		return false, "", derr
	}
	if !depsOK {
		_ = module.SaveCompat(&module.CompatVerdict{URL: url, Commit: commit, Epoch: epoch, Compatible: false, FailReason: depReason})
		return false, depReason, nil
	}

	hasTests, herr := moduleHasTests(modDir)
	if herr != nil {
		return false, "", herr
	}
	if !hasTests {
		return verifyModuleCompatCompileOnly(compilerBin, url, commit, epoch, modDir, warn)
	}

	cmd := exec.Command(compilerBin, "test", modDir)
	cmd.Env = append(os.Environ(), "PROMISE_NO_EPOCH_WARN=1")
	out, runErr := cmd.CombinedOutput()
	compatible := runErr == nil
	if !compatible {
		reason = lastLines(string(out), 20)
	}
	_ = module.SaveCompat(&module.CompatVerdict{URL: url, Commit: commit, Epoch: epoch, Compatible: compatible, FailReason: reason})
	return compatible, reason, nil
}

// verifyModuleCompatCompileOnly handles the no-test path: runs `promise emit-ir`
// on the module source to verify it compiles under the epoch. On success, records
// a compile-only verdict and calls warn. On compile failure, records incompatible.
// An empty module (no .pr files) is accepted vacuously with a warning.
func verifyModuleCompatCompileOnly(compilerBin, url, commit, epoch, modDir string, warn func(string)) (ok bool, reason string, err error) {
	srcFiles, serr := module.CollectModuleSources(modDir, false)
	if serr != nil {
		return false, "", serr
	}
	if len(srcFiles) == 0 {
		// No .pr source files at all — accept vacuously (emit-ir would error on empty project).
		warnMsg := fmt.Sprintf("module %q has no .pr files — treating as compatible (§9.9 vacuous pass)", url)
		warn(warnMsg)
		_ = module.SaveCompat(&module.CompatVerdict{URL: url, Commit: commit, Epoch: epoch, Compatible: true, CompileOnly: true, FailReason: warnMsg})
		return true, "", nil
	}
	// Compile-only check: emit-ir accepts a directory, compiles all non-test .pr
	// sources via discoverProject, does not require main(), exits 0/non-0.
	emitCmd := exec.Command(compilerBin, "emit-ir", modDir)
	emitCmd.Env = append(os.Environ(), "PROMISE_NO_EPOCH_WARN=1")
	out, runErr := emitCmd.CombinedOutput()
	if runErr != nil {
		reason = lastLines(string(out), 20)
		_ = module.SaveCompat(&module.CompatVerdict{URL: url, Commit: commit, Epoch: epoch, Compatible: false, FailReason: reason})
		return false, reason, nil
	}
	warnMsg := fmt.Sprintf("module %q has no `*_test.pr` files — verified by compilation only; add tests for full empirical compatibility (§9.9)", url)
	warn(warnMsg)
	_ = module.SaveCompat(&module.CompatVerdict{URL: url, Commit: commit, Epoch: epoch, Compatible: true, CompileOnly: true, FailReason: warnMsg})
	return true, "", nil
}

// verifyDeps verifies every pinned transitive git dependency in cfg against
// epoch (§9.8/§9.10 apply transitively). It returns (false, reason) for the first
// incompatible dependency; non-git (sha256) and incomplete named entries are
// skipped. Shared by verifyModuleCompat (remote modules) and
// verifyLocalModuleCompat (the cwd module on `package check-epoch`) so the
// transitive-compat rule lives in exactly one place.
func verifyDeps(compilerBin string, cfg *module.Config, epoch string, visiting map[string]bool, warn func(string)) (ok bool, reason string, err error) {
	for depURL, depCommit := range cfg.Require {
		if depCommit == "" {
			continue // non-git source (sha256) — not epoch-resolved here
		}
		depOK, depReason, derr := verifyModuleCompat(compilerBin, depURL, depCommit, epoch, visiting, warn)
		if derr != nil {
			return false, "", derr
		}
		if !depOK {
			return false, fmt.Sprintf("transitive dependency %s@%s is not compatible with epoch %s: %s", depURL, shortCommit(depCommit), epoch, depReason), nil
		}
	}
	for _, entry := range cfg.NamedRequire {
		if entry.URL == "" || entry.Commit == "" {
			continue
		}
		depOK, depReason, derr := verifyModuleCompat(compilerBin, entry.URL, entry.Commit, epoch, visiting, warn)
		if derr != nil {
			return false, "", derr
		}
		if !depOK {
			return false, fmt.Sprintf("transitive dependency %s@%s is not compatible with epoch %s: %s", entry.URL, shortCommit(entry.Commit), epoch, depReason), nil
		}
	}
	return true, "", nil
}

// verifyLocalModuleCompat establishes whether the module in the local directory
// modDir is compatible with epoch (§9.9), the same gate as verifyModuleCompat but
// for an on-disk module (the owner's cwd) rather than a fetched remote: its pinned
// transitive deps are verified first, then `compilerBin test modDir` must pass
// 100% of the module's `test` functions. Modules with no test files are verified by
// compilation only (§9.9 ad-hoc tier policy). Used by `promise package check-epoch`.
func verifyLocalModuleCompat(compilerBin, modDir, epoch string) (ok bool, reason string, err error) {
	cfg, perr := module.ParseConfig(filepath.Join(modDir, "promise.toml"))
	if perr != nil {
		return false, fmt.Sprintf("invalid promise.toml: %v", perr), nil
	}
	warn := func(msg string) { fmt.Fprintln(os.Stderr, "warning:", msg) }
	depsOK, depReason, derr := verifyDeps(compilerBin, cfg, epoch, map[string]bool{}, warn)
	if derr != nil {
		return false, "", derr
	}
	if !depsOK {
		return false, depReason, nil
	}

	hasTests, herr := moduleHasTests(modDir)
	if herr != nil {
		return false, "", herr
	}
	if !hasTests {
		srcFiles, serr := module.CollectModuleSources(modDir, false)
		if serr != nil {
			return false, "", serr
		}
		if len(srcFiles) == 0 {
			warn(fmt.Sprintf("module %q has no .pr files — treating as compatible (§9.9 vacuous pass)", cfg.Name))
			return true, "", nil
		}
		emitCmd := exec.Command(compilerBin, "emit-ir", modDir)
		emitCmd.Env = append(os.Environ(), "PROMISE_NO_EPOCH_WARN=1")
		out, runErr := emitCmd.CombinedOutput()
		if runErr != nil {
			return false, lastLines(string(out), 20), nil
		}
		warn(fmt.Sprintf("module %q has no `*_test.pr` files — verified by compilation only; add tests for full empirical compatibility (§9.9)", cfg.Name))
		return true, "", nil
	}

	cmd := exec.Command(compilerBin, "test", modDir)
	cmd.Env = append(os.Environ(), "PROMISE_NO_EPOCH_WARN=1")
	out, runErr := cmd.CombinedOutput()
	if runErr != nil {
		return false, lastLines(string(out), 20), nil
	}
	return true, "", nil
}

// fetchCommunityCatalog refreshes + parses the community catalog's name→URL map
// (§9.9). Returns the checkout directory (root of index/<epoch>.json) and the
// parsed modules.toml. A catalog with no modules.toml yet yields an empty catalog
// (not an error) so resolution cleanly falls through to the ad-hoc path.
func fetchCommunityCatalog() (dir string, cc *module.CommunityCatalog, err error) {
	dir, err = module.FetchCommunityCatalog(true)
	if err != nil {
		return "", nil, fmt.Errorf("fetching community catalog: %w", err)
	}
	data, rerr := os.ReadFile(filepath.Join(dir, "modules.toml"))
	if rerr != nil {
		if os.IsNotExist(rerr) {
			return dir, &module.CommunityCatalog{Modules: map[string]*module.CommunityEntry{}}, nil
		}
		return "", nil, rerr
	}
	cc, perr := module.ParseCommunityModules(data)
	if perr != nil {
		return "", nil, fmt.Errorf("invalid community catalog modules.toml: %w", perr)
	}
	return dir, cc, nil
}

// communityIndexPin returns the verified commit recorded for module name under
// epoch in the fetched community catalog at dir — the CI index IS the verdict for
// community modules (§9.9, no local test run). When the module has no verified
// entry for the epoch it returns a *module.NoCompatibleVersionError (§9.10),
// populated with the highest epoch the module IS recorded for.
func communityIndexPin(dir, name, epoch string) (commit string, err error) {
	idx, ierr := module.LoadCompatIndex(dir, epoch)
	if ierr != nil {
		return "", ierr
	}
	if ie, ok := idx.Verified(name); ok {
		return ie.Commit, nil
	}
	hi, hiTag := module.HighestIndexedEpoch(dir, name)
	return "", &module.NoCompatibleVersionError{Module: name, Epoch: epoch, HighestVerifiedEpoch: hi, HighestTag: hiTag}
}

// resolveCommunity resolves a bare module NAME through the community catalog
// (§9.9 step 4). found is false when name is not listed (caller falls through to
// the ad-hoc URL path); when listed, it returns the entry's URL and the
// index-verified commit, or a *module.NoCompatibleVersionError (§9.10).
func resolveCommunity(name, epoch string) (url, commit string, found bool, err error) {
	dir, cc, ferr := fetchCommunityCatalog()
	if ferr != nil {
		return "", "", false, ferr
	}
	entry := cc.Lookup(name)
	if entry == nil {
		return "", "", false, nil
	}
	c, cerr := communityIndexPin(dir, name, epoch)
	if cerr != nil {
		return entry.URL, "", true, cerr
	}
	return entry.URL, c, true, nil
}

// resolveCommunityByURL re-resolves an existing community [require] pin (keyed by
// URL) through the fresh index on `pkg update`. found is false when the URL is no
// longer listed in the community catalog (caller falls back to the generic
// epoch-tag engine path).
func resolveCommunityByURL(url, epoch string) (commit string, found bool, err error) {
	dir, cc, ferr := fetchCommunityCatalog()
	if ferr != nil {
		return "", false, ferr
	}
	entry := cc.LookupByURL(url)
	if entry == nil {
		return "", false, nil
	}
	c, cerr := communityIndexPin(dir, entry.Name, epoch)
	if cerr != nil {
		return "", true, cerr
	}
	return c, true, nil
}

// resolveEpochAware picks an epoch-appropriate commit for a module and verifies it
// under the project's epoch before returning it (§9.8/§9.9). With an explicit ref
// the user's choice is resolved and verified with no walk-back; without one, the
// largest `epoch-X ≤ E` tag is tried, stepping back through older tags on
// verification failure, with a `stable`/HEAD fallback only when there are no
// `epoch-*` tags at all. A module that carries `epoch-*` tags but whose tags are
// all newer than the project epoch is versioned (just not for this epoch) — it
// hits the §9.10 gate (OnlyNewerEpochs), not the unversioned `stable`/HEAD
// fallback. When nothing verifies it returns a *module.NoCompatibleVersionError
// (§9.10) — raised here, at resolve time, so raw dependency compiler errors never
// reach the project build.
//
// warn receives human-facing notices (e.g. the "unversioned" fallback warning).
func resolveEpochAware(compilerBin, projectEpoch, label, url, explicitRef string, warn func(string)) (commit string, err error) {
	visiting := map[string]bool{}

	if explicitRef != "" {
		c, perr := module.PinResolve(url, explicitRef)
		if perr != nil {
			return "", perr
		}
		ok, reason, verr := verifyModuleCompat(compilerBin, url, c, projectEpoch, visiting, warn)
		if verr != nil {
			return "", verr
		}
		if !ok {
			return "", fmt.Errorf("module %q at %s (%s) is not compatible with epoch %s:\n%s",
				label, explicitRef, shortCommit(c), projectEpoch, indent(reason, "  "))
		}
		if reason != "" {
			warn(reason) // compile-only advisory surfaced from cache hit
		}
		return c, nil
	}

	epochTags, stableCommit, terr := module.ListRepoTags(url)
	if terr != nil {
		return "", terr
	}
	candidates := module.Candidates(epochTags, projectEpoch)

	if len(candidates) == 0 && len(epochTags) > 0 {
		// The module IS versioned (carries epoch-* tags) but every tag targets a
		// newer epoch than the project's — it does not support this epoch. This is
		// the §9.10 gate, not the §9.8 unversioned fallback: pinning HEAD here would
		// mislabel a versioned module as "unversioned".
		lo, loTag := module.LowestEpoch(epochTags)
		return "", &module.NoCompatibleVersionError{
			Module: label, Epoch: projectEpoch,
			OnlyNewerEpochs: true, LowestSupportedEpoch: lo, LowestTag: loTag,
		}
	}

	if len(candidates) == 0 {
		// Truly unversioned — no epoch-* tags at all (§9.8 step 1 fallback): stable
		// tag, else HEAD with an "unversioned" warning.
		var fallback string
		if stableCommit != "" {
			fallback = stableCommit
			warn(fmt.Sprintf("module %q has no epoch-* tags ≤ %s; using its 'stable' tag", label, projectEpoch))
		} else {
			h, herr := module.PinResolve(url, "HEAD")
			if herr != nil {
				return "", herr
			}
			fallback = h
			warn(fmt.Sprintf("module %q is unversioned (no epoch-* or 'stable' tags); pinning default-branch HEAD %s", label, shortCommit(h)))
		}
		ok, reason, verr := verifyModuleCompat(compilerBin, url, fallback, projectEpoch, visiting, warn)
		if verr != nil {
			return "", verr
		}
		if ok {
			if reason != "" {
				warn(reason) // compile-only advisory surfaced from cache hit
			}
			return fallback, nil
		}
		// Reached only when there are no epoch-* tags (the only-newer case returned
		// above), so HighestVerifiedEpoch is empty by construction.
		return "", &module.NoCompatibleVersionError{Module: label, Epoch: projectEpoch}
	}

	// Walk back through candidate tags (largest epoch first); first verified wins.
	for _, cand := range candidates {
		ok, reason, verr := verifyModuleCompat(compilerBin, url, cand.Commit, projectEpoch, visiting, warn)
		if verr != nil {
			return "", verr
		}
		if ok {
			if reason != "" {
				warn(reason) // compile-only advisory surfaced from cache hit
			}
			return cand.Commit, nil
		}
		warn(fmt.Sprintf("module %q tag %s fails under epoch %s; stepping back", label, cand.Tag, projectEpoch))
	}

	hi, hiTag := module.HighestEpoch(epochTags)
	return "", &module.NoCompatibleVersionError{Module: label, Epoch: projectEpoch, HighestVerifiedEpoch: hi, HighestTag: hiTag}
}

// testVerifyCompilerBin, when non-empty, overrides the compiler binary used to
// verify module compatibility. It is set only by in-process tests, where
// os.Executable() is the test binary rather than a real Promise compiler — the
// epoch assertion below still runs, only the returned path is swapped.
var testVerifyCompilerBin string

// projectEpochCompiler returns the compiler binary to use for verifying modules on
// add/update. Verification must run under the project's epoch, so this asserts the
// running compiler's epoch matches the project's and returns its own path;
// otherwise it errors, directing the user to `promise use <epoch>`.
func projectEpochCompiler(projectEpoch string) (string, error) {
	myEpoch, err := module.CompilerEpoch(embeddedCatalog)
	if err != nil {
		return "", fmt.Errorf("cannot determine this compiler's epoch: %w", err)
	}
	if myEpoch != projectEpoch {
		return "", fmt.Errorf("this compiler is epoch %s, but the project pins epoch %s — verification must run under %s.\n  run 'promise use %s' (or the 'promise' launcher) and retry",
			myEpoch, projectEpoch, projectEpoch, projectEpoch)
	}
	if testVerifyCompilerBin != "" {
		return testVerifyCompilerBin, nil
	}
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	return exe, nil
}

// epochCompilerBin returns the path to the epoch-E compiler binary, installing it
// on demand if absent. Used by `package check-upgrade <E'>` to verify dependencies
// against a target epoch other than the one this binary implements. When E equals
// the running compiler's own epoch, the running binary is used directly — no need
// to locate or download a separate install.
func epochCompilerBin(epoch string) (string, error) {
	// In-process tests run as the test binary, not a real Promise compiler, so
	// they redirect to a freshly built `promise` via this hook (same override the
	// remote-module verifier uses).
	if testVerifyCompilerBin != "" {
		return testVerifyCompilerBin, nil
	}
	if myEpoch, err := module.CompilerEpoch(embeddedCatalog); err == nil && myEpoch == epoch {
		if exe, err := os.Executable(); err == nil {
			return exe, nil
		}
	}
	dir, err := module.EpochDir(epoch)
	if err != nil {
		return "", err
	}
	binPath := filepath.Join(dir, "bin", "promise")
	if runtime.GOOS == "windows" {
		binPath += ".exe"
	}
	if _, err := os.Stat(binPath); err == nil {
		return binPath, nil
	}
	if err := ensureEpochPresent(epoch); err != nil {
		return "", fmt.Errorf("epoch %s is not installed: %w\n  install it with 'promise install %s'", epoch, err, epoch)
	}
	if _, err := os.Stat(binPath); err != nil {
		return "", fmt.Errorf("epoch %s compiler not found at %s after install", epoch, binPath)
	}
	return binPath, nil
}

// runPackageCheckUpgrade implements `promise package check-upgrade <E'>`: resolve
// every dependency against target epoch E′ and report — before any change — which
// deps have a verified E′-compatible version and which would hit the §9.10 gate
// (§9.10). Exits non-zero if any dependency would be blocked.
func runPackageCheckUpgrade(args []string) {
	if len(args) != 1 {
		fmt.Fprintln(os.Stderr, "usage: promise package check-upgrade <epoch>")
		fmt.Fprintln(os.Stderr, "  Reports which dependencies have a verified version compatible with <epoch>.")
		os.Exit(1)
	}
	targetEpoch := args[0]
	if targetEpoch == module.ChannelNext {
		fmt.Fprintln(os.Stderr, "error: 'next' is a toolchain channel, not a project epoch — pass a numeric epoch (e.g. 2026.2)")
		os.Exit(1)
	}
	if _, _, ok := module.ParseEpoch(targetEpoch); !ok {
		fmt.Fprintf(os.Stderr, "error: %q is not a numeric epoch (expected YYYY.N)\n", targetEpoch)
		os.Exit(1)
	}

	dir, err := os.Getwd()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	cfg, err := module.FindConfig(dir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	if cfg == nil {
		fmt.Fprintln(os.Stderr, "error: no promise.toml found (run 'promise init' first)")
		os.Exit(1)
	}

	// Build the dependency list (URL-keyed + named git entries).
	type dep struct{ label, url string }
	var deps []dep
	for url := range cfg.Require {
		deps = append(deps, dep{url, url})
	}
	for name, entry := range cfg.NamedRequire {
		if entry.URL != "" && entry.Commit != "" {
			deps = append(deps, dep{name, entry.URL})
		}
	}
	if len(deps) == 0 {
		fmt.Println("No [require] dependencies to check.")
		return
	}
	sort.Slice(deps, func(i, j int) bool { return deps[i].label < deps[j].label })

	compilerBin, err := epochCompilerBin(targetEpoch)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("Checking %d dependencies against epoch %s:\n\n", len(deps), targetEpoch)
	blocked := 0
	for _, d := range deps {
		commit, rerr := resolveEpochAware(compilerBin, targetEpoch, d.label, d.url, "", func(string) {})
		if rerr != nil {
			blocked++
			if nce, ok := rerr.(*module.NoCompatibleVersionError); ok {
				fmt.Printf("  ✗ %s — no compatible version\n", d.label)
				if nce.OnlyNewerEpochs {
					fmt.Printf("      module only targets newer epochs (oldest: %s, tag %s)\n", nce.LowestSupportedEpoch, nce.LowestTag)
				} else if nce.HighestVerifiedEpoch != "" {
					fmt.Printf("      highest verified epoch: %s (%s)\n", nce.HighestVerifiedEpoch, nce.HighestTag)
				}
			} else {
				fmt.Printf("  ✗ %s — %v\n", d.label, rerr)
			}
			continue
		}
		fmt.Printf("  ✓ %s → %s\n", d.label, shortCommit(commit))
	}

	fmt.Println()
	if blocked > 0 {
		fmt.Printf("%d of %d dependencies have no version compatible with epoch %s.\n", blocked, len(deps), targetEpoch)
		fmt.Printf("Upgrading to %s would hit the §9.10 gate for those deps — resolve them before changing [module] epoch.\n", targetEpoch)
		os.Exit(1)
	}
	fmt.Printf("All %d dependencies have a verified version compatible with epoch %s.\n", len(deps), targetEpoch)
	fmt.Printf("To upgrade: set [module] epoch = %q, then run 'promise package update'.\n", targetEpoch)
}
