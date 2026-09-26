package common

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// runtime_slim_test.go covers the WASM test runtimes as pinned prebuilts
// (T2169): that the pin table actually declares them for every host a compiler
// is built for, that a staged runtime comes back executable, and that the
// manifest name the compiler will ask for is the one the projection writes.

// runtimePrebuiltsTOML renders a prebuilts.toml declaring llvm (so the
// BuildRuntimeManifestFromCatalog lookup succeeds) plus one WASM runtime whose
// single file is served from url.
func runtimePrebuiltsTOML(target, dep, out, url, sha string) string {
	return `schema = 1
[binaries.llvm]
version = "22.1.0"
bundle_dir = "compiler/cmd/promise/resources/llvm"
[binaries.llvm.targets.` + target + `]
url = "https://example.test/LLVM.tar.xz"
sha256 = "deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef0"
files = [
  { src = "bin/opt", out = "opt" },
]

[binaries.` + dep + `]
version = "9.9.9"
bundle_dir = ""
[binaries.` + dep + `.targets.` + target + `]
url = "` + url + `"
sha256 = "` + sha + `"
files = [
  { src = "` + out + `", out = "` + out + `" },
]
`
}

func writeRuntimeRoot(t *testing.T, toml string) string {
	t.Helper()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "catalog.toml"), []byte("epoch = \"2026.0\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	toolsBuild := filepath.Join(root, "tools", "build")
	if err := os.MkdirAll(toolsBuild, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(toolsBuild, "prebuilts.toml"), []byte(toml), 0o644); err != nil {
		t.Fatal(err)
	}
	return root
}

// An empty bundle_dir means "never embedded", and the manifest loader has to
// accept it: the WASM runtimes are the first dependencies with no embed step at
// all, and a validator that demands a directory nothing writes to would force a
// fictional path into the pin table.
func TestPrebuiltsManifest_EmptyBundleDirIsValid(t *testing.T) {
	root := writeRuntimeRoot(t, runtimePrebuiltsTOML("linux-amd64", "wasmtime", "wasmtime",
		"https://example.test/wasmtime.tar.xz", "deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef1"))
	pm, err := LoadPrebuiltsManifest(root)
	if err != nil {
		t.Fatalf("a dependency with an empty bundle_dir was rejected: %v", err)
	}
	if pm.Binaries["wasmtime"].BundleDir != "" {
		t.Errorf("bundle_dir = %q, want empty", pm.Binaries["wasmtime"].BundleDir)
	}
}

// The real pin table, not a fixture: every host a compiler can be built for must
// have BOTH runtimes pinned, or that platform silently loses a wasm suite —
// which is the exact failure T2169 exists to end, just relocated from the host's
// PATH into our own table.
func TestPrebuiltsManifest_DeclaresWasmRuntimesForEveryBuildableHost(t *testing.T) {
	root, err := RootForTests()
	if err != nil {
		t.Skipf("find root: %v", err)
	}
	pm, err := LoadPrebuiltsManifest(root)
	if err != nil {
		t.Fatal(err)
	}
	llvm := pm.Binaries["llvm"]
	if llvm == nil {
		t.Fatal("prebuilts.toml declares no llvm")
	}
	for target, llvmEntry := range llvm.Targets {
		// A host with no LLVM builds no compiler, so it needs no runtime — but
		// it must say so rather than simply omitting the cell, which reads as an
		// oversight.
		for _, dep := range WasmRuntimeDeps() {
			bin := pm.Binaries[dep]
			if bin == nil {
				t.Fatalf("prebuilts.toml declares no %s", dep)
			}
			entry := bin.Targets[target]
			if entry == nil {
				t.Errorf("%s has no entry for %s, which LLVM is pinned for", dep, target)
				continue
			}
			if llvmEntry.Unsupported != "" {
				if entry.Unsupported == "" {
					t.Errorf("%s/%s is pinned but LLVM is unsupported there — no compiler exists to run it", dep, target)
				}
				continue
			}
			if entry.Unsupported != "" {
				t.Errorf("%s/%s is marked unsupported, so that host can run no wasm suite", dep, target)
				continue
			}
			if strings.TrimSpace(entry.SHA256) == "" {
				t.Errorf("%s/%s has no sha256 — an unpinned pin verifies nothing", dep, target)
			}
			// One file each is the whole premise: neither runtime needs a
			// supporting tree, which is why pinning Node is affordable at all.
			if len(entry.Files) != 1 {
				t.Errorf("%s/%s declares %d files, want exactly 1", dep, target, len(entry.Files))
				continue
			}
			if got, want := entry.Files[0].Out, RuntimeExeName(dep, target); got != want {
				t.Errorf("%s/%s out = %q, want %q (the name the view and the resolver look for)", dep, target, got, want)
			}
		}
	}
}

