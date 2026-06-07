package common

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// release_workflow_test.go guards the GitHub-side orchestration added in T0774:
// the `.github/workflows/release.yml` pipeline (which is a thin wrapper around the
// `bin/release` driver, T0773) and the committed installer scripts. None of this
// runs end-to-end without GitHub runners + network + a published release, so these
// tests cover the parts that ARE statically verifiable: that every `bin/release`
// invocation the workflow makes targets a real subcommand+flag of the CLI, and
// that the installer scripts' checksum-extraction logic stays exact-match (the
// documented fix that distinguishes the thin `promise-<os>-<arch>` asset from the
// `-full` one).

// ── workflow ↔ CLI contract ─────────────────────────────────────────────────

// expr collapses a `${{ ... }}` GitHub Actions expression (which may contain
// spaces, e.g. `${{ github.ref_name }}`) into a single placeholder token so the
// surrounding command can be whitespace-split.
var expr = regexp.MustCompile(`\$\{\{[^}]*\}\}`)

// mergeContinuations joins shell line-continuations (`... \` + newline) so a
// multi-line `run:` command becomes one logical line.
func mergeContinuations(content string) []string {
	var logical []string
	var cur strings.Builder
	for _, line := range strings.Split(content, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasSuffix(trimmed, "\\") {
			cur.WriteString(strings.TrimSuffix(trimmed, "\\"))
			cur.WriteByte(' ')
			continue
		}
		cur.WriteString(trimmed)
		logical = append(logical, cur.String())
		cur.Reset()
	}
	if cur.Len() > 0 {
		logical = append(logical, cur.String())
	}
	return logical
}

// parseReleaseInvocations extracts every `bin/release <sub> <args...>` command
// from workflow YAML, skipping comment lines (where `bin/release` is only
// mentioned in prose). Each result is the token list AFTER `bin/release`, with
// `${{ ... }}` expressions collapsed.
func parseReleaseInvocations(content string) [][]string {
	var invs [][]string
	for _, line := range mergeContinuations(content) {
		t := strings.TrimSpace(line)
		if strings.HasPrefix(t, "#") {
			continue // comment — `bin/release` is only mentioned, not run
		}
		idx := strings.Index(line, "bin/release ")
		if idx < 0 {
			continue
		}
		cmd := expr.ReplaceAllString(line[idx:], "X")
		fields := strings.Fields(cmd)
		// fields[0] == "bin/release"; the rest is sub + args.
		invs = append(invs, fields[1:])
	}
	return invs
}

// realFlagsForSubcommand returns the set of flag names the CLI actually defines
// for a subcommand, captured from its `-h` usage output. This couples the test to
// the live flag definitions in release.go / release_build.go: rename a flag there
// and a workflow still passing the old name fails here.
func realFlagsForSubcommand(t *testing.T, root, sub string) map[string]bool {
	t.Helper()
	usage := captureStderr(t, func() {
		// -h makes flag.Parse short-circuit to ErrHelp BEFORE any real work, and
		// print the full flag list to os.Stderr.
		_ = RunRelease(root, []string{sub, "-h"})
	})
	flagLine := regexp.MustCompile(`(?m)^\s+-([a-zA-Z][\w-]*)`)
	flags := map[string]bool{}
	for _, m := range flagLine.FindAllStringSubmatch(usage, -1) {
		flags[m[1]] = true
	}
	if len(flags) == 0 {
		t.Fatalf("subcommand %q reported no flags in its -h usage:\n%s", sub, usage)
	}
	return flags
}

// captureStderr redirects os.Stderr to a pipe for the duration of fn and returns
// what was written. The flag package's usage printer resolves os.Stderr lazily,
// so the redirect captures it.
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	orig := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	os.Stderr = w
	done := make(chan string, 1)
	go func() {
		var sb strings.Builder
		buf := make([]byte, 4096)
		for {
			n, err := r.Read(buf)
			if n > 0 {
				sb.Write(buf[:n])
			}
			if err != nil {
				break
			}
		}
		done <- sb.String()
	}()
	fn()
	_ = w.Close()
	os.Stderr = orig
	out := <-done
	_ = r.Close()
	return out
}

