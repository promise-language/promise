package common

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// gate_install.go implements `bin/gate install` — the end-to-end install gate
// (T0803). It validates the REAL user install path: fetch the published install
// script, run it (download binary → verify checksum → decompress → `promise
// install`), sanity-check the install, then run the full test suite through the
// freshly INSTALLED distribution (not bin/promise).
//
// All gate logic lives here in cross-platform Go — there are deliberately NO
// per-platform gate scripts (that mirrored-script duplication is exactly what
// bin/gate exists to avoid). The ONLY OS-specific step is invoking the
// already-published installer the gate is testing: `sh install.sh` on
// linux/darwin vs `powershell install.ps1` on windows. Everything else (sandbox
// setup, fetch, sanity checks, running the suite through the installed stub,
// artifact aggregation) is identical across platforms.
//
// The gate deliberately exercises the installed STUB at $PROMISE_HOME/bin/promise
// — for `version`, the `exec` smoke test, AND the full suite — because the stub's
// hand-off to the real epoch compiler is on the critical path yet is not covered
// by bin/test / bin/verify (which run the compiler directly). That hand-off, and
// its Windows path in particular, is a primary reason this gate exists.
//
// Source provenance (T0854): the install gate validates the published
// distribution against the sources it was BUILT FROM — NOT the developer's local
// working tree. It resolves the binary's build commit via `promise version --json`
// (T1125), checks that SHA out into a detached worktree, and runs the suite
// there. This prevents spurious failures when bugfix-plus-regression-test commits
// land after the last prebuilt publish (the stale binary would otherwise fail the
// newer tests). Local compiler/source edits stay covered by bin/verify / bin/test.

// installGitHubRepo is the only place Promise is published — the repo is public,
// so the gate fetches the install script + release assets straight from GitHub
// releases. The promise-lang.org prebuilts bucket is gone (T0804); GitHub is the
// single source of truth.
const installGitHubRepo = "promise-language/promise"

// defaultGateChannel is the release channel the install gate validates when
// --channel is omitted: the moving `epoch-next` pre-release, which is what
// `bin/release cut next` publishes for validation before a stable cut.
const defaultGateChannel = "next"

// validateGateChannel accepts the three --channel forms: "next" (the moving
// pre-release), "stable" (the latest published stable epoch), or an explicit
// "Y.N" epoch (e.g. 2026.1).
func validateGateChannel(channel string) error {
	if channel == "next" || channel == "stable" {
		return nil
	}
	if _, err := parseEpochStr(channel); err != nil {
		return fmt.Errorf("--channel must be next, stable, or an epoch like 2026.1 (got %q)", channel)
	}
	return nil
}

// gateInstallSource maps a validated --channel to the GitHub release the gate
// installs from: the base URL the install script + assets are fetched from, and
// the --epoch argument handed to the installer so it pulls the binary + checksums
// from the SAME release.
//
//	next   → epoch-next pre-release      (installer --epoch next)
//	stable → latest published stable     (installer --epoch latest)
//	<Y.N>  → that specific epoch release  (installer --epoch <Y.N>)
func gateInstallSource(channel string) (scriptBaseURL, installerEpoch string) {
	const repoReleases = "https://github.com/" + installGitHubRepo + "/releases"
	switch channel {
	case "stable":
		// `releases/latest/download/...` is GitHub's redirect to the newest NON-
		// pre-release; `--epoch latest` makes the installer resolve the same tag.
		return repoReleases + "/latest/download", "latest"
	default: // "next" or an explicit "Y.N" → the epoch-<channel> release
		return repoReleases + "/download/epoch-" + channel, channel
	}
}

// installPhasesFor lists the gate phases recorded in phases.json for a variant,
// in execution order. Each maps to an `install_<variant>_<phase>_ok` ∈ {0,1}
// metric in the envelope. The full variant adds an "offline" phase: a
// self-contained compile+run with the network blackholed, proving the host LLVM
// toolchain blobs are pre-staged. The thin variant has no such guarantee (it
// fetches blobs on first compile), so it omits the phase.
func installPhasesFor(variant string) []string {
	phases := []string{"fetch", "install", "sanity", "test"}
	if variant == "full" {
		phases = append(phases, "offline")
	}
	return phases
}