// The manifest name is a cross-module contract: the compiler asks for it by
// this exact spelling (runtimeManifestName in llvm_cas.go) and the two live in
// separate Go modules, so nothing but a test on each side keeps them in step.
func TestRuntimeManifestName(t *testing.T) {
	for _, tc := range []struct{ dep, want string }{
		{"wasmtime", "runtime-wasmtime"},
		{"node", "runtime-node"},
	} {
		if got := RuntimeManifestName(tc.dep); got != tc.want {
			t.Errorf("RuntimeManifestName(%q) = %q, want %q", tc.dep, got, tc.want)
		}
	}
}

// Every host's spelling is asserted on every host. The Windows branch would
// otherwise be checked only where Windows runs, which is the shape T2206 and
// T2152 both had — hence the target is a parameter rather than runtime.GOOS.
func TestRuntimeExeName_NamesThePlatformRatherThanReadingIt(t *testing.T) {
	for _, tc := range []struct{ dep, target, want string }{
		{"node", "linux-amd64", "node"},
		{"node", "darwin-arm64", "node"},
		{"node", "windows-amd64", "node.exe"},
		{"wasmtime", "linux-arm64", "wasmtime"},
		{"wasmtime", "windows-amd64", "wasmtime.exe"},
	} {
		if got := RuntimeExeName(tc.dep, tc.target); got != tc.want {
			t.Errorf("RuntimeExeName(%q, %q) = %q, want %q", tc.dep, tc.target, got, tc.want)
		}
	}
}

// A name that is not a WASM runtime is refused rather than fetched. The error
// is the useful half: a typo here would otherwise become a confusing
// "not declared in manifest" from two layers down.
func TestEnsureWasmRuntime_RejectsAnUnknownRuntime(t *testing.T) {
	root := writeRuntimeRoot(t, runtimePrebuiltsTOML(CurrentBuildTarget(), "wasmtime", "wasmtime",
		"https://example.test/wasmtime.tar.xz", ""))
	_, err := EnsureWasmRuntime(root, "deno")
	if err == nil {
		t.Fatal("an unknown runtime was accepted")
	}
	if !strings.Contains(err.Error(), "wasmtime") || !strings.Contains(err.Error(), "node") {
		t.Errorf("error = %v, want it to name the runtimes that do exist", err)
	}
}

// executableTarGz builds a .tar.gz holding one file at `name`, marked
// executable the way every upstream runtime archive marks its binary.
func executableTarGz(t *testing.T, name, content string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: int64(len(content))}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write([]byte(content)); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// Staging returns a path to the executable itself, not the directory holding
// it, and the file has to be runnable. A runtime staged without the execute bit
// fails at exec time — an EACCES naming a path, far from the staging step that
// caused it — so the mode is asserted here where it is set.
func TestEnsureWasmRuntime_ReturnsAnExecutablePath(t *testing.T) {
	target := CurrentBuildTarget()
	exe := RuntimeExeName("wasmtime", target)
	// Wrapped in a versioned directory, as every upstream release archive is;
	// resolveInnerRoot is what has to see through that.
	tarBytes := executableTarGz(t, "wasmtime-v9.9.9-host/"+exe, "#!/bin/true\n")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(tarBytes)
	}))
	t.Cleanup(srv.Close)

	root := writeRuntimeRoot(t, runtimePrebuiltsTOML(target, "wasmtime", exe,
		srv.URL+"/wasmtime.tar.gz", sha256Hex(tarBytes)))
	t.Setenv("PROMISE_PREBUILTS_CACHE", t.TempDir())

	// No catalog entry → ensureSlimBlobs falls through to the pinned upstream
	// archive. That is also the path a maintainer hits between pinning a new
	// version and publishing its blobs, so it must work rather than merely warn.
	path, err := EnsureWasmRuntime(root, "wasmtime")
	if err != nil {
		t.Fatalf("staging the pinned wasmtime: %v", err)
	}
	if filepath.Base(path) != exe {
		t.Errorf("path = %q, want it to end in the executable %q", path, exe)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat staged runtime: %v", err)
	}
	if info.Mode().Perm()&0o111 == 0 {
		t.Errorf("staged runtime mode = %v, want the execute bit set", info.Mode().Perm())
	}
}

