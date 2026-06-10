package common

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// T0772: tests for `bin/release winlink`, the generator that turns the
// license-clean .def symbol lists into MSVC-ABI import libraries via
// llvm-dlltool. The .def is the source of truth; the .lib is a reproducible
// build artifact.

// writeTestDef writes a minimal valid .def declaring one export for dll.
func writeTestDef(t *testing.T, dir, base, dll, export string) {
	t.Helper()
	content := "LIBRARY " + dll + "\nEXPORTS\n" + export + "\n"
	if err := os.WriteFile(filepath.Join(dir, base+".def"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestRunReleaseWinlinkGenerates(t *testing.T) {
	if Which("llvm-dlltool") == "" {
		t.Skip("llvm-dlltool not on PATH")
	}
	root := t.TempDir()
	defDir := filepath.Join(root, "def")
	outDir := filepath.Join(root, "out")
	if err := os.MkdirAll(defDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeTestDef(t, defDir, "kernel32", "kernel32.dll", "ExitProcess")
	writeTestDef(t, defDir, "advapi32", "advapi32.dll", "GetUserNameA")

	if err := runReleaseWinlink(root, []string{"--def-dir", defDir, "--out", outDir}); err != nil {
		t.Fatalf("runReleaseWinlink: %v", err)
	}

	for _, name := range []string{"kernel32.lib", "advapi32.lib"} {
		path := filepath.Join(outDir, name)
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("generated lib %s missing: %v", name, err)
		}
		// An import lib is an ar archive — must start with the ar magic.
		if !strings.HasPrefix(string(data), "!<arch>\n") {
			t.Errorf("%s is not a valid ar archive (bad magic)", name)
		}
		if len(data) == 0 {
			t.Errorf("%s is empty", name)
		}
	}
}

func TestRunReleaseWinlinkMissingTool(t *testing.T) {
	root := t.TempDir()
	defDir := filepath.Join(root, "def")
	if err := os.MkdirAll(defDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeTestDef(t, defDir, "kernel32", "kernel32.dll", "ExitProcess")
	// Point at a nonexistent tool so the "tool not found / failed" path runs.
	err := runReleaseWinlink(root, []string{
		"--llvm-dlltool", filepath.Join(root, "does-not-exist-dlltool"),
		"--def-dir", defDir,
		"--out", filepath.Join(root, "out"),
	})
	if err == nil {
		t.Fatal("expected error when llvm-dlltool path is invalid")
	}
}

func TestRunReleaseWinlinkNoDefs(t *testing.T) {
	if Which("llvm-dlltool") == "" {
		t.Skip("llvm-dlltool not on PATH")
	}
	root := t.TempDir()
	emptyDef := filepath.Join(root, "def")
	if err := os.MkdirAll(emptyDef, 0o755); err != nil {
		t.Fatal(err)
	}
	err := runReleaseWinlink(root, []string{"--def-dir", emptyDef, "--out", filepath.Join(root, "out")})
	if err == nil {
		t.Fatal("expected error when no .def files are present")
	}
	if !strings.Contains(err.Error(), "no .def files") {
		t.Errorf("error = %v, want 'no .def files'", err)
	}
}

func TestRunReleaseWinlinkBadFlag(t *testing.T) {
	if err := runReleaseWinlink(t.TempDir(), []string{"--nonexistent-flag"}); err == nil {
		t.Fatal("expected error for unknown flag")
	}
}

// TestReleaseWinlinkSubcommandWiring verifies `winlink` is reachable through the
// RunRelease dispatcher (a bad flag is enough to prove it routed there rather
// than erroring as an unknown subcommand).
func TestReleaseWinlinkSubcommandWiring(t *testing.T) {
	err := RunRelease(t.TempDir(), []string{"winlink", "--nonexistent-flag"})
	if err == nil {
		t.Fatal("expected error from winlink subcommand")
	}
	if strings.Contains(err.Error(), "unknown") {
		t.Errorf("winlink subcommand not wired into RunRelease: %v", err)
	}
}

// winlinkDefRoot creates a temp repo root with the real winlinkDefDir layout and
// one .def, so ensureWinlinkLibs sees a non-trimmed checkout.
func winlinkDefRoot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	defDir := filepath.Join(root, filepath.FromSlash(winlinkDefDir))
	if err := os.MkdirAll(defDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeTestDef(t, defDir, "kernel32", "kernel32.dll", "ExitProcess")
	return root
}

// TestEnsureWinlinkLibsNoOpWhenLibsPresent guards the fast path: when the .lib
// already exist, ensureWinlinkLibs returns nil without resolving or fetching a
// tool (so a steady-state incremental build pays no cost). We point both the
// prebuilts cache and PATH at empty dirs — if the fast path were skipped and a
// fetch/resolve were attempted, it would error instead of returning nil.
func TestEnsureWinlinkLibsNoOpWhenLibsPresent(t *testing.T) {
	root := winlinkDefRoot(t)
	outDir := filepath.Join(root, filepath.FromSlash(winlinkOutDir))
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outDir, "kernel32.lib"), []byte("!<arch>\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PROMISE_PREBUILTS_CACHE", t.TempDir())
	t.Setenv("PATH", t.TempDir())
	if err := ensureWinlinkLibs(root); err != nil {
		t.Fatalf("ensureWinlinkLibs with libs present should be a no-op, got: %v", err)
	}
	// No slim cache should have been created.
	if Exists(filepath.Join(os.Getenv("PROMISE_PREBUILTS_CACHE"), "llvm-slim")) {
		t.Error("fast path fetched LLVM despite the .lib already being present")
	}
}

// TestEnsureWinlinkLibsNoOpWhenDefDirMissing covers a trimmed checkout: no .def
// source dir → nil, no fetch.
func TestEnsureWinlinkLibsNoOpWhenDefDirMissing(t *testing.T) {
	root := t.TempDir() // no tools/build/winlink/def
	t.Setenv("PROMISE_PREBUILTS_CACHE", t.TempDir())
	t.Setenv("PATH", t.TempDir())
	if err := ensureWinlinkLibs(root); err != nil {
		t.Fatalf("ensureWinlinkLibs with no def dir should be a no-op, got: %v", err)
	}
}

// TestEnsureWinlinkLibsSeedsSlimCache is the regression test for T0840: when the
// .lib are absent and llvm-dlltool is resolvable neither from the slim cache nor
// PATH, ensureWinlinkLibs must SEED the slim LLVM cache (which hosts the
// build-only llvm-dlltool, T0833) and use the fetched copy to generate the libs.
// Before the fix this errored with "llvm-dlltool not found" on a prebuilt-only
// host even though the blob was published.
func TestEnsureWinlinkLibsSeedsSlimCache(t *testing.T) {
	root := winlinkDefRoot(t)
	target := CurrentBuildTarget()

	// Declare a build-only llvm-dlltool for the host target in prebuilts.toml.
	writeHostLLVMPrebuilts(t, root, target, true)

	// The slim blob is a stub llvm-dlltool: it parses `-l <out>` and writes a
	// minimal ar archive there, so runReleaseWinlink produces a real .lib.
	cat := &BlobsCatalog{Schema: BlobsCatalogSchema}
	raw := []byte(dllToolStub)
	br := brotliBytes(t, raw)
	sha := sha256Hex(raw)
	if err := cat.Upsert(BlobEntry{
		Dependency:       "llvm",
		Version:          "22.1.0",
		Target:           target,
		Name:             "llvm-dlltool",
		SHA256:           sha,
		Size:             int64(len(raw)),
		Compression:      compressionBrotli,
		CompressedSize:   int64(len(br)),
		CompressedSHA256: sha256Hex(br),
	}); err != nil {
		t.Fatal(err)
	}
	if err := WriteBlobsCatalog(root, cat); err != nil {
		t.Fatal(err)
	}

	t.Setenv("PROMISE_PREBUILTS_CACHE", t.TempDir())
	t.Setenv("PATH", t.TempDir()) // no system llvm-dlltool → force the seed path
	prev := defaultBlobFetcher
	defaultBlobFetcher = &countingBlobFetcher{assets: map[string][]byte{sha + ".br": br}}
	t.Cleanup(func() { defaultBlobFetcher = prev })

	if err := ensureWinlinkLibs(root); err != nil {
		t.Fatalf("ensureWinlinkLibs should seed the slim cache and generate libs, got: %v", err)
	}

	libPath := filepath.Join(root, filepath.FromSlash(winlinkOutDir), "kernel32.lib")
	data, err := os.ReadFile(libPath)
	if err != nil {
		t.Fatalf("generated lib missing: %v", err)
	}
	if !strings.HasPrefix(string(data), "!<arch>\n") {
		t.Errorf("kernel32.lib is not a valid ar archive (bad magic)")
	}
}

// dllToolStub is a shell stub standing in for llvm-dlltool: it parses `-l <out>`
// and writes a minimal ar archive there so runReleaseWinlink produces a real
// .lib without a system LLVM. The marker after the magic lets a test prove
// *this* binary (not a fetched one) ran.
const dllToolStub = "#!/bin/sh\nout=\"\"\nwhile [ $# -gt 0 ]; do\n  case \"$1\" in\n    -l) out=\"$2\"; shift 2;;\n    *) shift;;\n  esac\ndone\nprintf '!<arch>\\nSLIM' > \"$out\"\n"

// writeHostLLVMPrebuilts writes a prebuilts.toml whose [binaries.llvm] resolves
// SlimLLVMCacheDir for the host. When withTarget is true it also adds a
// build-only llvm-dlltool target entry (so EnsureLLVMBlobs can plan a fetch);
// when false the host target table is absent (so EnsureLLVMBlobs errors).
func writeHostLLVMPrebuilts(t *testing.T, root, target string, withTarget bool) {
	t.Helper()
	toolsBuild := filepath.Join(root, "tools", "build")
	if err := os.MkdirAll(toolsBuild, 0o755); err != nil {
		t.Fatal(err)
	}
	toml := "schema = 1\n[binaries.llvm]\nversion = \"22.1.0\"\nbundle_dir = \"compiler/cmd/promise/resources/llvm\"\n"
	if withTarget {
		toml += "[binaries.llvm.targets." + target + "]\n" +
			"url = \"https://example.test/LLVM.tar.xz\"\n" +
			"sha256 = \"deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef0\"\n" +
			"files = [\n  { src = \"bin/llvm-dlltool\", out = \"llvm-dlltool\", build_only = true },\n]\n"
	}
	if err := os.WriteFile(filepath.Join(toolsBuild, "prebuilts.toml"), []byte(toml), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestEnsureWinlinkLibsUsesSlimCacheTool covers the slim-cache-hit branch of
// resolveWinlinkDllTool: when llvm-dlltool is already present in the slim LLVM
// cache, ensureWinlinkLibs must use it directly and NOT trigger a fetch. PATH is
// empty and no catalog/blob fetcher is wired, so the only way the lib can be
// generated is via the slim-cache copy.
func TestEnsureWinlinkLibsUsesSlimCacheTool(t *testing.T) {
	root := winlinkDefRoot(t)
	target := CurrentBuildTarget()
	writeHostLLVMPrebuilts(t, root, target, true)

	t.Setenv("PROMISE_PREBUILTS_CACHE", t.TempDir())
	t.Setenv("PATH", t.TempDir()) // no system llvm-dlltool

	dir, ok := SlimLLVMCacheDir(root, target)
	if !ok {
		t.Fatal("SlimLLVMCacheDir not resolvable with a [binaries.llvm] manifest")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "llvm-dlltool"+ExeSuffix()), []byte(dllToolStub), 0o755); err != nil {
		t.Fatal(err)
	}

	// An empty fetcher: any attempted blob fetch would error, proving a fetch
	// was NOT needed when the slim-cache tool is already present.
	prev := defaultBlobFetcher
	defaultBlobFetcher = &countingBlobFetcher{assets: map[string][]byte{}}
	t.Cleanup(func() { defaultBlobFetcher = prev })

	if err := ensureWinlinkLibs(root); err != nil {
		t.Fatalf("ensureWinlinkLibs should use the slim-cache tool, got: %v", err)
	}

	libPath := filepath.Join(root, filepath.FromSlash(winlinkOutDir), "kernel32.lib")
	data, err := os.ReadFile(libPath)
	if err != nil {
		t.Fatalf("generated lib missing: %v", err)
	}
	if string(data) != "!<arch>\nSLIM" {
		t.Errorf("lib not produced by the slim-cache stub (got %q)", string(data))
	}
}

// TestEnsureWinlinkLibsFetchError covers the unresolvable-tool branch of
// ensureWinlinkLibs (T0840): the .lib are absent, and llvm-dlltool resolves from
// neither the slim cache, PATH, nor a seed fetch (here: no host target entry in
// prebuilts.toml, so resolveWinlinkDllTool's EnsureLLVMBlobs fetch fails and it
// returns ""). runReleaseWinlink must then surface the "not found" error rather
// than silently producing no libs.
func TestEnsureWinlinkLibsFetchError(t *testing.T) {
	root := winlinkDefRoot(t)
	// [binaries.llvm] present (SlimLLVMCacheDir resolves) but no host target
	// entry → the resolver's EnsureLLVMBlobs fetch fails.
	writeHostLLVMPrebuilts(t, root, CurrentBuildTarget(), false)

	t.Setenv("PROMISE_PREBUILTS_CACHE", t.TempDir())
	t.Setenv("PATH", t.TempDir()) // no system llvm-dlltool → force the seed path

	err := ensureWinlinkLibs(root)
	if err == nil {
		t.Fatal("expected an error when llvm-dlltool cannot be resolved or fetched")
	}
	if !strings.Contains(err.Error(), "llvm-dlltool not found") {
		t.Errorf("error should report llvm-dlltool not found, got: %v", err)
	}
}
