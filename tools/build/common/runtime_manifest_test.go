package common

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGenerateRuntimeManifest(t *testing.T) {
	root := t.TempDir()
	resDir := filepath.Join(root, "compiler", "cmd", "promise", "resources")
	if err := os.MkdirAll(resDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// Fake FetchAll cache dir with flat `out` files.
	llvmCache := filepath.Join(root, "cache", "llvm")
	if err := os.MkdirAll(llvmCache, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, content := range map[string]string{"opt": "OPT", "llc": "LLC", "lld": "LLD"} {
		if err := os.WriteFile(filepath.Join(llvmCache, name), []byte(content), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	pm := &PrebuiltsManifest{
		Schema: PrebuiltsManifestSchema,
		Binaries: map[string]*PrebuiltEntry{
			"llvm": {
				Version:   "22.1.0",
				BundleDir: "compiler/cmd/promise/resources/llvm",
				Targets: map[string]*TargetEntry{
					"darwin-arm64": {
						URL:    "https://example/LLVM-22.1.0-macOS-ARM64.tar.xz",
						SHA256: "abc123",
						Files: []PrebuiltFile{
							{Src: "bin/opt", Out: "opt"},
							{Src: "bin/llc", Out: "llc"},
							{Src: "bin/lld", Out: "lld"},
						},
					},
				},
			},
		},
	}

	if err := GenerateRuntimeManifest(root, pm, llvmCache, "darwin-arm64", "2026.0"); err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(filepath.Join(resDir, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var m runtimeManifest
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("emitted manifest invalid JSON: %v", err)
	}
	if m.Schema != runtimeManifestSchema || m.Epoch != "2026.0" {
		t.Fatalf("schema/epoch = %d/%q", m.Schema, m.Epoch)
	}
	if len(m.Entries) != 3 {
		t.Fatalf("expected 3 entries, got %d", len(m.Entries))
	}
	byName := map[string]runtimeManifestEntry{}
	for _, e := range m.Entries {
		byName[e.Name] = e
	}
	opt, ok := byName["llvm-opt"]
	if !ok {
		t.Fatal("missing llvm-opt entry")
	}
	if opt.Kind != "macho-llvm" || opt.Size != 3 {
		t.Fatalf("opt entry kind/size = %q/%d", opt.Kind, opt.Size)
	}
	if len(opt.Sources) != 1 {
		t.Fatalf("expected 1 source, got %d", len(opt.Sources))
	}
	s := opt.Sources[0]
	if s.Archive != pm.Binaries["llvm"].Targets["darwin-arm64"].URL ||
		s.ArchivePath != "bin/opt" || s.ArchiveSHA256 != "abc123" {
		t.Fatalf("unexpected source: %+v", s)
	}
}

func TestGenerateRuntimeManifestLinuxKindIsBlob(t *testing.T) {
	root := t.TempDir()
	resDir := filepath.Join(root, "compiler", "cmd", "promise", "resources")
	if err := os.MkdirAll(resDir, 0o755); err != nil {
		t.Fatal(err)
	}
	llvmCache := filepath.Join(root, "cache", "llvm")
	if err := os.MkdirAll(llvmCache, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(llvmCache, "opt"), []byte("OPT"), 0o755); err != nil {
		t.Fatal(err)
	}
	pm := &PrebuiltsManifest{
		Schema: PrebuiltsManifestSchema,
		Binaries: map[string]*PrebuiltEntry{
			"llvm": {Version: "22.1.0", BundleDir: "x", Targets: map[string]*TargetEntry{
				"linux-amd64": {URL: "https://example/LLVM-linux.tar.xz", SHA256: "deadbeef",
					Files: []PrebuiltFile{{Src: "bin/opt", Out: "opt"}}},
			}},
		},
	}
	if err := GenerateRuntimeManifest(root, pm, llvmCache, "linux-amd64", "2026.0"); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(filepath.Join(resDir, "manifest.json"))
	var m runtimeManifest
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatal(err)
	}
	if len(m.Entries) != 1 || m.Entries[0].Kind != "blob" {
		t.Fatalf("linux LLVM entry kind should be blob, got %+v", m.Entries)
	}
}

func TestGenerateRuntimeManifestNoLLVMKeepsPlaceholder(t *testing.T) {
	root := t.TempDir()
	pm := &PrebuiltsManifest{Schema: PrebuiltsManifestSchema, Binaries: map[string]*PrebuiltEntry{"llvm": {Version: "1", BundleDir: "x"}}}
	// Empty llvmCacheDir → no-op, no error.
	if err := GenerateRuntimeManifest(root, pm, "", "darwin-arm64", "2026.0"); err != nil {
		t.Fatalf("expected no-op, got %v", err)
	}
}

func TestGenerateRuntimeManifestMissingLLVMBinary(t *testing.T) {
	root := t.TempDir()
	llvmCache := filepath.Join(root, "cache", "llvm")
	if err := os.MkdirAll(llvmCache, 0o755); err != nil {
		t.Fatal(err)
	}
	// No [binaries.llvm] entry at all → hard error (non-empty llvmCacheDir).
	pm := &PrebuiltsManifest{Schema: PrebuiltsManifestSchema, Binaries: map[string]*PrebuiltEntry{}}
	if err := GenerateRuntimeManifest(root, pm, llvmCache, "darwin-arm64", "2026.0"); err == nil {
		t.Fatal("expected error when [binaries.llvm] is missing")
	}
}

func TestHashAndSizeRejectsNonExistentFile(t *testing.T) {
	_, _, err := hashAndSize(filepath.Join(t.TempDir(), "does-not-exist"))
	if err == nil {
		t.Fatal("expected error for non-existent file")
	}
}

func TestBuildLLVMEntriesMissingFile(t *testing.T) {
	dir := t.TempDir()
	// Declare a file that does not exist in dir — hashAndSize should fail.
	tEntry := &TargetEntry{
		URL:    "https://example/LLVM.tar.xz",
		SHA256: "abc",
		Files:  []PrebuiltFile{{Src: "bin/opt", Out: "opt"}},
	}
	_, err := buildLLVMEntries(dir, tEntry, "linux-amd64", nil)
	if err == nil {
		t.Fatal("expected error when file listed in manifest is absent")
	}
	if !strings.Contains(err.Error(), "opt") {
		t.Errorf("error should name the missing file, got %q", err.Error())
	}
}

func TestGenerateRuntimeManifestHashFailure(t *testing.T) {
	root := t.TempDir()
	resDir := filepath.Join(root, "compiler", "cmd", "promise", "resources")
	if err := os.MkdirAll(resDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Create the cache dir but omit the declared file so hashAndSize fails.
	llvmCache := filepath.Join(root, "cache", "llvm")
	if err := os.MkdirAll(llvmCache, 0o755); err != nil {
		t.Fatal(err)
	}
	pm := &PrebuiltsManifest{
		Schema: PrebuiltsManifestSchema,
		Binaries: map[string]*PrebuiltEntry{
			"llvm": {Version: "22.1.0", BundleDir: "x", Targets: map[string]*TargetEntry{
				"linux-amd64": {
					URL:    "https://example/LLVM-linux.tar.xz",
					SHA256: "deadbeef",
					Files:  []PrebuiltFile{{Src: "bin/opt", Out: "opt"}},
				},
			}},
		},
	}
	// File "opt" is missing from llvmCache → buildLLVMEntries fails → GenerateRuntimeManifest returns error.
	if err := GenerateRuntimeManifest(root, pm, llvmCache, "linux-amd64", "2026.0"); err == nil {
		t.Fatal("expected error when LLVM binary is missing from cache dir")
	}
}

func TestGenerateRuntimeManifestUnsupportedTargetSkips(t *testing.T) {
	root := t.TempDir()
	llvmCache := filepath.Join(root, "cache", "llvm")
	if err := os.MkdirAll(llvmCache, 0o755); err != nil {
		t.Fatal(err)
	}
	// Target present but marked Unsupported → keep placeholder, no error, no write.
	pm := &PrebuiltsManifest{
		Schema: PrebuiltsManifestSchema,
		Binaries: map[string]*PrebuiltEntry{
			"llvm": {Version: "1", BundleDir: "x", Targets: map[string]*TargetEntry{
				"darwin-arm64": {Unsupported: "no LLVM build for this host"},
			}},
		},
	}
	if err := GenerateRuntimeManifest(root, pm, llvmCache, "darwin-arm64", "2026.0"); err != nil {
		t.Fatalf("unsupported target should be a no-op, got %v", err)
	}
	// The placeholder is left in place — no manifest.json written.
	if _, err := os.Stat(filepath.Join(root, "compiler", "cmd", "promise", "resources", "manifest.json")); err == nil {
		t.Fatal("no manifest.json should be written for an unsupported target")
	}
}