// runGateInstall runs the end-to-end install gate for one variant (thin|full)
// and writes the structured JSON gate envelope to stdout. Phase progress goes to
// stderr so stdout stays clean JSON.
func runGateInstall(root string, args []string) error {
	const usage = "usage: bin/gate install --variant {thin|full} [--channel {next|stable|<epoch>}] [--system]"
	variant := ""
	channel := defaultGateChannel
	system := false
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "--variant" || arg == "-variant":
			if i+1 >= len(args) {
				return fmt.Errorf("%s", usage)
			}
			i++
			variant = args[i]
		case arg == "--variant=thin" || arg == "-variant=thin":
			variant = "thin"
		case arg == "--variant=full" || arg == "-variant=full":
			variant = "full"
		case arg == "--channel" || arg == "-channel":
			if i+1 >= len(args) {
				return fmt.Errorf("%s", usage)
			}
			i++
			channel = args[i]
		case strings.HasPrefix(arg, "--channel=") || strings.HasPrefix(arg, "-channel="):
			channel = arg[strings.IndexByte(arg, '=')+1:]
		case arg == "--system" || arg == "-system":
			system = true
		default:
			return fmt.Errorf("%s", usage)
		}
	}
	if variant != "thin" && variant != "full" {
		return fmt.Errorf("bin/gate install: --variant must be thin or full")
	}
	if err := validateGateChannel(channel); err != nil {
		return fmt.Errorf("bin/gate install: %w", err)
	}

	hostTarget := strings.ToLower(runtime.GOOS) + "-" + runtime.GOARCH

	// Scratch dir for the phase artifacts (phases.json, tests.jsonl) and — in
	// clean-slate mode — the sandbox HOME. Removed on exit.
	work, err := os.MkdirTemp("", "gate-install-")
	if err != nil {
		return fmt.Errorf("mktemp: %w", err)
	}
	defer os.RemoveAll(work)

	fmt.Fprintf(os.Stderr, "Running install gate (variant=%s, system=%v, channel=%s)...\n", variant, system, channel)
	phaseErr := runInstallPhases(root, work, variant, channel, system)

	// Aggregate the phase artifacts into the standard envelope.
	out, err := buildInstallGateOutput(hostTarget, variant, work)
	if err != nil {
		return fmt.Errorf("aggregate gate output: %w", err)
	}

	// Sidecar so commit/periodic gate ingestion can read the metrics.
	gv := &GateValues{
		Timestamp: time.Now().UTC().Format(time.RFC3339),
		Platform:  hostTarget,
		Values:    out.Metrics,
	}
	if err := WriteGateValues(root, gv); err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not write gate values: %v\n", err)
	}

	data, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal gate output: %w", err)
	}
	fmt.Println(string(data))

	if phaseErr != nil {
		return fmt.Errorf("install gate (%s) failed: %w", variant, phaseErr)
	}
	return nil
}