// ── manifest projection (the client's only route to a runtime) ───────────────

const testRuntimeVersion = "9.9.9"

// bothRuntimesTOML declares llvm (so BuildRuntimeManifestFromCatalog's
// llvmTargetEntry lookup succeeds) plus BOTH wasm runtimes for target, using
// each platform's real `out` spelling so the windows `.exe` rows are exercised
// on every host.
func bothRuntimesTOML(target string, unsupported bool) string {
	s := `schema = 1
[binaries.llvm]
version = "22.1.0"
bundle_dir = "compiler/cmd/promise/resources/llvm"
[binaries.llvm.targets.` + target + `]
url = "https://example.test/LLVM.tar.xz"
sha256 = "deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef0"
files = [
  { src = "bin/opt", out = "opt" },
  { src = "bin/llc", out = "llc" },
]
`
	for _, dep := range WasmRuntimeDeps() {
		out := RuntimeExeName(dep, target)
		s += "\n[binaries." + dep + "]\nversion = \"" + testRuntimeVersion + "\"\nbundle_dir = \"\"\n" +
			"[binaries." + dep + ".targets." + target + "]\n"
		if unsupported {
			s += "unsupported = \"no upstream build for this platform\"\nurl = \"\"\nsha256 = \"\"\nfiles = []\n"
			continue
		}
		s += "url = \"https://example.test/" + dep + ".tar.xz\"\n" +
			"sha256 = \"deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef1\"\n" +
			"files = [ { src = \"inner/" + out + "\", out = \"" + out + "\" } ]\n"
	}
	return s
}