// TestReleaseWorkflowInvocationsMatchCLI is the static integrity gate for T0774's
// release.yml: every `bin/release` call it makes must target a real subcommand and
// only real flags, and the four pipeline subcommands must all be exercised. This
// catches workflow/CLI drift that otherwise stays latent until a release is cut
// (the workflow is not run end-to-end in CI).
func TestReleaseWorkflowInvocationsMatchCLI(t *testing.T) {
	root, err := FindRoot()
	if err != nil {
		t.Skipf("find root: %v", err)
	}
	wf := filepath.Join(root, ".github", "workflows", "release.yml")
	content, err := os.ReadFile(wf)
	if err != nil {
		t.Skipf("read %s: %v", wf, err)
	}

	invs := parseReleaseInvocations(string(content))
	if len(invs) == 0 {
		t.Fatal("no bin/release invocations found in release.yml")
	}

	flagCache := map[string]map[string]bool{}
	seenSubs := map[string]bool{}
	for _, inv := range invs {
		sub := inv[0]
		seenSubs[sub] = true

		// 1. Subcommand must dispatch — assert against the live RunRelease table.
		if derr := RunRelease(t.TempDir(), []string{sub}); derr != nil &&
			strings.Contains(derr.Error(), "unknown subcommand") {
			t.Errorf("release.yml invokes `bin/release %s` but that subcommand is not dispatched by RunRelease", sub)
			continue
		}

		// 2. Every flag passed must be defined on that subcommand.
		flags, ok := flagCache[sub]
		if !ok {
			flags = realFlagsForSubcommand(t, root, sub)
			flagCache[sub] = flags
		}
		for _, tok := range inv[1:] {
			if !strings.HasPrefix(tok, "-") {
				continue // positional or value
			}
			name := strings.TrimLeft(strings.SplitN(tok, "=", 2)[0], "-")
			if name == "" {
				continue
			}
			if !flags[name] {
				t.Errorf("release.yml passes --%s to `bin/release %s`, which has no such flag (defined: %v)",
					name, sub, sortedKeys(flags))
			}
		}
	}

	// 3. The whole build order must be present (manifest→thin/full→verify).
	// T0797 removed the per-epoch `blobs` job (it is now local-only); blobs
	// are pulled from the deps release via `fetch-blobs` instead.
	for _, sub := range []string{"manifest", "build", "fetch-blobs", "verify-manifest"} {
		if !seenSubs[sub] {
			t.Errorf("release.yml never invokes `bin/release %s` — the pipeline build order is incomplete", sub)
		}
	}
}

func sortedKeys(m map[string]bool) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	// small set; insertion sort keeps the test output stable without importing sort
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j-1] > out[j]; j-- {
			out[j-1], out[j] = out[j], out[j-1]
		}
	}
	return out
}

// ── installer checksum-extraction (the T0774 fix) ───────────────────────────

// TestInstallChecksumExactMatch guards the documented installer fix: SHA256SUMS
// lists BOTH the thin (`promise-linux-amd64.gz`) and full
// (`promise-linux-amd64-full.gz`) assets, so the old substring `grep
// "$BINARY_NAME"` matched two lines and produced a guaranteed "checksum
// mismatch". The scripts now match the filename field EXACTLY (install.sh: awk
// `$2 == name`; install.ps1: `-eq $AssetName`). Asset names carry a `.gz`
// suffix since T0796 (compression-only publishing).
func TestInstallChecksumExactMatch(t *testing.T) {
	const (
		hashThin    = "1111111111111111111111111111111111111111111111111111111111111111"
		hashFull    = "2222222222222222222222222222222222222222222222222222222222222222"
		hashWindows = "3333333333333333333333333333333333333333333333333333333333333333"
	)
	// Order matters: the thin line is a strict prefix of the full filename, the
	// exact case the fix targets.
	sums := hashThin + "  promise-linux-amd64.gz\n" +
		hashFull + "  promise-linux-amd64-full.gz\n" +
		hashWindows + "  promise-windows-amd64.exe.gz\n"

	// The old buggy behavior, made executable: a substring match on the thin name
	// hits BOTH the thin and full lines (so awk/grep would emit two hashes).
	substringHits := 0
	for _, line := range strings.Split(strings.TrimSpace(sums), "\n") {
		if strings.Contains(line, "promise-linux-amd64") {
			substringHits++
		}
	}
	if substringHits < 2 {
		t.Fatalf("fixture is not exercising the bug: substring match hit %d lines, want >=2", substringHits)
	}

	// The fix, run via the real awk program embedded in install.sh.
	if _, err := exec.LookPath("awk"); err != nil {
		t.Skip("awk not available; skipping awk-semantics check")
	}
	got := runAwkExtract(t, sums, "promise-linux-amd64.gz")
	if got != hashThin {
		t.Errorf("awk exact-match for promise-linux-amd64.gz = %q, want the thin hash %q (a substring match would have returned two hashes)", got, hashThin)
	}
	// The full name must resolve to its own hash, not the thin one.
	if got := runAwkExtract(t, sums, "promise-linux-amd64-full.gz"); got != hashFull {
		t.Errorf("awk exact-match for promise-linux-amd64-full.gz = %q, want %q", got, hashFull)
	}
}