// runInstallPhases runs the four gate phases (fetch → install → sanity → test)
// for one variant, writing phases.json and tests.jsonl into work for the
// aggregator. It always writes phases.json (via defer) — even on an early phase
// failure — so a fetch/install/sanity failure still reports the later phases as
// not-ok rather than leaving the envelope empty. Returns the first phase error,
// or nil if every phase passed.
func runInstallPhases(root, work, variant, channel string, system bool) error {
	phases := map[string]string{}
	for _, p := range installPhasesFor(variant) {
		phases[p] = "fail"
	}
	logf := func(format string, a ...any) {
		fmt.Fprintf(os.Stderr, "[gate-install:%s] %s\n", variant, fmt.Sprintf(format, a...))
	}
	defer func() {
		data, _ := json.Marshal(phases) // map[string]string marshal never fails
		if err := os.WriteFile(filepath.Join(work, "phases.json"), data, 0o644); err != nil {
			logf("warning: write phases.json: %v", err)
		}
	}()

	// ── env: clean-slate sandbox by default; --system uses the real environment.
	// The only OS-specific env detail is the home var (USERPROFILE on Windows).
	homeKey := "HOME"
	promiseLeaf := "promise"
	installScript := "install.sh"
	if runtime.GOOS == "windows" {
		homeKey = "USERPROFILE"
		promiseLeaf = "promise.exe"
		installScript = "install.ps1"
	}

	overrides := map[string]string{}
	var promiseHome string
	if !system {
		// Clean-slate isolation overrides PROMISE_HOME only - NOT $HOME. The goal
		// is to never touch the developer's real ~/.promise, which is governed
		// entirely by PROMISE_HOME; overriding $HOME as well would break tests
		// that legitimately assert os.home_dir == $HOME (os.home_dir reads the
		// passwd DB, which ignores an overridden $HOME), so $HOME stays real and
		// PROMISE_HOME points into the scratch dir.
		promiseHome = filepath.Join(work, ".promise")
		overrides["PROMISE_HOME"] = promiseHome
		// Never modify the developer's real User PATH from a sandboxed gate run. On
		// Windows `promise install` adds <PROMISE_HOME>\bin to the User PATH via the
		// registry, which ignores PROMISE_HOME isolation and would leak the scratch
		// dir into the real PATH every run (T0864). PROMISE_NO_MODIFY_PATH opts out;
		// the sanity check below runs the installed stub by absolute path anyway.
		overrides["PROMISE_NO_MODIFY_PATH"] = "1"
		// Scrub PATH to a minimal toolchain set on POSIX so the gate doesn't lean
		// on the dev environment. On Windows PowerShell needs the inherited PATH,
		// so it's left intact there.
		if runtime.GOOS != "windows" {
			overrides["PATH"] = "/usr/bin:/bin:/usr/sbin:/sbin"
		}
		logf("clean-slate sandbox: PROMISE_HOME=%s (real %s preserved)", promiseHome, homeKey)
	} else {
		promiseHome = os.Getenv("PROMISE_HOME")
		if promiseHome == "" {
			promiseHome = filepath.Join(os.Getenv(homeKey), ".promise")
		}
		overrides["PROMISE_HOME"] = promiseHome
		logf("system mode: PROMISE_HOME=%s", promiseHome)
	}
	baseEnv := envWith(os.Environ(), overrides)
	promiseBin := filepath.Join(promiseHome, "bin", promiseLeaf)

	// ── phase: fetch the published install script ────────────────────────────
	// The script + assets are GitHub release assets (the repo is public;
	// promise-lang.org is gone — GitHub is the only source). The --channel
	// selects which release: next / stable / an explicit epoch.
	scriptBase, installerEpoch := gateInstallSource(channel)
	scriptPath := filepath.Join(work, installScript)
	logf("fetching install script from %s/%s", scriptBase, installScript)
	if err := downloadFile(scriptBase+"/"+installScript, scriptPath); err != nil {
		logf("fetch failed: %v", err)
		return fmt.Errorf("fetch: %w", err)
	}
	phases["fetch"] = "pass"

	// ── phase: run the installer (the one OS-specific invocation) ─────────────
	// The installer's --epoch points it at the SAME release the script came from
	// (the binary .gz + SHA256SUMS live there too): next/<Y.N> use that epoch tag,
	// stable resolves the latest published epoch.
	logf("running installer (channel=%s epoch=%s variant=%s)", channel, installerEpoch, variant)
	var installCmd *exec.Cmd
	if runtime.GOOS == "windows" {
		a := []string{"-ExecutionPolicy", "Bypass", "-File", scriptPath, "-Epoch", installerEpoch}
		if variant == "full" {
			a = append(a, "-Full")
		}
		installCmd = exec.Command("powershell", a...)
	} else {
		a := []string{scriptPath, "--epoch", installerEpoch}
		if variant == "full" {
			a = append(a, "--full")
		}
		installCmd = exec.Command("sh", a...)
	}
	installCmd.Env = baseEnv
	installCmd.Stdout = os.Stderr
	installCmd.Stderr = os.Stderr
	if err := installCmd.Run(); err != nil {
		logf("install failed: %v", err)
		return fmt.Errorf("install: %w", err)
	}
	phases["install"] = "pass"

	// ── phase: sanity-check the install (always via the installed STUB) ───────
	if fi, err := os.Stat(promiseBin); err != nil || fi.IsDir() {
		logf("sanity: missing installed stub %s", promiseBin)
		return fmt.Errorf("sanity: missing installed stub %s", promiseBin)
	}
	verCmd := exec.Command(promiseBin, "version")
	verCmd.Env = baseEnv
	verCmd.Stdout = os.Stderr
	verCmd.Stderr = os.Stderr
	if err := verCmd.Run(); err != nil {
		logf("sanity: 'promise version' failed: %v", err)
		return fmt.Errorf("sanity: promise version: %w", err)
	}
	if matches, _ := filepath.Glob(filepath.Join(promiseHome, "epochs", "*", "bin", promiseLeaf)); len(matches) == 0 {
		logf("sanity: epoch compiler/stdlib not extracted under %s/epochs", promiseHome)
		return fmt.Errorf("sanity: epoch compiler not extracted")
	}
	smoke := exec.Command(promiseBin, "exec", `print_line("ok")`)
	smoke.Env = baseEnv
	smokeOut, smokeErr := smoke.Output()
	if got := strings.TrimSpace(string(smokeOut)); smokeErr != nil || got != "ok" {
		logf("sanity: exec smoke-test produced %q (want \"ok\"): %v", got, smokeErr)
		return fmt.Errorf("sanity: exec smoke-test failed")
	}
	phases["sanity"] = "pass"

	// ── pin sources to the published binary's build commit (T0854) ────────────
	// The gate validates the published distribution against ITS OWN sources, not
	// the dev's working tree. Resolve the SHA the binary was built from via
	// `promise version --json` (T1125), check it out into a detached worktree,
	// and run the suite there. This is the swap that makes the gate test the
	// published bytes — it MUST precede the shared testCmd block below (used by
	// thin/full and --system).
	jsonCmd := exec.Command(promiseBin, "version", "--json")
	jsonCmd.Env = baseEnv // resolve the SANDBOX epoch compiler (PROMISE_HOME lives in baseEnv)
	jsonOut, jsonErr := jsonCmd.Output()
	if jsonErr != nil {
		logf("warning: 'promise version --json' failed: %v", jsonErr)
	}
	sha := parseVersionCommit(jsonOut)
	// The `commit` field of `promise version --json` is the 40-char hex git SHA
	// the binary was built from (`-X main.commit=<GitSHAFull>`). An unstamped
	// build emits "" (omitempty); any non-SHA value is treated as "no provenance"
	// so the error is accurate rather than a confusing downstream cat-file failure.
	if !isFullGitSHA(sha) {
		logf("test: published binary has no provenance (no build commit recorded)")
		return fmt.Errorf("test: published binary has no provenance; re-publish a build that records its commit")
	}
	// Ensure the commit is present locally; fetch once if not.
	if err := exec.Command("git", "-C", root, "cat-file", "-e", sha+"^{commit}").Run(); err != nil {
		logf("commit %s not present locally; fetching...", sha)
		_ = exec.Command("git", "-C", root, "fetch", "--quiet").Run()
		if err := exec.Command("git", "-C", root, "cat-file", "-e", sha+"^{commit}").Run(); err != nil {
			return fmt.Errorf("test: published build commit %s not found locally even after fetch; run `git fetch` and retry", sha)
		}
	}
	srcDir := filepath.Join(work, "src")
	wt := exec.Command("git", "-C", root, "worktree", "add", "--detach", srcDir, sha)
	wt.Stdout, wt.Stderr = os.Stderr, os.Stderr
	if err := wt.Run(); err != nil {
		return fmt.Errorf("test: git worktree add %s @ %s: %w", srcDir, sha, err)
	}
	defer func() {
		rm := exec.Command("git", "-C", root, "worktree", "remove", "--force", srcDir)
		rm.Stdout, rm.Stderr = os.Stderr, os.Stderr
		_ = rm.Run()
	}()
	logf("checked out published sources @ %s into %s", sha, srcDir)

	// ── phase: run the full suite through the INSTALLED stub (always online) ───
	// The suite runs WITH network for both variants: some tests legitimately
	// fetch external catalog modules (e.g. wasi_preview_2 via git), which is a
	// user-program dependency, not a compiler one. The full variant's offline
	// guarantee (host LLVM toolchain pre-staged) is validated separately by the
	// "offline" phase below, on a self-contained program.
	logf("running full suite through installed stub (source=%s)", srcDir)
	// examples are the floor; tests/ + modules/ are the target. stdout = the
	// --json record stream (captured to tests.jsonl); stderr = human progress.
	testCmd := exec.Command(promiseBin, "test", "-timeout", "10", "--json", "examples/...", "tests/...", "modules/...")
	testCmd.Dir = srcDir
	testCmd.Env = baseEnv
	testCmd.Stderr = os.Stderr
	var buf bytes.Buffer
	testCmd.Stdout = &buf
	testErr := testCmd.Run()
	if werr := os.WriteFile(filepath.Join(work, "tests.jsonl"), buf.Bytes(), 0o644); werr != nil {
		logf("warning: write tests.jsonl: %v", werr)
	}
	// `promise test --json` is a data-emission mode: it reports each test's
	// outcome in the records and exits 0 regardless of failures. So the verdict
	// comes from the parsed records, NOT testErr. A non-zero testErr is still a
	// failure - it means the runner itself could not complete (e.g. the installed
	// stub or compiler crashed), which must never be masked.
	if testErr != nil {
		logf("test phase failed: test runner error: %v", testErr)
		return fmt.Errorf("test: runner error: %w", testErr)
	}
	if n := installTestFailures(buf.Bytes()); n > 0 {
		logf("test phase failed: %d non-passing test(s)", n)
		return fmt.Errorf("test: %d non-passing test(s)", n)
	}
	phases["test"] = "pass"

	// ── phase: offline smoke (full variant only) ──────────────────────────────
	// Prove the full binary's host toolchain is genuinely pre-staged: compile AND
	// run a SELF-CONTAINED program (no external module deps) with the network
	// blackholed. The only thing that could need the network is a missing LLVM
	// blob - which a correct full install has staged. Go's net/http honors these
	// proxy vars, so any blob/archive fetch fails fast.
	if variant == "full" {
		logf("offline smoke: compile+run a self-contained program with network blackholed")
		offlineEnv := envWith(baseEnv, map[string]string{
			"HTTPS_PROXY": "http://127.0.0.1:1",
			"HTTP_PROXY":  "http://127.0.0.1:1",
			"ALL_PROXY":   "http://127.0.0.1:1",
		})
		smoke := exec.Command(promiseBin, "exec", `print_line("offline ok")`)
		smoke.Env = offlineEnv
		smokeOut, smokeErr := smoke.Output()
		if got := strings.TrimSpace(string(smokeOut)); smokeErr != nil || got != "offline ok" {
			logf("offline smoke failed: produced %q (want \"offline ok\"): %v", got, smokeErr)
			return fmt.Errorf("offline: self-contained compile/run failed under network blackhole")
		}
		logf("offline smoke passed: full binary compiled+ran with no network")
		phases["offline"] = "pass"
	}

	logf("all phases passed")
	return nil
}