// seedRuntimeCatalog publishes both runtimes' blobs for target, so the
// projection takes its catalog-hit path.
func seedRuntimeCatalog(t *testing.T, root, target string) {
	t.Helper()
	cat, err := LoadBlobsCatalog(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, dep := range WasmRuntimeDeps() {
		raw := []byte("fake " + dep + " for " + target)
		br := brotliBytes(t, raw)
		if err := cat.Upsert(BlobEntry{
			Dependency:       dep,
			Version:          testRuntimeVersion,
			Target:           target,
			Name:             RuntimeExeName(dep, target),
			SHA256:           sha256Hex(raw),
			Size:             int64(len(raw)),
			Compression:      compressionBrotli,
			CompressedSize:   int64(len(br)),
			CompressedSHA256: sha256Hex(br),
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := WriteBlobsCatalog(root, cat); err != nil {
		t.Fatal(err)
	}
}

// A published runtime is projected as a `runtime-<dep>` entry with the three
// ranked sources the resolver walks. This is the client's ONLY route to a
// runtime — nothing embeds one — so an entry that is absent or misshapen means a
// thin binary cannot run a wasm target at all.
//
// Run for every target, not just this host: the runtimes are HOST dependencies,
// so each platform projects its own pair, and the windows row is the one whose
// `out` name differs.
func TestBuildRuntimeManifestFromCatalog_IncludesWasmRuntimes(t *testing.T) {
	for _, target := range []string{"linux-amd64", "linux-arm64", "darwin-arm64", "windows-amd64"} {
		t.Run(target, func(t *testing.T) {
			root := writeRuntimeRoot(t, bothRuntimesTOML(target, false))
			seedSlimCatalogFor(t, root, target, map[string]string{"opt": "OPT", "llc": "LLC"})
			seedRuntimeCatalog(t, root, target)

			m, err := BuildRuntimeManifestFromCatalog(root, target, "2026.0")
			if err != nil {
				t.Fatalf("BuildRuntimeManifestFromCatalog: %v", err)
			}
			byName := map[string]runtimeManifestEntry{}
			for _, e := range m.Entries {
				byName[e.Name] = e
			}
			for _, dep := range WasmRuntimeDeps() {
				name := RuntimeManifestName(dep)
				e, ok := byName[name]
				if !ok {
					t.Fatalf("missing %s entry (got %v)", name, byName)
				}
				raw := []byte("fake " + dep + " for " + target)
				if e.SHA256 != sha256Hex(raw) {
					t.Errorf("%s sha256 = %s, want the content hash", name, e.SHA256)
				}
				if e.Size != int64(len(raw)) {
					t.Errorf("%s size = %d, want %d", name, e.Size, len(raw))
				}
				// "blob", never "macho-llvm": a runtime must not receive the
				// install_name_tool patch an LLVM Mach-O gets.
				if e.Kind != "blob" {
					t.Errorf("%s kind = %q, want blob", name, e.Kind)
				}
				if len(e.Sources) != 3 {
					t.Fatalf("%s has %d sources, want 3 (release asset, mirror, upstream archive)", name, len(e.Sources))
				}
				if e.Sources[0].Blob == "" || !strings.Contains(e.Sources[0].Blob, DepsReleaseTag(dep, testRuntimeVersion)) {
					t.Errorf("%s primary source = %+v, want the deps release asset", name, e.Sources[0])
				}
				if e.Sources[1].Blob == "" || strings.Contains(e.Sources[1].Blob, "releases/download") {
					t.Errorf("%s second source = %+v, want the flat CAS mirror", name, e.Sources[1])
				}
				// The last resort names the member inside the upstream archive,
				// so a not-yet-published blob still resolves.
				wantMember := "inner/" + RuntimeExeName(dep, target)
				if e.Sources[2].Archive == "" || e.Sources[2].ArchivePath != wantMember {
					t.Errorf("%s fallback source = %+v, want upstream member %s", name, e.Sources[2], wantMember)
				}
			}
		})
	}
}

// Best-effort at projection: an unpublished runtime is skipped with a note, and
// must not strand the LLVM entries. Making it fatal would mean one unpublished
// runtime blob silently downgrades the whole binary to the empty placeholder.
func TestBuildRuntimeManifestFromCatalog_SkipsUnpublishedWasmRuntimes(t *testing.T) {
	root := writeRuntimeRoot(t, bothRuntimesTOML("linux-amd64", false))
	seedSlimCatalogFor(t, root, "linux-amd64", map[string]string{"opt": "OPT", "llc": "LLC"})
	// Deliberately do NOT seed the runtime blobs.

	m, err := BuildRuntimeManifestFromCatalog(root, "linux-amd64", "2026.0")
	if err != nil {
		t.Fatalf("an unpublished runtime blob must not fail the projection: %v", err)
	}
	for _, e := range m.Entries {
		if strings.HasPrefix(e.Name, "runtime-") {
			t.Errorf("projected an unpublished runtime entry %q — the compiler would fail to resolve it", e.Name)
		}
	}
	// The LLVM entries are what must survive, since nothing links without them.
	if len(m.Entries) == 0 {
		t.Error("the projection produced nothing; an unpublished runtime wiped the LLVM entries")
	}
}

// A target whose runtime is marked `unsupported` projects no entry for it, and
// still does not fail. That is the darwin-amd64 shape: the platform is declared
// so the refusal names its reason, not omitted as if forgotten.
func TestBuildRuntimeManifestFromCatalog_SkipsUnsupportedWasmRuntimes(t *testing.T) {
	root := writeRuntimeRoot(t, bothRuntimesTOML("linux-amd64", true))
	seedSlimCatalogFor(t, root, "linux-amd64", map[string]string{"opt": "OPT", "llc": "LLC"})
	seedRuntimeCatalog(t, root, "linux-amd64") // hosted, but the target is unsupported

	m, err := BuildRuntimeManifestFromCatalog(root, "linux-amd64", "2026.0")
	if err != nil {
		t.Fatalf("an unsupported runtime target must not fail the projection: %v", err)
	}
	for _, e := range m.Entries {
		if strings.HasPrefix(e.Name, "runtime-") {
			t.Errorf("projected %q for a target marked unsupported", e.Name)
		}
	}
}

// The catalog-miss note says which wasm target is lost, not merely which blob is
// absent — the reader's question is "what can this binary no longer do".
func TestWasmRuntimeTargetNote(t *testing.T) {
	for _, tc := range []struct{ dep, want string }{
		{"wasmtime", "wasm32-wasi"},
		{"node", "wasm32-web"},
	} {
		if got := wasmRuntimeTargetNote(tc.dep); got != tc.want {
			t.Errorf("wasmRuntimeTargetNote(%q) = %q, want %q", tc.dep, got, tc.want)
		}
	}
	// Every declared runtime must map to a target, or the note reads as a
	// confident wrong answer about a runtime nobody updated this for.
	for _, dep := range WasmRuntimeDeps() {
		if note := wasmRuntimeTargetNote(dep); !strings.HasPrefix(note, "wasm32-") {
			t.Errorf("wasmRuntimeTargetNote(%q) = %q, want a wasm32-* target", dep, note)
		}
	}
}

// ── bin/prereqs (what a developer is told) ──────────────────────────────────

// `bin/prereqs` must never offer a system install for a pinned runtime. It used
// to print `brew install wasmtime` / `winget` / `apt-get install nodejs`, which
// since T2169 is wrong twice over: installing one changes nothing (the host is
// never searched), and following the advice leaves the reader believing their
// wasm runs are using it. Same rule, and same test shape, as the LLVM line
// (TestRunPrereqs_LLVMNeverSuggestsASystemInstall).
func TestRunPrereqs_WasmRuntimesNeverSuggestASystemInstall(t *testing.T) {
	clearToolchainOverrides(t)
	t.Setenv("PROMISE_WASMTIME", "")
	t.Setenv("PROMISE_NODE", "")

	// root == "" → no prebuilts manifest to read, so every line takes its
	// unavailable/unknown branch without touching the network.
	out := captureStdout(t, func() {
		if err := RunPrereqs("", nil); err != nil {
			t.Errorf("RunPrereqs reports, it does not fail: %v", err)
		}
	})

	for _, dep := range WasmRuntimeDeps() {
		if !strings.Contains(out, dep+":") {
			t.Errorf("expected a %s line in the report:\n%s", dep, out)
		}
	}
	for _, unwanted := range []string{
		"brew install wasmtime", "brew install node",
		"winget install", "apt-get install nodejs",
		"wasmtime.dev/install.sh", "nodejs.org/",
	} {
		if strings.Contains(out, unwanted) {
			t.Errorf("prereqs must not offer a system runtime install (%q):\n%s", unwanted, out)
		}
	}
	// What it must say instead: the runtime is pinned, and how to stage it.
	if !strings.Contains(out, "pinned") {
		t.Errorf("the runtime lines should name the pinned runtime:\n%s", out)
	}
	if !strings.Contains(out, "bin/prereqs -wasm") {
		t.Errorf("the report should point at the materialization command:\n%s", out)
	}
}

// An unknown flag is a mistyped command line, and answering it with a report
// would hide the mistake. `-wasm` is the only flag, and it is real now — it used
// to be advertised by two error messages and implemented nowhere, because
// RunPrereqs discarded its arguments.
func TestRunPrereqs_RejectsAnUnknownFlag(t *testing.T) {
	clearToolchainOverrides(t)
	out := captureStdout(t, func() {
		err := RunPrereqs("", []string{"-nope"})
		if err == nil {
			t.Error("an unknown flag was accepted")
			return
		}
		if !strings.Contains(err.Error(), "-wasm") {
			t.Errorf("error = %v, want it to state the usage", err)
		}
	})
	if strings.Contains(out, "Prerequisites") {
		t.Errorf("a mistyped command line printed a report anyway:\n%s", out)
	}
}

// A host whose target has no pinned runtime is reported as a failure, not as a
// runtime that will be fetched later — there is nothing to fetch.
func TestRunPrereqs_ReportsAHostWithNoPinnedRuntime(t *testing.T) {
	clearToolchainOverrides(t)
	// Declare the runtimes for a target that is NOT this host, so the per-target
	// lookup misses on the running one.
	other := "linux-amd64"
	if CurrentBuildTarget() == other {
		other = "darwin-arm64"
	}
	root := writeRuntimeRoot(t, bothRuntimesTOML(other, false))

	out := captureStdout(t, func() {
		if err := RunPrereqs(root, nil); err != nil {
			t.Errorf("RunPrereqs: %v", err)
		}
	})
	for _, dep := range WasmRuntimeDeps() {
		want := "no pinned " + dep + " for " + CurrentBuildTarget()
		if !strings.Contains(out, want) {
			t.Errorf("expected %q in the report:\n%s", want, out)
		}
	}
}

// `-wasm` stages, and says where. When staging cannot happen the line reports
// the failure rather than claiming a runtime is ready.
func TestRunPrereqs_WasmStagingReportsItsOutcome(t *testing.T) {
	clearToolchainOverrides(t)
	target := CurrentBuildTarget()
	exe := RuntimeExeName("wasmtime", target)
	tarBytes := executableTarGz(t, "inner/"+exe, "#!/bin/true\n")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(tarBytes)
	}))
	t.Cleanup(srv.Close)

	// wasmtime resolves from the stub server; node's URL is bogus, so one line
	// reports a staged path and the other reports why it could not be staged —
	// both halves of the outcome in one run.
	toml := `schema = 1
[binaries.llvm]
version = "22.1.0"
bundle_dir = "compiler/cmd/promise/resources/llvm"
[binaries.llvm.targets.` + target + `]
url = "https://example.test/LLVM.tar.xz"
sha256 = "deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef0"
files = [ { src = "bin/opt", out = "opt" } ]

[binaries.wasmtime]
version = "` + testRuntimeVersion + `"
bundle_dir = ""
[binaries.wasmtime.targets.` + target + `]
url = "` + srv.URL + `/wasmtime.tar.gz"
sha256 = "` + sha256Hex(tarBytes) + `"
files = [ { src = "inner/` + exe + `", out = "` + exe + `" } ]

[binaries.node]
version = "` + testRuntimeVersion + `"
bundle_dir = ""
[binaries.node.targets.` + target + `]
url = "http://127.0.0.1:1/node.tar.gz"
sha256 = "deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef2"
files = [ { src = "inner/` + RuntimeExeName("node", target) + `", out = "` + RuntimeExeName("node", target) + `" } ]
`
	root := writeRuntimeRoot(t, toml)
	t.Setenv("PROMISE_PREBUILTS_CACHE", t.TempDir())

	out := captureStdout(t, func() {
		if err := RunPrereqs(root, []string{"-wasm"}); err != nil {
			t.Errorf("RunPrereqs -wasm reports, it does not fail: %v", err)
		}
	})
	if !strings.Contains(out, "pinned, staged at") {
		t.Errorf("a staged runtime should report its path:\n%s", out)
	}
	if !strings.Contains(out, "could not be staged") {
		t.Errorf("an unstageable runtime should report the failure:\n%s", out)
	}
}

// ── the per-runtime wrappers ────────────────────────────────────────────────

// EnsureWasmtime and EnsureNode must each ask for their OWN dependency. They are
// one-line wrappers, which is exactly why a copy-paste slip here would be
// invisible: both would work, and both would stage wasmtime.
func TestEnsureWasmtimeAndEnsureNodeAskForTheirOwnDependency(t *testing.T) {
	// No target entry for this host → each call fails naming the dependency it
	// looked for, which is the cheapest way to observe which one it asked for.
	root := writeRuntimeRoot(t, bothRuntimesTOML("some-other-target", false))
	t.Setenv("PROMISE_PREBUILTS_CACHE", t.TempDir())

	for _, tc := range []struct {
		dep  string
		call func(string) (string, error)
	}{
		{"wasmtime", EnsureWasmtime},
		{"node", EnsureNode},
	} {
		_, err := tc.call(root)
		if err == nil {
			t.Errorf("Ensure %s: expected an error for a host with no target entry", tc.dep)
			continue
		}
		if !strings.Contains(err.Error(), tc.dep) {
			t.Errorf("Ensure %s reported %v, which does not name %q — the wrapper asks for the wrong dependency",
				tc.dep, err, tc.dep)
		}
	}
}

// The darwin signature hook is a no-op everywhere else, and must stay harmless:
// it runs on every staged runtime, so a panic or a mangled file here would break
// staging on three platforms to serve one.
func TestEnsureRuntimeSignatureIsHarmless(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "wasmtime")
	const content = "not really a Mach-O\n"
	if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
	ensureRuntimeSignature(path) // the staged-file hook
	ensureRuntimeSignature("")   // a path that cannot exist
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("the hook removed the file it was given: %v", err)
	}
	if string(got) != content {
		t.Errorf("file = %q, want it left as %q", got, content)
	}
	if info, err := os.Stat(path); err == nil && info.Mode().Perm()&0o111 == 0 {
		t.Error("the hook cleared the execute bit")
	}
}