// runAwkExtract runs the exact awk program install.sh uses for checksum lookup.
func runAwkExtract(t *testing.T, sums, name string) string {
	t.Helper()
	cmd := exec.Command("awk", "-v", "name="+name, "$2 == name { print $1 }")
	cmd.Stdin = strings.NewReader(sums)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("awk: %v", err)
	}
	return strings.TrimSpace(string(out))
}

// TestInstallScriptsUseExactMatch is a source-level regression guard: it asserts
// the committed installer scripts still use the exact-match form and have NOT
// reverted to the buggy substring grep. Pairs with TestInstallChecksumExactMatch
// (which proves WHY exact match is required).
func TestInstallScriptsUseExactMatch(t *testing.T) {
	root, err := FindRoot()
	if err != nil {
		t.Skipf("find root: %v", err)
	}

	sh := readScript(t, root, "install.sh")
	if !strings.Contains(sh, "$2 == name") {
		t.Error("install.sh no longer uses the exact awk `$2 == name` filename match")
	}
	if strings.Contains(sh, `grep "${BINARY_NAME}" "$TMP_SUMS"`) {
		t.Error("install.sh reverted to the buggy substring `grep \"${BINARY_NAME}\"` checksum lookup")
	}

	ps1 := readScript(t, root, "install.ps1")
	// T0796 renamed `$BinaryName` → `$AssetName` (the .gz published asset; the
	// runtime .exe is `$RuntimeName`). Either name satisfies the guard — what
	// matters is the exact `-eq` match, not the variable identifier.
	if !strings.Contains(ps1, "-eq $AssetName") && !strings.Contains(ps1, "-eq $BinaryName") {
		t.Error("install.ps1 no longer uses an exact `-eq $AssetName` (or legacy `-eq $BinaryName`) filename match")
	}
}

func readScript(t *testing.T, root, name string) string {
	t.Helper()
	p := filepath.Join(root, "scripts", name)
	data, err := os.ReadFile(p)
	if err != nil {
		t.Skipf("read %s: %v", p, err)
	}
	return string(data)
}

// ── T0796: gzip publish + decompress wiring ─────────────────────────────────

