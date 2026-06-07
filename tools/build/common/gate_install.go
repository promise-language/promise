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

// defaultInstallBaseURL is the published dist bucket the gate fetches the install
// script + assets from while the repo is private (T0803). Overridable via
// PROMISE_BASE_URL. T0804: once the repo is public this points at GitHub
// releases and the override goes away.
const defaultInstallBaseURL = "https://prebuilts.promise-lang.org/dist"

// installPhases are the gate phases recorded in phases.json, in execution order.
// Each maps to an `install_<variant>_<phase>_ok` ∈ {0,1} metric in the envelope.
var installPhases = []string{"fetch", "install", "sanity", "test"}

// runGateInstall runs the end-to-end install gate for one variant (thin|full)
// and writes the structured JSON gate envelope to stdout. Phase progress goes to
// stderr so stdout stays clean JSON.
func runGateInstall(root string, args []string) error {
	variant := ""
	system := false
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--variant", "-variant":
			if i+1 >= len(args) {
				return fmt.Errorf("usage: bin/gate install --variant {thin|full} [--system]")
			}
			i++
			variant = args[i]
		case "--variant=thin", "-variant=thin":
			variant = "thin"
		case "--variant=full", "-variant=full":
			variant = "full"
		case "--system", "-system":
			system = true
		default:
			return fmt.Errorf("usage: bin/gate install --variant {thin|full} [--system]")
		}
	}
	if variant != "thin" && variant != "full" {
		return fmt.Errorf("bin/gate install: --variant must be thin or full")
	}

	baseURL := strings.TrimSpace(os.Getenv("PROMISE_BASE_URL"))
	if baseURL == "" {
		baseURL = defaultInstallBaseURL
	}
	hostTarget := strings.ToLower(runtime.GOOS) + "-" + runtime.GOARCH

	// Scratch dir for the phase artifacts (phases.json, tests.jsonl) and — in
	// clean-slate mode — the sandbox HOME. Removed on exit.
	work, err := os.MkdirTemp("", "gate-install-")
	if err != nil {
		return fmt.Errorf("mktemp: %w", err)
	}
	defer os.RemoveAll(work)

	fmt.Fprintf(os.Stderr, "Running install gate (variant=%s, system=%v, base-url=%s)...\n", variant, system, baseURL)
	phaseErr := runInstallPhases(root, work, variant, baseURL, system)

	// Aggregate the phase artifacts into the standard envelope.
	out := buildInstallGateOutput(root, hostTarget, variant, work)

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
func runInstallPhases(root, work, variant, baseURL string, system bool) error {
	phases := map[string]string{"fetch": "fail", "install": "fail", "sanity": "fail", "test": "fail"}
	logf := func(format string, a ...any) {
		fmt.Fprintf(os.Stderr, "[gate-install:%s] %s\n", variant, fmt.Sprintf(format, a...))
	}
	defer func() {
		data, _ := json.Marshal(phases)
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
		home := filepath.Join(work, "home")
		if err := os.MkdirAll(home, 0o755); err != nil {
			return fmt.Errorf("sandbox home: %w", err)
		}
		promiseHome = filepath.Join(home, ".promise")
		overrides[homeKey] = home
		overrides["PROMISE_HOME"] = promiseHome
		// Scrub PATH to a minimal toolchain set on POSIX so the gate doesn't lean
		// on the dev environment. On Windows PowerShell needs the inherited PATH,
		// so it's left intact there.
		if runtime.GOOS != "windows" {
			overrides["PATH"] = "/usr/bin:/bin:/usr/sbin:/sbin"
		}
		logf("clean-slate sandbox: %s=%s PROMISE_HOME=%s", homeKey, home, promiseHome)
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
	scriptPath := filepath.Join(work, installScript)
	logf("fetching install script from %s/%s", baseURL, installScript)
	if err := downloadFile(baseURL+"/"+installScript, scriptPath); err != nil {
		logf("fetch failed: %v", err)
		return fmt.Errorf("fetch: %w", err)
	}
	phases["fetch"] = "pass"

	// ── phase: run the installer (the one OS-specific invocation) ─────────────
	logf("running installer (PROMISE_BASE_URL=%s variant=%s)", baseURL, variant)
	var installCmd *exec.Cmd
	if runtime.GOOS == "windows" {
		a := []string{"-ExecutionPolicy", "Bypass", "-File", scriptPath}
		if variant == "full" {
			a = append(a, "-Full")
		}
		installCmd = exec.Command("powershell", a...)
	} else {
		a := []string{scriptPath}
		if variant == "full" {
			a = append(a, "--full")
		}
		installCmd = exec.Command("sh", a...)
	}
	installCmd.Env = envWith(baseEnv, map[string]string{"PROMISE_BASE_URL": baseURL})
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

	// ── phase: run the full suite through the INSTALLED stub ──────────────────
	logf("running full suite through installed stub (source=%s)", root)
	testEnv := baseEnv
	if variant == "full" {
		// A full binary pre-stages host blobs at install, so the compile/test step
		// must never reach the network. Go's net/http honors these proxy vars, so
		// any blob/archive fetch fails fast. (An arena may also wrap this in
		// `unshare -n` for a hard guarantee; the proxy is the portable path.)
		logf("full variant: enforcing offline (proxy blackhole)")
		testEnv = envWith(baseEnv, map[string]string{
			"HTTPS_PROXY": "http://127.0.0.1:1",
			"HTTP_PROXY":  "http://127.0.0.1:1",
			"ALL_PROXY":   "http://127.0.0.1:1",
		})
	}
	// examples are the floor; tests/ + modules/ are the target. stdout = the
	// --json record stream (captured to tests.jsonl); stderr = human progress.
	testCmd := exec.Command(promiseBin, "test", "-timeout", "10", "--json", "examples/...", "tests/...", "modules/...")
	testCmd.Dir = root
	testCmd.Env = testEnv
	testCmd.Stderr = os.Stderr
	var buf bytes.Buffer
	testCmd.Stdout = &buf
	testErr := testCmd.Run()
	if werr := os.WriteFile(filepath.Join(work, "tests.jsonl"), buf.Bytes(), 0o644); werr != nil {
		logf("warning: write tests.jsonl: %v", werr)
	}
	if testErr != nil {
		logf("test phase failed: %v", testErr)
		return fmt.Errorf("test: %w", testErr)
	}
	phases["test"] = "pass"
	logf("all phases passed")
	return nil
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
func buildInstallGateOutput(root, hostTarget, variant, work string) *GateOutput {
	metricPrefix := "install_" + variant
	jsonl, _ := os.ReadFile(filepath.Join(work, "tests.jsonl"))
	out := BuildGateOutput(root, hostTarget, metricPrefix, "install-"+variant, string(jsonl))

	phases := readInstallPhases(filepath.Join(work, "phases.json"))
	for _, p := range installPhases {
		ok := 0.0
		if phases[p] == "pass" {
			ok = 1
		}
		out.Metrics[metricPrefix+"_"+p+"_ok"] = ok
	}
	return out
}

// readInstallPhases reads the phases.json ({"fetch":"pass|fail",...}). A missing
// or malformed file yields an empty map so absent phases are reported as not-ok
// (0) rather than crashing the gate.
func readInstallPhases(path string) map[string]string {
	data, err := os.ReadFile(path)
	if err != nil {
		return map[string]string{}
	}
	var m map[string]string
	if err := json.Unmarshal(data, &m); err != nil {
		return map[string]string{}
	}
	return m
}