// installTestFailures counts non-passing test records in the gate's --json
// output. `promise test --json` exits 0 regardless of outcomes, so the test
// phase verdict must come from the records: any status other than "pass" or
// "excluded" (fail, leak, timeout, memory, not-run, or anything unexpected) is
// counted as a failure, matching BuildGateOutput's classification.
func installTestFailures(jsonl []byte) int {
	n := 0
	for _, r := range ParseTestJSONL(string(jsonl)) {
		switch r.Status {
		case "pass", "excluded":
			// not a failure
		default:
			n++
		}
	}
	return n
}

// parseVersionCommit extracts the "commit" field from `promise version --json`
// output (T1125). Returns the raw string; callers must validate with
// isFullGitSHA before treating it as provenance. Malformed or empty JSON yields
// "" — the same as an unstamped binary that omits the field entirely.
func parseVersionCommit(jsonData []byte) string {
	var v struct {
		Commit string `json:"commit"`
	}
	_ = json.Unmarshal(bytes.TrimSpace(jsonData), &v)
	return v.Commit
}

// isFullGitSHA reports whether s is a bare full-length (40-char) lowercase-hex
// git commit hash — the form expected in the `commit` field of
// `promise version --json`. Anything else (empty, missing field from an
// unstamped binary, stray value) is treated as "no provenance" by the install
// gate (T0854).
func isFullGitSHA(s string) bool {
	if len(s) != 40 {
		return false
	}
	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// envWith returns base with the given key=value pairs applied — any existing
// entry for an overridden key is dropped so the override wins regardless of
// platform exec semantics.
func envWith(base []string, kv map[string]string) []string {
	out := make([]string, 0, len(base)+len(kv))
	for _, e := range base {
		drop := false
		for k := range kv {
			if strings.HasPrefix(e, k+"=") {
				drop = true
				break
			}
		}
		if !drop {
			out = append(out, e)
		}
	}
	for k, v := range kv {
		out = append(out, k+"="+v)
	}
	return out
}

// downloadFile GETs url and writes the body to dest.
func downloadFile(url, dest string) error {
	resp, err := http.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	f, err := os.Create(dest)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := io.Copy(f, resp.Body); err != nil {
		return err
	}
	return nil
}

// buildInstallGateOutput aggregates the two phase artifacts in work
// (tests.jsonl + phases.json) into the standard GateOutput envelope. The per-test
// JSONL yields the install_<variant>_test_* metrics + per-file groups for free
// (reusing the exact gate machinery); the per-phase signals merge in as
// install_<variant>_<phase>_ok ∈ {0,1}. Pure (filesystem-read only) so the
// aggregation is unit-testable without running the gate.
func buildInstallGateOutput(hostTarget, variant, work string) (*GateOutput, error) {
	metricPrefix := "install_" + variant
	// Resolve symlinks so srcDir is in the same path namespace the test runner
	// reports (macOS /var→/private/var after chdir through a symlinked $TMPDIR).
	// work still exists here; work/src (the worktree) may already be removed.
	if resolved, err := filepath.EvalSymlinks(work); err == nil {
		work = resolved
	}
	// Relativize against srcDir (the worktree the suite ran in), not the dev
	// repo root — the worktree lives under a random temp dir, so root-relative
	// paths would escape root and produce unstable absolute identities (T0902).
	srcDir := filepath.Join(work, "src")
	data, readErr := os.ReadFile(filepath.Join(work, "tests.jsonl"))
	if readErr != nil && !os.IsNotExist(readErr) {
		fmt.Fprintf(os.Stderr, "warning: read tests.jsonl: %v\n", readErr)
	}
	out, err := BuildGateOutput(srcDir, hostTarget, metricPrefix, "install-"+variant, string(data))
	if err != nil {
		return nil, fmt.Errorf("buildInstallGateOutput: %w", err)
	}

	phases := readInstallPhases(filepath.Join(work, "phases.json"))
	for _, p := range installPhasesFor(variant) {
		ok := 0.0
		if phases[p] == "pass" {
			ok = 1
		}
		out.Metrics[metricPrefix+"_"+p+"_ok"] = ok
	}
	return out, nil
}

// readInstallPhases reads the phases.json ({"fetch":"pass|fail",...}). A missing
// file yields an empty map (expected when an early phase fails before the file is
// written). A malformed file logs a warning and yields an empty map so absent
// phases are reported as not-ok (0) rather than crashing the gate.
func readInstallPhases(path string) map[string]string {
	data, err := os.ReadFile(path)
	if err != nil {
		// Missing is expected when an early phase fails before writing phases.json.
		return map[string]string{}
	}
	var m map[string]string
	if err := json.Unmarshal(data, &m); err != nil {
		fmt.Fprintf(os.Stderr, "warning: malformed phases.json: %v\n", err)
		return map[string]string{}
	}
	return m
}