// TestReleaseWorkflowT0797Shape pins the structural changes T0797 makes to
// release.yml: the per-epoch `blobs` matrix job is gone (it's local-only now),
// `compiler` projects the manifest from the committed blobs catalog instead of
// hashing local blobs, both `compiler` and `publish` use `fetch-blobs` to pull
// pre-hosted blobs from the deps release on demand, and `dist/deps/*` is no
// longer attached to the epoch GitHub release. A regression in any one would
// silently re-introduce the 10-min brotli-11 and 700 MB LLVM download per
// release that the task description called out.
func TestReleaseWorkflowT0797Shape(t *testing.T) {
	root, err := FindRoot()
	if err != nil {
		t.Skipf("find root: %v", err)
	}
	wf := filepath.Join(root, ".github", "workflows", "release.yml")
	data, err := os.ReadFile(wf)
	if err != nil {
		t.Skipf("read %s: %v", wf, err)
	}
	content := string(data)

	// 1. No `blobs:` job key. Match `\n  blobs:\n` so prose mentions of the
	// word "blobs" don't trigger a false positive.
	if strings.Contains(content, "\n  blobs:\n") {
		t.Error("release.yml still defines a top-level `blobs:` job — T0797 moved blob production off the per-epoch path")
	}
	// And the `compiler` job must NOT need [setup, blobs].
	if strings.Contains(content, "needs: [setup, blobs]") {
		t.Error("compiler job still `needs: [setup, blobs]` — the blobs job is gone")
	}

	// 2. Manifest is projected from the catalog (no positional <blobsdir>, the
	// `--from-catalog` switch is the marker).
	if !strings.Contains(content, "manifest --from-catalog") {
		t.Error("release.yml does not project the manifest via `manifest --from-catalog` — without it, the workflow still relies on a deleted blobs matrix")
	}
	// Conversely: the legacy `bin/release manifest dist/blobs ...` invocation
	// is no longer used (its blobs dir came from the deleted job).
	if strings.Contains(content, "bin/release manifest dist/blobs") {
		t.Error("release.yml still invokes legacy `bin/release manifest dist/blobs ...` — that input dir disappeared with the blobs job")
	}

	// 3. fetch-blobs runs in both compiler and publish.
	if !strings.Contains(content, "bin/release fetch-blobs") {
		t.Error("release.yml does not invoke `bin/release fetch-blobs` — the full variant and verify-manifest both depend on it")
	}
	// publish must use --keep-compressed so verify-manifest's --against dir
	// holds the <sha>.br assets it hashes.
	if !strings.Contains(content, "--keep-compressed") {
		t.Error("publish job's fetch-blobs is missing --keep-compressed — verify-manifest hashes the compressed bytes")
	}

	// 4. The epoch GitHub release no longer attaches `dist/deps/*` (those are
	// the LLVM blobs that now live in deps-<dep>-<version>).
	if strings.Contains(content, "dist/deps/*") {
		t.Error("release.yml still attaches `dist/deps/*` to the epoch release — those blobs live in deps-<dep>-<version> now (T0797)")
	}
	// And the `release-blobs-<host>` artifact (uploaded by the deleted blobs
	// job) is no longer downloaded.
	if strings.Contains(content, "release-blobs-") {
		t.Error("release.yml still references `release-blobs-<host>` artifacts — those came from the deleted blobs job")
	}

	// 5. The from-catalog projection MUST NOT pass --tag. Doing so would
	// override the catalog-derived deps-<dep>-<version> tag with the epoch
	// tag, making the manifest's blob URLs point at a release that does not
	// host them — every fetch-blobs call would 404. Locate the projection
	// step's `run:` block and assert no --tag inside it.
	const startMarker = "manifest --from-catalog"
	idx := strings.Index(content, startMarker)
	if idx < 0 {
		t.Fatal("could not locate `manifest --from-catalog` step to inspect for stray --tag")
	}
	// Bound the search to the bash heredoc-style run block: from the marker
	// until either the next `- name:`/`- uses:` step OR end of file.
	tail := content[idx:]
	stepEnd := len(tail)
	for _, terminator := range []string{"\n      - name:", "\n      - uses:"} {
		if i := strings.Index(tail, terminator); i > 0 && i < stepEnd {
			stepEnd = i
		}
	}
	if strings.Contains(tail[:stepEnd], "--tag") {
		t.Error("the `manifest --from-catalog` step passes --tag — the deps release tag is catalog-derived; an override (e.g. ${{ github.ref_name }}) would yield a manifest whose URLs fetch-blobs cannot resolve")
	}
}

// TestReleaseWorkflowCompressesAssets pins the T0796 publish format: every
// platform binary must be gzipped before SHA256SUMS is generated and only the
// .gz assets get attached to the release. The three checks form a single
// contract — gzip with reproducible flags, hash the .gz, publish only .gz —
// and a regression in any one would silently break either reproducibility,
// the install scripts, or `promise update`.
func TestReleaseWorkflowCompressesAssets(t *testing.T) {
	root, err := FindRoot()
	if err != nil {
		t.Skipf("find root: %v", err)
	}
	wf := filepath.Join(root, ".github", "workflows", "release.yml")
	data, err := os.ReadFile(wf)
	if err != nil {
		t.Skipf("read %s: %v", wf, err)
	}
	content := string(data)

	// 1. gzip step exists and uses `-9 -n`. `-n` strips the embedded mtime so
	// re-running the workflow on the same commit produces byte-identical
	// artifacts (and therefore stable SHA256SUMS).
	if !strings.Contains(content, "gzip -9 -n") {
		t.Error("release.yml no longer runs `gzip -9 -n` — reproducible builds depend on `-n` stripping the mtime")
	}

	// 2. SHA256SUMS is computed over the .gz assets (what the user downloads),
	// not the uncompressed binaries.
	if !strings.Contains(content, "sha256sum promise-*.gz") {
		t.Error("release.yml no longer hashes the .gz assets — install.sh / install.ps1 / promise update all verify over the compressed download")
	}
	if strings.Contains(content, "sha256sum promise-* >") {
		t.Error("release.yml is still hashing the uncompressed binaries — that breaks the documented `verify the downloaded asset` invariant")
	}

	// 3. Only the .gz artifacts get attached to the release; no raw binary is
	// published (compression-only, per T0796 acceptance).
	if !strings.Contains(content, "dist/bin/promise-*.gz") {
		t.Error("release.yml no longer publishes `dist/bin/promise-*.gz`")
	}
	if strings.Contains(content, "dist/bin/promise-* \\") {
		t.Error("release.yml is still uploading raw uncompressed `dist/bin/promise-*` — T0796 mandates compression-only publishing")
	}
}

// TestWorkflowsDropLLVMInstall pins T0798: ci.yml AND release.yml's compiler
// job must NOT carry the per-OS `Install LLVM …` shim. `bin/build`/
// `bin/release build` self-fetch the pinned LLVM from the slim blob catalog;
// re-introducing an apt-get / brew install / extract-into-USERPROFILE step
// would (a) waste minutes of runner time the slim cache already saves, and
// (b) silently shadow the catalog-driven build with a system LLVM that may be
// at a different version (the exact drift T0790 originated from).
func TestWorkflowsDropLLVMInstall(t *testing.T) {
	root, err := FindRoot()
	if err != nil {
		t.Skipf("find root: %v", err)
	}
	cases := []struct {
		wf       string
		sentinel []string
	}{
		{
			wf: filepath.Join(root, ".github", "workflows", "ci.yml"),
			sentinel: []string{
				"apt.llvm.org",
				"apt-get install -y llvm",
				"brew install llvm",
				"clang+llvm-22.1.0-x86_64-pc-windows",
				"USERPROFILE\\LLVM",
			},
		},
		{
			wf: filepath.Join(root, ".github", "workflows", "release.yml"),
			sentinel: []string{
				"apt.llvm.org",
				"apt-get install -y llvm",
				"brew install llvm",
				"clang+llvm-22.1.0-x86_64-pc-windows",
				"USERPROFILE\\LLVM",
			},
		},
	}
	for _, tc := range cases {
		data, err := os.ReadFile(tc.wf)
		if err != nil {
			t.Skipf("read %s: %v", tc.wf, err)
			continue
		}
		content := string(data)
		for _, s := range tc.sentinel {
			if strings.Contains(content, s) {
				t.Errorf("%s still contains %q — T0798 removed system-LLVM install steps; `bin/build` self-fetches the pinned LLVM from the slim blob catalog",
					filepath.Base(tc.wf), s)
			}
		}
		// Belt-and-suspenders: the bootstrap step (`./make` or `.\make.cmd`)
		// MUST still be present — without it, the slim fetch path has no
		// `bin/build` to run.
		if !strings.Contains(content, "./make") && !strings.Contains(content, `.\make.cmd`) {
			t.Errorf("%s no longer bootstraps via ./make or .\\make.cmd — the slim-blob fetch path runs from bin/build",
				filepath.Base(tc.wf))
		}
	}
}

// TestInstallScriptsDecompressGzip pins T0796's consumer wiring: both shell
// scripts must download a `.gz` asset and decompress it before installing.
// The exact-match check in TestInstallScriptsUseExactMatch already validates
// SHA256SUMS lookup; this test validates the decompress step that the
// "compression-only" publishing requires every consumer to handle.
func TestInstallScriptsDecompressGzip(t *testing.T) {
	root, err := FindRoot()
	if err != nil {
		t.Skipf("find root: %v", err)
	}

	// install.sh: must form a `.gz` URL and gunzip after verification.
	sh := readScript(t, root, "install.sh")
	if !strings.Contains(sh, ".gz") {
		t.Error("install.sh does not reference .gz assets — compression-only publishing requires the script to download the .gz")
	}
	if !strings.Contains(sh, "gunzip") {
		t.Error("install.sh does not call gunzip — the downloaded .gz must be decompressed before install")
	}
	// Belt-and-suspenders: the runtime/asset naming separation introduced in
	// T0796 is what makes the verify-before-decompress order work.
	if !strings.Contains(sh, "ASSET_NAME") || !strings.Contains(sh, "RUNTIME_NAME") {
		t.Error("install.sh no longer separates ASSET_NAME (download/verify) from RUNTIME_NAME (decompressed binary) — verify-over-.gz invariant depends on it")
	}

	// install.ps1: must use the in-process GzipStream (no external gzip CLI on
	// Windows, per the design note in the T0796 description).
	ps1 := readScript(t, root, "install.ps1")
	if !strings.Contains(ps1, ".gz") {
		t.Error("install.ps1 does not reference .gz assets")
	}
	if !strings.Contains(ps1, "GzipStream") {
		t.Error("install.ps1 no longer uses System.IO.Compression.GzipStream — a regression to an external gzip dependency would break the fresh-machine install path on Windows")
	}
	if !strings.Contains(ps1, "$AssetName") || !strings.Contains(ps1, "$RuntimeName") {
		t.Error("install.ps1 no longer separates $AssetName from $RuntimeName")
	}
}
